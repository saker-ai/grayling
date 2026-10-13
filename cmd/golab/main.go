// Command golab starts the GoLab control plane + data plane:
//
//   - the GoLab HTTP API (admin, SDK config/eval/stream, collect) and the
//     embedded web console;
//   - an embedded gonacos server or external Nacos 3 instance providing the
//     Config and AI content substrate. Embedded listeners bind to loopback
//     by default. The native-auth mode scopes embedded SDK-token reads and
//     subscriptions (docs/Nacos原生客户端.md).
//
// Storage topology (docs/技术方案.md §3): GoLab server + Nacos substrate +
// platform store. The dev/default platform store is the
// file-backed JSON store; -sqlite and -pg select the GORM-backed SQLite and
// PostgreSQL stores (docs/storage.md).
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	golabapp "github.com/saker-ai/grayling"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/recovery"
	"github.com/godeps/gonacos/pkg/server"
)

func main() {
	var (
		listenAddr            = flag.String("addr", "127.0.0.1:8688", "GoLab HTTP listen address")
		dataFile              = flag.String("data", filepath.Join("data", "golab-store.json"), "platform store file ('' = memory-only; ignored when -pg or -sqlite is set)")
		pgDSN                 = flag.String("pg", "", "PostgreSQL DSN for the production platform store (e.g. postgres://golab:secret@127.0.0.1:5432/golab); overrides -data")
		sqlitePath            = flag.String("sqlite", "", "pure-Go SQLite database path for the platform store; overrides -data")
		gonacosRoot           = flag.String("gonacos-root", "", "gonacos contract root (repo with api/openapi/upstream); empty disables 501-stub registration")
		gonacosData           = flag.String("gonacos-data", "", "gonacos embedded-redis dump dir (default <data dir>/gonacos)")
		gonacosAddr           = flag.String("gonacos-addr", "127.0.0.1:18848", "gonacos HTTP bind — KEEP LOOPBACK/PRIVATE (§9)")
		gonacosGRPC           = flag.String("gonacos-grpc-addr", "127.0.0.1:19848", "gonacos gRPC bind — KEEP LOOPBACK/PRIVATE (§9)")
		nacosURL              = flag.String("nacos-url", "", "external Nacos 3 HTTP origin; empty uses embedded gonacos")
		nativeConfigAuth      = flag.Bool("nacos-native-auth", false, "enable SDK-token-scoped native Nacos config reads; HTTP exposes only SDK login")
		redisAddr             = flag.String("redis", "", "external Redis for gonacos cluster mode (default: embedded miniredis)")
		authSecret            = flag.String("auth-secret", "", "HMAC secret for gonacos tokens + store pseudonymization seed material (random per boot when empty — set it for restarts with cluster mode)")
		authSecretFile        = flag.String("auth-secret-file", "", "owner-only file containing the gonacos HMAC/auth secret")
		rolloutIv             = flag.Duration("rollout-interval", 30*time.Second, "auto-advance engine tick interval for staged release plans (0 disables automation; manual plan operations stay available)")
		rolloutLease          = flag.Duration("rollout-lease-duration", 0, "PostgreSQL rollout leader lease (0 = max(3 x rollout-interval, 30s); set longer than worst expected decision pass)")
		reconcileIv           = flag.Duration("reconcile-interval", 30*time.Second, "publish reconciliation tick interval: re-verifies the gonacos substrate against the platform store and idempotently republishes drift (0 disables); PostgreSQL control changes sync separately every 500ms")
		requestsPerSec        = flag.Float64("requests-per-second", 0, "per-token request rate limit on sdk/collect endpoints (0 = default 200)")
		evaluationSampleRate  = flag.Float64("evaluation-sample-rate", 0, "fraction [0,1] of safe feature-flag evaluation events to write as structured logs")
		bootstrapName         = flag.String("bootstrap-owner-name", "bootstrap-owner", "display name of the auto-created global owner token")
		printVersion          = flag.Bool("version", false, "print version and exit")
		recoveryPoint         = flag.String("recovery-point", "", "checkpoint to verify before opening the API after a restore")
		writePoint            = flag.String("write-recovery-point", "", "write a checkpoint from quiesced PostgreSQL and gonacos state, then exit")
		verifyPoint           = flag.String("verify-recovery-point", "", "verify restored components against a checkpoint, then exit without opening listeners")
		rotationKeyFile       = flag.String("rotate-hmac-key-file", "", "offline HMAC rotation key file (requires a verified backup and two owner tokens)")
		rotationOperatorFile  = flag.String("rotation-operator-token-file", "", "owner token file for offline HMAC rotation")
		rotationApproverFile  = flag.String("rotation-approver-token-file", "", "distinct owner token file approving offline HMAC rotation")
		rotationBackupPoint   = flag.String("rotation-backup-point", "", "checkpoint from the completed joint backup, verified before rotation")
		rotationProvisionFile = flag.String("provision-rotation-approver-file", "", "offline issue a second global owner token into a new owner-only file")
	)
	flag.Parse()
	if *pgDSN != "" && *sqlitePath != "" {
		log.Fatal("-pg and -sqlite are mutually exclusive")
	}
	if *evaluationSampleRate < 0 || *evaluationSampleRate > 1 {
		log.Fatal("-evaluation-sample-rate must be within [0,1]")
	}
	if *rolloutLease < 0 || (*rolloutLease > 0 && *rolloutLease <= *rolloutIv) {
		log.Fatal("-rollout-lease-duration must exceed -rollout-interval or be 0 for automatic sizing")
	}
	if *nacosURL != "" && (*nativeConfigAuth || *redisAddr != "" || *recoveryPoint != "" || *writePoint != "" || *verifyPoint != "" || *rotationKeyFile != "" || *rotationProvisionFile != "") {
		log.Fatal("external Nacos is incompatible with embedded native auth, Redis, recovery checkpoints, and offline key rotation")
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
	if *nacosURL == "" && (!isLoopback(*gonacosAddr) || !isLoopback(*gonacosGRPC)) {
		if !*nativeConfigAuth {
			logger.Fatal("refusing non-loopback gonacos listeners without -nacos-native-auth=true: anonymous native access cannot be exposed to a tenant-reachable network")
		}
		if os.Getenv("GOLAB_ALLOW_NON_LOOPBACK_GONACOS") != "1" {
			logger.Fatalf("refusing to bind gonacos to non-loopback address (%s / %s): set GOLAB_ALLOW_NON_LOOPBACK_GONACOS=1 only behind verified TLS termination and network policy", *gonacosAddr, *gonacosGRPC)
		}
		logger.Printf("WARNING: native Nacos bound to non-loopback addresses %s / %s — TLS termination and network policy MUST be verified", *gonacosAddr, *gonacosGRPC)
	}
	if *writePoint == "" && *verifyPoint == "" && *rotationKeyFile == "" && *rotationProvisionFile == "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		appCtx, appCancel := context.WithCancel(context.Background())
		defer appCancel()
		app, err := golabapp.Start(appCtx, golabapp.Options{
			DataFile: *dataFile, SQLitePath: *sqlitePath, PostgresDSN: *pgDSN,
			NacosURL: *nacosURL, NacosUsername: os.Getenv("GOLAB_NACOS_USERNAME"),
			NacosPassword: os.Getenv("GOLAB_NACOS_PASSWORD"), NacosToken: os.Getenv("GOLAB_NACOS_TOKEN"),
			GonacosAddr: *gonacosAddr, GonacosGRPCAddr: *gonacosGRPC,
			GonacosRoot: *gonacosRoot, GonacosData: *gonacosData, RedisAddr: *redisAddr,
			NativeConfigAuth:        *nativeConfigAuth,
			AllowNonLoopbackGonacos: os.Getenv("GOLAB_ALLOW_NON_LOOPBACK_GONACOS") == "1",
			AuthSecret:              *authSecret, AdminPassword: adminPassword, RecoveryPoint: *recoveryPoint,
			BootstrapOwnerName: *bootstrapName, RolloutInterval: *rolloutIv,
			RolloutLeaseDuration: *rolloutLease, ReconcileInterval: *reconcileIv,
			RequestsPerSecond: *requestsPerSec, EvaluationSampleRate: *evaluationSampleRate,
			Logger: logger,
		})
		if err != nil {
			logger.Fatalf("start GoLab: %v", err)
		}
		if httpAddr, grpcAddr := app.GonacosAddrs(); httpAddr != "" {
			logger.Printf("gonacos substrate: http=%s grpc=%s", httpAddr, grpcAddr)
		} else {
			logger.Printf("Nacos substrate: external %s", *nacosURL)
		}
		logBootstrapToken(logger, app.BootstrapOwnerToken())
		httpSrv := &http.Server{Addr: *listenAddr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
		serveErr := make(chan error, 1)
		go func() {
			logger.Printf("golab API + console listening on http://%s", *listenAddr)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
		select {
		case <-ctx.Done():
		case err := <-app.Errors():
			if err != nil {
				logger.Printf("GoLab background error: %v", err)
			}
		case err := <-serveErr:
			logger.Printf("GoLab HTTP error: %v", err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("HTTP shutdown: %v", err)
		}
		if err := app.Close(shutdownCtx); err != nil {
			logger.Printf("GoLab shutdown: %v", err)
		}
		return
	}

	if *nacosURL == "" && *dataFile != "" && *gonacosData == "" {
		*gonacosData = filepath.Join(filepath.Dir(*dataFile), "gonacos")
	}
	var diskSnapshotHash string
	if (*recoveryPoint != "" || *verifyPoint != "" || *rotationKeyFile != "" || *rotationProvisionFile != "") && *redisAddr == "" {
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
	} else if (*recoveryPoint != "" || *verifyPoint != "" || *rotationKeyFile != "" || *rotationProvisionFile != "") && *redisAddr != "" {
		var err error
		diskSnapshotHash, err = recovery.DigestRedisSnapshot(context.Background(), *redisAddr)
		if err != nil {
			logger.Fatalf("recovery verification: %v", err)
		}
	}

	// ---- offline operations use the embedded gonacos snapshot ----
	var store *platform.Store
	var err error
	var configStore configstore.Store
	gopts := []server.Option{server.WithAddr(*gonacosAddr), server.WithGRPCAddr(*gonacosGRPC)}
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
	gopts = append(gopts, server.WithStrictSnapshot(true))
	gsrv, err := server.New(gopts...)
	if err != nil {
		logger.Fatalf("gonacos server: %v", err)
	}
	bundle := gsrv.Services()
	if bundle == nil || bundle.Config == nil || bundle.AI == nil {
		logger.Fatal("gonacos services unavailable")
	}
	configStore = configstore.NewGonacosStore(bundle.Config)

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
	} else if *sqlitePath != "" {
		store, err = platform.NewSQLiteStore(ctx, *sqlitePath)
		if err != nil {
			logger.Fatalf("platform store (sqlite): %v", err)
		}
		logger.Printf("platform store: sqlite (%s)", *sqlitePath)
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
	if *rotationProvisionFile != "" {
		if *pgDSN == "" || *redisAddr == "" || *authSecretFile == "" || *rotationBackupPoint == "" || *rotationOperatorFile == "" || *rotationKeyFile != "" || *rotationApproverFile != "" || *recoveryPoint != "" || *writePoint != "" || *verifyPoint != "" {
			logger.Fatal("approver provisioning requires -pg, -redis, auth secret file, backup point, operator token file, and a new output path")
		}
		if err := provisionRotationApprover(ctx, store, *pgDSN, *authSecret, *redisAddr, diskSnapshotHash,
			configStore, *rotationBackupPoint, *rotationOperatorFile, *rotationProvisionFile); err != nil {
			logger.Fatalf("provision rotation approver: %v", err)
		}
		logger.Print("rotation approver token issued with audit; escrow the output and take a new joint backup")
		return
	}
	if *rotationKeyFile != "" {
		if *pgDSN == "" || *redisAddr == "" || *authSecretFile == "" || *rotationBackupPoint == "" || *rotationOperatorFile == "" || *rotationApproverFile == "" || *recoveryPoint != "" || *writePoint != "" || *verifyPoint != "" {
			logger.Fatal("rotation requires -pg, backup checkpoint, key and two owner token files; recovery modes are exclusive")
		}
		if err := runHMACRotation(ctx, store, *pgDSN, *authSecret, *redisAddr, diskSnapshotHash, configStore,
			*rotationBackupPoint, *rotationKeyFile, *rotationOperatorFile, *rotationApproverFile); err != nil {
			logger.Fatalf("HMAC rotation: %v", err)
		}
		logger.Print("HMAC rotation committed with audit; keep the pre-rotation backup until post-checks pass")
		return
	}
	if *recoveryPoint != "" || *writePoint != "" || *verifyPoint != "" {
		if *pgDSN == "" {
			logger.Fatal("recovery checkpoint requires -pg")
		}
		if (*writePoint != "" && (*recoveryPoint != "" || *verifyPoint != "")) || (*recoveryPoint != "" && *verifyPoint != "") {
			logger.Fatal("recovery point modes are mutually exclusive")
		}
		point, err := recovery.Capture(ctx, *pgDSN, *authSecret, store, configStore)
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
	logger.Fatal("offline operation did not exit")
}

func logBootstrapToken(logger *log.Logger, token string) {
	if token == "" {
		return
	}
	logger.Printf("")
	logger.Printf("  ╔══════════════════════════════════════════════════════════════╗")
	logger.Printf("  ║ GLOBAL OWNER TOKEN (shown once — store it now):              ║")
	logger.Printf("  ║ %s", token)
	logger.Printf("  ╚══════════════════════════════════════════════════════════════╝")
	logger.Printf("")
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
