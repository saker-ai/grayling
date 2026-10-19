package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
)

// AdminMutation comes only from authenticated management context, never body
// fields. The API must authorize the owner's workspace before invoking it.
type AdminMutation struct{ ActorID, ActorRole, OperationID string }
type CredentialInfo struct {
	ID                 string       `json:"id"`
	PrincipalID        string       `json:"principalId"`
	Scope              client.Scope `json:"scope"`
	AgentID            string       `json:"agentId"`
	InstanceID         string       `json:"instanceId"`
	BootID             string       `json:"bootId"`
	TakeoverFromBootID string       `json:"takeoverFromBootId,omitempty"`
	Permissions        []string     `json:"permissions"`
	ExpiresAt          time.Time    `json:"expiresAt"`
	Revoked            bool         `json:"revoked"`
}
type IssuedCredential struct {
	Credential CredentialInfo `json:"credential"`
	// Empty on replay. Secrets are never present in dedup results or audits.
	Secret   string `json:"secret,omitempty"`
	Replayed bool   `json:"replayed"`
}
type ScopeLimit struct {
	MaxConnections int `json:"maxConnections"`
}
type AdminAudit struct {
	ID          string    `json:"id"`
	OccurredAt  time.Time `json:"occurredAt"`
	ActorID     string    `json:"actorId"`
	ActorRole   string    `json:"actorRole"`
	OperationID string    `json:"operationId"`
	Action      string    `json:"action"`
	ObjectID    string    `json:"objectId"`
}

var permissionNames = []struct {
	bit  Permission
	name string
}{
	{ConfigRead, "config.read"},
	{DiscoveryRead, "discovery.read"},
	{InstanceRegister, "instance.register"},
	{InstanceRenew, "instance.renew"},
	{ReleaseReport, "release.report"},
	{EventsWrite, "events.write"},
}

func PermissionNames(value Permission) []string {
	names := make([]string, 0, len(permissionNames))
	for _, permission := range permissionNames {
		if value&permission.bit != 0 {
			names = append(names, permission.name)
		}
	}
	return names
}
func ParsePermissions(names []string) (Permission, error) {
	if len(names) == 0 || len(names) > len(permissionNames) {
		return 0, platform.ErrValidation
	}
	var value Permission
	for _, name := range names {
		var bit Permission
		for _, permission := range permissionNames {
			if permission.name == name {
				bit = permission.bit
				break
			}
		}
		if bit == 0 || value&bit != 0 {
			return 0, platform.ErrValidation
		}
		value |= bit
	}
	return value, nil
}
func credentialInfo(g Grant) CredentialInfo {
	return CredentialInfo{ID: g.ID, PrincipalID: g.PrincipalID, Scope: g.Scope, AgentID: g.AgentID, InstanceID: g.InstanceID, BootID: g.BootID, TakeoverFromBootID: g.TakeoverFromBootID, Permissions: PermissionNames(g.Permissions), ExpiresAt: g.ExpiresAt}
}

type adminResult struct {
	object string
	body   []byte
}

func (s *Sessions) adminMutation(ctx context.Context, scope client.Scope, actor AdminMutation, action string, input any, apply func(pgx.Tx) (adminResult, error)) ([]byte, bool, error) {
	if !identifier(actor.ActorID) || actor.ActorRole != "owner" {
		return nil, false, platform.ErrAccessDenied
	}
	if !scopeValid(scope) || !identifier(actor.OperationID) {
		return nil, false, platform.ErrValidation
	}
	encoded, err := json.Marshal(input, json.Deterministic(true))
	if err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256(encoded)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_admin_operations(workspace_id,environment_id,service_key,actor_id,operation_id,action,request_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, actor.ActorID, actor.OperationID, action, digest[:]); err != nil {
		return nil, false, err
	}
	var savedAction string
	var savedHash, result []byte
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT action,request_hash,result,expires_at<=clock_timestamp() FROM agent_admin_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND actor_id=$4 AND operation_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, actor.ActorID, actor.OperationID).Scan(&savedAction, &savedHash, &result, &expired); err != nil {
		return nil, false, err
	}
	if savedAction != action || !bytes.Equal(savedHash, digest[:]) || expired {
		return nil, false, platform.ErrConflict
	}
	if len(result) > 0 {
		return result, true, tx.Commit(ctx)
	}
	applied, err := apply(tx)
	if err != nil {
		return nil, false, err
	}
	auditWrite, err := tx.Exec(ctx, `INSERT INTO agent_admin_audit(workspace_id,environment_id,service_key,actor_id,actor_role,operation_id,action,object_id,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, actor.ActorID, actor.ActorRole, actor.OperationID, action, applied.object, applied.body)
	if err != nil {
		return nil, false, err
	}
	if auditWrite.RowsAffected() != 1 {
		return nil, false, platform.ErrConflict
	}
	operationWrite, err := tx.Exec(ctx, `UPDATE agent_admin_operations SET result=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND actor_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, actor.ActorID, actor.OperationID, applied.body)
	if err != nil {
		return nil, false, err
	}
	if operationWrite.RowsAffected() != 1 {
		return nil, false, platform.ErrConflict
	}
	return applied.body, false, tx.Commit(ctx)
}

