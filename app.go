// Package grayling embeds the Grayling control plane in a Go process. The caller
// owns the HTTP listener and process signals; App owns its storage, Nacos
// substrate, and background jobs.
package grayling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godeps/gonacos/pkg/server"
	"github.com/saker-ai/grayling/internal/ai"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/nacos"
	"github.com/saker-ai/grayling/internal/nativeauth"
	"github.com/saker-ai/grayling/internal/openaiproxy"
	"github.com/saker-ai/grayling/internal/platform"
	"github.com/saker-ai/grayling/internal/recovery"
	"github.com/saker-ai/grayling/web"
)

// Options selects platform storage and the Nacos substrate. Empty DataFile
// selects an in-memory platform store; empty NacosURL selects embedded gonacos.
// The zero-value embedded listener addresses use ephemeral loopback ports.
type Options struct {
	DataFile      string
	SQLitePath    string
	PostgresDSN   string
	RecoveryPoint string
	NacosURL      string
	NacosUsername string
	NacosPassword string
	NacosToken    string
	// OpenAIUpstreamURL enables governed Chat Completions. The client's
	// existing model provider key is forwarded unchanged.
	OpenAIUpstreamURL       string
	OpenAIWorkspace         string
	OpenAIEnvironment       string
	OpenAIFlagKey           string
	GonacosAddr             string
	GonacosGRPCAddr         string
	GonacosRoot             string
	GonacosData             string
	RedisAddr               string
	NativeConfigAuth        bool
	AllowNonLoopbackGonacos bool
	AuthSecret              string
	AdminPassword           string
	// DevAutoLogin signs console visitors in as the configured administrator.
	// cmd/grayling sets it from -dev-autologin / GRAYLING_DEV_AUTOLOGIN for
	// local development; it must stay false wherever the console is reachable
	// by anyone else.
	DevAutoLogin         bool
	BootstrapOwnerName   string
	RolloutInterval      time.Duration
	RolloutLeaseDuration time.Duration
	ReconcileInterval    time.Duration
	RequestsPerSecond    float64
	EvaluationSampleRate float64
	Logger               *log.Logger
}

// App is one running Grayling instance. Close is safe to call more than once.
type App struct {
	apiHandler http.Handler
	handler    http.Handler
	chatProxy  *openaiproxy.Proxy
	gonacos    *server.Server
	ownerToken string
	cancel     context.CancelFunc
	errors     chan error
	done       chan struct{}
	wg         sync.WaitGroup
	closeErr   error
}

