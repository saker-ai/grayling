package growthservice

import (
	"encoding/json/v2"
	"github.com/saker-ai/grayling/internal/growth"
	"path/filepath"
	"testing"
)

func fixtureBaseline(f *fixture) growth.AggregateBaseline {
	w := f.report.Window
	w.Version = "aggregate-baseline-v1"
	w.Backend = "finite-aggregate-v1"
	return growth.AggregateBaseline{SchemaVersion: "growth.aggregate-baseline.v1", SourceID: "synthetic-export", Version: 1, WorkspaceID: f.goal.WorkspaceID, EnvironmentID: f.goal.EnvironmentID, Metrics: []growth.MetricRef{f.goal.PrimaryMetric.Ref, f.goal.Guardrails[0].Ref}, Window: w, RefreshedAt: w.Watermark, Unit: "randomization_unit", License: "Apache-2.0 synthetic fixture", PrivacyPolicyRef: "synthetic-no-personal-data", Synthetic: true, Groups: []growth.BaselineGroup{{Role: "all", Units: 200, Converted: 40}}}
}
func TestFiniteBaselineDesignRecovery(t *testing.T) {
	f := newFixture(t, nil)
	cfg := f.service.cfg
	cfg.BaselineSources = []string{"synthetic-export"}
	f.service.Close()
	var err error
	f.service, err = New(t.Context(), f.store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureBaseline(f)
	body := map[string]any{"goal": f.goal, "baseline": b}
	code, raw := f.call(t, "POST", "/v1/tasks", editSecret, body, "baseline")
	if code != 202 {
		t.Fatalf("%d %s", code, raw)
	}
	task := taskFrom(t, raw)
	f.service.process(t.Context())
	task = f.load(t, task.ID)
	if task.State != "DIAGNOSED" || task.Evidence.PlanningBaseline == nil || len(task.Evidence.Evidence) != 0 {
		t.Fatalf("noncausal baseline did not diagnose: %s", task.State)
	}
	// Offline mode is truthful: the operator supplies suggestions explicitly for this synthetic test.
	task, err = f.store.change(t.Context(), task.ID, task.Revision, "synthetic-fixture", "synthetic_suggestions", func(t *Task) error {
		t.Suggestions = []Suggestion{{Hypothesis: "Shorter introduction reduces friction", Mechanism: "Fewer choices", Falsification: "No change in completion", Risk: "low", Effort: "low"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task = f.act(t, task, "design", Action{Design: &DesignInputs{FlagKey: "onboarding", Unit: "user_id", Audience: "synthetic new users", ExposureTrigger: "visible card", ControlAllocationPercent: 50, Implementation: f.proposal.Implementation, RollbackConditions: f.proposal.RollbackConditions}}, 200)
	task = f.act(t, task, "sample-plan", Action{Sample: &SampleInputs{AbsoluteMDE: .05, Alpha: .05, Power: .8, Direction: "increase", MinRuntimeHours: 336}}, 200)
	if task.Proposal.SamplePlan.MinSamplePerArm < 100 || task.Proposal.BaselinePin == nil {
		t.Fatal("missing deterministic plan or baseline pin")
	}
	task = f.act(t, task, "review", Action{Decision: growth.ReviewApprove, Rationale: "Synthetic acceptance only"}, 200)
	if task.Review == nil || task.Review.BaselinePin == nil {
		t.Fatal("review lost baseline pin")
	}
	threshold := .1
	settings := growth.PreparationInputs{AllocationMode: "fixed-split", IntegrationOwner: "synthetic", Alpha: .1, Power: .8, AnalysisWindowHours: 336, Guardrails: []growth.GuardrailPreparation{{Ref: f.goal.Guardrails[0].Ref, MaxRelativeRegression: &threshold, MinSample: 20}}}
	task = f.act(t, task, "prepare", Action{Settings: &settings}, 422)
	if task.Prepared != nil {
		t.Fatal("calculated alpha changed during preparation")
	}
	settings.Alpha = .05
	task = f.act(t, task, "prepare", Action{Settings: &settings}, 200)
	if task.Prepared.Metadata.BaselinePin == nil {
		t.Fatal("preparation lost source pin")
	}

	backup := filepath.Join(privateDir(t), "backup.sqlite")
	if err = f.store.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	snapshot, err := restored.get(t.Context(), task.ID)
	if err != nil || snapshot.Baseline == nil || snapshot.Proposal.BaselinePin.Fingerprint != task.Proposal.BaselinePin.Fingerprint {
		t.Fatal("restore lost finite source")
	}
	code, _ = f.call(t, "POST", "/v1/tasks", editSecret, body, "baseline")
	if code != 202 {
		t.Fatalf("idempotency %d", code)
	}
	b.Groups[0].Converted++
	body["baseline"] = b
	code, _ = f.call(t, "POST", "/v1/tasks", editSecret, body, "baseline")
	if code != 409 {
		t.Fatal("changed baseline reused key")
	}
}
func TestFiniteBaselineRejectsForgedSourceAndScope(t *testing.T) {
	f := newFixture(t, nil)
	cfg := f.service.cfg
	cfg.BaselineSources = []string{"synthetic-export"}
	f.service.Close()
	var err error
	f.service, err = New(t.Context(), f.store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := fixtureBaseline(f)
	for _, mutate := range []func(*growth.AggregateBaseline){func(b *growth.AggregateBaseline) { b.Causal = true }, func(b *growth.AggregateBaseline) { b.Groups[0].Units = 19 }, func(b *growth.AggregateBaseline) { b.EnvironmentID = "other" }, func(b *growth.AggregateBaseline) { b.Metrics[0].DefinitionHash = hb }, func(b *growth.AggregateBaseline) { b.SourceID = "unapproved" }} {
		c := b
		c.Groups = append([]growth.BaselineGroup(nil), b.Groups...)
		c.Metrics = append([]growth.MetricRef(nil), b.Metrics...)
		mutate(&c)
		status, _ := f.call(t, "POST", "/v1/tasks", editSecret, map[string]any{"goal": f.goal, "baseline": c}, "reject")
		if status < 400 {
			t.Fatal("unsafe aggregate accepted")
		}
	}
	raw, _ := json.Marshal(b)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	decoded["rawUsers"] = []string{"forbidden"}
	status, _ := f.call(t, "POST", "/v1/tasks", editSecret, map[string]any{"goal": f.goal, "baseline": decoded}, "reject")
	if status < 400 {
		t.Fatal("unknown raw field accepted")
	}
}
