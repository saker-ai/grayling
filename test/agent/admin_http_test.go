//go:build integration

package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func adminHTTP(t *testing.T, server *httptest.Server, token, method, path string, body any, want int) []byte {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	return adminRawHTTP(t, server, token, method, path, encoded, want)
}
func adminRawHTTP(t *testing.T, server *httptest.Server, token, method, path string, encoded []byte, want int) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d", method, path, response.StatusCode, want)
	}
	if strings.Contains(path, "/agent/") && response.StatusCode < 300 && response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive Agent management response is cacheable")
	}
	return result
}
func decodeAdmin[T any](t *testing.T, body []byte) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestAgentAdminHTTPProvisionConnectRevokeAndRBAC(t *testing.T) {
	dsn := pgtest.DSN(t)
	server := httptest.NewUnstartedServer(nil)
	signingKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7a}, 32))
	app, err := grayling.Start(t.Context(), grayling.Options{AgentSigningKeyID: "app-discovery-key", AgentSigningKey: signingKey, GonacosData: filepath.Join(t.TempDir(), "gonacos"), PostgresDSN: dsn, AuthSecret: "stable-agent-admin-http-test-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 4})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	server.Config.Handler = app.Handler()
	server.StartTLS()
	defer server.Close()
	root := app.BootstrapOwnerToken()
	workspace := decodeAdmin[platform.Workspace](t, adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces", map[string]string{"slug": "managed", "name": "Managed"}, 201))
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/managed/environments", map[string]string{"slug": "prod", "name": "Production"}, 201)
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/managed/environments", map[string]string{"slug": "other", "name": "Other"}, 201)
	adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces", map[string]string{"slug": "outsider", "name": "Outsider"}, 201)
	mint := func(ws, kind, role string) platform.CreatedToken {
		t.Helper()
		request := map[string]string{"name": "test-" + kind + role, "kind": kind, "role": role}
		if kind == "sdk" {
			delete(request, "role")
			request["environment"] = "prod"
		}
		return decodeAdmin[platform.CreatedToken](t, adminHTTP(t, server, root, "POST", "/api/v1/admin/workspaces/"+ws+"/tokens", request, 201))
	}
	owner := mint("managed", "admin", "owner")
	publisher := mint("managed", "admin", "publisher")
	viewer := mint("managed", "admin", "viewer")
	outsider := mint("outsider", "admin", "owner")
	sdk := mint("managed", "sdk", "")
	base := "/api/v1/admin/workspaces/managed/environments/prod/agent/services/checkout"
	for _, candidate := range []struct {
		token   string
		allowed bool
	}{{owner.Secret, true}, {publisher.Secret, false}} {
		capabilities := decodeAdmin[struct {
			Permissions map[string]bool `json:"permissions"`
		}](t, adminHTTP(t, server, candidate.token, "GET", "/api/v1/admin/workspaces/managed/capabilities", nil, 200))
		for _, permission := range []string{"agent.credential.manage", "agent.quota.manage", "agent.audit.read"} {
			value, exists := capabilities.Permissions[permission]
			if !exists || value != candidate.allowed {
				t.Fatal("Agent capabilities disagree with actual RBAC")
			}
		}
	}

	plain := httptest.NewRecorder()
	plainRequest := httptest.NewRequest(http.MethodGet, "http://example.test"+base+"/credentials", nil)
	plainRequest.Header.Set("Authorization", "Bearer "+owner.Secret)
	plainRequest.Header.Set("X-Forwarded-Proto", "https")
	app.APIHandler().ServeHTTP(plain, plainRequest)
	if plain.Code != http.StatusForbidden {
		t.Fatal("management secret path accepted proxy-only TLS")
	}

	for _, token := range []string{publisher.Secret, viewer.Secret, outsider.Secret, sdk.Secret} {
		for _, suffix := range []string{"/credentials", "/limits", "/audit", "/registration-policy"} {
			adminHTTP(t, server, token, "GET", base+suffix, nil, 403)
		}
		adminHTTP(t, server, token, "POST", base+"/credentials", map[string]string{}, 403)
		adminHTTP(t, server, token, "PUT", base+"/registration-policy", map[string]string{}, 403)
		adminHTTP(t, server, token, "POST", base+"/deployments", map[string]string{}, 403)
	}
	adminHTTP(t, server, owner.Secret, "GET", base+"/limits", nil, 404)
	adminHTTP(t, server, owner.Secret, "PUT", base+"/limits", map[string]any{"operationId": "initial-limit", "expectedMaxConnections": 0, "maxConnections": 4}, 200)
	// Missing CAS, duplicate JSON fields, concatenated documents and body-selected
	// scope/actor are rejected before any authoritative mutation.
	for _, body := range []string{`{"operationId":"missing-cas","maxConnections":4}`, `{"operationId":"duplicate","operationId":"other","expectedMaxConnections":4,"maxConnections":5}`, `{"operationId":"trailing","expectedMaxConnections":4,"maxConnections":5} {}`, `{"operationId":"forged-actor","expectedMaxConnections":4,"maxConnections":5,"actorId":"root"}`} {
		adminRawHTTP(t, server, owner.Secret, "PUT", base+"/limits", []byte(body), 400)
	}
	adminHTTP(t, server, owner.Secret, "GET", base+"/registration-policy", nil, 404)
	policyRequest := map[string]any{"operationId": "registration-policy", "expectedRevision": "0", "policy": map[string]any{"allowedNetworks": []string{"10.20.0.0/16"}, "maxInstances": 4}}
	configured := decodeAdmin[control.RegistrationPolicyState](t, adminHTTP(t, server, owner.Secret, "PUT", base+"/registration-policy", policyRequest, 200))
	if configured.Revision != 1 {
		t.Fatal("unexpected policy revision")
	}
	replayedPolicy := decodeAdmin[control.RegistrationPolicyState](t, adminHTTP(t, server, owner.Secret, "PUT", base+"/registration-policy", policyRequest, 200))
	if replayedPolicy.Revision != 1 {
		t.Fatal("policy retry incremented revision")
	}
	policyRequest["operationId"] = "stale-policy"
	adminHTTP(t, server, owner.Secret, "PUT", base+"/registration-policy", policyRequest, 409)
	for _, body := range []string{`{"operationId":"no-cas","policy":{}}`, `{"operationId":"bad","expectedRevision":"1","policy":{"allowedNetworks":["0.0.0.0/0"],"maxInstances":4}}`, `{"operationId":"forged","expectedRevision":"1","policy":{"allowedNetworks":["10.20.0.0/16"],"maxInstances":4},"scope":{"workspaceId":"other"}}`} {
		adminRawHTTP(t, server, owner.Secret, "PUT", base+"/registration-policy", []byte(body), 400)
	}
	issueBody := map[string]any{"operationId": "issue-machine", "principalId": "workload", "agentId": "agent-managed", "instanceId": "instance-managed", "bootId": "boot-managed", "permissions": []string{"config.read", "discovery.read", "instance.register", "instance.renew"}, "expiresAt": time.Now().Add(time.Hour).UTC()}
	issued := decodeAdmin[control.IssuedCredential](t, adminHTTP(t, server, owner.Secret, "POST", base+"/credentials", issueBody, 201))
	if issued.Secret == "" || issued.Replayed || issued.Credential.Scope.WorkspaceID != workspace.ID {
		t.Fatal("incorrect managed credential result")
	}
	repeated := decodeAdmin[control.IssuedCredential](t, adminHTTP(t, server, owner.Secret, "POST", base+"/credentials", issueBody, 200))
	if repeated.Secret != "" || !repeated.Replayed || repeated.Credential.ID != issued.Credential.ID {
		t.Fatal("replay exposed secret or reissued credential")
	}
	issueBody["bootId"] = "changed"
	adminHTTP(t, server, owner.Secret, "POST", base+"/credentials", issueBody, 409)
	issueBody["bootId"] = "boot-managed"
	issueBody["operationId"] = "unknown-permission"
	issueBody["permissions"] = []string{"release.activate"}
	adminHTTP(t, server, owner.Secret, "POST", base+"/credentials", issueBody, 400)
	issueBody["permissions"] = []string{"config.read", "config.read"}
	adminHTTP(t, server, owner.Secret, "POST", base+"/credentials", issueBody, 400)

	deploymentInstance := map[string]any{"instanceId": "instance-managed", "bootId": "boot-managed", "buildId": "build-a", "imageDigest": "sha256:" + strings.Repeat("a", 64), "address": "10.20.0.10", "port": 8443, "tlsServerName": "checkout.internal", "zone": "az-a", "protocolVersion": 1}
	deploymentRequest := map[string]any{"operationId": "authorize-deployment", "principalId": "workload", "agentId": "agent-managed", "policyRevision": "1", "expiresAt": time.Now().Add(time.Hour).UTC(), "instance": deploymentInstance}
	deployment := decodeAdmin[control.DeploymentGrant](t, adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 200))
	deploymentReplay := decodeAdmin[control.DeploymentGrant](t, adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 200))
	if deployment.ID == "" || deploymentReplay.ID != deployment.ID {
		t.Fatal("deployment dedup changed authorization")
	}
	deploymentInstance["address"] = "10.21.0.1"
	adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 409)
	deploymentRequest["operationId"] = "forbidden-address"
	adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 400)
	deploymentInstance["address"] = "10.20.0.10"
	deploymentInstance["ready"] = true
	adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 400)
	delete(deploymentInstance, "ready")
	deploymentRequest["policyRevision"] = "0"
	adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 409)
	deploymentRequest["policyRevision"] = "1"
	adminHTTP(t, server, owner.Secret, "POST", strings.Replace(base, "/prod/", "/other/", 1)+"/deployments", deploymentRequest, 404)
	connection, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: issued.Secret, Hello: &agentv1.Hello{AgentId: "agent-managed", InstanceId: "instance-managed", BootId: "boot-managed", RequiredCapabilities: []string{"discovery.read"}}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	instance := &agentv1.Instance{Scope: &agentv1.Scope{WorkspaceId: issued.Credential.Scope.WorkspaceID, EnvironmentId: issued.Credential.Scope.EnvironmentID, ServiceKey: "checkout"}, InstanceId: "instance-managed", BootId: "boot-managed", BuildId: "build-a", ImageDigest: "sha256:" + strings.Repeat("a", 64), Address: "10.20.0.10", Port: 8443, TlsServerName: "checkout.internal", Zone: "az-a", ProtocolVersion: 1, Ready: true}
	registered, err := connection.Register(t.Context(), "managed-register", &agentv1.Register{Instance: instance, DeploymentAuthorization: []byte(deployment.ID)})
	if err != nil || registered.Instance.Lease.Epoch != 1 {
		t.Fatal("managed provisioning to WSS registration failed", err)
	}

	awaitNamingState(t, dsn, `SELECT count(*) FROM agent_instances WHERE active AND confirmed_revision=desired_revision AND confirmed_revision>0`, "1")
	bundle := sha256.Sum256([]byte("managed-routing-bundle"))
	if err := connection.Subscribe(t.Context(), "managed-discovery", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_DISCOVERY, Key: client.DiscoveryKey(bundle)}); err != nil {
		t.Fatal(err)
	}
	discoveryEvent := clientEvent(t, connection)
	if discoveryEvent.GetSnapshot() == nil {
		t.Fatalf("managed discovery subscription failed: %v", discoveryEvent.GetError())
	}
	now := time.Now()
	verifier, err := client.NewVerifier(map[string]client.TrustKey{"app-discovery-key": {PublicKey: signingKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	discovered, _, err := client.VerifyDiscovery(verifier, discoveryEvent.GetSnapshot().GetInlineResource(), issued.Credential.Scope, bundle, client.Watermark{}, now)
	if err != nil || len(discovered.Candidates()) != 1 {
		t.Fatal("managed App did not serve verified discovery", err)
	}

	if err := clientHeartbeat(t, connection); err != nil {
		t.Fatal(err)
	}
	adminHTTP(t, server, issued.Secret, "GET", base+"/credentials", nil, 401)
	metadata := adminHTTP(t, server, owner.Secret, "GET", base+"/credentials", nil, 200)
	if bytes.Contains(metadata, []byte(issued.Secret)) || bytes.Contains(metadata, []byte("secretHash")) {
		t.Fatal("credential metadata leaked secret material")
	}
	foreignBase := strings.Replace(base, "/environments/prod/", "/environments/other/", 1)
	foreign := adminHTTP(t, server, owner.Secret, "POST", foreignBase+"/credentials/"+issued.Credential.ID+"/revoke", map[string]string{"operationId": "foreign-id"}, 404)
	missing := adminHTTP(t, server, owner.Secret, "POST", foreignBase+"/credentials/missing/revoke", map[string]string{"operationId": "missing-id"}, 404)
	if !bytes.Equal(foreign, missing) {
		t.Fatal("foreign credential existence leaked")
	}
	started := time.Now()
	revokePath := base + "/credentials/" + issued.Credential.ID + "/revoke"
	adminHTTP(t, server, owner.Secret, "POST", revokePath, map[string]string{"operationId": "revoke-machine"}, 204)
	select {
	case <-connection.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connected credential revocation exceeded five seconds")
	}
	t.Logf("connected credential revocation observed after %s", time.Since(started))
	awaitNamingState(t, dsn, `SELECT count(*) FROM agent_instances WHERE active`, "0")
	awaitNamingState(t, dsn, `SELECT count(*) FROM agent_naming_intents WHERE present OR NOT confirmed`, "0")
	adminHTTP(t, server, owner.Secret, "POST", revokePath, map[string]string{"operationId": "revoke-machine"}, 204)
	var audit struct {
		Items []control.AdminAudit `json:"items"`
	}
	audit = decodeAdmin[struct {
		Items []control.AdminAudit `json:"items"`
	}](t, adminHTTP(t, server, owner.Secret, "GET", base+"/audit", nil, 200))
	if len(audit.Items) != 5 {
		t.Fatalf("audit count %d, want 5 committed operations", len(audit.Items))
	}
	for _, entry := range audit.Items {
		if entry.ActorID != owner.Token.ID || entry.ActorRole != "owner" {
			t.Fatal("audit actor did not come from authenticated credential")
		}
	}
	pgtest.Exec(t, dsn, `CREATE FUNCTION reject_http_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected database diagnostic'; END $$; CREATE TRIGGER reject_http_audit BEFORE INSERT ON agent_admin_audit FOR EACH ROW EXECUTE FUNCTION reject_http_audit()`)
	failure := adminHTTP(t, server, owner.Secret, "PUT", base+"/limits", map[string]any{"operationId": "audit-unavailable", "expectedMaxConnections": 4, "maxConnections": 5}, 503)
	if bytes.Contains(failure, []byte("injected")) || bytes.Contains(failure, []byte("agent_admin")) {
		t.Fatal("HTTP failure leaked storage diagnostics")
	}
	current := decodeAdmin[control.ScopeLimit](t, adminHTTP(t, server, owner.Secret, "GET", base+"/limits", nil, 200))
	if current.MaxConnections != 4 {
		t.Fatal("HTTP mutation survived failed audit")
	}

	policyRequest["operationId"] = "audit-policy-failure"
	policyRequest["expectedRevision"] = "1"
	adminHTTP(t, server, owner.Secret, "PUT", base+"/registration-policy", policyRequest, 503)
	unchanged := decodeAdmin[control.RegistrationPolicyState](t, adminHTTP(t, server, owner.Secret, "GET", base+"/registration-policy", nil, 200))
	if unchanged.Revision != 1 {
		t.Fatal("policy survived failed audit")
	}
	deploymentRequest["operationId"] = "audit-deployment-failure"
	deploymentInstance["instanceId"] = "second-instance"
	deploymentInstance["bootId"] = "second-boot"
	adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 503)
	pgtest.Exec(t, dsn, `DROP TRIGGER reject_http_audit ON agent_admin_audit; DROP FUNCTION reject_http_audit()`)
	// Retrying after the failure must create the authorization; a stranded
	// business row would instead violate the immutable deployment uniqueness.
	repaired := decodeAdmin[control.DeploymentGrant](t, adminHTTP(t, server, owner.Secret, "POST", base+"/deployments", deploymentRequest, 200))
	if repaired.ID == "" {
		t.Fatal("failed deployment could not be retried")
	}

}

func awaitNamingState(t *testing.T, dsn, query, want string) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		rows := pgtest.Query(t, dsn, query)
		if len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("Naming state did not converge: got %v want %s", rows, want)
		case <-tick.C:
		}
	}
}
