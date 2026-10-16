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
// The sandbox has no grayling_lang and no navigator.language → en (default).
const code = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'presets.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'flag_editor.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'segment_editor.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'approvals.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(__dirname, 'exp_workbench.js'), 'utf8') + '\n' +
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
  AbortController,
  fetch: () => Promise.reject(new Error('no fetch in contract test')),
};
sandbox.window = sandbox;
vm.createContext(sandbox);
// Expose renderReport without modifying the shipped file (append a bridge).
vm.runInNewContext(code + '\n;globalThis.__loadEditorSegmentSuggestions = loadEditorSegmentSuggestions; globalThis.__resourceIdentity = resourceIdentity; globalThis.__flagListPage = flagListPage; globalThis.__renderFlagWorkflow = renderFlagWorkflow; globalThis.__renderPublishedSummary = renderPublishedSummary; globalThis.__renderReport = renderReport; globalThis.__renderDiagnostics = renderDiagnostics; globalThis.__renderExpDecisionHero = renderExpDecisionHero; globalThis.__classifyExpStatus = classifyExpStatus; globalThis.__renderExpHero = renderExpHero; globalThis.__S = S; globalThis.__renderBindReadiness = renderBindReadiness; globalThis.__renderBindConflict = renderBindConflict; globalThis.__renderExpStatusBand = renderExpStatusBand; globalThis.__renderExpZoneNav = renderExpZoneNav; globalThis.__renderHealthZone = renderHealthZone; globalThis.__renderHealthReportCard = renderHealthReportCard; globalThis.__fmtTime = fmtTime; globalThis.__sortFlagsForList = sortFlagsForList; globalThis.__flagListCategory = flagListCategory; globalThis.__renderFlagDiff = renderFlagDiff; globalThis.__renderFlagSaveConflict = renderFlagSaveConflict; globalThis.__renderFlagExperimentImpact = renderFlagExperimentImpact; globalThis.__renderFlagStructured = renderFlagStructured; globalThis.__lintFlag = lintFlag; globalThis.__renderSegmentStructured = renderSegmentStructured; globalThis.__lintSegment = lintSegment; globalThis.__renderSegmentDependencies = renderSegmentDependencies; globalThis.__renderSegmentHistory = renderSegmentHistory; globalThis.__renderSegmentMatchForm = renderSegmentMatchForm; globalThis.__renderSegmentMatchResult = renderSegmentMatchResult; globalThis.__parseSegmentPreviewContext = parseSegmentPreviewContext; globalThis.__renderApprovalDetail = renderApprovalDetail; globalThis.__renderApprovalComparison = renderApprovalComparison; globalThis.__renderAIResourceDetail = renderAIResourceDetail; globalThis.__renderOverviewRiskBand = renderOverviewRiskBand; globalThis.__renderOverviewStats = renderOverviewStats; globalThis.__renderOverviewApprovals = renderOverviewApprovals; globalThis.__renderOverviewReleases = renderOverviewReleases; globalThis.__renderOverviewModuleFailure = renderOverviewModuleFailure;', sandbox);
assert.strictEqual(typeof sandbox.__renderReport, 'function', 'renderReport() not exposed from app.js');

const delivered = sandbox.__renderDiagnostics({ exposureMode: 'onRender', perVariant: [
  { variant: 'control', assignments: 100, exposures: 80, conversions: 10, unexposed: 20 },
  { variant: 'treatment', assignments: 100, exposures: 70, conversions: 7, unexposed: 30 },
] });
assert(delivered.includes('Assignment, exposure, and conversion') &&
  delivered.includes('<strong>200</strong>') && delivered.includes('<strong>150</strong>') &&
  delivered.includes('<strong>17</strong>') && delivered.includes('Not exposed: 50') &&
  delivered.includes('unique units within each variant'),
  'onRender diagnostics must show true assignment, exposure and conversion counts with scope');
const decisionExposure = sandbox.__renderDiagnostics({ exposureMode: 'onDecision', perVariant: [
  { variant: 'control', assignments: 0, exposures: 80, conversions: 10, unexposed: 0 },
] });
assert(decisionExposure.includes('<strong>—</strong>') &&
  decisionExposure.includes('no separate assignment count') &&
  decisionExposure.includes('<td>—</td><td>80</td><td>—</td>') &&
  !decisionExposure.includes('Not exposed: 0'),
  'onDecision diagnostics must not turn missing historical assignments into zero');
const decisionWithAssignments = sandbox.__renderDiagnostics({ exposureMode: 'onDecision', perVariant: [
  { variant: 'control', assignments: 50, exposures: 45, conversions: 5, unexposed: 5 },
] });
assert(decisionWithAssignments.includes('<strong>50</strong>') &&
  decisionWithAssignments.includes('Not exposed: 5'),
  'onDecision diagnostics with real assignment facts must keep those counts visible');

const structuredFlag = sandbox.__renderFlagStructured({ type: 'bool',
  variations: { control: false, treatment: true }, defaultRule: { variation: 'control' },
  targeting: [{ name: '<script>alert(1)</script>', percentage: { control: 50, treatment: 50 },
    constraints: [{ field: 'country', op: 'eq', value: 'CN' }] }] });
assert(structuredFlag.includes('Percentage split') && structuredFlag.includes('data-split="control"') &&
  structuredFlag.includes('data-condition-field') && !structuredFlag.includes('<script>') &&
  structuredFlag.includes('&lt;script&gt;'), 'structured Flag editor must render split and escape rule content');
assert(sandbox.__lintFlag({ type: 'bool', variations: { control: 'false' },
  defaultRule: { variation: 'control' }, targeting: {} }).length >= 2,
  'JSON advanced mode must report invalid value types and rule shape without throwing');
const structuredSegment = sandbox.__renderSegmentStructured({ key: 'audience',
  constraints: [{ field: '<img src=x>', op: 'eq', value: 'CN' }], segments: ['staff'] });
assert(structuredSegment.includes('data-seg-op') && structuredSegment.includes('staff') &&
  !structuredSegment.includes('<img') && structuredSegment.includes('&lt;img'),
  'structured Segment editor must render conditions and escape user content');
assert(sandbox.__lintSegment({ key: 'audience', constraints: [{ field: 'country', op: 'regex', value: '[' }],
  segments: [] }).length > 0, 'invalid Segment regex must be reported');
