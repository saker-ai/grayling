package experiment

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestReportReplaysFrozenMetricWindowsAfterLocalRestart(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T, string) (*platform.Store, error)
	}{
		{"json", func(_ *testing.T, path string) (*platform.Store, error) { return platform.NewStore(path) }},
		{"sqlite", func(t *testing.T, path string) (*platform.Store, error) {
			return platform.NewSQLiteStore(t.Context(), path)
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			testFrozenMetricWindowReplay(t, filepath.Join(t.TempDir(), "metric-windows"), backend.open)
		})
	}
}

func TestReportReplaysFrozenMetricWindowsAfterPostgresRestart(t *testing.T) {
	testFrozenMetricWindowReplay(t, pgtest.DSN(t), func(t *testing.T, dsn string) (*platform.Store, error) {
		return platform.NewGORMPostgresStore(t.Context(), dsn)
	})
}

func testFrozenMetricWindowReplay(t *testing.T, path string,
	open func(*testing.T, string) (*platform.Store, error)) {
	t.Helper()
	store, err := open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metric-windows", "Metric windows")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	metric := func(key, event string, window time.Duration) platform.MetricRef {
		t.Helper()
		def, err := store.CreateMetricDefinition(&platform.MetricDefinition{
			WorkspaceID: ws.ID, Key: key, DisplayName: key, Owner: "growth", Source: "collect",
			EventKeys: []string{event}, DedupUnit: "user", AttributionWindow: window,
			Spec: platform.MetricSpec{Type: "proportion", Direction: "increase"},
		})
		if err != nil {
			t.Fatal(err)
		}
		ref := platform.MetricRef{Key: key, Version: def.Version}
		if _, err := store.PublishMetricDefinition(ws.ID, ref, "reviewer"); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	primary := metric("activation", "activation", 30*time.Second)
	guard := metric("safety", "safety", 2*time.Minute)
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
		Variants:         []string{"control", "treatment"},
		Proportions:      map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetricRef: &primary,
		Guardrails:       []platform.GuardrailSpec{{MetricRef: &guard, MinSample: 1, MaxRelativeRegression: 0.1}},
		Alpha:            0.05, AnalysisWindow: 7 * 24 * time.Hour, MinSamplePerArm: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.ExperimentState{model.ExperimentReviewed, model.ExperimentRunning} {
		if _, err := store.TransitionExperiment(exp.ID, state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(time.Second)
	makeEvent := func(id, typ, variant, unit, key string, at time.Time) *platform.Event {
		return &platform.Event{EventID: id, WorkspaceID: ws.ID, EnvironmentID: env.ID,
			ExperimentID: exp.ID, Epoch: exp.Epoch, Type: platform.EventType(typ),
			FlagKey: exp.FlagKey, Variant: variant, UnitHash: store.HashUnit(ws.ID, unit),
			Source: "split", MetricKey: key, Value: 1, ClientTime: at}
	}
	events := []*platform.Event{
		makeEvent("exposure-control", "exposure", "control", "control-user", "", base),
		makeEvent("exposure-treatment", "exposure", "treatment", "treatment-user", "", base),
		makeEvent("primary-control", "conversion", "control", "control-user", "activation", base.Add(10*time.Second)),
		makeEvent("primary-treatment", "conversion", "treatment", "treatment-user", "activation", base.Add(30*time.Second)),
		makeEvent("guard-control", "conversion", "control", "control-user", "safety", base.Add(70*time.Second)),
		makeEvent("guard-treatment", "conversion", "treatment", "treatment-user", "safety", base.Add(70*time.Second)),
	}
	if got, err := store.IngestEvents(events); err != nil || got.Accepted != len(events) {
		t.Fatalf("ingest: %+v, %v", got, err)
	}
	reportAt := base.Add(3 * time.Minute)
	check := func(s *platform.Store) {
		t.Helper()
		report, err := BuildReport(s, exp.ID, reportAt)
		if err != nil {
			t.Fatal(err)
		}
		if report.PrimaryAttributionWindow != 30*time.Second ||
			report.PerVariant[0].ConvertedUnits != 1 || report.PerVariant[1].ConvertedUnits != 0 {
			t.Fatalf("primary attribution: %+v", report)
		}
		if len(report.Guardrails) != 1 || !strings.Contains(report.Guardrails[0].Detail, "within") {
			t.Fatalf("guardrail lost longer attribution window: %+v", report.Guardrails)
		}
	}
	check(store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	check(reopened)
}
