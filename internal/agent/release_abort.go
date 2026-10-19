package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

type AbortReleaseRequest struct {
	Scope                        client.Scope
	PrincipalID, OperationID     string
	Generation, ExpectedRevision uint64
	Reason                       string
	AuthorizationExpiresAt       time.Time
	AuthorizeAbort               func(context.Context, pgx.Tx) error
}

// AbortRelease stops only a not-yet-activating release. It cannot roll back an
// active/applied source or silently regress generation. Rollback requires a new
// approved generation, not an Abort of an already activated release.
func (s *Sessions) AbortRelease(ctx context.Context, request AbortReleaseRequest) (ReleaseRecord, error) {
	if !scopeValid(request.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) || request.Generation == 0 || request.ExpectedRevision == 0 || request.ExpectedRevision == math.MaxUint64 || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 1024 || !utf8.ValidString(request.Reason) || strings.ContainsRune(request.Reason, 0) {
		return ReleaseRecord{}, denied()
	}
	deadline := int64(0)
	if !request.AuthorizationExpiresAt.IsZero() {
		deadline = request.AuthorizationExpiresAt.UnixMilli()
	}
	intent := struct {
		Scope       client.Scope `json:"scope"`
		Generation  uint64       `json:"generation,string"`
		Revision    uint64       `json:"revision,string"`
		Reason      string       `json:"reason"`
		HasDeadline bool         `json:"hasDeadline"`
		Deadline    int64        `json:"deadline,string"`
	}{Scope: request.Scope, Generation: request.Generation, Revision: request.ExpectedRevision, Reason: request.Reason, Deadline: deadline, HasDeadline: !request.AuthorizationExpiresAt.IsZero()}
	body, err := json.Marshal(intent, json.Deterministic(true))
	if err != nil {
		return ReleaseRecord{}, err
	}
	operationHash := sha256.Sum256(append([]byte("grayling.agent.v1.release-abort\x00"), body...))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReleaseRecord{}, err
	}
	defer tx.Rollback(ctx)
	scope := request.Scope
	replay, err := releaseOperation(ctx, tx, scope, request.PrincipalID, request.OperationID, operationHash)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if len(replay) > 0 {
		r, err := decodeRelease(replay)
		if err != nil {
			return r, err
		}
		if r.Definition.Scope != scope || r.Generation != request.Generation || r.Revision != request.ExpectedRevision+1 || r.State != agentv1.ReleaseState_RELEASE_STATE_ABORTED {
			return r, client.ErrResource
		}
		return r, tx.Commit(ctx)
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT generation::text FROM agent_release_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "release unavailable")
		}
		return ReleaseRecord{}, err
	}
	if current != strconv.FormatUint(request.Generation, 10) {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release generation changed")
	}
	var now time.Time
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return ReleaseRecord{}, err
	}
	local := time.Now()
	if epoch <= 0 || now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane epoch or clock changed")
	}
	if !request.AuthorizationExpiresAt.IsZero() && now.UnixMilli() >= deadline {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "abort authorization expired")
	}
	r, err := readRelease(ctx, tx, scope, request.Generation, true)
	if err != nil {
		return r, err
	}
	if r.ControlPlaneEpoch != uint64(epoch) {
		return r, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "release epoch changed")
	}
	if r.Revision != request.ExpectedRevision {
		return r, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release state changed")
	}
	if r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING && r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARED {
		return r, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "cannot abort an activating or terminal release")
	}
	if request.AuthorizeAbort != nil {
		if err := request.AuthorizeAbort(ctx, tx); err != nil {
			return r, err
		}
	}
	from := r.State
	r.State = agentv1.ReleaseState_RELEASE_STATE_ABORTED
	r.Revision++
	updated, err := tx.Exec(ctx, `UPDATE agent_releases SET state=$5,revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, current, int32(r.State), strconv.FormatUint(r.Revision, 10))
	if err != nil {
		return r, err
	}
	if updated.RowsAffected() != 1 {
		return r, client.ErrResource
	}
	if err := releaseEvent(ctx, tx, r, from, request.PrincipalID, request.OperationID, request.Reason); err != nil {
		return r, err
	}
	if err := finishReleaseOperation(ctx, tx, scope, request.PrincipalID, request.OperationID, r); err != nil {
		return r, err
	}
	var finalTime time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&finalTime); err != nil {
		return r, err
	}
	if finalTime.Before(now) || (!request.AuthorizationExpiresAt.IsZero() && finalTime.UnixMilli() >= deadline) {
		return r, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "abort authorization expired before commit")
	}
	return r, tx.Commit(ctx)
}
