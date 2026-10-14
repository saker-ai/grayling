/**
 * grayling-fnv1a-10000-v1 — the Grayling bucketing contract in TypeScript.
 *
 * Exists to prove cross-language determinism (P4 退出验收: 跨语言用例同值):
 * validated against the SAME golden vectors as the Go reference
 * implementation (pkg/bucketing/testdata/golden_vectors.json).
 *
 * The remote-evaluation client does NOT bucket locally — assignments come
 * from the server. Use this module to verify/replicate assignments offline
 * (analysis, migrations, tests).
 */

export const ALGORITHM_ID = "grayling-fnv1a-10000-v1";
export const BUCKETS = 10000;

const FNV_OFFSET = 0xcbf29ce484222325n;
const FNV_PRIME = 0x100000001b3n;
const MASK64 = (1n << 64n) - 1n;

export const PURPOSE_ELIGIBILITY = "eligibility";
export const PURPOSE_VARIANT = "variant";
export const PURPOSE_LAYER = "layer";

function netstring(value: string): Uint8Array {
  const raw = new TextEncoder().encode(value);
  const len = new TextEncoder().encode(String(raw.length));
  const out = new Uint8Array(len.length + 1 + raw.length);
  out.set(len, 0);
  out[len.length] = 0x3a; // ":"
  out.set(raw, len.length + 1);
  return out;
}

export function fnv1a64(data: Uint8Array): bigint {
  let h = FNV_OFFSET;
  for (const b of data) {
    h ^= BigInt(b);
    h = (h * FNV_PRIME) & MASK64;
  }
  return h;
}

export interface BucketInputs {
  workspace: string;
  environment: string;
  key: string;
  epoch: number;
  salt: string;
  unitValue: string;
  purpose: string;
}

/** Canonical hash input: length-prefixed (netstring) fields in fixed order,
 *  epoch as ASCII decimal. */
export function bucketInput(input: BucketInputs): Uint8Array {
  if (input.purpose.includes(":")) throw new Error("purpose must not contain ':'");
  if (!Number.isInteger(input.epoch) || input.epoch < 0) {
    throw new Error("epoch must be a non-negative integer");
  }
  const parts = [
    input.workspace, input.environment, input.key,
    String(input.epoch), input.salt, input.unitValue, input.purpose,
  ];
  const encoded = parts.map(netstring);
  const total = encoded.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of encoded) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/** Stable bucket in [0, 10000) for one hash coordinate. */
export function bucket(input: BucketInputs): number {
  return Number(fnv1a64(bucketInput(input)) % BigInt(BUCKETS));
}

export function hash64Hex(input: BucketInputs): string {
  return fnv1a64(bucketInput(input)).toString(16).padStart(16, "0");
}
