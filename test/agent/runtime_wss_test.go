//go:build integration

package agent_test

import (
	"bytes"
	"context"
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
	"github.com/saker-ai/grayling/pkg/model"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeWSSActualInstallationAndAppliedReceipt(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	srv, _ := channelServerWithOptions(t, f.b, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	var builds atomic.Int64
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{42}, 32), Verifier: v, Now: time.Now}
	o.Builder = func(_ context.Context, s client.RuntimeSpec) (client.RuntimeResources, error) {
		builds.Add(1)
		if s.Metadata.Watermark.Generation != p.Release.Generation || s.Metadata.CommandID != event.OperationId || s.Bundle.Policy.Key != "routing" || len(s.Members) != 1 {
			t.Error("wrong actual construction inputs")
		}
		return client.RuntimeResources{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta, ok := client.RuntimeFromContext(r.Context())
			if !ok || meta != s.Metadata {
				t.Error("request mixed views")
			}
			w.Header().Set("X-Installed-Build", meta.BuildID)
			_, _ = w.Write([]byte("installed handler response"))
		}), Close: func() error { return nil }}, nil
	}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	// A valid signature cannot authorize mismatched command metadata.
	bad := proto.Clone(event).(*agentv1.Envelope)
	bad.GetActivate().PreparedRevision++
	if _, err = c.InstallActivation(t.Context(), bad, runtime); !errors.Is(err, client.ErrResource) {
		t.Fatal("metadata mismatch accepted", err)
	}
	if builds.Load() != 0 {
		t.Fatal("built mismatched activation")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agentv1.Envelope)
	}{
		{"wrong command", func(e *agentv1.Envelope) { e.OperationId = "other-command" }},
		{"wrong artifact", func(e *agentv1.Envelope) { e.GetActivate().View.Revision++ }},
		{"wrong bundle", func(e *agentv1.Envelope) { e.GetActivate().Release.BundleHash = strings.Repeat("b", 64) }},
		{"expired command", func(e *agentv1.Envelope) { e.DeadlineUnixMs = time.Now().Add(-time.Second).UnixMilli() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := proto.Clone(event).(*agentv1.Envelope)
			tc.mutate(invalid)
			out, err := c.InstallActivation(t.Context(), invalid, runtime)
			if err == nil || out.Applied != nil || out.Receipt != nil || builds.Load() != 0 {
				t.Fatal("invalid command built or acknowledged", out, err)
			}
		})
	}
	// Advance the mutable policy head to an incompatible payload. Installation
	// must fetch the activation's pinned revision rather than assembling latest.
	pub, _ := publisher(t, f.a)
	if _, err := pub.PublishPolicy(t.Context(), control.PublishPolicyRequest{Scope: f.g.Scope, PrincipalID: "approved-publisher", OperationID: "newer-unrelated-policy", Key: "routing", ExpectedRevision: 1, BundleHash: activationBundleHash(t, p.Release), Payload: policyPayload(t, true)}); err != nil {
		t.Fatal(err)
	}
	result, err := c.InstallActivation(t.Context(), event, runtime)
	if err != nil || result.Applied == nil || result.Receipt == nil || !proto.Equal(result.Applied, appliedReport(f, p)) || result.Receipt.ReportRevision != 1 {
		t.Fatal("actual installation receipt", result, err)
	}
	lease, err := runtime.Capture()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := lease.Select(model.EvaluationContext{}, 0)
	if err != nil || !decision.Local || decision.BuildID != "build-a" {
		t.Fatal(decision, err)
	}
	response := httptest.NewRecorder()
	if err = lease.ServeHTTP(response, httptest.NewRequest("GET", "https://checkout/business", nil)); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if response.Body.String() != "installed handler response" || response.Header().Get("X-Installed-Build") != "build-a" {
		t.Fatal("actual handler not applied", response)
	}
	replay, err := c.InstallActivation(t.Context(), event, runtime)
	if err != nil || !proto.Equal(result.Applied, replay.Applied) || !proto.Equal(result.Receipt, replay.Receipt) || builds.Load() != 1 {
		t.Fatal("retry changed installation", replay, err)
	}
	status, err := c.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || status.State != agentv1.ReleaseState_RELEASE_STATE_VERIFYING || len(status.Applied) != 1 || len(status.Rejected) != 0 {
		t.Fatal("actual handler did not remain VERIFYING pending probes", status, err)
	}
}

