import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// SrmBar: R3 replacement for the legacy observed/expected JSON blob — paired
// bars (observed solid --series-1, expected hollow --chart-axis, ∝ counts)
// plus a grid table of the same numbers; the figure never stands alone.
// Missing/mismatched/non-finite arrays render nothing (text line carries on).
export interface SrmBarProps {
  observed?: number[];
  expected?: number[];
  // labels carries the variant names in count-array order.
  labels: string[];
}

const W = 320;
const LABEL_W = 96;
const BAR_X = LABEL_W + 8;
const BAR_W = W - BAR_X - 4;
const GROUP_H = 26;
const LEGEND_H = 18;

export function SrmBar({ observed, expected, labels }: SrmBarProps): ReactElement | null {
  const { t } = useTranslation();
  const valid =
    Array.isArray(observed) &&
    Array.isArray(expected) &&
    observed.length > 0 &&
    observed.length === expected.length &&
    labels.length >= observed.length &&
    observed.every(v => Number.isFinite(v) && v >= 0) &&
    expected.every(v => Number.isFinite(v) && v >= 0);
  if (!valid) return null;
  const obs = observed as number[];
  const exp = expected as number[];
  const names = labels.slice(0, obs.length);

  const max = Math.max(...obs, ...exp);
  const total = obs.reduce((sum, v) => sum + v, 0);
  const ariaLabel = t('viz.srmBars', {
    pairs: names
      .map((label, i) => t('viz.srmPair', { label, observed: obs[i], expected: exp[i] }))
      .join(' · '),
  });
  const height = LEGEND_H + names.length * GROUP_H + 2;

  return (
    <div className="viz-srm">
      <svg viewBox={'0 0 ' + W + ' ' + height} className="viz" role="img" aria-label={ariaLabel}>
        <title>{ariaLabel}</title>
        <rect x={BAR_X} y={5} width={12} height={8} className="viz-bar-obs" />
        <text x={BAR_X + 16} y={12} className="viz-label">
          {t('viz.srmObserved')}
        </text>
        <rect x={BAR_X + 92} y={5} width={12} height={8} className="viz-bar-exp" />
        <text x={BAR_X + 108} y={12} className="viz-label">
          {t('viz.srmExpected')}
        </text>
        {names.map((label, i) => {
          const y = LEGEND_H + i * GROUP_H;
          const obsW = max > 0 ? (obs[i] / max) * BAR_W : 0;
          const expW = max > 0 ? (exp[i] / max) * BAR_W : 0;
          return (
            <g key={label + ':' + i}>
              <text x={0} y={y + 15} className="viz-label">
                {label}
              </text>
              {obsW > 0 ? <rect x={BAR_X} y={y + 2} width={obsW} height={8} className="viz-bar-obs" /> : null}
              {expW > 0 ? <rect x={BAR_X} y={y + 13} width={expW} height={8} className="viz-bar-exp" /> : null}
            </g>
          );
        })}
      </svg>
      <table className="grid viz-srm-table">
        <thead>
          <tr>
            <th scope="col">{t('th.variant')}</th>
            <th scope="col">{t('viz.srmObserved')}</th>
            <th scope="col">{t('viz.srmExpected')}</th>
            <th scope="col">{t('viz.srmShare')}</th>
          </tr>
        </thead>
        <tbody>
          {names.map((label, i) => (
            <tr key={label + ':' + i}>
              <td className="mono">{label}</td>
              <td>{obs[i]}</td>
              <td>{exp[i]}</td>
              <td>{total > 0 ? ((100 * obs[i]) / total).toFixed(1) + '%' : '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
