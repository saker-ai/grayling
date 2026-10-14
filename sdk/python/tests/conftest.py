"""Integration fixtures: every test in this suite runs against a REAL
grayling binary (built from this repo with GOWORK=off) speaking real HTTP —
the same startup contract as test/boot (owner token scraped from stderr,
free ports, health poll). Nothing is mocked."""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
SDK_ROOT = REPO_ROOT / "sdk" / "python"

# Make `import grayling_sdk` work without installation.
sys.path.insert(0, str(SDK_ROOT))


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _gonacos_root() -> str:
    out = subprocess.run(
        ["go", "list", "-m", "-f", "{{.Dir}}", "github.com/godeps/gonacos"],
        cwd=REPO_ROOT, env={**os.environ, "GOWORK": "off"},
        capture_output=True, text=True, check=True)
    root = out.stdout.strip()
    assert (Path(root) / "api" / "openapi" / "upstream").is_dir(), root
    return root


class GraylingServer:
    """Handle for the running grayling process + admin conveniences."""

    def __init__(self, base_url: str, owner_token: str, proc: subprocess.Popen):
        self.base_url = base_url
        self.owner_token = owner_token
        self.reviewer_token = ""
        self.proc = proc

    def request(self, method: str, path: str, token: str | None = None,
                body=None, expect: int | None = None):
        data = None
        headers = {"Accept": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base_url + path, data=data,
                                     headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                raw = resp.read()
                status = resp.status
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            status = exc.code
        payload = json.loads(raw) if raw else None
        if expect is not None:
            assert status == expect, f"{method} {path} → {status}: {raw[:400]!r}"
        return status, payload

    # ---- admin workflow helpers ----

    def create_workspace(self, slug: str) -> dict:
        _, ws = self.request("POST", "/api/v1/admin/workspaces",
                             self.owner_token, {"slug": slug, "name": slug},
                             expect=201)
        return ws

    def create_environment(self, ws: str, slug: str) -> dict:
        _, env = self.request(
            "POST", f"/api/v1/admin/workspaces/{ws}/environments",
            self.owner_token, {"slug": slug, "name": slug}, expect=201)
        return env

    def create_token(self, ws: str, name: str, kind: str, *,
                     role: str | None = None, environment: str | None = None) -> str:
        body: dict = {"name": name, "kind": kind}
        if role:
            body["role"] = role
        if environment:
            body["environment"] = environment
        _, ct = self.request("POST", f"/api/v1/admin/workspaces/{ws}/tokens",
                             self.owner_token, body, expect=201)
        # platform.CreatedToken has no json tags → Go field names.
        return ct["Secret"]

    def publish_flag(self, ws: str, env: str, content: dict, *,
                     approver: str | None = None, risk: str = "normal") -> dict:
        base = f"/api/v1/admin/workspaces/{ws}/environments/{env}/flags/{content['key']}"
        _, current = self.request("GET", base, self.owner_token, expect=200)
        self.request("PUT", base + "/draft", self.owner_token,
                     {"content": content, "risk": risk,
                      "baseRevision": current["currentRevision"],
                      "expectedDraftHash": current["draftHash"]}, expect=200)
        self.request("POST", base + "/submit", self.owner_token,
                     {"reason": "Python SDK integration"}, expect=200)
        self.request("POST", base + "/approve", approver or self.reviewer_token,
                     {"note": "ok"}, expect=200)
        _, res = self.request("POST", base + "/publish", self.owner_token,
                              {"note": "ship"}, expect=200)
        return res


@pytest.fixture(scope="session")
def server():
    if shutil.which("go") is None:
        pytest.fail("go toolchain is required for SDK integration tests")
    tmp = Path(tempfile.mkdtemp(prefix="grayling-pysdk-"))
    binary = tmp / "grayling"
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/grayling"],
        cwd=REPO_ROOT, env={**os.environ, "GOWORK": "off"},
        capture_output=True, text=True)
    assert build.returncode == 0, f"build failed:\n{build.stderr}"

    api_port, nap_port, grpc_port = _free_port(), _free_port(), _free_port()
    proc = subprocess.Popen(
        [str(binary),
         "-addr", f"127.0.0.1:{api_port}",
         "-data", str(tmp / "store.json"),
         "-gonacos-root", _gonacos_root(),
         "-gonacos-data", str(tmp / "gonacos"),
         "-gonacos-addr", f"127.0.0.1:{nap_port}",
         "-gonacos-grpc-addr", f"127.0.0.1:{grpc_port}"],
        stderr=subprocess.PIPE, text=True)

    owner_token = ""
    deadline = time.time() + 30
    while time.time() < deadline and not owner_token:
        line = proc.stderr.readline()
        if not line:
            if proc.poll() is not None:
                pytest.fail(f"grayling exited early: {proc.poll()}")
            continue
        if "grayling_admin_" in line:
            idx = line.index("grayling_admin_")
            owner_token = line[idx:].split()[0].strip()
    assert owner_token, "bootstrap owner token not printed"

    base = f"http://127.0.0.1:{api_port}"
    healthy = False
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(base + "/api/v1/health", timeout=2) as r:
                if r.status == 200:
                    healthy = True
                    break
        except OSError:
            time.sleep(0.2)
    assert healthy, "server never became healthy"

    srv = GraylingServer(base, owner_token, proc)
    yield srv

    proc.terminate()
    try:
        proc.wait(timeout=15)
    except subprocess.TimeoutExpired:
        proc.kill()
    shutil.rmtree(tmp, ignore_errors=True)


