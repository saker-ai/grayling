'use strict';

// The structured editor and JSON mode share #flag-json as their single
// unsaved source of truth. A failed form parse never replaces that source.
function flagEditorValue(value) {
  return JSON.stringify(value);
}

// Names are human-facing metadata; integrations continue to address the key.
function editorMetadataIssues(content) {
  return [['displayName', 128, 'editor.displayNameLimit'], ['description', 2048, 'editor.descriptionLimit']]
    .filter(([field, limit]) => content[field] !== undefined &&
      (typeof content[field] !== 'string' || Array.from(content[field]).length > limit))
    .map(([, , key]) => t(key));
}
function editorMetadataMarkup(content, prefix) {
  return '<div class="editor-metadata"><p class="muted" id="' + prefix + '-metadata-help">' +
    esc(t('editor.metadataHelp')) + '</p><label class="field">' + esc(t('editor.displayName')) +
    '<input data-editor-display-name aria-describedby="' + prefix + '-metadata-help" value="' +
    esc(typeof content.displayName === 'string' ? content.displayName : '') + '"></label>' +
    '<label class="field">' + esc(t('editor.description')) +
    '<textarea data-editor-description rows="3" aria-describedby="' + prefix + '-metadata-help">' +
    esc(typeof content.description === 'string' ? content.description : '') + '</textarea></label></div>';
}
function editorReadMetadata(root, content, issues) {
  for (const [field, selector, limit, key] of [
    ['displayName', '[data-editor-display-name]', 128, 'editor.displayNameLimit'],
    ['description', '[data-editor-description]', 2048, 'editor.descriptionLimit'],
  ]) {
    const input = $(selector, root);
    if (!input) continue;
    input.removeAttribute('aria-invalid');
    const value = input.value;
    if (Array.from(value).length > limit) {
      input.setAttribute('aria-invalid', 'true');
      issues.push(t(key));
    }
    // An untouched optional field must not change an existing document's hash.
    if (value) content[field] = value;
    else if (Object.hasOwn(content, field)) delete content[field];
  }
}

