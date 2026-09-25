// Gateway forwardAuth evaluation endpoint — the Traefik ≥3.6 multi-layer
// routing 求值服务 (docs/技术方案.md §4, docs/traefik-golab-architecture.md).
//
// Traefik's forwardAuth middleware sends the (already header-stripped)
// request to this endpoint. It extracts the bucketing key, evaluates the
// configured GoLab flag through the upstream /api/v1/sdk/eval (which records
// exposures at this decision point — 曝光集中记录), and returns the variation
// name in X-Golab-Variant. Traefik copies the response headers listed in
// authResponseHeaders back onto the request; the child routers then match on
// Header(`X-Golab-Variant`, ...).
//
// Fail-open contract (P5 acceptance): internal errors — upstream
// unreachable, timeout, non-200, failed evaluation, missing bucketing key,
// missing gateway credential — NEVER produce a non-2xx from this endpoint.
// It answers 200 with the configured default variant plus an
// X-Golab-Fail-Open reason header, so a GoLab outage degrades to "everyone
// on the default backend" instead of gateway 5xx. (Total unavailability of
// THIS process is answered by HA deployment: Traefik forwardAuth denies the
// request when the auth service cannot be reached at all.)
package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// GatewayConfig configures the forwardAuth evaluation endpoint. The proxy
// holds this SDK credential itself: gateway clients are end users and cannot
// carry GoLab tokens. The token decides workspace/environment (§9), so one
// proxy instance serves exactly one environment scope.
type GatewayConfig struct {
	// Token is the GoLab SDK token used for upstream eval calls. Empty
	// disables evaluation: every request fail-opens to the default variant
	// (logged).
	Token string
	// Secret is the shared route secret embedded by the config generator in
	// the forwardAuth address (internal/gateway RouteSpec.Secret). When set,
	// requests without a matching `secret` query parameter are NEVER
	// evaluated upstream (no flag probing, no exposure pollution through
	// this credential-holding endpoint): they fail-open to the default
	// variant like any other internal refusal. When empty the endpoint is
	// unauthenticated — acceptable only on a network where every caller is a
	// trusted gateway.
	Secret string
	// EvalTimeout bounds one upstream evaluation (default 2s — this is a
	// gateway hot path; fail-open answers after the timeout).
	EvalTimeout time.Duration
}

// Contract constants shared with internal/gateway (generated Traefik
// configuration) — kept as literals here to avoid an import cycle between
// the proxy and the admin-side generator.
const (
	gwVariantHeader  = "X-Golab-Variant"
	gwFlagKeyHeader  = "X-Golab-Flag-Key"
	gwFailOpenHeader = "X-Golab-Fail-Open"
	gwReasonHeader   = "X-Golab-Reason"

	gwDefaultUnitHeader = "X-Bucketing-Key"
	gwDefaultUnitField  = "targetingKey"

	gwForwardAuthPath = "/api/v1/gateway/forwardauth"
)

// gatewayLogCounter rate-limits the misconfiguration warning.
var gatewayLogCounter atomic.Int64

// logGateway writes a gateway diagnostic line when a logger is configured.
func (p *Proxy) logGateway(format string, args ...any) {
	if p.logger != nil {
		p.logger.Printf(format, args...)
	}
}

