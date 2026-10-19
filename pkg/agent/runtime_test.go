package agent_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/pkg/model"
)

type runtimeFixture struct {
	f        *cacheFixture
	options  agent.RuntimeOptions
	input    agent.RuntimeInput
	document agent.ActivationDocument
	builds   atomic.Int64
	closes   atomic.Int64
}

func newRuntimeFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	f, d, _, _ := activationSDKFixture(t)
	_, b, payload := bundleFixture(t)
	artifact := []byte("actual frontend resource")
	digest := sha256.Sum256(artifact)
	b.Builds[0].Resources[0].SHA256 = hex.EncodeToString(digest[:])
	body, err := agent.MarshalBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	signed, hash := signBundle(t, f, body, nil)
	d.BundleHash = hex.EncodeToString(hash[:])
	d.Generation = 1
	d.ReleaseID = "release-1"
	policy, err := control.SignResource(agent.Claims{Scope: f.opts.Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: b.Policy.Key, ControlPlaneEpoch: 1, Revision: b.Policy.Revision, BundleHash: hash, IssuedUnixMs: f.opts.Now().UnixMilli(), ExpiresUnixMs: f.opts.Now().Add(time.Minute).UnixMilli(), KeyID: "key"}, payload, f.private)
	if err != nil {
		t.Fatal(err)
	}
	activationBody, err := agent.MarshalActivation(d)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &runtimeFixture{f: f, document: d, input: agent.RuntimeInput{Activation: signActivationSDK(t, f, d, activationBody, hash), Bundle: signed, Policy: policy}}
	fixture.options = agent.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.opts.Identity, Scope: f.opts.Scope, PolicyKey: f.opts.Key, InstanceID: "instance", BootID: "boot", BuildID: "stable", ImageDigest: b.Builds[0].ImageDigest, AuthenticationKey: f.opts.AuthenticationKey, Verifier: f.opts.Verifier, MaxAge: 10 * time.Second, Now: f.opts.Now}
	fixture.options.Builder = func(_ context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		fixture.builds.Add(1)
		return agent.RuntimeResources{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta, ok := agent.RuntimeFromContext(r.Context())
			if !ok || meta != s.Metadata {
				t.Error("handler did not capture installed metadata")
			}
			_, _ = w.Write(artifact)
		}), Artifacts: map[string][]byte{"frontend": artifact}, Close: func() error { fixture.closes.Add(1); return nil }}, nil
	}
	return fixture
}
func (f *runtimeFixture) generation(t *testing.T, g uint64) agent.RuntimeInput {
	t.Helper()
	d := f.document
	d.Generation = g
	if g == 2 {
		d.ReleaseID = "release-2"
	} else {
		t.Fatal("unsupported fixture generation")
	}
	body, err := agent.MarshalActivation(d)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := agent.ParseClaims(f.input.Activation.Claims)
	if err != nil {
		t.Fatal(err)
	}
	out := f.input
	out.Activation = signActivationSDK(t, f.f, d, body, claims.BundleHash)
	return out
}
func TestRuntimeActualInstallRestartAndNoFreshnessRenewal(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Capture(); !errors.Is(err, agent.ErrRuntimeNotReady) {
		t.Fatal(err)
	}
	report, err := m.Install(t.Context(), "activate-1", f.input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Release.Generation != 1 || report.PolicyRevision != f.document.PolicyRevision || f.builds.Load() != 1 {
		t.Fatal("inexact Applied", report)
	}
	lease, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := lease.Select(model.EvaluationContext{}, 0)
	if err != nil || !decision.Local || decision.BuildID != "stable" {
		t.Fatal(decision, err)
	}
	response := httptest.NewRecorder()
	if err := lease.ServeHTTP(response, httptest.NewRequest("GET", "https://checkout/", nil)); err != nil {
		t.Fatal(err)
	}
	if response.Body.String() != "actual frontend resource" {
		t.Fatal(response.Body.String())
	}
	lease.Release()
	floor, err := m.Watermark(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.f.clock.Add(6000)
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	if f.builds.Load() != 1 {
		t.Fatal("replay rebuilt resources")
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.options.Minimum = floor
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Applied(); !errors.Is(err, agent.ErrRuntimeNotReady) {
		t.Fatal("restart falsely ready", err)
	}
	if _, err = m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.builds.Load() != 2 {
		t.Fatal("restore skipped actual construction")
	}
	f.f.clock.Add(5000)
	if _, err = m.Applied(); !errors.Is(err, agent.ErrPolicyStale) {
		t.Fatal("restore renewed age", err)
	}
}
func TestRuntimeFailedBuildPreservesViewAndRestartFloor(t *testing.T) {
	f := newRuntimeFixture(t)
	good := f.options.Builder
	var fail atomic.Bool
	f.options.Builder = func(ctx context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		r, err := good(ctx, s)
		if fail.Load() {
			r.Artifacts = map[string][]byte{"frontend": []byte("wrong")}
		}
		return r, err
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	old, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	newer := f.generation(t, 2)
	if _, err = m.Install(t.Context(), "activate-1", newer); !errors.Is(err, agent.ErrRuntimeResources) {
		t.Fatal("invalid resources installed", err)
	}
	current, err := m.Applied()
	if err != nil || current.Release.Generation != 1 {
		t.Fatal(current, err)
	}
	fail.Store(false)
	if _, err = m.Install(t.Context(), "activate-1", newer); err != nil {
		t.Fatal(err)
	}
	meta, err := old.Metadata()
	if err != nil || meta.Watermark.Generation != 1 {
		t.Fatal("capture crossed publication", meta, err)
	}
	old.Release()
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Install(t.Context(), "activate-1", f.input); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("restart lost generation floor", err)
	}
	if _, err = m.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeRevocationCoversRetainedHandlerAndRestart(t *testing.T) {
	f := newRuntimeFixture(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	good := f.options.Builder
	f.options.Builder = func(ctx context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		r, err := good(ctx, s)
		r.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) })
		return r, err
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- lease.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://checkout/", nil))
	}()
	<-entered
	meta, err := lease.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Revoke(t.Context(), 1, [][32]byte{meta.Watermark.BundleHash}, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("revocation failed to cancel handler")
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err = lease.Metadata(); !errors.Is(err, agent.ErrPolicyRevoked) {
		t.Fatal("retained capture escaped revocation", err)
	}
	lease.Release()
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if _, err = m.Restore(t.Context()); err == nil {
		t.Fatal("restart erased revocation")
	}
}

func TestRuntimeSlowBuilderCannotOverwriteNewerPublication(t *testing.T) {
	f := newRuntimeFixture(t)
	entered := make(chan struct{})
	resume := make(chan struct{})
	good := f.options.Builder
	f.options.Builder = func(ctx context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		if s.Metadata.Watermark.Generation == 1 {
			close(entered)
			<-resume
		}
		return good(ctx, s)
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	result := make(chan error, 1)
	go func() { _, err := m.Install(t.Context(), "activate-1", f.input); result <- err }()
	<-entered
	if _, err = m.Install(t.Context(), "activate-1", f.generation(t, 2)); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err = <-result; !errors.Is(err, agent.ErrRollback) {
		t.Fatal("slow builder overwrote generation", err)
	}
	report, err := m.Applied()
	if err != nil || report.Release.Generation != 2 {
		t.Fatal(report, err)
	}
}
func TestRuntimeCancelAndPoolOwnership(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	old, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = m.Watermark(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = m.Applied(); err != nil {
		t.Fatal("canceled read poisoned installed view", err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.generation(t, 2)); err != nil {
		t.Fatal(err)
	}
	if f.closes.Load() != 0 {
		t.Fatal("resources retired with capture outstanding")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err = m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Close ignored outstanding capture", err)
	}
	old.Release()
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.closes.Load() != 2 {
		t.Fatal("resource owners not closed exactly once", f.closes.Load())
	}
}
func TestRuntimeExpiredRestoreAndBootChangeRemainUnready(t *testing.T) {
	for _, change := range []string{"expired", "boot"} {
		t.Run(change, func(t *testing.T) {
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
			if change == "boot" {
				f.options.BootID = "new-boot"
			} else {
				f.f.clock.Add(21000)
			}
			m, err = agent.OpenRuntime(t.Context(), f.options)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(t.Context())
			if _, err = m.Restore(t.Context()); err == nil {
				t.Fatal("unauthorized restore")
			}
			if _, err = m.Applied(); !errors.Is(err, agent.ErrRuntimeNotReady) {
				t.Fatal("failed restore became ready", err)
			}
		})
	}
}

func TestRuntimeDurableWriteFailureNeverReportsApplied(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.options.Directory, "runtime-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `CREATE TRIGGER ignore_runtime BEFORE UPDATE ON agent_runtime_cache BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if report, err := m.Install(t.Context(), "activate-1", f.generation(t, 2)); !errors.Is(err, agent.ErrCacheIO) || report != nil {
		t.Fatal("suppressed write reported installed", report, err)
	}
	if _, err = m.Applied(); !errors.Is(err, agent.ErrCacheIO) {
		t.Fatal("failed store did not fail closed", err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `DROP TRIGGER ignore_runtime`); err != nil {
		t.Fatal(err)
	}
	m, err = agent.OpenRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	report, err := m.Restore(t.Context())
	if err != nil || report.Release.Generation != 1 {
		t.Fatal("partial durable publication", report, err)
	}
}
func TestRuntimeSealedRecordTamperAndExclusiveOwnership(t *testing.T) {
	f := newRuntimeFixture(t)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = agent.OpenRuntime(t.Context(), f.options); err == nil {
		t.Fatal("second process owner accepted")
	}
	if _, err = m.Install(t.Context(), "activate-1", f.input); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.options.Directory, "runtime-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `UPDATE agent_runtime_cache SET seal=zeroblob(32)`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Watermark(t.Context()); !errors.Is(err, agent.ErrCacheCorrupt) {
		t.Fatal("tamper ignored", err)
	}
	if _, err = m.Applied(); !errors.Is(err, agent.ErrCacheCorrupt) {
		t.Fatal("corrupt store remains ready", err)
	}
	if err = m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = agent.OpenRuntime(t.Context(), f.options); !errors.Is(err, agent.ErrCacheCorrupt) {
		t.Fatal("tampered journal reopened", err)
	}
}

func TestRuntimeDeadlineCancelsHandlerWithoutRevocation(t *testing.T) {
	f := newRuntimeFixture(t)
	// Wall clock stays fixed. The request still expires against a process-local
	// monotonic deadline, so frozen caller time cannot keep streams authorized.
	f.options.MaxAge = 100 * time.Millisecond
	good := f.options.Builder
	entered := make(chan struct{})
	expired := make(chan error, 1)
	f.options.Builder = func(ctx context.Context, s agent.RuntimeSpec) (agent.RuntimeResources, error) {
		r, err := good(ctx, s)
		r.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			expired <- r.Context().Err()
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
	finished := make(chan error, 1)
	go func() {
		finished <- lease.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://checkout/", nil))
	}()
	<-entered
	select {
	case err := <-expired:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("view deadline did not cancel handler")
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err = m.Applied(); !errors.Is(err, agent.ErrExpired) {
		t.Fatal("expired runtime reported applied", err)
	}
}

func TestRuntimeCommitWaitCrossingExpiryDoesNotPublish(t *testing.T) {
	f := newRuntimeFixture(t)
	var calls atomic.Int64
	committing := make(chan struct{})
	f.options.Now = func() time.Time {
		now := f.f.opts.Now()
		if calls.Add(1) == 3 {
			close(committing)
		}
		return now
	}
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	db, err := sql.Open("sqlite", filepath.Join(f.options.Directory, "runtime-cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `UPDATE agent_runtime_cache SET version=version WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		report *agentv1.Applied
		err    error
	}
	result := make(chan outcome, 1)
	go func() { report, err := m.Install(t.Context(), "activate-1", f.input); result <- outcome{report, err} }()
	<-committing
	// The final pre-commit clock sample is fixed before releasing the writer.
	// Expiry is crossed while SQLite's write lock is held by another connection.
	f.f.clock.Add(21000)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	out := <-result
	if out.report != nil || !errors.Is(out.err, agent.ErrExpired) {
		t.Fatal("commit crossing expiry produced Applied", out.report, out.err)
	}
	if _, err = m.Applied(); !errors.Is(err, agent.ErrRuntimeNotReady) {
		t.Fatal("expired durable candidate published", err)
	}
	mark, err := m.Watermark(t.Context())
	if err != nil || mark.Generation != 1 {
		t.Fatal("commit lost durable floor", mark, err)
	}
}

func TestRuntimeDiscoveryAdvancesIndependentlyWithoutGenerationRollback(t *testing.T) {
	f := newRuntimeFixture(t)
	resign := func(d agent.ActivationDocument, revision uint64) agent.RuntimeInput {
		t.Helper()
		body, err := agent.MarshalActivation(d)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := agent.ParseClaims(f.input.Activation.Claims)
		if err != nil {
			t.Fatal(err)
		}
		claims.Key = agent.ActivationKey(d.Generation)
		claims.Revision = revision
		resource, err := control.SignResource(claims, body, f.f.private)
		if err != nil {
			t.Fatal(err)
		}
		input := f.input
		input.Activation = resource
		return input
	}
	d := f.document
	d.DiscoveryRevision = 1
	first := resign(d, 1)
	m, err := agent.InitializeRuntime(t.Context(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	initial, err := m.Install(t.Context(), "activate-1", first)
	if err != nil {
		t.Fatal(err)
	}
	old, err := m.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	d.DiscoveryRevision = 2
	second := resign(d, 2)
	next, err := m.Install(t.Context(), "activate-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if initial.PolicyRevision != next.PolicyRevision || initial.Release.Generation != next.Release.Generation || next.DiscoveryRevision != 2 {
		t.Fatal("discovery update changed fixed policy or generation", initial, next)
	}
	previous, err := old.Metadata()
	if err != nil || previous.Watermark.DiscoveryRevision != 1 {
		t.Fatal("old capture mixed independent versions", previous, err)
	}
	if _, err = m.Install(t.Context(), "activate-1", first); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("old discovery overwrote newer view", err)
	}
	d.Generation = 2
	d.ReleaseID = "release-2"
	d.DiscoveryRevision = 1
	if _, err = m.Install(t.Context(), "activate-1", resign(d, 3)); !errors.Is(err, agent.ErrRollback) {
		t.Fatal("higher generation reset independent discovery floor", err)
	}
}
