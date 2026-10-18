// vizSampleFlow.test.tsx pins the R3 sample-flow bars
// (web-pages/src/viz/SampleFlow.tsx): widths proportional to count/max, the
// relative-to-previous percentage text, the empty track with an em dash for
// a null count (never a borrowed number), and the all-empty degradation to
// nothing. The Diagnostics wiring tests assert the three number cards stay
// in place above the bars and that onDecision reports without assignment
// events leave the first stage empty.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ExperimentDiagnostics } from '@saker/grayling-client';
import { Diagnostics } from '@saker/grayling-pages/workbench/Diagnostics';
import { SampleFlow } from '@saker/grayling-pages/viz/SampleFlow';
import { expectNoA11yViolations } from './helpers';

const STAGES = [
  { key: 'a', label: 'Assigned', count: 1000 },
  { key: 'e', label: 'Exposed', count: 500 },
  { key: 'c', label: 'Converted', count: 100 },
];

describe('SampleFlow', () => {
  it('paints bars proportional to count/max with value and percent-of-previous text', () => {
    const { container } = render(<SampleFlow stages={STAGES} />);
    const bars = container.querySelectorAll('.viz-bar-1');
    expect(bars).toHaveLength(3);
    expect(Number(bars[0].getAttribute('width'))).toBeCloseTo(320);
    expect(Number(bars[1].getAttribute('width'))).toBeCloseTo(160);
    expect(Number(bars[2].getAttribute('width'))).toBeCloseTo(32);
    expect(container.textContent).toContain('1000');
    expect(container.textContent).toContain('500 · 50%');
    expect(container.textContent).toContain('100 · 20%');
  });

  it('null count draws the empty track with an em dash and no bar', () => {
    const { container } = render(
      <SampleFlow stages={[STAGES[0], { key: 'e', label: 'Exposed', count: null }, STAGES[2]]} />,
    );
    expect(container.querySelectorAll('.viz-bar-1')).toHaveLength(2);
    expect(container.querySelectorAll('.viz-track')).toHaveLength(3);
    expect(container.textContent).toContain('—');
  });

  it('skips the percent text when the previous stage is missing or zero', () => {
    const { container } = render(
      <SampleFlow stages={[{ key: 'a', label: 'A', count: 0 }, { key: 'b', label: 'B', count: 50 }]} />,
    );
    expect(container.textContent).toContain('50');
    expect(container.textContent).not.toContain('%');
  });

  it('renders nothing when every stage is null or zero', () => {
    const zeroes = render(
      <SampleFlow stages={[{ key: 'a', label: 'A', count: null }, { key: 'b', label: 'B', count: 0 }]} />,
    );
    expect(zeroes.container.querySelector('svg')).toBeNull();
    expect(zeroes.container.textContent).toBe('');
    const empty = render(<SampleFlow stages={[]} />);
    expect(empty.container.querySelector('svg')).toBeNull();
  });

  it('summarizes every stage in the group aria-label', () => {
    render(<SampleFlow stages={STAGES} />);
    const svg = screen.getByRole('img', { name: /Sample flow:/ });
    const label = svg.getAttribute('aria-label') || '';
    expect(label).toContain('Assigned: 1000');
    expect(label).toContain('Exposed: 500');
    expect(label).toContain('Converted: 100');
  });

  it('passes axe with no violations', async () => {
    const { container } = render(<SampleFlow stages={STAGES} />);
    await expectNoA11yViolations(container);
  });
});

describe('SampleFlow in Diagnostics', () => {
  function diag(over: Partial<ExperimentDiagnostics> = {}): ExperimentDiagnostics {
    return {
      exposureMode: 'onDecision',
      perVariant: [
        {
          variant: 'control',
          assignments: 500,
          randomizedAssignments: 500,
          exposures: 510,
          randomizedExposures: 510,
          unexposed: 0,
          conversions: 100,
          duplicateExposures: 0,
        },
        {
          variant: 'treatment',
          assignments: 500,
          randomizedAssignments: 500,
          exposures: 505,
          randomizedExposures: 505,
          unexposed: 0,
          conversions: 120,
          duplicateExposures: 0,
        },
      ],
      ...over,
    };
  }

  it('keeps the number cards and adds the bars below them', () => {
    const { container } = render(<Diagnostics d={diag()} />);
    const cards = container.querySelector('ol.sample-flow');
    const bars = container.querySelector('.viz-flow svg');
    expect(cards).toBeTruthy();
    expect(bars).toBeTruthy();
    // the cards stay the primary reading: they precede the bars
    expect(cards!.compareDocumentPosition(bars!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // assigned = 500 + 500 = 1000 across the two variants
    expect(bars!.getAttribute('aria-label')).toContain('1000');
  });

  it('onDecision without assignment events leaves the first stage an empty track', () => {
    const d = diag({
      perVariant: [
        {
          variant: 'control',
          assignments: 0,
          randomizedAssignments: 0,
          exposures: 510,
          randomizedExposures: 510,
          unexposed: 0,
          conversions: 100,
          duplicateExposures: 0,
        },
      ],
    });
    const { container } = render(<Diagnostics d={d} />);
    const flow = container.querySelector('.viz-flow');
    expect(flow).toBeTruthy();
    // first stage count is null → em dash, and only two bars are painted
    expect(flow!.textContent).toContain('—');
    expect(flow!.querySelectorAll('.viz-bar-1')).toHaveLength(2);
    // the card above shows the same em dash, not a zero
    const firstCard = container.querySelector('ol.sample-flow .sample-flow-stage strong');
    expect(firstCard!.textContent).toBe('—');
  });

  it('all-zero counters render the cards alone, no bars', () => {
    const { container } = render(<Diagnostics d={diag({ perVariant: [] })} />);
    expect(container.querySelector('ol.sample-flow')).toBeTruthy();
    expect(container.querySelector('.viz-flow')).toBeNull();
  });
});