const dependencies = sandbox.__renderSegmentDependencies({ segmentRevision: 2,
  flags: [{ key: '<img src=x>', revision: 7, via: ['nested', 'staff'] }] });
assert(dependencies.includes('nested → staff') && !dependencies.includes('<img') &&
  dependencies.includes('&lt;img'), 'dependency index must show the reference path and escape keys');
const segmentHistory = sandbox.__renderSegmentHistory({ currentRevision: 2, revisions: [
  { revision: 2, content: { schemaVersion: 1, constraints: [{ field: 'country', op: 'eq', value: '<img src=x>' }], segments: [] },
    hash: 'new', publishedAt: '2026-10-13T10:00:00Z', publishedBy: 'owner' },
  { revision: 1, content: { schemaVersion: 1, constraints: [{ field: 'country', op: 'eq', value: 'CN' }], segments: [] },
    hash: 'old', publishedAt: '2026-10-12T10:00:00Z', publishedBy: 'owner' },
] });
assert(segmentHistory.includes('r1') && segmentHistory.includes('r2') &&
  segmentHistory.includes('Conditions') && segmentHistory.includes('&lt;img') &&
  !segmentHistory.includes('<img'), 'Segment history must compare retained versions and escape content');
assert(sandbox.__renderSegmentMatchForm().includes('seg-match-context') &&
  sandbox.__renderSegmentMatchResult({ draftMatched: true, publishedMatched: false, publishedRevision: 2 })
    .includes('Published Segment r2'), 'sample match preview must show both versions');
assert.throws(() => sandbox.__parseSegmentPreviewContext('{"attributes":[]}'),
  'sample context must reject a non-object attributes value');
const approvalDetail = sandbox.__renderApprovalDetail({ id: 'a1', state: 'submitted',
  action: 'flag.rollback', object: '<img src=x>', submittedBy: 'alice',
  reason: '<script>alert(1)</script>', payload: '{"revision":3}' });
assert(!approvalDetail.includes('<script>') && !approvalDetail.includes('<img') &&
  approvalDetail.includes('&lt;script&gt;') && approvalDetail.includes('revision'),
  'approval detail must escape request data and show proposed operation input');
const approvalComparison = sandbox.__renderApprovalComparison({ action: 'flag.rollback' }, {
  available: true, stale: false, beforeRevision: 2, afterRevision: 1,
  before: { variations: { on: '<script>alert(1)</script>' } },
  after: { variations: { on: false } },
});
assert(approvalComparison.includes('Published Flag r2') && approvalComparison.includes('Variations') &&
  approvalComparison.includes('&lt;script&gt;') &&
  !approvalComparison.includes('<script>'), 'approval comparison must show escaped before and after values');
assert(sandbox.__renderApprovalComparison({ action: 'flag.rollback', state: 'approved' },
  { available: true, stale: true, beforeRevision: 2, afterRevision: 1, before: {}, after: {} })
  .includes('source changed'), 'stale approval must display its changed baseline');
assert(!sandbox.__renderApprovalComparison({ action: 'flag.rollback', state: 'executed' },
  { available: true, stale: true, beforeRevision: 2, afterRevision: 1, before: {}, after: {} })
  .includes('source changed'), 'completed rollback must not be presented as an unexecutable request');
const aiDetail = sandbox.__renderAIResourceDetail({ resource: { type: 'prompt', logicalId: '<img src=x>',
  physicalId: 'physical', owner: 'owner', createdAt: '2026-10-13T10:00:00Z' },
pins: [{ version: 'v2', digest: '<script>', pinnedBy: 'owner', pinnedAt: '2026-10-13T10:00:00Z' }],
revoked: true, revocationReason: 'incident', sourceMode: 'external-config' });
assert(aiDetail.includes('External Nacos config documents') && aiDetail.includes('Pinned version history') &&
  aiDetail.includes('Resolution is blocked') && aiDetail.includes('&lt;img') &&
  aiDetail.includes('&lt;script&gt;') && !aiDetail.includes('<img') && !aiDetail.includes('<script>'),
  'AI detail must show metadata, revocation and source without injecting content');

const flagDiff = sandbox.__renderFlagDiff(
  { revision: 4, content: { type: 'bool', variations: { safe: false, launch: true }, disable: false } },
  { content: { type: 'bool', variations: { safe: false, launch: '<img src=x onerror=alert(1)>' }, disable: true } },
);
assert(flagDiff.includes('Compared with published revision r4') && flagDiff.includes('Kill switch') &&
  flagDiff.includes('Variations') && !flagDiff.includes('<img') && flagDiff.includes('&lt;img'),
  'draft diff must show changed sections and escape variation values');
assert(!flagDiff.includes('Value type'), 'unchanged fields should not clutter the draft diff');
const flagImpact = sandbox.__renderFlagExperimentImpact({ total: 2, references: [
  { id: '<img src=x>', state: 'running', epoch: 3, flagRevision: 4 },
] });
assert(flagImpact.includes('Showing 1 of 2') && flagImpact.includes('running') &&
  flagImpact.includes('data-exp-from-flag=') && flagImpact.includes('&lt;img src=x&gt;') &&
  !flagImpact.includes('<img'), 'Flag relationship summary must show scope, state, truncation and escape IDs');
assert(sandbox.__renderFlagDiff(null, null).includes('No saved draft'), 'missing draft state');
assert(sandbox.__renderFlagDiff(null, { deleted: true }).includes('marks the Flag for deletion'), 'deletion state');
const conflict = sandbox.__renderFlagSaveConflict({ currentRevision: 5,
  current: { revision: 5, content: { type: 'bool', variations: { safe: false } } },
  draft: { content: { type: 'bool', variations: { safe: true } } } },
{ type: 'bool', variations: { safe: '<svg/onload=alert(1)>' } });
assert(conflict.includes('Published revision is now r5') && conflict.includes('Your unsaved input') &&
  conflict.includes('Your input vs latest server draft') &&
  conflict.includes('Saving after acknowledgement replaces that server draft') &&
  conflict.includes('Latest saved draft on the server') && !conflict.includes('<svg'),
  '409 conflict review must compare remote and local drafts, disclose replacement and escape input');

