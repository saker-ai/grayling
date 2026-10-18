// report.test.tsx pins the evidence-zone report renderer (ReportView +
// ReportSummary, web-pages/src/workbench/ReportView.tsx) against six report
// shapes: healthy proportion (every section populated), ratio-delta,
// quantile, CUPED, unhealthy-with-breaches (empty sequential/Bayesian slots)
// and the bandit report. Every populated field is asserted with its exact
// rendering; the sequential and Bayesian sections always carry their
// "separate view, not the decision gate" labels (render_contract ①).
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import { ExperimentDetailPage } from '@saker/grayling-pages/pages/ExperimentDetailPage';
import type { ExperimentDetail, ExperimentReport } from '@saker/grayling-client';
import { fetchMock, renderPage } from './helpers';

const ROUTE = {
  path: '/console/:ws/:env/experiments/:expId/:tab',
  route: '/console/ws1/prod/experiments/exp-1/evidence',
};
const EXP_PATH = '/api/v1/admin/experiments/exp-1';

const EXPERIMENT: ExperimentDetail = {
  id: 'exp-1',
  flagKey: 'checkout',
  state: 'running',
  epoch: 3,
  flagRevision: 7,
  environmentId: 'prod',
  createdBy: 'alice',
  unit: 'user_id',
  variants: ['control', 'treatment'],
  proportions: { control: 50, treatment: 50 },
  primaryMetric: { key: 'checkout_success', type: 'proportion', direction: 'increase' },
  alpha: 0.05,
  power: 0.8,
  mde: 0.01,
  minSamplePerArm: 1000,
  minRuntime: 604800000000000,
  analysisWindow: 604800000000000,
  exposureMode: 'onDecision',
};

function baseReport(): ExperimentReport {
  return {
    experimentId: 'exp-1',
    epoch: 3,
    flagKey: 'checkout',
    state: 'running',
    generatedAt: '2026-01-05T12:00:00Z',
    evidenceVersion: 'ev-9',
    window: {
      version: 'w2',
      start: '2026-01-01T00:00:00Z',
      end: '2026-01-08T00:00:00Z',
      watermark: '2026-01-05T11:59:00Z',
      complete: true,
      backend: 'event-log',
      recomputable: true,
      issues: [],
    },
    analysisMethod: 'fixed-horizon',
    healthy: true,
    healthIssues: [],
    srm: { chiSquare: 1.21, pValue: 0.271, healthy: true, observed: [502, 498], expected: [500, 500] },
    perVariant: [
      { variant: 'control', exposedUnits: 500, randomizedUnits: 502, exposures: 510, convertedUnits: 100, primaryValue: 0.2 },
      { variant: 'treatment', exposedUnits: 490, randomizedUnits: 498, exposures: 505, convertedUnits: 120, primaryValue: 0.24 },
    ],
    primary: {
      type: 'proportion',
      pValue: 0.0123,
      diff: 0.04,
      relativeDiff: 0.2,
      relativeMeaningful: true,
      ciLow: 0.01,
      ciHigh: 0.07,
      significant: true,
    },
    guardrails: [{ Metric: 'latency_p95', Detail: 'p95 +2.1% < 5% threshold', breached: false }],
    decision: { verdict: 'ship', reasons: ['primary significant', 'guardrails clear'] },
    minRuntimeSatisfied: true,
    sequential: { pValue: 0.00001234, significant: true, csLow: 0.011, csHigh: 0.069, tau: 0.5, logLambda: 3.21 },
    bayesian: {
      proportion: {
        probBSuperior: 0.991,
        expectedLossB: 0.00012,
        expectedLossA: 0.0042,
        credibleLowDiff: 0.011,
        credibleHighDiff: 0.068,
        postAA: 101,
        postAB: 401,
        postBA: 121,
        postBB: 372,
      },
    },
    journeys: [
      {
        metric: 'checkout_funnel',
        type: 'funnel',
        watermark: '2026-01-05T11:59:00Z',
        funnel: { steps: ['view', 'pay'] },
        arms: [
          { variant: 'control', denominator: 500, numerator: 100, immature: 3, excluded: { late: 2 } },
          { variant: 'treatment', denominator: 490, numerator: 120, immature: 1, excluded: {} },
        ],
      },
    ],
  };
}

