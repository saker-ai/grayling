// i18n DOM/contract test for the admin console. Loads the REAL web/i18n.js +
// web/app.js in a Node vm (no browser needed) and asserts:
//   1. language resolution order: localStorage golab_lang > navigator.language
//      (zh* → zh) > en;
//   2. every key view (flags list/detail, segments list/detail, experiments
//      list/create/detail + report, rollout block, tokens, audit, ai, debug)
//      renders in BOTH en and zh — sampled strings hit the right language and
//      the layout-critical node IDs are identical across languages;
//   3. the top-bar language switch reacts to click simulation: setLang
//      persists to localStorage, flips the active class and re-renders the
//      current view in place (no page reload — route() into the same #main);
//   4. dictionary completeness: zero t() misses collected across all renders
//      and report shapes in both languages, en/zh catalog key-set parity, and
//      every statically-extracted t('key') / data-i18n="key" exists in both
//      catalogs;
//   5. static scan: app.js and index.html contain no CJK characters outside
//      comments (comments are the whitelisted exception; the scan strips them
//      and then requires zero CJK code points in code/strings/markup).
// Run via `go test ./web/` (which shells to node) or `node web/i18n_contract.mjs`.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import assert from 'node:assert';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const i18nCode = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8');
const appCode = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const flagEditorCode = fs.readFileSync(path.join(__dirname, 'flag_editor.js'), 'utf8');
const segmentEditorCode = fs.readFileSync(path.join(__dirname, 'segment_editor.js'), 'utf8');
const approvalsCode = fs.readFileSync(path.join(__dirname, 'approvals.js'), 'utf8');
const expWorkbenchCode = fs.readFileSync(path.join(__dirname, 'exp_workbench.js'), 'utf8');
const indexHTML = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const code = i18nCode + '\n' + flagEditorCode + '\n' + segmentEditorCode + '\n' + approvalsCode + '\n' + expWorkbenchCode + '\n' + appCode;

const BRIDGE = '\n;globalThis.__x = { I18N, t, setLang, detectLang, localeFor, fmtTime, fmtDurNs,' +
  ' S, VIEWS, route, renderReport, renderRollout, lintFlag, applyStaticI18n, bindLangSwitch, registerI18nHooks, applyTheme, themePreference, consoleHash, restoreConsoleHash, classifyExpStatus, renderExpHero };';

// --- sandbox factory --------------------------------------------------------
// Rich element stub: tolerates property writes, records classList.toggle and
// answers querySelector with fresh stubs (rows built via createElement).
function el() {
  const s = {
    style: {}, dataset: {}, value: '', checked: false, className: '', rows: 0,
    innerHTML: '', textContent: '', placeholder: '', title: '', type: '',
    _toggles: [],
    classList: {
      add() {}, remove() {}, contains() { return false; },
      toggle(cls, on) { s._toggles.push([cls, on]); },
    },
    appendChild() {}, replaceChildren(child) { s.innerHTML = child.innerHTML; },
    remove() {}, setAttribute() {}, addEventListener() {},
    select() {}, closest() { return null; },
    querySelector() { return el(); }, querySelectorAll() { return []; },
  };
  return s;
}

// viewRoot answers #id selectors ONLY for ids present in the CURRENT
// innerHTML (like the real DOM after a render), everything else gets a
// generic stub so delegated bindings (tbody, table.grid) never crash.
function viewRoot() {
  const generic = new Map();
  const root = el();
  root.querySelector = function (sel) {
    if (typeof sel === 'string' && /^#[A-Za-z0-9_-]+$/.test(sel)) {
      if (!root.innerHTML.includes('id="' + sel.slice(1) + '"')) return null;
      if (!generic.has(sel)) generic.set(sel, el());
      return generic.get(sel);
    }
    const k = 'g:' + String(sel);
    if (!generic.has(k)) generic.set(k, el());
    return generic.get(k);
  };
  return root;
}

