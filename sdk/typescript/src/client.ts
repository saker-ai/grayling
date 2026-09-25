/**
 * GolabClient — remote evaluation with a revision-guarded cache, polling
 * freshness and batched collect-contract event reporting.
 *
 * Design (docs/技术方案.md §8, §10.1) — identical contract to the Python and
 * Go SDKs:
 *
 * - Every evaluation is a `POST /api/v1/sdk/eval` (directly or through a
 *   cmd/golab-proxy relay). The full configuration snapshot — internal
 *   rules, segments and AI content — is NEVER downloaded, so this client is
 *   safe for browser-adjacent/thin runtimes (受限投影).
 * - Freshness uses `GET /api/v1/sdk/revision` (revision number only): a
 *   background poller invalidates the evaluation cache when the substrate
 *   moved; eval responses piggy-back the revision so active clients
 *   converge even when polling fails.
 * - Exposures of remote evaluations are recorded SERVER-side (deterministic
 *   idempotent event ID). Conversions and manual exposures are batched to
 *   `POST /api/v1/collect` with the separate collect credential.
 * - On any transport/protocol error the typed variation methods resolve
 *   with the CALLER'S DEFAULT and a failed Result — never a guess.
 */

import { randomUUID } from "node:crypto";
import {
  type EvaluationContext, type EvaluationDetails, type Result,
  type Variation, errorResult, resultFromWire,
} from "./types.ts";

export * from "./types.ts";

export class GolabError extends Error {}

export interface Options {
  baseUrl: string;
  /** SDK-scope credential; workspace/environment derive from it server-side. */
  sdkToken: string;
  /** Separate collection credential (§9). Empty → reporting disabled. */
  collectToken?: string;
  /** Revision-probe cadence in ms (default 30_000). */
  pollIntervalMs?: number;
  /** Per-request HTTP timeout in ms (default 10_000). */
  requestTimeoutMs?: number;
  /** LRU bound of the evaluation cache (default 10_000). */
  cacheMaxEntries?: number;
  exposureFlushIntervalMs?: number;
  exposureBatchSize?: number;
  exposureQueueCap?: number;
  /** Start the poller automatically (default true). */
  autoPoll?: boolean;
  onError?: (op: string, err: unknown) => void;
}

interface CacheEntry {
  details: EvaluationDetails;
  revision: number;
  createdAt: number;
}

export interface Status {
  revision: number | null;
  cacheSize: number;
  evals: number;
  cacheHits: number;
  cacheMisses: number;
  evalErrors: number;
  eventsQueued: number;
  eventsFlushed: number;
  eventsDropped: number;
  revisionSyncs: number;
  lastError: string;
  closed: boolean;
}

function canonicalContext(ctx: EvaluationContext): string {
  const wire: Record<string, unknown> = {};
  if (ctx.targetingKey) wire.targetingKey = ctx.targetingKey;
  if (ctx.attributes && Object.keys(ctx.attributes).length > 0) {
    wire.attributes = ctx.attributes;
  }
  return stableStringify(wire);
}

/** Deterministic JSON for cache keys (sorted object keys). */
function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${stableStringify(obj[k])}`).join(",")}}`;
}

export class GolabClient {
  readonly #opts: Required<Pick<Options, "baseUrl" | "sdkToken">> & Options;
  readonly #base: string;
  readonly #pollMs: number;
  readonly #timeoutMs: number;
  readonly #cacheMax: number;
  readonly #flushMs: number;
  readonly #batchSize: number;
  readonly #queueCap: number;

