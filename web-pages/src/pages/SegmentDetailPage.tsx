// SegmentDetailPage ports legacy viewSegmentDetail (web/legacy/app.js
// 1824-1977) plus the segment_editor.js renderers: list-first data flow
// (find the key in GET {scope}/segments, then allSettled for preview /
// dependencies / history), the draft editor with the same JSON-source-of-truth
// contract as the flag editor, the match probe, the dependencies list with
// dirty-guarded navigation, and the revision history with its compare select.
// Segment asymmetries preserved: no draft-state gating on the four action
// buttons, save carries {content, risk} only, approve needs no dialog, submit
// collects the reason before the dirty guard fires, and the risk line shows
// the raw risk value (flags use diagnosticLabel).
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import type { ReviewPreview as ReviewPreviewData, SegmentDependencies, SegmentDraft, SegmentHistory } from '@saker/grayling-client';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { useConfirm } from '../components/ConfirmDialog';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { ReviewPreview } from '../components/ReviewPreview';
import { SegmentMatchProbe } from '../components/SegmentMatchProbe';
import type { SegmentMatchProbeHandle } from '../components/SegmentMatchProbe';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { EditorDirtyProvider, useEditorDirty } from '../editors/EditorDirtyContext';
import { SegmentEditor } from '../editors/SegmentEditor';
import type { SegmentEditorHandle } from '../editors/SegmentEditor';
import { StickyActionBar } from '../editors/StickyActionBar';
import { Can, useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { useRunAction } from '../hooks/useRunAction';
import { lintSegment } from '../lint';
import { presetSegmentContent, segmentTemplate } from '../presets';
import { DRAFT_CHIP, consoleViewPath, fmtTime, scopePath } from '../utils';

interface SegmentRouteState {
  preset?: string;
  metadata?: { displayName?: string; description?: string };
}

interface SegmentDetailData {
  d: SegmentDraft | null;
  keys: string[];
  preview: ReviewPreviewData | null;
  previewUnavailable: boolean;
  dependencies: SegmentDependencies | null;
  history: SegmentHistory | null;
}

// renderSegmentDependencies: the flags referencing this segment, each a
// dirty-guarded row-link over to the flag detail.
function SegmentDependenciesView({ data, onOpenFlag }: {
  data: SegmentDependencies | null;
  onOpenFlag: (key: string) => void;
}): ReactElement {
  const { t } = useTranslation();
  if (!data) return <div className="banner warn">{t('seg.dependenciesUnavailable')}</div>;
  return (
    <section className="card">
      <h3>{t('seg.dependenciesTitle')}</h3>
      <p className="muted">{t('seg.dependenciesScope', { n: data.segmentRevision || 0 })}</p>
      {data.flags?.length ? (
        <ul className="plain">
          {data.flags.map(flag => (
            <li key={flag.key}>
              <button className="row-link" type="button" data-dependent-flag={flag.key} onClick={() => onOpenFlag(flag.key)}>
                {flag.key}
              </button>
              {' · r'}{flag.revision}{' · '}{(flag.via || []).join(' → ')}
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">{t('seg.dependenciesEmpty')}</p>
      )}
    </section>
  );
}

const HISTORY_FIELD_KEYS: Record<string, string> = {
  schemaVersion: 'seg.historySchemaVersion',
  constraints: 'seg.historyConstraints',
  segments: 'seg.historyReferences',
};

// renderSegmentHistory: compare select + per-field diff + full revision list.
function SegmentHistoryView({ data, locale }: { data: SegmentHistory | null; locale: string }): ReactElement {
  const { t } = useTranslation();
  const [compare, setCompare] = useState<number | null>(null);
  if (!data) return <div className="banner warn">{t('seg.historyUnavailable')}</div>;
  const revisions = data.revisions || [];
  if (!revisions.length) {
    return (
      <section className="card">
        <h3>{t('seg.historyTitle')}</h3>
        <p className="muted">{t('seg.historyEmpty')}</p>
      </section>
    );
  }
  const current = revisions.find(item => item.revision === data.currentRevision);
  const previous = revisions.find(item => item.revision !== data.currentRevision);
  const selected = revisions.find(item => item.revision === compare) || previous || current;
  const fields = ['schemaVersion', 'constraints', 'segments'];
  const differences = current && selected
    ? fields.filter(field => JSON.stringify(current.content?.[field] ?? null) !== JSON.stringify(selected.content?.[field] ?? null))
    : [];
  return (
    <section className="card segment-history">
      <h3>{t('seg.historyTitle')}</h3>
      <p className="muted">{t('seg.historyScope', { n: data.currentRevision })}</p>
      <label className="field">
        {t('seg.historyCompare')}
        <select id="seg-history-select" value={selected?.revision} onChange={event => setCompare(Number(event.target.value))}>
          {revisions.map(item => (
            <option key={item.revision} value={item.revision}>
              r{item.revision}{' · '}{fmtTime(item.publishedAt, locale)}{' · '}{item.publishedBy}
            </option>
          ))}
        </select>
      </label>
      {current && selected ? (
        <>
          <p>{t('seg.historyDiff', { a: selected.revision, b: current.revision })}</p>
          {differences.length ? (
            differences.map(field => (
              <div className="segment-history-field" key={field}>
                <h4>{t(HISTORY_FIELD_KEYS[field])}</h4>
                <div className="flag-diff-values">
                  <div>
                    <strong>r{selected.revision}</strong>
                    <pre className="pre">{JSON.stringify(selected.content?.[field] ?? null, null, 2)}</pre>
                  </div>
                  <div>
                    <strong>r{current.revision}</strong>
                    <pre className="pre">{JSON.stringify(current.content?.[field] ?? null, null, 2)}</pre>
                  </div>
                </div>
              </div>
            ))
          ) : (
            <p className="muted">{t('seg.historySame')}</p>
          )}
        </>
      ) : (
        <div className="banner warn">{t('seg.historyCurrentMissing')}</div>
      )}
      <ul className="plain">
        {revisions.map(item => (
          <li key={item.revision}>
            r{item.revision}
            {item.revision === data.currentRevision ? ' · ' + t('seg.historyCurrent') : ''}
            {' · '}{fmtTime(item.publishedAt, locale)}{' · '}{item.publishedBy}
            {' · '}<span className="mono">{item.hash}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

export function SegmentDetailPage(): ReactElement {
  const [dirty, setDirty] = useState(false);
  return (
    <EditorDirtyProvider dirty={dirty}>
      <SegmentDetailBody setDirty={setDirty} />
    </EditorDirtyProvider>
  );
}

function SegmentDetailBody({ setDirty }: { setDirty: (dirty: boolean) => void }): ReactElement {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { client, ws, env, can } = useHost();
  const navigate = useNavigate();
  const location = useLocation();
  const { segKey = '' } = useParams();
  const confirm = useConfirm();
  const { confirmLeave } = useEditorDirty();
  const { toast } = useToast();
  const kp = scopePath(ws, env) + '/segments/' + encodeURIComponent(segKey);

  const routeState = (location.state || null) as SegmentRouteState | null;
  const routeStateRef = useRef(routeState);
  routeStateRef.current = routeState;

  const { data, loading, error, reload } = useApiData(
    async (signal): Promise<SegmentDetailData> => {
      const list = await client.api<SegmentDraft[]>(scopePath(ws, env) + '/segments', { signal });
      const d = (list || []).find(x => x.key === segKey) || null;
      let preview: ReviewPreviewData | null = null;
      let previewUnavailable = false;
      let dependencies: SegmentDependencies | null = { segmentRevision: 0, flags: [] };
      let history: SegmentHistory | null = { currentRevision: 0, revisions: [] };
      if (d) {
        const [previewResult, dependenciesResult, historyResult] = await Promise.allSettled([
          client.api<ReviewPreviewData>(kp + '/preview', { signal }),
          client.api<SegmentDependencies>(kp + '/dependencies', { signal }),
          client.api<SegmentHistory>(kp + '/history', { signal }),
        ]);
        if (previewResult.status === 'fulfilled') preview = previewResult.value;
        if (!preview) previewUnavailable = true;
        dependencies = dependenciesResult.status === 'fulfilled' ? dependenciesResult.value : null;
        history = historyResult.status === 'fulfilled' ? historyResult.value : null;
      }
      return { d, preview, previewUnavailable, dependencies, history, keys: (list || []).map(x => x.key) };
    },
    [client, kp, ws, env, segKey],
  );

  const editorRef = useRef<SegmentEditorHandle>(null);
  const probeRef = useRef<SegmentMatchProbeHandle>(null);
  const baselineRef = useRef('');
  const [jsonText, setJsonText] = useState<string | null>(null);
  const [risk, setRisk] = useState('normal');
  const [seedDoc, setSeedDoc] = useState<Record<string, unknown> | null>(null);
  const [editorEpoch, setEditorEpoch] = useState(0);
  const [editorIssues, setEditorIssues] = useState<string[]>([]);
  const [submitOpen, setSubmitOpen] = useState(false);
  const [dirtyLocal, setDirtyLocal] = useState(false);
  void dirtyLocal;

  const markDirty = (next: boolean): void => {
    setDirtyLocal(next);
    setDirty(next);
  };

  const seededFor = useRef<unknown>(undefined);
  useEffect(() => {
    if (loading || error) return;
    if (seededFor.current === data) return;
    seededFor.current = data;
    const d = data?.d;
    const state = routeStateRef.current;
    const content = d?.content ||
      { ...(presetSegmentContent(state?.preset || '', segKey) || segmentTemplate(segKey)), ...(state?.metadata || {}) };
    setSeedDoc(content as Record<string, unknown>);
    setJsonText(JSON.stringify(content, null, 2));
    setRisk(d?.risk || 'normal');
    setEditorEpoch(e => e + 1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, loading, error, segKey]);

  useLayoutEffect(() => {
    if (jsonText === null) return;
    baselineRef.current = editorRef.current?.snapshot() ?? '';
    markDirty(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editorEpoch]);

  const recomputeDirty = (): void => {
    const snap = editorRef.current?.snapshot();
    if (snap !== undefined) markDirty(snap !== baselineRef.current);
  };

  // Legacy clears the probe result on any draft/condition edit (delegated
  // input/click listeners); the probe clears itself on context edits.
  const onEditorActivity = (): void => {
    recomputeDirty();
    probeRef.current?.clear();
  };

  const runAction = useRunAction();

  const readJSON = (): Record<string, unknown> | null => {
    let c: Record<string, unknown>;
    try {
      c = JSON.parse(editorRef.current?.readSource() ?? '');
    } catch (e) {
      editorRef.current?.reportIssues([t('toast.invalidJson', { msg: (e as Error).message })]);
      return null;
    }
    const issues = lintSegment(t, c);
    if (!c || c.key !== segKey) issues.unshift(t('toast.keyMismatch', { key: segKey }));
    if (issues.length) {
      editorRef.current?.reportIssues(issues);
      return null;
    }
    return c;
  };

  const save = async (): Promise<void> => {
    if (!editorRef.current?.valid()) return;
    const content = readJSON();
    if (!content) return;
    await runAction(
      () => client.api(kp + '/draft', { method: 'PUT', body: { content, risk } }),
      t('toast.segSaved'),
      {
        savedEditor: () => {
          baselineRef.current = editorRef.current?.snapshot() ?? baselineRef.current;
          markDirty(false);
        },
        reload,
      },
    );
  };

  const publish = async (): Promise<void> => {
    if (!(await confirmLeave())) return;
    const accepted = await confirm({
      title: t('btn.publish'),
      summary: t('action.flagSummary', { key: segKey }),
      preview: <ReviewPreview preview={data?.preview} id="action-review-preview" />,
      reasonLabel: t('prompt.publishNote'),
      confirmLabel: t('btn.publish'),
      onConfirm: async ({ reason }) => {
        await client.api(kp + '/publish', { method: 'POST', body: { note: reason } });
        toast(t('toast.segPublished'));
      },
    });
    if (accepted) reload();
  };

  const openFlag = async (flagKey: string): Promise<void> => {
    if (!(await confirmLeave())) return;
    navigate(consoleViewPath(ws, env, 'flags', flagKey));
  };

  const goBack = async (): Promise<void> => {
    if (!(await confirmLeave())) return;
    navigate(consoleViewPath(ws, env, 'segments'));
  };

  if (loading || jsonText === null || !seedDoc) return <Skeleton lines={6} />;
  if (error) return <ErrorBanner error={error} />;

  const d = data?.d ?? null;
  const previewUnavailable = data?.previewUnavailable ?? false;
  // Legacy excludes the segment's own key from the reference suggestions.
  const segmentKeys = (data?.keys || []).filter(k => k !== segKey);

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="seg-back" onClick={() => void goBack()}>{t('seg.back')}</Button>
        <h2 className="resource-name">{String(seedDoc.displayName || segKey)}</h2>
        <div className="muted mono resource-key">{segKey}</div>
        {seedDoc.description ? <p className="muted resource-description">{String(seedDoc.description)}</p> : null}
        {d ? (
          <>
            <Chip label={d.state || ''} cls={DRAFT_CHIP[d.state || ''] || 'gray'} />{' '}
            <span className="muted">
              {t('flags.riskMeta', { risk: d.risk || '—', by: d.updatedBy || '—', time: fmtTime(d.updatedAt, locale) })}
            </span>
          </>
        ) : (
          <Chip label={t('chip.new')} cls="gray" />
        )}
      </div>
      <section className="card">
        <h3>{t('seg.draft')}</h3>
        <SegmentEditor
          key={editorEpoch}
          ref={editorRef}
          segKey={segKey}
          value={jsonText}
          onChange={setJsonText}
          risk={risk}
          onRiskChange={setRisk}
          readOnly={!can('flag.write')}
          segmentKeys={segmentKeys}
          onActivity={onEditorActivity}
          onIssuesChange={setEditorIssues}
        />
        <StickyActionBar>
          <Can permission="flag.write">
            <Button id="seg-save" variant="primary" disabled={editorIssues.length > 0} onClick={() => void save()}>
              {t('btn.saveDraft')}
            </Button>
          </Can>
          <Can permission="flag.write">
            <Button id="seg-submit" onClick={() => setSubmitOpen(true)}>{t('btn.submit')}</Button>
          </Can>
          <Can permission="flag.approve">
            <Button
              id="seg-approve"
              onClick={() => void runAction(() => client.api(kp + '/approve', { method: 'POST', body: {} }), t('toast.segApproved'), { reload })}
            >
              {t('btn.approve')}
            </Button>
          </Can>
          <Can permission="flag.publish">
            <Button id="seg-publish" disabled={previewUnavailable} onClick={() => void publish()}>{t('btn.publish')}</Button>
          </Can>
        </StickyActionBar>
      </section>
      {previewUnavailable ? (
        <div className="banner warn" role="status">{t('seg.previewUnavailable')}</div>
      ) : (
        <ReviewPreview preview={data?.preview} />
      )}
      <SegmentMatchProbe
        ref={probeRef}
        prepare={() => {
          if (!editorRef.current?.valid()) return null;
          return readJSON();
        }}
        sourceText={() => editorRef.current?.readSource() ?? ''}
        onProbe={(content, context) =>
          client.api(kp + '/match-preview', { method: 'POST', body: { content, context } })
        }
      />
      <SegmentDependenciesView data={data?.dependencies ?? null} onOpenFlag={key => void openFlag(key)} />
      <div id="seg-history">
        <SegmentHistoryView data={data?.history ?? null} locale={locale} />
      </div>
      {submitOpen ? (
        <FormDialog
          title={t('btn.submit')}
          fields={[{ name: 'reason', label: t('approval.reason'), required: true }]}
          submitLabel={t('btn.submit')}
          onClose={() => setSubmitOpen(false)}
          onSubmit={async ({ reason }) => {
            await runAction(() => client.api(kp + '/submit', { method: 'POST', body: { reason } }), t('toast.segSubmitted'), { reload });
          }}
        />
      ) : null}
    </>
  );
}
