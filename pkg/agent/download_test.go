package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestDownloadBoundaries(t *testing.T) {
	raw, err := proto.Marshal(&agentv1.SignedResource{Claims: []byte("claims"), Payload: []byte("payload")})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	var origin string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing auth")
		}
		switch r.URL.Path {
		case "/resource":
			_, _ = w.Write(raw)
		case "/oversize":
			_, _ = w.Write(append(raw, 0))
		case "/redirect":
			http.Redirect(w, r, origin+"/resource", 307)
		default:
			http.Error(w, "missing", 404)
		}
	}))
	defer server.Close()
	origin = server.URL
	c := &Client{opts: Options{URL: "wss" + strings.TrimPrefix(server.URL, "https"), Token: "token", HTTPClient: server.Client(), RequestTimeout: time.Second}, welcome: &agentv1.Envelope{Payload: &agentv1.Envelope_Welcome{Welcome: &agentv1.Welcome{Scope: &agentv1.Scope{WorkspaceId: "w", EnvironmentId: "e", ServiceKey: "s"}, Limits: &agentv1.Limits{MaxResourceBytes: MaxResourceBytes}}}}}
	// Production clients already disable redirect following during normalization.
	c.opts.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := &agentv1.ResourceRef{Scope: c.welcome.GetWelcome().Scope, Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "p", Revision: 1, HttpsUrl: server.URL + "/resource", Sha256: hex.EncodeToString(hash[:]), SizeBytes: uint64(len(raw)), ExpiresUnixMs: time.Now().Add(time.Minute).UnixMilli()}
	tests := map[string]struct {
		edit func(*agentv1.ResourceRef)
		want bool
	}{
		"valid":        {func(*agentv1.ResourceRef) {}, true},
		"digest":       {func(r *agentv1.ResourceRef) { r.Sha256 = strings.Repeat("0", 64) }, false},
		"scope":        {func(r *agentv1.ResourceRef) { r.Scope.WorkspaceId = "other" }, false},
		"expired":      {func(r *agentv1.ResourceRef) { r.ExpiresUnixMs = 1 }, false},
		"cross-origin": {func(r *agentv1.ResourceRef) { r.HttpsUrl = "https://example.invalid/resource" }, false},
		"query":        {func(r *agentv1.ResourceRef) { r.HttpsUrl += "?token=secret" }, false},
		"oversize":     {func(r *agentv1.ResourceRef) { r.HttpsUrl = server.URL + "/oversize" }, false},
		"redirect":     {func(r *agentv1.ResourceRef) { r.HttpsUrl = server.URL + "/redirect" }, false},
		"limit":        {func(r *agentv1.ResourceRef) { r.SizeBytes = MaxResourceBytes + 1 }, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ref := proto.Clone(base).(*agentv1.ResourceRef)
			tt.edit(ref)
			resource, err := c.Download(t.Context(), ref)
			if (err == nil) != tt.want {
				t.Fatalf("resource=%v err=%v", resource, err)
			}
		})
	}
}

func TestRunReconnectAndCancellation(t *testing.T) {
	var sessions int
	o := serverOptions(t, nil, func(ctx context.Context, c *websocket.Conn, e *agentv1.Envelope) { _ = c.CloseNow() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := Run(ctx, func(context.Context) (Options, error) { return o, nil }, func(c *Client) error {
		sessions++
		if sessions == 2 {
			return nil
		}
		_ = c.SendMessage(ctx, &agentv1.ReleaseStatusQuery{}, Metadata{})
		<-c.Done()
		return c.Err()
	})
	if err != nil || sessions != 2 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if err := Run(canceled, func(context.Context) (Options, error) { return o, nil }, func(*Client) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRunPermanentAuthenticationFailure(t *testing.T) {
	var attempts int
	o := serverOptions(t, nil, nil)
	o.Token = "wrong"
	err := Run(t.Context(), func(context.Context) (Options, error) { attempts++; return o, nil }, func(*Client) error { t.Error("unexpected session"); return nil })
	rejection, ok := errors.AsType[*HandshakeError](err)
	if attempts != 1 || !ok || rejection.StatusCode != 401 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}
