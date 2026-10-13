package experiment

import (
	"fmt"
	"testing"
	"time"

	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/model"
)

func TestHolmReportControlsPrimaryDecisionAndVersion(t *testing.T) {
	store, ws, env := setup(t)
	primary := platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"}
	secondary := platform.MetricSpec{Key: "engagement", Type: "proportion", Direction: "increase"}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "holm", Epoch: 1, Unit: "user_id",
		Variants:      []string{"control", "treatment", "third"},
		Proportions:   map[string]float64{"control": 40, "treatment": 30, "third": 30},
		PrimaryMetric: primary, Alpha: .05, AnalysisWindow: time.Hour, MinSamplePerArm: 50,
		HolmFamily: &platform.HolmFamily{ID: "launch", Hypotheses: []platform.HolmHypothesis{
			{ID: "conversion-treatment", Metric: primary, Control: "control", Treatment: "treatment"},
			{ID: "engagement-treatment", Metric: secondary, Control: "control", Treatment: "treatment"},
			{ID: "conversion-third", Metric: primary, Control: "control", Treatment: "third"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.ExperimentState{model.ExperimentReviewed, model.ExperimentRunning} {
		if _, err := store.TransitionExperiment(exp.ID, state, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Minute)
	limits := map[string][2]int{"control": {20, 20}, "treatment": {34, 22}, "third": {35, 20}}
	ns := map[string]int{"control": 100, "treatment": 75, "third": 75}
	ingestHolmBinaryEvents(t, store, ws, env, exp, base, ns, limits)
	rep, err := BuildReport(store, exp.ID, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Holm == nil || rep.Holm.Error != "" || len(rep.Holm.Comparisons) != 3 {
		t.Fatalf("Holm section: %+v", rep.Holm)
	}
	if rep.RegistrationVersion != exp.RegistrationVersion || rep.Holm.RegistrationVersion != exp.RegistrationVersion {
		t.Fatalf("registration version mismatch: report=%s experiment=%s", rep.RegistrationVersion, exp.RegistrationVersion)
	}
	if !rep.Healthy || rep.Decision.Verdict != "continue" || !rep.Primary.Significant ||
		!rep.Holm.Comparisons[0].Rejected || rep.Holm.Comparisons[1].Rejected {
		t.Fatalf("Holm must block ship for the nonsignificant secondary: healthy=%v decision=%+v primary=%+v Holm=%+v",
			rep.Healthy, rep.Decision, rep.Primary, rep.Holm)
	}
	if len(rep.PerVariant) != 3 || rep.SRM == nil || !rep.SRM.Healthy {
		t.Fatalf("multi-arm volumes/SRM: %+v %+v", rep.PerVariant, rep.SRM)
	}
	if rep.PerVariant[2].PrimaryValue <= 0 || rep.PerVariant[2].ConvertedUnits != 35 {
		t.Fatalf("third variant primary value: %+v", rep.PerVariant[2])
	}
	if rep.Holm.Comparisons[0].RawP != rep.Primary.PValue ||
		rep.Holm.Comparisons[0].AdjustedP < rep.Holm.Comparisons[0].RawP {
		t.Fatalf("primary raw/adjusted p: %+v, primary=%+v", rep.Holm.Comparisons[0], rep.Primary)
	}
	if rep.Decision.Verdict == "ship" {
		t.Fatalf("not all registered hypotheses improved significantly: %+v", rep.Decision)
	}
	// A caller's returned object is detached from the persisted contract.
	exp.HolmFamily.Hypotheses = exp.HolmFamily.Hypotheses[:2]
	stored, err := store.GetExperiment(exp.ID)
	if err != nil || len(stored.HolmFamily.Hypotheses) != 3 {
		t.Fatalf("returned pointer changed stored registration: %+v %v", stored, err)
	}
	stored.HolmFamily.Hypotheses[0].ID = "tampered"
	again, err := BuildReport(store, exp.ID, time.Now())
	if err != nil || again.Holm.Comparisons[0].ID != "conversion-treatment" {
		t.Fatalf("GetExperiment pointer changed report contract: %+v %v", again, err)
	}
}

func TestHolmReportRawSignificanceDoesNotShip(t *testing.T) {
	store, ws, env := setup(t)
	primary := platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"}
	secondary := platform.MetricSpec{Key: "engagement", Type: "proportion", Direction: "increase"}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "holm-raw", Epoch: 1, Unit: "user_id",
		Variants: []string{"control", "treatment"}, Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: primary, Alpha: .05, AnalysisWindow: time.Hour, MinSamplePerArm: 100,
		HolmFamily: &platform.HolmFamily{ID: "launch", Hypotheses: []platform.HolmHypothesis{
			{ID: "conversion", Metric: primary, Control: "control", Treatment: "treatment"},
			{ID: "engagement", Metric: secondary, Control: "control", Treatment: "treatment"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.ExperimentState{model.ExperimentReviewed, model.ExperimentRunning} {
		if _, err := store.TransitionExperiment(exp.ID, state, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	ingestHolmBinaryEvents(t, store, ws, env, exp, time.Now().Add(-time.Minute),
		map[string]int{"control": 200, "treatment": 200},
		map[string][2]int{"control": {40, 40}, "treatment": {58, 57}})
	rep, err := BuildReport(store, exp.ID, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy || !rep.Primary.Significant || rep.Decision.Verdict != "continue" || rep.Holm.Error != "" {
		t.Fatalf("raw significance must not ship: healthy=%v primary=%+v decision=%+v Holm=%+v",
			rep.Healthy, rep.Primary, rep.Decision, rep.Holm)
	}
	for _, row := range rep.Holm.Comparisons {
		if row.RawP >= .05 || row.AdjustedP <= .05 || row.Rejected {
			t.Fatalf("want raw-only significance, got %+v", row)
		}
	}
}

func ingestHolmBinaryEvents(t *testing.T, store *platform.Store, ws *platform.Workspace,
	env *platform.Environment, exp *platform.Experiment, base time.Time,
	ns map[string]int, limits map[string][2]int) {
	t.Helper()
	for arm, n := range ns {
		for i := range n {
			unit := store.HashUnit(ws.ID, fmt.Sprintf("%s-%d", arm, i))
			events := []*platform.Event{{EventID: fmt.Sprintf("%s-%d-exp", arm, i), WorkspaceID: ws.ID,
				EnvironmentID: env.ID, ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: exp.FlagKey,
				Variant: arm, UnitHash: unit, Type: platform.EventExposure, Source: "split", ClientTime: base}}
			for j, key := range []string{"conversion", "engagement"} {
				if i < limits[arm][j] {
					events = append(events, &platform.Event{EventID: fmt.Sprintf("%s-%d-%s", arm, i, key),
						WorkspaceID: ws.ID, EnvironmentID: env.ID, ExperimentID: exp.ID, Epoch: exp.Epoch,
						FlagKey: exp.FlagKey, Variant: arm, UnitHash: unit, Type: platform.EventConversion,
						MetricKey: key, Value: 1, ClientTime: base})
				}
			}
			if result, err := store.IngestEvents(events); err != nil || result.Accepted != len(events) {
				t.Fatalf("ingest %s %d: %+v %v", arm, i, result, err)
			}
		}
	}
}
