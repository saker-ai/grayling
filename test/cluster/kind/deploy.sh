#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${KUBECONFIG:-}" || -z "${ISSUE13_WORKDIR:-}" ]]; then
  printf 'Set KUBECONFIG and ISSUE13_WORKDIR for the isolated test cluster.\n' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
mkdir -p -- "$ISSUE13_WORKDIR"
chmod 700 -- "$ISSUE13_WORKDIR"
umask 077

openssl rand -hex 24 > "$ISSUE13_WORKDIR/pg-password"
openssl rand -hex 32 > "$ISSUE13_WORKDIR/auth-secret"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -keyout "$ISSUE13_WORKDIR/tls.key" -out "$ISSUE13_WORKDIR/tls.crt" \
  -subj '/CN=grayling-native-kind-test' \
  -addext 'subjectAltName=DNS:grayling-a,DNS:grayling-b,DNS:grayling-a.grayling-issue13.svc.cluster.local,DNS:grayling-b.grayling-issue13.svc.cluster.local' \
  >/dev/null 2>&1

kubectl create namespace grayling-issue13 --dry-run=client -o yaml | kubectl apply -f -
kubectl -n grayling-issue13 create secret generic issue13-secrets \
  --from-file=pg-password="$ISSUE13_WORKDIR/pg-password" \
  --from-file=auth-secret="$ISSUE13_WORKDIR/auth-secret" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n grayling-issue13 create secret tls native-tls \
  --cert="$ISSUE13_WORKDIR/tls.crt" --key="$ISSUE13_WORKDIR/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$script_dir/stack.yaml"

python3 - "$script_dir/grayling.yaml.tpl" "$ISSUE13_WORKDIR" <<'PY'
import pathlib
import sys

template = pathlib.Path(sys.argv[1]).read_text()
workdir = pathlib.Path(sys.argv[2])
password = (workdir / "pg-password").read_text().strip()
for name, node in (("a", "worker"), ("b", "worker2")):
    rendered = (template.replace("__NAME__", name)
                .replace("__NODE__", node)
                .replace("__PG_PASSWORD__", password))
    path = workdir / f"grayling-{name}.yaml"
    path.write_text(rendered)
    path.chmod(0o600)
PY

kubectl apply -f "$ISSUE13_WORKDIR/grayling-a.yaml"
kubectl apply -f "$ISSUE13_WORKDIR/grayling-b.yaml"
kubectl -n grayling-issue13 wait --for=condition=Ready pod/postgres pod/redis --timeout=180s
kubectl -n grayling-issue13 wait --for=condition=Ready pod/grayling-a pod/grayling-b \
  pod/allowed-a pod/allowed-b pod/denied-a pod/denied-b --timeout=180s
