// FlagEditor ports legacy bindFlagStructured + renderFlagStructured +
// renderFlagRule (web/legacy/flag_editor.js) onto React. The architecture is
// deliberately unchanged: the JSON textarea is the single unsaved source of
// truth, the structured form is uncontrolled and painted once per generation,
// and sync merges the form back onto a deep copy of the parsed source so
// unknown fields survive. Remounts (generation bumps) replace legacy's
// innerHTML redraws; variation renames patch the DOM surgically so select
// selections survive, exactly like the legacy change handler.
import { forwardRef, useEffect, useImperativeHandle, useLayoutEffect, useRef, useState } from 'react';
import type { ReactElement, SyntheticEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../components/Button';
import { useToast } from '../components/Toast';
import { lintConstraints, lintFlag } from '../lint';
import { FLAG_PRESETS, presetFlagContent } from '../presets';
import { PresetPicker } from './PresetPicker';
import { ConditionsEditor, MetadataFields, ReferenceField, TypedValueInput } from './controls';
import { conditionSupported, controlSnapshot, readFlagStructured } from './structuredDom';
import type { FlagDoc } from './structuredDom';

export interface FlagEditorHandle {
  // valid() syncs the structured form into the JSON source; false means
  // issues remain and focus moved to the first invalid control. JSON mode
  // always reports valid (the JSON parse gate lives in the caller).
  valid: () => boolean;
  // redraw() repaints the structured form from the current source — callers
  // that replace the source behind the form (the preset picker) must call it.
  redraw: () => void;
  // snapshot() is the legacy editorValue() flag arm: [kind, source, risk,
  // base, controlSnapshot] serialized for dirty comparison.
  snapshot: () => string;
  // reportIssues() ports reportDocumentIssues: err status + toast + focus.
  reportIssues: (issues: string[]) => void;
  // readSource() returns the current JSON source synchronously; callers that
  // run valid() and immediately re-read must not rely on React state having
  // propagated yet.
  readSource: () => string;
}

export interface FlagEditorProps {
  flagKey: string;
  value: string;
  onChange: (value: string) => void;
  risk: string;
  onRiskChange: (risk: string) => void;
  baseRevision: number;
  readOnly?: boolean;
  segmentKeys: string[];
  onActivity: () => void;
  onIssuesChange: (issues: string[]) => void;
}

interface ValidStatus {
  cls: string;
  text: string;
  alert?: boolean;
}

const DEFAULT_VALUE_BY_TYPE: Record<string, unknown> = { bool: false, string: '', number: 0, json: null };

// The structured form for one generation of the document. Never re-keyed by
// edits: uncontrolled inputs keep their DOM state until a structural change
// (or preset apply / mode re-entry) replaces the whole generation.
function FlagStructuredForm({ doc, generation, readOnly, segmentKeys, issues, formRef, hiddenConditions, onDeferredSync }: {
  doc: FlagDoc;
  generation: number;
  readOnly?: boolean;
  segmentKeys: string[];
  issues: string[];
  formRef: React.RefObject<HTMLDivElement | null>;
  hiddenConditions: ReadonlySet<string>;
  onDeferredSync: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const variations = (doc.variations || {}) as Record<string, unknown>;
  const names = Object.keys(variations);
  const bucketing = (doc.bucketing || {}) as Record<string, unknown>;
  const rules = [((doc.defaultRule || {}) as Record<string, unknown>), ...(((doc.targeting as Record<string, unknown>[]) || []))];

  return (
    <div id="flag-structured" ref={formRef} data-generation={generation}>
      <div className="flag-structured">
        <MetadataFields content={doc} prefix="flag" readOnly={readOnly} />
        <div className="flag-fields">
          <label className="field">
            {t('flags.editorType')}
            <select id="flag-type" defaultValue={String(doc.type ?? 'bool')} disabled={readOnly}>
              {['bool', 'string', 'number', 'json'].map(type => <option key={type} value={type}>{type}</option>)}
            </select>
          </label>
          <label className="inline">
            <input type="checkbox" id="flag-disable" defaultChecked={!!doc.disable} disabled={readOnly} /> {t('flags.editorDisable')}
          </label>
          <label className="field">
            {t('flags.editorUnit')}
            <input id="flag-unit" defaultValue={String(bucketing.unit ?? '')} disabled={readOnly} />
          </label>
          <label className="field">
            {t('flags.editorSalt')}
            <input id="flag-salt" defaultValue={String(bucketing.salt ?? '')} disabled={readOnly} />
          </label>
          <label className="field flag-wide">
            {t('flags.editorFields')}
            <input id="flag-fields" defaultValue={(((doc.fields as string[]) || [])).join(', ')} disabled={readOnly} />
          </label>
        </div>
        <h4>{t('flags.editorVariations')}</h4>
        <div className="flag-variations">
          {names.map((name, index) => (
            <div className="flag-variation" data-variation-row data-original={name} key={index}>
              <label className="field">
                {t('flags.editorVariationName')}
                <input data-var-name defaultValue={name} disabled={readOnly} />
              </label>
              <label className="field grow">
                {t('flags.editorVariationValue')}
                <TypedValueInput value={variations[name]} type={String(doc.type ?? 'bool')} attr="data-var-value" readOnly={readOnly} />
              </label>
              <Button size="sm" data-remove-variation={name} disabled={names.length === 1 || readOnly}>{t('flags.editorRemove')}</Button>
            </div>
          ))}
        </div>
        <Button size="sm" id="flag-add-variation" disabled={readOnly}>{t('flags.editorAddVariation')}</Button>
        <h4>{t('flags.editorRules')}</h4>
        {rules.map((rule, position) => {
          const index = position - 1; // 0 is the default rule (legacy index -1)
          const isDefault = index < 0;
          const percentage = rule.percentage as Record<string, number> | undefined;
          const mode = percentage ? 'percentage' : 'variation';
          return (
            <fieldset className="flag-rule" data-rule-index={index} key={index}>
              <legend>{isDefault ? t('flags.editorDefault') : t('flags.editorTargeting', { n: index + 1 })}</legend>
              {isDefault ? null : (
                <div className="row">
                  <label className="field">
                    {t('flags.editorRuleName')}
                    <input data-rule-name defaultValue={String(rule.name || '')} disabled={readOnly} />
                  </label>
                  <label className="inline">
                    <input type="checkbox" data-rule-disabled defaultChecked={!!rule.disable} disabled={readOnly} /> {t('flags.editorRuleDisabled')}
                  </label>
                  <Button size="sm" data-remove-rule={index} disabled={readOnly}>{t('flags.editorRemove')}</Button>
                </div>
              )}
              <div className="row">
                <label className="field">
                  {t('flags.editorRuleMode')}
                  <select data-rule-mode defaultValue={mode} disabled={readOnly}>
                    <option value="variation">{t('flags.editorFixed')}</option>
                    <option value="percentage">{t('flags.editorPercentage')}</option>
                  </select>
                </label>
                {mode === 'variation' ? (
                  <label className="field">
                    {t('flags.editorServedVariation')}
                    <select data-rule-variation defaultValue={rule.variation === undefined ? undefined : String(rule.variation as string)} disabled={readOnly}>
                      {names.map(name => <option key={name} value={name}>{name}</option>)}
                    </select>
                  </label>
                ) : (
                  <div className="flag-split">
                    {names.map(name => (
                      <label key={name} className="field">
                        {`${name} %`}
                        <input type="number" min={0} max={100} step={0.01} data-split={name} defaultValue={Number(percentage?.[name] ?? 0)} disabled={readOnly} />
                      </label>
                    ))}
                  </div>
                )}
              </div>
              <div className="row">
                <ReferenceField
                  label={t('flags.editorSegments')}
                  labelGrow
                  inputAttr="data-rule-segments"
                  defaultValue={(((rule.segments as string[]) || [])).join(', ')}
                  keys={segmentKeys}
                  listBaseId="flag-structured"
                  index={index + 1}
                  readOnly={readOnly}
                />
              </div>
              <fieldset data-rule-condition-group>
                <legend>{t('editor.conditions')}</legend>
                <ConditionsEditor
                  originals={(rule.constraints as unknown[]) || []}
                  readOnly={readOnly}
                  isHidden={i => hiddenConditions.has(index + ':' + i)}
                  onDeferredSync={onDeferredSync}
                />
              </fieldset>
            </fieldset>
          );
        })}
        <Button size="sm" id="flag-add-rule" disabled={readOnly}>{t('flags.editorAddRule')}</Button>
        {doc.experiment ? <p className="muted">{t('flags.editorManagedExperiment')}</p> : null}
        <div id="flag-structured-status" role="status" aria-live="polite" tabIndex={-1} className={issues.length ? 'warn-text' : 'ok-text'}>
          {issues.length ? <ul>{issues.map((issue, i) => <li key={i}>{issue}</li>)}</ul> : t('validate.ok')}
        </div>
      </div>
    </div>
  );
}

export const FlagEditor = forwardRef<FlagEditorHandle, FlagEditorProps>(function FlagEditor(props, ref): ReactElement {
  const { flagKey, value, onChange, risk, onRiskChange, baseRevision, readOnly, segmentKeys, onActivity, onIssuesChange } = props;
  const { t } = useTranslation();
  const { toast } = useToast();

  // Ref mirrors keep the delegated handlers free of stale closures: legacy
  // attached one listener set at bind time, so every read goes through refs.
  const sourceRef = useRef(value);
  sourceRef.current = value;
  const riskRef = useRef(risk);
  riskRef.current = risk;
  const baseRef = useRef(baseRevision);
  baseRef.current = baseRevision;
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onActivityRef = useRef(onActivity);
  onActivityRef.current = onActivity;
  const onIssuesChangeRef = useRef(onIssuesChange);
  onIssuesChangeRef.current = onIssuesChange;

  const [mode, setMode] = useState<'structured' | 'json'>('structured');
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const [doc, setDoc] = useState<FlagDoc>(() => {
    try {
      return JSON.parse(value);
    } catch {
      return {};
    }
  });
  const [generation, setGeneration] = useState(0);
  const [issues, setIssues] = useState<string[]>([]);
  const issuesRef = useRef<string[]>([]);
  const [validStatus, setValidStatus] = useState<ValidStatus>({ cls: 'muted grow', text: '' });
  const [hiddenConditions, setHiddenConditions] = useState<ReadonlySet<string>>(new Set());
  const formRef = useRef<HTMLDivElement | null>(null);
  const validRef = useRef<HTMLSpanElement | null>(null);
  const lastSyncedRef = useRef('');
  const pendingFocusRef = useRef<(() => void) | null>(null);
  const toastRef = useRef(toast);
  toastRef.current = toast;
  const tRef = useRef(t);
  tRef.current = t;

  const applyIssues = (next: string[]): void => {
    issuesRef.current = next;
    setIssues(next);
  };
  useEffect(() => {
    onIssuesChangeRef.current(issues);
  }, [issues]);

  // legacy redraw(): repaint from the document, recompute lint + per-condition
  // issues, and re-baseline the control snapshot. The snapshot half lands in
  // the layout effect below (it needs the freshly painted DOM).
  const redrawFromDoc = (content: FlagDoc): void => {
    const errs = [
      ...lintFlag(tRef.current, content),
      ...[content.defaultRule, ...((content.targeting as unknown[]) || [])].flatMap(rule =>
        (((rule as Record<string, unknown>)?.constraints || []) as unknown[])
          .filter(conditionSupported)
          .flatMap(c => lintConstraints(tRef.current, [c]))),
    ];
    setDoc(content);
    applyIssues(errs);
    setHiddenConditions(new Set());
    setGeneration(g => g + 1);
  };

  // Initial mount: legacy bindFlagStructured ends with redraw(), which lints
  // the initial document and baselines the control snapshot.
  useLayoutEffect(() => {
    const form = formRef.current;
    if (form) lastSyncedRef.current = controlSnapshot(form, true);
    pendingFocusRef.current?.();
    pendingFocusRef.current = null;
  }, [generation]);
  const mountedOnce = useRef(false);
  useLayoutEffect(() => {
    if (mountedOnce.current) return;
    mountedOnce.current = true;
    let initial: FlagDoc;
    try {
      initial = JSON.parse(sourceRef.current);
    } catch {
      return;
    }
    applyIssues([
      ...lintFlag(tRef.current, initial),
      ...[initial.defaultRule, ...((initial.targeting as unknown[]) || [])].flatMap(rule =>
        (((rule as Record<string, unknown>)?.constraints || []) as unknown[])
          .filter(conditionSupported)
          .flatMap(c => lintConstraints(tRef.current, [c]))),
    ]);
  }, []);

  // Focus + scroll the validation output when a save/gff attempt reports
  // document issues (reportDocumentIssues).
  useEffect(() => {
    if (!validStatus.alert || !validRef.current) return;
    validRef.current.scrollIntoView?.({ block: 'nearest' });
    validRef.current.focus?.();
  }, [validStatus]);

  const writeSource = (content: FlagDoc): void => {
    const text = JSON.stringify(content, null, 2);
    sourceRef.current = text;
    onChangeRef.current(text);
  };

  const sync = (): boolean => {
    const form = formRef.current;
    if (!form) return true;
    const snapshotNow = controlSnapshot(form, true);
    if (!issuesRef.current.length && snapshotNow === lastSyncedRef.current) return true;
    let base: FlagDoc;
    try {
      base = JSON.parse(sourceRef.current);
    } catch {
      return false;
    }
    const result = readFlagStructured(form, base, tRef.current, 'flag-structured-status');
    applyIssues(result.issues);
    if (!result.issues.length) {
      writeSource(result.content);
      lastSyncedRef.current = snapshotNow;
    }
    return !result.issues.length;
  };
  const syncRef = useRef(sync);
  syncRef.current = sync;

  const deferredSync = (): void => {
    setTimeout(() => {
      syncRef.current();
      onActivityRef.current();
    }, 0);
  };
  const deferredSyncRef = useRef(deferredSync);
  deferredSyncRef.current = deferredSync;

  // Delegated form listeners, mirroring legacy's trio on #flag-structured.
  const onFormInput = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    if (!target.matches('[data-var-name], [data-rule-mode], [data-condition-type], #flag-type')) {
      syncRef.current();
    }
    onActivityRef.current();
  };

  const onFormChange = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    // The condition op/type coupling lives in ConditionRow (it remounts the
    // value control); the deferred sync it schedules reads the new control.
    if (target.matches('[data-condition-op], [data-condition-type]')) {
      onActivityRef.current();
      return;
    }
    if (target.id === 'flag-type') {
      const select = target as HTMLSelectElement;
      const requested = select.value;
      let content: FlagDoc;
      try {
        content = JSON.parse(sourceRef.current);
      } catch {
        return;
      }
      select.value = String(content.type ?? 'bool');
      if (!syncRef.current()) {
        onActivityRef.current();
        return;
      }
      const updated: FlagDoc = JSON.parse(sourceRef.current);
      updated.type = requested;
      updated.variations = Object.fromEntries(Object.entries((updated.variations || {}) as Record<string, unknown>).map(([name, val]) => {
        if (requested === 'string') return [name, typeof val === 'object' ? JSON.stringify(val) : String(val)];
        if (requested === 'number' && typeof val === 'string' && val.trim() && Number.isFinite(Number(val))) return [name, Number(val)];
        if (requested === 'bool' && ['true', 'false'].includes(val as string)) return [name, val === 'true'];
        return [name, val];
      }));
      writeSource(updated);
      redrawFromDoc(updated);
      onActivityRef.current();
      return;
    }
    if (target.matches('[data-var-name]')) {
      const row = target.closest<HTMLElement>('[data-variation-row]')!;
      const oldName = row.dataset.original!;
      const newName = (target as HTMLInputElement).value.trim();
      if (syncRef.current()) {
        // Surgical rename: patch split labels, served-variation options and
        // the remove button in place so open selects keep their selection.
        row.dataset.original = newName;
        formRef.current?.querySelectorAll<HTMLInputElement>('[data-split]').forEach(input => {
          if (input.dataset.split === oldName) {
            input.dataset.split = newName;
            if (input.parentElement?.firstChild) input.parentElement.firstChild.textContent = newName + ' %';
          }
        });
        formRef.current?.querySelectorAll<HTMLOptionElement>('[data-rule-variation] option').forEach(option => {
          if (option.value === oldName) {
            option.value = newName;
            option.textContent = newName;
          }
        });
        const remove = row.querySelector<HTMLElement>('[data-remove-variation]');
        if (remove) remove.dataset.removeVariation = newName;
      }
      onActivityRef.current();
      return;
    }
    if (target.matches('[data-rule-mode]')) {
      const select = target as HTMLSelectElement;
      const row = target.closest<HTMLElement>('[data-rule-index]')!;
      const index = Number(row.dataset.ruleIndex);
      const requestedMode = select.value;
      let content: FlagDoc;
      try {
        content = JSON.parse(sourceRef.current);
      } catch {
        return;
      }
      const rules = (content.targeting || []) as Record<string, unknown>[];
      const rule = (index < 0 ? content.defaultRule : rules[index]) as Record<string, unknown> | undefined;
      select.value = rule?.percentage ? 'percentage' : 'variation';
      if (!syncRef.current()) {
        onActivityRef.current();
        return;
      }
      const updated: FlagDoc = JSON.parse(sourceRef.current);
      const updatedRules = (updated.targeting || []) as Record<string, unknown>[];
      const updatedRule = (index < 0 ? updated.defaultRule : updatedRules[index]) as Record<string, unknown>;
      const names = Object.keys((updated.variations || {}) as Record<string, unknown>);
      if (requestedMode === 'percentage') {
        delete updatedRule.variation;
        updatedRule.percentage = Object.fromEntries(names.map((name, i) => [name, i === 0 ? 100 : 0]));
      } else {
        delete updatedRule.percentage;
        updatedRule.variation = names[0] || '';
      }
      writeSource(updated);
      redrawFromDoc(updated);
      onActivityRef.current();
      return;
    }
    syncRef.current();
    onActivityRef.current();
  };

  const onFormClick = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    const conditionButton = target.closest<HTMLElement>('[data-editor-add-condition], [data-editor-remove-condition]');
    if (conditionButton) {
      const row = conditionButton.closest<HTMLElement>('[data-rule-index]')!;
      const ruleIndex = Number(row.dataset.ruleIndex);
      if (conditionButton.matches('[data-editor-remove-condition]')) {
        const condRow = conditionButton.closest<HTMLElement>('[data-editor-condition]')!;
        const condIndex = Number(condRow.dataset.editorCondition);
        // React unmounts the row first; the deferred sync then reads the DOM
        // without it — the legacy ordering (node removal before sync).
        setHiddenConditions(prev => new Set(prev).add(ruleIndex + ':' + condIndex));
        setTimeout(() => {
          const focusAdd = (): void => {
            formRef.current?.querySelector<HTMLElement>('[data-rule-index="' + ruleIndex + '"] [data-editor-add-condition]')?.focus();
          };
          if (syncRef.current()) {
            // The remount replaces the button node, so focus lands after the
            // generation layout effect via pendingFocusRef.
            pendingFocusRef.current = focusAdd;
            redrawFromDoc(JSON.parse(sourceRef.current));
          } else {
            focusAdd();
          }
          onActivityRef.current();
        }, 0);
        return;
      }
      const form = formRef.current!;
      if (controlSnapshot(form, true) !== lastSyncedRef.current && !syncRef.current()) {
        onActivityRef.current();
        return;
      }
      const content: FlagDoc = JSON.parse(sourceRef.current);
      const rule = (ruleIndex < 0 ? content.defaultRule : ((content.targeting || []) as Record<string, unknown>[])[ruleIndex]) as Record<string, unknown>;
      rule.constraints = [...((rule.constraints as unknown[]) || []), { field: '', op: 'eq', value: '' }];
      pendingFocusRef.current = () => {
        const changed = formRef.current?.querySelector('[data-rule-index="' + ruleIndex + '"]');
        const fields = changed?.querySelectorAll<HTMLElement>('[data-condition-field]');
        fields?.[fields.length - 1]?.focus();
      };
      writeSource(content);
      redrawFromDoc(content);
      onActivityRef.current();
      return;
    }
    const removeVariation = target.closest<HTMLElement>('[data-remove-variation]');
    const removeRule = target.closest<HTMLElement>('[data-remove-rule]');
    const addVariation = target.closest('#flag-add-variation');
    const addRule = target.closest('#flag-add-rule');
    if (!removeVariation && !removeRule && !addVariation && !addRule) return;
    if (!syncRef.current()) {
      onActivityRef.current();
      return;
    }
    const content: FlagDoc = JSON.parse(sourceRef.current);
    const variations = (content.variations || {}) as Record<string, unknown>;
    if (removeVariation) {
      const removed = removeVariation.dataset.removeVariation!;
      delete variations[removed];
      const first = Object.keys(variations)[0];
      if (!first) return;
      const repair = (rule: Record<string, unknown> | undefined): void => {
        if (!rule) return;
        if (rule.variation === removed) rule.variation = first;
        const percentage = rule.percentage as Record<string, number> | undefined;
        if (!percentage) return;
        delete percentage[removed];
        const total = Object.values(percentage).reduce((sum, val) => sum + val, 0);
        if (!total) percentage[first] = 100;
        else for (const name of Object.keys(percentage)) percentage[name] = 100 * percentage[name] / total;
      };
      repair(content.defaultRule as Record<string, unknown>);
      ((content.targeting || []) as Record<string, unknown>[]).forEach(repair);
    }
    if (removeRule) ((content.targeting || []) as unknown[]).splice(Number(removeRule.dataset.removeRule), 1);
    if (addVariation) {
      let index = 1;
      while (Object.hasOwn(variations, 'variation' + index)) index++;
      variations['variation' + index] = DEFAULT_VALUE_BY_TYPE[String(content.type)] ?? null;
      content.variations = variations;
    }
    if (addRule) {
      const targeting = ((content.targeting || []) as Record<string, unknown>[]);
      targeting.push({ name: '', variation: Object.keys(variations)[0] || '' });
      content.targeting = targeting;
    }
    writeSource(content);
    redrawFromDoc(content);
    onActivityRef.current();
  };

  const toStructured = (): void => {
    if (modeRef.current === 'structured') return;
    let content: FlagDoc;
    try {
      content = JSON.parse(sourceRef.current);
    } catch (error) {
      toastRef.current(tRef.current('toast.invalidJson', { msg: (error as Error).message }), false);
      return;
    }
    const errs = lintFlag(tRef.current, content);
    if (errs.length) {
      toastRef.current(errs.join(' · '), false);
      return;
    }
    setMode('structured');
    redrawFromDoc(content);
  };
  const toJson = (): void => {
    if (modeRef.current === 'json') return;
    if (!syncRef.current()) return;
    setMode('json');
  };

  const validate = (): void => {
    let c: FlagDoc;
    try {
      c = JSON.parse(sourceRef.current);
    } catch (error) {
      setValidStatus({ cls: 'err-text grow', text: tRef.current('validate.invalidJson', { msg: (error as Error).message }) });
      return;
    }
    const errs = lintFlag(tRef.current, c);
    if (!c || c.key !== flagKey) errs.unshift(tRef.current('toast.keyMismatch', { key: flagKey }));
    if (errs.length) setValidStatus({ cls: 'warn-text grow', text: '⚠ ' + errs.join(' · ') });
    else setValidStatus({ cls: 'ok-text grow', text: tRef.current('validate.ok') });
  };

  useImperativeHandle(ref, () => ({
    valid: () => {
      if (modeRef.current === 'json') return true;
      if (syncRef.current()) return true;
      const form = formRef.current;
      ((form?.querySelector('[aria-invalid="true"]') as HTMLElement | null) ||
        form?.querySelector<HTMLElement>('#flag-structured-status'))?.focus();
      return false;
    },
    redraw: () => {
      try {
        redrawFromDoc(JSON.parse(sourceRef.current));
      } catch {
        // An unparseable source cannot be painted; the JSON pane shows it.
      }
    },
    snapshot: () => JSON.stringify([
      'flag',
      sourceRef.current,
      riskRef.current,
      String(baseRef.current),
      formRef.current ? JSON.parse(controlSnapshot(formRef.current, true)) : [],
    ]),
    reportIssues: (list: string[]) => {
      setValidStatus({ cls: 'err-text grow', text: list.join(' · '), alert: true });
      toastRef.current(list.join(' · '), false);
    },
    readSource: () => sourceRef.current,
  }), []);

  return (
    <>
      <div className="row flag-editor-modes" role="group" aria-label={t('flags.editorMode')}>
        <Button size="sm" id="flag-mode-structured" aria-pressed={mode === 'structured'} onClick={toStructured}>
          {t('flags.editorStructured')}
        </Button>
        <Button size="sm" id="flag-mode-json" aria-pressed={mode === 'json'} onClick={toJson}>
          {t('flags.editorAdvanced')}
        </Button>
      </div>
      {readOnly ? null : (
        <div className="row">
          <PresetPicker
            id="flag-preset"
            presets={FLAG_PRESETS}
            build={id => presetFlagContent(id, flagKey)}
            currentSource={() => sourceRef.current}
            onApply={next => {
              writeSource(next);
              redrawFromDoc(next);
              onActivityRef.current();
            }}
          />
          <span className="muted">{t('preset.applyHint')}</span>
        </div>
      )}
      <div className={mode === 'json' ? 'hidden' : ''} onInput={onFormInput} onChange={onFormChange} onClick={onFormClick}>
        <FlagStructuredForm
          key={generation}
          doc={doc}
          generation={generation}
          readOnly={readOnly}
          segmentKeys={segmentKeys}
          issues={issues}
          formRef={formRef}
          hiddenConditions={hiddenConditions}
          onDeferredSync={() => deferredSyncRef.current()}
        />
      </div>
      <div id="flag-advanced" className={mode === 'structured' ? 'hidden' : ''}>
        <p className="muted">{t('flags.editorAdvancedHint')}</p>
        <textarea
          id="flag-json"
          className="json"
          rows={24}
          spellCheck={false}
          value={value}
          readOnly={readOnly}
          onChange={event => {
            // The textarea IS the unsaved source of truth (legacy editor
            // contract): write it into sourceRef eagerly so readSource()/
            // snapshot() — and the dirty check onActivity triggers — see the
            // new text now, not after the parent re-render lands.
            sourceRef.current = event.target.value;
            onChangeRef.current(event.target.value);
            onActivityRef.current();
          }}
        />
      </div>
      <div className="row">
        <Button size="sm" id="flag-validate" onClick={validate}>{t('btn.validateJson')}</Button>
        <span
          id="flag-valid"
          className={validStatus.cls}
          role={validStatus.alert ? 'alert' : undefined}
          tabIndex={validStatus.alert ? -1 : undefined}
          ref={validRef}
        >
          {validStatus.text}
        </span>
        <label className="inline">
          {t('label.risk')}{' '}
          <select
            id="flag-risk"
            value={risk}
            disabled={readOnly}
            onChange={event => {
              onRiskChange(event.target.value);
              onActivityRef.current();
            }}
          >
            <option value="normal">{t('diagnostic.normal')}</option>
            <option value="high">{t('diagnostic.high')}</option>
          </select>
        </label>
      </div>
    </>
  );
});
