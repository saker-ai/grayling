// Mechanical consistency test between the repository-root openapi.yaml and
// the authoritative HTTP route table in server.go's Handler().
//
// The route table is extracted from SOURCE with go/parser (not from a
// hand-maintained list), so adding/removing a mux.HandleFunc registration
// without updating the spec — or vice versa — fails this test. Path
// templates must match character-for-character, which includes the path
// parameter names ({ws}, {env}, {key}, {id}, {type}, {logical}).
package api_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// serverSource is the file holding the authoritative route table; it
	// lives in the same directory as this test (go test runs there).
	serverSource = "server.go"
	// specFile is the OpenAPI document at the repository root.
	specFile = "../../openapi.yaml"
)

// op identifies one HTTP operation by method and path template.
type op struct {
	method string
	path   string
}

func (o op) String() string { return o.method + " " + o.path }

// httpMethods are the operation keys of an OpenAPI Path Item Object. Any
// other key (parameters, summary, description, servers) is not an operation.
var httpMethods = map[string]bool{
	"get": true, "head": true, "post": true, "put": true,
	"patch": true, "delete": true, "options": true, "trace": true,
}

var pathParamRe = regexp.MustCompile(`\{([^{}]+)\}`)

func TestOpenAPISpecMatchesRouteTable(t *testing.T) {
	routes := parseRouteTable(t)
	doc := loadSpec(t)

	t.Run("SpecMetadata", func(t *testing.T) {
		version, ok := doc["openapi"].(string)
		if !ok || version == "" {
			t.Fatalf("spec: openapi version field missing or not a string: %#v", doc["openapi"])
		}
		if !strings.HasPrefix(version, "3.") {
			t.Errorf("spec: openapi version %q is not 3.x", version)
		}
		info := asMap(t, doc["info"], "info")
		title, ok := info["title"].(string)
		if !ok || strings.TrimSpace(title) == "" {
			t.Errorf("spec: info.title missing or empty: %#v", info["title"])
		}
	})

	specOps, pathCount := specOperations(t, doc)

	t.Run("EveryRouteInSpec", func(t *testing.T) {
		missing := 0
		for _, r := range sortedKeys(routes) {
			if _, ok := specOps[r]; !ok {
				t.Errorf("route %s is registered in server.go but missing from %s", r, specFile)
				missing++
			}
		}
		if missing > 0 {
			t.Fatalf("%d route(s) missing from the spec", missing)
		}
	})

	t.Run("EverySpecOpInRouteTable", func(t *testing.T) {
		extra := 0
		for _, s := range sortedKeys(specOps) {
			if !routes[s] {
				t.Errorf("spec declares %s but server.go has no such route", s)
				extra++
			}
		}
		if extra > 0 {
			t.Fatalf("%d spec operation(s) not backed by the route table", extra)
		}
	})

	t.Run("PathParameters", func(t *testing.T) {
		checkPathParameters(t, doc)
	})

	t.Run("OperationIDs", func(t *testing.T) {
		checkOperationIDs(t, specOps)
	})

	t.Logf("route table: %d operations; spec: %d paths / %d operations — bidirectionally consistent",
		len(routes), pathCount, len(specOps))
}

// ---------------------------------------------------------------------------
// Route-table extraction (go/parser + go/ast)
// ---------------------------------------------------------------------------

// parseRouteTable extracts every mux.HandleFunc("<METHOD> <pattern>")
// string literal from server.go.
func parseRouteTable(t *testing.T) map[op]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, serverSource, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", serverSource, err)
	}

	routes := map[op]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" || len(call.Args) < 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// Non-literal pattern: would defeat mechanical checking.
			t.Errorf("%s: HandleFunc pattern is not a string literal", fset.Position(call.Pos()))
			return true
		}
		raw, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("%s: unquote %s: %v", fset.Position(lit.Pos()), lit.Value, err)
			return true
		}
		method, pattern, found := strings.Cut(raw, " ")
		if !found || !isUpperAlpha(method) || !strings.HasPrefix(pattern, "/") {
			t.Errorf("%s: route literal %q is not in \"METHOD /path\" form", fset.Position(lit.Pos()), raw)
			return true
		}
		r := op{method: method, path: pattern}
		if routes[r] {
			t.Errorf("%s: duplicate route registration %s", fset.Position(lit.Pos()), r)
		}
		routes[r] = true
		return true
	})

	if len(routes) == 0 {
		t.Fatalf("extracted 0 routes from %s — route-table extraction is broken "+
			"(expected the mux.HandleFunc registrations of Handler())", serverSource)
	}
	return routes
}

func isUpperAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Spec loading and traversal
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(specFile)
	if err != nil {
		t.Fatalf("read %s: %v (the OpenAPI spec must live at the repository root)", specFile, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s as YAML: %v", specFile, err)
	}
	if doc == nil {
		t.Fatalf("%s is empty", specFile)
	}
	return doc
}

