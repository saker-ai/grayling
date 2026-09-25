"""Config cache + polling freshness: results are cached per (flag, context),
invalidated when the substrate revision moves (discovered by polling or
piggy-backed on eval responses), and the revision projection endpoint stays
minimal."""

from __future__ import annotations

import time

from test_eval import make_client


def test_cache_hit_skips_network(env):
    with make_client(env) as client:
        v1, d1 = client.bool_variation("py-bool", False,
                                       attributes={"tenant": "acme"})
        assert d1.from_cache is False
        v2, d2 = client.bool_variation("py-bool", False,
                                       attributes={"tenant": "acme"})
        assert v2 == v1
        assert d2.from_cache is True
        st = client.status()
        assert st["cacheHits"] == 1
        assert st["cacheMisses"] == 1
        assert st["evals"] == 2
        # Different context → different cache key → network eval.
        _, d3 = client.bool_variation("py-bool", False,
                                      attributes={"tenant": "globex"})
        assert d3.from_cache is False
        assert client.status()["cacheMisses"] == 2


def test_poll_invalidates_cache_after_publish(env):
    # Fast poller: the background thread must notice the new revision and
    # drop cached results computed against the old config.
    client = make_client(env, poll_interval=0.2, auto_poll=True)
    try:
        value, details = client.bool_variation("py-bool", False,
                                               attributes={"tenant": "globex"})
        assert value is False
        old_revision = details.manifest_revision

        # Publish a new revision flipping the default rule for non-acme.
        env.republish_bool("on")

        deadline = time.time() + 10
        while time.time() < deadline:
            if client.status()["revision"] != old_revision:
                break
            time.sleep(0.1)
        assert client.status()["revision"] != old_revision, \
            "poller never observed the new revision"

        value, details = client.bool_variation("py-bool", False,
                                               attributes={"tenant": "globex"})
        assert value is True, "cached pre-publish result must be invalidated"
        assert details.from_cache is False
        assert details.manifest_revision == client.status()["revision"]

        # Restore the fixture for later tests.
        env.republish_bool("off")
        deadline = time.time() + 10
        while time.time() < deadline:
            v, _ = client.bool_variation("py-bool", False,
                                         attributes={"tenant": "globex"})
            if v is False:
                break
            time.sleep(0.2)
    finally:
        client.close()


def test_eval_response_piggybacks_revision_invalidation(env):
    # Polling disabled: freshness must still converge because every eval
    # response carries the manifest revision.
    client = make_client(env, auto_poll=False)
    try:
        _, d1 = client.string_variation("py-string", "?")
        rev1 = d1.manifest_revision
        # Cached answer while the substrate is unchanged.
        _, d2 = client.string_variation("py-string", "?")
        assert d2.from_cache is True

        env.server.publish_flag(env.ws, env.env, {
            "schemaVersion": 1, "key": "py-string", "type": "string",
            "variations": {"a": "alpha2", "b": "beta"},
            "defaultRule": {"variation": "a"},
        })

        # A DIFFERENT flag's eval reveals the new revision and must purge
        # the stale py-string entry.
        _, d3 = client.string_variation("py-number", "?")  # number via string API → mismatch but still an eval
        assert client.status()["revision"] > rev1
        _, d4 = client.string_variation("py-string", "?")
        assert d4.from_cache is False
        assert d4.value == "alpha2"

        # Restore fixture.
        env.server.publish_flag(env.ws, env.env, {
            "schemaVersion": 1, "key": "py-string", "type": "string",
            "variations": {"a": "alpha", "b": "beta"},
            "defaultRule": {"variation": "a"},
        })
    finally:
        client.close()


def test_sync_revision_projection_is_minimal(env):
    with make_client(env) as client:
        rev = client.sync_revision()
        assert rev == client.status()["revision"]
        assert rev > 0
        # The projection endpoint carries nothing but revision + etag.
        body = client._http_json("GET", "/api/v1/sdk/revision",
                                 token=env.sdk_token)
        assert set(body.keys()) == {"revision", "etag"}
