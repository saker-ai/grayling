//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
	"google.golang.org/protobuf/proto"
)

func preparationDriverFixture(t *testing.T, lifetime ...time.Duration) (*registrationFixture, *control.Publisher, *client.Verifier, []byte) {
	t.Helper()
	f := newRegistrationFixtureWithPermissions(t, 2, control.ReleaseReport)
	registerInstance(t, f, "register")
	confirmPreparedNaming(t, f)
	p, v := publisher(t, f.a)
	if len(lifetime) > 0 {
		var err error
		p, err = control.NewPublisher(f.a, "policy-test-key", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x66}, 32)), lifetime[0])
		if err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("prepared application handler")
	hash := sha256.Sum256(body)
	q := bundlePairRequest(t)
	q.Document.Builds[0].ID = "build-a"
	q.Document.ValueBuilds["backend"] = "build-a"
	q.Document.Builds[0].Resources = []client.BundleResource{{Key: "handler", SHA256: hex.EncodeToString(hash[:])}}
	pair, err := p.PublishBundle(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	release := releaseRequest(pair)
	release.Capacity = control.ReleaseCapacity{Profile: "demo-v1", RequiredZones: []string{"az-a"}, MinimumPerBuildZone: 1}
	if _, err := p.StartRelease(t.Context(), release, v); err != nil {
		t.Fatal(err)
	}
	return f, p, v, body
}

