// DOM/contract test for the admin console staged-rollout (release plan) UI of
// the experiment-detail view. It loads the REAL web/app.js in a Node vm (no
// browser needed), drives the pure renderRollout() with mock plans covering
// EVERY plan state (no plan / armed / running / manual takeover / paused —
// manual and SRM-blocked / rolled_back / completed), then binds the rendered
// markup with the real bindRollout() against a mock root whose querySelector
// answers ONLY IDs present in the rendered HTML — an unguarded binding to a
// missing ID throws and fails the contract (same drift protection as
// render_contract.mjs). Operator actions use a recording dialog stub to verify
// scope, required reasons, cancellation and the exact request body.
// Run via `go test ./web/` (which shells to node) or `node web/rollout_contract.mjs`.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import assert from 'node:assert';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
// Load i18n.js + app.js in ONE vm context (same script-scope order as the
// browser's <script> tags) so app.js resolves t()/I18N from the catalogs.
// The sandbox has no grayling_lang and no navigator.language → en (default).
const code = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'flag_editor.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'segment_editor.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'approvals.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'exp_workbench.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

const el = () => ({
  style: {}, dataset: {}, value: '', checked: false, innerHTML: '', textContent: '',
  classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  appendChild() {}, remove() {}, setAttribute() {}, addEventListener() {},
  querySelector() { return null; }, querySelectorAll() { return []; },
});

const fetchCalls = [];
let currentExperiment = null;
let reportUnavailable = false;
let reportFetches = 0;
let reportVersion = 'sha256:report-v1';
const sandbox = {
  document: {
    addEventListener() {}, createElement: el,
    // Universal stub for the app chrome touched by toast()/route() (#toasts,
    // #main, …) — those containers always exist in the real page.
    querySelector() { return el(); },
    querySelectorAll() { return []; },
    body: { appendChild() {} },
  },
  localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
  location: { hash: '', href: 'http://localhost/', search: '' },
  history: { pushState() {}, replaceState() {} },
  navigator: { clipboard: { writeText() { return Promise.resolve(); } } },
  console,
  // No-op timers keep the harness process from lingering on toast fade-outs.
  setInterval: () => 0, clearInterval() {}, setTimeout: () => 0, clearTimeout() {},
  fetch(url, opts) {
    if (opts.method === 'GET' && url === '/api/v1/admin/experiments/exp-1') {
      return Promise.resolve({ ok: true, status: 200,
        text: () => Promise.resolve(JSON.stringify(currentExperiment)) });
    }
    if (opts.method === 'GET' && url === '/api/v1/admin/experiments/exp-1/report') {
      reportFetches++;
      return Promise.resolve(reportUnavailable
        ? { ok: false, status: 503, statusText: 'report unavailable',
          text: () => Promise.resolve('{"error":{"message":"report unavailable"}}') }
        : { ok: true, status: 200, text: () => Promise.resolve(JSON.stringify({ healthy: false,
          evidenceVersion: reportVersion, decision: { verdict: 'hold' }, generatedAt: '2026-10-13T00:00:00Z',
          window: { watermark: '2026-10-10T00:00:00Z' } })) });
    }
    fetchCalls.push({ url, opts });
    return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{}') });
  },
  confirm: () => sandbox.__confirm !== false,
  prompt: () => (sandbox.__prompt === undefined ? '' : sandbox.__prompt),
};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(
  code + '\n;globalThis.__renderRollout = renderRollout;' +
  'globalThis.__bindRollout = bindRollout;' +
  'globalThis.__setRolloutReportStatus = setRolloutReportStatus;' +
  'globalThis.__planNextGate = planNextGate;',
  sandbox);
assert.strictEqual(typeof sandbox.__renderRollout, 'function', 'renderRollout() not exposed from app.js');
assert.strictEqual(typeof sandbox.__bindRollout, 'function', 'bindRollout() not exposed from app.js');
assert.strictEqual(typeof sandbox.__planNextGate, 'function', 'planNextGate() not exposed from app.js');
vm.runInNewContext("S.env = 'prod'", sandbox);
vm.runInNewContext("S.capabilities = { permissions: { 'flag.publish': true } }", sandbox);
const dialogs = [];
sandbox.actionDialog = async options => {
  dialogs.push(options);
  if (sandbox.__confirm === false) return null;
  const reason = sandbox.__prompt || '';
  if (options.reasonLabel && options.reasonRequired !== false && !reason) return null;
  await options.onConfirm({ reason });
  return { reason };
};

const render = exp => {
  const html = sandbox.__renderRollout(exp);
  assert.strictEqual(typeof html, 'string', 'renderRollout must return an HTML string');
  return html;
};

