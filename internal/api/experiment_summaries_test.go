package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestExperimentSummariesScopePaginationAndReport(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("summary", "Summary")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "production", "Production")
	if err != nil {
		t.Fatal(err)
	}
	otherEnv, err := store.CreateEnvironment(ws.ID, "staging", "Staging")
	if err != nil {
		t.Fatal(err)
	}
	create := func(environmentID, flag string) *platform.Experiment {
		t.Helper()
		exp, createErr := store.CreateExperiment(&platform.Experiment{
			WorkspaceID: ws.ID, EnvironmentID: environmentID, FlagKey: flag, Unit: "user_id",
			Epoch: 1, Variants: []string{"control", "treatment"},
			Proportions:   map[string]float64{"control": 50, "treatment": 50},
			PrimaryMetric: platform.MetricSpec{Key: "complete", Type: "proportion", Direction: "increase"},
			Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return exp
	}
	draft := create(env.ID, "draft-flag")
	running := create(env.ID, "running-flag")
	create(otherEnv.ID, "other-environment")
	if _, err := store.TransitionExperiment(running.ID, model.ExperimentReviewed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionExperiment(running.ID, model.ExperimentRunning, "test"); err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	otherWS, err := store.CreateWorkspace("outsider-summary", "Outsider")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleViewer, otherWS.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/" + ws.ID + "/environments/" + env.ID + "/experiment-summaries"
	call := func(query, secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+query, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if got := call("", outsider.Secret).Code; got != http.StatusForbidden {
		t.Fatalf("cross-workspace status = %d", got)
	}
	for _, query := range []string{"?limit=0", "?limit=51", "?offset=-1"} {
		if got := call(query, viewer.Secret).Code; got != http.StatusBadRequest {
			t.Errorf("query %q status = %d", query, got)
		}
	}
	var page struct {
		Items []struct {
			Experiment platform.Experiment `json:"experiment"`
			Report     struct {
				Status  string `json:"status"`
				Healthy bool   `json:"healthy"`
				Verdict string `json:"verdict"`
				Issue   string `json:"issue"`
			} `json:"report"`
		} `json:"items"`
		Total int `json:"total"`
	}
	response := call("?state=running&limit=1", viewer.Secret)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache policy = %q", got)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Experiment.ID != running.ID ||
		page.Items[0].Report.Status != "ready" || page.Items[0].Report.Healthy ||
		page.Items[0].Report.Verdict != "hold" || page.Items[0].Report.Issue == "" {
		t.Fatalf("running summary = %+v", page)
	}
	response = call("?q=draft-flag", viewer.Secret)
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Experiment.ID != draft.ID ||
		page.Items[0].Report.Status != "pending" {
		t.Fatalf("draft summary = %+v", page)
	}
	response = call("?limit=1&offset=1", viewer.Secret)
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("paginated summary = %+v", page)
	}
}
