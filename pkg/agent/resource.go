// Package agent implements transport-independent Agent resource verification.
package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
	"unicode/utf8"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

const (
	MaxResourceBytes = 64 << 20
	maxClaimsBytes   = 4096
	// SignatureDomain separates resource signatures from all other uses of a key.
	SignatureDomain = "grayling.agent.v1.signed-resource\x00"
)

var (
	ErrResource = errors.New("agent: invalid signed resource")
	ErrScope    = errors.New("agent: resource scope mismatch")
	ErrExpired  = errors.New("agent: resource expired or not yet valid")
	ErrRollback = errors.New("agent: resource revision rollback or conflict")
)

// Scope contains immutable internal identifiers, never display names.
type Scope struct {
	WorkspaceID   string `json:"workspaceId"`
	EnvironmentID string `json:"environmentId"`
	ServiceKey    string `json:"serviceKey"`
}

// Claims uses a fixed, versioned binary encoding specified in the protocol ADR.
// BundleHash and PayloadHash contain raw SHA-256 bytes, not hexadecimal text.
type Claims struct {
	Scope                       Scope
	Kind                        agentv1.ResourceKind
	Key                         string
	ControlPlaneEpoch, Revision uint64
	BundleHash                  [32]byte
	IssuedUnixMs, ExpiresUnixMs int64
	KeyID                       string
	PayloadHash                 [32]byte
}

// Watermark is scoped to exactly one (scope, kind, key). Persist it atomically
// with the accepted cache; callers must serialize competing resource installs.
// A higher control-plane epoch requires independent recovery authorization.
type Watermark struct {
	ControlPlaneEpoch, Revision uint64
	PayloadHash, BundleHash     [32]byte
}

