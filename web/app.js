'use strict';

// ---------------------------------------------------------------------------
// GoLab admin console — plain fetch + DOM, no frameworks, no build step.
// All dynamic data is injected via esc()/textContent, never raw innerHTML.
// ---------------------------------------------------------------------------

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d.getTime()) ? '—' : d.toLocaleString(localeFor());
}
function fmtDurNs(ns) {
  if (ns == null) return '—';
  const h = Number(ns) / 3.6e12;
  return h >= 24 ? t('unit.days', { n: h / 24 }) : t('unit.hours', { n: h });
}
function shortHash(h) { return h ? String(h).slice(0, 12) : '—'; }
function chip(label, cls) { return '<span class="chip ' + (cls || 'gray') + '">' + esc(label) + '</span>'; }
function bindListSearch(m) {
  const input = $('.list-search', m);
  if (!input) return;
  input.oninput = () => {
    const query = input.value.trim().toLocaleLowerCase();
    $$('tbody tr', m).forEach(row => { row.hidden = !!query && !row.textContent.toLocaleLowerCase().includes(query); });
  };
}

function reviewPreview(p, id = 'review-preview') {
  if (!p) return '';
  return '<section class="card" id="' + esc(id) + '"><h3>' + esc(t('approval.preview')) + '</h3>' +
    '<p>' + esc(t('approval.environment')) + ': ' + esc(p.targetEnvironment) + ' · ' +
      esc(t('approval.risk')) + ': ' + esc(p.risk) + ' · ' + esc(t('approval.base')) + ': r' + esc(p.baseRevision) + '</p>' +
    '<p>' + esc(t('approval.changed')) + ': ' + esc((p.changedFields || []).join(', ') || '—') + '</p>' +
    '<p>' + esc(t('approval.impact')) + ': ' + esc(p.estimatedAffected) + '</p>' +
    '<p>' + esc(t('approval.rollback')) + ': ' + esc(p.rollbackTarget) + '</p>' +
    '<p>' + esc(t('approval.guardrails')) + ': ' + esc(p.guardrails) + '</p>' +
    '<p class="mono">' + esc(t('approval.hash')) + ': ' + esc(p.proposedHash) + '</p>' +
    (p.approvalExpiresAt ? '<p>' + esc(t('approval.expires')) + ': ' + esc(fmtTime(p.approvalExpiresAt)) + '</p>' : '') +
    '</section>';
}

async function requestChange(action, object, payload, reasonOverride = '') {
  if (!confirmLeaveEditor()) return null;
  const reason = reasonOverride || prompt(t('approval.reason'), '');
  if (!reason) { toast(t('toast.reasonRequired'), false); return null; }
  const a = await api(scopePath() + '/approvals', { method: 'POST', body: { action, object, payload, reason } });
  toast(t('approval.requested', { id: a.id }));
  return a;
}

const DRAFT_CHIP = { editing: 'blue', submitted: 'amber', approved: 'green' };
const EXP_CHIP = { draft: 'gray', reviewed: 'blue', bound: 'teal', running: 'green', paused: 'amber', stopped: 'red', analyzed: 'blue', archived: 'gray' };
const VERDICT_CHIP = { ship: 'green', rollback: 'red', continue: 'blue', hold: 'amber' };
const METRIC_CHIP = { draft: 'blue', published: 'green', deprecated: 'gray' };

function toast(msg, ok = true) {
  const d = document.createElement('div');
  d.className = 'toast ' + (ok ? 'ok' : 'err');
  d.textContent = msg;
  $('#toasts').appendChild(d);
  setTimeout(() => d.classList.add('fade'), 3800);
  setTimeout(() => d.remove(), 4300);
}

// ---------------------------------------------------------------------------
// API layer
// ---------------------------------------------------------------------------
class ApiError extends Error {
  constructor(status, code, message) { super(message); this.status = status; this.code = code; }
}
function baseUrl() { return (localStorage.getItem('golab_url') || '').replace(/\/+$/, ''); }
let memoryToken = '';
let cookieSession = false;
const sameOriginConsole = () => !baseUrl() || baseUrl() === location.origin;

async function api(path, opts = {}) {
  const { method = 'GET', body } = opts;
  const headers = {};
  if (!cookieSession && memoryToken) headers.Authorization = 'Bearer ' + memoryToken;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(baseUrl() + path, {
      method, headers, credentials: cookieSession ? 'same-origin' : 'omit',
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    throw new ApiError(0, 'network', t('err.cannotReach', { url: baseUrl() || t('err.sameOrigin'), msg: e.message }));
  }
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { /* non-JSON body */ } }
  if (!res.ok) {
    const err = (data && data.error) || {};
    const e = new ApiError(res.status, err.code || 'http_' + res.status, err.message || res.statusText);
    if (res.status === 401) { cookieSession = false; memoryToken = ''; showAuth(t('err.tokenRejected401')); }
    throw e;
  }
  return data;
}

async function downloadReport(path, format) {
  const headers = {};
  if (!cookieSession && memoryToken) headers.Authorization = 'Bearer ' + memoryToken;
  let res;
  try {
    res = await fetch(baseUrl() + path + '?format=' + format, {
      headers, credentials: cookieSession ? 'same-origin' : 'omit',
    });
  } catch (e) {
    throw new ApiError(0, 'network', t('err.cannotReach', { url: baseUrl() || t('err.sameOrigin'), msg: e.message }));
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = body.error || {};
    if (res.status === 401) { cookieSession = false; memoryToken = ''; showAuth(t('err.tokenRejected401')); }
    throw new ApiError(res.status, err.code || 'http_' + res.status, err.message || res.statusText);
  }
  const url = URL.createObjectURL(await res.blob());
  const link = document.createElement('a');
  link.href = url;
  link.download = 'grayling-report.' + format;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

function errMsg(e) {
  if (e.status === 403) return t('err.denied403');
  if (e.status === 409) return t('err.conflict409');
  if (e.status === 429) return t('err.limited429');
  let m = '[' + (e.code || 'error') + '] ' + (e.message || e);
  if (e.status === 423) m += t('err.revoked423');
  if (e.status === 501) m += t('err.notEnabled501');
  return m;
}
function errBanner(e) {
  let h = '<div class="banner err">' + esc(errMsg(e));
  if (e.status === 501) h += '<div class="muted">' + esc(t('banner.notEnabled501')) + '</div>';
  if (e.status === 403) h += ' <button class="btn sm" id="btn-reauth2">' + esc(t('btn.reenterToken')) + '</button>';
  return h + '</div>';
}
function reportError(e) {
  toast(errMsg(e), false);
  if (e.status === 403) refreshCapabilities().then(route);
}
// Runs a mutation: success → toast + re-route; failure → toast with code+message.
async function run(fn, okMsg, savedEditor = false) {
  if (!savedEditor && !confirmLeaveEditor()) return undefined;
  try {
    const r = await fn();
    if (savedEditor) S.editorBaseline = editorValue();
    toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
    route();
    return r;
  } catch (e) {
    toast(e.code === 'self_approval_denied' ? t('toast.selfApproval') : errMsg(e), false);
    if (e.status === 403) { await refreshCapabilities(); await route(); }
    return undefined;
  }
}

// ---------------------------------------------------------------------------
// App state
// ---------------------------------------------------------------------------
const S = {
  view: 'overview',
  ws: localStorage.getItem('golab_ws') || '',
  env: localStorage.getItem('golab_env') || '',
  workspaces: [], envs: [],
  capabilities: null,
  globalCapabilities: null,
  flagKey: null, flagNew: false,
  segKey: null, segDrafts: [],
  expId: null, expCreate: false, expTab: 'summary',
  metricRef: null, metricQuery: '', metricStatus: '', metricCursor: '',
  deepLinkRequested: false, deepLinkDenied: false,
  auditLimit: 50,
  auditQuery: '', auditCursors: [0], auditPage: 0,
  aiAvailable: true,
  booted: false,
  timer: null,
  pollCleanup: null,
  editorBaseline: null,
};
function editorValue() {
  const flag = $('#flag-json');
  if (flag) return JSON.stringify(['flag', flag.value, $('#flag-risk')?.value, $('#flag-base')?.value]);
  const segment = $('#seg-json');
  if (segment) return JSON.stringify(['segment', segment.value, $('#seg-risk')?.value]);
  return null;
}
function confirmLeaveEditor() {
  const value = editorValue();
  if (value === null || S.editorBaseline === null || value === S.editorBaseline) return true;
  return confirm(t('confirm.unsaved'));
}
const wsPath = () => '/api/v1/admin/workspaces/' + encodeURIComponent(S.ws);
const scopePath = () => wsPath() + '/environments/' + encodeURIComponent(S.env);
const can = permission => permission === 'workspace.admin'
  ? !!(S.globalCapabilities && S.globalCapabilities[permission])
  : !!(S.capabilities && S.capabilities.permissions[permission]);

async function refreshCapabilities() {
  const ws = S.ws;
  S.capabilities = null;
  S.globalCapabilities = null;
  updateCapabilities();
  try { S.globalCapabilities = await api('/api/v1/admin/capabilities'); }
  catch (e) { if (e.status !== 403 && e.status !== 404) reportError(e); }
  if (ws !== S.ws) return;
  if (ws) {
    try {
      const data = await api(wsPath() + '/capabilities');
      if (ws !== S.ws) return;
      S.capabilities = data;
    } catch (e) {
      if (e.status !== 403 && e.status !== 404) reportError(e);
    }
  }
  updateCapabilities();
  if (!can('token.manage') && S.view === 'tokens') S.view = 'flags';
  if (!can('quota.manage') && S.view === 'quotas') S.view = 'flags';
}

function updateCapabilities() {
  $('#nav-quotas').classList.toggle('hidden', !can('quota.manage'));
  $('[data-view="tokens"]').classList.toggle('hidden', !can('token.manage'));
  $('#btn-new-ws').classList.toggle('hidden', !can('workspace.admin'));
  $('#btn-new-env').classList.toggle('hidden', !S.ws || !can('workspace.admin'));
}

// Hide controls after each render. The server still checks every request.
function gateActions(root) {
  const groups = {
    'flag.write': ['#flag-new', '#flag-save', '#flag-submit', '#flag-delete', '#gff-run', '#seg-new', '#seg-save', '#seg-submit'],
    'flag.approve': ['#flag-approve', '#flag-reject', '#seg-approve', '[data-appr]'],
    'flag.publish': ['#flag-publish', '#flag-emergency-disable', '[data-rb]', '[data-er]', '#seg-publish', '#exp-bind', '#exp-transition', '#ro-create', '#ro-pause', '#ro-resume', '#ro-advance', '#ro-takeover-on', '#ro-takeover-off', '[data-pin]', '[data-exec]', '#metric-publish', '#metric-deprecate'],
    'experiment.write': ['#exp-new', '#x-create', '#exp-review', '#metric-new', '#metric-submit'],
    'ai.write': ['#ai-reg', '[data-revoke]', '[data-reinstate]'],
    'ai.content.read': ['#ai-resolve', '#ai-allow-add', '#ai-allow-save'],
  };
  for (const [permission, selectors] of Object.entries(groups)) {
    if (can(permission)) continue;
    for (const selector of selectors) $$(selector, root).forEach(el => el.classList.add('hidden'));
  }
}

function needWs(m) {
  if (!S.ws) { m.innerHTML = '<div class="banner warn">' + t('banner.noWs') + '</div>'; return false; }
  return true;
}
function needScope(m) {
  if (!S.ws || !S.env) { m.innerHTML = '<div class="banner warn">' + t('banner.noScope') + '</div>'; return false; }
  return true;
}

// Small modal form builder. fields: [{name,label,type,value,placeholder,options,rows}]
function promptForm(title, fields, submitLabel, onSubmit) {
  const ov = document.createElement('div');
  ov.className = 'overlay';
  ov.innerHTML = '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="modal-title"><h3 id="modal-title">' + esc(title) + '</h3><div class="modal-body"></div>' +
    '<div class="modal-foot"><button class="btn" data-x="cancel">' + esc(t('btn.cancel')) + '</button>' +
    '<button class="btn primary" data-x="ok">' + esc(submitLabel || t('btn.ok')) + '</button></div></div>';
  const body = $('.modal-body', ov);
  for (const f of fields) {
    const w = document.createElement('label');
    w.className = 'field';
    w.appendChild(document.createTextNode(f.label));
    let inp;
    if (f.type === 'textarea') { inp = document.createElement('textarea'); inp.rows = f.rows || 6; }
    else if (f.type === 'select') {
      inp = document.createElement('select');
      for (const o of f.options || []) {
        const op = document.createElement('option');
        op.value = o.value !== undefined ? o.value : o;
        op.textContent = o.label !== undefined ? o.label : o;
        inp.appendChild(op);
      }
    } else { inp = document.createElement('input'); inp.type = f.type || 'text'; }
    inp.name = f.name;
    inp.value = f.value ?? '';
    if (f.placeholder) inp.placeholder = f.placeholder;
    w.appendChild(inp);
    body.appendChild(w);
  }
  const previousFocus = document.activeElement;
  document.body.appendChild(ov);
  const close = () => { ov.remove(); previousFocus?.focus?.(); };
  ov.addEventListener('keydown', ev => {
    if (ev.key === 'Escape') { ev.preventDefault(); close(); return; }
    if (ev.key !== 'Tab') return;
    const focusables = $$('input, select, textarea, button', ov).filter(el => !el.disabled);
    if (!focusables.length) return;
    const first = focusables[0], last = focusables[focusables.length - 1];
    if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
    else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
  });
  ($('input, select, textarea', ov) || $('[data-x="cancel"]', ov))?.focus?.();
  $('[data-x="cancel"]', ov).onclick = close;
  ov.addEventListener('click', ev => { if (ev.target === ov) close(); });
  $('[data-x="ok"]', ov).onclick = async () => {
    const ok = $('[data-x="ok"]', ov);
    ok.disabled = true;
    const vals = {};
    for (const f of fields) vals[f.name] = body.querySelector('[name="' + f.name + '"]').value;
    try { await onSubmit(vals); close(); } catch (e) { reportError(e); ok.disabled = false; }
  };
}

function actionDialog({ title, summary, preview = '', reasonLabel = '', reasonRequired = true, confirmLabel = '' }) {
  return new Promise(resolve => {
    const ov = document.createElement('div');
    const previousFocus = document.activeElement;
    ov.className = 'overlay';
    ov.innerHTML = '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="action-title" aria-describedby="action-summary">' +
      '<h3 id="action-title">' + esc(title) + '</h3><p id="action-summary">' + esc(summary) + '</p>' + preview +
      (reasonLabel ? '<label class="field">' + esc(reasonLabel) + '<textarea id="action-reason" rows="3"' + (reasonRequired ? ' required' : '') + '></textarea></label>' : '') +
      '<div class="modal-foot"><button class="btn" id="action-cancel" type="button">' + esc(t('btn.cancel')) + '</button>' +
      '<button class="btn danger" id="action-confirm" type="button">' + esc(confirmLabel || t('btn.ok')) + '</button></div></div>';
    const finish = value => { ov.remove(); previousFocus?.focus?.(); resolve(value); };
    $('#action-cancel', ov).onclick = () => finish(null);
    ov.addEventListener('click', ev => { if (ev.target === ov) finish(null); });
    ov.addEventListener('keydown', ev => {
      if (ev.key === 'Escape') { ev.preventDefault(); finish(null); }
      if (ev.key === 'Tab') {
        const controls = $$('textarea, button', ov);
        if (ev.shiftKey && document.activeElement === controls[0]) { ev.preventDefault(); controls.at(-1).focus(); }
        else if (!ev.shiftKey && document.activeElement === controls.at(-1)) { ev.preventDefault(); controls[0].focus(); }
      }
    });
    $('#action-confirm', ov).onclick = () => {
      const reason = $('#action-reason', ov)?.value.trim() || '';
      if (reasonLabel && reasonRequired && !reason) { $('#action-reason', ov).focus(); return; }
      finish({ reason });
    };
    document.body.appendChild(ov);
    ($('#action-reason', ov) || $('#action-cancel', ov)).focus();
  });
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
const VIEWS = {}; // populated at the bottom of each view section
let routeEpoch = 0;

function consoleHash() {
  const parts = ['console', S.ws || '~', S.env || '~', S.view];
  if (S.view === 'flags' && S.flagKey) parts.push(S.flagKey);
  if (S.view === 'segments' && S.segKey) parts.push(S.segKey);
  if (S.view === 'experiments' && S.expId) {
    parts.push(S.expId);
    if (S.expTab === 'diagnostics') parts.push('diagnostics');
  }
  if (S.view === 'experiments' && S.expCreate) parts.push('~new');
  if (S.view === 'metrics' && S.metricRef) parts.push(S.metricRef.key, String(S.metricRef.version));
  return '#/' + parts.map(encodeURIComponent).join('/');
}

function restoreConsoleHash(hash) {
  if (!hash.startsWith('#/console/')) return false;
  let parts;
  try { parts = hash.slice(2).split('/').map(decodeURIComponent); }
  catch { return false; }
  if (parts.length < 4 || parts[0] !== 'console' || !Object.hasOwn(VIEWS, parts[3])) return false;
  if (parts[3] === 'experiments' && (parts.length > 6 ||
    (parts[5] && (parts[4] === '~new' || parts[5] !== 'diagnostics')))) return false;
  S.ws = parts[1] === '~' ? '' : parts[1];
  S.env = parts[2] === '~' ? '' : parts[2];
  S.view = parts[3];
  S.flagKey = S.segKey = S.expId = S.metricRef = null;
  S.expCreate = false;
  S.expTab = 'summary';
  if (S.view === 'flags' && parts[4]) S.flagKey = parts[4];
  if (S.view === 'segments' && parts[4]) S.segKey = parts[4];
  if (S.view === 'experiments' && parts[4]) {
    if (parts[4] === '~new') S.expCreate = true;
    else {
      S.expId = parts[4];
      if (parts[5] === 'diagnostics') S.expTab = 'diagnostics';
    }
  }
  if (S.view === 'metrics' && parts[4] && /^[1-9]\d*$/.test(parts[5] || '')) {
    S.metricRef = { key: parts[4], version: Number(parts[5]) };
  }
  S.deepLinkRequested = true;
  S.deepLinkDenied = false;
  return true;
}

function syncConsoleHash() {
  if (!S.booted || S.deepLinkDenied) return;
  const target = consoleHash();
  if (location.hash !== target) history.pushState(null, '', target);
}

async function route() {
  const epoch = ++routeEpoch;
  syncConsoleHash();
  if (S.pollCleanup) { S.pollCleanup(); S.pollCleanup = null; }
  if (S.timer) { clearTimeout(S.timer); S.timer = null; }
  $$('#nav button').forEach(b => {
    const active = b.dataset.view === S.view;
    b.classList.toggle('active', active);
    if (active) b.setAttribute?.('aria-current', 'page'); else b.removeAttribute?.('aria-current');
  });
  const m = $('#main');
  if (S.deepLinkDenied) {
    m.innerHTML = '<div class="banner err" role="alert">' + esc(t('err.deepLinkDenied')) + '</div>';
    return;
  }
  m.innerHTML = '<p class="muted">' + esc(t('misc.loading')) + '</p>';
  // Keep async views detached until their data belongs to the current route.
  // A slower request must never replace a newer editor or its unsaved work.
  const stage = document.createElement('div');
  try {
    await VIEWS[S.view](stage);
    if (epoch !== routeEpoch) return;
    m.replaceChildren(stage);
    gateActions(m);
    S.editorBaseline = editorValue();
  } catch (e) {
    if (epoch !== routeEpoch) return;
    if (e.status === 401) return; // showAuth already triggered by api()
    m.innerHTML = errBanner(e);
    const rb = $('#btn-reauth2', m);
    if (rb) rb.onclick = () => showAuth();
  }
}

// ---------------------------------------------------------------------------
// Auth screen & boot
// ---------------------------------------------------------------------------
function showAuth(msg) {
  $('#auth-url').value = localStorage.getItem('golab_url') || '';
  $('#auth-token').value = '';
  if (msg !== undefined) $('#auth-error').textContent = msg;
  $('#app').classList.add('hidden');
  $('#auth').classList.remove('hidden');
}

async function doConnect() {
  const tok = $('#auth-token').value.trim();
  if (!tok) { $('#auth-error').textContent = t('auth.enterToken'); return; }
  localStorage.setItem('golab_url', $('#auth-url').value.trim());
  $('#auth-error').textContent = t('auth.connecting');
  try {
    const target = new URL(baseUrl() || location.origin);
    if (target.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(target.hostname)) {
      throw new ApiError(0, 'tls_required', t('auth.httpsRequired'));
    }
    if (sameOriginConsole()) {
      const res = await fetch(baseUrl() + '/api/v1/admin/session', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'token=' + encodeURIComponent(tok),
      });
      if (!res.ok) {
        let error = {};
        try { error = (await res.json()).error || {}; } catch { /* server may return plain text */ }
        throw new ApiError(res.status, error.code || 'login_failed', error.message || t('auth.rejected401'));
      }
      cookieSession = true;
      memoryToken = '';
    } else {
      // Cross-origin API connections use an ephemeral bearer credential.
      // It is lost when the tab reloads and never enters browser storage.
      memoryToken = tok;
      cookieSession = false;
    }
    $('#auth-token').value = '';
    $('#auth-error').textContent = '';
    await boot();
  } catch (e) {
    memoryToken = '';
    $('#auth-token').value = '';
    $('#auth-error').textContent = e.code === 'tls_required' ? t('auth.httpsRequired') :
      e.status === 401 ? t('auth.rejected401') : errMsg(e);
  }
}

async function signOut() {
  if (!confirmLeaveEditor()) return;
  if (cookieSession) {
    try { await api('/api/v1/admin/session', { method: 'DELETE' }); }
    catch (e) { if (e.status !== 401) reportError(e); }
  }
  cookieSession = false;
  memoryToken = '';
  S.booted = false;
  $('#auth-token').value = '';
  showAuth('');
}

async function loadWorkspaces() {
  S.workspaces = (await api('/api/v1/admin/workspaces')) || [];
  const sel = $('#sel-ws');
  sel.innerHTML = S.workspaces.map(w =>
    '<option value="' + esc(w.slug) + '">' + esc(w.name || w.slug) + ' (' + esc(w.slug) + ')</option>').join('') ||
    '<option value="">' + esc(t('select.noneCreate')) + '</option>';
  if (!S.workspaces.some(w => w.slug === S.ws)) {
    if (S.deepLinkRequested && S.ws) S.deepLinkDenied = true;
    S.ws = S.workspaces.length ? S.workspaces[0].slug : '';
  }
  localStorage.setItem('golab_ws', S.ws);
  sel.value = S.ws;
  await loadEnvs();
  await refreshCapabilities();
}

async function loadEnvs() {
  S.envs = S.ws ? (await api(wsPath() + '/environments')) || [] : [];
  const sel = $('#sel-env');
  sel.innerHTML = S.envs.map(v =>
    '<option value="' + esc(v.slug) + '">' + esc(v.name || v.slug) + ' (' + esc(v.slug) + ')</option>').join('') ||
    '<option value="">' + esc(t('select.noneCreate')) + '</option>';
  if (!S.envs.some(v => v.slug === S.env)) {
    if (S.deepLinkRequested && S.env) S.deepLinkDenied = true;
    S.env = S.envs.length ? S.envs[0].slug : '';
  }
  localStorage.setItem('golab_env', S.env);
  sel.value = S.env;
}

async function boot() {
  await loadWorkspaces();
  S.deepLinkRequested = false;
  $('#auth').classList.add('hidden');
  $('#app').classList.remove('hidden');
  S.booted = true;
  $('#conn-info').textContent = (baseUrl() || location.origin) + ' · ' + t(cookieSession ? 'auth.session' : 'auth.temporary');
  await route();
  probeAI();
}

async function probeAI() {
  if (!S.ws || !S.env) return;
  const ws = S.ws, env = S.env;
  try {
    await api(scopePath() + '/ai/resources');
    if (ws !== S.ws || env !== S.env) return;
    S.aiAvailable = true;
  } catch (e) {
    if (ws !== S.ws || env !== S.env) return;
    if (e.status === 501) {
      S.aiAvailable = false;
      if (S.view === 'ai') { S.view = 'flags'; route(); }
    }
  }
  $('#nav-ai').classList.toggle('hidden', !S.aiAvailable);
}

// ---------------------------------------------------------------------------
// View: Overview
// ---------------------------------------------------------------------------
async function viewOverview(m) {
  if (!needScope(m)) return;
  const [experimentsRead, approvalsRead, flagsRead] = await Promise.allSettled([
    api(scopePath() + '/experiments'), api(wsPath() + '/approvals'), api(scopePath() + '/flags'),
  ]);
  const experiments = experimentsRead.status === 'fulfilled' && Array.isArray(experimentsRead.value) ? experimentsRead.value : null;
  const approvals = approvalsRead.status === 'fulfilled' && Array.isArray(approvalsRead.value) ? approvalsRead.value : null;
  const flags = flagsRead.status === 'fulfilled' && Array.isArray(flagsRead.value) ? flagsRead.value : null;
  const relevant = experiments?.filter(e => e.state === 'running' || e.state === 'paused').slice(0, 3) || [];
  const reports = await Promise.allSettled(relevant.map(e => api('/api/v1/admin/experiments/' + encodeURIComponent(e.id) + '/report')));
  const issues = [];
  for (const e of experiments || []) {
    if (e.state === 'paused') issues.push({ kind: 'experiment', id: e.id, text: t('overview.paused') });
  }
  for (let i = 0; i < relevant.length; i++) {
    const result = reports[i];
    if (result.status === 'fulfilled' && result.value && !result.value.healthy) {
      issues.push({ kind: 'experiment', id: relevant[i].id,
        text: t('overview.reportIssue') + ': ' + (result.value.healthIssues || []).slice(0, 1).join('') });
    }
  }
  for (const a of approvals || []) {
    if (a.state === 'submitted') issues.push({ kind: 'approval', id: a.id, text: a.action + ' · ' + a.object });
  }
  const partial = experiments === null || approvals === null || flags === null || reports.some(r => r.status === 'rejected');
  const count = (value, predicate) => value === null ? '—' : String(value.filter(predicate).length);
  m.innerHTML = '<div class="view-head"><h2>' + esc(t('overview.title')) + '</h2>' +
    '<span class="muted">' + esc(t('overview.scope')) + ': ' + esc(S.ws) + ' / ' + esc(S.env) + '</span>' +
    '<button class="btn sm" id="overview-refresh">' + esc(t('overview.refresh')) + '</button></div>' +
    (partial ? '<div class="banner warn" role="status">' + esc(t('overview.moduleUnavailable')) + '</div>' : '') +
    '<div class="overview-grid">' +
    [[t('overview.paused'), count(experiments, e => e.state === 'paused')],
      [t('overview.running'), count(experiments, e => e.state === 'running')],
      [t('overview.pending'), count(approvals, a => a.state === 'submitted')],
      [t('overview.flags'), count(flags, f => !!f.current)]].map(([label, value]) =>
      '<section class="card overview-stat"><span class="muted">' + esc(label) + '</span><strong>' + esc(value) + '</strong></section>').join('') +
    '</div><section class="card"><h3>' + esc(t('overview.attention')) + '</h3>' +
    (issues.length ? '<ul class="attention-list">' + issues.slice(0, 8).map(item =>
      '<li><span>' + esc(item.text) + '</span><button class="btn sm" data-overview-kind="' + item.kind + '" data-overview-id="' + esc(item.id) + '">' +
      esc(t(item.kind === 'experiment' ? 'overview.openExperiment' : 'overview.openApprovals')) + '</button></li>').join('') + '</ul>' :
      '<p class="muted">' + esc(t('overview.noAttention')) + '</p>') + '</section>';
  $('#overview-refresh', m).onclick = () => route();
  $('.attention-list', m)?.addEventListener('click', ev => {
    const button = ev.target.closest('[data-overview-kind]');
    if (!button) return;
    if (button.dataset.overviewKind === 'experiment') {
      S.view = 'experiments'; S.expId = button.dataset.overviewId; S.expTab = 'summary';
    } else S.view = 'audit';
    route();
  });
}

// ---------------------------------------------------------------------------
// View: Flags
// ---------------------------------------------------------------------------
const OPS = ['eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'time_before',
  'time_after', 'regex', 'in_cidr', 'semver_gt', 'semver_gte', 'semver_lt',
  'semver_lte', 'semver_eq'];

function flagTemplate(key) {
  return {
    schemaVersion: 1, key, type: 'bool', disable: false,
    bucketing: { algorithm: 'golab-fnv1a-10000-v1', unit: 'user_id', salt: 's1' },
    fields: ['user_id'],
    variations: { control: false, treatment: true },
    defaultRule: { variation: 'control' },
    targeting: [],
    experiment: null,
  };
}

// Client-side lint of the flag DSL; returns a list of issue strings.
function lintFlag(c) {
  const issues = [];
  if (typeof c !== 'object' || c === null) return [t('lint.notObject')];
  if (!['bool', 'string', 'number', 'json'].includes(c.type)) issues.push(t('lint.badType'));
  const vars = c.variations || {};
  const checkRule = (r, where) => {
    if (!r) { issues.push(t('lint.ruleMissing', { where })); return; }
    const hasVar = r.variation !== undefined, hasPct = r.percentage !== undefined;
    if (hasVar === hasPct) issues.push(t('lint.ruleExclusive', { where }));
    if (hasVar && !(r.variation in vars)) issues.push(t('lint.unknownVariation', { where, name: r.variation }));
    if (hasPct) {
      const sum = Object.values(r.percentage).reduce((a, b) => a + Number(b), 0);
      if (Math.abs(sum - 100) > 0.01) issues.push(t('lint.pctSum', { where, sum }));
      for (const vn of Object.keys(r.percentage)) if (!(vn in vars)) issues.push(t('lint.unknownVariation', { where, name: vn }));
    }
    for (const con of r.constraints || []) {
      if (!con.field) issues.push(t('lint.constraintField', { where }));
      if (!OPS.includes(con.op)) issues.push(t('lint.unknownOp', { where, op: con.op }));
    }
  };
  checkRule(c.defaultRule, 'defaultRule');
  (c.targeting || []).forEach((r, i) => checkRule(r, 'targeting[' + i + ']'));
  return issues;
}

async function viewFlags(m) {
  if (!needScope(m)) return;
  if (S.flagKey) return viewFlagDetail(m);
  const flags = (await api(scopePath() + '/flags')) || [];
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('flags.title')) + '</h2><span class="muted">' + esc(t('flags.count', { n: flags.length })) + '</span>' +
    '<input class="list-search" type="search" placeholder="' + esc(t('list.search')) + '" aria-label="' + esc(t('list.search')) + '"><button class="btn primary sm" id="flag-new">' + esc(t('flags.new')) + '</button></div>' +
    '<table class="grid"><thead><tr><th>' + esc(t('th.key')) + '</th><th>' + esc(t('th.type')) + '</th><th>' + esc(t('th.draftState')) + '</th><th>' + esc(t('th.revision')) + '</th><th>' + esc(t('th.updated')) + '</th></tr></thead><tbody>' +
    (flags.map(f => {
      const d = f.draft, c = f.current;
      const type = (d && d.content && d.content.type) || (c && c.content && c.content.type) || '—';
      const st = d
        ? chip(d.state, DRAFT_CHIP[d.state] || 'gray') + (d.deleted ? ' ' + chip(t('chip.del'), 'red') : '')
        : (c ? chip(t('chip.published'), 'green') : chip(t('chip.none'), 'gray'));
      const upd = (d && d.updatedAt) || (c && c.publishedAt) || null;
      return '<tr><td class="mono"><button class="row-link" type="button" data-flag="' + esc(f.key) + '">' + esc(f.key) + '</button>' +
        '</td><td>' + esc(type) + '</td><td>' + st + '</td><td>' + esc(f.currentRevision || 0) +
        '</td><td class="nowrap">' + esc(fmtTime(upd)) + '</td></tr>';
    }).join('') || '<tr><td colspan="5" class="muted">' + esc(t('flags.empty')) + '</td></tr>') +
    '</tbody></table>';
  bindListSearch(m);
  $('#flag-new', m).onclick = () => {
    const key = (prompt(t('prompt.newFlagKey')) || '').trim();
    if (!key) return;
    S.flagKey = key; S.flagNew = true; route();
  };
  $('tbody', m).addEventListener('click', ev => {
    const tr = ev.target.closest('[data-flag]');
    if (tr) { S.flagKey = tr.dataset.flag; S.flagNew = false; route(); }
  });
}

