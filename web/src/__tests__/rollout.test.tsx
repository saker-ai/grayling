// rollout.test.tsx pins the staged-rollout zone (RolloutBlock + RolloutCreate,
// web-pages/src/workbench/RolloutBlock.tsx) against the legacy rollout
// contract: all seven plan states render with their bound ids, every mutation
// goes through the two-step ConfirmDialog (danger button + reason), the wire
// bodies stay byte-exact (pause {reason}; resume {expectedReportVersion};
// advance {reason, expectedReportVersion}; takeover-on {enabled, reason};
// takeover-off {enabled, reason, expectedReportVersion}; create wrapped in an
// approval request), the stale-plan re-fetch guard refuses the dialog, and
// the report gate disables resume/advance/takeover-off until evidence loads.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ExperimentDetailPage } from '@saker/grayling-pages/pages/ExperimentDetailPage';
import type { ExperimentDetail, ExperimentReport, ReleasePlan, ReleaseStep } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const ROUTE = {
  path: '/console/:ws/:env/experiments/:expId/:tab',
  route: '/console/ws1/prod/experiments/exp-1/rollout',
};
const EXP_PATH = '/api/v1/admin/experiments/exp-1';
const PLAN_PATH = EXP_PATH + '/release-plan';
const NS_HOUR = 3600000000000;

const STEPS: ReleaseStep[] = [
  { name: 'canary', ratio: 5, minRuntime: 2 * NS_HOUR, observationWindow: NS_HOUR, autoAdvance: true },
  { name: 'half', ratio: 50, minRuntime: 4 * NS_HOUR, observationWindow: 2 * NS_HOUR, autoAdvance: true },
  { name: 'full', ratio: 100, minRuntime: 4 * NS_HOUR, observationWindow: 0, autoAdvance: false },
];

function plan(partial: Partial<ReleasePlan>): ReleasePlan {
  return {
    steps: STEPS,
    state: 'running',
    currentStep: 0,
    stepStartedAt: '2026-01-05T00:00:00Z',
    createdBy: 'alice',
    updatedAt: '2026-01-05T00:00:00Z',
    history: [
      { at: '2026-01-02T00:00:00Z', actor: 'operator', action: 'create', step: 0 },
      { at: '2026-01-03T00:00:00Z', actor: 'engine', action: 'advance', step: 0, detail: 'gate passed' },
    ],
    ...partial,
  };
}

function experimentWith(releasePlan: ReleasePlan | null): ExperimentDetail {
  return {
    id: 'exp-1',
    flagKey: 'checkout',
    state: 'running',
    epoch: 3,
    flagRevision: 7,
    environmentId: 'prod',
    createdBy: 'alice',
    unit: 'user_id',
    variants: ['control', 'treatment'],
    proportions: { control: 50, treatment: 50 },
    primaryMetric: { key: 'checkout_success', type: 'proportion', direction: 'increase' },
    exposureMode: 'onDecision',
    releasePlan,
  };
}

const REPORT: ExperimentReport = {
  experimentId: 'exp-1',
  epoch: 3,
  flagKey: 'checkout',
  state: 'running',
  generatedAt: '2026-01-05T12:00:00Z',
  evidenceVersion: 'ev-1',
  healthy: true,
  healthIssues: [],
  decision: { verdict: 'ship', reasons: [] },
};

interface Call {
  method: string;
  path: string;
  rawBody?: string;
}

interface SetupOptions {
  reportStatus?: number;
  permissions?: string[];
  // experimentFn lets a test change what the re-fetch returns (stale guard).
  experimentFn?: () => ExperimentDetail;
}

