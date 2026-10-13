package platform

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/saker-ai/grayling/test/pgtest"
)

func TestCollectionCountsSurviveRestartAndRespectScope(t *testing.T) {
	backends := []struct {
		name string
		open func(*testing.T, string) (*Store, error)
	}{
		{"json", func(_ *testing.T, path string) (*Store, error) { return NewStore(path) }},
		{"sqlite", func(t *testing.T, path string) (*Store, error) {
			return NewSQLiteStore(t.Context(), path)
		}},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			store, err := backend.open(t, path)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := store.CreateWorkspace("diagnostics", "Diagnostics")
			if err != nil {
				t.Fatal(err)
			}
			env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
			if err != nil {
				t.Fatal(err)
			}
			exp, err := store.CreateExperiment(&Experiment{
				WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
				Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
				PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
				Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			outcomes := []CollectionOutcome{
				{ExperimentID: exp.ID, Status: "rejected", Code: "stale_epoch"},
				{ExperimentID: exp.ID, Status: "rejected", Code: "stale_epoch"},
				{ExperimentID: exp.ID, Status: "duplicate", Code: "duplicate_event"},
				{ExperimentID: exp.ID, Status: "accepted"},
				{ExperimentID: "other", Status: "rejected", Code: "stale_epoch"},
				{ExperimentID: exp.ID, Status: "rejected", Code: "arbitrary-client-text"},
			}
			if err := store.RecordCollectionOutcomes(ws.ID, env.ID, outcomes); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordCollectionOutcomes("foreign-workspace", env.ID, outcomes); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := backend.open(t, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			counts, err := reopened.CollectionCounts(exp.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(counts) != 2 || counts["stale_epoch"] != 2 || counts["duplicate_event"] != 1 {
				t.Fatalf("unexpected durable counts: %+v", counts)
			}
		})
	}
}

func TestPostgresCollectionCountsSurviveRestart(t *testing.T) {
	backends := []struct {
		name string
		open func(*testing.T, string) (*Store, error)
	}{
		{"gorm", func(t *testing.T, dsn string) (*Store, error) {
			return NewGORMPostgresStore(t.Context(), dsn)
		}},
		{"native", func(t *testing.T, dsn string) (*Store, error) {
			return newLegacyPGStore(t.Context(), dsn)
		}},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			store, err := backend.open(t, dsn)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := store.CreateWorkspace("diagnostics", "Diagnostics")
			if err != nil {
				t.Fatal(err)
			}
			env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
			if err != nil {
				t.Fatal(err)
			}
			exp, err := store.CreateExperiment(&Experiment{
				WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
				Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
				PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
				Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			outcomes := []CollectionOutcome{{ExperimentID: exp.ID, Status: "rejected", Code: "stale_epoch"}}
			if err := store.RecordCollectionOutcomes(ws.ID, env.ID, outcomes); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordCollectionOutcomes(ws.ID, env.ID, outcomes); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := backend.open(t, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			counts, err := reopened.CollectionCounts(exp.ID)
			if err != nil {
				t.Fatal(err)
			}
			if counts["stale_epoch"] != 2 {
				t.Fatalf("durable count: %+v", counts)
			}
		})
	}
}
