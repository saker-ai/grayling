//go:build integration

package agent_test

import (
	"context"
	"github.com/godeps/gonacos/pkg/server"
	"path/filepath"
	"sync"
	"testing"
	"time"

	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestInstanceNamingScopePagination(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	pgtest.Exec(t, f.dsn, `INSERT INTO agent_registration_scopes(workspace_id,environment_id,service_key,policy,policy_revision,discovery_revision) SELECT workspace_id,environment_id,'scan-'||lpad(n::text,3,'0'),policy,policy_revision,discovery_revision FROM agent_registration_scopes CROSS JOIN generate_series(1,101) n WHERE workspace_id=$1 AND environment_id=$2 AND service_key=$3`, f.g.Scope.WorkspaceID, f.g.Scope.EnvironmentID, f.g.Scope.ServiceKey)
	first, err := f.a.RegistrationScopes(t.Context(), client.Scope{})
	if err != nil || len(first) != 100 {
		t.Fatal("first bounded scope page", len(first), err)
	}
	second, err := f.b.RegistrationScopes(t.Context(), first[len(first)-1])
	if err != nil || len(second) != 2 {
		t.Fatal("second bounded scope page", len(second), err)
	}
	seen := map[client.Scope]bool{}
	for _, page := range [][]client.Scope{first, second} {
		for _, scope := range page {
			if seen[scope] {
				t.Fatal("duplicate scope")
			}
			seen[scope] = true
		}
	}
	end, err := f.a.RegistrationScopes(t.Context(), second[len(second)-1])
	if err != nil || len(end) != 0 {
		t.Fatal("scope scan did not terminate", err)
	}
	if _, err := f.a.RegistrationScopes(t.Context(), client.Scope{WorkspaceID: "partial"}); err == nil {
		t.Fatal("partial cursor accepted")
	}
}

type blockedNaming struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockedNaming) Upsert(ctx context.Context, _ control.NamingIntent) error {
	close(b.entered)
	<-b.release // models a substrate callback that cannot be interrupted
	return ctx.Err()
}
func (*blockedNaming) Remove(ctx context.Context, _ control.NamingIntent) error { return ctx.Err() }
func (*blockedNaming) Verify(ctx context.Context, _ control.NamingIntent) error { return ctx.Err() }

func TestInstanceNamingWorkerWaitsForInFlightCallback(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	registerInstance(t, f, "worker")
	backend := &blockedNaming{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	release := sync.OnceFunc(func() { close(backend.release) })
	go func() { defer close(done); f.a.RunNaming(ctx, backend, nil) }()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after substrate returned")
		}
	})
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not recover durable intent")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("worker abandoned an in-flight callback")
	default:
	}
	// The owner retains live resources until the structured worker returns.
	if _, err := f.b.GetRegistrationPolicy(t.Context(), f.g.Scope); err != nil {
		t.Fatal("scope unavailable during drain", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker failed to stop")
	}
}

func TestInstanceNamingWorkerRestoresConfirmedProjectionAfterRestart(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	registered := registerInstance(t, f, "restart-projection")
	makeBackend := func(name string) (*server.Server, *control.EmbeddedNaming) {
		t.Helper()
		srv, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"), server.WithDataDir(filepath.Join(t.TempDir(), name)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		})
		backend, err := control.NewEmbeddedNaming(srv.Services().Naming)
		if err != nil {
			t.Fatal(err)
		}
		return srv, backend
	}
	_, first := makeBackend("first")
	result, err := f.a.ReconcileNaming(t.Context(), f.g.Scope, first, 1)
	if err != nil || result.Confirmed != 1 {
		t.Fatal("initial projection failed", result, err)
	}
	// A fresh substrate has no projection even though governance has confirmed
	// it. Periodic refresh must restore it without a new registration operation.
	fresh, backend := makeBackend("replacement")
	if fresh.Services().Naming.CountAllInstances() != 0 {
		t.Fatal("fresh substrate unexpectedly retained members")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.b.RunNaming(ctx, backend, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recovery worker did not stop")
		}
	})
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for fresh.Services().Naming.CountAllInstances() != 1 {
		select {
		case <-deadline.C:
			t.Fatal("confirmed projection not recovered by periodic refresh")
		case <-tick.C:
		}
	}
	cancel()
	<-done
	if err := backend.Verify(t.Context(), control.NamingIntent{Scope: f.g.Scope, Instance: registered.Instance, Revision: registered.DiscoveryRevision, Present: true}); err != nil {
		t.Fatal("restored projection does not match immutable governance", err)
	}
	snapshot, err := f.a.DiscoveryCandidates(t.Context(), f.session)
	if err != nil || len(snapshot.Members) != 1 || !snapshot.Members[0].NamingConfirmed {
		t.Fatal("restart changed governed incarnation", err)
	}
}