function makeSandbox({ storedLang, navLang } = {}) {
  const store = new Map();
  if (storedLang) store.set('golab_lang', storedLang);
  const warns = [];
  const chrome = new Map(); // document.querySelector stubs keyed by selector
  const fetchCalls = [];
  const sandbox = {
    URLSearchParams,
    document: {
      documentElement: { dataset: {} },
      addEventListener() {},
      createElement: () => el(),
      querySelector(sel) {
        if (!chrome.has(sel)) chrome.set(sel, el());
        return chrome.get(sel);
      },
      querySelectorAll() { return []; },
      body: { appendChild() {} },
      title: '',
    },
    localStorage: {
      getItem: k => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
      removeItem: k => store.delete(k),
    },
    location: { hash: '', href: 'http://localhost/', search: '', origin: 'http://localhost' },
    history: { pushState() {}, replaceState() {} },
    navigator: navLang === undefined ? { clipboard: { writeText: () => Promise.resolve() } }
      : { language: navLang, clipboard: { writeText: () => Promise.resolve() } },
    console: { warn: (...a) => warns.push(a.join(' ')), log() {}, error() {} },
    setInterval: () => 0, clearInterval() {}, setTimeout: () => 0, clearTimeout() {},
    fetch(url, opts) {
      fetchCalls.push({ url, opts });
      return mockResponse(url);
    },
    confirm: () => true,
    prompt: () => 'reason',
    matchMedia: () => ({ matches: true }),
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInNewContext(code + BRIDGE, sandbox);
  return { x: sandbox.__x, store, warns, chrome, fetchCalls, sandbox };
}

const tick = () => new Promise(r => setTimeout(r, 5));

// --- mock API data ----------------------------------------------------------
const NS_H = 3.6e12;
const TS = '2026-09-20T00:00:00Z';
const flagSummary = {
  key: 'my-flag', currentRevision: 2,
  draft: { state: 'editing', deleted: false, updatedAt: TS, updatedBy: 'alice', risk: 'normal', content: { type: 'bool' } },
  current: { content: { type: 'bool' }, hash: 'abc123def456789abc', publishedAt: TS, publishedBy: 'bob' },
};
const flagDetail = {
  ...flagSummary,
  experimentImpact: { total: 1, references: [
    { id: 'exp-1', state: 'running', epoch: 1, flagRevision: 2, updatedAt: TS },
  ] },
  revisions: [
    { revision: 2, hash: 'abc123def456789abc', publishedAt: TS, publishedBy: 'bob', note: 'ship', rollbackOf: null },
    { revision: 1, hash: '0123456789abcdef00', publishedAt: TS, publishedBy: 'alice', note: '', rollbackOf: null },
  ],
};
const flagContent = { schemaVersion: 1, key: 'my-flag', type: 'bool', variations: { control: false, treatment: true }, defaultRule: { variation: 'control' }, targeting: [] };
flagDetail.draft = { ...flagDetail.draft, content: flagContent };
const segments = [{ key: 'seg-1', state: 'submitted', risk: 'normal', updatedBy: 'alice', updatedAt: TS, content: { schemaVersion: 1, key: 'seg-1', constraints: [], segments: [] } }];
const expSummary = { id: 'exp-1', flagKey: 'my-flag', unit: 'user_id', state: 'running', variants: ['control', 'treatment'], epoch: 1, createdAt: TS };
const plan = {
  steps: [
    { name: 'canary-5', ratio: 5, minRuntime: 24 * NS_H, observationWindow: 12 * NS_H, autoAdvance: true },
    { name: 'ramp-100', ratio: 100, minRuntime: 48 * NS_H, observationWindow: 24 * NS_H, autoAdvance: false },
  ],
  state: 'running', currentStep: 1, stepStartedAt: TS, createdAt: TS, updatedAt: TS, createdBy: 'owner-token',
  history: [{ at: TS, actor: 'rollout-engine', action: 'plan.start', step: 0, detail: 'entering canary' }],
};
const expDetail = {
  ...expSummary, flagRevision: 2, createdBy: 'alice', layer: null, layerSlot: null, eligibility: 50,
  registrationVersion: 'sha256:reg-fixture', environmentId: 'prod',
  proportions: { control: 50, treatment: 50 },
  primaryMetric: { key: 'checkout_success', type: 'welch', direction: 'increase' },
  alpha: 0.05, power: 0.8, mde: 0.01, minSamplePerArm: 1000,
  minRuntime: 24 * NS_H, analysisWindow: 7 * 24 * NS_H, analysisMethod: 'welch',
  hypothesis: 'faster checkout converts better',
  guardrails: [{ metricKey: 'latency_p95', type: 'mean', direction: 'decrease', maxRelativeRegression: 0.05, minSample: 100 }],
  releasePlan: plan,
};
const report = {
  healthy: true, epoch: 1, state: 'running', minRuntimeSatisfied: true, analysisMethod: 'welch',
  srm: { chiSquare: 0.1, pValue: 0.75, healthy: true, observed: [500, 500], expected: [500, 500] },
  perVariant: [{ variant: 'control', exposedUnits: 500, randomizedUnits: 500, exposures: 900, convertedUnits: 50, primaryValue: 0.1 }],
  primary: { type: 'welch', pValue: 0.001, diff: 0.2, relativeDiff: 0.1, relativeMeaningful: true, ciLow: 0.1, ciHigh: 0.3, significant: true },
  sequential: { pValue: 0.002, csLow: 0.05, csHigh: 0.35, tau: 0.2, logLambda: 6.2, significant: true },
  bayesian: { mean: { probBSuperior: 0.99, expectedLossB: 0.0001, credibleLow: 0.1, credibleHigh: 0.3 } },
  guardrails: [{ metric: 'latency_p95', detail: 'ok', breached: false }],
  decision: { verdict: 'ship', reasons: ['primary significantly better'], guardrailBreaches: [] },
};
const diagnostics = {
  experimentId: 'exp-1', epoch: 1, exposureMode: 'onRender', generatedAt: TS,
  window: { version: 'utc-exposure-v1', start: TS, end: TS, watermark: TS,
    backend: 'sqlite-raw', complete: false, recomputable: true, issues: ['watermark pending'] },
  perVariant: [{ variant: 'control', assignments: 100, randomizedAssignments: 100,
    exposures: 90, randomizedExposures: 90, unexposed: 10, conversions: 8, duplicateExposures: 0 }],
  crossVariantUnits: 0, assignmentMismatch: 0, lastCollectedAt: TS,
  issues: ['10 assigned units have no recorded exposure'],
};
// Paused fixture for the six-state hero: the ended copy renders synchronously
// from the experiment state and the transition audit is fetched with an exact
// object filter (asserted below via fetchCalls).
const expDetailEnded = {
  ...expDetail, id: 'exp-2', state: 'paused', epoch: 2, releasePlan: null,
};
const endedReport = {
  ...report, state: 'paused', epoch: 2,
  decision: { verdict: 'continue', reasons: ['primary metric not significant'], guardrailBreaches: [] },
};
const tokens = [
  { id: 'tk1', name: 'ci-token', kind: 'sdk', role: 'viewer', prefix: 'glab_sdk_ab', environmentId: 'prod', createdAt: TS, revoked: false },
  { id: 'tk2', name: 'old-admin', kind: 'admin', role: 'owner', prefix: 'glab_adm_cd', environmentId: '', createdAt: TS, revoked: true },
];
const audit = [{ time: TS, actor: 'alice', action: 'flag.publish', object: 'my-flag', environmentId: 'prod', detail: { revision: 2 } }];
const aiResources = {
  resources: [{ type: 'prompt', logicalId: 'checkout-helper', physicalId: 'phys-1', owner: 'alice', createdAt: TS }],
  revocations: { 'prompt\u0000old-one': 'digest drift' },
};
const allowlist = [{ provider: 'openai', model: 'gpt-4o', enabled: true }];
const quotas = { maxFlags: 500, maxFlagBytes: 65536, maxSegments: 200,
  maxExperiments: 100, maxEventBytes: 16384, maxEventsPerBatch: 500,
  eventRetention: 2592000000000000 };
const metric = { key: 'activation.complete', version: 1, displayName: 'Activation completion',
  description: 'Completed activation', owner: 'growth', source: 'collect', eventKeys: ['activation_complete'],
  attributionWindow: 604800000000000, spec: { key: 'activation_complete', type: 'proportion', direction: 'increase' },
  status: 'draft', hash: 'sha256:abc123' };
// Overview aggregation fixture: every count present, one blocking risk quoting
// the server reason verbatim, one approval waiting for my review, two release
// records, and partial risk coverage so the degraded copy is swept too.
const overview = {
  generatedAt: TS, workspaceId: 'acme', environmentId: 'prod',
  counts: { pausedExperiments: 1, runningExperiments: 2, pendingApprovals: 3, publishedFlags: 4 },
  risks: { status: 'partial', reportCoverage: 'partial',
    coverage: { inspected: 3, total: 7, complete: false },
    items: [{ kind: 'experiment_unhealthy', id: 'exp-1', environmentId: 'prod',
      severity: 'block', text: 'no randomized exposures recorded yet' }] },
  pendingApprovals: { status: 'ok', actionableTotal: 1, items: [
    { id: 'approval-1', action: 'flag.publish', object: 'my-flag', environmentId: 'prod',
      submittedBy: 'alice', submittedAt: TS, risk: 'normal', expiresAt: null,
      myAction: 'review', blockedReason: '' }] },
  recentReleases: { status: 'ok',
    coverage: { inspected: 2, total: 2, complete: true, note: 'audit-scan-window' },
    items: [
      { time: TS, actor: 'alice', action: 'flag.publish', object: 'my-flag', environmentId: 'prod' },
      { time: TS, actor: 'rollout-engine', action: 'release_plan.apply_ratio', object: 'exp-1', environmentId: 'prod' }] },
};

function mockResponse(url) {
  const json = v => Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(v)) });
  if (url.includes('/overview')) return json(overview);
  if (url.includes('/debug/eval')) return json({ result: { value: true, variationType: 'treatment', reason: 'targeting', bucket: 42 }, manifestRevision: 7, note: '' });
  if (url.includes('/ai/resources')) return json(aiResources);
  if (url.includes('/model-allowlist')) return json(allowlist);
  if (url.endsWith('/quotas')) return json(quotas);
  if (url.endsWith('/tokens')) return json(tokens);
  if (url.endsWith('/approvals')) return json([{ id: 'approval-1', state: 'submitted', action: 'flag.publish', object: 'my-flag' }]);
  if (url.includes('/metrics/activation.complete/versions/1')) return json(metric);
  if (url.includes('/metrics?')) return json({ items: [metric] });
  if (url.includes('/audit')) return json(audit);
  if (url.includes('/experiments/exp-1/report')) return json(report);
  if (url.includes('/experiments/exp-1/diagnostics')) return json(diagnostics);
  if (url.includes('/experiments/exp-1')) return json(expDetail);
  if (url.includes('/experiments/exp-2/report')) return json(endedReport);
  if (url.includes('/experiments/exp-2/diagnostics')) return json(diagnostics);
  if (url.includes('/experiments/exp-2')) return json(expDetailEnded);
  if (url.includes('/experiment-summaries?')) return json({ items: [{ experiment: expSummary,
    report: { status: 'ready', healthy: false, verdict: 'hold', issue: 'watermark pending', generatedAt: TS } }],
    total: 1, offset: 0, limit: 20 });
  if (url.endsWith('/experiments')) return json([expSummary]);
  if (url.endsWith('/flags/my-flag')) return json(flagDetail);
  if (url.endsWith('/flags')) return json([flagSummary]);
  if (url.endsWith('/segments')) return json(segments);
  if (url.endsWith('/environments')) return json([{ slug: 'prod', name: 'Production' }]);
  if (url.endsWith('/admin/workspaces')) return json([{ slug: 'acme', name: 'Acme' }]);
  return json({});
}

