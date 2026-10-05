package platform

import (
	"errors"
	"testing"
	"time"
)

func TestJourneyRegistrationAndIsolation(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	funnel := &FunnelSpec{Steps: []string{"view", "pay"}, DedupUnit: "user", StepTimeout: time.Hour}
	good := baseP5Experiment(ws.ID, env.ID, MetricSpec{Key: "checkout", Type: "funnel", Direction: "increase", Funnel: funnel})
	good.Guardrails = []GuardrailSpec{{MetricKey: "d1", Type: "retention", Direction: "increase", MinSample: 10, MaxRelativeRegression: .1, Retention: &RetentionSpec{ReturnKey: "visit", Day: 1, Timezone: "Asia/Shanghai"}}}
	saved, err := s.CreateExperiment(good)
	if err != nil {
		t.Fatal(err)
	}
	original := saved.RegistrationVersion
	saved.PrimaryMetric.Funnel.Steps[0] = "tampered"
	saved.Guardrails[0].Retention.ReturnKey = "tampered"
	reread, err := s.GetExperiment(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.PrimaryMetric.Funnel.Steps[0] != "view" || reread.Guardrails[0].Retention.ReturnKey != "visit" || reread.RegistrationVersion != original {
		t.Fatalf("registration mutable: %+v", reread)
	}
	bad := []MetricSpec{
		{Key: "x", Type: "funnel", Direction: "increase"},
		{Key: "x", Type: "funnel", Direction: "increase", Funnel: &FunnelSpec{Steps: []string{"a", "a"}, DedupUnit: "user", StepTimeout: time.Hour}},
		{Key: "x", Type: "retention", Direction: "increase", Retention: &RetentionSpec{ReturnKey: "visit", Day: 7, Timezone: "bad/timezone"}},
		{Key: "x", Type: "retention", Direction: "increase", Retention: &RetentionSpec{ReturnKey: "visit", Day: 2, Timezone: "UTC"}},
	}
	for i, metric := range bad {
		exp := baseP5Experiment(ws.ID, env.ID, metric)
		exp.Epoch = int64(i + 2)
		if _, err := s.CreateExperiment(exp); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid %d: %v", i, err)
		}
	}
}
