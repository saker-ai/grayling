// a11y.test.tsx runs axe (the WCAG 2.x A/AA rule set pinned in helpers) over
// every component R1 touched: the Dialog overlay (trap + per-instance title
// id), the ZoneNav APG tabs, the Skeleton loading block, the rollout zone
// with its progressbar, and a DataTable with scoped column headers.
import { afterEach, describe, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Dialog } from '@saker/grayling-pages/components/Dialog';
import { DataTable } from '@saker/grayling-pages/components/DataTable';
import { Skeleton } from '@saker/grayling-pages/components/Skeleton';
import { ZoneNav } from '@saker/grayling-pages/workbench/ZoneNav';
import { ExperimentDetailPage } from '@saker/grayling-pages/pages/ExperimentDetailPage';
import type { ExperimentDetail, ReleasePlan } from '@saker/grayling-client';
import { expectNoA11yViolations, fetchMock, renderPage } from './helpers';

const NS_HOUR = 3600000000000;

const RUNNING_PLAN: ReleasePlan = {
  steps: [
    { name: 'canary', ratio: 5, minRuntime: 2 * NS_HOUR, observationWindow: NS_HOUR, autoAdvance: true },
    { name: 'full', ratio: 100, minRuntime: 4 * NS_HOUR, observationWindow: 0, autoAdvance: false },
  ],
  state: 'running',
  currentStep: 0,
  stepStartedAt: '2026-01-05T00:00:00Z',
  createdBy: 'alice',
  updatedAt: '2026-01-05T00:00:00Z',
  history: [{ at: '2026-01-02T00:00:00Z', actor: 'operator', action: 'create', step: 0 }],
};

const EXP: ExperimentDetail = {
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
  releasePlan: RUNNING_PLAN,
};

describe('axe: R1 components', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('Dialog with fields, a link and a footer has no violations', async () => {
    render(
      <Dialog
        title="Confirm action"
        onClose={() => {}}
        footer={<button type="button">Cancel</button>}
      >
        <label>
          Reason
          <input />
        </label>
        <a href="#details">Details</a>
      </Dialog>,
    );
    // Dialog portals into document.body — axe the body, not the container.
    await expectNoA11yViolations(document.body);
  });

  it('ZoneNav tabs have no violations', async () => {
    // Standalone render includes the active tab's panel: axe flags a dangling
    // aria-controls on the focusable tab (on the real page ExperimentDetail-
    // Page always renders it).
    const { container } = render(
      <>
        <ZoneNav active="summary" onSelect={() => {}} />
        <div role="tabpanel" id="exp-zonepanel-summary" aria-labelledby="exp-tab-summary" />
      </>,
    );
    await expectNoA11yViolations(container);
  });

  it('Skeleton announces loading without violations', async () => {
    const { container } = render(<Skeleton lines={4} />);
    await expectNoA11yViolations(container);
  });

  it('the rollout zone with its progressbar has no violations', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        fetchMock(url => {
          if (url.pathname === '/api/v1/admin/experiments/exp-1') return { body: EXP };
          if (url.pathname.endsWith('/report')) return { status: 404, body: {} };
          return { status: 404, body: {} };
        }),
      ),
    );
    renderPage(<ExperimentDetailPage />, {
      path: '/console/:ws/:env/experiments/:expId/:tab',
      route: '/console/ws1/prod/experiments/exp-1/rollout',
      permissions: [],
    });
    await screen.findByRole('progressbar', { name: 'Rollout progress' });
    await expectNoA11yViolations(document.body);
  });

  it('DataTable with scoped headers has no violations', async () => {
    const { container } = render(
      <DataTable
        columns={[
          { key: 'name', header: 'Name', cell: (r: { name: string }) => r.name },
          { key: 'n', header: 'Count', cell: (r: { name: string }) => r.name.length },
        ]}
        rows={[{ name: 'alpha' }, { name: 'beta' }]}
        rowKey={r => r.name}
        emptyText="none"
        ariaLabel="demo table"
      />,
    );
    await expectNoA11yViolations(container);
  });
});