// ===========================================================================
// 1. Language resolution order: golab_lang > navigator.language (zh*→zh) > en
// ===========================================================================
assert.strictEqual(makeSandbox({ storedLang: 'en', navLang: 'zh-CN' }).x.I18N.lang, 'en',
  'localStorage golab_lang must win over navigator.language');
assert.strictEqual(makeSandbox({ storedLang: 'zh', navLang: 'en-US' }).x.I18N.lang, 'zh',
  'localStorage golab_lang=zh must win');
assert.strictEqual(makeSandbox({ navLang: 'zh-CN' }).x.I18N.lang, 'zh', 'zh-CN → zh');
assert.strictEqual(makeSandbox({ navLang: 'zh-TW' }).x.I18N.lang, 'zh', 'zh-TW → zh');
assert.strictEqual(makeSandbox({ navLang: 'en-US' }).x.I18N.lang, 'en', 'en-US → en');
assert.strictEqual(makeSandbox({ navLang: 'fr-FR' }).x.I18N.lang, 'en', 'non-zh → en');
assert.strictEqual(makeSandbox({}).x.I18N.lang, 'en', 'no navigator.language → en');

const themed = makeSandbox();
themed.x.applyTheme();
assert.strictEqual(themed.sandbox.document.documentElement.dataset.theme, 'light', 'system theme follows media');
themed.store.set('grayling_theme', 'dark');
themed.x.applyTheme();
assert.strictEqual(themed.sandbox.document.documentElement.dataset.theme, 'dark', 'saved theme overrides media');
assert(themed.x.restoreConsoleHash('#/console/acme/prod/metrics/activation.complete/1'));
assert.strictEqual(themed.x.S.metricRef.key, 'activation.complete');
assert.strictEqual(themed.x.consoleHash(), '#/console/acme/prod/metrics/activation.complete/1');
assert(!themed.x.restoreConsoleHash('#/console/acme/prod/unknown'), 'unknown deep link view rejected');
// experiment zone deep links: every tab restores and round-trips through
// consoleHash; the legacy 'diagnostics' link normalizes to the health zone
// (old bookmarks keep working); unknown tabs stay rejected.
for (const tab of ['health', 'evidence', 'rollout', 'settings']) {
  assert(themed.x.restoreConsoleHash('#/console/acme/prod/experiments/exp-1/' + tab), tab + ' deep link rejected');
  assert.strictEqual(themed.x.S.expTab, tab);
  assert.strictEqual(themed.x.consoleHash(), '#/console/acme/prod/experiments/exp-1/' + tab);
}
assert(themed.x.restoreConsoleHash('#/console/acme/prod/experiments/exp-1'));
assert.strictEqual(themed.x.S.expTab, 'summary', 'no tab segment means summary');
assert.strictEqual(themed.x.consoleHash(), '#/console/acme/prod/experiments/exp-1');
assert(themed.x.restoreConsoleHash('#/console/acme/prod/experiments/exp-1/diagnostics'));
assert.strictEqual(themed.x.S.expTab, 'health', 'legacy diagnostics alias must normalize to health');
assert.strictEqual(themed.x.consoleHash(), '#/console/acme/prod/experiments/exp-1/health');
assert(!themed.x.restoreConsoleHash('#/console/acme/prod/experiments/exp-1/unknown'),
  'unknown experiment tab rejected');
assert(themed.x.restoreConsoleHash('#/console/acme/prod/audit/exp-8'));
assert.strictEqual(themed.x.S.auditObject, 'exp-8');
assert.strictEqual(themed.x.consoleHash(), '#/console/acme/prod/audit/exp-8');
assert(!themed.x.restoreConsoleHash('#/console/acme/prod/audit/exp-8/extra'),
  'audit deep link accepts one exact object only');
const scopedAudit = makeSandbox();
scopedAudit.x.S.ws = 'acme';
scopedAudit.x.S.auditObject = 'exp-8';
const auditRoot = viewRoot();
await scopedAudit.x.VIEWS.audit(auditRoot);
assert(scopedAudit.fetchCalls.some(call => call.url.includes('/audit?') &&
  call.url.includes('&object=exp-8')), 'audit deep link must request an exact object filter');
assert(auditRoot.innerHTML.includes('id="a-object-filter"'), 'audit page must show active exact filter');
assert(auditRoot.querySelector('#a-box').innerHTML.includes('class="mobile-record audit-record"'),
  'audit results must provide readable mobile records');

