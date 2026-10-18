// staleRoute.test.tsx ports the stale-route-guard assertion of the legacy
// i18n contract (web/legacy/i18n_contract.mjs 821-837): a slow response for
// view A must never overwrite view B once the operator has navigated away.
// The React mechanism is useApiData's per-effect AbortController (no
// setState after abort) — this test drives exactly that race: mount the slow
// view, navigate to the fast view, resolve the slow fetch late, and assert
// the fast view is intact and the slow content never appears.
import { describe, expect, it, vi } from 'vitest';
import { act, screen, waitFor } from '@testing-library/react';
import type { ReactElement } from 'react';
import { useLocation } from 'react-router-dom';
import { useApiData } from '@saker/grayling-pages';
import { fetchMock, renderPage } from './helpers';

function fetchText(path: string, signal: AbortSignal): Promise<string> {
  return fetch(path, { signal })
    .then(r => (r as unknown as { text: () => Promise<string> }).text())
    .then(JSON.parse);
}

function SlowView(): ReactElement {
  const { data, loading } = useApiData<string>(signal => fetchText('/api/slow', signal), []);
  return <div data-testid="view">{loading ? 'slow-loading' : 'slow:' + data}</div>;
}

function FastView(): ReactElement {
  const { data, loading } = useApiData<string>(signal => fetchText('/api/fast', signal), []);
  return <div data-testid="view">{loading ? 'fast-loading' : 'fast:' + data}</div>;
}

function Switcher(): ReactElement {
  const { pathname } = useLocation();
  return pathname.endsWith('/a') ? <SlowView /> : <FastView />;
}

describe('stale route guard', () => {
  it('a late response for the previous view never overwrites the current one', async () => {
    let resolveSlow: (value: { body: string }) => void = () => undefined;
    const slow = new Promise<{ body: string }>(resolve => {
      resolveSlow = resolve;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(
        fetchMock(url => {
          if (url.pathname === '/api/slow') return slow;
          if (url.pathname === '/api/fast') return { body: 'FAST' };
          return { status: 404, body: {} };
        }),
      ),
    );
    const { router } = renderPage(<Switcher />, { route: '/console/ws1/prod/a' });
    // View A is stuck loading its slow fetch.
    expect(screen.getByTestId('view').textContent).toBe('slow-loading');

    // Navigate to B while A is in flight; B resolves immediately.
    await act(async () => {
      await router.navigate('/console/ws1/prod/b');
    });
    await waitFor(() => expect(screen.getByTestId('view').textContent).toBe('fast:FAST'));

    // A's response finally lands — the current view must not change.
    await act(async () => {
      resolveSlow({ body: 'SLOW' });
      await slow;
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByTestId('view').textContent).toBe('fast:FAST');
    expect(screen.queryByText(/^slow:/)).toBeNull();
  });
});
