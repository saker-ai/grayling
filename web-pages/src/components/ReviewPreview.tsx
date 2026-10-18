// ReviewPreview ports legacy reviewPreview + renderFlagExperimentImpact
// (web/legacy/app.js 64-95). With `links` the experiment ids render as
// row-link buttons carrying data-exp-from-flag; the owning page delegates the
// click (dirty-guard, then navigate to the experiment). Inside dialogs the
// links collapse to plain mono spans, matching legacy's `links = false` arm.
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentImpact, ReviewPreview as ReviewPreviewData } from '@saker/grayling-client';
import { diagnosticLabel, fmtTime } from '../utils';

export function ExperimentImpactView({ impact, links, onExperimentClick }: {
  impact: ExperimentImpact | null | undefined;
  links: boolean;
  onExperimentClick?: (id: string) => void;
}): ReactElement | null {
  const { t } = useTranslation();
  if (!impact) return null;
  const references = impact.references || [];
  const content = (
    <>
      <p>{t('flags.linkedExperimentsCount', { shown: references.length, total: impact.total || 0 })}</p>
      {references.length ? (
        <ul className="flag-experiment-list">
          {references.map(ref => (
            <li key={ref.id}>
              {links ? (
                <button className="row-link mono" type="button" data-exp-from-flag={ref.id} onClick={() => onExperimentClick?.(ref.id)}>{ref.id}</button>
              ) : (
                <span className="mono">{ref.id}</span>
              )}
              {' · '}{ref.state || ''}{' · '}{t('th.epoch')} {ref.epoch}{' · r'}{ref.flagRevision}
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">{t('flags.noLinkedExperiments')}</p>
      )}
      {(impact.total || 0) > references.length ? <p className="muted">{t('flags.linkedExperimentsMore')}</p> : null}
    </>
  );
  if (!links) return content;
  return (
    <section className="card">
      <h3>{t('flags.linkedExperiments')}</h3>
      {content}
    </section>
  );
}

export function ReviewPreview({ preview, id = 'review-preview' }: { preview: ReviewPreviewData | null | undefined; id?: string }): ReactElement | null {
  const { t, i18n } = useTranslation();
  if (!preview) return null;
  return (
    <section className="card" id={id}>
      <h3>{t('approval.preview')}</h3>
      <p>
        {t('approval.environment')}: {preview.targetEnvironment || ''} · {t('approval.risk')}: {diagnosticLabel(preview.risk, t)} · {t('approval.base')}: r{preview.baseRevision}
      </p>
      <p>{t('approval.changed')}: {(preview.changedFields || []).join(', ') || '—'}</p>
      <p>{t('approval.impact')}: {preview.estimatedAffected || ''}</p>
      <ExperimentImpactView impact={preview.experimentImpact} links={false} />
      <p className="wrap-anywhere">{t('approval.rollback')}: {preview.rollbackTarget || ''}</p>
      <p>{t('approval.guardrails')}: {preview.guardrails || ''}</p>
      <p className="mono">{t('approval.hash')}: {preview.proposedHash || ''}</p>
      {preview.approvalExpiresAt ? <p>{t('approval.expires')}: {fmtTime(preview.approvalExpiresAt, i18n.language)}</p> : null}
    </section>
  );
}
