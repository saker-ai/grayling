package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	agentclient "github.com/saker-ai/grayling/pkg/agent"
)

type Permission uint64

const (
	ConfigRead Permission = 1 << iota
	DiscoveryRead
	InstanceRegister
	InstanceRenew
	ReleaseReport
	EventsWrite
	allPermissions = ConfigRead | DiscoveryRead | InstanceRegister | InstanceRenew | ReleaseReport | EventsWrite
)
const sessionTTL = 30 * time.Second

//go:embed sessions.sql
var sessionSchema string

//go:embed resources.sql
var resourceSchema string

//go:embed admin.sql
var adminSchema string

//go:embed registration.sql
var registrationSchema string

//go:embed discovery.sql
var discoverySchema string

// Sessions owns shared governance state. There is intentionally no in-memory
// fallback: a failed primary DB cannot authenticate or advance connection epochs.
type Sessions struct{ pool *pgxpool.Pool }

func OpenSessions(ctx context.Context, dsn string) (*Sessions, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("agent: postgres configuration: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Sessions{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}
func (s *Sessions) Close() { s.pool.Close() }
func (s *Sessions) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(714209161)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sessionSchema); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT version FROM agent_schema WHERE id=true`).Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		if _, err := tx.Exec(ctx, resourceSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=2 WHERE id=true`); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		if _, err := tx.Exec(ctx, adminSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=3 WHERE id=true`); err != nil {
			return err
		}
		version = 3
	}
	if version == 3 {
		if _, err := tx.Exec(ctx, registrationSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=4 WHERE id=true`); err != nil {
			return err
		}
		version = 4
	}
	if version == 4 {
		if _, err := tx.Exec(ctx, discoverySchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=5 WHERE id=true`); err != nil {
			return err
		}
		version = 5
	}
	if version == 5 {
		if _, err := tx.Exec(ctx, releaseSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=6 WHERE id=true`); err != nil {
			return err
		}
		version = 6
	}
	if version == 6 {
		if _, err := tx.Exec(ctx, preparedSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=7 WHERE id=true`); err != nil {
			return err
		}
		version = 7
	}
	if version == 7 {
		if _, err := tx.Exec(ctx, preparationOutcomesSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=8 WHERE id=true`); err != nil {
			return err
		}
		version = 8
	}
	if version == 8 {
		if _, err := tx.Exec(ctx, activationSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=9 WHERE id=true`); err != nil {
			return err
		}
		version = 9
	}
	if version == 9 {
		if _, err := tx.Exec(ctx, activationReportsSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=10 WHERE id=true`); err != nil {
			return err
		}
		version = 10
	}
	if version == 10 {
		if _, err := tx.Exec(ctx, activationVersionsSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_schema SET version=11 WHERE id=true`); err != nil {
			return err
		}
		version = 11
	}
	if version != 11 {
		return errors.New("agent: unsupported database schema")
	}
	return tx.Commit(ctx)
}
func protocolError(code agentv1.ErrorCode, reason string) error {
	return &agentv1.ProtocolError{Code: code, Reason: reason}
}
func denied() error {
	return protocolError(agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "operation not authorized")
}
func unauthenticated() error {
	return protocolError(agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED, "invalid agent credential")
}
func fenced() error {
	return protocolError(agentv1.ErrorCode_ERROR_CODE_FENCED, "session superseded or expired")
}
func identifier(s string) bool {
	if s == "" || len(s) > 128 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func scopeValid(s agentclient.Scope) bool {
	return identifier(s.WorkspaceID) && identifier(s.EnvironmentID) && identifier(s.ServiceKey)
}
func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Grant is created only by an authorized control-plane provisioning action.
// BootID is deployment authorization, not a self-reported permission. Replacing
// another boot additionally requires an explicit expected old boot, checked under
// the same row lock as the connection epoch update.
type Grant struct {
	ID, PrincipalID                                 string
	Scope                                           agentclient.Scope
	AgentID, InstanceID, BootID, TakeoverFromBootID string
	Permissions                                     Permission
	ExpiresAt                                       time.Time
}

// SetScopeLimit configures explicit admission; no scope is enabled implicitly.
func (s *Sessions) SetScopeLimit(ctx context.Context, scope agentclient.Scope, maxConnections int) error {
	if !scopeValid(scope) || maxConnections < 1 {
		return denied()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_scope_limits VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,environment_id,service_key) DO UPDATE SET max_connections=EXCLUDED.max_connections`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, maxConnections)
	return err
}

