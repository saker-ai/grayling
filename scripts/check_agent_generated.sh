#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
before=$(mktemp)
trap 'rm -f "$before"' EXIT HUP INT TERM
cp api/agent/v1/agent.pb.go "$before"
sh scripts/generate_agent_proto.sh
if ! cmp -s "$before" api/agent/v1/agent.pb.go; then
  echo 'Agent generated code differs; run make generate-agent and review the diff' >&2
  diff -u "$before" api/agent/v1/agent.pb.go || true
  exit 1
fi
