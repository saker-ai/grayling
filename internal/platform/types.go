// Package platform is the Grayling control-plane domain store: workspaces,
// environments, credentials, flag/segment drafts and immutable revisions,
// experiments, collected events with unit-level aggregates, audit trail and
// the AI-resource ownership index (docs/技术方案.md §5, §9, §10).
//
// Storage backends plug in behind the Persistence interface (see
// persistence.go): every Store method is a single transactional unit of
// work with explicit keys, no cross-store transactions are assumed (§3:
// Config 与 PostgreSQL 没有跨库原子事务). Shipped backends: in-memory with
// optional atomic JSON-file persistence (dev/single-node default), pure-Go
// SQLite and PostgreSQL (gormstore.go). The database backends persist the
// event ledger and aggregates separately from control state; see
// docs/storage.md. The earlier pgx backend remains for legacy tests.
// gonacos Config remains the distribution substrate in all topologies.
package platform

import (
	"time"

	"github.com/saker-ai/grayling/pkg/model"
)

// Roles (§9: 管理角色最小为 owner、publisher、analyst、viewer).
type Role string

const (
	RoleOwner     Role = "owner"
	RolePublisher Role = "publisher"
	RoleAnalyst   Role = "analyst"
	RoleViewer    Role = "viewer"
)

// TokenKind separates the three credential audiences (§9: 管理 API、SDK
// API、采集 API 使用不同凭证和受众).
type TokenKind string

const (
	TokenAdmin   TokenKind = "admin"
	TokenSDK     TokenKind = "sdk"
	TokenCollect TokenKind = "collect"
)

// Workspace is a top-level tenant.
type Workspace struct {
	ID        string    `json:"id"` // server-generated surrogate, never user text
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// Environment belongs to one workspace. The pair (workspace, environment)
// maps to exactly one gonacos namespace ID, generated server-side (§5.1:
// 映射由 Grayling 持久化，不把用户提供的字符串直接拼进底座 ID).
type Environment struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	NamespaceID string    `json:"namespaceId"` // Nacos namespace for this pair
	CreatedAt   time.Time `json:"createdAt"`
}

// Token is a stored credential. Only the SHA-256 hash of the secret is
// persisted; the plaintext secret is shown once at creation.
type Token struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          TokenKind `json:"kind"`
	Role          Role      `json:"role,omitempty"`          // admin tokens only
	WorkspaceID   string    `json:"workspaceId,omitempty"`   // scope; empty on global owner admin tokens
	EnvironmentID string    `json:"environmentId,omitempty"` // sdk/collect tokens only
	SecretHash    string    `json:"secretHash"`
	Prefix        string    `json:"prefix"` // display-only plaintext prefix
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt,omitempty"`
	Revoked       bool      `json:"revoked,omitempty"`
	CreatedBy     string    `json:"createdBy"`
}

// DraftState is the review workflow position of a draft (§5.3).
type DraftState string

const (
	DraftEditing   DraftState = "editing"
	DraftSubmitted DraftState = "submitted"
	DraftApproved  DraftState = "approved"
)

// Risk classifies a draft. Every change requires a distinct approver.
type Risk string

const (
	RiskNormal Risk = "normal"
	RiskHigh   Risk = "high"
)

// FlagDraft is the mutable working copy of one flag in one environment.
type FlagDraft struct {
	WorkspaceID       string      `json:"workspaceId"`
	EnvironmentID     string      `json:"environmentId"`
	Key               string      `json:"key"`
	Content           *model.Flag `json:"content"`
	State             DraftState  `json:"state"`
	Risk              Risk        `json:"risk"`
	UpdatedBy         string      `json:"updatedBy"`
	UpdatedAt         time.Time   `json:"updatedAt"`
	SubmittedBy       string      `json:"submittedBy,omitempty"`
	SubmittedTokenID  string      `json:"submittedTokenId,omitempty"`
	SubmittedAt       *time.Time  `json:"submittedAt,omitempty"`
	SubmittedHash     string      `json:"submittedHash,omitempty"`
	ApprovedBy        string      `json:"approvedBy,omitempty"`
	ApprovedTokenID   string      `json:"approvedTokenId,omitempty"`
	ApprovedAt        *time.Time  `json:"approvedAt,omitempty"`
	ApprovedHash      string      `json:"approvedHash,omitempty"`
	ApprovalExpiresAt *time.Time  `json:"approvalExpiresAt,omitempty"`
	ReviewNote        string      `json:"reviewNote,omitempty"`
	Deleted           bool        `json:"deleted,omitempty"` // pending tombstone publish
	BaseRevision      int64       `json:"baseRevision"`      // optimistic lock: revision this draft was edited against
}

