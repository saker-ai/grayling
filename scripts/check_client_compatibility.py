#!/usr/bin/env python3
"""Run the pinned, real-client compatibility matrix and publish a CI summary."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
MATRIX = ROOT / "docs/client-compatibility-matrix.json"


def module_version(go_mod, module):
    match = re.search(r"^\s*" + re.escape(module) + r"\s+(v\S+)", go_mod, re.MULTILINE)
    return match.group(1) if match else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tier", choices=("local", "cluster", "all"), default="all")
    args = parser.parse_args()
    matrix = json.loads(MATRIX.read_text())
    go_mod = (ROOT / "go.mod").read_text()
    pins = (
        ("nacosSDK", "nacosVersion"),
        ("gonacos", "gonacosVersion"),
        ("openFeatureSDK", "openFeatureSDKVersion"),
    )
    mismatches = [
        f"{matrix[module]}: go.mod={module_version(go_mod, matrix[module])}, matrix={matrix[version]}"
        for module, version in pins
        if module_version(go_mod, matrix[module]) != matrix[version]
    ]
    rows = []
    if mismatches:
        rows.append("| dependency pins | FAIL | " + "; ".join(mismatches) + " |")
    else:
        rows.append("| dependency pins | PASS | SDK, gonacos and OpenFeature versions match go.mod |")
        for case in matrix["cases"]:
            if args.tier != "all" and case["tier"] != args.tier:
                continue
            expected = set(case["tests"])
            pattern = "^(?:" + "|".join(re.escape(name) for name in case["tests"]) + ")$"
            cmd = ["go", "test", "-json", case["package"], "-run", pattern, "-count=1", "-timeout=5m"]
            env = dict(os.environ, GOWORK="off")
            print("Running", " ".join(cmd), flush=True)
            result = subprocess.run(cmd, cwd=ROOT, env=env, capture_output=True, text=True)
            passed = set()
            skipped = set()
            failed = set()
            test_output = {name: [] for name in expected}
            for line in result.stdout.splitlines():
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    continue
                name = event.get("Test", "")
                if name in expected and "/" not in name:
                    if event.get("Action") == "output":
                        test_output[name].append(event.get("Output", ""))
                    if event.get("Action") == "pass":
                        passed.add(name)
                    elif event.get("Action") == "skip":
                        skipped.add(name)
                    elif event.get("Action") == "fail":
                        failed.add(name)
            missing = expected - passed - skipped - failed
            if result.returncode or failed or skipped or missing:
                status = "FAIL" if result.returncode or failed else "INCOMPLETE"
                for name in sorted(failed):
                    print(f"{name}:\n{''.join(test_output[name])[-3000:]}", file=sys.stderr)
                if result.returncode and not failed:
                    print(result.stdout[-3000:], file=sys.stderr)
                if result.stderr:
                    print(result.stderr[-3000:], file=sys.stderr)
            else:
                status = "PASS"
            detail = f"{len(passed)}/{len(expected)} passed; {case['topology']}"
            if skipped or missing or failed:
                detail += "; " + "; ".join(
                    f"{label}: {', '.join(sorted(names))}"
                    for label, names in (("skipped", skipped), ("missing", missing), ("failed", failed)) if names
                )
            rows.append(f"| {case['id']} | {status} | {detail} |")
    for limitation in matrix["knownLimitations"]:
        rows.append(f"| known limitation | LIMIT | {limitation} |")
    summary = "## Client compatibility matrix\n\n| Case | Result | Evidence / limit |\n| --- | --- | --- |\n" + "\n".join(rows) + "\n"
    print(summary)
    if target := os.getenv("GITHUB_STEP_SUMMARY"):
        with open(target, "a", encoding="utf-8") as output:
            output.write(summary)
    return int(bool(mismatches or any("| FAIL |" in row or "| INCOMPLETE |" in row for row in rows)))


if __name__ == "__main__":
    sys.exit(main())
