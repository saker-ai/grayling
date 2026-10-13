package admin

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/godeps/grayling/internal/configstore"
	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/pkg/model"
)

// TestReconcileHealthyNoop: a freshly published environment reconciles to
// "healthy" with zero repairs — the reconciler must be a no-op when store and
// substrate already agree (idempotent, never inflates revisions).
func TestReconcileHealthyNoop(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	stats := f.svc.ReconcileOnce()
	// The fixture has two workspaces (acme + globex); only acme/prod is
	// published. Both environments reconcile cleanly (globex is empty).
	if stats.Environments < 1 {
		t.Fatalf("environments = %d, want ≥1", stats.Environments)
	}
	if stats.Repaired() || len(stats.Errors) != 0 {
		t.Fatalf("expected a clean healthy pass, got %+v", stats)
	}
	if stats.Healthy != stats.Environments {
		t.Fatalf("healthy = %d, want all %d environments clean (%+v)",
			stats.Healthy, stats.Environments, stats)
	}
	// Manifest revision must NOT move on a healthy reconcile.
	if rev := f.store.ManifestRevision(f.ws.ID, f.env.ID); rev != 1 {
		t.Fatalf("manifest revision drifted to %d on a no-op reconcile", rev)
	}
	// Second pass is also a no-op (stable fixed point).
	if s2 := f.svc.ReconcileOnce(); s2.Repaired() || len(s2.Errors) != 0 {
		t.Fatalf("second pass not idempotent: %+v", s2)
	}
}

// TestReconcileRepairsMissingFlagDoc: the substrate loses an in-force flag
// document (e.g. a gonacos node that missed a Redis pub/sub event while
// disconnected). The reconciler republishes it by read-back and the snapshot
// becomes loadable again — WITHOUT touching the manifest revision.
func TestReconcileRepairsMissingFlagDoc(t *testing.T) {
	f := newFixture(t)
	res := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	// Sanity: the snapshot loads before the injected loss.
	if _, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID); err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	// Inject drift: delete the immutable flag document from the substrate.
	if err := f.svc.Config.DeleteDoc(f.env.NamespaceID, res.DataID); err != nil {
		t.Fatalf("inject doc loss: %v", err)
	}
	if _, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID); err == nil {
		t.Fatal("snapshot must be unloadable after losing a referenced document")
	}

	stats := f.svc.ReconcileOnce()
	if stats.DocsRepaired != 1 {
		t.Fatalf("docsRepaired = %d, want 1 (%+v)", stats.DocsRepaired, stats)
	}
	if stats.ManifestsRepaired != 0 {
		t.Fatalf("manifest must not be republished for a pure doc repair: %+v", stats)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", stats.Errors)
	}
	// The document reads back with the recorded hash again.
	_, hash, err := f.svc.Config.GetDoc(f.env.NamespaceID, res.DataID)
	if err != nil || hash != res.Hash {
		t.Fatalf("doc after repair: hash=%q err=%v, want %q", hash, err, res.Hash)
	}
	if _, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID); err != nil {
		t.Fatalf("snapshot after repair: %v", err)
	}
	// Revision unchanged (doc repair does not bump the manifest).
	if rev := f.store.ManifestRevision(f.ws.ID, f.env.ID); rev != res.ManifestRevision {
		t.Fatalf("manifest revision changed to %d on a doc repair", rev)
	}
}

// TestReconcileRepairsMissingManifest: the substrate loses the manifest
// entirely (fresh/restarted node). The reconciler catches up AT the store's
// recorded revision — no revision inflation — and the snapshot loads again.
func TestReconcileRepairsMissingManifest(t *testing.T) {
	f := newFixture(t)
	res := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	if err := f.svc.Config.DeleteDoc(f.env.NamespaceID, configstore.ManifestDataID); err != nil {
		t.Fatalf("inject manifest loss: %v", err)
	}
	if _, err := f.svc.Config.GetManifest(f.env.NamespaceID); err == nil {
		t.Fatal("manifest must be gone after deletion")
	}

	stats := f.svc.ReconcileOnce()
	if stats.ManifestsRepaired != 1 {
		t.Fatalf("manifestsRepaired = %d, want 1 (%+v)", stats.ManifestsRepaired, stats)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", stats.Errors)
	}
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("manifest after repair: %v", err)
	}
	if m.Revision != res.ManifestRevision {
		t.Fatalf("manifest revision = %d, want catch-up at %d (no inflation)",
			m.Revision, res.ManifestRevision)
	}
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("snapshot after repair: %v", err)
	}
	if _, ok := snap.Flags["checkout"]; !ok {
		t.Fatal("repaired snapshot is missing the flag")
	}
}

