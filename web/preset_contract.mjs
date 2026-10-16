// Contract for the console's built-in gray-release presets (web/presets.js).
//
// It loads the REAL console sources (i18n.js + presets.js + the editors +
// approvals + app.js) in a Node vm and asserts:
//   1. every preset builds a fresh, independent document for the requested key
//      and the ids inside each catalog are unique;
//   2. the console's own linters accept every document (lintFlag/lintSegment) —
//      the structured editor shows exactly the same verdict before a save;
//   3. every preset's nameKey/descKey/hintKey resolves in BOTH catalogs, is
//      unique per preset and never lands in I18N.missing, and the picker's own
//      labels resolve too;
//   4. the wiring: the create dialogs store the pick in S.flagPreset/S.segPreset,
//      the detail views seed #flag-json/#seg-json from it, both toolbars list
//      every preset, and the toolbar picker really replaces the JSON source,
//      repaints the structured form and clears the select;
//   5. it prints one machine-readable line — `preset_contract.presets {…}` — so
//      web/render_contract_test.go can re-validate every document with the real
//      server validators (model.ValidateFlag / model.ValidateSegment). The
//      console linter and the server validator are different code paths: a
//      preset is only trustworthy when both accept it.
import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';

const sources = ['i18n.js', 'presets.js', 'flag_editor.js', 'segment_editor.js', 'approvals.js', 'exp_workbench.js', 'app.js'];
const appSource = fs.readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const code = sources.map(name => fs.readFileSync(new URL(name, import.meta.url), 'utf8')).join('\n') + '\n' +
  ';globalThis.x = { S, VIEWS, I18N, t, FLAG_PRESETS, SEGMENT_PRESETS, presetFlagContent, presetSegmentContent, lintFlag, lintSegment };';

// --- DOM stubs --------------------------------------------------------------
// Every created element is registered so a test can reach the modal that
// actionDialog/promptForm just appended, and querySelector caches per selector
// so `$('#x', root)` returns the same node the console bound a handler to.
const created = [];
const element = () => {
  const classes = new Set();
  const kids = new Map();
  const node = {
    innerHTML: '', textContent: '', value: '', checked: false, className: '', disabled: false,
    style: {}, dataset: {}, children: [],
    classList: {
      add: c => classes.add(c), remove: c => classes.delete(c),
      contains: c => classes.has(c), toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)),
    },
    append() {}, after() {}, addEventListener() {}, appendChild(child) { node.children.push(child); }, remove() {},
    parentElement: { insertAdjacentElement() {} },
    matches() { return false; }, removeAttribute() {},
    setAttribute() {}, focus() {}, select() {}, closest() { return null; }, reportValidity() { return true; },
    replaceChildren(child) { node.innerHTML = child.innerHTML; },
    querySelector(sel) { if (!kids.has(sel)) kids.set(sel, element()); return kids.get(sel); },
    querySelectorAll(sel) { return sel === '[data-rule-segments], #seg-references' ? [] : [node.querySelector(sel)]; },
  };
  created.push(node);
  return node;
};
const chrome = new Map();
const sandbox = {
  document: {
    documentElement: { dataset: {} }, activeElement: null,
    addEventListener() {}, createElement: element, querySelectorAll() { return []; },
    querySelector(sel) { if (!chrome.has(sel)) chrome.set(sel, element()); return chrome.get(sel); },
    body: { appendChild() {} }, title: '',
  },
  localStorage: { getItem: k => (k === 'grayling_lang' ? 'en' : null), setItem() {}, removeItem() {} },
  navigator: { language: 'en-US' }, location: { origin: 'http://localhost', hash: '', href: 'http://localhost/', search: '' },
  history: { pushState() {}, replaceState() {} },
  setTimeout: () => 0, clearInterval() {}, clearTimeout() {}, console,
  AbortController,
  fetch: async url => mockResponse(url),
};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInContext(code, sandbox);
const { S, VIEWS, I18N, t, FLAG_PRESETS, SEGMENT_PRESETS,
  presetFlagContent, presetSegmentContent, lintFlag, lintSegment } = sandbox.x;

