//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func waitReleaseLoop(t *testing.T, loop *client.ReleaseLoop, condition func(client.ReleaseLoopState) bool) client.ReleaseLoopState {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := loop.State()
		if condition(state) {
			return state
		}
		select {
		case <-ticker.C:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-deadline.C:
			t.Fatalf("release state timeout: running=%v prepare=%v activation=%v revoke=%v", state.Running, state.PreparationError, state.ActivationError, state.RevocationError)
		}
	}
}
func releaseLoopRuntime(t *testing.T, f *registrationFixture, v *client.Verifier, set *client.PreparationSet) (*client.RuntimeManager, *client.ReleaseLoop) {
	t.Helper()
	runtime, err := client.InitializeRuntime(t.Context(), client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{77}, 32), Verifier: v, Now: time.Now, Builder: set.BuildRuntime})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := client.NewReleaseLoop(set, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, loop
}
func stopReleaseLoop(t *testing.T, cancel context.CancelFunc, done <-chan error, runtime *client.RuntimeManager, set *client.PreparationSet) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Error("event workers did not drain")
		return
	}
	if err := runtime.Close(t.Context()); err != nil {
		t.Error(err)
	}
	if err := set.Close(t.Context()); err != nil {
		t.Error(err)
	}
}

func TestReleaseLoopRegisteredActualInstallAndReplicaReconnect(t *testing.T) {
	parallelAgentFixture(t)
	f, p, v, body := preparationDriverFixture(t)
	var builds, closed atomic.Int64
	set, err := client.NewPreparationSet(preparationSetOptions(f, v, body, &builds, &closed))
	if err != nil {
		t.Fatal(err)
	}
	runtime, loop := releaseLoopRuntime(t, f, v, set)
	registration, err := client.NewInstanceRegistration(f.request)
	if err != nil {
		t.Fatal(err)
	}
	first, channel := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	second, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	var dials atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- client.RunAgent(ctx, func(context.Context) (client.Options, error) {
			server := first
			if dials.Add(1) > 1 {
				server = second
			}
			return client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}, nil
		}, registration, loop)
	}()
	defer stopReleaseLoop(t, cancel, done, runtime, set)
	waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool { return s.Running && registration.State().Connected })
	confirmPreparedNaming(t, f)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	prepared := waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool { return s.Preparation.Receipt != nil })
	if prepared.Preparation.Workload != nil || prepared.Preparation.Prepared == nil {
		t.Fatal("diagnostics leaked ownership or pretended preparation")
	}
	if _, err := runtime.Capture(); !errors.Is(err, client.ErrRuntimeNotReady) {
		t.Fatal("Prepared installed runtime", err)
	}
	if gate, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil || !gate.Eligible {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	installed := waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool { return s.Activation.Receipt != nil })
	capture, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	if err := capture.ServeHTTP(response, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	capture.Release()
	if response.Body.String() != "1/"+string(body) || builds.Load() != 1 {
		t.Fatal("actual handler not installed once")
	}
	shutdown, finish := context.WithTimeout(t.Context(), 3*time.Second)
	err = channel.Shutdown(shutdown)
	finish()
	if err != nil {
		t.Fatal(err)
	}
	waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool {
		return s.Running && dials.Load() > 1 && registration.State().Connected
	})
	current := loop.State()
	if builds.Load() != 1 || !proto.Equal(installed.Activation.Applied, current.Activation.Applied) || !proto.Equal(installed.Activation.Receipt, current.Activation.Receipt) {
		t.Fatal("reconnect rebuilt or changed known Applied receipt")
	}
	current.Activation.Applied.BootId = "mutated"
	if loop.State().Activation.Applied.BootId == "mutated" {
		t.Fatal("diagnostic output leaked mutable report")
	}
	if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_activation_reports`)[0][0]; count != "1" {
		t.Fatal("reconnect created duplicate terminal outcome", count)
	}
}

func TestReleaseLoopSlowOldPreparationCannotBlockNewTarget(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, body := preparationDriverFixture(t)
	var builds, closed, entered atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	opts := preparationSetOptions(f, v, body, &builds, &closed)
	construct := opts.Builder
	opts.Builder = func(ctx context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
		if entered.Add(1) == 1 {
			close(started)
			<-release
		}
		return construct(ctx, s)
	}
	set, err := client.NewPreparationSet(opts)
	if err != nil {
		t.Fatal(err)
	}
	runtime, loop := releaseLoopRuntime(t, f, v, set)
	c, _ := preparationSetConnection(t, f, v)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- loop.Serve(ctx, c) }()
	defer func() { unblock.Do(func() { close(release) }); stopReleaseLoop(t, cancel, done, runtime, set) }()
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old builder did not start")
	}
	latest, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "second", 1), v)
	if err != nil {
		t.Fatal(err)
	}
	accepted := waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool {
		return s.Preparation.CommandID == latest.CommandID && s.Preparation.Receipt != nil
	})
	unblock.Do(func() { close(release) })
	// The old callback finishing cannot replace the newer diagnostic or candidate.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for closed.Load() < 1 {
		select {
		case <-deadline.C:
			t.Fatal("old construction not cleaned")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if !proto.Equal(loop.State().Preparation.Prepared, accepted.Preparation.Prepared) || builds.Load() != 2 {
		t.Fatal("late old result overwrote current candidate")
	}
	if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; count != "1" {
		t.Fatal("stale construction reported evidence", count)
	}
}

func TestReleaseLoopRevocationDoesNotWaitForBlockedBuilder(t *testing.T) {
	parallelAgentFixture(t)
	f, p, v, body := preparationDriverFixture(t)
	var builds, closed, entered atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	opts := preparationSetOptions(f, v, body, &builds, &closed)
	construct := opts.Builder
	opts.Builder = func(ctx context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
		if entered.Add(1) == 2 {
			close(started)
			<-release
		}
		return construct(ctx, s)
	}
	set, err := client.NewPreparationSet(opts)
	if err != nil {
		t.Fatal(err)
	}
	runtime, loop := releaseLoopRuntime(t, f, v, set)
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	connection := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	connection.Hello.RequiredCapabilities = []string{"release.prepare", "release.activate", "release.revoke"}
	c, err := client.Dial(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- loop.Serve(ctx, c) }()
	defer func() { unblock.Do(func() { close(release) }); stopReleaseLoop(t, cancel, done, runtime, set) }()
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool { return s.Preparation.Receipt != nil })
	if gate, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil || !gate.Eligible {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	active := waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool { return s.Activation.Receipt != nil })
	capture, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Release()
	if _, err := f.a.QueueReprepare(t.Context(), reprepareRequest(f, "blocked", 1), v); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement builder did not start")
	}
	if _, err := p.RevokeBundles(t.Context(), f.g.Scope, adminMutation("deny-while-building"), control.RevokeBundlesRequest{BundleHashes: []string{active.Activation.Applied.Release.BundleHash}, CancelActiveStreams: true, Reason: "security incident"}, v); err != nil {
		t.Fatal(err)
	}
	waitReleaseLoop(t, loop, func(s client.ReleaseLoopState) bool {
		return s.RevocationEpoch == 1 && s.RevocationArtifactRevision > 0
	})
	if _, err := runtime.Capture(); err == nil {
		t.Fatal("revoked view still selectable")
	}
	recorder := httptest.NewRecorder()
	if err := capture.ServeHTTP(recorder, httptest.NewRequest("GET", "https://service/business", nil)); err == nil {
		t.Fatal("old captured revoked view still usable")
	}
	if entered.Load() != 2 || builds.Load() != 1 {
		t.Fatal("revocation waited for blocked constructor")
	}
	floor, err := runtime.RevocationFloor(t.Context())
	if err != nil || floor.Epoch != 1 {
		t.Fatal("denial was not durable before observation", err)
	}
	capture.Release()
	// Cancel before releasing the hostile callback; no terminal outcome is
	// invented for a construction whose report was never attempted.
	cancel()
	unblock.Do(func() { close(release) })
	if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; count != "1" {
		t.Fatal("blocked construction reported evidence", count)
	}
}
