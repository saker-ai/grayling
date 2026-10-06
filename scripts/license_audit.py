#!/usr/bin/env python3
"""Pin dependency license evidence and gate binary/source packaging."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "third_party" / "licenses.json"
NOTICES = ROOT / "THIRD_PARTY_NOTICES.md"
GONACOS = "github.com/godeps/gonacos"
GONACOS_VERSION = "v0.0.0-20261006082753-5ac0bf0b6806"


def sha(data):
    return hashlib.sha256(data).hexdigest()


def go_modules():
    go = shutil.which("go") or "/usr/local/go/bin/go"
    raw = subprocess.check_output([go, "list", "-m", "-json", "all"], cwd=ROOT, env={**os.environ, "GOWORK": "off"}).decode()
    decoder = json.JSONDecoder()
    while raw.strip():
        item, length = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[length:]
        if item["Path"] != "github.com/godeps/golab":
            yield item


def license_type(data):
    s = data[:2000].decode(errors="replace").lower()
    if "apache license" in s[:500] and "version 2.0" in s[:500]:
        return "Apache-2.0"
    if "permission is hereby granted, free of charge" in s:
        return "MIT"
    if "redistribution and use in source and binary forms" in s:
        return "BSD-3-Clause" if "neither the name" in s or "neither the copyright" in s else "BSD-2-Clause"
    return "REVIEW"


def runtime_modules():
    go = shutil.which("go") or "/usr/local/go/bin/go"
    output = subprocess.check_output([go, "list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}{{end}}", "./cmd/golab", "./cmd/golab-proxy"], cwd=ROOT, env={**os.environ, "GOWORK": "off"}).decode()
    return set(output.splitlines())


def inventory():
    runtime = runtime_modules()
    rows = []
    for mod in go_modules():
        actual = mod.get("Replace", mod)
        used = mod["Path"] in runtime
        directory = Path(mod["Dir"]) if used else None
        if used and directory is None:
            raise RuntimeError(f"runtime module missing source: {mod['Path']}")
        files = sorted(p for p in directory.iterdir() if p.is_file() and ("license" in p.name.lower() or "copying" in p.name.lower() or p.name.lower() == "notice")) if used else []
        evidence = [{"file": p.name, "sha256": sha(p.read_bytes())} for p in files]
        primary = next((p for p in files if "license" in p.name.lower() or "copying" in p.name.lower()), None)
        row = {"module": mod["Path"], "version": mod["Version"], "runtime": used, "license": (license_type(primary.read_bytes()) if primary else "UNRESOLVED") if used else "NOT_DISTRIBUTED", "evidence": evidence}
        if "Replace" in mod:
            row["resolved_module"] = actual["Path"]
            row["resolved_version"] = actual["Version"]
        rows.append(row)
    lock = json.loads((ROOT / "sdk/typescript/package-lock.json").read_text())
    npm = [{"package": name.removeprefix("node_modules/"), "version": value["version"], "license": value.get("license", "UNRESOLVED")}
           for name, value in sorted(lock["packages"].items()) if name]
    return {"schema": 1, "scope": "Go module graph (includes test/tool modules), plus TypeScript lockfile; Python SDK has no pinned runtime packages", "go": rows, "npm": npm}


def atomic_json(path, value):
    with tempfile.NamedTemporaryFile("w", dir=path.parent, prefix=".licenses-", delete=False) as f:
        json.dump(value, f, indent=2, ensure_ascii=False)
        f.write("\n")
        temp = f.name
    os.replace(temp, path)


def notice_text(current):
    lines = ["# Third-party notices", "", "Generated from `third_party/licenses.json` by `scripts/license_audit.py update`.", "The list covers modules linked by `cmd/golab` or `cmd/golab-proxy`; the JSON also records the complete selected Go module graph and the TypeScript lockfile.", "A release bundle includes the full license and NOTICE texts for every linked module.", "", "| Module | Version | License | Evidence (SHA-256) |", "| --- | --- | --- | --- |"]
    for row in current["go"]:
        if not row["runtime"]:
            continue
        evidence = "<br>".join(f"{entry['file']} `{entry['sha256']}`" for entry in row["evidence"]) or "**No license file; release blocked**"
        resolved = f" → {row['resolved_module']}@{row['resolved_version']}" if "resolved_module" in row else ""
        lines.append(f"| {row['module']}{resolved} | {row['version']} | {row['license']} | {evidence} |")
    lines += ["", "GoLab itself is Apache-2.0 under `LICENSE`. The pinned gonacos commit includes Apache-2.0 `LICENSE` and `NOTICE`; its [upstream declaration](https://github.com/godeps/gonacos/commit/5ac0bf0b6806) explicitly covers project-authored contributions in the earlier pinned commit.", ""]
    return "\n".join(lines)


def check(current, expected, release=False):
    if current != expected or NOTICES.read_text() != notice_text(current):
        raise RuntimeError("dependency versions, license evidence, or notices changed; inspect and run scripts/license_audit.py update, then review the diff")
    unresolved = [f"{r['module']}@{r['version']}" for r in current["go"] if r["runtime"] and r["license"] in ("UNRESOLVED", "REVIEW")]
    gonacos = next((r for r in current["go"] if r["module"] == GONACOS), None)
    if gonacos is None or gonacos["version"] != GONACOS_VERSION:
        raise RuntimeError("gonacos pin changed; review upstream license scope and update the pinned-version gate")
    if gonacos["license"] != "Apache-2.0" or not {"LICENSE", "NOTICE"}.issubset(
        {entry["file"] for entry in gonacos["evidence"]}
    ):
        raise RuntimeError("gonacos pin lacks the reviewed Apache-2.0 LICENSE and NOTICE evidence")
    if release and unresolved:
        raise RuntimeError("release blocked by unresolved license evidence: " + ", ".join(unresolved))
    print(f"license inventory matches: {len(current['go'])} Go modules, {len(current['npm'])} npm packages")


def bundle(current, target):
    check(current, inventory(), release=True)
    if target.exists():
        raise RuntimeError(f"output already exists: {target}")
    target.mkdir(parents=True)
    shutil.copy2(ROOT / "README.md", target / "README.md")
    shutil.copy2(ROOT / "LICENSE", target / "LICENSE")
    shutil.copy2(MANIFEST, target / "THIRD_PARTY_LICENSES.json")
    shutil.copy2(NOTICES, target / "THIRD_PARTY_NOTICES.md")
    notices = target / "third-party-notices"
    notices.mkdir()
    for index, (row, mod) in enumerate(zip(current["go"], go_modules()), 1):
        if not row["runtime"]:
            continue
        for evidence in row["evidence"]:
            src = Path(mod["Dir"]) / evidence["file"]
            dst = notices / f"{index:03d}-{evidence['file']}"
            shutil.copy2(src, dst)
    for name in ("golab", "golab-proxy"):
        shutil.copy2(ROOT / "bin" / name, target / name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("update", "check", "release-check", "bundle"))
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    current = inventory()
    if args.action == "update":
        atomic_json(MANIFEST, current)
        NOTICES.write_text(notice_text(current))
        print(f"wrote {MANIFEST} and {NOTICES}")
        return
    expected = json.loads(MANIFEST.read_text())
    if args.action == "bundle":
        if args.output is None:
            parser.error("bundle requires --output")
        bundle(expected, args.output)
    else:
        check(expected, current, release=args.action == "release-check")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.CalledProcessError) as error:
        print(f"license audit: {error}", file=sys.stderr)
        sys.exit(1)
