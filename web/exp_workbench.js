'use strict';

// ---------------------------------------------------------------------------
// Grayling admin console — experiment workbench (detail view). Embedded static
// file, no build step; originally extracted from app.js and reorganized into
// the four-zone layout (§4.4). Loaded BEFORE app.js (plain <script> order)
// and concatenated with the other console scripts by the Node-VM contract
// harnesses, so all files share one script scope: the functions below
// resolve the shared app.js/i18n.js helpers (esc/t/chip/api/S/can/route/
// $/$$/toast/run/errBanner/reportError/actionDialog/fmtTime/fmtDurNs/
// NS_HOUR/EXP_CHIP/EXP_TABS/VERDICT_CHIP/ApiError/requestChange/
// downloadReport/confirmLeaveEditor/shortHash) at call time, and app.js
// resolves viewExpDetail from here.
//
// Contents: experiment detail — the always-visible status band
// (renderExpStatusBand), the deep-linkable zone navigation
// (renderExpZoneNav; S.expTab ∈ EXP_TABS, router lives in app.js), the
// six-state first-screen hero (classifyExpStatus + renderExpHero, §4.4
// 状态×文案) and the five zone loaders (decision summary / data health /
// effect evidence / rollout timeline / settings & audit) — plus report,
// health and bind-readiness renderers and the staged-rollout section
// (PLAN_* constants, renderRollout family, bindRollout). The experiments
// list and the creation wizard stay in app.js.
// ---------------------------------------------------------------------------

