package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// RuntimeWatermark keeps generation and each independently ordered version.
// External Minimum must live outside the cache directory to resist whole-file replay.
type RuntimeWatermark struct {
	ControlPlaneEpoch      uint64   `json:"controlPlaneEpoch,string"`
	Generation             uint64   `json:"generation,string"`
	ActivationRevision     uint64   `json:"activationRevision,string"`
	BundleArtifactRevision uint64   `json:"bundleArtifactRevision,string"`
	PolicyRevision         uint64   `json:"policyRevision,string"`
	DiscoveryRevision      uint64   `json:"discoveryRevision,string"`
	PreparedRevision       uint64   `json:"preparedRevision,string"`
	RevocationEpoch        uint64   `json:"revocationEpoch,string"`
	BundleHash             [32]byte `json:"bundleHash"`
	ActivationHash         [32]byte `json:"activationHash"`
	PolicyHash             [32]byte `json:"policyHash"`
	InputHash              [32]byte `json:"inputHash"`
}

func (m RuntimeWatermark) valid() bool {
	return m == (RuntimeWatermark{}) || m.ControlPlaneEpoch > 0 && m.Generation > 0 && m.ActivationRevision > 0 && m.BundleArtifactRevision > 0 && m.PolicyRevision > 0 && m.DiscoveryRevision > 0 && m.PreparedRevision > 0 && m.BundleHash != [32]byte{} && m.ActivationHash != [32]byte{} && m.PolicyHash != [32]byte{} && m.InputHash != [32]byte{}
}
func runtimeAbove(a, b RuntimeWatermark) bool {
	if !a.valid() || !b.valid() {
		return false
	}
	if b == (RuntimeWatermark{}) {
		return true
	}
	if a.ControlPlaneEpoch != b.ControlPlaneEpoch || a.Generation < b.Generation || a.PolicyRevision < b.PolicyRevision || a.DiscoveryRevision < b.DiscoveryRevision || a.RevocationEpoch < b.RevocationEpoch {
		return false
	}
	if a.PolicyRevision == b.PolicyRevision && a.PolicyHash != b.PolicyHash {
		return false
	}
	if a.BundleHash == b.BundleHash && a.BundleArtifactRevision < b.BundleArtifactRevision {
		return false
	}
	if a.Generation > b.Generation {
		return true
	}
	if a.BundleHash != b.BundleHash || a.PolicyRevision != b.PolicyRevision || a.PreparedRevision < b.PreparedRevision || a.ActivationRevision < b.ActivationRevision {
		return false
	}
	if a.ActivationRevision == b.ActivationRevision {
		return a == b
	}
	return true
}

// RuntimeInput is cloned before verification/construction. Its three signatures
// and dependency joins are checked together, never trusted because a file exists.
type RuntimeInput struct{ Activation, Bundle, Policy *agentv1.SignedResource }
type runtimeRecord struct {
	Mark                        RuntimeWatermark `json:"mark"`
	Command                     string           `json:"command"`
	VerifiedMs                  int64            `json:"verifiedMs,string"`
	ClockHighMs                 int64            `json:"clockHighMs,string"`
	RevocationEpoch             uint64           `json:"revocationEpoch,string"`
	RevocationArtifactRevision  uint64           `json:"revocationArtifactRevision,string,omitzero"`
	RevocationArtifactHash      [32]byte         `json:"revocationArtifactHash,omitzero"`
	RevocationControlPlaneEpoch uint64           `json:"revocationControlPlaneEpoch,string,omitzero"`
	Cancelled                   [][32]byte       `json:"cancelled,omitempty"`
	RevokeDigest                [32]byte         `json:"revokeDigest"`
	Revoked                     [][32]byte       `json:"revoked"`
	Inputs                      [3][]byte        `json:"-"`
}

const runtimeMetaMax = 65536
const maxRuntimeRecordBytes = 3*MaxResourceBytes + runtimeMetaMax + 20

