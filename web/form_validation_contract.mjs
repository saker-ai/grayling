import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const files = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js'];
const code = files.map(file => fs.readFileSync(new URL(file, import.meta.url), 'utf8')).join('\n');
const sandbox = { console, document: { addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; } },
  navigator: { language: 'en' }, localStorage: { getItem() { return null; } },
  location: { hash: '', search: '', href: 'http://localhost/' }, history: {}, setTimeout, clearTimeout };
vm.runInNewContext(code + '\n;globalThis.forms = { metricDefinitionFromForm, validateReleaseSteps };', sandbox);
const { metricDefinitionFromForm: metric, validateReleaseSteps: release } = sandbox.forms;
const valid = { key: 'checkout.rate', displayName: 'Checkout rate', owner: 'analytics', description: '', eventKeys: 'checkout,visit', windowDays: '7', spec: '{"type":"proportion","direction":"increase"}' };
const body = metric(valid);
assert.equal(body.spec.key, 'checkout');
assert.equal(body.attributionWindow, 7 * 24 * 60 * 60 * 1e9);
assert.equal(valid.spec, '{"type":"proportion","direction":"increase"}', 'validation does not mutate typed form values');
for (const patch of [
  { key: 'Bad key' }, { displayName: ' ' }, { owner: '' }, { eventKeys: 'checkout,checkout' },
  { eventKeys: 'checkout,' }, { eventKeys: 'invalid key' }, { eventKeys: Array.from({ length: 33 }, (_, i) => 'event' + i).join(',') },
  { windowDays: '' }, { windowDays: 'NaN' }, { windowDays: 'Infinity' }, { windowDays: '31' },
  { spec: 'null' }, { spec: '[]' }, { spec: '{' }, { spec: '{"type":"unknown","direction":"increase"}' },
  { spec: '{"type":"mean","direction":"up"}' }, { spec: '{"type":"mean","direction":"increase","key":"absent"}' },
  { spec: '{"type":"ratio","direction":"increase","denominatorKey":"absent"}' },
  { spec: '{"type":"quantile","direction":"increase","quantileLevel":1}' },
  { spec: '{"type":"quantile","direction":"increase","quantileLevel":"0.95"}' },
  { spec: '{"type":"mean","direction":"increase","floor":"0"}' },
  { spec: '{"type":"mean","direction":"increase","floor":10,"ceil":1}' },
  { spec: '{"type":"mean","direction":"increase","ceil":1e400}' },
  { spec: '{"type":"funnel","direction":"increase","funnel":{"steps":["checkout","missing"],"dedupUnit":"user","stepTimeout":1000}}' },
  { spec: '{"type":"retention","direction":"increase","retention":{"returnKey":"checkout","day":2,"timezone":"UTC"}}' },
]) assert.throws(() => metric({ ...valid, ...patch }), undefined, 'reject malformed metric input ' + JSON.stringify(patch));
assert.equal(metric({ ...valid, spec: '{"type":"ratio","direction":"increase","denominatorKey":"visit"}' }).spec.denominatorKey, 'visit');
const step = { name: 'canary', ratio: 5, minRuntime: 3600e9, observationWindow: 0 };
assert.equal(release([step, { ...step, name: 'full', ratio: 100 }]), '');
for (const rows of [[], Array.from({ length: 33 }, (_, i) => ({ ...step, name: String(i), ratio: i + 1 })),
  [{ ...step, name: '' }], [step, { ...step, ratio: 10 }], [step, { ...step, name: 'next', ratio: 5 }],
  [{ ...step, ratio: NaN }], [{ ...step, ratio: Infinity }], [{ ...step, minRuntime: Infinity }],
  [{ ...step, minRuntime: 0 }], [{ ...step, observationWindow: NaN }], [{ ...step, observationWindow: -1 }],
  [{ ...step, observationWindow: Infinity }], [step, { name: '', ratio: 0, minRuntime: 0, observationWindow: 0 }],
]) assert.notEqual(release(rows), '', 'reject malformed release plan without dropping incomplete rows');
console.log('Form validation contract: metric definitions and staged release steps reject malformed inputs');
