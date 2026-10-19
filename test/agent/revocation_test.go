//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func TestRevocationAtomicCatalogReplicaReplayAndAuthority(t *testing.T) {
	f, v, pending := appliedFixture(t)
	p, _ := publisher(t, f.a)
	replica, _ := publisher(t, f.b)
	actor := adminMutation("revoke-bundle")
	request := control.RevokeBundlesRequest{BundleHashes: []string{pending.Release.Definition.BundleHash}, Reason: "compromised application resource"}
	results := make(chan control.StoredResource, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			node := p
			if i%2 != 0 {
				node = replica
			}
			resource, err := node.RevokeBundles(t.Context(), f.g.Scope, actor, request, v)
			if err != nil {
				t.Error(err)
				return
			}
			results <- resource
		})
	}
	wg.Wait()
	close(results)
	var first control.StoredResource
	for result := range results {
		if first.Revision == 0 {
			first = result
		}
		if result.Revision != 1 || !bytes.Equal(first.Body, result.Body) {
			t.Fatal("duplicate changed revocation")
		}
	}
	if first.Revision == 0 {
		t.Fatal("no committed revocation")
	}
	resource, err := f.b.RevocationResource(t.Context(), f.session, 1, v)
	if err != nil || !bytes.Equal(first.Body, resource.Body) {
		t.Fatal("replica lost catalog", err)
	}
	d, _, err := client.VerifyRevocation(v, signed(t, resource.Body), f.g.Scope, client.Watermark{}, time.Now())
	if err != nil || !slices.Equal(d.BundleHashes, request.BundleHashes) || len(d.CancelledHashes) != 0 {
		t.Fatal(d, err)
	}
	if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_admin_audit WHERE action='agent.bundle.revoke'`)[0][0]; n != "1" {
		t.Fatal("audit not atomic/deduplicated", n)
	}
	if _, err = f.a.RecordApplied(t.Context(), f.session, pending.Source.CommandID, appliedReport(f, pending), v); err == nil {
		t.Fatal("revoked bundle accepted Applied")
	}
	hash := activationBundleHash(t, pending.Release)
	if _, err = f.a.BundleResource(t.Context(), f.session, hash, 1); err == nil {
		t.Fatal("revoked bundle downloaded")
	}
	if _, err = f.a.Resource(t.Context(), f.session, "routing", 1); err == nil {
		t.Fatal("revoked policy downloaded")
	}
	if _, err = p.RefreshBundle(t.Context(), control.RefreshBundleRequest{Scope: f.g.Scope, PrincipalID: "owner", OperationID: "refresh-revoked", BundleHash: hash, ExpectedRevision: 1}, v); err == nil {
		t.Fatal("revoked bundle refreshed")
	}
	if _, err = p.RevokeBundles(t.Context(), f.g.Scope, actor, control.RevokeBundlesRequest{BundleHashes: request.BundleHashes, CancelActiveStreams: true, Reason: request.Reason}, v); !errors.Is(err, platform.ErrConflict) {
		t.Fatal("operation payload changed", err)
	}
	otherHash := sha256.Sum256([]byte("other-bundle"))
	other := hex.EncodeToString(otherHash[:])
	second, err := replica.RevokeBundles(t.Context(), f.g.Scope, adminMutation("revoke-other"), control.RevokeBundlesRequest{ExpectedEpoch: 1, BundleHashes: []string{other}, CancelActiveStreams: true, Reason: "urgent revocation"}, v)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err = client.VerifyRevocation(v, signed(t, second.Body), f.g.Scope, client.Watermark{}, time.Now())
	if err != nil || d.Epoch != 2 || len(d.BundleHashes) != 2 || !slices.Equal(d.CancelledHashes, []string{other}) {
		t.Fatal("cumulative catalog or graceful cancellation changed", d, err)
	}
	replay, err := p.RevokeBundles(t.Context(), f.g.Scope, actor, request, v)
	if err != nil || !bytes.Equal(replay.Body, first.Body) {
		t.Fatal("historical receipt changed", err)
	}
	if _, err = f.b.RevocationResource(t.Context(), f.session, 1, v); err == nil {
		t.Fatal("old catalog delivered as current authority")
	}
}
func TestRevocationWSSSignedDenialCancelsRuntimeAndReconnects(t *testing.T) {
	f, v, pending := appliedFixture(t)
	server, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate", "release.revoke"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	entered := make(chan struct{})
	finished := make(chan error, 1)
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{45}, 32), Verifier: v, Now: time.Now}
	o.Builder = func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
		return client.RuntimeResources{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }), Close: func() error { return nil }}, nil
	}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	if _, err = c.InstallActivation(t.Context(), event, runtime); err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	go func() {
		finished <- lease.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://checkout/stream", nil))
	}()
	<-entered
	p, _ := publisher(t, f.b)
	revokeStarted := time.Now()
	if _, err = p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("urgent-denial"), control.RevokeBundlesRequest{BundleHashes: []string{pending.Release.Definition.BundleHash}, CancelActiveStreams: true, Reason: "security incident"}, v); err != nil {
		t.Fatal(err)
	}
	revoke := clientEvent(t, c)
	if revoke.GetRevoke() == nil || revoke.GetRevoke().View == nil {
		t.Fatal("no signed revoke event", revoke)
	}
	forged := proto.Clone(revoke).(*agentv1.Envelope)
	forged.GetRevoke().CancelActiveStreams = false
	if err = c.ApplyRevocation(t.Context(), forged, runtime); err == nil {
		t.Fatal("unsigned frame changed cancellation")
	}
	if err = c.ApplyRevocation(t.Context(), revoke, runtime); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("signed urgent revoke did not cancel active handler")
	}
	elapsed := time.Since(revokeStarted)
	t.Logf("connected publication-to-runtime cancellation: %s", elapsed)
	if elapsed > 5*time.Second {
		t.Fatal("connected revocation exceeded 5s component budget", elapsed)
	}
	if _, err = lease.Metadata(); !errors.Is(err, client.ErrPolicyRevoked) {
		t.Fatal("old capture escaped denial", err)
	}
	lease.Release()
	c.Close()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	replay := clientEvent(t, successor)
	if replay.GetRevoke() == nil || replay.GetRevoke().Epoch != 1 {
		t.Fatal("reconnect lost cumulative denial", replay)
	}
	if err = successor.ApplyRevocation(t.Context(), replay, runtime); err != nil {
		t.Fatal("exact denial replay", err)
	}
}

func TestRevocationSuppressedWritesRollbackCatalogAndReceipt(t *testing.T) {
	for _, table := range []string{"agent_resources", "agent_resource_heads", "agent_admin_audit", "agent_admin_operations"} {
		t.Run(table, func(t *testing.T) {
			f, v, pending := appliedFixture(t)
			p, _ := publisher(t, f.a)
			// Head and operation placeholders must exist before suppressing their final
			// updates, while audit/resource suppression affects the insert itself.
			event := "INSERT"
			if table == "agent_resource_heads" || table == "agent_admin_operations" {
				event = "UPDATE"
			}
			pgtest.Exec(t, f.dsn, `CREATE FUNCTION suppress_revocation() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; CREATE TRIGGER suppress_revocation BEFORE `+event+` ON `+table+` FOR EACH ROW EXECUTE FUNCTION suppress_revocation()`)
			q := control.RevokeBundlesRequest{BundleHashes: []string{pending.Release.Definition.BundleHash}, Reason: "atomic rollback test"}
			actor := adminMutation("revoke-rollback")
			if _, err := p.RevokeBundles(t.Context(), f.g.Scope, actor, q, v); err == nil {
				t.Fatal("suppressed write acknowledged", table)
			}
			if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_resources WHERE resource_key='revocation/state'`)[0][0]; n != "0" {
				t.Fatal("partial catalog committed", n)
			}
			if n := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_admin_audit WHERE action='agent.bundle.revoke'`)[0][0]; n != "0" {
				t.Fatal("partial audit committed", n)
			}
			pgtest.Exec(t, f.dsn, `DROP TRIGGER suppress_revocation ON `+table+`; DROP FUNCTION suppress_revocation()`)
			if _, err := p.RevokeBundles(t.Context(), f.g.Scope, actor, q, v); err != nil {
				t.Fatal("same operation not retryable after rollback", err)
			}
		})
	}
}

func TestRevocationDeliveryProofRefreshPreservesExpiredDenials(t *testing.T) {
	f, v, pending := appliedFixture(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32))
	p, err := control.NewPublisher(f.a, "policy-test-key", key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	original, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("short-denial"), control.RevokeBundlesRequest{BundleHashes: []string{pending.Release.Definition.BundleHash}, CancelActiveStreams: true, Reason: "short proof test"}, v)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(original.ExpiresAt) + 10*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if _, err = f.b.RevocationResource(t.Context(), f.session, 0, v); !errors.Is(err, client.ErrExpired) {
		t.Fatal("expired delivery proof accepted", err)
	}
	if _, err = f.b.BundleResource(t.Context(), f.session, activationBundleHash(t, pending.Release), 1); err == nil {
		t.Fatal("proof expiry erased authoritative denial")
	}
	refreshed, err := p.RefreshRevocation(t.Context(), f.g.Scope, adminMutation("refresh-denial-proof"), 1, v)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := client.VerifyRevocation(v, signed(t, refreshed.Body), f.g.Scope, client.Watermark{}, time.Now())
	if err != nil || d.Epoch != 1 || refreshed.Revision != 2 || !slices.Equal(d.BundleHashes, []string{pending.Release.Definition.BundleHash}) || !slices.Equal(d.CancelledHashes, d.BundleHashes) {
		t.Fatal("refresh cleared or downgraded denial", d, err)
	}
	replay, err := p.RefreshRevocation(t.Context(), f.g.Scope, adminMutation("refresh-denial-proof"), 1, v)
	if err != nil || !bytes.Equal(replay.Body, refreshed.Body) {
		t.Fatal("proof refresh retry changed", err)
	}
}

func TestRevocationActivationBootstrapsExactNonzeroCatalog(t *testing.T) {
	f, p, v, _ := activationFixture(t)
	digest := sha256.Sum256([]byte("previously-revoked-other-bundle"))
	hash := hex.EncodeToString(digest[:])
	if _, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("previous-denial"), control.RevokeBundlesRequest{BundleHashes: []string{hash}, Reason: "existing catalog"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	// Bootstrapping exact revocation authority must not depend on receiving a
	// proactive event or on an application having separately handled that event.
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	if event.GetActivate() == nil || event.GetActivate().RevocationEpoch != 1 {
		t.Fatal("activation omitted current denial epoch", event)
	}
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{46}, 32), Verifier: v, Now: time.Now, Builder: func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
		return client.RuntimeResources{Handler: http.NotFoundHandler(), Close: func() error { return nil }}, nil
	}}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	result, err := c.InstallActivation(t.Context(), event, runtime)
	if err != nil || result.Applied == nil || result.Applied.RevocationEpoch != 1 || result.Receipt == nil {
		t.Fatal("nonzero catalog installation failed", result, err)
	}
	floor, err := runtime.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 {
		t.Fatal("activation did not persist required denial proof", floor, err)
	}
	if _, err = p.RefreshRevocation(t.Context(), f.g.Scope, adminMutation("newer-proof-epoch"), 1, v); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReportApplied(t.Context(), result.CommandID, result.Applied); err != nil {
		t.Fatal("proof-only refresh invalidated unchanged activation authority", err)
	}
	if _, err = p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("changed-content-after-refresh"), control.RevokeBundlesRequest{ExpectedEpoch: 1, BundleHashes: []string{strings.Repeat("d", 64)}, Reason: "new semantic denial"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReportApplied(t.Context(), result.CommandID, result.Applied); err == nil {
		t.Fatal("old semantic epoch accepted after denial catalog changed")
	}
}

func TestRevocationBlocksPreemptivelyDeniedPublication(t *testing.T) {
	dsn := pgtest.DSN(t)
	sessions := openSessions(t, dsn)
	p, v := publisher(t, sessions)
	request := bundlePairRequest(t)
	body, err := client.MarshalBundle(request.Document)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	text := hex.EncodeToString(hash[:])
	if _, err = p.RevokeBundles(t.Context(), request.Document.Scope, adminMutation("preemptive-denial"), control.RevokeBundlesRequest{BundleHashes: []string{text}, Reason: "compromised prepared artifact"}, v); err != nil {
		t.Fatal(err)
	}
	if _, err = p.PublishBundle(t.Context(), request); err == nil {
		t.Fatal("denied Bundle minted new policy/Bundle authorization")
	}
	if _, err = p.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: request.Document.Scope, PrincipalID: "approved-publisher", OperationID: "denied-policy", Key: "routing", BundleHash: hash, Payload: request.PolicyPayload}); err == nil {
		t.Fatal("denied Bundle minted new policy authorization")
	}
	if n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources WHERE resource_key<>'revocation/state'`)[0][0]; n != "0" {
		t.Fatal("denied publication left a positive artifact", n)
	}
}