// FlagRevision is one immutable published revision (§5.1).
type FlagRevision struct {
	WorkspaceID   string      `json:"workspaceId"`
	EnvironmentID string      `json:"environmentId"`
	Key           string      `json:"key"`
	Revision      int64       `json:"revision"`
	Content       *model.Flag `json:"content"`
	Hash          string      `json:"hash"`   // SHA-256 of canonical JSON
	DataID        string      `json:"dataId"` // gonacos dataId, immutable
	PublishedAt   time.Time   `json:"publishedAt"`
	PublishedBy   string      `json:"publishedBy"`
	Note          string      `json:"note,omitempty"`
	// RollbackOf records that this manifest state intentionally repoints at
	// an older revision (rollback publishes do NOT create new flag content).
	RollbackOf int64 `json:"rollbackOf,omitempty"`
}

// SegmentDraft / SegmentRevision mirror the flag lifecycle for segments.
type SegmentDraft struct {
	WorkspaceID       string         `json:"workspaceId"`
	EnvironmentID     string         `json:"environmentId"`
	Key               string         `json:"key"`
	Content           *model.Segment `json:"content"`
	State             DraftState     `json:"state"`
	Risk              Risk           `json:"risk"`
	UpdatedBy         string         `json:"updatedBy"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	SubmittedBy       string         `json:"submittedBy,omitempty"`
	SubmittedTokenID  string         `json:"submittedTokenId,omitempty"`
	SubmittedAt       *time.Time     `json:"submittedAt,omitempty"`
	SubmittedHash     string         `json:"submittedHash,omitempty"`
	ApprovedBy        string         `json:"approvedBy,omitempty"`
	ApprovedTokenID   string         `json:"approvedTokenId,omitempty"`
	ApprovedAt        *time.Time     `json:"approvedAt,omitempty"`
	ApprovedHash      string         `json:"approvedHash,omitempty"`
	ApprovalExpiresAt *time.Time     `json:"approvalExpiresAt,omitempty"`
	BaseRevision      int64          `json:"baseRevision"`
}

type SegmentRevision struct {
	WorkspaceID   string         `json:"workspaceId"`
	EnvironmentID string         `json:"environmentId"`
	Key           string         `json:"key"`
	Revision      int64          `json:"revision"`
	Content       *model.Segment `json:"content"`
	Hash          string         `json:"hash"`
	DataID        string         `json:"dataId"`
	PublishedAt   time.Time      `json:"publishedAt"`
	PublishedBy   string         `json:"publishedBy"`
}

// MetricSpec declares the primary metric of an experiment (§10.1: 明确分母、
// 零值、方向). v1 supports proportion and mean; P5 (§10.3) adds ratio, quantile
// and CUPED (covariate-adjusted mean), each pre-registered here.
type MetricSpec struct {
	Key       string `json:"key"`
	Type      string `json:"type"`      // "proportion" | "mean" | "ratio" | "quantile"
	Direction string `json:"direction"` // "increase" | "decrease" — what "better" means
	Unit      string `json:"unit,omitempty"`
	// Floor/Ceil clamp conversion values for mean metrics (nil = no clamp).
	Floor *float64 `json:"floor,omitempty"`
	Ceil  *float64 `json:"ceil,omitempty"`

	// Ratio metrics (§10.1: ratio 需每单元分子/分母及交叉乘积). NumeratorKey
	// and DenominatorKey name the two collected metrics whose per-unit totals
	// form the ratio R = Σ numerator / Σ denominator. When NumeratorKey is
	// empty, Key is used as the numerator.
	NumeratorKey   string `json:"numeratorKey,omitempty"`
	DenominatorKey string `json:"denominatorKey,omitempty"`

	// Quantile metrics (§10.1: p95 推断需保留单元明细). QuantileLevel is the
	// percentile p ∈ (0,1) taken over per-unit values (e.g. 0.95 for p95).
	QuantileLevel float64 `json:"quantileLevel,omitempty"`

	// CovariateKey names the PRE-EXPERIMENT covariate metric for CUPED
	// (§10.1: CUPED 需实验前协变量和实验后结果的同单元配对数据). When set on a
	// mean metric and paired data exists, the report uses the CUPED-adjusted
	// (variance-reduced) test as the primary result.
	CovariateKey string         `json:"covariateKey,omitempty"`
	Funnel       *FunnelSpec    `json:"funnel,omitempty"`
	Retention    *RetentionSpec `json:"retention,omitempty"`
}

// FunnelSpec names ordered conversion metric keys. The first matching event
// starts the timeout. Session dedup requires an opaque session ID on events.
type FunnelSpec struct {
	Steps       []string      `json:"steps"`
	DedupUnit   string        `json:"dedupUnit"` // user|session
	StepTimeout time.Duration `json:"stepTimeout"`
}

// RetentionSpec uses the first split exposure (or first CohortKey conversion)
// as cohort start. Day is a local calendar day, not a 24-hour duration.
type RetentionSpec struct {
	CohortKey string `json:"cohortKey"` // empty means first split exposure
	ReturnKey string `json:"returnKey"`
	Day       int    `json:"day"` // 1 or 7
	Timezone  string `json:"timezone"`
}

// GuardrailSpec is an operational protection rule, evaluated separately from
// statistical significance of the primary metric (§5.3, §10.3).
type GuardrailSpec struct {
	MetricRef *MetricRef `json:"metricRef,omitempty"`
	MetricKey string     `json:"metricKey"`
	Type      string     `json:"type"` // "proportion" | "mean"
	// Direction declares what movement is GOOD for this metric:
	// "increase" (default) or "decrease" (cost/latency style). A breach is
	// movement against the good direction beyond MaxRelativeRegression.
	Direction string `json:"direction,omitempty"`
	// MaxRelativeRegression: breach when the treatment arm regresses against
	// control by more than this fraction (0.05 = 5%).
	MaxRelativeRegression float64        `json:"maxRelativeRegression"`
	MinSample             int64          `json:"minSample"` // below this, no breach — insufficient data
	Funnel                *FunnelSpec    `json:"funnel,omitempty"`
	Retention             *RetentionSpec `json:"retention,omitempty"`
}

// HolmHypothesis is one confirmatory fixed-sample comparison. Every member of
// a family is registered before exposure, including its metric and arm pair.
type HolmHypothesis struct {
	ID        string     `json:"id"`
	Metric    MetricSpec `json:"metric"`
	Control   string     `json:"control"`
	Treatment string     `json:"treatment"`
}

// HolmFamily is the single actionable family for an experiment epoch. All
// confirmatory metrics and variant comparisons share its experiment alpha.
// Guardrails are operational rules outside this family; additional observed
// comparisons are exploratory and cannot enter the family after registration.
type HolmFamily struct {
	ID         string           `json:"id"`
	Hypotheses []HolmHypothesis `json:"hypotheses"`
}

// Experiment is the pre-registered analysis contract (§5.3). Changing the
// randomization unit, metric definitions, proportions or resource bindings
// creates a NEW epoch; history is never overwritten (§6).
type Experiment struct {
	ID            string             `json:"id"`
	WorkspaceID   string             `json:"workspaceId"`
	EnvironmentID string             `json:"environmentId"`
	FlagKey       string             `json:"flagKey"`
	FlagRevision  int64              `json:"flagRevision"` // revision the experiment was registered against
	Epoch         int64              `json:"epoch"`
	Unit          string             `json:"unit"` // randomization-unit context field
	Layer         string             `json:"layer,omitempty"`
	LayerSlot     *model.BucketRange `json:"layerSlot,omitempty"`
	Eligibility   float64            `json:"eligibility"` // percent, 0 = 100
	Variants      []string           `json:"variants"`    // explicit stable order
	Proportions   map[string]float64 `json:"proportions"`
	// AllocationMode is fixed-split (the legacy default) or bandit.
	AllocationMode   string          `json:"allocationMode,omitempty"`
	Bandit           *BanditSpec     `json:"bandit,omitempty"`
	ExposureMode     string          `json:"exposureMode,omitempty"` // onDecision | onRender
	ExposureTrigger  string          `json:"exposureTrigger,omitempty"`
	IntegrationOwner string          `json:"integrationOwner,omitempty"`
	PrimaryMetricRef *MetricRef      `json:"primaryMetricRef,omitempty"`
	PrimaryMetric    MetricSpec      `json:"primaryMetric"`
	Guardrails       []GuardrailSpec `json:"guardrails,omitempty"`
	HolmFamily       *HolmFamily     `json:"holmFamily,omitempty"`
	// RegistrationVersion fingerprints the immutable hypothesis contract.
	RegistrationVersion string        `json:"registrationVersion,omitempty"`
	Alpha               float64       `json:"alpha"`
	Power               float64       `json:"power"`
	MDE                 float64       `json:"mde"`
	MinSamplePerArm     int64         `json:"minSamplePerArm"`
	MinRuntime          time.Duration `json:"minRuntime"`
	AnalysisWindow      time.Duration `json:"analysisWindow"`
	Hypothesis          string        `json:"hypothesis"`
	AnalysisMethod      string        `json:"analysisMethod"` // pre-registered, v1: "welch" | "two-proportion-z"
	// ReleasePlan is the optional pre-registered staged-rollout plan driven
	// by the auto-advance engine (§5.3 计划步骤/观察窗, P4).
	ReleasePlan *ReleasePlan          `json:"releasePlan,omitempty"`
	State       model.ExperimentState `json:"state"`
	StartedAt   *time.Time            `json:"startedAt,omitempty"`
	StoppedAt   *time.Time            `json:"stoppedAt,omitempty"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	CreatedBy   string                `json:"createdBy"`
}

