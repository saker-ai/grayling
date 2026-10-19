package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

// Revoke is a trusted local entry point. The adapter must authenticate the
// control-plane command first. Denials persist before covering old captures;
// optional cancellation also reaches retained response bodies and local handlers.
func (m *RuntimeManager) Revoke(ctx context.Context, epoch uint64, bundles [][32]byte, cancelActive bool) error {
	if epoch == 0 || len(bundles) == 0 || len(bundles) > maxRevokedBundles {
		return ErrResource
	}
	requested := map[[32]byte]bool{}
	for _, hash := range bundles {
		if hash == [32]byte{} {
			return ErrResource
		}
		requested[hash] = true
	}
	hashes := sortedHashes(requested)
	body := binary.BigEndian.AppendUint64(nil, epoch)
	if cancelActive {
		body = append(body, 1)
	} else {
		body = append(body, 0)
	}
	for _, hash := range hashes {
		body = append(body, hash[:]...)
	}
	digest := sha256.Sum256(body)
	cancelled := map[[32]byte]bool{}
	if cancelActive {
		for h := range requested {
			cancelled[h] = true
		}
	}
	return m.applyRevocation(ctx, epoch, 0, requested, cancelled, digest, false, nil, nil)
}

// InstallRevocation authenticates a complete catalog before persisting denials.
// Signed proof expiry never removes a denial which was already accepted.
func (m *RuntimeManager) InstallRevocation(ctx context.Context, resource *agentv1.SignedResource) error {
	if resource == nil {
		return ErrResource
	}
	resource = proto.Clone(resource).(*agentv1.SignedResource)
	now, err := m.guard.time()
	if err != nil {
		return err
	}
	d, claims, err := VerifyRevocation(m.guard.verifier.Load(), resource, m.opts.Scope, Watermark{}, now)
	if err != nil {
		return err
	}
	revoked := map[[32]byte]bool{}
	cancelled := map[[32]byte]bool{}
	for _, text := range d.BundleHashes {
		raw, _ := hex.DecodeString(text)
		var h [32]byte
		copy(h[:], raw)
		revoked[h] = true
	}
	for _, text := range d.CancelledHashes {
		raw, _ := hex.DecodeString(text)
		var h [32]byte
		copy(h[:], raw)
		cancelled[h] = true
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(resource)
	if err != nil {
		return err
	}
	proof := &revocationProof{Revision: claims.Revision, Hash: sha256.Sum256(encoded)}
	return m.applyRevocation(ctx, d.Epoch, d.ControlPlaneEpoch, revoked, cancelled, claims.PayloadHash, true, proof, func() error {
		now, err := m.guard.time()
		if err != nil {
			return err
		}
		_, _, err = VerifyRevocation(m.guard.verifier.Load(), resource, m.opts.Scope, Watermark{}, now)
		return err
	})
}

type revocationProof struct {
	Revision uint64
	Hash     [32]byte
}

func (m *RuntimeManager) applyRevocation(ctx context.Context, epoch, controlEpoch uint64, requested, cancelled map[[32]byte]bool, digest [32]byte, cumulative bool, proof *revocationProof, verify func() error) error {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	if verify != nil {
		if err := verify(); err != nil {
			return err
		}
	}
	record, err := m.journal.read(ctx)
	if err != nil {
		m.poison(err)
		return err
	}
	if controlEpoch == 0 {
		controlEpoch = record.Mark.ControlPlaneEpoch
	}
	artifactRevision, artifactHash := record.RevocationArtifactRevision, record.RevocationArtifactHash
	if proof != nil {
		if proof.Revision < artifactRevision || proof.Revision == artifactRevision && proof.Hash != artifactHash {
			return ErrRollback
		}
		if epoch > record.RevocationEpoch && proof.Revision <= artifactRevision {
			return ErrRollback
		}
		artifactRevision, artifactHash = proof.Revision, proof.Hash
	}
	prospective := runtimeRecord{RevocationArtifactRevision: artifactRevision, RevocationArtifactHash: artifactHash, RevocationControlPlaneEpoch: controlEpoch, RevocationEpoch: epoch, RevokeDigest: digest}
	if !revocationAbove(prospective, m.opts.MinimumRevocation) {
		return ErrRollback
	}
	if controlEpoch != 0 && (record.Mark.ControlPlaneEpoch != 0 && record.Mark.ControlPlaneEpoch != controlEpoch || record.RevocationControlPlaneEpoch != 0 && record.RevocationControlPlaneEpoch != controlEpoch) {
		return ErrRollback
	}
	if epoch < record.RevocationEpoch || epoch == record.RevocationEpoch && record.RevokeDigest != digest {
		return ErrRollback
	}
	if epoch == record.RevocationEpoch && (proof == nil || proof.Revision == record.RevocationArtifactRevision) {
		m.cancelBundles(cancelled)
		return nil
	}
	all := revokedSet(record.Revoked)
	oldCancelled := revokedSet(record.Cancelled)
	if cumulative {
		for h := range all {
			if !requested[h] {
				return ErrRollback
			}
		}
		for h := range oldCancelled {
			if !cancelled[h] {
				return ErrRollback
			}
		}
	}
	for h := range cancelled {
		oldCancelled[h] = true
	}
	for hash := range requested {
		all[hash] = true
	}
	if len(all) > maxRevokedBundles {
		return ErrResource
	}
	now, err := m.guard.time()
	if err != nil {
		return err
	}
	record.RevocationArtifactRevision = artifactRevision
	record.RevocationArtifactHash = artifactHash
	record.RevocationEpoch = epoch
	record.RevokeDigest = digest
	record.Cancelled = sortedHashes(oldCancelled)
	if controlEpoch != 0 {
		record.RevocationControlPlaneEpoch = controlEpoch
	}
	record.Revoked = sortedHashes(all)
	record.ClockHighMs = max(record.ClockHighMs, now.UnixMilli())
	if err := m.journal.write(ctx, record); err != nil {
		m.poison(err)
		m.cancelBundles(cancelled)
		return err
	}
	m.guard.revoked.Store(&all)
	m.cancelBundles(oldCancelled)
	return nil
}
func (m *RuntimeManager) cancelBundles(set map[[32]byte]bool) {
	m.mu.Lock()
	views := make([]*runtimeSnapshot, 0, len(m.views))
	for view := range m.views {
		if set[view.candidate.metadata.Watermark.BundleHash] {
			views = append(views, view)
		}
	}
	m.mu.Unlock()
	for _, view := range views {
		view.cancelRequests()
	}
}

// SetVerifier replaces externally provisioned trust without clearing any floor.
func (m *RuntimeManager) SetVerifier(ctx context.Context, v *Verifier) error {
	if v == nil {
		return ErrResource
	}
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	m.guard.verifier.Store(v)
	return nil
}

// RevocationFloor is stored outside the runtime directory to detect a whole-file
// replay which predates accepted denials, independently of activation versions.
type RevocationFloor struct {
	ArtifactRevision  uint64   `json:"artifactRevision,string,omitzero"`
	ArtifactHash      [32]byte `json:"artifactHash,omitzero"`
	ControlPlaneEpoch uint64   `json:"controlPlaneEpoch,string"`
	Epoch             uint64   `json:"epoch,string"`
	Digest            [32]byte `json:"digest"`
}

func (f RevocationFloor) valid() bool {
	return f == (RevocationFloor{}) || f.Epoch > 0 && f.Digest != [32]byte{} && ((f.ArtifactRevision == 0) == (f.ArtifactHash == [32]byte{})) && (f.ArtifactRevision == 0 || f.ControlPlaneEpoch != 0)
}
func revocationAbove(r runtimeRecord, f RevocationFloor) bool {
	if !f.valid() {
		return false
	}
	if f == (RevocationFloor{}) {
		return true
	}
	return r.RevocationArtifactRevision >= f.ArtifactRevision && (f.ArtifactRevision == 0 || r.RevocationArtifactRevision != f.ArtifactRevision || r.RevocationArtifactHash == f.ArtifactHash) && (f.ControlPlaneEpoch == 0 || f.ControlPlaneEpoch == r.RevocationControlPlaneEpoch) && r.RevocationEpoch >= f.Epoch && (r.RevocationEpoch != f.Epoch || r.RevokeDigest == f.Digest)
}
func (m *RuntimeManager) RevocationFloor(ctx context.Context) (RevocationFloor, error) {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return RevocationFloor{}, err
	}
	defer done()
	if err := m.lock(ctx); err != nil {
		return RevocationFloor{}, err
	}
	defer m.unlock()
	r, err := m.journal.read(ctx)
	if err != nil {
		m.poison(err)
		return RevocationFloor{}, err
	}
	return RevocationFloor{ArtifactRevision: r.RevocationArtifactRevision, ArtifactHash: r.RevocationArtifactHash, ControlPlaneEpoch: r.RevocationControlPlaneEpoch, Epoch: r.RevocationEpoch, Digest: r.RevokeDigest}, nil
}