// The list endpoints the detail views fetch first; every other path answers an
// empty document, which is exactly the "brand new object" case a preset seeds.
function mockResponse(url) {
  const json = value => Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(value)) });
  if (url.endsWith('/flags')) return json([]);
  if (url.endsWith('/segments')) return json([]);
  return json({});
}

// viewRoot answers #id selectors with a cached stub (like the real DOM after a
// render), so bindings and the assertions below share the same node objects.
function viewRoot() {
  const nodes = new Map();
  return {
    innerHTML: '', dataset: {},
    addEventListener() {},
    querySelector(sel) { if (!nodes.has(sel)) nodes.set(sel, element()); return nodes.get(sel); },
    querySelectorAll(sel) { return [this.querySelector(sel)]; },
  };
}
const lastOverlay = () => [...created].reverse().find(node => node.innerHTML.includes('id="action-confirm"'));
const tick = () => new Promise(resolve => setImmediate(resolve));

S.ws = 'acme';
S.env = 'prod';
S.capabilities = { workspaceId: 'acme',
  permissions: Object.fromEntries(['flag.read', 'flag.write', 'flag.approve', 'flag.publish'].map(p => [p, true])) };

// --- 1. catalog shape -------------------------------------------------------
const KEY = preset => 'preset-' + preset.id;
const assertUnique = (list, label) => {
  const seen = new Set();
  for (const preset of list) {
    assert.ok(preset.id && typeof preset.id === 'string', label + ': every preset needs a non-empty id');
    assert.ok(!seen.has(preset.id), label + ': duplicate preset id ' + preset.id);
    seen.add(preset.id);
    assert.equal(typeof preset.build, 'function', label + ': ' + preset.id + ' needs a build(key) function');
  }
};
assertUnique(FLAG_PRESETS, 'FLAG_PRESETS');
assertUnique(SEGMENT_PRESETS, 'SEGMENT_PRESETS');
assert.ok(FLAG_PRESETS.length >= 6, 'the flag preset catalog must ship at least 6 presets');
assert.ok(SEGMENT_PRESETS.length >= 5, 'the segment preset catalog must ship at least 5 presets');

// --- 2. documents pass the console linter -----------------------------------
const build = (preset, lint, label) => {
  const key = KEY(preset);
  const content = preset.build(key);
  assert.ok(content && typeof content === 'object' && !Array.isArray(content),
    label + ' ' + preset.id + ' must build a document object');
  assert.equal(content.key, key, label + ' ' + preset.id + ' must use the requested key');
  // lint* runs inside the vm realm: compare serialized issues, not prototypes.
  const issues = lint(content);
  assert.equal(issues.length, 0, label + ' ' + preset.id + ' must pass the console linter: ' + issues.join(' · '));
  const probe = preset.build(key);
  probe.key = 'mutated-by-the-test';
  assert.equal(preset.build(key).key, key,
    label + ' ' + preset.id + ' must return a fresh document on every build (no shared template)');
  return { id: preset.id, content };
};
const flagDocuments = FLAG_PRESETS.map(preset => build(preset, lintFlag, 'flag'));
const segmentDocuments = SEGMENT_PRESETS.map(preset => build(preset, lintSegment, 'segment'));
assert.ok(flagDocuments.every(doc => doc.content.schemaVersion === 1), 'flag presets must carry schemaVersion 1');
assert.ok(segmentDocuments.every(doc => doc.content.schemaVersion === 1), 'segment presets must carry schemaVersion 1');

