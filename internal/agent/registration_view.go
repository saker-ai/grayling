package agent

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// Candidate is governance data, never proof that a build accepts a Bundle.
// Consumers must additionally require valid Prepared evidence and compatibility.
type Candidate struct {
	Instance        *agentv1.Instance
	NamingConfirmed bool
}
type CandidateSnapshot struct {
	Revision          uint64
	ControlPlaneEpoch uint64
	IssuedAt          time.Time
	ExpiresAt         time.Time
	Members           []Candidate
}

// NamingIntent is an immutable projection revision. Backend keys must include
// both Lease.ID and Revision: late writes and cleanup must not mutate a newer
// projection, including renewals on the same lease. Only exact readback confirms.
type NamingIntent struct {
	Scope    client.Scope
	Instance *agentv1.Instance
	Revision uint64
	Present  bool
}

// expireRegistrationScope removes new-route eligibility before Naming cleanup.
// The caller holds the independent registration scope row lock. Tombstones and
// epoch counters remain; no link heartbeat or backend health can resurrect them.
func expireRegistrationScope(ctx context.Context, tx pgx.Tx, scope client.Scope, state RegistrationPolicyState) (uint64, error) {
	rows, err := tx.Query(ctx, `SELECT i.instance_id,i.lease_id FROM agent_instances i JOIN agent_credentials c ON c.id=i.credential_id WHERE i.workspace_id=$1 AND i.environment_id=$2 AND i.service_key=$3 AND i.active AND (i.expires_at<=clock_timestamp() OR c.revoked OR c.expires_at<=clock_timestamp() OR i.policy_revision<>$4 OR NOT EXISTS(SELECT 1 FROM agent_sessions s WHERE s.workspace_id=i.workspace_id AND s.environment_id=i.environment_id AND s.service_key=i.service_key AND s.instance_id=i.instance_id AND s.boot_id=i.boot_id)) ORDER BY i.instance_id FOR UPDATE OF i`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(state.Revision, 10))
	if err != nil {
		return 0, err
	}
	var ids, leases []string
	for rows.Next() {
		var id, lease string
		if err := rows.Scan(&id, &lease); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		leases = append(leases, lease)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return state.DiscoveryRevision, nil
	}
	revision, err := advanceDiscovery(ctx, tx, scope, state.DiscoveryRevision)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_instances SET active=false,desired_revision=$5,confirmed_revision=0 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND instance_id=ANY($4::text[])`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, ids, strconv.FormatUint(revision, 10)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_naming_intents SET present=false,confirmed=false,next_attempt_at=clock_timestamp() WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND lease_id=ANY($4::text[])`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, leases); err != nil {
		return 0, err
	}
	return revision, nil
}

// DiscoveryCandidates captures a coherent scope revision and its full candidate
// set under one governance barrier. This is not the signed discovery transport.
func (s *Sessions) DiscoveryCandidates(ctx context.Context, se Session) (CandidateSnapshot, error) {
	return s.captureDiscovery(ctx, se, nil)
}

