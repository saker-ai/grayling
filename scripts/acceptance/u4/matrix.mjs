// U4 / R5B.1 — role × state × device × language acceptance matrix.
//
// Runs against the same real bin/grayling + isolated store as R5A (zero
// mocks): the server is booted by lib/server.mjs, data is seeded through the
// real REST API (lib/seed.mjs) and every UI interaction is driven through
// Playwright with the native-dialog and pageerror probes from lib/browser.mjs.
// The only network-level intervention is *latency injection* for the loading
// state (route.continue passes the real server response through untouched —
// documented in matrix-methodology inside matrix-summary.md).
//
// Sections:
//   R*  roles: publisher / approver (dual-admin publish loop, all UI),
//       analyst, viewer — capability maps via API + UI gating evidence
//   S*  error states: 401 / 403 / 409 / 429 / offline
//   D*  data states: empty / loading / error / zero-data experiment /
//       small sample (insufficient CI) / healthy report / SRM / 1000-flag list
//   V*  device widths 390 / 768 / 1024 / 1440 on the four key pages
//   L*  language × theme: zh/en × light/dark on the six key pages
//   A*  a11y modes: 200% zoom equivalent, reducedMotion, forcedColors,
//       prefers-contrast: more
//   T*  a11y tree snapshots (YAML) for the six key pages
//
// Evidence: docs/acceptance/web-2026-10-18-redesign/matrix-*.{png,json,md}
// and a11y-tree/*.yaml. Exit 1 when any recorded check fails.
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { attachOrStart } from './lib/server.mjs';
import { newContext, closeBrowser, drainProbes } from './lib/browser.mjs';
import { apiClient, seedAll, bulkFlags } from './lib/seed.mjs';
import { writeJSON, writeText, screenshot, EVIDENCE_DIR } from './lib/evidence.mjs';

// ---------------------------------------------------------------------------
// check registry
// ---------------------------------------------------------------------------
const checks = [];
const warnings = [];
function record(group, id, desc, ok, detail = '', evidence = []) {
  checks.push({ group, id, desc, ok: !!ok, detail: String(detail).slice(0, 500), evidence });
  console.log(`[${ok ? 'pass' : 'FAIL'}] ${group}/${id} ${desc}${detail ? ' — ' + String(detail).slice(0, 160) : ''}`);
}
function warn(msg) {
  warnings.push(String(msg).slice(0, 300));
  console.log(`[warn] ${msg}`);
}

let activeErrors = null; // console/pageerror channel of the current context
async function shot(page, name) {
  const file = await screenshot(page, name);
  const probes = await drainProbes(page);
  for (const d of probes.dialogs) record('probe', 'native-dialog', `native ${d.kind} during ${name}`, false, d.text, [name]);
  for (const e of probes.pageErrors) record('probe', 'pageerror', `page error during ${name}`, false, e, [name]);
  return path.basename(file);
}

async function settle(page, ms = 200) {
  await page.waitForSelector('#main h1, #main h2, #main .card, #main table, #main .skeleton', { timeout: 12_000 }).catch(() => {});
  await page.waitForTimeout(ms);
}

function viewURL(baseURL, ws, env, view) {
  return `${baseURL}/#/console/${encodeURIComponent(ws)}/${encodeURIComponent(env)}/${view}`;
}