// specOperations collects every (method, path) operation declared under the
// spec's paths object, plus the number of distinct path templates.
func specOperations(t *testing.T, doc map[string]any) (map[op]map[string]any, int) {
	t.Helper()
	paths := asMap(t, doc["paths"], "paths")
	ops := map[op]map[string]any{}
	for path, rawItem := range paths {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("spec path %q does not start with '/'", path)
			continue
		}
		item := asMap(t, rawItem, fmt.Sprintf("paths.%s", path))
		for key, rawOp := range item {
			if !httpMethods[strings.ToLower(key)] {
				continue // parameters/summary/description/servers on the path item
			}
			o := op{method: strings.ToUpper(key), path: path}
			if _, dup := ops[o]; dup {
				t.Errorf("spec declares %s twice", o)
			}
			ops[o] = asMap(t, rawOp, o.String())
		}
	}
	if len(ops) == 0 {
		t.Fatalf("spec declares no operations — wrong document?")
	}
	return ops, len(paths)
}

// checkPathParameters verifies, for every operation, that the declared
// in:path parameters are exactly the {names} of the path template and that
// each is required.
func checkPathParameters(t *testing.T, doc map[string]any) {
	t.Helper()
	paths := asMap(t, doc["paths"], "paths")
	for path, rawItem := range paths {
		item := asMap(t, rawItem, fmt.Sprintf("paths.%s", path))

		template := map[string]bool{}
		for _, m := range pathParamRe.FindAllStringSubmatch(path, -1) {
			template[m[1]] = true
		}

		for key, rawOp := range item {
			if !httpMethods[strings.ToLower(key)] {
				continue
			}
			operation := asMap(t, rawOp, key+" "+path)

			declared := map[string]bool{}
			// Parameters may be declared on the path item (shared) and/or
			// the operation; both apply.
			for _, list := range []any{item["parameters"], operation["parameters"]} {
				for _, p := range parameterList(t, list, key+" "+path) {
					in, _ := p["in"].(string)
					if in != "path" {
						continue
					}
					name, _ := p["name"].(string)
					if name == "" {
						t.Errorf("%s %s: path parameter without a name", key, path)
						continue
					}
					if req, ok := p["required"].(bool); !ok || !req {
						t.Errorf("%s %s: path parameter %q must have required: true", key, path, name)
					}
					declared[name] = true
				}
			}

			for name := range template {
				if !declared[name] {
					t.Errorf("%s %s: template parameter {%s} is not declared as an in:path parameter", key, path, name)
				}
			}
			for name := range declared {
				if !template[name] {
					t.Errorf("%s %s: declares path parameter %q that does not appear in the template", key, path, name)
				}
			}
		}
	}
}

// parameterList normalizes a parameters array, resolving local $refs.
func parameterList(t *testing.T, list any, ctx string) []map[string]any {
	t.Helper()
	if list == nil {
		return nil
	}
	arr, ok := list.([]any)
	if !ok {
		t.Errorf("%s: parameters is not a list", ctx)
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("%s: parameter entry is not a map: %#v", ctx, entry)
			continue
		}
		if ref, ok := m["$ref"].(string); ok {
			m = resolveRef(t, doc(t), ref, ctx)
		}
		out = append(out, m)
	}
	return out
}

// checkOperationIDs verifies every operation has a non-empty, unique
// operationId.
func checkOperationIDs(t *testing.T, specOps map[op]map[string]any) {
	t.Helper()
	seen := map[string]op{}
	for _, o := range sortedKeys(specOps) {
		operation := specOps[o]
		id, ok := operation["operationId"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			t.Errorf("%s: missing operationId", o)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("operationId %q used by both %s and %s", id, prev, o)
		}
		seen[id] = o
		if _, has := operation["responses"].(map[string]any); !has {
			t.Errorf("%s: missing responses object", o)
		}
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// docRoot caches the parsed spec for $ref resolution inside a single test run.
var specRoot map[string]any

func doc(t *testing.T) map[string]any {
	t.Helper()
	if specRoot == nil {
		specRoot = loadSpec(t)
	}
	return specRoot
}

// resolveRef follows a local JSON pointer like "#/components/parameters/WsPath".
func resolveRef(t *testing.T, root map[string]any, ref, ctx string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(ref, "#/") {
		t.Fatalf("%s: only local $ref pointers are supported, got %q", ctx, ref)
	}
	var cur any = root
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: $ref %q walks into a non-map", ctx, ref)
		}
		cur, ok = m[part]
		if !ok {
			t.Fatalf("%s: $ref %q does not resolve (missing %q)", ctx, ref, part)
		}
	}
	return asMap(t, cur, ctx+" $ref "+ref)
}

func asMap(t *testing.T, v any, ctx string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected a YAML mapping, got %T", ctx, v)
	}
	return m
}

// sortedKeys deterministically orders operations for stable diagnostics.
func sortedKeys[V any](m map[op]V) []op {
	out := make([]op, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}
