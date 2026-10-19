package agent

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

// RegistrationPolicy is explicitly provisioned per immutable service scope.
// Literal addresses only: registration never resolves names or probes endpoints.
type RegistrationPolicy struct {
	AllowedNetworks    []string `json:"allowedNetworks"`
	ForbiddenAddresses []string `json:"forbiddenAddresses,omitempty"`
	AllowLoopback      bool     `json:"allowLoopback"`
	MaxInstances       int      `json:"maxInstances"`
}
type RegistrationPolicyState struct {
	Revision          uint64             `json:"revision,string"`
	DiscoveryRevision uint64             `json:"discoveryRevision,string"`
	Policy            RegistrationPolicy `json:"policy"`
}
type DeploymentGrant struct {
	ID             string    `json:"id"`
	PolicyRevision uint64    `json:"policyRevision,string"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

func (p RegistrationPolicy) validate() error {
	if p.MaxInstances < 1 || p.MaxInstances > 100000 || len(p.AllowedNetworks) == 0 || len(p.AllowedNetworks) > 32 || len(p.ForbiddenAddresses) > 128 {
		return platform.ErrValidation
	}
	seen := map[string]bool{}
	for _, network := range p.AllowedNetworks {
		prefix, err := netip.ParsePrefix(network)
		if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.String() != network || prefix.Addr().Is4In6() || seen[network] {
			return platform.ErrValidation
		}
		seen[network] = true
	}
	clear(seen)
	for _, value := range p.ForbiddenAddresses {
		address, err := netip.ParseAddr(value)
		if err != nil || address.String() != value || address.Zone() != "" || seen[value] {
			return platform.ErrValidation
		}
		seen[value] = true
	}
	return nil
}
func (p RegistrationPolicy) allows(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil || address.String() != value || address.Zone() != "" || address.Is4In6() || (!address.IsGlobalUnicast() && !(address.IsLoopback() && p.AllowLoopback)) || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsLoopback() && !p.AllowLoopback || slices.Contains(p.ForbiddenAddresses, value) {
		return false
	}
	for _, value := range p.AllowedNetworks {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
func validServerName(name string) bool {
	if len(name) == 0 || len(name) > 253 || strings.ToLower(name) != name {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, letter := range label {
			if !(letter >= 'a' && letter <= 'z' || letter >= '0' && letter <= '9' || letter == '-') {
				return false
			}
		}
	}
	return true
}
func validInstanceDescriptor(instance *agentv1.Instance, scope client.Scope) bool {
	if instance == nil || instance.Scope == nil || instance.Scope.WorkspaceId != scope.WorkspaceID || instance.Scope.EnvironmentId != scope.EnvironmentID || instance.Scope.ServiceKey != scope.ServiceKey || !identifier(instance.InstanceId) || !identifier(instance.BootId) || !identifier(instance.BuildId) || !identifier(instance.Zone) || instance.Port == 0 || instance.Port > 65535 || instance.ProtocolVersion != agentv1.ProtocolMajor || !validServerName(instance.TlsServerName) || len(instance.ImageDigest) != 71 || !strings.HasPrefix(instance.ImageDigest, "sha256:") || strings.ToLower(instance.ImageDigest) != instance.ImageDigest {
		return false
	}
	_, err := hex.DecodeString(instance.ImageDigest[7:])
	return err == nil
}
func descriptorIdentity(instance *agentv1.Instance) *agentv1.Instance {
	copy := proto.Clone(instance).(*agentv1.Instance)
	copy.Ready = false
	copy.Draining = false
	copy.Lease = nil
	return copy
}
func readRegistrationScope(ctx context.Context, tx pgx.Tx, scope client.Scope) (RegistrationPolicyState, error) {
	var state RegistrationPolicyState
	var body []byte
	var policy, revision string
	err := tx.QueryRow(ctx, `SELECT policy,policy_revision::text,discovery_revision::text FROM agent_registration_scopes WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey).Scan(&body, &policy, &revision)
	if err != nil {
		return state, err
	}
	state.Revision, err = strconv.ParseUint(policy, 10, 64)
	if err != nil {
		return state, err
	}
	state.DiscoveryRevision, err = strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return state, err
	}
	if state.Revision != 0 {
		if err := json.Unmarshal(body, &state.Policy, json.RejectUnknownMembers(true)); err != nil {
			return state, err
		}
		if err := state.Policy.validate(); err != nil {
			return state, err
		}
	}
	return state, nil
}
func advanceDiscovery(ctx context.Context, tx pgx.Tx, scope client.Scope, current uint64) (uint64, error) {
	if current == math.MaxUint64 {
		return 0, protocolError(agentv1.ErrorCode_ERROR_CODE_CONFLICT, "discovery revision exhausted")
	}
	next := current + 1
	_, err := tx.Exec(ctx, `UPDATE agent_registration_scopes SET discovery_revision=$4,updated_at=clock_timestamp() WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(next, 10))
	return next, err
}

// SetRegistrationPolicy invalidates existing registrations when policy changes.
// The caller authorizes management scope; mutation/result/audit commit together.
func (s *Sessions) SetRegistrationPolicy(ctx context.Context, scope client.Scope, actor AdminMutation, expected uint64, policy RegistrationPolicy) (RegistrationPolicyState, error) {
	policy.AllowedNetworks = slices.Clone(policy.AllowedNetworks)
	policy.ForbiddenAddresses = slices.Clone(policy.ForbiddenAddresses)
	if err := policy.validate(); err != nil {
		return RegistrationPolicyState{}, err
	}
	body, _, err := s.adminMutation(ctx, scope, actor, "registration.policy", struct {
		Expected uint64
		Policy   RegistrationPolicy
	}{expected, policy}, func(tx pgx.Tx) (adminResult, error) {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_registration_scopes(workspace_id,environment_id,service_key,policy,policy_revision,discovery_revision) VALUES($1,$2,$3,''::bytea,0,0) ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey); err != nil {
			return adminResult{}, err
		}
		state, err := readRegistrationScope(ctx, tx, scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return adminResult{}, platform.ErrNotFound
		}
		if err != nil {
			return adminResult{}, err
		}
		if state.Revision != expected || expected == math.MaxUint64 {
			return adminResult{}, platform.ErrConflict
		}
		revision, err := advanceDiscovery(ctx, tx, scope, state.DiscoveryRevision)
		if err != nil {
			return adminResult{}, err
		}
		encoded, err := json.Marshal(policy, json.Deterministic(true))
		if err != nil {
			return adminResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_registration_scopes SET policy=$4,policy_revision=$5 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, encoded, strconv.FormatUint(expected+1, 10)); err != nil {
			return adminResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_naming_intents SET present=false,confirmed=false,next_attempt_at=clock_timestamp() WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND present`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey); err != nil {
			return adminResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_instances SET active=false,desired_revision=$4,confirmed_revision=0 WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 AND active`, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, strconv.FormatUint(revision, 10)); err != nil {
			return adminResult{}, err
		}
		result, err := json.Marshal(RegistrationPolicyState{Revision: expected + 1, DiscoveryRevision: revision, Policy: policy})
		return adminResult{object: scope.ServiceKey, body: result}, err
	})
	if err != nil {
		return RegistrationPolicyState{}, err
	}
	var state RegistrationPolicyState
	err = json.Unmarshal(body, &state)
	return state, err
}

// AuthorizeDeployment binds an exact image/build/boot/endpoint to a workload.
// It returns an opaque reference, not a transferable bearer credential.
func (s *Sessions) AuthorizeDeployment(ctx context.Context, actor AdminMutation, scope client.Scope, principalID, agentID string, policyRevision uint64, instance *agentv1.Instance, expiresAt time.Time) (DeploymentGrant, error) {
	if !identifier(principalID) || !identifier(agentID) || !validInstanceDescriptor(instance, scope) || instance.Lease != nil || instance.Ready || instance.Draining {
		return DeploymentGrant{}, platform.ErrValidation
	}
	instance = proto.Clone(instance).(*agentv1.Instance)
	descriptor, err := (proto.MarshalOptions{Deterministic: true}).Marshal(instance)
	if err != nil {
		return DeploymentGrant{}, err
	}
	input := struct {
		PrincipalID, AgentID string
		PolicyRevision       uint64
		Descriptor           []byte
		ExpiresAt            time.Time
	}{principalID, agentID, policyRevision, descriptor, expiresAt}
	body, _, err := s.adminMutation(ctx, scope, actor, "deployment.authorize", input, func(tx pgx.Tx) (adminResult, error) {
		state, err := readRegistrationScope(ctx, tx, scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return adminResult{}, platform.ErrNotFound
		}
		if err != nil {
			return adminResult{}, err
		}
		if state.Revision != policyRevision || state.Revision == 0 {
			return adminResult{}, platform.ErrConflict
		}
		if !state.Policy.allows(instance.Address) {
			return adminResult{}, platform.ErrValidation
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT $1>clock_timestamp() AND $1<=clock_timestamp()+interval '24 hours'`, expiresAt).Scan(&valid); err != nil {
			return adminResult{}, err
		}
		if !valid {
			return adminResult{}, platform.ErrValidation
		}
		id, err := randomID()
		if err != nil {
			return adminResult{}, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_deployments VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, id, scope.WorkspaceID, scope.EnvironmentID, scope.ServiceKey, instance.InstanceId, instance.BootId, principalID, agentID, strconv.FormatUint(policyRevision, 10), descriptor, expiresAt)
		if err != nil {
			return adminResult{}, err
		}
		if tag.RowsAffected() != 1 {
			return adminResult{}, platform.ErrConflict
		}
		result, err := json.Marshal(DeploymentGrant{ID: id, PolicyRevision: policyRevision, ExpiresAt: expiresAt})
		return adminResult{object: id, body: result}, err
	})
	if err != nil {
		return DeploymentGrant{}, err
	}
	var grant DeploymentGrant
	err = json.Unmarshal(body, &grant)
	return grant, err
}

// GetRegistrationPolicy reads the scope policy and discovery revision at one
// governance barrier. Management authorization belongs to the caller.
func (s *Sessions) GetRegistrationPolicy(ctx context.Context, scope client.Scope) (RegistrationPolicyState, error) {
	if !scopeValid(scope) {
		return RegistrationPolicyState{}, platform.ErrValidation
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RegistrationPolicyState{}, err
	}
	defer tx.Rollback(ctx)
	state, err := readRegistrationScope(ctx, tx, scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistrationPolicyState{}, platform.ErrNotFound
	}
	if err != nil {
		return RegistrationPolicyState{}, err
	}
	return state, tx.Commit(ctx)
}
