// Contract for the real Flag publish confirmation flow. It verifies that a
// changed draft or experiment relationship requires another explicit review.
import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';

const code = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js']
  .map(name => fs.readFileSync(new URL(name, import.meta.url), 'utf8')).join('\n');
const reviewBox = { innerHTML: '' };
const posts = [];
let current;
const base = '/api/v1/admin/workspaces/acme/environments/prod/flags/checkout';
const view = (state, hash = 'draft-v1') => ({
  key: 'checkout', currentRevision: 1, draftHash: hash,
  current: { revision: 1, content: { type: 'bool' } },
  draft: { content: { type: 'bool' } },
  preview: { targetEnvironment: 'acme/prod', risk: 'normal', baseRevision: 1,
    changedFields: [], estimatedAffected: 'unknown', rollbackTarget: 'r1', guardrails: 'review',
    experimentImpact: { total: 1, references: [{ id: 'exp-1', state, epoch: 1, flagRevision: 1 }] } },
  experimentImpact: { total: 1, references: [{ id: 'exp-1', state, epoch: 1, flagRevision: 1 }] },
});
const sandbox = {
  document: {
    addEventListener() {}, createElement() { return { classList: { add() {} }, remove() {} }; },
    querySelector(selector) {
      if (selector === '#flag-publish-review') return reviewBox;
      if (selector === '#toasts') return { appendChild() {} };
      return null;
    },
    querySelectorAll() { return []; }, body: { appendChild() {} },
  },
  localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
  location: { hash: '', origin: 'http://localhost' }, history: { pushState() {}, replaceState() {} },
  navigator: {}, console, setTimeout() {}, clearTimeout() {},
  fetch: async (url, opts) => {
    if (url === base && opts.method === 'GET') {
      return { ok: true, status: 200, text: async () => JSON.stringify(current) };
    }
    if (url === base + '/publish' && opts.method === 'POST') {
      posts.push(JSON.parse(opts.body));
      return { ok: true, status: 200, text: async () => JSON.stringify({ flagRevision: 2, manifestRevision: 2 }) };
    }
    throw new Error('unexpected request: ' + opts.method + ' ' + url);
  },
};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInContext(code + '\n;globalThis.__confirmFlagPublish = confirmFlagPublish;', sandbox);
current = view('running');
let firstConflict;
let preview;
sandbox.actionDialog = async options => {
  preview = options.preview;
  current = view('paused');
  try { await options.onConfirm({ reason: 'reviewed impact' }); }
  catch (error) { firstConflict = error; }
  assert.equal(posts.length, 0, 'changed experiment status must not publish');
  assert.equal(firstConflict?.code, 'flag_review_changed');
  assert(reviewBox.innerHTML.includes('paused'), 'updated relationship must replace confirmation evidence');
  await options.onConfirm({ reason: 'reviewed impact' });
  return null; // avoid router side effects in this focused contract
};
await sandbox.__confirmFlagPublish('checkout', base);
assert(preview.includes('running'), 'confirmation must show the initially reviewed experiment state');
assert.deepEqual(posts, [{ note: 'reviewed impact' }], 'second confirmation publishes with preserved reason');

current = view('running');
posts.length = 0;
let draftConflict;
sandbox.actionDialog = async options => {
  current = view('running', 'draft-v2');
  try { await options.onConfirm({ reason: 'old draft' }); }
  catch (error) { draftConflict = error; }
  return null;
};
await sandbox.__confirmFlagPublish('checkout', base);
assert.equal(draftConflict?.code, 'flag_review_changed', 'changed draft must require re-review');
assert.equal(posts.length, 0, 'changed draft must not publish');

console.log('Flag publish review contract: stale relationships and drafts require re-review');
