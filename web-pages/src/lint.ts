// Client-side lint of the flag/segment DSL, ported branch-for-branch from
// web/legacy/app.js (lintFlag) and web/legacy/segment_editor.js (constraint
// validators, lintConstraints/lintSegmentReferences/lintSegment). All pure:
// localization arrives via the caller's t() so the module never touches i18n
// state. The server validators (pkg/model) remain authoritative — these exist
// so the editors can block an invalid save before any request is issued.
import type { Translate } from '@saker/grayling-client';
import { resourceKeyError } from './utils';

export const OPS = [
  'eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'time_before',
  'time_after', 'regex', 'in_cidr', 'semver_gt', 'semver_gte', 'semver_lt',
  'semver_lte', 'semver_eq',
] as const;

export interface ConstraintShape {
  field?: unknown;
  op?: unknown;
  value?: unknown;
  values?: unknown;
  [k: string]: unknown;
}

// editorUTF8Length counts bytes the way Go's len(string) would.
export function editorUTF8Length(value: string): number {
  return Array.from(value).reduce((length, character) => {
    const point = character.codePointAt(0) ?? 0;
    return length + (point < 0x80 ? 1 : point < 0x800 ? 2 : point < 0x10000 ? 3 : 4);
  }, 0);
}

export function editorValidCIDR(value: unknown): boolean {
  if (typeof value !== 'string') return false;
  const parts = value.split('/');
  if (parts.length !== 2 || !/^(0|[1-9]\d*)$/.test(parts[1])) return false;
  const ipv4 = (address: string): boolean => {
    const octets = address.split('.');
    return octets.length === 4 && octets.every(n => /^(0|[1-9]\d{0,2})$/.test(n) && Number(n) <= 255);
  };
  const address = parts[0];
  const prefix = Number(parts[1]);
  if (!address.includes(':')) return prefix <= 32 && ipv4(address);
  if (prefix > 128 || /:::|^:[^:]|[^:]:$/.test(address) || address.includes('%') || /[^0-9a-fA-F:.]/.test(address)) return false;
  const halves = address.split('::');
  if (halves.length > 2) return false;
  const groups = address.split(':').filter(Boolean);
  let count = 0;
  for (let i = 0; i < groups.length; i++) {
    if (groups[i].includes('.')) {
      if (i !== groups.length - 1 || !address.endsWith(groups[i]) || !ipv4(groups[i])) return false;
      count += 2;
    } else {
      if (!/^[0-9a-fA-F]{1,4}$/.test(groups[i])) return false;
      count++;
    }
  }
  return halves.length === 2 ? count < 8 : count === 8 && !address.startsWith(':') && !address.endsWith(':');
}

export function editorValidSemver(value: unknown): boolean {
  if (typeof value !== 'string') return false;
  const match = /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(value);
  if (!match) return false;
  const max = 18446744073709551615n;
  if (match.slice(1, 4).some(part => BigInt(part) > max)) return false;
  return !match[4] || match[4].split('.').every(part => !/^\d+$/.test(part) ||
    (!/^0\d/.test(part) && BigInt(part) <= max));
}

export function editorValidTime(value: unknown): boolean {
  if (typeof value === 'number') return Number.isFinite(value);
  if (typeof value !== 'string') return false;
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,]\d+)?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const days = [31, y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return m >= 1 && m <= 12 && d >= 1 && d <= days[m - 1] && Number(hour) < 24 &&
    Number(minute) < 60 && Number(second) < 60 &&
    (zone === 'Z' || (Number(zone.slice(1, 3)) <= 24 && Number(zone.slice(4)) <= 60));
}

