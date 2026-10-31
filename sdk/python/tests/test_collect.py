"""Collect-contract alignment: batched conversion/exposure reporting from
the SDK, server-side dedup by eventId, unit-required rejection and epoch
mismatch rejection — verified through the real /api/v1/collect endpoint."""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
import uuid

import pytest

from test_eval import make_client


def raw_collect(env, events: list[dict], token: str | None = None) -> dict:
    req = urllib.request.Request(
        env.server.base_url + "/api/v1/collect",
        data=json.dumps({"events": events}).encode(),
        headers={
            "Authorization": f"Bearer {token or env.collect_token}",
            "Content-Type": "application/json",
        }, method="POST")
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read())


def _now_rfc3339() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def test_track_conversion_reaches_report(env):
    errors = []

    def record_error(phase, exc):
        # Never print exception text, HTTP bodies, tokens or evaluation context.
        # The SDK wraps HTTPError; keep only transport class and status.
        cause = exc.__cause__
        errors.append({"phase": phase, "type": type(exc).__name__,
                       "cause": type(cause).__name__ if cause else None,
                       "httpStatus": cause.code if isinstance(
                           cause, urllib.error.HTTPError) else None})

    with make_client(env, exposure_flush_interval=3600,
                     on_error=record_error) as client:
        converted = 0
        converted_details = None
        for i in range(30):
            unit = f"conv-user-{i}"
            _, details = client.string_variation(
                "py-exp", "?", attributes={"user_id": unit})
            assert details.result.experiment is not None, {
                "failed": details.result.failed,
                "transportErrors": errors,
                "serverExit": env.server.proc.poll(),
            }
            if details.result.experiment.variant == "treatment":
                client.track_conversion(details, "conversion", 1.0,
                                        session_id=f"sess-{unit}")
                converted += 1
                decision = details.result.decision_id
                converted_details = details
        assert converted > 0, "30 units at 50/50 must hit treatment"
        n = client.flush()
        assert n >= converted
        diag = client.diagnose(decision)
        assert diag["exposureStatus"] == "recorded"
        assert diag["conversionCount"] >= 1
        before = diag["conversionCount"]
        exp = converted_details.result.experiment
        raw_collect(env, [{
            "eventId": f"forged-{uuid.uuid4().hex}",
            "decisionId": decision, "type": "conversion",
            "experimentId": exp.experiment_id, "epoch": exp.epoch,
            "flagKey": "py-exp", "variant": exp.variant,
            "unit": "different-unit", "metricKey": "conversion", "value": 1,
            "clientTime": _now_rfc3339(),
        }])
        assert client.diagnose(decision)["conversionCount"] == before
        _, metrics = env.server.request("GET", "/api/v1/sdk/metrics",
                                        env.sdk_token, expect=200)
        assert metrics["exposures"] >= converted
        assert metrics["conversions"] >= converted
        assert set(metrics) == {"exposures", "conversions"}

        rep = env.report()
        treat = next(v for v in rep["perVariant"] if v["variant"] == "treatment")
        assert treat["convertedUnits"] >= converted


def test_collect_dedups_by_event_id(env):
    event_id = f"py-dedup-{uuid.uuid4().hex}"
    event = {
        "eventId": event_id, "type": "exposure",
        "experimentId": env.experiment_id, "epoch": env.epoch,
        "flagKey": "py-exp", "variant": "control",
        "unit": "dedup-user", "source": "split",
        "clientTime": _now_rfc3339(),
    }
    first = raw_collect(env, [event])
    assert first["accepted"] == 1 and first["duplicates"] == 0
    second = raw_collect(env, [event, dict(event, eventId=event_id + "-x")])
    assert second["accepted"] == 1
    assert second["duplicates"] == 1
    assert [item["status"] for item in second["results"]] == ["duplicate", "accepted"]


def test_collect_rejects_missing_unit_and_bad_epoch(env):
    res = raw_collect(env, [
        {  # missing unit → rejected (no attribution without a unit)
            "eventId": f"py-nounit-{uuid.uuid4().hex}", "type": "exposure",
            "experimentId": env.experiment_id, "epoch": env.epoch,
            "variant": "control", "clientTime": _now_rfc3339(),
        },
        {  # wrong epoch → rejected (mixed epochs are forbidden, §10.1)
            "eventId": f"py-epoch-{uuid.uuid4().hex}", "type": "exposure",
            "experimentId": env.experiment_id, "epoch": env.epoch + 99,
            "variant": "control", "unit": "epoch-user", "source": "split",
            "clientTime": _now_rfc3339(),
        },
    ])
    assert res["accepted"] == 0
    assert res["rejected"] == 2
    assert res["rejectMessages"]
    assert [item["code"] for item in res["results"]] == ["missing_unit", "stale_epoch"]


def test_partial_collect_counts_success_and_rejection(env):
    with make_client(env, exposure_flush_interval=3600) as client:
        client.track_exposure(env.experiment_id, env.epoch, "py-exp", "control", "partial-ok")
        client.track_exposure(env.experiment_id, env.epoch + 99, "py-exp", "control", "partial-bad")
        with pytest.raises(Exception):
            client.flush()
        status = client.status()
        assert status["eventsFlushed"] == 1
        assert status["eventsDropped"] == 1


def test_manual_exposure_reporting(env):
    with make_client(env, exposure_flush_interval=3600) as client:
        client.track_exposure(env.experiment_id, env.epoch, "py-exp",
                            "control", "manual-user-1", source="split")
        assert client.flush() == 1
        st = client.status()
        assert st["eventsFlushed"] >= 1 and st["eventsDropped"] == 0
        # Manual exposure without a unit is refused client-side already.
        with pytest.raises(Exception):
            client.track_exposure(env.experiment_id, env.epoch, "py-exp",
                                  "control", "")


