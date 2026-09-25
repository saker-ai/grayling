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
function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  return isNaN(d.getTime()) ? '—' : d.toLocaleString();
}
function fmtDurNs(ns) {
  if (ns == null) return '—';
  const h = Number(ns) / 3.6e12;
  return h >= 24 ? (h / 24) + 'd' : h + 'h';
}
function shortHash(h) { return h ? String(h).slice(0, 12) : '—'; }
function chip(label, cls) { return '<span class="chip ' + (cls || 'gray') + '">' + esc(label) + '</span>'; }

const DRAFT_CHIP = { editing: 'blue', submitted: 'amber', approved: 'green' };
const EXP_CHIP = { draft: 'gray', reviewed: 'blue', bound: 'teal', running: 'green', paused: 'amber', stopped: 'red', analyzed: 'blue', archived: 'gray' };
const VERDICT_CHIP = { ship: 'green', rollback: 'red', continue: 'blue', hold: 'amber' };

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

async function api(path, opts = {}) {
  const { method = 'GET', body } = opts;
  const headers = { 'Authorization': 'Bearer ' + (localStorage.getItem('golab_token') || '') };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(baseUrl() + path, {
      method, headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    throw new ApiError(0, 'network', 'cannot reach ' + (baseUrl() || 'same origin') + ': ' + e.message);
  }
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { /* non-JSON body */ } }
  if (!res.ok) {
    const err = (data && data.error) || {};
    const e = new ApiError(res.status, err.code || 'http_' + res.status, err.message || res.statusText);
    if (res.status === 401) showAuth('Token rejected (401). Please re-enter your admin token.');
    throw e;
  }
  return data;
}

function errMsg(e) {
  let m = '[' + (e.code || 'error') + '] ' + (e.message || e);
  if (e.status === 409) m += ' — conflict (optimistic lock): the object changed elsewhere; reload and re-apply.';
  if (e.status === 423) m += ' — resource revoked.';
  if (e.status === 501) m += ' — feature not enabled on this server.';
  return m;
}
function errBanner(e) {
  let h = '<div class="banner err"><b>' + esc(e.code || 'error') + '</b> — ' + esc(e.message || String(e));
  if (e.status === 409) h += '<div class="muted">Optimistic-lock conflict: the draft was changed elsewhere. Reload, re-apply your edit, and save again.</div>';
  if (e.status === 501) h += '<div class="muted">This feature is not enabled on the server (501).</div>';
  if (e.status === 403) h += ' <button class="btn sm" id="btn-reauth2">Re-enter token</button>';
  return h + '</div>';
}
// Runs a mutation: success → toast + re-route; failure → toast with code+message.
async function run(fn, okMsg) {
  try {
    const r = await fn();
    toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
    route();
    return r;
  } catch (e) {
    toast(e.code === 'self_approval_denied'
      ? 'Self-approval denied: the submitter cannot approve their own draft.'
      : errMsg(e), false);
    return undefined;
  }
}

// ---------------------------------------------------------------------------
// App state
// ---------------------------------------------------------------------------
const S = {
  view: 'flags',
  ws: localStorage.getItem('golab_ws') || '',
  env: localStorage.getItem('golab_env') || '',
  workspaces: [], envs: [],
  flagKey: null, flagNew: false,
  segKey: null, segDrafts: [],
  expId: null, expCreate: false,
  auditLimit: 200,
  aiAvailable: true,
  timer: null,
};
const wsPath = () => '/api/v1/admin/workspaces/' + encodeURIComponent(S.ws);
const scopePath = () => wsPath() + '/environments/' + encodeURIComponent(S.env);

function needWs(m) {
  if (!S.ws) { m.innerHTML = '<div class="banner warn">No workspace selected — create one with <b>+</b> next to the workspace selector.</div>'; return false; }
  return true;
}
function needScope(m) {
  if (!S.ws || !S.env) { m.innerHTML = '<div class="banner warn">Select a workspace and environment in the top bar (or create them with the <b>+</b> buttons).</div>'; return false; }
  return true;
}

