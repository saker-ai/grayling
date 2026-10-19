package agent_test

import (
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

func discoveryCacheFixture(t *testing.T) (*cacheFixture, client.DiscoveryCacheOptions) {
	t.Helper()
	f := newCacheFixture(t)
	base := f.opts
	base.Key = ""
	base.MaxAge = 10 * time.Second
	return f, client.DiscoveryCacheOptions{CacheOptions: base, BundleHash: sha256.Sum256([]byte("discovery-bundle"))}
}
func cachedDiscoveryResource(t *testing.T, f *cacheFixture, o client.DiscoveryCacheOptions, epoch, artifact, membership uint64, change bool) *agentv1.SignedResource {
	t.Helper()
	now := f.opts.Now()
	document := client.DiscoveryDocument{SchemaVersion: 1, Purpose: "registered-candidates", Scope: o.Scope, MembershipRevision: membership, Members: []client.DiscoveryMember{}}
	if change {
		document.Members = []client.DiscoveryMember{{InstanceID: "instance", BootID: "boot", BuildID: "build", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Address: "10.20.0.10", Port: 8443, TLSServerName: "service.internal", Zone: "az", ProtocolVersion: 1, Ready: true, Lease: client.DiscoveryLease{ID: "lease", Epoch: 1, ExpiresUnixMs: now.Add(time.Minute).UnixMilli()}}}
	}
	body, err := client.MarshalDiscovery(document)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := control.SignResource(client.Claims{Scope: o.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(o.BundleHash), ControlPlaneEpoch: epoch, Revision: artifact, BundleHash: o.BundleHash, IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "key"}, body, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}
func TestDiscoveryCacheRestartFloorsAndFreshness(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	if _, err := client.OpenDiscoveryCache(t.Context(), o); !errors.Is(err, client.ErrCacheMissing) {
		t.Fatal("missing cache initialized silently", err)
	}
	c, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Capture(); !errors.Is(err, client.ErrCacheNotReady) {
		t.Fatal("empty cache ready", err)
	}
	first := cachedDiscoveryResource(t, f, o, 1, 1, 10, false)
	if err := c.Install(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	view, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	mark, err := c.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []*agentv1.SignedResource{cachedDiscoveryResource(t, f, o, 1, 2, 9, false), cachedDiscoveryResource(t, f, o, 1, 2, 10, true), cachedDiscoveryResource(t, f, o, 2, 2, 1, false)} {
		if err := c.Install(t.Context(), candidate); !errors.Is(err, client.ErrRollback) {
			t.Fatal("membership rollback/conflict or unauthorized epoch accepted", err)
		}
	}
	f.clock.Add(9 * time.Second.Milliseconds())
	if err := c.Install(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(2 * time.Second.Milliseconds())
	if _, err := view.Candidates(); !errors.Is(err, client.ErrDiscoveryStale) {
		t.Fatal("same-artifact replay refreshed discovery", err)
	}
	refreshed := cachedDiscoveryResource(t, f, o, 1, 2, 10, false)
	if err := c.Install(t.Context(), refreshed); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MembershipRevision() != 10 {
		t.Fatal("artifact refresh changed member revision")
	}
	if _, err := view.Candidates(); !errors.Is(err, client.ErrDiscoveryStale) {
		t.Fatal("old captured view renewed by later artifact", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	o.Minimum = mark
	o.MinimumMembershipRevision = 10
	reopened, err := client.OpenDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Capture()
	if err != nil || restored.Claims().Revision != 2 || restored.MembershipRevision() != 10 {
		t.Fatal("restart lost atomic artifact/member baseline", err)
	}
	if err := reopened.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 3, 9, false)); !errors.Is(err, client.ErrRollback) {
		t.Fatal("restart lost membership floor", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	o.MinimumMembershipRevision = 11
	if _, err := client.OpenDiscoveryCache(t.Context(), o); !errors.Is(err, client.ErrRollback) {
		t.Fatal("external member floor ignored", err)
	}
}
func TestDiscoveryCacheBindingLocksTrustAndRevocation(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	c, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := client.OpenDiscoveryCache(t.Context(), o); !errors.Is(err, client.ErrCacheBusy) {
		t.Fatal("parallel cache ownership accepted", err)
	}
	resource := cachedDiscoveryResource(t, f, o, 1, 1, 1, true)
	if err := c.Install(t.Context(), resource); err != nil {
		t.Fatal(err)
	}
	view, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	members, err := view.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	members[0].Address = "127.0.0.1"
	again, err := view.Candidates()
	if err != nil || again[0].Address != "10.20.0.10" {
		t.Fatal("captured cache candidate mutated", err)
	}
	if err := c.Revoke(t.Context(), 1, [][32]byte{o.BundleHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Candidates(); !errors.Is(err, client.ErrPolicyRevoked) {
		t.Fatal("revocation did not cover captured discovery", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Candidates(); !errors.Is(err, client.ErrCacheClosed) {
		t.Fatal("closed guard ignored", err)
	}
	wrong := o
	wrong.BundleHash = sha256.Sum256([]byte("other"))
	if _, err := client.OpenDiscoveryCache(t.Context(), wrong); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("bundle binding ignored", err)
	}
	// Copy the sealed discovery database into a policy slot. The domain binding
	// must reject it even when identity/scope/key and authentication key match.
	raw, err := os.ReadFile(filepath.Join(o.Directory, "discovery-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Directory, "policy-cache.sqlite"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	policyOpts := o.CacheOptions
	policyOpts.Key = client.DiscoveryKey(o.BundleHash)
	if _, err := client.OpenPolicyCache(t.Context(), policyOpts); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("cross-kind sealed cache accepted", err)
	}
}

func TestDiscoveryCacheIgnoredWriteNeverPublishesCandidate(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	c, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 1, 1, false)); err != nil {
		t.Fatal(err)
	}
	captured, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(o.Directory, "discovery-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER ignore_write BEFORE UPDATE ON agent_policy_cache BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 2, 2, true)); !errors.Is(err, client.ErrCacheIO) {
		t.Fatal("ignored write accepted as durable", err)
	}
	if _, err := captured.Candidates(); !errors.Is(err, client.ErrCacheIO) {
		t.Fatal("persistence failure did not guard captured views", err)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER ignore_write`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil && !errors.Is(err, client.ErrCacheIO) {
		t.Fatal(err)
	}
	reopened, err := client.OpenDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	old, err := reopened.Capture()
	if err != nil || old.Claims().Revision != 1 || old.MembershipRevision() != 1 {
		t.Fatal("failed candidate reached durable state", err)
	}
}

func TestDiscoveryCacheRecoveryCannotClearMemberFloorWithoutGrant(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	recoveryKey := recoveryRoots(t, f)
	o.RecoveryVerifier = f.opts.RecoveryVerifier
	c, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 100, 100, false)); err != nil {
		t.Fatal(err)
	}
	from, err := c.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	o.Minimum = from
	o.MinimumMembershipRevision = 100
	c, err = client.OpenDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	target := cachedDiscoveryResource(t, f, o, 2, 1, 1, false)
	if err := c.Install(t.Context(), target); !errors.Is(err, client.ErrRollback) {
		t.Fatal("new epoch bypassed member floor", err)
	}
	grant := recoveryGrant(t, f, from, target, recoveryKey)
	forged := recoveryGrant(t, f, from, target, f.private)
	if err := c.InstallRecovery(t.Context(), target, forged); err == nil {
		t.Fatal("resource key authorized discovery recovery")
	}
	if err := c.InstallRecovery(t.Context(), target, grant); err != nil {
		t.Fatal(err)
	}
	recovered, err := c.Capture()
	if err != nil || recovered.Claims().ControlPlaneEpoch != 2 || recovered.MembershipRevision() != 1 {
		t.Fatal("authorized exact recovery failed", err)
	}
	f.clock.Add(9 * time.Second.Milliseconds())
	if err := c.InstallRecovery(t.Context(), target, grant); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(2 * time.Second.Milliseconds())
	if _, err := recovered.Candidates(); !errors.Is(err, client.ErrDiscoveryStale) {
		t.Fatal("replayed recovery renewed discovery", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := client.OpenDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal("higher authorized epoch could not reopen", err)
	}
	defer reopened.Close()
	mark, err := reopened.Watermark(t.Context())
	if err != nil || mark.ControlPlaneEpoch != 2 || mark.Revision != 1 {
		t.Fatal("recovery lost durable epoch", err)
	}

}

func TestDiscoveryCacheTrustRotationAndClockGuard(t *testing.T) {
	f, o := discoveryCacheFixture(t)
	c, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Install(t.Context(), cachedDiscoveryResource(t, f, o, 1, 1, 1, false)); err != nil {
		t.Fatal(err)
	}
	old, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	now := f.opts.Now()
	root := client.TrustKey{PublicKey: f.private.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	overlap, err := client.NewVerifier(map[string]client.TrustKey{"key": root, "next": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), overlap); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Candidates(); err != nil {
		t.Fatal("overlap broke captured view", err)
	}
	replacement, err := client.NewVerifier(map[string]client.TrustKey{"next": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Candidates(); !errors.Is(err, client.ErrTrustChanged) {
		t.Fatal("retired signer remained usable", err)
	}
	target := cachedDiscoveryResource(t, f, o, 1, 2, 1, false)
	claims, err := client.ParseClaims(target.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.KeyID = "next"
	target, err = control.SignResource(claims, target.Payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	current, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Add(-6 * time.Second.Milliseconds())
	if _, err := current.Candidates(); !errors.Is(err, client.ErrClockSkew) {
		t.Fatal("clock rollback allowed discovery use", err)
	}
}

func TestDiscoveryCacheEmptyStateHasIndependentSealDomain(t *testing.T) {
	_, o := discoveryCacheFixture(t)
	discovery, err := client.InitializeDiscoveryCache(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := discovery.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(o.Directory, "discovery-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Directory, "policy-cache.sqlite"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sameBinding := o.CacheOptions
	sameBinding.Key = client.DiscoveryKey(o.BundleHash)
	if _, err := client.OpenPolicyCache(t.Context(), sameBinding); !errors.Is(err, client.ErrCacheCorrupt) {
		t.Fatal("empty cross-kind database accepted without a resource-kind field to distinguish it", err)
	}
}