// ===========================================================================
// 2. Bilingual view sweep — sampled strings hit each language, layout IDs are
//    identical across languages.
// ===========================================================================
const SWEEPS = [
  { name: 'overview', view: 'overview',
    en: ['Workspace overview', 'Risk band', 'Pending approvals', 'Approvals waiting for me',
      'Recent releases', 'Refresh status', 'Waiting for my review', 'Flag published',
      'Rollout step applied', 'Scanned the latest 2 audit records.',
      'most recently updated of 7 experiments'],
    zh: ['工作空间总览', '风险带', '待审批变更', '待我处理审批',
      '近期发布', '刷新状态', '待我审批', 'Flag 已发布',
      '放量步骤已生效', '已扫描最近 2 条审计记录。',
      '最近更新的 3/7 个实验'],
    shared: ['no randomized exposures recorded yet', 'my-flag', 'alice', 'flag.publish', 'exp-1'],
    ids: ['overview-refresh', 'overview-band', 'overview-pending', 'overview-releases',
      'overview-coverage', 'overview-all-experiments'] },
  { name: 'metrics-list', view: 'metrics',
    setup: S => { S.metricRef = null; S.metricQuery = ''; S.metricStatus = ''; S.metricCursor = ''; },
    en: ['Metric catalog', 'New metric', 'All statuses'],
    zh: ['指标目录', '新建指标', '全部状态'],
    shared: ['Activation completion'],
    ids: ['metric-new', 'metric-search'] },
  { name: 'metrics-detail', view: 'metrics',
    setup: S => { S.metricRef = { key: 'activation.complete', version: 1 }; },
    en: ['Definition hash', 'Submit for review', 'Publish approved version'],
    zh: ['定义哈希', '提交评审', '发布已批准版本'],
    ids: ['metric-back', 'metric-submit', 'metric-publish'] },
  { name: 'flags-list', view: 'flags',
    setup: S => { S.flagKey = null; S.flagNew = false; },
    en: ['<h2>Flags</h2>', '+ New flag', 'Draft state', 'Revision', '1 flag(s)'],
    zh: ['<h2>功能开关</h2>', '+ 新建开关', '草稿状态', '版本', '1 个开关'],
    shared: ['my-flag'],
    ids: ['flag-new'] },
  { name: 'flags-detail', view: 'flags',
    setup: S => { S.flagKey = 'my-flag'; S.flagNew = false; },
    en: ['Draft editor', 'Save draft', 'Mark deleted', 'History &amp; rollback', 'Registered experiments',
      'Import (creates HIGH-risk draft)', 'Rollback to r2', 'Current published (r2)', 'Validate JSON'],
    zh: ['草稿编辑器', '保存草稿', '标记删除', '历史与回滚', '已登记的关联实验',
      '导入（创建 HIGH 风险草稿）', '回滚到 r2', '当前已发布（r2）', '校验 JSON'],
    ids: ['flag-json', 'flag-save', 'flag-submit', 'flag-approve', 'flag-reject',
      'flag-publish', 'flag-delete', 'flag-revs', 'gff-run'] },
  { name: 'segments-list', view: 'segments',
    setup: S => { S.segKey = null; },
    en: ['<h2>Segments</h2>', '+ New segment', 'Updated by', '1 draft(s)'],
    zh: ['<h2>受众分段</h2>', '+ 新建分段', '更新人', '1 个草稿'],
    ids: ['seg-new'] },
  { name: 'segments-detail', view: 'segments',
    setup: S => { S.segKey = 'seg-1'; },
    en: ['Segment draft', 'Validate JSON', 'Save draft', 'Publish'],
    zh: ['分段草稿', '校验 JSON', '保存草稿', '发布'],
    ids: ['seg-json', 'seg-save', 'seg-submit', 'seg-approve', 'seg-publish'] },
  { name: 'experiments-list', view: 'experiments',
    setup: S => { S.expId = null; S.expCreate = false; },
    en: ['<h2>Experiments</h2>', '+ New experiment', 'Data health / decision', 'Needs diagnosis'],
    zh: ['<h2>实验</h2>', '+ 新建实验', '数据健康 / 决策', '需要诊断'],
    shared: ['exp-1', 'watermark pending'],
    ids: ['exp-new', 'exp-list-filter', 'exp-prev', 'exp-next'] },
  { name: 'experiments-create', view: 'experiments',
    setup: S => { S.expCreate = true; S.expId = null; },
    en: ['New experiment', 'Variants &amp; proportions (percent — must sum to 100)',
      'Create experiment', 'Statistics &amp; runtime', '+ Add guardrail', 'Primary metric'],
    zh: ['新建实验', '变体与比例（百分比 — 之和必须为 100）',
      '创建实验', '统计与运行时长', '+ 添加护栏', '主指标'],
    ids: ['x-create', 'x-vars', 'x-guards', 'x-mkey'] },
  { name: 'experiments-detail', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-1'; S.expTab = 'summary'; },
    en: ['Status band', 'Registration version', 'Environment', 'Report time',
      'Experiment sections', 'Decision summary', 'Data health', 'Effect evidence', 'Rollout timeline',
      'Settings &amp; audit', 'View the pre-registration contract',
      'Loading the current analysis and data health…'],
    zh: ['状态带', '注册版本', '环境', '报告时间',
      '实验区域导航', '决策摘要', '数据健康', '效果证据', '放量时间轴',
      '设置与审计', '查看预注册契约',
      '正在读取当前分析与数据健康状态…'],
    shared: ['exp-1', 'my-flag', 'sha256:reg-fixture'],
    ids: ['exp-statusband', 'exp-zone-nav', 'exp-decision', 'exp-band-report-time',
      'exp-view-prereg', 'exp-auto'] },
  { name: 'experiments-detail-ended', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-2'; S.expTab = 'summary'; },
    en: ['Transition details are unavailable; check the audit log.', 'View audit',
      'Settings &amp; audit'],
    zh: ['未获取到状态变更详情，请查看审计记录。', '查看审计',
      '设置与审计'],
    shared: ['exp-2', 'paused', 'my-flag'],
    ids: ['exp-statusband', 'exp-zone-nav', 'exp-decision', 'exp-hero-actions',
      'exp-summary-snapshot'] },
  { name: 'experiments-health', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-1'; S.expTab = 'health'; },
    en: ['Experiment diagnostics', 'Data status', 'Blocking issues', 'Assignment to delivery', 'Not exposed',
      'Load the report to see this section.'],
    zh: ['实验诊断', '数据状态', '阻断问题', '分配到实际触达', '未曝光单元',
      '读取报告后展示本区域。'],
    shared: ['watermark pending'],
    ids: ['diag-refresh', 'diag-report-card'] },
  { name: 'experiments-evidence', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-1'; S.expTab = 'evidence'; },
    en: ['Load / refresh report', 'Download CSV', 'Download JSON'],
    zh: ['加载 / 刷新报告', '下载 CSV', '下载 JSON'],
    ids: ['exp-report', 'exp-export-csv', 'exp-export-json', 'report-box'] },
  { name: 'experiments-rollout', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-1'; S.expTab = 'rollout'; },
    en: ['Staged rollout', 'Plan steps', 'Next gate evaluation:', 'Action history'],
    zh: ['分阶段放量', '计划步骤', '下次决策门评估：', '执行历史'],
    ids: ['rollout-box', 'ro-steps-tbl', 'ro-history-tbl', 'ro-refresh', 'ro-pause', 'ro-advance'] },
  { name: 'experiments-settings', view: 'experiments',
    setup: S => { S.expCreate = false; S.expId = 'exp-1'; S.expTab = 'settings'; },
    en: ['Pre-registration', 'Lifecycle', 'Transition', 'View audit records for this experiment',
      'Hypothesis: faster checkout converts better'],
    zh: ['预注册', '生命周期', '状态迁移', '查看该实验的审计记录',
      '假设：faster checkout converts better'],
    ids: ['exp-transition', 'exp-bind', 'exp-review', 'exp-bind-check', 'exp-open-audit',
      'exp-to', 'exp-reason'] },
  { name: 'tokens', view: 'tokens',
    en: ['<h2>Tokens</h2>', 'Create token', '2 token(s)', 'Revoke', 'Status'],
    zh: ['<h2>令牌</h2>', '创建令牌', '2 个令牌', '撤销', '状态'],
    ids: ['t-create', 'tok-secret'] },
  { name: 'quotas', view: 'quotas',
    en: ['Workspace quotas', 'Maximum flags', 'Event retention (nanoseconds)', 'Save quotas', '500'],
    zh: ['工作空间配额', '开关数量上限', '事件保留时间（纳秒）', '保存配额', '500'],
    ids: ['quota-form', 'quota-save', 'quota-refresh', 'quota-feedback', 'quota-maxFlags', 'quota-eventRetention'] },
  { name: 'audit', view: 'audit',
    en: ['<h2>Audit log</h2>', 'limit', 'Actor', 'Object'],
    zh: ['<h2>审计日志</h2>', '条数上限', '操作者', '对象'],
    shared: ['flag.publish', 'alice'],
    ids: ['a-limit', 'a-box'] },
  { name: 'ai', view: 'ai',
    en: ['<h2>AI resources</h2>', 'Register resource', 'Model allowlist', 'Revocations',
      'Pin version', 'Reinstate', 'Resolve viewer'],
    zh: ['<h2>AI 资源</h2>', '注册资源', '模型白名单', '撤销记录',
      '固定版本', '恢复使用', '解析查看器'],
    shared: ['checkout-helper', 'digest drift'],
    ids: ['ai-reg', 'ai-res-tbl', 'ai-rev-tbl', 'ai-allow', 'ai-resolve'] },
  { name: 'debug', view: 'debug',
    en: ['Debug evaluation', 'debug never records exposures', 'Evaluate', 'Attributes (JSON object)'],
    zh: ['调试求值', '调试求值绝不记录曝光', '求值', '属性（JSON 对象）'],
    ids: ['d-run', 'd-attrs', 'd-out'] },
];

