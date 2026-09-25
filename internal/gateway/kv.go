package gateway

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func jsonMarshal(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}

// KV rendering (Consul/etcd/Redis/ZooKeeper providers). Key layout follows
// the Traefik KV reference (lowercase path segments, list elements as
// /0, /1, ...), e.g.:
//
//	traefik/http/routers/<name>/rule
//	traefik/http/routers/<name>/parentrefs/0
//	traefik/http/middlewares/<name>/forwardauth/authresponseheaders/0
//	traefik/http/services/<name>/loadbalancer/servers/0/url
//
// test/gateway loads this exact key set into a real Redis and serves it
// through a real Traefik redis provider.

// DefaultKVRoot is the conventional Traefik KV root key.
const DefaultKVRoot = "traefik"

// KV renders the configuration as flat key/value pairs under root (empty
// root = DefaultKVRoot).
//
// KV-specific adaptation: a headers middleware that only BLANKS request
// headers (empty values) cannot round-trip through a KV store — Traefik's KV
// decoder drops empty values, so the middleware would vanish and the router
// referencing it would be rejected ("middleware does not exist"). The
// generated forwardAuth middleware therefore carries AuthResponseHeadersRegex,
// which strips every incoming X-Golab-* header on ALL providers; the KV
// renderer relies on that and omits the unrepresentable blank-headers
// middleware (and its router references). file/IngressRoute keep the explicit
// headers middleware because those providers represent empty values fine.
func (c *Configuration) KV(root string) (map[string]string, error) {
	if err := c.ValidateHierarchy(); err != nil {
		return nil, err
	}
	if root == "" {
		root = DefaultKVRoot
	}
	root = strings.TrimSuffix(root, "/")
	out := map[string]string{}
	put := func(k, v string) { out[root+"/"+k] = v }

	// Middlewares that blank headers only (no forwardAuth) are dropped for
	// KV; record their names so router references can be filtered.
	omitted := map[string]bool{}
	for _, m := range c.Middlewares {
		if m.ForwardAuth == nil {
			omitted[m.Name] = true
		}
	}

	for _, r := range c.Routers {
		base := fmt.Sprintf("http/routers/%s", r.Name)
		put(base+"/rule", r.Rule)
		if r.Service != "" {
			put(base+"/service", r.Service)
		}
		if r.Priority > 0 {
			put(base+"/priority", strconv.Itoa(r.Priority))
		}
		for i, ep := range r.EntryPoints {
			put(fmt.Sprintf("%s/entrypoints/%d", base, i), ep)
		}
		mi := 0
		for _, mw := range r.Middlewares {
			if omitted[mw] {
				continue // stripped by forwardAuth's authResponseHeadersRegex
			}
			put(fmt.Sprintf("%s/middlewares/%d", base, mi), mw)
			mi++
		}
		for i, p := range r.ParentRefs {
			put(fmt.Sprintf("%s/parentrefs/%d", base, i), p)
		}
	}
	for _, m := range c.Middlewares {
		if omitted[m.Name] {
			continue
		}
		base := fmt.Sprintf("http/middlewares/%s", m.Name)
		// Only forwardAuth middlewares survive the omission filter above
		// (headers-strip middlewares blank values, which KV cannot carry).
		put(base+"/forwardauth/address", m.ForwardAuth.Address)
		for i, h := range m.ForwardAuth.AuthResponseHeaders {
			put(fmt.Sprintf("%s/forwardauth/authresponseheaders/%d", base, i), h)
		}
		if m.ForwardAuth.AuthResponseHeadersRegex != "" {
			put(base+"/forwardauth/authresponseheadersregex", m.ForwardAuth.AuthResponseHeadersRegex)
		}
	}
	for _, s := range c.Services {
		base := fmt.Sprintf("http/services/%s", s.Name)
		for i, u := range s.Servers {
			put(fmt.Sprintf("%s/loadbalancer/servers/%d/url", base, i), u)
		}
	}
	return out, nil
}

// KVSorted renders the KV pair set deterministically (key-sorted) — the
// canonical form for diffs, review artifacts and KV-store writers.
func (c *Configuration) KVSorted(root string) ([]KVPair, error) {
	m, err := c.KV(root)
	if err != nil {
		return nil, err
	}
	out := make([]KVPair, 0, len(m))
	for k, v := range m {
		out = append(out, KVPair{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// KVPair is one rendered KV-store entry.
type KVPair struct {
	Key   string
	Value string
}
