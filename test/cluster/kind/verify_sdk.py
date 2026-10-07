#!/usr/bin/env python3
"""Exercise the official Nacos SDK from two cross-node Kind client Pods."""

import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import time


NAMESPACE = "golab-issue13"
SCRIPT_DIR = Path(__file__).resolve().parent


def kubectl(*args):
    return ["kubectl", "-n", NAMESPACE, *args]


def run(*args):
    result = subprocess.run(kubectl(*args), capture_output=True, text=True, timeout=40)
    if result.returncode:
        raise RuntimeError(f"kubectl {' '.join(args[:2])} failed: {result.stderr.strip()}")
    return result.stdout.strip()


def prepare(workdir):
    binary = workdir / "sdk-probe"
    grpc_binary = workdir / "native-grpc-probe"
    for artifact in (binary, grpc_binary):
        if not artifact.is_file():
            raise RuntimeError(f"build probe first: {artifact}")
    for pod in ("allowed-a", "allowed-b"):
        for source, target in (
            (binary, "sdk-probe"), (grpc_binary, "native-grpc-probe"),
            (workdir / "tls.crt", "tls.crt"),
            *((workdir / f"tenant-{tenant}-{suffix}", f"tenant-{tenant}-{suffix}")
              for tenant in ("a", "b") for suffix in ("namespace", "token")),
        ):
            if not source.is_file():
                raise RuntimeError(f"fixture missing: {source}")
            run("cp", str(source), f"{pod}:/tmp/{target}")


def probe(pod, server, namespace_tenant, token_tenant, *extra):
    return kubectl(
        "exec", pod, "--", "/tmp/sdk-probe", "-server", server,
        "-namespace-file", f"/tmp/tenant-{namespace_tenant}-namespace",
        "-token-file", f"/tmp/tenant-{token_tenant}-token",
        "-ca-file", "/tmp/tls.crt", *extra,
    )


def expect(command, fragment):
    result = subprocess.run(command, capture_output=True, text=True, timeout=30)
    if result.returncode or fragment not in result.stdout:
        raise RuntimeError(f"SDK check failed: {result.stdout.strip()} {result.stderr.strip()}")
    print(result.stdout.strip())


def verify_grpc(pod, server, workdir):
    service_ip = run("get", "svc", server, "-o", "jsonpath={.spec.clusterIP}")
    namespace_a = (workdir / "tenant-a-namespace").read_text().strip()
    namespace_b = (workdir / "tenant-b-namespace").read_text().strip()
    command = kubectl(
        "exec", pod, "--", "/tmp/native-grpc-probe",
        "-address", f"{service_ip}:9848", "-server-name", server,
        "-ca-file", "/tmp/tls.crt",
        "-tenant-a-namespace", namespace_a,
        "-tenant-b-namespace", namespace_b,
        "-tenant-a-token-file", "/tmp/tenant-a-token",
        "-tenant-b-token-file", "/tmp/tenant-b-token",
    )
    expect(command, "PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied")


def subscribed(process):
    # readline owns a Python text buffer, so checking the raw fd after each
    # line can miss a second line already buffered by the first read.
    for _ in range(3):
        line = process.stdout.readline().strip()
        if line:
            print(line)
        if line == "SUBSCRIBED":
            return
        if not line or process.poll() is not None:
            break
    raise RuntimeError("SDK subscription did not start")


def publish_next_revision():
    spec = importlib.util.spec_from_file_location("provision", SCRIPT_DIR / "provision.py")
    provision = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(provision)
    owner = provision.owner_token()
    base = "/api/v1/admin/workspaces/tenant-a/environments/prod/flags/sample"
    reviewer = provision.request(
        "POST", "/api/v1/admin/workspaces/tenant-a/tokens", owner,
        {"name": "kind-reviewer", "kind": "admin", "role": "publisher"}, 201,
    )["Secret"]
    flag = {
        "schemaVersion": 1, "key": "sample", "type": "string",
        "variations": {"on": f"tenant-a-{time.time_ns()}"},
        "defaultRule": {"variation": "on"},
    }
    for method, path, token, body in (
        ("PUT", base + "/draft", owner, {"content": flag, "risk": "normal"}),
        ("POST", base + "/submit", owner, {"reason": "Kind SDK push"}),
        ("POST", base + "/approve", reviewer, {"note": "ok"}),
        ("POST", base + "/publish", owner, {"note": "Kind SDK push"}),
    ):
        provision.request(method, path, token, body)
    print("PASS published a new tenant-a revision")


def main():
    if not os.environ.get("KUBECONFIG") or not os.environ.get("ISSUE13_WORKDIR"):
        raise RuntimeError("set KUBECONFIG and ISSUE13_WORKDIR")
    workdir = Path(os.environ["ISSUE13_WORKDIR"])
    prepare(workdir)
    verify_grpc("allowed-a", "golab-a", workdir)
    verify_grpc("allowed-b", "golab-b", workdir)
    expect(probe("allowed-a", "golab-a", "a", "a"), "PASS official SDK read")
    expect(probe("allowed-b", "golab-b", "b", "b"), "PASS official SDK read")
    expect(probe("allowed-a", "golab-a", "b", "a", "-expect-denied"),
           "PASS official SDK cross-tenant read denied")
    expect(probe("allowed-b", "golab-b", "a", "b", "-expect-denied"),
           "PASS official SDK cross-tenant read denied")
    watchers = []
    try:
        for pod, server in (("allowed-a", "golab-a"), ("allowed-b", "golab-b")):
            process = subprocess.Popen(
                probe(pod, server, "a", "a", "-watch"),
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            watchers.append(process)
            subscribed(process)
        publish_next_revision()
        for process in watchers:
            out, err = process.communicate(timeout=45)
            if process.returncode or "PASS official SDK received revision" not in out:
                raise RuntimeError(f"SDK push failed: {out.strip()} {err.strip()}")
            print(out.strip())
    finally:
        for process in watchers:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        sys.exit(1)
