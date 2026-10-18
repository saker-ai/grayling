// controls.tsx hosts the React skins for the legacy structured-editor
// widgets. Everything here stays uncontrolled (defaultValue/defaultChecked)
// so the DOM remains the live editing surface and sync reads it, exactly like
// legacy. The single exception is the condition value-type switch, which
// legacy performed by replacing the control node; React does it with per-row
// state plus a deferred sync so the remounted control is the one read.
import { useEffect, useRef, useState } from 'react';
import type { ChangeEvent, ReactElement, RefObject } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../components/Button';
import { OPS } from '../lint';
import { conditionSupported, conditionValueType } from './structuredDom';
import type { FlagDoc } from './structuredDom';

const TYPE_LABEL_KEYS: Record<string, string> = {
  string: 'editor.text',
  number: 'editor.number',
  bool: 'editor.boolean',
  list: 'editor.list',
  json: 'editor.advanced',
};

// editorTypedValue: bool renders an explicit-choice select (an invalid stored
// value gets the empty placeholder option selected); other types render an
// input whose data-value-type sync reads back.
export function TypedValueInput({ value, type, attr, readOnly, inputRef }: {
  value: unknown;
  type: string;
  attr: string;
  readOnly?: boolean;
  inputRef?: RefObject<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement | null>;
}): ReactElement {
  const { t } = useTranslation();
  if (type === 'bool') {
    const valid = value === true || value === false;
    return (
      <select {...{ [attr]: true }} ref={inputRef as RefObject<HTMLSelectElement | null>} defaultValue={valid ? String(value) : ''} disabled={readOnly}>
        {valid ? null : <option value="">{t('editor.chooseValue')}</option>}
        <option value="true">true</option>
        <option value="false">false</option>
      </select>
    );
  }
  return (
    <input
      {...{ [attr]: true }}
      data-value-type={type}
      inputMode={type === 'number' ? 'decimal' : undefined}
      defaultValue={type === 'string' || type === 'number' ? String((value ?? '') as string | number) : JSON.stringify(value)}
      readOnly={readOnly}
      disabled={readOnly}
      ref={inputRef as RefObject<HTMLInputElement | null>}
    />
  );
}

// editorMetadataMarkup: displayName + description above both editors.
export function MetadataFields({ content, prefix, readOnly }: {
  content: FlagDoc;
  prefix: string;
  readOnly?: boolean;
}): ReactElement {
  const { t } = useTranslation();
  return (
    <div className="editor-metadata">
      <p className="muted" id={prefix + '-metadata-help'}>{t('editor.metadataHelp')}</p>
      <label className="field">
        {t('editor.displayName')}
        <input
          data-editor-display-name
          aria-describedby={prefix + '-metadata-help'}
          defaultValue={typeof content.displayName === 'string' ? content.displayName : ''}
          disabled={readOnly}
        />
      </label>
      <label className="field">
        {t('editor.description')}
        <textarea
          data-editor-description
          rows={3}
          aria-describedby={prefix + '-metadata-help'}
          defaultValue={typeof content.description === 'string' ? content.description : ''}
          disabled={readOnly}
        />
      </label>
    </div>
  );
}

