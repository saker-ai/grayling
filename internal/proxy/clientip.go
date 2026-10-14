// Trusted-proxy-chain client IP derivation for the evaluation paths
// (docs/traefik-grayling-architecture.md "按 client IP 灰度").
//
// The proxy can inject the client IP it derives from the connection into
// the evaluation context under the reserved attribute name
// model.ClientIPField ("client_ip"), so flags can target IP ranges
// (in_cidr) or — discouraged for experiments, see pkg/model — bucket on it.
//
// Trust model (anti-forgery is the whole point):
//
//   - X-Forwarded-For is client-influenced input. Only the segment of the
//     chain the operator explicitly declared trusted
//     (Config.TrustedProxyCIDRs) may contribute to the derived IP.
//   - Derivation walks from the connection peer (RemoteAddr) right-to-left
//     through XFF ONLY while the current candidate IP lies inside a trusted
//     CIDR; the first IP outside the trusted set is the client IP and every
//     XFF entry further left is DISCARDED. A client prepending
//     "X-Forwarded-For: <internal-range>, <its real IP>" therefore cannot
//     move itself into another IP's gray group: the walk stops at the real
//     peer address recorded by the last trusted hop.
//   - No trusted CIDRs configured (the default) ⇒ NO XFF is trusted at
//     all: the derived client IP is the direct connection peer only. This
//     keeps existing deployments' behavior unchanged.
//   - A malformed XFF entry in a position the walk must consume breaks the
//     chain of trust: derivation fails closed (empty result, nothing is
//     injected) rather than guessing — absent client_ip matches no
//     constraint, so the flag falls through to its default rule.
//
// Injection precedence (documented contract, see model.ClientIPField):
//
//   - gateway forwardAuth path: the context is proxy-constructed, so the
//     derived value is always what lands in attributes["client_ip"];
//     request headers never feed it.
//   - /sdk/eval relay path: an explicit caller-provided
//     attributes["client_ip"] WINS and is never overwritten (the SDK caller
//     is an authenticated server-side client that may itself sit behind a
//     sanitized edge); the derived value is injected only when absent.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/saker-ai/grayling/pkg/model"
)

// parseTrustedProxyCIDRs parses the comma-separated TrustedProxyCIDRs
// configuration. Each entry is either a CIDR ("10.0.0.0/8", "fd00::/8") or
// a bare IP ("127.0.0.1" ⇒ /32, "::1" ⇒ /128). Empty input yields no
// trusted proxies (the safe default: XFF is never believed).
func parseTrustedProxyCIDRs(spec string) ([]netip.Prefix, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(part); err == nil {
			out = append(out, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf(
				"proxy: invalid TrustedProxyCIDRs entry %q (want a CIDR like 10.0.0.0/8 or a bare IP)", part)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// trustedProxyHit reports whether ip lies inside one of the configured
// trusted proxy ranges. ip must already be unmapped.
func (p *Proxy) trustedProxyHit(ip netip.Addr) bool {
	for _, pfx := range p.trustedProxies {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

// deriveClientIP applies the trust model documented at the top of this file
// and returns the derived client IP as a canonical string, or "" when it
// cannot be established (unparseable RemoteAddr, or a broken/malformed
// trusted chain — fail closed, inject nothing).
func (p *Proxy) deriveClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr // no port component (rare, e.g. hand-built requests)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	ip = ip.Unmap()

	// Default posture: no trusted proxies configured ⇒ the peer IS the
	// client; XFF is never consulted.
	if len(p.trustedProxies) == 0 || !p.trustedProxyHit(ip) {
		return ip.String()
	}

	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if strings.TrimSpace(xff) == "" {
		return ip.String()
	}
	entries := strings.Split(xff, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		if !p.trustedProxyHit(ip) {
			break // boundary found: the current candidate is the client
		}
		part := strings.TrimSpace(entries[i])
		if part == "" {
			continue // tolerate stray commas — carries no information
		}
		next, err := netip.ParseAddr(part)
		if err != nil {
			// The next hop recorded by a TRUSTED proxy is not a valid IP:
			// the chain of trust is broken. Fail closed rather than
			// serving a proxy's own address (which could accidentally
			// match internal-range targeting) or attacker-chosen text.
			return ""
		}
		ip = next.Unmap()
	}
	return ip.String()
}

// maxEvalBodyInjectBytes bounds how large a /sdk/eval request body may be
// for client_ip injection. Eval bodies are small JSON by contract; larger
// (or unknown-length oversized) bodies relay verbatim.
const maxEvalBodyInjectBytes = 1 << 20

// injectEvalClientIP rewrites a relayed POST /api/v1/sdk/eval request in
// place so the upstream evaluation context carries the derived client IP —
// ONLY when the caller did not explicitly provide attributes.client_ip
// (caller precedence) and the body is a well-formed JSON object. Anything
// unexpected (non-JSON, oversized, trailing data, non-object context) is
// relayed verbatim: the relay must never turn a working request into a
// broken one, and the upstream stays the validation point.
func (p *Proxy) injectEvalClientIP(r *http.Request) {
	if r.ContentLength > maxEvalBodyInjectBytes {
		return
	}
	clientIP := p.deriveClientIP(r)
	if clientIP == "" {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEvalBodyInjectBytes+1))
	if err != nil {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return
	}
	if len(body) > maxEvalBodyInjectBytes {
		// Unknown-length oversized body: restore the unread remainder and
		// relay verbatim (ContentLength was already -1/unknown).
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return
	}
	out, ok := injectClientIPAttr(body, clientIP)
	if !ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
}

// injectClientIPAttr adds context.attributes.client_ip to a /sdk/eval JSON
// body when absent. Numbers round-trip through json.Number so large
// integers keep full precision. ok=false means "relay the original bytes".
func injectClientIPAttr(body []byte, clientIP string) (out []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, false
	}
	if dec.More() {
		return nil, false // trailing data after the JSON object: upstream decides
	}
	var evalCtx map[string]any
	switch v := doc["context"].(type) {
	case nil:
		evalCtx = map[string]any{}
	case map[string]any:
		evalCtx = v
	default:
		return nil, false // context is not an object — verbatim relay
	}
	attrs, _ := evalCtx["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	if _, exists := attrs[model.ClientIPField]; exists {
		// Explicit caller-provided value wins — NEVER overwritten.
		return nil, false
	}
	attrs[model.ClientIPField] = clientIP
	evalCtx["attributes"] = attrs
	doc["context"] = evalCtx
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}
