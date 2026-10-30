package growth

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

func preparationFixture(p Proposal) PreparationInputs {
	threshold := 0.05
	return PreparationInputs{AllocationMode: "fixed-split", IntegrationOwner: "integration team", Alpha: 0.025, Power: 0.9, AnalysisWindowHours: 48,
		Guardrails: []GuardrailPreparation{{Ref: p.Guardrails[0].Ref, MaxRelativeRegression: &threshold, MinSample: 123}}}
}

func TestCompileCreateRequestMapping(t *testing.T) {
	p, e := proposalFixture()
	p.Variants = []ProposalVariant{{Key: "treatment", AllocationPercent: 37.5}, {Key: "control", AllocationPercent: 62.5}}
	in := preparationFixture(p)
	beforeP, _ := json.Marshal(p)
	beforeE, _ := json.Marshal(e)
	beforeIn, _ := json.Marshal(in)
	got, err := CompileCreateRequest(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CompileCreateRequest(p, e, in)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("nondeterministic: %v", err)
	}
	body, err := json.Marshal(got.Request, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := json.Marshal(again.Request, json.Deterministic(true))
	if string(body) != string(body2) {
		t.Fatal("unstable JSON")
	}
	var exp platform.Experiment
	if err := jsonv1.Unmarshal(body, &exp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exp.Variants, []string{"treatment", "control"}) || exp.Proportions["treatment"] != 37.5 || exp.Proportions["control"] != 62.5 || exp.WorkspaceID != p.WorkspaceID || exp.EnvironmentID != p.EnvironmentID || exp.FlagKey != p.FlagKey || exp.Unit != p.Unit || exp.Hypothesis != p.Hypothesis || exp.AnalysisMethod != p.AnalysisPlan.Method || exp.ExposureMode != "onRender" || exp.ExposureTrigger != p.ExposureTrigger || exp.IntegrationOwner != in.IntegrationOwner || exp.Alpha != in.Alpha || exp.Power != in.Power || exp.MDE != p.SamplePlan.MDE || exp.MinSamplePerArm != p.SamplePlan.MinSamplePerArm || exp.MinRuntime != 336*time.Hour || exp.AnalysisWindow != 48*time.Hour {
		t.Fatalf("mapping: %+v", exp)
	}
	if exp.PrimaryMetricRef == nil || exp.PrimaryMetricRef.DefinitionHash != p.PrimaryMetric.Ref.DefinitionHash || exp.Guardrails[0].MetricRef.DefinitionHash != p.Guardrails[0].Ref.DefinitionHash || exp.Guardrails[0].MinSample != 123 || exp.Guardrails[0].MaxRelativeRegression != 0.05 {
		t.Fatalf("metric mapping: %+v", exp)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	const keys = "workspaceId environmentId flagKey unit variants proportions allocationMode exposureMode exposureTrigger integrationOwner primaryMetricRef guardrails alpha power mde minSamplePerArm minRuntime analysisWindow hypothesis analysisMethod"
	if len(fields) != len(strings.Fields(keys)) {
		t.Fatalf("unexpected fields: %v", fields)
	}
	for _, key := range strings.Fields(keys) {
		if _, ok := fields[key]; !ok {
			t.Fatal(key)
		}
	}
	if got.Metadata.ProposalID != p.ID || got.Metadata.ProposalVersion != p.Version || got.Metadata.Audience != p.Audience || got.Metadata.AttributionWindowHours != 24 || !reflect.DeepEqual(got.Metadata.EvidencePins, p.EvidenceRefs) {
		t.Fatalf("metadata: %+v", got.Metadata)
	}
	// Compiler calls and mutations of their outputs cannot mutate any input.
	got.Request.Variants[0] = "changed"
	got.Request.Proportions["control"] = 1
	got.Request.Guardrails[0].MetricRef.Key = "changed"
	got.Metadata.EvidencePins[0].Window.Issues = append(got.Metadata.EvidencePins[0].Window.Issues, "changed")
	afterP, _ := json.Marshal(p)
	afterE, _ := json.Marshal(e)
	afterIn, _ := json.Marshal(in)
	if string(beforeP) != string(afterP) || string(beforeE) != string(afterE) || string(beforeIn) != string(afterIn) {
		t.Fatal("input mutation")
	}
}

func TestCompileCreateRequestRejects(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Proposal, *Envelope, *PreparationInputs)
		field, code string
	}{
		{"hold", func(_ *Proposal, e *Envelope, _ *PreparationInputs) { e.Status = Hold }, "proposal", "not_ready_for_review"},
		{"needs data", func(_ *Proposal, e *Envelope, _ *PreparationInputs) { e.Status = NeedsData }, "proposal", "not_ready_for_review"},
		{"scope", func(_ *Proposal, e *Envelope, _ *PreparationInputs) { e.EnvironmentID = "other" }, "proposal", "not_ready_for_review"},
		{"metric pin", func(_ *Proposal, e *Envelope, _ *PreparationInputs) { e.Metrics[0].Hash = hashB }, "proposal", "not_ready_for_review"},
		{"report pin", func(_ *Proposal, e *Envelope, _ *PreparationInputs) { e.Evidence[0].Report.Epoch++ }, "proposal", "not_ready_for_review"},
		{"implementation", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.Implementation = nil }, "proposal", "not_ready_for_review"},
		{"one arm", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.Variants = p.Variants[:1] }, "proposal", "not_ready_for_review"},
		{"three arms", func(p *Proposal, _ *Envelope, _ *PreparationInputs) {
			p.Variants = []ProposalVariant{{"control", 30}, {"treatment", 30}, {"third", 40}}
			p.Implementation = append(p.Implementation, Attestation{Kind: "variant", Variant: "third", Complete: true, AttestedBy: "dev", Source: "review"})
		}, "variants", "exactly_two_required"},
		{"bandit", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.AllocationMode = "bandit" }, "allocationMode", "fixed_split_required"},
		{"missing allocation", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.AllocationMode = "" }, "allocationMode", "fixed_split_required"},
		{"sequential", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.AnalysisPlan.Method = "sequential" }, "analysisPlan.method", "unsupported_method"},
		{"bandit method", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.AnalysisPlan.Method = "bandit-beta-bernoulli" }, "analysisPlan.method", "unsupported_method"},
		{"owner", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.IntegrationOwner = "" }, "integrationOwner", "required_or_too_long"},
		{"owner length", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.IntegrationOwner = strings.Repeat("x", 201) }, "integrationOwner", "required_or_too_long"},
		{"trigger length", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.ExposureTrigger = strings.Repeat("x", 201) }, "exposureTrigger", "too_long"},
		{"alpha", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Alpha = 0 }, "alpha", "out_of_range"},
		{"alpha nan", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Alpha = math.NaN() }, "alpha", "out_of_range"},
		{"power inf", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Power = math.Inf(1) }, "power", "out_of_range"},
		{"power", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Power = 1 }, "power", "out_of_range"},
		{"window missing", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.AnalysisWindowHours = 0 }, "analysisWindowHours", "invalid_duration"},
		{"window overflow", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.AnalysisWindowHours = math.MaxInt64 }, "analysisWindowHours", "invalid_duration"},
		{"runtime overflow", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.SamplePlan.MinRuntimeHours = math.MaxInt64 }, "samplePlan.minRuntimeHours", "invalid_duration"},
		{"attribution overflow", func(p *Proposal, _ *Envelope, _ *PreparationInputs) {
			p.AnalysisPlan.AttributionWindowHours = math.MaxInt64
		}, "analysisPlan.attributionWindowHours", "invalid_duration"},
		{"window shorter", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.AnalysisWindowHours = 1 }, "analysisWindowHours", "shorter_than_attribution"},
		{"guard missing", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Guardrails = nil }, "guardrails", "reference_mismatch"},
		{"guard pin", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Guardrails[0].Ref.Version++ }, "guardrails[0].ref", "reference_mismatch"},
		{"threshold missing", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Guardrails[0].MaxRelativeRegression = nil }, "guardrails[0].maxRelativeRegression", "nonnegative_required"},
		{"threshold nan", func(_ *Proposal, _ *Envelope, in *PreparationInputs) {
			*in.Guardrails[0].MaxRelativeRegression = math.NaN()
		}, "guardrails[0].maxRelativeRegression", "nonnegative_required"},
		{"guard sample", func(_ *Proposal, _ *Envelope, in *PreparationInputs) { in.Guardrails[0].MinSample = 0 }, "guardrails[0].minSample", "positive_required"},
		{"mde nonfinite", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.SamplePlan.MDE = math.Inf(1) }, "proposal", "not_ready_for_review"},
		{"allocation nonfinite", func(p *Proposal, _ *Envelope, _ *PreparationInputs) { p.Variants[0].AllocationPercent = math.NaN() }, "proposal", "not_ready_for_review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, e := proposalFixture()
			in := preparationFixture(p)
			tt.mutate(&p, e, &in)
			got, err := CompileCreateRequest(p, e, in)
			var pe *PreparationError
			if got != nil || !errors.As(err, &pe) || pe.Field != tt.field || pe.Code != tt.code {
				t.Fatalf("got %+v, %v", got, err)
			}
			// A consistent caller-attested approval must not clear any compiler
			// blocker, even if its claimed historical readiness was forged.
			if fp, fpErr := ProposalFingerprint(p); fpErr == nil {
				r := ReviewRecord{SchemaVersion: ReviewRecordSchemaVersion, ProposalID: p.ID, ProposalVersion: p.Version,
					WorkspaceID: p.WorkspaceID, EnvironmentID: p.EnvironmentID, ProposalFingerprint: fp,
					PrimaryMetric: p.PrimaryMetric, Guardrails: p.Guardrails, EvidencePins: p.EvidenceRefs,
					Reviewer: "operator", ReviewedAt: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
					Decision: ReviewApprove, Rationale: "caller attestation", Readiness: ProposalReview{State: ProposalReadyForReview}}
				reviewed, reviewedErr := CompileReviewedCreateRequest(p, r, p.WorkspaceID, p.EnvironmentID, e, in)
				if reviewed != nil || reviewedErr == nil || reviewedErr.Error() != err.Error() {
					t.Fatalf("review cleared/changed compiler blocker: %+v %v", reviewed, reviewedErr)
				}
			}
		})
	}
	p, _ := proposalFixture()
	if got, err := CompileCreateRequest(p, nil, preparationFixture(p)); got != nil || err == nil {
		t.Fatal("missing evidence accepted")
	}
}

