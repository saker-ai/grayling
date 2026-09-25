// Package gateway generates Traefik ≥3.6 multi-layer routing configuration
// for GoLab-driven traffic splitting (docs/技术方案.md §4,
// docs/traefik-golab-architecture.md).
//
// The generated hierarchy implements the P5 gateway chain:
//
//	parent router (no service, owns entryPoints)
//	  ├─ middleware ①: headers — strip client-supplied X-Golab-* headers
//	  │                (variant headers are ONLY trusted from the eval service)
//	  └─ middleware ②: forwardAuth → GoLab 求值服务 (golab-proxy
//	                   /api/v1/gateway/forwardauth), authResponseHeaders
//	                   write X-Golab-Variant back onto the request
//	child routers (parentRefs → parent)
//	  ├─ Header(`X-Golab-Variant`, `<variant>`) → variant backend
//	  └─ fallback (parent rule)                 → default variant backend
//
// Multi-layer routing is only supported by the file, KV and Kubernetes CRD
// (IngressRoute) providers, so this package renders exactly those three
// targets. Every rendered configuration passes ValidateHierarchy — the
// layering rules from the Traefik docs: root routers own entryPoints/tls and
// (when they have children) no service; intermediate routers have no service;
// leaf routers must have a service.
//
// Ratio changes never touch the generated configuration: the child routers
// match on the header the eval service writes, and the eval service reads the
// split from the GoLab flag. Only adding/removing variants changes routers.
package gateway

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Header contract between the generated configuration and the GoLab
// forwardAuth evaluation service (internal/proxy gateway endpoint).
const (
	// VariantHeader carries the evaluated variation name; child routers
	// match on it. Stripped from external requests, re-written only via
	// forwardAuth authResponseHeaders.
	VariantHeader = "X-Golab-Variant"
	// FlagKeyHeader echoes the evaluated flag key (diagnostics/attribution).
	FlagKeyHeader = "X-Golab-Flag-Key"
	// FailOpenHeader is set by the eval service when it fail-opens to the
	// default variant (reason code). Never set on a healthy evaluation.
	FailOpenHeader = "X-Golab-Fail-Open"
	// ReasonHeader carries the GoLab evaluation reason (SPLIT/DEFAULT/...).
	ReasonHeader = "X-Golab-Reason"
	// DefaultUnitHeader carries the bucketing key on client requests.
	DefaultUnitHeader = "X-Bucketing-Key"
	// DefaultUnitField is the default evaluation-context field the bucketing
	// key is placed into.
	DefaultUnitField = "targetingKey"
	// ForwardAuthPath is the golab-proxy endpoint the generated forwardAuth
	// middlewares point at.
	ForwardAuthPath = "/api/v1/gateway/forwardauth"
)

// managedHeaders are the headers stripped from external requests by the
// generated parent-router middleware: routing-relevant GoLab headers must
// only ever come from the eval service response.
var managedHeaders = []string{VariantHeader, FlagKeyHeader, FailOpenHeader, ReasonHeader}

// AuthResponseHeaders lists the eval-service response headers Traefik copies
// back onto the request (child-router matching + backend diagnostics).
var AuthResponseHeaders = []string{VariantHeader, FlagKeyHeader, FailOpenHeader, ReasonHeader}

// AuthResponseHeadersRegex is set on the forwardAuth middleware so Traefik
// strips EVERY incoming X-Golab-* header before copying back only the headers
// the eval service actually returned. This is the provider-portable anti-
// forgery mechanism: unlike a headers middleware with empty values (which the
// KV providers cannot represent — empty values are dropped by Traefik's KV
// decoder), a regex value round-trips through file, KV and IngressRoute
// identically. It also protects future X-Golab-* headers automatically.
const AuthResponseHeadersRegex = "^X-Golab-"

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	variantRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// VariantBackend binds one GoLab variation name to the backend serving it.
type VariantBackend struct {
	// Variant is the GoLab variation name (the X-Golab-Variant value the
	// eval service writes for it).
	Variant string
	// URL is the backend server URL (file/KV targets).
	URL string
	// K8sService / K8sPort name the Kubernetes Service backing this variant
	// (IngressRoute target only).
	K8sService string
	K8sPort    int
}

