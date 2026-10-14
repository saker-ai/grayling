"""grayling-fnv1a-10000-v1 — the Grayling bucketing contract in Python.

This module exists to prove cross-language determinism (P4 退出验收: 跨语言
用例同值): it is validated against the SAME golden vectors as the Go
reference implementation (pkg/bucketing/testdata/golden_vectors.json).

The remote-evaluation client does NOT bucket locally — assignments come from
the server. Use this module when you need to verify/replicate an assignment
offline (analysis, migrations, tests).
"""

from __future__ import annotations

ALGORITHM_ID = "grayling-fnv1a-10000-v1"
BUCKETS = 10000

_FNV_OFFSET = 0xCBF29CE484222325
_FNV_PRIME = 0x100000001B3
_MASK64 = (1 << 64) - 1

PURPOSE_ELIGIBILITY = "eligibility"
PURPOSE_VARIANT = "variant"
PURPOSE_LAYER = "layer"


def _netstring(value: str) -> bytes:
    raw = value.encode("utf-8")
    return str(len(raw)).encode("ascii") + b":" + raw


def fnv1a64(data: bytes) -> int:
    h = _FNV_OFFSET
    for b in data:
        h ^= b
        h = (h * _FNV_PRIME) & _MASK64
    return h


def bucket_input(workspace: str, environment: str, key: str, epoch: int,
                 salt: str, unit_value: str, purpose: str) -> bytes:
    """Canonical hash input: length-prefixed (netstring) fields in fixed
    order, epoch as ASCII decimal."""
    if ":" in purpose:
        raise ValueError("purpose must not contain ':'")
    if epoch < 0:
        raise ValueError("epoch must be non-negative")
    parts = [workspace, environment, key, str(epoch), salt, unit_value, purpose]
    return b"".join(_netstring(p) for p in parts)


def bucket(workspace: str, environment: str, key: str, epoch: int, salt: str,
           unit_value: str, purpose: str) -> int:
    """Stable bucket in [0, 10000) for one hash coordinate."""
    data = bucket_input(workspace, environment, key, epoch, salt, unit_value,
                        purpose)
    return fnv1a64(data) % BUCKETS


def hash64_hex(workspace: str, environment: str, key: str, epoch: int,
               salt: str, unit_value: str, purpose: str) -> str:
    data = bucket_input(workspace, environment, key, epoch, salt, unit_value,
                        purpose)
    return format(fnv1a64(data), "016x")