function setup(releasePlan: ReleasePlan | null, options: SetupOptions = {}) {
  const calls: Call[] = [];
  const getExp = options.experimentFn ?? (() => experimentWith(releasePlan));
  vi.stubGlobal(
    'fetch',
    vi.fn(
      fetchMock((url, init) => {
        const method = String(init.method || 'GET').toUpperCase();
        const rawBody = init.body ? String(init.body) : undefined;
        calls.push({ method, path: url.pathname + url.search, rawBody });
        if (url.pathname === EXP_PATH + '/report') {
          return options.reportStatus ? { status: options.reportStatus, body: {} } : { body: REPORT };
        }
        if (url.pathname === EXP_PATH) return { body: getExp() };
        if (method === 'POST' && url.pathname.startsWith(PLAN_PATH)) return { body: {} };
        if (url.pathname.endsWith('/approvals') && method === 'POST') return { body: { id: 'appr-1' } };
        return { status: 404, body: {} };
      }),
    ),
  );
  renderPage(<ExperimentDetailPage />, {
    ...ROUTE,
    permissions: options.permissions ?? ['report.read', 'flag.publish'],
  });
  return { calls };
}

function posts(calls: Call[], suffix: string): Call[] {
  return calls.filter(c => c.method === 'POST' && c.path === PLAN_PATH + suffix);
}

async function openDialog(name: string | RegExp): Promise<HTMLElement> {
  await userEvent.click(screen.getByRole('button', { name }));
  return screen.findByRole('dialog');
}

