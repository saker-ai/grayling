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
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestRevocationProofAppWorkerRunsAndDrains(t *testing.T) {
	dsn := pgtest.DSN(t)
	server := httptest.NewUnstartedServer(nil)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x78}, 32))
	now := time.Now()
	verifier, err := client.NewVerifier(map[string]client.TrustKey{"proof-app": {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	app, err := grayling.Start(t.Context(), grayling.Options{PostgresDSN: dsn, GonacosData: filepath.Join(t.TempDir(), "gonacos"), AuthSecret: "revocation-worker-app-test-stable-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 4, AgentSigningKeyID: "proof-app", AgentSigningKey: key, AgentReleaseVerifier: verifier, AgentRevocationRefresh: true})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	server.Config.Handler = app.Handler()
	server.StartTLS()
	defer server.Close()
	root := app.BootstrapOwnerToken()
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces", map[string]string{"slug": "proofs", "name": "Proofs"}, 201)
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/proofs/environments", map[string]string{"slug": "prod", "name": "Production"}, 201)
	path := "/api/v1/admin/workspaces/proofs/environments/prod/agent/services/checkout/revocations"
	first := decodeAdmin[struct {
		Epoch            string    `json:"epoch"`
		ArtifactRevision string    `json:"artifactRevision"`
		ExpiresAt        time.Time `json:"expiresAt"`
	}](t, adminHTTP(t, server, root, "POST", path, map[string]any{"operationId": "initial", "expectedEpoch": "0", "bundleHashes": []string{strings.Repeat("a", 64)}, "reason": "app worker test"}, 200))
	if first.Epoch != "1" || first.ArtifactRevision != "1" {
		t.Fatal(first)
	}
	deadline := time.NewTimer(time.Until(first.ExpiresAt) - time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		rows := pgtest.Query(t, dsn, `SELECT revision::text FROM agent_resource_heads WHERE resource_key='revocation/state'`)
		if rows[0][0] != "1" {
			if !time.Now().Before(first.ExpiresAt) {
				t.Fatal("App worker renewed after old proof expiry")
			}
			n := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_admin_audit WHERE actor_id='system-agent-revocation-refresh'`)[0][0]
			if n != "1" {
				t.Fatal("App worker lacked atomic service audit", n)
			}
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("App worker did not renew default one-minute proof before expiry")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err = app.Close(ctx); err != nil {
		t.Fatal("App did not stop proof worker before closing sessions", err)
	}
}