function revisionsTable(f) {
  const revs = (f && f.revisions) || [];
  if (!revs.length) return '<p class="muted">' + esc(t('flags.noRevs')) + '</p>';
  return '<table class="grid"><thead><tr><th>' + esc(t('th.rev')) + '</th><th>' + esc(t('th.hash')) + '</th><th>' + esc(t('th.published')) + '</th><th>' + esc(t('th.by')) + '</th><th>' + esc(t('th.note')) + '</th><th></th></tr></thead><tbody>' +
    revs.map(r => '<tr><td>r' + esc(r.revision) + (r.rollbackOf ? ' <span class="muted">' + esc(t('flags.rollbackOf', { n: r.rollbackOf })) + '</span>' : '') +
      '</td><td class="mono">' + esc(shortHash(r.hash)) + '</td><td class="nowrap">' + esc(fmtTime(r.publishedAt)) +
      '</td><td>' + esc(r.publishedBy || '—') + '</td><td>' + esc(r.note || '') +
      '</td><td><button class="btn sm" data-rb="' + esc(r.revision) + '">' + esc(t('flags.rollbackTo', { n: r.revision })) + '</button> ' +
      '<button class="btn sm danger" data-er="' + esc(r.revision) + '">' + esc(t('approval.emergency')) + '</button></td></tr>').join('') +
    '</tbody></table>';
}

async function viewFlagDetail(m) {
  const key = S.flagKey;
  const kp = scopePath() + '/flags/' + encodeURIComponent(key);
  let f = null;
  try {
    f = await api(kp);
  } catch (e) {
    if (!(e.status === 404 && S.flagNew)) throw e;
  }
  const draft = f && f.draft;
  const content = (draft && draft.content) || (f && f.current && f.current.content) || flagTemplate(key);
  const st = draft
    ? chip(draft.state, DRAFT_CHIP[draft.state] || 'gray') + (draft.deleted ? ' ' + chip(t('chip.deletionPending'), 'red') : '')
    : (f && f.current ? chip(t('chip.published'), 'green') : chip(t('chip.new'), 'gray'));
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="flag-back">' + esc(t('flags.back')) + '</button>' +
    '<h2 class="mono">' + esc(key) + '</h2>' + st +
    (draft ? '<span class="muted">' + esc(t('flags.riskMeta', { risk: draft.risk || '—', by: draft.updatedBy || '—', time: fmtTime(draft.updatedAt) })) + '</span>' : '') +
    '</div>' +
    '<div class="cols">' +
      '<section class="card"><h3>' + esc(t('flags.draftEditor')) + '</h3>' +
        '<textarea id="flag-json" class="json" rows="24" spellcheck="false"></textarea>' +
        '<div class="row"><button class="btn sm" id="flag-validate">' + esc(t('btn.validateJson')) + '</button><span id="flag-valid" class="muted grow"></span>' +
        '<label class="inline">' + esc(t('label.risk')) + ' <select id="flag-risk"><option value="normal">normal</option><option value="high">high</option></select></label>' +
        '<label class="inline">baseRevision <input id="flag-base" type="number" min="0" size="4" value="' + esc((f && f.currentRevision) || 0) + '"></label></div>' +
        '<div class="row actions">' +
        '<button class="btn primary" id="flag-save">' + esc(t('btn.saveDraft')) + '</button>' +
        '<button class="btn" id="flag-submit">' + esc(t('btn.submit')) + '</button>' +
        '<button class="btn" id="flag-approve">' + esc(t('btn.approve')) + '</button>' +
        '<button class="btn" id="flag-reject">' + esc(t('btn.reject')) + '</button>' +
        '<button class="btn" id="flag-publish">' + esc(t('btn.publish')) + '</button>' +
        '<button class="btn danger" id="flag-delete">' + esc(t('btn.markDeleted')) + '</button>' +
        '<button class="btn danger" id="flag-emergency-disable">' + esc(t('approval.emergencyDisable')) + '</button></div></section>' +
      '<section class="card"><h3>' + esc(t('flags.currentPublished', { n: (f && f.currentRevision) || 0 })) + '</h3>' +
        (f && f.current
          ? '<div class="muted">' + esc(t('flags.currentMeta', { hash: shortHash(f.current.hash), time: fmtTime(f.current.publishedAt), by: f.current.publishedBy || '—' })) + '</div>'
          : '<div class="muted">' + esc(t('flags.nothingPublished')) + '</div>') +
        '<pre id="flag-current" class="pre"></pre></section>' +
    '</div>' +
    reviewPreview(f && f.preview) +
    '<section class="card"><h3>' + t('flags.history') + '</h3><div id="flag-revs">' + revisionsTable(f) + '</div></section>' +
    '<section class="card"><h3>' + esc(t('flags.importTitle')) + '</h3>' +
      '<textarea id="gff-doc" class="json" rows="8" spellcheck="false" placeholder="' + esc(t('flags.importPh')) + '"></textarea>' +
      '<div class="row"><label class="inline">key <input id="gff-key" value="' + esc(key) + '"></label>' +
      '<label class="inline">salt <input id="gff-salt" placeholder="' + esc(t('flags.saltPh')) + '"></label>' +
      '<button class="btn" id="gff-run">' + esc(t('flags.importBtn')) + '</button></div>' +
      '<div id="gff-warn"></div></section>';
  $('#flag-json', m).value = JSON.stringify(content, null, 2);
  $('#flag-current', m).textContent = f && f.current ? JSON.stringify(f.current.content, null, 2) : '';
  if (draft && draft.risk) $('#flag-risk', m).value = draft.risk;
  bindFlagDetail(m, key, kp, f);
}