// --- 2b. the generated time windows never rot --------------------------------
// The window presets compute their timestamps when the operator applies them, so
// these assertions deliberately never name an absolute instant: a written-down
// date would pass a "parses as RFC3339" check for years and then silently pin
// every unit to the control value, which reads as a broken preset.
const constraintsOf = content => content.constraints || content.targeting.flatMap(rule => rule.constraints || []);
const windowOf = content => {
  const operands = constraintsOf(content).filter(c => c.op === 'time_after' || c.op === 'time_before');
  return { after: operands.find(c => c.op === 'time_after'), before: operands.find(c => c.op === 'time_before') };
};
// Assert the operand is a timestamp the API accepts and carries second precision
// (no milliseconds), so the generated JSON stays readable.
const operandMs = (operand, label) => {
  assert.ok(operand, label + ' must generate the missing time operand');
  assert.match(String(operand.value), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/,
    label + ' must generate a second-precision RFC3339 timestamp, got ' + operand.value);
  return Date.parse(operand.value);
};
// Freezing the vm's clock proves the value is computed per apply rather than
// written down: the same preset must move with the clock. Date.now is patched
// inside the context (the realm owns its intrinsics) and restored right after.
vm.runInContext('globalThis.__realNow = Date.now;', sandbox);
const buildAt = (preset, ms) => {
  vm.runInContext('Date.now = () => ' + ms + ';', sandbox);
  try {
    return preset.build('clock-probe');
  } finally {
    vm.runInContext('Date.now = globalThis.__realNow;', sandbox);
  }
};
for (const [label, preset] of [['time-window', FLAG_PRESETS.find(p => p.id === 'time-window')],
  ['active-window', SEGMENT_PRESETS.find(p => p.id === 'active-window')]]) {
  assert.ok(preset, label + ': the window preset must ship in the catalog');
  const where = part => label + ' ' + part + ': ';
  const window = windowOf(preset.build('probe'));
  const afterMs = operandMs(window.after, where('time_after'));
  const beforeMs = operandMs(window.before, where('time_before'));
  assert.ok(afterMs > Date.now(), where('time_after') + 'must open in the future, got ' + window.after.value);
  assert.ok(beforeMs > afterMs, where('time_before') + 'must close after it opens');
  assert.ok(beforeMs - afterMs >= 24 * 3600e3, where('window') + 'must span at least a day');
  const t0 = Date.parse('2026-10-15T00:00:00Z');
  const first = windowOf(buildAt(preset, t0));
  const second = windowOf(buildAt(preset, t0 + 86400e3));
  assert.notEqual(first.after.value, second.after.value,
    where('time_after') + 'must be recomputed on every apply, not written down in the source');
  assert.ok(Date.parse(first.after.value) > t0 && Date.parse(first.after.value) - t0 <= 2 * 3600e3,
    where('time_after') + 'must open shortly after the apply moment');
}

// --- 3. i18n keys -----------------------------------------------------------
const keysOf = preset => [preset.nameKey, preset.descKey, preset.hintKey].filter(Boolean);
const presets = FLAG_PRESETS.concat(SEGMENT_PRESETS);
const allKeys = presets.flatMap(keysOf);
assert.equal(new Set(allKeys).size, allKeys.length, 'every preset needs its own i18n keys');
for (const key of allKeys) {
  assert.ok(!I18N.missing.includes(key), 'preset key must resolve while rendering: ' + key);
  for (const [lang, catalog] of Object.entries({ en: I18N.catalogs.en, zh: I18N.catalogs.zh })) {
    assert.ok(Object.hasOwn(catalog, key), lang + ' catalog is missing ' + key);
    assert.ok(catalog[key].trim().length > 0, lang + ' catalog has an empty string for ' + key);
  }
}
for (const key of ['preset.label', 'preset.none', 'preset.applyHint', 'preset.applyTitle',
  'preset.applySummary', 'preset.applyConfirm', 'preset.applied']) {
  for (const catalog of [I18N.catalogs.en, I18N.catalogs.zh]) assert.ok(Object.hasOwn(catalog, key), 'missing ' + key);
  assert.notEqual(t(key), key, key + ' must resolve to a translated string');
}

