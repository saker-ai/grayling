package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/libtnb/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// databaseState is the transaction boundary for the platform working set.
// Raw events have their own table so retention and deduplication do not depend
// on the bounded in-memory event window.
type databaseState struct {
	ID             int64  `gorm:"primaryKey"`
	Version        int64  `gorm:"not null"`
	ControlVersion int64  `gorm:"not null"`
	Payload        []byte `gorm:"not null"`
}

type databaseEvent struct {
	EventID       string    `gorm:"primaryKey"`
	CommitVersion int64     `gorm:"index:idx_event_commit,priority:1;not null"`
	CommitOrdinal int       `gorm:"index:idx_event_commit,priority:2;not null"`
	WorkspaceID   string    `gorm:"index:idx_analysis,priority:1;not null"`
	EnvironmentID string    `gorm:"index:idx_analysis,priority:2;not null"`
	ExperimentID  string    `gorm:"index:idx_analysis,priority:3;not null"`
	Epoch         int64     `gorm:"index:idx_analysis,priority:4;not null"`
	ServerTime    time.Time `gorm:"index:idx_analysis,priority:5;index:idx_event_retention;not null"`
	Payload       []byte    `gorm:"not null"`
}

type databasePin struct {
	Key     string `gorm:"primaryKey"`
	Payload []byte `gorm:"not null"`
}

type databaseToken struct {
	SecretHash string `gorm:"primaryKey"`
	TokenID    string `gorm:"uniqueIndex;not null"`
	Payload    []byte `gorm:"not null"`
}

type databaseMetric struct {
	Category string `gorm:"primaryKey"`
	Key      string `gorm:"primaryKey;index:idx_metric_key"`
	Payload  []byte `gorm:"not null"`
}

type databaseMetricDefinition struct {
	WorkspaceID string `gorm:"primaryKey;index:idx_metric_definition_list,priority:1"`
	Key         string `gorm:"primaryKey;index:idx_metric_definition_list,priority:3"`
	Version     int64  `gorm:"primaryKey"`
	Status      string `gorm:"not null;index:idx_metric_definition_list,priority:2"`
	Payload     []byte `gorm:"not null"`
}

type databaseLease struct {
	ID             int64     `gorm:"primaryKey"`
	Holder         string    `gorm:"not null"`
	Generation     int64     `gorm:"not null"`
	ExpiresAt      time.Time `gorm:"not null"`
	LastRunAt      *time.Time
	LastTakeoverAt *time.Time
	LastError      string `gorm:"not null"`
}

type gormPersistence struct {
	db              *gorm.DB
	dialect         string
	version         int64
	lastAcceptedIDs map[string]bool
}

func (p *gormPersistence) acceptedEventIDs() map[string]bool { return p.lastAcceptedIDs }

// NewSQLiteStore opens a single-node SQLite platform store. The driver is
// pure Go and requires neither CGO nor a system SQLite library.
func NewSQLiteStore(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("platform: empty SQLite path")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("platform: create SQLite directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("platform: open SQLite file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("platform: secure SQLite file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("platform: close SQLite file: %w", err)
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = (&url.URL{Scheme: "file", Path: path}).String()
	}
	settings := url.Values{}
	settings.Set("_txlock", "immediate")
	settings.Add("_pragma", "foreign_keys(1)")
	settings.Add("_pragma", "busy_timeout(5000)")
	if path != ":memory:" {
		settings.Add("_pragma", "journal_mode(WAL)")
	}
	return newGORMStore(ctx, sqlite.Open(dsn+"?"+settings.Encode()), "sqlite")
}

// NewGORMPostgresStore opens the PostgreSQL platform store through GORM.
func NewGORMPostgresStore(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("platform: empty PostgreSQL DSN")
	}
	return newGORMStore(ctx, postgres.Open(dsn), "postgres")
}