// editorUnsupportedRE2Pattern flags constructs RE2 rejects (backreferences,
// lookahead/lookbehind) so the linter can reject them locally instead of
// letting the server 400 the save.
export function editorUnsupportedRE2Pattern(pattern: string): boolean {
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

// lintConstraints is the shared constraint linter. `registered` (when given)
// restricts constraint fields to the declared allowlist; `maxCount` caps the
// list length. Local syntax checks complement server validation; RE2 remains
// authoritative.
export function lintConstraints(
  t: Translate,
  constraints: unknown,
  registered: ReadonlySet<string> | null = null,
  maxCount: number | null = null,
): string[] {
  if (constraints == null) return [];
  if (!Array.isArray(constraints)) return [t('flags.editorConstraintsArray')];
  const issues: string[] = [];
  if (maxCount !== null && constraints.length > maxCount) issues.push(t('editor.constraintLimit', { max: maxCount }));
  const scalar = (value: unknown): boolean => ['string', 'boolean'].includes(typeof value) ||
    (typeof value === 'number' && Number.isFinite(value));
  constraints.forEach((constraint: ConstraintShape, index) => {
    const n = index + 1;
    if (!constraint || typeof constraint !== 'object' || Array.isArray(constraint) ||
        typeof constraint.field !== 'string' || !constraint.field || !(OPS as readonly string[]).includes(constraint.op as string)) {
      issues.push(t('seg.editorBadConstraint', { n }));
      return;
    }
    if (registered && !registered.has(constraint.field)) issues.push(t('editor.unregisteredField', { field: constraint.field }));
    const { op, value, values } = constraint;
    if (op === 'in' || op === 'not_in') {
      if (!Array.isArray(values) || !values.length || values.length > 512 || !values.every(scalar)) issues.push(t('editor.setLimit', { n }));
    } else if (op === 'regex') {
      if (typeof value !== 'string' || editorUTF8Length(value) > 512) issues.push(t('editor.regexLimit', { n }));
      else if (editorUnsupportedRE2Pattern(value)) issues.push(t('seg.editorBadRegex', { n }));
      else {
        // Go supports inline/scoped flags and quoted literals unavailable in
        // JS. Leave those patterns to the backend instead of falsely
        // rejecting them.
        if (/\(\?[imsU-]+[:)]|\\Q/.test(value)) return;
        try {
          new RegExp(value.replace(/\(\?P</g, '(?<'));
        } catch {
          issues.push(t('seg.editorBadRegex', { n }));
        }
      }
    } else if (op === 'in_cidr') {
      if (!editorValidCIDR(value)) issues.push(t('editor.invalidCIDR', { n }));
    } else if (typeof op === 'string' && op.startsWith('semver_')) {
      if (!editorValidSemver(value)) issues.push(t('editor.invalidSemver', { n }));
    } else if (op === 'time_before' || op === 'time_after') {
      if (!editorValidTime(value)) issues.push(t('editor.invalidTime', { n }));
    } else if (op === 'gt' || op === 'gte' || op === 'lt' || op === 'lte') {
      if (typeof value !== 'number' || !Number.isFinite(value)) issues.push(t('seg.editorNumberValue', { n }));
    } else if (!scalar(value)) issues.push(t('seg.editorScalarValue', { n }));
  });
  return [...new Set(issues)];
}

export function lintSegmentReferences(
  t: Translate,
  references: unknown,
  key = '',
  maxCount: number | null = null,
): string[] {
  if (references == null) return [];
  if (!Array.isArray(references)) return [t('seg.editorBadReference')];
  const issues: string[] = [];
  if (maxCount !== null && references.length > maxCount) issues.push(t('editor.referenceLimit', { max: maxCount }));
  if (references.some(ref => resourceKeyError(ref, t) || ref === key)) issues.push(t('seg.editorBadReference'));
  return issues;
}

// editorMetadataIssues checks the human-facing metadata fields shared by both
// editors: displayName ≤ 128 chars, description ≤ 2048 chars (Unicode-aware).
export function editorMetadataIssues(t: Translate, content: Record<string, unknown>): string[] {
  return ([['displayName', 128, 'editor.displayNameLimit'], ['description', 2048, 'editor.descriptionLimit']] as const)
    .filter(([field, limit]) => content[field] !== undefined &&
      (typeof content[field] !== 'string' || Array.from(content[field] as string).length > limit))
    .map(([, , key]) => t(key));
}

export function lintSegment(t: Translate, content: unknown): string[] {
  if (!content || typeof content !== 'object' || Array.isArray(content)) return [t('lint.notObject')];
  const c = content as Record<string, unknown>;
  const issues = editorMetadataIssues(t, c);
  if (c.schemaVersion !== 1) issues.push(t('editor.schemaVersion'));
  const keyIssue = resourceKeyError(c.key, t);
  if (keyIssue) issues.push(keyIssue);
  issues.push(...lintConstraints(t, c.constraints), ...lintSegmentReferences(t, c.segments, c.key as string));
  if (!(Array.isArray(c.constraints) && c.constraints.length) &&
      !(Array.isArray(c.segments) && c.segments.length)) issues.push(t('validate.segmentNonEmpty'));
  return [...new Set(issues)];
}

// lintFlag is the flag-document linter, a direct port of legacy lintFlag().
export function lintFlag(t: Translate, content: unknown): string[] {
  if (typeof content !== 'object' || content === null || Array.isArray(content)) return [t('lint.notObject')];
  const c = content as Record<string, unknown>;
  const issues = editorMetadataIssues(t, c);
  const bad = (field: string): void => { issues.push(t('validation.fieldType', { field })); };
  const limit = (field: string, count: number, max: number): void => {
    if (count > max) issues.push(t('validation.itemLimit', { field, max }));
  };
  if (c.schemaVersion !== 1) issues.push(t('editor.schemaVersion'));
  if (resourceKeyError(c.key, t)) issues.push(t('key.formatHint'));
  if (!['bool', 'string', 'number', 'json'].includes(c.type as string)) issues.push(t('lint.badType'));
  for (const field of ['disable'] as const) if (c[field] != null && typeof c[field] !== 'boolean') bad(field);
  if (c.version != null && typeof c.version !== 'string') bad('version');
  const fields = Array.isArray(c.fields) ? c.fields : [];
  if (c.fields != null && !Array.isArray(c.fields)) bad('fields');
  if (fields.some(field => typeof field !== 'string' || !field)) bad('fields');
  limit('fields', fields.length, 64);
  const registered = new Set(['targetingKey', ...fields as string[]]);
  if (!c.variations || typeof c.variations !== 'object' || Array.isArray(c.variations)) {
    return [...issues, t('flags.editorNeedVariation')];
  }
  const vars = c.variations as Record<string, unknown>;
  if (!Object.keys(vars).length) issues.push(t('flags.editorNeedVariation'));
  limit('variations', Object.keys(vars).length, 64);
  for (const [name, value] of Object.entries(vars)) {
    if (resourceKeyError(name, t)) issues.push(t('validation.identifier', { field: 'variations.' + name }));
    const valid = c.type === 'json' ||
      (c.type === 'bool' && typeof value === 'boolean') ||
      (c.type === 'string' && typeof value === 'string') ||
      (c.type === 'number' && typeof value === 'number' && Number.isFinite(value));
    if (!valid) issues.push(t('flags.editorValueType', { name }));
  }
  let needsBucketing = c.experiment != null;
  const checkRule = (r: unknown, where: string): void => {
    if (!r || typeof r !== 'object' || Array.isArray(r)) { issues.push(t('lint.ruleMissing', { where })); return; }
    const rule = r as Record<string, unknown>;
    if (rule.name != null && typeof rule.name !== 'string') bad(where + '.name');
    if (rule.disable != null && typeof rule.disable !== 'boolean') bad(where + '.disable');
    if (rule.variation != null && typeof rule.variation !== 'string') bad(where + '.variation');
    const hasVar = typeof rule.variation === 'string' && !!rule.variation;
    const split = !!rule.percentage && typeof rule.percentage === 'object' && !Array.isArray(rule.percentage);
    if (rule.percentage != null && !split) bad(where + '.percentage');
    const pct = split ? (rule.percentage as Record<string, unknown>) : null;
    const hasPct = split && Object.keys(pct!).length > 0;
    if (hasVar === hasPct) issues.push(t('lint.ruleExclusive', { where }));
    if (hasVar && !Object.hasOwn(vars, rule.variation as string)) issues.push(t('lint.unknownVariation', { where, name: rule.variation }));
    if (hasPct && pct) {
      needsBucketing = true;
      const sum = Object.values(pct).reduce<number>((a, b) => a + Number(b), 0);
      if (!Number.isFinite(sum) || Math.abs(sum - 100) > 1e-9) issues.push(t('lint.pctSum', { where, sum }));
      for (const [name, percent] of Object.entries(pct)) {
        if (!Object.hasOwn(vars, name)) issues.push(t('lint.unknownVariation', { where, name }));
        if (typeof percent !== 'number' || !Number.isFinite(percent) || percent < 0 || percent > 100) issues.push(t('flags.editorPercentRange'));
      }
    }
    issues.push(...lintConstraints(t, rule.constraints, registered, 32));
    issues.push(...lintSegmentReferences(t, rule.segments, '', 16));
  };
  checkRule(c.defaultRule, 'defaultRule');
  if (c.targeting != null && !Array.isArray(c.targeting)) issues.push(t('flags.editorRulesArray'));
  else {
    const rules = (c.targeting || []) as unknown[];
    limit('targeting', rules.length, 64);
    const names = new Set<string>();
    rules.forEach((rule, i) => {
      const r = rule as Record<string, unknown> | null;
      if (r?.name && names.has(r.name as string)) issues.push(t('validation.duplicateRule', { name: r.name }));
      if (r?.name) names.add(r.name as string);
      checkRule(rule, 'targeting[' + i + ']');
    });
  }
  const b = c.bucketing as Record<string, unknown> | null | undefined;
  if (b != null && (typeof b !== 'object' || Array.isArray(b))) bad('bucketing');
  if (needsBucketing) {
    if (!b || b.algorithm !== 'grayling-fnv1a-10000-v1') bad('bucketing.algorithm');
    if (!b || typeof b.unit !== 'string' || !registered.has(b.unit)) bad('bucketing.unit');
    if (b?.salt != null && (typeof b.salt !== 'string' || editorUTF8Length(b.salt) > 128)) bad('bucketing.salt');
  }
  if (c.experiment != null) {
    const exp = c.experiment as Record<string, unknown>;
    if (typeof exp !== 'object' || Array.isArray(exp)) bad('experiment');
    else {
      if (resourceKeyError(exp.id, t)) issues.push(t('validation.identifier', { field: 'experiment.id' }));
      if (exp.epoch != null && (!Number.isInteger(exp.epoch) || (exp.epoch as number) < 0)) bad('experiment.epoch');
      if (!['draft', 'reviewed', 'scheduled', 'running', 'paused', 'stopped', 'analyzed', 'archived'].includes(exp.state as string)) bad('experiment.state');
      if (exp.eligibility != null && (typeof exp.eligibility !== 'number' || !Number.isFinite(exp.eligibility) || exp.eligibility < 0 || exp.eligibility > 100)) bad('experiment.eligibility');
      if (exp.exposureMode && !['onDecision', 'onRender'].includes(exp.exposureMode as string)) bad('experiment.exposureMode');
      if (exp.layer && resourceKeyError(exp.layer, t)) issues.push(t('validation.identifier', { field: 'experiment.layer' }));
      const slot = exp.layerSlot as Record<string, unknown> | undefined;
      if (exp.layer && (!slot || !Number.isInteger(slot.start) || !Number.isInteger(slot.end) || (slot.start as number) < 0 || (slot.end as number) > 10000 || (slot.start as number) >= (slot.end as number))) bad('experiment.layerSlot');
      if (!(c.defaultRule as Record<string, unknown> | undefined)?.variation) bad('defaultRule.variation');
    }
  }
  return [...new Set(issues)];
}

// canonicalFlagValue sorts object keys recursively so two documents differing
// only in key order compare equal (used by the draft-vs-published diff).
export function canonicalFlagValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalFlagValue);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.keys(value as Record<string, unknown>).sort().map(key => [key, canonicalFlagValue((value as Record<string, unknown>)[key])]));
  }
  return value;
}

// flagDiffValue renders one diff side: '—' for absent, canonical JSON else.
export function flagDiffValue(value: unknown): string {
  return value === undefined ? '—' : JSON.stringify(canonicalFlagValue(value), null, 2);
}
