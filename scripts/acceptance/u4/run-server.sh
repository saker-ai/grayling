#!/bin/sh
# U4 acceptance — standalone server bootstrap for manual reruns.
# Builds bin/grayling if missing, boots an isolated instance (fresh store in a
# mktemp dir, embedded gonacos on ephemeral loopback ports, dev autologin on),
# waits for /api/v1/health, prints the base URL, and tears down on Ctrl-C.
#
# Usage:
#   scripts/acceptance/u4/run-server.sh            # port 28090 (or next free)
#   U4_PORT=28095 scripts/acceptance/u4/run-server.sh
# Then drive any U4 script against it without a rebuild/reboot:
#   U4_BASE_URL=http://127.0.0.1:28090 node scripts/acceptance/u4/axe-audit.mjs
set -eu
cd "$(dirname "$0")/../../.."

export TMPDIR="${TMPDIR:-$HOME/tmp-go}"
mkdir -p "$TMPDIR"

if [ ! -x bin/grayling ] || [ "${U4_REBUILD:-0}" = "1" ]; then
  echo "[run-server] building bin/grayling…" >&2
  make build >&2
fi

PORT="${U4_PORT:-28090}"
i=0
while [ $i -lt 20 ]; do
  if (exec 3<>/dev/tcp/127.0.0.1/"$PORT") 2>/dev/null; then
    PORT=$((PORT + 1)); i=$((i + 1))
  else
    break
  fi
done

STORE="$(mktemp -d "$TMPDIR/grayling-u4-XXXXXX")"
echo "[run-server] store: $STORE" >&2

GONACOS_ROOT="$(GOWORK=off go list -m -f '{{.Dir}}' github.com/godeps/gonacos 2>/dev/null || true)"

GRAYLING_DEV_AUTOLOGIN=1 ./bin/grayling \
  -addr "127.0.0.1:$PORT" \
  -data "$STORE/store.json" \
  -gonacos-data "$STORE/gonacos" \
  -gonacos-addr 127.0.0.1:0 \
  -gonacos-grpc-addr 127.0.0.1:0 \
  ${GONACOS_ROOT:+-gonacos-root "$GONACOS_ROOT"} &
PID=$!

cleanup() {
  kill "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  rm -rf "$STORE"
  echo "[run-server] stopped; removed $STORE" >&2
}
trap cleanup EXIT INT TERM

i=0
until curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ $i -gt 120 ]; then
    echo "[run-server] health check timed out" >&2
    exit 1
  fi
  sleep 0.25
done

echo "[run-server] healthy at http://127.0.0.1:$PORT (Ctrl-C to stop)" >&2
echo "http://127.0.0.1:$PORT"
wait "$PID"
