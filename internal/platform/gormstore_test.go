package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saker-ai/grayling/test/pgtest"
)

func TestSQLiteStoreRoundtripAndDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platform.sqlite")
	store, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("sqlite", "SQLite")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "test", "Test")
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
	ev := &Event{EventID: "sqlite-event", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1, Variant: "a",
		UnitHash: store.HashUnit(ws.ID, "unit"), Source: "split", ClientTime: time.Now()}
	if got, err := store.IngestEvents([]*Event{ev}); err != nil || got.Accepted != 1 {
		t.Fatalf("ingest: %+v, %v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("SQLite file permissions: %v, %v", info, err)
	}
	reopened, err := NewSQLiteStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.IngestEvents([]*Event{ev}); err != nil || got.Duplicates != 1 {
		t.Fatalf("dedup after reopen: %+v, %v", got, err)
	}
}

func TestSQLiteMemoryStore(t *testing.T) {
	store, err := NewSQLiteStore(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var databases []struct {
		File string
	}
	if err := store.persist.(*gormPersistence).db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		t.Fatal(err)
	}
	if len(databases) == 0 || databases[0].File != "" {
		t.Fatalf("expected an in-memory database: %+v", databases)
	}
}

func TestSQLiteEventSyncDoesNotReloadWorkingSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	a, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ws, err := a.CreateWorkspace("shared", "Shared")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "dev", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	before := b.ChangeSyncMetrics().FullReloads
	ev := &Event{EventID: "cross-store-event", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1, Variant: "a",
		UnitHash: a.HashUnit(ws.ID, "unit"), Source: "split", ClientTime: time.Now()}
	if result, err := a.IngestEvents([]*Event{ev}); err != nil || result.Accepted != 1 {
		t.Fatalf("ingest: %+v, %v", result, err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got := b.ChangeSyncMetrics().FullReloads; got != before {
		t.Fatalf("event sync caused full reload: %d -> %d", before, got)
	}
	if exposures, _ := b.EventMetricTotals(ws.ID, env.ID); exposures != 1 {
		t.Fatalf("synced exposures = %d, want 1", exposures)
	}
	if !b.HasAcceptedEvent(ev.EventID, ws.ID, env.ID, EventExposure) {
		t.Fatal("peer event absent from recent event window")
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if exposures, _ := b.EventMetricTotals(ws.ID, env.ID); exposures != 1 {
		t.Fatalf("replayed event on second sync: %d exposures", exposures)
	}
}

func TestGORMPostgresRoundtripAndDedup(t *testing.T) {
	dsn := pgtest.DSN(t)
	store, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("gorm-pg", "Postgres")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.GetWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteTokenRevocationAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.sqlite")
	a, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	created, err := a.CreateToken(TokenAdmin, "owner", RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.ResolveToken(created.Secret); err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeToken(created.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ResolveToken(created.Secret); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked token remained valid: %v", err)
	}
}

func TestGORMPostgresReplicaSyncAndLease(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ws, err := a.CreateWorkspace("shared", "Shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	gen, ok, err := a.AcquireRolloutLease(t.Context(), "a", time.Minute)
	if err != nil || !ok || gen == 0 {
		t.Fatalf("first lease: %d %v %v", gen, ok, err)
	}
	_, ok, err = b.AcquireRolloutLease(t.Context(), "b", time.Minute)
	if err != nil || ok {
		t.Fatalf("second lease unexpectedly acquired: %v %v", ok, err)
	}
	if err := a.CheckRolloutLease(t.Context(), "a", gen); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishRolloutLease(t.Context(), "a", gen, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGORMPostgresEventDeltaAndBootstrap(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	secrets := make(chan string, 2)
	errors := make(chan error, 2)
	for _, store := range []*Store{a, b} {
		wg.Go(func() {
			secret, err := store.BootstrapOwner(t.Context(), "initial")
			secrets <- secret
			errors <- err
		})
	}
	wg.Wait()
	close(secrets)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	issued := 0
	for secret := range secrets {
		if secret != "" {
			issued++
		}
	}
	if issued != 1 {
		t.Fatalf("bootstrap issued %d owner credentials, want 1", issued)
	}
	ws, err := a.CreateWorkspace("delta", "Delta")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "dev", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	before := b.ChangeSyncMetrics().FullReloads
	ev := &Event{EventID: "pg-delta-event", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1, Variant: "a",
		UnitHash: a.HashUnit(ws.ID, "unit"), Source: "split", ClientTime: time.Now()}
	if result, err := a.IngestEvents([]*Event{ev}); err != nil || result.Accepted != 1 {
		t.Fatalf("ingest: %+v, %v", result, err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got := b.ChangeSyncMetrics().FullReloads; got != before {
		t.Fatalf("event sync caused full reload: %d -> %d", before, got)
	}
	if exposures, _ := b.EventMetricTotals(ws.ID, env.ID); exposures != 1 {
		t.Fatalf("synced exposures = %d, want 1", exposures)
	}
}

func TestSQLiteLedgerRotationAndAnalysis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.sqlite")
	store, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("rotate", "Rotate")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	unit := store.HashUnit(ws.ID, "user")
	ev := &Event{EventID: "rotation-event", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1, Variant: "a", UnitHash: unit,
		Source: "split", ClientTime: time.Now()}
	if _, err := store.IngestEvents([]*Event{ev}); err != nil {
		t.Fatal(err)
	}
	if err := store.RotateHMACKey(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.AnalysisEventSnapshot(exp, time.Now())
	if err != nil || len(events) != 1 {
		t.Fatalf("analysis ledger: %d events, %v", len(events), err)
	}
	if events[0].UnitHash != reopened.HashUnit(ws.ID, "user") || events[0].UnitHash == unit {
		t.Fatal("durable unit hash was not rotated")
	}
	if _, err := reopened.BanditEvents(exp, time.Now()); err != nil {
		t.Fatalf("raw ledger and aggregates differ after rotation: %v", err)
	}
}

func TestGORMPostgresConcurrentDistinctEvents(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ws, err := a.CreateWorkspace("events", "Events")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id", Epoch: 1,
		Variants: []string{"a", "b"}, Proportions: map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	unit := a.HashUnit(ws.ID, "same-unit")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, store := range []*Store{a, b} {
		wg.Go(func() {
			<-start
			ev := &Event{EventID: fmt.Sprintf("distinct-%d", i), WorkspaceID: ws.ID,
				EnvironmentID: env.ID, Type: EventExposure, ExperimentID: exp.ID,
				Epoch: 1, Variant: "a", UnitHash: unit, Source: "split", ClientTime: time.Now()}
			_, err := store.IngestEvents([]*Event{ev})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err := NewGORMPostgresStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exposures, _ := c.EventMetricTotals(ws.ID, env.ID)
	if exposures != 2 {
		t.Fatalf("exposure count = %d, want 2", exposures)
	}
	if counts := c.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1); counts["a"] != 1 {
		t.Fatalf("unique exposure count: %v", counts)
	}
}

func TestSQLiteAssignmentDoesNotCountAsExposure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignments.sqlite")
	store, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("assignments", "Assignments")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "flag", Unit: "user_id",
		Epoch: 1, ExposureMode: "onRender", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "conv", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	unit := store.HashUnit(ws.ID, "user-1")
	assignment := &Event{EventID: "asg-dec-1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventAssignment, ExperimentID: exp.ID, Epoch: 1, FlagKey: exp.FlagKey,
		Variant: "a", UnitHash: unit, Source: "split", ClientTime: time.Now().UTC()}
	if got, err := store.IngestEvents([]*Event{assignment}); err != nil || got.Accepted != 1 {
		t.Fatalf("ingest assignment: %+v %v", got, err)
	}
	if total, _ := store.EventMetricTotals(ws.ID, env.ID); total != 0 {
		t.Fatalf("assignment counted as exposure: %d", total)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.AcceptedEvent(assignment.EventID, ws.ID, env.ID, EventAssignment); !ok {
		t.Fatal("assignment fact was not replayed")
	}
	reopened.mu.Lock()
	reopened.data.Events = nil
	reopened.data.EventIndex = map[string]int{}
	reopened.mu.Unlock()
	if _, ok := reopened.AcceptedEvent(assignment.EventID, ws.ID, env.ID, EventAssignment); !ok {
		t.Fatal("durable assignment lookup failed after working-set eviction")
	}
	if total, _ := reopened.EventMetricTotals(ws.ID, env.ID); total != 0 {
		t.Fatalf("replayed assignment counted as exposure: %d", total)
	}
}
