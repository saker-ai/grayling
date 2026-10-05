module github.com/godeps/golab

go 1.27.0

require (
	github.com/blang/semver/v4 v4.0.0
	github.com/godeps/gonacos v0.0.0-20260706000526-514208bd40a1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/open-feature/go-sdk v1.19.0
	github.com/redis/go-redis/v9 v9.21.0
	github.com/thomaspoignant/go-feature-flag/modules/core v0.7.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/alicebob/miniredis/v2 v2.38.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/diegoholiveira/jsonlogic/v3 v3.10.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/higress-group/openapi-to-mcpserver v1.0.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/modelcontextprotocol/go-sdk v1.6.1 // indirect
	github.com/nikunjy/rules v1.5.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/exp v0.0.0-20250717185816-542afb5b7346 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.12.0 // indirect
)

// gonacos is pinned to a specific upstream commit (pseudo-version) instead of
// the former sibling-directory replace: builds are now reproducible from the
// module cache alone (docs/技术方案.md §12.1). The commit corresponds to
// github.com/godeps/gonacos main @ 514208bd40a131b5badb830bff2daf704ab97ee9.

// gff core v0.7.2 requires the forked rules package; a dependency module's
// own replace directives do not apply to the main module, so it is restated
// here (docs/技术方案.md §2).
replace github.com/nikunjy/rules => github.com/hairyhenderson/rules v0.0.0-20250704181428-58ee76134adc
