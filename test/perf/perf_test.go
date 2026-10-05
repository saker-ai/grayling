// Package perf contains the formal load tests behind the performance
// evidence section of docs/实施状态.md (§13 待测目标 → measured results):
//
//  1. Local-evaluation latency distribution (P50/P90/P99/P99.9/max) over a
//     500-flag snapshot — target P99 < 1ms.
//  2. Publish → SDK-visible latency over the real SSE push path — target
//     < 2s.
//  3. Concurrent SSE connections across scopes with fan-out latency and the
//     per-scope connection cap behaviour.
//
// Every number logged here includes the hardware, sample size and the exact
// code path measured; results are reproducible with:
//
//	GOWORK=off go test ./test/perf/ -count=1 -v
//
// These tests are heavy relative to unit tests; they skip under -short
// (same convention as test/boot).
package perf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godeps/golab/internal/api"
	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/pkg/bucketing"
	"github.com/godeps/golab/pkg/evaluation"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/pkg/sdk"
	gconfig "github.com/godeps/gonacos/pkg/config"
)

// ---------------------------------------------------------------------------
// Environment report + statistics helpers
// ---------------------------------------------------------------------------

func hardwareReport(t *testing.T) string {
	t.Helper()
	cpu := "unknown"
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					cpu = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}
	mem := "unknown"
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				mem = strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:"))
				break
			}
		}
	}
	report := fmt.Sprintf("cpu=%q cores(GOMAXPROCS)=%d mem=%s go=%s",
		cpu, runtime.GOMAXPROCS(0), mem, runtime.Version())
	t.Logf("HARDWARE: %s", report)
	return report
}

func sortedCopy(samples []time.Duration) []time.Duration {
	out := make([]time.Duration, len(samples))
	copy(out, samples)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// percentile returns the p-th percentile (0..100) of the samples.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := sortedCopy(samples)
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

func summarize(name string, samples []time.Duration) string {
	var sum time.Duration
	for _, s := range samples {
		sum += s
	}
	mean := time.Duration(int64(sum) / int64(len(samples)))
	sorted := sortedCopy(samples)
	return fmt.Sprintf("%s: n=%d mean=%v p50=%v p90=%v p95=%v p99=%v p99.9=%v max=%v",
		name, len(samples), mean.Round(time.Microsecond),
		percentile(sorted, 50), percentile(sorted, 90), percentile(sorted, 95),
		percentile(sorted, 99), percentile(sorted, 99.9), sorted[len(sorted)-1])
}

// ---------------------------------------------------------------------------
// 1. Local evaluation P99
// ---------------------------------------------------------------------------

// buildSnapshot mirrors pkg/evaluation's benchmark document: n flags, each
// with 3 targeting rules (constraint + segment + semver) ahead of a
// percentage default — the same shape the published mean (2.1µs/op) was
// measured on.
func buildSnapshot(n int) *model.Snapshot {
	segments := map[string]*model.Segment{
		"beta": {
			SchemaVersion: model.SchemaVersion, Key: "beta", Version: "1",
			Constraints: []model.Constraint{{Field: "user_id", Op: model.OpRegex, Value: `^beta-`}},
		},
	}
	flags := map[string]*model.Flag{}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("flag-%d", i)
		flags[key] = &model.Flag{
			SchemaVersion: model.SchemaVersion, Key: key, Type: model.FlagString,
			Fields:    []string{"user_id", "tenant", "version"},
			Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key},
			Variations: map[string]any{
				"control": "c", "candidate": "t", "extra": "e",
			},
			DefaultRule: &model.Rule{Percentage: map[string]float64{
				"control": 80, "candidate": 15, "extra": 5,
			}},
			Targeting: []model.Rule{
				{Name: "tenant", Constraints: []model.Constraint{
					{Field: "tenant", Op: model.OpIn, Values: []any{"acme", "globex"}},
				}, Variation: "extra"},
				{Name: "old", Constraints: []model.Constraint{
					{Field: "version", Op: model.OpSemverLt, Value: "2.0.0"},
				}, Variation: "control"},
				{Name: "beta", Segments: []string{"beta"}, Variation: "candidate"},
			},
		}
	}
	return &model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		Workspace:     "perf-ws", Environment: "perf-env",
		ManifestRevision: 1,
		Flags:            flags, Segments: segments,
	}
}