// Writing through the native setter keeps the input uncontrolled while still
// letting React's delegated onInput/onChange observers see the change, which
// is how the legacy picker kept the form's sync loop in the loop.
export function setNativeValue(el: HTMLInputElement, value: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function parseReferenceList(value: string): string[] {
  return [...new Set(value.split(',').map(key => key.trim()).filter(Boolean))];
}

// editorBindReferences: the manual comma input stays in place inside its
// label; the search + datalist + chips picker is its sibling (legacy inserts
// the group after the label — nesting it inside would make label activation
// steal focus from the search box). Chip edits write back into the manual
// input (dispatching input) so the editor's sync loop sees them.
export function ReferenceField({ label, labelGrow, inputAttr, inputId, defaultValue, keys, listBaseId, index, readOnly }: {
  label: string;
  labelGrow?: boolean;
  inputAttr?: string;
  inputId?: string;
  defaultValue: string;
  keys: string[];
  listBaseId: string;
  index: number;
  readOnly?: boolean;
}): ReactElement {
  const { t } = useTranslation();
  const inputRef = useRef<HTMLInputElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const [chips, setChips] = useState<string[]>(() => parseReferenceList(defaultValue));
  const listID = listBaseId + '-segment-options-' + index;

  const add = (): void => {
    const key = searchRef.current?.value.trim() || '';
    if (!key) {
      searchRef.current?.focus();
      return;
    }
    const next = [...new Set([...parseReferenceList(inputRef.current?.value || ''), key])];
    if (inputRef.current) setNativeValue(inputRef.current, next.join(', '));
    if (searchRef.current) searchRef.current.value = '';
    searchRef.current?.focus();
  };
  const remove = (key: string): void => {
    const next = parseReferenceList(inputRef.current?.value || '').filter(item => item !== key);
    if (inputRef.current) setNativeValue(inputRef.current, next.join(', '));
    searchRef.current?.focus();
  };

  return (
    <>
      <label className={labelGrow ? 'field grow' : 'field'}>
        {label}
        <input
          {...(inputAttr ? { [inputAttr]: true } : {})}
          id={inputId}
          ref={inputRef}
          defaultValue={defaultValue}
          disabled={readOnly}
          onInput={event => setChips(parseReferenceList((event.target as HTMLInputElement).value))}
        />
      </label>
      <div className="reference-picker">
        <label className="field">
          {t('editor.searchSegments')}
          <input
            type="search"
            data-reference-search
            list={listID}
            autoComplete="off"
            ref={searchRef}
            disabled={readOnly}
            onKeyDown={event => {
              if (event.key === 'Enter') {
                event.preventDefault();
                add();
              }
            }}
          />
        </label>
        <datalist id={listID}>
          {keys.map(key => <option key={key} value={key} />)}
        </datalist>
        <Button size="sm" data-reference-add onClick={add} disabled={readOnly}>{t('editor.addReference')}</Button>
        <div className="row" data-reference-selected>
          {chips.map(key => (
            <button
              key={key}
              type="button"
              className="btn sm"
              data-reference-remove={key}
              aria-label={t('editor.removeReference', { key })}
              onClick={() => remove(key)}
              disabled={readOnly}
            >
              {key + ' ×'}
            </button>
          ))}
        </div>
      </div>
    </>
  );
}

// One condition row. Rows backed by an unsupported shape keep the raw JSON
// textarea; supported rows expose field/op/type/value controls. The value
// control is keyed by its type so a type switch remounts it, carrying the
// previous raw text across exactly like legacy's outerHTML replacement.
function ConditionRow({ index, original, readOnly, onDeferredSync }: {
  index: number;
  original: unknown;
  readOnly?: boolean;
  onDeferredSync: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const valueRef = useRef<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement | null>(null);
  const focusPending = useRef(false);
  const [vtype, setVtype] = useState<string>(() => (conditionSupported(original) ? conditionValueType(original) : 'json'));
  const [carried, setCarried] = useState<unknown>(() => {
    if (!conditionSupported(original)) return original;
    return original.op === 'in' || original.op === 'not_in'
      ? ((original.values as string[]) || []).join('\n')
      : original.value;
  });
  // Legacy focuses the freshly replaced value control after a type switch.
  useEffect(() => {
    if (focusPending.current) {
      focusPending.current = false;
      valueRef.current?.focus();
    }
  }, [vtype]);

  if (!conditionSupported(original)) {
    return (
      <label className="field" data-editor-condition={index}>
        {t('editor.advanced')}
        <textarea className="json" data-condition-json rows={3} defaultValue={JSON.stringify(original, null, 2)} disabled={readOnly} />
      </label>
    );
  }

  const switchType = (nextType: string): void => {
    const oldText = valueRef.current ? valueRef.current.value : '';
    setCarried(nextType === 'bool' ? (oldText === 'true' ? true : oldText === 'false' ? false : oldText) : oldText);
    setVtype(nextType);
    focusPending.current = true;
    onDeferredSync();
  };
  const onOpChange = (event: ChangeEvent<HTMLSelectElement>): void => {
    const op = event.target.value;
    if ((op === 'in' || op === 'not_in') && vtype !== 'list' && vtype !== 'json') switchType('list');
    else if (op !== 'in' && op !== 'not_in' && vtype === 'list') switchType('string');
    else onDeferredSync();
  };

  let valueControl: ReactElement;
  if (vtype === 'list') {
    valueControl = (
      <textarea data-condition-value data-value-type="list" rows={3} defaultValue={String((carried ?? '') as string)} disabled={readOnly} ref={valueRef as RefObject<HTMLTextAreaElement | null>} />
    );
  } else {
    valueControl = <TypedValueInput value={carried} type={vtype} attr="data-condition-value" readOnly={readOnly} inputRef={valueRef} />;
  }

  return (
    <div className="segment-condition" data-editor-condition={index}>
      <label className="field">
        {t('seg.editorField')}
        <input data-condition-field defaultValue={String((original.field ?? '') as string)} disabled={readOnly} />
      </label>
      <label className="field">
        {t('seg.editorOperator')}
        <select data-condition-op data-seg-op defaultValue={String(original.op as string)} disabled={readOnly} onChange={onOpChange}>
          {OPS.map(op => <option key={op} value={op}>{op}</option>)}
        </select>
      </label>
      <label className="field">
        {t('editor.valueType')}
        <select data-condition-type value={vtype} disabled={readOnly} onChange={event => switchType(event.target.value)}>
          {['string', 'number', 'bool', 'list', 'json'].map(v => <option key={v} value={v}>{t(TYPE_LABEL_KEYS[v])}</option>)}
        </select>
      </label>
      <label className="field grow">
        {t('seg.editorValue')}
        {valueControl}
      </label>
      <Button size="sm" data-editor-remove-condition disabled={readOnly}>{t('flags.editorRemove')}</Button>
    </div>
  );
}

// The condition list for one rule (or the segment itself). Structural
// add/remove is handled by the editor's delegated click listener (the buttons
// carry the legacy data attributes), so this component only needs to know
// which rows are hidden pending a failed sync, and how to schedule the
// deferred sync after a value-type switch remounts a control.
export function ConditionsEditor({ originals, readOnly, isHidden, onDeferredSync }: {
  originals: unknown[];
  readOnly?: boolean;
  isHidden?: (index: number) => boolean;
  onDeferredSync: () => void;
}): ReactElement {
  const { t } = useTranslation();
  return (
    <>
      <div data-editor-conditions>
        {originals.map((original, index) =>
          (isHidden?.(index) ? null : (
            <ConditionRow
              key={index}
              index={index}
              original={original}
              readOnly={readOnly}
              onDeferredSync={onDeferredSync}
            />
          )))}
      </div>
      <Button size="sm" data-editor-add-condition disabled={readOnly}>{t('editor.addCondition')}</Button>
    </>
  );
}
