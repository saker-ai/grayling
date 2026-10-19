package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/websocket"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"google.golang.org/protobuf/proto"
)

var (
	ErrClosed       = errors.New("agent: session closed")
	ErrBackpressure = errors.New("agent: consumer capacity exceeded")
	ErrProtocol     = errors.New("agent: invalid server protocol")
)

// GoAwayError carries the minimum reconnect delay requested by the server.
type GoAwayError struct{ RetryAfter time.Duration }

func (e *GoAwayError) Error() string { return "agent: server requested reconnect" }

// HandshakeError is a sanitized HTTP rejection. RetryAfter is a minimum delay.
type HandshakeError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *HandshakeError) Error() string { return fmt.Sprintf("agent: handshake HTTP %d", e.StatusCode) }

// RemoteError omits the peer reason from Error() to prevent accidental logging
// of server-supplied sensitive data. Detail remains available for diagnostics.
type RemoteError struct{ Detail *agentv1.Error }

func (e *RemoteError) Error() string {
	return fmt.Sprintf("agent: remote error %s", e.Detail.GetCode())
}

// Options are copied by Dial. HTTPClient must not follow redirects: credentials
// belong to the configured origin. Dial enforces this even for a custom client.
type Options struct {
	URL        string
	Token      string
	Hello      *agentv1.Hello
	HTTPClient *http.Client
	// AllowInsecure permits ws only for explicitly configured local development.
	AllowInsecure    bool
	HandshakeTimeout time.Duration
	RequestTimeout   time.Duration
	EventCapacity    int
	MaxPending       int
}

func (o Options) normalized() (Options, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "wss" && !(o.AllowInsecure && u.Scheme == "ws")) {
		return o, errors.New("agent: expected WSS origin or connect URL without credentials, query or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = agentv1.ConnectPath
	}
	if u.Path != agentv1.ConnectPath {
		return o, errors.New("agent: invalid connect path")
	}
	if o.Token == "" || strings.ContainsAny(o.Token, "\r\n") {
		return o, errors.New("agent: missing or invalid token")
	}
	if o.Hello == nil {
		return o, errors.New("agent: missing hello")
	}
	o.Hello = proto.Clone(o.Hello).(*agentv1.Hello)
	if err := agentv1.Validate(&agentv1.Envelope{ProtocolMajor: 1, MessageId: "hello", Payload: &agentv1.Envelope_Hello{Hello: o.Hello}}); err != nil {
		return o, err
	}
	if o.HandshakeTimeout < 0 || o.RequestTimeout < 0 || o.EventCapacity < 0 || o.MaxPending < 0 {
		return o, errors.New("agent: negative option")
	}
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.EventCapacity == 0 {
		o.EventCapacity = 32
	}
	if o.MaxPending == 0 {
		o.MaxPending = 64
	}
	if o.EventCapacity > 32 || o.MaxPending > 1024 {
		return o, errors.New("agent: capacity exceeds safety limit")
	}
	hc := new(http.Client)
	if o.HTTPClient != nil {
		*hc = *o.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTPClient = hc
	o.URL = u.String()
	return o, nil
}

// Client is one authenticated session. It is safe for concurrent requests.
// A disconnect fails outstanding calls; mutations are never replayed implicitly.
// The caller owns applying resources and reporting Prepared/Applied.
type Client struct {
	conn    *websocket.Conn
	opts    Options
	welcome *agentv1.Envelope
	cancel  context.CancelCauseFunc
	done    chan struct{}
	events  chan *agentv1.Envelope
	slots   chan struct{}
	write   chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[string]chan *agentv1.Envelope
	err     error
}

// Dial authenticates, negotiates and starts bounded event/heartbeat processing.
// Canceling ctx terminates the session and all outstanding requests.
func Dial(ctx context.Context, options Options) (*Client, error) {
	o, err := options.normalized()
	if err != nil {
		return nil, err
	}
	handshake, cancel := context.WithTimeout(ctx, o.HandshakeTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(handshake, o.URL, &websocket.DialOptions{HTTPClient: o.HTTPClient, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + o.Token}}, Subprotocols: []string{agentv1.Subprotocol}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil {
			delay := time.Duration(0)
			if seconds, parseErr := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 32); parseErr == nil {
				delay = time.Duration(seconds) * time.Second
			} else if date, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil {
				delay = max(0, time.Until(date))
			}
			return nil, &HandshakeError{StatusCode: response.StatusCode, RetryAfter: delay}
		}
		// Transport errors can contain the URL or credentials from custom transports.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("agent: WebSocket connection failed")
	}
	success := false
	defer func() {
		if !success {
			_ = conn.CloseNow()
		}
	}()
	if conn.Subprotocol() != agentv1.Subprotocol {
		return nil, ErrProtocol
	}
	conn.SetReadLimit(agentv1.MaxFrameBytes)
	hello := &agentv1.Envelope{ProtocolMajor: 1, MessageId: uuid.New().String(), Payload: &agentv1.Envelope_Hello{Hello: o.Hello}}
	b, err := agentv1.Encode(hello)
	if err != nil {
		return nil, err
	}
	if err = conn.Write(handshake, websocket.MessageBinary, b); err != nil {
		return nil, errors.New("agent: hello write failed")
	}
	typ, b, err := conn.Read(handshake)
	if err != nil {
		return nil, errors.New("agent: welcome read failed")
	}
	if typ != websocket.MessageBinary {
		return nil, ErrProtocol
	}
	welcome, err := agentv1.Decode(b)
	if err != nil {
		return nil, err
	}
	if e := welcome.GetError(); e != nil {
		return nil, &RemoteError{Detail: e}
	}
	w := welcome.GetWelcome()
	if w == nil || welcome.ReplyTo != hello.MessageId || w.Scope == nil || w.Scope.WorkspaceId == "" || w.Scope.EnvironmentId == "" || w.Scope.ServiceKey == "" || w.ControlPlaneEpoch == 0 {
		return nil, ErrProtocol
	}
	for _, cap := range o.Hello.RequiredCapabilities {
		if !slices.Contains(w.Capabilities, cap) {
			return nil, ErrProtocol
		}
	}
	if !clockWithinBudget(w.ServerUnixMs) {
		return nil, ErrProtocol
	}
	limits := w.Limits
	if limits == nil || limits.MaxFrameBytes == 0 || limits.MaxFrameBytes > agentv1.MaxFrameBytes || limits.HeartbeatIntervalMs == 0 || limits.HeartbeatTimeoutMs <= limits.HeartbeatIntervalMs {
		return nil, ErrProtocol
	}
	conn.SetReadLimit(int64(limits.MaxFrameBytes))
	run, cancelRun := context.WithCancelCause(ctx)
	c := &Client{conn: conn, opts: o, welcome: welcome, cancel: cancelRun, done: make(chan struct{}), events: make(chan *agentv1.Envelope, o.EventCapacity), slots: make(chan struct{}, o.MaxPending), write: make(chan struct{}, 1), pending: make(map[string]chan *agentv1.Envelope)}
	c.wg.Go(func() { c.readLoop(run) })
	c.wg.Go(func() { c.heartbeatLoop(run) })
	c.wg.Go(func() { <-run.Done(); c.stop(context.Cause(run)) })
	success = true
	return c, nil
}

