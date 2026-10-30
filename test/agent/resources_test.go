//go:build integration

package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func policyPayload(t *testing.T, value any) []byte {
	t.Helper()
	g := grant()
	kind := model.FlagBool
	if _, ok := value.(map[string]any); ok {
		kind = model.FlagJSON
	}
	snapshot := model.Snapshot{SchemaVersion: 1, Workspace: g.Scope.WorkspaceID, Environment: g.Scope.EnvironmentID, ManifestRevision: 1, GeneratedAt: time.Now().UTC(), Flags: map[string]*model.Flag{"route": {SchemaVersion: 1, Key: "route", Type: kind, Variations: map[string]any{"stable": value}, DefaultRule: &model.Rule{Variation: "stable"}}}}
	payload, err := json.Marshal(snapshot, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
func publisher(t *testing.T, s *control.Sessions) (*control.Publisher, *client.Verifier) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32))
	p, err := control.NewPublisher(s, "policy-test-key", key, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	v, err := client.NewVerifier(map[string]client.TrustKey{"policy-test-key": {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	return p, v
}
func signed(t *testing.T, body []byte) *agentv1.SignedResource {
	t.Helper()
	r := new(agentv1.SignedResource)
	if err := proto.Unmarshal(body, r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestPolicyPublicationCASDedupAndReplicaRead(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	pa, v := publisher(t, a)
	pb, _ := publisher(t, b)
	g := grant()
	request := control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "approved-publisher", OperationID: "publish-1", Key: "routing", BundleHash: sha256.Sum256([]byte("bundle-1")), Payload: policyPayload(t, true)}
	var wg sync.WaitGroup
	results := make(chan control.StoredResource, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			p := pa
			if i%2 != 0 {
				p = pb
			}
			resource, err := p.PublishPolicy(t.Context(), request)
			if err != nil {
				errs <- err
			} else {
				results <- resource
			}
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first control.StoredResource
	for resource := range results {
		if resource.Revision != 1 {
			t.Fatal("duplicate operation allocated revision")
		}
		if first.Revision != 0 && !bytes.Equal(first.Body, resource.Body) {
			t.Fatal("duplicate operation changed artifact")
		}
		first = resource
	}
	policy, c, err := client.VerifyPolicy(v, signed(t, first.Body), g.Scope, "routing", client.Watermark{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result := policy.Evaluate("route", model.EvaluationContext{TargetingKey: "user-1"})
	if result.Failed || result.Value != true {
		t.Fatalf("local evaluation: %v", result)
	}
	changed := request
	changed.Payload = policyPayload(t, false)
	_, err = pb.PublishPolicy(t.Context(), changed)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	stale := changed
	stale.OperationID = "publish-stale"
	_, err = pa.PublishPolicy(t.Context(), stale)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
	changed.ExpectedRevision = 1
	changed.OperationID = "publish-2"
	second, err := pb.PublishPolicy(t.Context(), changed)
	if err != nil || second.Revision != 2 {
		t.Fatalf("publish next: %v", err)
	}
	_, _, err = client.VerifyPolicy(v, signed(t, first.Body), g.Scope, "routing", func() client.Watermark {
		claims, err := client.ParseClaims(signed(t, second.Body).Claims)
		if err != nil {
			t.Fatal(err)
		}
		return claims.Watermark()
	}(), time.Now())
	if !errors.Is(err, client.ErrRollback) {
		t.Fatal("signed old notification bypassed watermark")
	}
	if err := a.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, a, g)
	se, err := b.Connect(t.Context(), secret, helloFor(g))
	if err != nil {
		t.Fatal(err)
	}
	current, err := a.Resource(t.Context(), se, "routing", 0)
	if err != nil || current.Revision != 2 {
		t.Fatalf("replica resource: %v", err)
	}
	if current.Hash != sha256.Sum256(current.Body) || c.Revision != 1 {
		t.Fatal("transport or claims revision mismatch")
	}
	other := g
	other.Scope.EnvironmentID = "other-env"
	if err := a.SetScopeLimit(t.Context(), other.Scope, 1); err != nil {
		t.Fatal(err)
	}
	_, otherSecret := issue(t, a, other)
	otherSession, err := b.Connect(t.Context(), otherSecret, helloFor(other))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Resource(t.Context(), otherSession, "routing", 1)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_NOT_READY)
	noRead := g
	noRead.InstanceID = "no-read"
	noRead.Permissions = control.EventsWrite
	_, noReadSecret := issue(t, a, noRead)
	noReadSession, err := b.Connect(t.Context(), noReadSecret, helloFor(noRead))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Resource(t.Context(), noReadSession, "routing", 1)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	bad := changed
	bad.ExpectedRevision = 2
	bad.OperationID = "invalid-policy"
	bad.Payload = []byte(`{"schemaVersion":999}`)
	_, err = pb.PublishPolicy(t.Context(), bad)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	rows := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_resources`)
	if rows[0][0] != "2" {
		t.Fatal("failed publication persisted resource")
	}
	reopened := openSessions(t, dsn)
	resource, err := reopened.Resource(t.Context(), se, "routing", 0)
	if err != nil || resource.Revision != 2 {
		t.Fatal("head did not survive reopen")
	}
}
func TestPolicyRevisionUsesFullUint64(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	p, _ := publisher(t, s)
	g := grant()
	r := control.PublishPolicyRequest{Scope: g.Scope, PrincipalID: "publisher", OperationID: "first", Key: "routing", BundleHash: sha256.Sum256([]byte("bundle")), Payload: policyPayload(t, true)}
	if _, err := p.PublishPolicy(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// Only advance the policy counter; publication also creates a revision-zero
	// revocation head, which must keep pointing at its unchanged catalog.
	pgtest.Query(t, dsn, `UPDATE agent_resource_heads SET revision=18446744073709551614 WHERE resource_key='routing' RETURNING revision::text`)
	if rows := pgtest.Query(t, dsn, `SELECT revision::text FROM agent_resource_heads WHERE resource_key='revocation/state'`); len(rows) != 1 || rows[0][0] != "0" {
		t.Fatal("uint64 fixture changed revocation head", rows)
	}
	r.ExpectedRevision = 18446744073709551614
	r.OperationID = "last"
	last, err := p.PublishPolicy(t.Context(), r)
	if err != nil || last.Revision != 18446744073709551615 {
		t.Fatalf("uint64 storage truncated: %v", err)
	}
	r.ExpectedRevision = last.Revision
	r.OperationID = "overflow"
	_, err = p.PublishPolicy(t.Context(), r)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_STALE_REVISION)
}