func newGORMStore(ctx context.Context, dialector gorm.Dialector, dialect string) (*Store, error) {
	db, err := gorm.Open(dialector, &gorm.Config{
		SkipDefaultTransaction: true,
		Logger: logger.New(
			log.New(os.Stderr, "gorm ", log.LstdFlags),
			logger.Config{LogLevel: logger.Error, IgnoreRecordNotFoundError: true},
		),
	})
	if err != nil {
		return nil, fmt.Errorf("platform: open %s: %w", dialect, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("platform: database pool: %w", err)
	}
	if dialect == "sqlite" {
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0)
	} else {
		sqlDB.SetMaxOpenConns(8)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("platform: ping %s: %w", dialect, err)
	}
	if dialect == "postgres" {
		lockCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		conn, err := sqlDB.Conn(lockCtx)
		if err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("platform: migration connection: %w", err)
		}
		const migrationLock int64 = 0x476f4c61624d6967
		if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
			_ = conn.Close()
			_ = sqlDB.Close()
			return nil, fmt.Errorf("platform: migration lock: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLock)
			_ = conn.Close()
		}()
	}
	if err := db.WithContext(ctx).AutoMigrate(&databaseState{}, &databaseEvent{}, &databasePin{}, &databaseToken{}, &databaseMetric{}, &databaseMetricDefinition{}, &databaseLease{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("platform: migrate %s: %w", dialect, err)
	}
	if err := db.WithContext(ctx).Exec(`CREATE TABLE IF NOT EXISTS collection_diagnostics (
		workspace_id text NOT NULL, environment_id text NOT NULL,
		experiment_id text NOT NULL, epoch bigint NOT NULL,
		code text NOT NULL, cnt bigint NOT NULL,
		PRIMARY KEY (workspace_id, environment_id, experiment_id, epoch, code)
	)`).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("platform: migrate collection diagnostics: %w", err)
	}
	store, err := newStore(&gormPersistence{db: db, dialect: dialect})
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return store, nil
}

func (p *gormPersistence) Close() error {
	db, err := p.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

func (p *gormPersistence) recordCollectionCounts(counts []collectionCount) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, count := range counts {
			if err := tx.Exec(`INSERT INTO collection_diagnostics
				(workspace_id, environment_id, experiment_id, epoch, code, cnt)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (workspace_id, environment_id, experiment_id, epoch, code)
				DO UPDATE SET cnt = collection_diagnostics.cnt + excluded.cnt`,
				count.WorkspaceID, count.EnvironmentID, count.ExperimentID,
				count.Epoch, count.Code, count.Count).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *gormPersistence) collectionCounts(workspaceID, environmentID, experimentID string,
	epoch int64) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var rows []struct {
		Code string
		Cnt  int64
	}
	err := p.db.WithContext(ctx).Raw(`SELECT code, cnt FROM collection_diagnostics
		WHERE workspace_id = ? AND environment_id = ? AND experiment_id = ? AND epoch = ?`,
		workspaceID, environmentID, experimentID, epoch).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.Code] = row.Cnt
	}
	return counts, nil
}

