// Proxy end-to-end acceptance: a standalone proxy relay in front of the real
// GoLab API server (real HTTP, real gonacos substrate). Covers the P4 proxy
// deployment shape: remote evaluation with server-side exposure capture,
// conversion collection, SSE revision fan-out, credential passthrough and
// audience enforcement — all through the relay (docs/技术方案.md §3, §8).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godeps/grayling/internal/platform"
	"github.com/godeps/grayling/internal/proxy"
	"github.com/godeps/grayling/pkg/model"
)

func newProxyFront(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	p, err := proxy.New(proxy.Config{
		Upstream:       h.srv.URL,
		RequestTimeout: 5 * time.Second,
		HealthTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(func() {
		srv.Close()
		p.CloseIdleConnections()
	})
	return srv
}

func TestE2EProxyRemoteEvalAndCollect(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()

	flag := testFlag("proxied")
	flag.Variations = map[string]any{"control": "c", "treatment": "t"}
	flag.Targeting = []model.Rule{
		{Name: "randomized", Percentage: map[string]float64{"control": 50, "treatment": 50}},
	}
	h.publishFlag(t, flag, platform.RiskNormal, h.owner)

	// Register + run an experiment so remote evals capture exposures.
	var exp platform.Experiment
	expReq := map[string]any{
		"flagKey": "proxied", "unit": "user_id",
		"variants":    []string{"control", "treatment"},
		"proportions": map[string]float64{"control": 50, "treatment": 50},
		"primaryMetric": map[string]any{
			"key": "conversion", "type": "proportion", "direction": "increase",
		},
		"alpha": 0.05, "power": 0.8, "mde": 0.02, "minSamplePerArm": 10,
		"analysisWindow": int64(7 * 24 * time.Hour),
	}
	if code := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/experiments",
		h.owner, expReq, &exp); code != http.StatusCreated {
		t.Fatalf("create experiment: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/review",
		h.owner, map[string]any{"note": "pre"}, nil); code != http.StatusOK {
		t.Fatalf("review: %d", code)
	}
	if code := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/bind",
		h.owner, nil, nil); code != http.StatusOK {
		t.Fatalf("bind: %d", code)
	}
	var pubCT platform.CreatedToken
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/tokens", h.owner,
		map[string]any{"name": "publisher-b", "kind": "admin", "role": "publisher"}, &pubCT); c != http.StatusCreated {
		t.Fatalf("publisher token: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/proxied/approve",
		pubCT.Secret, map[string]any{"note": "2nd"}, nil); c != http.StatusOK {
		t.Fatalf("approve: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/workspaces/acme/environments/prod/flags/proxied/publish",
		h.owner, map[string]any{"note": "bind"}, nil); c != http.StatusOK {
		t.Fatalf("publish binding: %d", c)
	}
	if c := h.do("POST", "/api/v1/admin/experiments/"+exp.ID+"/transition",
		h.owner, map[string]any{"to": "running"}, nil); c != http.StatusOK {
		t.Fatalf("run: %d", c)
	}

	px := newProxyFront(t, h)

	// Readiness must reflect the live upstream.
	rdy, err := http.Get(px.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("proxy health: %v", err)
	}
	rdyBody, _ := io.ReadAll(rdy.Body)
	rdy.Body.Close()
	if rdy.StatusCode != http.StatusOK || !strings.Contains(string(rdyBody), `"status":"ok"`) {
		t.Fatalf("proxy readiness: %d %s", rdy.StatusCode, rdyBody)
	}

	// Remote evaluation through the proxy, with an SDK token.
	evalBody, _ := json.Marshal(map[string]any{
		"flagKey": "proxied",
		"context": map[string]any{
			"targetingKey": "u1",
			"attributes":   map[string]any{"user_id": "u1"},
		},
	})
	req, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval", bytes.NewReader(evalBody))
	req.Header.Set("Authorization", "Bearer "+h.sdkTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("eval via proxy: %v", err)
	}
	var evalOut struct {
		Result     model.Result `json:"result"`
		Value      string       `json:"value"`
		ExposureID string       `json:"exposureEventId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&evalOut); err != nil {
		resp.Body.Close()
		t.Fatalf("eval decode: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || evalOut.Value == "" || evalOut.Value == "?" {
		t.Fatalf("eval via proxy: %d %+v", resp.StatusCode, evalOut)
	}
	if evalOut.Result.Experiment == nil || evalOut.ExposureID == "" {
		t.Fatalf("server-side exposure capture must work through the proxy: %+v", evalOut)
	}
	if h.store.EventCount() == 0 {
		t.Fatal("exposure from proxied eval never reached the store")
	}

	// Conversion collection through the proxy with the collect token.
	collectBody, _ := json.Marshal(map[string]any{"events": []map[string]any{{
		"eventId": "px-conv-1", "type": "conversion",
		"experimentId": exp.ID, "epoch": exp.Epoch,
		"flagKey": "proxied", "variant": evalOut.Result.Experiment.Variant,
		"unit": "u1", "metricKey": "conversion", "value": 1,
		"clientTime": time.Now().UTC().Format(time.RFC3339Nano),
	}}})
	creq, _ := http.NewRequest("POST", px.URL+"/api/v1/collect", bytes.NewReader(collectBody))
	creq.Header.Set("Authorization", "Bearer "+h.colTok)
	creq.Header.Set("Content-Type", "application/json")
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatalf("collect via proxy: %v", err)
	}
	var cres map[string]any
	_ = json.NewDecoder(cresp.Body).Decode(&cres)
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK || cres["accepted"] != float64(1) {
		t.Fatalf("collect via proxy: %d %v", cresp.StatusCode, cres)
	}

	// Audience separation still holds through the relay: an admin token on
	// eval must be rejected by the upstream with 403.
	areq, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval", bytes.NewReader(evalBody))
	areq.Header.Set("Authorization", "Bearer "+h.owner)
	areq.Header.Set("Content-Type", "application/json")
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatalf("admin eval via proxy: %v", err)
	}
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin token on proxied eval: %d, want 403", aresp.StatusCode)
	}
}

func TestE2EProxySSERelay(t *testing.T) {
	h := newHarness(t)
	h.setupWorkspace()
	h.publishFlag(t, testFlag("sse-flag"), platform.RiskNormal, h.owner)
	px := newProxyFront(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", px.URL+"/api/v1/sdk/stream", nil)
	req.Header.Set("Authorization", "Bearer "+h.sdkTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream via proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// The hub replays the current revision on connect — the first event must
	// arrive through the relay immediately.
	maxSeen := readMaxRevision(ctx, t, resp.Body, 0)
	if maxSeen < 1 {
		t.Fatalf("initial revision replay missing (max=%d)", maxSeen)
	}

	// Publishing a new flag revision must fan out through the proxy relay.
	h.publishFlag(t, testFlag("sse-flag-2"), platform.RiskNormal, h.owner)
	if got := readMaxRevision(ctx, t, resp.Body, maxSeen); got <= maxSeen {
		t.Fatalf("revision event never relayed: stuck at %d", got)
	}
}

// readMaxRevision consumes SSE lines until an event revision greater than
// `after` arrives (or the initial replay when after == 0), returning the max
// revision seen. Fails the test on timeout.
func readMaxRevision(ctx context.Context, t *testing.T, body io.Reader, after int64) int64 {
	t.Helper()
	type result struct {
		rev int64
		err error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(body)
		max := after
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "id: ") {
				continue
			}
			rev, err := strconv.ParseInt(strings.TrimSpace(line[4:]), 10, 64)
			if err != nil {
				continue
			}
			if rev > max {
				max = rev
			}
			if max > after {
				ch <- result{rev: max}
				return
			}
		}
		ch <- result{err: sc.Err()}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("stream ended: %v", r.err)
		}
		return r.rev
	case <-ctx.Done():
		t.Fatalf("timed out waiting for revision > %d", after)
		return 0
	}
}
