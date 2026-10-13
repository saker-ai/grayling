/**
 * Full remote-evaluation contract against the REAL golab binary: typed
 * variations, targeting, caller-default semantics, experiment attribution
 * with server-side exposures, collect-contract alignment (dedup, unit
 * required, epoch mismatch), cache/polling freshness and the restricted
 * revision projection. Mirrors sdk/python/tests — cross-language 用例同值.
 */

import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { startServer, TestEnv, type GolabServer } from "./helpers.ts";
import { GolabClient, GolabError } from "../src/client.ts";

let server: GolabServer;
let env: TestEnv;

before(async () => {
  server = await startServer("it");
  env = await TestEnv.create(server);
});

after(async () => {
  await server?.stop();
});

function makeClient(overrides: Partial<Parameters<typeof makeOptions>[0]> = {}) {
  return new GolabClient(makeOptions(overrides));
}

function makeOptions(overrides: Record<string, unknown> = {}) {
  return {
    baseUrl: env.server.baseUrl,
    sdkToken: env.sdkToken,
    collectToken: env.collectToken,
    pollIntervalMs: 3_600_000, // tests drive freshness explicitly
    autoPoll: false,
    ...overrides,
  };
}

test("bool variation: targeting + default rule", async () => {
  const client = makeClient();
  try {
    const hit = await client.boolVariation("ts-bool", false, { attributes: { tenant: "acme" } });
    assert.equal(hit.value, true);
    assert.equal(hit.details.result.reason, "TARGETING_MATCH");
    assert.equal(hit.details.result.failed, false);
    assert.ok(hit.details.manifestRevision > 0);
    const diag = await client.diagnose(hit.details.result.decisionId);
    const result = diag.result as Record<string, unknown>;
    assert.equal(result.rule, hit.details.result.rule);
    assert.equal(result.manifestRevision, hit.details.manifestRevision);
    assert.equal(diag.exposureStatus, "none");
    assert.ok(!JSON.stringify(diag).includes("user_id"));

    const miss = await client.boolVariation("ts-bool", false, { attributes: { tenant: "other" } });
    assert.equal(miss.value, false);
    assert.equal(miss.details.result.reason, "DEFAULT");
  } finally { await client.close(); }
});

test("string / number / json variations", async () => {
  const client = makeClient();
  try {
    const s = await client.stringVariation("ts-string", "fallback");
    assert.equal(s.value, "alpha");
    assert.equal(s.details.result.reason, "DEFAULT");

    const n = await client.numberVariation("ts-number", 0);
    assert.equal(n.value, 42.5);

    const j = await client.jsonVariation<{ model: string; temperature: number }>("ts-json", { model: "", temperature: 0 });
    assert.deepEqual(j.value, { model: "qwen-max", temperature: 0.2 });
  } finally { await client.close(); }
});

test("unknown flag → caller default + FLAG_NOT_FOUND", async () => {
  const client = makeClient();
  try {
    const v = await client.boolVariation("no-such-flag", true);
    assert.equal(v.value, true);
    assert.equal(v.details.result.failed, true);
    assert.equal(v.details.result.errorCode, "FLAG_NOT_FOUND");
  } finally { await client.close(); }
});

test("type mismatch → caller default + TYPE_MISMATCH", async () => {
  const client = makeClient();
  try {
    const v = await client.boolVariation("ts-string", true);
    assert.equal(v.value, true);
    assert.equal(v.details.result.failed, true);
    assert.equal(v.details.result.errorCode, "TYPE_MISMATCH");
    assert.equal(v.details.result.reason, "ERROR");
    assert.equal(v.details.result.variationType, "");
  } finally { await client.close(); }
});

test("transport error → caller default + ERROR, counted", async () => {
  const client = new GolabClient({
    baseUrl: "http://127.0.0.1:1", sdkToken: "glab_sdk_x", autoPoll: false,
    requestTimeoutMs: 2_000,
  });
  try {
    const v = await client.boolVariation("ts-bool", false);
    assert.equal(v.value, false);
    assert.equal(v.details.result.failed, true);
    assert.equal(v.details.result.reason, "ERROR");
    assert.equal(client.status().evalErrors, 1);
    assert.equal(client.status().state, "ERROR");
  } finally { await client.close(); }
});