func (s *Sessions) captureDiscovery(ctx context.Context, se Session, finish func(pgx.Tx, CandidateSnapshot) error) (CandidateSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CandidateSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, DiscoveryRead); err != nil {
		return CandidateSnapshot{}, err
	}
	state, err := readRegistrationScope(ctx, tx, se.Grant.Scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return CandidateSnapshot{}, denied()
	}
	if err != nil {
		return CandidateSnapshot{}, err
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return CandidateSnapshot{}, err
	}
	revision, err := expireRegistrationScope(ctx, tx, se.Grant.Scope, state)
	if err != nil {
		return CandidateSnapshot{}, err
	}
	var updated time.Time
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_plane_epoch,clock_timestamp() FROM agent_schema WHERE id=true FOR SHARE`).Scan(&epoch, &updated); err != nil {
		return CandidateSnapshot{}, err
	}
	if epoch <= 0 {
		return CandidateSnapshot{}, client.ErrResource
	}
	snapshot := CandidateSnapshot{ControlPlaneEpoch: uint64(epoch), IssuedAt: updated, Revision: revision, ExpiresAt: updated.Add(instanceLeaseTTL), Members: make([]Candidate, 0)}
	rows, err := tx.Query(ctx, `SELECT descriptor,confirmed_revision=desired_revision,expires_at FROM agent_instances WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND active ORDER BY instance_id`, se.Grant.Scope.WorkspaceID, se.Grant.Scope.EnvironmentID, se.Grant.Scope.ServiceKey)
	if err != nil {
		return CandidateSnapshot{}, err
	}
	for rows.Next() {
		var body []byte
		var confirmed bool
		var expiry time.Time
		if err := rows.Scan(&body, &confirmed, &expiry); err != nil {
			rows.Close()
			return CandidateSnapshot{}, err
		}
		instance := new(agentv1.Instance)
		if err := proto.Unmarshal(body, instance); err != nil {
			rows.Close()
			return CandidateSnapshot{}, err
		}
		snapshot.Members = append(snapshot.Members, Candidate{Instance: instance, NamingConfirmed: confirmed})
		if expiry.Before(snapshot.ExpiresAt) {
			snapshot.ExpiresAt = expiry
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CandidateSnapshot{}, err
	}
	if se.Grant.ExpiresAt.Before(snapshot.ExpiresAt) {
		snapshot.ExpiresAt = se.Grant.ExpiresAt
	}
	snapshot.ExpiresAt = snapshot.ExpiresAt.Truncate(time.Millisecond)
	if finish != nil {
		if err := finish(tx, snapshot); err != nil {
			return CandidateSnapshot{}, err
		}
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return CandidateSnapshot{}, err
	}
	return snapshot, tx.Commit(ctx)
}

// ExpireInstances is called by the reconciler even when no reader is connected.
func (s *Sessions) ExpireInstances(ctx context.Context, scope client.Scope) (uint64, error) {
	if !scopeValid(scope) {
		return 0, denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	state, err := readRegistrationScope(ctx, tx, scope)
	if err != nil {
		return 0, err
	}
	revision, err := expireRegistrationScope(ctx, tx, scope, state)
	if err != nil {
		return 0, err
	}
	return revision, tx.Commit(ctx)
}

// PendingNamingIntents returns at most 100 unconfirmed writes and active
// refreshes, including confirmed tombstones to remove delayed stale writes. The adapter must also enforce ExpiresUnixMs at dispatch time.
func (s *Sessions) PendingNamingIntents(ctx context.Context, scope client.Scope) ([]NamingIntent, error) {
	if !scopeValid(scope) {
		return nil, denied()
	}
	rows, err := s.pool.Query(ctx, `WITH due AS (SELECT lease_id,revision FROM agent_naming_intents WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,revision,lease_id LIMIT 100 FOR UPDATE SKIP LOCKED) UPDATE agent_naming_intents i SET next_attempt_at=clock_timestamp()+interval '5 seconds' FROM due WHERE i.workspace_id=$1 AND i.environment_id=$2 AND i.service_key=$3 AND i.lease_id=due.lease_id AND i.revision=due.revision RETURNING i.descriptor,i.revision::text,i.present AND i.expires_at>clock_timestamp()`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]NamingIntent, 0)
	for rows.Next() {
		var body []byte
		var revision string
		var present bool
		if err := rows.Scan(&body, &revision, &present); err != nil {
			return nil, err
		}
		instance := new(agentv1.Instance)
		if err := proto.Unmarshal(body, instance); err != nil {
			return nil, err
		}
		value, err := strconv.ParseUint(revision, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, NamingIntent{Scope: scope, Instance: instance, Revision: value, Present: present})
	}
	return result, rows.Err()
}

// ConfirmNamingIntent is internal control-plane code. Call only after a backend
// readback verified the exact descriptor/lease/revision, or confirmed absence.
// A stale acknowledgement cannot publish or extend the successor incarnation.
func (s *Sessions) ConfirmNamingIntent(ctx context.Context, intent NamingIntent) error {
	if !validInstanceDescriptor(intent.Instance, intent.Scope) || intent.Instance.Lease == nil || intent.Revision == 0 {
		return denied()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	state, err := readRegistrationScope(ctx, tx, intent.Scope)
	if err != nil {
		return err
	}
	var body []byte
	var revision string
	var present, confirmed, alive bool
	err = tx.QueryRow(ctx, `SELECT descriptor,revision::text,present,confirmed,expires_at>clock_timestamp() FROM agent_naming_intents WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND lease_id=$4 AND revision=$5 FOR UPDATE`, intent.Scope.WorkspaceID, intent.Scope.EnvironmentID, intent.Scope.ServiceKey, intent.Instance.Lease.Id, strconv.FormatUint(intent.Revision, 10)).Scan(&body, &revision, &present, &confirmed, &alive)
	if errors.Is(err, pgx.ErrNoRows) {
		return fenced()
	}
	if err != nil {
		return err
	}
	desired := new(agentv1.Instance)
	if err := proto.Unmarshal(body, desired); err != nil {
		return err
	}
	if !proto.Equal(desired, intent.Instance) || revision != strconv.FormatUint(intent.Revision, 10) || present != intent.Present || present && !alive {
		return fenced()
	}
	if confirmed {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_naming_intents SET confirmed=true WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND lease_id=$4 AND revision=$5`, intent.Scope.WorkspaceID, intent.Scope.EnvironmentID, intent.Scope.ServiceKey, intent.Instance.Lease.Id, strconv.FormatUint(intent.Revision, 10)); err != nil {
		return err
	}
	if present {
		record, err := readInstance(ctx, tx, intent.Scope, intent.Instance.InstanceId)
		if err != nil {
			return err
		}
		if record == nil || !record.active || record.instance.Lease.Id != intent.Instance.Lease.Id || record.desiredRevision != intent.Revision {
			return fenced()
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT NOT c.revoked AND c.expires_at>clock_timestamp() AND i.expires_at>clock_timestamp() AND i.policy_revision=$5 AND EXISTS(SELECT 1 FROM agent_sessions s WHERE s.workspace_id=i.workspace_id AND s.environment_id=i.environment_id AND s.service_key=i.service_key AND s.instance_id=i.instance_id AND s.boot_id=i.boot_id) FROM agent_instances i JOIN agent_credentials c ON c.id=i.credential_id WHERE i.workspace_id=$1 AND i.environment_id=$2 AND i.service_key=$3 AND i.instance_id=$4`, intent.Scope.WorkspaceID, intent.Scope.EnvironmentID, intent.Scope.ServiceKey, intent.Instance.InstanceId, strconv.FormatUint(state.Revision, 10)).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return fenced()
		}
		if _, err := advanceDiscovery(ctx, tx, intent.Scope, state.DiscoveryRevision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_instances SET confirmed_revision=desired_revision WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND instance_id=$4 AND lease_id=$5`, intent.Scope.WorkspaceID, intent.Scope.EnvironmentID, intent.Scope.ServiceKey, intent.Instance.InstanceId, intent.Instance.Lease.Id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