// Shared by Flag and Segment editors; unknown condition shapes stay in JSON.
function editorTypedValue(value, type, attr) {
  if (type === 'bool') {
    const valid = value === true || value === false;
    return '<select ' + attr + '>' + (!valid ? '<option value="" selected>' + esc(t('editor.chooseValue')) + '</option>' : '') +
      '<option value="true"' + (value === true ? ' selected' : '') + '>true</option>' +
      '<option value="false"' + (value === false ? ' selected' : '') + '>false</option></select>';
  }
  return '<input ' + attr + ' data-value-type="' + esc(type) + '"' +
    (type === 'number' ? ' inputmode="decimal"' : '') + ' value="' +
    esc(type === 'string' || type === 'number' ? String(value ?? '') : JSON.stringify(value)) + '">';
}
function editorReadValue(input, type) {
  input.removeAttribute('aria-invalid');
  input.removeAttribute('aria-describedby');
  try {
    if (type === 'string') return input.value;
    if (type === 'number') {
      if (!input.value.trim() || !Number.isFinite(Number(input.value))) throw new Error('number');
      return Number(input.value);
    }
    if (type === 'bool') {
      if (!['true', 'false'].includes(input.value)) throw new Error('boolean');
      return input.value === 'true';
    }
    return JSON.parse(input.value);
  } catch (error) { input.setAttribute('aria-invalid', 'true');
    input.setAttribute('aria-describedby', input.closest('#seg-structured') ? 'seg-structured-status' : 'flag-structured-status');
    throw error; }
}
function editorConditionSupported(c) {
  return c && typeof c === 'object' && !Array.isArray(c) && OPS.includes(c.op) &&
    (['in', 'not_in'].includes(c.op) ? Array.isArray(c.values) && c.values.every(v => typeof v === 'string' && !v.includes('\n')) : ['string', 'number', 'boolean'].includes(typeof c.value));
}
function editorConditionsMarkup(constraints) {
  return '<div data-editor-conditions>' + constraints.map((c, i) => {
    if (!editorConditionSupported(c)) return '<label class="field" data-editor-condition="' + i + '">' + esc(t('editor.advanced')) + '<textarea class="json" data-condition-json rows="3">' + esc(JSON.stringify(c, null, 2)) + '</textarea></label>';
    const list = ['in', 'not_in'].includes(c.op);
    const type = list ? 'list' : ({boolean: 'bool', number: 'number', string: 'string'}[typeof c.value]);
    return '<div class="segment-condition" data-editor-condition="' + i + '"><label class="field">' + esc(t('seg.editorField')) +
      '<input data-condition-field value="' + esc(c.field || '') + '"></label><label class="field">' + esc(t('seg.editorOperator')) +
      '<select data-condition-op data-seg-op>' + OPS.map(op => '<option' + (op === c.op ? ' selected' : '') + '>' + esc(op) +
      '</option>').join('') +
      '</select></label><label class="field">' + esc(t('editor.valueType')) +
      '<select data-condition-type>' + ['string', 'number', 'bool', 'list', 'json'].map(v => '<option value="' + v + '"' + (type === v ? ' selected' : '') + '>' + esc(t({string:'editor.text',number:'editor.number',bool:'editor.boolean',list:'editor.list',json:'editor.advanced'}[v])) +
      '</option>').join('') +
      '</select></label><label class="field grow">' + esc(t('seg.editorValue')) +
      (list ? '<textarea data-condition-value data-value-type="list" rows="3">' + esc(c.values.join('\n')) +
      '</textarea>' : editorTypedValue(c.value, type, 'data-condition-value')) +
      '</label><button class="btn sm" type="button" data-editor-remove-condition>' + esc(t('flags.editorRemove')) +
      '</button></div>';
  }).join('') + '</div><button class="btn sm" type="button" data-editor-add-condition>' + esc(t('editor.addCondition')) + '</button>';
}
function editorReadConditions(container, originals, issues) {
  return $$('[data-editor-condition]', container).map(row => {
    const original = originals[Number(row.dataset.editorCondition)];
    const raw = $('[data-condition-json]', row);
    try {
      if (raw) return editorReadValue(raw, 'json');
      const next = { ...original, field: $('[data-condition-field]', row).value.trim(), op: $('[data-condition-op]', row).value };
      const fieldControl = $('[data-condition-field]', row);
      fieldControl.removeAttribute('aria-invalid');
      if (!next.field) { fieldControl.setAttribute('aria-invalid', 'true'); issues.push(t('seg.editorBadConstraint', {n: Number(row.dataset.editorCondition) + 1})); }
      const input = $('[data-condition-value]', row), type = $('[data-condition-type]', row).value;
      const value = type === 'list' ? input.value.split('\n').filter(v => v.length) : editorReadValue(input, type);
      if (['in', 'not_in'].includes(next.op)) {
        if (!Array.isArray(value)) { input.setAttribute('aria-invalid', 'true'); throw new Error('list'); }
        delete next.value; next.values = value;
      } else { delete next.values; next.value = value; }
      const validation = lintConstraints([next]);
      if (validation.length) { input.setAttribute('aria-invalid', 'true'); issues.push(...validation); }
      return next;
    } catch { issues.push(t('editor.valueRequired')); return original; }
  });
}
function editorChangeConditionType(event) {
  if (event.target.matches('[data-condition-op]')) {
    const row = event.target.closest('[data-editor-condition]'), type = $('[data-condition-type]', row);
    if (['in', 'not_in'].includes(event.target.value) && !['list', 'json'].includes(type.value)) type.value = 'list';
    else if (!['in', 'not_in'].includes(event.target.value) && type.value === 'list') type.value = 'string';
    else return false;
    return editorChangeConditionType({target: type});
  }
  if (!event.target.matches('[data-condition-type]')) return false;
  const row = event.target.closest('[data-editor-condition]'), old = $('[data-condition-value]', row), type = event.target.value;
  const value = type === 'bool' ? (old.value === 'true' ? true : old.value === 'false' ? false : old.value) : old.value;
  old.outerHTML = type === 'list' ? '<textarea data-condition-value rows="3">' + esc(old.value) + '</textarea>' : editorTypedValue(value, type, 'data-condition-value');
  $('[data-condition-value]', row).focus();
  return true;
}
// Search one reference at a time, while retaining the original manual input.
function editorBindReferences(form, root) {
  if (!root.dataset?.segmentKeys) return;
  let keys = [];
  try { keys = JSON.parse(root.dataset?.segmentKeys || '[]'); } catch { /* manual entry remains available */ }
  if (!Array.isArray(keys)) return;
  keys = keys.map(key => typeof key === 'string' ? key : key.key).filter(Boolean);
  $$('[data-rule-segments], #seg-references', form).forEach((input, index) => {
    const group = document.createElement('div');
    group.className = 'reference-picker';
    const listID = (form.id || 'editor') + '-segment-options-' + index;
    group.innerHTML = '<label class="field">' + esc(t('editor.searchSegments')) +
      '<input type="search" data-reference-search list="' + listID + '" autocomplete="off"></label>' +
      '<datalist id="' + listID + '">' + keys.map(key => '<option value="' + esc(key) + '"></option>').join('') +
      '</datalist><button type="button" class="btn sm" data-reference-add>' + esc(t('editor.addReference')) +
      '</button><div class="row" data-reference-selected></div>';
    input.parentElement.insertAdjacentElement('afterend', group);
    const search = $('[data-reference-search]', group);
    const values = () => [...new Set(input.value.split(',').map(key => key.trim()).filter(Boolean))];
    const render = () => {
      $('[data-reference-selected]', group).innerHTML = values().map(key =>
        '<button type="button" class="btn sm" data-reference-remove="' + esc(key) + '" aria-label="' +
        esc(t('editor.removeReference', {key})) + '">' + esc(key) + ' ×</button>').join('');
    };
    const update = next => { input.value = next.join(', '); input.dispatchEvent(new Event('input', {bubbles: true})); render(); };
    const add = () => {
      const key = search.value.trim();
      if (!key) { search.focus(); return; }
      update([...new Set([...values(), key])]); search.value = ''; search.focus();
    };
    $('[data-reference-add]', group).onclick = add;
    search.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); add(); } });
    group.addEventListener('click', event => {
      const remove = event.target.closest('[data-reference-remove]');
      if (!remove) return;
      update(values().filter(key => key !== remove.dataset.referenceRemove)); search.focus();
    });
    input.addEventListener('input', render);
    render();
  });
}