function bindFlagDetail(m, key, kp, f) {
  $('#flag-back', m).onclick = () => { if (!confirmLeaveEditor()) return; S.flagKey = null; S.flagNew = false; route(); };
  // Returns parsed content, or null (with toast) when invalid.
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#flag-json', m).value); }
    catch (e) { toast(t('toast.invalidJson', { msg: e.message }), false); return null; }
    if (!c || c.key !== key) { toast(t('toast.keyMismatch', { key }), false); return null; }
    return c;
  };
  $('#flag-validate', m).onclick = () => {
    const out = $('#flag-valid', m);
    let c;
    try { c = JSON.parse($('#flag-json', m).value); }
    catch (e) { out.className = 'err-text grow'; out.textContent = t('validate.invalidJson', { msg: e.message }); return; }
    const issues = lintFlag(c);
    if (!c || c.key !== key) issues.unshift(t('toast.keyMismatch', { key }));
    if (issues.length) { out.className = 'warn-text grow'; out.textContent = '⚠ ' + issues.join(' · '); }
    else { out.className = 'ok-text grow'; out.textContent = t('validate.ok'); }
  };
  $('#flag-save', m).onclick = () => {
    const content = readJSON();
    if (!content) return;
    run(() => api(kp + '/draft', {
      method: 'PUT',
      body: { content, risk: $('#flag-risk', m).value, baseRevision: Number($('#flag-base', m).value || 0) },
    }), t('toast.draftSaved'), true);
  };
  $('#flag-submit', m).onclick = () => {
    const reason = prompt(t('approval.reason'), '');
    if (!reason) { toast(t('toast.reasonRequired'), false); return; }
    run(() => api(kp + '/submit', { method: 'POST', body: { reason } }), t('toast.draftSubmitted'));
  };
  $('#flag-approve', m).onclick = () => {
    const note = prompt(t('prompt.approveNote'), '') || '';
    run(() => api(kp + '/approve', { method: 'POST', body: { note } }), t('toast.draftApproved'));
  };
  $('#flag-reject', m).onclick = () => {
    const note = prompt(t('prompt.rejectReason'), '') || '';
    run(() => api(kp + '/reject', { method: 'POST', body: { note } }), t('toast.draftRejected'));
  };
  $('#flag-publish', m).onclick = async () => {
    const approval = await actionDialog({ title: t('btn.publish'), summary: t('action.flagSummary', { key }),
      preview: reviewPreview(f?.preview, 'action-review-preview'), reasonLabel: t('prompt.publishNote'), reasonRequired: false, confirmLabel: t('btn.publish') });
    if (!approval) return;
    run(() => api(kp + '/publish', { method: 'POST', body: { note: approval.reason } }),
      r => t('toast.published', { flag: r && r.flagRevision, manifest: r && r.manifestRevision }));
  };
  $('#flag-delete', m).onclick = async () => {
    const approval = await actionDialog({ title: t('btn.markDeleted'), summary: t('confirm.markDeleted', { key }), confirmLabel: t('btn.markDeleted') });
    if (!approval) return;
    run(() => api(kp, { method: 'DELETE' }), t('toast.deletionMarked'));
  };
  $('#flag-emergency-disable', m).onclick = async () => {
    const approval = await actionDialog({ title: t('approval.emergencyDisable'), summary: t('approval.confirmDisable', { key }),
      preview: reviewPreview(f?.preview, 'action-review-preview'), reasonLabel: t('prompt.rollbackReason'), confirmLabel: t('approval.emergencyDisable') });
    if (!approval) return;
    run(() => api(kp + '/emergency-disable', { method: 'POST', body: { reason: approval.reason } }),
      r => t('approval.emergencyDone', { id: r.reviewId }));
  };
  $('#flag-revs', m).addEventListener('click', async ev => {
    const b = ev.target.closest('[data-rb], [data-er]');
    if (!b) return;
    const emergency = b.hasAttribute('data-er');
    const rev = Number(emergency ? b.dataset.er : b.dataset.rb);
    const approval = await actionDialog({ title: t('flags.rollbackTo', { n: rev }), summary: t('confirm.rollback', { rev }),
      reasonLabel: t('prompt.rollbackReason'), confirmLabel: t('flags.rollbackTo', { n: rev }) });
    if (!approval) return;
    if (!emergency) {
      requestChange('flag.rollback', key, { revision: rev }, approval.reason).catch(e => reportError(e));
      return;
    }
    run(() => api(kp + '/rollback', { method: 'POST', body: { revision: rev, reason: approval.reason, emergency: true } }),
      r => t('approval.emergencyDone', { id: r.reviewId }));
  });
  $('#gff-run', m).onclick = async () => {
    const doc = $('#gff-doc', m).value;
    if (!doc.trim()) { toast(t('toast.pasteGff'), false); return; }
    try {
      const res = await api(kp + '/import-gff', {
        method: 'POST',
        body: { document: doc, key: $('#gff-key', m).value.trim() || key, salt: $('#gff-salt', m).value.trim() },
      });
      const warns = (res && res.warnings) || [];
      $('#gff-warn', m).innerHTML = '<div class="banner warn"><b>' + esc(t('flags.importWarnings', { n: warns.length })) + '</b><ul>' +
        (warns.map(w => '<li>' + esc(typeof w === 'string' ? w : JSON.stringify(w)) + '</li>').join('') || '<li>' + esc(t('misc.none')) + '</li>') +
        '</ul>' + (res && res.note ? '<div class="muted">' + esc(res.note) + '</div>' : '') + '</div>';
      toast(t('toast.imported'));
      const nf = await api(kp); // refresh editor with the imported draft, keep warnings visible
      if (nf && nf.draft) $('#flag-json', m).value = JSON.stringify(nf.draft.content, null, 2);
      $('#flag-revs', m).innerHTML = revisionsTable(nf);
    } catch (e) { reportError(e); }
  };
}

// ---------------------------------------------------------------------------
// View: Segments
// ---------------------------------------------------------------------------
function segmentTemplate(key) { return { schemaVersion: 1, key, constraints: [], segments: [] }; }