// --- 4. wiring --------------------------------------------------------------
// The create dialogs are the second entry point; they hand the pick to the
// detail view through S.flagPreset/S.segPreset.
const flat = appSource.replace(/\s+/g, ' ');
const wired = (fragment, why) => assert.ok(flat.includes(fragment), why);
wired("{ name: 'preset', label: t('preset.label'), type: 'select', options: presetOptions(FLAG_PRESETS) }",
  'the new-flag dialog must offer the flag presets');
wired("{ name: 'preset', label: t('preset.label'), type: 'select', options: presetOptions(SEGMENT_PRESETS) }",
  'the new-segment dialog must offer the segment presets');
wired("S.flagKey = key.trim(); S.flagNew = true; S.flagPreset = preset || '';",
  'the new-flag dialog must store the picked preset');
wired("S.segKey = key.trim(); S.segPreset = preset || '';",
  'the new-segment dialog must store the picked preset');
wired("presetSelect('flag-preset', FLAG_PRESETS)", 'the flag editor toolbar must render the preset picker');
wired("presetSelect('seg-preset', SEGMENT_PRESETS)", 'the segment editor toolbar must render the preset picker');

// Unknown ids and the empty pick fall back to the blank template.
for (const lookup of [presetFlagContent, presetSegmentContent]) {
  assert.equal(lookup('', 'fallback-probe'), null, 'an empty pick must fall back to the blank template');
  assert.equal(lookup('nope-not-a-preset', 'fallback-probe'), null, 'an unknown id must fall back too');
}
assert.equal(presetFlagContent('percent-rollout', 'fallback-probe').key, 'fallback-probe');

// The create-dialog pick seeds the brand-new draft: the detail view renders the
// preset document into the JSON source that backs the structured editor.
const selected = FLAG_PRESETS.find(preset => preset.id === 'percent-rollout');
S.flagNew = true;
S.flagKey = 'preset-probe-flag';
S.flagPreset = selected.id;
const flagRoot = viewRoot();
await VIEWS.flags(flagRoot);
assert.equal(flagRoot.querySelector('#flag-json').value, JSON.stringify(selected.build(S.flagKey), null, 2),
  'a brand-new flag editor must be seeded from the create-dialog preset');
for (const preset of FLAG_PRESETS) {
  assert.ok(flagRoot.innerHTML.includes('value="' + preset.id + '"'),
    'the flag toolbar must list preset ' + preset.id);
  assert.ok(flagRoot.innerHTML.includes(t(preset.nameKey)), 'the flag toolbar must label ' + preset.id);
}

const segment = SEGMENT_PRESETS.find(preset => preset.id === 'user-list');
S.segKey = 'preset-probe-segment';
S.segPreset = segment.id;
const segRoot = viewRoot();
await VIEWS.segments(segRoot);
assert.equal(segRoot.querySelector('#seg-json').value, JSON.stringify(segment.build(S.segKey), null, 2),
  'a brand-new segment editor must be seeded from the create-dialog preset');
for (const preset of SEGMENT_PRESETS) {
  assert.ok(segRoot.innerHTML.includes('value="' + preset.id + '"'),
    'the segment toolbar must list preset ' + preset.id);
  assert.ok(segRoot.innerHTML.includes(t(preset.nameKey)), 'the segment toolbar must label ' + preset.id);
}

// The toolbar picker: confirm, replace the JSON source, repaint the structured
// form from the new document, clear the select. The pick differs from the
// create-dialog seed on purpose, so the replacement is observable.
const flagPicker = flagRoot.querySelector('#flag-preset');
const killSwitch = FLAG_PRESETS.find(preset => preset.id === 'kill-switch');
const before = flagRoot.querySelector('#flag-json').value;
flagPicker.value = killSwitch.id;
const applied = flagPicker.onchange();
await tick();
const overlay = lastOverlay();
assert.ok(overlay, 'applying a preset must open a confirmation dialog');
assert.ok(overlay.innerHTML.includes(t(killSwitch.nameKey)) &&
  overlay.innerHTML.includes(t(killSwitch.descKey)),
  'the confirmation must name and describe the preset before it is applied');