func TestPerfLocalEvaluationP99(t *testing.T) {
	if testing.Short() {
		t.Skip("perf suite is heavy; skipped in -short")
	}
	hardwareReport(t)
	ev, err := evaluation.NewEvaluator(buildSnapshot(500))
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	ctxs := make([]model.EvaluationContext, 64)
	for i := range ctxs {
		ctxs[i] = model.EvaluationContext{
			TargetingKey: fmt.Sprintf("user-%d", i),
			Attributes: map[string]any{
				"user_id": fmt.Sprintf("user-%d", i),
				"tenant":  []string{"acme", "initech", "globex"}[i%3],
				"version": fmt.Sprintf("%d.%d.0", i%4, i%7),
			},
		}
	}

	const samplesN = 1_000_000
	samples := make([]time.Duration, 0, samplesN)
	// Warm caches/branch predictors like a long-running process would.
	for i := 0; i < 10_000; i++ {
		ev.Evaluate("flag-"+strconv.Itoa(i%500), ctxs[i%len(ctxs)])
	}
	for i := 0; i < samplesN; i++ {
		key := "flag-" + strconv.Itoa(i%500)
		ctx := ctxs[i%len(ctxs)]
		start := time.Now()
		res := ev.Evaluate(key, ctx)
		samples = append(samples, time.Since(start))
		if res.Failed {
			t.Fatalf("eval failed at %d: %+v", i, res)
		}
	}
	t.Logf("RESULT %s", summarize("local-eval(500 flags, 64 contexts)", samples))
	p99 := percentile(samples, 99)
	if p99 >= time.Millisecond {
		t.Fatalf("§13 target violated: local eval P99 = %v >= 1ms", p99)
	}
	t.Logf("§13 target 本地求值 P99 < 1ms: MET (p99=%v, n=%d)", p99, samplesN)
}

// ---------------------------------------------------------------------------
// Server harness (real gonacos substrate, real HTTP)
// ---------------------------------------------------------------------------

type perfServer struct {
	store    *platform.Store
	api      *api.Server
	srv      *httptest.Server
	owner    string
	reviewer string

	wsMu      sync.Mutex
	createdWS map[string]bool
}

func newPerfServer(t *testing.T, rps float64) *perfServer {
	t.Helper()
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := configstore.NewGonacosStore(gconfig.NewService())
	apiSrv := api.NewServer(store, cfg, api.Options{RequestsPerSecond: rps})
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)
	ct, err := store.CreateToken(platform.TokenAdmin, "perf-owner", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	reviewer, err := store.CreateToken(platform.TokenAdmin, "perf-reviewer", platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		t.Fatalf("reviewer token: %v", err)
	}
	return &perfServer{store: store, api: apiSrv, srv: srv, owner: ct.Secret, reviewer: reviewer.Secret,
		createdWS: map[string]bool{}}
}

