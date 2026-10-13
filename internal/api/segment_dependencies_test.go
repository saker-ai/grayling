package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/pkg/model"
)

func TestSegmentDependenciesAPIRequiresScopedRead(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("audiences", "Audiences")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSegmentDraft(ws.ID, env.ID, &model.Segment{SchemaVersion: model.SchemaVersion,
		Key: "staff", Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: "CN"}}},
		platform.RiskNormal, "owner"); err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := store.CreateToken(platform.TokenAdmin, "outsider", platform.RoleViewer, other.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	path := "/api/v1/admin/workspaces/audiences/environments/prod/segments/staff/dependencies"
	call := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if response := call(outsider.Secret); response.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace read: %d %s", response.Code, response.Body.String())
	}
	response := call(viewer.Secret)
	if response.Code != http.StatusOK {
		t.Fatalf("viewer read: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		SegmentKey      string `json:"segmentKey"`
		SegmentRevision int64  `json:"segmentRevision"`
		Flags           []any  `json:"flags"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.SegmentKey != "staff" || body.SegmentRevision != 0 || body.Flags == nil {
		t.Fatalf("draft-only dependencies: %+v", body)
	}
	previewPath := "/api/v1/admin/workspaces/audiences/environments/prod/segments/staff/match-preview"
	preview := func(secret, key string) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"content": &model.Segment{SchemaVersion: model.SchemaVersion, Key: key,
				Constraints: []model.Constraint{{Field: "country", Op: model.OpEq, Value: "CN"}}},
			"context": model.EvaluationContext{Attributes: map[string]any{"country": "CN"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, previewPath, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if got := preview(outsider.Secret, "staff"); got.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace preview: %d %s", got.Code, got.Body.String())
	}
	if got := preview(viewer.Secret, "other"); got.Code != http.StatusBadRequest {
		t.Fatalf("mismatched preview key: %d %s", got.Code, got.Body.String())
	}
	got := preview(viewer.Secret, "staff")
	if got.Code != http.StatusOK {
		t.Fatalf("viewer preview: %d %s", got.Code, got.Body.String())
	}
	var result struct {
		DraftMatched      bool  `json:"draftMatched"`
		PublishedMatched  *bool `json:"publishedMatched"`
		PublishedRevision int64 `json:"publishedRevision"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.DraftMatched || result.PublishedMatched != nil || result.PublishedRevision != 0 {
		t.Fatalf("draft-only sample preview: %+v", result)
	}
	historyPath := "/api/v1/admin/workspaces/audiences/environments/prod/segments/staff/history"
	historyCall := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, historyPath, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if got := historyCall(outsider.Secret); got.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace history: %d %s", got.Code, got.Body.String())
	}
	historyResponse := historyCall(viewer.Secret)
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("viewer history: %d %s", historyResponse.Code, historyResponse.Body.String())
	}
	var history struct {
		CurrentRevision int64 `json:"currentRevision"`
		Revisions       []any `json:"revisions"`
	}
	if err := json.Unmarshal(historyResponse.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.CurrentRevision != 0 || history.Revisions == nil || len(history.Revisions) != 0 {
		t.Fatalf("draft-only history: %+v", history)
	}
}