func TestPreparationDriverOwnedResourcesReconnectAndActualRuntime(t *testing.T) {
	f, p, v, body := preparationDriverFixture(t)
	intent, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "prepare-resources", 0), v)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare", "release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	var builds, instances, closed atomic.Int64
	var actualSpec client.RuntimeSpec
	options := client.PreparationOptions{Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, Verifier: v, Now: time.Now, Builder: func(ctx context.Context, s client.PreparationSpec) (client.PreparationResources, error) {
		builds.Add(1)
		if s.Build.ID != "build-a" || s.Release.Generation != 1 || s.Policy == nil {
			t.Error("wrong construction inputs")
		}
		loaded := bytes.Clone(body)
		return client.PreparationResources{Artifacts: map[string][]byte{"handler": loaded}, CheckHealth: func(context.Context) error {
			if string(loaded) != "prepared application handler" {
				return errors.New("application health failed")
			}
			return nil
		}, Instantiate: func(_ context.Context, spec client.RuntimeSpec) (client.RuntimeResources, error) {
			instances.Add(1)
			actualSpec = spec
			return client.RuntimeResources{Artifacts: map[string][]byte{"handler": loaded}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				meta, ok := client.RuntimeFromContext(r.Context())
				if !ok || meta != spec.Metadata {
					t.Error("prepared handler mixed runtime metadata")
				}
				_, _ = w.Write(loaded)
			}), Close: func() error { return nil }}, nil
		}, Close: func() error { closed.Add(1); return nil }}, nil
	}}
	result, err := c.PrepareRelease(t.Context(), event, options)
	if err != nil || result.Prepared == nil || result.Receipt == nil || result.Receipt.EvidenceRevision != 1 || result.CommandID != intent.CommandID || builds.Load() != 1 || instances.Load() != 0 {
		t.Fatal("actual preparation failed", err)
	}
	defer result.Workload.Close(t.Context())
	otherKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x31}, 32))
	now := time.Now()
	untrusted, err := client.NewVerifier(map[string]client.TrustKey{"other": {PublicKey: otherKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Workload.UpdateTrustRoots(untrusted); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Workload.PreparedReport(t.Context()); err == nil {
		t.Fatal("retired root continued preparing")
	}
	if err := result.Workload.UpdateTrustRoots(v); err != nil {
		t.Fatal(err)
	}
	c.Close()
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	ack, err := result.Workload.RetryPrepared(t.Context(), successor)
	if err != nil || !proto.Equal(result.Receipt, ack) || builds.Load() != 1 {
		t.Fatal("reconnect rebuilt or renewed evidence", err)
	}
	runtime, err := client.InitializeRuntime(t.Context(), client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{54}, 32), Verifier: v, Now: time.Now, Builder: result.Workload.BuildRuntime})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	if lease, err := runtime.Capture(); !errors.Is(err, client.ErrRuntimeNotReady) {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("Prepared became active", err)
	}
	if _, err := f.a.EvaluatePreparation(t.Context(), gateRequest(f, "gate", 1), v); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartActivation(t.Context(), activationRequest(f), v); err != nil {
		t.Fatal(err)
	}
	active := clientEvent(t, successor)
	installed, err := successor.InstallActivation(t.Context(), active, runtime)
	if err != nil || installed.Receipt == nil || instances.Load() != 1 {
		t.Fatal("prepared workload not instantiated", err)
	}
	wrongBoot := actualSpec
	wrongBoot.Metadata.BootID = "another-boot"
	if resources, err := result.Workload.BuildRuntime(t.Context(), wrongBoot); err == nil {
		if resources.Close != nil {
			resources.Close()
		}
		t.Fatal("prepared candidate inherited another boot")
	}
	if instances.Load() != 1 {
		t.Fatal("mismatched boot instantiated handler")
	}
	lease, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	response := httptest.NewRecorder()
	if err := lease.ServeHTTP(response, httptest.NewRequest("GET", "https://service/business", nil)); err != nil {
		t.Fatal(err)
	}
	if response.Body.String() != string(body) {
		t.Fatal("actual resource-backed handler not used")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err = result.Workload.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || closed.Load() != 0 {
		t.Fatal("retired prepared resources closed under active view", err)
	}
	continued := httptest.NewRecorder()
	if err := lease.ServeHTTP(continued, httptest.NewRequest("GET", "https://service/draining", nil)); err != nil || continued.Body.String() != string(body) {
		t.Fatal("retiring preparation invalidated retained active handler", err)
	}
	lease.Release()
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := result.Workload.Close(t.Context()); err != nil || closed.Load() != 1 {
		t.Fatal("prepared resources not closed exactly once", err)
	}
}

func TestPreparationDriverFailuresNeverReportPrepared(t *testing.T) {
	f, _, v, body := preparationDriverFixture(t)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "prepare", 0), v); err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	var calls, closed atomic.Int64
	healthy := true
	wrongBytes := false
	options := client.PreparationOptions{Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, Verifier: v, Now: time.Now, Builder: func(context.Context, client.PreparationSpec) (client.PreparationResources, error) {
		calls.Add(1)
		artifact := bytes.Clone(body)
		if wrongBytes {
			artifact = []byte("corrupt")
		}
		return client.PreparationResources{Artifacts: map[string][]byte{"handler": artifact}, CheckHealth: func(context.Context) error {
			if !healthy {
				return errors.New("health failed")
			}
			return nil
		}, Instantiate: func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
			t.Error("failure instantiated active resources")
			return client.RuntimeResources{}, nil
		}, Close: func() error { closed.Add(1); return nil }}, nil
	}}
	bad := proto.Clone(event).(*agentv1.Envelope)
	bad.OperationId = ""
	if result, err := c.PrepareRelease(t.Context(), bad, options); err == nil || result.Prepared != nil || calls.Load() != 0 {
		t.Fatal("bad frame built resources")
	}
	mismatch := options
	mismatch.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	result, err := c.PrepareRelease(t.Context(), event, mismatch)
	if err == nil || result.Prepared != nil || calls.Load() != 0 {
		t.Fatal("wrong image built resources")
	}
	if result.Workload != nil {
		if err := result.Workload.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, failure := range []string{"bytes", "health"} {
		wrongBytes = failure == "bytes"
		healthy = failure != "health"
		result, err := c.PrepareRelease(t.Context(), event, options)
		if err == nil || result.Prepared != nil || result.Receipt != nil || result.Workload == nil {
			t.Fatal("failed preparation was reported", failure, err)
		}
		if err := result.Workload.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || closed.Load() != 2 {
		t.Fatal("failed candidates leaked", calls.Load(), closed.Load())
	}
	if got := pgtest.Query(t, f.dsn, `SELECT (SELECT count(*) FROM agent_prepared_evidence)+(SELECT count(*) FROM agent_prepare_rejections)`)[0][0]; got != "0" {
		t.Fatal("failure consumed a terminal outcome", got)
	}
}

func TestPreparationDriverExpiryClosesCandidateWithoutActivation(t *testing.T) {
	f, _, v, body := preparationDriverFixture(t, 5*time.Second)
	if _, err := f.a.QueuePrepare(t.Context(), reprepareRequest(f, "short-preparation", 0), v); err != nil {
		t.Fatal(err)
	}
	server, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: server.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.prepare"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var closed atomic.Int64
	result, err := c.PrepareRelease(t.Context(), clientEvent(t, c), client.PreparationOptions{Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, Verifier: v, Now: time.Now, Builder: func(context.Context, client.PreparationSpec) (client.PreparationResources, error) {
		return client.PreparationResources{Artifacts: map[string][]byte{"handler": body}, CheckHealth: func(context.Context) error { return nil }, Instantiate: func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
			t.Error("expired candidate was activated")
			return client.RuntimeResources{}, nil
		}, Close: func() error { closed.Add(1); return nil }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Workload.Close(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("expired candidate retained owner resources")
	}
	if _, err := result.Workload.PreparedReport(t.Context()); err == nil {
		t.Fatal("expired candidate renewed its report")
	}
}
