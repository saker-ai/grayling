package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
)

func signedRevocation(t *testing.T, f *runtimeFixture, epoch uint64, hashes, cancelled []string) *agentv1.SignedResource {
	t.Helper()
	hashes = slices.Clone(hashes)
	cancelled = slices.Clone(cancelled)
	slices.Sort(hashes)
	slices.Sort(cancelled)
	if cancelled == nil {
		cancelled = []string{}
	}
	d := agent.RevocationDocument{SchemaVersion: 1, Purpose: "bundle-revocation", Scope: f.options.Scope, ControlPlaneEpoch: 1, Epoch: epoch, BundleHashes: hashes, CancelledHashes: cancelled}
	body, err := agent.MarshalRevocation(d)
	if err != nil {
		t.Fatal(err)
	}
	now := f.f.opts.Now()
	resource, err := control.SignResource(agent.Claims{Scope: f.options.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_REVOCATION, Key: agent.RevocationKey, ControlPlaneEpoch: 1, Revision: epoch, BundleHash: sha256.Sum256(body), IssuedUnixMs: now.UnixMilli(), ExpiresUnixMs: now.Add(time.Minute).UnixMilli(), KeyID: "key"}, body, f.f.private)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}
func TestRevocationSignedCatalogPersistsAndNeverShrinks(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	claims, err := agent.ParseClaims(f.input.Activation.Claims)
	if err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(claims.BundleHash[:])
	otherHash := sha256.Sum256([]byte("other-bundle"))
	other := hex.EncodeToString(otherHash[:])
	proof := signedRevocation(t, f, 1, []string{hash}, nil)
	if err = m.InstallRevocation(t.Context(), proof); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Applied(); !errors.Is(err, agent.ErrPolicyRevoked) {
		t.Fatal("signed denial not applied", err)
	}
	if err = m.InstallRevocation(t.Context(), proof); err != nil {
		t.Fatal("exact replay rejected", err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 1, []string{other}, nil)); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("epoch conflict allowed", err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 2, []string{other}, nil)); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("catalog shrink allowed", err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 2, []string{hash, other}, []string{hash})); err != nil {
		t.Fatal(err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 3, []string{hash, other}, nil)); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("cancellation downgraded", err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.f.clock.Add(61000)
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Restore(t.Context()); err == nil {
		t.Fatal("proof expiry erased durable denial")
	}
	if err = m.InstallRevocation(t.Context(), proof); !errors.Is(err, agent.ErrExpired) {
		t.Fatal("expired delivery proof accepted", err)
	}
}
func TestRevocationRejectsTamperedSignedCatalog(t *testing.T) {
	f := newRuntimeFixture(t)
	h := sha256.Sum256([]byte("bundle"))
	hash := hex.EncodeToString(h[:])
	proof := signedRevocation(t, f, 1, []string{hash}, nil)
	if _, _, err := agent.VerifyRevocation(f.options.Verifier, proof, f.options.Scope, agent.Watermark{}, f.options.Now()); err != nil {
		t.Fatal(err)
	}
	proof.Payload[0] ^= 1
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if err = m.InstallRevocation(t.Context(), proof); err == nil {
		t.Fatal("tampered proof accepted")
	}
}

func TestRevocationCancellationRemainsPerBundle(t *testing.T) {
	f := newRuntimeFixture(t)
	entered := make(chan context.Context, 1)
	finished := make(chan struct{})
	good := f.options.Builder
	f.options.Builder = func(ctx context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		r, err := good(ctx, s)
		r.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			entered <- r.Context()
			<-r.Context().Done()
			close(finished)
		})
		return r, err
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	go func() {
		_ = lease.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://checkout/stream", nil))
	}()
	requestCtx := <-entered
	claims, err := agent.ParseClaims(f.input.Activation.Claims)
	if err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(claims.BundleHash[:])
	otherDigest := sha256.Sum256([]byte("unrelated"))
	other := hex.EncodeToString(otherDigest[:])
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 1, []string{hash}, nil)); err != nil {
		t.Fatal(err)
	}
	if err = requestCtx.Err(); err != nil {
		t.Fatal("graceful denial canceled active stream", err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 2, []string{hash, other}, []string{other})); err != nil {
		t.Fatal(err)
	}
	if err = requestCtx.Err(); err != nil {
		t.Fatal("unrelated urgent denial canceled graceful stream", err)
	}
	if err = m.InstallRevocation(t.Context(), signedRevocation(t, f, 3, []string{hash, other}, []string{hash, other})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("upgraded denial failed to cancel own stream")
	}
}

