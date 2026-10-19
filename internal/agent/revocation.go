package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

const revocationStorageKey = "revocation/state"

type RevokeBundlesRequest struct {
	TransactionGuard         func(context.Context, pgx.Tx) error `json:"-"`
	ExpectedArtifactRevision uint64                              `json:"expectedArtifactRevision,string,omitzero"`
	ExpectedEpoch            uint64                              `json:"expectedEpoch,string"`
	BundleHashes             []string                            `json:"bundleHashes"`
	CancelActiveStreams      bool                                `json:"cancelActiveStreams"`
	Reason                   string                              `json:"reason"`
	RefreshOnly              bool                                `json:"refreshOnly,omitzero"`
}

// RevokeBundles is an owner-only trusted control-plane operation. Denials, signed
// artifact, scope epoch, immutable receipt and audit commit as one transaction.
func (p *Publisher) RevokeBundles(ctx context.Context, scope client.Scope, actor AdminMutation, q RevokeBundlesRequest, v *client.Verifier) (StoredResource, error) {
	if v == nil || (len(q.BundleHashes) == 0 && !q.RefreshOnly) || (q.RefreshOnly && (len(q.BundleHashes) > 0 || q.CancelActiveStreams || q.ExpectedEpoch == 0)) || len(q.BundleHashes) > client.MaxRevokedBundles || !identifier(q.Reason) || q.ExpectedEpoch == math.MaxUint64 {
		return StoredResource{}, platform.ErrValidation
	}
	q.BundleHashes = slices.Clone(q.BundleHashes)
	slices.Sort(q.BundleHashes)
	for i, h := range q.BundleHashes {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 32 || h != hex.EncodeToString(raw) || bytes.Equal(raw, make([]byte, 32)) || i > 0 && q.BundleHashes[i-1] == h {
			return StoredResource{}, platform.ErrValidation
		}
	}
	action := "agent.bundle.revoke"
	if q.RefreshOnly {
		action = "agent.bundle.revocation.refresh"
	}
	body, _, err := p.sessions.adminMutation(ctx, scope, actor, action, q, func(tx pgx.Tx) (adminResult, error) {
		if q.TransactionGuard != nil {
			if err := q.TransactionGuard(ctx, tx); err != nil {
				return adminResult{}, err
			}
		}
		var epoch int64
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
			return adminResult{}, err
		}
		if epoch <= 0 || now.Before(time.Now().Add(-5*time.Second)) || now.After(time.Now().Add(5*time.Second)) {
			return adminResult{}, fenced()
		}
		d, previousArtifact, err := lockRevocations(ctx, tx, scope, uint64(epoch), true, v)
		if err != nil {
			return adminResult{}, err
		}
		if q.RefreshOnly && d.Epoch == 0 {
			return adminResult{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "revocation catalog unavailable")
		}
		if d.Epoch != q.ExpectedEpoch || q.ExpectedArtifactRevision != 0 && q.ExpectedArtifactRevision != previousArtifact.Revision || previousArtifact.Revision == math.MaxUint64 {
			return adminResult{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "revocation epoch changed")
		}
		previousDocument := d
		hashes := map[string]bool{}
		cancelled := map[string]bool{}
		for _, h := range d.BundleHashes {
			hashes[h] = true
		}
		for _, h := range d.CancelledHashes {
			cancelled[h] = true
		}
		for _, h := range q.BundleHashes {
			hashes[h] = true
			if q.CancelActiveStreams {
				cancelled[h] = true
			}
		}
		if len(hashes) > client.MaxRevokedBundles {
			return adminResult{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "revocation catalog full")
		}
		if !q.RefreshOnly && len(hashes) == len(d.BundleHashes) && len(cancelled) == len(d.CancelledHashes) {
			encoded, err := json.Marshal(previousArtifact, json.Deterministic(true))
			return adminResult{object: strconv.FormatUint(d.Epoch, 10), body: encoded}, err
		}
		nextEpoch := q.ExpectedEpoch + 1
		if q.RefreshOnly {
			nextEpoch = q.ExpectedEpoch
		}
		d = client.RevocationDocument{SchemaVersion: 1, Purpose: "bundle-revocation", Scope: scope, ControlPlaneEpoch: uint64(epoch), Epoch: nextEpoch, BundleHashes: []string{}, CancelledHashes: []string{}}
		for h := range hashes {
			d.BundleHashes = append(d.BundleHashes, h)
		}
		for h := range cancelled {
			d.CancelledHashes = append(d.CancelledHashes, h)
		}
		slices.Sort(d.BundleHashes)
		slices.Sort(d.CancelledHashes)
		if q.RefreshOnly {
			d = previousDocument
		}
		payload, err := client.MarshalRevocation(d)
		if err != nil {
			return adminResult{}, err
		}
		resource, err := p.insertBundleResource(ctx, tx, revocationStorageKey, client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, Key: client.RevocationKey, ControlPlaneEpoch: uint64(epoch), Revision: previousArtifact.Revision + 1, BundleHash: sha256.Sum256(payload), IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(p.lifetime).UnixMilli(), KeyID: p.keyID}, payload)
		if err != nil {
			return adminResult{}, err
		}
		var signed agentv1.SignedResource
		if proto.Unmarshal(resource.Body, &signed) != nil {
			return adminResult{}, client.ErrResource
		}
		if _, _, err := client.VerifyRevocation(v, &signed, scope, client.Watermark{}, now); err != nil {
			return adminResult{}, err
		}
		if err := signedResourceStillFresh(ctx, tx, resource.ExpiresAt); err != nil {
			return adminResult{}, err
		}
		encoded, err := json.Marshal(resource, json.Deterministic(true))
		return adminResult{object: strconv.FormatUint(d.Epoch, 10), body: encoded}, err
	})
	if err != nil {
		return StoredResource{}, err
	}
	var result StoredResource
	if json.Unmarshal(body, &result) != nil {
		return result, client.ErrResource
	}
	return result, nil
}

