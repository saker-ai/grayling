package gateway

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func validSpec() *RouteSpec {
	return &RouteSpec{
		Name:           "checkout-api",
		EntryPoint:     "web",
		Rule:           "PathPrefix(`/api`)",
		FlagKey:        "checkout-router",
		DefaultVariant: "stable",
		UnitField:      "user_id",
		EvalAddress:    "http://127.0.0.1:8689",
		Backends: []VariantBackend{
			{Variant: "stable", URL: "http://127.0.0.1:9001", K8sService: "checkout-stable", K8sPort: 8080},
			{Variant: "canary", URL: "http://127.0.0.1:9002", K8sService: "checkout-canary", K8sPort: 8080},
		},
		Namespace: "golab",
	}
}

func TestBuildValidatesAndNamesObjects(t *testing.T) {
	spec := validSpec()
	cfg, err := Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// 3 routers (parent + canary child + default child), 2 middlewares, 2 services.
	if len(cfg.Routers) != 3 || len(cfg.Middlewares) != 2 || len(cfg.Services) != 2 {
		t.Fatalf("shape: %d routers, %d middlewares, %d services",
			len(cfg.Routers), len(cfg.Middlewares), len(cfg.Services))
	}
	parent := cfg.Routers[0]
	if parent.Name != "checkout-api-parent" || parent.Service != "" {
		t.Fatalf("parent router: %+v", parent)
	}
	if len(parent.EntryPoints) != 1 || parent.EntryPoints[0] != "web" {
		t.Fatalf("parent must exclusively own entryPoints: %+v", parent)
	}
	if strings.Join(parent.Middlewares, ",") != "checkout-api-strip,checkout-api-eval" {
		t.Fatalf("middleware order (strip BEFORE eval): %+v", parent.Middlewares)
	}
	def := cfg.Routers[1]
	if def.Name != "checkout-api-default" || def.Service != "checkout-api-svc-stable" ||
		def.Priority != 1 || def.Rule != spec.Rule {
		t.Fatalf("default fallback child: %+v", def)
	}
	canary := cfg.Routers[2]
	wantRule := "Header(`X-Golab-Variant`, `canary`)"
	if canary.Name != "checkout-api-v-canary" || canary.Rule != wantRule ||
		canary.Service != "checkout-api-svc-canary" || canary.Priority <= def.Priority {
		t.Fatalf("canary child: %+v (want rule %q, higher priority than default)", canary, wantRule)
	}
	if len(canary.ParentRefs) != 1 || canary.ParentRefs[0] != parent.Name {
		t.Fatalf("canary parentRefs: %+v", canary.ParentRefs)
	}
}