await overlay.querySelector('#action-confirm').onclick();
await applied;
assert.equal(flagPicker.value, '', 'the picker must not keep looking "applied"');
const after = flagRoot.querySelector('#flag-json').value;
assert.notEqual(after, before, 'applying a preset must replace the JSON source');
assert.equal(after, JSON.stringify(killSwitch.build(S.flagKey), null, 2),
  'the applied document must match the preset');
assert.ok(flagRoot.querySelector('#flag-structured').innerHTML.includes('data-var-name value="off"'),
  'the structured form must be repainted from the applied preset');

// Cancelling leaves the draft untouched.
const cancelled = flagRoot.querySelector('#flag-json').value;
flagPicker.value = 'percent-rollout';
const dropped = flagPicker.onchange();
await tick();
await lastOverlay().querySelector('#action-cancel').onclick();
await dropped;
assert.equal(flagRoot.querySelector('#flag-json').value, cancelled, 'cancelling must not touch the draft');

// Segment picker: same contract through bindSegmentStructured.
const segPicker = segRoot.querySelector('#seg-preset');
segPicker.value = segment.id;
const segApplied = segPicker.onchange();
await tick();
await lastOverlay().querySelector('#action-confirm').onclick();
await segApplied;
assert.equal(segRoot.querySelector('#seg-json').value, JSON.stringify(segment.build(S.segKey), null, 2),
  'the segment picker must replace the JSON source with the preset');
assert.ok(segRoot.querySelector('#seg-structured').innerHTML.includes('alice'),
  'the segment structured form must be repainted from the preset');

// Applying rule templates preserves descriptive metadata in both editors.
for (const [root, id, presetId, keyName] of [[flagRoot, 'flag', 'kill-switch', 'flagKey'], [segRoot, 'seg', 'email-domain', 'segKey']]) {
  const source = root.querySelector('#' + id + '-json');
  const named = JSON.parse(source.value);
  named.displayName = '\u4e2d\u6587\u540d\u79f0'; named.description = 'Context survives template replacement';
  source.value = JSON.stringify(named);
  const picker = root.querySelector('#' + id + '-preset'); picker.value = presetId;
  const apply = picker.onchange(); await tick();
  await lastOverlay().querySelector('#action-confirm').onclick(); await apply;
  const result = JSON.parse(source.value);
  assert.equal(result.displayName, named.displayName, 'template replacement retains the display name');
  assert.equal(result.description, named.description, 'template replacement retains the description');
  assert.equal(result.key, S[keyName], 'template replacement retains the machine key');
}

const seed = { displayName: '\u65b0\u7b56\u7565', description: 'Only saved when the operator saves the draft' };
S.flagKey = 'metadata-seed-flag'; S.flagNew = true; S.flagPreset = 'kill-switch'; S.flagMetadata = seed;
const metadataFlagRoot = viewRoot(); await VIEWS.flags(metadataFlagRoot);
const seededFlag = JSON.parse(metadataFlagRoot.querySelector('#flag-json').value);
assert.equal(seededFlag.displayName, seed.displayName, 'create-dialog display name seeds the unsaved flag');
assert.equal(seededFlag.key, 'metadata-seed-flag', 'display name never replaces the SDK key');
S.segKey = 'metadata-seed-segment'; S.segPreset = 'user-list'; S.segMetadata = seed;
const metadataSegmentRoot = viewRoot(); await VIEWS.segments(metadataSegmentRoot);
assert.equal(JSON.parse(metadataSegmentRoot.querySelector('#seg-json').value).description, seed.description, 'create-dialog description seeds the unsaved segment');
S.flagMetadata = S.segMetadata = null;

// --- 5. machine-readable hand-off to the Go side ----------------------------
console.log('preset_contract.presets ' + JSON.stringify({ flags: flagDocuments, segments: segmentDocuments }));
console.log('preset contract: all assertions passed (' + flagDocuments.length + ' flag presets + ' +
  segmentDocuments.length + ' segment presets × lint + i18n en/zh + create-dialog seeds + toolbar picker + ' +
  'apply-time windows that still open in the future)');