// RouteSpec describes one gateway-split route. All generated object names are
// derived from Name: <name>-parent, <name>-v-<variant>, <name>-default,
// <name>-strip, <name>-eval, <name>-svc-<variant>.
type RouteSpec struct {
	// Name is the route identity; DNS-1123 style ([a-z0-9-]) so the same
	// spec can render every target.
	Name string
	// EntryPoint is the Traefik entryPoint the root router exclusively owns.
	EntryPoint string
	// Rule is the parent-router match rule (Traefik rule syntax), e.g.
	// "PathPrefix(`/api`)". The fallback child router reuses it.
	Rule string
	// Priority optionally pins the parent router priority (0 = Traefik
	// default rule-length priority).
	Priority int

	// FlagKey is the GoLab flag the eval service evaluates.
	FlagKey string
	// DefaultVariant is the variation served when evaluation fail-opens or
	// the unit is missing; it must be one of Backends.
	DefaultVariant string
	// UnitField is the evaluation-context field the bucketing key is placed
	// into (must match the flag's bucketing unit; empty = "targetingKey").
	UnitField string
	// UnitHeader is the client request header carrying the bucketing key
	// (empty = "X-Bucketing-Key").
	UnitHeader string
	// EvalAddress is the base URL of the GoLab forwardAuth evaluation
	// service (golab-proxy), e.g. "http://127.0.0.1:8689". The generated
	// middleware appends ForwardAuthPath and its query parameters.
	EvalAddress string
	// Secret is the shared secret proving the forwardAuth call comes from
	// this generated route (golab-proxy -gateway-secret). Without it, anyone
	// who can reach the eval service could probe arbitrary flags through the
	// endpoint; STRONGLY recommended for any network-reachable deployment.
	// Empty omits the parameter (loopback/trusted-mesh setups).
	Secret string

	// Backends binds every variation (including DefaultVariant) to its
	// backend. At least one non-default variant is required — a split with
	// a single arm needs no gateway configuration.
	Backends []VariantBackend

	// Namespace is the Kubernetes namespace for the IngressRoute target
	// (ignored by file/KV).
	Namespace string
}

func (s *RouteSpec) unitField() string {
	if s.UnitField == "" {
		return DefaultUnitField
	}
	return s.UnitField
}

func (s *RouteSpec) unitHeader() string {
	if s.UnitHeader == "" {
		return DefaultUnitHeader
	}
	return s.UnitHeader
}

// EvalURL renders the forwardAuth address for this spec: the eval service
// base URL + the well-known path + the (escaped) per-route query parameters.
// Traefik sends the forwardAuth request to this address verbatim, preserving
// the query string (verified against Traefik v3.7.13 in test/gateway).
func (s *RouteSpec) EvalURL() string {
	q := url.Values{}
	q.Set("flagKey", s.FlagKey)
	q.Set("defaultVariant", s.DefaultVariant)
	q.Set("unitField", s.unitField())
	q.Set("unitHeader", s.unitHeader())
	if s.Secret != "" {
		q.Set("secret", s.Secret)
	}
	base := strings.TrimSuffix(s.EvalAddress, "/") + ForwardAuthPath
	return base + "?" + q.Encode()
}