// makeRoot builds the bind target: querySelector answers ONLY IDs present in
// the rendered HTML (stable stub per ID), everything else is null — exactly
// like the real DOM after the innerHTML replacement.
function makeRoot(html) {
  const ids = new Set();
  for (const mt of html.matchAll(/id="([^"]+)"/g)) ids.add(mt[1]);
  const stubs = new Map();
  return {
    _ids: ids, _stubs: stubs, innerHTML: '', dataset: {},
    querySelector(sel) {
      if (typeof sel !== 'string' || !/^#[A-Za-z0-9_-]+$/.test(sel)) return null;
      const id = sel.slice(1);
      if (!ids.has(id)) return null;
      if (!stubs.has(id)) stubs.set(id, el());
      return stubs.get(id);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
}

// renderState renders + binds one plan state; binding throws when a bound ID
// is missing from the markup.
function renderState(exp) {
  currentExperiment = exp;
  const html = render(exp);
  const root = makeRoot(html);
  sandbox.__bindRollout(root, 'exp-1', exp);
  return { html, root };
}
const bound = (root, id) => {
  const s = root._stubs.get(id);
  return !!s && typeof s.onclick === 'function';
};
const tick = () => new Promise(r => setTimeout(r, 5)); // let async handlers settle

// --- mock plan data (wire shape of platform.ReleasePlan) -------------------
const NS_H = 3.6e12;
const steps = [
  { name: 'canary-5', ratio: 5, minRuntime: 24 * NS_H, observationWindow: 12 * NS_H, autoAdvance: true },
  { name: 'ramp-25', ratio: 25, minRuntime: 48 * NS_H, observationWindow: 24 * NS_H, autoAdvance: true },
  { name: 'ramp-100', ratio: 100, minRuntime: 72 * NS_H, autoAdvance: false },
];
const t0 = '2026-09-20T00:00:00Z';
const basePlan = {
  steps, state: 'running', currentStep: 1, stepStartedAt: t0,
  createdAt: t0, updatedAt: t0, createdBy: 'owner-token',
  history: [
    { at: t0, actor: 'rollout-engine', action: 'plan.start', step: 0, detail: 'experiment running; entering step "canary-5" at 5%' },
    { at: '2026-09-22T00:00:00Z', actor: 'rollout-engine', action: 'step.advance', step: 1, detail: 'gate verdict continue → step "ramp-25" at 25%' },
  ],
};
const withPlan = over => ({ id: 'exp-1', state: 'running', releasePlan: { ...basePlan, ...(over || {}) } });

// 1. no plan → pre-registration form only, no plan controls.
{
  const { html, root } = renderState({ id: 'exp-1', state: 'reviewed' });
  assert(html.includes('No release plan registered'), 'create-form hint missing');
  assert(html.includes('ro-steps-editor') && html.includes('ro-add-step') && html.includes('ro-create'),
    'create-form controls missing');
  assert(!html.includes('ro-pause') && !html.includes('ro-refresh') && !html.includes('ro-steps-tbl'),
    'plan controls must not render without a plan');
  assert(bound(root, 'ro-create') && bound(root, 'ro-add-step'), 'create-form buttons not bound');
  // Create click with zero collected rows → client-side validation, no request.
  sandbox.__confirm = true; fetchCalls.length = 0;
  root._stubs.get('ro-create').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'empty plan must not reach the API');
}

// 2. armed → waiting for the experiment, pause/takeover available, no gate time.
{
  const plan = { ...basePlan, state: 'armed', currentStep: 0, stepStartedAt: null, history: [] };
  const { html, root } = renderState({ id: 'exp-1', state: 'reviewed', releasePlan: plan });
  assert(html.includes('>plan armed<'), 'armed state chip missing');
  assert(html.includes('Armed — waiting for the experiment'), 'armed banner missing');
  assert(html.includes('id="ro-next-eval">—<'), 'armed plan must not claim a next-gate time');
  assert(html.includes('ro-pause') && html.includes('ro-takeover-on'), 'armed: pause/takeover must be available');
  assert(!html.includes('ro-resume') && !html.includes('ro-advance'), 'armed: resume/advance must not render');
  assert(bound(root, 'ro-pause') && bound(root, 'ro-takeover-on') && bound(root, 'ro-refresh'), 'armed actions not bound');
  assert.strictEqual(sandbox.__planNextGate(plan), null, 'planNextGate must be null when not running');
}

// 3. running → progress, next-gate time, step table, history, live actions.
{
  const exp = withPlan();
  const { html, root } = renderState(exp);
  assert(html.includes('>plan running<'), 'running chip missing');
  assert(html.includes('>engine auto<'), 'engine-auto chip missing');
  assert(html.includes('step 2/3'), 'current-step progress text missing');
  assert(html.includes('ramp-25') && html.includes('25%'), 'current step name/ratio missing');
  assert(html.includes('width:33%'), 'progress bar width wrong (1 of 3 steps done)');
  assert(!html.includes('id="ro-next-eval">—<'), 'running plan must expose the next-gate time');
  assert.strictEqual(sandbox.__planNextGate(exp.releasePlan).toISOString(), '2026-09-23T00:00:00.000Z',
    'next gate = stepStartedAt + minRuntime + observationWindow');
  assert(html.includes('ro-steps-tbl') && html.includes('Observation window'), 'plan-step table missing');
  assert(html.includes('id="ro-steps-tbl"') && html.includes('mobile-card-table') &&
    html.includes('mobile-record-cards') && html.includes('Eligibility'),
  'rollout steps must have readable mobile cards alongside the desktop table');
  assert(html.includes('>done<') && html.includes('>current<') && html.includes('>pending<'), 'step status chips missing');
  assert(html.includes('>auto<') && html.includes('>manual hold<'), 'advance-mode chips missing');
  assert(html.includes('ro-history-tbl'), 'history table missing');
  assert(html.includes('rollout-engine') && html.includes('mobile-record'),
    'action history must have mobile records with actor and detail');
  assert(html.indexOf('step.advance') < html.indexOf('plan.start'), 'history must render newest first');
  assert(html.includes('rollout-engine'), 'history actor missing');
  for (const id of ['ro-refresh', 'ro-pause', 'ro-advance', 'ro-takeover-on']) {
    assert(bound(root, id), id + ' not bound');
  }
  assert(!html.includes('ro-resume'), 'running plan must not offer resume');
  assert(!html.includes('ro-takeover-off'), 'takeover-off must render only while overridden');
}

// 4. running + manualOverride → takeover banner, toggle flips to release.
{
  const exp = withPlan({
    manualOverride: true,
    history: [...basePlan.history,
      { at: '2026-09-22T06:00:00Z', actor: 'owner-token', action: 'manual.takeover_on', step: 1, detail: 'incident INC-42' }],
  });
  const { html, root } = renderState(exp);
  assert(html.includes('ro-takeover-banner') && html.includes('Manual takeover active'), 'takeover banner missing');
  assert(html.includes('>manual override<'), 'manual-override chip missing');
  assert(html.includes('ro-takeover-off') && !html.includes('ro-takeover-on'), 'takeover toggle must flip to release');
  assert(html.includes('manual.takeover_on') && html.includes('INC-42'), 'takeover history entry missing');
  assert(bound(root, 'ro-takeover-off') && bound(root, 'ro-pause') && bound(root, 'ro-advance'),
    'override-state actions not bound');
}

// 5. paused (manual reason) → warn banner, resume + held-here step.
{
  const exp = withPlan({
    state: 'paused', pausedReason: 'operator request: investigate anomaly',
    history: [...basePlan.history,
      { at: '2026-09-22T07:00:00Z', actor: 'owner-token', action: 'manual.pause', step: 1, detail: 'operator request: investigate anomaly' }],
  });
  const { html, root } = renderState(exp);
  assert(html.includes('>plan paused<'), 'paused chip missing');
  assert(html.includes('banner warn') && html.includes('>PAUSED<'), 'manual pause must render the warn banner');
  assert(html.includes('operator request: investigate anomaly'), 'pause reason missing');
  assert(html.includes('>held here<'), 'current step must show held-here status while paused');
  assert(html.includes('ro-resume') && !html.includes('ro-pause') && !html.includes('ro-advance'),
    'paused: only resume (+takeover) actions');
  assert(bound(root, 'ro-resume') && bound(root, 'ro-takeover-on'), 'paused actions not bound');
}

// 6. paused (SRM block) → error-banner styling, same alert weight as the report.
{
  const exp = withPlan({
    state: 'paused', pausedReason: 'data health gate: SRM alarm χ² p=0.0004 below 0.001',
    history: [...basePlan.history,
      { at: '2026-09-22T08:00:00Z', actor: 'rollout-engine', action: 'plan.pause', step: 1, detail: 'data health gate: SRM alarm' }],
  });
  const { html } = renderState(exp);
  assert(html.includes('banner err') && html.includes('BLOCKED'), 'SRM block must render in the error-banner style');
  assert(html.includes('SRM'), 'SRM reason missing');
  assert(html.includes('never auto-resumes'), 'SRM guidance note missing');
  assert(html.includes('>plan.pause<'), 'engine pause history entry missing');
}

// 7. rolled_back (guardrail breach) → terminal alert, no mutation actions.
{
  const exp = withPlan({
    state: 'rolled_back', pausedReason: 'release plan rollback: guardrail breached: latency_p95 +12% over cap',
    history: [...basePlan.history,
      { at: '2026-09-22T09:00:00Z', actor: 'rollout-engine', action: 'plan.rollback', step: 1, detail: 'release plan rollback: guardrail breached' }],
  });
  const { html, root } = renderState(exp);
  assert(html.includes('banner err') && html.includes('ROLLED BACK'), 'rolled-back alert banner missing');
  assert(html.includes('guardrail breached'), 'rollback reason missing');
  assert(html.includes('>plan rolled_back<'), 'rolled_back chip missing');
  assert(html.includes('>stopped here<'), 'current step must be marked stopped-here');
  assert(!html.includes('ro-pause') && !html.includes('ro-resume') &&
    !html.includes('ro-advance') && !html.includes('ro-takeover'),
    'terminal plan must offer no mutation actions');
  assert(bound(root, 'ro-refresh'), 'refresh must stay available');
}

// 8. completed → ok banner, 100% progress, no-auto-victory note.
{
  const exp = withPlan({
    state: 'completed', currentStep: 2,
    history: [...basePlan.history,
      { at: '2026-09-24T00:00:00Z', actor: 'rollout-engine', action: 'plan.complete', step: 2, detail: 'ship verdict on final step' }],
  });
  const { html, root } = renderState(exp);
  assert(html.includes('banner ok') && html.includes('Plan completed'), 'completed banner missing');
  assert(html.includes('human publish decision'), 'no-auto-victory note missing');
  assert(html.includes('width:100%'), 'completed progress must be 100%');
  assert(html.includes('step 3/3'), 'completed progress text missing');
  assert(!html.includes('>pending<') && !html.includes('>current<'), 'completed: every step is done');
  assert(html.includes('>plan.complete<'), 'completion history entry missing');
  assert(!html.includes('ro-pause') && !html.includes('ro-advance'), 'completed plan must offer no mutation actions');
  assert(bound(root, 'ro-refresh'), 'refresh must stay available');
}

// --- operator-action contract: two-step confirm + exact wire requests -------

// 9. manual takeover ON: declined confirm → silent; empty reason → blocked;
//    confirmed + reason → POST /takeover {enabled:true, reason}.
{
  const { root } = renderState(withPlan());
  const tk = root._stubs.get('ro-takeover-on');
  sandbox.__confirm = false; fetchCalls.length = 0;
  tk.onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'takeover must not fire when the confirmation is declined');
  sandbox.__confirm = true; sandbox.__prompt = '';
  tk.onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'takeover must require a reason');
  sandbox.__prompt = 'incident INC-42';
  tk.onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 1, 'takeover must issue exactly one request');
  assert.strictEqual(fetchCalls[0].url, '/api/v1/admin/experiments/exp-1/release-plan/takeover');
  assert.strictEqual(fetchCalls[0].opts.method, 'POST');
  assert.deepStrictEqual(JSON.parse(fetchCalls[0].opts.body), { enabled: true, reason: 'incident INC-42' });
  assert(dialogs.at(-1).summary.includes('environment prod') &&
    dialogs.at(-1).summary.includes('ramp-25') && dialogs.at(-1).summary.includes('25%'),
  'takeover confirmation must identify environment and current rollout step');
}

