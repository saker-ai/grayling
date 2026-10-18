import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ExperimentsPage } from '@saker/grayling-pages';
import type { ExperimentReportSummary, ExperimentSummaryItem, ExperimentSummaryPage } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

function item(id: string, flagKey: string, state: string, report?: ExperimentReportSummary | null): ExperimentSummaryItem {
  return {
    experiment: { id, flagKey, state, createdBy: 'carol', updatedAt: '2026-01-04T10:00:00Z' },
    report,
  };
}

function items(n: number, start = 1): ExperimentSummaryItem[] {
  return Array.from({ length: n }, (_, i) => item('exp-' + (start + i), 'flag-' + (start + i), 'running', { status: 'ready', healthy: true }));
}

// expMock serves offset-keyed pages from the experiment-summaries endpoint
// and records every query string for assertions.
function expMock(pages: Record<number, ExperimentSummaryPage>) {
  const calls: string[] = [];
  const mock = vi.fn(
    fetchMock(url => {
      if (!url.pathname.endsWith('/experiment-summaries')) return { status: 404, body: {} };
      calls.push(url.search);
      const offset = Number(url.searchParams.get('offset') || 0);
      return { body: pages[offset] ?? { items: [], total: 0, offset, limit: 20 } };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

describe('ExperimentsPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('fetches the first page and renders rows with the range label', async () => {
    const { calls } = expMock({ 0: { items: items(20), total: 25, offset: 0, limit: 20 } });
    renderPage(<ExperimentsPage />);
    expect(await screen.findAllByText('exp-1')).toBeTruthy();
    expect(calls[0]).toContain('limit=20');
    expect(calls[0]).toContain('offset=0');
    expect(screen.getByText('1–20 of 25')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Previous' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('walks the offset pager forward and back', async () => {
    const { calls } = expMock({
      0: { items: items(20), total: 25, offset: 0, limit: 20 },
      20: { items: items(5, 21), total: 25, offset: 20, limit: 20 },
    });
    renderPage(<ExperimentsPage />);
    await screen.findAllByText('exp-1');

    await userEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findAllByText('exp-21');
    expect(calls.at(-1)).toContain('offset=20');
    expect(screen.getByText('21–25 of 25')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Previous' }) as HTMLButtonElement).disabled).toBe(false);

    await userEvent.click(screen.getByRole('button', { name: 'Previous' }));
    await screen.findAllByText('exp-1');
    expect(calls.at(-1)).toContain('offset=0');
  });

  it('applies the search and state filters on submit', async () => {
    const { calls } = expMock({ 0: { items: items(2), total: 2, offset: 0, limit: 20 } });
    renderPage(<ExperimentsPage />);
    await screen.findAllByText('exp-1');

    await userEvent.type(screen.getByLabelText(/Search experiments/), 'checkout');
    await userEvent.selectOptions(screen.getByLabelText(/State/), 'running');
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => {
      expect(calls.at(-1)).toContain('q=checkout');
      expect(calls.at(-1)).toContain('state=running');
      expect(calls.at(-1)).toContain('offset=0');
    });
  });

  it('renders report cells for pending, healthy and unhealthy rows', async () => {
    expMock({
      0: {
        items: [
          item('exp-p', 'flag-p', 'draft', { status: 'pending' }),
          item('exp-h', 'flag-h', 'running', { status: 'ready', healthy: true, verdict: 'ship' }),
          item('exp-u', 'flag-u', 'running', { status: 'ready', healthy: false, verdict: 'rollback', issue: 'guardrail tripped' }),
        ],
        total: 3,
        offset: 0,
        limit: 20,
      },
    });
    renderPage(<ExperimentsPage />);
    expect((await screen.findAllByText('Not started'))[0]).toBeTruthy();
    expect(screen.getAllByText('Healthy')[0]).toBeTruthy();
    expect(screen.getAllByText('ship')[0].className).toContain('chip green');
    expect(screen.getAllByText('Needs diagnosis')[0]).toBeTruthy();
    expect(screen.getAllByText('rollback')[0].className).toContain('chip red');
    expect(screen.getAllByText('guardrail tripped')[0].getAttribute('title')).toBe('guardrail tripped');
  });

  it('renders the empty state for a zero total', async () => {
    expMock({ 0: { items: [], total: 0, offset: 0, limit: 20 } });
    renderPage(<ExperimentsPage />);
    expect((await screen.findAllByText('No experiments.'))[0]).toBeTruthy();
    expect(screen.getByText('0–0 of 0')).toBeTruthy();
  });

  it('gates the create button on experiment.write and navigates to the ~new route', async () => {
    expMock({ 0: { items: items(1), total: 1, offset: 0, limit: 20 } });
    const { unmount } = renderPage(<ExperimentsPage />, { permissions: [] });
    await screen.findAllByText('exp-1');
    expect(screen.queryByRole('button', { name: '+ New experiment' })).toBeNull();
    unmount();

    renderPage(<ExperimentsPage />, {
      permissions: ['experiment.write'],
      path: '/console/:ws/:env/experiments',
      route: '/console/ws1/prod/experiments',
    });
    await userEvent.click(await screen.findByRole('button', { name: '+ New experiment' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments/~new');
  });

  it('navigates to the experiment detail when the id is clicked', async () => {
    expMock({ 0: { items: items(1), total: 1, offset: 0, limit: 20 } });
    renderPage(<ExperimentsPage />, { path: '/console/:ws/:env/experiments', route: '/console/ws1/prod/experiments' });
    await userEvent.click((await screen.findAllByText('exp-1'))[0]);
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/experiments/exp-1');
  });
});
