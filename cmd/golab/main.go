// Command golab starts the GoLab control plane + data plane:
//
//   - the GoLab HTTP API (admin, SDK config/eval/stream, collect) and the
//     embedded web console;
//   - an in-process gonacos server providing the Config substrate (flag
//     revisions + manifest distribution) and the AI resource store. The
//     gonacos listeners are bound to LOOPBACK by default and must never be
//     exposed to tenants (§9); all tenant traffic flows through the GoLab
//     API, which enforces authorization gonacos does not.
//
// Storage topology (docs/技术方案.md §3): GoLab server + same-process
// gonacos + platform store. The dev/default platform store is the
// file-backed JSON store; -pg switches to the production PostgreSQL backend
// (schema auto-migrated, events monthly-partitioned, HMAC key-ring rotation
// migration — docs/技术方案.md §10.2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	aisvc "github.com/godeps/golab/internal/ai"
	"github.com/godeps/golab/internal/api"
	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/platform"
	"github.com/godeps/golab/web"
	"github.com/godeps/gonacos/pkg/server"
)

func main() {
	var (
		listenAddr     = flag.String("addr", "127.0.0.1:8688", "GoLab HTTP listen address")
		dataFile       = flag.String("data", filepath.Join("data", "golab-store.json"), "platform store file ('' = memory-only; ignored when -pg is set)")
		pgDSN          = flag.String("pg", "", "PostgreSQL DSN for the production platform store (e.g. postgres://golab:secret@127.0.0.1:5432/golab); overrides -data")
		gonacosRoot    = flag.String("gonacos-root", "", "gonacos contract root (repo with api/openapi/upstream); empty disables 501-stub registration")
		gonacosData    = flag.String("gonacos-data", "", "gonacos embedded-redis dump dir (default <data dir>/gonacos)")
		gonacosAddr    = flag.String("gonacos-addr", "127.0.0.1:18848", "gonacos HTTP bind — KEEP LOOPBACK/PRIVATE (§9)")
		gonacosGRPC    = flag.String("gonacos-grpc-addr", "127.0.0.1:19848", "gonacos gRPC bind — KEEP LOOPBACK/PRIVATE (§9)")
		redisAddr      = flag.String("redis", "", "external Redis for gonacos cluster mode (default: embedded miniredis)")
		authSecret     = flag.String("auth-secret", "", "HMAC secret for gonacos tokens + store pseudonymization seed material (random per boot when empty — set it for restarts with cluster mode)")
		rolloutIv      = flag.Duration("rollout-interval", 30*time.Second, "auto-advance engine tick interval for staged release plans (0 disables automation; manual plan operations stay available)")
		reconcileIv    = flag.Duration("reconcile-interval", 30*time.Second, "publish reconciliation tick interval: re-verifies the gonacos substrate against the platform store and idempotently republishes drift; also the multi-replica state-refresh cadence (0 disables)")
		requestsPerSec = flag.Float64("requests-per-second", 0, "per-token request rate limit on sdk/collect endpoints (0 = default 200)")
		bootstrapName  = flag.String("bootstrap-owner-name", "bootstrap-owner", "display name of the auto-created global owner token")
		printVersion   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "golab ", log.LstdFlags|log.Lmsgprefix)
	if *printVersion {
		fmt.Println("golab dev (schema v1)")
		return
	}

	// Fail loudly on non-loopback gonacos binds without an explicit opt-in:
	// the substrate has no tenant authorization (§9).
	if !isLoopback(*gonacosAddr) || !isLoopback(*gonacosGRPC) {
		if os.Getenv("GOLAB_ALLOW_NON_LOOPBACK_GONACOS") != "1" {
			logger.Fatalf("refusing to bind gonacos to non-loopback address (%s / %s): the substrate performs no tenant authorization. Set GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1 ONLY behind a verified network policy.", *gonacosAddr, *gonacosGRPC)
		}
		logger.Printf("WARNING: gonacos bound to non-loopback addresses %s / %s — network policy MUST deny tenant access", *gonacosAddr, *gonacosGRPC)
	}

	if *dataFile != "" && *gonacosData == "" {
		*gonacosData = filepath.Join(filepath.Dir(*dataFile), "gonacos")
	}

	// ---- gonacos substrate ----
	gopts := []server.Option{
		server.WithAddr(*gonacosAddr),
		server.WithGRPCAddr(*gonacosGRPC),
	}
	if *gonacosRoot != "" {
		gopts = append(gopts, server.WithRoot(*gonacosRoot))
	}
	if *gonacosData != "" {
		gopts = append(gopts, server.WithDataDir(*gonacosData))
	}
	if *redisAddr != "" {
		gopts = append(gopts, server.WithRedisAddr(*redisAddr))
	}
	if *authSecret != "" {
		gopts = append(gopts, server.WithAuthSecret(*authSecret))
	}
	gsrv, err := server.New(gopts...)
	if err != nil {
		logger.Fatalf("gonacos server: %v", err)
	}
	bundle := gsrv.Services()
	if bundle == nil || bundle.Config == nil || bundle.AI == nil {
		logger.Fatal("gonacos services unavailable")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := gsrv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("gonacos serve: %v", err)
		}
	}()
	logger.Printf("gonacos substrate: http=%s grpc=%s (loopback-only by design)", gsrv.HTTPAddr(), gsrv.GRPCAddr())

	// ---- platform store ----
	var store *platform.Store
	if *pgDSN != "" {
		store, err = platform.NewPGStore(ctx, *pgDSN)
		if err != nil {
			logger.Fatalf("platform store (postgres): %v", err)
		}
		logger.Printf("platform store: postgresql (schema migrated, state loaded)")
	} else {
		store, err = platform.NewStore(*dataFile)
		if err != nil {
			logger.Fatalf("platform store: %v", err)
		}
		if *dataFile != "" {
			logger.Printf("platform store: %s", *dataFile)
		} else {
			logger.Printf("platform store: memory-only (no persistence)")
		}
	}
	defer func() { _ = store.Close() }()

	// ---- API + AI gate ----
	apiSrv := api.NewServer(store, configstore.NewGonacosStore(bundle.Config), api.Options{
		Logger:            log.New(os.Stderr, "golab-api ", log.LstdFlags),
		RequestsPerSecond: *requestsPerSec,
	})
	apiSrv.SetAI(aisvc.NewService(store, bundle.AI))

	// ---- staged-release auto-advance engine (§5.3 P4) ----
	go apiSrv.Rollout.Run(ctx, *rolloutIv)
	// ---- publish reconciliation background task (§3/§11) ----
	go apiSrv.Admin.RunReconciler(ctx, *reconcileIv)
	// ---- bootstrap owner credential (printed once) ----
	if token := bootstrapOwner(store, *bootstrapName, logger); token != "" {
		logger.Printf("")
		logger.Printf("  ╔══════════════════════════════════════════════════════════════╗")
		logger.Printf("  ║ GLOBAL OWNER TOKEN (shown once — store it now):              ║")
		logger.Printf("  ║ %s", token)
		logger.Printf("  ╚══════════════════════════════════════════════════════════════╝")
		logger.Printf("")
	}

	// ---- HTTP surface: API + embedded console ----
	handler := withConsole(apiSrv.Handler())
	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Printf("golab API + console listening on http://%s", *listenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = gsrv.Shutdown(shutdownCtx)
	logger.Printf("bye")
}

