package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

type preparationTarget struct{ instance, command, revision string }
type preparationObservation struct {
	Command   string    `json:"command"`
	Revision  uint64    `json:"revision,string"`
	Outcome   string    `json:"outcome"`
	Evidence  uint64    `json:"evidence,string"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func evaluatePreparationLocked(ctx context.Context, tx pgx.Tx, r ReleaseRecord, state RegistrationPolicyState, credentials map[string]preparationCredential, now time.Time) (PreparationEvaluation, error) {
	return evaluatePreparationSnapshotLocked(ctx, tx, r, state, credentials, now, nil)
}
func evaluatePreparationSnapshotLocked(ctx context.Context, tx pgx.Tx, r ReleaseRecord, state RegistrationPolicyState, credentials map[string]preparationCredential, now time.Time, members *[]client.ActivationMember) (PreparationEvaluation, error) {
	scope := r.Definition.Scope
	g := strconv.FormatUint(r.Generation, 10)
	result := PreparationEvaluation{Release: r, EvaluatedAt: now.Truncate(time.Millisecond).UTC(), ValidUntil: r.PrepareDeadline, Cells: []PreparedCapacityCell{}}
	if r.State == agentv1.ReleaseState_RELEASE_STATE_ACTIVATING || r.State == agentv1.ReleaseState_RELEASE_STATE_VERIFYING || r.State == agentv1.ReleaseState_RELEASE_STATE_ACTIVE {
		result.ValidUntil = r.ResourcesExpiresAt
	}
	rows, err := tx.Query(ctx, `SELECT instance_id,command_id,revision::text FROM agent_prepare_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 ORDER BY instance_id LIMIT 100001`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	targets := []preparationTarget{}
	for rows.Next() {
		var target preparationTarget
		if err := rows.Scan(&target.instance, &target.command, &target.revision); err != nil {
			return result, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	rows.Close()
	if len(targets) > 100000 {
		return result, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "preparation target bound exceeded")
	}
	var catalog string
	err = tx.QueryRow(ctx, `SELECT revision::text FROM agent_prepared_heads WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, g).Scan(&catalog)
	if err == nil {
		result.EvidenceRevision, err = strconv.ParseUint(catalog, 10, 64)
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return result, err
	}
	type cell struct{ build, zone string }
	counts := map[cell]uint64{}
	observations := make([]preparationObservation, 0, len(targets))
	for _, target := range targets {
		intent, _, err := readPrepareIntent(ctx, tx, scope, target.command)
		if err != nil {
			return result, err
		}
		if intent.Generation != r.Generation || intent.Instance.InstanceId != target.instance || target.revision != strconv.FormatUint(intent.TargetRevision, 10) {
			return result, client.ErrResource
		}
		credential, locked := credentials[intent.CredentialID]
		if !locked {
			return result, preparationTargetRace()
		}
		instance, err := readInstance(ctx, tx, scope, target.instance)
		if err != nil {
			return result, err
		}
		outcome, err := readPreparationOutcome(ctx, tx, r, intent, result.EvidenceRevision, now)
		if err != nil {
			return result, err
		}
		obs := preparationObservation{Command: intent.CommandID, Revision: intent.TargetRevision, Outcome: "pending", Evidence: outcome.revision, ExpiresAt: outcome.expires}
		// Admission SHARE prevents identity changes by Connect, without taking session
		// locks after registration scope (which would invert the report lock order).
		var owner bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND instance_id=$4 AND credential_id=$5 AND agent_id=$6 AND boot_id=$7)`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, target.instance, intent.CredentialID, intent.AgentID, intent.Instance.BootId).Scan(&owner)
		if err != nil {
			return result, err
		}
		se := Session{Grant: Grant{ID: intent.CredentialID, PrincipalID: intent.TargetPrincipalID, AgentID: intent.AgentID, InstanceID: target.instance, BootID: intent.Instance.BootId}}
		live := owner && credential.allowed && credential.expires.After(now) && matchesPrepareTarget(intent, se, instance) && prepareInstanceValid(instance, state, r) && instance.expiresAt.After(now)
		switch {
		case outcome.negative || !live || outcome.revision == 0 && !intent.ExpiresAt.After(now) || (outcome.revision > 0 && !outcome.expires.After(now)):
			result.Failed++
			obs.Outcome = "failed"
		case outcome.revision == 0:
			result.Pending++
		default:
			result.Prepared++
			obs.Outcome = "prepared"
			counts[cell{intent.Instance.BuildId, intent.Instance.Zone}]++
			if members != nil {
				i := instance.instance
				*members = append(*members, client.ActivationMember{Instance: client.DiscoveryMember{InstanceID: i.InstanceId, BootID: i.BootId, BuildID: i.BuildId, ImageDigest: i.ImageDigest, Address: i.Address, Port: i.Port, TLSServerName: i.TlsServerName, Zone: i.Zone, ProtocolVersion: i.ProtocolVersion, Ready: i.Ready, Draining: i.Draining, Lease: client.DiscoveryLease{ID: i.Lease.Id, Epoch: i.Lease.Epoch, ExpiresUnixMs: i.Lease.ExpiresUnixMs}}, PrepareCommandID: intent.CommandID, EvidenceRevision: outcome.revision, PreparedUnixMs: outcome.preparedAt, ExpiresUnixMs: min(outcome.expires.UnixMilli(), instance.expiresAt.UnixMilli())})
			}

			for _, expiry := range []time.Time{outcome.expires, instance.expiresAt, credential.expires} {
				if expiry.Before(result.ValidUntil) {
					result.ValidUntil = expiry
				}
			}
		}
		result.Targets++
		observations = append(observations, obs)
	}
	body, err := json.Marshal(observations, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	hash := sha256.Sum256(body)
	result.TargetDigest = hex.EncodeToString(hash[:])
	// Worst-case bound includes pending targets. Adding unreported targets cannot
	// dilute known failures. Target count is bounded, so integer products fit uint64.
	result.Eligible = result.Targets > 0 && (result.Failed+result.Pending)*10000 <= result.Targets*uint64(r.Definition.Capacity.MaximumFailureBasisPoints)
	for _, build := range r.Definition.Document.Builds {
		for _, zone := range r.Definition.Capacity.RequiredZones {
			count := counts[cell{build.ID, zone}]
			result.Cells = append(result.Cells, PreparedCapacityCell{BuildID: build.ID, Zone: zone, Prepared: count, Required: r.Definition.Capacity.MinimumPerBuildZone})
			if count < uint64(r.Definition.Capacity.MinimumPerBuildZone) {
				result.Eligible = false
			}
		}
	}
	return result, nil
}

type preparationOutcome struct {
	revision   uint64
	preparedAt int64
	negative   bool
	expires    time.Time
}

func readPreparationOutcome(ctx context.Context, tx pgx.Tx, r ReleaseRecord, intent PrepareIntent, catalog uint64, now time.Time) (preparationOutcome, error) {
	scope := r.Definition.Scope
	rows, err := tx.Query(ctx, `SELECT false,revision::text,report_hash,report,received_at,expires_at FROM agent_prepared_evidence WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5 UNION ALL SELECT true,revision::text,report_hash,report,received_at,expires_at FROM agent_prepare_rejections WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND generation=$4 AND command_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(r.Generation, 10), intent.CommandID)
	if err != nil {
		return preparationOutcome{}, err
	}
	defer rows.Close()
	var outcome preparationOutcome
	found := false
	for rows.Next() {
		if found {
			return outcome, client.ErrResource
		}
		found = true
		var text string
		var hash, body []byte
		var received time.Time
		if err := rows.Scan(&outcome.negative, &text, &hash, &body, &received, &outcome.expires); err != nil {
			return outcome, err
		}
		outcome.revision, err = strconv.ParseUint(text, 10, 64)
		if err != nil || outcome.revision == 0 || outcome.revision > catalog || !outcome.expires.After(received) || received.Before(intent.CreatedAt) || received.After(now.Add(5*time.Second)) || !received.Before(intent.ExpiresAt) || outcome.expires.After(r.ResourcesExpiresAt) {
			return outcome, client.ErrResource
		}
		var identity *agentv1.ReleaseIdentity
		domain := "grayling.agent.v1.prepared-report\x00"
		if outcome.negative {
			domain = "grayling.agent.v1.prepare-rejected\x00"
			report := new(agentv1.Rejected)
			if proto.Unmarshal(body, report) != nil || report.Error == nil || report.Error.Code == 0 || outcome.expires.After(intent.ExpiresAt) {
				return outcome, client.ErrResource
			}
			identity = report.Release
		} else {
			report := new(agentv1.Prepared)
			if proto.Unmarshal(body, report) != nil || report.InstanceId != intent.Instance.InstanceId || report.BootId != intent.Instance.BootId || report.BuildId != intent.Instance.BuildId || report.AcceptedProtocolVersion != intent.Instance.ProtocolVersion || report.AppliedGeneration > r.Generation || report.PreparedUnixMs <= 0 || report.ExpiresUnixMs <= report.PreparedUnixMs || report.ExpiresUnixMs-report.PreparedUnixMs > releasePreparedTTL.Milliseconds() || report.PreparedUnixMs > received.Add(5*time.Second).UnixMilli() || report.PreparedUnixMs < intent.CreatedAt.Add(-5*time.Second).UnixMilli() || outcome.expires.UnixMilli() > report.ExpiresUnixMs || outcome.expires.After(received.Add(releasePreparedTTL)) {
				return outcome, client.ErrResource
			}
			identity = report.Release
			outcome.preparedAt = report.PreparedUnixMs
		}
		digest := sha256.Sum256(append([]byte(domain), body...))
		if !bytes.Equal(digest[:], hash) || identity == nil || identity.ReleaseId != r.ReleaseID || identity.Generation != r.Generation || identity.BundleHash != r.Definition.BundleHash {
			return outcome, client.ErrResource
		}
		outcome.expires = outcome.expires.UTC()
	}
	return outcome, rows.Err()
}
