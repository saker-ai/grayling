// Package boot verifies the compiled cmd/golab binary actually starts,
// bootstraps an owner credential, serves the console and the health
// endpoint, and shuts down cleanly on SIGTERM.
package boot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestBinaryBootsAndServes(t *testing.T) {
	if testing.Short() {
		t.Skip("boot test builds and runs the binary; skipped in -short")
	}
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "golab")
	build := exec.Command("go", "build", "-o", bin, "./cmd/golab")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	gonacosRoot := filepath.Join(root, "..", "gonacos")
	if _, err := os.Stat(filepath.Join(gonacosRoot, "api", "openapi", "upstream")); err != nil {
		t.Skipf("gonacos sibling repo not available at %s: %v", gonacosRoot, err)
	}

	dataDir := t.TempDir()
	apiPort := freePort(t)
	napPort := freePort(t)
	grpcPort := freePort(t)

	cmd := exec.Command(bin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", apiPort),
		"-data", filepath.Join(dataDir, "store.json"),
		"-gonacos-root", gonacosRoot,
		"-gonacos-data", filepath.Join(dataDir, "gonacos"),
		"-gonacos-addr", fmt.Sprintf("127.0.0.1:%d", napPort),
		"-gonacos-grpc-addr", fmt.Sprintf("127.0.0.1:%d", grpcPort),
	)
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

	// Capture the bootstrap owner token from the startup logs.
	tokenCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if idx := strings.Index(line, "glab_admin_"); idx >= 0 {
				fields := strings.Fields(line[idx:])
				if len(fields) > 0 {
					tokenCh <- strings.TrimSpace(fields[0])
					return
				}
			}
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	deadline := time.Now().Add(20 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.Contains(string(body), `"ok"`) {
				healthy = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("server never became healthy")
	}

	var token string
	select {
	case token = <-tokenCh:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap owner token not printed")
	}
	if !strings.HasPrefix(token, "glab_admin_") {
		t.Fatalf("unexpected token format: %q", token)
	}

	// The console must be served at / with the SPA shell.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("console get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Fatalf("console: %d %s", resp.StatusCode, truncate(string(body), 120))
	}
	// Static assets referenced by the shell must be served.
	for _, asset := range []string{"/app.js", "/style.css"} {
		ar, err := http.Get(base + asset)
		if err != nil {
			t.Fatalf("asset %s: %v", asset, err)
		}
		abody, _ := io.ReadAll(ar.Body)
		ar.Body.Close()
		if ar.StatusCode != 200 || len(abody) < 100 {
			t.Fatalf("asset %s: %d (%d bytes)", asset, ar.StatusCode, len(abody))
		}
	}
	// SPA fallback: unknown routes return the shell, not 404.
	fr, err := http.Get(base + "/flags/some/deep/route")
	if err != nil {
		t.Fatalf("spa fallback: %v", err)
	}
	fbody, _ := io.ReadAll(fr.Body)
	fr.Body.Close()
	if fr.StatusCode != 200 || !strings.Contains(strings.ToLower(string(fbody)), "<html") {
		t.Fatalf("spa fallback: %d", fr.StatusCode)
	}

	// The bootstrap token must work against the admin API.
	req, _ := http.NewRequest("POST", base+"/api/v1/admin/workspaces",
		strings.NewReader(`{"slug":"bootcheck","name":"Boot Check"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	wsResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	wsBody, _ := io.ReadAll(wsResp.Body)
	wsResp.Body.Close()
	if wsResp.StatusCode != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsResp.StatusCode, wsBody)
	}
	var ws map[string]any
	if err := json.Unmarshal(wsBody, &ws); err != nil || ws["slug"] != "bootcheck" {
		t.Fatalf("workspace payload: %s", wsBody)
	}

	// The store file must exist with owner-only permissions (HMAC key).
	info, err := os.Stat(filepath.Join(dataDir, "store.json"))
	if err != nil {
		t.Fatalf("store file: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("store permissions too open: %v", info.Mode())
	}

	// Graceful shutdown on SIGTERM.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// test/boot → repo root
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found at %s", root)
	}
	return root
}
