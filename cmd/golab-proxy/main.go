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

	"github.com/godeps/golab/internal/proxy"
)

func main() {
	var (
		listenAddr  = flag.String("addr", "127.0.0.1:8689", "proxy listen address")
		upstream    = flag.String("upstream", "http://127.0.0.1:8688", "upstream GoLab server base URL")
		reqTimeout  = flag.Duration("timeout", 10*time.Second, "per-request timeout for eval/collect/config relays")
		healthTO    = flag.Duration("health-timeout", 3*time.Second, "upstream readiness probe timeout")
		maxIdle     = flag.Int("max-idle-conns-per-host", 100, "keep-alive pool size toward the upstream")
		maxConns    = flag.Int("max-conns-per-host", 0, "total upstream connection cap (0 = unlimited)")
		dialTimeout = flag.Duration("dial-timeout", 5*time.Second, "TCP dial timeout toward the upstream")
		gwToken     = flag.String("gateway-token", "", "GoLab SDK token for the Traefik forwardAuth evaluation endpoint (empty = endpoint fail-opens to default variants)")
		gwSecret    = flag.String("gateway-secret", "", "shared secret the router-config generator embeds in the forwardAuth address (internal/gateway RouteSpec.Secret); when set, requests without a matching secret are never evaluated upstream — STRONGLY recommended whenever the proxy is network-reachable")
		gwTimeout   = flag.Duration("gateway-eval-timeout", 2*time.Second, "per-evaluation upstream timeout on the gateway forwardAuth hot path (fail-open after)")
		version     = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "golab-proxy ", log.LstdFlags|log.Lmsgprefix)
	if *version {
		fmt.Println("golab-proxy dev")
		return
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
		Logger: logger,
	})
	if err != nil {
		logger.Fatalf("proxy: %v", err)
	}
	defer p.CloseIdleConnections()
	if *gwToken != "" && *gwSecret == "" {
		logger.Printf("WARNING: gateway forwardAuth endpoint enabled WITHOUT -gateway-secret: any caller able to reach this proxy can trigger evaluations with the gateway credential (flag probing / exposure pollution). Set -gateway-secret and generate router config with the same secret (internal/gateway RouteSpec.Secret) unless the proxy is strictly loopback/trusted-mesh.")
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
