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

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

type StartActivationRequest struct {
	Scope                        client.Scope
	PrincipalID, OperationID     string
	Generation, ExpectedRevision uint64
	RequiredSourceIDs            []string
	AuthorizationExpiresAt       time.Time
	Authorize                    func(context.Context, pgx.Tx) error
}

// ActivationReceipt is immutable historical intent, not delivery/Applied evidence.
type ActivationReceipt struct {
	Release           ReleaseRecord `json:"release"`
	ViewKey           string        `json:"viewKey"`
	ArtifactRevision  uint64        `json:"artifactRevision,string"`
	SHA256            string        `json:"sha256"`
	SourceCount       uint64        `json:"sourceCount,string"`
	DiscoveryRevision uint64        `json:"discoveryRevision,string"`
	PreparedRevision  uint64        `json:"preparedRevision,string"`
	CreatedAt         time.Time     `json:"createdAt"`
	ExpiresAt         time.Time     `json:"expiresAt"`
}

func activationStorageKey(generation uint64) string {
	return "activation/" + strconv.FormatUint(generation, 10)
}

// StartActivation re-evaluates current preparation at the atomic activation
// decision. Lock order matches EvaluatePreparation. Required sources are explicit
// approved intent; they cannot be inferred from whatever reports happened to arrive.
func (p *Publisher) StartActivation(ctx context.Context, q StartActivationRequest, v *client.Verifier) (ActivationReceipt, error) {
	return p.activate(ctx, q, v, 0)
}

// RefreshActivation re-evaluates all positive authority. RequiredSourceIDs must
// equal the original approved source set; this operation cannot shrink it.
// Each refresh requires fresh authorization and an exact release/artifact CAS.
func (p *Publisher) RefreshActivation(ctx context.Context, q StartActivationRequest, expectedArtifactRevision uint64, v *client.Verifier) (ActivationReceipt, error) {
	if expectedArtifactRevision == 0 || expectedArtifactRevision == math.MaxUint64 {
		return ActivationReceipt{}, denied()
	}
	return p.activate(ctx, q, v, expectedArtifactRevision)
}

