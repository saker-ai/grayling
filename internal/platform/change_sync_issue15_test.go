package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godeps/grayling/pkg/model"
	"github.com/godeps/grayling/test/pgtest"
)

func TestPGScopedFlagSegmentSync(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("scoped", "scoped")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	flag := testFlag("feature")
	if _, err = a.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "a", 0); err != nil {
		t.Fatal(err)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetFlagDraft(ws.ID, env.ID, "feature"); err != nil || got.Content.Key != "feature" {
		t.Fatalf("flag draft: %+v %v", got, err)
	}
	seg := &model.Segment{SchemaVersion: model.SchemaVersion, Key: "cohort", Constraints: []model.Constraint{{Field: "beta", Op: model.OpEq, Value: true}}}
	if _, err = a.SaveSegmentDraft(ws.ID, env.ID, seg, RiskNormal, "a"); err != nil {
		t.Fatal(err)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetSegmentDraft(ws.ID, env.ID, "cohort"); err != nil || got.Content.Key != "cohort" {
		t.Fatalf("segment draft: %+v %v", got, err)
	}
	if err = a.AppendFlagRevision(&FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "feature", Revision: 1, Content: flag, Hash: "h1", DataID: "flag-r1"}); err != nil {
		t.Fatal(err)
	}
	if err = a.AppendSegmentRevision(&SegmentRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: "cohort", Revision: 1, Content: seg, Hash: "s1", DataID: "segment-r1"}); err != nil {
		t.Fatal(err)
	}
	if err = a.SetManifestRevision(ws.ID, env.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	scope := flagScopeKey(ws.ID, env.ID, "feature")
	segScope := flagScopeKey(ws.ID, env.ID, "cohort")
	if b.CurrentFlagRevision(ws.ID, env.ID, "feature") != 1 || len(b.data.FlagRevisions[scope]) != 1 || b.CurrentSegmentRevision(ws.ID, env.ID, "cohort") != 1 || len(b.data.SegmentRevisions[segScope]) != 1 || b.data.ManifestRevisions[scopeKey(ws.ID, env.ID)] != 1 {
		t.Fatal("scoped revision/state did not converge")
	}
	// Both replicas start at the same epoch; only one conflicting edit commits.
	if err = a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{a, b} {
		go func() {
			<-start
			_, err := store.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "concurrent", 1)
			results <- err
		}()
	}
	close(start)
	e1, e2 := <-results, <-results
	if (e1 == nil) == (e2 == nil) || (e1 != nil && !errors.Is(e1, ErrConflict)) || (e2 != nil && !errors.Is(e2, ErrConflict)) {
		t.Fatalf("concurrent writes: %v, %v", e1, e2)
	}
	if err = a.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if a.ChangeSyncMetrics().FallbackReloads != 0 || b.ChangeSyncMetrics().FallbackReloads != 0 {
		t.Fatalf("normal flag/segment update fell back: a=%+v b=%+v", a.ChangeSyncMetrics(), b.ChangeSyncMetrics())
	}
	// A restart reloads exactly the same canonical state.
	c := openPGStore(t, dsn)
	if c.CurrentFlagRevision(ws.ID, env.ID, "feature") != b.CurrentFlagRevision(ws.ID, env.ID, "feature") || c.CurrentSegmentRevision(ws.ID, env.ID, "cohort") != 1 {
		t.Fatal("restart differs from synced replica")
	}
}

