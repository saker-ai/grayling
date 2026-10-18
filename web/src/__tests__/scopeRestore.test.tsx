// Pins the cold deep-link scope race fixed with the React migration
// acceptance: while the environment list of the deep-linked workspace is
// still in flight, the URL env slug must be trusted as-is. The pre-fix
// provider judged the slug against the not-yet-arrived empty list and
// transiently rewrote `#/console/ws1/prod/flags/…` to `#/console/ws1/~/flags`,
// firing view fetches with an empty env (404) and losing the deep link.
import type { ReactElement } from 'react';
import { RouterProvider, createMemoryRouter, useLocation } from 'react-router-dom';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup } from '@testing-library/react';
import { SessionProvider } from '../session/SessionProvider';
import { ScopeProvider, useScope } from '../scope/ScopeProvider';
import { fetchMock } from './helpers';
import '../i18n';

afterEach(cleanup);

function Probe(): ReactElement {
  const { ws, env } = useScope();
  const location = useLocation();
  return <output data-testid="probe" data-ws={ws} data-env={env} data-pathname={location.pathname} />;
}

describe('ScopeProvider cold deep link', () => {
  it('trusts the URL env slug while the environments fetch is in flight', async () => {
    const requested: string[] = [];
    let resolveEnvs: (list: Array<{ slug: string; name: string }>) => void = () => {};
    const envsPending = new Promise<Array<{ slug: string; name: string }>>(resolve => {
      resolveEnvs = resolve;
    });
    vi.stubGlobal(
      'fetch',
      fetchMock(url => {
        requested.push(url.pathname);
        if (url.pathname === '/api/v1/admin/session') return { body: { name: 'owner', principalId: 'owner' } };
        if (url.pathname === '/api/v1/admin/workspaces') return { body: [{ slug: 'ws1', name: 'Workspace One' }] };
        if (url.pathname === '/api/v1/admin/workspaces/ws1/environments') {
          return envsPending.then(list => ({ body: list }));
        }
        return { status: 404, body: { error: 'unexpected ' + url.pathname } };
      }),
    );
    const router = createMemoryRouter(
      [{ path: '/console/:ws/:env/*', element: <ScopeProvider><Probe /></ScopeProvider> }],
      { initialEntries: ['/console/ws1/prod/flags/checkout'] },
    );
    render(
      <SessionProvider>
        <RouterProvider router={router} />
      </SessionProvider>,
    );

    // Session + workspaces settle; environments deliberately still pending.
    await screen.findByTestId('probe');
    await waitFor(() => {
      expect(requested).toContain('/api/v1/admin/workspaces/ws1/environments');
    });
    // The pre-fix code rewrote the URL to /console/ws1/~/flags here.
    expect(router.state.location.pathname).toBe('/console/ws1/prod/flags/checkout');
    const probe = screen.getByTestId('probe');
    expect(probe.getAttribute('data-ws')).toBe('ws1');
    expect(probe.getAttribute('data-env')).toBe('prod');
    expect(requested.some(p => p.includes('environments//'))).toBe(false);

    // Once the list lands, the scope stays put (slug exists) and persists.
    resolveEnvs([{ slug: 'prod', name: 'Production' }]);
    await waitFor(() => {
      expect(localStorage.getItem('grayling_env')).toBe('prod');
    });
    expect(screen.getByTestId('probe').getAttribute('data-env')).toBe('prod');
    expect(router.state.location.pathname).toBe('/console/ws1/prod/flags/checkout');
    expect(requested.some(p => p.includes('environments//'))).toBe(false);
    vi.unstubAllGlobals();
  });

  it('still corrects an unknown env slug once the list has arrived', async () => {
    vi.stubGlobal(
      'fetch',
      fetchMock(url => {
        if (url.pathname === '/api/v1/admin/session') return { body: { name: 'owner', principalId: 'owner' } };
        if (url.pathname === '/api/v1/admin/workspaces') return { body: [{ slug: 'ws1', name: 'Workspace One' }] };
        if (url.pathname === '/api/v1/admin/workspaces/ws1/environments') {
          return { body: [{ slug: 'prod', name: 'Production' }] };
        }
        return { status: 404, body: { error: 'unexpected ' + url.pathname } };
      }),
    );
    const router = createMemoryRouter(
      [{ path: '/console/:ws/:env/*', element: <ScopeProvider><Probe /></ScopeProvider> }],
      { initialEntries: ['/console/ws1/staging/flags'] },
    );
    render(
      <SessionProvider>
        <RouterProvider router={router} />
      </SessionProvider>,
    );
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/console/ws1/prod/flags');
    });
    expect(screen.getByTestId('probe').getAttribute('data-env')).toBe('prod');
    vi.unstubAllGlobals();
  });
});
