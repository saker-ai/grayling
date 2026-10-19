package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

type EvaluatePreparationRequest struct {
	Scope                        client.Scope
	PrincipalID, OperationID     string
	Generation, ExpectedRevision uint64
	AuthorizationExpiresAt       time.Time
	Authorize                    func(context.Context, pgx.Tx) error
}
type PreparedCapacityCell struct {
	BuildID  string `json:"buildId"`
	Zone     string `json:"zone"`
	Prepared uint64 `json:"prepared,string"`
	Required uint32 `json:"required"`
}

// PreparationEvaluation is a time-bounded historical observation. Activation
// must recompute current governance and capacity, never authorize from this receipt.
type PreparationEvaluation struct {
	Release          ReleaseRecord          `json:"release"`
	Eligible         bool                   `json:"eligible"`
	Targets          uint64                 `json:"targets,string"`
	Prepared         uint64                 `json:"prepared,string"`
	Failed           uint64                 `json:"failed,string"`
	Pending          uint64                 `json:"pending,string"`
	EvidenceRevision uint64                 `json:"evidenceRevision,string"`
	TargetDigest     string                 `json:"targetDigest"`
	Cells            []PreparedCapacityCell `json:"cells"`
	EvaluatedAt      time.Time              `json:"evaluatedAt"`
	ValidUntil       time.Time              `json:"validUntil"`
}

