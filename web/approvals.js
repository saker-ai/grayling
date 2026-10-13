'use strict';

function approvalExpired(approval) {
  return approval.state === 'approved' && approval.expiresAt &&
    Date.parse(approval.expiresAt) <= Date.now();
}

function approvalAction(approval) {
  if (approval.state === 'submitted' && can('flag.approve') &&
      S.actorName && approval.submittedBy !== S.actorName) return 'review';
  if (approval.state === 'approved' && !approvalExpired(approval) &&
      approval.action !== 'breakglass.review' && can('flag.publish')) return 'execute';
  return '';
}

function approvalStateLabel(approval) {
  if (approvalExpired(approval)) return t('approval.expired');
  switch (approval.state) {
    case 'submitted': return t('approval.state.submitted');
    case 'approved': return t('approval.state.approved');
    case 'rejected': return t('approval.state.rejected');
    case 'used': return t('approval.state.used');
    default: return approval.state;
  }
}

function approvalChipClass(approval) {
  return approval.state === 'rejected' ? 'red' : DRAFT_CHIP[approval.state] || 'gray';
}

function renderApprovalList(approvals) {
  const pending = approvals.filter(approval => approvalAction(approval));
  const processed = approvals.filter(approval => approval.state !== 'submitted');
  const rows = S.approvalFilter === 'pending' ? pending :
    S.approvalFilter === 'processed' ? processed : approvals;
  return '<div class="view-head"><h2>' + esc(t('approval.inbox')) + '</h2>' +
    '<button class="btn sm" id="approval-refresh" type="button">' + esc(t('btn.refresh')) + '</button></div>' +
    '<div class="row flag-editor-modes" role="group" aria-label="' + esc(t('approval.filter')) + '">' +
    [['pending', t('approval.pending', { n: pending.length })],
      ['processed', t('approval.processed', { n: processed.length })],
      ['all', t('approval.all', { n: approvals.length })]].map(([key, label]) =>
      '<button type="button" class="btn sm" data-approval-filter="' + key + '" aria-pressed="' +
      (S.approvalFilter === key) + '">' + esc(label) + '</button>').join('') + '</div>' +
    (rows.length ? '<div class="approval-list">' + rows.map(approval =>
      '<article class="card approval-item"><div class="row"><button type="button" class="row-link grow" data-approval-id="' +
      esc(approval.id) + '">' + esc(approval.action) + ' · ' + esc(approval.object) + '</button>' +
      chip(approvalStateLabel(approval), approvalChipClass(approval)) + '</div>' +
      '<p class="muted">' + esc(approval.environmentId || t('misc.none')) + ' · ' +
      esc(approval.submittedBy) + ' · ' + esc(fmtTime(approval.submittedAt)) + '</p>' +
      '<p>' + esc(t('approval.risk')) + ': ' + esc(approval.risk || '—') + ' · ' +
      esc(t('approval.impact')) + ': ' + esc(approval.impact || '—') + '</p></article>').join('') + '</div>' :
      '<div class="banner info">' + esc(S.approvalFilter === 'pending' ?
        t('approval.noneForMe') : t('approval.none')) + '</div>');
}

function renderApprovalDetail(approval) {
  let payload = '';
  if (approval.payload) {
    try { payload = JSON.stringify(JSON.parse(approval.payload), null, 2); }
    catch { payload = approval.payload; }
  }
  const action = approvalAction(approval);
  return '<div class="view-head"><button type="button" class="btn sm" id="approval-back">' +
    esc(t('approval.back')) + '</button><h2>' + esc(approval.action) + '</h2>' +
    chip(approvalStateLabel(approval), approvalChipClass(approval)) + '</div>' +
    '<section class="card approval-detail"><h3>' + esc(t('approval.reviewObject')) + ': ' + esc(approval.object) + '</h3>' +
    '<dl class="approval-facts"><dt>' + esc(t('approval.environment')) + '</dt><dd>' + esc(approval.environmentId || '—') + '</dd>' +
    '<dt>' + esc(t('approval.risk')) + '</dt><dd>' + esc(approval.risk || '—') + '</dd>' +
    '<dt>' + esc(t('approval.base')) + '</dt><dd>r' + esc(approval.baseRevision ?? 0) + '</dd>' +
    '<dt>' + esc(t('approval.requester')) + '</dt><dd>' + esc(approval.submittedBy) + ' · ' +
    esc(fmtTime(approval.submittedAt)) + '</dd>' +
    '<dt>' + esc(t('approval.reason')) + '</dt><dd>' + esc(approval.reason || '—') + '</dd>' +
    '<dt>' + esc(t('approval.impact')) + '</dt><dd>' + esc(approval.impact || '—') + '</dd>' +
    '<dt>' + esc(t('approval.rollback')) + '</dt><dd>' + esc(approval.rollbackTarget || '—') + '</dd>' +
    '<dt>' + esc(t('approval.guardrails')) + '</dt><dd>' + esc(approval.guardrails || '—') + '</dd>' +
    '<dt>' + esc(t('approval.hash')) + '</dt><dd class="mono">' + esc(approval.payloadHash) + '</dd>' +
    '<dt>' + esc(t('approval.expires')) + '</dt><dd>' + esc(fmtTime(approval.expiresAt)) + '</dd>' +
    '<dt>' + esc(t('approval.reviewer')) + '</dt><dd>' + esc(approval.approvedBy || approval.rejectedBy || '—') + '</dd>' +
    (approval.rejectionReason ? '<dt>' + esc(t('approval.rejectionReason')) + '</dt><dd>' +
      esc(approval.rejectionReason) + '</dd>' : '') + '</dl>' +
    (payload ? '<h4>' + esc(t('approval.proposedPayload')) + '</h4><pre class="pre">' + esc(payload) + '</pre>' : '') +
    (approval.submittedBy === S.actorName && approval.state === 'submitted'
      ? '<div class="banner info">' + esc(t('approval.selfReview')) + '</div>' : '') +
    (approvalExpired(approval) ? '<div class="banner warn">' + esc(t('approval.expiredHelp')) + '</div>' : '') +
    (action === 'review' ? '<label class="field">' + esc(t('approval.reviewNote')) +
      '<textarea id="approval-note" rows="3"></textarea></label>' : '') +
    '<div id="approval-feedback" role="alert"></div><div class="row actions">' +
    (action === 'review' ? '<button type="button" class="btn primary" id="approval-review">' + esc(t('btn.approve')) +
      '</button><button type="button" class="btn danger" id="approval-reject">' + esc(t('approval.reject')) + '</button>' : '') +
    (action === 'execute' ? '<button type="button" class="btn primary" id="approval-execute">' + esc(t('approval.execute')) + '</button>' : '') +
    '</div></section>';
}

