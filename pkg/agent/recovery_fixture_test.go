package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestPythonRecoveryFixture(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../test/agent/fixtures/recovery-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(key string) []byte {
		t.Helper()
		value, err := hex.DecodeString(fixture[key])
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	claims, err := agent.ParseRecoveryClaims(decode("recoveryHex"))
	if err != nil {
		t.Fatal(err)
	}
	round, err := agent.MarshalRecoveryClaims(claims)
	if err != nil || !bytes.Equal(round, decode("recoveryHex")) {
		t.Fatal("Python GRA1 disagrees with Go encoding")
	}
	f := newCacheFixture(t)
	f.clock.Store(1700000000000)
	root := func(key string) agent.TrustKey {
		return agent.TrustKey{PublicKey: ed25519.PublicKey(decode(key)), NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}
	}
	f.opts.Verifier, err = agent.NewVerifier(map[string]agent.TrustKey{"policy-key": root("policyPublicHex")})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.RecoveryVerifier, err = agent.NewRecoveryVerifier(map[string]agent.TrustKey{"recovery-key": root("recoveryPublicHex")})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Minimum = claims.From
	if claims.From.Revision != 9007199254740993 {
		t.Fatal("Python recovery watermarks lost precision")
	}
	cache := initCache(t, f)
	resource := &agentv1.SignedResource{Claims: decode("targetHex"), Payload: decode("payloadHex"), Signature: decode("resourceSignatureHex"), KeyId: "policy-key"}
	grant := &agent.RecoveryGrant{Claims: decode("recoveryHex"), Signature: decode("recoverySignatureHex")}
	if err := cache.InstallRecovery(t.Context(), resource, grant); err != nil {
		t.Fatal(err)
	}
	view, err := cache.Capture()
	if err != nil {
		t.Fatal(err)
	}
	result, err := view.Evaluate("route", model.EvaluationContext{})
	if err != nil || result.Value != "backend" {
		t.Fatalf("Python recovery evaluation: %+v %v", result, err)
	}
}
