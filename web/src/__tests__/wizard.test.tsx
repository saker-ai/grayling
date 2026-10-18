// wizard.test.tsx pins the five-step experiment create wizard
// (web-pages/src/pages/ExperimentCreatePage.tsx): the per-step validation
// order, the published-flag readiness gate between goal and split, the
// forced exposure-mode choice (with onRender trigger/owner registration),
// the review-step contract, the byte-exact create POST (a draft only), and
// the exp.wizardDiscard leave guard wired through useBlocker.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ExperimentCreatePage } from '@saker/grayling-pages/pages/ExperimentCreatePage';
import type { FlagListItem } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const ROUTE = {
  path: '/console/:ws/:env/experiments/~new',
  route: '/console/ws1/prod/experiments/~new',
};
const SCOPE = '/api/v1/admin/workspaces/ws1/environments/prod';
const FLAG_PATH = SCOPE + '/flags/checkout';
const METRICS_PATH = '/api/v1/admin/workspaces/ws1/metrics';

const PUBLISHED_FLAG: FlagListItem = {
  key: 'checkout',
  currentRevision: 7,
  current: {
    revision: 7,
    publishedAt: '2026-01-01T00:00:00Z',
    content: { variations: { control: { value: false }, treatment: { value: true } } },
  },
} as FlagListItem;

interface Call {
  method: string;
  path: string;
  rawBody?: string;
}

function setup(options: { flagStatus?: number; flag?: FlagListItem } = {}) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      fetchMock((url, init) => {
        const method = String(init.method || 'GET').toUpperCase();
        calls.push({ method, path: url.pathname + url.search, rawBody: init.body ? String(init.body) : undefined });
        if (url.pathname === METRICS_PATH) return { body: { items: [] } };
        if (url.pathname === FLAG_PATH) {
          return options.flagStatus ? { status: options.flagStatus, body: {} } : { body: options.flag ?? PUBLISHED_FLAG };
        }
        if (url.pathname === SCOPE + '/experiments' && method === 'POST') return { body: { id: 'exp-9' } };
        return { status: 404, body: {} };
      }),
    ),
  );
  const view = renderPage(<ExperimentCreatePage />, { ...ROUTE, permissions: ['experiment.write', 'flag.read'] });
  return { calls, ...view };
}

async function fillGoal(hypothesis = 'Shipping the new checkout increases success') {
  await userEvent.type(screen.getByLabelText(/Flag key/), 'checkout');
  await userEvent.type(screen.getByLabelText(/Hypothesis/), hypothesis);
}

async function advancePastGoal() {
  await fillGoal();
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
  // The flag readiness banner proves the gate ran before the step moved.
  await screen.findByText('Published Flag r7 is available. Variations: control, treatment.');
  await screen.findByText('Step 2 of 5');
}

function wizardError(): string {
  return document.getElementById('x-error')?.textContent ?? '';
}