// Welcome returns a copy; link readiness is distinct from application readiness.
func (c *Client) Welcome() *agentv1.Envelope       { return proto.Clone(c.welcome).(*agentv1.Envelope) }
func (c *Client) Events() <-chan *agentv1.Envelope { return c.events }
func (c *Client) Done() <-chan struct{}            { return c.done }
func (c *Client) Err() error                       { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

func (c *Client) stop(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	if err == nil {
		err = ErrClosed
	}
	c.err = err
	close(c.done)
	c.mu.Unlock()
	c.cancel(err)
	_ = c.conn.CloseNow()
}

// Close interrupts I/O and waits for owned goroutines. It is idempotent.
func (c *Client) Close() error { c.stop(ErrClosed); c.wg.Wait(); return nil }

func clientMessage(e *agentv1.Envelope) bool {
	switch e.Payload.(type) {
	case *agentv1.Envelope_Heartbeat, *agentv1.Envelope_Goodbye, *agentv1.Envelope_Subscribe, *agentv1.Envelope_Unsubscribe, *agentv1.Envelope_Resume, *agentv1.Envelope_Register, *agentv1.Envelope_RenewLease, *agentv1.Envelope_UpdateInstance, *agentv1.Envelope_Deregister, *agentv1.Envelope_FetchResource, *agentv1.Envelope_ResourceApplied, *agentv1.Envelope_ResourceRejected, *agentv1.Envelope_Prepared, *agentv1.Envelope_Applied, *agentv1.Envelope_Rejected, *agentv1.Envelope_ReleaseStatusQuery, *agentv1.Envelope_ReadinessReport, *agentv1.Envelope_DrainReport, *agentv1.Envelope_EventBatch:
		return true
	default:
		return false
	}
}

func (c *Client) outgoing(ctx context.Context, e *agentv1.Envelope) (*agentv1.Envelope, error) {
	if e == nil {
		return nil, ErrProtocol
	}
	e = proto.Clone(e).(*agentv1.Envelope)
	if !clientMessage(e) {
		return nil, ErrProtocol
	}
	e.ProtocolMajor = agentv1.ProtocolMajor
	e.ProtocolMinor = c.welcome.ProtocolMinor
	e.SessionId = c.welcome.SessionId
	e.ConnectionEpoch = c.welcome.ConnectionEpoch
	e.MessageId = uuid.New().String()
	e.ReplyTo = ""
	if deadline, ok := ctx.Deadline(); ok && (e.DeadlineUnixMs == 0 || deadline.UnixMilli() < e.DeadlineUnixMs) {
		e.DeadlineUnixMs = deadline.UnixMilli()
	}
	return e, nil
}

func (c *Client) send(ctx context.Context, e *agentv1.Envelope) error {
	b, err := agentv1.Encode(e)
	if err != nil {
		return err
	}
	if len(b) > int(c.welcome.GetWelcome().Limits.MaxFrameBytes) {
		return ErrProtocol
	}
	select {
	case c.write <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
	defer func() { <-c.write }()
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	// A per-call context closes blocked writes without leaving a writer goroutine.
	if err = c.conn.Write(ctx, websocket.MessageBinary, b); err != nil {
		c.stop(errors.New("agent: WebSocket write failed"))
		return c.Err()
	}
	return nil
}

// Send sends a one-way report. Success means written, never applied or persisted.
// Mutations must carry a stable operation_id chosen by the caller.
func (c *Client) Send(ctx context.Context, e *agentv1.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	out, err := c.outgoing(ctx, e)
	if err != nil {
		return err
	}
	return c.send(ctx, out)
}

// Call correlates a response by reply_to with a bounded pending-request budget.
// An error after writing can mean an unknown outcome; retry only with the same
// operation_id after checking the command's idempotency contract.
func (c *Client) Call(ctx context.Context, e *agentv1.Envelope) (*agentv1.Envelope, error) {
	return c.call(ctx, e, false)
}

func (c *Client) call(ctx context.Context, e *agentv1.Envelope, heartbeat bool) (*agentv1.Envelope, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	if !heartbeat {
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, c.Err()
		}
		defer func() { <-c.slots }()
	}
	out, err := c.outgoing(ctx, e)
	if err != nil {
		return nil, err
	}
	if err = agentv1.Validate(out); err != nil {
		return nil, err
	}
	reply := make(chan *agentv1.Envelope, 1)
	c.mu.Lock()
	c.pending[out.MessageId] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, out.MessageId); c.mu.Unlock() }()
	if err = c.send(ctx, out); err != nil {
		return nil, err
	}
	select {
	case response := <-reply:
		if e := response.GetError(); e != nil {
			return nil, &RemoteError{Detail: e}
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		// A correlated NACK/result already received before transport closure wins
		// over a generic disconnect. Otherwise a fenced request can lose its cause.
		select {
		case response := <-reply:
			if e := response.GetError(); e != nil {
				return nil, &RemoteError{Detail: e}
			}
			return response, nil
		default:
			return nil, c.Err()
		}
	}
}

