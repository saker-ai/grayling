//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func preparationSetOptions(f *registrationFixture, v *client.Verifier, body []byte, builds, closed *atomic.Int64) client.PreparationOptions {
	return client.PreparationOptions{Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, Verifier: v, Now: time.Now, Builder: func(_ context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
		serial := builds.Add(1)
		loaded := bytes.Clone(body)
		return client.PreparationResources{Artifacts: map[string][]byte{"handler": loaded}, CheckHealth: func(context.Context) error { return nil }, Instantiate: func(_ context.Context, spec client.RuntimeSpec) (client.RuntimeResources, error) {
			return client.RuntimeResources{Artifacts: map[string][]byte{"handler": loaded}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				meta, ok := client.RuntimeFromContext(r.Context())
				if !ok || meta != spec.Metadata {
					http.Error(w, "incoherent view", 500)
					return
				}
				_, _ = fmt.Fprintf(w, "%d/%s", serial, loaded)
			}), Close: func() error { return nil }}, nil
		}, Close: func() error { closed.Add(1); return nil }}, nil
	}}
}
func preparationSetConnection(t *testing.T, f *registrationFixture, v *client.Verifier) (*client.Client, client.Options) {
	t.Helper()
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare", "release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, opts
}

func TestPreparationSetHotSwapReconnectAndStaleBarrier(t *testing.T) {
	parallelAgentFixture(t)
	f, p, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	c, connection := preparationSetConnection(t, f, v)
	first := clientEvent(t, c)
	if first.GetPrepare() == nil || first.GetPrepare().TargetRevision != 1 {
		t.Fatal("missing target revision")
	}
	var builds, closed atomic.Int64
	set, err := client.NewPreparationSet(preparationSetOptions(f, v, body, &builds, &closed))
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(t.Context())
	if _, err := set.BuildRuntime(t.Context(), client.RuntimeSpec{}); !errors.Is(err, client.ErrRuntimeNotReady) {
		t.Fatal("empty candidate ready", err)
	}
	prepared, err := set.Prepare(t.Context(), c, first)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	successor, err := client.Dial(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	retry, err := set.Prepare(t.Context(), successor, first)
	if err != nil || builds.Load() != 1 || !proto.Equal(prepared.Prepared, retry.Prepared) || !proto.Equal(prepared.Receipt, retry.Receipt) {
		t.Fatal("reconnect rebuilt candidate or renewed report", err)
	}
	runtime, err := client.InitializeRuntime(t.Context(), client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{55}, 32), Verifier: v, Now: time.Now, Builder: set.BuildRuntime})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	if result, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil || !result.Eligible {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	installed, err := successor.InstallActivation(t.Context(), clientEvent(t, successor), runtime)
	if err != nil {
		t.Fatal(err)
	}
	old, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	if _, err := f.a.QueueReprepare(t.Context(), reprepareRequest(f, "second", 1), v); err != nil {
		t.Fatal(err)
	}
	second := clientEvent(t, successor)
	if second.GetPrepare() == nil || second.GetPrepare().TargetRevision != 2 {
		t.Fatal("replacement target missing", second)
	}
	next, err := set.Prepare(t.Context(), successor, second)
	if err != nil || next.Prepared == nil || builds.Load() != 2 {
		t.Fatal(err)
	}
	if _, err := set.Prepare(t.Context(), successor, first); !errors.Is(err, client.ErrRollback) {
		t.Fatal("stale target displaced current candidate", err)
	}
	conflict := proto.Clone(second).(*agentv1.Envelope)
	conflict.GetPrepare().Bundle.Revision++
	if _, err := set.Prepare(t.Context(), successor, conflict); !errors.Is(err, client.ErrRollback) {
		t.Fatal("same target changed immutable resource", err)
	}
	aq := activationRequest(f)
	aq.OperationID = "refresh-set"
	aq.ExpectedRevision = installed.Receipt.ReleaseRevision
	if _, err := p.RefreshActivation(t.Context(), aq, 1, v); err != nil {
		t.Fatal(err)
	}
	if _, err := successor.InstallActivation(t.Context(), clientEvent(t, successor), runtime); err != nil {
		t.Fatal(err)
	}
	current, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	a, b := httptest.NewRecorder(), httptest.NewRecorder()
	if err := old.ServeHTTP(a, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	if err := current.ServeHTTP(b, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	if a.Body.String() != "1/"+string(body) || b.Body.String() != "2/"+string(body) || closed.Load() != 0 {
		t.Fatal("candidate replacement mixed or prematurely closed views")
	}
	old.Release()
	for until := time.Now().Add(time.Second); closed.Load() < 1 && time.Now().Before(until); {
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("retired old workload did not drain")
	}
	current.Release()
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(t.Context()); err != nil || closed.Load() != 2 {
		t.Fatal("selected workload not closed once", err)
	}
}

func TestPreparationSetSlowOldBuilderCannotOverwriteNewTarget(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	c, _ := preparationSetConnection(t, f, v)
	first := clientEvent(t, c)
	var builds, closed, entered atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	o := preparationSetOptions(f, v, body, &builds, &closed)
	construct := o.Builder
	o.Builder = func(ctx context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
		if entered.Add(1) == 1 {
			close(started)
			<-release
		}
		return construct(ctx, s)
	}
	set, err := client.NewPreparationSet(o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock.Do(func() { close(release) }); set.Close(context.Background()) }()
	old := make(chan error, 1)
	go func() { _, err := set.Prepare(t.Context(), c, first); old <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("builder not started")
	}
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "second", 1), v); err != nil {
		t.Fatal(err)
	}
	second := clientEvent(t, c)
	next, err := set.Prepare(t.Context(), c, second)
	if err != nil {
		t.Fatal("new target waited for old builder", err)
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-old:
		if !errors.Is(err, client.ErrRollback) {
			t.Fatal("old work not fenced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old builder did not settle")
	}
	repeat, err := set.Prepare(t.Context(), c, second)
	if err != nil || builds.Load() != 2 || !proto.Equal(repeat.Prepared, next.Prepared) {
		t.Fatal("old work displaced newer candidate", err)
	}
	if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; count != "1" {
		t.Fatal("canceled old builder reported evidence", count)
	}
}

func TestPreparationSetBoundsStuckConstructionAndCleanup(t *testing.T) {
	parallelAgentFixture(t)
	for _, tc := range []struct {
		name         string
		limit        int
		construction bool
	}{{"construction", 4, true}, {"cleanup", 8, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, v, body := preparationDriverFixture(t)
			var builds, closed atomic.Int64
			o := preparationSetOptions(f, v, body, &builds, &closed)
			base := o.Builder
			release := make(chan struct{})
			var unblock sync.Once
			o.Builder = func(ctx context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
				r, err := base(ctx, s)
				if tc.construction {
					<-release
				} else {
					r.CheckHealth = func(context.Context) error { return errors.New("health failed") }
					closeResource := r.Close
					r.Close = func() error { <-release; return closeResource() }
				}
				return r, err
			}
			set, err := client.NewPreparationSet(o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { unblock.Do(func() { close(release) }); set.Close(context.Background()) }()
			if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "target-1", 0), v); err != nil {
				t.Fatal(err)
			}
			c, _ := preparationSetConnection(t, f, v)
			results := make(chan error, tc.limit)
			for i := range tc.limit {
				if i > 0 {
					if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, fmt.Sprintf("target-%d", i+1), uint64(i)), v); err != nil {
						t.Fatal(err)
					}
				}
				event := clientEvent(t, c)
				if tc.construction {
					go func() { _, err := set.Prepare(t.Context(), c, event); results <- err }()
					until := time.Now().Add(3 * time.Second)
					for builds.Load() < int64(i+1) && time.Now().Before(until) {
						time.Sleep(5 * time.Millisecond)
					}
					if builds.Load() != int64(i+1) {
						t.Fatal("construction did not start")
					}
				} else {
					if _, err := set.Prepare(t.Context(), c, event); err == nil {
						t.Fatal("unhealthy candidate accepted")
					}
				}
			}
			if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "beyond-limit", uint64(tc.limit)), v); err != nil {
				t.Fatal(err)
			}
			if _, err := set.Prepare(t.Context(), c, clientEvent(t, c)); !errors.Is(err, client.ErrBackpressure) {
				t.Fatal("resource bound did not apply", err)
			}
			if builds.Load() != int64(tc.limit) || closed.Load() != 0 {
				t.Fatal("unbounded replacement or premature cleanup", builds.Load(), closed.Load())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			err = set.Close(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("close failed to retain stuck work", err)
			}
			unblock.Do(func() { close(release) })
			if tc.construction {
				for range tc.limit {
					select {
					case err := <-results:
						if !errors.Is(err, client.ErrRollback) {
							t.Fatal("old work not fenced", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("construction did not stop")
					}
				}
			}
			if err := set.Close(t.Context()); err != nil || closed.Load() != int64(tc.limit) {
				t.Fatal("bounded work did not fully drain", err, closed.Load())
			}
			if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; count != "0" {
				t.Fatal("stuck/failed work reported preparation", count)
			}
		})
	}
}

