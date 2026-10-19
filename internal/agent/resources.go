package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"google.golang.org/protobuf/proto"
)

// Publisher is control-plane-only. An Agent credential cannot publish policies.
// RBAC/approval must authorize its caller before invoking PublishPolicy.
type Publisher struct {
	sessions *Sessions
	keyID    string
	key      ed25519.PrivateKey
	lifetime time.Duration
}

func NewPublisher(s *Sessions, keyID string, key ed25519.PrivateKey, lifetime time.Duration) (*Publisher, error) {
	if s == nil || !identifier(keyID) || len(key) != ed25519.PrivateKeySize || lifetime < time.Second || lifetime > 24*time.Hour {
		return nil, client.ErrResource
	}
	return &Publisher{sessions: s, keyID: keyID, key: bytes.Clone(key), lifetime: lifetime}, nil
}

type PublishPolicyRequest struct {
	Scope                         client.Scope
	PrincipalID, OperationID, Key string
	ExpectedRevision              uint64
	BundleHash                    [32]byte
	// Preserve these bytes across retries; their digest is the operation identity.
	Payload []byte
}

// StoredResource is an immutable signed artifact, including its transport digest.
// Callers receive newly read byte slices, never shared mutable cache state.
type StoredResource struct {
	Revision  uint64
	Body      []byte
	Hash      [32]byte
	ExpiresAt time.Time
}

