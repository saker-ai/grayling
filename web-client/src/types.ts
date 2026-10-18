// Shared wire types for the grayling admin API. Phase 0 covers only what the
// shell needs: session, workspaces/environments and capabilities. View-level
// types (flags, segments, experiments, reports, …) land with their views.

export interface SessionInfo {
  name: string;
  principalId: string;
}

export interface Workspace {
  slug: string;
  name?: string;
}

export interface Environment {
  slug: string;
  name?: string;
}

// ScopedCapabilities is the per-workspace shape: permissions live under a
// `permissions` map. GlobalCapabilities (the /api/v1/admin/capabilities
// response) is a flat permission → boolean map; `workspace.admin` is read
// from the global map, every other permission from the scoped one.
export interface ScopedCapabilities {
  permissions: Record<string, boolean>;
}

export type GlobalCapabilities = Record<string, boolean>;

// ---------------------------------------------------------------------------
// Phase 1 view types (debug, quotas, tokens, audit, segments, experiments).
// ---------------------------------------------------------------------------

// WorkspaceQuotas is the GET/PUT body of {ws}/quotas. Retention is in
// nanoseconds; every field is a positive safe integer.
export interface WorkspaceQuotas {
  maxFlags: number;
  maxFlagBytes: number;
  maxSegments: number;
  maxExperiments: number;
  maxEventBytes: number;
  maxEventsPerBatch: number;
  eventRetention: number;
}

export interface TokenInfo {
  id: string;
  name: string;
  kind: string;
  role?: string;
  prefix?: string;
  environmentId?: string;
  createdAt?: string;
  revoked?: boolean;
}

// CreatedToken is the POST {ws}/tokens response: the `Secret` field (capital
// S, matching the wire format) is returned exactly once at creation time.
export interface CreatedToken extends TokenInfo {
  Secret?: string;
}

export interface AuditEntry {
  id: string;
  time?: string;
  actor?: string;
  action?: string;
  object?: string;
  environmentId?: string;
  detail?: unknown;
}

// SegmentDraft is one entry of GET {scope}/segments: a draft document with
// workflow state, plus optional published content.
export interface SegmentDraft {
  key: string;
  state?: string;
  risk?: string;
  updatedBy?: string;
  updatedAt?: string;
  draft?: { content?: Record<string, unknown> };
  current?: { content?: Record<string, unknown> };
  content?: Record<string, unknown>;
}

export interface DebugEvalResult {
  value?: unknown;
  variationType?: string;
  reason?: string;
  failed?: boolean;
  errorCode?: string;
  bucket?: number;
}

export interface DebugEvalResponse {
  result?: DebugEvalResult;
  manifestRevision?: number;
  note?: string;
}

export interface ExperimentReportSummary {
  status?: string;
  healthy?: boolean;
  verdict?: string;
  issue?: string;
}

export interface ExperimentSummaryItem {
  experiment: {
    id: string;
    flagKey?: string;
    state?: string;
    createdBy?: string;
    updatedAt?: string;
  };
  report?: ExperimentReportSummary | null;
}

// ExperimentSummaryPage is the GET {scope}/experiment-summaries response:
// server-side offset pagination over the filtered list.
export interface ExperimentSummaryPage {
  items?: ExperimentSummaryItem[];
  total: number;
  offset: number;
  limit: number;
}

// ---------------------------------------------------------------------------
// Phase 2 view types (overview, approvals, metrics, ai, flag/segment detail).
// ---------------------------------------------------------------------------

// FlagRevision is one entry of the flag history table.
export interface FlagRevision {
  revision: number;
  hash?: string;
  publishedAt?: string;
  publishedBy?: string;
  note?: string;
  rollbackOf?: number;
  content?: Record<string, unknown>;
}

// FlagDraft is the workflow draft half of a flag detail: content plus the
// review state (editing → submitted → approved), risk and deletion marker.
export interface FlagDraft {
  state?: string;
  risk?: string;
  deleted?: boolean;
  updatedBy?: string;
  updatedAt?: string;
  content?: Record<string, unknown>;
}

