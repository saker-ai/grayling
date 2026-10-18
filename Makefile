# Grayling Makefile
#
# Grayling builds as a standalone module. Force GOWORK=off so a parent
# workspace does not override the pinned gonacos module version.
GOWORK := off
GO     := GOWORK=$(GOWORK) go
RUN_ARGS ?=
ADMIN_PASSWORD_FILE ?= data/admin-password
# Developer mode for 'make run': the console signs itself in as the global
# owner, so no password file is generated and nothing has to be typed. It is
# on by default for a local checkout; run 'make run DEV=0' to get the password
# prompt back. NEVER leave it on where the console is reachable by others.
DEV ?= 1
# Port for 'make run'. Bound on all interfaces (:PORT), so the console and API
# are reachable from the network; embedded gonacos stays on ephemeral loopback.
RUN_PORT ?= 27002

.PHONY: all build test vet tidy smoke run check-run-port ensure-admin-password show-admin-password clean sdk-test sdk-python sdk-typescript license-check release-check dist client-compatibility

all: vet test build

build: frontend
	$(GO) build ./...
	$(GO) build -o bin/grayling ./cmd/grayling
	$(GO) build -o bin/grayling-proxy ./cmd/grayling-proxy
	$(GO) build -o bin/grayling-native-proxy ./cmd/grayling-native-proxy
	$(GO) build -o bin/nacos-native-grpc-probe ./cmd/nacos-native-grpc-probe

# web/node_modules must stay out of the package list: pnpm dependencies can
# carry Go files that `go test ./...` would otherwise scan as project packages.
# (The pipeline form `go list | grep | xargs $(GO) test` is unrolled into
# command substitution because $(GO) carries an env prefix xargs cannot exec.)
test: test-static
	$(GO) test -count=1 $$(GOWORK=$(GOWORK) go list ./... | grep -v /web/node_modules/)

client-compatibility:
	python3 scripts/check_client_compatibility.py --tier all

smoke:
	$(GO) test ./test/smoke/ -v -count=1

# Multi-language SDK integration suites — both build and run the REAL
# cmd/grayling binary (test/boot startup contract) and exercise it over HTTP.
sdk-python:
	cd sdk/python && (test -d .venv || python3 -m venv .venv) \
	  && .venv/bin/python -m pip install -q pytest \
	  && .venv/bin/python -m pytest tests/ -q

sdk-typescript:
	cd sdk/typescript && (test -d node_modules || npm install --no-audit --no-fund) \
	  && npm run typecheck --silent && npm test --silent

sdk-test: sdk-python sdk-typescript

vet:
	$(GO) vet ./...

license-check:
	python3 scripts/license_audit.py check

release-check:
	python3 scripts/license_audit.py release-check

dist: release-check build
	python3 scripts/license_audit.py bundle --output dist/grayling

tidy:
	$(GO) mod tidy

ensure-admin-password:
ifeq ($(DEV),0)
	@mkdir -p "$(dir $(ADMIN_PASSWORD_FILE))"
	@if [ -z "$$GRAYLING_ADMIN_PASSWORD" ] && [ ! -e "$(ADMIN_PASSWORD_FILE)" ]; then \
	  umask 077; \
	  password=$$(od -An -N24 -tx1 /dev/urandom | tr -d '[:space:]'); \
	  printf '%s\n' "$$password" > "$(ADMIN_PASSWORD_FILE)"; \
	  echo "Saved to $(ADMIN_PASSWORD_FILE) (use 'make show-admin-password' to view it again)"; \
	fi
	@echo "Admin password configured (use 'make show-admin-password' to display the password file)."
else
	@echo "Dev mode: console sign-in is automatic; no admin password file is used."
endif

show-admin-password:
	@cat "$(ADMIN_PASSWORD_FILE)"

check-run-port:
	@python3 scripts/free_run_port.py "$(RUN_PORT)"

run: build ensure-admin-password check-run-port
	GRAYLING_DEV_AUTOLOGIN=$(DEV) \
	$(if $(filter 0,$(DEV)),GRAYLING_ADMIN_PASSWORD_FILE="$(ADMIN_PASSWORD_FILE)") \
	./bin/grayling -addr :$(RUN_PORT) \
	  -gonacos-addr 127.0.0.1:0 -gonacos-grpc-addr 127.0.0.1:0 \
	  -gonacos-root "$$($(GO) list -m -f '{{.Dir}}' github.com/godeps/gonacos)" $(RUN_ARGS)

clean:
	rm -rf bin/

# Agent SDK contract and real WSS transport checks.
.PHONY: test-agent-sdk generate-agent check-agent-generated
test-agent-sdk:
	$(GO) test -race ./api/agent/v1 ./pkg/agent -count=1

# Agent contract checks only; these are not the full Agent/cluster/gateway
# acceptance suites required for GA in docs/统一Agent协议实施与验收方案.md.
.PHONY: generate-agent check-agent-generated test-agent-contract
generate-agent:
	sh scripts/generate_agent_proto.sh

check-agent-generated:
	sh scripts/check_agent_generated.sh

test-agent-contract:
	$(GO) test -race -count=1 ./api/agent/v1

.PHONY: test-agent-resources test-agent-naming-poc
test-agent-resources:
	$(GO) test -race -count=1 ./pkg/agent ./internal/agent

