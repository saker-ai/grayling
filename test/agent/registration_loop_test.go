//go:build integration

package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestRegisteredLoopReplicaReconnectAndIndependentRenewal(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	// Lease/link state must not manufacture Ready for the registered process.
	f.request.Instance.Ready = false
	first, _ := channelServer(t, f.a)
	second, _ := channelServer(t, f.b)
	owner, err := client.NewInstanceRegistration(f.request)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	var sessions atomic.Int64
	var firstLeaseID string
	var firstEpoch uint64
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	err = client.RunRegistered(ctx, func(context.Context) (client.Options, error) {
		server := first
		if attempts.Add(1) > 1 {
			server = second
		}
		return client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}, nil
	}, owner, func(ctx context.Context, c *client.Client) error {
		state := owner.State()
		if !state.Connected || !state.LeaseValid || state.Instance == nil || state.Instance.Ready {
			return fmt.Errorf("registration state invalid")
		}
		if sessions.Add(1) == 1 {
			firstLeaseID = state.Instance.Lease.Id
			firstEpoch = state.Instance.Lease.Epoch
			c.Close()
			return c.Err()
		}
		if state.Instance.Lease.Id != firstLeaseID || state.Instance.Lease.Epoch != firstEpoch {
			return fmt.Errorf("reconnect replaced lease")
		}
		revision, expiry := state.DiscoveryRevision, state.Instance.Lease.ExpiresUnixMs
		// The callback does no work while the real default renewal configuration
		// runs independently (20-second upper bound, earlier near expiry). No
		// shortened server fixture interval is used.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				state = owner.State()
				if state.DiscoveryRevision > revision {
					if !state.LeaseValid || state.Instance.Ready || state.Instance.Lease.Id != firstLeaseID || state.Instance.Lease.Epoch != firstEpoch || state.Instance.Lease.ExpiresUnixMs <= expiry {
						return fmt.Errorf("renewal changed identity/readiness or failed to extend lease")
					}
					return nil
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || sessions.Load() != 2 {
		t.Fatal("unexpected reconnect count", attempts.Load(), sessions.Load())
	}
	state := owner.State()
	if state.Connected || !state.LeaseValid {
		t.Fatal("final state invalid", state)
	}
	rows := pgtest.Query(t, f.dsn, `SELECT action,count(*)::text FROM agent_instance_operations GROUP BY action ORDER BY action`)
	if fmt.Sprint(rows) != "[[register 1] [renew 2]]" {
		t.Fatal("reconnect registered again or missed renewal", rows)
	}
}

func TestRegisteredLoopShutdownStopsCallback(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	server, channel := channelServer(t, f.a)
	owner, err := client.NewInstanceRegistration(f.request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- client.RunRegistered(ctx, func(context.Context) (client.Options, error) {
			return client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}, nil
		}, owner, func(ctx context.Context, c *client.Client) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("runner not registered")
	}
	// Server shutdown sends GoAway and cancels the callback before reconnect.
	if err := channel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback survived cancellation")
	}
	if owner.State().Connected {
		t.Fatal("shutdown left connected state")
	}
}

func TestRegisteredLoopCredentialRevocationStopsCallback(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	server, _ := channelServer(t, f.a)
	owner, err := client.NewInstanceRegistration(f.request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- client.RunRegistered(ctx, func(context.Context) (client.Options, error) {
			return client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}, nil
		}, owner, func(ctx context.Context, c *client.Client) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("runner not registered")
	}
	if err := f.a.RevokeCredential(ctx, f.session.Grant.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		remote, ok := errors.AsType[*client.RemoteError](err)
		handshake, hOK := errors.AsType[*client.HandshakeError](err)
		if !(ok && (remote.Detail.Code == agentv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED || remote.Detail.Code == agentv1.ErrorCode_ERROR_CODE_FENCED)) && !(hOK && handshake.StatusCode == 401) {
			t.Fatal("revoked runner retried indefinitely or lost rejection", err)
		}
	case <-ctx.Done():
		t.Fatal("revoked runner did not stop")
	}
	if owner.State().Connected {
		t.Fatal("revocation left connected state")
	}
}
