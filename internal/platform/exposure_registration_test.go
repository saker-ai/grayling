package platform

import (
	"errors"
	"testing"
)

func TestOnRenderExposureIntegrationIsFrozen(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("trigger", "Trigger")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	exp := baseP5Experiment(ws.ID, env.ID,
		MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"})
	exp.ExposureMode = "onRender"
	if _, err := store.CreateExperiment(exp); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing integration contract accepted: %v", err)
	}
	exp.ExposureTrigger = "  after the card is visible  "
	exp.IntegrationOwner = "  growth team  "
	saved, err := store.CreateExperiment(exp)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ExposureTrigger != "after the card is visible" || saved.IntegrationOwner != "growth team" {
		t.Fatalf("integration contract was not normalized: %+v", saved)
	}
	version, err := ExperimentRegistrationVersion(saved)
	if err != nil || version != saved.RegistrationVersion {
		t.Fatalf("registration version mismatch: %s, %v", version, err)
	}
	saved.ExposureTrigger = "after evaluation"
	changed, err := ExperimentRegistrationVersion(saved)
	if err != nil || changed == version {
		t.Fatalf("trigger did not change registration fingerprint: %s, %v", changed, err)
	}
}
