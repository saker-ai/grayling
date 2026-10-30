//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/godeps/gonacos/pkg/server"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func TestDiscoveryWSSRealNamingCrossReplicaAndImmutableDownload(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	a, v := publisher(t, f.a)
	b, _ := publisher(t, f.b)
	firstServer, _ := channelServerWithPublisher(t, f.b, b)
	opts := client.Options{URL: "wss" + strings.TrimPrefix(firstServer.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: firstServer.Client()}
	opts.Hello.RequiredCapabilities = []string{"discovery.read"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bundle := sha256.Sum256([]byte("routing-bundle"))
	cursor := &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle)}
	if err := c.Subscribe(t.Context(), "members", cursor); err != nil {
		t.Fatal(err)
	}
	initial := clientEvent(t, c)
	empty, claims, err := client.VerifyDiscovery(v, initial.GetSnapshot().GetInlineResource(), f.g.Scope, bundle, client.Watermark{}, time.Now())
	if err != nil || len(empty.Candidates()) != 0 || initial.StreamSeq != 1 {
		t.Fatal("initial full set unavailable", err)
	}
	if _, err := c.Register(t.Context(), "wss-discovery-register", f.request); err != nil {
		t.Fatal(err)
	}
	substrate, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"), server.WithDataDir(filepath.Join(t.TempDir(), "naming")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := substrate.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	backend, err := control.NewEmbeddedNaming(substrate.Services().Naming)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.ReconcileNaming(t.Context(), f.g.Scope, backend, 2); err != nil {
		t.Fatal(err)
	}
	var full *agentv1.Envelope
	for range 5 {
		event := clientEvent(t, c)
		if event.GetSnapshot() == nil {
			t.Fatalf("unexpected discovery event: %v", event)
		}
		discovered, next, err := client.VerifyDiscovery(v, event.GetSnapshot().GetInlineResource(), f.g.Scope, bundle, claims.Watermark(), time.Now())
		if err != nil || event.ResourceKind != cursor.Kind || event.StreamSeq <= initial.StreamSeq {
			t.Fatal("discovery snapshot invalid", err)
		}
		claims = next
		if len(discovered.Candidates()) == 1 {
			full = event
			break
		}
	}
	if full == nil {
		t.Fatal("real Naming member never reached WSS")
	}
	ref, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: cursor.Kind, Key: cursor.Key, Revision: claims.Revision})
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := c.Download(t.Context(), ref)
	if err != nil || !proto.Equal(downloaded, full.GetSnapshot().GetInlineResource()) {
		t.Fatal("HTTPS did not return immutable WSS artifact", err)
	}
	cacheOptions := client.DiscoveryCacheOptions{Directory: filepath.Join(t.TempDir(), "discovery-cache"), Identity: f.g.AgentID, Scope: f.g.Scope, AuthenticationKey: bytes.Repeat([]byte{0x31}, 32), Verifier: v, BundleHash: bundle}
	cached, err := client.InitializeDiscoveryCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := cached.Install(t.Context(), downloaded); err != nil {
		t.Fatal(err)
	}
	if err := cached.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := client.OpenDiscoveryCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	offline, err := reopened.Capture()
	if err != nil {
		t.Fatal(err)
	}
	offlineMembers, err := offline.Candidates()
	if err != nil || len(offlineMembers) != 1 || offline.Claims().Revision != claims.Revision {
		t.Fatal("verified network discovery did not survive cache reopen", err)
	}
	// Failover establishes a full-set barrier with a new stream sequence; the
	// immutable artifact identity and bytes survive node changes.
	otherServer, _ := channelServerWithPublisher(t, f.a, a)
	opts.URL = "wss" + strings.TrimPrefix(otherServer.URL, "https")
	opts.HTTPClient = otherServer.Client()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	if err := successor.Subscribe(t.Context(), "members", cursor); err != nil {
		t.Fatal(err)
	}
	recovered := clientEvent(t, successor)
	if recovered.StreamSeq != 1 || !proto.Equal(recovered.GetSnapshot().GetInlineResource(), downloaded) {
		t.Fatal("failover re-signed or changed the full snapshot")
	}
	// Only one discovery stream per connection is admitted.
	if err := successor.Subscribe(t.Context(), "extra", cursor); err != nil {
		t.Fatal(err)
	}
	denied := clientEvent(t, successor)
	if denied.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_RATE_LIMITED {
		t.Fatal("unbounded discovery subscriptions accepted")
	}
	// Old session download fencing is enforced at the original HTTPS origin.
	request, err := http.NewRequestWithContext(t.Context(), "GET", ref.HttpsUrl, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.secret)
	request.Header.Set("X-Grayling-Agent-Session", c.Welcome().SessionId)
	request.Header.Set("X-Grayling-Agent-Epoch", fmt.Sprint(c.Welcome().ConnectionEpoch))
	response, err := firstServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("superseded session downloaded immutable resource", response.StatusCode)
	}
	foreign := f.g
	foreign.Scope.EnvironmentID += "-foreign"
	foreign.Permissions = control.DiscoveryRead
	if err := f.a.SetScopeLimit(t.Context(), foreign.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, foreignSecret := issue(t, f.a, foreign)
	foreignSession, err := f.a.Connect(t.Context(), foreignSecret, helloFor(foreign))
	if err != nil {
		t.Fatal(err)
	}
	var missingBody []byte
	for _, target := range []string{ref.HttpsUrl, strings.Replace(ref.HttpsUrl, cursor.Key, client.DiscoveryKey(sha256.Sum256([]byte("unknown"))), 1)} {
		probe, err := http.NewRequestWithContext(t.Context(), "GET", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		probe.Header.Set("Authorization", "Bearer "+foreignSecret)
		probe.Header.Set("X-Grayling-Agent-Session", foreignSession.ID)
		probe.Header.Set("X-Grayling-Agent-Epoch", fmt.Sprint(foreignSession.Epoch))
		result, err := firstServer.Client().Do(probe)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(result.Body, 1024))
		result.Body.Close()
		if readErr != nil || result.StatusCode != http.StatusNotFound {
			t.Fatal("foreign artifact existence leaked", result.StatusCode, readErr)
		}
		if missingBody == nil {
			missingBody = body
		} else if !bytes.Equal(missingBody, body) {
			t.Fatal("foreign and unknown artifact errors differ")
		}
	}

}