async function runSweeps(x) {
  const out = {};
  for (const s of SWEEPS) {
    if (s.setup) s.setup(x.S);
    x.S.view = s.view; // route() always renders the view stored in S.view
    const root = viewRoot();
    await x.VIEWS[s.view](root);
    let html = root.innerHTML;
    const box = root.querySelector('#a-box'); // audit loads its table into #a-box
    if (box && box.innerHTML) html += box.innerHTML;
    const zone = root.querySelector('#exp-zone'); // experiment zones render into #exp-zone
    if (zone && zone.innerHTML) html += zone.innerHTML;
    out[s.name] = html;
  }
  return out;
}

const main = makeSandbox({ navLang: 'en-US' });
const x = main.x;
assert.strictEqual(x.I18N.lang, 'en');
x.S.ws = 'acme'; x.S.env = 'prod';
x.S.capabilities = { workspaceId: 'acme', permissions: Object.fromEntries(['workspace.admin', 'token.manage', 'flag.read', 'flag.write', 'flag.approve', 'flag.publish', 'experiment.write', 'report.read', 'ai.write', 'ai.content.read', 'quota.manage'].map(p => [p, true])) };
x.S.envs = [{ slug: 'prod', name: 'Production' }];

const reportHtml = () => { const box = {}; x.renderReport(box, report); return box.innerHTML; };
const repEn = reportHtml();
const roRunEn = x.renderRollout(expDetail);
assert(roRunEn.includes('id="ro-open-audit"') && roRunEn.includes('View full audit trail'));
const roCreateEn = x.renderRollout({ id: 'exp-1', state: 'reviewed' });
const roBlockedEn = x.renderRollout({ id: 'exp-1', state: 'running', releasePlan: { ...plan, state: 'paused', pausedReason: 'data health gate: SRM alarm', history: [] } });
const htmlEn = await runSweeps(x);
const timeEn = x.fmtTime(TS);
const lintEn = x.lintFlag({ schemaVersion: 1, key: 'x', type: 'bad', variations: {}, defaultRule: null, targeting: [] }).join('|');

// switch to zh WITHOUT reload (setLang only flips the catalog + persists here;
// the click-driven re-render path is exercised in section 3)
x.setLang('zh');
assert.strictEqual(x.I18N.lang, 'zh');
assert.strictEqual(main.store.get('golab_lang'), 'zh', 'setLang must persist to localStorage');

const repZh = reportHtml();
const roRunZh = x.renderRollout(expDetail);
assert(roRunZh.includes('id="ro-open-audit"') && roRunZh.includes('查看完整审计记录'));
const roCreateZh = x.renderRollout({ id: 'exp-1', state: 'reviewed' });
const roBlockedZh = x.renderRollout({ id: 'exp-1', state: 'running', releasePlan: { ...plan, state: 'paused', pausedReason: 'data health gate: SRM alarm', history: [] } });
const htmlZh = await runSweeps(x);
const timeZh = x.fmtTime(TS);
const lintZh = x.lintFlag({ schemaVersion: 1, key: 'x', type: 'bad', variations: {}, defaultRule: null, targeting: [] }).join('|');

for (const s of SWEEPS) {
  const idsOf = h => [...h.matchAll(/id="([^"]+)"/g)].map(mt => mt[1]).sort();
  const idList = idsOf(htmlEn[s.name]);
  for (const str of s.en) {
    assert(htmlEn[s.name].includes(str), '[en] ' + s.name + ' missing sample: ' + str);
    // translated prose must not leak into the zh render; skip dynamic
    // passthrough samples and id fragments (e.g. "limit" ⊂ "a-limit")
    if (str.length > 5 && !idList.some(id => id.includes(str))) {
      assert(!htmlZh[s.name].includes(str), '[zh] ' + s.name + ' must not render the en sample: ' + str);
    }
  }
  for (const str of s.zh) assert(htmlZh[s.name].includes(str), '[zh] ' + s.name + ' missing sample: ' + str);
  for (const str of s.shared || []) {
    assert(htmlEn[s.name].includes(str) && htmlZh[s.name].includes(str),
      'server content must pass through verbatim in both languages: ' + str);
  }
  assert.deepStrictEqual(idsOf(htmlZh[s.name]), idList,
    'layout node IDs must not change across languages: ' + s.name);
  for (const id of s.ids) assert(idList.includes(id), s.name + ' missing layout id: ' + id);
}