// BanditSpec is frozen at registration for one epoch. FeedbackDelay keeps
// recent exposures out of the reward denominator while outcomes arrive.
type BanditSpec struct {
	PolicyVersion string        `json:"policyVersion"`
	Strategy      string        `json:"strategy"`
	PriorAlpha    float64       `json:"priorAlpha"`
	PriorBeta     float64       `json:"priorBeta"`
	Epsilon       float64       `json:"epsilon,omitempty"`
	FeedbackDelay time.Duration `json:"feedbackDelay"`
}

// ReleaseStep is one planned step of a staged rollout (§5.3 发布记录: 计划
// 步骤、观察窗). Steps ramp the experiment ELIGIBILITY only — proportions,
// unit, salt and bindings are fixed by the pre-registered epoch (§6: 改比例
// 须新 epoch). Eligibility hashing is a prefix interval, so raising the ratio
// never un-enrolls an already-enrolled unit.
type ReleaseStep struct {
	Name string `json:"name"`
	// Ratio is the experiment eligibility percentage in (0,100] applied
	// while this step is current. Must strictly increase across steps.
	Ratio float64 `json:"ratio"`
	// MinRuntime is the minimum dwell time on this step before the decision
	// gate may fire (§5.3: 自动推进只在观测窗口、数据完整性、最小样本及全部
	// 护栏均满足时执行).
	MinRuntime time.Duration `json:"minRuntime"`
	// ObservationWindow is additional soak time after MinRuntime during
	// which data keeps accumulating before the gate is consulted (观察窗).
	ObservationWindow time.Duration `json:"observationWindow,omitempty"`
	// AutoAdvance permits the engine to enter the next step without human
	// confirmation when the gate passes. false = the engine holds at the
	// step boundary until an operator advances manually.
	AutoAdvance bool `json:"autoAdvance"`
}

