//go:build integration

package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	control "github.com/saker-ai/grayling/internal/agent"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/platform"
	client "github.com/saker-ai/grayling/pkg/agent"
	"github.com/saker-ai/grayling/test/pgtest"
)

func channelServer(t *testing.T, sessions *control.Sessions) (*httptest.Server, *control.Channel) {
	t.Helper()
	return channelServerWithPublisher(t, sessions, nil)
}
func channelServerWithPublisher(t *testing.T, sessions *control.Sessions, publisher *control.Publisher) (*httptest.Server, *control.Channel) {
	t.Helper()
	return channelServerWithOptions(t, sessions, control.ChannelOptions{DiscoveryPublisher: publisher})
}
func channelServerWithOptions(t *testing.T, sessions *control.Sessions, opts control.ChannelOptions) (*httptest.Server, *control.Channel) {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	opts.AllowedHosts = []string{srv.Listener.Addr().String()}
	opts.MaxConnections = 20
	channel, err := control.NewChannel(sessions, opts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv.Config.Handler = api.NewServer(store, nil, api.Options{AgentChannel: channel, DevAutoLogin: true}).Handler()
	srv.StartTLS()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := channel.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		srv.Close()
	})
	return srv, channel
}
func dialAgent(t *testing.T, srv *httptest.Server, secret string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(srv.URL, "https")+agentv1.ConnectPath, &websocket.DialOptions{HTTPClient: srv.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + secret}}, Subprotocols: []string{agentv1.Subprotocol}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}
func writeEnvelope(t *testing.T, c *websocket.Conn, e *agentv1.Envelope) {
	t.Helper()
	b, err := agentv1.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}
