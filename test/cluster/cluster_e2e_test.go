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
//
// Convergence-wait contract (WHY the waits are shaped the way they are):
//
//   - The wait target is always the KNOWN TRUE FINAL revision from the
//     store/admin plane — the manifestRevision of a publish response
//     (allocated under the PG advisory lock), the gap-free-verified base+n
//     of the concurrent burst, the winner revision of the same-flag race —
//     never a sample of a possibly-lagging replica's served view (a replica
//     only sees publishes completed THROUGH THE OTHER replica after a
//     reload/pub-sub tick, so a sampled "final" revision can be lower than
//     the true one and a ">=" wait against it passes instantly).
//   - waitRevision means EVERY replica serves >= want; the final sweep uses
//     waitSettledRevision (every replica serves the SAME revision >= want).
//   - Revision parity does NOT imply document completeness: gonacos Redis
//     pub/sub syncs per document, fire-and-forget, so a replica can briefly
//     hold a manifest whose flag documents are still in flight; its read
//     path then correctly refuses to serve torn state (HTTP 5xx,
//     configstore.ErrIncompleteSnapshot) and the 1s reconciler backstop
//     converges (docs/实施状态.md 已知限制 2 边界③). Snapshot/eval assertions
//     therefore ride that window out with BOUNDED retries on transport/5xx
//     only, then assert hard. Everything else fails immediately so product
//     defects stay detectable: an HTTP 200 snapshot at revision >= the true
//     final that is missing keys (manifest key loss), two successful evals
//     that disagree (split brain), 4xx, decode failures. No wait is
//     unbounded.
package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	t        *testing.T
	base     string
	token    string
	reviewer string
}

