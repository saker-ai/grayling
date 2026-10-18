import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// BanditArms: R3 overlay above the bandit posterior table (which stays as
// the textual equivalent) — one bar per arm, length ∝ posterior mean on the
// 0–1 scale, mean on the bar, trials/successes as text. Missing/non-finite
// mean → empty track + em dash; no usable mean at all → renders nothing.

export interface BanditArm {
  key: string;
  label: string;
  mean: number | null;
  trials: number | null;
  successes: number | null;
}

export interface BanditArmsProps {
  arms: BanditArm[];
}

const W = 320;
const ROW_H = 34;
const LABEL_Y = 10;
const BAR_Y = 16;
const BAR_H = 14;

export function BanditArms({ arms }: BanditArmsProps): ReactElement | null {
  const { t } = useTranslation();
  if (!arms.length) return null;
  const means = arms.map(a => (a.mean !== null && Number.isFinite(a.mean) ? Math.min(1, Math.max(0, a.mean)) : null));
  if (!means.some(m => m !== null)) return null;

  const ariaLabel = t('viz.banditBars', {
    arms: arms
      .map((a, i) =>
        t('viz.banditArm', {
          label: a.label,
          value: means[i] === null ? '—' : (means[i] as number).toFixed(4),
          trials: a.trials ?? '—',
          successes: a.successes ?? '—',
        }),
      )
      .join(' · '),
  });

  return (
    <div className="viz-bandit">
      <svg viewBox={'0 0 ' + W + ' ' + arms.length * ROW_H} className="viz" role="img" aria-label={ariaLabel}>
        <title>{ariaLabel}</title>
        {arms.map((a, i) => {
          const mean = means[i];
          const rowY = i * ROW_H;
          const barW = mean === null ? 0 : mean * W;
          const meanText = mean === null ? '—' : mean.toFixed(4);
          const counterText = (a.trials ?? '—') + '/' + (a.successes ?? '—');
          const fitsInside = mean !== null && barW >= meanText.length * 6 + 12;
          return (
            <g key={a.key}>
              <text x={0} y={rowY + LABEL_Y} className="viz-label">
                {a.label}
              </text>
              <text x={W} y={rowY + LABEL_Y} textAnchor="end" className="viz-num-dim">
                {counterText}
              </text>
              <rect x={0} y={rowY + BAR_Y} width={W} height={BAR_H} rx={2} className="viz-track" />
              {barW > 0 ? (
                <rect x={0} y={rowY + BAR_Y} width={barW} height={BAR_H} rx={2} className="viz-bar-1" />
              ) : null}
              <text
                x={mean === null ? 6 : fitsInside ? barW - 5 : barW + 5}
                y={rowY + BAR_Y + 11}
                textAnchor={fitsInside ? 'end' : 'start'}
                className={mean === null ? 'viz-num-dim' : 'viz-num'}
              >
                {meanText}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