// 10. pause / advance on a running plan.
{
  const { root } = renderState(withPlan());
  sandbox.__confirm = false; fetchCalls.length = 0;
  root._stubs.get('ro-pause').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'pause must not fire when declined');
  sandbox.__confirm = true; sandbox.__prompt = 'freeze for release train';
  root._stubs.get('ro-pause').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 1);
  assert.strictEqual(fetchCalls[0].url, '/api/v1/admin/experiments/exp-1/release-plan/pause');
  assert.deepStrictEqual(JSON.parse(fetchCalls[0].opts.body), { reason: 'freeze for release train' });

  fetchCalls.length = 0; sandbox.__prompt = 'approved in change meeting';
  root._stubs.get('ro-advance').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 1);
  assert.strictEqual(fetchCalls[0].url, '/api/v1/admin/experiments/exp-1/release-plan/advance');
  assert.deepStrictEqual(JSON.parse(fetchCalls[0].opts.body),
    { reason: 'approved in change meeting', expectedReportVersion: reportVersion });
}

// 11. resume on a paused plan: confirmation carries the reviewed report version.
{
  const { root } = renderState(withPlan({ state: 'paused', pausedReason: 'operator hold' }));
  sandbox.__confirm = false; fetchCalls.length = 0;
  root._stubs.get('ro-resume').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'resume must not fire when declined');
  sandbox.__confirm = true;
  root._stubs.get('ro-resume').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 1);
  assert.strictEqual(fetchCalls[0].url, '/api/v1/admin/experiments/exp-1/release-plan/resume');
  assert.deepStrictEqual(JSON.parse(fetchCalls[0].opts.body), { expectedReportVersion: reportVersion });
}

