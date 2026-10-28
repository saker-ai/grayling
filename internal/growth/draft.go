package growth

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// PreparationInputs are trusted caller settings, supplied separately from model
// JSON and Reader evidence. AllocationMode must explicitly be fixed-split.
// AnalysisWindowHours is an observation window, never attribution duration.
type PreparationInputs struct {
	AllocationMode      string
	IntegrationOwner    string
	Alpha               float64
	Power               float64
	AnalysisWindowHours int64
	Guardrails          []GuardrailPreparation
}

type GuardrailPreparation struct {
	Ref MetricRef
	// A pointer distinguishes an explicitly supplied zero threshold from omission.
	MaxRelativeRegression *float64
	MinSample             int64
}

// CreateRequest is a create-API allowlist compatible with platform.Experiment.
// Do not embed platform.Experiment: server-owned fields must never appear here.
// Durations serialize as integer nanoseconds, as required by the existing API.
type CreateRequest struct {
	WorkspaceID      string             `json:"workspaceId"`
	EnvironmentID    string             `json:"environmentId"`
	FlagKey          string             `json:"flagKey"`
	Unit             string             `json:"unit"`
	Variants         []string           `json:"variants"`
	Proportions      map[string]float64 `json:"proportions"`
	AllocationMode   string             `json:"allocationMode"`
	ExposureMode     string             `json:"exposureMode"`
	ExposureTrigger  string             `json:"exposureTrigger"`
	IntegrationOwner string             `json:"integrationOwner"`
	PrimaryMetricRef MetricRef          `json:"primaryMetricRef"`
	Guardrails       []CreateGuardrail  `json:"guardrails"`
	Alpha            float64            `json:"alpha"`
	Power            float64            `json:"power"`
	MDE              float64            `json:"mde"`
	MinSamplePerArm  int64              `json:"minSamplePerArm"`
	MinRuntime       int64              `json:"minRuntime"`
	AnalysisWindow   int64              `json:"analysisWindow"`
	Hypothesis       string             `json:"hypothesis"`
	AnalysisMethod   string             `json:"analysisMethod"`
}

type CreateGuardrail struct {
	MetricRef             MetricRef `json:"metricRef"`
	MaxRelativeRegression float64   `json:"maxRelativeRegression"`
	MinSample             int64     `json:"minSample"`
}

// PreparationMetadata is provenance, not part of the experiment API request.
// Audience is retained as a planning claim; targeting/binding still needs review.
type PreparationMetadata struct {
	ProposalID             string                `json:"proposalId"`
	ProposalVersion        int64                 `json:"proposalVersion"`
	Audience               string                `json:"audience"`
	AttributionWindowHours int64                 `json:"attributionWindowHours"`
	EvidencePins           []ProposalEvidenceRef `json:"evidencePins"`
}

type PreparedExperiment struct {
	Request  CreateRequest       `json:"request"`
	Metadata PreparationMetadata `json:"metadata"`
}

// PreparationError contains only bounded compiler-owned field/code strings.
// Failure always returns a nil PreparedExperiment, never a partial request.
type PreparationError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

func (e *PreparationError) Error() string { return "growth: preparation " + e.Field + ": " + e.Code }

