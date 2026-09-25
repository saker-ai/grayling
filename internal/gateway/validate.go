package gateway

import (
	"fmt"
	"strings"
)

// Configuration is the provider-agnostic dynamic configuration model. The
// renderers (file/KV/IngressRoute) translate it 1:1; ValidateHierarchy runs
// on this model so every target is checked against the same rules.
type Configuration struct {
	Routers     []*Router
	Middlewares []*Middleware
	Services    []*Service
}

// Router is one Traefik router. Position in the hierarchy:
//   - root:  no ParentRefs; may own EntryPoints; when it has children it
//     MUST NOT define a Service (the children do the routing);
//   - intermediate: ParentRefs + children; MUST NOT define a Service;
//     MUST NOT define EntryPoints;
//   - leaf:  ParentRefs, no children; MUST define a Service; MUST NOT
//     define EntryPoints.
type Router struct {
	Name        string
	Rule        string
	Service     string
	Middlewares []string
	EntryPoints []string
	ParentRefs  []string
	// Priority is an explicit router priority (0 = omit, Traefik falls
	// back to rule-length priority).
	Priority int
}

// Middleware is exactly one middleware definition: either a header-strip
// (headers.customRequestHeaders with empty values) or a forwardAuth.
type Middleware struct {
	Name         string
	StripHeaders []string
	ForwardAuth  *ForwardAuth
}

// ForwardAuth is the forwardAuth middleware definition.
type ForwardAuth struct {
	Address             string
	AuthResponseHeaders []string
	// AuthResponseHeadersRegex, when set, makes Traefik strip every matching
	// incoming request header before copying back the eval-service response
	// headers — the provider-portable anti-forgery strip (see the package
	// doc and AuthResponseHeadersRegex constant).
	AuthResponseHeadersRegex string
}

// Service is a loadBalancer service with plain server URLs.
type Service struct {
	Name    string
	Servers []string
}

// ValidateHierarchy enforces the multi-layer routing rules (Traefik docs:
// root/intermediate/leaf constraints) plus referential integrity:
//
//   - parentRefs must reference existing routers; no self-reference, no
//     duplicate refs, no cycles;
//   - intermediate routers (children AND parents) must not define a service;
//   - leaf routers must define a service;
//   - non-root routers must not define entryPoints (root routers exclusively
//     own entryPoints/tls);
//   - standalone root routers (no children) must define a service;
//   - referenced services and middlewares must exist.
func (c *Configuration) ValidateHierarchy() error {
	if c == nil {
		return fmt.Errorf("nil configuration")
	}
	var errs []string

	routerByName := map[string]*Router{}
	for _, r := range c.Routers {
		if r.Name == "" {
			errs = append(errs, "router with empty name")
			continue
		}
		if _, dup := routerByName[r.Name]; dup {
			errs = append(errs, fmt.Sprintf("duplicate router %q", r.Name))
		}
		routerByName[r.Name] = r
		if strings.TrimSpace(r.Rule) == "" {
			errs = append(errs, fmt.Sprintf("router %q: rule is required", r.Name))
		}
	}
	svcByName := map[string]bool{}
	for _, s := range c.Services {
		svcByName[s.Name] = true
		if len(s.Servers) == 0 {
			errs = append(errs, fmt.Sprintf("service %q: at least one server is required", s.Name))
		}
	}
	mwByName := map[string]bool{}
	for _, m := range c.Middlewares {
		mwByName[m.Name] = true
		if (len(m.StripHeaders) == 0) == (m.ForwardAuth == nil) {
			errs = append(errs, fmt.Sprintf("middleware %q: exactly one of StripHeaders/ForwardAuth is required", m.Name))
		}
	}

	childrenOf := map[string][]string{} // parent → children
	for _, r := range c.Routers {
		seen := map[string]bool{}
		for _, p := range r.ParentRefs {
			if p == r.Name {
				errs = append(errs, fmt.Sprintf("router %q: self-reference in parentRefs", r.Name))
				continue
			}
			if seen[p] {
				errs = append(errs, fmt.Sprintf("router %q: duplicate parentRef %q", r.Name, p))
				continue
			}
			seen[p] = true
			if _, ok := routerByName[p]; !ok {
				errs = append(errs, fmt.Sprintf("router %q: parentRef %q does not exist", r.Name, p))
				continue
			}
			childrenOf[p] = append(childrenOf[p], r.Name)
		}
	}

	// Cycle detection across the FULL parent graph: a router may reference
	// several parents, and a cycle through ANY of them is illegal. DFS with
	// a recursion stack; `done` memoizes acyclic subgraphs.
	const (
		stateInStack = 1
		stateDone    = 2
	)
	state := map[string]int{}
	cyclic := ""
	var visit func(name string) bool
	visit = func(name string) bool {
		switch state[name] {
		case stateInStack:
			cyclic = name
			return true
		case stateDone:
			return false
		}
		state[name] = stateInStack
		if r := routerByName[name]; r != nil {
			for _, p := range r.ParentRefs {
				if _, ok := routerByName[p]; ok && visit(p) {
					return true
				}
			}
		}
		state[name] = stateDone
		return false
	}
	for _, r := range c.Routers {
		if visit(r.Name) {
			errs = append(errs, fmt.Sprintf("router %q: parentRefs cycle (through %q)", r.Name, cyclic))
			break
		}
	}

	for _, r := range c.Routers {
		hasParent := len(r.ParentRefs) > 0
		hasChildren := len(childrenOf[r.Name]) > 0
		switch {
		case hasParent && hasChildren:
			// Intermediate router.
			if r.Service != "" {
				errs = append(errs, fmt.Sprintf("router %q: intermediate routers must not define a service", r.Name))
			}
			if len(r.EntryPoints) > 0 {
				errs = append(errs, fmt.Sprintf("router %q: non-root routers must not define entryPoints (root routers exclusively own them)", r.Name))
			}
		case hasParent && !hasChildren:
			// Leaf router.
			if r.Service == "" {
				errs = append(errs, fmt.Sprintf("router %q: leaf routers must define a service", r.Name))
			}
			if len(r.EntryPoints) > 0 {
				errs = append(errs, fmt.Sprintf("router %q: non-root routers must not define entryPoints (root routers exclusively own them)", r.Name))
			}
		case !hasParent && hasChildren:
			// Root parent router.
			if r.Service != "" {
				errs = append(errs, fmt.Sprintf("router %q: parent routers must not define a service", r.Name))
			}
		default:
			// Standalone root router.
			if r.Service == "" {
				errs = append(errs, fmt.Sprintf("router %q: standalone routers must define a service", r.Name))
			}
		}
		if r.Service != "" && !svcByName[r.Service] {
			errs = append(errs, fmt.Sprintf("router %q: service %q does not exist", r.Name, r.Service))
		}
		for _, mw := range r.Middlewares {
			if !mwByName[mw] {
				errs = append(errs, fmt.Sprintf("router %q: middleware %q does not exist", r.Name, mw))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