// the ended hero fetches the transition audit with an exact object filter
assert(main.fetchCalls.some(call => call.url.includes('/audit?') &&
  call.url.includes('object=exp-2') && call.url.includes('limit=20')),
  'the ended hero must fetch the object-filtered transition audit');

// ---------------------------------------------------------------------------
// Six-state hero copy (§4.4 状态×文案): every classified state renders its own
// localized first-screen copy in BOTH languages; server reasons, operator
// strings and metric numbers pass through verbatim.
// ---------------------------------------------------------------------------
{
  const heroExp = { id: 'exp-1', state: 'running', flagKey: 'my-flag', environmentId: 'prod', epoch: 3 };
  const srmReason = 'SRM alarm: observed [30 10] vs expected [20 20] (p=0.0016) — delivery pipeline suspect';
  const noSamplesReport = { healthy: false, healthIssues: ['no randomized exposures recorded yet'],
    minRuntimeSatisfied: false, decision: { verdict: 'hold', reasons: [] } };
  const heroCases = [
    { name: 'not_started', kind: 'not_started', e: { ...heroExp, state: 'draft' },
      report: null, diag: null, transition: null,
      en: ['No experiment results are being collected yet.',
        'Complete the readiness checks, then submit for review and launch.', 'View readiness checks'],
      zh: ['还未开始采集实验结果。', '完成准备检查后提交评审并启动。', '查看准备检查'] },
    { name: 'no_samples_assigned', kind: 'no_samples', e: heroExp, report: noSamplesReport,
      diag: { exposureMode: 'onRender',
        perVariant: [{ variant: 'control', assignments: 120, exposures: 0, unexposed: 120, conversions: 0 }] },
      transition: null,
      en: ['Waiting for the first real exposures.', '120 units assigned but none exposed yet',
        'View SDK setup and collection diagnostics'],
      zh: ['等待首批实际曝光。', '已分配 120 个单元但尚无实际曝光', '查看 SDK 接入和采集诊断'] },
    { name: 'no_samples_none', kind: 'no_samples', e: heroExp, report: noSamplesReport,
      diag: { exposureMode: 'onRender',
        perVariant: [{ variant: 'control', assignments: 0, exposures: 0, unexposed: 0, conversions: 0 }] },
      transition: null,
      en: ['No assignments recorded yet'], zh: ['尚无分配记录'] },
    { name: 'immature_eta', kind: 'immature',
      e: { ...heroExp, startedAt: '2026-10-14T00:00:00Z', minRuntime: 24 * NS_H },
      report: { healthy: true, minRuntimeSatisfied: false,
        decision: { verdict: 'hold', reasons: ['pre-registered minimum runtime not yet satisfied'] } },
      diag: null, transition: null,
      en: ['Results are not mature yet; current numbers are for observation only.',
        'Earliest decidable time: ', 'View watermark'],
      zh: ['结果未成熟，当前仅供观察。', '预计可判定时间：', '查看水位'],
      shared: ['pre-registered minimum runtime not yet satisfied'] },
    { name: 'immature_eta_unknown', kind: 'immature', e: heroExp,
      report: { healthy: true, minRuntimeSatisfied: false, decision: { verdict: 'hold', reasons: [] } },
      diag: null, transition: null,
      en: ['Start time is not recorded; the earliest decidable time is unavailable.'],
      zh: ['未记录启动时间，无法给出预计可判定时间。'] },
    { name: 'blocked', kind: 'blocked', e: heroExp,
      report: { healthy: false, healthIssues: [srmReason], minRuntimeSatisfied: true,
        decision: { verdict: 'hold', reasons: [],
          guardrailBreaches: [{ metric: 'latency_p95', detail: '+12% over cap' }] } },
      diag: null, transition: null,
      en: ['Automatic advancement is blocked.', 'Affected: flag my-flag · environment prod · epoch 3',
        'View diagnostics', 'Pause / rollback flow', 'View recent changes'],
      zh: ['自动推进已阻止。', '影响范围：Flag my-flag · 环境 prod · epoch 3',
        '查看诊断', '暂停/回滚流程', '查看最近变化'],
      shared: [srmReason, 'latency_p95', '+12% over cap'] },
    { name: 'can_advance', kind: 'can_advance', e: heroExp,
      report: { healthy: true, minRuntimeSatisfied: true,
        primary: { type: 'welch', pValue: 0.001, diff: 0.2, relativeDiff: 0.1, ciLow: 0.1, ciHigh: 0.3 },
        guardrails: [{ metric: 'latency_p95', detail: 'ok', breached: false }],
        decision: { verdict: 'ship', reasons: [] } },
      diag: null, transition: null,
      en: ['Data meets the current rollout gate.', 'This is not a claim of absolute victory', 'View evidence'],
      zh: ['数据满足当前放量门槛。', '这不宣称绝对获胜', '查看证据'],
      shared: ['p = 0.001', 'latency_p95'] },
    { name: 'ended', kind: 'ended', e: { ...heroExp, state: 'paused' }, report: null, diag: null,
      transition: { actor: 'alice (tok-1)', time: '2026-10-14T09:00:00Z', to: 'paused',
        reason: 'guardrail investigation' },
      en: ['paused: alice (tok-1) at ', 'reason: guardrail investigation', 'View audit'],
      zh: ['paused：alice (tok-1) 于 ', '原因：guardrail investigation', '查看审计'],
      shared: ['guardrail investigation', 'alice (tok-1)'] },
    { name: 'ended_unknown', kind: 'ended', e: { ...heroExp, state: 'stopped' },
      report: null, diag: null, transition: null,
      en: ['Transition details are unavailable; check the audit log.'],
      zh: ['未获取到状态变更详情，请查看审计记录。'] },
  ];
  const hs = makeSandbox({ navLang: 'en-US' });
  hs.x.S.capabilities = { workspaceId: 'acme',
    permissions: { 'report.read': true, 'flag.publish': true } };
  const classifyCase = c => hs.x.classifyExpStatus(c.e, c.report, c.diag, c.transition, { canRead: true });
  const renderCase = c => hs.x.renderExpHero(classifyCase(c), c.report, null);
  heroCases.forEach(c => {
    assert.strictEqual(classifyCase(c).kind, c.kind,
      'hero case ' + c.name + ' must classify as ' + c.kind);
  });
  const enHeroes = heroCases.map(renderCase);
  heroCases.forEach((c, i) => {
    for (const str of c.en) assert(enHeroes[i].includes(str), '[en] hero ' + c.name + ' missing: ' + str);
  });
  hs.x.setLang('zh');
  const zhHeroes = heroCases.map(renderCase);
  heroCases.forEach((c, i) => {
    for (const str of c.zh) assert(zhHeroes[i].includes(str), '[zh] hero ' + c.name + ' missing: ' + str);
    for (const str of c.shared || []) {
      assert(enHeroes[i].includes(str) && zhHeroes[i].includes(str),
        'server content must pass through verbatim in both languages: hero ' + c.name + ': ' + str);
    }
    for (const str of c.en) {
      assert(!zhHeroes[i].includes(str), '[zh] hero ' + c.name + ' must not render the en sample: ' + str);
    }
  });
}