async function viewExpDetail(m) {
  const id = S.expId;
  const p = '/api/v1/admin/experiments/' + encodeURIComponent(id);
  const e = await api(p);
  if (!EXP_TABS.includes(S.expTab)) S.expTab = 'summary';
  const tab = S.expTab;
  const heading =
    '<div class="view-head"><button class="btn sm" id="exp-back">' + esc(t('exp.back')) + '</button>' +
    '<h2 class="mono">' + esc(e.id) + '</h2>' + chip(e.state, EXP_CHIP[e.state] || 'gray') +
    '<span class="muted">' + esc(t('exp.meta', { flag: e.flagKey, rev: e.flagRevision || 0, epoch: e.epoch || 0, by: e.createdBy || '—' })) + '</span></div>';
  // Persistent chrome (§4.4): the status band stays visible above every zone,
  // the auto-refresh switch is global, and each zone renders into #exp-zone.
  m.innerHTML = heading +
    renderExpStatusBand(e, null) +
    '<div class="row" id="exp-report-controls"><label class="inline"><input type="checkbox" id="exp-auto"> ' +
      esc(t('exp.autoRefresh')) + '</label></div>' +
    renderExpZoneNav(tab) +
    '<div id="exp-zone"></div>';
  $('#exp-back', m).onclick = () => { S.expId = null; S.expTab = 'summary'; route(); };
  for (const button of $$('[data-exp-tab]', m)) {
    button.onclick = () => {
      if (S.expTab !== button.dataset.expTab) { S.expTab = button.dataset.expTab; route(); }
    };
  }
  const zone = $('#exp-zone', m);
  const gotoZone = next => { S.expTab = next; route(); };
  let lastReport = null;
  let reportDetailOpen = false;
  let reportRequest = 0;
  let lastReportError = null;
  // Six-state hero extras (§4.4). undefined = not fetched yet, null = fetch
  // failed / nothing found, object = real payload. Both fetchers carry
  // staleness guards (view + experiment + request counter) and only ever
  // re-render the hero.
  let heroDiagnostics;
  let heroDiagRequest = 0;
  let latestTransition;
  let transitionRequest = 0;
  const preLaunch = ['draft', 'reviewed', 'scheduled'].includes(e.state);
  // The rollout gate status lives on m.dataset so the rollout zone replays the
  // latest known report status whenever it renders (pre-zoning pattern).
  setRolloutReportStatus(m, preLaunch ? 'not_required' : can('report.read') ? 'pending' : 'restricted');
  const heroClass = () => classifyExpStatus(e, lastReport, heroDiagnostics, latestTransition,
    { error: lastReportError, canRead: can('report.read'), diagFailed: heroDiagnostics === null });
  const snapshotHtml = () =>
    lastReport && !lastReportError ? renderReportSummary(lastReport) : '';
  const heroGoto = goto => {
    if (goto === 'audit') {
      S.view = 'audit'; S.auditObject = id; S.auditQuery = '';
      S.auditCursors = [0]; S.auditPage = 0;
      route();
    } else if (goto === 'retry-diagnostics') {
      heroDiagnostics = undefined;
      fetchHeroDiagnostics();
    } else if (EXP_TABS.includes(goto)) gotoZone(goto);
  };
  const bindHeroActions = root => {
    for (const button of $$('[data-hero-goto]', root)) {
      button.onclick = () => heroGoto(button.dataset.heroGoto);
    }
  };
  // parseTransition reads the real experiment.transition audit entry: the
  // actor/time come from the audit record, to/reason from its JSON detail
  // ({"to":…,"reason":…} written by the transition handler).
  const parseTransition = entry => {
    let detail = {};
    try {
      detail = typeof entry.detail === 'string' && entry.detail
        ? JSON.parse(entry.detail) : (entry.detail || {});
    } catch { detail = {}; }
    return { actor: entry.actor || '', time: entry.time || '',
      to: detail.to || '', reason: detail.reason || '' };
  };
  const fetchLatestTransition = async () => {
    const request = ++transitionRequest;
    try {
      const page = (await api(wsPath() + '/audit?object=' + encodeURIComponent(id) + '&limit=20')) || [];
      if (S.expId !== id || S.view !== 'experiments' || request !== transitionRequest) return;
      const entry = page.find(a => a && a.action === 'experiment.transition');
      latestTransition = entry ? parseTransition(entry) : null;
    } catch {
      if (S.expId !== id || S.view !== 'experiments' || request !== transitionRequest) return;
      latestTransition = null; // degraded endedUnknown copy, never a guess
    }
    renderHero();
  };
  const fetchHeroDiagnostics = async () => {
    const request = ++heroDiagRequest;
    try {
      const d = await api(p + '/diagnostics');
      if (S.expId !== id || S.view !== 'experiments' || request !== heroDiagRequest) return;
      heroDiagnostics = d;
    } catch {
      if (S.expId !== id || S.view !== 'experiments' || request !== heroDiagRequest) return;
      heroDiagnostics = null; // generic waiting copy + retry, no guessed sub-branch
    }
    renderHero();
  };
  // ensureHeroData lazily fetches the extra sources the classified kind
  // needs: the diagnostics payload for the no_samples sub-branch (only when
  // the report carries no embedded diagnostics) and the transition audit for
  // the ended copy. Guards make each fetch happen at most once per retry.
  const ensureHeroData = cls => {
    if (!cls) return;
    if (cls.kind === 'no_samples' && cls.params.subBranch === 'unknown' &&
        heroDiagnostics === undefined && !(lastReport && lastReport.diagnostics)) {
      fetchHeroDiagnostics();
    }
    if (cls.kind === 'ended' && latestTransition === undefined) fetchLatestTransition();
  };
  const renderHero = () => {
    const hero = $('#exp-decision', m);
    if (!hero) return;
    const cls = heroClass();
    hero.innerHTML = renderExpHero(cls, lastReport, lastReportError);
    bindHeroActions(hero);
    ensureHeroData(cls);
  };
  const loadReport = async (showDetail = false) => {
    if (showDetail) reportDetailOpen = true;
    const request = ++reportRequest;
    try {
      const report = await api(p + '/report');
      if (S.expId !== id || S.view !== 'experiments' || request !== reportRequest) return false;
      lastReport = report;
      lastReportError = null;
      renderHero();
      const snapshot = $('#exp-summary-snapshot', m);
      if (snapshot) snapshot.innerHTML = snapshotHtml();
      const reportTime = $('#exp-band-report-time', m);
      if (reportTime) reportTime.textContent = fmtTime(report.generatedAt);
      const healthCard = $('#diag-report-card', m);
      if (healthCard) healthCard.innerHTML = renderHealthReportCard(report);
      setRolloutReportStatus(m, 'ready');
      const box = $('#report-box', m);
      if (reportDetailOpen && box) renderReport(box, report);
      return true;
    } catch (err) {
      if (request !== reportRequest) return false;
      lastReportError = err;
      renderHero();
      const snapshot = $('#exp-summary-snapshot', m);
      if (snapshot) snapshot.innerHTML = snapshotHtml(); // cleared — a stale report is never presented as current
      setRolloutReportStatus(m, 'unavailable');
      const box = $('#report-box', m);
      if (reportDetailOpen && box) {
        if (lastReport) {
          renderReport(box, lastReport);
          box.innerHTML = '<div class="banner warn" role="status">' + esc(t('report.stale')) + '</div>' + errBanner(err) + box.innerHTML;
        } else box.innerHTML = errBanner(err);
      }
      return false;
    }
  };
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
  // --- zone: decision summary (six-state hero + snapshot + actions) --------
  const loadZoneSummary = () => {
    zone.innerHTML =
      '<div class="exp-decision" id="exp-decision" role="status" aria-live="polite">' +
      renderExpHero(heroClass(), lastReport, lastReportError) + '</div>' +
      '<div id="exp-summary-snapshot">' + snapshotHtml() + '</div>' +
      '<div class="row actions" id="exp-zone-actions">' +
      '<button class="btn" id="exp-goto-health" type="button">' + esc(t('exp.zone.health')) + '</button>' +
      '<button class="btn" id="exp-goto-evidence" type="button">' + esc(t('exp.zone.evidence')) + '</button>' +
      '<button class="btn" id="exp-goto-rollout" type="button">' + esc(t('exp.zone.rollout')) + '</button>' +
      '<button class="btn" id="exp-goto-settings" type="button">' + esc(t('exp.tab.settings')) + '</button>' +
      '</div>' +
      '<p><button class="row-link" id="exp-view-prereg" type="button">' + esc(t('exp.viewPrereg')) + '</button></p>';
    bindHeroActions(zone);
    for (const next of ['health', 'evidence', 'rollout', 'settings']) {
      const button = $('#exp-goto-' + next, zone);
      if (button) button.onclick = () => gotoZone(next);
    }
    const preregLink = $('#exp-view-prereg', zone);
    if (preregLink) preregLink.onclick = () => gotoZone('settings');
    ensureHeroData(heroClass());
  };
  // --- zone: data health (merged diagnostics view) --------------------------
  const loadZoneHealth = async () => {
    zone.innerHTML = '<p class="muted">' + esc(t('misc.loading')) + '</p>';
    let d;
    try {
      d = await api(p + '/diagnostics');
    } catch (err) {
      if (S.expId !== id || S.view !== 'experiments') return;
      zone.innerHTML = errBanner(err) +
        '<div class="row"><button class="btn" id="diag-refresh">' + esc(t('btn.refresh')) + '</button></div>';
      const retry = $('#diag-refresh', zone);
      if (retry) retry.onclick = () => route();
      return;
    }
    if (S.expId !== id || S.view !== 'experiments') return;
    zone.innerHTML = renderHealthZone(e, d, lastReport);
    const refresh = $('#diag-refresh', zone);
    if (refresh) refresh.onclick = () => route();
  };
  // --- zone: effect evidence (full report + exports) -------------------------
  const loadZoneEvidence = () => {
    zone.innerHTML = '<section class="card"><h3>' + esc(t('exp.report')) + '</h3>' +
      '<div class="row"><button class="btn" id="exp-report">' + esc(t('btn.loadReport')) + '</button>' +
      (can('report.read') ? '<button class="btn" id="exp-export-csv">' + esc(t('report.exportCsv')) + '</button>' +
        '<button class="btn" id="exp-export-json">' + esc(t('report.exportJson')) + '</button>' : '') +
      '</div>' +
      '<div id="report-box"><p class="muted">' + esc(t('exp.notLoaded')) + '</p></div></section>';
    $('#exp-report', zone).onclick = () => loadReport(true);
    if (can('report.read')) {
      for (const format of ['csv', 'json']) {
        $('#exp-export-' + format, zone).onclick = async () => {
          try { await downloadReport(p + '/report/export', format); }
          catch (err) { reportError(err); }
        };
      }
    }
  };
  // --- zone: rollout timeline (IDs/gating unchanged — rollout_contract) -----
  const loadZoneRollout = () => {
    zone.innerHTML = '<section class="card"><h3>' + esc(t('ro.title')) + ' <span class="muted">' + esc(t('ro.subtitle')) + '</span></h3>' +
      '<div id="rollout-box">' + renderRollout(e) + '</div></section>';
    bindRollout(m, id, e);
    setRolloutReportStatus(m, m.dataset.rolloutReportStatus);
  };
  // --- zone: settings & audit (lifecycle + pre-registration, moved whole) ---
  const loadZoneSettings = () => {
  const prereg = '<section class="card"><h3>' + esc(t('exp.prereg')) + '</h3><table class="kv">' +
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
      (e.exposureMode === 'onRender' ? '<tr><th>' + esc(t('exp.exposureTrigger')) + '</th><td>' + esc(e.exposureTrigger || '—') + '</td></tr>' +
        '<tr><th>' + esc(t('exp.integrationOwner')) + '</th><td>' + esc(e.integrationOwner || '—') + '</td></tr>' : '') +
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
    '</section>';
    zone.innerHTML = '<div class="cols exp-detail-cols"><section class="card"><h3>' + esc(t('exp.lifecycle')) + '</h3>' +
      '<div class="bind-readiness" id="exp-bind-check" role="status">' + esc(t('exp.bindCheckLoading')) + '</div>' +
      '<div class="row actions" style="border:0;padding:0;margin:0">' +
      '<button class="btn" id="exp-review">' + esc(t('btn.review')) + '</button>' +
      '<button class="btn" id="exp-open-flag">' + esc(t('exp.openFlag')) + '</button>' +
      '<button class="btn" id="exp-recheck">' + esc(t('exp.bindRecheck')) + '</button>' +
      '<button class="btn" id="exp-bind" disabled>' + esc(t('btn.bind')) + '</button></div>' +
      '<p class="muted">' + t('exp.bindHint') + '</p>' +
      '<div class="row"><select id="exp-to">' +
      '<option value="running">running</option><option value="paused">paused</option>' +
      '<option value="stopped">stopped</option><option value="analyzed">analyzed</option>' +
      '<option value="archived">archived</option></select>' +
      '<input id="exp-reason" class="grow" placeholder="' + esc(t('ph.reasonRequired')) + '">' +
      '<button class="btn primary" id="exp-transition">' + esc(t('btn.transition')) + '</button></div>' +
    '</section>' + prereg + '</div>' +
    '<section class="card"><div class="row"><button class="btn" id="exp-open-audit" type="button">' +
      esc(t('exp.openAuditObject')) + '</button></div></section>';
    const act = (path, body, msg) => run(() => api(p + path, { method: 'POST', body }), msg);
    $('#exp-review', zone).onclick = () => {
      const note = prompt(t('prompt.reviewNote'), '') || '';
      act('/review', { note }, t('toast.reviewRecorded'));
    };
    let bindReadiness = null;
    const loadBindCheck = async () => {
      const box = $('#exp-bind-check', zone);
      const bindButton = $('#exp-bind', zone);
      if (!box || !bindButton) return false;
      bindButton.disabled = true;
      box.textContent = t('exp.bindCheckLoading');
      try {
        const readiness = await api(p + '/bind-check');
        if (S.expId !== id || S.view !== 'experiments') return false;
        bindReadiness = readiness;
        box.innerHTML = renderBindReadiness(readiness);
        bindButton.disabled = !readiness.ready || !can('flag.write');
        return readiness.ready;
      } catch (err) {
        bindReadiness = null;
        box.innerHTML = errBanner(err);
        return false;
      }
    };
    $('#exp-recheck', zone).onclick = loadBindCheck;
    $('#exp-open-flag', zone).onclick = () => {
      S.view = 'flags'; S.flagKey = e.flagKey; route();
    };
    $('#exp-bind', zone).onclick = async () => {
      if (!await loadBindCheck() || !bindReadiness?.ready) {
        toast(t('exp.bindNotReady'), false);
        return;
      }
      const checkedReadiness = bindReadiness;
      try {
        await api(p + '/bind', { method: 'POST', body: {} });
        toast(t('toast.expBound'));
        route();
      } catch (err) {
        reportError(err);
        await loadBindCheck();
        const conflictBox = $('#exp-bind-check', zone);
        if (err.status === 409 && conflictBox) {
          conflictBox.insertAdjacentHTML('afterbegin', renderBindConflict(checkedReadiness, bindReadiness));
        }
      }
    };
    if (can('flag.read')) loadBindCheck();
    else {
      $('#exp-bind-check', zone).textContent = t('exp.bindCheckPermission');
      $('#exp-recheck', zone).disabled = true;
    }
    $('#exp-transition', zone).onclick = () => {
      const to = $('#exp-to', zone).value;
      const reason = $('#exp-reason', zone).value.trim();
      if (!reason) { toast(t('toast.transitionReason'), false); return; }
      if (!confirm(t('confirm.transition', { to }))) return;
      act('/transition', { to, reason }, t('toast.transitioned', { to }));
    };
    $('#exp-open-audit', zone).onclick = () => {
      S.view = 'audit'; S.auditObject = id; S.auditQuery = '';
      S.auditCursors = [0]; S.auditPage = 0;
      route();
    };
  };
  if (tab === 'health') await loadZoneHealth();
  else if (tab === 'evidence') loadZoneEvidence();
  else if (tab === 'rollout') loadZoneRollout();
  else if (tab === 'settings') loadZoneSettings();
  else loadZoneSummary();
  if (can('report.read') && !preLaunch) {
    if (tab === 'evidence') await loadReport(true);
    else loadReport();
  }
}

