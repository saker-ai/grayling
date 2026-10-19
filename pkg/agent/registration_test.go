package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

func registrationRequest() *agentv1.Register {
	return &agentv1.Register{Instance: &agentv1.Instance{Scope: &agentv1.Scope{WorkspaceId: "ws", EnvironmentId: "env", ServiceKey: "svc"}, InstanceId: "instance", BootId: "boot", BuildId: "build", ImageDigest: "sha256:" + strings.Repeat("a", 64), Address: "10.0.0.1", Port: 8443, TlsServerName: "service.internal", Zone: "az", ProtocolVersion: 1}, DeploymentAuthorization: []byte("deployment")}
}
func registrationWelcome(e *agentv1.Envelope) {
	w := e.GetWelcome()
	w.Capabilities = []string{"instance.register", "instance.renew"}
	w.Limits.LeaseTtlMs = 10000
	w.Limits.LeaseRenewIntervalMs = 100
}
func registrationReply(e *agentv1.Envelope, body proto.Message) *agentv1.Envelope {
	response := new(agentv1.Envelope)
	switch value := body.(type) {
	case *agentv1.Registered:
		response.Payload = &agentv1.Envelope_Registered{Registered: value}
	case *agentv1.LeaseRenewed:
		response.Payload = &agentv1.Envelope_LeaseRenewed{LeaseRenewed: value}
	}
	response.ProtocolMajor = 1
	response.SessionId = "session"
	response.ConnectionEpoch = 2
	response.MessageId = "reply"
	response.ReplyTo = e.MessageId
	response.OperationId = e.OperationId
	return response
}

func TestRunRegisteredReplaysUnknownRegisterAndRenew(t *testing.T) {
	var mu sync.Mutex
	var registration *agentv1.Register
	var renewal *agentv1.RenewLease
	var registerID, renewID string
	var registered *agentv1.Registered
	registerCalls, renewCalls := 0, 0
	var callbacks atomic.Int64
	o := serverOptions(t, func(e *agentv1.Envelope) { registrationWelcome(e); e.GetWelcome().Limits.LeaseTtlMs = 60000 }, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		if request := e.GetRegister(); request != nil {
			registerCalls++
			if registration == nil {
				registration = proto.Clone(request).(*agentv1.Register)
				registerID = e.OperationId
				instance := proto.Clone(request.Instance).(*agentv1.Instance)
				instance.Lease = &agentv1.Lease{Id: "lease", Epoch: 1, ExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()}
				registered = &agentv1.Registered{Instance: instance, DiscoveryRevision: 1}
				_ = c.CloseNow()
				return
			}
			if registerID != e.OperationId || !proto.Equal(registration, request) {
				t.Error("register retry changed identity")
			}
			writePeer(t, ctx, c, registrationReply(e, registered))
			return
		}
		if request := e.GetRenewLease(); request != nil {
			renewCalls++
			if renewal == nil {
				renewal = proto.Clone(request).(*agentv1.RenewLease)
				renewID = e.OperationId
				_ = c.CloseNow()
				return
			}
			if renewID != e.OperationId || !proto.Equal(renewal, request) {
				t.Error("renew retry changed identity")
			}
			lease := proto.Clone(request.Lease).(*agentv1.Lease)
			lease.ExpiresUnixMs = time.Now().Add(time.Minute).UnixMilli()
			writePeer(t, ctx, c, registrationReply(e, &agentv1.LeaseRenewed{Lease: lease, DiscoveryRevision: 2}))
		}
	})
	owner, err := NewInstanceRegistration(registrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	err = RunRegistered(ctx, func(context.Context) (Options, error) { return o, nil }, owner, func(ctx context.Context, c *Client) error {
		if callbacks.Add(1) == 2 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if registerCalls != 2 || renewCalls != 2 || callbacks.Load() != 2 {
		t.Fatal("pending mutations not replayed exactly", registerCalls, renewCalls, callbacks.Load())
	}
	state := owner.State()
	if !state.LeaseValid || state.Connected || state.DiscoveryRevision != 2 || state.Instance.Lease.Epoch != 1 || state.Instance.Ready {
		t.Fatal("registration promoted readiness or lost state", state)
	}
	state.Instance.Lease.Id = "modified"
	if owner.State().Instance.Lease.Id != "lease" {
		t.Fatal("state leaked mutable alias")
	}
}

func TestRunRegisteredRejectsIdentityAndLeaseReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*agentv1.Instance)
	}{
		{"boot", func(i *agentv1.Instance) { i.BootId = "other" }},
		{"readiness", func(i *agentv1.Instance) { i.Ready = true }},
		{"expired", func(i *agentv1.Instance) { i.Lease.ExpiresUnixMs = time.Now().UnixMilli() }},
		{"oversized lifetime", func(i *agentv1.Instance) { i.Lease.ExpiresUnixMs = time.Now().Add(time.Hour).UnixMilli() }},
		{"zero epoch", func(i *agentv1.Instance) { i.Lease.Epoch = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := serverOptions(t, registrationWelcome, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
				i := proto.Clone(e.GetRegister().Instance).(*agentv1.Instance)
				i.Lease = &agentv1.Lease{Id: "lease", Epoch: 1, ExpiresUnixMs: time.Now().Add(10 * time.Second).UnixMilli()}
				tc.edit(i)
				writePeer(t, ctx, c, registrationReply(e, &agentv1.Registered{Instance: i, DiscoveryRevision: 1}))
			})
			owner, err := NewInstanceRegistration(registrationRequest())
			if err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err = RunRegistered(bounded, func(context.Context) (Options, error) { return o, nil }, owner, func(context.Context, *Client) error { t.Error("invalid lease reached application"); return nil })
			if err == nil || owner.State().LeaseValid {
				t.Fatal("invalid registration accepted", err)
			}
		})
	}
}

