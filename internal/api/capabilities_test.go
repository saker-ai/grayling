package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godeps/grayling/internal/admin"
	"github.com/godeps/grayling/internal/platform"
)

func TestWorkspaceCapabilitiesAndRBAC(t *testing.T) {
	store, err := platform.NewStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateWorkspace("other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	env, err := store.CreateEnvironment(ws.ID, "prod", "Production")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	call := func(path, secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	cases := []struct {
		name    string
		role    platform.Role
		kind    platform.TokenKind
		scope   string
		path    string
		status  int
		allowed []admin.Permission
	}{
		{"global owner", platform.RoleOwner, platform.TokenAdmin, "", "acme", 200, []admin.Permission{admin.PermWorkspaceAdmin, admin.PermTokenManage, admin.PermFlagRead, admin.PermFlagWrite, admin.PermFlagApprove, admin.PermFlagPublish, admin.PermExperimentWrite, admin.PermReportRead, admin.PermAIWrite, admin.PermAIContentRead, admin.PermQuotaManage}},
		{"workspace owner", platform.RoleOwner, platform.TokenAdmin, ws.ID, "acme", 200, []admin.Permission{admin.PermTokenManage, admin.PermFlagRead, admin.PermFlagWrite, admin.PermFlagApprove, admin.PermFlagPublish, admin.PermExperimentWrite, admin.PermReportRead, admin.PermAIWrite, admin.PermAIContentRead, admin.PermQuotaManage}},
		{"publisher", platform.RolePublisher, platform.TokenAdmin, ws.ID, "acme", 200, []admin.Permission{admin.PermFlagRead, admin.PermFlagWrite, admin.PermFlagApprove, admin.PermFlagPublish, admin.PermExperimentWrite, admin.PermReportRead, admin.PermAIWrite}},
		{"analyst", platform.RoleAnalyst, platform.TokenAdmin, ws.ID, "acme", 200, []admin.Permission{admin.PermFlagRead, admin.PermExperimentWrite, admin.PermReportRead, admin.PermAIWrite}},
		{"viewer", platform.RoleViewer, platform.TokenAdmin, ws.ID, "acme", 200, []admin.Permission{admin.PermFlagRead, admin.PermReportRead}},
		{"cross workspace", platform.RoleOwner, platform.TokenAdmin, ws.ID, "other", 403, nil},
		{"sdk credential", "", platform.TokenSDK, ws.ID, "acme", 403, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envID := ""
			if tc.kind == platform.TokenSDK {
				envID = env.ID
			}
			token, err := store.CreateToken(tc.kind, tc.name, tc.role, tc.scope, envID, "test")
			if err != nil {
				t.Fatal(err)
			}
			got := call("/api/v1/admin/workspaces/"+tc.path+"/capabilities?role=owner", token.Secret)
			if got.Code != tc.status {
				t.Fatalf("status %d want %d: %s", got.Code, tc.status, got.Body.String())
			}
			global := call("/api/v1/admin/capabilities", token.Secret)
			if tc.kind == platform.TokenAdmin {
				var flags map[string]bool
				if global.Code != 200 || json.Unmarshal(global.Body.Bytes(), &flags) != nil {
					t.Fatalf("global capability: %d %s", global.Code, global.Body.String())
				}
				if flags[string(admin.PermWorkspaceAdmin)] != (tc.scope == "" && tc.role == platform.RoleOwner) {
					t.Errorf("global workspace creation: %+v", flags)
				}
			} else if global.Code != 403 {
				t.Errorf("SDK global capability: %d want 403", global.Code)
			}
			if tc.status != 200 {
				return
			}
			var body struct {
				WorkspaceID string                    `json:"workspaceId"`
				Permissions map[admin.Permission]bool `json:"permissions"`
			}
			if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.WorkspaceID != ws.ID {
				t.Errorf("workspace %q want %q", body.WorkspaceID, ws.ID)
			}
			expected := make(map[admin.Permission]bool)
			for _, perm := range tc.allowed {
				expected[perm] = true
			}
			if len(body.Permissions) != 11 {
				t.Fatalf("permissions count %d: %+v", len(body.Permissions), body.Permissions)
			}
			for perm, allowed := range body.Permissions {
				if allowed != expected[perm] {
					t.Errorf("%s: got %v want %v", perm, allowed, expected[perm])
				}
			}
			if tc.name == "viewer" || tc.name == "publisher" || tc.name == "analyst" {
				// The read model cannot grant a write that the real token lacks.
				if resp := call("/api/v1/admin/workspaces/acme/tokens", token.Secret); resp.Code != 403 {
					t.Errorf("token list: %d want 403", resp.Code)
				}
			}
		})
	}
	if got := call("/api/v1/admin/workspaces/missing/capabilities", "invalid"); got.Code != 401 {
		t.Errorf("invalid token: %d", got.Code)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "revoke-owner", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "revoke-viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateToken(platform.TokenAdmin, "target", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	revoke := func(secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/tokens/"+target.Token.ID,
			strings.NewReader(`{"reason":"credential rotation"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	if got := revoke(viewer.Secret); got.Code != 403 {
		t.Errorf("viewer revoke: %d want 403", got.Code)
	}
	if got := revoke(owner.Secret); got.Code != 200 {
		t.Fatalf("owner revoke: %d %s", got.Code, got.Body.String())
	}
	found := false
	for _, entry := range store.AuditList(ws.ID, 100) {
		if entry.Action == "token.revoke" && entry.Object == target.Token.ID && entry.Detail == "credential rotation" {
			found = true
		}
	}
	if !found {
		t.Error("token revoke reason missing from audit")
	}
}

func TestGlobalCapabilityBeforeFirstWorkspace(t *testing.T) {
	store, err := platform.NewStore(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "bootstrap", platform.RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+owner.Secret)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	var flags map[string]bool
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &flags) != nil || !flags[string(admin.PermWorkspaceAdmin)] {
		t.Fatalf("empty-store global capability: %d %s", response.Code, response.Body.String())
	}
}
