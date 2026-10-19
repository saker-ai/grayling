package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// QueuePrepare persists a directed command after validating current governance.
// Lock order: registration scope -> release head -> schema -> instance -> target.
// A repeated operation returns immutable old intent; it is not dispatch authority.
func (s *Sessions) QueuePrepare(ctx context.Context, request QueuePrepareRequest, v *client.Verifier) (PrepareIntent, error) {
	return s.queuePrepare(ctx, request, v, false)
}

// QueueReprepare creates a fresh directed command in an established release.
// A new explicit operation and target CAS are required; replay never renews it.
func (s *Sessions) QueueReprepare(ctx context.Context, request QueuePrepareRequest, v *client.Verifier) (PrepareIntent, error) {
	return s.queuePrepare(ctx, request, v, true)
}

func (s *Sessions) queuePrepare(ctx context.Context, request QueuePrepareRequest, v *client.Verifier, renewal bool) (PrepareIntent, error) {
	deadline := int64(0)
	if !request.AuthorizationExpiresAt.IsZero() {
		deadline = request.AuthorizationExpiresAt.UnixMilli()
	}

	if !scopeValid(request.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) || !identifier(request.InstanceID) || request.Generation == 0 || request.ExpectedTargetRevision == math.MaxUint64 || v == nil {
		return PrepareIntent{}, denied()
	}
	body, err := json.Marshal(struct {
		Scope       client.Scope `json:"scope"`
		Principal   string       `json:"principal"`
		Operation   string       `json:"operation"`
		Instance    string       `json:"instance"`
		Generation  uint64       `json:"generation,string"`
		Expected    uint64       `json:"expected,string"`
		Renewal     bool         `json:"renewal,omitzero"`
		Deadline    int64        `json:"deadline,string,omitzero"`
		HasDeadline bool         `json:"hasDeadline,omitzero"`
	}{request.Scope, request.PrincipalID, request.OperationID, request.InstanceID, request.Generation, request.ExpectedTargetRevision, renewal, deadline, !request.AuthorizationExpiresAt.IsZero()}, json.Deterministic(true))
	if err != nil {
		return PrepareIntent{}, err
	}
	hash := prepareRequestHash(body)
	if renewal {
		hash = sha256.Sum256(append([]byte("grayling.agent.v1.reprepare-intent\x00"), body...))
	}
	command := "prepare-" + hex.EncodeToString(hash[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PrepareIntent{}, err
	}
	defer tx.Rollback(ctx)
	scope := request.Scope
	existing := func() (PrepareIntent, bool, error) {
		var cmd string
		err := tx.QueryRow(ctx, `SELECT command_id FROM agent_prepare_intents WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID).Scan(&cmd)
		if errors.Is(err, pgx.ErrNoRows) {
			return PrepareIntent{}, false, nil
		}
		if err != nil {
			return PrepareIntent{}, false, err
		}
		intent, saved, err := readPrepareIntent(ctx, tx, scope, cmd)
		if err != nil {
			return intent, false, err
		}
		if !bytes.Equal(hash[:], saved) || cmd != command {
			return intent, false, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "prepare operation changed")
		}
		return intent, true, nil
	}
	if old, found, err := existing(); err != nil {
		return old, err
	} else if found {
		return old, tx.Commit(ctx)
	}
	state, err := readRegistrationScope(ctx, tx, scope)
	if err != nil {
		return PrepareIntent{}, err
	}
	if old, found, err := existing(); err != nil {
		return old, err
	} else if found {
		return old, tx.Commit(ctx)
	}
	var r ReleaseRecord
	var now time.Time
	if renewal {
		r, now, err = lockCurrentRelease(ctx, tx, scope, request.Generation)
	} else {
		r, now, err = lockPreparingRelease(ctx, tx, scope, request.Generation)
	}
	if err != nil {
		return PrepareIntent{}, err
	}
	if renewal && r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING && r.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING && r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVE {
		return PrepareIntent{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "release is not established")
	}
	if !request.AuthorizationExpiresAt.IsZero() && !request.AuthorizationExpiresAt.After(now) {
		return PrepareIntent{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "preparation approval expired")
	}
	instance, err := readInstance(ctx, tx, scope, request.InstanceID)
	if err != nil {
		return PrepareIntent{}, err
	}
	if !prepareInstanceValid(instance, state, r) || !instance.expiresAt.After(now) {
		return PrepareIntent{}, denied()
	}
	var credentialExpiry time.Time
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT expires_at,NOT revoked AND permissions & $2 = $2 FROM agent_credentials WHERE id=$1`, instance.credentialID, int64(ConfigRead|ReleaseReport)).Scan(&credentialExpiry, &allowed)
	if err != nil {
		return PrepareIntent{}, err
	}
	if !allowed || !credentialExpiry.After(now) {
		return PrepareIntent{}, denied()
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return PrepareIntent{}, err
	}
	current := uint64(0)
	var text string
	err = tx.QueryRow(ctx, `SELECT revision::text FROM agent_prepare_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(request.Generation, 10), request.InstanceID).Scan(&text)
	if err == nil {
		current, err = strconv.ParseUint(text, 10, 64)
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return PrepareIntent{}, err
	}
	if current != request.ExpectedTargetRevision {
		return PrepareIntent{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "prepare target changed")
	}
	if request.Authorize != nil {
		if err := request.Authorize(ctx, tx); err != nil {
			return PrepareIntent{}, err
		}
	}
	expires := r.PrepareDeadline
	if renewal {
		expires = now.Add(releasePrepareTimeout)
	}
	for _, bound := range []time.Time{credentialExpiry, r.ResourcesExpiresAt, request.AuthorizationExpiresAt} {
		if !bound.IsZero() && bound.Before(expires) {
			expires = bound
		}
	}
	expires = expires.Truncate(time.Millisecond).UTC()
	intent := PrepareIntent{Scope: scope, Generation: request.Generation, TargetRevision: current + 1, CommandID: command, Instance: descriptorIdentity(instance.instance), CredentialID: instance.credentialID, TargetPrincipalID: instance.principalID, AgentID: instance.agentID, LeaseID: instance.instance.Lease.Id, LeaseEpoch: instance.instance.Lease.Epoch, CreatedAt: now.Truncate(time.Millisecond).UTC(), ExpiresAt: expires}
	descriptorBytes, err := (proto.MarshalOptions{Deterministic: true}).Marshal(intent.Instance)
	if err != nil {
		return intent, err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_prepare_intents VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(intent.Generation, 10), command, request.PrincipalID, request.OperationID, hash[:], strconv.FormatUint(intent.TargetRevision, 10), request.InstanceID, intent.Instance.BootId, intent.CredentialID, intent.TargetPrincipalID, intent.AgentID, intent.LeaseID, strconv.FormatUint(intent.LeaseEpoch, 10), descriptorBytes, intent.CreatedAt, intent.ExpiresAt)
	if err != nil {
		return intent, err
	}
	if inserted.RowsAffected() != 1 {
		return intent, client.ErrResource
	}
	updated, err := tx.Exec(ctx, `INSERT INTO agent_prepare_heads VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(workspace_id,environment_id,service_key,generation,instance_id) DO UPDATE SET revision=EXCLUDED.revision,command_id=EXCLUDED.command_id`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(intent.Generation, 10), request.InstanceID, strconv.FormatUint(intent.TargetRevision, 10), command)
	if err != nil {
		return intent, err
	}
	if updated.RowsAffected() != 1 {
		return intent, client.ErrResource
	}
	if err := preparationStillFresh(ctx, tx, now, time.UnixMilli(min(expires.UnixMilli(), instance.expiresAt.UnixMilli()))); err != nil {
		return intent, err
	}
	return intent, tx.Commit(ctx)
}

// Bind the target to current authenticated registration. Lease expiry and
// Naming readback are rechecked on every report, independent of link heartbeat.
func matchesPrepareTarget(intent PrepareIntent, se Session, record *instanceRecord) bool {
	return record != nil && intent.CredentialID == se.Grant.ID && intent.TargetPrincipalID == se.Grant.PrincipalID && intent.AgentID == se.Grant.AgentID && record.credentialID == intent.CredentialID && record.principalID == intent.TargetPrincipalID && record.agentID == intent.AgentID && record.instance.InstanceId == se.Grant.InstanceID && record.instance.BootId == se.Grant.BootID && intent.Instance.InstanceId == se.Grant.InstanceID && intent.Instance.BootId == se.Grant.BootID && record.instance.Lease.Id == intent.LeaseID && record.instance.Lease.Epoch == intent.LeaseEpoch && proto.Equal(descriptorIdentity(record.instance), intent.Instance)
}