// renderExpStatusBand is the always-visible band above every zone (§4.4):
// state chip, epoch, Flag revision, registration version, report time and
// environment. The report time renders '—' until a report actually loaded;
// loadReport refreshes #exp-band-report-time in place. Never invents values.
function renderExpStatusBand(e, report) {
  const field = (label, valueHtml) =>
    '<span class="exp-status-field"><span class="muted">' + esc(label) + '</span> ' + valueHtml + '</span>';
  return '<div class="exp-statusband" id="exp-statusband" role="status" aria-label="' + esc(t('exp.statusBand')) + '">' +
    chip(e.state, EXP_CHIP[e.state] || 'gray') +
    field('epoch', '<b class="mono">' + esc(e.epoch || 0) + '</b>') +
    field(t('exp.flagRevisionShort'), '<b class="mono">r' + esc(e.flagRevision || 0) + '</b>') +
    field(t('exp.registrationVersion'), '<b class="mono">' + esc(e.registrationVersion || '—') + '</b>') +
    field(t('exp.reportTime'), '<b class="mono" id="exp-band-report-time">' + esc(report ? fmtTime(report.generatedAt) : '—') + '</b>') +
    field(t('exp.environment'), '<b class="mono">' + esc(e.environmentId || '—') + '</b>') +
  '</div>';
}

// renderExpZoneNav renders the five deep-linkable zone buttons (four zones +
// the settings & audit tab). Each button routes via S.expTab so every zone
// keeps its own hash deep link; the active one carries aria-current.
function renderExpZoneNav(activeTab) {
  return '<nav class="exp-zone-nav" id="exp-zone-nav" role="tablist" aria-label="' + esc(t('exp.zones.nav')) + '">' +
    EXP_TABS.map(name => {
      const active = name === activeTab;
      return '<button type="button" role="tab" id="exp-tab-' + name + '" class="btn exp-zone-tab' +
        (active ? ' active' : '') + '" data-exp-tab="' + name + '"' +
        (active ? ' aria-current="true"' : '') + '>' +
        esc(t(name === 'settings' ? 'exp.tab.settings' : 'exp.zone.' + name)) + '</button>';
    }).join('') + '</nav>';
}

