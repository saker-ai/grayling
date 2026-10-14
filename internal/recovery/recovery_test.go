package recovery

import (
	"strings"
	"testing"

	"github.com/saker-ai/grayling/pkg/model"
)

func TestRecoveryGateRejectsPartialAndWrongKey(t *testing.T) {
	point := &Point{Version: 1, AuthSHA256: digest("correct-auth-secret"), HMACKeysSHA256: digest("pg-key-ring"),
		Migrations: []int{1, 2, 3}, ControlVersion: 7, EventCount: 14, TokenCount: 2, PostgresSHA256: digest("postgres"), GonacosSHA256: digest("gonacos"),
		Environments: []Environment{{Namespace: "grayling-w-e", Revision: 4,
			Flags: []model.ManifestEntry{{Key: "checkout", Revision: 2, DataID: "grayling.flag.checkout.r2", Hash: "abc"}}}},
	}
	path := t.TempDir() + "/point.json"
	if err := Write(path, point); err != nil {
		t.Fatal(err)
	}
	if err := Verify(path, point); err != nil {
		t.Fatalf("matching recovery point: %v", err)
	}
	wrongKey := *point
	wrongKey.AuthSHA256 = digest("wrong-auth-secret")
	if err := Verify(path, &wrongKey); err == nil || !strings.Contains(err.Error(), "auth secret mismatch") {
		t.Fatalf("wrong key: %v", err)
	}
	oldPG := *point
	oldPG.EventCount--
	if err := Verify(path, &oldPG); err == nil || !strings.Contains(err.Error(), "recovery point mismatch") {
		t.Fatalf("old postgres: %v", err)
	}
	changedPG := *point
	changedPG.PostgresSHA256 = digest("same count, different content")
	if err := Verify(path, &changedPG); err == nil || !strings.Contains(err.Error(), "recovery point mismatch") {
		t.Fatalf("changed postgres content: %v", err)
	}
	oldNacos := *point
	oldNacos.Environments = []Environment{{Namespace: "grayling-w-e", Revision: 3}}
	if err := Verify(path, &oldNacos); err == nil || !strings.Contains(err.Error(), "recovery point mismatch") {
		t.Fatalf("old gonacos: %v", err)
	}
	missingKey := *point
	missingKey.HMACKeysSHA256 = digest("different-ring")
	if err := Verify(path, &missingKey); err == nil || !strings.Contains(err.Error(), "HMAC key ring mismatch") {
		t.Fatalf("missing key: %v", err)
	}
	changedSnapshot := *point
	changedSnapshot.GonacosSHA256 = digest("different AI resource with same flag manifest")
	if err := Verify(path, &changedSnapshot); err == nil || !strings.Contains(err.Error(), "gonacos Snapshot mismatch") {
		t.Fatalf("changed full gonacos snapshot: %v", err)
	}
}

func TestDigestSnapshotIgnoresMapInsertionOrder(t *testing.T) {
	a, err := DigestSnapshot(map[string]any{"auth": map[string]any{"users": 2, "tokens": 1}, "ai": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DigestSnapshot(map[string]any{"ai": "v1", "auth": map[string]any{"tokens": 1, "users": 2}})
	if err != nil || a != b {
		t.Fatalf("unstable snapshot digest: %q != %q: %v", a, b, err)
	}
}