const flagRows = [
  { key: 'published', current: { publishedAt: '2026-10-13T10:00:00Z' } },
  { key: 'review', draft: { state: 'submitted', updatedAt: '2026-10-10T10:00:00Z' } },
  { key: 'running-change', draft: { state: 'editing', risk: 'high', updatedAt: '2026-10-09T10:00:00Z' },
    current: { content: { experiment: { state: 'running' } } } },
  { key: 'newer-review', draft: { state: 'submitted', updatedAt: '2026-10-12T10:00:00Z' } },
];
assert.deepEqual(Array.from(sandbox.__sortFlagsForList(flagRows), row => row.key),
  ['running-change', 'newer-review', 'review', 'published'], 'flag list priority and recency order');
assert.equal(flagRows[0].key, 'published', 'sorting must not mutate API response');
assert.equal(sandbox.__flagListCategory(flagRows[1]), 'submitted', 'filter category comes from draft state');

const blockedBind = sandbox.__renderBindReadiness({ ready: false, registeredRevision: 2, currentRevision: 3,
  checks: [{ code: 'registered_revision', ready: false }, { code: 'draft_clear', ready: false }] });
assert(blockedBind.includes('Binding blocked') && blockedBind.includes('Current: 3') &&
  blockedBind.includes('Flag revision matches registration') && blockedBind.includes('Flag draft has no pending changes'),
  'binding precheck must explain blocked revision and draft');
const readyBind = sandbox.__renderBindReadiness({ ready: true, registeredRevision: 2, currentRevision: 2,
  checks: [{ code: 'state', ready: true }] });
assert(readyBind.includes('Ready to bind') && readyBind.includes('Pass'), 'ready binding state missing');
const escapedBind = sandbox.__renderBindReadiness({ ready: false, registeredRevision: 2, currentRevision: 0,
  checks: [{ code: '<script>alert(1)</script>', ready: false }] });
assert(!escapedBind.includes('<script>') && escapedBind.includes('&lt;script&gt;'), 'binding check code was not escaped');
const bindConflict = sandbox.__renderBindConflict(
  { currentRevision: 2, checks: [{ code: 'registered_revision', ready: true }, { code: 'draft_clear', ready: true }] },
  { currentRevision: 3, checks: [{ code: 'registered_revision', ready: false }, { code: 'draft_clear', ready: false }] });
assert(bindConflict.includes('source changed') && bindConflict.includes('Current: 3') &&
  bindConflict.includes('Flag revision matches registration') && bindConflict.includes('Flag draft has no pending changes'),
  '409 binding recovery must show the latest changed prerequisites');
assert(sandbox.__renderBindConflict({ currentRevision: 2 }, null).includes('could not be loaded'),
  '409 binding recovery must explain failed reload');
assert(sandbox.__renderExpDecisionHero(null, 'reviewed').includes('No experiment results are being collected yet.'),
  'pre-launch experiment must not be presented as an unhealthy result');
const unavailableHero = sandbox.__renderExpDecisionHero({ healthy: true, generatedAt: 'old-time' }, 'running', new Error('offline'));
assert(unavailableHero.includes('Current analysis is unavailable') && !unavailableHero.includes('Report healthy'),
  'failed refresh must not present an older report as a current healthy decision');

// ---------------------------------------------------------------------------
// Six-state hero classification (§4.4 状态×文案). The classifier reads only
// real server fields; every kind renders its own first-screen copy, missing
// data degrades to explicit unknown copy, and no number, time or victory
// claim is ever invented.
// ---------------------------------------------------------------------------
sandbox.__S.capabilities = { permissions: { 'report.read': true, 'flag.publish': true } };
const heroExp = { id: 'exp-1', state: 'running', flagKey: 'my-flag', environmentId: 'prod', epoch: 3 };
const classify = (e, report, diag, transition, meta = {}) =>
  sandbox.__classifyExpStatus(e, report, diag, transition, { canRead: true, ...meta });

// 1. not_started — state-derived, needs no report.
for (const st of ['draft', 'reviewed', 'scheduled']) {
  assert.strictEqual(classify({ ...heroExp, state: st }, null, null, null).kind, 'not_started',
    st + ' must classify as not_started');
}
const notStartedHero = sandbox.__renderExpHero(classify({ ...heroExp, state: 'draft' }, null, null, null), null);
assert(notStartedHero.includes('No experiment results are being collected yet.') &&
  notStartedHero.includes('Complete the readiness checks, then submit for review and launch.') &&
  notStartedHero.includes('View readiness checks') && notStartedHero.includes('data-hero-goto="settings"'),
  'not_started hero shows the collection-not-started copy with the readiness action');

// 2. no_samples — the server issue string verbatim plus two real diagnostics
//    sub-branches; missing assignment facts never guess a sub-branch.
const noSamplesReport = { healthy: false, healthIssues: ['no randomized exposures recorded yet'],
  minRuntimeSatisfied: false, decision: { verdict: 'hold',
    reasons: ['pre-registered minimum runtime not yet satisfied'] } };
const cAssigned = classify(heroExp, noSamplesReport, { exposureMode: 'onRender', perVariant: [
  { variant: 'control', assignments: 120, exposures: 0, unexposed: 120, conversions: 0 },
  { variant: 'treatment', assignments: 0, exposures: 0, unexposed: 0, conversions: 0 }] }, null);
assert.strictEqual(cAssigned.kind, 'no_samples', 'no-exposure health issue classifies as no_samples');
assert.strictEqual(cAssigned.params.subBranch, 'assigned');
const assignedHero = sandbox.__renderExpHero(cAssigned, noSamplesReport);
assert(assignedHero.includes('Waiting for the first real exposures.') &&
  assignedHero.includes('120 units assigned but none exposed yet') &&
  assignedHero.includes('data-hero-goto="health"'),
  'assigned-not-exposed sub-branch quotes the real assignment total');
const cNone = classify(heroExp, noSamplesReport, { exposureMode: 'onRender', perVariant: [
  { variant: 'control', assignments: 0, exposures: 0, unexposed: 0, conversions: 0 }] }, null);
assert.strictEqual(cNone.params.subBranch, 'none');
assert(sandbox.__renderExpHero(cNone, noSamplesReport).includes('No assignments recorded yet'),
  'zero-assignment sub-branch copy missing');
const cNoFacts = classify(heroExp, noSamplesReport, { exposureMode: 'onDecision', perVariant: [
  { variant: 'control', assignments: 0, exposures: 0, unexposed: 0, conversions: 0 }] }, null);
