package admin

import (
	"errors"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestFlagViewReportsScopedExperimentRelationships(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("other"))

	create := func(key string) *platform.Experiment {
		t.Helper()
		exp, err := f.svc.CreateExperiment(f.analyst, f.ws.ID, f.env.ID, &platform.Experiment{
			FlagKey: key, Unit: "user_id", Variants: []string{"on", "off"},
			Proportions:   map[string]float64{"on": 50, "off": 50},
			PrimaryMetric: platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"},
			Alpha:         0.05, Power: 0.8, MDE: 0.02,
			MinSamplePerArm: 10, AnalysisWindow: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		return exp
	}
	for range maxFlagExperimentReferences {
		create("checkout")
	}
	running := create("checkout")
	create("other")
	if _, err := f.store.TransitionExperiment(running.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionExperiment(running.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}

	view, err := f.svc.GetFlagView(f.viewer, f.ws.ID, f.env.ID, "checkout", true)
	if err != nil {
		t.Fatal(err)
	}
	impact := view.ExperimentImpact
	if impact == nil || impact.Total != maxFlagExperimentReferences+1 ||
		len(impact.References) != maxFlagExperimentReferences ||
		impact.References[0].ID != running.ID || impact.References[0].State != model.ExperimentRunning {
		t.Fatalf("wrong bounded, prioritized impact: %+v", impact)
	}
	for _, ref := range impact.References {
		if ref.ID == "" || ref.Epoch <= 0 || ref.FlagRevision != 1 {
			t.Fatalf("incomplete experiment reference: %+v", ref)
		}
	}
	if listView, err := f.svc.GetFlagView(f.viewer, f.ws.ID, f.env.ID, "checkout", false); err != nil ||
		listView.ExperimentImpact != nil {
		t.Fatalf("list view should omit relationship scan: %+v %v", listView, err)
	}
	if _, err := f.svc.GetFlagView(f.otherWS, f.ws.ID, f.env.ID, "checkout", true); !errors.Is(err, platform.ErrAccessDenied) {
		t.Fatalf("cross-workspace relationship read: %v", err)
	}

	changed := testFlag("checkout")
	changed.Disable = true
	if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, changed, platform.RiskHigh, 1); err != nil {
		t.Fatal(err)
	}
	view, err = f.svc.GetFlagView(f.viewer, f.ws.ID, f.env.ID, "checkout", true)
	if err != nil {
		t.Fatal(err)
	}
	if view.Preview == nil || view.Preview.ExperimentImpact == nil ||
		view.Preview.ExperimentImpact.Total != impact.Total {
		t.Fatalf("publication preview omits related experiments: %+v", view.Preview)
	}
}
