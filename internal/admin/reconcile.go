// Publish reconciliation — the 发布对账后台任务 of docs/技术方案.md §3/§11
// ("Config 与 PostgreSQL 没有跨库原子事务，发布状态记录目标 revision 并靠
// 重试/对账收敛") and the convergence mechanism for multi-replica and
// multi-node gonacos deployments.
//
// The platform store (PostgreSQL) is the source of truth for WHAT must be
// published; the gonacos substrate is what SDKs actually read. They can
// diverge: a publish whose manifest step failed, a gonacos cluster node that
// missed Redis pub/sub events while disconnected (Redis pub/sub is
// fire-and-forget), or a node restored from an older persistence envelope.
// The reconciler periodically re-verifies every environment's substrate
// state against the store (发布读回) and idempotently republishes whatever
// drifted (状态对账), then notifies SSE subscribers so SDKs refetch.
//
// Repairs are conservative and revision-exact:
//   - substrate BEHIND the store → republish missing documents and the
//     manifest AT the store's recorded revision (catch-up; no inflation);
//   - substrate AT the store's revision but entries/documents disagree →
//     repair documents; if the manifest itself drifted, republish at
//     revision+1 so ETag/revision watchers actually refetch;
//   - substrate AHEAD of the store → reload once (a replica may have moved);
//     if it is genuinely ahead (a publish whose final store commit was
//     lost), adopt the revision number monotonically and verify content;
//     an unexplained manifest with no store state at all is surfaced for
//     manual review, never silently adopted.
package admin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// reconcilerActor is the audit identity of automated repairs (mirrors the
// rollout engine's system identity convention).
var reconcilerActor = Actor{
	TokenID: "system", Name: "reconciler",
	Kind: platform.TokenAdmin, Role: platform.RoleOwner,
}

// ReconcileStats summarizes one reconciliation pass.
type ReconcileStats struct {
	Environments      int      `json:"environments"`
	Healthy           int      `json:"healthy"`
	DocsRepaired      int      `json:"docsRepaired"`
	ManifestsRepaired int      `json:"manifestsRepaired"`
	Errors            []string `json:"errors,omitempty"`
}

// Repaired reports whether the pass changed anything.
func (st ReconcileStats) Repaired() bool {
	return st.DocsRepaired+st.ManifestsRepaired > 0
}

// ReconcileOnce runs one pass over every environment. It first catches up the
// PostgreSQL control journal, then reconciles each environment under the publish lock (the
// same lock publishes take — a repair can never interleave with a publish).
func (s *Service) ReconcileOnce() ReconcileStats {
	return s.ReconcileOnceContext(context.Background())
}

// ReconcileOnceContext runs a pass that stops when ctx is canceled.
func (s *Service) ReconcileOnceContext(ctx context.Context) ReconcileStats {
	stats := ReconcileStats{}
	if err := s.Store.SyncChanges(); err != nil {
		stats.Errors = append(stats.Errors, "sync: "+err.Error())
		return stats
	}
	for _, ws := range s.Store.ListWorkspaces() {
		for _, env := range s.Store.ListEnvironments(ws.ID) {
			if err := ctx.Err(); err != nil {
				return stats
			}
			s.reconcileEnv(ctx, ws.ID, env.ID, &stats)
		}
	}
	return stats
}

// RunReconciler runs ReconcileOnce on a ticker until ctx is done. interval
// ≤ 0 disables the loop (manual ReconcileOnce calls remain available).
func (s *Service) RunReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		s.logf("reconcile: disabled (interval %s)", interval)
		<-ctx.Done()
		return
	}
	s.logf("reconcile: publish reconciliation started (interval %s)", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			s.logf("reconcile: stopped")
			return
		case <-t.C:
			stats := s.ReconcileOnceContext(ctx)
			m := s.Store.ChangeSyncMetrics()
			s.logf("reconcile: envs=%d healthy=%d docsRepaired=%d manifestsRepaired=%d errors=%v control_applied=%d control_queries=%d control_failures=%d control_version_lag=%d control_max_notification_delay=%s full_reloads=%d last_full_reload=%s control_fallback_reloads=%d control_journal_rows=%d control_journal_bytes=%d control_oldest_unconfirmed_cursor=%d control_pruned_through=%d",
				stats.Environments, stats.Healthy, stats.DocsRepaired,
				stats.ManifestsRepaired, stats.Errors, m.Applied, m.Queries, m.Failures, m.VersionLag,
				m.MaxNotificationDelay, m.FullReloads, m.LastFullReload, m.FallbackReloads, m.JournalRows, m.JournalBytes, m.OldestUnconfirmedCursor, m.PrunedThrough)
		}
	}
}

func (s *Service) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// acquireReconcileLock takes the publish locks WITHOUT the publish-path
// reload: ReconcileOnce already reloaded at pass start, and per-environment
// reloads happen only when the substrate looks ahead of the working set.
func (s *Service) acquireReconcileLock(env *platform.Environment) (func(), error) {
	s.publishMu.Lock()
	releaseDB, err := s.Store.PublishLock(env.WorkspaceID, env.ID)
	if err != nil {
		s.publishMu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseDB()
			s.publishMu.Unlock()
		})
	}, nil
}

