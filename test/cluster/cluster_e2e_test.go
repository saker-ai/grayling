// Package cluster is the multi-node / multi-replica acceptance test
// (docs/技术方案.md §3 集群, §12.2, docs/实施状态.md §3/§11 未做项):
//
//	PostgreSQL (shared platform store)  ──┐
//	Redis (gonacos cluster sync)        ──┤
//	                                       ▼
//	golab replica A (in-process gonacos node A)   golab replica B (gonacos node B)
//
// Two REAL golab binaries run as two replicas sharing one PostgreSQL and one
// external Redis; their in-process gonacos nodes form a two-node cluster
// synced through Redis pub/sub. Everything is real: compiled binaries, real
// PostgreSQL (test/pgtest), real Redis (test/redistest), real HTTP.
//
// Acceptance assertions:
//
//  1. 发布收敛: a publish on A becomes visible on B — via gonacos Redis
//     pub/sub while Redis is up (measured; 方案目标 5s 内).
//  2. 断连恢复: with Redis DOWN, a publish on A still converges on B via the
//     reconciler (shared PostgreSQL truth + republish), proving convergence
//     does not depend on the pub/sub fast path; Redis restart is survived.
//  3. 节点重启后快照一致: restarting B re-converges to A's exact published
//     state (same manifest revision, same per-key evaluation).
//  4. 多副本并发发布串行化: concurrent publishes across A and B on distinct
//     flags all succeed with a gap-free monotonic manifest carrying every
//     flag (PostgreSQL advisory lock + reload-under-lock serialization); a
//     same-flag race yields exactly one winner.
package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godeps/golab/pkg/bucketing"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/golab/test/pgtest"
	"github.com/godeps/golab/test/redistest"
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

func buildGolab(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "golab")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/golab")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build golab: %v\n%s", err, out)
	}
	return bin
}

type proc struct {
	name   string
	cmd    *exec.Cmd
	stderr bytes.Buffer
	mu     sync.Mutex
}

func startProc(t *testing.T, name, bin string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, cmd: exec.Command(bin, args...)}
	p.cmd.Stderr = &syncBuffer{p: p}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() { p.kill() })
	return p
}

type syncBuffer struct{ p *proc }

func (b *syncBuffer) Write(d []byte) (int, error) {
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	return b.p.stderr.Write(d)
}

func (p *proc) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *proc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
}

func (p *proc) terminate(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		p.kill()
		<-done
	}
}

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

func waitHealth(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/health")
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
	t.Fatalf("%s never became healthy: %v", base, lastErr)
}

// ---------------------------------------------------------------------------
// Replica
// ---------------------------------------------------------------------------

type replica struct {
	name string
	bin  string
	args []string
	base string
	proc *proc
	t    *testing.T
}

func startReplica(t *testing.T, name, bin, dsn, redisAddr, authSecret string,
	reconcileIv time.Duration) *replica {
	t.Helper()
	port := freePort(t)
	args := []string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-pg", dsn,
		"-redis", redisAddr,
		"-auth-secret", authSecret,
		"-gonacos-data", t.TempDir(),
		"-gonacos-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-gonacos-grpc-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-rollout-interval", "0",
		"-reconcile-interval", reconcileIv.String(),
		"-requests-per-second", "100000",
	}
	r := &replica{name: name, bin: bin, args: args,
		base: fmt.Sprintf("http://127.0.0.1:%d", port), t: t}
	r.proc = startProc(t, "golab-"+name, bin, args...)
	// On test failure, surface this replica's stderr (crash diagnosis).
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("=== replica %s stderr tail ===\n%s", name, tail(r.proc.stderrText(), 120))
		}
	})
	waitHealth(t, r.base, 60*time.Second)
	return r
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// restart terminates the replica and brings it back on the SAME address with
// the SAME shared backend — simulating a node restart in the cluster. The new
// process's cleanup is registered on the TOP-LEVEL test (r.t), never on a
// subtest t: a subtest-scoped cleanup would kill the fresh replica the moment
// the subtest ends.
func (r *replica) restart(t *testing.T) {
	t.Helper()
	old := r.proc
	old.terminate(t)
	r.proc = startProc(r.t, "golab-"+r.name+"(restarted)", r.bin, r.args...)
	waitHealth(t, r.base, 60*time.Second)
}

// ---------------------------------------------------------------------------
// HTTP clients (admin + sdk)
// ---------------------------------------------------------------------------

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatalf("request: %v", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil && resp.StatusCode < 400 {
			c.t.Fatalf("decode %s %s: %v (%s)", method, path, err, data)
		}
	}
	return resp.StatusCode
}