func TestRunRegisteredCancellationWaitsForCallback(t *testing.T) {
	o := serverOptions(t, registrationWelcome, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		if e.GetRegister() == nil {
			return
		}
		i := proto.Clone(e.GetRegister().Instance).(*agentv1.Instance)
		i.Lease = &agentv1.Lease{Id: "lease", Epoch: 1, ExpiresUnixMs: time.Now().Add(10 * time.Second).UnixMilli()}
		writePeer(t, ctx, c, registrationReply(e, &agentv1.Registered{Instance: i, DiscoveryRevision: 1}))
	})
	owner, err := NewInstanceRegistration(registrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	go func() {
		done <- RunRegistered(ctx, func(context.Context) (Options, error) { return o, nil }, owner, func(ctx context.Context, c *Client) error { close(entered); <-ctx.Done(); <-release; return ctx.Err() })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not started")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("returned before callback drained", err)
	case <-time.After(30 * time.Millisecond):
	}
	err = RunRegistered(t.Context(), func(context.Context) (Options, error) { t.Error("overlapping runner dialed"); return o, nil }, owner, func(context.Context, *Client) error { return nil })
	if !errors.Is(err, ErrBackpressure) {
		t.Fatal("owner replaced while callback blocked", err)
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not drain")
	}
	if owner.State().Connected {
		t.Fatal("runner remained connected")
	}
}

func TestRunRegisteredZeroOwnerDoesNotDial(t *testing.T) {
	err := RunRegistered(t.Context(), func(context.Context) (Options, error) { t.Error("invalid owner dialed"); return Options{}, nil }, new(InstanceRegistration), func(context.Context, *Client) error { return nil })
	if !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

func TestRunRegisteredSameExpiryCannotKeepLeaseAlive(t *testing.T) {
	var expiry atomic.Int64
	var renewals atomic.Uint64
	o := serverOptions(t, registrationWelcome, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) {
		if e.GetRegister() != nil {
			i := proto.Clone(e.GetRegister().Instance).(*agentv1.Instance)
			expiry.Store(time.Now().Add(6 * time.Second).UnixMilli())
			i.Lease = &agentv1.Lease{Id: "lease", Epoch: 1, ExpiresUnixMs: expiry.Load()}
			writePeer(t, ctx, c, registrationReply(e, &agentv1.Registered{Instance: i, DiscoveryRevision: 1}))
			return
		}
		if e.GetRenewLease() != nil {
			lease := proto.Clone(e.GetRenewLease().Lease).(*agentv1.Lease)
			lease.ExpiresUnixMs = expiry.Load()
			writePeer(t, ctx, c, registrationReply(e, &agentv1.LeaseRenewed{Lease: lease, DiscoveryRevision: renewals.Add(1) + 1}))
		}
	})
	owner, err := NewInstanceRegistration(registrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = RunRegistered(ctx, func(context.Context) (Options, error) { return o, nil }, owner, func(ctx context.Context, c *Client) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, ErrExpired) || owner.State().LeaseValid || owner.State().Connected {
		t.Fatal("unchanged expiry retained registration", err, owner.State())
	}
	if renewals.Load() == 0 || renewals.Load() > 15 {
		t.Fatal("renewal spun or did not run", renewals.Load())
	}
}
