package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"slices"
	"strconv"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// StartRelease allocates an independent, durable release generation. It is a
// trusted control-plane operation, not an Agent capability. Callers must bind
// capacity intent and authorization to approval before exposing this method.
// Lock order: operation -> release head -> schema share; resource reads never
// acquire resource heads. Prepare delivery starts only after commit elsewhere.
// A replay is the original receipt, not current state; dispatch must recheck the
// current release head, epoch, state and deadline instead of trusting this result.
func (p *Publisher) StartRelease(ctx context.Context, request StartReleaseRequest, verifier *client.Verifier) (ReleaseRecord, error) {
	if !scopeValid(request.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) || request.BundleHash == [32]byte{} || request.BundleArtifactRevision == 0 || !request.Capacity.valid() || verifier == nil {
		return ReleaseRecord{}, denied()
	}
	if request.ExpectedGeneration == math.MaxUint64 {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release generation exhausted")
	}
	// Time zone and sub-millisecond differences do not change a deadline identity.
	deadline := int64(0)
	if !request.AuthorizationExpiresAt.IsZero() {
		deadline = request.AuthorizationExpiresAt.UnixMilli()
	}
	intent := struct {
		Scope       client.Scope    `json:"scope"`
		BundleHash  string          `json:"bundleHash"`
		Artifact    uint64          `json:"artifact,string"`
		Expected    uint64          `json:"expected,string"`
		Capacity    ReleaseCapacity `json:"capacity"`
		HasDeadline bool            `json:"hasDeadline"`
		Deadline    int64           `json:"deadline,string"`
	}{Scope: request.Scope, BundleHash: hex.EncodeToString(request.BundleHash[:]), Artifact: request.BundleArtifactRevision, Expected: request.ExpectedGeneration, Capacity: request.Capacity, Deadline: deadline, HasDeadline: !request.AuthorizationExpiresAt.IsZero()}
	body, err := json.Marshal(intent, json.Deterministic(true))
	if err != nil {
		return ReleaseRecord{}, err
	}
	operationHash := sha256.Sum256(append([]byte("grayling.agent.v1.release-start\x00"), body...))
	tx, err := p.sessions.pool.Begin(ctx)
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
		if r.Definition.Scope != scope || r.Generation != request.ExpectedGeneration+1 || r.Definition.BundleHash != intent.BundleHash || r.Definition.BundleArtifactRevision != request.BundleArtifactRevision || !releaseCapacityEqual(r.Definition.Capacity, request.Capacity) || r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING || r.Revision != 1 {
			return r, client.ErrResource
		}
		return r, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_release_heads VALUES($1,$2,$3,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey); err != nil {
		return ReleaseRecord{}, err
	}
	var currentText string
	if err := tx.QueryRow(ctx, `SELECT generation::text FROM agent_release_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&currentText); err != nil {
		return ReleaseRecord{}, err
	}
	current, err := strconv.ParseUint(currentText, 10, 64)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if current != request.ExpectedGeneration {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release generation changed")
	}
	var epoch int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return ReleaseRecord{}, err
	}
	local := time.Now()
	if epoch <= 0 || now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane epoch or clock changed")
	}
	if !request.AuthorizationExpiresAt.IsZero() && now.UnixMilli() >= deadline {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "release approval expired")
	}
	// Only a terminal or ACTIVE predecessor permits a new rollout. An expired
	// prepare can be failed atomically with allocating its successor; ACTIVE is
	// never silently aborted or regressed by this path.
	if current > 0 {
		previous, err := readRelease(ctx, tx, scope, current, true)
		if err != nil {
			return ReleaseRecord{}, err
		}
		switch previous.State {
		case agentv1.ReleaseState_RELEASE_STATE_ACTIVE, agentv1.ReleaseState_RELEASE_STATE_FAILED, agentv1.ReleaseState_RELEASE_STATE_ABORTED, agentv1.ReleaseState_RELEASE_STATE_ROLLED_BACK:
		case agentv1.ReleaseState_RELEASE_STATE_PREPARING, agentv1.ReleaseState_RELEASE_STATE_PREPARED:
			if previous.ControlPlaneEpoch == uint64(epoch) && previous.PrepareDeadline.After(now) {
				return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "previous release is still preparing")
			}
			if previous.Revision == math.MaxUint64 {
				return ReleaseRecord{}, client.ErrResource
			}
			from := previous.State
			previous.State = agentv1.ReleaseState_RELEASE_STATE_FAILED
			previous.Revision++
			updated, err := tx.Exec(ctx, `UPDATE agent_releases SET state=$5,revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, currentText, int32(previous.State), strconv.FormatUint(previous.Revision, 10))
			if err != nil {
				return ReleaseRecord{}, err
			}
			if updated.RowsAffected() != 1 {
				return ReleaseRecord{}, client.ErrResource
			}
			if err := releaseEvent(ctx, tx, previous, from, request.PrincipalID, request.OperationID, "prepare expired or epoch superseded"); err != nil {
				return ReleaseRecord{}, err
			}
		default:
			return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "previous release is not terminal")
		}
	}
	if _, err := checkBundleRevocation(ctx, tx, scope, uint64(epoch), request.BundleHash, verifier); err != nil {
		return ReleaseRecord{}, err
	}
	artifact, err := loadResource(ctx, tx, scope, bundleStorageKey(request.BundleHash), request.BundleArtifactRevision, true)
	if err != nil {
		return ReleaseRecord{}, err
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(artifact.Body, &signed) != nil {
		return ReleaseRecord{}, client.ErrResource
	}
	bundle, bc, err := client.VerifyBundle(verifier, &signed, scope, request.BundleHash, client.Watermark{}, now)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if bc.Revision != request.BundleArtifactRevision || bc.ControlPlaneEpoch != uint64(epoch) || bc.ExpiresUnixMs != artifact.ExpiresAt.UnixMilli() {
		return ReleaseRecord{}, client.ErrResource
	}
	document := bundle.Document()
	policy, err := loadResource(ctx, tx, scope, document.Policy.Key, document.Policy.Revision, true)
	if err != nil {
		return ReleaseRecord{}, err
	}
	var signedPolicy agentv1.SignedResource
	if proto.Unmarshal(policy.Body, &signedPolicy) != nil {
		return ReleaseRecord{}, client.ErrResource
	}
	_, pc, err := bundle.VerifyPolicy(verifier, &signedPolicy, client.Watermark{}, now)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if pc.ExpiresUnixMs != policy.ExpiresAt.UnixMilli() {
		return ReleaseRecord{}, client.ErrResource
	}
	if request.AuthorizeNewRelease != nil {
		if err := request.AuthorizeNewRelease(ctx, tx); err != nil {
			return ReleaseRecord{}, err
		}
	}
	resourcesExpire := time.UnixMilli(min(bc.ExpiresUnixMs, pc.ExpiresUnixMs)).UTC()
	created := now.Truncate(time.Millisecond).UTC()
	prepareDeadline := created.Add(releasePrepareTimeout)
	if resourcesExpire.Before(prepareDeadline) {
		prepareDeadline = resourcesExpire
	}
	r := ReleaseRecord{Definition: ReleaseDefinition{Scope: scope, BundleHash: intent.BundleHash, BundleArtifactRevision: request.BundleArtifactRevision, Document: document, Capacity: request.Capacity, PrepareTimeoutMs: releasePrepareTimeout.Milliseconds(), PreparedTTLms: releasePreparedTTL.Milliseconds()}, ReleaseID: releaseID(current + 1), Generation: current + 1, ControlPlaneEpoch: uint64(epoch), State: agentv1.ReleaseState_RELEASE_STATE_PREPARING, Revision: 1, CreatedAt: created, PrepareDeadline: prepareDeadline, ResourcesExpiresAt: resourcesExpire}
	if !releaseRecordValid(r) {
		return ReleaseRecord{}, client.ErrResource
	}
	definition, err := json.Marshal(r.Definition, json.Deterministic(true))
	if err != nil || len(definition) > client.MaxResourceBytes {
		return ReleaseRecord{}, client.ErrResource
	}
	definitionHash := sha256.Sum256(definition)
	generation := strconv.FormatUint(r.Generation, 10)
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_releases VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$11,$12)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generation, r.ReleaseID, epoch, definition, definitionHash[:], int32(r.State), r.CreatedAt, r.PrepareDeadline, r.ResourcesExpiresAt)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if inserted.RowsAffected() != 1 {
		return ReleaseRecord{}, client.ErrResource
	}
	pinKeys := []string{bundleStorageKey(request.BundleHash), document.Policy.Key}
	slices.Sort(pinKeys)
	for _, key := range pinKeys {
		revision := document.Policy.Revision
		if key == bundleStorageKey(request.BundleHash) {
			revision = request.BundleArtifactRevision
		}
		pinned, err := tx.Exec(ctx, `INSERT INTO agent_release_resource_refs VALUES($1,$2,$3,$4,$5,$6)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generation, key, strconv.FormatUint(revision, 10))
		if err != nil {
			return ReleaseRecord{}, err
		}
		if pinned.RowsAffected() != 1 {
			return ReleaseRecord{}, client.ErrResource
		}
	}
	updated, err := tx.Exec(ctx, `UPDATE agent_release_heads SET generation=$4 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generation)
	if err != nil {
		return ReleaseRecord{}, err
	}
	if updated.RowsAffected() != 1 {
		return ReleaseRecord{}, client.ErrResource
	}
	if err := releaseEvent(ctx, tx, r, agentv1.ReleaseState_RELEASE_STATE_UNSPECIFIED, request.PrincipalID, request.OperationID, "exact Bundle and capacity intent validated; awaiting Prepare evidence"); err != nil {
		return ReleaseRecord{}, err
	}
	if err := finishReleaseOperation(ctx, tx, scope, request.PrincipalID, request.OperationID, r); err != nil {
		return ReleaseRecord{}, err
	}
	var finalTime time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&finalTime); err != nil {
		return ReleaseRecord{}, err
	}
	if finalTime.Before(created) || !finalTime.Before(r.PrepareDeadline) || (!request.AuthorizationExpiresAt.IsZero() && finalTime.UnixMilli() >= deadline) {
		return ReleaseRecord{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "release authorization expired before commit")
	}
	return r, tx.Commit(ctx)
}