def test_background_conversion_and_manual_exposure_delivery(env):
    with make_client(env, exposure_flush_interval=0.2) as client:
        unit = f"background-conv-{uuid.uuid4().hex}"
        _, details = client.string_variation(
            "py-exp", "?", attributes={"user_id": unit})
        assert details.result.experiment is not None
        decision = details.result.decision_id
        assert client.diagnose(decision)["conversionCount"] == 0
        before = {v["variant"]: v for v in env.report()["perVariant"]}

        client.track_conversion(details, "conversion", 1.0,
                                session_id=f"sess-{unit}")
        client.track_exposure(env.experiment_id, env.epoch, "py-exp",
                              "control", f"background-manual-{uuid.uuid4().hex}",
                              source="split")

        # Only the background worker delivers these events. Its cumulative
        # counters include batches that an explicit flush can no longer drain.
        deadline = time.monotonic() + 10
        while True:
            status = client.status()
            assert status["eventsDropped"] == 0, status
            if status["eventsFlushed"] >= 2 or time.monotonic() >= deadline:
                break
            time.sleep(0.05)
        assert status["eventsFlushed"] == 2, status

        diag = client.diagnose(decision)
        assert diag["exposureStatus"] == "recorded"
        assert diag["conversionCount"] == 1
        after = {v["variant"]: v for v in env.report()["perVariant"]}
        variant = details.result.experiment.variant
        assert after[variant]["convertedUnits"] == before[variant]["convertedUnits"] + 1
        assert after["control"]["randomizedUnits"] == before["control"]["randomizedUnits"] + 1
        assert client.flush() == 0
        status = client.status()
        assert status["eventsFlushed"] == 2 and status["eventsDropped"] == 0


def test_collect_requires_collect_audience(env):
    # The SDK token is the wrong audience for /collect (403).
    req = urllib.request.Request(
        env.server.base_url + "/api/v1/collect",
        data=json.dumps({"events": []}).encode(),
        headers={"Authorization": f"Bearer {env.sdk_token}",
                 "Content-Type": "application/json"}, method="POST")
    with pytest.raises(urllib.error.HTTPError) as ei:
        urllib.request.urlopen(req, timeout=10)
    assert ei.value.code == 403


def test_on_render_requires_explicit_exposure(env):
    key = "py-render"
    env.server.publish_flag(env.ws, env.env, {
        "schemaVersion": 1, "key": key, "type": "string", "fields": ["user_id"],
        "bucketing": {"algorithm": "grayling-fnv1a-10000-v1", "unit": "user_id", "salt": "py-render-salt"},
        "variations": {"control": "c", "treatment": "t"},
        "defaultRule": {"variation": "control"},
        "targeting": [{"name": "split", "percentage": {"control": 50, "treatment": 50}}],
    })
    _, exp = env.server.request(
        "POST", f"/api/v1/admin/workspaces/{env.ws}/environments/{env.env}/experiments",
        env.server.owner_token, {
            "flagKey": key, "unit": "user_id", "exposureMode": "onRender",
            "exposureTrigger": "after display", "integrationOwner": "python SDK test",
            "variants": ["control", "treatment"],
            "proportions": {"control": 50, "treatment": 50},
            "primaryMetric": {"key": "conversion", "type": "proportion", "direction": "increase"},
            "alpha": 0.05, "analysisWindow": 7 * 24 * 3600 * 10**9, "minSamplePerArm": 5,
        }, expect=201)
    assert exp["exposureTrigger"] == "after display"
    assert exp["integrationOwner"] == "python SDK test"
    env.server.request("POST", f"/api/v1/admin/experiments/{exp['id']}/review",
                       env.server.owner_token, {"note": "render test"}, expect=200)
    env.server.request("POST", f"/api/v1/admin/experiments/{exp['id']}/bind",
                       env.server.owner_token, None, expect=200)
    base = f"/api/v1/admin/workspaces/{env.ws}/environments/{env.env}/flags/{key}"
    env.server.request("POST", base + "/approve", env.publisher_token,
                       {"note": "approve"}, expect=200)
    env.server.request("POST", base + "/publish", env.server.owner_token,
                       {"note": "publish"}, expect=200)
    env.server.request("POST", f"/api/v1/admin/experiments/{exp['id']}/transition",
                       env.server.owner_token, {"to": "running", "reason": "launch"}, expect=200)

    with make_client(env, exposure_flush_interval=3600) as client:
        _, before = env.server.request("GET", "/api/v1/sdk/metrics", env.sdk_token, expect=200)
        _, details = client.string_variation(key, "?", attributes={"user_id": "py-render-user"})
        assert details.result.experiment.exposure_mode == "onRender"
        assert details.exposure_event_id == "" and details.exposure_token
        from dataclasses import replace
        with pytest.raises(Exception):
            client.track_evaluated_exposure(replace(details, expires_at="2000-01-01T00:00:00Z"))
        _, pending = env.server.request("GET", "/api/v1/sdk/metrics", env.sdk_token, expect=200)
        assert pending["exposures"] == before["exposures"]
        client.track_evaluated_exposure(details)
        assert client.flush() == 1
        _, after = env.server.request("GET", "/api/v1/sdk/metrics", env.sdk_token, expect=200)
        assert after["exposures"] == before["exposures"] + 1
