# Grayling Makefile
#
# Grayling builds as a standalone module. Force GOWORK=off so a parent
# workspace does not override the pinned gonacos module version.
GOWORK := off
GO     := GOWORK=$(GOWORK) go
RUN_ARGS ?=
ADMIN_PASSWORD_FILE ?= data/admin-password
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
	@mkdir -p "$(dir $(ADMIN_PASSWORD_FILE))"
	@if [ -z "$$GRAYLING_ADMIN_PASSWORD" ] && [ ! -e "$(ADMIN_PASSWORD_FILE)" ]; then \
	  umask 077; \
	  password=$$(od -An -N24 -tx1 /dev/urandom | tr -d '[:space:]'); \
	  printf '%s\n' "$$password" > "$(ADMIN_PASSWORD_FILE)"; \
	  echo "Saved to $(ADMIN_PASSWORD_FILE) (use 'make show-admin-password' to view it again)"; \
	fi
	@echo "Admin password configured (use 'make show-admin-password' to display the password file)."

show-admin-password:
	@cat "$(ADMIN_PASSWORD_FILE)"

check-run-port:
	@if command -v lsof >/dev/null && lsof -nP -tiTCP:$(RUN_PORT) -sTCP:LISTEN >/dev/null; then \
	  echo "Port $(RUN_PORT) is occupied; stop its listener explicitly before running Grayling" >&2; exit 1; \
	fi

run: build ensure-admin-password check-run-port
	GRAYLING_ADMIN_PASSWORD_FILE="$(ADMIN_PASSWORD_FILE)" ./bin/grayling -addr :$(RUN_PORT) \
	  -gonacos-addr 127.0.0.1:0 -gonacos-grpc-addr 127.0.0.1:0 \
	  -gonacos-root "$$($(GO) list -m -f '{{.Dir}}' github.com/godeps/gonacos)" $(RUN_ARGS)

clean:
	rm -rf bin/
