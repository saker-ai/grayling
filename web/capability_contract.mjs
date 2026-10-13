import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';

const code = fs.readFileSync(new URL('./i18n.js', import.meta.url), 'utf8') + '\n' +
  fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8') + '\n' +
  ';globalThis.x = { S, can, gateActions, updateCapabilities, refreshCapabilities, errMsg, errBanner, ApiError, VIEWS, I18N, setLang, route, registerI18nHooks };';
const element = () => {
  const classes = new Set();
  return { innerHTML: '', textContent: '', value: '', dataset: {},
    classList: { add: c => classes.add(c), remove: c => classes.delete(c),
      contains: c => classes.has(c), toggle: (c, on) => on ? classes.add(c) : classes.delete(c) },
    addEventListener() {}, appendChild() {}, replaceChildren(child) { this.innerHTML = child.innerHTML; },
    querySelector() { return element(); }, querySelectorAll() { return []; } };
};
const chrome = new Map();
const root = { innerHTML: '', nodes: new Map(), querySelector(selector) {
  if (!this.nodes.has(selector)) this.nodes.set(selector, element());
  return this.nodes.get(selector);
}, querySelectorAll(selector) { return [this.querySelector(selector)]; } };
const responses = new Map();
const sandbox = {
  document: { addEventListener() {}, querySelector(s) { if (!chrome.has(s)) chrome.set(s, element()); return chrome.get(s); },
    querySelectorAll() { return []; }, createElement: element, body: { appendChild() {} } },
  localStorage: { getItem() { return null; }, setItem() {} },
  navigator: { language: 'en-US' }, location: { origin: 'http://localhost' },
  setTimeout() {}, clearInterval() {}, console,
  fetch: async path => {
    const body = responses.get(path) ?? [];
    const status = body?.status || 200;
    return { ok: status < 400, status, text: async () => JSON.stringify(body) };
  },
};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInContext(code, sandbox);
const { S, can, gateActions, updateCapabilities, refreshCapabilities, errMsg, errBanner, ApiError, VIEWS, setLang } = sandbox.x;
S.ws = 'acme'; S.env = 'prod';
const byRole = {
  owner: ['token.manage', 'quota.manage', 'flag.read', 'flag.write', 'flag.approve', 'flag.publish', 'experiment.write', 'ai.write', 'ai.content.read'],
  publisher: ['flag.read', 'flag.write', 'flag.approve', 'flag.publish', 'experiment.write', 'ai.write'],
  analyst: ['flag.read', 'experiment.write', 'ai.write'],
  viewer: ['flag.read'],
};
for (const [role, permissions] of Object.entries(byRole)) {
  S.capabilities = { workspaceId: 'acme', permissions: Object.fromEntries(permissions.map(p => [p, true])) };
  updateCapabilities();
  assert.equal(chrome.get('[data-view="tokens"]').classList.contains('hidden'), role !== 'owner', `${role} token nav`);
  assert.equal(chrome.get('#nav-quotas').classList.contains('hidden'), role !== 'owner', `${role} quota nav`);
  assert(chrome.get('#btn-new-ws').classList.contains('hidden'), `${role} workspace creation`);
  const actions = { '#flag-save': 'flag.write', '#flag-approve': 'flag.approve', '#flag-publish': 'flag.publish',
    '#x-create': 'experiment.write', '#metric-new': 'experiment.write', '#metric-submit': 'experiment.write',
    '#metric-publish': 'flag.publish', '#metric-deprecate': 'flag.publish',
    '#ai-reg': 'ai.write', '[data-pin]': 'flag.publish', '#ai-resolve': 'ai.content.read' };
  const nodes = { querySelectorAll: () => [], };
  const actual = new Map();
  nodes.querySelectorAll = selector => {
    if (!actual.has(selector)) actual.set(selector, element());
    return [actual.get(selector)];
  };
  for (const selector of Object.keys(actions)) actual.set(selector, element());
  gateActions(nodes);
  for (const [selector, permission] of Object.entries(actions))
    assert.equal(actual.get(selector).classList.contains('hidden'), !can(permission), `${role} ${selector}`);
  if (role !== 'owner') {
    const page = element();
    await VIEWS.tokens(page);
    assert(page.innerHTML.includes('403'), `${role} direct token view denied`);
  }
}

