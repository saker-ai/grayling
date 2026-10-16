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

build:
	$(GO) build ./...
	$(GO) build -o bin/grayling ./cmd/grayling
	$(GO) build -o bin/grayling-proxy ./cmd/grayling-proxy
	$(GO) build -o bin/grayling-native-proxy ./cmd/grayling-native-proxy
	$(GO) build -o bin/nacos-native-grpc-probe ./cmd/nacos-native-grpc-probe

test:
	$(GO) test ./... -count=1

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

generate-agent:
	sh scripts/generate_agent_proto.sh

check-agent-generated:
	sh scripts/check_agent_generated.sh