func (s *Sessions) IssueManagedCredential(ctx context.Context, actor AdminMutation, g Grant) (IssuedCredential, error) {
	var secret string
	body, replayed, err := s.adminMutation(ctx, g.Scope, actor, "credential.issue", g, func(tx pgx.Tx) (adminResult, error) {
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT $1>clock_timestamp() AND $1<=clock_timestamp()+interval '24 hours'`, g.ExpiresAt).Scan(&valid); err != nil {
			return adminResult{}, err
		}
		if !valid {
			return adminResult{}, platform.ErrValidation
		}
		id, value, err := issueCredential(ctx, tx, g)
		if err != nil {
			return adminResult{}, err
		}
		g.ID = id
		secret = value
		body, err := json.Marshal(credentialInfo(g))
		return adminResult{object: id, body: body}, err
	})
	if err != nil {
		return IssuedCredential{}, err
	}
	var info CredentialInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return IssuedCredential{}, err
	}
	if replayed {
		secret = ""
	}
	return IssuedCredential{Credential: info, Secret: secret, Replayed: replayed}, nil
}
func (s *Sessions) SetManagedScopeLimit(ctx context.Context, scope client.Scope, actor AdminMutation, expected, maximum int) (ScopeLimit, error) {
	if expected < 0 || maximum < 1 || maximum > 100000 {
		return ScopeLimit{}, platform.ErrValidation
	}
	body, _, err := s.adminMutation(ctx, scope, actor, "scope.limit", struct{ Expected, Maximum int }{expected, maximum}, func(tx pgx.Tx) (adminResult, error) {
		query := `UPDATE agent_scope_limits SET max_connections=$4 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND max_connections=$5`
		args := []any{scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, maximum, expected}
		if expected == 0 {
			query = `INSERT INTO agent_scope_limits VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`
			args = args[:4]
		}
		result, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return adminResult{}, err
		}
		if result.RowsAffected() != 1 {
			return adminResult{}, platform.ErrConflict
		}
		body, err := json.Marshal(ScopeLimit{MaxConnections: maximum})
		return adminResult{object: scope.ServiceKey, body: body}, err
	})
	if err != nil {
		return ScopeLimit{}, err
	}
	var result ScopeLimit
	err = json.Unmarshal(body, &result)
	return result, err
}
func (s *Sessions) RevokeManagedCredential(ctx context.Context, scope client.Scope, actor AdminMutation, id string) error {
	if !identifier(id) {
		return platform.ErrValidation
	}
	_, _, err := s.adminMutation(ctx, scope, actor, "credential.revoke", id, func(tx pgx.Tx) (adminResult, error) {
		result, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked=true WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND id=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, id)
		if err != nil {
			return adminResult{}, err
		}
		if result.RowsAffected() != 1 {
			return adminResult{}, platform.ErrNotFound
		}
		body, err := json.Marshal(struct {
			ID      string `json:"id"`
			Revoked bool   `json:"revoked"`
		}{id, true})
		return adminResult{object: id, body: body}, err
	})
	return err
}
func (s *Sessions) ScopeLimit(ctx context.Context, scope client.Scope) (ScopeLimit, error) {
	if !scopeValid(scope) {
		return ScopeLimit{}, platform.ErrValidation
	}
	var result ScopeLimit
	err := s.pool.QueryRow(ctx, `SELECT max_connections FROM agent_scope_limits WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&result.MaxConnections)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScopeLimit{}, platform.ErrNotFound
	}
	return result, err
}
func (s *Sessions) ListCredentials(ctx context.Context, scope client.Scope, after string) ([]CredentialInfo, error) {
	if !scopeValid(scope) || (after != "" && !identifier(after)) {
		return nil, platform.ErrValidation
	}
	rows, err := s.pool.Query(ctx, `SELECT id,principal_id,agent_id,instance_id,boot_id,takeover_from_boot_id,permissions,expires_at,revoked FROM agent_credentials WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND id>$4 ORDER BY id LIMIT 100`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CredentialInfo, 0)
	for rows.Next() {
		var info CredentialInfo
		var permissions int64
		info.Scope = scope
		if err := rows.Scan(&info.ID, &info.PrincipalID, &info.AgentID, &info.InstanceID, &info.BootID, &info.TakeoverFromBootID, &permissions, &info.ExpiresAt, &info.Revoked); err != nil {
			return nil, err
		}
		info.Permissions = PermissionNames(Permission(permissions))
		result = append(result, info)
	}
	return result, rows.Err()
}
func (s *Sessions) ListAdminAudit(ctx context.Context, scope client.Scope, before uint64) ([]AdminAudit, error) {
	if !scopeValid(scope) || before > uint64(1<<63-1) {
		return nil, platform.ErrValidation
	}
	if before == 0 {
		before = uint64(1<<63 - 1)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,occurred_at,actor_id,actor_role,operation_id,action,object_id FROM agent_admin_audit WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND id<$4 ORDER BY id DESC LIMIT 100`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, int64(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AdminAudit, 0)
	for rows.Next() {
		var entry AdminAudit
		var id int64
		if err := rows.Scan(&id, &entry.OccurredAt, &entry.ActorID, &entry.ActorRole, &entry.OperationID, &entry.Action, &entry.ObjectID); err != nil {
			return nil, err
		}
		entry.ID = strconv.FormatInt(id, 10)
		result = append(result, entry)
	}
	return result, rows.Err()
}
