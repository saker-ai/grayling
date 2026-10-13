package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"time"
)

const exposureTicketTTL = 15 * time.Minute

// ExposureTicket binds an onRender exposure to one recorded assignment.
// Its serialized form is encrypted before it leaves the server.
type ExposureTicket struct {
	Version               int       `json:"version"`
	WorkspaceID           string    `json:"workspaceId"`
	EnvironmentID         string    `json:"environmentId"`
	ExperimentID          string    `json:"experimentId"`
	Epoch                 int64     `json:"epoch"`
	FlagKey               string    `json:"flagKey"`
	FlagRevision          int64     `json:"flagRevision"`
	Variant               string    `json:"variant"`
	UnitHash              string    `json:"unitHash"`
	KeyDepth              int       `json:"keyDepth"`
	Source                string    `json:"source"`
	DecisionID            string    `json:"decisionId"`
	PolicyVersion         string    `json:"policyVersion,omitempty"`
	StateVersion          int64     `json:"stateVersion,omitzero"`
	AllocationProbability float64   `json:"allocationProbability,omitzero"`
	IssuedAt              time.Time `json:"issuedAt"`
	ExpiresAt             time.Time `json:"expiresAt"`
}

func validExposureTicket(ticket ExposureTicket) bool {
	return ticket.Version == 1 && ticket.WorkspaceID != "" && ticket.EnvironmentID != "" &&
		ticket.ExperimentID != "" && ticket.Epoch > 0 && ticket.FlagKey != "" &&
		ticket.Variant != "" && ticket.UnitHash != "" && ticket.KeyDepth > 0 && ticket.Source != "" && ticket.DecisionID != "" &&
		!ticket.IssuedAt.IsZero() && ticket.ExpiresAt.After(ticket.IssuedAt) &&
		ticket.ExpiresAt.Sub(ticket.IssuedAt) <= exposureTicketTTL
}

func exposureTicketCipher(keyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("%w: invalid ticket encryption key", ErrInvalidState)
	}
	derived := sha256.Sum256(append([]byte("grayling-exposure-v1\x00"), key...))
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SignExposureTicket returns an encrypted, authenticated, short-lived claim.
// Its random nonce makes equal assignments produce unlinkable tokens.
func (s *Store) SignExposureTicket(ticket ExposureTicket) (string, time.Time, error) {
	now := s.now().UTC()
	ticket.Version = 1
	ticket.IssuedAt = now
	ticket.ExpiresAt = now.Add(exposureTicketTTL)
	s.mu.RLock()
	if len(s.data.HMACKeyRing) == 0 {
		s.mu.RUnlock()
		return "", time.Time{}, fmt.Errorf("%w: no ticket encryption key", ErrInvalidState)
	}
	ticket.KeyDepth = len(s.data.HMACKeyRing)
	key := s.data.HMACKeyRing[ticket.KeyDepth-1]
	s.mu.RUnlock()
	if !validExposureTicket(ticket) {
		return "", time.Time{}, fmt.Errorf("%w: incomplete exposure ticket", ErrValidation)
	}
	payload, err := json.Marshal(ticket)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: encode exposure ticket: %v", ErrValidation, err)
	}
	aead, err := exposureTicketCipher(key)
	if err != nil {
		return "", time.Time{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", time.Time{}, err
	}
	sealed := aead.Seal(nonce, nonce, payload, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), ticket.ExpiresAt, nil
}

// VerifyExposureTicket accepts tickets sealed by any active key in the ring.
func (s *Store) VerifyExposureTicket(token string) (ExposureTicket, error) {
	invalid := fmt.Errorf("%w: invalid exposure token", ErrValidation)
	if len(token) == 0 || len(token) > 4096 {
		return ExposureTicket{}, invalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ExposureTicket{}, invalid
	}
	s.mu.RLock()
	keys := append([]string(nil), s.data.HMACKeyRing...)
	s.mu.RUnlock()
	for _, key := range keys {
		aead, err := exposureTicketCipher(key)
		if err != nil {
			return ExposureTicket{}, err
		}
		if len(sealed) < aead.NonceSize() {
			return ExposureTicket{}, invalid
		}
		payload, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
		if err != nil {
			continue
		}
		var ticket ExposureTicket
		if err := json.Unmarshal(payload, &ticket); err != nil || !validExposureTicket(ticket) {
			return ExposureTicket{}, invalid
		}
		if ticket.KeyDepth > len(keys) || key != keys[ticket.KeyDepth-1] {
			return ExposureTicket{}, invalid
		}
		now := s.now().UTC()
		if now.After(ticket.ExpiresAt) || ticket.IssuedAt.After(now.Add(futureSkew)) {
			return ExposureTicket{}, fmt.Errorf("%w: expired exposure token", ErrInvalidState)
		}
		for _, newerKey := range keys[ticket.KeyDepth:] {
			ticket.UnitHash = hashUnitLayer(newerKey, ticket.WorkspaceID, ticket.UnitHash)
		}
		return ticket, nil
	}
	return ExposureTicket{}, invalid
}
