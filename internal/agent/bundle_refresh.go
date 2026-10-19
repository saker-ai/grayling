package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

type RefreshBundleRequest struct {
	Scope                    client.Scope
	PrincipalID, OperationID string
	BundleHash               [32]byte
	ExpectedRevision         uint64
}

// RefreshBundle signs a new immutable artifact for an already authorized Bundle.
// The control-plane caller must supply its trusted keyring and independently
// enforce approval/revocation. This is not an Agent-callable capability.
// Neither policy authorization nor the Bundle's hard expiry can be extended.
func (p *Publisher) RefreshBundle(ctx context.Context, request RefreshBundleRequest, verifier *client.Verifier) (StoredResource, error) {
	if !scopeValid(request.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) || request.BundleHash == [32]byte{} || verifier == nil {
		return StoredResource{}, denied()
	}
	if request.ExpectedRevision == 0 || request.ExpectedRevision == math.MaxUint64 {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "bundle revision changed")
	}
	input := []byte("grayling.agent.v1.bundle-refresh\x00")
	for _, field := range []string{request.Scope.WorkspaceID, request.Scope.EnvironmentID, request.Scope.ServiceKey} {
		input = binary.BigEndian.AppendUint16(input, uint16(len(field)))
		input = append(input, field...)
	}
	input = append(input, request.BundleHash[:]...)
	input = binary.BigEndian.AppendUint64(input, request.ExpectedRevision)
	operationHash := sha256.Sum256(input)
	tx, err := p.sessions.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	scope := request.Scope
	storageKey := bundleStorageKey(request.BundleHash)
	if _, err := tx.Exec(ctx, `DELETE FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 AND expires_at<=clock_timestamp()`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID); err != nil {
		return StoredResource{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_operations VALUES($1,$2,$3,$4,$5,$6,$7,0,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, operationHash[:], storageKey); err != nil {
		return StoredResource{}, err
	}
	var priorHash []byte
	var revisionText string
	if err := tx.QueryRow(ctx, `SELECT request_hash,revision::text FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID).Scan(&priorHash, &revisionText); err != nil {
		return StoredResource{}, err
	}
	if !bytes.Equal(priorHash, operationHash[:]) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "operation payload changed")
	}
	previous, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return StoredResource{}, err
	}
	if previous > 0 {
		result, err := loadResource(ctx, tx, scope, storageKey, previous, false)
		if err != nil {
			return StoredResource{}, err
		}
		var signed agentv1.SignedResource
		if proto.Unmarshal(result.Body, &signed) != nil || sha256.Sum256(signed.Payload) != request.BundleHash {
			return StoredResource{}, client.ErrResource
		}
		claims, err := client.ParseClaims(signed.Claims)
		if err != nil || !storedBundleMember(result, scope, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, client.BundleKey(request.BundleHash), previous, claims.ControlPlaneEpoch, request.BundleHash, signed.Payload) {
			return StoredResource{}, client.ErrResource
		}
		return result, tx.Commit(ctx)
	}
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, storageKey).Scan(&revisionText); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "resource unavailable")
		}
		return StoredResource{}, err
	}
	current, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return StoredResource{}, err
	}
	if current != request.ExpectedRevision {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "bundle revision changed")
	}
	var epoch int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return StoredResource{}, err
	}
	local := time.Now()
	if now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane clock changed")
	}
	if _, err := checkBundleRevocation(ctx, tx, scope, uint64(epoch), request.BundleHash, verifier); err != nil {
		return StoredResource{}, err
	}
	// Original artifact and exact policy remain immutable authorization evidence.
	// Reads do not lock the policy head and cannot reverse publication lock order.
	original, err := loadResource(ctx, tx, scope, storageKey, 1, true)
	if err != nil {
		return StoredResource{}, err
	}
	var signed agentv1.SignedResource
	if err := proto.Unmarshal(original.Body, &signed); err != nil {
		return StoredResource{}, client.ErrResource
	}
	bundle, originalClaims, err := client.VerifyBundle(verifier, &signed, scope, request.BundleHash, client.Watermark{}, now)
	if err != nil {
		return StoredResource{}, err
	}
	if originalClaims.Revision != 1 || originalClaims.ExpiresUnixMs != original.ExpiresAt.UnixMilli() || epoch <= 0 || originalClaims.ControlPlaneEpoch != uint64(epoch) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane epoch changed")
	}
	document := bundle.Document()
	policy, err := loadResource(ctx, tx, scope, document.Policy.Key, document.Policy.Revision, true)
	if err != nil {
		return StoredResource{}, err
	}
	var signedPolicy agentv1.SignedResource
	if err := proto.Unmarshal(policy.Body, &signedPolicy); err != nil {
		return StoredResource{}, client.ErrResource
	}
	_, policyClaims, err := bundle.VerifyPolicy(verifier, &signedPolicy, client.Watermark{}, now)
	if err != nil {
		return StoredResource{}, err
	}
	if policyClaims.ExpiresUnixMs != policy.ExpiresAt.UnixMilli() {
		return StoredResource{}, client.ErrResource
	}
	expires := now.Add(p.lifetime)
	if hard := time.UnixMilli(document.ValidUntilUnixMs); hard.Before(expires) {
		expires = hard
	}
	if policy.ExpiresAt.Before(expires) {
		expires = policy.ExpiresAt
	}
	if !expires.After(now) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "bundle authorization expired")
	}
	claims := client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(request.BundleHash), ControlPlaneEpoch: uint64(epoch), Revision: current + 1, BundleHash: request.BundleHash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: expires.UnixMilli(), KeyID: p.keyID}
	result, err := p.insertBundleResource(ctx, tx, storageKey, claims, signed.Payload)
	if err != nil {
		return StoredResource{}, err
	}
	var refreshed agentv1.SignedResource
	if err := proto.Unmarshal(result.Body, &refreshed); err != nil {
		return StoredResource{}, err
	}
	if _, _, err := client.VerifyBundle(verifier, &refreshed, scope, request.BundleHash, originalClaims.Watermark(), now); err != nil {
		return StoredResource{}, err
	}
	updated, err := tx.Exec(ctx, `UPDATE agent_resource_operations SET revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, strconv.FormatUint(result.Revision, 10))
	if err != nil {
		return StoredResource{}, err
	}
	if updated.RowsAffected() != 1 {
		return StoredResource{}, client.ErrResource
	}
	if err := signedResourceStillFresh(ctx, tx, result.ExpiresAt); err != nil {
		return StoredResource{}, err
	}
	return result, tx.Commit(ctx)
}
