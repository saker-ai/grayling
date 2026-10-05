package platform

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

// Multi-replica primitives (docs/技术方案.md §3 集群, docs/实施状态.md §11
// 发布对账后台任务).
//
// The Store read path is served from the in-memory working set. A single
// replica loads it at open and keeps it authoritative through its own
// commits. With SEVERAL replicas on one PostgreSQL backend that working set
// goes stale the moment another replica commits, so writes that allocate
// monotonic state (publish pipelines: flag revisions, manifest revisions)
// must run under a DATABASE-LEVEL lock with a fresh load inside it:
//
//	release, err := store.PublishLock(ws, env)   // pg_advisory_lock (PG backend)
//	defer release()
//	store.ReloadFromBackend()                    // see every committed change
//	... allocate revisions, publish, commit ...
//
// PublishLock is a no-op on single-process backends (file/memory), where the
// admin service's in-process publishMu already serializes everything.
// The normal PostgreSQL read path converges through SyncChanges; this full
// reload remains required inside publish locks and as an exceptional fallback.

// scopeLocker is implemented by backends that can serialize publishes across
// processes (PostgreSQL advisory locks).
type scopeLocker interface {
	// LockScope blocks until the advisory lock `key` is held and returns
	// the unlock function. The lock lives on a dedicated connection and is
	// released exactly once.
	LockScope(ctx context.Context, key int64) (func(), error)
}

// publishLockTimeout bounds waiting for a cross-process publish lock. The
// publish pipeline is short (document writes + read-backs); a minute of
// queueing already indicates a stuck replica.
const publishLockTimeout = time.Minute

// scopeLockKey derives the advisory-lock key of one (workspace, environment)
// publish scope. FNV-1a 64 keeps it stable across processes; advisory-lock
// collisions across unrelated scopes are harmless (they serialize).
func scopeLockKey(workspaceID, environmentID string) int64 {
	h := fnv.New64a()
	h.Write([]byte("golab-publish\x00"))
	h.Write([]byte(workspaceID))
	h.Write([]byte{0})
	h.Write([]byte(environmentID))
	return int64(h.Sum64())
}

func planLockKey(experimentID string) int64 {
	h := fnv.New64a()
	h.Write([]byte("golab-release-plan\x00"))
	h.Write([]byte(experimentID))
	return int64(h.Sum64())
}

// PlanLock serializes release-plan actions for one experiment across
// replicas. Callers reload the store after acquiring it and release it only
// after the plan update and any corresponding flag publish finish.
func (s *Store) PlanLock(experimentID string) (func(), error) {
	lk, ok := s.persist.(scopeLocker)
	if !ok {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishLockTimeout)
	defer cancel()
	release, err := lk.LockScope(ctx, planLockKey(experimentID))
	if err != nil {
		return nil, fmt.Errorf("platform: plan lock: %w", err)
	}
	return release, nil
}

// PublishLock acquires the cross-process publish lock of one (workspace,
// environment) scope when the backend supports it. The returned function
// releases the lock and is always safe to call exactly once. On
// single-process backends it returns a no-op release.
func (s *Store) PublishLock(workspaceID, environmentID string) (func(), error) {
	lk, ok := s.persist.(scopeLocker)
	if !ok {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishLockTimeout)
	defer cancel()
	release, err := lk.LockScope(ctx, scopeLockKey(workspaceID, environmentID))
	if err != nil {
		return nil, fmt.Errorf("platform: publish lock: %w", err)
	}
	return release, nil
}

// ReloadFromBackend flushes pending changes and atomically swaps the
// in-memory working set with the persisted state. It is the convergence
// primitive for multi-replica deployments: called under PublishLock it
// guarantees revision allocation sees every committed change of every
// replica. Backends without shared state (memory) keep the current working
// set; the file backend re-reads its own dump (harmless single-process).
//
// Readers never observe a torn state: the swap happens under the store write
// lock, and previously handed-out pointers remain valid immutable snapshots.
func (s *Store) ReloadFromBackend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	if s.persist == nil {
		return nil
	}
	// Flush retained changes first (commit-failure replay semantics): a
	// reload must never discard work the backend has not seen.
	if err := s.commitLocked(); err != nil {
		return fmt.Errorf("platform: reload flush: %w", err)
	}
	d, err := s.persist.Load()
	if err != nil {
		return fmt.Errorf("platform: reload: %w", err)
	}
	if d == nil {
		return nil // empty backend — keep the current working set
	}
	s.data = d
	if err := s.normalizeKeyRing(); err != nil {
		return err
	}
	s.syncStats.fullReloads.Add(1)
	s.syncStats.lastReloadNS.Store(time.Since(start).Nanoseconds())
	s.syncStats.versionLag.Store(0)
	return nil
}