// eventDelta reads a consistent event batch and its authoritative aggregate
// rows. A control change or a gap (retention, old schema, or a large backlog)
// asks the caller to reload the whole working set instead.
func (p *gormPersistence) eventDelta(ctx context.Context, after, controlVersion int64) (databaseState, []*Event, []databaseMetric, bool, error) {
	var state databaseState
	var rows []databaseEvent
	var metrics []databaseMetric
	const maxDelta = 4096
	fullReload := false
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if p.dialect == "postgres" {
			if err := tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ").Error; err != nil {
				return err
			}
		}
		if err := tx.Select("version", "control_version").First(&state, 1).Error; err != nil {
			return err
		}
		if state.ControlVersion != controlVersion || state.Version <= after {
			return nil
		}
		if err := tx.Where("commit_version > ? AND commit_version <= ?", after, state.Version).
			Order("commit_version, commit_ordinal").Limit(maxDelta + 1).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 || len(rows) > maxDelta {
			fullReload = true
			return nil
		}
		keys := make(map[string]struct{}, len(rows)*3)
		for _, row := range rows {
			var ev Event
			if err := json.Unmarshal(row.Payload, &ev); err != nil {
				return fmt.Errorf("platform: decode delta event %s: %w", row.EventID, err)
			}
			for _, key := range metricKeysForEvent(&ev) {
				keys[key] = struct{}{}
			}
		}
		batch := make([]string, 0, 1000)
		loadBatch := func() error {
			if len(batch) == 0 {
				return nil
			}
			var found []databaseMetric
			if err := tx.Where("key IN ?", batch).Find(&found).Error; err != nil {
				return err
			}
			metrics = append(metrics, found...)
			batch = batch[:0]
			return nil
		}
		for key := range keys {
			batch = append(batch, key)
			if len(batch) == cap(batch) {
				if err := loadBatch(); err != nil {
					return err
				}
			}
		}
		return loadBatch()
	})
	if err != nil || fullReload || state.ControlVersion != controlVersion {
		return state, nil, nil, fullReload || state.ControlVersion != controlVersion, err
	}
	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		var ev Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			return state, nil, nil, false, err
		}
		events = append(events, &ev)
	}
	return state, events, metrics, false, nil
}

func metricKeysForEvent(ev *Event) []string {
	scope := fmt.Sprintf("%s/%s/%s/%d/%s", ev.WorkspaceID, ev.EnvironmentID,
		ev.ExperimentID, ev.Epoch, ev.Variant)
	if ev.Type == EventAssignment {
		return []string{scope + "/assignment"}
	}
	if ev.Type == EventExposure {
		return []string{scope + "/exposure", scope + "/" + ev.UnitHash}
	}
	return []string{scope + "/conversion", scope + "/" + ev.UnitHash + "/" + ev.MetricKey}
}

func (p *gormPersistence) Load() (*Data, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgLoadTimeout)
	defer cancel()
	var row databaseState
	var recent []databaseEvent
	var metrics []databaseMetric
	var definitions []databaseMetricDefinition
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if p.dialect == "postgres" {
			if err := tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ").Error; err != nil {
				return err
			}
		}
		if err := tx.First(&row, 1).Error; err != nil {
			return err
		}
		if err := tx.Find(&metrics).Error; err != nil {
			return err
		}
		if err := tx.Find(&definitions).Error; err != nil {
			return err
		}
		return tx.Order("server_time DESC, event_id DESC").Limit(MaxRetainedEvents).Find(&recent).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		p.version = 0
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform: load state: %w", err)
	}
	var data Data
	if err := json.Unmarshal(row.Payload, &data); err != nil {
		return nil, fmt.Errorf("platform: decode state: %w", err)
	}
	if data.SchemaVersion != dataSchemaVersion {
		return nil, fmt.Errorf("platform: schema %d, want %d", data.SchemaVersion, dataSchemaVersion)
	}
	if err := hydrateMetrics(&data, metrics); err != nil {
		return nil, err
	}
	data.MetricDefinitions = make(map[string]*MetricDefinition, len(definitions))
	for _, row := range definitions {
		def := &MetricDefinition{}
		if err := json.Unmarshal(row.Payload, def); err != nil {
			return nil, fmt.Errorf("platform: decode metric definition %s v%d: %w", row.Key, row.Version, err)
		}
		data.MetricDefinitions[metricDefinitionKey(row.WorkspaceID, row.Key, row.Version)] = def
	}
	data.Events = nil
	data.EventIndex = make(map[string]int, len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		var ev Event
		if err := json.Unmarshal(recent[i].Payload, &ev); err != nil {
			return nil, fmt.Errorf("platform: decode event %s: %w", recent[i].EventID, err)
		}
		data.EventIndex[ev.EventID] = len(data.Events)
		data.Events = append(data.Events, &ev)
	}
	p.version = row.Version
	return &data, nil
}