func TestRuntimeWSSFailedBuilderDoesNotConsumeOutcome(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	srv, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	var fail atomic.Bool
	fail.Store(true)
	buildErr := errors.New("actual handler initialization failed")
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{43}, 32), Verifier: v, Now: time.Now}
	o.Builder = func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
		if fail.Load() {
			return client.RuntimeResources{}, buildErr
		}
		return client.RuntimeResources{Handler: http.NotFoundHandler(), Close: func() error { return nil }}, nil
	}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	result, err := c.InstallActivation(t.Context(), event, runtime)
	if !errors.Is(err, buildErr) || result.Applied != nil || result.Receipt != nil {
		t.Fatal("failed handler installation acknowledged", result, err)
	}
	if _, err = runtime.Applied(); !errors.Is(err, client.ErrRuntimeNotReady) {
		t.Fatal("failed handler became ready", err)
	}
	status, err := c.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || len(status.Applied) != 0 || len(status.Rejected) != 0 || len(status.PendingSourceIds) != 1 || status.ReportRevision != 0 {
		t.Fatal("failure silently consumed terminal outcome", status, err)
	}
	fail.Store(false)
	result, err = c.InstallActivation(t.Context(), event, runtime)
	if err != nil || result.Applied == nil || result.Receipt == nil {
		t.Fatal("explicit retry failed", result, err)
	}
}

func TestRuntimeWSSInstalledViewSurvivesReportConnectionLoss(t *testing.T) {
	parallelAgentFixture(t)
	f, v, p := appliedFixture(t)
	srv, _ := channelServerWithOptions(t, f.a, control.ChannelOptions{ReleaseVerifier: v})
	opts := client.Options{URL: "wss" + strings.TrimPrefix(srv.URL, "https"), Token: f.secret, Hello: helloFor(f.g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"release.activate"}
	c, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	event := clientEvent(t, c)
	var builds atomic.Int64
	o := client.RuntimeOptions{Directory: filepath.Join(t.TempDir(), "runtime"), Identity: f.g.AgentID, Scope: f.g.Scope, PolicyKey: "routing", InstanceID: f.g.InstanceID, BootID: f.g.BootID, BuildID: "build-a", ImageDigest: f.request.Instance.ImageDigest, AuthenticationKey: bytes.Repeat([]byte{44}, 32), Verifier: v, Now: time.Now}
	o.Builder = func(context.Context, client.RuntimeSpec) (client.RuntimeResources, error) {
		builds.Add(1)
		_ = c.Close()
		return client.RuntimeResources{Handler: http.NotFoundHandler(), Close: func() error { return nil }}, nil
	}
	runtime, err := client.InitializeRuntime(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(t.Context())
	result, err := c.InstallActivation(t.Context(), event, runtime)
	if err == nil || result.Applied == nil || result.Receipt != nil {
		t.Fatal("connection loss obscured actual install", result, err)
	}
	current, err := runtime.Applied()
	if err != nil || !proto.Equal(current, result.Applied) {
		t.Fatal("network failure rolled back actual process view", current, err)
	}
	successor, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	retry := clientEvent(t, successor)
	if retry.OperationId != result.CommandID {
		t.Fatal("reconnect replaced command")
	}
	recovered, err := successor.InstallActivation(t.Context(), retry, runtime)
	if err != nil || recovered.Receipt == nil || !proto.Equal(recovered.Applied, result.Applied) || builds.Load() != 1 {
		t.Fatal("recovery rebuilt or changed applied view", recovered, err)
	}
	status, err := successor.ReleaseStatus(t.Context(), &agentv1.ReleaseStatusQuery{ReleaseId: p.Release.ReleaseID})
	if err != nil || len(status.Applied) != 1 || len(status.Rejected) != 0 || status.ReportRevision != 1 {
		t.Fatal("report recovery not exact", status, err)
	}
}
