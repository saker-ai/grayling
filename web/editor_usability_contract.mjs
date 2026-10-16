import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const context = vm.createContext({
  esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
  t: key => key,
  lintConstraints: () => [],
  OPS: ['eq', 'in', 'not_in', 'gt'],
  $: (selector, root) => root.controls[selector],
  $$: (selector, root) => root.rows || [],
});
vm.runInContext(fs.readFileSync(new URL('./flag_editor.js', import.meta.url), 'utf8') +
  '\nthis.api = { editorReadValue, editorTypedValue, editorConditionsMarkup, editorReadConditions, editorChangeConditionType, editorMetadataIssues, editorMetadataMarkup, editorReadMetadata };', context);
const { editorReadValue, editorTypedValue, editorConditionsMarkup, editorReadConditions } = context.api;
const input = value => ({ value, attributes: {}, removeAttribute(name) { delete this.attributes[name]; },
  setAttribute(name, val) { this.attributes[name] = val; }, closest() { return null; } });
assert.equal(editorReadValue(input('hello "world"'), 'string'), 'hello "world"');
assert.equal(editorReadValue(input('false'), 'bool'), false);
assert.equal(editorReadValue(input('0.25'), 'number'), 0.25);
const invalid = input('');
assert.throws(() => editorReadValue(invalid, 'number'));
assert.equal(invalid.attributes['aria-invalid'], 'true');
assert(editorTypedValue('hello', 'string', 'data-var-value').includes('value="hello"'));
assert(editorTypedValue(true, 'bool', 'data-var-value').includes('<select'));
assert(editorTypedValue('invalid', 'number', 'data-var-value').includes('value="invalid"'));
assert(editorTypedValue('invalid', 'bool', 'data-var-value').includes('value="" selected'));
assert.throws(() => editorReadValue(input(''), 'bool'), 'invalid conversion must require an explicit Boolean choice');
assert(editorConditionsMarkup([{ field: 'ids', op: 'in', values: [1, true, 'x'] }]).includes('data-condition-json'), 'mixed lists must keep exact JSON types');
assert(editorConditionsMarkup([{ field: 'name', op: 'eq', value: '<script>' }]).includes('&lt;script>'));
const original = { field: 'country', op: 'eq', value: 'CN', futureOption: { enabled: true } };
const row = { dataset: { editorCondition: '0' }, controls: {
  '[data-condition-json]': null,
  '[data-condition-field]': input('country'), '[data-condition-op]': input('eq'),
  '[data-condition-type]': input('string'), '[data-condition-value]': input('US'),
} };
const issues = [];
const updated = editorReadConditions({ rows: [row] }, [original], issues)[0];
assert.equal(updated.value, 'US');
assert.equal(updated.futureOption.enabled, true, 'editing known fields must preserve unknown condition metadata');
assert.equal(original.value, 'CN', 'editing must not mutate the saved document');
assert.equal(issues.length, 0);
row.controls['[data-condition-op]'].value = 'in';
row.controls['[data-condition-type]'].value = 'list';
row.controls['[data-condition-value]'].value = 'alice\nbob';
const list = editorReadConditions({ rows: [row] }, [original], [])[0];
assert.equal(JSON.stringify(list.values), '["alice","bob"]');
assert.equal(Object.hasOwn(list, 'value'), false);
// Failed input is reported without replacing the last saved condition.
row.controls['[data-condition-type]'].value = 'number';
row.controls['[data-condition-value]'].value = '';
const errors = [];
const rejected = editorReadConditions({ rows: [row] }, [original], errors)[0];
assert.equal(rejected, original);
assert.equal(errors.length, 1);
assert.equal(row.controls['[data-condition-value]'].attributes['aria-invalid'], 'true');
// Switching a set operator adapts the value widget without a second schema decision.
const control = input('alice');
control.focus = () => {};
const type = input('string');
type.matches = selector => selector === '[data-condition-type]';
const condition = {controls: {'[data-condition-type]': type, '[data-condition-value]': control}};
type.closest = () => condition;
const op = input('in');
op.matches = selector => selector === '[data-condition-op]';
op.closest = () => condition;
context.api.editorChangeConditionType({target: op});
assert.equal(type.value, 'list');
assert(control.outerHTML.includes('<textarea'));
op.value = 'eq';
context.api.editorChangeConditionType({target: op});
assert.equal(type.value, 'string');
assert(control.outerHTML.includes('value="alice"'));
const metadataControls = {controls: {
  '[data-editor-display-name]': input('中文策略 🚀'),
  '[data-editor-description]': input('用于内部用户\n保留换行和中文'),
}};
const metadata = {key: 'internal-release', future: {enabled: true}};
context.api.editorReadMetadata(metadataControls, metadata, []);
assert.equal(metadata.displayName, '中文策略 🚀');
assert.equal(metadata.description, '用于内部用户\n保留换行和中文');
assert.equal(metadata.key, 'internal-release');
assert.equal(metadata.future.enabled, true);
assert(context.api.editorMetadataMarkup({displayName: '<img src=x>', description: '</textarea><script>'}, 'flag').includes('&lt;img'));
assert(!context.api.editorMetadataMarkup({description: '</textarea><script>'}, 'flag').includes('</textarea><script>'));
assert.equal(context.api.editorMetadataIssues({displayName: '🚀'.repeat(128)}).length, 0, 'limits count Unicode code points, not UTF-16 units');
assert.equal(context.api.editorMetadataIssues({displayName: '🚀'.repeat(129)}).length, 1);
assert.equal(context.api.editorMetadataIssues({description: '中'.repeat(2049)}).length, 1);
assert.equal(context.api.editorMetadataIssues({displayName: 123}).length, 1);
metadataControls.controls['[data-editor-display-name]'].value = '';
metadataControls.controls['[data-editor-description]'].value = '';
const oldDocument = {key: 'unchanged'};
context.api.editorReadMetadata(metadataControls, oldDocument, []);
assert.equal(JSON.stringify(oldDocument), '{"key":"unchanged"}', 'blank optional metadata must not change legacy document hashes');
context.api.editorReadMetadata(metadataControls, metadata, []);
assert.equal(Object.hasOwn(metadata, 'displayName'), false, 'metadata can be cleared explicitly');
metadataControls.controls['[data-editor-display-name]'].value = '中'.repeat(129);
const metadataErrors = [];
context.api.editorReadMetadata(metadataControls, {}, metadataErrors);
assert.equal(metadataErrors.length, 1);
assert.equal(metadataControls.controls['[data-editor-display-name]'].attributes['aria-invalid'], 'true');
console.log('editor usability contracts passed');