func hydrateMetrics(data *Data, metrics []databaseMetric) error {
	data.UnitAggs = map[string]*UnitAgg{}
	data.ExposedUnits = map[string]map[string]bool{}
	data.SplitExposedUnits = map[string]map[string]bool{}
	data.EventCounts = map[string]int64{}
	for _, metric := range metrics {
		if err := applyDatabaseMetric(data, metric); err != nil {
			return err
		}
	}
	return nil
}

func applyDatabaseMetric(data *Data, metric databaseMetric) error {
	switch metric.Category {
	case "agg":
		var value UnitAgg
		if err := json.Unmarshal(metric.Payload, &value); err != nil {
			return fmt.Errorf("platform: decode aggregate %s: %w", metric.Key, err)
		}
		data.UnitAggs[metric.Key] = &value
	case "count":
		var value int64
		if err := json.Unmarshal(metric.Payload, &value); err != nil {
			return fmt.Errorf("platform: decode count %s: %w", metric.Key, err)
		}
		data.EventCounts[metric.Key] = value
	case "exposed", "split":
		scope, unit, ok := strings.CutLast(metric.Key, "/")
		if !ok {
			return fmt.Errorf("platform: malformed exposure key %q", metric.Key)
		}
		target := data.ExposedUnits
		if metric.Category == "split" {
			target = data.SplitExposedUnits
		}
		if target[scope] == nil {
			target[scope] = map[string]bool{}
		}
		target[scope][unit] = true
	default:
		return fmt.Errorf("platform: unknown metric category %q", metric.Category)
	}
	return nil
}

func (p *gormPersistence) KnownEventIDs(ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var found []string
	if err := p.db.WithContext(ctx).Model(&databaseEvent{}).Where("event_id IN ?", ids).Pluck("event_id", &found).Error; err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(found))
	for _, id := range found {
		known[id] = true
	}
	return known, nil
}

func (p *gormPersistence) lookupAcceptedEvent(eventID string) (Event, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var row databaseEvent
	err := p.db.WithContext(ctx).First(&row, "event_id = ?", eventID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	var ev Event
	if err := json.Unmarshal(row.Payload, &ev); err != nil {
		return Event{}, false, err
	}
	return ev, true, nil
}

func (p *gormPersistence) Commit(data *Data, ch *ChangeSet) error {
	if ch.empty() {
		return nil
	}
	timeout := pgCommitTimeout
	if ch.Rotation {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	control := ch.Meta || ch.Rotation || len(ch.Audit) > 0
	for _, keys := range ch.Dirty {
		control = control || len(keys) > 0
	}
	for _, keys := range ch.Deleted {
		control = control || len(keys) > 0
	}
	accepted := make(map[string]bool, len(ch.Events))
	var committedVersion int64
	var peerChanged bool
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current databaseState
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, 1).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			current = databaseState{ID: 1}
		} else if err != nil {
			return err
		}
		if control && current.ControlVersion != data.ControlVersion {
			return ErrConflict
		}
		peerChanged = current.Version != p.version
		if ch.Rotation {
			if err := p.rotateLedger(tx, data); err != nil {
				return err
			}
		}
		for id := range ch.Deleted[collTokens] {
			if err := tx.Where("token_id = ?", id).Delete(&databaseToken{}).Error; err != nil {
				return err
			}
		}
		for id := range ch.Dirty[collTokens] {
			token := data.Tokens[id]
			if token == nil {
				return fmt.Errorf("platform: dirty token %s absent", id)
			}
			payload, err := json.Marshal(token)
			if err != nil {
				return err
			}
			row := databaseToken{SecretHash: token.SecretHash, TokenID: id, Payload: payload}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		for key := range ch.Dirty[collMetricDefinitions] {
			def := data.MetricDefinitions[key]
			if def == nil {
				return fmt.Errorf("platform: dirty metric definition %q absent", key)
			}
			payload, err := json.Marshal(def)
			if err != nil {
				return err
			}
			row := databaseMetricDefinition{WorkspaceID: def.WorkspaceID, Key: def.Key,
				Version: def.Version, Status: string(def.Status), Payload: payload}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		committedVersion = current.Version + 1
		for ordinal, ev := range ch.Events {
			payload, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			row := databaseEvent{EventID: ev.EventID, CommitVersion: committedVersion,
				CommitOrdinal: ordinal, WorkspaceID: ev.WorkspaceID,
				EnvironmentID: ev.EnvironmentID, ExperimentID: ev.ExperimentID,
				Epoch: ev.Epoch, ServerTime: ev.ServerTime, Payload: payload}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				accepted[ev.EventID] = true
				if err := p.commitEventMetric(tx, ev); err != nil {
					return err
				}
			}
		}
		if !ch.PruneEventsBefore.IsZero() {
			if err := tx.Where("server_time < ?", ch.PruneEventsBefore).Delete(&databaseEvent{}).Error; err != nil {
				return err
			}
		}
		if ch.Rotation {
			if err := p.replaceMetrics(tx, data); err != nil {
				return err
			}
		}
		persisted := *data
		persisted.Events = nil
		persisted.EventIndex = nil
		persisted.UnitAggs = nil
		persisted.ExposedUnits = nil
		persisted.SplitExposedUnits = nil
		persisted.EventCounts = nil
		persisted.CollectionCounts = nil
		persisted.MetricDefinitions = nil
		if control {
			persisted.ControlVersion++
		}
		if current.Version == 0 {
			payload, err := json.Marshal(&persisted)
			if err != nil {
				return err
			}
			return tx.Create(&databaseState{ID: 1, Version: 1, ControlVersion: persisted.ControlVersion, Payload: payload}).Error
		}
		updates := map[string]any{"version": committedVersion}
		if control {
			payload, err := json.Marshal(&persisted)
			if err != nil {
				return err
			}
			updates["control_version"] = persisted.ControlVersion
			updates["payload"] = payload
		}
		result := tx.Model(&databaseState{}).Where("id = ? AND version = ?", 1, current.Version).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return err
		}
		return fmt.Errorf("platform: commit %s: %w", p.dialect, err)
	}
	p.version = committedVersion
	if control {
		data.ControlVersion++
	}
	if peerChanged {
		// A peer may have committed events before this transaction. Polling
		// must reload the independent aggregate rows before serving reports.
		p.version = committedVersion - 1
	}
	p.lastAcceptedIDs = accepted
	return nil
}

