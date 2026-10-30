package growth

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const ProposalSchemaVersion = "growth.proposal.v1"
const MaxProposalBytes = 64 << 10

var ErrProposal = errors.New("growth: invalid proposal JSON")

type ProposalState string

const (
	ProposalDraft               ProposalState = "draft"
	ProposalNeedsData           ProposalState = "needs_data"
	ProposalNeedsImplementation ProposalState = "needs_implementation"
	ProposalReadyForReview      ProposalState = "ready_for_review"
)

// Proposal is untrusted planning data, never authorization. No input state,
// approval, submission, or evidence envelope is part of this contract.
type Proposal struct {
	SchemaVersion      string                `json:"schemaVersion"`
	ID                 string                `json:"id"`
	Version            int64                 `json:"version"`
	GoalID             string                `json:"goalId"`
	WorkspaceID        string                `json:"workspaceId"`
	EnvironmentID      string                `json:"environmentId"`
	Hypothesis         string                `json:"hypothesis"`
	Rationale          string                `json:"rationale"`
	PrimaryMetric      ProposalMetric        `json:"primaryMetric"`
	Guardrails         []ProposalMetric      `json:"guardrails"`
	EvidenceRefs       []ProposalEvidenceRef `json:"evidenceRefs"`
	FlagKey            string                `json:"flagKey"`
	Variants           []ProposalVariant     `json:"variants"`
	Audience           string                `json:"audience"`
	Unit               string                `json:"unit"`
	ExposureMode       string                `json:"exposureMode"`
	ExposureTrigger    string                `json:"exposureTrigger"`
	Risk               string                `json:"risk"`
	Implementation     []Attestation         `json:"implementation"`
	AnalysisPlan       AnalysisPlan          `json:"analysisPlan"`
	SamplePlan         SamplePlan            `json:"samplePlan"`
	RollbackConditions []string              `json:"rollbackConditions"`
}

type ProposalMetric struct {
	Ref        MetricRef `json:"ref"`
	SourcePath string    `json:"sourcePath"`
}

type ProposalEvidenceRef struct {
	SourcePath      string `json:"sourcePath"`
	ExperimentID    string `json:"experimentId"`
	Epoch           int64  `json:"epoch"`
	EvidenceVersion string `json:"evidenceVersion"`
	Window          Window `json:"window"`
}

type ProposalVariant struct {
	Key               string  `json:"key"`
	AllocationPercent float64 `json:"allocationPercent"`
}

// Attestations must come from the implementing team in a future trusted flow.
// Presence here records a claim; this library cannot verify instrumentation.
// Required kinds: variant (one per key), stable_identity, render_exposure,
// conversion_unit. Unit must match the proposal on the latter three.
type Attestation struct {
	Kind       string `json:"kind"`
	Variant    string `json:"variant"`
	Unit       string `json:"unit"`
	Complete   bool   `json:"complete"`
	AttestedBy string `json:"attestedBy"`
	Source     string `json:"source"`
}

type Provenance struct {
	Source     string    `json:"source"`
	SuppliedBy string    `json:"suppliedBy"`
	SuppliedAt time.Time `json:"suppliedAt"`
}

type AnalysisPlan struct {
	Method                 string     `json:"method"`
	AttributionWindowHours int64      `json:"attributionWindowHours"`
	Provenance             Provenance `json:"provenance"`
}

// SamplePlan contains supplied planning inputs, not calculated statistics or
// a claim that sample size, power, or runtime is statistically adequate.
type SamplePlan struct {
	MinSamplePerArm int64      `json:"minSamplePerArm"`
	MinRuntimeHours int64      `json:"minRuntimeHours"`
	MDE             float64    `json:"mde"`
	Provenance      Provenance `json:"provenance"`
}

type Blocker struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

type ProposalReview struct {
	State    ProposalState `json:"state"`
	Blockers []Blocker     `json:"blockers"`
}

// DecodeProposal bounds input and rejects unknown members, duplicate members,
// invalid UTF-8, malformed/trailing JSON and null roots. Errors never echo text.
// Semantic incompleteness is reported by ReviewProposal, allowing draft editing.
func DecodeProposal(r io.Reader) (*Proposal, error) {
	if r == nil {
		return nil, ErrProposal
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxProposalBytes+1))
	if err != nil || len(data) > MaxProposalBytes {
		return nil, ErrProposal
	}
	var p *Proposal
	if json.Unmarshal(data, &p, json.RejectUnknownMembers(true)) != nil || p == nil {
		return nil, ErrProposal
	}
	return p, nil
}