function editorShowIssues(box, issues) {
  box.className = issues.length ? 'warn-text' : 'ok-text';
  box.tabIndex = -1;
  box.innerHTML = issues.length ? '<ul>' + issues.map(issue => '<li>' + esc(issue) + '</li>').join('') + '</ul>' : esc(t('validate.ok'));
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
    '<fieldset data-rule-condition-group><legend>' + esc(t('editor.conditions')) + '</legend>' + editorConditionsMarkup(rule.constraints || []) + '</fieldset></fieldset>';
}

function renderFlagStructured(content) {
  const names = Object.keys(content.variations || {});
  const bucketing = content.bucketing || {};
  return '<div class="flag-structured">' + editorMetadataMarkup(content, 'flag') + '<div class="flag-fields">' +
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
      editorTypedValue(content.variations[name], content.type, 'data-var-value') + '</label>' +
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
  editorReadMetadata(root, next, issues);
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
      const value = editorReadValue($('[data-var-value]', row), next.type);
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
    rule.constraints = editorReadConditions($('[data-rule-condition-group]', row), original.constraints || [], issues);
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
    .filter(control => !control.matches('[data-reference-search]')).map(control => [control.value, control.checked]));
  const status = () => {
    const box = $('#flag-structured-status', form);
    editorShowIssues(box, errors);
    $('#flag-save', root).disabled = errors.length > 0;
  };
  const redraw = () => {
    form.innerHTML = renderFlagStructured(JSON.parse(json.value));
    editorBindReferences(form, root);
    const content = JSON.parse(json.value);
    errors = [...lintFlag(content), ...[content.defaultRule, ...(content.targeting || [])].flatMap(rule =>
      (rule?.constraints || []).filter(editorConditionSupported).flatMap(condition =>
        lintConstraints([condition])))];
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
    if (event.target.matches('[data-var-name], [data-rule-mode], [data-condition-type], #flag-type')) return;
    sync();
  });
  form.addEventListener('change', event => {
    editorChangeConditionType(event);
    if (event.target.id === 'flag-type') {
      const requested = event.target.value, content = JSON.parse(json.value);
      event.target.value = content.type;
      if (!sync()) return;
      const updated = JSON.parse(json.value); updated.type = requested;
      updated.variations = Object.fromEntries(Object.entries(updated.variations).map(([name, value]) => {
        if (requested === 'string') return [name, typeof value === 'object' ? JSON.stringify(value) : String(value)];
        if (requested === 'number' && typeof value === 'string' && value.trim() && Number.isFinite(Number(value))) return [name, Number(value)];
        if (requested === 'bool' && ['true', 'false'].includes(value)) return [name, value === 'true'];
        return [name, value];
      }));
      json.value = JSON.stringify(updated, null, 2); redraw(); return;
    }
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
    const conditionButton = event.target.closest('[data-editor-add-condition], [data-editor-remove-condition]');
    if (conditionButton) {
      const row = conditionButton.closest('[data-rule-index]'), index = Number(row.dataset.ruleIndex);
      if (conditionButton.matches('[data-editor-remove-condition]')) {
        conditionButton.closest('[data-editor-condition]').remove();
        if (sync()) redraw();
        $('[data-rule-index="' + index + '"] [data-editor-add-condition]', form)?.focus();
        return;
      }
      if (controlSnapshot() !== lastSyncedControls && !sync()) return;
      const content = JSON.parse(json.value), rule = index < 0 ? content.defaultRule : content.targeting[index];
      rule.constraints ||= [];
      rule.constraints.push({field: '', op: 'eq', value: ''});
      json.value = JSON.stringify(content, null, 2); redraw();
      const changed = $('[data-rule-index="' + index + '"]', form);
      (conditionButton.matches('[data-editor-add-condition]') ? $$('[data-condition-field]', changed).at(-1) : $('[data-editor-add-condition]', changed))?.focus();
      return;
    }
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
      ($('[aria-invalid="true"]', form) || $('#flag-structured-status', form)).focus();
      return false;
    },
    redraw,
  };
}
