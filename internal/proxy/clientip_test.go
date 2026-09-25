package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Config parsing
// ---------------------------------------------------------------------------

func TestParseTrustedProxyCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		wantLen int
	}{
		{"empty trusts nothing", "", 0},
		{"whitespace only", "   ", 0},
		{"single cidr", "10.0.0.0/8", 1},
		{"bare ip becomes host prefix", "127.0.0.1", 1},
		{"mixed with spaces", " 10.0.0.0/8 , 172.16.5.5 , fd00::/8 ", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTrustedProxyCIDRs(tc.spec)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.spec, err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("parse %q → %d prefixes, want %d", tc.spec, len(got), tc.wantLen)
			}
		})
	}
	for _, bad := range []string{"10.0.0.0/33", "bogus", "10.0.0.0/8,bogus", "999.1.1.1"} {
		if _, err := parseTrustedProxyCIDRs(bad); err == nil {
			t.Fatalf("parse %q: expected error, got nil", bad)
		}
	}
	// New() must reject an invalid set at startup (fail fast, not per-request).
	if _, err := New(Config{Upstream: "http://127.0.0.1:1", TrustedProxyCIDRs: "10.0.0.0/33"}); err == nil {
		t.Fatal("New accepted invalid TrustedProxyCIDRs")
	}
}

// ---------------------------------------------------------------------------
// Derivation walk: RemoteAddr × multi-level XFF × trusted CIDRs × forgery
// ---------------------------------------------------------------------------

