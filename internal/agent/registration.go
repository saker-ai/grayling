package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

const instanceLeaseTTL = 60 * time.Second

type instanceRecord struct {
	instance                                           *agentv1.Instance
	principalID, agentID, deploymentID, credentialID   string
	active                                             bool
	policyRevision, desiredRevision, confirmedRevision uint64
	expiresAt                                          time.Time
}

func readInstance(ctx context.Context, tx pgx.Tx, scope client.Scope, id string) (*instanceRecord, error) {
	var record instanceRecord
	var body []byte
	var epoch, desired, confirmed, lease, policy string
	err := tx.QueryRow(ctx, `SELECT descriptor,principal_id,agent_id,deployment_id,credential_id,active,lease_id,lease_epoch::text,expires_at,desired_revision::text,confirmed_revision::text,policy_revision::text FROM agent_instances WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND instance_id=$4 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, id).Scan(&body, &record.principalID, &record.agentID, &record.deploymentID, &record.credentialID, &record.active, &lease, &epoch, &record.expiresAt, &desired, &confirmed, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.instance = new(agentv1.Instance)
	if err := proto.Unmarshal(body, record.instance); err != nil {
		return nil, err
	}
	parsed, err := strconv.ParseUint(epoch, 10, 64)
	if err != nil {
		return nil, err
	}
	if record.instance.Lease == nil || !identifier(record.instance.Lease.Id) || record.instance.Lease.Epoch == 0 || record.instance.Lease.ExpiresUnixMs <= 0 || record.instance.InstanceId != id || record.instance.Lease.Id != lease || record.instance.Lease.Epoch != parsed || record.instance.Lease.ExpiresUnixMs != record.expiresAt.UnixMilli() || !validInstanceDescriptor(record.instance, scope) {
		return nil, client.ErrResource
	}
	record.policyRevision, err = strconv.ParseUint(policy, 10, 64)
	if err != nil {
		return nil, err
	}
	record.desiredRevision, err = strconv.ParseUint(desired, 10, 64)
	if err != nil {
		return nil, err
	}
	record.confirmedRevision, err = strconv.ParseUint(confirmed, 10, 64)
	return &record, err
}
func writeInstance(ctx context.Context, tx pgx.Tx, se Session, record *instanceRecord, policyRevision, revision uint64) error {
	instance := record.instance
	body, err := proto.Marshal(instance)
	if err != nil {
		return err
	}
	scope := se.Grant.Scope
	_, err = tx.Exec(ctx, `INSERT INTO agent_instances(workspace_id,environment_id,service_key,instance_id,boot_id,principal_id,agent_id,deployment_id,credential_id,descriptor,lease_id,lease_epoch,expires_at,active,policy_revision,desired_revision,confirmed_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,0) ON CONFLICT(workspace_id,environment_id,service_key,instance_id) DO UPDATE SET boot_id=EXCLUDED.boot_id,principal_id=EXCLUDED.principal_id,agent_id=EXCLUDED.agent_id,deployment_id=EXCLUDED.deployment_id,credential_id=EXCLUDED.credential_id,descriptor=EXCLUDED.descriptor,lease_id=EXCLUDED.lease_id,lease_epoch=EXCLUDED.lease_epoch,expires_at=EXCLUDED.expires_at,active=EXCLUDED.active,policy_revision=EXCLUDED.policy_revision,desired_revision=EXCLUDED.desired_revision,confirmed_revision=0`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, instance.InstanceId, instance.BootId, se.Grant.PrincipalID, se.Grant.AgentID, record.deploymentID, se.Grant.ID, body, instance.Lease.Id, strconv.FormatUint(instance.Lease.Epoch, 10), record.expiresAt, record.active, strconv.FormatUint(policyRevision, 10), strconv.FormatUint(revision, 10))
	if err != nil {
		return err
	}
	// Retain immutable projection revisions for cleanup of delayed old writes.
	if _, err := tx.Exec(ctx, `UPDATE agent_naming_intents SET present=false,confirmed=false,next_attempt_at=clock_timestamp() WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND lease_id=$4 AND present`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, instance.Lease.Id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_naming_intents(workspace_id,environment_id,service_key,instance_id,lease_id,lease_epoch,revision,descriptor,present,expires_at,confirmed) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false) ON CONFLICT(workspace_id,environment_id,service_key,lease_id,revision) DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, instance.InstanceId, instance.Lease.Id, strconv.FormatUint(instance.Lease.Epoch, 10), strconv.FormatUint(revision, 10), body, record.active, record.expiresAt)
	return err
}
func lockedSessionStillValid(ctx context.Context, tx pgx.Tx, se Session) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions s JOIN agent_credentials c ON c.id=s.credential_id WHERE s.session_id=$1 AND s.connection_epoch=$2 AND c.id=$3 AND NOT c.revoked AND c.expires_at>clock_timestamp() AND s.expires_at>clock_timestamp())`, se.ID, int64(se.Epoch), se.Grant.ID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fenced()
	}
	return nil
}
func matchesLease(se Session, record *instanceRecord, instanceID, bootID string, lease *agentv1.Lease) bool {
	return record != nil && instanceID == se.Grant.InstanceID && bootID == se.Grant.BootID && record.instance.BootId == bootID && record.principalID == se.Grant.PrincipalID && record.agentID == se.Grant.AgentID && lease != nil && lease.Id == record.instance.Lease.Id && lease.Epoch == record.instance.Lease.Epoch
}

