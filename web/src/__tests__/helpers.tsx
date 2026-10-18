// Shared test harness for the view tests: a jsdom render of one
// @saker/grayling-pages page under the same providers the shell installs
// (i18n side effect, Toast, Confirm, Host bridge) plus an in-memory DATA
// router so route params, navigation and useBlocker (the dirty-editor guard)
// all work. fetch is stubbed per test with a URL-routed handler, mirroring
// web-client/src/__tests__/client.test.ts.
import type { ReactElement } from 'react';
import { RouterProvider, createMemoryRouter, useLocation } from 'react-router-dom';
import { afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/react';
import { GraylingClient } from '@saker/grayling-client';
import { HostProvider, ToastProvider, ConfirmProvider } from '@saker/grayling-pages';
import type { HostServices } from '@saker/grayling-pages';
import '../i18n';

// vitest runs with globals off, so @testing-library's auto-cleanup never
// registers — do it here for every page test (they all import this module).
afterEach(cleanup);

export interface MockResponse {
  status?: number;
  body?: unknown;
}

export type FetchHandler = (url: URL, init: RequestInit) => MockResponse | Promise<MockResponse>;

export function fetchMock(handler: FetchHandler) {
  return (input: unknown, init?: unknown): Promise<unknown> => {
    // The client issues relative paths (baseUrl() is empty in tests), so
    // resolve against the jsdom origin before handing the URL to handlers.
    const url = new URL(String(input), location.origin);
    return Promise.resolve(handler(url, (init || {}) as RequestInit)).then(r => {
      const status = r.status ?? 200;
      const text = JSON.stringify(r.body ?? null);
      return {
        ok: status >= 200 && status < 300,
        status,
        statusText: '',
        text: () => Promise.resolve(text),
        blob: () => Promise.resolve(new Blob([text])),
      };
    });
  };
}

// FetchMock is the vi.fn-compatible fetch implementation fetchMock returns;
// setup() helpers that accept an optional override should take this type.
export type FetchMock = ReturnType<typeof fetchMock>;

// LocationProbe renders the current in-router location so navigation tests
// can assert where a page sent the operator (and which router state it
// carried).
export function LocationProbe(): ReactElement {
  const location = useLocation();
  return (
    <output
      data-testid="location"
      data-pathname={location.pathname}
      data-state={JSON.stringify(location.state ?? null)}
    />
  );
}

export interface RenderOptions {
  // route is the MemoryRouter initial entry; path the <Route> pattern that
  // hosts the page (defaults to the catch-all, for pages without params).
  route?: string;
  path?: string;
  permissions?: string[];
  envs?: Array<{ slug: string; name?: string }>;
  ws?: string;
  env?: string;
  actor?: string;
  refreshCapabilities?: () => Promise<void>;
  reauth?: () => void;
}

export function renderPage(page: ReactElement, options: RenderOptions = {}) {
  const {
    route = '/console/ws1/prod',
    path = '*',
    permissions = [],
    envs = [{ slug: 'prod', name: 'Production' }],
    ws = 'ws1',
    env = 'prod',
    actor = 'operator',
    refreshCapabilities = () => Promise.resolve(),
    reauth = () => {},
  } = options;
  const perms = new Set(permissions);
  const host: HostServices = {
    client: new GraylingClient(),
    ws,
    env,
    envs,
    actor,
    can: p => perms.has(p),
    refreshCapabilities,
    reauth,
  };
  // A data router (not a plain MemoryRouter) because the dirty-editor guard
  // uses useBlocker, which only exists on data routers.
  const router = createMemoryRouter(
    [
      { path, element: page },
      { path: '*', element: <LocationProbe /> },
    ],
    { initialEntries: [route] },
  );
  const view = render(
    <ToastProvider>
      <ConfirmProvider>
        <HostProvider value={host}>
          <RouterProvider router={router} />
        </HostProvider>
      </ConfirmProvider>
    </ToastProvider>,
  );
  // The router is exposed so navigation tests (the dirty-editor guard) can
  // drive transitions directly — window.history is not wired to a memory
  // router.
  return { ...view, router };
}
