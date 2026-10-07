# Issue #13: isolated Kind network and native SDK fixture

This fixture runs two GoLab replicas on different Kind workers with shared PostgreSQL/Redis, Cilium NetworkPolicy, and a TLS/L4 sidecar on each replica. The sidecar logs the client Pod IP and opens one backend TCP connection per client connection. It is a **local simulation**, not proof of production cross-host routing or production ingress configuration.

Requirements: Linux Docker with enough free space, Kind v0.33.0, Helm v3.19.0, Go matching `go.mod`, `kubectl`, Python 3, and OpenSSL. Use a dedicated Docker daemon if the default Docker data root is full. The commands below use whichever daemon `DOCKER_HOST` selects. Do not use a shared cluster: `verify_policy.sh` briefly removes and restores the test namespace's NetworkPolicy to prove that it causes the denial.

From the repository root:

```sh
export DOCKER_HOST=unix:///path/to/isolated/docker.sock
export KUBECONFIG=/path/to/isolated/kubeconfig
export ISSUE13_WORKDIR=/path/to/private/golab-issue13
mkdir -p "$ISSUE13_WORKDIR"
chmod 700 "$ISSUE13_WORKDIR"
test/cluster/kind/setup_cluster.sh
test/cluster/kind/verify_policy.sh
```

In a separate terminal, keep the admin API port-forward running for disposable tenant provisioning and the SDK push check:

```sh
kubectl -n golab-issue13 port-forward svc/golab-a 28688:8688
```

Then:

```sh
python3 test/cluster/kind/provision.py
python3 test/cluster/kind/verify_sdk.py
```

`provision.py` is for a fresh namespace and creates two disposable workspaces, one published flag each, and SDK tokens. It stores tokens, namespaces, and the local test certificate in `ISSUE13_WORKDIR`; restrict that directory to the test operator and never commit its contents. `verify_sdk.py` copies the official Go SDK and direct gRPC probes plus fixture credentials into the allowed client Pods. It checks HTTP config bypass/invalid login, anonymous and cross-tenant gRPC read/subscribe refusal, each tenant's SDK read, and an update pushed to subscriptions on both replicas. The Go SDK probe uses a fresh cache per invocation so an old local SDK response cannot pass a read check.

`verify_policy.sh` checks both native ports from allowed and denied clients on the opposite worker, and checks that the loopback backend ports are unreachable. Removing the policy makes denied clients connect; restoring it blocks them again. Run only in this disposable namespace. To inspect actual source IP and backend connection mapping:

```sh
kubectl -n golab-issue13 get pods -o wide
kubectl -n golab-issue13 logs golab-a -c tls-l4-proxy
kubectl -n golab-issue13 logs golab-b -c tls-l4-proxy
```

The fixed addresses in `kind.yaml` are for the isolated test cluster; change them if they overlap the host network. `setup_cluster.sh` expects the pinned base images to be pullable from the selected Docker daemon. If registry access is unavailable, load those pinned images into that daemon before running the script and skip its `docker pull` lines. The test-only sidecar is not a production proxy configuration. Before production support can be declared, run the deployment evidence gate in `scripts/check_nacos_native_topology.py` against the real TLS/L4 ingress from separate allowed and denied physical hosts, inspect ingress connection logs, and repeat the lifecycle checks there.

Cleanup after retaining sanitized evidence:

```sh
kind delete cluster --name golab-issue13
```
