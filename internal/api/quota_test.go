package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/saker-ai/grayling/internal/platform"
)

func TestQuotaAdminReadWriteAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := platform.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.CreateWorkspace("acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspace("other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.CreateToken(platform.TokenAdmin, "owner", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.CreateToken(platform.TokenAdmin, "viewer", platform.RoleViewer, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	global, err := store.CreateToken(platform.TokenAdmin, "global", platform.RoleOwner, "", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, nil, Options{})
	call := func(method, slug, secret string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, "/api/v1/admin/workspaces/"+slug+"/quotas", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp := httptest.NewRecorder()
		srv.Handler().ServeHTTP(resp, req)
		return resp
	}
	decode := func(resp *httptest.ResponseRecorder) platform.Quotas {
		t.Helper()
		var q platform.Quotas
		if err := json.Unmarshal(resp.Body.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		return q
	}
	if got := call(http.MethodGet, "acme", owner.Secret, nil); got.Code != 200 || decode(got) != platform.DefaultQuotas() {
		t.Fatalf("default GET: %d %s", got.Code, got.Body.String())
	}
	for _, tc := range []struct {
		method, slug, secret string
		want                 int
	}{
		{http.MethodGet, "acme", viewer.Secret, 403},
		{http.MethodPut, "acme", viewer.Secret, 403},
		{http.MethodGet, "other", owner.Secret, 403},
		{http.MethodPut, "other", owner.Secret, 403},
		{http.MethodGet, "missing", global.Secret, 404},
		{http.MethodGet, "acme", "invalid", 401},
	} {
		resp := call(tc.method, tc.slug, tc.secret, platform.DefaultQuotas())
		if resp.Code != tc.want {
			t.Errorf("%s %s: got %d want %d: %s", tc.method, tc.slug, resp.Code, tc.want, resp.Body.String())
		}
	}
	q := platform.DefaultQuotas()
	q.MaxFlags = 123
	if got := call(http.MethodPut, "acme", owner.Secret, q); got.Code != 200 || decode(got) != q {
		t.Fatalf("owner PUT: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodGet, "acme", global.Secret, nil); got.Code != 200 || decode(got) != q {
		t.Fatalf("global owner GET: %d %s", got.Code, got.Body.String())
	}
	bad := q
	bad.MaxFlags = 0
	if got := call(http.MethodPut, "acme", owner.Secret, bad); got.Code != 400 {
		t.Fatalf("invalid quota: %d %s", got.Code, got.Body.String())
	}
	reopened, err := platform.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.GetQuotas(ws.ID); got != q {
		t.Fatalf("persisted quota: %+v want %+v", got, q)
	}
	if got := reopened.GetQuotas(other.ID); got != platform.DefaultQuotas() {
		t.Fatalf("other default: %+v", got)
	}
}
