// segmentEditor.test.tsx ports the segment half of the retired legacy editor
// contracts onto the React SegmentEditor: conditions and references sync from
// the uncontrolled form into the JSON source, the value-type switch carries
// the raw text across the remount, removing a condition redraws from the
// merged document, unsupported constraint shapes stay as raw JSON rows, and
// the validate slot reproduces the legacy wording (keyShould / valid /
// invalidJson).
import { createRef, useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render } from '@testing-library/react';
import { ConfirmProvider, ToastProvider } from '@saker/grayling-pages';
import { SegmentEditor } from '@saker/grayling-pages/editors/SegmentEditor';
import type { SegmentEditorHandle } from '@saker/grayling-pages/editors/SegmentEditor';
import '../i18n';

afterEach(cleanup);

const tick = (): Promise<void> => new Promise(resolve => setTimeout(resolve, 0));

const BASE_DOC: Record<string, unknown> = {
  schemaVersion: 1,
  key: 'seg-one',
  displayName: 'Beta users',
  description: 'Users in the beta cohort',
  constraints: [
    { field: 'country', op: 'eq', value: 'US' },
    { field: 'tags', op: 'in', values: ['a', 'b'] },
  ],
  segments: ['seg-a'],
  customNote: { keep: 'yes' },
};

function renderEditor(doc: Record<string, unknown> = BASE_DOC) {
  const ref = createRef<SegmentEditorHandle>();
  const onActivity = vi.fn();
  function Host() {
    const [value, setValue] = useState(JSON.stringify(doc, null, 2));
    const [risk, setRisk] = useState('normal');
    return (
      <SegmentEditor
        ref={ref}
        segKey="seg-one"
        value={value}
        onChange={setValue}
        risk={risk}
        onRiskChange={setRisk}
        segmentKeys={['seg-a', 'seg-b']}
        onActivity={onActivity}
        onIssuesChange={() => {}}
      />
    );
  }
  render(<ToastProvider><ConfirmProvider><Host /></ConfirmProvider></ToastProvider>);
  return { ref, onActivity };
}

function source(ref: { current: SegmentEditorHandle | null }): Record<string, unknown> {
  return JSON.parse(ref.current!.readSource()) as Record<string, unknown>;
}

