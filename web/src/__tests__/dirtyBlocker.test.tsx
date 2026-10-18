// dirtyBlocker.test.tsx pins the dirty-editor route guard (the React port of
// legacy confirmLeaveEditor, web-pages/src/editors/EditorDirtyContext.tsx):
// while the FlagDetailPage editor is dirty, ANY router navigation is held
// behind one confirm dialog — cancelling leaves the URL AND the editor
// content untouched, confirming lets the navigation through — and a
// successful save re-baselines the editor so the next navigation passes
// silently (legacy run(..., savedEditor) skips the confirm gate).
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { FlagDetailPage } from '@saker/grayling-pages/pages/FlagDetailPage';
import { fetchMock, renderPage } from './helpers';

// helpers.tsx registers afterEach(cleanup) for every importer — no local
// cleanup hook needed here.

const FLAG_DOC: Record<string, unknown> = {
  schemaVersion: 1,
  key: 'checkout',
  type: 'bool',
  disable: false,
  bucketing: { algorithm: 'grayling-fnv1a-10000-v1', unit: 'user_id', salt: 's1' },
  fields: ['user_id'],
  variations: { control: false, treatment: true },
  defaultRule: { variation: 'control' },
  targeting: [],
  experiment: null,
};

function detail(): Record<string, unknown> {
  return {
    key: 'checkout',
    draft: { state: 'editing', risk: 'normal', updatedBy: 'alice', updatedAt: '2026-01-02T10:00:00Z', content: FLAG_DOC },
    current: { revision: 1, hash: 'abc123def4567890', publishedAt: '2026-01-01T09:00:00Z', publishedBy: 'bob', content: FLAG_DOC },
    currentRevision: 1,
    draftHash: 'hash-1',
    revisions: [{ revision: 1, hash: 'abc123def4567890', publishedAt: '2026-01-01T09:00:00Z', publishedBy: 'bob', content: FLAG_DOC }],
    preview: null,
    experimentImpact: null,
  };
}

const ROUTE = { path: '/console/:ws/:env/flags/:flagKey', route: '/console/ws1/prod/flags/checkout' };
const PERMS = ['flag.read', 'flag.write', 'flag.approve', 'flag.publish'];
const FLAG_PATH = '/api/v1/admin/workspaces/ws1/environments/prod/flags/checkout';
const ELSEWHERE = '/console/ws1/prod';

function setup() {
  const puts: unknown[] = [];
  vi.stubGlobal('fetch', vi.fn(fetchMock(url => {
    if (url.pathname === FLAG_PATH + '/draft') {
      puts.push({});
      return { body: {} };
    }
    if (url.pathname === FLAG_PATH) return { body: detail() };
    if (url.pathname.endsWith('/segments')) return { body: [] };
    return { status: 404, body: {} };
  })));
  const { router } = renderPage(<FlagDetailPage />, { ...ROUTE, permissions: PERMS });
  return { router, puts };
}

function waitForEditor(): Promise<void> {
  return waitFor(() => expect(document.getElementById('flag-save')).toBeTruthy(), { timeout: 3000 });
}

// Dirty the editor through the JSON source (the single unsaved source of
// truth): one character is enough for snapshot() to diverge from baseline.
function makeDirty() {
  fireEvent.click(document.getElementById('flag-mode-json')!);
  const edited = { ...FLAG_DOC, description: 'unsaved edit' };
  fireEvent.change(document.getElementById('flag-json')!, { target: { value: JSON.stringify(edited, null, 2) } });
}

function dirtyDialog() {
  return waitFor(() => {
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('Discard unsaved editor changes?');
    return dialog;
  });
}

describe('dirty editor route guard', () => {
  it('lets a clean editor navigate without any dialog', async () => {
    const { router } = setup();
    await waitForEditor();
    router.navigate(ELSEWHERE);
    const probe = await screen.findByTestId('location');
    expect(probe.dataset.pathname).toBe(ELSEWHERE);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('holds a dirty navigation; cancelling leaves URL and editor content untouched', async () => {
    const { router } = setup();
    await waitForEditor();
    makeDirty();
    router.navigate(ELSEWHERE);
    const dialog = await dirtyDialog();
    // Nothing moved: the catch-all probe never rendered, the page is intact.
    expect(screen.queryByTestId('location')).toBeNull();
    expect(document.getElementById('flag-save')).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    // The URL never changed and the unsaved edit survived the cancel.
    expect(screen.queryByTestId('location')).toBeNull();
    expect(document.getElementById('flag-save')).toBeTruthy();
    expect((document.getElementById('flag-json') as HTMLTextAreaElement).value).toContain('unsaved edit');
  });

  it('proceeds with the held navigation once confirmed', async () => {
    const { router } = setup();
    await waitForEditor();
    makeDirty();
    router.navigate(ELSEWHERE);
    const dialog = await dirtyDialog();
    fireEvent.click(within(dialog).getByRole('button', { name: 'OK' }));
    const probe = await screen.findByTestId('location');
    expect(probe.dataset.pathname).toBe(ELSEWHERE);
  });

  it('re-baselines on save so the next navigation passes silently', async () => {
    const { router, puts } = setup();
    await waitForEditor();
    makeDirty();
    fireEvent.click(document.getElementById('flag-save')!);
    await screen.findByText('Draft saved');
    expect(puts.length).toBe(1);
    router.navigate(ELSEWHERE);
    const probe = await screen.findByTestId('location');
    expect(probe.dataset.pathname).toBe(ELSEWHERE);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
