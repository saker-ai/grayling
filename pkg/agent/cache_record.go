package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

var (
	ErrCacheMissing  = errors.New("agent: cache does not exist; explicit initialization required")
	ErrCacheCorrupt  = errors.New("agent: cache integrity or identity check failed")
	ErrCacheBusy     = errors.New("agent: cache already owned by another process")
	ErrCacheNotReady = errors.New("agent: no trusted policy baseline")
	ErrCacheClosed   = errors.New("agent: cache is closed")
	ErrCacheIO       = errors.New("agent: cache persistence outcome unknown")
	ErrClockSkew     = errors.New("agent: local clock moved outside allowed budget")
	ErrPolicyStale   = errors.New("agent: policy freshness expired")
	ErrPolicyRevoked = errors.New("agent: policy bundle revoked")
	ErrTrustChanged  = errors.New("agent: captured policy trust configuration changed")
)

const maxRevokedBundles = 128
const maxCacheRecordBytes = MaxResourceBytes + 8192

type cacheRecord struct {
	mark                         Watermark
	verifiedMs, clockHighMs      int64
	revocationEpoch              uint64
	revokeDigest, recoveryDigest [32]byte
	revoked                      [][32]byte
	resource                     []byte
}

func validWatermark(w Watermark) bool {
	if w == (Watermark{}) {
		return true
	}
	return w.ControlPlaneEpoch > 0 && w.Revision > 0 && w.PayloadHash != [32]byte{} && w.BundleHash != [32]byte{}
}
func aboveFloor(mark, floor Watermark) bool {
	if floor == (Watermark{}) {
		return true
	}
	if mark.ControlPlaneEpoch != floor.ControlPlaneEpoch {
		return mark.ControlPlaneEpoch > floor.ControlPlaneEpoch
	}
	if mark.Revision != floor.Revision {
		return mark.Revision > floor.Revision
	}
	return mark == floor
}
func appendMark(b []byte, m Watermark) []byte {
	b = binary.BigEndian.AppendUint64(b, m.ControlPlaneEpoch)
	b = binary.BigEndian.AppendUint64(b, m.Revision)
	b = append(b, m.PayloadHash[:]...)
	return append(b, m.BundleHash[:]...)
}
func readMark(b []byte) Watermark {
	m := Watermark{ControlPlaneEpoch: binary.BigEndian.Uint64(b), Revision: binary.BigEndian.Uint64(b[8:])}
	copy(m.PayloadHash[:], b[16:48])
	copy(m.BundleHash[:], b[48:80])
	return m
}
func encodeCache(r cacheRecord) ([]byte, error) {
	if !validWatermark(r.mark) || r.verifiedMs < 0 || r.clockHighMs < r.verifiedMs || len(r.revoked) > maxRevokedBundles || len(r.resource) > MaxResourceBytes {
		return nil, ErrCacheCorrupt
	}
	b := appendMark([]byte("GPC1"), r.mark)
	b = binary.BigEndian.AppendUint64(b, uint64(r.verifiedMs))
	b = binary.BigEndian.AppendUint64(b, uint64(r.clockHighMs))
	b = binary.BigEndian.AppendUint64(b, r.revocationEpoch)
	b = append(b, r.revokeDigest[:]...)
	b = append(b, r.recoveryDigest[:]...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(r.revoked)))
	for _, h := range r.revoked {
		b = append(b, h[:]...)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(r.resource)))
	b = append(b, r.resource...)
	return b, nil
}
func decodeCache(b []byte) (cacheRecord, error) {
	var r cacheRecord
	if len(b) < 178 || len(b) > maxCacheRecordBytes || string(b[:4]) != "GPC1" {
		return r, ErrCacheCorrupt
	}
	r.mark = readMark(b[4:84])
	r.verifiedMs = int64(binary.BigEndian.Uint64(b[84:]))
	r.clockHighMs = int64(binary.BigEndian.Uint64(b[92:]))
	r.revocationEpoch = binary.BigEndian.Uint64(b[100:])
	copy(r.revokeDigest[:], b[108:140])
	copy(r.recoveryDigest[:], b[140:172])
	count := int(binary.BigEndian.Uint16(b[172:]))
	if count > maxRevokedBundles || len(b) < 178+count*32 {
		return cacheRecord{}, ErrCacheCorrupt
	}
	rest := b[174:]
	r.revoked = make([][32]byte, count)
	for i := range count {
		copy(r.revoked[i][:], rest[:32])
		rest = rest[32:]
		if r.revoked[i] == [32]byte{} || i > 0 && bytes.Compare(r.revoked[i-1][:], r.revoked[i][:]) >= 0 {
			return cacheRecord{}, ErrCacheCorrupt
		}
	}
	size := int(binary.BigEndian.Uint32(rest))
	rest = rest[4:]
	if size != len(rest) || size > MaxResourceBytes || !validWatermark(r.mark) || r.verifiedMs < 0 || r.clockHighMs < r.verifiedMs {
		return cacheRecord{}, ErrCacheCorrupt
	}
	r.resource = bytes.Clone(rest)
	if size == 0 {
		if r.verifiedMs != 0 {
			return cacheRecord{}, ErrCacheCorrupt
		}
		return r, nil
	}
	signed := new(agentv1.SignedResource)
	if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(rest, signed); err != nil {
		return cacheRecord{}, ErrCacheCorrupt
	}
	claims, err := ParseClaims(signed.Claims)
	if err != nil || claims.Watermark() != r.mark || sha256.Sum256(signed.Payload) != claims.PayloadHash || r.verifiedMs == 0 {
		return cacheRecord{}, ErrCacheCorrupt
	}
	return r, nil
}
func cacheMAC(key, binding, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("grayling.agent.v1.local-policy-cache\x00"))
	m.Write(binding)
	m.Write(payload)
	return m.Sum(nil)
}
func revokedSet(hashes [][32]byte) map[[32]byte]bool {
	m := make(map[[32]byte]bool, len(hashes))
	for _, h := range hashes {
		m[h] = true
	}
	return m
}
func sortedHashes(set map[[32]byte]bool) [][32]byte {
	hashes := make([][32]byte, 0, len(set))
	for hash := range set {
		hashes = append(hashes, hash)
	}
	slices.SortFunc(hashes, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	return hashes
}
