import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { BindingReadiness } from '@saker/grayling-client';
import { bindChangedChecks, bindCheckNames } from './classify';

// BindReadiness ports renderBindReadiness (exp_workbench.js ~786): the
// pre-bind checklist banner — ok/warn headline, the registered-vs-current
// revision comparison and every check with its pass/fail label.
export function BindReadinessView({ readiness }: { readiness: BindingReadiness }): ReactElement {
  const { t } = useTranslation();
  const names = bindCheckNames(t);
  const checks = Array.isArray(readiness.checks) ? readiness.checks : [];
  return (
    <div className={'banner ' + (readiness.ready ? 'ok' : 'warn')}>
      <strong>{t(readiness.ready ? 'exp.bindReady' : 'exp.bindNotReady')}</strong>
      <p className="muted">
        {t('exp.bindRevisionCompare', {
          registered: readiness.registeredRevision ?? '—',
          current: readiness.currentRevision || '—',
        })}
      </p>
      <ul className="bind-check-list">
        {checks.map(check => (
          <li key={check.code}>
            <span className="bind-check-state">{t(check.ready ? 'exp.bindCheckPass' : 'exp.bindCheckFail')}</span>{' '}
            {names[check.code] || check.code}
          </li>
        ))}
      </ul>
    </div>
  );
}

// BindConflict ports renderBindConflict (exp_workbench.js ~800): the 409
// re-check banner after a failed bind — which revision moved and which checks
// newly fail. latest=null means the re-check itself failed.
export function BindConflict({
  previous,
  latest,
}: {
  previous: BindingReadiness | null;
  latest: BindingReadiness | null;
}): ReactElement {
  const { t } = useTranslation();
  const changed = bindChangedChecks(previous, latest);
  const names = bindCheckNames(t);
  return (
    <div className="banner warn" id="exp-bind-conflict" role="alert">
      <strong>{t('exp.bindConflictTitle')}</strong>
      <p>{t(latest ? 'exp.bindConflictLoaded' : 'exp.bindConflictFailed')}</p>
      {latest ? (
        <p>
          {t('exp.bindConflictRevision', {
            before: previous?.currentRevision || '—',
            now: latest.currentRevision || '—',
          })}
        </p>
      ) : null}
      {changed.length ? (
        <ul>
          {changed.map(check => (
            <li key={check.code}>{names[check.code] || check.code}</li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