// ---------------------------------------------------------------------------
// Six-state first-screen copy (§4.4 状态×文案). classifyExpStatus is a pure
// function over REAL server data only: the experiment JSON, the loaded
// report, the diagnostics payload and the latest experiment.transition audit
// entry. Priority: report-fetch error (never present a stale report as a
// current conclusion) > missing report.read > state-derived kinds
// (not_started, ended) > report-derived kinds (no_samples, immature, blocked,
// can_advance) > loading. Hard rules: a missing ETA degrades to explicit
// unknown copy (never an estimate), assignment counts are only quoted when
// the diagnostics payload carries real assignment facts, server reasons and
// health issues are quoted verbatim, and can_advance never claims absolute
// victory. Any shape that matches no rule renders no banner at all — the
// decision snapshot below the hero carries the raw server verdict instead.
// ---------------------------------------------------------------------------
const EXP_PRELAUNCH_STATES = ['draft', 'reviewed', 'scheduled'];
const EXP_ENDED_STATES = ['paused', 'stopped', 'archived'];

// heroDiagTotals sums the REAL diagnostics flow fields (assignments and
// exposures are distinct facts; evaluations never masquerade as exposures).
function heroDiagTotals(diag) {
  const flow = (diag && diag.perVariant) || [];
  const sum = field => flow.reduce((total, v) => total + Number(v[field] || 0), 0);
  const assignments = sum('assignments');
  return {
    present: !!diag && flow.length > 0,
    assignments,
    exposures: sum('exposures'),
    // onDecision histories may carry no assignment facts at all — quoting a
    // zero then would invent "no assignments" from missing data.
    hasAssignmentFacts: !!diag && (diag.exposureMode === 'onRender' || assignments > 0),
  };
}

// immatureEta derives the earliest decidable time from real fields only:
// the running plan's next gate (stepStartedAt + step minRuntime +
// observationWindow) when a plan is running, otherwise startedAt +
// minRuntime. Returns null when neither is recorded — callers must show the
// etaUnknown copy, never an estimate.
function immatureEta(e) {
  const gate = planNextGate(e && e.releasePlan);
  if (gate) return gate;
  if (e && e.startedAt) {
    const ms = Date.parse(e.startedAt) + Number(e.minRuntime || 0) / 1e6;
    if (!isNaN(ms)) return new Date(ms);
  }
  return null;
}

// endedRollbackTarget reports where a rolled-back release plan stopped, from
// the plan's real current step — null when there is no rolled-back plan.
function endedRollbackTarget(e) {
  const plan = e && e.releasePlan;
  if (!plan || plan.state !== 'rolled_back') return null;
  const step = (plan.steps || [])[plan.currentStep];
  return step ? step.name + ' · ' + step.ratio + '%' : null;
}

// classifyExpStatus(e, report, diagnostics, latestTransition, meta) →
// { kind, params, actions }. diagnostics/latestTransition: undefined = not
// fetched yet, null = fetch failed / none found, object = real payload.
// meta: { error, canRead, diagFailed }. actions are { goto, label } button
// descriptors; goto ∈ EXP_TABS ∪ {audit, retry-diagnostics}.
function classifyExpStatus(e, report, diagnostics, latestTransition, meta = {}) {
  const state = (e && e.state) || '';
  const canRead = meta.canRead !== undefined ? meta.canRead : can('report.read');
  const act = (goto, label) => ({ goto, label });
  if (meta.error) return { kind: 'error', params: { stale: !!report }, actions: [] };
  if (EXP_PRELAUNCH_STATES.includes(state)) {
    return { kind: 'not_started', params: { state },
      actions: [act('settings', t('exp.hero.viewReadiness'))] };
  }
  if (!canRead) return { kind: 'restricted', params: {}, actions: [] };
  if (EXP_ENDED_STATES.includes(state)) {
    return { kind: 'ended',
      params: { state, transition: latestTransition || null, rollbackTarget: endedRollbackTarget(e) },
      actions: [act('audit', t('exp.hero.viewAudit')), act('settings', t('exp.tab.settings'))] };
  }
  if (!report) return { kind: 'loading', params: {}, actions: [] };
  const diag = diagnostics || report.diagnostics || null;
  const issues = report.healthIssues || [];
  const verdict = (report.decision && report.decision.verdict) || '';
  const reasons = (report.decision && report.decision.reasons) || [];
  const breaches = (report.decision && report.decision.guardrailBreaches) || [];
  const totals = heroDiagTotals(diag);
  // no_samples — the server issue string verbatim, or a real diagnostics
  // payload whose exposure total is zero.
  if (issues.includes('no randomized exposures recorded yet') ||
      (totals.present && totals.exposures === 0)) {
    let subBranch = 'unknown';
    if (totals.hasAssignmentFacts) subBranch = totals.assignments > 0 ? 'assigned' : 'none';
    const actions = [act('health', t('exp.hero.viewCollection'))];
    if (meta.diagFailed) actions.push(act('retry-diagnostics', t('btn.refresh')));
    return { kind: 'no_samples',
      params: { subBranch, assigned: totals.assignments, diagFailed: !!meta.diagFailed },
      actions };
  }
  // immature — minimum runtime not satisfied, an insufficient-sample hold, or
  // a healthy "keep running" verdict.
  if (report.minRuntimeSatisfied === false ||
      reasons.some(r => String(r).includes('minimum runtime') || String(r).includes('insufficient sample')) ||
      (report.healthy && verdict === 'continue')) {
    return { kind: 'immature', params: { eta: immatureEta(e), reasons },
      actions: [act('health', t('exp.hero.viewWatermark'))] };
  }
  // blocked — unhealthy pipeline / SRM / contamination / guardrail breach, or
  // a rollback verdict (which also covers a significantly worse primary).
  if (!report.healthy || verdict === 'rollback') {
    return { kind: 'blocked',
      params: {
        reasons: [...issues, ...reasons], breaches,
        scope: { flag: (e && e.flagKey) || '—', env: (e && e.environmentId) || '—',
          epoch: e && e.epoch !== undefined && e.epoch !== null ? e.epoch : '—' },
      },
      actions: [act('health', t('exp.hero.viewDiagnostics')),
        act('rollout', t('exp.hero.blockedRollout')),
        act('audit', t('exp.hero.blockedChanges'))] };
  }
  // can_advance — healthy ship verdict with the minimum runtime satisfied.
  if (report.healthy && verdict === 'ship' && report.minRuntimeSatisfied) {
    const actions = [act('evidence', t('exp.hero.viewEvidence'))];
    if (can('flag.publish')) actions.push(act('rollout', t('exp.zone.rollout')));
    return { kind: 'can_advance',
      params: { primary: report.primary || null, guardrails: report.guardrails || [], verdict },
      actions };
  }
  // Defensive: no rule matched — render no banner rather than invent a state;
  // the decision snapshot shows the raw server verdict.
  return { kind: 'unknown', params: { verdict }, actions: [] };
}

