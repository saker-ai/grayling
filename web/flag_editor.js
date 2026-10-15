'use strict';

// The structured editor and JSON mode share #flag-json as their single
// unsaved source of truth. A failed form parse never replaces that source.
function flagEditorValue(value) {
  return JSON.stringify(value);
}

function renderFlagRule(rule, index, variationNames) {
  const isDefault = index < 0;
  const mode = rule.percentage ? 'percentage' : 'variation';
  const options = variationNames.map(name => '<option value="' + esc(name) + '"' +
    (rule.variation === name ? ' selected' : '') + '>' + esc(name) + '</option>').join('');
  return '<fieldset class="flag-rule" data-rule-index="' + index + '"><legend>' +
    esc(isDefault ? t('flags.editorDefault') : t('flags.editorTargeting', { n: index + 1 })) + '</legend>' +
    (isDefault ? '' : '<div class="row"><label class="field">' + esc(t('flags.editorRuleName')) +
      '<input data-rule-name value="' + esc(rule.name || '') + '"></label>' +
      '<label class="inline"><input type="checkbox" data-rule-disabled' + (rule.disable ? ' checked' : '') +
      '> ' + esc(t('flags.editorRuleDisabled')) + '</label>' +
      '<button type="button" class="btn sm" data-remove-rule="' + index + '">' + esc(t('flags.editorRemove')) + '</button></div>') +
    '<div class="row"><label class="field">' + esc(t('flags.editorRuleMode')) + '<select data-rule-mode>' +
      '<option value="variation"' + (mode === 'variation' ? ' selected' : '') + '>' + esc(t('flags.editorFixed')) + '</option>' +
      '<option value="percentage"' + (mode === 'percentage' ? ' selected' : '') + '>' + esc(t('flags.editorPercentage')) + '</option></select></label>' +
      (mode === 'variation' ? '<label class="field">' + esc(t('flags.editorServedVariation')) +
        '<select data-rule-variation>' + options + '</select></label>' :
        '<div class="flag-split">' + variationNames.map(name =>
          '<label class="field">' + esc(name) + ' %<input type="number" min="0" max="100" step="0.01" data-split="' +
          esc(name) + '" value="' + esc(rule.percentage?.[name] ?? 0) + '"></label>').join('') + '</div>') + '</div>' +
    '<div class="row"><label class="field grow">' + esc(t('flags.editorSegments')) +
      '<input data-rule-segments value="' + esc((rule.segments || []).join(', ')) + '"></label></div>' +
    '<label class="field">' + esc(t('flags.editorConstraints')) +
      '<textarea class="json" rows="3" data-rule-constraints spellcheck="false">' +
      esc(JSON.stringify(rule.constraints || [], null, 2)) + '</textarea></label></fieldset>';
}

function renderFlagStructured(content) {
  const names = Object.keys(content.variations || {});
  const bucketing = content.bucketing || {};
  return '<div class="flag-structured"><div class="flag-fields">' +
    '<label class="field">' + esc(t('flags.editorType')) + '<select id="flag-type">' +
      ['bool', 'string', 'number', 'json'].map(type => '<option value="' + type + '"' +
        (content.type === type ? ' selected' : '') + '>' + type + '</option>').join('') + '</select></label>' +
    '<label class="inline"><input type="checkbox" id="flag-disable"' + (content.disable ? ' checked' : '') +
      '> ' + esc(t('flags.editorDisable')) + '</label>' +
    '<label class="field">' + esc(t('flags.editorUnit')) +
      '<input id="flag-unit" value="' + esc(bucketing.unit || '') + '"></label>' +
    '<label class="field">' + esc(t('flags.editorSalt')) +
      '<input id="flag-salt" value="' + esc(bucketing.salt || '') + '"></label>' +
    '<label class="field flag-wide">' + esc(t('flags.editorFields')) +
      '<input id="flag-fields" value="' + esc((content.fields || []).join(', ')) + '"></label></div>' +
    '<h4>' + esc(t('flags.editorVariations')) + '</h4><div class="flag-variations">' +
    names.map(name => '<div class="flag-variation" data-variation-row data-original="' + esc(name) + '">' +
      '<label class="field">' + esc(t('flags.editorVariationName')) +
      '<input data-var-name value="' + esc(name) + '"></label>' +
      '<label class="field grow">' + esc(t('flags.editorVariationValue')) +
      '<input class="mono" data-var-value value="' + esc(flagEditorValue(content.variations[name])) + '"></label>' +
      '<button type="button" class="btn sm" data-remove-variation="' + esc(name) + '"' +
      (names.length === 1 ? ' disabled' : '') + '>' +
      esc(t('flags.editorRemove')) + '</button></div>').join('') + '</div>' +
    '<button type="button" class="btn sm" id="flag-add-variation">' + esc(t('flags.editorAddVariation')) + '</button>' +
    '<h4>' + esc(t('flags.editorRules')) + '</h4>' +
    renderFlagRule(content.defaultRule || {}, -1, names) +
    (content.targeting || []).map((rule, index) => renderFlagRule(rule, index, names)).join('') +
    '<button type="button" class="btn sm" id="flag-add-rule">' + esc(t('flags.editorAddRule')) + '</button>' +
    (content.experiment ? '<p class="muted">' + esc(t('flags.editorManagedExperiment')) + '</p>' : '') +
    '<div id="flag-structured-status" role="status" aria-live="polite"></div></div>';
}