async function viewSegments(m) {
  if (!needScope(m)) return;
  S.segDrafts = (await api(scopePath() + '/segments')) || [];
  if (S.segKey) return viewSegmentDetail(m);
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('seg.title')) + '</h2><span class="muted">' + esc(t('seg.count', { n: S.segDrafts.length })) + '</span>' +
    '<input class="list-search" type="search" placeholder="' + esc(t('list.search')) + '" aria-label="' + esc(t('list.search')) + '"><button class="btn primary sm" id="seg-new">' + esc(t('seg.new')) + '</button></div>' +
    '<table class="grid"><thead><tr><th>' + esc(t('th.key')) + '</th><th>' + esc(t('th.state')) + '</th><th>' + esc(t('th.risk')) + '</th><th>' + esc(t('th.updatedBy')) + '</th><th>' + esc(t('th.updated')) + '</th></tr></thead><tbody>' +
    (S.segDrafts.map(d =>
      '<tr><td class="mono"><button class="row-link" type="button" data-seg="' + esc(d.key) + '">' + esc(d.key) + '</button></td><td>' +
      chip(d.state, DRAFT_CHIP[d.state] || 'gray') + '</td><td>' + esc(d.risk || '—') + '</td><td>' +
      esc(d.updatedBy || '—') + '</td><td class="nowrap">' + esc(fmtTime(d.updatedAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">' + esc(t('seg.empty')) + '</td></tr>') +
    '</tbody></table>';
  bindListSearch(m);
  $('#seg-new', m).onclick = () => {
    const key = (prompt(t('prompt.newSegKey')) || '').trim();
    if (!key) return;
    S.segKey = key; route();
  };
  $('tbody', m).addEventListener('click', ev => {
    const tr = ev.target.closest('[data-seg]');
    if (tr) { S.segKey = tr.dataset.seg; route(); }
  });
}

async function viewSegmentDetail(m) {
  const key = S.segKey;
  const kp = scopePath() + '/segments/' + encodeURIComponent(key);
  const d = S.segDrafts.find(x => x.key === key);
  const content = (d && d.content) || segmentTemplate(key);
  let preview = null;
  if (d) preview = await api(kp + '/preview');
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="seg-back">' + esc(t('seg.back')) + '</button>' +
    '<h2 class="mono">' + esc(key) + '</h2>' +
    (d ? chip(d.state, DRAFT_CHIP[d.state] || 'gray') +
      ' <span class="muted">' + esc(t('flags.riskMeta', { risk: d.risk || '—', by: d.updatedBy || '—', time: fmtTime(d.updatedAt) })) + '</span>'
      : chip(t('chip.new'), 'gray')) + '</div>' +
    '<section class="card"><h3>' + esc(t('seg.draft')) + '</h3>' +
    '<textarea id="seg-json" class="json" rows="18" spellcheck="false"></textarea>' +
    '<div class="row"><button class="btn sm" id="seg-validate">' + esc(t('btn.validateJson')) + '</button><span id="seg-valid" class="muted grow"></span>' +
    '<label class="inline">' + esc(t('label.risk')) + ' <select id="seg-risk"><option value="normal">normal</option><option value="high">high</option></select></label></div>' +
    '<div class="row actions">' +
    '<button class="btn primary" id="seg-save">' + esc(t('btn.saveDraft')) + '</button>' +
    '<button class="btn" id="seg-submit">' + esc(t('btn.submit')) + '</button>' +
    '<button class="btn" id="seg-approve">' + esc(t('btn.approve')) + '</button>' +
    '<button class="btn" id="seg-publish">' + esc(t('btn.publish')) + '</button></div></section>' + reviewPreview(preview);
  $('#seg-json', m).value = JSON.stringify(content, null, 2);
  if (d && d.risk) $('#seg-risk', m).value = d.risk;
  $('#seg-back', m).onclick = () => { if (!confirmLeaveEditor()) return; S.segKey = null; route(); };
  $('#seg-validate', m).onclick = () => {
    const out = $('#seg-valid', m);
    try {
      const c = JSON.parse($('#seg-json', m).value);
      const hasRules = c && ((Array.isArray(c.constraints) && c.constraints.length > 0) ||
        (Array.isArray(c.segments) && c.segments.length > 0));
      out.className = (c && c.key === key && hasRules) ? 'ok-text grow' : 'warn-text grow';
      out.textContent = !c || c.key !== key ? t('validate.keyShould', { key }) :
        !hasRules ? t('validate.segmentNonEmpty') : t('validate.valid');
    } catch (e) { out.className = 'err-text grow'; out.textContent = t('validate.invalidJson', { msg: e.message }); }
  };
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#seg-json', m).value); }
    catch (e) { toast(t('toast.invalidJson', { msg: e.message }), false); return null; }
    if (!c || c.key !== key) { toast(t('toast.keyMismatch', { key }), false); return null; }
    if (!((Array.isArray(c.constraints) && c.constraints.length > 0) ||
      (Array.isArray(c.segments) && c.segments.length > 0))) {
      toast(t('validate.segmentNonEmpty'), false); return null;
    }
    return c;
  };
  $('#seg-save', m).onclick = () => {
    const content2 = readJSON();
    if (!content2) return;
    run(() => api(kp + '/draft', { method: 'PUT', body: { content: content2, risk: $('#seg-risk', m).value } }), t('toast.segSaved'), true);
  };
  $('#seg-submit', m).onclick = () => {
    const reason = prompt(t('approval.reason'), '');
    if (!reason) { toast(t('toast.reasonRequired'), false); return; }
    run(() => api(kp + '/submit', { method: 'POST', body: { reason } }), t('toast.segSubmitted'));
  };
  $('#seg-approve', m).onclick = () => run(() => api(kp + '/approve', { method: 'POST', body: {} }), t('toast.segApproved'));
  $('#seg-publish', m).onclick = async () => {
    const approval = await actionDialog({ title: t('btn.publish'), summary: t('action.flagSummary', { key }),
      preview: reviewPreview(preview, 'action-review-preview'), reasonLabel: t('prompt.publishNote'), reasonRequired: false, confirmLabel: t('btn.publish') });
    if (!approval) return;
    run(() => api(kp + '/publish', { method: 'POST', body: { note: approval.reason } }), t('toast.segPublished'));
  };
  return Promise.resolve();
}

// ---------------------------------------------------------------------------
// View: Experiments
// ---------------------------------------------------------------------------
const NS_HOUR = 3.6e12, NS_DAY = 8.64e13;

async function viewExperiments(m) {
  if (!needScope(m)) return;
  if (S.expCreate) return viewExpCreate(m);
  if (S.expId) return viewExpDetail(m);
  const exps = (await api(scopePath() + '/experiments')) || [];
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('exp.title')) + '</h2><span class="muted">' + esc(exps.length) + '</span>' +
    '<input class="list-search" type="search" placeholder="' + esc(t('list.search')) + '" aria-label="' + esc(t('list.search')) + '"><button class="btn primary sm" id="exp-new">' + esc(t('exp.new')) + '</button></div>' +
    '<table class="grid"><thead><tr><th>' + esc(t('th.id')) + '</th><th>' + esc(t('th.flag')) + '</th><th>' + esc(t('th.unit')) + '</th><th>' + esc(t('th.state')) + '</th><th>' + esc(t('th.variants')) + '</th><th>' + esc(t('th.epoch')) + '</th><th>' + esc(t('th.created')) + '</th></tr></thead><tbody>' +
    (exps.map(e =>
      '<tr><td class="mono"><button class="row-link" type="button" data-exp="' + esc(e.id) + '">' + esc(e.id) + '</button></td><td class="mono">' +
      esc(e.flagKey) + '</td><td class="mono">' + esc(e.unit) + '</td><td>' +
      chip(e.state, EXP_CHIP[e.state] || 'gray') + '</td><td>' + esc((e.variants || []).join(', ')) +
      '</td><td>' + esc(e.epoch || 0) + '</td><td class="nowrap">' + esc(fmtTime(e.createdAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="7" class="muted">' + esc(t('exp.empty')) + '</td></tr>') +
    '</tbody></table>';
  bindListSearch(m);
  $('#exp-new', m).onclick = () => { S.expCreate = true; S.expTab = 'summary'; route(); };
  $('tbody', m).addEventListener('click', ev => {
    const tr = ev.target.closest('[data-exp]');
    if (tr) { S.expId = tr.dataset.exp; S.expTab = 'summary'; route(); }
  });
}

async function viewExpCreate(m) {
  const metricPage = await api(wsPath() + '/metrics?status=published&limit=200');
  const publishedMetrics = metricPage.items || [];
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="exp-back">' + esc(t('exp.back')) + '</button><h2>' + esc(t('exp.newTitle')) + '</h2></div>' +
    '<section class="card">' +
    '<div class="form-grid">' +
      '<label class="field">' + esc(t('exp.flagKey')) + '<input id="x-flag" placeholder="my-flag"></label>' +
      '<label class="field">' + esc(t('exp.unit')) + '<input id="x-unit" value="user_id"></label>' +
      '<label class="field">' + esc(t('exp.layer')) + '<input id="x-layer"></label>' +
      '<label class="field">' + esc(t('exp.elig')) + '<input id="x-elig" type="number" step="1" min="0" max="100"></label>' +
	  '<label class="field">' + esc(t('exp.allocationMode')) + '<select id="x-mode"><option value="fixed-split">fixed-split</option><option value="bandit">bandit</option></select></label>' +
      '<label class="field">' + esc(t('exp.exposureMode')) + '<select id="x-exposure-mode"><option value="onDecision">' + esc(t('exp.onDecision')) + '</option><option value="onRender">' + esc(t('exp.onRender')) + '</option></select></label>' +
    '</div>' +
    '<p class="muted" id="x-exposure-help">' + esc(t('exp.onDecisionHelp')) + '</p>' +
    '<h4>' + t('exp.variantsHead') + '</h4>' +
    '<div id="x-vars"></div><button class="btn sm" id="x-add-var">' + esc(t('exp.addVariant')) + '</button>' +
    '<h4>' + esc(t('exp.primaryMetric')) + '</h4>' +
    '<label class="field">' + esc(t('metrics.selectPrimary')) + '<select id="x-mref"><option value="">' + esc(t('metrics.inline')) + '</option>' +
      publishedMetrics.map(d => '<option value="' + esc(d.key + '/v' + d.version) + '">' + esc(d.displayName + ' · ' + d.key + ' v' + d.version) + '</option>').join('') + '</select></label>' +
    '<div class="form-grid">' +
      '<label class="field">' + esc(t('exp.metricKey')) + '<input id="x-mkey" placeholder="checkout_success"></label>' +
      '<label class="field">' + esc(t('th.type')) + '<select id="x-mtype"><option>proportion</option><option>mean</option><option>ratio</option><option>quantile</option><option>funnel</option><option>retention</option></select></label>' +
      '<label class="field">' + esc(t('exp.betterDir')) + '<select id="x-mdir"><option>increase</option><option>decrease</option></select></label>' +
    '</div>' +
    '<div class="form-grid" id="x-metric-adv">' +
      '<label class="field" id="x-numkey-wrap">' + esc(t('exp.numeratorKey')) + ' <span class="muted">' + esc(t('exp.numeratorHint')) + '</span><input id="x-numkey" placeholder="revenue"></label>' +
      '<label class="field" id="x-denkey-wrap">' + esc(t('exp.denominatorKey')) + ' <span class="muted">' + esc(t('exp.denominatorHint')) + '</span><input id="x-denkey" placeholder="sessions"></label>' +
      '<label class="field" id="x-qlevel-wrap">' + esc(t('exp.quantileLevel')) + ' <span class="muted">' + esc(t('exp.quantileHint')) + '</span><input id="x-qlevel" type="number" step="0.01" min="0.01" max="0.99" placeholder="0.95"></label>' +
      '<label class="field" id="x-covkey-wrap">' + esc(t('exp.covariateKey')) + ' <span class="muted">' + esc(t('exp.covariateHint')) + '</span><input id="x-covkey" placeholder="pre_period_value"></label>' +
      '<label class="field" id="x-journey-wrap">' + esc(t('exp.journeySpec')) + '<textarea id="x-journey" rows="3" placeholder="{&quot;steps&quot;:[&quot;view&quot;,&quot;pay&quot;],&quot;dedupUnit&quot;:&quot;user&quot;,&quot;stepTimeout&quot;:3600000000000}"></textarea></label>' +
    '</div>' +
	'<div class="form-grid" id="x-bandit" style="display:none">' +
	  '<label class="field">' + esc(t('exp.policyVersion')) + '<input id="x-policy-version" value="bandit-v1"></label>' +
	  '<label class="field">' + esc(t('exp.strategy')) + '<select id="x-strategy"><option value="thompson">Thompson</option><option value="epsilon-greedy">ε-greedy</option></select></label>' +
	  '<label class="field">Prior α<input id="x-prior-a" type="number" step="0.1" value="1"></label>' +
	  '<label class="field">Prior β<input id="x-prior-b" type="number" step="0.1" value="1"></label>' +
	  '<label class="field">ε<input id="x-epsilon" type="number" step="0.01" value="0.1"></label>' +
	  '<label class="field">' + esc(t('exp.feedbackDelayD')) + '<input id="x-feedback-delay" type="number" step="1" value="11"></label>' +
	'</div>' +
    '<h4>' + esc(t('exp.guardrails')) + '</h4>' +
    '<div id="x-guards"></div><button class="btn sm" id="x-add-guard">' + esc(t('exp.addGuardrail')) + '</button>' +
    '<h4>' + t('exp.statsHead') + '</h4>' +
    '<div class="form-grid">' +
      '<label class="field">Alpha<input id="x-alpha" type="number" step="0.001" value="0.05"></label>' +
      '<label class="field">Power<input id="x-power" type="number" step="0.01" value="0.8"></label>' +
      '<label class="field">MDE<input id="x-mde" type="number" step="0.001" value="0.01"></label>' +
      '<label class="field">' + esc(t('exp.minSample')) + '<input id="x-minsample" type="number" value="1000"></label>' +
      '<label class="field">' + esc(t('exp.minRuntimeH')) + '<input id="x-minruntime" type="number" step="0.5" value="168"></label>' +
      '<label class="field">' + esc(t('exp.windowD')) + '<input id="x-window" type="number" step="1" value="7"></label>' +
    '</div>' +
    '<label class="field">' + esc(t('exp.hypothesis')) + '<textarea id="x-hyp" rows="2"></textarea></label>' +
    '<label class="field">' + esc(t('exp.holmFamily')) + '<textarea id="x-holm" rows="5" placeholder="{&quot;id&quot;:&quot;primary&quot;,&quot;hypotheses&quot;:[...]}"></textarea></label>' +
    '<p class="muted">' + esc(t('exp.holmHelp')) + '</p>' +
    '<div class="row"><button class="btn primary" id="x-create">' + esc(t('exp.create')) + '</button></div></section>';
  const vars = $('#x-vars', m), guards = $('#x-guards', m);
  const addVar = (name, p) => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="v-name" placeholder="' + esc(t('ph.variantName')) + '">' +
      '<input class="v-prop" type="number" step="1" min="0" max="100" placeholder="' + esc(t('ph.percent')) + '">' +
      '<button class="btn sm" title="' + esc(t('btn.remove')) + '">✕</button>';
    $('.v-name', row).value = name || '';
    $('.v-prop', row).value = p ?? '';
    $('button', row).onclick = () => row.remove();
    vars.appendChild(row);
  };
  addVar('control', 50);
  addVar('treatment', 50);
  $('#x-add-var', m).onclick = () => addVar();
  // Show only the advanced metric fields relevant to the selected type.
  const mtype = $('#x-mtype', m);
  const syncMetricAdv = () => {
    const t = mtype.value;
    const show = (id, on) => { const el = $('#' + id, m); if (el) el.style.display = on ? '' : 'none'; };
    show('x-numkey-wrap', t === 'ratio');
    show('x-denkey-wrap', t === 'ratio');
    show('x-qlevel-wrap', t === 'quantile');
    show('x-covkey-wrap', t === 'mean');
    show('x-journey-wrap', t === 'funnel' || t === 'retention');
  };
  mtype.onchange = syncMetricAdv;
  syncMetricAdv();
  $('#x-mref', m).onchange = ev => {
    const def = publishedMetrics.find(d => d.key + '/v' + d.version === ev.target.value);
    $$('#x-mkey, #x-mtype, #x-mdir, #x-numkey, #x-denkey, #x-qlevel, #x-covkey, #x-journey', m)
      .forEach(input => { input.disabled = !!def; });
    if (!def) return;
    $('#x-mkey', m).value = def.spec.key;
    $('#x-mtype', m).value = def.spec.type;
    $('#x-mdir', m).value = def.spec.direction;
    $('#x-numkey', m).value = def.spec.numeratorKey || '';
    $('#x-denkey', m).value = def.spec.denominatorKey || '';
    $('#x-qlevel', m).value = def.spec.quantileLevel || '';
    $('#x-covkey', m).value = def.spec.covariateKey || '';
    if (def.spec.funnel || def.spec.retention) $('#x-journey', m).value = JSON.stringify(def.spec.funnel || def.spec.retention);
    syncMetricAdv();
  };
	$('#x-mode', m).onchange = ev => { $('#x-bandit', m).style.display = ev.target.value === 'bandit' ? '' : 'none'; };
  $('#x-exposure-mode', m).onchange = ev => {
    $('#x-exposure-help', m).textContent = t(ev.target.value === 'onRender' ? 'exp.onRenderHelp' : 'exp.onDecisionHelp');
  };
  $('#x-add-guard', m).onclick = () => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="g-key" placeholder="metricKey">' +
      '<select class="g-type"><option>proportion</option><option>mean</option><option>funnel</option><option>retention</option></select>' +
      '<select class="g-dir"><option value="increase">' + esc(t('exp.increaseGood')) + '</option><option value="decrease">' + esc(t('exp.decreaseGood')) + '</option></select>' +
      '<input class="g-reg" type="number" step="0.01" placeholder="' + esc(t('ph.maxReg')) + '">' +
      '<input class="g-min" type="number" placeholder="minSample">' +
      '<textarea class="g-journey" rows="2" placeholder="' + esc(t('exp.journeySpec')) + ' JSON"></textarea>' +
      '<button class="btn sm">✕</button>';
    $('button', row).onclick = () => row.remove();
    guards.appendChild(row);
  };
  $('#exp-back', m).onclick = () => { S.expCreate = false; route(); };
  $('#x-create', m).onclick = async () => {
    const variants = $$('.vrow', vars)
      .map(r => ({ name: $('.v-name', r).value.trim(), prop: Number($('.v-prop', r).value) }))
      .filter(v => v.name);
    if (variants.length < 2) { toast(t('toast.need2variants'), false); return; }
    const propSum = variants.reduce((a, v) => a + (v.prop || 0), 0);
    if (Math.abs(propSum - 100) > 0.01) { toast(t('toast.propSum', { sum: propSum }), false); return; }
    const proportions = {};
    variants.forEach(v => { proportions[v.name] = v.prop; });
    const guardrails = $$('.vrow', guards).map(r => ({
      metricKey: $('.g-key', r).value.trim(),
      type: $('.g-type', r).value,
      direction: $('.g-dir', r).value,
      maxRelativeRegression: Number($('.g-reg', r).value || 0),
      minSample: Number($('.g-min', r).value || 0),
    })).filter(g => g.metricKey);
    try {
      $$('.vrow', guards).filter(r => $('.g-key', r).value.trim()).forEach((r, i) => {
        const g = guardrails[i];
        if (g.type === 'funnel' || g.type === 'retention') g[g.type] = JSON.parse($('.g-journey', r).value);
      });
    } catch { toast(t('exp.journeyInvalid'), false); return; }
    const pm = { key: $('#x-mkey', m).value.trim(), type: $('#x-mtype', m).value, direction: $('#x-mdir', m).value };
    if (pm.type === 'ratio') {
      const num = $('#x-numkey', m).value.trim();
      if (num) pm.numeratorKey = num;
      pm.denominatorKey = $('#x-denkey', m).value.trim();
    } else if (pm.type === 'quantile') {
      pm.quantileLevel = Number($('#x-qlevel', m).value);
    } else if (pm.type === 'mean') {
      const cov = $('#x-covkey', m).value.trim();
      if (cov) pm.covariateKey = cov;
    } else if (pm.type === 'funnel' || pm.type === 'retention') {
      try { pm[pm.type] = JSON.parse($('#x-journey', m).value); }
      catch { toast(t('exp.journeyInvalid'), false); return; }
    }
    const body = {
      flagKey: $('#x-flag', m).value.trim(),
      unit: $('#x-unit', m).value.trim(),
      variants: variants.map(v => v.name),
      proportions,
      primaryMetric: pm,
      guardrails,
      alpha: Number($('#x-alpha', m).value),
      power: Number($('#x-power', m).value),
      mde: Number($('#x-mde', m).value),
      minSamplePerArm: Number($('#x-minsample', m).value),
      minRuntime: Math.round(Number($('#x-minruntime', m).value) * NS_HOUR),
      analysisWindow: Math.round(Number($('#x-window', m).value) * NS_DAY),
      hypothesis: $('#x-hyp', m).value.trim(),
      exposureMode: $('#x-exposure-mode', m).value,
    };
    const selectedMetric = publishedMetrics.find(d => d.key + '/v' + d.version === $('#x-mref', m).value);
    if (selectedMetric) body.primaryMetricRef = { key: selectedMetric.key, version: selectedMetric.version };
	body.allocationMode = $('#x-mode', m).value;
	if (body.allocationMode === 'bandit') {
	  if (variants.length !== 2 || pm.type !== 'proportion') { toast(t('exp.banditRequiresProportion'), false); return; }
	  body.bandit = { policyVersion: $('#x-policy-version', m).value.trim(), strategy: $('#x-strategy', m).value,
	    priorAlpha: Number($('#x-prior-a', m).value), priorBeta: Number($('#x-prior-b', m).value),
	    epsilon: Number($('#x-epsilon', m).value), feedbackDelay: Math.round(Number($('#x-feedback-delay', m).value) * NS_DAY) };
	}
    if (!body.flagKey || !body.primaryMetric.key) { toast(t('toast.flagMetricRequired'), false); return; }
    if (pm.type === 'ratio' && !pm.denominatorKey) { toast(t('toast.ratioDenominator'), false); return; }
    if (pm.type === 'quantile' && !(pm.quantileLevel > 0 && pm.quantileLevel < 1)) { toast(t('toast.quantileLevel'), false); return; }
    const layer = $('#x-layer', m).value.trim();
    if (layer) body.layer = layer;
    const elig = $('#x-elig', m).value;
    if (elig !== '') body.eligibility = Number(elig);
    const holmText = $('#x-holm', m).value.trim();
    if (holmText) {
      try { body.holmFamily = JSON.parse(holmText); }
      catch (_) { toast(t('exp.holmInvalid'), false); return; }
    }
    try {
      const exp = await api(scopePath() + '/experiments', { method: 'POST', body });
      toast(t('toast.expCreated', { id: exp.id }));
      S.expCreate = false; S.expId = exp.id; route();
    } catch (e) { reportError(e); }
  };
  return Promise.resolve();
}

async function viewExpDetail(m) {
  const id = S.expId;
  const p = '/api/v1/admin/experiments/' + encodeURIComponent(id);
  const e = await api(p);
  if (S.expTab === 'diagnostics') return viewExpDiagnostics(m, e, p);
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="exp-back">' + esc(t('exp.back')) + '</button>' +
    '<h2 class="mono">' + esc(e.id) + '</h2>' + chip(e.state, EXP_CHIP[e.state] || 'gray') +
    '<span class="muted">' + esc(t('exp.meta', { flag: e.flagKey, rev: e.flagRevision || 0, epoch: e.epoch || 0, by: e.createdBy || '—' })) + '</span>' +
    '<button class="btn" id="exp-diagnostics">' + esc(t('exp.openDiagnostics')) + '</button></div>' +
    '<div class="cols"><section class="card"><h3>' + esc(t('exp.prereg')) + '</h3><table class="kv">' +
      '<tr><th>' + esc(t('th.unit')) + '</th><td class="mono">' + esc(e.unit) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.layerShort')) + '</th><td>' + esc(e.layer || '—') + (e.layerSlot ? ' <span class="muted">' + esc(t('exp.slot', { json: JSON.stringify(e.layerSlot) })) + '</span>' : '') + '</td></tr>' +
      '<tr><th>' + esc(t('exp.eligShort')) + '</th><td>' + (e.eligibility ? esc(e.eligibility) + '%' : '100%') + '</td></tr>' +
      '<tr><th>' + esc(t('th.variants')) + '</th><td>' + (e.variants || []).map(v => '<span class="mono">' + esc(v) + '</span> (' + esc((e.proportions || {})[v] ?? '?') + ')').join(', ') + '</td></tr>' +
      '<tr><th>' + esc(t('exp.primaryShort')) + '</th><td class="mono">' + esc(e.primaryMetric.key) + '</td></tr>' +
      '<tr><th></th><td>' + esc(t('exp.betterEq', { type: e.primaryMetric.type, dir: e.primaryMetric.direction })) + '</td></tr>' +
      '<tr><th>α / power / MDE</th><td>' + esc(e.alpha) + ' / ' + esc(e.power) + ' / ' + esc(e.mde) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.minSampleArm')) + '</th><td>' + esc(e.minSamplePerArm) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.minRuntime')) + '</th><td>' + esc(fmtDurNs(e.minRuntime)) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.window')) + '</th><td>' + esc(fmtDurNs(e.analysisWindow)) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.exposureMode')) + '</th><td>' + esc(t(e.exposureMode === 'onRender' ? 'exp.onRender' : 'exp.onDecision')) + '</td></tr>' +
      '<tr><th>' + esc(t('exp.method')) + '</th><td>' + esc(e.analysisMethod || '—') + '</td></tr>' +
	  '<tr><th>' + esc(t('exp.allocationMode')) + '</th><td>' + esc(e.allocationMode || 'fixed-split') +
	  (e.bandit ? ' · ' + esc(e.bandit.policyVersion) + ' · ' + esc(e.bandit.strategy) : '') + '</td></tr>' +
    '</table><div class="muted">' + esc(t('exp.hypothesisLine', { h: e.hypothesis || '—' })) + '</div>' +
    ((e.guardrails || []).length
      ? '<h4>' + esc(t('exp.guardrails')) + '</h4><table class="grid"><thead><tr><th>' + esc(t('th.metric')) + '</th><th>' + esc(t('th.type')) + '</th><th>' + esc(t('th.goodDir')) + '</th><th>' + esc(t('th.maxReg')) + '</th><th>' + esc(t('th.minSample')) + '</th></tr></thead><tbody>' +
        e.guardrails.map(g => '<tr><td class="mono">' + esc(g.metricKey) + '</td><td>' + esc(g.type) + '</td><td>' +
          esc(g.direction || 'increase') + '</td><td>' + esc(g.maxRelativeRegression) + '</td><td>' + esc(g.minSample) + '</td></tr>').join('') +
        '</tbody></table>'
      : '') +
    (e.holmFamily ? '<h4>' + esc(t('exp.holmFamily')) + '</h4><p class="muted">' + esc(t('exp.holmHelp')) +
      ' · ' + esc(e.registrationVersion || '—') + '</p><ul class="plain">' +
      (e.holmFamily.hypotheses || []).map(h => '<li>' + esc(h.id) + ': ' + esc(h.metric.key) +
        ' (' + esc(h.control) + ' → ' + esc(h.treatment) + ')</li>').join('') + '</ul>' : '') +
    '</section>' +
    '<section class="card"><h3>' + esc(t('exp.lifecycle')) + '</h3>' +
      '<div class="row actions" style="border:0;padding:0;margin:0">' +
      '<button class="btn" id="exp-review">' + esc(t('btn.review')) + '</button>' +
      '<button class="btn" id="exp-bind">' + esc(t('btn.bind')) + '</button></div>' +
      '<p class="muted">' + t('exp.bindHint') + '</p>' +
      '<div class="row"><select id="exp-to">' +
      '<option value="running">running</option><option value="paused">paused</option>' +
      '<option value="stopped">stopped</option><option value="analyzed">analyzed</option>' +
      '<option value="archived">archived</option></select>' +
      '<input id="exp-reason" class="grow" placeholder="' + esc(t('ph.reasonRequired')) + '">' +
      '<button class="btn primary" id="exp-transition">' + esc(t('btn.transition')) + '</button></div>' +
      '<h3>' + esc(t('exp.report')) + '</h3>' +
      '<div class="row"><button class="btn" id="exp-report">' + esc(t('btn.loadReport')) + '</button>' +
      (can('report.read') ? '<button class="btn" id="exp-export-csv">' + esc(t('report.exportCsv')) + '</button>' +
        '<button class="btn" id="exp-export-json">' + esc(t('report.exportJson')) + '</button>' : '') +
      '<label class="inline"><input type="checkbox" id="exp-auto"> ' + esc(t('exp.autoRefresh')) + '</label></div>' +
      '<div id="report-box"><p class="muted">' + esc(t('exp.notLoaded')) + '</p></div>' +
    '</section></div>' +
    '<section class="card"><h3>' + esc(t('ro.title')) + ' <span class="muted">' + esc(t('ro.subtitle')) + '</span></h3>' +
    '<div id="rollout-box">' + renderRollout(e) + '</div></section>';
  $('#exp-back', m).onclick = () => { S.expId = null; S.expTab = 'summary'; route(); };
  $('#exp-diagnostics', m).onclick = () => { S.expTab = 'diagnostics'; route(); };
  const act = (path, body, msg) => run(() => api(p + path, { method: 'POST', body }), msg);
  $('#exp-review', m).onclick = () => {
    const note = prompt(t('prompt.reviewNote'), '') || '';
    act('/review', { note }, t('toast.reviewRecorded'));
  };
  $('#exp-bind', m).onclick = () => act('/bind', {}, t('toast.expBound'));
  $('#exp-transition', m).onclick = () => {
    const to = $('#exp-to', m).value;
    const reason = $('#exp-reason', m).value.trim();
    if (!reason) { toast(t('toast.transitionReason'), false); return; }
    if (!confirm(t('confirm.transition', { to }))) return;
    act('/transition', { to, reason }, t('toast.transitioned', { to }));
  };
  let lastReport = null;
  const loadReport = async () => {
    const box = $('#report-box', m);
    if (!box) return false;
    try {
      const report = await api(p + '/report');
      if (S.expId !== id || S.view !== 'experiments') return false;
      lastReport = report;
      renderReport(box, report);
      return true;
    } catch (err) {
      if (lastReport) {
        renderReport(box, lastReport);
        box.innerHTML = '<div class="banner warn" role="status">' + esc(t('report.stale')) + '</div>' + errBanner(err) + box.innerHTML;
      } else box.innerHTML = errBanner(err);
      return false;
    }
  };
  $('#exp-report', m).onclick = loadReport;
  if (can('report.read')) {
    for (const format of ['csv', 'json']) {
      $('#exp-export-' + format, m).onclick = async () => {
        try { await downloadReport(p + '/report/export', format); }
        catch (err) { reportError(err); }
      };
    }
  }
  let autoRefresh = false;
  let failures = 0;
  let polling = false;
  const stopTimer = () => { if (S.timer) { clearTimeout(S.timer); S.timer = null; } };
  const poll = async () => {
    if (!autoRefresh || polling || document.visibilityState === 'hidden') return;
    polling = true;
    const ok = await loadReport();
    polling = false;
    failures = ok ? 0 : Math.min(failures + 1, 3);
    if (autoRefresh && document.visibilityState !== 'hidden' && S.expId === id && S.view === 'experiments') {
      S.timer = setTimeout(poll, Math.min(60000, 10000 * 2 ** failures));
    }
  };
  const onVisibility = () => {
    stopTimer();
    if (autoRefresh && document.visibilityState !== 'hidden') poll();
  };
  document.addEventListener('visibilitychange', onVisibility);
  S.pollCleanup = () => { autoRefresh = false; stopTimer(); document.removeEventListener?.('visibilitychange', onVisibility); };
  $('#exp-auto', m).onchange = ev => {
    autoRefresh = ev.target.checked;
    stopTimer();
    failures = 0;
    if (autoRefresh) poll();
  };
  bindRollout(m, id);
}

function metricReferenceSummary(r) {
  const ref = r.primaryMetricRef;
  return '<div class="muted">' + esc(t('report.metricVersion')) + ': ' +
    (ref ? '<span class="mono">' + esc(ref.key) + ' v' + esc(ref.version) + ' · ' + esc(shortHash(ref.definitionHash)) + '</span>' :
      esc(t('report.legacyInline'))) +
    (r.primaryAttributionWindow ? ' · ' + esc(t('report.attributionWindow')) + ': ' + esc(fmtDurNs(r.primaryAttributionWindow)) : '') +
    '</div>';
}

async function viewExpDiagnostics(m, experiment, path) {
  const d = await api(path + '/diagnostics');
  const window = d.window || {};
  const blockers = [...new Set([...(window.issues || []), ...(d.issues || [])])];
  const totalAssignments = (d.perVariant || []).reduce((sum, v) => sum + (v.assignments || 0), 0);
  const totalExposures = (d.perVariant || []).reduce((sum, v) => sum + (v.exposures || 0), 0);
  m.innerHTML = '<div class="view-head"><button class="btn sm" id="diag-back">' + esc(t('exp.back')) + '</button>' +
    '<h2>' + esc(t('exp.diagnosticsTitle')) + '</h2>' +
    '<span class="mono">' + esc(experiment.id) + '</span>' + chip(experiment.state, EXP_CHIP[experiment.state] || 'gray') +
    '<button class="btn" id="diag-refresh">' + esc(t('btn.refresh')) + '</button></div>' +
    '<section class="card"><h3>' + esc(t('exp.diagnosticsStatus')) + '</h3>' +
      '<p>' + esc(t('exp.exposureMode')) + ': ' + esc(t(d.exposureMode === 'onRender' ? 'exp.onRender' : 'exp.onDecision')) +
      ' · epoch ' + esc(d.epoch) + ' · ' + esc(t('report.windowVersion')) + ': <span class="mono">' + esc(window.version || '—') + '</span></p>' +
      '<p>' + esc(t('report.watermark')) + ': ' + esc(fmtTime(window.watermark)) +
      ' · ' + esc(t('report.backend')) + ': ' + esc(window.backend || '—') +
      ' · ' + esc(t('exp.lastCollected')) + ': ' + esc(fmtTime(d.lastCollectedAt)) + '</p>' +
      '<p>' + chip(window.complete ? t('report.complete') : t('report.incomplete'), window.complete ? 'green' : 'amber') +
      (window.recomputable ? '' : ' ' + chip(t('report.notRecomputable'), 'amber')) + '</p>' +
      (totalAssignments + totalExposures === 0 ? '<p class="muted">' + esc(t('exp.noDiagnosticEvents')) + '</p>' : '') +
    '</section>' +
    '<section class="card"><h3>' + esc(t('exp.diagnosticIssues')) + '</h3>' +
      (blockers.length ? '<ul>' + blockers.map(i => '<li>' + esc(i) + '</li>').join('') + '</ul>' :
        '<p class="muted">' + esc(t('exp.noDiagnosticIssues')) + '</p>') +
    '</section>' +
    '<section class="card">' + renderDiagnostics(d) + '</section>';
  $('#diag-back', m).onclick = () => { S.expTab = 'summary'; route(); };
  $('#diag-refresh', m).onclick = () => route();
}

