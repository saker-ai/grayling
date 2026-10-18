// ApprovalDetailPage is the approval detail view, a port of the detail half
// of viewApprovals plus renderApprovalDetail in web/legacy/approvals.js. The
// page fetches the workspace approval list, picks the route param's entry
// (unknown id renders the legacy not_found banner), then loads the
// before/after comparison — comparison failures are tolerated so rejection
// and navigation stay available (legacy caught and kept comparison null).
// The facts table is followed by ApprovalReview (comparison, banners, note,
// actions) and the pretty-printed proposed payload.
import { Trans, useTranslation } from 'react-i18next';
import type { ReactElement } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '@saker/grayling-client';
import type { ApprovalComparison, ApprovalItem } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { ApprovalReview, approvalChipClass, approvalStateLabel } from '../components/ApprovalReview';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { consoleViewPath, fmtTime, wsPath } from '../utils';

interface ApprovalDetailData {
  approval: ApprovalItem | null;
  comparison: ApprovalComparison | null;
}

export function ApprovalDetailPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const routeParams = useParams<{ approvalID?: string }>();
  const approvalID = routeParams.approvalID || '';
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';

  const { data, loading, error } = useApiData(
    async (signal): Promise<ApprovalDetailData> => {
      const list = await client.api<ApprovalItem[]>(wsPath(ws) + '/approvals', { signal });
      if (!Array.isArray(list)) throw new ApiError(0, 'invalid_response', t('approval.invalidResponse'));
      const approval = list.find(item => item.id === approvalID) ?? null;
      if (!approval) return { approval: null, comparison: null };
      let comparison: ApprovalComparison | null = null;
      try {
        comparison = await client.api<ApprovalComparison>(
          wsPath(ws) + '/approvals/' + encodeURIComponent(approval.id) + '/comparison',
          { signal },
        );
      } catch {
        // Keep rejection and navigation available when comparison cannot load.
      }
      return { approval, comparison };
    },
    [client, ws, approvalID],
  );

  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;

  const approval = data?.approval ?? null;
  if (!approval) return <ErrorBanner error={new ApiError(404, 'not_found', '')} />;
  const comparison = data?.comparison ?? null;

  let payload = '';
  if (approval.payload) {
    try {
      payload = JSON.stringify(JSON.parse(approval.payload), null, 2);
    } catch {
      payload = approval.payload;
    }
  }

  return (
    <>
      <div className="view-head">
        <Button size="sm" id="approval-back" onClick={() => navigate(consoleViewPath(ws, env, 'approvals'))}>
          {t('approval.back')}
        </Button>
        <h2>{approval.action}</h2>
        <Chip label={approvalStateLabel(approval, t)} cls={approvalChipClass(approval)} />
      </div>
      <section className="card approval-detail">
        <h3>
          {t('approval.reviewObject')}: {approval.object}
        </h3>
        <dl className="approval-facts">
          <dt>{t('approval.environment')}</dt>
          <dd>{approval.environmentId || '—'}</dd>
          <dt>{t('approval.risk')}</dt>
          <dd>{approval.risk || '—'}</dd>
          <dt>{t('approval.base')}</dt>
          <dd>r{approval.baseRevision ?? 0}</dd>
          <dt>{t('approval.requester')}</dt>
          <dd>
            {approval.submittedBy} · {fmtTime(approval.submittedAt, locale)}
          </dd>
          <dt>{t('approval.reason')}</dt>
          <dd>{approval.reason || '—'}</dd>
          <dt>{t('approval.impact')}</dt>
          <dd>{approval.impact || '—'}</dd>
          <dt>{t('approval.rollback')}</dt>
          <dd>{approval.rollbackTarget || '—'}</dd>
          <dt>{t('approval.guardrails')}</dt>
          <dd>{approval.guardrails || '—'}</dd>
          <dt>{t('approval.hash')}</dt>
          <dd className="mono">{approval.payloadHash}</dd>
          <dt>{t('approval.expires')}</dt>
          <dd>{fmtTime(approval.expiresAt, locale)}</dd>
          <dt>{t('approval.reviewer')}</dt>
          <dd>{approval.approvedBy || approval.rejectedBy || '—'}</dd>
          {approval.rejectionReason ? (
            <>
              <dt>{t('approval.rejectionReason')}</dt>
              <dd>{approval.rejectionReason}</dd>
            </>
          ) : null}
        </dl>
        <ApprovalReview
          approval={approval}
          comparison={comparison}
          onDone={() => navigate(consoleViewPath(ws, env, 'approvals'))}
        />
        {payload ? (
          <>
            <h4>{t('approval.proposedPayload')}</h4>
            <pre className="pre">{payload}</pre>
          </>
        ) : null}
      </section>
    </>
  );
}
