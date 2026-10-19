package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
)

func bundleCacheFixture(t *testing.T) (*cacheFixture, client.BundleCacheOptions, client.BundleDocument, []byte) {
	t.Helper()
	f, d, payload := bundleFixture(t)
	body, err := client.MarshalBundle(d)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts
	base.Directory = filepath.Join(t.TempDir(), "bundle")
	base.Key = ""
	base.MaxAge = 10 * time.Second
	return f, client.BundleCacheOptions{CacheOptions: base, BundleHash: sha256.Sum256(body)}, d, payload
}
func cachedBundleResource(t *testing.T, f *cacheFixture, d client.BundleDocument, revision uint64, keyID string, key ed25519.PrivateKey) *agentv1.SignedResource {
	t.Helper()
	body, err := client.MarshalBundle(d)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	now := f.opts.Now()
	r, err := control.SignResource(client.Claims{Scope: d.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_BUNDLE, Key: client.BundleKey(hash), ControlPlaneEpoch: d.Policy.ControlPlaneEpoch, Revision: revision, BundleHash: hash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: keyID}, body, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestBundleCacheRestartFloorsAndArtifactFreshness(t *testing.T) {
	f, o, d, _ := bundleCacheFixture(t)
	if _, err := client.OpenBundleCache(t.Context(), o); !errors.Is(err, client.ErrCacheMissing) {
		t.Fatal("missing cache silently initialized", err)
	}
	c, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Capture(); !errors.Is(err, client.ErrCacheNotReady) {
		t.Fatal("empty cache ready", err)
	}
	first := cachedBundleResource(t, f, d, 1, "key", f.private)
	if err := c.Install(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	captured, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := captured.Document()
	if err != nil {
		t.Fatal(err)
	}
	doc.ValueBuilds["backend"] = "attacker"
	doc.Builds[0].Resources[0].SHA256 = "attacker"
	again, err := captured.Document()
	if err != nil || again.ValueBuilds["backend"] != "stable" || again.Builds[0].Resources[0].SHA256 == "attacker" {
		t.Fatal("mutable Bundle view alias", err)
	}
	f.clock.Add(9 * time.Second.Milliseconds())
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 1, "key", f.private)); !errors.Is(err, client.ErrRollback) {
		t.Fatal("same artifact identity accepted altered signed expiry", err)
	}
	if err := c.Install(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(2 * time.Second.Milliseconds())
	if _, err := captured.Document(); !errors.Is(err, client.ErrBundleStale) {
		t.Fatal("replay renewed Bundle", err)
	}
	if _, err := c.Capture(); !errors.Is(err, client.ErrBundleStale) {
		t.Fatal("cache replay renewed freshness", err)
	}
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 2, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.Capture()
	if err != nil || fresh.Claims().Revision != 2 {
		t.Fatal("fresh artifact unavailable", err)
	}
	if _, err := captured.Document(); !errors.Is(err, client.ErrBundleStale) {
		t.Fatal("later install renewed captured view", err)
	}
	mark, err := c.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	o.Minimum = mark
	reopened, err := client.OpenBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Capture()
	if err != nil || restored.Claims().Revision != 2 {
		t.Fatal("restart lost Bundle watermark", err)
	}
	if err := reopened.Install(t.Context(), first); !errors.Is(err, client.ErrRollback) {
		t.Fatal("old artifact accepted after restart", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	o.Minimum.Revision++
	if _, err := client.OpenBundleCache(t.Context(), o); !errors.Is(err, client.ErrRollback) {
		t.Fatal("external floor bypassed", err)
	}
}
func TestBundleCachePolicyJoinAndLiveGuards(t *testing.T) {
	f, o, d, payload := bundleCacheFixture(t)
	c, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 1, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	view, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	policies := initCache(t, f)
	now := f.opts.Now()
	pc := client.Claims{Scope: d.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: d.Policy.Key, ControlPlaneEpoch: d.Policy.ControlPlaneEpoch, Revision: d.Policy.Revision, BundleHash: o.BundleHash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "key"}
	signed, err := control.SignResource(pc, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err := policies.Install(t.Context(), signed); err != nil {
		t.Fatal(err)
	}
	policy, err := policies.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ValidatePolicy(policy); err != nil {
		t.Fatal("durable dependency join rejected", err)
	}
	if err := view.ValidatePolicy(nil); !errors.Is(err, client.ErrCacheNotReady) {
		t.Fatal("missing policy accepted", err)
	}
	foreignOptions := f.opts
	foreignOptions.Directory = filepath.Join(t.TempDir(), "foreign-policy")
	foreign, err := client.InitializePolicyCache(t.Context(), foreignOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	pc.BundleHash = sha256.Sum256([]byte("foreign"))
	wrong, err := control.SignResource(pc, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Install(t.Context(), wrong); err != nil {
		t.Fatal(err)
	}
	foreignView, err := foreign.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ValidatePolicy(foreignView); !errors.Is(err, client.ErrResource) {
		t.Fatal("foreign Bundle policy joined", err)
	}
	if err := policies.Revoke(t.Context(), 1, [][32]byte{o.BundleHash}); err != nil {
		t.Fatal(err)
	}
	if err := view.ValidatePolicy(policy); !errors.Is(err, client.ErrPolicyRevoked) {
		t.Fatal("revoked policy remained joinable", err)
	}
	if err := c.Revoke(t.Context(), 1, [][32]byte{o.BundleHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Document(); !errors.Is(err, client.ErrPolicyRevoked) {
		t.Fatal("captured Bundle ignored revocation", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := client.OpenBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Capture(); !errors.Is(err, client.ErrPolicyRevoked) {
		t.Fatal("restart lost revocation", err)
	}
	if _, err := view.Document(); !errors.Is(err, client.ErrCacheClosed) {
		t.Fatal("closed Bundle view usable", err)
	}
}
func TestBundleCacheIndependentSealAndTargetBinding(t *testing.T) {
	f, o, d, _ := bundleCacheFixture(t)
	c, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenBundleCache(t.Context(), o); !errors.Is(err, client.ErrCacheBusy) {
		t.Fatal("multiple owners admitted", err)
	}
	// An empty record has no signed kind to distinguish it; the MAC domain must.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(o.Directory, "bundle-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"policy-cache.sqlite", "discovery-cache.sqlite"} {
		if err := os.WriteFile(filepath.Join(o.Directory, filename), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policyOptions := o.CacheOptions
	policyOptions.Key = client.BundleKey(o.BundleHash)
	if _, err := client.OpenPolicyCache(t.Context(), policyOptions); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("Bundle seal accepted as policy", err)
	}
	if _, err := client.OpenDiscoveryCache(t.Context(), client.DiscoveryCacheOptions{CacheOptions: policyOptions, BundleHash: o.BundleHash}); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("Bundle seal accepted as discovery", err)
	}
	c, err = client.OpenBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 1, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	changed := d
	changed.Policy.ControlPlaneEpoch++
	if err := c.Install(t.Context(), cachedBundleResource(t, f, changed, 2, "key", f.private)); err == nil {
		t.Fatal("epoch transition reused fixed Bundle cache")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	other := o
	other.BundleHash = sha256.Sum256([]byte("other"))
	if _, err := client.OpenBundleCache(t.Context(), other); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("wrong Bundle binding accepted", err)
	}
}
func TestBundleCacheTrustRotationAndClockRollback(t *testing.T) {
	f, o, d, _ := bundleCacheFixture(t)
	c, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 1, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	old, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	rotated := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	now := f.opts.Now()
	roots := map[string]client.TrustKey{"key": {PublicKey: f.private.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, "rotated": {PublicKey: rotated.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}
	overlap, err := client.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), overlap); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Document(); err != nil {
		t.Fatal("overlap retired root prematurely", err)
	}
	delete(roots, "key")
	retired, err := client.NewVerifier(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), retired); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Document(); !errors.Is(err, client.ErrTrustChanged) {
		t.Fatal("retired root allowed captured Bundle", err)
	}
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 2, "rotated", rotated)); err != nil {
		t.Fatal(err)
	}
	current, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Add(-6 * time.Second.Milliseconds())
	if _, err := current.Document(); !errors.Is(err, client.ErrClockSkew) {
		t.Fatal("clock rollback allowed Bundle", err)
	}
}
func TestBundleCacheSuppressedWriteCannotPublishCandidate(t *testing.T) {
	f, o, d, _ := bundleCacheFixture(t)
	c, err := client.InitializeBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 1, "key", f.private)); err != nil {
		t.Fatal(err)
	}
	old, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(o.Directory, "bundle-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER ignore_bundle BEFORE UPDATE ON agent_policy_cache BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), cachedBundleResource(t, f, d, 2, "key", f.private)); !errors.Is(err, client.ErrCacheIO) {
		t.Fatal("nondurable Bundle candidate accepted", err)
	}
	if _, err := old.Document(); !errors.Is(err, client.ErrCacheIO) {
		t.Fatal("failed write did not guard old view", err)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER ignore_bundle`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil && !errors.Is(err, client.ErrCacheIO) {
		t.Fatal(err)
	}
	reopened, err := client.OpenBundleCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Capture()
	if err != nil || restored.Claims().Revision != 1 {
		t.Fatal("suppressed write changed durable watermark", err)
	}
}
