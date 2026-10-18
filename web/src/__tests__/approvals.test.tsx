// Tests for the approvals views (phase 2): the inbox list with its
// pending/processed/all filter and actionability rules, and the detail view
// with facts, the before/after comparison, and the review/reject/execute
// flows including the post-confirmation re-read guards. The pages are not
// re-exported from @saker/grayling-pages yet (the orchestrator wires the
// index), so the tests import them by subpath.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ApprovalsPage } from '@saker/grayling-pages/pages/ApprovalsPage';
import { ApprovalDetailPage } from '@saker/grayling-pages/pages/ApprovalDetailPage';
import type { ApprovalComparison, ApprovalItem } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

// Submitted by alice; actionable as 'review' for an operator with
// flag.approve.
const AP1: ApprovalItem = {
  id: 'ap1',
  action: 'flag.publish',
  object: 'new-checkout',
  state: 'submitted',
  environmentId: 'prod',
  submittedBy: 'alice',
  submittedAt: '2026-01-02T10:00:00Z',
  risk: 'high',
  impact: '12% of traffic',
  reason: 'roll out the new checkout flow',
  baseRevision: 7,
  rollbackTarget: 'r6',
  guardrails: 'error budget holds',
  payloadHash: 'hash-ap1',
  payload: '{"revision": 3}',
  expiresAt: '2030-01-01T00:00:00Z',
};

const AP2: ApprovalItem = {
  id: 'ap2',
  action: 'flag.rollback',
  object: 'old-flag',
  state: 'approved',
  submittedBy: 'bob',
  submittedAt: '2026-01-03T10:00:00Z',
  expiresAt: '2030-01-01T00:00:00Z',
};

const AP3: ApprovalItem = {
  id: 'ap3',
  action: 'flag.publish',
  object: 'dead-flag',
  state: 'rejected',
  submittedBy: 'carol',
  submittedAt: '2026-01-04T10:00:00Z',
};

// Approved flag.rollback, executable by an operator with flag.publish.
const AP8: ApprovalItem = {
  id: 'ap8',
  action: 'flag.rollback',
  object: 'new-checkout',
  state: 'approved',
  environmentId: 'prod',
  submittedBy: 'alice',
  submittedAt: '2026-01-02T10:00:00Z',
  reason: 'rollback after incident',
  baseRevision: 8,
  payload: '{"revision": 3}',
  payloadHash: 'hash-ap8',
  expiresAt: '2030-01-01T00:00:00Z',
  approvedBy: 'operator',
};

const COMPARISON: ApprovalComparison = {
  available: true,
  stale: false,
  beforeRevision: 7,
  afterRevision: 7,
  before: { type: 'bool', disable: false },
  after: { type: 'bool', disable: true },
};

interface Call {
  url: string;
  method: string;
  body: unknown;
}

function record(calls: Call[], url: URL, init: RequestInit): void {
  calls.push({
    url: url.pathname,
    method: init.method || 'GET',
    body: init.body ? JSON.parse(String(init.body)) : undefined,
  });
}

