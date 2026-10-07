#!/usr/bin/env python3
"""Keep one official SDK subscription while its initial GoLab Pod is replaced."""

import importlib.util
import os
from pathlib import Path
import socket
import subprocess
import sys
import time

from verify_sdk import NAMESPACE, SCRIPT_DIR, prepare, probe, subscribed, verify_grpc, publish_next_revision


def kubectl(*args):
    return ["kubectl", "-n", NAMESPACE, *args]


def invoke(*args):
    result = subprocess.run(kubectl(*args), capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError(f"kubectl {' '.join(args[:2])} failed: {result.stderr.strip()}")


def wait_api():
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", 38688), timeout=1):
                return
        except OSError:
            time.sleep(0.2)
    raise RuntimeError("GoLab B admin API port-forward did not start")


def owner_token():
    saved = Path(os.environ["ISSUE13_WORKDIR"]) / "owner-token"
    if saved.is_file():
        return saved.read_text().strip()
    spec = importlib.util.spec_from_file_location("provision", SCRIPT_DIR / "provision.py")
    provision = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(provision)
    return provision.owner_token()


def main():
    if not os.environ.get("KUBECONFIG") or not os.environ.get("ISSUE13_WORKDIR"):
        raise RuntimeError("set KUBECONFIG and ISSUE13_WORKDIR")
    workdir = Path(os.environ["ISSUE13_WORKDIR"])
    for name in ("a", "b"):
        if not (workdir / f"golab-{name}.yaml").is_file():
            raise RuntimeError("deploy.sh must render both GoLab Pod manifests first")
    prepare(workdir)
    owner = owner_token()
    watcher = None
    port_forward = None
    try:
        invoke("delete", "pod", "golab-b", "--wait=true")
        print("PASS GoLab B stopped before official SDK subscription")
        watcher = subprocess.Popen(
            probe("allowed-a", "golab-a,golab-b", "a", "a", "-watch", "-watch-timeout", "120s"),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        subscribed(watcher)
        invoke("apply", "-f", str(workdir / "golab-b.yaml"))
        invoke("wait", "--for=condition=Ready", "pod/golab-b", "--timeout=90s")
        for attempt in range(15):
            try:
                verify_grpc("allowed-b", "golab-b", workdir)
                break
            except RuntimeError:
                if attempt == 14:
                    raise
                time.sleep(1)
        invoke("delete", "pod", "golab-a", "--wait=true")
        print("PASS GoLab A stopped after B recovered")
        port_forward = subprocess.Popen(
            kubectl("port-forward", "svc/golab-b", "38688:8688"),
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
        )
        wait_api()
        publish_next_revision(api="http://127.0.0.1:38688", owner=owner)
        out, err = watcher.communicate(timeout=125)
        if watcher.returncode or "PASS official SDK received revision" not in out:
            raise RuntimeError(f"official SDK failover push failed: {out.strip()} {err.strip()}")
        print(out.strip())
    finally:
        if watcher is not None and watcher.poll() is None:
            watcher.terminate()
            watcher.wait(timeout=10)
        if port_forward is not None and port_forward.poll() is None:
            port_forward.terminate()
            port_forward.wait(timeout=10)
        for name in ("a", "b"):
            subprocess.run(kubectl("apply", "-f", str(workdir / f"golab-{name}.yaml")),
                           capture_output=True, text=True, timeout=30)
        invoke("wait", "--for=condition=Ready", "pod/golab-a", "pod/golab-b", "--timeout=90s")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        sys.exit(1)
