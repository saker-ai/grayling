package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestMetricDefinitionApprovalAPI(t *testing.T) {
	store, err := platform.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("metrics", "Metrics")
	if err != nil {
		t.Fatal(err)
	}
	analyst, err := store.CreateToken(platform.TokenAdmin, "analyst", platform.RoleAnalyst, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "owner", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	call := func(method, path, secret string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	base := "/api/v1/admin/workspaces/metrics/metrics"
	definition := platform.MetricDefinition{
		Key: "activation.complete", DisplayName: "Activation completion", Owner: "growth-team",
		Source: "collect", EventKeys: []string{"activation_complete"}, DedupUnit: "user",
		AttributionWindow: 7 * 24 * time.Hour,
		Spec:              platform.MetricSpec{Type: "proportion", Direction: "increase"},
	}
	createdResp := call(http.MethodPost, base, analyst.Secret, definition)
	if createdResp.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", createdResp.Code, createdResp.Body.String())
	}
	var created platform.MetricDefinition
	if err := json.Unmarshal(createdResp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("%s/%s/versions/%d", base, created.Key, created.Version)
	if got := call(http.MethodGet, path, owner.Secret, nil); got.Code != 200 {
		t.Fatalf("get: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodGet, base+"?limit=1", owner.Secret, nil); got.Code != 200 {
		t.Fatalf("list: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, path+"/publish", owner.Secret, map[string]string{"approvalId": "missing"}); got.Code != 404 {
		t.Fatalf("unapproved publish: %d %s", got.Code, got.Body.String())
	}
	submitted := call(http.MethodPost, path+"/submit", analyst.Secret, map[string]string{"reason": "pilot activation metric"})
	if submitted.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", submitted.Code, submitted.Body.String())
	}
	var approval platform.Approval
	if err := json.Unmarshal(submitted.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	approvePath := "/api/v1/admin/workspaces/metrics/approvals/" + approval.ID + "/approve"
	if got := call(http.MethodPost, approvePath, analyst.Secret, map[string]string{"note": "self"}); got.Code != 403 {
		t.Fatalf("self approval: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, approvePath, owner.Secret, map[string]string{"note": "verified"}); got.Code != 200 {
		t.Fatalf("approve: %d %s", got.Code, got.Body.String())
	}
	published := call(http.MethodPost, path+"/publish", owner.Secret, map[string]string{"approvalId": approval.ID})
	if published.Code != 200 {
		t.Fatalf("publish: %d %s", published.Code, published.Body.String())
	}
	if got := call(http.MethodPost, path+"/publish", owner.Secret, map[string]string{"approvalId": approval.ID}); got.Code != 409 {
		t.Fatalf("replayed publish: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, path+"/deprecate", analyst.Secret, map[string]string{"reason": "unused"}); got.Code != 403 {
		t.Fatalf("analyst deprecate: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, path+"/deprecate", owner.Secret, map[string]string{"reason": "replaced"}); got.Code != 200 {
		t.Fatalf("deprecate: %d %s", got.Code, got.Body.String())
	}
}