func (c *client) do(method, path string, body any, out any) int {
	// Management writes use a PostgreSQL compare-and-swap epoch. A replica
	// that just joined or reloaded may legitimately return a retryable 409
	// when another replica committed first; refresh happens on that conflict.
	for attempt := range 5 {
		status := c.doOnce(method, path, body, out)
		if status != http.StatusConflict || attempt == 4 {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	return http.StatusConflict
}

func (c *client) doOnce(method, path string, body any, out any) int {
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
	if status = c.do("POST", base+"/submit", map[string]any{"reason": "cluster publish"}, nil); status != 200 {
		return 0, status
	}
	approver := &client{t: c.t, base: c.base, token: c.reviewer}
	if status = approver.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); status != 200 {
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

// transientError marks a convergence-window failure that bounded waits may
// retry: transport failures and HTTP 5xx — notably the documented
// configstore.ErrIncompleteSnapshot 500 a replica answers while gonacos
// per-document pub/sub propagation has delivered the manifest but not yet
// every flag document it references (docs/实施状态.md 已知限制 2 边界③:
// pub/sub fire-and-forget, 收敛上界=对账间隔+修复耗时).
type transientError struct{ msg string }

func (e *transientError) Error() string { return e.msg }

func isTransient(err error) bool {
	var tr *transientError
	return errors.As(err, &tr)
}

// sdkEvalStrict evaluates one flag for a bucketing key. A nil-error result
// is always a successful (HTTP 200) evaluation with a NON-EMPTY variation:
// transport failures and 5xx return errors instead of silently degrading to
// "" (which used to masquerade as a cross-replica eval disagreement). An
// HTTP 200 carrying an empty variationType is a hard error: for the
// default-rule string flags exercised here every legitimate outcome names a
// variation, so an empty one means a failure result (flag not in the served
// snapshot, missing bucket key, variation type mismatch) — a broken state
// that must never pass as a value.
func (c *client) sdkEvalStrict(flagKey, unit string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"flagKey": flagKey,
		"context": map[string]any{
			"targetingKey": unit,
			"attributes":   map[string]any{"user_id": unit},
		},
	})
	req, err := http.NewRequest("POST", c.base+"/api/v1/sdk/eval", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("eval(%s,%s) %s: build request: %w", flagKey, unit, c.base, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &transientError{fmt.Sprintf(
			"eval(%s,%s) %s: transport: %v", flagKey, unit, c.base, err)}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		return "", &transientError{fmt.Sprintf(
			"eval(%s,%s) %s: HTTP %d: %s", flagKey, unit, c.base, resp.StatusCode, truncate(string(data), 300))}
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("eval(%s,%s) %s: HTTP %d: %s",
			flagKey, unit, c.base, resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Result struct {
			VariationType string `json:"variationType"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("eval(%s,%s) %s: decode: %v (%s)",
			flagKey, unit, c.base, err, truncate(string(data), 300))
	}
	if out.Result.VariationType == "" {
		return "", fmt.Errorf("eval(%s,%s) %s: HTTP 200 but the evaluation is missing "+
			"(flag not in the served snapshot?): %s", flagKey, unit, c.base, truncate(string(data), 300))
	}
	return out.Result.VariationType, nil
}

// sdkSnapshotStrict fetches /api/v1/sdk/config with full error semantics:
// transport failures and 5xx (the convergence window — a replica must never
// serve a torn snapshot, so it answers ErrIncompleteSnapshot while manifest
// and documents are mid-sync) return errors; a nil-error result is the
// served manifest revision plus the flag map. An error body must never be
// silently decoded into an "empty snapshot" (which used to masquerade as
// missing flags).
func (c *client) sdkSnapshotStrict() (int64, map[string]map[string]any, error) {
	var out struct {
		ManifestRevision int64                      `json:"manifestRevision"`
		Flags            map[string]json.RawMessage `json:"flags"`
	}
	req, err := http.NewRequest("GET", c.base+"/api/v1/sdk/config", nil)
	if err != nil {
		return 0, nil, fmt.Errorf("sdk/config %s: build request: %w", c.base, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, &transientError{fmt.Sprintf("sdk/config %s: transport: %v", c.base, err)}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		return 0, nil, &transientError{fmt.Sprintf(
			"sdk/config %s: HTTP %d: %s", c.base, resp.StatusCode, truncate(string(data), 300))}
	}
	if resp.StatusCode != 200 {
		return 0, nil, fmt.Errorf("sdk/config %s: HTTP %d: %s",
			c.base, resp.StatusCode, truncate(string(data), 300))
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, nil, fmt.Errorf("sdk/config %s: decode: %v (%s)", c.base, err, truncate(string(data), 200))
	}
	flags := map[string]map[string]any{}
	for k, raw := range out.Flags {
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		flags[k] = f
	}
	return out.ManifestRevision, flags, nil
}

func bases(clients []*client) []string {
	out := make([]string, len(clients))
	for i, c := range clients {
		out[i] = c.base
	}
	return out
}

// waitRevision polls EVERY given replica until all of them serve a manifest
// revision >= want — the convergence contract is "all replicas >= want", not
// "one sampled replica >= a possibly-lagging sample" — and returns the
// observed latency. Transport errors while polling are retried through the
// deadline; the failure message keeps the last observed revisions.
func waitRevision(t *testing.T, want int64, timeout time.Duration, clients ...*client) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	last := make([]int64, len(clients))
	for i := range last {
		last[i] = -1
	}
	for time.Now().Before(deadline) {
		all := true
		for i, c := range clients {
			last[i] = c.sdkRevision()
			if last[i] < want {
				all = false
			}
		}
		if all {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replicas never all reached manifest r%d within %s (last=%v replicas=%v)",
		want, timeout, last, bases(clients))
	return 0
}

// waitSettledRevision waits until EVERY replica serves the SAME manifest
// revision and it is >= want ("两副本 revision 相等且 >= 真终值"). Returns the
// settled revision and the wait latency.
func waitSettledRevision(t *testing.T, want int64, timeout time.Duration, clients ...*client) (int64, time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	last := make([]int64, len(clients))
	for i := range last {
		last[i] = -1
	}
	for time.Now().Before(deadline) {
		settled := true
		for i, c := range clients {
			last[i] = c.sdkRevision()
			if last[i] < want || last[i] != last[0] {
				settled = false
			}
		}
		if settled {
			return last[0], time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replicas never settled on a common revision >= %d within %s (last=%v replicas=%v)",
		want, timeout, last, bases(clients))
	return 0, 0
}

// waitFullSnapshot waits until c serves an HTTP 200 snapshot at
// manifestRevision >= wantRev, riding out the per-document sync window
// (transport/5xx are transient and retried; a 200 below wantRev is lag and
// also retried), then returns the served revision and flags for HARD
// assertions. Non-transient errors fail immediately; so does the timeout —
// the wait is bounded, never unconditional.
func waitFullSnapshot(t *testing.T, c *client, wantRev int64, timeout time.Duration) (int64, map[string]map[string]any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		rev, flags, err := c.sdkSnapshotStrict()
		switch {
		case err == nil && rev >= wantRev:
			return rev, flags
		case err == nil:
			lastErr = fmt.Errorf("served snapshot at r%d < r%d", rev, wantRev)
		case isTransient(err):
			lastErr = err
		default:
			t.Fatalf("snapshot from %s: %v", c.base, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replica %s never served a complete snapshot at r>= %d within %s (last: %v; revision now %d)",
		c.base, wantRev, timeout, lastErr, c.sdkRevision())
	return 0, nil
}

// waitEvalAgreement waits until every replica returns a SUCCESSFUL eval for
// every unit — transient convergence-window errors (transport/5xx) are
// retried inside the bounded timeout — then hard-asserts cross-replica
// agreement: two successful evals that differ after the revision wait are a
// real inconsistency and fail immediately (never retried away).
func waitEvalAgreement(t *testing.T, flagKey string, units []string, timeout time.Duration, clients ...*client) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	got := make([]string, len(clients))
	for _, u := range units {
		for {
			var lastErr error
			ok := true
			for i, c := range clients {
				v, err := c.sdkEvalStrict(flagKey, u)
				if err != nil {
					lastErr = err
					if !isTransient(err) {
						t.Fatalf("eval(%s) on %s: %v", u, c.base, err)
					}
					ok = false
					break
				}
				got[i] = v
			}
			if ok {
				for i := 1; i < len(got); i++ {
					if got[i] != got[0] {
						t.Fatalf("replicas disagree on eval(%s) after convergence: %s=%q %s=%q",
							u, clients[0].base, got[0], clients[i].base, got[i])
					}
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("eval(%s) never converged within %s (last: %v)", u, timeout, lastErr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// captureFingerprint evaluates flagKey for every unit on c and returns the
// unit→variation map once EVERY eval succeeded; transient convergence-window
// errors are retried inside the bounded timeout so a fingerprint can never
// record a "" born from a failed HTTP call.
func captureFingerprint(t *testing.T, c *client, flagKey string, units []string, timeout time.Duration) map[string]string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	out := make(map[string]string, len(units))
	for _, u := range units {
		for {
			v, err := c.sdkEvalStrict(flagKey, u)
			if err == nil {
				out[u] = v
				break
			}
			if !isTransient(err) || time.Now().After(deadline) {
				t.Fatalf("fingerprint eval(%s) on %s: %v", u, c.base, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return out
}

// waitEvalMatch waits until c returns a SUCCESSFUL eval for every wanted
// unit (transient errors retried inside the bounded timeout), hard-comparing
// each against the expected value: two successful evals that differ are a
// real divergence and are never retried away.
func waitEvalMatch(t *testing.T, c *client, flagKey string, want map[string]string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	units := make([]string, 0, len(want))
	for u := range want {
		units = append(units, u)
	}
	sort.Strings(units)
	mismatch := 0
	for _, u := range units {
		for {
			got, err := c.sdkEvalStrict(flagKey, u)
			if err == nil {
				if got != want[u] {
					mismatch++
					if mismatch <= 5 {
						t.Logf("eval(%s) on %s = %q, want %q", u, c.base, got, want[u])
					}
				}
				break
			}
			if !isTransient(err) || time.Now().After(deadline) {
				t.Fatalf("eval(%s) on %s never succeeded within %s (last: %v)", u, c.base, timeout, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if mismatch != 0 {
		t.Fatalf("%d/%d evaluations diverged on %s", mismatch, len(want), c.base)
	}
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
	var reviewerCT struct{ Secret string }
	if code := adminA.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewerCT); code != http.StatusCreated {
		t.Fatalf("create reviewer token: %d", code)
	}
	adminA.reviewer = reviewerCT.Secret
	reviewerA := &client{t: t, base: A.base, token: reviewerCT.Secret}
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

	// B must already serve r1 (loaded/synced at boot); reconcile guarantees
	// it. revA is the store truth from the publish response, and the wait
	// covers BOTH replicas (see the convergence-wait contract in the header).
	waitRevision(t, revA, 15*time.Second, sdkA, sdkB)
	t.Logf("B serves manifest r%d at join", sdkB.sdkRevision())

	// lastPublishRev carries the store-side truth (the manifestRevision of
	// the most recent publish response) across subtests — the wait target
	// is never sampled from a replica's possibly-lagging served view.
	var lastPublishRev int64

	// =====================================================================
	// 1. 发布收敛 (Redis up): publish r2 on A → visible on B via gonacos
	//    Redis pub/sub. Measured against the 方案 5s cluster-convergence goal.
	// =====================================================================
	t.Run("ConvergenceRedisUp", func(t *testing.T) {
		start := time.Now()
		rev2, st := adminA.publishFlag(splitFlag("feature", 60)) // new revision of the flag
		if st != 200 {
			t.Fatalf("publish r2 on A: %d", st)
		}
		// rev2 (publish response) is the store truth; wait for BOTH replicas.
		// A serves its own publish synchronously, so the measured latency is
		// B's convergence — what the 5s cluster goal is about.
		lat := waitRevision(t, rev2, 5*time.Second, sdkA, sdkB)
		t.Logf("1 PASS: publish on A → visible on B in %s (manifest r%d); gonacos Redis pub/sub fast path (< 5s goal, wall since publish-start %s)",
			lat, rev2, time.Since(start))
		if lat >= 5*time.Second {
			t.Fatalf("cluster convergence %s exceeded the 5s goal", lat)
		}
		// Same-key evaluations agree across replicas (one cluster, one truth).
		// Transient convergence-window 5xx (per-document sync still in
		// flight) are retried inside the bounded wait; two SUCCESSFUL evals
		// that disagree fail immediately.
		waitEvalAgreement(t, "feature", []string{"u1", "u2", "u3", "u7", "u42"}, 15*time.Second, sdkA, sdkB)
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
		rev3, st := adminA.publishFlag(splitFlag("feature", 75))
		if st != 200 {
			// Redis being down must not break the publish path (PG is the
			// durable store; gonacos sync is best-effort).
			rdb.StartAgain(t)
			t.Fatalf("publish on A during Redis outage: %d", st)
		}
		if rev3 <= beforeA {
			rdb.StartAgain(t)
			t.Fatalf("store truth r%d did not advance past the pre-outage served r%d", rev3, beforeA)
		}
		t.Logf("2: A advanced to manifest r%d during the outage", rev3)

		// B converges via its reconciler reloading the shared PG truth and
		// republishing into B's gonacos — independent of Redis.
		lat := waitRevision(t, rev3, 30*time.Second, sdkA, sdkB)
		t.Logf("2 PASS: B converged to manifest r%d via reconciler+PostgreSQL while Redis was DOWN (%s)", rev3, lat)

		// Restart Redis; the cluster must survive and stay consistent.
		rdb.StartAgain(t)
		t.Logf("2: Redis restarted; verifying continued consistency")
		// A subsequent publish propagates through BOTH paths again.
		rev4, st := adminA.publishFlag(splitFlag("feature", 25))
		if st != 200 {
			t.Fatalf("publish after Redis restart: %d", st)
		}
		waitRevision(t, rev4, 15*time.Second, sdkA, sdkB)
		// Anchor recorded as soon as the revision converged — later failures
		// in this subtest must not degrade subtests 3/4 to a floor of 0.
		lastPublishRev = rev4
		waitEvalAgreement(t, "feature", []string{"u1", "u5", "u9"}, 15*time.Second, sdkA, sdkB)
		t.Logf("2 PASS: after Redis restart, publish r%d converged on B and evaluations agree", rev4)
	})

	// =====================================================================
	// 3. 节点重启后快照一致: restart B; it must re-converge to A's exact
	//    published state (manifest revision + per-key evaluation).
	// =====================================================================
	t.Run("RestartSnapshotConsistency", func(t *testing.T) {
		wantA := lastPublishRev // store truth: subtest 2's final publish response
		// Capture A's evaluation fingerprint across many keys BEFORE restart.
		// captureFingerprint only records SUCCESSFUL evals (a transient 5xx
		// during the sync window is retried, never frozen into the
		// fingerprint as "").
		units := make([]string, 60)
		for i := range units {
			units[i] = fmt.Sprintf("restart-user-%d", i)
		}
		fingerprint := captureFingerprint(t, sdkA, "feature", units, 15*time.Second)
		t.Logf("3: restarting replica B (store truth r%d)", wantA)
		B.restart(t)
		sdkB2 := &client{t: t, base: B.base, token: sdkToken}

		lat := waitRevision(t, wantA, 30*time.Second, sdkA, sdkB2)
		t.Logf("3: B re-converged to manifest r%d %s after restart", wantA, lat)

		// Bounded retry covers the post-restart document-sync window (5xx);
		// a SUCCESSFUL eval that differs from the fingerprint is a real
		// divergence and fails immediately.
		waitEvalMatch(t, sdkB2, "feature", fingerprint, 30*time.Second)
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
			if code := adminA.do("POST", base+"/submit", map[string]any{"reason": "cluster publish"}, nil); code != 200 {
				t.Fatalf("submit %d: %d", i, code)
			}
			if code := reviewerA.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != 200 {
				t.Fatalf("approve %d: %d", i, code)
			}
		}
		adminB := &client{t: t, base: B.base, token: ownerToken}
		sdkBcur := &client{t: t, base: B.base, token: sdkToken}

		// Anchor: settle BOTH replicas on a common served revision >= the
		// last known store truth, so `base` below is deterministic (a
		// single-replica sample could lag and silently shift the expected
		// [base+1, base+n] window).
		base, _ := waitSettledRevision(t, lastPublishRev, 15*time.Second, sdkA, sdkBcur)
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
		// carries every concurrently published flag exactly once. The wait
		// target is the KNOWN true final revision base+n — the gap-free
		// check above proved the burst allocated exactly [base+1, base+n] —
		// never a sample of A's served view (publishes completed THROUGH B
		// only reach A on a later reload/sync tick, so a sample can be low
		// and a single-replica ">=" wait then passes instantly).
		trueFinal := base + int64(n)
		waitRevision(t, trueFinal, 30*time.Second, sdkA, sdkBcur)
		// Revision parity does NOT imply document completeness: gonacos
		// per-document pub/sub can land the manifest before every flag doc,
		// and the read path then correctly 5xx's (ErrIncompleteSnapshot)
		// until the docs arrive / the 1s reconciler repairs. Ride that
		// bounded window out, then assert hard on the successful snapshot:
		// a 200 at r >= trueFinal missing a key is manifest key loss (a
		// product defect) and fails immediately — it is never retried away.
		for _, c := range []*client{sdkA, sdkBcur} {
			rev, snap := waitFullSnapshot(t, c, trueFinal, 30*time.Second)
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("conc-%d", i)
				if _, ok := snap[key]; !ok {
					keys := make([]string, 0, len(snap))
					for k := range snap {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					t.Fatalf("snapshot on %s at r%d (true final r%d) is missing concurrently published flag %s; served keys=%v",
						c.base, rev, trueFinal, key, keys)
				}
			}
			if _, ok := snap["feature"]; !ok {
				t.Fatalf("snapshot on %s at r%d lost the pre-existing feature flag", c.base, rev)
			}
		}
		t.Logf("4 PASS: both replicas' final snapshots (r>= %d) carry all %d concurrent flags + the pre-existing flag", trueFinal, n)
	})

	// =====================================================================
	// 4b. Same-flag race across replicas: exactly one winner.
	// =====================================================================
	// revA4b/revB4b capture the store-side truth of the race: exactly one
	// call allocates a manifest revision (the winner); the loser gets 409/0.
	var revA4b, revB4b int64
	t.Run("SameFlagRaceOneWinner", func(t *testing.T) {
		fl := splitFlag("race-flag", 50)
		base := fmt.Sprintf("/api/v1/admin/workspaces/acme/environments/prod/flags/%s", fl.Key)
		if code := adminA.do("PUT", base+"/draft", map[string]any{"content": fl, "risk": "normal"}, nil); code != 200 {
			t.Fatalf("draft: %d", code)
		}
		if code := adminA.do("POST", base+"/submit", map[string]any{"reason": "cluster publish"}, nil); code != 200 {
			t.Fatalf("submit: %d", code)
		}
		if code := reviewerA.do("POST", base+"/approve", map[string]any{"note": "ok"}, nil); code != 200 {
			t.Fatalf("approve: %d", code)
		}
		adminB := &client{t: t, base: B.base, token: ownerToken}

		var wg sync.WaitGroup
		stA, stB := 0, 0
		wg.Add(2)
		go func() { defer wg.Done(); revA4b, stA = adminB.publishFlagApproved(fl) }()
		go func() { defer wg.Done(); revB4b, stB = adminA.publishFlagApproved(fl) }()
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
		// The winner's revision is the final sweep's store-side truth floor —
		// a 200 that carried no manifestRevision would silently degrade that
		// floor to 0, so refuse it here.
		if max(revA4b, revB4b) == 0 {
			t.Fatalf("winner's publish response carried no manifestRevision (revB-call=%d revA-call=%d, statuses %d/%d)",
				revA4b, revB4b, stA, stB)
		}
		t.Logf("4b PASS: concurrent same-flag publish on A and B → exactly one 200, the other 409 (draft already consumed; A=%d B=%d)",
			stA, stB)
	})

	// Final consistency sweep: both replicas settle on the SAME revision —
	// the store truth allocated by the 4b winner, never a sample of A's
	// served view — and agree on per-key evaluation. Transient
	// convergence-window 5xx (per-document sync still completing) are
	// retried inside the bounded wait; two SUCCESSFUL evals that disagree
	// fail immediately.
	trueFinal4b := max(revA4b, revB4b)
	sdkBfin := &client{t: t, base: B.base, token: sdkToken}
	settledRev, lat := waitSettledRevision(t, trueFinal4b, 30*time.Second, sdkA, sdkBfin)
	waitEvalAgreement(t, "feature", []string{"u1", "u2", "u3", "final-1", "final-2"}, 30*time.Second, sdkA, sdkBfin)
	t.Logf("=== FINAL: A and B consistent at manifest r%d (settled in %s; 4b store truth r%d) ===",
		settledRev, lat, trueFinal4b)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
