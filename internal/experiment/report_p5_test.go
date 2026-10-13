package experiment

import (
	"fmt"
	mrand "math/rand/v2"
	"testing"
	"time"

	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/model"
)

// newExpP5 pre-registers an experiment with an arbitrary MetricSpec (ratio,
// quantile, or a mean with a CUPED covariate) directly through the platform
// store, exercising the §10.3 requirement that the new metric types are
// pre-registrable. MinRuntime=0 and MinSamplePerArm=50 so a healthy,
// well-powered simulation reaches a verdict.
func newExpP5(t *testing.T, store *platform.Store, ws *platform.Workspace, env *platform.Environment,
	metric platform.MetricSpec, guardrails ...platform.GuardrailSpec) *platform.Experiment {
	t.Helper()
	exp := &platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants:      []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: metric,
		Guardrails:    guardrails,
		Alpha:         0.05, Power: 0.8, MDE: 0.05,
		MinSamplePerArm: 50, MinRuntime: 0,
		AnalysisWindow: 7 * 24 * time.Hour,
		Hypothesis:     "p5 metric test",
	}
	created, err := store.CreateExperiment(exp)
	if err != nil {
		t.Fatalf("CreateExperiment(%s): %v", metric.Type, err)
	}
	if _, err := store.TransitionExperiment(created.ID, model.ExperimentReviewed, "t"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := store.TransitionExperiment(created.ID, model.ExperimentRunning, "t"); err != nil {
		t.Fatalf("run: %v", err)
	}
	return created
}

// p5Ingester records split exposures + per-metric conversion events for units
// with unique, deterministic event IDs.
type p5Ingester struct {
	store *platform.Store
	ws    *platform.Workspace
	env   *platform.Environment
	exp   *platform.Experiment
	base  time.Time
	n     int
}

func (ing *p5Ingester) record(arm, unit string, metrics map[string][]float64) {
	ing.n++
	unitHash := ing.store.HashUnit(ing.ws.ID, unit)
	exposure := &platform.Event{
		EventID: fmt.Sprintf("e-%d", ing.n), WorkspaceID: ing.ws.ID, EnvironmentID: ing.env.ID,
		Type: platform.EventExposure, ExperimentID: ing.exp.ID, Epoch: ing.exp.Epoch,
		FlagKey: "f", Variant: arm, UnitHash: unitHash, Source: "split", ClientTime: ing.base,
	}
	if _, err := ing.store.IngestEvents([]*platform.Event{exposure}); err != nil {
		panic(err)
	}
	// Deterministic metric-key order for reproducible event IDs.
	for _, mk := range sortedKeys(metrics) {
		for _, v := range metrics[mk] {
			ing.n++
			conv := &platform.Event{
				EventID: fmt.Sprintf("c-%d", ing.n), WorkspaceID: ing.ws.ID, EnvironmentID: ing.env.ID,
				Type: platform.EventConversion, ExperimentID: ing.exp.ID, Epoch: ing.exp.Epoch,
				FlagKey: "f", Variant: arm, UnitHash: unitHash, MetricKey: mk, Value: v, ClientTime: ing.base,
			}
			if _, err := ing.store.IngestEvents([]*platform.Event{conv}); err != nil {
				panic(err)
			}
		}
	}
}

