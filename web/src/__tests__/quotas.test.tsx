import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QuotasPage } from '@saker/grayling-pages';
import { fetchMock, renderPage } from './helpers';

const QUOTAS = {
  maxFlags: 100,
  maxFlagBytes: 65536,
  maxSegments: 50,
  maxExperiments: 25,
  maxEventBytes: 1048576,
  maxEventsPerBatch: 500,
  eventRetention: 604800000000000,
};

function setup(putImpl?: (body: unknown) => { status?: number; body?: unknown }) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const mock = vi.fn(
    fetchMock((url, init) => {
      calls.push({ url: url.pathname + url.search, init });
      if (url.pathname.endsWith('/quotas')) {
        if (init.method === 'PUT') return putImpl ? putImpl(JSON.parse(String(init.body))) : { body: QUOTAS };
        return { body: QUOTAS };
      }
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return { mock, calls };
}

async function openForm() {
  renderPage(<QuotasPage />);
  const input = (await screen.findByLabelText(/Maximum flags/)) as HTMLInputElement;
  // The controlled inputs render empty on the first paint after the fetch and
  // are seeded by an effect — wait for the seed to land.
  await waitFor(() => expect(input.value).not.toBe(''));
  return input;
}

describe('QuotasPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('loads current quotas into the form', async () => {
    setup();
    const input = (await openForm()) as HTMLInputElement;
    expect(input.value).toBe('100');
    expect((screen.getByLabelText(/Event retention/) as HTMLInputElement).value).toBe('604800000000000');
  });

  it('rejects invalid values before any confirmation or request', async () => {
    const { calls } = setup();
    const input = await openForm();
    await userEvent.clear(input);
    // '0' would be caught by native min=1 validation before the handler runs
    // (same as legacy); 2^53+2 passes native checks but is not a safe integer.
    await userEvent.type(input, '9007199254740994');
    await userEvent.click(screen.getByRole('button', { name: 'Save quotas' }));
    expect(await screen.findByText('Every limit must be a positive safe integer.')).toBeTruthy();
    expect(calls.some(c => c.init.method === 'PUT')).toBe(false);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('asks for confirmation via ConfirmDialog, then sends the exact body', async () => {
    const { calls } = setup();
    const input = await openForm();
    await userEvent.clear(input);
    await userEvent.type(input, '120');
    await userEvent.click(screen.getByRole('button', { name: 'Save quotas' }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Apply these workspace quotas?');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save quotas' }));

    await waitFor(() => expect(calls.some(c => c.init.method === 'PUT')).toBe(true));
    const put = calls.find(c => c.init.method === 'PUT');
    expect(JSON.parse(String(put?.init.body))).toEqual({ ...QUOTAS, maxFlags: 120 });
    expect(await screen.findByText('Quotas saved')).toBeTruthy();
  });

  it('cancelling the confirmation sends no request', async () => {
    const { calls } = setup();
    await openForm();
    await userEvent.click(screen.getByRole('button', { name: 'Save quotas' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(calls.some(c => c.init.method === 'PUT')).toBe(false);
  });

  it('surfaces a 403 with the denial text and refreshes capabilities', async () => {
    const refresh = vi.fn(() => Promise.resolve());
    setup(() => ({ status: 403, body: { error: { code: 'forbidden', message: 'denied' } } }));
    renderPage(<QuotasPage />, { refreshCapabilities: refresh });
    const input = (await screen.findByLabelText(/Maximum flags/)) as HTMLInputElement;
    await waitFor(() => expect(input.value).not.toBe(''));
    await userEvent.click(screen.getByRole('button', { name: 'Save quotas' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save quotas' }));
    expect(await screen.findByText(/Access denied \(403\)/)).toBeTruthy();
    expect(refresh).toHaveBeenCalled();
  });
});
