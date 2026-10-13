package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/evaluation"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestRemoteOnRenderExposureRequiresRecordedAssignment(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("exposure", "Exposure")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "production", "Production")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "onboarding",
		Unit: "user_id", Epoch: 1, ExposureMode: "onRender",
		ExposureTrigger: "after display", IntegrationOwner: "test",
		Variants:      []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "complete", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect, err := store.CreateToken(platform.TokenCollect, "collect", "", ws.ID, env.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	snapshot := &model.Snapshot{Flags: map[string]*model.Flag{
		"onboarding": {Key: "onboarding", Version: "1", Bucketing: &model.Bucketing{Unit: "user_id"}},
	}}
	result := model.Result{FlagKey: "onboarding", DecisionID: "dec-test", Experiment: &model.Exposure{
		ExperimentID: exp.ID, Epoch: exp.Epoch, Variant: "treatment", Source: "split", ExposureMode: "onRender",
	}}
	token, unit, _, err := server.recordRemoteAssignment(env, snapshot, evalRequest{
		Context: model.EvaluationContext{Attributes: map[string]any{"user_id": "user-1"}},
	}, result)
	if err != nil || unit != "user-1" {
		t.Fatalf("assignment: token=%q unit=%q err=%v", token, unit, err)
	}
	if got, _ := store.EventMetricTotals(ws.ID, env.ID); got != 0 {
		t.Fatalf("assignment counted as exposure: %d", got)
	}
	call := func(ev collectEventDTO) map[string]any {
		t.Helper()
		payload, err := json.Marshal(collectRequest{Events: []collectEventDTO{ev}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/collect", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+collect.Secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("collect HTTP %d: %s", resp.Code, resp.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	event := collectEventDTO{EventID: "display-1", DecisionID: "dec-test", ExposureToken: token,
		Type: "exposure", ExperimentID: exp.ID, Epoch: exp.Epoch, FlagKey: "onboarding",
		FlagRevision: 1, Variant: "treatment", Unit: unit, Source: "split", ClientTime: time.Now().UTC()}
	conversion := event
	conversion.EventID, conversion.Type, conversion.ExposureToken = "convert-1", "conversion", ""
	conversion.MetricKey, conversion.Value = "complete", 1
	if got := call(conversion); got["rejected"] != float64(1) {
		t.Fatalf("conversion before exposure accepted: %+v", got)
	} else if results := got["results"].([]any); results[0].(map[string]any)["code"] != "missing_exposure" {
		t.Fatalf("missing exposure result: %+v", got)
	}
	bad := event
	bad.Unit = "someone-else"
	if got := call(bad); got["rejected"] != float64(1) {
		t.Fatalf("mismatched unit accepted: %+v", got)
	} else if results := got["results"].([]any); results[0].(map[string]any)["code"] != "assignment_mismatch" {
		t.Fatalf("assignment result: %+v", got)
	}
	if got := call(event); got["accepted"] != float64(1) {
		t.Fatalf("valid exposure rejected: %+v", got)
	}
	if got := call(event); got["duplicates"] != float64(1) {
		t.Fatalf("retry not idempotent: %+v", got)
	} else if results := got["results"].([]any); results[0].(map[string]any)["code"] != "duplicate_event" {
		t.Fatalf("duplicate result: %+v", got)
	}
	if got := call(conversion); got["accepted"] != float64(1) {
		t.Fatalf("conversion after exposure rejected: %+v", got)
	}
	if got, _ := store.EventMetricTotals(ws.ID, env.ID); got != 1 {
		t.Fatalf("expected one exposure, got %d", got)
	}
	counts, err := store.CollectionCounts(exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["missing_exposure"] != 1 || counts["assignment_mismatch"] != 1 ||
		counts["duplicate_event"] != 1 {
		t.Fatalf("collection diagnostics: %+v", counts)
	}
}

type exposureConfigStub struct {
	configstore.Store
	snapshot *model.Snapshot
}

func (s exposureConfigStub) GetManifestContext(context.Context, string) (*model.Manifest, error) {
	return &model.Manifest{Revision: s.snapshot.ManifestRevision}, nil
}

func (s exposureConfigStub) LoadSnapshotContext(context.Context, string) (*model.Snapshot, error) {
	return s.snapshot, nil
}

func TestLocalOnRenderAssignmentAndExposureBatch(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "production", "Production")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "onboarding", Unit: "user_id",
		Epoch: 1, ExposureMode: "onRender", ExposureTrigger: "after display", IntegrationOwner: "test",
		Variants:      []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "complete", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	collect, err := store.CreateToken(platform.TokenCollect, "collect", "", ws.ID, env.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	flag := &model.Flag{SchemaVersion: model.SchemaVersion, Key: "onboarding", Version: "1",
		Type: model.FlagBool, Bucketing: &model.Bucketing{Algorithm: "golab-fnv1a-10000-v1", Unit: "user_id", Salt: "test"},
		Fields: []string{"user_id"}, Variations: map[string]any{"control": false, "treatment": true},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting:   []model.Rule{{Percentage: map[string]float64{"control": 50, "treatment": 50}}},
		Experiment:  &model.ExperimentBinding{ID: exp.ID, Epoch: 1, ExposureMode: "onRender", State: model.ExperimentRunning},
	}
	snapshot := &model.Snapshot{SchemaVersion: model.SchemaVersion, Workspace: ws.ID, Environment: env.ID, ManifestRevision: 1,
		GeneratedAt: time.Now().UTC(), Flags: map[string]*model.Flag{"onboarding": flag}}
	evaluator, err := evaluation.NewEvaluator(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	ctx := model.EvaluationContext{Attributes: map[string]any{"user_id": "user-1"}}
	decision := evaluator.Evaluate("onboarding", ctx)
	if !decision.TrackEvents || decision.Experiment == nil {
		t.Fatalf("not enrolled: %+v", decision)
	}
	server := NewServer(store, exposureConfigStub{snapshot: snapshot}, Options{})
	base := collectEventDTO{DecisionID: "dec-local", ExperimentID: exp.ID, Epoch: 1,
		FlagKey: "onboarding", FlagRevision: 1, Variant: decision.Experiment.Variant,
		Unit: "user-1", Source: decision.Experiment.Source, ClientTime: time.Now().UTC()}
	assignment := base
	assignment.EventID, assignment.Type, assignment.Context = "asg-dec-local", "assignment", &ctx
	exposure := base
	exposure.EventID, exposure.Type = "exp-dec-local", "exposure"
	missingUnit := base
	missingUnit.EventID, missingUnit.Type, missingUnit.Unit = "missing-unit", "conversion", ""
	payload, err := json.Marshal(collectRequest{Events: []collectEventDTO{assignment, missingUnit, exposure}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/collect", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+collect.Secret)
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != 200 {
		t.Fatalf("HTTP %d: %s", resp.Code, resp.Body.String())
	}
	var got platform.IngestResult
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 2 || got.Rejected != 1 {
		t.Fatalf("batch: %+v", got)
	}
	if len(got.Results) != 3 || got.Results[0].Status != "accepted" ||
		got.Results[1].Code != "missing_unit" || got.Results[2].Status != "accepted" {
		t.Fatalf("ordered results: %+v", got.Results)
	}
	if total, _ := store.EventMetricTotals(ws.ID, env.ID); total != 1 {
		t.Fatalf("exposure total %d", total)
	}
}
