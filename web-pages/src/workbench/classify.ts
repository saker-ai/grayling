// classify.ts — the pure decision logic of the experiment workbench, ported
// function-for-function from web/legacy/exp_workbench.js: the six-state hero
// classifier, the diagnostics totals, the ETA/next-gate derivations, the
// release-plan chip maps and the release-step validator. No React here so the
// vitest contract suites can drive these directly.
import type {
  BindingReadiness,
  ExperimentDetail,
  ExperimentDiagnostics,
  ExperimentReport,
  ReleasePlan,
  ReleaseStep,
  Translate,
} from '@saker/grayling-client';

// Legacy state groups (exp_workbench.js EXP_PRELAUNCH_STATES/EXP_ENDED_STATES).
export const EXP_PRELAUNCH_STATES = ['draft', 'reviewed', 'scheduled'];
export const EXP_ENDED_STATES = ['paused', 'stopped', 'archived'];

// NS_HOUR/NS_DAY are the legacy duration constants (app.js 2038): the wizard
// and the rollout editor enter hours/days and submit nanoseconds.
export const NS_HOUR = 3.6e12;
export const NS_DAY = 8.64e13;

// PLAN_CHIP / PLAN_ACTION_CHIP / PLAN_BLOCK_RE are the legacy rollout maps.
export const PLAN_CHIP: Record<string, string> = {
  armed: 'gray',
  running: 'green',
  paused: 'amber',
  completed: 'blue',
  rolled_back: 'red',
};

export const PLAN_ACTION_CHIP: Record<string, string> = {
  'plan.start': 'green',
  'step.advance': 'green',
  'manual.advance': 'green',
  'plan.complete': 'blue',
  'manual.complete': 'blue',
  'plan.pause': 'amber',
  'manual.pause': 'amber',
  'manual.resume': 'teal',
  'plan.rollback': 'red',
  repair_publish: 'teal',
  'manual.takeover_on': 'red',
  'manual.takeover_off': 'gray',
};

// Engine block reasons surface in pausedReason / plan.pause details — they
// render with the same alert styling the report uses for SRM/guardrail
// breaches.
export const PLAN_BLOCK_RE = /srm|guardrail|unhealthy|health|rollback|breach/i;

export interface HeroDiagTotals {
  present: boolean;
  assignments: number;
  exposures: number;
  hasAssignmentFacts: boolean;
}

// heroDiagTotals sums the REAL diagnostics flow fields (assignments and
// exposures are distinct facts; evaluations never masquerade as exposures).
export function heroDiagTotals(diag: ExperimentDiagnostics | null | undefined): HeroDiagTotals {
  const flow = diag?.perVariant || [];
  const sum = (field: 'assignments' | 'exposures') => flow.reduce((total, v) => total + Number(v[field] || 0), 0);
  const assignments = sum('assignments');
  return {
    present: !!diag && flow.length > 0,
    assignments,
    exposures: sum('exposures'),
    // onDecision histories may carry no assignment facts at all — quoting a
    // zero then would invent "no assignments" from missing data.
    hasAssignmentFacts: !!diag && (diag.exposureMode === 'onRender' || assignments > 0),
  };
}

// planNextGate returns the earliest time the decision gate may fire for the
// current step (stepStartedAt + minRuntime + observationWindow), or null when
// it does not apply (not running / no step clock). Durations arrive as ns.
export function planNextGate(plan: ReleasePlan | null | undefined): Date | null {
  if (!plan || plan.state !== 'running' || !plan.stepStartedAt) return null;
  const step = (plan.steps || [])[plan.currentStep];
  if (!step) return null;
  const ms = Date.parse(plan.stepStartedAt) + (Number(step.minRuntime || 0) + Number(step.observationWindow || 0)) / 1e6;
  return isNaN(ms) ? null : new Date(ms);
}

// immatureEta derives the earliest decidable time from real fields only: the
// running plan's next gate when a plan is running, otherwise startedAt +
// minRuntime. Null when neither is recorded — callers show the etaUnknown
// copy, never an estimate.
export function immatureEta(e: ExperimentDetail | null | undefined): Date | null {
  const gate = planNextGate(e?.releasePlan);
  if (gate) return gate;
  if (e?.startedAt) {
    const ms = Date.parse(e.startedAt) + Number(e.minRuntime || 0) / 1e6;
    if (!isNaN(ms)) return new Date(ms);
  }
  return null;
}

