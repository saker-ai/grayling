package agent_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

type cacheFixture struct {
	opts    agent.CacheOptions
	clock   atomic.Int64
	private ed25519.PrivateKey
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	f := &cacheFixture{private: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))}
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	f.clock.Store(now.UnixMilli())
	verifier, err := agent.NewVerifier(map[string]agent.TrustKey{"key": {PublicKey: f.private.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	f.opts = agent.CacheOptions{Directory: filepath.Join(t.TempDir(), "cache"), Identity: "instance", Scope: agent.Scope{WorkspaceID: "w", EnvironmentID: "e", ServiceKey: "s"}, Key: "policy", AuthenticationKey: bytes.Repeat([]byte{9}, 32), Verifier: verifier, MaxAge: time.Minute, Now: func() time.Time { return time.UnixMilli(f.clock.Load()) }}
	return f
}
func (f *cacheFixture) resource(t *testing.T, revision uint64, bundle string) *agentv1.SignedResource {
	t.Helper()
	snapshot := model.Snapshot{SchemaVersion: 1, Workspace: "w", Environment: "e", ManifestRevision: 1, Flags: map[string]*model.Flag{"route": {SchemaVersion: 1, Key: "route", Type: "string", Variations: map[string]any{"stable": "backend"}, DefaultRule: &model.Rule{Variation: "stable"}}}}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	now := f.opts.Now()
	r, err := control.SignResource(agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: f.opts.Key, ControlPlaneEpoch: 1, Revision: revision, BundleHash: sha256.Sum256([]byte(bundle)), IssuedUnixMs: now.Add(-time.Second).UnixMilli(), ExpiresUnixMs: now.Add(10 * time.Minute).UnixMilli(), KeyID: "key"}, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func initCache(t *testing.T, f *cacheFixture) *agent.PolicyCache {
	t.Helper()
	c, err := agent.InitializePolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func requireCacheError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
func TestPolicyCacheRestartAndFreshness(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	_, err := c.Capture()
	requireCacheError(t, err, agent.ErrCacheNotReady)
	r := f.resource(t, 2, "bundle")
	if err := c.Install(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	view, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	result, err := view.Evaluate("route", model.EvaluationContext{})
	if err != nil || result.Value != "backend" {
		t.Fatalf("evaluation: %+v %v", result, err)
	}
	mark, err := c.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(61 * time.Second / time.Millisecond))
	c, err = agent.OpenPolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Capture()
	requireCacheError(t, err, agent.ErrPolicyStale)
	restored, err := c.Watermark(t.Context())
	if err != nil || restored != mark {
		t.Fatalf("lost watermark: %+v %v", restored, err)
	}
	requireCacheError(t, c.Install(t.Context(), f.resource(t, 1, "older")), agent.ErrRollback)
	if err := c.Install(t.Context(), f.resource(t, 3, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capture(); err != nil {
		t.Fatal(err)
	}
}
func TestPolicyCacheRejectedCandidateAndAtomicWrite(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	if err := c.Install(t.Context(), f.resource(t, 2, "good")); err != nil {
		t.Fatal(err)
	}
	mark, _ := c.Watermark(t.Context())
	bad := f.resource(t, 3, "bad")
	bad.Signature[0] ^= 1
	if err := c.Install(t.Context(), bad); err == nil {
		t.Fatal("accepted bad signature")
	}
	db, err := sql.Open("sqlite", filepath.Join(f.opts.Directory, "policy-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_write BEFORE UPDATE ON agent_policy_cache BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), f.resource(t, 4, "candidate")); err == nil {
		t.Fatal("write failure ignored")
	}
	current, err := c.Watermark(t.Context())
	if err != nil || current != mark {
		t.Fatalf("failed write advanced mark: %+v %v", current, err)
	}
	if _, err := c.Capture(); err != nil {
		t.Fatalf("failed candidate destroyed good policy: %v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_write`); err != nil {
		t.Fatal(err)
	}
}
func TestPolicyCacheRevocationPersistsAndCoversCapturedView(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	if err := c.Install(t.Context(), f.resource(t, 1, "denied")); err != nil {
		t.Fatal(err)
	}
	view, _ := c.Capture()
	hash := sha256.Sum256([]byte("denied"))
	if err := c.Revoke(t.Context(), 1, [][32]byte{hash}); err != nil {
		t.Fatal(err)
	}
	_, err := view.Evaluate("route", model.EvaluationContext{})
	requireCacheError(t, err, agent.ErrPolicyRevoked)
	if err := c.Revoke(t.Context(), 1, [][32]byte{hash}); err != nil {
		t.Fatal(err)
	}
	requireCacheError(t, c.Revoke(t.Context(), 1, [][32]byte{sha256.Sum256([]byte("different"))}), agent.ErrRollback)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = agent.OpenPolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	requireCacheError(t, c.Install(t.Context(), f.resource(t, 2, "denied")), agent.ErrPolicyRevoked)
	if err := c.Install(t.Context(), f.resource(t, 3, "allowed")); err != nil {
		t.Fatal(err)
	}
}
func TestPolicyCacheIdentityOwnershipAndTampering(t *testing.T) {
	f := newCacheFixture(t)
	_, err := agent.OpenPolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheMissing)
	c := initCache(t, f)
	_, err = agent.OpenPolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheBusy)
	if err := c.Install(t.Context(), f.resource(t, 1, "bundle")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	other := f.opts
	other.Identity = "different"
	_, err = agent.OpenPolicyCache(t.Context(), other)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
	db, err := sql.Open("sqlite", filepath.Join(f.opts.Directory, "policy-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_policy_cache SET seal=zeroblob(32)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = agent.OpenPolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
	if _, err := agent.InitializePolicyCache(t.Context(), f.opts); err == nil {
		t.Fatal("initialization replaced corrupted state")
	}
}

func TestPolicyCacheTrustRotationAndClockRollback(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	if err := c.Install(t.Context(), f.resource(t, 1, "bundle")); err != nil {
		t.Fatal(err)
	}
	view, _ := c.Capture()
	pub := f.private.Public().(ed25519.PublicKey)
	root := agent.TrustKey{PublicKey: pub, NotBefore: f.opts.Now().Add(-time.Hour), NotAfter: f.opts.Now().Add(time.Hour)}
	overlap, err := agent.NewVerifier(map[string]agent.TrustKey{"key": root, "next": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), overlap); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Evaluate("route", model.EvaluationContext{}); err != nil {
		t.Fatalf("overlap invalidated view: %v", err)
	}
	retired, err := agent.NewVerifier(map[string]agent.TrustKey{"next": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetVerifier(t.Context(), retired); err != nil {
		t.Fatal(err)
	}
	_, err = view.Evaluate("route", model.EvaluationContext{})
	requireCacheError(t, err, agent.ErrTrustChanged)
	if _, err := c.Capture(); err == nil {
		t.Fatal("retired signer remained ready")
	}
	if err := c.SetVerifier(t.Context(), f.opts.Verifier); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(61 * time.Second / time.Millisecond))
	_, err = c.Capture()
	requireCacheError(t, err, agent.ErrPolicyStale)
	f.clock.Add(-2000)
	_, err = c.Capture()
	requireCacheError(t, err, agent.ErrPolicyStale)
	f.clock.Add(-6000)
	_, err = c.Capture()
	requireCacheError(t, err, agent.ErrClockSkew)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// A clean restart keeps the observed clock floor instead of resurrecting TTL.
	reopened, err := agent.OpenPolicyCache(t.Context(), f.opts)
	if err == nil {
		defer reopened.Close()
		_, err = reopened.Capture()
	}
	requireCacheError(t, err, agent.ErrClockSkew)
}

func TestPolicyCacheExternalFloorRejectsAuthenticBackup(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	if err := c.Install(t.Context(), f.resource(t, 1, "old")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.opts.Directory, "policy-cache.sqlite")
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err = agent.OpenPolicyCache(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Install(t.Context(), f.resource(t, 2, "new")); err != nil {
		t.Fatal(err)
	}
	floor, err := c.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	f.opts.Minimum = floor
	_, err = agent.OpenPolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrRollback)
}
func TestPolicyCacheConcurrentInstallCannotRegress(t *testing.T) {
	f := newCacheFixture(t)
	c := initCache(t, f)
	resources := make([]*agentv1.SignedResource, 16)
	for i := range resources {
		resources[i] = f.resource(t, uint64(i+1), "bundle")
	}
	var workers sync.WaitGroup
	for _, resource := range resources {
		workers.Go(func() {
			err := c.Install(t.Context(), resource)
			if err != nil && !errors.Is(err, agent.ErrRollback) {
				t.Errorf("install: %v", err)
			}
			if _, err := c.Capture(); err != nil {
				t.Errorf("capture: %v", err)
			}
		})
	}
	workers.Wait()
	mark, err := c.Watermark(t.Context())
	if err != nil || mark.Revision != 16 {
		t.Fatalf("durable revision: %+v %v", mark, err)
	}
	view, err := c.Capture()
	if err != nil || view.Claims().Revision != 16 {
		t.Fatalf("memory diverged: %v", err)
	}
}

func TestPolicyCacheRefusesPublicDirectoryAndSymlink(t *testing.T) {
	f := newCacheFixture(t)
	if err := os.MkdirAll(f.opts.Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.opts.Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := agent.InitializePolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
	if err := os.Chmod(f.opts.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	c := initCache(t, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.opts.Directory, "policy-cache.sqlite")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	_, err = agent.OpenPolicyCache(t.Context(), f.opts)
	requireCacheError(t, err, agent.ErrCacheCorrupt)
}