func (p *Publisher) PublishPolicy(ctx context.Context, request PublishPolicyRequest) (StoredResource, error) {
	if !scopeValid(request.Scope) || !identifier(request.PrincipalID) || !identifier(request.OperationID) || !model.KeyPattern.MatchString(request.Key) || request.BundleHash == [32]byte{} {
		return StoredResource{}, denied()
	}
	// Compile the complete candidate before holding any governance locks.
	if _, err := client.CompilePolicy(request.Payload, request.Scope); err != nil {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid policy snapshot")
	}
	digest := sha256.Sum256(request.Payload)
	input := binary.BigEndian.AppendUint16(nil, uint16(len(request.Key)))
	input = append(input, request.Key...)
	input = binary.BigEndian.AppendUint64(input, request.ExpectedRevision)
	input = append(input, request.BundleHash[:]...)
	input = append(input, digest[:]...)
	requestHash := sha256.Sum256(input)
	s := p.sessions
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	scope := request.Scope
	// The operation row serializes duplicate IDs across different keys as well as
	// nodes. Placeholder + resource + result commit in one transaction.
	if _, err := tx.Exec(ctx, `DELETE FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 AND expires_at<=clock_timestamp()`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID); err != nil {
		return StoredResource{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_operations VALUES($1,$2,$3,$4,$5,$6,$7,0,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, requestHash[:], request.Key); err != nil {
		return StoredResource{}, err
	}
	var previousHash []byte
	var revisionText string
	err = tx.QueryRow(ctx, `SELECT request_hash,revision::text FROM agent_resource_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID).Scan(&previousHash, &revisionText)
	if err != nil {
		return StoredResource{}, err
	}
	if !bytes.Equal(previousHash, requestHash[:]) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "operation payload changed")
	}
	previousRevision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return StoredResource{}, err
	}
	if previousRevision > 0 {
		resource, err := loadResource(ctx, tx, scope, request.Key, previousRevision, false)
		if err != nil {
			return StoredResource{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return StoredResource{}, err
		}
		return resource, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.Key); err != nil {
		return StoredResource{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.Key).Scan(&revisionText); err != nil {
		return StoredResource{}, err
	}
	current, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return StoredResource{}, err
	}
	if current != request.ExpectedRevision || current == math.MaxUint64 {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "policy revision changed")
	}
	var now time.Time
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return StoredResource{}, err
	}
	if now.Before(time.Now().Add(-5*time.Second)) || now.After(time.Now().Add(5*time.Second)) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane clock outside budget")
	}
	if _, err := checkBundleRevocation(ctx, tx, scope, uint64(epoch), request.BundleHash, nil); err != nil {
		return StoredResource{}, err
	}
	revision := current + 1
	claims := client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: request.Key, ControlPlaneEpoch: uint64(epoch), Revision: revision, BundleHash: request.BundleHash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(p.lifetime).UnixMilli(), KeyID: p.keyID}
	signed, err := SignResource(claims, request.Payload, p.key)
	if err != nil {
		return StoredResource{}, err
	}
	body, err := proto.Marshal(signed)
	if err != nil {
		return StoredResource{}, err
	}
	if len(body) > client.MaxResourceBytes {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "signed resource too large")
	}
	hash := sha256.Sum256(body)
	expires := time.UnixMilli(claims.ExpiresUnixMs)
	nextText := strconv.FormatUint(revision, 10)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resources VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.Key, nextText, body, hash[:], expires); err != nil {
		return StoredResource{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_resource_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.Key, nextText); err != nil {
		return StoredResource{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_resource_operations SET revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, request.PrincipalID, request.OperationID, nextText); err != nil {
		return StoredResource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredResource{}, err
	}
	return StoredResource{Revision: revision, Body: body, Hash: hash, ExpiresAt: expires}, nil
}
func loadResource(ctx context.Context, tx pgx.Tx, scope client.Scope, key string, revision uint64, requireFresh bool) (StoredResource, error) {
	var resource StoredResource
	var hash []byte
	var revisionText string
	var valid bool
	err := tx.QueryRow(ctx, `SELECT r.revision::text,r.body,r.sha256,r.expires_at,r.expires_at>clock_timestamp() FROM agent_resources r JOIN agent_resource_heads h USING(workspace_id,environment_id,service_key,resource_key) WHERE r.workspace_id=$1 AND r.environment_id=$2 AND r.service_key=$3 AND r.resource_key=$4 AND r.revision=CASE WHEN $5::numeric=0 THEN h.revision ELSE $5::numeric END`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, key, strconv.FormatUint(revision, 10)).Scan(&revisionText, &resource.Body, &hash, &resource.ExpiresAt, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "resource unavailable")
	}
	if err != nil {
		return StoredResource{}, err
	}
	if requireFresh && !valid {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "resource expired")
	}
	resource.Revision, err = strconv.ParseUint(revisionText, 10, 64)
	if err != nil {
		return StoredResource{}, err
	}
	if len(hash) != 32 || len(resource.Body) > client.MaxResourceBytes {
		return StoredResource{}, client.ErrResource
	}
	copy(resource.Hash[:], hash)
	if sha256.Sum256(resource.Body) != resource.Hash {
		return StoredResource{}, client.ErrResource
	}
	return resource, nil
}

// Resource derives scope from the freshly authorized session. The credential,
// fencing check and read share one transaction; no cross-scope lookup occurs.
func (s *Sessions) Resource(ctx context.Context, se Session, key string, revision uint64) (StoredResource, error) {
	if !model.KeyPattern.MatchString(key) {
		return StoredResource{}, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead); err != nil {
		return StoredResource{}, err
	}
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch); err != nil {
		return StoredResource{}, err
	}
	resource, err := loadResource(ctx, tx, se.Grant.Scope, key, revision, true)
	if err != nil {
		return StoredResource{}, err
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		return StoredResource{}, client.ErrResource
	}
	claims, err := client.ParseClaims(signed.Claims)
	if err != nil {
		return StoredResource{}, err
	}
	if _, err := checkBundleRevocation(ctx, tx, se.Grant.Scope, uint64(epoch), claims.BundleHash, nil); err != nil {
		return StoredResource{}, err
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return StoredResource{}, err
	}
	if err := signedResourceStillFresh(ctx, tx, resource.ExpiresAt); err != nil {
		return StoredResource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredResource{}, err
	}
	return resource, nil
}
