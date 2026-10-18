import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { AIResourceDetail, AIResourcesResponse } from '@saker/grayling-client';
import { AiPage } from '@saker/grayling-pages/pages/AiPage';
import { fetchMock, renderPage } from './helpers';
import type { FetchHandler } from './helpers';

const RESOURCES: AIResourcesResponse = {
  resources: [
    {
      type: 'prompt',
      logicalId: 'checkout-helper',
      physicalId: 'nacos://prompts/checkout-helper',
      owner: 'alice',
      createdAt: '2026-01-02T10:00:00Z',
    },
    {
      type: 'skill',
      logicalId: 'old-skill',
      physicalId: 'nacos://skills/old-skill',
      owner: 'bob',
      createdAt: '2026-01-03T10:00:00Z',
    },
  ],
  // Revocation map keys are "type<NUL>logicalId", like the wire format.
  revocations: { ['skill\u0000old-skill']: 'leaked in logs' },
};

const LIST_ROUTE = { path: '/console/:ws/:env/ai', route: '/console/ws1/prod/ai' };
const PERMS = ['ai.write', 'ai.content.read'];

function setup(handler?: FetchHandler) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const mock = vi.fn(
    fetchMock((url, init) => {
      calls.push({ url: url.pathname + url.search, init });
      if (handler) return handler(url, init);
      if (url.pathname.endsWith('/ai/resources') && init.method === 'POST') return { body: {} };
      if (url.pathname.endsWith('/ai/resources')) return { body: RESOURCES };
      if (url.pathname.endsWith('/model-allowlist')) return { body: [] };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

describe('AiPage list', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders resources with type chips and revocation state', async () => {
    const { calls } = setup();
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    expect(await screen.findByText('checkout-helper')).toBeTruthy();
    // old-skill renders in both the resources row and the revocations table.
    expect(screen.getAllByText('old-skill')).toHaveLength(2);
    expect(screen.getByText('Resources (2)')).toBeTruthy();
    // The type selects also render prompt/skill as options, so pick the chips.
    const prompt = screen.getAllByText('prompt').find(el => el.className.includes('chip'));
    expect(prompt?.className).toContain('teal');
    const revokedType = screen
      .getAllByText('skill')
      .find(el => el.className.includes('chip') && el.className.includes('red'));
    expect(revokedType).toBeTruthy();
    // The revocation reason shows in the revocations table; the revoked row
    // offers Reinstate while the active row offers Revoke.
    expect(screen.getByText('leaked in logs')).toBeTruthy();
    // R1.0 regression pin: the revocations block stays mounted and unhidden —
    // the ≤700px grid rule used to display:none the whole wrapper, dropping
    // the content on mobile (the class now survives only as the R2 anchor).
    const revBlock = document.querySelector('.ai-revocations-desktop') as HTMLElement | null;
    expect(revBlock).toBeTruthy();
    expect(revBlock?.style.display).not.toBe('none');
    expect(revBlock?.hasAttribute('hidden')).toBe(false);
    expect(within(revBlock as HTMLElement).getByText('leaked in logs')).toBeTruthy();
    expect(screen.getAllByRole('button', { name: 'Reinstate' })).toHaveLength(2);
    expect(screen.getAllByRole('button', { name: 'Revoke' })).toHaveLength(1);
    expect(calls.some(c => c.url === '/api/v1/admin/workspaces/ws1/environments/prod/ai/resources')).toBe(true);
    expect(calls.some(c => c.url === '/api/v1/admin/workspaces/ws1/model-allowlist')).toBe(true);
  });

  it('validates the register fields before posting', async () => {
    const { calls } = setup();
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.click(screen.getByRole('button', { name: 'Register' }));
    expect(await screen.findByText('logicalId and name are required')).toBeTruthy();
    expect(calls.some(c => c.init.method === 'POST')).toBe(false);
  });

  it('registers a resource with the exact body', async () => {
    const { calls } = setup();
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.type(screen.getByPlaceholderText('checkout-helper'), 'summarizer');
    await userEvent.type(screen.getByLabelText('Name'), 'Summarizer');
    await userEvent.click(screen.getByRole('button', { name: 'Register' }));
    await waitFor(() => expect(calls.some(c => c.init.method === 'POST')).toBe(true));
    const post = calls.find(c => c.init.method === 'POST');
    expect(post?.url).toBe('/api/v1/admin/workspaces/ws1/environments/prod/ai/resources');
    expect(JSON.parse(String(post?.init.body))).toEqual({
      type: 'prompt',
      logicalId: 'summarizer',
      name: 'Summarizer',
    });
    expect(await screen.findByText('Resource registered')).toBeTruthy();
  });

  it('shows only the not-enabled banner when the API answers 501', async () => {
    setup(() => ({ status: 501, body: { error: { code: 'not_enabled', message: 'off' } } }));
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    expect(
      await screen.findByText('AI governance endpoints are not enabled on this server (501).'),
    ).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Register' })).toBeNull();
    expect(screen.queryByText('Resources (0)')).toBeNull();
  });

  it('pins a version through the approval dialog', async () => {
    const { calls } = setup((url, init) => {
      if (url.pathname.endsWith('/approvals') && init.method === 'POST') return { body: { id: 'ap1' } };
      if (url.pathname.endsWith('/ai/resources')) return { body: RESOURCES };
      if (url.pathname.endsWith('/model-allowlist')) return { body: [] };
      return { status: 404, body: {} };
    });
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.click(screen.getAllByRole('button', { name: 'Pin version' })[0]);
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Pin new version — prompt/checkout-helper');

    // Content is required: submitting empty content toasts and stays open.
    await userEvent.type(within(dialog).getByLabelText('Reason for this change'), 'pin v1');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pin' }));
    expect(await screen.findByText(/content is required/)).toBeTruthy();
    expect(calls.some(c => c.url.endsWith('/approvals'))).toBe(false);

    await userEvent.type(within(dialog).getByLabelText('Content'), 'say hello');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pin' }));
    await waitFor(() => expect(calls.some(c => c.url.endsWith('/approvals'))).toBe(true));
    const post = calls.find(c => c.url.endsWith('/approvals'));
    expect(post?.url).toBe('/api/v1/admin/workspaces/ws1/environments/prod/approvals');
    expect(JSON.parse(String(post?.init.body))).toEqual({
      action: 'ai.pin',
      object: 'prompt/checkout-helper',
      payload: { content: 'say hello' },
      reason: 'pin v1',
    });
    expect(await screen.findByText('Approval request ap1 created. Review it in Approvals.')).toBeTruthy();
  });

  it('revokes a resource through the confirm dialog with a required reason', async () => {
    const { calls } = setup((url, init) => {
      if (url.pathname.endsWith('/revoke') && init.method === 'POST') return { body: {} };
      if (url.pathname.endsWith('/ai/resources')) return { body: RESOURCES };
      if (url.pathname.endsWith('/model-allowlist')) return { body: [] };
      return { status: 404, body: {} };
    });
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.click(screen.getByRole('button', { name: 'Revoke' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Revoke this AI resource? SDK resolves will fail with 423.');

    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    expect(calls.some(c => c.url.endsWith('/revoke'))).toBe(false);
    await userEvent.type(within(dialog).getByRole('textbox'), 'compromised upstream');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => expect(calls.some(c => c.url.endsWith('/revoke'))).toBe(true));
    const post = calls.find(c => c.url.endsWith('/revoke'));
    expect(post?.url).toBe(
      '/api/v1/admin/workspaces/ws1/environments/prod/ai/resources/prompt/checkout-helper/revoke',
    );
    expect(JSON.parse(String(post?.init.body))).toEqual({ reason: 'compromised upstream' });
    expect(await screen.findByText('Resource revoked')).toBeTruthy();
  });

  it('resolves a resource and renders the version/digest chips', async () => {
    const { calls } = setup(url => {
      if (url.pathname.endsWith('/ai/resolve')) {
        return { body: { version: 'v3', digest: 'sha256:abc', content: 'hello prompt' } };
      }
      if (url.pathname.endsWith('/ai/resources')) return { body: RESOURCES };
      if (url.pathname.endsWith('/model-allowlist')) return { body: [] };
      return { status: 404, body: {} };
    });
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.type(screen.getByPlaceholderText('logicalId'), 'checkout-helper');
    await userEvent.click(screen.getByRole('button', { name: 'Resolve' }));
    expect(await screen.findByText('version: v3')).toBeTruthy();
    expect(screen.getByText('digest: sha256:abc')).toBeTruthy();
    expect(screen.getByText('hello prompt')).toBeTruthy();
    const resolve = calls.find(c => c.url.includes('/ai/resolve'));
    expect(resolve?.url).toBe(
      '/api/v1/admin/workspaces/ws1/environments/prod/ai/resolve?type=prompt&logicalId=checkout-helper',
    );
  });

  it('saves the allowlist with the collected entries', async () => {
    const { calls } = setup((url, init) => {
      if (url.pathname.endsWith('/model-allowlist') && init.method === 'PUT') return { body: {} };
      if (url.pathname.endsWith('/model-allowlist')) return { body: [] };
      if (url.pathname.endsWith('/ai/resources')) return { body: RESOURCES };
      return { status: 404, body: {} };
    });
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    expect(screen.getByText('Allowlist empty.')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: '+ Add entry' }));
    await userEvent.type(screen.getByPlaceholderText('provider e.g. openai'), 'openai');
    await userEvent.type(screen.getByPlaceholderText('model e.g. gpt-4o'), 'gpt-4o');
    await userEvent.click(screen.getByRole('button', { name: 'Save allowlist' }));
    await waitFor(() => expect(calls.some(c => c.init.method === 'PUT')).toBe(true));
    const put = calls.find(c => c.init.method === 'PUT');
    expect(put?.url).toBe('/api/v1/admin/workspaces/ws1/model-allowlist');
    expect(JSON.parse(String(put?.init.body))).toEqual({
      entries: [{ provider: 'openai', model: 'gpt-4o', enabled: true }],
    });
    expect(await screen.findByText('Allowlist saved')).toBeTruthy();
  });

  it('rejects a half-filled allowlist row without a PUT', async () => {
    const { calls } = setup();
    renderPage(<AiPage />, { ...LIST_ROUTE, permissions: PERMS });
    await screen.findByText('checkout-helper');
    await userEvent.click(screen.getByRole('button', { name: '+ Add entry' }));
    await userEvent.type(screen.getByPlaceholderText('provider e.g. openai'), 'openai');
    await userEvent.click(screen.getByRole('button', { name: 'Save allowlist' }));
    expect(await screen.findByText(/Allowlist row 1/)).toBeTruthy();
    expect(calls.some(c => c.init.method === 'PUT')).toBe(false);
  });
});

