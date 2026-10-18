// ApprovalReview is the detail-page action section of the approvals console,
// a port of web/legacy/approvals.js renderApprovalComparison plus the act()
// dispatch (lines 62-90 and 157-214) and the executeApproval/reviewPreview
// helpers from web/legacy/app.js. It renders the before/after comparison, the
// self-review and expiry banners, the review note field and the
// approve/reject/execute buttons with the legacy re-read guards (the approval
// and its comparison are fetched again after confirmation; any drift blocks
// the action). The pure per-approval helpers (expiry, action visibility,
// state label, chip class) live here too, shared by the list and detail
// pages. ai.pin's content re-entry replaces the legacy native prompt() with a
// FormDialog, per the React migration rules.
import { useRef, useState } from 'react';
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, errMsg } from '@saker/grayling-client';
import type { ApprovalComparison, ApprovalItem, Translate } from '@saker/grayling-client';
import { Banner } from './Banner';
import { Button } from './Button';
import { useConfirm } from './ConfirmDialog';
import { FormDialog } from './FormDialog';
import { useToast } from './Toast';
import { useHost } from '../host';
import { flagDiffValue } from '../lint';
import { DRAFT_CHIP, diagnosticLabel, fmtTime, wsPath } from '../utils';

// approvalExpired: an approved request past its expiresAt can no longer be
// executed (legacy approvalExpired).
export function approvalExpired(approval: ApprovalItem): boolean {
  return (
    approval.state === 'approved' && !!approval.expiresAt && Date.parse(approval.expiresAt) <= Date.now()
  );
}

// approvalAction decides what the signed-in operator may do with one request
// (legacy approvalAction): 'review' for submitted requests the operator may
// approve — including their own, except break-glass post-reviews, which need
// an independent reviewer — and 'execute' for approved, unexpired, publishable
// requests. '' hides the action buttons.
export function approvalAction(approval: ApprovalItem, can: (permission: string) => boolean, actor: string): string {
  const self = !!actor && approval.submittedBy === actor;
  if (
    approval.state === 'submitted' &&
    can('flag.approve') &&
    actor &&
    !(self && approval.action === 'breakglass.review')
  ) {
    return 'review';
  }
  if (
    approval.state === 'approved' &&
    !approvalExpired(approval) &&
    approval.action !== 'breakglass.review' &&
    can('flag.publish')
  ) {
    return 'execute';
  }
  return '';
}

// Full catalog keys, never 'approval.state.' + state concatenation.
const APPROVAL_STATE_KEYS: Record<string, string> = {
  submitted: 'approval.state.submitted',
  approved: 'approval.state.approved',
  rejected: 'approval.state.rejected',
  used: 'approval.state.used',
};

// approvalStateLabel: expired outranks the recorded state; unknown states pass
// through verbatim (legacy approvalStateLabel).
export function approvalStateLabel(approval: ApprovalItem, t: Translate): string {
  if (approvalExpired(approval)) return t('approval.expired');
  const key = APPROVAL_STATE_KEYS[approval.state];
  return key ? t(key) : approval.state;
}

// approvalChipClass: rejected is red, everything else follows the draft chip
// map with gray fallback (legacy approvalChipClass).
export function approvalChipClass(approval: ApprovalItem): string {
  return approval.state === 'rejected' ? 'red' : DRAFT_CHIP[approval.state] || 'gray';
}

// ReviewPreviewData mirrors the object legacy act() built for reviewPreview();
// approvals never carry changedFields/experimentImpact/approvalExpiresAt.
interface ReviewPreviewData {
  targetEnvironment?: string;
  risk?: string;
  baseRevision?: number;
  changedFields?: string[];
  estimatedAffected?: string;
  rollbackTarget?: string;
  guardrails?: string;
  proposedHash?: string;
  approvalExpiresAt?: string;
}