// TestReconcileRepairsStaleManifestContent: the substrate manifest sits at the
// SAME revision as the store but its entries disagree (lost/desynced write).
// The reconciler republishes BUMPED (rev+1) so revision/ETag watchers refetch.
func TestReconcileRepairsStaleManifestContent(t *testing.T) {
	f := newFixture(t)
	res := fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	// Overwrite the substrate manifest with an empty one at the SAME revision
	// (simulates a desync where the store recorded a flag the substrate lost).
	stale := &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		Workspace:     f.ws.ID, Environment: f.env.ID,
		Revision: res.ManifestRevision, // same revision, wrong content
	}
	if err := f.svc.Config.PublishManifest(f.env.NamespaceID, stale); err != nil {
		t.Fatalf("inject stale manifest: %v", err)
	}

	stats := f.svc.ReconcileOnce()
	if stats.ManifestsRepaired != 1 {
		t.Fatalf("manifestsRepaired = %d, want 1 (%+v)", stats.ManifestsRepaired, stats)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", stats.Errors)
	}
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("manifest after repair: %v", err)
	}
	if m.Revision != res.ManifestRevision+1 {
		t.Fatalf("manifest revision = %d, want bumped %d (same-revision content drift must refetch)",
			m.Revision, res.ManifestRevision+1)
	}
	if len(m.Flags) != 1 || m.Flags[0].Key != "checkout" {
		t.Fatalf("repaired manifest entries = %+v, want the checkout flag", m.Flags)
	}
}

// TestReconcileUnexplainedSubstrateManifest: a manifest exists in the
// substrate with NO store record at all. The store is the source of truth, so
// the reconciler must surface this for manual review — never silently adopt or
// delete it.
func TestReconcileUnexplainedSubstrateManifest(t *testing.T) {
	f := newFixture(t)
	// Publish directly to the substrate behind the store's back (no store rev).
	ghost := &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		Workspace:     f.ws.ID, Environment: f.env.ID,
		Revision: 9,
		Flags: []model.ManifestEntry{
			{Key: "ghost", Revision: 1, DataID: configstore.FlagDataID("ghost", 1), Hash: "x"},
		},
	}
	if err := f.svc.Config.PublishManifest(f.env.NamespaceID, ghost); err != nil {
		t.Fatalf("inject ghost manifest: %v", err)
	}
	stats := f.svc.ReconcileOnce()
	if len(stats.Errors) == 0 {
		t.Fatal("expected a manual-review error for an unexplained substrate manifest")
	}
	if stats.ManifestsRepaired != 0 {
		t.Fatalf("must not repair/adopt a ghost manifest: %+v", stats)
	}
	// The ghost manifest is left untouched (not deleted, not adopted).
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil || m.Revision != 9 {
		t.Fatalf("ghost manifest altered: rev=%d err=%v", func() int64 {
			if m == nil {
				return -1
			}
			return m.Revision
		}(), err)
	}
	if rev := f.store.ManifestRevision(f.ws.ID, f.env.ID); rev != 0 {
		t.Fatalf("store must not adopt the ghost revision, got %d", rev)
	}
}

// flakyConfig injects transient GetManifest failures in front of the real
// gonacos-backed configstore.
type flakyConfig struct {
	configstore.Store
	failGetManifest int
}

func (f *flakyConfig) GetManifest(namespaceID string) (*model.Manifest, error) {
	if f.failGetManifest > 0 {
		f.failGetManifest--
		return nil, fmt.Errorf("transient substrate read failure")
	}
	return f.Store.GetManifest(namespaceID)
}

func (f *flakyConfig) GetManifestContext(ctx context.Context, namespaceID string) (*model.Manifest, error) {
	if f.failGetManifest > 0 {
		f.failGetManifest--
		return nil, fmt.Errorf("transient substrate read failure")
	}
	return f.Store.GetManifestContext(ctx, namespaceID)
}

