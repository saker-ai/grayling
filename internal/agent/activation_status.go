package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// ActivationStatus is paginated diagnostic history for an authenticated source,
// not a freshness/capacity/routing authorization check. Expired views remain
// queryable without accepting new reports or extending any authority.
func (s *Sessions) ActivationStatus(ctx context.Context, se Session, q *agentv1.ReleaseStatusQuery) (*agentv1.ReleaseStatus, error) {
	if q == nil || q.OperationId != "" || q.AfterInstanceId != "" && (q.ExpectedReportRevision == nil || q.ExpectedReleaseRevision == nil || q.ExpectedActivationArtifactRevision == nil) || q.PageSize > 128 || q.AfterInstanceId != "" && !identifier(q.AfterInstanceId) {
		return nil, denied()
	}
	generation, err := strconv.ParseUint(strings.TrimPrefix(q.ReleaseId, "release-"), 10, 64)
	if err != nil || generation == 0 || q.ReleaseId != releaseID(generation) {
		return nil, denied()
	}
	limit := q.PageSize
	if limit == 0 {
		limit = 64
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
	g := strconv.FormatUint(generation, 10)
	var authorized bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_activation_sources a JOIN agent_prepare_intents i ON i.workspace_id=a.workspace_id AND i.environment_id=a.environment_id AND i.service_key=a.service_key AND i.generation=a.generation AND i.command_id=a.prepare_command_id WHERE a.workspace_id=$1 AND a.environment_id=$2 AND a.service_key=$3 AND a.generation=$4 AND a.instance_id=$5 AND a.credential_id=$6 AND i.boot_id=$7)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, se.Grant.InstanceID, se.Grant.ID, se.Grant.BootID).Scan(&authorized)
	if err != nil {
		return nil, err
	}
	if !authorized {
		return nil, denied()
	}
	if _, err := readRegistrationScope(ctx, tx, scope); err != nil {
		return nil, err
	}
	r, err := readRelease(ctx, tx, scope, generation, true)
	if err != nil {
		return nil, err
	}
	var artifactText string
	if err := tx.QueryRow(ctx, `SELECT resource_revision::text FROM agent_activations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&artifactText); err != nil {
		return nil, err
	}
	artifact, err := strconv.ParseUint(artifactText, 10, 64)
	if err != nil || artifact == 0 {
		return nil, client.ErrResource
	}
	result := &agentv1.ReleaseStatus{Release: &agentv1.ReleaseIdentity{ReleaseId: r.ReleaseID, Generation: r.Generation, BundleHash: r.Definition.BundleHash}, State: r.State, ReleaseRevision: r.Revision, ActivationArtifactRevision: artifact}
	var revision string
	err = tx.QueryRow(ctx, `SELECT revision::text FROM agent_activation_report_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&revision)
	if err == nil {
		result.ReportRevision, err = strconv.ParseUint(revision, 10, 64)
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if q.ExpectedActivationArtifactRevision != nil && *q.ExpectedActivationArtifactRevision != artifact || q.ExpectedReportRevision != nil && *q.ExpectedReportRevision != result.ReportRevision || q.ExpectedReleaseRevision != nil && *q.ExpectedReleaseRevision != result.ReleaseRevision {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_RESYNC_REQUIRED, "activation status changed; restart pagination")
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_activation_sources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND artifact_revision=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, artifactText).Scan(&result.RequiredSources); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT instance_id FROM agent_activation_sources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND instance_id>$5 AND artifact_revision=$7 ORDER BY instance_id LIMIT $6`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g, q.AfterInstanceId, int(limit)+1, artifactText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(ids) > int(limit) {
		result.More = true
		ids = ids[:limit]
		result.NextInstanceId = ids[len(ids)-1]
	}
	for _, id := range ids {
		record, err := readActivationReport(ctx, tx, scope, generation, id, artifact)
		if errors.Is(err, pgx.ErrNoRows) {
			result.PendingSourceIds = append(result.PendingSourceIds, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		if record.ack.ReportRevision > result.ReportRevision || record.ack.Release.ReleaseId != result.Release.ReleaseId || record.ack.Release.Generation != result.Release.Generation || record.ack.Release.BundleHash != result.Release.BundleHash {
			return nil, client.ErrResource
		}
		// Reports retain unknown fields for exact replay; status exposes only bounded
		// known schema fields so optional padding cannot make a page exceed a frame.
		switch record.report.(type) {
		case *agentv1.Applied:
			out := new(agentv1.Applied)
			if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(record.body, out); err != nil {
				return nil, err
			}
			result.Applied = append(result.Applied, out)
		case *agentv1.Rejected:
			out := new(agentv1.Rejected)
			if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(record.body, out); err != nil {
				return nil, err
			}
			result.Rejected = append(result.Rejected, out)
		default:
			return nil, client.ErrResource
		}
	}
	if proto.Size(result) > agentv1.MaxFrameBytes-1024 {
		return nil, client.ErrResource
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}
