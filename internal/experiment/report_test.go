package experiment

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestReportCarriesFrozenMetricReference(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metrics", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	def, err := store.CreateMetricDefinition(&platform.MetricDefinition{
		WorkspaceID: ws.ID, Key: "activation.complete", DisplayName: "Activation completion",
		Owner: "growth", Source: "collect", EventKeys: []string{"activation_complete"},
		DedupUnit: "user", AttributionWindow: 7 * 24 * time.Hour,
		Spec: platform.MetricSpec{Type: "proportion", Direction: "increase"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := platform.MetricRef{Key: def.Key, Version: def.Version}
	if _, err := store.PublishMetricDefinition(ws.ID, ref, "reviewer"); err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants: []string{"control", "treatment"}, Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetricRef: &ref, Alpha: 0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := BuildReport(store, exp.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.PrimaryMetricRef == nil || report.PrimaryMetricRef.DefinitionHash != def.Hash ||
		report.RegistrationVersion != exp.RegistrationVersion {
		t.Fatalf("report lost frozen reference: %+v", report)
	}
}

func TestPostgresReportWindowRecomputesAfterReload(t *testing.T) {
	dsn := pgtest.DSN(t)
	store, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ws, err := store.CreateWorkspace("window", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1, Unit: "user_id",
		Variants: []string{"control", "treatment"}, Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(exp.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	start := *exp.StartedAt
	base := platform.Event{WorkspaceID: ws.ID, EnvironmentID: env.ID, ExperimentID: exp.ID, Epoch: 1, FlagKey: "f"}
	makeEvent := func(id string, typ platform.EventType, arm, unit string, at time.Time) *platform.Event {
		ev := base
		ev.EventID, ev.Type, ev.Variant, ev.UnitHash, ev.ClientTime = id, typ, arm, unit, at
		if typ == platform.EventExposure {
			ev.Source = "split"
		} else {
			ev.MetricKey, ev.Value = "conversion", 1
		}
		return &ev
	}
	events := []*platform.Event{
		makeEvent("before", platform.EventExposure, "control", "outside-before", start.Add(-time.Nanosecond)),
		makeEvent("targeted", platform.EventExposure, "control", "u1", start.Add(100*time.Microsecond)),
		makeEvent("pre-conv", platform.EventConversion, "control", "u1", start.Add(time.Millisecond)),
		makeEvent("in-a", platform.EventExposure, "control", "u1", start.Add(2*time.Millisecond)),
		makeEvent("in-b", platform.EventExposure, "treatment", "u2", start),
		makeEvent("valid", platform.EventConversion, "control", "u1", start.Add(3*time.Millisecond)),
		makeEvent("at-end", platform.EventExposure, "treatment", "outside-end", start.Add(time.Second)),
		makeEvent("conv-end", platform.EventConversion, "treatment", "u2", start.Add(time.Second)),
	}
	events[1].Source = "targeting"
	res, err := store.IngestEvents(events)
	if err != nil || res.Accepted != len(events) {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	res, err = store.IngestEvents([]*platform.Event{events[5]})
	if err != nil || res.Duplicates != 1 || res.Accepted != 0 {
		t.Fatalf("duplicate: %+v %v", res, err)
	}
	late := makeEvent("late", platform.EventExposure, "control", "late-unit", start.Add(-73*time.Hour))
	wrongEpoch := makeEvent("wrong-epoch", platform.EventExposure, "control", "other-epoch", start)
	wrongEpoch.Epoch = 2
	res, err = store.IngestEvents([]*platform.Event{late, wrongEpoch})
	if err != nil || res.Rejected != 2 || res.Accepted != 0 {
		t.Fatalf("late and cross-epoch events: %+v %v", res, err)
	}
	clock := start.Add(time.Second + 72*time.Hour)
	rep, err := BuildReport(store, exp.ID, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Window.Complete || rep.Window.Version != "utc-exposure-v1" || rep.Window.Backend != "postgresql-raw" {
		t.Fatalf("window: %+v", rep.Window)
	}
	if rep.PerVariant[0].RandomizedUnits != 1 || rep.PerVariant[1].RandomizedUnits != 1 ||
		rep.PerVariant[0].ConvertedUnits != 1 || rep.PerVariant[1].ConvertedUnits != 0 ||
		rep.PerVariant[0].Exposures != 2 || rep.PerVariant[1].Exposures != 1 {
		t.Fatalf("windowed arms: %+v", rep.PerVariant)
	}
	if rep.SRM == nil || !rep.SRM.Healthy {
		t.Fatalf("SRM: %+v", rep.SRM)
	}
	pending, err := BuildReport(store, exp.ID, start.Add(time.Second+71*time.Hour))
	if err != nil || pending.Window.Complete || pending.Healthy || pending.Decision.Verdict != "hold" {
		t.Fatalf("pending watermark: %+v %v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	again, err := BuildReport(reloaded, exp.ID, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep, again) {
		t.Fatalf("recomputed report differs after reload: before=%+v after=%+v", rep, again)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `UPDATE database_metrics
		SET payload=convert_to(jsonb_set(convert_from(payload, 'UTF8')::jsonb,
		'{count}', '999'::jsonb)::text, 'UTF8')
		WHERE category='agg' AND key LIKE '%/' || $1 || '/%'`, exp.ID); err != nil {
		t.Fatal(err)
	}
	drift, err := BuildReport(reloaded, exp.ID, clock)
	if err != nil || drift.Window.Complete || drift.Healthy || drift.Decision.Verdict != "hold" {
		t.Fatalf("aggregate drift must hold: %+v %v", drift, err)
	}
}

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
