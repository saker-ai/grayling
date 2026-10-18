import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentDiagnostics, ExperimentReport, SRMResult } from '@saker/grayling-client';
import { Chip } from '../components/Chip';
import { DataTable } from '../components/DataTable';
import type { Column } from '../components/DataTable';
import { fmtDurNs, shortHash } from '../utils';
import { SampleFlow } from '../viz/SampleFlow';
import { SrmBar } from '../viz/SrmBar';

// Diagnostics ports renderDiagnostics (exp_workbench.js ~703): the delivery
// diagnostics block shared by the evidence zone (fixed-split reports always,
// bandit reports only for onRender) and the health zone. Sample flow, the two
// SRM layers (bandit-feedback windows show the banditSrmSeparate note), the
// per-variant flow table, contamination counters and rejected events with the
// collect.code.* label map. Never invents numbers: onDecision histories show
// '—' for assignment facts when no assignment events exist. The legacy
// table.diagnostic-table + .diagnostic-cards pair is one DataTable whose
// mobileCards tree serves narrow screens.

// FlowRow is one per-variant delivery counter row of the diagnostics payload.
type FlowRow = NonNullable<ExperimentDiagnostics['perVariant']>[number];

const COLLECT_CODE_KEYS: Record<string, string> = {
  invalid_event: 'collect.code.invalid_event',
  unknown_experiment: 'collect.code.unknown_experiment',
  stale_epoch: 'collect.code.stale_epoch',
  assignment_mismatch: 'collect.code.assignment_mismatch',
  missing_exposure: 'collect.code.missing_exposure',
  missing_unit: 'collect.code.missing_unit',
  invalid_exposure_token: 'collect.code.invalid_exposure_token',
  expired_exposure_token: 'collect.code.expired_exposure_token',
  late_event: 'collect.code.late_event',
  future_event: 'collect.code.future_event',
};

