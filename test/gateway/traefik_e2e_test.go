// Package gatewaye2e is the P5 gateway acceptance test (docs/技术方案.md §12.4,
// docs/traefik-golab-architecture.md): a REAL official Traefik ≥3.6 binary
// runs the multi-layer routing chain end to end —
//
//	client → Traefik parent router
//	           ① headers middleware: strip external X-Golab-* (anti-forgery)
//	           ② forwardAuth → golab-proxy /api/v1/gateway/forwardauth
//	              → GoLab /api/v1/sdk/eval (real flag, real bucketing)
//	           ③ child routers match the written-back X-Golab-Variant
//	         → stable backend / canary backend (real stub servers)
//
// Everything is a real process with real requests: the compiled cmd/golab
// binary (in-process gonacos), the compiled cmd/golab-proxy binary, the
// official Traefik release binary (checksum-verified — see test/traefiktest)
// and httptest stub backends. The dynamic configuration under test is the
// exact output of the admin-side generator (internal/gateway), so the
// generator itself is validated by Traefik accepting and routing on it.
//
// Acceptance assertions (P5 网关路由在实际版本上端到端验证):
//
//	A. 同一 bucketingKey 稳定路由到同一版本（repeated requests, one variant）
//	B. 50/50 分布在网关层可观测（两个后端都收到流量，比例在容差内）
//	C. 比例调整（改 GoLab 分桶参数重发布）后分布变化，Traefik 动态配置
//	   SHA-256 与进程零变更
//	D. 外部伪造 X-Golab-Variant 被剥离，不影响路由
//	E. 求值服务内部故障（GoLab 上游不可达）fail-open 到默认变体，网关
//	   不产生 5xx
//	F. KV(Redis) provider 目标：同一生成器输出写入真实 Redis，第二个真实
//	   Traefik 实例经 redis provider 加载并正确分流（multi-layer routing
//	   的 KV 目标实证）
//	G. 原始请求方法透传（POST 经网关仍为 POST）
package gatewaye2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gw "github.com/saker-ai/grayling/internal/gateway"
	"github.com/saker-ai/grayling/pkg/bucketing"
	"github.com/saker-ai/grayling/pkg/model"
	"github.com/saker-ai/grayling/test/redistest"
	"github.com/saker-ai/grayling/test/traefiktest"
)

// ---------------------------------------------------------------------------
// Process helpers
// ---------------------------------------------------------------------------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func repoRoot(t *testing.T) string {
	t.Helper()
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

func buildBinary(t *testing.T, root, pkg, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// proc is a supervised child process with captured stderr.
type proc struct {
	name    string
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	mu      sync.Mutex
	stopped bool
	logPath string
}

func startProc(t *testing.T, name, bin string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, cmd: exec.Command(bin, args...)}
	p.cmd.Stderr = &lockedBuffer{p: p}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() { p.stop() })
	return p
}

// lockedBuffer funnels child stderr into proc.stderr under the SAME mutex the
// reader (stderrText) holds — a bytes.Buffer is not safe for concurrent use.
type lockedBuffer struct {
	p *proc
}

func (b *lockedBuffer) Write(d []byte) (int, error) {
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	return b.p.stderr.Write(d)
}

func (p *proc) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *proc) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
}

func (p *proc) waitExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s did not exit within %s", p.name, timeout)
	}
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