function renderDiagnostics(d) {
  if (!d) return '';
  const collectionLabels = {
    invalid_event: t('collect.code.invalid_event'),
    unknown_experiment: t('collect.code.unknown_experiment'),
    stale_epoch: t('collect.code.stale_epoch'),
    assignment_mismatch: t('collect.code.assignment_mismatch'),
    missing_exposure: t('collect.code.missing_exposure'),
    missing_unit: t('collect.code.missing_unit'),
    invalid_exposure_token: t('collect.code.invalid_exposure_token'),
    expired_exposure_token: t('collect.code.expired_exposure_token'),
    late_event: t('collect.code.late_event'),
    future_event: t('collect.code.future_event'),
  };
  const srm = (label, value) => '<div>' + esc(label) + ': ' +
    (value ? 'p = ' + esc(value.pValue ?? '—') + ' ' +
      chip(value.healthy ? t('chip.noSrm') : t('chip.srmDetected'), value.healthy ? 'green' : 'red') :
      esc(t('report.srmInsufficient'))) + '</div>';
  const srmSummary = d.window?.version === 'bandit-feedback-v1'
    ? '<p class="muted">' + esc(t('report.banditSrmSeparate')) + '</p>'
    : '<div class="row">' + srm(t('report.assignmentSrm'), d.assignmentSrm) +
      srm(t('report.exposureSrm'), d.exposureSrm) + '</div>';
  const rejected = Object.entries(d.rejectedByReason || {}).sort(([a], [b]) => a.localeCompare(b));
  return '<section aria-label="' + esc(t('report.deliveryDiagnostics')) + '">' +
    '<h4>' + esc(t('report.deliveryDiagnostics')) + '</h4>' +
    '<p class="muted">' + esc(t('report.deliveryExplanation')) + '</p>' +
    srmSummary +
    '<table class="grid diagnostic-table"><thead><tr><th>' + esc(t('th.variant')) + '</th><th>' +
      esc(t('report.assignments')) + '</th><th>' + esc(t('report.exposures')) +
      '</th><th>' + esc(t('report.unexposed')) + '</th><th>' +
      esc(t('th.converted')) + '</th></tr></thead><tbody>' +
      (d.perVariant || []).map(v => '<tr><td class="mono">' + esc(v.variant) +
        '</td><td>' + esc(v.assignments) + '</td><td>' + esc(v.exposures) +
        '</td><td>' + esc(v.unexposed) + '</td><td>' + esc(v.conversions) + '</td></tr>').join('') +
      '</tbody></table>' +
    '<div class="diagnostic-cards">' + (d.perVariant || []).map(v =>
      '<dl class="diagnostic-card"><dt class="mono">' + esc(v.variant) + '</dt><dd>' +
      '<span>' + esc(t('report.assignments')) + '</span><strong>' + esc(v.assignments) + '</strong></dd><dd>' +
      '<span>' + esc(t('report.exposures')) + '</span><strong>' + esc(v.exposures) + '</strong></dd><dd>' +
      '<span>' + esc(t('report.unexposed')) + '</span><strong>' + esc(v.unexposed) + '</strong></dd><dd>' +
      '<span>' + esc(t('th.converted')) + '</span><strong>' + esc(v.conversions) + '</strong></dd></dl>').join('') + '</div>' +
    '<div>' + esc(t('report.crossVariantUnits')) + ': ' + esc(d.crossVariantUnits) +
      ' · ' + esc(t('report.assignmentMismatch')) + ': ' + esc(d.assignmentMismatch) + '</div>' +
    '<h5>' + esc(t('report.collectionAttempts')) + '</h5>' +
    '<p class="muted">' + esc(t('report.collectionAttemptsNote')) + '</p>' +
    '<div>' + esc(t('report.duplicateEvents')) + ': ' + esc(d.duplicateEvents || 0) + '</div>' +
    (rejected.length ? '<ul>' + rejected.map(([code, count]) => '<li>' +
      esc(collectionLabels[code] || code) + ' <span class="mono">(' + esc(code) + ')</span>: ' +
      esc(count) + '</li>').join('') + '</ul>' :
      '<p class="muted">' + esc(t('report.noRejectedEvents')) + '</p>') +
    '</section>';
}

