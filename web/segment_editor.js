'use strict';

function lintSegment(content) {
  if (!content || typeof content !== 'object' || Array.isArray(content)) return [t('lint.notObject')];
  const issues = [];
  if (!Array.isArray(content.constraints) || !Array.isArray(content.segments)) {
    issues.push(t('seg.editorArrayShape'));
    return issues;
  }
  if (!content.constraints.length && !content.segments.length) issues.push(t('validate.segmentNonEmpty'));
  content.constraints.forEach((constraint, index) => {
    if (!constraint || typeof constraint !== 'object' || Array.isArray(constraint) || !constraint.field ||
        !OPS.includes(constraint.op)) {
      issues.push(t('seg.editorBadConstraint', { n: index + 1 }));
      return;
    }
    if (constraint.op === 'in' || constraint.op === 'not_in') {
      if (!Array.isArray(constraint.values) || !constraint.values.length ||
          constraint.values.some(value => value === null || typeof value === 'object')) {
        issues.push(t('seg.editorSetValues', { n: index + 1 }));
      }
    } else if (constraint.value === undefined || constraint.value === null ||
        typeof constraint.value === 'object') {
      issues.push(t('seg.editorScalarValue', { n: index + 1 }));
    } else if (constraint.op === 'regex') {
      if (typeof constraint.value !== 'string') issues.push(t('seg.editorBadRegex', { n: index + 1 }));
      else {
        try { new RegExp(constraint.value); }
        catch { issues.push(t('seg.editorBadRegex', { n: index + 1 })); }
      }
    } else if (['gt', 'gte', 'lt', 'lte'].includes(constraint.op) && typeof constraint.value !== 'number') {
      issues.push(t('seg.editorNumberValue', { n: index + 1 }));
    }
  });
  if (content.segments.some(name => typeof name !== 'string' || !name.trim() || name === content.key)) {
    issues.push(t('seg.editorBadReference'));
  }
  return issues;
}

function renderSegmentDependencies(data) {
  if (!data) return '<div class="banner warn">' + esc(t('seg.dependenciesUnavailable')) + '</div>';
  return '<section class="card"><h3>' + esc(t('seg.dependenciesTitle')) + '</h3>' +
    '<p class="muted">' + esc(t('seg.dependenciesScope', { n: data.segmentRevision || 0 })) + '</p>' +
    (data.flags?.length ? '<ul class="plain">' + data.flags.map(flag =>
      '<li><button class="row-link" type="button" data-dependent-flag="' + esc(flag.key) + '">' +
      esc(flag.key) + '</button> · r' + esc(flag.revision) + ' · ' +
      esc((flag.via || []).join(' → ')) + '</li>').join('') + '</ul>' :
      '<p class="muted">' + esc(t('seg.dependenciesEmpty')) + '</p>') + '</section>';
}

function renderSegmentHistory(data, compareRevision) {
  if (!data) return '<div class="banner warn">' + esc(t('seg.historyUnavailable')) + '</div>';
  const revisions = data.revisions || [];
  if (!revisions.length) return '<section class="card"><h3>' + esc(t('seg.historyTitle')) +
    '</h3><p class="muted">' + esc(t('seg.historyEmpty')) + '</p></section>';
  const current = revisions.find(item => item.revision === data.currentRevision);
  const previous = revisions.find(item => item.revision !== data.currentRevision);
  const selected = revisions.find(item => item.revision === compareRevision) || previous || current;
  const fields = ['schemaVersion', 'constraints', 'segments'];
  const differences = current && selected ? fields.filter(field =>
    JSON.stringify(current.content?.[field] ?? null) !== JSON.stringify(selected.content?.[field] ?? null)) : [];
  return '<section class="card segment-history"><h3>' + esc(t('seg.historyTitle')) + '</h3>' +
    '<p class="muted">' + esc(t('seg.historyScope', { n: data.currentRevision })) + '</p>' +
    '<label class="field">' + esc(t('seg.historyCompare')) + '<select id="seg-history-select">' +
    revisions.map(item => '<option value="' + esc(item.revision) + '"' +
      (item.revision === selected.revision ? ' selected' : '') + '>r' + esc(item.revision) +
      ' · ' + esc(fmtTime(item.publishedAt)) + ' · ' + esc(item.publishedBy) + '</option>').join('') +
    '</select></label>' +
    (current ? '<p>' + esc(t('seg.historyDiff', { a: selected.revision, b: current.revision })) + '</p>' +
      (differences.length ? differences.map(field =>
        '<div class="segment-history-field"><h4>' + esc({ schemaVersion: t('seg.historySchemaVersion'),
          constraints: t('seg.historyConstraints'), segments: t('seg.historyReferences') }[field]) + '</h4>' +
        '<div class="flag-diff-values"><div><strong>r' + esc(selected.revision) + '</strong><pre class="pre">' +
        esc(JSON.stringify(selected.content?.[field] ?? null, null, 2)) + '</pre></div>' +
        '<div><strong>r' + esc(current.revision) + '</strong><pre class="pre">' +
        esc(JSON.stringify(current.content?.[field] ?? null, null, 2)) + '</pre></div></div></div>').join('') :
        '<p class="muted">' + esc(t('seg.historySame')) + '</p>') :
      '<div class="banner warn">' + esc(t('seg.historyCurrentMissing')) + '</div>') +
    '<ul class="plain">' + revisions.map(item => '<li>r' + esc(item.revision) +
      (item.revision === data.currentRevision ? ' · ' + esc(t('seg.historyCurrent')) : '') +
      ' · ' + esc(fmtTime(item.publishedAt)) + ' · ' + esc(item.publishedBy) +
      ' · <span class="mono">' + esc(item.hash) + '</span></li>').join('') + '</ul></section>';
}