// renderExpHero renders the classified first-screen copy. The error /
// restricted / loading branches keep the pre-C5 markup and semantics exactly
// (a failed refresh never presents an older report as a current conclusion).
function renderExpHero(cls, report, error = null) {
  const kind = (cls && cls.kind) || 'unknown';
  const p = (cls && cls.params) || {};
  const actions = ((cls && cls.actions) || []).map(a =>
    '<button class="btn sm" type="button" data-hero-goto="' + esc(a.goto) + '">' + esc(a.label) + '</button>').join('');
  const actionRow = actions ? '<div class="row actions" id="exp-hero-actions">' + actions + '</div>' : '';
  if (kind === 'error') {
    return '<div class="banner warn">' + esc(t('exp.reportUnavailable')) + '</div>' +
      (report ? '<p class="muted">' + esc(t('report.stale')) + ' · ' + esc(t('report.generatedAt')) + ': ' +
        esc(fmtTime(report.generatedAt)) + '</p>' : '');
  }
  if (kind === 'restricted') return '<div class="banner">' + esc(t('exp.reportRestricted')) + '</div>';
  if (kind === 'loading') return '<div class="banner">' + esc(t('exp.reportLoading')) + '</div>';
  if (kind === 'not_started') {
    return '<div class="banner" id="exp-hero-banner"><strong>' + esc(t('exp.hero.notStarted')) + '</strong>' +
      '<div>' + esc(t('exp.hero.notStartedNext')) + '</div></div>' + actionRow;
  }
  if (kind === 'no_samples') {
    const detail = p.subBranch === 'assigned'
      ? t('exp.hero.noSamplesAssigned', { assigned: p.assigned })
      : p.subBranch === 'none' ? t('exp.hero.noSamplesNone') : '';
    return '<div class="banner" id="exp-hero-banner"><strong>' + esc(t('exp.hero.noSamples')) + '</strong>' +
      (detail ? '<div>' + esc(detail) + '</div>' : '') + '</div>' + actionRow;
  }
  if (kind === 'immature') {
    const eta = p.eta
      ? t('exp.hero.immatureEta', { time: fmtTime(p.eta) })
      : t('exp.hero.immatureEtaUnknown');
    return '<div class="banner" id="exp-hero-banner"><strong>' + esc(t('exp.hero.immature')) + '</strong>' +
      '<div>' + esc(eta) + '</div>' +
      ((p.reasons || []).length
        ? '<ul class="muted">' + p.reasons.map(r => '<li>' + esc(r) + '</li>').join('') + '</ul>' : '') +
      '</div>' + actionRow;
  }
  if (kind === 'blocked') {
    const scope = t('exp.hero.blockedScope', { flag: p.scope.flag, env: p.scope.env, epoch: p.scope.epoch });
    return '<div class="banner err" id="exp-hero-banner"><strong>' + esc(t('exp.hero.blocked')) + '</strong>' +
      ((p.reasons || []).length
        ? '<ul>' + p.reasons.map(r => '<li>' + esc(r) + '</li>').join('') + '</ul>' : '') +
      ((p.breaches || []).length
        ? '<div>' + esc(t('report.breaches')) + '<ul>' + p.breaches.map(gb =>
          '<li><span class="mono">' + esc(gb.metric) + '</span> — ' + esc(gb.detail || '') + '</li>').join('') + '</ul></div>' : '') +
      '<div class="muted">' + esc(scope) + '</div></div>' + actionRow;
  }
  if (kind === 'can_advance') {
    const primary = p.primary;
    const guards = p.guardrails || [];
    return '<div class="banner ok" id="exp-hero-banner"><strong>' + esc(t('exp.hero.canAdvance')) + '</strong>' +
      (primary
        ? '<div class="mono">p = ' + esc(primary.pValue ?? '—') + ' · diff = ' + esc(primary.diff ?? '—') +
          ' · rel = ' + esc(primary.relativeDiff ?? '—') + ' · CI [' + esc(primary.ciLow ?? '—') + ', ' +
          esc(primary.ciHigh ?? '—') + ']</div>'
        : '') +
      (guards.length
        ? '<div>' + guards.map(g =>
          chip(g.breached ? t('chip.breached') : t('chip.ok'), g.breached ? 'red' : 'green') +
          ' <span class="mono">' + esc(g.metric ?? g.Metric ?? '—') + '</span>').join(' ') + '</div>'
        : '') +
      '<div class="muted">' + esc(t('exp.hero.canAdvanceNote')) + '</div></div>' + actionRow;
  }
  if (kind === 'ended') {
    const tr = p.transition;
    return '<div class="banner" id="exp-hero-banner"><strong>' + esc(tr
      ? t('exp.hero.ended', { state: p.state, actor: tr.actor || '—', time: fmtTime(tr.time), reason: tr.reason || '—' })
      : t('exp.hero.endedUnknown')) + '</strong>' +
      (p.rollbackTarget ? '<div>' + esc(t('exp.hero.endedRollback', { target: p.rollbackTarget })) + '</div>' : '') +
      '</div>' + actionRow;
  }
  return '';
}

// renderExpDecisionHero keeps the pre-C5 signature for the contract bridge:
// it classifies from (report, state, error) alone — no diagnostics payload,
// no transition audit — and renders the same hero markup.
function renderExpDecisionHero(report, state, error = null) {
  const cls = classifyExpStatus({ state }, report, undefined, undefined,
    { error, canRead: can('report.read') });
  return renderExpHero(cls, report, error);
}

function metricReferenceSummary(r) {
  const ref = r.primaryMetricRef;
  return '<div class="muted">' + esc(t('report.metricVersion')) + ': ' +
    (ref ? '<span class="mono">' + esc(ref.key) + ' v' + esc(ref.version) + ' · ' + esc(shortHash(ref.definitionHash)) + '</span>' :
      esc(t('report.legacyInline'))) +
    (r.primaryAttributionWindow ? ' · ' + esc(t('report.attributionWindow')) + ': ' + esc(fmtDurNs(r.primaryAttributionWindow)) : '') +
    '</div>';
}

// renderHealthZone renders the data-health zone — the former standalone
// diagnostics view merged into the experiment detail (§4.4). Pure renderer
// over the GET {id}/diagnostics payload plus the shared report (may be null:
// the report card then degrades to exp.zoneEmpty.report, never an empty
// table). Contents preserved: watermark/backend/complete, blocking issues,
// sample flow, the two SRM layers, contamination and rejected events.
function renderHealthZone(e, d, report) {
  const window = d.window || {};
  const blockers = [...new Set([...(window.issues || []), ...(d.issues || [])])];
  const totalAssignments = (d.perVariant || []).reduce((sum, v) => sum + (v.assignments || 0), 0);
  const totalExposures = (d.perVariant || []).reduce((sum, v) => sum + (v.exposures || 0), 0);
  return '<div class="view-head"><h2>' + esc(t('exp.diagnosticsTitle')) + '</h2>' +
    '<span class="mono">' + esc(e.id) + '</span>' + chip(e.state, EXP_CHIP[e.state] || 'gray') +
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
    '<section class="card" id="diag-report-card">' + renderHealthReportCard(report) + '</section>' +
    '<section class="card">' + renderDiagnostics(d) + '</section>';
}

