//go:build integration

package boot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestAgentBinarySigningTrustAndRevocationProof(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binary and uses PostgreSQL/TLS/WSS")
	}
	bin := buildBin(t, "./cmd/grayling", "grayling-agent-signing")
	dir := t.TempDir()
	dsn := pgtest.DSN(t)
	sample := httptest.NewTLSServer(nil)
	cert := sample.TLS.Certificates[0]
	sample.Close()
	certificateFile := filepath.Join(dir, "listener.pem")
	tlsKeyFile := filepath.Join(dir, "listener-key.pem")
	private, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{certificateFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), tlsKeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	signingKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x53}, 32))
	encoded, err := x509.MarshalPKCS8PrivateKey(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	signingFile := filepath.Join(dir, "signing.pem")
	rootsFile := filepath.Join(dir, "roots.json")
	if err := os.WriteFile(signingFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	roots := map[string]any{"schemaVersion": 1, "keys": []map[string]any{{"id": "cli-test", "publicKey": base64.StdEncoding.EncodeToString(signingKey.Public().(ed25519.PublicKey)), "notBefore": now.Add(-time.Hour), "notAfter": now.Add(time.Hour)}}}
	raw, err := json.Marshal(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootsFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	listener := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	args := []string{"-agent", "-pg", dsn, "-addr", listener, "-agent-hosts", listener, "-agent-max-connections", "4", "-tls-cert", certificateFile, "-tls-key", tlsKeyFile, "-agent-signing-key-id", "cli-test", "-agent-signing-key-file", signingFile, "-agent-trust-roots-file", rootsFile, "-agent-revocation-refresh", "-gonacos-addr", "127.0.0.1:0", "-gonacos-grpc-addr", "127.0.0.1:0", "-gonacos-data", filepath.Join(dir, "gonacos"), "-auth-secret", "agent-cli-boot-test-stable-secret"}
	// Actual command rejects unsafe secret permissions before creating any tables.
	if err := os.Chmod(signingFile, 0o644); err != nil {
		t.Fatal(err)
	}
	rejectedCtx, rejectedCancel := context.WithTimeout(t.Context(), 10*time.Second)
	output, runErr := exec.CommandContext(rejectedCtx, bin, args...).CombinedOutput()
	rejectedCancel()
	if runErr == nil || !strings.Contains(string(output), "owner-only file") {
		t.Fatal("binary did not fail closed before boot")
	}
	if tables := pgtest.Query(t, dsn, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`)[0][0]; tables != "0" {
		t.Fatal("invalid signing configuration opened stores", tables)
	}
	for _, invalid := range []struct {
		args   []string
		reason string
	}{
		{[]string{"-agent-trust-roots-file", rootsFile, "-pg", dsn}, "require -agent"},
		{[]string{"-agent-signing-key-id", "cli-test", "-write-recovery-point", filepath.Join(dir, "must-not-exist")}, "cannot be used with offline"},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		output, err := exec.CommandContext(ctx, bin, invalid.args...).CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), invalid.reason) {
			t.Fatal("binary accepted incompatible Agent flags")
		}
	}
	if err := os.Chmod(signingFile, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	tokens := make(chan string, 1)
	go func() {
		sent := false
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if at := strings.Index(line, "grayling_admin_"); at >= 0 && !sent {
				tokens <- strings.Fields(line[at:])[0]
				sent = true
			}
		}
	}()
	var token string
	select {
	case token = <-tokens:
	case <-time.After(20 * time.Second):
		t.Fatal("CLI did not bootstrap Agent server")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	origin := "https://" + listener
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err := httpClient.Get(origin + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Agent TLS listener not ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	request := func(method, path string, body any, status int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, origin+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil || resp.StatusCode != status {
			t.Fatal("CLI management request failed", path, resp.StatusCode, err)
		}
		return result
	}
	request("POST", "/api/v1/admin/workspaces", map[string]string{"slug": "agentcli", "name": "CLI Agent"}, 201)
	request("POST", "/api/v1/admin/workspaces/agentcli/environments", map[string]string{"slug": "prod", "name": "Production"}, 201)
	base := "/api/v1/admin/workspaces/agentcli/environments/prod/agent/services/checkout"
	request("PUT", base+"/limits", map[string]any{"operationId": "limit", "expectedMaxConnections": 0, "maxConnections": 4}, 200)
	var issued control.IssuedCredential
	if err := json.Unmarshal(request("POST", base+"/credentials", map[string]any{"operationId": "issue", "principalId": "workload", "agentId": "cli-agent", "instanceId": "cli-instance", "bootId": "cli-boot", "permissions": []string{"config.read"}, "expiresAt": time.Now().Add(time.Hour).UTC()}, 201), &issued); err != nil {
		t.Fatal(err)
	}
	request("POST", base+"/revocations", map[string]any{"operationId": "deny", "expectedEpoch": "0", "bundleHashes": []string{strings.Repeat("a", 64)}, "reason": "CLI proof fixture"}, 200)
	c, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(origin, "https"), Token: issued.Secret, HTTPClient: httpClient, Hello: &agentv1.Hello{AgentId: "cli-agent", InstanceId: "cli-instance", BootId: "cli-boot", RequiredCapabilities: []string{"release.revoke"}}})
	if err != nil {
		t.Fatal("CLI did not wire independent verifier capability", err)
	}
	defer c.Close()
	var event *agentv1.Envelope
	select {
	case event = <-c.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("CLI revocation proof not delivered")
	}
	if event == nil || event.GetRevoke() == nil {
		t.Fatal("wrong CLI event")
	}
	proof, err := c.Download(t.Context(), event.GetRevoke().View)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := client.NewVerifier(map[string]client.TrustKey{"cli-test": {PublicKey: signingKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := client.VerifyRevocation(verifier, proof, issued.Credential.Scope, client.Watermark{}, time.Now())
	if err != nil || document.Epoch != 1 {
		t.Fatal("CLI proof did not verify independently", err)
	}
	select {
	case next := <-c.Events():
		if next == nil || next.GetRevoke() == nil || next.GetRevoke().Epoch != 1 || next.GetRevoke().View.Revision != 2 {
			t.Fatal("CLI worker did not refresh delivery proof")
		}
		renewed, err := c.Download(t.Context(), next.GetRevoke().View)
		if err != nil {
			t.Fatal(err)
		}
		doc, claims, err := client.VerifyRevocation(verifier, renewed, issued.Credential.Scope, client.Watermark{}, time.Now())
		if err != nil || doc.Epoch != 1 || claims.Revision != 2 || !bytes.Equal(proof.Payload, renewed.Payload) {
			t.Fatal("CLI worker changed denial semantics", err)
		}
	case <-time.After(50 * time.Second):
		t.Fatal("CLI proof refresh flag did not start maintenance")
	}
	c.Close()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal("CLI shutdown failed", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("CLI failed to drain")
	}
}
