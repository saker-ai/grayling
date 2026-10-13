package platform

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExposureTicketOpaqueScopedAndRotatable(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	claim := ExposureTicket{
		WorkspaceID: "workspace", EnvironmentID: "production", ExperimentID: "experiment",
		Epoch: 1, FlagKey: "flag", FlagRevision: 2, Variant: "treatment",
		UnitHash: strings.Repeat("a", 64), Source: "split", DecisionID: "decision",
	}
	token, expiry, err := store.SignExposureTicket(claim)
	if err != nil {
		t.Fatal(err)
	}
	if !expiry.After(time.Now()) || strings.Contains(token, claim.UnitHash) {
		t.Fatalf("claim was exposed or expired: %q", token)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || json.Valid(decoded) {
		t.Fatalf("ticket must be opaque ciphertext: %v", err)
	}
	got, err := store.VerifyExposureTicket(token)
	if err != nil || got.UnitHash != claim.UnitHash || got.DecisionID != claim.DecisionID {
		t.Fatalf("verify: %+v, %v", got, err)
	}
	decoded[len(decoded)-1] ^= 1
	if _, err := store.VerifyExposureTicket(base64.RawURLEncoding.EncodeToString(decoded)); !errors.Is(err, ErrValidation) {
		t.Fatalf("tampered ticket accepted: %v", err)
	}
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RotateHMACKey(hex.EncodeToString(newKey)); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.VerifyExposureTicket(token)
	if err != nil {
		t.Fatalf("ticket lost across rotation: %v", err)
	}
	if rotated.UnitHash != hashUnitLayer(hex.EncodeToString(newKey), claim.WorkspaceID, claim.UnitHash) {
		t.Fatal("ticket hash did not follow the unit rotation")
	}
}

func TestExposureTicketExpired(t *testing.T) {
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claim := ExposureTicket{
		Version: 1, WorkspaceID: "ws", EnvironmentID: "env", ExperimentID: "exp",
		Epoch: 1, FlagKey: "flag", Variant: "control", UnitHash: "hash", KeyDepth: 1,
		Source: "split", DecisionID: "dec", IssuedAt: now.Add(-20 * time.Minute),
		ExpiresAt: now.Add(-5 * time.Minute),
	}
	payload, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := exposureTicketCipher(store.data.HMACKeyRing[0])
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, payload, nil))
	if _, err := store.VerifyExposureTicket(token); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired ticket accepted: %v", err)
	}
}