// renderHealthReportCard summarizes report.healthIssues and the guardrail
// states inside the health zone. Without a loaded report it shows the
// exp.zoneEmpty.report copy — no invented verdict, no fabricated numbers.
function renderHealthReportCard(report) {
  if (!report) return '<h3>' + esc(t('exp.report')) + '</h3>' +
    '<p class="muted">' + esc(t('exp.zoneEmpty.report')) + '</p>';
  const issues = (report.healthIssues || []).length
    ? '<ul>' + report.healthIssues.map(issue => '<li>' + esc(issue) + '</li>').join('') + '</ul>'
    : '';
  return '<h3>' + esc(t('exp.report')) + '</h3>' +
    (report.healthy
      ? '<p class="muted">' + esc(t('report.healthy', { epoch: report.epoch, state: report.state })) + '</p>' + issues
      : '<div class="banner warn"><strong>' + esc(t('report.unhealthy')) + '</strong>' + issues + '</div>') +
    '<h4>' + esc(t('exp.guardrails')) + '</h4>' +
    ((report.guardrails || []).length
      ? '<ul class="plain">' + report.guardrails.map(g =>
        '<li>' + chip(g.breached ? t('chip.breached') : t('chip.ok'), g.breached ? 'red' : 'green') +
        ' <span class="mono">' + esc(g.Metric ?? g.metric ?? '—') + '</span> <span class="muted">' + esc(g.Detail ?? g.detail ?? '') + '</span></li>').join('') + '</ul>'
      : '<div class="muted">' + esc(t('misc.none')) + '</div>');
}

