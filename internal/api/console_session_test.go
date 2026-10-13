package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/godeps/grayling/internal/platform"
)

func TestConsoleSessionLifecycleAndCSRF(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(platform.TokenAdmin, "owner", platform.RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	newServer := func() *Server { return NewServer(store, nil, Options{ConsoleSessionSecret: "shared-test-secret"}) }
	srv := newServer()
	call := func(method, path, origin string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://console.example"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	if got := call(http.MethodPost, "/api/v1/admin/session", "https://attacker.example", nil, "token="+url.QueryEscape(token.Secret)); got.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", got.Code)
	}
	login := call(http.MethodPost, "/api/v1/admin/session", "https://console.example", nil, "token="+url.QueryEscape(token.Secret))
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || strings.Contains(cookie.Value, token.Secret) {
		t.Fatalf("unsafe session cookie attributes: %+v", cookie)
	}
	if got := call(http.MethodGet, "/api/v1/admin/workspaces", "", cookie, ""); got.Code != http.StatusOK {
		t.Fatalf("session read: %d %s", got.Code, got.Body.String())
	}
	// Every replica must accept a session minted with the shared secret.
	srv = newServer()
	if got := call(http.MethodGet, "/api/v1/admin/workspaces", "", cookie, ""); got.Code != http.StatusOK {
		t.Fatalf("session read on second replica: %d %s", got.Code, got.Body.String())
	}
	bad := *cookie
	bad.Value += "x"
	if got := call(http.MethodGet, "/api/v1/admin/workspaces", "", &bad, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: %d", got.Code)
	}
	if got := call(http.MethodPost, "/api/v1/admin/workspaces", "", cookie, ""); got.Code != http.StatusForbidden {
		t.Fatalf("missing origin mutation: %d", got.Code)
	}
	if got := call(http.MethodPost, "/api/v1/admin/workspaces", "https://attacker.example", cookie, ""); got.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation: %d", got.Code)
	}
	if err := store.RevokeToken(token.Token.ID); err != nil {
		t.Fatal(err)
	}
	if got := call(http.MethodGet, "/api/v1/admin/workspaces", "", cookie, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token session: %d", got.Code)
	}
	if got := call(http.MethodGet, "/api/v1/admin/workspaces", "", cookie, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token on second replica: %d", got.Code)
	}
	logout := call(http.MethodDelete, "/api/v1/admin/session", "https://console.example", cookie, "")
	if logout.Code != http.StatusOK || len(logout.Result().Cookies()) != 1 || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout: %d headers %v", logout.Code, logout.Header())
	}
}

func TestConsoleLoginRequiresTLSOutsideLoopback(t *testing.T) {
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, nil, Options{})
	for _, tc := range []struct {
		host string
		want int
	}{
		{"console.example", http.StatusForbidden},
		{"localhost:8080", http.StatusBadRequest},
		{"127.0.0.1:8080", http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/api/v1/admin/session", strings.NewReader(""))
		r.Header.Set("Origin", "http://"+tc.host)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("host %s: got %d, want %d", tc.host, w.Code, tc.want)
		}
	}
}