function renderSegmentMatchForm() {
  return '<section class="card segment-match"><h3>' + esc(t('seg.matchTitle')) + '</h3>' +
    '<p class="muted">' + esc(t('seg.matchHelp')) + '</p>' +
    '<label class="field">' + esc(t('seg.matchContext')) +
    '<textarea id="seg-match-context" class="json" rows="5" spellcheck="false">' +
    esc(JSON.stringify({ targetingKey: 'sample-user', attributes: {} }, null, 2)) + '</textarea></label>' +
    '<div class="row"><button type="button" class="btn sm" id="seg-match-run">' +
    esc(t('seg.matchRun')) + '</button></div><div id="seg-match-result" role="status" aria-live="polite"></div></section>';
}

function renderSegmentMatchResult(result) {
  const state = matched => t(matched ? 'seg.matchYes' : 'seg.matchNo');
  return '<div class="flag-diff-values"><div><strong>' + esc(t('seg.matchDraft')) + '</strong><p>' +
    esc(state(result.draftMatched)) + '</p></div><div><strong>' +
    esc(result.publishedMatched == null ? t('seg.matchNoPublished') :
      t('seg.matchPublished', { n: result.publishedRevision })) + '</strong><p>' +
    esc(result.publishedMatched == null ? '—' : state(result.publishedMatched)) + '</p></div></div>';
}

function parseSegmentPreviewContext(text) {
  const value = JSON.parse(text);
  if (!value || typeof value !== 'object' || Array.isArray(value) ||
      (value.targetingKey !== undefined && typeof value.targetingKey !== 'string') ||
      (value.attributes !== undefined && (!value.attributes || typeof value.attributes !== 'object' ||
        Array.isArray(value.attributes)))) throw new Error(t('seg.matchBadContext'));
  return value;
}

function renderSegmentStructured(content) {
  const constraints = Array.isArray(content.constraints) ? content.constraints : [];
  const segments = Array.isArray(content.segments) ? content.segments : [];
  return '<div class="segment-structured"><p class="muted">' + esc(t('seg.editorSemantics')) + '</p>' +
    '<h4>' + esc(t('seg.editorConditions')) + '</h4><div id="seg-conditions">' +
    constraints.map((constraint, index) => '<div class="segment-condition" data-condition="' + index + '">' +
      '<label class="field">' + esc(t('seg.editorField')) + '<input data-seg-field value="' + esc(constraint.field || '') + '"></label>' +
      '<label class="field">' + esc(t('seg.editorOperator')) + '<select data-seg-op>' +
        OPS.map(op => '<option value="' + op + '"' + (constraint.op === op ? ' selected' : '') + '>' + op + '</option>').join('') +
      '</select></label><label class="field grow">' + esc(t('seg.editorValue')) +
      '<input class="mono" data-seg-value value="' + esc(JSON.stringify(
        constraint.op === 'in' || constraint.op === 'not_in' ? constraint.values ?? [] : constraint.value ?? '')) + '"></label>' +
      '<button type="button" class="btn sm" data-remove-condition="' + index + '">' + esc(t('flags.editorRemove')) + '</button></div>').join('') +
    '</div><button type="button" class="btn sm" id="seg-add-condition">' + esc(t('seg.editorAddCondition')) + '</button>' +
    '<label class="field">' + esc(t('seg.editorReferences')) +
    '<input id="seg-references" value="' + esc(segments.join(', ')) + '"></label>' +
    '<div id="seg-structured-status" role="status" aria-live="polite"></div></div>';
}