// 12. takeover OFF: confirm + optional reason → POST /takeover {enabled:false}.
{
  const { root } = renderState(withPlan({ manualOverride: true }));
  sandbox.__confirm = true; sandbox.__prompt = 'incident closed'; fetchCalls.length = 0;
  root._stubs.get('ro-takeover-off').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 1);
  assert.strictEqual(fetchCalls[0].url, '/api/v1/admin/experiments/exp-1/release-plan/takeover');
  assert.deepStrictEqual(JSON.parse(fetchCalls[0].opts.body),
    { enabled: false, reason: 'incident closed', expectedReportVersion: reportVersion });
}

// 13. A plan that advanced after rendering must be refreshed before an
// operator can confirm a mutation against the now-stale step.
{
  const { root } = renderState(withPlan());
  currentExperiment = withPlan({ currentStep: 2 });
  const before = dialogs.length;
  fetchCalls.length = 0;
  root._stubs.get('ro-advance').onclick();
  await tick();
  assert.strictEqual(dialogs.length, before, 'stale step must not open confirmation');
  assert.strictEqual(fetchCalls.length, 0, 'stale step must not POST a mutation');
}

// 14. A failed report blocks advancing and resuming, while pause and manual
// takeover remain available. A later successful read restores the controls.
{
  const { root } = renderState(withPlan());
  sandbox.__setRolloutReportStatus(root, 'pending');
  assert(root._stubs.get('ro-advance').disabled, 'advance must wait for the current report');
  sandbox.__setRolloutReportStatus(root, 'unavailable');
  assert(root._stubs.get('ro-advance').disabled, 'advance must be disabled after report failure');
  assert(!root._stubs.get('ro-pause').disabled && !root._stubs.get('ro-takeover-on').disabled,
    'pause and manual takeover must remain available');
  assert(root._stubs.get('ro-report-gate').innerHTML.includes('current report is unavailable'),
    'report failure must explain the blocked actions');
  reportUnavailable = true; fetchCalls.length = 0;
  root._stubs.get('ro-advance').onclick();
  await tick();
  assert.strictEqual(fetchCalls.length, 0, 'failed just-in-time report check must not POST advance');
  reportUnavailable = false;
  sandbox.__setRolloutReportStatus(root, 'ready');
  assert(!root._stubs.get('ro-advance').disabled, 'advance must recover after a successful report read');

  const paused = renderState(withPlan({ state: 'paused' })).root;
  sandbox.__setRolloutReportStatus(paused, 'unavailable');
  assert(paused._stubs.get('ro-resume').disabled, 'resume must be blocked on report failure');
  const takeover = renderState(withPlan({ manualOverride: true })).root;
  sandbox.__setRolloutReportStatus(takeover, 'unavailable');
  assert(takeover._stubs.get('ro-takeover-off').disabled,
    'return to automatic control must be blocked on report failure');
  assert(reportFetches > 0, 'advance must re-read the report before showing confirmation');
}

// 15. Evidence changing while the confirmation is open requires another
// explicit review. The first confirmation must not send a rollout mutation.
{
  const { root } = renderState(withPlan());
  const priorDialog = sandbox.actionDialog;
  let conflict;
  fetchCalls.length = 0;
  reportVersion = 'sha256:report-v1';
  sandbox.actionDialog = async options => {
    reportVersion = 'sha256:report-v2';
    try { await options.onConfirm({ reason: 'operator reviewed prior evidence' }); }
    catch (err) { conflict = err; }
    return null;
  };
  root._stubs.get('ro-advance').onclick();
  await tick();
  assert.strictEqual(conflict?.code, 'report_changed', 'changed evidence must require a fresh confirmation');
  assert.strictEqual(fetchCalls.length, 0, 'changed evidence must not POST a rollout mutation');
  sandbox.actionDialog = priorDialog;
  reportVersion = 'sha256:report-v1';
}

console.log('rollout UI DOM contract: all assertions passed (8 plan states + report safety gate + evidence confirmation + action flows)');
