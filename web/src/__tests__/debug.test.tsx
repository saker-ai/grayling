import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DebugPage } from '@saker/grayling-pages';
import { fetchMock, renderPage } from './helpers';
import type { FetchMock } from './helpers';

function setup(handler?: FetchMock) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const mock = vi.fn(
    handler ??
      fetchMock((url, init) => {
        calls.push({ url: url.pathname + url.search, init });
        if (url.pathname.endsWith('/debug/eval')) {
          return {
            body: {
              result: { value: true, variationType: 'bool', reason: 'targeting', failed: false, bucket: 417 },
              manifestRevision: 12,
              note: 'ok',
            },
          };
        }
        return { status: 404, body: {} };
      }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

describe('DebugPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('renders the form immediately with no fetch', () => {
    const { mock } = setup();
    renderPage(<DebugPage />);
    expect(screen.getByRole('button', { name: 'Evaluate' })).toBeTruthy();
    expect(mock).not.toHaveBeenCalled();
  });

  it('rejects invalid attributes JSON before any request', async () => {
    const { mock } = setup();
    renderPage(<DebugPage />);
    const attrs = document.querySelector('textarea[name="d-attrs"]') as HTMLTextAreaElement;
    await userEvent.clear(attrs);
    await userEvent.type(attrs, '{{ not json');
    await userEvent.click(screen.getByRole('button', { name: 'Evaluate' }));
    expect(await screen.findByText(/Attributes JSON invalid/)).toBeTruthy();
    expect(mock).not.toHaveBeenCalled();
  });

  it('requires a flag key', async () => {
    const { mock } = setup();
    renderPage(<DebugPage />);
    await userEvent.click(screen.getByRole('button', { name: 'Evaluate' }));
    expect(await screen.findByText('Flag key required')).toBeTruthy();
    expect(mock).not.toHaveBeenCalled();
  });

  it('submits the evaluation and renders the result summary and JSON', async () => {
    const { calls } = setup();
    renderPage(<DebugPage />);
    await userEvent.type(screen.getByPlaceholderText('my-flag'), 'checkout-v2');
    await userEvent.type(screen.getByPlaceholderText('user_id value'), 'user-9');
    await userEvent.click(screen.getByRole('button', { name: 'Evaluate' }));

    const post = calls.find(c => c.url.endsWith('/debug/eval'));
    expect(post).toBeTruthy();
    expect(JSON.parse(String(post?.init.body))).toEqual({
      flagKey: 'checkout-v2',
      context: { targetingKey: 'user-9', attributes: {} },
    });

    expect(await screen.findByText('value: true')).toBeTruthy();
    expect(screen.getByText('variation: bool')).toBeTruthy();
    expect(screen.getByText('reason: Targeting rule')).toBeTruthy();
    expect(screen.getByText('bucket 417')).toBeTruthy();
    expect(screen.getByText(/manifest r12/)).toBeTruthy();
    const pre = document.getElementById('d-json');
    expect(pre?.textContent).toContain('"manifestRevision": 12');
  });

  it('toasts the error and keeps the form when evaluation fails', async () => {
    setup(
      fetchMock(url =>
        url.pathname.endsWith('/debug/eval')
          ? { status: 500, body: { error: { code: 'boom', message: 'kaboom' } } }
          : { status: 404, body: {} },
      ),
    );
    renderPage(<DebugPage />);
    await userEvent.type(screen.getByPlaceholderText('my-flag'), 'checkout-v2');
    await userEvent.click(screen.getByRole('button', { name: 'Evaluate' }));
    expect(await screen.findByText(/server returned 500/)).toBeTruthy();
    expect(document.getElementById('d-out')?.className).toContain('hidden');
  });
});