// Start constructs the stores and begins background jobs. It does not open
// an HTTP listener; serve Handler or APIHandler in the caller's server.
// Canceling ctx also closes the App. An unexpected embedded server failure is
// sent to Errors and triggers shutdown.
func Start(ctx context.Context, opts Options) (*App, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	var chatHandler http.Handler
	var chatProxy *openaiproxy.Proxy
	if opts.OpenAIUpstreamURL != "" {
		if opts.OpenAIFlagKey == "" {
			opts.OpenAIFlagKey = "openai-chat"
		}
		var err error
		chatProxy, err = openaiproxy.New(opts.OpenAIUpstreamURL)
		if err != nil {
			return nil, fmt.Errorf("grayling: %w", err)
		}
		chatHandler = chatProxy
	}
	if opts.GonacosAddr == "" {
		opts.GonacosAddr = "127.0.0.1:0"
	}
	if opts.GonacosGRPCAddr == "" {
		opts.GonacosGRPCAddr = "127.0.0.1:0"
	}
	if opts.BootstrapOwnerName == "" {
		opts.BootstrapOwnerName = "bootstrap-owner"
	}
	if opts.NacosURL == "" && (!loopbackBind(opts.GonacosAddr) || !loopbackBind(opts.GonacosGRPCAddr)) &&
		(!opts.NativeConfigAuth || !opts.AllowNonLoopbackGonacos) {
		return nil, fmt.Errorf("grayling: non-loopback gonacos requires native config auth and explicit allowance")
	}
	if opts.NacosURL == "" && opts.GonacosData == "" && opts.DataFile != "" {
		opts.GonacosData = filepath.Join(filepath.Dir(opts.DataFile), "gonacos")
	}
	var snapshotHash string
	if opts.RecoveryPoint != "" {
		var err error
		if opts.RedisAddr == "" {
			if opts.GonacosData == "" {
				return nil, fmt.Errorf("grayling: recovery requires a gonacos data directory")
			}
			snapshot := filepath.Join(opts.GonacosData, "snapshot.json")
			for _, path := range []string{snapshot, snapshot + ".hmac"} {
				if _, err := os.Stat(path); err != nil {
					return nil, fmt.Errorf("grayling: recovery snapshot %s: %w", path, err)
				}
			}
			snapshotHash, err = recovery.DigestFile(snapshot)
		} else {
			snapshotHash, err = recovery.DigestRedisSnapshot(ctx, opts.RedisAddr)
		}
		if err != nil {
			return nil, fmt.Errorf("grayling: recovery snapshot: %w", err)
		}
	}

	store, err := openPlatform(ctx, opts)
	if err != nil {
		return nil, err
	}
	cleanupStore := true
	defer func() {
		if cleanupStore {
			_ = store.Close()
		}
	}()

	var gsrv *server.Server
	var cfg configstore.Store
	var aiService *ai.Service
	defer func() {
		if cleanupStore && gsrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = gsrv.Shutdown(shutdownCtx)
		}
	}()
	if opts.NacosURL != "" {
		client, err := nacos.NewClient(nacos.Config{
			URL: opts.NacosURL, Username: opts.NacosUsername,
			Password: opts.NacosPassword, Token: opts.NacosToken,
		})
		if err != nil {
			return nil, fmt.Errorf("grayling: external Nacos: %w", err)
		}
		if err := client.PingContext(ctx); err != nil {
			return nil, fmt.Errorf("grayling: external Nacos health check: %w", err)
		}
		cfg = configstore.NewNacosStore(client)
		aiService = ai.NewExternalService(store, configstore.NewNacosAIStore(client))
	} else {
		gopts := []server.Option{server.WithAddr(opts.GonacosAddr), server.WithGRPCAddr(opts.GonacosGRPCAddr)}
		if opts.NativeConfigAuth {
			gopts = append(gopts, server.WithNativeConfigAuth(func(secret, namespace, group, dataID string) error {
				return nativeauth.Authorize(store, secret, namespace, group, dataID)
			}))
		}
		if opts.GonacosRoot != "" {
			gopts = append(gopts, server.WithRoot(opts.GonacosRoot))
		}
		if opts.GonacosData != "" {
			gopts = append(gopts, server.WithDataDir(opts.GonacosData))
		}
		if opts.RedisAddr != "" {
			gopts = append(gopts, server.WithRedisAddr(opts.RedisAddr))
		}
		if opts.AuthSecret != "" {
			gopts = append(gopts, server.WithAuthSecret(opts.AuthSecret))
		}
		if opts.RecoveryPoint != "" {
			gopts = append(gopts, server.WithStrictSnapshot(true))
		}
		gsrv, err = server.New(gopts...)
		if err != nil {
			return nil, fmt.Errorf("grayling: embedded gonacos: %w", err)
		}
		bundle := gsrv.Services()
		if bundle == nil || bundle.Config == nil || bundle.AI == nil {
			return nil, fmt.Errorf("grayling: embedded gonacos services unavailable")
		}
		cfg = configstore.NewGonacosStore(bundle.Config)
		aiService = ai.NewService(store, bundle.AI)
	}
	if opts.RecoveryPoint != "" {
		point, err := recovery.Capture(ctx, opts.PostgresDSN, opts.AuthSecret, store, cfg)
		if err != nil {
			return nil, fmt.Errorf("grayling: recovery gate: %w", err)
		}
		point.GonacosSHA256 = snapshotHash
		if err := recovery.Verify(opts.RecoveryPoint, point); err != nil {
			return nil, fmt.Errorf("grayling: recovery gate: %w", err)
		}
	}

	apiServer := api.NewServer(store, cfg, api.Options{
		Logger: opts.Logger, AdminPassword: opts.AdminPassword, DevAutoLogin: opts.DevAutoLogin,
		ChatCompletions:      chatHandler,
		OpenAIWorkspace:      opts.OpenAIWorkspace,
		OpenAIEnvironment:    opts.OpenAIEnvironment,
		OpenAIFlagKey:        opts.OpenAIFlagKey,
		ConsoleSessionSecret: opts.AuthSecret,
		RequestsPerSecond:    opts.RequestsPerSecond,
		EvaluationSampleRate: opts.EvaluationSampleRate,
	})
	apiServer.SetAI(aiService)
	apiServer.Rollout.LeaseDuration = opts.RolloutLeaseDuration
	ownerToken, err := store.BootstrapOwner(ctx, opts.BootstrapOwnerName)
	if err != nil {
		return nil, err
	}
	apiHandler := apiServer.Handler()
	runCtx, cancel := context.WithCancel(ctx)
	app := &App{
		apiHandler: apiHandler, handler: web.Handler(apiHandler), chatProxy: chatProxy,
		gonacos: gsrv, ownerToken: ownerToken, cancel: cancel,
		errors: make(chan error, 1), done: make(chan struct{}),
	}
	cleanupStore = false
	if gsrv != nil {
		app.wg.Go(func() {
			if err := gsrv.Start(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case app.errors <- fmt.Errorf("grayling: embedded gonacos stopped: %w", err):
				default:
				}
				cancel()
			}
		})
	}
	app.wg.Go(func() { apiServer.Rollout.Run(runCtx, opts.RolloutInterval) })
	app.wg.Go(func() { apiServer.Admin.RunReconciler(runCtx, opts.ReconcileInterval) })
	app.wg.Go(func() { store.RunChangeSync(runCtx, 500*time.Millisecond, opts.Logger.Printf) })
	if opts.EvaluationSampleRate > 0 {
		telemetry := slog.New(slog.NewJSONHandler(opts.Logger.Writer(), nil))
		app.wg.Go(func() {
			for {
				select {
				case event := <-apiServer.SampledEvents():
					telemetry.InfoContext(runCtx, event.Name, "decision_id", event.DecisionID, "attributes", event.Attributes)
				case <-runCtx.Done():
					return
				}
			}
		})
	}
	go func() {
		<-runCtx.Done()
		app.wg.Wait()
		if app.chatProxy != nil {
			app.chatProxy.CloseIdleConnections()
		}
		app.closeErr = store.Close()
		close(app.done)
		close(app.errors)
	}()
	return app, nil
}

