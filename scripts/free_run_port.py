#!/usr/bin/env python3
"""Release the development server's TCP port before make run starts."""

import argparse
import os
import re
import shutil
import signal
import subprocess
import time


def listener_pids(port):
    if shutil.which("lsof"):
        result = subprocess.run(
            ["lsof", "-nP", f"-tiTCP:{port}", "-sTCP:LISTEN"],
            capture_output=True, text=True, check=False,
        )
        if result.returncode not in (0, 1) or result.stderr.strip():
            raise RuntimeError(result.stderr.strip() or "Cannot inspect listeners")
        return {int(pid) for pid in result.stdout.split()}
    if shutil.which("ss"):
        result = subprocess.run(
            ["ss", "-H", "-ltnp", f"sport = :{port}"],
            capture_output=True, text=True, check=True,
        )
        pids = {int(pid) for pid in re.findall(r"pid=(\d+)", result.stdout)}
        if result.stdout.strip() and not pids:
            raise RuntimeError("Cannot identify listener PID; check process permissions")
        return pids
    raise RuntimeError("Install lsof or ss to automatically release the run port")


def stop_listeners(port, timeout=5):
    original = listener_pids(port)
    if not original:
        return
    if os.getpid() in original or os.getppid() in original:
        raise RuntimeError("Cannot stop the port cleanup process or its parent")
    print(f"Port {port} is occupied; stopping listener PID(s): {sorted(original)}", flush=True)

    def send(pids, sig):
        for pid in pids:
            try:
                os.kill(pid, sig)
            except ProcessLookupError:
                pass

    send(original, signal.SIGTERM)
    deadline = time.monotonic() + timeout
    remaining = original & listener_pids(port)
    while remaining and time.monotonic() < deadline:
        time.sleep(0.1)
        remaining = original & listener_pids(port)
    if remaining:
        print(f"Listener did not stop; forcing PID(s): {sorted(remaining)}", flush=True)
        send(remaining, signal.SIGKILL)
        deadline = time.monotonic() + 2
        while listener_pids(port) and time.monotonic() < deadline:
            time.sleep(0.1)
    if listener_pids(port):
        raise RuntimeError(f"Port {port} is still occupied; another listener may have restarted")
    print(f"Port {port} is free.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("port", type=int)
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("port must be between 1 and 65535")
    try:
        stop_listeners(args.port)
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        parser.exit(1, f"Cannot release port {args.port}: {error}\n")


if __name__ == "__main__":
    main()