// mutateInstance serializes one scope without taking legacy admission locks.
// Lock order: credential -> session -> registration scope -> instance -> operation.
// Connect never takes registration scope/instance locks, so takeover cannot form
// a session/admission lock inversion. Naming calls must remain outside this tx.
func (s *Sessions) mutateInstance(ctx context.Context, se Session, operation, action string, permission Permission, input proto.Message, lease *agentv1.Lease, apply func(pgx.Tx, RegistrationPolicyState, *instanceRecord, time.Time) (*agentv1.Envelope, error)) (*agentv1.Envelope, error) {
	if !identifier(operation) || input == nil || proto.Size(input) > 32<<10 {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid instance operation")
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(input)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockSession(ctx, tx, se, permission); err != nil {
		return nil, err
	}
	scope := se.Grant.Scope
	state, err := readRegistrationScope(ctx, tx, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, denied()
	}
	if err != nil {
		return nil, err
	}
	if state.Revision == 0 {
		return nil, denied()
	}
	record, err := readInstance(ctx, tx, scope, se.Grant.InstanceID)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if lease != nil {
		if !matchesLease(se, record, se.Grant.InstanceID, se.Grant.BootID, lease) {
			return nil, fenced()
		}
		if action != "deregister" && (!record.active || !record.expiresAt.After(now)) {
			return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED, "instance lease expired")
		}
	}
	if err := lockedSessionStillValid(ctx, tx, se); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_instance_operations(workspace_id,environment_id,service_key,principal_id,operation_id,action,request_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, se.Grant.PrincipalID, operation, action, digest[:]); err != nil {
		return nil, err
	}
	var savedAction string
	var savedHash, result []byte
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT action,request_hash,result,expires_at<=clock_timestamp() FROM agent_instance_operations WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, se.Grant.PrincipalID, operation).Scan(&savedAction, &savedHash, &result, &expired); err != nil {
		return nil, err
	}
	if savedAction != action || !bytes.Equal(savedHash, digest[:]) || expired {
		return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "instance operation changed or expired")
	}
	if len(result) > 0 {
		reply := new(agentv1.Envelope)
		if err := proto.Unmarshal(result, reply); err != nil {
			return nil, err
		}
		return reply, tx.Commit(ctx)
	}
	reply, err := apply(tx, state, record, now)
	if err != nil {
		return nil, err
	}
	encoded, err := proto.Marshal(reply)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_instance_operations SET result=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND principal_id=$4 AND operation_id=$5`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, se.Grant.PrincipalID, operation, encoded); err != nil {
		return nil, err
	}
	return reply, tx.Commit(ctx)
}

func (s *Sessions) RegisterInstance(ctx context.Context, se Session, operation string, request *agentv1.Register) (*agentv1.Registered, error) {
	scope := se.Grant.Scope
	if request == nil || !validInstanceDescriptor(request.Instance, scope) || request.Instance.Lease != nil || request.Instance.InstanceId != se.Grant.InstanceID || request.Instance.BootId != se.Grant.BootID || !identifier(string(request.DeploymentAuthorization)) {
		return nil, denied()
	}
	request = proto.Clone(request).(*agentv1.Register)
	reply, err := s.mutateInstance(ctx, se, operation, "register", InstanceRegister, request, nil, func(tx pgx.Tx, state RegistrationPolicyState, old *instanceRecord, now time.Time) (*agentv1.Envelope, error) {
		var approved []byte
		var expiresAt time.Time
		err := tx.QueryRow(ctx, `SELECT descriptor,expires_at FROM agent_deployments WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND id=$4 AND principal_id=$5 AND agent_id=$6 AND instance_id=$7 AND boot_id=$8 AND policy_revision=$9 AND expires_at>clock_timestamp() FOR SHARE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, string(request.DeploymentAuthorization), se.Grant.PrincipalID, se.Grant.AgentID, se.Grant.InstanceID, se.Grant.BootID, strconv.FormatUint(state.Revision, 10)).Scan(&approved, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, denied()
		}
		if err != nil {
			return nil, err
		}
		authorized := new(agentv1.Instance)
		if err := proto.Unmarshal(approved, authorized); err != nil {
			return nil, err
		}
		if !proto.Equal(descriptorIdentity(request.Instance), authorized) || !state.Policy.allows(request.Instance.Address) {
			return nil, denied()
		}
		epoch := uint64(1)
		if old != nil {
			if old.instance.Lease.Epoch == math.MaxUint64 {
				return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "lease epoch exhausted")
			}
			epoch = old.instance.Lease.Epoch + 1
		} else {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_instances WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&count); err != nil {
				return nil, err
			}
			if count >= state.Policy.MaxInstances {
				return nil, protocolError(agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED, "instance identity quota reached")
			}
		}
		leaseID, err := randomID()
		if err != nil {
			return nil, err
		}
		revision, err := advanceDiscovery(ctx, tx, scope, state.DiscoveryRevision)
		if err != nil {
			return nil, err
		}
		if old != nil {
			if _, err := tx.Exec(ctx, `UPDATE agent_naming_intents SET present=false,confirmed=false,next_attempt_at=clock_timestamp() WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND lease_id=$4`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, old.instance.Lease.Id); err != nil {
				return nil, err
			}
		}
		deadline := now.Add(instanceLeaseTTL)
		for _, limit := range []time.Time{expiresAt, se.Grant.ExpiresAt} {
			if limit.Before(deadline) {
				deadline = limit
			}
		}
		deadline = deadline.Truncate(time.Millisecond)
		instance := proto.Clone(request.Instance).(*agentv1.Instance)
		instance.Lease = &agentv1.Lease{Id: leaseID, Epoch: epoch, ExpiresUnixMs: deadline.UnixMilli()}
		record := &instanceRecord{instance: instance, active: true, expiresAt: deadline, deploymentID: string(request.DeploymentAuthorization)}
		if err := writeInstance(ctx, tx, se, record, state.Revision, revision); err != nil {
			return nil, err
		}
		return &agentv1.Envelope{ResourceRevision: revision, Payload: &agentv1.Envelope_Registered{Registered: &agentv1.Registered{Instance: instance, DiscoveryRevision: revision}}}, nil
	})
	if err != nil {
		return nil, err
	}
	return reply.GetRegistered(), nil
}
func (s *Sessions) RenewInstanceLease(ctx context.Context, se Session, operation string, request *agentv1.RenewLease) (*agentv1.LeaseRenewed, error) {
	if request == nil || request.InstanceId != se.Grant.InstanceID || request.BootId != se.Grant.BootID || request.Lease == nil {
		return nil, denied()
	}
	request = proto.Clone(request).(*agentv1.RenewLease)
	reply, err := s.mutateInstance(ctx, se, operation, "renew", InstanceRenew, request, request.Lease, func(tx pgx.Tx, state RegistrationPolicyState, record *instanceRecord, now time.Time) (*agentv1.Envelope, error) {
		var deploymentExpiry time.Time
		if err := tx.QueryRow(ctx, `SELECT expires_at FROM agent_deployments WHERE id=$1 AND policy_revision=$2 AND workspace_id=$3 AND environment_id=$4 AND service_key=$5 AND principal_id=$6 AND agent_id=$7 AND instance_id=$8 AND boot_id=$9 AND expires_at>clock_timestamp()`, record.deploymentID, strconv.FormatUint(state.Revision, 10), se.Grant.Scope.WorkspaceID, se.Grant.Scope.EnvironmentID, se.Grant.Scope.ServiceKey, se.Grant.PrincipalID, se.Grant.AgentID, se.Grant.InstanceID, se.Grant.BootID).Scan(&deploymentExpiry); errors.Is(err, pgx.ErrNoRows) {
			return nil, denied()
		} else if err != nil {
			return nil, err
		}
		expiry := now.Add(instanceLeaseTTL)
		for _, limit := range []time.Time{deploymentExpiry, se.Grant.ExpiresAt} {
			if limit.Before(expiry) {
				expiry = limit
			}
		}
		expiry = expiry.Truncate(time.Millisecond)
		record.expiresAt = expiry
		record.instance.Lease.ExpiresUnixMs = expiry.UnixMilli()
		revision, err := advanceDiscovery(ctx, tx, se.Grant.Scope, state.DiscoveryRevision)
		if err != nil {
			return nil, err
		}
		if err := writeInstance(ctx, tx, se, record, state.Revision, revision); err != nil {
			return nil, err
		}
		return &agentv1.Envelope{ResourceRevision: revision, Payload: &agentv1.Envelope_LeaseRenewed{LeaseRenewed: &agentv1.LeaseRenewed{Lease: record.instance.Lease, DiscoveryRevision: revision}}}, nil
	})
	if err != nil {
		return nil, err
	}
	return reply.GetLeaseRenewed(), nil
}
func (s *Sessions) UpdateRegisteredInstance(ctx context.Context, se Session, operation string, request *agentv1.UpdateInstance) (*agentv1.InstanceChanged, error) {
	if request == nil || !validInstanceDescriptor(request.Instance, se.Grant.Scope) || request.Instance.InstanceId != se.Grant.InstanceID || request.Instance.BootId != se.Grant.BootID || request.Instance.Lease == nil {
		return nil, denied()
	}
	request = proto.Clone(request).(*agentv1.UpdateInstance)
	reply, err := s.mutateInstance(ctx, se, operation, "update", InstanceRegister, request, request.Instance.Lease, func(tx pgx.Tx, state RegistrationPolicyState, record *instanceRecord, _ time.Time) (*agentv1.Envelope, error) {
		if !proto.Equal(descriptorIdentity(request.Instance), descriptorIdentity(record.instance)) {
			return nil, denied()
		}
		record.instance.Ready = request.Instance.Ready
		record.instance.Draining = request.Instance.Draining
		revision, err := advanceDiscovery(ctx, tx, se.Grant.Scope, state.DiscoveryRevision)
		if err != nil {
			return nil, err
		}
		if err := writeInstance(ctx, tx, se, record, state.Revision, revision); err != nil {
			return nil, err
		}
		return &agentv1.Envelope{ResourceRevision: revision, Payload: &agentv1.Envelope_InstanceChanged{InstanceChanged: &agentv1.InstanceChanged{Instance: record.instance}}}, nil
	})
	if err != nil {
		return nil, err
	}
	return reply.GetInstanceChanged(), nil
}
func (s *Sessions) DeregisterInstance(ctx context.Context, se Session, operation string, request *agentv1.Deregister) (*agentv1.InstanceChanged, error) {
	if request == nil || request.InstanceId != se.Grant.InstanceID || request.BootId != se.Grant.BootID || request.Lease == nil {
		return nil, denied()
	}
	request = proto.Clone(request).(*agentv1.Deregister)
	reply, err := s.mutateInstance(ctx, se, operation, "deregister", InstanceRegister, request, request.Lease, func(tx pgx.Tx, state RegistrationPolicyState, record *instanceRecord, now time.Time) (*agentv1.Envelope, error) {
		if !record.active {
			return nil, fenced()
		}
		record.active = false
		record.instance.Ready = false
		record.instance.Draining = true
		record.expiresAt = now.Truncate(time.Millisecond)
		record.instance.Lease.ExpiresUnixMs = record.expiresAt.UnixMilli()
		revision, err := advanceDiscovery(ctx, tx, se.Grant.Scope, state.DiscoveryRevision)
		if err != nil {
			return nil, err
		}
		if err := writeInstance(ctx, tx, se, record, state.Revision, revision); err != nil {
			return nil, err
		}
		return &agentv1.Envelope{ResourceRevision: revision, Payload: &agentv1.Envelope_InstanceChanged{InstanceChanged: &agentv1.InstanceChanged{Instance: record.instance, Removed: true}}}, nil
	})
	if err != nil {
		return nil, err
	}
	return reply.GetInstanceChanged(), nil
}
