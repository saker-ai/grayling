package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func lockCurrentRelease(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (ReleaseRecord, time.Time, error) {
	var head string
	if err := tx.QueryRow(ctx, `SELECT generation::text FROM agent_release_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&head); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReleaseRecord{}, time.Time{}, denied()
		}
		return ReleaseRecord{}, time.Time{}, err
	}
	if head != strconv.FormatUint(generation, 10) {
		return ReleaseRecord{}, time.Time{}, fenced()
	}
	var epoch int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &now); err != nil {
		return ReleaseRecord{}, now, err
	}
	r, err := readRelease(ctx, tx, scope, generation, true)
	if err != nil {
		return r, now, err
	}
	local := time.Now()
	if epoch <= 0 || r.ControlPlaneEpoch != uint64(epoch) || now.Before(local.Add(-5*time.Second)) || now.After(local.Add(5*time.Second)) {
		return r, now, fenced()
	}
	return r, now.UTC(), nil
}

func lockPreparingRelease(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (ReleaseRecord, time.Time, error) {
	r, now, err := lockCurrentRelease(ctx, tx, scope, generation)
	if err != nil {
		return r, now, err
	}
	if r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARING && r.State != agentv1.ReleaseState_RELEASE_STATE_PREPARED {
		return r, now, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "release is not accepting preparation")
	}
	if !r.PrepareDeadline.After(now) {
		return r, now, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "prepare deadline expired")
	}
	return r, now.UTC(), nil
}

// Commands can be explicitly renewed after initial activation. The original
// preparation window still governs PREPARING/PREPARED, never established phases.
func lockPrepareCommandRelease(ctx context.Context, tx pgx.Tx, scope client.Scope, generation uint64) (ReleaseRecord, time.Time, error) {
	r, now, err := lockCurrentRelease(ctx, tx, scope, generation)
	if err != nil {
		return r, now, err
	}
	switch r.State {
	case agentv1.ReleaseState_RELEASE_STATE_PREPARING, agentv1.ReleaseState_RELEASE_STATE_PREPARED:
		if !r.PrepareDeadline.After(now) {
			return r, now, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "prepare deadline expired")
		}
	case agentv1.ReleaseState_RELEASE_STATE_ACTIVATING, agentv1.ReleaseState_RELEASE_STATE_VERIFYING, agentv1.ReleaseState_RELEASE_STATE_ACTIVE:
	default:
		return r, now, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "release is not accepting preparation")
	}
	return r, now, nil
}

func verifyPreparingResources(ctx context.Context, tx pgx.Tx, r ReleaseRecord, v *client.Verifier, now time.Time) error {
	if v == nil {
		return denied()
	}
	decoded, err := hex.DecodeString(r.Definition.BundleHash)
	if err != nil || len(decoded) != 32 {
		return client.ErrResource
	}
	var hash [32]byte
	copy(hash[:], decoded)
	if _, err := checkBundleRevocation(ctx, tx, r.Definition.Scope, r.ControlPlaneEpoch, hash, v); err != nil {
		return err
	}
	artifact, err := loadResource(ctx, tx, r.Definition.Scope, bundleStorageKey(hash), r.Definition.BundleArtifactRevision, true)
	if err != nil {
		return err
	}
	var signed agentv1.SignedResource
	if proto.Unmarshal(artifact.Body, &signed) != nil {
		return client.ErrResource
	}
	bundle, bc, err := client.VerifyBundle(v, &signed, r.Definition.Scope, hash, client.Watermark{}, now)
	if err != nil {
		return err
	}
	if bc.Revision != r.Definition.BundleArtifactRevision || bc.ControlPlaneEpoch != r.ControlPlaneEpoch || bc.ExpiresUnixMs != artifact.ExpiresAt.UnixMilli() {
		return client.ErrResource
	}
	document := bundle.Document()
	policy, err := loadResource(ctx, tx, r.Definition.Scope, document.Policy.Key, document.Policy.Revision, true)
	if err != nil {
		return err
	}
	var sp agentv1.SignedResource
	if proto.Unmarshal(policy.Body, &sp) != nil {
		return client.ErrResource
	}
	_, pc, err := bundle.VerifyPolicy(v, &sp, client.Watermark{}, now)
	if err != nil {
		return err
	}
	if pc.ExpiresUnixMs != policy.ExpiresAt.UnixMilli() || min(bc.ExpiresUnixMs, pc.ExpiresUnixMs) != r.ResourcesExpiresAt.UnixMilli() {
		return client.ErrResource
	}
	return nil
}
func prepareInstanceValid(record *instanceRecord, state RegistrationPolicyState, r ReleaseRecord) bool {
	if record == nil || !record.active || record.policyRevision != state.Revision || !record.instance.Ready || record.instance.Draining || record.desiredRevision == 0 || record.confirmedRevision != record.desiredRevision {
		return false
	}
	for _, build := range r.Definition.Document.Builds {
		if record.instance.BuildId == build.ID {
			return record.instance.ImageDigest == build.ImageDigest && record.instance.ProtocolVersion == build.ProtocolVersion && record.instance.ProtocolVersion >= r.Definition.Document.MinimumProtocolVersion
		}
	}
	return false
}
func readPrepareIntent(ctx context.Context, tx pgx.Tx, scope client.Scope, command string) (PrepareIntent, []byte, error) {
	intent := PrepareIntent{Scope: scope, CommandID: command}
	var generation, revision, epoch string
	var descriptor, hash []byte
	err := tx.QueryRow(ctx, `SELECT generation::text,target_revision::text,credential_id,target_principal_id,agent_id,lease_id,lease_epoch::text,descriptor,created_at,expires_at,request_hash FROM agent_prepare_intents WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND command_id=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, command).Scan(&generation, &revision, &intent.CredentialID, &intent.TargetPrincipalID, &intent.AgentID, &intent.LeaseID, &epoch, &descriptor, &intent.CreatedAt, &intent.ExpiresAt, &hash)
	if err != nil {
		return intent, nil, err
	}
	intent.Generation, err = strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return intent, nil, err
	}
	intent.TargetRevision, err = strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return intent, nil, err
	}
	intent.LeaseEpoch, err = strconv.ParseUint(epoch, 10, 64)
	if err != nil {
		return intent, nil, err
	}
	intent.Instance = new(agentv1.Instance)
	if proto.Unmarshal(descriptor, intent.Instance) != nil {
		return intent, nil, client.ErrResource
	}
	canonical, marshalErr := (proto.MarshalOptions{Deterministic: true}).Marshal(descriptorIdentity(intent.Instance))
	if marshalErr != nil {
		return intent, nil, client.ErrResource
	}
	if !identifier(intent.CredentialID) || !identifier(intent.TargetPrincipalID) || !identifier(intent.AgentID) || !identifier(intent.LeaseID) || intent.LeaseEpoch == 0 || command != "prepare-"+hex.EncodeToString(hash) {
		return intent, nil, client.ErrResource
	}
	if !validInstanceDescriptor(intent.Instance, scope) || intent.Instance.Lease != nil || !bytes.Equal(descriptor, canonical) || len(hash) != 32 || intent.Generation == 0 || intent.TargetRevision == 0 || !intent.ExpiresAt.After(intent.CreatedAt) {
		return intent, nil, client.ErrResource
	}
	intent.CreatedAt = intent.CreatedAt.UTC()
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	return intent, hash, nil
}
func prepareRequestHash(body []byte) [32]byte {
	return sha256.Sum256(append([]byte("grayling.agent.v1.prepare-intent\x00"), body...))
}
