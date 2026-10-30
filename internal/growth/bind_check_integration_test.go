package growth_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/growth"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

// All writes here set up local fixtures, outside the reader's transport.
func TestBindCheckReadyWithRealHandler(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("bind-ready", "Binding fixture")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "test", "Test")
	if err != nil {
		t.Fatal(err)
	}
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "onboarding", Type: model.FlagBool,
		Fields: []string{"user_id"}, Bucketing: &model.Bucketing{Algorithm: "grayling-fnv1a-10000-v1", Unit: "user_id", Salt: "fixture"},
		Variations: map[string]any{"control": false, "treatment": true}, DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{{Percentage: map[string]float64{"control": 50, "treatment": 50}}},
	}
	if err := store.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: flag.Key, Revision: 7, Content: flag}); err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: flag.Key, FlagRevision: 7, Epoch: 1, Unit: "user_id",
		Variants: []string{"control", "treatment"}, Proportions: map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "conversion", Type: "proportion", Direction: "increase"}, Alpha: 0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	exp, err = store.TransitionExperiment(exp.ID, model.ExperimentReviewed, "fixture operator")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "binding viewer", platform.RoleViewer, ws.ID, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewServer(store, nil, api.Options{}).Handler()
	var reads, writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
		} else {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	reader, err := growth.NewReader(growth.Config{BaseURL: srv.URL, WorkspaceID: ws.ID, EnvironmentID: env.ID, BearerToken: viewer.Secret})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	m := growth.RolloutManifest{WorkspaceID: ws.ID, EnvironmentID: env.ID, ExperimentID: exp.ID, Epoch: exp.Epoch, RegistrationVersion: exp.RegistrationVersion}
	o, err := reader.BindCheck(t.Context(), m)
	if err != nil || o == nil || !o.Ready || len(o.Checks) != 9 || len(o.Blockers) != 0 || o.RegisteredRevision != 7 || o.CurrentRevision != 7 || o.PublicationAuthorized {
		t.Fatalf("ready: %+v %v", o, err)
	}
	// A newer flag is a prerequisite blocker, not permission to refresh registration.
	if err := store.AppendFlagRevision(&platform.FlagRevision{WorkspaceID: ws.ID, EnvironmentID: env.ID, Key: flag.Key, Revision: 8, Content: flag}); err != nil {
		t.Fatal(err)
	}
	o, err = reader.BindCheck(t.Context(), m)
	if err != nil || o == nil || o.Ready || len(o.Blockers) != 1 || o.Blockers[0].Code != "registered_revision" || o.CurrentRevision != 8 || o.PublicationAuthorized {
		t.Fatalf("drift: %+v %v", o, err)
	}
	after, err := store.GetExperiment(exp.ID)
	if err != nil || after.FlagRevision != 7 || after.State != model.ExperimentReviewed || !after.UpdatedAt.Equal(exp.UpdatedAt) {
		t.Fatalf("reader changed experiment: %+v %v", after, err)
	}
	if reads.Load() != 6 || writes.Load() != 0 {
		t.Fatalf("requests: %d reads %d writes", reads.Load(), writes.Load())
	}
}