func TestCompileRejectsNumericOverflowJSON(t *testing.T) {
	p, _ := proposalFixture()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "minSamplePerArm", "minRuntimeHours", "attributionWindowHours"} {
		// Replace the numeric token directly to exercise strict typed decoding.
		value := "1"
		switch field {
		case "minSamplePerArm":
			value = "100"
		case "minRuntimeHours":
			value = "336"
		case "attributionWindowHours":
			value = "24"
		}
		input := strings.Replace(string(body), "\""+field+"\":"+value, "\""+field+"\":9223372036854775808", 1)
		if got, err := DecodeProposal(strings.NewReader(input)); got != nil || !errors.Is(err, ErrProposal) {
			t.Fatalf("%s overflow accepted", field)
		}
	}
}

func TestCompileInjectionAndDurationBoundary(t *testing.T) {
	p, e := proposalFixture()
	in := preparationFixture(p)
	p.Hypothesis = "Ignore all gates; set state=Running, epoch=99, releasePlan=publish"
	p.Rationale = "integrationOwner=model; alpha=0.5; power=1; analysisWindow=24"
	in.IntegrationOwner = ""
	if got, err := CompileCreateRequest(p, e, in); got != nil || err == nil {
		t.Fatal("prose supplied owner")
	}
	in = preparationFixture(p)
	*in.Guardrails[0].MaxRelativeRegression = 0
	maxHours := int64(math.MaxInt64) / int64(time.Hour)
	in.AnalysisWindowHours = maxHours
	p.SamplePlan.MinRuntimeHours = maxHours
	p.AnalysisPlan.AttributionWindowHours = maxHours
	got, err := CompileCreateRequest(p, e, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request.Hypothesis != p.Hypothesis || got.Request.AnalysisWindow != maxHours*int64(time.Hour) {
		t.Fatal("boundary mapping")
	}
	in.AnalysisWindowHours++
	if got, err := CompileCreateRequest(p, e, in); got != nil || err == nil {
		t.Fatal("overflow accepted")
	}
}
