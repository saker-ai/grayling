package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestExperimentReportExportScopeAndFormats(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("export", "Export")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "production", "Production")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := store.CreateExperiment(&platform.Experiment{
		WorkspaceID: ws.ID, EnvironmentID: env.ID, FlagKey: "onboarding", Unit: "user_id",
		Epoch: 1, Variants: []string{"control", "treatment"},
		Proportions:   map[string]float64{"control": 50, "treatment": 50},
		PrimaryMetric: platform.MetricSpec{Key: "complete", Type: "proportion", Direction: "increase"},
		Alpha:         0.05, AnalysisWindow: 24 * time.Hour, MinSamplePerArm: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("other-export", "Other")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleViewer, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/experiments/" + exp.ID + "/report/export"
	call := func(query, secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+query, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	if resp := call("?format=csv", outsider.Secret); resp.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace status = %d: %s", resp.Code, resp.Body.String())
	}
	if resp := call("?format=xlsx", viewer.Secret); resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid format status = %d", resp.Code)
	}
	jsonResp := call("?format=json", viewer.Secret)
	if jsonResp.Code != http.StatusOK {
		t.Fatalf("JSON status = %d: %s", jsonResp.Code, jsonResp.Body.String())
	}
	var exported struct {
		WorkspaceID   string `json:"workspaceId"`
		EnvironmentID string `json:"environmentId"`
		Report        struct {
			ExperimentID    string `json:"experimentId"`
			Epoch           int64  `json:"epoch"`
			EvidenceVersion string `json:"evidenceVersion"`
		} `json:"report"`
	}
	if err := json.Unmarshal(jsonResp.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.WorkspaceID != ws.ID || exported.EnvironmentID != env.ID ||
		exported.Report.ExperimentID != exp.ID || exported.Report.Epoch != exp.Epoch || exported.Report.EvidenceVersion == "" {
		t.Fatalf("wrong export scope: %+v", exported)
	}
	csvResp := call("?format=csv", viewer.Secret)
	if csvResp.Code != http.StatusOK {
		t.Fatalf("CSV status = %d: %s", csvResp.Code, csvResp.Body.String())
	}
	if got := csvResp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache policy = %q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(csvResp.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || len(rows[0]) != len(rows[1]) || len(rows[0]) != len(rows[2]) {
		t.Fatalf("CSV shape = %v", rows)
	}
	if rows[0][11] != "evidence_version" || rows[1][11] == "" ||
		rows[1][0] != ws.ID || rows[1][1] != env.ID || rows[1][2] != exp.ID ||
		rows[1][20] != "control" || rows[2][20] != "treatment" {
		t.Fatalf("CSV scope/variants = %v", rows)
	}
}

func TestReportCSVCellEscapesSpreadsheetFormulas(t *testing.T) {
	for _, input := range []string{"=1+1", " +cmd", "\t@function", "-SUM(A1)", "@HYPERLINK(1)"} {
		if got := csvCell(input); !strings.HasPrefix(got, "'") {
			t.Errorf("csvCell(%q) = %q", input, got)
		}
	}
}