func TestPreparationSetKnownRemoteConflictDoesNotRemainSelected(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	c, _ := preparationSetConnection(t, f, v)
	event := clientEvent(t, c)
	var builds, closed atomic.Int64
	o := preparationSetOptions(f, v, body, &builds, &closed)
	initial, err := c.PrepareRelease(t.Context(), event, o)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Workload.Close(t.Context())
	// A different local attempt must not pretend it owns this immutable command.
	time.Sleep(2 * time.Millisecond)
	set, err := client.NewPreparationSet(o)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(t.Context())
	_, err = set.Prepare(t.Context(), c, event)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if _, err := set.BuildRuntime(t.Context(), client.RuntimeSpec{}); !errors.Is(err, client.ErrRuntimeNotReady) {
		t.Fatal("conflicting candidate remained selected", err)
	}
	_, err = set.Prepare(t.Context(), c, event)
	expectCode(t, err, agentv1.ErrorCode_ERROR_CODE_CONFLICT)
	if builds.Load() != 2 {
		t.Fatal("terminal command was reconstructed")
	}
}

func TestPreparationSetCleanupFailureStopsNewConstruction(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	c, _ := preparationSetConnection(t, f, v)
	event := clientEvent(t, c)
	var builds, closed atomic.Int64
	o := preparationSetOptions(f, v, body, &builds, &closed)
	original := o.Builder
	cleanupErr := errors.New("application cleanup failed")
	o.Builder = func(ctx context.Context, spec client.PreparationSpec) (client.PreparationResources, error) {
		resources, err := original(ctx, spec)
		resources.CheckHealth = func(context.Context) error { return errors.New("unhealthy") }
		resources.Close = func() error { closed.Add(1); return cleanupErr }
		return resources, err
	}
	set, err := client.NewPreparationSet(o)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(t.Context())
	result, err := set.Prepare(t.Context(), c, event)
	if err == nil || result.Workload == nil {
		t.Fatal("unhealthy construction accepted", err)
	}
	if err := result.Workload.Close(t.Context()); !errors.Is(err, cleanupErr) {
		t.Fatal(err)
	}
	// The selector's cleanup watcher must latch the error before accepting work.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = set.BuildRuntime(t.Context(), client.RuntimeSpec{})
		if errors.Is(err, client.ErrRuntimeResources) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup failure not latched", err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := set.Prepare(t.Context(), c, event); !errors.Is(err, client.ErrRuntimeResources) {
		t.Fatal(err)
	}
	if err := set.Close(t.Context()); !errors.Is(err, cleanupErr) {
		t.Fatal("cleanup error lost", err)
	}
	if builds.Load() != 1 || closed.Load() != 1 {
		t.Fatal("failed cleanup permitted new allocation", builds.Load(), closed.Load())
	}
}

