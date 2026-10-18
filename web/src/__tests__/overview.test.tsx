import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { OverviewPage } from '@saker/grayling-pages/pages/OverviewPage';
import type { OverviewResponse } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';
import type { MockResponse } from './helpers';

const FULL: OverviewResponse = {
  risks: {
    status: 'ok',
    coverage: { inspected: 20, total: 20, complete: true },
    items: [
      { id: 'exp-7', kind: 'experiment_paused', severity: 'warn', environmentId: 'prod', text: 'paused for 3 days' },
      { id: 'exp-9', kind: 'experiment_report_unavailable', severity: 'block' },
    ],
  },
  counts: { pausedExperiments: 2, runningExperiments: 5, pendingApprovals: 1, publishedFlags: 42 },
  pendingApprovals: {
    status: 'ok',
    actionableTotal: 1,
    items: [
      {
        id: 'ap-1',
        action: 'flag.publish',
        object: 'new-checkout',
        environmentId: 'prod',
        submittedBy: 'alice',
        submittedAt: '2026-01-02T10:00:00Z',
        risk: 'high',
        expiresAt: '2026-02-01T10:00:00Z',
        myAction: 'review',
      },
    ],
  },
  recentReleases: {
    status: 'ok',
    coverage: { inspected: 20, complete: true },
    items: [
      { time: '2026-01-03T10:00:00Z', action: 'flag.publish', object: 'new-checkout', actor: 'alice', environmentId: 'prod' },
    ],
  },
};