// endedRollbackTarget reports where a rolled-back release plan stopped, from
// the plan's real current step — null when there is no rolled-back plan.
export function endedRollbackTarget(e: ExperimentDetail | null | undefined): string | null {
  const plan = e?.releasePlan;
  if (!plan || plan.state !== 'rolled_back') return null;
  const step = (plan.steps || [])[plan.currentStep];
  return step ? step.name + ' · ' + step.ratio + '%' : null;
}

// LatestTransition is the parsed newest experiment.transition audit entry.
export interface LatestTransition {
  actor?: string;
  time?: string;
  reason?: string;
  to?: string;
}

export type HeroKind =
  | 'error'
  | 'not_started'
  | 'restricted'
  | 'ended'
  | 'loading'
  | 'no_samples'
  | 'immature'
  | 'blocked'
  | 'can_advance'
  | 'unknown';

export interface HeroAction {
  // goto is one of the zone tabs or a special target: 'audit' (the audit
  // view) or 'retry-diagnostics' (re-fetch the hero diagnostics).
  goto: string;
  label: string;
}

export interface HeroClassification {
  kind: HeroKind;
  params: {
    stale?: boolean;
    state?: string;
    transition?: LatestTransition | null;
    rollbackTarget?: string | null;
    subBranch?: 'assigned' | 'none' | 'unknown';
    assigned?: number;
    diagFailed?: boolean;
    eta?: Date | null;
    reasons?: string[];
    breaches?: Array<{ metric?: string; detail?: string }>;
    scope?: { flag: string; env: string; epoch: number | string };
    primary?: ExperimentReport['primary'] | null;
    guardrails?: ExperimentReport['guardrails'];
    verdict?: string;
  };
  actions: HeroAction[];
}

export interface ClassifyMeta {
  error?: unknown;
  canRead?: boolean;
  diagFailed?: boolean;
}

// classifyExpStatus is the legacy six-state first-screen classifier, a pure
// function over REAL server data only. Priority: report-fetch error >
// prelaunch states > missing report.read > ended states > loading >
// no_samples > immature > blocked > can_advance; anything else renders no
// banner (kind 'unknown') rather than inventing a state. The global can()/t()
// of the legacy arrive as parameters (meta.canRead, canPublish, t).
export function classifyExpStatus(
  e: ExperimentDetail | null | undefined,
  report: ExperimentReport | null | undefined,
  diagnostics: ExperimentDiagnostics | null | undefined,
  latestTransition: LatestTransition | null | undefined,
  meta: ClassifyMeta,
  canPublish: boolean,
  t: Translate,
): HeroClassification {
  const state = e?.state || '';
  const canRead = meta.canRead !== undefined ? meta.canRead : true;
  const act = (goto: string, label: string): HeroAction => ({ goto, label });
  if (meta.error) return { kind: 'error', params: { stale: !!report }, actions: [] };
  if (EXP_PRELAUNCH_STATES.includes(state)) {
    return {
      kind: 'not_started',
      params: { state },
      actions: [act('settings', t('exp.hero.viewReadiness'))],
    };
  }
  if (!canRead) return { kind: 'restricted', params: {}, actions: [] };
  if (EXP_ENDED_STATES.includes(state)) {
    return {
      kind: 'ended',
      params: { state, transition: latestTransition || null, rollbackTarget: endedRollbackTarget(e) },
      actions: [act('audit', t('exp.hero.viewAudit')), act('settings', t('exp.tab.settings'))],
    };
  }
  if (!report) return { kind: 'loading', params: {}, actions: [] };
  const diag = diagnostics || report.diagnostics || null;
  const issues = report.healthIssues || [];
  const verdict = report.decision?.verdict || '';
  const reasons = report.decision?.reasons || [];
  const breaches = report.decision?.guardrailBreaches || [];
  const totals = heroDiagTotals(diag);
  // no_samples — the server issue string verbatim, or a real diagnostics
  // payload whose exposure total is zero.
  if (issues.includes('no randomized exposures recorded yet') || (totals.present && totals.exposures === 0)) {
    let subBranch: 'assigned' | 'none' | 'unknown' = 'unknown';
    if (totals.hasAssignmentFacts) subBranch = totals.assignments > 0 ? 'assigned' : 'none';
    const actions = [act('health', t('exp.hero.viewCollection'))];
    if (meta.diagFailed) actions.push(act('retry-diagnostics', t('btn.refresh')));
    return {
      kind: 'no_samples',
      params: { subBranch, assigned: totals.assignments, diagFailed: !!meta.diagFailed },
      actions,
    };
  }
  // immature — minimum runtime not satisfied, an insufficient-sample hold, or
  // a healthy "keep running" verdict.
  if (
    report.minRuntimeSatisfied === false ||
    reasons.some(r => String(r).includes('minimum runtime') || String(r).includes('insufficient sample')) ||
    (report.healthy && verdict === 'continue')
  ) {
    return {
      kind: 'immature',
      params: { eta: immatureEta(e), reasons },
      actions: [act('health', t('exp.hero.viewWatermark'))],
    };
  }
  // blocked — unhealthy pipeline / SRM / contamination / guardrail breach, or
  // a rollback verdict (which also covers a significantly worse primary).
  if (!report.healthy || verdict === 'rollback') {
    return {
      kind: 'blocked',
      params: {
        reasons: [...issues, ...reasons],
        breaches,
        scope: {
          flag: e?.flagKey || '—',
          env: e?.environmentId || '—',
          epoch: e?.epoch !== undefined && e?.epoch !== null ? e.epoch : '—',
        },
      },
      actions: [
        act('health', t('exp.hero.viewDiagnostics')),
        act('rollout', t('exp.hero.blockedRollout')),
        act('audit', t('exp.hero.blockedChanges')),
      ],
    };
  }
  // can_advance — healthy ship verdict with the minimum runtime satisfied.
  if (report.healthy && verdict === 'ship' && report.minRuntimeSatisfied) {
    const actions = [act('evidence', t('exp.hero.viewEvidence'))];
    if (canPublish) actions.push(act('rollout', t('exp.zone.rollout')));
    return {
      kind: 'can_advance',
      params: { primary: report.primary || null, guardrails: report.guardrails || [], verdict },
      actions,
    };
  }
  // Defensive: no rule matched — render no banner rather than invent a state;
  // the decision snapshot shows the raw server verdict.
  return { kind: 'unknown', params: { verdict }, actions: [] };
}