  #cache = new Map<string, CacheEntry>();
  #revision: number | null = null;
  #queue: Record<string, unknown>[] = [];
  #pollTimer: ReturnType<typeof setInterval> | null = null;
  #flushTimer: ReturnType<typeof setInterval> | null = null;
  #closed = false;
  #stats = {
    evals: 0, cacheHits: 0, cacheMisses: 0, evalErrors: 0,
    eventsFlushed: 0, eventsDropped: 0, revisionSyncs: 0, lastError: "",
  };

  constructor(opts: Options) {
    if (!opts.baseUrl || !opts.sdkToken) {
      throw new GolabError("golab: baseUrl and sdkToken are required");
    }
    this.#opts = opts;
    this.#base = opts.baseUrl.replace(/\/+$/, "");
    this.#pollMs = opts.pollIntervalMs ?? 30_000;
    this.#timeoutMs = opts.requestTimeoutMs ?? 10_000;
    this.#cacheMax = opts.cacheMaxEntries ?? 10_000;
    this.#flushMs = opts.exposureFlushIntervalMs ?? 3_000;
    this.#batchSize = opts.exposureBatchSize ?? 100;
    this.#queueCap = opts.exposureQueueCap ?? 10_000;
    if (opts.autoPoll !== false) this.#startPoller();
    this.#ensureFlusher();
  }

  // ------------------------------------------------------------------
  // Evaluation
  // ------------------------------------------------------------------

  async boolVariation(flagKey: string, def = false, ctx: EvaluationContext = {}): Promise<Variation<boolean>> {
    const d = await this.#evaluate(flagKey, ctx);
    if (!d.result.failed && typeof d.value !== "boolean") {
      return { value: def, details: this.#typeMismatch(flagKey, d, "boolean") };
    }
    if (d.result.failed) return { value: def, details: d };
    return { value: d.value as boolean, details: d };
  }

  async stringVariation(flagKey: string, def = "", ctx: EvaluationContext = {}): Promise<Variation<string>> {
    const d = await this.#evaluate(flagKey, ctx);
    if (!d.result.failed && typeof d.value !== "string") {
      return { value: def, details: this.#typeMismatch(flagKey, d, "string") };
    }
    if (d.result.failed) return { value: def, details: d };
    return { value: d.value as string, details: d };
  }

  async numberVariation(flagKey: string, def = 0, ctx: EvaluationContext = {}): Promise<Variation<number>> {
    const d = await this.#evaluate(flagKey, ctx);
    if (!d.result.failed && (typeof d.value !== "number" || !Number.isFinite(d.value))) {
      return { value: def, details: this.#typeMismatch(flagKey, d, "number") };
    }
    if (d.result.failed) return { value: def, details: d };
    return { value: d.value as number, details: d };
  }

  async jsonVariation<T = unknown>(flagKey: string, def: T, ctx: EvaluationContext = {}): Promise<Variation<T>> {
    const d = await this.#evaluate(flagKey, ctx);
    if (d.result.failed) return { value: def, details: d };
    return { value: d.value as T, details: d };
  }

  // ------------------------------------------------------------------
  // Collect-contract event reporting
  // ------------------------------------------------------------------

  /** Report a conversion attributed to the experiment of a prior
   *  evaluation. The unit is the randomization unit the SERVER echoed for
   *  the exposure, so attribution lands on the identical unit hash (§10.1). */
  trackConversion(details: EvaluationDetails, metricKey: string, value = 1,
    opts: { sessionId?: string; taskId?: string } = {}): void {
    const exp = details.result.experiment;
    if (!exp) {
      throw new GolabError(`result carries no experiment attribution (reason ${details.result.reason})`);
    }
    if (!this.#opts.collectToken) throw new GolabError("collect token not configured");
    if (!details.randomizationUnit) {
      throw new GolabError("cannot derive randomization unit (server recorded no exposure)");
    }
    const flagRevision = /^\d+$/.test(details.result.flagVersion)
      ? Number(details.result.flagVersion) : 0;
    this.#enqueue({
      eventId: `conv-${randomUUID()}`,
      type: "conversion",
      experimentId: exp.experimentId,
      epoch: exp.epoch,
      flagKey: details.result.flagKey,
      flagRevision,
      variant: exp.variant,
      unit: details.randomizationUnit,
      source: exp.source,
      metricKey,
      value,
      sessionId: opts.sessionId ?? "",
      taskId: opts.taskId ?? "",
      clientTime: new Date().toISOString(),
    });
  }

  /** Manually report an exposure aligned with the collect contract. NOT
   *  needed for remote evaluations (the server records those itself with an
   *  idempotent event ID) — use for assignments made elsewhere. */
  trackExposure(experimentId: string, epoch: number, flagKey: string,
    variant: string, unit: string,
    opts: { source?: string; flagRevision?: number } = {}): void {
    if (!this.#opts.collectToken) throw new GolabError("collect token not configured");
    if (!unit) {
      throw new GolabError("exposure without a randomization unit is rejected (no attribution without a unit)");
    }
    this.#enqueue({
      eventId: `exp-${randomUUID()}`,
      type: "exposure",
      experimentId, epoch, flagKey, variant, unit,
      source: opts.source ?? "split",
      flagRevision: opts.flagRevision ?? 0,
      clientTime: new Date().toISOString(),
    });
  }

  // ------------------------------------------------------------------
  // Freshness / lifecycle
  // ------------------------------------------------------------------

  /** Probe /api/v1/sdk/revision; invalidate the cache when it moved. */
  async syncRevision(): Promise<number> {
    const body = await this.#http<Record<string, unknown>>("GET", "/api/v1/sdk/revision", this.#opts.sdkToken);
    const revision = Number(body.revision ?? 0);
    if (this.#revision !== null && revision !== this.#revision) {
      this.#cache.clear();
    }
    this.#revision = revision;
    this.#stats.revisionSyncs++;
    return revision;
  }

  /** Drain queued events to /api/v1/collect. Returns the count posted. */
  async flush(): Promise<number> {
    const batch = this.#drain(this.#batchSize * 16);
    if (batch.length === 0) return 0;
    try {
      await this.#postEvents(batch);
    } catch (err) {
      this.#queue.push(...batch); // keep for the periodic flusher
      throw new GolabError(`flush failed: ${String(err)}`);
    }
    this.#stats.eventsFlushed += batch.length;
    return batch.length;
  }

  status(): Status {
    return {
      revision: this.#revision,
      cacheSize: this.#cache.size,
      evals: this.#stats.evals,
      cacheHits: this.#stats.cacheHits,
      cacheMisses: this.#stats.cacheMisses,
      evalErrors: this.#stats.evalErrors,
      eventsQueued: this.#queue.length,
      eventsFlushed: this.#stats.eventsFlushed,
      eventsDropped: this.#stats.eventsDropped,
      revisionSyncs: this.#stats.revisionSyncs,
      lastError: this.#stats.lastError,
      closed: this.#closed,
    };
  }

  /** Stop timers and flush pending events once. */
  async close(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    if (this.#pollTimer) clearInterval(this.#pollTimer);
    if (this.#flushTimer) clearInterval(this.#flushTimer);
    try { await this.flush(); } catch { /* drops are counted */ }
  }

  // ------------------------------------------------------------------
  // Internals
  // ------------------------------------------------------------------

  #startPoller(): void {
    if (this.#pollTimer === null) {
      this.#pollTimer = setInterval(() => {
        this.syncRevision().catch((err) => {
          this.#stats.lastError = `revision poll: ${String(err)}`;
          this.#opts.onError?.("poll", err);
        });
      }, this.#pollMs);
      this.#pollTimer.unref?.();
    }
  }

  #ensureFlusher(): void {
    if (this.#flushTimer === null && this.#opts.collectToken) {
      this.#flushTimer = setInterval(() => { void this.#flushOnce(); }, this.#flushMs);
      this.#flushTimer.unref?.();
    }
  }

  async #evaluate(flagKey: string, ctx: EvaluationContext): Promise<EvaluationDetails> {
    if (this.#closed) {
      return { value: null, result: errorResult(flagKey, "client is closed"),
        manifestRevision: 0, exposureEventId: "", randomizationUnit: "", fromCache: false };
    }
    this.#ensureFlusher();
    const wire: Record<string, unknown> = {};
    if (ctx.targetingKey) wire.targetingKey = ctx.targetingKey;
    if (ctx.attributes) wire.attributes = ctx.attributes;
    const cacheKey = `${flagKey}\0${canonicalContext(ctx)}`;

    this.#stats.evals++;
    const hit = this.#cache.get(cacheKey);
    if (hit && (this.#revision === null || hit.revision === this.#revision)) {
      this.#stats.cacheHits++;
      return { ...hit.details, fromCache: true };
    }
    this.#stats.cacheMisses++;

    let body: Record<string, unknown>;
    try {
      body = await this.#http<Record<string, unknown>>(
        "POST", "/api/v1/sdk/eval", this.#opts.sdkToken, { flagKey, context: wire });
    } catch (err) {
      this.#stats.evalErrors++;
      this.#stats.lastError = `eval ${flagKey}: ${String(err)}`;
      this.#opts.onError?.("eval", err);
      return { value: null, result: errorResult(flagKey, `remote evaluation failed: ${String(err)}`),
        manifestRevision: 0, exposureEventId: "", randomizationUnit: "", fromCache: false };
    }

    const result: Result = resultFromWire(body.result as Partial<Result>);
    const details: EvaluationDetails = {
      value: body.value ?? null,
      result,
      manifestRevision: Number(body.manifestRevision ?? 0),
      exposureEventId: String(body.exposureEventId ?? ""),
      randomizationUnit: String(body.randomizationUnit ?? ""),
      fromCache: false,
    };

    // The eval response is itself a revision observation: newer than our
    // view → invalidate everything computed against older config.
    if (this.#revision !== null && details.manifestRevision > this.#revision) {
      this.#cache.clear();
    }
    if (this.#revision === null || details.manifestRevision > this.#revision) {
      this.#revision = details.manifestRevision;
    }
    this.#cache.set(cacheKey, { details, revision: details.manifestRevision, createdAt: Date.now() });
    if (this.#cache.size > this.#cacheMax) {
      // Map preserves insertion order: evict oldest first.
      const excess = this.#cache.size - this.#cacheMax;
      let i = 0;
      for (const k of this.#cache.keys()) {
        if (i++ >= excess) break;
        this.#cache.delete(k);
      }
    }
    return details;
  }

  #typeMismatch(flagKey: string, d: EvaluationDetails, want: string): EvaluationDetails {
    return {
      ...d,
      result: {
        ...d.result,
        errorCode: "TYPE_MISMATCH",
        errorDetails: `flag value is ${typeof d.value}, want ${want}`,
        failed: true,
        trackEvents: false,
        experiment: null,
      },
    };
  }

  #enqueue(event: Record<string, unknown>): void {
    if (this.#queue.length >= this.#queueCap) {
      this.#stats.eventsDropped++;
      return;
    }
    this.#queue.push(event);
  }

  #drain(max: number): Record<string, unknown>[] {
    return this.#queue.splice(0, max);
  }

  async #flushOnce(): Promise<void> {
    const batch = this.#drain(this.#batchSize);
    if (batch.length === 0) return;
    try {
      await this.#postEvents(batch);
    } catch {
      await new Promise((r) => setTimeout(r, 500)); // one retry (Go SDK parity)
      try {
        await this.#postEvents(batch);
      } catch (err) {
        this.#stats.eventsDropped += batch.length;
        this.#stats.lastError = `collect flush: ${String(err)}`;
        this.#opts.onError?.("collect", err);
        return;
      }
    }
    this.#stats.eventsFlushed += batch.length;
  }

  #postEvents(batch: Record<string, unknown>[]): Promise<Record<string, unknown>> {
    return this.#http<Record<string, unknown>>(
      "POST", "/api/v1/collect", this.#opts.collectToken!, { events: batch });
  }

  async #http<T>(method: string, path: string, token: string | undefined,
    payload?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (token) headers.Authorization = `Bearer ${token}`;
    let body: string | undefined;
    if (payload !== undefined) {
      body = JSON.stringify(payload);
      headers["Content-Type"] = "application/json";
    }
    let resp: Response;
    try {
      resp = await fetch(this.#base + path, {
        method, headers, body, signal: AbortSignal.timeout(this.#timeoutMs),
      });
    } catch (err) {
      throw new GolabError(`${method} ${path}: ${String(err)}`);
    }
    const text = await resp.text();
    if (!resp.ok) {
      throw new GolabError(`${method} ${path} → HTTP ${resp.status}: ${text.slice(0, 512)}`);
    }
    if (!text) return {} as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new GolabError(`${method} ${path}: invalid JSON response`);
    }
  }
}
