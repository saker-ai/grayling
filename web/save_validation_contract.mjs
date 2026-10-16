// Exercise the real save handlers in JSON mode, where structured controls
// cannot be relied upon to validate a document before it reaches the API.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js']
  .map(file => fs.readFileSync(new URL(file, import.meta.url), 'utf8')).join('\n');
function element() {
  const children = new Map();
  return { value: '', innerHTML: '', textContent: '', dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    querySelector(selector) { if (!children.has(selector)) children.set(selector, element()); return children.get(selector); },
    querySelectorAll() { return []; }, addEventListener() {}, setAttribute() {},
    append() {}, appendChild() {}, after() {}, remove() {}, focus() {}, scrollIntoView() {},
  };
}
const chrome = element();
let documentBody;
let kind;
let writes;
let saveFailure = false;
const sandbox = { document: { addEventListener() {}, querySelector: selector => chrome.querySelector(selector),
  querySelectorAll() { return []; }, createElement: element },
  navigator: {}, window: {}, localStorage: { getItem() { return null; } },
  location: { origin: 'http://localhost', hash: '', href: 'http://localhost/' }, history: {},
  AbortController, setTimeout, clearTimeout, console, messages: [],
  fetch: async (url, options) => {
    if (options.method !== 'GET') {
      writes++;
      return saveFailure ? { ok: false, status: 400, text: async () => JSON.stringify({ error: {
        code: 'validation_failed', message: 'server rejected the draft',
      } }) } : { ok: true, text: async () => '{}' };
    }
    let data = {};
    if (kind === 'flag' && /\/flags\//.test(url)) data = { key: documentBody.key,
      draft: { content: documentBody, state: 'editing' }, currentRevision: 0 };
    if (kind === 'seg' && url.endsWith('/segments')) data = [{ key: documentBody.key,
      content: documentBody, state: 'editing' }];
    if (url.endsWith('/history')) data = { revisions: [] };
    return { ok: true, text: async () => JSON.stringify(data) };
  },
};
vm.createContext(sandbox);
vm.runInContext(source + `\nglobalThis.x = { S, VIEWS, lintFlag, presetFlagContent };
bindFlagStructured = bindSegmentStructured = () => ({ valid: () => true, redraw() {} });
editorValue = () => 'edited-document';
route = async () => {};
toast = (message, ok = true) => { globalThis.messages.push({ message, ok }); };
`, sandbox);
const { S, VIEWS, lintFlag, presetFlagContent } = sandbox.x;
S.ws = 'acme'; S.env = 'prod'; S.capabilities = { permissions: { 'flag.write': true } };
for (kind of ['flag', 'seg']) {
  const valid = kind === 'flag' ? JSON.parse(JSON.stringify(presetFlagContent('allowlist', 'office-network'))) :
    { schemaVersion: 1, key: 'office-network', constraints: [{ field: 'country', op: 'eq', value: 'CN' }], segments: [] };
  for (const patch of [{ key: '办公网' }, { key: 'office\n' }, { schemaVersion: 2 },
    { displayName: '中'.repeat(129) }, { description: '中'.repeat(2049) }]) {
    documentBody = { ...valid, ...patch }; writes = 0;
    S.flagKey = kind === 'flag' ? documentBody.key : null;
    S.segKey = kind === 'seg' ? documentBody.key : null;
    const root = element();
    await VIEWS[kind === 'flag' ? 'flags' : 'segments'](root);
    await root.querySelector('#' + kind + '-save').onclick();
    assert.equal(writes, 0, `${kind}: invalid JSON document must not issue a save request`);
    assert(root.querySelector('#' + kind + '-valid').textContent, 'show a persistent validation error');
    assert.equal(JSON.parse(root.querySelector('#' + kind + '-json').value).key, documentBody.key, 'retain invalid input for correction');
  }
  documentBody = { ...valid, displayName: '办公网', description: '仅向内网开放' }; writes = 0;
  S.flagKey = kind === 'flag' ? valid.key : null; S.segKey = kind === 'seg' ? valid.key : null;
  const root = element();
  await VIEWS[kind === 'flag' ? 'flags' : 'segments'](root);
  await root.querySelector('#' + kind + '-save').onclick();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(writes, 1, `${kind}: valid ASCII key with Chinese metadata can save`);
  writes = 0; saveFailure = true; sandbox.messages.length = 0; S.editorBaseline = 'before-save';
  await root.querySelector('#' + kind + '-save').onclick();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(writes, 1);
  assert.equal(S.editorBaseline, 'before-save', 'rejected save must not mark inputs as saved');
  assert(sandbox.messages.some(message => !message.ok && message.message.includes('server rejected')));
  assert(!sandbox.messages.some(message => message.ok), 'server rejection must never show a saved notification');
  saveFailure = false;
}
const flag = JSON.parse(JSON.stringify(presetFlagContent('allowlist', 'office-network')));
for (const patch of [
  { variations: { 办公网: true } }, { fields: ['user_id', ''] },
  { targeting: [{ variation: 'toString' }] },
  { targeting: [{ name: 'same', variation: 'control' }, { name: 'same', variation: 'candidate' }] },
  { targeting: [{ percentage: { control: 99.999, candidate: 0 } }] },
  { targeting: [{ percentage: { control: 95, candidate: 5 } }], bucketing: null },
]) assert(lintFlag({ ...flag, ...patch }).length, 'reject invalid rule and identifier structure');
console.log('Save validation: invalid keys/documents issue zero writes, keep inputs, and valid Chinese metadata saves');
