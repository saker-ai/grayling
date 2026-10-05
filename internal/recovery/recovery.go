// Package recovery records and verifies a quiesced GoLab recovery point.
package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/model"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

type Environment struct {
	Namespace string                `json:"namespace"`
	Revision  int64                 `json:"revision"`
	Flags     []model.ManifestEntry `json:"flags"`
	Segments  []model.ManifestEntry `json:"segments"`
}

// Point binds the three recovery components without placing plaintext keys in
// the checkpoint. The encrypted archive must still contain the actual keys.
type Point struct {
	Version        int           `json:"version"`
	AuthSHA256     string        `json:"authSha256"`
	HMACKeysSHA256 string        `json:"hmacKeysSha256"`
	Migrations     []int         `json:"migrations"`
	ControlVersion int64         `json:"controlVersion"`
	EventCount     int64         `json:"eventCount"`
	TokenCount     int64         `json:"tokenCount"`
	PostgresSHA256 string        `json:"postgresSha256"`
	GonacosSHA256  string        `json:"gonacosSha256"`
	Environments   []Environment `json:"environments"`
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// DigestFile binds the complete persisted gonacos envelope, including its
// Auth and AI service data, to an embedded-mode recovery point.
func DigestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("recovery: open gonacos snapshot: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("recovery: digest gonacos snapshot: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DigestRedisSnapshot binds the persisted envelope in external Redis mode.
// The key is gonacos's native snapshot key; a missing key is never a valid
// recovery point.
func DigestRedisSnapshot(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	b, err := client.Get(ctx, "gonacos:snapshot").Bytes()
	if err != nil {
		return "", fmt.Errorf("recovery: read gonacos Redis snapshot: %w", err)
	}
	if len(b) == 0 {
		return "", fmt.Errorf("recovery: empty gonacos Redis snapshot")
	}
	return digest(string(b)), nil
}

// DigestSnapshot hashes the entire gonacos service state (including AI and
// Auth), ignoring the envelope's changing creation time. JSON normalization
// makes freshly captured structs and restored maps byte-identical.
func DigestSnapshot(services any) (string, error) {
	b, err := json.Marshal(services, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("recovery: encode gonacos snapshot: %w", err)
	}
	var normalized any
	if err := json.Unmarshal(b, &normalized); err != nil {
		return "", fmt.Errorf("recovery: normalize gonacos snapshot: %w", err)
	}
	b, err = json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return digest(string(b)), nil
}

// Capture reads the installed store and substrate. Operators must stop all
// writers before calling it, including collectors, reconcilers and rollout.
func Capture(ctx context.Context, dsn, authSecret string, store *platform.Store, cfg configstore.Store) (*Point, error) {
	if authSecret == "" {
		return nil, fmt.Errorf("recovery: -auth-secret is required; restore the escrowed secret")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("recovery: connect postgres: %w", err)
	}
	defer conn.Close(ctx)
	p := &Point{Version: 1, AuthSHA256: digest(authSecret)}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("recovery: schema migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		p.Migrations = append(p.Migrations, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := conn.QueryRow(ctx, `SELECT control_version FROM meta WHERE id=1`).Scan(&p.ControlVersion); err != nil {
		return nil, fmt.Errorf("recovery: control version: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM event_index`).Scan(&p.EventCount); err != nil {
		return nil, fmt.Errorf("recovery: event ledger: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM tokens`).Scan(&p.TokenCount); err != nil {
		return nil, fmt.Errorf("recovery: token count: %w", err)
	}
	p.PostgresSHA256, err = postgresDigest(ctx, conn)
	if err != nil {
		return nil, err
	}
	rows, err = conn.Query(ctx, `SELECT key_hex FROM hmac_keys ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("recovery: HMAC key ring: %w", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(keys) == 0 {
		return nil, fmt.Errorf("recovery: missing PostgreSQL HMAC key ring")
	}
	keyJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	p.HMACKeysSHA256 = digest(string(keyJSON))
	for _, ws := range store.ListWorkspaces() {
		for _, env := range store.ListEnvironments(ws.ID) {
			rev := store.ManifestRevision(ws.ID, env.ID)
			if rev == 0 {
				continue
			}
			m, err := cfg.GetManifest(env.NamespaceID)
			if err != nil {
				return nil, fmt.Errorf("recovery: %s manifest: %w", env.NamespaceID, err)
			}
			if m.Revision != rev {
				return nil, fmt.Errorf("recovery: %s manifest revision %d, postgres %d; restore a matching pair", env.NamespaceID, m.Revision, rev)
			}
			if _, err := cfg.LoadSnapshot(env.NamespaceID); err != nil {
				return nil, fmt.Errorf("recovery: %s incomplete snapshot: %w", env.NamespaceID, err)
			}
			for _, entry := range m.Flags {
				if entry.Deleted {
					continue
				}
				got := store.CurrentFlagRevision(ws.ID, env.ID, entry.Key)
				if got != entry.Revision {
					return nil, fmt.Errorf("recovery: %s flag %s revision %d, postgres %d", env.NamespaceID, entry.Key, entry.Revision, got)
				}
			}
			p.Environments = append(p.Environments, Environment{Namespace: env.NamespaceID, Revision: rev, Flags: m.Flags, Segments: m.Segments})
		}
	}
	slices.SortFunc(p.Environments, func(a, b Environment) int {
		if a.Namespace < b.Namespace {
			return -1
		}
		if a.Namespace > b.Namespace {
			return 1
		}
		return 0
	})
	return p, nil
}

// postgresDigest covers contents rather than only row counts. The operator
// quiesces writes before capture; deterministic row ordering makes a dump and
// restore of the same PostgreSQL major version reproduce the same digest.
func postgresDigest(ctx context.Context, conn *pgx.Conn) (string, error) {
	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_class c
JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname='public' AND c.relkind IN ('r','p')
AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid=c.oid)
ORDER BY c.relname`)
	if err != nil {
		return "", fmt.Errorf("recovery: list postgres tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	h := sha256.New()
	for _, table := range tables {
		writeHashPart(h, table)
		query := `SELECT row_to_json(t)::text FROM ` + (pgx.Identifier{"public", table}).Sanitize() + ` AS t ORDER BY row_to_json(t)::text`
		items, err := conn.Query(ctx, query)
		if err != nil {
			return "", fmt.Errorf("recovery: digest table %s: %w", table, err)
		}
		for items.Next() {
			var row string
			if err := items.Scan(&row); err != nil {
				items.Close()
				return "", err
			}
			writeHashPart(h, row)
		}
		if err := items.Err(); err != nil {
			items.Close()
			return "", err
		}
		items.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeHashPart(h hash.Hash, value string) {
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

func Write(path string, p *Point) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".golab-recovery-point-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func Verify(path string, actual *Point) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recovery: read checkpoint: %w", err)
	}
	var want Point
	if err := json.Unmarshal(b, &want); err != nil {
		return fmt.Errorf("recovery: invalid checkpoint: %w", err)
	}
	if want.Version != 1 || want.AuthSHA256 == "" || want.HMACKeysSHA256 == "" || want.PostgresSHA256 == "" || want.GonacosSHA256 == "" {
		return fmt.Errorf("recovery: incomplete or unsupported checkpoint")
	}
	if want.AuthSHA256 != actual.AuthSHA256 {
		return fmt.Errorf("recovery: auth secret mismatch; restore escrowed -auth-secret")
	}
	if want.HMACKeysSHA256 != actual.HMACKeysSHA256 {
		return fmt.Errorf("recovery: PostgreSQL HMAC key ring mismatch; restore matching database backup")
	}
	if want.GonacosSHA256 != actual.GonacosSHA256 {
		return fmt.Errorf("recovery: gonacos Snapshot mismatch; restore the exact Snapshot from the same archive")
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		return err
	}
	wantJSON, err := json.Marshal(&want)
	if err != nil {
		return err
	}
	if !bytes.Equal(wantJSON, actualJSON) {
		return fmt.Errorf("recovery: PostgreSQL/gonacos recovery point mismatch (schema, control version, event ledger, tokens or manifests); restore all components from the same archive")
	}
	return nil
}
