package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	agent "github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func fixture(t *testing.T) (agent.Claims, ed25519.PrivateKey, *agent.Verifier, time.Time) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	now := time.UnixMilli(1700000000000)
	c := agent.Claims{Scope: agent.Scope{WorkspaceID: "workspace-1", EnvironmentID: "env-1", ServiceKey: "checkout"}, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "policy", ControlPlaneEpoch: 1, Revision: 9007199254740993, BundleHash: sha256.Sum256([]byte("bundle")), IssuedUnixMs: now.Add(-time.Minute).UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "test-key-1"}
	v, err := agent.NewVerifier(map[string]agent.TrustKey{c.KeyID: {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	return c, key, v, now
}
func TestSignedResource(t *testing.T) {
	t.Parallel()
	c, key, v, now := fixture(t)
	r, err := control.SignResource(c, []byte("test policy bytes"), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != c.Revision || got.PayloadHash != sha256.Sum256(r.Payload) {
		t.Fatal("claims changed")
	}
	if _, err := v.Verify(r, c.Scope, c.Kind, c.Key, got.Watermark(), now); err != nil {
		t.Fatalf("idempotent install: %v", err)
	}
	tests := []struct {
		name   string
		change func(*agentv1.SignedResource, *agent.Claims, *agent.Watermark, *time.Time)
		want   error
	}{
		{"payload tamper", func(r *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, _ *time.Time) { r.Payload[0] ^= 1 }, agent.ErrResource},
		{"claims tamper", func(r *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, _ *time.Time) { r.Claims[5] ^= 1 }, agent.ErrResource},
		{"signature tamper", func(r *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, _ *time.Time) {
			r.Signature[0] ^= 1
		}, agent.ErrResource},
		{"wrong key", func(r *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, _ *time.Time) {
			r.KeyId = "missing"
		}, agent.ErrResource},
		{"wrong scope", func(_ *agentv1.SignedResource, c *agent.Claims, _ *agent.Watermark, _ *time.Time) {
			c.Scope.EnvironmentID = "env-2"
		}, agent.ErrScope},
		{"wrong resource", func(_ *agentv1.SignedResource, c *agent.Claims, _ *agent.Watermark, _ *time.Time) {
			c.Key = "discovery"
		}, agent.ErrScope},
		{"wrong kind", func(_ *agentv1.SignedResource, c *agent.Claims, _ *agent.Watermark, _ *time.Time) {
			c.Kind = agentv1.ResourceKind_RESOURCE_KIND_BUNDLE
		}, agent.ErrScope},
		{"expiry boundary", func(_ *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, n *time.Time) {
			*n = time.UnixMilli(c.ExpiresUnixMs)
		}, agent.ErrExpired},
		{"not yet issued", func(_ *agentv1.SignedResource, _ *agent.Claims, _ *agent.Watermark, n *time.Time) {
			*n = time.UnixMilli(c.IssuedUnixMs - 1)
		}, agent.ErrExpired},
		{"older revision", func(_ *agentv1.SignedResource, _ *agent.Claims, m *agent.Watermark, _ *time.Time) { m.Revision++ }, agent.ErrRollback},
		{"conflicting payload", func(_ *agentv1.SignedResource, _ *agent.Claims, m *agent.Watermark, _ *time.Time) {
			m.PayloadHash[0] ^= 1
		}, agent.ErrRollback},
		{"conflicting bundle", func(_ *agentv1.SignedResource, _ *agent.Claims, m *agent.Watermark, _ *time.Time) {
			m.BundleHash[0] ^= 1
		}, agent.ErrRollback},
		{"new epoch needs recovery authorization", func(_ *agentv1.SignedResource, _ *agent.Claims, m *agent.Watermark, _ *time.Time) {
			m.ControlPlaneEpoch++
		}, agent.ErrRollback},
		{"malformed watermark", func(_ *agentv1.SignedResource, _ *agent.Claims, m *agent.Watermark, _ *time.Time) {
			m.ControlPlaneEpoch = 0
		}, agent.ErrRollback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := proto.Clone(r).(*agentv1.SignedResource)
			want := c
			mark := got.Watermark()
			n := now
			tt.change(r, &want, &mark, &n)
			_, err := v.Verify(r, want.Scope, want.Kind, want.Key, mark, n)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v; want %v", err, tt.want)
			}
		})
	}
}
func TestRootValidityBoundsResource(t *testing.T) {
	t.Parallel()
	c, key, _, now := fixture(t)
	r, err := control.SignResource(c, []byte("payload"), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name          string
		before, after time.Time
	}{
		{"root expires before resource", now.Add(-time.Hour), now.Add(time.Second)},
		{"root starts after issuance", now.Add(-time.Second), now.Add(time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := agent.NewVerifier(map[string]agent.TrustKey{c.KeyID: {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: tt.before, NotAfter: tt.after}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now); !errors.Is(err, agent.ErrExpired) {
				t.Fatalf("resource exceeded root window: %v", err)
			}
		})
	}
}

func TestSigningOwnsPayload(t *testing.T) {
	t.Parallel()
	c, key, v, now := fixture(t)
	p := []byte("original")
	r, err := control.SignResource(c, p, key)
	if err != nil {
		t.Fatal(err)
	}
	p[0] ^= 1
	if _, err := v.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := control.SignResource(c, p, nil); !errors.Is(err, agent.ErrResource) {
		t.Fatal("nil signing key accepted")
	}
}
func TestRotationAndRetirement(t *testing.T) {
	t.Parallel()
	c, key, _, now := fixture(t)
	r, err := control.SignResource(c, []byte("payload"), key)
	if err != nil {
		t.Fatal(err)
	}
	public := key.Public().(ed25519.PublicKey)
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x43}, 32)).Public().(ed25519.PublicKey)
	roots := map[string]agent.TrustKey{c.KeyID: {PublicKey: public, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, "new-key": {PublicKey: other, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(2 * time.Hour)}}
	v, err := agent.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	public[0] ^= 1
	delete(roots, c.KeyID)
	if _, err := v.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now); err != nil {
		t.Fatalf("root input alias: %v", err)
	}
	retired, err := agent.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retired.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now); !errors.Is(err, agent.ErrResource) {
		t.Fatal("retired root accepted")
	}
	expired, err := agent.NewVerifier(map[string]agent.TrustKey{c.KeyID: {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expired.Verify(r, c.Scope, c.Kind, c.Key, agent.Watermark{}, now); !errors.Is(err, agent.ErrExpired) {
		t.Fatal("root expiry ignored")
	}
}