func (c *Client) readLoop(ctx context.Context) {
	defer func() { c.stop(context.Cause(ctx)); close(c.events) }()
	for {
		typ, b, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.stop(context.Cause(ctx))
			} else {
				c.stop(errors.New("agent: WebSocket read failed"))
			}
			return
		}
		if typ != websocket.MessageBinary {
			c.stop(ErrProtocol)
			return
		}
		e, err := agentv1.Decode(b)
		if err != nil {
			c.stop(err)
			return
		}
		if e.SessionId != c.welcome.SessionId || e.ConnectionEpoch != c.welcome.ConnectionEpoch || e.ProtocolMinor != c.welcome.ProtocolMinor || clientMessage(e) || e.GetHello() != nil || e.GetWelcome() != nil {
			c.stop(ErrProtocol)
			return
		}
		if e.ReplyTo != "" {
			c.mu.Lock()
			ch := c.pending[e.ReplyTo]
			c.mu.Unlock()
			// Late responses to timed-out calls must not become unsolicited commands.
			if ch != nil {
				select {
				case ch <- e:
				default:
				}
			}
			continue
		}
		// Capture terminal state before transferring message ownership to Events.
		var terminal error
		if away := e.GetGoAway(); away != nil {
			terminal = &GoAwayError{RetryAfter: time.Duration(away.RetryAfterMs) * time.Millisecond}
		}
		if remote := e.GetError(); remote != nil {
			terminal = &RemoteError{Detail: proto.Clone(remote).(*agentv1.Error)}
		}
		select {
		case c.events <- e:
		case <-ctx.Done():
			return
		default:
			c.stop(ErrBackpressure)
			return
		}
		if terminal != nil {
			c.stop(terminal)
			return
		}
	}
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	limits := c.welcome.GetWelcome().Limits
	ticker := time.NewTicker(time.Duration(limits.HeartbeatIntervalMs) * time.Millisecond)
	defer ticker.Stop()
	var nonce uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		nonce++
		beat, cancel := context.WithTimeout(ctx, time.Duration(limits.HeartbeatTimeoutMs)*time.Millisecond)
		response, err := c.call(beat, &agentv1.Envelope{Payload: &agentv1.Envelope_Heartbeat{Heartbeat: &agentv1.Heartbeat{Nonce: nonce}}}, true)
		cancel()
		if err != nil {
			c.stop(err)
			return
		}
		if response.GetHeartbeatAck() == nil || response.GetHeartbeatAck().Nonce != nonce || !clockWithinBudget(response.GetHeartbeatAck().ServerUnixMs) {
			c.stop(ErrProtocol)
			return
		}
	}
}
