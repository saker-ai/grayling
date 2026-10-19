package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

type PrepareDispatch struct {
	Intent  PrepareIntent
	Release ReleaseRecord
	Bundle  StoredResource
}

// PendingPrepare captures one current directed command for a live session.
// A dispatch snapshot is time-bounded, not proof of delivery or preparation;
// RecordPrepared repeats the barriers after any network/reconnect delay.
func (s *Sessions) PendingPrepare(ctx context.Context, se Session, v *client.Verifier) (*PrepareDispatch, error) {
	if v == nil {
		return nil, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|ReleaseReport); err != nil {
		return nil, err
	}
	scope := se.Grant.Scope
	state, err := readRegistrationScope(ctx, tx, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	var generationText string
	err = tx.QueryRow(ctx, `SELECT generation::text FROM agent_release_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&generationText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil {
		return nil, err
	}
	if generation == 0 {
		return nil, tx.Commit(ctx)
	}
	r, now, err := lockPrepareCommandRelease(ctx, tx, scope, generation)
	if err != nil {
		if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok && (detail.Code == agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED || detail.Code == agentv1.ErrorCode_ERROR_CODE_CONFLICT || detail.Code == agentv1.ErrorCode_ERROR_CODE_FENCED) {
			return nil, tx.Commit(ctx)
		}
		return nil, err
	}
	instance, err := readInstance(ctx, tx, scope, se.Grant.InstanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if !prepareInstanceValid(instance, state, r) || !instance.expiresAt.After(now) {
		return nil, tx.Commit(ctx)
	}
	var command, targetRevision string
	err = tx.QueryRow(ctx, `SELECT h.command_id,h.revision::text FROM agent_prepare_heads h WHERE h.workspace_id=$1 AND h.environment_id=$2 AND h.service_key=$3 AND h.generation=$4 AND h.instance_id=$5 AND NOT EXISTS(SELECT 1 FROM agent_prepared_evidence e WHERE e.workspace_id=h.workspace_id AND e.environment_id=h.environment_id AND e.service_key=h.service_key AND e.generation=h.generation AND e.command_id=h.command_id) AND NOT EXISTS(SELECT 1 FROM agent_prepare_rejections n WHERE n.workspace_id=h.workspace_id AND n.environment_id=h.environment_id AND n.service_key=h.service_key AND n.generation=h.generation AND n.command_id=h.command_id)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generationText, se.Grant.InstanceID).Scan(&command, &targetRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	intent, _, err := readPrepareIntent(ctx, tx, scope, command)
	if err != nil {
		return nil, err
	}
	if targetRevision != strconv.FormatUint(intent.TargetRevision, 10) {
		return nil, client.ErrResource
	}
	if !matchesPrepareTarget(intent, se, instance) || !intent.ExpiresAt.After(now) {
		return nil, tx.Commit(ctx)
	}
	if err := verifyPreparingResources(ctx, tx, r, v, now); err != nil {
		return nil, err
	}
	var hash [32]byte
	decoded, err := hex.DecodeString(r.Definition.BundleHash)
	if err != nil || len(decoded) != 32 {
		return nil, client.ErrResource
	}
	copy(hash[:], decoded)
	bundle, err := loadResource(ctx, tx, scope, bundleStorageKey(hash), r.Definition.BundleArtifactRevision, true)
	if err != nil {
		return nil, err
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return nil, err
	}
	if err := preparationStillFresh(ctx, tx, now, time.UnixMilli(min(intent.ExpiresAt.UnixMilli(), instance.expiresAt.UnixMilli(), se.Grant.ExpiresAt.UnixMilli()))); err != nil {
		return nil, err
	}
	return &PrepareDispatch{Intent: intent, Release: r, Bundle: bundle}, tx.Commit(ctx)
}