func sortedKeys(m map[string][]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// tiny insertion sort (deterministic order)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestReportRatioMetricShips(t *testing.T) {
	store, ws, env := setup(t)
	metric := platform.MetricSpec{
		Key: "revenue", Type: "ratio", Direction: "increase",
		NumeratorKey: "revenue", DenominatorKey: "sessions",
	}
	exp := newExpP5(t, store, ws, env, metric)
	ing := &p5Ingester{store: store, ws: ws, env: env, exp: exp, base: time.Now().Add(-2 * time.Hour)}
	rng := mrand.New(mrand.NewPCG(11, 11))
	// control revenue/session ≈ 10/5 = 2.0; treatment ≈ 13/5 = 2.6.
	for _, arm := range []string{"control", "treatment"} {
		revMu := 10.0
		if arm == "treatment" {
			revMu = 13.0
		}
		for u := 0; u < 400; u++ {
			rev := revMu + rng.NormFloat64()
			ses := 5 + 0.3*rng.NormFloat64()
			if ses < 0.5 {
				ses = 0.5
			}
			ing.record(arm, fmt.Sprintf("%s-u-%d", arm, u),
				map[string][]float64{"revenue": {rev}, "sessions": {ses}})
		}
	}
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "ratio-delta" {
		t.Fatalf("primary = %+v (err %s), want ratio-delta", rep.Primary, rep.PrimaryError)
	}
	if !rep.Primary.Significant || rep.Primary.Diff <= 0 {
		t.Fatalf("ratio lift must be significant positive: %+v", rep.Primary)
	}
	if rep.Primary.Ratio == nil || rep.Primary.Ratio.RatioA <= 0 {
		t.Fatalf("ratio detail missing: %+v", rep.Primary.Ratio)
	}
	if rep.Decision.Verdict != "ship" {
		t.Fatalf("verdict = %s (%v), want ship", rep.Decision.Verdict, rep.Decision.Reasons)
	}
}

func TestReportQuantileMetricShips(t *testing.T) {
	store, ws, env := setup(t)
	metric := platform.MetricSpec{
		Key: "latency", Type: "quantile", Direction: "decrease", QuantileLevel: 0.5,
	}
	exp := newExpP5(t, store, ws, env, metric)
	ing := &p5Ingester{store: store, ws: ws, env: env, exp: exp, base: time.Now().Add(-2 * time.Hour)}
	rng := mrand.New(mrand.NewPCG(12, 12))
	// control median ≈ 100, treatment ≈ 80; lower is better (direction=decrease).
	for _, arm := range []string{"control", "treatment"} {
		mu := 100.0
		if arm == "treatment" {
			mu = 80.0
		}
		for u := 0; u < 400; u++ {
			vals := []float64{mu + 15*rng.NormFloat64(), mu + 15*rng.NormFloat64()}
			ing.record(arm, fmt.Sprintf("%s-u-%d", arm, u), map[string][]float64{"latency": vals})
		}
	}
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "quantile" {
		t.Fatalf("primary = %+v (err %s), want quantile", rep.Primary, rep.PrimaryError)
	}
	if !rep.Primary.Significant {
		t.Fatalf("median 100→80 must be significant: %+v", rep.Primary)
	}
	// Raw diff is negative (treatment lower); direction=decrease flips it to an
	// improvement, so the gate ships.
	if rep.Primary.Diff >= 0 {
		t.Fatalf("quantile diff = %v, want negative (treatment lower)", rep.Primary.Diff)
	}
	if rep.Decision.Verdict != "ship" {
		t.Fatalf("verdict = %s (%v), want ship", rep.Decision.Verdict, rep.Decision.Reasons)
	}
}

