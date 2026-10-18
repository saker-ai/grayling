import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// SampleFlow: R3 proportional bars under the diagnostics number cards (which
// stay the primary reading). Width ∝ count/max; null count → empty track +
// em dash, never a borrowed number; all-null/zero → renders nothing.

export interface SampleFlowStage {
  key: string;
  label: string;
  count: number | null;
}

export interface SampleFlowProps {
  stages: SampleFlowStage[];
  note?: string;
}

const W = 320;
const ROW_H = 34;
const LABEL_Y = 10;
const BAR_Y = 16;
const BAR_H = 14;

export function SampleFlow({ stages, note }: SampleFlowProps): ReactElement | null {
  const { t } = useTranslation();
  if (!stages.length) return null;
  // Non-finite or negative counts are treated as missing, never as zero.
  const counts = stages.map(s =>
    s.count !== null && Number.isFinite(s.count) && s.count >= 0 ? s.count : null,
  );
  const max = Math.max(...counts.map(c => c ?? 0));
  if (!(max > 0)) return null;

  const ariaLabel = t('viz.flowBars', {
    stages: stages
      .map((s, i) => t('viz.flowStage', { label: s.label, count: counts[i] === null ? '—' : String(counts[i]) }))
      .join(' · '),
  });

  return (
    <div className="viz-flow">
      <svg
        viewBox={'0 0 ' + W + ' ' + stages.length * ROW_H}
        className="viz"
        role="img"
        aria-label={ariaLabel}
      >
        <title>{ariaLabel}</title>
        {stages.map((s, i) => {
          const count = counts[i];
          const rowY = i * ROW_H;
          const barW = count === null ? 0 : (count / max) * W;
          const prev = i > 0 ? counts[i - 1] : null;
          const pct = count !== null && prev !== null && prev > 0 ? Math.round((1000 * count) / prev) / 10 : null;
          const valueText = count === null ? '—' : String(count) + (pct === null ? '' : ' · ' + pct + '%');
          // inside the bar when it fits (rough width estimate), else just
          // past the bar end on the track
          const fitsInside = count !== null && barW >= valueText.length * 6 + 12;
          return (
            <g key={s.key}>
              {pct !== null ? <title>{t('viz.flowOfPrevious', { pct })}</title> : null}
              <text x={0} y={rowY + LABEL_Y} className="viz-label">
                {s.label}
              </text>
              <rect x={0} y={rowY + BAR_Y} width={W} height={BAR_H} rx={2} className="viz-track" />
              {barW > 0 ? (
                <rect x={0} y={rowY + BAR_Y} width={barW} height={BAR_H} rx={2} className="viz-bar-1" />
              ) : null}
              <text
                x={count === null ? 6 : fitsInside ? barW - 5 : barW + 5}
                y={rowY + BAR_Y + 11}
                textAnchor={fitsInside ? 'end' : 'start'}
                className={count === null ? 'viz-num-dim' : 'viz-num'}
              >
                {valueText}
              </text>
            </g>
          );
        })}
      </svg>
      {note ? <p className="muted">{note}</p> : null}
    </div>
  );
}
