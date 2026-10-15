package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
)

// devOptions is the developer-mode configuration: an admin password the
// automatic sign-in can mint sessions for.
func devOptions() Options {
	return Options{
		AdminPassword:        "grayling-development-admin",
		ConsoleSessionSecret: "dev-autologin-test-secret",
		DevAutoLogin:         true,
	}
}

func devServer(t *testing.T) *Server {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store, nil, devOptions())
}

// get issues a GET as a browser would, optionally carrying cookies.
func get(t *testing.T, srv *Server, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://localhost:27002"+path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// sessionCookie returns the console session cookie a response set, if any.
func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleCookieName {
			return c
		}
	}
	return nil
}

func TestDevAutoLoginSignsConsoleIn(t *testing.T) {
	srv := devServer(t)

	// The console's very first probe — web/app.js asks for the session as it
	// loads — must already see the owner, or the sign-in form would flash
	// before a reload fixed it.
	probe := get(t, srv, "/api/v1/admin/session")
	if probe.Code != http.StatusOK || !strings.Contains(probe.Body.String(), "configured-admin-password") {
		t.Fatalf("first session probe: %d %s", probe.Code, probe.Body.String())
	}
	cookie := sessionCookie(t, probe)
	if cookie == nil {
		t.Fatal("first session probe did not set a session cookie")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		strings.Contains(cookie.Value, devConsoleCredential) {
		t.Fatalf("unsafe dev session cookie attributes: %+v", cookie)
	}
	if workspaces := get(t, srv, "/api/v1/admin/workspaces", cookie); workspaces.Code != http.StatusOK {
		t.Fatalf("admin read with the minted cookie: %d %s", workspaces.Code, workspaces.Body.String())
	}
}

func TestDevAutoLoginOnlyMintsForReads(t *testing.T) {
	srv := devServer(t)
	// A mutation without a session must not be signed in silently: the CSRF
	// same-origin rule still applies, and no cookie is handed out.
	r := httptest.NewRequest(http.MethodPost, "http://localhost:27002/api/v1/admin/workspaces", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if sessionCookie(t, w) != nil {
		t.Fatal("POST was signed in automatically")
	}
	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated POST succeeded: %d", w.Code)
	}
}

func TestDevAutoLoginRespectsSignOut(t *testing.T) {
	srv := devServer(t)
	cookie := sessionCookie(t, get(t, srv, "/"))
	if cookie == nil {
		t.Fatal("console load did not set a session cookie")
	}
	// Signing out must stick: refreshing the console cannot undo it, or the
	// sign-out button would do nothing in developer mode. Browsers keep a
	// cookie until it expires, so the marker has to win even alongside one.
	marker := &http.Cookie{Name: consoleDevSignedOutCookie, Value: "1"}
	if page := get(t, srv, "/", cookie, marker); sessionCookie(t, page) != nil {
		t.Fatal("signed-out browser was signed in again")
	}
	if probe := get(t, srv, "/api/v1/admin/session", marker); probe.Code != http.StatusUnauthorized {
		t.Fatalf("probe after sign-out: %d %s", probe.Code, probe.Body.String())
	}
	// A manual sign-in clears the marker, restoring automatic sign-in.
	r := httptest.NewRequest(http.MethodPost, "http://localhost:27002/api/v1/admin/session",
		strings.NewReader("token="+url.QueryEscape("grayling-development-admin")))
	r.Header.Set("Origin", "http://localhost:27002")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(marker)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("manual sign-in: %d %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == consoleDevSignedOutCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("manual sign-in did not clear the sign-out marker: %v", w.Result().Cookies())
	}
}

func TestDevAutoLoginOffByDefault(t *testing.T) {
	// A server built without the switch behaves exactly as before: probing the
	// console session the way web/app.js does must show the sign-in form.
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, nil, Options{
		AdminPassword:        "grayling-development-admin",
		ConsoleSessionSecret: "dev-autologin-test-secret",
	})
	if page := get(t, srv, "/"); sessionCookie(t, page) != nil {
		t.Fatal("session cookie minted without the developer-mode switch")
	}
	if probe := get(t, srv, "/api/v1/admin/session"); probe.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous probe: %d %s", probe.Code, probe.Body.String())
	}
}

func TestDevAutoLoginNeedsAdminPassword(t *testing.T) {
	// The switch alone must never open the console: without a configured
	// administrator there is no credential to mint a session from.
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, nil, Options{
		ConsoleSessionSecret: "dev-autologin-test-secret",
		DevAutoLogin:         true,
	})
	if page := get(t, srv, "/"); sessionCookie(t, page) != nil {
		t.Fatal("session cookie minted without a configured admin password")
	}
	if probe := get(t, srv, "/api/v1/admin/session"); probe.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous probe: %d %s", probe.Code, probe.Body.String())
	}
}

func TestDevAutoLoginKeepsExistingSession(t *testing.T) {
	srv := devServer(t)
	cookie := sessionCookie(t, get(t, srv, "/"))
	// Once a browser holds a session, later requests must not be handed a
	// fresh cookie on every call.
	if again := sessionCookie(t, get(t, srv, "/", cookie)); again != nil {
		t.Fatal("session cookie was reissued on every request")
	}
}

func TestDevAutoLoginReplacesStaleSession(t *testing.T) {
	srv := devServer(t)
	// A cookie left over from an earlier run (the session key is per process
	// unless a shared secret is configured) cannot be decrypted. It must not
	// shadow the session minted for this very request, or the console would
	// show its sign-in form until the next reload.
	stale := &http.Cookie{Name: consoleCookieName, Value: "stale-garbage-from-an-older-run"}
	probe := get(t, srv, "/api/v1/admin/session", stale)
	if probe.Code != http.StatusOK {
		t.Fatalf("probe with a stale cookie: %d %s", probe.Code, probe.Body.String())
	}
	if sessionCookie(t, probe) == nil {
		t.Fatal("stale cookie was not replaced")
	}
	if workspaces := get(t, srv, "/api/v1/admin/workspaces", stale); workspaces.Code != http.StatusOK {
		t.Fatalf("admin read with a stale cookie: %d %s", workspaces.Code, workspaces.Body.String())
	}
}
