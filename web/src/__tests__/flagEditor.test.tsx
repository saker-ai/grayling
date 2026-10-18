// flagEditor.test.tsx ports the editor-fidelity half of the retired legacy
// contracts (editor_usability_contract.mjs + document_validation_contract.mjs)
// onto the React FlagEditor: the JSON textarea is the single unsaved source of
// truth, the structured form syncs DOM reads onto a deep copy of the parsed
// source (type fidelity, unknown-field preservation, rule rewrites on
// variation rename), and the validate/handle contract matches what the detail
// pages drive (valid() gates saves, readSource() reflects the post-sync source
// synchronously, snapshot() feeds the dirty check).
import { createRef, useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, act } from '@testing-library/react';
import { ConfirmProvider, ToastProvider } from '@saker/grayling-pages';
import { FlagEditor } from '@saker/grayling-pages/editors/FlagEditor';
import type { FlagEditorHandle } from '@saker/grayling-pages/editors/FlagEditor';
import '../i18n';

afterEach(cleanup);

const BASE_DOC: Record<string, unknown> = {
  schemaVersion: 1,
  key: 'flag-one',
  displayName: 'Checkout rollout',
  description: 'Fraction of traffic on the new checkout',
  type: 'bool',
  disable: false,
  bucketing: { algorithm: 'grayling-fnv1a-10000-v1', unit: 'user_id', salt: 's1' },
  fields: ['user_id', 'country'],
  variations: { control: false, treatment: true },
  defaultRule: { variation: 'control' },
  targeting: [
    { name: 'beta', variation: 'treatment', constraints: [{ field: 'country', op: 'eq', value: 'US' }] },
  ],
  experiment: null,
  customNote: { keep: 'yes' },
};

function renderEditor(doc: Record<string, unknown> = BASE_DOC) {
  const ref = createRef<FlagEditorHandle>();
  const onActivity = vi.fn();
  const onIssuesChange = vi.fn();
  function Host() {
    const [value, setValue] = useState(JSON.stringify(doc, null, 2));
    const [risk, setRisk] = useState('normal');
    return (
      <FlagEditor
        ref={ref}
        flagKey="flag-one"
        value={value}
        onChange={setValue}
        risk={risk}
        onRiskChange={setRisk}
        baseRevision={1}
        segmentKeys={['seg-a', 'seg-b']}
        onActivity={onActivity}
        onIssuesChange={onIssuesChange}
      />
    );
  }
  render(
    <ToastProvider>
      <ConfirmProvider>
        <Host />
      </ConfirmProvider>
    </ToastProvider>,
  );
  return { ref, onActivity, onIssuesChange };
}

function source(ref: { current: FlagEditorHandle | null }): Record<string, unknown> {
  return JSON.parse(ref.current!.readSource()) as Record<string, unknown>;
}

describe('FlagEditor structured form', () => {
  it('paints the parsed document: metadata, variations, rules, conditions', () => {
    renderEditor();
    const form = document.getElementById('flag-structured')!;
    const names = Array.from(form.querySelectorAll<HTMLInputElement>('[data-var-name]')).map(i => i.value);
    expect(names).toEqual(['control', 'treatment']);
    expect(form.querySelector<HTMLInputElement>('[data-editor-display-name]')!.value).toBe('Checkout rollout');
    expect(form.querySelector<HTMLTextAreaElement>('[data-editor-description]')!.value).toContain('checkout');
    expect(form.querySelector<HTMLInputElement>('[data-condition-field]')!.value).toBe('country');
    expect(form.querySelector<HTMLSelectElement>('[data-condition-op]')!.value).toBe('eq');
    expect((document.getElementById('flag-json') as HTMLTextAreaElement).value).toContain('"customNote"');
  });

  it('syncs a metadata edit into the JSON source and preserves unknown fields', () => {
    const { ref } = renderEditor();
    const nameInput = document.querySelector<HTMLInputElement>('[data-editor-display-name]')!;
    fireEvent.input(nameInput, { target: { value: 'Renamed rollout' } });
    const doc = source(ref);
    expect(doc.displayName).toBe('Renamed rollout');
    expect(doc.customNote).toEqual({ keep: 'yes' });
    expect((doc.bucketing as Record<string, unknown>).salt).toBe('s1');
  });

  it('renames a variation surgically: rules and splits follow the new name', () => {
    const { ref } = renderEditor();
    const nameInput = document.querySelectorAll<HTMLInputElement>('[data-var-name]')[1];
    fireEvent.change(nameInput, { target: { value: 'candidate' } });
    const doc = source(ref);
    expect(Object.keys(doc.variations as object)).toEqual(['control', 'candidate']);
    expect((doc.targeting as Array<{ variation?: string }>)[0].variation).toBe('candidate');
  });

  it('keeps variation value types faithful per flag type', () => {
    const doc = { ...BASE_DOC, type: 'number', variations: { control: 0, treatment: 25.5 } };
    const { ref } = renderEditor(doc);
    const valueInput = document.querySelectorAll<HTMLInputElement>('[data-var-value]')[1];
    fireEvent.input(valueInput, { target: { value: '42.25' } });
    const next = source(ref);
    expect((next.variations as Record<string, unknown>).treatment).toBe(42.25);
    expect((next.variations as Record<string, unknown>).control).toBe(0);
  });

  it('adds a variation and a targeting rule through the toolbar buttons', () => {
    const { ref } = renderEditor();
    fireEvent.click(document.getElementById('flag-add-variation')!);
    let doc = source(ref);
    expect(Object.keys(doc.variations as object)).toEqual(['control', 'treatment', 'variation1']);
    fireEvent.click(document.getElementById('flag-add-rule')!);
    doc = source(ref);
    expect((doc.targeting as unknown[]).length).toBe(2);
    expect((doc.targeting as Array<{ variation?: string }>)[1].variation).toBe('control');
  });

  it('reports an issue and stays invalid when metadata exceeds the limits', () => {
    const { ref, onIssuesChange } = renderEditor();
    const nameInput = document.querySelector<HTMLInputElement>('[data-editor-display-name]')!;
    fireEvent.input(nameInput, { target: { value: 'x'.repeat(129) } });
    expect(ref.current!.valid()).toBe(false);
    const lastCall = onIssuesChange.mock.calls.at(-1)?.[0] as string[];
    expect(lastCall.length).toBeGreaterThan(0);
    // The source still parses and the oversized value is NOT written back.
    expect(source(ref).displayName).toBe('Checkout rollout');
  });
});

