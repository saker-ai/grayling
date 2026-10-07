package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgPersistence is the production PostgreSQL backend (docs/技术方案.md §3,
// §10.2). Design:
//
//   - Every Store operation commits as ONE transaction (单事务单元). Control
//     records use absolute-state upserts; events use ledger-guarded inserts
//     and increments, so a retained ChangeSet can be replayed safely.
//   - Raw events live in a monthly RANGE-partitioned table; retention
//     pruning drops whole partitions (按保留期分区删除) plus a boundary
//     delete, and shrinks the global dedup ledger (event_index) to match.
//   - Unit-level sufficient statistics (N, Σx, Σx²), exposed-unit sets and
//     event counts are real aggregate tables. Event aggregates are updated
//     from ledger-accepted rows inside the same transaction, so concurrent
//     collectors cannot overwrite one another's increments.
//   - HMAC pseudonymization keys form a versioned ring (hmac_keys).
//     RotateHMACKey appends a version and re-hashes every stored pseudonym
//     through the new layer inside one transaction (密钥轮换迁移路径); the
//     hash-chain property keeps unit identity linkable across rotations.
//
// Most reads use the in-memory working set; token authentication queries
// PostgreSQL directly so cross-replica revocation is immediately enforced.
// Management writes compare the loaded control version in the database.
type pgPersistence struct {
	pool            *pgxpool.Pool
	lastAcceptedIDs map[string]bool
}

// pgCommitTimeout bounds one Store-operation transaction.
const pgCommitTimeout = 2 * time.Minute

// pgLoadTimeout bounds the full-state load at open.
const pgLoadTimeout = 5 * time.Minute

func newPGPersistence(ctx context.Context, dsn string) (*pgPersistence, error) {
	if dsn == "" {
		return nil, errors.New("platform: empty PostgreSQL DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("platform: parse postgres DSN: %w", err)
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 8
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "golab"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("platform: postgres pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("platform: postgres unreachable: %w", err)
	}
	p := &pgPersistence{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *pgPersistence) Close() error {
	if p.pool != nil {
		p.pool.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schema migrations
// ---------------------------------------------------------------------------

type pgMigration struct {
	version int
	sql     string
}

var pgMigrations = []pgMigration{
	{1, pgSchemaV1},
	{2, `ALTER TABLE meta ADD COLUMN control_version bigint NOT NULL DEFAULT 0`},
	{3, `CREATE TABLE control_changes (
        version bigint NOT NULL,
        collection text NOT NULL,
        object_id text NOT NULL,
        committed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
        PRIMARY KEY (version, collection, object_id)
    )`},
	{4, `ALTER TABLE flag_drafts
        ADD COLUMN submitted_token_id text NOT NULL DEFAULT '',
        ADD COLUMN submitted_hash text NOT NULL DEFAULT '',
        ADD COLUMN approved_token_id text NOT NULL DEFAULT '',
        ADD COLUMN approved_hash text NOT NULL DEFAULT '',
        ADD COLUMN approval_expires_at timestamptz;
        ALTER TABLE segment_drafts
        ADD COLUMN submitted_token_id text NOT NULL DEFAULT '',
        ADD COLUMN submitted_at timestamptz,
        ADD COLUMN submitted_hash text NOT NULL DEFAULT '',
        ADD COLUMN approved_token_id text NOT NULL DEFAULT '',
        ADD COLUMN approved_at timestamptz,
        ADD COLUMN approved_hash text NOT NULL DEFAULT '',
        ADD COLUMN approval_expires_at timestamptz`},
	{5, `CREATE TABLE approvals (
        id text PRIMARY KEY,
        workspace_id text NOT NULL,
        doc jsonb NOT NULL
    );
    CREATE INDEX approvals_workspace_id_idx ON approvals(workspace_id)`},
	{6, `ALTER TABLE events
        ADD COLUMN policy_version text NOT NULL DEFAULT '',
        ADD COLUMN state_version bigint NOT NULL DEFAULT 0,
        ADD COLUMN allocation_probability double precision NOT NULL DEFAULT 0`},
	{7, `CREATE TABLE bandit_assignments (
        experiment_id text NOT NULL,
        epoch bigint NOT NULL,
        unit_hash text NOT NULL,
        variant text NOT NULL,
        source text NOT NULL,
        policy_version text NOT NULL,
        state_version bigint NOT NULL,
        allocation_probability double precision NOT NULL,
        PRIMARY KEY (experiment_id, epoch, unit_hash)
    )`},
	{8, `CREATE TABLE rollout_leader (
        id boolean PRIMARY KEY DEFAULT true CHECK (id),
        holder text NOT NULL,
        generation bigint NOT NULL,
        expires_at timestamptz NOT NULL,
        last_run_at timestamptz,
        last_takeover_at timestamptz,
        last_error text NOT NULL DEFAULT ''
    )`},
}

const pgSchemaV1 = `
CREATE TABLE meta (
    id             int PRIMARY KEY,
    next_counter   bigint NOT NULL,
    next_audit     bigint NOT NULL,
    schema_version int NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE hmac_keys (
    version    int PRIMARY KEY,
    key_hex    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspaces (
    id         text PRIMARY KEY,
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);

CREATE TABLE environments (
    id           text PRIMARY KEY,
    workspace_id text NOT NULL,
    slug         text NOT NULL,
    name         text NOT NULL DEFAULT '',
    namespace_id text NOT NULL,
    created_at   timestamptz NOT NULL,
    UNIQUE (workspace_id, slug)
);

CREATE TABLE tokens (
    id             text PRIMARY KEY,
    name           text NOT NULL,
    kind           text NOT NULL,
    role           text NOT NULL DEFAULT '',
    workspace_id   text NOT NULL DEFAULT '',
    environment_id text NOT NULL DEFAULT '',
    secret_hash    text NOT NULL UNIQUE,
    prefix         text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    last_used_at   timestamptz,
    revoked        boolean NOT NULL DEFAULT false,
    created_by     text NOT NULL DEFAULT ''
);

CREATE TABLE flag_drafts (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    key            text NOT NULL,
    content        jsonb NOT NULL,
    state          text NOT NULL,
    risk           text NOT NULL,
    updated_by     text NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL,
    submitted_by   text NOT NULL DEFAULT '',
    submitted_at   timestamptz,
    approved_by    text NOT NULL DEFAULT '',
    approved_at    timestamptz,
    review_note    text NOT NULL DEFAULT '',
    deleted        boolean NOT NULL DEFAULT false,
    base_revision  bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, environment_id, key)
);

CREATE TABLE flag_revisions (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    key            text NOT NULL,
    revision       bigint NOT NULL,
    content        jsonb NOT NULL,
    hash           text NOT NULL,
    data_id        text NOT NULL,
    published_at   timestamptz NOT NULL,
    published_by   text NOT NULL DEFAULT '',
    note           text NOT NULL DEFAULT '',
    rollback_of    bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, environment_id, key, revision)
);

CREATE TABLE flag_state (
    workspace_id           text NOT NULL,
    environment_id         text NOT NULL,
    key                    text NOT NULL,
    current_revision       bigint NOT NULL DEFAULT 0,
    next_revision          bigint NOT NULL DEFAULT 0,
    tombstone_manifest_rev bigint,
    PRIMARY KEY (workspace_id, environment_id, key)
);

CREATE TABLE segment_drafts (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    key            text NOT NULL,
    content        jsonb NOT NULL,
    state          text NOT NULL,
    risk           text NOT NULL,
    updated_by     text NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL,
    submitted_by   text NOT NULL DEFAULT '',
    approved_by    text NOT NULL DEFAULT '',
    base_revision  bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, environment_id, key)
);

CREATE TABLE segment_revisions (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    key            text NOT NULL,
    revision       bigint NOT NULL,
    content        jsonb NOT NULL,
    hash           text NOT NULL,
    data_id        text NOT NULL,
    published_at   timestamptz NOT NULL,
    published_by   text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, environment_id, key, revision)
);

CREATE TABLE segment_state (
    workspace_id     text NOT NULL,
    environment_id   text NOT NULL,
    key              text NOT NULL,
    current_revision bigint NOT NULL DEFAULT 0,
    next_revision    bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, environment_id, key)
);

CREATE TABLE env_state (
    workspace_id      text NOT NULL,
    environment_id    text NOT NULL,
    manifest_revision bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (workspace_id, environment_id)
);

CREATE TABLE experiments (
    id             text PRIMARY KEY,
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    flag_key       text NOT NULL,
    epoch          bigint NOT NULL,
    state          text NOT NULL,
    doc            jsonb NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);
CREATE INDEX experiments_scope_idx ON experiments (workspace_id, environment_id);

CREATE TABLE event_index (
    event_id    text PRIMARY KEY,
    server_time timestamptz NOT NULL
);

CREATE TABLE events (
    event_id       text NOT NULL,
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    type           text NOT NULL,
    experiment_id  text NOT NULL,
    epoch          bigint NOT NULL,
    flag_key       text NOT NULL DEFAULT '',
    flag_revision  bigint NOT NULL DEFAULT 0,
    variant        text NOT NULL,
    unit_hash      text NOT NULL,
    session_id     text NOT NULL DEFAULT '',
    task_id        text NOT NULL DEFAULT '',
    source         text NOT NULL DEFAULT '',
    metric_key     text NOT NULL DEFAULT '',
    value          double precision NOT NULL DEFAULT 0,
    client_time    timestamptz NOT NULL,
    server_time    timestamptz NOT NULL,
    PRIMARY KEY (server_time, event_id)
) PARTITION BY RANGE (server_time);
CREATE INDEX events_experiment_idx ON events (workspace_id, environment_id, experiment_id, epoch);
CREATE INDEX events_unit_idx ON events (workspace_id, unit_hash, type);

CREATE TABLE exposed_units (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    experiment_id  text NOT NULL,
    epoch          bigint NOT NULL,
    variant        text NOT NULL,
    unit_hash      text NOT NULL,
    first_at       timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash)
);

CREATE TABLE split_exposed_units (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    experiment_id  text NOT NULL,
    epoch          bigint NOT NULL,
    variant        text NOT NULL,
    unit_hash      text NOT NULL,
    first_at       timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash)
);

CREATE TABLE unit_aggs (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    experiment_id  text NOT NULL,
    epoch          bigint NOT NULL,
    variant        text NOT NULL,
    unit_hash      text NOT NULL,
    metric_key     text NOT NULL,
    cnt            bigint NOT NULL,
    sum_val        double precision NOT NULL,
    sum_sq         double precision NOT NULL,
    first_at       timestamptz NOT NULL,
    last_at        timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash, metric_key)
);

CREATE TABLE event_counts (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    experiment_id  text NOT NULL,
    epoch          bigint NOT NULL,
    variant        text NOT NULL,
    kind           text NOT NULL,
    cnt            bigint NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, experiment_id, epoch, variant, kind)
);

CREATE TABLE audit (
    id             bigint PRIMARY KEY,
    time           timestamptz NOT NULL,
    actor          text NOT NULL DEFAULT '',
    actor_role     text NOT NULL DEFAULT '',
    action         text NOT NULL,
    workspace_id   text NOT NULL DEFAULT '',
    environment_id text NOT NULL DEFAULT '',
    object         text NOT NULL DEFAULT '',
    detail         text NOT NULL DEFAULT ''
);

CREATE TABLE ai_resources (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    type           text NOT NULL,
    logical_id     text NOT NULL,
    physical_id    text NOT NULL UNIQUE,
    owner          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, environment_id, type, logical_id)
);

CREATE TABLE pinned_versions (
    seq            bigserial,
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    type           text NOT NULL,
    logical_id     text NOT NULL,
    physical_id    text NOT NULL,
    version        text NOT NULL,
    digest         text NOT NULL,
    pinned_at      timestamptz NOT NULL,
    pinned_by      text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, environment_id, type, logical_id, version)
);

CREATE TABLE model_allowlist (
    ord          bigserial,
    workspace_id text NOT NULL,
    provider     text NOT NULL,
    model        text NOT NULL,
    params       jsonb,
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL,
    created_by   text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, provider, model)
);

CREATE TABLE ai_revocations (
    workspace_id   text NOT NULL,
    environment_id text NOT NULL,
    type           text NOT NULL,
    logical_id     text NOT NULL,
    reason         text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, environment_id, type, logical_id)
);

CREATE TABLE quotas (
    workspace_id text PRIMARY KEY,
    doc          jsonb NOT NULL
);
`

func (p *pgPersistence) migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pgCommitTimeout)
	defer cancel()
	release, err := p.LockScope(ctx, 0x676f6c61622d6462)
	if err != nil {
		return fmt.Errorf("platform: migration lock: %w", err)
	}
	defer release()
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("platform: init schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := p.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("platform: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	for _, m := range pgMigrations {
		if applied[m.version] {
			continue
		}
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("platform: migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("platform: record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("platform: commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Commit
// ---------------------------------------------------------------------------

func (p *pgPersistence) Commit(data *Data, ch *ChangeSet) error {
	if ch.empty() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgCommitTimeout)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	control := ch.Meta || ch.Rotation || len(ch.Audit) > 0
	for _, keys := range ch.Dirty {
		control = control || len(keys) > 0
	}
	for _, keys := range ch.Deleted {
		control = control || len(keys) > 0
	}
	if control {
		if err := p.claimControlVersion(ctx, tx, data); err != nil {
			return err
		}
	}

	// Rotation BEFORE meta: it detects missing ring versions, and meta then
	// persists the ring rows idempotently.
	if ch.Rotation {
		if err := p.applyRotation(ctx, tx, data); err != nil {
			return pgConflict(err)
		}
	}
	if ch.Meta {
		if err := p.commitMeta(ctx, tx, data); err != nil {
			return err
		}
	}
	if err := p.commitDirty(ctx, tx, data, ch); err != nil {
		return pgConflict(err)
	}
	if err := p.commitDeleted(ctx, tx, ch); err != nil {
		return pgConflict(err)
	}
	accepted := map[string]bool{}
	if len(ch.Events) > 0 {
		accepted, err = p.commitEvents(ctx, tx, ch.Events)
		if err != nil {
			return err
		}
	}
	if len(ch.Aggs) > 0 && len(ch.Events) == 0 {
		if err := p.commitAggs(ctx, tx, ch.Aggs); err != nil {
			return err
		}
	}
	if len(ch.Counts) > 0 && len(ch.Events) == 0 {
		if err := p.commitCounts(ctx, tx, ch.Counts); err != nil {
			return err
		}
	}
	if len(ch.Audit) > 0 {
		if err := p.commitAudit(ctx, tx, ch.Audit); err != nil {
			return pgConflict(err)
		}
	}
	if control {
		if err := p.commitChanges(ctx, tx, data.ControlVersion+1, ch); err != nil {
			return err
		}
	}
	if !ch.PruneEventsBefore.IsZero() {
		if err := p.pruneEvents(ctx, tx, ch.PruneEventsBefore); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pgConflict(err)
	}
	if control {
		data.ControlVersion++
	}
	p.lastAcceptedIDs = accepted
	return nil
}

// commitChanges records the exact objects modified by this transaction. The
// notification is only a hint; consumers query control_changes and then the
// canonical entity tables. A meta-only operation still advances the cursor.
func (p *pgPersistence) commitChanges(ctx context.Context, tx pgx.Tx, version int64, ch *ChangeSet) error {
	batch := &pgx.Batch{}
	count := 0
	for coll, keys := range ch.Dirty {
		for key := range keys {
			batch.Queue(`INSERT INTO control_changes (version, collection, object_id) VALUES ($1,$2,$3)`, version, string(coll), changeKey(key))
			count++
		}
	}
	for coll, keys := range ch.Deleted {
		for key := range keys {
			batch.Queue(`INSERT INTO control_changes (version, collection, object_id) VALUES ($1,$2,$3)`, version, string(coll), changeKey(key))
			count++
		}
	}
	for _, entry := range ch.Audit {
		batch.Queue(`INSERT INTO control_changes (version, collection, object_id) VALUES ($1,'audit',$2)`, version, changeKey(strconv.FormatInt(entry.ID, 10)))
		count++
	}
	if count == 0 {
		batch.Queue(`INSERT INTO control_changes (version, collection, object_id) VALUES ($1,'meta','')`, version)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("platform: persist control changes: %w", err)
	}
	// Hints contain only type, ID and version; state always comes from tables.
	for coll, keys := range ch.Dirty {
		for key := range keys {
			if err := p.notifyChange(ctx, tx, version, string(coll), key); err != nil {
				return err
			}
		}
	}
	for coll, keys := range ch.Deleted {
		for key := range keys {
			if err := p.notifyChange(ctx, tx, version, string(coll), key); err != nil {
				return err
			}
		}
	}
	for _, entry := range ch.Audit {
		if err := p.notifyChange(ctx, tx, version, "audit", strconv.FormatInt(entry.ID, 10)); err != nil {
			return err
		}
	}
	if count == 0 {
		return p.notifyChange(ctx, tx, version, "meta", "")
	}
	return nil
}

func (p *pgPersistence) notifyChange(ctx context.Context, tx pgx.Tx, version int64, coll, key string) error {
	payload, err := json.Marshal(struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{coll, changeKey(key), version})
	if err != nil {
		return err
	}
	// A hint must never make an otherwise valid management transaction fail.
	// The durable journal and periodic version check cover oversized keys.
	if len(payload) > 7900 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('golab_control', $1)`, string(payload)); err != nil {
		return fmt.Errorf("platform: notify control change: %w", err)
	}
	return nil
}

// PostgreSQL text and NOTIFY payloads cannot contain NUL, while scoped
// collection keys deliberately use NUL separators.
func changeKey(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

func pgConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "40001", "40P01":
			return fmt.Errorf("%w: PostgreSQL write conflict (%s); reload and retry: %v", ErrConflict, pgErr.Code, err)
		}
	}
	return err
}

func (p *pgPersistence) claimControlVersion(ctx context.Context, tx pgx.Tx, data *Data) error {
	if _, err := tx.Exec(ctx, `INSERT INTO meta (id, next_counter, next_audit, schema_version, control_version)
		VALUES (1, 1, 0, $1, 0) ON CONFLICT (id) DO NOTHING`, dataSchemaVersion); err != nil {
		return pgConflict(err)
	}
	result, err := tx.Exec(ctx, `UPDATE meta SET next_counter=$1, next_audit=$2,
		control_version=control_version+1, updated_at=now()
		WHERE id=1 AND control_version=$3`, data.NextCounter, data.NextAudit, data.ControlVersion)
	if err != nil {
		return pgConflict(err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("%w: management state changed on another replica; reload and retry", ErrConflict)
	}
	return nil
}

func (p *pgPersistence) commitMeta(ctx context.Context, tx pgx.Tx, data *Data) error {
	batch := &pgx.Batch{}
	for i, key := range data.HMACKeyRing {
		batch.Queue(`INSERT INTO hmac_keys (version, key_hex) VALUES ($1, $2)
                     ON CONFLICT (version) DO NOTHING`, i+1, key)
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("platform: persist hmac ring: %w", err)
		}
	}
	return nil
}

// applyRotation is the key-rotation migration: for every ring version not
// yet recorded, re-hash all stored pseudonyms through that key layer and
// record the version — atomically, in ring order.
func (p *pgPersistence) applyRotation(ctx context.Context, tx pgx.Tx, data *Data) error {
	existing := map[int]bool{}
	rows, err := tx.Query(ctx, `SELECT version FROM hmac_keys`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		existing[v] = true
	}
	rows.Close()
	for i, key := range data.HMACKeyRing {
		version := i + 1
		if existing[version] {
			continue
		}
		if version > 1 {
			if err := p.rehashStored(ctx, tx, key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO hmac_keys (version, key_hex) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING`,
			version, key); err != nil {
			return err
		}
	}
	return nil
}

// rehashStored migrates every stored unit pseudonym through one new key
// layer: new = hex(HMAC-SHA256(key, ws || 0x00 || old)) — byte-identical to
// hashUnitLayer in memory. The remap is computed in Go (no pgcrypto
// dependency) and applied set-based via a temp table.
func (p *pgPersistence) rehashStored(ctx context.Context, tx pgx.Tx, keyHex string) error {
	rows, err := tx.Query(ctx, `
        SELECT DISTINCT workspace_id, unit_hash FROM events
        UNION SELECT DISTINCT workspace_id, session_id FROM events WHERE session_id <> ''
        UNION SELECT workspace_id, unit_hash FROM exposed_units
        UNION SELECT workspace_id, unit_hash FROM split_exposed_units
        UNION SELECT workspace_id, unit_hash FROM unit_aggs
        UNION SELECT e.workspace_id, b.unit_hash FROM bandit_assignments b
            JOIN experiments e ON e.id = b.experiment_id`)
	if err != nil {
		return fmt.Errorf("platform: rotation scan: %w", err)
	}
	type remap struct{ ws, old, new string }
	var remaps []remap
	for rows.Next() {
		var ws, old string
		if err := rows.Scan(&ws, &old); err != nil {
			rows.Close()
			return err
		}
		remaps = append(remaps, remap{ws, old, hashUnitLayer(keyHex, ws, old)})
	}
	rows.Close()
	if len(remaps) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE _remap (
        workspace_id text NOT NULL, old_hash text NOT NULL, new_hash text NOT NULL,
        PRIMARY KEY (workspace_id, old_hash)) ON COMMIT DROP`); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, r := range remaps {
		batch.Queue(`INSERT INTO _remap (workspace_id, old_hash, new_hash)
		             VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, r.ws, r.old, r.new)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("platform: rotation remap upload: %w", err)
	}
	for _, stmt := range []string{
		`UPDATE events e SET unit_hash = r.new_hash
		   FROM _remap r WHERE e.workspace_id = r.workspace_id AND e.unit_hash = r.old_hash`,
		`UPDATE events e SET session_id = r.new_hash
		   FROM _remap r WHERE e.workspace_id = r.workspace_id AND e.session_id = r.old_hash`,
		`UPDATE exposed_units u SET unit_hash = r.new_hash
		   FROM _remap r WHERE u.workspace_id = r.workspace_id AND u.unit_hash = r.old_hash`,
		`UPDATE split_exposed_units u SET unit_hash = r.new_hash
		   FROM _remap r WHERE u.workspace_id = r.workspace_id AND u.unit_hash = r.old_hash`,
		`UPDATE unit_aggs a SET unit_hash = r.new_hash
		   FROM _remap r WHERE a.workspace_id = r.workspace_id AND a.unit_hash = r.old_hash`,
		`UPDATE bandit_assignments b SET unit_hash = r.new_hash
		   FROM _remap r, experiments e
		   WHERE b.experiment_id = e.id AND e.workspace_id = r.workspace_id AND b.unit_hash = r.old_hash`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("platform: rotation rehash: %w", err)
		}
	}
	return nil
}

func splitScope2(key string) (string, string, error) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("platform: bad scope key %q", key)
	}
	return parts[0], parts[1], nil
}

func splitScope3(key string) (string, string, string, error) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("platform: bad scope key %q", key)
	}
	return parts[0], parts[1], parts[2], nil
}

