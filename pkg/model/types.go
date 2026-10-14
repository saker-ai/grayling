// Package model defines the Grayling flag DSL (schemaVersion 1), segment,
// snapshot, manifest and evaluation-result types shared by the control
// plane, the Go SDK and remote evaluation (docs/技术方案.md §5).
//
// The DSL is deliberately NOT the go-feature-flag document format: the
// importer (internal/gffimport) translates a whitelisted subset of gff
// fields and rejects everything else. Native evaluation implements the
// grayling-fnv1a-10000-v1 fixed-bucket semantics from pkg/bucketing.
package model

import (
	"time"
)

// SchemaVersion is the current DSL/snapshot/manifest schema generation.
const SchemaVersion = 1

// FlagType enumerates the supported variation value types.
type FlagType string

const (
	FlagBool   FlagType = "bool"
	FlagString FlagType = "string"
	FlagNumber FlagType = "number"
	FlagJSON   FlagType = "json"
)

// Flag is one feature flag / experiment carrier in Grayling DSL form.
type Flag struct {
	SchemaVersion int      `yaml:"schemaVersion" json:"schemaVersion"`
	Key           string   `yaml:"key" json:"key"`
	Type          FlagType `yaml:"type" json:"type"`
	// Disable is the kill switch. A disabled flag serves its DefaultRule
	// with reason DISABLED; offline nodes only observe it after the next
	// successful sync (bounded by the SDK max-staleness window).
	Disable bool `yaml:"disable,omitempty" json:"disable,omitempty"`
	// Version carries the Grayling release revision (informational; the
	// authoritative revision lives in the manifest).
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Bucketing configures the stable hash. Required when any rule uses
	// percentage splits or the flag binds an experiment.
	Bucketing *Bucketing `yaml:"bucketing,omitempty" json:"bucketing,omitempty"`
	// Fields is the registered context-field allowlist. Every constraint
	// field (including those of referenced segments) and the bucketing unit
	// must appear here; "targetingKey" is always implicitly registered.
	Fields []string `yaml:"fields,omitempty" json:"fields,omitempty"`
	// Variations maps variation name → value. Values must match Type
	// (json accepts any object/array/scalar).
	Variations map[string]any `yaml:"variations" json:"variations"`
	// DefaultRule is applied when no targeting rule matches. Required.
	DefaultRule *Rule `yaml:"defaultRule" json:"defaultRule"`
	// Targeting rules match in list order; the first match wins.
	Targeting []Rule `yaml:"targeting,omitempty" json:"targeting,omitempty"`
	// Experiment optionally binds this flag to an experiment. When set,
	// hash coordinates use (Experiment.ID, Experiment.Epoch) instead of
	// (Key, 0), and eligibility/layer gating runs before rule matching.
	Experiment *ExperimentBinding `yaml:"experiment,omitempty" json:"experiment,omitempty"`
}

// Bucketing pins the stable-hash configuration of a flag.
type Bucketing struct {
	// Algorithm must be "grayling-fnv1a-10000-v1" in schema v1.
	Algorithm string `yaml:"algorithm" json:"algorithm"`
	// Unit names the context field providing the randomization unit
	// ("targetingKey" or one of Flag.Fields). Changing it requires a new
	// experiment epoch. ClientIPField ("client_ip") is a legal unit for
	// traffic-routing flags but is experiment-discouraged: IPs drift
	// (NAT, DHCP, proxy chains), which breaks unit stability and thus
	// stickiness/attribution (§6) — allowed, warned in docs, not blocked.
	Unit string `yaml:"unit" json:"unit"`
	// Salt domain-separates the hash. Changing it requires a new epoch.
	Salt string `yaml:"salt,omitempty" json:"salt,omitempty"`
}

// ExperimentState is the lifecycle state mirrored into published snapshots.
// State transitions in the control plane republish the flag revision so that
// locally-evaluating SDKs observe them through the normal sync path.
type ExperimentState string

const (
	ExperimentDraft     ExperimentState = "draft"
	ExperimentReviewed  ExperimentState = "reviewed"
	ExperimentScheduled ExperimentState = "scheduled"
	ExperimentRunning   ExperimentState = "running"
	ExperimentPaused    ExperimentState = "paused"
	ExperimentStopped   ExperimentState = "stopped"
	ExperimentAnalyzed  ExperimentState = "analyzed"
	ExperimentArchived  ExperimentState = "archived"
)