function setupList(approvals: ApprovalItem[]) {
  const calls: Call[] = [];
  const mock = vi.fn(
    fetchMock((url, init) => {
      record(calls, url, init);
      if (url.pathname.endsWith('/approvals')) return { body: approvals };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

function setupDetail(
  options: {
    comparison?: ApprovalComparison;
    onList?: (readCount: number) => ApprovalItem[];
  } = {},
) {
  const calls: Call[] = [];
  let listReads = 0;
  const mock = vi.fn(
    fetchMock((url, init) => {
      const method = init.method || 'GET';
      record(calls, url, init);
      if (url.pathname.endsWith('/comparison')) return { body: options.comparison ?? COMPARISON };
      if (url.pathname.endsWith('/approvals') && method === 'GET') {
        listReads += 1;
        return { body: options.onList ? options.onList(listReads) : [AP1] };
      }
      if (method === 'POST') return { body: {} };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

function renderDetail(permissions: string[] = ['flag.approve'], id = 'ap1') {
  return renderPage(<ApprovalDetailPage />, {
    path: 'console/:ws/:env/approvals/:approvalID',
    route: '/console/ws1/prod/approvals/' + id,
    permissions,
  });
}

beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

describe('ApprovalsPage', () => {
  it('renders the inbox with filter counts, filters the rows and navigates to the detail route', async () => {
    setupList([AP1, AP2, AP3]);
    renderPage(<ApprovalsPage />, {
      path: '/console/:ws/:env/approvals',
      route: '/console/ws1/prod/approvals',
      permissions: ['flag.approve'],
    });
    expect(await screen.findByText('Approval and emergency review inbox')).toBeTruthy();
    const pendingButton = screen.getByRole('button', { name: 'Waiting for me (1)' });
    expect(screen.getByRole('button', { name: 'Processed (2)' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'All (3)' })).toBeTruthy();
    // The default filter is pending: only the actionable item is listed.
    expect(pendingButton.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByText('flag.publish · new-checkout')).toBeTruthy();
    expect(screen.queryByText('flag.rollback · old-flag')).toBeNull();
    expect(screen.queryByText('flag.publish · dead-flag')).toBeNull();

    await userEvent.click(screen.getByRole('button', { name: 'All (3)' }));
    expect(screen.getByText('flag.rollback · old-flag')).toBeTruthy();
    expect(screen.getByText('flag.publish · dead-flag')).toBeTruthy();

    await userEvent.click(screen.getByText('flag.publish · new-checkout'));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/approvals/ap1');
  });

  it('lists a submitted request from another user under pending', async () => {
    setupList([AP1]);
    renderPage(<ApprovalsPage />, { permissions: ['flag.approve'] });
    expect(await screen.findByText('flag.publish · new-checkout')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Waiting for me (1)' })).toBeTruthy();
  });

  it('lists the operator own submitted request (self-approval is allowed)', async () => {
    setupList([{ ...AP1, id: 'ap-self', submittedBy: 'operator' }]);
    renderPage(<ApprovalsPage />, { permissions: ['flag.approve'] });
    expect(await screen.findByText('flag.publish · new-checkout')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Waiting for me (1)' })).toBeTruthy();
  });

  it('hides the operator own break-glass post-review from pending', async () => {
    setupList([{ ...AP1, id: 'ap-bg', action: 'breakglass.review', submittedBy: 'operator' }]);
    renderPage(<ApprovalsPage />, { permissions: ['flag.approve'] });
    expect(await screen.findByText('Nothing needs your action right now.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Waiting for me (0)' })).toBeTruthy();
  });
});

describe('ApprovalDetailPage', () => {
  it('renders the facts, the changed-field diff and the pretty-printed payload', async () => {
    setupDetail();
    renderDetail();
    expect(await screen.findByText('roll out the new checkout flow')).toBeTruthy();
    expect(screen.getByText('12% of traffic')).toBeTruthy();
    expect(screen.getByText('r6')).toBeTruthy();
    expect(screen.getByText('error budget holds')).toBeTruthy();
    expect(screen.getByText('hash-ap1')).toBeTruthy();

    // Only the changed field appears in the comparison diff.
    expect(await screen.findByText('Kill switch')).toBeTruthy();
    expect(screen.queryByText('Value type')).toBeNull();
    const item = screen.getByText('Kill switch').closest('.flag-diff-item') as HTMLElement;
    expect(within(item).getByText('false')).toBeTruthy();
    expect(within(item).getByText('true')).toBeTruthy();

    expect(screen.getByText('Proposed operation input')).toBeTruthy();
    expect(screen.getByText(/"revision": 3/)).toBeTruthy();
  });

  it('requires a review note before any request is sent', async () => {
    const { calls } = setupDetail();
    renderDetail();
    await userEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    expect(await screen.findByText('Enter a review note before deciding.')).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(calls.some(c => c.method === 'POST')).toBe(false);
  });

  it('approves with a note after confirmation and returns to the inbox', async () => {
    const { calls } = setupDetail();
    renderDetail();
    await userEvent.type(await screen.findByLabelText('Review note'), 'looks good');
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Approve flag.publish for new-checkout?');
    expect(within(dialog).getByText('Release review preview')).toBeTruthy();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));

    await waitFor(() => expect(calls.some(c => c.method === 'POST' && c.url.endsWith('/approve'))).toBe(true));
    const approveIndex = calls.findIndex(c => c.method === 'POST' && c.url.endsWith('/approve'));
    expect(calls[approveIndex].body).toEqual({ note: 'looks good' });
    // The list and the comparison are re-read after confirmation, before the POST.
    const listReads = calls.map((c, i) => ({ c, i })).filter(x => x.c.method === 'GET' && x.c.url.endsWith('/approvals'));
    expect(listReads.length).toBeGreaterThanOrEqual(2);
    expect(listReads[listReads.length - 1].i).toBeLessThan(approveIndex);
    expect(calls.filter(c => c.method === 'GET' && c.url.endsWith('/comparison')).length).toBeGreaterThanOrEqual(2);

    expect(await screen.findByText('Review recorded')).toBeTruthy();
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/approvals');
  });

  it('rejects with the note as the reason', async () => {
    const { calls } = setupDetail();
    renderDetail();
    await userEvent.type(await screen.findByLabelText('Review note'), 'not safe');
    await userEvent.click(screen.getByRole('button', { name: 'Reject' }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Reject flag.publish for new-checkout?');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Reject' }));

    await waitFor(() => expect(calls.some(c => c.method === 'POST' && c.url.endsWith('/reject'))).toBe(true));
    const reject = calls.find(c => c.method === 'POST' && c.url.endsWith('/reject'));
    expect(reject?.body).toEqual({ reason: 'not safe' });
    expect(await screen.findByText('Approval request rejected')).toBeTruthy();
  });

  it('disables approve and shows the stale banner when the comparison is stale', async () => {
    setupDetail({ comparison: { ...COMPARISON, stale: true } });
    renderDetail();
    const banner = await screen.findByText(
      'The source changed since this approval was requested. This request cannot be approved or executed; create a new request.',
    );
    expect(banner.getAttribute('role')).toBe('alert');
    expect(banner.className).toContain('banner warn');
    expect((screen.getByRole('button', { name: 'Approve' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('blocks the action when the approval changed before submission', async () => {
    const { calls } = setupDetail({
      onList: readCount => (readCount === 1 ? [AP1] : [{ ...AP1, payloadHash: 'hash-changed' }]),
    });
    renderDetail();
    await userEvent.type(await screen.findByLabelText('Review note'), 'looks good');
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));

    expect(
      await screen.findByText('The approval changed before submission. Refresh this page and review it again.'),
    ).toBeTruthy();
    expect(calls.some(c => c.method === 'POST')).toBe(false);
  });

  it('executes a flag.rollback against the environment scope', async () => {
    const { calls } = setupDetail({
      onList: () => [AP8],
      comparison: { available: true, stale: false, beforeRevision: 8, afterRevision: 3, before: {}, after: {} },
    });
    renderDetail(['flag.publish'], 'ap8');
    await userEvent.click(await screen.findByRole('button', { name: 'Execute' }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Execute approved flag.rollback for new-checkout?');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Execute' }));

    await waitFor(() =>
      expect(calls.some(c => c.method === 'POST' && c.url.endsWith('/flags/new-checkout/rollback'))).toBe(true),
    );
    const post = calls.find(c => c.method === 'POST' && c.url.endsWith('/flags/new-checkout/rollback'));
    expect(post?.url).toBe('/api/v1/admin/workspaces/ws1/environments/prod/flags/new-checkout/rollback');
    expect(post?.body).toEqual({ revision: 3, reason: 'rollback after incident', approvalId: 'ap8' });
    expect(await screen.findByText('Approved action executed')).toBeTruthy();
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/approvals');
  });
});
