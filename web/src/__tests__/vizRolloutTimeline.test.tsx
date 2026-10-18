// vizRolloutTimeline.test.tsx pins the R3 rollout timeline
// (web-pages/src/viz/RolloutTimeline.tsx): the five node states follow the
// same logic as the steps-table chips (done/current/held/stopped/pending),
// each item carries the step name, ratio and a visually-hidden state label,
// and an empty step list renders nothing. The RolloutBlock wiring test
// asserts the timeline lands between the progress bar and the steps table,
// which stays in place.
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import type { ExperimentDetail, ReleasePlan, ReleaseStep } from '@saker/grayling-client';
import { RolloutBlock } from '@saker/grayling-pages/workbench/RolloutBlock';
import { RolloutTimeline } from '@saker/grayling-pages/viz/RolloutTimeline';
import { expectNoA11yViolations, renderPage } from './helpers';

const STEPS = [
  { name: 'canary', ratio: 5 },
  { name: 'half', ratio: 50 },
  { name: 'full', ratio: 100 },
];

function classes(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll('.ro-tl-step')).map(li => li.className);
}

describe('RolloutTimeline', () => {
  it('running plan marks the current step and leaves the rest pending', () => {
    const { container } = render(<RolloutTimeline steps={STEPS} currentStep={0} state="running" />);
    expect(classes(container)).toEqual([
      'ro-tl-step ro-tl-current',
      'ro-tl-step ro-tl-pending',
      'ro-tl-step ro-tl-pending',
    ]);
    // step name and ratio are real text under each node
    expect(screen.getByText('canary')).toBeTruthy();
    expect(screen.getByText('5%')).toBeTruthy();
    // dual channel: the state also exists as (visually hidden) text
    const items = screen.getAllByRole('listitem');
    expect(within(items[0]).getByText('current')).toBeTruthy();
    expect(within(items[1]).getByText('pending')).toBeTruthy();
  });

  it('paused plan marks earlier steps done and the current one held', () => {
    const { container } = render(<RolloutTimeline steps={STEPS} currentStep={1} state="paused" />);
    expect(classes(container)).toEqual([
      'ro-tl-step ro-tl-done',
      'ro-tl-step ro-tl-held',
      'ro-tl-step ro-tl-pending',
    ]);
  });

  it('rolled-back plan marks the current step stopped', () => {
    const { container } = render(<RolloutTimeline steps={STEPS} currentStep={1} state="rolled_back" />);
    expect(classes(container)).toEqual([
      'ro-tl-step ro-tl-done',
      'ro-tl-step ro-tl-stopped',
      'ro-tl-step ro-tl-pending',
    ]);
  });

  it('completed plan marks every step done', () => {
    const { container } = render(<RolloutTimeline steps={STEPS} currentStep={3} state="completed" />);
    expect(classes(container)).toEqual([
      'ro-tl-step ro-tl-done',
      'ro-tl-step ro-tl-done',
      'ro-tl-step ro-tl-done',
    ]);
  });

  it('empty step list renders nothing', () => {
    const { container } = render(<RolloutTimeline steps={[]} currentStep={0} state="running" />);
    expect(container.querySelector('.ro-timeline')).toBeNull();
  });

  it('the list carries its own accessible name', () => {
    render(<RolloutTimeline steps={STEPS} currentStep={0} state="running" />);
    expect(screen.getByRole('list', { name: 'Rollout step timeline' })).toBeTruthy();
  });

  it('passes axe with no violations', async () => {
    const { container } = render(<RolloutTimeline steps={STEPS} currentStep={1} state="paused" />);
    await expectNoA11yViolations(container);
  });
});

describe('RolloutTimeline in RolloutBlock', () => {
  const NS_HOUR = 3600000000000;
  const PLAN_STEPS: ReleaseStep[] = [
    { name: 'canary', ratio: 5, minRuntime: 2 * NS_HOUR, observationWindow: NS_HOUR, autoAdvance: true },
    { name: 'half', ratio: 50, minRuntime: 4 * NS_HOUR, observationWindow: 2 * NS_HOUR, autoAdvance: true },
    { name: 'full', ratio: 100, minRuntime: 4 * NS_HOUR, observationWindow: 0, autoAdvance: false },
  ];
  const PLAN: ReleasePlan = {
    steps: PLAN_STEPS,
    state: 'running',
    currentStep: 0,
    stepStartedAt: '2026-01-05T00:00:00Z',
    createdBy: 'alice',
    updatedAt: '2026-01-05T00:00:00Z',
    history: [],
  };
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
    exposureMode: 'onDecision',
    releasePlan: PLAN,
  };

  it('renders between the progress bar and the steps table, both kept', () => {
    const { container } = renderPage(
      <RolloutBlock
        experiment={EXPERIMENT}
        env="prod"
        reportStatus="not_required"
        canPublish={false}
        locale="en"
        onReportStatus={() => {}}
        onChanged={() => {}}
        onOpenAudit={() => {}}
      />,
    );
    const bar = container.querySelector('#ro-bar');
    const timeline = screen.getByRole('list', { name: 'Rollout step timeline' });
    const table = container.querySelector('#ro-steps-tbl');
    expect(bar).toBeTruthy();
    expect(table).toBeTruthy();
    // bar → timeline → table in document order
    expect(bar!.compareDocumentPosition(timeline) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(timeline.compareDocumentPosition(table!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // the table still carries the status chips (the text channel)
    expect(within(table as HTMLElement).getAllByText(/^(done|current|pending)$/).length).toBeGreaterThan(0);
  });
});
