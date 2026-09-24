package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Persistence is the durable backing store behind Store. The in-memory Data
// remains the authoritative working set (every read path is served from it);
// the backend durably records each Store operation and reconstructs the full
// state on Load (docs/技术方案.md §3: 接口面按单事务单元设计 — every Store
// method maps to exactly one Commit).
//
// Implementations must be idempotent under re-application: when Commit fails
// the Store retains the ChangeSet and replays it with the next successful
// Commit, so backends apply absolute state (upserts) or conflict-guarded
// inserts, never blind deltas.
type Persistence interface {
	// Load reconstructs the persisted state. A nil *Data with a nil error
	// means the backend is empty; the Store then initializes fresh state.
	Load() (*Data, error)
	// Commit atomically persists one Store operation: the ChangeSet
	// describes what changed, data carries the current values for dirty
	// keys. On error nothing is persisted and the ChangeSet is retained.
	Commit(data *Data, ch *ChangeSet) error
	Close() error
}

// eventDedup is implemented by backends whose event-ID dedup ledger outlives
// the in-memory retention cap (PostgreSQL event_index). When present,
// IngestEvents consults it so re-delivered events are rejected for the whole
// retention window, not just while their raw record is still in memory.
type eventDedup interface {
	KnownEventIDs(ids []string) (map[string]bool, error)
}

// collection identifies one persisted entity group. Keys are the same scope
// strings the in-memory maps use (surrogate IDs, or ws\x00env\x00key joins).
type collection string

const (
	collWorkspaces        collection = "workspaces"
	collEnvironments      collection = "environments"
	collTokens            collection = "tokens"
	collFlagDrafts        collection = "flagDrafts"
	collFlagRevisions     collection = "flagRevisions"
	collFlagState         collection = "flagState" // current/next revision + tombstone
	collSegmentDrafts     collection = "segmentDrafts"
	collSegmentRevisions  collection = "segmentRevisions"
	collSegmentState      collection = "segmentState"
	collManifestRevisions collection = "manifestRevisions"
	collExperiments       collection = "experiments"
	collAIResources       collection = "aiResources"
	collPinnedVersions    collection = "pinnedVersions"
	collModelAllowlist    collection = "modelAllowlist"
	collAIRevocations     collection = "aiRevocations"
	collQuotas            collection = "quotas"
)

// AggUpsert is one absolute unit-aggregate row touched by an ingest batch
// (§10.2 单元级充分统计). Values are the FINAL in-memory state of the
// (experiment, epoch, variant, unit, metric) tuple, which makes replays
// idempotent.
type AggUpsert struct {
	WorkspaceID   string
	EnvironmentID string
	ExperimentID  string
	Epoch         int64
	Variant       string
	UnitHash      string
	MetricKey     string
	Count         int64
	Sum           float64
	SumSquares    float64
	First         time.Time
	Last          time.Time
}

// CountUpsert is one absolute event-count row (scope × exposure|conversion).
type CountUpsert struct {
	WorkspaceID   string
	EnvironmentID string
	ExperimentID  string
	Epoch         int64
	Variant       string
	Kind          string // "exposure" | "conversion"
	Count         int64
}

// ChangeSet is the durable delta of one Store operation. Dirty marks
// "persist the current value of this key"; Deleted marks removal. Slice-valued
// collections (flag/segment revisions, pinned versions, allowlist) are
// reconciled from data by the backend when their scope key is dirty.
type ChangeSet struct {
	Dirty   map[collection]map[string]struct{}
	Deleted map[collection]map[string]struct{}

	// Events are newly accepted raw events. Backends derive the exposure
	// unit-set rows from them and insert payloads conflict-guarded.
	Events []*Event
	// Aggs / Counts carry absolute aggregate rows touched by this op.
	Aggs   []AggUpsert
	Counts []CountUpsert
	// Audit entries appended by this op (IDs are final).
	Audit []*AuditEntry

	// PruneEventsBefore, when non-zero, applies the retention window:
	// backends delete raw events (and their dedup-ledger rows) with
	// server_time before it — in PostgreSQL by dropping whole time
	// partitions (§10.2: 按保留期分区删除).
	PruneEventsBefore time.Time

	// Rotation signals that the HMAC key ring in data grew: backends with
	// pseudonymized rows re-hash stored unit hashes with the new key layer
	// (密钥轮换迁移路径, §10.2). The ring itself is persisted via Meta.
	Rotation bool
	// Meta signals counter/ring changes (nextCounter, nextAudit, HMAC ring).
	Meta bool
}

func newChangeSet() *ChangeSet {
	return &ChangeSet{
		Dirty:   map[collection]map[string]struct{}{},
		Deleted: map[collection]map[string]struct{}{},
	}
}

func (c *ChangeSet) markDirty(coll collection, key string) {
	if c.Dirty[coll] == nil {
		c.Dirty[coll] = map[string]struct{}{}
	}
	c.Dirty[coll][key] = struct{}{}
}

func (c *ChangeSet) markDeleted(coll collection, key string) {
	if c.Deleted[coll] == nil {
		c.Deleted[coll] = map[string]struct{}{}
	}
	c.Deleted[coll][key] = struct{}{}
	delete(c.Dirty[coll], key)
}

// empty reports whether the ChangeSet describes no durable change at all.
func (c *ChangeSet) empty() bool {
	if c.Meta || c.Rotation || !c.PruneEventsBefore.IsZero() {
		return false
	}
	if len(c.Events) > 0 || len(c.Aggs) > 0 || len(c.Counts) > 0 || len(c.Audit) > 0 {
		return false
	}
	for _, m := range c.Dirty {
		if len(m) > 0 {
			return false
		}
	}
	for _, m := range c.Deleted {
		if len(m) > 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// nop persistence (memory-only stores)
// ---------------------------------------------------------------------------

type nopPersistence struct{}

func (nopPersistence) Load() (*Data, error)              { return nil, nil }
func (nopPersistence) Commit(*Data, *ChangeSet) error    { return nil }
func (nopPersistence) Close() error                      { return nil }

// ---------------------------------------------------------------------------
// File persistence (dev/single-node default): atomic full-state JSON dump —
// the historical golab-store.json format, unchanged.
// ---------------------------------------------------------------------------

type filePersistence struct {
	path string
}

func (f *filePersistence) Load() (*Data, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform: read %s: %w", f.path, err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("platform: parse %s: %w", f.path, err)
	}
	if d.SchemaVersion != dataSchemaVersion {
		return nil, fmt.Errorf("platform: store file schema %d, want %d", d.SchemaVersion, dataSchemaVersion)
	}
	return &d, nil
}

// Commit persists state atomically (tmp file + rename). The ChangeSet is
// ignored: the dump is the whole state, so replays are trivially idempotent.
func (f *filePersistence) Commit(data *Data, _ *ChangeSet) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("platform: encode state: %w", err)
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".golab-store-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil { // contains HMAC key material
		return err
	}
	return os.Rename(tmpName, f.path)
}

func (f *filePersistence) Close() error { return nil }