func (s *Service) reconcileEnv(ctx context.Context, wsID, envID string, stats *ReconcileStats) {
	env, err := s.Store.GetEnvironment(wsID, envID)
	if err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf("env %s/%s: %v", wsID, envID, err))
		return
	}
	stats.Environments++
	release, err := s.acquireReconcileLock(env)
	if err != nil {
		stats.Errors = append(stats.Errors,
			fmt.Sprintf("env %s/%s: publish lock: %v", wsID, envID, err))
		return
	}
	defer release()

	reloaded := false
	for {
		ws, err := s.Store.GetWorkspace(env.WorkspaceID)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("env %s: %v", envID, err))
			return
		}
		env, err = s.Store.GetEnvironment(ws.ID, env.ID)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("env %s: %v", envID, err))
			return
		}
		expected := s.Store.ManifestRevision(ws.ID, env.ID)
		actual, actualErr := s.Config.GetManifestContext(ctx, env.NamespaceID)
		actualRev := int64(0)
		if actualErr == nil {
			actualRev = actual.Revision
		}
		// A missing manifest is a KNOWN state (never published, or fully
		// lost substrate), not an "unknown" one — only other read failures
		// warrant the reload-and-redecide guard below.
		notFound := errors.Is(actualErr, configstore.ErrNotFound)

		if expected == 0 && (notFound || (actualErr == nil && actualRev == 0)) {
			stats.Healthy++ // never published — nothing to reconcile
			return
		}
		if !reloaded && (actualRev > expected || (actualErr != nil && !notFound)) {
			// Either the substrate is ahead of this working set (another
			// replica published after the pass-start reload) OR the substrate
			// read failed with an unknown-state error. Reload once under the
			// lock and re-decide with fresh store truth: this prevents a
			// transient GetManifest error from making us republish a STALE
			// working set over a newer substrate manifest (which would
			// silently undo a concurrent replica's publish, e.g. a
			// kill-switch, cluster-wide).
			if err := s.Store.ReloadFromBackend(); err != nil {
				stats.Errors = append(stats.Errors,
					fmt.Sprintf("%s/%s: reload: %v", ws.Slug, env.Slug, err))
				return
			}
			reloaded = true
			continue
		}
		if expected == 0 && actualRev > 0 {
			// Reload did not explain it: a gonacos manifest exists with no
			// store record at all. The store is the source of truth (§3);
			// this is residue of a crashed pre-store publish — surfaced for
			// manual review, never silently adopted or deleted.
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"%s/%s: substrate manifest r%d exists but the store has no published revision (manual review)",
				ws.Slug, env.Slug, actualRev))
			return
		}
		if expected == 0 {
			// Nothing was ever published for this environment and the
			// substrate manifest is unreadable (actualErr): there is no store
			// truth to repair TO — report, never publish an empty r0.
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"%s/%s: substrate manifest unreadable and the store has no published revision: %v",
				ws.Slug, env.Slug, actualErr))
			return
		}
		if actualErr != nil || actualRev < expected {
			// Substrate behind the store: lost sync events, a fresh/restarted
			// node, or a rolled-back persistence envelope. Republish the
			// store truth AT the recorded revision (catch-up, no inflation).
			docs := s.repairCurrentDocs(ctx, ws, env, stats)
			if ctx.Err() != nil {
				return
			}
			s.republishManifest(ctx, ws, env, expected, fmt.Sprintf(
				"substrate at r%d (read err: %v), store at r%d, docs repaired %d",
				actualRev, actualErr, expected, docs), stats)
			return
		}
		if actualRev > expected {
			// Genuinely ahead after reload: a publish whose final store
			// commit never landed. Adopt the revision number (monotonic) and
			// verify content against the store below.
			if err := s.Store.SetManifestRevision(ws.ID, env.ID, actualRev); err != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf(
					"%s/%s: adopt substrate r%d: %v", ws.Slug, env.Slug, actualRev, err))
				return
			}
			expected = actualRev
			s.logf("reconcile: %s/%s adopted substrate manifest r%d into the store",
				ws.Slug, env.Slug, actualRev)
		}

		// Substrate AT the store revision: verify every in-force document
		// reads back with its recorded hash, and the manifest entries point
		// at exactly the store's current set.
		docs := s.repairCurrentDocs(ctx, ws, env, stats)
		if ctx.Err() != nil {
			return
		}
		if manifestMatchesStore(s.Store, ws, env, actual) {
			if docs == 0 {
				stats.Healthy++
			}
			return
		}
		// Same revision, different content: republish bumped so revision/ETag
		// watchers actually refetch the repaired index.
		s.republishManifest(ctx, ws, env, expected+1, fmt.Sprintf(
			"manifest r%d entries disagree with the store; docs repaired %d",
			expected, docs), stats)
		return
	}
}

