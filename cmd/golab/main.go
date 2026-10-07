// Command golab starts the GoLab control plane + data plane:
//
//   - the GoLab HTTP API (admin, SDK config/eval/stream, collect) and the
//     embedded web console;
//   - an in-process gonacos server providing the Config substrate (flag
//     revisions + manifest distribution) and the AI resource store. The
//     gonacos listeners are bound to LOOPBACK by default. Anonymous native
//     access must never be exposed to tenants; the explicit native-auth mode
//     scopes SDK-token reads and subscriptions (docs/Nacos原生客户端.md).
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
	"log/slog"
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
	"github.com/godeps/golab/internal/recovery"
	"github.com/godeps/golab/web"
	"github.com/godeps/gonacos/pkg/server"
)

func main() {
	var (
		listenAddr           = flag.String("addr", "127.0.0.1:8688", "GoLab HTTP listen address")
		dataFile             = flag.String("data", filepath.Join("data", "golab-store.json"), "platform store file ('' = memory-only; ignored when -pg is set)")
		pgDSN                = flag.String("pg", "", "PostgreSQL DSN for the production platform store (e.g. postgres://golab:secret@127.0.0.1:5432/golab); overrides -data")
		gonacosRoot          = flag.String("gonacos-root", "", "gonacos contract root (repo with api/openapi/upstream); empty disables 501-stub registration")
		gonacosData          = flag.String("gonacos-data", "", "gonacos embedded-redis dump dir (default <data dir>/gonacos)")
		gonacosAddr          = flag.String("gonacos-addr", "127.0.0.1:18848", "gonacos HTTP bind — KEEP LOOPBACK/PRIVATE (§9)")
		gonacosGRPC          = flag.String("gonacos-grpc-addr", "127.0.0.1:19848", "gonacos gRPC bind — KEEP LOOPBACK/PRIVATE (§9)")
		nativeConfigAuth     = flag.Bool("nacos-native-auth", false, "enable SDK-token-scoped native Nacos config reads; HTTP exposes only SDK login")
		redisAddr            = flag.String("redis", "", "external Redis for gonacos cluster mode (default: embedded miniredis)")
		authSecret           = flag.String("auth-secret", "", "HMAC secret for gonacos tokens + store pseudonymization seed material (random per boot when empty — set it for restarts with cluster mode)")
		authSecretFile       = flag.String("auth-secret-file", "", "owner-only file containing the gonacos HMAC/auth secret")
		rolloutIv            = flag.Duration("rollout-interval", 30*time.Second, "auto-advance engine tick interval for staged release plans (0 disables automation; manual plan operations stay available)")
		reconcileIv          = flag.Duration("reconcile-interval", 30*time.Second, "publish reconciliation tick interval: re-verifies the gonacos substrate against the platform store and idempotently republishes drift (0 disables); PostgreSQL control changes sync separately every 500ms")
		requestsPerSec       = flag.Float64("requests-per-second", 0, "per-token request rate limit on sdk/collect endpoints (0 = default 200)")
		evaluationSampleRate = flag.Float64("evaluation-sample-rate", 0, "fraction [0,1] of safe feature-flag evaluation events to write as structured logs")
		bootstrapName        = flag.String("bootstrap-owner-name", "bootstrap-owner", "display name of the auto-created global owner token")
		printVersion         = flag.Bool("version", false, "print version and exit")
		recoveryPoint        = flag.String("recovery-point", "", "checkpoint to verify before opening the API after a restore")
		writePoint           = flag.String("write-recovery-point", "", "write a checkpoint from quiesced PostgreSQL and gonacos state, then exit")
		verifyPoint          = flag.String("verify-recovery-point", "", "verify restored components against a checkpoint, then exit without opening listeners")
	)
	flag.Parse()
	if *evaluationSampleRate < 0 || *evaluationSampleRate > 1 {
		log.Fatal("-evaluation-sample-rate must be within [0,1]")
	}

	logger := log.New(os.Stderr, "golab ", log.LstdFlags|log.Lmsgprefix)
	if *printVersion {
		fmt.Println("golab dev (schema v1)")
		return
	}
	if *authSecretFile != "" {
		if *authSecret != "" {
			logger.Fatal("-auth-secret and -auth-secret-file are mutually exclusive")
		}
		info, err := os.Stat(*authSecretFile)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			logger.Fatal("auth secret file must be a regular owner-only file (mode 0600)")
		}
		b, err := os.ReadFile(*authSecretFile)
		if err != nil {
			logger.Fatalf("read auth secret file: %v", err)
		}
		*authSecret = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if *authSecret == "" {
			logger.Fatal("auth secret file is empty")
		}
	}
	adminPassword := os.Getenv("GOLAB_ADMIN_PASSWORD")
	if passwordFile := os.Getenv("GOLAB_ADMIN_PASSWORD_FILE"); passwordFile != "" && adminPassword == "" {
		info, err := os.Stat(passwordFile)
		if err != nil {
			logger.Fatalf("admin password file: %v", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			logger.Fatal("admin password file must be readable only by its owner (mode 0600)")
		}
		contents, err := os.ReadFile(passwordFile)
		if err != nil {
			logger.Fatalf("admin password file: %v", err)
		}
		adminPassword = strings.TrimSuffix(strings.TrimSuffix(string(contents), "\n"), "\r")
		if adminPassword == "" {
			logger.Fatal("admin password file is empty")
		}
	}
	if adminPassword != "" && (len(adminPassword) < 12 || strings.TrimSpace(adminPassword) == "") {
		logger.Fatal("admin password must contain at least 12 bytes and non-whitespace characters")
	}

	// Anonymous gonacos has no tenant authorization. Any non-loopback bind
	// requires native SDK-token auth as well as an explicit deployment opt-in.
	if !isLoopback(*gonacosAddr) || !isLoopback(*gonacosGRPC) {
		if !*nativeConfigAuth {
			logger.Fatal("refusing non-loopback gonacos listeners without -nacos-native-auth=true: anonymous native access cannot be exposed to a tenant-reachable network")
		}
		if os.Getenv("GOLAB_ALLOW_NON_LOOPBACK_GONACOS") != "1" {
			logger.Fatalf("refusing to bind gonacos to non-loopback address (%s / %s): set GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1 only behind verified TLS termination and network policy", *gonacosAddr, *gonacosGRPC)
		}
		logger.Printf("WARNING: native Nacos bound to non-loopback addresses %s / %s — TLS termination and network policy MUST be verified", *gonacosAddr, *gonacosGRPC)
	}

	if *dataFile != "" && *gonacosData == "" {
		*gonacosData = filepath.Join(filepath.Dir(*dataFile), "gonacos")
	}
	var diskSnapshotHash string
	if (*recoveryPoint != "" || *verifyPoint != "") && *redisAddr == "" {
		if *gonacosData == "" {
			logger.Fatal("recovery verification requires -gonacos-data in embedded mode")
		}
		for _, name := range []string{"snapshot.json", "snapshot.json.hmac"} {
			if _, err := os.Stat(filepath.Join(*gonacosData, name)); err != nil {
				logger.Fatalf("recovery verification: missing gonacos %s: %v", name, err)
			}
		}
		var err error
		diskSnapshotHash, err = recovery.DigestFile(filepath.Join(*gonacosData, "snapshot.json"))
		if err != nil {
			logger.Fatalf("recovery verification: %v", err)
		}
	} else if (*recoveryPoint != "" || *verifyPoint != "") && *redisAddr != "" {
		var err error
		diskSnapshotHash, err = recovery.DigestRedisSnapshot(context.Background(), *redisAddr)
		if err != nil {
			logger.Fatalf("recovery verification: %v", err)
		}
	}

	// ---- gonacos substrate ----
	var store *platform.Store
	gopts := []server.Option{
		server.WithAddr(*gonacosAddr),
		server.WithGRPCAddr(*gonacosGRPC),
	}
	if *nativeConfigAuth {
		gopts = append(gopts, server.WithNativeConfigAuth(func(secret, namespace, group, dataID string) error {
			return authorizeNativeConfig(store, secret, namespace, group, dataID)
		}))
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
	if *recoveryPoint != "" || *verifyPoint != "" {
		gopts = append(gopts, server.WithStrictSnapshot(true))
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
	// Substrate handlers start serving only after the recovery gate passes.

	// ---- platform store ----
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
	if *recoveryPoint != "" || *writePoint != "" || *verifyPoint != "" {
		if *pgDSN == "" {
			logger.Fatal("recovery checkpoint requires -pg")
		}
		if (*writePoint != "" && (*recoveryPoint != "" || *verifyPoint != "")) || (*recoveryPoint != "" && *verifyPoint != "") {
			logger.Fatal("recovery point modes are mutually exclusive")
		}
		point, err := recovery.Capture(ctx, *pgDSN, *authSecret, store, configstore.NewGonacosStore(bundle.Config))
		if err != nil {
			logger.Fatalf("recovery gate: %v", err)
		}
		if *writePoint != "" {
			previousRedisSnapshot := ""
			if *redisAddr != "" {
				previousRedisSnapshot, _ = recovery.DigestRedisSnapshot(ctx, *redisAddr)
			}
			saveStarted := time.Now()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := gsrv.Shutdown(shutdownCtx); err != nil {
				logger.Fatalf("save gonacos snapshot: %v", err)
			}
			if *redisAddr == "" {
				snapshotPath := filepath.Join(*gonacosData, "snapshot.json")
				for _, path := range []string{snapshotPath, snapshotPath + ".hmac"} {
					info, statErr := os.Stat(path)
					if statErr != nil || info.ModTime().Before(saveStarted) {
						logger.Fatalf("recovery gate: gonacos snapshot was not saved during shutdown: %s: %v", path, statErr)
					}
				}
				point.GonacosSHA256, err = recovery.DigestFile(snapshotPath)
			} else {
				point.GonacosSHA256, err = recovery.DigestRedisSnapshot(ctx, *redisAddr)
				if err == nil && point.GonacosSHA256 == previousRedisSnapshot {
					logger.Fatal("recovery gate: gonacos Redis snapshot was not refreshed during shutdown")
				}
			}
			if err != nil {
				logger.Fatalf("recovery gate: gonacos digest: %v", err)
			}
			if err := recovery.Write(*writePoint, point); err != nil {
				logger.Fatalf("write recovery checkpoint: %v", err)
			}
			logger.Printf("recovery checkpoint written: %s", *writePoint)
			return
		}
		point.GonacosSHA256 = diskSnapshotHash
		path := *recoveryPoint
		if *verifyPoint != "" {
			path = *verifyPoint
		}
		if err := recovery.Verify(path, point); err != nil {
			logger.Fatalf("recovery gate: %v", err)
		}
		logger.Printf("recovery gate: checkpoint verified")
		if *verifyPoint != "" {
			return
		}
	}
	go func() {
		if err := gsrv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("gonacos serve: %v", err)
		}
	}()
	logger.Printf("gonacos substrate: http=%s grpc=%s (loopback-only by design)", gsrv.HTTPAddr(), gsrv.GRPCAddr())

	// ---- API + AI gate ----
	apiSrv := api.NewServer(store, configstore.NewGonacosStore(bundle.Config), api.Options{
		Logger:               log.New(os.Stderr, "golab-api ", log.LstdFlags),
		AdminPassword:        adminPassword,
		RequestsPerSecond:    *requestsPerSec,
		EvaluationSampleRate: *evaluationSampleRate,
	})
	if *evaluationSampleRate > 0 {
		telemetryLogger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		go func() {
			for {
				select {
				case event := <-apiSrv.SampledEvents():
					telemetryLogger.InfoContext(ctx, event.Name,
						"decision_id", event.DecisionID, "attributes", event.Attributes)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	if adminPassword != "" {
		logger.Printf("fixed admin password enabled")
	}
	apiSrv.SetAI(aisvc.NewService(store, bundle.AI))

	// ---- staged-release auto-advance engine (§5.3 P4) ----
	go apiSrv.Rollout.Run(ctx, *rolloutIv)
	// ---- publish reconciliation background task (§3/§11) ----
	go apiSrv.Admin.RunReconciler(ctx, *reconcileIv)
	go store.RunChangeSync(ctx, 500*time.Millisecond, logger.Printf)
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
