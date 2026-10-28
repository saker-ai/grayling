package growth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/platform"
)

// This exercises the real authorization, metric serialization and report
// builder over HTTP, without a database service, write route or AI provider.
func TestReaderWithRealAPIHandler(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("growth", "Growth")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	otherEnv, err := store.CreateEnvironment(ws.ID, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("other-growth", "Other")
	if err != nil {
		t.Fatal(err)
	}
	def, err := store.CreateMetricDefinition(&platform.MetricDefinition{
		WorkspaceID: ws.ID, Key: "activation", DisplayName: "Activation", Owner: "growth", Source: "collect", EventKeys: []string{"complete"}, DedupUnit: "user", AttributionWindow: 24 * time.Hour,
		Spec: platform.MetricSpec{Type: "proportion", Direction: "increase"}, CreatedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err = store.PublishMetricDefinition(ws.ID, platform.MetricRef{Key: def.Key, Version: def.Version}, "test")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "onboarding", Unit: "user_id", Epoch: 1, ExposureMode: "onRender", ExposureTrigger: "after fixture display", IntegrationOwner: "test", AnalysisMethod: "two-proportion-z",
		Variants: []string{"control", "treatment"}, Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetricRef: &platform.MetricRef{Key: def.Key, Version: def.Version, DefinitionHash: def.Hash},
		Alpha:            0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "growth-viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "other-viewer", platform.RoleViewer, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewServer(store, nil, api.Options{}).Handler()
	var reads, writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		} else {
			reads.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	newReader := func(workspace, environment, secret string) *growth.Reader {
		t.Helper()
		reader, err := growth.NewReader(growth.Config{BaseURL: srv.URL, WorkspaceID: workspace, EnvironmentID: environment, BearerToken: secret})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reader.Close)
		return reader
	}
	query := growth.Query{Metrics: []growth.MetricRef{{Key: def.Key, Version: def.Version, DefinitionHash: def.Hash}}, Experiments: []growth.ExperimentRef{{ID: exp.ID, Epoch: exp.Epoch}}}
	reader := newReader(ws.ID, env.ID, viewer.Secret)
	got, err := reader.Read(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != growth.Hold || len(got.Evidence) != 1 || len(got.Metrics) != 1 {
		t.Fatalf("local incomplete evidence = %+v", got)
	}
	p := got.Evidence[0].Report
	if p.Window.Recomputable || p.Decision.Verdict != "hold" || p.EvidenceVersion == "" || p.PrimaryMetricRef.DefinitionHash != def.Hash || p.Diagnostics == nil {
		t.Fatalf("lost real server semantics: %+v", p)
	}
	query.Experiments[0].EvidenceVersion = p.EvidenceVersion
	if _, err := reader.Read(context.Background(), query); err != nil {
		t.Fatalf("pinned snapshot: %v", err)
	}
	if _, err := newReader(ws.ID, otherEnv.ID, viewer.Secret).Read(context.Background(), query); !errors.Is(err, growth.ErrMismatch) {
		t.Fatalf("cross environment: %v", err)
	}
	if _, err := newReader(other.ID, env.ID, viewer.Secret).Read(context.Background(), query); !errors.Is(err, growth.ErrAuthorization) {
		t.Fatalf("cross workspace: %v", err)
	}
	if _, err := newReader(ws.ID, env.ID, outsider.Secret).Read(context.Background(), query); !errors.Is(err, growth.ErrAuthorization) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := newReader(ws.ID, env.ID, "invalid-secret").Read(context.Background(), query); !errors.Is(err, growth.ErrAuthorization) {
		t.Fatalf("invalid credential: %v", err)
	}
	if _, err := newReader(other.ID, env.ID, outsider.Secret).Read(context.Background(), query); !errors.Is(err, growth.ErrAuthorization) {
		t.Fatalf("report access after missing metric: %v", err)
	}
	query.Experiments[0].ID = "missing-experiment"
	got, err = reader.Read(context.Background(), query)
	if err != nil || got.Status != growth.NeedsData || len(got.MissingPaths) != 1 {
		t.Fatalf("missing report: %+v, %v", got, err)
	}
	if writes.Load() != 0 || reads.Load() < 10 {
		t.Fatalf("requests: %d reads %d writes", reads.Load(), writes.Load())
	}
}