// bootstrapOwner creates the initial global owner token when the store has
// none yet. Returns the plaintext secret to print, or "" when credentials
// already exist.
func bootstrapOwner(store *platform.Store, name string, logger *log.Logger) string {
	if len(store.ListTokens("")) > 0 {
		return ""
	}
	ct, err := store.CreateToken(platform.TokenAdmin, name, platform.RoleOwner, "", "", "bootstrap")
	if err != nil {
		logger.Fatalf("bootstrap owner token: %v", err)
	}
	_ = store.Audit(name, platform.RoleOwner, "bootstrap.owner", "", "", ct.Token.ID, "initial credential")
	return ct.Secret
}

// withConsole serves the embedded SPA for every non-/api path, with an
// index.html fallback for client-side routes. index.html is written
// directly (http.FileServer would redirect /index.html → / in a loop).
func withConsole(apiHandler http.Handler) http.Handler {
	assets, err := fs.Sub(web.Assets, ".")
	if err != nil {
		return apiHandler
	}
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		// Without console assets the API must still run headless.
		return apiHandler
	}
	fileServer := http.FileServerFS(assets)
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndex(w)
			return
		}
		// Static file if present, else SPA fallback.
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if f, err := assets.Open(clean); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w)
	})
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]", "":
		return true
	}
	return strings.HasPrefix(host, "127.")
}