// ReviewPreview is the legacy reviewPreview() card rendered inside the
// confirm dialog (id action-review-preview).
function ReviewPreview({ preview }: { preview: ReviewPreviewData }): ReactElement {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  return (
    <section className="card" id="action-review-preview">
      <h3>{t('approval.preview')}</h3>
      <p>
        {t('approval.environment')}: {preview.targetEnvironment} · {t('approval.risk')}:{' '}
        {diagnosticLabel(preview.risk, t)} · {t('approval.base')}: r{preview.baseRevision}
      </p>
      <p>
        {t('approval.changed')}: {(preview.changedFields || []).join(', ') || '—'}
      </p>
      <p>
        {t('approval.impact')}: {preview.estimatedAffected}
      </p>
      <p>
        {t('approval.rollback')}: {preview.rollbackTarget}
      </p>
      <p>
        {t('approval.guardrails')}: {preview.guardrails}
      </p>
      <p className="mono">
        {t('approval.hash')}: {preview.proposedHash}
      </p>
      {preview.approvalExpiresAt ? (
        <p>
          {t('approval.expires')}: {fmtTime(preview.approvalExpiresAt, locale)}
        </p>
      ) : null}
    </section>
  );
}

export interface ApprovalReviewProps {
  approval: ApprovalItem;
  comparison: ApprovalComparison | null;
  // onDone runs after a successful approve/reject/execute (the detail page
  // navigates back to the inbox; legacy called route()).
  onDone: () => void;
}

