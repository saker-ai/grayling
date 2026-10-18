import type { ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import type {
  OverviewModule,
  OverviewReleaseItem,
  OverviewResponse,
  Translate,
} from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { consoleViewPath, diagnosticLabel, fmtTime, wsPath } from '../utils';

// OverviewPage ports legacy viewOverview (web/legacy/app.js): the server-owned
// read model GET {ws}/overview with four independently degradable modules —
// risk band (renderOverviewRiskBand), count cards (renderOverviewStats),
// approvals waiting for me (renderOverviewApprovals) and recent releases
// (renderOverviewReleases, whose legacy dual desktop/mobile markup collapses
// into one DataTable) — plus the report-coverage card. A failed module gets
// renderOverviewModuleFailure's honest banner with retry; the browser never
// re-derives myAction/blockedReason, and null counts render as '—', never a
// fake zero. riskKindLabel/releaseActionLabel/overviewMyActionLabel/
// overviewBlockedNote/RELEASE_ACTION_CHIP are ported verbatim.

// RELEASE_ACTION_CHIP colors the whitelisted audit actions; anything outside
// the whitelist falls back to gray.
const RELEASE_ACTION_CHIP: Record<string, string> = {
  'flag.publish': 'green',
  'flag.rollback': 'red',
  'flag.emergency_disable': 'red',
  'segment.publish': 'green',
  'release_plan.apply_ratio': 'blue',
  'release_plan.rollback': 'red',
};

// riskKindLabel maps the server's risk kinds to localized labels; an unknown
// kind passes through verbatim (server content is never translated).
function riskKindLabel(kind: string | undefined, t: Translate): string | undefined {
  switch (kind) {
    case 'experiment_unhealthy':
      return t('overview.riskKind.experiment_unhealthy');
    case 'experiment_report_unavailable':
      return t('overview.reportUnavailable');
    case 'experiment_paused':
      return t('overview.riskKind.experiment_paused');
    case 'plan_paused':
      return t('overview.riskKind.plan_paused');
    case 'plan_rolled_back':
      return t('overview.riskKind.plan_rolled_back');
    default:
      return kind;
  }
}

// releaseActionLabel maps the audit whitelist actions; anything outside the
// whitelist falls back to the raw action string.
function releaseActionLabel(action: string | undefined, t: Translate): string | undefined {
  switch (action) {
    case 'flag.publish':
      return t('release.action.flag.publish');
    case 'flag.rollback':
      return t('release.action.flag.rollback');
    case 'flag.emergency_disable':
      return t('release.action.flag.emergency_disable');
    case 'segment.publish':
      return t('release.action.segment.publish');
    case 'release_plan.apply_ratio':
      return t('release.action.release_plan.apply_ratio');
    case 'release_plan.rollback':
      return t('release.action.release_plan.rollback');
    default:
      return action;
  }
}

// overviewMyActionLabel renders the server's verdict about what the caller
// may do with a pending approval (review|execute).
function overviewMyActionLabel(myAction: string, t: Translate): string {
  if (myAction === 'review') return t('overview.myAction.review');
  if (myAction === 'execute') return t('overview.myAction.execute');
  return myAction;
}

// overviewBlockedNote explains why an approval is not actionable for the
// caller; unknown reasons pass through verbatim.
function overviewBlockedNote(reason: string, t: Translate): string {
  switch (reason) {
    case 'role':
      return t('overview.blocked.role');
    case 'expired':
      return t('overview.blocked.expired');
    case 'breakglass_review':
      return t('overview.blocked.breakglass_review');
    default:
      return reason;
  }
}

export function OverviewPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';
  const { data, loading, error, reload } = useApiData(
    signal =>
      ws && env
        ? client.api<OverviewResponse>(wsPath(ws) + '/overview?limit=20' + (env ? '&env=' + encodeURIComponent(env) : ''), {
            signal,
          })
        : Promise.resolve(null),
    [client, ws, env],
  );

  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;

  const head = (
    <div className="view-head">
      <h2>{t('overview.title')}</h2>
      <span className="muted">{t('overview.scope') + ': ' + ws + ' / ' + env}</span>
      <Button size="sm" id="overview-refresh" onClick={reload}>
        {t('overview.refresh')}
      </Button>
    </div>
  );

  if (error) {
    return (
      <>
        {head}
        <ErrorBanner error={error} />
        <div className="row">
          <Button size="sm" id="overview-retry" onClick={reload}>
            {t('overview.retry')}
          </Button>
        </div>
      </>
    );
  }

  // moduleFailure is renderOverviewModuleFailure: why the module is missing
  // (status/reason/coverage) plus a retry affordance — a missing module is
  // never presented as an all-clear.
  const moduleFailure = (nameKey: string, mod: OverviewModule | null | undefined) => {
    const status = mod && mod.status ? mod.status : 'unavailable';
    const reason = mod && mod.reason ? mod.reason : '—';
    const cov = mod && mod.coverage ? mod.coverage : null;
    return (
      <div className="banner warn" role="status">
        <p>{t('overview.moduleFailed', { module: t(nameKey), reason })}</p>
        <p className="muted">
          {t('overview.moduleStatus', { status })}
          {cov ? ' · ' + t('overview.moduleCoverage', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' }) : ''}
        </p>
        <Button size="sm" data-overview-retry="" onClick={reload}>
          {t('overview.retry')}
        </Button>
      </div>
    );
  };

  const risks = data?.risks;
  const riskItems = risks?.items || [];
  const riskCov = risks?.coverage || {};
  const riskFailed = !risks || risks.status === 'unavailable' || !risks.status;
  // The "no blocking risks" all-clear appears only when status is ok AND
  // coverage is complete; every degraded state uses the partial wording.
  const riskClear = risks?.status === 'ok' && !!riskCov.complete;

  const counts = data?.counts;
  const stats: Array<[string, number | null | undefined]> = [
    [t('overview.paused'), counts ? counts.pausedExperiments : null],
    [t('overview.running'), counts ? counts.runningExperiments : null],
    [t('overview.pending'), counts ? counts.pendingApprovals : null],
    [t('overview.flags'), counts ? counts.publishedFlags : null],
  ];

  const pending = data?.pendingApprovals;
  const pendingFailed = !pending || pending.status !== 'ok';
  const pendingItems = pending?.items || [];

  const releases = data?.recentReleases;
  const releasesFailed = !releases || releases.status !== 'ok';
  const releaseItems = releases?.items || [];
  const releaseCov = releases?.coverage || {};

  const releaseColumns: Array<Column<OverviewReleaseItem>> = [
    { key: 'time', header: t('th.time'), className: 'nowrap', cell: item => fmtTime(item.time, locale) },
    {
      key: 'action',
      header: t('th.action'),
      cell: item => (
        <>
          <Chip label={releaseActionLabel(item.action, t)} cls={RELEASE_ACTION_CHIP[item.action || ''] || 'gray'} />{' '}
          <span className="mono">{diagnosticLabel(item.action, t)}</span>{' '}
          <details className="inline-diagnostic">
            <summary>{t('diagnostic.raw')}</summary>
            <code>{item.action}</code>
          </details>
        </>
      ),
    },
    { key: 'object', header: t('th.object'), cell: item => <span className="mono">{item.object || '—'}</span> },
    { key: 'actor', header: t('th.actor'), cell: item => item.actor || '—' },
    { key: 'env', header: t('th.env'), className: 'mono', cell: item => item.environmentId || '—' },
    {
      key: 'open',
      header: '',
      cell: item => (
        <Button
          size="sm"
          data-overview-release={item.object || ''}
          onClick={() => navigate(consoleViewPath(ws, env, 'audit', item.object || ''))}
        >
          {t('overview.openRelease')}
        </Button>
      ),
    },
  ];

  // The coverage card restates the risk report's coverage: what the server
  // inspected out of the total, and a drill-down when the scan was truncated.
  const cov = risks?.coverage || {};
  const coverageNote =
    !risks?.status || risks.status === 'unavailable' || risks.status === 'restricted'
      ? t('overview.reportCoverageUnavailable')
      : risks.status === 'ok' && cov.complete
        ? t('overview.reportCoverageComplete', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' })
        : t('overview.reportCoveragePartial', { inspected: cov.inspected ?? '—', total: cov.total ?? '—' });

  return (
    <>
      {head}
      <section className="card overview-band" id="overview-band">
        <h3>{t('overview.riskBand')}</h3>
        {riskFailed ? (
          moduleFailure('overview.riskBand', risks)
        ) : (
          <>
            {riskItems.length ? (
              <ul className="attention-list">
                {riskItems.map(item => {
                  const blocked = item.severity === 'block';
                  return (
                    <li key={item.id}>
                      <span>
                        <Chip
                          label={blocked ? t('overview.riskBlock') : t('overview.riskWarn')}
                          cls={blocked ? 'red' : 'amber'}
                        />{' '}
                        <strong className="mono">{item.id}</strong>
                        {' · '}
                        {riskKindLabel(item.kind, t)}
                        {item.environmentId ? (
                          <>
                            {' · '}
                            <span className="muted">{item.environmentId}</span>
                          </>
                        ) : null}
                        {item.text ? (
                          <>
                            <br />
                            <span className="muted">{item.text}</span>
                          </>
                        ) : null}
                      </span>
                      <Button
                        size="sm"
                        data-overview-risk={item.id}
                        onClick={() => navigate(consoleViewPath(ws, env, 'experiments', item.id))}
                      >
                        {t('overview.openExperiment')}
                      </Button>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <p className="muted">{t(riskClear ? 'overview.riskBandClear' : 'overview.riskBandPartial')}</p>
            )}
            {risks && (risks.reportCoverage === 'restricted' || risks.status === 'restricted') ? (
              <p className="muted">{t('overview.reportRestricted')}</p>
            ) : null}
          </>
        )}
      </section>
      <div className="overview-grid">
        {stats.map(([label, value]) => (
          <section className="card overview-stat" key={label}>
            <span className="muted">{label}</span>
            <strong>{value ?? '—'}</strong>
          </section>
        ))}
      </div>
      <section className="card overview-pending" id="overview-pending">
        <h3>{t('overview.myApprovals')}</h3>
        {pendingFailed ? (
          moduleFailure('overview.myApprovals', pending)
        ) : (
          <>
            {pending && (pending.actionableTotal ?? 0) > 0 ? (
              <p>
                <Chip label={t('overview.myApprovalsTotal', { n: pending.actionableTotal })} cls="amber" />
              </p>
            ) : null}
            {pendingItems.length ? (
              <ul className="attention-list">
                {pendingItems.map(item => (
                  <li key={item.id}>
                    <span>
                      <button
                        className="row-link mono"
                        type="button"
                        data-overview-approval={item.id}
                        onClick={() => navigate(consoleViewPath(ws, env, 'approvals', item.id))}
                      >
                        {diagnosticLabel(item.action, t)}
                        {' · '}
                        {item.object}
                      </button>{' '}
                      <details className="inline-diagnostic">
                        <summary>{t('diagnostic.raw')}</summary>
                        <code>{item.action}</code>
                      </details>{' '}
                      {item.myAction ? (
                        <Chip
                          label={overviewMyActionLabel(item.myAction, t)}
                          cls={item.myAction === 'review' ? 'amber' : 'green'}
                        />
                      ) : null}
                      {item.blockedReason ? <span className="muted">{overviewBlockedNote(item.blockedReason, t)}</span> : null}
                      <br />
                      <span className="muted">
                        {item.environmentId || '—'}
                        {' · '}
                        {item.submittedBy || '—'}
                        {' · '}
                        {fmtTime(item.submittedAt, locale)}
                        {item.risk ? (
                          <>
                            {' · '}
                            {t('approval.risk')}
                            {': '}
                            {diagnosticLabel(item.risk, t)}
                          </>
                        ) : null}
                        {item.expiresAt ? (
                          <>
                            {' · '}
                            {t('approval.expires')}
                            {' '}
                            {fmtTime(item.expiresAt, locale)}
                          </>
                        ) : null}
                      </span>
                    </span>
                    <Button
                      size="sm"
                      data-overview-approval={item.id}
                      onClick={() => navigate(consoleViewPath(ws, env, 'approvals', item.id))}
                    >
                      {t('overview.openApproval')}
                    </Button>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">{t('overview.myApprovalsEmpty')}</p>
            )}
          </>
        )}
      </section>
      <section className="card overview-releases" id="overview-releases">
        <h3>{t('overview.recentReleases')}</h3>
        {releasesFailed ? (
          moduleFailure('overview.recentReleases', releases)
        ) : (
          <>
            <p className="muted">
              {t('overview.recentReleasesCoverage', { scanned: releaseCov.inspected ?? '—' })}
              {releaseCov.complete ? '' : ' ' + t('overview.recentReleasesTruncated')}
            </p>
            {releaseItems.length ? (
              <DataTable
                columns={releaseColumns}
                rows={releaseItems}
                rowKey={item => [item.time, item.action, item.object, item.actor].join('|')}
                emptyText={t('overview.recentReleasesEmpty')}
                ariaLabel={t('overview.recentReleases')}
              />
            ) : (
              <p className="muted">{t('overview.recentReleasesEmpty')}</p>
            )}
          </>
        )}
      </section>
      <section className="card" id="overview-coverage">
        <p className="muted">{coverageNote}</p>
        {cov.total !== undefined && cov.inspected !== undefined && cov.total > cov.inspected ? (
          <Button size="sm" id="overview-all-experiments" onClick={() => navigate(consoleViewPath(ws, env, 'experiments'))}>
            {t('overview.allExperiments')}
          </Button>
        ) : null}
      </section>
    </>
  );
}
