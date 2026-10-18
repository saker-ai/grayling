import { useEffect, useState } from 'react';
import type { FormEvent, ReactElement } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import type { ExperimentReportSummary, ExperimentSummaryItem, ExperimentSummaryPage } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { ErrorBanner } from '../components/ErrorBanner';
import { Skeleton } from '../components/Skeleton';
import { Can } from '../host';
import { useHost } from '../host';
import { useApiData } from '../hooks/useApiData';
import { EXP_CHIP, VERDICT_CHIP, consoleViewPath, fmtTime, scopePath } from '../utils';

// ExperimentsPage ports legacy viewExperiments (list half): server-side
// offset pagination (limit 20) over experiment-summaries with q/state
// filters applied by the filter form. Create and detail navigation land on
// the phase-3 routes (~new wizard, experiment workbench).
const EXP_STATES = ['draft', 'reviewed', 'scheduled', 'running', 'paused', 'stopped', 'analyzed', 'archived'];
const PAGE_SIZE = 20;

// expReportCell ports the legacy report cell: pending/restricted/unavailable
// short-circuit; otherwise a health chip, an optional verdict chip and an
// optional issue line.
function ReportCell({ report, t }: { report: ExperimentReportSummary | null | undefined; t: (k: string) => string }): ReactElement {
  if (report?.status === 'pending') return <Chip label={t('exp.listPending')} cls="gray" />;
  if (report?.status === 'restricted') return <Chip label={t('exp.listRestricted')} cls="gray" />;
  if (report?.status === 'unavailable') return <Chip label={t('exp.listUnavailable')} cls="red" />;
  const healthy = !!report?.healthy;
  return (
    <>
      <Chip label={t(healthy ? 'exp.listHealthy' : 'exp.listUnhealthy')} cls={healthy ? 'green' : 'amber'} />
      {report?.verdict ? <> <Chip label={report.verdict} cls={VERDICT_CHIP[report.verdict] || 'gray'} /></> : null}
      {report?.issue ? (
        <div className="muted exp-list-issue" title={report.issue}>
          {report.issue}
        </div>
      ) : null}
    </>
  );
}

export function ExperimentsPage(): ReactElement {
  const { t, i18n } = useTranslation();
  const { client, ws, env } = useHost();
  const navigate = useNavigate();
  const locale = i18n.language === 'zh' ? 'zh-CN' : 'en-US';

  // Filter state: draft values live in the form; applied values drive the
  // fetch. Changing scope resets both (legacy expList.scope check).
  const scope = ws + '/' + env;
  const [draftQ, setDraftQ] = useState('');
  const [draftState, setDraftState] = useState('');
  const [applied, setApplied] = useState({ q: '', state: '', offset: 0, scope });
  useEffect(() => {
    setApplied(a => (a.scope === scope ? a : { q: '', state: '', offset: 0, scope }));
    setDraftQ('');
    setDraftState('');
  }, [scope]);

  const { data, loading, error } = useApiData(
    signal => {
      const params = new URLSearchParams({ limit: String(PAGE_SIZE), offset: String(applied.offset) });
      if (applied.q) params.set('q', applied.q);
      if (applied.state) params.set('state', applied.state);
      return client.api<ExperimentSummaryPage>(scopePath(ws, env) + '/experiment-summaries?' + params, { signal });
    },
    [client, ws, env, applied],
  );

  if (!ws || !env) {
    return (
      <Banner kind="warn">
        <Trans i18nKey="banner.noScope" />
      </Banner>
    );
  }
  if (loading) return <Skeleton lines={4} />;
  if (error) return <ErrorBanner error={error} />;

  const pageData = data ?? { items: [], total: 0, offset: 0, limit: PAGE_SIZE };
  const rows = pageData.items || [];
  const first = pageData.total ? pageData.offset + 1 : 0;
  const last = pageData.offset + rows.length;

  const applyFilters = (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    setApplied({ q: draftQ.trim(), state: draftState, offset: 0, scope });
  };

  const columns: Array<Column<ExperimentSummaryItem>> = [
    {
      key: 'id',
      header: t('th.id'),
      className: 'mono',
      mobilePrimary: true, // card title: the experiment id drill-down
      cell: ({ experiment: e }) => (
        <button
          className="row-link"
          type="button"
          data-exp={e.id}
          onClick={() => navigate(consoleViewPath(ws, env, 'experiments', e.id))}
        >
          {e.id}
        </button>
      ),
    },
    { key: 'flag', header: t('th.flag'), className: 'mono', cell: ({ experiment: e }) => e.flagKey },
    {
      key: 'state',
      header: t('th.state'),
      cell: ({ experiment: e }) => <Chip label={e.state} cls={EXP_CHIP[e.state || ''] || 'gray'} />,
    },
    { key: 'health', header: t('exp.listHealth'), cell: ({ report }) => <ReportCell report={report} t={t} /> },
    { key: 'creator', header: t('exp.listCreator'), cell: ({ experiment: e }) => e.createdBy || '—' },
    { key: 'updated', header: t('th.updated'), className: 'nowrap', cell: ({ experiment: e }) => fmtTime(e.updatedAt, locale) },
  ];

  return (
    <>
      <div className="view-head">
        <h2>{t('exp.title')}</h2>
        <span className="muted">{pageData.total}</span>
        <Can permission="experiment.write">
          <Button
            variant="primary"
            id="exp-new"
            onClick={() => navigate(consoleViewPath(ws, env, 'experiments', '~new'))}
          >
            {t('exp.new')}
          </Button>
        </Can>
      </div>
      <form className="exp-list-filters" id="exp-list-filter" onSubmit={applyFilters}>
        <label>
          {t('exp.listSearch')}
          <input
            id="exp-list-q"
            type="search"
            value={draftQ}
            placeholder={t('exp.listSearchHint')}
            onChange={ev => setDraftQ(ev.currentTarget.value)}
          />
        </label>
        <label>
          {t('th.state')}
          <select id="exp-list-state" value={draftState} onChange={ev => setDraftState(ev.currentTarget.value)}>
            <option value="">{t('exp.listAllStates')}</option>
            {EXP_STATES.map(state => (
              <option key={state} value={state}>
                {state}
              </option>
            ))}
          </select>
        </label>
        <Button size="sm" type="submit">
          {t('exp.listApply')}
        </Button>
      </form>
      <DataTable columns={columns} rows={rows} rowKey={({ experiment: e }) => e.id} emptyText={t('exp.empty')} mobileCards />
      <div className="exp-list-pager">
        <span className="muted">{t('exp.listRange', { first, last, total: pageData.total })}</span>
        <Button
          size="sm"
          id="exp-prev"
          disabled={pageData.offset === 0}
          onClick={() => setApplied(a => ({ ...a, offset: Math.max(0, pageData.offset - pageData.limit) }))}
        >
          {t('exp.listPrev')}
        </Button>
        <Button
          size="sm"
          id="exp-next"
          disabled={last >= pageData.total}
          onClick={() => setApplied(a => ({ ...a, offset: last }))}
        >
          {t('exp.listNext')}
        </Button>
      </div>
    </>
  );
}