// bindCheckNames maps the server's stable check codes to their localized
// labels (legacy bindCheckNames). Unknown codes pass through raw.
export function bindCheckNames(t: Translate): Record<string, string> {
  return {
    state: t('exp.bindCheckState'),
    published_flag: t('exp.bindCheckPublished'),
    draft_clear: t('exp.bindCheckDraft'),
    registered_revision: t('exp.bindCheckRevision'),
    fallback: t('exp.bindCheckFallback'),
    unit: t('exp.bindCheckUnit'),
    split: t('exp.bindCheckSplit'),
    bandit_rule: t('exp.bindCheckBandit'),
    existing_binding: t('exp.bindCheckExisting'),
  };
}

// bindChangedChecks is the legacy renderBindConflict diff: the checks that
// were ready in the previous readiness snapshot and fail in the latest.
export function bindChangedChecks(
  previous: BindingReadiness | null | undefined,
  latest: BindingReadiness | null | undefined,
): Array<{ code: string }> {
  return (latest?.checks || []).filter(
    check => !check.ready && (previous?.checks || []).some(before => before.code === check.code && before.ready),
  );
}

// validateReleaseSteps is the legacy client-side plan validator: 1..32 steps,
// every step named with a finite ratio in (0,100], unique names, strictly
// increasing ratios, minRuntime in (0, int64max], observationWindow in
// [0, int64max]. Returns '' when valid, else the localized issue.
export function validateReleaseSteps(steps: ReleaseStep[], t: Translate): string {
  if (!steps.length || steps.length > 32) return t('ro.validationCount');
  const names = new Set<string>();
  for (let index = 0; index < steps.length; index++) {
    const step = steps[index];
    if (!step.name || !Number.isFinite(step.ratio) || step.ratio <= 0 || step.ratio > 100) return t('toast.stepFields');
    if (names.has(step.name)) return t('ro.validationNames');
    names.add(step.name);
    if (index && step.ratio <= steps[index - 1].ratio) return t('toast.ratioIncrease', { i: index + 1 });
    if (!Number.isFinite(step.minRuntime) || step.minRuntime <= 0 || step.minRuntime > 9223372036854775807)
      return t('toast.minRuntimePositive');
    if (
      !Number.isFinite(step.observationWindow || 0) ||
      (step.observationWindow || 0) < 0 ||
      (step.observationWindow || 0) > 9223372036854775807
    )
      return t('ro.validationObservation');
  }
  return '';
}
