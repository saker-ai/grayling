// Every data-fetching phase-1 view shows the Skeleton block while its request
// is in flight; the debug view is the exception — its form renders immediately
// with no fetch. The bars are aria-hidden decoration; a visually-hidden
// role=status line carries the loading announcement for screen readers.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import type { ReactElement } from 'react';
import { AuditPage, DebugPage, ExperimentsPage, QuotasPage, SegmentsPage, TokensPage } from '@saker/grayling-pages';
import { renderPage } from './helpers';
import type { RenderOptions } from './helpers';

function pendingFetch() {
  const mock = vi.fn(() => new Promise(() => {}));
  vi.stubGlobal('fetch', mock);
  return mock;
}

describe('skeleton loading states', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  const cases: Array<[string, ReactElement, RenderOptions]> = [
    ['quotas', <QuotasPage key="q" />, {}],
    ['tokens', <TokensPage key="t" />, { permissions: ['token.manage'] }],
    ['audit', <AuditPage key="a" />, {}],
    ['segments', <SegmentsPage key="s" />, {}],
    ['experiments', <ExperimentsPage key="e" />, {}],
  ];
  it.each(cases)('%s renders skeletons while the fetch pends', (_name, page, options) => {
    pendingFetch();
    const { unmount } = renderPage(page, options);
    expect(document.querySelectorAll('.skeleton').length).toBeGreaterThan(0);
    // R1.2: the loading state is announced via a visually-hidden live region
    // (no bare "Loading…" text); the decorative bars stay aria-hidden. The
    // toast outlet also has role=status, so scope the lookup by text.
    const status = screen.getByText('Loading…');
    expect(status.getAttribute('role')).toBe('status');
    expect(status.className).toContain('visually-hidden');
    const bars = document.querySelector('.skeleton')?.parentElement;
    expect(bars?.getAttribute('aria-hidden')).toBe('true');
    unmount();
  });

  it('debug renders the form immediately without fetching', () => {
    const mock = pendingFetch();
    renderPage(<DebugPage />);
    expect(screen.getByRole('button', { name: 'Evaluate' })).toBeTruthy();
    expect(document.querySelectorAll('.skeleton').length).toBe(0);
    expect(mock).not.toHaveBeenCalled();
  });
});

