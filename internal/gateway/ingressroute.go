package gateway

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kubernetes CRD rendering (traefik.io/v1alpha1). Multi-layer routing maps
// onto separate IngressRoute resources: the parent IngressRoute carries the
// entryPoints and the middleware chain (its route has NO services); each
// child IngressRoute references the parent via spec.parentRefs and binds one
// match rule to a Kubernetes Service.
//
// The live-cluster acceptance test is test/gateway/k8s_e2e_test.go; the file
// and KV targets are exercised against a real Traefik binary in test/gateway.
// Backends must carry K8sService/K8sPort for this target.

const crdAPIVersion = "traefik.io/v1alpha1"

type objectMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

type crdObject struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   objectMeta  `yaml:"metadata"`
	Spec       interface{} `yaml:"spec"`
}

type middlewareSpec struct {
	Headers     *crdHeaders     `yaml:"headers,omitempty"`
	ForwardAuth *crdForwardAuth `yaml:"forwardAuth,omitempty"`
}

type crdHeaders struct {
	CustomRequestHeaders map[string]string `yaml:"customRequestHeaders,omitempty"`
}

type crdForwardAuth struct {
	Address                  string   `yaml:"address"`
	AuthResponseHeaders      []string `yaml:"authResponseHeaders,omitempty"`
	AuthResponseHeadersRegex string   `yaml:"authResponseHeadersRegex,omitempty"`
}

type ingressRouteSpec struct {
	EntryPoints []string         `yaml:"entryPoints,omitempty"`
	Routes      []crdRoute       `yaml:"routes"`
	ParentRefs  []ingressRoutRef `yaml:"parentRefs,omitempty"`
}

type ingressRoutRef struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

type crdRoute struct {
	Match       string         `yaml:"match"`
	Kind        string         `yaml:"kind,omitempty"`
	Priority    int            `yaml:"priority,omitempty"`
	Services    []crdService   `yaml:"services,omitempty"`
	Middlewares []middlewareRf `yaml:"middlewares,omitempty"`
}

type middlewareRf struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

type crdService struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

// IngressRoutes renders the spec as a multi-document YAML stream: the strip
// and eval Middleware objects, the parent IngressRoute, then one child
// IngressRoute per router (variants + default fallback).
func IngressRoutes(spec *RouteSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// Reuse the validated generic configuration for the router hierarchy so
	// the CRD output passes exactly the same layering rules.
	cfg, err := Build(spec)
	if err != nil {
		return nil, err
	}
	k8sPort := map[string]VariantBackend{}
	for _, b := range spec.Backends {
		if b.K8sService == "" || b.K8sPort <= 0 {
			return nil, fmt.Errorf("gateway: variant %q: K8sService/K8sPort are required for the IngressRoute target", b.Variant)
		}
		k8sPort[spec.ServiceName(b.Variant)] = b
	}

	var docs []string

	// Middleware CRDs.
	for _, m := range cfg.Middlewares {
		ms := middlewareSpec{}
		if m.ForwardAuth != nil {
			ms.ForwardAuth = &crdForwardAuth{
				Address:                  m.ForwardAuth.Address,
				AuthResponseHeaders:      m.ForwardAuth.AuthResponseHeaders,
				AuthResponseHeadersRegex: m.ForwardAuth.AuthResponseHeadersRegex,
			}
		} else {
			headers := map[string]string{}
			for _, h := range m.StripHeaders {
				headers[h] = ""
			}
			ms.Headers = &crdHeaders{CustomRequestHeaders: headers}
		}
		doc, err := yaml.Marshal(crdObject{
			APIVersion: crdAPIVersion, Kind: "Middleware",
			Metadata: objectMeta{Name: m.Name, Namespace: spec.Namespace},
			Spec:     ms,
		})
		if err != nil {
			return nil, err
		}
		docs = append(docs, string(doc))
	}

	// IngressRoutes: the parent first, then children in configuration order.
	svcOf := func(routerService string) ([]crdService, error) {
		b, ok := k8sPort[routerService]
		if !ok {
			return nil, fmt.Errorf("gateway: service %q has no Kubernetes backend", routerService)
		}
		return []crdService{{Name: b.K8sService, Port: b.K8sPort}}, nil
	}
	for _, r := range cfg.Routers {
		route := crdRoute{Match: r.Rule, Kind: "Rule", Priority: r.Priority}
		for _, mw := range r.Middlewares {
			route.Middlewares = append(route.Middlewares, middlewareRf{Name: mw, Namespace: spec.Namespace})
		}
		if r.Service != "" {
			svcs, err := svcOf(r.Service)
			if err != nil {
				return nil, err
			}
			route.Services = svcs
		}
		irs := ingressRouteSpec{Routes: []crdRoute{route}}
		if len(r.EntryPoints) > 0 {
			irs.EntryPoints = r.EntryPoints
		}
		for _, p := range r.ParentRefs {
			irs.ParentRefs = append(irs.ParentRefs, ingressRoutRef{Name: p, Namespace: spec.Namespace})
		}
		doc, err := yaml.Marshal(crdObject{
			APIVersion: crdAPIVersion, Kind: "IngressRoute",
			Metadata: objectMeta{Name: r.Name, Namespace: spec.Namespace},
			Spec:     irs,
		})
		if err != nil {
			return nil, err
		}
		docs = append(docs, string(doc))
	}

	return []byte(strings.Join(docs, "---\n")), nil
}