// ExperimentImpact lists the experiments referencing a flag (or attached to a
// review preview), as embedded in the flag detail / preview responses.
export interface ExperimentImpact {
  total?: number;
  references?: Array<{ id: string; state?: string; epoch?: number; flagRevision?: number }>;
}

// ReviewPreview is the server-computed publish impact summary (flag and
// segment publish both embed one; approval dialogs reuse it).
export interface ReviewPreview {
  targetEnvironment?: string;
  risk?: string;
  baseRevision?: number;
  changedFields?: string[];
  estimatedAffected?: string;
  rollbackTarget?: string;
  guardrails?: string;
  proposedHash?: string;
  approvalExpiresAt?: string;
  experimentImpact?: ExperimentImpact | null;
}

// FlagDetail is the GET {scope}/flags/{key} response.
export interface FlagDetail {
  key: string;
  draft?: FlagDraft | null;
  current?: {
    revision?: number;
    hash?: string;
    publishedAt?: string;
    publishedBy?: string;
    content?: Record<string, unknown>;
  } | null;
  currentRevision?: number;
  draftHash?: string;
  revisions?: FlagRevision[];
  preview?: ReviewPreview | null;
  experimentImpact?: ExperimentImpact | null;
}

// ApprovalItem is one entry of GET {ws}/approvals.
export interface ApprovalItem {
  id: string;
  action: string;
  object: string;
  state: string;
  environmentId?: string;
  submittedBy?: string;
  submittedAt?: string;
  risk?: string;
  impact?: string;
  reason?: string;
  baseRevision?: number;
  rollbackTarget?: string;
  guardrails?: string;
  payloadHash?: string;
  payload?: string;
  expiresAt?: string;
  approvedBy?: string;
  rejectedBy?: string;
  rejectionReason?: string;
}

// ApprovalComparison is the GET {ws}/approvals/{id}/comparison response: the
// before/after field map the reviewer approves against. `stale` means the
// object changed since the request was filed; `reason` explains an
// unavailable comparison ('content_not_retained' or unsupported).
export interface ApprovalComparison {
  available: boolean;
  stale?: boolean;
  reason?: string;
  before?: Record<string, unknown>;
  after?: Record<string, unknown>;
  beforeRevision?: number;
  afterRevision?: number;
}

// MetricDefinition is one metric catalog entry (list rows and version detail
// share the shape).
export interface MetricDefinition {
  key: string;
  version: number;
  displayName: string;
  description?: string;
  owner: string;
  source?: string;
  eventKeys?: string[];
  attributionWindow?: number;
  hash?: string;
  status?: string;
  spec?: Record<string, unknown>;
}

// MetricPage is the cursor-paginated GET {ws}/metrics response.
export interface MetricPage {
  items?: MetricDefinition[];
  nextCursor?: string;
}

// AIResource is one registered resource (prompt/skill/agentspec).
export interface AIResource {
  type: string;
  logicalId: string;
  physicalId?: string;
  owner?: string;
  createdAt?: string;
}

// AIResourcesResponse is GET {scope}/ai/resources; revocations arrive as a map
// keyed "type<NUL>logicalId" → reason.
export interface AIResourcesResponse {
  resources?: AIResource[];
  revocations?: Record<string, string>;
}

export interface AIResourcePin {
  version: string;
  pinnedAt?: string;
  pinnedBy?: string;
  digest?: string;
}

// AIResourceDetail is GET {scope}/ai/resources/{type}/{logicalId}.
export interface AIResourceDetail {
  resource: AIResource;
  pins?: AIResourcePin[];
  sourceMode?: string;
  revoked?: boolean;
  revocationReason?: string;
}

// AllowlistEntry is one row of the workspace model allowlist.
export interface AllowlistEntry {
  provider: string;
  model: string;
  enabled?: boolean;
}

// OverviewResponse is GET {ws}/overview — an aggregate of independently
// degradable modules; every module carries its own status and the console
// never invents data for a failed one (null counts render as '—').
export interface OverviewCoverage {
  inspected?: number;
  total?: number;
  complete?: boolean;
}

export interface OverviewRiskItem {
  id: string;
  kind?: string;
  severity?: string;
  environmentId?: string;
  text?: string;
}

