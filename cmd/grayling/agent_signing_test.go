package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func agentSigningFixture(t *testing.T) (time.Time, ed25519.PrivateKey, string, string, agentTrustFile) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x51}, 32))
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.pem")
	rootsFile := filepath.Join(dir, "roots.json")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := agentTrustFile{SchemaVersion: 1, Keys: []agentTrustEntry{{ID: "current-2026", PublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}}
	writeAgentTrust(t, rootsFile, doc)
	return now, key, keyFile, rootsFile, doc
}
func writeAgentTrust(t *testing.T, path string, doc agentTrustFile) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSigningIndependentTrustAndVerification(t *testing.T) {
	now, key, keyFile, rootsFile, _ := agentSigningFixture(t)
	material, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, true, now)
	if err != nil || material.KeyID != "current-2026" || !material.Key.Equal(key) || material.Verifier == nil {
		t.Fatal("valid signing configuration unavailable", err)
	}
	payload := []byte("signed fixture")
	hash := sha256.Sum256(payload)
	scope := client.Scope{WorkspaceID: "workspace", EnvironmentID: "prod", ServiceKey: "checkout"}
	signed, err := control.SignResource(client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing", ControlPlaneEpoch: 1, Revision: 1, BundleHash: hash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: material.KeyID}, payload, material.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := material.Verifier.Verify(signed, scope, agentv1.ResourceKind_RESOURCE_KIND_POLICY, "routing", client.Watermark{}, now); err != nil {
		t.Fatal("loaded independent root does not verify signature", err)
	}
	rootsOnly, err := loadAgentSigning(true, "", "", rootsFile, false, now)
	if err != nil || len(rootsOnly.Key) != 0 || rootsOnly.Verifier == nil {
		t.Fatal("read-only verifier replica unsupported", err)
	}
	if material, err := loadAgentSigning(false, "", "", "", false, now); err != nil || material.Verifier != nil {
		t.Fatal("defaults enable signing", err)
	}
}

func TestAgentSigningRejectsInvalidCombinations(t *testing.T) {
	now, _, keyFile, rootsFile, _ := agentSigningFixture(t)
	for _, tc := range []struct {
		enabled        bool
		id, key, roots string
		refresh        bool
	}{
		{false, "current-2026", keyFile, rootsFile, false},
		{false, "", "", rootsFile, false},
		{false, "", "", "", true},
		{true, "current-2026", "", rootsFile, false},
		{true, "", keyFile, rootsFile, false},
		{true, "current-2026", keyFile, "", false},
		{true, "", "", rootsFile, true},
		{true, "injected\nvalue", keyFile, rootsFile, false},
		{true, "missing-root", keyFile, rootsFile, false},
	} {
		if _, err := loadAgentSigning(tc.enabled, tc.id, tc.key, tc.roots, tc.refresh, now); err == nil {
			t.Fatal("invalid combination accepted")
		}
	}
}

