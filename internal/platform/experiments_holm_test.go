package platform

import (
	"errors"
	"strings"
	"testing"
)

func TestHolmRegistrationValidationAndVersion(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace("holm", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	metric := MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"}
	makeExp := func(epoch int64) *Experiment {
		e := baseP5Experiment(ws.ID, env.ID, metric)
		e.Epoch = epoch
		e.HolmFamily = &HolmFamily{ID: "primary", Hypotheses: []HolmHypothesis{
			{ID: "conversion", Metric: metric, Control: "control", Treatment: "treatment"},
			{ID: "engagement", Metric: MetricSpec{Key: "engagement", Type: "proportion", Direction: "increase"}, Control: "control", Treatment: "treatment"},
		}}
		return e
	}
	good, err := s.CreateExperiment(makeExp(1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(good.RegistrationVersion, "sha256:") {
		t.Fatalf("version: %q", good.RegistrationVersion)
	}
	version, err := ExperimentRegistrationVersion(good)
	if err != nil || version != good.RegistrationVersion {
		t.Fatalf("version drift: %q %v", version, err)
	}
	bad := makeExp(2)
	bad.HolmFamily.Hypotheses[0].Metric.Key = "engagement"
	if _, err := s.CreateExperiment(bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("primary mismatch accepted: %v", err)
	}
	bad = makeExp(3)
	bad.HolmFamily.Hypotheses[1].ID = "conversion"
	if _, err := s.CreateExperiment(bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate ID accepted: %v", err)
	}
	bad = makeExp(4)
	bad.HolmFamily.Hypotheses[1].Treatment = "unknown"
	if _, err := s.CreateExperiment(bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown arm accepted: %v", err)
	}
	bad = makeExp(5)
	bad.AllocationMode = "bandit"
	if _, err := s.CreateExperiment(bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("bandit Holm accepted: %v", err)
	}
	bad = makeExp(6)
	bad.Variants = append(bad.Variants, "third")
	bad.Proportions = map[string]float64{"control": 40, "treatment": 30, "third": 30}
	if _, err := s.CreateExperiment(bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("uncovered variant accepted: %v", err)
	}
	bad.HolmFamily.Hypotheses = append(bad.HolmFamily.Hypotheses, HolmHypothesis{
		ID: "third", Metric: metric, Control: "control", Treatment: "third"})
	if _, err := s.CreateExperiment(bad); err != nil {
		t.Fatalf("covered variant rejected: %v", err)
	}
}