func (p *gormPersistence) metricValue(tx *gorm.DB, category, key string) ([]byte, error) {
	var row databaseMetric
	err := tx.First(&row, "category = ? AND key = ?", category, key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row.Payload, nil
}

func (p *gormPersistence) putMetric(tx *gorm.DB, category, key string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&databaseMetric{Category: category, Key: key, Payload: payload}).Error
}

// commitEventMetric runs only after the unique raw-event insert succeeded.
// All rows are changed in the same transaction, so a retried event can never
// increment aggregates twice. The state row lock serializes peer collectors.
func (p *gormPersistence) commitEventMetric(tx *gorm.DB, ev *Event) error {
	scope := fmt.Sprintf("%s/%s/%s/%d/%s", ev.WorkspaceID, ev.EnvironmentID,
		ev.ExperimentID, ev.Epoch, ev.Variant)
	kind := string(ev.Type)
	countKey := scope + "/" + kind
	raw, err := p.metricValue(tx, "count", countKey)
	if err != nil {
		return err
	}
	var count int64
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &count); err != nil {
			return err
		}
	}
	if err := p.putMetric(tx, "count", countKey, count+1); err != nil {
		return err
	}
	if ev.Type == EventExposure {
		key := scope + "/" + ev.UnitHash
		if err := p.putMetric(tx, "exposed", key, true); err != nil {
			return err
		}
		if ev.Source == "split" {
			return p.putMetric(tx, "split", key, true)
		}
		return nil
	}
	if ev.Type == EventAssignment {
		return nil
	}
	key := scope + "/" + ev.UnitHash + "/" + ev.MetricKey
	raw, err = p.metricValue(tx, "agg", key)
	if err != nil {
		return err
	}
	var agg UnitAgg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &agg); err != nil {
			return err
		}
	} else {
		agg.First = ev.ClientTime
	}
	agg.Count++
	agg.Sum += ev.Value
	agg.SumSquares += ev.Value * ev.Value
	agg.Last = ev.ClientTime
	return p.putMetric(tx, "agg", key, &agg)
}