func TestAgentSigningRejectsMalformedTrustAndMismatchedKey(t *testing.T) {
	now, _, keyFile, rootsFile, original := agentSigningFixture(t)
	cases := []struct {
		name   string
		change func(*agentTrustFile)
	}{
		{"schema", func(d *agentTrustFile) { d.SchemaVersion = 2 }},
		{"empty", func(d *agentTrustFile) { d.Keys = nil }},
		{"duplicate", func(d *agentTrustFile) { d.Keys = append(d.Keys, d.Keys[0]) }},
		{"expired", func(d *agentTrustFile) { d.Keys[0].NotAfter = now }},
		{"future", func(d *agentTrustFile) { d.Keys[0].NotBefore = now.Add(time.Minute) }},
		{"short signing lifetime", func(d *agentTrustFile) { d.Keys[0].NotAfter = now.Add(time.Minute) }},
		{"public key length", func(d *agentTrustFile) { d.Keys[0].PublicKey = base64.StdEncoding.EncodeToString([]byte{1}) }},
		{"noncanonical public key", func(d *agentTrustFile) { d.Keys[0].PublicKey += "\n" }},
		{"mismatch", func(d *agentTrustFile) {
			d.Keys[0].PublicKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
		}},
		{"too many", func(d *agentTrustFile) {
			for i := range 32 {
				entry := d.Keys[0]
				entry.ID = fmt.Sprintf("extra-%d", i)
				d.Keys = append(d.Keys, entry)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := original
			doc.Keys = append([]agentTrustEntry(nil), original.Keys...)
			tc.change(&doc)
			writeAgentTrust(t, rootsFile, doc)
			if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err == nil {
				t.Fatal("invalid trust accepted")
			}
		})
	}
	valid, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	unknown := string(valid[:len(valid)-1]) + `,"unknown":true}`
	duplicate := strings.Replace(string(valid), `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1)
	for _, raw := range []string{unknown, duplicate, strings.Repeat("x", (32<<10)+1)} {
		if err := os.WriteFile(rootsFile, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err == nil {
			t.Fatal("invalid root file accepted")
		}
	}
}

func TestAgentSigningMaterialPermissionsTypesAndNoSecretErrors(t *testing.T) {
	now, _, keyFile, rootsFile, doc := agentSigningFixture(t)
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	if err := os.Chmod(keyFile, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rootsFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err != nil {
		t.Fatal("read-only secret or public roots rejected", err)
	}
	if err := os.Chmod(rootsFile, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err == nil {
		t.Fatal("writable trust accepted")
	}
	if err := os.Chmod(rootsFile, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "key-link")
	if err := os.Symlink(keyFile, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentSigning(true, "current-2026", link, rootsFile, false, now); err == nil {
		t.Fatal("symlink secret accepted")
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "private-material-must-not-appear-in-error"
	for _, raw := range []string{secret, strings.Repeat(secret, 200)} {
		if err := os.WriteFile(keyFile, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("invalid material accepted or exposed")
		}
	}
	writeAgentTrust(t, rootsFile, doc)
	if _, err := readAgentMaterial(t.TempDir(), true, 4096); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestAgentSigningRejectsOtherKeyFormatsAndExtraBlocks(t *testing.T) {
	now, _, keyFile, rootsFile, _ := agentSigningFixture(t)
	valid, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(valid)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKCS8PrivateKey(other)
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		append(append([]byte(nil), valid...), valid...),
		append([]byte("untrusted-prefix\n"), valid...),
		pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: block.Bytes}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: block.Bytes}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER}),
	}
	for _, raw := range cases {
		if err := os.WriteFile(keyFile, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now); err == nil {
			t.Fatal("ambiguous or unsupported private key accepted")
		}
	}
	rootLink := filepath.Join(t.TempDir(), "roots-link")
	if err := os.Symlink(rootsFile, rootLink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentSigning(true, "", "", rootLink, false, now); err == nil {
		t.Fatal("symlink trust configuration accepted")
	}
}

func TestAgentSigningTrustOverlapRetainsAndStagesRoots(t *testing.T) {
	now, key, keyFile, rootsFile, doc := agentSigningFixture(t)
	old := doc.Keys[0]
	old.ID = "retained"
	old.NotBefore = now.Add(-2 * time.Hour)
	old.NotAfter = now.Add(-time.Hour)
	future := doc.Keys[0]
	future.ID = "staged"
	future.NotBefore = now.Add(time.Hour)
	future.NotAfter = now.Add(2 * time.Hour)
	doc.Keys = append(doc.Keys, old, future)
	writeAgentTrust(t, rootsFile, doc)
	material, err := loadAgentSigning(true, "current-2026", keyFile, rootsFile, false, now)
	if err != nil {
		t.Fatal("valid overlap configuration rejected", err)
	}
	payload := []byte("root overlap fixture")
	hash := sha256.Sum256(payload)
	scope := client.Scope{WorkspaceID: "workspace", EnvironmentID: "prod", ServiceKey: "checkout"}
	signed, err := control.SignResource(client.Claims{Scope: scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "routing", ControlPlaneEpoch: 1, Revision: 1, BundleHash: hash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "staged"}, payload, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := material.Verifier.Verify(signed, scope, agentv1.ResourceKind_RESOURCE_KIND_POLICY, "routing", client.Watermark{}, now); err == nil {
		t.Fatal("future root became active before notBefore")
	}
}
