package agent_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/agent"
)

func TestPythonSignatureFixture(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../test/agent/fixtures/signed-policy-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Claims    string `json:"claimsHex"`
		Payload   string `json:"payloadHex"`
		Signature string `json:"signatureHex"`
		PublicKey string `json:"publicKeyHex"`
		KeyID     string `json:"keyId"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte {
		t.Helper()
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	now := time.UnixMilli(1700000000000)
	v, err := agent.NewVerifier(map[string]agent.TrustKey{fixture.KeyID: {PublicKey: ed25519.PublicKey(decode(fixture.PublicKey)), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	r := &agentv1.SignedResource{Claims: decode(fixture.Claims), Payload: decode(fixture.Payload), Signature: decode(fixture.Signature), KeyId: fixture.KeyID}
	c, err := v.Verify(r, agent.Scope{WorkspaceID: "workspace-1", EnvironmentID: "env-1", ServiceKey: "checkout"}, agentv1.ResourceKind_RESOURCE_KIND_POLICY, "policy", agent.Watermark{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != 9007199254740993 {
		t.Fatal("cross-language revision lost precision")
	}
}
