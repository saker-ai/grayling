package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
)

// Revoke persists a monotonically versioned bundle deny set before publishing
// the live guard. Its caller must authenticate the control-plane command and
// scope first. This never renews policy freshness or clears previous revocations.
func (c *PolicyCache) Revoke(ctx context.Context, epoch uint64, bundles [][32]byte) error {
	if epoch == 0 || len(bundles) == 0 || len(bundles) > maxRevokedBundles {
		return ErrResource
	}
	requested := make(map[[32]byte]bool, len(bundles))
	for _, hash := range bundles {
		if hash == [32]byte{} {
			return ErrResource
		}
		requested[hash] = true
	}
	hashes := sortedHashes(requested)
	input := binary.BigEndian.AppendUint64(nil, epoch)
	for _, hash := range hashes {
		input = append(input, hash[:]...)
	}
	digest := sha256.Sum256(input)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guard.closed.Load() {
		return ErrCacheClosed
	}
	if fail := c.guard.failure.Load(); fail != nil {
		return fail.err
	}
	now, err := c.guard.time()
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := c.read(ctx, tx)
	if err != nil {
		c.poison(err)
		return err
	}
	if epoch < r.revocationEpoch || epoch == r.revocationEpoch && digest != r.revokeDigest {
		return ErrRollback
	}
	if epoch == r.revocationEpoch {
		return tx.Commit()
	}
	all := revokedSet(r.revoked)
	for hash := range requested {
		all[hash] = true
	}
	if len(all) > maxRevokedBundles {
		return ErrResource
	}
	r.revocationEpoch = epoch
	r.revokeDigest = digest
	r.revoked = sortedHashes(all)
	r.clockHighMs = max(r.clockHighMs, now.UnixMilli())
	if err := c.write(ctx, tx, r); err != nil {
		return err
	}
	c.guard.revoked.Store(&all)
	return nil
}
