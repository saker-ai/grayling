// Client-IP gray routing end-to-end acceptance (docs/traefik-golab-architecture.md
// "按 client IP 灰度"): REAL cmd/golab + REAL cmd/golab-proxy processes, real
// in_cidr-targeted flag, requests through the proxy's forwardAuth evaluation
// endpoint and through the /sdk/eval relay.
//
// Topology note (self-consistent by design): every test connection arrives
// over loopback, so the loopback address plays the role of the sanitized
// edge (Traefik) — the proxy is started with -trusted-proxy-cidrs covering
// ONLY loopback, i.e. exactly one trusted hop. The rightmost XFF entry is
// therefore what "the edge recorded about its peer" (the real client), and
// anything a client prepends to its left must be discarded:
//
//	XFF "10.9.9.9, 203.0.113.9"  ⇒ derived client_ip = 203.0.113.9 (外网)
//	                               — the forged internal prefix changes nothing.
//
// Assertions:
//
//	A. 内网段来源（edge 记录 10.x）命中 candidate（in_cidr 10.0.0.0/8）
//	B. 外网段来源（edge 记录 203.0.113.x）命中 control
//	C. 伪造 XFF 前缀不改变结果（外网伪造内网 → 仍 control；内网被前缀 → 仍 candidate）
//	D. 无 XFF → 派生为直连对端（回环，不在 10/8）→ control
//	E. 未配置 -trusted-proxy-cidrs 的第二个真实 proxy 进程：任何 XFF 一律
//	   忽略（现网默认行为），内网 XFF 仍 control
//	F. /sdk/eval 中继路径：注入 client_ip 后 in_cidr 命中；调用方显式
//	   attributes.client_ip 优先，注入不覆盖
package gatewaye2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/godeps/grayling/pkg/model"
)

const (
	ipFlagKey     = "ip-gray"
	ipRouteSecret = "clientip-e2e-secret"
)

// ipGrayFlag targets the reserved client_ip attribute with in_cidr: sources
// inside 10.0.0.0/8 (内网段) get "candidate", everyone else the default
// "control". No percentage ⇒ no bucketing section needed; user_id stays the
// randomization-unit field of the route (unitField=user_id).
func ipGrayFlag() *model.Flag {
	return &model.Flag{
		SchemaVersion: model.SchemaVersion,
		Key:           ipFlagKey, Type: model.FlagString,
		Fields: []string{"user_id", model.ClientIPField},
		Variations: map[string]any{
			"control": "control-value", "candidate": "candidate-value",
		},
		DefaultRule: &model.Rule{Variation: "control"},
		Targeting: []model.Rule{{
			Name: "internal-net",
			Constraints: []model.Constraint{
				{Field: model.ClientIPField, Op: model.OpInCIDR, Value: "10.0.0.0/8"},
			},
			Variation: "candidate",
		}},
	}
}

