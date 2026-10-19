//go:build integration

package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func TestAgentAppTLSLifecycleAndRestart(t *testing.T) {
	dsn := pgtest.DSN(t)
	sessions := openSessions(t, dsn)
	g := grant()
	if err := sessions.SetScopeLimit(t.Context(), g.Scope, 4); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, sessions, g)
	var previousEpoch uint64
	for iteration := range 2 {
		server := httptest.NewUnstartedServer(nil)
		opts := grayling.Options{GonacosData: filepath.Join(t.TempDir(), "gonacos"), PostgresDSN: dsn, AuthSecret: "stable-agent-app-test-secret", AgentEnabled: true, AgentAllowedHosts: []string{server.Listener.Addr().String()}, AgentMaxConnections: 4, DevAutoLogin: true}
		app, err := grayling.Start(t.Context(), opts)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Close(context.Background()) })
		server.Config.Handler = app.Handler()
		server.StartTLS()
		t.Cleanup(server.Close)
		// The enabled endpoint still requires actual TLS, independent of proxy headers.
		insecure := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://"+server.Listener.Addr().String()+agentv1.ConnectPath, nil)
		request.Header.Set("X-Forwarded-Proto", "https")
		app.APIHandler().ServeHTTP(insecure, request)
		if insecure.Code != http.StatusForbidden {
			t.Fatalf("proxy scheme bypassed TLS: %d", insecure.Code)
		}
		// Console autologin/owner authority does not authorize a machine handshake.
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+agentv1.ConnectPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+app.BootstrapOwnerToken())
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("owner token entered Agent: %d", response.StatusCode)
		}
		connection, err := client.Dial(t.Context(), client.Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: secret, Hello: helloFor(g), HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		epoch := connection.Welcome().ConnectionEpoch
		if iteration > 0 && epoch <= previousEpoch {
			t.Fatal("App restart reset shared session epoch")
		}
		previousEpoch = epoch
		if err := clientHeartbeat(t, connection); err != nil {
			t.Fatal(err)
		}
		closeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		err = app.Close(closeCtx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-connection.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("App shutdown left WSS running")
		}
		connection.Close()
		// Requests reaching a mounted handler after shutdown cannot reacquire a session.
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+agentv1.ConnectPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err = server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("closed channel accepted request: %d", response.StatusCode)
		}
		if err := app.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		server.Close()
	}
}
func TestAgentAppDisabledKeepsRouteUnavailable(t *testing.T) {
	app, err := grayling.Start(t.Context(), grayling.Options{GonacosData: filepath.Join(t.TempDir(), "gonacos")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, agentv1.ConnectPath, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled Agent route: %d", response.Code)
	}
	for _, route := range []struct{ method, path string }{{"GET", "registration-policy"}, {"PUT", "registration-policy"}, {"POST", "deployments"}} {
		request := httptest.NewRequest(route.method, "https://example.test/api/v1/admin/workspaces/default/environments/default/agent/services/checkout/"+route.path, nil)
		request.Header.Set("Authorization", "Bearer "+app.BootstrapOwnerToken())
		result := httptest.NewRecorder()
		app.Handler().ServeHTTP(result, request)
		if result.Code != http.StatusNotFound {
			t.Fatalf("disabled registration management route %s: %d", route.path, result.Code)
		}
	}
}