// CompileCreateRequest is pure and optional. Evidence must come separately from
// Reader.Read via trusted caller code. ReadyForReview is not human approval.
// This function neither resolves metric specs nor checks flags, allocates epochs,
// submits requests or establishes final platform legality/authorization.
func CompileCreateRequest(p Proposal, evidence *Envelope, in PreparationInputs) (*PreparedExperiment, error) {
	fail := func(field, code string) (*PreparedExperiment, error) {
		return nil, &PreparationError{Field: field, Code: code}
	}
	if ReviewProposal(p, evidence).State != ProposalReadyForReview {
		return fail("proposal", "not_ready_for_review")
	}
	if len(p.Variants) != 2 {
		return fail("variants", "exactly_two_required")
	}
	if in.AllocationMode != "fixed-split" {
		return fail("allocationMode", "fixed_split_required")
	}
	switch p.AnalysisPlan.Method {
	case "welch", "two-proportion-z", "ratio-delta", "quantile", "cuped":
	default:
		return fail("analysisPlan.method", "unsupported_method")
	}
	if strings.TrimSpace(in.IntegrationOwner) == "" || utf8.RuneCountInString(in.IntegrationOwner) > 200 || !utf8.ValidString(in.IntegrationOwner) {
		return fail("integrationOwner", "required_or_too_long")
	}
	if utf8.RuneCountInString(p.ExposureTrigger) > 200 {
		return fail("exposureTrigger", "too_long")
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(in.Alpha) || in.Alpha <= 0 || in.Alpha >= 1 {
		return fail("alpha", "out_of_range")
	}
	if !finite(in.Power) || in.Power <= 0 || in.Power >= 1 {
		return fail("power", "out_of_range")
	}
	validHours := func(hours int64) bool { return hours > 0 && hours <= math.MaxInt64/int64(time.Hour) }
	if !validHours(in.AnalysisWindowHours) {
		return fail("analysisWindowHours", "invalid_duration")
	}
	if !validHours(p.AnalysisPlan.AttributionWindowHours) {
		return fail("analysisPlan.attributionWindowHours", "invalid_duration")
	}
	if !validHours(p.SamplePlan.MinRuntimeHours) {
		return fail("samplePlan.minRuntimeHours", "invalid_duration")
	}
	if p.AnalysisPlan.AttributionWindowHours > in.AnalysisWindowHours {
		return fail("analysisWindowHours", "shorter_than_attribution")
	}
	if len(in.Guardrails) != len(p.Guardrails) {
		return fail("guardrails", "reference_mismatch")
	}
	guards := make([]CreateGuardrail, len(p.Guardrails))
	for i, m := range p.Guardrails {
		g := in.Guardrails[i]
		field := fmt.Sprintf("guardrails[%d]", i)
		if g.Ref != m.Ref {
			return fail(field+".ref", "reference_mismatch")
		}
		if g.MaxRelativeRegression == nil || !finite(*g.MaxRelativeRegression) || *g.MaxRelativeRegression < 0 {
			return fail(field+".maxRelativeRegression", "nonnegative_required")
		}
		if g.MinSample <= 0 {
			return fail(field+".minSample", "positive_required")
		}
		guards[i] = CreateGuardrail{MetricRef: m.Ref, MaxRelativeRegression: *g.MaxRelativeRegression, MinSample: g.MinSample}
	}
	variants := make([]string, 2)
	proportions := make(map[string]float64, 2)
	for i, v := range p.Variants {
		variants[i] = v.Key
		proportions[v.Key] = v.AllocationPercent
	}
	pins := make([]ProposalEvidenceRef, len(p.EvidenceRefs))
	for i, pin := range p.EvidenceRefs {
		pins[i] = pin
		pins[i].Window.Issues = append([]string(nil), pin.Window.Issues...)
	}
	return &PreparedExperiment{
		Request: CreateRequest{WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, FlagKey: p.FlagKey, Unit: p.Unit,
			Variants: variants, Proportions: proportions, AllocationMode: in.AllocationMode, ExposureMode: p.ExposureMode,
			ExposureTrigger: p.ExposureTrigger, IntegrationOwner: in.IntegrationOwner, PrimaryMetricRef: p.PrimaryMetric.Ref, Guardrails: guards,
			Alpha: in.Alpha, Power: in.Power, MDE: p.SamplePlan.MDE, MinSamplePerArm: p.SamplePlan.MinSamplePerArm,
			MinRuntime: p.SamplePlan.MinRuntimeHours * int64(time.Hour), AnalysisWindow: in.AnalysisWindowHours * int64(time.Hour),
			Hypothesis: p.Hypothesis, AnalysisMethod: p.AnalysisPlan.Method},
		Metadata: PreparationMetadata{ProposalID: p.ID, ProposalVersion: p.Version, Audience: p.Audience,
			AttributionWindowHours: p.AnalysisPlan.AttributionWindowHours, EvidencePins: pins},
	}, nil
}