export interface OverviewModule {
  status?: string;
  reason?: string;
  coverage?: OverviewCoverage;
}

export interface OverviewRisks extends OverviewModule {
  reportCoverage?: string;
  items?: OverviewRiskItem[];
}

export interface OverviewCounts {
  pausedExperiments?: number | null;
  runningExperiments?: number | null;
  pendingApprovals?: number | null;
  publishedFlags?: number | null;
}

export interface OverviewApprovalItem {
  id: string;
  action?: string;
  object?: string;
  environmentId?: string;
  submittedBy?: string;
  submittedAt?: string;
  risk?: string;
  expiresAt?: string;
  myAction?: string;
  blockedReason?: string;
}

export interface OverviewPending extends OverviewModule {
  actionableTotal?: number;
  items?: OverviewApprovalItem[];
}

export interface OverviewReleaseItem {
  time?: string;
  action?: string;
  object?: string;
  actor?: string;
  environmentId?: string;
}

export interface OverviewReleases extends OverviewModule {
  items?: OverviewReleaseItem[];
}

export interface OverviewResponse {
  risks?: OverviewRisks;
  counts?: OverviewCounts | null;
  pendingApprovals?: OverviewPending;
  recentReleases?: OverviewReleases;
}

// SegmentDependencies is GET {scope}/segments/{key}/dependencies.
export interface SegmentDependencies {
  segmentRevision?: number;
  flags?: Array<{ key: string; revision: number; via?: string[] }>;
}

// SegmentHistory is GET {scope}/segments/{key}/history.
export interface SegmentHistory {
  currentRevision?: number;
  revisions?: Array<{
    revision: number;
    publishedAt?: string;
    publishedBy?: string;
    hash?: string;
    content?: Record<string, unknown>;
  }>;
}

// SegmentMatchResult is POST {scope}/segments/{key}/match-preview: whether the
// draft and the published revision match the probe context.
export interface SegmentMatchResult {
  draftMatched: boolean;
  publishedMatched?: boolean | null;
  publishedRevision?: number;
}

// ---------------------------------------------------------------------------
// Phase 3 view types (flags list, experiment detail/workbench, release plan).
// Field names mirror the server JSON exactly (internal/platform/types.go,
// internal/experiment/report.go, internal/admin/experiments.go); durations
// are time.Duration values, i.e. int64 nanoseconds on the wire.
// ---------------------------------------------------------------------------

// FlagListItem is one entry of GET {scope}/flags (platform.FlagView).
export interface FlagListItem {
  key: string;
  draft?: {
    state?: string;
    risk?: string;
    updatedBy?: string;
    updatedAt?: string;
    deleted?: boolean;
    content?: Record<string, unknown>;
  } | null;
  current?: {
    revision?: number;
    hash?: string;
    publishedAt?: string;
    publishedBy?: string;
    content?: Record<string, unknown>;
  } | null;
  currentRevision?: number;
}

// MetricRef identifies a frozen metric version (platform.MetricRef); the
// report echoes the frozen definitionHash for provenance.
export interface MetricRef {
  key: string;
  version: number;
  definitionHash?: string;
}

// ExperimentMetricSpec mirrors platform.MetricSpec.
export interface ExperimentMetricSpec {
  key: string;
  type: string;
  direction?: string;
  numeratorKey?: string;
  denominatorKey?: string;
  quantileLevel?: number;
  covariateKey?: string;
  funnel?: { steps?: string[]; timeout?: number };
  retention?: Record<string, unknown>;
}

// ExperimentGuardrail mirrors platform.GuardrailSpec.
export interface ExperimentGuardrail {
  metricRef?: MetricRef;
  metricKey: string;
  type: string;
  direction?: string;
  maxRelativeRegression: number;
  minSample: number;
  funnel?: { steps?: string[]; timeout?: number };
  retention?: Record<string, unknown>;
}

// ExperimentBandit mirrors platform.BanditSpec (feedbackDelay in ns).
export interface ExperimentBandit {
  policyVersion: string;
  strategy: string;
  priorAlpha: number;
  priorBeta: number;
  epsilon?: number;
  feedbackDelay: number;
}