// replaceMetrics is an offline operation used by HMAC rotation. It rewrites
// all hash-bearing aggregate keys atomically with the raw-event ledger.
func (p *gormPersistence) replaceMetrics(tx *gorm.DB, data *Data) error {
	if err := tx.Where("1 = 1").Delete(&databaseMetric{}).Error; err != nil {
		return err
	}
	rows := make([]databaseMetric, 0, 500)
	appendMetric := func(category, key string, value any) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		rows = append(rows, databaseMetric{Category: category, Key: key, Payload: payload})
		if len(rows) < cap(rows) {
			return nil
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		rows = rows[:0]
		return nil
	}
	for key, value := range data.UnitAggs {
		if err := appendMetric("agg", key, value); err != nil {
			return err
		}
	}
	for key, value := range data.EventCounts {
		if err := appendMetric("count", key, value); err != nil {
			return err
		}
	}
	for category, groups := range map[string]map[string]map[string]bool{
		"exposed": data.ExposedUnits, "split": data.SplitExposedUnits,
	} {
		for scope, units := range groups {
			for unit := range units {
				if err := appendMetric(category, scope+"/"+unit, true); err != nil {
					return err
				}
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Create(&rows).Error
}

func (p *gormPersistence) rotateLedger(tx *gorm.DB, data *Data) error {
	if len(data.HMACKeyRing) < 2 {
		return fmt.Errorf("platform: rotation requires a new HMAC key")
	}
	key := data.HMACKeyRing[len(data.HMACKeyRing)-1]
	var rows []databaseEvent
	if err := tx.Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var ev Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			return err
		}
		ev.UnitHash = hashUnitLayer(key, ev.WorkspaceID, ev.UnitHash)
		if ev.SessionID != "" {
			ev.SessionID = hashUnitLayer(key, ev.WorkspaceID, ev.SessionID)
		}
		payload, err := json.Marshal(&ev)
		if err != nil {
			return err
		}
		if err := tx.Model(&databaseEvent{}).Where("event_id = ?", row.EventID).
			Update("payload", payload).Error; err != nil {
			return err
		}
	}
	var pins []databasePin
	if err := tx.Find(&pins).Error; err != nil {
		return err
	}
	for _, row := range pins {
		var ev Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			return err
		}
		ev.UnitHash = hashUnitLayer(key, ev.WorkspaceID, ev.UnitHash)
		payload, err := json.Marshal(&ev)
		if err != nil {
			return err
		}
		newKey := fmt.Sprintf("%s/%d/%s", ev.ExperimentID, ev.Epoch, ev.UnitHash)
		if err := tx.Delete(&databasePin{}, "key = ?", row.Key).Error; err != nil {
			return err
		}
		if err := tx.Create(&databasePin{Key: newKey, Payload: payload}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (p *gormPersistence) LookupToken(hash string) (*Token, error) {
	// Authentication reads the committed token row rather than a replica's
	// working set, so revocation is visible before control sync runs.
	var row databaseToken
	err := p.db.Select("payload").First(&row, "secret_hash = ?", hash).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: unknown or revoked token", ErrAccessDenied)
	}
	if err != nil {
		return nil, fmt.Errorf("platform: read token: %w", err)
	}
	var token Token
	if err := json.Unmarshal(row.Payload, &token); err != nil {
		return nil, fmt.Errorf("platform: decode token: %w", err)
	}
	if token.Revoked {
		return nil, fmt.Errorf("%w: unknown or revoked token", ErrAccessDenied)
	}
	return &token, nil
}

func (p *gormPersistence) pinBanditAssignment(exp *Experiment, candidate *Event) (*Event, error) {
	key := fmt.Sprintf("%s/%d/%s", exp.ID, exp.Epoch, candidate.UnitHash)
	payload, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	row := databasePin{Key: key, Payload: payload}
	if err := p.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	if err := p.db.First(&row, "key = ?", key).Error; err != nil {
		return nil, err
	}
	var pin Event
	if err := json.Unmarshal(row.Payload, &pin); err != nil {
		return nil, err
	}
	return &pin, nil
}

func (p *gormPersistence) analysisEvents(exp *Experiment, now time.Time) ([]*Event, int64, int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgLoadTimeout)
	defer cancel()
	var rows []databaseEvent
	var metrics []databaseMetric
	var state databaseState
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if p.dialect == "postgres" {
			if err := tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ").Error; err != nil {
				return err
			}
		}
		if err := tx.First(&state, 1).Error; err != nil {
			return err
		}
		prefix := fmt.Sprintf("%s/%s/%s/%d/", exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
		if err := tx.Where("key LIKE ?", prefix+"%").Find(&metrics).Error; err != nil {
			return err
		}
		return tx.Where("workspace_id = ? AND environment_id = ? AND experiment_id = ? AND epoch = ? AND server_time <= ?",
			exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch, now).
			Order("server_time, event_id").Find(&rows).Error
	})
	if err != nil {
		return nil, 0, 0, false, err
	}
	var persisted Data
	if err := json.Unmarshal(state.Payload, &persisted); err != nil {
		return nil, 0, 0, false, err
	}
	if err := hydrateMetrics(&persisted, metrics); err != nil {
		return nil, 0, 0, false, err
	}
	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		var ev Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			return nil, 0, 0, false, err
		}
		events = append(events, &ev)
	}
	prefix := fmt.Sprintf("%s/%s/%s/%d/", exp.WorkspaceID, exp.EnvironmentID, exp.ID, exp.Epoch)
	var durable int64
	for key, count := range persisted.EventCounts {
		if strings.HasPrefix(key, prefix) {
			durable += count
		}
	}
	rebuilt := &Store{data: newData()}
	for _, ev := range events {
		rebuilt.appendEventLocked(ev)
	}
	mismatch := !equalScopeCounts(prefix, rebuilt.data.EventCounts, persisted.EventCounts) ||
		!equalScopeSets(prefix, rebuilt.data.ExposedUnits, persisted.ExposedUnits) ||
		!equalScopeSets(prefix, rebuilt.data.SplitExposedUnits, persisted.SplitExposedUnits) ||
		!equalScopeAggs(prefix, rebuilt.data.UnitAggs, persisted.UnitAggs)
	return events, int64(len(events)), durable, mismatch, nil
}

