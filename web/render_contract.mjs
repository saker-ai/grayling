// DOM/contract test for the admin console report renderer. It loads the REAL
// web/app.js in a Node vm (no browser needed), calls the actual renderReport()
// with mock reports covering every P5 field, and asserts the produced DOM HTML
// contains the expected sections. Run via `go test ./web/` (which shells to
// node) or directly `node web/render_contract.mjs`.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import assert from 'node:assert';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
// Load i18n.js + app.js in ONE vm context (same script-scope order as the
// browser's <script> tags) so app.js resolves t()/I18N from the catalogs.
// The sandbox has no golab_lang and no navigator.language → en (default).
const code = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

const el = () => ({
  style: {}, classList: { add() {}, remove() {} }, appendChild() {}, remove() {},
  setAttribute() {}, addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; },
});
const sandbox = {
  document: {
    addEventListener() {}, createElement: el, querySelector() { return null; },
    querySelectorAll() { return []; }, body: { appendChild() {} },
  },
  localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
  location: { hash: '', href: 'http://localhost/', search: '' },
  history: { pushState() {}, replaceState() {} },
  navigator: { clipboard: { writeText() { return Promise.resolve(); } } },
  console, setInterval, clearInterval, setTimeout, clearTimeout,
  fetch: () => Promise.reject(new Error('no fetch in contract test')),
};
sandbox.window = sandbox;
vm.createContext(sandbox);
// Expose renderReport without modifying the shipped file (append a bridge).
vm.runInNewContext(code + '\n;globalThis.__renderReport = renderReport;', sandbox);
assert.strictEqual(typeof sandbox.__renderReport, 'function', 'renderReport() not exposed from app.js');

function render(report) {
  const box = {};
  sandbox.__renderReport(box, report);
  assert.strictEqual(typeof box.innerHTML, 'string', 'renderReport must set box.innerHTML');
  return box.innerHTML;
}

const base = {
  healthy: true, epoch: 1, state: 'running', minRuntimeSatisfied: true, analysisMethod: 'welch',
  window: { version: 'utc-exposure-v1', start: '2026-10-01T00:00:00Z', end: '2026-10-02T00:00:00Z',
    watermark: '2026-10-02T00:00:00Z', complete: true, backend: 'postgresql-raw', recomputable: true },
  srm: { chiSquare: 0.1, pValue: 0.75, healthy: true, observed: [500, 500], expected: [500, 500] },
  perVariant: [{ variant: 'control', exposedUnits: 500, randomizedUnits: 500, exposures: 900, convertedUnits: 50, primaryValue: 0.1 }],
  decision: { verdict: 'ship', reasons: ['primary significantly better'] },
};

// 1. mean + sequential (always-valid) + bayesian.mean — the two P5 views must
//    render AND be labelled as separate from the decision gate (§10.3).
let html = render({
  ...base,
  primary: { type: 'welch', pValue: 0.001, diff: 0.2, relativeDiff: 0.1, relativeMeaningful: true, ciLow: 0.1, ciHigh: 0.3, significant: true },
  sequential: { pValue: 0.002, csLow: 0.05, csHigh: 0.35, tau: 0.2, logLambda: 6.2, significant: true },
  bayesian: { mean: { probBSuperior: 0.99, expectedLossB: 0.0001, credibleLow: 0.1, credibleHigh: 0.3 } },
});
assert(html.includes('Sequential'), 'sequential heading missing');
assert(html.includes('Analysis window (UTC)') && html.includes('Data watermark') &&
  html.includes('utc-exposure-v1') && html.includes('2026-10-02T00:00:00Z'), 'window metadata missing');
assert(html.includes('anytime-valid p'), 'sequential anytime-valid p missing');
assert(html.includes('not the decision gate'), 'sequential separation note missing');
assert(html.includes('Bayesian'), 'bayesian heading missing');
assert(html.includes('P(B&gt;A)'), 'bayesian probability-of-superiority missing');
assert(html.includes('expected loss'), 'bayesian expected loss missing');
assert(html.includes('separate from frequentist'), 'bayesian separation note missing');
const incomplete = render({ ...base, healthy: false, healthIssues: ['watermark pending'],
  window: { ...base.window, complete: false, issues: ['watermark pending'] } });
assert(incomplete.includes('Incomplete') && incomplete.includes('watermark pending'), 'incomplete window reason missing');

const delivery = render({ ...base, healthy: false, healthIssues: ['delivery skew'],
  diagnostics: { assignmentSrm: { healthy: true, pValue: 0.8 },
    exposureSrm: { healthy: false, pValue: 0.00001 }, crossVariantUnits: 2,
    assignmentMismatch: 1, perVariant: [
      { variant: 'control', assignments: 100, exposures: 100, unexposed: 0, conversions: 12 },
      { variant: 'treatment', assignments: 100, exposures: 20, unexposed: 80, conversions: 3 },
    ] } });
assert(delivery.includes('Assignment SRM') && delivery.includes('Exposure SRM') &&
  delivery.includes('Not exposed') && delivery.includes('delivery skew') && delivery.includes('80'),
  'delivery diagnostics or distinct SRM checks missing');

const journey = render({ ...base, healthy: false, healthIssues: ['immature cohorts'],
  journeys: [{ metric: 'd1', type: 'retention', watermark: '2026-10-02T00:00:00Z',
    retention: { returnKey: 'visit', day: 1, timezone: 'UTC' },
    arms: [{ variant: 'control', denominator: 8, numerator: 3, immature: 2,
      excluded: { immature_cohort: 2, no_return_in_window: 5 } }] }] });
assert(journey.includes('Funnel / retention populations') && journey.includes('Denominator') &&
  journey.includes('Numerator') && journey.includes('Immature') && journey.includes('immature_cohort') &&
  journey.includes('2026-10-02T00:00:00Z'), 'journey counts or watermark missing');

