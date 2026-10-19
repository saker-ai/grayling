package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
	"google.golang.org/protobuf/proto"
)

func recoveryRoots(t *testing.T, f *cacheFixture) ed25519.PrivateKey {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{27}, 32))
	roots, err := agent.NewRecoveryVerifier(map[string]agent.TrustKey{"recovery": {PublicKey: private.Public().(ed25519.PublicKey), NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.RecoveryVerifier = roots
	return private
}
func recoveryTarget(t *testing.T, f *cacheFixture, epoch, revision uint64, bundle string) *agentv1.SignedResource {
	t.Helper()
	resource := f.resource(t, revision, bundle)
	claims, err := agent.ParseClaims(resource.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.ControlPlaneEpoch = epoch
	resource, err = control.SignResource(claims, resource.Payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}
func recoveryGrant(t *testing.T, f *cacheFixture, from agent.Watermark, target *agentv1.SignedResource, private ed25519.PrivateKey) *agent.RecoveryGrant {
	t.Helper()
	claims, err := agent.ParseClaims(target.Claims)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := control.SignRecovery(agent.RecoveryClaims{From: from, Target: claims, Nonce: sha256.Sum256([]byte("test-nonce")), KeyID: "recovery", IssuedUnixMs: f.opts.Now().Add(-time.Second).UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(5 * time.Minute).UnixMilli()}, private)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}
func TestRecoveryRequiresIndependentAuthorityAndExactTransition(t *testing.T) {
	f := newCacheFixture(t)
	private := recoveryRoots(t, f)
	cache := initCache(t, f)
	if err := cache.Install(t.Context(), f.resource(t, 100, "before")); err != nil {
		t.Fatal(err)
	}
	from, err := cache.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := recoveryTarget(t, f, 2, 1, "restored")
	requireCacheError(t, cache.Install(t.Context(), target), agent.ErrRollback)
	grant := recoveryGrant(t, f, from, target, private)
	forged := &agent.RecoveryGrant{Claims: bytes.Clone(grant.Claims), Signature: bytes.Clone(grant.Signature)}
	forged.Signature[0] ^= 1
	if err := cache.InstallRecovery(t.Context(), target, forged); err == nil {
		t.Fatal("accepted forged recovery")
	}
	policySigned := recoveryGrant(t, f, from, target, f.private)
	if err := cache.InstallRecovery(t.Context(), target, policySigned); err == nil {
		t.Fatal("policy key authorized recovery")
	}
	swapped := recoveryTarget(t, f, 2, 2, "different")
	requireCacheError(t, cache.InstallRecovery(t.Context(), swapped, grant), agent.ErrResource)
	if err := cache.InstallRecovery(t.Context(), target, grant); err != nil {
		t.Fatal(err)
	}
	mark, err := cache.Watermark(t.Context())
	if err != nil || mark.ControlPlaneEpoch != 2 || mark.Revision != 1 {
		t.Fatalf("recovery watermark: %+v %v", mark, err)
	}
	requireCacheError(t, cache.Install(t.Context(), f.resource(t, 101, "old-plane")), agent.ErrRollback)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = agent.OpenPolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	f.clock.Add(int64(61 * time.Second / time.Millisecond))
	// Idempotent replay preserves the original verification time across restart.
	if err := cache.InstallRecovery(t.Context(), target, grant); err != nil {
		t.Fatal(err)
	}
	_, err = cache.Capture()
	requireCacheError(t, err, agent.ErrPolicyStale)
	malformed := proto.Clone(target).(*agentv1.SignedResource)
	malformed.Payload[0] ^= 1
	requireCacheError(t, cache.InstallRecovery(t.Context(), malformed, grant), agent.ErrResource)
}
func TestRecoveryRejectsSharedRoots(t *testing.T) {
	f := newCacheFixture(t)
	roots, err := agent.NewRecoveryVerifier(map[string]agent.TrustKey{"alias": {PublicKey: f.private.Public().(ed25519.PublicKey), NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.RecoveryVerifier = roots
	_, err = agent.InitializePolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
}
func TestRecoveryRetainsRevocationsAndRejectsWrongSavedWatermark(t *testing.T) {
	f := newCacheFixture(t)
	private := recoveryRoots(t, f)
	cache := initCache(t, f)
	if err := cache.Install(t.Context(), f.resource(t, 10, "before")); err != nil {
		t.Fatal(err)
	}
	from, _ := cache.Watermark(t.Context())
	target := recoveryTarget(t, f, 2, 1, "denied")
	grant := recoveryGrant(t, f, from, target, private)
	denied := sha256.Sum256([]byte("denied"))
	if err := cache.Revoke(t.Context(), 1, [][32]byte{denied}); err != nil {
		t.Fatal(err)
	}
	requireCacheError(t, cache.InstallRecovery(t.Context(), target, grant), agent.ErrPolicyRevoked)
	if err := cache.Install(t.Context(), f.resource(t, 11, "advanced")); err != nil {
		t.Fatal(err)
	}
	target = recoveryTarget(t, f, 2, 1, "allowed")
	grant = recoveryGrant(t, f, from, target, private)
	requireCacheError(t, cache.InstallRecovery(t.Context(), target, grant), agent.ErrRollback)
	current, _ := cache.Watermark(t.Context())
	if current.Revision != 11 || current.ControlPlaneEpoch != 1 {
		t.Fatal("rejected recovery changed saved watermark")
	}
}
func TestRecoveryEncodingStrict(t *testing.T) {
	f := newCacheFixture(t)
	private := recoveryRoots(t, f)
	first, err := agent.ParseClaims(f.resource(t, 10, "old").Claims)
	if err != nil {
		t.Fatal(err)
	}
	grant := recoveryGrant(t, f, first.Watermark(), recoveryTarget(t, f, 2, 1, "new"), private)
	parsed, err := agent.ParseRecoveryClaims(grant.Claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := agent.MarshalRecoveryClaims(parsed)
	if err != nil || !bytes.Equal(encoded, grant.Claims) {
		t.Fatal("noncanonical recovery round trip")
	}
	for i := range len(encoded) {
		if _, err := agent.ParseRecoveryClaims(encoded[:i]); err == nil {
			t.Fatalf("accepted truncated grant %d", i)
		}
	}
	if _, err := agent.ParseRecoveryClaims(append(bytes.Clone(encoded), 0)); err == nil {
		t.Fatal("accepted trailing grant fields")
	}
	for _, epoch := range []uint64{0, 1} {
		parsed.Target.ControlPlaneEpoch = epoch
		if _, err := agent.MarshalRecoveryClaims(parsed); !errors.Is(err, agent.ErrResource) {
			t.Fatal("accepted non-increasing recovery epoch")
		}
	}
}
func FuzzRecoveryClaims(f *testing.F) {
	body, err := os.ReadFile("../../test/agent/fixtures/recovery-v1.json")
	if err != nil {
		f.Fatal(err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(body, &fixture); err != nil {
		f.Fatal(err)
	}
	seed, err := hex.DecodeString(fixture["recoveryHex"])
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("GRA1"))
	f.Add(bytes.Repeat([]byte{0}, 200))
	f.Fuzz(func(t *testing.T, b []byte) {
		claims, err := agent.ParseRecoveryClaims(b)
		if err != nil {
			return
		}
		encoded, err := agent.MarshalRecoveryClaims(claims)
		if err != nil || !bytes.Equal(encoded, b) {
			t.Fatal("noncanonical grant accepted")
		}
	})
}

func TestRecoveryExpiredGrantCannotAdvanceEpoch(t *testing.T) {
	f := newCacheFixture(t)
	private := recoveryRoots(t, f)
	cache := initCache(t, f)
	if err := cache.Install(t.Context(), f.resource(t, 10, "before")); err != nil {
		t.Fatal(err)
	}
	from, _ := cache.Watermark(t.Context())
	target := recoveryTarget(t, f, 2, 1, "restored")
	grant := recoveryGrant(t, f, from, target, private)
	f.clock.Add(int64(5 * time.Minute / time.Millisecond))
	requireCacheError(t, cache.InstallRecovery(t.Context(), target, grant), agent.ErrExpired)
	current, err := cache.Watermark(t.Context())
	if err != nil || current != from {
		t.Fatalf("expired grant changed watermark: %+v %v", current, err)
	}
	overlapping, err := agent.NewVerifier(map[string]agent.TrustKey{"recovery-alias": {PublicKey: private.Public().(ed25519.PublicKey), NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	requireCacheError(t, cache.SetVerifier(t.Context(), overlapping), agent.ErrResource)
}

func TestRecoveryZeroVerifierFailsClosed(t *testing.T) {
	f := newCacheFixture(t)
	f.opts.RecoveryVerifier = &agent.RecoveryVerifier{}
	_, err := agent.InitializePolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
}

func TestRecoveryFailedWriteKeepsOriginalEpoch(t *testing.T) {
	f := newCacheFixture(t)
	private := recoveryRoots(t, f)
	cache := initCache(t, f)
	if err := cache.Install(t.Context(), f.resource(t, 10, "before")); err != nil {
		t.Fatal(err)
	}
	from, _ := cache.Watermark(t.Context())
	target := recoveryTarget(t, f, 2, 1, "restored")
	grant := recoveryGrant(t, f, from, target, private)
	db, err := sql.Open("sqlite", filepath.Join(f.opts.Directory, "policy-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_recovery BEFORE UPDATE ON agent_policy_cache BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := cache.InstallRecovery(t.Context(), target, grant); err == nil {
		t.Fatal("ignored persistence failure")
	}
	current, err := cache.Watermark(t.Context())
	if err != nil || current != from {
		t.Fatalf("failed recovery changed durable epoch: %+v %v", current, err)
	}
	view, err := cache.Capture()
	if err != nil || view.Claims().Watermark() != from {
		t.Fatalf("failed recovery changed active view: %v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_recovery`); err != nil {
		t.Fatal(err)
	}
	if err := cache.InstallRecovery(t.Context(), target, grant); err != nil {
		t.Fatal(err)
	}
}