func validateOptions(opts Options) error {
	if opts.OpenAIUpstreamURL != "" && (opts.OpenAIWorkspace == "" || opts.OpenAIEnvironment == "") {
		return fmt.Errorf("grayling: OpenAI workspace and environment are required with an upstream")
	}
	if opts.OpenAIUpstreamURL == "" && (opts.OpenAIWorkspace != "" || opts.OpenAIEnvironment != "" || opts.OpenAIFlagKey != "") {
		return fmt.Errorf("grayling: OpenAI scope and flag require an upstream")
	}
	if opts.SQLitePath != "" && opts.PostgresDSN != "" {
		return fmt.Errorf("grayling: SQLite and PostgreSQL stores are mutually exclusive")
	}
	if opts.NacosURL != "" && (opts.RedisAddr != "" || opts.NativeConfigAuth || opts.GonacosData != "" || opts.GonacosRoot != "") {
		return fmt.Errorf("grayling: external Nacos cannot use embedded gonacos options")
	}
	if opts.RecoveryPoint != "" && (opts.PostgresDSN == "" || opts.NacosURL != "" || opts.AuthSecret == "") {
		return fmt.Errorf("grayling: recovery point requires PostgreSQL, embedded gonacos, and auth secret")
	}
	if opts.EvaluationSampleRate < 0 || opts.EvaluationSampleRate > 1 {
		return fmt.Errorf("grayling: evaluation sample rate must be within [0,1]")
	}
	if opts.RolloutLeaseDuration < 0 || (opts.RolloutLeaseDuration > 0 && opts.RolloutLeaseDuration <= opts.RolloutInterval) {
		return fmt.Errorf("grayling: rollout lease must exceed rollout interval")
	}
	if opts.AdminPassword != "" && (len(opts.AdminPassword) < 12 || strings.TrimSpace(opts.AdminPassword) == "") {
		return fmt.Errorf("grayling: admin password must contain at least 12 bytes and non-whitespace characters")
	}
	return nil
}

func openPlatform(ctx context.Context, opts Options) (*platform.Store, error) {
	if opts.PostgresDSN != "" {
		store, err := platform.NewPGStore(ctx, opts.PostgresDSN)
		if err != nil {
			return nil, fmt.Errorf("grayling: PostgreSQL store: %w", err)
		}
		return store, nil
	}
	if opts.SQLitePath != "" {
		store, err := platform.NewSQLiteStore(ctx, opts.SQLitePath)
		if err != nil {
			return nil, fmt.Errorf("grayling: SQLite store: %w", err)
		}
		return store, nil
	}
	store, err := platform.NewStore(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("grayling: platform store: %w", err)
	}
	return store, nil
}

func loopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Handler serves the API and embedded console. Mount APIHandler if the host
// application wants to serve its own pages at non-API paths.
func (a *App) Handler() http.Handler { return a.handler }

// APIHandler serves only Grayling API routes.
func (a *App) APIHandler() http.Handler { return a.apiHandler }

// BootstrapOwnerToken returns the new owner credential once at first start.
// It is empty when an owner credential already existed.
func (a *App) BootstrapOwnerToken() string { return a.ownerToken }

// Errors reports unexpected background server failures. The channel closes
// when the App has finished shutting down.
func (a *App) Errors() <-chan error { return a.errors }

// Done closes after storage and all background jobs have stopped.
func (a *App) Done() <-chan struct{} { return a.done }

// GonacosAddrs reports embedded listener addresses, or empty values in
// external Nacos mode.
func (a *App) GonacosAddrs() (httpAddr, grpcAddr string) {
	if a.gonacos == nil {
		return "", ""
	}
	return a.gonacos.HTTPAddr(), a.gonacos.GRPCAddr()
}

// Close requests shutdown and waits until resources have been released.
// The caller owns its HTTP server and should shut it down separately.
func (a *App) Close(ctx context.Context) error {
	a.cancel()
	select {
	case <-a.done:
		return a.closeErr
	default:
	}
	select {
	case <-a.done:
		return a.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