function renderReport(box, r) {
  if (r.bandit) { renderBanditReport(box, r); return; }
  const p = r.primary, d = r.decision || {}, srm = r.srm;
  const window = r.window;
  box.innerHTML =
    (r.healthy
      ? '<div class="banner ok">' + esc(t('report.healthy', { epoch: r.epoch, state: r.state })) + '</div>'
      : '<div class="banner err"><b>' + esc(t('report.unhealthy')) + '</b><ul>' +
        (r.healthIssues || []).map(i => '<li>' + esc(i) + '</li>').join('') + '</ul></div>') +
    metricReferenceSummary(r) +
    renderDiagnostics(r.diagnostics) +
    '<div class="row">' + chip(r.minRuntimeSatisfied ? t('chip.minRuntimeOk') : t('chip.minRuntimeNo'), r.minRuntimeSatisfied ? 'green' : 'amber') +
    ' ' + chip(r.analysisMethod || '—', 'gray') + '</div>' +
    (window ? '<h4>' + esc(t('report.window')) + '</h4>' +
      '<div>' + esc(t('report.windowVersion')) + ': <span class="mono">' + esc(window.version) + '</span> · ' +
      esc(t('report.windowStart')) + ': <span class="mono">' + esc(window.start) + '</span> · ' +
      esc(t('report.windowEnd')) + ': <span class="mono">' + esc(window.end) + '</span></div>' +
      '<div>' + esc(t('report.watermark')) + ': <span class="mono">' + esc(window.watermark) + '</span> · ' +
      esc(t('report.backend')) + ': ' + esc(window.backend) + ' ' +
      chip(window.complete ? t('report.complete') : t('report.incomplete'), window.complete ? 'green' : 'amber') +
      (window.recomputable ? '' : ' ' + chip(t('report.notRecomputable'), 'amber')) + '</div>' +
      ((window.issues || []).length ? '<ul>' + window.issues.map(i => '<li>' + esc(i) + '</li>').join('') + '</ul>' : '')
    : '') +
    '<h4>' + esc(t('report.srm')) + '</h4>' +
    (srm
      ? '<div>χ² = ' + esc(srm.chiSquare ?? '—') + ' · p = ' + esc(srm.pValue ?? '—') + ' ' +
        chip(srm.healthy ? t('chip.noSrm') : t('chip.srmDetected'), srm.healthy ? 'green' : 'red') + '</div>' +
        '<pre class="pre small">' + esc(JSON.stringify({ observed: srm.observed, expected: srm.expected }, null, 2)) + '</pre>'
      : '<div class="muted">' + esc(t('report.noSrm')) + '</div>') +
    '<h4>' + esc(t('report.perVariant')) + '</h4>' +
    '<table class="grid"><thead><tr><th>' + esc(t('th.variant')) + '</th><th>' + esc(t('th.exposedUnits')) + '</th><th>' + esc(t('th.randomized')) + '</th><th>' + esc(t('th.exposures')) + '</th><th>' + esc(t('th.converted')) + '</th><th>' + esc(t('th.primaryValue')) + '</th></tr></thead><tbody>' +
    ((r.perVariant || []).map(v =>
      '<tr><td class="mono">' + esc(v.variant) + '</td><td>' + esc(v.exposedUnits) + '</td><td>' + esc(v.randomizedUnits) +
      '</td><td>' + esc(v.exposures) + '</td><td>' + esc(v.convertedUnits) + '</td><td>' + esc(v.primaryValue) + '</td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">' + esc(t('report.noData')) + '</td></tr>') +
    '</tbody></table>' +
    ((r.journeys || []).length ? '<h4>' + esc(t('report.journeys')) + '</h4>' +
      r.journeys.map(j => '<div><b>' + esc(j.metric) + '</b> · ' + esc(j.type) +
        ' · ' + esc(t('report.watermark')) + ': ' + esc(j.watermark) +
        '<pre class="pre small">' + esc(JSON.stringify(j.funnel || j.retention)) + '</pre>' +
        '<table class="grid"><thead><tr><th>' + esc(t('th.variant')) + '</th><th>' + esc(t('report.denominator')) +
        '</th><th>' + esc(t('report.numerator')) + '</th><th>' + esc(t('report.immature')) +
        '</th><th>' + esc(t('report.excluded')) + '</th></tr></thead><tbody>' +
        (j.arms || []).map(a => '<tr><td>' + esc(a.variant) + '</td><td>' + esc(a.denominator) +
          '</td><td>' + esc(a.numerator) + '</td><td>' + esc(a.immature) +
          '</td><td class="mono">' + esc(JSON.stringify(a.excluded || {})) + '</td></tr>').join('') +
        '</tbody></table></div>').join('') : '') +
    '<h4>' + esc(t('report.primary')) + (p ? ' <span class="muted">(' + esc(p.type || '') + ')</span>' : '') +
      (r.holm ? ' <span class="muted">' + esc(t('report.primaryRawNote')) + '</span>' : '') + '</h4>' +
    (p
      ? '<div>p = ' + esc(p.pValue ?? '—') + ' · diff = ' + esc(p.diff ?? '—') + ' · rel = ' + esc(p.relativeDiff ?? '—') +
        (p.relativeMeaningful === false ? ' ' + chip(t('chip.notMeaningful'), 'amber') : '') +
        ' · CI [' + esc(p.ciLow ?? '—') + ', ' + esc(p.ciHigh ?? '—') + '] ' +
        chip(p.significant ? t('chip.significant') : t('chip.notSignificant'), p.significant ? 'green' : 'gray') + '</div>'
      : '<div class="muted">' + esc(r.primaryError || t('report.noPrimary')) + '</div>') +
    (p && p.type === 'cuped' && p.cuped
      ? '<div class="muted">' + esc(t('report.cuped', {
          pct: (100 * (p.varianceReduction || 0)).toFixed(1),
          theta: (p.cuped.theta ?? 0).toFixed(4),
          rho: (p.cuped.correlation ?? 0).toFixed(3),
          raw: (p.cuped.rawDiff ?? 0).toFixed(4),
        })) + '</div>'
      : '') +
    (p && p.type === 'ratio-delta' && p.ratio
      ? '<div class="muted">ratio: A=' + esc((p.ratio.ratioA ?? 0).toFixed(4)) +
        ' · B=' + esc((p.ratio.ratioB ?? 0).toFixed(4)) + ' · SE=' + esc((p.ratio.se ?? 0).toFixed(4)) + '</div>'
      : '') +
    (p && p.type === 'quantile' && p.quantile
      ? '<div class="muted">' + esc(t('report.quantilePrefix')) + esc(p.quantile.level) + ': A=' + esc((p.quantile.quantileA ?? 0).toFixed(3)) +
        ' · B=' + esc((p.quantile.quantileB ?? 0).toFixed(3)) + ' · A CI [' + esc((p.quantile.ciALow ?? 0).toFixed(3)) +
        ', ' + esc((p.quantile.ciAHigh ?? 0).toFixed(3)) + '] (' + esc(t('report.distFree')) + ')</div>'
      : '') +
    (r.holm ? '<h4>' + esc(t('report.holm')) + '</h4><p class="muted">' + esc(t('report.holmNote')) +
      ' · ' + esc(r.holm.familyId) + ' · ' + esc(r.holm.registrationVersion) + '</p>' +
      (r.holm.error ? '<div class="banner err">' + esc(r.holm.error) + '</div>' :
        '<table class="grid"><thead><tr><th>ID</th><th>' + esc(t('th.metric')) + '</th><th>' + esc(t('report.pair')) +
        '</th><th>' + esc(t('report.rawP')) + '</th><th>' + esc(t('report.adjustedP')) + '</th><th>' +
        esc(t('report.effectiveAlpha')) + '</th><th>' + esc(t('report.rank')) + '</th><th>' + esc(t('report.decision')) +
        '</th></tr></thead><tbody>' + (r.holm.comparisons || []).map(c => '<tr><td>' + esc(c.id) + '</td><td>' +
          esc(c.metric.key) + '</td><td>' + esc(c.control) + ' → ' + esc(c.treatment) + '</td><td>' + esc(c.rawP) +
          '</td><td>' + esc(c.adjustedP) + '</td><td>' + esc(c.effectiveAlpha) + '</td><td>' + esc(c.rank) +
          '</td><td>' + chip(c.rejected ? t('chip.significant') : t('chip.notSignificant'), c.rejected ? 'green' : 'gray') +
          '</td></tr>').join('') + '</tbody></table>') : '') +
    '<h4>' + esc(t('report.sequential')) + ' <span class="muted">' + esc(t('report.sequentialNote')) + '</span></h4>' +
    (r.sequential
      ? '<div>' + esc(t('report.anytimeP')) + ' = ' + esc((r.sequential.pValue ?? 0).toExponential(3)) + ' ' +
        chip(r.sequential.significant ? t('chip.sigAnyLook') : t('chip.notSignificant'), r.sequential.significant ? 'green' : 'gray') +
        ' · CS [' + esc((r.sequential.csLow ?? 0).toFixed(4)) + ', ' + esc((r.sequential.csHigh ?? 0).toFixed(4)) +
        '] · τ=' + esc(r.sequential.tau) + ' · logΛ=' + esc((r.sequential.logLambda ?? 0).toFixed(3)) + '</div>'
      : '<div class="muted">' + esc(t('report.seqNotComputed')) + '</div>') +
    '<h4>' + esc(t('report.bayesian')) + ' <span class="muted">' + esc(t('report.bayesianNote')) + '</span></h4>' +
    (r.bayesian && r.bayesian.mean
      ? '<div>P(B&gt;A) = ' + esc((r.bayesian.mean.probBSuperior ?? 0).toFixed(4)) + ' ' +
        chip(t('chip.probSuperior'), 'teal') + ' · ' + esc(t('report.expectedLoss')) + ' = ' + esc((r.bayesian.mean.expectedLossB ?? 0).toExponential(3)) +
        ' · 95% CrI [' + esc((r.bayesian.mean.credibleLow ?? 0).toFixed(4)) + ', ' + esc((r.bayesian.mean.credibleHigh ?? 0).toFixed(4)) + ']</div>'
      : r.bayesian && r.bayesian.proportion
      ? '<div>P(B&gt;A) = ' + esc((r.bayesian.proportion.probBSuperior ?? 0).toFixed(4)) + ' ' +
        chip(t('chip.probSuperior'), 'teal') + ' · ' + esc(t('report.expectedLoss')) + ' = ' + esc((r.bayesian.proportion.expectedLossB ?? 0).toExponential(3)) +
        ' · posterior A=Beta(' + esc((r.bayesian.proportion.postAA ?? 0).toFixed(1)) + ',' + esc((r.bayesian.proportion.postAB ?? 0).toFixed(1)) +
        ') B=Beta(' + esc((r.bayesian.proportion.postBA ?? 0).toFixed(1)) + ',' + esc((r.bayesian.proportion.postBB ?? 0).toFixed(1)) + ')</div>'
      : '<div class="muted">' + esc(t('report.notComputed')) + '</div>') +
    '<h4>' + esc(t('exp.guardrails')) + '</h4>' +
    ((r.guardrails || []).length
      ? '<ul class="plain">' + r.guardrails.map(g =>
        '<li>' + chip(g.breached ? t('chip.breached') : t('chip.ok'), g.breached ? 'red' : 'green') +
        ' <span class="mono">' + esc(g.Metric ?? g.metric ?? '—') + '</span> <span class="muted">' + esc(g.Detail ?? g.detail ?? '') + '</span></li>').join('') + '</ul>'
      : '<div class="muted">' + esc(t('misc.none')) + '</div>') +
    '<h4>' + esc(t('report.decision')) + '</h4>' +
    '<div>' + chip(String(d.verdict || '—').toUpperCase(), VERDICT_CHIP[d.verdict] || 'gray') + '</div>' +
    '<ul class="plain">' + (d.reasons || []).map(x => '<li>' + esc(x) + '</li>').join('') + '</ul>' +
    ((d.guardrailBreaches || []).length
      ? '<div class="banner warn">' + esc(t('report.breaches')) + '<ul>' + d.guardrailBreaches.map(gb =>
        '<li><span class="mono">' + esc(gb.metric) + '</span> — ' + esc(gb.detail || '') + '</li>').join('') + '</ul></div>' : '');
}

function renderBanditReport(box, r) {
  const b = r.bandit;
  box.innerHTML =
    '<div class="banner ' + (r.healthy ? 'ok' : 'err') + '"><b>' + esc(t('report.banditTitle')) + '</b> · ' +
    esc(b.frozen ? t('report.banditFrozen') : t('report.banditActive')) + '</div>' + metricReferenceSummary(r) +
    '<p class="muted">' + esc(t('report.banditNoFixed')) + '</p>' +
    '<div>' + esc(t('exp.policyVersion')) + ': <span class="mono">' + esc(b.policyVersion) +
    '</span> · state: <span class="mono">' + esc(b.stateVersion) + '</span> · ' +
    esc(t('report.watermark')) + ': <span class="mono">' + esc(b.feedbackWatermark) + '</span></div>' +
    '<div>' + esc(t('report.banditPending')) + ': ' + esc(b.pending) + ' · ' +
    esc(t('report.banditMissing')) + ': ' + esc(b.missingFeedback) + ' · ' +
    esc(t('report.banditFallback')) + ': ' + esc(b.fallbackAssignments) + '</div>' +
    (r.exposureMode === 'onRender' ? renderDiagnostics(r.diagnostics) : '') +
    (b.srmAlarm ? '<div class="banner err">' + esc(t('report.banditSrm')) + '</div>' : '') +
    (b.freezeReason ? '<div class="muted">' + esc(b.freezeReason) + '</div>' : '') +
    '<table class="grid"><thead><tr><th>' + esc(t('th.variant')) + '</th><th>' + esc(t('report.banditTrials')) +
    '</th><th>' + esc(t('report.banditSuccesses')) + '</th><th>' + esc(t('report.banditPosterior')) + '</th></tr></thead><tbody>' +
    (r.perVariant || []).map((v, i) => '<tr><td>' + esc(v.variant) + '</td><td>' + esc(b.trials[i]) +
      '</td><td>' + esc(b.successes[i]) + '</td><td>' + esc((b.posteriorMean[i] ?? 0).toFixed(4)) + '</td></tr>').join('') +
    '</tbody></table>' +
    (r.healthIssues || []).map(x => '<div class="banner err">' + esc(x) + '</div>').join('') +
    (r.guardrails || []).map(g => '<div>' + esc(g.Metric ?? g.metric) + ': ' + esc(g.Detail ?? g.detail) + '</div>').join('');
}

// ---------------------------------------------------------------------------
// Staged rollout (release plan) — experiment-detail section (§5.3 P4 自动推进).
// Pure renderers over the ReleasePlan JSON (steps/state/currentStep/history)
// returned by GET /api/v1/admin/experiments/{id}; all mutations go through the
// existing release-plan routes only. Danger actions (pause/takeover) require
// confirm + reason, mirroring the rollback confirmation style.
// ---------------------------------------------------------------------------
const PLAN_CHIP = { armed: 'gray', running: 'green', paused: 'amber', completed: 'blue', rolled_back: 'red' };
const PLAN_ACTION_CHIP = {
  'plan.start': 'green', 'step.advance': 'green', 'manual.advance': 'green',
  'plan.complete': 'blue', 'manual.complete': 'blue',
  'plan.pause': 'amber', 'manual.pause': 'amber', 'manual.resume': 'teal',
  'plan.rollback': 'red', 'repair_publish': 'teal',
  'manual.takeover_on': 'red', 'manual.takeover_off': 'gray',
};
// Engine block reasons surface in pausedReason / plan.pause details — render
// them with the same alert styling the report uses for SRM/guardrail breaches.
const PLAN_BLOCK_RE = /srm|guardrail|unhealthy|health|rollback|breach/i;

// planNextGate returns the earliest time the decision gate may fire for the
// current step (stepStartedAt + minRuntime + observationWindow), or null when
// it does not apply (not running / no step clock). Durations arrive as ns.
function planNextGate(plan) {
  if (!plan || plan.state !== 'running' || !plan.stepStartedAt) return null;
  const step = (plan.steps || [])[plan.currentStep];
  if (!step) return null;
  const ms = Date.parse(plan.stepStartedAt) +
    (Number(step.minRuntime || 0) + Number(step.observationWindow || 0)) / 1e6;
  return isNaN(ms) ? null : new Date(ms);
}

function renderRolloutSteps(plan) {
  const steps = plan.steps || [];
  return '<table class="grid" id="ro-steps-tbl"><thead><tr><th>#</th><th>' + esc(t('th.step')) + '</th><th>' + esc(t('exp.eligShort')) + '</th>' +
    '<th>' + esc(t('exp.minRuntime')) + '</th><th>' + esc(t('ro.obsWindow')) + '</th><th>' + esc(t('ro.advance')) + '</th><th>' + esc(t('th.status')) + '</th></tr></thead><tbody>' +
    (steps.map((s, i) => {
      let st;
      if (plan.state === 'completed' || i < plan.currentStep) st = chip(t('chip.done'), 'green');
      else if (i === plan.currentStep) {
        st = plan.state === 'rolled_back' ? chip(t('chip.stoppedHere'), 'red')
          : chip(plan.state === 'paused' ? t('chip.heldHere') : t('chip.current'), plan.state === 'paused' ? 'amber' : 'blue');
      } else st = chip(t('chip.pending'), 'gray');
      return '<tr' + (i === plan.currentStep ? ' class="ro-current"' : '') + '><td>' + i +
        '</td><td class="mono">' + esc(s.name) + '</td><td>' + esc(s.ratio) +
        '%</td><td>' + esc(fmtDurNs(s.minRuntime)) + '</td><td>' + esc(fmtDurNs(s.observationWindow)) +
        '</td><td>' + chip(s.autoAdvance ? t('chip.auto') : t('chip.manualHold'), s.autoAdvance ? 'green' : 'amber') +
        '</td><td>' + st + '</td></tr>';
    }).join('') || '<tr><td colspan="7" class="muted">' + esc(t('ro.noSteps')) + '</td></tr>') +
    '</tbody></table>' +
    '<div class="muted">' + esc(t('ro.gateNote')) + '</div>';
}

function renderRolloutHistory(plan) {
  const hist = (plan.history || []).slice().reverse(); // newest first
  return '<table class="grid" id="ro-history-tbl"><thead><tr><th>' + esc(t('th.time')) + '</th><th>' + esc(t('th.actor')) + '</th><th>' + esc(t('th.action')) + '</th>' +
    '<th>' + esc(t('th.step')) + '</th><th>' + esc(t('th.detailSkip')) + '</th></tr></thead><tbody>' +
    (hist.map(ev =>
      '<tr><td class="nowrap">' + esc(fmtTime(ev.at)) + '</td><td>' + esc(ev.actor || '—') + '</td><td>' +
      chip(ev.action, PLAN_ACTION_CHIP[ev.action] || 'gray') + '</td><td>' + esc(ev.step ?? '—') +
      '</td><td>' + esc(ev.detail || '—') + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">' + esc(t('ro.noActions')) + '</td></tr>') +
    '</tbody></table>';
}

function renderRolloutCreate() {
  return '<p class="muted" id="ro-none">' + esc(t('ro.none')) + '</p>' +
    '<div id="ro-steps-editor"></div>' +
    '<div class="row"><button class="btn sm" id="ro-add-step">' + esc(t('ro.addStep')) + '</button></div>' +
    '<div class="row"><button class="btn primary" id="ro-create">' + esc(t('ro.register')) + '</button></div>' +
    '<div class="muted">' + t('ro.validates') + '</div>';
}

// renderRollout is the pure renderer of the rollout card content: state
// banners (SRM/guardrail blocks in the report's alert style), progress,
// next-gate time, plan-step table, action history and the operator actions.
function renderRollout(exp) {
  const plan = exp && exp.releasePlan;
  if (!plan) return renderRolloutCreate();
  const steps = plan.steps || [];
  const cur = steps[plan.currentStep];
  const active = plan.state === 'armed' || plan.state === 'running' || plan.state === 'paused';
  const next = planNextGate(plan);

  let banner = '';
  if (plan.state === 'rolled_back') {
    banner = '<div class="banner err" id="ro-banner"><b>' + esc(t('ro.rolledBack')) + '</b><div>' +
      esc(plan.pausedReason || t('ro.rolledBackDefault')) + '</div></div>';
  } else if (plan.state === 'paused') {
    const reason = plan.pausedReason || '';
    const blocked = PLAN_BLOCK_RE.test(reason);
    banner = '<div class="banner ' + (blocked ? 'err' : 'warn') + '" id="ro-banner"><b>' +
      esc(blocked ? t('ro.blocked') : t('ro.paused')) + '</b><div>' + esc(reason || t('ro.noReason')) + '</div>' +
      (blocked
        ? '<div class="muted">' + esc(t('ro.blockedNote')) + '</div>'
        : '') + '</div>';
  } else if (plan.state === 'completed') {
    banner = '<div class="banner ok" id="ro-banner">' + esc(t('ro.completed', { ratio: cur ? cur.ratio : '—' })) + '</div>';
  } else if (plan.state === 'armed') {
    banner = '<div class="banner" id="ro-banner">' + t('ro.armed') + '</div>';
  }
  if (plan.manualOverride) {
    banner += '<div class="banner warn" id="ro-takeover-banner"><b>' + esc(t('ro.takeoverActive')) + '</b>' +
      esc(t('ro.takeoverNote')) + '</div>';
  }

  const done = plan.state === 'completed' ? steps.length : Math.min(plan.currentStep, steps.length);
  const pct = steps.length ? Math.round(100 * done / steps.length) : 0;
  return banner +
    '<div class="row" id="ro-head">' +
      chip(t('ro.planState', { state: plan.state }), PLAN_CHIP[plan.state] || 'gray') + ' ' +
      (plan.manualOverride ? chip(t('chip.manualOverride'), 'red') : chip(t('chip.engineAuto'), 'green')) +
      ' <span id="ro-progress-txt">' + esc(t('ro.stepProgress', { cur: steps.length ? plan.currentStep + 1 : 0, total: steps.length })) +
      (cur ? t('ro.currentStepMeta', { name: esc(cur.name), ratio: esc(cur.ratio) }) : '') + '</span>' +
    '</div>' +
    '<div class="bar" id="ro-bar"><i style="width:' + pct + '%"></i></div>' +
    '<div class="row"><span class="muted">' + esc(t('ro.nextGate')) + '</span> <b id="ro-next-eval">' + esc(next ? fmtTime(next) : '—') + '</b>' +
    (next ? ' <span class="muted">' + esc(t('ro.gateStartedNote', { time: fmtTime(plan.stepStartedAt) })) + '</span>'
      : plan.state === 'running' ? ' <span class="muted">' + esc(t('ro.waitClock')) + '</span>'
      : plan.state === 'paused' ? ' <span class="muted">' + esc(t('ro.heldNote')) + '</span>'
      : plan.state === 'armed' ? ' <span class="muted">' + esc(t('ro.armedNote')) + '</span>' : '') +
    '</div>' +
    '<h4>' + esc(t('ro.planSteps')) + '</h4>' + renderRolloutSteps(plan) +
    '<h4>' + esc(t('ro.actionHistory')) + ' <span class="muted">' + esc(t('ro.historyNote')) + '</span></h4>' +
    renderRolloutHistory(plan) +
    '<div class="row actions">' +
      '<button class="btn sm" id="ro-refresh">' + esc(t('btn.refresh')) + '</button>' +
      (plan.state === 'armed' || plan.state === 'running' ? '<button class="btn" id="ro-pause">' + esc(t('btn.pause')) + '</button>' : '') +
      (plan.state === 'paused' ? '<button class="btn primary" id="ro-resume">' + esc(t('btn.resume')) + '</button>' : '') +
      (plan.state === 'running' ? '<button class="btn" id="ro-advance">' + esc(t('btn.advance')) + '</button>' : '') +
      (active
        ? (plan.manualOverride
          ? '<button class="btn" id="ro-takeover-off">' + esc(t('btn.takeoverOff')) + '</button>'
          : '<button class="btn danger" id="ro-takeover-on">' + esc(t('btn.takeoverOn')) + '</button>')
        : '') +
    '</div>' +
    '<div class="muted">' + esc(t('ro.registeredBy', { by: plan.createdBy || '—', time: fmtTime(plan.updatedAt) })) + '</div>';
}

function rolloutStepRow() {
  const row = document.createElement('div');
  row.className = 'vrow';
  row.innerHTML = '<input class="rs-name" placeholder="' + esc(t('ph.stepName')) + '">' +
    '<input class="rs-ratio" type="number" step="0.01" min="0" max="100" placeholder="' + esc(t('ph.eligPct')) + '">' +
    '<input class="rs-min" type="number" step="0.5" min="0" placeholder="' + esc(t('ph.minRuntimeH')) + '">' +
    '<input class="rs-obs" type="number" step="0.5" min="0" placeholder="' + esc(t('ph.obsWindowH')) + '">' +
    '<label class="inline"><input type="checkbox" class="rs-auto"> ' + esc(t('ro.autoAdvance')) + '</label>' +
    '<button class="btn sm" title="' + esc(t('btn.remove')) + '">✕</button>';
  const x = $('button', row);
  if (x) x.onclick = () => row.remove();
  return row;
}

// bindRollout wires the rendered rollout card. State-conditional buttons are
// guarded (they exist only in the states where the action is legal server-side).
function bindRollout(m, expId) {
  const base = '/api/v1/admin/experiments/' + encodeURIComponent(expId) + '/release-plan';

  const refresh = $('#ro-refresh', m);
  if (refresh) refresh.onclick = async () => {
    const box = $('#rollout-box', m);
    try {
      const e2 = await api('/api/v1/admin/experiments/' + encodeURIComponent(expId));
      if (!box) return;
      box.innerHTML = renderRollout(e2);
      bindRollout(m, expId);
    } catch (err) { if (box) box.innerHTML = errBanner(err); }
  };

  const pause = $('#ro-pause', m);
  if (pause) pause.onclick = () => {
    if (!confirm(t('confirm.pause'))) return;
    const reason = prompt(t('prompt.pauseReason'), '');
    if (!reason) { toast(t('toast.reasonRequired'), false); return; }
    run(() => api(base + '/pause', { method: 'POST', body: { reason } }), t('toast.planPaused'));
  };

  const resume = $('#ro-resume', m);
  if (resume) resume.onclick = () => {
    if (!confirm(t('confirm.resume'))) return;
    run(() => api(base + '/resume', { method: 'POST' }), t('toast.planResumed'));
  };

  const adv = $('#ro-advance', m);
  if (adv) adv.onclick = () => {
    if (!confirm(t('confirm.advance'))) return;
    const reason = prompt(t('prompt.advanceReason'), '') || '';
    run(() => api(base + '/advance', { method: 'POST', body: { reason } }), t('toast.planAdvanced'));
  };

  const tkOn = $('#ro-takeover-on', m);
  if (tkOn) tkOn.onclick = () => {
    if (!confirm(t('confirm.takeoverOn'))) return;
    const reason = prompt(t('prompt.takeoverReason'), '');
    if (!reason) { toast(t('toast.reasonRequired'), false); return; }
    run(() => api(base + '/takeover', { method: 'POST', body: { enabled: true, reason } }),
      t('toast.takeoverOn'));
  };

  const tkOff = $('#ro-takeover-off', m);
  if (tkOff) tkOff.onclick = () => {
    if (!confirm(t('confirm.takeoverOff'))) return;
    const reason = prompt(t('prompt.reasonOptional'), '') || '';
    run(() => api(base + '/takeover', { method: 'POST', body: { enabled: false, reason } }),
      t('toast.takeoverOff'));
  };

  const editor = $('#ro-steps-editor', m);
  const addStep = $('#ro-add-step', m);
  if (editor && addStep) {
    const add = () => editor.appendChild(rolloutStepRow());
    addStep.onclick = add;
    if (!$$('.vrow', editor).length) add();
  }
  const create = $('#ro-create', m);
  if (create) create.onclick = () => {
    const steps = $$('.vrow', editor || m).map(r => ({
      name: $('.rs-name', r).value.trim(),
      ratio: Number($('.rs-ratio', r).value),
      minRuntime: Math.round(Number($('.rs-min', r).value || 0) * NS_HOUR),
      observationWindow: Math.round(Number($('.rs-obs', r).value || 0) * NS_HOUR),
      autoAdvance: $('.rs-auto', r).checked,
    })).filter(s => s.name || s.ratio);
    if (!steps.length) { toast(t('toast.needStep'), false); return; }
    if (steps.some(s => !s.name || !(s.ratio > 0 && s.ratio <= 100))) { toast(t('toast.stepFields'), false); return; }
    for (let i = 1; i < steps.length; i++) {
      if (steps[i].ratio <= steps[i - 1].ratio) { toast(t('toast.ratioIncrease', { i: i + 1 }), false); return; }
    }
    if (steps.some(s => !(s.minRuntime > 0))) { toast(t('toast.minRuntimePositive'), false); return; }
    requestChange('release_plan.create', expId, { steps }).catch(e => reportError(e));
  };
}

// ---------------------------------------------------------------------------
// View: Debug
// ---------------------------------------------------------------------------
async function viewDebug(m) {
  if (!needScope(m)) return;
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('debug.title')) + '</h2>' +
    '<span class="muted">' + esc(t('debug.hint')) + '</span></div>' +
    '<section class="card">' +
    '<div class="form-grid">' +
      '<label class="field">' + esc(t('exp.flagKey')) + '<input id="d-key" placeholder="my-flag"></label>' +
      '<label class="field">' + esc(t('debug.targetingKey')) + '<input id="d-tk" placeholder="' + esc(t('debug.tkPh')) + '"></label>' +
    '</div>' +
    '<label class="field">' + esc(t('debug.attrs')) + '<textarea id="d-attrs" class="json" rows="8" spellcheck="false"></textarea></label>' +
    '<div class="row"><button class="btn primary" id="d-run">' + esc(t('btn.evaluate')) + '</button></div></section>' +
    '<section class="card hidden" id="d-out"><h3>' + esc(t('debug.result')) + '</h3><div id="d-summary" class="row"></div>' +
    '<pre id="d-json" class="pre"></pre></section>';
  $('#d-attrs', m).value = '{\n  \n}';
  $('#d-run', m).onclick = async () => {
    let attrs;
    try { attrs = JSON.parse($('#d-attrs', m).value || '{}'); }
    catch (e) { toast(t('toast.attrsInvalid', { msg: e.message }), false); return; }
    const flagKey = $('#d-key', m).value.trim();
    if (!flagKey) { toast(t('toast.flagKeyRequired'), false); return; }
    try {
      const res = await api(scopePath() + '/debug/eval', {
        method: 'POST',
        body: { flagKey, context: { targetingKey: $('#d-tk', m).value.trim(), attributes: attrs } },
      });
      const r = (res && res.result) || {};
      $('#d-out', m).classList.remove('hidden');
      $('#d-summary', m).innerHTML =
        chip(t('debug.valueChip', { v: JSON.stringify(r.value) }), 'blue') + ' ' +
        chip(t('debug.variationChip', { v: r.variationType || '—' }), 'teal') + ' ' +
        chip(t('debug.reasonChip', { v: r.reason || '—' }), r.failed ? 'red' : 'green') +
        (r.errorCode ? ' ' + chip(t('debug.errorChip', { v: r.errorCode }), 'red') : '') +
        (r.bucket != null ? ' <span class="muted">' + esc(t('debug.bucket', { n: r.bucket })) + '</span>' : '') +
        ' <span class="muted">' + esc(t('debug.manifest', { n: res.manifestRevision ?? '—' })) +
        (res.note ? ' · ' + esc(res.note) : '') + '</span>';
      $('#d-json', m).textContent = JSON.stringify(res, null, 2);
    } catch (e) { reportError(e); }
  };
}

// ---------------------------------------------------------------------------
// View: workspace quotas (owner only)
const QUOTA_FIELDS = [
  ['maxFlags', 'quotas.maxFlags'],
  ['maxFlagBytes', 'quotas.maxFlagBytes'],
  ['maxSegments', 'quotas.maxSegments'],
  ['maxExperiments', 'quotas.maxExperiments'],
  ['maxEventBytes', 'quotas.maxEventBytes'],
  ['maxEventsPerBatch', 'quotas.maxEventsPerBatch'],
  ['eventRetention', 'quotas.eventRetention'],
];

function quotaError(e) {
  return errMsg(e);
}

async function viewQuotas(m) {
  if (!needWs(m)) return;
  const ws = S.ws;
  const path = wsPath() + '/quotas';
  const q = await api(path);
  if (ws !== S.ws) return;
  m.innerHTML = '<div class="view-head"><h2>' + esc(t('quotas.title')) + '</h2></div>' +
    '<section class="card"><p class="muted">' + esc(t('quotas.hint')) + '</p>' +
    '<form id="quota-form"><div class="form-grid">' + QUOTA_FIELDS.map(([key, label]) =>
      '<label class="field">' + esc(t(label)) +
      '<input id="quota-' + key + '" name="' + key + '" type="number" min="1" step="1" required value="' + esc(q[key]) + '"></label>').join('') +
    '</div><div id="quota-feedback" role="alert"></div>' +
    '<button class="btn primary" id="quota-save" type="submit">' + esc(t('quotas.save')) + '</button> ' +
    '<button class="btn" id="quota-refresh" type="button">' + esc(t('btn.refresh')) + '</button></form></section>';
  $('#quota-refresh', m).onclick = () => route();
  $('#quota-form', m).onsubmit = async ev => {
    ev.preventDefault();
    const body = {};
    for (const [key] of QUOTA_FIELDS) {
      const raw = $('#quota-' + key, m).value.trim();
      const n = Number(raw);
      if (!raw || !Number.isSafeInteger(n) || n <= 0) {
        $('#quota-feedback', m).textContent = t('quotas.invalid');
        return;
      }
      body[key] = n;
    }
    if (!confirm(t('quotas.confirm'))) return;
    const save = $('#quota-save', m);
    save.disabled = true;
    try {
      if (ws !== S.ws) return;
      const saved = await api(path, { method: 'PUT', body });
      if (ws !== S.ws) return;
      for (const [key] of QUOTA_FIELDS) $('#quota-' + key, m).value = String(saved[key]);
      $('#quota-feedback', m).textContent = t('quotas.saved');
    } catch (e) {
      $('#quota-feedback', m).textContent = quotaError(e);
      if (e.status === 403) refreshCapabilities();
    } finally { save.disabled = false; }
  };
}

// View: Tokens
// ---------------------------------------------------------------------------
function tokenRows(tokens) {
  return tokens.map(tk =>
    '<tr><td>' + esc(tk.name) + '</td><td>' + chip(tk.kind, tk.kind === 'admin' ? 'teal' : tk.kind === 'sdk' ? 'blue' : 'gray') +
    '</td><td>' + esc(tk.role || '—') + '</td><td class="mono">' + esc(tk.prefix || '—') +
    '</td><td class="mono">' + esc(tk.environmentId || '—') + '</td><td class="nowrap">' + esc(fmtTime(tk.createdAt)) +
    '</td><td>' + (tk.revoked ? chip(t('chip.revoked'), 'red') : chip(t('chip.active'), 'green')) + '</td><td>' +
    (tk.revoked ? '' : '<button class="btn sm danger" data-rev="' + esc(tk.id) + '" data-name="' + esc(tk.name) + '">' + esc(t('btn.revoke')) + '</button>') +
    '</td></tr>').join('') || '<tr><td colspan="8" class="muted">' + esc(t('tokens.empty')) + '</td></tr>';
}

async function viewTokens(m) {
  if (!needWs(m)) return;
  if (!can('token.manage')) {
    m.innerHTML = errBanner(new ApiError(403, 'forbidden', ''));
    const reauth = $('#btn-reauth2', m);
    if (reauth) reauth.onclick = () => showAuth();
    return;
  }
  const tokens = (await api(wsPath() + '/tokens')) || [];
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('tokens.title')) + '</h2><span class="muted">' + esc(t('tokens.count', { n: tokens.length })) + '</span></div>' +
    '<div id="tok-secret"></div>' +
    '<section class="card"><h3>' + esc(t('tokens.create')) + '</h3><div class="form-grid">' +
      '<label class="field">' + esc(t('label.name')) + '<input id="t-name"></label>' +
      '<label class="field">' + esc(t('label.kind')) + '<select id="t-kind"><option>admin</option><option>sdk</option><option>collect</option></select></label>' +
      '<label class="field">' + esc(t('label.role')) + '<select id="t-role"><option>owner</option><option>publisher</option><option>analyst</option><option>viewer</option></select></label>' +
      '<label class="field">' + esc(t('tokens.env')) + '<select id="t-env"><option value="">' + esc(t('tokens.envNone')) + '</option>' +
      S.envs.map(v => '<option value="' + esc(v.slug) + '">' + esc(v.name || v.slug) + '</option>').join('') +
      '</select></label></div>' +
    '<div class="row"><button class="btn primary" id="t-create">' + esc(t('btn.create')) + '</button></div></section>' +
    '<table class="grid"><thead><tr><th>' + esc(t('label.name')) + '</th><th>' + esc(t('label.kind')) + '</th><th>' + esc(t('label.role')) + '</th><th>' + esc(t('th.prefix')) + '</th><th>' + esc(t('th.env')) + '</th><th>' + esc(t('th.created')) + '</th><th>' + esc(t('th.status')) + '</th><th></th></tr></thead>' +
    '<tbody>' + tokenRows(tokens) + '</tbody></table>';
  $('#t-create', m).onclick = async () => {
    const body = {
      name: $('#t-name', m).value.trim(),
      kind: $('#t-kind', m).value,
      role: $('#t-role', m).value,
      environment: $('#t-env', m).value,
    };
    if (!body.name) { toast(t('toast.nameRequired'), false); return; }
    try {
      const res = await api(wsPath() + '/tokens', { method: 'POST', body });
      const secret = res.Secret || '';
      $('#tok-secret', m).innerHTML =
        '<div class="banner warn"><b>' + esc(t('tokens.secretWarn')) + '</b>' +
        '<div class="row"><input class="mono grow" id="secret-val" readonly>' +
        '<button class="btn sm" id="secret-copy">' + esc(t('btn.copy')) + '</button></div></div>';
      $('#secret-val', m).value = secret;
      $('#secret-copy', m).onclick = () => {
        const inp = $('#secret-val', m);
        inp.select();
        if (navigator.clipboard) navigator.clipboard.writeText(inp.value).then(() => toast(t('toast.secretCopied')));
        else document.execCommand('copy');
      };
      toast(t('toast.tokenCreated'));
      $('tbody', m).innerHTML = tokenRows((await api(wsPath() + '/tokens')) || []);
    } catch (e) { reportError(e); }
  };
  $('table.grid', m).addEventListener('click', async ev => {
    const b = ev.target.closest('[data-rev]');
    if (!b) return;
    const approval = await actionDialog({ title: t('btn.revoke'), summary: t('confirm.revokeToken', { name: b.dataset.name }),
      reasonLabel: t('prompt.tokenRevokeReason', { name: b.dataset.name }), confirmLabel: t('btn.revoke') });
    if (!approval) return;
    run(() => api('/api/v1/admin/tokens/' + encodeURIComponent(b.dataset.rev),
      { method: 'DELETE', body: { reason: approval.reason } }), t('toast.tokenRevoked'));
  });
}

// ---------------------------------------------------------------------------
// View: Audit
// ---------------------------------------------------------------------------
async function viewAudit(m) {
  if (!needWs(m)) return;
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('audit.title')) + '</h2>' +
    '<label class="inline">' + esc(t('audit.limit')) + ' <select id="a-limit">' +
    [25, 50, 100].map(n => '<option' + (n === S.auditLimit ? ' selected' : '') + '>' + n + '</option>').join('') +
    '</select></label><form id="a-search-form" class="inline-search"><input id="a-search" type="search" maxlength="128" value="' + esc(S.auditQuery) + '" placeholder="' + esc(t('audit.search')) + '" aria-label="' + esc(t('audit.search')) + '"><button class="btn sm" type="submit">' + esc(t('audit.searchButton')) + '</button></form></div><section class="card"><h3>' + esc(t('approval.inbox')) + '</h3><div id="approval-box"></div></section>' +
    '<div id="a-box"><p class="muted">' + esc(t('misc.loading')) + '</p></div>';
  let loadSeq = 0;
  const load = async () => {
    const seq = ++loadSeq;
    const approvalData = await api(wsPath() + '/approvals');
    const approvals = Array.isArray(approvalData) ? approvalData : [];
    $('#approval-box', m).innerHTML = '<table class="grid"><thead><tr><th>' + esc(t('th.time')) + '</th><th>' + esc(t('th.action')) + '</th>' +
      '<th>' + esc(t('th.object')) + '</th><th>' + esc(t('th.state')) + '</th><th>' + esc(t('approval.impact')) +
      '</th><th>' + esc(t('approval.rollback')) + '</th><th>' + esc(t('approval.guardrails')) + '</th><th>' + esc(t('th.actions')) + '</th></tr></thead><tbody>' +
      (approvals.map(a => '<tr><td>' + esc(fmtTime(a.submittedAt)) + '</td><td>' + esc(a.action) + '<div class="mono">' + esc(shortHash(a.payloadHash)) + '</div></td>' +
        '<td>' + esc(a.object) + '<div class="muted">' + esc(a.environmentId) + ' · r' + esc(a.baseRevision) + '</div></td>' +
        '<td>' + chip(a.state, DRAFT_CHIP[a.state] || 'gray') + '<div>' + esc(a.submittedBy) + ' → ' + esc(a.approvedBy || '—') + '</div>' +
        '<div class="muted">' + esc(fmtTime(a.expiresAt)) + '</div></td><td>' + esc(a.impact) + '</td><td>' + esc(a.rollbackTarget) +
        '</td><td>' + esc(a.guardrails) + '</td><td>' +
        (can('flag.approve') && a.state === 'submitted' ? '<button class="btn sm" data-appr="' + esc(a.id) + '">' + esc(t('btn.approve')) + '</button>' : '') +
        (can('flag.publish') && a.state === 'approved' && a.action !== 'breakglass.review' ? '<button class="btn sm primary" data-exec="' + esc(a.id) + '">' + esc(t('approval.execute')) + '</button>' : '') +
        '</td></tr>').join('') || '<tr><td colspan="8" class="muted">' + esc(t('approval.none')) + '</td></tr>') + '</tbody></table>';
    $('#approval-box', m).onclick = async ev => {
      const btn = ev.target.closest('[data-appr], [data-exec]');
      if (!btn) return;
      const a = approvals.find(x => x.id === (btn.dataset.appr || btn.dataset.exec));
      if (!a) return;
      if (btn.dataset.appr) {
        if (!can('flag.approve')) return;
        const approval = await actionDialog({ title: t('btn.approve'), summary: t('approval.confirmReview', { action: a.action, object: a.object }),
          preview: reviewPreview({ targetEnvironment: a.environmentId, risk: a.risk, baseRevision: a.baseRevision,
            estimatedAffected: a.impact, rollbackTarget: a.rollbackTarget, guardrails: a.guardrails, proposedHash: a.payloadHash }, 'action-review-preview'),
          reasonLabel: t('prompt.approvalReason'), confirmLabel: t('btn.approve') });
        if (!approval) return;
        run(() => api(wsPath() + '/approvals/' + encodeURIComponent(a.id) + '/approve',
          { method: 'POST', body: { note: approval.reason } }), t('approval.reviewed'));
      } else {
        if (!can('flag.publish')) return;
        const approval = await actionDialog({ title: t('approval.execute'), summary: t('approval.confirmExecute', { action: a.action, object: a.object }),
          preview: reviewPreview({ targetEnvironment: a.environmentId, risk: a.risk, baseRevision: a.baseRevision,
            estimatedAffected: a.impact, rollbackTarget: a.rollbackTarget, guardrails: a.guardrails, proposedHash: a.payloadHash }, 'action-review-preview'),
          confirmLabel: t('approval.execute') });
        if (!approval) return;
        run(() => executeApproval(a), t('approval.executed'));
      }
    };
    const cursor = S.auditCursors[S.auditPage] || 0;
    const params = 'limit=' + (S.auditLimit + 1) + '&q=' + encodeURIComponent(S.auditQuery) +
      (cursor ? '&before=' + encodeURIComponent(cursor) : '');
    const page = (await api(wsPath() + '/audit?' + params)) || [];
    if (seq !== loadSeq) return;
    const rows = page.slice(0, S.auditLimit);
    const hasMore = page.length > S.auditLimit;
    $('#a-box', m).innerHTML =
      '<table class="grid"><thead><tr><th>' + esc(t('th.time')) + '</th><th>' + esc(t('th.actor')) + '</th><th>' + esc(t('th.action')) + '</th><th>' + esc(t('th.object')) + '</th><th>' + esc(t('th.env')) + '</th><th>' + esc(t('th.detail')) + '</th></tr></thead><tbody>' +
      (rows.map(a => {
        const detail = typeof a.detail === 'string' ? a.detail : JSON.stringify(a.detail ?? '');
        return '<tr><td class="nowrap">' + esc(fmtTime(a.time)) + '</td><td>' + esc(a.actor || '—') +
          '</td><td class="mono">' + esc(a.action) + '</td><td class="mono">' + esc(a.object || '—') +
          '</td><td class="mono">' + esc(a.environmentId || '—') + '</td>' +
          '<td class="detail" title="' + esc(detail) + '">' + esc(detail) + '</td></tr>';
      }).join('') || '<tr><td colspan="6" class="muted">' + esc(t('audit.empty')) + '</td></tr>') +
      '</tbody></table><div class="pager"><button class="btn sm" id="a-prev" type="button"' + (S.auditPage ? '' : ' disabled') + '>' + esc(t('audit.previous')) + '</button><span>' + esc(t('audit.page', { n: S.auditPage + 1 })) + '</span><button class="btn sm" id="a-next" type="button"' + (hasMore ? '' : ' disabled') + '>' + esc(t('audit.next')) + '</button></div>';
    const prev = $('#a-prev', m), next = $('#a-next', m);
    if (prev) prev.onclick = () => { if (S.auditPage > 0) { S.auditPage--; load().catch(reportError); } };
    if (next) next.onclick = () => {
      if (!hasMore || !rows.length) return;
      S.auditCursors[S.auditPage + 1] = rows[rows.length - 1].id;
      S.auditPage++;
      load().catch(reportError);
    };
    gateActions(m);
  };
  $('#a-limit', m).onchange = ev => {
    S.auditLimit = Number(ev.target.value);
    S.auditCursors = [0]; S.auditPage = 0;
    load().catch(e => { $('#a-box', m).innerHTML = errBanner(e); });
  };
  const searchForm = $('#a-search-form', m);
  if (searchForm) searchForm.onsubmit = ev => {
    ev.preventDefault();
    S.auditQuery = $('#a-search', m).value.trim();
    S.auditCursors = [0]; S.auditPage = 0;
    load().catch(e => { $('#a-box', m).innerHTML = errBanner(e); });
  };
  await load();
}

