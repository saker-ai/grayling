package platform

import (
	"context"
	"errors"
	"fmt"
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
	if err := s1.ApproveFlagDraft(ws.ID, env.ID, "f1", "root", "ok"); err != nil {
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
	if err := s.ApproveSegmentDraft(ws.ID, env.ID, "beta-users", "root"); err != nil {
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
// without error and the schema records exactly one applied version.
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
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM schema_migrations`); got[0][0] != "1" {
		t.Fatalf("schema_migrations rows: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM workspaces`); got[0][0] != "3" {
		t.Fatalf("workspaces: %v", got)
	}
}
