#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${DOCKER_HOST:-}" || -z "${KUBECONFIG:-}" || -z "${ISSUE13_WORKDIR:-}" ]]; then
  printf 'Set DOCKER_HOST, KUBECONFIG, and ISSUE13_WORKDIR.\n' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
mkdir -p -- "$ISSUE13_WORKDIR/images" "$ISSUE13_WORKDIR/go-tmp"
chmod 700 -- "$ISSUE13_WORKDIR"

if ! kind get clusters | grep -qx 'golab-issue13'; then
  kind create cluster --name golab-issue13 --config "$script_dir/kind.yaml" --image kindest/node:v1.37.0
fi

for image in quay.io/cilium/cilium:v1.20.2 quay.io/cilium/operator-generic:v1.20.2; do
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker pull "$image"
  fi
done
kind load docker-image --name golab-issue13 \
  quay.io/cilium/cilium:v1.20.2 quay.io/cilium/operator-generic:v1.20.2

cilium_chart="$(helm env HELM_REPOSITORY_CACHE)/cilium-1.20.2.tgz"
if [[ ! -f "$cilium_chart" ]]; then
  helm repo add cilium https://helm.cilium.io/ --force-update
  helm repo update cilium
  cilium_chart=cilium/cilium
fi
helm upgrade --install cilium "$cilium_chart" --version 1.20.2 \
  --namespace kube-system \
  --set ipam.mode=kubernetes \
  --set kubeProxyReplacement=false \
  --set envoy.enabled=false \
  --set image.useDigest=false \
  --set operator.image.useDigest=false \
  --wait --timeout 5m

GOWORK=off CGO_ENABLED=0 GOTMPDIR="$ISSUE13_WORKDIR/go-tmp" \
  go build -o "$ISSUE13_WORKDIR/images/golab" ./cmd/golab
GOWORK=off CGO_ENABLED=0 GOTMPDIR="$ISSUE13_WORKDIR/go-tmp" \
  go build -o "$ISSUE13_WORKDIR/images/tls-l4-proxy" ./cmd/golab-native-proxy
GOWORK=off CGO_ENABLED=0 GOTMPDIR="$ISSUE13_WORKDIR/go-tmp" \
  go build -o "$ISSUE13_WORKDIR/sdk-probe" "$script_dir/sdk_probe"
GOWORK=off CGO_ENABLED=0 GOTMPDIR="$ISSUE13_WORKDIR/go-tmp" \
  go build -o "$ISSUE13_WORKDIR/native-grpc-probe" ./cmd/nacos-native-grpc-probe

printf 'FROM scratch\nCOPY golab /golab\nENTRYPOINT ["/golab"]\n' |
  docker build -t golab:issue13-local -f - "$ISSUE13_WORKDIR/images"
printf 'FROM scratch\nCOPY tls-l4-proxy /tls-l4-proxy\nENTRYPOINT ["/tls-l4-proxy"]\n' |
  docker build -t golab-native-proxy:issue13-local -f - "$ISSUE13_WORKDIR/images"

for image in postgres:16-alpine redis:8-alpine busybox:1.36; do
  if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker pull "$image"
  fi
done
kind load docker-image --name golab-issue13 \
  golab:issue13-local golab-native-proxy:issue13-local \
  postgres:16-alpine redis:8-alpine busybox:1.36

"$script_dir/deploy.sh"