function renderDiagnostics(d) {
  if (!d) return '';
  const flow = d.perVariant || [];
  const count = field => flow.reduce((sum, variant) => sum + Number(variant[field] || 0), 0);
  const assigned = count('assignments');
  const exposed = count('exposures');
  const converted = count('conversions');
  const unexposed = count('unexposed');
  const hasAssignmentFacts = d.exposureMode === 'onRender' || assigned > 0;
  const sampleFlow = '<ol class="sample-flow" aria-label="' + esc(t('report.sampleFlow')) + '">' +
    '<li class="sample-flow-stage"><span>' + esc(t('report.assignments')) + '</span><strong>' +
      esc(hasAssignmentFacts ? assigned : '—') + '</strong></li>' +
    '<li class="sample-flow-stage"><span>' + esc(t('report.exposures')) + '</span><strong>' + esc(exposed) + '</strong></li>' +
    '<li class="sample-flow-stage"><span>' + esc(t('report.collectedConverters')) + '</span><strong>' + esc(converted) + '</strong></li></ol>' +
    '<p class="muted">' + esc(t(hasAssignmentFacts ? 'report.sampleFlowOnRender' : 'report.sampleFlowOnDecision')) +
    (hasAssignmentFacts ? ' ' + esc(t('report.unexposed')) + ': ' + esc(unexposed) + '.' : '') + '</p>' +
    '<p class="muted">' + esc(t('report.sampleFlowScope')) + '</p>';
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
    sampleFlow +
    srmSummary +
    '<table class="grid diagnostic-table"><thead><tr><th>' + esc(t('th.variant')) + '</th><th>' +
      esc(t('report.assignments')) + '</th><th>' + esc(t('report.exposures')) +
      '</th><th>' + esc(t('report.unexposed')) + '</th><th>' +
      esc(t('th.converted')) + '</th></tr></thead><tbody>' +
      flow.map(v => '<tr><td class="mono">' + esc(v.variant) +
        '</td><td>' + esc(hasAssignmentFacts ? v.assignments : '—') + '</td><td>' + esc(v.exposures) +
        '</td><td>' + esc(hasAssignmentFacts ? v.unexposed : '—') + '</td><td>' + esc(v.conversions) + '</td></tr>').join('') +
      '</tbody></table>' +
    '<div class="diagnostic-cards">' + flow.map(v =>
      '<dl class="diagnostic-card"><dt class="mono">' + esc(v.variant) + '</dt><dd>' +
      '<span>' + esc(t('report.assignments')) + '</span><strong>' + esc(hasAssignmentFacts ? v.assignments : '—') + '</strong></dd><dd>' +
      '<span>' + esc(t('report.exposures')) + '</span><strong>' + esc(v.exposures) + '</strong></dd><dd>' +
      '<span>' + esc(t('report.unexposed')) + '</span><strong>' + esc(hasAssignmentFacts ? v.unexposed : '—') + '</strong></dd><dd>' +
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

function bindCheckNames() {
  return {
    state: t('exp.bindCheckState'),
    published_flag: t('exp.bindCheckPublished'),
    draft_clear: t('exp.bindCheckDraft'),
    registered_revision: t('exp.bindCheckRevision'),
    fallback: t('exp.bindCheckFallback'),
    unit: t('exp.bindCheckUnit'),
    split: t('exp.bindCheckSplit'),
    bandit_rule: t('exp.bindCheckBandit'),
    existing_binding: t('exp.bindCheckExisting'),
  };
}

function renderBindReadiness(readiness) {
  const names = bindCheckNames();
  const checks = Array.isArray(readiness.checks) ? readiness.checks : [];
  return '<div class="banner ' + (readiness.ready ? 'ok' : 'warn') + '">' +
    '<strong>' + esc(t(readiness.ready ? 'exp.bindReady' : 'exp.bindNotReady')) + '</strong>' +
    '<p class="muted">' + esc(t('exp.bindRevisionCompare', {
      registered: readiness.registeredRevision ?? '—', current: readiness.currentRevision || '—',
    })) + '</p>' +
    '<ul class="bind-check-list">' + checks.map(check =>
      '<li><span class="bind-check-state">' + esc(t(check.ready ? 'exp.bindCheckPass' : 'exp.bindCheckFail')) +
      '</span> ' + esc(names[check.code] || check.code) + '</li>').join('') + '</ul>' +
    '</div>';
}

function renderBindConflict(previous, latest) {
  const changed = (latest?.checks || []).filter(check => !check.ready &&
    (previous?.checks || []).some(before => before.code === check.code && before.ready));
  const names = bindCheckNames();
  return '<div class="banner warn" id="exp-bind-conflict" role="alert"><strong>' +
    esc(t('exp.bindConflictTitle')) + '</strong><p>' + esc(t(latest ? 'exp.bindConflictLoaded' : 'exp.bindConflictFailed')) + '</p>' +
    (latest ? '<p>' + esc(t('exp.bindConflictRevision', {
      before: previous?.currentRevision || '—', now: latest.currentRevision || '—',
    })) + '</p>' : '') +
    (changed.length ? '<ul>' + changed.map(check => '<li>' + esc(names[check.code] || check.code) + '</li>').join('') + '</ul>' : '') +
    '</div>';
}

function renderReport(box, r) {
  if (r.bandit) { renderBanditReport(box, r); return; }
  const p = r.primary, srm = r.srm;
  const window = r.window;
  box.innerHTML =
    renderReportSummary(r) +
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
      : '<div class="muted">' + esc(t('misc.none')) + '</div>');
}

function renderReportSummary(r) {
  const d = r.decision || {};
  const reasons = [...(r.healthIssues || []), ...(d.reasons || [])];
  const verdict = String(d.verdict || 'hold').toUpperCase();
  return '<section class="report-summary ' + (r.healthy ? '' : 'report-summary-blocked') + '" aria-label="' +
    esc(t('report.decisionSnapshot')) + '">' +
    '<div class="report-summary-head"><div><h4>' + esc(t('report.decisionSnapshot')) + '</h4>' +
    '<div class="muted">' + esc(t('report.generatedAt')) + ': <time title="' + esc(r.generatedAt || '') + '">' +
    esc(fmtTime(r.generatedAt)) + '</time> · ' + esc(t('report.watermark')) + ': <time title="' +
    esc(r.window?.watermark || '') + '">' + esc(fmtTime(r.window?.watermark)) + '</time></div></div>' +
    chip(verdict, r.healthy ? (VERDICT_CHIP[d.verdict] || 'gray') : 'amber') + '</div>' +
    '<p>' + esc(r.healthy ? t('report.healthy', { epoch: r.epoch, state: r.state }) : t('report.unhealthy')) + '</p>' +
    (reasons.length ? '<ul>' + reasons.map(reason => '<li>' + esc(reason) + '</li>').join('') + '</ul>' : '') +
    ((d.guardrailBreaches || []).length
      ? '<div class="banner warn">' + esc(t('report.breaches')) + '<ul>' + d.guardrailBreaches.map(gb =>
        '<li><span class="mono">' + esc(gb.metric) + '</span> — ' + esc(gb.detail || '') + '</li>').join('') + '</ul></div>' : '') +
    '</section>';
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
  const rows = steps.map((s, i) => {
    let status;
    if (plan.state === 'completed' || i < plan.currentStep) status = chip(t('chip.done'), 'green');
    else if (i === plan.currentStep) {
      status = plan.state === 'rolled_back' ? chip(t('chip.stoppedHere'), 'red')
        : chip(plan.state === 'paused' ? t('chip.heldHere') : t('chip.current'), plan.state === 'paused' ? 'amber' : 'blue');
    } else status = chip(t('chip.pending'), 'gray');
    const advance = chip(s.autoAdvance ? t('chip.auto') : t('chip.manualHold'), s.autoAdvance ? 'green' : 'amber');
    return { step: s, index: i, status, advance };
  });
  return '<table class="grid mobile-card-table" id="ro-steps-tbl"><thead><tr><th>#</th><th>' + esc(t('th.step')) + '</th><th>' + esc(t('exp.eligShort')) + '</th>' +
    '<th>' + esc(t('exp.minRuntime')) + '</th><th>' + esc(t('ro.obsWindow')) + '</th><th>' + esc(t('ro.advance')) + '</th><th>' + esc(t('th.status')) + '</th></tr></thead><tbody>' +
    (rows.map(({ step, index, status, advance }) => {
      return '<tr' + (index === plan.currentStep ? ' class="ro-current"' : '') + '><td>' + index +
        '</td><td class="mono">' + esc(step.name) + '</td><td>' + esc(step.ratio) +
        '%</td><td>' + esc(fmtDurNs(step.minRuntime)) + '</td><td>' + esc(fmtDurNs(step.observationWindow)) +
        '</td><td>' + advance + '</td><td>' + status + '</td></tr>';
    }).join('') || '<tr><td colspan="7" class="muted">' + esc(t('ro.noSteps')) + '</td></tr>') +
    '</tbody></table><div class="mobile-record-cards">' + rows.map(({ step, index, status, advance }) =>
      '<article class="mobile-record"><div class="row"><strong class="mono">' + esc(index + 1) + '. ' + esc(step.name) +
      '</strong>' + status + '</div><div><b>' + esc(t('exp.eligShort')) + ':</b> ' + esc(step.ratio) +
      '% · <b>' + esc(t('ro.advance')) + ':</b> ' + advance + '</div><div class="muted">' +
      esc(t('exp.minRuntime')) + ': ' + esc(fmtDurNs(step.minRuntime)) + ' · ' + esc(t('ro.obsWindow')) +
      ': ' + esc(fmtDurNs(step.observationWindow)) + '</div></article>').join('') +
    (rows.length ? '' : '<p class="muted">' + esc(t('ro.noSteps')) + '</p>') + '</div>' +
    '<div class="muted">' + esc(t('ro.gateNote')) + '</div>';
}

function renderRolloutHistory(plan) {
  const hist = (plan.history || []).slice().reverse(); // newest first
  return '<table class="grid mobile-card-table" id="ro-history-tbl"><thead><tr><th>' + esc(t('th.time')) + '</th><th>' + esc(t('th.actor')) + '</th><th>' + esc(t('th.action')) + '</th>' +
    '<th>' + esc(t('th.step')) + '</th><th>' + esc(t('th.detailSkip')) + '</th></tr></thead><tbody>' +
    (hist.map(ev =>
      '<tr><td class="nowrap">' + esc(fmtTime(ev.at)) + '</td><td>' + esc(ev.actor || '—') + '</td><td>' +
      chip(ev.action, PLAN_ACTION_CHIP[ev.action] || 'gray') + '</td><td>' + esc(ev.step ?? '—') +
      '</td><td>' + esc(ev.detail || '—') + '</td></tr>').join('') ||
      '<tr><td colspan="5" class="muted">' + esc(t('ro.noActions')) + '</td></tr>') +
    '</tbody></table><div class="mobile-record-cards">' + hist.map(ev =>
      '<article class="mobile-record"><div class="row">' + chip(ev.action, PLAN_ACTION_CHIP[ev.action] || 'gray') +
      '<strong>' + esc(t('th.step')) + ' ' + esc(ev.step ?? '—') + '</strong></div><div class="muted">' +
      esc(fmtTime(ev.at)) + ' · ' + esc(ev.actor || '—') + '</div><div>' + esc(ev.detail || '—') +
      '</div></article>').join('') + (hist.length ? '' : '<p class="muted">' + esc(t('ro.noActions')) + '</p>') + '</div>';
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
    '<div class="row"><button class="btn sm" id="ro-open-audit" type="button">' + esc(t('ro.openAudit')) + '</button></div>' +
    '<div id="ro-report-gate" role="status"></div>' +
    '<div class="row actions">' +
      '<button class="btn sm" id="ro-refresh">' + esc(t('btn.refresh')) + '</button>' +
      (plan.state === 'armed' || plan.state === 'running' ? '<button class="btn danger" id="ro-pause">' + esc(t('btn.pause')) + '</button>' : '') +
      (plan.state === 'paused' ? '<button class="btn primary" id="ro-resume">' + esc(t('btn.resume')) + '</button>' : '') +
      (plan.state === 'running' ? '<button class="btn danger" id="ro-advance">' + esc(t('btn.advance')) + '</button>' : '') +
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

function setRolloutReportStatus(m, status) {
  m.dataset.rolloutReportStatus = status;
  const gate = $('#ro-report-gate', m);
  if (gate) {
    const message = status === 'ready' || status === 'not_required' ? '' : status === 'restricted' ? 'ro.reportRestricted' :
      status === 'unavailable' ? 'ro.reportUnavailable' : 'ro.reportPending';
    gate.innerHTML = message ? '<div class="banner warn">' + esc(t(message)) + '</div>' : '';
  }
  for (const selector of ['#ro-resume', '#ro-advance', '#ro-takeover-off']) {
    const button = $(selector, m);
    if (button) button.disabled = !['ready', 'not_required'].includes(status) || !can('flag.publish');
  }
}

// bindRollout wires the rendered rollout card. State-conditional buttons are
// guarded (they exist only in the states where the action is legal server-side).
function bindRollout(m, expId, exp = null) {
  const base = '/api/v1/admin/experiments/' + encodeURIComponent(expId) + '/release-plan';
  const plan = exp?.releasePlan;
  const openAudit = $('#ro-open-audit', m);
  if (openAudit) openAudit.onclick = () => {
    S.view = 'audit'; S.auditObject = expId; S.auditQuery = '';
    S.auditCursors = [0]; S.auditPage = 0;
    route();
  };
  const checkReport = async (notify = true) => {
    try {
      const report = await api('/api/v1/admin/experiments/' + encodeURIComponent(expId) + '/report');
      if (!report?.evidenceVersion) throw new Error(t('ro.reportVersionMissing'));
      setRolloutReportStatus(m, 'ready');
      return report;
    } catch (err) {
      setRolloutReportStatus(m, err.status === 403 ? 'restricted' : 'unavailable');
      if (notify) reportError(err);
      return null;
    }
  };
  const decide = async ({ title, summary, reasonLabel = '', reasonRequired = true,
    confirmLabel, path, body, success, requireReport = false }) => {
    if (!confirmLeaveEditor()) return;
    const reportRequired = requireReport && !['draft', 'reviewed', 'scheduled'].includes(exp?.state);
    let openedReport = reportRequired ? await checkReport() : null;
    if (reportRequired && !openedReport) return;
    let latest;
    try { latest = await api('/api/v1/admin/experiments/' + encodeURIComponent(expId)); }
    catch (e) { reportError(e); return; }
    const current = latest?.releasePlan;
    if (!current || (plan && (current.state !== plan.state || current.currentStep !== plan.currentStep ||
      current.manualOverride !== plan.manualOverride || current.stepStartedAt !== plan.stepStartedAt))) {
      toast(t('ro.planChanged'), false);
      route();
      return;
    }
    const step = current.steps?.[current.currentStep];
    const scope = t('ro.actionScope', { id: expId, env: S.env || '—',
      step: step?.name || '—', ratio: step?.ratio ?? '—' });
    const reportEvidence = report => renderReportSummary(report) +
      '<p class="muted"><strong>' + esc(t('ro.reportEvidenceVersion')) + ':</strong> ' +
      '<span class="mono">' + esc(report.evidenceVersion) + '</span></p>';
    const preview = openedReport ? '<div id="ro-report-evidence">' + reportEvidence(openedReport) + '</div>' : '';
    const accepted = await actionDialog({ title, summary: scope + ' ' + summary,
      preview, reasonLabel, reasonRequired, confirmLabel, onConfirm: async ({ reason }) => {
        if (reportRequired) {
          const currentReport = await checkReport(false);
          if (!currentReport) throw new ApiError(0, 'report_unavailable', t('ro.reportUnavailable'));
          if (currentReport.evidenceVersion !== openedReport.evidenceVersion) {
            openedReport = currentReport;
            const evidence = $('#ro-report-evidence');
            if (evidence) evidence.innerHTML = reportEvidence(currentReport);
            throw new ApiError(0, 'report_changed', t('ro.reportChanged'));
          }
        }
        const options = { method: 'POST' };
        if (body || reportRequired) {
          options.body = body ? body(reason) : {};
          if (reportRequired) options.body.expectedReportVersion = openedReport.evidenceVersion;
        }
        try { await api(base + path, options); }
        catch (err) {
          if (err.status === 409 && reportRequired) {
            await checkReport(false);
          }
          throw err;
        }
        toast(t(success));
      } });
    if (accepted) route();
  };

  const refresh = $('#ro-refresh', m);
  if (refresh) refresh.onclick = async () => {
    const box = $('#rollout-box', m);
    try {
      const e2 = await api('/api/v1/admin/experiments/' + encodeURIComponent(expId));
      if (!box) return;
      box.innerHTML = renderRollout(e2);
      bindRollout(m, expId, e2);
      setRolloutReportStatus(m, m.dataset.rolloutReportStatus || 'pending');
    } catch (err) { if (box) box.innerHTML = errBanner(err); }
  };

  const pause = $('#ro-pause', m);
  if (pause) pause.onclick = () => decide({ title: t('btn.pause'), summary: t('confirm.pause'),
    reasonLabel: t('prompt.pauseReason'), confirmLabel: t('btn.pause'),
    path: '/pause', body: reason => ({ reason }), success: 'toast.planPaused' });

  const resume = $('#ro-resume', m);
  if (resume) resume.onclick = () => decide({ title: t('btn.resume'), summary: t('confirm.resume'),
    confirmLabel: t('btn.resume'), path: '/resume', success: 'toast.planResumed', requireReport: true });

  const adv = $('#ro-advance', m);
  if (adv) adv.onclick = () => decide({ title: t('btn.advance'), summary: t('confirm.advance'),
    reasonLabel: t('prompt.advanceReason'), reasonRequired: false, confirmLabel: t('btn.advance'),
    path: '/advance', body: reason => ({ reason }), success: 'toast.planAdvanced', requireReport: true });

  const tkOn = $('#ro-takeover-on', m);
  if (tkOn) tkOn.onclick = () => decide({ title: t('btn.takeoverOn'), summary: t('confirm.takeoverOn'),
    reasonLabel: t('prompt.takeoverReason'), confirmLabel: t('btn.takeoverOn'),
    path: '/takeover', body: reason => ({ enabled: true, reason }), success: 'toast.takeoverOn' });

  const tkOff = $('#ro-takeover-off', m);
  if (tkOff) tkOff.onclick = () => decide({ title: t('btn.takeoverOff'), summary: t('confirm.takeoverOff'),
    reasonLabel: t('prompt.reasonOptional'), reasonRequired: false, confirmLabel: t('btn.takeoverOff'),
    path: '/takeover', body: reason => ({ enabled: false, reason }), success: 'toast.takeoverOff', requireReport: true });

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