// EvaluatePreparation serializes against credential revocation, boot takeover,
// registration/lease/Naming changes and both report types. Lock order: operation
// -> sorted credentials -> admission -> registration scope -> release -> schema.
// A newly introduced target credential causes retry, never lock inversion.
func (s *Sessions) EvaluatePreparation(ctx context.Context, q EvaluatePreparationRequest, v *client.Verifier) (PreparationEvaluation, error) {
	if !scopeValid(q.Scope) || !identifier(q.PrincipalID) || !identifier(q.OperationID) || q.Generation == 0 || q.ExpectedRevision == 0 || q.ExpectedRevision == math.MaxUint64 || v == nil {
		return PreparationEvaluation{}, denied()
	}
	intent, err := json.Marshal(struct {
		Scope       client.Scope `json:"scope"`
		Generation  uint64       `json:"generation,string"`
		Revision    uint64       `json:"revision,string"`
		HasDeadline bool         `json:"hasDeadline"`
		Deadline    int64        `json:"deadline,string"`
	}{q.Scope, q.Generation, q.ExpectedRevision, !q.AuthorizationExpiresAt.IsZero(), q.AuthorizationExpiresAt.UnixMilli()}, json.Deterministic(true))
	if err != nil {
		return PreparationEvaluation{}, err
	}
	hash := sha256.Sum256(append([]byte("grayling.agent.v1.preparation-gate\x00"), intent...))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PreparationEvaluation{}, err
	}
	defer tx.Rollback(ctx)
	old, err := releaseOperation(ctx, tx, q.Scope, q.PrincipalID, q.OperationID, hash)
	if err != nil {
		return PreparationEvaluation{}, err
	}
	if len(old) > 0 {
		var result PreparationEvaluation
		if json.Unmarshal(old, &result, json.RejectUnknownMembers(true)) != nil || !result.Eligible || result.Release.Definition.Scope != q.Scope || result.Release.Generation != q.Generation || !releaseRecordValid(result.Release) {
			return result, client.ErrResource
		}
		return result, tx.Commit(ctx)
	}
	credentials, err := lockPreparationCredentials(ctx, tx, q.Scope, q.Generation)
	if err != nil {
		return PreparationEvaluation{}, err
	}
	var admission int
	if err := tx.QueryRow(ctx, `SELECT max_connections FROM agent_scope_limits WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR SHARE`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey).Scan(&admission); err != nil {
		return PreparationEvaluation{}, err
	}
	state, err := readRegistrationScope(ctx, tx, q.Scope)
	if err != nil {
		return PreparationEvaluation{}, err
	}
	r, now, err := lockPreparingRelease(ctx, tx, q.Scope, q.Generation)
	if err != nil {
		return PreparationEvaluation{}, err
	}
	if r.Revision != q.ExpectedRevision {
		return PreparationEvaluation{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release state changed")
	}
	if !q.AuthorizationExpiresAt.IsZero() && !q.AuthorizationExpiresAt.After(now) {
		return PreparationEvaluation{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "capacity authorization expired")
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return PreparationEvaluation{}, err
	}
	result, err := evaluatePreparationLocked(ctx, tx, r, state, credentials, now)
	if err != nil {
		return result, err
	}
	if !result.Eligible {
		return result, nil
	} // Rollback provisional operation: retry must observe new evidence.
	if q.Authorize != nil {
		if err := q.Authorize(ctx, tx); err != nil {
			return result, err
		}
	}
	if r.State == agentv1.ReleaseState_RELEASE_STATE_PREPARING {
		from := r.State
		r.State = agentv1.ReleaseState_RELEASE_STATE_PREPARED
		r.Revision++
		changed, err := tx.Exec(ctx, `UPDATE agent_releases SET state=$5,revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey, strconv.FormatUint(q.Generation, 10), int32(r.State), strconv.FormatUint(r.Revision, 10))
		if err != nil {
			return result, err
		}
		if changed.RowsAffected() != 1 {
			return result, client.ErrResource
		}
		if err := releaseEvent(ctx, tx, r, from, q.PrincipalID, q.OperationID, "preparation capacity satisfied"); err != nil {
			return result, err
		}
		result.Release = r
		body, err := json.Marshal(result, json.Deterministic(true))
		if err != nil {
			return result, err
		}
		digest := sha256.Sum256(body)
		inserted, err := tx.Exec(ctx, `INSERT INTO agent_preparation_decisions VALUES($1,$2,$3,$4,$5,$6,$7)`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey, strconv.FormatUint(r.Generation, 10), strconv.FormatUint(r.Revision, 10), body, digest[:])
		if err != nil {
			return result, err
		}
		if inserted.RowsAffected() != 1 {
			return result, client.ErrResource
		}
	}
	body, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	if len(body) > client.MaxResourceBytes {
		return result, client.ErrResource
	}
	saved, err := tx.Exec(ctx, `UPDATE agent_release_operations SET result=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey, q.PrincipalID, q.OperationID, body)
	if err != nil {
		return result, err
	}
	if saved.RowsAffected() != 1 {
		return result, client.ErrResource
	}
	expiry := result.ValidUntil
	if !q.AuthorizationExpiresAt.IsZero() && q.AuthorizationExpiresAt.Before(expiry) {
		expiry = q.AuthorizationExpiresAt
	}
	if err := preparationStillFresh(ctx, tx, now, expiry); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

type preparationCredential struct {
	allowed     bool
	permissions Permission
	expires     time.Time
}

func lockPreparationCredentials(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (map[string]preparationCredential, error) {
	// Snapshot before scope locks. Re-read targets after locking to detect additions.
	rows, err := tx.Query(ctx, `SELECT DISTINCT i.credential_id FROM agent_prepare_heads h JOIN agent_prepare_intents i USING(workspace_id,environment_id,service_key,generation,command_id) WHERE h.workspace_id=$1 AND h.environment_id=$2 AND h.service_key=$3 AND h.generation=$4 LIMIT 100001`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(generation, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(ids) > 100000 {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "preparation target bound exceeded")
	}
	rows, err = tx.Query(ctx, `SELECT id,NOT revoked AND permissions & $2=$2,expires_at,permissions FROM agent_credentials WHERE id=ANY($1) ORDER BY id FOR SHARE`, ids, int64(ConfigRead|ReleaseReport))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]preparationCredential, len(ids))
	for rows.Next() {
		var id string
		var c preparationCredential
		if err := rows.Scan(&id, &c.allowed, &c.expires, &c.permissions); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Missing current target credentials imply a prefetch race, not absent capacity.
func preparationTargetRace() error {
	return &agentv1.ProtocolError{Code: agentv1.ErrorCode_ERROR_CODE_CONFLICT, Reason: "preparation targets changed; retry", Retryable: true, RetryAfterMs: 100}
}
