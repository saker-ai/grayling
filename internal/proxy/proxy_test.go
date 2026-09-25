package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, upstream string, mutate func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{Upstream: upstream}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
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

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty upstream accepted")
	}
	if _, err := New(Config{Upstream: "ftp://x"}); err == nil {
		t.Fatal("non-HTTP scheme accepted")
	}
	if _, err := New(Config{Upstream: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestRelayEvalPassthrough(t *testing.T) {
	var gotAuth, gotBody, gotXFF, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"flagKey":"f1"}}`))
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	req, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval?debug=1",
		strings.NewReader(`{"flagKey":"f1"}`))
	req.Header.Set("Authorization", "Bearer glab_sdk_x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("eval via proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"result":{"flagKey":"f1"}}` {
		t.Fatalf("relay: %d %s", resp.StatusCode, body)
	}
	if gotAuth != "Bearer glab_sdk_x" {
		t.Fatalf("authorization must pass through untouched: %q", gotAuth)
	}
	// The eval body relays untouched EXCEPT for the reserved client_ip
	// injection (clientip.go): /sdk/eval is server-side evaluation, so the
	// derived client IP must travel inside the context. No trusted proxies
	// are configured here, so the derived value is the direct connection
	// peer — and any caller-sent X-Forwarded-For is ignored.
	var relayed struct {
		FlagKey string `json:"flagKey"`
		Context struct {
			TargetingKey string         `json:"targetingKey"`
			Attributes   map[string]any `json:"attributes"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(gotBody), &relayed); err != nil {
		t.Fatalf("relayed body is not JSON: %v (%q)", err, gotBody)
	}
	if relayed.FlagKey != "f1" {
		t.Fatalf("body relay must preserve caller fields: %q", gotBody)
	}
	if got := relayed.Context.Attributes["client_ip"]; got != "127.0.0.1" {
		t.Fatalf("client_ip injection: got %v, want the connection peer 127.0.0.1 (%q)", got, gotBody)
	}
	if len(relayed.Context.Attributes) != 1 {
		t.Fatalf("injection must add ONLY client_ip: %q", gotBody)
	}
	if gotQuery != "debug=1" {
		t.Fatalf("query relay: %q", gotQuery)
	}
	if !strings.Contains(gotXFF, "127.0.0.1") {
		t.Fatalf("X-Forwarded-For missing: %q", gotXFF)
	}
}

func TestRelayUpstreamStatuses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"wrong_audience","message":"sdk tokens only"}}`))
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	req, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "wrong_audience") {
		t.Fatalf("upstream error must pass through verbatim: %d %s", resp.StatusCode, body)
	}
}

func TestRelayUpstreamDown502(t *testing.T) {
	px := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) {
		c.DialTimeout = 500 * time.Millisecond
		c.RequestTimeout = 2 * time.Second
	})
	req, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "upstream_unreachable") {
		t.Fatalf("down upstream: %d %s", resp.StatusCode, body)
	}
}

func TestRelayUpstreamTimeout504(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, func(c *Config) { c.RequestTimeout = 200 * time.Millisecond })

	req, _ := http.NewRequest("POST", px.URL+"/api/v1/sdk/eval", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(body), "upstream_timeout") {
		t.Fatalf("timeout: %d %s", resp.StatusCode, body)
	}
}

func TestHealthEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","service":"golab"}`))
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	// Liveness answers even with a DEAD upstream.
	dead := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.DialTimeout = 300 * time.Millisecond })
	live, err := http.Get(dead.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Fatalf("healthz with dead upstream: %d", live.StatusCode)
	}
	// …but readiness must fail closed.
	rdy, err := http.Get(dead.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	rdy.Body.Close()
	if rdy.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness with dead upstream: %d", rdy.StatusCode)
	}

	// With a healthy upstream both answer 200 and readiness embeds upstream status.
	resp, err := http.Get(px.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"service":"golab"`) {
		t.Fatalf("readiness: %d %s", resp.StatusCode, body)
	}
}

func TestSSERelayFlushesIncrementally(t *testing.T) {
	var gotLastID, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLastID = r.Header.Get("Last-Event-ID")
		gotAuth = r.Header.Get("Authorization")
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "id: %d\nevent: revision\ndata: {\"revision\":%d}\n\n", i, i)
			flusher.Flush()
			time.Sleep(400 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, func(c *Config) { c.RequestTimeout = 3 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", px.URL+"/api/v1/sdk/stream", nil)
	req.Header.Set("Authorization", "Bearer glab_sdk_y")
	req.Header.Set("Last-Event-ID", "7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream via proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream headers: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if gotAuth != "Bearer glab_sdk_y" || gotLastID != "7" {
		t.Fatalf("stream header passthrough: auth=%q last-id=%q", gotAuth, gotLastID)
	}

	// Incremental delivery: event #1 must arrive long before the upstream
	// finished its 3×400ms stream — proof the relay flushes per chunk.
	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
	dataSeen := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			dataSeen++
			if dataSeen == 1 {
				if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
					t.Fatalf("first event relayed after %s — relay is buffering", elapsed)
				}
			}
			if dataSeen == 3 {
				break
			}
		}
	}
	if dataSeen != 3 {
		t.Fatalf("relayed %d events, want 3 (%v)", dataSeen, sc.Err())
	}
}

func TestConfigETagPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"golab-r5"` {
			w.Header().Set("ETag", `"golab-r5"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"golab-r5"`)
		_, _ = w.Write([]byte(`{"flags":{}}`))
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	req, _ := http.NewRequest("GET", px.URL+"/api/v1/sdk/config", nil)
	req.Header.Set("If-None-Match", `"golab-r5"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("config via proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified || resp.Header.Get("ETag") != `"golab-r5"` {
		t.Fatalf("304 passthrough: %d etag=%q", resp.StatusCode, resp.Header.Get("ETag"))
	}
}

func TestUnknownRouteNotRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for unrouted paths: %s", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	// Admin surface is deliberately NOT relayed: the proxy exposes only the
	// remote-evaluation endpoints.
	for _, path := range []string{"/api/v1/admin/workspaces", "/api/v1/sdk/ai/session", "/"} {
		resp, err := http.Get(px.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path %s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestRevisionRelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sdk/revision" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer glab_sdk_test" {
			t.Errorf("credential not passed through: %q", got)
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"revision":7,"etag":"\"golab-r7\""}`))
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, nil)

	req, _ := http.NewRequest("GET", px.URL+"/api/v1/sdk/revision", nil)
	req.Header.Set("Authorization", "Bearer glab_sdk_test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revision via proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"revision":7`) {
		t.Fatalf("revision relay: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control not passed through: %q", resp.Header.Get("Cache-Control"))
	}
}
