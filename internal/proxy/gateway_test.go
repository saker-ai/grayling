package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// evalUpstream mimics the GoLab /api/v1/sdk/eval contract and records what
// the gateway endpoint sent.
type evalUpstream struct {
	t        *testing.T
	hits     atomic.Int64
	lastBody atomic.Value // string
	lastAuth atomic.Value // string
	// responder produces the upstream reply; nil = canned success.
	responder func(w http.ResponseWriter)
	variant   string
	reason    string
}

func newEvalUpstream(t *testing.T, variant, reason string) (*evalUpstream, *httptest.Server) {
	u := &evalUpstream{t: t, variant: variant, reason: reason}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if r.URL.Path != "/api/v1/sdk/eval" || r.Method != http.MethodPost {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		u.lastBody.Store(string(body))
		u.lastAuth.Store(r.Header.Get("Authorization"))
		if u.responder != nil {
			u.responder(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"variationType": u.variant,
				"reason":        u.reason,
				"failed":        false,
			},
			"value": "v", "manifestRevision": 7,
		})
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

const gwPath = "/api/v1/gateway/forwardauth?flagKey=checkout-router&defaultVariant=stable&unitField=user_id"

func TestGatewayForwardAuthHappyPath(t *testing.T) {
	up, srv := newEvalUpstream(t, "canary", "SPLIT")
	px := newTestProxy(t, srv.URL, func(c *Config) {
		c.Gateway = GatewayConfig{Token: "glab_sdk_test", EvalTimeout: 2 * time.Second}
	})

	req, _ := http.NewRequest("GET", px.URL+gwPath, nil)
	req.Header.Set("X-Bucketing-Key", "user-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forwardauth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Golab-Variant"); got != "canary" {
		t.Fatalf("X-Golab-Variant = %q, want canary", got)
	}
	if got := resp.Header.Get("X-Golab-Reason"); got != "SPLIT" {
		t.Fatalf("X-Golab-Reason = %q", got)
	}
	if got := resp.Header.Get("X-Golab-Flag-Key"); got != "checkout-router" {
		t.Fatalf("X-Golab-Flag-Key = %q", got)
	}
	if got := resp.Header.Get("X-Golab-Fail-Open"); got != "" {
		t.Fatalf("healthy evaluation must not set X-Golab-Fail-Open: %q", got)
	}

	// The upstream call must carry the proxy's own SDK credential and the
	// bucketing key in BOTH targetingKey and the configured unit field.
	if auth, _ := up.lastAuth.Load().(string); auth != "Bearer glab_sdk_test" {
		t.Fatalf("upstream auth = %q", auth)
	}
	body, _ := up.lastBody.Load().(string)
	var payload struct {
		FlagKey string `json:"flagKey"`
		Context struct {
			TargetingKey string         `json:"targetingKey"`
			Attributes   map[string]any `json:"attributes"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, body)
	}
	if payload.FlagKey != "checkout-router" || payload.Context.TargetingKey != "user-42" ||
		payload.Context.Attributes["user_id"] != "user-42" {
		t.Fatalf("upstream payload: %s", body)
	}
}

func TestGatewayForwardAuthTargetingKeyOnly(t *testing.T) {
	up, srv := newEvalUpstream(t, "canary", "SPLIT")
	px := newTestProxy(t, srv.URL, func(c *Config) {
		c.Gateway = GatewayConfig{Token: "tok"}
	})
	resp, err := http.Get(px.URL +
		"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=d&unitField=targetingKey")
	if err != nil {
		t.Fatalf("get (no bucketing header): %v", err)
	}
	resp.Body.Close()
	// unitField=targetingKey + no unit header → fail-open, upstream untouched.
	if resp.Header.Get("X-Golab-Fail-Open") != "missing_bucket_key" {
		t.Fatalf("fail-open reason: %q", resp.Header.Get("X-Golab-Fail-Open"))
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream must not be called without a bucketing key")
	}

	req, _ := http.NewRequest("GET", px.URL+
		"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=d&unitField=targetingKey", nil)
	req.Header.Set("X-Bucketing-Key", "u1")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp2.Body.Close()
	body, _ := up.lastBody.Load().(string)
	var payload struct {
		Context struct {
			TargetingKey string         `json:"targetingKey"`
			Attributes   map[string]any `json:"attributes"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, body)
	}
	// The unit lives in targetingKey ONLY — it must not be duplicated into
	// attributes under the unitField name. (attributes carries the reserved
	// client_ip injection, which is unrelated to the bucketing key.)
	if _, dup := payload.Context.Attributes["targetingKey"]; dup {
		t.Fatalf("targetingKey unit field must not duplicate into attributes: %s", body)
	}
	if len(payload.Context.Attributes) != 1 || payload.Context.Attributes["client_ip"] == nil {
		t.Fatalf("attributes must carry ONLY the derived client_ip: %s", body)
	}
}

func TestGatewayFailOpenMatrix(t *testing.T) {
	// Every internal failure must answer 200 with the default variant and
	// an X-Golab-Fail-Open reason — never a 4xx/5xx (the gateway would
	// reject or 5xx the client request).
	cases := []struct {
		name       string
		token      string
		query      string
		unit       string
		responder  func(w http.ResponseWriter)
		wantReason string
		wantHits   int64
	}{
		{
			name: "no flagKey", token: "tok",
			query:      "?defaultVariant=stable",
			unit:       "u1",
			wantReason: "bad_config_no_flag_key",
		},
		{
			name: "not configured", token: "",
			query:      "?flagKey=f&defaultVariant=stable",
			unit:       "u1",
			wantReason: "not_configured",
		},
		{
			name: "missing bucketing key", token: "tok",
			query:      "?flagKey=f&defaultVariant=stable",
			unit:       "",
			wantReason: "missing_bucket_key",
		},
		{
			name: "upstream http error", token: "tok",
			query:      "?flagKey=f&defaultVariant=stable",
			unit:       "u1",
			responder:  func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
			wantReason: "upstream_http_error",
			wantHits:   1,
		},
		{
			name: "flag not found", token: "tok",
			query: "?flagKey=ghost&defaultVariant=stable",
			unit:  "u1",
			responder: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"failed":true,"errorCode":"FLAG_NOT_FOUND","reason":"ERROR"},"value":null}`))
			},
			wantReason: "flag_not_found",
			wantHits:   1,
		},
		{
			name: "malformed upstream body", token: "tok",
			query: "?flagKey=f&defaultVariant=stable",
			unit:  "u1",
			responder: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`not json`))
			},
			wantReason: "upstream_unreachable",
			wantHits:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, srv := newEvalUpstream(t, "", "")
			up.responder = tc.responder
			px := newTestProxy(t, srv.URL, func(c *Config) {
				c.Gateway = GatewayConfig{Token: tc.token, EvalTimeout: time.Second}
			})
			req, _ := http.NewRequest("GET", px.URL+"/api/v1/gateway/forwardauth"+tc.query, nil)
			if tc.unit != "" {
				req.Header.Set("X-Bucketing-Key", tc.unit)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("forwardauth: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("fail-open must answer 200, got %d", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Golab-Fail-Open"); got != tc.wantReason {
				t.Fatalf("X-Golab-Fail-Open = %q, want %q", got, tc.wantReason)
			}
			// Every matrix case configures defaultVariant=stable; the
			// fail-open answer must carry it.
			if got := resp.Header.Get("X-Golab-Variant"); got != "stable" {
				t.Fatalf("fail-open variant = %q, want stable", got)
			}
			if up.hits.Load() != tc.wantHits {
				t.Fatalf("upstream hits = %d, want %d", up.hits.Load(), tc.wantHits)
			}
		})
	}
}

func TestGatewayFailOpenUpstreamDown(t *testing.T) {
	// Upstream unreachable (real closed port) → fail-open, not 5xx.
	_, dead := newEvalUpstream(t, "", "")
	deadURL := dead.URL
	dead.Close() // the eval service keeps running while GoLab is down
	px := newTestProxy(t, deadURL, func(c *Config) {
		c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: time.Second}
	})
	req, _ := http.NewRequest("GET", px.URL+
		"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=stable", nil)
	req.Header.Set("X-Bucketing-Key", "u1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forwardauth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Golab-Fail-Open"); got != "upstream_unreachable" {
		t.Fatalf("fail-open reason = %q", got)
	}
	if got := resp.Header.Get("X-Golab-Variant"); got != "stable" {
		t.Fatalf("variant = %q, want stable", got)
	}
}

func TestGatewayFailOpenTimeout(t *testing.T) {
	up, srv := newEvalUpstream(t, "", "")
	up.responder = func(w http.ResponseWriter) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}
	px := newTestProxy(t, srv.URL, func(c *Config) {
		c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: 50 * time.Millisecond}
	})
	req, _ := http.NewRequest("GET", px.URL+
		"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=stable", nil)
	req.Header.Set("X-Bucketing-Key", "u1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forwardauth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Golab-Fail-Open"); got != "upstream_timeout" {
		t.Fatalf("fail-open reason = %q, want upstream_timeout", got)
	}
}

func TestGatewaySecretEnforcement(t *testing.T) {
	// With a shared secret configured, requests without the matching secret
	// must NEVER be evaluated upstream (no flag-probing oracle, no exposure
	// pollution through the proxy's own credential) — they fail-open to the
	// static default like any other refusal.
	up, srv := newEvalUpstream(t, "canary", "SPLIT")
	px := newTestProxy(t, srv.URL, func(c *Config) {
		c.Gateway = GatewayConfig{Token: "tok", Secret: "s3cr3t", EvalTimeout: time.Second}
	})
	base := "/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=stable"

	for _, tc := range []struct{ name, query string }{
		{"missing secret", base},
		{"wrong secret", base + "&secret=wrong"},
		{"prefix secret", base + "&secret=s3cr3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hitsBefore := up.hits.Load()
			req, _ := http.NewRequest("GET", px.URL+tc.query, nil)
			req.Header.Set("X-Bucketing-Key", "u1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("forwardauth: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (fail-open invariant)", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Golab-Fail-Open"); got != "unauthorized" {
				t.Fatalf("fail-open reason = %q, want unauthorized", got)
			}
			if got := resp.Header.Get("X-Golab-Variant"); got != "stable" {
				t.Fatalf("variant = %q, want the static default stable", got)
			}
			if up.hits.Load() != hitsBefore {
				t.Fatal("upstream MUST NOT be evaluated without the correct secret")
			}
		})
	}

	t.Run("correct secret evaluates", func(t *testing.T) {
		req, _ := http.NewRequest("GET", px.URL+base+"&secret=s3cr3t", nil)
		req.Header.Set("X-Bucketing-Key", "u1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forwardauth: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Golab-Variant"); got != "canary" {
			t.Fatalf("variant = %q, want the evaluated canary", got)
		}
		if got := resp.Header.Get("X-Golab-Fail-Open"); got != "" {
			t.Fatalf("authorized evaluation must not fail open: %q", got)
		}
		if up.hits.Load() == 0 {
			t.Fatal("upstream was not evaluated for the correct secret")
		}
	})
}