func (p *perfServer) do(t *testing.T, method, path, token string, body any, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil && resp.StatusCode < 400 {
			t.Fatalf("decode %s %s: %v (%s)", method, path, err, data)
		}
	}
	if resp.StatusCode >= 400 {
		t.Logf("%s %s → %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// setupEnv creates workspace+environment and mints sdk/collect tokens.
func (p *perfServer) setupEnv(t *testing.T, wsSlug, envSlug string) (sdkTok, colTok string) {
	t.Helper()
	p.ensureWorkspace(t, wsSlug, envSlug)
	var sdkCT platform.CreatedToken
	if code := p.do(t, "POST", "/api/v1/admin/workspaces/"+wsSlug+"/tokens", p.owner,
		map[string]any{"name": "sdk-" + envSlug, "kind": "sdk", "environment": envSlug}, &sdkCT); code != http.StatusCreated {
		t.Fatalf("create sdk token: %d", code)
	}
	var colCT platform.CreatedToken
	if code := p.do(t, "POST", "/api/v1/admin/workspaces/"+wsSlug+"/tokens", p.owner,
		map[string]any{"name": "col-" + envSlug, "kind": "collect", "environment": envSlug}, &colCT); code != http.StatusCreated {
		t.Fatalf("create collect token: %d", code)
	}
	return sdkCT.Secret, colCT.Secret
}

// setupTokenOnly creates the workspace/environment once and mints one extra
// sdk token — used to spread SSE dials across credentials.
func (p *perfServer) setupTokenOnly(t *testing.T, ws, env, name string) string {
	t.Helper()
	p.ensureWorkspace(t, ws, env)
	var ct platform.CreatedToken
	if code := p.do(t, "POST", "/api/v1/admin/workspaces/"+ws+"/tokens", p.owner,
		map[string]any{"name": name, "kind": "sdk", "environment": env}, &ct); code != http.StatusCreated {
		t.Fatalf("create token: %d", code)
	}
	return ct.Secret
}

func (p *perfServer) ensureWorkspace(t *testing.T, ws, env string) {
	t.Helper()
	p.wsMu.Lock()
	fresh := !p.createdWS[ws]
	if fresh {
		p.createdWS[ws] = true
	}
	p.wsMu.Unlock()
	if !fresh {
		return
	}
	if code := p.do(t, "POST", "/api/v1/admin/workspaces", p.owner,
		map[string]any{"slug": ws, "name": ws}, nil); code != http.StatusCreated {
		t.Fatalf("create workspace %s: %d", ws, code)
	}
	if code := p.do(t, "POST", "/api/v1/admin/workspaces/"+ws+"/environments", p.owner,
		map[string]any{"slug": env, "name": env}, nil); code != http.StatusCreated {
		t.Fatalf("create env: %d", code)
	}
}

func (p *perfServer) publishFlag(t *testing.T, ws, env string, flag *model.Flag) int64 {
	t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/%s/environments/%s/flags/%s", ws, env, flag.Key)
	if code := p.do(t, "PUT", base+"/draft", p.owner,
		map[string]any{"content": flag, "risk": "normal"}, nil); code != http.StatusOK {
		t.Fatalf("draft: %d", code)
	}
	if code := p.do(t, "POST", base+"/submit", p.owner, map[string]any{"reason": "performance verification"}, nil); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	if code := p.do(t, "POST", base+"/approve", p.reviewer, map[string]any{"note": "ok"}, nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	var res struct {
		ManifestRevision int64 `json:"manifestRevision"`
	}
	if code := p.do(t, "POST", base+"/publish", p.owner, map[string]any{"note": "perf"}, &res); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	return res.ManifestRevision
}

func perfFlag(key, note string) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagString,
		Fields:      []string{"user_id"},
		Bucketing:   &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key},
		Variations:  map[string]any{"control": "c-" + note, "candidate": "t-" + note},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Name: "ramp", Percentage: map[string]float64{"control": 90, "candidate": 10}},
		},
	}
}

// ---------------------------------------------------------------------------
// 2. Publish → SDK-visible latency (SSE push path)
// ---------------------------------------------------------------------------

func TestPerfPublishToSDKVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("perf suite is heavy; skipped in -short")
	}
	hardwareReport(t)
	p := newPerfServer(t, 0)
	sdkTok, colTok := p.setupEnv(t, "perfws", "prod")
	rev0 := p.publishFlag(t, "perfws", "prod", perfFlag("vis-flag", "r0"))

	client, err := sdk.New(context.Background(), sdk.Options{
		BaseURL: p.srv.URL, Token: sdkTok, CollectToken: colTok,
		PollInterval: 60 * time.Second, // measure the PUSH path, not polling
		MaxStaleness: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("sdk.New: %v", err)
	}
	client.Start(context.Background())
	t.Cleanup(client.Close)

	// Wait for the initial snapshot.
	deadline := time.Now().Add(5 * time.Second)
	for client.Snapshot() == nil || client.Snapshot().ManifestRevision < rev0 {
		if time.Now().After(deadline) {
			t.Fatal("initial snapshot never arrived")
		}
		time.Sleep(time.Millisecond)
	}

	const iterations = 30
	samples := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		wantRev := rev0 + int64(i) + 1
		flag := perfFlag("vis-flag", fmt.Sprintf("r%d", i+1))
		publishStart := time.Now()
		gotRev := p.publishFlag(t, "perfws", "prod", flag)
		if gotRev != wantRev {
			t.Fatalf("publish %d: manifest revision %d, want %d", i, gotRev, wantRev)
		}
		// Visibility = the moment the client's atomic snapshot swap exposes
		// the new manifest revision, measured from the instant the publish
		// call was issued (pipeline + push + pull + swap).
		for client.Snapshot().ManifestRevision < wantRev {
			if time.Since(publishStart) > 10*time.Second {
				t.Fatalf("iteration %d: revision %d never became visible", i, wantRev)
			}
			time.Sleep(50 * time.Microsecond)
		}
		samples = append(samples, time.Since(publishStart))
	}
	t.Logf("RESULT %s", summarize("publish→SDK-visible(SSE push, incl. publish pipeline)", samples))
	if max := percentile(samples, 100); max >= 2*time.Second {
		t.Fatalf("§13 target violated: publish→visible max = %v >= 2s", max)
	}
	t.Logf("§13 target 推送后 2s 内可见: MET (max=%v over %d publishes)",
		percentile(samples, 100), iterations)
}

// ---------------------------------------------------------------------------
// 3. SSE concurrent connections
// ---------------------------------------------------------------------------

// sseEventObs is one revision event with the arrival timestamp taken IN THE
// READER GOROUTINE (drain order would otherwise bias per-connection
// latency).
type sseEventObs struct {
	revision int64
	at       time.Time
}

type sseClient struct {
	events chan sseEventObs
	resp   *http.Response
}

// close releases the underlying connection: the reader goroutine's blocked
// Read returns, the server handler observes the cancelled request context
// and exits, and httptest.Server.Close (registered first, running last in
// LIFO cleanup order) no longer waits on held streams.
func (c *sseClient) close() {
	if c.resp != nil && c.resp.Body != nil {
		_ = c.resp.Body.Close()
	}
}

func dialSSE(t *testing.T, base, token string) *sseClient {
	t.Helper()
	req, err := http.NewRequest("GET", base+"/api/v1/sdk/stream", nil)
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse dial: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		t.Fatalf("sse status %d: %s", resp.StatusCode, body)
	}
	c := &sseClient{events: make(chan sseEventObs, 64), resp: resp}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Revision int64 `json:"revision"`
			}
			if json.Unmarshal([]byte(line[6:]), &ev) == nil {
				obs := sseEventObs{revision: ev.Revision, at: time.Now()}
				select {
				case c.events <- obs:
				default: // bounded buffer; the harness drains promptly
				}
			}
		}
	}()
	return c
}

func TestPerfSSEConcurrentConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("perf suite is heavy; skipped in -short")
	}
	hardwareReport(t)
	// RequestsPerSecond raised for the harness: 1000 simultaneous dials on a
	// handful of tokens would otherwise trip the per-token API rate limiter,
	// which bounds REQUEST rate, not held connections (the connection cap is
	// the SSE hub's per-scope limit, asserted separately below).
	p := newPerfServer(t, 100_000)

	// Registry of every held connection; closed in LIFO cleanup order
	// BEFORE httptest.Server.Close so the server never waits on streams.
	var allConns []*sseClient
	t.Cleanup(func() {
		for _, c := range allConns {
			c.close()
		}
	})

	const (
		envs         = 4
		connsPerEnv  = 250
		tokensPerEnv = 5
	)
	type envScope struct {
		ws     string
		tokens []string
		conns  []*sseClient
	}
	scopes := make([]*envScope, 0, envs)
	dialStart := time.Now()
	for e := 0; e < envs; e++ {
		sc := &envScope{ws: fmt.Sprintf("ssfws%d", e)}
		for k := 0; k < tokensPerEnv; k++ {
			tok := p.setupTokenOnly(t, sc.ws, "prod", fmt.Sprintf("sdk-%d-%d", e, k))
			sc.tokens = append(sc.tokens, tok)
			for c := 0; c < connsPerEnv/tokensPerEnv; c++ {
				conn := dialSSE(t, p.srv.URL, tok)
				sc.conns = append(sc.conns, conn)
				allConns = append(allConns, conn)
			}
		}
		scopes = append(scopes, sc)
	}
	total := 0
	for _, sc := range scopes {
		total += len(sc.conns)
	}
	t.Logf("RESULT sse dialed %d connections across %d scopes in %v",
		total, envs, time.Since(dialStart).Round(time.Millisecond))
	if total != envs*connsPerEnv {
		t.Fatalf("connection count %d != %d", total, envs*connsPerEnv)
	}

	// Every connection must receive the initial revision replay on connect
	// (revision 0 — nothing published in these scopes yet).
	for _, sc := range scopes {
		for i, c := range sc.conns {
			select {
			case obs := <-c.events:
				if obs.revision != 0 {
					t.Fatalf("initial replay revision = %d, want 0", obs.revision)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("conn %d never received the initial replay", i)
			}
		}
	}

	// Publish one flag per scope; every held connection must observe the new
	// revision. Fan-out latency is stamped in each reader goroutine from the
	// instant the publish call was issued.
	var fanout []time.Duration
	for _, sc := range scopes {
		publishStart := time.Now()
		rev := p.publishFlag(t, sc.ws, "prod", perfFlag("sse-flag", sc.ws))
		for i, c := range sc.conns {
			deadline := time.After(15 * time.Second)
			got := int64(-1)
			var arrival time.Time
			for got < rev {
				select {
				case obs := <-c.events:
					got, arrival = obs.revision, obs.at
				case <-deadline:
					t.Fatalf("conn %d never received revision %d (last %d)", i, rev, got)
				}
			}
			fanout = append(fanout, arrival.Sub(publishStart))
		}
	}
	t.Logf("RESULT %s", summarize(
		fmt.Sprintf("sse-fanout(publish→conn observed, %d conns × %d scopes)", connsPerEnv, envs),
		fanout))
	if max := percentile(fanout, 100); max > 10*time.Second {
		t.Fatalf("fan-out max %v exceeds the 10s observation bound", max)
	}

	// Per-scope connection cap: the hub default is 256 and each scope holds
	// 250, so 6 more fit and the next dial must fail with 503 sse_cap
	// (§11 SSE 连接数上限).
	sc := scopes[0]
	for i := 0; i < 6; i++ {
		allConns = append(allConns, dialSSE(t, p.srv.URL, sc.tokens[0]))
	}
	req, _ := http.NewRequest("GET", p.srv.URL+"/api/v1/sdk/stream", nil)
	req.Header.Set("Authorization", "Bearer "+sc.tokens[0])
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cap probe dial: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(body), "sse_cap") {
		t.Fatalf("connection cap not enforced: %d %s", resp.StatusCode, body)
	}
	t.Logf("RESULT sse per-scope cap enforced: connection #257 → 503 sse_cap")
	t.Logf("RESULT sse total concurrent connections held: %d", total+6)
}