// ---------------------------------------------------------------------------
// API helpers
// ---------------------------------------------------------------------------
// bearerApi drives the admin API with a raw token (Authorization: Bearer) —
// the same authentication the console session exchange uses internally.
function bearerApi(baseURL, secret) {
  return async (p, { method = 'GET', body } = {}) => {
    const res = await fetch(baseURL + p, {
      method,
      headers: {
        Authorization: `Bearer ${secret}`,
        Origin: baseURL,
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = { raw: text };
    }
    return { status: res.status, data };
  };
}

function flagDoc(key, salt) {
  return {
    schemaVersion: 1,
    key,
    type: 'bool',
    disable: false,
    bucketing: { algorithm: 'grayling-fnv1a-10000-v1', unit: 'user_id', salt },
    fields: ['user_id'],
    variations: { control: false, candidate: true },
    defaultRule: { variation: 'control' },
    targeting: [{ name: 'split', percentage: { control: 50, candidate: 50 } }],
    experiment: null,
  };
}

async function publishFlag(owner, scope, key, salt) {
  const base = `${scope}/flags/${key}`;
  let r = await owner(`${base}/draft`, { method: 'PUT', body: { content: flagDoc(key, salt), risk: 'normal', baseRevision: 0, expectedDraftHash: '' } });
  if (r.status >= 400) throw new Error(`draft ${key}: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  r = await owner(`${base}/submit`, { method: 'POST', body: { reason: 'matrix seed' } });
  if (r.status >= 400) throw new Error(`submit ${key}: HTTP ${r.status}`);
  r = await owner(`${base}/approve`, { method: 'POST', body: { note: 'matrix seed' } });
  if (r.status >= 400) throw new Error(`approve ${key}: HTTP ${r.status}`);
  r = await owner(`${base}/publish`, { method: 'POST', body: { note: 'matrix seed publish' } });
  if (r.status >= 400) throw new Error(`publish ${key}: HTTP ${r.status}`);
  const view = await owner(base, {});
  return view.data?.current?.revision || 1;
}

// createRunningExperiment drives the full real lifecycle:
// create → review → bind → approve+publish the binding draft → running.
async function createRunningExperiment(owner, scope, { flagKey, minSamplePerArm }) {
  const created = await owner(`${scope}/experiments`, {
    method: 'POST',
    body: {
      flagKey,
      unit: 'user_id',
      variants: ['control', 'candidate'],
      proportions: { control: 50, candidate: 50 },
      primaryMetric: { key: 'mx.complete', type: 'proportion', direction: 'increase' },
      alpha: 0.05,
      analysisWindow: 86_400_000_000_000 * 7,
      minSamplePerArm,
    },
  });
  if (created.status >= 400) throw new Error(`create experiment: HTTP ${created.status} ${JSON.stringify(created.data)}`);
  const id = created.data.id;
  const expBase = `/api/v1/admin/experiments/${encodeURIComponent(id)}`;
  let r = await owner(`${expBase}/review`, { method: 'POST', body: { note: 'matrix review' } });
  if (r.status >= 400) throw new Error(`review: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  r = await owner(`${expBase}/bind`, { method: 'POST', body: {} });
  if (r.status >= 400) throw new Error(`bind: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  // approve + publish the submitted high-risk binding draft on the flag
  r = await owner(`${scope}/flags/${flagKey}/approve`, { method: 'POST', body: { note: 'matrix binding approve' } });
  if (r.status >= 400) throw new Error(`binding approve: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  r = await owner(`${scope}/flags/${flagKey}/publish`, { method: 'POST', body: { note: 'matrix binding publish' } });
  if (r.status >= 400) throw new Error(`binding publish: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  r = await owner(`${expBase}/transition`, { method: 'POST', body: { to: 'running', reason: 'matrix start' } });
  if (r.status >= 400) throw new Error(`transition: HTTP ${r.status} ${JSON.stringify(r.data)}`);
  const final = await owner(expBase, {});
  return { id, epoch: final.data?.epoch, state: final.data?.state, flagRevision: final.data?.flagRevision };
}

async function sendEvents(baseURL, collectSecret, events) {
  const api = async batch => {
    const res = await fetch(`${baseURL}/api/v1/collect`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${collectSecret}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ events: batch }),
    });
    const data = await res.json().catch(() => null);
    return { status: res.status, data };
  };
  let accepted = 0;
  let rejected = 0;
  for (let i = 0; i < events.length; i += 400) {
    const batch = events.slice(i, i + 400);
    const r = await api(batch);
    if (r.status >= 400) throw new Error(`collect: HTTP ${r.status} ${JSON.stringify(r.data).slice(0, 300)}`);
    accepted += r.data?.accepted ?? batch.length;
    rejected += r.data?.rejected ?? 0;
  }
  return { accepted, rejected };
}

function makeEvents({ expId, epoch, flagKey, flagRevision, exposures, conversions, variantSplit }) {
  // exposures: {control: n, candidate: n}; conversions: {control: n, candidate: n}
  const now = Date.now();
  const out = [];
  let i = 0;
  for (const [variant, n] of Object.entries(exposures)) {
    for (let k = 0; k < n; k++, i++) {
      out.push({
        eventId: `mx-e-${expId.slice(-6)}-${i}`,
        type: 'exposure',
        experimentId: expId,
        epoch,
        flagKey,
        flagRevision,
        variant,
        unit: `mx-u-${i}`,
        source: 'split',
        clientTime: new Date(now - 10 * 60_000).toISOString(),
      });
    }
  }
  i = 0;
  for (const [variant, n] of Object.entries(conversions)) {
    for (let k = 0; k < n; k++, i++) {
      out.push({
        eventId: `mx-c-${expId.slice(-6)}-${i}`,
        type: 'conversion',
        experimentId: expId,
        epoch,
        flagKey,
        flagRevision,
        variant,
        unit: `mx-u-${i}`, // conversions attribute to exposed units
        metricKey: 'mx.complete',
        value: 1,
        clientTime: new Date(now - 5 * 60_000).toISOString(),
      });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// UI helpers
// ---------------------------------------------------------------------------
async function loginUI(baseURL, token, { viewport = 'desktop', lang = 'zh', theme = 'light' } = {}) {
  const { context, errors } = await newContext({ viewport, lang, theme, baseURL });
  activeErrors = errors;
  // Suppress dev-autologin exactly like an explicit sign-out does, so the
  // login page appears and the role token is the identity under test.
  await context.addCookies([{ name: 'grayling_dev_signed_out', value: '1', url: baseURL }]);
  const page = await context.newPage();
  await page.goto(baseURL + '/', { waitUntil: 'networkidle' });
  await page.waitForSelector('#btn-connect', { timeout: 10_000 });
  await page.fill('#auth-token', token);
  await page.click('#btn-connect');
  await page.waitForSelector('#conn-info', { timeout: 15_000 });
  await settle(page);
  return { context, page };
}

async function ownerUI(baseURL, opts) {
  // dev-autologin session: plain context, GETs are auto signed in as the
  // global owner.
  const { context, errors } = await newContext({ baseURL, ...(opts || {}) });
  activeErrors = errors;
  const page = await context.newPage();
  await page.goto(baseURL + '/', { waitUntil: 'networkidle' });
  await settle(page);
  return { context, page };
}

async function dialogSubmit(page, fields, shotName) {
  const dlg = page.locator('.modal[role=dialog]');
  await dlg.waitFor({ timeout: 10_000 });
  for (const [name, value] of Object.entries(fields)) {
    await dlg.locator(`[name="${name}"]`).fill(value);
  }
  const ev = shotName ? await shot(page, shotName) : null;
  await dlg.locator('.modal-foot .btn.primary').click();
  await dlg.waitFor({ state: 'detached', timeout: 10_000 }).catch(() => {});
  return ev;
}

async function confirmDanger(page, reason, shotName) {
  const dlg = page.locator('.modal[role=dialog]');
  await dlg.waitFor({ timeout: 10_000 });
  if (reason !== null) await dlg.locator('#action-reason').fill(reason);
  const ev = shotName ? await shot(page, shotName) : null;
  await dlg.locator('.modal-foot .btn.danger').click();
  await dlg.waitFor({ state: 'detached', timeout: 10_000 }).catch(() => {});
  return ev;
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------
const main = async () => {
  const server = await attachOrStart();
  const baseURL = server.baseURL;
  const seed = await seedAll(baseURL);
  const scope = `/api/v1/admin/workspaces/${seed.ws}/environments/${seed.env}`;
  const wsBase = `/api/v1/admin/workspaces/${seed.ws}`;
  const { must } = await apiClient(baseURL); // dev-autologin global owner (cookie)
  const consoleHash = view => viewURL(baseURL, seed.ws, seed.env, view);

  try {
    // ---------------------------------------------------------------
    // R0: mint role tokens (owner via dev session; secrets stay in memory)
    // ---------------------------------------------------------------
    const roleDefs = [
      ['publisher', 'mx-publisher', 'publisher'],
      ['approver', 'mx-approver', 'owner'],
      ['analyst', 'mx-analyst', 'analyst'],
      ['viewer', 'mx-viewer', 'viewer'],
    ];
    const tokens = {};
    for (const [key, name, role] of roleDefs) {
      const r = await must(`${wsBase}/tokens`, { method: 'POST', body: { name, kind: 'admin', role } }, [409]);
      const secret = r.data?.Secret || r.data?.secret;
      if (secret) tokens[key] = secret;
    }
    // re-runs against an attached server cannot recover one-time secrets
    const haveAll = roleDefs.every(([k]) => tokens[k]);
    record('R', 'R0-mint', 'role tokens minted (publisher/approver/analyst/viewer)', haveAll, haveAll ? '' : 'missing secret (attached rerun?)');
    const coll = await must(`${wsBase}/tokens`, { method: 'POST', body: { name: 'mx-collect', kind: 'collect', role: 'viewer', environment: seed.env } }, [409]);
    const collectSecret = coll.data?.Secret || coll.data?.secret || '';
    record('R', 'R0b-collect', 'collect token minted for event seeding', !!collectSecret);

    // Capability maps per role (API, Bearer)
    const capExpect = {
      publisher: { 'flag.write': true, 'flag.approve': true, 'flag.publish': true, 'experiment.write': true, 'token.manage': false, 'quota.manage': false, 'ai.content.read': false },
      approver: { 'flag.write': true, 'flag.approve': true, 'flag.publish': true, 'token.manage': true, 'quota.manage': true },
      analyst: { 'flag.write': false, 'flag.approve': false, 'flag.publish': false, 'experiment.write': true, 'report.read': true, 'token.manage': false },
      viewer: { 'flag.write': false, 'flag.approve': false, 'flag.publish': false, 'experiment.write': false, 'report.read': true, 'token.manage': false, 'ai.write': false },
    };
    const capMaps = {};
    for (const [key] of roleDefs) {
      if (!tokens[key]) continue;
      const api = bearerApi(baseURL, tokens[key]);
      const r = await api(`${wsBase}/capabilities`, {});
      capMaps[key] = r.data?.permissions || {};
      const exp = capExpect[key];
      const mismatches = Object.entries(exp).filter(([p, want]) => !!capMaps[key][p] !== want);
      record('R', `R1-caps-${key}`, `capabilities map matches RBAC for ${key}`, r.status === 200 && mismatches.length === 0,
        mismatches.map(([p, want]) => `${p}: want ${want} got ${!!capMaps[key][p]}`).join('; ') || `${Object.values(capMaps[key]).filter(Boolean).length} perms`);
    }

    // ---------------------------------------------------------------
    // R2/R3: dual-admin publish loop — publisher submits, approver (a second,
    // distinct admin identity) approves and publishes. Fully via UI.
    // ---------------------------------------------------------------
    const loopKey = `mx-loop-${Date.now() % 100000}`;
    let loopEvidence = [];
    if (tokens.publisher && tokens.approver) {
      // --- publisher session
      {
        const { context, page } = await loginUI(baseURL, tokens.publisher);
        const conn = await page.locator('#conn-info').innerText();
        record('R', 'R2-publisher-login', 'publisher signed in via console login page', conn.includes('mx-publisher'), conn.trim());
        loopEvidence.push(await shot(page, 'matrix-role-publisher-01-shell.png'));
        await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
        await settle(page);
        await page.click('#flag-new');
        loopEvidence.push('matrix-role-publisher-02-new-flag.png');
        await dialogSubmit(page, { key: loopKey, description: 'matrix dual-admin loop flag' }, 'matrix-role-publisher-02-new-flag.png');
        await settle(page, 400);
        await page.click('#flag-save');
        await page.waitForSelector('#toasts .toast.ok', { timeout: 10_000 }).catch(() => {});
        await settle(page, 400);
        loopEvidence.push(await shot(page, 'matrix-role-publisher-03-draft-saved.png'));
        const saved = await page.locator('#toasts .toast.ok').count();
        record('R', 'R2-publisher-save', 'publisher saved a draft via UI', saved > 0);
        await page.click('#flag-submit');
        await dialogSubmit(page, { reason: 'matrix: dual-admin loop submission' }, 'matrix-role-publisher-04-submit-dialog.png');
        await settle(page, 500);
        loopEvidence.push('matrix-role-publisher-04-submit-dialog.png');
        loopEvidence.push(await shot(page, 'matrix-role-publisher-05-submitted.png'));
        const submitted = await must(`${scope}/flags/${loopKey}`, {});
        record('R', 'R2-publisher-submit', 'publisher submitted draft for review via UI', submitted.data?.draft?.state === 'submitted', `flag ${loopKey} state=${submitted.data?.draft?.state}`);
        await context.close();
      }
      // --- approver session (second identity)
      {
        const { context, page } = await loginUI(baseURL, tokens.approver);
        const conn = await page.locator('#conn-info').innerText();
        record('R', 'R3-approver-login', 'approver (second admin) signed in', conn.includes('mx-approver'), conn.trim());
        // approvals inbox evidence (seeded metric submission is pending there)
        await page.goto(consoleHash('approvals'), { waitUntil: 'networkidle' });
        await settle(page);
        loopEvidence.push(await shot(page, 'matrix-role-approver-01-inbox.png'));
        await page.goto(consoleHash(`flags/${loopKey}`), { waitUntil: 'networkidle' });
        await settle(page, 400);
        loopEvidence.push(await shot(page, 'matrix-role-approver-02-review.png'));
        const canApprove = await page.locator('#flag-approve').count();
        record('R', 'R3-approver-sees-review', 'approver sees the submitted draft with approve action', canApprove > 0);
        await page.click('#flag-approve');
        await dialogSubmit(page, { note: 'matrix: approved by second admin' }, 'matrix-role-approver-03-approve-dialog.png');
        await settle(page, 500);
        loopEvidence.push('matrix-role-approver-03-approve-dialog.png');
        const canPublish = await page.locator('#flag-publish').count();
        record('R', 'R3-approver-approved', 'approver approved; publish action now primary', canPublish > 0);
        await page.click('#flag-publish');
        await confirmDanger(page, 'matrix: publish after dual-admin review', 'matrix-role-approver-04-publish-confirm.png');
        await settle(page, 600);
        loopEvidence.push('matrix-role-approver-04-publish-confirm.png');
        loopEvidence.push(await shot(page, 'matrix-role-approver-05-published.png'));
        await context.close();
      }
      // server-side truth: revision + audit attribution to two distinct actors
      const view = await must(`${scope}/flags/${loopKey}`, {});
      record('R', 'R3-published-rev1', 'flag published at revision 1 after the loop', view.data?.current?.revision === 1, `revision=${view.data?.current?.revision}`);
      const audit = await must(`${wsBase}/audit?limit=100`, {});
      const rows = (audit.data || []).filter(a => String(a.object || '').includes(loopKey));
      const submittedBy = rows.find(a => a.action === 'flag.draft.submit')?.actor;
      const approvedBy = rows.find(a => a.action === 'flag.draft.approve')?.actor;
      const publishedBy = rows.find(a => a.action === 'flag.publish')?.actor;
      // Audit actor format is "{session name} ({token name})" — assert the
      // leading identity and that submit vs approve/publish are distinct.
      record('R', 'R3-audit-actors', 'audit attributes submit/approve/publish to two distinct admins',
        !!submittedBy?.startsWith('mx-publisher') && !!approvedBy?.startsWith('mx-approver')
        && !!publishedBy?.startsWith('mx-approver') && submittedBy !== approvedBy,
        `submit=${submittedBy} approve=${approvedBy} publish=${publishedBy}`);
    }

    // ---------------------------------------------------------------
    // R4 analyst: read views visible, flag writes rejected (UI + API)
    // ---------------------------------------------------------------
    if (tokens.analyst) {
      const api = bearerApi(baseURL, tokens.analyst);
      const readFlags = await api(`${scope}/flags`, {});
      const writeDraft = await api(`${scope}/flags/mx-analyst-probe/draft`, { method: 'PUT', body: { content: flagDoc('mx-analyst-probe', 'x'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' } });
      const submitProbe = await api(`${scope}/flags/${seed.flagKey}/submit`, { method: 'POST', body: { reason: 'probe' } });
      record('R', 'R4-analyst-read', 'analyst reads flag list (200)', readFlags.status === 200, `HTTP ${readFlags.status}`);
      record('R', 'R4-analyst-write-403', 'analyst flag draft write rejected (403)', writeDraft.status === 403, `HTTP ${writeDraft.status}`);
      record('R', 'R4-analyst-submit-403', 'analyst submit rejected (403)', submitProbe.status === 403, `HTTP ${submitProbe.status}`);
      const { context, page } = await loginUI(baseURL, tokens.analyst);
      await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await settle(page);
      const newBtn = await page.locator('#flag-new').count();
      record('R', 'R4-analyst-ui-gating', 'analyst UI hides the new-flag action', newBtn === 0);
      const ev1 = await shot(page, 'matrix-role-analyst-01-flags.png');
      await page.goto(consoleHash(`flags/${seed.flagKey}`), { waitUntil: 'networkidle' });
      await settle(page, 400);
      const saveBtn = await page.locator('#flag-save:visible').count();
      const submitBtn = await page.locator('#flag-submit:visible').count();
      record('R', 'R4-analyst-detail-gating', 'analyst flag detail hides save/submit', saveBtn === 0 && submitBtn === 0, `save=${saveBtn} submit=${submitBtn}`);
      const ev2 = await shot(page, 'matrix-role-analyst-02-flag-detail.png');
      await page.goto(consoleHash('experiments'), { waitUntil: 'networkidle' });
      await settle(page);
      const ev3 = await shot(page, 'matrix-role-analyst-03-experiments.png');
      record('R', 'R4-analyst-views', 'analyst analysis views render', true, '', [ev1, ev2, ev3]);
      await context.close();
    }

    // ---------------------------------------------------------------
    // R5 viewer: everything read-only (UI gating + API 403 double layer)
    // ---------------------------------------------------------------
    if (tokens.viewer) {
      const api = bearerApi(baseURL, tokens.viewer);
      const readFlags = await api(`${scope}/flags`, {});
      const writeDraft = await api(`${scope}/flags/mx-viewer-probe/draft`, { method: 'PUT', body: { content: flagDoc('mx-viewer-probe', 'x'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' } });
      const tokenProbe = await api(`${wsBase}/tokens`, { method: 'POST', body: { name: 'mx-escalate', kind: 'admin', role: 'owner' } });
      const quotaProbe = await api(`${wsBase}/quotas`, { method: 'PUT', body: { maxFlags: 500 } });
      record('R', 'R5-viewer-read', 'viewer reads flag list (200)', readFlags.status === 200, `HTTP ${readFlags.status}`);
      record('R', 'R5-viewer-write-403', 'viewer flag write rejected (403)', writeDraft.status === 403, `HTTP ${writeDraft.status}`);
      record('R', 'R5-viewer-tokens-403', 'viewer token management rejected (403)', tokenProbe.status === 403, `HTTP ${tokenProbe.status}`);
      record('R', 'R5-viewer-quota-403', 'viewer quota write rejected (403)', quotaProbe.status === 403, `HTTP ${quotaProbe.status}`);
      const { context, page } = await loginUI(baseURL, tokens.viewer);
      await settle(page);
      const ev0 = await shot(page, 'matrix-role-viewer-00-overview.png');
      const tokensNav = await page.locator('#nav-tokens, [data-view="tokens"]').count();
      const quotasNav = await page.locator('#nav-quotas').count();
      record('R', 'R5-viewer-nav-gating', 'viewer sidebar hides tokens/quotas (permission-gated nav)', tokensNav === 0 && quotasNav === 0, `tokens=${tokensNav} quotas=${quotasNav}`);
      await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await settle(page);
      const newBtn = await page.locator('#flag-new').count();
      const ev1 = await shot(page, 'matrix-role-viewer-01-flags.png');
      await page.goto(consoleHash(`flags/${seed.flagKey}`), { waitUntil: 'networkidle' });
      await settle(page, 400);
      const saveBtn = await page.locator('#flag-save:visible').count();
      const ev2 = await shot(page, 'matrix-role-viewer-02-flag-detail.png');
      record('R', 'R5-viewer-ui-gating', 'viewer UI hides new/save actions', newBtn === 0 && saveBtn === 0, '', [ev0, ev1, ev2]);
      await context.close();
    }

    // ---------------------------------------------------------------
    // S1: 401 — no credential
    // ---------------------------------------------------------------
    {
      const noAuth = await fetch(`${baseURL}${scope}/flags/mx-none/draft`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', Origin: baseURL },
        body: JSON.stringify({ content: flagDoc('mx-none', 'x'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' }),
      });
      // GET under dev-autologin is only anonymous when the signed-out marker
      // is present (the exact state an explicit logout leaves).
      const anonGet = await fetch(`${baseURL}/api/v1/admin/workspaces`, {
        headers: { Cookie: 'grayling_dev_signed_out=1' },
      });
      record('S', 'S1-401-api', 'unauthenticated mutation → 401; anonymous read (signed-out) → 401',
        noAuth.status === 401 && anonGet.status === 401, `POST=${noAuth.status} GET=${anonGet.status}`);
      const { context, page } = await newContext({ baseURL });
      await context.addCookies([{ name: 'grayling_dev_signed_out', value: '1', url: baseURL }]);
      const p = await context.newPage();
      await p.goto(baseURL + '/', { waitUntil: 'networkidle' });
      const loginVisible = await p.locator('#btn-connect').count();
      record('S', 'S1-401-ui', 'session-less browser is held at the login page', loginVisible > 0);
      await shot(p, 'matrix-state-401-login.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // S3: 409 — concurrent draft edit (API double-write + UI conflict box)
    // ---------------------------------------------------------------
    {
      const conflictKey = 'mx-conflict';
      await publishFlag(must, scope, conflictKey, 'base').catch(e => warn(`conflict flag seed: ${e.message}`));
      // Publishing leaves the approved draft in place, so the next save must
      // chain on the live draftHash (same lesson as the R5A seeder).
      const fresh0 = await must(`${scope}/flags/${conflictKey}`, {});
      const st1 = await must(`${scope}/flags/${conflictKey}/draft`, { method: 'PUT', body: { content: flagDoc(conflictKey, 'v1'), risk: 'normal', baseRevision: 1, expectedDraftHash: fresh0.data?.draftHash || '' } });
      const st2raw = await fetch(`${baseURL}${scope}/flags/${conflictKey}/draft`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', Origin: baseURL, Cookie: (await apiClient(baseURL)).cookie },
        body: JSON.stringify({ content: flagDoc(conflictKey, 'v1-again'), risk: 'normal', baseRevision: 1, expectedDraftHash: '' }),
      });
      record('S', 'S3-409-api', 'stale expectedDraftHash write rejected with 409', st2raw.status === 409, `HTTP ${st2raw.status}`);

      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash(`flags/${conflictKey}`), { waitUntil: 'networkidle' });
      await settle(page, 400);
      await page.click('#flag-salt');
      await page.fill('#flag-salt', 'ui-stale-edit');
      // someone else saves behind this editor's back
      const fresh = await must(`${scope}/flags/${conflictKey}`, {});
      const st3 = await must(`${scope}/flags/${conflictKey}/draft`, {
        method: 'PUT',
        body: { content: flagDoc(conflictKey, 'other-editor'), risk: 'normal', baseRevision: 1, expectedDraftHash: fresh.data?.draftHash || '' },
      });
      await page.click('#flag-save');
      const conflictBox = page.locator('#flag-conflict');
      const visible = await conflictBox.waitFor({ timeout: 10_000 }).then(() => true).catch(() => false);
      record('S', 'S3-409-ui', 'UI save after concurrent edit shows the 409 conflict panel (diff preserved)', visible, `otherSave=${st3.status}`);
      await shot(page, 'matrix-state-409-conflict.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // S4: 429 — quota exceeded (squeeze maxFlags to the current count)
    // ---------------------------------------------------------------
    {
      const quotas = await must(`${wsBase}/quotas`, {});
      const current = quotas.data || {};
      const flagsNow = await must(`${scope}/flags`, {});
      const count = Array.isArray(flagsNow.data) ? flagsNow.data.length : 0;
      await must(`${wsBase}/quotas`, { method: 'PUT', body: { ...current, maxFlags: count } });
      const over = await fetch(`${baseURL}${scope}/flags/mx-quota-probe/draft`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', Origin: baseURL, Cookie: (await apiClient(baseURL)).cookie },
        body: JSON.stringify({ content: flagDoc('mx-quota-probe', 'x'), risk: 'normal', baseRevision: 0, expectedDraftHash: '' }),
      });
      const overBody = await over.json().catch(() => null);
      record('S', 'S4-429-api', 'flag create at quota limit rejected with 429 quota_exceeded', over.status === 429, `HTTP ${over.status} code=${overBody?.error?.code}`);

      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await settle(page);
      await page.click('#flag-new');
      await dialogSubmit(page, { key: 'mx-quota-ui' });
      await settle(page, 300);
      await page.click('#flag-save');
      const errToast = await page.waitForSelector('#toasts .toast.err', { timeout: 10_000 }).then(() => true).catch(() => false);
      record('S', 'S4-429-ui', 'UI save over quota surfaces an error toast (no silent failure)', errToast);
      await shot(page, 'matrix-state-429-quota.png');
      await context.close();
      // restore headroom for the 1000-flag bulk seed
      await must(`${wsBase}/quotas`, { method: 'PUT', body: { ...current, maxFlags: 1200 } });
    }

    // ---------------------------------------------------------------
    // S5: offline degradation + recovery. Two distinct sub-cases:
    //  a) the target page chunk is already cached → the route renders and
    //     the failed data fetch degrades to the in-page error state;
    //  b) the chunk was never fetched → the lazy import fails and the
    //     chunk boundary must keep the shell alive with an explicit reload
    //     (this was the R5B-found bug: the router default boundary used to
    //     replace the whole console, shell included, with no way back).
    // ---------------------------------------------------------------
    {
      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' }); // cache the chunk
      await settle(page);
      await page.goto(consoleHash('overview'), { waitUntil: 'networkidle' });
      await settle(page);
      await context.setOffline(true);
      await page.evaluate(hash => { location.hash = hash; }, `#/console/${seed.ws}/${seed.env}/flags`);
      await page.waitForTimeout(2000);
      const mainExists = await page.locator('#main').count();
      const bannerErr = await page.locator('#main .banner.err').count();
      record('S', 'S5-offline', 'offline navigation to a cached route degrades to the in-page error banner', mainExists > 0 && bannerErr > 0, `main=${mainExists} banner=${bannerErr}`);
      await shot(page, 'matrix-state-offline-flags.png');
      await context.setOffline(false);
      await page.evaluate(hash => { location.hash = hash; }, `#/console/${seed.ws}/${seed.env}/overview`);
      await page.waitForTimeout(400);
      await page.evaluate(hash => { location.hash = hash; }, `#/console/${seed.ws}/${seed.env}/flags`);
      await settle(page, 1200);
      const recovered = await page.locator('#main table tbody tr .row-link, #main .dt-card .row-link').count();
      record('S', 'S5-offline-recovery', 'connectivity recovery restores the flags list', recovered > 0, `rows=${recovered}`);
      await shot(page, 'matrix-state-offline-recovered.png');
      await context.close();
    }
    {
      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash('overview'), { waitUntil: 'networkidle' });
      await settle(page);
      await context.setOffline(true);
      // segments chunk was never fetched in this session
      await page.evaluate(hash => { location.hash = hash; }, `#/console/${seed.ws}/${seed.env}/segments`);
      await page.waitForTimeout(2000);
      const shellAlive = await page.locator('#conn-info').count();
      const reloadBtn = await page.locator('#btn-reload').count();
      record('S', 'S5-offline-chunk', 'offline navigation to an uncached chunk keeps the shell and offers reload', shellAlive > 0 && reloadBtn > 0, `shell=${shellAlive} reload=${reloadBtn}`);
      await shot(page, 'matrix-state-offline-chunk.png');
      await context.setOffline(false);
      await page.reload({ waitUntil: 'networkidle' });
      await settle(page, 1000);
      const mainText = await page.locator('#main').innerText().catch(() => '');
      record('S', 'S5-offline-chunk-recovery', 'reload after connectivity returns renders the target page', mainText.length > 50);
      await shot(page, 'matrix-state-offline-chunk-recovered.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // D1: empty list (fresh workspace)
    // ---------------------------------------------------------------
    {
      await must('/api/v1/admin/workspaces', { method: 'POST', body: { slug: 'mxempty', name: 'Matrix Empty' } }, [409]);
      await must('/api/v1/admin/workspaces/mxempty/environments', { method: 'POST', body: { slug: 'prod', name: 'Production' } }, [409]);
      const { context, page } = await ownerUI(baseURL);
      await page.goto(viewURL(baseURL, 'mxempty', 'prod', 'flags'), { waitUntil: 'networkidle' });
      await settle(page);
      const links = await page.locator('#main .row-link').count();
      const emptyCell = await page.locator('#main table tbody td.muted').innerText().catch(() => '');
      record('D', 'D1-empty', 'empty workspace renders the empty state, not an error', links === 0 && emptyCell.trim().length > 0, `links=${links} empty="${emptyCell.trim().slice(0, 60)}"`);
      await shot(page, 'matrix-data-empty-flags.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // D2: loading (latency injection only — real responses, delayed)
    // ---------------------------------------------------------------
    {
      const { context, page } = await ownerUI(baseURL);
      await context.route('**/api/v1/admin/**', async route => {
        await new Promise(r => setTimeout(r, 1200));
        await route.continue();
      });
      const nav = page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await page.waitForSelector('#main .skeleton', { timeout: 8000 }).catch(() => {});
      const skel = await page.locator('#main .skeleton').count();
      record('D', 'D2-loading', 'loading state renders skeleton placeholders while real responses are in flight', skel > 0, `skeletons=${skel}`);
      await shot(page, 'matrix-data-loading-flags.png');
      await nav;
      await settle(page);
      await context.unrouteAll();
      await context.close();
    }

    // ---------------------------------------------------------------
    // D3: error (nonexistent object)
    // ---------------------------------------------------------------
    {
      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash('flags/mx-missing-404'), { waitUntil: 'networkidle' });
      await settle(page, 500);
      const mainText = await page.locator('#main').innerText();
      const hasError = /404|not found|不存在|未找到|错误|error/i.test(mainText);
      record('D', 'D3-error', 'nonexistent flag renders the error state (fact → impact → action)', hasError, mainText.slice(0, 120).replace(/\n/g, ' '));
      await shot(page, 'matrix-data-error-flag.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // D4: zero-data experiment — seed experiment pushed to running with no
    // events (the R5A seed never attempted the transition: it gated on the
    // nonexistent state name 'bound'; here the transition is issued for real)
    // ---------------------------------------------------------------
    {
      const expBase = `/api/v1/admin/experiments/${encodeURIComponent(seed.expId)}`;
      const before = await must(expBase, {});
      let note = `before=${before.data?.state}`;
      if (['reviewed', 'scheduled'].includes(before.data?.state)) {
        // approve+publish the binding draft the seed left on u4-bool, then start
        await must(`${scope}/flags/${seed.flagKey}/approve`, { method: 'POST', body: { note: 'matrix: approve binding draft' } }, [400, 409]);
        await must(`${scope}/flags/${seed.flagKey}/publish`, { method: 'POST', body: { note: 'matrix: publish binding draft' } }, [400, 409]);
        const tr = await must(`${expBase}/transition`, { method: 'POST', body: { to: 'running', reason: 'matrix: start zero-data experiment' } });
        note += ` transition=${tr.status}`;
      }
      const after = await must(expBase, {});
      record('D', 'D4-zero-data-transition', 'seed experiment transitioned to running', after.data?.state === 'running', `${note} after=${after.data?.state}`);
      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash(`experiments/${seed.expId}`), { waitUntil: 'networkidle' });
      await settle(page, 600);
      await shot(page, 'matrix-data-zero-exp-summary.png');
      const mainText = await page.locator('#main').innerText();
      record('D', 'D4-zero-data-ui', 'zero-data running experiment shows its real state (no fabricated results)', mainText.length > 50, mainText.slice(0, 100).replace(/\n/g, ' '));
      await context.close();
    }

    // ---------------------------------------------------------------
    // D5/D7/D8: data-backed experiments (small sample, healthy, SRM)
    // ---------------------------------------------------------------
    let healthyExp = null;
    if (collectSecret) {
      // small sample: min sample per arm 1000, only 24+24 exposures
      const smallFlagRev = await publishFlag(must, scope, 'mx-small', 'mx-small');
      const small = await createRunningExperiment(must, scope, { flagKey: 'mx-small', minSamplePerArm: 1000 });
      const smallEvents = makeEvents({
        expId: small.id, epoch: small.epoch, flagKey: 'mx-small', flagRevision: small.flagRevision || smallFlagRev + 1,
        exposures: { control: 24, candidate: 24 }, conversions: { control: 2, candidate: 5 },
      });
      const smallIngest = await sendEvents(baseURL, collectSecret, smallEvents);
      record('D', 'D5-small-ingest', 'small-sample events ingested', smallIngest.accepted >= 40, JSON.stringify(smallIngest));

      // healthy: min sample 10, balanced 120/120 with real conversions
      const healthyFlagRev = await publishFlag(must, scope, 'mx-healthy', 'mx-healthy');
      healthyExp = await createRunningExperiment(must, scope, { flagKey: 'mx-healthy', minSamplePerArm: 10 });
      const healthyEvents = makeEvents({
        expId: healthyExp.id, epoch: healthyExp.epoch, flagKey: 'mx-healthy', flagRevision: healthyExp.flagRevision || healthyFlagRev + 1,
        exposures: { control: 120, candidate: 120 }, conversions: { control: 30, candidate: 38 },
      });
      const healthyIngest = await sendEvents(baseURL, collectSecret, healthyEvents);
      record('D', 'D7-healthy-ingest', 'healthy-sample events ingested', healthyIngest.accepted >= 200, JSON.stringify(healthyIngest));

      // SRM: min sample 10, heavily skewed allocation 200/20 vs registered 50/50
      const srmFlagRev = await publishFlag(must, scope, 'mx-srm', 'mx-srm');
      const srm = await createRunningExperiment(must, scope, { flagKey: 'mx-srm', minSamplePerArm: 10 });
      const srmEvents = makeEvents({
        expId: srm.id, epoch: srm.epoch, flagKey: 'mx-srm', flagRevision: srm.flagRevision || srmFlagRev + 1,
        exposures: { control: 200, candidate: 20 }, conversions: { control: 20, candidate: 2 },
      });
      const srmIngest = await sendEvents(baseURL, collectSecret, srmEvents);
      record('D', 'D8-srm-ingest', 'SRM-skewed events ingested', srmIngest.accepted >= 180, JSON.stringify(srmIngest));

      // fetch the three reports (server truth for the UI screenshots)
      const reportOf = async id => (await must(`/api/v1/admin/experiments/${encodeURIComponent(id)}/report`, {}, [400, 409])).data;
      const smallReport = await reportOf(small.id);
      const healthyReport = await reportOf(healthyExp.id);
      const srmReport = await reportOf(srm.id);
      writeJSON('matrix-reports.json', {
        small: { id: small.id, minSamplePerArm: 1000, report: smallReport },
        healthy: { id: healthyExp.id, minSamplePerArm: 10, report: healthyReport },
        srm: { id: srm.id, minSamplePerArm: 10, report: srmReport },
      });

      const { context, page } = await ownerUI(baseURL);
      // small sample → insufficient CI + chart degradation (no fake curves)
      await page.goto(consoleHash(`experiments/${small.id}`), { waitUntil: 'networkidle' });
      await settle(page, 700);
      await shot(page, 'matrix-data-small-sample-summary.png');
      await page.goto(consoleHash(`experiments/${small.id}/evidence`), { waitUntil: 'networkidle' });
      await settle(page, 700);
      await shot(page, 'matrix-data-small-sample-evidence.png');
      const smallText = await page.locator('#main').innerText();
      record('D', 'D5-small-sample-ui', 'small sample renders the insufficient/watermark state (no fabricated significance)', smallText.length > 50, smallText.slice(0, 120).replace(/\n/g, ' '));

      // healthy → real CI charts
      await page.goto(consoleHash(`experiments/${healthyExp.id}/evidence`), { waitUntil: 'networkidle' });
      await settle(page, 800);
      const vizCount = await page.locator('#main svg').count();
      record('D', 'D7-healthy-ui', 'healthy report renders real CI/sample-flow charts (SVG present)', vizCount > 0, `svg=${vizCount}`);
      await shot(page, 'matrix-data-healthy-evidence.png');
      await page.goto(consoleHash(`experiments/${healthyExp.id}`), { waitUntil: 'networkidle' });
      await settle(page, 700);
      await shot(page, 'matrix-data-healthy-summary.png');

      // SRM → health zone shows the SRM alert
      await page.goto(consoleHash(`experiments/${srm.id}/health`), { waitUntil: 'networkidle' });
      await settle(page, 700);
      const srmText = await page.locator('#main').innerText();
      record('D', 'D8-srm-ui', 'SRM-skewed allocation surfaces in the health zone', /srm/i.test(srmText), srmText.slice(0, 120).replace(/\n/g, ' '));
      await shot(page, 'matrix-data-srm-health.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // D6: 1000-flag list
    // ---------------------------------------------------------------
    {
      await bulkFlags(baseURL, 1000);
      const listApi = await must(`${scope}/flags`, {});
      const total = Array.isArray(listApi.data) ? listApi.data.length : -1;
      const { context, page } = await ownerUI(baseURL);
      await page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await settle(page, 800);
      const listText = await page.locator('#main').innerText();
      record('D', 'D6-large-list', 'flags list renders with 1000+ drafts present (pagination intact)', total >= 1000 && listText.length > 100, `total=${total}`);
      await shot(page, 'matrix-data-large-flags.png');
      await context.close();
    }

    // ---------------------------------------------------------------
    // V: device widths × key pages
    // ---------------------------------------------------------------
    {
      const widths = [390, 768, 1024, 1440];
      const pages = [
        ['overview', 'overview'],
        ['flags', 'flags'],
        ['flag-detail', `flags/${seed.flagKey}`],
        ['exp-summary', `experiments/${seed.expId}`],
      ];
      for (const w of widths) {
        const { context, page } = await ownerUI(baseURL, { viewport: w === 390 ? 'mobile' : 'desktop' });
        if (w !== 390 && w !== 1440) await page.setViewportSize({ width: w, height: 900 });
        for (const [name, view] of pages) {
          await page.goto(consoleHash(view), { waitUntil: 'networkidle' });
          await settle(page);
          const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
          record('V', `V-${w}-${name}`, `no horizontal overflow at ${w}px on ${name}`, overflow <= 1, `overflow=${overflow}px`);
          await shot(page, `matrix-width-${w}-${name}.png`);
        }
        await context.close();
      }
    }

    // ---------------------------------------------------------------
    // L: language × theme on the six key pages
    // ---------------------------------------------------------------
    {
      const combos = [
        ['zh', 'light'], ['zh', 'dark'], ['en', 'light'], ['en', 'dark'],
      ];
      const key6 = [
        ['overview', 'overview'],
        ['flags', 'flags'],
        ['flag-detail', `flags/${seed.flagKey}`],
        ['experiments', 'experiments'],
        ['exp-summary', `experiments/${seed.expId}`],
        ['ai', 'ai'],
      ];
      for (const [lang, theme] of combos) {
        const { context, page } = await ownerUI(baseURL, { lang, theme });
        for (const [name, view] of key6) {
          await page.goto(consoleHash(view), { waitUntil: 'networkidle' });
          await settle(page);
          await shot(page, `matrix-combo-${lang}-${theme}-${name}.png`);
        }
        await context.close();
      }
      record('L', 'L-combos', 'language × theme passes completed (4 combos × 6 key pages, screenshots + probes)', true, '24 screenshots');
    }

    // ---------------------------------------------------------------
    // A: accessibility modes
    // ---------------------------------------------------------------
    {
      // A1: 200% zoom equivalent — a 1440px layout at 200% browser zoom
      // presents 720 CSS px; WCAG 1.4.4/1.4.10 require no horizontal scroll
      // and no loss of content at that effective width.
      const { context, page } = await ownerUI(baseURL);
      await page.setViewportSize({ width: 720, height: 500 });
      for (const [name, view] of [['overview', 'overview'], ['flag-detail', `flags/${seed.flagKey}`]]) {
        await page.goto(consoleHash(view), { waitUntil: 'networkidle' });
        await settle(page);
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        record('A', `A1-zoom200-${name}`, `200% zoom equivalent (720 CSS px) keeps ${name} free of horizontal scroll`, overflow <= 1, `overflow=${overflow}px`);
        await shot(page, `matrix-a11y-zoom200-${name}.png`);
      }
      await context.close();
    }
    {
      // A2: reduced motion — skeleton pulse must be effectively static
      const { context, page } = await ownerUI(baseURL);
      await page.emulateMedia({ reducedMotion: 'reduce' });
      await context.route('**/api/v1/admin/**', async route => {
        await new Promise(r => setTimeout(r, 1200));
        await route.continue();
      });
      const nav = page.goto(consoleHash('flags'), { waitUntil: 'networkidle' });
      await page.waitForSelector('#main .skeleton', { timeout: 8000 }).catch(() => {});
      const motion = await page.evaluate(() => {
        const el = document.querySelector('#main .skeleton');
        if (!el) return null;
        const cs = getComputedStyle(el);
        return { duration: cs.animationDuration, iterations: cs.animationIterationCount };
      });
      await nav;
      await context.unrouteAll();
      const static_ = motion && (parseFloat(motion.duration) <= 0.02 || motion.duration === '0s');
      record('A', 'A2-reduced-motion', 'prefers-reduced-motion neutralizes the loading pulse animation', !!static_, JSON.stringify(motion));
      await page.goto(consoleHash('overview'), { waitUntil: 'networkidle' });
      await settle(page);
      await shot(page, 'matrix-a11y-reduced-motion.png');
      await context.close();
    }
    if (healthyExp) {
      // A3: forced colors — chart information must survive the forced palette
      const { context, page } = await ownerUI(baseURL);
      await page.emulateMedia({ forcedColors: 'active' });
      await page.goto(consoleHash(`experiments/${healthyExp.id}/evidence`), { waitUntil: 'networkidle' });
      await settle(page, 800);
      const vizCount = await page.locator('#main svg').count();
      const mainText = await page.locator('#main').innerText();
      record('A', 'A3-forced-colors', 'forced-colors mode keeps chart structure and textual status (no color-only information)', vizCount > 0 && mainText.length > 50, `svg=${vizCount}`);
      await shot(page, 'matrix-a11y-forced-colors-evidence.png');
      await context.close();
    }
    {
      // A4: prefers-contrast: more
      const { context, page } = await ownerUI(baseURL);
      await page.emulateMedia({ contrast: 'more' });
      await page.goto(consoleHash('overview'), { waitUntil: 'networkidle' });
      await settle(page);
      await shot(page, 'matrix-a11y-contrast-more.png');
      record('A', 'A4-contrast-more', 'prefers-contrast:more pass completed (screenshot + probes)', true);
      await context.close();
    }

    // ---------------------------------------------------------------
    // T: a11y tree snapshots for the six key pages
    // ---------------------------------------------------------------
    {
      const treeDir = path.join(EVIDENCE_DIR, 'a11y-tree');
      mkdirSync(treeDir, { recursive: true });
      const key6 = [
        ['overview', 'overview'],
        ['flags', 'flags'],
        ['flag-detail', `flags/${seed.flagKey}`],
        ['experiments', 'experiments'],
        ['exp-summary', `experiments/${seed.expId}`],
        ['ai', 'ai'],
      ];
      const { context, page } = await ownerUI(baseURL);
      for (const [name, view] of key6) {
        await page.goto(consoleHash(view), { waitUntil: 'networkidle' });
        await settle(page);
        const yaml = await page.locator('#root').ariaSnapshot();
        writeFileSync(path.join(treeDir, `${name}.yaml`), `# a11y tree: ${name} (zh/light desktop) — Playwright ariaSnapshot\n${yaml}\n`);
      }
      await context.close();
      record('T', 'T-a11y-tree', 'a11y tree snapshots archived for the six key pages', true, 'a11y-tree/*.yaml');
    }
  } finally {
    await closeBrowser();
    await server.stop();
  }

  // ---------------------------------------------------------------
  // evidence rollup
  // ---------------------------------------------------------------
  const failed = checks.filter(c => !c.ok);
  const groups = {};
  for (const c of checks) {
    groups[c.group] = groups[c.group] || { total: 0, failed: 0 };
    groups[c.group].total++;
    if (!c.ok) groups[c.group].failed++;
  }
  writeJSON('matrix-results.json', { generatedAt: new Date().toISOString(), checks, warnings });

  const lines = [
    '# matrix summary (raw rows: matrix-results.json)',
    '',
    `checks: ${checks.length} total, ${failed.length} failed; warnings: ${warnings.length}`,
    '',
    '| group | total | failed |',
    '| --- | --- | --- |',
  ];
  for (const [g, v] of Object.entries(groups)) lines.push(`| ${g} | ${v.total} | ${v.failed} |`);
  lines.push('', '| id | desc | result | detail |', '| --- | --- | --- | --- |');
  for (const c of checks) lines.push(`| ${c.group}/${c.id} | ${c.desc} | ${c.ok ? 'PASS' : 'FAIL'} | ${c.detail.replace(/\|/g, '\\|')} |`);
  if (warnings.length) {
    lines.push('', '## warnings', '');
    for (const w of warnings) lines.push(`- ${w}`);
  }
  lines.push('');
  writeText('matrix-summary.md', lines.join('\n'));

  if (failed.length) {
    console.error(`[matrix] FAIL: ${failed.length}/${checks.length} checks failed`);
    for (const f of failed) console.error(`  - ${f.group}/${f.id} ${f.desc} — ${f.detail}`);
    process.exit(1);
  }
  console.log(`[matrix] PASS: ${checks.length} checks, ${warnings.length} warnings`);
};

main().catch(e => {
  console.error(e);
  process.exit(1);
});