func TestDeriveClientIP(t *testing.T) {
	cases := []struct {
		name       string
		cfg        string
		remoteAddr string
		xff        []string // X-Forwarded-For header line(s); joined like a chain
		want       string
	}{
		{
			name: "no config: XFF never trusted, peer only", cfg: "",
			remoteAddr: "93.184.216.34:5555", xff: []string{"10.0.0.1, 127.0.0.1"},
			want: "93.184.216.34",
		},
		{
			name: "no config, no XFF", cfg: "",
			remoteAddr: "93.184.216.34:5555", want: "93.184.216.34",
		},
		{
			name: "untrusted peer: XFF discarded entirely", cfg: "10.0.0.0/8",
			remoteAddr: "93.184.216.34:1", xff: []string{"10.1.1.1"},
			want: "93.184.216.34",
		},
		{
			name: "one trusted hop", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"93.184.216.34"},
			want: "93.184.216.34",
		},
		{
			name: "one trusted hop, forged left entry discarded", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"10.9.9.9, 93.184.216.34"},
			want: "93.184.216.34",
		},
		{
			name: "two trusted hops", cfg: "10.0.0.0/8, 172.16.0.0/12",
			remoteAddr: "10.0.0.2:80", xff: []string{"203.0.113.7, 172.16.5.5"},
			want: "203.0.113.7",
		},
		{
			name: "two trusted hops, forged left entry discarded", cfg: "10.0.0.0/8, 172.16.0.0/12",
			remoteAddr: "10.0.0.2:80", xff: []string{"10.255.0.1, 203.0.113.7, 172.16.5.5"},
			want: "203.0.113.7",
		},
		{
			name: "entire chain trusted: leftmost entry is the client", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"10.1.2.3"},
			want: "10.1.2.3",
		},
		{
			name: "bare IP config entry", cfg: "127.0.0.1",
			remoteAddr: "127.0.0.1:4444", xff: []string{"10.1.2.3"},
			want: "10.1.2.3",
		},
		{
			name: "malformed entry at trust boundary fails closed", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"not-an-ip"},
			want: "",
		},
		{
			name: "malformed entry LEFT of the boundary is never consumed", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"garbage, 93.184.216.34"},
			want: "93.184.216.34",
		},
		{
			name: "ipv6 chain", cfg: "fd00::/8",
			remoteAddr: "[fd00::1]:80", xff: []string{"2001:db8::5"},
			want: "2001:db8::5",
		},
		{
			name: "ipv6 forged left entry discarded", cfg: "fd00::/8",
			remoteAddr: "[fd00::1]:80", xff: []string{"fd00::dead, 2001:db8::5"},
			want: "2001:db8::5",
		},
		{
			name: "v4-mapped XFF entry unmapped at boundary", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"::ffff:203.0.113.7"},
			want: "203.0.113.7",
		},
		{
			name: "v4-mapped peer unmapped for trust check", cfg: "10.0.0.0/8",
			remoteAddr: "[::ffff:10.0.0.2]:80", xff: []string{"203.0.113.7"},
			want: "203.0.113.7",
		},
		{
			name: "stray commas tolerated, not a trust break", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{" 203.0.113.7 , ,"},
			want: "203.0.113.7",
		},
		{
			name: "trusted peer without XFF", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80",
			want:       "10.0.0.2",
		},
		{
			name: "multiple XFF header lines are joined as one chain", cfg: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:80", xff: []string{"203.0.113.7", "10.0.0.9"},
			want: "203.0.113.7",
		},
		{
			name: "loopback-only trust (e2e shape): sanitized edge value used", cfg: "127.0.0.1/32",
			remoteAddr: "127.0.0.1:34567", xff: []string{"10.1.2.3"},
			want: "10.1.2.3",
		},
		{
			name: "loopback-only trust: forged internal prefix discarded", cfg: "127.0.0.1/32",
			remoteAddr: "127.0.0.1:34567", xff: []string{"10.9.9.9, 203.0.113.9"},
			want: "203.0.113.9",
		},
		{
			name: "unparseable RemoteAddr injects nothing", cfg: "10.0.0.0/8",
			remoteAddr: "unix-socket-junk", xff: []string{"10.1.2.3"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Config{Upstream: "http://127.0.0.1:1", TrustedProxyCIDRs: tc.cfg})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer p.CloseIdleConnections()
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			for _, line := range tc.xff {
				r.Header.Add("X-Forwarded-For", line)
			}
			if got := p.deriveClientIP(r); got != tc.want {
				t.Fatalf("deriveClientIP(remote=%q, xff=%q, cfg=%q) = %q, want %q",
					tc.remoteAddr, strings.Join(tc.xff, "|"), tc.cfg, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// forwardAuth injection
// ---------------------------------------------------------------------------

func TestGatewayClientIPInjection(t *testing.T) {
	t.Run("default config injects peer, forged XFF ignored", func(t *testing.T) {
		up, srv := newEvalUpstream(t, "canary", "SPLIT")
		px := newTestProxy(t, srv.URL, func(c *Config) {
			c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: time.Second}
		})
		req, _ := http.NewRequest("GET", px.URL+gwPath, nil)
		req.Header.Set("X-Bucketing-Key", "user-42")
		req.Header.Set("X-Forwarded-For", "10.1.2.3") // 不受信：必须被忽略
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forwardauth: %v", err)
		}
		resp.Body.Close()
		attrs := gatewayUpstreamAttrs(t, up)
		if attrs["client_ip"] != "127.0.0.1" {
			t.Fatalf("client_ip = %v, want the connection peer 127.0.0.1 (XFF untrusted by default)", attrs["client_ip"])
		}
		if attrs["user_id"] != "user-42" {
			t.Fatalf("unit attribute lost: %v", attrs)
		}
	})

	t.Run("trusted edge: derived client, forged prefix discarded", func(t *testing.T) {
		up, srv := newEvalUpstream(t, "canary", "SPLIT")
		px := newTestProxy(t, srv.URL, func(c *Config) {
			c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: time.Second}
			c.TrustedProxyCIDRs = "127.0.0.1/32"
		})
		for _, tc := range []struct{ name, xff, want string }{
			{"sanitized edge value", "203.0.113.7", "203.0.113.7"},
			{"forged internal prefix", "10.9.9.9, 203.0.113.7", "203.0.113.7"},
			{"forged only", "10.9.9.9", "10.9.9.9"}, // 边界=最后一跳记录的对端，值本身仍按 flag 规则判定
		} {
			t.Run(tc.name, func(t *testing.T) {
				req, _ := http.NewRequest("GET", px.URL+gwPath, nil)
				req.Header.Set("X-Bucketing-Key", "user-42")
				req.Header.Set("X-Forwarded-For", tc.xff)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("forwardauth: %v", err)
				}
				resp.Body.Close()
				attrs := gatewayUpstreamAttrs(t, up)
				if attrs["client_ip"] != tc.want {
					t.Fatalf("xff=%q → client_ip = %v, want %q", tc.xff, attrs["client_ip"], tc.want)
				}
			})
		}
	})

	t.Run("unitField=client_ip buckets on the derived IP, header ignored", func(t *testing.T) {
		up, srv := newEvalUpstream(t, "canary", "SPLIT")
		px := newTestProxy(t, srv.URL, func(c *Config) {
			c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: time.Second}
			c.TrustedProxyCIDRs = "127.0.0.1/32"
		})
		req, _ := http.NewRequest("GET", px.URL+
			"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=stable&unitField=client_ip", nil)
		req.Header.Set("X-Bucketing-Key", "attacker-picked-group") // 必须被忽略
		req.Header.Set("X-Forwarded-For", "10.1.2.3")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forwardauth: %v", err)
		}
		resp.Body.Close()
		body, _ := up.lastBody.Load().(string)
		if strings.Contains(body, "attacker-picked-group") {
			t.Fatalf("header-supplied unit value must NEVER reach the eval context: %s", body)
		}
		var payload struct {
			Context struct {
				TargetingKey string         `json:"targetingKey"`
				Attributes   map[string]any `json:"attributes"`
			} `json:"context"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("upstream body: %v (%s)", err, body)
		}
		if payload.Context.TargetingKey != "10.1.2.3" || payload.Context.Attributes["client_ip"] != "10.1.2.3" {
			t.Fatalf("derived IP must be both unit and client_ip attribute: %s", body)
		}
	})

	t.Run("unitField=client_ip with broken chain fails open, no upstream call", func(t *testing.T) {
		up, srv := newEvalUpstream(t, "canary", "SPLIT")
		px := newTestProxy(t, srv.URL, func(c *Config) {
			c.Gateway = GatewayConfig{Token: "tok", EvalTimeout: time.Second}
			c.TrustedProxyCIDRs = "127.0.0.1/32"
		})
		hitsBefore := up.hits.Load()
		req, _ := http.NewRequest("GET", px.URL+
			"/api/v1/gateway/forwardauth?flagKey=f&defaultVariant=stable&unitField=client_ip", nil)
		req.Header.Set("X-Forwarded-For", "garbage-not-an-ip") // 信任链断裂 → fail closed
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forwardauth: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail-open invariant)", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Golab-Fail-Open"); got != "missing_bucket_key" {
			t.Fatalf("fail-open reason = %q, want missing_bucket_key", got)
		}
		if up.hits.Load() != hitsBefore {
			t.Fatal("upstream must not be evaluated when the trusted chain is broken")
		}
	})
}

// gatewayUpstreamAttrs decodes the attributes of the last upstream eval body.
func gatewayUpstreamAttrs(t *testing.T, up *evalUpstream) map[string]any {
	t.Helper()
	body, _ := up.lastBody.Load().(string)
	var payload struct {
		Context struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, body)
	}
	return payload.Context.Attributes
}

// ---------------------------------------------------------------------------
// /sdk/eval relay injection (server-side evaluation through the relay)
// ---------------------------------------------------------------------------

func newBodyCaptureUpstream(t *testing.T) (*atomic.Value, *httptest.Server) {
	var last atomic.Value
	last.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return &last, srv
}

func postEval(t *testing.T, pxURL, body, xff string) {
	t.Helper()
	req, _ := http.NewRequest("POST", pxURL+"/api/v1/sdk/eval", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("eval via proxy: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestEvalRelayClientIPInjection(t *testing.T) {
	t.Run("derived value injected when attribute absent", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, func(c *Config) { c.TrustedProxyCIDRs = "127.0.0.1/32" })
		postEval(t, px.URL,
			`{"flagKey":"f1","context":{"targetingKey":"u1","attributes":{"user_id":"u1"}}}`,
			"10.9.9.9, 203.0.113.7") // 伪造前缀必须被丢弃
		var relayed struct {
			FlagKey string `json:"flagKey"`
			Context struct {
				TargetingKey string         `json:"targetingKey"`
				Attributes   map[string]any `json:"attributes"`
			} `json:"context"`
		}
		body := last.Load().(string)
		if err := json.Unmarshal([]byte(body), &relayed); err != nil {
			t.Fatalf("relayed body: %v (%s)", err, body)
		}
		if relayed.FlagKey != "f1" || relayed.Context.TargetingKey != "u1" ||
			relayed.Context.Attributes["user_id"] != "u1" {
			t.Fatalf("caller fields must survive injection: %s", body)
		}
		if relayed.Context.Attributes["client_ip"] != "203.0.113.7" {
			t.Fatalf("client_ip = %v, want the trusted-chain derived 203.0.113.7: %s",
				relayed.Context.Attributes["client_ip"], body)
		}
	})

	t.Run("explicit caller value wins, body relayed verbatim", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, func(c *Config) { c.TrustedProxyCIDRs = "127.0.0.1/32" })
		orig := `{"flagKey":"f1","context":{"targetingKey":"u1","attributes":{"client_ip":"1.2.3.4"}}}`
		postEval(t, px.URL, orig, "10.1.2.3")
		if got := last.Load().(string); got != orig {
			t.Fatalf("explicit client_ip must never be overwritten (want verbatim relay):\n got %q\nwant %q", got, orig)
		}
	})

	t.Run("no trusted config: XFF ignored, peer injected", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, nil)
		postEval(t, px.URL, `{"flagKey":"f1","context":{"targetingKey":"u1"}}`, "10.1.2.3")
		body := last.Load().(string)
		if !strings.Contains(body, `"client_ip":"127.0.0.1"`) {
			t.Fatalf("peer-only injection expected: %s", body)
		}
		if strings.Contains(body, "10.1.2.3") {
			t.Fatalf("untrusted XFF must not leak into the eval context: %s", body)
		}
	})

	t.Run("non-JSON body relays verbatim", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, nil)
		postEval(t, px.URL, `not json at all`, "")
		if got := last.Load().(string); got != "not json at all" {
			t.Fatalf("non-JSON body must relay verbatim: %q", got)
		}
	})

	t.Run("big integers keep full precision through re-encoding", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, nil)
		postEval(t, px.URL,
			`{"flagKey":"f1","context":{"attributes":{"uid":12345678901234567890}}}`, "")
		body := last.Load().(string)
		if !strings.Contains(body, "12345678901234567890") {
			t.Fatalf("json.Number round-trip lost precision: %s", body)
		}
		if !strings.Contains(body, `"client_ip":"127.0.0.1"`) {
			t.Fatalf("injection missing: %s", body)
		}
	})

	t.Run("context absent is created", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, nil)
		postEval(t, px.URL, `{"flagKey":"f1"}`, "")
		body := last.Load().(string)
		var relayed struct {
			FlagKey string `json:"flagKey"`
			Context struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"context"`
		}
		if err := json.Unmarshal([]byte(body), &relayed); err != nil {
			t.Fatalf("relayed body: %v (%s)", err, body)
		}
		if relayed.FlagKey != "f1" || relayed.Context.Attributes["client_ip"] != "127.0.0.1" {
			t.Fatalf("context creation: %s", body)
		}
	})

	t.Run("non-eval relay paths are untouched", func(t *testing.T) {
		last, srv := newBodyCaptureUpstream(t)
		px := newTestProxy(t, srv.URL, nil)
		orig := `{"events":[{"eventId":"e1"}]}`
		req, _ := http.NewRequest("POST", px.URL+"/api/v1/collect", strings.NewReader(orig))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("collect via proxy: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := last.Load().(string); got != orig {
			t.Fatalf("collect body must relay verbatim: %q", got)
		}
	})
}