// Caller holds schema SHARE before this catalog lock. A permanent zero head
// serializes first publication with consumers; absence is never an unlocked grant.
func lockRevocations(ctx context.Context, tx pgx.Tx, scope client.Scope, epoch uint64, update bool, v *client.Verifier) (client.RevocationDocument, StoredResource, error) {
	empty := client.RevocationDocument{}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_resource_heads VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, revocationStorageKey); err != nil {
		return empty, StoredResource{}, err
	}
	query := `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR SHARE`
	if update {
		query = `SELECT revision::text FROM agent_resource_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 FOR UPDATE`
	}
	var text string
	if err := tx.QueryRow(ctx, query, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, revocationStorageKey).Scan(&text); err != nil {
		return empty, StoredResource{}, err
	}
	rev, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return empty, StoredResource{}, err
	}
	if rev == 0 {
		return empty, StoredResource{}, nil
	}
	resource, err := loadResource(ctx, tx, scope, revocationStorageKey, rev, false)
	if err != nil {
		return empty, resource, err
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		return empty, resource, client.ErrResource
	}
	claims, err := client.ParseClaims(signed.Claims)
	if err != nil {
		return empty, resource, err
	}
	var d client.RevocationDocument
	if json.Unmarshal(signed.Payload, &d, json.RejectUnknownMembers(true)) != nil {
		return empty, resource, client.ErrResource
	}
	payload, err := client.MarshalRevocation(d)
	if err != nil || !bytes.Equal(payload, signed.Payload) || claims.Scope != scope || d.Scope != scope || claims.ControlPlaneEpoch != epoch || d.ControlPlaneEpoch != epoch || d.Epoch > rev || claims.Revision != rev || claims.Key != client.RevocationKey || claims.Kind != agentv1.ResourceKind_RESOURCE_KIND_REVOCATION || claims.PayloadHash != sha256.Sum256(payload) || claims.BundleHash != claims.PayloadHash || claims.ExpiresUnixMs != resource.ExpiresAt.UnixMilli() {
		return empty, resource, client.ErrResource
	}
	// Previously committed denials remain binding after their delivery proof
	// expires. New clients still require a currently valid signed proof.
	if v != nil {
		if _, _, err := client.VerifyRevocation(v, &signed, scope, client.Watermark{}, time.UnixMilli(claims.IssuedUnixMs)); err != nil {
			return empty, resource, err
		}
	}
	return d, resource, nil
}
func checkBundleRevocation(ctx context.Context, tx pgx.Tx, scope client.Scope, epoch uint64, hash [32]byte, v *client.Verifier) (uint64, error) {
	d, _, err := lockRevocations(ctx, tx, scope, epoch, false, v)
	if err != nil {
		return 0, err
	}
	if slices.Contains(d.BundleHashes, hex.EncodeToString(hash[:])) {
		return d.Epoch, denied()
	}
	return d.Epoch, nil
}
func (s *Sessions) RevocationResource(ctx context.Context, se Session, revision uint64, v *client.Verifier) (StoredResource, error) {
	if v == nil {
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
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return StoredResource{}, err
	}
	d, resource, err := lockRevocations(ctx, tx, se.Grant.Scope, uint64(epoch), false, v)
	if err != nil {
		return resource, err
	}
	if d.Epoch == 0 {
		return resource, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "revocation snapshot unavailable")
	}
	currentPayload, err := client.MarshalRevocation(d)
	if err != nil {
		return resource, err
	}
	if revision != 0 && revision != resource.Revision {
		resource, err = loadResource(ctx, tx, se.Grant.Scope, revocationStorageKey, revision, true)
		if err != nil {
			return resource, err
		}
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(resource.Body, &signed) != nil {
		return resource, client.ErrResource
	}
	proof, claims, err := client.VerifyRevocation(v, &signed, se.Grant.Scope, client.Watermark{}, now)
	if err != nil {
		return resource, err
	}
	if proof.Epoch != d.Epoch || !bytes.Equal(signed.Payload, currentPayload) {
		return resource, protocolError(agentv1.ErrorCode_ERROR_CODE_RESYNC_REQUIRED, "revocation content changed")
	}
	if claims.Revision != resource.Revision || claims.ExpiresUnixMs != resource.ExpiresAt.UnixMilli() {
		return resource, client.ErrResource
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return resource, err
	}
	if err := signedResourceStillFresh(ctx, tx, resource.ExpiresAt); err != nil {
		return resource, err
	}
	return resource, tx.Commit(ctx)
}

func hashFromRelease(r ReleaseRecord) [32]byte {
	raw, _ := hex.DecodeString(r.Definition.BundleHash)
	var hash [32]byte
	copy(hash[:], raw)
	return hash
}

// RefreshRevocation renews delivery proof for the cumulative denyset. It advances
// only the artifact revision, never clears entries or extends positive authorization.
func (p *Publisher) RefreshRevocation(ctx context.Context, scope client.Scope, actor AdminMutation, expectedEpoch uint64, v *client.Verifier) (StoredResource, error) {
	return p.RevokeBundles(ctx, scope, actor, RevokeBundlesRequest{ExpectedEpoch: expectedEpoch, RefreshOnly: true, Reason: "refresh revocation delivery proof"}, v)
}