func splitScope4(key string) (string, string, string, string, error) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("platform: bad AI key %q", key)
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func (p *pgPersistence) commitDirty(ctx context.Context, tx pgx.Tx, data *Data, ch *ChangeSet) error {
	batch := &pgx.Batch{}
	// reconcileRevisions marks scopes whose immutable revision list must be
	// diffed against storage (appends AND prunes).
	var flagRevScopes, segRevScopes []string
	for coll, keys := range ch.Dirty {
		for key := range keys {
			switch coll {
			case collWorkspaces:
				w := data.Workspaces[key]
				if w == nil {
					continue
				}
				batch.Queue(`INSERT INTO workspaces (id, slug, name, created_at)
                    VALUES ($1,$2,$3,$4)
                    ON CONFLICT (id) DO UPDATE SET slug = EXCLUDED.slug, name = EXCLUDED.name`,
					w.ID, w.Slug, w.Name, w.CreatedAt)
			case collEnvironments:
				e := data.Environments[key]
				if e == nil {
					continue
				}
				batch.Queue(`INSERT INTO environments
                    (id, workspace_id, slug, name, namespace_id, created_at)
                    VALUES ($1,$2,$3,$4,$5,$6)
                    ON CONFLICT (id) DO UPDATE SET
                    workspace_id = EXCLUDED.workspace_id, slug = EXCLUDED.slug,
                    name = EXCLUDED.name, namespace_id = EXCLUDED.namespace_id`,
					e.ID, e.WorkspaceID, e.Slug, e.Name, e.NamespaceID, e.CreatedAt)
			case collTokens:
				t := data.Tokens[key]
				if t == nil {
					continue
				}
				batch.Queue(`INSERT INTO tokens
                    (id, name, kind, role, workspace_id, environment_id, secret_hash,
                     prefix, created_at, last_used_at, revoked, created_by)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
                    ON CONFLICT (id) DO UPDATE SET
                     name = EXCLUDED.name, kind = EXCLUDED.kind, role = EXCLUDED.role,
                     workspace_id = EXCLUDED.workspace_id, environment_id = EXCLUDED.environment_id,
                     secret_hash = EXCLUDED.secret_hash, prefix = EXCLUDED.prefix,
                     last_used_at = EXCLUDED.last_used_at, revoked = EXCLUDED.revoked`,
					t.ID, t.Name, string(t.Kind), string(t.Role), t.WorkspaceID, t.EnvironmentID,
					t.SecretHash, t.Prefix, t.CreatedAt, nullableTime(t.LastUsedAt), t.Revoked, t.CreatedBy)
			case collApprovals:
				a := data.Approvals[key]
				if a == nil {
					continue
				}
				doc, err := json.Marshal(a)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO approvals(id, workspace_id, doc) VALUES ($1,$2,$3)
                    ON CONFLICT (id) DO UPDATE SET doc=EXCLUDED.doc`, key, a.WorkspaceID, doc)
			case collFlagDrafts:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				d := data.FlagDrafts[key]
				if d == nil {
					continue
				}
				content, err := json.Marshal(d.Content)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO flag_drafts
                    (workspace_id, environment_id, key, content, state, risk, updated_by,
                     updated_at, submitted_by, submitted_at, approved_by, approved_at,
                     review_note, deleted, base_revision, submitted_token_id, submitted_hash,
                     approved_token_id, approved_hash, approval_expires_at)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
                    ON CONFLICT (workspace_id, environment_id, key) DO UPDATE SET
                     content = EXCLUDED.content, state = EXCLUDED.state, risk = EXCLUDED.risk,
                     updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at,
                     submitted_by = EXCLUDED.submitted_by, submitted_at = EXCLUDED.submitted_at,
                     approved_by = EXCLUDED.approved_by, approved_at = EXCLUDED.approved_at,
                     review_note = EXCLUDED.review_note, deleted = EXCLUDED.deleted,
                     base_revision = EXCLUDED.base_revision,
                     submitted_token_id = EXCLUDED.submitted_token_id,
                     submitted_hash = EXCLUDED.submitted_hash,
                     approved_token_id = EXCLUDED.approved_token_id,
                     approved_hash = EXCLUDED.approved_hash,
                     approval_expires_at = EXCLUDED.approval_expires_at`,
					ws, env, k, content, string(d.State), string(d.Risk), d.UpdatedBy,
					d.UpdatedAt, d.SubmittedBy, d.SubmittedAt, d.ApprovedBy, d.ApprovedAt,
					d.ReviewNote, d.Deleted, d.BaseRevision, d.SubmittedTokenID, d.SubmittedHash,
					d.ApprovedTokenID, d.ApprovedHash, d.ApprovalExpiresAt)
			case collFlagRevisions:
				flagRevScopes = append(flagRevScopes, key)
			case collFlagState:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				var tomb *int64
				if rev, ok := data.FlagTombstones[key]; ok {
					tomb = &rev
				}
				batch.Queue(`INSERT INTO flag_state
                    (workspace_id, environment_id, key, current_revision, next_revision, tombstone_manifest_rev)
                    VALUES ($1,$2,$3,$4,$5,$6)
                    ON CONFLICT (workspace_id, environment_id, key) DO UPDATE SET
                     current_revision = EXCLUDED.current_revision,
                     next_revision = EXCLUDED.next_revision,
                     tombstone_manifest_rev = EXCLUDED.tombstone_manifest_rev`,
					ws, env, k, data.CurrentFlagRev[key], data.NextFlagRev[key], tomb)
			case collSegmentDrafts:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				d := data.SegmentDrafts[key]
				if d == nil {
					continue
				}
				content, err := json.Marshal(d.Content)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO segment_drafts
                    (workspace_id, environment_id, key, content, state, risk, updated_by,
                     updated_at, submitted_by, approved_by, base_revision,
                     submitted_token_id, submitted_at, submitted_hash, approved_token_id,
                     approved_at, approved_hash, approval_expires_at)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
                    ON CONFLICT (workspace_id, environment_id, key) DO UPDATE SET
                     content = EXCLUDED.content, state = EXCLUDED.state, risk = EXCLUDED.risk,
                     updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at,
                     submitted_by = EXCLUDED.submitted_by, approved_by = EXCLUDED.approved_by,
                     base_revision = EXCLUDED.base_revision,
                     submitted_token_id = EXCLUDED.submitted_token_id,
                     submitted_at = EXCLUDED.submitted_at,
                     submitted_hash = EXCLUDED.submitted_hash,
                     approved_token_id = EXCLUDED.approved_token_id,
                     approved_at = EXCLUDED.approved_at,
                     approved_hash = EXCLUDED.approved_hash,
                     approval_expires_at = EXCLUDED.approval_expires_at`,
					ws, env, k, content, string(d.State), string(d.Risk), d.UpdatedBy,
					d.UpdatedAt, d.SubmittedBy, d.ApprovedBy, d.BaseRevision,
					d.SubmittedTokenID, d.SubmittedAt, d.SubmittedHash, d.ApprovedTokenID,
					d.ApprovedAt, d.ApprovedHash, d.ApprovalExpiresAt)
			case collSegmentRevisions:
				segRevScopes = append(segRevScopes, key)
			case collSegmentState:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO segment_state
                    (workspace_id, environment_id, key, current_revision, next_revision)
                    VALUES ($1,$2,$3,$4,$5)
                    ON CONFLICT (workspace_id, environment_id, key) DO UPDATE SET
                     current_revision = EXCLUDED.current_revision,
                     next_revision = EXCLUDED.next_revision`,
					ws, env, k, data.CurrentSegmentRev[key], data.NextSegmentRev[key])
			case collManifestRevisions:
				ws, env, err := splitScope2(key)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO env_state (workspace_id, environment_id, manifest_revision)
                    VALUES ($1,$2,$3)
                    ON CONFLICT (workspace_id, environment_id) DO UPDATE SET
                     manifest_revision = EXCLUDED.manifest_revision`,
					ws, env, data.ManifestRevisions[key])
			case collExperiments:
				e := data.Experiments[key]
				if e == nil {
					continue
				}
				doc, err := json.Marshal(e)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO experiments
                    (id, workspace_id, environment_id, flag_key, epoch, state, doc, created_at, updated_at)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
                    ON CONFLICT (id) DO UPDATE SET
                     workspace_id = EXCLUDED.workspace_id, environment_id = EXCLUDED.environment_id,
                     flag_key = EXCLUDED.flag_key, epoch = EXCLUDED.epoch, state = EXCLUDED.state,
                     doc = EXCLUDED.doc, updated_at = EXCLUDED.updated_at`,
					e.ID, e.WorkspaceID, e.EnvironmentID, e.FlagKey, e.Epoch, string(e.State),
					doc, e.CreatedAt, e.UpdatedAt)
			case collAIResources:
				ws, env, typ, logical, err := splitScope4(key)
				if err != nil {
					return err
				}
				r := data.AIResources[key]
				if r == nil {
					continue
				}
				batch.Queue(`INSERT INTO ai_resources
                    (workspace_id, environment_id, type, logical_id, physical_id, owner, created_at)
                    VALUES ($1,$2,$3,$4,$5,$6,$7)
                    ON CONFLICT (workspace_id, environment_id, type, logical_id) DO UPDATE SET
                     physical_id = EXCLUDED.physical_id, owner = EXCLUDED.owner`,
					ws, env, typ, logical, r.PhysicalID, r.Owner, r.CreatedAt)
			case collPinnedVersions:
				ws, env, typ, logical, err := splitScope4(key)
				if err != nil {
					return err
				}
				pins := data.PinnedVersions[key]
				batch.Queue(`DELETE FROM pinned_versions
                    WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4`,
					ws, env, typ, logical)
				for _, pin := range pins {
					batch.Queue(`INSERT INTO pinned_versions
                        (workspace_id, environment_id, type, logical_id, physical_id,
                         version, digest, pinned_at, pinned_by)
                        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
                        ON CONFLICT (workspace_id, environment_id, type, logical_id, version) DO UPDATE SET
                         physical_id = EXCLUDED.physical_id, digest = EXCLUDED.digest,
                         pinned_at = EXCLUDED.pinned_at, pinned_by = EXCLUDED.pinned_by`,
						pin.WorkspaceID, pin.EnvironmentID, pin.Type, pin.LogicalID,
						pin.PhysicalID, pin.Version, pin.Digest, pin.PinnedAt, pin.PinnedBy)
				}
			case collModelAllowlist:
				entries := data.ModelAllowlist[key]
				batch.Queue(`DELETE FROM model_allowlist WHERE workspace_id=$1`, key)
				for _, e := range entries {
					params, err := json.Marshal(e.Params)
					if err != nil {
						return err
					}
					batch.Queue(`INSERT INTO model_allowlist
                        (workspace_id, provider, model, params, enabled, created_at, created_by)
                        VALUES ($1,$2,$3,$4,$5,$6,$7)
                        ON CONFLICT (workspace_id, provider, model) DO UPDATE SET
                         params = EXCLUDED.params, enabled = EXCLUDED.enabled`,
						e.WorkspaceID, e.Provider, e.Model, params, e.Enabled, e.CreatedAt, e.CreatedBy)
				}
			case collAIRevocations:
				ws, env, typ, logical, err := splitScope4(key)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO ai_revocations
                    (workspace_id, environment_id, type, logical_id, reason)
                    VALUES ($1,$2,$3,$4,$5)
                    ON CONFLICT (workspace_id, environment_id, type, logical_id) DO UPDATE SET
                     reason = EXCLUDED.reason`,
					ws, env, typ, logical, data.AIRevocations[key])
			case collQuotas:
				q, ok := data.Quotas[key]
				if !ok {
					continue
				}
				doc, err := json.Marshal(q)
				if err != nil {
					return err
				}
				batch.Queue(`INSERT INTO quotas (workspace_id, doc) VALUES ($1,$2)
                    ON CONFLICT (workspace_id) DO UPDATE SET doc = EXCLUDED.doc`, key, doc)
			}
		}
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("platform: persist state: %w", err)
		}
	}
	// Revision lists are reconciled (insert missing, delete pruned) because
	// one scope key covers an immutable append-only slice.
	for _, scope := range flagRevScopes {
		if err := p.reconcileFlagRevisions(ctx, tx, scope, data.FlagRevisions[scope]); err != nil {
			return err
		}
	}
	for _, scope := range segRevScopes {
		if err := p.reconcileSegmentRevisions(ctx, tx, scope, data.SegmentRevisions[scope]); err != nil {
			return err
		}
	}
	return nil
}

func (p *pgPersistence) reconcileFlagRevisions(ctx context.Context, tx pgx.Tx,
	scope string, want []*FlagRevision) error {
	ws, env, key, err := splitScope3(scope)
	if err != nil {
		return err
	}
	have := map[int64]bool{}
	rows, err := tx.Query(ctx, `SELECT revision FROM flag_revisions
        WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rev int64
		if err := rows.Scan(&rev); err != nil {
			rows.Close()
			return err
		}
		have[rev] = true
	}
	rows.Close()
	batch := &pgx.Batch{}
	wantSet := map[int64]bool{}
	for _, r := range want {
		wantSet[r.Revision] = true
		if have[r.Revision] {
			continue // immutable: never rewritten
		}
		content, err := json.Marshal(r.Content)
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO flag_revisions
            (workspace_id, environment_id, key, revision, content, hash, data_id,
             published_at, published_by, note, rollback_of)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
            ON CONFLICT (workspace_id, environment_id, key, revision) DO NOTHING`,
			ws, env, key, r.Revision, content, r.Hash, r.DataID,
			r.PublishedAt, r.PublishedBy, r.Note, r.RollbackOf)
	}
	var pruned []int64
	for rev := range have {
		if !wantSet[rev] {
			pruned = append(pruned, rev)
		}
	}
	if len(pruned) > 0 {
		batch.Queue(`DELETE FROM flag_revisions
            WHERE workspace_id=$1 AND environment_id=$2 AND key=$3 AND revision = ANY($4)`,
			ws, env, key, pruned)
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("platform: persist flag revisions: %w", err)
		}
	}
	return nil
}

func (p *pgPersistence) reconcileSegmentRevisions(ctx context.Context, tx pgx.Tx,
	scope string, want []*SegmentRevision) error {
	ws, env, key, err := splitScope3(scope)
	if err != nil {
		return err
	}
	have := map[int64]bool{}
	rows, err := tx.Query(ctx, `SELECT revision FROM segment_revisions
        WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, key)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rev int64
		if err := rows.Scan(&rev); err != nil {
			rows.Close()
			return err
		}
		have[rev] = true
	}
	rows.Close()
	batch := &pgx.Batch{}
	wantSet := map[int64]bool{}
	for _, r := range want {
		wantSet[r.Revision] = true
		if have[r.Revision] {
			continue
		}
		content, err := json.Marshal(r.Content)
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO segment_revisions
            (workspace_id, environment_id, key, revision, content, hash, data_id,
             published_at, published_by)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
            ON CONFLICT (workspace_id, environment_id, key, revision) DO NOTHING`,
			ws, env, key, r.Revision, content, r.Hash, r.DataID, r.PublishedAt, r.PublishedBy)
	}
	var pruned []int64
	for rev := range have {
		if !wantSet[rev] {
			pruned = append(pruned, rev)
		}
	}
	if len(pruned) > 0 {
		batch.Queue(`DELETE FROM segment_revisions
            WHERE workspace_id=$1 AND environment_id=$2 AND key=$3 AND revision = ANY($4)`,
			ws, env, key, pruned)
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("platform: persist segment revisions: %w", err)
		}
	}
	return nil
}

func (p *pgPersistence) commitDeleted(ctx context.Context, tx pgx.Tx, ch *ChangeSet) error {
	batch := &pgx.Batch{}
	for coll, keys := range ch.Deleted {
		for key := range keys {
			switch coll {
			case collFlagDrafts:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				batch.Queue(`DELETE FROM flag_drafts
                    WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, k)
			case collSegmentDrafts:
				ws, env, k, err := splitScope3(key)
				if err != nil {
					return err
				}
				batch.Queue(`DELETE FROM segment_drafts
                    WHERE workspace_id=$1 AND environment_id=$2 AND key=$3`, ws, env, k)
			case collAIResources:
				ws, env, typ, logical, err := splitScope4(key)
				if err != nil {
					return err
				}
				batch.Queue(`DELETE FROM ai_resources
                    WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4`,
					ws, env, typ, logical)
			case collAIRevocations:
				ws, env, typ, logical, err := splitScope4(key)
				if err != nil {
					return err
				}
				batch.Queue(`DELETE FROM ai_revocations
                    WHERE workspace_id=$1 AND environment_id=$2 AND type=$3 AND logical_id=$4`,
					ws, env, typ, logical)
			}
		}
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("platform: persist deletions: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Events: partitioned raw storage + derived unit sets
// ---------------------------------------------------------------------------

var eventColumns = []string{
	"event_id", "workspace_id", "environment_id", "type", "experiment_id", "epoch",
	"flag_key", "flag_revision", "variant", "unit_hash", "session_id", "task_id",
	"source", "metric_key", "value", "client_time", "server_time",
	"policy_version", "state_version", "allocation_probability",
}

type copyEventSource struct {
	events []*Event
	pos    int
}

func (c *copyEventSource) Next() bool { c.pos++; return c.pos <= len(c.events) }
func (c *copyEventSource) Err() error { return nil }
func (c *copyEventSource) Values() ([]any, error) {
	ev := c.events[c.pos-1]
	return []any{
		ev.EventID, ev.WorkspaceID, ev.EnvironmentID, string(ev.Type), ev.ExperimentID,
		ev.Epoch, ev.FlagKey, ev.FlagRevision, ev.Variant, ev.UnitHash, ev.SessionID,
		ev.TaskID, ev.Source, ev.MetricKey, ev.Value, ev.ClientTime.UTC(), ev.ServerTime.UTC(),
		ev.PolicyVersion, ev.StateVersion, ev.AllocationProbability,
	}, nil
}

// ensureEventsPartition creates the monthly partition covering t (UTC) when
// missing. The transaction-level advisory lock serializes creation across
// replicas; IF NOT EXISTS alone does not prevent a catalog race.
func ensureEventsPartition(ctx context.Context, tx pgx.Tx, t time.Time) error {
	utc := t.UTC()
	name := fmt.Sprintf("events_%04d%02d", utc.Year(), int(utc.Month()))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, scopeLockKey("events-partition", name)); err != nil {
		return fmt.Errorf("platform: lock partition %s: %w", name, err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&exists); err != nil {
		return fmt.Errorf("platform: check partition %s: %w", name, err)
	}
	if exists {
		return nil
	}
	from := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF events FOR VALUES FROM ('%s') TO ('%s')`,
		name, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("platform: create partition %s: %w", name, err)
	}
	return nil
}

func (p *pgPersistence) pinBanditAssignment(exp *Experiment, candidate *Event) (*Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pin := *candidate
	err := p.pool.QueryRow(ctx, `INSERT INTO bandit_assignments
        (experiment_id, epoch, unit_hash, variant, source, policy_version, state_version, allocation_probability)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
        ON CONFLICT DO NOTHING
        RETURNING variant, source, policy_version, state_version, allocation_probability`,
		exp.ID, exp.Epoch, candidate.UnitHash, candidate.Variant, candidate.Source,
		candidate.PolicyVersion, candidate.StateVersion, candidate.AllocationProbability).
		Scan(&pin.Variant, &pin.Source, &pin.PolicyVersion, &pin.StateVersion, &pin.AllocationProbability)
	if err == nil {
		return &pin, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("platform: pin bandit assignment: %w", err)
	}
	err = p.pool.QueryRow(ctx, `SELECT variant, source, policy_version, state_version, allocation_probability
        FROM bandit_assignments WHERE experiment_id=$1 AND epoch=$2 AND unit_hash=$3`,
		exp.ID, exp.Epoch, candidate.UnitHash).
		Scan(&pin.Variant, &pin.Source, &pin.PolicyVersion, &pin.StateVersion, &pin.AllocationProbability)
	if err != nil {
		return nil, fmt.Errorf("platform: read bandit assignment: %w", err)
	}
	return &pin, nil
}

func (p *pgPersistence) commitEvents(ctx context.Context, tx pgx.Tx, events []*Event) (map[string]bool, error) {
	months := map[time.Time]bool{}
	for _, ev := range events {
		utc := ev.ServerTime.UTC()
		months[time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)] = true
	}
	for _, m := range slices.SortedFunc(maps.Keys(months), time.Time.Compare) {
		if err := ensureEventsPartition(ctx, tx, m); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE _ingest (
        event_id text, workspace_id text, environment_id text, type text,
        experiment_id text, epoch bigint, flag_key text, flag_revision bigint,
        variant text, unit_hash text, session_id text, task_id text,
        source text, metric_key text, value double precision,
        client_time timestamptz, server_time timestamptz,
        policy_version text, state_version bigint, allocation_probability double precision) ON COMMIT DROP`); err != nil {
		return nil, err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"_ingest"}, eventColumns,
		&copyEventSource{events: events}); err != nil {
		return nil, fmt.Errorf("platform: stage events: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE _accepted_ingest
        (LIKE _ingest) ON COMMIT DROP`); err != nil {
		return nil, err
	}
	// The ledger is the sole authority on acceptance. Every derived write
	// below reads only these rows; an ambiguous commit replay adds nothing.
	_, err := tx.Exec(ctx, `
WITH led AS (
    INSERT INTO event_index (event_id, server_time)
    SELECT event_id, server_time FROM _ingest
    ON CONFLICT (event_id) DO NOTHING
    RETURNING event_id
)
INSERT INTO _accepted_ingest SELECT i.* FROM _ingest i JOIN led USING (event_id)`)
	if err != nil {
		return nil, fmt.Errorf("platform: accept events: %w", err)
	}
	accepted := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT event_id FROM _accepted_ingest`)
	if err != nil {
		return nil, fmt.Errorf("platform: read accepted event ids: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("platform: scan accepted event id: %w", err)
		}
		accepted[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("platform: read accepted event ids: %w", err)
	}
	rows.Close()
	// Locally evaluated SDK exposures must also pin the randomization unit.
	// This runs in the same transaction as event acceptance so a subsequent
	// remote evaluation on another replica sees the first assignment.
	if _, err := tx.Exec(ctx, `INSERT INTO bandit_assignments
        (experiment_id, epoch, unit_hash, variant, source, policy_version, state_version, allocation_probability)
        SELECT DISTINCT ON (experiment_id, epoch, unit_hash)
            experiment_id, epoch, unit_hash, variant, source, policy_version, state_version, allocation_probability
        FROM _accepted_ingest
        WHERE type='exposure' AND policy_version<>'' AND source IN ('bandit','split','bandit-fallback')
        ORDER BY experiment_id, epoch, unit_hash, client_time, event_id
        ON CONFLICT DO NOTHING`); err != nil {
		return nil, fmt.Errorf("platform: pin accepted bandit exposures: %w", err)
	}
	_, err = tx.Exec(ctx, `
WITH payload AS (
    INSERT INTO events (event_id, workspace_id, environment_id, type, experiment_id,
        epoch, flag_key, flag_revision, variant, unit_hash, session_id, task_id,
        source, metric_key, value, client_time, server_time,
        policy_version, state_version, allocation_probability)
    SELECT i.event_id, i.workspace_id, i.environment_id, i.type, i.experiment_id,
        i.epoch, i.flag_key, i.flag_revision, i.variant, i.unit_hash, i.session_id,
        i.task_id, i.source, i.metric_key, i.value, i.client_time, i.server_time,
        i.policy_version, i.state_version, i.allocation_probability
    FROM _accepted_ingest i
    ON CONFLICT (server_time, event_id) DO NOTHING
),
exposures AS (
    INSERT INTO exposed_units (workspace_id, environment_id, experiment_id, epoch,
        variant, unit_hash, first_at)
    SELECT i.workspace_id, i.environment_id, i.experiment_id, i.epoch, i.variant,
        i.unit_hash, min(i.client_time)
    FROM _accepted_ingest i
    WHERE i.type = 'exposure'
    GROUP BY 1, 2, 3, 4, 5, 6
    ON CONFLICT DO NOTHING
),
splits AS (
    INSERT INTO split_exposed_units (workspace_id, environment_id, experiment_id,
        epoch, variant, unit_hash, first_at)
    SELECT i.workspace_id, i.environment_id, i.experiment_id, i.epoch, i.variant,
        i.unit_hash, min(i.client_time)
    FROM _accepted_ingest i
    WHERE i.type = 'exposure' AND i.source = 'split'
    GROUP BY 1, 2, 3, 4, 5, 6
    ON CONFLICT DO NOTHING
)
SELECT count(*) FROM _accepted_ingest`)
	if err != nil {
		return nil, fmt.Errorf("platform: persist events: %w", err)
	}
	// Aggregate increments are computed from accepted rows, never from a
	// replica's potentially stale in-memory counters. PostgreSQL serializes
	// conflicting row updates and each accepted event contributes once.
	if _, err := tx.Exec(ctx, `
INSERT INTO unit_aggs (workspace_id, environment_id, experiment_id, epoch,
    variant, unit_hash, metric_key, cnt, sum_val, sum_sq, first_at, last_at)
SELECT workspace_id, environment_id, experiment_id, epoch, variant, unit_hash,
    metric_key, count(*), sum(value), sum(value * value),
    min(client_time), max(client_time)
FROM _accepted_ingest WHERE type = 'conversion'
GROUP BY 1,2,3,4,5,6,7
ON CONFLICT (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash, metric_key)
DO UPDATE SET cnt = unit_aggs.cnt + EXCLUDED.cnt,
    sum_val = unit_aggs.sum_val + EXCLUDED.sum_val,
    sum_sq = unit_aggs.sum_sq + EXCLUDED.sum_sq,
    first_at = LEAST(unit_aggs.first_at, EXCLUDED.first_at),
    last_at = GREATEST(unit_aggs.last_at, EXCLUDED.last_at)`); err != nil {
		return nil, fmt.Errorf("platform: persist unit aggregates: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO event_counts (workspace_id, environment_id, experiment_id, epoch,
    variant, kind, cnt)
SELECT workspace_id, environment_id, experiment_id, epoch, variant, type, count(*)
FROM _accepted_ingest GROUP BY 1,2,3,4,5,6
ON CONFLICT (workspace_id, environment_id, experiment_id, epoch, variant, kind)
DO UPDATE SET cnt = event_counts.cnt + EXCLUDED.cnt`); err != nil {
		return nil, fmt.Errorf("platform: persist event counts: %w", err)
	}
	return accepted, nil
}

func (p *pgPersistence) commitAggs(ctx context.Context, tx pgx.Tx, aggs []AggUpsert) error {
	batch := &pgx.Batch{}
	for _, a := range aggs {
		batch.Queue(`INSERT INTO unit_aggs
            (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash,
             metric_key, cnt, sum_val, sum_sq, first_at, last_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
            ON CONFLICT (workspace_id, environment_id, experiment_id, epoch, variant, unit_hash, metric_key)
            DO UPDATE SET cnt = EXCLUDED.cnt, sum_val = EXCLUDED.sum_val,
             sum_sq = EXCLUDED.sum_sq, first_at = EXCLUDED.first_at, last_at = EXCLUDED.last_at`,
			a.WorkspaceID, a.EnvironmentID, a.ExperimentID, a.Epoch, a.Variant,
			a.UnitHash, a.MetricKey, a.Count, a.Sum, a.SumSquares,
			a.First.UTC(), a.Last.UTC())
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("platform: persist unit aggregates: %w", err)
	}
	return nil
}

func (p *pgPersistence) commitCounts(ctx context.Context, tx pgx.Tx, counts []CountUpsert) error {
	batch := &pgx.Batch{}
	for _, c := range counts {
		batch.Queue(`INSERT INTO event_counts
            (workspace_id, environment_id, experiment_id, epoch, variant, kind, cnt)
            VALUES ($1,$2,$3,$4,$5,$6,$7)
            ON CONFLICT (workspace_id, environment_id, experiment_id, epoch, variant, kind)
            DO UPDATE SET cnt = EXCLUDED.cnt`,
			c.WorkspaceID, c.EnvironmentID, c.ExperimentID, c.Epoch, c.Variant, c.Kind, c.Count)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("platform: persist event counts: %w", err)
	}
	return nil
}

func (p *pgPersistence) commitAudit(ctx context.Context, tx pgx.Tx, entries []*AuditEntry) error {
	batch := &pgx.Batch{}
	for _, e := range entries {
		batch.Queue(`INSERT INTO audit
            (id, time, actor, actor_role, action, workspace_id, environment_id, object, detail)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			e.ID, e.Time.UTC(), e.Actor, string(e.ActorRole), e.Action,
			e.WorkspaceID, e.EnvironmentID, e.Object, e.Detail)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("platform: persist audit: %w", err)
	}
	return nil
}

// pruneEvents applies the retention window (§10.2 按保留期分区删除): whole
// monthly partitions ending before the cutoff are DROPPED (O(1), no row
// churn), the boundary partition is row-deleted, and the dedup ledger is
// shrunk to match so pruned event IDs stop occupying dedup space.
func (p *pgPersistence) pruneEvents(ctx context.Context, tx pgx.Tx, cutoff time.Time) error {
	names, err := p.eventPartitions(ctx, tx)
	if err != nil {
		return err
	}
	cutoffUTC := cutoff.UTC()
	for _, part := range names {
		upper, ok := partitionUpperBound(part.name)
		if !ok {
			continue
		}
		if !upper.After(cutoffUTC) {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, part.name)); err != nil {
				return fmt.Errorf("platform: drop partition %s: %w", part.name, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM events WHERE server_time < $1`, cutoffUTC); err != nil {
		return fmt.Errorf("platform: prune boundary events: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM event_index WHERE server_time < $1`, cutoffUTC); err != nil {
		return fmt.Errorf("platform: prune dedup ledger: %w", err)
	}
	return nil
}

type partitionInfo struct{ name string }

func (p *pgPersistence) eventPartitions(ctx context.Context, tx pgx.Tx) ([]partitionInfo, error) {
	rows, err := tx.Query(ctx, `
        SELECT c.relname
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        JOIN pg_class parent ON parent.oid = i.inhparent
        WHERE parent.relname = 'events' AND c.relkind = 'r'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []partitionInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, partitionInfo{name: name})
	}
	return out, rows.Err()
}

// partitionUpperBound derives the exclusive upper time bound of a partition
// from its events_YYYYMM name (creation is owned by ensureEventsPartition,
// so the naming convention is authoritative).
func partitionUpperBound(name string) (time.Time, bool) {
	suffix := strings.TrimPrefix(name, "events_")
	if len(suffix) != 6 || suffix == name {
		return time.Time{}, false
	}
	year, err1 := strconv.Atoi(suffix[:4])
	month, err2 := strconv.Atoi(suffix[4:])
	if err1 != nil || err2 != nil || month < 1 || month > 12 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0), true
}

// ---------------------------------------------------------------------------
// Dedup ledger (eventDedup)
// ---------------------------------------------------------------------------

func (p *pgPersistence) KnownEventIDs(ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT event_id FROM event_index WHERE event_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// analysisEvents reads a consistent raw-event snapshot and the independently
// maintained event_counts ledger for the same experiment epoch.
func (p *pgPersistence) analysisEvents(exp *Experiment, now time.Time) ([]*Event, int64, int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgLoadTimeout)
	defer cancel()
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("platform: begin analysis snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT event_id, workspace_id, environment_id, type,
        experiment_id, epoch, flag_key, flag_revision, variant, unit_hash, session_id,
        task_id, source, metric_key, value, client_time, server_time,
        policy_version, state_version, allocation_probability
        FROM events WHERE workspace_id=$1 AND environment_id=$2 AND experiment_id=$3
        AND epoch=$4 AND server_time <= $5 ORDER BY server_time, event_id`,
		exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, now)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("platform: query analysis events: %w", err)
	}
	var events []*Event
	for rows.Next() {
		ev := &Event{}
		var typ string
		if err := rows.Scan(&ev.EventID, &ev.WorkspaceID, &ev.EnvironmentID, &typ,
			&ev.ExperimentID, &ev.Epoch, &ev.FlagKey, &ev.FlagRevision, &ev.Variant,
			&ev.UnitHash, &ev.SessionID, &ev.TaskID, &ev.Source, &ev.MetricKey,
			&ev.Value, &ev.ClientTime, &ev.ServerTime,
			&ev.PolicyVersion, &ev.StateVersion, &ev.AllocationProbability); err != nil {
			rows.Close()
			return nil, 0, 0, false, fmt.Errorf("platform: scan analysis event: %w", err)
		}
		ev.Type = EventType(typ)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, false, fmt.Errorf("platform: read analysis events: %w", err)
	}
	rows.Close()
	var durable int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cnt), 0) FROM event_counts
        WHERE workspace_id=$1 AND environment_id=$2 AND experiment_id=$3 AND epoch=$4`,
		exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch).Scan(&durable); err != nil {
		return nil, 0, 0, false, fmt.Errorf("platform: read analysis counts: %w", err)
	}
	type aggregateKey struct{ variant, unit, metric string }
	type aggregateValue struct {
		count        int64
		sum, squares float64
	}
	rawAggs := map[aggregateKey]aggregateValue{}
	type exposureKey struct{ variant, unit string }
	rawExposed := map[exposureKey]bool{}
	rawSplit := map[exposureKey]bool{}
	for _, ev := range events {
		if ev.Type == EventExposure {
			key := exposureKey{ev.Variant, ev.UnitHash}
			rawExposed[key] = true
			if ev.Source == "split" {
				rawSplit[key] = true
			}
		}
		if ev.Type == EventConversion {
			key := aggregateKey{ev.Variant, ev.UnitHash, ev.MetricKey}
			v := rawAggs[key]
			v.count++
			v.sum += ev.Value
			v.squares += ev.Value * ev.Value
			rawAggs[key] = v
		}
	}
	aggRows, err := tx.Query(ctx, `SELECT variant, unit_hash, metric_key, cnt, sum_val, sum_sq FROM unit_aggs
        WHERE workspace_id=$1 AND environment_id=$2 AND experiment_id=$3 AND epoch=$4
        `, exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("platform: read analysis aggregates: %w", err)
	}
	aggregateMismatch := false
	for aggRows.Next() {
		var key aggregateKey
		var persisted aggregateValue
		if err := aggRows.Scan(&key.variant, &key.unit, &key.metric, &persisted.count,
			&persisted.sum, &persisted.squares); err != nil {
			aggRows.Close()
			return nil, 0, 0, false, fmt.Errorf("platform: scan analysis aggregate: %w", err)
		}
		raw, ok := rawAggs[key]
		if !ok || raw.count != persisted.count ||
			math.Abs(raw.sum-persisted.sum) > 1e-9*math.Max(1, math.Abs(persisted.sum)) ||
			math.Abs(raw.squares-persisted.squares) > 1e-9*math.Max(1, math.Abs(persisted.squares)) {
			aggregateMismatch = true
		}
		delete(rawAggs, key)
	}
	if err := aggRows.Err(); err != nil {
		aggRows.Close()
		return nil, 0, 0, false, fmt.Errorf("platform: read analysis aggregates: %w", err)
	}
	aggRows.Close()
	if len(rawAggs) > 0 {
		aggregateMismatch = true
	}
	for _, view := range []struct {
		table    string
		expected map[exposureKey]bool
	}{
		{"exposed_units", rawExposed}, {"split_exposed_units", rawSplit},
	} {
		query := fmt.Sprintf(`SELECT variant, unit_hash FROM %s WHERE workspace_id=$1 AND environment_id=$2 AND experiment_id=$3 AND epoch=$4`, view.table)
		rows, err := tx.Query(ctx, query, exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
		if err != nil {
			return nil, 0, 0, false, fmt.Errorf("platform: read analysis exposures: %w", err)
		}
		seen := map[exposureKey]bool{}
		for rows.Next() {
			var key exposureKey
			if err := rows.Scan(&key.variant, &key.unit); err != nil {
				rows.Close()
				return nil, 0, 0, false, fmt.Errorf("platform: scan analysis exposure: %w", err)
			}
			seen[key] = true
			if !view.expected[key] {
				aggregateMismatch = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, 0, false, fmt.Errorf("platform: read analysis exposures: %w", err)
		}
		rows.Close()
		if len(seen) != len(view.expected) {
			aggregateMismatch = true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, 0, false, fmt.Errorf("platform: commit analysis snapshot: %w", err)
	}
	return events, int64(len(events)), durable, aggregateMismatch, nil
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func (p *pgPersistence) Load() (*Data, error) {
	for range 5 {
		d, err := p.loadOnce()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), pgLoadTimeout)
		var version int64
		err = p.pool.QueryRow(ctx, `SELECT control_version FROM meta WHERE id=1`).Scan(&version)
		cancel()
		if errors.Is(err, pgx.ErrNoRows) && d.ControlVersion == 0 {
			return d, nil
		}
		if err != nil {
			return nil, err
		}
		if version == d.ControlVersion {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%w: management state changed repeatedly during reload; retry", ErrConflict)
}

func (p *pgPersistence) loadOnce() (*Data, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgLoadTimeout)
	defer cancel()
	d := newData()

	var nextCounter, nextAudit, schemaVersion int64
	err := p.pool.QueryRow(ctx,
		`SELECT next_counter, next_audit, schema_version, control_version FROM meta WHERE id = 1`).
		Scan(&nextCounter, &nextAudit, &schemaVersion, &d.ControlVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil // empty backend; bootstrap follows
	}
	if err != nil {
		return nil, fmt.Errorf("platform: load meta: %w", err)
	}
	if schemaVersion != dataSchemaVersion {
		return nil, fmt.Errorf("platform: postgres schema %d, want %d", schemaVersion, dataSchemaVersion)
	}
	d.NextCounter = nextCounter
	d.NextAudit = nextAudit

	keyRows, err := p.pool.Query(ctx, `SELECT key_hex FROM hmac_keys ORDER BY version`)
	if err != nil {
		return nil, err
	}
	for keyRows.Next() {
		var k string
		if err := keyRows.Scan(&k); err != nil {
			keyRows.Close()
			return nil, err
		}
		d.HMACKeyRing = append(d.HMACKeyRing, k)
	}
	keyRows.Close()
	if len(d.HMACKeyRing) > 0 {
		d.HMACKey = d.HMACKeyRing[len(d.HMACKeyRing)-1]
	}

	if err := p.loadTenants(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadFlags(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadApprovals(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadExperiments(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadEvents(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadAudit(ctx, d); err != nil {
		return nil, err
	}
	if err := p.loadAI(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// LookupToken checks the primary database for every authentication decision.
// A token revoked on another replica must never remain valid through this
// replica's cached working set.
func (p *pgPersistence) LookupToken(hash string) (*Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t := &Token{}
	var kind, role string
	var lastUsed *time.Time
	err := p.pool.QueryRow(ctx, `SELECT id, name, kind, role, workspace_id, environment_id,
		secret_hash, prefix, created_at, last_used_at, revoked, created_by
		FROM tokens WHERE secret_hash=$1`, hash).Scan(&t.ID, &t.Name, &kind, &role,
		&t.WorkspaceID, &t.EnvironmentID, &t.SecretHash, &t.Prefix, &t.CreatedAt,
		&lastUsed, &t.Revoked, &t.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: unknown token", ErrAccessDenied)
	}
	if err != nil {
		return nil, err
	}
	if t.Revoked {
		return nil, fmt.Errorf("%w: token revoked", ErrAccessDenied)
	}
	t.Kind, t.Role = TokenKind(kind), Role(role)
	if lastUsed != nil {
		t.LastUsedAt = *lastUsed
	}
	return t, nil
}

func (p *pgPersistence) loadTenants(ctx context.Context, d *Data) error {
	rows, err := p.pool.Query(ctx, `SELECT id, slug, name, created_at FROM workspaces`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Slug, &w.Name, &w.CreatedAt); err != nil {
			return err
		}
		d.Workspaces[w.ID] = w
	}
	if err := rows.Err(); err != nil {
		return err
	}

	envRows, err := p.pool.Query(ctx,
		`SELECT id, workspace_id, slug, name, namespace_id, created_at FROM environments`)
	if err != nil {
		return err
	}
	defer envRows.Close()
	for envRows.Next() {
		e := &Environment{}
		if err := envRows.Scan(&e.ID, &e.WorkspaceID, &e.Slug, &e.Name, &e.NamespaceID, &e.CreatedAt); err != nil {
			return err
		}
		d.Environments[e.ID] = e
		d.EnvBySlugKey[e.WorkspaceID+"/"+e.Slug] = e.ID
	}
	if err := envRows.Err(); err != nil {
		return err
	}

	tokRows, err := p.pool.Query(ctx, `SELECT id, name, kind, role, workspace_id, environment_id,
        secret_hash, prefix, created_at, last_used_at, revoked, created_by FROM tokens`)
	if err != nil {
		return err
	}
	defer tokRows.Close()
	for tokRows.Next() {
		t := &Token{}
		var kind, role string
		var lastUsed *time.Time
		if err := tokRows.Scan(&t.ID, &t.Name, &kind, &role, &t.WorkspaceID, &t.EnvironmentID,
			&t.SecretHash, &t.Prefix, &t.CreatedAt, &lastUsed, &t.Revoked, &t.CreatedBy); err != nil {
			return err
		}
		t.Kind = TokenKind(kind)
		t.Role = Role(role)
		if lastUsed != nil {
			t.LastUsedAt = *lastUsed
		}
		d.Tokens[t.ID] = t
		d.TokensByHash[t.SecretHash] = t.ID
	}
	if err := tokRows.Err(); err != nil {
		return err
	}

	qRows, err := p.pool.Query(ctx, `SELECT workspace_id, doc FROM quotas`)
	if err != nil {
		return err
	}
	defer qRows.Close()
	for qRows.Next() {
		var wsID string
		var doc []byte
		if err := qRows.Scan(&wsID, &doc); err != nil {
			return err
		}
		var q Quotas
		if err := json.Unmarshal(doc, &q); err != nil {
			return fmt.Errorf("platform: parse quotas of %s: %w", wsID, err)
		}
		d.Quotas[wsID] = q
	}
	return qRows.Err()
}

func (p *pgPersistence) loadApprovals(ctx context.Context, d *Data) error {
	rows, err := p.pool.Query(ctx, `SELECT doc FROM approvals`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return err
		}
		var a Approval
		if err := json.Unmarshal(doc, &a); err != nil {
			return err
		}
		d.Approvals[a.ID] = &a
	}
	return rows.Err()
}

func (p *pgPersistence) loadFlags(ctx context.Context, d *Data) error {
	draftRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, content,
        state, risk, updated_by, updated_at, submitted_by, submitted_at, approved_by,
        approved_at, review_note, deleted, base_revision, submitted_token_id,
        submitted_hash, approved_token_id, approved_hash, approval_expires_at FROM flag_drafts`)
	if err != nil {
		return err
	}
	defer draftRows.Close()
	for draftRows.Next() {
		dr := &FlagDraft{}
		var content []byte
		var state, risk string
		if err := draftRows.Scan(&dr.WorkspaceID, &dr.EnvironmentID, &dr.Key, &content,
			&state, &risk, &dr.UpdatedBy, &dr.UpdatedAt, &dr.SubmittedBy, &dr.SubmittedAt,
			&dr.ApprovedBy, &dr.ApprovedAt, &dr.ReviewNote, &dr.Deleted, &dr.BaseRevision,
			&dr.SubmittedTokenID, &dr.SubmittedHash, &dr.ApprovedTokenID, &dr.ApprovedHash,
			&dr.ApprovalExpiresAt); err != nil {
			return err
		}
		dr.State = DraftState(state)
		dr.Risk = Risk(risk)
		if err := json.Unmarshal(content, &dr.Content); err != nil {
			return fmt.Errorf("platform: parse flag draft %s: %w", dr.Key, err)
		}
		d.FlagDrafts[flagScopeKey(dr.WorkspaceID, dr.EnvironmentID, dr.Key)] = dr
	}
	if err := draftRows.Err(); err != nil {
		return err
	}

	revRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, revision,
        content, hash, data_id, published_at, published_by, note, rollback_of
        FROM flag_revisions ORDER BY workspace_id, environment_id, key, revision`)
	if err != nil {
		return err
	}
	defer revRows.Close()
	for revRows.Next() {
		r := &FlagRevision{}
		var content []byte
		if err := revRows.Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Key, &r.Revision,
			&content, &r.Hash, &r.DataID, &r.PublishedAt, &r.PublishedBy, &r.Note,
			&r.RollbackOf); err != nil {
			return err
		}
		if err := json.Unmarshal(content, &r.Content); err != nil {
			return fmt.Errorf("platform: parse flag revision %s r%d: %w", r.Key, r.Revision, err)
		}
		scope := flagScopeKey(r.WorkspaceID, r.EnvironmentID, r.Key)
		d.FlagRevisions[scope] = append(d.FlagRevisions[scope], r)
	}
	if err := revRows.Err(); err != nil {
		return err
	}

	stateRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key,
        current_revision, next_revision, tombstone_manifest_rev FROM flag_state`)
	if err != nil {
		return err
	}
	defer stateRows.Close()
	for stateRows.Next() {
		var ws, env, key string
		var current, next int64
		var tomb *int64
		if err := stateRows.Scan(&ws, &env, &key, &current, &next, &tomb); err != nil {
			return err
		}
		scope := flagScopeKey(ws, env, key)
		if current > 0 {
			d.CurrentFlagRev[scope] = current
		}
		if next > 0 {
			d.NextFlagRev[scope] = next
		}
		if tomb != nil {
			d.FlagTombstones[scope] = *tomb
		}
	}
	if err := stateRows.Err(); err != nil {
		return err
	}

	segDraftRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, content,
        state, risk, updated_by, updated_at, submitted_by, approved_by, base_revision,
        submitted_token_id, submitted_at, submitted_hash, approved_token_id,
        approved_at, approved_hash, approval_expires_at
        FROM segment_drafts`)
	if err != nil {
		return err
	}
	defer segDraftRows.Close()
	for segDraftRows.Next() {
		dr := &SegmentDraft{}
		var content []byte
		var state, risk string
		if err := segDraftRows.Scan(&dr.WorkspaceID, &dr.EnvironmentID, &dr.Key, &content,
			&state, &risk, &dr.UpdatedBy, &dr.UpdatedAt, &dr.SubmittedBy, &dr.ApprovedBy,
			&dr.BaseRevision, &dr.SubmittedTokenID, &dr.SubmittedAt, &dr.SubmittedHash,
			&dr.ApprovedTokenID, &dr.ApprovedAt, &dr.ApprovedHash, &dr.ApprovalExpiresAt); err != nil {
			return err
		}
		dr.State = DraftState(state)
		dr.Risk = Risk(risk)
		if err := json.Unmarshal(content, &dr.Content); err != nil {
			return fmt.Errorf("platform: parse segment draft %s: %w", dr.Key, err)
		}
		d.SegmentDrafts[flagScopeKey(dr.WorkspaceID, dr.EnvironmentID, dr.Key)] = dr
	}
	if err := segDraftRows.Err(); err != nil {
		return err
	}

	segRevRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key, revision,
        content, hash, data_id, published_at, published_by
        FROM segment_revisions ORDER BY workspace_id, environment_id, key, revision`)
	if err != nil {
		return err
	}
	defer segRevRows.Close()
	for segRevRows.Next() {
		r := &SegmentRevision{}
		var content []byte
		if err := segRevRows.Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Key, &r.Revision,
			&content, &r.Hash, &r.DataID, &r.PublishedAt, &r.PublishedBy); err != nil {
			return err
		}
		if err := json.Unmarshal(content, &r.Content); err != nil {
			return fmt.Errorf("platform: parse segment revision %s r%d: %w", r.Key, r.Revision, err)
		}
		scope := flagScopeKey(r.WorkspaceID, r.EnvironmentID, r.Key)
		d.SegmentRevisions[scope] = append(d.SegmentRevisions[scope], r)
	}
	if err := segRevRows.Err(); err != nil {
		return err
	}

	segStateRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, key,
        current_revision, next_revision FROM segment_state`)
	if err != nil {
		return err
	}
	defer segStateRows.Close()
	for segStateRows.Next() {
		var ws, env, key string
		var current, next int64
		if err := segStateRows.Scan(&ws, &env, &key, &current, &next); err != nil {
			return err
		}
		scope := flagScopeKey(ws, env, key)
		if current > 0 {
			d.CurrentSegmentRev[scope] = current
		}
		if next > 0 {
			d.NextSegmentRev[scope] = next
		}
	}
	if err := segStateRows.Err(); err != nil {
		return err
	}

	manRows, err := p.pool.Query(ctx,
		`SELECT workspace_id, environment_id, manifest_revision FROM env_state`)
	if err != nil {
		return err
	}
	defer manRows.Close()
	for manRows.Next() {
		var ws, env string
		var rev int64
		if err := manRows.Scan(&ws, &env, &rev); err != nil {
			return err
		}
		if rev > 0 {
			d.ManifestRevisions[scopeKey(ws, env)] = rev
		}
	}
	return manRows.Err()
}

func (p *pgPersistence) loadExperiments(ctx context.Context, d *Data) error {
	rows, err := p.pool.Query(ctx, `SELECT doc FROM experiments ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return err
		}
		e := &Experiment{}
		if err := json.Unmarshal(doc, e); err != nil {
			return fmt.Errorf("platform: parse experiment: %w", err)
		}
		d.Experiments[e.ID] = e
	}
	return rows.Err()
}

