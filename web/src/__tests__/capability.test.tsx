// Capability gating contract, mirroring legacy gateActions: create buttons
// are hidden (never disabled) without the write permission, and the tokens
// view falls back to the 403 error banner without token.manage.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ExperimentsPage, SegmentsPage, TokensPage } from '@saker/grayling-pages';
import { fetchMock, renderPage } from './helpers';

function setup() {
  const mock = vi.fn(
    fetchMock(url => {
      if (url.pathname.endsWith('/segments')) return { body: [] };
      if (url.pathname.endsWith('/experiment-summaries')) return { body: { items: [], total: 0, offset: 0, limit: 20 } };
      if (url.pathname.endsWith('/tokens')) return { body: [] };
      return { status: 404, body: {} };
    }),
  );
  vi.stubGlobal('fetch', mock);
  return mock;
}

describe('capability gating', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it('shows the tokens 403 banner without token.manage and re-auths from it', async () => {
    setup();
    const reauth = vi.fn();
    renderPage(<TokensPage />, { permissions: [], reauth });
    expect(await screen.findByText(/Access denied \(403\)/)).toBeTruthy();
    expect(screen.queryByText('Tokens')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Re-enter token' }));
    expect(reauth).toHaveBeenCalledTimes(1);
  });

  it('renders the tokens list with token.manage', async () => {
    setup();
    renderPage(<TokensPage />, { permissions: ['token.manage'] });
    expect(await screen.findByText('Tokens')).toBeTruthy();
    expect(screen.queryByText(/Access denied/)).toBeNull();
  });

  it('hides #seg-new without flag.write and shows it with the permission', async () => {
    setup();
    const first = renderPage(<SegmentsPage />, { permissions: [] });
    await screen.findByText('No segment drafts.');
    expect(document.getElementById('seg-new')).toBeNull();
    first.unmount();

    renderPage(<SegmentsPage />, { permissions: ['flag.write'] });
    await screen.findByText('No segment drafts.');
    expect(document.getElementById('seg-new')).toBeTruthy();
  });

  it('hides #exp-new without experiment.write and shows it with the permission', async () => {
    setup();
    const first = renderPage(<ExperimentsPage />, { permissions: [] });
    await screen.findByText('No experiments.');
    expect(document.getElementById('exp-new')).toBeNull();
    first.unmount();

    renderPage(<ExperimentsPage />, { permissions: ['experiment.write'] });
    await screen.findByText('No experiments.');
    expect(document.getElementById('exp-new')).toBeTruthy();
  });
});
