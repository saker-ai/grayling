import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// ConfidenceInterval: the R3 primary-metric interval sketch (baseline,
// dashed null reference, whisker+caps, point estimate) in --chart-*/--series-*
// tokens. Additive — the numeric CI line stays below. Degrades honestly:
// 'unavailable'/non-finite → muted note; 'insufficient' → empty track + note.

export type ConfidenceIntervalState = 'ready' | 'insufficient' | 'unavailable';

export interface ConfidenceIntervalProps {
  estimate: number;
  low: number;
  high: number;
  // nullValue: the no-effect reference — 0 for absolute diffs (default),
  // ratio-scale callers pass 1.
  nullValue?: number;
  // format renders the endpoint tick labels so they match the numeric line.
  format: (v: number) => string;
  // ariaLabel: the full textual equivalent, pre-composed by the caller
  // through i18n so the sentence stays one localizable unit.
  ariaLabel: string;
  state: ConfidenceIntervalState;
}

const W = 320;
const H = 48;
const AXIS_Y = 24;
const PAD = 10;

export function ConfidenceInterval(props: ConfidenceIntervalProps): ReactElement {
  const { t } = useTranslation();
  const nullValue = props.nullValue ?? 0;

  if (props.state === 'unavailable') {
    return <div className="muted">{t('viz.ciUnavailable')}</div>;
  }
  if (props.state === 'insufficient') {
    return (
      <>
        <svg viewBox={'0 0 ' + W + ' ' + H} className="viz" aria-hidden="true" focusable="false">
          <line x1={PAD} x2={W - PAD} y1={AXIS_Y} y2={AXIS_Y} className="viz-baseline" />
        </svg>
        <div className="muted">{t('viz.ciInsufficient')}</div>
      </>
    );
  }

  const { estimate, low, high } = props;
  if (![estimate, low, high, nullValue].every(Number.isFinite)) {
    return <div className="muted">{t('viz.ciUnavailable')}</div>;
  }

  let dMin = Math.min(low, nullValue, estimate);
  let dMax = Math.max(high, nullValue, estimate);
  if (dMin === dMax) {
    dMin -= 1;
    dMax += 1;
  }
  const x = (v: number): number => PAD + ((v - dMin) / (dMax - dMin)) * (W - 2 * PAD);
  const hasWidth = high > low;
  // Narrow intervals skip the endpoint labels (they would collide); the
  // numeric line below always carries the values.
  const labelGap = hasWidth ? x(high) - x(low) : Infinity;
  const showLabels = labelGap >= 36;
  const labelX = (v: number): number => Math.min(W - 16, Math.max(16, x(v)));

  return (
    <svg viewBox={'0 0 ' + W + ' ' + H} className="viz" role="img" aria-label={props.ariaLabel}>
      <title>{props.ariaLabel}</title>
      <line x1={PAD} x2={W - PAD} y1={AXIS_Y} y2={AXIS_Y} className="viz-baseline" />
      <line x1={x(nullValue)} x2={x(nullValue)} y1={8} y2={H - 14} className="viz-null" />
      {hasWidth ? (
        <>
          <line x1={x(low)} x2={x(high)} y1={AXIS_Y} y2={AXIS_Y} className="viz-range" />
          <line x1={x(low)} x2={x(low)} y1={AXIS_Y - 6} y2={AXIS_Y + 6} className="viz-cap" />
          <line x1={x(high)} x2={x(high)} y1={AXIS_Y - 6} y2={AXIS_Y + 6} className="viz-cap" />
        </>
      ) : null}
      <circle cx={x(estimate)} cy={AXIS_Y} r={4} className="viz-fill-1" />
      {showLabels ? (
        <>
          <text x={labelX(low)} y={H - 2} textAnchor="middle" className="viz-num-dim">
            {props.format(low)}
          </text>
          {hasWidth ? (
            <text x={labelX(high)} y={H - 2} textAnchor="middle" className="viz-num-dim">
              {props.format(high)}
            </text>
          ) : null}
        </>
      ) : null}
    </svg>
  );
}