// scrapeOwnerToken extracts the bootstrap owner token from startup logs.
func scrapeOwnerToken(t *testing.T, p *proc, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text := p.stderrText()
		if idx := strings.Index(text, "glab_admin_"); idx >= 0 {
			fields := strings.Fields(text[idx:])
			if len(fields) > 0 {
				return strings.TrimSpace(fields[0])
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: owner token not printed within %s\nstderr:\n%s", p.name, timeout, p.stderrText())
	return ""
}

func waitHealth(t *testing.T, base, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + path)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("%s%s never became healthy: %v", base, path, lastErr)
}

// ---------------------------------------------------------------------------
// Admin API client (raw HTTP against the real binary)
// ---------------------------------------------------------------------------

type adminClient struct {
	t        *testing.T
	base     string
	token    string
	reviewer string
}

func (a *adminClient) do(method, path string, body any, out any) int {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		a.t.Fatalf("request: %v", err)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil && resp.StatusCode < 400 {
			a.t.Fatalf("decode %s %s: %v (%s)", method, path, err, data)
		}
	}
	if resp.StatusCode >= 400 {
		a.t.Logf("%s %s → %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// publishFlag walks draft→submit→approve→publish against the real binary.
func (a *adminClient) publishFlag(flag *model.Flag) (manifestRev int64) {
	a.t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", flag.Key)
	var current struct {
		CurrentRevision int64  `json:"currentRevision"`
		DraftHash       string `json:"draftHash"`
	}
	if code := a.do("GET", base, nil, &current); code != 200 {
		a.t.Fatalf("read flag before saving draft: %d", code)
	}
	if code := a.do("PUT", base+"/draft", map[string]any{
		"content": flag, "risk": "normal",
		"baseRevision": current.CurrentRevision, "expectedDraftHash": current.DraftHash,
	}, nil); code != 200 {
		a.t.Fatalf("save draft: %d", code)
	}
	if code := a.do("POST", base+"/submit", map[string]any{"reason": "gateway verification"}, nil); code != 200 {
		a.t.Fatalf("submit: %d", code)
	}
	approver := &adminClient{t: a.t, base: a.base, token: a.reviewer}
	if code := approver.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != 200 {
		a.t.Fatalf("approve: %d", code)
	}
	var res struct {
		ManifestRevision int64 `json:"manifestRevision"`
	}
	if code := a.do("POST", base+"/publish", map[string]any{"note": "ship"}, &res); code != 200 {
		a.t.Fatalf("publish: %d", code)
	}
	return res.ManifestRevision
}

// ---------------------------------------------------------------------------
// Stub backends
// ---------------------------------------------------------------------------

type stubBackend struct {
	name string
	srv  *httptest.Server
}

func newStubBackend(t *testing.T, name string) *stubBackend {
	t.Helper()
	b := &stubBackend{name: name}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backend":       b.name,
			"method":        r.Method,
			"path":          r.URL.Path,
			"variantHeader": r.Header.Get(gw.VariantHeader),
			"flagKeyHeader": r.Header.Get(gw.FlagKeyHeader),
			"failOpen":      r.Header.Get(gw.FailOpenHeader),
			"reason":        r.Header.Get(gw.ReasonHeader),
			"forgedExtra":   r.Header.Get("X-Golab-Forged"),
		})
	}))
	t.Cleanup(b.srv.Close)
	return b
}