// ReleasePlanState is the engine-visible lifecycle of a release plan.
type ReleasePlanState string

const (
	// ReleasePlanArmed: registered, waiting for the experiment to run.
	ReleasePlanArmed ReleasePlanState = "armed"
	// ReleasePlanRunning: the engine is applying/observing steps.
	ReleasePlanRunning ReleasePlanState = "running"
	// ReleasePlanPaused: held — by the engine (unhealthy data, SRM) or by
	// an operator. No automatic action until resumed.
	ReleasePlanPaused ReleasePlanState = "paused"
	// ReleasePlanCompleted: the final step passed its gate (ship verdict or
	// manual completion). The flag stays at the final step's ratio; full
	// rollout is a human publish decision (no automatic victory claim).
	ReleasePlanCompleted ReleasePlanState = "completed"
	// ReleasePlanRolledBack: the decision gate fired rollback (guardrail
	// breach or significantly worse primary); enrollment was stopped.
	ReleasePlanRolledBack ReleasePlanState = "rolled_back"
)

// PlanEvent is one entry of the plan's action history — the engine and
// every manual operation append here (bounded, newest last) so operators can
// reconstruct exactly what automation did and why (§9: 审计).
type PlanEvent struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"` // "rollout-engine" or the admin token name
	Action string    `json:"action"`
	Step   int       `json:"step"`
	Detail string    `json:"detail,omitempty"`
}

