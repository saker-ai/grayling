//go:build integration

package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
)

func bundlePairRequest(t *testing.T) control.PublishBundleRequest {
	t.Helper()
	scope := grant().Scope
	snapshot := model.Snapshot{SchemaVersion: 1, Workspace: scope.WorkspaceID, Environment: scope.EnvironmentID, ManifestRevision: 1, Flags: map[string]*model.Flag{"route": {SchemaVersion: 1, Key: "route", Type: model.FlagString, Variations: map[string]any{"stable": "backend"}, DefaultRule: &model.Rule{Variation: "stable"}}}}
	payload, err := json.Marshal(snapshot, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	d := client.BundleDocument{SchemaVersion: 1, Purpose: "release-bundle", Scope: scope, Policy: client.BundlePolicy{Key: "routing", ControlPlaneEpoch: 1, Revision: 1, PayloadSHA256: hex.EncodeToString(hash[:])}, RouteFlag: "route", ValueBuilds: map[string]string{"backend": "stable"}, MinimumProtocolVersion: 1, DefaultBehavior: "reject", ValidUntilUnixMs: time.Now().Add(time.Hour).UnixMilli(), Builds: []client.BundleBuild{{ID: "stable", ImageDigest: "sha256:" + strings.Repeat("a", 64), ProtocolVersion: 1, Resources: []client.BundleResource{}}}}
	return control.PublishBundleRequest{PrincipalID: "approved-owner", OperationID: "bundle-pair", Document: d, PolicyPayload: payload}
}
func TestBundlePairConcurrentReplicaDedupAndCAS(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	pa, v := publisher(t, a)
	pb, _ := publisher(t, b)
	request := bundlePairRequest(t)
	results := make(chan control.PublishedBundle, 8)
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			p := pa
			if i%2 != 0 {
				p = pb
			}
			r, err := p.PublishBundle(t.Context(), request)
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		})
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first control.PublishedBundle
	for result := range results {
		if first.BundleHash == [32]byte{} {
			first = result
		}
		if result.Policy.Revision != 1 || result.Bundle.Revision != 1 || !bytes.Equal(result.Policy.Body, first.Policy.Body) || !bytes.Equal(result.Bundle.Body, first.Bundle.Body) {
			t.Fatal("duplicate allocated new pair")
		}
	}
	bundle, claims, err := client.VerifyBundle(v, signed(t, first.Bundle.Body), request.Document.Scope, first.BundleHash, client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	policy, pc, err := bundle.VerifyPolicy(v, signed(t, first.Policy.Body), client.Watermark{}, time.Now())
	if err != nil || policy.Evaluate("route", model.EvaluationContext{}).Value != "backend" || claims.IssuedUnixMs != pc.IssuedUnixMs || claims.ExpiresUnixMs != pc.ExpiresUnixMs {
		t.Fatal("pair not coherently bound", err)
	}
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; count != "2" {
		t.Fatal("unexpected resource count", count)
	}
	changed := request
	changed.Document.DefaultBehavior = "build"
	changed.Document.DefaultBuild = "stable"
	_, err = pb.PublishBundle(t.Context(), changed)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	competing := request
	competing.OperationID = "different-operation"
	_, err = pb.PublishBundle(t.Context(), competing)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	// Reusing an operation across the legacy publisher and pair publisher must fail.
	_, err = pa.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: request.Document.Scope, PrincipalID: request.PrincipalID, OperationID: request.OperationID, Key: request.Document.Policy.Key, BundleHash: first.BundleHash, Payload: request.PolicyPayload})
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	// Garbage collection cannot make an old request allocate a new pair.
	pgtest.Exec(t, dsn, `UPDATE agent_resource_operations SET expires_at=clock_timestamp()-interval '1 second'`)
	_, err = pb.PublishBundle(t.Context(), request)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; count != "2" {
		t.Fatal("old request overwrote pair", count)
	}
}
func TestBundlePairSecondWriteFailureRollsBackEverything(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, _ := publisher(t, s)
	request := bundlePairRequest(t)
	pgtest.Exec(t, dsn, `ALTER TABLE agent_resources ADD CONSTRAINT test_deny_bundle CHECK(resource_key NOT LIKE 'bundle/%')`)
	if _, err := p.PublishBundle(t.Context(), request); err == nil {
		t.Fatal("injected second write succeeded")
	}
	for _, table := range []string{"agent_resources", "agent_resource_heads", "agent_resource_operations"} {
		if count := pgtest.Query(t, dsn, `SELECT count(*) FROM `+table)[0][0]; count != "0" {
			t.Fatalf("partial transaction in %s: %s", table, count)
		}
	}
	pgtest.Exec(t, dsn, `ALTER TABLE agent_resources DROP CONSTRAINT test_deny_bundle`)
	pair, err := p.PublishBundle(t.Context(), request)
	if err != nil || pair.Policy.Revision != 1 || pair.Bundle.Revision != 1 {
		t.Fatal("rollback prevented clean retry", err)
	}
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	se, err := s.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	internalKey := "bundle/" + hex.EncodeToString(pair.BundleHash[:])
	if _, err := s.Resource(t.Context(), se, internalKey, 1); err == nil {
		t.Fatal("policy read exposed internal bundle namespace")
	}
	if _, err := s.ResourceHeads(t.Context(), se, []string{internalKey}); err == nil {
		t.Fatal("policy heads exposed internal bundle namespace")
	}
	// A syntactically valid policy key equal to the public Bundle key is separate.
	publicKey := client.BundleKey(pair.BundleHash)
	if _, err := p.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "approved", OperationID: "public-key-policy", Key: publicKey, BundleHash: pair.BundleHash, Payload: request.PolicyPayload}); err != nil {
		t.Fatal(err)
	}
	if count := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)[0][0]; count != strconv.Itoa(3) {
		t.Fatal("namespaces collided", count)
	}
}
func TestBundlePairInvalidDependencyAndEpochLeaveNoState(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, _ := publisher(t, s)
	request := bundlePairRequest(t)
	bad := request
	bad.Document.Policy.PayloadSHA256 = strings.Repeat("f", 64)
	_, err := p.PublishBundle(t.Context(), bad)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	bad = request
	bad.Document.Policy.ControlPlaneEpoch++
	_, err = p.PublishBundle(t.Context(), bad)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	bad = request
	bad.Document.ValidUntilUnixMs = time.Now().Add(-time.Second).UnixMilli()
	_, err = p.PublishBundle(t.Context(), bad)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_RESOURCE_EXPIRED)
	for _, table := range []string{"agent_resources", "agent_resource_heads", "agent_resource_operations"} {
		if count := pgtest.Query(t, dsn, `SELECT count(*) FROM `+table)[0][0]; count != "0" {
			t.Fatal("invalid pair persisted state", table, count)
		}
	}
}

func TestBundlePairUsesMinimumPublisherLifetime(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, err := control.NewPublisher(s, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := p.PublishBundle(t.Context(), bundlePairRequest(t))
	if err != nil {
		t.Fatal("valid minimum publisher lifetime rejected", err)
	}
	claims, err := client.ParseClaims(signed(t, pair.Bundle.Body).Claims)
	if err != nil || claims.ExpiresUnixMs-claims.IssuedUnixMs != 1000 {
		t.Fatal("publisher lifetime changed", err)
	}
}