function setup(report: ExperimentReport) {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      fetchMock(url => {
        urls.push(url.pathname + url.search);
        if (url.pathname === EXP_PATH + '/report') return { body: report };
        if (url.pathname === EXP_PATH) return { body: EXPERIMENT };
        return { status: 404, body: {} };
      }),
    ),
  );
  renderPage(<ExperimentDetailPage />, { ...ROUTE, permissions: ['report.read', 'flag.publish'] });
  return { urls };
}

// The evidence tab auto-loads the report (legacy viewExpDetail awaited
// loadReport(true) there); wait for the decision snapshot.
async function snapshot(): Promise<HTMLElement> {
  return screen.findByLabelText('Decision snapshot');
}

describe('ReportView', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('renders the healthy proportion report with every field populated', async () => {
    setup(baseReport());
    const snap = await snapshot();

    // Decision snapshot: verdict chip, healthy line, verbatim reasons.
    expect(within(snap).getByText('SHIP').className).toContain('chip green');
    expect(within(snap).getByText('Report healthy · epoch 3 · state running')).toBeTruthy();
    expect(within(snap).getByText('primary significant')).toBeTruthy();
    expect(within(snap).getByText('guardrails clear')).toBeTruthy();

    // min-runtime + analysis-method chips.
    expect(screen.getByText('min runtime satisfied').className).toContain('chip green');
    expect(screen.getByText('fixed-horizon').className).toContain('chip');

    // Window block.
    expect(screen.getByText('w2')).toBeTruthy();
    expect(screen.getByText('2026-01-01T00:00:00Z')).toBeTruthy();
    expect(screen.getByText('2026-01-08T00:00:00Z')).toBeTruthy();
    expect(screen.getByText('Complete').className).toContain('chip green');

    // SRM: χ²/p, chip, and — since R3 — the observed/expected paired bars
    // plus grid table (one row per arm), replacing the legacy JSON blob.
    expect(screen.getByText(/χ² = 1\.21 · p = 0\.271/)).toBeTruthy();
    expect(screen.getByText('no SRM').className).toContain('chip green');
    const srmFigure = screen.getByRole('img', { name: /Observed versus expected/ });
    expect(srmFigure.getAttribute('aria-label')).toContain('control: observed 502, expected 500');
    const srmRows = within(srmFigure.nextElementSibling as HTMLElement).getAllByRole('row');
    expect(within(srmRows[1]).getByText('502')).toBeTruthy();
    expect(within(srmRows[1]).getByText('50.2%')).toBeTruthy();
    expect(within(srmRows[2]).getByText('498')).toBeTruthy();

    // Per-variant table: all six columns for both arms. The h4 sits next to
    // its table inside a section that also holds the journeys tables, so
    // scope by the table element itself.
    const pv = screen.getByText('Per variant').nextElementSibling as HTMLElement;
    const rows = within(pv).getAllByRole('row');
    expect(rows).toHaveLength(3);
    expect(within(pv).getByText('control')).toBeTruthy();
    expect(within(pv).getByText('502')).toBeTruthy();
    expect(within(pv).getByText('510')).toBeTruthy();
    expect(within(pv).getByText('100')).toBeTruthy();
    expect(within(pv).getByText('0.2')).toBeTruthy();
    expect(within(pv).getByText('treatment')).toBeTruthy();
    expect(within(pv).getByText('0.24')).toBeTruthy();

    // Primary: the exact fixed-horizon line plus the significant chip.
    expect(screen.getByText(/p = 0\.0123 · diff = 0\.04 · rel = 0\.2 · CI \[0\.01, 0\.07\]/)).toBeTruthy();
    expect(screen.getByText('significant').className).toContain('chip green');

    // Journeys: compact funnel JSON + the arms table (JSON.stringify with no
    // indent, so no spaces after colons).
    expect(screen.getByText(/"steps":\[/)).toBeTruthy();
    expect(screen.getByText('checkout_funnel')).toBeTruthy();
    expect(screen.getByText(/"late":2/)).toBeTruthy();

    // Sequential: populated, and ALWAYS labelled as a separate view.
    expect(screen.getByText('(always-valid · separate view, not the decision gate)')).toBeTruthy();
    expect(screen.getByText(/anytime-valid p = 1\.234e-5/)).toBeTruthy();
    expect(screen.getByText('significant at any look').className).toContain('chip green');
    expect(screen.getByText(/CS \[0\.0110, 0\.0690\] · τ=0\.5 · logΛ=3\.210/)).toBeTruthy();

    // Bayesian: populated, labelled separate from the frequentist gate.
    expect(screen.getByText('(posterior · separate from frequentist)')).toBeTruthy();
    expect(screen.getByText(/P\(B>A\) = 0\.9910/)).toBeTruthy();
    expect(screen.getByText('prob superior').className).toContain('chip teal');
    expect(screen.getByText(/posterior A=Beta\(101\.0,401\.0\) B=Beta\(121\.0,372\.0\)/)).toBeTruthy();

    // Guardrails: dual-case Metric/Detail fields render.
    expect(screen.getByText('latency_p95')).toBeTruthy();
    expect(screen.getByText('p95 +2.1% < 5% threshold')).toBeTruthy();
  });

  it('renders the ratio-delta detail line', async () => {
    const report = baseReport();
    report.primary = {
      type: 'ratio-delta',
      pValue: 0.04,
      diff: 0.05,
      relativeDiff: 0.03,
      relativeMeaningful: true,
      ciLow: 0.001,
      ciHigh: 0.09,
      significant: true,
      ratio: { ratioA: 1.5, ratioB: 2.25, se: 0.031 },
    };
    setup(report);
    await snapshot();
    expect(screen.getByText(/ratio: A=1\.5000 · B=2\.2500 · SE=0\.0310/)).toBeTruthy();
  });

  it('renders the quantile detail line with the distribution-free note', async () => {
    const report = baseReport();
    report.primary = {
      type: 'quantile',
      pValue: 0.2,
      diff: 12,
      relativeDiff: 0.01,
      relativeMeaningful: true,
      ciLow: -3,
      ciHigh: 27,
      significant: false,
      quantile: { level: 0.95, quantileA: 812.5, quantileB: 800.125, ciALow: 790.5, ciAHigh: 830.75, distFree: true },
    };
    setup(report);
    await snapshot();
    expect(
      screen.getByText(/quantile p0\.95: A=812\.500 · B=800\.125 · A CI \[790\.500, 830\.750\] \(distribution-free\)/),
    ).toBeTruthy();
    expect(screen.getByText('not significant').className).toContain('chip');
  });

  it('renders the CUPED detail line', async () => {
    const report = baseReport();
    report.primary = {
      type: 'cuped',
      pValue: 0.02,
      diff: 0.03,
      relativeDiff: 0.15,
      relativeMeaningful: true,
      ciLow: 0.005,
      ciHigh: 0.055,
      significant: true,
      cuped: { theta: 0.9, correlation: 0.6, rawDiff: 0.02, varianceReduction: 0.42 },
      varianceReduction: 0.42,
    };
    setup(report);
    await snapshot();
    expect(screen.getByText('CUPED: variance reduction 42.0% · θ=0.9000 · ρ=0.600 · raw diff 0.0200')).toBeTruthy();
  });

  it('renders the unhealthy report: breaches, hold verdict, honest empty slots', async () => {
    const report = baseReport();
    report.healthy = false;
    report.healthIssues = ['guardrail latency_p95 breached'];
    report.decision = {
      verdict: 'hold',
      reasons: ['minimum runtime not reached'],
      guardrailBreaches: [{ metric: 'latency_p95', detail: 'p95 +8.2% > 5% threshold' }],
    };
    report.primary = null;
    report.primaryError = 'insufficient sample';
    report.sequential = null;
    report.bayesian = null;
    report.journeys = [];
    report.minRuntimeSatisfied = false;
    setup(report);
    const snap = await snapshot();

    // Verdict chip degrades to amber on an unhealthy report; the breach
    // banner lists metric + detail verbatim.
    expect(within(snap).getByText('HOLD').className).toContain('chip amber');
    expect(within(snap).getByText('Unhealthy report')).toBeTruthy();
    expect(within(snap).getByText('Guardrail breaches:')).toBeTruthy();
    expect(within(snap).getByText('latency_p95')).toBeTruthy();
    expect(within(snap).getByText(/p95 \+8\.2% > 5% threshold/)).toBeTruthy();
    expect(screen.getByText('guardrail latency_p95 breached')).toBeTruthy();
    expect(screen.getByText('minimum runtime not reached')).toBeTruthy();
    expect(screen.getByText('min runtime NOT satisfied').className).toContain('chip amber');

    // Honest empty states — never fabricated numbers.
    expect(screen.getByText('insufficient sample')).toBeTruthy();
    expect(screen.getByText('not computed (mean metrics only).')).toBeTruthy();
    expect(screen.getByText('not computed.')).toBeTruthy();
    // The separation labels stay attached even when the slots are empty.
    expect(screen.getByText('(always-valid · separate view, not the decision gate)')).toBeTruthy();
    expect(screen.getByText('(posterior · separate from frequentist)')).toBeTruthy();
  });

  it('renders the bandit report family instead of the frequentist sections', async () => {
    const report = baseReport();
    report.bandit = {
      policyVersion: 'bandit-v1',
      stateVersion: 12,
      feedbackWatermark: '2026-01-05T10:00:00Z',
      trials: [400, 600],
      successes: [80, 150],
      posteriorMean: [0.2, 0.25],
      pending: 7,
      missingFeedback: 3,
      fallbackAssignments: 2,
      srmAlarm: true,
      frozen: true,
      freezeReason: 'SRM alarm: allocation drifted',
    };
    report.exposureMode = 'onDecision';
    setup(report);
    await screen.findByText('Adaptive Bandit report');
    // The banner is "<b>title</b> · frozen ..." — the frozen label carries a
    // leading " · " inside the same div, so match it as a substring.
    expect(screen.getByText(/frozen to registered fixed split/)).toBeTruthy();
    expect(
      screen.getByText(
        'Posterior summaries are descriptive. Fixed-sample A/B p-values, confidence intervals, and winner verdicts do not apply.',
      ),
    ).toBeTruthy();
    expect(screen.getByText('bandit-v1')).toBeTruthy();
    expect(screen.getByText('12')).toBeTruthy();
    expect(screen.getByText(/Pending feedback: 7 · Mature exposures without conversion: 3 · Fixed fallback assignments: 2/)).toBeTruthy();
    expect(screen.getByText('Adaptive allocation balance alarm')).toBeTruthy();
    expect(screen.getByText('SRM alarm: allocation drifted')).toBeTruthy();
    // Posterior table rows.
    expect(screen.getByText('400')).toBeTruthy();
    expect(screen.getByText('600')).toBeTruthy();
    expect(screen.getByText('80')).toBeTruthy();
    expect(screen.getByText('150')).toBeTruthy();
    // Posterior values render twice since R3: the BanditArms bars overlay and
    // the unchanged table cells (chart + table dual channel).
    expect(screen.getAllByText('0.2000').length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText('0.2500').length).toBeGreaterThanOrEqual(2);
    // No frequentist primary section in the bandit family.
    expect(screen.queryByText('Primary metric')).toBeNull();
  });
});
