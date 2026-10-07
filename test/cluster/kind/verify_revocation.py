#!/usr/bin/env python3
"""Revoke a disposable SDK token and prove login/read/subscribe/push denial."""

import importlib.util
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import time

from verify_sdk import NAMESPACE, SCRIPT_DIR, prepare, probe, subscribed, publish_next_revision, run


def api_module():
    spec = importlib.util.spec_from_file_location("provision", SCRIPT_DIR / "provision.py")
    provision = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(provision)
    return provision


def wait_api():
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", 48688), timeout=1):
                return
        except OSError:
            time.sleep(0.2)
    raise RuntimeError("GoLab A admin API port-forward did not start")


def expect(command, fragment, timeout=20):
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    if result.returncode or fragment not in result.stdout:
        raise RuntimeError(f"revocation check failed: {result.stdout.strip()} {result.stderr.strip()}")
    print(result.stdout.strip())


def wait_shared_revision():
    deadline = time.monotonic() + 35
    last = None
    while time.monotonic() < deadline:
        revisions = []
        for pod, server in (("allowed-a", "golab-a"), ("allowed-b", "golab-b")):
            result = subprocess.run(probe(pod, server, "a", "a"),
                                    capture_output=True, text=True, timeout=15)
            match = re.search(r"PASS official SDK read revision (\d+)", result.stdout)
            if result.returncode or not match:
                revisions = []
                break
            revisions.append(int(match.group(1)))
        last = revisions
        if len(revisions) == 2 and revisions[0] == revisions[1] and revisions[0] > 0:
            print(f"PASS both GoLab entries serve tenant-a revision {revisions[0]}")
            return
        time.sleep(1)
    raise RuntimeError(f"GoLab entries did not converge before revocation: {last}")


def main():
    if not os.environ.get("KUBECONFIG") or not os.environ.get("ISSUE13_WORKDIR"):
        raise RuntimeError("set KUBECONFIG and ISSUE13_WORKDIR")
    workdir = Path(os.environ["ISSUE13_WORKDIR"])
    for name in ("sdk-probe", "native-grpc-probe", "tls.crt", "tenant-a-namespace"):
        if not (workdir / name).is_file():
            raise RuntimeError(f"fixture missing: {workdir / name}")
    prepare(workdir)
    wait_shared_revision()
    port_forward = subprocess.Popen(
        ["kubectl", "-n", NAMESPACE, "port-forward", "svc/golab-a", "48688:8688"],
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
    )
    watcher = None
    try:
        wait_api()
        provision = api_module()
        provision.API = "http://127.0.0.1:48688"
        owner = (workdir / "owner-token").read_text().strip()
        issued = provision.request(
            "POST", "/api/v1/admin/workspaces/tenant-a/tokens", owner,
            {"name": "kind-revocation", "kind": "sdk", "environment": "prod"}, 201,
        )
        secret = workdir / "revocation-token"
        provision.write_secret(secret, issued["Secret"])
        run("cp", str(secret), "allowed-a:/tmp/revocation-token")
        watcher = subprocess.Popen(
            ["kubectl", "-n", NAMESPACE, "exec", "allowed-a", "--", "/tmp/sdk-probe",
             "-server", "golab-a", "-namespace-file", "/tmp/tenant-a-namespace",
             "-token-file", "/tmp/revocation-token", "-ca-file", "/tmp/tls.crt",
             "-watch", "-watch-timeout", "20s"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        subscribed(watcher)
        provision.request("DELETE", "/api/v1/admin/tokens/" + issued["Token"]["id"], owner)
        print("PASS disposable SDK token revoked")
        expect(["kubectl", "-n", NAMESPACE, "exec", "allowed-a", "--", "/tmp/sdk-probe",
                "-server", "golab-a", "-namespace-file", "/tmp/tenant-a-namespace",
                "-token-file", "/tmp/revocation-token", "-ca-file", "/tmp/tls.crt",
                "-expect-revoked-login"],
               "PASS revoked native login denied with HTTP 401")
        service_ip = run("get", "svc", "golab-a", "-o", "jsonpath={.spec.clusterIP}")
        namespace = (workdir / "tenant-a-namespace").read_text().strip()
        expect(["kubectl", "-n", NAMESPACE, "exec", "allowed-a", "--", "/tmp/native-grpc-probe",
                "-address", f"{service_ip}:9848", "-server-name", "golab-a", "-ca-file", "/tmp/tls.crt",
                "-revoked-namespace", namespace, "-revoked-token-file", "/tmp/revocation-token"],
               "PASS: anonymous and revoked native gRPC reads and subscriptions rejected with status 16")
        publish_next_revision(api=provision.API, owner=owner)
        out, err = watcher.communicate(timeout=25)
        if "PASS official SDK received revision" in out:
            raise RuntimeError("revoked SDK subscription received a later revision")
        if watcher.returncode == 0 or "timed out waiting for manifest push" not in err:
            raise RuntimeError(f"revoked SDK watcher ended unexpectedly: {out.strip()} {err.strip()}")
        print("PASS revoked SDK subscription received no new revision")
    finally:
        if watcher is not None and watcher.poll() is None:
            watcher.terminate()
            watcher.wait(timeout=10)
        if port_forward.poll() is None:
            port_forward.terminate()
            port_forward.wait(timeout=10)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, KeyError, subprocess.TimeoutExpired) as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        sys.exit(1)
