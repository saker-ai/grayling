package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

type PublishBundleRequest struct {
	PrincipalID, OperationID string
	ExpectedPolicyRevision   uint64
	// Nonzero authorization deadline is checked only for new mint, not replay.
	AuthorizationExpiresAt time.Time
	// AuthorizeNewPublication is a trusted control-plane guard. It runs only for
	// a new mint, inside the effect transaction; locks must remain held to commit.
	// It is not a client-supplied operation input or a replacement for the deadline.
	AuthorizeNewPublication func(context.Context, pgx.Tx) error
	Document                client.BundleDocument
	PolicyPayload           []byte
}
type PublishedBundle struct {
	BundleHash     [32]byte
	Bundle, Policy StoredResource
}

// bundleStorageKey cannot collide with any valid policy key. The slash is
// private storage separation; signed claims always use client.BundleKey.
func bundleStorageKey(hash [32]byte) string { return "bundle/" + hex.EncodeToString(hash[:]) }

// PublishBundle atomically stores an exact Bundle/policy pair. Its caller must
// independently authorize the complete source and build/resource mapping.
// It creates no release generation, Prepared evidence or active routes.
func (p *Publisher) PublishBundle(ctx context.Context, request PublishBundleRequest) (PublishedBundle, error) {
	d := request.Document
	if !scopeValid(d.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) {
		return PublishedBundle{}, denied()
	}
	if request.ExpectedPolicyRevision == math.MaxUint64 || d.Policy.Revision != request.ExpectedPolicyRevision+1 {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "policy revision changed")
	}
	body, err := client.MarshalBundle(d)
	if err != nil {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid bundle")
	}
	if _, err := client.CompileBundlePolicy(d, request.PolicyPayload); err != nil {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid bundle policy dependency")
	}
	hash := sha256.Sum256(body)
	payloadHash := sha256.Sum256(request.PolicyPayload)
	input := append([]byte("grayling.agent.v1.bundle-policy-pair\x00"), hash[:]...)
	input = append(input, payloadHash[:]...)
	input = binary.BigEndian.AppendUint64(input, request.ExpectedPolicyRevision)
	if !request.AuthorizationExpiresAt.IsZero() {
		input = append(input, []byte("\x00authorization-deadline\x00")...)
		input = binary.BigEndian.AppendUint64(input, uint64(request.AuthorizationExpiresAt.UnixMilli()))
	}
	operationHash := sha256.Sum256(input)
	tx, err := p.sessions.pool.Begin(ctx)
	if err != nil {
		return PublishedBundle{}, err
	}
	defer tx.Rollback(ctx)
	scope := d.Scope
	if _, err := tx.Exec(ctx, `DELETE FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 AND expires_at<=clock_timestamp()`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID); err != nil {
		return PublishedBundle{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_operations VALUES($1,$2,$3,$4,$5,$6,$7,0,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, operationHash[:], d.Policy.Key); err != nil {
		return PublishedBundle{}, err
	}
	var previousHash []byte
	var previousText string
	if err := tx.QueryRow(ctx, `SELECT request_hash,revision::text FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID).Scan(&previousHash, &previousText); err != nil {
		return PublishedBundle{}, err
	}
	if !bytes.Equal(previousHash, operationHash[:]) {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "operation payload changed")
	}
	previous, err := strconv.ParseUint(previousText, 10, 64)
	if err != nil {
		return PublishedBundle{}, err
	}
	if previous > 0 {
		if previous != d.Policy.Revision {
			return PublishedBundle{}, client.ErrResource
		}
		policy, err := loadResource(ctx, tx, scope, d.Policy.Key, previous, false)
		if err != nil {
			return PublishedBundle{}, err
		}
		bundle, err := loadResource(ctx, tx, scope, bundleStorageKey(hash), 1, false)
		if err != nil {
			return PublishedBundle{}, err
		}
		if !storedBundleMember(policy, scope, agentv1.ResourceKind_RESOURCE_KIND_POLICY, d.Policy.Key, previous, d.Policy.ControlPlaneEpoch, hash, request.PolicyPayload) || !storedBundleMember(bundle, scope, agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, client.BundleKey(hash), 1, d.Policy.ControlPlaneEpoch, hash, body) {
			return PublishedBundle{}, client.ErrResource
		}
		if err := tx.Commit(ctx); err != nil {
			return PublishedBundle{}, err
		}
		return PublishedBundle{BundleHash: hash, Bundle: bundle, Policy: policy}, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, d.Policy.Key); err != nil {
		return PublishedBundle{}, err
	}
	var currentText string
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, d.Policy.Key).Scan(&currentText); err != nil {
		return PublishedBundle{}, err
	}
	current, err := strconv.ParseUint(currentText, 10, 64)
	if err != nil {
		return PublishedBundle{}, err
	}
	if current != request.ExpectedPolicyRevision {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "policy revision changed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundleStorageKey(hash)); err != nil {
		return PublishedBundle{}, err
	}
	var bundleRevision string
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundleStorageKey(hash)).Scan(&bundleRevision); err != nil {
		return PublishedBundle{}, err
	}
	if bundleRevision != "0" {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "bundle already exists")
	}
	var epoch int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return PublishedBundle{}, err
	}
	local := time.Now()
	if now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) || uint64(epoch) != d.Policy.ControlPlaneEpoch {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane epoch or clock changed")
	}
	if !request.AuthorizationExpiresAt.IsZero() && now.UnixMilli() >= request.AuthorizationExpiresAt.UnixMilli() {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "publication approval expired")
	}
	if request.AuthorizeNewPublication != nil {
		if err := request.AuthorizeNewPublication(ctx, tx); err != nil {
			return PublishedBundle{}, err
		}
	}
	if _, err := checkBundleRevocation(ctx, tx, scope, uint64(epoch), hash, nil); err != nil {
		return PublishedBundle{}, err
	}
	expires := now.Add(p.lifetime)
	if hard := time.UnixMilli(d.ValidUntilUnixMs); hard.Before(expires) {
		expires = hard
	}
	if !expires.After(now) {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "bundle authorization expired")
	}
	claims := client.Claims{Scope: scope, ControlPlaneEpoch: uint64(epoch), BundleHash: hash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: expires.UnixMilli(), KeyID: p.keyID}
	claims.Kind = agentv1.ResourceKind_RESOURCE_KIND_POLICY
	claims.Key = d.Policy.Key
	claims.Revision = d.Policy.Revision
	policy, err := p.insertBundleResource(ctx, tx, d.Policy.Key, claims, request.PolicyPayload)
	if err != nil {
		return PublishedBundle{}, err
	}
	claims.Kind = agentv1.ResourceKind_RESOURCE_KIND_BUNDLE
	claims.Key = client.BundleKey(hash)
	claims.Revision = 1
	bundle, err := p.insertBundleResource(ctx, tx, bundleStorageKey(hash), claims, body)
	if err != nil {
		return PublishedBundle{}, err
	}
	updated, err := tx.Exec(ctx, `UPDATE agent_resource_operations SET revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, strconv.FormatUint(d.Policy.Revision, 10))
	if err != nil {
		return PublishedBundle{}, err
	}
	if updated.RowsAffected() != 1 {
		return PublishedBundle{}, client.ErrResource
	}
	var finalTime time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&finalTime); err != nil {
		return PublishedBundle{}, err
	}
	if !request.AuthorizationExpiresAt.IsZero() && finalTime.UnixMilli() >= request.AuthorizationExpiresAt.UnixMilli() {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "publication approval expired before commit")
	}
	if !finalTime.Before(time.UnixMilli(claims.ExpiresUnixMs)) {
		return PublishedBundle{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "bundle expired before commit")
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishedBundle{}, err
	}
	return PublishedBundle{BundleHash: hash, Bundle: bundle, Policy: policy}, nil
}
func (p *Publisher) insertBundleResource(ctx context.Context, tx pgx.Tx, storageKey string, claims client.Claims, payload []byte) (StoredResource, error) {
	signed, err := SignResource(claims, payload, p.key)
	if err != nil {
		return StoredResource{}, err
	}
	body, err := proto.Marshal(signed)
	if err != nil {
		return StoredResource{}, err
	}
	if len(body) > client.MaxResourceBytes {
		return StoredResource{}, client.ErrResource
	}
	hash := sha256.Sum256(body)
	expires := time.UnixMilli(claims.ExpiresUnixMs)
	scope := claims.Scope
	revision := strconv.FormatUint(claims.Revision, 10)
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_resources VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, storageKey, revision, body, hash[:], expires)
	if err != nil {
		return StoredResource{}, err
	}
	if inserted.RowsAffected() != 1 {
		return StoredResource{}, client.ErrResource
	}
	result, err := tx.Exec(ctx, `UPDATE agent_resource_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, storageKey, revision)
	if err != nil {
		return StoredResource{}, err
	}
	if result.RowsAffected() != 1 {
		return StoredResource{}, client.ErrResource
	}
	return StoredResource{Revision: claims.Revision, Body: body, Hash: hash, ExpiresAt: expires}, nil
}
func storedBundleMember(resource StoredResource, scope client.Scope, kind agentv1.ResourceKind, key string, revision, epoch uint64, bundleHash [32]byte, payload []byte) bool {
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		return false
	}
	claims, err := client.ParseClaims(signed.Claims)
	return err == nil && claims.Scope == scope && claims.Kind == kind && claims.Key == key && claims.Revision == revision && claims.ControlPlaneEpoch == epoch && claims.KeyID == signed.KeyId && len(signed.Signature) == ed25519.SignatureSize && claims.BundleHash == bundleHash && claims.PayloadHash == sha256.Sum256(signed.Payload) && resource.ExpiresAt.UnixMilli() == claims.ExpiresUnixMs && bytes.Equal(signed.Payload, payload)
}