// 2. ratio primary → delta-method detail line.
html = render({
  ...base,
  primary: { type: 'ratio-delta', pValue: 0.01, diff: 0.6, relativeDiff: 0.3, relativeMeaningful: true, ciLow: 0.2, ciHigh: 1.0, significant: true, ratio: { ratioA: 2.0, ratioB: 2.6, se: 0.2 } },
});
assert(html.includes('ratio: A='), 'ratio detail line missing');

// 3. quantile primary → distribution-free CI note.
html = render({
  ...base,
  primary: { type: 'quantile', pValue: 0.01, diff: -20, relativeDiff: -0.2, relativeMeaningful: true, ciLow: -25, ciHigh: -15, significant: true, quantile: { level: 0.5, quantileA: 100, quantileB: 80, ciALow: 95, ciAHigh: 105 } },
});
assert(html.includes('quantile p'), 'quantile detail line missing');
assert(html.includes('distribution-free'), 'quantile distribution-free CI note missing');

// 4. CUPED primary → variance-reduction line.
html = render({
  ...base,
  primary: { type: 'cuped', pValue: 0.01, diff: 0.35, relativeDiff: 0.03, relativeMeaningful: true, ciLow: 0.1, ciHigh: 0.6, significant: true, varianceReduction: 0.47, cuped: { theta: 0.9, correlation: 0.68, rawDiff: 0.36 } },
});
assert(html.includes('CUPED: variance reduction'), 'cuped detail line missing');
assert(html.includes('47.0%'), 'cuped variance-reduction percent missing');

// 5. proportion primary + bayesian.proportion → Beta posterior line.
html = render({
  ...base,
  primary: { type: 'two-proportion-z', pValue: 0.01, diff: 0.06, relativeDiff: 0.6, relativeMeaningful: true, ciLow: 0.02, ciHigh: 0.1, significant: true },
  bayesian: { proportion: { probBSuperior: 0.98, expectedLossB: 0.0002, postAA: 31, postAB: 71, postBA: 46, postBB: 56 } },
});
assert(html.includes('posterior A=Beta('), 'bayesian proportion posterior line missing');

// A registered fixed-sample family has its own raw/adjusted p-value table,
// registration fingerprint and scope note; exploratory/guardrail results are
// explicitly excluded from the Holm claims.
html = render({
  ...base,
  primary: { type: 'two-proportion-z', pValue: 0.01, diff: 0.1, significant: true },
  registrationVersion: 'sha256:fixture',
  holm: { familyId: 'launch', registrationVersion: 'sha256:fixture', alpha: 0.05,
    comparisons: [{ id: 'conversion-B', metric: { key: 'conversion' }, control: 'control', treatment: 'treatment',
      rawP: 0.01, adjustedP: 0.02, effectiveAlpha: 0.025, rank: 1, rejected: true }] },
});
assert(html.includes('Holm fixed-sample family') && html.includes('sha256:fixture') &&
  html.includes('Raw p') && html.includes('Adjusted p') && html.includes('Effective α') &&
  html.includes('conversion-B') && html.includes('unlisted comparisons are exploratory'),
  'Holm family table or scope explanation missing');

// 6. A report with no P5 sections must render the "not computed" placeholders
//    (graceful — the renderer must not crash on the v1 shape).
html = render({ ...base, primary: { type: 'welch', pValue: 0.2, diff: 0.01, relativeDiff: 0.01, relativeMeaningful: true, ciLow: -0.01, ciHigh: 0.03, significant: false } });
assert(html.includes('not computed (mean metrics only)'), 'sequential placeholder missing');

// 7. Adaptive allocation must never render the fixed-sample winner card.
html = render({ ...base, analysisMethod: 'bandit-beta-bernoulli', decision: { verdict: 'ship' },
  primary: { type: 'two-proportion-z', pValue: 0.001, significant: true },
  perVariant: [{ variant: 'control' }, { variant: 'treatment' }],
  bandit: { policyVersion: 'p1', stateVersion: 7, feedbackWatermark: '2026-10-01T00:00:00Z',
    trials: [10, 10], successes: [2, 5], posteriorMean: [0.25, 0.5], pending: 4,
    missingFeedback: 13, fallbackAssignments: 2, srmAlarm: false, frozen: false } });
assert(html.includes('Adaptive Bandit report') && html.includes('p1'), 'bandit policy provenance missing');
assert(!html.includes('two-proportion-z') && !html.includes('SHIP') && !html.includes('p = 0.001'),
  'fixed-sample inference leaked into bandit report');

const banditDelivery = render({ ...base, exposureMode: 'onRender',
  analysisMethod: 'bandit-beta-bernoulli',
  perVariant: [{ variant: 'control' }, { variant: 'treatment' }],
  diagnostics: { window: { version: 'bandit-feedback-v1' },
    perVariant: [{ variant: 'control', assignments: 2, exposures: 1, unexposed: 1, conversions: 0 }],
    crossVariantUnits: 0, assignmentMismatch: 0 },
  bandit: { policyVersion: 'p1', stateVersion: 8, feedbackWatermark: '2026-10-01T00:00:00Z',
    trials: [1, 0], successes: [0, 0], posteriorMean: [0.5, 0.5], pending: 0,
    missingFeedback: 1, fallbackAssignments: 0, srmAlarm: false, frozen: true,
    freezeReason: 'assigned units have no recorded exposure' } });
assert(banditDelivery.includes('Not exposed') && banditDelivery.includes('propensity-aware') &&
  !banditDelivery.includes('Too few units for an SRM check'),
  'bandit delivery gap must be visible without fixed-split SRM wording');

console.log('renderReport DOM contract: all assertions passed (9 report shapes)');