func (p *Publisher) activate(ctx context.Context, q StartActivationRequest, v *client.Verifier, expectedArtifact uint64) (ActivationReceipt, error) {
	artifact := expectedArtifact + 1

	if !scopeValid(q.Scope) || !identifier(q.PrincipalID) || !identifier(q.OperationID) || q.Generation == 0 || q.ExpectedRevision == 0 || q.ExpectedRevision == math.MaxUint64 || v == nil || len(q.RequiredSourceIDs) == 0 || len(q.RequiredSourceIDs) > client.MaxDiscoveryMembers || !slices.IsSorted(q.RequiredSourceIDs) {
		return ActivationReceipt{}, denied()
	}
	q.RequiredSourceIDs = slices.Clone(q.RequiredSourceIDs)
	for i, id := range q.RequiredSourceIDs {
		if !identifier(id) || (i > 0 && id == q.RequiredSourceIDs[i-1]) {
			return ActivationReceipt{}, denied()
		}
	}
	request, err := json.Marshal(struct {
		Scope       client.Scope `json:"scope"`
		Principal   string       `json:"principal"`
		Operation   string       `json:"operation"`
		Generation  uint64       `json:"generation,string"`
		Revision    uint64       `json:"revision,string"`
		Sources     []string     `json:"sources"`
		HasDeadline bool         `json:"hasDeadline"`
		Deadline    int64        `json:"deadline,string"`
		Artifact    uint64       `json:"artifact,string,omitzero"`
	}{q.Scope, q.PrincipalID, q.OperationID, q.Generation, q.ExpectedRevision, q.RequiredSourceIDs, !q.AuthorizationExpiresAt.IsZero(), q.AuthorizationExpiresAt.UnixMilli(), expectedArtifact}, json.Deterministic(true))
	if err != nil {
		return ActivationReceipt{}, err
	}
	domain := "grayling.agent.v1.activation-start\x00"
	if expectedArtifact > 0 {
		domain = "grayling.agent.v1.activation-refresh\x00"
	}
	hash := sha256.Sum256(append([]byte(domain), request...))
	tx, err := p.sessions.pool.Begin(ctx)
	if err != nil {
		return ActivationReceipt{}, err
	}
	defer tx.Rollback(ctx)
	old, err := releaseOperation(ctx, tx, q.Scope, q.PrincipalID, q.OperationID, hash)
	if err != nil {
		return ActivationReceipt{}, err
	}
	if len(old) > 0 {
		var receipt ActivationReceipt
		if json.Unmarshal(old, &receipt, json.RejectUnknownMembers(true)) != nil || receipt.Release.Definition.Scope != q.Scope || receipt.Release.Generation != q.Generation || receipt.Release.Revision != q.ExpectedRevision+1 || receipt.Release.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING || !releaseRecordValid(receipt.Release) || receipt.ViewKey != client.ActivationKey(q.Generation) || receipt.ArtifactRevision != artifact || receipt.SourceCount != uint64(len(q.RequiredSourceIDs)) {
			return receipt, client.ErrResource
		}
		return receipt, tx.Commit(ctx)
	}
	credentials, err := lockPreparationCredentials(ctx, tx, q.Scope, q.Generation)
	if err != nil {
		return ActivationReceipt{}, err
	}
	var admission int
	if err := tx.QueryRow(ctx, `SELECT max_connections FROM agent_scope_limits WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR SHARE`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey).Scan(&admission); err != nil {
		return ActivationReceipt{}, err
	}
	state, err := readRegistrationScope(ctx, tx, q.Scope)
	if err != nil {
		return ActivationReceipt{}, err
	}
	var r ReleaseRecord
	var now time.Time
	if expectedArtifact == 0 {
		r, now, err = lockPreparingRelease(ctx, tx, q.Scope, q.Generation)
	} else {
		r, now, err = lockCurrentRelease(ctx, tx, q.Scope, q.Generation)
	}
	if err != nil {
		return ActivationReceipt{}, err
	}
	if r.Revision != q.ExpectedRevision {
		return ActivationReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "release state changed")
	}
	if expectedArtifact == 0 && r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARED || expectedArtifact > 0 && r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING && r.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING && r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVE {
		return ActivationReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "release is not prepared")
	}
	if expectedArtifact > 0 {
		if err := lockActivationRefreshIntent(ctx, tx, q, r, expectedArtifact, v); err != nil {
			return ActivationReceipt{}, err
		}
	}
	if !q.AuthorizationExpiresAt.IsZero() && !q.AuthorizationExpiresAt.After(now) {
		return ActivationReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "activation approval expired")
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return ActivationReceipt{}, err
	}
	revocationEpoch, err := checkBundleRevocation(ctx, tx, q.Scope, r.ControlPlaneEpoch, hashFromRelease(r), v)
	if err != nil {
		return ActivationReceipt{}, err
	}
	members := []client.ActivationMember{}
	evaluation, err := evaluatePreparationSnapshotLocked(ctx, tx, r, state, credentials, now, &members)
	if err != nil {
		return ActivationReceipt{}, err
	}
	if !evaluation.Eligible {
		return ActivationReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "current preparation capacity is insufficient")
	}
	sources := make([]client.ActivationSource, 0, len(q.RequiredSourceIDs))
	byID := make(map[string]client.ActivationMember, len(members))
	for _, m := range members {
		byID[m.Instance.InstanceID] = m
	}
	for _, id := range q.RequiredSourceIDs {
		m, ok := byID[id]
		if !ok {
			return ActivationReceipt{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "required source is not currently prepared")
		}
		sourceIntent, _, err := readPrepareIntent(ctx, tx, q.Scope, m.PrepareCommandID)
		if err != nil {
			return ActivationReceipt{}, err
		}
		if credentials[sourceIntent.CredentialID].permissions&DiscoveryRead == 0 {
			return ActivationReceipt{}, denied()
		}
		digest := sha256.Sum256(append(append([]byte("grayling.agent.v1.activate-command\x00"), hash[:]...), []byte(id)...))
		sources = append(sources, client.ActivationSource{InstanceID: id, BootID: m.Instance.BootID, LeaseID: m.Instance.Lease.ID, LeaseEpoch: m.Instance.Lease.Epoch, PrepareCommandID: m.PrepareCommandID, CommandID: "activate-" + hex.EncodeToString(digest[:])})
	}
	if q.Authorize != nil {
		if err := q.Authorize(ctx, tx); err != nil {
			return ActivationReceipt{}, err
		}
	}
	expiry := evaluation.ValidUntil
	for _, limit := range []time.Time{now.Add(p.lifetime), now.Add(60 * time.Second), q.AuthorizationExpiresAt} {
		if !limit.IsZero() && limit.Before(expiry) {
			expiry = limit
		}
	}
	expiry = expiry.Truncate(time.Millisecond).UTC()
	created := now.Truncate(time.Millisecond).UTC()
	document := client.ActivationDocument{SchemaVersion: 1, Purpose: "activation-view", Scope: q.Scope, ControlPlaneEpoch: r.ControlPlaneEpoch, Generation: r.Generation, ReleaseID: r.ReleaseID, BundleHash: r.Definition.BundleHash, BundleArtifactRevision: r.Definition.BundleArtifactRevision, PolicyRevision: r.Definition.Document.Policy.Revision, DiscoveryRevision: state.DiscoveryRevision, PreparedRevision: evaluation.EvidenceRevision, RevocationEpoch: revocationEpoch, Members: members, Sources: sources}
	payload, err := client.MarshalActivation(document)
	if err != nil {
		return ActivationReceipt{}, err
	}
	var bundle [32]byte
	decoded, _ := hex.DecodeString(r.Definition.BundleHash)
	copy(bundle[:], decoded)
	signed, err := SignResource(client.Claims{Scope: q.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION, Key: client.ActivationKey(q.Generation), ControlPlaneEpoch: r.ControlPlaneEpoch, Revision: artifact, BundleHash: bundle, IssuedUnixMs: created.UnixMilli(), ExpiresUnixMs: expiry.UnixMilli(), KeyID: p.keyID}, payload, p.key)
	if err != nil {
		return ActivationReceipt{}, err
	}
	if _, _, err := client.VerifyActivation(v, signed, q.Scope, q.Generation, bundle, client.Watermark{}, now); err != nil {
		return ActivationReceipt{}, err
	}
	body, err := proto.Marshal(signed)
	if err != nil || len(body) > client.MaxResourceBytes {
		return ActivationReceipt{}, client.ErrResource
	}
	digest := sha256.Sum256(body)
	from := r.State
	r.State = agentv1.ReleaseState_RELEASE_STATE_ACTIVATING
	r.Revision++
	changed, err := tx.Exec(ctx, `UPDATE agent_releases SET state=$5,revision=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey, strconv.FormatUint(q.Generation, 10), int32(r.State), strconv.FormatUint(r.Revision, 10))
	if err != nil {
		return ActivationReceipt{}, err
	}
	if changed.RowsAffected() != 1 {
		return ActivationReceipt{}, client.ErrResource
	}
	if err := releaseEvent(ctx, tx, r, from, q.PrincipalID, q.OperationID, "activation authorized with current capacity"); err != nil {
		return ActivationReceipt{}, err
	}
	receipt := ActivationReceipt{Release: r, ViewKey: client.ActivationKey(q.Generation), ArtifactRevision: artifact, SHA256: hex.EncodeToString(digest[:]), SourceCount: uint64(len(sources)), DiscoveryRevision: state.DiscoveryRevision, PreparedRevision: evaluation.EvidenceRevision, CreatedAt: created, ExpiresAt: expiry}
	if err := storeActivation(ctx, tx, receipt, body, digest, sources); err != nil {
		return receipt, err
	}
	result, err := json.Marshal(receipt, json.Deterministic(true))
	if err != nil {
		return receipt, err
	}
	saved, err := tx.Exec(ctx, `UPDATE agent_release_operations SET result=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, q.Scope.WorkspaceID, q.Scope.EnvironmentID, q.Scope.ServiceKey, q.PrincipalID, q.OperationID, result)
	if err != nil {
		return receipt, err
	}
	if saved.RowsAffected() != 1 {
		return receipt, client.ErrResource
	}
	if err := preparationStillFresh(ctx, tx, now, expiry); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}

