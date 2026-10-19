package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"time"

	client "github.com/saker-ai/grayling/pkg/agent"
)

type agentSigningMaterial struct {
	KeyID    string
	Key      ed25519.PrivateKey
	Verifier *client.Verifier
}

type agentTrustFile struct {
	SchemaVersion uint32            `json:"schemaVersion"`
	Keys          []agentTrustEntry `json:"keys"`
}
type agentTrustEntry struct {
	ID        string    `json:"id"`
	PublicKey string    `json:"publicKey"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
}

// Signing and verification have separate inputs; possessing a private key never
// silently creates a trust root. This runs before the App opens any stores.
func loadAgentSigning(enabled bool, keyID, keyPath, rootsPath string, refresh bool, now time.Time) (agentSigningMaterial, error) {
	var result agentSigningMaterial
	if !enabled && (keyID != "" || keyPath != "" || rootsPath != "" || refresh) {
		return result, errors.New("Agent signing, trust and refresh settings require -agent")
	}
	if (keyID == "") != (keyPath == "") {
		return result, errors.New("-agent-signing-key-id and -agent-signing-key-file must be provided together")
	}
	if keyPath != "" && (rootsPath == "" || !agentKeyIDValid(keyID)) {
		return result, errors.New("Agent signing requires an explicit valid key ID and -agent-trust-roots-file")
	}
	if refresh && (keyPath == "" || rootsPath == "") {
		return result, errors.New("-agent-revocation-refresh requires signing key and independent trust roots")
	}
	if rootsPath == "" {
		return result, nil
	}
	raw, err := readAgentMaterial(rootsPath, false, 32<<10)
	if err != nil {
		return result, errors.New("unable to read Agent trust roots: require a bounded regular file without group/world write or execute permissions")
	}
	var document agentTrustFile
	if json.Unmarshal(raw, &document, json.RejectUnknownMembers(true)) != nil || document.SchemaVersion != 1 || len(document.Keys) == 0 || len(document.Keys) > 32 {
		return result, errors.New("invalid Agent trust roots document")
	}
	keys := make(map[string]client.TrustKey, len(document.Keys))
	active := false
	for _, entry := range document.Keys {
		public, err := base64.StdEncoding.Strict().DecodeString(entry.PublicKey)
		if err != nil || len(public) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(public) != entry.PublicKey || !agentKeyIDValid(entry.ID) || entry.NotBefore.IsZero() || entry.NotAfter.IsZero() || !entry.NotAfter.After(entry.NotBefore) {
			return result, errors.New("invalid Agent trust root entry")
		}
		if _, duplicate := keys[entry.ID]; duplicate {
			return result, errors.New("duplicate Agent trust root ID")
		}
		keys[entry.ID] = client.TrustKey{PublicKey: public, NotBefore: entry.NotBefore, NotAfter: entry.NotAfter}
		active = active || (!now.Before(entry.NotBefore) && now.Before(entry.NotAfter))
	}
	if !active {
		return result, errors.New("Agent trust roots contain no currently accepted key")
	}
	result.Verifier, err = client.NewVerifier(keys)
	if err != nil {
		return agentSigningMaterial{}, errors.New("invalid Agent trust roots")
	}
	if keyPath == "" {
		return result, nil
	}
	raw, err = readAgentMaterial(keyPath, true, 4096)
	if err != nil {
		return agentSigningMaterial{}, errors.New("unable to read Agent signing key: require a bounded regular owner-only file (0400 or 0600)")
	}
	defer clear(raw)
	raw = bytes.TrimSpace(raw)
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || !bytes.HasPrefix(raw, []byte("-----BEGIN PRIVATE KEY-----")) {
		return agentSigningMaterial{}, errors.New("Agent signing key must be one unencrypted PKCS8 Ed25519 PEM block")
	}
	defer clear(block.Bytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return agentSigningMaterial{}, errors.New("invalid Agent PKCS8 signing key")
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(private) != ed25519.PrivateKeySize {
		return agentSigningMaterial{}, errors.New("Agent signing key must be Ed25519")
	}
	root, found := keys[keyID]
	if !found || !bytes.Equal(root.PublicKey, private.Public().(ed25519.PublicKey)) || now.Before(root.NotBefore) || !now.Add(time.Minute).Before(root.NotAfter) {
		return agentSigningMaterial{}, errors.New("Agent signing key must match an independently configured root valid for more than one minute")
	}
	result.KeyID = keyID
	result.Key = private
	return result, nil
}

func agentKeyIDValid(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func readAgentMaterial(path string, secret bool, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	allowed := func(info os.FileInfo) bool {
		mode := info.Mode()
		if !mode.IsRegular() || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() < 1 || info.Size() > limit {
			return false
		}
		if secret {
			return mode.Perm() == 0o400 || mode.Perm() == 0o600
		}
		return mode.Perm()&0o133 == 0
	}
	if !allowed(info) {
		return nil, errors.New("invalid material file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !allowed(opened) {
		return nil, errors.New("material file changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit || int64(len(data)) != opened.Size() {
		return nil, errors.New("invalid material file size")
	}
	after, err := file.Stat()
	if err != nil || !allowed(after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("material file changed during read")
	}
	return data, nil
}
