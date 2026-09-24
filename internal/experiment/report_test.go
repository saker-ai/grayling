package experiment

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
)

func setup(t *testing.T) (*platform.Store, *platform.Workspace, *platform.Environment) {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ws, err := store.CreateWorkspace("acme", "")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return store, ws, env
}

func newExp(t *testing.T, store *platform.Store, ws *platform.Workspace, env *platform.Environment,
	metricType string, guardrails ...platform.GuardrailSpec) *platform.Experiment {
	t.Helper()
	exp := &platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants:    []string{"control", "treatment"},
		Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{
			Key: "conversion", Type: metricType, Direction: "increase",
		},
		Guardrails:      guardrails,
		Alpha:           0.05,
		Power:           0.8,
		MDE:             0.01,
		MinSamplePerArm: 100,
		MinRuntime:      time.Hour,
		AnalysisWindow:  7 * 24 * time.Hour,
		Hypothesis:      "treatment lifts conversion",
	}
	created, err := store.CreateExperiment(exp)
	if err != nil {
		t.Fatalf("CreateExperiment: %v", err)
	}
	if _, err := store.TransitionExperiment(created.ID, model.ExperimentReviewed, "t"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := store.TransitionExperiment(created.ID, model.ExperimentRunning, "t"); err != nil {
		t.Fatalf("run: %v", err)
	}
	return created
}

// simulate feeds n units per arm; conversion probabilities per arm; mean
// metrics use per-request values with 1-3 requests per unit.
func simulate(t *testing.T, store *platform.Store, ws *platform.Workspace, env *platform.Environment,
	exp *platform.Experiment, nPerArm int, pCtrl, pTreat float64, metricType string, skew float64) {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	base := time.Now().Add(-2 * time.Hour)
	id := 0
	for arm, p := range map[string]float64{"control": pCtrl, "treatment": pTreat} {
		count := nPerArm
		if arm == "treatment" && skew != 0 {
			count = int(float64(nPerArm) * skew)
		}
		for u := 0; u < count; u++ {
			unit := fmt.Sprintf("%s-user-%d", arm, u)
			unitHash := store.HashUnit(ws.ID, unit)
			id++
			exposure := &platform.Event{
				EventID: fmt.Sprintf("exp-%d", id), WorkspaceID: ws.ID, EnvironmentID: env.ID,
				Type: platform.EventExposure, ExperimentID: exp.ID, Epoch: exp.Epoch,
				FlagKey: "f", Variant: arm, UnitHash: unitHash, Source: "split",
				ClientTime: base.Add(time.Duration(u) * time.Millisecond),
			}
			if _, err := store.IngestEvents([]*platform.Event{exposure}); err != nil {
				t.Fatalf("ingest exposure: %v", err)
			}
			if rng.Float64() < p {
				requests := 1
				if metricType == "mean" {
					requests = 1 + rng.Intn(3)
				}
				for r := 0; r < requests; r++ {
					id++
					value := 1.0
					if metricType == "mean" {
						if arm == "control" {
							value = 1.0 + rng.NormFloat64()*0.1
						} else {
							value = 1.2 + rng.NormFloat64()*0.1
						}
					}
					conv := &platform.Event{
						EventID: fmt.Sprintf("conv-%d", id), WorkspaceID: ws.ID, EnvironmentID: env.ID,
						Type: platform.EventConversion, ExperimentID: exp.ID, Epoch: exp.Epoch,
						FlagKey: "f", Variant: arm, UnitHash: unitHash,
						MetricKey: "conversion", Value: value,
						ClientTime: base.Add(time.Duration(u) * time.Millisecond),
					}
					res, err := store.IngestEvents([]*platform.Event{conv})
					if err != nil {
						t.Fatalf("ingest conversion: %v", err)
					}
					if res.Accepted != 1 {
						t.Fatalf("conversion not accepted: %+v", res)
					}
				}
			}
		}
	}
}

func TestReportNoDataHolds(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Healthy {
		t.Fatal("no-data report must be unhealthy")
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("verdict = %s, want hold", rep.Decision.Verdict)
	}
}

func TestReportProportionShip(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	// Strong lift: 10% vs 16% on 2000 units/arm → clearly significant.
	simulate(t, store, ws, env, exp, 2000, 0.10, 0.16, "proportion", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("report unhealthy: %v (primary err: %s)", rep.HealthIssues, rep.PrimaryError)
	}
	if rep.SRM == nil || !rep.SRM.Healthy {
		t.Fatalf("SRM must be healthy for a balanced simulation: %+v", rep.SRM)
	}
	if rep.Primary == nil || !rep.Primary.Significant {
		t.Fatalf("primary must be significant: %+v", rep.Primary)
	}
	if rep.Primary.Diff <= 0 {
		t.Fatalf("diff must be positive: %v", rep.Primary.Diff)
	}
	// The experiment started seconds ago; the 1h min-runtime is NOT yet
	// satisfied, so the verdict must hold despite significance — peeking
	// early never ships (§10.3).
	if rep.MinRuntimeSatisfied {
		t.Fatal("min runtime (1h) cannot be satisfied right after start")
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("verdict = %s, want hold (min runtime)", rep.Decision.Verdict)
	}
}

func TestReportMeanMetricWelch(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "mean")
	exp.AnalysisMethod = "welch"
	exp.MinRuntime = 0
	simulate(t, store, ws, env, exp, 800, 0.9, 0.9, "mean", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Primary == nil || rep.Primary.Type != "welch" {
		t.Fatalf("primary: %+v (err %s)", rep.Primary, rep.PrimaryError)
	}
	if !rep.Primary.Significant || rep.Primary.Diff <= 0 {
		t.Fatalf("mean lift 1.0→1.2 must be significant: %+v", rep.Primary)
	}
	if rep.Decision.Verdict != "ship" {
		t.Fatalf("verdict = %s (%v), want ship", rep.Decision.Verdict, rep.Decision.Reasons)
	}
}