async function viewApprovals(root) {
  if (!needWs(root)) return;
  const approvals = await api(wsPath() + '/approvals') || [];
  if (!Array.isArray(approvals)) throw new ApiError(0, 'invalid_response', t('approval.invalidResponse'));
  if (!S.approvalID) {
    root.innerHTML = renderApprovalList(approvals);
    $('#approval-refresh', root).onclick = () => route();
    root.onclick = event => {
      const filter = event.target.closest('[data-approval-filter]');
      if (filter) { S.approvalFilter = filter.dataset.approvalFilter; route(); return; }
      const item = event.target.closest('[data-approval-id]');
      if (item) { S.approvalID = item.dataset.approvalId; route(); }
    };
    return;
  }
  const approval = approvals.find(item => item.id === S.approvalID);
  if (!approval) { root.innerHTML = errBanner(new ApiError(404, 'not_found', '')); return; }
  root.innerHTML = renderApprovalDetail(approval);
  $('#approval-back', root).onclick = () => { S.approvalID = null; route(); };
  const act = async kind => {
    const note = $('#approval-note', root)?.value.trim() || '';
    if ((kind === 'review' || kind === 'reject') && !note) {
      $('#approval-note', root).focus();
      $('#approval-feedback', root).textContent = t('approval.noteRequired');
      return;
    }
    const confirmation = await actionDialog({ title: kind === 'review' ? t('btn.approve') :
      kind === 'reject' ? t('approval.reject') : t('approval.execute'),
      summary: t(kind === 'review' ? 'approval.confirmReview' :
        kind === 'reject' ? 'approval.confirmReject' : 'approval.confirmExecute',
        { action: approval.action, object: approval.object }),
      preview: reviewPreview({ targetEnvironment: approval.environmentId, risk: approval.risk,
        baseRevision: approval.baseRevision, estimatedAffected: approval.impact,
        rollbackTarget: approval.rollbackTarget, guardrails: approval.guardrails,
        proposedHash: approval.payloadHash }, 'action-review-preview'),
      confirmLabel: kind === 'review' ? t('btn.approve') :
        kind === 'reject' ? t('approval.reject') : t('approval.execute') });
    if (!confirmation) return;
    const button = kind === 'review' ? $('#approval-review', root) :
      kind === 'reject' ? $('#approval-reject', root) : $('#approval-execute', root);
    button.disabled = true;
    try {
      const latest = (await api(wsPath() + '/approvals')).find(item => item.id === approval.id);
      if (!latest || latest.state !== approval.state || latest.payloadHash !== approval.payloadHash ||
          approvalExpired(latest)) {
        $('#approval-feedback', root).textContent = t('approval.changedBeforeAction');
        return;
      }
      if (kind === 'review') {
        await api(wsPath() + '/approvals/' + encodeURIComponent(approval.id) + '/approve',
          { method: 'POST', body: { note } });
      } else if (kind === 'reject') {
        await api(wsPath() + '/approvals/' + encodeURIComponent(approval.id) + '/reject',
          { method: 'POST', body: { reason: note } });
      } else await executeApproval(approval);
      toast(t(kind === 'review' ? 'approval.reviewed' :
        kind === 'reject' ? 'approval.rejected' : 'approval.executed'));
      route();
    } catch (error) {
      $('#approval-feedback', root).textContent = errMsg(error);
      if (error.status === 403) await refreshCapabilities();
    } finally { button.disabled = false; }
  };
  if ($('#approval-review', root)) $('#approval-review', root).onclick = () => act('review');
  if ($('#approval-reject', root)) $('#approval-reject', root).onclick = () => act('reject');
  if ($('#approval-execute', root)) $('#approval-execute', root).onclick = () => act('execute');
}
