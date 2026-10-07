#!/usr/bin/env python3
"""Provision disposable tenants for the isolated native Nacos Kind topology."""

import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request


API = "http://127.0.0.1:28688"
NAMESPACE = "golab-issue13"


def owner_token():
    for pod in ("golab-a", "golab-b"):
        result = subprocess.run(
            ["kubectl", "-n", NAMESPACE, "logs", pod, "-c", "golab"],
            check=True, capture_output=True, text=True,
        )
        for line in result.stdout.splitlines():
            for word in line.split():
                if word.startswith("glab_admin_"):
                    return word
    raise RuntimeError("bootstrap owner token was not found in test Pod logs")


def request(method, path, token, body=None, want=200):
    data = None if body is None else json.dumps(body).encode()
    headers = {"Authorization": "Bearer " + token}
    if data is not None:
        headers["Content-Type"] = "application/json"
    for attempt in range(5):
        call = urllib.request.Request(API + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(call, timeout=10) as response:
                status = response.status
                payload = response.read()
        except urllib.error.HTTPError as error:
            status = error.code
            payload = error.read()
        if status == 409 and attempt < 4:
            time.sleep(0.1)
            continue
        if status != want:
            raise RuntimeError(f"{method} {path} returned HTTP {status}; expected {want}")
        return json.loads(payload) if payload else {}
    raise RuntimeError(f"{method} {path} did not complete")


def write_secret(path, value):
    path.write_text(value + "\n")
    path.chmod(0o600)


def main():
    workdir = Path(os.environ["ISSUE13_WORKDIR"])
    workdir.mkdir(mode=0o700, parents=True, exist_ok=True)
    owner = owner_token()
    for slug in ("tenant-a", "tenant-b"):
        request("POST", "/api/v1/admin/workspaces", owner,
                {"slug": slug, "name": slug}, 201)
        environment = request("POST", f"/api/v1/admin/workspaces/{slug}/environments",
                              owner, {"slug": "prod", "name": "Production"}, 201)
        reviewer = request("POST", f"/api/v1/admin/workspaces/{slug}/tokens",
                           owner, {"name": "reviewer", "kind": "admin", "role": "publisher"}, 201)
        key = "sample"
        base = f"/api/v1/admin/workspaces/{slug}/environments/prod/flags/{key}"
        flag = {"schemaVersion": 1, "key": key, "type": "string",
                "variations": {"on": slug}, "defaultRule": {"variation": "on"}}
        request("PUT", base + "/draft", owner, {"content": flag, "risk": "normal"})
        request("POST", base + "/submit", owner, {"reason": "Kind native isolation"})
        request("POST", base + "/approve", reviewer["Secret"], {"note": "ok"})
        request("POST", base + "/publish", owner, {"note": "ship"})
        sdk = request("POST", f"/api/v1/admin/workspaces/{slug}/tokens", owner,
                      {"name": "kind-sdk", "kind": "sdk", "environment": "prod"}, 201)
        write_secret(workdir / f"{slug}-token", sdk["Secret"])
        write_secret(workdir / f"{slug}-namespace", environment["namespaceId"])
        print(f"{slug}: published and SDK token prepared")


if __name__ == "__main__":
    try:
        main()
    except (KeyError, OSError, RuntimeError, ValueError) as exc:
        print(f"provision failed: {exc}", file=sys.stderr)
        sys.exit(1)
