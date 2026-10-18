// flagsList.test.tsx pins FlagsPage (web-pages/src/pages/FlagsPage.tsx), the
// React port of legacy viewFlags: the status-category filter, the substring
// search over key/displayName/description, the legacy sort (running-change
// first, then workflow state, newest first), the client-side 25-per-page
// pagination, row drill-down, the flag.write capability gate and the create
// dialog handing { preset, metadata } to the detail route.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FlagsPage } from '@saker/grayling-pages/pages/FlagsPage';
import type { FlagListItem } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const ROUTE = { path: '/console/:ws/:env/flags', route: '/console/ws1/prod/flags' };

function flag(partial: Partial<FlagListItem> & { key: string }): FlagListItem {
  return partial as FlagListItem;
}

const FLAGS: FlagListItem[] = [
  flag({
    key: 'published-only',
    current: {
      revision: 3,
      publishedAt: '2026-01-01T09:00:00Z',
      content: { displayName: 'Published only', type: 'bool' },
    },
    currentRevision: 3,
  }),
  flag({
    key: 'editing-flag',
    draft: {
      state: 'editing',
      updatedAt: '2026-01-02T10:00:00Z',
      content: { displayName: 'Editing flag', type: 'string', description: 'local experiment copy' },
    },
  }),
  flag({
    key: 'approved-flag',
    draft: { state: 'approved', updatedAt: '2026-01-03T10:00:00Z', content: { displayName: 'Approved flag', type: 'bool' } },
  }),
  flag({
    key: 'submitted-flag',
    draft: { state: 'submitted', updatedAt: '2026-01-04T10:00:00Z', content: { displayName: 'Submitted flag', type: 'bool' } },
  }),
  flag({
    key: 'running-change',
    draft: { state: 'editing', risk: 'high', updatedAt: '2026-01-01T10:00:00Z', content: { displayName: 'Risky change', type: 'bool' } },
    current: {
      revision: 7,
      publishedAt: '2025-12-31T09:00:00Z',
      content: { displayName: 'Risky change', type: 'bool', experiment: { state: 'running' } },
    },
    currentRevision: 7,
  }),
];

function setup(flags: FlagListItem[] = FLAGS, permissions: string[] = []) {
  const calls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      fetchMock(url => {
        calls.push(url.pathname + url.search);
        if (url.pathname.endsWith('/flags')) return { body: flags };
        return { status: 404, body: {} };
      }),
    ),
  );
  return { calls, permissions };
}

function recordKeys(): string[] {
  return Array.from(document.querySelectorAll('[data-flag-record]')).map(row => row.getAttribute('data-key') || '');
}

