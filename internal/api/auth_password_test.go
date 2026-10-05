package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/godeps/golab/internal/platform"
)

func TestConfiguredAdminPassword(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "owner", platform.RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	const password = "fixed-admin-password-123"
	server := NewServer(store, nil, Options{AdminPassword: password})

	for _, tc := range []struct {
		name, path, credential string
		want                   int
	}{
		{"password grants admin access", "/api/v1/admin/workspaces", password, http.StatusOK},
		{"existing token still works", "/api/v1/admin/workspaces", owner.Secret, http.StatusOK},
		{"wrong password is rejected", "/api/v1/admin/workspaces", "wrong-password", http.StatusUnauthorized},
		{"password has no SDK audience", "/api/v1/sdk/revision", password, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+tc.credential)
			resp := httptest.NewRecorder()
			server.Handler().ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}

	// Password authentication depends on configuration at each boot.
	withoutPassword := NewServer(store, nil, Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+password)
	resp := httptest.NewRecorder()
	withoutPassword.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured password status = %d, want 401", resp.Code)
	}
}

func TestAdminPasswordGuessesSharePeerLimit(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, nil, Options{
		AdminPassword: "fixed-admin-password-123", RequestsPerSecond: 1,
	})
	for i, want := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workspaces", nil)
		req.RemoteAddr = []string{"192.0.2.10:1234", "192.0.2.10:5678"}[i]
		req.Header.Set("Authorization", "Bearer "+[]string{"wrong-guess-a", "wrong-guess-b"}[i])
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		if resp.Code != want {
			t.Fatalf("guess %d status = %d, want %d", i, resp.Code, want)
		}
	}
}