func (p *pgPersistence) loadEvents(ctx context.Context, d *Data) error {
	// Newest MaxRetainedEvents rows, then flipped to ascending order — the
	// same working-set cap the file backend enforces. Older rows stay in
	// PostgreSQL for retention-window queries and the dedup ledger.
	rows, err := p.pool.Query(ctx, `SELECT event_id, workspace_id, environment_id, type,
        experiment_id, epoch, flag_key, flag_revision, variant, unit_hash, session_id,
        task_id, source, metric_key, value, client_time, server_time,
        policy_version, state_version, allocation_probability
        FROM events ORDER BY server_time DESC, event_id DESC LIMIT $1`, MaxRetainedEvents)
	if err != nil {
		return err
	}
	var desc []*Event
	for rows.Next() {
		ev := &Event{}
		var typ string
		if err := rows.Scan(&ev.EventID, &ev.WorkspaceID, &ev.EnvironmentID, &typ,
			&ev.ExperimentID, &ev.Epoch, &ev.FlagKey, &ev.FlagRevision, &ev.Variant,
			&ev.UnitHash, &ev.SessionID, &ev.TaskID, &ev.Source, &ev.MetricKey,
			&ev.Value, &ev.ClientTime, &ev.ServerTime,
			&ev.PolicyVersion, &ev.StateVersion, &ev.AllocationProbability); err != nil {
			rows.Close()
			return err
		}
		ev.Type = EventType(typ)
		desc = append(desc, ev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for i := len(desc) - 1; i >= 0; i-- {
		ev := desc[i]
		d.Events = append(d.Events, ev)
		d.EventIndex[ev.EventID] = len(d.Events) - 1
	}

	for _, q := range []struct {
		sql  string
		dest map[string]map[string]bool
	}{
		{`SELECT workspace_id, environment_id, experiment_id, epoch, variant, unit_hash
		   FROM exposed_units`, d.ExposedUnits},
		{`SELECT workspace_id, environment_id, experiment_id, epoch, variant, unit_hash
		   FROM split_exposed_units`, d.SplitExposedUnits},
	} {
		rows, err := p.pool.Query(ctx, q.sql)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ws, env, exp, variant, unit string
			var epoch int64
			if err := rows.Scan(&ws, &env, &exp, &epoch, &variant, &unit); err != nil {
				rows.Close()
				return err
			}
			scope := fmt.Sprintf("%s/%s/%s/%d/%s", ws, env, exp, epoch, variant)
			if q.dest[scope] == nil {
				q.dest[scope] = map[string]bool{}
			}
			q.dest[scope][unit] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	aggRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, experiment_id,
        epoch, variant, unit_hash, metric_key, cnt, sum_val, sum_sq, first_at, last_at
        FROM unit_aggs`)
	if err != nil {
		return err
	}
	defer aggRows.Close()
	for aggRows.Next() {
		var ws, env, exp, variant, unit, metric string
		var epoch int64
		agg := &UnitAgg{}
		if err := aggRows.Scan(&ws, &env, &exp, &epoch, &variant, &unit, &metric,
			&agg.Count, &agg.Sum, &agg.SumSquares, &agg.First, &agg.Last); err != nil {
			return err
		}
		key := fmt.Sprintf("%s/%s/%s/%d/%s/%s/%s", ws, env, exp, epoch, variant, unit, metric)
		d.UnitAggs[key] = agg
	}
	if err := aggRows.Err(); err != nil {
		return err
	}

	cntRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, experiment_id,
        epoch, variant, kind, cnt FROM event_counts`)
	if err != nil {
		return err
	}
	defer cntRows.Close()
	for cntRows.Next() {
		var ws, env, exp, variant, kind string
		var epoch, cnt int64
		if err := cntRows.Scan(&ws, &env, &exp, &epoch, &variant, &kind, &cnt); err != nil {
			return err
		}
		d.EventCounts[fmt.Sprintf("%s/%s/%s/%d/%s/%s", ws, env, exp, epoch, variant, kind)] = cnt
	}
	return cntRows.Err()
}

func (p *pgPersistence) loadAudit(ctx context.Context, d *Data) error {
	rows, err := p.pool.Query(ctx, `SELECT id, time, actor, actor_role, action,
        workspace_id, environment_id, object, detail FROM audit ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e := &AuditEntry{}
		var role string
		if err := rows.Scan(&e.ID, &e.Time, &e.Actor, &role, &e.Action,
			&e.WorkspaceID, &e.EnvironmentID, &e.Object, &e.Detail); err != nil {
			return err
		}
		e.ActorRole = Role(role)
		d.Audit = append(d.Audit, e)
	}
	return rows.Err()
}

func (p *pgPersistence) loadAI(ctx context.Context, d *Data) error {
	resRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, type, logical_id,
        physical_id, owner, created_at FROM ai_resources`)
	if err != nil {
		return err
	}
	defer resRows.Close()
	for resRows.Next() {
		r := &AIResourceRef{}
		if err := resRows.Scan(&r.WorkspaceID, &r.EnvironmentID, &r.Type, &r.LogicalID,
			&r.PhysicalID, &r.Owner, &r.CreatedAt); err != nil {
			return err
		}
		d.AIResources[aiKey(r.WorkspaceID, r.EnvironmentID, r.Type, r.LogicalID)] = r
	}
	if err := resRows.Err(); err != nil {
		return err
	}

	pinRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, type, logical_id,
        physical_id, version, digest, pinned_at, pinned_by FROM pinned_versions ORDER BY seq`)
	if err != nil {
		return err
	}
	defer pinRows.Close()
	for pinRows.Next() {
		pin := &PinnedVersion{}
		if err := pinRows.Scan(&pin.WorkspaceID, &pin.EnvironmentID, &pin.Type, &pin.LogicalID,
			&pin.PhysicalID, &pin.Version, &pin.Digest, &pin.PinnedAt, &pin.PinnedBy); err != nil {
			return err
		}
		key := aiKey(pin.WorkspaceID, pin.EnvironmentID, pin.Type, pin.LogicalID)
		d.PinnedVersions[key] = append(d.PinnedVersions[key], pin)
	}
	if err := pinRows.Err(); err != nil {
		return err
	}

	allowRows, err := p.pool.Query(ctx, `SELECT workspace_id, provider, model, params,
        enabled, created_at, created_by FROM model_allowlist ORDER BY ord`)
	if err != nil {
		return err
	}
	defer allowRows.Close()
	for allowRows.Next() {
		e := ModelAllowEntry{}
		var params []byte
		if err := allowRows.Scan(&e.WorkspaceID, &e.Provider, &e.Model, &params,
			&e.Enabled, &e.CreatedAt, &e.CreatedBy); err != nil {
			return err
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &e.Params); err != nil {
				return fmt.Errorf("platform: parse allowlist params: %w", err)
			}
		}
		d.ModelAllowlist[e.WorkspaceID] = append(d.ModelAllowlist[e.WorkspaceID], e)
	}
	if err := allowRows.Err(); err != nil {
		return err
	}

	revRows, err := p.pool.Query(ctx, `SELECT workspace_id, environment_id, type, logical_id,
        reason FROM ai_revocations`)
	if err != nil {
		return err
	}
	defer revRows.Close()
	for revRows.Next() {
		var ws, env, typ, logical, reason string
		if err := revRows.Scan(&ws, &env, &typ, &logical, &reason); err != nil {
			return err
		}
		d.AIRevocations[aiKey(ws, env, typ, logical)] = reason
	}
	return revRows.Err()
}

// nullableTime maps the zero time to NULL so absence and "never" stay
// distinguishable and scans round-trip to time.Time{}.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ---------------------------------------------------------------------------
// Multi-replica publish sequencing (docs/实施状态.md §11 发布对账; §3 集群:
// "多节点需要数据库级序列化"). PostgreSQL session-level advisory locks are the
// cross-process analogue of the admin service's in-process publishMu: the
// first replica to acquire the scope lock publishes while the others wait, so
// concurrent publishes on different replicas never interleave revision
// allocation. The lock lives on ONE dedicated pooled connection (session
// locks are per-connection) released exactly once.
// ---------------------------------------------------------------------------

// LockScope acquires the session-level advisory lock `key` on a dedicated
// pooled connection. The ctx (from Store.PublishLock) bounds the wait; the
// returned release function unlocks on the same connection exactly once and
// returns it to the pool.
func (p *pgPersistence) LockScope(ctx context.Context, key int64) (func(), error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("platform: acquire lock conn: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("platform: pg_advisory_lock(%d): %w", key, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// Unlock on the SAME connection that holds the lock; use a fresh
			// context because the caller's may already be cancelled.
			uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", key)
			conn.Release()
		})
	}
	return release, nil
}
