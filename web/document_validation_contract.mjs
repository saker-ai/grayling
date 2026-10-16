import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const context = vm.createContext({
  t: (key, values = {}) => key + JSON.stringify(values),
  resourceKeyError: value => typeof value === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(value) ? '' : 'key.formatHint',
  OPS: ['eq', 'ne', 'in', 'not_in', 'gt', 'gte', 'lt', 'lte', 'regex', 'in_cidr',
    'time_before', 'time_after', 'semver_gt', 'semver_gte', 'semver_lt', 'semver_lte', 'semver_eq'],
});
vm.runInContext(['flag_editor.js', 'segment_editor.js'].map(name =>
  fs.readFileSync(new URL(name, import.meta.url), 'utf8')).join('\n') +
  '\nthis.api = {lintConstraints, lintSegment, lintSegmentReferences, editorValidCIDR, editorValidSemver, editorValidTime};', context);
const {lintConstraints, lintSegment, lintSegmentReferences, editorValidCIDR, editorValidSemver, editorValidTime} = context.api;
const constraint = (op, value) => [{field: 'country', op, value}];
for (const document of [null, true, 1, 'text', [], {}]) {
  assert.doesNotThrow(() => lintSegment(document));
  assert(lintSegment(document).length, 'malformed Segment must not validate');
}
for (const invalid of [true, 1, 'conditions', {}]) assert(lintConstraints(invalid).length);
assert.equal(lintConstraints(null).length, 0);
assert.equal(lintConstraints(undefined).length, 0);
assert.equal(lintSegment({schemaVersion: 1, key: 'audience', segments: ['staff']}).length, 0, 'omitted empty arrays match Go nil slices');
assert(lintSegment({schemaVersion: 1, key: '中文', segments: ['staff']}).length);
assert(lintSegment({schemaVersion: 2, key: 'audience', segments: ['staff']}).length);
assert(lintSegmentReferences(['中文']).length);
assert(lintSegmentReferences(['self'], 'self').length);
assert(lintSegmentReferences(Array(17).fill('staff'), '', 16).length);
for (const value of [null, {}, [], Infinity]) assert(lintConstraints(constraint('eq', value)).length);
for (const value of ['CN', true, 0]) assert.equal(lintConstraints(constraint('eq', value)).length, 0);
assert(lintConstraints(constraint('gt', '10')).length, 'numeric operators must reject numeric strings');
assert.equal(lintConstraints([{field: 'ids', op: 'in', values: [1, true, 'x']}]).length, 0);
for (const values of [[], [null], [{}], Array(513).fill('x')]) assert(lintConstraints([{field: 'ids', op: 'in', values}]).length);
assert(lintConstraints(constraint('eq', 'CN'), new Set(['userId'])).length, 'closed field declarations must be checked');
assert(lintConstraints(Array(33).fill({field: 'country', op: 'eq', value: 'CN'}), null, 32).length);
assert.equal(lintConstraints(Array(33).fill({field: 'country', op: 'eq', value: 'CN'})).length, 0, 'standalone Segment has no top-level constraint limit in the backend');
for (const pattern of ['[', '(?=x)', '(a)\\1', '中'.repeat(171)]) assert(lintConstraints(constraint('regex', pattern)).length);
for (const pattern of ['^a.*$', '(?P<name>a)', '(?i)hello', '[(?=]', '\\\\1']) assert.equal(lintConstraints(constraint('regex', pattern)).length, 0);
for (const cidr of ['10.0.0.0/8', '192.168.1.12/32', '::/0', '2001:db8::/32', '::ffff:192.168.1.1/128']) assert(editorValidCIDR(cidr), cidr);
for (const cidr of ['256.1.1.1/24', '01.2.3.4/24', '10.0.0.0/33', ':::/0', ':1::/64', '1:2:3:4:5:6:7:8::/64', '::1/129', '1.2.3.4::/128', '10.0.0.0/8\n', '1\n.2.3.4/24']) assert(!editorValidCIDR(cidr), cidr);
for (const version of ['1.2.3', 'v1.2.3-rc.1+build.04', '0.0.0']) assert(editorValidSemver(version), version);
for (const version of ['1.2', '01.2.3', '1.2.3-01', '1.2.3+', '1.2.3-hello..world', '18446744073709551616.0.0', '1.2.3\n']) assert(!editorValidSemver(version), version);
for (const time of [0, 1.5, '2024-02-29T12:00:00Z', '2026-10-15T12:00:00.123+08:00']) assert(editorValidTime(time), String(time));
for (const time of [null, true, Infinity, '2025-02-29T00:00:00Z', '2026-10-15', '2026-10-15T24:00:00Z', '2026-10-15T12:00:00+99:99', '2026-10-15T12:00:00Z\n']) assert(!editorValidTime(time), String(time));
for (const [op, value] of [['in_cidr', 'bad'], ['semver_gte', '1.2'], ['time_after', 'yesterday']]) assert(lintConstraints(constraint(op, value)).length);
console.log('document validation contracts passed');