func TestRevocationExternalFloorRejectsWholeDatabaseReplay(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.options.Directory, "runtime-cache.sqlite")
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("deny unrelated bundle"))
	hash := hex.EncodeToString(digest[:])
	proof := signedRevocation(t, f, 1, []string{hash}, nil)
	if err = m.InstallRevocation(t.Context(), proof); err != nil {
		t.Fatal(err)
	}
	floor, err := m.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 || floor.ControlPlaneEpoch != 1 {
		t.Fatal(floor, err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, backup, 0600); err != nil {
		t.Fatal(err)
	}
	f.options.MinimumRevocation = floor
	if _, err = agent.OpenRuntime(t.Context(), f.options); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("whole-file replay erased denial floor", err)
	}
	// A genuinely empty initialization can fetch proof, but cannot grant routing
	// until the externally required revocation floor has actually been recovered.
	f.options.Directory = filepath.Join(t.TempDir(), "fresh-runtime")
	m, err = agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Install(t.Context(), "activate-1", f.input); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("empty initialization bypassed external revocation floor", err)
	}
	if err = m.InstallRevocation(t.Context(), proof); err != nil {
		t.Fatal("fresh runtime cannot recover required denial proof", err)
	}
}

func TestRevocationProofRevisionAdvancesWithoutRefreshingRuntime(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	unrelated := sha256.Sum256([]byte("unrelated denied bundle"))
	hash := hex.EncodeToString(unrelated[:])
	first := signedRevocation(t, f, 1, []string{hash}, nil)
	if err = m.InstallRevocation(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	f.f.clock.Add(2000)
	claims, err := agent.ParseClaims(first.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.Revision = 2
	claims.IssuedUnixMs = f.options.Now().UnixMilli()
	claims.ExpiresUnixMs = f.options.Now().Add(time.Minute).UnixMilli()
	second, err := control.SignResource(claims, first.Payload, f.f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.InstallRevocation(t.Context(), second); err != nil {
		t.Fatal("fresh proof for unchanged epoch rejected", err)
	}
	floor, err := m.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 || floor.ArtifactRevision != 2 {
		t.Fatal("proof and content floors conflated", floor, err)
	}
	report, err := m.Applied()
	if err != nil || report.Release.Generation != 1 {
		t.Fatal("proof refresh removed active view", report, err)
	}
	if err = m.InstallRevocation(t.Context(), first); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("older proof artifact overwrote floor", err)
	}
	claims.ExpiresUnixMs++
	conflicting, err := control.SignResource(claims, first.Payload, f.f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.InstallRevocation(t.Context(), conflicting); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("same artifact changed signed metadata", err)
	}
	f.f.clock.Add(9000)
	if _, err = m.Applied(); !errors.Is(err, agent.ErrPolicyStale) {
		t.Fatal("proof refresh extended positive runtime freshness", err)
	}
}

func TestRevocationProofArtifactFloorSurvivesWholeFileReplay(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("denied"))
	first := signedRevocation(t, f, 1, []string{hex.EncodeToString(h[:])}, nil)
	if err = m.InstallRevocation(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.options.Directory, "runtime-cache.sqlite")
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := agent.ParseClaims(first.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.Revision = 2
	second, err := control.SignResource(claims, first.Payload, f.f.private)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.InstallRevocation(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	floor, err := m.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 || floor.ArtifactRevision != 2 {
		t.Fatal(floor, err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, backup, 0600); err != nil {
		t.Fatal(err)
	}
	f.options.MinimumRevocation = floor
	if _, err = agent.OpenRuntime(t.Context(), f.options); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("same-epoch database replay lowered proof artifact floor", err)
	}
}

func TestRevocationProofArtifactMaximumUint64PersistsExactly(t *testing.T) {
	f := newRuntimeFixture(t)
	h := sha256.Sum256([]byte("denied"))
	proof := signedRevocation(t, f, 1, []string{hex.EncodeToString(h[:])}, nil)
	claims, err := agent.ParseClaims(proof.Claims)
	if err != nil {
		t.Fatal(err)
	}
	claims.Revision = math.MaxUint64
	proof, err = control.SignResource(claims, proof.Payload, f.f.private)
	if err != nil {
		t.Fatal(err)
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.InstallRevocation(t.Context(), proof); err != nil {
		t.Fatal(err)
	}
	floor, err := m.RevocationFloor(t.Context())
	if err != nil || floor.ArtifactRevision != math.MaxUint64 || floor.Epoch != 1 {
		t.Fatal(floor, err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.options.MinimumRevocation = floor
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	restored, err := m.RevocationFloor(t.Context())
	if err != nil || restored != floor {
		t.Fatal("sealed proof floor lost uint64 precision", restored, err)
	}
}