async function executeApproval(a) {
  const scope = wsPath() + '/environments/' + encodeURIComponent(a.environmentId);
  if (a.action === 'metric.publish') {
    const match = /^(.*)\/v([1-9]\d*)$/.exec(a.object);
    if (!match) throw new ApiError(0, 'validation_failed', t('approval.unsupported'));
    return api(metricVersionPath({ key: match[1], version: Number(match[2]) }) + '/publish',
      { method: 'POST', body: { approvalId: a.id } });
  }
  if (a.action === 'release_plan.create') {
    const payload = JSON.parse(a.payload);
    return api('/api/v1/admin/experiments/' + encodeURIComponent(a.object) + '/release-plan',
      { method: 'POST', body: { steps: payload.steps, approvalId: a.id } });
  }
  if (a.action === 'flag.rollback') {
    const payload = JSON.parse(a.payload);
    return api(scope + '/flags/' + encodeURIComponent(a.object) + '/rollback',
      { method: 'POST', body: { revision: payload.revision, reason: a.reason, approvalId: a.id } });
  }
  if (a.action === 'ai.pin') {
    const content = prompt(t('approval.pinContent'), '');
    if (!content) throw new ApiError(0, 'validation_failed', t('err.contentRequired'));
    const i = a.object.indexOf('/');
    return api(scope + '/ai/resources/' + encodeURIComponent(a.object.slice(0, i)) + '/' +
      encodeURIComponent(a.object.slice(i + 1)) + '/pin',
      { method: 'POST', body: { content, approvalId: a.id } });
  }
  throw new ApiError(0, 'validation_failed', t('approval.unsupported'));
}

// ---------------------------------------------------------------------------
// View: AI resources
// ---------------------------------------------------------------------------
async function viewAI(m) {
  if (!needScope(m)) return;
  let data;
  try {
    data = await api(scopePath() + '/ai/resources');
  } catch (e) {
    if (e.status === 501) {
      S.aiAvailable = false;
      $('#nav-ai').classList.add('hidden');
      m.innerHTML = '<div class="banner warn">' + esc(t('ai.notEnabled')) + '</div>';
      return;
    }
    throw e;
  }
  const resources = data.resources || [];
  // revocations arrive as a map keyed "type<NUL>logicalId" -> reason
  const revocations = Object.entries(data.revocations || {}).map(([k, reason]) => {
    const i = k.indexOf('\u0000');
    return { type: i < 0 ? k : k.slice(0, i), logicalId: i < 0 ? '' : k.slice(i + 1), reason };
  });
  let allow = [];
  try { allow = (await api(wsPath() + '/model-allowlist')) || []; } catch { /* optional */ }
  const aiPath = scopePath() + '/ai/resources';
  const typeOpts = ['prompt', 'skill', 'agentspec'];
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('ai.title')) + '</h2></div>' +
    '<section class="card"><h3>' + esc(t('ai.register')) + '</h3><div class="form-grid">' +
      '<label class="field">' + esc(t('th.type')) + '<select id="ai-type">' + typeOpts.map(ty => '<option>' + ty + '</option>').join('') + '</select></label>' +
      '<label class="field">' + esc(t('ai.logicalId')) + '<input id="ai-logical" placeholder="checkout-helper"></label>' +
      '<label class="field">' + esc(t('label.name')) + '<input id="ai-name"></label></div>' +
    '<div class="row"><button class="btn primary" id="ai-reg">' + esc(t('btn.register')) + '</button></div></section>' +
    '<section class="card"><h3>' + esc(t('ai.resources', { n: resources.length })) + '</h3>' +
    '<table class="grid" id="ai-res-tbl"><thead><tr><th>' + esc(t('th.type')) + '</th><th>' + esc(t('ai.logicalId')) + '</th><th>' + esc(t('ai.physicalId')) + '</th><th>' + esc(t('th.owner')) + '</th><th>' + esc(t('th.created')) + '</th><th>' + esc(t('th.actions')) + '</th></tr></thead><tbody>' +
    (resources.map(r =>
      '<tr><td>' + chip(r.type, 'teal') + '</td><td class="mono">' + esc(r.logicalId) + '</td><td class="mono">' +
      esc(r.physicalId || '—') + '</td><td>' + esc(r.owner || '—') + '</td><td class="nowrap">' + esc(fmtTime(r.createdAt)) +
      '</td><td class="nowrap"><button class="btn sm" data-pin="' + esc(r.type + '|' + r.logicalId) + '">' + esc(t('btn.pinVersion')) + '</button> ' +
      '<button class="btn sm danger" data-revoke="' + esc(r.type + '|' + r.logicalId) + '">' + esc(t('btn.revoke')) + '</button></td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">' + esc(t('ai.noneRegistered')) + '</td></tr>') +
    '</tbody></table>' +
    (revocations.length
      ? '<h4>' + esc(t('ai.revocations')) + '</h4><table class="grid" id="ai-rev-tbl"><thead><tr><th>' + esc(t('th.type')) + '</th><th>' + esc(t('ai.logicalId')) + '</th><th>' + esc(t('th.reason')) + '</th><th></th></tr></thead><tbody>' +
        revocations.map(rv =>
          '<tr><td>' + chip(rv.type, 'red') + '</td><td class="mono">' + esc(rv.logicalId) + '</td><td>' + esc(rv.reason || '—') +
          '</td><td><button class="btn sm" data-reinstate="' + esc(rv.type + '|' + rv.logicalId) + '">' + esc(t('btn.reinstate')) + '</button></td></tr>').join('') +
        '</tbody></table>'
      : '') +
    '</section>' +
    '<section class="card"><h3>' + esc(t('ai.resolveViewer')) + '</h3><div class="row">' +
      '<select id="ai-r-type">' + typeOpts.map(ty => '<option>' + ty + '</option>').join('') + '</select>' +
      '<input id="ai-r-logical" placeholder="logicalId">' +
      '<input id="ai-r-ver" placeholder="' + esc(t('ai.versionPh')) + '">' +
      '<button class="btn" id="ai-resolve">' + esc(t('btn.resolve')) + '</button></div>' +
      '<div id="ai-r-out"></div></section>' +
    '<section class="card"><h3>' + esc(t('ai.allowlist')) + '</h3><div id="ai-allow"></div>' +
    '<div class="row"><button class="btn sm" id="ai-allow-add">' + esc(t('ai.addEntry')) + '</button>' +
    '<button class="btn primary" id="ai-allow-save">' + esc(t('ai.saveAllowlist')) + '</button></div></section>';

  const split = v => { const i = v.indexOf('|'); return [v.slice(0, i), v.slice(i + 1)]; };
  $('#ai-reg', m).onclick = () => {
    const body = {
      type: $('#ai-type', m).value,
      logicalId: $('#ai-logical', m).value.trim(),
      name: $('#ai-name', m).value.trim(),
    };
    if (!body.logicalId || !body.name) { toast(t('toast.aiFieldsRequired'), false); return; }
    run(() => api(aiPath, { method: 'POST', body }), t('toast.resourceRegistered'));
  };
  $('#ai-res-tbl', m).addEventListener('click', async ev => {
    const pin = ev.target.closest('[data-pin]'), rev = ev.target.closest('[data-revoke]');
    if (pin) {
      const [ty, lid] = split(pin.dataset.pin);
      promptForm(t('ai.pinTitle', { type: ty, id: lid }),
        [{ name: 'content', label: t('ai.content'), type: 'textarea', rows: 10, placeholder: t('ai.contentPh') }],
        t('btn.pin'), async v => {
          if (!v.content) throw new ApiError(0, 'validation_failed', t('err.contentRequired'));
          await requestChange('ai.pin', ty + '/' + lid, { content: v.content });
        });
    } else if (rev) {
      const [ty, lid] = split(rev.dataset.revoke);
      const approval = await actionDialog({ title: t('btn.revoke'), summary: t('confirm.revokeResource'),
        reasonLabel: t('prompt.revokeReason', { type: ty, id: lid }), confirmLabel: t('btn.revoke') });
      if (!approval) return;
      run(() => api(aiPath + '/' + encodeURIComponent(ty) + '/' + encodeURIComponent(lid) + '/revoke',
        { method: 'POST', body: { reason: approval.reason } }), t('toast.resourceRevoked'));
    }
  });
  const revTbl = $('#ai-rev-tbl', m);
  if (revTbl) {
    revTbl.addEventListener('click', async ev => {
      const b = ev.target.closest('[data-reinstate]');
      if (!b) return;
      const [ty, lid] = split(b.dataset.reinstate);
      const approval = await actionDialog({ title: t('btn.reinstate'), summary: t('confirm.reinstateResource', { type: ty, id: lid }),
        reasonLabel: t('prompt.reinstateReason', { type: ty, id: lid }), confirmLabel: t('btn.reinstate') });
      if (!approval) return;
      run(() => api(aiPath + '/' + encodeURIComponent(ty) + '/' + encodeURIComponent(lid) + '/reinstate',
        { method: 'POST', body: { reason: approval.reason } }), t('toast.resourceReinstated'));
    });
  }
  $('#ai-resolve', m).onclick = async () => {
    const q = new URLSearchParams({
      type: $('#ai-r-type', m).value,
      logicalId: $('#ai-r-logical', m).value.trim(),
    });
    const ver = $('#ai-r-ver', m).value.trim();
    if (ver) q.set('version', ver);
    try {
      const res = await api(aiPath.replace(/\/resources$/, '/resolve') + '?' + q);
      $('#ai-r-out', m).innerHTML =
        '<div class="row">' + chip(t('ai.versionChip', { v: res.version || '—' }), 'blue') + ' ' +
        chip(t('ai.digestChip', { v: res.digest || '—' }), 'gray') + '</div>';
      const pre = document.createElement('pre');
      pre.className = 'pre';
      pre.textContent = res.content !== undefined ? res.content : JSON.stringify(res, null, 2);
      $('#ai-r-out', m).appendChild(pre);
    } catch (e) { $('#ai-r-out', m).innerHTML = errBanner(e); }
  };
  const allowBox = $('#ai-allow', m);
  const addAllowRow = ent => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="al-provider" placeholder="' + esc(t('ph.provider')) + '">' +
      '<input class="al-model" placeholder="' + esc(t('ph.model')) + '">' +
      '<label class="inline"><input type="checkbox" class="al-enabled"> ' + esc(t('ai.enabled')) + '</label>' +
      '<button class="btn sm">✕</button>';
    $('.al-provider', row).value = (ent && ent.provider) || '';
    $('.al-model', row).value = (ent && ent.model) || '';
    $('.al-enabled', row).checked = !ent || ent.enabled !== false;
    $('button', row).onclick = () => row.remove();
    allowBox.appendChild(row);
  };
  allow.forEach(addAllowRow);
  if (!allow.length) allowBox.innerHTML = '<p class="muted">' + esc(t('ai.allowEmpty')) + '</p>';
  $('#ai-allow-add', m).onclick = () => { if (!allow.length && !$('.vrow', allowBox)) allowBox.innerHTML = ''; addAllowRow(null); };
  $('#ai-allow-save', m).onclick = () => {
    const entries = $$('.vrow', allowBox).map(row => ({
      provider: $('.al-provider', row).value.trim(),
      model: $('.al-model', row).value.trim(),
      enabled: $('.al-enabled', row).checked,
    })).filter(e2 => e2.provider && e2.model);
    run(() => api(wsPath() + '/model-allowlist', { method: 'PUT', body: { entries } }), t('toast.allowSaved'));
  };
}

