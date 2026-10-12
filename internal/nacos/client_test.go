package nacos

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientLoginAndAuthenticatedRequest(t *testing.T) {
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nacos/v3/auth/user/login":
			_ = r.ParseForm()
			if r.Method != http.MethodPost || r.Form.Get("username") != "user" || r.Form.Get("password") != "secret" {
				t.Errorf("unexpected login request")
			}
			logins++
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "issued", "tokenTtl": 3600})
		case "/nacos/v3/admin/core/namespace/list":
			if r.Header.Get("Authorization") != "Bearer issued" || r.Header.Get("accessToken") != "issued" {
				t.Errorf("missing access token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": []any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client, err := NewClient(Config{URL: srv.URL, Username: "user", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatalf("expected cached login, got %d logins", logins)
	}
}

func TestNewClientRejectsInvalidCredentialsAndURL(t *testing.T) {
	for _, cfg := range []Config{
		{URL: "https://example.com/path"},
		{URL: "http://user:secret@example.com"},
		{URL: "ftp://example.com"},
		{URL: "https://example.com", Username: "user"},
		{URL: "https://example.com", Username: "user", Password: "password", Token: "token"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("expected invalid config for %+v", cfg)
		}
	}
}
