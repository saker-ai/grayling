package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// DiscoveryResource retrieves an exact immutable artifact under current reader
// authorization. Missing versions never fall back to a newer snapshot.
func (s *Sessions) DiscoveryResource(ctx context.Context, se Session, bundle [32]byte, revision uint64) (StoredResource, error) {
	if bundle == [32]byte{} || revision == 0 {
		return StoredResource{}, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, DiscoveryRead); err != nil {
		return StoredResource{}, err
	}
	var epoch int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return StoredResource{}, err
	}
	if _, err := checkBundleRevocation(ctx, tx, se.Grant.Scope, uint64(epoch), bundle, nil); err != nil {
		return StoredResource{}, err
	}
	scope := se.Grant.Scope
	var result StoredResource
	var hash []byte
	var storedEpoch int64
	err = tx.QueryRow(ctx, `SELECT body,sha256,expires_at,control_plane_epoch FROM agent_discovery_resources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND bundle_hash=$4 AND revision=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:], strconv.FormatUint(revision, 10)).Scan(&result.Body, &hash, &result.ExpiresAt, &storedEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "resource unavailable")
	}
	if err != nil {
		return StoredResource{}, err
	}
	if !result.ExpiresAt.After(now) || result.ExpiresAt.After(se.Grant.ExpiresAt) {
		return StoredResource{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "resource expired")
	}
	result.Revision = revision
	result.Hash = sha256.Sum256(result.Body)
	if len(result.Body) > client.MaxResourceBytes || !bytes.Equal(result.Hash[:], hash) {
		return StoredResource{}, client.ErrResource
	}
	signed := new(agentv1.SignedResource)
	if err := proto.Unmarshal(result.Body, signed); err != nil {
		return StoredResource{}, client.ErrResource
	}
	claims, err := client.ParseClaims(signed.Claims)
	if err != nil || epoch <= 0 || storedEpoch != epoch || claims.ControlPlaneEpoch != uint64(epoch) || claims.Revision != revision || claims.Scope != scope || claims.Kind != agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY || claims.Key != client.DiscoveryKey(bundle) || claims.BundleHash != bundle || claims.PayloadHash != sha256.Sum256(signed.Payload) || claims.KeyID != signed.KeyId || claims.ExpiresUnixMs != result.ExpiresAt.UnixMilli() || claims.IssuedUnixMs > now.UnixMilli() {
		return StoredResource{}, client.ErrResource
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return StoredResource{}, err
	}
	if err := signedResourceStillFresh(ctx, tx, result.ExpiresAt); err != nil {
		return StoredResource{}, err
	}
	return result, tx.Commit(ctx)
}