func TestReportCUPEDReducesVariance(t *testing.T) {
	store, ws, env := setup(t)
	metric := platform.MetricSpec{
		Key: "outcome", Type: "mean", Direction: "increase", CovariateKey: "preperiod",
	}
	exp := newExpP5(t, store, ws, env, metric)
	ing := &p5Ingester{store: store, ws: ws, env: env, exp: exp, base: time.Now().Add(-2 * time.Hour)}
	rng := mrand.New(mrand.NewPCG(13, 13))
	// Covariate x ~ N(0,1); outcome = base + 0.9x + N(0,1) → corr ≈ 0.67,
	// ρ² ≈ 0.45, so CUPED should cut variance substantially. Treatment +0.4.
	for _, arm := range []string{"control", "treatment"} {
		base := 10.0
		if arm == "treatment" {
			base = 10.4
		}
		for u := 0; u < 400; u++ {
			x := rng.NormFloat64()
			y := base + 0.9*x + rng.NormFloat64()
			ing.record(arm, fmt.Sprintf("%s-u-%d", arm, u),
				map[string][]float64{"preperiod": {x}, "outcome": {y}})
		}
	}
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "cuped" {
		t.Fatalf("primary = %+v (err %s), want cuped", rep.Primary, rep.PrimaryError)
	}
	if rep.Primary.CUPED == nil {
		t.Fatal("CUPED detail missing")
	}
	if rep.Primary.VarianceReduction < 0.2 {
		t.Fatalf("CUPED variance reduction = %v, want > 0.2 (ρ²≈0.45)", rep.Primary.VarianceReduction)
	}
	if rep.Primary.Diff <= 0 {
		t.Fatalf("treatment lift must be positive: %v", rep.Primary.Diff)
	}
	t.Logf("CUPED report: varianceReduction=%.3f diff=%.4f p=%.3g verdict=%s",
		rep.Primary.VarianceReduction, rep.Primary.Diff, rep.Primary.PValue, rep.Decision.Verdict)
}

// TestReportSRMBlocksRatioMetric proves the SRM validity gate blocks a winner
// for the NEW ratio metric type exactly as it does for mean/proportion
// (docs/技术方案.md §10.3: SRM 阻断对新方法同样生效).
func TestReportSRMBlocksRatioMetric(t *testing.T) {
	store, ws, env := setup(t)
	metric := platform.MetricSpec{
		Key: "revenue", Type: "ratio", Direction: "increase",
		NumeratorKey: "revenue", DenominatorKey: "sessions",
	}
	exp := newExpP5(t, store, ws, env, metric)
	ing := &p5Ingester{store: store, ws: ws, env: env, exp: exp, base: time.Now().Add(-2 * time.Hour)}
	rng := mrand.New(mrand.NewPCG(14, 14))
	// Skewed enrollment (400 vs 640) → SRM alarm; ratio lift is significant
	// regardless, so the gate MUST hold.
	for _, arm := range []string{"control", "treatment"} {
		count := 400
		revMu := 10.0
		if arm == "treatment" {
			count = 640
			revMu = 14.0
		}
		for u := 0; u < count; u++ {
			rev := revMu + rng.NormFloat64()
			ses := 5 + 0.3*rng.NormFloat64()
			if ses < 0.5 {
				ses = 0.5
			}
			ing.record(arm, fmt.Sprintf("%s-u-%d", arm, u),
				map[string][]float64{"revenue": {rev}, "sessions": {ses}})
		}
	}
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.SRM == nil || rep.SRM.Healthy {
		t.Fatalf("SRM must alarm: %+v", rep.SRM)
	}
	if rep.Primary != nil && !rep.Primary.Significant {
		t.Fatal("setup broken: ratio lift should be significant")
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("SRM must block ratio verdict → hold, got %s", rep.Decision.Verdict)
	}
	if rep.Healthy {
		t.Fatal("report must be unhealthy under SRM")
	}
}