// IssueCredential returns a secret once. Only its SHA-256 digest is persisted.
func (s *Sessions) IssueCredential(ctx context.Context, g Grant) (string, string, error) {
	return issueCredential(ctx, s.pool, g)
}
func issueCredential(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, g Grant) (string, string, error) {
	if !identifier(g.PrincipalID) || !scopeValid(g.Scope) || !identifier(g.AgentID) || !identifier(g.InstanceID) || !identifier(g.BootID) || (g.TakeoverFromBootID != "" && !identifier(g.TakeoverFromBootID)) || g.Permissions == 0 || g.Permissions & ^allPermissions != 0 || g.ExpiresAt.IsZero() {
		return "", "", denied()
	}
	id, err := randomID()
	if err != nil {
		return "", "", err
	}
	secret, err := randomID()
	if err != nil {
		return "", "", err
	}
	secret = "grayling_agent_" + secret
	hash := sha256.Sum256([]byte(secret))
	result, err := q.Exec(ctx, `INSERT INTO agent_credentials(id,secret_hash,principal_id,workspace_id,environment_id,service_key,agent_id,instance_id,boot_id,takeover_from_boot_id,permissions,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12 WHERE $12>clock_timestamp()`, id, hash[:], g.PrincipalID, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey, g.AgentID, g.InstanceID, g.BootID, g.TakeoverFromBootID, int64(g.Permissions), g.ExpiresAt)
	if err != nil {
		return "", "", err
	}
	if result.RowsAffected() != 1 {
		return "", "", denied()
	}
	return id, secret, nil
}
func (s *Sessions) RevokeCredential(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE agent_credentials SET revoked=true WHERE id=$1`, id)
	return err
}
func credential(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, secret string, lock bool) (Grant, error) {
	if !strings.HasPrefix(secret, "grayling_agent_") || len(secret) != len("grayling_agent_")+43 {
		return Grant{}, unauthenticated()
	}
	hash := sha256.Sum256([]byte(secret))
	query := `SELECT id,principal_id,workspace_id,environment_id,service_key,agent_id,instance_id,boot_id,takeover_from_boot_id,permissions,expires_at FROM agent_credentials WHERE secret_hash=$1 AND NOT revoked AND expires_at>clock_timestamp()`
	if lock {
		query += " FOR SHARE"
	}
	var g Grant
	var p int64
	err := q.QueryRow(ctx, query, hash[:]).Scan(&g.ID, &g.PrincipalID, &g.Scope.WorkspaceID, &g.Scope.EnvironmentID, &g.Scope.ServiceKey, &g.AgentID, &g.InstanceID, &g.BootID, &g.TakeoverFromBootID, &p, &g.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, unauthenticated()
	}
	if err != nil {
		return Grant{}, err
	}
	g.Permissions = Permission(p)
	return g, nil
}
func (s *Sessions) Authenticate(ctx context.Context, secret string) (Grant, error) {
	return credential(ctx, s.pool, secret, false)
}

type Session struct {
	ID        string
	Epoch     uint64
	Grant     Grant
	ExpiresAt time.Time
}

// Connect serializes scope admission and instance takeover in the primary DB.
// Failed authentication, capacity checks or boot CAS never change the old epoch.
func (s *Sessions) Connect(ctx context.Context, secret string, h *agentv1.Hello) (Session, error) {
	if h == nil {
		return Session{}, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	g, err := credential(ctx, tx, secret, true)
	if err != nil {
		return Session{}, err
	}
	if h.AgentId != g.AgentID || h.InstanceId != g.InstanceID || h.BootId != g.BootID {
		return Session{}, denied()
	}
	var maxConnections int
	err = tx.QueryRow(ctx, `SELECT max_connections FROM agent_scope_limits WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey).Scan(&maxConnections)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, denied()
	}
	if err != nil {
		return Session{}, err
	}
	var oldBoot, oldAgent, oldPrincipal string
	var epoch int64
	err = tx.QueryRow(ctx, `SELECT s.boot_id,s.agent_id,c.principal_id,s.connection_epoch FROM agent_sessions s JOIN agent_credentials c ON c.id=s.credential_id WHERE s.workspace_id=$1 AND s.environment_id=$2 AND s.service_key=$3 AND s.instance_id=$4 FOR UPDATE OF s`, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey, g.InstanceID).Scan(&oldBoot, &oldAgent, &oldPrincipal, &epoch)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Session{}, err
	}
	if epoch == 0 && g.TakeoverFromBootID != "" {
		return Session{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "expected deployment instance missing")
	}
	if epoch != 0 {
		if oldBoot == g.BootID {
			if oldAgent != g.AgentID || oldPrincipal != g.PrincipalID {
				return Session{}, denied()
			}
		} else if g.TakeoverFromBootID != oldBoot {
			return Session{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "deployment takeover not authorized")
		}
	}
	if epoch == math.MaxInt64 {
		return Session{}, fenced()
	}
	var connections int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_sessions WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND instance_id<>$4 AND expires_at>clock_timestamp()`, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey, g.InstanceID).Scan(&connections); err != nil {
		return Session{}, err
	}
	if connections >= maxConnections {
		return Session{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "scope connection limit")
	}
	id, err := randomID()
	if err != nil {
		return Session{}, err
	}
	result := Session{ID: id, Epoch: uint64(epoch + 1), Grant: g}
	err = tx.QueryRow(ctx, `INSERT INTO agent_sessions(workspace_id,environment_id,service_key,instance_id,agent_id,boot_id,credential_id,session_id,connection_epoch,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,LEAST(clock_timestamp()+interval '30 seconds',$10)) ON CONFLICT(workspace_id,environment_id,service_key,instance_id) DO UPDATE SET agent_id=EXCLUDED.agent_id,boot_id=EXCLUDED.boot_id,credential_id=EXCLUDED.credential_id,session_id=EXCLUDED.session_id,connection_epoch=EXCLUDED.connection_epoch,expires_at=EXCLUDED.expires_at RETURNING expires_at`, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey, g.InstanceID, g.AgentID, g.BootID, g.ID, id, epoch+1, g.ExpiresAt).Scan(&result.ExpiresAt)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	return result, nil
}

// CheckSession is a fresh primary read, suitable for proactive fencing/revocation
// detection. Mutations must use lockSession in their own transaction instead.
func (s *Sessions) CheckSession(ctx context.Context, se Session) error {
	if se.Epoch == 0 || se.Epoch > math.MaxInt64 {
		return fenced()
	}
	var valid bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions s JOIN agent_credentials c ON c.id=s.credential_id WHERE s.session_id=$1 AND s.connection_epoch=$2 AND c.id=$3 AND NOT c.revoked AND c.expires_at>clock_timestamp() AND s.expires_at>clock_timestamp())`, se.ID, int64(se.Epoch), se.Grant.ID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fenced()
	}
	return nil
}
func lockSession(ctx context.Context, tx pgx.Tx, se Session, permission Permission) error {
	if se.Epoch == 0 || se.Epoch > math.MaxInt64 || permission & ^allPermissions != 0 {
		return fenced()
	}
	var allowed int64
	err := tx.QueryRow(ctx, `SELECT permissions FROM agent_credentials WHERE id=$1 AND NOT revoked AND expires_at>clock_timestamp() FOR SHARE`, se.Grant.ID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return unauthenticated()
	}
	if err != nil {
		return err
	}
	if Permission(allowed)&permission != permission {
		return denied()
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT true FROM agent_sessions WHERE session_id=$1 AND connection_epoch=$2 AND credential_id=$3 AND expires_at>clock_timestamp() FOR UPDATE`, se.ID, int64(se.Epoch), se.Grant.ID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return fenced()
	}
	return err
}

// Heartbeat extends link liveness only, never instance leases or signed policy.
func (s *Sessions) Heartbeat(ctx context.Context, se Session) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, 0); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET expires_at=LEAST(clock_timestamp()+interval '30 seconds',(SELECT expires_at FROM agent_credentials WHERE id=$2)) WHERE session_id=$1`, se.ID, se.Grant.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Sessions) Disconnect(ctx context.Context, se Session) error {
	// Only expires the exact connection. A late close cannot kill its successor.
	_, err := s.pool.Exec(ctx, `UPDATE agent_sessions SET expires_at=LEAST(expires_at,clock_timestamp()) WHERE session_id=$1 AND connection_epoch=$2 AND credential_id=$3`, se.ID, int64(se.Epoch), se.Grant.ID)
	return err
}

func (s *Sessions) ServerState(ctx context.Context) (uint64, time.Time, error) {
	var epoch int64
	var now time.Time
	err := s.pool.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true`).Scan(&epoch, &now)
	return uint64(epoch), now, err
}

//go:embed release.sql
var releaseSchema string

//go:embed prepared.sql
var preparedSchema string

//go:embed preparation_outcomes.sql
var preparationOutcomesSchema string

//go:embed activation.sql
var activationSchema string

//go:embed activation_reports.sql
var activationReportsSchema string

//go:embed activation_versions.sql
var activationVersionsSchema string
