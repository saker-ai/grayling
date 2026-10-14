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
    let visible = 0;
    $$('[data-list-record]', m).forEach(record => {
      record.hidden = !!query && !record.textContent.toLocaleLowerCase().includes(query);
      if (!record.hidden && record.tagName === 'TR') visible++;
    });
    $('[data-list-no-match]', m)?.classList.toggle('hidden', !query || visible > 0);
  };
}

function reviewPreview(p, id = 'review-preview') {
  if (!p) return '';
  return '<section class="card" id="' + esc(id) + '"><h3>' + esc(t('approval.preview')) + '</h3>' +
    '<p>' + esc(t('approval.environment')) + ': ' + esc(p.targetEnvironment) + ' · ' +
      esc(t('approval.risk')) + ': ' + esc(p.risk) + ' · ' + esc(t('approval.base')) + ': r' + esc(p.baseRevision) + '</p>' +
    '<p>' + esc(t('approval.changed')) + ': ' + esc((p.changedFields || []).join(', ') || '—') + '</p>' +
    '<p>' + esc(t('approval.impact')) + ': ' + esc(p.estimatedAffected) + '</p>' +
    renderFlagExperimentImpact(p.experimentImpact, false) +
    '<p>' + esc(t('approval.rollback')) + ': ' + esc(p.rollbackTarget) + '</p>' +
    '<p>' + esc(t('approval.guardrails')) + ': ' + esc(p.guardrails) + '</p>' +
    '<p class="mono">' + esc(t('approval.hash')) + ': ' + esc(p.proposedHash) + '</p>' +
    (p.approvalExpiresAt ? '<p>' + esc(t('approval.expires')) + ': ' + esc(fmtTime(p.approvalExpiresAt)) + '</p>' : '') +
    '</section>';
}

function renderFlagExperimentImpact(impact, links = true) {
  if (!impact) return '';
  const references = impact.references || [];
  const summary = '<p>' + esc(t('flags.linkedExperimentsCount', {
    shown: references.length, total: impact.total || 0,
  })) + '</p>';
  const rows = references.map(ref => '<li>' +
    (links ? '<button class="row-link mono" type="button" data-exp-from-flag="' + esc(ref.id) + '">' +
      esc(ref.id) + '</button>' : '<span class="mono">' + esc(ref.id) + '</span>') +
    ' · ' + esc(ref.state) + ' · ' + esc(t('th.epoch')) + ' ' + esc(ref.epoch) +
    ' · r' + esc(ref.flagRevision) + '</li>').join('');
  const content = summary + (rows ? '<ul class="flag-experiment-list">' + rows + '</ul>' :
    '<p class="muted">' + esc(t('flags.noLinkedExperiments')) + '</p>') +
    (impact.total > references.length ? '<p class="muted">' + esc(t('flags.linkedExperimentsMore')) + '</p>' : '');
  return links ? '<section class="card"><h3>' + esc(t('flags.linkedExperiments')) +
    '</h3>' + content + '</section>' : content;
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
    if (res.status === 401) { cookieSession = false; memoryToken = ''; showAuth(t('err.tokenRejected401'), true); }
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
    if (res.status === 401) { cookieSession = false; memoryToken = ''; showAuth(t('err.tokenRejected401'), true); }
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
  if (e.code === 'flag_review_changed') return t('flags.reviewChanged');
  if (e.code === 'report_changed') return t('ro.reportChanged');
  if (e.code === 'report_unavailable') return t('ro.reportUnavailable');
  if (e.status === 403) return t('err.denied403');
  if (e.status === 409) return t('err.conflict409');
  if (e.status === 429) return t('err.limited429');
  if (e.code === 'network') return t('err.networkRetry');
  if (e.status >= 500 && e.status !== 501) return t('err.server5xx', { status: e.status });
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
  approvalID: null, approvalFilter: 'pending', actorName: '',
  principalID: '', reauthDraft: null,
  expId: null, expCreate: false, expTab: 'summary',
  wizardDirty: false,
  metricRef: null, metricQuery: '', metricStatus: '', metricCursor: '',
  deepLinkRequested: false, deepLinkDenied: false,
  auditLimit: 50,
  auditQuery: '', auditObject: '', auditCursors: [0], auditPage: 0,
  aiAvailable: true, aiRef: null,
  booted: false,
  timer: null,
  pollCleanup: null,
  editorBaseline: null,
};
function editorValue() {
  const flag = $('#flag-json');
  if (flag) {
    const form = $('#flag-structured');
    const fields = form ? $$('input, select, textarea', form).map(control => [control.value, control.checked]) : [];
    return JSON.stringify(['flag', flag.value, $('#flag-risk')?.value, $('#flag-base')?.value, fields]);
  }
  const segment = $('#seg-json');
  if (segment) {
    const form = $('#seg-structured');
    const fields = form ? $$('input, select', form).map(control => control.value) : [];
    return JSON.stringify(['segment', segment.value, $('#seg-risk')?.value, fields]);
  }
  return null;
}
function captureReauthDraft() {
  if (!S.principalID || !S.ws || !S.env) return;
  let content = null;
  let key = '';
  let meta = '';
  if (S.view === 'flags' && S.flagKey && editorValue() !== S.editorBaseline) {
    key = S.flagKey;
    content = $('#flag-json')?.value || '';
    meta = t('auth.draftMetaFlag', { risk: $('#flag-risk')?.value || '—',
      revision: $('#flag-base')?.value || '0' });
  } else if (S.view === 'segments' && S.segKey && editorValue() !== S.editorBaseline) {
    key = S.segKey;
    content = $('#seg-json')?.value || '';
    meta = t('auth.draftMetaSegment', { risk: $('#seg-risk')?.value || '—' });
  } else if (S.view === 'experiments' && S.expCreate && S.wizardDirty) {
    key = '~new';
    content = JSON.stringify($$('input, select, textarea', $('#main')).map(control => ({
      field: control.id || control.name || control.getAttribute('aria-label') || control.placeholder || '',
      value: control.value, checked: control.type === 'checkbox' ? control.checked : undefined,
    })), null, 2);
  }
  if (!content) return;
  S.reauthDraft = { principalID: S.principalID, ws: S.ws, env: S.env, view: S.view, key, content, meta };
}

function renderReauthDraft() {
  const draft = S.reauthDraft;
  if (!draft) return;
  const key = S.view === 'flags' ? S.flagKey : S.view === 'segments' ? S.segKey : S.expCreate ? '~new' : '';
  if (draft.principalID !== S.principalID || draft.ws !== S.ws || draft.env !== S.env ||
      draft.view !== S.view || draft.key !== key) { S.reauthDraft = null; return; }
  if ($('#reauth-draft')) return;
  const card = document.createElement('section');
  card.className = 'card';
  card.id = 'reauth-draft';
  card.innerHTML = '<h3 tabindex="-1">' + esc(t('auth.draftSaved')) + '</h3><p>' + esc(t('auth.draftReview')) + '</p>' +
    (draft.meta ? '<p class="muted">' + esc(draft.meta) + '</p>' : '') +
    '<label class="field">' + esc(t('auth.draftContent')) + '<textarea class="json" rows="8" readonly></textarea></label>' +
    '<div class="row actions"><button class="btn sm" type="button" data-draft-dismiss>' +
      esc(t('btn.dismiss')) + '</button></div>';
  $('textarea', card).value = draft.content;
  $('[data-draft-dismiss]', card).onclick = () => { S.reauthDraft = null; card.remove(); };
  $('#main').prepend(card);
  $('h3', card).focus();
}
function confirmLeaveEditor() {
  if (S.expCreate && S.wizardDirty) {
    if (!confirm(t('exp.wizardDiscard'))) return false;
    S.wizardDirty = false;
  }
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
    'flag.write': ['#flag-new', '#flag-save', '#flag-submit', '#flag-delete', '#gff-run', '#seg-new', '#seg-save', '#seg-submit', '#exp-bind'],
    'flag.approve': ['#flag-approve', '#flag-reject', '#seg-approve', '[data-appr]'],
    'flag.publish': ['#flag-publish', '#flag-emergency-disable', '[data-rb]', '[data-er]', '#seg-publish', '#exp-transition', '#ro-create', '#ro-pause', '#ro-resume', '#ro-advance', '#ro-takeover-on', '#ro-takeover-off', '[data-pin]', '[data-exec]', '#metric-publish', '#metric-deprecate'],
    'experiment.write': ['#exp-new', '#x-create', '#exp-review', '#metric-new', '#metric-submit'],
    'ai.write': ['#ai-reg', '[data-pin]', '[data-revoke]', '[data-reinstate]'],
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
    if (f.required) inp.required = true;
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
    $$('input, textarea', ov).filter(el => el.required).forEach(el => { el.value = el.value.trim(); });
    const invalid = $$('input, select, textarea', ov).find(el => !el.reportValidity());
    if (invalid) { invalid.focus(); return; }
    ok.disabled = true;
    const vals = {};
    for (const f of fields) vals[f.name] = body.querySelector('[name="' + f.name + '"]').value;
    try { await onSubmit(vals); close(); } catch (e) { reportError(e); ok.disabled = false; }
  };
}