describe('RolloutBlock', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('renders the no-plan create editor and posts the approval-wrapped create body', async () => {
    const { calls } = setup(null);
    expect(await screen.findByText(/No release plan registered/)).toBeTruthy();
    expect(document.getElementById('ro-steps-editor')).toBeTruthy();

    // R1.4: every step field has a real (visually-hidden) label, so the row
    // is locatable by accessible name — placeholders stay sighted-only hints.
    expect(screen.getByRole('textbox', { name: 'Step name' })).toBeTruthy();
    expect(screen.getByRole('spinbutton', { name: 'Eligibility (%)' })).toBeTruthy();
    expect(screen.getByRole('spinbutton', { name: 'Min runtime (hours)' })).toBeTruthy();
    expect(screen.getByRole('spinbutton', { name: 'Observation window (hours)' })).toBeTruthy();

    // Fill the single step row, then register.
    const name = document.querySelector('.rs-name') as HTMLInputElement;
    const ratio = document.querySelector('.rs-ratio') as HTMLInputElement;
    const min = document.querySelector('.rs-min') as HTMLInputElement;
    const obs = document.querySelector('.rs-obs') as HTMLInputElement;
    const auto = document.querySelector('.rs-auto') as HTMLInputElement;
    await userEvent.type(name, 'canary');
    await userEvent.type(ratio, '5');
    await userEvent.type(min, '2');
    await userEvent.type(obs, '1');
    await userEvent.click(auto);
    await userEvent.click(screen.getByRole('button', { name: 'Register release plan' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Reason for this change')).toBeTruthy();
    await userEvent.type(within(dialog).getByLabelText('Reason for this change'), 'roll it');
    await userEvent.click(within(dialog).getByRole('button', { name: 'OK' }));

    await screen.findByText('Approval request appr-1 created. Review it in Approvals.');
    const post = calls.find(c => c.method === 'POST' && c.path.endsWith('/approvals'));
    expect(post?.path).toBe('/api/v1/admin/workspaces/ws1/environments/prod/approvals');
    expect(post?.rawBody).toBe(
      '{"action":"release_plan.create","object":"exp-1","payload":{"steps":[' +
        '{"name":"canary","ratio":5,"minRuntime":7200000000000,"observationWindow":3600000000000,"autoAdvance":true}' +
        ']},"reason":"roll it"}',
    );
  });

  it('renders the armed state: banner, engine chip, steps and armed note', async () => {
    setup(plan({ state: 'armed', currentStep: 0, stepStartedAt: undefined }));
    const banner = await screen.findByText(/Armed — waiting for the experiment to enter/);
    expect(document.getElementById('ro-banner')).toBeTruthy();
    expect(banner).toBeTruthy();
    expect(screen.getByText('plan armed')).toBeTruthy();
    expect(screen.getByText('engine auto').className).toContain('chip green');
    expect(screen.getByText(/step 1\/3/)).toBeTruthy();
    expect(screen.getByText('(starts when the experiment enters running)')).toBeTruthy();
    // Bound ids.
    expect(document.getElementById('ro-steps-tbl')).toBeTruthy();
    expect(document.getElementById('ro-history-tbl')).toBeTruthy();
    expect(document.getElementById('ro-bar')).toBeTruthy();
    expect(document.getElementById('ro-next-eval')?.textContent).toBe('—');
    expect(document.getElementById('ro-report-gate')).toBeTruthy();
    // Armed offers pause + takeover-on only.
    expect(document.getElementById('ro-pause')).toBeTruthy();
    expect(document.getElementById('ro-takeover-on')).toBeTruthy();
    expect(document.getElementById('ro-resume')).toBeNull();
    expect(document.getElementById('ro-advance')).toBeNull();
    // Step 0 carries the current marker, the rest are pending.
    const stepsTbl = document.getElementById('ro-steps-tbl') as HTMLElement;
    expect(within(stepsTbl).getByText('current').className).toContain('chip blue');
    expect(within(stepsTbl).getAllByText('pending')).toHaveLength(2);
  });

  it('renders the running state with the computed gate and the full action row', async () => {
    setup(plan({ state: 'running' }));
    await screen.findByText('plan running');
    // No state banner while running.
    expect(document.getElementById('ro-banner')).toBeNull();
    // Gate = stepStartedAt + minRuntime + observationWindow (3h after midnight).
    expect(document.getElementById('ro-next-eval')?.textContent).not.toBe('—');
    expect(screen.getByText(/step started/)).toBeTruthy();
    expect(document.getElementById('ro-pause')).toBeTruthy();
    expect(document.getElementById('ro-advance')).toBeTruthy();
    expect(document.getElementById('ro-takeover-on')).toBeTruthy();
    expect(document.getElementById('ro-resume')).toBeNull();
    // R1.3: the plan bar is a real progressbar (0 of 3 steps done here).
    const bar = screen.getByRole('progressbar', { name: 'Rollout progress' });
    expect(bar.getAttribute('aria-valuenow')).toBe('0');
    expect(bar.getAttribute('aria-valuemin')).toBe('0');
    expect(bar.getAttribute('aria-valuemax')).toBe('100');
    // History renders newest first.
    const history = document.getElementById('ro-history-tbl') as HTMLElement;
    const rows = within(history).getAllByRole('row');
    expect(within(rows[1]).getByText('advance')).toBeTruthy();
    expect(within(rows[2]).getByText('create')).toBeTruthy();
    expect(within(rows[1]).getByText('gate passed')).toBeTruthy();
  });

  it('renders manual takeover: banner, red chip and the release action', async () => {
    setup(plan({ state: 'running', manualOverride: true }));
    await screen.findByText('plan running');
    expect(document.getElementById('ro-takeover-banner')?.textContent).toContain('Manual takeover active');
    expect(screen.getByText('manual override').className).toContain('chip red');
    expect(document.getElementById('ro-takeover-off')).toBeTruthy();
    expect(document.getElementById('ro-takeover-on')).toBeNull();
  });

  it('renders a manual pause as a warn banner with the operator reason', async () => {
    setup(plan({ state: 'paused', pausedReason: 'operator hold: watching latency' }));
    const banner = (await screen.findByText('PAUSED')).closest('.banner') as HTMLElement;
    expect(banner.className).toContain('warn');
    expect(banner.textContent).toContain('operator hold: watching latency');
    expect(screen.getByText('(held — nothing is evaluated while paused)')).toBeTruthy();
    // The current step is held, not current.
    const stepsTbl = document.getElementById('ro-steps-tbl') as HTMLElement;
    expect(within(stepsTbl).getByText('held here').className).toContain('chip amber');
    expect(document.getElementById('ro-resume')).toBeTruthy();
    expect(document.getElementById('ro-takeover-on')).toBeTruthy();
    expect(document.getElementById('ro-pause')).toBeNull();
  });

  it('renders an SRM-paused plan as the blocked error banner', async () => {
    setup(plan({ state: 'paused', pausedReason: 'SRM alarm: chi-square p=0.001' }));
    const banner = (await screen.findByText('BLOCKED — auto-paused by the engine')).closest('.banner') as HTMLElement;
    expect(banner.className).toContain('err');
    expect(banner.textContent).toContain('SRM alarm: chi-square p=0.001');
    expect(banner.textContent).toContain('The engine auto-pauses on SRM / data-health problems and never auto-resumes.');
  });

  it('renders rolled_back with the stopped-here step and no mutation buttons', async () => {
    setup(plan({ state: 'rolled_back', currentStep: 1, pausedReason: 'guardrail latency_p95 breached' }));
    const banner = (await screen.findByText('ROLLED BACK — enrollment stopped')).closest('.banner') as HTMLElement;
    expect(banner.className).toContain('err');
    expect(banner.textContent).toContain('guardrail latency_p95 breached');
    const stepsTbl = document.getElementById('ro-steps-tbl') as HTMLElement;
    expect(within(stepsTbl).getByText('stopped here').className).toContain('chip red');
    expect(within(stepsTbl).getByText('done')).toBeTruthy();
    expect(document.getElementById('ro-pause')).toBeNull();
    expect(document.getElementById('ro-resume')).toBeNull();
    expect(document.getElementById('ro-advance')).toBeNull();
    expect(document.getElementById('ro-takeover-on')).toBeNull();
    expect(document.getElementById('ro-takeover-off')).toBeNull();
    expect(document.getElementById('ro-refresh')).toBeTruthy();
  });

  it('renders completed with all steps done and no actions', async () => {
    setup(plan({ state: 'completed', currentStep: 2 }));
    const banner = (await screen.findByText(/Plan completed — the flag stays at 100%/)).closest('.banner') as HTMLElement;
    expect(banner.className).toContain('ok');
    const stepsTbl = document.getElementById('ro-steps-tbl') as HTMLElement;
    expect(within(stepsTbl).getAllByText('done')).toHaveLength(3);
    expect(screen.getByRole('progressbar', { name: 'Rollout progress' }).getAttribute('aria-valuenow')).toBe('100');
    expect(document.getElementById('ro-pause')).toBeNull();
    expect(document.getElementById('ro-advance')).toBeNull();
  });

  it('pause: two-step confirm with required reason posts byte-exact {reason}', async () => {
    const { calls } = setup(plan({ state: 'running' }));
    await screen.findByText('plan running');
    const dialog = await openDialog('Pause…');
    // Scope summary + the legacy warning copy.
    expect(within(dialog).getByText(/Experiment exp-1 · environment prod · step canary · 5% eligibility/)).toBeTruthy();
    expect(within(dialog).getByText(/the step clock RESTARTS/)).toBeTruthy();
    // Reason is required: confirming empty does nothing.
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pause…' }));
    expect(posts(calls, '/pause')).toHaveLength(0);
    await userEvent.type(within(dialog).getByLabelText('Pause reason (required):'), 'operator hold');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pause…' }));
    await screen.findByText('Release plan paused');
    expect(posts(calls, '/pause')).toHaveLength(1);
    expect(posts(calls, '/pause')[0].rawBody).toBe('{"reason":"operator hold"}');
  });

  it('resume: no reason field, body carries only the pinned evidence version', async () => {
    const { calls } = setup(plan({ state: 'paused', pausedReason: 'operator hold' }));
    await screen.findByText('PAUSED');
    const dialog = await openDialog('Resume…');
    expect(within(dialog).queryByRole('textbox')).toBeNull();
    // The evidence preview pins the version being asserted (the label is
    // "<strong>Report evidence version:</strong>" — match the prefix).
    expect(within(dialog).getByText(/Report evidence version/)).toBeTruthy();
    expect(within(dialog).getByText('ev-1')).toBeTruthy();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Resume…' }));
    await screen.findByText('Release plan resumed');
    expect(posts(calls, '/resume')).toHaveLength(1);
    expect(posts(calls, '/resume')[0].rawBody).toBe('{"expectedReportVersion":"ev-1"}');
  });

  it('advance: optional reason plus the pinned evidence version', async () => {
    const { calls } = setup(plan({ state: 'running' }));
    await screen.findByText('plan running');
    const dialog = await openDialog('Advance now…');
    expect(within(dialog).getByText(/NOT gated by the report verdict/)).toBeTruthy();
    await userEvent.type(within(dialog).getByLabelText('Advance reason (optional):'), 'go');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Advance now…' }));
    await screen.findByText('Release plan advanced');
    expect(posts(calls, '/advance')).toHaveLength(1);
    expect(posts(calls, '/advance')[0].rawBody).toBe('{"reason":"go","expectedReportVersion":"ev-1"}');
  });

  it('takeover-on: required reason, no evidence pinning', async () => {
    const { calls } = setup(plan({ state: 'running' }));
    await screen.findByText('plan running');
    const dialog = await openDialog('Manual takeover…');
    expect(within(dialog).getByText(/the engine will take NO automatic action/i)).toBeTruthy();
    await userEvent.type(within(dialog).getByLabelText('Takeover reason (required):'), 'incident');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Manual takeover…' }));
    await screen.findByText('Manual takeover enabled — the engine stands down');
    expect(posts(calls, '/takeover')).toHaveLength(1);
    expect(posts(calls, '/takeover')[0].rawBody).toBe('{"enabled":true,"reason":"incident"}');
  });

  it('takeover-off: optional empty reason plus the pinned evidence version', async () => {
    const { calls } = setup(plan({ state: 'running', manualOverride: true }));
    await screen.findByText('plan running');
    const dialog = await openDialog('Release takeover…');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Release takeover…' }));
    await screen.findByText('Manual takeover released — engine control restored');
    expect(posts(calls, '/takeover')).toHaveLength(1);
    expect(posts(calls, '/takeover')[0].rawBody).toBe('{"enabled":false,"reason":"","expectedReportVersion":"ev-1"}');
  });

  it('stale-plan guard: a re-fetched plan that moved refuses the dialog', async () => {
    let current = experimentWith(plan({ state: 'running' }));
    const { calls } = setup(null, { experimentFn: () => current });
    await screen.findByText('plan running');
    // The plan advances under the operator before they confirm.
    current = experimentWith(plan({ state: 'running', currentStep: 1 }));
    await userEvent.click(screen.getByRole('button', { name: 'Pause…' }));
    await screen.findByText('The release plan changed. Review the current step and open the action again.');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(posts(calls, '/pause')).toHaveLength(0);
  });

  it('report gate: an unavailable report disables resume but not takeover-on', async () => {
    setup(plan({ state: 'paused', pausedReason: 'operator hold' }), { reportStatus: 500 });
    await screen.findByText('PAUSED');
    const gate = document.getElementById('ro-report-gate') as HTMLElement;
    await waitFor(() =>
      expect(gate.textContent).toContain(
        'The current report is unavailable. Resume, advance, and return to automatic control are disabled',
      ),
    );
    expect((document.getElementById('ro-resume') as HTMLButtonElement).disabled).toBe(true);
    expect((document.getElementById('ro-takeover-on') as HTMLButtonElement).disabled).toBe(false);
  });

  it('report gate: without report.read the gate renders the restricted copy', async () => {
    setup(plan({ state: 'paused', pausedReason: 'operator hold' }), { permissions: ['flag.publish'] });
    await screen.findByText('PAUSED');
    const gate = document.getElementById('ro-report-gate') as HTMLElement;
    expect(gate.textContent).toContain('Report read access is required before resuming, advancing, or returning to automatic control.');
    expect((document.getElementById('ro-resume') as HTMLButtonElement).disabled).toBe(true);
  });
});
