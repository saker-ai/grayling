// Package boot verifies the compiled cmd/golab binary actually starts,
// bootstraps an owner credential, serves the console and the health
// endpoint, and shuts down cleanly on SIGTERM.
package boot

import (
	"bufio"
	"context"
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

	"github.com/godeps/golab/test/pgtest"
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

func TestAnonymousNativeListenerRejectsNonLoopbackEvenWithOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the GoLab binary")
	}
	bin := buildBin(t, "./cmd/golab", "golab-native-bind-check")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-gonacos-addr", "0.0.0.0:0", "-gonacos-grpc-addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "without -nacos-native-auth=true") {
		t.Fatalf("anonymous native non-loopback listener was not rejected: err=%v output=%s", err, out)
	}
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

	gonacosRoot := resolveGonacosRoot(t, root)

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
	for _, asset := range []string{"/app.js", "/i18n.js", "/style.css"} {
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

// TestBinaryBootsWithPostgres runs the compiled binary against a REAL
// PostgreSQL (docker or GOLAB_TEST_PG_DSN — see test/pgtest): schema
// migration at boot, owner bootstrap into the database, admin write path
// through PG, and a clean SIGTERM shutdown.
func TestBinaryBootsWithPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("boot test builds and runs the binary; skipped in -short")
	}
	dsn := pgtest.DSN(t) // skips with a reason when no PostgreSQL is available
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "golab-pg")
	build := exec.Command("go", "build", "-o", bin, "./cmd/golab")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	gonacosRoot := resolveGonacosRoot(t, root)
	dataDir := t.TempDir()
	apiPort := freePort(t)
	napPort := freePort(t)
	grpcPort := freePort(t)

	cmd := exec.Command(bin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", apiPort),
		"-pg", dsn,
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
	deadline := time.Now().Add(30 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
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

	// Admin write path must land in PostgreSQL.
	req, _ := http.NewRequest("POST", base+"/api/v1/admin/workspaces",
		strings.NewReader(`{"slug":"pgboot","name":"PG Boot"}`))
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
	if got := pgtest.Query(t, dsn, `SELECT count(*) FROM database_states s,
		jsonb_each(convert_from(s.payload, 'UTF8')::jsonb->'workspaces') AS ws WHERE ws.value->>'slug' = 'pgboot'`); got[0][0] != "1" {
		t.Fatalf("workspace not persisted in postgres: %v", got)
	}
	if got := pgtest.Query(t, dsn, `SELECT jsonb_array_length(convert_from(payload, 'UTF8')::jsonb->'hmacKeyRing') FROM database_states WHERE id=1`); got[0][0] != "1" {
		t.Fatalf("hmac ring not bootstrapped in postgres: %v", got)
	}
	// No file-store may be created when -pg is active.
	if _, err := os.Stat(filepath.Join("data", "golab-store.json")); err == nil {
		t.Fatal("file store must not be used when -pg is set")
	}

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

func TestBinaryBootsWithSQLite(t *testing.T) {
	if testing.Short() {
		t.Skip("boot test builds and runs the binary")
	}
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "golab-sqlite")
	build := exec.Command("go", "build", "-o", bin, "./cmd/golab")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	path := filepath.Join(t.TempDir(), "nested", "platform.sqlite")
	apiPort := freePort(t)
	cmd := exec.Command(bin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", apiPort),
		"-sqlite", path,
		"-gonacos-root", resolveGonacosRoot(t, root),
		"-gonacos-data", filepath.Join(t.TempDir(), "gonacos"),
		"-gonacos-addr", "127.0.0.1:0",
		"-gonacos-grpc-addr", "127.0.0.1:0",
	)
	if out, err := cmd.StderrPipe(); err == nil {
		go io.Copy(io.Discard, out)
	} else {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", apiPort)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("SQLite file missing: %v", err)
				}
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("SQLite-backed server never became healthy")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// resolveGonacosRoot returns a gonacos contract root (api/openapi/upstream).
// The pinned module's cache directory is preferred so the boot test runs
// against exactly the dependency go.mod pins; the sibling checkout is a
// development fallback.
func resolveGonacosRoot(t *testing.T, repoRoot string) string {
	t.Helper()
	list := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/godeps/gonacos")
	list.Dir = repoRoot
	list.Env = append(os.Environ(), "GOWORK=off")
	if out, err := list.Output(); err == nil {
		dir := strings.TrimSpace(string(out))
		if _, err := os.Stat(filepath.Join(dir, "api", "openapi", "upstream")); err == nil {
			return dir
		}
	}
	sibling := filepath.Join(repoRoot, "..", "gonacos")
	if _, err := os.Stat(filepath.Join(sibling, "api", "openapi", "upstream")); err != nil {
		t.Skipf("gonacos contract root not available (module cache or %s): %v", sibling, err)
	}
	return sibling
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