export function ApprovalReview({ approval, comparison, onDone }: ApprovalReviewProps): ReactElement {
  const { t } = useTranslation();
  const { client, ws, actor, can, refreshCapabilities } = useHost();
  const confirmAction = useConfirm();
  const { toast } = useToast();
  const noteRef = useRef<HTMLTextAreaElement>(null);
  const [feedback, setFeedback] = useState('');
  const [busy, setBusy] = useState<'' | 'review' | 'reject' | 'execute'>('');
  // pinPrompt holds the resolver of the in-flight ai.pin content re-entry
  // dialog; null while no dialog is open.
  const [pinPrompt, setPinPrompt] = useState<{ resolve: (content: string | null) => void } | null>(null);

  const action = approvalAction(approval, can, actor);
  const actionable = approval.state === 'submitted' || approval.state === 'approved';

  const requestPinContent = (): Promise<string | null> =>
    new Promise(resolve => setPinPrompt({ resolve }));

  const settlePin = (content: string | null): void => {
    setPinPrompt(current => {
      current?.resolve(content);
      return null;
    });
  };

  // executeApproval ports legacy executeApproval (app.js 2737-2764): the
  // approved operation is replayed against its target scope with the approval
  // id attached. Unsupported actions raise validation_failed, rendered
  // through errMsg like any other failure.
  const executeApproval = async (a: ApprovalItem): Promise<unknown> => {
    const envScope = wsPath(ws) + '/environments/' + encodeURIComponent(a.environmentId || '');
    if (a.action === 'metric.publish') {
      const match = /^(.*)\/v([1-9]\d*)$/.exec(a.object);
      if (!match) throw new ApiError(0, 'validation_failed', t('approval.unsupported'));
      return client.api(
        wsPath(ws) + '/metrics/' + encodeURIComponent(match[1]) + '/versions/' + encodeURIComponent(match[2]) + '/publish',
        { method: 'POST', body: { approvalId: a.id } },
      );
    }
    if (a.action === 'release_plan.create') {
      const payload = JSON.parse(a.payload || '') as { steps?: unknown };
      return client.api('/api/v1/admin/experiments/' + encodeURIComponent(a.object) + '/release-plan', {
        method: 'POST',
        body: { steps: payload.steps, approvalId: a.id },
      });
    }
    if (a.action === 'flag.rollback') {
      const payload = JSON.parse(a.payload || '') as { revision?: number };
      return client.api(envScope + '/flags/' + encodeURIComponent(a.object) + '/rollback', {
        method: 'POST',
        body: { revision: payload.revision, reason: a.reason, approvalId: a.id },
      });
    }
    if (a.action === 'ai.pin') {
      const content = await requestPinContent();
      if (!content) throw new ApiError(0, 'validation_failed', t('err.contentRequired'));
      const i = a.object.indexOf('/');
      return client.api(
        envScope +
          '/ai/resources/' +
          encodeURIComponent(a.object.slice(0, i)) +
          '/' +
          encodeURIComponent(a.object.slice(i + 1)) +
          '/pin',
        { method: 'POST', body: { content, approvalId: a.id } },
      );
    }
    throw new ApiError(0, 'validation_failed', t('approval.unsupported'));
  };

  // act is the legacy act(kind) dispatch, guard order preserved: comparison
  // gate, note gate, confirm dialog, then the post-confirm re-reads before
  // the mutating call.
  const act = async (kind: 'review' | 'reject' | 'execute'): Promise<void> => {
    if (kind !== 'reject' && (!comparison || comparison.stale)) {
      setFeedback(t('approval.comparisonUnavailable'));
      return;
    }
    const note = noteRef.current?.value.trim() || '';
    if ((kind === 'review' || kind === 'reject') && !note) {
      noteRef.current?.focus();
      setFeedback(t('approval.noteRequired'));
      return;
    }
    const title = kind === 'review' ? t('btn.approve') : kind === 'reject' ? t('approval.reject') : t('approval.execute');
    const summaryKey =
      kind === 'review' ? 'approval.confirmReview' : kind === 'reject' ? 'approval.confirmReject' : 'approval.confirmExecute';
    const confirmation = await confirmAction({
      title,
      summary: t(summaryKey, { action: approval.action, object: approval.object }),
      preview: (
        <ReviewPreview
          preview={{
            targetEnvironment: approval.environmentId,
            risk: approval.risk,
            baseRevision: approval.baseRevision,
            estimatedAffected: approval.impact,
            rollbackTarget: approval.rollbackTarget,
            guardrails: approval.guardrails,
            proposedHash: approval.payloadHash,
          }}
        />
      ),
      confirmLabel: title,
    });
    if (!confirmation) return;
    setBusy(kind);
    try {
      const latest = (await client.api<ApprovalItem[]>(wsPath(ws) + '/approvals')).find(item => item.id === approval.id);
      if (
        !latest ||
        latest.state !== approval.state ||
        latest.payloadHash !== approval.payloadHash ||
        approvalExpired(latest)
      ) {
        setFeedback(t('approval.changedBeforeAction'));
        return;
      }
      if (kind !== 'reject') {
        const latestComparison = await client.api<ApprovalComparison>(
          wsPath(ws) + '/approvals/' + encodeURIComponent(approval.id) + '/comparison',
        );
        if (latestComparison.stale) {
          setFeedback(t('approval.comparisonStale'));
          return;
        }
      }
      if (kind === 'review') {
        await client.api(wsPath(ws) + '/approvals/' + encodeURIComponent(approval.id) + '/approve', {
          method: 'POST',
          body: { note },
        });
      } else if (kind === 'reject') {
        await client.api(wsPath(ws) + '/approvals/' + encodeURIComponent(approval.id) + '/reject', {
          method: 'POST',
          body: { reason: note },
        });
      } else {
        await executeApproval(approval);
      }
      toast(t(kind === 'review' ? 'approval.reviewed' : kind === 'reject' ? 'approval.rejected' : 'approval.executed'));
      onDone();
    } catch (e) {
      setFeedback(errMsg(e as ApiError, t));
      if ((e as ApiError).status === 403) await refreshCapabilities();
    } finally {
      setBusy('');
    }
  };

  // comparisonSection is renderApprovalComparison: a warning when the
  // comparison failed to load, an explanation when none is available, else
  // the per-field before/after diff limited to the fields that actually
  // changed (flagDiffValue compares canonical JSON).
  const comparisonSection = (): ReactElement => {
    if (!comparison) return <Banner kind="warn">{t('approval.comparisonUnavailable')}</Banner>;
    const staleBanner =
      actionable && comparison.stale ? (
        <div className="banner warn" role="alert">
          {t('approval.comparisonStale')}
        </div>
      ) : null;
    if (!comparison.available) {
      return (
        <>
          {staleBanner}
          <Banner kind="info">
            {comparison.reason === 'content_not_retained'
              ? t('approval.contentNotRetained')
              : t('approval.comparisonUnsupported')}
          </Banner>
        </>
      );
    }
    const before = comparison.before || {};
    const after = comparison.after || {};
    const labels: Record<string, string> = {
      type: t('flags.diffType'),
      disable: t('flags.diffDisable'),
      bucketing: t('flags.diffBucketing'),
      fields: t('flags.diffFields'),
      variations: t('flags.diffVariations'),
      defaultRule: t('flags.diffDefaultRule'),
      targeting: t('flags.diffTargeting'),
      experiment: t('flags.diffExperiment'),
      steps: t('approval.releaseSteps'),
    };
    const fields = [...new Set([...Object.keys(before), ...Object.keys(after)])].filter(
      field => flagDiffValue(before[field]) !== flagDiffValue(after[field]),
    );
    return (
      <section className="approval-comparison">
        <h4>{t('approval.comparison')}</h4>
        {staleBanner}
        <p className="muted">
          {approval.action === 'flag.rollback'
            ? t('approval.comparisonVersions', { a: comparison.beforeRevision, b: comparison.afterRevision })
            : t('approval.releasePlanBase', { n: comparison.beforeRevision })}
        </p>
        {fields.length ? (
          fields.map(field => (
            <div className="flag-diff-item" key={field}>
              <strong>{labels[field] || field}</strong>
              <div className="flag-diff-values">
                <div>
                  <span>{t('approval.before')}</span>
                  <pre className="pre">{flagDiffValue(before[field])}</pre>
                </div>
                <div>
                  <span>{t('approval.after')}</span>
                  <pre className="pre">{flagDiffValue(after[field])}</pre>
                </div>
              </div>
            </div>
          ))
        ) : (
          <p className="muted">{t('approval.noFieldChanges')}</p>
        )}
      </section>
    );
  };

  return (
    <>
      {comparisonSection()}
      {approval.submittedBy === actor && approval.state === 'submitted' ? (
        <Banner kind={approval.action === 'breakglass.review' ? 'warn' : 'info'}>
          {approval.action === 'breakglass.review' ? t('approval.selfReviewBreakglass') : t('approval.selfReview')}
        </Banner>
      ) : null}
      {approvalExpired(approval) ? <Banner kind="warn">{t('approval.expiredHelp')}</Banner> : null}
      {action === 'review' ? (
        <label className="field">
          {t('approval.reviewNote')}
          <textarea id="approval-note" rows={3} ref={noteRef} />
        </label>
      ) : null}
      <div id="approval-feedback" role="alert">
        {feedback}
      </div>
      <div className="row actions">
        {action === 'review' ? (
          <>
            <Button
              variant="primary"
              id="approval-review"
              disabled={!comparison || comparison.stale || busy === 'review'}
              onClick={() => void act('review')}
            >
              {t('btn.approve')}
            </Button>
            <Button variant="danger" id="approval-reject" disabled={busy === 'reject'} onClick={() => void act('reject')}>
              {t('approval.reject')}
            </Button>
          </>
        ) : null}
        {action === 'execute' ? (
          <Button
            variant="primary"
            id="approval-execute"
            disabled={!comparison || comparison.stale || busy === 'execute'}
            onClick={() => void act('execute')}
          >
            {t('approval.execute')}
          </Button>
        ) : null}
      </div>
      {pinPrompt ? (
        <FormDialog
          title={t('approval.pinContent')}
          fields={[{ name: 'content', label: t('ai.content'), type: 'textarea', rows: 6, placeholder: t('ai.contentPh') }]}
          onSubmit={values => settlePin(values.content)}
          onClose={() => settlePin(null)}
        />
      ) : null}
    </>
  );
}