func TestPGJournalPruneAndLaggedRecovery(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("prune", "prune")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	flag := testFlag("safe")
	if _, err = a.SaveFlagDraft(ws.ID, env.ID, flag, RiskNormal, "a", 0); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	ap := a.persist.(*pgPersistence)
	bp := b.persist.(*pgPersistence)
	if err = ap.ackJournal(ctx, a.data.ControlVersion); err != nil {
		t.Fatal(err)
	}
	if err = ap.pruneJournal(ctx, &a.syncStats); err != nil {
		t.Fatal(err)
	}
	// The live lagging B cursor keeps its missing change in the journal.
	var watermark int64
	if err = ap.pool.QueryRow(ctx, `SELECT pruned_through FROM control_journal_watermark`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if watermark != b.data.ControlVersion {
		t.Fatalf("watermark %d passed live cursor %d", watermark, b.data.ControlVersion)
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	if b.ChangeSyncMetrics().FallbackReloads != 0 {
		t.Fatal("live lagging replica reloaded")
	}
	// A disconnected B misses another change and its lease expires.
	if _, err = a.SaveFlagDraft(ws.ID, env.ID, flag, RiskHigh, "a", 0); err != nil {
		t.Fatal(err)
	}
	if err = ap.ackJournal(ctx, a.data.ControlVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = ap.pool.Exec(ctx, `UPDATE control_replica_cursors SET heartbeat_at=clock_timestamp()-interval '1 minute' WHERE replica_id=$1`, bp.replicaID); err != nil {
		t.Fatal(err)
	}
	if err = ap.pruneJournal(ctx, &a.syncStats); err != nil {
		t.Fatal(err)
	}
	if a.ChangeSyncMetrics().PrunedThrough <= b.data.ControlVersion {
		t.Fatal("expired cursor blocked cleanup")
	}
	if err = b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetFlagDraft(ws.ID, env.ID, "safe")
	if err != nil || got.Risk != RiskHigh {
		t.Fatalf("recovery missed final flag: %+v %v", got, err)
	}
	if b.ChangeSyncMetrics().FallbackReloads != 1 {
		t.Fatalf("expected one verified fallback: %+v", b.ChangeSyncMetrics())
	}
	if err = bp.pruneJournal(ctx, &b.syncStats); err != nil {
		t.Fatal(err)
	}
	if b.ChangeSyncMetrics().JournalRows != 0 || b.ChangeSyncMetrics().OldestUnconfirmedCursor != b.data.ControlVersion {
		t.Fatalf("journal metrics: %+v", b.ChangeSyncMetrics())
	}
}

func TestPGScopedIdleAndListenerRecovery(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPGStore(t, dsn), openPGStore(t, dsn)
	ws, err := a.CreateWorkspace("idle", "idle")
	if err != nil {
		t.Fatal(err)
	}
	env, err := a.CreateEnvironment(ws.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); b.RunChangeSync(ctx, 50*time.Millisecond, nil) }()
	defer func() { cancel(); <-done }()
	wait := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("listener failed to converge")
	}
	wait(func() bool { _, err := b.GetEnvironment(ws.ID, env.ID); return err == nil })
	// Terminating LISTEN tests lost notification while the independent poll runs.
	_, err = a.persist.(*pgPersistence).pool.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND query='LISTEN golab_control'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SaveFlagDraft(ws.ID, env.ID, testFlag("idle-flag"), RiskNormal, "a", 0); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { _, err := b.GetFlagDraft(ws.ID, env.ID, "idle-flag"); return err == nil })
	before := b.ChangeSyncMetrics()
	bp := b.persist.(*pgPersistence)
	var heartbeatBefore, heartbeatAfter time.Time
	if err := bp.pool.QueryRow(t.Context(), `SELECT heartbeat_at FROM control_replica_cursors WHERE replica_id=$1`, bp.replicaID).Scan(&heartbeatBefore); err != nil {
		t.Fatal(err)
	}
	time.Sleep(11 * time.Second)
	if err := bp.pool.QueryRow(t.Context(), `SELECT heartbeat_at FROM control_replica_cursors WHERE replica_id=$1`, bp.replicaID).Scan(&heartbeatAfter); err != nil {
		t.Fatal(err)
	}
	after := b.ChangeSyncMetrics()
	if !heartbeatAfter.After(heartbeatBefore) {
		t.Fatalf("idle lease was not renewed: %s -> %s", heartbeatBefore, heartbeatAfter)
	}
	if after.Queries <= before.Queries || after.FallbackReloads != 0 || after.FullReloads != 0 || after.VersionLag != 0 {
		t.Fatalf("idle sync: before=%+v after=%+v", before, after)
	}
}

func TestPGJournalPruneRollbackOnFailure(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPGStore(t, dsn)
	if _, err := s.CreateWorkspace("atomic", "atomic"); err != nil {
		t.Fatal(err)
	}
	p := s.persist.(*pgPersistence)
	if err := p.ackJournal(t.Context(), s.data.ControlVersion); err != nil {
		t.Fatal(err)
	}
	before := pgtest.Query(t, dsn, `SELECT count(*) FROM control_changes`)[0][0]
	tx, err := p.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `LOCK TABLE control_changes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err = p.pruneJournal(ctx, &s.syncStats); err == nil {
		t.Fatal("expected blocked deletion to fail")
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := pgtest.Query(t, dsn, `SELECT count(*) FROM control_changes`)[0][0]
	watermark := pgtest.Query(t, dsn, `SELECT pruned_through FROM control_journal_watermark`)[0][0]
	if before != after || watermark != "0" {
		t.Fatalf("failed prune partly committed: rows %s -> %s, watermark %s", before, after, watermark)
	}
}