// Small modal form builder. fields: [{name,label,type,value,placeholder,options,rows}]
function promptForm(title, fields, submitLabel, onSubmit) {
  const ov = document.createElement('div');
  ov.className = 'overlay';
  ov.innerHTML = '<div class="modal"><h3>' + esc(title) + '</h3><div class="modal-body"></div>' +
    '<div class="modal-foot"><button class="btn" data-x="cancel">Cancel</button>' +
    '<button class="btn primary" data-x="ok">' + esc(submitLabel || 'OK') + '</button></div></div>';
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
  document.body.appendChild(ov);
  const close = () => ov.remove();
  $('[data-x="cancel"]', ov).onclick = close;
  ov.addEventListener('click', ev => { if (ev.target === ov) close(); });
  $('[data-x="ok"]', ov).onclick = async () => {
    const vals = {};
    for (const f of fields) vals[f.name] = body.querySelector('[name="' + f.name + '"]').value;
    try { await onSubmit(vals); close(); } catch (e) { toast(errMsg(e), false); }
  };
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
const VIEWS = {}; // populated at the bottom of each view section

async function route() {
  if (S.timer) { clearInterval(S.timer); S.timer = null; }
  $$('#nav button').forEach(b => b.classList.toggle('active', b.dataset.view === S.view));
  const m = $('#main');
  m.innerHTML = '<p class="muted">Loading…</p>';
  try {
    await VIEWS[S.view](m);
  } catch (e) {
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
  $('#auth-token').value = localStorage.getItem('golab_token') || '';
  if (msg !== undefined) $('#auth-error').textContent = msg;
  $('#app').classList.add('hidden');
  $('#auth').classList.remove('hidden');
}

async function doConnect() {
  const tok = $('#auth-token').value.trim();
  if (!tok) { $('#auth-error').textContent = 'Enter your admin token.'; return; }
  localStorage.setItem('golab_url', $('#auth-url').value.trim());
  localStorage.setItem('golab_token', tok);
  $('#auth-error').textContent = 'Connecting…';
  try {
    const h = await api('/api/v1/health');
    if (!h || h.status !== 'ok') throw new ApiError(0, 'bad_health', 'health endpoint returned: ' + JSON.stringify(h));
    $('#auth-error').textContent = '';
    await boot();
  } catch (e) {
    $('#auth-error').textContent = e.status === 401 ? 'Token rejected by server (401).' : errMsg(e);
  }
}

async function loadWorkspaces() {
  S.workspaces = (await api('/api/v1/admin/workspaces')) || [];
  const sel = $('#sel-ws');
  sel.innerHTML = S.workspaces.map(w =>
    '<option value="' + esc(w.slug) + '">' + esc(w.name || w.slug) + ' (' + esc(w.slug) + ')</option>').join('') ||
    '<option value="">(none — create one)</option>';
  if (!S.workspaces.some(w => w.slug === S.ws)) S.ws = S.workspaces.length ? S.workspaces[0].slug : '';
  localStorage.setItem('golab_ws', S.ws);
  sel.value = S.ws;
  await loadEnvs();
}

async function loadEnvs() {
  S.envs = S.ws ? (await api(wsPath() + '/environments')) || [] : [];
  const sel = $('#sel-env');
  sel.innerHTML = S.envs.map(v =>
    '<option value="' + esc(v.slug) + '">' + esc(v.name || v.slug) + ' (' + esc(v.slug) + ')</option>').join('') ||
    '<option value="">(none — create one)</option>';
  if (!S.envs.some(v => v.slug === S.env)) S.env = S.envs.length ? S.envs[0].slug : '';
  localStorage.setItem('golab_env', S.env);
  sel.value = S.env;
}

async function boot() {
  await loadWorkspaces();
  $('#auth').classList.add('hidden');
  $('#app').classList.remove('hidden');
  const tok = localStorage.getItem('golab_token') || '';
  $('#conn-info').textContent = (baseUrl() || location.origin) + ' · ' + tok.slice(0, 10) + '…';
  await route();
  probeAI();
}

async function probeAI() {
  if (!S.ws || !S.env) return;
  try {
    await api(scopePath() + '/ai/resources');
    S.aiAvailable = true;
  } catch (e) {
    if (e.status === 501) {
      S.aiAvailable = false;
      if (S.view === 'ai') { S.view = 'flags'; route(); }
    }
  }
  $('#nav-ai').classList.toggle('hidden', !S.aiAvailable);
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
  if (typeof c !== 'object' || c === null) return ['content must be a JSON object'];
  if (!['bool', 'string', 'number', 'json'].includes(c.type)) issues.push('type must be bool|string|number|json');
  const vars = c.variations || {};
  const checkRule = (r, where) => {
    if (!r) { issues.push(where + ': rule missing'); return; }
    const hasVar = r.variation !== undefined, hasPct = r.percentage !== undefined;
    if (hasVar === hasPct) issues.push(where + ': exactly one of variation|percentage required');
    if (hasVar && !(r.variation in vars)) issues.push(where + ': unknown variation "' + r.variation + '"');
    if (hasPct) {
      const sum = Object.values(r.percentage).reduce((a, b) => a + Number(b), 0);
      if (Math.abs(sum - 100) > 0.01) issues.push(where + ': percentages sum to ' + sum + ' (want 100)');
      for (const vn of Object.keys(r.percentage)) if (!(vn in vars)) issues.push(where + ': unknown variation "' + vn + '"');
    }
    for (const con of r.constraints || []) {
      if (!con.field) issues.push(where + ': constraint missing field');
      if (!OPS.includes(con.op)) issues.push(where + ': unknown op "' + con.op + '"');
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
    '<div class="view-head"><h2>Flags</h2><span class="muted">' + flags.length + ' flag(s)</span>' +
    '<button class="btn primary sm" id="flag-new">+ New flag</button></div>' +
    '<table class="grid"><thead><tr><th>Key</th><th>Type</th><th>Draft state</th><th>Revision</th><th>Updated</th></tr></thead><tbody>' +
    (flags.map(f => {
      const d = f.draft, c = f.current;
      const type = (d && d.content && d.content.type) || (c && c.content && c.content.type) || '—';
      const st = d
        ? chip(d.state, DRAFT_CHIP[d.state] || 'gray') + (d.deleted ? ' ' + chip('del', 'red') : '')
        : (c ? chip('published', 'green') : chip('none', 'gray'));
      const upd = (d && d.updatedAt) || (c && c.publishedAt) || null;
      return '<tr class="clickable" data-flag="' + esc(f.key) + '"><td class="mono">' + esc(f.key) +
        '</td><td>' + esc(type) + '</td><td>' + st + '</td><td>' + esc(f.currentRevision || 0) +
        '</td><td class="nowrap">' + esc(fmtTime(upd)) + '</td></tr>';
    }).join('') || '<tr><td colspan="5" class="muted">No flags yet — create one.</td></tr>') +
    '</tbody></table>';
  $('#flag-new', m).onclick = () => {
    const key = (prompt('New flag key (e.g. my-flag):') || '').trim();
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
  if (!revs.length) return '<p class="muted">No published revisions yet.</p>';
  return '<table class="grid"><thead><tr><th>Rev</th><th>Hash</th><th>Published</th><th>By</th><th>Note</th><th></th></tr></thead><tbody>' +
    revs.map(r => '<tr><td>r' + esc(r.revision) + (r.rollbackOf ? ' <span class="muted">(rollback of r' + esc(r.rollbackOf) + ')</span>' : '') +
      '</td><td class="mono">' + esc(shortHash(r.hash)) + '</td><td class="nowrap">' + esc(fmtTime(r.publishedAt)) +
      '</td><td>' + esc(r.publishedBy || '—') + '</td><td>' + esc(r.note || '') +
      '</td><td><button class="btn sm" data-rb="' + esc(r.revision) + '">Rollback to r' + esc(r.revision) + '</button></td></tr>').join('') +
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
    ? chip(draft.state, DRAFT_CHIP[draft.state] || 'gray') + (draft.deleted ? ' ' + chip('deletion pending', 'red') : '')
    : (f && f.current ? chip('published', 'green') : chip('new', 'gray'));
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="flag-back">← Flags</button>' +
    '<h2 class="mono">' + esc(key) + '</h2>' + st +
    (draft ? '<span class="muted">risk ' + esc(draft.risk || '—') + ' · ' + esc(draft.updatedBy || '—') + ' · ' + esc(fmtTime(draft.updatedAt)) + '</span>' : '') +
    '</div>' +
    '<div class="cols">' +
      '<section class="card"><h3>Draft editor</h3>' +
        '<textarea id="flag-json" class="json" rows="24" spellcheck="false"></textarea>' +
        '<div class="row"><button class="btn sm" id="flag-validate">Validate JSON</button><span id="flag-valid" class="muted grow"></span>' +
        '<label class="inline">risk <select id="flag-risk"><option value="normal">normal</option><option value="high">high</option></select></label>' +
        '<label class="inline">baseRevision <input id="flag-base" type="number" min="0" size="4" value="' + esc((f && f.currentRevision) || 0) + '"></label></div>' +
        '<div class="row actions">' +
        '<button class="btn primary" id="flag-save">Save draft</button>' +
        '<button class="btn" id="flag-submit">Submit</button>' +
        '<button class="btn" id="flag-approve">Approve</button>' +
        '<button class="btn" id="flag-reject">Reject</button>' +
        '<button class="btn" id="flag-publish">Publish</button>' +
        '<button class="btn danger" id="flag-delete">Mark deleted</button></div></section>' +
      '<section class="card"><h3>Current published (r' + esc((f && f.currentRevision) || 0) + ')</h3>' +
        (f && f.current
          ? '<div class="muted">hash ' + esc(shortHash(f.current.hash)) + ' · ' + esc(fmtTime(f.current.publishedAt)) + ' by ' + esc(f.current.publishedBy || '—') + '</div>'
          : '<div class="muted">Nothing published yet.</div>') +
        '<pre id="flag-current" class="pre"></pre></section>' +
    '</div>' +
    '<section class="card"><h3>History &amp; rollback</h3><div id="flag-revs">' + revisionsTable(f) + '</div></section>' +
    '<section class="card"><h3>Import from gff (YAML or JSON)</h3>' +
      '<textarea id="gff-doc" class="json" rows="8" spellcheck="false" placeholder="paste gff flag YAML or JSON…"></textarea>' +
      '<div class="row"><label class="inline">key <input id="gff-key" value="' + esc(key) + '"></label>' +
      '<label class="inline">salt <input id="gff-salt" placeholder="bucketing salt"></label>' +
      '<button class="btn" id="gff-run">Import (creates HIGH-risk draft)</button></div>' +
      '<div id="gff-warn"></div></section>';
  $('#flag-json', m).value = JSON.stringify(content, null, 2);
  $('#flag-current', m).textContent = f && f.current ? JSON.stringify(f.current.content, null, 2) : '';
  if (draft && draft.risk) $('#flag-risk', m).value = draft.risk;
  bindFlagDetail(m, key, kp);
}

function bindFlagDetail(m, key, kp) {
  $('#flag-back', m).onclick = () => { S.flagKey = null; S.flagNew = false; route(); };
  // Returns parsed content, or null (with toast) when invalid.
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#flag-json', m).value); }
    catch (e) { toast('Invalid JSON: ' + e.message, false); return null; }
    if (!c || c.key !== key) { toast('content.key must equal "' + key + '"', false); return null; }
    return c;
  };
  $('#flag-validate', m).onclick = () => {
    const out = $('#flag-valid', m);
    let c;
    try { c = JSON.parse($('#flag-json', m).value); }
    catch (e) { out.className = 'err-text grow'; out.textContent = '✗ invalid JSON: ' + e.message; return; }
    const issues = lintFlag(c);
    if (!c || c.key !== key) issues.unshift('content.key must equal "' + key + '"');
    if (issues.length) { out.className = 'warn-text grow'; out.textContent = '⚠ ' + issues.join(' · '); }
    else { out.className = 'ok-text grow'; out.textContent = '✓ valid — no issues found'; }
  };
  $('#flag-save', m).onclick = () => {
    const content = readJSON();
    if (!content) return;
    run(() => api(kp + '/draft', {
      method: 'PUT',
      body: { content, risk: $('#flag-risk', m).value, baseRevision: Number($('#flag-base', m).value || 0) },
    }), 'Draft saved');
  };
  $('#flag-submit', m).onclick = () => run(() => api(kp + '/submit', { method: 'POST', body: {} }), 'Draft submitted for review');
  $('#flag-approve', m).onclick = () => {
    const note = prompt('Approve draft — review note (optional):', '') || '';
    run(() => api(kp + '/approve', { method: 'POST', body: { note } }), 'Draft approved');
  };
  $('#flag-reject', m).onclick = () => {
    const note = prompt('Reject draft — reason:', '') || '';
    run(() => api(kp + '/reject', { method: 'POST', body: { note } }), 'Draft rejected');
  };
  $('#flag-publish', m).onclick = () => {
    const note = prompt('Publish — note (optional):', '') || '';
    run(() => api(kp + '/publish', { method: 'POST', body: { note } }),
      r => 'Published flag r' + (r && r.flagRevision) + ' · manifest r' + (r && r.manifestRevision));
  };
  $('#flag-delete', m).onclick = () => {
    if (!confirm('Mark flag "' + key + '" as deleted? A tombstone is published after submit → approve → publish.')) return;
    run(() => api(kp, { method: 'DELETE' }), 'Deletion marked — now submit, approve and publish to tombstone the flag');
  };
  $('#flag-revs', m).addEventListener('click', ev => {
    const b = ev.target.closest('[data-rb]');
    if (!b) return;
    const rev = Number(b.dataset.rb);
    if (!confirm('Rollback to revision r' + rev + '? The manifest will repoint at the old content.')) return;
    const reason = prompt('Rollback reason (required):', '');
    if (!reason) { toast('Reason is required', false); return; }
    run(() => api(kp + '/rollback', { method: 'POST', body: { revision: rev, reason } }), 'Rolled back to r' + rev);
  });
  $('#gff-run', m).onclick = async () => {
    const doc = $('#gff-doc', m).value;
    if (!doc.trim()) { toast('Paste a gff document first', false); return; }
    try {
      const res = await api(kp + '/import-gff', {
        method: 'POST',
        body: { document: doc, key: $('#gff-key', m).value.trim() || key, salt: $('#gff-salt', m).value.trim() },
      });
      const warns = (res && res.warnings) || [];
      $('#gff-warn', m).innerHTML = '<div class="banner warn"><b>Import warnings (' + warns.length + ')</b><ul>' +
        (warns.map(w => '<li>' + esc(typeof w === 'string' ? w : JSON.stringify(w)) + '</li>').join('') || '<li>none</li>') +
        '</ul>' + (res && res.note ? '<div class="muted">' + esc(res.note) + '</div>' : '') + '</div>';
      toast('Imported — HIGH-risk draft created');
      const nf = await api(kp); // refresh editor with the imported draft, keep warnings visible
      if (nf && nf.draft) $('#flag-json', m).value = JSON.stringify(nf.draft.content, null, 2);
      $('#flag-revs', m).innerHTML = revisionsTable(nf);
    } catch (e) { toast(errMsg(e), false); }
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
    '<div class="view-head"><h2>Segments</h2><span class="muted">' + S.segDrafts.length + ' draft(s)</span>' +
    '<button class="btn primary sm" id="seg-new">+ New segment</button></div>' +
    '<table class="grid"><thead><tr><th>Key</th><th>State</th><th>Risk</th><th>Updated by</th><th>Updated</th></tr></thead><tbody>' +
    (S.segDrafts.map(d =>
      '<tr class="clickable" data-seg="' + esc(d.key) + '"><td class="mono">' + esc(d.key) + '</td><td>' +
      chip(d.state, DRAFT_CHIP[d.state] || 'gray') + '</td><td>' + esc(d.risk || '—') + '</td><td>' +
      esc(d.updatedBy || '—') + '</td><td class="nowrap">' + esc(fmtTime(d.updatedAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">No segment drafts.</td></tr>') +
    '</tbody></table>';
  $('#seg-new', m).onclick = () => {
    const key = (prompt('New segment key:') || '').trim();
    if (!key) return;
    S.segKey = key; route();
  };
  $('tbody', m).addEventListener('click', ev => {
    const tr = ev.target.closest('[data-seg]');
    if (tr) { S.segKey = tr.dataset.seg; route(); }
  });
}

function viewSegmentDetail(m) {
  const key = S.segKey;
  const kp = scopePath() + '/segments/' + encodeURIComponent(key);
  const d = S.segDrafts.find(x => x.key === key);
  const content = (d && d.content) || segmentTemplate(key);
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="seg-back">← Segments</button>' +
    '<h2 class="mono">' + esc(key) + '</h2>' +
    (d ? chip(d.state, DRAFT_CHIP[d.state] || 'gray') +
      ' <span class="muted">risk ' + esc(d.risk || '—') + ' · ' + esc(d.updatedBy || '—') + ' · ' + esc(fmtTime(d.updatedAt)) + '</span>'
      : chip('new', 'gray')) + '</div>' +
    '<section class="card"><h3>Segment draft</h3>' +
    '<textarea id="seg-json" class="json" rows="18" spellcheck="false"></textarea>' +
    '<div class="row"><button class="btn sm" id="seg-validate">Validate JSON</button><span id="seg-valid" class="muted grow"></span>' +
    '<label class="inline">risk <select id="seg-risk"><option value="normal">normal</option><option value="high">high</option></select></label></div>' +
    '<div class="row actions">' +
    '<button class="btn primary" id="seg-save">Save draft</button>' +
    '<button class="btn" id="seg-submit">Submit</button>' +
    '<button class="btn" id="seg-approve">Approve</button>' +
    '<button class="btn" id="seg-publish">Publish</button></div></section>';
  $('#seg-json', m).value = JSON.stringify(content, null, 2);
  if (d && d.risk) $('#seg-risk', m).value = d.risk;
  $('#seg-back', m).onclick = () => { S.segKey = null; route(); };
  $('#seg-validate', m).onclick = () => {
    const out = $('#seg-valid', m);
    try {
      const c = JSON.parse($('#seg-json', m).value);
      out.className = (c && c.key === key) ? 'ok-text grow' : 'warn-text grow';
      out.textContent = (c && c.key === key) ? '✓ valid' : '⚠ content.key should equal "' + key + '"';
    } catch (e) { out.className = 'err-text grow'; out.textContent = '✗ invalid JSON: ' + e.message; }
  };
  const readJSON = () => {
    let c;
    try { c = JSON.parse($('#seg-json', m).value); }
    catch (e) { toast('Invalid JSON: ' + e.message, false); return null; }
    if (!c || c.key !== key) { toast('content.key must equal "' + key + '"', false); return null; }
    return c;
  };
  $('#seg-save', m).onclick = () => {
    const content2 = readJSON();
    if (!content2) return;
    run(() => api(kp + '/draft', { method: 'PUT', body: { content: content2, risk: $('#seg-risk', m).value } }), 'Segment draft saved');
  };
  $('#seg-submit', m).onclick = () => run(() => api(kp + '/submit', { method: 'POST', body: {} }), 'Segment submitted');
  $('#seg-approve', m).onclick = () => run(() => api(kp + '/approve', { method: 'POST', body: {} }), 'Segment approved');
  $('#seg-publish', m).onclick = () => run(() => api(kp + '/publish', { method: 'POST', body: {} }), 'Segment published');
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
    '<div class="view-head"><h2>Experiments</h2><span class="muted">' + exps.length + '</span>' +
    '<button class="btn primary sm" id="exp-new">+ New experiment</button></div>' +
    '<table class="grid"><thead><tr><th>ID</th><th>Flag</th><th>Unit</th><th>State</th><th>Variants</th><th>Epoch</th><th>Created</th></tr></thead><tbody>' +
    (exps.map(e =>
      '<tr class="clickable" data-exp="' + esc(e.id) + '"><td class="mono">' + esc(e.id) + '</td><td class="mono">' +
      esc(e.flagKey) + '</td><td class="mono">' + esc(e.unit) + '</td><td>' +
      chip(e.state, EXP_CHIP[e.state] || 'gray') + '</td><td>' + esc((e.variants || []).join(', ')) +
      '</td><td>' + esc(e.epoch || 0) + '</td><td class="nowrap">' + esc(fmtTime(e.createdAt)) + '</td></tr>').join('') ||
      '<tr><td colspan="7" class="muted">No experiments.</td></tr>') +
    '</tbody></table>';
  $('#exp-new', m).onclick = () => { S.expCreate = true; route(); };
  $('tbody', m).addEventListener('click', ev => {
    const tr = ev.target.closest('[data-exp]');
    if (tr) { S.expId = tr.dataset.exp; route(); }
  });
}

function viewExpCreate(m) {
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="exp-back">← Experiments</button><h2>New experiment</h2></div>' +
    '<section class="card">' +
    '<div class="form-grid">' +
      '<label class="field">Flag key<input id="x-flag" placeholder="my-flag"></label>' +
      '<label class="field">Randomization unit<input id="x-unit" value="user_id"></label>' +
      '<label class="field">Layer (optional)<input id="x-layer"></label>' +
      '<label class="field">Eligibility % (blank = 100)<input id="x-elig" type="number" step="1" min="0" max="100"></label>' +
    '</div>' +
    '<h4>Variants &amp; proportions (percent — must sum to 100)</h4>' +
    '<div id="x-vars"></div><button class="btn sm" id="x-add-var">+ Add variant</button>' +
    '<h4>Primary metric</h4>' +
    '<div class="form-grid">' +
      '<label class="field">Metric key<input id="x-mkey" placeholder="checkout_success"></label>' +
      '<label class="field">Type<select id="x-mtype"><option>proportion</option><option>mean</option><option>ratio</option><option>quantile</option></select></label>' +
      '<label class="field">Better direction<select id="x-mdir"><option>increase</option><option>decrease</option></select></label>' +
    '</div>' +
    '<div class="form-grid" id="x-metric-adv">' +
      '<label class="field" id="x-numkey-wrap">Numerator key <span class="muted">(ratio; blank = metric key)</span><input id="x-numkey" placeholder="revenue"></label>' +
      '<label class="field" id="x-denkey-wrap">Denominator key <span class="muted">(ratio)</span><input id="x-denkey" placeholder="sessions"></label>' +
      '<label class="field" id="x-qlevel-wrap">Quantile level <span class="muted">(quantile, 0–1)</span><input id="x-qlevel" type="number" step="0.01" min="0.01" max="0.99" placeholder="0.95"></label>' +
      '<label class="field" id="x-covkey-wrap">Covariate key <span class="muted">(CUPED, mean)</span><input id="x-covkey" placeholder="pre_period_value"></label>' +
    '</div>' +
    '<h4>Guardrails</h4>' +
    '<div id="x-guards"></div><button class="btn sm" id="x-add-guard">+ Add guardrail</button>' +
    '<h4>Statistics &amp; runtime</h4>' +
    '<div class="form-grid">' +
      '<label class="field">Alpha<input id="x-alpha" type="number" step="0.001" value="0.05"></label>' +
      '<label class="field">Power<input id="x-power" type="number" step="0.01" value="0.8"></label>' +
      '<label class="field">MDE<input id="x-mde" type="number" step="0.001" value="0.01"></label>' +
      '<label class="field">Min sample / arm<input id="x-minsample" type="number" value="1000"></label>' +
      '<label class="field">Min runtime (hours)<input id="x-minruntime" type="number" step="0.5" value="168"></label>' +
      '<label class="field">Analysis window (days)<input id="x-window" type="number" step="1" value="7"></label>' +
    '</div>' +
    '<label class="field">Hypothesis<textarea id="x-hyp" rows="2"></textarea></label>' +
    '<div class="row"><button class="btn primary" id="x-create">Create experiment</button></div></section>';
  const vars = $('#x-vars', m), guards = $('#x-guards', m);
  const addVar = (name, p) => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="v-name" placeholder="variant name">' +
      '<input class="v-prop" type="number" step="1" min="0" max="100" placeholder="percent e.g. 50">' +
      '<button class="btn sm" title="remove">✕</button>';
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
  };
  mtype.onchange = syncMetricAdv;
  syncMetricAdv();
  $('#x-add-guard', m).onclick = () => {
    const row = document.createElement('div');
    row.className = 'vrow';
    row.innerHTML = '<input class="g-key" placeholder="metricKey">' +
      '<select class="g-type"><option>proportion</option><option>mean</option></select>' +
      '<select class="g-dir"><option value="increase">increase = good</option><option value="decrease">decrease = good</option></select>' +
      '<input class="g-reg" type="number" step="0.01" placeholder="max rel. regression e.g. 0.05">' +
      '<input class="g-min" type="number" placeholder="minSample">' +
      '<button class="btn sm">✕</button>';
    $('button', row).onclick = () => row.remove();
    guards.appendChild(row);
  };
  $('#exp-back', m).onclick = () => { S.expCreate = false; route(); };
  $('#x-create', m).onclick = async () => {
    const variants = $$('.vrow', vars)
      .map(r => ({ name: $('.v-name', r).value.trim(), prop: Number($('.v-prop', r).value) }))
      .filter(v => v.name);
    if (variants.length < 2) { toast('Need at least 2 variants', false); return; }
    const propSum = variants.reduce((a, v) => a + (v.prop || 0), 0);
    if (Math.abs(propSum - 100) > 0.01) { toast('Proportions must sum to 100 (got ' + propSum + ')', false); return; }
    const proportions = {};
    variants.forEach(v => { proportions[v.name] = v.prop; });
    const guardrails = $$('.vrow', guards).map(r => ({
      metricKey: $('.g-key', r).value.trim(),
      type: $('.g-type', r).value,
      direction: $('.g-dir', r).value,
      maxRelativeRegression: Number($('.g-reg', r).value || 0),
      minSample: Number($('.g-min', r).value || 0),
    })).filter(g => g.metricKey);
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
    };
    if (!body.flagKey || !body.primaryMetric.key) { toast('flagKey and primary metric key are required', false); return; }
    if (pm.type === 'ratio' && !pm.denominatorKey) { toast('ratio metric needs a denominator key', false); return; }
    if (pm.type === 'quantile' && !(pm.quantileLevel > 0 && pm.quantileLevel < 1)) { toast('quantile metric needs a level in (0,1)', false); return; }
    const layer = $('#x-layer', m).value.trim();
    if (layer) body.layer = layer;
    const elig = $('#x-elig', m).value;
    if (elig !== '') body.eligibility = Number(elig);
    try {
      const exp = await api(scopePath() + '/experiments', { method: 'POST', body });
      toast('Experiment created: ' + exp.id);
      S.expCreate = false; S.expId = exp.id; route();
    } catch (e) { toast(errMsg(e), false); }
  };
  return Promise.resolve();
}

async function viewExpDetail(m) {
  const id = S.expId;
  const p = '/api/v1/admin/experiments/' + encodeURIComponent(id);
  const e = await api(p);
  m.innerHTML =
    '<div class="view-head"><button class="btn sm" id="exp-back">← Experiments</button>' +
    '<h2 class="mono">' + esc(e.id) + '</h2>' + chip(e.state, EXP_CHIP[e.state] || 'gray') +
    '<span class="muted">flag ' + esc(e.flagKey) + ' @ r' + esc(e.flagRevision || 0) + ' · epoch ' + esc(e.epoch || 0) +
    ' · created by ' + esc(e.createdBy || '—') + '</span></div>' +
    '<div class="cols"><section class="card"><h3>Pre-registration</h3><table class="kv">' +
      '<tr><th>Unit</th><td class="mono">' + esc(e.unit) + '</td></tr>' +
      '<tr><th>Layer</th><td>' + esc(e.layer || '—') + (e.layerSlot ? ' <span class="muted">slot ' + esc(JSON.stringify(e.layerSlot)) + '</span>' : '') + '</td></tr>' +
      '<tr><th>Eligibility</th><td>' + (e.eligibility ? esc(e.eligibility) + '%' : '100%') + '</td></tr>' +
      '<tr><th>Variants</th><td>' + (e.variants || []).map(v => '<span class="mono">' + esc(v) + '</span> (' + esc((e.proportions || {})[v] ?? '?') + ')').join(', ') + '</td></tr>' +
      '<tr><th>Primary</th><td class="mono">' + esc(e.primaryMetric.key) + '</td></tr>' +
      '<tr><th></th><td>' + esc(e.primaryMetric.type) + ' · better = ' + esc(e.primaryMetric.direction) + '</td></tr>' +
      '<tr><th>α / power / MDE</th><td>' + esc(e.alpha) + ' / ' + esc(e.power) + ' / ' + esc(e.mde) + '</td></tr>' +
      '<tr><th>Min sample/arm</th><td>' + esc(e.minSamplePerArm) + '</td></tr>' +
      '<tr><th>Min runtime</th><td>' + esc(fmtDurNs(e.minRuntime)) + '</td></tr>' +
      '<tr><th>Analysis window</th><td>' + esc(fmtDurNs(e.analysisWindow)) + '</td></tr>' +
      '<tr><th>Method</th><td>' + esc(e.analysisMethod || '—') + '</td></tr>' +
    '</table><div class="muted">Hypothesis: ' + esc(e.hypothesis || '—') + '</div>' +
    ((e.guardrails || []).length
      ? '<h4>Guardrails</h4><table class="grid"><thead><tr><th>Metric</th><th>Type</th><th>Good dir</th><th>Max rel. reg.</th><th>Min sample</th></tr></thead><tbody>' +
        e.guardrails.map(g => '<tr><td class="mono">' + esc(g.metricKey) + '</td><td>' + esc(g.type) + '</td><td>' +
          esc(g.direction || 'increase') + '</td><td>' + esc(g.maxRelativeRegression) + '</td><td>' + esc(g.minSample) + '</td></tr>').join('') +
        '</tbody></table>'
      : '') +
    '</section>' +
    '<section class="card"><h3>Lifecycle</h3>' +
      '<div class="row actions" style="border:0;padding:0;margin:0">' +
      '<button class="btn" id="exp-review">Review</button>' +
      '<button class="btn" id="exp-bind">Bind</button></div>' +
      '<p class="muted">After Bind the flag draft is prepared automatically — approve &amp; publish it in the Flags view, then transition to Running.</p>' +
      '<div class="row"><select id="exp-to">' +
      '<option value="running">running</option><option value="paused">paused</option>' +
      '<option value="stopped">stopped</option><option value="analyzed">analyzed</option>' +
      '<option value="archived">archived</option></select>' +
      '<input id="exp-reason" class="grow" placeholder="reason (required)">' +
      '<button class="btn primary" id="exp-transition">Transition</button></div>' +
      '<h3>Report</h3>' +
      '<div class="row"><button class="btn" id="exp-report">Load / refresh report</button>' +
      '<label class="inline"><input type="checkbox" id="exp-auto"> auto-refresh 10s</label></div>' +
      '<div id="report-box"><p class="muted">Not loaded.</p></div>' +
    '</section></div>' +
    '<section class="card"><h3>Staged rollout <span class="muted">(release plan · auto-advance)</span></h3>' +
    '<div id="rollout-box">' + renderRollout(e) + '</div></section>';
  $('#exp-back', m).onclick = () => { S.expId = null; route(); };
  const act = (path, body, msg) => run(() => api(p + path, { method: 'POST', body }), msg);
  $('#exp-review', m).onclick = () => {
    const note = prompt('Review note:', '') || '';
    act('/review', { note }, 'Review recorded');
  };
  $('#exp-bind', m).onclick = () => act('/bind', {}, 'Experiment bound to flag — approve & publish the flag draft next');
  $('#exp-transition', m).onclick = () => {
    const to = $('#exp-to', m).value;
    const reason = $('#exp-reason', m).value.trim();
    if (!reason) { toast('Reason is required for transitions', false); return; }
    if (!confirm('Transition experiment to "' + to + '"?')) return;
    act('/transition', { to, reason }, 'Transitioned to ' + to);
  };
  const loadReport = async () => {
    const box = $('#report-box', m);
    if (!box) { if (S.timer) { clearInterval(S.timer); S.timer = null; } return; }
    try { renderReport(box, await api(p + '/report')); }
    catch (err) { box.innerHTML = errBanner(err); }
  };
  $('#exp-report', m).onclick = loadReport;
  $('#exp-auto', m).onchange = ev => {
    if (S.timer) { clearInterval(S.timer); S.timer = null; }
    if (ev.target.checked) { loadReport(); S.timer = setInterval(loadReport, 10000); }
  };
  bindRollout(m, id);
}

function renderReport(box, r) {
  const p = r.primary, d = r.decision || {}, srm = r.srm;
  box.innerHTML =
    (r.healthy
      ? '<div class="banner ok">Report healthy · epoch ' + esc(r.epoch) + ' · state ' + esc(r.state) + '</div>'
      : '<div class="banner err"><b>Unhealthy report</b><ul>' +
        (r.healthIssues || []).map(i => '<li>' + esc(i) + '</li>').join('') + '</ul></div>') +
    '<div class="row">' + chip('min runtime ' + (r.minRuntimeSatisfied ? 'satisfied' : 'NOT satisfied'), r.minRuntimeSatisfied ? 'green' : 'amber') +
    ' ' + chip(r.analysisMethod || '—', 'gray') + '</div>' +
    '<h4>SRM check</h4>' +
    (srm
      ? '<div>χ² = ' + esc(srm.chiSquare ?? '—') + ' · p = ' + esc(srm.pValue ?? '—') + ' ' +
        chip(srm.healthy ? 'no SRM' : 'SRM detected', srm.healthy ? 'green' : 'red') + '</div>' +
        '<pre class="pre small">' + esc(JSON.stringify({ observed: srm.observed, expected: srm.expected }, null, 2)) + '</pre>'
      : '<div class="muted">No SRM data yet (no randomized exposures).</div>') +
    '<h4>Per variant</h4>' +
    '<table class="grid"><thead><tr><th>Variant</th><th>Exposed units</th><th>Randomized</th><th>Exposures</th><th>Converted</th><th>Primary value</th></tr></thead><tbody>' +
    ((r.perVariant || []).map(v =>
      '<tr><td class="mono">' + esc(v.variant) + '</td><td>' + esc(v.exposedUnits) + '</td><td>' + esc(v.randomizedUnits) +
      '</td><td>' + esc(v.exposures) + '</td><td>' + esc(v.convertedUnits) + '</td><td>' + esc(v.primaryValue) + '</td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">no data yet</td></tr>') +
    '</tbody></table>' +
    '<h4>Primary metric' + (p ? ' <span class="muted">(' + esc(p.type || '') + ')</span>' : '') + '</h4>' +
    (p
      ? '<div>p = ' + esc(p.pValue ?? '—') + ' · diff = ' + esc(p.diff ?? '—') + ' · rel = ' + esc(p.relativeDiff ?? '—') +
        (p.relativeMeaningful === false ? ' ' + chip('not meaningful', 'amber') : '') +
        ' · CI [' + esc(p.ciLow ?? '—') + ', ' + esc(p.ciHigh ?? '—') + '] ' +
        chip(p.significant ? 'significant' : 'not significant', p.significant ? 'green' : 'gray') + '</div>'
      : '<div class="muted">' + esc(r.primaryError || 'No primary-metric result yet.') + '</div>') +
    (p && p.type === 'cuped' && p.cuped
      ? '<div class="muted">CUPED: variance reduction ' + esc((100 * (p.varianceReduction || 0)).toFixed(1)) +
        '% · θ=' + esc((p.cuped.theta ?? 0).toFixed(4)) + ' · ρ=' + esc((p.cuped.correlation ?? 0).toFixed(3)) +
        ' · raw diff ' + esc((p.cuped.rawDiff ?? 0).toFixed(4)) + '</div>'
      : '') +
    (p && p.type === 'ratio-delta' && p.ratio
      ? '<div class="muted">ratio: A=' + esc((p.ratio.ratioA ?? 0).toFixed(4)) +
        ' · B=' + esc((p.ratio.ratioB ?? 0).toFixed(4)) + ' · SE=' + esc((p.ratio.se ?? 0).toFixed(4)) + '</div>'
      : '') +
    (p && p.type === 'quantile' && p.quantile
      ? '<div class="muted">quantile p' + esc(p.quantile.level) + ': A=' + esc((p.quantile.quantileA ?? 0).toFixed(3)) +
        ' · B=' + esc((p.quantile.quantileB ?? 0).toFixed(3)) + ' · A CI [' + esc((p.quantile.ciALow ?? 0).toFixed(3)) +
        ', ' + esc((p.quantile.ciAHigh ?? 0).toFixed(3)) + '] (distribution-free)</div>'
      : '') +
    '<h4>Sequential <span class="muted">(always-valid · separate view, not the decision gate)</span></h4>' +
    (r.sequential
      ? '<div>anytime-valid p = ' + esc((r.sequential.pValue ?? 0).toExponential(3)) + ' ' +
        chip(r.sequential.significant ? 'significant at any look' : 'not significant', r.sequential.significant ? 'green' : 'gray') +
        ' · CS [' + esc((r.sequential.csLow ?? 0).toFixed(4)) + ', ' + esc((r.sequential.csHigh ?? 0).toFixed(4)) +
        '] · τ=' + esc(r.sequential.tau) + ' · logΛ=' + esc((r.sequential.logLambda ?? 0).toFixed(3)) + '</div>'
      : '<div class="muted">not computed (mean metrics only).</div>') +
    '<h4>Bayesian <span class="muted">(posterior · separate from frequentist)</span></h4>' +
    (r.bayesian && r.bayesian.mean
      ? '<div>P(B&gt;A) = ' + esc((r.bayesian.mean.probBSuperior ?? 0).toFixed(4)) + ' ' +
        chip('prob superior', 'teal') + ' · expected loss (choose B) = ' + esc((r.bayesian.mean.expectedLossB ?? 0).toExponential(3)) +
        ' · 95% CrI [' + esc((r.bayesian.mean.credibleLow ?? 0).toFixed(4)) + ', ' + esc((r.bayesian.mean.credibleHigh ?? 0).toFixed(4)) + ']</div>'
      : r.bayesian && r.bayesian.proportion
      ? '<div>P(B&gt;A) = ' + esc((r.bayesian.proportion.probBSuperior ?? 0).toFixed(4)) + ' ' +
        chip('prob superior', 'teal') + ' · expected loss (choose B) = ' + esc((r.bayesian.proportion.expectedLossB ?? 0).toExponential(3)) +
        ' · posterior A=Beta(' + esc((r.bayesian.proportion.postAA ?? 0).toFixed(1)) + ',' + esc((r.bayesian.proportion.postAB ?? 0).toFixed(1)) +
        ') B=Beta(' + esc((r.bayesian.proportion.postBA ?? 0).toFixed(1)) + ',' + esc((r.bayesian.proportion.postBB ?? 0).toFixed(1)) + ')</div>'
      : '<div class="muted">not computed.</div>') +
    '<h4>Guardrails</h4>' +
    ((r.guardrails || []).length
      ? '<ul class="plain">' + r.guardrails.map(g =>
        '<li>' + chip(g.breached ? 'breached' : 'ok', g.breached ? 'red' : 'green') +
        ' <span class="mono">' + esc(g.Metric ?? g.metric ?? '—') + '</span> <span class="muted">' + esc(g.Detail ?? g.detail ?? '') + '</span></li>').join('') + '</ul>'
      : '<div class="muted">none</div>') +
    '<h4>Decision</h4>' +
    '<div>' + chip(String(d.verdict || '—').toUpperCase(), VERDICT_CHIP[d.verdict] || 'gray') + '</div>' +
    '<ul class="plain">' + (d.reasons || []).map(x => '<li>' + esc(x) + '</li>').join('') + '</ul>' +
    ((d.guardrailBreaches || []).length
      ? '<div class="banner warn">Guardrail breaches:<ul>' + d.guardrailBreaches.map(gb =>
        '<li><span class="mono">' + esc(gb.metric) + '</span> — ' + esc(gb.detail || '') + '</li>').join('') + '</ul></div>' : '');
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
  return '<table class="grid" id="ro-steps-tbl"><thead><tr><th>#</th><th>Step</th><th>Eligibility</th>' +
    '<th>Min runtime</th><th>Observation window</th><th>Advance</th><th>Status</th></tr></thead><tbody>' +
    (steps.map((s, i) => {
      let st;
      if (plan.state === 'completed' || i < plan.currentStep) st = chip('done', 'green');
      else if (i === plan.currentStep) {
        st = plan.state === 'rolled_back' ? chip('stopped here', 'red')
          : chip(plan.state === 'paused' ? 'held here' : 'current', plan.state === 'paused' ? 'amber' : 'blue');
      } else st = chip('pending', 'gray');
      return '<tr' + (i === plan.currentStep ? ' class="ro-current"' : '') + '><td>' + i +
        '</td><td class="mono">' + esc(s.name) + '</td><td>' + esc(s.ratio) +
        '%</td><td>' + esc(fmtDurNs(s.minRuntime)) + '</td><td>' + esc(fmtDurNs(s.observationWindow)) +
        '</td><td>' + chip(s.autoAdvance ? 'auto' : 'manual hold', s.autoAdvance ? 'green' : 'amber') +
        '</td><td>' + st + '</td></tr>';
    }).join('') || '<tr><td colspan="7" class="muted">no steps</td></tr>') +
    '</tbody></table>' +
    '<div class="muted">Gate = the pre-registered fixed-sample decision (data health → guardrails → min sample → significance). ' +
    'Auto-advance fires only after min runtime + observation window on healthy data; SRM / guardrail breach pauses or rolls the plan back.</div>';
}

function renderRolloutHistory(plan) {
  const hist = (plan.history || []).slice().reverse(); // newest first
  return '<table class="grid" id="ro-history-tbl"><thead><tr><th>Time</th><th>Actor</th><th>Action</th>' +
    '<th>Step</th><th>Detail (skip/block reason)</th></tr></thead><tbody>' +
    (hist.map(ev =>
      '<tr><td class="nowrap">' + esc(fmtTime(ev.at)) + '</td><td>' + esc(ev.actor || '—') + '</td><td>' +
      chip(ev.action, PLAN_ACTION_CHIP[ev.action] || 'gray') + '</td><td>' + esc(ev.step ?? '—') +
      '</td><td>' + esc(ev.detail || '—') + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">no actions recorded yet</td></tr>') +
    '</tbody></table>';
}

function renderRolloutCreate() {
  return '<p class="muted" id="ro-none">No release plan registered. Pre-register a staged rollout: each step raises the experiment ' +
    'eligibility (strictly increasing) and dwells for min runtime + observation window before the decision gate may advance it.</p>' +
    '<div id="ro-steps-editor"></div>' +
    '<div class="row"><button class="btn sm" id="ro-add-step">+ Add step</button></div>' +
    '<div class="row"><button class="btn primary" id="ro-create">Register release plan</button></div>' +
    '<div class="muted">Server validates: ratio ∈ (0,100] strictly increasing · minRuntime &gt; 0 · experiment reviewed/scheduled/running · one plan per experiment (409 on duplicate).</div>';
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
    banner = '<div class="banner err" id="ro-banner"><b>ROLLED BACK — enrollment stopped</b><div>' +
      esc(plan.pausedReason || 'decision gate fired rollback (guardrail breach or significantly worse primary)') + '</div></div>';
  } else if (plan.state === 'paused') {
    const reason = plan.pausedReason || '';
    const blocked = PLAN_BLOCK_RE.test(reason);
    banner = '<div class="banner ' + (blocked ? 'err' : 'warn') + '" id="ro-banner"><b>' +
      (blocked ? 'BLOCKED — auto-paused by the engine' : 'PAUSED') + '</b><div>' + esc(reason || 'no reason recorded') + '</div>' +
      (blocked
        ? '<div class="muted">The engine auto-pauses on SRM / data-health problems and never auto-resumes. Inspect the report, then Resume — the step clock restarts.</div>'
        : '') + '</div>';
  } else if (plan.state === 'completed') {
    banner = '<div class="banner ok" id="ro-banner">Plan completed — the flag stays at ' + esc(cur ? cur.ratio : '—') +
      '%. Full rollout is a human publish decision (Flags view).</div>';
  } else if (plan.state === 'armed') {
    banner = '<div class="banner" id="ro-banner">Armed — waiting for the experiment to enter <b>running</b>; the engine then applies step 0.</div>';
  }
  if (plan.manualOverride) {
    banner += '<div class="banner warn" id="ro-takeover-banner"><b>Manual takeover active</b> — the engine takes no automatic action ' +
      'on this plan (no advance / pause / rollback). Operators drive it with the actions below.</div>';
  }

  const done = plan.state === 'completed' ? steps.length : Math.min(plan.currentStep, steps.length);
  const pct = steps.length ? Math.round(100 * done / steps.length) : 0;
  return banner +
    '<div class="row" id="ro-head">' +
      chip('plan ' + plan.state, PLAN_CHIP[plan.state] || 'gray') + ' ' +
      (plan.manualOverride ? chip('manual override', 'red') : chip('engine auto', 'green')) +
      ' <span id="ro-progress-txt">step ' + (steps.length ? plan.currentStep + 1 : 0) + '/' + steps.length +
      (cur ? ' — <span class="mono">' + esc(cur.name) + '</span> @ ' + esc(cur.ratio) + '% eligibility' : '') + '</span>' +
    '</div>' +
    '<div class="bar" id="ro-bar"><i style="width:' + pct + '%"></i></div>' +
    '<div class="row"><span class="muted">Next gate evaluation:</span> <b id="ro-next-eval">' + esc(next ? fmtTime(next) : '—') + '</b>' +
    (next ? ' <span class="muted">(step started ' + esc(fmtTime(plan.stepStartedAt)) + ' — the gate fires only after min runtime + observation window on healthy data)</span>'
      : plan.state === 'running' ? ' <span class="muted">(waiting for the step clock)</span>'
      : plan.state === 'paused' ? ' <span class="muted">(held — nothing is evaluated while paused)</span>'
      : plan.state === 'armed' ? ' <span class="muted">(starts when the experiment enters running)</span>' : '') +
    '</div>' +
    '<h4>Plan steps</h4>' + renderRolloutSteps(plan) +
    '<h4>Action history <span class="muted">(newest first · capped at 64 · immutable trail in Audit)</span></h4>' +
    renderRolloutHistory(plan) +
    '<div class="row actions">' +
      '<button class="btn sm" id="ro-refresh">Refresh</button>' +
      (plan.state === 'armed' || plan.state === 'running' ? '<button class="btn" id="ro-pause">Pause…</button>' : '') +
      (plan.state === 'paused' ? '<button class="btn primary" id="ro-resume">Resume…</button>' : '') +
      (plan.state === 'running' ? '<button class="btn" id="ro-advance">Advance now…</button>' : '') +
      (active
        ? (plan.manualOverride
          ? '<button class="btn" id="ro-takeover-off">Release takeover…</button>'
          : '<button class="btn danger" id="ro-takeover-on">Manual takeover…</button>')
        : '') +
    '</div>' +
    '<div class="muted">Plan registered by ' + esc(plan.createdBy || '—') + ' · updated ' + esc(fmtTime(plan.updatedAt)) + '</div>';
}

function rolloutStepRow() {
  const row = document.createElement('div');
  row.className = 'vrow';
  row.innerHTML = '<input class="rs-name" placeholder="step name e.g. canary-5">' +
    '<input class="rs-ratio" type="number" step="0.01" min="0" max="100" placeholder="eligibility % (increasing)">' +
    '<input class="rs-min" type="number" step="0.5" min="0" placeholder="min runtime (h)">' +
    '<input class="rs-obs" type="number" step="0.5" min="0" placeholder="observation window (h)">' +
    '<label class="inline"><input type="checkbox" class="rs-auto"> auto-advance</label>' +
    '<button class="btn sm" title="remove">✕</button>';
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
    if (!confirm('Pause the release plan? The engine takes no automatic action until resumed; on resume the step clock RESTARTS (the full observation window must elapse again).')) return;
    const reason = prompt('Pause reason (required):', '');
    if (!reason) { toast('Reason is required', false); return; }
    run(() => api(base + '/pause', { method: 'POST', body: { reason } }), 'Release plan paused');
  };

  const resume = $('#ro-resume', m);
  if (resume) resume.onclick = () => {
    if (!confirm('Resume the release plan? The step clock restarts — the full pre-registered observation window must elapse again before the gate may fire.')) return;
    run(() => api(base + '/resume', { method: 'POST' }), 'Release plan resumed');
  };

  const adv = $('#ro-advance', m);
  if (adv) adv.onclick = () => {
    if (!confirm('Advance the plan now? Manual advance is a human publish decision — it is NOT gated by the report verdict (the current verdict is recorded in history). On the final step this COMPLETES the plan.')) return;
    const reason = prompt('Advance reason (optional):', '') || '';
    run(() => api(base + '/advance', { method: 'POST', body: { reason } }), 'Release plan advanced');
  };

  const tkOn = $('#ro-takeover-on', m);
  if (tkOn) tkOn.onclick = () => {
    if (!confirm('Enable MANUAL TAKEOVER? The engine will take NO automatic action on this plan — no advancing, no pausing, no rollback — until takeover is released. Operators drive the rollout manually.')) return;
    const reason = prompt('Takeover reason (required):', '');
    if (!reason) { toast('Reason is required', false); return; }
    run(() => api(base + '/takeover', { method: 'POST', body: { enabled: true, reason } }),
      'Manual takeover enabled — the engine stands down');
  };

  const tkOff = $('#ro-takeover-off', m);
  if (tkOff) tkOff.onclick = () => {
    if (!confirm('Release manual takeover? The engine resumes automatic operation from the recorded plan state.')) return;
    const reason = prompt('Reason (optional):', '') || '';
    run(() => api(base + '/takeover', { method: 'POST', body: { enabled: false, reason } }),
      'Manual takeover released — engine control restored');
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
    if (!steps.length) { toast('Add at least one step', false); return; }
    if (steps.some(s => !s.name || !(s.ratio > 0 && s.ratio <= 100))) { toast('Each step needs a name and eligibility % in (0,100]', false); return; }
    for (let i = 1; i < steps.length; i++) {
      if (steps[i].ratio <= steps[i - 1].ratio) { toast('Ratios must strictly increase (step ' + (i + 1) + ')', false); return; }
    }
    if (steps.some(s => !(s.minRuntime > 0))) { toast('Min runtime must be > 0 for every step', false); return; }
    run(() => api(base, { method: 'POST', body: { steps } }), 'Release plan registered (armed)');
  };
}

// ---------------------------------------------------------------------------
// View: Debug
// ---------------------------------------------------------------------------
async function viewDebug(m) {
  if (!needScope(m)) return;
  m.innerHTML =
    '<div class="view-head"><h2>Debug evaluation</h2>' +
    '<span class="muted">debug never records exposures</span></div>' +
    '<section class="card">' +
    '<div class="form-grid">' +
      '<label class="field">Flag key<input id="d-key" placeholder="my-flag"></label>' +
      '<label class="field">Targeting key<input id="d-tk" placeholder="user_id value"></label>' +
    '</div>' +
    '<label class="field">Attributes (JSON object)<textarea id="d-attrs" class="json" rows="8" spellcheck="false"></textarea></label>' +
    '<div class="row"><button class="btn primary" id="d-run">Evaluate</button></div></section>' +
    '<section class="card hidden" id="d-out"><h3>Result</h3><div id="d-summary" class="row"></div>' +
    '<pre id="d-json" class="pre"></pre></section>';
  $('#d-attrs', m).value = '{\n  \n}';
  $('#d-run', m).onclick = async () => {
    let attrs;
    try { attrs = JSON.parse($('#d-attrs', m).value || '{}'); }
    catch (e) { toast('Attributes JSON invalid: ' + e.message, false); return; }
    const flagKey = $('#d-key', m).value.trim();
    if (!flagKey) { toast('Flag key required', false); return; }
    try {
      const res = await api(scopePath() + '/debug/eval', {
        method: 'POST',
        body: { flagKey, context: { targetingKey: $('#d-tk', m).value.trim(), attributes: attrs } },
      });
      const r = (res && res.result) || {};
      $('#d-out', m).classList.remove('hidden');
      $('#d-summary', m).innerHTML =
        chip('value: ' + JSON.stringify(r.value), 'blue') + ' ' +
        chip('variation: ' + (r.variationType || '—'), 'teal') + ' ' +
        chip('reason: ' + (r.reason || '—'), r.failed ? 'red' : 'green') +
        (r.errorCode ? ' ' + chip('error: ' + r.errorCode, 'red') : '') +
        (r.bucket != null ? ' <span class="muted">bucket ' + esc(r.bucket) + '</span>' : '') +
        ' <span class="muted">manifest r' + esc(res.manifestRevision ?? '—') +
        (res.note ? ' · ' + esc(res.note) : '') + '</span>';
      $('#d-json', m).textContent = JSON.stringify(res, null, 2);
    } catch (e) { toast(errMsg(e), false); }
  };
}

// ---------------------------------------------------------------------------
// View: Tokens
// ---------------------------------------------------------------------------
function tokenRows(tokens) {
  return tokens.map(t =>
    '<tr><td>' + esc(t.name) + '</td><td>' + chip(t.kind, t.kind === 'admin' ? 'teal' : t.kind === 'sdk' ? 'blue' : 'gray') +
    '</td><td>' + esc(t.role || '—') + '</td><td class="mono">' + esc(t.prefix || '—') +
    '</td><td class="mono">' + esc(t.environmentId || '—') + '</td><td class="nowrap">' + esc(fmtTime(t.createdAt)) +
    '</td><td>' + (t.revoked ? chip('revoked', 'red') : chip('active', 'green')) + '</td><td>' +
    (t.revoked ? '' : '<button class="btn sm danger" data-rev="' + esc(t.id) + '" data-name="' + esc(t.name) + '">Revoke</button>') +
    '</td></tr>').join('') || '<tr><td colspan="8" class="muted">no tokens</td></tr>';
}

async function viewTokens(m) {
  if (!needWs(m)) return;
  const tokens = (await api(wsPath() + '/tokens')) || [];
  m.innerHTML =
    '<div class="view-head"><h2>Tokens</h2><span class="muted">' + tokens.length + ' token(s)</span></div>' +
    '<div id="tok-secret"></div>' +
    '<section class="card"><h3>Create token</h3><div class="form-grid">' +
      '<label class="field">Name<input id="t-name"></label>' +
      '<label class="field">Kind<select id="t-kind"><option>admin</option><option>sdk</option><option>collect</option></select></label>' +
      '<label class="field">Role<select id="t-role"><option>owner</option><option>publisher</option><option>analyst</option><option>viewer</option></select></label>' +
      '<label class="field">Environment (sdk/collect)<select id="t-env"><option value="">— none (workspace scope) —</option>' +
      S.envs.map(v => '<option value="' + esc(v.slug) + '">' + esc(v.name || v.slug) + '</option>').join('') +
      '</select></label></div>' +
    '<div class="row"><button class="btn primary" id="t-create">Create</button></div></section>' +
    '<table class="grid"><thead><tr><th>Name</th><th>Kind</th><th>Role</th><th>Prefix</th><th>Env</th><th>Created</th><th>Status</th><th></th></tr></thead>' +
    '<tbody>' + tokenRows(tokens) + '</tbody></table>';
  $('#t-create', m).onclick = async () => {
    const body = {
      name: $('#t-name', m).value.trim(),
      kind: $('#t-kind', m).value,
      role: $('#t-role', m).value,
      environment: $('#t-env', m).value,
    };
    if (!body.name) { toast('Name required', false); return; }
    try {
      const res = await api(wsPath() + '/tokens', { method: 'POST', body });
      const secret = res.Secret || '';
      $('#tok-secret', m).innerHTML =
        '<div class="banner warn"><b>Copy this secret now — it cannot be shown again.</b>' +
        '<div class="row"><input class="mono grow" id="secret-val" readonly>' +
        '<button class="btn sm" id="secret-copy">Copy</button></div></div>';
      $('#secret-val', m).value = secret;
      $('#secret-copy', m).onclick = () => {
        const inp = $('#secret-val', m);
        inp.select();
        if (navigator.clipboard) navigator.clipboard.writeText(inp.value).then(() => toast('Secret copied'));
        else document.execCommand('copy');
      };
      toast('Token created');
      $('tbody', m).innerHTML = tokenRows((await api(wsPath() + '/tokens')) || []);
    } catch (e) { toast(errMsg(e), false); }
  };
  $('table.grid', m).addEventListener('click', ev => {
    const b = ev.target.closest('[data-rev]');
    if (!b) return;
    if (!confirm('Revoke token "' + b.dataset.name + '"? Clients using it will immediately get 401.')) return;
    run(() => api('/api/v1/admin/tokens/' + encodeURIComponent(b.dataset.rev), { method: 'DELETE' }), 'Token revoked');
  });
}

// ---------------------------------------------------------------------------
// View: Audit
// ---------------------------------------------------------------------------
async function viewAudit(m) {
  if (!needWs(m)) return;
  m.innerHTML =
    '<div class="view-head"><h2>Audit log</h2>' +
    '<label class="inline">limit <select id="a-limit">' +
    [50, 200, 500, 1000].map(n => '<option' + (n === S.auditLimit ? ' selected' : '') + '>' + n + '</option>').join('') +
    '</select></label></div><div id="a-box"><p class="muted">Loading…</p></div>';
  const load = async () => {
    const rows = (await api(wsPath() + '/audit?limit=' + S.auditLimit)) || [];
    $('#a-box', m).innerHTML =
      '<table class="grid"><thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Object</th><th>Env</th><th>Detail</th></tr></thead><tbody>' +
      (rows.map(a => {
        const detail = typeof a.detail === 'string' ? a.detail : JSON.stringify(a.detail ?? '');
        return '<tr><td class="nowrap">' + esc(fmtTime(a.time)) + '</td><td>' + esc(a.actor || '—') +
          '</td><td class="mono">' + esc(a.action) + '</td><td class="mono">' + esc(a.object || '—') +
          '</td><td class="mono">' + esc(a.environmentId || '—') + '</td>' +
          '<td class="detail" title="' + esc(detail) + '">' + esc(detail) + '</td></tr>';
      }).join('') || '<tr><td colspan="6" class="muted">empty</td></tr>') +
      '</tbody></table>';
  };
  $('#a-limit', m).onchange = ev => {
    S.auditLimit = Number(ev.target.value);
    load().catch(e => { $('#a-box', m).innerHTML = errBanner(e); });
  };
  await load();
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
      m.innerHTML = '<div class="banner warn">AI governance endpoints are not enabled on this server (501).</div>';
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
    '<div class="view-head"><h2>AI resources</h2></div>' +
    '<section class="card"><h3>Register resource</h3><div class="form-grid">' +
      '<label class="field">Type<select id="ai-type">' + typeOpts.map(t => '<option>' + t + '</option>').join('') + '</select></label>' +
      '<label class="field">Logical ID<input id="ai-logical" placeholder="checkout-helper"></label>' +
      '<label class="field">Name<input id="ai-name"></label></div>' +
    '<div class="row"><button class="btn primary" id="ai-reg">Register</button></div></section>' +
    '<section class="card"><h3>Resources (' + resources.length + ')</h3>' +
    '<table class="grid" id="ai-res-tbl"><thead><tr><th>Type</th><th>Logical ID</th><th>Physical ID</th><th>Owner</th><th>Created</th><th>Actions</th></tr></thead><tbody>' +
    (resources.map(r =>
      '<tr><td>' + chip(r.type, 'teal') + '</td><td class="mono">' + esc(r.logicalId) + '</td><td class="mono">' +
      esc(r.physicalId || '—') + '</td><td>' + esc(r.owner || '—') + '</td><td class="nowrap">' + esc(fmtTime(r.createdAt)) +
      '</td><td class="nowrap"><button class="btn sm" data-pin="' + esc(r.type + '|' + r.logicalId) + '">Pin version</button> ' +
      '<button class="btn sm danger" data-revoke="' + esc(r.type + '|' + r.logicalId) + '">Revoke</button></td></tr>').join('') ||
      '<tr><td colspan="6" class="muted">none registered</td></tr>') +
    '</tbody></table>' +
    (revocations.length
      ? '<h4>Revocations</h4><table class="grid" id="ai-rev-tbl"><thead><tr><th>Type</th><th>Logical ID</th><th>Reason</th><th></th></tr></thead><tbody>' +
        revocations.map(rv =>
          '<tr><td>' + chip(rv.type, 'red') + '</td><td class="mono">' + esc(rv.logicalId) + '</td><td>' + esc(rv.reason || '—') +
          '</td><td><button class="btn sm" data-reinstate="' + esc(rv.type + '|' + rv.logicalId) + '">Reinstate</button></td></tr>').join('') +
        '</tbody></table>'
      : '') +
    '</section>' +
    '<section class="card"><h3>Resolve viewer</h3><div class="row">' +
      '<select id="ai-r-type">' + typeOpts.map(t => '<option>' + t + '</option>').join('') + '</select>' +
      '<input id="ai-r-logical" placeholder="logicalId">' +
      '<input id="ai-r-ver" placeholder="version (blank = current pin)">' +
      '<button class="btn" id="ai-resolve">Resolve</button></div>' +
      '<div id="ai-r-out"></div></section>' +
    '<section class="card"><h3>Model allowlist</h3><div id="ai-allow"></div>' +
    '<div class="row"><button class="btn sm" id="ai-allow-add">+ Add entry</button>' +
    '<button class="btn primary" id="ai-allow-save">Save allowlist</button></div></section>';

  const split = v => { const i = v.indexOf('|'); return [v.slice(0, i), v.slice(i + 1)]; };
  $('#ai-reg', m).onclick = () => {
    const body = {
      type: $('#ai-type', m).value,
      logicalId: $('#ai-logical', m).value.trim(),
      name: $('#ai-name', m).value.trim(),
    };
    if (!body.logicalId || !body.name) { toast('logicalId and name are required', false); return; }
    run(() => api(aiPath, { method: 'POST', body }), 'Resource registered');
  };
  $('#ai-res-tbl', m).addEventListener('click', ev => {
    const pin = ev.target.closest('[data-pin]'), rev = ev.target.closest('[data-revoke]');
    if (pin) {
      const [t, l] = split(pin.dataset.pin);
      promptForm('Pin new version — ' + t + '/' + l,
        [{ name: 'content', label: 'Content', type: 'textarea', rows: 10, placeholder: 'prompt text / skill JSON / agent spec…' }],
        'Pin', async v => {
          if (!v.content) throw new ApiError(0, 'validation_failed', 'content is required');
          await run(() => api(aiPath + '/' + encodeURIComponent(t) + '/' + encodeURIComponent(l) + '/pin',
            { method: 'POST', body: { content: v.content } }), 'Version pinned');
        });
    } else if (rev) {
      const [t, l] = split(rev.dataset.revoke);
      const reason = prompt('Revoke ' + t + '/' + l + ' — reason (required):', '');
      if (!reason) { toast('Reason is required', false); return; }
      if (!confirm('Revoke this AI resource? SDK resolves will fail with 423.')) return;
      run(() => api(aiPath + '/' + encodeURIComponent(t) + '/' + encodeURIComponent(l) + '/revoke',
        { method: 'POST', body: { reason } }), 'Resource revoked');
    }
  });
  const revTbl = $('#ai-rev-tbl', m);
  if (revTbl) {
    revTbl.addEventListener('click', ev => {
      const b = ev.target.closest('[data-reinstate]');
      if (!b) return;
      const [t, l] = split(b.dataset.reinstate);
      const reason = prompt('Reinstate ' + t + '/' + l + ' — reason:', '') || '';
      run(() => api(aiPath + '/' + encodeURIComponent(t) + '/' + encodeURIComponent(l) + '/reinstate',
        { method: 'POST', body: { reason } }), 'Resource reinstated');
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
        '<div class="row">' + chip('version: ' + (res.version || '—'), 'blue') + ' ' +
        chip('digest: ' + (res.digest || '—'), 'gray') + '</div>';
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
    row.innerHTML = '<input class="al-provider" placeholder="provider e.g. openai">' +
      '<input class="al-model" placeholder="model e.g. gpt-4o">' +
      '<label class="inline"><input type="checkbox" class="al-enabled"> enabled</label>' +
      '<button class="btn sm">✕</button>';
    $('.al-provider', row).value = (ent && ent.provider) || '';
    $('.al-model', row).value = (ent && ent.model) || '';
    $('.al-enabled', row).checked = !ent || ent.enabled !== false;
    $('button', row).onclick = () => row.remove();
    allowBox.appendChild(row);
  };
  allow.forEach(addAllowRow);
  if (!allow.length) allowBox.innerHTML = '<p class="muted">Allowlist empty.</p>';
  $('#ai-allow-add', m).onclick = () => { if (!allow.length && !$('.vrow', allowBox)) allowBox.innerHTML = ''; addAllowRow(null); };
  $('#ai-allow-save', m).onclick = () => {
    const entries = $$('.vrow', allowBox).map(row => ({
      provider: $('.al-provider', row).value.trim(),
      model: $('.al-model', row).value.trim(),
      enabled: $('.al-enabled', row).checked,
    })).filter(e2 => e2.provider && e2.model);
    run(() => api(wsPath() + '/model-allowlist', { method: 'PUT', body: { entries } }), 'Allowlist saved');
  };
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------
Object.assign(VIEWS, {
  flags: viewFlags,
  segments: viewSegments,
  experiments: viewExperiments,
  debug: viewDebug,
  tokens: viewTokens,
  audit: viewAudit,
  ai: viewAI,
});

function init() {
  $('#btn-connect').onclick = doConnect;
  $('#auth-token').addEventListener('keydown', ev => { if (ev.key === 'Enter') doConnect(); });
  $('#btn-reauth').onclick = () => showAuth('');

  $$('#nav button').forEach(b => {
    b.onclick = () => {
      if (S.view === b.dataset.view) return;
      S.view = b.dataset.view;
      S.flagKey = S.segKey = S.expId = null;
      S.flagNew = S.expCreate = false;
      route();
    };
  });
  $('#sel-ws').onchange = async ev => {
    S.ws = ev.target.value;
    S.env = '';
    S.flagKey = S.segKey = S.expId = null;
    localStorage.setItem('golab_ws', S.ws);
    try { await loadEnvs(); } catch (e) { toast(errMsg(e), false); }
    route();
    probeAI();
  };
  $('#sel-env').onchange = ev => {
    S.env = ev.target.value;
    S.flagKey = S.segKey = S.expId = null;
    localStorage.setItem('golab_env', S.env);
    route();
    probeAI();
  };
  $('#btn-new-ws').onclick = () => promptForm('New workspace', [
    { name: 'slug', label: 'Slug', placeholder: 'acme' },
    { name: 'name', label: 'Display name', placeholder: 'Acme Inc' },
  ], 'Create', async v => {
    const slug = v.slug.trim();
    if (!slug) throw new ApiError(0, 'validation_failed', 'slug is required');
    await api('/api/v1/admin/workspaces', { method: 'POST', body: { slug, name: v.name.trim() || slug } });
    toast('Workspace created');
    S.ws = slug;
    localStorage.setItem('golab_ws', S.ws);
    await loadWorkspaces();
    route();
    probeAI();
  });
  $('#btn-new-env').onclick = () => {
    if (!S.ws) { toast('Create/select a workspace first', false); return; }
    promptForm('New environment in ' + S.ws, [
      { name: 'slug', label: 'Slug', placeholder: 'production' },
      { name: 'name', label: 'Display name', placeholder: 'Production' },
    ], 'Create', async v => {
      const slug = v.slug.trim();
      if (!slug) throw new ApiError(0, 'validation_failed', 'slug is required');
      await api(wsPath() + '/environments', { method: 'POST', body: { slug, name: v.name.trim() || slug } });
      toast('Environment created');
      S.env = slug;
      localStorage.setItem('golab_env', S.env);
      await loadEnvs();
      route();
      probeAI();
    });
  };

  if (localStorage.getItem('golab_token')) {
    $('#auth-error').textContent = '';
    boot().catch(e => showAuth(e.status === 401 ? '' : errMsg(e)));
  } else {
    showAuth('');
  }
}

document.addEventListener('DOMContentLoaded', init);