describe('SegmentEditor structured form', () => {
  it('paints conditions, references and metadata from the source', () => {
    renderEditor();
    const rows = document.querySelectorAll('[data-editor-condition]');
    expect(rows.length).toBe(2);
    expect(rows[0].querySelector<HTMLInputElement>('[data-condition-field]')!.value).toBe('country');
    expect(rows[1].querySelector<HTMLSelectElement>('[data-condition-op]')!.value).toBe('in');
    expect(document.querySelector<HTMLInputElement>('#seg-references')!.value).toBe('seg-a');
    expect(document.querySelector<HTMLInputElement>('[data-editor-display-name]')!.value).toBe('Beta users');
  });

  it('syncs condition edits into the source and preserves unknown fields', () => {
    const { ref } = renderEditor();
    const field = document.querySelectorAll<HTMLInputElement>('[data-condition-field]')[0];
    fireEvent.input(field, { target: { value: 'region' } });
    const doc = source(ref);
    expect((doc.constraints as Array<{ field: string }>)[0].field).toBe('region');
    expect(doc.customNote).toEqual({ keep: 'yes' });
  });

  it('carries the raw text across a value-type switch (string → number)', async () => {
    const { ref } = renderEditor();
    const row = document.querySelectorAll('[data-editor-condition]')[0];
    const valueInput = row.querySelector<HTMLInputElement>('[data-condition-value]')!;
    fireEvent.input(valueInput, { target: { value: '42' } });
    const typeSelect = row.querySelector<HTMLSelectElement>('[data-condition-type]')!;
    fireEvent.change(typeSelect, { target: { value: 'number' } });
    await act(tick); // the deferred sync reads the remounted control
    const doc = source(ref);
    expect((doc.constraints as Array<{ value: unknown }>)[0].value).toBe(42);
  });

  it('switches an operator to a set operator and collects values', async () => {
    const { ref } = renderEditor();
    const row = document.querySelectorAll('[data-editor-condition]')[0];
    fireEvent.change(row.querySelector<HTMLSelectElement>('[data-condition-op]')!, { target: { value: 'not_in' } });
    await act(tick);
    const list = row.querySelector<HTMLTextAreaElement>('[data-condition-value]')!;
    expect(list.dataset.valueType).toBe('list');
    fireEvent.input(list, { target: { value: 'US\nCA' } });
    const doc = source(ref);
    expect((doc.constraints as Array<{ op: string; values: string[] }>)[0]).toEqual({ field: 'country', op: 'not_in', values: ['US', 'CA'] });
  });

  it('removes a condition after the deferred sync confirms the merge', async () => {
    const { ref } = renderEditor();
    const rows = document.querySelectorAll('[data-editor-condition]');
    fireEvent.click(rows[0].querySelector('[data-editor-remove-condition]')!);
    await act(tick);
    const doc = source(ref);
    expect((doc.constraints as unknown[]).length).toBe(1);
    expect((doc.constraints as Array<{ field: string }>)[0].field).toBe('tags');
    expect(document.querySelectorAll('[data-editor-condition]').length).toBe(1);
  });

  it('adds a condition row; the empty field lands in the source with a visible issue until fixed', async () => {
    const { ref } = renderEditor();
    fireEvent.click(document.querySelector('[data-editor-add-condition]')!);
    expect(document.querySelectorAll('[data-editor-condition]').length).toBe(3);
    // Legacy pushes the blank row into the JSON source immediately; the lint
    // issue shows in the status panel until the operator names the field.
    expect((source(ref).constraints as Array<{ field: string }>)[2]).toEqual({ field: '', op: 'eq', value: '' });
    expect(document.getElementById('seg-structured-status')!.className).toContain('warn-text');
    const field = document.querySelectorAll<HTMLInputElement>('[data-condition-field]')[2];
    fireEvent.input(field, { target: { value: 'plan' } });
    const doc = source(ref);
    expect((doc.constraints as Array<{ field: string }>).map(c => c.field)).toEqual(['country', 'tags', 'plan']);
    expect(document.getElementById('seg-structured-status')!.className).toContain('ok-text');
  });

  it('keeps unsupported constraint shapes as raw JSON rows', () => {
    const doc = {
      ...BASE_DOC,
      constraints: [{ field: 'payload', op: 'eq', value: { nested: true } }],
    };
    renderEditor(doc);
    const raw = document.querySelector<HTMLTextAreaElement>('[data-condition-json]');
    expect(raw).toBeTruthy();
    expect(JSON.parse(raw!.value)).toEqual({ field: 'payload', op: 'eq', value: { nested: true } });
  });

  it('syncs the references field into the segments array', () => {
    const { ref } = renderEditor();
    const input = document.querySelector<HTMLInputElement>('#seg-references')!;
    fireEvent.input(input, { target: { value: 'seg-a, seg-b' } });
    expect(source(ref).segments).toEqual(['seg-a', 'seg-b']);
  });
});

describe('SegmentEditor modes and handles', () => {
  it('validate reproduces the legacy wording: keyShould, invalidJson, valid', () => {
    renderEditor();
    fireEvent.click(document.getElementById('seg-validate')!);
    expect(document.getElementById('seg-valid')!.className).toContain('ok-text');

    fireEvent.click(document.getElementById('seg-mode-json')!);
    const json = document.getElementById('seg-json') as HTMLTextAreaElement;
    fireEvent.change(json, { target: { value: '{nope' } });
    fireEvent.click(document.getElementById('seg-validate')!);
    expect(document.getElementById('seg-valid')!.className).toContain('err-text');

    const wrongKey = { ...BASE_DOC, key: 'other-seg' };
    fireEvent.change(json, { target: { value: JSON.stringify(wrongKey) } });
    fireEvent.click(document.getElementById('seg-validate')!);
    expect(document.getElementById('seg-valid')!.className).toContain('warn-text');
  });

  it('snapshot() tracks the risk select as well as the document', () => {
    const { ref } = renderEditor();
    const before = ref.current!.snapshot();
    fireEvent.change(document.getElementById('seg-risk')!, { target: { value: 'high' } });
    expect(ref.current!.snapshot()).not.toBe(before);
  });

  it('valid() fails on a field-level issue and readSource stays unchanged', () => {
    const { ref } = renderEditor();
    const field = document.querySelectorAll<HTMLInputElement>('[data-condition-field]')[0];
    fireEvent.input(field, { target: { value: '' } });
    expect(ref.current!.valid()).toBe(false);
    expect((source(ref).constraints as Array<{ field: string }>)[0].field).toBe('country');
  });
});
