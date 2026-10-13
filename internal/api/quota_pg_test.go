package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/test/pgtest"
)

// A real PostgreSQL-backed pair verifies the HTTP read sees a peer's write
// after the same invalidation sync used by the running replica loop.
func TestQuotaAdminReplicaRead(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := t.Context()
	a, err := platform.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := platform.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ws, err := a.CreateWorkspace("quota-replica", "Quota replica")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := a.CreateToken(platform.TokenAdmin, "owner", platform.RoleOwner, ws.ID, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	q := platform.DefaultQuotas()
	q.MaxSegments = 17
	payload, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequest(http.MethodPut, "/api/v1/admin/workspaces/quota-replica/quotas", bytes.NewReader(payload))
	put.Header.Set("Authorization", "Bearer "+owner.Secret)
	putResp := httptest.NewRecorder()
	NewServer(a, nil, Options{}).Handler().ServeHTTP(putResp, put)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", putResp.Code, putResp.Body.String())
	}
	if err := b.SyncChanges(); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workspaces/quota-replica/quotas", nil)
	get.Header.Set("Authorization", "Bearer "+owner.Secret)
	getResp := httptest.NewRecorder()
	NewServer(b, nil, Options{}).Handler().ServeHTTP(getResp, get)
	var got platform.Quotas
	if err := json.Unmarshal(getResp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if getResp.Code != http.StatusOK || got != q {
		t.Fatalf("replica GET: %d %+v want %+v", getResp.Code, got, q)
	}
}