func validText(s string) bool {
	if s == "" || len(s) > 512 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func (c Claims) valid() bool {
	return validText(c.Scope.WorkspaceID) && validText(c.Scope.EnvironmentID) && validText(c.Scope.ServiceKey) && validText(c.Key) && validText(c.KeyID) && c.Kind >= agentv1.ResourceKind_RESOURCE_KIND_POLICY && c.Kind <= agentv1.ResourceKind_RESOURCE_KIND_ACTIVATION && c.ControlPlaneEpoch > 0 && c.Revision > 0 && c.IssuedUnixMs > 0 && c.ExpiresUnixMs > c.IssuedUnixMs && c.BundleHash != [32]byte{} && c.PayloadHash != [32]byte{}
}

// MarshalClaims produces the sole supported canonical claims encoding.
func MarshalClaims(c Claims) ([]byte, error) {
	if !c.valid() {
		return nil, ErrResource
	}
	b := []byte{'G', 'A', 'R', '1'}
	for _, s := range []string{c.Scope.WorkspaceID, c.Scope.EnvironmentID, c.Scope.ServiceKey} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
		b = append(b, s...)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(c.Kind))
	b = binary.BigEndian.AppendUint16(b, uint16(len(c.Key)))
	b = append(b, c.Key...)
	b = binary.BigEndian.AppendUint64(b, c.ControlPlaneEpoch)
	b = binary.BigEndian.AppendUint64(b, c.Revision)
	b = append(b, c.BundleHash[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(c.IssuedUnixMs))
	b = binary.BigEndian.AppendUint64(b, uint64(c.ExpiresUnixMs))
	b = binary.BigEndian.AppendUint16(b, uint16(len(c.KeyID)))
	b = append(b, c.KeyID...)
	b = append(b, c.PayloadHash[:]...)
	return b, nil
}

// ParseClaims rejects alternate encodings and extensions until a new magic
// version is defined. This avoids silently ignoring signed authorization fields.
func ParseClaims(b []byte) (Claims, error) {
	var c Claims
	if len(b) > maxClaimsBytes || len(b) < 4 || !bytes.Equal(b[:4], []byte("GAR1")) {
		return c, ErrResource
	}
	rest := b[4:]
	text := func() (string, bool) {
		if len(rest) < 2 {
			return "", false
		}
		n := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if n > len(rest) {
			return "", false
		}
		s := string(rest[:n])
		rest = rest[n:]
		return s, validText(s)
	}
	var ok bool
	if c.Scope.WorkspaceID, ok = text(); !ok {
		return Claims{}, ErrResource
	}
	if c.Scope.EnvironmentID, ok = text(); !ok {
		return Claims{}, ErrResource
	}
	if c.Scope.ServiceKey, ok = text(); !ok {
		return Claims{}, ErrResource
	}
	if len(rest) < 4 {
		return Claims{}, ErrResource
	}
	c.Kind = agentv1.ResourceKind(binary.BigEndian.Uint32(rest))
	rest = rest[4:]
	if c.Key, ok = text(); !ok {
		return Claims{}, ErrResource
	}
	if len(rest) < 64 {
		return Claims{}, ErrResource
	}
	c.ControlPlaneEpoch = binary.BigEndian.Uint64(rest)
	c.Revision = binary.BigEndian.Uint64(rest[8:])
	copy(c.BundleHash[:], rest[16:48])
	c.IssuedUnixMs = int64(binary.BigEndian.Uint64(rest[48:]))
	c.ExpiresUnixMs = int64(binary.BigEndian.Uint64(rest[56:]))
	rest = rest[64:]
	if c.KeyID, ok = text(); !ok {
		return Claims{}, ErrResource
	}
	if len(rest) != 32 {
		return Claims{}, ErrResource
	}
	copy(c.PayloadHash[:], rest)
	if !c.valid() {
		return Claims{}, ErrResource
	}
	return c, nil
}

// TrustKey has explicit acceptance bounds; overlap permits rotation, while
// removing a key from a newly installed Verifier revokes it. Neither heartbeat
// nor a cached resource extends these bounds or the signed resource's expiry.
type TrustKey struct {
	PublicKey           ed25519.PublicKey
	NotBefore, NotAfter time.Time
}
type Verifier struct{ keys map[string]TrustKey }

func NewVerifier(keys map[string]TrustKey) (*Verifier, error) {
	if len(keys) == 0 || len(keys) > 32 {
		return nil, ErrResource
	}
	v := &Verifier{keys: make(map[string]TrustKey, len(keys))}
	for id, key := range keys {
		if !validText(id) || len(key.PublicKey) != ed25519.PublicKeySize || !key.NotAfter.After(key.NotBefore) {
			return nil, ErrResource
		}
		key.PublicKey = bytes.Clone(key.PublicKey)
		v.keys[id] = key
	}
	return v, nil
}

// Verify checks integrity, authorization and time before comparing the durable
// watermark. Caller supplies an independently trusted, clock-checked time.
// It never changes the watermark or makes an Agent ready by itself.
func (v *Verifier) Verify(r *agentv1.SignedResource, scope Scope, kind agentv1.ResourceKind, key string, mark Watermark, now time.Time) (Claims, error) {
	if v == nil || r == nil || len(r.Claims) > maxClaimsBytes || len(r.Payload) > MaxResourceBytes || len(r.Signature) != ed25519.SignatureSize {
		return Claims{}, ErrResource
	}
	c, err := ParseClaims(r.Claims)
	if err != nil {
		return Claims{}, err
	}
	root, ok := v.keys[r.KeyId]
	if !ok || c.KeyID != r.KeyId || !ed25519.Verify(root.PublicKey, append([]byte(SignatureDomain), r.Claims...), r.Signature) {
		return Claims{}, ErrResource
	}
	if c.Scope != scope || c.Kind != kind || c.Key != key {
		return Claims{}, ErrScope
	}
	if sha256.Sum256(r.Payload) != c.PayloadHash {
		return Claims{}, ErrResource
	}
	if now.Before(root.NotBefore) || !now.Before(root.NotAfter) || now.UnixMilli() < c.IssuedUnixMs || now.UnixMilli() >= c.ExpiresUnixMs || time.UnixMilli(c.IssuedUnixMs).Before(root.NotBefore) || time.UnixMilli(c.ExpiresUnixMs).After(root.NotAfter) {
		return Claims{}, ErrExpired
	}
	if mark.ControlPlaneEpoch == 0 {
		if mark.Revision != 0 || mark.PayloadHash != [32]byte{} || mark.BundleHash != [32]byte{} {
			return Claims{}, ErrRollback
		}
	} else if mark.Revision == 0 || mark.PayloadHash == [32]byte{} || mark.BundleHash == [32]byte{} || c.ControlPlaneEpoch != mark.ControlPlaneEpoch || c.Revision < mark.Revision || c.Revision == mark.Revision && (c.PayloadHash != mark.PayloadHash || c.BundleHash != mark.BundleHash) {
		return Claims{}, ErrRollback
	}
	return c, nil
}
func (c Claims) Watermark() Watermark {
	return Watermark{ControlPlaneEpoch: c.ControlPlaneEpoch, Revision: c.Revision, PayloadHash: c.PayloadHash, BundleHash: c.BundleHash}
}
