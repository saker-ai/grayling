//go:build integration

package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func TestInstanceWSSSDKLifecycleAndReconnectDedup(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	srv, _ := channelServer(t, f.b)
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client(), RequestTimeout: 5 * time.Second}
	opts.Hello.RequiredCapabilities = []string{"instance.register", "instance.renew"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	welcome := c.Welcome().GetWelcome()
	if !slices.Contains(welcome.Capabilities, "instance.register") || welcome.Limits.LeaseTtlMs != 60000 || welcome.Limits.LeaseRenewIntervalMs != 20000 {
		t.Fatal("registration capability/limits missing")
	}
	first, err := c.Register(t.Context(), "wss-register", f.request)
	if err != nil {
		t.Fatal(err)
	}
	// Reconnect to the other replica, retrying the same durable logical operation.
	srv2, _ := channelServer(t, f.a)
	opts.URL = "wss" + strings.TrimPrefix(srv2.URL, "https")
	opts.HTTPClient = srv2.Client()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	replay, err := successor.Register(t.Context(), "wss-register", f.request)
	if err != nil || !proto.Equal(first, replay) {
		t.Fatal("reconnect changed persisted registration", err)
	}
	renewRequest := &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: first.Instance.Lease}
	if _, err := c.RenewLease(t.Context(), "old-connection", renewRequest); err == nil {
		t.Fatal("superseded connection renewed lease")
	}
	renewed, err := successor.RenewLease(t.Context(), "wss-renew", renewRequest)
	if err != nil || renewed.Lease.Id != first.Instance.Lease.Id {
		t.Fatal("renew failed", err)
	}
	renewedAgain, err := successor.RenewLease(t.Context(), "wss-renew", renewRequest)
	if err != nil || !proto.Equal(renewed, renewedAgain) {
		t.Fatal("renew replay extended lease", err)
	}
	draining := proto.Clone(first.Instance).(*agentv1.Instance)
	draining.Lease = renewed.Lease
	draining.Draining = true
	updated, err := successor.UpdateInstance(t.Context(), "wss-update", &agentv1.UpdateInstance{Instance: draining})
	if err != nil || !updated.Instance.Draining || updated.Removed {
		t.Fatal("update failed", err)
	}
	changed := proto.Clone(f.request).(*agentv1.Register)
	changed.Instance.Ready = !changed.Instance.Ready
	_, err = successor.Register(t.Context(), "wss-register", changed)
	detail, ok := errors.AsType[*client.RemoteError](err)
	if !ok || detail.Detail.Code != agentv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatal("changed operation payload not rejected", err)
	}
	removed, err := successor.Deregister(t.Context(), "wss-deregister", &agentv1.Deregister{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: renewed.Lease})
	if err != nil || !removed.Removed {
		t.Fatal("deregister failed", err)
	}
	_, err = successor.RenewLease(t.Context(), "after-deregister", renewRequest)
	if err == nil {
		t.Fatal("renew resurrected deregistered lease")
	}
}

func TestInstanceWSSPermissionCannotRegisterOrRenew(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	limited := f.g
	limited.Permissions = control.ConfigRead
	_, secret := issue(t, f.a, limited)
	srv, _ := channelServer(t, f.b)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: secret, Hello: helloFor(limited), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if slices.Contains(c.Welcome().GetWelcome().Capabilities, "instance.register") || slices.Contains(c.Welcome().GetWelcome().Capabilities, "instance.renew") {
		t.Fatal("ungranted capability advertised")
	}
	_, err = c.Register(t.Context(), "denied-register", f.request)
	detail, ok := errors.AsType[*client.RemoteError](err)
	if !ok || detail.Detail.Code != agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatal("missing registration permission accepted", err)
	}
	if _, err = c.RenewLease(t.Context(), "denied-renew", &agentv1.RenewLease{InstanceId: f.g.InstanceID, BootId: f.g.BootID, Lease: &agentv1.Lease{Id: "not-a-lease", Epoch: 1}}); err == nil {
		t.Fatal("missing renewal permission accepted")
	}
}

func TestInstanceWSSDeadlineCancelsBlockedMutation(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	srv, _ := channelServer(t, f.b)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := pgx.Connect(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM agent_registration_scopes WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3 FOR UPDATE`, f.g.Scope.WorkspaceID, f.g.Scope.EnvironmentID, f.g.Scope.ServiceKey); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_, err = c.Register(bounded, "bounded-register", f.request)
	cancel()
	remote, remoteOK := errors.AsType[*client.RemoteError](err)
	if !errors.Is(err, context.DeadlineExceeded) && !(remoteOK && remote.Detail.Code == agentv1.ErrorCode_ERROR_CODE_DEADLINE_EXCEEDED) {
		t.Fatal("blocked command ignored caller deadline", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The timed-out attempt must not commit after its lock becomes available.
	registered, err := c.Register(t.Context(), "post-timeout", f.request)
	if err != nil || registered.Instance.Lease.Epoch != 1 {
		t.Fatal("cancelled attempt changed registration", err)
	}
}