// TestReportGuardrailBlocksQuantileMetric proves the guardrail gate rolls back
// for the NEW quantile primary metric type (docs/技术方案.md §10.3: 护栏阻断对新
// 方法同样生效).
func TestReportGuardrailBlocksQuantileMetric(t *testing.T) {
	store, ws, env := setup(t)
	metric := platform.MetricSpec{
		Key: "latency", Type: "quantile", Direction: "decrease", QuantileLevel: 0.5,
	}
	guard := platform.GuardrailSpec{
		MetricKey: "errors", Type: "proportion", Direction: "decrease",
		MaxRelativeRegression: 0.02, MinSample: 50,
	}
	exp := newExpP5(t, store, ws, env, metric, guard)
	ing := &p5Ingester{store: store, ws: ws, env: env, exp: exp, base: time.Now().Add(-2 * time.Hour)}
	rng := mrand.New(mrand.NewPCG(15, 15))
	// Latency similar in both arms (primary not decisive); treatment error rate
	// jumps 10% → 50% (worse for direction=decrease) → guardrail breach.
	for _, arm := range []string{"control", "treatment"} {
		errRate := 0.10
		if arm == "treatment" {
			errRate = 0.50
		}
		for u := 0; u < 400; u++ {
			m := map[string][]float64{"latency": {100 + 15*rng.NormFloat64()}}
			if rng.Float64() < errRate {
				m["errors"] = []float64{1}
			}
			ing.record(arm, fmt.Sprintf("%s-u-%d", arm, u), m)
		}
	}
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "quantile" {
		t.Fatalf("primary = %+v, want quantile", rep.Primary)
	}
	if len(rep.Guardrails) != 1 || !rep.Guardrails[0].Breached {
		t.Fatalf("errors guardrail must breach: %+v", rep.Guardrails)
	}
	if rep.Decision.Verdict != "rollback" {
		t.Fatalf("guardrail breach must roll back quantile experiment, got %s (%v)",
			rep.Decision.Verdict, rep.Decision.Reasons)
	}
}

// TestReportMeanHasSeparateBayesianSequential verifies the Bayesian and
// sequential views are populated for a mean metric and kept structurally
// SEPARATE from the frequentist Primary/Decision (docs/技术方案.md §10.3).
func TestReportMeanHasSeparateBayesianSequential(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "mean")
	exp.MDE = 0.2 // sequential mixing scale
	exp.MinRuntime = 0
	simulate(t, store, ws, env, exp, 800, 0.9, 0.9, "mean", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "welch" {
		t.Fatalf("frequentist primary must remain Welch: %+v", rep.Primary)
	}
	if rep.Sequential == nil {
		t.Fatal("sequential (always-valid) view must be populated for a mean metric")
	}
	if rep.Bayesian == nil || rep.Bayesian.Mean == nil {
		t.Fatalf("bayesian mean view must be populated: %+v", rep.Bayesian)
	}
	// Separation: the frequentist decision is what ships; the Bayesian view is
	// reported alongside, not merged into the verdict.
	if rep.Decision.Verdict != "ship" {
		t.Fatalf("mean lift 1.0→1.2 should ship on the frequentist gate, got %s", rep.Decision.Verdict)
	}
	bp := rep.Bayesian.Mean.ProbBSuperior
	if bp < 0 || bp > 1 {
		t.Fatalf("bayesian P(B>A) out of range: %v", bp)
	}
	if bp <= 0.9 {
		t.Fatalf("clear mean lift should give high P(B>A), got %v", bp)
	}
	if rep.Bayesian.Proportion != nil {
		t.Fatal("mean metric must not populate the proportion bayesian view")
	}
	t.Logf("mean report: frequentist verdict=%s p=%.3g | bayesian P(B>A)=%.4f expLossB=%.4g | sequential p=%.3g sig=%v",
		rep.Decision.Verdict, rep.Primary.PValue, bp, rep.Bayesian.Mean.ExpectedLossB,
		rep.Sequential.PValue, rep.Sequential.Significant)
}

// TestReportProportionHasBayesianView verifies the proportion family populates
// the Bayesian proportion view (separate from the frequentist z-test).
func TestReportProportionHasBayesianView(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	exp.MinRuntime = 0
	simulate(t, store, ws, env, exp, 2000, 0.10, 0.16, "proportion", 0)
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "two-proportion-z" {
		t.Fatalf("primary = %+v, want two-proportion-z", rep.Primary)
	}
	if rep.Bayesian == nil || rep.Bayesian.Proportion == nil {
		t.Fatalf("bayesian proportion view must be populated: %+v", rep.Bayesian)
	}
	if rep.Bayesian.Proportion.ProbBSuperior < 0.9 {
		t.Fatalf("10%%→16%% lift should give high P(B>A), got %v", rep.Bayesian.Proportion.ProbBSuperior)
	}
}
