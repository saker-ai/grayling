// Package growthservice implements the optional, isolated Growth Copilot service.
package growthservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("growth service: version or state conflict")
var ErrInput = errors.New("growth service: invalid input")

// A Task is a versioned aggregate. Every mutation and its audit checkpoint commit
// in one SQLite transaction. No credentials, raw events or model prompts belong here.
type Task struct {
	Readiness            *growth.ProposalReview     `json:"readiness,omitempty"`
	Remediation          string                     `json:"remediation,omitempty"`
	PreparedFlagRevision int64                      `json:"preparedFlagRevision,omitempty"`
	SubmissionConfirmed  bool                       `json:"submissionConfirmed"`
	NextObservationAt    time.Time                  `json:"nextObservationAt,omitempty"`
	Observation          *growth.MonitorSummary     `json:"observation,omitempty"`
	ID                   string                     `json:"id"`
	Revision             int64                      `json:"revision"`
	Goal                 growth.GrowthGoal          `json:"goal"`
	Proposal             growth.Proposal            `json:"proposal"`
	Manifest             *growth.MonitorManifest    `json:"manifest,omitempty"`
	Evidence             *growth.Envelope           `json:"evidence,omitempty"`
	Suggestions          []Suggestion               `json:"suggestions"`
	Review               *growth.ReviewRecord       `json:"review,omitempty"`
	Prepared             *growth.PreparedExperiment `json:"prepared,omitempty"`
	Learning             *growth.LearningRecord     `json:"learning,omitempty"`
	Rollout              *growth.RolloutObservation `json:"rollout,omitempty"`
	State                string                     `json:"state"`
	ErrorCode            string                     `json:"errorCode,omitempty"`
	Attempts             int                        `json:"attempts"`
	ModelCalls           int                        `json:"modelCalls"`
	ReservedEpoch        int64                      `json:"reservedEpoch,omitempty"`
	SubmissionHash       string                     `json:"submissionHash,omitempty"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
}

type Audit struct {
	ProposalVersion     int64     `json:"proposalVersion"`
	ProposalFingerprint string    `json:"proposalFingerprint"`
	GoalFingerprint     string    `json:"goalFingerprint"`
	Checkpoint          string    `json:"checkpoint"`
	Revision            int64     `json:"revision"`
	Actor               string    `json:"actor"`
	Action              string    `json:"action"`
	State               string    `json:"state"`
	At                  time.Time `json:"at"`
	Previous            string    `json:"previous"`
	Hash                string    `json:"hash"`
}

type Store struct {
	db   *sql.DB
	lock *os.File
	mu   sync.Mutex
}

// Open requires an owned directory. flock excludes a second service, including
// after a crash; SQLite FULL synchronous commits protect completed checkpoints.
func Open(ctx context.Context, path string) (*Store, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !filepath.IsAbs(path) {
		return nil, ErrInput
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dirInfo, e := os.Lstat(filepath.Dir(path))
	if e != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, ErrInput
	}
	if stat, ok := dirInfo.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrInput
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	lockInfo, le := f.Stat()
	if le != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, ErrInput
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrConflict
	}
	dbfile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		f.Close()
		return nil, err
	}
	info, statErr := dbfile.Stat()
	dbfile.Close()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, ErrInput
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		f.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: f}
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA synchronous=FULL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, workspace TEXT NOT NULL, environment TEXT NOT NULL, request_key TEXT UNIQUE NOT NULL, input_hash TEXT NOT NULL, body BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit(task TEXT NOT NULL, revision INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(task,revision))`,
		`CREATE TABLE IF NOT EXISTS proposals(task TEXT NOT NULL, version INTEGER NOT NULL, fingerprint TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(task,version))`,
		`CREATE TABLE IF NOT EXISTS learning(task TEXT NOT NULL, fingerprint TEXT PRIMARY KEY, body BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS scope(singleton INTEGER PRIMARY KEY CHECK(singleton=1), workspace TEXT NOT NULL, environment TEXT NOT NULL)`} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err = s.Verify(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { e := s.db.Close(); s.lock.Close(); return e }
func digest(v any) string {
	b, _ := json.Marshal(v, json.Deterministic(true))
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func (s *Store) bind(ctx context.Context, workspace, environment string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO scope VALUES(1,?,?)`, workspace, environment)
	if err != nil {
		return err
	}
	var w, e string
	err = s.db.QueryRowContext(ctx, `SELECT workspace,environment FROM scope`).Scan(&w, &e)
	if err != nil {
		return err
	}
	if w != workspace || e != environment {
		return ErrConflict
	}
	return nil
}
func (s *Store) get(ctx context.Context, id string) (*Task, error) {
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM tasks WHERE id=?`, id).Scan(&b); err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
func (s *Store) list(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM tasks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		var b []byte
		var t Task
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}
func (s *Store) history(ctx context.Context, id string) ([]Audit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM audit WHERE task=? ORDER BY revision`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Audit{}
	for rows.Next() {
		var b []byte
		var a Audit
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) create(ctx context.Context, t *Task, key, actor string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := digest(struct {
		Goal     growth.GrowthGoal
		Manifest *growth.MonitorManifest
	}{t.Goal, t.Manifest})
	var id, oldHash string
	err := s.db.QueryRowContext(ctx, `SELECT id,input_hash FROM tasks WHERE request_key=?`, key).Scan(&id, &oldHash)
	if err == nil {
		if oldHash != hash {
			return nil, ErrConflict
		}
		return s.get(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t.Revision = 1
	t.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tasks VALUES(?,?,?,?,?,?)`, t.ID, t.Goal.WorkspaceID, t.Goal.EnvironmentID, key, hash, b); err != nil {
		return nil, err
	}
	if err = appendAudit(ctx, tx, t, actor, "create"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}
func appendAudit(ctx context.Context, tx *sql.Tx, t *Task, actor, action string) error {
	var previous string
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT body FROM audit WHERE task=? ORDER BY revision DESC LIMIT 1`, t.ID).Scan(&b)
	if err == nil {
		var a Audit
		if err = json.Unmarshal(b, &a); err != nil {
			return err
		}
		previous = a.Hash
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	a := Audit{Revision: t.Revision, Actor: actor, Action: action, State: t.State, At: t.UpdatedAt, Previous: previous}
	pf, err := growth.ProposalFingerprint(t.Proposal)
	if err != nil {
		return err
	}
	gf, err := growth.GoalFingerprint(t.Goal)
	if err != nil {
		return err
	}
	a.ProposalVersion = t.Proposal.Version
	a.ProposalFingerprint = pf
	a.GoalFingerprint = gf
	proposalBytes, err := json.Marshal(t.Proposal)
	if err != nil {
		return err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM proposals WHERE task=? AND version=?`, t.ID, t.Proposal.Version).Scan(&existing)
	if err == nil && existing != pf {
		return ErrConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO proposals VALUES(?,?,?,?)`, t.ID, t.Proposal.Version, pf, proposalBytes); err != nil {
		return err
	}
	if t.Learning != nil {
		learningBytes, e := json.Marshal(t.Learning)
		if e != nil {
			return e
		}
		key := digest(t.Learning)
		var count, total int
		if e = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(body)),0) FROM learning WHERE fingerprint != ?`, key).Scan(&count, &total); e != nil {
			return e
		}
		if count >= growth.MaxLearningSearchFiles || total+len(learningBytes) > growth.MaxLearningSearchBytes {
			return ErrConflict
		}
		if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO learning VALUES(?,?,?)`, t.ID, key, learningBytes); e != nil {
			return e
		}
	}
	a.Checkpoint = digest(t)
	a.Hash = digest(a)
	b, err = json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit VALUES(?,?,?)`, t.ID, t.Revision, b)
	return err
}
func (s *Store) change(ctx context.Context, id string, revision int64, actor, action string, fn func(*Task) error) (*Task, error) {
	// Completion checkpoints and audit must survive an HTTP timeout after a
	// successful external write. Bound the detached commit independently.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Revision != revision {
		return nil, ErrConflict
	}
	if err = fn(t); err != nil {
		return nil, err
	}
	t.Revision++
	t.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET body=? WHERE id=?`, b, id); err != nil {
		return nil, err
	}
	if err = appendAudit(ctx, tx, t, actor, action); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}

// Verify detects incomplete restore, altered checkpoints, truncated audit chains
// and wrong task revisions. Hashes detect corruption, not a hostile DB owner.
func (s *Store) Verify(ctx context.Context) error {
	tasks, err := s.list(ctx)
	if err != nil {
		return err
	}
	pins := map[string]map[int64]string{}
	for _, t := range tasks {
		versions, e := s.versions(ctx, t.ID)
		if e != nil {
			return e
		}
		pins[t.ID] = map[int64]string{}
		for _, p := range versions {
			fp, e := growth.ProposalFingerprint(p)
			if e != nil {
				return e
			}
			pins[t.ID][p.Version] = fp
		}
		goalHash, e := growth.GoalFingerprint(t.Goal)
		if e != nil {
			return e
		}
		history, e := s.history(ctx, t.ID)
		if e != nil {
			return e
		}
		previous := ""
		for i, a := range history {
			hash := a.Hash
			a.Hash = ""
			if a.Revision != int64(i+1) || a.Previous != previous || digest(a) != hash || pins[t.ID][a.ProposalVersion] != a.ProposalFingerprint || a.GoalFingerprint != goalHash {
				return ErrConflict
			}
			previous = hash
		}
		if len(history) == 0 || int64(len(history)) != t.Revision || history[len(history)-1].Checkpoint != digest(t) {
			return ErrConflict
		}
	}
	records, err := s.journal(ctx)
	if err != nil {
		return err
	}
	for _, r := range records {
		if pins[r.ProposalID][r.ProposalVersion] != r.ProposalFingerprint {
			return ErrConflict
		}
	}
	return nil
}

// Backup creates a consistent SQLite snapshot with no WAL dependency. Keep it
// private and restore only into a stopped service's owned data directory.
func (s *Store) Backup(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !filepath.IsAbs(path) {
		return ErrInput
	}
	info, e := os.Lstat(filepath.Dir(path))
	if e != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInput
	}
	if err := s.Verify(ctx); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	f.Close()
	if _, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func (s *Store) versions(ctx context.Context, id string) ([]growth.Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM proposals WHERE task=? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []growth.Proposal{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		p, e := growth.DecodeProposal(bytes.NewReader(b))
		if e != nil {
			return nil, e
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
func (s *Store) journal(ctx context.Context) ([]growth.LearningRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task,fingerprint,body FROM learning ORDER BY fingerprint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []growth.LearningRecord{}
	for rows.Next() {
		var b []byte
		var id, key string
		if err = rows.Scan(&id, &key, &b); err != nil {
			return nil, err
		}
		record, e := growth.DecodeLearningRecord(bytes.NewReader(b))
		if e != nil {
			return nil, e
		}
		if key != digest(record) || record.ProposalID != id {
			return nil, ErrConflict
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}
