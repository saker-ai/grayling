#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${KUBECONFIG:-}" ]]; then
  printf 'Set KUBECONFIG for the isolated test cluster.\n' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
namespace=golab-issue13
policy_restored=true

restore_policy() {
  if [[ "$policy_restored" == false ]]; then
    kubectl apply -f "$script_dir/stack.yaml" >/dev/null
  fi
}
trap restore_policy EXIT

connects() {
  kubectl -n "$namespace" exec "$1" -- nc -vz -w 2 "$2" "$3" >/dev/null 2>&1
}

expect_connection() {
  local want=$1 pod=$2 host=$3 port=$4
  for _ in {1..12}; do
    if connects "$pod" "$host" "$port"; then
      if [[ "$want" == open ]]; then
        printf 'PASS open %s -> %s:%s\n' "$pod" "$host" "$port"
        return
      fi
    elif [[ "$want" == closed ]]; then
      printf 'PASS closed %s -> %s:%s\n' "$pod" "$host" "$port"
      return
    fi
    sleep 1
  done
  printf 'FAIL wanted %s %s -> %s:%s\n' "$want" "$pod" "$host" "$port" >&2
  exit 1
}

for replica in a b; do
  pod_ip=$(kubectl -n "$namespace" get pod "golab-$replica" -o jsonpath='{.status.podIP}')
  for port in 8848 9848; do
    expect_connection open "allowed-$replica" "golab-$replica" "$port"
    expect_connection closed "denied-$replica" "golab-$replica" "$port"
  done
  for port in 18848 19848; do
    expect_connection closed "allowed-$replica" "$pod_ip" "$port"
  done
done

kubectl -n "$namespace" delete networkpolicy native-allow-listed-clients >/dev/null
policy_restored=false
expect_connection open denied-a golab-a 8848
expect_connection open denied-b golab-b 9848
restore_policy
policy_restored=true
expect_connection closed denied-a golab-a 8848
expect_connection closed denied-b golab-b 9848
