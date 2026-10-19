package grayling_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	agentclient "github.com/saker-ai/grayling/pkg/agent"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling"
)

func TestEmbeddedAppCanBeMountedAndRestarted(t *testing.T) {
	dir := t.TempDir()
	opts := grayling.Options{
		SQLitePath: filepath.Join(dir, "grayling.db"), GonacosData: filepath.Join(dir, "gonacos"),
		AuthSecret: "stable-secret-for-test", ReconcileInterval: 0,
	}
	app, err := grayling.Start(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(app.BootstrapOwnerToken(), "grayling_admin_") {
		t.Fatal("missing bootstrap owner credential")
	}
	for _, suffix := range []string{"approvals", "publications"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/workspaces/unknown/environments/prod/agent/services/gateway/bundles/"+suffix, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+app.BootstrapOwnerToken())
		response := httptest.NewRecorder()
		app.APIHandler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("Agent disabled route %s: %d", suffix, response.Code)
		}
	}
	httpAddr, grpcAddr := app.GonacosAddrs()
	if httpAddr == "" || grpcAddr == "" {
		t.Fatal("embedded gonacos did not bind")
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", app.APIHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("host")) })
	srv := httptest.NewServer(mux)
	resp, err := srv.Client().Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	resp, err = srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("host route status %d", resp.StatusCode)
	}
	console := httptest.NewRecorder()
	app.Handler().ServeHTTP(console, httptest.NewRequest(http.MethodGet, "/", nil))
	if console.Code != http.StatusOK || !strings.Contains(console.Body.String(), "<html") {
		t.Fatalf("console response: %d", console.Code)
	}
	disabledChat := httptest.NewRecorder()
	app.Handler().ServeHTTP(disabledChat, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if disabledChat.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured Chat Completions status %d", disabledChat.Code)
	}
	srv.Close()
	closeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := app.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(closeCtx); err != nil {
		t.Fatalf("second close: %v", err)
	}

	app, err = grayling.Start(t.Context(), opts)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if app.BootstrapOwnerToken() != "" {
		t.Fatal("restart issued another owner credential")
	}
	if err := app.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestExternalAppUsesHostHTTPListener(t *testing.T) {
	nacos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nacos/v3/admin/core/namespace/list" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected external Nacos request: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": []any{}})
	}))
	defer nacos.Close()
	ctx, cancel := context.WithCancel(t.Context())
	app, err := grayling.Start(ctx, grayling.Options{NacosURL: nacos.URL, NacosToken: "test-token"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if addr, grpc := app.GonacosAddrs(); addr != "" || grpc != "" {
		t.Fatal("external mode started embedded gonacos")
	}
	rec := httptest.NewRecorder()
	app.APIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("API health status %d", rec.Code)
	}
	cancel()
	select {
	case <-app.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("context cancellation did not close app")
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsConflictingOptions(t *testing.T) {
	for _, opts := range []grayling.Options{
		{SQLitePath: "x", PostgresDSN: "postgres://example"},
		{AgentEnabled: true},
		{AgentEnabled: true, PostgresDSN: "postgres://example"},
		{AgentAllowedHosts: []string{"localhost"}},
		{AgentMaxConnections: 10},
		{AgentEnabled: true, PostgresDSN: "postgres://example", AgentAllowedHosts: []string{"localhost/path"}, AgentMaxConnections: 1},
		{AgentEnabled: true, PostgresDSN: "postgres://example", AgentAllowedHosts: []string{"localhost"}, AgentMaxConnections: 1, RecoveryPoint: "backup"},
		{NacosURL: "http://example.com", NativeConfigAuth: true},
		{GonacosAddr: "0.0.0.0:1234"},
		{RecoveryPoint: "point.json", NacosURL: "http://example.com"},
		{OpenAIUpstreamURL: "https://example.com/not-v1"},
	} {
		if app, err := grayling.Start(t.Context(), opts); err == nil {
			_ = app.Close(t.Context())
			t.Fatalf("expected invalid options: %+v", opts)
		}
	}
}

func TestOpenAICompatibleRouteThroughEmbeddedApp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer existing-key" {
			t.Errorf("unexpected upstream request: %s, auth = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()
	app, err := grayling.Start(t.Context(), grayling.Options{
		OpenAIUpstreamURL: upstream.URL + "/v1", OpenAIWorkspace: "acme", OpenAIEnvironment: "prod", GonacosData: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"example","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer existing-key")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("console handler response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAgentRevocationRefreshRequiresSigningAndTrust(t *testing.T) {
	for _, opts := range []grayling.Options{
		{AgentRevocationRefresh: true},
		{AgentRevocationRefresh: true, AgentEnabled: true, PostgresDSN: "not-a-database"},
	} {
		if app, err := grayling.Start(t.Context(), opts); err == nil {
			app.Close(t.Context())
			t.Fatal("proof refresh enabled without its explicit prerequisites")
		}
	}
}

func TestAgentVerifierRequiresAgentBeforeOpeningStores(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	now := time.Now()
	verifier, err := agentclient.NewVerifier(map[string]agentclient.TrustKey{"test": {PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = grayling.Start(t.Context(), grayling.Options{PostgresDSN: "postgres://invalid.example/unreachable", AgentReleaseVerifier: verifier})
	if err == nil || !strings.Contains(err.Error(), "ReleaseVerifier requires AgentEnabled") {
		t.Fatal("invalid Agent trust configuration reached stores", err)
	}
}
