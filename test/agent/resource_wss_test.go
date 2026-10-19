//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func clientEvent(t *testing.T, c *client.Client) *agentv1.Envelope {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case e := <-c.Events():
		if e == nil {
			t.Fatalf("client closed: %v", c.Err())
		}
		return e
	case <-timer.C:
		t.Fatal("resource event timeout")
		return nil
	}
}
func TestPolicyWSSCrossReplicaAndLargeDownload(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	p, v := publisher(t, a)
	g := grant()
	if err := a.SetScopeLimit(t.Context(), g.Scope, 4); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, a, g)
	srv, _ := channelServer(t, b)
	request := control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "approved", OperationID: "first", Key: "routing", BundleHash: sha256.Sum256([]byte("bundle-1")), Payload: policyPayload(t, true)}
	first, err := p.PublishPolicy(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: srv.Client(), RequestTimeout: 5 * time.Second}
	opts.Hello.RequiredCapabilities = []string{"config.read"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Subscribe(t.Context(), "routing-sub", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing"}); err != nil {
		t.Fatal(err)
	}
	event := clientEvent(t, c)
	if event.GetSnapshot().GetInlineResource() == nil || event.StreamSeq != 1 || event.SubscriptionId != "routing-sub" {
		t.Fatalf("initial snapshot: %v", event)
	}
	policy, claims, err := client.VerifyPolicy(v, event.GetSnapshot().GetInlineResource(), g.Scope, "routing", client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result := policy.Evaluate("route", model.EvaluationContext{TargetingKey: "u"}); result.Failed || result.Value != true {
		t.Fatal("initial local policy incorrect")
	}
	request.ExpectedRevision = first.Revision
	request.OperationID = "second"
	request.Payload = policyPayload(t, false)
	second, err := p.PublishPolicy(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	invalidation := clientEvent(t, c)
	if invalidation.GetInvalidated().GetRevision() != second.Revision || invalidation.StreamSeq != 2 {
		t.Fatalf("cross-node invalidation: %v", invalidation)
	}
	ref, err := c.FetchResource(t.Context(), &agentv1.FetchResource{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing", Revision: second.Revision})
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := c.Download(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	policy, nextClaims, err := client.VerifyPolicy(v, downloaded, g.Scope, "routing", claims.Watermark(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result := policy.Evaluate("route", model.EvaluationContext{TargetingKey: "u"}); result.Failed || result.Value != false {
		t.Fatal("updated local policy incorrect")
	}
	_, _, err = client.VerifyPolicy(v, signed(t, first.Body), g.Scope, "routing", nextClaims.Watermark(), time.Now())
	if !errors.Is(err, client.ErrRollback) {
		t.Fatal("old signed snapshot replaced newer resource")
	}
	if err := c.Resume(t.Context(), "routing-sub", []byte("expired-cursor")); err != nil {
		t.Fatal(err)
	}
	resync := clientEvent(t, c)
	if resync.GetResyncRequired() == nil || resync.SubscriptionId != "routing-sub" {
		t.Fatalf("resume did not request complete snapshot: %v", resync)
	}
	request.ExpectedRevision = second.Revision
	request.OperationID = "large"
	request.Payload = policyPayload(t, map[string]any{"padding": strings.Repeat("x", 300<<10), "build": "stable"})
	large, err := p.PublishPolicy(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = clientEvent(t, c)
	if err := c.Subscribe(t.Context(), "routing-sub", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing", Revision: second.Revision}); err != nil {
		t.Fatal(err)
	}
	snapshot := clientEvent(t, c)
	ref = snapshot.GetSnapshot().GetReference()
	if ref == nil || ref.Revision != large.Revision || ref.SizeBytes <= 192<<10 || snapshot.StreamSeq <= invalidation.StreamSeq {
		t.Fatal("large resource used inline transport or sequence went backward")
	}
	downloaded, err = c.Download(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	policy, _, err = client.VerifyPolicy(v, downloaded, g.Scope, "routing", nextClaims.Watermark(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	cacheOptions := client.CacheOptions{Directory: filepath.Join(t.TempDir(), "policy-cache"), Identity: "wss-integration", Scope: g.Scope, Key: "routing", AuthenticationKey: bytes.Repeat([]byte{17}, 32), Verifier: v}
	cache, err := client.InitializePolicyCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), downloaded); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = client.OpenPolicyCache(t.Context(), cacheOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	pinned, err := cache.Capture()
	if err != nil {
		t.Fatal(err)
	}
	cachedResult, err := pinned.Evaluate("route", model.EvaluationContext{})
	if err != nil || cachedResult.Value.(map[string]any)["build"] != "stable" {
		t.Fatalf("restored WSS policy: %v", err)
	}
	if err := cache.Install(t.Context(), signed(t, first.Body)); !errors.Is(err, client.ErrRollback) {
		t.Fatalf("restart forgot watermark: %v", err)
	}
	value := policy.Evaluate("route", model.EvaluationContext{}).Value.(map[string]any)
	value["build"] = "mutated"
	if got := policy.Evaluate("route", model.EvaluationContext{}).Value.(map[string]any)["build"]; got != "stable" {
		t.Fatal("caller mutated pinned evaluator")
	}
	if _, _, err := c.DownloadPolicy(t.Context(), ref, v, g.Scope, nextClaims.Watermark(), time.Now()); err != nil {
		t.Fatal(err)
	}
	forged := proto.Clone(ref).(*agentv1.ResourceRef)
	forged.Revision++
	if _, _, err := c.DownloadPolicy(t.Context(), forged, v, g.Scope, nextClaims.Watermark(), time.Now()); !errors.Is(err, client.ErrResource) {
		t.Fatal("reference revision disagreed with signed claims")
	}
	// Resource HTTP credentials alone are insufficient; current session fencing
	// and the credential's scope/permission are checked again on the data plane.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ref.HttpsUrl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	response, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("download omitted session fencing")
	}
	welcome := c.Welcome()
	req.Header.Set("X-Grayling-Agent-Session", welcome.SessionId)
	req.Header.Set("X-Grayling-Agent-Epoch", strconv.FormatUint(welcome.ConnectionEpoch+1, 10))
	response, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("download accepted stale epoch")
	}
	req.Header.Set("X-Grayling-Agent-Epoch", strconv.FormatUint(welcome.ConnectionEpoch, 10))
	req.Header.Set("Origin", srv.URL)
	response, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("browser resource request accepted")
	}
	req.Header.Del("Origin")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 3 {
		reply, err := c.CallMessage(ctx, &agentv1.Heartbeat{Nonce: 44}, client.Metadata{})
		if err != nil || reply.GetHeartbeatAck().GetNonce() != 44 {
			t.Fatalf("resource traffic blocked heartbeat: %v", err)
		}
	}
	replacement, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	response, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("old session downloaded after takeover")
	}
}