describe('AiPage detail', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  const DETAIL: AIResourceDetail = {
    resource: {
      type: 'prompt',
      logicalId: 'checkout-helper',
      physicalId: 'nacos://prompts/checkout-helper',
      owner: 'alice',
      createdAt: '2026-01-02T10:00:00Z',
    },
    pins: [{ version: 'v2', pinnedAt: '2026-01-03T10:00:00Z', pinnedBy: 'bob', digest: 'sha256:deadbeef' }],
    sourceMode: 'embedded-gonacos',
    revoked: true,
    revocationReason: 'leaked in logs',
  };

  it('renders the status card, pin history and the revoked banner', async () => {
    setup(url => {
      if (url.pathname.endsWith('/ai/resources/prompt/checkout-helper')) return { body: DETAIL };
      return { status: 404, body: {} };
    });
    renderPage(<AiPage />, {
      path: '/console/:ws/:env/ai/:type/:logicalId',
      route: '/console/ws1/prod/ai/prompt/checkout-helper',
      permissions: PERMS,
    });
    expect(await screen.findByText('prompt / checkout-helper')).toBeTruthy();
    expect(screen.getByText('revoked').className).toContain('chip red');
    expect(screen.getByText('Embedded gonacos')).toBeTruthy();
    expect(screen.getByText('nacos://prompts/checkout-helper')).toBeTruthy();
    expect(screen.getByText('alice')).toBeTruthy();
    expect(screen.getByText('Resolution is blocked now. Reason: leaked in logs')).toBeTruthy();
    // Latest pin dd plus the pin history entry both render v2.
    expect(screen.getAllByText('v2')).toHaveLength(2);
    expect(screen.getByText('sha256:deadbeef')).toBeTruthy();
    expect(screen.getByText(/Downstream Flag and entry dependencies/)).toBeTruthy();
    // Revoked detail offers reinstate, not revoke.
    expect(screen.getByRole('button', { name: 'Pin version' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Reinstate' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull();

    await userEvent.click(screen.getByRole('button', { name: 'Back to AI resources' }));
    const loc = await screen.findByTestId('location');
    expect(loc.getAttribute('data-pathname')).toBe('/console/ws1/prod/ai');
  });
});
