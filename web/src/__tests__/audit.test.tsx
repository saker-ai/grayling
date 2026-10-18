import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AuditPage } from '@saker/grayling-pages';
import type { AuditEntry } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

function entries(n: number, start = 1): AuditEntry[] {
  return Array.from({ length: n }, (_, i) => ({
    id: 'evt-' + (start + i),
    time: '2026-01-0' + ((i % 9) + 1) + 'T10:00:00Z',
    actor: 'admin' + ((start + i) % 3),
    action: i % 2 ? 'flag.publish' : 'custom.action',
    object: 'flag/flag-' + (start + i),
    environmentId: 'prod',
    detail: { revision: start + i },
  }));
}

// auditMock routes the audit endpoint through `pages`: pages[0] serves the
// cursor-less request, pages[1] serves before=evt-50 (the cursor page 1
// produces), and every call is recorded for URL assertions.
function auditMock(pages: AuditEntry[][]) {
  const calls: string[] = [];
  const mock = vi.fn(
    fetchMock(url => {
      if (!url.pathname.endsWith('/audit')) return { status: 404, body: {} };
      calls.push(url.search);
      const before = url.searchParams.get('before') || '';
      const idx = before === '' ? 0 : before === 'evt-50' ? 1 : pages.length - 1;
      return { body: pages[Math.min(idx, pages.length - 1)] };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

const ROUTE = { path: '/console/:ws/:env/audit/:object?', route: '/console/ws1/prod/audit' };

describe('AuditPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('fetches limit+1 rows and renders the table', async () => {
    const { calls } = auditMock([entries(51)]);
    renderPage(<AuditPage />, ROUTE);
    expect((await screen.findAllByText('flag/flag-1'))[0]).toBeTruthy();
    expect(calls[0]).toContain('limit=51');
    expect(calls[0]).toContain('q=');
    expect(calls[0]).not.toContain('before=');
    // 51 rows returned, page size 50: row 51 is held back, next enabled.
    expect(screen.queryByText('flag/flag-51')).toBeNull();
    expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Previous' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText('Page 1')).toBeTruthy();
  });

  it('walks forward and back through the cursor stack', async () => {
    const { calls } = auditMock([entries(51), entries(10, 51)]);
    renderPage(<AuditPage />, ROUTE);
    await screen.findAllByText('flag/flag-1');

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findAllByText('flag/flag-51');
    expect(calls[1]).toContain('before=evt-50');
    expect(screen.getByText('Page 2')).toBeTruthy();
    // Page 2 returned 10 (< 51): no further page.
    expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Previous' }) as HTMLButtonElement).disabled).toBe(false);

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findAllByText('flag/flag-1');
    expect(screen.getByText('Page 1')).toBeTruthy();
    expect(calls[2]).not.toContain('before=');
  });

  it('applies the search query and restarts pagination', async () => {
    const { calls } = auditMock([entries(3)]);
    renderPage(<AuditPage />, ROUTE);
    await screen.findAllByText('flag/flag-1');
    await userEvent.type(screen.getByLabelText(/Search actor/), 'flag.publish');
    await userEvent.click(screen.getByRole('button', { name: 'Search' }));
    await waitFor(() => expect(calls.at(-1)).toContain('q=flag.publish'));
    expect(calls.at(-1)).not.toContain('before=');
  });

  it('honors the object filter deep link and clears it back to the plain view', async () => {
    const { calls } = auditMock([entries(2)]);
    renderPage(<AuditPage />, {
      path: '/console/:ws/:env/audit/:object?',
      route: '/console/ws1/prod/audit/' + encodeURIComponent('flag/my-flag'),
    });
    expect(await screen.findByText(/Object: flag\/my-flag/)).toBeTruthy();
    expect(calls[0]).toContain('object=' + encodeURIComponent('flag/my-flag'));

    await userEvent.click(screen.getByRole('button', { name: 'Show all objects' }));
    // The deep link is gone; the bare audit route renders (LocationProbe is
    // the catch-all, so assert the fetch re-fires without object= instead).
    await waitFor(() => expect(calls.at(-1)).not.toContain('object='));
  });

  it('renders the inline raw-action disclosure per row', async () => {
    auditMock([entries(2)]);
    renderPage(<AuditPage />, ROUTE);
    await screen.findAllByText('flag/flag-1');
    const summaries = screen.getAllByText('Technical details');
    expect(summaries.length).toBe(4);
    expect(summaries[0].closest('details')).toBeTruthy();
    expect(summaries[0].closest('details')?.querySelector('code')?.textContent).toBe('custom.action');
    expect(summaries[1].closest('details')?.querySelector('code')?.textContent).toBe('flag.publish');
  });

  it('switches the page size and restarts from page 1', async () => {
    const { calls } = auditMock([entries(30)]);
    renderPage(<AuditPage />, ROUTE);
    await screen.findAllByText('flag/flag-1');
    await userEvent.selectOptions(screen.getByLabelText(/limit/), '25');
    await waitFor(() => expect(calls.at(-1)).toContain('limit=26'));
  });
});
