package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shell tests pin the static/API split of the console handler: the React
// SPA at the root (no-store), SPA fallback for unknown paths (including the
// retired /legacy/ prefix), far-future caching for hashed /assets/, and
// /api//v1/ delegation untouched.

func testAPI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

func request(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHandlerServesReactSPA(t *testing.T) {
	h := Handler(testAPI())
	res := request(t, h, "/")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /: got %d, want 200", res.Code)
	}
	if cc := res.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET /: Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(res.Body.String(), "root") {
		t.Fatalf("GET /: body does not look like the React shell: %q", res.Body.String())
	}
	// SPA fallback: unknown client-side paths get the React shell too.
	deep := request(t, h, "/some/deep/link")
	if deep.Code != http.StatusOK || deep.Body.String() != res.Body.String() {
		t.Fatalf("SPA fallback mismatch: got %d", deep.Code)
	}
	// The legacy console is gone: its old prefix now falls back to the SPA.
	legacy := request(t, h, "/legacy/")
	if legacy.Code != http.StatusOK || legacy.Body.String() != res.Body.String() {
		t.Fatalf("GET /legacy/: got %d, want the React SPA fallback", legacy.Code)
	}
}

// findHashedAsset locates one real content-hashed asset in the embedded Vite
// output, so the cache test exercises a file the build actually produced.
func findHashedAsset(t *testing.T) string {
	t.Helper()
	entries, err := fs.ReadDir(StaticFS, "static/assets")
	if err != nil || len(entries) == 0 {
		t.Skip("no built assets under web/static/assets (placeholder test shell)")
	}
	return "assets/" + entries[0].Name()
}

func TestHandlerCachesHashedAssets(t *testing.T) {
	h := Handler(testAPI())
	name := findHashedAsset(t)
	res := request(t, h, "/"+name)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /%s: got %d, want 200", name, res.Code)
	}
	if cc := res.Header().Get("Cache-Control"); cc != immutableCache {
		t.Fatalf("GET /%s: Cache-Control = %q, want %q", name, cc, immutableCache)
	}
	// A missing asset still gets the SPA shell, not a 404.
	missing := request(t, h, "/assets/does-not-exist.js")
	if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), "root") {
		t.Fatalf("GET /assets/does-not-exist.js: got %d, want the SPA fallback", missing.Code)
	}
}

func TestHandlerDelegatesAPI(t *testing.T) {
	h := Handler(testAPI())
	for _, path := range []string{"/api/v1/admin/session", "/v1/flags"} {
		if res := request(t, h, path); res.Code != http.StatusTeapot {
			t.Fatalf("GET %s: got %d, want the API handler (418)", path, res.Code)
		}
	}
}
