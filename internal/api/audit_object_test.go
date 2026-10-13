package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestAuditExactObjectFilterAndWorkspaceScope(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("audit-main", "Main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("audit-other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ workspace, object string }{
		{ws.ID, "exp-8"}, {ws.ID, "exp-80"}, {other.ID, "exp-8"}, {ws.ID, "exp-8"},
	} {
		if err := store.Audit("operator", platform.RoleOwner, "manual.advance", entry.workspace, "prod", entry.object, ""); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(store, nil, Options{})
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token.Secret)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	path := "/api/v1/admin/workspaces/" + ws.ID + "/audit?object=" + url.QueryEscape("exp-8") + "&limit=10"
	response := call(path)
	if response.Code != http.StatusOK {
		t.Fatalf("filtered audit: %d %s", response.Code, response.Body.String())
	}
	var entries []platform.AuditEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Object != "exp-8" || entries[1].Object != "exp-8" {
		t.Fatalf("exact object filter: %+v", entries)
	}
	if got := call("/api/v1/admin/workspaces/" + other.ID + "/audit?object=exp-8"); got.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace audit: %d %s", got.Code, got.Body.String())
	}
	if got := call("/api/v1/admin/workspaces/" + ws.ID + "/audit?object=" + strings.Repeat("x", 129)); got.Code != http.StatusBadRequest {
		t.Fatalf("oversized object: %d %s", got.Code, got.Body.String())
	}
}
