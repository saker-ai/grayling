//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"google.golang.org/protobuf/proto"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func proofPublisher(t *testing.T, s *control.Sessions, lifetime time.Duration) *control.Publisher {
	t.Helper()
	p, err := control.NewPublisher(s, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRevocationProofScanBoundedAndIndependentOfRegistration(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, v := publisher(t, s)
	for i := range 102 {
		scope := grant().Scope
		scope.ServiceKey = fmt.Sprintf("scope-%03d", i)
		if _, err := p.RevokeBundles(t.Context(), scope, adminMutation("initial"), control.RevokeBundlesRequest{BundleHashes: []string{fmt.Sprintf("%064x", i+1)}, Reason: "pagination fixture"}, v); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.DueRevocationProofs(t.Context(), client.Scope{}, time.Hour)
	if err != nil || len(first) != 100 {
		t.Fatal(len(first), err)
	}
	second, err := s.DueRevocationProofs(t.Context(), first[len(first)-1].Scope, time.Hour)
	if err != nil || len(second) != 2 {
		t.Fatal(len(second), err)
	}
	if second[0].Scope.ServiceKey != "scope-100" || second[1].Scope.ServiceKey != "scope-101" {
		t.Fatal(second)
	}
	if next, err := s.DueRevocationProofs(t.Context(), second[1].Scope, time.Hour); err != nil || len(next) != 0 {
		t.Fatal(next, err)
	}
	if _, err := s.DueRevocationProofs(t.Context(), client.Scope{WorkspaceID: "partial"}, time.Hour); err == nil {
		t.Fatal("invalid keyset cursor accepted")
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_registration_scopes`)[0][0]; n != "0" {
		t.Fatal("test unexpectedly depends on registration scopes", n)
	}
}
func TestRevocationProofWorkersReplicaDeduplicateAndStop(t *testing.T) {
	f := newRegistrationFixture(t, 1)
	_, v := publisher(t, f.a)
	a, b := proofPublisher(t, f.a, 3*time.Second), proofPublisher(t, f.b, 3*time.Second)
	initial, err := a.RevokeBundles(t.Context(), f.g.Scope, adminMutation("initial-proof"), control.RevokeBundlesRequest{BundleHashes: []string{fmt.Sprintf("%064x", 1)}, Reason: "periodic proof test"}, v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, p := range []*control.Publisher{a, b} {
		workers.Go(func() { results <- p.RunRevocationRefresh(ctx, v, nil) })
	}
	defer func() { cancel(); workers.Wait() }()
	deadline := time.NewTimer(9 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		resource, err := f.a.RevocationResource(t.Context(), f.session, 0, v)
		if err == nil && resource.Revision >= 3 {
			d, c, err := client.VerifyRevocation(v, signed(t, resource.Body), f.g.Scope, client.Watermark{}, time.Now())
			if err != nil || d.Epoch != 1 || c.Revision != resource.Revision || !bytes.Equal(signed(t, initial.Body).Payload, signed(t, resource.Body).Payload) {
				t.Fatal("worker changed denial content", d, c, err)
			}
			n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_admin_audit WHERE actor_id='system-agent-revocation-refresh'`)[0][0]
			if n != fmt.Sprint(resource.Revision-1) {
				t.Fatal("replicas minted duplicate audit effects", n, resource.Revision)
			}
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("workers did not renew proof repeatedly", err)
		}
	}
	cancel()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("proof worker did not stop")
		}
	}
}
func TestRevocationProofWorkerDoesNotStarvePastLockedScopes(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	_, v := publisher(t, s)
	p := proofPublisher(t, s, time.Second)
	base := grant().Scope
	for _, name := range []string{"a0", "a1", "a2", "a3", "z-healthy"} {
		scope := base
		scope.ServiceKey = name
		if _, err := p.RevokeBundles(t.Context(), scope, adminMutation("initial"), control.RevokeBundlesRequest{BundleHashes: []string{fmt.Sprintf("%064x", 1)}, Reason: "fairness test"}, v); err != nil {
			t.Fatal(err)
		}
	}
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `UPDATE agent_resource_heads SET revision=revision WHERE resource_key='revocation/state' AND service_key=ANY($1)`, []string{"a0", "a1", "a2", "a3"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.RunRevocationRefresh(ctx, v, nil) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker failed to cancel locked queries")
		}
	}()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		n := pgtest.Query(t, dsn, `SELECT revision::text FROM agent_resource_heads WHERE resource_key='revocation/state' AND service_key='z-healthy'`)[0][0]
		if n != "1" {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("locked prefix starved later healthy scope")
		}
	}
}

func TestRevocationProofWSSRefreshPreservesInstalledSource(t *testing.T) {
	f, p, v, _ := activationFixture(t)
	if _, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("deny-other"), control.RevokeBundlesRequest{BundleHashes: []string{fmt.Sprintf("%064x", 1)}, Reason: "unrelated denial"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate", "release.revoke"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var builds atomic.Int64
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{47}, 32), Verifier: v, Now: time.Now, Builder: func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
		builds.Add(1)
		return client.RuntimeResources{Handler: http.NotFoundHandler(), Close: func() error { return nil }}, nil
	}}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	first := clientEvent(t, c)
	if first.GetRevoke() == nil {
		t.Fatal("denial proof not sent before activation", first)
	}
	if err = c.ApplyRevocation(t.Context(), first, runtime); err != nil {
		t.Fatal(err)
	}
	activate := clientEvent(t, c)
	result, err := c.InstallActivation(t.Context(), activate, runtime)
	if err != nil {
		t.Fatal(err)
	}
	oldRef, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, Key: client.RevocationKey, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("renew-proof"), control.RevokeBundlesRequest{ExpectedEpoch: 1, ExpectedArtifactRevision: 1, RefreshOnly: true, Reason: "refresh delivery proof"}, v)
	if err != nil {
		t.Fatal(err)
	}
	oldProof, err := c.Download(t.Context(), oldRef)
	if err != nil {
		t.Fatal("proof renewal invalidated an outstanding exact download", err)
	}
	_, oldClaims, err := client.VerifyRevocation(v, oldProof, f.g.Scope, client.Watermark{}, time.Now())
	if err != nil || oldClaims.Revision != 1 {
		t.Fatal("historical download substituted a newer proof", oldClaims, err)
	}
	next := clientEvent(t, c)
	if next.GetRevoke() == nil || next.GetRevoke().Epoch != 1 || next.GetRevoke().View.Revision != refreshed.Revision || next.OperationId == first.OperationId {
		t.Fatal("proof update was not independently delivered", next)
	}
	if err = c.ApplyRevocation(t.Context(), next, runtime); err != nil {
		t.Fatal(err)
	}
	floor, err := runtime.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 || floor.ArtifactRevision != 2 {
		t.Fatal(floor, err)
	}
	current, err := runtime.Applied()
	if err != nil || !proto.Equal(current, result.Applied) {
		t.Fatal("proof-only refresh changed installed source", current, err)
	}
	replay, err := c.InstallActivation(t.Context(), activate, runtime)
	if err != nil || builds.Load() != 1 || !proto.Equal(replay.Receipt, result.Receipt) {
		t.Fatal("proof refresh forced a rebuild or new Applied outcome", replay, err)
	}
	if _, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("change-denial-content"), control.RevokeBundlesRequest{ExpectedEpoch: 1, ExpectedArtifactRevision: 2, BundleHashes: []string{fmt.Sprintf("%064x", 2)}, Reason: "new denial"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Download(t.Context(), oldRef); err == nil {
		t.Fatal("old proof remained downloadable after denial content changed")
	}
}
