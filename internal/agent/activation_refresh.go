package agent

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// Caller holds the release barrier. Both heads must name the requested version,
// and the explicit source set must match the immutable first activation.
func lockActivationRefreshIntent(ctx context.Context, tx pgx.Tx, q StartActivationRequest, r ReleaseRecord, expected uint64, v *client.Verifier) error {
	scope := q.Scope
	var head, resource string
	err := tx.QueryRow(ctx, `SELECT a.resource_revision::text,h.revision::text FROM agent_activations a JOIN agent_resource_heads h ON h.workspace_id=a.workspace_id AND h.environment_id=a.environment_id AND h.service_key=a.service_key AND h.resource_key=a.resource_key WHERE a.workspace_id=$1 AND a.environment_id=$2 AND a.service_key=$3 AND a.generation=$4 FOR UPDATE OF a,h`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(q.Generation, 10)).Scan(&head, &resource)
	if err != nil {
		return err
	}
	if head != strconv.FormatUint(expected, 10) || resource != head {
		return protocolError(agentv1.ErrorCode_ERROR_CODE_STALE_REVISION, "activation artifact changed")
	}
	var frozen []string
	for _, revision := range []uint64{1, expected} {
		if revision == 1 && frozen != nil {
			continue
		}
		stored, err := loadResource(ctx, tx, scope, activationStorageKey(q.Generation), revision, false)
		if err != nil {
			return err
		}
		var signed agentv1.SignedResource
		if proto.Unmarshal(stored.Body, &signed) != nil {
			return client.ErrResource
		}
		claims, err := client.ParseClaims(signed.Claims)
		if err != nil {
			return err
		}
		view, verified, err := client.VerifyActivation(v, &signed, scope, q.Generation, hashFromRelease(r), client.Watermark{}, time.UnixMilli(claims.IssuedUnixMs))
		if err != nil {
			return err
		}
		d := view.Document()
		if verified.Revision != revision || verified.ControlPlaneEpoch != r.ControlPlaneEpoch || verified.ExpiresUnixMs != stored.ExpiresAt.UnixMilli() || d.PolicyRevision != r.Definition.Document.Policy.Revision || d.BundleArtifactRevision != r.Definition.BundleArtifactRevision {
			return client.ErrResource
		}
		ids := make([]string, 0, len(d.Sources))
		for _, source := range d.Sources {
			ids = append(ids, source.InstanceID)
		}
		if frozen == nil {
			frozen = ids
		} else if !slices.Equal(frozen, ids) {
			return client.ErrResource
		}
	}
	if !slices.Equal(frozen, q.RequiredSourceIDs) {
		return denied()
	}
	rows, err := tx.Query(ctx, `SELECT instance_id FROM agent_activation_sources WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND artifact_revision=1 ORDER BY instance_id LIMIT 100001`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(q.Generation, 10))
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) > client.MaxDiscoveryMembers || !slices.Equal(ids, q.RequiredSourceIDs) {
		return denied()
	}
	return nil
}