test("remote status expires and recovers after revision probe", async () => {
  const client = makeClient({ maxStalenessMs: 20 });
  try {
    assert.equal(client.status().state, "NOT_READY");
    const result = await client.stringVariation("ts-string", "?");
    assert.ok(result.details.manifestRevision > 0);
    assert.equal(client.status().state, "READY");
    await new Promise(resolve => setTimeout(resolve, 40));
    assert.equal(client.status().state, "STALE");
    await client.syncRevision();
    assert.equal(client.status().state, "READY");
  } finally { await client.close(); }
});

test("wrong audience token → caller default (never crashes)", async () => {
  const client = new GolabClient({
    baseUrl: env.server.baseUrl, sdkToken: env.collectToken, autoPoll: false,
  });
  try {
    const v = await client.stringVariation("ts-string", "safe");
    assert.equal(v.value, "safe");
    assert.equal(v.details.result.failed, true);
  } finally { await client.close(); }
});

test("experiment enrollment: server-side exposure + deterministic variant", async () => {
  const client = makeClient();
  try {
    const units = new Map<string, string>();
    for (let i = 0; i < 60; i++) {
      const unit = `ts-user-${i}`;
      const v = await client.stringVariation("ts-exp", "?", { attributes: { user_id: unit } });
      assert.ok(v.value === "c" || v.value === "t");
      const exp = v.details.result.experiment;
      assert.ok(exp, `${unit} should be enrolled`);
      assert.equal(exp.experimentId, env.experimentId);
      assert.equal(exp.epoch, env.epoch);
      assert.equal(exp.source, "split");
      assert.equal(v.details.result.trackEvents, true);
      assert.ok(v.details.exposureEventId.startsWith("srv-"));
      assert.equal(v.details.randomizationUnit, unit);
      units.set(unit, v.value);
    }
    // Deterministic assignment (fixed-bucket contract, §6).
    let i = 0;
    for (const [unit, value] of units) {
      if (i++ >= 10) break;
      const again = await client.stringVariation("ts-exp", "?", { attributes: { user_id: unit } });
      assert.equal(again.value, value);
    }
    // Missing unit → MISSING_BUCKET_KEY + caller default, never random.
    const noUnit = await client.stringVariation("ts-exp", "safe-default");
    assert.equal(noUnit.value, "safe-default");
    assert.equal(noUnit.details.result.errorCode, "MISSING_BUCKET_KEY");
    assert.equal(noUnit.details.result.experiment, null);

    const rep = await env.report();
    const total = rep.perVariant.reduce((n: number, v: any) => n + v.randomizedUnits, 0);
    assert.ok(total >= 50, `randomized units should be ≥50, got ${total}`);
  } finally { await client.close(); }
});

test("trackConversion attribution lands in the report", async () => {
  const client = makeClient({ exposureFlushIntervalMs: 200 });
  try {
    let converted = 0;
    let decisionId = "";
    for (let i = 0; i < 30; i++) {
      const unit = `ts-conv-${i}`;
      const v = await client.stringVariation("ts-exp", "?", { attributes: { user_id: unit } });
      const exp = v.details.result.experiment;
      assert.ok(exp);
      if (exp.variant === "treatment") {
        client.trackConversion(v.details, "conversion", 1, { sessionId: `sess-${unit}` });
        decisionId = v.details.result.decisionId;
        converted++;
      }
    }
    assert.ok(converted > 0, "30 units at 50/50 must hit treatment");
    const flushed = await client.flush();
    assert.ok(flushed >= converted);
    const diag = await client.diagnose(decisionId);
    assert.equal(diag.exposureStatus, "recorded");
    assert.ok(Number(diag.conversionCount) >= 1);
    const metrics = await fetch(`${env.server.baseUrl}/api/v1/sdk/metrics`, {
      headers: { Authorization: `Bearer ${env.sdkToken}` },
    }).then(r => r.json()) as Record<string, number>;
    assert.ok(metrics.exposures >= converted);
    assert.ok(metrics.conversions >= converted);
    assert.deepEqual(Object.keys(metrics).sort(), ["conversions", "exposures"]);

    const rep = await env.report();
    const treat = rep.perVariant.find((v: any) => v.variant === "treatment");
    assert.ok(treat.convertedUnits >= converted,
      `convertedUnits ${treat.convertedUnits} < ${converted}`);
  } finally { await client.close(); }
});

test("trackConversion requires attribution and collect token", async () => {
  const client = makeClient();
  try {
    const s = await client.stringVariation("ts-string", "?");
    assert.throws(() => client.trackConversion(s.details, "conversion", 1), GolabError);
  } finally { await client.close(); }
  const noCollect = new GolabClient({
    baseUrl: env.server.baseUrl, sdkToken: env.sdkToken, autoPoll: false,
  });
  try {
    const v = await noCollect.stringVariation("ts-exp", "?", { attributes: { user_id: "x-1" } });
    assert.throws(() => noCollect.trackConversion(v.details, "conversion", 1), GolabError);
  } finally { await noCollect.close(); }
});