// Validate checks the spec in isolation (names, charsets, backend set).
func (s *RouteSpec) Validate() error {
	var errs []string
	if s == nil {
		return errors.New("gateway: nil RouteSpec")
	}
	if !nameRe.MatchString(s.Name) {
		errs = append(errs, fmt.Sprintf("Name %q must match %s", s.Name, nameRe))
	}
	if s.EntryPoint == "" {
		errs = append(errs, "EntryPoint is required (root routers exclusively own entryPoints)")
	}
	if strings.TrimSpace(s.Rule) == "" {
		errs = append(errs, "Rule is required")
	}
	if s.FlagKey == "" {
		errs = append(errs, "FlagKey is required")
	}
	if !variantRe.MatchString(s.DefaultVariant) {
		errs = append(errs, fmt.Sprintf("DefaultVariant %q must match %s", s.DefaultVariant, variantRe))
	}
	if !variantRe.MatchString(s.unitField()) {
		errs = append(errs, fmt.Sprintf("UnitField %q must match %s", s.UnitField, variantRe))
	}
	if s.EvalAddress == "" {
		errs = append(errs, "EvalAddress is required")
	} else if u, err := url.Parse(s.EvalAddress); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Sprintf("EvalAddress %q must be an absolute http(s) URL", s.EvalAddress))
	}
	seen := map[string]bool{}
	hasDefault := false
	nonDefault := 0
	for i, b := range s.Backends {
		if !variantRe.MatchString(b.Variant) {
			errs = append(errs, fmt.Sprintf("Backends[%d].Variant %q must match %s", i, b.Variant, variantRe))
			continue
		}
		if seen[b.Variant] {
			errs = append(errs, fmt.Sprintf("Backends[%d].Variant %q is duplicated", i, b.Variant))
			continue
		}
		seen[b.Variant] = true
		if b.Variant == s.DefaultVariant {
			hasDefault = true
		} else {
			nonDefault++
		}
		if b.URL == "" {
			errs = append(errs, fmt.Sprintf("Backends[%d] (%s): URL is required", i, b.Variant))
		} else if u, err := url.Parse(b.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("Backends[%d] (%s): URL %q must be absolute http(s)", i, b.Variant, b.URL))
		}
	}
	if len(s.Backends) > 0 && !hasDefault {
		errs = append(errs, fmt.Sprintf("DefaultVariant %q has no backend", s.DefaultVariant))
	}
	if nonDefault < 1 {
		errs = append(errs, "at least one non-default variant backend is required")
	}
	if len(errs) > 0 {
		return fmt.Errorf("gateway: invalid RouteSpec: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Object names (exported so tests/admin tooling can reference them).

func (s *RouteSpec) ParentRouter() string  { return s.Name + "-parent" }
func (s *RouteSpec) DefaultRouter() string { return s.Name + "-default" }
func (s *RouteSpec) VariantRouter(v string) string {
	return s.Name + "-v-" + strings.ToLower(v)
}
func (s *RouteSpec) StripMiddleware() string { return s.Name + "-strip" }
func (s *RouteSpec) EvalMiddleware() string  { return s.Name + "-eval" }
func (s *RouteSpec) ServiceName(v string) string {
	return s.Name + "-svc-" + strings.ToLower(v)
}

// HeaderRule renders the child-router rule matching one variant header value.
func HeaderRule(variant string) string {
	return fmt.Sprintf("Header(`%s`, `%s`)", VariantHeader, variant)
}

// Build renders the spec into the provider-agnostic Configuration and
// validates the hierarchy. It is the single entry point used by all three
// render targets, so every artifact this package emits has passed the
// multi-layer routing rules.
func Build(spec *RouteSpec) (*Configuration, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	cfg := &Configuration{}

	// Parent (root) router: owns the entryPoint, runs strip → eval, no service.
	parent := &Router{
		Name:        spec.ParentRouter(),
		Rule:        spec.Rule,
		EntryPoints: []string{spec.EntryPoint},
		Middlewares: []string{spec.StripMiddleware(), spec.EvalMiddleware()},
		Priority:    spec.Priority,
	}
	cfg.Routers = append(cfg.Routers, parent)

	// Fallback child: same rule as the parent, lowest priority — catches
	// the default variant AND any request whose variant header is absent
	// (fail-open without header still routes to the default backend).
	cfg.Routers = append(cfg.Routers, &Router{
		Name:       spec.DefaultRouter(),
		Rule:       spec.Rule,
		Service:    spec.ServiceName(spec.DefaultVariant),
		ParentRefs: []string{spec.ParentRouter()},
		Priority:   1,
	})

	// One child router + one service per non-default variant, then the
	// default variant's service (shared with the fallback child).
	prio := 100
	for _, b := range spec.Backends {
		if b.Variant == spec.DefaultVariant {
			continue
		}
		cfg.Routers = append(cfg.Routers, &Router{
			Name:       spec.VariantRouter(b.Variant),
			Rule:       HeaderRule(b.Variant),
			Service:    spec.ServiceName(b.Variant),
			ParentRefs: []string{spec.ParentRouter()},
			Priority:   prio,
		})
		prio++
	}
	for _, b := range spec.Backends {
		cfg.Services = append(cfg.Services, &Service{
			Name:    spec.ServiceName(b.Variant),
			Servers: []string{b.URL},
		})
	}

	// ① strip externally supplied GoLab headers (anti-forgery).
	cfg.Middlewares = append(cfg.Middlewares, &Middleware{
		Name:         spec.StripMiddleware(),
		StripHeaders: append([]string(nil), managedHeaders...),
	})
	// ② forwardAuth → GoLab eval service; the regex strips every incoming
	// X-Golab-* header (anti-forgery) before Traefik copies back only the
	// headers the eval service returned, for child-router matching.
	cfg.Middlewares = append(cfg.Middlewares, &Middleware{
		Name: spec.EvalMiddleware(),
		ForwardAuth: &ForwardAuth{
			Address:                  spec.EvalURL(),
			AuthResponseHeaders:      append([]string(nil), AuthResponseHeaders...),
			AuthResponseHeadersRegex: AuthResponseHeadersRegex,
		},
	})

	if err := cfg.ValidateHierarchy(); err != nil {
		return nil, fmt.Errorf("gateway: generated configuration is invalid: %w", err)
	}
	return cfg, nil
}
