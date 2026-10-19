package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// Discovery captures, signs and persists one immutable candidate artifact at a
// single governance barrier. Refresh gets a new artifact revision, independently
// of membership. Bundle binding grants no Prepared or routing authorization.
func (p *Publisher) Discovery(ctx context.Context, se Session, bundle [32]byte) (StoredResource, error) {
	if bundle == [32]byte{} {
		return StoredResource{}, denied()
	}
	var result StoredResource
	_, err := p.sessions.captureDiscovery(ctx, se, func(tx pgx.Tx, snapshot CandidateSnapshot) error {
		if _, err := checkBundleRevocation(ctx, tx, se.Grant.Scope, snapshot.ControlPlaneEpoch, bundle, nil); err != nil {
			return err
		}
		now := snapshot.IssuedAt
		if now.Before(time.Now().Add(-5*time.Second)) || now.After(time.Now().Add(5*time.Second)) {
			return protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "control-plane clock outside budget")
		}
		payload, err := EncodeDiscoveryCandidates(se.Grant.Scope, snapshot)
		if err != nil {
			return err
		}
		expires := snapshot.ExpiresAt
		if limit := now.Add(p.lifetime).Truncate(time.Millisecond); limit.Before(expires) {
			expires = limit
		}
		if expires.UnixMilli() <= now.UnixMilli() {
			return protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "resource lifetime exhausted")
		}
		scope := se.Grant.Scope
		if _, err := tx.Exec(ctx, `INSERT INTO agent_discovery_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:]); err != nil {
			return err
		}
		var currentText string
		if err := tx.QueryRow(ctx, `SELECT revision::text FROM agent_discovery_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND bundle_hash=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:]).Scan(&currentText); err != nil {
			return err
		}
		current, err := strconv.ParseUint(currentText, 10, 64)
		if err != nil {
			return err
		}
		if current > 0 {
			var old StoredResource
			var storedHash []byte
			var membership string
			var epoch int64
			err := tx.QueryRow(ctx, `SELECT body,sha256,expires_at,membership_revision::text,control_plane_epoch FROM agent_discovery_resources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND bundle_hash=$4 AND revision=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:], currentText).Scan(&old.Body, &storedHash, &old.ExpiresAt, &membership, &epoch)
			if errors.Is(err, pgx.ErrNoRows) {
				return client.ErrResource
			}
			if err != nil {
				return err
			}
			if len(old.Body) > client.MaxResourceBytes {
				return client.ErrResource
			}
			old.Hash = sha256.Sum256(old.Body)
			old.Revision = current
			if !bytes.Equal(storedHash, old.Hash[:]) {
				return client.ErrResource
			}
			signed := new(agentv1.SignedResource)
			if err := proto.Unmarshal(old.Body, signed); err != nil {
				return client.ErrResource
			}
			claims, err := client.ParseClaims(signed.Claims)
			if err != nil || claims.Revision != current || claims.ControlPlaneEpoch != uint64(epoch) || claims.BundleHash != bundle || claims.Scope != scope || claims.Kind != agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY || claims.Key != client.DiscoveryKey(bundle) || claims.ExpiresUnixMs != old.ExpiresAt.UnixMilli() || claims.PayloadHash != sha256.Sum256(signed.Payload) || claims.KeyID != signed.KeyId {
				return client.ErrResource
			}
			if claims.IssuedUnixMs > now.UnixMilli() {
				return protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "discovery clock moved backwards")
			}
			window := min(15*time.Second, p.lifetime/2, expires.Sub(now)/2)
			if epoch == int64(snapshot.ControlPlaneEpoch) && membership == strconv.FormatUint(snapshot.Revision, 10) && signed.KeyId == p.keyID && bytes.Equal(signed.Payload, payload) && !old.ExpiresAt.After(expires) && old.ExpiresAt.After(now.Add(window)) {
				if !ed25519.Verify(p.key.Public().(ed25519.PublicKey), append([]byte(client.SignatureDomain), signed.Claims...), signed.Signature) {
					return client.ErrResource
				}
				result = old
				return signedResourceStillFresh(ctx, tx, result.ExpiresAt)
			}
		}
		if current == math.MaxUint64 {
			return protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "discovery artifact revision exhausted")
		}
		next := current + 1
		claims := client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle), ControlPlaneEpoch: snapshot.ControlPlaneEpoch, Revision: next, BundleHash: bundle, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: expires.UnixMilli(), KeyID: p.keyID}
		signed, err := SignResource(claims, payload, p.key)
		if err != nil {
			return err
		}
		body, err := proto.Marshal(signed)
		if err != nil {
			return err
		}
		if len(body) > client.MaxResourceBytes {
			return client.ErrResource
		}
		hash := sha256.Sum256(body)
		text := strconv.FormatUint(next, 10)
		if _, err := tx.Exec(ctx, `INSERT INTO agent_discovery_resources VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:], text, strconv.FormatUint(snapshot.Revision, 10), int64(snapshot.ControlPlaneEpoch), body, hash[:], expires); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_discovery_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND bundle_hash=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, bundle[:], text); err != nil {
			return err
		}
		result = StoredResource{Revision: next, Body: body, Hash: hash, ExpiresAt: expires}
		return signedResourceStillFresh(ctx, tx, result.ExpiresAt)
	})
	if err != nil {
		return StoredResource{}, err
	}
	return result, nil
}

func signedResourceStillFresh(ctx context.Context, tx pgx.Tx, expires time.Time) error {
	var fresh bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, expires).Scan(&fresh); err != nil {
		return err
	}
	if !fresh {
		return protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "resource lifetime exhausted")
	}
	return nil
}