func TestPreparationSetTrustUpdateFencesInflightBuilder(t *testing.T) {
	parallelAgentFixture(t)
	f, _, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "first", 0), v); err != nil {
		t.Fatal(err)
	}
	c, _ := preparationSetConnection(t, f, v)
	event := clientEvent(t, c)
	var builds, closed, entered atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	o := preparationSetOptions(f, v, body, &builds, &closed)
	construct := o.Builder
	o.Builder = func(ctx context.Context, spec client.PreparationSpec) (client.PreparationResources, error) {
		if entered.Add(1) == 1 {
			close(started)
			<-release
		}
		return construct(ctx, spec)
	}
	set, err := client.NewPreparationSet(o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock.Do(func() { close(release) }); set.Close(context.Background()) }()
	result := make(chan error, 1)
	go func() { _, err := set.Prepare(t.Context(), c, event); result <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("builder not started")
	}
	if err := set.UpdateTrustRoots(v); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-result:
		if !errors.Is(err, client.ErrRollback) {
			t.Fatal("old trust snapshot published", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("builder did not finish")
	}
	if count := pgtest.Query(t, f.dsn, `SELECT count(*) FROM agent_prepared_evidence`)[0][0]; count != "0" {
		t.Fatal("old trust construction reported evidence", count)
	}
	if _, err := set.Prepare(t.Context(), c, event); err != nil {
		t.Fatal("fresh trust attempt failed", err)
	}
	if builds.Load() != 2 {
		t.Fatal("fresh trust attempt reused old candidate")
	}
}