function readFlagStructured(root, current) {
  const next = JSON.parse(JSON.stringify(current));
  const issues = [];
  next.type = $('#flag-type', root).value;
  next.disable = $('#flag-disable', root).checked;
  const unit = $('#flag-unit', root).value.trim();
  const salt = $('#flag-salt', root).value.trim();
  if (next.bucketing || unit || salt) {
    next.bucketing = { ...next.bucketing, algorithm: next.bucketing?.algorithm || 'grayling-fnv1a-10000-v1',
      unit, salt };
  }
  next.fields = [...new Set($('#flag-fields', root).value.split(',').map(value => value.trim()).filter(Boolean))];
  const rename = {};
  const variations = {};
  $$('[data-variation-row]', root).forEach(row => {
    const name = $('[data-var-name]', row).value.trim();
    if (!name || Object.hasOwn(variations, name)) issues.push(t('flags.editorUniqueNames'));
    try {
      const value = JSON.parse($('[data-var-value]', row).value);
      const valid = next.type === 'json' ||
        (next.type === 'bool' && typeof value === 'boolean') ||
        (next.type === 'string' && typeof value === 'string') ||
        (next.type === 'number' && typeof value === 'number' && Number.isFinite(value));
      if (!valid) issues.push(t('flags.editorValueType', { name }));
      variations[name] = value;
    } catch { issues.push(t('flags.editorValueJson', { name })); }
    if (row.dataset.original !== name) rename[row.dataset.original] = name;
  });
  if (!Object.keys(variations).length) issues.push(t('flags.editorNeedVariation'));
  next.variations = variations;
  const remap = name => rename[name] || name;
  const readRule = (row, original) => {
    const rule = { ...original };
    if (row.dataset.ruleIndex !== '-1') {
      rule.name = $('[data-rule-name]', row).value.trim();
      rule.disable = $('[data-rule-disabled]', row).checked;
    }
    rule.segments = $('[data-rule-segments]', row).value.split(',').map(value => value.trim()).filter(Boolean);
    try {
      rule.constraints = JSON.parse($('[data-rule-constraints]', row).value);
      if (!Array.isArray(rule.constraints)) issues.push(t('flags.editorConstraintsArray'));
    } catch { issues.push(t('flags.editorConstraintsJson')); }
    if ($('[data-rule-mode]', row).value === 'percentage') {
      delete rule.variation;
      rule.percentage = {};
      $$('[data-split]', row).forEach(input => {
        const percent = Number(input.value);
        if (!input.value.trim() || !Number.isFinite(percent) || percent < 0 || percent > 100) {
          issues.push(t('flags.editorPercentRange'));
        }
        rule.percentage[remap(input.dataset.split)] = percent;
      });
    } else {
      delete rule.percentage;
      rule.variation = remap($('[data-rule-variation]', row).value);
    }
    return rule;
  };
  next.defaultRule = readRule($('[data-rule-index="-1"]', root), current.defaultRule || {});
  next.targeting = $$('[data-rule-index]', root).filter(row => row.dataset.ruleIndex !== '-1')
    .map((row, index) => readRule(row, current.targeting?.[index] || {}));
  return { content: next, issues: [...new Set([...issues, ...lintFlag(next)])] };
}

