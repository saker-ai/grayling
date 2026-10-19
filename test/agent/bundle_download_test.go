//go:build integration

package agent_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func TestBundleWSSHTTPSExactReadFailoverAndFencing(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	p, v := publisher(t, a)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	firstServer, _ := channelServer(t, b)
	secondServer, _ := channelServer(t, a)
	g := grant()
	g.ExpiresAt = time.Now().Add(45 * time.Second).Truncate(time.Millisecond)
	if err := a.SetScopeLimit(t.Context(), g.Scope, 4); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, a, g)
	opts := client.Options{URL: "wss" + strings.TrimPrefix(firstServer.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: firstServer.Client()}
	opts.Hello.RequiredCapabilities = []string{"bundle.read"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fetch := &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(pair.BundleHash), Revision: 1}
	ref, err := c.FetchResource(t.Context(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Kind != fetch.Kind || ref.Key != fetch.Key || ref.Revision != 1 || ref.Sha256 != hex.EncodeToString(pair.Bundle.Hash[:]) || !strings.Contains(ref.HttpsUrl, "/resources/3/") || ref.ExpiresUnixMs > g.ExpiresAt.UnixMilli() || ref.ExpiresUnixMs >= pair.Bundle.ExpiresAt.UnixMilli() {
		t.Fatal("bundle reference not bound or credential-clipped", ref)
	}
	downloaded, err := c.Download(t.Context(), ref)
	if err != nil || !proto.Equal(downloaded, signed(t, pair.Bundle.Body)) {
		t.Fatal("HTTPS returned different artifact", err)
	}
	bundle, claims, err := c.DownloadBundle(t.Context(), ref, v, g.Scope, pair.BundleHash, client.Watermark{}, time.Now())
	if err != nil || bundle.Hash() != pair.BundleHash || claims.Revision != 1 {
		t.Fatal("downloaded bundle verification failed", err)
	}
	cacheRoot := t.TempDir()
	cacheOptions := client.BundleCacheOptions{Directory: filepath.Join(cacheRoot, "bundle"), Identity: g.AgentID, Scope: g.Scope, AuthenticationKey: bytes.Repeat([]byte{0x29}, 32), Verifier: v, BundleHash: pair.BundleHash}
	cache, err := client.InitializeBundleCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), downloaded); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := client.OpenBundleCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cachedBundle, err := reopened.Capture()
	if err != nil {
		t.Fatal(err)
	}
	localDocument, err := cachedBundle.Document()
	if err != nil || localDocument.RouteFlag != "route" {
		t.Fatal("network Bundle did not survive trusted reopen", err)
	}
	policyRef, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	policyResource, err := c.Download(t.Context(), policyRef)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := client.InitializePolicyCache(t.Context(), client.CacheOptions{Directory: filepath.Join(cacheRoot, "policy"), Identity: g.AgentID, Scope: g.Scope, Key: "routing", AuthenticationKey: bytes.Repeat([]byte{0x29}, 32), Verifier: v})
	if err != nil {
		t.Fatal(err)
	}
	defer policies.Close()
	if err := policies.Install(t.Context(), policyResource); err != nil {
		t.Fatal(err)
	}
	policyView, err := policies.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if err := cachedBundle.ValidatePolicy(policyView); err != nil {
		t.Fatal("durable network Bundle/policy dependency join failed", err)
	}
	for _, test := range []struct {
		name   string
		change func(*agentv1.ResourceRef)
	}{
		{"wrong kind", func(r *agentv1.ResourceRef) { r.Kind = agentv1.ResourceKind_RESOURCE_KIND_POLICY }},
		{"wrong key", func(r *agentv1.ResourceRef) { r.Key = "foreign" }},
		{"wrong revision", func(r *agentv1.ResourceRef) { r.Revision++ }},
		{"extended authorization", func(r *agentv1.ResourceRef) { r.ExpiresUnixMs = claims.ExpiresUnixMs + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := proto.Clone(ref).(*agentv1.ResourceRef)
			test.change(bad)
			if _, _, err := c.DownloadBundle(t.Context(), bad, v, g.Scope, pair.BundleHash, client.Watermark{}, time.Now()); !errors.Is(err, client.ErrResource) {
				t.Fatal("reference metadata mismatch accepted", err)
			}
		})
	}
	for _, revision := range []uint64{0, 2} {
		bad := proto.Clone(fetch).(*agentv1.FetchResource)
		bad.Revision = revision
		_, err := c.FetchResource(t.Context(), bad)
		code := agentv1.ErrorCode_ERROR_CODE_NOT_READY
		if revision == 0 {
			code = agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
		}
		expectCode(t, err, code)
	}
	// A policy with the same public key must not shadow the private Bundle row.
	request := bundlePairRequest(t)
	if _, err := p.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "approved", OperationID: "same-public-key", Key: fetch.Key, BundleHash: pair.BundleHash, Payload: request.PolicyPayload}); err != nil {
		t.Fatal(err)
	}
	again, err := c.FetchResource(t.Context(), fetch)
	if err != nil || again.Sha256 != ref.Sha256 {
		t.Fatal("policy shadowed Bundle", err)
	}
	opts.URL = "wss" + strings.TrimPrefix(secondServer.URL, "https")
	opts.HTTPClient = secondServer.Client()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	next, err := successor.FetchResource(t.Context(), fetch)
	if err != nil || next.Sha256 != ref.Sha256 || successor.Welcome().ConnectionEpoch <= c.Welcome().ConnectionEpoch {
		t.Fatal("cross-replica bundle recovery changed identity", err)
	}
	if _, _, err := successor.DownloadBundle(t.Context(), next, v, g.Scope, pair.BundleHash, claims.Watermark(), time.Now()); err != nil {
		t.Fatal(err)
	}
	probe := func(target, token, session string, epoch uint64) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Grayling-Agent-Session", session)
		req.Header.Set("X-Grayling-Agent-Epoch", strconv.FormatUint(epoch, 10))
		response, err := firstServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	if status, _ := probe(ref.HttpsUrl, secret, c.Welcome().SessionId, c.Welcome().ConnectionEpoch); status != http.StatusForbidden {
		t.Fatal("old session downloaded Bundle", status)
	}
	foreign := g
	foreign.Scope.EnvironmentID += "-foreign"
	if err := a.SetScopeLimit(t.Context(), foreign.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, foreignSecret := issue(t, a, foreign)
	foreignSession, err := a.Connect(t.Context(), foreignSecret, helloFor(foreign))
	if err != nil {
		t.Fatal(err)
	}
	var previous []byte
	for _, key := range []string{fetch.Key, client.BundleKey(sha256.Sum256([]byte("unknown")))} {
		target := strings.Replace(ref.HttpsUrl, fetch.Key, key, 1)
		status, body := probe(target, foreignSecret, foreignSession.ID, foreignSession.Epoch)
		if status != http.StatusNotFound {
			t.Fatal("foreign Bundle existence leaked", status)
		}
		if previous != nil && !bytes.Equal(previous, body) {
			t.Fatal("foreign and absent errors differ")
		}
		previous = body
	}
	// Restored control-plane epochs cannot serve artifacts from the old epoch.
	pgtest.Exec(t, dsn, `UPDATE agent_schema SET control_plane_epoch=control_plane_epoch+1 WHERE id=true`)
	_, err = successor.FetchResource(t.Context(), fetch)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
}

func TestBundleReadRequiresConfigPermissionBeforeExistence(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, _ := publisher(t, s)
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	g := grant()
	g.Permissions = control.DiscoveryRead
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	srv, _ := channelServer(t, s)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, capability := range c.Welcome().GetWelcome().Capabilities {
		if capability == "bundle.read" {
			t.Fatal("unauthorized Bundle capability advertised")
		}
	}
	for _, hash := range [][32]byte{pair.BundleHash, sha256.Sum256([]byte("absent"))} {
		_, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(hash), Revision: 1})
		expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	}
}