// A global owner can create the first workspace, with no scoped capability URL.
S.ws = '';
responses.set('/api/v1/admin/capabilities', { 'workspace.admin': true });
await refreshCapabilities();
assert(!chrome.get('#btn-new-ws').classList.contains('hidden'), 'global owner first-workspace entry');
assert(chrome.get('#btn-new-env').classList.contains('hidden'), 'no workspace means no environment entry');
S.ws = 'acme';
responses.set('/api/v1/admin/capabilities', { 'workspace.admin': false });
// A workspace change clears old permissions while the new server read is pending.
S.capabilities = null;
updateCapabilities();
assert(chrome.get('[data-view="tokens"]').classList.contains('hidden'));
responses.set('/api/v1/admin/workspaces/acme/capabilities', { workspaceId: 'acme', permissions: { 'token.manage': true } });
await refreshCapabilities();
assert(!chrome.get('[data-view="tokens"]').classList.contains('hidden'));
S.ws = 'other';
responses.set('/api/v1/admin/workspaces/other/capabilities', { status: 403, error: { code: 'access_denied', message: 'out of scope' } });
await refreshCapabilities();
assert(chrome.get('[data-view="tokens"]').classList.contains('hidden'), 'cross workspace denial must not inherit old rights');
assert(!can('flag.read'), 'cross workspace denial grants no read capability');

const approvals = [
  { id: 'a1', state: 'submitted', action: 'flag.rollback', object: 'flag-a', environmentId: 'prod', submittedBy: 'alice' },
  { id: 'a2', state: 'approved', action: 'ai.pin', object: 'prompt/p1', environmentId: 'prod', submittedBy: 'alice' },
];
S.ws = 'acme';
responses.set('/api/v1/admin/workspaces/acme/approvals', approvals);
responses.set('/api/v1/admin/workspaces/acme/audit?limit=200', []);
for (const [role, wantApprove, wantExecute] of [['viewer', false, false], ['analyst', false, false], ['publisher', true, true], ['owner', true, true]]) {
  S.capabilities = { permissions: Object.fromEntries(byRole[role].map(p => [p, true])) };
  root.nodes.clear();
  await VIEWS.audit(root);
  const html = root.querySelector('#approval-box').innerHTML;
  assert.equal(html.includes('data-appr='), wantApprove, `${role} approval action`);
  assert.equal(html.includes('data-exec='), wantExecute, `${role} execution action`);
  assert(html.includes('submitted') && html.includes('approved'), 'approval states visible after reload');
}
for (const [status, english, chinese] of [
  [403, 'Access denied', '无权操作'], [409, 'Conflict', '发生冲突'], [429, 'Request limited', '请求受限'],
]) {
  const error = new ApiError(status, 'server', 'English server text');
  setLang('en');
  assert(errMsg(error).includes(english) && errBanner(error).includes(english));
  setLang('zh');
  assert(errMsg(error).includes(chinese) && errBanner(error).includes(chinese));
  assert(!errBanner(error).includes('English server text'), 'localized banner must not expose server text');
}
// The live route rerenders an error banner in the newly selected language.
sandbox.x.registerI18nHooks();
S.booted = true; S.view = 'flags';
for (const [status, english, chinese] of [
  [403, 'Access denied', '无权操作'], [409, 'Conflict', '发生冲突'], [429, 'Request limited', '请求受限'],
]) {
  VIEWS.flags = async () => { throw new ApiError(status, 'server', 'server message'); };
  setLang('en');
  await sandbox.x.route();
  assert(chrome.get('#main').innerHTML.includes(english), `en route ${status}`);
  setLang('zh');
  await new Promise(resolve => setTimeout(resolve, 0));
  assert(chrome.get('#main').innerHTML.includes(chinese), `zh route ${status}`);
}
console.log('capability role, workspace, approval state and bilingual error DOM contracts passed');
