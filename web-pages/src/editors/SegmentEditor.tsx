// SegmentEditor ports legacy bindSegmentStructured + renderSegmentStructured
// (web/legacy/segment_editor.js). Same architecture as FlagEditor: the JSON
// textarea is the single unsaved source of truth, the structured form is
// uncontrolled per generation, and sync merges the form onto a deep copy of
// the parsed source. Legacy asymmetries preserved: readSegmentStructured
// skips lintSegment entirely when a field-level issue exists, and the dirty
// snapshot tracks control values only (no checkbox pairs).
import { forwardRef, useEffect, useImperativeHandle, useLayoutEffect, useRef, useState } from 'react';
import type { ReactElement, SyntheticEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../components/Button';
import { useToast } from '../components/Toast';
import { lintSegment } from '../lint';
import { SEGMENT_PRESETS, presetSegmentContent } from '../presets';
import { PresetPicker } from './PresetPicker';
import { ConditionsEditor, MetadataFields, ReferenceField } from './controls';
import { controlSnapshot, readSegmentStructured } from './structuredDom';
import type { FlagDoc } from './structuredDom';

export interface SegmentEditorHandle {
  valid: () => boolean;
  redraw: () => void;
  snapshot: () => string;
  reportIssues: (issues: string[]) => void;
  readSource: () => string;
}

export interface SegmentEditorProps {
  segKey: string;
  value: string;
  onChange: (value: string) => void;
  risk: string;
  onRiskChange: (risk: string) => void;
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

function SegmentStructuredForm({ doc, generation, readOnly, segmentKeys, issues, formRef, hiddenConditions, onDeferredSync }: {
  doc: FlagDoc;
  generation: number;
  readOnly?: boolean;
  segmentKeys: string[];
  issues: string[];
  formRef: React.RefObject<HTMLDivElement | null>;
  hiddenConditions: ReadonlySet<number>;
  onDeferredSync: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const constraints = Array.isArray(doc.constraints) ? doc.constraints : [];
  const segments = Array.isArray(doc.segments) ? (doc.segments as string[]) : [];
  return (
    <div id="seg-structured" ref={formRef} data-generation={generation}>
      <div className="segment-structured">
        <MetadataFields content={doc} prefix="seg" readOnly={readOnly} />
        <p className="muted">{t('seg.editorSemantics')}</p>
        <h4>{t('seg.editorConditions')}</h4>
        <div id="seg-conditions">
          <ConditionsEditor
            originals={constraints}
            readOnly={readOnly}
            isHidden={i => hiddenConditions.has(i)}
            onDeferredSync={onDeferredSync}
          />
        </div>
        <ReferenceField
          label={t('seg.editorReferences')}
          inputId="seg-references"
          defaultValue={segments.join(', ')}
          keys={segmentKeys}
          listBaseId="seg-structured"
          index={0}
          readOnly={readOnly}
        />
        <div id="seg-structured-status" role="status" aria-live="polite" tabIndex={-1} className={issues.length ? 'warn-text' : 'ok-text'}>
          {issues.length ? <ul>{issues.map((issue, i) => <li key={i}>{issue}</li>)}</ul> : t('validate.ok')}
        </div>
      </div>
    </div>
  );
}

export const SegmentEditor = forwardRef<SegmentEditorHandle, SegmentEditorProps>(function SegmentEditor(props, ref): ReactElement {
  const { segKey, value, onChange, risk, onRiskChange, readOnly, segmentKeys, onActivity, onIssuesChange } = props;
  const { t } = useTranslation();
  const { toast } = useToast();

  const sourceRef = useRef(value);
  sourceRef.current = value;
  const riskRef = useRef(risk);
  riskRef.current = risk;
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onActivityRef = useRef(onActivity);
  onActivityRef.current = onActivity;
  const onIssuesChangeRef = useRef(onIssuesChange);
  onIssuesChangeRef.current = onIssuesChange;
  const toastRef = useRef(toast);
  toastRef.current = toast;
  const tRef = useRef(t);
  tRef.current = t;

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
  const [hiddenConditions, setHiddenConditions] = useState<ReadonlySet<number>>(new Set());
  const formRef = useRef<HTMLDivElement | null>(null);
  const validRef = useRef<HTMLSpanElement | null>(null);
  const lastSyncedRef = useRef('');
  const pendingFocusRef = useRef<(() => void) | null>(null);

  const applyIssues = (next: string[]): void => {
    issuesRef.current = next;
    setIssues(next);
  };
  useEffect(() => {
    onIssuesChangeRef.current(issues);
  }, [issues]);

  const redrawFromDoc = (content: FlagDoc): void => {
    setDoc(content);
    applyIssues(lintSegment(tRef.current, content));
    setHiddenConditions(new Set());
    setGeneration(g => g + 1);
  };

  useLayoutEffect(() => {
    const form = formRef.current;
    if (form) lastSyncedRef.current = controlSnapshot(form, false);
    pendingFocusRef.current?.();
    pendingFocusRef.current = null;
  }, [generation]);
  const mountedOnce = useRef(false);
  useLayoutEffect(() => {
    if (mountedOnce.current) return;
    mountedOnce.current = true;
    try {
      applyIssues(lintSegment(tRef.current, JSON.parse(sourceRef.current)));
    } catch {
      // Unparseable initial source: the JSON pane reports it on validate.
    }
  }, []);

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
    const snapshotNow = controlSnapshot(form, false);
    if (!issuesRef.current.length && snapshotNow === lastSyncedRef.current) return true;
    let base: FlagDoc;
    try {
      base = JSON.parse(sourceRef.current);
    } catch {
      return false;
    }
    const result = readSegmentStructured(form, base, tRef.current, 'seg-structured-status');
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

  const onFormInput = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    if (!target.matches('[data-condition-type]')) syncRef.current();
    onActivityRef.current();
  };

  const onFormChange = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    if (target.matches('[data-condition-op], [data-condition-type]')) {
      onActivityRef.current();
      return;
    }
    syncRef.current();
    onActivityRef.current();
  };

  const onFormClick = (event: SyntheticEvent): void => {
    const target = event.target as HTMLElement;
    const remove = target.closest<HTMLElement>('[data-editor-remove-condition]');
    const add = target.closest('[data-editor-add-condition]');
    if (!remove && !add) return;
    if (remove) {
      const condRow = remove.closest<HTMLElement>('[data-editor-condition]')!;
      const condIndex = Number(condRow.dataset.editorCondition);
      setHiddenConditions(prev => new Set(prev).add(condIndex));
      setTimeout(() => {
        const focusAdd = (): void => {
          formRef.current?.querySelector<HTMLElement>('[data-editor-add-condition]')?.focus();
        };
        if (syncRef.current()) {
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
    if (controlSnapshot(form, false) !== lastSyncedRef.current && !syncRef.current()) {
      onActivityRef.current();
      return;
    }
    const content: FlagDoc = JSON.parse(sourceRef.current);
    content.constraints = [...((content.constraints as unknown[]) || []), { field: '', op: 'eq', value: '' }];
    pendingFocusRef.current = () => {
      const fields = formRef.current?.querySelectorAll<HTMLElement>('[data-condition-field]');
      fields?.[fields.length - 1]?.focus();
    };
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
    const errs = lintSegment(tRef.current, content);
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

  const validInternal = (): boolean => {
    if (modeRef.current === 'json') return true;
    if (syncRef.current()) return true;
    const form = formRef.current;
    ((form?.querySelector('[aria-invalid="true"]') as HTMLElement | null) ||
      form?.querySelector<HTMLElement>('#seg-structured-status'))?.focus();
    return false;
  };

  // Legacy seg-validate runs structured.valid() first, then lints the source.
  const validate = (): void => {
    if (!validInternal()) return;
    try {
      const c = JSON.parse(sourceRef.current) as FlagDoc;
      const errs = lintSegment(tRef.current, c);
      const cls = c && c.key === segKey && !errs.length ? 'ok-text grow' : 'warn-text grow';
      const text = !c || c.key !== segKey
        ? tRef.current('validate.keyShould', { key: segKey })
        : errs.length
          ? errs.join(' · ')
          : tRef.current('validate.valid');
      setValidStatus({ cls, text });
    } catch (error) {
      setValidStatus({ cls: 'err-text grow', text: tRef.current('validate.invalidJson', { msg: (error as Error).message }) });
    }
  };

  useImperativeHandle(ref, () => ({
    valid: validInternal,
    redraw: () => {
      try {
        redrawFromDoc(JSON.parse(sourceRef.current));
      } catch {
        // An unparseable source cannot be painted; the JSON pane shows it.
      }
    },
    snapshot: () => JSON.stringify([
      'segment',
      sourceRef.current,
      riskRef.current,
      formRef.current ? JSON.parse(controlSnapshot(formRef.current, false)) : [],
    ]),
    reportIssues: (list: string[]) => {
      setValidStatus({ cls: 'err-text grow', text: list.join(' · '), alert: true });
      toastRef.current(list.join(' · '), false);
    },
    readSource: () => sourceRef.current,
  }), []);

  return (
    <>
      <div className="row flag-editor-modes" role="group" aria-label={t('seg.editorMode')}>
        <Button size="sm" id="seg-mode-structured" aria-pressed={mode === 'structured'} onClick={toStructured}>
          {t('flags.editorStructured')}
        </Button>
        <Button size="sm" id="seg-mode-json" aria-pressed={mode === 'json'} onClick={toJson}>
          {t('flags.editorAdvanced')}
        </Button>
      </div>
      {readOnly ? null : (
        <div className="row">
          <PresetPicker
            id="seg-preset"
            presets={SEGMENT_PRESETS}
            build={id => presetSegmentContent(id, segKey)}
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
        <SegmentStructuredForm
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
      <div id="seg-advanced" className={mode === 'structured' ? 'hidden' : ''}>
        <textarea
          id="seg-json"
          className="json"
          rows={18}
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
        <Button size="sm" id="seg-validate" onClick={validate}>{t('btn.validateJson')}</Button>
        <span
          id="seg-valid"
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
            id="seg-risk"
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
