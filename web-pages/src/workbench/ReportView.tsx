import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { ExperimentReport } from '@saker/grayling-client';
import { Chip } from '../components/Chip';
import { VERDICT_CHIP, fmtTime } from '../utils';
import { Diagnostics, MetricReferenceSummary } from './Diagnostics';

// ReportView ports renderReport + renderReportSummary + renderBanditReport
// (exp_workbench.js 813-960): the full evidence-zone renderer. Every P5
// report field renders — ratio/quantile/CUPED detail lines, the always-valid
// sequential view and the Bayesian view, each labelled separate from the
// frequentist decision gate (report.sequentialNote / report.bayesianNote).
// Empty and unhealthy shapes render honest placeholders (report.noData,
// report.noPrimary/primaryError, report.seqNotComputed, report.notComputed)
// — never fabricated numbers.

// ReportSummary is the decision snapshot heading a report (and reused as the
// rollout confirm preview): generated/watermark times, verdict chip (amber
// when unhealthy), the healthy/unhealthy line, verbatim reasons and the
// guardrail-breach banner.
export function ReportSummary({ report, locale }: { report: ExperimentReport; locale: string }): ReactElement {
  const { t } = useTranslation();
  const d = report.decision || {};
  const reasons = [...(report.healthIssues || []), ...(d.reasons || [])];
  const verdict = String(d.verdict || 'hold').toUpperCase();
  return (
    <section
      className={'report-summary' + (report.healthy ? '' : ' report-summary-blocked')}
      aria-label={t('report.decisionSnapshot')}
    >
      <div className="report-summary-head">
        <div>
          <h4>{t('report.decisionSnapshot')}</h4>
          <div className="muted">
            {t('report.generatedAt') + ': '}
            <time title={report.generatedAt || ''}>{fmtTime(report.generatedAt, locale)}</time>
            {' · ' + t('report.watermark') + ': '}
            <time title={report.window?.watermark || ''}>{fmtTime(report.window?.watermark, locale)}</time>
          </div>
        </div>
        <Chip label={verdict} cls={report.healthy ? VERDICT_CHIP[d.verdict || ''] || 'gray' : 'amber'} />
      </div>
      <p>{report.healthy ? t('report.healthy', { epoch: report.epoch, state: report.state }) : t('report.unhealthy')}</p>
      {reasons.length ? (
        <ul>
          {reasons.map((reason, i) => (
            <li key={i}>{reason}</li>
          ))}
        </ul>
      ) : null}
      {(d.guardrailBreaches || []).length ? (
        <div className="banner warn">
          {t('report.breaches')}
          <ul>
            {(d.guardrailBreaches || []).map((gb, i) => (
              <li key={i}>
                <span className="mono">{gb.metric}</span> — {gb.detail || ''}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  );
}

export function ReportView({ report, locale }: { report: ExperimentReport; locale: string }): ReactElement {
  const { t } = useTranslation();
  if (report.bandit) return <BanditReport report={report} />;
  const r = report;
  const p = r.primary;
  const srm = r.srm;
  const window = r.window;
  return (
    <>
      <ReportSummary report={r} locale={locale} />
      <MetricReferenceSummary report={r} t={t} />
      <Diagnostics d={r.diagnostics} />
      <div className="row">
        <Chip
          label={r.minRuntimeSatisfied ? t('chip.minRuntimeOk') : t('chip.minRuntimeNo')}
          cls={r.minRuntimeSatisfied ? 'green' : 'amber'}
        />{' '}
        <Chip label={r.analysisMethod || '—'} cls="gray" />
      </div>
      {window ? (
        <>
          <h4>{t('report.window')}</h4>
          <div>
            {t('report.windowVersion') + ': '}
            <span className="mono">{window.version}</span>
            {' · ' + t('report.windowStart') + ': '}
            <span className="mono">{window.start}</span>
            {' · ' + t('report.windowEnd') + ': '}
            <span className="mono">{window.end}</span>
          </div>
          <div>
            {t('report.watermark') + ': '}
            <span className="mono">{window.watermark}</span>
            {' · ' + t('report.backend') + ': ' + (window.backend ?? '') + ' '}
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
          </div>
          {(window.issues || []).length ? (
            <ul>
              {(window.issues || []).map((issue, i) => (
                <li key={i}>{issue}</li>
              ))}
            </ul>
          ) : null}
        </>
      ) : null}
      <h4>{t('report.srm')}</h4>
      {srm ? (
        <>
          <div>
            {'χ² = ' + (srm.chiSquare ?? '—') + ' · p = ' + (srm.pValue ?? '—') + ' '}
            <Chip label={srm.healthy ? t('chip.noSrm') : t('chip.srmDetected')} cls={srm.healthy ? 'green' : 'red'} />
          </div>
          <pre className="pre small">{JSON.stringify({ observed: srm.observed, expected: srm.expected }, null, 2)}</pre>
        </>
      ) : (
        <div className="muted">{t('report.noSrm')}</div>
      )}
      <h4>{t('report.perVariant')}</h4>
      <table className="grid">
        <thead>
          <tr>
            <th>{t('th.variant')}</th>
            <th>{t('th.exposedUnits')}</th>
            <th>{t('th.randomized')}</th>
            <th>{t('th.exposures')}</th>
            <th>{t('th.converted')}</th>
            <th>{t('th.primaryValue')}</th>
          </tr>
        </thead>
        <tbody>
          {(r.perVariant || []).length ? (
            (r.perVariant || []).map(v => (
              <tr key={v.variant}>
                <td className="mono">{v.variant}</td>
                <td>{v.exposedUnits}</td>
                <td>{v.randomizedUnits}</td>
                <td>{v.exposures}</td>
                <td>{v.convertedUnits}</td>
                <td>{v.primaryValue}</td>
              </tr>
            ))
          ) : (
            <tr>
              <td colSpan={6} className="muted">
                {t('report.noData')}
              </td>
            </tr>
          )}
        </tbody>
      </table>
      {(r.journeys || []).length ? (
        <>
          <h4>{t('report.journeys')}</h4>
          {(r.journeys || []).map((j, i) => (
            <div key={i}>
              <b>{j.metric}</b>
              {' · ' + j.type + ' · ' + t('report.watermark') + ': ' + (j.watermark ?? '')}
              <pre className="pre small">{JSON.stringify((j.funnel || j.retention) ?? null)}</pre>
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t('th.variant')}</th>
                    <th>{t('report.denominator')}</th>
                    <th>{t('report.numerator')}</th>
                    <th>{t('report.immature')}</th>
                    <th>{t('report.excluded')}</th>
                  </tr>
                </thead>
                <tbody>
                  {(j.arms || []).map(a => (
                    <tr key={a.variant}>
                      <td>{a.variant}</td>
                      <td>{a.denominator}</td>
                      <td>{a.numerator}</td>
                      <td>{a.immature}</td>
                      <td className="mono">{JSON.stringify(a.excluded || {})}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ))}
        </>
      ) : null}
      <h4>
        {t('report.primary')}
        {p ? <span className="muted">{' (' + (p.type || '') + ')'}</span> : null}
        {r.holm ? <span className="muted">{' ' + t('report.primaryRawNote')}</span> : null}
      </h4>
      {p ? (
        <div>
          {'p = ' + (p.pValue ?? '—') + ' · diff = ' + (p.diff ?? '—') + ' · rel = ' + (p.relativeDiff ?? '—')}
          {p.relativeMeaningful === false ? (
            <>
              {' '}
              <Chip label={t('chip.notMeaningful')} cls="amber" />
            </>
          ) : null}
          {' · CI [' + (p.ciLow ?? '—') + ', ' + (p.ciHigh ?? '—') + '] '}
          <Chip
            label={p.significant ? t('chip.significant') : t('chip.notSignificant')}
            cls={p.significant ? 'green' : 'gray'}
          />
        </div>
      ) : (
        <div className="muted">{r.primaryError || t('report.noPrimary')}</div>
      )}
      {p && p.type === 'cuped' && p.cuped ? (
        <div className="muted">
          {t('report.cuped', {
            pct: (100 * (p.varianceReduction || 0)).toFixed(1),
            theta: (p.cuped.theta ?? 0).toFixed(4),
            rho: (p.cuped.correlation ?? 0).toFixed(3),
            raw: (p.cuped.rawDiff ?? 0).toFixed(4),
          })}
        </div>
      ) : null}
      {p && p.type === 'ratio-delta' && p.ratio ? (
        <div className="muted">
          {'ratio: A=' + (p.ratio.ratioA ?? 0).toFixed(4) + ' · B=' + (p.ratio.ratioB ?? 0).toFixed(4) + ' · SE=' +
            (p.ratio.se ?? 0).toFixed(4)}
        </div>
      ) : null}
      {p && p.type === 'quantile' && p.quantile ? (
        <div className="muted">
          {t('report.quantilePrefix') + p.quantile.level + ': A=' + (p.quantile.quantileA ?? 0).toFixed(3) + ' · B=' +
            (p.quantile.quantileB ?? 0).toFixed(3) + ' · A CI [' + (p.quantile.ciALow ?? 0).toFixed(3) + ', ' +
            (p.quantile.ciAHigh ?? 0).toFixed(3) + '] (' + t('report.distFree') + ')'}
        </div>
      ) : null}
      {r.holm ? (
        <>
          <h4>{t('report.holm')}</h4>
          <p className="muted">
            {t('report.holmNote') + ' · ' + r.holm.familyId + ' · ' + r.holm.registrationVersion}
          </p>
          {r.holm.error ? (
            <div className="banner err">{r.holm.error}</div>
          ) : (
            <table className="grid">
              <thead>
                <tr>
                  <th>ID</th>
                  <th>{t('th.metric')}</th>
                  <th>{t('report.pair')}</th>
                  <th>{t('report.rawP')}</th>
                  <th>{t('report.adjustedP')}</th>
                  <th>{t('report.effectiveAlpha')}</th>
                  <th>{t('report.rank')}</th>
                  <th>{t('report.decision')}</th>
                </tr>
              </thead>
              <tbody>
                {(r.holm.comparisons || []).map(c => (
                  <tr key={c.id}>
                    <td>{c.id}</td>
                    <td>{c.metric?.key}</td>
                    <td>{c.control + ' → ' + c.treatment}</td>
                    <td>{c.rawP}</td>
                    <td>{c.adjustedP}</td>
                    <td>{c.effectiveAlpha}</td>
                    <td>{c.rank}</td>
                    <td>
                      <Chip
                        label={c.rejected ? t('chip.significant') : t('chip.notSignificant')}
                        cls={c.rejected ? 'green' : 'gray'}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      ) : null}
      <h4>
        {t('report.sequential')} <span className="muted">{t('report.sequentialNote')}</span>
      </h4>
      {r.sequential ? (
        <div>
          {t('report.anytimeP') + ' = ' + (r.sequential.pValue ?? 0).toExponential(3) + ' '}
          <Chip
            label={r.sequential.significant ? t('chip.sigAnyLook') : t('chip.notSignificant')}
            cls={r.sequential.significant ? 'green' : 'gray'}
          />
          {' · CS [' + (r.sequential.csLow ?? 0).toFixed(4) + ', ' + (r.sequential.csHigh ?? 0).toFixed(4) + '] · τ=' +
            r.sequential.tau + ' · logΛ=' + (r.sequential.logLambda ?? 0).toFixed(3)}
        </div>
      ) : (
        <div className="muted">{t('report.seqNotComputed')}</div>
      )}
      <h4>
        {t('report.bayesian')} <span className="muted">{t('report.bayesianNote')}</span>
      </h4>
      {r.bayesian?.mean ? (
        <div>
          {'P(B>A) = ' + (r.bayesian.mean.probBSuperior ?? 0).toFixed(4) + ' '}
          <Chip label={t('chip.probSuperior')} cls="teal" />
          {' · ' + t('report.expectedLoss') + ' = ' + (r.bayesian.mean.expectedLossB ?? 0).toExponential(3) +
            ' · 95% CrI [' + (r.bayesian.mean.credibleLow ?? 0).toFixed(4) + ', ' +
            (r.bayesian.mean.credibleHigh ?? 0).toFixed(4) + ']'}
        </div>
      ) : r.bayesian?.proportion ? (
        <div>
          {'P(B>A) = ' + (r.bayesian.proportion.probBSuperior ?? 0).toFixed(4) + ' '}
          <Chip label={t('chip.probSuperior')} cls="teal" />
          {' · ' + t('report.expectedLoss') + ' = ' + (r.bayesian.proportion.expectedLossB ?? 0).toExponential(3) +
            ' · posterior A=Beta(' + (r.bayesian.proportion.postAA ?? 0).toFixed(1) + ',' +
            (r.bayesian.proportion.postAB ?? 0).toFixed(1) + ') B=Beta(' + (r.bayesian.proportion.postBA ?? 0).toFixed(1) +
            ',' + (r.bayesian.proportion.postBB ?? 0).toFixed(1) + ')'}
        </div>
      ) : (
        <div className="muted">{t('report.notComputed')}</div>
      )}
      <h4>{t('exp.guardrails')}</h4>
      {(r.guardrails || []).length ? (
        <ul className="plain">
          {(r.guardrails || []).map((g, i) => (
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

// BanditReport ports renderBanditReport: banner + frozen/active state,
// metric reference, the no-fixed-decision note, policy/state/watermark,
// pending/missing/fallback counters, delivery diagnostics only for onRender,
// SRM alarm and freeze reason, the per-variant posterior table, health issues
// and guardrail lines.
export function BanditReport({ report }: { report: ExperimentReport }): ReactElement | null {
  const { t } = useTranslation();
  const b = report.bandit;
  if (!b) return null;
  return (
    <>
      <div className={'banner ' + (report.healthy ? 'ok' : 'err')}>
        <b>{t('report.banditTitle')}</b>
        {' · ' + (b.frozen ? t('report.banditFrozen') : t('report.banditActive'))}
      </div>
      <MetricReferenceSummary report={report} t={t} />
      <p className="muted">{t('report.banditNoFixed')}</p>
      <div>
        {t('exp.policyVersion') + ': '}
        <span className="mono">{b.policyVersion}</span>
        {' · state: '}
        <span className="mono">{b.stateVersion}</span>
        {' · ' + t('report.watermark') + ': '}
        <span className="mono">{b.feedbackWatermark}</span>
      </div>
      <div>
        {t('report.banditPending') + ': ' + b.pending + ' · ' + t('report.banditMissing') + ': ' + b.missingFeedback +
          ' · ' + t('report.banditFallback') + ': ' + b.fallbackAssignments}
      </div>
      {report.exposureMode === 'onRender' ? <Diagnostics d={report.diagnostics} /> : null}
      {b.srmAlarm ? <div className="banner err">{t('report.banditSrm')}</div> : null}
      {b.freezeReason ? <div className="muted">{b.freezeReason}</div> : null}
      <table className="grid">
        <thead>
          <tr>
            <th>{t('th.variant')}</th>
            <th>{t('report.banditTrials')}</th>
            <th>{t('report.banditSuccesses')}</th>
            <th>{t('report.banditPosterior')}</th>
          </tr>
        </thead>
        <tbody>
          {(report.perVariant || []).map((v, i) => (
            <tr key={v.variant}>
              <td>{v.variant}</td>
              <td>{b.trials[i]}</td>
              <td>{b.successes[i]}</td>
              <td>{(b.posteriorMean[i] ?? 0).toFixed(4)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {(report.healthIssues || []).map((x, i) => (
        <div className="banner err" key={i}>
          {x}
        </div>
      ))}
      {(report.guardrails || []).map((g, i) => (
        <div key={i}>{(g.Metric ?? g.metric) + ': ' + (g.Detail ?? g.detail)}</div>
      ))}
    </>
  );
}