function bindFlagStructured(root) {
  const form = $('#flag-structured', root);
  const advanced = $('#flag-advanced', root);
  const json = $('#flag-json', root);
  let errors = [];
  let lastSyncedControls = '';
  const controlSnapshot = () => JSON.stringify($$('input, select, textarea', form)
    .map(control => [control.value, control.checked]));
  const status = () => {
    const box = $('#flag-structured-status', form);
    box.className = errors.length ? 'warn-text' : 'ok-text';
    box.textContent = errors.length ? errors.join(' · ') : t('validate.ok');
    $('#flag-save', root).disabled = errors.length > 0;
  };
  const redraw = () => {
    form.innerHTML = renderFlagStructured(JSON.parse(json.value));
    errors = [];
    lastSyncedControls = controlSnapshot();
    status();
  };
  const sync = () => {
    const snapshot = controlSnapshot();
    if (!errors.length && snapshot === lastSyncedControls) return true;
    const result = readFlagStructured(form, JSON.parse(json.value));
    errors = result.issues;
    status();
    if (!errors.length) {
      json.value = JSON.stringify(result.content, null, 2);
      lastSyncedControls = snapshot;
    }
    return !errors.length;
  };
  form.addEventListener('input', event => {
    if (event.target.matches('[data-var-name], [data-rule-mode]')) return;
    sync();
  });
  form.addEventListener('change', event => {
    if (event.target.matches('[data-var-name]')) {
      const row = event.target.closest('[data-variation-row]');
      const oldName = row.dataset.original;
      const newName = event.target.value.trim();
      if (sync()) {
        row.dataset.original = newName;
        $$('[data-split]', form).filter(input => input.dataset.split === oldName).forEach(input => {
          input.dataset.split = newName;
          input.parentElement.firstChild.textContent = newName + ' %';
        });
        $$('[data-rule-variation] option', form).filter(option => option.value === oldName).forEach(option => {
          option.value = newName;
          option.textContent = newName;
        });
        const remove = $('[data-remove-variation]', row);
        remove.dataset.removeVariation = newName;
      }
      return;
    }
    if (event.target.matches('[data-rule-mode]')) {
      const row = event.target.closest('[data-rule-index]');
      const index = Number(row.dataset.ruleIndex);
      const requestedMode = event.target.value;
      const content = JSON.parse(json.value);
      const rule = index < 0 ? content.defaultRule : content.targeting[index];
      event.target.value = rule.percentage ? 'percentage' : 'variation';
      if (!sync()) return;
      const updated = JSON.parse(json.value);
      const updatedRule = index < 0 ? updated.defaultRule : updated.targeting[index];
      if (requestedMode === 'percentage') {
        delete updatedRule.variation;
        updatedRule.percentage = Object.fromEntries(Object.keys(updated.variations).map(name =>
          [name, name === Object.keys(updated.variations)[0] ? 100 : 0]));
      } else {
        delete updatedRule.percentage;
        updatedRule.variation = Object.keys(updated.variations)[0] || '';
      }
      json.value = JSON.stringify(updated, null, 2);
      redraw();
      return;
    }
    sync();
  });
  form.addEventListener('click', event => {
    const removeVariation = event.target.closest('[data-remove-variation]');
    const removeRule = event.target.closest('[data-remove-rule]');
    if (!removeVariation && !removeRule && event.target.id !== 'flag-add-variation' &&
        event.target.id !== 'flag-add-rule') return;
    if (!sync()) return;
    const content = JSON.parse(json.value);
    if (removeVariation) {
      const removed = removeVariation.dataset.removeVariation;
      delete content.variations[removed];
      const first = Object.keys(content.variations)[0];
      if (!first) return;
      const repair = rule => {
        if (rule.variation === removed) rule.variation = first;
        if (!rule.percentage) return;
        delete rule.percentage[removed];
        const total = Object.values(rule.percentage).reduce((sum, value) => sum + value, 0);
        if (!total) rule.percentage[first] = 100;
        else for (const name of Object.keys(rule.percentage)) rule.percentage[name] = 100 * rule.percentage[name] / total;
      };
      repair(content.defaultRule);
      (content.targeting || []).forEach(repair);
    }
    if (removeRule) content.targeting.splice(Number(removeRule.dataset.removeRule), 1);
    if (event.target.id === 'flag-add-variation') {
      let index = 1;
      while (Object.hasOwn(content.variations, 'variation' + index)) index++;
      const value = { bool: false, string: '', number: 0, json: null }[content.type];
      content.variations['variation' + index] = value;
    }
    if (event.target.id === 'flag-add-rule') {
      content.targeting ||= [];
      content.targeting.push({ name: '', variation: Object.keys(content.variations)[0] || '' });
    }
    json.value = JSON.stringify(content, null, 2);
    redraw();
  });
  $('#flag-mode-structured', root).onclick = () => {
    if (!form.classList.contains('hidden')) return;
    let content;
    try { content = JSON.parse(json.value); }
    catch (error) { toast(t('toast.invalidJson', { msg: error.message }), false); return; }
    const issues = lintFlag(content);
    if (issues.length) { toast(issues.join(' · '), false); return; }
    form.classList.remove('hidden'); advanced.classList.add('hidden');
    $('#flag-mode-structured', root).setAttribute('aria-pressed', 'true');
    $('#flag-mode-json', root).setAttribute('aria-pressed', 'false');
    redraw();
  };
  $('#flag-mode-json', root).onclick = () => {
    if (!advanced.classList.contains('hidden')) return;
    if (!sync()) return;
    form.classList.add('hidden'); advanced.classList.remove('hidden');
    $('#flag-mode-structured', root).setAttribute('aria-pressed', 'false');
    $('#flag-mode-json', root).setAttribute('aria-pressed', 'true');
  };
  redraw();
  // redraw is handed out with valid() because callers can replace the JSON
  // source behind the structured form (the preset picker does exactly that):
  // they must re-render from the new value instead of leaving stale controls.
  return {
    valid: () => {
      if (!advanced.classList.contains('hidden')) return true;
      if (sync()) return true;
      toast(errors[0], false);
      return false;
    },
    redraw,
  };
}
