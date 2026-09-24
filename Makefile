# GoLab Makefile
#
# The repository is developed inside a larger Go workspace (../.. has go.work),
# but golab must build standalone: its `replace ../gonacos` directive is
# ignored in workspace mode. All targets therefore force GOWORK=off.
GOWORK := off
GO     := GOWORK=$(GOWORK) go

.PHONY: all build test vet tidy smoke run clean

all: vet test build

build:
	$(GO) build ./...
	$(GO) build -o bin/golab ./cmd/golab

test:
	$(GO) test ./... -count=1

smoke:
	$(GO) test ./test/smoke/ -v -count=1

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run: build
	./bin/golab

clean:
	rm -rf bin/