class Env:
    """A prepared workspace/environment with tokens and published flags."""

    def __init__(self, server: GraylingServer):
        self.server = server
        self.ws = "pyws"
        self.env = "prod"
        server.create_workspace(self.ws)
        server.create_environment(self.ws, self.env)
        self.sdk_token = server.create_token(self.ws, "py-sdk", "sdk",
                                             environment=self.env)
        self.collect_token = server.create_token(self.ws, "py-collect",
                                                 "collect", environment=self.env)
        self.publisher_token = server.create_token(self.ws, "py-publisher",
                                                   "admin", role="publisher")
        server.reviewer_token = self.publisher_token

        # Bool flag: tenant=acme → on; default off.
        server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-bool", "type": "bool",
            "fields": ["tenant"],
            "variations": {"on": True, "off": False},
            "defaultRule": {"variation": "off"},
            "targeting": [{
                "name": "acme",
                "constraints": [{"field": "tenant", "op": "eq", "value": "acme"}],
                "variation": "on",
            }],
        })
        # String / number / json flags.
        server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-string", "type": "string",
            "variations": {"a": "alpha", "b": "beta"},
            "defaultRule": {"variation": "a"},
        })
        server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-number", "type": "number",
            "variations": {"low": 1, "high": 42.5},
            "defaultRule": {"variation": "high"},
        })
        server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-json", "type": "json",
            "variations": {"cfg": {"model": "qwen-max", "temperature": 0.2}},
            "defaultRule": {"variation": "cfg"},
        })
        # Experiment flag: 50/50 split, registered + bound + running.
        server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-exp", "type": "string",
            "fields": ["user_id"],
            "bucketing": {"algorithm": "grayling-fnv1a-10000-v1",
                          "unit": "user_id", "salt": "py-exp-salt"},
            "variations": {"control": "c", "treatment": "t"},
            "defaultRule": {"variation": "control"},
            "targeting": [{"name": "split",
                           "percentage": {"control": 50, "treatment": 50}}],
        })
        _, exp = server.request(
            "POST",
            f"/api/v1/admin/workspaces/{self.ws}/environments/{self.env}/experiments",
            server.owner_token, {
                "flagKey": "py-exp", "unit": "user_id",
                "variants": ["control", "treatment"],
                "proportions": {"control": 50, "treatment": 50},
                "primaryMetric": {"key": "conversion", "type": "proportion",
                                  "direction": "increase"},
                "alpha": 0.05, "power": 0.8, "mde": 0.02,
                "minSamplePerArm": 5, "minRuntime": 0,
                "analysisWindow": 7 * 24 * 3600 * 10**9,
                "hypothesis": "treatment lifts conversion",
            }, expect=201)
        self.experiment_id = exp["id"]
        self.epoch = exp["epoch"]
        server.request("POST",
                       f"/api/v1/admin/experiments/{self.experiment_id}/review",
                       server.owner_token, {"note": "pre-registered"},
                       expect=200)
        server.request("POST",
                       f"/api/v1/admin/experiments/{self.experiment_id}/bind",
                       server.owner_token, None, expect=200)
        # High-risk binding draft: second actor approves (二人规则).
        server.request(
            "POST",
            f"/api/v1/admin/workspaces/{self.ws}/environments/{self.env}"
            f"/flags/py-exp/approve",
            self.publisher_token, {"note": "second pair of eyes"}, expect=200)
        server.request(
            "POST",
            f"/api/v1/admin/workspaces/{self.ws}/environments/{self.env}"
            f"/flags/py-exp/publish",
            server.owner_token, {"note": "bind"}, expect=200)
        server.request("POST",
                       f"/api/v1/admin/experiments/{self.experiment_id}/transition",
                       server.owner_token,
                       {"to": "running", "reason": "launch"}, expect=200)

    def report(self) -> dict:
        _, rep = self.server.request(
            "GET", f"/api/v1/admin/experiments/{self.experiment_id}/report",
            self.server.owner_token, None, expect=200)
        return rep

    def republish_bool(self, default_variation: str) -> dict:
        """Publish a new revision of py-bool flipping the default rule —
        used to prove cache invalidation follows the substrate."""
        return self.server.publish_flag(self.ws, self.env, {
            "schemaVersion": 1, "key": "py-bool", "type": "bool",
            "fields": ["tenant"],
            "variations": {"on": True, "off": False},
            "defaultRule": {"variation": default_variation},
            "targeting": [{
                "name": "acme",
                "constraints": [{"field": "tenant", "op": "eq", "value": "acme"}],
                "variation": "on",
            }],
        })


@pytest.fixture(scope="session")
def env(server) -> Env:
    return Env(server)