describe('FlagsPage', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('renders the legacy sort: running change first, then submitted/approved/editing/published', async () => {
    const { calls } = setup();
    renderPage(<FlagsPage />, ROUTE);
    await screen.findByText('Risky change');
    expect(recordKeys()).toEqual(['running-change', 'submitted-flag', 'approved-flag', 'editing-flag', 'published-only']);
    expect(screen.getByText('5 flag(s)')).toBeTruthy();
    expect(screen.getByText('High-risk draft on a running experiment').className).toContain('chip red');
    expect(screen.getByText('published').className).toContain('chip green');
    expect(calls[0]).toBe('/api/v1/admin/workspaces/ws1/environments/prod/flags');
  });

  it('filters by status category and shows the dedicated no-match hint', async () => {
    setup();
    renderPage(<FlagsPage />, ROUTE);
    await screen.findByText('Risky change');

    await userEvent.selectOptions(screen.getByLabelText(/^Status/), 'published');
    expect(recordKeys()).toEqual(['published-only']);

    await userEvent.selectOptions(screen.getByLabelText(/^Status/), 'submitted');
    expect(recordKeys()).toEqual(['submitted-flag']);
    expect(screen.getByText('Submitted flag')).toBeTruthy();

    await userEvent.selectOptions(screen.getByLabelText(/^Status/), 'approved');
    expect(recordKeys()).toEqual(['approved-flag']);
  });

  it('shows #flag-filter-empty when the query leaves zero rows', async () => {
    setup();
    renderPage(<FlagsPage />, ROUTE);
    await screen.findByText('Risky change');
    const search = screen.getByLabelText('Filter this list');
    await userEvent.type(search, 'zzz-no-match');
    const empty = document.getElementById('flag-filter-empty');
    expect(empty?.textContent).toBe('No flags match these filters.');
    expect(empty?.hasAttribute('hidden')).toBe(false);
    expect(recordKeys()).toEqual([]);
  });

  it('searches key, display name and description', async () => {
    setup();
    renderPage(<FlagsPage />, ROUTE);
    await screen.findByText('Risky change');
    const search = screen.getByLabelText('Filter this list');
    await userEvent.type(search, 'local experiment copy'); // description only
    expect(recordKeys()).toEqual(['editing-flag']);
  });

  it('paginates client-side at 25 rows per page', async () => {
    const many: FlagListItem[] = Array.from({ length: 26 }, (_, i) =>
      flag({
        key: 'flag-' + String(i).padStart(2, '0'),
        draft: {
          state: 'editing',
          updatedAt: new Date(Date.UTC(2026, 0, 1, 0, i)).toISOString(), // newest last in the array
          content: { displayName: 'Flag ' + i, type: 'bool' },
        },
      }),
    );
    setup(many);
    renderPage(<FlagsPage />, ROUTE);
    await screen.findByText('Flag 25');
    expect(recordKeys()).toHaveLength(25);
    expect(screen.getByText('Page 1 of 2 · 26 matches')).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(recordKeys()).toEqual(['flag-00']); // newest first → the oldest lands alone on page 2
    expect(screen.getByText('Page 2 of 2 · 26 matches')).toBeTruthy();

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    expect(recordKeys()).toHaveLength(25);
  });

  it('gates the create button on flag.write', async () => {
    setup(FLAGS, []);
    renderPage(<FlagsPage />, { ...ROUTE, permissions: [] });
    await screen.findByText('Risky change');
    expect(document.getElementById('flag-new')).toBeNull();
  });

  it('creates through the dialog and hands preset and metadata to the detail route', async () => {
    setup();
    renderPage(<FlagsPage />, { ...ROUTE, permissions: ['flag.write'] });
    await userEvent.click(await screen.findByRole('button', { name: '+ New flag' }));

    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText(/^Name/), 'Office network');
    await userEvent.type(within(dialog).getByLabelText(/New flag key/), 'office-network');
    const preset = within(dialog).getByLabelText(/Built-in preset/);
    const firstPreset = (preset as HTMLSelectElement).options[1]?.value;
    await userEvent.selectOptions(preset, firstPreset);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }));

    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/flags/office-network');
    expect(JSON.parse(loc.getAttribute('data-state') || 'null')).toEqual({
      preset: firstPreset,
      metadata: { displayName: 'Office network', description: '' },
    });
  });

  it('rejects an invalid key without leaving the dialog', async () => {
    setup();
    renderPage(<FlagsPage />, { ...ROUTE, permissions: ['flag.write'] });
    await userEvent.click(await screen.findByRole('button', { name: '+ New flag' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(within(dialog).getByLabelText(/New flag key/), 'bad key!');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.queryByTestId('location')).toBeNull();
  });

  it('renders the empty state without pagination when the scope has no flags', async () => {
    setup([]);
    renderPage(<FlagsPage />, ROUTE);
    expect(await screen.findByText('No flags yet — create one.')).toBeTruthy();
    expect(screen.getByText('0 flag(s)')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Next' })).toBeNull();
    expect(document.getElementById('flag-filter-empty')?.hasAttribute('hidden')).toBe(true);
  });
});
