// Behavioral coverage for request deadlines, cancellation and dirty history navigation.
import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const code = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js']
  .map(name => fs.readFileSync(new URL(name, import.meta.url), 'utf8')).join('\n');
let fetchImpl;
let confirms = 0;
let shouldLeave = false;
let routes = 0;
const entries = [{ url: 'http://localhost/#/console/acme/prod/flags', state: { graylingIndex: 0 } },
  { url: 'http://localhost/#/console/acme/prod/flags/new', state: { graylingIndex: 1 } }];
let position = 1;
const location = { href: entries[1].url, hash: '#/console/acme/prod/flags/new', origin: 'http://localhost' };
const history = {
  state: entries[1].state,
  replaceState(state, _, url) { entries[position] = { state, url }; this.state = state; location.href = url; },
  pushState(state, _, hash) { entries.splice(++position, entries.length, { state, url: 'http://localhost/' + hash }); this.state = state; location.href = entries[position].url; location.hash = hash; },
  go(delta) { position += delta; this.state = entries[position].state; location.href = entries[position].url; location.hash = new URL(location.href).hash; void pop({ state: this.state }); },
};
const sandbox = { document: { addEventListener() {} }, localStorage: { getItem() { return null; } },
  window: {}, navigator: {}, location, history, console, AbortController, setTimeout, clearTimeout,
  fetch: (...args) => fetchImpl(...args),
};
vm.createContext(sandbox);
vm.runInContext(code + `\nglobalThis.x = { requestBody, api, downloadReport, S, handleConsolePopState, loadWorkspaces, resourceKeyError, resourceKeyField };
confirmLeaveEditor = () => globalThis.leave();
loadWorkspaces = async () => {};
route = async () => globalThis.routed();
`, sandbox);
sandbox.leave = () => { confirms++; return shouldLeave; };
sandbox.routed = () => { routes++; };
const { requestBody, api, S, handleConsolePopState: pop } = sandbox.x;
for (const key of ['office-network', 'Office.Net_1', '0', 'a'.repeat(128)]) {
  assert.equal(sandbox.x.resourceKeyError(key), '', 'valid API identifiers must be accepted');
}
for (const key of ['办公网', '', '-office', 'office network', 'office/network', 'a'.repeat(129)]) {
  assert(sandbox.x.resourceKeyError(key), 'invalid API identifiers must be rejected before opening an editor');
}
assert.equal(sandbox.x.resourceKeyField('Key').validate, sandbox.x.resourceKeyError);
let releaseWorkspaces;
fetchImpl = () => new Promise(resolve => { releaseWorkspaces = resolve; });
let active = true;
S.workspaces = [{ slug: 'newer-scope' }];
const staleLoad = sandbox.x.loadWorkspaces(() => active);
active = false;
releaseWorkspaces({ ok: true, text: async () => '[{"slug":"stale-scope"}]' });
await staleLoad;
assert.equal(S.workspaces[0].slug, 'newer-scope', 'stale scope loading must not overwrite current navigation');
let calls = 0;
const aborted = signal => new Promise((_, reject) => {
  if (signal.aborted) reject(new Error('aborted'));
  signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
});
fetchImpl = async (_, { signal }) => { calls++; return aborted(signal); };
await assert.rejects(requestBody('/slow-headers', {}, r => r.text(), 10), e => e.code === 'timeout');
assert.equal(calls, 1, 'deadline must not retry writes or reads');
fetchImpl = async (_, { signal }) => ({ text: () => aborted(signal) });
await assert.rejects(requestBody('/slow-body', {}, r => r.text(), 10), e => e.code === 'timeout');
const controller = new AbortController();
const pending = api('/cancelled', { method: 'POST', body: { draft: true }, signal: controller.signal });
controller.abort();
await assert.rejects(pending, e => e.code === 'cancelled');
fetchImpl = async () => ({ ok: true, text: async () => '{"saved":true}' });
assert.equal((await api('/healthy')).saved, true, 'a subsequent request must still work');
S.booted = true; S.view = 'flags'; S.ws = 'acme'; S.env = 'prod'; S.flagKey = 'new';
S.expCreate = true; S.wizardDirty = true;
position = 0; history.state = entries[0].state; location.href = entries[0].url; location.hash = new URL(location.href).hash;
await pop({ state: history.state });
assert.equal(position, 1, 'cancelled back restores the original history entry');
assert.equal(location.href, entries[1].url);
assert.equal(S.flagKey, 'new', 'cancelled history keeps the editor state');
assert.equal(S.wizardDirty, true, 'cancelled history keeps wizard state');
assert.equal(confirms, 1, 'restoration must not show a second confirmation');
assert.equal(routes, 0, 'cancelled history must not render over the editor');
shouldLeave = true;
position = 0; history.state = entries[0].state; location.href = entries[0].url; location.hash = new URL(location.href).hash;
await pop({ state: history.state });
assert.equal(routes, 1, 'accepted back renders its destination');
shouldLeave = false;
S.flagKey = 'forward-unsaved';
position = 1; history.state = entries[1].state; location.href = entries[1].url; location.hash = new URL(location.href).hash;
await pop({ state: history.state });
assert.equal(position, 0, 'cancelled forward restores the earlier entry');
assert.equal(S.flagKey, 'forward-unsaved');
assert.equal(routes, 1);
shouldLeave = true;
sandbox.failLoad = true;
sandbox.reported = null;
vm.runInContext('loadWorkspaces = async () => { throw new Error("offline"); }; reportError = error => { globalThis.reported = error.message; };', sandbox);
position = 1; history.state = entries[1].state; location.href = entries[1].url; location.hash = new URL(location.href).hash;
await pop({ state: history.state });
assert.equal(position, 0, 'failed destination loading restores history');
assert.equal(S.flagKey, 'forward-unsaved', 'failed loading preserves editor state');
assert.equal(sandbox.reported, 'offline');
assert.equal(routes, 1);
console.log('Resilience contract: header/body deadlines, abort, no retries and dirty back navigation passed');
