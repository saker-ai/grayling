// Package proxy is the standalone GoLab data-plane proxy: a stateless,
// independently deployable relay for the remote-evaluation surface
// (docs/技术方案.md §3: GoLab proxy 可选，用于浏览器受限快照、远程求值、SSE
// 转发；不要求全部应用请求经过 proxy).
//
// Relayed endpoints (upstream authorization is passed through unchanged —
// credentials still decide workspace/environment, §9):
//
//	POST /api/v1/sdk/eval     remote evaluation (server-side exposure capture)
//	GET  /api/v1/sdk/stream   SSE revision invalidation (flushing relay)
//	POST /api/v1/collect      exposure/conversion ingestion
//	GET  /api/v1/sdk/config   scoped snapshot pull (ETag/304 passthrough)
//	GET  /api/v1/sdk/revision restricted freshness probe (revision only)
//
// Proxy-owned endpoints:
//
//	GET /healthz              liveness — process is up (no upstream call)
//	GET /api/v1/health        readiness — probes the upstream health endpoint
//
// The proxy keeps NO state and NO credentials of its own: timeouts, hop-by-hop
// header hygiene and connection pooling are its only concerns. Per-token rate
// limiting stays on the upstream, which is the only component that can
// authenticate.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config parameterizes a Proxy.
type Config struct {
	// Upstream is the GoLab server base URL, e.g. http://127.0.0.1:8688.
	Upstream string
	// RequestTimeout bounds one unary relay (eval/collect/config);
	// default 10s. The SSE relay is NOT bounded by it (streams are
	// long-lived by design; client disconnect cancels the upstream call).
	RequestTimeout time.Duration
	// HealthTimeout bounds the upstream readiness probe (default 3s).
	HealthTimeout time.Duration
	// MaxIdleConnsPerHost sizes the keep-alive pool toward the upstream
	// (default 100). MaxConnsPerHost bounds total upstream connections
	// (0 = unlimited).
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	// DialTimeout bounds TCP establishment (default 5s).
	DialTimeout time.Duration
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.RequestTimeout <= 0 {
		out.RequestTimeout = 10 * time.Second
	}
	if out.HealthTimeout <= 0 {
		out.HealthTimeout = 3 * time.Second
	}
	if out.MaxIdleConnsPerHost <= 0 {
		out.MaxIdleConnsPerHost = 100
	}
	if out.DialTimeout <= 0 {
		out.DialTimeout = 5 * time.Second
	}
	return out
}

// Proxy relays the GoLab remote-evaluation surface to an upstream server.
// Safe for concurrent use.
type Proxy struct {
	cfg      Config
	upstream *url.URL
	// unary serves eval/collect/config with a hard total timeout; stream
	// serves the SSE relay with NO total timeout (only dial/response-header
	// bounds) so long-lived event streams are not killed mid-flight.
	unary  *http.Client
	stream *http.Client
}

// New validates the config and builds the shared connection pools.
func New(cfg Config) (*Proxy, error) {
	if strings.TrimSpace(cfg.Upstream) == "" {
		return nil, errors.New("proxy: Upstream is required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.Upstream, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q", cfg.Upstream)
	}
	c := cfg.withDefaults()
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   c.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          c.MaxIdleConnsPerHost * 2,
		MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
		MaxConnsPerHost:       c.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	streamTransport := transport.Clone()
	// Bound only the header phase for streams: the upstream sends the first
	// SSE event immediately after subscribe, so a silent upstream is an error
	// while a streaming one must stay open.
	streamTransport.ResponseHeaderTimeout = c.RequestTimeout
	return &Proxy{
		cfg:      c,
		upstream: u,
		unary:    &http.Client{Transport: transport, Timeout: c.RequestTimeout},
		stream:   &http.Client{Transport: streamTransport},
	}, nil
}

// CloseIdleConnections releases pooled upstream connections (shutdown).
func (p *Proxy) CloseIdleConnections() {
	p.unary.CloseIdleConnections()
	p.stream.CloseIdleConnections()
}

// Handler returns the routed proxy handler.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", p.handleLivez)
	mux.HandleFunc("GET /api/v1/health", p.handleReadyz)
	mux.HandleFunc("POST /api/v1/sdk/eval", p.relayUnary)
	mux.HandleFunc("GET /api/v1/sdk/config", p.relayUnary)
	mux.HandleFunc("GET /api/v1/sdk/revision", p.relayUnary)
	mux.HandleFunc("POST /api/v1/collect", p.relayUnary)
	mux.HandleFunc("GET /api/v1/sdk/stream", p.relayStream)
	return mux
}

// handleLivez answers liveness without touching the upstream: a restarting or
// degraded upstream must not make orchestrators kill the proxy fleet.
func (p *Proxy) handleLivez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{"status":"ok","service":"golab-proxy","check":"liveness"}`)
}

// handleReadyz probes the upstream health endpoint; 503 while unreachable.
func (p *Proxy) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream.String()+"/api/v1/health", nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("upstream_error", err.Error()))
		return
	}
	resp, err := p.unary.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errBody("upstream_unreachable", classify(err)))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusServiceUnavailable,
			errBody("upstream_unhealthy", fmt.Sprintf("HTTP %d", resp.StatusCode)))
		return
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"status":"ok","service":"golab-proxy","check":"readiness","upstream":%s}`,
		string(body)))
}

func (p *Proxy) relayUnary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.RequestTimeout)
	defer cancel()
	upReq, err := p.newUpstreamRequest(ctx, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad_request", err.Error()))
		return
	}
	resp, err := p.unary.Do(upReq)
	if err != nil {
		status := http.StatusBadGateway
		code := "upstream_unreachable"
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		writeJSON(w, status, errBody(code, classify(err)))
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// relayStream relays the SSE revision stream with per-chunk flushing and no
// total timeout. Client disconnect cancels the upstream subscription.
func (p *Proxy) relayStream(w http.ResponseWriter, r *http.Request) {
	upReq, err := p.newUpstreamRequest(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad_request", err.Error()))
		return
	}
	resp, err := p.stream.Do(upReq)
	if err != nil {
		status := http.StatusBadGateway
		code := "upstream_unreachable"
		if isTimeout(err) {
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
		}
		writeJSON(w, status, errBody(code, classify(err)))
		return
	}
	defer resp.Body.Close()

	flusher, canFlush := w.(http.Flusher)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if !canFlush || resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 64*1024))
		return
	}
	// Read/write pump: every upstream chunk is forwarded and flushed
	// immediately so SSE event boundaries survive the hop.
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return // downstream gone; ctx cancels the upstream request
			}
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// hopByHop headers must not be forwarded in either direction (RFC 9110 §7.6.1).
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func (p *Proxy) newUpstreamRequest(ctx context.Context, r *http.Request) (*http.Request, error) {
	target := *p.upstream
	target.Path = singleJoin(p.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	upReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), r.Body)
	if err != nil {
		return nil, err
	}
	upReq.ContentLength = r.ContentLength
	// Copy end-to-end headers; Authorization passes through untouched so the
	// upstream stays the single authorization point (§9).
	for name, values := range r.Header {
		if hopByHop[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range values {
			upReq.Header.Add(name, v)
		}
	}
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := upReq.Header.Get("X-Forwarded-For"); prior != "" {
			upReq.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			upReq.Header.Set("X-Forwarded-For", clientIP)
		}
	}
	return upReq, nil
}

func singleJoin(base, path string) string {
	switch {
	case base == "" || base == "/":
		return path
	default:
		return strings.TrimSuffix(base, "/") + path
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if hopByHop[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func classify(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func errBody(code, msg string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, msg)
}