test-agent-naming-poc:
	$(GO) test -tags=integration -race -count=1 ./test/agent -run Naming

.PHONY: test-agent-channel
test-agent-channel:
	$(GO) test -race -count=1 ./api/agent/v1 ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=5m ./test/agent -run 'TestSession|TestWSS|TestGoAgent|TestAgentApp'

.PHONY: test-agent-policy
test-agent-policy:
	$(GO) test -race -count=1 ./pkg/agent ./internal/agent
	$(GO) test -tags=integration -race -count=1 -timeout=5m ./test/agent -run TestPolicy

.PHONY: test-agent-recovery
test-agent-recovery:
	$(GO) test -race -count=1 ./pkg/agent ./internal/agent
	python3 scripts/agent_recovery_fixture.py --check

.PHONY: test-agent-app
test-agent-app:
	$(GO) test -race -count=1 ./ ./cmd/grayling -run 'TestStartRejectsConflictingOptions|TestListenerTLS'
	$(GO) test -tags=integration -race -count=1 -timeout=3m ./test/agent -run '^TestAgentApp'

.PHONY: test-agent-admin
test-agent-admin:
	$(GO) test -race -count=1 ./internal/admin ./internal/api ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=3m ./test/agent -run '^TestAgentAdmin|^TestAgentManaged|^TestAgentManagement'

.PHONY: test-agent-instances
test-agent-instances:
	$(GO) test -race -count=1 ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestInstance|^TestRegisteredLoop'

.PHONY: test-agent-discovery
test-agent-discovery:
	$(GO) test -race -count=1 ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestDiscovery|^TestInstanceDiscovery|^TestAgentAdminHTTP'

.PHONY: test-agent-bundle
test-agent-bundle:
	$(GO) test -race -count=1 ./internal/admin ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestBundle|^TestPublishedSourceReplica'

.PHONY: test-agent-release
test-agent-release:
	$(GO) test -race -count=1 ./internal/agent -run '^TestRelease'
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestRelease|^TestAgentManagementMigrates|^TestInstanceDiscoveryV4Migration|^TestInstanceMigrationFromV3'

.PHONY: test-agent-prepared
test-agent-prepared:
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestPrepared'

.PHONY: test-agent-preparation
test-agent-preparation:
	$(GO) test -race -count=1 ./internal/agent ./pkg/agent
	$(GO) test -tags=integration -race -count=1 -timeout=5m ./test/agent -run '^TestPrepared|^TestPreparation|^TestReprepare'

.PHONY: test-agent-activation
test-agent-activation:
	$(GO) test -race -count=1 ./pkg/agent -run '^TestActivation'
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestActivation'

.PHONY: test-agent-applied
test-agent-applied:
	$(GO) test -race -count=1 ./pkg/agent ./internal/agent
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestApplied'

.PHONY: test-agent-runtime
test-agent-runtime:
	$(GO) test -race -count=1 ./pkg/agent -run '^TestRuntime'
	$(GO) test -tags=integration -race -count=1 -timeout=3m ./test/agent -run '^TestRuntimeWSS'

.PHONY: test-agent-revocation
test-agent-revocation:
	$(GO) test -race -count=1 ./pkg/agent ./internal/agent
	$(GO) test -tags=integration -race -count=1 -timeout=4m ./test/agent -run '^TestRevocation'

.PHONY: test-agent-cli
test-agent-cli:
	$(GO) test -race -count=1 ./cmd/grayling
	$(GO) test -tags=integration -count=1 -timeout=3m ./test/boot -run '^TestAgentBinarySigningTrustAndRevocationProof$$'

# --- React console frontend --------------------------------------------------
# pnpm workspace at the repo root (packages: web, web-client, web-pages). The
# Vite build output web/static is embedded via web/embed.go and committed;
# check-static fails when the committed tree drifts from a fresh build.
.PHONY: frontend check-static test-static ensure-static build-go dev-frontend dev-backend

frontend:
	pnpm install --frozen-lockfile --prefer-offline
	pnpm --dir web build

check-static: frontend
	@git diff --exit-code -- web/static || (echo "grayling web/static is stale; run 'make frontend' and commit the result" && exit 1)

# The smallest embedded tree so clean checkouts can run Go-only commands. It
# never clobbers a real Vite build: existing web/static/index.html wins.
test-static:
	@if [ ! -f web/static/index.html ]; then \
	  mkdir -p web/static/assets; \
	  printf '<!doctype html><html><head><meta charset="utf-8"><title>Grayling test shell</title></head><body><div id="root"></div></body></html>\n' > web/static/index.html; \
	  : > web/static/assets/.gitkeep; \
	fi

ensure-static: test-static

build-go: ensure-static
	$(GO) build ./...
	$(GO) build -o bin/grayling ./cmd/grayling
	$(GO) build -o bin/grayling-proxy ./cmd/grayling-proxy
	$(GO) build -o bin/grayling-native-proxy ./cmd/grayling-native-proxy
	$(GO) build -o bin/nacos-native-grpc-probe ./cmd/nacos-native-grpc-probe

dev-frontend:
	pnpm --dir web dev

# dev-backend is the existing run target: build everything and start the
# server; use dev-frontend alongside it for Vite hot reload.
dev-backend: run
