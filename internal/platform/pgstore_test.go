package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/test/pgtest"
)

// openPGStore opens a PostgreSQL-backed store against a real server
// (docker or GOLAB_TEST_PG_DSN — see test/pgtest). Skips with a reason when
// neither is available.
func openPGStore(t *testing.T, dsn string) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPGReopenDoesNotAdvanceRecoveryPoint(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	if _, err := a.CreateWorkspace("recovery", "Recovery"); err != nil {
		t.Fatal(err)
	}
	before := pgtest.Query(t, dsn, `SELECT control_version FROM meta WHERE id=1`)[0][0]
	b := openPGStore(t, dsn)
	if _, err := b.GetWorkspace("recovery"); err != nil {
		t.Fatal(err)
	}
	after := pgtest.Query(t, dsn, `SELECT control_version FROM meta WHERE id=1`)[0][0]
	if after != before {
		t.Fatalf("read-only reopen changed control version: %s to %s", before, after)
	}
}

// Two independent working sets converge through object reads, while token
// authorization remains a direct primary-DB check even before invalidation.
func TestPGIncrementalControlSync(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	if err := a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	ws, err := a.CreateWorkspace("incremental", "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetWorkspace(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected pre-sync workspace: %v", err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetWorkspace(ws.ID); err != nil || got.Name != "old" {
		t.Fatalf("workspace: %+v %v", got, err)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "production")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetEnvironment(ws.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := a.CreateToken(TokenAdmin, "owner", RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if len(b.ListTokens("")) != 1 {
		t.Fatalf("token list did not converge")
	}
	revokedAt := time.Now()
	if err := a.RevokeToken(tok.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ResolveToken(tok.Secret); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
	if delay := time.Since(revokedAt); delay > 2*time.Second {
		t.Fatalf("revocation authorization delay %s exceeded healthy-PG test bound", delay)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if !b.ListTokens("")[0].Revoked {
		t.Fatal("cached token still active")
	}
	exp, err := a.CreateExperiment(&Experiment{WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "flag", Epoch: 1, Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TransitionExperiment(exp.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetExperiment(exp.ID); err != nil || got.State != model.ExperimentReviewed {
		t.Fatalf("experiment: %+v %v", got, err)
	}
	q := DefaultQuotas()
	q.MaxFlags = 2
	if err := a.SetQuotas(ws.ID, q); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if b.GetQuotas(ws.ID).MaxFlags != 2 {
		t.Fatal("quota tightening did not converge")
	}
	if err := a.RevokeAIResource(ws.ID, env.ID, AITypePrompt, "prompt", "stop", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if _, revoked := b.IsAIRevoked(ws.ID, env.ID, AITypePrompt, "prompt"); !revoked {
		t.Fatal("AI revocation did not converge")
	}
	// The repository has no workspace rename API; inject a valid committed
	// protocol write to verify a changed row is re-read from workspaces.
	p := a.persist.(*pgPersistence)
	ctx := t.Context()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE meta SET control_version=control_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET name='new' WHERE id=$1`, ws.ID); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT control_version FROM meta WHERE id=1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO control_changes(version,collection,object_id) VALUES ($1,'workspaces',$2)`, version, changeKey(ws.ID)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetWorkspace(ws.ID); err != nil || got.Name != "new" {
		t.Fatalf("workspace update: %+v %v", got, err)
	}
	// An old hint carries no state and cannot roll the working set back.
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.GetWorkspace(ws.ID); got.Name != "new" {
		t.Fatal("old hint rolled back workspace")
	}
	m := b.ChangeSyncMetrics()
	if m.FullReloads != 0 || m.VersionLag != 0 || m.Applied < 6 {
		t.Fatalf("incremental metrics: %+v", m)
	}
}

func TestPGChangeSyncMissedNoticeAndIdleCost(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	if err := a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	// No listener runs while A writes; the durable sequence repairs the gap.
	ws, err := a.CreateWorkspace("missed", "missed")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	before := b.ChangeSyncMetrics()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	start := time.Now()
	for range 100 {
		if err := b.SyncChanges(); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&memAfter)
	after := b.ChangeSyncMetrics()
	if after.Queries-before.Queries != 100 || after.FullReloads != 0 {
		t.Fatalf("idle sync cost: before=%+v after=%+v", before, after)
	}
	t.Logf("idle sync: 100 version queries, %s total, %d allocated bytes, heap before=%d after=%d, 0 full reloads",
		elapsed, memAfter.TotalAlloc-memBefore.TotalAlloc, memBefore.HeapAlloc, memAfter.HeapAlloc)
}

func TestPGChangeSyncListenerReconnect(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	if err := a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); b.RunChangeSync(ctx, 100*time.Millisecond, nil) }()
	defer func() { cancel(); <-done }()
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("change sync did not converge within 4s")
	}
	waitFor(func() bool {
		var n int
		err := a.persist.(*pgPersistence).pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND query='LISTEN golab_control'`).Scan(&n)
		return err == nil && n > 0
	})
	ws, err := a.CreateWorkspace("listener", "listener")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { _, err := b.GetWorkspace(ws.ID); return err == nil })
	// Kill the dedicated LISTEN session. The loop reconnects, and the version
	// poll closes the gap even if the NOTIFY was lost during that interval.
	var killed bool
	err = a.persist.(*pgPersistence).pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity WHERE datname=current_database() AND query='LISTEN golab_control' LIMIT 1`).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate listener: %v %t", err, killed)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { _, err := b.GetEnvironment(ws.ID, env.ID); return err == nil })
	p := a.persist.(*pgPersistence)
	tx, err := p.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `UPDATE meta SET control_version=control_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE workspaces SET name='newer' WHERE id=$1`, ws.ID); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := tx.QueryRow(t.Context(), `SELECT control_version FROM meta WHERE id=1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO control_changes(version,collection,object_id) VALUES ($1,'workspaces',$2)`, version, changeKey(ws.ID)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { got, err := b.GetWorkspace(ws.ID); return err == nil && got.Name == "newer" })
	// A late duplicate with an older version carries no state and is ignored.
	oldHint := fmt.Sprintf(`{"type":"workspaces","id":"%s","version":1}`, changeKey(ws.ID))
	if _, err := p.pool.Exec(t.Context(), `SELECT pg_notify('golab_control', $1)`, oldHint); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got, _ := b.GetWorkspace(ws.ID); got.Name != "newer" {
		t.Fatal("old notification rolled back workspace")
	}
	if m := b.ChangeSyncMetrics(); m.VersionLag != 0 || m.FullReloads != 0 {
		t.Fatalf("reconnect metrics: %+v", m)
	}
}

func TestPGConcurrentDuplicateCountMatchesLedger(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("w", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := openPGStore(t, dsn)
	start := make(chan struct{})
	results := make(chan IngestResult, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{a, b} {
		wg.Go(func() {
			<-start
			res, err := store.IngestEvents([]*Event{{
				EventID: "same", WorkspaceID: ws.ID, EnvironmentID: env.ID,
				Type: EventExposure, ExperimentID: exp.ID, Epoch: exp.Epoch,
				Variant: "a", UnitHash: "unit", Source: "split", ClientTime: time.Now().UTC(),
			}})
			if err != nil {
				t.Errorf("ingest: %v", err)
			}
			results <- res
		})
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, duplicates := 0, 0
	for res := range results {
		accepted += res.Accepted
		duplicates += res.Duplicates
	}
	if accepted != 1 || duplicates != 1 {
		t.Fatalf("API accepted=%d duplicate=%d", accepted, duplicates)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != "1" {
		t.Fatalf("database accepted=%v, want 1", got)
	}
}

func testFlag(key string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion, Key: key, Type: model.FlagBool,
		Variations:  map[string]any{"on": true, "off": false},
		DefaultRule: &model.Rule{Variation: "off"},
	}
}

// TestPGPersistenceRoundtrip mirrors TestPersistenceRoundtrip against real
// PostgreSQL and additionally proves the relational side: aggregate tables,
// the partitioned event table and the dedup ledger hold the expected rows.
func TestPGPersistenceRoundtrip(t *testing.T) {
	dsn := pgtest.DSN(t)
	s1 := openPGStore(t, dsn)

	ws, err := s1.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	env, err := s1.CreateEnvironment(ws.ID, "prod", "")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	ct, err := s1.CreateToken(TokenAdmin, "root", RoleOwner, "", "", "boot")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := s1.SaveFlagDraft(ws.ID, env.ID, testFlag("f1"), RiskNormal, "root", 0); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := s1.SubmitFlagDraft(ws.ID, env.ID, "f1", "root"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s1.ApproveFlagDraft(ws.ID, env.ID, "f1", "reviewer", "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	rev := &FlagRevision{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "f1", Revision: 1,
		Content: testFlag("f1"), Hash: "h1", DataID: "golab.flag.f1.r1", PublishedBy: "root",
	}
	if err := s1.AppendFlagRevision(rev); err != nil {
		t.Fatalf("append revision: %v", err)
	}
	if err := s1.SetManifestRevision(ws.ID, env.ID, 1); err != nil {
		t.Fatalf("manifest rev: %v", err)
	}
	exp, err := s1.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID,
		FlagKey: "f1", Unit: "user_id", Epoch: 1,
		Variants:    []string{"on", "off"},
		Proportions: map[string]float64{"on": 50, "off": 50},
		PrimaryMetric: MetricSpec{
			Key: "conv", Type: "proportion", Direction: "increase",
		},
		Alpha: 0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	unitHash := s1.HashUnit(ws.ID, "user-1")
	if _, err := s1.IngestEvents([]*Event{
		{
			EventID: "e1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
			Variant: "on", UnitHash: unitHash, Source: "split", ClientTime: time.Now(),
		},
		{
			EventID: "c1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventConversion, ExperimentID: exp.ID, Epoch: 1,
			Variant: "on", UnitHash: unitHash, MetricKey: "conv", Value: 1,
			ClientTime: time.Now(),
		},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := s1.RegisterAIResource(ws.ID, env.ID, AITypePrompt, "tone", "root"); err != nil {
		t.Fatalf("ai register: %v", err)
	}
	if err := s1.RevokeAIResource(ws.ID, env.ID, AITypePrompt, "tone", "drill", "root"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s1.SetQuotas(ws.ID, Quotas{MaxFlags: 42}); err != nil {
		t.Fatalf("quotas: %v", err)
	}
	if err := s1.Audit("root", RoleOwner, "test.action", ws.ID, env.ID, "f1", "{}"); err != nil {
		t.Fatalf("audit: %v", err)
	}

	// --- relational side: the tables themselves hold the state ---
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM unit_aggs WHERE unit_hash = $1 AND cnt = 1`, unitHash); got[0][0] != "1" {
		t.Fatalf("unit_aggs row missing: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM split_exposed_units WHERE unit_hash = $1`, unitHash); got[0][0] != "1" {
		t.Fatalf("split_exposed_units row missing: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM exposed_units`); got[0][0] != "1" {
		t.Fatalf("exposed_units rows: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT cnt FROM event_counts WHERE kind = 'exposure'`); got[0][0] != "1" {
		t.Fatalf("event_counts exposure: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM event_index`); got[0][0] != "2" {
		t.Fatalf("dedup ledger: %v", got)
	}
	// The events table must be declaratively partitioned and carry a
	// partition for the current month (§10.2 按保留期分区删除).
	if got := pgtest.Query(t, dsn, `SELECT relkind::text FROM pg_class WHERE relname = 'events'`); got[0][0] != "p" {
		t.Fatalf("events must be partitioned (relkind=p): %v", got)
	}
	month := time.Now().UTC().Format("200601")
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = $1`, "events_"+month); got[0][0] != "1" {
		t.Fatalf("month partition events_%s missing: %v", month, got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM hmac_keys`); got[0][0] != "1" {
		t.Fatalf("hmac ring must hold the bootstrap key: %v", got)
	}

	// --- reopen: everything must survive, including the HMAC key ---
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openPGStore(t, dsn)
	ws2, err := s2.GetWorkspace("acme")
	if err != nil || ws2.ID != ws.ID {
		t.Fatalf("ws roundtrip: %v", err)
	}
	envs := s2.ListEnvironments(ws.ID)
	if len(envs) != 1 || envs[0].NamespaceID != env.NamespaceID {
		t.Fatalf("env roundtrip: %+v", envs)
	}
	if _, err := s2.ResolveToken(ct.Secret); err != nil {
		t.Fatalf("token roundtrip: %v", err)
	}
	draft, err := s2.GetFlagDraft(ws.ID, env.ID, "f1")
	if err != nil {
		t.Fatalf("draft roundtrip: %v", err)
	}
	// AppendFlagRevision resets an approved draft to editing against the new base.
	if draft.State != DraftEditing || draft.BaseRevision != 1 {
		t.Fatalf("draft state roundtrip: %+v", draft)
	}
	if got := s2.CurrentFlagRevision(ws.ID, env.ID, "f1"); got != 1 {
		t.Fatalf("current revision roundtrip: %d", got)
	}
	if got := s2.ManifestRevision(ws.ID, env.ID); got != 1 {
		t.Fatalf("manifest revision roundtrip: %d", got)
	}
	exp2, err := s2.GetExperiment(exp.ID)
	if err != nil || exp2.PrimaryMetric.Key != "conv" || exp2.AnalysisWindow != 24*time.Hour {
		t.Fatalf("experiment roundtrip: %v %+v", err, exp2)
	}
	if s2.HashUnit(ws.ID, "user-1") != unitHash {
		t.Fatal("HMAC key must survive restart (unit attribution stability)")
	}
	split := s2.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1)
	if split["on"] != 1 {
		t.Fatalf("exposure aggregates lost: %v", split)
	}
	aggs := s2.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "conv")
	if aggs["on"] == nil || aggs["on"].N != 1 || aggs["on"].Sum != 1 {
		t.Fatalf("unit aggregates lost: %+v", aggs)
	}
	if _, revoked := s2.IsAIRevoked(ws.ID, env.ID, AITypePrompt, "tone"); !revoked {
		t.Fatal("revocation must survive restart")
	}
	if q := s2.GetQuotas(ws.ID); q.MaxFlags != 42 {
		t.Fatalf("quotas roundtrip: %+v", q)
	}
	if len(s2.AuditList(ws.ID, 0)) == 0 {
		t.Fatal("audit must survive restart")
	}
	events := s2.EventsForExperiment(ws.ID, env.ID, exp.ID, 1, "")
	if len(events) != 2 {
		t.Fatalf("raw events roundtrip: %d", len(events))
	}
	// Ingest after reload must see the durable dedup ledger.
	res, err := s2.IngestEvents([]*Event{{
		EventID: "e1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
		Variant: "on", UnitHash: unitHash, Source: "split", ClientTime: time.Now(),
	}})
	if err != nil {
		t.Fatalf("reingest: %v", err)
	}
	if res.Duplicates != 1 || res.Accepted != 0 {
		t.Fatalf("reingest must be deduplicated: %+v", res)
	}
}

// TestPGEventPartitioningAndPrune proves retention pruning drops whole
// monthly partitions (O(1) 分区删除) plus the boundary rows and ledger
// entries, while fresh events survive.
func TestPGEventPartitioningAndPrune(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	mkEvent := func(id, unit string) *Event {
		return &Event{
			EventID: id, WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: s.HashUnit(ws.ID, unit), Source: "split",
			ClientTime: time.Now(),
		}
	}
	if _, err := s.IngestEvents([]*Event{mkEvent("old-1", "u-old"), mkEvent("new-1", "u-new")}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Backdate "old-1" into a partition two months in the past. The
	// partition must exist for row movement, mirroring what the retention
	// job would encounter on a long-running server.
	oldMonth := time.Now().UTC().AddDate(0, -2, 0)
	from := time.Date(oldMonth.Year(), oldMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	pgtest.Exec(t, dsn, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS events_%s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')`,
		from.Format("200601"), from.Format(time.RFC3339), to.Format(time.RFC3339)))
	backdated := from.Add(24 * time.Hour)
	pgtest.Exec(t, dsn, `UPDATE events SET server_time = $1 WHERE event_id = 'old-1'`, backdated)
	pgtest.Exec(t, dsn, `UPDATE event_index SET server_time = $1 WHERE event_id = 'old-1'`, backdated)

	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != "2" {
		t.Fatalf("row movement lost events: %v", got)
	}

	// Retention 24h: everything older than yesterday must go — the whole
	// old partition by DROP, the ledger rows by delete.
	s.PruneEvents(24 * time.Hour)

	if got := pgtest.Query(t, dsn,
		`SELECT count(*) FROM pg_class WHERE relname = $1`, "events_"+from.Format("200601")); got[0][0] != "0" {
		t.Fatalf("old partition must be dropped: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != "1" {
		t.Fatalf("only the fresh event may survive: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM event_index WHERE event_id = 'old-1'`); got[0][0] != "0" {
		t.Fatalf("dedup ledger must shrink with retention: %v", got)
	}
	// Aggregates are sufficient statistics and must NOT be pruned (§10.2).
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM split_exposed_units`); got[0][0] != "2" {
		t.Fatalf("aggregates must survive retention pruning: %v", got)
	}
	// Reopen so the in-memory working set reflects the pruned durable state,
	// then verify the pruned ledger entry frees the event ID for re-delivery.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openPGStore(t, dsn)
	if got := s2.EventCount(); got != 1 {
		t.Fatalf("reloaded working set = %d events, want 1", got)
	}
	res, err := s2.IngestEvents([]*Event{mkEvent("old-1", "u-old")})
	if err != nil {
		t.Fatalf("reingest: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("pruned event id must be acceptable again: %+v", res)
	}
}

// TestPGHMACKeyRotation walks the key-rotation migration path (§10.2): the
// ring extends, every stored pseudonym is re-hashed atomically, unit
// identity stays linkable (chain property), and the migration survives a
// restart.
func TestPGHMACKeyRotation(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "mean", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	h1 := s.HashUnit(ws.ID, "user-9")
	if _, err := s.IngestEvents([]*Event{
		{
			EventID: "x1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: h1, Source: "split", ClientTime: time.Now(),
		},
		{
			EventID: "x2", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventConversion, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: h1, MetricKey: "m", Value: 3, ClientTime: time.Now(),
		},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Invalid keys are rejected.
	if err := s.RotateHMACKey("not-hex"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid key accepted: %v", err)
	}
	newKey := strings.Repeat("ab", 32)
	if err := s.RotateHMACKey(newKey); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Chain property: the live hash equals one new layer over the old one,
	// so the SAME unit keeps a single identity across the rotation.
	h2 := s.HashUnit(ws.ID, "user-9")
	if want := hashUnitLayer(newKey, ws.ID, h1); h2 != want {
		t.Fatalf("rotated hash %s, want %s", h2, want)
	}
	// Every stored row migrated — no pre-rotation hash remains anywhere.
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events WHERE unit_hash = $1`, h1); got[0][0] != "0" {
		t.Fatalf("events not migrated: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events WHERE unit_hash = $1`, h2); got[0][0] != "2" {
		t.Fatalf("events must carry the rotated hash: %v", got)
	}
	for _, table := range []string{"exposed_units", "split_exposed_units", "unit_aggs"} {
		if got := pgtest.Query(t, dsn, fmt.Sprintf(`SELECT count(*) FROM %s WHERE unit_hash = $1`, table), h1); got[0][0] != "0" {
			t.Fatalf("%s not migrated: %v", table, got)
		}
		if got := pgtest.Query(t, dsn, fmt.Sprintf(`SELECT count(*) FROM %s WHERE unit_hash = $1`, table), h2); got[0][0] != "1" {
			t.Fatalf("%s must carry the rotated hash: %v", table, got)
		}
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM hmac_keys`); got[0][0] != "2" {
		t.Fatalf("ring must hold both keys: %v", got)
	}

	// New events for the same unit must land on the migrated identity:
	// exposed set stays at ONE unit, aggregates keep accumulating.
	if _, err := s.IngestEvents([]*Event{
		{
			EventID: "x3", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: s.HashUnit(ws.ID, "user-9"), Source: "split", ClientTime: time.Now(),
		},
		{
			EventID: "x4", WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventConversion, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: s.HashUnit(ws.ID, "user-9"), MetricKey: "m", Value: 5, ClientTime: time.Now(),
		},
	}); err != nil {
		t.Fatalf("post-rotation ingest: %v", err)
	}
	if split := s.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1); split["a"] != 1 {
		t.Fatalf("unit identity must survive rotation: %v", split)
	}
	aggs := s.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "m")
	if aggs["a"] == nil || aggs["a"].N != 1 || aggs["a"].Sum != 8 {
		t.Fatalf("aggregates must keep accumulating across rotation: %+v", aggs)
	}

	// Restart: the ring survives and hashing stays on the rotated chain.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openPGStore(t, dsn)
	if len(s2.data.HMACKeyRing) != 2 {
		t.Fatalf("ring not persisted: %v", s2.data.HMACKeyRing)
	}
	if got := s2.HashUnit(ws.ID, "user-9"); got != h2 {
		t.Fatalf("hash after restart %s, want %s", got, h2)
	}
	if split := s2.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1); split["a"] != 1 {
		t.Fatalf("exposed sets after restart: %v", split)
	}
	if aggs := s2.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "m"); aggs["a"] == nil || aggs["a"].Sum != 8 {
		t.Fatalf("aggregates after restart: %+v", aggs)
	}
}

// TestPGDedupLedgerBeyondMemoryWindow: the PostgreSQL dedup ledger covers
// the FULL retention window, so duplicates are still rejected after the raw
// event left the in-memory working set (simulated eviction).
func TestPGDedupLedgerBeyondMemoryWindow(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	ev := &Event{
		EventID: "dup-1", WorkspaceID: ws.ID, EnvironmentID: env.ID,
		Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
		Variant: "a", UnitHash: s.HashUnit(ws.ID, "u1"), Source: "split", ClientTime: time.Now(),
	}
	// The SAME event ID twice within one batch must count once — intra-batch
	// dedup is part of the idempotency contract (§10.1), not just cross-batch.
	dup := *ev
	first, err := s.IngestEvents([]*Event{ev, &dup})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if first.Accepted != 1 || first.Duplicates != 1 {
		t.Fatalf("intra-batch dedup: %+v", first)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != "1" {
		t.Fatalf("intra-batch duplicate reached storage: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT cnt FROM event_counts WHERE kind = 'exposure'`); got[0][0] != "1" {
		t.Fatalf("exposure count double-counted: %v", got)
	}

	// Simulate FIFO eviction from the in-memory working set: the raw event
	// and its index entry are gone, but PostgreSQL keeps the ledger.
	s.mu.Lock()
	s.data.Events = nil
	s.data.EventIndex = map[string]int{}
	s.mu.Unlock()

	retry := *ev
	retry.ClientTime = time.Now()
	res, err := s.IngestEvents([]*Event{&retry})
	if err != nil {
		t.Fatalf("reingest: %v", err)
	}
	if res.Duplicates != 1 || res.Accepted != 0 {
		t.Fatalf("ledger must dedup beyond the memory window: %+v", res)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != "1" {
		t.Fatalf("payload must not be double-inserted: %v", got)
	}
}

// TestPGConcurrentIngest hammers one store from multiple goroutines so the
// race detector sees the ingest/commit path under concurrency.
func TestPGConcurrentIngest(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "mean", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				unit := fmt.Sprintf("u-%d-%d", w, i)
				h := s.HashUnit(ws.ID, unit)
				_, err := s.IngestEvents([]*Event{
					{
						EventID: fmt.Sprintf("w%d-e%d", w, i), WorkspaceID: ws.ID, EnvironmentID: env.ID,
						Type: EventExposure, ExperimentID: exp.ID, Epoch: 1,
						Variant: "a", UnitHash: h, Source: "split", ClientTime: time.Now(),
					},
					{
						EventID: fmt.Sprintf("w%d-c%d", w, i), WorkspaceID: ws.ID, EnvironmentID: env.ID,
						Type: EventConversion, ExperimentID: exp.ID, Epoch: 1,
						Variant: "a", UnitHash: h, MetricKey: "m", Value: 2, ClientTime: time.Now(),
					},
				})
				if err != nil {
					t.Errorf("ingest: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	total := writers * perWriter
	if got := s.EventCount(); got != 2*total {
		t.Fatalf("memory events = %d, want %d", got, 2*total)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM events`); got[0][0] != fmt.Sprint(2*total) {
		t.Fatalf("pg events = %v, want %d", got, 2*total)
	}
	if split := s.SplitExposedUnitCounts(ws.ID, env.ID, exp.ID, 1); split["a"] != int64(total) {
		t.Fatalf("exposed units = %v, want %d", split, total)
	}
	aggs := s.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "m")
	if aggs["a"] == nil || aggs["a"].N != int64(total) || aggs["a"].Sum != float64(2*total) {
		t.Fatalf("aggregates = %+v, want N=%d Sum=%d", aggs["a"], total, 2*total)
	}
	if got := pgtest.Query(t, dsn, `SELECT cnt, sum_val FROM unit_aggs LIMIT 1`); got[0][0] != "1" || got[0][1] != "2" {
		t.Fatalf("pg unit_aggs sample = %v, want cnt=1 sum=2", got)
	}

	// Full reload must reproduce the aggregate view exactly.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openPGStore(t, dsn)
	if got := s2.EventCount(); got != 2*total {
		t.Fatalf("reload events = %d, want %d", got, 2*total)
	}
	if aggs := s2.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "m"); aggs["a"] == nil || aggs["a"].N != int64(total) {
		t.Fatalf("reload aggregates = %+v", aggs)
	}
}

// Two replicas begin with the same working set. Their event writes must add
// to durable aggregates instead of overwriting each other's stale values.
func TestPGMultiReplicaIngestAggregates(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("w", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := a.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "f", Epoch: 1,
		Unit: "user_id", Variants: []string{"a", "b"},
		Proportions:   map[string]float64{"a": 50, "b": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "mean", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := openPGStore(t, dsn)
	unit := a.HashUnit(ws.ID, "same-unit")
	makeEvent := func(id string) *Event {
		return &Event{EventID: id, WorkspaceID: ws.ID, EnvironmentID: env.ID,
			Type: EventConversion, ExperimentID: exp.ID, Epoch: 1,
			Variant: "a", UnitHash: unit, MetricKey: "m", Value: 2, ClientTime: time.Now()}
	}
	var wg sync.WaitGroup
	for replica, store := range []*Store{a, b} {
		wg.Go(func() {
			for i := range 10 {
				if _, err := store.IngestEvents([]*Event{makeEvent(fmt.Sprintf("r%d-%d", replica, i))}); err != nil {
					t.Errorf("replica %d ingest: %v", replica, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := pgtest.Query(t, dsn, `SELECT cnt, sum_val FROM unit_aggs WHERE unit_hash = $1`, unit); got[0][0] != "20" || got[0][1] != "40" {
		t.Fatalf("durable aggregate = %v, want count=20 sum=40", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT cnt FROM event_counts WHERE kind = 'conversion'`); got[0][0] != "20" {
		t.Fatalf("durable event count = %v, want 20", got)
	}
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if got := a.MetricAggregate(ws.ID, env.ID, exp.ID, 1, "m")["a"]; got == nil || got.Count != 20 || got.Sum != 40 {
		t.Fatalf("reloaded metric = %+v, want count=20 sum=40", got)
	}
}

// TestPGFlagLifecycleAndAIPins covers the persistence paths the roundtrip
// test does not: segments, rollback (SetCurrentFlagRevision), tombstones,
// revision pruning reconciliation, layer-slot assignment and AI version
// pins/allowlist — each asserted through a full store reload.
func TestPGFlagLifecycleAndAIPins(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	ws, _ := s.CreateWorkspace("w", "")
	env, _ := s.CreateEnvironment(ws.ID, "e", "")

	// Segment lifecycle.
	seg := &model.Segment{
		SchemaVersion: model.SchemaVersion, Key: "beta-users",
		Constraints: []model.Constraint{{Field: "beta", Op: model.OpEq, Value: true}},
	}
	if _, err := s.SaveSegmentDraft(ws.ID, env.ID, seg, RiskNormal, "root"); err != nil {
		t.Fatalf("segment draft: %v", err)
	}
	if err := s.SubmitSegmentDraft(ws.ID, env.ID, "beta-users", "root"); err != nil {
		t.Fatalf("segment submit: %v", err)
	}
	if err := s.ApproveSegmentDraft(ws.ID, env.ID, "beta-users", "reviewer"); err != nil {
		t.Fatalf("segment approve: %v", err)
	}
	if err := s.AppendSegmentRevision(&SegmentRevision{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "beta-users", Revision: 1,
		Content: seg, Hash: "sh1", DataID: "golab.segment.beta-users.r1", PublishedBy: "root",
	}); err != nil {
		t.Fatalf("segment revision: %v", err)
	}

	// Three flag revisions, then rollback to r1 and prune.
	flag := testFlag("life")
	for i := int64(1); i <= 3; i++ {
		if err := s.AppendFlagRevision(&FlagRevision{
			WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "life", Revision: i,
			Content: flag, Hash: fmt.Sprintf("h%d", i), DataID: fmt.Sprintf("golab.flag.life.r%d", i),
			PublishedBy: "root", PublishedAt: time.Now().Add(-time.Duration(4-i) * 48 * time.Hour),
		}); err != nil {
			t.Fatalf("revision %d: %v", i, err)
		}
	}
	if err := s.SetCurrentFlagRevision(ws.ID, env.ID, "life", 1, "op"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	removed, err := s.PruneFlagRevisions(ws.ID, env.ID, "life", 1, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "golab.flag.life.r2" {
		t.Fatalf("prune removed %v, want r2 only (r1 current, r3 newest)", removed)
	}

	// Tombstone + a layered experiment slot.
	if err := s.DeleteFlagCurrent(ws.ID, env.ID, "life", 9); err != nil {
		t.Fatalf("delete: %v", err)
	}
	slot := s.AssignLayerSlot(ws.ID, env.ID, "layer-a", 5000)
	if slot == nil {
		t.Fatal("layer slot assignment failed")
	}
	exp, err := s.CreateExperiment(&Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "life", Epoch: 1,
		Unit: "user_id", Layer: "layer-a", LayerSlot: slot,
		Variants:      []string{"on", "off"},
		Proportions:   map[string]float64{"on": 50, "off": 50},
		PrimaryMetric: MetricSpec{Key: "m", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: time.Hour,
	})
	if err != nil {
		t.Fatalf("experiment: %v", err)
	}
	if _, err := s.TransitionExperiment(exp.ID, model.ExperimentReviewed, "op"); err != nil {
		t.Fatalf("transition: %v", err)
	}

	// AI pins + allowlist.
	ref, err := s.RegisterAIResource(ws.ID, env.ID, AITypePrompt, "tone", "root")
	if err != nil {
		t.Fatalf("ai register: %v", err)
	}
	if err := s.PinAIVersion(&PinnedVersion{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Type: AITypePrompt, LogicalID: "tone",
		PhysicalID: ref.PhysicalID, Version: "v1", Digest: "d1", PinnedBy: "root",
	}); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	if err := s.PinAIVersion(&PinnedVersion{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, Type: AITypePrompt, LogicalID: "tone",
		PhysicalID: ref.PhysicalID, Version: "v2", Digest: "d2", PinnedBy: "root",
	}); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	if err := s.SetModelAllowlist(ws.ID, []ModelAllowEntry{
		{Provider: "qwen", Model: "qwen-max", Enabled: true, CreatedBy: "root"},
		{Provider: "anthropic", Model: "m1", Params: map[string]string{"k": "v"}, Enabled: false, CreatedBy: "root"},
	}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}

	// Reload and verify every path survived.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openPGStore(t, dsn)
	if revs := s2.ListFlagRevisions(ws.ID, env.ID, "life"); len(revs) != 2 ||
		revs[0].Revision != 3 || revs[1].Revision != 1 {
		t.Fatalf("revisions after prune reload: %+v", revs)
	}
	if cur := s2.CurrentFlagRevision(ws.ID, env.ID, "life"); cur != 0 {
		t.Fatalf("tombstoned flag must have no current revision, got %d", cur)
	}
	tombs := s2.FlagTombstones(ws.ID, env.ID, 10)
	if len(tombs) != 1 || tombs[0].Key != "life" || !tombs[0].Deleted {
		t.Fatalf("tombstone reload: %+v", tombs)
	}
	segDraft, err := s2.GetSegmentDraft(ws.ID, env.ID, "beta-users")
	if err != nil || segDraft.State != DraftEditing { // reset by AppendSegmentRevision
		t.Fatalf("segment draft reload: %+v %v", segDraft, err)
	}
	if segs := s2.CurrentSegmentContents(ws.ID, env.ID); segs["beta-users"] == nil || segs["beta-users"].Revision != 1 {
		t.Fatalf("segment revision reload: %+v", segs)
	}
	exp2, err := s2.GetExperiment(exp.ID)
	if err != nil || exp2.State != model.ExperimentReviewed ||
		exp2.LayerSlot == nil || exp2.LayerSlot.Start != slot.Start || exp2.LayerSlot.End != slot.End {
		t.Fatalf("experiment reload: %+v %v", exp2, err)
	}
	// AssignLayerSlot must see the reloaded slot as occupied.
	if next := s2.AssignLayerSlot(ws.ID, env.ID, "layer-a", 5001); next != nil {
		t.Fatalf("layer capacity lost on reload: got slot %+v", next)
	}
	pins := s2.ListPinnedVersions(ws.ID, env.ID, AITypePrompt, "tone")
	if len(pins) != 2 || pins[0].Version != "v1" || pins[1].Version != "v2" {
		t.Fatalf("pins reload: %+v", pins)
	}
	if _, ok := s2.ModelAllowed(ws.ID, "qwen", "qwen-max"); !ok {
		t.Fatal("allowlist lost on reload")
	}
	if _, ok := s2.ModelAllowed(ws.ID, "anthropic", "m1"); ok {
		t.Fatal("disabled allowlist entry must stay disabled")
	}
	allow := s2.ListModelAllowlist(ws.ID)
	if len(allow) != 2 || allow[0].Provider != "qwen" || allow[1].Params["k"] != "v" {
		t.Fatalf("allowlist order/params reload: %+v", allow)
	}
}

// TestPGMigrationsIdempotent: repeated opens re-run the migration gate
// without error and the schema records each applied version once.
func TestPGMigrationsIdempotent(t *testing.T) {
	dsn := pgtest.DSN(t)
	for i := 0; i < 3; i++ {
		s := openPGStore(t, dsn)
		if _, err := s.CreateWorkspace(fmt.Sprintf("ws%d", i), ""); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM schema_migrations`); got[0][0] != "8" {
		t.Fatalf("schema_migrations rows: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM workspaces`); got[0][0] != "3" {
		t.Fatalf("workspaces: %v", got)
	}
}

func TestPGManagementConcurrentWrites(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	type created struct {
		ws  *Workspace
		err error
	}
	results := make(chan created, 2)
	start := make(chan struct{})
	for _, s := range []*Store{a, b} {
		go func() { <-start; ws, err := s.CreateWorkspace("shared", ""); results <- created{ws, err} }()
	}
	close(start)
	r1, r2 := <-results, <-results
	if (r1.err == nil) == (r2.err == nil) {
		t.Fatalf("expected one winner and one conflict: %v, %v", r1.err, r2.err)
	}
	loser := r1
	winner := r2
	if r2.err != nil {
		loser, winner = r2, r1
	}
	if !errors.Is(loser.err, ErrConflict) {
		t.Fatalf("loser: %v", loser.err)
	}
	if _, err := a.CreateWorkspace("shared", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("retry same slug: %v", err)
	}
	if _, err := b.CreateWorkspace("shared", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("retry same slug on other replica: %v", err)
	}
	ws := winner.ws
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	first, err := a.CreateToken(TokenAdmin, "first", RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateToken(TokenAdmin, "second", RoleViewer, ws.ID, "", "test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale token create: %v", err)
	}
	second, err := b.CreateToken(TokenAdmin, "second", RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token.ID == second.Token.ID {
		t.Fatalf("duplicate token ID %q", first.Token.ID)
	}
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeToken(first.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ResolveToken(first.Secret); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked token accepted on other replica: %v", err)
	}
	if err := b.Audit("test", RoleOwner, "test.first", ws.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if err := a.Audit("test", RoleOwner, "test.second", ws.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	auditErrors := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		go func() { <-start; auditErrors <- s.Audit("test", RoleOwner, "test.concurrent", ws.ID, "", "", "") }()
	}
	close(start)
	for range 2 {
		if err := <-auditErrors; err != nil {
			t.Fatalf("concurrent audit: %v", err)
		}
	}
	if err := a.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	q1, q2 := DefaultQuotas(), DefaultQuotas()
	q1.MaxFlags, q2.MaxFlags = 101, 102
	if err := a.SetQuotas(ws.ID, q1); err != nil {
		t.Fatal(err)
	}
	if err := b.SetQuotas(ws.ID, q2); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale quota update: %v", err)
	}
	if err := b.SetQuotas(ws.ID, q2); err != nil {
		t.Fatalf("quota retry: %v", err)
	}
	c := openPGStore(t, dsn)
	if got := c.GetQuotas(ws.ID).MaxFlags; got != 102 {
		t.Fatalf("quota after restart: %d", got)
	}
	if got := c.ListTokens(ws.ID); len(got) != 2 || !got[0].Revoked && !got[1].Revoked {
		t.Fatalf("tokens after restart: %+v", got)
	}
	audits := c.AuditList(ws.ID, 0)
	if len(audits) != 4 || audits[0].ID == audits[1].ID || audits[1].ID == audits[2].ID || audits[2].ID == audits[3].ID {
		t.Fatalf("audit after restart: %+v", audits)
	}
}

// The two contenders below are separate OS processes with independent pools
// and memory images, both connected to the same real PostgreSQL database.
func TestPGManagementSubprocess(t *testing.T) {
	if dsn := os.Getenv("GOLAB_PG_CHILD_DSN"); dsn != "" {
		store := openPGStore(t, dsn)
		ready, start := os.Getenv("GOLAB_PG_CHILD_READY"), os.Getenv("GOLAB_PG_CHILD_START")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(start); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("start barrier timeout")
			}
			time.Sleep(10 * time.Millisecond)
		}
		var err error
		switch os.Getenv("GOLAB_PG_CHILD_ACTION") {
		case "create":
			_, err = store.CreateWorkspace("process-shared", "")
		case "update":
			q := DefaultQuotas()
			q.MaxFlags = 77
			err = store.SetQuotas(os.Getenv("GOLAB_PG_CHILD_WS"), q)
		case "revoke":
			err = store.RevokeToken(os.Getenv("GOLAB_PG_CHILD_TOKEN"))
		case "audit":
			err = store.Audit("child", RoleOwner, "child.concurrent", os.Getenv("GOLAB_PG_CHILD_WS"), "", "", "")
		default:
			t.Fatal("unknown child action")
		}
		switch {
		case err == nil:
			fmt.Println("RESULT WIN")
		case errors.Is(err, ErrConflict):
			fmt.Println("RESULT CONFLICT")
		default:
			t.Fatal(err)
		}
		return
	}
	dsn := pgtest.DSN(t)
	type outcome struct {
		output []byte
		err    error
	}
	runRace := func(action, wsID, tokenID string, wantWins, wantConflicts int) {
		t.Helper()
		dir := t.TempDir()
		start := filepath.Join(dir, "start")
		results := make(chan outcome, 2)
		for i := range 2 {
			ready := filepath.Join(dir, fmt.Sprintf("ready-%d", i))
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPGManagementSubprocess$", "-test.v")
			cmd.Env = append(os.Environ(), "GOLAB_PG_CHILD_DSN="+dsn,
				"GOLAB_PG_CHILD_ACTION="+action, "GOLAB_PG_CHILD_WS="+wsID,
				"GOLAB_PG_CHILD_TOKEN="+tokenID,
				"GOLAB_PG_CHILD_READY="+ready, "GOLAB_PG_CHILD_START="+start)
			go func() { output, err := cmd.CombinedOutput(); results <- outcome{output, err} }()
		}
		deadline := time.Now().Add(20 * time.Second)
		for i := range 2 {
			ready := filepath.Join(dir, fmt.Sprintf("ready-%d", i))
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s process %d failed to reach barrier", action, i)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := os.WriteFile(start, []byte("start"), 0o600); err != nil {
			t.Fatal(err)
		}
		wins, conflicts := 0, 0
		for range 2 {
			r := <-results
			if r.err != nil {
				t.Fatalf("%s child: %v\n%s", action, r.err, r.output)
			}
			if strings.Contains(string(r.output), "RESULT WIN") {
				wins++
			}
			if strings.Contains(string(r.output), "RESULT CONFLICT") {
				conflicts++
			}
		}
		if wins != wantWins || conflicts != wantConflicts {
			t.Fatalf("%s: wins=%d conflicts=%d", action, wins, conflicts)
		}
	}
	runRace("create", "", "", 1, 1)
	c := openPGStore(t, dsn)
	if got := c.ListWorkspaces(); len(got) != 1 {
		t.Fatalf("workspaces after restart: %+v", got)
	}
	wsID := c.ListWorkspaces()[0].ID
	runRace("update", wsID, "", 1, 1)
	if err := c.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if got := c.GetQuotas(wsID).MaxFlags; got != 77 {
		t.Fatalf("quota after processes: %d", got)
	}
	created, err := c.CreateToken(TokenAdmin, "process-token", RoleViewer, wsID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	runRace("revoke", wsID, created.Token.ID, 1, 1)
	if _, err := c.ResolveToken(created.Secret); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked token after processes: %v", err)
	}
	runRace("audit", wsID, "", 2, 0)
	if err := c.ReloadFromBackend(); err != nil {
		t.Fatal(err)
	}
	if got := c.AuditList(wsID, 0); len(got) != 2 || got[0].ID == got[1].ID {
		t.Fatalf("audit after processes: %+v", got)
	}
}

func TestPGManagementLockTimeoutAndDisconnect(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	key := int64(0x676f6c61622d7465)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release, err := a.persist.(*pgPersistence).LockScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	if _, err := b.persist.(*pgPersistence).LockScope(short, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock timeout: %v", err)
	}
	release()
	reacquire, err := b.persist.(*pgPersistence).LockScope(ctx, key)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	reacquire()
	// A killed PostgreSQL session must release its advisory lock.
	conn, err := a.persist.(*pgPersistence).pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := b.persist.(*pgPersistence).pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	reacquire, err = b.persist.(*pgPersistence).LockScope(ctx, key)
	if err != nil {
		t.Fatalf("lock after terminated session: %v", err)
	}
	reacquire()
}