// publishFlag walks draft→submit→approve→publish; returns the manifest rev.
func (c *client) publishFlag(flag *model.Flag) (manifestRev int64, status int) {
	c.t.Helper()
	base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", flag.Key)
	if status = c.do("PUT", base+"/draft", map[string]any{"content": flag, "risk": "normal"}, nil); status != 200 {
		return 0, status
	}
	if status = c.do("POST", base+"/submit", nil, nil); status != 200 {
		return 0, status
	}
	if status = c.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); status != 200 {
		return 0, status
	}
	var res struct {
		ManifestRevision int64 `json:"manifestRevision"`
	}
	if status = c.do("POST", base+"/publish", map[string]any{"note": "ship"}, &res); status != 200 {
		return 0, status
	}
	return res.ManifestRevision, 200
}

// sdkRevision reads the current manifest revision served by this replica.
// Returns -1 on any error (used while polling through disruptions).
func (c *client) sdkRevision() int64 {
	var out struct {
		Revision int64 `json:"revision"`
	}
	req, err := http.NewRequest("GET", c.base+"/api/v1/sdk/revision", nil)
	if err != nil {
		return -1
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return -1
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &out); err != nil {
		return -1
	}
	return out.Revision
}

// sdkEval evaluates one flag for a bucketing key; returns the variation name.
func (c *client) sdkEval(flagKey, unit string) string {
	body, _ := json.Marshal(map[string]any{
		"flagKey": flagKey,
		"context": map[string]any{
			"targetingKey": unit,
			"attributes":   map[string]any{"user_id": unit},
		},
	})
	req, err := http.NewRequest("POST", c.base+"/api/v1/sdk/eval", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var out struct {
		Result struct {
			VariationType string `json:"variationType"`
		} `json:"result"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	return out.Result.VariationType
}

// waitRevision polls a replica's served manifest revision until it reaches
// want or the timeout elapses. Returns the observed latency.
func waitRevision(t *testing.T, c *client, want int64, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	var last int64 = -1
	for time.Now().Before(deadline) {
		last = c.sdkRevision()
		if last >= want {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replica revision never reached %d within %s (last=%d)", want, timeout, last)
	return 0
}

// ---------------------------------------------------------------------------
// Flag fixtures
// ---------------------------------------------------------------------------

func splitFlag(key string, onPct float64) *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           key, Type: model.FlagString,
		Fields:    []string{"user_id"},
		Bucketing: &model.Bucketing{Algorithm: bucketing.AlgorithmID, Unit: "user_id", Salt: key + "-salt"},
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{
			{Name: "split", Percentage: map[string]float64{
				"control": 100 - onPct, "candidate": onPct,
			}},
		},
	}
}

// ---------------------------------------------------------------------------
// The cluster test
// ---------------------------------------------------------------------------

func TestClusterMultiReplica(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster e2e runs two real replicas + PostgreSQL + Redis; skipped in -short")
	}
	dsn := pgtest.DSN(t)      // real PostgreSQL (docker or GOLAB_TEST_PG_DSN)
	rdb := redistest.Start(t) // real Redis (docker or GOLAB_TEST_REDIS_ADDR)
	bin := buildGolab(t, repoRoot(t))
	const authSecret = "cluster-e2e-shared-secret-0123456789abcdef"

	t.Logf("=== cluster: PostgreSQL + Redis(%s) + two golab replicas ===", rdb.Addr)

	// ---- Replica A (bootstraps the shared store) ----
	A := startReplica(t, "A", bin, dsn, rdb.Addr, authSecret, time.Second)
	ownerToken := scrapeOwnerToken(t, A.proc, 20*time.Second)
	adminA := &client{t: t, base: A.base, token: ownerToken}

	// ---- Tenant setup on A (committed to shared PG before B starts) ----
	if code := adminA.do("POST", "/api/v1/admin/workspaces",
		map[string]any{"slug": "acme", "name": "Acme"}, nil); code != http.StatusCreated {
		t.Fatalf("create workspace: %d", code)
	}
	if code := adminA.do("POST", "/api/v1/admin/workspaces/acme/environments",
		map[string]any{"slug": "prod", "name": "Production"}, nil); code != http.StatusCreated {
		t.Fatalf("create env: %d", code)
	}
	var sdkCT struct{ Secret string }
	if code := adminA.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "sdk", "kind": "sdk", "environment": "prod"}, &sdkCT); code != http.StatusCreated {
		t.Fatalf("create sdk token: %d", code)
	}
	sdkToken := sdkCT.Secret

	// First publication on A (manifest r1).
	revA, status := adminA.publishFlag(splitFlag("feature", 50))
	if status != 200 {
		t.Fatalf("publish r1 on A: %d", status)
	}
	t.Logf("published feature r1 on A (manifest r%d)", revA)

	// ---- Replica B joins the cluster (loads shared PG at boot) ----
	B := startReplica(t, "B", bin, dsn, rdb.Addr, authSecret, time.Second)
	sdkB := &client{t: t, base: B.base, token: sdkToken}
	sdkA := &client{t: t, base: A.base, token: sdkToken}

	// B must already serve r1 (loaded/synced at boot); reconcile guarantees it.
	waitRevision(t, sdkB, revA, 15*time.Second)
	t.Logf("B serves manifest r%d at join", sdkB.sdkRevision())

	// =====================================================================
	// 1. 发布收敛 (Redis up): publish r2 on A → visible on B via gonacos
	//    Redis pub/sub. Measured against the 方案 5s cluster-convergence goal.
	// =====================================================================
	t.Run("ConvergenceRedisUp", func(t *testing.T) {
		start := time.Now()
		_, st := adminA.publishFlag(splitFlag("feature", 60)) // new revision of the flag
		if st != 200 {
			t.Fatalf("publish r2 on A: %d", st)
		}
		// A's publish bumps the manifest revision; read what A now serves.
		wantA := sdkA.sdkRevision()
		lat := waitRevision(t, sdkB, wantA, 5*time.Second)
		t.Logf("1 PASS: publish on A → visible on B in %s (manifest r%d); gonacos Redis pub/sub fast path (< 5s goal, wall since publish-start %s)",
			lat, wantA, time.Since(start))
		if lat >= 5*time.Second {
			t.Fatalf("cluster convergence %s exceeded the 5s goal", lat)
		}
		// Same-key evaluations agree across replicas (one cluster, one truth).
		for _, u := range []string{"u1", "u2", "u3", "u7", "u42"} {
			if a, b := sdkA.sdkEval("feature", u), sdkB.sdkEval("feature", u); a != b {
				t.Fatalf("replicas disagree on eval(%s): A=%q B=%q", u, a, b)
			}
		}
		t.Logf("1 PASS: per-key evaluation identical on A and B")
	})

	// =====================================================================
	// 2. 断连恢复 (Redis down): convergence must NOT depend on the pub/sub
	//    fast path. With Redis stopped, a publish on A still reaches B via
	//    the reconciler (shared PostgreSQL truth + republish to B's gonacos).
	// =====================================================================
	t.Run("ConvergenceRedisDown", func(t *testing.T) {
		beforeA := sdkA.sdkRevision()
		t.Logf("2: stopping Redis (pub/sub fast path down); A at manifest r%d", beforeA)
		rdb.Stop(t)

		// Publish r3 on A while Redis is down. A commits to PG (up) and writes
		// its own gonacos locally; the Redis sync publish fails and is ignored.
		_, st := adminA.publishFlag(splitFlag("feature", 75))
		if st != 200 {
			// Redis being down must not break the publish path (PG is the
			// durable store; gonacos sync is best-effort).
			rdb.StartAgain(t)
			t.Fatalf("publish on A during Redis outage: %d", st)
		}
		wantA := sdkA.sdkRevision()
		if wantA <= beforeA {
			rdb.StartAgain(t)
			t.Fatalf("A did not advance during outage (still r%d)", wantA)
		}
		t.Logf("2: A advanced to manifest r%d during the outage", wantA)

		// B converges via its reconciler reloading the shared PG truth and
		// republishing into B's gonacos — independent of Redis.
		lat := waitRevision(t, sdkB, wantA, 30*time.Second)
		t.Logf("2 PASS: B converged to manifest r%d via reconciler+PostgreSQL while Redis was DOWN (%s)", wantA, lat)

		// Restart Redis; the cluster must survive and stay consistent.
		rdb.StartAgain(t)
		t.Logf("2: Redis restarted; verifying continued consistency")
		// A subsequent publish propagates through BOTH paths again.
		_, st = adminA.publishFlag(splitFlag("feature", 25))
		if st != 200 {
			t.Fatalf("publish after Redis restart: %d", st)
		}
		wantA2 := sdkA.sdkRevision()
		waitRevision(t, sdkB, wantA2, 15*time.Second)
		for _, u := range []string{"u1", "u5", "u9"} {
			if a, b := sdkA.sdkEval("feature", u), sdkB.sdkEval("feature", u); a != b {
				t.Fatalf("post-recovery replicas disagree on eval(%s): A=%q B=%q", u, a, b)
			}
		}
		t.Logf("2 PASS: after Redis restart, publish r%d converged on B and evaluations agree", wantA2)
	})

	// =====================================================================
	// 3. 节点重启后快照一致: restart B; it must re-converge to A's exact
	//    published state (manifest revision + per-key evaluation).
	// =====================================================================
	t.Run("RestartSnapshotConsistency", func(t *testing.T) {
		wantA := sdkA.sdkRevision()
		// Capture A's evaluation fingerprint across many keys BEFORE restart.
		fingerprint := map[string]string{}
		for i := 0; i < 60; i++ {
			u := fmt.Sprintf("restart-user-%d", i)
			fingerprint[u] = sdkA.sdkEval("feature", u)
		}
		t.Logf("3: restarting replica B (A at manifest r%d)", wantA)
		B.restart(t)
		sdkB2 := &client{t: t, base: B.base, token: sdkToken}

		lat := waitRevision(t, sdkB2, wantA, 30*time.Second)
		t.Logf("3: B re-converged to manifest r%d %s after restart", wantA, lat)

		mismatch := 0
		for u, wantVariant := range fingerprint {
			if got := sdkB2.sdkEval("feature", u); got != wantVariant {
				mismatch++
				if mismatch <= 5 {
					t.Logf("3: eval(%s) after restart = %q, A had %q", u, got, wantVariant)
				}
			}
		}
		if mismatch != 0 {
			t.Fatalf("3: %d/%d evaluations diverged after B restart", mismatch, len(fingerprint))
		}
		t.Logf("3 PASS: after restart B matches A on manifest r%d and all %d per-key evaluations (snapshot consistent)",
			wantA, len(fingerprint))
	})

	// =====================================================================
	// 4. 多副本并发发布串行化: concurrent publishes across A and B.
	// =====================================================================
	t.Run("ConcurrentCrossReplicaPublish", func(t *testing.T) {
		const n = 8
		// Prepare n approved drafts on A (shared PG). Distinct flags so the
		// race is on manifest-revision allocation, not on one flag's content.
		for i := 0; i < n; i++ {
			fl := splitFlag(fmt.Sprintf("conc-%d", i), 50)
			base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", fl.Key)
			if code := adminA.do("PUT", base+"/draft", map[string]any{"content": fl, "risk": "normal"}, nil); code != 200 {
				t.Fatalf("draft %d: %d", i, code)
			}
			if code := adminA.do("POST", base+"/submit", nil, nil); code != 200 {
				t.Fatalf("submit %d: %d", i, code)
			}
			if code := adminA.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != 200 {
				t.Fatalf("approve %d: %d", i, code)
			}
		}
		adminB := &client{t: t, base: B.base, token: ownerToken}

		base := sdkA.sdkRevision()
		var wg sync.WaitGroup
		statuses := make([]int, n)
		revs := make([]int64, n)
		// Publish even-indexed flags on A, odd-indexed on B — concurrently.
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := adminA
				if i%2 == 1 {
					c = adminB
				}
				fl := splitFlag(fmt.Sprintf("conc-%d", i), 50)
				rev, st := c.publishFlagApproved(fl)
				statuses[i] = st
				revs[i] = rev
			}(i)
		}
		wg.Wait()

		seen := map[int64]int{}
		for i := 0; i < n; i++ {
			if statuses[i] != 200 {
				t.Fatalf("concurrent publish %d failed: status %d", i, statuses[i])
			}
			seen[revs[i]]++
		}
		// Every manifest revision must be unique and form [base+1, base+n].
		if len(seen) != n {
			t.Fatalf("manifest revisions collided across replicas: %v", revs)
		}
		for r := base + 1; r <= base+int64(n); r++ {
			if seen[r] != 1 {
				t.Fatalf("manifest revision %d appears %d times (want 1): %v", r, seen[r], revs)
			}
		}
		t.Logf("4 PASS: %d concurrent cross-replica publishes → gap-free monotonic manifest revisions %v", n, revs)

		// The final manifest (read from BOTH replicas after convergence)
		// carries every concurrently published flag exactly once.
		finalRev := sdkA.sdkRevision()
		waitRevision(t, &client{t: t, base: B.base, token: sdkToken}, finalRev, 15*time.Second)
		for _, c := range []*client{sdkA, {t: t, base: B.base, token: sdkToken}} {
			snap := c.fullSnapshot()
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("conc-%d", i)
				if _, ok := snap[key]; !ok {
					t.Fatalf("final snapshot missing concurrently published flag %s", key)
				}
			}
			if _, ok := snap["feature"]; !ok {
				t.Fatal("final snapshot lost the pre-existing feature flag")
			}
		}
		t.Logf("4 PASS: both replicas' final snapshots carry all %d concurrent flags + the pre-existing flag", n)
	})

	// =====================================================================
	// 4b. Same-flag race across replicas: exactly one winner.
	// =====================================================================
	t.Run("SameFlagRaceOneWinner", func(t *testing.T) {
		fl := splitFlag("race-flag", 50)
		base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", fl.Key)
		if code := adminA.do("PUT", base+"/draft", map[string]any{"content": fl, "risk": "normal"}, nil); code != 200 {
			t.Fatalf("draft: %d", code)
		}
		if code := adminA.do("POST", base+"/submit", nil, nil); code != 200 {
			t.Fatalf("submit: %d", code)
		}
		if code := adminA.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != 200 {
			t.Fatalf("approve: %d", code)
		}
		adminB := &client{t: t, base: B.base, token: ownerToken}

		var wg sync.WaitGroup
		stA, stB := 0, 0
		wg.Add(2)
		go func() { defer wg.Done(); _, stA = adminB.publishFlagApproved(fl) }()
		go func() { defer wg.Done(); _, stB = adminA.publishFlagApproved(fl) }()
		wg.Wait()

		// A transport error (status 0) is NOT a serialization outcome — both
		// replicas must stay up and answer over HTTP for this proof to mean
		// anything.
		if stA == 0 || stB == 0 {
			t.Fatalf("transport failure during same-flag race (A=%d B=%d) — a replica was unreachable", stA, stB)
		}
		wins, losses := 0, 0
		for _, s := range []int{stA, stB} {
			switch {
			case s == 200:
				wins++
			case s == http.StatusConflict:
				losses++ // loser saw the draft already consumed (ErrInvalidState)
			default:
				t.Fatalf("unexpected race status %d (want 200 for the winner, 409 for the loser)", s)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("same-flag race produced wins=%d losses=%d (statuses A=%d B=%d), want exactly one winner", wins, losses, stA, stB)
		}
		t.Logf("4b PASS: concurrent same-flag publish on A and B → exactly one 200, the other 409 (draft already consumed; A=%d B=%d)",
			stA, stB)
	})

	// Final consistency sweep: A and B serve the same revision and agree.
	endRev := sdkA.sdkRevision()
	waitRevision(t, &client{t: t, base: B.base, token: sdkToken}, endRev, 15*time.Second)
	for _, u := range []string{"u1", "u2", "u3", "final-1", "final-2"} {
		if a, b := sdkA.sdkEval("feature", u), (&client{t: t, base: B.base, token: sdkToken}).sdkEval("feature", u); a != b {
			t.Fatalf("final replicas disagree on eval(%s): A=%q B=%q", u, a, b)
		}
	}
	t.Logf("=== FINAL: A and B consistent at manifest r%d ===", endRev)
}

// publishFlagApproved publishes an ALREADY approved draft (draft/submit/
// approve done separately) — used by the concurrency tests where drafts are
// prepared up front and published concurrently. It is goroutine-safe: it
// never calls t.Fatalf (which is illegal from a non-test goroutine) and
// instead reports status/error to the caller.
func (c *client) publishFlagApproved(flag *model.Flag) (int64, int) {
	path := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s/publish", flag.Key)
	body, _ := json.Marshal(map[string]any{"note": "concurrent"})
	req, err := http.NewRequest("POST", c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var res struct {
		ManifestRevision int64 `json:"manifestRevision"`
	}
	_ = json.Unmarshal(data, &res)
	return res.ManifestRevision, resp.StatusCode
}

// fullSnapshot returns the flagKey→flag map this replica currently serves.
func (c *client) fullSnapshot() map[string]map[string]any {
	var out struct {
		Flags map[string]json.RawMessage `json:"flags"`
	}
	req, _ := http.NewRequest("GET", c.base+"/api/v1/sdk/config", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("sdk/config: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &out); err != nil {
		c.t.Fatalf("decode snapshot: %v (%s)", err, truncate(string(data), 200))
	}
	res := map[string]map[string]any{}
	for k, raw := range out.Flags {
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		res[k] = f
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
