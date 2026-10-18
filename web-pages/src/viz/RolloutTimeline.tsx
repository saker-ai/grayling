import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// RolloutTimeline: R3 node-per-step overlay (plain HTML/CSS, no SVG) between
// the progress bar and the steps table, which stays the textual equivalent.
// Shape dual-codes state (filled done / ring current / diamond held / square
// stopped / hollow pending) beside the chips + hidden labels. CSS (incl. the
// ≤700px vertical flip) lives in the ro-timeline block of global.css.

export type RolloutStepStatus = 'done' | 'current' | 'held' | 'stopped' | 'pending';

export interface RolloutTimelineStep {
  name: string;
  ratio: number;
}

export interface RolloutTimelineProps {
  steps: RolloutTimelineStep[];
  currentStep: number;
  // ReleasePlan.state; only 'completed' / 'rolled_back' / 'paused' change
  // the node coding.
  state: string;
}

const STATUS_CHIP_KEY: Record<RolloutStepStatus, string> = {
  done: 'chip.done',
  current: 'chip.current',
  held: 'chip.heldHere',
  stopped: 'chip.stoppedHere',
  pending: 'chip.pending',
};

// statusForStep mirrors the chip logic in RolloutSteps exactly, so the
// timeline and the table can never disagree about a step's state.
export function statusForStep(state: string, currentStep: number, index: number): RolloutStepStatus {
  if (state === 'completed' || index < currentStep) return 'done';
  if (index === currentStep) {
    if (state === 'rolled_back') return 'stopped';
    if (state === 'paused') return 'held';
    return 'current';
  }
  return 'pending';
}

export function RolloutTimeline({ steps, currentStep, state }: RolloutTimelineProps): ReactElement | null {
  const { t } = useTranslation();
  if (!steps.length) return null;
  return (
    <ol className="ro-timeline" aria-label={t('viz.roTimeline')}>
      {steps.map((step, i) => {
        const status = statusForStep(state, currentStep, i);
        return (
          <li key={i} className={'ro-tl-step ro-tl-' + status}>
            <span className="ro-tl-node" aria-hidden="true" />
            <span className="ro-tl-text">
              <span className="ro-tl-name">{step.name || '—'}</span>
              <span className="ro-tl-ratio">{step.ratio}%</span>
              <span className="visually-hidden">{t(STATUS_CHIP_KEY[status])}</span>
            </span>
          </li>
        );
      })}
    </ol>
  );
}
