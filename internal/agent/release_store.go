package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func releaseID(generation uint64) string { return "release-" + strconv.FormatUint(generation, 10) }
func readRelease(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64, lock bool) (ReleaseRecord, error) {
	var r ReleaseRecord
	var definition, hash []byte
	var g, revision string
	var epoch int64
	query := `SELECT release_id,generation::text,control_plane_epoch,definition,definition_sha256,state,revision::text,created_at,prepare_deadline,resources_expires_at FROM agent_releases WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(generation, 10)).Scan(&r.ReleaseID, &g, &epoch, &definition, &hash, &r.State, &revision, &r.CreatedAt, &r.PrepareDeadline, &r.ResourcesExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "release unavailable")
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.PrepareDeadline = r.PrepareDeadline.UTC()
	r.ResourcesExpiresAt = r.ResourcesExpiresAt.UTC()
	actual := sha256.Sum256(definition)
	if len(definition) > client.MaxResourceBytes || !bytes.Equal(actual[:], hash) || json.Unmarshal(definition, &r.Definition, json.RejectUnknownMembers(true)) != nil {
		return r, client.ErrResource
	}
	canonical, err := json.Marshal(r.Definition, json.Deterministic(true))
	if err != nil || !bytes.Equal(canonical, definition) {
		return r, client.ErrResource
	}
	r.Generation, err = strconv.ParseUint(g, 10, 64)
	if err != nil {
		return r, err
	}
	r.Revision, err = strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return r, err
	}
	if epoch <= 0 {
		return r, client.ErrResource
	}
	r.ControlPlaneEpoch = uint64(epoch)
	if r.Generation != generation || r.Definition.Scope != scope || !releaseRecordValid(r) {
		return r, client.ErrResource
	}
	return r, nil
}

// Release reads diagnostic state only. The caller must authorize scope before
// exposing it. No signature freshness or current routing authorization is implied.
func (s *Sessions) Release(ctx context.Context, scope client.Scope, generation uint64) (ReleaseRecord, error) {
	if !scopeValid(scope) || generation == 0 {
		return ReleaseRecord{}, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReleaseRecord{}, err
	}
	defer tx.Rollback(ctx)
	r, err := readRelease(ctx, tx, scope, generation, false)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

func releaseOperation(ctx context.Context, tx pgx.Tx, scope client.Scope, principal, operation string, hash [32]byte) ([]byte, error) {
	args := []any{scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, principal, operation}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_release_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 AND expires_at<=clock_timestamp()`, args...); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_release_operations VALUES($1,$2,$3,$4,$5,$6,''::bytea,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, append(args, hash[:])...); err != nil {
		return nil, err
	}
	var stored, result []byte
	if err := tx.QueryRow(ctx, `SELECT request_hash,result FROM agent_release_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 FOR UPDATE`, args...).Scan(&stored, &result); err != nil {
		return nil, err
	}
	if !bytes.Equal(stored, hash[:]) {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "release operation changed")
	}
	return result, nil
}
func finishReleaseOperation(ctx context.Context, tx pgx.Tx, scope client.Scope, principal, operation string, r ReleaseRecord) error {
	body, err := encodeRelease(r)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE agent_release_operations SET result=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, principal, operation, body)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return client.ErrResource
	}
	return nil
}
func releaseEvent(ctx context.Context, tx pgx.Tx, r ReleaseRecord, from agentv1.ReleaseState, principal, operation, reason string) error {
	scope := r.Definition.Scope
	result, err := tx.Exec(ctx, `INSERT INTO agent_release_events VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,clock_timestamp())`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), strconv.FormatUint(r.Revision, 10), int32(from), int32(r.State), principal, operation, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return client.ErrResource
	}
	return nil
}
