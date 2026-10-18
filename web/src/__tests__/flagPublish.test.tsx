// flagPublish.test.tsx ports web/legacy/flag_publish_contract.mjs onto the
// React FlagDetailPage: the publish review dialog re-fetches the detail when
// confirming and refuses to publish a draft (or experiment impact, or base
// revision) that changed since the operator reviewed it — the dialog stays
// open with the flag_review_changed feedback and the operator's reason
// preserved; a stable re-fetch publishes with exactly that reason. It also
// pins the draft state machine's button visibility and the 409 save-conflict
// flow (local input survives, ack adopts the server's hashes).
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { FlagDetailPage } from '@saker/grayling-pages/pages/FlagDetailPage';
import { fetchMock, renderPage } from './helpers';

afterEach(() => vi.unstubAllGlobals());

// respond mirrors the response shape helpers.fetchMock fabricates, for tests
// that need a stateful handler rather than a static route table.
function respond(body: unknown, status = 200): Promise<unknown> {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    statusText: '',
    text: () => Promise.resolve(JSON.stringify(body)),
    blob: () => Promise.resolve(new Blob([JSON.stringify(body)])),
  });
}

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

function detail(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    key: 'checkout',
    draft: { state: 'approved', risk: 'normal', updatedBy: 'alice', updatedAt: '2026-01-02T10:00:00Z', content: FLAG_DOC },
    current: { revision: 1, hash: 'abc123def4567890', publishedAt: '2026-01-01T09:00:00Z', publishedBy: 'bob', content: { ...FLAG_DOC, disable: true } },
    currentRevision: 1,
    draftHash: 'hash-1',
    revisions: [{ revision: 1, hash: 'abc123def4567890', publishedAt: '2026-01-01T09:00:00Z', publishedBy: 'bob', content: { ...FLAG_DOC, disable: true } }],
    preview: {
      targetEnvironment: 'prod', risk: 'normal', baseRevision: 1,
      changedFields: ['defaultRule'], estimatedAffected: 'all traffic',
      rollbackTarget: 'r1', guardrails: 'none', proposedHash: 'hash-1',
    },
    experimentImpact: { total: 1, references: [{ id: 'exp-1', state: 'running', epoch: 1, flagRevision: 1 }] },
    ...over,
  };
}

const ROUTE = { path: '/console/:ws/:env/flags/:flagKey', route: '/console/ws1/prod/flags/checkout' };
const PERMS = ['flag.read', 'flag.write', 'flag.approve', 'flag.publish'];
const FLAG_PATH = '/api/v1/admin/workspaces/ws1/environments/prod/flags/checkout';

function setup(options: { detailQueue?: Array<Record<string, unknown>>; onPublish?: (body: unknown) => void; publishResponse?: unknown } = {}) {
  const queue = [...(options.detailQueue || [])];
  const posts: unknown[] = [];
  const mock = vi.fn(fetchMock(url => {
    if (url.pathname === FLAG_PATH + '/publish') {
      const body = options.publishResponse ?? { flagRevision: 3, manifestRevision: 9 };
      return { body };
    }
    if (url.pathname === FLAG_PATH) return { body: queue.length ? queue.shift() : detail() };
    if (url.pathname.endsWith('/segments')) return { body: [] };
    return { status: 404, body: {} };
  }));
  const recording = vi.fn((input: unknown, init?: { body?: string }) => {
    const url = new URL(String(input), location.origin);
    if (url.pathname === FLAG_PATH + '/publish' && (init as RequestInit | undefined)?.method === 'POST') {
      const body = JSON.parse((init as { body: string }).body);
      posts.push(body);
      options.onPublish?.(body);
    }
    return mock(input, init);
  });
  vi.stubGlobal('fetch', recording);
  return { posts, mock: recording };
}

function renderDetail() {
  return renderPage(<FlagDetailPage />, { ...ROUTE, permissions: PERMS });
}

// The page is loaded once the editor frame exists (the flag key renders in
// several places, so text queries are ambiguous).
function waitForEditor(): Promise<void> {
  return waitFor(() => expect(document.getElementById('flag-save')).toBeTruthy(), { timeout: 3000 });
}

async function openPublishDialog() {
  await waitForEditor();
  fireEvent.click(document.getElementById('flag-publish')!);
  return screen.findByRole('dialog');
}