func runtimeInputBytes(in RuntimeInput) ([3][]byte, error) {
	var data [3][]byte
	for i, r := range []*agentv1.SignedResource{in.Activation, in.Bundle, in.Policy} {
		if r == nil || proto.Size(r) > MaxResourceBytes {
			return data, ErrResource
		}
		body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(r)
		if err != nil {
			return data, err
		}
		data[i] = body
	}
	return data, nil
}
func runtimeInputs(data [3][]byte) (RuntimeInput, error) {
	values := make([]*agentv1.SignedResource, 3)
	for i, body := range data {
		values[i] = new(agentv1.SignedResource)
		if len(body) == 0 || len(body) > MaxResourceBytes || (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(body, values[i]) != nil {
			return RuntimeInput{}, ErrCacheCorrupt
		}
	}
	return RuntimeInput{Activation: values[0], Bundle: values[1], Policy: values[2]}, nil
}
func runtimeInputHash(data [3][]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("grayling.agent.v1.runtime-inputs\x00"))
	for _, body := range data {
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(body))))
		h.Write(body)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func encodeRuntime(r runtimeRecord) ([]byte, error) {
	if !r.Mark.valid() || r.VerifiedMs < 0 || r.ClockHighMs < r.VerifiedMs || len(r.Revoked) > maxRevokedBundles || r.RevocationEpoch < r.Mark.RevocationEpoch {
		return nil, ErrCacheCorrupt
	}
	if r.Mark == (RuntimeWatermark{}) {
		if r.VerifiedMs != 0 || r.Command != "" {
			return nil, ErrCacheCorrupt
		}
		for _, body := range r.Inputs {
			if len(body) != 0 {
				return nil, ErrCacheCorrupt
			}
		}
	} else if r.VerifiedMs == 0 || !discoveryID(r.Command) || runtimeInputHash(r.Inputs) != r.Mark.InputHash {
		return nil, ErrCacheCorrupt
	}
	for i, hash := range r.Revoked {
		if hash == [32]byte{} || i > 0 && bytes.Compare(r.Revoked[i-1][:], hash[:]) >= 0 {
			return nil, ErrCacheCorrupt
		}
	}
	for i, hash := range r.Cancelled {
		if !revokedSet(r.Revoked)[hash] || i > 0 && bytes.Compare(r.Cancelled[i-1][:], hash[:]) >= 0 {
			return nil, ErrCacheCorrupt
		}
	}
	if (r.RevocationArtifactRevision == 0) != (r.RevocationArtifactHash == [32]byte{}) || r.RevocationArtifactRevision != 0 && (r.RevocationControlPlaneEpoch == 0) {
		return nil, ErrCacheCorrupt
	}
	if r.RevocationControlPlaneEpoch != 0 && (r.RevocationEpoch == 0 || r.Mark.ControlPlaneEpoch != 0 && r.Mark.ControlPlaneEpoch != r.RevocationControlPlaneEpoch) {
		return nil, ErrCacheCorrupt
	}
	meta, err := json.Marshal(r, json.Deterministic(true))
	if err != nil || len(meta) > runtimeMetaMax {
		return nil, ErrCacheCorrupt
	}
	b := binary.BigEndian.AppendUint32([]byte("GRV1"), uint32(len(meta)))
	b = append(b, meta...)
	for _, body := range r.Inputs {
		if len(body) > MaxResourceBytes {
			return nil, ErrCacheCorrupt
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(body)))
		b = append(b, body...)
	}
	return b, nil
}
func decodeRuntime(b []byte) (runtimeRecord, error) {
	var r runtimeRecord
	if len(b) < 20 || len(b) > maxRuntimeRecordBytes || string(b[:4]) != "GRV1" {
		return r, ErrCacheCorrupt
	}
	rest := b[4:]
	take := func(maximum int) ([]byte, bool) {
		if len(rest) < 4 {
			return nil, false
		}
		size := uint64(binary.BigEndian.Uint32(rest))
		rest = rest[4:]
		if size > uint64(maximum) || size > uint64(len(rest)) {
			return nil, false
		}
		out := rest[:int(size)]
		rest = rest[int(size):]
		return out, true
	}
	meta, ok := take(runtimeMetaMax)
	if !ok || json.Unmarshal(meta, &r, json.RejectUnknownMembers(true)) != nil {
		return r, ErrCacheCorrupt
	}
	for i := range 3 {
		body, ok := take(MaxResourceBytes)
		if !ok {
			return r, ErrCacheCorrupt
		}
		r.Inputs[i] = bytes.Clone(body)
	}
	canonical, err := encodeRuntime(r)
	if len(rest) != 0 || err != nil || !bytes.Equal(canonical, b) {
		return runtimeRecord{}, ErrCacheCorrupt
	}
	return r, nil
}
func runtimeMAC(key, binding, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("grayling.agent.v1.runtime-cache\x00"))
	m.Write(binding)
	m.Write(body)
	return m.Sum(nil)
}
func runtimeFresh(r runtimeRecord, now time.Time, maxAge time.Duration) error {
	if r.Mark == (RuntimeWatermark{}) {
		return ErrCacheNotReady
	}
	if !now.Before(time.UnixMilli(r.VerifiedMs).Add(maxAge)) {
		return ErrPolicyStale
	}
	return nil
}
