# GoLab Makefile
#
# The repository is developed inside a larger Go workspace (../.. has go.work),
# but golab must build standalone: its `replace ../gonacos` directive is
# ignored in workspace mode. All targets therefore force GOWORK=off.
GOWORK := off
GO     := GOWORK=$(GOWORK) go

.PHONY: all build test vet tidy smoke run clean sdk-test sdk-python sdk-typescript

all: vet test build

build:
	$(GO) build ./...
	$(GO) build -o bin/golab ./cmd/golab
	$(GO) build -o bin/golab-proxy ./cmd/golab-proxy

test:
	$(GO) test ./... -count=1

smoke:
	$(GO) test ./test/smoke/ -v -count=1

# Multi-language SDK integration suites — both build and run the REAL
# cmd/golab binary (test/boot startup contract) and exercise it over HTTP.
sdk-python:
	cd sdk/python && (test -d .venv || python3 -m venv .venv) \
	  && .venv/bin/pip install -q pytest \
	  && .venv/bin/pytest tests/ -q

sdk-typescript:
	cd sdk/typescript && (test -d node_modules || npm install --no-audit --no-fund) \
	  && npm run typecheck --silent && npm test --silent

sdk-test: sdk-python sdk-typescript

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run: build
	./bin/golab

clean:
	rm -rf bin/
