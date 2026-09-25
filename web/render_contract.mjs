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
const code = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

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
assert(html.includes('anytime-valid p'), 'sequential anytime-valid p missing');
assert(html.includes('not the decision gate'), 'sequential separation note missing');
assert(html.includes('Bayesian'), 'bayesian heading missing');
assert(html.includes('P(B&gt;A)'), 'bayesian probability-of-superiority missing');
assert(html.includes('expected loss'), 'bayesian expected loss missing');
assert(html.includes('separate from frequentist'), 'bayesian separation note missing');

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

// 6. A report with no P5 sections must render the "not computed" placeholders
//    (graceful — the renderer must not crash on the v1 shape).
html = render({ ...base, primary: { type: 'welch', pValue: 0.2, diff: 0.01, relativeDiff: 0.01, relativeMeaningful: true, ciLow: -0.01, ciHigh: 0.03, significant: false } });
assert(html.includes('not computed (mean metrics only)'), 'sequential placeholder missing');

console.log('renderReport DOM contract: all assertions passed (6 report shapes)');
