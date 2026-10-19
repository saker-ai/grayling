package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// ActivationResource is exact and source-bound. There is no latest alias and
// merely knowing a service's activation key cannot authorize another instance.
func (s *Sessions) ActivationResource(ctx context.Context, se Session, key string, revision uint64, v *client.Verifier) (StoredResource, error) {
	generation, err := strconv.ParseUint(strings.TrimPrefix(key, "activation-"), 10, 64)
	if err != nil || generation == 0 || key != client.ActivationKey(generation) || revision == 0 || v == nil {
		return StoredResource{}, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StoredResource{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|DiscoveryRead|ReleaseReport); err != nil {
		return StoredResource{}, err
	}
	locked, err := lockActivationSource(ctx, tx, se, generation, revision, v, true)
	if err != nil {
		return StoredResource{}, err
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return StoredResource{}, err
	}
	if err := preparationStillFresh(ctx, tx, locked.Now, locked.Resource.ExpiresAt); err != nil {
		return StoredResource{}, err
	}
	return locked.Resource, tx.Commit(ctx)
}

type activationSourceView struct {
	Release  ReleaseRecord
	Resource StoredResource
	View     *client.ActivationView
	Source   client.ActivationSource
	Instance *instanceRecord
	Now      time.Time
}

// Caller holds its credential and session before registration scope. Negative
// installation reports retain identity/lease barriers but need not be Ready.
func lockActivationSource(ctx context.Context, tx pgx.Tx, se Session, generation, revision uint64, v *client.Verifier, requireReady bool) (activationSourceView, error) {
	state, err := readRegistrationScope(ctx, tx, se.Grant.Scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return activationSourceView{}, denied()
	}
	if err != nil {
		return activationSourceView{}, err
	}
	r, now, err := lockCurrentRelease(ctx, tx, se.Grant.Scope, generation)
	if err != nil {
		return activationSourceView{}, err
	}
	if r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVATING && r.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING && r.State != agentv1.ReleaseState_RELEASE_STATE_ACTIVE {
		return activationSourceView{}, protocolError(agentv1.ErrorCode_ERROR_CODE_NOT_READY, "release is not activating")
	}
	var command, credential, prepare string
	scope := se.Grant.Scope
	err = tx.QueryRow(ctx, `SELECT command_id,credential_id,prepare_command_id FROM agent_activation_sources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 AND artifact_revision=$6`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(generation, 10), se.Grant.InstanceID, strconv.FormatUint(revision, 10)).Scan(&command, &credential, &prepare)
	if errors.Is(err, pgx.ErrNoRows) {
		return activationSourceView{}, denied()
	}
	if err != nil {
		return activationSourceView{}, err
	}
	if credential != se.Grant.ID {
		return activationSourceView{}, fenced()
	}
	intent, _, err := readPrepareIntent(ctx, tx, scope, prepare)
	if err != nil {
		return activationSourceView{}, err
	}
	instance, err := readInstance(ctx, tx, scope, se.Grant.InstanceID)
	if err != nil {
		return activationSourceView{}, err
	}
	if !matchesPrepareTarget(intent, se, instance) || !instance.active || instance.policyRevision != state.Revision || !instance.expiresAt.After(now) || requireReady && !prepareInstanceValid(instance, state, r) {
		return activationSourceView{}, fenced()
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT command_id FROM agent_prepare_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(generation, 10), se.Grant.InstanceID).Scan(&current); err != nil {
		return activationSourceView{}, err
	}
	if current != prepare {
		return activationSourceView{}, fenced()
	}
	resource, err := loadResource(ctx, tx, scope, activationStorageKey(generation), revision, true)
	if err != nil {
		return activationSourceView{}, err
	}
	signed := new(agentv1.SignedResource)
	if proto.Unmarshal(resource.Body, signed) != nil {
		return activationSourceView{}, client.ErrResource
	}
	var bundle [32]byte
	decoded, _ := hex.DecodeString(r.Definition.BundleHash)
	copy(bundle[:], decoded)
	view, claims, err := client.VerifyActivation(v, signed, scope, generation, bundle, client.Watermark{}, now)
	if err != nil {
		return activationSourceView{}, err
	}
	if claims.Revision != revision || claims.ControlPlaneEpoch != r.ControlPlaneEpoch || claims.ExpiresUnixMs != resource.ExpiresAt.UnixMilli() || view.Document().PolicyRevision != r.Definition.Document.Policy.Revision || view.Document().BundleArtifactRevision != r.Definition.BundleArtifactRevision {
		return activationSourceView{}, client.ErrResource
	}
	matched := false
	var selected client.ActivationSource
	for _, source := range view.Document().Sources {
		if source.InstanceID == se.Grant.InstanceID && source.BootID == se.Grant.BootID && source.LeaseID == intent.LeaseID && source.LeaseEpoch == intent.LeaseEpoch && source.CommandID == command && source.PrepareCommandID == prepare {
			matched = true
			selected = source
			break
		}
	}
	if !matched {
		return activationSourceView{}, client.ErrResource
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return activationSourceView{}, err
	}
	revocationEpoch, err := checkBundleRevocation(ctx, tx, scope, r.ControlPlaneEpoch, bundle, v)
	if err != nil {
		return activationSourceView{}, err
	}
	if view.Document().RevocationEpoch != revocationEpoch {
		return activationSourceView{}, protocolError(agentv1.ErrorCode_ERROR_CODE_RESYNC_REQUIRED, "activation revocation epoch changed")
	}
	return activationSourceView{Release: r, Resource: resource, View: view, Source: selected, Instance: instance, Now: now}, nil
}
