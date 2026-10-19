//go:build integration

package agent_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func openSessions(t *testing.T, dsn string) *control.Sessions {
	t.Helper()
	s, err := control.OpenSessions(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func grant() control.Grant {
	return control.Grant{PrincipalID: "workload-1", Scope: client.Scope{WorkspaceID: "w", EnvironmentID: "e", ServiceKey: "checkout"}, AgentID: "agent-1", InstanceID: "instance-1", BootID: "boot-1", Permissions: control.ConfigRead | control.InstanceRegister | control.InstanceRenew, ExpiresAt: time.Now().Add(time.Hour)}
}
func issue(t *testing.T, s *control.Sessions, g control.Grant) (string, string) {
	t.Helper()
	id, secret, err := s.IssueCredential(t.Context(), g)
	if err != nil {
		t.Fatal(err)
	}
	return id, secret
}
func helloFor(g control.Grant) *agentv1.Hello {
	return &agentv1.Hello{AgentId: g.AgentID, InstanceId: g.InstanceID, BootId: g.BootID}
}
func expectCode(t *testing.T, err error, code agentv1.ErrorCode) {
	t.Helper()
	if remote, ok := errors.AsType[*client.RemoteError](err); ok {
		if remote.Detail.Code != code {
			t.Fatalf("remote error %v; want %v", remote, code)
		}
		return
	}
	pe, ok := errors.AsType[*agentv1.ProtocolError](err)
	if !ok || pe.Code != code {
		t.Fatalf("error %v; want %v", err, code)
	}
}
func TestSessionReplicaFencingAndRevocation(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	g := grant()
	if err := a.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	id, secret := issue(t, a, g)
	if got, err := b.Authenticate(t.Context(), secret); err != nil || got.ID != id || got.Scope != g.Scope {
		t.Fatalf("replica authenticate: %v", err)
	}
	first, err := a.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != first.Epoch+1 || second.ID == first.ID {
		t.Fatal("reconnect did not fence")
	}
	expectCode(t, a.Heartbeat(t.Context(), first), agentv1.ErrorCode_ERROR_CODE_FENCED)
	if err := a.Disconnect(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := b.Heartbeat(t.Context(), second); err != nil {
		t.Fatalf("old close killed successor: %v", err)
	}
	if err := a.RevokeCredential(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	expectCode(t, b.CheckSession(t.Context(), second), agentv1.ErrorCode_ERROR_CODE_FENCED)
	expectCode(t, b.Heartbeat(t.Context(), second), agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
	_, err = b.Authenticate(t.Context(), secret)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
	rows := pgtest.Query(t, dsn, `SELECT octet_length(secret_hash),position($1 in encode(secret_hash,'hex')) FROM agent_credentials WHERE id=$2`, secret, id)
	if len(rows) != 1 || rows[0][0] != "32" || rows[0][1] != "0" {
		t.Fatal("credential persistence mismatch")
	}
}
func TestSessionDeploymentBootCASAndAdmission(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	_, secret := issue(t, s, g)
	_, err := s.Connect(t.Context(), secret, helloFor(g))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	if err := s.SetScopeLimit(t.Context(), g.Scope, 1); err != nil {
		t.Fatal(err)
	}
	missing := g
	missing.InstanceID = "missing-instance"
	missing.TakeoverFromBootID = "missing-boot"
	_, missingSecret := issue(t, s, missing)
	_, err = s.Connect(t.Context(), missingSecret, helloFor(missing))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	initial, err := s.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	wrong := helloFor(g)
	wrong.BootId = "self-selected"
	_, err = s.Connect(t.Context(), secret, wrong)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	g2 := g
	g2.BootID = "boot-2"
	_, secret2 := issue(t, s, g2)
	_, err = s.Connect(t.Context(), secret2, helloFor(g2))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if err := s.CheckSession(t.Context(), initial); err != nil {
		t.Fatal("failed takeover damaged old session")
	}
	g2.TakeoverFromBootID = g.BootID
	_, secret2 = issue(t, s, g2)
	next, err := s.Connect(t.Context(), secret2, helloFor(g2))
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch != initial.Epoch+1 {
		t.Fatal("failed attempts allocated epoch")
	}
	_, err = s.Connect(t.Context(), secret, helloFor(g))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	third := g
	third.InstanceID = "instance-2"
	third.AgentID = "agent-2"
	_, secret3 := issue(t, s, third)
	_, err = s.Connect(t.Context(), secret3, helloFor(third))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED)
	// An unrelated principal cannot hijack an existing boot with a new token.
	other := g2
	other.PrincipalID = "other"
	_, otherSecret := issue(t, s, other)
	_, err = s.Connect(t.Context(), otherSecret, helloFor(other))
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	if err := s.Disconnect(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Connect(t.Context(), secret3, helloFor(third)); err != nil {
		t.Fatalf("expired connection retained admission: %v", err)
	}
}
func TestSessionConcurrentConnectAndReopen(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	g := grant()
	if err := a.SetScopeLimit(t.Context(), g.Scope, 1); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, a, g)
	const n = 12
	results := make(chan control.Session, n)
	failures := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s := a
			if i%2 != 0 {
				s = b
			}
			se, err := s.Connect(t.Context(), secret, helloFor(g))
			if err != nil {
				failures <- err
			} else {
				results <- se
			}
		})
	}
	wg.Wait()
	close(failures)
	close(results)
	for err := range failures {
		t.Fatal(err)
	}
	var current control.Session
	seen := map[uint64]bool{}
	for se := range results {
		if seen[se.Epoch] {
			t.Fatal("duplicate epoch")
		}
		seen[se.Epoch] = true
		if se.Epoch > current.Epoch {
			current = se
		}
	}
	if len(seen) != n || current.Epoch != n {
		t.Fatalf("epochs: %v", seen)
	}
	reopened := openSessions(t, dsn)
	for epoch := range seen {
		se := current
		se.Epoch = epoch
		if epoch != current.Epoch {
			expectCode(t, reopened.Heartbeat(t.Context(), se), agentv1.ErrorCode_ERROR_CODE_FENCED)
		}
	}
	if err := reopened.Heartbeat(t.Context(), current); err != nil {
		t.Fatalf("current session did not survive reopen: %v", err)
	}
	pgtest.Query(t, dsn, `UPDATE agent_sessions SET expires_at=clock_timestamp()-interval '1 second' RETURNING instance_id`)
	expectCode(t, reopened.Heartbeat(t.Context(), current), agentv1.ErrorCode_ERROR_CODE_FENCED)
	_, err := reopened.Authenticate(t.Context(), "grayling_admin_invalid")
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED)
}
