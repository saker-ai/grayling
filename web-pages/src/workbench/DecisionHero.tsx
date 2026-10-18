import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentReport } from '@saker/grayling-client';
import { Banner } from '../components/Banner';
import { Button } from '../components/Button';
import { Chip } from '../components/Chip';
import { fmtTime } from '../utils';
import type { HeroClassification } from './classify';

// DecisionHero ports renderExpHero (exp_workbench.js ~561): the classified
// first-screen copy. The error/restricted/loading branches keep the legacy
// semantics exactly (a failed refresh never presents an older report as a
// current conclusion). The wrapper carries role=status aria-live=polite like
// the legacy #exp-decision container.
export function DecisionHero({
  cls,
  report,
  locale,
  onGoto,
}: {
  cls: HeroClassification;
  report: ExperimentReport | null;
  locale: string;
  onGoto: (target: string) => void;
}): ReactElement {
  const { t } = useTranslation();
  const kind = cls.kind || 'unknown';
  const p = cls.params || {};
  const actionRow = cls.actions.length ? (
    <div className="row actions" id="exp-hero-actions">
      {cls.actions.map(a => (
        <Button key={a.goto + a.label} size="sm" data-hero-goto={a.goto} onClick={() => onGoto(a.goto)}>
          {a.label}
        </Button>
      ))}
    </div>
  ) : null;

  // reasonItem ports the legacy verbatim-reasons list: the known no-exposure
  // issue string gets a localized headline plus a details/code disclosure of
  // the raw server text; every other reason renders verbatim.
  const reasonItem = (r: string) =>
    r === 'no randomized exposures recorded yet' ? (
      <>
        {t('diagnostic.noExposures')}
        <details>
          <summary>{t('diagnostic.raw')}</summary>
          <code>{r}</code>
        </details>
      </>
    ) : (
      r
    );

  let body: React.ReactNode = null;
  if (kind === 'error') {
    body = (
      <>
        <Banner kind="warn">{t('exp.reportUnavailable')}</Banner>
        {report ? (
          <p className="muted">
            {t('report.stale') + ' · ' + t('report.generatedAt') + ': ' + fmtTime(report.generatedAt, locale)}
          </p>
        ) : null}
      </>
    );
  } else if (kind === 'restricted') {
    body = <div className="banner">{t('exp.reportRestricted')}</div>;
  } else if (kind === 'loading') {
    body = <div className="banner">{t('exp.reportLoading')}</div>;
  } else if (kind === 'not_started') {
    body = (
      <>
        <div className="banner" id="exp-hero-banner">
          <strong>{t('exp.hero.notStarted')}</strong>
          <div>{t('exp.hero.notStartedNext')}</div>
        </div>
        {actionRow}
      </>
    );
  } else if (kind === 'no_samples') {
    const detail =
      p.subBranch === 'assigned'
        ? t('exp.hero.noSamplesAssigned', { assigned: p.assigned })
        : p.subBranch === 'none'
          ? t('exp.hero.noSamplesNone')
          : '';
    body = (
      <>
        <div className="banner" id="exp-hero-banner">
          <strong>{t('exp.hero.noSamples')}</strong>
          {detail ? <div>{detail}</div> : null}
        </div>
        {actionRow}
      </>
    );
  } else if (kind === 'immature') {
    const eta = p.eta ? t('exp.hero.immatureEta', { time: fmtTime(p.eta.toISOString(), locale) }) : t('exp.hero.immatureEtaUnknown');
    body = (
      <>
        <div className="banner" id="exp-hero-banner">
          <strong>{t('exp.hero.immature')}</strong>
          <div>{eta}</div>
          {(p.reasons || []).length ? (
            <ul className="muted">
              {(p.reasons || []).map((r, i) => (
                <li key={i}>{reasonItem(r)}</li>
              ))}
            </ul>
          ) : null}
        </div>
        {actionRow}
      </>
    );
  } else if (kind === 'blocked') {
    body = (
      <>
        <div className="banner err" id="exp-hero-banner">
          <strong>{t('exp.hero.blocked')}</strong>
          {(p.reasons || []).length ? (
            <ul>
              {(p.reasons || []).map((r, i) => (
                <li key={i}>{reasonItem(r)}</li>
              ))}
            </ul>
          ) : null}
          {(p.breaches || []).length ? (
            <div>
              {t('report.breaches')}
              <ul>
                {(p.breaches || []).map((gb, i) => (
                  <li key={i}>
                    <span className="mono">{gb.metric}</span> — {gb.detail || ''}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          <div className="muted">
            {t('exp.hero.blockedScope', { flag: p.scope?.flag, env: p.scope?.env, epoch: p.scope?.epoch })}
          </div>
        </div>
        {actionRow}
      </>
    );
  } else if (kind === 'can_advance') {
    const primary = p.primary;
    const guards = p.guardrails || [];
    body = (
      <>
        <div className="banner ok" id="exp-hero-banner">
          <strong>{t('exp.hero.canAdvance')}</strong>
          {primary ? (
            <div className="mono">
              {'p = ' + (primary.pValue ?? '—') + ' · diff = ' + (primary.diff ?? '—') + ' · rel = ' +
                (primary.relativeDiff ?? '—') + ' · CI [' + (primary.ciLow ?? '—') + ', ' + (primary.ciHigh ?? '—') + ']'}
            </div>
          ) : null}
          {guards.length ? (
            <div>
              {guards.map((g, i) => (
                <span key={i}>
                  {i > 0 ? ' ' : null}
                  <Chip label={g.breached ? t('chip.breached') : t('chip.ok')} cls={g.breached ? 'red' : 'green'} />
                  {' '}
                  <span className="mono">{g.metric ?? g.Metric ?? '—'}</span>
                </span>
              ))}
            </div>
          ) : null}
          <div className="muted">{t('exp.hero.canAdvanceNote')}</div>
        </div>
        {actionRow}
      </>
    );
  } else if (kind === 'ended') {
    const tr = p.transition;
    body = (
      <>
        <div className="banner" id="exp-hero-banner">
          <strong>
            {tr
              ? t('exp.hero.ended', {
                  state: p.state,
                  actor: tr.actor || '—',
                  time: fmtTime(tr.time, locale),
                  reason: tr.reason || '—',
                })
              : t('exp.hero.endedUnknown')}
          </strong>
          {p.rollbackTarget ? <div>{t('exp.hero.endedRollback', { target: p.rollbackTarget })}</div> : null}
        </div>
        {actionRow}
      </>
    );
  }
  return (
    <div className="exp-decision" id="exp-decision" role="status" aria-live="polite">
      {body}
    </div>
  );
}
