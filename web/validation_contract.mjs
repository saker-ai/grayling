import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js']
  .map(name => fs.readFileSync(new URL(name, import.meta.url), 'utf8')).join('\n');
const element = () => {
  const children = new Map();
  return { value: '', checked: true, innerHTML: '', textContent: '', dataset: {},
    classList: { add() {}, remove() {}, toggle() {} }, addEventListener() {}, appendChild() {},
    querySelector(selector) { if (!children.has(selector)) children.set(selector, element()); return children.get(selector); },
    querySelectorAll() { return []; },
  };
};
const chrome = element();
let writes = 0;
const sandbox = { document: { addEventListener() {}, querySelector: s => chrome.querySelector(s), querySelectorAll() { return []; }, createElement: element },
  window: {}, navigator: {}, location: { href: 'http://localhost/', origin: 'http://localhost', hash: '' },
  history: { state: null }, localStorage: { getItem() { return null; } }, console, AbortController, setTimeout, clearTimeout,
  fetch: async (_, opts) => { if (opts.method !== 'GET') writes++; return { ok: true, text: async () => '[]' }; },
};
vm.createContext(sandbox);
vm.runInContext(source + '\nglobalThis.x = { S, collectAllowlistEntries, collectExperimentGuardrails, experimentNumericIssue, validateCreationSlug, validateTokenScope, viewAI, viewTokens }; toast = () => {};', sandbox);
const x = sandbox.x;
const invalid = fn => assert.throws(fn, e => e.code === 'validation_failed');
invalid(() => x.collectAllowlistEntries([{ provider: 'openai', model: '', enabled: true }]));
assert.equal(x.collectAllowlistEntries([{ provider: '', model: '', enabled: true }]).length, 0);
assert.equal(x.collectAllowlistEntries([{ provider: 'openai', model: 'gpt', enabled: false }])[0].enabled, false);
const guard = { metricKey: 'latency', type: 'mean', direction: 'decrease', threshold: '0', sample: '0', journey: '' };
assert.equal(x.collectExperimentGuardrails([guard])[0].minSample, 0);
for (const patch of [{ metricKey: '' }, { threshold: '-1' }, { threshold: 'Infinity' }, { sample: '0.5' }, { sample: '-1' }]) {
 invalid(() => x.collectExperimentGuardrails([{ ...guard, ...patch }]));
}
invalid(() => x.collectExperimentGuardrails([{ ...guard, type: 'funnel', sample: '1', journey: 'null' }]));
assert.equal(x.collectExperimentGuardrails([{ metricKey: '', threshold: '', sample: '', journey: '', type: 'proportion', direction: 'increase' }]).length, 0);
const stats = { alpha: '.05', power: '.8', mde: '.01', minsample: '0', minruntime: '0.5', window: '7', elig: '' };
assert.equal(x.experimentNumericIssue(stats), null);
for (const [key, bad] of [['power','1'], ['power','0'], ['mde','0'], ['minsample','1.5'], ['minruntime','-1'], ['window','Infinity'], ['elig','101'], ['elig','-1']]) {
 assert.equal(x.experimentNumericIssue({ ...stats, [key]: bad }).field, key);
}
for (const slug of ['中文', 'Upper', '-bad', 'a'.repeat(65)]) invalid(() => x.validateCreationSlug(slug));
x.validateCreationSlug('a-b0');
invalid(() => x.validateTokenScope({ kind: 'sdk', environment: '' }));
x.validateTokenScope({ kind: 'admin', environment: '' });
// Drive the real handlers: partial rows must retain input and issue no writes.
x.S.ws = 'acme'; x.S.env = 'prod';
const root = element();
await x.viewAI(root);
const row = element();
row.querySelector('.al-provider').value = 'openai';
const allow = root.querySelector('#ai-allow');
allow.querySelectorAll = selector => selector === '.vrow' ? [row] : [];
root.querySelector('#ai-allow-save').onclick();
assert.equal(writes, 0, 'partially completed allowlist must not replace saved entries');
assert.equal(row.querySelector('.al-provider').value, 'openai', 'rejected save keeps input');
x.S.capabilities = { permissions: { "token.manage": true } };
const tokenRoot = element();
await x.viewTokens(tokenRoot);
tokenRoot.querySelector('#t-name').value = 'SDK token';
tokenRoot.querySelector('#t-kind').value = 'sdk';
await tokenRoot.querySelector('#t-create').onclick();
assert.equal(writes, 0, 'SDK token without environment must not be submitted');
console.log('Validation contract: incomplete rows preserve input, no invalid writes, numeric and identifier boundaries passed');
