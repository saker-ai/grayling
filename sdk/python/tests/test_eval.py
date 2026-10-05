"""Remote-evaluation contract: typed variations, targeting, caller-default
semantics on error/type mismatch, experiment attribution and server-side
exposure capture — all against the real binary."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from golab_sdk import GolabClient, GolabError, Options  # noqa: E402


def make_client(env, **overrides) -> GolabClient:
    kwargs = dict(
        base_url=env.server.base_url,
        sdk_token=env.sdk_token,
        collect_token=env.collect_token,
        poll_interval=3600,  # tests drive freshness explicitly
        auto_poll=False,
    )
    kwargs.update(overrides)
    return GolabClient(Options(**kwargs))


def test_bool_variation_targeting(env):
    with make_client(env) as client:
        # Targeting rule: tenant=acme → on.
        value, details = client.bool_variation(
            "py-bool", False, attributes={"tenant": "acme"})
        assert value is True
        assert details.result.reason == "TARGETING_MATCH"
        assert details.result.failed is False
        assert details.manifest_revision > 0

        # Scoped diagnosis uses the same semantic fields as the Go SDK.
        diag = client.diagnose(details.result.decision_id)
        assert diag["result"]["rule"] == details.result.rule
        assert diag["result"]["reason"] == details.result.reason
        assert diag["result"]["manifestRevision"] == details.manifest_revision
        assert diag["exposureStatus"] == "none"
        assert "user_id" not in str(diag)
        status, _ = env.server.request(
            "GET", f"/api/v1/sdk/decisions/{details.result.decision_id}",
            env.collect_token)
        assert status == 403
        env.server.create_workspace("diag-other")
        env.server.create_environment("diag-other", "prod")
        other_token = env.server.create_token("diag-other", "diag-sdk", "sdk",
                                              environment="prod")
        status, _ = env.server.request(
            "GET", f"/api/v1/sdk/decisions/{details.result.decision_id}",
            other_token)
        assert status == 404

        # Default rule for everyone else.
        value, details = client.bool_variation(
            "py-bool", False, attributes={"tenant": "other"})
        assert value is False
        assert details.result.reason == "DEFAULT"


def test_string_number_json_variations(env):
    with make_client(env) as client:
        s, d = client.string_variation("py-string", "fallback")
        assert s == "alpha" and d.result.reason == "DEFAULT"

        n, d = client.number_variation("py-number", 0.0)
        assert n == 42.5 and not d.result.failed

        j, d = client.json_variation("py-json", {})
        assert j == {"model": "qwen-max", "temperature": 0.2}


def test_unknown_flag_returns_caller_default(env):
    with make_client(env) as client:
        value, details = client.bool_variation("no-such-flag", True)
        assert value is True  # caller default, never a guess
        assert details.result.failed is True
        assert details.result.error_code == "FLAG_NOT_FOUND"


def test_type_mismatch_returns_caller_default(env):
    with make_client(env) as client:
        # py-string evaluated through the bool API → caller default +
        # TYPE_MISMATCH, mirroring the Go SDK's typed-variation semantics.
        value, details = client.bool_variation("py-string", True)
        assert value is True
        assert details.result.failed is True
        assert details.result.error_code == "TYPE_MISMATCH"
        assert details.result.reason == "ERROR"
        assert details.result.variation_type == ""


def test_transport_error_returns_caller_default(env):
    dead = Options(base_url="http://127.0.0.1:1", sdk_token="glab_sdk_x",
                   auto_poll=False)
    with GolabClient(dead) as client:
        value, details = client.bool_variation("py-bool", False)
        assert value is False
        assert details.result.failed is True
        assert details.result.reason == "ERROR"
        assert client.status()["evalErrors"] == 1
        assert client.status()["state"] == "ERROR"


def test_remote_status_expiry_and_recovery(env):
    with make_client(env, max_staleness=0.02) as client:
        assert client.status()["state"] == "NOT_READY"
        _, details = client.string_variation("py-string", "?")
        assert details.manifest_revision > 0
        assert client.status()["state"] == "READY"
        import time
        time.sleep(0.04)
        assert client.status()["state"] == "STALE"
        client.sync_revision()
        assert client.status()["state"] == "READY"


def test_unauthorized_token_never_crashes(env):
    bad = Options(base_url=env.server.base_url, sdk_token="glab_sdk_invalid",
                  auto_poll=False)
    with GolabClient(bad) as client:
        value, details = client.string_variation("py-string", "safe")
        assert value == "safe"
        assert details.result.failed is True
        assert details.result.reason == "ERROR"


def test_wrong_audience_rejected(env):
    # The collect token is a different audience: sdk endpoints must reject it
    # and the SDK must fall back to the caller default (§9 三受众凭证分离).
    opts = Options(base_url=env.server.base_url, sdk_token=env.collect_token,
                   auto_poll=False)
    with GolabClient(opts) as client:
        value, details = client.string_variation("py-string", "safe")
        assert value == "safe"
        assert details.result.failed is True


def test_experiment_enrollment_and_server_exposure(env):
    with make_client(env) as client:
        units = {}
        for i in range(60):
            unit = f"py-user-{i}"
            value, details = client.string_variation(
                "py-exp", "?", attributes={"user_id": unit})
            assert value in ("c", "t")
            res = details.result
            assert res.experiment is not None, f"{unit} should be enrolled"
            assert res.experiment.experiment_id == env.experiment_id
            assert res.experiment.epoch == env.epoch
            assert res.experiment.source == "split"
            assert res.track_events is True
            # The SERVER recorded the exposure itself; the deterministic
            # event ID is echoed for observability.
            assert details.exposure_event_id.startswith("srv-")
            assert details.randomization_unit == unit
            units[unit] = value

        # Deterministic assignment: re-evaluating the same unit gives the
        # same variant (fixed-bucket contract, §6).
        for unit, value in list(units.items())[:10]:
            again, _ = client.string_variation(
                "py-exp", "?", attributes={"user_id": unit})
            assert again == value

        # Missing unit → MISSING_BUCKET_KEY + caller default, never random.
        value, details = client.string_variation("py-exp", "safe-default")
        assert value == "safe-default"
        assert details.result.error_code == "MISSING_BUCKET_KEY"
        assert details.result.experiment is None

        # Server-side exposures are visible in the analysis report.
        rep = env.report()
        total = sum(v["randomizedUnits"] for v in rep["perVariant"])
        assert total >= 50, rep["perVariant"]


def test_track_conversion_requires_attribution(env):
    with make_client(env) as client:
        _, details = client.string_variation("py-string", "?")
        try:
            client.track_conversion(details, "conversion", 1.0)
            raise AssertionError("conversion without attribution must raise")
        except GolabError:
            pass
    # No collect token configured → explicit error.
    opts = Options(base_url=env.server.base_url, sdk_token=env.sdk_token,
                   auto_poll=False)
    with GolabClient(opts) as client:
        _, details = client.string_variation(
            "py-exp", "?", attributes={"user_id": "conv-user-1"})
        try:
            client.track_conversion(details, "conversion", 1.0)
            raise AssertionError("conversion without collect token must raise")
        except GolabError:
            pass
