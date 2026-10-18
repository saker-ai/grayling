// FlagDetailPage ports legacy viewFlagDetail + bindFlagDetail
// (web/legacy/app.js 1525-1782): the draft editor with its structured/JSON
// modes, the draft state machine (editing → submitted → approved → published)
// driving button visibility and the primary action, save with the 409
// conflict comparison flow, the publish review dialog with its re-fetch
// consistency check, mark-deleted and emergency-disable, revision rollback
// (approval-routed and emergency), and the gff import tool. The JSON source
// remains the single unsaved source of truth throughout.
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import { ApiError, errMsg } from '@saker/grayling-client';
import type { FlagDetail, FlagRevision } from '@saker/grayling-client';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { DiffView } from '../components/DiffView';
import { ErrorBanner } from '../components/ErrorBanner';
import { FormDialog } from '../components/FormDialog';
import { RevisionsTable } from '../components/RevisionsTable';
import { ExperimentImpactView, ReviewPreview } from '../components/ReviewPreview';
import { Skeleton } from '../components/Skeleton';
import { WorkflowBand } from '../components/WorkflowBand';
import type { WorkflowState } from '../components/WorkflowBand';
import { useConfirm } from '../components/ConfirmDialog';
import { useToast } from '../components/Toast';
import { EditorDirtyProvider, useEditorDirty } from '../editors/EditorDirtyContext';
import { FlagEditor } from '../editors/FlagEditor';
import type { FlagEditorHandle } from '../editors/FlagEditor';
import { StickyActionBar } from '../editors/StickyActionBar';
import { Can, useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { useRunAction } from '../hooks/useRunAction';
import { lintFlag } from '../lint';
import { flagTemplate, presetFlagContent } from '../presets';
import { DRAFT_CHIP, consoleViewPath, diagnosticLabel, fmtTime, resourceKeyError, scopePath } from '../utils';

interface FlagRouteState {
  preset?: string;
  metadata?: { displayName?: string; description?: string };
}

function flagStatusLabel(state: string, t: (key: string) => string): string {
  const keys: Record<string, string> = {
    submitted: 'flags.statusSubmitted',
    approved: 'flags.statusApproved',
    editing: 'flags.statusEditing',
    published: 'chip.published',
  };
  return keys[state] ? t(keys[state]) : state;
}

// renderPublishedSummary: the read-only dl of the published document.
function PublishedSummary({ content }: { content: Record<string, unknown> | null | undefined }): ReactElement | null {
  const { t } = useTranslation();
  if (!content) return null;
  const rule = (content.defaultRule || {}) as Record<string, unknown>;
  const fallback = rule.variation !== undefined
    ? String(rule.variation)
    : Object.entries((rule.percentage || {}) as Record<string, number>).map(([k, v]) => k + ': ' + v + '%').join(' · ');
  return (
    <dl className="published-summary">
      <dt>{t('resource.displayName')}</dt>
      <dd>{String(content.displayName || content.key || '—')}</dd>
      <dt>{t('resource.description')}</dt>
      <dd>{String(content.description || '—')}</dd>
      <dt>{t('th.type')}</dt>
      <dd>{String(content.type)}</dd>
      <dt>{t('flags.summaryVariations')}</dt>
      <dd>{Object.entries((content.variations || {}) as Record<string, unknown>).map(([k, v]) => k + ': ' + JSON.stringify(v)).join(' · ')}</dd>
      <dt>{t('flags.summaryDefault')}</dt>
      <dd>{fallback || '—'}</dd>
      <dt>{t('flags.summaryRules')}</dt>
      <dd>{((content.targeting as unknown[]) || []).length}</dd>
    </dl>
  );
}

// PublishReview is the live region inside the publish dialog: the server
// preview plus the draft-vs-published diff. onConfirm re-fetches and, when
// the underlying draft changed, pushes the fresh detail in through the
// registered refresh callback (legacy rewrote #flag-publish-review in place).
function PublishReview({ initial, registerRefresh }: {
  initial: FlagDetail;
  registerRefresh: (fn: (detail: FlagDetail) => void) => void;
}): ReactElement {
  const [detail, setDetail] = useState(initial);
  useEffect(() => {
    registerRefresh(() => setDetail);
  }, [registerRefresh]);
  return (
    <div id="flag-publish-review">
      <ReviewPreview preview={detail.preview} id="action-review-preview" />
      <DiffView current={detail.current} draft={detail.draft} baseRevision={detail.current?.revision} />
    </div>
  );
}

export function FlagDetailPage(): ReactElement {
  const [dirty, setDirty] = useState(false);
  return (
    <EditorDirtyProvider dirty={dirty}>
      <FlagDetailBody setDirty={setDirty} />
    </EditorDirtyProvider>
  );
}

function FlagDetailBody({ setDirty }: { setDirty: (dirty: boolean) => void }): ReactElement {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { client, ws, env, can, refreshCapabilities } = useHost();
  const navigate = useNavigate();
  const location = useLocation();
  const { flagKey = '' } = useParams();
  const confirm = useConfirm();
  const { confirmLeave } = useEditorDirty();
  const { toast } = useToast();
  const kp = scopePath(ws, env) + '/flags/' + encodeURIComponent(flagKey);

  // Legacy S.flagNew: the create dialog navigates here carrying router state;
  // a deep link without state treats a 404 as a real error.
  const routeState = (location.state || null) as FlagRouteState | null;
  const routeStateRef = useRef(routeState);
  routeStateRef.current = routeState;

  const { data, loading, error, reload } = useApiData(
    signal => client.api<FlagDetail>(kp, { signal }).catch(e => {
      if ((e as ApiError).status === 404 && routeStateRef.current) return null;
      throw e;
    }),
    [client, kp],
  );

  // Editor-backed state (legacy module-level editorValue()/S.editorBaseline).
  const editorRef = useRef<FlagEditorHandle>(null);
  const baselineRef = useRef('');
  const [jsonText, setJsonText] = useState<string | null>(null);
  const [risk, setRisk] = useState('normal');
  const [base, setBase] = useState(0);
  const [savedDraftHash, setSavedDraftHash] = useState('');
  const [seedDoc, setSeedDoc] = useState<Record<string, unknown> | null>(null);
  const [editorEpoch, setEditorEpoch] = useState(0);
  const [editorIssues, setEditorIssues] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [dirty, setDirtyLocal] = useState(false);
  const dirtyRef = useRef(false);
  const [conflict, setConflict] = useState<{ latest: FlagDetail; local: Record<string, unknown> } | null>(null);
  const [conflictAcked, setConflictAcked] = useState(false);
  const [conflictError, setConflictError] = useState<unknown>(null);
  const [gffWarn, setGffWarn] = useState<{ warnings: unknown[]; note?: string } | null>(null);
  const [revisionsOverride, setRevisionsOverride] = useState<FlagRevision[] | null>(null);
  const [submitOpen, setSubmitOpen] = useState(false);
  const [approveOpen, setApproveOpen] = useState(false);
  const [rejectOpen, setRejectOpen] = useState(false);
  const conflictBoxRef = useRef<HTMLDivElement>(null);
  const gffDocRef = useRef<HTMLTextAreaElement>(null);
  const gffKeyRef = useRef<HTMLInputElement>(null);
  const gffSaltRef = useRef<HTMLInputElement>(null);
  const publishRefreshRef = useRef<((detail: FlagDetail) => void) | null>(null);

  const markDirty = (next: boolean): void => {
    dirtyRef.current = next;
    setDirtyLocal(next);
    setDirty(next);
  };

  // Segment suggestions for the reference pickers (legacy
  // loadEditorSegmentSuggestions): fire-and-forget, datalists only.
  const { data: segmentList } = useApiData(
    signal => client.api<Array<{ key?: string }>>(scopePath(ws, env) + '/segments', { signal }).catch(() => []),
    [client, ws, env],
  );
  const segmentKeys = (segmentList || []).map(s => s.key).filter((k): k is string => !!k);

  // Seed the editor when (and only when) the fetch delivers fresh data —
  // legacy rebuilt the whole view on route(), so a reload re-seeds the source.
  const seededFor = useRef<unknown>(undefined);
  useEffect(() => {
    if (loading || error) return;
    if (seededFor.current === data) return;
    seededFor.current = data;
    const f = data;
    const draft = f?.draft;
    const state = routeStateRef.current;
    const content = draft?.content || f?.current?.content ||
      { ...(presetFlagContent(state?.preset || '', flagKey) || flagTemplate(flagKey)), ...(state?.metadata || {}) };
    setSeedDoc(content as Record<string, unknown>);
    setJsonText(JSON.stringify(content, null, 2));
    setRisk(draft?.risk || 'normal');
    setBase(f?.currentRevision || 0);
    setSavedDraftHash(f?.draftHash || '');
    setConflict(null);
    setConflictAcked(false);
    setConflictError(null);
    setRevisionsOverride(null);
    setEditorEpoch(e => e + 1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data, loading, error, flagKey]);

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

  const runAction = useRunAction();

  // readJSON: parse + lint + key check; reports into the editor's validate
  // slot (legacy reportDocumentIssues) and returns null on any issue.
  const readJSON = (): Record<string, unknown> | null => {
    let c: Record<string, unknown>;
    try {
      c = JSON.parse(editorRef.current?.readSource() ?? '');
    } catch (e) {
      editorRef.current?.reportIssues([t('toast.invalidJson', { msg: (e as Error).message })]);
      return null;
    }
    const issues = lintFlag(t, c);
    if (!c || c.key !== flagKey) issues.unshift(t('toast.keyMismatch', { key: flagKey }));
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
    setSaving(true);
    try {
      await client.api(kp + '/draft', {
        method: 'PUT',
        body: { content, risk, baseRevision: base, expectedDraftHash: savedDraftHash },
      });
      baselineRef.current = editorRef.current?.snapshot() ?? baselineRef.current;
      markDirty(false);
      toast(t('toast.draftSaved'));
      reload();
    } catch (e) {
      const err = e as ApiError;
      if (err.status === 409) {
        try {
          const latest = await client.api<FlagDetail>(kp);
          setConflict({ latest, local: content });
          setConflictAcked(false);
          setConflictError(null);
          conflictBoxRef.current?.scrollIntoView?.({ block: 'start', behavior: 'smooth' });
        } catch (refreshErr) {
          setConflictError(refreshErr);
        }
      }
      toast(errMsg(err, t), false);
      if (err.status === 403) {
        try {
          await refreshCapabilities();
        } catch {
          // Keep the unsaved editor visible.
        }
      }
    } finally {
      setSaving(false);
    }
  };

  const requestChange = async (action: string, object: string, payload: unknown, reasonOverride = ''): Promise<unknown> => {
    if (!(await confirmLeave())) return null;
    const reason = reasonOverride;
    if (!reason) {
      toast(t('toast.reasonRequired'), false);
      return null;
    }
    try {
      const a = await client.api<{ id: string }>(scopePath(ws, env) + '/approvals', {
        method: 'POST',
        body: { action, object, payload, reason },
      });
      toast(t('approval.requested', { id: a.id }));
      return a;
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
      if ((e as ApiError).status === 403) {
        await refreshCapabilities();
        reload();
      }
      return null;
    }
  };

  // confirmFlagPublish: re-fetch, review dialog with live preview, publish
  // only when the re-fetched detail still matches what was reviewed.
  const publish = async (): Promise<void> => {
    if (!(await confirmLeave())) return;
    let reviewed: FlagDetail;
    try {
      reviewed = await client.api<FlagDetail>(kp);
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
      if ((e as ApiError).status === 403) {
        await refreshCapabilities();
        reload();
      }
      return;
    }
    const accepted = await confirm({
      title: t('btn.publish'),
      summary: t('action.flagSummary', { key: flagKey }),
      preview: <PublishReview initial={reviewed} registerRefresh={fn => { publishRefreshRef.current = fn; }} />,
      reasonLabel: t('prompt.publishNote'),
      confirmLabel: t('btn.publish'),
      onConfirm: async ({ reason }) => {
        const latest = await client.api<FlagDetail>(kp);
        if (latest.draftHash !== reviewed.draftHash || latest.currentRevision !== reviewed.currentRevision ||
          JSON.stringify(latest.experimentImpact) !== JSON.stringify(reviewed.experimentImpact)) {
          reviewed = latest;
          publishRefreshRef.current?.(latest);
          throw new ApiError(0, 'flag_review_changed', t('flags.reviewChanged'));
        }
        const result = await client.api<{ flagRevision?: number; manifestRevision?: number }>(kp + '/publish', {
          method: 'POST',
          body: { note: reason },
        });
        toast(t('toast.published', { flag: result?.flagRevision, manifest: result?.manifestRevision }));
      },
    });
    if (accepted) reload();
  };

  const markDeleted = async (): Promise<void> => {
    const approval = await confirm({
      title: t('btn.markDeleted'),
      summary: t('confirm.markDeleted', { key: flagKey }),
      confirmLabel: t('btn.markDeleted'),
    });
    if (!approval) return;
    await runAction(() => client.api(kp, { method: 'DELETE' }), t('toast.deletionMarked'), { reload });
  };

  const emergencyDisable = async (): Promise<void> => {
    const approval = await confirm({
      title: t('approval.emergencyDisable'),
      summary: t('approval.confirmDisable', { key: flagKey }),
      preview: <ReviewPreview preview={data?.preview} id="action-review-preview" />,
      reasonLabel: t('prompt.rollbackReason'),
      confirmLabel: t('approval.emergencyDisable'),
    });
    if (!approval) return;
    await runAction(
      () => client.api<{ reviewId?: string }>(kp + '/emergency-disable', { method: 'POST', body: { reason: approval.reason } }),
      r => t('approval.emergencyDone', { id: r?.reviewId }),
      { reload },
    );
  };

  const rollback = async (revision: number, emergency: boolean): Promise<void> => {
    const approval = await confirm({
      title: t('flags.rollbackTo', { n: revision }),
      summary: t('confirm.rollback', { rev: revision }),
      reasonLabel: t('prompt.rollbackReason'),
      confirmLabel: t('flags.rollbackTo', { n: revision }),
    });
    if (!approval) return;
    if (!emergency) {
      await requestChange('flag.rollback', flagKey, { revision }, approval.reason);
      return;
    }
    await runAction(
      () => client.api<{ reviewId?: string }>(kp + '/rollback', {
        method: 'POST',
        body: { revision, reason: approval.reason, emergency: true },
      }),
      r => t('approval.emergencyDone', { id: r?.reviewId }),
      { reload },
    );
  };

  const runImport = async (): Promise<void> => {
    const doc = gffDocRef.current?.value || '';
    if (!doc.trim()) {
      toast(t('toast.pasteGff'), false);
      return;
    }
    const importKey = (gffKeyRef.current?.value || '').trim() || flagKey;
    const keyIssue = resourceKeyError(importKey, t);
    if (keyIssue) {
      editorRef.current?.reportIssues([keyIssue]);
      return;
    }
    try {
      const res = await client.api<{ warnings?: unknown[]; note?: string }>(kp + '/import-gff', {
        method: 'POST',
        body: { document: doc, key: importKey, salt: (gffSaltRef.current?.value || '').trim() },
      });
      setGffWarn({ warnings: res?.warnings || [], note: res?.note });
      toast(t('toast.imported'));
      // Refresh the editor's JSON source with the imported draft and update
      // the revisions table; the warnings banner stays visible, and the
      // structured form is intentionally NOT redrawn (legacy behavior).
      const nf = await client.api<FlagDetail>(kp);
      if (nf?.draft) setJsonText(JSON.stringify(nf.draft.content, null, 2));
      if (nf) setRevisionsOverride(nf.revisions ?? []);
    } catch (e) {
      toast(errMsg(e as ApiError, t), false);
      if ((e as ApiError).status === 403) {
        await refreshCapabilities();
        reload();
      }
    }
  };

  const openExperiment = async (id: string): Promise<void> => {
    if (!(await confirmLeave())) return;
    navigate(consoleViewPath(ws, env, 'experiments', id));
  };

  const goBack = async (): Promise<void> => {
    if (!(await confirmLeave())) return;
    navigate(consoleViewPath(ws, env, 'flags'));
  };

  if (loading || jsonText === null || !seedDoc) return <Skeleton lines={6} />;
  if (error) return <ErrorBanner error={error} />;

  const f = data;
  const draft = f?.draft;
  const state = draft?.state || 'editing';
  const primary = state === 'submitted' ? 'flag-approve' : state === 'approved' ? 'flag-publish' : 'flag-save';
  const diffSource = conflict?.latest ?? f;
  const lifecycleTitle = dirty ? t('flags.saveBeforeAction') : undefined;
  const statusChip = draft ? (
    <>
      <Chip label={flagStatusLabel(state, t)} cls={DRAFT_CHIP[state] || 'gray'} />
      {draft.deleted ? <> <Chip label={t('chip.deletionPending')} cls="red" /></> : null}
    </>
  ) : f?.current ? (
    <Chip label={t('chip.published')} cls="green" />
  ) : (
    <Chip label={t('chip.new')} cls="gray" />
  );

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="flag-back" onClick={() => void goBack()}>{t('flags.back')}</Button>
        <h2 className="resource-name">{String(seedDoc.displayName || flagKey)}</h2>
        <div className="muted mono resource-key">{flagKey}</div>
        {seedDoc.description ? <p className="muted resource-description">{String(seedDoc.description)}</p> : null}
        {statusChip}
        {draft ? (
          <span className="muted">
            {t('flags.riskMeta', { risk: diagnosticLabel(draft.risk, t), by: draft.updatedBy || '—', time: fmtTime(draft.updatedAt, locale) })}
          </span>
        ) : null}
      </div>
      <WorkflowBandWrapper state={state} published={!!f?.current} />
      <div id="flag-saved-diff">
        {diffSource?.draft ? (
          <DiffView current={diffSource.current} draft={diffSource.draft} baseRevision={diffSource.current?.revision} />
        ) : null}
      </div>
      <ExperimentImpactView impact={f?.experimentImpact} links onExperimentClick={id => void openExperiment(id)} />
      <div id="flag-conflict" ref={conflictBoxRef}>
        {conflictError ? <ErrorBanner error={conflictError} /> : null}
        {conflict && !conflictAcked ? (
          <>
            <div className="banner warn" role="alert">
              <strong>{t('flags.conflictTitle')}</strong>
              <p>{t('flags.conflictHelp', { revision: conflict.latest.currentRevision || 0 })}</p>
            </div>
            <DiffView
              current={conflict.latest.current}
              draft={{ content: conflict.local }}
              baseRevision={conflict.latest.current?.revision}
              title={t('flags.conflictCompare')}
              afterLabel={t('flags.conflictLocal')}
              emptyLabel={t('flags.conflictNoChanges')}
            />
            {conflict.latest.draft ? (
              <DiffView
                current={{ content: conflict.latest.draft.content }}
                draft={{ content: conflict.local }}
                title={t('flags.conflictCompareDraft')}
                beforeLabel={t('flags.conflictRemoteDraft')}
                afterLabel={t('flags.conflictLocal')}
                hideBase
                emptyLabel={t('flags.conflictNoDraftChanges')}
              />
            ) : null}
            <Button
              id="flag-conflict-ack"
              onClick={() => {
                setSavedDraftHash(conflict.latest.draftHash || '');
                setBase(conflict.latest.currentRevision || 0);
                setConflictAcked(true);
              }}
            >
              {t('flags.conflictAcknowledge')}
            </Button>
          </>
        ) : null}
        {conflict && conflictAcked ? <div className="banner ok">{t('flags.conflictAccepted')}</div> : null}
      </div>
      <div className="cols">
        <section className="card">
          <h3>{t('flags.draftEditor')}</h3>
          <FlagEditor
            key={editorEpoch}
            ref={editorRef}
            flagKey={flagKey}
            value={jsonText}
            onChange={setJsonText}
            risk={risk}
            onRiskChange={setRisk}
            baseRevision={base}
            readOnly={!can('flag.write')}
            segmentKeys={segmentKeys}
            onActivity={recomputeDirty}
            onIssuesChange={setEditorIssues}
          />
          <StickyActionBar>
            <Can permission="flag.write">
              <Button
                id="flag-save"
                variant={primary === 'flag-save' ? 'primary' : 'default'}
                disabled={saving || editorIssues.length > 0}
                onClick={() => void save()}
              >
                {t('btn.saveDraft')}
              </Button>
            </Can>
            <Can permission="flag.write">
              {draft && state === 'editing' ? (
                <Button id="flag-submit" disabled={dirty} title={lifecycleTitle} onClick={() => void confirmLeave().then(ok => { if (ok) setSubmitOpen(true); })}>
                  {t('btn.submit')}
                </Button>
              ) : null}
            </Can>
            <Can permission="flag.approve">
              {state === 'submitted' ? (
                <Button id="flag-approve" variant={primary === 'flag-approve' ? 'primary' : 'default'} disabled={dirty} title={lifecycleTitle} onClick={() => void confirmLeave().then(ok => { if (ok) setApproveOpen(true); })}>
                  {t('btn.approve')}
                </Button>
              ) : null}
            </Can>
            <Can permission="flag.approve">
              {state === 'submitted' || state === 'approved' ? (
                <Button id="flag-reject" disabled={dirty} title={lifecycleTitle} onClick={() => void confirmLeave().then(ok => { if (ok) setRejectOpen(true); })}>
                  {t('btn.reject')}
                </Button>
              ) : null}
            </Can>
            <Can permission="flag.publish">
              {state === 'approved' ? (
                <Button id="flag-publish" variant={primary === 'flag-publish' ? 'primary' : 'default'} disabled={dirty} title={lifecycleTitle} onClick={() => void publish()}>
                  {t('btn.publish')}
                </Button>
              ) : null}
            </Can>
          </StickyActionBar>
          <details className="flag-danger-tools" hidden={!f || (!can('flag.write') && !can('flag.publish'))}>
            <summary>{t('flags.dangerTools')}</summary>
            <div className="row">
              <Can permission="flag.write">
                <Button id="flag-delete" variant="danger" onClick={() => void markDeleted()}>{t('btn.markDeleted')}</Button>
              </Can>
              <Can permission="flag.publish">
                {f?.current ? (
                  <Button id="flag-emergency-disable" variant="danger" onClick={() => void emergencyDisable()}>
                    {t('approval.emergencyDisable')}
                  </Button>
                ) : null}
              </Can>
            </div>
          </details>
        </section>
        <section className="card">
          <h3>{t('flags.currentPublished', { n: f?.currentRevision || 0 })}</h3>
          {f?.current ? (
            <div className="muted">
              {t('flags.currentMeta', {
                hash: f.current.hash ? String(f.current.hash).slice(0, 12) : '—',
                time: fmtTime(f.current.publishedAt, locale),
                by: f.current.publishedBy || '—',
              })}
            </div>
          ) : (
            <div className="muted">{t('flags.nothingPublished')}</div>
          )}
          <PublishedSummary content={f?.current?.content} />
          <details hidden={!f?.current}>
            <summary>{t('flags.publishedJson')}</summary>
            <pre id="flag-current" className="pre">{f?.current ? JSON.stringify(f.current.content, null, 2) : ''}</pre>
          </details>
        </section>
      </div>
      <ReviewPreview preview={f?.preview} />
      <section className="card">
        <h3>{t('flags.history')}</h3>
        <div id="flag-revs">
          <RevisionsTable
            revisions={revisionsOverride ?? f?.revisions}
            locale={locale}
            canRollback={can('flag.publish')}
            onRollback={revision => void rollback(revision, false)}
            onEmergencyRollback={revision => void rollback(revision, true)}
          />
        </div>
      </section>
      <details className="card advanced-tools">
        <summary>{t('flags.importTitle')}</summary>
        <textarea id="gff-doc" className="json" rows={8} spellCheck={false} placeholder={t('flags.importPh')} ref={gffDocRef} />
        <div className="row">
          <label className="inline">key <input id="gff-key" defaultValue={flagKey} ref={gffKeyRef} /></label>
          <label className="inline">salt <input id="gff-salt" placeholder={t('flags.saltPh')} ref={gffSaltRef} /></label>
          <Can permission="flag.write">
            <Button id="gff-run" onClick={() => void runImport()}>{t('flags.importBtn')}</Button>
          </Can>
        </div>
        <div id="gff-warn">
          {gffWarn ? (
            <div className="banner warn">
              <b>{t('flags.importWarnings', { n: gffWarn.warnings.length })}</b>
              <ul>
                {gffWarn.warnings.length
                  ? gffWarn.warnings.map((w, i) => <li key={i}>{typeof w === 'string' ? w : JSON.stringify(w)}</li>)
                  : <li>{t('misc.none')}</li>}
              </ul>
              {gffWarn.note ? <div className="muted">{gffWarn.note}</div> : null}
            </div>
          ) : null}
        </div>
      </details>
      {submitOpen ? (
        <FormDialog
          title={t('btn.submit')}
          fields={[{ name: 'reason', label: t('approval.reason'), required: true }]}
          submitLabel={t('btn.submit')}
          onClose={() => setSubmitOpen(false)}
          onSubmit={async ({ reason }) => {
            await client.api(kp + '/submit', { method: 'POST', body: { reason } });
            toast(t('toast.draftSubmitted'));
            reload();
          }}
        />
      ) : null}
      {approveOpen ? (
        <FormDialog
          title={t('btn.approve')}
          fields={[{ name: 'note', label: t('prompt.approveNote') }]}
          submitLabel={t('btn.approve')}
          onClose={() => setApproveOpen(false)}
          onSubmit={async ({ note }) => {
            await client.api(kp + '/approve', { method: 'POST', body: { note } });
            toast(t('toast.draftApproved'));
            reload();
          }}
        />
      ) : null}
      {rejectOpen ? (
        <FormDialog
          title={t('btn.reject')}
          fields={[{ name: 'note', label: t('prompt.rejectReason'), required: true }]}
          submitLabel={t('btn.reject')}
          onClose={() => setRejectOpen(false)}
          onSubmit={async ({ note }) => {
            await client.api(kp + '/reject', { method: 'POST', body: { note } });
            toast(t('toast.draftRejected'));
            reload();
          }}
        />
      ) : null}
    </>
  );
}

// WorkflowBand wrapper: the draft state wins; a published flag with no draft
// shows the terminal step (legacy renderFlagWorkflow(draft, published)).
function WorkflowBandWrapper({ state, published }: { state: string; published: boolean }): ReactElement {
  const steps: WorkflowState[] = ['editing', 'submitted', 'approved', 'published'];
  const resolved: WorkflowState = (steps as string[]).includes(state) ? (state as WorkflowState) : published ? 'published' : 'editing';
  return <WorkflowBand state={resolved} />;
}