// ExperimentBinding ties a flag revision to an experiment epoch.
type ExperimentBinding struct {
	ID           string `yaml:"id" json:"id"`
	Epoch        int64  `yaml:"epoch" json:"epoch"`
	ExposureMode string `yaml:"exposureMode,omitempty" json:"exposureMode,omitempty"`
	// Bandit is an immutable published policy snapshot. Older SDKs ignore it
	// and use the registered percentage rule as the safe fixed split.
	Bandit *BanditBinding `yaml:"bandit,omitempty" json:"bandit,omitempty"`
	// Layer is the mutual-exclusion layer ID (empty = no layer).
	Layer string `yaml:"layer,omitempty" json:"layer,omitempty"`
	// LayerSlot is the server-assigned disjoint bucket range inside the
	// layer's hash coordinate. Units outside the slot are not enrolled.
	LayerSlot *BucketRange `yaml:"layerSlot,omitempty" json:"layerSlot,omitempty"`
	// Eligibility is the enrollment percentage in [0,100]; 0/unset = 100.
	// Evaluated on the separate purpose="eligibility" coordinate.
	Eligibility float64 `yaml:"eligibility,omitempty" json:"eligibility,omitempty"`
	// State is the experiment state at publish time. Only "running"
	// enrolls units and emits exposures.
	State ExperimentState `yaml:"state" json:"state"`
}

// BanditBinding carries one versioned policy state in a published flag.
// Counts are ordered exactly like Variants; they are recomputed from the
// durable event ledger at the feedback watermark before publication.
type BanditBinding struct {
	PolicyVersion string    `yaml:"policyVersion" json:"policyVersion"`
	Strategy      string    `yaml:"strategy" json:"strategy"`
	Variants      []string  `yaml:"variants" json:"variants"`
	PriorAlpha    float64   `yaml:"priorAlpha" json:"priorAlpha"`
	PriorBeta     float64   `yaml:"priorBeta" json:"priorBeta"`
	Epsilon       float64   `yaml:"epsilon,omitempty" json:"epsilon,omitempty"`
	StateVersion  int64     `yaml:"stateVersion" json:"stateVersion"`
	Watermark     time.Time `yaml:"watermark" json:"watermark"`
	Trials        []int64   `yaml:"trials" json:"trials"`
	Successes     []int64   `yaml:"successes" json:"successes"`
	Frozen        bool      `yaml:"frozen,omitempty" json:"frozen,omitempty"`
	FreezeReason  string    `yaml:"freezeReason,omitempty" json:"freezeReason,omitempty"`
}

// BucketRange is a half-open bucket interval [Start, End) within [0,10000).
type BucketRange struct {
	Start int `yaml:"start" json:"start"`
	End   int `yaml:"end" json:"end"`
}

// Rule is a targeting rule or the default rule.
//
// Exactly one of Variation / Percentage must be set. Constraints combine
// with AND; referenced segments combine with OR among themselves and AND
// with the constraints. Disabled rules are skipped.
type Rule struct {
	Name        string             `yaml:"name,omitempty" json:"name,omitempty"`
	Disable     bool               `yaml:"disable,omitempty" json:"disable,omitempty"`
	Constraints []Constraint       `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Segments    []string           `yaml:"segments,omitempty" json:"segments,omitempty"`
	Percentage  map[string]float64 `yaml:"percentage,omitempty" json:"percentage,omitempty"`
	Variation   string             `yaml:"variation,omitempty" json:"variation,omitempty"`
}

// Constraint is one bounded field test. The operator set is closed (§5.2);
// unregistered fields and unknown operators are rejected at validation.
type Constraint struct {
	Field string `yaml:"field" json:"field"`
	Op    string `yaml:"op" json:"op"`
	// Value is the operand for scalar operators (eq, ne, gt, ..., regex,
	// in_cidr, semver_*, time_*).
	Value any `yaml:"value,omitempty" json:"value,omitempty"`
	// Values is the operand list for set operators (in, not_in).
	Values []any `yaml:"values,omitempty" json:"values,omitempty"`
}

// Closed operator set (docs §5.2: 等值、集合、数值、时间、正则、CIDR、semver).
const (
	OpEq         = "eq"
	OpNe         = "ne"
	OpIn         = "in"
	OpNotIn      = "not_in"
	OpGt         = "gt"
	OpGte        = "gte"
	OpLt         = "lt"
	OpLte        = "lte"
	OpTimeBefore = "time_before"
	OpTimeAfter  = "time_after"
	OpRegex      = "regex"
	OpInCIDR     = "in_cidr"
	OpSemverGt   = "semver_gt"
	OpSemverGte  = "semver_gte"
	OpSemverLt   = "semver_lt"
	OpSemverLte  = "semver_lte"
	OpSemverEq   = "semver_eq"
)

// Operators is the closed set accepted by validation.
var Operators = map[string]bool{
	OpEq: true, OpNe: true, OpIn: true, OpNotIn: true,
	OpGt: true, OpGte: true, OpLt: true, OpLte: true,
	OpTimeBefore: true, OpTimeAfter: true, OpRegex: true, OpInCIDR: true,
	OpSemverGt: true, OpSemverGte: true, OpSemverLt: true, OpSemverLte: true,
	OpSemverEq: true,
}

// Segment is a reusable, versioned audience definition scoped to a
// workspace. Constraints AND; nested segments OR; cycles are rejected at
// validation and depth is bounded.
type Segment struct {
	SchemaVersion int          `yaml:"schemaVersion" json:"schemaVersion"`
	Key           string       `yaml:"key" json:"key"`
	Version       string       `yaml:"version,omitempty" json:"version,omitempty"`
	Constraints   []Constraint `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Segments      []string     `yaml:"segments,omitempty" json:"segments,omitempty"`
}

