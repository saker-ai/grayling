#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# Versions are deliberately frozen; do not silently regenerate with another
# compiler. Install the plugin with the version below in a dedicated GOBIN.
[ "$(protoc --version)" = 'libprotoc 3.21.12' ] || { echo 'requires protoc 3.21.12' >&2; exit 1; }
[ "$(protoc-gen-go --version)" = 'protoc-gen-go v1.36.11' ] || { echo 'requires protoc-gen-go v1.36.11' >&2; exit 1; }
protoc --go_out=. --go_opt=paths=source_relative api/agent/v1/agent.proto
