/**
 * Wire types mirroring the Grayling evaluation contract (pkg/model).
 */

/** Randomization unit + targeting attributes for one evaluation (§6). */
export interface EvaluationContext {
  /** Stable, authenticated or server-generated identity — never invented. */
  targetingKey?: string;
  /** Attributes must be registered in the flag's `fields` allowlist. */
  attributes?: Record<string, unknown>;
}

/** Experiment attribution of a result (mirrors model.Exposure). */
export interface Exposure {
  experimentId: string;
  epoch: number;
  variant: string;
  /** split | targeting | default — only "split" is randomized. */
  source: string;
  exposureMode?: "onDecision" | "onRender";
}

/** One evaluation outcome (mirrors model.Result). Failed results carry the
 *  caller-provided default in the typed APIs and must never be interpreted
 *  as an assignment. */
export interface Result {
  flagKey: string;
  value: unknown;
  variationType: string;
  reason: string;
  errorCode: string;
  errorDetails: string;
  failed: boolean;
  flagVersion: string;
  trackEvents: boolean;
  experiment: Exposure | null;
  bucket: number;
  rule: string;
  decisionId: string;
  manifestRevision: number;
  snapshotAgeMillis: number;
  exposureEventId: string;
}

/** Value + full result + server-side exposure metadata of one eval. */
export interface EvaluationDetails {
  value: unknown;
  result: Result;
  manifestRevision: number;
  /** Deterministic server-side exposure event ID ("" when not enrolled). */
  exposureEventId: string;
  exposureToken?: string;
  expiresAt?: string;
  /** Canonical randomization unit the exposure was attributed to. */
  randomizationUnit: string;
  fromCache: boolean;
}

/** Typed variation outcome. */
export interface Variation<T> {
  value: T;
  details: EvaluationDetails;
}

export function resultFromWire(raw: Partial<Result> | undefined): Result {
  const r = raw ?? {};
  return {
    flagKey: r.flagKey ?? "",
    value: r.value ?? null,
    variationType: r.variationType ?? "",
    reason: r.reason ?? "",
    errorCode: r.errorCode ?? "",
    errorDetails: r.errorDetails ?? "",
    failed: Boolean(r.failed),
    flagVersion: r.flagVersion ?? "",
    trackEvents: Boolean(r.trackEvents),
    experiment: r.experiment ?? null,
    bucket: typeof r.bucket === "number" ? r.bucket : -1,
    rule: r.rule ?? "",
    decisionId: r.decisionId ?? "",
    manifestRevision: r.manifestRevision ?? 0,
    snapshotAgeMillis: r.snapshotAgeMillis ?? 0,
    exposureEventId: r.exposureEventId ?? "",
  };
}

/** Synthetic failure result for transport/protocol errors — same semantics
 *  as the Go SDK's cold-start behaviour (§3: return caller default, report
 *  the reason; never guess). */
export function errorResult(flagKey: string, details: string, code = "GENERAL"): Result {
  return {
    flagKey, value: null, variationType: "", reason: "ERROR",
    errorCode: code, errorDetails: details, failed: true,
    flagVersion: "", trackEvents: false, experiment: null, bucket: -1,
    rule: "", decisionId: "", manifestRevision: 0, snapshotAgeMillis: 0,
    exposureEventId: "",
  };
}