// repairCurrentDocs re-verifies every in-force flag/segment document by
// read-back and idempotently republishes the ones missing or corrupted in
// THIS replica's substrate. Returns the number of documents republished.
func (s *Service) repairCurrentDocs(ctx context.Context, ws *platform.Workspace, env *platform.Environment,
	stats *ReconcileStats) int {

	repaired := 0
	for _, fr := range s.Store.CurrentFlagContents(ws.ID, env.ID) {
		if ctx.Err() != nil {
			break
		}
		raw, err := model.CanonicalJSON(fr.Content)
		if err != nil || model.HashBytes(raw) != fr.Hash {
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"flag %s r%d: stored content does not re-hash to %s — cannot repair",
				fr.Key, fr.Revision, fr.Hash))
			continue
		}
		if _, gotHash, err := s.Config.GetDocContext(ctx, env.NamespaceID, fr.DataID); err == nil && gotHash == fr.Hash {
			continue // readable and correct
		}
		if _, err := s.Config.PublishDocContext(ctx, env.NamespaceID, fr.DataID, raw); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"flag %s r%d: republish: %v", fr.Key, fr.Revision, err))
			continue
		}
		repaired++
		s.logf("reconcile: %s/%s flag doc %s republished", ws.Slug, env.Slug, fr.DataID)
	}
	for _, sr := range s.Store.CurrentSegmentContents(ws.ID, env.ID) {
		if ctx.Err() != nil {
			break
		}
		raw, err := model.CanonicalJSON(sr.Content)
		if err != nil || model.HashBytes(raw) != sr.Hash {
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"segment %s r%d: stored content does not re-hash to %s — cannot repair",
				sr.Key, sr.Revision, sr.Hash))
			continue
		}
		if _, gotHash, err := s.Config.GetDocContext(ctx, env.NamespaceID, sr.DataID); err == nil && gotHash == sr.Hash {
			continue
		}
		if _, err := s.Config.PublishDocContext(ctx, env.NamespaceID, sr.DataID, raw); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf(
				"segment %s r%d: republish: %v", sr.Key, sr.Revision, err))
			continue
		}
		repaired++
		s.logf("reconcile: %s/%s segment doc %s republished", ws.Slug, env.Slug, sr.DataID)
	}
	stats.DocsRepaired += repaired
	return repaired
}

// republishManifest writes the store truth as manifest revision `rev` and
// records it (monotonic). PublishManifest itself confirms by read-back.
func (s *Service) republishManifest(ctx context.Context, ws *platform.Workspace, env *platform.Environment,
	rev int64, reason string, stats *ReconcileStats) {

	m, err := s.buildManifestAt(ws, env, rev)
	if err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf(
			"%s/%s: build manifest r%d: %v", ws.Slug, env.Slug, rev, err))
		return
	}
	if err := s.Config.PublishManifestContext(ctx, env.NamespaceID, m); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf(
			"%s/%s: publish manifest r%d: %v", ws.Slug, env.Slug, rev, err))
		return
	}
	if err := s.Store.SetManifestRevision(ws.ID, env.ID, rev); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf(
			"%s/%s: record manifest r%d: %v", ws.Slug, env.Slug, rev, err))
		return
	}
	s.notify(ws.ID, env.ID, rev)
	s.audit(reconcilerActor, "release.reconcile.repair", ws.ID, env.ID,
		configstore.ManifestDataID, map[string]any{"revision": rev, "reason": reason})
	stats.ManifestsRepaired++
	s.logf("reconcile: %s/%s manifest repaired → r%d (%s)", ws.Slug, env.Slug, rev, reason)
}

// manifestMatchesStore reports whether the manifest's live entries point at
// exactly the store's in-force revisions. Tombstones are ignored: their
// window-based presence can legitimately differ while the store truth is
// unchanged, and clients evict on absence regardless (§3).
func manifestMatchesStore(store *platform.Store, ws *platform.Workspace,
	env *platform.Environment, m *model.Manifest) bool {

	if m == nil {
		return false
	}
	wantFlags := store.CurrentFlagContents(ws.ID, env.ID)
	gotFlags := map[string]model.ManifestEntry{}
	for _, e := range m.Flags {
		if !e.Deleted {
			gotFlags[e.Key] = e
		}
	}
	if len(gotFlags) != len(wantFlags) {
		return false
	}
	for key, fr := range wantFlags {
		e, ok := gotFlags[key]
		if !ok || e.Revision != fr.Revision || e.Hash != fr.Hash || e.DataID != fr.DataID {
			return false
		}
	}
	wantSegs := store.CurrentSegmentContents(ws.ID, env.ID)
	gotSegs := map[string]model.ManifestEntry{}
	for _, e := range m.Segments {
		if !e.Deleted {
			gotSegs[e.Key] = e
		}
	}
	if len(gotSegs) != len(wantSegs) {
		return false
	}
	for key, sr := range wantSegs {
		e, ok := gotSegs[key]
		if !ok || e.Revision != sr.Revision || e.Hash != sr.Hash || e.DataID != sr.DataID {
			return false
		}
	}
	return true
}
