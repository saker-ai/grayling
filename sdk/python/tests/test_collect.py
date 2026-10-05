"""Collect-contract alignment: batched conversion/exposure reporting from
the SDK, server-side dedup by eventId, unit-required rejection and epoch
mismatch rejection — verified through the real /api/v1/collect endpoint."""

from __future__ import annotations

import json
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
    with make_client(env, exposure_flush_interval=0.2) as client:
        converted = 0
        converted_details = None
        for i in range(30):
            unit = f"conv-user-{i}"
            _, details = client.string_variation(
                "py-exp", "?", attributes={"user_id": unit})
            assert details.result.experiment is not None
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


def test_manual_exposure_reporting(env):
    with make_client(env, exposure_flush_interval=0.2) as client:
        client.track_exposure(env.experiment_id, env.epoch, "py-exp",
                            "control", "manual-user-1", source="split")
        assert client.flush() == 1
        st = client.status()
        assert st["eventsFlushed"] >= 1 and st["eventsDropped"] == 0
        # Manual exposure without a unit is refused client-side already.
        with pytest.raises(Exception):
            client.track_exposure(env.experiment_id, env.epoch, "py-exp",
                                  "control", "")


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