function actionDialog({ title, summary, preview = '', reasonLabel = '', reasonRequired = true,
  confirmLabel = '', onConfirm = null }) {
  return new Promise(resolve => {
    const ov = document.createElement('div');
    const previousFocus = document.activeElement;
    ov.className = 'overlay';
    ov.innerHTML = '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="action-title" aria-describedby="action-summary">' +
      '<h3 id="action-title">' + esc(title) + '</h3><p id="action-summary">' + esc(summary) + '</p>' + preview +
      (reasonLabel ? '<label class="field">' + esc(reasonLabel) + '<textarea id="action-reason" rows="3"' + (reasonRequired ? ' required' : '') + '></textarea></label>' : '') +
      '<div id="action-feedback" role="alert"></div>' +
      '<div class="modal-foot"><button class="btn" id="action-cancel" type="button">' + esc(t('btn.cancel')) + '</button>' +
      '<button class="btn danger" id="action-confirm" type="button">' + esc(confirmLabel || t('btn.ok')) + '</button></div></div>';
    let busy = false;
    let settled = false;
    const finish = value => {
      if (settled) return;
      settled = true;
      ov.remove(); previousFocus?.focus?.(); resolve(value);
    };
    $('#action-cancel', ov).onclick = () => { if (!busy) finish(null); };
    ov.addEventListener('click', ev => { if (ev.target === ov && !busy) finish(null); });
    ov.addEventListener('keydown', ev => {
      if (ev.key === 'Escape') { ev.preventDefault(); if (!busy) finish(null); }
      if (ev.key === 'Tab') {
        const controls = $$('summary, textarea, button', ov);
        if (ev.shiftKey && document.activeElement === controls[0]) { ev.preventDefault(); controls.at(-1).focus(); }
        else if (!ev.shiftKey && document.activeElement === controls.at(-1)) { ev.preventDefault(); controls[0].focus(); }
      }
    });
    $('#action-confirm', ov).onclick = async () => {
      const reason = $('#action-reason', ov)?.value.trim() || '';
      if (reasonLabel && reasonRequired && !reason) { $('#action-reason', ov).focus(); return; }
      if (onConfirm) {
        const button = $('#action-confirm', ov);
        busy = true;
        button.disabled = true;
        try { await onConfirm({ reason }); }
        catch (e) {
          if (e.status === 401) { finish(null); return; }
          $('#action-feedback', ov).textContent = errMsg(e);
          button.disabled = false;
          busy = false;
          return;
        }
      }
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
// Experiment-detail zones (§4.4 四区域). 'summary' is the default and carries
// no hash segment; the legacy 'diagnostics' deep link is an alias of 'health'
// and is normalized on restore so old bookmarks keep working.
const EXP_TABS = ['summary', 'health', 'evidence', 'rollout', 'settings'];
const EXP_TAB_ALIASES = { diagnostics: 'health' };

function consoleHash() {
  const parts = ['console', S.ws || '~', S.env || '~', S.view];
  if (S.view === 'flags' && S.flagKey) parts.push(S.flagKey);
  if (S.view === 'segments' && S.segKey) parts.push(S.segKey);
  if (S.view === 'experiments' && S.expId) {
    parts.push(S.expId);
    if (S.expTab && S.expTab !== 'summary') parts.push(S.expTab);
  }
  if (S.view === 'experiments' && S.expCreate) parts.push('~new');
  if (S.view === 'metrics' && S.metricRef) parts.push(S.metricRef.key, String(S.metricRef.version));
  if (S.view === 'approvals' && S.approvalID) parts.push(S.approvalID);
  if (S.view === 'ai' && S.aiRef) parts.push(S.aiRef.type, S.aiRef.logicalId);
  if (S.view === 'audit' && S.auditObject) parts.push(S.auditObject);
  return '#/' + parts.map(encodeURIComponent).join('/');
}

function restoreConsoleHash(hash) {
  if (!hash.startsWith('#/console/')) return false;
  let parts;
  try { parts = hash.slice(2).split('/').map(decodeURIComponent); }
  catch { return false; }
  if (parts.length < 4 || parts[0] !== 'console' || !Object.hasOwn(VIEWS, parts[3])) return false;
  if (parts[3] === 'experiments' && (parts.length > 6 ||
    (parts[5] && (parts[4] === '~new' ||
      !(EXP_TABS.includes(parts[5]) || Object.hasOwn(EXP_TAB_ALIASES, parts[5])))))) return false;
  if (parts[3] === 'ai' && parts.length !== 4 && parts.length !== 6) return false;
  if (parts[3] === 'audit' && parts.length > 5) return false;
  S.ws = parts[1] === '~' ? '' : parts[1];
  S.env = parts[2] === '~' ? '' : parts[2];
  S.view = parts[3];
  S.flagKey = S.segKey = S.expId = S.metricRef = S.approvalID = S.aiRef = null;
  S.auditObject = S.view === 'audit' ? (parts[4] || '') : '';
  S.auditCursors = [0]; S.auditPage = 0;
  S.expCreate = false;
  S.expTab = 'summary';
  if (S.view === 'flags' && parts[4]) S.flagKey = parts[4];
  if (S.view === 'segments' && parts[4]) S.segKey = parts[4];
  if (S.view === 'experiments' && parts[4]) {
    if (parts[4] === '~new') S.expCreate = true;
    else {
      S.expId = parts[4];
      if (parts[5]) S.expTab = EXP_TAB_ALIASES[parts[5]] || parts[5];
    }
  }
  if (S.view === 'metrics' && parts[4] && /^[1-9]\d*$/.test(parts[5] || '')) {
    S.metricRef = { key: parts[4], version: Number(parts[5]) };
  }
  if (S.view === 'approvals' && parts[4]) S.approvalID = parts[4];
  if (S.view === 'ai' && parts[4] && parts[5]) S.aiRef = { type: parts[4], logicalId: parts[5] };
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
    S.reauthDraft = null;
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
    renderReauthDraft();
  } catch (e) {
    if (epoch !== routeEpoch) return;
    if (e.status === 401) return; // showAuth already triggered by api()
    m.innerHTML = errBanner(e);
    renderReauthDraft();
    const rb = $('#btn-reauth2', m);
    if (rb) rb.onclick = () => showAuth();
  }
}

// ---------------------------------------------------------------------------
// Auth screen & boot
// ---------------------------------------------------------------------------
function showAuth(msg, preserveEditor = false) {
  if (preserveEditor) captureReauthDraft();
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
  S.actorName = '';
  S.principalID = '';
  S.reauthDraft = null;
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
  const session = await api('/api/v1/admin/session');
  S.actorName = session.name || '';
  S.principalID = session.principalId || '';
  if (S.reauthDraft && S.reauthDraft.principalID !== S.principalID) S.reauthDraft = null;
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
// View: Overview — one server-owned read model (GET {ws}/overview): risk band,
// approvals waiting for me, recent releases and real counts. Every module
// degrades on its own with an honest "why missing" banner; the browser never
// re-derives myAction/blockedReason and never claims "no risks" beyond the
// coverage the server states.
// ---------------------------------------------------------------------------
const RELEASE_ACTION_CHIP = {
  'flag.publish': 'green', 'flag.rollback': 'red', 'flag.emergency_disable': 'red',
  'segment.publish': 'green', 'release_plan.apply_ratio': 'blue', 'release_plan.rollback': 'red',
};

// riskKindLabel maps the server's risk kinds to localized labels; an unknown
// kind passes through verbatim (server content is never translated).
function riskKindLabel(kind) {
  switch (kind) {
    case 'experiment_unhealthy': return t('overview.riskKind.experiment_unhealthy');
    case 'experiment_report_unavailable': return t('overview.reportUnavailable');
    case 'experiment_paused': return t('overview.riskKind.experiment_paused');
    case 'plan_paused': return t('overview.riskKind.plan_paused');
    case 'plan_rolled_back': return t('overview.riskKind.plan_rolled_back');
    default: return kind;
  }
}

// releaseActionLabel maps the audit whitelist actions; anything outside the
// whitelist falls back to the raw action string.
function releaseActionLabel(action) {
  switch (action) {
    case 'flag.publish': return t('release.action.flag.publish');
    case 'flag.rollback': return t('release.action.flag.rollback');
    case 'flag.emergency_disable': return t('release.action.flag.emergency_disable');
    case 'segment.publish': return t('release.action.segment.publish');
    case 'release_plan.apply_ratio': return t('release.action.release_plan.apply_ratio');
    case 'release_plan.rollback': return t('release.action.release_plan.rollback');
    default: return action;
  }
}

// overviewMyActionLabel renders the server's verdict about what the caller
// may do with a pending approval (review|execute).
function overviewMyActionLabel(myAction) {
  if (myAction === 'review') return t('overview.myAction.review');
  if (myAction === 'execute') return t('overview.myAction.execute');
  return myAction;
}

// overviewBlockedNote explains why an approval is not actionable for the
// caller; unknown reasons pass through verbatim.
function overviewBlockedNote(reason) {
  switch (reason) {
    case 'self_approval': return t('overview.blocked.self_approval');
    case 'role': return t('overview.blocked.role');
    case 'expired': return t('overview.blocked.expired');
    case 'breakglass_review': return t('overview.blocked.breakglass_review');
    default: return reason;
  }
}

// renderOverviewModuleFailure is the module-level degradation banner: why the
// module is missing (status/reason/coverage) plus a retry affordance. It never
// presents a missing module as an all-clear.
function renderOverviewModuleFailure(nameKey, module) {
  const status = module && module.status ? module.status : 'unavailable';
  const reason = module && module.reason ? module.reason : '—';
  const cov = module && module.coverage;
  return '<div class="banner warn" role="status"><p>' +
    esc(t('overview.moduleFailed', { module: t(nameKey), reason })) + '</p>' +
    '<p class="muted">' + esc(t('overview.moduleStatus', { status })) +
    (cov ? ' · ' + esc(t('overview.moduleCoverage', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' })) : '') +
    '</p><button class="btn sm" type="button" data-overview-retry>' + esc(t('overview.retry')) + '</button></div>';
}

// renderOverviewRiskBand shows server-owned risk facts verbatim. The
// "no blocking risks" all-clear appears only when status is ok AND coverage is
// complete; every degraded state uses the partial wording instead.
function renderOverviewRiskBand(ov) {
  const risks = ov && ov.risks;
  const head = '<section class="card overview-band" id="overview-band"><h3>' + esc(t('overview.riskBand')) + '</h3>';
  if (!risks || risks.status === 'unavailable' || !risks.status) {
    return head + renderOverviewModuleFailure('overview.riskBand', risks) + '</section>';
  }
  const items = risks.items || [];
  const cov = risks.coverage || {};
  const clear = risks.status === 'ok' && cov.complete;
  return head +
    (items.length ? '<ul class="attention-list">' + items.map(item => {
      const blocked = item.severity === 'block';
      return '<li><span>' +
        chip(blocked ? t('overview.riskBlock') : t('overview.riskWarn'), blocked ? 'red' : 'amber') +
        ' <strong class="mono">' + esc(item.id) + '</strong> · ' + esc(riskKindLabel(item.kind)) +
        (item.environmentId ? ' · <span class="muted">' + esc(item.environmentId) + '</span>' : '') +
        (item.text ? '<br><span class="muted">' + esc(item.text) + '</span>' : '') +
        '</span><button class="btn sm" type="button" data-overview-risk="' + esc(item.id) + '">' +
        esc(t('overview.openExperiment')) + '</button></li>';
    }).join('') + '</ul>' :
      '<p class="muted">' + esc(t(clear ? 'overview.riskBandClear' : 'overview.riskBandPartial')) + '</p>') +
    (risks.reportCoverage === 'restricted' || risks.status === 'restricted'
      ? '<p class="muted">' + esc(t('overview.reportRestricted')) + '</p>' : '') +
    '</section>';
}

// renderOverviewStats renders the four count cards; a null count means the
// data source failed and shows "—", never a fake zero.
function renderOverviewStats(counts) {
  const shown = v => v === null || v === undefined ? '—' : String(v);
  return '<div class="overview-grid">' +
    [[t('overview.paused'), counts ? counts.pausedExperiments : null],
      [t('overview.running'), counts ? counts.runningExperiments : null],
      [t('overview.pending'), counts ? counts.pendingApprovals : null],
      [t('overview.flags'), counts ? counts.publishedFlags : null]].map(([label, value]) =>
      '<section class="card overview-stat"><span class="muted">' + esc(label) + '</span><strong>' +
      esc(shown(value)) + '</strong></section>').join('') + '</div>';
}

// renderOverviewApprovals consumes the server's myAction/blockedReason fields
// as-is; the browser does not re-derive who may act.
function renderOverviewApprovals(pending) {
  const head = '<section class="card overview-pending" id="overview-pending"><h3>' +
    esc(t('overview.myApprovals')) + '</h3>';
  if (!pending || pending.status !== 'ok') {
    return head + renderOverviewModuleFailure('overview.myApprovals', pending) + '</section>';
  }
  const items = pending.items || [];
  return head +
    (pending.actionableTotal > 0 ? '<p>' + chip(t('overview.myApprovalsTotal',
      { n: pending.actionableTotal }), 'amber') + '</p>' : '') +
    (items.length ? '<ul class="attention-list">' + items.map(item =>
      '<li><span>' +
      '<button class="row-link mono" type="button" data-overview-approval="' + esc(item.id) + '">' +
      esc(item.action) + ' · ' + esc(item.object) + '</button> ' +
      (item.myAction ? chip(overviewMyActionLabel(item.myAction), item.myAction === 'review' ? 'amber' : 'green') : '') +
      (item.blockedReason ? '<span class="muted">' + esc(overviewBlockedNote(item.blockedReason)) + '</span>' : '') +
      '<br><span class="muted">' + esc(item.environmentId || '—') + ' · ' + esc(item.submittedBy || '—') +
      ' · ' + esc(fmtTime(item.submittedAt)) +
      (item.risk ? ' · ' + esc(t('approval.risk')) + ': ' + esc(item.risk) : '') +
      (item.expiresAt ? ' · ' + esc(t('approval.expires')) + ' ' + esc(fmtTime(item.expiresAt)) : '') +
      '</span></span><button class="btn sm" type="button" data-overview-approval="' + esc(item.id) + '">' +
      esc(t('overview.openApproval')) + '</button></li>').join('') + '</ul>' :
      '<p class="muted">' + esc(t('overview.myApprovalsEmpty')) + '</p>') +
    '</section>';
}

// renderOverviewReleases shows the whitelisted audit records (newest first)
// with the scan-window coverage statement; truncation is disclosed, never
// smoothed over with an estimated total.
function renderOverviewReleases(releases) {
  const head = '<section class="card overview-releases" id="overview-releases"><h3>' +
    esc(t('overview.recentReleases')) + '</h3>';
  if (!releases || releases.status !== 'ok') {
    return head + renderOverviewModuleFailure('overview.recentReleases', releases) + '</section>';
  }
  const items = releases.items || [];
  const cov = releases.coverage || {};
  const row = item => '<td class="nowrap">' + esc(fmtTime(item.time)) + '</td>' +
    '<td>' + chip(releaseActionLabel(item.action), RELEASE_ACTION_CHIP[item.action] || 'gray') +
    ' <span class="mono">' + esc(item.action) + '</span></td>' +
    '<td class="mono">' + esc(item.object || '—') + '</td><td>' + esc(item.actor || '—') + '</td>' +
    '<td class="mono">' + esc(item.environmentId || '—') + '</td>' +
    '<td><button class="btn sm" type="button" data-overview-release="' + esc(item.object || '') + '">' +
    esc(t('overview.openRelease')) + '</button></td>';
  return head +
    '<p class="muted">' + esc(t('overview.recentReleasesCoverage', { scanned: cov.inspected ?? '—' })) +
    (cov.complete ? '' : ' ' + esc(t('overview.recentReleasesTruncated'))) + '</p>' +
    (items.length ? '<table class="grid mobile-card-table"><thead><tr><th>' + esc(t('th.time')) +
      '</th><th>' + esc(t('th.action')) + '</th><th>' + esc(t('th.object')) + '</th><th>' +
      esc(t('th.actor')) + '</th><th>' + esc(t('th.env')) + '</th><th></th></tr></thead><tbody>' +
      items.map(item => '<tr>' + row(item) + '</tr>').join('') + '</tbody></table>' +
      '<div class="mobile-record-cards">' + items.map(item =>
        '<article class="mobile-record"><div class="row">' +
        chip(releaseActionLabel(item.action), RELEASE_ACTION_CHIP[item.action] || 'gray') +
        '<span class="muted">' + esc(fmtTime(item.time)) + '</span></div>' +
        '<div><b>' + esc(t('th.object')) + ':</b> <span class="mono">' + esc(item.object || '—') + '</span></div>' +
        '<div><b>' + esc(t('th.actor')) + ':</b> ' + esc(item.actor || '—') + '</div>' +
        '<div><b>' + esc(t('th.env')) + ':</b> ' + esc(item.environmentId || '—') + '</div>' +
        '<div><button class="btn sm" type="button" data-overview-release="' + esc(item.object || '') + '">' +
        esc(t('overview.openRelease')) + '</button></div></article>').join('') + '</div>' :
      '<p class="muted">' + esc(t('overview.recentReleasesEmpty')) + '</p>') +
    '</section>';
}

async function viewOverview(m) {
  if (!needScope(m)) return;
  const head = '<div class="view-head"><h2>' + esc(t('overview.title')) + '</h2>' +
    '<span class="muted">' + esc(t('overview.scope')) + ': ' + esc(S.ws) + ' / ' + esc(S.env) + '</span>' +
    '<button class="btn sm" id="overview-refresh">' + esc(t('overview.refresh')) + '</button></div>';
  let ov;
  try {
    ov = await api(wsPath() + '/overview?limit=20' + (S.env ? '&env=' + encodeURIComponent(S.env) : ''));
  } catch (e) {
    m.innerHTML = head + errBanner(e) + '<div class="row"><button class="btn sm" id="overview-retry" type="button">' +
      esc(t('overview.retry')) + '</button></div>';
    $('#overview-refresh', m).onclick = () => route();
    $('#overview-retry', m).onclick = () => route();
    const reauth = $('#btn-reauth2', m);
    if (reauth) reauth.onclick = () => showAuth();
    return;
  }
  const risks = (ov && ov.risks) || {};
  const cov = risks.coverage || {};
  const coverageNote = !risks.status || risks.status === 'unavailable' || risks.status === 'restricted'
    ? t('overview.reportCoverageUnavailable')
    : risks.status === 'ok' && cov.complete
      ? t('overview.reportCoverageComplete', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' })
      : t('overview.reportCoveragePartial', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' });
  m.innerHTML = head +
    renderOverviewRiskBand(ov) +
    renderOverviewStats(ov ? ov.counts : null) +
    renderOverviewApprovals(ov ? ov.pendingApprovals : null) +
    renderOverviewReleases(ov ? ov.recentReleases : null) +
    '<section class="card" id="overview-coverage"><p class="muted">' + esc(coverageNote) + '</p>' +
    (cov.total > cov.inspected
      ? '<button class="btn sm" id="overview-all-experiments">' + esc(t('overview.allExperiments')) + '</button>' : '') +
    '</section>';
  $('#overview-refresh', m).onclick = () => route();
  const allExperiments = $('#overview-all-experiments', m);
  if (allExperiments) allExperiments.onclick = () => { S.view = 'experiments'; S.expId = null; route(); };
  m.addEventListener('click', ev => {
    const target = ev.target;
    if (!target || typeof target.closest !== 'function') return;
    if (target.closest('[data-overview-retry]')) { route(); return; }
    const risk = target.closest('[data-overview-risk]');
    if (risk) {
      S.view = 'experiments'; S.expId = risk.dataset.overviewRisk;
      S.expCreate = false; S.expTab = 'summary';
      route();
      return;
    }
    const approval = target.closest('[data-overview-approval]');
    if (approval) { S.view = 'approvals'; S.approvalID = approval.dataset.overviewApproval; route(); return; }
    const release = target.closest('[data-overview-release]');
    if (release) {
      S.view = 'audit'; S.auditObject = release.dataset.overviewRelease; S.auditQuery = '';
      S.auditCursors = [0]; S.auditPage = 0;
      route();
    }
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
  if (typeof c !== 'object' || c === null || Array.isArray(c)) return [t('lint.notObject')];
  if (!['bool', 'string', 'number', 'json'].includes(c.type)) issues.push(t('lint.badType'));
  if (!c.variations || typeof c.variations !== 'object' || Array.isArray(c.variations)) {
    return [...issues, t('flags.editorNeedVariation')];
  }
  const vars = c.variations;
  if (!Object.keys(vars).length) issues.push(t('flags.editorNeedVariation'));
  for (const [name, value] of Object.entries(vars)) {
    const valid = c.type === 'json' ||
      (c.type === 'bool' && typeof value === 'boolean') ||
      (c.type === 'string' && typeof value === 'string') ||
      (c.type === 'number' && typeof value === 'number' && Number.isFinite(value));
    if (!valid) issues.push(t('flags.editorValueType', { name }));
  }
  const checkRule = (r, where) => {
    if (!r || typeof r !== 'object' || Array.isArray(r)) { issues.push(t('lint.ruleMissing', { where })); return; }
    const hasVar = r.variation !== undefined, hasPct = r.percentage !== undefined;
    if (hasVar === hasPct) issues.push(t('lint.ruleExclusive', { where }));
    if (hasVar && !(r.variation in vars)) issues.push(t('lint.unknownVariation', { where, name: r.variation }));
    if (hasPct) {
      if (!r.percentage || typeof r.percentage !== 'object' || Array.isArray(r.percentage)) {
        issues.push(t('flags.editorPercentRange'));
      } else {
        const sum = Object.values(r.percentage).reduce((a, b) => a + Number(b), 0);
        if (!Number.isFinite(sum) || Math.abs(sum - 100) > 0.01) issues.push(t('lint.pctSum', { where, sum }));
        for (const [vn, percent] of Object.entries(r.percentage)) {
          if (!(vn in vars)) issues.push(t('lint.unknownVariation', { where, name: vn }));
          if (typeof percent !== 'number' || !Number.isFinite(percent) || percent < 0 || percent > 100) {
            issues.push(t('flags.editorPercentRange'));
          }
        }
      }
    }
    if (r.constraints && !Array.isArray(r.constraints)) {
      issues.push(t('flags.editorConstraintsArray'));
      return;
    }
    for (const con of r.constraints || []) {
      if (!con || typeof con !== 'object' || Array.isArray(con)) {
        issues.push(t('flags.editorConstraintsArray'));
        continue;
      }
      if (!con.field) issues.push(t('lint.constraintField', { where }));
      if (!OPS.includes(con.op)) issues.push(t('lint.unknownOp', { where, op: con.op }));
    }
  };
  checkRule(c.defaultRule, 'defaultRule');
  if (c.targeting && !Array.isArray(c.targeting)) issues.push(t('flags.editorRulesArray'));
  else (c.targeting || []).forEach((r, i) => checkRule(r, 'targeting[' + i + ']'));
  return issues;
}

async function viewFlags(m) {
  if (!needScope(m)) return;
  if (S.flagKey) return viewFlagDetail(m);
  const flags = sortFlagsForList((await api(scopePath() + '/flags')) || []);
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('flags.title')) + '</h2><span class="muted">' + esc(t('flags.count', { n: flags.length })) + '</span>' +
    '<input class="list-search" type="search" placeholder="' + esc(t('list.search')) + '" aria-label="' + esc(t('list.search')) + '"><button class="btn primary sm" id="flag-new">' + esc(t('flags.new')) + '</button></div>' +
    '<div class="row"><label class="inline">' + esc(t('flags.statusFilter')) + ' <select id="flag-status-filter">' +
      '<option value="all">' + esc(t('flags.statusAll')) + '</option>' +
      '<option value="submitted">' + esc(t('flags.statusSubmitted')) + '</option>' +
      '<option value="approved">' + esc(t('flags.statusApproved')) + '</option>' +
      '<option value="editing">' + esc(t('flags.statusEditing')) + '</option>' +
      '<option value="published">' + esc(t('flags.statusPublished')) + '</option></select></label>' +
      '<span class="muted">' + esc(t('flags.sortNote')) + '</span></div>' +
    '<table class="grid flag-list-table"><thead><tr><th>' + esc(t('th.key')) + '</th><th>' + esc(t('th.type')) + '</th><th>' + esc(t('th.draftState')) + '</th><th>' + esc(t('th.revision')) + '</th><th>' + esc(t('th.updated')) + '</th></tr></thead><tbody>' +
    (flags.map(f => {
      const d = f.draft, c = f.current;
      const type = (d && d.content && d.content.type) || (c && c.content && c.content.type) || '—';
      const st = d
        ? chip(flagListStatusLabel(f), DRAFT_CHIP[d.state] || 'gray') +
          (d.deleted ? ' ' + chip(t('chip.del'), 'red') : '')
        : (c ? chip(t('chip.published'), 'green') : chip(t('chip.none'), 'gray'));
      const upd = (d && d.updatedAt) || (c && c.publishedAt) || null;
      return '<tr data-flag-record data-key="' + esc(f.key) + '" data-category="' + esc(flagListCategory(f)) + '"><td class="mono"><button class="row-link" type="button" data-flag="' + esc(f.key) + '">' + esc(f.key) + '</button>' +
        '</td><td>' + esc(type) + '</td><td>' + st +
        (flagRunningChange(f) ? ' ' + chip(t('flags.runningChange'), 'red') : '') +
        '</td><td>' + esc(f.currentRevision || 0) +
        '</td><td class="nowrap">' + esc(fmtTime(upd)) + '</td></tr>';
    }).join('') || '<tr><td colspan="5" class="muted">' + esc(t('flags.empty')) + '</td></tr>') +
    '</tbody></table>' +
    '<div class="flag-record-cards">' + flags.map(f => {
      const d = f.draft, c = f.current;
      const status = flagListStatusLabel(f);
      const upd = (d && d.updatedAt) || (c && c.publishedAt) || null;
      return '<article class="flag-record" data-flag-record data-key="' + esc(f.key) + '" data-category="' + esc(flagListCategory(f)) + '">' +
        '<button class="row-link mono" type="button" data-flag="' + esc(f.key) + '">' + esc(f.key) + '</button>' +
        '<div>' + esc(status) + ' · r' + esc(f.currentRevision || 0) + '</div>' +
        (flagRunningChange(f) ? '<div>' + chip(t('flags.runningChange'), 'red') + '</div>' : '') +
        '<div class="muted">' + esc(fmtTime(upd)) + '</div></article>';
    }).join('') + '</div>' +
    (flags.length ? '' : '<p class="muted flag-empty-mobile">' + esc(t('flags.empty')) + '</p>') +
    '<p class="muted hidden" id="flag-filter-empty">' + esc(t('flags.noMatch')) + '</p>';
  const applyFilter = () => {
    const query = $('.list-search', m).value.trim().toLocaleLowerCase();
    const category = $('#flag-status-filter', m).value;
    let visible = 0;
    $$('[data-flag-record]', m).forEach(record => {
      const match = (category === 'all' || record.dataset.category === category) &&
        (!query || record.dataset.key.toLocaleLowerCase().includes(query));
      record.hidden = !match;
      if (match && record.tagName === 'TR') visible++;
    });
    $('#flag-filter-empty', m).classList.toggle('hidden', visible > 0 || flags.length === 0);
  };
  $('.list-search', m).oninput = applyFilter;
  $('#flag-status-filter', m).onchange = applyFilter;
  $('#flag-new', m).onclick = () => promptForm(t('flags.new'), [
    { name: 'key', label: t('prompt.newFlagKey'), required: true },
  ], t('btn.create'), ({ key }) => {
    S.flagKey = key.trim(); S.flagNew = true; route();
  });
  m.addEventListener('click', ev => {
    const tr = ev.target.closest('[data-flag]');
    if (tr) { S.flagKey = tr.dataset.flag; S.flagNew = false; route(); }
  });
}

function flagListCategory(flag) {
  return flag.draft?.state || (flag.current ? 'published' : 'editing');
}

function flagListStatusLabel(flag) {
  return {
    submitted: t('flags.statusSubmitted'),
    approved: t('flags.statusApproved'),
    editing: t('flags.statusEditing'),
    published: t('chip.published'),
  }[flagListCategory(flag)] || flagListCategory(flag);
}

function flagRunningChange(flag) {
  return flag.current?.content?.experiment?.state === 'running' && flag.draft?.risk === 'high';
}

function sortFlagsForList(flags) {
  const rank = flag => {
    if (flagRunningChange(flag)) return 0;
    return { submitted: 1, approved: 2, editing: 3, published: 4 }[flagListCategory(flag)] ?? 5;
  };
  const updated = flag => Date.parse(flag.draft?.updatedAt || flag.current?.publishedAt || '') || 0;
  return [...flags].sort((a, b) => rank(a) - rank(b) ||
    updated(b) - updated(a) ||
    String(a.key).localeCompare(String(b.key)));
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

function canonicalFlagValue(value) {
  if (Array.isArray(value)) return value.map(canonicalFlagValue);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonicalFlagValue(value[key])]));
  }
  return value;
}

function flagDiffValue(value) {
  return value === undefined ? '—' : JSON.stringify(canonicalFlagValue(value), null, 2);
}

function renderFlagDiff(current, draft, options = {}) {
  const title = options.title || t('flags.diffTitle');
  if (!draft) return '<section class="card"><h3>' + esc(title) + '</h3><p class="muted">' +
    esc(t('flags.diffNoDraft')) + '</p></section>';
  if (draft.deleted) return '<section class="card"><h3>' + esc(title) + '</h3><div class="banner warn">' +
    esc(t('flags.diffDeleted')) + '</div></section>';
  const before = current?.content || {};
  const after = draft.content || {};
  const labels = {
    type: t('flags.diffType'), disable: t('flags.diffDisable'), bucketing: t('flags.diffBucketing'),
    fields: t('flags.diffFields'), variations: t('flags.diffVariations'),
    defaultRule: t('flags.diffDefaultRule'), targeting: t('flags.diffTargeting'),
    experiment: t('flags.diffExperiment'),
  };
  const keys = Object.keys(labels).filter(key => flagDiffValue(before[key]) !== flagDiffValue(after[key]));
  return '<section class="card"><h3>' + esc(title) + '</h3>' +
    (options.hideBase ? '' : '<p class="muted">' + esc(t('flags.diffBase', { revision: current?.revision || 0 })) + '</p>') +
    (keys.length ? '<div class="flag-diff-list">' + keys.map(key =>
      '<details class="flag-diff-item"><summary>' + esc(labels[key]) + '</summary>' +
      '<div class="flag-diff-values"><div><strong>' + esc(options.beforeLabel || t('flags.diffBefore')) +
      '</strong><pre class="pre small">' + esc(flagDiffValue(before[key])) + '</pre></div>' +
      '<div><strong>' + esc(options.afterLabel || t('flags.diffAfter')) + '</strong><pre class="pre small">' +
      esc(flagDiffValue(after[key])) + '</pre></div></div></details>').join('') + '</div>'
      : '<p>' + esc(options.emptyLabel || t('flags.diffNoChanges')) + '</p>') + '</section>';
}

function renderFlagSaveConflict(latest, local) {
  return '<div class="banner warn" role="alert"><strong>' + esc(t('flags.conflictTitle')) + '</strong>' +
    '<p>' + esc(t('flags.conflictHelp', { revision: latest.currentRevision || 0 })) + '</p></div>' +
    renderFlagDiff(latest.current, { content: local },
      { title: t('flags.conflictCompare'), afterLabel: t('flags.conflictLocal'),
        emptyLabel: t('flags.conflictNoChanges') }) +
    (latest.draft ? renderFlagDiff({ content: latest.draft.content }, { content: local },
      { title: t('flags.conflictCompareDraft'), beforeLabel: t('flags.conflictRemoteDraft'),
        afterLabel: t('flags.conflictLocal'), hideBase: true,
        emptyLabel: t('flags.conflictNoDraftChanges') }) : '') +
    '<button class="btn" type="button" id="flag-conflict-ack">' + esc(t('flags.conflictAcknowledge')) + '</button>';
}

async function confirmFlagPublish(key, kp) {
  if (!confirmLeaveEditor()) return;
  let reviewed;
  try { reviewed = await api(kp); }
  catch (e) { reportError(e); return; }
  const reviewContent = value => reviewPreview(value?.preview, 'action-review-preview') +
    renderFlagDiff(value?.current, value?.draft);
  const accepted = await actionDialog({ title: t('btn.publish'), summary: t('action.flagSummary', { key }),
    preview: '<div id="flag-publish-review">' + reviewContent(reviewed) + '</div>',
    reasonLabel: t('prompt.publishNote'), confirmLabel: t('btn.publish'), onConfirm: async ({ reason }) => {
      const latest = await api(kp);
      if (latest.draftHash !== reviewed.draftHash || latest.currentRevision !== reviewed.currentRevision ||
        JSON.stringify(latest.experimentImpact) !== JSON.stringify(reviewed.experimentImpact)) {
        reviewed = latest;
        const box = $('#flag-publish-review');
        if (box) box.innerHTML = reviewContent(latest);
        throw new ApiError(0, 'flag_review_changed', t('flags.reviewChanged'));
      }
      const result = await api(kp + '/publish', { method: 'POST', body: { note: reason } });
      toast(t('toast.published', { flag: result?.flagRevision, manifest: result?.manifestRevision }));
    } });
  if (accepted) route();
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
    '<div id="flag-saved-diff">' + renderFlagDiff(f && f.current, draft) + '</div>' +
    renderFlagExperimentImpact(f?.experimentImpact) +
    '<div id="flag-conflict"></div>' +
    '<div class="cols">' +
      '<section class="card"><h3>' + esc(t('flags.draftEditor')) + '</h3>' +
        '<div class="row flag-editor-modes" role="group" aria-label="' + esc(t('flags.editorMode')) + '">' +
        '<button type="button" class="btn sm" id="flag-mode-structured" aria-pressed="true">' +
          esc(t('flags.editorStructured')) + '</button>' +
        '<button type="button" class="btn sm" id="flag-mode-json" aria-pressed="false">' +
          esc(t('flags.editorAdvanced')) + '</button></div>' +
        '<div id="flag-structured"></div>' +
        '<div id="flag-advanced" class="hidden"><p class="muted">' + esc(t('flags.editorAdvancedHint')) + '</p>' +
          '<textarea id="flag-json" class="json" rows="24" spellcheck="false"></textarea></div>' +
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
  if (!can('flag.write')) {
    $$('input, select, textarea, button', $('#flag-structured', m)).forEach(control => { control.disabled = true; });
    $('#flag-json', m).readOnly = true;
    $('#flag-risk', m).disabled = true;
    $('#flag-base', m).disabled = true;
  }
}

function bindFlagDetail(m, key, kp, f) {
  let savedDraftHash = f?.draftHash || '';
  const structured = bindFlagStructured(m);
  m.addEventListener('click', ev => {
    const link = ev.target.closest('[data-exp-from-flag]');
    if (!link || !confirmLeaveEditor()) return;
    S.view = 'experiments'; S.expId = link.dataset.expFromFlag; S.expTab = 'summary';
    S.flagKey = null; route();
  });
  $('#flag-back', m).onclick = () => { if (!confirmLeaveEditor()) return; S.flagKey = null; S.flagNew = false; route(); };
  // Returns parsed content, or null (with toast) when invalid.
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#flag-json', m).value); }
    catch (e) { toast(t('toast.invalidJson', { msg: e.message }), false); return null; }
    if (!c || c.key !== key) { toast(t('toast.keyMismatch', { key }), false); return null; }
    const issues = lintFlag(c);
    if (issues.length) { toast(issues.join(' · '), false); return null; }
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
  $('#flag-save', m).onclick = async () => {
    if (!structured.valid()) return;
    const content = readJSON();
    if (!content) return;
    const button = $('#flag-save', m);
    button.disabled = true;
    try {
      await api(kp + '/draft', {
        method: 'PUT',
        body: { content, risk: $('#flag-risk', m).value, baseRevision: Number($('#flag-base', m).value || 0),
          expectedDraftHash: savedDraftHash },
      });
      S.editorBaseline = editorValue();
      toast(t('toast.draftSaved'));
      route();
    } catch (err) {
      if (err.status === 409) {
        try {
          const latest = await api(kp);
          $('#flag-saved-diff', m).innerHTML = renderFlagDiff(latest.current, latest.draft);
          $('#flag-conflict', m).innerHTML = renderFlagSaveConflict(latest, content);
          $('#flag-conflict-ack', m).onclick = () => {
            savedDraftHash = latest.draftHash || '';
            $('#flag-base', m).value = latest.currentRevision || 0;
            $('#flag-conflict', m).innerHTML = '<div class="banner ok">' +
              esc(t('flags.conflictAccepted')) + '</div>';
          };
          $('#flag-conflict', m).scrollIntoView({ block: 'start', behavior: 'smooth' });
        } catch (refreshErr) {
          $('#flag-conflict', m).innerHTML = errBanner(refreshErr);
        }
      }
      toast(errMsg(err), false);
      if (err.status === 403) {
        try { await refreshCapabilities(); gateActions(m); } catch { /* keep the unsaved editor visible */ }
      }
    } finally {
      button.disabled = false;
    }
  };
  $('#flag-submit', m).onclick = () => {
    if (!confirmLeaveEditor()) return;
    promptForm(t('btn.submit'), [{ name: 'reason', label: t('approval.reason'), required: true }],
      t('btn.submit'), async ({ reason }) => {
        await api(kp + '/submit', { method: 'POST', body: { reason } });
        toast(t('toast.draftSubmitted'));
        route();
      });
  };
  $('#flag-approve', m).onclick = () => {
    if (!confirmLeaveEditor()) return;
    promptForm(t('btn.approve'), [{ name: 'note', label: t('prompt.approveNote') }],
      t('btn.approve'), async ({ note }) => {
        await api(kp + '/approve', { method: 'POST', body: { note } });
        toast(t('toast.draftApproved'));
        route();
      });
  };
  $('#flag-reject', m).onclick = () => {
    if (!confirmLeaveEditor()) return;
    promptForm(t('btn.reject'), [{ name: 'note', label: t('prompt.rejectReason'), required: true }],
      t('btn.reject'), async ({ note }) => {
        await api(kp + '/reject', { method: 'POST', body: { note } });
        toast(t('toast.draftRejected'));
        route();
      });
  };
  $('#flag-publish', m).onclick = () => confirmFlagPublish(key, kp);
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
    '<table class="grid mobile-card-table"><thead><tr><th>' + esc(t('th.key')) + '</th><th>' + esc(t('th.state')) + '</th><th>' + esc(t('th.risk')) + '</th><th>' + esc(t('th.updatedBy')) + '</th><th>' + esc(t('th.updated')) + '</th></tr></thead><tbody>' +
    (S.segDrafts.map(d =>
      '<tr data-list-record><td class="mono"><button class="row-link" type="button" data-seg="' + esc(d.key) + '">' + esc(d.key) + '</button></td><td>' +
      chip(d.state, DRAFT_CHIP[d.state] || 'gray') + '</td><td>' + esc(d.risk || '—') + '</td><td>' +
      esc(d.updatedBy || '—') + '</td><td class="nowrap">' + esc(fmtTime(d.updatedAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">' + esc(t('seg.empty')) + '</td></tr>') +
    '</tbody></table><div class="mobile-record-cards">' + S.segDrafts.map(d =>
      '<article class="mobile-record" data-list-record><button class="row-link mono" type="button" data-seg="' +
      esc(d.key) + '">' + esc(d.key) + '</button><div>' + chip(d.state, DRAFT_CHIP[d.state] || 'gray') +
      ' · ' + esc(d.risk || '—') + '</div><div class="muted">' + esc(d.updatedBy || '—') +
      ' · ' + esc(fmtTime(d.updatedAt)) + '</div></article>').join('') + '</div>' +
    (S.segDrafts.length ? '' : '<p class="muted mobile-empty">' + esc(t('seg.empty')) + '</p>') +
    '<p class="muted hidden" data-list-no-match>' + esc(t('list.noMatch')) + '</p>';
  bindListSearch(m);
  $('#seg-new', m).onclick = () => promptForm(t('seg.new'), [
    { name: 'key', label: t('prompt.newSegKey'), required: true },
  ], t('btn.create'), ({ key }) => {
    S.segKey = key.trim(); route();
  });
  m.addEventListener('click', ev => {
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
  let previewUnavailable = false;
  let dependencies = { segmentRevision: 0, flags: [] };
  let history = { currentRevision: 0, revisions: [] };
  if (d) {
    const [previewResult, dependenciesResult, historyResult] = await Promise.allSettled([
      api(kp + '/preview'), api(kp + '/dependencies'), api(kp + '/history'),
    ]);
    if (previewResult.status === 'fulfilled') preview = previewResult.value;
    if (!preview) previewUnavailable = true;
    if (dependenciesResult.status === 'fulfilled') dependencies = dependenciesResult.value;
    else dependencies = null;
    if (historyResult.status === 'fulfilled') history = historyResult.value;
    else history = null;
  }
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="seg-back">' + esc(t('seg.back')) + '</button>' +
    '<h2 class="mono">' + esc(key) + '</h2>' +
    (d ? chip(d.state, DRAFT_CHIP[d.state] || 'gray') +
      ' <span class="muted">' + esc(t('flags.riskMeta', { risk: d.risk || '—', by: d.updatedBy || '—', time: fmtTime(d.updatedAt) })) + '</span>'
      : chip(t('chip.new'), 'gray')) + '</div>' +
    '<section class="card"><h3>' + esc(t('seg.draft')) + '</h3>' +
    '<div class="row flag-editor-modes" role="group" aria-label="' + esc(t('seg.editorMode')) + '">' +
      '<button type="button" class="btn sm" id="seg-mode-structured" aria-pressed="true">' + esc(t('flags.editorStructured')) + '</button>' +
      '<button type="button" class="btn sm" id="seg-mode-json" aria-pressed="false">' + esc(t('flags.editorAdvanced')) + '</button></div>' +
    '<div id="seg-structured"></div>' +
    '<div id="seg-advanced" class="hidden"><textarea id="seg-json" class="json" rows="18" spellcheck="false"></textarea></div>' +
    '<div class="row"><button class="btn sm" id="seg-validate">' + esc(t('btn.validateJson')) + '</button><span id="seg-valid" class="muted grow"></span>' +
    '<label class="inline">' + esc(t('label.risk')) + ' <select id="seg-risk"><option value="normal">normal</option><option value="high">high</option></select></label></div>' +
    '<div class="row actions">' +
    '<button class="btn primary" id="seg-save">' + esc(t('btn.saveDraft')) + '</button>' +
    '<button class="btn" id="seg-submit">' + esc(t('btn.submit')) + '</button>' +
    '<button class="btn" id="seg-approve">' + esc(t('btn.approve')) + '</button>' +
    '<button class="btn" id="seg-publish"' + (previewUnavailable ? ' disabled' : '') + '>' +
      esc(t('btn.publish')) + '</button></div></section>' +
    (previewUnavailable ? '<div class="banner warn" role="status">' + esc(t('seg.previewUnavailable')) + '</div>' :
      reviewPreview(preview)) + renderSegmentMatchForm() + renderSegmentDependencies(dependencies) +
      '<div id="seg-history">' + renderSegmentHistory(history) + '</div>';
  $('#seg-json', m).value = JSON.stringify(content, null, 2);
  if (d && d.risk) $('#seg-risk', m).value = d.risk;
  const structured = bindSegmentStructured(m);
  if (!can('flag.write')) {
    $$('input, select, button', $('#seg-structured', m)).forEach(control => { control.disabled = true; });
    $('#seg-json', m).readOnly = true;
    $('#seg-risk', m).disabled = true;
  }
  $('#seg-back', m).onclick = () => { if (!confirmLeaveEditor()) return; S.segKey = null; route(); };
  $('#seg-history', m).addEventListener('change', event => {
    if (event.target.id === 'seg-history-select') {
      $('#seg-history', m).innerHTML = renderSegmentHistory(history, Number(event.target.value));
    }
  });
  m.addEventListener('click', event => {
    const flag = event.target.closest('[data-dependent-flag]');
    if (!flag || !confirmLeaveEditor()) return;
    S.view = 'flags'; S.flagKey = flag.dataset.dependentFlag; S.segKey = null; route();
  });
  m.addEventListener('input', event => {
    if (event.target.closest('#seg-structured') ||
        ['seg-json', 'seg-match-context'].includes(event.target.id)) {
      $('#seg-match-result', m).textContent = '';
    }
  });
  m.addEventListener('click', event => {
    if (event.target.closest('[data-remove-condition], #seg-add-condition')) {
      $('#seg-match-result', m).textContent = '';
    }
  });
  $('#seg-validate', m).onclick = () => {
    if (!structured.valid()) return;
    const out = $('#seg-valid', m);
    try {
      const c = JSON.parse($('#seg-json', m).value);
      const issues = lintSegment(c);
      out.className = (c && c.key === key && !issues.length) ? 'ok-text grow' : 'warn-text grow';
      out.textContent = !c || c.key !== key ? t('validate.keyShould', { key }) :
        issues.length ? issues.join(' · ') : t('validate.valid');
    } catch (e) { out.className = 'err-text grow'; out.textContent = t('validate.invalidJson', { msg: e.message }); }
  };
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#seg-json', m).value); }
    catch (e) { toast(t('toast.invalidJson', { msg: e.message }), false); return null; }
    if (!c || c.key !== key) { toast(t('toast.keyMismatch', { key }), false); return null; }
    const issues = lintSegment(c);
    if (issues.length) { toast(issues.join(' · '), false); return null; }
    return c;
  };
  $('#seg-match-run', m).onclick = async () => {
    if (!structured.valid()) return;
    const content = readJSON();
    if (!content) return;
    let context;
    try { context = parseSegmentPreviewContext($('#seg-match-context', m).value); }
    catch {
      $('#seg-match-result', m).textContent = t('seg.matchBadContext');
      return;
    }
    const contentText = $('#seg-json', m).value;
    const contextText = $('#seg-match-context', m).value;
    const button = $('#seg-match-run', m);
    const resultBox = $('#seg-match-result', m);
    button.disabled = true;
    resultBox.textContent = t('misc.loading');
    try {
      const result = await api(kp + '/match-preview', { method: 'POST', body: { content, context } });
      if ($('#seg-json', m).value !== contentText || $('#seg-match-context', m).value !== contextText) {
        resultBox.textContent = t('seg.matchStale');
      } else resultBox.innerHTML = renderSegmentMatchResult(result);
    } catch (error) { resultBox.textContent = errMsg(error); }
    finally { button.disabled = false; }
  };
  $('#seg-save', m).onclick = () => {
    if (!structured.valid()) return;
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
    if (!confirmLeaveEditor()) return;
    const accepted = await actionDialog({ title: t('btn.publish'), summary: t('action.flagSummary', { key }),
      preview: reviewPreview(preview, 'action-review-preview'), reasonLabel: t('prompt.publishNote'),
      confirmLabel: t('btn.publish'), onConfirm: async ({ reason }) => {
        await api(kp + '/publish', { method: 'POST', body: { note: reason } });
        toast(t('toast.segPublished'));
      } });
    if (accepted) route();
  };
  return Promise.resolve();
}

// ---------------------------------------------------------------------------
// View: Experiments
// ---------------------------------------------------------------------------
const NS_HOUR = 3.6e12, NS_DAY = 8.64e13;
const expList = { scope: '', offset: 0, q: '', state: '' };

function expReportCell(report) {
  if (report.status === 'pending') return chip(t('exp.listPending'), 'gray');
  if (report.status === 'restricted') return chip(t('exp.listRestricted'), 'gray');
  if (report.status === 'unavailable') return chip(t('exp.listUnavailable'), 'red');
  const health = chip(t(report.healthy ? 'exp.listHealthy' : 'exp.listUnhealthy'), report.healthy ? 'green' : 'amber');
  const verdict = report.verdict ? ' ' + chip(report.verdict, VERDICT_CHIP[report.verdict] || 'gray') : '';
  const issue = report.issue ? '<div class="muted exp-list-issue" title="' + esc(report.issue) + '">' + esc(report.issue) + '</div>' : '';
  return health + verdict + issue;
}

async function viewExperiments(m) {
  if (!needScope(m)) return;
  if (S.expCreate) return viewExpCreate(m);
  if (S.expId) return viewExpDetail(m);
  const scope = S.ws + '/' + S.env;
  if (expList.scope !== scope) Object.assign(expList, { scope, offset: 0, q: '', state: '' });
  const params = new URLSearchParams({ limit: '20', offset: String(expList.offset) });
  if (expList.q) params.set('q', expList.q);
  if (expList.state) params.set('state', expList.state);
  const page = await api(scopePath() + '/experiment-summaries?' + params);
  const rows = page.items || [];
  const first = page.total ? page.offset + 1 : 0;
  const last = page.offset + rows.length;
  m.innerHTML =
    '<div class="view-head"><h2>' + esc(t('exp.title')) + '</h2><span class="muted">' + esc(page.total) + '</span>' +
    '<button class="btn primary sm" id="exp-new">' + esc(t('exp.new')) + '</button></div>' +
    '<form class="exp-list-filters" id="exp-list-filter"><label>' + esc(t('exp.listSearch')) +
    '<input id="exp-list-q" type="search" value="' + esc(expList.q) + '" placeholder="' + esc(t('exp.listSearchHint')) + '"></label>' +
    '<label>' + esc(t('th.state')) + '<select id="exp-list-state"><option value="">' + esc(t('exp.listAllStates')) + '</option>' +
    ['draft', 'reviewed', 'scheduled', 'running', 'paused', 'stopped', 'analyzed', 'archived'].map(state =>
      '<option value="' + state + '"' + (expList.state === state ? ' selected' : '') + '>' + esc(state) + '</option>').join('') +
    '</select></label><button class="btn sm" type="submit">' + esc(t('exp.listApply')) + '</button></form>' +
    '<table class="grid mobile-card-table"><thead><tr><th>' + esc(t('th.id')) + '</th><th>' + esc(t('th.flag')) + '</th><th>' + esc(t('th.state')) + '</th><th>' + esc(t('exp.listHealth')) + '</th><th>' + esc(t('exp.listCreator')) + '</th><th>' + esc(t('th.updated')) + '</th></tr></thead><tbody>' +
    (rows.map(({ experiment: e, report }) =>
      '<tr><td class="mono"><button class="row-link" type="button" data-exp="' + esc(e.id) + '">' + esc(e.id) + '</button></td><td class="mono">' +
      esc(e.flagKey) + '</td><td>' + chip(e.state, EXP_CHIP[e.state] || 'gray') + '</td><td>' + expReportCell(report) +
      '</td><td>' + esc(e.createdBy || '—') + '</td><td class="nowrap">' + esc(fmtTime(e.updatedAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">' + esc(t('exp.empty')) + '</td></tr>') +
    '</tbody></table><div class="mobile-record-cards">' + rows.map(({ experiment: e, report }) =>
      '<article class="mobile-record" data-list-record><button class="row-link mono" type="button" data-exp="' +
      esc(e.id) + '">' + esc(e.id) + '</button><div>' + chip(e.state, EXP_CHIP[e.state] || 'gray') +
      ' · ' + esc(e.flagKey) + '</div><div>' + expReportCell(report) + '</div><div class="muted">' +
      esc(e.createdBy || '—') + ' · ' + esc(fmtTime(e.updatedAt)) + '</div></article>').join('') + '</div>' +
    (rows.length ? '' : '<p class="muted mobile-empty">' + esc(t('exp.empty')) + '</p>') +
    '<div class="exp-list-pager"><span class="muted">' + esc(t('exp.listRange', { first, last, total: page.total })) + '</span>' +
    '<button class="btn sm" id="exp-prev"' + (page.offset === 0 ? ' disabled' : '') + '>' + esc(t('exp.listPrev')) + '</button>' +
    '<button class="btn sm" id="exp-next"' + (last >= page.total ? ' disabled' : '') + '>' + esc(t('exp.listNext')) + '</button></div>';
  $('#exp-list-filter', m).onsubmit = ev => {
    ev.preventDefault();
    expList.q = $('#exp-list-q', m).value.trim();
    expList.state = $('#exp-list-state', m).value;
    expList.offset = 0;
    viewExperiments(m);
  };
  $('#exp-prev', m).onclick = () => { expList.offset = Math.max(0, page.offset - page.limit); viewExperiments(m); };
  $('#exp-next', m).onclick = () => { expList.offset = last; viewExperiments(m); };
  $('#exp-new', m).onclick = () => { S.expCreate = true; S.wizardDirty = false; S.expTab = 'summary'; route(); };
  $$('[data-exp]', m).forEach(button => {
    button.onclick = () => { S.expId = button.dataset.exp; S.expTab = 'summary'; route(); };
  });
}

async function viewExpCreate(m) {
  const metricPage = await api(wsPath() + '/metrics?status=published&limit=200');
  const publishedMetrics = metricPage.items || [];
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="exp-back">' + esc(t('exp.back')) + '</button><h2>' + esc(t('exp.newTitle')) + '</h2></div>' +
    '<section class="card">' +
    '<ol class="wizard-progress" id="x-progress" aria-label="' + esc(t('exp.wizardProgress')) + '">' +
      [t('exp.wizard.goal'), t('exp.wizard.split'), t('exp.wizard.metrics'),
        t('exp.wizard.exposure'), t('exp.wizard.review')].map((label, index) =>
        '<li data-wizard-step="' + index + '"><span>' + (index + 1) + '</span>' + esc(label) + '</li>').join('') + '</ol>' +
    '<div class="banner err" id="x-error" role="alert" hidden></div>' +
    '<div class="wizard-step" data-step="0"><h3 tabindex="-1">' + esc(t('exp.wizard.goal')) + '</h3>' +
    '<p class="muted">' + esc(t('exp.wizardGoalHelp', { ws: S.ws, env: S.env })) + '</p>' +
    '<div class="form-grid">' +
      '<label class="field">' + esc(t('exp.flagKey')) + '<input id="x-flag" placeholder="my-flag"></label>' +
    '</div>' +
    '<div id="x-flag-check" role="status" aria-live="polite"></div>' +
    '<label class="field">' + esc(t('exp.hypothesis')) + '<textarea id="x-hyp" rows="2"></textarea></label></div>' +
    '<div class="wizard-step" data-step="1" hidden><h3 tabindex="-1">' + esc(t('exp.wizard.split')) + '</h3>' +
    '<div class="form-grid">' +
      '<label class="field">' + esc(t('exp.unit')) + '<input id="x-unit" value="user_id"></label>' +
      '<label class="field">' + esc(t('exp.layer')) + '<input id="x-layer"></label>' +
      '<label class="field">' + esc(t('exp.elig')) + '<input id="x-elig" type="number" step="1" min="0" max="100"></label>' +
	  '<label class="field">' + esc(t('exp.allocationMode')) + '<select id="x-mode"><option value="fixed-split">fixed-split</option><option value="bandit">bandit</option></select></label>' +
    '</div>' +
    '<h4>' + t('exp.variantsHead') + '</h4>' +
    '<div id="x-vars"></div><button class="btn sm" id="x-add-var">' + esc(t('exp.addVariant')) + '</button>' +
    '<div class="form-grid" id="x-bandit" style="display:none">' +
      '<label class="field">' + esc(t('exp.policyVersion')) + '<input id="x-policy-version" value="bandit-v1"></label>' +
      '<label class="field">' + esc(t('exp.strategy')) + '<select id="x-strategy"><option value="thompson">Thompson</option><option value="epsilon-greedy">ε-greedy</option></select></label>' +
      '<label class="field">Prior α<input id="x-prior-a" type="number" step="0.1" value="1"></label>' +
      '<label class="field">Prior β<input id="x-prior-b" type="number" step="0.1" value="1"></label>' +
      '<label class="field">ε<input id="x-epsilon" type="number" step="0.01" value="0.1"></label>' +
      '<label class="field">' + esc(t('exp.feedbackDelayD')) + '<input id="x-feedback-delay" type="number" step="1" value="11"></label>' +
    '</div></div>' +
    '<div class="wizard-step" data-step="2" hidden><h3 tabindex="-1">' + esc(t('exp.wizard.metrics')) + '</h3>' +
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
    '<h4>' + esc(t('exp.guardrails')) + '</h4>' +
    '<div id="x-guards"></div><button class="btn sm" id="x-add-guard">' + esc(t('exp.addGuardrail')) + '</button></div>' +
    '<div class="wizard-step" data-step="3" hidden><h3 tabindex="-1">' + esc(t('exp.wizard.exposure')) + '</h3>' +
    '<label class="field">' + esc(t('exp.exposureMode')) + '<select id="x-exposure-mode"><option value="">' + esc(t('exp.chooseExposureMode')) + '</option><option value="onDecision">' + esc(t('exp.onDecision')) + '</option><option value="onRender">' + esc(t('exp.onRender')) + '</option></select></label>' +
    '<p class="muted" id="x-exposure-help">' + esc(t('exp.chooseExposureHelp')) + '</p>' +
    '<div class="form-grid" id="x-render-fields" hidden>' +
      '<label class="field">' + esc(t('exp.exposureTrigger')) + '<input id="x-exposure-trigger" maxlength="200" placeholder="' + esc(t('exp.exposureTriggerExample')) + '"></label>' +
      '<label class="field">' + esc(t('exp.integrationOwner')) + '<input id="x-integration-owner" maxlength="200" placeholder="' + esc(t('exp.integrationOwnerExample')) + '"></label></div></div>' +
    '<div class="wizard-step" data-step="4" hidden><h3 tabindex="-1">' + esc(t('exp.wizard.review')) + '</h3>' +
    '<h4>' + t('exp.statsHead') + '</h4>' +
    '<div class="form-grid">' +
      '<label class="field">Alpha<input id="x-alpha" type="number" step="0.001" value="0.05"></label>' +
      '<label class="field">Power<input id="x-power" type="number" step="0.01" value="0.8"></label>' +
      '<label class="field">MDE<input id="x-mde" type="number" step="0.001" value="0.01"></label>' +
      '<label class="field">' + esc(t('exp.minSample')) + '<input id="x-minsample" type="number" value="1000"></label>' +
      '<label class="field">' + esc(t('exp.minRuntimeH')) + '<input id="x-minruntime" type="number" step="0.5" value="168"></label>' +
      '<label class="field">' + esc(t('exp.windowD')) + '<input id="x-window" type="number" step="1" value="7"></label>' +
    '</div>' +
    '<label class="field">' + esc(t('exp.holmFamily')) + '<textarea id="x-holm" rows="5" placeholder="{&quot;id&quot;:&quot;primary&quot;,&quot;hypotheses&quot;:[...]}"></textarea></label>' +
    '<p class="muted">' + esc(t('exp.holmHelp')) + '</p>' +
    '<div class="wizard-review" id="x-review"></div>' +
    '<div class="row"><button class="btn primary" id="x-create">' + esc(t('exp.create')) + '</button></div></div>' +
    '<div class="row wizard-actions"><button class="btn" id="x-prev">' + esc(t('exp.wizardBack')) + '</button>' +
    '<span class="muted" id="x-step-label"></span><button class="btn primary" id="x-next">' + esc(t('exp.wizardNext')) + '</button></div>' +
    '</section>';
  const vars = $('#x-vars', m), guards = $('#x-guards', m);
  let step = 0;
  let flagView = null;
  const checkWizardFlag = async () => {
    const key = $('#x-flag', m).value.trim();
    const box = $('#x-flag-check', m);
    box.textContent = t('exp.flagCheckLoading');
    try {
      const view = await api(scopePath() + '/flags/' + encodeURIComponent(key));
      if (!m.isConnected || $('#x-flag', m)?.value.trim() !== key) return false;
      flagView = view.current ? view : null;
      box.innerHTML = flagView ? '<div class="banner ok">' +
        esc(t('exp.flagCheckReady', { n: view.currentRevision,
          variants: Object.keys(view.current.content?.variations || {}).join(', ') })) + '</div>' :
        '<div class="banner warn">' + esc(t('exp.flagCheckUnpublished', { key })) +
        ' <button type="button" class="btn sm" id="x-open-flag">' + esc(t('exp.flagOpen')) + '</button></div>';
      return !!flagView;
    } catch (error) {
      if (!m.isConnected || $('#x-flag', m)?.value.trim() !== key) return false;
      flagView = null;
      box.innerHTML = '<div class="banner warn">' + esc(t('exp.flagCheckFailed')) +
        ' ' + esc(errMsg(error)) + '</div>';
      return false;
    }
  };
  const showWizardError = (message, field) => {
    const error = $('#x-error', m);
    error.textContent = message;
    error.hidden = false;
    if (field) $('#' + field, m)?.focus();
  };
  const validateWizardStep = index => {
    if (index === 0) {
      if (!$('#x-flag', m).value.trim()) return [t('exp.wizardFlagRequired'), 'x-flag'];
      if (!$('#x-hyp', m).value.trim()) return [t('exp.wizardHypothesisRequired'), 'x-hyp'];
    }
    if (index === 1) {
      if (!$('#x-unit', m).value.trim()) return [t('exp.wizardUnitRequired'), 'x-unit'];
      const variants = $$('.vrow', vars).map(row => ({
        name: $('.v-name', row).value.trim(), proportion: Number($('.v-prop', row).value),
      }));
      if (variants.length < 2 || variants.some(v => !v.name)) return [t('toast.need2variants'), 'x-add-var'];
      if (new Set(variants.map(v => v.name)).size !== variants.length) return [t('exp.wizardDuplicateVariant'), 'x-vars'];
      const published = flagView?.current?.content?.variations || {};
      const unknown = variants.find(v => !Object.hasOwn(published, v.name));
      if (flagView && unknown) return [t('exp.flagVariantMissing', { name: unknown.name }), 'x-vars'];
      const sum = variants.reduce((total, v) => total + v.proportion, 0);
      if (variants.some(v => !Number.isFinite(v.proportion) || v.proportion <= 0) || Math.abs(sum - 100) > 0.01)
        return [t('toast.propSum', { sum }), 'x-vars'];
      if ($('#x-mode', m).value === 'bandit' && variants.length !== 2)
        return [t('exp.banditRequiresProportion'), 'x-mode'];
    }
    if (index === 2) {
      if (!$('#x-mkey', m).value.trim()) return [t('exp.wizardMetricRequired'), 'x-mkey'];
      if ($('#x-mtype', m).value === 'ratio' && !$('#x-denkey', m).value.trim())
        return [t('toast.ratioDenominator'), 'x-denkey'];
      if ($('#x-mtype', m).value === 'quantile' &&
          !(Number($('#x-qlevel', m).value) > 0 && Number($('#x-qlevel', m).value) < 1))
        return [t('toast.quantileLevel'), 'x-qlevel'];
      if ($('#x-mode', m).value === 'bandit' && $('#x-mtype', m).value !== 'proportion')
        return [t('exp.banditRequiresProportion'), 'x-mtype'];
    }
    if (index === 3) {
      if (!$('#x-exposure-mode', m).value) return [t('exp.chooseExposureHelp'), 'x-exposure-mode'];
      if ($('#x-exposure-mode', m).value === 'onRender') {
        if (!$('#x-exposure-trigger', m).value.trim())
          return [t('exp.exposureTriggerRequired'), 'x-exposure-trigger'];
        if (!$('#x-integration-owner', m).value.trim())
          return [t('exp.integrationOwnerRequired'), 'x-integration-owner'];
      }
    }
    if (index === 4) {
      if (!(Number($('#x-alpha', m).value) > 0 && Number($('#x-alpha', m).value) < 1) ||
          !(Number($('#x-minsample', m).value) > 0) || !(Number($('#x-window', m).value) > 0))
        return [t('exp.wizardStatsInvalid'), 'x-alpha'];
    }
    return null;
  };
  const showWizardStep = (index, focus = true) => {
    step = index;
    $('#x-error', m).hidden = true;
    $$('.wizard-step', m).forEach((section, i) => { section.hidden = i !== step; });
    $$('#x-progress li', m).forEach((item, i) => {
      item.classList.toggle('current', i === step);
      if (i === step) item.setAttribute('aria-current', 'step');
      else item.removeAttribute('aria-current');
    });
    $('#x-prev', m).hidden = step === 0;
    $('#x-next', m).hidden = step === 4;
    $('#x-step-label', m).textContent = t('exp.wizardStep', { current: step + 1, total: 5 });
    if (step === 4) {
      const metric = publishedMetrics.find(d => d.key + '/v' + d.version === $('#x-mref', m).value);
      const variants = $$('.vrow', vars).map(row => $('.v-name', row).value.trim() + ' ' + $('.v-prop', row).value + '%');
      const guardKeys = $$('.g-key', guards).map(input => input.value.trim()).filter(Boolean);
      $('#x-review', m).innerHTML = '<h4>' + esc(t('exp.wizardContract')) + '</h4><dl class="kv">' +
        '<dt>' + esc(t('exp.flagKey')) + '</dt><dd>' + esc($('#x-flag', m).value.trim()) + '</dd>' +
        '<dt>' + esc(t('exp.flagRevision')) + '</dt><dd>r' + esc(flagView?.currentRevision || 0) + '</dd>' +
        '<dt>' + esc(t('exp.hypothesis')) + '</dt><dd>' + esc($('#x-hyp', m).value.trim()) + '</dd>' +
        '<dt>' + esc(t('exp.unit')) + '</dt><dd>' + esc($('#x-unit', m).value.trim()) + '</dd>' +
        '<dt>' + esc(t('exp.eligShort')) + '</dt><dd>' + esc($('#x-elig', m).value || '100') + '%</dd>' +
        '<dt>' + esc(t('th.variants')) + '</dt><dd>' + esc(variants.join(' · ')) + '</dd>' +
        '<dt>' + esc(t('exp.primaryMetric')) + '</dt><dd>' + esc(metric ? metric.key + ' v' + metric.version : $('#x-mkey', m).value.trim()) + '</dd>' +
        (metric ? '<dt>' + esc(t('report.attributionWindow')) + '</dt><dd>' + esc(fmtDurNs(metric.attributionWindow)) + '</dd>' : '') +
        '<dt>' + esc(t('exp.guardrails')) + '</dt><dd>' + esc(guardKeys.length ? guardKeys.join(' · ') : t('misc.none')) + '</dd>' +
        '<dt>' + esc(t('exp.exposureMode')) + '</dt><dd>' + esc(t($('#x-exposure-mode', m).value === 'onRender' ? 'exp.onRender' : 'exp.onDecision')) + '</dd>' +
        ($('#x-exposure-mode', m).value === 'onRender'
          ? '<dt>' + esc(t('exp.exposureTrigger')) + '</dt><dd>' + esc($('#x-exposure-trigger', m).value.trim()) + '</dd>' +
            '<dt>' + esc(t('exp.integrationOwner')) + '</dt><dd>' + esc($('#x-integration-owner', m).value.trim()) + '</dd>' : '') +
        '<dt>' + esc(t('exp.window')) + '</dt><dd>' + esc($('#x-window', m).value) + ' ' + esc(t('exp.wizardDays')) + '</dd></dl>' +
        '<p>' + esc(t('exp.wizardSampleRule', { sample: $('#x-minsample', m).value, runtime: $('#x-minruntime', m).value })) + '</p>' +
        '<p class="muted">' + esc(t('exp.wizardDraftOnly')) + '</p>';
    }
    if (focus) $('.wizard-step:not([hidden]) h3', m)?.focus();
  };
  $('#x-prev', m).onclick = () => showWizardStep(step - 1);
  $('#x-next', m).onclick = async () => {
    const error = validateWizardStep(step);
    if (error) { showWizardError(...error); return; }
    if (step === 0) {
      const button = $('#x-next', m);
      button.disabled = true;
      const ready = await checkWizardFlag();
      if (!button.isConnected) return;
      button.disabled = false;
      if (!ready) { showWizardError(t('exp.flagCheckBlocked'), 'x-flag'); return; }
    }
    showWizardStep(step + 1);
  };
  $('#x-flag', m).addEventListener('input', () => {
    flagView = null;
    $('#x-flag-check', m).textContent = '';
  });
  m.addEventListener('click', event => {
    if (event.target.id !== 'x-open-flag' || !confirmLeaveEditor()) return;
    S.expCreate = false; S.wizardDirty = false; S.view = 'flags';
    S.flagKey = $('#x-flag', m).value.trim(); S.flagNew = false; route();
  });
  m.addEventListener('input', () => { S.wizardDirty = true; });
  m.addEventListener('change', () => { S.wizardDirty = true; });
  showWizardStep(0, false);
  const addVar = (name, p) => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="v-name" placeholder="' + esc(t('ph.variantName')) + '">' +
      '<input class="v-prop" type="number" step="1" min="0" max="100" placeholder="' + esc(t('ph.percent')) + '">' +
      '<button class="btn sm" title="' + esc(t('btn.remove')) + '">✕</button>';
    $('.v-name', row).value = name || '';
    $('.v-prop', row).value = p ?? '';
    $('button', row).onclick = () => { S.wizardDirty = true; row.remove(); };
    vars.appendChild(row);
  };
  addVar('control', 50);
  addVar('treatment', 50);
  $('#x-add-var', m).onclick = () => { S.wizardDirty = true; addVar(); };
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
    $('#x-render-fields', m).hidden = ev.target.value !== 'onRender';
    $('#x-exposure-help', m).textContent = t(ev.target.value === 'onRender' ? 'exp.onRenderHelp' :
      ev.target.value === 'onDecision' ? 'exp.onDecisionHelp' : 'exp.chooseExposureHelp');
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
    $('button', row).onclick = () => { S.wizardDirty = true; row.remove(); };
    guards.appendChild(row);
    S.wizardDirty = true;
  };
  $('#exp-back', m).onclick = () => {
    if (!confirmLeaveEditor()) return;
    S.expCreate = false; S.wizardDirty = false; route();
  };
  $('#x-create', m).onclick = async () => {
    const createButton = $('#x-create', m);
    if (createButton.disabled) return;
    createButton.disabled = true;
    try {
      const ready = await checkWizardFlag();
      if (!createButton.isConnected) return;
      if (!ready) { showWizardStep(0); showWizardError(t('exp.flagCheckBlocked'), 'x-flag'); return; }
      for (let index = 0; index < 5; index++) {
        const error = validateWizardStep(index);
        if (error) { showWizardStep(index); showWizardError(...error); return; }
      }
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
      if (body.exposureMode === 'onRender') {
        body.exposureTrigger = $('#x-exposure-trigger', m).value.trim();
        body.integrationOwner = $('#x-integration-owner', m).value.trim();
      }
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
        S.expCreate = false; S.wizardDirty = false; S.expId = exp.id; route();
      } catch (e) { reportError(e); }
    } finally { createButton.disabled = false; }
  };
  return Promise.resolve();
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
    '</select></label><form id="a-search-form" class="inline-search"><input id="a-search" type="search" maxlength="128" value="' + esc(S.auditQuery) + '" placeholder="' + esc(t('audit.search')) + '" aria-label="' + esc(t('audit.search')) + '"><button class="btn sm" type="submit">' + esc(t('audit.searchButton')) + '</button></form></div>' +
    (S.auditObject ? '<div class="banner" id="a-object-filter">' + esc(t('audit.objectFilter', { object: S.auditObject })) +
      ' <button class="btn sm" id="a-clear-object" type="button">' + esc(t('audit.clearObject')) + '</button></div>' : '') +
    '<div id="a-box"><p class="muted">' + esc(t('misc.loading')) + '</p></div>';
  let loadSeq = 0;
  const load = async () => {
    const seq = ++loadSeq;
    const cursor = S.auditCursors[S.auditPage] || 0;
    const params = 'limit=' + (S.auditLimit + 1) + '&q=' + encodeURIComponent(S.auditQuery) +
      (S.auditObject ? '&object=' + encodeURIComponent(S.auditObject) : '') +
      (cursor ? '&before=' + encodeURIComponent(cursor) : '');
    const page = (await api(wsPath() + '/audit?' + params)) || [];
    if (seq !== loadSeq) return;
    const rows = page.slice(0, S.auditLimit);
    const hasMore = page.length > S.auditLimit;
    const records = rows.map(entry => ({ entry,
      detail: typeof entry.detail === 'string' ? entry.detail : JSON.stringify(entry.detail ?? '') }));
    $('#a-box', m).innerHTML =
      '<table class="grid mobile-card-table"><thead><tr><th>' + esc(t('th.time')) + '</th><th>' + esc(t('th.actor')) + '</th><th>' + esc(t('th.action')) + '</th><th>' + esc(t('th.object')) + '</th><th>' + esc(t('th.env')) + '</th><th>' + esc(t('th.detail')) + '</th></tr></thead><tbody>' +
      (records.map(({ entry: a, detail }) => {
        return '<tr><td class="nowrap">' + esc(fmtTime(a.time)) + '</td><td>' + esc(a.actor || '—') +
          '</td><td class="mono">' + esc(a.action) + '</td><td class="mono">' + esc(a.object || '—') +
          '</td><td class="mono">' + esc(a.environmentId || '—') + '</td>' +
          '<td class="detail" title="' + esc(detail) + '">' + esc(detail) + '</td></tr>';
      }).join('') || '<tr><td colspan="6" class="muted">' + esc(t('audit.empty')) + '</td></tr>') +
      '</tbody></table><div class="mobile-record-cards" aria-label="' + esc(t('audit.title')) + '">' +
      (records.map(({ entry: a, detail }) => '<article class="mobile-record audit-record"><div class="row"><strong class="mono">' +
        esc(a.action) + '</strong><span class="muted">' + esc(fmtTime(a.time)) + '</span></div><div><b>' +
        esc(t('th.object')) + ':</b> <span class="mono">' + esc(a.object || '—') + '</span></div><div><b>' +
        esc(t('th.actor')) + ':</b> ' + esc(a.actor || '—') + '</div><div><b>' + esc(t('th.env')) +
        ':</b> ' + esc(a.environmentId || '—') + '</div>' + (detail ? '<details><summary>' +
        esc(t('th.detail')) + '</summary><pre>' + esc(detail) + '</pre></details>' : '') + '</article>').join('') ||
        '<p class="muted">' + esc(t('audit.empty')) + '</p>') + '</div>' +
      '<div class="pager"><button class="btn sm" id="a-prev" type="button"' + (S.auditPage ? '' : ' disabled') + '>' + esc(t('audit.previous')) + '</button><span>' + esc(t('audit.page', { n: S.auditPage + 1 })) + '</span><button class="btn sm" id="a-next" type="button"' + (hasMore ? '' : ' disabled') + '>' + esc(t('audit.next')) + '</button></div>';
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
  const clearObject = $('#a-clear-object', m);
  if (clearObject) clearObject.onclick = () => {
    S.auditObject = ''; S.auditCursors = [0]; S.auditPage = 0;
    route();
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
function renderAIResourceDetail(detail) {
  const resource = detail.resource;
  const pins = detail.pins || [];
  const source = {
    'embedded-gonacos': t('ai.sourceEmbedded'),
    'external-config': t('ai.sourceExternal'),
    unavailable: t('ai.sourceUnavailable'),
  }[detail.sourceMode] || t('ai.sourceUnavailable');
  return '<div class="view-head"><button type="button" class="btn sm" id="ai-back">' + esc(t('ai.back')) +
    '</button><h2 class="wrap-anywhere">' + esc(resource.type) + ' / ' + esc(resource.logicalId) + '</h2>' +
    chip(detail.revoked ? t('chip.revoked') : t('chip.active'), detail.revoked ? 'red' : 'green') + '</div>' +
    '<section class="card"><h3>' + esc(t('ai.resourceStatus')) + '</h3><dl class="detail-grid">' +
    '<dt>' + esc(t('ai.source')) + '</dt><dd>' + esc(source) + '</dd>' +
    '<dt>' + esc(t('ai.physicalId')) + '</dt><dd class="mono wrap-anywhere">' + esc(resource.physicalId) + '</dd>' +
    '<dt>' + esc(t('th.owner')) + '</dt><dd>' + esc(resource.owner) + '</dd>' +
    '<dt>' + esc(t('th.created')) + '</dt><dd>' + esc(fmtTime(resource.createdAt)) + '</dd>' +
    '<dt>' + esc(t('ai.latestPin')) + '</dt><dd>' + esc(pins[0]?.version || '—') + '</dd></dl>' +
    (detail.revoked ? '<div class="banner warn">' + esc(t('ai.revocationDetail', {
      reason: detail.revocationReason || '—' })) + '</div>' : '') +
    '<p class="muted">' + esc(t('ai.sourceNoHealth')) + '</p>' +
    '<div class="row actions"><button type="button" class="btn" data-pin="1">' + esc(t('btn.pinVersion')) + '</button>' +
    (detail.revoked ? '<button type="button" class="btn" data-reinstate="1">' + esc(t('btn.reinstate')) + '</button>' :
      '<button type="button" class="btn danger" data-revoke="1">' + esc(t('btn.revoke')) + '</button>') +
    '</div></section><section class="card"><h3>' + esc(t('ai.pinnedHistory')) + '</h3>' +
    (pins.length ? '<ol class="ai-pin-history">' + pins.map(pin => '<li><strong class="mono">' + esc(pin.version) +
      '</strong><span>' + esc(fmtTime(pin.pinnedAt)) + ' · ' + esc(pin.pinnedBy) +
      '</span><span class="mono wrap-anywhere">' + esc(pin.digest) + '</span></li>').join('') + '</ol>' :
      '<p class="muted">' + esc(t('ai.noPins')) + '</p>') +
    '<div class="banner info">' + esc(t('ai.dependenciesUnknown')) + '</div></section>';
}

async function handleAIResourceAction(action, type, logical) {
  const path = scopePath() + '/ai/resources/' + encodeURIComponent(type) + '/' + encodeURIComponent(logical);
  if (action === 'pin') {
    promptForm(t('ai.pinTitle', { type, id: logical }),
      [{ name: 'content', label: t('ai.content'), type: 'textarea', rows: 10, placeholder: t('ai.contentPh') }],
      t('btn.pin'), async values => {
        if (!values.content) throw new ApiError(0, 'validation_failed', t('err.contentRequired'));
        await requestChange('ai.pin', type + '/' + logical, { content: values.content });
      });
    return;
  }
  const revoke = action === 'revoke';
  const approval = await actionDialog({ title: t(revoke ? 'btn.revoke' : 'btn.reinstate'),
    summary: revoke ? t('confirm.revokeResource') : t('confirm.reinstateResource', { type, id: logical }),
    reasonLabel: t(revoke ? 'prompt.revokeReason' : 'prompt.reinstateReason', { type, id: logical }),
    confirmLabel: t(revoke ? 'btn.revoke' : 'btn.reinstate') });
  if (!approval) return;
  run(() => api(path + (revoke ? '/revoke' : '/reinstate'),
    { method: 'POST', body: { reason: approval.reason } }),
  t(revoke ? 'toast.resourceRevoked' : 'toast.resourceReinstated'));
}

async function viewAIResourceDetail(m) {
  const ref = S.aiRef;
  const path = scopePath() + '/ai/resources/' + encodeURIComponent(ref.type) + '/' + encodeURIComponent(ref.logicalId);
  const detail = await api(path);
  m.innerHTML = renderAIResourceDetail(detail);
  $('#ai-back', m).onclick = () => { S.aiRef = null; route(); };
  m.addEventListener('click', event => {
    const action = event.target.closest('[data-pin], [data-revoke], [data-reinstate]');
    if (!action) return;
    const kind = action.hasAttribute('data-pin') ? 'pin' :
      action.hasAttribute('data-revoke') ? 'revoke' : 'reinstate';
    handleAIResourceAction(kind, ref.type, ref.logicalId).catch(reportError);
  });
}

async function viewAI(m) {
  if (!needScope(m)) return;
  if (S.aiRef) return viewAIResourceDetail(m);
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
  const revocationReasons = new Map(revocations.map(item => [item.type + '\u0000' + item.logicalId, item.reason]));
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
    '<input class="list-search" type="search" placeholder="' + esc(t('list.search')) +
      '" aria-label="' + esc(t('list.search')) + '">' +
    '<table class="grid mobile-card-table" id="ai-res-tbl"><thead><tr><th>' + esc(t('th.type')) + '</th><th>' + esc(t('ai.logicalId')) + '</th><th>' + esc(t('ai.physicalId')) + '</th><th>' + esc(t('th.owner')) + '</th><th>' + esc(t('th.created')) + '</th><th>' + esc(t('th.actions')) + '</th></tr></thead><tbody>' +
    (resources.map(r =>
      '<tr data-list-record><td>' + chip(r.type, 'teal') + '</td><td class="mono"><button type="button" class="row-link" data-ai-detail="' +
      esc(r.type + '|' + r.logicalId) + '">' + esc(r.logicalId) + '</button></td><td class="mono">' +
      esc(r.physicalId || '—') + '</td><td>' + esc(r.owner || '—') + '</td><td class="nowrap">' + esc(fmtTime(r.createdAt)) +
      '</td><td class="nowrap"><button class="btn sm" data-pin="' + esc(r.type + '|' + r.logicalId) + '">' + esc(t('btn.pinVersion')) + '</button> ' +
      (revocationReasons.has(r.type + '\u0000' + r.logicalId) ?
        '<button class="btn sm" data-reinstate="' + esc(r.type + '|' + r.logicalId) + '">' + esc(t('btn.reinstate')) + '</button>' :
        '<button class="btn sm danger" data-revoke="' + esc(r.type + '|' + r.logicalId) + '">' + esc(t('btn.revoke')) + '</button>') + '</td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">' + esc(t('ai.noneRegistered')) + '</td></tr>') +
    '</tbody></table><div class="mobile-record-cards" id="ai-resource-cards">' + resources.map(r =>
      '<article class="mobile-record" data-list-record><button type="button" class="row-link mono" data-ai-detail="' +
      esc(r.type + '|' + r.logicalId) + '">' + esc(r.logicalId) + '</button><div>' + chip(r.type, 'teal') + ' ' +
      (revocationReasons.has(r.type + '\u0000' + r.logicalId) ? chip(t('chip.revoked'), 'red') : chip(t('chip.active'), 'green')) +
      '</div><div class="mono wrap-anywhere muted">' + esc(r.physicalId || '—') + '</div>' +
      '<div class="muted">' + esc(r.owner || '—') + ' · ' + esc(fmtTime(r.createdAt)) + '</div>' +
      (revocationReasons.has(r.type + '\u0000' + r.logicalId) ?
        '<div class="warn-text">' + esc(revocationReasons.get(r.type + '\u0000' + r.logicalId)) + '</div>' : '') +
      '<div class="row"><button type="button" class="btn sm" data-pin="' + esc(r.type + '|' + r.logicalId) +
      '">' + esc(t('btn.pinVersion')) + '</button> ' +
      (revocationReasons.has(r.type + '\u0000' + r.logicalId) ?
        '<button type="button" class="btn sm" data-reinstate="' + esc(r.type + '|' + r.logicalId) +
        '">' + esc(t('btn.reinstate')) + '</button>' :
        '<button type="button" class="btn sm danger" data-revoke="' + esc(r.type + '|' + r.logicalId) +
        '">' + esc(t('btn.revoke')) + '</button>') + '</div></article>').join('') + '</div>' +
    (resources.length ? '' : '<p class="muted mobile-empty">' + esc(t('ai.noneRegistered')) + '</p>') +
    '<p class="muted hidden" data-list-no-match>' + esc(t('list.noMatch')) + '</p>' +
    (revocations.length
      ? '<div class="ai-revocations-desktop"><h4>' + esc(t('ai.revocations')) + '</h4><table class="grid" id="ai-rev-tbl"><thead><tr><th>' + esc(t('th.type')) + '</th><th>' + esc(t('ai.logicalId')) + '</th><th>' + esc(t('th.reason')) + '</th><th></th></tr></thead><tbody>' +
        revocations.map(rv =>
          '<tr><td>' + chip(rv.type, 'red') + '</td><td class="mono">' + esc(rv.logicalId) + '</td><td>' + esc(rv.reason || '—') +
          '</td><td><button class="btn sm" data-reinstate="' + esc(rv.type + '|' + rv.logicalId) + '">' + esc(t('btn.reinstate')) + '</button></td></tr>').join('') +
        '</tbody></table></div>'
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
  bindListSearch(m);
  $('#ai-reg', m).onclick = () => {
    const body = {
      type: $('#ai-type', m).value,
      logicalId: $('#ai-logical', m).value.trim(),
      name: $('#ai-name', m).value.trim(),
    };
    if (!body.logicalId || !body.name) { toast(t('toast.aiFieldsRequired'), false); return; }
    run(() => api(aiPath, { method: 'POST', body }), t('toast.resourceRegistered'));
  };
  m.addEventListener('click', ev => {
    const detail = ev.target.closest('[data-ai-detail]');
    if (detail) {
      const [type, logicalId] = split(detail.dataset.aiDetail);
      S.aiRef = { type, logicalId }; route(); return;
    }
    const pin = ev.target.closest('[data-pin]'), revoke = ev.target.closest('[data-revoke]');
    const reinstate = ev.target.closest('[data-reinstate]');
    if (pin || revoke || reinstate) {
      const [type, logicalId] = split(pin ? pin.dataset.pin : revoke ? revoke.dataset.revoke : reinstate.dataset.reinstate);
      handleAIResourceAction(pin ? 'pin' : revoke ? 'revoke' : 'reinstate', type, logicalId).catch(reportError);
    }
  });
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
  approvals: viewApprovals,
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
      if (S.view === 'audit') { S.auditObject = ''; S.auditQuery = ''; S.auditCursors = [0]; S.auditPage = 0; }
      S.deepLinkDenied = false;
      S.flagKey = S.segKey = S.expId = S.metricRef = S.approvalID = S.aiRef = null;
      S.flagNew = S.expCreate = S.wizardDirty = false;
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
    S.auditObject = ''; S.auditQuery = ''; S.auditCursors = [0]; S.auditPage = 0;
    S.flagKey = S.segKey = S.expId = S.metricRef = S.approvalID = S.aiRef = null;
    S.expCreate = S.wizardDirty = false;
    localStorage.setItem('golab_ws', S.ws);
    try { await loadEnvs(); await refreshCapabilities(); } catch (e) { reportError(e); }
    route();
    probeAI();
  };
  $('#sel-env').onchange = ev => {
    if (!confirmLeaveEditor()) { ev.target.value = S.env; return; }
    S.env = ev.target.value;
    S.auditObject = ''; S.auditQuery = ''; S.auditCursors = [0]; S.auditPage = 0;
    S.deepLinkDenied = false;
    S.deepLinkRequested = false;
    S.flagKey = S.segKey = S.expId = S.metricRef = S.approvalID = S.aiRef = null;
    S.expCreate = S.wizardDirty = false;
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
    if ((S.expCreate && S.wizardDirty) ||
        (editorValue() !== null && S.editorBaseline !== null && editorValue() !== S.editorBaseline)) {
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
