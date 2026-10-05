package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChangeSyncMetrics is a snapshot of the cross-replica invalidation path.
type ChangeSyncMetrics struct {
	Applied              int64
	Queries              int64
	Failures             int64
	VersionLag           int64
	MaxNotificationDelay time.Duration
	FullReloads          int64
	LastFullReload       time.Duration
}

type syncMetrics struct {
	applied      atomic.Int64
	queries      atomic.Int64
	failures     atomic.Int64
	versionLag   atomic.Int64
	maxDelayNS   atomic.Int64
	fullReloads  atomic.Int64
	lastReloadNS atomic.Int64
}

func (s *Store) ChangeSyncMetrics() ChangeSyncMetrics {
	return ChangeSyncMetrics{
		Applied: s.syncStats.applied.Load(), Queries: s.syncStats.queries.Load(), Failures: s.syncStats.failures.Load(),
		VersionLag:           s.syncStats.versionLag.Load(),
		MaxNotificationDelay: time.Duration(s.syncStats.maxDelayNS.Load()),
		FullReloads:          s.syncStats.fullReloads.Load(),
		LastFullReload:       time.Duration(s.syncStats.lastReloadNS.Load()),
	}
}

type controlChange struct {
	version int64
	coll    collection
	id      string
	at      time.Time
}

