#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
work_dir=$(mktemp -d)
cluster_name=${GRAYLING_KIND_NAME:-grayling-issue12-$$}
[[ $cluster_name =~ ^[a-z0-9][a-z0-9-]{1,49}$ ]] || { echo "invalid disposable cluster name" >&2; exit 1; }
# Never read or mutate the operator's current kubeconfig.
export KUBECONFIG="$work_dir/kubeconfig"
context=kind-${cluster_name}
kind_version=v0.27.0
kind_sha=a6875aaea358acf0ac07786b1a6755d08fd640f4c79b7a2e46681cc13f49a04b
node_image=kindest/node:v1.32.2@sha256:142f543559cc55d64e1ab9341df08e5ced84bd2e893736da8f51320f26f5950b
crd_url=https://raw.githubusercontent.com/traefik/traefik/v3.7.13/docs/content/reference/dynamic-configuration/kubernetes-crd-definition-v1.yml
crd_sha=1d5e8558803804562238b0d4a57174aadd6d87c36e3c4c55199ebcc356a9406c
created=0
tagged_images=()

cleanup() {
  if [[ $created == 1 && ${KEEP_CLUSTER:-0} != 1 ]]; then
    kind delete cluster --name "$cluster_name"
  fi
  if (( ${#tagged_images[@]} )); then
    docker image rm "${tagged_images[@]}" >/dev/null 2>&1 || true
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
export PATH="$work_dir:$PATH"
export GOTMPDIR="${GOTMPDIR:-/tmp}"
export CGO_ENABLED=0

if kind get clusters | rg -qx "$cluster_name"; then
  echo "refusing to reuse an existing cluster: $cluster_name" >&2
  exit 1
fi
created=1
kind create cluster --name "$cluster_name" --image "$node_image" --wait 120s
kubectl --context "$context" version
server_version=$(kubectl --context "$context" get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')
[[ $server_version == v1.32.2 ]] || { echo "unexpected Kubernetes version: $server_version" >&2; exit 1; }

curl -fsSL "$crd_url" -o "$work_dir/crds.yml"
echo "$crd_sha  $work_dir/crds.yml" | sha256sum -c -
kubectl --context "$context" apply -f "$work_dir/crds.yml"
kubectl --context "$context" wait --for=condition=Established crd/ingressroutes.traefik.io crd/middlewares.traefik.io --timeout=90s

# Pull the checked immutable manifests, not mutable tags whose current targets
# can drift. Use unique owned aliases for kind's image loader and pod manifests.
traefik_digest=traefik@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0
python_digest=python@sha256:1b668429b3511ab407d8e00648891631b0b1a4d7e15e3ca70f38ab5b91ad4ab4
traefik_alias=grayling-fixture/traefik:$cluster_name
python_alias=grayling-fixture/python:$cluster_name
for ref in "$traefik_digest" "$python_digest"; do
  docker pull "$ref"
  docker image inspect "$ref" --format '{{range .RepoDigests}}{{println .}}{{end}}' | rg -Fxq "$ref"
done
for alias in "$traefik_alias" "$python_alias"; do
  if docker image inspect "$alias" >/dev/null 2>&1; then
    echo "refusing to overwrite an existing fixture image alias" >&2
    exit 1
  fi
done
docker tag "$traefik_digest" "$traefik_alias"
tagged_images+=("$traefik_alias")
docker tag "$python_digest" "$python_alias"
tagged_images+=("$python_alias")
echo "Images: $traefik_digest $python_digest"
kind load docker-image "$traefik_alias" "$python_alias" --name "$cluster_name"

host_gateway=$(docker network inspect kind --format '{{(index .IPAM.Config 0).Gateway}}')
kube_api=$(kubectl --context "$context" get service kubernetes -o jsonpath='{.spec.clusterIP}')
sed -e "s/__HOST_GATEWAY__/$host_gateway/g" -e "s/__KUBE_API__/$kube_api/g" \
  -e "s|traefik:v3.7.13|$traefik_alias|g" -e "s|python:3.12-alpine|$python_alias|g" \
  "$repo_root/test/gateway/k8s/stack.yaml" | kubectl --context "$context" apply -f -
traefik_actor=system:serviceaccount:grayling-issue12:traefik
[[ $(kubectl --context "$context" auth can-i list ingressroutes.traefik.io -n grayling-issue12 --as="$traefik_actor") == yes ]]
[[ $(kubectl --context "$context" auth can-i create ingressroutes.traefik.io -n grayling-issue12 --as="$traefik_actor") == no ]]
[[ $(kubectl --context "$context" auth can-i list nodes --as="$traefik_actor" 2>/dev/null) == no ]]
echo "RBAC: IngressRoute read allowed; IngressRoute write and cluster Node read denied"

export GRAYLING_K8S_CONTEXT="$context"
export GRAYLING_KIND_NAME="$cluster_name"
export GRAYLING_K8S_HOST_GATEWAY="$host_gateway"
cd "$repo_root"
GOWORK=off go test -tags=integration,k8s ./test/gateway -run '^TestKubernetesIngressRouteE2E$' -count=1 -v
