// structuredDom.ts ports the DOM-reading half of the legacy editors
// (web/legacy/flag_editor.js + segment_editor.js). The React ports keep
// legacy's uncontrolled-input architecture: the structured form is painted
// once per generation and reads happen straight off the DOM, so a failed
// parse never clobbers the JSON source of truth.
import type { Translate } from '@saker/grayling-client';
import { OPS, lintConstraints, lintFlag, lintSegment } from '../lint';

export type FlagDoc = Record<string, unknown>;

export function conditionSupported(c: unknown): c is Record<string, unknown> {
  const shape = c as { op?: unknown; values?: unknown; value?: unknown };
  return !!c && typeof c === 'object' && !Array.isArray(c) &&
    (OPS as readonly string[]).includes(shape.op as string) &&
    (shape.op === 'in' || shape.op === 'not_in'
      ? Array.isArray(shape.values) && shape.values.every(v => typeof v === 'string' && !v.includes('\n'))
      : ['string', 'number', 'boolean'].includes(typeof shape.value));
}

// The value-control type for a supported condition, matching legacy
// editorConditionsMarkup's type derivation.
export function conditionValueType(c: Record<string, unknown>): string {
  if (c.op === 'in' || c.op === 'not_in') return 'list';
  return ({ boolean: 'bool', number: 'number', string: 'string' } as Record<string, string>)[typeof c.value];
}

// editorReadValue: coerces a control's text into the declared type, marking
// aria-invalid + aria-describedby on failure. statusId selects the owning
// editor's status box, matching legacy's `#seg-structured` lookup.
export function editorReadValue(input: HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement, type: string, statusId: string): unknown {
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
  } catch (error) {
    input.setAttribute('aria-invalid', 'true');
    input.setAttribute('aria-describedby', statusId);
    throw error;
  }
}

// editorReadMetadata: displayName/description with Unicode-aware limits. An
// untouched optional field must not change an existing document's hash.
export function editorReadMetadata(root: ParentNode, content: FlagDoc, issues: string[], t: Translate): void {
  for (const [field, selector, limit, key] of [
    ['displayName', '[data-editor-display-name]', 128, 'editor.displayNameLimit'],
    ['description', '[data-editor-description]', 2048, 'editor.descriptionLimit'],
  ] as const) {
    const input = root.querySelector<HTMLInputElement | HTMLTextAreaElement>(selector);
    if (!input) continue;
    input.removeAttribute('aria-invalid');
    const value = input.value;
    if (Array.from(value).length > limit) {
      input.setAttribute('aria-invalid', 'true');
      issues.push(t(key));
    }
    if (value) content[field] = value;
    else if (Object.hasOwn(content, field)) delete content[field];
  }
}

// editorReadConditions: unsupported shapes round-trip through their JSON
// textarea; supported rows are rebuilt from the field/op/type/value controls.
export function editorReadConditions(container: ParentNode, originals: unknown[], issues: string[], t: Translate, statusId: string): unknown[] {
  return Array.from(container.querySelectorAll<HTMLElement>('[data-editor-condition]')).map(row => {
    const original = originals[Number(row.dataset.editorCondition)];
    const raw = row.querySelector<HTMLTextAreaElement>('[data-condition-json]');
    try {
      if (raw) return editorReadValue(raw, 'json', statusId);
      const fieldControl = row.querySelector<HTMLInputElement>('[data-condition-field]')!;
      const next: Record<string, unknown> = {
        ...(original as Record<string, unknown>),
        field: fieldControl.value.trim(),
        op: row.querySelector<HTMLSelectElement>('[data-condition-op]')!.value,
      };
      fieldControl.removeAttribute('aria-invalid');
      if (!next.field) {
        fieldControl.setAttribute('aria-invalid', 'true');
        issues.push(t('seg.editorBadConstraint', { n: Number(row.dataset.editorCondition) + 1 }));
      }
      const input = row.querySelector<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>('[data-condition-value]')!;
      const type = row.querySelector<HTMLSelectElement>('[data-condition-type]')!.value;
      const value = type === 'list'
        ? input.value.split('\n').filter(v => v.length)
        : editorReadValue(input, type, statusId);
      if (next.op === 'in' || next.op === 'not_in') {
        if (!Array.isArray(value)) {
          input.setAttribute('aria-invalid', 'true');
          throw new Error('list');
        }
        delete next.value;
        next.values = value;
      } else {
        delete next.values;
        next.value = value;
      }
      const validation = lintConstraints(t, [next]);
      if (validation.length) {
        input.setAttribute('aria-invalid', 'true');
        issues.push(...validation);
      }
      return next;
    } catch {
      issues.push(t('editor.valueRequired'));
      return original;
    }
  });
}

