// Proxy deployment boot test: builds BOTH binaries (cmd/golab and
// cmd/golab-proxy), runs the proxy as an independent process in front of the
// real server, and verifies the deployment shape end to end — readiness vs
// liveness, remote evaluation, collection relay and clean shutdowns
// (docs/技术方案.md §3: 独立 proxy 部署形态).
package boot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func buildBin(t *testing.T, pkg, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, pkg)
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// startProcess launches a binary and returns it with a stderr line scanner
// wired to tokenCh (matches lines containing marker).
func startProcess(t *testing.T, name string, args []string, marker string, tokenCh chan<- string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	if marker != "" && tokenCh != nil {
		go func() {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				line := sc.Text()
				if idx := strings.Index(line, marker); idx >= 0 {
					fields := strings.Fields(line[idx:])
					if len(fields) > 0 {
						tokenCh <- strings.TrimSpace(fields[0])
						return
					}
				}
			}
		}()
	} else {
		go func() { _, _ = io.Copy(io.Discard, stderr) }()
	}
	return cmd
}

func waitHealthy(t *testing.T, base string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never became healthy", base)
}

func TestProxyBinaryDeployment(t *testing.T) {
	if testing.Short() {
		t.Skip("boot test builds and runs binaries; skipped in -short")
	}
	root := repoRoot(t)
	golabBin := buildBin(t, "./cmd/golab", "golab")
	proxyBin := buildBin(t, "./cmd/golab-proxy", "golab-proxy")
	gonacosRoot := resolveGonacosRoot(t, root)

	dataDir := t.TempDir()
	apiPort := freePort(t)
	proxyPort := freePort(t)
	napPort := freePort(t)
	grpcPort := freePort(t)

	tokenCh := make(chan string, 1)
	golabCmd := startProcess(t, golabBin, []string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", apiPort),
		"-data", filepath.Join(dataDir, "store.json"),
		"-gonacos-root", gonacosRoot,
		"-gonacos-data", filepath.Join(dataDir, "gonacos"),
		"-gonacos-addr", fmt.Sprintf("127.0.0.1:%d", napPort),
		"-gonacos-grpc-addr", fmt.Sprintf("127.0.0.1:%d", grpcPort),
	}, "glab_admin_", tokenCh)

	apiBase := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	waitHealthy(t, apiBase, 20*time.Second)
	var owner string
	select {
	case owner = <-tokenCh:
	case <-time.After(5 * time.Second):
		t.Fatal("owner token not printed")
	}

	// Provision workspace/env/tokens/flag through the real admin API.
	authToken := owner
	admin := func(method, path string, body any, out any) int {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, apiBase+path, rdr)
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if out != nil && len(data) > 0 {
			_ = json.Unmarshal(data, out)
		}
		if resp.StatusCode >= 400 {
			t.Logf("%s %s → %d: %s", method, path, resp.StatusCode, data)
		}
		return resp.StatusCode
	}
	if code := admin("POST", "/api/v1/admin/workspaces",
		map[string]any{"slug": "pxboot"}, nil); code != http.StatusCreated {
		t.Fatalf("workspace: %d", code)
	}
	if code := admin("POST", "/api/v1/admin/workspaces/pxboot/environments",
		map[string]any{"slug": "prod"}, nil); code != http.StatusCreated {
		t.Fatalf("env: %d", code)
	}
	var sdkCT struct {
		Secret string `json:"secret"`
	}
	if code := admin("POST", "/api/v1/admin/workspaces/pxboot/tokens",
		map[string]any{"name": "sdk", "kind": "sdk", "environment": "prod"}, &sdkCT); code != http.StatusCreated {
		t.Fatalf("sdk token: %d", code)
	}
	var colCT struct {
		Secret string `json:"secret"`
	}
	if code := admin("POST", "/api/v1/admin/workspaces/pxboot/tokens",
		map[string]any{"name": "col", "kind": "collect", "environment": "prod"}, &colCT); code != http.StatusCreated {
		t.Fatalf("collect token: %d", code)
	}
	flagDoc := map[string]any{
		"schemaVersion": 1, "key": "boot-flag", "type": "bool",
		"variations":  map[string]any{"on": true, "off": false},
		"defaultRule": map[string]any{"variation": "on"},
	}
	base := "/api/v1/admin/workspaces/pxboot/environments/prod/flags/boot-flag"
	if code := admin("PUT", base+"/draft", map[string]any{
		"content": flagDoc, "baseRevision": 0, "expectedDraftHash": "",
	}, nil); code != http.StatusOK {
		t.Fatalf("draft: %d", code)
	}
	if code := admin("POST", base+"/submit", map[string]any{"reason": "proxy boot"}, nil); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	var reviewer struct{ Secret string }
	if code := admin("POST", "/api/v1/admin/workspaces/pxboot/tokens",
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewer); code != http.StatusCreated {
		t.Fatalf("reviewer token: %d", code)
	}
	authToken = reviewer.Secret
	if code := admin("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	authToken = owner
	if code := admin("POST", base+"/publish", map[string]any{"note": "ship"}, nil); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}

	// Start the proxy as an independent process in front of the server.
	proxyCmd := startProcess(t, proxyBin, []string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", proxyPort),
		"-upstream", apiBase,
		"-timeout", "5s",
	}, "", nil)
	pxBase := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	waitHealthy(t, pxBase, 10*time.Second) // proxy readiness == upstream health

	// Liveness is proxy-owned and answers even before/independent of upstream.
	live, err := http.Get(pxBase + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", live.StatusCode)
	}

	// Remote evaluation through the proxy process.
	evalBody, _ := json.Marshal(map[string]any{
		"flagKey": "boot-flag",
		"context": map[string]any{"attributes": map[string]any{"user_id": "u1"}},
	})
	ereq, _ := http.NewRequest("POST", pxBase+"/api/v1/sdk/eval", bytes.NewReader(evalBody))
	ereq.Header.Set("Authorization", "Bearer "+sdkCT.Secret)
	ereq.Header.Set("Content-Type", "application/json")
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatalf("eval via proxy: %v", err)
	}
	edata, _ := io.ReadAll(eresp.Body)
	eresp.Body.Close()
	if eresp.StatusCode != http.StatusOK {
		t.Fatalf("eval via proxy: %d %s", eresp.StatusCode, edata)
	}
	var evalOut struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(edata, &evalOut); err != nil || !evalOut.Value {
		t.Fatalf("eval payload: %s", edata)
	}

	// Collect relay accepts the collect credential (empty batch → accepted 0).
	cbody, _ := json.Marshal(map[string]any{"events": []any{}})
	creq, _ := http.NewRequest("POST", pxBase+"/api/v1/collect", bytes.NewReader(cbody))
	creq.Header.Set("Authorization", "Bearer "+colCT.Secret)
	creq.Header.Set("Content-Type", "application/json")
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatalf("collect via proxy: %v", err)
	}
	cdata, _ := io.ReadAll(cresp.Body)
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("collect via proxy: %d %s", cresp.StatusCode, cdata)
	}
	// …and rejects a wrong-audience credential passed through untouched.
	creq2, _ := http.NewRequest("POST", pxBase+"/api/v1/collect", bytes.NewReader(cbody))
	creq2.Header.Set("Authorization", "Bearer "+sdkCT.Secret)
	creq2.Header.Set("Content-Type", "application/json")
	cresp2, err := http.DefaultClient.Do(creq2)
	if err != nil {
		t.Fatalf("collect wrong audience: %v", err)
	}
	cresp2.Body.Close()
	if cresp2.StatusCode != http.StatusForbidden {
		t.Fatalf("sdk token on collect via proxy: %d, want 403", cresp2.StatusCode)
	}

	// Admin surface must not be exposed by the proxy.
	adminReq, _ := http.NewRequest("GET", pxBase+"/api/v1/admin/workspaces", nil)
	adminReq.Header.Set("Authorization", "Bearer "+owner)
	adminResp, err := http.DefaultClient.Do(adminReq)
	if err != nil {
		t.Fatalf("admin via proxy: %v", err)
	}
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin via proxy: %d, want 404", adminResp.StatusCode)
	}

	// Clean shutdowns: proxy first, then the server.
	if err := proxyCmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("proxy sigterm: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- proxyCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("proxy did not exit after SIGTERM")
	}
	if err := golabCmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("golab sigterm: %v", err)
	}
	done2 := make(chan error, 1)
	go func() { done2 <- golabCmd.Wait() }()
	select {
	case <-done2:
	case <-time.After(20 * time.Second):
		t.Fatal("golab did not exit after SIGTERM")
	}
}
