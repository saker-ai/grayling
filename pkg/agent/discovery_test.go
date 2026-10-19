package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func TestDiscoverySignedFullSetAndImmutableCandidates(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	verifier, err := client.NewVerifier(map[string]client.TrustKey{"discovery": {PublicKey: public, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	scope := client.Scope{WorkspaceID: "ws", EnvironmentID: "prod", ServiceKey: "checkout"}
	bundle := sha256.Sum256([]byte("approved-bundle"))
	document := client.DiscoveryDocument{SchemaVersion: 1, Purpose: "registered-candidates", Scope: scope, MembershipRevision: math.MaxUint64, Members: []client.DiscoveryMember{{InstanceID: "instance", BootID: "boot", BuildID: "build", ImageDigest: "sha256:" + strings.Repeat("a", 64), Address: "10.20.0.10", Port: 8443, TLSServerName: "checkout.internal", Zone: "az-a", ProtocolVersion: 1, Ready: true, Lease: client.DiscoveryLease{ID: "lease", Epoch: math.MaxUint64, ExpiresUnixMs: now.Add(time.Minute).UnixMilli()}}}}
	sign := func(body []byte, claims client.Claims) *agentv1.SignedResource {
		t.Helper()
		r, err := control.SignResource(claims, body, private)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	claims := client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle), ControlPlaneEpoch: 1, Revision: math.MaxUint64, BundleHash: bundle, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "discovery"}
	body, err := client.MarshalDiscovery(document)
	if err != nil || !bytes.Contains(body, []byte(`"epoch":"18446744073709551615"`)) {
		t.Fatal("64-bit epochs lost decimal-string representation", err)
	}
	signed := sign(body, claims)
	discovery, verified, err := client.VerifyDiscovery(verifier, signed, scope, bundle, client.Watermark{}, now)
	if err != nil || discovery.MembershipRevision() != math.MaxUint64 || discovery.Candidates()[0].Lease.Epoch != math.MaxUint64 {
		t.Fatal("full set failed verification", err)
	}
	candidates := discovery.Candidates()
	candidates[0].Address = "127.0.0.1"
	candidates[0].Scope.WorkspaceId = "forged"
	if discovery.Candidates()[0].Address != "10.20.0.10" || discovery.Candidates()[0].Scope.WorkspaceId != "ws" {
		t.Fatal("caller mutated accepted discovery")
	}
	for _, test := range []struct {
		name   string
		change func(*client.DiscoveryDocument)
	}{
		{"duplicate-instance", func(d *client.DiscoveryDocument) { d.Members = append(d.Members, d.Members[0]) }},
		{"wrong-purpose", func(d *client.DiscoveryDocument) { d.Purpose = "eligible-routes" }},
		{"dns-endpoint", func(d *client.DiscoveryDocument) { d.Members[0].Address = "metadata.internal" }},
		{"metadata-ip", func(d *client.DiscoveryDocument) { d.Members[0].Address = "169.254.169.254" }},
		{"mapped-ip", func(d *client.DiscoveryDocument) { d.Members[0].Address = "::ffff:10.20.0.10" }},
		{"url-sni", func(d *client.DiscoveryDocument) { d.Members[0].TLSServerName = "https://checkout.internal" }},
		{"zero-lease", func(d *client.DiscoveryDocument) { d.Members[0].Lease.Epoch = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := document
			copy.Members = append([]client.DiscoveryMember(nil), document.Members...)
			test.change(&copy)
			if _, err := client.MarshalDiscovery(copy); err == nil {
				t.Fatal("invalid discovery encoded")
			}
			raw, err := json.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.VerifyDiscovery(verifier, sign(raw, claims), scope, bundle, client.Watermark{}, now); err == nil {
				t.Fatal("signed invalid payload accepted")
			}
		})
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"unknown-field", append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"prepared":true}`)...)},
		{"duplicate-field", append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"schemaVersion":1}`)...)},
		{"numeric-epoch", bytes.Replace(body, []byte(`"epoch":"18446744073709551615"`), []byte(`"epoch":18446744073709551615`), 1)},
		{"trailing-document", append(append([]byte(nil), body...), []byte(` {}`)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := client.VerifyDiscovery(verifier, sign(test.body, claims), scope, bundle, client.Watermark{}, now); err == nil {
				t.Fatal("non-schema payload accepted")
			}
		})
	}
	t.Run("snapshot-outlives-lease", func(t *testing.T) {
		short := document
		short.Members = append([]client.DiscoveryMember(nil), document.Members...)
		short.Members[0].Lease.ExpiresUnixMs--
		raw, _ := client.MarshalDiscovery(short)
		if _, _, err := client.VerifyDiscovery(verifier, sign(raw, claims), scope, bundle, client.Watermark{}, now); err == nil {
			t.Fatal("snapshot extends member lease")
		}
	})
	t.Run("excessive-ttl", func(t *testing.T) {
		long := claims
		long.ExpiresUnixMs++
		if _, _, err := client.VerifyDiscovery(verifier, sign(body, long), scope, bundle, client.Watermark{}, now); err == nil {
			t.Fatal("TTL exceeds one minute")
		}
	})
	t.Run("different-bundle", func(t *testing.T) {
		other := sha256.Sum256([]byte("other"))
		if _, _, err := client.VerifyDiscovery(verifier, signed, scope, other, client.Watermark{}, now); err == nil {
			t.Fatal("bundle mismatch accepted")
		}
	})
	t.Run("rollback", func(t *testing.T) {
		old := claims
		old.Revision--
		if _, _, err := client.VerifyDiscovery(verifier, sign(body, old), scope, bundle, verified.Watermark(), now); err == nil {
			t.Fatal("artifact rollback accepted")
		}
	})
	t.Run("expired", func(t *testing.T) {
		if _, _, err := client.VerifyDiscovery(verifier, signed, scope, bundle, client.Watermark{}, now.Add(time.Minute)); err == nil {
			t.Fatal("expired discovery accepted")
		}
	})
	t.Run("valid-empty-set", func(t *testing.T) {
		empty := document
		empty.Members = []client.DiscoveryMember{}
		raw, _ := client.MarshalDiscovery(empty)
		d, _, err := client.VerifyDiscovery(verifier, sign(raw, claims), scope, bundle, client.Watermark{}, now)
		if err != nil || len(d.Candidates()) != 0 {
			t.Fatal("explicit empty set rejected", err)
		}
	})
}
