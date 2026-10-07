package platform_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/test/pgtest"
)

func TestRolloutLeaseTakeoverAndFencing(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := platform.NewPGStore(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	ctx := t.Context()
	genA, acquired, err := a.AcquireRolloutLease(ctx, "replica-a", 200*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("A acquire: %d %v %v", genA, acquired, err)
	}
	if _, acquired, err := b.AcquireRolloutLease(ctx, "replica-b", time.Second); err != nil || acquired {
		t.Fatalf("B acquired during live A lease: %v %v", acquired, err)
	}
	if err := a.CheckRolloutLease(ctx, "replica-a", genA); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	genB, acquired, err := b.AcquireRolloutLease(ctx, "replica-b", time.Second)
	if err != nil || !acquired || genB <= genA {
		t.Fatalf("B takeover: %d %v %v", genB, acquired, err)
	}
	if err := a.CheckRolloutLease(ctx, "replica-a", genA); !errors.Is(err, platform.ErrRolloutLeaseLost) {
		t.Fatalf("stale A: %v", err)
	}
	if err := a.FinishRolloutLease(ctx, "replica-a", genA, "stale"); !errors.Is(err, platform.ErrRolloutLeaseLost) {
		t.Fatalf("stale A finish: %v", err)
	}
	if err := b.FinishRolloutLease(ctx, "replica-b", genB, ""); err != nil {
		t.Fatal(err)
	}
	status, err := a.RolloutLeadership(ctx)
	if err != nil || status.Holder != "replica-b" || status.Generation != genB || status.LastRunAt == nil || status.LastTakeoverAt == nil {
		t.Fatalf("leadership status: %+v, %v", status, err)
	}
	// Drop both pools' PostgreSQL sessions. The row survives connection loss;
	// reconnecting must not allow another holder through an unexpired lease.
	pgtest.Query(t, dsn, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, acquired, err = a.AcquireRolloutLease(ctx, "replica-a", time.Second)
		if err == nil {
			if acquired {
				t.Fatal("connection interruption stole a live lease")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease did not recover after session interruption: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRolloutLeaseDatabaseOutage(t *testing.T) {
	dsn := pgtest.DSN(t)
	url, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newPGCutoff(t, url.Host)
	url.Host = proxy.listener.Addr().String()
	a, err := platform.NewPGStore(t.Context(), url.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := platform.NewPGStore(t.Context(), url.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	gen, acquired, err := a.AcquireRolloutLease(t.Context(), "a", 300*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("A acquire: %d %v %v", gen, acquired, err)
	}
	proxy.setAvailable(false)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := a.CheckRolloutLease(ctx, "a", gen); err == nil {
		t.Fatal("database outage must fail closed")
	}
	if _, acquired, err := b.AcquireRolloutLease(ctx, "b", time.Second); err == nil || acquired {
		t.Fatalf("B elected without database: %v %v", acquired, err)
	}
	proxy.setAvailable(true)
	time.Sleep(350 * time.Millisecond)
	var newGen int64
	deadline := time.Now().Add(2 * time.Second)
	for {
		newGen, acquired, err = b.AcquireRolloutLease(t.Context(), "b", time.Second)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !acquired || newGen <= gen {
		t.Fatalf("B did not take over after outage: %d %v %v", newGen, acquired, err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		err = a.CheckRolloutLease(t.Context(), "a", gen)
		if errors.Is(err, platform.ErrRolloutLeaseLost) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(err, platform.ErrRolloutLeaseLost) {
		t.Fatalf("old holder after recovery: %v", err)
	}
}

type pgCutoff struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	available   bool
	connections map[net.Conn]struct{}
}

func newPGCutoff(t *testing.T, target string) *pgCutoff {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pgCutoff{listener: listener, target: target, available: true, connections: map[net.Conn]struct{}{}}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() { p.forward(client) })
		}
	})
	t.Cleanup(func() { _ = listener.Close(); p.setAvailable(false); workers.Wait() })
	return p
}

func (p *pgCutoff) setAvailable(available bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.available = available
	if !available {
		for conn := range p.connections {
			_ = conn.Close()
		}
	}
}

func (p *pgCutoff) forward(client net.Conn) {
	p.mu.Lock()
	if !p.available {
		p.mu.Unlock()
		_ = client.Close()
		return
	}
	p.mu.Unlock()
	server, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if !p.available {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.connections[client] = struct{}{}
	p.connections[server] = struct{}{}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(server, client); close(done) }()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	_ = server.Close()
	<-done
	p.mu.Lock()
	delete(p.connections, client)
	delete(p.connections, server)
	p.mu.Unlock()
}