func TestGatewayClientIPE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("client-IP e2e runs real golab/golab-proxy processes; skipped in -short")
	}
	root := repoRoot(t)

	// ---- golab control plane + data plane (real binary, real gonacos) ----
	golabBin := buildBinary(t, root, "./cmd/golab", "golab")
	golabPort := freePort(t)
	dataDir := t.TempDir()
	golab := startProc(t, "golab", golabBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", golabPort),
		"-data", filepath.Join(dataDir, "store.json"),
		"-gonacos-data", filepath.Join(dataDir, "gonacos"),
		"-gonacos-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-gonacos-grpc-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"-rollout-interval", "0",
		"-reconcile-interval", "0",
		"-requests-per-second", "100000",
	)
	golabBase := fmt.Sprintf("http://127.0.0.1:%d", golabPort)
	waitHealth(t, golabBase, "/api/v1/health", 30*time.Second)
	ownerToken := scrapeOwnerToken(t, golab, 15*time.Second)
	admin := &adminClient{t: t, base: golabBase, token: ownerToken}

	if code := admin.do("POST", "/api/v1/admin/workspaces",
		map[string]any{"slug": "acme", "name": "Acme"}, nil); code != http.StatusCreated {
		t.Fatalf("create workspace: %d", code)
	}
	if code := admin.do("POST", "/api/v1/admin/workspaces/acme/environments",
		map[string]any{"slug": "prod", "name": "Production"}, nil); code != http.StatusCreated {
		t.Fatalf("create env: %d", code)
	}
	var reviewerCT struct{ Secret string }
	if code := admin.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "reviewer", "kind": "admin", "role": "publisher"}, &reviewerCT); code != http.StatusCreated {
		t.Fatalf("create reviewer token: %d", code)
	}
	admin.reviewer = reviewerCT.Secret
	var sdkCT struct {
		Secret string
	}
	if code := admin.do("POST", "/api/v1/admin/workspaces/acme/tokens",
		map[string]any{"name": "ip-sdk", "kind": "sdk", "environment": "prod"}, &sdkCT); code != http.StatusCreated {
		t.Fatalf("create sdk token: %d", code)
	}
	admin.publishFlag(ipGrayFlag())

	// ---- two real proxy processes: trusted-edge vs default posture ----
	proxyBin := buildBinary(t, root, "./cmd/golab-proxy", "golab-proxy")

	startProxy := func(name string, extraArgs ...string) string {
		port := freePort(t)
		args := []string{
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-upstream", golabBase,
			"-timeout", "5s",
			"-gateway-token", sdkCT.Secret,
			"-gateway-secret", ipRouteSecret,
			"-gateway-eval-timeout", "2s",
		}
		startProc(t, name, proxyBin, append(args, extraArgs...)...)
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		waitHealth(t, base, "/healthz", 20*time.Second)
		return base
	}
	// Exactly one trusted hop: the loopback "edge" that records the real
	// client in the rightmost XFF entry.
	trustedProxy := startProxy("golab-proxy-trusted", "-trusted-proxy-cidrs", "127.0.0.1/32")
	// Default posture: no trusted proxies ⇒ XFF never consulted.
	defaultProxy := startProxy("golab-proxy-default")

	// ---- forwardAuth evaluation through the trusted-edge proxy ----
	forwardAuthVariant := func(proxyURL, xff string) (variant, failOpen string) {
		t.Helper()
		url := proxyURL + "/api/v1/gateway/forwardauth?flagKey=" + ipFlagKey +
			"&defaultVariant=control&unitField=user_id&secret=" + ipRouteSecret
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("X-Bucketing-Key", "user-1")
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forwardauth (xff=%q): %v", xff, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("forwardauth status = %d (fail-open contract violated)", resp.StatusCode)
		}
		return resp.Header.Get("X-Golab-Variant"), resp.Header.Get("X-Golab-Fail-Open")
	}

	for _, tc := range []struct {
		name         string
		proxyURL     string
		xff          string
		wantVariant  string
		wantFailOpen string
	}{
		{"A 内网段命中 candidate", trustedProxy, "10.1.2.3", "candidate", ""},
		{"B 外网段命中 control", trustedProxy, "203.0.113.9", "control", ""},
		{"C1 伪造内网前缀无效（外网仍 control）", trustedProxy, "10.9.9.9, 203.0.113.9", "control", ""},
		{"C2 伪造前缀不改变内网结果", trustedProxy, "203.0.113.9, 10.1.2.3", "candidate", ""},
		{"C3 多级伪造全部无效", trustedProxy, "10.0.0.1, 10.255.255.255, 203.0.113.9", "control", ""},
		{"D 无 XFF → 直连对端（回环∉10/8）→ control", trustedProxy, "", "control", ""},
		{"E 未配置信任链：内网 XFF 被忽略 → control", defaultProxy, "10.1.2.3", "control", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variant, failOpen := forwardAuthVariant(tc.proxyURL, tc.xff)
			if variant != tc.wantVariant {
				t.Fatalf("xff=%q → variant = %q, want %q", tc.xff, variant, tc.wantVariant)
			}
			if failOpen != tc.wantFailOpen {
				t.Fatalf("xff=%q → X-Golab-Fail-Open = %q, want %q", tc.xff, failOpen, tc.wantFailOpen)
			}
		})
	}

	// ---- /sdk/eval relay path (server-side evaluation through the proxy) ----
	relayEval := func(body map[string]any) model.Result {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", trustedProxy+"/api/v1/sdk/eval", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+sdkCT.Secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "10.1.2.3") // edge 记录的内网真实来源
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("eval via proxy: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("eval status %d: %s", resp.StatusCode, raw)
		}
		var out struct {
			Result model.Result `json:"result"`
			Value  string       `json:"value"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("eval decode: %v (%s)", err, raw)
		}
		if out.Value != out.Result.Value {
			t.Fatalf("eval value mismatch: %s", raw)
		}
		return out.Result
	}

	t.Run("F1 中继注入 client_ip → in_cidr 命中 candidate", func(t *testing.T) {
		res := relayEval(map[string]any{
			"flagKey": ipFlagKey,
			"context": map[string]any{
				"targetingKey": "user-2",
				"attributes":   map[string]any{"user_id": "user-2"},
			},
		})
		if res.VariationType != "candidate" || res.Reason != model.ReasonTargetingMatch {
			t.Fatalf("relay eval: got %q/%s, want candidate/TARGETING_MATCH", res.VariationType, res.Reason)
		}
	})

	t.Run("F2 调用方显式 client_ip 优先，注入不覆盖", func(t *testing.T) {
		res := relayEval(map[string]any{
			"flagKey": ipFlagKey,
			"context": map[string]any{
				"targetingKey": "user-3",
				"attributes": map[string]any{
					"user_id":   "user-3",
					"client_ip": "203.0.113.9", // 显式外网值：即使 XFF 报内网也不覆盖
				},
			},
		})
		if res.VariationType != "control" || res.Reason != model.ReasonDefault {
			t.Fatalf("explicit client_ip must win: got %q/%s, want control/DEFAULT",
				res.VariationType, res.Reason)
		}
	})
}
