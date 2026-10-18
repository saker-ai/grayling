// ApprovalsPage is the approval inbox list, a port of renderApprovalList plus
// the list half of viewApprovals in web/legacy/approvals.js: refreshable
// workspace-scoped list, pending/processed/all filter (default pending, like
// legacy S.approvalFilter), and row drill-down into the detail route. Pending
// holds exactly the items approvalAction() marks actionable for the signed-in
// operator.
import { useState } from 'react';
import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { ApiError } from '@saker/grayling-client';
import type { ApprovalItem } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { approvalAction, approvalChipClass, approvalStateLabel } from '../components/ApprovalReview';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { consoleViewPath, fmtTime, wsPath } from '../utils';

type ApprovalFilter = 'pending' | 'processed' | 'all';

export function ApprovalsPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env, can, actor } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error, reload } = useApiData(
    signal =>
      ws
        ? client.api<ApprovalItem[]>(wsPath(ws) + '/approvals', { signal }).then(list => {
            if (!Array.isArray(list)) throw new ApiError(0, 'invalid_response', t('approval.invalidResponse'));
            return list;
          })
        : Promise.resolve(null),
    [client, ws],
  );
  const [filter, setFilter] = useState<ApprovalFilter>('pending');

  if (!ws) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noWs" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;

  const approvals = data ?? [];
  const pending = approvals.filter(approval => approvalAction(approval, can, actor));
  const processed = approvals.filter(approval => approval.state !== 'submitted');
  const rows = filter === 'pending' ? pending : filter === 'processed' ? processed : approvals;
  const filterLabels: Record<ApprovalFilter, string> = {
    pending: t('approval.pending', { n: pending.length }),
    processed: t('approval.processed', { n: processed.length }),
    all: t('approval.all', { n: approvals.length }),
  };

  return (
    <>
      <div className="view-head">
        <h2>{t('approval.inbox')}</h2>
        <Button size="sm" id="approval-refresh" onClick={reload}>
          {t('btn.refresh')}
        </Button>
      </div>
      <div className="row flag-editor-modes" role="group" aria-label={t('approval.filter')}>
        {(['pending', 'processed', 'all'] as const).map(key => (
          <Button key={key} size="sm" data-approval-filter={key} aria-pressed={filter === key} onClick={() => setFilter(key)}>
            {filterLabels[key]}
          </Button>
        ))}
      </div>
      {rows.length ? (
        <div className="approval-list">
          {rows.map(approval => (
            <article className="card approval-item" key={approval.id}>
              <div className="row">
                <button
                  type="button"
                  className="row-link grow"
                  data-approval-id={approval.id}
                  onClick={() => navigate(consoleViewPath(ws, env, 'approvals', approval.id))}
                >
                  {approval.action} · {approval.object}
                </button>
                <Chip label={approvalStateLabel(approval, t)} cls={approvalChipClass(approval)} />
              </div>
              <p className="muted">
                {approval.environmentId || t('misc.none')} · {approval.submittedBy} · {fmtTime(approval.submittedAt, locale)}
              </p>
              <p>
                {t('approval.risk')}: {approval.risk || '—'} · {t('approval.impact')}: {approval.impact || '—'}
              </p>
            </article>
          ))}
        </div>
      ) : (
        <Banner kind="info">{filter === 'pending' ? t('approval.noneForMe') : t('approval.none')}</Banner>
      )}
    </>
  );
}
