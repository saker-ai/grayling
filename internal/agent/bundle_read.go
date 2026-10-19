package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// BundleResource reads one exact immutable Bundle artifact. Authorization and
// fencing precede existence checks, and scope is derived from the session.
func (s *Sessions) BundleResource(ctx context.Context, se Session, hash [32]byte, revision uint64) (StoredResource, error) {
	if hash == [32]byte{} || revision == 0 {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "exact bundle version required")
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
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return StoredResource{}, err
	}
	if _, err := checkBundleRevocation(ctx, tx, se.Grant.Scope, uint64(epoch), hash, nil); err != nil {
		return StoredResource{}, err
	}
	result, err := loadResource(ctx, tx, se.Grant.Scope, bundleStorageKey(hash), revision, true)
	if err != nil {
		return StoredResource{}, err
	}
	var signed agentv1.SignedResource
	if err := proto.Unmarshal(result.Body, &signed); err != nil {
		return StoredResource{}, client.ErrResource
	}
	claims, err := client.ParseClaims(signed.Claims)
	if err != nil || claims.Kind != agentv1.ResourceKind_RESOURCE_KIND_BUNDLE || claims.Scope != se.Grant.Scope || claims.Key != client.BundleKey(hash) || claims.Revision != revision || claims.BundleHash != hash || claims.PayloadHash != sha256.Sum256(signed.Payload) || sha256.Sum256(signed.Payload) != hash || claims.KeyID != signed.KeyId || len(signed.Signature) != ed25519.SignatureSize || claims.ExpiresUnixMs != result.ExpiresAt.UnixMilli() || claims.IssuedUnixMs > now.UnixMilli() {
		return StoredResource{}, client.ErrResource
	}
	if epoch <= 0 || claims.ControlPlaneEpoch != uint64(epoch) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "resource unavailable")
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return StoredResource{}, err
	}
	if err := signedResourceStillFresh(ctx, tx, result.ExpiresAt); err != nil {
		return StoredResource{}, err
	}
	return result, tx.Commit(ctx)
}
