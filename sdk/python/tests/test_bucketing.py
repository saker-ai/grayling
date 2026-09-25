"""Cross-language bucketing contract: the Python implementation of
golab-fnv1a-10000-v1 must reproduce EVERY golden vector shared with the Go
reference (P4 退出验收: 跨语言用例同值)."""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from golab_sdk import bucketing  # noqa: E402

VECTORS = json.loads(
    (Path(__file__).resolve().parents[3] / "pkg" / "bucketing" / "testdata"
     / "golden_vectors.json").read_text())


def test_golden_vectors():
    assert VECTORS["algorithm"] == "golab-fnv1a-10000-v1"
    assert VECTORS["modulus"] == 10000
    assert len(VECTORS["vectors"]) >= 20
    for v in VECTORS["vectors"]:
        got_hash = bucketing.hash64_hex(
            v["workspace"], v["environment"], v["key"], v["epoch"],
            v["salt"], v["unitValue"], v["purpose"])
        assert got_hash == v["hash64Hex"], f"hash mismatch: {v['desc']}"
        got_bucket = bucketing.bucket(
            v["workspace"], v["environment"], v["key"], v["epoch"],
            v["salt"], v["unitValue"], v["purpose"])
        assert got_bucket == v["bucket"], f"bucket mismatch: {v['desc']}"


def test_purpose_coordinates_are_disjoint():
    # eligibility/variant/layer purposes must not collide for equal inputs.
    coords = {
        bucketing.bucket("ws", "prod", "flag", 1, "salt", "user-1", p)
        for p in ("eligibility", "variant", "layer")
    }
    assert len(coords) == 3


def test_input_validation():
    import pytest
    with pytest.raises(ValueError):
        bucketing.bucket("ws", "prod", "flag", -1, "salt", "u", "variant")
    with pytest.raises(ValueError):
        bucketing.bucket("ws", "prod", "flag", 0, "salt", "u", "bad:purpose")