// handleGatewayForwardAuth serves Traefik forwardAuth probes.
//
// Per-route configuration travels in the forwardAuth address query string
// (Traefik preserves it verbatim — verified against v3.7.13):
//
//	flagKey        required — the GoLab flag to evaluate
//	defaultVariant the fail-open / missing-key variant value
//	unitField      evaluation-context field for the bucketing key
//	               (default "targetingKey")
//	unitHeader     request header carrying the bucketing key
//	               (default "X-Bucketing-Key")
func (p *Proxy) handleGatewayForwardAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flagKey := q.Get("flagKey")
	defaultVariant := q.Get("defaultVariant")
	unitField := q.Get("unitField")
	if unitField == "" {
		unitField = gwDefaultUnitField
	}
	unitHeader := q.Get("unitHeader")
	if unitHeader == "" {
		unitHeader = gwDefaultUnitHeader
	}

	// failOpen answers 200 with the default variant. The generated default
	// child router matches even WITHOUT the header (parent-rule fallback),
	// so an empty defaultVariant still lands on the default backend.
	failOpen := func(reason string) {
		if n := gatewayLogCounter.Add(1); n == 1 || n%100 == 0 {
			p.logGateway("fail-open #%d (%s): flagKey=%q unit header %q", n, reason, flagKey, unitHeader)
		}
		w.Header().Set(gwFailOpenHeader, reason)
		writeGatewayOK(w, defaultVariant, flagKey, "FAIL_OPEN")
	}

	// Shared route secret: this endpoint holds an SDK credential, so without
	// the secret any caller able to reach the proxy could probe arbitrary
	// flags and pollute server-recorded exposures. A mismatch is NEVER
	// evaluated upstream — it fail-opens to the static default like any
	// other refusal (legitimate gateways carry the secret in the generated
	// forwardAuth address).
	if p.cfg.Gateway.Secret != "" &&
		subtle.ConstantTimeCompare([]byte(q.Get("secret")), []byte(p.cfg.Gateway.Secret)) != 1 {
		failOpen("unauthorized")
		return
	}

	if flagKey == "" {
		failOpen("bad_config_no_flag_key")
		return
	}
	if p.cfg.Gateway.Token == "" {
		failOpen("not_configured")
		return
	}
	unit := r.Header.Get(unitHeader)
	if unit == "" {
		// No bucketing key → deterministic default, never a random split (§6).
		failOpen("missing_bucket_key")
		return
	}

	ctx := r.Context()
	timeout := p.cfg.Gateway.EvalTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	variant, reason, err := p.gatewayEval(ctx, flagKey, unitField, unit)
	if err != nil {
		failOpen(gatewayFailReason(err))
		return
	}
	if variant == "" {
		failOpen("eval_failed")
		return
	}
	writeGatewayOK(w, variant, flagKey, reason)
}

// gatewayEval calls the upstream remote-evaluation endpoint and extracts the
// variation name. A failed evaluation result (FLAG_NOT_FOUND, type errors)
// is returned as an error so the caller fail-opens.
func (p *Proxy) gatewayEval(ctx context.Context, flagKey, unitField, unit string) (variant, reason string, err error) {
	evalCtx := map[string]any{"targetingKey": unit}
	if unitField != gwDefaultUnitField {
		evalCtx["attributes"] = map[string]any{unitField: unit}
	}
	body, err := json.Marshal(map[string]any{
		"flagKey": flagKey,
		"context": evalCtx,
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.upstream.String()+"/api/v1/sdk/eval", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.Gateway.Token)
	resp, err := p.unary.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("eval http %d: %s", resp.StatusCode, classify(errors.New(string(raw))))
	}
	var out struct {
		Result struct {
			VariationType string `json:"variationType"`
			Reason        string `json:"reason"`
			Failed        bool   `json:"failed"`
			ErrorCode     string `json:"errorCode"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("eval response parse: %w", err)
	}
	if out.Result.Failed {
		code := out.Result.ErrorCode
		if code == "" {
			code = "eval_failed"
		}
		return "", "", errors.New(code)
	}
	return out.Result.VariationType, out.Result.Reason, nil
}

// gatewayFailReason maps transport errors to short fail-open reason codes.
func gatewayFailReason(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "upstream_timeout"
	case strings.HasPrefix(msg, "eval http "):
		return "upstream_http_error"
	case msg == "FLAG_NOT_FOUND":
		return "flag_not_found"
	}
	return "upstream_unreachable"
}

func writeGatewayOK(w http.ResponseWriter, variant, flagKey, reason string) {
	if variant != "" {
		w.Header().Set(gwVariantHeader, variant)
	}
	if flagKey != "" {
		w.Header().Set(gwFlagKeyHeader, flagKey)
	}
	if reason != "" {
		w.Header().Set(gwReasonHeader, reason)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}