// HolmHypothesis is one registered multiple-comparison hypothesis
// (platform.HolmHypothesis): id, the frozen metric reference and the
// control→treatment pair.
export interface HolmHypothesis {
  id: string;
  metric: MetricRef;
  control: string;
  treatment: string;
}

// ReleaseStep mirrors platform.ReleaseStep (durations in ns).
export interface ReleaseStep {
  name: string;
  ratio: number;
  minRuntime: number;
  observationWindow?: number;
  autoAdvance: boolean;
}

// PlanEvent mirrors platform.PlanEvent.
export interface PlanEvent {
  at: string;
  actor: string;
  action: string;
  step: number;
  detail?: string;
}

// ReleasePlan mirrors platform.ReleasePlan.
export interface ReleasePlan {
  steps: ReleaseStep[];
  state: string;
  currentStep: number;
  stepStartedAt?: string;
  manualOverride?: boolean;
  pausedReason?: string;
  createdAt?: string;
  updatedAt?: string;
  createdBy?: string;
  history?: PlanEvent[];
}

// ExperimentDetail is GET /api/v1/admin/experiments/{id}
// (platform.Experiment). Durations (minRuntime, analysisWindow) are ns.
export interface ExperimentDetail {
  id: string;
  workspaceId?: string;
  environmentId?: string;
  flagKey: string;
  flagRevision?: number;
  epoch?: number;
  unit?: string;
  layer?: string;
  layerSlot?: { min?: number; max?: number } | null;
  eligibility?: number;
  variants?: string[];
  proportions?: Record<string, number>;
  allocationMode?: string;
  bandit?: ExperimentBandit | null;
  exposureMode?: string;
  exposureTrigger?: string;
  integrationOwner?: string;
  primaryMetricRef?: MetricRef | null;
  primaryMetric?: ExperimentMetricSpec;
  guardrails?: ExperimentGuardrail[];
  holmFamily?: { id: string; hypotheses?: HolmHypothesis[] } | null;
  registrationVersion?: string;
  alpha?: number;
  power?: number;
  mde?: number;
  minSamplePerArm?: number;
  minRuntime?: number;
  analysisWindow?: number;
  hypothesis?: string;
  analysisMethod?: string;
  releasePlan?: ReleasePlan | null;
  state?: string;
  startedAt?: string;
  stoppedAt?: string;
  createdAt?: string;
  updatedAt?: string;
  createdBy?: string;
}

// BindingCheck/BindingReadiness mirror internal/admin BindingCheck and
// BindingReadiness (GET …/experiments/{id}/bind-check).
export interface BindingCheck {
  code: string;
  ready: boolean;
  detail: string;
}

export interface BindingReadiness {
  experimentId: string;
  registeredRevision: number;
  currentRevision: number;
  ready: boolean;
  checks: BindingCheck[];
}

// AnalysisWindowStatus mirrors platform.AnalysisWindowStatus.
export interface AnalysisWindowStatus {
  version?: string;
  start?: string;
  end?: string;
  watermark?: string;
  complete?: boolean;
  backend?: string;
  recomputable?: boolean;
  issues?: string[];
}

// ExperimentDiagnostics mirrors experiment.Diagnostics.
export interface ExperimentDiagnostics {
  experimentId?: string;
  epoch?: number;
  exposureMode?: string;
  generatedAt?: string;
  window?: AnalysisWindowStatus;
  perVariant?: Array<{
    variant: string;
    assignments: number;
    randomizedAssignments: number;
    exposures: number;
    randomizedExposures: number;
    unexposed: number;
    conversions: number;
    duplicateExposures: number;
  }>;
  assignmentSrm?: SRMResult | null;
  exposureSrm?: SRMResult | null;
  crossVariantUnits?: number;
  assignmentMismatch?: number;
  duplicateEvents?: number;
  rejectedByReason?: Record<string, number>;
  lastCollectedAt?: string;
  issues?: string[];
}

export interface SRMResult {
  chiSquare: number;
  pValue: number;
  healthy: boolean;
  observed?: number[];
  expected?: number[];
}