func equalScopeCounts(prefix string, a, b map[string]int64) bool {
	for key, value := range a {
		if strings.HasPrefix(key, prefix) && b[key] != value {
			return false
		}
	}
	for key, value := range b {
		if strings.HasPrefix(key, prefix) && a[key] != value {
			return false
		}
	}
	return true
}

func equalScopeSets(prefix string, a, b map[string]map[string]bool) bool {
	for key, value := range a {
		if strings.HasPrefix(key, prefix) && !reflect.DeepEqual(value, b[key]) {
			return false
		}
	}
	for key, value := range b {
		if strings.HasPrefix(key, prefix) && !reflect.DeepEqual(value, a[key]) {
			return false
		}
	}
	return true
}

func equalScopeAggs(prefix string, a, b map[string]*UnitAgg) bool {
	for key, value := range a {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		other := b[key]
		if other == nil || value.Count != other.Count ||
			math.Abs(value.Sum-other.Sum) > 1e-9*math.Max(1, math.Abs(other.Sum)) ||
			math.Abs(value.SumSquares-other.SumSquares) > 1e-9*math.Max(1, math.Abs(other.SumSquares)) {
			return false
		}
	}
	for key := range b {
		if strings.HasPrefix(key, prefix) && a[key] == nil {
			return false
		}
	}
	return true
}