// ReleasePlan is the pre-registered staged-rollout contract of one
// experiment (§5.3, P4 自动推进). It lives inside the Experiment record so
// both persistence backends carry it transactionally; history is capped at
// PlanHistoryCap entries.
type ReleasePlan struct {
	Steps       []ReleaseStep    `json:"steps"`
	State       ReleasePlanState `json:"state"`
	CurrentStep int              `json:"currentStep"`
	// StepStartedAt stamps when the current step began (engine clock).
	StepStartedAt *time.Time `json:"stepStartedAt,omitempty"`
	// ManualOverride is the 人工接管开关 (§9: 人工可暂停开关): while true the
	// engine takes NO automatic action on this plan; operators drive it via
	// the manual endpoints.
	ManualOverride bool        `json:"manualOverride,omitempty"`
	PausedReason   string      `json:"pausedReason,omitempty"`
	CreatedAt      time.Time   `json:"createdAt"`
	UpdatedAt      time.Time   `json:"updatedAt"`
	CreatedBy      string      `json:"createdBy"`
	History        []PlanEvent `json:"history,omitempty"`
}

// PlanHistoryCap bounds the in-record action history (audit trail lives in
// the immutable audit log; this is the operator-facing recent window).
const PlanHistoryCap = 64

// AppendHistory adds one event and trims to the cap.
func (p *ReleasePlan) AppendHistory(ev PlanEvent) {
	p.History = append(p.History, ev)
	if len(p.History) > PlanHistoryCap {
		p.History = p.History[len(p.History)-PlanHistoryCap:]
	}
}

// EventType distinguishes exposures from conversions (§10.1).
type EventType string

const (
	EventAssignment EventType = "assignment"
	EventExposure   EventType = "exposure"
	EventConversion EventType = "conversion"
)

// Event is one deduplicated ingestion record. Unit is stored only as the
// tenant-scoped HMAC pseudonym (§10.2), never in clear.
type Event struct {
	EventID               string    `json:"eventId"`
	WorkspaceID           string    `json:"workspaceId"`
	EnvironmentID         string    `json:"environmentId"`
	Type                  EventType `json:"type"`
	ExperimentID          string    `json:"experimentId"`
	Epoch                 int64     `json:"epoch"`
	FlagKey               string    `json:"flagKey,omitempty"`
	FlagRevision          int64     `json:"flagRevision,omitempty"`
	Variant               string    `json:"variant"`
	UnitHash              string    `json:"unitHash"`
	SessionID             string    `json:"sessionId,omitempty"`
	TaskID                string    `json:"taskId,omitempty"`
	Source                string    `json:"source,omitempty"` // split|targeting|default (exposures)
	PolicyVersion         string    `json:"policyVersion,omitempty"`
	StateVersion          int64     `json:"stateVersion,omitempty"`
	AllocationProbability float64   `json:"allocationProbability,omitempty"`
	MetricKey             string    `json:"metricKey,omitempty"`
	Value                 float64   `json:"value,omitempty"`
	ClientTime            time.Time `json:"clientTime"`
	ServerTime            time.Time `json:"serverTime"`
}