assert.strictEqual(cNoFacts.params.subBranch, 'unknown',
  'onDecision histories without assignment facts must not fake a zero');
const noFactsHero = sandbox.__renderExpHero(cNoFacts, noSamplesReport);
assert(noFactsHero.includes('Waiting for the first real exposures.') &&
  !noFactsHero.includes('No assignments recorded yet') && !noFactsHero.includes('units assigned'),
  'missing assignment facts degrade to the generic waiting copy, never a guessed sub-branch');
const cDiagFailed = classify(heroExp, noSamplesReport, null, null, { diagFailed: true });
assert.strictEqual(cDiagFailed.params.subBranch, 'unknown');
const diagFailedHero = sandbox.__renderExpHero(cDiagFailed, noSamplesReport);
assert(diagFailedHero.includes('Waiting for the first real exposures.') &&
  diagFailedHero.includes('data-hero-goto="retry-diagnostics"'),
  'a failed diagnostics fetch keeps the generic copy and offers a retry');

// 3. immature — ETA from real fields only; missing startedAt degrades to the
//    explicit unknown copy with zero invented numbers.
const immatureReport = { healthy: true, minRuntimeSatisfied: false,
  decision: { verdict: 'hold', reasons: ['pre-registered minimum runtime not yet satisfied'] } };
const startedExp = { ...heroExp, startedAt: '2026-10-14T00:00:00Z', minRuntime: 86_400_000_000_000 };
const cEta = classify(startedExp, immatureReport, null, undefined);
assert.strictEqual(cEta.kind, 'immature');
assert.strictEqual(cEta.params.eta.toISOString(), '2026-10-15T00:00:00.000Z',
  'ETA = startedAt + minRuntime from the real registered fields');
const etaHero = sandbox.__renderExpHero(cEta, immatureReport);
assert(etaHero.includes('Results are not mature yet; current numbers are for observation only.') &&
  etaHero.includes('Earliest decidable time: ' + sandbox.__fmtTime('2026-10-15T00:00:00.000Z')) &&
  etaHero.includes('pre-registered minimum runtime not yet satisfied') &&
  etaHero.includes('data-hero-goto="health"'),
  'immature hero shows the observation-only copy, the real ETA and the server reason verbatim');
const cGate = classify({ ...startedExp, releasePlan: { state: 'running', currentStep: 0,
  stepStartedAt: '2026-10-14T00:00:00Z',
  steps: [{ name: 'canary', ratio: 5, minRuntime: 3_600_000_000_000, observationWindow: 0,
    autoAdvance: true }] } }, immatureReport, null, undefined);
assert.strictEqual(cGate.params.eta.toISOString(), '2026-10-14T01:00:00.000Z',
  'a running plan takes the ETA from planNextGate, not startedAt + minRuntime');
const cNoStart = classify({ ...heroExp, minRuntime: 86_400_000_000_000 }, immatureReport, null, undefined);
assert.strictEqual(cNoStart.params.eta, null, 'missing startedAt must not produce an ETA');
const noStartHero = sandbox.__renderExpHero(cNoStart, immatureReport);
assert(noStartHero.includes('Start time is not recorded; the earliest decidable time is unavailable.') &&
  !noStartHero.includes('Earliest decidable time:') && !/[0-9]/.test(noStartHero),
  'unknown ETA degrades to explicit copy with no invented time numbers');
assert.strictEqual(classify(heroExp, { healthy: true, minRuntimeSatisfied: true,
  decision: { verdict: 'hold',
    reasons: ['insufficient sample: NA=12 NB=9 below pre-registered minimum 100 per arm'] } },
  null, undefined).kind, 'immature', 'an insufficient-sample hold is immature, not blocked');
assert.strictEqual(classify(heroExp, { healthy: true, minRuntimeSatisfied: true,
  decision: { verdict: 'continue', reasons: ['primary metric not significant'] } },
  null, undefined).kind, 'immature', 'a healthy continue verdict keeps observing');

// 4. blocked — unhealthy pipeline/SRM/guardrails or a rollback verdict; the
//    server reasons appear verbatim with the bound scope.
const srmReason = 'SRM alarm: observed [30 10] vs expected [20 20] (p=0.0016) — delivery pipeline suspect';
const blockedReport = { healthy: false, healthIssues: [srmReason], minRuntimeSatisfied: true,
  decision: { verdict: 'hold', reasons: ['data pipeline unhealthy'],
    guardrailBreaches: [{ metric: 'latency_p95', detail: '+12% over cap' }] } };
const cBlocked = classify(heroExp, blockedReport, null, undefined);
assert.strictEqual(cBlocked.kind, 'blocked');
const blockedHero = sandbox.__renderExpHero(cBlocked, blockedReport);
assert(blockedHero.includes('Automatic advancement is blocked.') && blockedHero.includes(srmReason) &&
  blockedHero.includes('data pipeline unhealthy') && blockedHero.includes('latency_p95') &&
  blockedHero.includes('+12% over cap') &&
  blockedHero.includes('Affected: flag my-flag · environment prod · epoch 3') &&
  blockedHero.includes('data-hero-goto="health"') && blockedHero.includes('data-hero-goto="rollout"') &&
  blockedHero.includes('data-hero-goto="audit"'),
  'blocked hero quotes server reasons verbatim, names the server-bound scope and offers diagnostics / pause-rollback / recent changes');
assert.strictEqual(classify(heroExp, { healthy: true, minRuntimeSatisfied: true,
  decision: { verdict: 'rollback',
    reasons: ['primary metric significantly worse in treatment: diff=-0.2'] } },
  null, undefined).kind, 'blocked', 'a rollback verdict blocks even when the pipeline is healthy');

// 5. can_advance — healthy ship with the minimum runtime satisfied; the copy
//    shows real evidence and never claims absolute victory.
const shipReport = { healthy: true, minRuntimeSatisfied: true,
  primary: { type: 'welch', pValue: 0.001, diff: 0.2, relativeDiff: 0.1, ciLow: 0.1, ciHigh: 0.3,
    significant: true },
  guardrails: [{ metric: 'latency_p95', detail: 'ok', breached: false }],
  decision: { verdict: 'ship', reasons: ['primary metric significantly better in treatment'] } };
const cShip = classify(heroExp, shipReport, null, undefined);
assert.strictEqual(cShip.kind, 'can_advance');
assert(cShip.actions.some(a => a.goto === 'evidence') && cShip.actions.some(a => a.goto === 'rollout'),
  'can_advance offers the evidence action and, with flag.publish, the rollout action');