type backendReply struct {
	Backend       string `json:"backend"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	VariantHeader string `json:"variantHeader"`
	FlagKeyHeader string `json:"flagKeyHeader"`
	FailOpen      string `json:"failOpen"`
	Reason        string `json:"reason"`
	ForgedExtra   string `json:"forgedExtra"`
}

// ---------------------------------------------------------------------------
// Traefik process
// ---------------------------------------------------------------------------

type traefikInst struct {
	proc       *proc
	entryAddr  string
	entryURL   string
	staticPath string
	logPath    string
	accessPath string
}

func startTraefik(t *testing.T, bin, staticYAML string) *traefikInst {
	t.Helper()
	dir := t.TempDir()
	staticPath := filepath.Join(dir, "static.yml")
	if err := os.WriteFile(staticPath, []byte(staticYAML), 0o644); err != nil {
		t.Fatalf("write static config: %v", err)
	}
	inst := &traefikInst{
		staticPath: staticPath,
		logPath:    filepath.Join(dir, "traefik.log"),
		accessPath: filepath.Join(dir, "access.log"),
	}
	inst.proc = startProc(t, "traefik", bin, "--configFile="+staticPath)
	return inst
}

func (tf *traefikInst) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", tf.entryAddr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	tf.dumpLogs(t)
	t.Fatalf("traefik entrypoint %s never came up", tf.entryAddr)
}

func (tf *traefikInst) dumpLogs(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(tf.logPath); err == nil && len(data) > 0 {
		t.Logf("traefik log (%s):\n%s", tf.logPath, tail(string(data), 60))
	}
	if data, err := os.ReadFile(tf.accessPath); err == nil && len(data) > 0 {
		t.Logf("traefik access log tail:\n%s", tail(string(data), 30))
	}
	t.Logf("traefik stderr tail:\n%s", tail(tf.proc.stderrText(), 30))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// gatewayRequest sends one client request through the Traefik entrypoint.
func gatewayRequest(t *testing.T, entryURL, path, bucketKey string,
	extraHeaders map[string]string, method string) (int, *backendReply, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, entryURL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if bucketKey != "" {
		req.Header.Set("X-Bucketing-Key", bucketKey)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var reply backendReply
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatalf("decode backend reply: %v (%s)", err, body)
		}
	}
	respHeader := resp.Header.Clone()
	return resp.StatusCode, &reply, respHeader
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

const (
	flagKey = "gateway-route"
	// gatewaySecret is the shared route secret: the generator embeds it in
	// the forwardAuth address and the proxy enforces it (-gateway-secret),
	// so direct calls to the eval endpoint without the secret are never
	// evaluated upstream (no flag-probing oracle / exposure pollution).
	gatewaySecret = "gw-e2e-shared-route-secret"
)

func gatewayFlag(stablePct, canaryPct float64) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           flagKey, Type: model.FlagString,
		Fields:    []string{"user_id"},
		Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: "gw-e2e"},
		Variations: map[string]any{
			"stable": "stable-value", "canary": "canary-value",
		},
		DefaultRule: &model.Rule{Variation: "stable"},
		Targeting: []model.Rule{
			{Name: "split", Percentage: map[string]float64{"stable": stablePct, "canary": canaryPct}},
		},
	}
}

type fixture struct {
	t      *testing.T
	root   string
	golab  *proc
	proxy  *proc
	admin  *adminClient
	stable *stubBackend
	canary *stubBackend
	spec   *gw.RouteSpec

	golabBase string
	proxyBase string
	sdkToken  string
}

func setupFixture(t *testing.T) *fixture {
	return setupFixtureOnHost(t, "127.0.0.1")
}

func setupFixtureOnHost(t *testing.T, listenHost string) *fixture {
	t.Helper()
	root := repoRoot(t)
	fx := &fixture{t: t, root: root}

	// ---- stub backends ----
	fx.stable = newStubBackend(t, "service-stable")
	fx.canary = newStubBackend(t, "service-canary")

	// ---- golab control plane + data plane (real binary, real gonacos) ----
	golabBin := buildBinary(t, root, "./cmd/golab", "golab")
	golabPort := freePort(t)
	dataDir := t.TempDir()
	fx.golab = startProc(t, "golab", golabBin,
		"-addr", fmt.Sprintf("%s:%d", listenHost, golabPort),
		"-data", filepath.Join(dataDir, "store.json"),
		"-gonacos-data", filepath.Join(dataDir, "gonacos"),
		"-gonacos-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-gonacos-grpc-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-rollout-interval", "0",
		"-reconcile-interval", "0",
		// The gateway eval path is one upstream /sdk/eval per client request
		// on a SINGLE sdk token; the default 200 rps per-token limit would
		// throttle the test's request bursts (and each 429 would fail-open to
		// the default variant, skewing distribution assertions). Raise it.
		"-requests-per-second", "100000",
	)
	fx.golabBase = fmt.Sprintf("http://127.0.0.1:%d", golabPort)
	waitHealth(t, fx.golabBase, "/api/v1/health", 30*time.Second)
	ownerToken := scrapeOwnerToken(t, fx.golab, 15*time.Second)
	fx.admin = &adminClient{t: t, base: fx.golabBase, token: ownerToken}

	// ---- tenant setup ----
	if code := fx.admin.do("POST", "/api/v1/admin/workspaces",
		map[string]any{"slug": "acme", "name": "Acme"}, nil); code != http.StatusCreated {
		t.Fatalf("create workspace: %d", code)
	}
	if code := fx.admin.do("POST", "/api/v1/admin/workspaces/acme/environments",
		map[string]any{"slug": "prod", "name": "Production"}, nil); code != http.StatusCreated {
		t.Fatalf("create env: %d", code)
	}
	var reviewerCT struct{ Secret string }
	if code := fx.admin.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewerCT); code != http.StatusCreated {
		t.Fatalf("create reviewer token: %d", code)
	}
	fx.admin.reviewer = reviewerCT.Secret
	var sdkCT struct {
		Secret string
	}
	if code := fx.admin.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "gw-sdk", "kind": "sdk", "environment": "prod"}, &sdkCT); code != http.StatusCreated {
		t.Fatalf("create sdk token: %d", code)
	}
	if !strings.HasPrefix(sdkCT.Secret, "glab_sdk_") {
		t.Fatalf("unexpected sdk token %q", sdkCT.Secret)
	}
	fx.sdkToken = sdkCT.Secret

	// ---- initial 50/50 publication ----
	rev := fx.admin.publishFlag(gatewayFlag(50, 50))
	t.Logf("published %s r1 (manifest r%d) — 50/50 split", flagKey, rev)

	// ---- golab-proxy as the forwardAuth eval service (real binary) ----
	proxyBin := buildBinary(t, root, "./cmd/golab-proxy", "golab-proxy")
	proxyPort := freePort(t)
	fx.proxy = startProc(t, "golab-proxy", proxyBin,
		"-addr", fmt.Sprintf("%s:%d", listenHost, proxyPort),
		"-upstream", fx.golabBase,
		"-timeout", "5s",
		"-gateway-token", fx.sdkToken,
		"-gateway-secret", gatewaySecret,
		"-gateway-eval-timeout", "2s",
	)
	fx.proxyBase = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	waitHealth(t, fx.proxyBase, "/healthz", 20*time.Second)

	fx.spec = &gw.RouteSpec{
		Name:           "gw-e2e",
		EntryPoint:     "web",
		Rule:           "PathPrefix(`/`)",
		FlagKey:        flagKey,
		DefaultVariant: "stable",
		UnitField:      "user_id",
		UnitHeader:     "X-Bucketing-Key",
		EvalAddress:    fx.proxyBase,
		Secret:         gatewaySecret,
		Backends: []gw.VariantBackend{
			{Variant: "stable", URL: fx.stable.srv.URL, K8sService: "svc-stable", K8sPort: 80},
			{Variant: "canary", URL: fx.canary.srv.URL, K8sService: "svc-canary", K8sPort: 80},
		},
		Namespace: "golab",
	}
	return fx
}

// fileStaticConfig renders the Traefik static configuration for the file
// provider target.
func fileStaticConfig(entryAddr, dynamicPath, logPath, accessPath string) string {
	return fmt.Sprintf(`entryPoints:
  web:
    address: "%s"
providers:
  file:
    filename: "%s"
    watch: false
log:
  level: WARN
  filePath: "%s"
accessLog:
  filePath: "%s"
`, entryAddr, dynamicPath, logPath, accessPath)
}

// redisStaticConfig renders the Traefik static configuration for the KV
// (Redis) provider target.
func redisStaticConfig(entryAddr, redisAddr, logPath, accessPath string) string {
	return fmt.Sprintf(`entryPoints:
  web:
    address: "%s"
providers:
  redis:
    endpoints:
      - "%s"
    rootKey: "traefik"
log:
  level: WARN
  filePath: "%s"
accessLog:
  filePath: "%s"
`, entryAddr, redisAddr, logPath, accessPath)
}

// ---------------------------------------------------------------------------
// The end-to-end test
// ---------------------------------------------------------------------------

func TestTraefikGatewayE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("gateway e2e runs real traefik/golab/proxy processes; skipped in -short")
	}
	traefikBin := traefiktest.Binary(t)
	versionOut := traefiktest.VersionString(t, traefikBin)
	t.Logf("=== Traefik under test ===\n%s", versionOut)
	if !strings.Contains(versionOut, "Version:") {
		t.Fatalf("unexpected traefik version output:\n%s", versionOut)
	}

	fx := setupFixture(t)

	// ---- generate the dynamic configuration with the admin-side tool ----
	cfg, err := gw.Build(fx.spec)
	if err != nil {
		t.Fatalf("gateway.Build: %v", err)
	}
	dynamicYAML, err := cfg.FileYAML()
	if err != nil {
		t.Fatalf("FileYAML: %v", err)
	}
	dynDir := t.TempDir()
	dynPath := filepath.Join(dynDir, "dynamic.yml")
	if err := os.WriteFile(dynPath, dynamicYAML, 0o644); err != nil {
		t.Fatalf("write dynamic config: %v", err)
	}
	dynHashBefore := fileSHA256(t, dynPath)
	t.Logf("=== generated dynamic config (sha256 %s) ===\n%s", dynHashBefore, dynamicYAML)

	// ---- start the real Traefik on the file provider ----
	entryPort := freePort(t)
	entryAddr := fmt.Sprintf("127.0.0.1:%d", entryPort)
	tf := startTraefik(t, traefikBin, fileStaticConfig(entryAddr, dynPath,
		filepath.Join(dynDir, "traefik.log"), filepath.Join(dynDir, "access.log")))
	tf.entryAddr = entryAddr
	tf.entryURL = "http://" + entryAddr
	tf.waitReady(t)
	traefikPID := tf.proc.cmd.Process.Pid

	// Probe until the full chain answers (dynamic config loaded, proxy warm).
	probeKey := "probe-user"
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/hello", probeKey, nil, "GET")
		if status == 200 && (reply.Backend == "service-stable" || reply.Backend == "service-canary") {
			t.Logf("chain probe OK: %s → %s (variant header %q, flag %q, reason %q)",
				probeKey, reply.Backend, reply.VariantHeader, reply.FlagKeyHeader, reply.Reason)
			break
		}
		if time.Now().After(deadline) {
			tf.dumpLogs(t)
			t.Fatalf("gateway chain never came up (last status %d, reply %+v)", status, reply)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// =====================================================================
	// A. 稳定路由: same bucketingKey → same backend on every request.
	// =====================================================================
	t.Run("A_StableRouting", func(t *testing.T) {
		keys := make([]string, 30)
		for i := range keys {
			keys[i] = fmt.Sprintf("user-%d", i)
		}
		mapping := map[string]string{}
		for _, key := range keys {
			for attempt := 0; attempt < 3; attempt++ {
				status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/orders", key, nil, "GET")
				if status != 200 {
					tf.dumpLogs(t)
					t.Fatalf("key %s attempt %d: status %d", key, attempt, status)
				}
				if reply.FlagKeyHeader != flagKey {
					t.Fatalf("key %s: backend saw flag key %q, want %q", key, reply.FlagKeyHeader, flagKey)
				}
				if first, ok := mapping[key]; ok {
					if reply.Backend != first {
						t.Fatalf("UNSTABLE routing: key %s → %s then %s", key, first, reply.Backend)
					}
				} else {
					mapping[key] = reply.Backend
				}
				if (reply.VariantHeader == "canary") != (reply.Backend == "service-canary") {
					t.Fatalf("key %s: variant header %q disagrees with backend %s",
						key, reply.VariantHeader, reply.Backend)
				}
			}
		}
		canary, stable := 0, 0
		for _, b := range mapping {
			if b == "service-canary" {
				canary++
			} else {
				stable++
			}
		}
		t.Logf("A PASS: 30 keys × 3 requests each — every key pinned to one backend (stable=%d canary=%d)", stable, canary)
	})

	// =====================================================================
	// B. 50/50 分布可观测.
	// =====================================================================
	distribution := func(t *testing.T, n int, url string) (stableN, canaryN int) {
		t.Helper()
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("dist-user-%d", i)
			status, reply, _ := gatewayRequest(t, url, "/api/dist", key, nil, "GET")
			if status != 200 {
				t.Fatalf("distribution probe %d: status %d", i, status)
			}
			switch reply.Backend {
			case "service-stable":
				stableN++
			case "service-canary":
				canaryN++
			default:
				t.Fatalf("unknown backend %q", reply.Backend)
			}
		}
		return stableN, canaryN
	}
	t.Run("B_SplitDistribution", func(t *testing.T) {
		stableN, canaryN := distribution(t, 200, tf.entryURL)
		t.Logf("B: 200 distinct keys at 50/50 → stable=%d canary=%d", stableN, canaryN)
		if canaryN < 70 || canaryN > 130 {
			t.Fatalf("50%% split out of tolerance: stable=%d canary=%d", stableN, canaryN)
		}
		t.Logf("B PASS: both variants observed within [35%%,65%%] of traffic")
	})

	// =====================================================================
	// D. 伪造 header 被剥离 (run before the ratio change so known-stable keys
	//    exist from phase A/B semantics).
	// =====================================================================
	t.Run("D_ForgedVariantHeaderStripped", func(t *testing.T) {
		// Find 10 keys that legitimately evaluate to stable.
		var stableKeys []string
		for i := 0; len(stableKeys) < 10 && i < 400; i++ {
			key := fmt.Sprintf("dist-user-%d", i)
			status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/forgery", key, nil, "GET")
			if status != 200 {
				t.Fatalf("status %d", status)
			}
			if reply.Backend == "service-stable" {
				stableKeys = append(stableKeys, key)
			}
		}
		if len(stableKeys) < 10 {
			t.Fatal("could not collect 10 stable-evaluating keys")
		}
		// Forge the canary variant header on every one of them: the parent
		// router's headers middleware must strip it BEFORE forwardAuth, and
		// the eval service's answer must win.
		for _, key := range stableKeys {
			status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/forgery", key,
				map[string]string{gw.VariantHeader: "canary"}, "GET")
			if status != 200 {
				t.Fatalf("forged request status %d", status)
			}
			if reply.Backend != "service-stable" {
				t.Fatalf("FORGERY WORKED: key %s with forged X-Golab-Variant:canary reached %s",
					key, reply.Backend)
			}
			if reply.VariantHeader != "stable" {
				t.Fatalf("key %s: backend saw variant %q, want stable (forged value must be replaced)",
					key, reply.VariantHeader)
			}
		}
		// Forge ancillary headers too: they must not reach the backend with
		// the forged values (stripped, then only re-set by the eval service).
		status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/forgery", stableKeys[0],
			map[string]string{
				gw.VariantHeader:  "canary",
				gw.FlagKeyHeader:  "forged-flag",
				gw.FailOpenHeader: "forged-reason",
				"X-Golab-Forged":  "attacker",
			}, "GET")
		if status != 200 || reply.Backend != "service-stable" ||
			reply.FlagKeyHeader != flagKey || reply.FailOpen != "" || reply.ForgedExtra != "" {
			t.Fatalf("ancillary header forgery leaked: status=%d reply=%+v", status, reply)
		}
		t.Logf("D PASS: %d forged X-Golab-Variant:canary requests on stable keys all stayed on service-stable; ancillary X-Golab-* forgeries stripped", len(stableKeys))

		// Direct unauthenticated probe of the eval endpoint (bypassing
		// Traefik): without the generated route secret the request must NOT
		// be evaluated upstream — it fail-opens to the static default, so the
		// credential-holding endpoint is not a flag-probing/exposure-pollution
		// oracle for anyone who can reach the proxy.
		probeReq, _ := http.NewRequest("GET", fx.proxyBase+
			"/api/v1/gateway/forwardauth?flagKey="+flagKey+"&defaultVariant=stable&unitField=user_id", nil)
		probeReq.Header.Set("X-Bucketing-Key", "attacker-key")
		probeResp, err := http.DefaultClient.Do(probeReq)
		if err != nil {
			t.Fatalf("direct probe: %v", err)
		}
		defer probeResp.Body.Close()
		if probeResp.StatusCode != 200 {
			t.Fatalf("direct probe status = %d, want 200 (fail-open invariant)", probeResp.StatusCode)
		}
		if got := probeResp.Header.Get(gw.FailOpenHeader); got != "unauthorized" {
			t.Fatalf("direct probe without route secret: fail-open reason = %q, want unauthorized", got)
		}
		if got := probeResp.Header.Get(gw.VariantHeader); got != "stable" {
			t.Fatalf("direct probe variant = %q, want the static default", got)
		}
		t.Logf("D PASS: direct probe of the eval endpoint without the route secret → unauthorized fail-open (no upstream evaluation)")
	})

	// =====================================================================
	// G. 方法透传: POST stays POST through the gateway.
	// =====================================================================
	t.Run("G_MethodPassthrough", func(t *testing.T) {
		status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/orders", "post-user-1", nil, "POST")
		if status != 200 {
			t.Fatalf("POST status %d", status)
		}
		if reply.Method != "POST" || reply.Path != "/api/orders" {
			t.Fatalf("POST reached backend as %s %s", reply.Method, reply.Path)
		}
		t.Logf("G PASS: POST /api/orders reached %s as %s %s", reply.Backend, reply.Method, reply.Path)
	})

	// =====================================================================
	// C. 比例调整: republish with 0/100 — distribution flips while the
	//    Traefik dynamic config file and process are untouched.
	// =====================================================================
	t.Run("C_RatioChangeZeroGatewayConfigChange", func(t *testing.T) {
		newRev := fx.admin.publishFlag(gatewayFlag(0, 100))
		t.Logf("C: republished %s at 0/100 (manifest r%d) — no Traefik change issued", flagKey, newRev)

		// Poll until the first key flips (remote eval rebuilds on the new
		// manifest revision; bounded by 10s).
		deadline := time.Now().Add(10 * time.Second)
		flipped := false
		for time.Now().Before(deadline) {
			status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/dist", "dist-user-0", nil, "GET")
			if status == 200 && reply.Backend == "service-canary" {
				flipped = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !flipped {
			t.Fatal("ratio change never became visible through the gateway")
		}
		stableN, canaryN := distribution(t, 100, tf.entryURL)
		t.Logf("C: 100 distinct keys at 0/100 → stable=%d canary=%d", stableN, canaryN)
		if canaryN != 100 {
			t.Fatalf("0/100 split must send every key to canary: stable=%d canary=%d", stableN, canaryN)
		}

		// The gateway configuration and process must be bit-identical.
		dynHashAfter := fileSHA256(t, dynPath)
		if dynHashAfter != dynHashBefore {
			t.Fatalf("dynamic config changed: %s → %s", dynHashBefore, dynHashAfter)
		}
		if tf.proc.cmd.Process.Pid != traefikPID {
			t.Fatalf("traefik restarted: pid %d → %d", traefikPID, tf.proc.cmd.Process.Pid)
		}
		if tf.proc.cmd.ProcessState != nil {
			t.Fatal("traefik process exited during the ratio change")
		}
		t.Logf("C PASS: distribution 50/50→0/100 with dynamic config sha256 UNCHANGED (%s) and same traefik pid %d",
			dynHashAfter, traefikPID)

		// Restore 50/50 for the remaining subtests (also proves the reverse
		// direction is a pure GoLab-side change).
		fx.admin.publishFlag(gatewayFlag(50, 50))
		deadline = time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			stableN, canaryN = distribution(t, 40, tf.entryURL)
			if stableN > 0 && canaryN > 0 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if stableN == 0 || canaryN == 0 {
			t.Fatalf("restored 50/50 split not visible: stable=%d canary=%d", stableN, canaryN)
		}
		if fileSHA256(t, dynPath) != dynHashBefore {
			t.Fatal("dynamic config changed during restore")
		}
		t.Logf("C PASS (restore): back to 50/50 (stable=%d canary=%d of 40), config still %s",
			stableN, canaryN, dynHashBefore)
	})

	// =====================================================================
	// F. KV(Redis) provider: the same generator output, rendered to KV and
	//    loaded by a SECOND real Traefik instance through its redis provider.
	// =====================================================================
	t.Run("F_KVRedisProvider", func(t *testing.T) {
		rdb := redistest.Start(t)
		pairs, err := cfg.KVSorted(gw.DefaultKVRoot)
		if err != nil {
			t.Fatalf("KVSorted: %v", err)
		}
		for _, p := range pairs {
			rdb.Set(t, p.Key, p.Value)
		}
		t.Logf("F: wrote %d KV pairs into real Redis %s (dbsize %s)", len(pairs), rdb.Addr, rdb.KeysCount(t))

		kvPort := freePort(t)
		kvAddr := fmt.Sprintf("127.0.0.1:%d", kvPort)
		kvDir := t.TempDir()
		kvTf := startTraefik(t, traefikBin, redisStaticConfig(kvAddr, rdb.Addr,
			filepath.Join(kvDir, "traefik.log"), filepath.Join(kvDir, "access.log")))
		kvTf.entryAddr = kvAddr
		kvTf.entryURL = "http://" + kvAddr
		kvTf.waitReady(t)

		kvURL := kvTf.entryURL
		probeDeadline := time.Now().Add(30 * time.Second)
		for {
			status, reply, _ := gatewayRequest(t, kvURL, "/api/kv", "kv-probe", nil, "GET")
			if status == 200 && reply.Backend != "" {
				break
			}
			if time.Now().After(probeDeadline) {
				kvTf.dumpLogs(t)
				t.Fatalf("KV-provider traefik chain never came up (last status %d)", status)
			}
			time.Sleep(250 * time.Millisecond)
		}
		// Stability through the KV-loaded hierarchy.
		mapping := map[string]string{}
		for i := 0; i < 12; i++ {
			key := fmt.Sprintf("kv-user-%d", i)
			for attempt := 0; attempt < 2; attempt++ {
				status, reply, _ := gatewayRequest(t, kvURL, "/api/kv", key, nil, "GET")
				if status != 200 {
					t.Fatalf("kv key %s: status %d", key, status)
				}
				if first, ok := mapping[key]; ok && first != reply.Backend {
					t.Fatalf("KV chain UNSTABLE: %s → %s then %s", key, first, reply.Backend)
				}
				mapping[key] = reply.Backend
			}
		}
		// Forgery stripped on the KV chain too.
		var stableKey string
		for k, b := range mapping {
			if b == "service-stable" {
				stableKey = k
				break
			}
		}
		if stableKey == "" {
			t.Fatal("no stable key observed on the KV chain")
		}
		status, reply, _ := gatewayRequest(t, kvURL, "/api/kv", stableKey,
			map[string]string{gw.VariantHeader: "canary"}, "GET")
		if status != 200 || reply.Backend != "service-stable" {
			t.Fatalf("KV chain forgery leaked: status=%d reply=%+v", status, reply)
		}
		// The two Traefik instances agree per key (same generator, same eval
		// service, same deterministic bucketing).
		for key, kvBackend := range mapping {
			status, fileReply, _ := gatewayRequest(t, tf.entryURL, "/api/kv", key, nil, "GET")
			if status != 200 || fileReply.Backend != kvBackend {
				t.Fatalf("file chain (%s) and KV chain (%s) disagree on %s",
					fileReply.Backend, kvBackend, key)
			}
		}
		t.Logf("F PASS: %d-key stable routing + forgery strip + file/KV parity on a real Traefik redis-provider instance", len(mapping))
	})

	// =====================================================================
	// E. fail-open: kill the GoLab control/data plane; the eval service
	//    (proxy) stays up and must degrade to the default variant — the
	//    gateway keeps answering 200 from the default backend, never 5xx.
	//    (Run LAST: it terminates golab.)
	// =====================================================================
	t.Run("E_FailOpenOnEvalServiceFailure", func(t *testing.T) {
		// Pick a key that currently routes to CANARY — after the outage it
		// must deterministically fall back to the default (stable) backend.
		var canaryKey string
		for i := 0; i < 400 && canaryKey == ""; i++ {
			key := fmt.Sprintf("dist-user-%d", i)
			status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/failopen", key, nil, "GET")
			if status == 200 && reply.Backend == "service-canary" {
				canaryKey = key
			}
		}
		if canaryKey == "" {
			t.Fatal("no canary key found before the outage (cannot prove fallback)")
		}
		t.Logf("E: killing golab (upstream of the eval service); canary key = %s", canaryKey)
		fx.golab.stop()
		fx.golab.waitExit(t, 20*time.Second)

		// Give the proxy a moment to observe the closed upstream.
		time.Sleep(300 * time.Millisecond)

		for _, key := range []string{canaryKey, "fail-open-1", "fail-open-2", "fail-open-3"} {
			status, reply, hdr := gatewayRequest(t, tf.entryURL, "/api/failopen", key, nil, "GET")
			if status != 200 {
				tf.dumpLogs(t)
				t.Fatalf("FAIL-OPEN VIOLATED: gateway answered %d during the eval outage (want 200)", status)
			}
			if strings.HasPrefix(hdr.Get("Server"), "traefik") && reply.Backend == "" {
				t.Fatalf("gateway generated the response itself (no backend reached): %v", hdr)
			}
			if reply.Backend != "service-stable" {
				t.Fatalf("fail-open routed to %q, want the default backend service-stable", reply.Backend)
			}
			if reply.FailOpen == "" {
				t.Fatalf("fail-open reason header missing at the backend: %+v", reply)
			}
			if reply.VariantHeader != "stable" {
				t.Fatalf("fail-open variant header = %q, want stable", reply.VariantHeader)
			}
			t.Logf("E: key %s → %s (fail-open reason %q, variant %q)",
				key, reply.Backend, reply.FailOpen, reply.VariantHeader)
		}
		// Missing bucketing key during the outage: still 200 via default.
		status, reply, _ := gatewayRequest(t, tf.entryURL, "/api/failopen", "", nil, "GET")
		if status != 200 || reply.Backend != "service-stable" {
			t.Fatalf("no-key request during outage: status=%d reply=%+v", status, reply)
		}
		t.Logf("E PASS: GoLab outage → eval service fail-open; 5/5 requests served 200 by the default backend (reason %q), zero gateway 5xx",
			reply.FailOpen)
	})

	// Final evidence: config truly never changed across ALL phases.
	if final := fileSHA256(t, dynPath); final != dynHashBefore {
		t.Fatalf("dynamic config hash drifted across the test: %s → %s", dynHashBefore, final)
	}
	t.Logf("=== FINAL: dynamic config sha256 unchanged end-to-end: %s ===", dynHashBefore)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
