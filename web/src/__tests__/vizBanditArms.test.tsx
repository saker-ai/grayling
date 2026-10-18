// vizBanditArms.test.tsx pins the R3 bandit-arms overlay
// (web-pages/src/viz/BanditArms.tsx): one bar per arm proportional to the
// posterior mean on its natural 0–1 scale, trials/successes as text, the
// empty track with an em dash for a missing mean, and the all-missing
// degradation to nothing. The BanditReport wiring test asserts the bars sit
// above the unchanged posterior table with the same numbers in both.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { ExperimentReport } from '@saker/grayling-client';
import { BanditArms } from '@saker/grayling-pages/viz/BanditArms';
import { ReportView } from '@saker/grayling-pages/workbench/ReportView';
import { expectNoA11yViolations } from './helpers';

const ARMS = [
  { key: 'control', label: 'control', mean: 0.2, trials: 500, successes: 100 },
  { key: 'treatment', label: 'treatment', mean: 0.24, trials: 505, successes: 121 },
];

describe('BanditArms', () => {
  it('paints bars proportional to the posterior mean on the 0–1 scale', () => {
    const { container } = render(<BanditArms arms={ARMS} />);
    const bars = container.querySelectorAll('.viz-bar-1');
    expect(bars).toHaveLength(2);
    expect(Number(bars[0].getAttribute('width'))).toBeCloseTo(64);
    expect(Number(bars[1].getAttribute('width'))).toBeCloseTo(76.8);
    expect(container.textContent).toContain('0.2000');
    expect(container.textContent).toContain('0.2400');
    expect(container.textContent).toContain('500/100');
    expect(container.textContent).toContain('505/121');
  });

  it('missing mean draws the empty track with an em dash and no bar', () => {
    const { container } = render(
      <BanditArms arms={[ARMS[0], { key: 't', label: 'treatment', mean: null, trials: null, successes: null }]} />,
    );
    expect(container.querySelectorAll('.viz-bar-1')).toHaveLength(1);
    expect(container.textContent).toContain('—');
    expect(container.textContent).toContain('—/—');
  });

  it('renders nothing when no arm has a usable mean', () => {
    const { container } = render(
      <BanditArms arms={[{ key: 'a', label: 'a', mean: null, trials: 3, successes: 1 }]} />,
    );
    expect(container.querySelector('svg')).toBeNull();
    expect(container.textContent).toBe('');
  });

  it('summarizes every arm in the group aria-label', () => {
    render(<BanditArms arms={ARMS} />);
    const svg = screen.getByRole('img', { name: /Posterior mean per arm:/ });
    const label = svg.getAttribute('aria-label') || '';
    expect(label).toContain('control: mean 0.2000, 500 trials, 100 successes');
    expect(label).toContain('treatment: mean 0.2400, 505 trials, 121 successes');
  });

  it('passes axe with no violations', async () => {
    const { container } = render(<BanditArms arms={ARMS} />);
    await expectNoA11yViolations(container);
  });
});

describe('BanditArms in BanditReport', () => {
  const REPORT: ExperimentReport = {
    experimentId: 'exp-1',
    epoch: 1,
    flagKey: 'checkout',
    state: 'running',
    generatedAt: '2026-01-05T12:00:00Z',
    evidenceVersion: 'ev-1',
    healthy: true,
    healthIssues: [],
    decision: { verdict: 'continue', reasons: [] },
    bandit: {
      policyVersion: 'p1',
      stateVersion: 3,
      feedbackWatermark: '2026-01-05T11:59:00Z',
      trials: [500, 505],
      successes: [100, 121],
      posteriorMean: [0.2, 0.24],
      pending: 2,
      missingFeedback: 1,
      fallbackAssignments: 0,
      srmAlarm: false,
      frozen: false,
    },
    perVariant: [
      { variant: 'control', exposedUnits: 500, randomizedUnits: 500, exposures: 510, convertedUnits: 100, primaryValue: 0.2 },
      { variant: 'treatment', exposedUnits: 505, randomizedUnits: 505, exposures: 505, convertedUnits: 121, primaryValue: 0.24 },
    ],
  };

  it('renders the bars above the unchanged posterior table, same numbers in both', () => {
    const { container } = render(<ReportView report={REPORT} locale="en" />);
    const svg = screen.getByRole('img', { name: /Posterior mean per arm:/ });
    const table = container.querySelector('table.grid');
    expect(table).toBeTruthy();
    // bars precede the table
    expect(svg.compareDocumentPosition(table!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // the same posterior values appear in the bars and in the table cells
    expect(svg.getAttribute('aria-label')).toContain('0.2000');
    expect(svg.getAttribute('aria-label')).toContain('0.2400');
    const cells = Array.from(table!.querySelectorAll('td')).map(td => td.textContent);
    expect(cells).toContain('0.2000');
    expect(cells).toContain('0.2400');
    expect(cells).toContain('500');
    expect(cells).toContain('121');
  });
});