describe('FlagDetailPage publish review', () => {
  it('publishes after the review dialog with the typed note', async () => {
    const { posts } = setup();
    renderDetail();
    const dialog = await openPublishDialog();
    // The review shows the server preview and the draft-vs-published diff.
    expect(dialog.textContent).toContain('all traffic');
    expect(document.getElementById('flag-publish-review')).toBeTruthy();
    fireEvent.change(document.getElementById('action-reason')!, { target: { value: 'reviewed impact' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Publish' }));
    await waitFor(() => expect(posts).toEqual([{ note: 'reviewed impact' }]));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('refuses to publish when the draft changed since the review, then succeeds once stable', async () => {
    // The draft moves under the operator between review and confirm: the
    // first onConfirm re-fetch returns a different draftHash; the second
    // matches the (now refreshed) review and the publish goes through.
    let current = detail({ draftHash: 'hash-1' });
    const posts: unknown[] = [];
    vi.stubGlobal('fetch', vi.fn((input: unknown, init?: { body?: string; method?: string }) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname === FLAG_PATH + '/publish' && init?.method === 'POST') {
        posts.push(JSON.parse(init.body as string));
        return respond({ flagRevision: 3, manifestRevision: 9 });
      }
      if (url.pathname === FLAG_PATH) return respond(current);
      if (url.pathname.endsWith('/segments')) return respond([]);
      return respond({}, 404);
    }));

    renderDetail();
    const dialog = await openPublishDialog();
    fireEvent.change(document.getElementById('action-reason')!, { target: { value: 'reviewed impact' } });
    current = detail({ draftHash: 'hash-2' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Publish' }));
    // The dialog stays open with the re-review feedback; nothing publishes.
    await waitFor(() => expect(dialog.textContent).toContain('Review the updated evidence'));
    expect(posts).toEqual([]);
    // The refreshed preview is shown for the new draft.
    expect(document.getElementById('flag-publish-review')).toBeTruthy();
    // Second confirm: the re-fetch now matches the reviewed state, and the
    // reason typed for the first attempt is preserved.
    fireEvent.click(within(dialog).getByRole('button', { name: 'Publish' }));
    await waitFor(() => expect(posts).toEqual([{ note: 'reviewed impact' }]));
  });

  it('keeps local editor input through a 409 conflict until the operator acknowledges', async () => {
    const puts: Array<{ expectedDraftHash?: string; baseRevision?: number; content?: { description?: string } }> = [];
    let latest = detail({ draftHash: 'hash-1' });
    vi.stubGlobal('fetch', vi.fn((input: unknown, init?: { body?: string; method?: string }) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname === FLAG_PATH + '/draft' && init?.method === 'PUT') {
        puts.push(JSON.parse(init.body as string));
        if (puts.length === 1) return respond({ code: 'flag_draft_conflict', message: 'conflict' }, 409);
        return respond({});
      }
      if (url.pathname === FLAG_PATH) return respond(latest);
      if (url.pathname.endsWith('/segments')) return respond([]);
      return respond({}, 404);
    }));

    renderDetail();
    await waitForEditor();
    // Edit the JSON source: the conflict flow must not clobber it.
    fireEvent.click(document.getElementById('flag-mode-json')!);
    const edited = { ...FLAG_DOC, description: 'local edit in progress' };
    fireEvent.change(document.getElementById('flag-json')!, { target: { value: JSON.stringify(edited, null, 2) } });
    // The server moved to revision 2 while the operator edited.
    latest = detail({ draftHash: 'hash-remote', currentRevision: 2 });
    fireEvent.click(document.getElementById('flag-save')!);
    const conflict = await waitFor(() => {
      const box = document.getElementById('flag-conflict')!;
      expect(box.textContent).toContain('changed while you were editing');
      return box;
    });
    // Local input survived; no implicit merge happened.
    expect((document.getElementById('flag-json') as HTMLTextAreaElement).value).toContain('local edit in progress');
    expect(puts.length).toBe(1);
    // Acknowledge: the next save carries the server's hashes and succeeds.
    fireEvent.click(document.getElementById('flag-conflict-ack')!);
    await waitFor(() => expect(conflict.textContent).toContain('Latest revision and draft version selected'));
    fireEvent.click(document.getElementById('flag-save')!);
    await waitFor(() => expect(puts.length).toBe(2));
    const second = puts[1];
    expect(second.expectedDraftHash).toBe('hash-remote');
    expect(second.baseRevision).toBe(2);
    expect(second.content?.description).toBe('local edit in progress');
  });
});

describe('FlagDetailPage draft state machine', () => {
  const states: Array<[string, { save: boolean; submit: boolean; approve: boolean; reject: boolean; publish: boolean; primary: string }]> = [
    ['editing', { save: true, submit: true, approve: false, reject: false, publish: false, primary: 'flag-save' }],
    ['submitted', { save: true, submit: false, approve: true, reject: true, publish: false, primary: 'flag-approve' }],
    ['approved', { save: true, submit: false, approve: false, reject: true, publish: true, primary: 'flag-publish' }],
  ];
  for (const [state, want] of states) {
    it(`state ${state}: buttons and primary follow the legacy state machine`, async () => {
      vi.stubGlobal('fetch', vi.fn(fetchMock(url => {
        if (url.pathname === FLAG_PATH) {
          return { body: detail({ draft: { state, risk: 'normal', updatedBy: 'alice', updatedAt: '2026-01-02T10:00:00Z', content: FLAG_DOC } }) };
        }
        if (url.pathname.endsWith('/segments')) return { body: [] };
        return { status: 404, body: {} };
      })));
      renderDetail();
      await waitForEditor();
      await waitFor(() => expect(document.getElementById('flag-save')).toBeTruthy());
      for (const id of ['flag-save', 'flag-submit', 'flag-approve', 'flag-reject', 'flag-publish'] as const) {
        const key = id.replace('flag-', '') as 'save' | 'submit' | 'approve' | 'reject' | 'publish';
        expect(!!document.getElementById(id), `${state}: ${id}`).toBe(want[key]);
      }
      const primaryButton = document.getElementById(want.primary)!;
      expect(primaryButton.className).toContain('primary');
    });
  }

  it('emergency disable only appears with a published revision', async () => {
    vi.stubGlobal('fetch', vi.fn(fetchMock(url => {
      if (url.pathname === FLAG_PATH) return { body: detail({ current: null, currentRevision: 0 }) };
      if (url.pathname.endsWith('/segments')) return { body: [] };
      return { status: 404, body: {} };
    })));
    renderDetail();
    await waitForEditor();
    await waitFor(() => expect(document.getElementById('flag-save')).toBeTruthy());
    expect(document.getElementById('flag-emergency-disable')).toBeNull();
    expect(document.getElementById('flag-delete')).toBeTruthy();
  });
});
