//go:build integration

package agent_test

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/test/pgtest"
)

func adminMutation(operation string) control.AdminMutation {
	return control.AdminMutation{ActorID: "owner-id", ActorRole: "owner", OperationID: operation}
}
func TestAgentManagedCredentialConcurrentDedupAndAudit(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	g := grant()
	const count = 8
	results := make(chan control.IssuedCredential, count)
	failures := make(chan error, count)
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			sessions := a
			if i%2 == 0 {
				sessions = b
			}
			result, err := sessions.IssueManagedCredential(t.Context(), adminMutation("issue"), g)
			if err != nil {
				failures <- err
				return
			}
			results <- result
		})
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	id, secret := "", ""
	secrets := 0
	for result := range results {
		if id != "" && id != result.Credential.ID {
			t.Fatal("duplicate issue created another credential")
		}
		id = result.Credential.ID
		if result.Secret != "" {
			secrets++
			secret = result.Secret
			if result.Replayed {
				t.Fatal("replay revealed secret")
			}
		}
	}
	if secrets != 1 {
		t.Fatalf("secret returned %d times", secrets)
	}
	authenticated, err := b.Authenticate(t.Context(), secret)
	if err != nil || authenticated.ID != id {
		t.Fatalf("issued credential unusable: %v", err)
	}
	entries, err := b.ListAdminAudit(t.Context(), g.Scope, 0)
	if err != nil || len(entries) != 1 || entries[0].ActorID != "owner-id" || entries[0].ObjectID != id {
		t.Fatalf("audit: %+v %v", entries, err)
	}
	rows := pgtest.Query(t, dsn, `SELECT (SELECT count(*) FROM agent_credentials),(SELECT count(*) FROM agent_admin_operations),(SELECT count(*) FROM agent_admin_audit WHERE position($1 in convert_from(result,'UTF8'))>0)`, secret)
	if rows[0][0] != "1" || rows[0][1] != "1" || rows[0][2] != "0" {
		t.Fatal("dedup/audit persisted duplicate or secret")
	}
	g.BootID = "changed"
	_, err = a.IssueManagedCredential(t.Context(), adminMutation("issue"), g)
	if !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("operation payload changed: %v", err)
	}
	items, err := a.ListCredentials(t.Context(), g.Scope, "")
	if err != nil || len(items) != 1 {
		t.Fatal("credential metadata unavailable")
	}
	if err := a.RevokeManagedCredential(t.Context(), g.Scope, adminMutation("revoke"), id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Authenticate(t.Context(), secret); err == nil {
		t.Fatal("revocation did not reach other node")
	}
	if err := b.RevokeManagedCredential(t.Context(), g.Scope, adminMutation("revoke"), id); err != nil {
		t.Fatal(err)
	}
	other := g.Scope
	other.ServiceKey = "other"
	for _, candidate := range []string{id, "missing"} {
		if err := a.RevokeManagedCredential(t.Context(), other, adminMutation("revoke-"+candidate), candidate); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("cross-scope revoke leaked existence: %v", err)
		}
	}
}
func TestAgentManagedScopeLimitCASAndReplay(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	scope := grant().Scope
	if _, err := s.SetManagedScopeLimit(t.Context(), scope, adminMutation("first"), 0, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetManagedScopeLimit(t.Context(), scope, adminMutation("second"), 2, 3); err != nil {
		t.Fatal(err)
	}
	replay, err := s.SetManagedScopeLimit(t.Context(), scope, adminMutation("first"), 0, 2)
	if err != nil || replay.MaxConnections != 2 {
		t.Fatal("lost immutable operation result")
	}
	current, err := s.ScopeLimit(t.Context(), scope)
	if err != nil || current.MaxConnections != 3 {
		t.Fatal("old replay rolled quota back")
	}
	if _, err := s.SetManagedScopeLimit(t.Context(), scope, adminMutation("stale"), 2, 4); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	entries, err := s.ListAdminAudit(t.Context(), scope, 0)
	if err != nil || len(entries) != 2 {
		t.Fatal("replay or failed CAS added audit")
	}
	pgtest.Exec(t, dsn, `UPDATE agent_admin_operations SET expires_at=clock_timestamp()-interval '1 second' WHERE operation_id='first'`)
	if _, err := s.SetManagedScopeLimit(t.Context(), scope, adminMutation("first"), 0, 2); !errors.Is(err, platform.ErrConflict) {
		t.Fatal("expired operation was reexecuted")
	}
}
func TestAgentAdminAuditFailureRollsBackAllMutations(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	if _, err := s.SetManagedScopeLimit(t.Context(), g.Scope, adminMutation("initial"), 0, 2); err != nil {
		t.Fatal(err)
	}
	credential, err := s.IssueManagedCredential(t.Context(), adminMutation("credential"), g)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, dsn, `CREATE FUNCTION reject_agent_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$; CREATE TRIGGER reject_audit BEFORE INSERT ON agent_admin_audit FOR EACH ROW EXECUTE FUNCTION reject_agent_audit()`)
	if _, err := s.SetManagedScopeLimit(t.Context(), g.Scope, adminMutation("failed-limit"), 2, 4); err == nil {
		t.Fatal("quota committed without audit")
	}
	if _, err := s.IssueManagedCredential(t.Context(), adminMutation("failed-issue"), g); err == nil {
		t.Fatal("credential committed without audit")
	}
	if err := s.RevokeManagedCredential(t.Context(), g.Scope, adminMutation("failed-revoke"), credential.Credential.ID); err == nil {
		t.Fatal("revoke committed without audit")
	}
	if _, err := s.Authenticate(t.Context(), credential.Secret); err != nil {
		t.Fatal("failed revoke changed credential")
	}
	current, err := s.ScopeLimit(t.Context(), g.Scope)
	if err != nil || current.MaxConnections != 2 {
		t.Fatal("failed quota changed scope")
	}
	items, err := s.ListCredentials(t.Context(), g.Scope, "")
	if err != nil || len(items) != 1 {
		t.Fatal("failed issue persisted credential")
	}
	rows := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_admin_operations WHERE operation_id LIKE 'failed-%'`)
	if rows[0][0] != "0" {
		t.Fatal("failed transaction retained operation result")
	}
	pgtest.Exec(t, dsn, `DROP TRIGGER reject_audit ON agent_admin_audit; DROP FUNCTION reject_agent_audit()`)
	if _, err := s.SetManagedScopeLimit(t.Context(), g.Scope, adminMutation("failed-limit"), 2, 4); err != nil {
		t.Fatalf("rolled-back operation cannot retry: %v", err)
	}
	g.ExpiresAt = time.Now().Add(25 * time.Hour)
	if _, err := s.IssueManagedCredential(t.Context(), adminMutation("too-long"), g); !errors.Is(err, platform.ErrValidation) {
		t.Fatalf("unbounded credential lifetime: %v", err)
	}
}

func TestAgentManagementMigratesV2WithoutResettingSessions(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	session, err := s.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous v2 component schema, preserving its data.
	s.Close()
	pgtest.Exec(t, dsn, `DROP TABLE agent_activation_reports,agent_activation_report_heads,agent_activation_sources,agent_activation_versions,agent_activations,agent_preparation_decisions,agent_prepare_rejections,agent_prepared_evidence,agent_prepared_heads,agent_prepare_heads,agent_prepare_intents,agent_release_resource_refs,agent_release_events,agent_release_operations,agent_releases,agent_release_heads,agent_discovery_resources,agent_discovery_heads,agent_naming_intents,agent_instance_operations,agent_instances,agent_deployments,agent_registration_scopes,agent_admin_operations,agent_admin_audit; UPDATE agent_schema SET version=2 WHERE id=true`)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	authenticated, err := b.Authenticate(t.Context(), secret)
	if err != nil || authenticated.BootID != g.BootID {
		t.Fatalf("migration lost credential: %v", err)
	}
	if err := a.CheckSession(t.Context(), session); err != nil {
		t.Fatalf("migration reset session: %v", err)
	}
	next, err := b.Connect(t.Context(), secret, helloFor(g))
	if err != nil || next.Epoch != session.Epoch+1 {
		t.Fatalf("migration lost epoch: %v", err)
	}
	if _, err := a.SetManagedScopeLimit(t.Context(), g.Scope, adminMutation("after-upgrade"), 2, 3); err != nil {
		t.Fatal(err)
	}
	rows := pgtest.Query(t, dsn, `SELECT version FROM agent_schema WHERE id=true`)
	if rows[0][0] != "11" {
		t.Fatal("schema did not advance exactly once")
	}
}

func TestAgentManagementMetadataAndAuditPagination(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	for i := range 101 {
		g.InstanceID = fmt.Sprintf("instance-%03d", i)
		issue(t, s, g)
	}
	first, err := s.ListCredentials(t.Context(), g.Scope, "")
	if err != nil || len(first) != 100 {
		t.Fatalf("first page size %d: %v", len(first), err)
	}
	second, err := s.ListCredentials(t.Context(), g.Scope, first[len(first)-1].ID)
	if err != nil || len(second) != 1 {
		t.Fatalf("second page size %d: %v", len(second), err)
	}
	for _, item := range first {
		if item.ID == second[0].ID {
			t.Fatal("cursor repeated credential")
		}
	}
	other := g.Scope
	other.ServiceKey = "foreign"
	empty, err := s.ListCredentials(t.Context(), other, "")
	if err != nil || len(empty) != 0 {
		t.Fatal("metadata crossed service scope")
	}
	pgtest.Exec(t, dsn, `INSERT INTO agent_admin_audit(workspace_id,environment_id,service_key,actor_id,actor_role,operation_id,action,object_id,result) SELECT $1,$2,$3,'owner','owner','fixture-'||n,'fixture','object','{}'::bytea FROM generate_series(1,101) n`, g.Scope.WorkspaceID, g.Scope.EnvironmentID, g.Scope.ServiceKey)
	entries, err := s.ListAdminAudit(t.Context(), g.Scope, 0)
	if err != nil || len(entries) != 100 {
		t.Fatalf("audit first page %d: %v", len(entries), err)
	}
	before, err := strconv.ParseUint(entries[len(entries)-1].ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := s.ListAdminAudit(t.Context(), g.Scope, before)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("audit second page %d: %v", len(remaining), err)
	}
}