// TestReconcileTransientManifestReadErrorDoesNotRepublish: a FAILED manifest
// read means "substrate state unknown" — not "substrate behind". The
// reconciler must reload once and re-decide instead of republishing its
// (possibly stale) working set over a substrate that may hold a NEWER
// manifest (which would silently undo a concurrent replica's publish, e.g.
// a kill-switch, cluster-wide).
func TestReconcileTransientManifestReadErrorDoesNotRepublish(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	flaky := &flakyConfig{Store: f.svc.Config, failGetManifest: 1}
	f.svc.Config = flaky
	stats := f.svc.ReconcileOnce()
	if stats.ManifestsRepaired != 0 || stats.DocsRepaired != 0 {
		t.Fatalf("transient read error must not trigger any repair: %+v", stats)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("transient read error must be absorbed by reload+retry, got errors: %v", stats.Errors)
	}
	if stats.Healthy != stats.Environments {
		t.Fatalf("expected all environments healthy after retry: %+v", stats)
	}
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil || m.Revision != 1 {
		t.Fatalf("substrate manifest disturbed: rev=%d err=%v", func() int64 {
			if m == nil {
				return -1
			}
			return m.Revision
		}(), err)
	}
}

// TestReconcilePersistentManifestReadErrorRepairs: when the manifest read
// keeps failing AFTER the reload (substrate genuinely broken/unreachable for
// reads but writable), the reconciler falls back to republishing the store
// truth at the recorded revision.
func TestReconcilePersistentManifestReadErrorRepairs(t *testing.T) {
	f := newFixture(t)
	fullPublishFlow(t, f, f.publisher, f.owner, testFlag("checkout"))

	flaky := &flakyConfig{Store: f.svc.Config, failGetManifest: 1 << 30}
	f.svc.Config = flaky
	stats := f.svc.ReconcileOnce()
	if stats.ManifestsRepaired != 1 {
		t.Fatalf("persistent read failure must be repaired by republish, got %+v", stats)
	}
	if flaky.failGetManifest >= 1<<30 {
		t.Fatal("GetManifest was never attempted")
	}
}

// TestConcurrentPublishSerialization: N concurrent PublishFlag calls on
// distinct flags in the SAME environment must all succeed with a strictly
// monotonic, gap-free set of manifest revisions and every flag present in the
// final manifest — the publish lock serializes revision allocation so no two
// publishes collide on a manifest revision (the single-process half of the
// multi-replica serialization guarantee; the cross-process half runs on a
// PostgreSQL advisory lock and is proven in test/cluster).
func TestConcurrentPublishSerialization(t *testing.T) {
	f := newFixture(t)
	const n = 12

	// Pre-approve N distinct drafts (approval itself is not the racing op).
	for i := 0; i < n; i++ {
		fl := testFlag(fmt.Sprintf("flag-%02d", i))
		if _, err := f.svc.SaveFlagDraft(f.publisher, f.ws.ID, f.env.ID, fl, platform.RiskNormal, 0); err != nil {
			t.Fatalf("save draft %d: %v", i, err)
		}
		if err := f.svc.SubmitFlagDraft(f.publisher, f.ws.ID, f.env.ID, fl.Key); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if err := f.svc.ApproveFlagDraft(f.owner, f.ws.ID, f.env.ID, fl.Key, "ok"); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	revs := make([]int64, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := f.svc.PublishFlag(f.owner, f.ws.ID, f.env.ID,
				fmt.Sprintf("flag-%02d", i), "ship")
			if err != nil {
				errs[i] = err
				return
			}
			revs[i] = res.ManifestRevision
		}(i)
	}
	wg.Wait()

	seenRev := map[int64]int{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("publish %d failed: %v", i, errs[i])
		}
		seenRev[revs[i]]++
	}
	// Every manifest revision must be unique (no two publishes collided).
	if len(seenRev) != n {
		t.Fatalf("manifest revisions collided: %v (want %d unique)", revs, n)
	}
	// Revisions form the contiguous range [1, n].
	for r := int64(1); r <= n; r++ {
		if seenRev[r] != 1 {
			t.Fatalf("manifest revision %d appears %d times, want exactly 1", r, seenRev[r])
		}
	}
	// The final manifest carries all N flags and sits at revision n.
	m, err := f.svc.Config.GetManifest(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("final manifest: %v", err)
	}
	if m.Revision != n {
		t.Fatalf("final manifest revision = %d, want %d", m.Revision, n)
	}
	live := 0
	for _, e := range m.Flags {
		if !e.Deleted {
			live++
		}
	}
	if live != n {
		t.Fatalf("final manifest has %d live flags, want %d", live, n)
	}
	// The snapshot is complete and loadable (no lost flag documents).
	snap, err := f.svc.Config.LoadSnapshot(f.env.NamespaceID)
	if err != nil {
		t.Fatalf("final snapshot: %v", err)
	}
	if len(snap.Flags) != n {
		t.Fatalf("snapshot has %d flags, want %d", len(snap.Flags), n)
	}
}
