import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentDetail, ExperimentReport } from '@saker/grayling-client';
import { Chip } from '../components/Chip';
import { EXP_CHIP, fmtTime } from '../utils';

// StatusBand ports renderExpStatusBand (exp_workbench.js ~401): the
// always-visible band above every zone — state chip, epoch, flag revision,
// registration version, report time ('—' until a report actually loaded) and
// environment. Never invents values.
export function StatusBand({
  experiment,
  report,
  locale,
}: {
  experiment: ExperimentDetail;
  report: ExperimentReport | null;
  locale: string;
}): ReactElement {
  const { t } = useTranslation();
  const field = (label: string, value: React.ReactNode) => (
    <span className="exp-status-field">
      <span className="muted">{label}</span> {value}
    </span>
  );
  return (
    <div className="exp-statusband" id="exp-statusband" role="status" aria-label={t('exp.statusBand')}>
      <Chip label={experiment.state} cls={EXP_CHIP[experiment.state || ''] || 'gray'} />
      {field('epoch', <b className="mono">{experiment.epoch || 0}</b>)}
      {field(t('exp.flagRevisionShort'), <b className="mono">{'r' + (experiment.flagRevision || 0)}</b>)}
      {field(t('exp.registrationVersion'), <b className="mono">{experiment.registrationVersion || '—'}</b>)}
      {field(
        t('exp.reportTime'),
        <b className="mono" id="exp-band-report-time">
          {report ? fmtTime(report.generatedAt, locale) : '—'}
        </b>,
      )}
      {field(t('exp.environment'), <b className="mono">{experiment.environmentId || '—'}</b>)}
    </div>
  );
}
