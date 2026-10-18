// vizSrmBar.test.tsx pins the R3 SRM figure (web-pages/src/viz/SrmBar.tsx):
// paired bars per variant (observed solid, expected hollow) proportional to
// counts, the grid table as the textual equivalent with the observed share,
// the cross-check that the SVG summary and the table cells come from the
// same numbers, and the silent degradation when the arrays are missing or
// mismatched — the χ²/p text line then stands alone, nothing is fabricated.
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import type { ExperimentReport } from '@saker/grayling-client';
import { SrmBar } from '@saker/grayling-pages/viz/SrmBar';
import { ReportView } from '@saker/grayling-pages/workbench/ReportView';
import { expectNoA11yViolations } from './helpers';

const PROPS = { observed: [502, 498], expected: [500, 500], labels: ['control', 'treatment'] };

describe('SrmBar', () => {
  it('paints one solid+hollow pair per variant, lengths proportional to counts', () => {
    const { container } = render(<SrmBar {...PROPS} />);
    // one legend swatch + two data bars per class
    const obs = container.querySelectorAll('.viz-bar-obs');
    const exp = container.querySelectorAll('.viz-bar-exp');
    expect(obs).toHaveLength(3);
    expect(exp).toHaveLength(3);
    // max count (502) takes the full bar area (212px); the rest scale down
    expect(Number(obs[1].getAttribute('width'))).toBeCloseTo(212);
    expect(Number(obs[2].getAttribute('width'))).toBeCloseTo((498 / 502) * 212);
    expect(Number(exp[1].getAttribute('width'))).toBeCloseTo((500 / 502) * 212);
  });

  it('renders the grid table with scoped column headers and the observed share', () => {
    render(<SrmBar {...PROPS} />);
    const table = screen.getByRole('table');
    const headers = within(table as HTMLElement).getAllByRole('columnheader');
    expect(headers.map(h => h.textContent)).toEqual(['Variant', 'Observed', 'Expected', 'Share']);
    for (const h of headers) expect(h.getAttribute('scope')).toBe('col');
    const rows = within(table as HTMLElement).getAllByRole('row');
    expect(rows).toHaveLength(3);
    expect(within(rows[1]).getByText('502')).toBeTruthy();
    expect(within(rows[1]).getByText('50.2%')).toBeTruthy();
    expect(within(rows[2]).getByText('498')).toBeTruthy();
    expect(within(rows[2]).getByText('49.8%')).toBeTruthy();
  });

  it('keeps the SVG summary and the table cells sourced from the same numbers', () => {
    render(<SrmBar {...PROPS} />);
    const svg = screen.getByRole('img', { name: /Observed versus expected/ });
    const label = svg.getAttribute('aria-label') || '';
    expect(label).toContain('control: observed 502, expected 500');
    expect(label).toContain('treatment: observed 498, expected 500');
    const table = screen.getByRole('table');
    for (const n of [502, 498, 500]) {
      expect(label).toContain(String(n));
      expect(within(table as HTMLElement).getAllByText(String(n)).length).toBeGreaterThan(0);
    }
  });

  it('renders nothing when arrays are missing, mismatched or non-finite', () => {
    const cases: Array<Parameters<typeof SrmBar>[0]> = [
      { labels: ['a', 'b'] },
      { observed: [1, 2], labels: ['a', 'b'] },
      { observed: [1, 2], expected: [1], labels: ['a', 'b'] },
      { observed: [1, 2], expected: [1, 2], labels: ['a'] },
      { observed: [1, NaN], expected: [1, 2], labels: ['a', 'b'] },
      { observed: [], expected: [], labels: [] },
    ];
    for (const props of cases) {
      const { container, unmount } = render(<SrmBar {...props} />);
      expect(container.querySelector('svg')).toBeNull();
      expect(container.querySelector('table')).toBeNull();
      unmount();
    }
  });

  it('passes axe with no violations', async () => {
    const { container } = render(<SrmBar {...PROPS} />);
    await expectNoA11yViolations(container);
  });
});

describe('SrmBar in ReportView', () => {
  function reportWith(srm: ExperimentReport['srm']): ExperimentReport {
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
      minRuntimeSatisfied: true,
      srm,
      perVariant: [
        { variant: 'control', exposedUnits: 500, randomizedUnits: 502, exposures: 510, convertedUnits: 100, primaryValue: 0.2 },
        { variant: 'treatment', exposedUnits: 490, randomizedUnits: 498, exposures: 505, convertedUnits: 120, primaryValue: 0.24 },
      ],
    };
  }

  it('replaces the JSON blob with the figure plus table, keeping the χ²/p line', () => {
    const { container } = render(
      <ReportView report={reportWith({ chiSquare: 1.21, pValue: 0.271, healthy: true, observed: [502, 498], expected: [500, 500] })} locale="en" />,
    );
    // the legacy pre blob is gone
    expect(container.querySelector('pre')).toBeNull();
    // the text line stays
    expect(screen.getByText(/χ² = 1\.21 · p = 0\.271/)).toBeTruthy();
    // the figure and the table render the same numbers
    const svg = screen.getByRole('img', { name: /Observed versus expected/ });
    expect(svg.getAttribute('aria-label')).toContain('control: observed 502, expected 500');
    const tables = screen.getAllByRole('table');
    const srmTable = tables.find(tb => within(tb as HTMLElement).queryByRole('columnheader', { name: 'Observed' }));
    expect(srmTable).toBeTruthy();
    expect(within(srmTable as HTMLElement).getByText('502')).toBeTruthy();
  });

  it('array mismatch degrades to the text line alone', () => {
    const { container } = render(
      <ReportView report={reportWith({ chiSquare: 1.21, pValue: 0.271, healthy: true, observed: [502], expected: [500, 500] })} locale="en" />,
    );
    expect(screen.getByText(/χ² = 1\.21/)).toBeTruthy();
    expect(screen.queryByRole('img', { name: /Observed versus expected/ })).toBeNull();
    // no SRM table (only the per-variant grid remains)
    expect(within(container).queryByRole('columnheader', { name: 'Observed' })).toBeNull();
  });

  it('null SRM keeps the no-data note', () => {
    render(<ReportView report={reportWith(null)} locale="en" />);
    expect(screen.getByText('No SRM data yet (no randomized exposures).')).toBeTruthy();
    expect(screen.queryByRole('img', { name: /Observed versus expected/ })).toBeNull();
  });
});
