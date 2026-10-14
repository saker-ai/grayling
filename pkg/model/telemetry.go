package model

import (
	"hash/fnv"
	"strings"
)

// EvaluationEvent is an opt-in, bounded telemetry projection of a decision.
// It contains neither the evaluation context nor the raw randomization unit.
type EvaluationEvent struct {
	Name       string         `json:"name"`
	DecisionID string         `json:"decisionId"`
	Attributes map[string]any `json:"attributes"`
}

// TelemetryEvent maps a result to the OpenTelemetry feature-flag evaluation
// event shape. The unstructured flag value is deliberately excluded.
func (r Result) TelemetryEvent() EvaluationEvent {
	attrs := map[string]any{
		"feature_flag.key":           r.FlagKey,
		"feature_flag.provider.name": "grayling",
		"feature_flag.result.reason": strings.ToLower(string(r.Reason)),
		"grayling.manifest.revision": r.ManifestRevision,
		"grayling.snapshot.age_ms":   r.SnapshotAgeMillis,
	}
	if r.FlagVersion != "" {
		attrs["feature_flag.version"] = r.FlagVersion
	}
	if r.VariationType != "" {
		attrs["feature_flag.result.variant"] = r.VariationType
	}
	if r.Failed {
		attrs["error.type"] = strings.ToLower(string(r.ErrorCode))
	}
	return EvaluationEvent{Name: "feature_flag.evaluation", DecisionID: r.DecisionID, Attributes: attrs}
}

// SampleDecision uses only the opaque decision ID, so repeated sampling
// decisions are stable and independent of any targeting data.
func SampleDecision(decisionID string, rate float64) bool {
	if rate <= 0 || decisionID == "" {
		return false
	}
	if rate >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(decisionID))
	return float64(h.Sum64())/float64(^uint64(0)) < rate
}
