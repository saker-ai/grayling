// Package platform is the GoLab control-plane domain store: workspaces,
// environments, credentials, flag/segment drafts and immutable revisions,
// experiments, collected events with unit-level aggregates, audit trail and
// the AI-resource ownership index (docs/技术方案.md §5, §9, §10).
//
// Storage backends plug in behind the Persistence interface (see
// persistence.go): every Store method is a single transactional unit of
// work with explicit keys, no cross-store transactions are assumed (§3:
// Config 与 PostgreSQL 没有跨库原子事务). Shipped backends: in-memory with
// optional atomic JSON-file persistence (dev/single-node default) and
// PostgreSQL (pgstore.go — production path per §3/§10.2: versioned schema
// migrations, unit-level aggregate tables, monthly-partitioned raw events
// with partition-drop retention, and an HMAC key-ring rotation migration).
// gonacos Config remains the distribution substrate in all topologies.
package platform

import (
	"time"

	"github.com/godeps/golab/pkg/model"
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
// 映射由 GoLab 持久化，不把用户提供的字符串直接拼进底座 ID).
type Environment struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	NamespaceID string    `json:"namespaceId"` // gonacos namespace for this pair
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

// Risk classifies a draft. High-risk drafts cannot be approved by their own
// submitter (§9: 审批人不能批准自己的高风险变更).
type Risk string

const (
	RiskNormal Risk = "normal"
	RiskHigh   Risk = "high"
)

// FlagDraft is the mutable working copy of one flag in one environment.
type FlagDraft struct {
	WorkspaceID   string      `json:"workspaceId"`
	EnvironmentID string      `json:"environmentId"`
	Key           string      `json:"key"`
	Content       *model.Flag `json:"content"`
	State         DraftState  `json:"state"`
	Risk          Risk        `json:"risk"`
	UpdatedBy     string      `json:"updatedBy"`
	UpdatedAt     time.Time   `json:"updatedAt"`
	SubmittedBy   string      `json:"submittedBy,omitempty"`
	SubmittedAt   *time.Time  `json:"submittedAt,omitempty"`
	ApprovedBy    string      `json:"approvedBy,omitempty"`
	ApprovedAt    *time.Time  `json:"approvedAt,omitempty"`
	ReviewNote    string      `json:"reviewNote,omitempty"`
	Deleted       bool        `json:"deleted,omitempty"` // pending tombstone publish
	BaseRevision  int64       `json:"baseRevision"`      // optimistic lock: revision this draft was edited against
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
	WorkspaceID   string         `json:"workspaceId"`
	EnvironmentID string         `json:"environmentId"`
	Key           string         `json:"key"`
	Content       *model.Segment `json:"content"`
	State         DraftState     `json:"state"`
	Risk          Risk           `json:"risk"`
	UpdatedBy     string         `json:"updatedBy"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	SubmittedBy   string         `json:"submittedBy,omitempty"`
	ApprovedBy    string         `json:"approvedBy,omitempty"`
	BaseRevision  int64          `json:"baseRevision"`
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

// MetricSpec declares the primary metric of an experiment (§10.1: 首版指标
// 仅支持 proportion 与 mean，明确分母、零值、方向).
type MetricSpec struct {
	Key       string `json:"key"`
	Type      string `json:"type"`      // "proportion" | "mean"
	Direction string `json:"direction"` // "increase" | "decrease" — what "better" means
	Unit      string `json:"unit,omitempty"`
	// MinValue clamps conversion values for mean metrics (0 = no clamp).
	Floor *float64 `json:"floor,omitempty"`
	Ceil  *float64 `json:"ceil,omitempty"`
}

// GuardrailSpec is an operational protection rule, evaluated separately from
// statistical significance of the primary metric (§5.3, §10.3).
type GuardrailSpec struct {
	MetricKey string `json:"metricKey"`
	Type      string `json:"type"` // "proportion" | "mean"
	// Direction declares what movement is GOOD for this metric:
	// "increase" (default) or "decrease" (cost/latency style). A breach is
	// movement against the good direction beyond MaxRelativeRegression.
	Direction string `json:"direction,omitempty"`
	// MaxRelativeRegression: breach when the treatment arm regresses against
	// control by more than this fraction (0.05 = 5%).
	MaxRelativeRegression float64 `json:"maxRelativeRegression"`
	MinSample             int64   `json:"minSample"` // below this, no breach — insufficient data
}

// Experiment is the pre-registered analysis contract (§5.3). Changing the
// randomization unit, metric definitions, proportions or resource bindings
// creates a NEW epoch; history is never overwritten (§6).
type Experiment struct {
	ID              string                `json:"id"`
	WorkspaceID     string                `json:"workspaceId"`
	EnvironmentID   string                `json:"environmentId"`
	FlagKey         string                `json:"flagKey"`
	FlagRevision    int64                 `json:"flagRevision"` // revision the experiment was registered against
	Epoch           int64                 `json:"epoch"`
	Unit            string                `json:"unit"` // randomization-unit context field
	Layer           string                `json:"layer,omitempty"`
	LayerSlot       *model.BucketRange    `json:"layerSlot,omitempty"`
	Eligibility     float64               `json:"eligibility"` // percent, 0 = 100
	Variants        []string              `json:"variants"`    // explicit stable order
	Proportions     map[string]float64    `json:"proportions"`
	PrimaryMetric   MetricSpec            `json:"primaryMetric"`
	Guardrails      []GuardrailSpec       `json:"guardrails,omitempty"`
	Alpha           float64               `json:"alpha"`
	Power           float64               `json:"power"`
	MDE             float64               `json:"mde"`
	MinSamplePerArm int64                 `json:"minSamplePerArm"`
	MinRuntime      time.Duration         `json:"minRuntime"`
	AnalysisWindow  time.Duration         `json:"analysisWindow"`
	Hypothesis      string                `json:"hypothesis"`
	AnalysisMethod  string                `json:"analysisMethod"` // pre-registered, v1: "welch" | "two-proportion-z"
	State           model.ExperimentState `json:"state"`
	StartedAt       *time.Time            `json:"startedAt,omitempty"`
	StoppedAt       *time.Time            `json:"stoppedAt,omitempty"`
	CreatedAt       time.Time             `json:"createdAt"`
	UpdatedAt       time.Time             `json:"updatedAt"`
	CreatedBy       string                `json:"createdBy"`
}

// EventType distinguishes exposures from conversions (§10.1).
type EventType string

const (
	EventExposure   EventType = "exposure"
	EventConversion EventType = "conversion"
)

// Event is one deduplicated ingestion record. Unit is stored only as the
// tenant-scoped HMAC pseudonym (§10.2), never in clear.
type Event struct {
	EventID       string    `json:"eventId"`
	WorkspaceID   string    `json:"workspaceId"`
	EnvironmentID string    `json:"environmentId"`
	Type          EventType `json:"type"`
	ExperimentID  string    `json:"experimentId"`
	Epoch         int64     `json:"epoch"`
	FlagKey       string    `json:"flagKey,omitempty"`
	FlagRevision  int64     `json:"flagRevision,omitempty"`
	Variant       string    `json:"variant"`
	UnitHash      string    `json:"unitHash"`
	SessionID     string    `json:"sessionId,omitempty"`
	TaskID        string    `json:"taskId,omitempty"`
	Source        string    `json:"source,omitempty"` // split|targeting|default (exposures)
	MetricKey     string    `json:"metricKey,omitempty"`
	Value         float64   `json:"value,omitempty"`
	ClientTime    time.Time `json:"clientTime"`
	ServerTime    time.Time `json:"serverTime"`
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
	Digest        string    `json:"digest"` // MD5 hex from gonacos version metadata
	PinnedAt      time.Time `json:"pinnedAt"`
	PinnedBy      string    `json:"pinnedBy"`
}

// ModelAllowEntry is one row of the provider/model allowlist (§7: modelRef
// 是 GoLab 适配器维护的白名单).
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
