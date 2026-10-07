#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${KUBECONFIG:-}" || -z "${ISSUE13_WORKDIR:-}" ]]; then
  printf 'Set KUBECONFIG and ISSUE13_WORKDIR for the isolated test cluster.\n' >&2
  exit 2
fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../../.." && pwd)
workdir="$ISSUE13_WORKDIR/deploy-manifest"
mkdir -p -- "$workdir"
chmod 700 "$workdir"
umask 077

for name in pg-password auth-secret sdk-probe tenant-a-namespace tenant-a-token tenant-b-namespace tenant-b-token; do
  test -f "$ISSUE13_WORKDIR/$name"
done

kubectl create namespace golab --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace golab-native-clients --dry-run=client -o yaml | kubectl apply -f -
python3 - "$ISSUE13_WORKDIR/pg-password" "$workdir" <<'PY'
import pathlib
import sys
import urllib.parse

password = pathlib.Path(sys.argv[1]).read_text().strip()
workdir = pathlib.Path(sys.argv[2])
(workdir / "pg-dsn").write_text(
    "postgres://golab:" + urllib.parse.quote(password, safe="") +
    "@postgres.golab-issue13.svc.cluster.local:5432/golab?sslmode=disable"
)
(workdir / "redis-addr").write_text("redis.golab-issue13.svc.cluster.local:6379")
PY
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$workdir/tls.key" -out "$workdir/tls.crt" \
  -subj '/CN=golab-native-kind-test' \
  -addext 'subjectAltName=DNS:golab-native-0.golab-native-headless.golab.svc.cluster.local,DNS:golab-native-1.golab-native-headless.golab.svc.cluster.local' \
  >/dev/null 2>&1
kubectl -n golab create secret generic golab-native-secrets \
  --from-file=pg-dsn="$workdir/pg-dsn" \
  --from-file=redis-addr="$workdir/redis-addr" \
  --from-file=auth-secret="$ISSUE13_WORKDIR/auth-secret" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n golab create secret tls golab-native-tls \
  --cert="$workdir/tls.crt" --key="$workdir/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -

sed \
  -e 's#registry.example.invalid/golab:replace-with-reviewed-digest#golab:issue13-local#' \
  -e 's#registry.example.invalid/golab-native-proxy:replace-with-reviewed-digest#golab-native-proxy:issue13-local#' \
  "$repo_root/deploy/kubernetes/native/manifest.yaml" > "$workdir/manifest.yaml"
kubectl apply -f "$workdir/manifest.yaml"
kubectl -n golab rollout status statefulset/golab-native --timeout=180s

cat > "$workdir/clients.yaml" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: native-allowed
  namespace: golab-native-clients
  labels: {native-access: allowed}
spec:
  containers:
    - {name: probe, image: busybox:1.36, imagePullPolicy: Never, command: [sleep, "36000"]}
---
apiVersion: v1
kind: Pod
metadata:
  name: native-denied
  namespace: golab-native-clients
  labels: {native-access: denied}
spec:
  containers:
    - {name: probe, image: busybox:1.36, imagePullPolicy: Never, command: [sleep, "36000"]}
YAML
kubectl apply -f "$workdir/clients.yaml"
kubectl -n golab-native-clients wait --for=condition=Ready pod/native-allowed pod/native-denied --timeout=120s

for ordinal in 0 1; do
  server="golab-native-$ordinal.golab-native-headless.golab.svc.cluster.local"
  pod_ip=$(kubectl -n golab get pod "golab-native-$ordinal" -o jsonpath='{.status.podIP}')
  for port in 8848 9848; do
    kubectl -n golab-native-clients exec native-allowed -- nc -vz -w 2 "$server" "$port" >/dev/null
    if kubectl -n golab-native-clients exec native-denied -- nc -vz -w 2 "$server" "$port" >/dev/null 2>&1; then
      printf 'FAIL denied client reached %s:%s\n' "$server" "$port" >&2
      exit 1
    fi
  done
  for port in 18848 19848; do
    if kubectl -n golab-native-clients exec native-allowed -- nc -vz -w 2 "$pod_ip" "$port" >/dev/null 2>&1; then
      printf 'FAIL plaintext backend reachable at %s:%s\n' "$pod_ip" "$port" >&2
      exit 1
    fi
  done
  printf 'PASS production manifest policy and loopback backends for %s\n' "$server"
done

for source in sdk-probe tenant-a-namespace tenant-a-token tenant-b-namespace tenant-b-token; do
  kubectl -n golab-native-clients cp "$ISSUE13_WORKDIR/$source" "native-allowed:/tmp/$source"
done
kubectl -n golab-native-clients cp "$workdir/tls.crt" native-allowed:/tmp/tls.crt
for ordinal in 0 1; do
  server="golab-native-$ordinal.golab-native-headless.golab.svc.cluster.local"
  for tenant in a b; do
    kubectl -n golab-native-clients exec native-allowed -- /tmp/sdk-probe \
      -server "$server" -namespace-file "/tmp/tenant-$tenant-namespace" \
      -token-file "/tmp/tenant-$tenant-token" -ca-file /tmp/tls.crt
  done
done
printf 'PASS official SDK reads from both tenants through both production-example replicas\n'
