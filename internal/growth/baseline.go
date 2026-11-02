package growth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"time"
)

// AggregateBaseline is one finite, operator-authorized aggregate export. It is
// observational planning evidence, never an experimental or causal verdict.
type AggregateBaseline struct {
	SchemaVersion    string          `json:"schemaVersion"`
	SourceID         string          `json:"sourceId"`
	Version          int64           `json:"version"`
	WorkspaceID      string          `json:"workspaceId"`
	EnvironmentID    string          `json:"environmentId"`
	Metrics          []MetricRef     `json:"metrics"`
	Window           Window          `json:"window"`
	RefreshedAt      time.Time       `json:"refreshedAt"`
	Unit             string          `json:"unit"`
	License          string          `json:"license"`
	PrivacyPolicyRef string          `json:"privacyPolicyRef"`
	Synthetic        bool            `json:"synthetic"`
	Causal           bool            `json:"causal"`
	Groups           []BaselineGroup `json:"groups"`
}
type BaselineGroup struct {
	Role      string `json:"role"`
	Units     int64  `json:"units"`
	Converted int64  `json:"converted"`
}
type BaselinePin struct {
	SourceID    string `json:"sourceId"`
	Version     int64  `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

func validBaselinePin(p *BaselinePin) bool {
	return p == nil || (validSegment(p.SourceID) && p.Version > 0 && fingerprint.MatchString(p.Fingerprint))
}
func DecodeAggregateBaseline(r io.Reader) (*AggregateBaseline, error) {
	raw, e := io.ReadAll(io.LimitReader(r, 64<<10+1))
	if e != nil || len(raw) > 64<<10 {
		return nil, ErrConfig
	}
	var b *AggregateBaseline
	if json.Unmarshal(raw, &b, json.RejectUnknownMembers(true)) != nil || b == nil || ValidateAggregateBaseline(*b) != nil {
		return nil, ErrConfig
	}
	return b, nil
}
func ValidateAggregateBaseline(b AggregateBaseline) error {
	raw, e := json.Marshal(b)
	if e != nil || len(raw) > 64<<10 || b.SchemaVersion != "growth.aggregate-baseline.v1" || !validSegment(b.SourceID) || b.Version < 1 || !validSegment(b.WorkspaceID) || !validSegment(b.EnvironmentID) || b.Causal || b.Unit != "randomization_unit" || !learningText(b.License, 128) || !learningText(b.PrivacyPolicyRef, 256) || len(b.Metrics) < 2 || len(b.Metrics) > 21 || len(b.Groups) < 1 || len(b.Groups) > 2 || b.Window.Version != "aggregate-baseline-v1" || b.Window.Backend != "finite-aggregate-v1" || !usableWindow(b.Window) || len(b.Window.Issues) != 0 || b.RefreshedAt.IsZero() || b.RefreshedAt.Before(b.Window.Watermark) {
		return ErrConfig
	}
	seen := map[string]bool{}
	for _, m := range b.Metrics {
		if !validMetric(m) || seen[m.Key] {
			return ErrConfig
		}
		seen[m.Key] = true
	}
	roles := map[string]bool{}
	for _, g := range b.Groups {
		if (g.Role != "all" && g.Role != "control" && g.Role != "treatment") || roles[g.Role] || g.Units < 20 || g.Units > 1000000000 || g.Converted < 0 || g.Converted > g.Units {
			return ErrConfig
		}
		roles[g.Role] = true
	}
	if (len(b.Groups) == 1 && !roles["all"]) || (len(b.Groups) == 2 && (!roles["control"] || !roles["treatment"])) {
		return ErrConfig
	}
	return nil
}
func BaselineFingerprint(b AggregateBaseline) (string, error) {
	if ValidateAggregateBaseline(b) != nil {
		return "", ErrConfig
	}
	raw, e := json.Marshal(b, json.Deterministic(true))
	if e != nil {
		return "", ErrConfig
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func PinBaseline(b AggregateBaseline) (*BaselinePin, error) {
	h, e := BaselineFingerprint(b)
	if e != nil {
		return nil, e
	}
	return &BaselinePin{SourceID: b.SourceID, Version: b.Version, Fingerprint: h}, nil
}
func baselineMatches(p Proposal, e *Envelope) bool {
	if p.BaselinePin == nil || !validBaselinePin(p.BaselinePin) || e == nil || e.PlanningBaseline == nil {
		return false
	}
	b := e.PlanningBaseline
	pin, err := PinBaseline(*b)
	if err != nil || *pin != *p.BaselinePin || b.WorkspaceID != p.WorkspaceID || b.EnvironmentID != p.EnvironmentID || b.Metrics[0] != p.PrimaryMetric.Ref {
		return false
	}
	guards := []MetricRef{}
	for _, g := range p.Guardrails {
		guards = append(guards, g.Ref)
	}
	return exactMonitorGuards(b.Metrics[1:], guards)
}

// AttachPlanningBaseline requires a separate trusted aggregate adapter. A model
// proposal cannot manufacture this envelope field. Catalog facts remain exact.
func AttachPlanningBaseline(e *Envelope, b AggregateBaseline) (*Envelope, error) {
	if ValidateAggregateBaseline(b) != nil || e == nil || e.WorkspaceID != b.WorkspaceID || e.EnvironmentID != b.EnvironmentID || len(e.MissingPaths) != 0 || len(e.Metrics) != len(b.Metrics) || len(e.Evidence) != 0 {
		return nil, ErrMismatch
	}
	for _, ref := range b.Metrics {
		found := false
		for _, m := range e.Metrics {
			if m.WorkspaceID == b.WorkspaceID && m.Key == ref.Key && m.Version == ref.Version && m.Hash == ref.DefinitionHash && m.Status == "published" {
				found = true
			}
		}
		if !found {
			return nil, ErrMismatch
		}
	}
	copy := *e
	raw, _ := json.Marshal(b)
	var snapshot AggregateBaseline
	if json.Unmarshal(raw, &snapshot) != nil {
		return nil, ErrConfig
	}
	copy.PlanningBaseline = &snapshot
	copy.Status = Ready
	return &copy, nil
}