test("cache: second identical eval served from cache", async () => {
  const client = makeClient();
  try {
    const a = await client.boolVariation("ts-bool", false, { attributes: { tenant: "acme" } });
    assert.equal(a.details.fromCache, false);
    const b = await client.boolVariation("ts-bool", false, { attributes: { tenant: "acme" } });
    assert.equal(b.details.fromCache, true);
    assert.equal(b.value, a.value);
    const st = client.status();
    assert.equal(st.evals, 2);
    assert.equal(st.cacheHits, 1);
    assert.equal(st.cacheMisses, 1);
    // Different context → different key → network eval.
    await client.boolVariation("ts-bool", false, { attributes: { tenant: "globex" } });
    assert.equal(client.status().cacheMisses, 2);
  } finally { await client.close(); }
});

test("poller invalidates cache after publish", async () => {
  const client = makeClient({ pollIntervalMs: 200, autoPoll: true });
  try {
    const before = await client.boolVariation("ts-bool", false, { attributes: { tenant: "globex" } });
    assert.equal(before.value, false);
    const oldRevision = before.details.manifestRevision;

    await env.republishBool("on"); // default rule flips for non-acme

    const deadline = Date.now() + 10_000;
    while (Date.now() < deadline && client.status().revision === oldRevision) {
      await new Promise((r) => setTimeout(r, 100));
    }
    assert.notEqual(client.status().revision, oldRevision,
      "poller never observed the new revision");

    const after = await client.boolVariation("ts-bool", false, { attributes: { tenant: "globex" } });
    assert.equal(after.value, true, "cached pre-publish result must be invalidated");
    assert.equal(after.details.fromCache, false);

    await env.republishBool("off"); // restore fixture
    const restoreDeadline = Date.now() + 10_000;
    for (;;) {
      const v = await client.boolVariation("ts-bool", false, { attributes: { tenant: "globex" } });
      if (v.value === false) break;
      assert.ok(Date.now() < restoreDeadline, "restore not observed");
      await new Promise((r) => setTimeout(r, 200));
    }
  } finally { await client.close(); }
});

test("eval response piggy-backs revision invalidation (no polling)", async () => {
  const client = makeClient({ autoPoll: false });
  try {
    const d1 = await client.stringVariation("ts-string", "?");
    const rev1 = d1.details.manifestRevision;
    const d2 = await client.stringVariation("ts-string", "?");
    assert.equal(d2.details.fromCache, true);

    await env.server.publishFlag(env.ws, env.env, {
      schemaVersion: 1, key: "ts-string", type: "string",
      variations: { a: "alpha2", b: "beta" },
      defaultRule: { variation: "a" },
    });

    // A different flag's eval reveals the new revision and purges the cache.
    await client.numberVariation("ts-number", 0);
    assert.ok((client.status().revision ?? 0) > rev1);
    const d3 = await client.stringVariation("ts-string", "?");
    assert.equal(d3.details.fromCache, false);
    assert.equal(d3.value, "alpha2");

    await env.server.publishFlag(env.ws, env.env, {
      schemaVersion: 1, key: "ts-string", type: "string",
      variations: { a: "alpha", b: "beta" },
      defaultRule: { variation: "a" },
    }); // restore
  } finally { await client.close(); }
});

test("collect contract: dedup by eventId, unit required, epoch mismatch rejected", async () => {
  const collect = async (events: unknown[]) => {
    const r = await env.server.request("POST", "/api/v1/collect", env.collectToken, { events });
    assert.equal(r.status, 200);
    return r.body;
  };
  const eventId = `ts-dedup-${Date.now()}`;
  const event = {
    eventId, type: "exposure",
    experimentId: env.experimentId, epoch: env.epoch,
    flagKey: "ts-exp", variant: "control",
    unit: "ts-dedup-user", source: "split",
    clientTime: new Date().toISOString(),
  };
  const first = await collect([event]);
  assert.equal(first.accepted, 1);
  assert.equal(first.duplicates, 0);
  const second = await collect([event, { ...event, eventId: `${eventId}-x` }]);
  assert.equal(second.accepted, 1);
  assert.equal(second.duplicates, 1);

  const rejected = await collect([
    { ...event, eventId: `${eventId}-nounit`, unit: "" },
    { ...event, eventId: `${eventId}-epoch`, epoch: env.epoch + 99, unit: "u2" },
  ]);
  assert.equal(rejected.accepted, 0);
  assert.equal(rejected.rejected, 2);
  assert.ok(rejected.rejectMessages?.length > 0);

  // The SDK token is the wrong audience for /collect (403).
  const wrong = await env.server.request("POST", "/api/v1/collect", env.sdkToken, { events: [] });
  assert.equal(wrong.status, 403);
});

