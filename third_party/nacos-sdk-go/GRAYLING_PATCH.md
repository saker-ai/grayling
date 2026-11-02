# Grayling local synchronization patch

Source: github.com/nacos-group/nacos-sdk-go/v2 v2.3.5, copied from the verified
Go module cache. Original Apache-2.0 license and notices are retained.
Upstream example applications (including placeholder credential/certificate
files) and upstream GitHub automation are omitted from this source copy;
protocol/client packages are retained. Markdown whitespace is normalized.

The local changes in `common/remote/rpc` publish the current connection through
an atomic pointer and guard registry lookup/snapshot/deletion with the existing
creation mutex. Connection abandonment and diagnostic status reads also use
atomic access rather than racing their writers. They repair concurrent reconnect/event delivery and
shutdown/client-creation races observed by the complete local race gate.
No authentication, permission, request payload, retry budget or protocol behavior
is intentionally changed. Compatibility fixtures and the race detector remain
enabled. This is a maintained local patch, not an assertion that unmodified
upstream v2.3.5 is race-free.

Regression test from repository root: `make test-nacos-synchronization`.
Repository integration: `go test -race -count=1 ./test/cluster ./test/gateway`.

Upstream reference: [reported RPC data race](https://github.com/nacos-group/nacos-sdk-go/issues/833).
The local race log, rather than that issue title, determines the repaired paths.