// TestReportSRMBlocksVerdict is the P2 acceptance rule: 异常数据不出胜负结论.
func TestReportSRMBlocksVerdict(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	exp.MinRuntime = 0
	// 60% more treatment units than registered → massive SRM, and the
	// treatment lift is significant regardless. The gate must hold.
	simulate(t, store, ws, env, exp, 2000, 0.10, 0.20, "proportion", 1.6)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.SRM == nil || rep.SRM.Healthy {
		t.Fatalf("SRM must alarm: %+v", rep.SRM)
	}
	if rep.Primary != nil && !rep.Primary.Significant {
		t.Fatal("test setup broken: lift should still be statistically significant")
	}
	if rep.Decision.Verdict == "ship" || rep.Decision.Verdict == "rollback" {
		t.Fatalf("SRM alarm must block win/lose verdicts, got %s", rep.Decision.Verdict)
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("verdict = %s, want hold", rep.Decision.Verdict)
	}
	if rep.Healthy {
		t.Fatal("report must be marked unhealthy")
	}
}

func TestReportGuardrailRollback(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion", platform.GuardrailSpec{
		MetricKey: "conversion", Type: "proportion", Direction: "increase",
		MaxRelativeRegression: 0.02, MinSample: 50,
	})
	exp.MinRuntime = 0
	// Primary metric REGRESSES badly (10% → 5%) → guardrail breach and
	// significant negative primary both point at rollback.
	simulate(t, store, ws, env, exp, 2000, 0.10, 0.05, "proportion", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(rep.Guardrails) != 1 || !rep.Guardrails[0].Breached {
		t.Fatalf("guardrail must breach: %+v", rep.Guardrails)
	}
	if rep.Decision.Verdict != "rollback" {
		t.Fatalf("verdict = %s, want rollback", rep.Decision.Verdict)
	}
}

func TestReportInsufficientSampleHolds(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	exp.MinRuntime = 0
	exp.MinSamplePerArm = 5000 // never reached
	simulate(t, store, ws, env, exp, 300, 0.10, 0.30, "proportion", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("verdict = %s, want hold despite significant p", rep.Decision.Verdict)
	}
}

func TestReportMinRuntimeHolds(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "proportion")
	exp.MinRuntime = 30 * 24 * time.Hour // 30 days — just started
	simulate(t, store, ws, env, exp, 2000, 0.10, 0.20, "proportion", 0)

	rep, err := BuildReport(store, exp.ID, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.MinRuntimeSatisfied {
		t.Fatal("min runtime cannot be satisfied seconds after start")
	}
	if rep.Decision.Verdict != "hold" {
		t.Fatalf("verdict = %s, want hold", rep.Decision.Verdict)
	}
}

func TestEventDedupAndUnitAggregation(t *testing.T) {
	store, ws, env := setup(t)
	exp := newExp(t, store, ws, env, "mean")
	now := time.Now()
	unitHash := store.HashUnit(ws.ID, "u1")
	mk := func(id string, value float64) *platform.Event {
		return &platform.Event{
			EventID: id, WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: platform.EventConversion, ExperimentID: exp.ID, Epoch: exp.Epoch,
			FlagKey: "f", Variant: "control", UnitHash: unitHash,
			MetricKey: "conversion", Value: value, ClientTime: now,
		}
	}
	res1, err := store.IngestEvents([]*platform.Event{mk("c1", 2.0), mk("c2", 4.0)})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res1.Accepted != 2 {
		t.Fatalf("accepted = %d", res1.Accepted)
	}
	// Duplicate batch: idempotent.
	res2, err := store.IngestEvents([]*platform.Event{mk("c1", 2.0), mk("c2", 4.0), mk("c3", 6.0)})
	if err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if res2.Accepted != 1 || res2.Duplicates != 2 {
		t.Fatalf("dedup broken: %+v", res2)
	}
	// Per-unit aggregate: count=3, sum=12 → unit mean 4.
	agg := store.MetricAggregate(ws.ID, env.ID, exp.ID, exp.Epoch, "conversion")
	ua := agg["control"]
	if ua == nil || ua.N != 1 || ua.Count != 3 || ua.Sum != 12 {
		t.Fatalf("aggregate: %+v", ua)
	}

	// Wrong epoch rejected (mixed epochs forbidden §10.1).
	bad := mk("c4", 1)
	bad.Epoch = exp.Epoch + 1
	res3, err := store.IngestEvents([]*platform.Event{bad})
	if err != nil {
		t.Fatalf("ingest 3: %v", err)
	}
	if res3.Accepted != 0 || res3.Rejected != 1 {
		t.Fatalf("epoch mismatch must reject: %+v", res3)
	}
	// Far-past client time rejected.
	old := mk("c5", 1)
	old.ClientTime = now.Add(-30 * 24 * time.Hour)
	res4, _ := store.IngestEvents([]*platform.Event{old})
	if res4.Accepted != 0 {
		t.Fatalf("late beyond window must reject: %+v", res4)
	}
}

func TestReportRejectsNonTwoArm(t *testing.T) {
	store, ws, env := setup(t)
	exp := &platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants:    []string{"a", "b", "c"},
		Proportions: map[string]float64{"a": 34, "b": 33, "c": 33},
		PrimaryMetric: platform.MetricSpec{
			Key: "conversion", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, AnalysisWindow: time.Hour, MinSamplePerArm: 1,
	}
	created, err := store.CreateExperiment(exp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := BuildReport(store, created.ID, time.Now()); err == nil {
		t.Fatal("three-arm report must be rejected in v1")
	}
}
