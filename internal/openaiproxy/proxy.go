// Package openaiproxy relays the OpenAI Chat Completions endpoint to one
// operator-configured upstream. It does not interpret model requests or keys.
package openaiproxy

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// MaxRequestBytes bounds one chat request, including gateway-injected content.
const MaxRequestBytes = 32 << 20

// Proxy serves Chat Completions and owns its upstream connection pool.
type Proxy struct {
	handler   http.Handler
	transport *http.Transport
}

// New returns a Chat Completions handler. upstreamBase is an OpenAI-compatible
// base URL ending in /v1, for example https://api.openai.com/v1.
func New(upstreamBase string) (*Proxy, error) {
	base, err := url.Parse(upstreamBase)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		!strings.HasSuffix(strings.TrimSuffix(base.Path, "/"), "/v1") {
		return nil, fmt.Errorf("openai upstream must be an http(s) base URL ending in /v1")
	}
	target := base.Clone()
	target.Path = strings.TrimSuffix(base.Path, "/") + "/chat/completions"
	target.RawPath = ""
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL = target.Clone()
			req.Out.URL.RawQuery = req.In.URL.RawQuery
			req.Out.Host = target.Host
			// A browser may attach the Grayling console session cookie. Only the
			// client's model credential is relevant to this upstream.
			req.Out.Header.Del("Cookie")
			req.Out.Header.Del("Proxy-Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "chat request exceeds 32 MiB")
				return
			}
			WriteError(w, http.StatusBadGateway, "upstream_unavailable", "model upstream unavailable")
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := BearerKey(w, r); !ok {
			return
		}
		if r.ContentLength > MaxRequestBytes {
			WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "chat request exceeds 32 MiB")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		proxy.ServeHTTP(w, r)
	})
	return &Proxy{handler: handler, transport: transport}, nil
}

// ServeHTTP relays one Chat Completions request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.handler.ServeHTTP(w, r)
}

// CloseIdleConnections releases pooled upstream connections on shutdown.
func (p *Proxy) CloseIdleConnections() { p.transport.CloseIdleConnections() }

// BearerKey checks a model credential without changing or recording it.
func BearerKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(key) == "" {
		WriteError(w, http.StatusUnauthorized, "invalid_api_key", "a bearer API key is required")
		return "", false
	}
	key = strings.TrimSpace(key)
	// The "glab_" form is the pre-rename prefix. Tokens minted before the
	// rename are still valid — ResolveToken matches on the secret hash alone —
	// so an audience guard that only knew "grayling_" would forward them to the
	// model upstream. Both forms must be refused.
	if strings.HasPrefix(key, "grayling_") || strings.HasPrefix(key, "glab_") {
		WriteError(w, http.StatusForbidden, "wrong_audience", "Grayling credentials cannot be sent to the model upstream")
		return "", false
	}
	return key, true
}

// WriteError emits an OpenAI-compatible error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(map[string]any{"error": map[string]string{
		"message": message, "type": "api_error", "code": code,
	}})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Disabled answers requests when no upstream was configured.
func Disabled(w http.ResponseWriter, _ *http.Request) {
	WriteError(w, http.StatusNotImplemented, "not_configured", "OpenAI-compatible upstream is not configured")
}

// RateLimited reports a Grayling edge rate limit in the upstream error shape.
func RateLimited(w http.ResponseWriter, _ *http.Request) {
	WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "chat request rate exceeded")
}