func TestDiscoveryCapabilityRequiresPublisherAndPermission(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	plain, _ := channelServer(t, f.a)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(plain.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: plain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(c.Welcome().GetWelcome().Capabilities, "discovery.read") {
		t.Fatal("unconfigured discovery advertised")
	}
	c.Close()
	foreignPublisher, _ := publisher(t, f.b)
	if _, err := control.NewChannel(f.a, control.ChannelOptions{AllowedHosts: []string{"example.test"}, MaxConnections: 1, DiscoveryPublisher: foreignPublisher}); err == nil {
		t.Fatal("publisher from different Sessions accepted")
	}
	p, v := publisher(t, f.a)
	signed, _ := channelServerWithPublisher(t, f.a, p)
	readGrant := f.g
	readGrant.Permissions = control.DiscoveryRead
	_, readSecret := issue(t, f.a, readGrant)
	reader, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(signed.URL, "https"), Token: readSecret, Hello: helloFor(readGrant), HTTPClient: signed.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	readBundle := sha256.Sum256([]byte("read-only-discovery"))
	if err := reader.Subscribe(t.Context(), "read-only", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(readBundle)}); err != nil {
		t.Fatal(err)
	}
	readEvent := clientEvent(t, reader)
	if _, _, err := client.VerifyDiscovery(v, readEvent.GetSnapshot().GetInlineResource(), f.g.Scope, readBundle, client.Watermark{}, time.Now()); err != nil {
		t.Fatal("discovery.read depended on config.read", err)
	}
	g := f.g
	g.Permissions = control.ConfigRead
	_, secret := issue(t, f.a, g)
	limited, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(signed.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: signed.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if slices.Contains(limited.Welcome().GetWelcome().Capabilities, "discovery.read") {
		t.Fatal("ungranted capability advertised")
	}
	bundle := sha256.Sum256([]byte("denied"))
	if err := limited.Subscribe(t.Context(), "denied", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle)}); err != nil {
		t.Fatal(err)
	}
	_, fetchErr := limited.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(readBundle), Revision: 1})
	remote, ok := errors.AsType[*client.RemoteError](fetchErr)
	if !ok || remote.Detail.Code != agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatal("ungranted immutable discovery fetch accepted", fetchErr)
	}
	failure := clientEvent(t, limited)
	if failure.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatal("ungranted discovery subscription accepted")
	}
}

func TestDiscoveryWSSRefreshesStableEmptySet(t *testing.T) {
	parallelAgentFixture(t)
	f := newRegistrationFixture(t, 1)
	_, verifier := publisher(t, f.a)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32))
	short, err := control.NewPublisher(f.a, "policy-test-key", key, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := channelServerWithPublisher(t, f.a, short)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bundle := sha256.Sum256([]byte("stable-empty"))
	if err := c.Subscribe(t.Context(), "stable", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle)}); err != nil {
		t.Fatal(err)
	}
	initial := clientEvent(t, c)
	first, claims, err := client.VerifyDiscovery(verifier, initial.GetSnapshot().GetInlineResource(), f.g.Scope, bundle, client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refreshed := clientEvent(t, c)
	second, next, err := client.VerifyDiscovery(verifier, refreshed.GetSnapshot().GetInlineResource(), f.g.Scope, bundle, claims.Watermark(), time.Now())
	if err != nil || next.Revision <= claims.Revision || next.ExpiresUnixMs <= claims.ExpiresUnixMs || refreshed.StreamSeq != 2 || first.MembershipRevision() != second.MembershipRevision() || len(second.Candidates()) != 0 {
		t.Fatal("stable set did not refresh with a new artifact identity", err)
	}
}
