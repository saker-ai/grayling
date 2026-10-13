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
  return { valid: () => {
    if (advanced.classList.contains('hidden') && !sync()) { toast(issues[0], false); return false; }
    return true;
  } };
}