// ReviewProposal consumes an envelope supplied separately by trusted caller code
// from Reader.Read, never one decoded from model output. Readiness means only
// completeness for human review, not approval, binding clearance or publishing.
// Structural blockers take precedence over data, then implementation blockers.
// Results are deterministic and field/code strings are stable contract values.
func ReviewProposal(p Proposal, evidence *Envelope) ProposalReview {
	out := ProposalReview{State: ProposalReadyForReview, Blockers: []Blocker{}}
	draft, data, implementation := false, false, false
	add := func(field, code, category string) {
		out.Blockers = append(out.Blockers, Blocker{Field: field, Code: code})
		switch category {
		case "draft":
			draft = true
		case "data":
			data = true
		case "implementation":
			implementation = true
		}
	}
	required := func(field, value string) {
		if strings.TrimSpace(value) == "" || len(value) > 4096 {
			add(field, "required_or_too_long", "draft")
		}
	}
	encoded, err := json.Marshal(p)
	if err != nil || len(encoded) > MaxProposalBytes {
		add("$", "proposal_too_large_or_invalid", "draft")
		out.State = ProposalDraft
		return out
	}
	if p.SchemaVersion != ProposalSchemaVersion {
		add("schemaVersion", "unsupported_version", "draft")
	}
	for _, f := range []struct{ name, value string }{{"id", p.ID}, {"goalId", p.GoalID}, {"workspaceId", p.WorkspaceID}, {"environmentId", p.EnvironmentID}, {"flagKey", p.FlagKey}, {"unit", p.Unit}} {
		if !validSegment(f.value) {
			add(f.name, "invalid_identifier", "draft")
		}
	}
	if p.Version < 1 {
		add("version", "positive_required", "draft")
	}
	for _, f := range []struct{ name, value string }{{"hypothesis", p.Hypothesis}, {"rationale", p.Rationale}, {"audience", p.Audience}, {"exposureTrigger", p.ExposureTrigger}, {"analysisPlan.method", p.AnalysisPlan.Method}} {
		required(f.name, f.value)
	}
	if p.ExposureMode != "onRender" {
		add("exposureMode", "on_render_required", "draft")
	}
	if p.Risk != "low" && p.Risk != "medium" && p.Risk != "high" {
		add("risk", "invalid_risk", "draft")
	}
	if len(p.RollbackConditions) < 1 || len(p.RollbackConditions) > 20 {
		add("rollbackConditions", "count_out_of_bounds", "draft")
	}
	for i, condition := range p.RollbackConditions {
		required(fmt.Sprintf("rollbackConditions[%d]", i), condition)
	}
	provenance := func(field string, v Provenance) {
		required(field+".source", v.Source)
		required(field+".suppliedBy", v.SuppliedBy)
		if v.SuppliedAt.IsZero() {
			add(field+".suppliedAt", "required", "draft")
		}
	}
	provenance("analysisPlan.provenance", p.AnalysisPlan.Provenance)
	provenance("samplePlan.provenance", p.SamplePlan.Provenance)
	if p.AnalysisPlan.AttributionWindowHours < 1 {
		add("analysisPlan.attributionWindowHours", "positive_required", "draft")
	}
	if p.SamplePlan.MinSamplePerArm < 1 {
		add("samplePlan.minSamplePerArm", "positive_required", "draft")
	}
	if p.SamplePlan.MinRuntimeHours < 1 {
		add("samplePlan.minRuntimeHours", "positive_required", "draft")
	}
	if math.IsNaN(p.SamplePlan.MDE) || math.IsInf(p.SamplePlan.MDE, 0) || p.SamplePlan.MDE <= 0 {
		add("samplePlan.mde", "positive_required", "draft")
	}
	if len(p.Variants) < 2 || len(p.Variants) > 20 {
		add("variants", "count_out_of_bounds", "draft")
	}
	variants := map[string]bool{}
	total := 0.0
	for i, v := range p.Variants {
		field := fmt.Sprintf("variants[%d]", i)
		if !validSegment(v.Key) || variants[v.Key] {
			add(field+".key", "invalid_or_duplicate", "draft")
		}
		variants[v.Key] = true
		if v.AllocationPercent <= 0 || v.AllocationPercent > 100 {
			add(field+".allocationPercent", "invalid_allocation", "draft")
		}
		total += v.AllocationPercent
	}
	if math.Abs(total-100) > 1e-9 {
		add("variants", "allocation_total", "draft")
	}
	if evidence == nil {
		add("evidenceRefs", "missing_envelope", "data")
	} else {
		if evidence.WorkspaceID != p.WorkspaceID || evidence.EnvironmentID != p.EnvironmentID {
			add("evidenceRefs", "scope_mismatch", "data")
		}
		if evidence.Status != Ready || len(evidence.MissingPaths) != 0 {
			add("evidenceRefs", "evidence_not_ready", "data")
		}
	}
	metrics := append([]ProposalMetric{p.PrimaryMetric}, p.Guardrails...)
	if len(p.Guardrails) < 1 || len(p.Guardrails) > 20 {
		add("guardrails", "count_out_of_bounds", "draft")
	}
	seenMetrics := map[string]bool{}
	for i, m := range metrics {
		field := "primaryMetric"
		if i > 0 {
			field = fmt.Sprintf("guardrails[%d]", i-1)
		}
		if !validMetric(m.Ref) || seenMetrics[metricID(m.Ref)] {
			add(field+".ref", "invalid_or_duplicate_metric", "data")
		}
		seenMetrics[metricID(m.Ref)] = true
		path := "/api/v1/admin/workspaces/" + p.WorkspaceID + "/metrics/" + m.Ref.Key + "/versions/" + strconv.FormatInt(m.Ref.Version, 10)
		matched := false
		if evidence != nil {
			for _, got := range evidence.Metrics {
				if got.WorkspaceID == p.WorkspaceID && got.Key == m.Ref.Key && got.Version == m.Ref.Version && got.Hash == m.Ref.DefinitionHash && got.SourcePath == m.SourcePath && m.SourcePath == path {
					matched = true
					if got.Status != "published" {
						add(field, "metric_not_published", "data")
					}
				}
			}
		}
		if !matched {
			add(field, "metric_evidence_mismatch", "data")
		}
	}
	if len(p.EvidenceRefs) < 1 || len(p.EvidenceRefs) > 20 {
		add("evidenceRefs", "count_out_of_bounds", "data")
	}
	seenEvidence := map[string]bool{}
	for i, ref := range p.EvidenceRefs {
		field := fmt.Sprintf("evidenceRefs[%d]", i)
		if !validSegment(ref.ExperimentID) || ref.Epoch < 1 || !fingerprint.MatchString(ref.EvidenceVersion) || !usableWindow(ref.Window) || seenEvidence[ref.ExperimentID] {
			add(field, "invalid_or_duplicate_reference", "data")
		}
		seenEvidence[ref.ExperimentID] = true
		matched := false
		if evidence != nil {
			for _, got := range evidence.Evidence {
				r := got.Report
				if got.SourcePath != ref.SourcePath || ref.SourcePath != "/api/v1/admin/experiments/"+ref.ExperimentID+"/report/export?format=json" || r.ExperimentID != ref.ExperimentID || r.Epoch != ref.Epoch || r.EvidenceVersion != ref.EvidenceVersion || !sameWindow(r.Window, ref.Window) {
					continue
				}
				matched = true
				if got.Status != Ready || !usableWindow(r.Window) {
					add(field, "evidence_not_ready", "data")
				}
				if r.PrimaryMetricRef == nil || *r.PrimaryMetricRef != p.PrimaryMetric.Ref || len(r.GuardrailMetricRefs) != len(p.Guardrails) {
					add(field, "report_metrics_mismatch", "data")
				} else {
					for _, m := range p.Guardrails {
						found := false
						for _, g := range r.GuardrailMetricRefs {
							if m.Ref == g {
								found = true
							}
						}
						if !found {
							add(field, "report_metrics_mismatch", "data")
							break
						}
					}
				}
			}
		}
		if !matched {
			add(field, "evidence_reference_mismatch", "data")
		}
	}
	if len(p.Implementation) > 40 {
		add("implementation", "count_out_of_bounds", "draft")
	}
	attested := map[string]bool{}
	for i, a := range p.Implementation {
		field := fmt.Sprintf("implementation[%d]", i)
		key := a.Kind
		valid := a.Kind == "stable_identity" || a.Kind == "render_exposure" || a.Kind == "conversion_unit"
		if a.Kind == "variant" {
			valid = variants[a.Variant]
			key += ":" + a.Variant
		} else if a.Variant != "" {
			valid = false
		}
		if !valid || attested[key] {
			add(field, "invalid_or_duplicate_attestation", "implementation")
		}
		if !a.Complete || strings.TrimSpace(a.AttestedBy) == "" || strings.TrimSpace(a.Source) == "" || (a.Kind != "variant" && a.Unit != p.Unit) {
			add(field, "incomplete_attestation", "implementation")
		} else if valid {
			attested[key] = true
		}
	}
	for _, v := range p.Variants {
		if !attested["variant:"+v.Key] {
			add("implementation.variant."+v.Key, "attestation_required", "implementation")
		}
	}
	for _, kind := range []string{"stable_identity", "render_exposure", "conversion_unit"} {
		if !attested[kind] {
			add("implementation."+kind, "attestation_required", "implementation")
		}
	}
	switch {
	case draft:
		out.State = ProposalDraft
	case data:
		out.State = ProposalNeedsData
	case implementation:
		out.State = ProposalNeedsImplementation
	}
	return out
}