test("manual exposure reporting via collect contract", async () => {
  const client = makeClient({ exposureFlushIntervalMs: 200 });
  try {
    client.trackExposure(env.experimentId, env.epoch, "ts-exp", "control",
      "ts-manual-user", { source: "split" });
    assert.equal(await client.flush(), 1);
    assert.ok(client.status().eventsFlushed >= 1);
    assert.equal(client.status().eventsDropped, 0);
    assert.throws(() => client.trackExposure(env.experimentId, env.epoch, "ts-exp", "control", ""),
      GolabError);
  } finally { await client.close(); }
});

test("revision projection stays minimal", async () => {
  const client = makeClient();
  try {
    const rev = await client.syncRevision();
    assert.equal(rev, client.status().revision);
    assert.ok(rev > 0);
    const r = await env.server.request("GET", "/api/v1/sdk/revision", env.sdkToken);
    assert.equal(r.status, 200);
    assert.deepEqual(Object.keys(r.body).sort(), ["etag", "revision"]);
    // Admin token is the wrong audience on sdk endpoints.
    const denied = await env.server.request("GET", "/api/v1/sdk/revision", env.server.ownerToken);
    assert.equal(denied.status, 403);
  } finally { await client.close(); }
});

test("onRender records assignment before explicit exposure", async () => {
  const key = "ts-render";
  await server.publishFlag(env.ws, env.env, {
    schemaVersion: 1, key, type: "string", fields: ["user_id"],
    bucketing: { algorithm: "golab-fnv1a-10000-v1", unit: "user_id", salt: "ts-render-salt" },
    variations: { control: "c", treatment: "t" },
    defaultRule: { variation: "control" },
    targeting: [{ name: "split", percentage: { control: 50, treatment: 50 } }],
  });
  const exp = await server.expect("POST",
    `/api/v1/admin/workspaces/${env.ws}/environments/${env.env}/experiments`, 201,
    server.ownerToken, {
      flagKey: key, unit: "user_id", exposureMode: "onRender",
      variants: ["control", "treatment"], proportions: { control: 50, treatment: 50 },
      primaryMetric: { key: "conversion", type: "proportion", direction: "increase" },
      alpha: 0.05, analysisWindow: 7 * 24 * 3600 * 1e9, minSamplePerArm: 5,
    });
  await server.expect("POST", `/api/v1/admin/experiments/${exp.id}/review`, 200,
    server.ownerToken, { note: "render test" });
  await server.expect("POST", `/api/v1/admin/experiments/${exp.id}/bind`, 200, server.ownerToken);
  await server.expect("POST", `/api/v1/admin/workspaces/${env.ws}/environments/${env.env}/flags/${key}/approve`,
    200, env.publisherToken, { note: "approve" });
  await server.expect("POST", `/api/v1/admin/workspaces/${env.ws}/environments/${env.env}/flags/${key}/publish`,
    200, server.ownerToken, { note: "publish" });
  await server.expect("POST", `/api/v1/admin/experiments/${exp.id}/transition`, 200,
    server.ownerToken, { to: "running", reason: "launch" });

  const client = makeClient();
  try {
    const before = await server.expect("GET", "/api/v1/sdk/metrics", 200, env.sdkToken);
    const result = await client.stringVariation(key, "?", { attributes: { user_id: "ts-render-user" } });
    assert.equal(result.details.result.experiment?.exposureMode, "onRender");
    assert.equal(result.details.exposureEventId, "");
    assert.ok(result.details.exposureToken);
    assert.throws(() => client.trackEvaluatedExposure({
      ...result.details, expiresAt: new Date(Date.now() - 1000).toISOString(),
    }), GolabError);
    const pending = await server.expect("GET", "/api/v1/sdk/metrics", 200, env.sdkToken);
    assert.equal(pending.exposures, before.exposures);
    client.trackEvaluatedExposure(result.details);
    assert.equal(await client.flush(), 1);
    const after = await server.expect("GET", "/api/v1/sdk/metrics", 200, env.sdkToken);
    assert.equal(after.exposures, before.exposures + 1);
  } finally { await client.close(); }
});