function readSegmentStructured(root, current) {
  const content = JSON.parse(JSON.stringify(current));
  const issues = [];
  content.constraints = $$('[data-condition]', root).map(row => {
    const field = $('[data-seg-field]', row).value.trim();
    const op = $('[data-seg-op]', row).value;
    let value;
    try { value = JSON.parse($('[data-seg-value]', row).value); }
    catch { issues.push(t('seg.editorInvalidJson')); }
    return op === 'in' || op === 'not_in' ? { field, op, values: value } : { field, op, value };
  });
  content.segments = [...new Set($('#seg-references', root).value.split(',').map(value => value.trim()).filter(Boolean))];
  return { content, issues: [...new Set(issues.length ? issues : lintSegment(content))] };
}

function bindSegmentStructured(root) {
  const form = $('#seg-structured', root);
  const advanced = $('#seg-advanced', root);
  const json = $('#seg-json', root);
  let issues = [];
  let lastControls = '';
  const snapshot = () => JSON.stringify($$('input, select', form).map(control => control.value));
  const status = () => {
    const box = $('#seg-structured-status', form);
    box.className = issues.length ? 'warn-text' : 'ok-text';
    box.textContent = issues.length ? issues.join(' · ') : t('validate.ok');
    $('#seg-save', root).disabled = issues.length > 0;
  };
  const redraw = () => {
    const content = JSON.parse(json.value);
    form.innerHTML = renderSegmentStructured(content);
    issues = lintSegment(content);
    lastControls = snapshot();
    status();
  };
  const sync = () => {
    const controls = snapshot();
    if (!issues.length && controls === lastControls) return true;
    const result = readSegmentStructured(form, JSON.parse(json.value));
    issues = result.issues;
    status();
    if (!issues.length) {
      json.value = JSON.stringify(result.content, null, 2);
      lastControls = controls;
    }
    return !issues.length;
  };
  form.addEventListener('input', sync);
  form.addEventListener('change', sync);
  form.addEventListener('click', event => {
    const remove = event.target.closest('[data-remove-condition]');
    if (!remove && event.target.id !== 'seg-add-condition') return;
    if (snapshot() !== lastControls && !sync()) return;
    const content = JSON.parse(json.value);
    if (remove) content.constraints.splice(Number(remove.dataset.removeCondition), 1);
    else content.constraints.push({ field: '', op: 'eq', value: '' });
    json.value = JSON.stringify(content, null, 2);
    redraw();
  });
  $('#seg-mode-structured', root).onclick = () => {
    if (!form.classList.contains('hidden')) return;
    let content;
    try { content = JSON.parse(json.value); }
    catch (error) { toast(t('toast.invalidJson', { msg: error.message }), false); return; }
    const errors = lintSegment(content);
    if (errors.length) { toast(errors.join(' · '), false); return; }
    form.classList.remove('hidden'); advanced.classList.add('hidden');
    $('#seg-mode-structured', root).setAttribute('aria-pressed', 'true');
    $('#seg-mode-json', root).setAttribute('aria-pressed', 'false');
    redraw();
  };
  $('#seg-mode-json', root).onclick = () => {
    if (!advanced.classList.contains('hidden') || !sync()) return;
    form.classList.add('hidden'); advanced.classList.remove('hidden');
    $('#seg-mode-structured', root).setAttribute('aria-pressed', 'false');
    $('#seg-mode-json', root).setAttribute('aria-pressed', 'true');
  };
  redraw();
  // Same contract as bindFlagStructured: a caller that replaces #seg-json
  // (the preset picker) re-renders the structured form through this handle.
  return {
    valid: () => {
      if (advanced.classList.contains('hidden') && !sync()) { toast(issues[0], false); return false; }
      return true;
    },
    redraw,
  };
}
