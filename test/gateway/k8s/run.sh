#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
work_dir=$(mktemp -d)
cluster_name=golab-issue12
context=kind-${cluster_name}
kind_version=v0.27.0
kind_sha=a6875aaea358acf0ac07786b1a6755d08fd640f4c79b7a2e46681cc13f49a04b
node_image=kindest/node:v1.32.2@sha256:142f543559cc55d64e1ab9341df08e5ced84bd2e893736da8f51320f26f5950b
crd_url=https://raw.githubusercontent.com/traefik/traefik/v3.7.13/docs/content/reference/dynamic-configuration/kubernetes-crd-definition-v1.yml
crd_sha=1d5e8558803804562238b0d4a57174aadd6d87c36e3c4c55199ebcc356a9406c
created=0

cleanup() {
  if [[ $created == 1 && ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$cluster_name"
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT

if [[ -n ${KIND_BIN:-} ]]; then
  ln -s "$KIND_BIN" "$work_dir/kind"
else
  curl -fsSL "https://kind.sigs.k8s.io/dl/${kind_version}/kind-linux-amd64" -o "$work_dir/kind"
  chmod +x "$work_dir/kind"
fi
echo "$kind_sha  $work_dir/kind" | sha256sum -c -
export PATH="$work_dir:/usr/local/go/bin:$PATH"
export GOTMPDIR="${GOTMPDIR:-/tmp}"
export CGO_ENABLED=0

if ! kind get clusters | rg -qx "$cluster_name"; then
  kind create cluster --name "$cluster_name" --image "$node_image" --wait 120s
  created=1
fi
kubectl --context "$context" version
server_version=$(kubectl --context "$context" get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')
[[ $server_version == v1.32.2 ]] || { echo "unexpected Kubernetes version: $server_version" >&2; exit 1; }

curl -fsSL "$crd_url" -o "$work_dir/crds.yml"
echo "$crd_sha  $work_dir/crds.yml" | sha256sum -c -
kubectl --context "$context" apply -f "$work_dir/crds.yml"
kubectl --context "$context" wait --for=condition=Established crd/ingressroutes.traefik.io crd/middlewares.traefik.io --timeout=90s

docker pull traefik:v3.7.13
docker pull python:3.12-alpine
traefik_digest=$(docker image inspect traefik:v3.7.13 --format '{{index .RepoDigests 0}}')
python_digest=$(docker image inspect python:3.12-alpine --format '{{index .RepoDigests 0}}')
[[ $traefik_digest == traefik@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0 ]]
[[ $python_digest == python@sha256:1b668429b3511ab407d8e00648891631b0b1a4d7e15e3ca70f38ab5b91ad4ab4 ]]
echo "Images: $traefik_digest $python_digest"
kind load docker-image traefik:v3.7.13 python:3.12-alpine --name "$cluster_name"

host_gateway=$(docker network inspect kind --format '{{(index .IPAM.Config 0).Gateway}}')
kube_api=$(kubectl --context "$context" get service kubernetes -o jsonpath='{.spec.clusterIP}')
sed -e "s/__HOST_GATEWAY__/$host_gateway/g" -e "s/__KUBE_API__/$kube_api/g" \
  "$repo_root/test/gateway/k8s/stack.yaml" | kubectl --context "$context" apply -f -
traefik_actor=system:serviceaccount:golab-issue12:traefik
[[ $(kubectl --context "$context" auth can-i list ingressroutes.traefik.io -n golab-issue12 --as="$traefik_actor") == yes ]]
[[ $(kubectl --context "$context" auth can-i create ingressroutes.traefik.io -n golab-issue12 --as="$traefik_actor") == no ]]
[[ $(kubectl --context "$context" auth can-i list nodes --as="$traefik_actor" 2>/dev/null) == no ]]
echo "RBAC: IngressRoute read allowed; IngressRoute write and cluster Node read denied"

export GOLAB_K8S_CONTEXT="$context"
export GOLAB_KIND_NAME="$cluster_name"
export GOLAB_K8S_HOST_GATEWAY="$host_gateway"
cd "$repo_root"
GOWORK=off go test -tags=integration,k8s ./test/gateway -run '^TestKubernetesIngressRouteE2E$' -count=1 -v
