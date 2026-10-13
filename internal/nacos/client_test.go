package nacos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestClientRefreshesRejectedCachedToken(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nacos/v3/auth/user/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken": "issued-" + string(rune('0'+logins.Add(1))), "tokenTtl": 3600,
			})
		case "/nacos/v3/admin/core/namespace/list":
			if r.Header.Get("accessToken") == "issued-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("expired"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{}})
		}
	}))
	defer srv.Close()
	client, err := NewClient(Config{URL: srv.URL, Username: "user", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("logins = %d, want 2", got)
	}
}

func TestClientRequestHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := NewClient(Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PingContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PingContext error = %v, want context cancellation", err)
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