func TestFileYAMLStructure(t *testing.T) {
	cfg, err := Build(validSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := cfg.FileYAML()
	if err != nil {
		t.Fatalf("FileYAML: %v", err)
	}
	var parsed struct {
		HTTP struct {
			Routers     map[string]map[string]any `yaml:"routers"`
			Middlewares map[string]map[string]any `yaml:"middlewares"`
			Services    map[string]map[string]any `yaml:"services"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("generated YAML does not re-parse: %v\n%s", err, out)
	}
	parent := parsed.HTTP.Routers["checkout-api-parent"]
	if _, has := parent["service"]; has {
		t.Fatalf("parent router must not define a service: %v", parent)
	}
	eps, _ := parent["entryPoints"].([]any)
	if len(eps) != 1 || eps[0] != "web" {
		t.Fatalf("parent entryPoints: %v", parent["entryPoints"])
	}
	canary := parsed.HTTP.Routers["checkout-api-v-canary"]
	refs, _ := canary["parentRefs"].([]any)
	if len(refs) != 1 || refs[0] != "checkout-api-parent" {
		t.Fatalf("canary parentRefs: %v", canary["parentRefs"])
	}
	if canary["rule"] != "Header(`X-Golab-Variant`, `canary`)" {
		t.Fatalf("canary rule: %v", canary["rule"])
	}
	strip := parsed.HTTP.Middlewares["checkout-api-strip"]
	headers, _ := strip["headers"].(map[string]any)
	crh, _ := headers["customRequestHeaders"].(map[string]any)
	for _, h := range []string{"X-Golab-Variant", "X-Golab-Flag-Key", "X-Golab-Fail-Open", "X-Golab-Reason"} {
		v, ok := crh[h]
		if !ok || v != "" {
			t.Fatalf("strip middleware must blank %s (empty value = remove): %v", h, crh)
		}
	}
	eval := parsed.HTTP.Middlewares["checkout-api-eval"]
	fa, _ := eval["forwardAuth"].(map[string]any)
	addr, _ := fa["address"].(string)
	if !strings.HasPrefix(addr, "http://127.0.0.1:8689/api/v1/gateway/forwardauth?") {
		t.Fatalf("forwardAuth address: %q", addr)
	}
	for _, want := range []string{"flagKey=checkout-router", "defaultVariant=stable", "unitField=user_id", "unitHeader=X-Bucketing-Key"} {
		if !strings.Contains(addr, want) {
			t.Fatalf("forwardAuth address missing %q: %s", want, addr)
		}
	}
	arh, _ := fa["authResponseHeaders"].([]any)
	if len(arh) != 4 || arh[0] != "X-Golab-Variant" {
		t.Fatalf("authResponseHeaders: %v", arh)
	}
	if fa["authResponseHeadersRegex"] != "^X-Golab-" {
		t.Fatalf("authResponseHeadersRegex = %v, want ^X-Golab-", fa["authResponseHeadersRegex"])
	}
	svc := parsed.HTTP.Services["checkout-api-svc-canary"]
	lb, _ := svc["loadBalancer"].(map[string]any)
	servers, _ := lb["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["url"] != "http://127.0.0.1:9002" {
		t.Fatalf("canary service servers: %v", servers)
	}
}

func TestFileJSONParses(t *testing.T) {
	cfg, err := Build(validSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := cfg.FileJSON()
	if err != nil {
		t.Fatalf("FileJSON: %v", err)
	}
	if !strings.Contains(string(out), `"parentRefs"`) || !strings.Contains(string(out), `"X-Golab-Variant": ""`) {
		t.Fatalf("unexpected JSON:\n%s", out)
	}
}

func TestKVLayout(t *testing.T) {
	cfg, err := Build(validSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	kv, err := cfg.KV("")
	if err != nil {
		t.Fatalf("KV: %v", err)
	}
	want := map[string]string{
		"traefik/http/routers/checkout-api-parent/rule":                                   "PathPrefix(`/api`)",
		"traefik/http/routers/checkout-api-parent/entrypoints/0":                          "web",
		"traefik/http/routers/checkout-api-parent/middlewares/0":                          "checkout-api-eval",
		"traefik/http/routers/checkout-api-default/parentrefs/0":                          "checkout-api-parent",
		"traefik/http/routers/checkout-api-default/service":                               "checkout-api-svc-stable",
		"traefik/http/routers/checkout-api-default/priority":                              "1",
		"traefik/http/routers/checkout-api-v-canary/rule":                                 "Header(`X-Golab-Variant`, `canary`)",
		"traefik/http/middlewares/checkout-api-eval/forwardauth/address":                  cfg.Middlewares[1].ForwardAuth.Address,
		"traefik/http/middlewares/checkout-api-eval/forwardauth/authresponseheaders/0":    "X-Golab-Variant",
		"traefik/http/middlewares/checkout-api-eval/forwardauth/authresponseheadersregex": "^X-Golab-",
		"traefik/http/services/checkout-api-svc-canary/loadbalancer/servers/0/url":        "http://127.0.0.1:9002",
	}
	for k, v := range want {
		if got := kv[k]; got != v {
			t.Errorf("KV[%q] = %q, want %q", k, got, v)
		}
	}
	if _, ok := kv["traefik/http/routers/checkout-api-parent/service"]; ok {
		t.Error("parent router must not have a service key")
	}
	// KV cannot represent empty header values: the blank-strip middleware is
	// omitted and the parent references only the forwardAuth middleware
	// (whose authResponseHeadersRegex does the stripping).
	if _, ok := kv["traefik/http/middlewares/checkout-api-strip/headers/customrequestheaders/X-Golab-Variant"]; ok {
		t.Error("KV must omit the empty-valued strip middleware (Traefik KV drops empty values)")
	}
	if _, ok := kv["traefik/http/routers/checkout-api-parent/middlewares/1"]; ok {
		t.Error("parent must reference exactly one middleware in KV (strip omitted)")
	}
	// Deterministic sorted rendering.
	sorted, err := cfg.KVSorted("golab")
	if err != nil {
		t.Fatalf("KVSorted: %v", err)
	}
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].Key >= sorted[i].Key {
			t.Fatalf("KVSorted not sorted at %d: %s >= %s", i, sorted[i-1].Key, sorted[i].Key)
		}
	}
	if !strings.HasPrefix(sorted[0].Key, "golab/") {
		t.Fatalf("custom root not applied: %s", sorted[0].Key)
	}
}

func TestIngressRoutes(t *testing.T) {
	out, err := IngressRoutes(validSpec())
	if err != nil {
		t.Fatalf("IngressRoutes: %v", err)
	}
	docs := strings.Split(string(out), "---\n")
	kinds := map[string]int{}
	var parentRoute, childRoute map[string]any
	for _, d := range docs {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(d), &obj); err != nil {
			t.Fatalf("CRD doc does not parse: %v\n%s", err, d)
		}
		kind, _ := obj["kind"].(string)
		kinds[kind]++
		meta, _ := obj["metadata"].(map[string]any)
		spec, _ := obj["spec"].(map[string]any)
		switch {
		case kind == "IngressRoute" && meta["name"] == "checkout-api-parent":
			parentRoute = spec
		case kind == "IngressRoute" && meta["name"] == "checkout-api-v-canary":
			childRoute = spec
		}
		if meta["namespace"] != "golab" {
			t.Fatalf("object %v missing namespace: %v", meta["name"], meta)
		}
	}
	if kinds["Middleware"] != 2 || kinds["IngressRoute"] != 3 {
		t.Fatalf("doc kinds: %v", kinds)
	}
	// Parent: entryPoints + middlewares, route WITHOUT services.
	if eps, _ := parentRoute["entryPoints"].([]any); len(eps) != 1 || eps[0] != "web" {
		t.Fatalf("parent entryPoints: %v", parentRoute["entryPoints"])
	}
	routes, _ := parentRoute["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("parent routes: %v", routes)
	}
	pr := routes[0].(map[string]any)
	if _, has := pr["services"]; has {
		t.Fatalf("parent route must not define services: %v", pr)
	}
	if mws, _ := pr["middlewares"].([]any); len(mws) != 2 {
		t.Fatalf("parent middlewares: %v", mws)
	}
	if _, has := parentRoute["parentRefs"]; has {
		t.Fatalf("parent must not have parentRefs: %v", parentRoute)
	}
	// Child: parentRefs + services, no entryPoints.
	refs, _ := childRoute["parentRefs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "checkout-api-parent" {
		t.Fatalf("child parentRefs: %v", refs)
	}
	if _, has := childRoute["entryPoints"]; has {
		t.Fatalf("child must not define entryPoints: %v", childRoute)
	}
	crs, _ := childRoute["routes"].([]any)
	cr := crs[0].(map[string]any)
	svcs, _ := cr["services"].([]any)
	if len(svcs) != 1 || svcs[0].(map[string]any)["name"] != "checkout-canary" || svcs[0].(map[string]any)["port"] != 8080 {
		t.Fatalf("child services: %v", svcs)
	}
	if cr["match"] != "Header(`X-Golab-Variant`, `canary`)" {
		t.Fatalf("child match: %v", cr["match"])
	}

	// Missing Kubernetes backend info must fail loudly, not silently render.
	bad := validSpec()
	bad.Backends[1].K8sService = ""
	if _, err := IngressRoutes(bad); err == nil {
		t.Fatal("IngressRoutes must fail when K8sService is missing")
	}
}

func TestHierarchyValidationRejects(t *testing.T) {
	base := func() *Configuration {
		return &Configuration{
			Routers: []*Router{
				{Name: "root", Rule: "PathPrefix(`/`)", EntryPoints: []string{"web"}},
				{Name: "leaf", Rule: "PathPrefix(`/`)", Service: "svc", ParentRefs: []string{"root"}},
			},
			Services: []*Service{{Name: "svc", Servers: []string{"http://127.0.0.1:1"}}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*Configuration)
		want   string
	}{
		{"intermediate with service", func(c *Configuration) {
			c.Routers = append(c.Routers,
				&Router{Name: "mid", Rule: "PathPrefix(`/`)", ParentRefs: []string{"root"}},
				&Router{Name: "leaf2", Rule: "PathPrefix(`/x`)", Service: "svc", ParentRefs: []string{"mid"}})
			c.Routers[1].ParentRefs = []string{"mid"}
			c.Routers[1].Name = "leaf"
			// make "mid" an intermediate WITH a service — illegal.
			c.Routers[2].Service = "svc"
		}, "intermediate routers must not define a service"},
		{"leaf without service", func(c *Configuration) {
			c.Routers[1].Service = ""
		}, "leaf routers must define a service"},
		{"non-root with entryPoints", func(c *Configuration) {
			c.Routers[1].EntryPoints = []string{"web"}
		}, "non-root routers must not define entryPoints"},
		{"root parent with service", func(c *Configuration) {
			c.Routers[0].Service = "svc"
		}, "parent routers must not define a service"},
		{"standalone without service", func(c *Configuration) {
			c.Routers[0].Service = ""
			c.Routers[1].ParentRefs = nil
			c.Routers[1].Service = ""
		}, "standalone routers must define a service"},
		{"dangling parentRef", func(c *Configuration) {
			c.Routers[1].ParentRefs = []string{"nope"}
		}, `parentRef "nope" does not exist`},
		{"self reference", func(c *Configuration) {
			c.Routers[1].ParentRefs = []string{"leaf"}
		}, "self-reference"},
		{"cycle", func(c *Configuration) {
			c.Routers[0].ParentRefs = []string{"leaf"}
			c.Routers[0].Service = "svc"
			c.Routers[1].Service = ""
		}, "cycle"},
		{"cycle via second parent", func(c *Configuration) {
			// mid references BOTH root and loop; loop references mid — the
			// cycle runs through mid's second parentRef, which a first-parent
			// chain walk would miss.
			c.Routers = append(c.Routers,
				&Router{Name: "mid", Rule: "PathPrefix(`/m`)", ParentRefs: []string{"root", "loop"}},
				&Router{Name: "loop", Rule: "PathPrefix(`/l`)", Service: "svc", ParentRefs: []string{"mid"}})
			c.Routers[1].ParentRefs = []string{"mid"}
			c.Routers[1].Service = ""
		}, "cycle"},
		{"missing service", func(c *Configuration) {
			c.Routers[1].Service = "ghost"
		}, `service "ghost" does not exist`},
		{"missing middleware", func(c *Configuration) {
			c.Routers[0].Middlewares = []string{"ghost"}
		}, `middleware "ghost" does not exist`},
		{"empty rule", func(c *Configuration) {
			c.Routers[1].Rule = " "
		}, "rule is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.ValidateHierarchy()
			if err == nil {
				t.Fatalf("expected rejection (%s), got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
	if err := base().ValidateHierarchy(); err != nil {
		t.Fatalf("valid hierarchy rejected: %v", err)
	}
}

func TestMultiParentAcyclicAllowed(t *testing.T) {
	// Traefik parentRefs is a list: an acyclic router with several parents
	// is legal and must pass validation (the generator itself only emits
	// single-parent chains).
	c := &Configuration{
		Routers: []*Router{
			{Name: "root-a", Rule: "PathPrefix(`/a`)", EntryPoints: []string{"web"}},
			{Name: "root-b", Rule: "PathPrefix(`/b`)", EntryPoints: []string{"websecure"}},
			{Name: "leaf", Rule: "PathPrefix(`/`)", Service: "svc", ParentRefs: []string{"root-a", "root-b"}},
		},
		Services: []*Service{{Name: "svc", Servers: []string{"http://127.0.0.1:1"}}},
	}
	if err := c.ValidateHierarchy(); err != nil {
		t.Fatalf("acyclic multi-parent hierarchy rejected: %v", err)
	}
}

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RouteSpec)
	}{
		{"bad name charset", func(s *RouteSpec) { s.Name = "Bad_Name" }},
		{"missing entrypoint", func(s *RouteSpec) { s.EntryPoint = "" }},
		{"missing flag key", func(s *RouteSpec) { s.FlagKey = "" }},
		{"default variant without backend", func(s *RouteSpec) { s.DefaultVariant = "ghost" }},
		{"no non-default variant", func(s *RouteSpec) {
			s.Backends = s.Backends[:1]
		}},
		{"duplicate variant", func(s *RouteSpec) {
			s.Backends = append(s.Backends, s.Backends[1])
		}},
		{"relative backend url", func(s *RouteSpec) { s.Backends[0].URL = "backend:9001" }},
		{"relative eval address", func(s *RouteSpec) { s.EvalAddress = "127.0.0.1:8689" }},
		{"bad variant charset", func(s *RouteSpec) { s.Backends[1].Variant = "ca nary" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(s)
			if err := s.Validate(); err == nil {
				t.Fatal("expected validation error")
			} else if _, err := Build(s); err == nil {
				t.Fatal("Build must reject invalid spec")
			}
		})
	}
}

func TestEvalURLEscapesParameters(t *testing.T) {
	s := validSpec()
	s.FlagKey = "flag with&special=chars"
	u := s.EvalURL()
	if strings.Contains(u, "flag with&special=chars") {
		t.Fatalf("flagKey not escaped: %s", u)
	}
	if !strings.Contains(u, "flagKey=flag+with%26special%3Dchars") {
		t.Fatalf("unexpected escaping: %s", u)
	}
	// Without a secret the parameter must be absent entirely.
	if strings.Contains(u, "secret=") {
		t.Fatalf("secret param leaked without RouteSpec.Secret: %s", u)
	}
	// With a secret it is embedded (and escaped) for the proxy's
	// -gateway-secret check.
	s.Secret = "k9+v/sec=ret"
	u2 := s.EvalURL()
	if !strings.Contains(u2, "secret=k9%2Bv%2Fsec%3Dret") {
		t.Fatalf("secret not embedded/escaped: %s", u2)
	}
}
