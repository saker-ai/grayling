//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestRevocationHTTPOwnerTLSCASAndProofRefresh(t *testing.T) {
	parallelAgentFixture(t)
	dsn := pgtest.DSN(t)
	server := httptest.NewUnstartedServer(nil)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, 32))
	now := time.Now()
	verifier, err := client.NewVerifier(map[string]client.TrustKey{"revocation-http": {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	app, err := grayling.Start(t.Context(), grayling.Options{PostgresDSN: dsn, GonacosData: filepath.Join(t.TempDir(), "gonacos"), AuthSecret: "revocation-http-test-stable-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 4, AgentSigningKeyID: "revocation-http", AgentSigningKey: key, AgentReleaseVerifier: verifier})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	server.Config.Handler = app.Handler()
	server.StartTLS()
	defer server.Close()
	root := app.BootstrapOwnerToken()
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces", map[string]string{"slug": "revocations", "name": "Revocations"}, 201)
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/revocations/environments", map[string]string{"slug": "prod", "name": "Production"}, 201)
	mint := func(role string) platform.CreatedToken {
		return decodeAdmin[platform.CreatedToken](t, adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/revocations/tokens", map[string]string{"name": role, "kind": "admin", "role": role}, 201))
	}
	owner := mint("owner")
	publisher := mint("publisher")
	path := "/api/v1/admin/workspaces/revocations/environments/prod/agent/services/checkout/revocations"
	request := map[string]any{"operationId": "revoke-http", "expectedEpoch": "0", "bundleHashes": []string{strings.Repeat("a", 64)}, "reason": "security incident", "cancelActiveStreams": true}
	adminHTTP(t, server, publisher.Secret, "POST", path, request, 403)
	plain := httptest.NewServer(app.Handler())
	defer plain.Close()
	adminHTTP(t, plain, owner.Secret, "POST", path, request, 403)
	adminRawHTTP(t, server, owner.Secret, "POST", path, []byte(`{"operationId":"bad","expectedEpoch":0,"bundleHashes":[],"reason":"invalid"}`), 400)
	adminRawHTTP(t, server, owner.Secret, "POST", path, []byte(`{"operationId":"bad","expectedEpoch":"18446744073709551616","bundleHashes":[],"reason":"invalid"}`), 400)
	type receipt struct {
		Epoch            string `json:"epoch"`
		ArtifactRevision string `json:"artifactRevision"`
		OperationID      string `json:"operationId"`
	}
	first := adminHTTP(t, server, owner.Secret, "POST", path, request, 200)
	if r := decodeAdmin[receipt](t, first); r.Epoch != "1" || r.OperationID != "revoke-http" {
		t.Fatal(r)
	}
	if replay := adminHTTP(t, server, owner.Secret, "POST", path, request, 200); !bytes.Equal(first, replay) {
		t.Fatal("owner operation replay changed")
	}
	request["operationId"] = "stale-http"
	adminHTTP(t, server, owner.Secret, "POST", path, request, 409)
	adminHTTP(t, server, owner.Secret, "POST", path+"/refresh", map[string]string{"operationId": "stale-proof-artifact", "expectedEpoch": "1", "expectedArtifactRevision": "999"}, 409)
	refreshed := decodeAdmin[receipt](t, adminHTTP(t, server, owner.Secret, "POST", path+"/refresh", map[string]string{"operationId": "refresh-http", "expectedEpoch": "1"}, 200))
	if refreshed.Epoch != "1" || refreshed.ArtifactRevision != "2" {
		t.Fatal(refreshed)
	}
	rows := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_admin_audit WHERE action IN ('agent.bundle.revoke','agent.bundle.revocation.refresh')`)
	if rows[0][0] != "2" {
		t.Fatal("receipt/audit not atomic", rows)
	}
}