// Manifest is the per-(workspace, environment) index that solves the Nacos
// "known dataId only" enumeration problem (§3): SDKs subscribe to or poll
// the manifest, then fetch the immutable per-revision flag documents it
// references. Published only after all referenced flag writes are read-back
// verified; deletions appear as tombstones.
//
// Workspace/Environment carry the server-generated surrogate IDs — these
// feed the bucketing hash coordinates and must be rename-proof. The Slug
// fields are display-only.
type Manifest struct {
	SchemaVersion   int             `yaml:"schemaVersion" json:"schemaVersion"`
	Workspace       string          `yaml:"workspace" json:"workspace"`
	Environment     string          `yaml:"environment" json:"environment"`
	WorkspaceSlug   string          `yaml:"workspaceSlug,omitempty" json:"workspaceSlug,omitempty"`
	EnvironmentSlug string          `yaml:"environmentSlug,omitempty" json:"environmentSlug,omitempty"`
	Revision        int64           `yaml:"revision" json:"revision"`
	GeneratedAt     time.Time       `yaml:"generatedAt" json:"generatedAt"`
	Flags           []ManifestEntry `yaml:"flags" json:"flags"`
	Segments        []ManifestEntry `yaml:"segments,omitempty" json:"segments,omitempty"`
}

// ManifestEntry points at one immutable config document.
type ManifestEntry struct {
	Key      string `yaml:"key" json:"key"`
	Revision int64  `yaml:"revision" json:"revision"`
	// DataID is the gonacos dataId holding the document, e.g.
	// grayling.flag.<key>.r<revision>.
	DataID string `yaml:"dataId" json:"dataId"`
	// Hash is the SHA-256 hex of the document's canonical JSON encoding.
	Hash string `yaml:"hash" json:"hash"`
	// Deleted marks a tombstone: clients must evict the key from cache.
	Deleted bool `yaml:"deleted,omitempty" json:"deleted,omitempty"`
}

// Snapshot is the atomic unit an evaluator works on: one manifest revision
// with every referenced document downloaded, hash-verified and parsed.
// Partial mixes of manifest revisions are forbidden (§5.2).
// Workspace/Environment are surrogate IDs (hash coordinates); slugs are
// display-only.
type Snapshot struct {
	SchemaVersion    int                 `yaml:"schemaVersion" json:"schemaVersion"`
	Workspace        string              `yaml:"workspace" json:"workspace"`
	Environment      string              `yaml:"environment" json:"environment"`
	WorkspaceSlug    string              `yaml:"workspaceSlug,omitempty" json:"workspaceSlug,omitempty"`
	EnvironmentSlug  string              `yaml:"environmentSlug,omitempty" json:"environmentSlug,omitempty"`
	ManifestRevision int64               `yaml:"manifestRevision" json:"manifestRevision"`
	GeneratedAt      time.Time           `yaml:"generatedAt" json:"generatedAt"`
	Flags            map[string]*Flag    `yaml:"flags" json:"flags"`
	Segments         map[string]*Segment `yaml:"segments,omitempty" json:"segments,omitempty"`
}

// EvaluationContext carries the randomization unit and targeting attributes.
// TargetingKey must be a stable, authenticated or server-generated identity
// (§6); the evaluator never invents one.
type EvaluationContext struct {
	TargetingKey string         `yaml:"targetingKey,omitempty" json:"targetingKey,omitempty"`
	Attributes   map[string]any `yaml:"attributes,omitempty" json:"attributes,omitempty"`
}

// TargetingKeyField is the implicit context field name bound to
// EvaluationContext.TargetingKey.
const TargetingKeyField = "targetingKey"