const shipHero = sandbox.__renderExpHero(cShip, shipReport);
assert(shipHero.includes('Data meets the current rollout gate.') && shipHero.includes('p = 0.001') &&
  shipHero.includes('CI [0.1, 0.3]') && shipHero.includes('latency_p95') &&
  shipHero.includes('This is not a claim of absolute victory'),
  'can_advance shows the real primary/guardrail evidence with the no-absolute-victory note');
assert(!/\bwinner\b|has won|declared victor|胜出|获胜方|宣布获胜/i.test(shipHero),
  'can_advance copy must not contain a victory claim');
sandbox.__S.capabilities = { permissions: { 'report.read': true } };
assert(!classify(heroExp, shipReport, null, undefined).actions.some(a => a.goto === 'rollout'),
  'the advance action is gated behind flag.publish');
sandbox.__S.capabilities = { permissions: { 'report.read': true, 'flag.publish': true } };

// 6. ended — operator/time/reason from the real transition audit; a missing
//    or failed audit degrades to the explicit unknown copy with a link.
const pausedExp = { ...heroExp, state: 'paused' };
for (const st of ['paused', 'stopped', 'archived']) {
  assert.strictEqual(classify({ ...heroExp, state: st }, null, null, null).kind, 'ended',
    st + ' is an ended state');
}
const transition = { actor: 'alice (tok-1)', time: '2026-10-14T09:00:00Z', to: 'paused',
  reason: 'guardrail investigation' };
const endedHero = sandbox.__renderExpHero(classify(pausedExp, null, null, transition), null);
assert(endedHero.includes('paused: alice (tok-1) at ' + sandbox.__fmtTime('2026-10-14T09:00:00Z') +
    ' — reason: guardrail investigation') &&
  endedHero.includes('data-hero-goto="audit"') && endedHero.includes('data-hero-goto="settings"'),
  'ended hero names the real operator, time and reason and links the audit + transition controls');
const endedUnknownHero = sandbox.__renderExpHero(classify(pausedExp, null, null, null), null);
assert(endedUnknownHero.includes('Transition details are unavailable; check the audit log.') &&
  endedUnknownHero.includes('data-hero-goto="audit"') &&
  !endedUnknownHero.includes('alice'),
  'a missing transition audit degrades to the unknown copy with an audit link, never a guessed operator');
const rolledHero = sandbox.__renderExpHero(classify({ ...heroExp, state: 'stopped',
  releasePlan: { state: 'rolled_back', currentStep: 1,
    steps: [{ name: 'canary', ratio: 5 }, { name: 'ramp', ratio: 50 }] } }, null, null, transition), null);
assert(rolledHero.includes('Rollback position: ramp · 50%'),
  'a rolled-back plan shows the real step it stopped at');

// Priority: report error outranks every state kind; missing report.read shows
// the restricted copy; a granted permission with no report yet keeps the
// existing loading copy.
const errCls = sandbox.__classifyExpStatus(pausedExp, { healthy: true, generatedAt: 'old-time' },
  null, transition, { error: new Error('offline'), canRead: true });
assert.strictEqual(errCls.kind, 'error', 'a report fetch failure outranks the six state kinds');
const errHero = sandbox.__renderExpHero(errCls, { healthy: true, generatedAt: 'old-time' }, new Error('offline'));
assert(errHero.includes('Current analysis is unavailable') && errHero.includes('Showing the last successful report') &&
  !errHero.includes('Report healthy'), 'the error hero keeps the existing stale-report semantics');
assert.strictEqual(sandbox.__classifyExpStatus(heroExp, null, null, null, { canRead: false }).kind,
  'restricted', 'missing report.read shows the restricted copy');
assert(sandbox.__renderExpHero(classify(heroExp, null, null, null, { canRead: false }), null)
  .includes('Report read access is required'), 'restricted hero copy missing');
assert.strictEqual(classify(heroExp, null, null, null).kind, 'loading',
  'a granted permission with no report yet keeps the loading copy');
// Defensive: a shape matching no rule renders no banner — the decision
// snapshot below carries the raw server verdict instead of an invented state.
assert.strictEqual(sandbox.__renderExpHero({ kind: 'unknown', params: {}, actions: [] }, null), '',
  'an unclassified report shape must not invent a status banner');
sandbox.__S.capabilities = null;

// ---------------------------------------------------------------------------
// Experiment zones (§4.4): status band, zone navigation, data-health zone.
// The band shows the real registered contract fields and an em dash for the
// report time until a report actually loaded — never a fabricated value.
// ---------------------------------------------------------------------------
const bandExp = { id: 'exp-1', state: 'running', epoch: 3, flagRevision: 4,
  registrationVersion: 'sha256:reg-1', environmentId: 'prod' };
const bandNoReport = sandbox.__renderExpStatusBand(bandExp, null);
assert(bandNoReport.includes('id="exp-statusband"') && bandNoReport.includes('Status band'),
  'status band must be a labelled status region');
assert(bandNoReport.includes('Registration version') && bandNoReport.includes('sha256:reg-1') &&
  bandNoReport.includes('epoch') && bandNoReport.includes('>3<') &&
  bandNoReport.includes('Flag revision') && bandNoReport.includes('r4') &&
  bandNoReport.includes('Environment') && bandNoReport.includes('prod'),
  'status band must carry epoch, flag revision, registration version and environment');
assert(bandNoReport.includes('id="exp-band-report-time">—</b>'),
  'report time must render an em dash while no report is loaded');
assert(sandbox.__renderExpStatusBand({ ...bandExp, registrationVersion: '', environmentId: '' }, null)
  .split('id="exp-statusband"')[1].includes('—'),
  'missing registration data degrades to em dashes, never invented values');
const bandWithReport = sandbox.__renderExpStatusBand(bandExp, { generatedAt: '2026-10-14T08:00:00Z' });
assert(bandWithReport.includes('id="exp-band-report-time">' + sandbox.__fmtTime('2026-10-14T08:00:00Z') + '</b>'),
  'loaded report must show its real generation time in the band');

