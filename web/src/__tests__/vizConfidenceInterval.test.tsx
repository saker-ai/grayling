// vizConfidenceInterval.test.tsx pins the R3 interval sketch
// (web-pages/src/viz/ConfidenceInterval.tsx): the ready state paints the
// baseline, the dashed null reference, the whisker with end caps and the
// point estimate; the degraded states never fabricate a shape (non-finite
// numbers or 'unavailable' → muted note, 'insufficient' → an empty
// decorative track plus the not-mature note); a zero-width interval draws
// the point alone. The ReportView wiring tests assert the numeric CI text
// line stays verbatim with the SVG aria-label composed from the same values.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ExperimentReport } from '@saker/grayling-client';
import { ConfidenceInterval } from '@saker/grayling-pages/viz/ConfidenceInterval';
import type { ConfidenceIntervalProps } from '@saker/grayling-pages/viz/ConfidenceInterval';
import { ReportView } from '@saker/grayling-pages/workbench/ReportView';
import { expectNoA11yViolations } from './helpers';

const fmt = (v: number): string => String(v);

function renderCI(props: Partial<ConfidenceIntervalProps> = {}) {
  return render(
    <ConfidenceInterval
      estimate={0.04}
      low={0.01}
      high={0.07}
      format={fmt}
      ariaLabel="Confidence interval 0.01 to 0.07, point estimate 0.04, null reference at 0."
      state="ready"
      {...props}
    />,
  );
}

describe('ConfidenceInterval', () => {
  it('ready state paints baseline, dashed null reference, whisker with caps and the point', () => {
    const { container } = renderCI();
    const svg = screen.getByRole('img', { name: /Confidence interval 0\.01 to 0\.07/ });
    expect(svg.tagName.toLowerCase()).toBe('svg');
    expect(container.querySelector('.viz-baseline')).toBeTruthy();
    expect(container.querySelector('.viz-null')).toBeTruthy();
    expect(container.querySelector('.viz-range')).toBeTruthy();
    expect(container.querySelectorAll('.viz-cap')).toHaveLength(2);
    expect(container.querySelector('.viz-fill-1')).toBeTruthy();
    // endpoint tick labels come from the caller's formatter
    expect(container.textContent).toContain('0.01');
    expect(container.textContent).toContain('0.07');
  });

  it('zero-width interval draws only the point (no whisker, no caps)', () => {
    const { container } = renderCI({ low: 0.04, high: 0.04 });
    expect(container.querySelector('.viz-range')).toBeNull();
    expect(container.querySelectorAll('.viz-cap')).toHaveLength(0);
    expect(container.querySelector('.viz-fill-1')).toBeTruthy();
  });

  it.each([{ estimate: NaN }, { low: Infinity }, { high: NaN }] as Array<Partial<ConfidenceIntervalProps>>)(
    'non-finite input drops the SVG for the unavailable note: %o',
    patch => {
      const { container } = renderCI(patch);
      expect(container.querySelector('svg')).toBeNull();
      expect(screen.getByText('Interval unavailable; see the numeric values.')).toBeTruthy();
    },
  );

  it('unavailable state renders the muted note only', () => {
    const { container } = renderCI({ state: 'unavailable' });
    expect(container.querySelector('svg')).toBeNull();
    expect(screen.getByText('Interval unavailable; see the numeric values.')).toBeTruthy();
  });

  it('insufficient state shows an empty decorative track plus the not-mature note', () => {
    const { container } = renderCI({ state: 'insufficient' });
    const svg = container.querySelector('svg');
    expect(svg).toBeTruthy();
    expect(svg?.getAttribute('aria-hidden')).toBe('true');
    expect(container.querySelector('.viz-range')).toBeNull();
    expect(container.querySelector('.viz-fill-1')).toBeNull();
    expect(screen.getByText('Data not yet mature')).toBeTruthy();
  });

  it('moves the dashed reference when nullValue is overridden (ratio scale)', () => {
    const a = renderCI({ nullValue: 0 });
    const x0 = a.container.querySelector('.viz-null')?.getAttribute('x1');
    a.unmount();
    const b = renderCI({ nullValue: 1 });
    const x1 = b.container.querySelector('.viz-null')?.getAttribute('x1');
    expect(x0).toBeTruthy();
    expect(x1).toBeTruthy();
    expect(x0).not.toEqual(x1);
  });

  it('passes axe with no violations', async () => {
    const { container } = renderCI();
    await expectNoA11yViolations(container);
  });
});

describe('ConfidenceInterval in ReportView', () => {
  function reportWith(primary: ExperimentReport['primary'], minRuntimeSatisfied = true): ExperimentReport {
    return {
      experimentId: 'exp-1',
      epoch: 1,
      flagKey: 'checkout',
      state: 'running',
      generatedAt: '2026-01-05T12:00:00Z',
      evidenceVersion: 'ev-1',
      healthy: true,
      healthIssues: [],
      decision: { verdict: 'hold', reasons: [] },
      analysisMethod: 'fixed-horizon',
      minRuntimeSatisfied,
      primary,
    };
  }

  const PRIMARY = {
    type: 'proportion',
    pValue: 0.0123,
    diff: 0.04,
    relativeDiff: 0.2,
    relativeMeaningful: true,
    ciLow: 0.01,
    ciHigh: 0.07,
    significant: true,
  };

  it('renders the SVG above the unchanged CI text line, both from the same values', () => {
    render(<ReportView report={reportWith(PRIMARY)} locale="en" />);
    const svg = screen.getByRole('img', { name: /Confidence interval/ });
    const label = svg.getAttribute('aria-label') || '';
    expect(label).toContain('0.01');
    expect(label).toContain('0.07');
    expect(label).toContain('0.04');
    // the numeric text line stays verbatim and carries the same numbers
    expect(screen.getByText(/CI \[0\.01, 0\.07\]/)).toBeTruthy();
  });

  it('before min runtime the interval stays an empty track with the not-mature note', () => {
    render(<ReportView report={reportWith(PRIMARY, false)} locale="en" />);
    expect(screen.getByText('Data not yet mature')).toBeTruthy();
    expect(screen.queryByRole('img', { name: /Confidence interval/ })).toBeNull();
    // the numeric line is still there
    expect(screen.getByText(/CI \[0\.01, 0\.07\]/)).toBeTruthy();
  });

  it('non-finite CI from the server degrades to the unavailable note', () => {
    render(<ReportView report={reportWith({ ...PRIMARY, ciLow: NaN })} locale="en" />);
    expect(screen.getByText('Interval unavailable; see the numeric values.')).toBeTruthy();
    expect(screen.queryByRole('img', { name: /Confidence interval/ })).toBeNull();
  });

  it('without a primary result no interval renders', () => {
    const report = reportWith(undefined);
    report.primaryError = 'primary metric not computable';
    render(<ReportView report={report} locale="en" />);
    expect(screen.queryByRole('img', { name: /Confidence interval/ })).toBeNull();
    expect(screen.getByText('primary metric not computable')).toBeTruthy();
  });
});
