import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SegmentsPage } from '@saker/grayling-pages';
import type { SegmentDraft } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const SEGMENTS: SegmentDraft[] = [
  {
    key: 'beta-users',
    state: 'editing',
    risk: 'low',
    updatedBy: 'alice',
    updatedAt: '2026-01-02T10:00:00Z',
    draft: { content: { displayName: 'Beta users', description: 'cohort for beta rollouts' } },
  },
  {
    key: 'internal-net',
    state: 'approved',
    updatedBy: 'bob',
    updatedAt: '2026-01-03T10:00:00Z',
    current: { content: { displayName: 'Internal network' } },
  },
];

function setup(segments: SegmentDraft[] = SEGMENTS) {
  const calls: string[] = [];
  const mock = vi.fn(
    fetchMock(url => {
      calls.push(url.pathname + url.search);
      if (url.pathname.endsWith('/segments')) return { body: segments };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

function probe() {
  return screen.queryByTestId('location');
}

describe('SegmentsPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders the draft list with state chips and the draft count', async () => {
    const { calls } = setup();
    renderPage(<SegmentsPage />);
    expect(await screen.findByText('Beta users')).toBeTruthy();
    expect(screen.getByText('Internal network')).toBeTruthy();
    expect(screen.getByText('cohort for beta rollouts')).toBeTruthy();
    expect(screen.getByText('editing').className).toContain('chip blue');
    expect(screen.getByText('approved').className).toContain('chip green');
    expect(screen.getByText('2 draft(s)')).toBeTruthy();
    expect(calls[0]).toBe('/api/v1/admin/workspaces/ws1/environments/prod/segments');
  });

  it('filters rows by the rendered text and shows the no-match hint', async () => {
    setup();
    renderPage(<SegmentsPage />);
    await screen.findByText('Beta users');

    const search = screen.getByLabelText('Filter this list');
    await userEvent.type(search, 'internal');
    expect(screen.queryByText('Beta users')).toBeNull();
    expect(screen.getByText('Internal network')).toBeTruthy();

    await userEvent.clear(search);
    await userEvent.type(search, 'zzz-no-match');
    const noMatch = await screen.findByText('No records match this search.');
    expect(noMatch.hasAttribute('hidden')).toBe(false);
    expect(noMatch.getAttribute('data-list-no-match')).toBe('');

    await userEvent.clear(search);
    await screen.findByText('Beta users');
    expect(screen.getByText('No records match this search.').hasAttribute('hidden')).toBe(true);
  });

  it('navigates to the detail route when a row is clicked', async () => {
    setup();
    renderPage(<SegmentsPage />, { path: '/console/:ws/:env/segments', route: '/console/ws1/prod/segments' });
    await userEvent.click(await screen.findByText('Beta users'));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/segments/beta-users');
    expect(loc.getAttribute('data-state')).toBe('null');
  });

  it('hides the create button without flag.write', async () => {
    setup();
    renderPage(<SegmentsPage />, { permissions: [] });
    await screen.findByText('Beta users');
    expect(screen.queryByRole('button', { name: '+ New segment' })).toBeNull();
  });

  it('creates through the dialog and hands preset and metadata to the detail route', async () => {
    setup();
    renderPage(<SegmentsPage />, {
      permissions: ['flag.write'],
      path: '/console/:ws/:env/segments',
      route: '/console/ws1/prod/segments',
    });
    await userEvent.click(await screen.findByRole('button', { name: '+ New segment' }));

    const dialog = await screen.findByRole('dialog');
    // The FormDialog label text includes the hint — match on the prefix.
    await userEvent.type(within(dialog).getByLabelText(/^Name/), 'Quarterly cohort');
    await userEvent.type(within(dialog).getByLabelText(/New segment key/), 'quarterly-cohort');
    await userEvent.selectOptions(within(dialog).getByLabelText(/Built-in preset/), 'user-list');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/segments/quarterly-cohort');
    expect(JSON.parse(loc.getAttribute('data-state') || 'null')).toEqual({
      preset: 'user-list',
      metadata: { displayName: 'Quarterly cohort', description: '' },
    });
  });

  it('rejects an invalid key without leaving the dialog', async () => {
    setup();
    renderPage(<SegmentsPage />, { permissions: ['flag.write'] });
    await userEvent.click(await screen.findByRole('button', { name: '+ New segment' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText(/New segment key/), 'bad key!');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(probe()).toBeNull();
  });

  it('renders the empty state when the scope has no drafts', async () => {
    setup([]);
    renderPage(<SegmentsPage />);
    expect(await screen.findByText('No segment drafts.')).toBeTruthy();
    expect(screen.getByText('0 draft(s)')).toBeTruthy();
  });
});
