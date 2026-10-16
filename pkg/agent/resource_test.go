package agent_test

import (
	"bytes"
	"crypto/sha256"
	"testing"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/agent"
)

func claims() agent.Claims {
	return agent.Claims{Scope: agent.Scope{WorkspaceID: "w", EnvironmentID: "e", ServiceKey: "s"}, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "p", ControlPlaneEpoch: 1, Revision: 2, BundleHash: sha256.Sum256([]byte("b")), PayloadHash: sha256.Sum256([]byte("p")), IssuedUnixMs: 1, ExpiresUnixMs: 2, KeyID: "k"}
}
func TestClaimsEncoding(t *testing.T) {
	t.Parallel()
	c := claims()
	b, err := agent.MarshalClaims(c)
	if err != nil {
		t.Fatal(err)
	}
	round, err := agent.ParseClaims(b)
	if err != nil || c != round {
		t.Fatalf("roundtrip: %v", err)
	}
	for i := range len(b) {
		if _, err := agent.ParseClaims(b[:i]); err == nil {
			t.Fatalf("accepted truncated claims at %d", i)
		}
	}
	if _, err := agent.ParseClaims(append(bytes.Clone(b), 0)); err == nil {
		t.Fatal("accepted trailing data")
	}
	c.Scope.WorkspaceID = "invalid\x00scope"
	if _, err := agent.MarshalClaims(c); err == nil {
		t.Fatal("accepted control character")
	}
	c = claims()
	c.ExpiresUnixMs = c.IssuedUnixMs
	if _, err := agent.MarshalClaims(c); err == nil {
		t.Fatal("accepted empty validity")
	}
}
func FuzzClaims(f *testing.F) {
	b, _ := agent.MarshalClaims(claims())
	f.Add(b)
	f.Add([]byte("GAR1"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := agent.ParseClaims(b)
		if err != nil {
			return
		}
		encoded, err := agent.MarshalClaims(c)
		if err != nil || !bytes.Equal(encoded, b) {
			t.Fatalf("noncanonical encoding accepted: %v", err)
		}
	})
}