export function Diagnostics({ d }: { d: ExperimentDiagnostics | null | undefined }): ReactElement | null {
  const { t } = useTranslation();
  if (!d) return null;
  const flow = d.perVariant || [];
  const count = (field: 'assignments' | 'exposures' | 'conversions' | 'unexposed') =>
    flow.reduce((sum, variant) => sum + Number(variant[field] || 0), 0);
  const assigned = count('assignments');
  const exposed = count('exposures');
  const converted = count('conversions');
  const unexposed = count('unexposed');
  const hasAssignmentFacts = d.exposureMode === 'onRender' || assigned > 0;

  const srmLine = (label: string, value: SRMResult | null | undefined) => (
    <div>
      {label + ': '}
      {value ? (
        <>
          {'p = ' + (value.pValue ?? '—') + ' '}
          <Chip
            label={value.healthy ? t('chip.noSrm') : t('chip.srmDetected')}
            cls={value.healthy ? 'green' : 'red'}
          />
        </>
      ) : (
        t('report.srmInsufficient')
      )}
    </div>
  );

  const rejected = Object.entries(d.rejectedByReason || {}).sort(([a], [b]) => a.localeCompare(b));

  const flowColumns: Array<Column<FlowRow>> = [
    { key: 'variant', header: t('th.variant'), mobilePrimary: true, cell: v => <span className="mono">{v.variant}</span> },
    { key: 'assignments', header: t('report.assignments'), cell: v => (hasAssignmentFacts ? v.assignments : '—') },
    { key: 'exposures', header: t('report.exposures'), cell: v => v.exposures },
    { key: 'unexposed', header: t('report.unexposed'), cell: v => (hasAssignmentFacts ? v.unexposed : '—') },
    { key: 'converted', header: t('th.converted'), cell: v => v.conversions },
  ];

  return (
    <section aria-label={t('report.deliveryDiagnostics')}>
      <h4>{t('report.deliveryDiagnostics')}</h4>
      <p className="muted">{t('report.deliveryExplanation')}</p>
      <ol className="sample-flow" aria-label={t('report.sampleFlow')}>
        <li className="sample-flow-stage">
          <span>{t('report.assignments')}</span>
          <strong>{hasAssignmentFacts ? assigned : '—'}</strong>
        </li>
        <li className="sample-flow-stage">
          <span>{t('report.exposures')}</span>
          <strong>{exposed}</strong>
        </li>
        <li className="sample-flow-stage">
          <span>{t('report.collectedConverters')}</span>
          <strong>{converted}</strong>
        </li>
      </ol>
      {/* Proportional bars below the unchanged cards. Without assignment
          facts the first stage is null and draws an empty track — never a
          number borrowed from another stage. All-empty input renders nothing. */}
      <SampleFlow
        stages={[
          { key: 'assignments', label: t('report.assignments'), count: hasAssignmentFacts ? assigned : null },
          { key: 'exposures', label: t('report.exposures'), count: exposed },
          { key: 'converters', label: t('report.collectedConverters'), count: converted },
        ]}
      />
      <p className="muted">
        {t(hasAssignmentFacts ? 'report.sampleFlowOnRender' : 'report.sampleFlowOnDecision')}
        {hasAssignmentFacts ? ' ' + t('report.unexposed') + ': ' + unexposed + '.' : ''}
      </p>
      <p className="muted">{t('report.sampleFlowScope')}</p>
      {d.window?.version === 'bandit-feedback-v1' ? (
        <p className="muted">{t('report.banditSrmSeparate')}</p>
      ) : (
        <>
          {/* Each SRM layer keeps its text line, with the paired-bar figure
              directly beneath it (same component as the evidence-zone SRM).
              A figure renders nothing when its arrays are missing or
              mismatched, leaving the text line as the sole carrier. */}
          {srmLine(t('report.assignmentSrm'), d.assignmentSrm)}
          <SrmBar
            observed={d.assignmentSrm?.observed}
            expected={d.assignmentSrm?.expected}
            labels={flow.map(v => v.variant)}
          />
          {srmLine(t('report.exposureSrm'), d.exposureSrm)}
          <SrmBar
            observed={d.exposureSrm?.observed}
            expected={d.exposureSrm?.expected}
            labels={flow.map(v => v.variant)}
          />
        </>
      )}
      <DataTable
        columns={flowColumns}
        rows={flow}
        rowKey={v => v.variant}
        emptyText={t('report.noData')}
        ariaLabel={t('report.perVariant')}
        mobileCards
      />
      <div>
        {t('report.crossVariantUnits') + ': ' + (d.crossVariantUnits ?? 0) + ' · ' + t('report.assignmentMismatch') + ': ' +
          (d.assignmentMismatch ?? 0)}
      </div>
      <h5>{t('report.collectionAttempts')}</h5>
      <p className="muted">{t('report.collectionAttemptsNote')}</p>
      <div>{t('report.duplicateEvents') + ': ' + (d.duplicateEvents || 0)}</div>
      {rejected.length ? (
        <ul>
          {rejected.map(([code, n]) => (
            <li key={code}>
              {(COLLECT_CODE_KEYS[code] ? t(COLLECT_CODE_KEYS[code]) : code) + ' '}
              <span className="mono">{'(' + code + ')'}</span>
              {': ' + n}
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">{t('report.noRejectedEvents')}</p>
      )}
    </section>
  );
}

// MetricReferenceSummary ports metricReferenceSummary (exp_workbench.js ~635):
// the frozen primary-metric version line (or legacyInline), plus the primary
// attribution window when present.
export function MetricReferenceSummary({
  report,
  t,
}: {
  report: ExperimentReport;
  t: (key: string, params?: Record<string, unknown>) => string;
}): ReactElement {
  const ref = report.primaryMetricRef;
  return (
    <div className="muted">
      {t('report.metricVersion') + ': '}
      {ref ? (
        <span className="mono">{ref.key + ' v' + ref.version + ' · ' + shortHash(ref.definitionHash)}</span>
      ) : (
        t('report.legacyInline')
      )}
      {report.primaryAttributionWindow
        ? ' · ' + t('report.attributionWindow') + ': ' + fmtDurNs(report.primaryAttributionWindow, t)
        : ''}
    </div>
  );
}