// ClientIPField is the RESERVED context-attribute name under which trusted
// edge infrastructure (grayling-proxy `-trusted-proxy-cidrs`,
// docs/traefik-grayling-architecture.md "按 client IP 灰度") injects the client
// IP it derived from the trusted proxy chain. It is NOT implicitly
// registered: a flag opts in by listing "client_ip" in Fields, after which
// it can drive constraints (typically in_cidr) or — allowed but
// EXPERIMENT-DISCOURAGED (§6: NAT/动态分配/代理链变化使 IP 单元不稳定，
// 实验级分桶应使用认证稳定 ID) — act as the bucketing unit.
//
// Precedence: on the remote-evaluation relay the caller's explicit
// attributes["client_ip"] wins and is never overwritten (an authenticated
// SDK caller may itself sit behind a sanitized edge); the proxy-derived
// value is injected only when the attribute is absent. On the gateway
// forwardAuth path the value is ALWAYS the proxy-derived one — header
// input never feeds it (anti-forgery).
const ClientIPField = "client_ip"

// Field looks up a registered field: "targetingKey" resolves to the context
// key, everything else to Attributes.
func (c EvaluationContext) Field(name string) (any, bool) {
	if name == TargetingKeyField {
		if c.TargetingKey == "" {
			return nil, false
		}
		return c.TargetingKey, true
	}
	v, ok := c.Attributes[name]
	return v, ok
}

// Reason explains how an evaluation reached its value (OpenFeature-aligned
// plus Grayling experiment states).
type Reason string

const (
	ReasonDefault              Reason = "DEFAULT"
	ReasonTargetingMatch       Reason = "TARGETING_MATCH"
	ReasonSplit                Reason = "SPLIT"
	ReasonDisabled             Reason = "DISABLED"
	ReasonError                Reason = "ERROR"
	ReasonNotEnrolled          Reason = "NOT_ENROLLED"
	ReasonExperimentNotRunning Reason = "EXPERIMENT_NOT_RUNNING"
	ReasonStale                Reason = "STALE" // SDK: snapshot past max staleness
)

// ErrorCode classifies failures; empty when none.
type ErrorCode string

const (
	ErrNone             ErrorCode = ""
	ErrFlagNotFound     ErrorCode = "FLAG_NOT_FOUND"
	ErrMissingBucketKey ErrorCode = "MISSING_BUCKET_KEY"
	ErrTypeMismatch     ErrorCode = "TYPE_MISMATCH"
	ErrParse            ErrorCode = "PARSE_ERROR"
	ErrGeneral          ErrorCode = "GENERAL"
)

// Exposure describes an experiment assignment that should be reported.
type Exposure struct {
	ExperimentID string `json:"experimentId"`
	Epoch        int64  `json:"epoch"`
	Variant      string `json:"variant"`
	ExposureMode string `json:"exposureMode,omitempty"`
	// Source is how the arm was reached: "split", "targeting" or "default".
	// Analysis filters on this because only "split" arms are randomized
	// (SRM applies to the randomized population).
	Source                string  `json:"source"`
	PolicyVersion         string  `json:"policyVersion,omitempty"`
	StateVersion          int64   `json:"stateVersion,omitempty"`
	AllocationProbability float64 `json:"allocationProbability,omitempty"`
}

// Result is the outcome of one evaluation. Failed results carry the
// caller-provided default in the typed APIs and must never be interpreted
// as an assignment.
type Result struct {
	FlagKey           string    `json:"flagKey"`
	Value             any       `json:"value"`
	VariationType     string    `json:"variationType"`
	Reason            Reason    `json:"reason"`
	ErrorCode         ErrorCode `json:"errorCode,omitempty"`
	ErrorDetails      string    `json:"errorDetails,omitempty"`
	Failed            bool      `json:"failed"`
	FlagVersion       string    `json:"flagVersion,omitempty"`
	TrackEvents       bool      `json:"trackEvents"`
	Experiment        *Exposure `json:"experiment,omitempty"`
	Bucket            int       `json:"bucket"`         // -1 when unused; bucket 0 is valid
	Rule              string    `json:"rule,omitempty"` // matched rule name or "default"
	DecisionID        string    `json:"decisionId,omitempty"`
	ManifestRevision  int64     `json:"manifestRevision"`
	SnapshotAgeMillis int64     `json:"snapshotAgeMillis"`
	ExposureEventID   string    `json:"exposureEventId,omitempty"`
}

// DecisionDiagnostic is a scoped, bounded record of a recent evaluation.
// It intentionally contains no context attributes or randomization unit.
type DecisionDiagnostic struct {
	Result               Result `json:"result"`
	ExposureStatus       string `json:"exposureStatus"` // none|queued|submitted|recorded|dropped
	ConversionCount      int64  `json:"conversionCount"`
	ConversionsTruncated bool   `json:"conversionsTruncated,omitzero"`
}