for (const activeTab of ['summary', 'health', 'evidence', 'rollout', 'settings']) {
  const nav = sandbox.__renderExpZoneNav(activeTab);
  assert(nav.includes('id="exp-zone-nav"') && nav.includes('role="tablist"'),
    'zone nav must be a labelled tablist');
  for (const tab of ['summary', 'health', 'evidence', 'rollout', 'settings']) {
    assert(nav.includes('data-exp-tab="' + tab + '"'), 'zone nav missing tab ' + tab);
  }
  assert((nav.match(/aria-current="true"/g) || []).length === 1 &&
    nav.includes('data-exp-tab="' + activeTab + '" aria-current="true"'),
    'exactly the active zone tab carries aria-current: ' + activeTab);
}
assert(sandbox.__renderExpZoneNav('summary').includes('Decision summary') &&
  sandbox.__renderExpZoneNav('summary').includes('Settings &amp; audit'),
  'zone nav renders all zone labels');

const healthDiag = { exposureMode: 'onRender', epoch: 3, lastCollectedAt: '2026-10-14T07:00:00Z',
  window: { version: 'utc-exposure-v1', watermark: '2026-10-14T06:00:00Z', backend: 'sqlite-raw',
    complete: false, recomputable: true, issues: ['watermark pending'] },
  perVariant: [{ variant: 'control', assignments: 100, exposures: 90, unexposed: 10, conversions: 8 }],
  crossVariantUnits: 0, assignmentMismatch: 0, duplicateEvents: 0,
  issues: ['10 assigned units have no recorded exposure'] };
const healthNoReport = sandbox.__renderHealthZone(bandExp, healthDiag, null);
assert(healthNoReport.includes('id="diag-refresh"') && healthNoReport.includes('watermark pending') &&
  healthNoReport.includes('sqlite-raw') && healthNoReport.includes('Incomplete') &&
  healthNoReport.includes('10 assigned units have no recorded exposure') &&
  healthNoReport.includes('Assignment, exposure, and conversion'),
  'health zone must keep watermark/backend/complete, blocking issues and the sample flow');
assert(healthNoReport.includes('Load the report to see this section.') &&
  !healthNoReport.includes('Unhealthy report') && !healthNoReport.includes('Report healthy'),
  'without a loaded report the health summary degrades instead of inventing a verdict');
const healthWithReport = sandbox.__renderHealthZone(bandExp, healthDiag,
  { healthy: false, epoch: 3, state: 'running', healthIssues: ['no randomized exposures recorded yet'],
    guardrails: [{ metric: 'latency_p95', detail: '+12% over cap', breached: true }] });
assert(healthWithReport.includes('no randomized exposures recorded yet') &&
  healthWithReport.includes('latency_p95') && healthWithReport.includes('+12% over cap') &&
  healthWithReport.includes('Unhealthy report'),
  'health zone must quote server healthIssues and guardrail breaches verbatim');
assert(sandbox.__renderHealthReportCard({ healthy: true, epoch: 3, state: 'running', guardrails: [] })
  .includes('Report healthy · epoch 3 · state running'),
  'healthy report summary shows the server-generated health line');

function render(report) {
  const box = {};
  sandbox.__renderReport(box, report);
  assert.strictEqual(typeof box.innerHTML, 'string', 'renderReport must set box.innerHTML');
  return box.innerHTML;
}