// Report building blocks mirror internal/stats and internal/experiment.
export interface ReportVariant {
  variant: string;
  exposedUnits: number;
  randomizedUnits: number;
  exposures: number;
  convertedUnits: number;
  primaryValue: number;
}

export interface ReportPrimary {
  type: string;
  pValue: number;
  diff: number;
  relativeDiff: number;
  relativeMeaningful: boolean;
  ciLow: number;
  ciHigh: number;
  significant: boolean;
  ratio?: {
    ratioA: number;
    ratioB: number;
    se: number;
    [k: string]: unknown;
  } | null;
  quantile?: {
    level: number;
    quantileA: number;
    quantileB: number;
    ciALow: number;
    ciAHigh: number;
    distFree?: boolean;
    [k: string]: unknown;
  } | null;
  cuped?: {
    theta: number;
    correlation: number;
    rawDiff: number;
    varianceReduction: number;
    [k: string]: unknown;
  } | null;
  varianceReduction?: number;
  [k: string]: unknown;
}

export interface SequentialResult {
  pValue: number;
  significant: boolean;
  csLow: number;
  csHigh: number;
  tau: number;
  logLambda: number;
  [k: string]: unknown;
}

export interface BayesianMeanResult {
  probBSuperior: number;
  expectedLossB: number;
  expectedLossA: number;
  credibleLow: number;
  credibleHigh: number;
  [k: string]: unknown;
}

export interface BayesianProportionResult {
  probBSuperior: number;
  expectedLossB: number;
  expectedLossA: number;
  credibleLowDiff: number;
  credibleHighDiff: number;
  postAA: number;
  postAB: number;
  postBA: number;
  postBB: number;
  [k: string]: unknown;
}

export interface BanditSection {
  policyVersion: string;
  stateVersion: number;
  feedbackWatermark: string;
  trials: number[];
  successes: number[];
  posteriorMean: number[];
  pending: number;
  missingFeedback: number;
  fallbackAssignments: number;
  srmAlarm: boolean;
  frozen: boolean;
  freezeReason?: string;
}

export interface HolmComparison {
  id: string;
  metric?: ExperimentMetricSpec;
  control: string;
  treatment: string;
  rawP: number;
  adjustedP: number;
  effectiveAlpha: number;
  rank: number;
  rejected: boolean;
  diff?: number;
}

export interface HolmSection {
  familyId: string;
  registrationVersion: string;
  alpha: number;
  comparisons: HolmComparison[];
  error?: string;
}

export interface JourneyArm {
  variant: string;
  denominator: number;
  numerator: number;
  immature: number;
  excluded?: Record<string, number>;
}

export interface JourneySection {
  metric: string;
  type: string;
  watermark?: string;
  arms: JourneyArm[];
  [k: string]: unknown;
}

// ExperimentReport is GET …/experiments/{id}/report (experiment.Report).
export interface ExperimentReport {
  experimentId: string;
  epoch: number;
  flagKey: string;
  state: string;
  generatedAt: string;
  evidenceVersion?: string;
  window?: AnalysisWindowStatus;
  primaryAttributionWindow?: number;
  exposureMode?: string;
  diagnostics?: ExperimentDiagnostics | null;
  analysisMethod?: string;
  alpha?: number;
  mde?: number;
  control?: string;
  treatment?: string;
  healthy: boolean;
  healthIssues?: string[];
  srm?: SRMResult | null;
  perVariant?: ReportVariant[];
  primary?: ReportPrimary | null;
  primaryError?: string;
  guardrails?: Array<{ metric?: string; Metric?: string; detail?: string; Detail?: string; breached?: boolean }>;
  decision?: { verdict?: string; reasons?: string[]; guardrailBreaches?: Array<{ metric?: string; detail?: string }> };
  minRuntimeSatisfied?: boolean;
  sequential?: SequentialResult | null;
  bayesian?: { mean?: BayesianMeanResult | null; proportion?: BayesianProportionResult | null } | null;
  bandit?: BanditSection | null;
  registrationVersion?: string;
  primaryMetricRef?: MetricRef | null;
  holm?: HolmSection | null;
  journeys?: JourneySection[];
}
