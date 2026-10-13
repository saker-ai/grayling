// Command golab-proxy runs the GoLab remote-evaluation proxy as a standalone,
// stateless deployment (docs/技术方案.md §3, §8): it relays
//
//	POST /api/v1/sdk/eval    remote evaluation (server-side exposure capture)
//	GET  /api/v1/sdk/stream  SSE revision invalidation (flushing relay)
//	POST /api/v1/collect     exposure/conversion ingestion
//	GET  /api/v1/sdk/config  scoped snapshot pull (ETag/304 passthrough)
//
// to an upstream GoLab server, and adds proxy-owned endpoints:
//
//	GET /healthz             liveness (no upstream dependency)
//	GET /api/v1/health       readiness (probes the upstream)
//	GET /api/v1/gateway/forwardauth
//	                         Traefik ≥3.6 multi-layer routing evaluation
//	                         (docs/技术方案.md §4): extracts the bucketing
//	                         key, evaluates via the upstream, returns the
//	                         variant header; fail-opens to the configured
//	                         default variant on ANY internal error so the
//	                         gateway never 5xx on a GoLab outage. Requires
//	                         -gateway-token (the proxy's own SDK credential —
//	                         end-user traffic cannot carry one).
//
// Both server-side evaluation paths (forwardAuth and the /sdk/eval relay)
// additionally derive the client IP from the trusted proxy chain
// (-trusted-proxy-cidrs; see internal/proxy/clientip.go for the trust model)
// and inject it into the evaluation context as the reserved "client_ip"
// attribute, enabling in_cidr targeting / IP-bucketed gray routing. On the
// relay path an explicit caller-provided client_ip is never overwritten.
//
// Relay credentials are passed through untouched — the upstream remains the
// single authentication/authorization point (§9), and per-token rate limiting
// stays there as well. The proxy holds no request state, so it scales
// horizontally and can be restarted at any time; deployments put it in front
// of browsers and thin services that must not receive full configuration
// snapshots, and (with -gateway-token) next to the API gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/godeps/grayling/internal/proxy"
)

func main() {
	var (
		listenAddr   = flag.String("addr", "127.0.0.1:8689", "proxy listen address")
		upstream     = flag.String("upstream", "http://127.0.0.1:8688", "upstream GoLab server base URL")
		reqTimeout   = flag.Duration("timeout", 10*time.Second, "per-request timeout for eval/collect/config relays")
		healthTO     = flag.Duration("health-timeout", 3*time.Second, "upstream readiness probe timeout")
		maxIdle      = flag.Int("max-idle-conns-per-host", 100, "keep-alive pool size toward the upstream")
		maxConns     = flag.Int("max-conns-per-host", 0, "total upstream connection cap (0 = unlimited)")
		dialTimeout  = flag.Duration("dial-timeout", 5*time.Second, "TCP dial timeout toward the upstream")
		gwToken      = flag.String("gateway-token", "", "GoLab SDK token for the Traefik forwardAuth evaluation endpoint (empty = endpoint fail-opens to default variants)")
		gwSecret     = flag.String("gateway-secret", "", "shared secret the router-config generator embeds in the forwardAuth address (internal/gateway RouteSpec.Secret); when set, requests without a matching secret are never evaluated upstream — STRONGLY recommended whenever the proxy is network-reachable")
		gwTimeout    = flag.Duration("gateway-eval-timeout", 2*time.Second, "per-evaluation upstream timeout on the gateway forwardAuth hot path (fail-open after)")
		trustedCIDRs = flag.String("trusted-proxy-cidrs", "",
			"comma-separated CIDRs and/or bare IPs (e.g. \"10.0.0.0/8,172.16.5.5,fd00::/8\") of the reverse proxies allowed to sit in front of this proxy (Traefik/LB subnets); env GOLAB_TRUSTED_PROXY_CIDRS is used when the flag is empty. "+
				"SECURITY SEMANTICS: the \"client_ip\" evaluation-context attribute (in_cidr targeting / IP bucketing) is derived by walking X-Forwarded-For from the connection peer right-to-left ONLY while the current IP lies inside these ranges; the first IP outside the set becomes client_ip and every XFF entry further left is DISCARDED — a client cannot forge its way into another IP's gray group. "+
				"Empty (default) trusts NO XFF at all: client_ip is the direct connection peer (RemoteAddr) only, preserving current behavior. "+
				"NEVER list ranges clients can connect from (e.g. 0.0.0.0/0): trusting a range means believing the XFF entries it \"appended\", which lets anyone speaking from it self-select gray groups.")
		version = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "golab-proxy ", log.LstdFlags|log.Lmsgprefix)
	if *version {
		fmt.Println("golab-proxy dev")
		return
	}

	trusted := *trustedCIDRs
	if trusted == "" {
		trusted = os.Getenv("GOLAB_TRUSTED_PROXY_CIDRS")
	}

	p, err := proxy.New(proxy.Config{
		Upstream:            *upstream,
		RequestTimeout:      *reqTimeout,
		HealthTimeout:       *healthTO,
		MaxIdleConnsPerHost: *maxIdle,
		MaxConnsPerHost:     *maxConns,
		DialTimeout:         *dialTimeout,
		Gateway: proxy.GatewayConfig{
			Token:       *gwToken,
			Secret:      *gwSecret,
			EvalTimeout: *gwTimeout,
		},
		TrustedProxyCIDRs: trusted,
		Logger:            logger,
	})
	if err != nil {
		logger.Fatalf("proxy: %v", err)
	}
	defer p.CloseIdleConnections()
	if *gwToken != "" && *gwSecret == "" {
		logger.Printf("WARNING: gateway forwardAuth endpoint enabled WITHOUT -gateway-secret: any caller able to reach this proxy can trigger evaluations with the gateway credential (flag probing / exposure pollution). Set -gateway-secret and generate router config with the same secret (internal/gateway RouteSpec.Secret) unless the proxy is strictly loopback/trusted-mesh.")
	}
	if trusted == "" {
		logger.Printf("client_ip derivation: no -trusted-proxy-cidrs configured — X-Forwarded-For is NOT trusted; client_ip is the direct connection peer only (set the flag/env to the edge proxy ranges to derive the real client IP)")
	} else {
		logger.Printf("client_ip derivation: trusting proxy chain from -trusted-proxy-cidrs %q (untrusted XFF segments are discarded)", trusted)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE relay is intentionally long-lived;
		// per-request deadlines are enforced toward the upstream instead.
	}
	go func() {
		logger.Printf("relaying %s → %s (eval/stream/collect/config + health)", *listenAddr, *upstream)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	logger.Printf("bye")
}
