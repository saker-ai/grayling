package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

func bundleFixture(t *testing.T) (*cacheFixture, agent.BundleDocument, []byte) {
	t.Helper()
	f := newCacheFixture(t)
	payload := f.resource(t, math.MaxUint64, "unbound").Payload
	hash := sha256.Sum256(payload)
	d := agent.BundleDocument{SchemaVersion: 1, Purpose: "release-bundle", Scope: f.opts.Scope, Policy: agent.BundlePolicy{Key: f.opts.Key, ControlPlaneEpoch: 1, Revision: math.MaxUint64, PayloadSHA256: hex.EncodeToString(hash[:])}, RouteFlag: "route", ValueBuilds: map[string]string{"backend": "stable"}, MinimumProtocolVersion: agentv1.ProtocolMajor, DefaultBehavior: "reject", ValidUntilUnixMs: f.opts.Now().Add(time.Hour).UnixMilli(), Builds: []agent.BundleBuild{{ID: "stable", ImageDigest: "sha256:" + strings.Repeat("a", 64), ProtocolVersion: 1, Resources: []agent.BundleResource{{Key: "frontend", SHA256: strings.Repeat("b", 64)}}}}}
	return f, d, payload
}
func signBundle(t *testing.T, f *cacheFixture, body []byte, change func(*agent.Claims)) (*agentv1.SignedResource, [32]byte) {
	t.Helper()
	hash := sha256.Sum256(body)
	claims := agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: agent.BundleKey(hash), ControlPlaneEpoch: 1, Revision: math.MaxUint64, BundleHash: hash, IssuedUnixMs: f.opts.Now().UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(time.Hour).UnixMilli(), KeyID: "key"}
	if change != nil {
		change(&claims)
	}
	r, err := control.SignResource(claims, body, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return r, hash
}
func TestBundleSignedDependenciesAndImmutableDocument(t *testing.T) {
	f, d, payload := bundleFixture(t)
	body, err := agent.MarshalBundle(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"revision":"18446744073709551615"`)) {
		t.Fatal("policy revision lost precision")
	}
	r, hash := signBundle(t, f, body, nil)
	b, claims, err := agent.VerifyBundle(f.opts.Verifier, r, f.opts.Scope, hash, agent.Watermark{}, f.opts.Now())
	if err != nil || b.Hash() != hash || claims.Revision != math.MaxUint64 {
		t.Fatal("bundle rejected", err)
	}
	copy := b.Document()
	copy.ValueBuilds["backend"] = "attacker"
	copy.Builds[0].ID = "attacker"
	copy.Builds[0].Resources[0].SHA256 = strings.Repeat("c", 64)
	if b.Document().ValueBuilds["backend"] != "stable" || b.Document().Builds[0].ID != "stable" || b.Document().Builds[0].Resources[0].SHA256 != strings.Repeat("b", 64) {
		t.Fatal("mutable bundle alias")
	}
	pc := agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: d.Policy.Key, ControlPlaneEpoch: 1, Revision: d.Policy.Revision, BundleHash: hash, IssuedUnixMs: f.opts.Now().UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(time.Minute).UnixMilli(), KeyID: "key"}
	policy, err := control.SignResource(pc, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	r.Payload[0] ^= 1
	r.Signature[0] ^= 1
	compiled, _, err := b.VerifyPolicy(f.opts.Verifier, policy, agent.Watermark{}, f.opts.Now())
	if err != nil || compiled.Evaluate("route", model.EvaluationContext{}).Value != "backend" {
		t.Fatal("bound evaluator unavailable", err)
	}
	for _, test := range []struct {
		name   string
		change func(*agent.Claims)
	}{
		{"other bundle", func(c *agent.Claims) { c.BundleHash = sha256.Sum256([]byte("other")) }},
		{"other revision", func(c *agent.Claims) { c.Revision-- }},
		{"other epoch", func(c *agent.Claims) { c.ControlPlaneEpoch++ }},
		{"other key", func(c *agent.Claims) { c.Key = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := pc
			test.change(&c)
			wrong, err := control.SignResource(c, payload, f.private)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.VerifyPolicy(f.opts.Verifier, wrong, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("foreign policy dependency accepted")
			}
		})
	}
	retired, err := agent.NewVerifier(map[string]agent.TrustKey{"replacement": {PublicKey: f.private.Public().(ed25519.PublicKey), NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	newClaims := pc
	newClaims.KeyID = "replacement"
	newPolicy, err := control.SignResource(newClaims, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.VerifyPolicy(retired, newPolicy, agent.Watermark{}, f.opts.Now()); err == nil {
		t.Fatal("retired bundle root accepted through fresh policy root")
	}
	if _, _, err := b.VerifyPolicy(f.opts.Verifier, policy, agent.Watermark{}, f.opts.Now().Add(time.Hour)); err == nil {
		t.Fatal("expired bundle dependency accepted")
	}
}
func TestBundleSchemaAndRoutingMapRejectIncompleteCandidates(t *testing.T) {
	f, d, _ := bundleFixture(t)
	for _, test := range []struct {
		name   string
		change func(*agent.BundleDocument)
	}{
		{"wrong purpose", func(d *agent.BundleDocument) { d.Purpose = "registered-candidates" }},
		{"empty builds", func(d *agent.BundleDocument) { d.Builds = nil }},
		{"duplicate build", func(d *agent.BundleDocument) { d.Builds = append(d.Builds, d.Builds[0]) }},
		{"unknown default", func(d *agent.BundleDocument) { d.DefaultBehavior = "build"; d.DefaultBuild = "unknown" }},
		{"implicit fallback", func(d *agent.BundleDocument) { d.DefaultBuild = "stable" }},
		{"unmapped target", func(d *agent.BundleDocument) { d.ValueBuilds["backend"] = "missing" }},
		{"missing resources", func(d *agent.BundleDocument) { d.Builds[0].Resources = nil }},
		{"duplicate resource", func(d *agent.BundleDocument) {
			d.Builds[0].Resources = append(d.Builds[0].Resources, d.Builds[0].Resources[0])
		}},
		{"unknown protocol", func(d *agent.BundleDocument) { d.MinimumProtocolVersion = 2 }},
		{"unknown build protocol", func(d *agent.BundleDocument) { d.Builds[0].ProtocolVersion = 2 }},
		{"invalid resource digest", func(d *agent.BundleDocument) { d.Builds[0].Resources[0].SHA256 = strings.Repeat("B", 64) }},
		{"zero image digest", func(d *agent.BundleDocument) { d.Builds[0].ImageDigest = "sha256:" + strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(d)
			var changed agent.BundleDocument
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			test.change(&changed)
			if _, err := agent.MarshalBundle(changed); err == nil {
				t.Fatal("invalid bundle encoded")
			}
			raw, _ = json.Marshal(changed, json.Deterministic(true), json.FormatNilSliceAsNull(true))
			r, hash := signBundle(t, f, raw, nil)
			if _, _, err := agent.VerifyBundle(f.opts.Verifier, r, f.opts.Scope, hash, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("signed invalid bundle accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*agent.BundleDocument)
	}{
		{"missing route flag", func(d *agent.BundleDocument) { d.RouteFlag = "missing" }},
		{"unknown output", func(d *agent.BundleDocument) { d.ValueBuilds = map[string]string{"unrecognized": "stable"} }},
		{"extra output", func(d *agent.BundleDocument) { d.ValueBuilds["extra"] = "stable" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, candidate, payload := bundleFixture(t)
			test.change(&candidate)
			body, err := agent.MarshalBundle(candidate)
			if err != nil {
				t.Fatal(err)
			}
			r, hash := signBundle(t, f, body, nil)
			b, _, err := agent.VerifyBundle(f.opts.Verifier, r, f.opts.Scope, hash, agent.Watermark{}, f.opts.Now())
			if err != nil {
				t.Fatal(err)
			}
			pc := agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: d.Policy.Key, ControlPlaneEpoch: 1, Revision: d.Policy.Revision, BundleHash: hash, IssuedUnixMs: f.opts.Now().UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(time.Minute).UnixMilli(), KeyID: "key"}
			policy, err := control.SignResource(pc, payload, f.private)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.VerifyPolicy(f.opts.Verifier, policy, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("incomplete routing dependency accepted")
			}
		})
	}
}
func TestBundleCanonicalBytesScopeAndAuthorizationExpiry(t *testing.T) {
	f, d, _ := bundleFixture(t)
	body, err := agent.MarshalBundle(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		body   []byte
		change func(*agent.Claims)
	}{
		{"noncanonical whitespace", append([]byte(" "), body...), nil},
		{"unknown field", append(append([]byte{}, body[:len(body)-1]...), []byte(`,"unknown":true}`)...), nil},
		{"authorization extension", body, func(c *agent.Claims) { c.ExpiresUnixMs++ }},
		{"cross epoch policy", body, func(c *agent.Claims) { c.ControlPlaneEpoch++ }},
		{"foreign scope", body, func(c *agent.Claims) { c.Scope.ServiceKey = "other" }},
		{"foreign bundle", body, func(c *agent.Claims) { c.BundleHash = sha256.Sum256([]byte("foreign")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, hash := signBundle(t, f, test.body, test.change)
			if _, _, err := agent.VerifyBundle(f.opts.Verifier, r, f.opts.Scope, hash, agent.Watermark{}, f.opts.Now()); err == nil {
				t.Fatal("invalid signed bundle accepted")
			}
		})
	}
}