// LockScope uses a session advisory lock on PostgreSQL. SQLite stores are
// single-node and are already serialized by Store's mutex.
func (p *gormPersistence) LockScope(ctx context.Context, key int64) (func(), error) {
	if p.dialect == "sqlite" {
		return func() {}, nil
	}
	pool, err := p.db.DB()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("platform: acquire scope lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key)
			_ = conn.Close()
		})
	}, nil
}

func (p *gormPersistence) databaseNow(ctx context.Context, tx *gorm.DB) (time.Time, error) {
	if p.dialect == "sqlite" {
		return time.Now().UTC(), nil
	}
	var now time.Time
	err := tx.WithContext(ctx).Raw("SELECT clock_timestamp()").Scan(&now).Error
	return now, err
}

func (p *gormPersistence) acquireRollout(ctx context.Context, holder string, ttl time.Duration) (int64, bool, error) {
	if holder == "" || ttl <= 0 {
		return 0, false, fmt.Errorf("platform: invalid rollout lease")
	}
	if p.dialect == "sqlite" {
		return 1, true, nil
	}
	var generation int64
	var acquired bool
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := p.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		seed := databaseLease{ID: 1, Holder: "", Generation: 0, ExpiresAt: now, LastError: ""}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var lease databaseLease
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lease, 1).Error; err != nil {
			return err
		}
		now, err = p.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if lease.Holder != holder && lease.ExpiresAt.After(now) {
			return nil
		}
		if !lease.ExpiresAt.After(now) {
			lease.Generation++
			lease.LastTakeoverAt = &now
		}
		lease.Holder = holder
		lease.ExpiresAt = now.Add(ttl)
		if err := tx.Save(&lease).Error; err != nil {
			return err
		}
		generation, acquired = lease.Generation, true
		return nil
	})
	return generation, acquired, err
}

func (p *gormPersistence) checkRollout(ctx context.Context, holder string, generation int64) error {
	if p.dialect == "sqlite" {
		return nil
	}
	now, err := p.databaseNow(ctx, p.db)
	if err != nil {
		return err
	}
	var count int64
	err = p.db.WithContext(ctx).Model(&databaseLease{}).
		Where("id = ? AND holder = ? AND generation = ? AND expires_at > ?", 1, holder, generation, now).Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrRolloutLeaseLost
	}
	return nil
}

func (p *gormPersistence) finishRollout(ctx context.Context, holder string, generation int64, runError string) error {
	if p.dialect == "sqlite" {
		return nil
	}
	now, err := p.databaseNow(ctx, p.db)
	if err != nil {
		return err
	}
	result := p.db.WithContext(ctx).Model(&databaseLease{}).
		Where("id = ? AND holder = ? AND generation = ? AND expires_at > ?", 1, holder, generation, now).
		Updates(map[string]any{"last_run_at": now, "last_error": runError})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRolloutLeaseLost
	}
	return nil
}

func (p *gormPersistence) rolloutStatus(ctx context.Context) (RolloutLeadership, error) {
	if p.dialect == "sqlite" {
		return RolloutLeadership{Holder: "local"}, nil
	}
	var lease databaseLease
	err := p.db.WithContext(ctx).First(&lease, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RolloutLeadership{}, nil
	}
	if err != nil {
		return RolloutLeadership{}, err
	}
	now, err := p.databaseNow(ctx, p.db)
	if err != nil {
		return RolloutLeadership{}, err
	}
	status := RolloutLeadership{Holder: lease.Holder, Generation: lease.Generation,
		ExpiresAt: lease.ExpiresAt, LastRunAt: lease.LastRunAt,
		LastTakeoverAt: lease.LastTakeoverAt, LastError: lease.LastError}
	if !lease.ExpiresAt.After(now) {
		status.Holder = ""
	}
	return status, nil
}

var _ Persistence = (*gormPersistence)(nil)
var _ eventDedup = (*gormPersistence)(nil)
var _ scopeLocker = (*gormPersistence)(nil)
var _ rolloutLeaseBackend = (*gormPersistence)(nil)
