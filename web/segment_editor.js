'use strict';

function editorUTF8Length(value) {
  return Array.from(value).reduce((length, character) => {
    const point = character.codePointAt(0);
    return length + (point < 0x80 ? 1 : point < 0x800 ? 2 : point < 0x10000 ? 3 : 4);
  }, 0);
}
function editorValidCIDR(value) {
  if (typeof value !== 'string') return false;
  const parts = value.split('/');
  if (parts.length !== 2 || !/^(0|[1-9]\d*)$(?![\s\S])/.test(parts[1])) return false;
  const ipv4 = address => {
    const octets = address.split('.');
    return octets.length === 4 && octets.every(n => /^(0|[1-9]\d{0,2})$(?![\s\S])/.test(n) && Number(n) <= 255);
  };
  const address = parts[0], prefix = Number(parts[1]);
  if (!address.includes(':')) return prefix <= 32 && ipv4(address);
  if (prefix > 128 || /:::|^:[^:]|[^:]:$(?![\s\S])/.test(address) || address.includes('%') || /[^0-9a-fA-F:.]/.test(address)) return false;
  const halves = address.split('::');
  if (halves.length > 2) return false;
  const groups = address.split(':').filter(Boolean);
  let count = 0;
  for (let i = 0; i < groups.length; i++) {
    if (groups[i].includes('.')) {
      if (i !== groups.length - 1 || !address.endsWith(groups[i]) || !ipv4(groups[i])) return false;
      count += 2;
    } else {
      if (!/^[0-9a-fA-F]{1,4}$(?![\s\S])/.test(groups[i])) return false;
      count++;
    }
  }
  return halves.length === 2 ? count < 8 : count === 8 && !address.startsWith(':') && !address.endsWith(':');
}
function editorValidSemver(value) {
  if (typeof value !== 'string') return false;
  const match = /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$(?![\s\S])/.exec(value);
  if (!match) return false;
  const max = 18446744073709551615n;
  if (match.slice(1, 4).some(part => BigInt(part) > max)) return false;
  return !match[4] || match[4].split('.').every(part => !/^\d+$(?![\s\S])/.test(part) ||
    (!/^0\d/.test(part) && BigInt(part) <= max));
}
function editorValidTime(value) {
  if (typeof value === 'number') return Number.isFinite(value);
  if (typeof value !== 'string') return false;
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,]\d+)?(Z|[+-]\d{2}:\d{2})$(?![\s\S])/.exec(value);
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const days = [31, y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return m >= 1 && m <= 12 && d >= 1 && d <= days[m - 1] && Number(hour) < 24 &&
    Number(minute) < 60 && Number(second) < 60 &&
    (zone === 'Z' || (Number(zone.slice(1, 3)) <= 24 && Number(zone.slice(4)) <= 60));
}
function editorUnsupportedRE2Pattern(pattern) {
  let characterClass = false;
  for (let index = 0; index < pattern.length; index++) {
    const character = pattern[index];
    if (character === '\\') {
      const escape = pattern[++index];
      if (!characterClass && (/[1-9]/.test(escape || '') || (escape === 'k' && pattern[index + 1] === '<'))) return true;
      continue;
    }
    if (character === '[') characterClass = true;
    if (character === ']') characterClass = false;
    if (!characterClass && character === '(' && /^\(\?(?:[=!]|<[=!])/.test(pattern.slice(index))) return true;
  }
  return false;
}
// Local syntax checks complement server validation; RE2 remains authoritative.
function lintConstraints(constraints, registered = null, maxCount = null) {
  if (constraints == null) return [];
  if (!Array.isArray(constraints)) return [t('flags.editorConstraintsArray')];
  const issues = [];
  if (maxCount !== null && constraints.length > maxCount) issues.push(t('editor.constraintLimit', {max: maxCount}));
  const fields = registered === null ? null : new Set(registered);
  const scalar = value => ['string', 'boolean'].includes(typeof value) ||
    (typeof value === 'number' && Number.isFinite(value));
  constraints.forEach((constraint, index) => {
    const n = index + 1;
    if (!constraint || typeof constraint !== 'object' || Array.isArray(constraint) ||
        typeof constraint.field !== 'string' || !constraint.field || !OPS.includes(constraint.op)) {
      issues.push(t('seg.editorBadConstraint', {n})); return;
    }
    if (fields && !fields.has(constraint.field)) issues.push(t('editor.unregisteredField', {field: constraint.field}));
    const {op, value, values} = constraint;
    if (['in', 'not_in'].includes(op)) {
      if (!Array.isArray(values) || !values.length || values.length > 512 || !values.every(scalar)) issues.push(t('editor.setLimit', {n}));
    } else if (op === 'regex') {
      if (typeof value !== 'string' || editorUTF8Length(value) > 512) issues.push(t('editor.regexLimit', {n}));
      else if (editorUnsupportedRE2Pattern(value)) issues.push(t('seg.editorBadRegex', {n}));
      else {
        // Go supports inline/scoped flags and quoted literals unavailable in JS.
        // Leave those patterns to the backend instead of falsely rejecting them.
        if (/\(\?[imsU-]+[:)]|\\Q/.test(value)) return;
        try { new RegExp(value.replace(/\(\?P</g, '(?<')); }
        catch { issues.push(t('seg.editorBadRegex', {n})); }
      }
    } else if (op === 'in_cidr') {
      if (!editorValidCIDR(value)) issues.push(t('editor.invalidCIDR', {n}));
    } else if (op.startsWith('semver_')) {
      if (!editorValidSemver(value)) issues.push(t('editor.invalidSemver', {n}));
    } else if (['time_before', 'time_after'].includes(op)) {
      if (!editorValidTime(value)) issues.push(t('editor.invalidTime', {n}));
    } else if (['gt', 'gte', 'lt', 'lte'].includes(op)) {
      if (typeof value !== 'number' || !Number.isFinite(value)) issues.push(t('seg.editorNumberValue', {n}));
    } else if (!scalar(value)) issues.push(t('seg.editorScalarValue', {n}));
  });
  return [...new Set(issues)];
}
function lintSegmentReferences(references, key = '', maxCount = null) {
  if (references == null) return [];
  if (!Array.isArray(references)) return [t('seg.editorBadReference')];
  const issues = [];
  if (maxCount !== null && references.length > maxCount) issues.push(t('editor.referenceLimit', {max: maxCount}));
  if (references.some(ref => resourceKeyError(ref) || ref === key)) issues.push(t('seg.editorBadReference'));
  return issues;
}
function lintSegment(content) {
  if (!content || typeof content !== 'object' || Array.isArray(content)) return [t('lint.notObject')];
  const issues = editorMetadataIssues(content);
  if (content.schemaVersion !== 1) issues.push(t('editor.schemaVersion'));
  const keyIssue = resourceKeyError(content.key);
  if (keyIssue) issues.push(keyIssue);
  issues.push(...lintConstraints(content.constraints), ...lintSegmentReferences(content.segments, content.key));
  if (!(Array.isArray(content.constraints) && content.constraints.length) &&
      !(Array.isArray(content.segments) && content.segments.length)) issues.push(t('validate.segmentNonEmpty'));
  return [...new Set(issues)];
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
  return '<div class="segment-structured">' + editorMetadataMarkup(content, 'seg') + '<p class="muted">' + esc(t('seg.editorSemantics')) + '</p>' +
    '<h4>' + esc(t('seg.editorConditions')) + '</h4><div id="seg-conditions">' +
    editorConditionsMarkup(constraints) + '</div>' +
    '<label class="field">' + esc(t('seg.editorReferences')) +
    '<input id="seg-references" value="' + esc(segments.join(', ')) + '"></label>' +
    '<div id="seg-structured-status" role="status" aria-live="polite"></div></div>';
}

function readSegmentStructured(root, current) {
  const content = JSON.parse(JSON.stringify(current));
  const issues = [];
  editorReadMetadata(root, content, issues);
  content.constraints = editorReadConditions(root, current.constraints || [], issues);
  content.segments = [...new Set($('#seg-references', root).value.split(',').map(value => value.trim()).filter(Boolean))];
  return { content, issues: [...new Set(issues.length ? issues : lintSegment(content))] };
}

function bindSegmentStructured(root) {
  const form = $('#seg-structured', root);
  const advanced = $('#seg-advanced', root);
  const json = $('#seg-json', root);
  let issues = [];
  let lastControls = '';
  const snapshot = () => JSON.stringify($$('input, select, textarea', form).filter(control => !control.matches('[data-reference-search]')).map(control => control.value));
  const status = () => {
    const box = $('#seg-structured-status', form);
    editorShowIssues(box, issues);
    $('#seg-save', root).disabled = issues.length > 0;
  };
  const redraw = () => {
    const content = JSON.parse(json.value);
    form.innerHTML = renderSegmentStructured(content);
    editorBindReferences(form, root);
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
  form.addEventListener('input', event => { if (!event.target.matches('[data-condition-type]')) sync(); });
  form.addEventListener('change', event => { editorChangeConditionType(event); sync(); });
  form.addEventListener('click', event => {
    const remove = event.target.closest('[data-editor-remove-condition]');
    if (!remove && !event.target.closest('[data-editor-add-condition]')) return;
    if (remove) { remove.closest('[data-editor-condition]').remove(); if (sync()) redraw(); $('[data-editor-add-condition]', form)?.focus(); return; }
    if (snapshot() !== lastControls && !sync()) return;
    const content = JSON.parse(json.value);
    if (remove) content.constraints.splice(Number(remove.closest('[data-editor-condition]').dataset.editorCondition), 1);
    else content.constraints.push({ field: '', op: 'eq', value: '' });
    json.value = JSON.stringify(content, null, 2);
    redraw();
    (remove ? $('[data-editor-add-condition]', form) : $$('[data-condition-field]', form).at(-1))?.focus();
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
      if (advanced.classList.contains('hidden') && !sync()) { ($('[aria-invalid="true"]', form) || $('#seg-structured-status', form)).focus(); return false; }
      return true;
    },
    redraw,
  };
}