func readEnvelope(t *testing.T, c *websocket.Conn) *agentv1.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Fatal("nonbinary response")
	}
	e, err := agentv1.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func handshake(t *testing.T, c *websocket.Conn, g control.Grant) *agentv1.Envelope {
	t.Helper()
	writeEnvelope(t, c, &agentv1.Envelope{ProtocolMajor: 1, MessageId: "hello", Payload: &agentv1.Envelope_Hello{Hello: helloFor(g)}})
	e := readEnvelope(t, c)
	if e.GetWelcome() == nil || e.SessionId == "" || e.ConnectionEpoch == 0 || e.GetWelcome().ControlPlaneEpoch == 0 {
		t.Fatalf("invalid welcome: %v", e)
	}
	return e
}
func heartbeat(welcome *agentv1.Envelope) *agentv1.Envelope {
	return &agentv1.Envelope{ProtocolMajor: 1, MessageId: "heartbeat", SessionId: welcome.SessionId, ConnectionEpoch: welcome.ConnectionEpoch, Payload: &agentv1.Envelope_Heartbeat{Heartbeat: &agentv1.Heartbeat{Nonce: 123}}}
}
func TestWSSReplicaFencingRevocationAndShutdown(t *testing.T) {
	dsn := pgtest.DSN(t)
	a := openSessions(t, dsn)
	b := openSessions(t, dsn)
	g := grant()
	if err := a.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	id, secret := issue(t, a, g)
	firstServer, _ := channelServer(t, a)
	secondServer, secondChannel := channelServer(t, b)
	resp, err := firstServer.Client().Get(firstServer.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("shared TLS listener health failed")
	}
	first := dialAgent(t, firstServer, secret)
	welcome1 := handshake(t, first, g)
	writeEnvelope(t, first, heartbeat(welcome1))
	ack := readEnvelope(t, first)
	if ack.GetHeartbeatAck().GetNonce() != 123 || ack.ReplyTo != "heartbeat" {
		t.Fatal("heartbeat correlation failed")
	}
	second := dialAgent(t, secondServer, secret)
	welcome2 := handshake(t, second, g)
	if welcome2.ConnectionEpoch <= welcome1.ConnectionEpoch {
		t.Fatal("cross-node reconnect did not advance epoch")
	}
	// Old node closes proactively even when the old peer sends no more frames.
	old := readEnvelope(t, first)
	if old.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_FENCED {
		t.Fatalf("old session: %v", old)
	}
	if err := a.RevokeCredential(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	revoked := readEnvelope(t, second)
	if revoked.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_FENCED {
		t.Fatalf("revocation not propagated: %v", revoked)
	}
	_, secret2 := issue(t, b, g)
	third := dialAgent(t, secondServer, secret2)
	_ = handshake(t, third, g)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- secondChannel.Shutdown(ctx) }()
	if e := readEnvelope(t, third); e.GetGoAway() == nil {
		t.Fatalf("shutdown: %v", e)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
func TestWSSGuardsAndCapabilities(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	srv, ch := channelServer(t, s)
	for _, tt := range []struct {
		name, query, origin, cookie, token, host string
		want                                     int
	}{
		{name: "query credential", query: "?token=forbidden", token: secret, want: 403},
		{name: "browser origin", origin: srv.URL, token: secret, want: 403},
		{name: "developer cookie", cookie: "grayling_console=developer", token: secret, want: 403},
		{name: "admin audience", token: "grayling_admin_invalid", want: 401},
		{name: "unknown token", token: "grayling_agent_" + strings.Repeat("a", 43), want: 401},
		{name: "wrong host", host: "untrusted.example", token: secret, want: 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+agentv1.ConnectPath+tt.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+tt.token)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.cookie != "" {
				r.Header.Set("Cookie", tt.cookie)
			}
			if tt.host != "" {
				r.Host = tt.host
			}
			resp, err := srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("got %d; want %d", resp.StatusCode, tt.want)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "http://"+srv.Listener.Addr().String()+agentv1.ConnectPath, nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	rr := httptest.NewRecorder()
	ch.ServeHTTP(rr, request)
	if rr.Code != 403 {
		t.Fatal("plaintext accepted")
	}
	conn := dialAgent(t, srv, secret)
	h := helloFor(g)
	h.RequiredCapabilities = []string{"remote-shell"}
	writeEnvelope(t, conn, &agentv1.Envelope{ProtocolMajor: 1, MessageId: "hello", Payload: &agentv1.Envelope_Hello{Hello: h}})
	e := readEnvelope(t, conn)
	if e.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE || e.SessionId != "" {
		t.Fatalf("unknown required capability: %v", e)
	}
	rows := pgtest.Query(t, dsn, `SELECT count(*) FROM agent_sessions`)
	if rows[0][0] != "0" {
		t.Fatal("rejected hello created session")
	}
	conn2 := dialAgent(t, srv, secret)
	w := handshake(t, conn2, g)
	writeEnvelope(t, conn2, &agentv1.Envelope{ProtocolMajor: 1, MessageId: "subscribe", SessionId: w.SessionId, ConnectionEpoch: w.ConnectionEpoch, Payload: &agentv1.Envelope_Subscribe{Subscribe: &agentv1.Subscribe{Scope: &agentv1.Scope{WorkspaceId: "other"}}}})
	if e := readEnvelope(t, conn2); e.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatal("cross-scope subscription accepted")
	}
	bad := heartbeat(w)
	bad.ConnectionEpoch++
	writeEnvelope(t, conn2, bad)
	if e := readEnvelope(t, conn2); e.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_FENCED {
		t.Fatal("wrong epoch accepted")
	}
}

func TestGoAgentClientSession(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	srv, _ := channelServer(t, s)
	endpoint := "wss" + strings.TrimPrefix(srv.URL, "https") + agentv1.ConnectPath
	opts := client.Options{URL: endpoint, Token: secret, Hello: helloFor(g), HTTPClient: srv.Client()}
	opts.Hello.RequiredCapabilities = []string{"session"}
	conn, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := conn.Welcome()
	before.SessionId = "mutated"
	if err := clientHeartbeat(t, conn); err != nil {
		t.Fatal(err)
	}
	replacement, err := client.Dial(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	expectCode(t, clientHeartbeat(t, conn), agentv1.ErrorCode_ERROR_CODE_FENCED)
	if err := clientHeartbeat(t, replacement); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"ws" + strings.TrimPrefix(endpoint, "wss"), endpoint + "?token=forbidden", "wss://user:pass@example.test" + agentv1.ConnectPath} {
		bad := opts
		bad.URL = invalid
		if got, err := client.Dial(t.Context(), bad); err == nil {
			got.Close()
			t.Fatal("invalid endpoint accepted")
		}
	}
}

func TestWSSRejectsNonbinaryCorruptionAndUnavailableBackend(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openSessions(t, dsn)
	g := grant()
	if err := s.SetScopeLimit(t.Context(), g.Scope, 2); err != nil {
		t.Fatal(err)
	}
	_, secret := issue(t, s, g)
	srv, _ := channelServer(t, s)
	for _, tt := range []struct {
		name    string
		kind    websocket.MessageType
		payload []byte
	}{
		{"text message", websocket.MessageText, []byte("not protobuf")},
		{"malformed protobuf", websocket.MessageBinary, []byte{0xff}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := dialAgent(t, srv, secret)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := c.Write(ctx, tt.kind, tt.payload); err != nil {
				t.Fatal(err)
			}
			e := readEnvelope(t, c)
			if e.GetError() == nil {
				t.Fatal("invalid message not rejected")
			}
		})
	}
	c := dialAgent(t, srv, secret)
	_ = handshake(t, c, g)
	s.Close()
	if e := readEnvelope(t, c); e.GetError().GetCode() != agentv1.ErrorCode_ERROR_CODE_BACKEND_UNAVAILABLE {
		t.Fatalf("closed backend: %v", e)
	}
}

func clientHeartbeat(t *testing.T, c *client.Client) error {
	t.Helper()
	reply, err := c.CallMessage(t.Context(), &agentv1.Heartbeat{Nonce: 123}, client.Metadata{})
	if err != nil {
		return err
	}
	if reply.GetHeartbeatAck().GetNonce() != 123 {
		t.Fatal("heartbeat correlation lost")
	}
	return nil
}