// readFlagStructured merges the whole structured form onto a deep copy of the
// parsed JSON source, preserving unknown fields. Names the user typed become
// the variation keys; references elsewhere remap through the rename table.
export function readFlagStructured(root: ParentNode, current: FlagDoc, t: Translate, statusId: string): { content: FlagDoc; issues: string[] } {
  const next: FlagDoc = JSON.parse(JSON.stringify(current));
  const issues: string[] = [];
  editorReadMetadata(root, next, issues, t);
  next.type = (root.querySelector<HTMLSelectElement>('#flag-type'))!.value;
  next.disable = (root.querySelector<HTMLInputElement>('#flag-disable'))!.checked;
  const unit = (root.querySelector<HTMLInputElement>('#flag-unit'))!.value.trim();
  const salt = (root.querySelector<HTMLInputElement>('#flag-salt'))!.value.trim();
  if (next.bucketing || unit || salt) {
    const bucketing = (next.bucketing || {}) as Record<string, unknown>;
    next.bucketing = { ...bucketing, algorithm: bucketing.algorithm || 'grayling-fnv1a-10000-v1', unit, salt };
  }
  next.fields = [...new Set((root.querySelector<HTMLInputElement>('#flag-fields'))!.value.split(',').map(value => value.trim()).filter(Boolean))];
  const rename: Record<string, string> = {};
  const variations: Record<string, unknown> = {};
  root.querySelectorAll<HTMLElement>('[data-variation-row]').forEach(row => {
    const name = (row.querySelector<HTMLInputElement>('[data-var-name]'))!.value.trim();
    if (!name || Object.hasOwn(variations, name)) issues.push(t('flags.editorUniqueNames'));
    try {
      const value = editorReadValue(row.querySelector<HTMLInputElement | HTMLSelectElement>('[data-var-value]')!, next.type as string, statusId);
      const valid = next.type === 'json' ||
        (next.type === 'bool' && typeof value === 'boolean') ||
        (next.type === 'string' && typeof value === 'string') ||
        (next.type === 'number' && typeof value === 'number' && Number.isFinite(value));
      if (!valid) issues.push(t('flags.editorValueType', { name }));
      variations[name] = value;
    } catch {
      issues.push(t('flags.editorValueJson', { name }));
    }
    if (row.dataset.original !== name) rename[row.dataset.original!] = name;
  });
  if (!Object.keys(variations).length) issues.push(t('flags.editorNeedVariation'));
  next.variations = variations;
  const remap = (name: string): string => rename[name] || name;
  const readRule = (row: HTMLElement, original: Record<string, unknown>): Record<string, unknown> => {
    const rule: Record<string, unknown> = { ...original };
    if (row.dataset.ruleIndex !== '-1') {
      rule.name = row.querySelector<HTMLInputElement>('[data-rule-name]')!.value.trim();
      rule.disable = row.querySelector<HTMLInputElement>('[data-rule-disabled]')!.checked;
    }
    rule.segments = row.querySelector<HTMLInputElement>('[data-rule-segments]')!.value.split(',').map(value => value.trim()).filter(Boolean);
    rule.constraints = editorReadConditions(row.querySelector<HTMLElement>('[data-rule-condition-group]')!, (original.constraints as unknown[]) || [], issues, t, statusId);
    if (row.querySelector<HTMLSelectElement>('[data-rule-mode]')!.value === 'percentage') {
      delete rule.variation;
      const percentage: Record<string, number> = {};
      row.querySelectorAll<HTMLInputElement>('[data-split]').forEach(input => {
        const percent = Number(input.value);
        if (!input.value.trim() || !Number.isFinite(percent) || percent < 0 || percent > 100) {
          issues.push(t('flags.editorPercentRange'));
        }
        percentage[remap(input.dataset.split!)] = percent;
      });
      rule.percentage = percentage;
    } else {
      delete rule.percentage;
      rule.variation = remap(row.querySelector<HTMLSelectElement>('[data-rule-variation]')!.value);
    }
    return rule;
  };
  next.defaultRule = readRule(root.querySelector<HTMLElement>('[data-rule-index="-1"]')!, (current.defaultRule as Record<string, unknown>) || {});
  next.targeting = Array.from(root.querySelectorAll<HTMLElement>('[data-rule-index]'))
    .filter(row => row.dataset.ruleIndex !== '-1')
    .map((row, index) => readRule(row, ((current.targeting as Record<string, unknown>[]) || [])[index] || {}));
  return { content: next, issues: [...new Set([...issues, ...lintFlag(t, next)])] };
}

// readSegmentStructured differs from the flag read in one legacy asymmetry:
// when any field-level issue exists, lintSegment is skipped entirely.
export function readSegmentStructured(root: ParentNode, current: FlagDoc, t: Translate, statusId: string): { content: FlagDoc; issues: string[] } {
  const content: FlagDoc = JSON.parse(JSON.stringify(current));
  const issues: string[] = [];
  editorReadMetadata(root, content, issues, t);
  content.constraints = editorReadConditions(root, (current.constraints as unknown[]) || [], issues, t, statusId);
  content.segments = [...new Set(root.querySelector<HTMLInputElement>('#seg-references')!.value.split(',').map(value => value.trim()).filter(Boolean))];
  return { content, issues: [...new Set(issues.length ? issues : lintSegment(t, content))] };
}

// controlSnapshot is the dirty-detection half of the legacy editors: the flag
// editor tracks [value, checked] pairs; the segment editor tracks values only.
// The reference-search box is excluded in both (it is not document state).
export function controlSnapshot(form: ParentNode, pairs: boolean): string {
  return JSON.stringify(
    Array.from(form.querySelectorAll<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>('input, select, textarea'))
      .filter(control => !control.matches('[data-reference-search]'))
      .map(control => (pairs ? [control.value, (control as HTMLInputElement).checked] : control.value)),
  );
}