function setup(handler: (url: URL, callIndex: number) => MockResponse) {
  const calls: string[] = [];
  let n = 0;
  const mock = vi.fn(
    fetchMock(url => {
      calls.push(url.pathname + url.search);
      return handler(url, n++);
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

const ok = (body: OverviewResponse): MockResponse => ({ body });

const ROUTE = { path: '/console/:ws/:env/overview', route: '/console/ws1/prod/overview' };

describe('OverviewPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders all four modules from a full payload', async () => {
    const { calls } = setup(() => ok(FULL));
    const { container } = renderPage(<OverviewPage />, ROUTE);

    expect(await screen.findByText('exp-7')).toBeTruthy();
    expect(screen.getByRole('heading', { name: 'Workspace overview' })).toBeTruthy();
    expect(screen.getByText('Current environment: ws1 / prod')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Refresh status' })).toBeTruthy();
    expect(calls[0]).toBe('/api/v1/admin/workspaces/ws1/overview?limit=20&env=prod');

    // Risk band: severity chips, kind labels, environment and free text.
    expect(screen.getByText('Warning').className).toContain('chip amber');
    expect(screen.getByText('Blocking').className).toContain('chip red');
    expect(screen.getByText('Experiment paused', { exact: false })).toBeTruthy();
    expect(screen.getByText('Experiment report unavailable; check its data source', { exact: false })).toBeTruthy();
    expect(screen.getByText('paused for 3 days')).toBeTruthy();

    // Stats grid: real counts under their labels.
    const stats = within(container.querySelector('.overview-grid') as HTMLElement);
    expect(stats.getByText('Paused experiments')).toBeTruthy();
    expect(stats.getByText('2')).toBeTruthy();
    expect(stats.getByText('Running experiments')).toBeTruthy();
    expect(stats.getByText('5')).toBeTruthy();
    expect(stats.getByText('Pending approvals')).toBeTruthy();
    expect(stats.getByText('1')).toBeTruthy();
    expect(stats.getByText('Published flags')).toBeTruthy();
    expect(stats.getByText('42')).toBeTruthy();

    // Pending approvals: actionable total chip, row link, myAction chip, risk note.
    expect(screen.getByText('1 waiting for you').className).toContain('chip amber');
    expect(screen.getByRole('button', { name: /Publish strategy · new-checkout/ })).toBeTruthy();
    expect(screen.getByText('Waiting for my review').className).toContain('chip amber');
    expect(screen.getByText(/Risk: High risk/)).toBeTruthy();

    // Recent releases: action chip, scan-window coverage line.
    expect(screen.getByText('Flag published').className).toContain('chip green');
    expect(screen.getByText('Scanned the latest 20 audit records.')).toBeTruthy();

    // Coverage card: complete variant, no drill-down button.
    expect(
      screen.getByText('Reviewed the status and available reports for all 20 experiments in this environment.'),
    ).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Review all experiments' })).toBeNull();
  });

  it('navigates from a risk item to the experiment detail', async () => {
    setup(() => ok(FULL));
    renderPage(<OverviewPage />, ROUTE);
    const [open] = await screen.findAllByRole('button', { name: 'Open experiment' });
    await userEvent.click(open);
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments/exp-7');
  });

  it('navigates from a pending approval row to the approval detail', async () => {
    setup(() => ok(FULL));
    renderPage(<OverviewPage />, ROUTE);
    await userEvent.click(await screen.findByRole('button', { name: /Publish strategy/ }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/approvals/ap-1');
  });

  it('navigates from the open button of a pending approval to the approval detail', async () => {
    setup(() => ok(FULL));
    renderPage(<OverviewPage />, ROUTE);
    await userEvent.click(await screen.findByRole('button', { name: 'Open approval' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/approvals/ap-1');
  });

  it('navigates from a release row to the audit view filtered by object', async () => {
    setup(() => ok(FULL));
    renderPage(<OverviewPage />, ROUTE);
    await userEvent.click(await screen.findByRole('button', { name: 'View in audit log' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/audit/new-checkout');
  });

  it('renders em dashes for all four stats when counts are null', async () => {
    setup(() => ok({ ...FULL, counts: null }));
    const { container } = renderPage(<OverviewPage />, ROUTE);
    await screen.findByText('exp-7');
    const values = [...container.querySelectorAll('.overview-grid strong')].map(el => el.textContent);
    expect(values).toEqual(['—', '—', '—', '—']);
  });

  it('shows a failure banner with retry per degraded module and recovers on retry', async () => {
    const degraded: OverviewResponse = {
      risks: { status: 'unavailable', reason: 'report backend down', coverage: { inspected: 3, total: 20, complete: false } },
      counts: null,
      pendingApprovals: { status: 'error', reason: 'permission denied' },
      // recentReleases missing entirely
    };
    const { mock } = setup((_url, n) => (n === 0 ? ok(degraded) : ok(FULL)));
    const { container } = renderPage(<OverviewPage />, ROUTE);

    expect(
      await screen.findByText('Risk band is unavailable: report backend down. Other modules are unaffected — retry to refresh.'),
    ).toBeTruthy();
    expect(
      screen.getByText('Approvals waiting for me is unavailable: permission denied. Other modules are unaffected — retry to refresh.'),
    ).toBeTruthy();
    expect(
      screen.getByText('Recent releases is unavailable: —. Other modules are unaffected — retry to refresh.'),
    ).toBeTruthy();
    expect(screen.getByText('Status: unavailable · Inspected 3 of 20')).toBeTruthy();
    expect(container.querySelectorAll('.banner.warn[role="status"]').length).toBe(3);

    await userEvent.click(screen.getAllByRole('button', { name: 'Retry' })[0]);
    expect(await screen.findByText('exp-7')).toBeTruthy();
    expect(mock).toHaveBeenCalledTimes(2);
  });

  it('uses partial-coverage wording when the risk report is incomplete', async () => {
    const partial: OverviewResponse = {
      ...FULL,
      risks: { status: 'ok', coverage: { inspected: 5, total: 20, complete: false }, items: [] },
    };
    setup(() => ok(partial));
    renderPage(<OverviewPage />, ROUTE);

    expect(
      await screen.findByText(
        'No blocking risks found in the inspected scope; coverage is incomplete — this is not a guarantee that nothing is wrong.',
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(
        'Reviewed the status and available reports for the 5 most recently updated of 20 experiments. Open the list to review the rest.',
      ),
    ).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Review all experiments' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments');
  });

  it('shows the no-scope banner and skips the fetch when env is empty', async () => {
    const { mock } = setup(() => ok(FULL));
    const { container } = renderPage(<OverviewPage />, { env: '' });
    const banner = container.querySelector('.banner.warn');
    expect(banner?.textContent).toContain('Select a workspace and environment');
    expect(mock).not.toHaveBeenCalled();
  });

  it('shows the error banner and retries after a failed fetch', async () => {
    const { mock } = setup((_url, n) => (n === 0 ? { status: 500, body: {} } : ok(FULL)));
    renderPage(<OverviewPage />, ROUTE);

    expect(await screen.findByRole('alert')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('exp-7')).toBeTruthy();
    expect(mock).toHaveBeenCalledTimes(2);
  });

  it('shows the restricted note when report coverage is restricted', async () => {
    const restricted: OverviewResponse = { ...FULL, risks: { status: 'restricted' } };
    setup(() => ok(restricted));
    renderPage(<OverviewPage />, ROUTE);

    expect(await screen.findByText('Report health is hidden by your permissions.')).toBeTruthy();
    expect(
      screen.getByText(
        'No blocking risks found in the inspected scope; coverage is incomplete — this is not a guarantee that nothing is wrong.',
      ),
    ).toBeTruthy();
    expect(screen.getByText('Report health could not be checked in this view.')).toBeTruthy();
  });
});