const base = {
  healthy: true, epoch: 1, state: 'running', generatedAt: '2026-10-03T00:00:00Z',
  minRuntimeSatisfied: true, analysisMethod: 'welch',
  primaryAttributionWindow: 3_600_000_000_000,
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
assert(html.indexOf('Decision snapshot') < html.indexOf('Analysis window (UTC)') &&
  html.includes('2026-10-03T00:00:00Z') && html.includes('primary significantly better'),
  'decision summary must lead with current verdict, reasons, generation time and watermark');
assert(html.includes('Analysis window (UTC)') && html.includes('Data watermark') &&
  html.includes('utc-exposure-v1') && html.includes('2026-10-02T00:00:00Z'), 'window metadata missing');
assert(html.includes('anytime-valid p'), 'sequential anytime-valid p missing');
assert(html.includes('not the decision gate'), 'sequential separation note missing');
assert(html.includes('Bayesian'), 'bayesian heading missing');
assert(html.includes('Attribution window'), 'frozen primary attribution window missing');
assert(html.includes('P(B&gt;A)'), 'bayesian probability-of-superiority missing');
assert(html.includes('expected loss'), 'bayesian expected loss missing');
assert(html.includes('separate from frequentist'), 'bayesian separation note missing');
const incomplete = render({ ...base, healthy: false, healthIssues: ['watermark pending'],
  window: { ...base.window, complete: false, issues: ['watermark pending'] } });
assert(incomplete.includes('Incomplete') && incomplete.includes('watermark pending'), 'incomplete window reason missing');
assert(incomplete.includes('report-summary-blocked') &&
  incomplete.indexOf('watermark pending') < incomplete.indexOf('Analysis window (UTC)'),
  'unhealthy report must show the blocking reason before technical detail');

const delivery = render({ ...base, healthy: false, healthIssues: ['delivery skew'],
  diagnostics: { assignmentSrm: { healthy: true, pValue: 0.8 },
    exposureSrm: { healthy: false, pValue: 0.00001 }, crossVariantUnits: 2,
    assignmentMismatch: 1, duplicateEvents: 3, rejectedByReason: { stale_epoch: 4 }, perVariant: [
      { variant: 'control', assignments: 100, exposures: 100, unexposed: 0, conversions: 12 },
      { variant: 'treatment', assignments: 100, exposures: 20, unexposed: 80, conversions: 3 },
    ] } });
assert(delivery.includes('Assignment SRM') && delivery.includes('Exposure SRM') &&
  delivery.includes('Not exposed') && delivery.includes('delivery skew') && delivery.includes('80') &&
  delivery.includes('Wrong experiment epoch') && delivery.includes('stale_epoch') &&
  delivery.includes('Duplicate events'),
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

// ---------------------------------------------------------------------------
// Overview modules — risk band / approvals waiting for me / recent releases.
// The browser consumes server-owned myAction/blockedReason/coverage verbatim:
// a degraded module must explain why it is missing and must NEVER claim
// "no risks" or invent a green all-clear, a zero count or a total.
// ---------------------------------------------------------------------------
const OVS = '2026-10-13T10:00:00Z';
const riskBand = sandbox.__renderOverviewRiskBand({ risks: { status: 'ok', reportCoverage: 'complete',
  coverage: { inspected: 2, total: 2, complete: true },
  items: [
    { kind: 'experiment_unhealthy', id: 'exp-9', environmentId: 'prod', severity: 'block',
      text: 'no randomized exposures recorded yet' },
    { kind: 'plan_paused', id: 'exp-8', environmentId: 'prod', severity: 'warn',
      text: 'data health gate: <img src=x>' }] } });
assert(riskBand.includes('Blocking') && riskBand.includes('Warning') &&
  riskBand.includes('no randomized exposures recorded yet') && riskBand.includes('Report unhealthy') &&
  riskBand.includes('data health gate: &lt;img src=x&gt;') && !riskBand.includes('<img') &&
  riskBand.includes('data-overview-risk="exp-9"'),
  'risk band must quote server reasons verbatim, mark severity and escape text');
const bandDown = sandbox.__renderOverviewRiskBand({ risks: { status: 'unavailable',
  reason: 'experiments_unavailable', reportCoverage: 'unavailable',
  coverage: { inspected: 0, total: 0, complete: false }, items: [] } });
assert(bandDown.includes('experiments_unavailable') && bandDown.includes('unavailable') &&
  bandDown.includes('data-overview-retry') && !bandDown.includes('No blocking risks'),
  'unavailable risk band must explain why and never claim no risks');
const bandPartial = sandbox.__renderOverviewRiskBand({ risks: { status: 'partial',
  reportCoverage: 'partial', coverage: { inspected: 50, total: 120, complete: false }, items: [] } });
assert(bandPartial.includes('not a guarantee') &&
  !bandPartial.includes('No blocking risks in the inspected scope.'),
  'partial coverage must not render the clear all-clear copy');
const bandOkIncomplete = sandbox.__renderOverviewRiskBand({ risks: { status: 'ok',
  reportCoverage: 'complete', coverage: { inspected: 50, total: 120, complete: false }, items: [] } });
assert(bandOkIncomplete.includes('not a guarantee'),
  'ok status with incomplete coverage still must not claim no risks');
const bandClear = sandbox.__renderOverviewRiskBand({ risks: { status: 'ok', reportCoverage: 'complete',
  coverage: { inspected: 0, total: 0, complete: true }, items: [] } });
assert(bandClear.includes('No blocking risks in the inspected scope.') &&
  !bandClear.includes('banner ok') && !bandClear.includes('chip green') && !bandClear.includes('0%'),
  'empty data must not produce a green success state or fabricated percentages');
const bandRestricted = sandbox.__renderOverviewRiskBand({ risks: { status: 'restricted',
  reportCoverage: 'restricted', coverage: { inspected: 1, total: 1, complete: true }, items: [] } });
assert(bandRestricted.includes('hidden by your permissions') &&
  !bandRestricted.includes('No blocking risks in the inspected scope.'),
  'restricted report coverage must stay visible and block the all-clear');

const pending = sandbox.__renderOverviewApprovals({ status: 'ok', actionableTotal: 2, items: [
  { id: 'ap-1', action: 'flag.publish', object: '<img src=x>', environmentId: 'prod',
    submittedBy: 'alice', submittedAt: OVS, risk: 'normal', myAction: 'review', blockedReason: '' },
  { id: 'ap-2', action: 'flag.rollback', object: 'flag-b', environmentId: 'prod',
    submittedBy: 'alice', submittedAt: OVS, risk: 'high', expiresAt: OVS,
    myAction: 'execute', blockedReason: '' },
  { id: 'ap-3', action: 'flag.publish', object: 'flag-c', environmentId: 'prod',
    // The three codes the server actually emits (role|expired|breakglass_review)
    // are pinned by ap-4/5/6 below; ap-3 keeps the unknown-code passthrough
    // covered, since a future server enum value must never render as blank.
    submittedBy: 'bob', submittedAt: OVS, risk: 'normal', myAction: '', blockedReason: 'future_reason' },
  { id: 'ap-4', action: 'flag.publish', object: 'flag-d', environmentId: 'prod',
    submittedBy: 'carol', submittedAt: OVS, risk: 'normal', myAction: '', blockedReason: 'role' },
  { id: 'ap-5', action: 'flag.publish', object: 'flag-e', environmentId: 'prod',
    submittedBy: 'dave', submittedAt: OVS, risk: 'normal', myAction: '', blockedReason: 'expired' },
  { id: 'ap-6', action: 'breakglass.review', object: 'flag-f', environmentId: 'prod',
    submittedBy: 'erin', submittedAt: OVS, risk: 'high', myAction: '', blockedReason: 'breakglass_review' }] });
assert(pending.includes('Waiting for my review') && pending.includes('Waiting for my execution') &&
  pending.includes('2 waiting for you') && pending.includes('data-overview-approval="ap-1"'),
  'server myAction verdicts must render as chips with the true actionable total');
assert(pending.includes('future_reason') && pending.includes('cannot approve or execute') &&
  pending.includes('Grant expired') && pending.includes('approvals inbox'),
  'mapped blocked reasons localize, an unknown code passes through verbatim');
assert(pending.includes('&lt;img src=x&gt;') && !pending.includes('<img'),
  'approval objects must be escaped');
const pendingDown = sandbox.__renderOverviewApprovals({ status: 'unavailable', reason: 'approvals_unavailable' });
assert(pendingDown.includes('approvals_unavailable') && pendingDown.includes('data-overview-retry') &&
  !pendingDown.includes('Nothing is waiting'),
  'unavailable approvals module must not present an empty inbox');
assert(sandbox.__renderOverviewApprovals({ status: 'ok', actionableTotal: 0, items: [] })
  .includes('Nothing is waiting for your action.'), 'empty actionable inbox copy missing');

const releases = sandbox.__renderOverviewReleases({ status: 'ok',
  coverage: { inspected: 500, total: 500, complete: false, note: 'audit-scan-window' },
  items: [
    { time: OVS, actor: 'alice', action: 'flag.publish', object: 'flag-a', environmentId: 'prod' },
    { time: OVS, actor: 'alice', action: 'flag.rollback', object: 'flag-a', environmentId: 'prod' },
    { time: OVS, actor: 'bob', action: 'flag.emergency_disable', object: 'flag-b', environmentId: 'prod' },
    { time: OVS, actor: 'bob', action: 'segment.publish', object: 'seg-1', environmentId: 'prod' },
    { time: OVS, actor: 'rollout-engine', action: 'release_plan.apply_ratio', object: 'exp-1', environmentId: 'prod' },
    { time: OVS, actor: 'rollout-engine', action: 'release_plan.rollback', object: 'exp-1', environmentId: 'prod' },
    { time: OVS, actor: 'carol', action: 'metric.publish', object: 'metric-1/v2', environmentId: 'prod' }] });
for (const label of ['Flag published', 'Flag rolled back', 'Flag emergency-disabled',
  'Segment published', 'Rollout step applied', 'Rollout rolled back']) {
  assert(releases.includes(label), 'release action label missing: ' + label);
}
assert(releases.includes('metric.publish'), 'unknown actions must fall back to the raw audit string');
assert(releases.includes('Scanned the latest 500 audit records.') &&
  releases.includes('Older records were not scanned.'),
  'a full scan window must disclose truncation');
assert(releases.includes('data-overview-release="flag-a"') && releases.includes('mobile-record'),
  'release rows must link into the audit log and provide mobile records');
assert(sandbox.__renderOverviewReleases({ status: 'ok',
  coverage: { inspected: 3, total: 3, complete: true }, items: [] })
  .includes('No releases recorded in the scanned audit window.'), 'empty release window copy missing');
const releasesDown = sandbox.__renderOverviewReleases({ status: 'unavailable', reason: 'audit_unavailable' });
assert(releasesDown.includes('audit_unavailable') && releasesDown.includes('data-overview-retry'),
  'unavailable releases module must explain why and offer retry');

const stats = sandbox.__renderOverviewStats({ pausedExperiments: null, runningExperiments: 3,
  pendingApprovals: null, publishedFlags: 0 });
assert(stats.includes('<strong>3</strong>') && stats.includes('<strong>0</strong>') &&
  (stats.match(/<strong>—<\/strong>/g) || []).length === 2 && !stats.includes('<strong>null</strong>'),
  'a null count means the source failed and renders as an em dash, a real zero stays zero');
assert(sandbox.__renderOverviewStats(null).split('<strong>—</strong>').length === 5,
  'missing counts object degrades to em dashes, never zeros');

const moduleFailure = sandbox.__renderOverviewModuleFailure('overview.recentReleases',
  { status: 'unavailable', reason: 'audit_unavailable', coverage: { inspected: 0, total: 0, complete: false } });
assert(moduleFailure.includes('Recent releases is unavailable: audit_unavailable') &&
  moduleFailure.includes('Status: unavailable') && moduleFailure.includes('Inspected 0 of 0') &&
  moduleFailure.includes('data-overview-retry') && moduleFailure.includes('banner warn'),
  'module failure banner must state name, reason, status, coverage and retry');

console.log('renderReport DOM contract: all assertions passed (9 report shapes + six-state hero matrix)');

const manyFlags = Array.from({ length: 101 }, (_, i) => ({ key: 'flag-' + i, draft: { state: i % 2 ? 'submitted' : 'editing' } }));
assert.equal(sandbox.__flagListPage(manyFlags).records.length, 25, 'large lists render a bounded page');
assert.equal(sandbox.__flagListPage(manyFlags, { page: 4 }).records.length, 1, 'last page preserves remaining records');
assert.equal(sandbox.__flagListPage(manyFlags, { query: 'FLAG-100' }).records[0].key, 'flag-100', 'search covers records outside the visible page');
assert.equal(sandbox.__flagListPage(manyFlags, { category: 'submitted' }).total, 50, 'status filter count includes all pages');
assert.equal(sandbox.__flagListPage([], { page: 99 }).page, 0, 'empty search clamps page safely');
assert(sandbox.__renderFlagWorkflow({ state: 'approved' }).includes('aria-current="step">Approved'), 'approved workflow identifies the next publishing stage');
assert(sandbox.__renderPublishedSummary({ type: 'string', variations: { a: '<script>' }, defaultRule: { variation: 'a' } }).includes('&lt;script&gt;'), 'published summary escapes variant values');

const namedFlags = [{ key: 'checkout-v2', draft: { content: { displayName: '\u4e2d\u6587\u540d\u79f0', description: 'For regional checkout' } } }];
assert.equal(sandbox.__flagListPage(namedFlags, { query: '\u4e2d\u6587' }).total, 1, 'list search includes display names');
assert.equal(sandbox.__flagListPage(namedFlags, { query: 'REGIONAL' }).total, 1, 'list search includes descriptions without changing machine keys');
assert(sandbox.__resourceIdentity({ displayName: '<name>', description: '<description>' }, 'safe-key', 'data-flag').includes('data-flag="safe-key"'), 'named navigation continues to use the ASCII key');
assert(sandbox.__resourceIdentity({ displayName: '<name>' }, 'safe-key').includes('&lt;name&gt;'), 'display names are escaped');
assert(sandbox.__renderFlagDiff({ content: { displayName: 'Before' } }, { content: { displayName: 'After', description: 'New context' } }).includes('New context'), 'metadata changes are included in draft review');
let resolveSuggestions;
sandbox.__testApi = () => new Promise(resolve => { resolveSuggestions = resolve; });
vm.runInNewContext('api = globalThis.__testApi;', sandbox);
const suggestionList = { innerHTML: '' };
const suggestionRoot = { dataset: { segmentKeys: '[]' }, isConnected: true,
  querySelectorAll: () => [suggestionList] };
assert.equal(sandbox.__loadEditorSegmentSuggestions(suggestionRoot, 'self'), undefined, 'optional suggestions never block editor setup');
resolveSuggestions([{ key: 'self' }, { key: '<audience>' }]);
await Promise.resolve();
assert(suggestionList.innerHTML.includes('&lt;audience&gt;') && !suggestionList.innerHTML.includes('self'), 'lazy suggestions exclude self references and escape keys');
assert.deepEqual(JSON.parse(suggestionRoot.dataset.segmentKeys), ['<audience>'], 'lazy suggestions update cached keys without repainting controls');
const staleRoot = { dataset: {}, isConnected: false, querySelectorAll() { throw new Error('stale editor mutated'); } };
sandbox.__loadEditorSegmentSuggestions(staleRoot);
resolveSuggestions([{ key: 'ignored' }]);
await Promise.resolve();
assert.equal(staleRoot.dataset.segmentKeys, undefined, 'detached editors ignore late suggestions');