// Quota editor uses the GET values as its complete PUT body; validation and
// permission/conflict/rate-limit feedback remain in the view.
{
  const cs = makeSandbox({ navLang: 'en-US' });
  cs.x.S.ws = 'acme';
  const root = viewRoot();
  await cs.x.VIEWS.quotas(root);
  for (const [key, value] of Object.entries(quotas)) root.querySelector('#quota-' + key).value = String(value);
  const submit = root.querySelector('#quota-form').onsubmit;
  root.querySelector('#quota-maxFlags').value = '0';
  await submit({ preventDefault() {} });
  assert(root.querySelector('#quota-feedback').textContent.includes('positive safe integer'));
  assert.strictEqual(cs.fetchCalls.length, 1, 'invalid quota must not PUT');
  root.querySelector('#quota-maxFlags').value = '501';
  cs.sandbox.confirm = () => false;
  await submit({ preventDefault() {} });
  assert.strictEqual(cs.fetchCalls.length, 1, 'cancelled quota save must not PUT');
  cs.sandbox.confirm = () => true;
  await submit({ preventDefault() {} });
  const put = cs.fetchCalls.at(-1);
  assert.strictEqual(put.opts.method, 'PUT');
  assert.strictEqual(put.url, '/api/v1/admin/workspaces/acme/quotas');
  assert.deepStrictEqual(JSON.parse(put.opts.body), { ...quotas, maxFlags: 501 });
  assert(root.querySelector('#quota-feedback').textContent.includes('saved'));
  cs.sandbox.fetch = () => Promise.resolve({ ok: true, status: 200,
    text: () => Promise.resolve(JSON.stringify({ ...quotas, maxFlags: 501 })) });
  const refreshed = viewRoot();
  await cs.x.VIEWS.quotas(refreshed);
  assert(refreshed.innerHTML.includes('name="maxFlags" type="number" min="1" step="1" required value="501"'),
    'refresh must display the persisted quota value');
  for (const [status, sample] of [[403, 'Access denied (403)'], [409, 'Conflict (409)'], [429, 'Request limited (429)']]) {
    cs.sandbox.fetch = () => Promise.resolve({ ok: false, status, statusText: 'error',
      text: () => Promise.resolve(JSON.stringify({ error: { code: 'test', message: 'test' } })) });
    await submit({ preventDefault() {} });
    assert(root.querySelector('#quota-feedback').textContent.includes(sample), `quota ${status} feedback`);
  }
}

// report fixed labels — including the "separate view / not the decision gate"
// annotations, bilingual (§10.3 separation contract).
for (const str of ['Report healthy · epoch 1 · state running', 'Sequential',
  '(always-valid · separate view, not the decision gate)', 'anytime-valid p',
  'Bayesian', '(posterior · separate from frequentist)', 'expected loss (choose B)',
  'SRM check', 'Per variant', 'Guardrails', 'Decision', '>significant<']) {
  assert(repEn.includes(str), '[en] report missing: ' + str);
}
for (const str of ['报告健康 · epoch 1 · 状态 running', '序贯',
  '（always-valid · 独立视图，非决策门）', '任意时刻有效 p',
  '贝叶斯', '（后验 · 与频率派分离）', '期望损失（选择 B）',
  'SRM 检查', '按变体', '护栏', '决策', '>显著<']) {
  assert(repZh.includes(str), '[zh] report missing: ' + str);
}
// server-returned dynamic content stays verbatim in both languages
for (const html of [repEn, repZh]) {
  for (const str of ['>SHIP<', 'primary significantly better', 'latency_p95', 'welch']) {
    assert(html.includes(str), 'dynamic server content must pass through untranslated: ' + str);
  }
}

// rollout block — running / create form / SRM-blocked banner, bilingual.
for (const str of ['Plan steps', 'Action history', 'Next gate evaluation:', 'step 2/2',
  '>engine auto<', '>plan running<', '>auto<', '>manual hold<', '>current<', '>done<',
  'Plan registered by owner-token']) {
  assert(roRunEn.includes(str), '[en] rollout running missing: ' + str);
}
for (const str of ['计划步骤', '执行历史', '下次决策门评估：', '第 2/2 步',
  '>引擎自动<', '>计划 running<', '>自动<', '>手动推进<', '>当前<', '>已完成<',
  '计划注册人 owner-token']) {
  assert(roRunZh.includes(str), '[zh] rollout running missing: ' + str);
}
for (const str of ['No release plan registered', 'Register release plan', '+ Add step']) {
  assert(roCreateEn.includes(str), '[en] rollout create missing: ' + str);
}
for (const str of ['尚未注册发布计划', '注册发布计划', '+ 添加步骤']) {
  assert(roCreateZh.includes(str), '[zh] rollout create missing: ' + str);
}
assert(roBlockedEn.includes('BLOCKED — auto-paused by the engine') && roBlockedEn.includes('never auto-resumes'), '[en] blocked banner missing');
assert(roBlockedZh.includes('已阻断 — 引擎自动暂停') && roBlockedZh.includes('绝不自动恢复'), '[zh] blocked banner missing');
// engine-block reason is server data → verbatim in both languages
assert(roBlockedEn.includes('data health gate: SRM alarm') && roBlockedZh.includes('data health gate: SRM alarm'),
  'pausedReason (server content) must pass through untranslated');

// date formatting follows the language
assert(timeEn !== '—' && timeZh !== '—', 'fmtTime must render the mock timestamp');
assert(timeEn !== timeZh, 'date format must differ between en (' + timeEn + ') and zh (' + timeZh + ')');
assert(/\d{4}\/\d{1,2}\/\d{1,2}/.test(timeZh), 'zh date must use a localized format: ' + timeZh);
// client-side lint messages are localized; the DSL keywords stay verbatim
assert(lintEn.includes('type must be bool|string|number|json'), '[en] lint message missing');
assert(lintZh.includes('type 必须是 bool|string|number|json'), '[zh] lint message missing');

