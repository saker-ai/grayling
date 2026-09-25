package platform

import (
	"errors"
	"testing"
	"time"
)

// baseP5Experiment returns a minimal valid two-arm experiment whose
// PrimaryMetric the caller overrides, for pre-registration validation tests.
func baseP5Experiment(ws, env string, metric MetricSpec) *Experiment {
	return &Experiment{
		WorkspaceID: ws, EnvironmentID: env, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: metric,
		Alpha:         0.05, AnalysisWindow: time.Hour, MinSamplePerArm: 1,
	}
}

// TestP5MetricTypesPreRegistrable proves the new §10.3 metric types (ratio,
// quantile, CUPED-adjusted mean) can be pre-registered, and that the
// type-specific invariants are enforced.
func TestP5MetricTypesPreRegistrable(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")

	good := []struct {
		name   string
		metric MetricSpec
	}{
		{"ratio", MetricSpec{Key: "rev", Type: "ratio", Direction: "increase", NumeratorKey: "rev", DenominatorKey: "sess"}},
		{"ratio key-as-numerator", MetricSpec{Key: "rev", Type: "ratio", Direction: "increase", DenominatorKey: "sess"}},
		{"quantile", MetricSpec{Key: "lat", Type: "quantile", Direction: "decrease", QuantileLevel: 0.95}},
		{"mean with cuped covariate", MetricSpec{Key: "out", Type: "mean", Direction: "increase", CovariateKey: "pre"}},
	}
	for i, tc := range good {
		exp := baseP5Experiment(ws.ID, env.ID, tc.metric)
		exp.Epoch = int64(100 + i) // unique epoch per flag
		if _, err := s.CreateExperiment(exp); err != nil {
			t.Fatalf("%s: expected pre-registration to succeed, got %v", tc.name, err)
		}
	}

	bad := []struct {
		name   string
		metric MetricSpec
	}{
		{"ratio without denominator", MetricSpec{Key: "rev", Type: "ratio", Direction: "increase", NumeratorKey: "rev"}},
		{"ratio without numerator or key", MetricSpec{Type: "ratio", Direction: "increase", DenominatorKey: "sess"}},
		{"quantile level 0", MetricSpec{Key: "lat", Type: "quantile", Direction: "decrease", QuantileLevel: 0}},
		{"quantile level 1", MetricSpec{Key: "lat", Type: "quantile", Direction: "decrease", QuantileLevel: 1}},
		{"quantile level unset", MetricSpec{Key: "lat", Type: "quantile", Direction: "decrease"}},
		{"unknown type", MetricSpec{Key: "x", Type: "funnel", Direction: "increase"}},
		{"covariate on non-mean", MetricSpec{Key: "lat", Type: "quantile", Direction: "decrease", QuantileLevel: 0.5, CovariateKey: "pre"}},
	}
	for i, tc := range bad {
		exp := baseP5Experiment(ws.ID, env.ID, tc.metric)
		exp.Epoch = int64(500 + i)
		_, err := s.CreateExperiment(exp)
		if err == nil || !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: expected ErrValidation, got %v", tc.name, err)
		}
	}
}

// TestP5AnalysisMethodWhitelist checks the pre-registered analysis-method
// whitelist now admits the P5 methods and still rejects unknown ones.
func TestP5AnalysisMethodWhitelist(t *testing.T) {
	s, _ := NewStore("")
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")

	allowed := []string{"welch", "two-proportion-z", "ratio-delta", "quantile", "cuped"}
	for i, m := range allowed {
		exp := baseP5Experiment(ws.ID, env.ID, MetricSpec{Key: "m", Type: "mean", Direction: "increase"})
		exp.AnalysisMethod = m
		exp.Epoch = int64(900 + i)
		if _, err := s.CreateExperiment(exp); err != nil {
			t.Fatalf("analysis method %q should be allowed: %v", m, err)
		}
	}
	exp := baseP5Experiment(ws.ID, env.ID, MetricSpec{Key: "m", Type: "mean", Direction: "increase"})
	exp.AnalysisMethod = "bootstrap-magic"
	exp.Epoch = 999
	if _, err := s.CreateExperiment(exp); err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown analysis method must be rejected, got %v", err)
	}
}