// SyncChanges reconciles committed management versions in order. The durable
// journal is checked even if LISTEN lost a notification. Supported objects are
// fetched by primary key from their canonical tables; less frequent collection
// types use the existing atomic full reload until they gain a dedicated loader.
func (s *Store) SyncChanges() error {
	p, ok := s.persist.(*pgPersistence)
	if !ok {
		return s.ReloadFromBackend()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.commitLocked(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var latest, nextCounter, nextAudit int64
	s.syncStats.queries.Add(1)
	if err := p.pool.QueryRow(ctx, `SELECT control_version, next_counter, next_audit FROM meta WHERE id=1`).Scan(&latest, &nextCounter, &nextAudit); err != nil {
		s.syncStats.failures.Add(1)
		return fmt.Errorf("platform: read control version: %w", err)
	}
	if latest <= s.data.ControlVersion {
		s.syncStats.versionLag.Store(0)
		return nil
	}
	s.syncStats.versionLag.Store(latest - s.data.ControlVersion)
	s.syncStats.queries.Add(1)
	rows, err := p.pool.Query(ctx, `SELECT version, collection, object_id, committed_at FROM control_changes
		WHERE version > $1 AND version <= $2 ORDER BY version, collection, object_id LIMIT 4096`, s.data.ControlVersion, latest)
	if err != nil {
		s.syncStats.failures.Add(1)
		return err
	}
	var changes []controlChange
	for rows.Next() {
		var change controlChange
		if err := rows.Scan(&change.version, &change.coll, &change.id, &change.at); err != nil {
			rows.Close()
			s.syncStats.failures.Add(1)
			return err
		}
		decoded, err := base64.RawURLEncoding.DecodeString(change.id)
		if err != nil {
			rows.Close()
			s.syncStats.failures.Add(1)
			return err
		}
		change.id = string(decoded)
		changes = append(changes, change)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.syncStats.failures.Add(1)
		return err
	}
	if len(changes) == 0 || len(changes) == 4096 || changes[0].version != s.data.ControlVersion+1 {
		return s.fullReloadLocked()
	}
	// A version can contain several objects. Do not advance past an incomplete
	// version; a journal gap falls back to a verified full load.
	for i, change := range changes {
		if i > 0 && change.version > changes[i-1].version+1 {
			return s.fullReloadLocked()
		}
		if incrementalCollection(change.coll) {
			s.syncStats.queries.Add(1)
		}
		if err := p.applyControlObject(ctx, s.data, change); err != nil {
			s.syncStats.failures.Add(1)
			return fmt.Errorf("platform: apply %s/%s at v%d: %w", change.coll, change.id, change.version, err)
		}
		if change.coll == "meta" || !incrementalCollection(change.coll) {
			return s.fullReloadLocked()
		}
		s.syncStats.applied.Add(1)
		delay := time.Since(change.at).Nanoseconds()
		for old := s.syncStats.maxDelayNS.Load(); delay > old; old = s.syncStats.maxDelayNS.Load() {
			if s.syncStats.maxDelayNS.CompareAndSwap(old, delay) {
				break
			}
		}
	}
	s.data.ControlVersion = changes[len(changes)-1].version
	s.data.NextCounter = nextCounter
	s.data.NextAudit = nextAudit
	s.syncStats.versionLag.Store(max(0, latest-s.data.ControlVersion))
	return nil
}

func incrementalCollection(coll collection) bool {
	switch coll {
	case collWorkspaces, collEnvironments, collTokens, collExperiments,
		collQuotas, collAIResources, collAIRevocations, collPinnedVersions, collModelAllowlist, "audit":
		return true
	default:
		return false
	}
}

func (s *Store) fullReloadLocked() error {
	start := time.Now()
	d, err := s.persist.Load()
	if err != nil {
		s.syncStats.failures.Add(1)
		return err
	}
	if d != nil {
		s.data = d
	}
	s.syncStats.fullReloads.Add(1)
	s.syncStats.lastReloadNS.Store(time.Since(start).Nanoseconds())
	s.syncStats.versionLag.Store(0)
	return nil
}

func (p *pgPersistence) applyControlObject(ctx context.Context, d *Data, c controlChange) error {
	switch c.coll {
	case collWorkspaces:
		w := &Workspace{}
		err := p.pool.QueryRow(ctx, `SELECT id, slug, name, created_at FROM workspaces WHERE id=$1`, c.id).
			Scan(&w.ID, &w.Slug, &w.Name, &w.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.Workspaces, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		d.Workspaces[c.id] = w
	case collEnvironments:
		if old := d.Environments[c.id]; old != nil {
			delete(d.EnvBySlugKey, old.WorkspaceID+"/"+old.Slug)
		}
		e := &Environment{}
		err := p.pool.QueryRow(ctx, `SELECT id, workspace_id, slug, name, namespace_id, created_at FROM environments WHERE id=$1`, c.id).
			Scan(&e.ID, &e.WorkspaceID, &e.Slug, &e.Name, &e.NamespaceID, &e.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.Environments, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		d.Environments[c.id] = e
		d.EnvBySlugKey[e.WorkspaceID+"/"+e.Slug] = e.ID
	case collTokens:
		if old := d.Tokens[c.id]; old != nil {
			delete(d.TokensByHash, old.SecretHash)
		}
		t := &Token{}
		var kind, role string
		var lastUsed *time.Time
		err := p.pool.QueryRow(ctx, `SELECT id, name, kind, role, workspace_id, environment_id,
			secret_hash, prefix, created_at, last_used_at, revoked, created_by FROM tokens WHERE id=$1`, c.id).
			Scan(&t.ID, &t.Name, &kind, &role, &t.WorkspaceID, &t.EnvironmentID,
				&t.SecretHash, &t.Prefix, &t.CreatedAt, &lastUsed, &t.Revoked, &t.CreatedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.Tokens, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		t.Kind, t.Role = TokenKind(kind), Role(role)
		if lastUsed != nil {
			t.LastUsedAt = *lastUsed
		}
		d.Tokens[c.id] = t
		d.TokensByHash[t.SecretHash] = t.ID
	case collExperiments:
		var doc []byte
		err := p.pool.QueryRow(ctx, `SELECT doc FROM experiments WHERE id=$1`, c.id).Scan(&doc)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.Experiments, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		e := &Experiment{}
		if err := json.Unmarshal(doc, e); err != nil {
			return err
		}
		d.Experiments[c.id] = e
	case collQuotas:
		var doc []byte
		err := p.pool.QueryRow(ctx, `SELECT doc FROM quotas WHERE workspace_id=$1`, c.id).Scan(&doc)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.Quotas, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		var q Quotas
		if err := json.Unmarshal(doc, &q); err != nil {
			return err
		}
		d.Quotas[c.id] = q
	case collAIResources:
		ws, env, typ, logical, err := splitScope4(c.id)
		if err != nil {
			return err
		}
		r := &AIResourceRef{}
		err = p.pool.QueryRow(ctx, `SELECT workspace_id, environment_id, type, logical_id,
			physical_id, owner, created_at FROM ai_resources WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4`,
			ws, env, typ, logical).Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Type, &r.LogicalID,
			&r.PhysicalID, &r.Owner, &r.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.AIResources, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		d.AIResources[c.id] = r
	case collAIRevocations:
		ws, env, typ, logical, err := splitScope4(c.id)
		if err != nil {
			return err
		}
		var reason string
		err = p.pool.QueryRow(ctx, `SELECT reason FROM ai_revocations WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4`,
			ws, env, typ, logical).Scan(&reason)
		if errors.Is(err, pgx.ErrNoRows) {
			delete(d.AIRevocations, c.id)
			return nil
		}
		if err != nil {
			return err
		}
		d.AIRevocations[c.id] = reason
	case collPinnedVersions:
		ws, env, typ, logical, err := splitScope4(c.id)
		if err != nil {
			return err
		}
		rows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, type, logical_id,
			physical_id, version, digest, pinned_at, pinned_by FROM pinned_versions
			WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4 ORDER BY seq`, ws, env, typ, logical)
		if err != nil {
			return err
		}
		var pins []*PinnedVersion
		for rows.Next() {
			pin := &PinnedVersion{}
			if err := rows.Scan(&pin.WorkspaceID, &pin.EnvironmentID, &pin.Type, &pin.LogicalID,
				&pin.PhysicalID, &pin.Version, &pin.Digest, &pin.PinnedAt, &pin.PinnedBy); err != nil {
				rows.Close()
				return err
			}
			pins = append(pins, pin)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			delete(d.PinnedVersions, c.id)
		} else {
			d.PinnedVersions[c.id] = pins
		}
	case collModelAllowlist:
		rows, err := p.pool.Query(ctx, `SELECT workspace_id, provider, model, params, enabled, created_at, created_by
			FROM model_allowlist WHERE workspace_id=$1 ORDER BY ord`, c.id)
		if err != nil {
			return err
		}
		var entries []ModelAllowEntry
		for rows.Next() {
			var e ModelAllowEntry
			var params []byte
			if err := rows.Scan(&e.WorkspaceID, &e.Provider, &e.Model, &params, &e.Enabled, &e.CreatedAt, &e.CreatedBy); err != nil {
				rows.Close()
				return err
			}
			if len(params) > 0 {
				if err := json.Unmarshal(params, &e.Params); err != nil {
					rows.Close()
					return err
				}
			}
			entries = append(entries, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			delete(d.ModelAllowlist, c.id)
		} else {
			d.ModelAllowlist[c.id] = entries
		}
	case "audit":
		id, err := strconv.ParseInt(c.id, 10, 64)
		if err != nil {
			return err
		}
		entry := &AuditEntry{}
		err = p.pool.QueryRow(ctx, `SELECT id, time, actor, actor_role, action, workspace_id,
			environment_id, object, detail FROM audit WHERE id=$1`, id).
			Scan(&entry.ID, &entry.Time, &entry.Actor, &entry.ActorRole, &entry.Action,
				&entry.WorkspaceID, &entry.EnvironmentID, &entry.Object, &entry.Detail)
		if err != nil {
			return err
		}
		index := sort.Search(len(d.Audit), func(i int) bool { return d.Audit[i].ID >= id })
		if index == len(d.Audit) {
			d.Audit = append(d.Audit, entry)
		} else if d.Audit[index].ID != id {
			d.Audit = append(d.Audit, nil)
			copy(d.Audit[index+1:], d.Audit[index:])
			d.Audit[index] = entry
		}
	}
	return nil
}

// RunChangeSync listens for hints and checks the durable sequence on every
// poll. A broken LISTEN connection is retried; polling continues meanwhile.
func (s *Store) RunChangeSync(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	p, ok := s.persist.(*pgPersistence)
	if !ok {
		return
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for ctx.Err() == nil {
		connectCtx, connectCancel := context.WithTimeout(ctx, interval)
		conn, err := pgx.Connect(connectCtx, p.pool.Config().ConnConfig.ConnString())
		if err == nil {
			_, err = conn.Exec(connectCtx, `LISTEN golab_control`)
		}
		connectCancel()
		if err != nil {
			if conn != nil {
				_ = conn.Close(context.Background())
			}
			if logf != nil {
				logf("control sync: listen: %v", err)
			}
			if syncErr := s.SyncChanges(); syncErr != nil && logf != nil {
				logf("control sync: poll: %v", syncErr)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			continue
		}
		if err := s.SyncChanges(); err != nil && logf != nil {
			logf("control sync: initial: %v", err)
		}
		for ctx.Err() == nil {
			waitCtx, cancel := context.WithTimeout(ctx, interval)
			_, err = conn.WaitForNotification(waitCtx)
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				break
			}
			if syncErr := s.SyncChanges(); syncErr != nil && logf != nil {
				logf("control sync: reconcile: %v", syncErr)
			}
		}
		_ = conn.Close(context.Background())
	}
}
