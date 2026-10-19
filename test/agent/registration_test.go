//go:build integration

package agent_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

type registrationFixture struct {
	dsn     string
	a, b    *control.Sessions
	g       control.Grant
	session control.Session
	secret  string
	policy  control.RegistrationPolicyState
	request *agentv1.Register
}

func registrationDescriptor(g control.Grant) *agentv1.Instance {
	return &agentv1.Instance{Scope: &agentv1.Scope{WorkspaceId: g.Scope.WorkspaceID, EnvironmentId: g.Scope.EnvironmentID, ServiceKey: g.Scope.ServiceKey}, InstanceId: g.InstanceID, BootId: g.BootID, BuildId: "build-a", ImageDigest: "sha256:" + strings.Repeat("a", 64), Address: "10.20.0.10", Port: 8443, TlsServerName: "checkout.internal", Zone: "az-a", ProtocolVersion: 1}
}
func newRegistrationFixture(t *testing.T, maximum int) *registrationFixture {
	return newRegistrationFixtureWithPermissions(t, maximum, 0)
}
func newRegistrationFixtureWithPermissions(t *testing.T, maximum int, permissions control.Permission) *registrationFixture {
	t.Helper()
	f := &registrationFixture{dsn: pgtest.DSN(t), g: grant()}
	f.g.Permissions |= control.DiscoveryRead | permissions
	f.a = openSessions(t, f.dsn)
	f.b = openSessions(t, f.dsn)
	if err := f.a.SetScopeLimit(t.Context(), f.g.Scope, 4); err != nil {
		t.Fatal(err)
	}
	var err error
	f.policy, err = f.a.SetRegistrationPolicy(t.Context(), f.g.Scope, adminMutation("registration-policy"), 0, control.RegistrationPolicy{AllowedNetworks: []string{"10.20.0.0/16"}, ForbiddenAddresses: []string{"10.20.0.1"}, MaxInstances: maximum})
	if err != nil {
		t.Fatal(err)
	}
	_, f.secret = issue(t, f.a, f.g)
	f.session, err = f.a.Connect(t.Context(), f.secret, helloFor(f.g))
	if err != nil {
		t.Fatal(err)
	}
	instance := registrationDescriptor(f.g)
	authorization, err := f.a.AuthorizeDeployment(t.Context(), adminMutation("deployment"), f.g.Scope, f.g.PrincipalID, f.g.AgentID, f.policy.Revision, instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	instance.Ready = true
	f.request = &agentv1.Register{Instance: instance, DeploymentAuthorization: []byte(authorization.ID)}
	return f
}
func registerInstance(t *testing.T, f *registrationFixture, operation string) *agentv1.Registered {
	t.Helper()
	registered, err := f.a.RegisterInstance(t.Context(), f.session, operation, f.request)
	if err != nil {
		t.Fatal(err)
	}
	return registered
}
func TestInstanceRegistrationLeaseLifecycleAndFencing(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "register")
	remaining := time.Until(time.UnixMilli(registered.Instance.Lease.ExpiresUnixMs))
	if remaining < 58*time.Second || remaining > 60*time.Second {
		t.Fatalf("lease TTL %s", remaining)
	}
	snapshot, err := f.b.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 1 || snapshot.Members[0].NamingConfirmed {
		t.Fatal("unconfirmed registration was published as reconciled")
	}
	intents, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(intents) != 1 || !intents[0].Present {
		t.Fatalf("missing durable naming intent: %v", err)
	}
	if err := f.b.ConfirmNamingIntent(t.Context(), intents[0]); err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || !confirmed.Members[0].NamingConfirmed || confirmed.Revision <= snapshot.Revision || confirmed.ExpiresAt.After(time.UnixMilli(registered.Instance.Lease.ExpiresUnixMs)) {
		t.Fatal("confirmation did not create a bounded coherent revision")
	}
	for range 3 {
		if err := f.a.Heartbeat(t.Context(), f.session); err != nil {
			t.Fatal(err)
		}
	}
	unchanged, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || unchanged.Members[0].Instance.Lease.ExpiresUnixMs != registered.Instance.Lease.ExpiresUnixMs {
		t.Fatal("heartbeat extended instance lease")
	}
	successor, err := f.b.Connect(t.Context(), f.secret, helloFor(f.g))
	if err != nil {
		t.Fatal(err)
	}
	renewal := &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: registered.Instance.Lease}
	_, err = f.a.RenewInstanceLease(t.Context(), f.session, "old-session", renewal)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	f.session = successor
	renewed, err := f.b.RenewInstanceLease(t.Context(), successor, "renew", renewal)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.a.RenewInstanceLease(t.Context(), successor, "renew", renewal)
	if err != nil || !proto.Equal(renewed, replay) {
		t.Fatal("renew replay changed lease deadline or revision")
	}
	repeated, err := f.a.RegisterInstance(t.Context(), successor, "register", f.request)
	if err != nil || !proto.Equal(repeated, registered) {
		t.Fatal("same-boot reconnect lost operation result")
	}
	replacement := registerInstance(t, f, "replace")
	if replacement.Instance.Lease.Epoch != registered.Instance.Lease.Epoch+1 || replacement.Instance.Lease.Id == registered.Instance.Lease.Id {
		t.Fatal("new registration did not fence old lease")
	}
	_, err = f.a.RenewInstanceLease(t.Context(), successor, "stale-lease", renewal)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_FENCED)
	if err := f.b.ConfirmNamingIntent(t.Context(), intents[0]); err == nil {
		t.Fatal("delayed Naming confirmation published old incarnation")
	}
	changed := proto.Clone(replacement.Instance).(*agentv1.Instance)
	changed.Draining = true
	if _, err := f.b.UpdateRegisteredInstance(t.Context(), successor, "drain", &agentv1.UpdateInstance{Instance: changed}); err != nil {
		t.Fatal(err)
	}
	changed.Address = "10.20.0.11"
	_, err = f.a.UpdateRegisteredInstance(t.Context(), successor, "change-address", &agentv1.UpdateInstance{Instance: changed})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	deregister := &agentv1.Deregister{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: replacement.Instance.Lease}
	removed, err := f.a.DeregisterInstance(t.Context(), successor, "remove", deregister)
	if err != nil || !removed.Removed {
		t.Fatal("deregister failed")
	}
	if _, err := f.b.DeregisterInstance(t.Context(), successor, "remove", deregister); err != nil {
		t.Fatal("deregister replay was not idempotent")
	}
	empty, err := f.a.DiscoveryCandidates(t.Context(), successor)
	if err != nil || len(empty.Members) != 0 {
		t.Fatal("deregister remained in candidate set")
	}
	again := registerInstance(t, f, "again")
	if again.Instance.Lease.Epoch != replacement.Instance.Lease.Epoch+1 {
		t.Fatal("deregister cleared fencing watermark")
	}
}
func TestInstanceRegisterConcurrentDedupAndExactDeployment(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	results := make(chan *agentv1.Registered, 12)
	failures := make(chan error, 12)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			store := f.a
			if i%2 == 0 {
				store = f.b
			}
			registered, err := store.RegisterInstance(t.Context(), f.session, "same-operation", f.request)
			if err != nil {
				failures <- err
			} else {
				results <- registered
			}
		})
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first *agentv1.Registered
	for result := range results {
		if first == nil {
			first = result
		}
		if !proto.Equal(first, result) || result.Instance.Lease.Epoch != 1 {
			t.Fatal("duplicate registration allocated another lease")
		}
	}
	changed := proto.Clone(f.request).(*agentv1.Register)
	changed.Instance.Ready = false
	_, err := f.a.RegisterInstance(t.Context(), f.session, "same-operation", changed)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	for _, change := range []func(*agentv1.Instance){func(i *agentv1.Instance) { i.Address = "10.20.0.11" }, func(i *agentv1.Instance) { i.BuildId = "build-b" }, func(i *agentv1.Instance) { i.TlsServerName = "other.internal" }, func(i *agentv1.Instance) { i.ImageDigest = "sha256:" + strings.Repeat("b", 64) }, func(i *agentv1.Instance) { i.Scope.EnvironmentId = "foreign" }} {
		changed := proto.Clone(f.request).(*agentv1.Register)
		change(changed.Instance)
		if _, err := f.b.RegisterInstance(t.Context(), f.session, "forged-deployment", changed); err == nil {
			t.Fatal("credential allowed an unauthorized deployment descriptor")
		}
	}
	rows := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_instances),(SELECT count(*) FROM agent_instance_operations),(SELECT count(*) FROM agent_naming_intents)`)
	if rows[0][0] != "1" || rows[0][1] != "1" || rows[0][2] != "1" {
		t.Fatal("duplicate/failed register left partial state")
	}
}
func TestInstanceExpiryRevocationAndPolicyChangeFailClosed(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "register")
	oldIntents, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil {
		t.Fatal(err)
	}
	expired := proto.Clone(registered.Instance).(*agentv1.Instance)
	deadline := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	expired.Lease.ExpiresUnixMs = deadline.UnixMilli()
	body, err := proto.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_instances SET descriptor=$1,expires_at=$2`, body, deadline)
	renewal := &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: registered.Instance.Lease}
	_, err = f.b.RenewInstanceLease(t.Context(), f.session, "expired", renewal)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	if err := f.a.Heartbeat(t.Context(), f.session); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 0 || snapshot.Revision <= registered.DiscoveryRevision {
		t.Fatal("expired lease remained discoverable")
	}
	if err := f.b.ConfirmNamingIntent(t.Context(), oldIntents[0]); err == nil {
		t.Fatal("Naming readback resurrected expired lease")
	}
	registerInstance(t, f, "replacement")
	policy := f.policy.Policy
	policy.MaxInstances = 2
	if _, err := f.a.SetRegistrationPolicy(t.Context(), f.g.Scope, adminMutation("policy-change"), f.policy.Revision, policy); err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.b.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 0 {
		t.Fatal("policy change kept old deployment authorized")
	}
	_, err = f.a.RegisterInstance(t.Context(), f.session, "old-policy", f.request)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	instance := proto.Clone(f.request.Instance).(*agentv1.Instance)
	instance.Ready = false
	instance.Draining = false
	instance.Lease = nil
	authorization, err := f.a.AuthorizeDeployment(t.Context(), adminMutation("new-policy-deployment"), f.g.Scope, f.g.PrincipalID, f.g.AgentID, f.policy.Revision+1, instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fresh := proto.Clone(f.request).(*agentv1.Register)
	fresh.DeploymentAuthorization = []byte(authorization.ID)
	if _, err := f.b.RegisterInstance(t.Context(), f.session, "accepted-new-policy", fresh); err != nil {
		t.Fatalf("policy change prevented authorized recovery: %v", err)
	}

}
func TestInstanceBootTakeoverExcludesOldBootBeforeNewRegistration(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	first := registerInstance(t, f, "old")
	next := f.g
	next.BootID = "boot-next"
	next.TakeoverFromBootID = f.g.BootID
	_, secret := issue(t, f.b, next)
	session, err := f.b.Connect(t.Context(), secret, helloFor(next))
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := f.b.DiscoveryCandidates(t.Context(), session)
	if err != nil || len(candidates.Members) != 0 {
		t.Fatal("boot takeover retained old candidate")
	}
	instance := registrationDescriptor(next)
	authorization, err := f.a.AuthorizeDeployment(t.Context(), adminMutation("new-deployment"), next.Scope, next.PrincipalID, next.AgentID, f.policy.Revision, instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	instance.Ready = true
	registered, err := f.b.RegisterInstance(t.Context(), session, "new", &agentv1.Register{Instance: instance, DeploymentAuthorization: []byte(authorization.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Instance.BootId != next.BootID || registered.Instance.Lease.Epoch != first.Instance.Lease.Epoch+1 {
		t.Fatal("boot takeover reset lease fencing")
	}
	renewal := &agentv1.RenewLease{InstanceId: next.InstanceID, BootId: f.g.BootID, Lease: first.Instance.Lease}
	if _, err := f.a.RenewInstanceLease(t.Context(), session, "wrong-boot", renewal); err == nil {
		t.Fatal("successor session renewed prior boot")
	}
	if err := f.a.RevokeCredential(t.Context(), session.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.RenewInstanceLease(t.Context(), session, "revoked", &agentv1.RenewLease{InstanceId: next.InstanceID, BootId: next.BootID, Lease: registered.Instance.Lease}); err == nil {
		t.Fatal("revoked identity renewed lease")
	}
	if _, err := f.a.ExpireInstances(t.Context(), next.Scope); err != nil {
		t.Fatal(err)
	}
	rows := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_instances WHERE active`)
	if rows[0][0] != "0" {
		t.Fatal("revoked registration remained active")
	}
}
func TestInstanceIntentFailureAndUint64OverflowDoNotPartiallyCommit(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	pgtest.Exec(t, f.dsn, `CREATE FUNCTION reject_naming_intent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER reject_naming_intent BEFORE INSERT ON agent_naming_intents FOR EACH ROW EXECUTE FUNCTION reject_naming_intent()`)
	if _, err := f.a.RegisterInstance(t.Context(), f.session, "retry", f.request); err == nil {
		t.Fatal("registration committed without durable Naming intent")
	}
	rows := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_instances),(SELECT count(*) FROM agent_instance_operations),discovery_revision::text FROM agent_registration_scopes`)
	if rows[0][0] != "0" || rows[0][1] != "0" || rows[0][2] != fmt.Sprint(f.policy.DiscoveryRevision) {
		t.Fatal("registration failure partially committed")
	}
	pgtest.Exec(t, f.dsn, `DROP TRIGGER reject_naming_intent ON agent_naming_intents; DROP FUNCTION reject_naming_intent()`)
	registered := registerInstance(t, f, "retry")
	maxInstance := proto.Clone(registered.Instance).(*agentv1.Instance)
	maxInstance.Lease.Epoch = math.MaxUint64
	body, err := proto.Marshal(maxInstance)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, f.dsn, `UPDATE agent_instances SET lease_epoch=18446744073709551615,descriptor=$1`, body)
	_, err = f.a.RegisterInstance(t.Context(), f.session, "overflow-lease", f.request)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	pgtest.Exec(t, f.dsn, `UPDATE agent_registration_scopes SET discovery_revision=18446744073709551615`)
	_, err = f.b.RenewInstanceLease(t.Context(), f.session, "overflow-discovery", &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: maxInstance.Lease})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
}
func TestInstanceEndpointAdmissionAndConcurrentTakeoverNoDeadlock(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	for index, address := range []string{"localhost", "127.0.0.1", "0.0.0.0", "169.254.169.254", "224.0.0.1", "10.20.0.1", "10.21.0.10", "::1", "::ffff:10.20.0.10"} {
		instance := registrationDescriptor(f.g)
		instance.BootId = fmt.Sprintf("invalid-%d", index)
		instance.Address = address
		_, err := f.a.AuthorizeDeployment(t.Context(), adminMutation(fmt.Sprintf("bad-%d", index)), f.g.Scope, f.g.PrincipalID, f.g.AgentID, f.policy.Revision, instance, time.Now().Add(time.Hour))
		if !errors.Is(err, platform.ErrValidation) {
			t.Fatalf("address %s admitted: %v", address, err)
		}
	}
	registerInstance(t, f, "initial")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	workers.Go(func() {
		for range 15 {
			if _, err := f.b.Connect(ctx, f.secret, helloFor(f.g)); err != nil {
				failures <- err
				return
			}
		}
	})
	workers.Go(func() {
		for i := range 15 {
			_, err := f.a.RegisterInstance(ctx, f.session, fmt.Sprintf("race-%d", i), f.request)
			if err != nil {
				var detail *agentv1.ProtocolError
				if !errors.As(err, &detail) || detail.Code != agentv1.ErrorCode_ERROR_CODE_FENCED {
					failures <- err
					return
				}
			}
		}
	})
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

func TestInstanceIdentityQuotaAndNamingQueueFairness(t *testing.T) {
	f := newRegistrationFixture(t, 101)
	for index := range 101 {
		g := f.g
		g.InstanceID = fmt.Sprintf("member-%03d", index)
		_, secret := issue(t, f.a, g)
		session, err := f.a.Connect(t.Context(), secret, helloFor(g))
		if err != nil {
			t.Fatal(err)
		}
		instance := registrationDescriptor(g)
		authorization, err := f.a.AuthorizeDeployment(t.Context(), adminMutation(fmt.Sprintf("authorize-%03d", index)), g.Scope, g.PrincipalID, g.AgentID, f.policy.Revision, instance, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.a.RegisterInstance(t.Context(), session, fmt.Sprintf("register-%03d", index), &agentv1.Register{Instance: instance, DeploymentAuthorization: []byte(authorization.ID)}); err != nil {
			t.Fatal(err)
		}
		if err := f.a.Disconnect(t.Context(), session); err != nil {
			t.Fatal(err)
		}
	}
	// The original identity was authorized but has no allocated lease row yet.
	_, err := f.a.RegisterInstance(t.Context(), f.session, "over-quota", f.request)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED)
	first, err := f.a.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(first) != 100 {
		t.Fatalf("first bounded naming batch %d: %v", len(first), err)
	}
	second, err := f.b.PendingNamingIntents(t.Context(), f.g.Scope)
	if err != nil || len(second) != 1 {
		t.Fatalf("second batch starved by active refreshes: %d %v", len(second), err)
	}
	ids := map[string]bool{}
	for _, intent := range first {
		ids[intent.Instance.Lease.Id] = true
	}
	if ids[second[0].Instance.Lease.Id] {
		t.Fatal("two replicas claimed same due intent")
	}
}
func TestInstanceMigrationFromV3PreservesManagementData(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	credential, err := s.IssueManagedCredential(t.Context(), adminMutation("issue-v3"), g)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	pgtest.Exec(t, dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections,agent_prepared_evidence,agent_prepared_heads,agent_prepare_heads,agent_prepare_intents,agent_release_resource_refs,agent_release_events,agent_release_operations,agent_releases,agent_release_heads,agent_discovery_resources,agent_discovery_heads,agent_naming_intents,agent_instance_operations,agent_instances,agent_deployments,agent_registration_scopes; UPDATE agent_schema SET version=3 WHERE id=true`)
	upgraded := openSessions(t, dsn)
	if _, err := upgraded.Authenticate(t.Context(), credential.Secret); err != nil {
		t.Fatal("v4 migration lost credential")
	}
	audit, err := upgraded.ListAdminAudit(t.Context(), g.Scope, 0)
	if err != nil || len(audit) != 1 || audit[0].ObjectID != credential.Credential.ID {
		t.Fatal("v4 migration lost management audit")
	}
	if _, err := upgraded.SetRegistrationPolicy(t.Context(), g.Scope, adminMutation("after-v4"), 0, control.RegistrationPolicy{AllowedNetworks: []string{"10.20.0.0/16"}, MaxInstances: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceCredentialRotationRebindsLeaseWithoutResurrection(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "initial")
	_, secret := issue(t, f.b, f.g)
	successor, err := f.b.Connect(t.Context(), secret, helloFor(f.g))
	if err != nil {
		t.Fatal(err)
	}
	request := &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: registered.Instance.Lease}
	if _, err := f.b.RenewInstanceLease(t.Context(), successor, "rotate-renew", request); err != nil {
		t.Fatal(err)
	}
	if err := f.a.RevokeCredential(t.Context(), f.session.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.ExpireInstances(t.Context(), f.g.Scope); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.b.DiscoveryCandidates(t.Context(), successor)
	if err != nil || len(snapshot.Members) != 1 || snapshot.Members[0].Instance.Lease.Epoch != registered.Instance.Lease.Epoch {
		t.Fatal("retired credential removed successor-owned lease")
	}
	if _, err := f.a.RenewInstanceLease(t.Context(), f.session, "retired-renew", request); err == nil {
		t.Fatal("retired credential extended lease")
	}
}

func TestInstanceEmptyDiscoveryRefreshesWithoutRevisionChange(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	pgtest.Exec(t, f.dsn, `UPDATE agent_registration_scopes SET updated_at=clock_timestamp()-interval '2 hours'`)
	first, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Members) != 0 || !first.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		t.Fatal("stable empty scope returned expired snapshot", first)
	}
	second, err := f.b.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || second.Revision != first.Revision || second.ExpiresAt.Before(first.ExpiresAt) {
		t.Fatal("refresh changed revision or moved expiry backwards", second, err)
	}
}