describe('FlagEditor modes and handles', () => {
  it('switches to JSON mode and back, repainting the form from the source', () => {
    renderEditor();
    fireEvent.click(document.getElementById('flag-mode-json')!);
    expect(document.getElementById('flag-mode-json')!.getAttribute('aria-pressed')).toBe('true');
    const json = document.getElementById('flag-json') as HTMLTextAreaElement;
    const edited = JSON.parse(json.value);
    edited.fields = ['user_id', 'country'];
    fireEvent.change(json, { target: { value: JSON.stringify(edited, null, 2) } });
    fireEvent.click(document.getElementById('flag-mode-structured')!);
    expect(document.getElementById('flag-mode-structured')!.getAttribute('aria-pressed')).toBe('true');
    expect(document.querySelector<HTMLInputElement>('#flag-fields')!.value).toBe('user_id, country');
  });

  it('validate reports invalid JSON and key mismatches without touching the source', () => {
    renderEditor();
    fireEvent.click(document.getElementById('flag-mode-json')!);
    const json = document.getElementById('flag-json') as HTMLTextAreaElement;
    fireEvent.change(json, { target: { value: '{not json' } });
    fireEvent.click(document.getElementById('flag-validate')!);
    expect(document.getElementById('flag-valid')!.className).toContain('err-text');

    const wrongKey = JSON.parse(JSON.stringify(BASE_DOC));
    wrongKey.key = 'someone-else';
    fireEvent.change(json, { target: { value: JSON.stringify(wrongKey) } });
    fireEvent.click(document.getElementById('flag-validate')!);
    expect(document.getElementById('flag-valid')!.className).toContain('warn-text');

    fireEvent.change(json, { target: { value: JSON.stringify(BASE_DOC) } });
    fireEvent.click(document.getElementById('flag-validate')!);
    expect(document.getElementById('flag-valid')!.className).toContain('ok-text');
  });

  it('snapshot() reflects edits and readSource() stays synchronous after valid()', () => {
    const { ref, onActivity } = renderEditor();
    const before = ref.current!.snapshot();
    const nameInput = document.querySelector<HTMLInputElement>('[data-editor-display-name]')!;
    fireEvent.input(nameInput, { target: { value: 'Dirty now' } });
    expect(onActivity).toHaveBeenCalled();
    expect(ref.current!.snapshot()).not.toBe(before);
    expect(ref.current!.valid()).toBe(true);
    // The detail page reads the source immediately after valid() in the same
    // event; readSource must not wait for React state to propagate.
    expect(source(ref).displayName).toBe('Dirty now');
  });

  it('reportIssues surfaces save-time problems in the validate slot', () => {
    const { ref } = renderEditor();
    act(() => ref.current!.reportIssues(['server said no']));
    expect(document.getElementById('flag-valid')!.textContent).toContain('server said no');
    expect(document.getElementById('flag-valid')!.className).toContain('err-text');
  });

  it('readOnly disables every structured control and the JSON textarea', () => {
    function Host() {
      const [value, setValue] = useState(JSON.stringify(BASE_DOC, null, 2));
      const [risk, setRisk] = useState('normal');
      return (
        <FlagEditor
          flagKey="flag-one"
          value={value}
          onChange={setValue}
          risk={risk}
          onRiskChange={setRisk}
          baseRevision={1}
          readOnly
          segmentKeys={[]}
          onActivity={() => {}}
          onIssuesChange={() => {}}
        />
      );
    }
    render(<ToastProvider><ConfirmProvider><Host /></ConfirmProvider></ToastProvider>);
    const form = document.getElementById('flag-structured')!;
    const controls = form.querySelectorAll('input, select, textarea, button');
    expect(controls.length).toBeGreaterThan(0);
    controls.forEach(c => expect((c as HTMLInputElement).disabled).toBe(true));
    expect((document.getElementById('flag-json') as HTMLTextAreaElement).readOnly).toBe(true);
    expect((document.getElementById('flag-risk') as HTMLSelectElement).disabled).toBe(true);
  });
});