// ===========================================================================
// 3. Language switch — click simulation: persisted, active-class flip and an
//    in-place re-render of the current view via route() (no page reload).
// ===========================================================================
{
  const cs = makeSandbox({ navLang: 'en-US' });
  cs.x.registerI18nHooks();
  cs.x.bindLangSwitch();
  const zhBtn = cs.chrome.get('#lang-zh'), enBtn = cs.chrome.get('#lang-en');
  assert(zhBtn && enBtn, 'top-bar language buttons must exist');
  assert.strictEqual(typeof zhBtn.onclick, 'function', '#lang-zh click handler not bound');
  assert.strictEqual(typeof enBtn.onclick, 'function', '#lang-en click handler not bound');
  assert(enBtn._toggles.some(([c, on]) => c === 'active' && on === true), 'EN must start active');

  // booted app on the flags view with no scope → route() renders the warn banner
  cs.x.S.booted = true; cs.x.S.view = 'flags'; cs.x.S.ws = ''; cs.x.S.env = '';
  zhBtn.onclick();
  await tick();
  const mainEl = cs.chrome.get('#main'); // created by route() during the switch
  assert(mainEl, 'route() must render into #main on language switch');
  assert.strictEqual(cs.x.I18N.lang, 'zh', 'click on 中 must switch the language');
  assert.strictEqual(cs.store.get('golab_lang'), 'zh', 'switch must persist to localStorage');
  assert(zhBtn._toggles.some(([c, on]) => c === 'active' && on === true), '中 must become active');
  assert(mainEl.innerHTML.includes('请在顶栏选择工作空间与环境'),
    'switch must re-render the current view in place (zh banner), got: ' + mainEl.innerHTML);
  assert(!mainEl.innerHTML.includes('Select a workspace'), 'en text must be gone after the switch');

  enBtn.onclick();
  await tick();
  assert.strictEqual(cs.x.I18N.lang, 'en', 'click on EN must switch back');
  assert.strictEqual(cs.store.get('golab_lang'), 'en', 'switch back must persist');
  assert(mainEl.innerHTML.includes('Select a workspace and environment'),
    'switch back must re-render the current view in en');
}

// ===========================================================================
// 4. Dictionary completeness — zero t() misses across every render above, and
//    catalog/key cross-checks.
// ===========================================================================
assert.deepStrictEqual([...x.I18N.missing], [], 't() missing keys must be zero, got: ' + JSON.stringify([...x.I18N.missing]));
assert(!main.warns.some(w => w.includes('[i18n] missing')), 'console.warn recorded a catalog gap');
{
  const en = Object.keys(x.I18N.catalogs.en).sort();
  const zh = Object.keys(x.I18N.catalogs.zh).sort();
  assert.deepStrictEqual(zh, en, 'en/zh catalogs must have identical key sets');
  // every literal t('key') call in app.js exists in both catalogs
  const used = new Set();
  for (const mt of appCode.matchAll(/\bt\('([A-Za-z0-9._-]+)'/g)) used.add(mt[1]);
  for (const mt of flagEditorCode.matchAll(/\bt\('([A-Za-z0-9._-]+)'/g)) used.add(mt[1]);
  for (const mt of segmentEditorCode.matchAll(/\bt\('([A-Za-z0-9._-]+)'/g)) used.add(mt[1]);
  for (const mt of approvalsCode.matchAll(/\bt\('([A-Za-z0-9._-]+)'/g)) used.add(mt[1]);
  for (const mt of expWorkbenchCode.matchAll(/\bt\('([A-Za-z0-9._-]+)'/g)) used.add(mt[1]);
  assert(used.size > 100, 'static extraction should find the migrated keys, got ' + used.size);
  for (const k of used) {
    assert(k in x.I18N.catalogs.en, 'app.js uses key missing from en catalog: ' + k);
    assert(k in x.I18N.catalogs.zh, 'app.js uses key missing from zh catalog: ' + k);
  }
  // every data-i18n* key in index.html exists in both catalogs
  let htmlKeys = 0;
  for (const mt of indexHTML.matchAll(/data-i18n(?:-ph|-title)?="([^"]+)"/g)) {
    htmlKeys++;
    assert(mt[1] in x.I18N.catalogs.en, 'index.html uses key missing from en catalog: ' + mt[1]);
    assert(mt[1] in x.I18N.catalogs.zh, 'index.html uses key missing from zh catalog: ' + mt[1]);
  }
  assert(htmlKeys >= 20, 'index.html should carry the migrated static labels, got ' + htmlKeys);
}

// ===========================================================================
// 5. Static scan — no CJK literals outside the catalogs. Comments are the
//    whitelisted exception (stripped before scanning); app.js has no '//'
//    inside string literals, so line-based stripping is exact. The catalogs
//    live in i18n.js, which is intentionally NOT scanned.
// ===========================================================================
{
  const CJK_RE = /[　-〿㐀-䶿一-鿿豈-﫿︰-﹏＀-￯]/;
  const stripJsComments = src => src
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n').map(l => { const i = l.indexOf('//'); return i < 0 ? l : l.slice(0, i); }).join('\n');
  const scan = (name, src) => {
    const mt = src.match(CJK_RE);
    if (mt) {
      assert.fail(name + ' contains a CJK literal outside the catalogs: ' + JSON.stringify(mt[0]) +
        ' near: ' + src.slice(Math.max(0, mt.index - 60), mt.index + 40).replace(/\n/g, ' '));
    }
  };
  scan('app.js (code + string literals)', stripJsComments(appCode));
  scan('flag_editor.js (code + string literals)', stripJsComments(flagEditorCode));
  scan('segment_editor.js (code + string literals)', stripJsComments(segmentEditorCode));
  scan('approvals.js (code + string literals)', stripJsComments(approvalsCode));
  scan('exp_workbench.js (code + string literals)', stripJsComments(expWorkbenchCode));
  scan('index.html (markup + attributes)', indexHTML.replace(/<!--[\s\S]*?-->/g, ''));
}

// An older response must not replace the current view after navigation.
{
  const cs = makeSandbox();
  let finishSlow;
  cs.x.VIEWS.flags = async m => {
    await new Promise(resolve => { finishSlow = resolve; });
    m.innerHTML = '<p>stale response</p>';
  };
  cs.x.VIEWS.audit = async m => { m.innerHTML = '<p>current response</p>'; };
  cs.x.S.view = 'flags';
  const slow = cs.x.route();
  cs.x.S.view = 'audit';
  await cs.x.route();
  finishSlow();
  await slow;
  assert(cs.chrome.get('#main').innerHTML.includes('current response'), 'stale response replaced current route');
}

console.log('i18n DOM contract: all assertions passed ' +
  '(7 detection orders + ' + SWEEPS.length + ' views × en/zh + report/rollout bilingual + ' +
  'switch click simulation + ' + Object.keys(x.I18N.catalogs.en).length + ' catalog keys + CJK static scan)');
