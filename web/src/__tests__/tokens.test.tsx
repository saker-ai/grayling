import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { TokensPage } from '@saker/grayling-pages';
import { fetchMock, renderPage } from './helpers';
import type { FetchMock } from './helpers';

const TOKENS = [
  { id: 't1', name: 'ci-token', kind: 'sdk', role: 'viewer', prefix: 'gsdk1', environmentId: 'prod', createdAt: '2026-01-01T00:00:00Z', revoked: false },
  { id: 't2', name: 'old-admin', kind: 'admin', role: 'owner', prefix: 'gadm2', createdAt: '2025-12-01T00:00:00Z', revoked: true },
];

function setup(handler?: FetchMock) {
  const mock = vi.fn(
    handler ??
      fetchMock(url => {
        if (url.pathname.endsWith('/tokens')) return { body: TOKENS };
        return { status: 404, body: { error: { code: 'not_found', message: 'nope' } } };
      }),
  );
  vi.stubGlobal('fetch', mock);
  return mock;
}

describe('TokensPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders the token list with kind/status chips', async () => {
    setup();
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    expect((await screen.findAllByText('ci-token'))[0]).toBeTruthy();
    expect(screen.getAllByText('old-admin')[0]).toBeTruthy();
    expect(screen.getAllByText('revoked')[0]).toBeTruthy();
    expect(screen.getAllByText('active')[0]).toBeTruthy();
    // A revoked token carries no revoke action.
    expect(screen.getAllByRole('button', { name: 'Revoke' })).toHaveLength(2);
  });

  it('rejects an empty name without issuing a POST', async () => {
    const mock = setup();
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    const create = await screen.findByRole('button', { name: 'Create' });
    await userEvent.click(create);
    expect(await screen.findByText('Name required')).toBeTruthy();
    expect(mock.mock.calls.some(([, init]) => (init as RequestInit)?.method === 'POST')).toBe(false);
  });

  it('enforces the sdk/collect environment scope rule', async () => {
    const mock = setup();
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    await userEvent.type(await screen.findByLabelText(/Name/), 'edge-sdk');
    await userEvent.selectOptions(screen.getByLabelText(/Kind/), 'sdk');
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));
    expect(await screen.findByText(/Select an environment/)).toBeTruthy();
    expect(mock.mock.calls.some(([, init]) => (init as RequestInit)?.method === 'POST')).toBe(false);
  });

  it('creates a token, shows the secret once, and refreshes the list', async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    setup(
      fetchMock((url, init) => {
        calls.push({ url: url.pathname + url.search, init });
        if (url.pathname.endsWith('/tokens') && init.method === 'POST') {
          return { body: { id: 't3', name: 'edge-sdk', kind: 'sdk', role: 'viewer', prefix: 'gsdk3', Secret: 'gsecret-value' } };
        }
        if (url.pathname.endsWith('/tokens')) return { body: TOKENS };
        return { status: 404, body: {} };
      }),
    );
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    await userEvent.type(await screen.findByLabelText(/Name/), 'edge-sdk');
    await userEvent.selectOptions(screen.getByLabelText(/Kind/), 'sdk');
    await userEvent.selectOptions(screen.getByLabelText(/Environment/), 'prod');
    await userEvent.click(screen.getByRole('button', { name: 'Create' }));

    // The one-time secret banner carries the exact secret value.
    const input = (await screen.findByDisplayValue('gsecret-value')) as HTMLInputElement;
    expect(input.readOnly).toBe(true);
    expect(screen.getByText(/cannot be shown again/)).toBeTruthy();

    const post = calls.find(c => c.init.method === 'POST');
    expect(JSON.parse(String(post?.init.body))).toEqual({
      name: 'edge-sdk',
      kind: 'sdk',
      role: 'owner',
      environment: 'prod',
    });
    // The list is re-fetched after creation.
    await waitFor(() =>
      expect(calls.filter(c => c.url.endsWith('/tokens') && (!c.init.method || c.init.method === 'GET')).length).toBeGreaterThanOrEqual(2),
    );
  });

  it('revokes a token through the confirm dialog with a required reason', async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    setup(
      fetchMock((url, init) => {
        calls.push({ url: url.pathname + url.search, init });
        if (url.pathname.includes('/tokens/') && init.method === 'DELETE') return { body: {} };
        if (url.pathname.endsWith('/tokens')) return { body: TOKENS };
        return { status: 404, body: {} };
      }),
    );
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    await userEvent.click((await screen.findAllByRole('button', { name: 'Revoke' }))[0]);

    // Reason is required: confirming with an empty reason keeps the dialog.
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('ci-token');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    expect(calls.some(c => c.init.method === 'DELETE')).toBe(false);

    await userEvent.type(within(dialog).getByRole('textbox'), 'leaked in CI logs');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => expect(calls.some(c => c.init.method === 'DELETE')).toBe(true));
    const del = calls.find(c => c.init.method === 'DELETE');
    expect(del?.url).toBe('/api/v1/admin/tokens/t1');
    expect(JSON.parse(String(del?.init.body))).toEqual({ reason: 'leaked in CI logs' });
  });
});
