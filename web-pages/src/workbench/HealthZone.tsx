import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentDetail, ExperimentDiagnostics, ExperimentReport } from '@saker/grayling-client';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { EXP_CHIP, fmtTime } from '../utils';
import { Diagnostics } from './Diagnostics';

// HealthZone ports renderHealthZone + renderHealthReportCard
// (exp_workbench.js 656-701): the data-health zone. Pure renderer over the
// GET {id}/diagnostics payload plus the shared report (may be null: the
// report card then degrades to exp.zoneEmpty.report, never an empty table).
export function HealthZone({
  experiment,
  diagnostics,
  report,
  locale,
  onRefresh,
}: {
  experiment: ExperimentDetail;
  diagnostics: ExperimentDiagnostics;
  report: ExperimentReport | null;
  locale: string;
  onRefresh: () => void;
}): ReactElement {
  const { t } = useTranslation();
  const d = diagnostics;
  const window = d.window || {};
  const blockers = [...new Set([...(window.issues || []), ...(d.issues || [])])];
  const totalAssignments = (d.perVariant || []).reduce((sum, v) => sum + (v.assignments || 0), 0);
  const totalExposures = (d.perVariant || []).reduce((sum, v) => sum + (v.exposures || 0), 0);
  return (
    <>
      <div className="view-head">
        <h2>{t('exp.diagnosticsTitle')}</h2>
        <span className="mono">{experiment.id}</span>
        <Chip label={experiment.state} cls={EXP_CHIP[experiment.state || ''] || 'gray'} />
        <Button id="diag-refresh" onClick={onRefresh}>
          {t('btn.refresh')}
        </Button>
      </div>
      <section className="card">
        <h3>{t('exp.diagnosticsStatus')}</h3>
        <p>
          {t('exp.exposureMode') + ': ' + t(d.exposureMode === 'onRender' ? 'exp.onRender' : 'exp.onDecision') +
            ' · epoch ' + d.epoch + ' · ' + t('report.windowVersion') + ': '}
          <span className="mono">{window.version || '—'}</span>
        </p>
        <p>
          {t('report.watermark') + ': ' + fmtTime(window.watermark, locale) + ' · ' + t('report.backend') + ': ' +
            (window.backend || '—') + ' · ' + t('exp.lastCollected') + ': ' + fmtTime(d.lastCollectedAt, locale)}
        </p>
        <p>
          <Chip
            label={window.complete ? t('report.complete') : t('report.incomplete')}
            cls={window.complete ? 'green' : 'amber'}
          />
          {window.recomputable ? null : (
            <>
              {' '}
              <Chip label={t('report.notRecomputable')} cls="amber" />
            </>
          )}
        </p>
        {totalAssignments + totalExposures === 0 ? <p className="muted">{t('exp.noDiagnosticEvents')}</p> : null}
      </section>
      <section className="card">
        <h3>{t('exp.diagnosticIssues')}</h3>
        {blockers.length ? (
          <ul>
            {blockers.map(issue => (
              <li key={issue}>{issue}</li>
            ))}
          </ul>
        ) : (
          <p className="muted">{t('exp.noDiagnosticIssues')}</p>
        )}
      </section>
      <section className="card" id="diag-report-card">
        <HealthReportCard report={report} />
      </section>
      <section className="card">
        <Diagnostics d={d} />
      </section>
    </>
  );
}

// HealthReportCard summarizes report.healthIssues and the guardrail states
// inside the health zone. Without a loaded report it shows the
// exp.zoneEmpty.report copy — no invented verdict, no fabricated numbers.
export function HealthReportCard({ report }: { report: ExperimentReport | null }): ReactElement {
  const { t } = useTranslation();
  if (!report) {
    return (
      <>
        <h3>{t('exp.report')}</h3>
        <p className="muted">{t('exp.zoneEmpty.report')}</p>
      </>
    );
  }
  const issues = (report.healthIssues || []).length ? (
    <ul>
      {(report.healthIssues || []).map(issue => (
        <li key={issue}>{issue}</li>
      ))}
    </ul>
  ) : null;
  return (
    <>
      <h3>{t('exp.report')}</h3>
      {report.healthy ? (
        <>
          <p className="muted">{t('report.healthy', { epoch: report.epoch, state: report.state })}</p>
          {issues}
        </>
      ) : (
        <div className="banner warn">
          <strong>{t('report.unhealthy')}</strong>
          {issues}
        </div>
      )}
      <h4>{t('exp.guardrails')}</h4>
      {(report.guardrails || []).length ? (
        <ul className="plain">
          {(report.guardrails || []).map((g, i) => (
            <li key={i}>
              <Chip label={g.breached ? t('chip.breached') : t('chip.ok')} cls={g.breached ? 'red' : 'green'} />
              {' '}
              <span className="mono">{g.Metric ?? g.metric ?? '—'}</span>{' '}
              <span className="muted">{g.Detail ?? g.detail ?? ''}</span>
            </li>
          ))}
        </ul>
      ) : (
        <div className="muted">{t('misc.none')}</div>
      )}
    </>
  );
}