func storeActivation(ctx context.Context, tx pgx.Tx, r ActivationReceipt, body []byte, hash [32]byte, sources []client.ActivationSource) error {
	scope := r.Release.Definition.Scope
	g := strconv.FormatUint(r.Release.Generation, 10)
	key := activationStorageKey(r.Release.Generation)
	artifact := strconv.FormatUint(r.ArtifactRevision, 10)
	previous := strconv.FormatUint(r.ArtifactRevision-1, 10)
	headSQL := `INSERT INTO agent_resource_heads VALUES($1,$2,$3,$4,$5)`
	args := []any{scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, key, artifact}
	if r.ArtifactRevision > 1 {
		headSQL = `UPDATE agent_resource_heads SET revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND resource_key=$4 AND revision=$6`
		args = append(args, previous)
	}
	inserted, err := tx.Exec(ctx, headSQL, args...)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return client.ErrResource
	}
	inserted, err = tx.Exec(ctx, `INSERT INTO agent_resources VALUES($1,$2,$3,$4,$8,$5,$6,$7)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, key, body, hash[:], r.ExpiresAt, artifact)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return client.ErrResource
	}
	activationSQL := `INSERT INTO agent_activations VALUES($1,$2,$3,$4,$5,$6,$9,$7,$8)`
	args = []any{scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, strconv.FormatUint(r.Release.Revision, 10), key, r.CreatedAt, r.ExpiresAt, artifact}
	if r.ArtifactRevision > 1 {
		activationSQL = `UPDATE agent_activations SET release_revision=$5,resource_key=$6,created_at=$7,expires_at=$8,resource_revision=$9 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND resource_revision=$10`
		args = append(args, previous)
	}
	inserted, err = tx.Exec(ctx, activationSQL, args...)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return client.ErrResource
	}
	inserted, err = tx.Exec(ctx, `INSERT INTO agent_activation_versions SELECT * FROM agent_activations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return client.ErrResource
	}
	inserted, err = tx.Exec(ctx, `INSERT INTO agent_release_resource_refs VALUES($1,$2,$3,$4,$5,$6)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, key, artifact)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return client.ErrResource
	}
	for _, source := range sources {
		intent, _, err := readPrepareIntent(ctx, tx, scope, source.PrepareCommandID)
		if err != nil {
			return err
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO agent_activation_sources VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, source.InstanceID, source.CommandID, intent.CredentialID, source.PrepareCommandID, artifact)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() != 1 {
			return client.ErrResource
		}
	}
	return nil
}