describe('ExperimentCreatePage wizard', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('validates the goal step in order: flag first, then hypothesis', async () => {
    setup();
    await screen.findByText('Step 1 of 5');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Choose a Flag before continuing.');
    expect(screen.getByText('Step 1 of 5')).toBeTruthy();

    await userEvent.type(screen.getByLabelText(/Flag key/), 'checkout');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Enter the hypothesis before continuing.');
    expect(screen.getByText('Step 1 of 5')).toBeTruthy();
    // No flag fetch fired while validation failed.
    expect(document.getElementById('x-flag-check')?.textContent).toBe('');
  });

  it('blocks step advance when the flag check fails', async () => {
    setup({ flagStatus: 404 });
    await screen.findByText('Step 1 of 5');
    await fillGoal();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText(/The Flag check failed/);
    expect(wizardError()).toBe('A published Flag is required. Review the Flag status below and retry.');
    expect(screen.getByText('Step 1 of 5')).toBeTruthy();
  });

  it('blocks step advance with the unpublished-flag banner and the flag shortcut', async () => {
    setup({ flag: { key: 'checkout' } as FlagListItem });
    await screen.findByText('Step 1 of 5');
    await fillGoal();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText(/Flag checkout has no published revision/);
    expect(document.getElementById('x-open-flag')).toBeTruthy();
    expect(wizardError()).toBe('A published Flag is required. Review the Flag status below and retry.');
  });

  it('walks all five steps, renders the review contract and posts the byte-exact draft body', async () => {
    const { calls } = setup();
    await screen.findByText('Step 1 of 5');
    await advancePastGoal();

    // Split step: break the proportions, get the sum error, fix, advance.
    const props = document.querySelectorAll('.v-prop');
    await userEvent.clear(props[1] as HTMLElement);
    await userEvent.type(props[1] as HTMLElement, '60');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Proportions must sum to 100 (got 110)');
    expect(screen.getByText('Step 2 of 5')).toBeTruthy();
    await userEvent.clear(props[1] as HTMLElement);
    await userEvent.type(props[1] as HTMLElement, '50');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('Step 3 of 5');

    // Metrics step: fill the inline primary metric.
    await userEvent.type(screen.getByLabelText(/Metric key/), 'checkout_success');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('Step 4 of 5');

    // Exposure step: the choice is forced.
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Choose when a decision becomes a real exposure before continuing.');
    await userEvent.selectOptions(screen.getByLabelText(/Exposure timing/), 'onDecision');
    expect(screen.getByText('Use only when every successful decision applies the feature immediately.')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('Step 5 of 5');

    // Review contract.
    const review = document.getElementById('x-review') as HTMLElement;
    expect(within(review).getByText('Experiment contract')).toBeTruthy();
    expect(within(review).getByText('checkout')).toBeTruthy();
    expect(within(review).getByText('r7')).toBeTruthy();
    expect(within(review).getByText('Shipping the new checkout increases success')).toBeTruthy();
    expect(within(review).getByText('user_id')).toBeTruthy();
    expect(within(review).getByText('control 50% · treatment 50%')).toBeTruthy();
    expect(within(review).getByText('checkout_success')).toBeTruthy();
    expect(within(review).getByText('7 days')).toBeTruthy();
    expect(within(review).getByText('Minimum 1000 exposed units per arm and 168 hours before a decision.')).toBeTruthy();
    expect(
      within(review).getByText('Creating this experiment saves a draft. Review, approval, binding, and start remain separate actions.'),
    ).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Create experiment' }));
    await screen.findByText('Experiment created: exp-9');
    const post = calls.find(c => c.method === 'POST' && c.path === SCOPE + '/experiments');
    expect(post?.rawBody).toBe(
      '{"flagKey":"checkout","unit":"user_id","variants":["control","treatment"],' +
        '"proportions":{"control":50,"treatment":50},' +
        '"primaryMetric":{"key":"checkout_success","type":"proportion","direction":"increase"},' +
        '"guardrails":[],"alpha":0.05,"power":0.8,"mde":0.01,"minSamplePerArm":1000,' +
        '"minRuntime":604800000000000,"analysisWindow":604800000000000,' +
        '"hypothesis":"Shipping the new checkout increases success","exposureMode":"onDecision",' +
        '"allocationMode":"fixed-split"}',
    );
    // Creating navigates to the new experiment's detail view.
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments/exp-9');
  });

  it('requires trigger point and owner for onRender and puts both in the body', async () => {
    const { calls } = setup();
    await screen.findByText('Step 1 of 5');
    await advancePastGoal();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' })); // split ok (defaults)
    await screen.findByText('Step 3 of 5');
    await userEvent.type(screen.getByLabelText(/Metric key/), 'checkout_success');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('Step 4 of 5');

    await userEvent.selectOptions(screen.getByLabelText(/Exposure timing/), 'onRender');
    expect(screen.getByText(/Your service must report exposure after the feature is actually shown/)).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Describe the exact point that reports a real exposure.');
    await userEvent.type(screen.getByLabelText(/Exposure trigger point/), 'checkout UI shown');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(wizardError()).toBe('Name the team or person responsible for exposure tracking.');
    await userEvent.type(screen.getByLabelText(/Integration owner/), 'growth-frontend');
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('Step 5 of 5');

    // The review contract lists the registration pair.
    const review = document.getElementById('x-review') as HTMLElement;
    expect(within(review).getByText('checkout UI shown')).toBeTruthy();
    expect(within(review).getByText('growth-frontend')).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Create experiment' }));
    await screen.findByText('Experiment created: exp-9');
    const post = calls.find(c => c.method === 'POST' && c.path === SCOPE + '/experiments');
    expect(post?.rawBody).toContain('"exposureMode":"onRender"');
    expect(post?.rawBody).toContain('"exposureTrigger":"checkout UI shown","integrationOwner":"growth-frontend"');
  });

  it('intercepts leaving with the wizard-discard dialog; cancel stays, confirm leaves', async () => {
    setup();
    await screen.findByText('Step 1 of 5');
    await userEvent.type(screen.getByLabelText(/Flag key/), 'checkout');

    await userEvent.click(screen.getByRole('button', { name: '← Experiments' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Discard the unfinished experiment setup?')).toBeTruthy();

    // Cancel: the wizard stays put.
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByText('New experiment')).toBeTruthy();
    expect(screen.queryByTestId('location')).toBeNull();

    // Confirm: navigation proceeds to the experiments list.
    await userEvent.click(screen.getByRole('button', { name: '← Experiments' }));
    const dialog2 = await screen.findByRole('dialog');
    await userEvent.click(within(dialog2).getByRole('button', { name: 'OK' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments');
  });
});