// UnitAgg is the per-unit sufficient statistic for one
// (experiment, epoch, variant, unit, metric) tuple (§10.2). Keeping unit
// granularity preserves what mean/ratio metrics and per-unit aggregation
// rules require (§10.1: 主分析单位是随机化单元).
type UnitAgg struct {
	Count      int64     `json:"count"`
	Sum        float64   `json:"sum"`
	SumSquares float64   `json:"sumSquares"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
}

// AuditEntry is one immutable audit record (§9, §11).
type AuditEntry struct {
	ID            int64     `json:"id"`
	Time          time.Time `json:"time"`
	Actor         string    `json:"actor"` // token ID/name — who acted
	ActorRole     Role      `json:"actorRole,omitempty"`
	Action        string    `json:"action"` // e.g. flag.publish, experiment.start
	WorkspaceID   string    `json:"workspaceId,omitempty"`
	EnvironmentID string    `json:"environmentId,omitempty"`
	Object        string    `json:"object,omitempty"`
	Detail        string    `json:"detail,omitempty"` // JSON, may be redacted
}

// AIResourceRef is one entry of the ownership index mapping logical,
// tenant-scoped names onto server-generated physical gonacos AI IDs
// (§5.1/§7: gonacos AI 资源以全局 ID 索引，没有 namespace 字段).
type AIResourceRef struct {
	WorkspaceID   string    `json:"workspaceId"`
	EnvironmentID string    `json:"environmentId"`
	Type          string    `json:"type"` // prompt|skill|agentspec|mcp|pipeline
	LogicalID     string    `json:"logicalId"`
	PhysicalID    string    `json:"physicalId"` // generated, globally unique
	Owner         string    `json:"owner"`
	CreatedAt     time.Time `json:"createdAt"`
}

// PinnedVersion records an immutable resource version bound at publish time
// (§7: label 解析为明确的 version 与 digest，快照中固定该版本).
type PinnedVersion struct {
	WorkspaceID   string    `json:"workspaceId"`
	EnvironmentID string    `json:"environmentId"`
	Type          string    `json:"type"`
	LogicalID     string    `json:"logicalId"`
	PhysicalID    string    `json:"physicalId"`
	Version       string    `json:"version"`
	Digest        string    `json:"digest"` // MD5 hex of the pinned content
	PinnedAt      time.Time `json:"pinnedAt"`
	PinnedBy      string    `json:"pinnedBy"`
}

// ModelAllowEntry is one row of the provider/model allowlist (§7: modelRef
// 是 Grayling 适配器维护的白名单).
type ModelAllowEntry struct {
	WorkspaceID string            `json:"workspaceId"`
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Params      map[string]string `json:"params,omitempty"` // constrained default params
	Enabled     bool              `json:"enabled"`
	CreatedAt   time.Time         `json:"createdAt"`
	CreatedBy   string            `json:"createdBy"`
}

// Quotas bounds per-workspace resource usage (§11: 资源限额按工作空间配置).
type Quotas struct {
	MaxFlags          int           `json:"maxFlags"`
	MaxFlagBytes      int           `json:"maxFlagBytes"`
	MaxSegments       int           `json:"maxSegments"`
	MaxExperiments    int           `json:"maxExperiments"`
	MaxEventBytes     int           `json:"maxEventBytes"`
	MaxEventsPerBatch int           `json:"maxEventsPerBatch"`
	EventRetention    time.Duration `json:"eventRetention"`
}

// DefaultQuotas are conservative dev defaults; production values must come
// from load testing (§11: 默认值由压测给出).
func DefaultQuotas() Quotas {
	return Quotas{
		MaxFlags:          500,
		MaxFlagBytes:      64 * 1024,
		MaxSegments:       200,
		MaxExperiments:    100,
		MaxEventBytes:     16 * 1024,
		MaxEventsPerBatch: 500,
		EventRetention:    30 * 24 * time.Hour,
	}
}