// ---------------------------------------------------------------------------
// View: Metric catalog
// ---------------------------------------------------------------------------
function metricVersionPath(ref) {
  return wsPath() + '/metrics/' + encodeURIComponent(ref.key) + '/versions/' + encodeURIComponent(ref.version);
}

async function viewMetrics(m) {
  if (!needWs(m)) return;
  if (S.metricRef) {
    const def = await api(metricVersionPath(S.metricRef));
    const path = metricVersionPath(def);
    m.innerHTML = '<div class="view-head"><button class="btn sm" id="metric-back">' + esc(t('exp.back')) + '</button>' +
      '<h2>' + esc(def.displayName) + '</h2>' + chip(def.status, METRIC_CHIP[def.status] || 'gray') + '</div>' +
      '<section class="card"><dl class="detail-grid">' +
      '<dt>' + esc(t('metrics.key')) + '</dt><dd class="mono">' + esc(def.key) + '</dd>' +
      '<dt>' + esc(t('metrics.version')) + '</dt><dd>v' + esc(def.version) + '</dd>' +
      '<dt>' + esc(t('metrics.owner')) + '</dt><dd>' + esc(def.owner) + '</dd>' +
      '<dt>' + esc(t('metrics.description')) + '</dt><dd>' + esc(def.description || '—') + '</dd>' +
      '<dt>' + esc(t('metrics.source')) + '</dt><dd>' + esc(def.source) + '</dd>' +
      '<dt>' + esc(t('metrics.eventKeys')) + '</dt><dd class="mono">' + esc((def.eventKeys || []).join(', ')) + '</dd>' +
      '<dt>' + esc(t('metrics.windowDays')) + '</dt><dd>' + esc(Number(def.attributionWindow) / NS_DAY) + '</dd>' +
      '<dt>' + esc(t('metrics.hash')) + '</dt><dd class="mono">' + esc(def.hash) + '</dd>' +
      '</dl><h3>' + esc(t('metrics.spec')) + '</h3><pre class="mono">' + esc(JSON.stringify(def.spec, null, 2)) + '</pre>' +
      '<div class="action-row">' +
      (def.status === 'draft' ? '<button class="btn sm" id="metric-submit">' + esc(t('metrics.submit')) + '</button>' +
        '<button class="btn primary sm" id="metric-publish">' + esc(t('metrics.publish')) + '</button>' : '') +
      (def.status === 'published' ? '<button class="btn sm" id="metric-deprecate">' + esc(t('metrics.deprecate')) + '</button>' : '') +
      '</div></section>';
    $('#metric-back', m).onclick = () => { S.metricRef = null; route(); };
    const submit = $('#metric-submit', m);
    if (submit) submit.onclick = async () => {
      const choice = await actionDialog({ title: t('metrics.submit'), summary: def.displayName + ' v' + def.version,
        reasonLabel: t('approval.reason'), confirmLabel: t('metrics.submit') });
      if (choice) run(() => api(path + '/submit', { method: 'POST', body: { reason: choice.reason } }), t('metrics.submitted'));
    };
    const publish = $('#metric-publish', m);
    if (publish) publish.onclick = () => promptForm(t('metrics.publish'), [
      { name: 'approvalId', label: t('metrics.approvalId') },
    ], t('metrics.publish'), async v => {
      if (!v.approvalId.trim()) throw new ApiError(0, 'validation_failed', t('metrics.approvalId'));
      await api(path + '/publish', { method: 'POST', body: { approvalId: v.approvalId.trim() } });
      toast(t('metrics.published')); route();
    });
    const deprecate = $('#metric-deprecate', m);
    if (deprecate) deprecate.onclick = async () => {
      const choice = await actionDialog({ title: t('metrics.deprecate'), summary: def.displayName + ' v' + def.version,
        reasonLabel: t('approval.reason'), confirmLabel: t('metrics.deprecate') });
      if (choice) run(() => api(path + '/deprecate', { method: 'POST', body: { reason: choice.reason } }), t('metrics.deprecated'));
    };
    return;
  }

  const query = new URLSearchParams({ limit: '50' });
  if (S.metricQuery) query.set('q', S.metricQuery);
  if (S.metricStatus) query.set('status', S.metricStatus);
  if (S.metricCursor) query.set('cursor', S.metricCursor);
  const page = await api(wsPath() + '/metrics?' + query);
  const rows = page.items || [];
  m.innerHTML = '<div class="view-head"><h2>' + esc(t('metrics.title')) + '</h2>' +
    '<form id="metric-search" class="inline-search"><input id="metric-q" type="search" value="' + esc(S.metricQuery) + '" placeholder="' + esc(t('list.search')) + '" aria-label="' + esc(t('list.search')) + '">' +
    '<select id="metric-status" aria-label="' + esc(t('metrics.status')) + '">' +
    ['', 'draft', 'published', 'deprecated'].map(v => '<option value="' + v + '"' + (v === S.metricStatus ? ' selected' : '') + '>' + esc(v || t('metrics.all')) + '</option>').join('') +
    '</select><button class="btn sm" type="submit">' + esc(t('audit.searchButton')) + '</button></form>' +
    '<button class="btn primary sm" id="metric-new">' + esc(t('metrics.new')) + '</button></div>' +
    '<table class="grid"><thead><tr><th>' + esc(t('metrics.key')) + '</th><th>' + esc(t('metrics.version')) + '</th><th>' + esc(t('label.displayName')) + '</th><th>' + esc(t('metrics.status')) + '</th><th>' + esc(t('metrics.owner')) + '</th></tr></thead><tbody>' +
    (rows.map(d => '<tr><td><button class="row-link mono" type="button" data-metric-key="' + esc(d.key) + '" data-metric-version="' + esc(d.version) + '">' + esc(d.key) + '</button></td><td>v' + esc(d.version) + '</td><td>' + esc(d.displayName) + '</td><td>' + chip(d.status, METRIC_CHIP[d.status] || 'gray') + '</td><td>' + esc(d.owner) + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">' + esc(t('metrics.empty')) + '</td></tr>') + '</tbody></table>' +
    (page.nextCursor ? '<button class="btn sm" id="metric-next">' + esc(t('audit.next')) + '</button>' : '');
  $('#metric-search', m).onsubmit = ev => {
    ev.preventDefault();
    S.metricQuery = $('#metric-q', m).value.trim();
    S.metricStatus = $('#metric-status', m).value;
    S.metricCursor = '';
    route();
  };
  $('tbody', m).onclick = ev => {
    const button = ev.target.closest('[data-metric-key]');
    if (button) { S.metricRef = { key: button.dataset.metricKey, version: Number(button.dataset.metricVersion) }; route(); }
  };
  if (page.nextCursor) $('#metric-next', m).onclick = () => { S.metricCursor = page.nextCursor; route(); };
  $('#metric-new', m).onclick = () => promptForm(t('metrics.new'), [
    { name: 'key', label: t('metrics.key'), placeholder: 'activation.complete' },
    { name: 'displayName', label: t('label.displayName') },
    { name: 'description', label: t('metrics.description'), type: 'textarea', rows: 2 },
    { name: 'owner', label: t('metrics.owner') },
    { name: 'eventKeys', label: t('metrics.eventKeys'), placeholder: 'activation_complete' },
    { name: 'windowDays', label: t('metrics.windowDays'), type: 'number', value: '7' },
    { name: 'spec', label: t('metrics.spec'), type: 'textarea', rows: 5,
      value: JSON.stringify({ type: 'proportion', direction: 'increase' }, null, 2) },
  ], t('metrics.create'), async v => {
    let spec;
    try { spec = JSON.parse(v.spec); }
    catch { throw new ApiError(0, 'validation_failed', t('exp.journeyInvalid')); }
    const body = { key: v.key.trim(), displayName: v.displayName.trim(), description: v.description.trim(),
      owner: v.owner.trim(), source: 'collect', dedupUnit: 'user',
      eventKeys: v.eventKeys.split(',').map(s => s.trim()).filter(Boolean),
      attributionWindow: Math.round(Number(v.windowDays) * NS_DAY), spec };
    const created = await api(wsPath() + '/metrics', { method: 'POST', body });
    S.metricRef = { key: created.key, version: created.version };
    toast(t('metrics.created')); route();
  });
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------
Object.assign(VIEWS, {
  overview: viewOverview,
  flags: viewFlags,
  segments: viewSegments,
  experiments: viewExperiments,
  metrics: viewMetrics,
  debug: viewDebug,
  tokens: viewTokens,
  quotas: viewQuotas,
  audit: viewAudit,
  ai: viewAI,
});

// ---------------------------------------------------------------------------
// i18n chrome — static DOM text, language switch, in-place re-render
// ---------------------------------------------------------------------------
const THEME_KEY = 'grayling_theme';
function themePreference() {
  const saved = localStorage.getItem(THEME_KEY);
  return saved === 'light' || saved === 'dark' ? saved : 'system';
}
function applyTheme() {
  const preference = themePreference();
  const resolved = preference === 'system'
    ? (window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark')
    : preference;
  if (document.documentElement) document.documentElement.dataset.theme = resolved;
  const selector = $('#sel-theme');
  if (selector) selector.value = preference;
}
// applyStaticI18n refreshes every index.html node carrying data-i18n /
// data-i18n-ph / data-i18n-title from the active catalog (no page reload).
function applyStaticI18n() {
  $$('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
  $$('[data-i18n-ph]').forEach(el => { el.placeholder = t(el.dataset.i18nPh); });
  $$('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
  $('#nav')?.setAttribute?.('aria-label', t('nav.primary'));
  if (document.documentElement) document.documentElement.lang = I18N.lang === 'zh' ? 'zh-CN' : 'en';
  document.title = t('app.title');
}

function updateLangActive() {
  const zh = $('#lang-zh'), en = $('#lang-en');
  if (zh) zh.classList.toggle('active', I18N.lang === 'zh');
  if (en) en.classList.toggle('active', I18N.lang === 'en');
  zh?.setAttribute?.('aria-pressed', String(I18N.lang === 'zh'));
  en?.setAttribute?.('aria-pressed', String(I18N.lang === 'en'));
}

// bindLangSwitch wires the top-bar 中/EN toggle: switching persists the
// choice (setLang → localStorage) and re-renders chrome + current view.
function bindLangSwitch() {
  const zh = $('#lang-zh'), en = $('#lang-en');
  if (zh) zh.onclick = () => { if (confirmLeaveEditor()) setLang('zh'); };
  if (en) en.onclick = () => { if (confirmLeaveEditor()) setLang('en'); };
  updateLangActive();
}

// registerI18nHooks installs the in-place re-render callback fired by
// setLang() in i18n.js.
function registerI18nHooks() {
  I18N.onLangChange = () => {
    applyStaticI18n();
    updateLangActive();
    if (S.booted) route();
  };
}

function init() {
  restoreConsoleHash(location.hash || '');
  applyTheme();
  $('#sel-theme').onchange = ev => {
    localStorage.setItem(THEME_KEY, ev.target.value);
    applyTheme();
  };
  window.matchMedia?.('(prefers-color-scheme: light)').addEventListener?.('change', () => {
    if (themePreference() === 'system') applyTheme();
  });
  registerI18nHooks();
  applyStaticI18n();
  bindLangSwitch();
  $('#btn-connect').onclick = doConnect;
  $('#auth-token').addEventListener('keydown', ev => { if (ev.key === 'Enter') doConnect(); });
  $('#btn-reauth').onclick = signOut;

  $('#btn-menu').onclick = () => {
    const opened = $('#nav').classList.toggle('mobile-open');
    $('#btn-menu').setAttribute('aria-expanded', String(opened));
  };

  $$('#nav button').forEach(b => {
    b.onclick = () => {
      if (S.view === b.dataset.view) {
        $('#nav').classList.remove('mobile-open');
        $('#btn-menu').setAttribute('aria-expanded', 'false');
        return;
      }
      if (!confirmLeaveEditor()) return;
      S.view = b.dataset.view;
      S.deepLinkDenied = false;
      S.flagKey = S.segKey = S.expId = S.metricRef = null;
      S.flagNew = S.expCreate = false;
      $('#nav').classList.remove('mobile-open');
      $('#btn-menu').setAttribute('aria-expanded', 'false');
      route();
    };
  });
  $('#sel-ws').onchange = async ev => {
    if (!confirmLeaveEditor()) { ev.target.value = S.ws; return; }
    S.ws = ev.target.value;
    S.deepLinkDenied = false;
    S.deepLinkRequested = false;
    S.capabilities = null;
    S.globalCapabilities = null;
    updateCapabilities();
    S.env = '';
    S.flagKey = S.segKey = S.expId = S.metricRef = null;
    localStorage.setItem('golab_ws', S.ws);
    try { await loadEnvs(); await refreshCapabilities(); } catch (e) { reportError(e); }
    route();
    probeAI();
  };
  $('#sel-env').onchange = ev => {
    if (!confirmLeaveEditor()) { ev.target.value = S.env; return; }
    S.env = ev.target.value;
    S.deepLinkDenied = false;
    S.deepLinkRequested = false;
    S.flagKey = S.segKey = S.expId = S.metricRef = null;
    localStorage.setItem('golab_env', S.env);
    route();
    probeAI();
  };
  $('#btn-new-ws').onclick = () => promptForm(t('ws.newTitle'), [
    { name: 'slug', label: t('label.slug'), placeholder: 'acme' },
    { name: 'name', label: t('label.displayName'), placeholder: 'Acme Inc' },
  ], t('btn.create'), async v => {
    const slug = v.slug.trim();
    if (!slug) throw new ApiError(0, 'validation_failed', t('err.slugRequired'));
    await api('/api/v1/admin/workspaces', { method: 'POST', body: { slug, name: v.name.trim() || slug } });
    toast(t('toast.wsCreated'));
    S.ws = slug;
    localStorage.setItem('golab_ws', S.ws);
    await loadWorkspaces();
    route();
    probeAI();
  });
  $('#btn-new-env').onclick = () => {
    if (!S.ws) { toast(t('toast.needWs'), false); return; }
    promptForm(t('env.newTitle', { ws: S.ws }), [
      { name: 'slug', label: t('label.slug'), placeholder: 'production' },
      { name: 'name', label: t('label.displayName'), placeholder: 'Production' },
    ], t('btn.create'), async v => {
      const slug = v.slug.trim();
      if (!slug) throw new ApiError(0, 'validation_failed', t('err.slugRequired'));
      await api(wsPath() + '/environments', { method: 'POST', body: { slug, name: v.name.trim() || slug } });
      toast(t('toast.envCreated'));
      S.env = slug;
      localStorage.setItem('golab_env', S.env);
      await loadEnvs();
      route();
      probeAI();
    });
  };

  localStorage.removeItem('golab_token'); // remove credentials saved by older console versions
  if (sameOriginConsole()) {
    cookieSession = true;
    api('/api/v1/admin/session').then(() => boot()).catch(() => { cookieSession = false; showAuth(''); });
  } else showAuth('');
  window.addEventListener('beforeunload', ev => {
    if (editorValue() !== null && S.editorBaseline !== null && editorValue() !== S.editorBaseline) {
      ev.preventDefault();
      ev.returnValue = '';
    }
  });
  window.addEventListener('popstate', async () => {
    if (!restoreConsoleHash(location.hash || '') || !S.booted) return;
    await loadWorkspaces();
    S.deepLinkRequested = false;
    await route();
  });
}

document.addEventListener('DOMContentLoaded', init);
