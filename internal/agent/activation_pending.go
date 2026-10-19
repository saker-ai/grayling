package agent

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
)

type ActivationDispatch struct {
	Release  ReleaseRecord
	Resource StoredResource
	Document client.ActivationDocument
	Source   client.ActivationSource
}

// PendingActivation recovers only current unreported commands. A successful
// return is a bounded dispatch snapshot, never proof of delivery or installation.
func (s *Sessions) PendingActivation(ctx context.Context, se Session, v *client.Verifier) (*ActivationDispatch, error) {
	if v == nil {
		return nil, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, ConfigRead|DiscoveryRead|ReleaseReport); err != nil {
		return nil, err
	}
	scope := se.Grant.Scope
	var generation, artifact string
	err = tx.QueryRow(ctx, `SELECT h.generation::text,a.artifact_revision::text FROM agent_release_heads h JOIN agent_activations v USING(workspace_id,environment_id,service_key,generation) JOIN agent_activation_sources a USING(workspace_id,environment_id,service_key,generation) WHERE h.workspace_id=$1 AND h.environment_id=$2 AND h.service_key=$3 AND a.artifact_revision=v.resource_revision AND a.instance_id=$4 AND a.credential_id=$5 AND NOT EXISTS(SELECT 1 FROM agent_activation_reports r WHERE r.workspace_id=a.workspace_id AND r.environment_id=a.environment_id AND r.service_key=a.service_key AND r.generation=a.generation AND r.instance_id=a.instance_id AND r.artifact_revision=a.artifact_revision)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, se.Grant.InstanceID, se.Grant.ID).Scan(&generation, &artifact)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	g, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return nil, err
	}
	revision, err := strconv.ParseUint(artifact, 10, 64)
	if err != nil {
		return nil, err
	}
	locked, err := lockActivationSource(ctx, tx, se, g, revision, v, true)
	if err != nil {
		if detail, ok := errors.AsType[*agentv1.ProtocolError](err); ok && (detail.Code == agentv1.ErrorCode_ERROR_CODE_NOT_READY || detail.Code == agentv1.ErrorCode_ERROR_CODE_FENCED || detail.Code == agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED) {
			return nil, tx.Commit(ctx)
		}
		if errors.Is(err, client.ErrExpired) {
			return nil, tx.Commit(ctx)
		}
		return nil, err
	}
	// The head can advance while this dispatch waits for the release barrier.
	var current bool
	if err := tx.QueryRow(ctx, `SELECT resource_revision=$5::numeric FROM agent_activations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generation, artifact).Scan(&current); err != nil {
		return nil, err
	}
	if !current {
		return nil, tx.Commit(ctx)
	}
	// Report may have committed while waiting for the shared release barrier.
	var reported bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_activation_reports WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id=$5 AND artifact_revision=$6)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, generation, se.Grant.InstanceID, artifact).Scan(&reported); err != nil {
		return nil, err
	}
	if reported {
		return nil, tx.Commit(ctx)
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return nil, err
	}
	if err := preparationStillFresh(ctx, tx, locked.Now, locked.Resource.ExpiresAt); err != nil {
		return nil, err
	}
	return &ActivationDispatch{Release: locked.Release, Resource: locked.Resource, Document: locked.View.Document(), Source: locked.Source}, tx.Commit(ctx)
}
