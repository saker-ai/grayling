/**
 * Cross-language bucketing contract: the TypeScript implementation of
 * grayling-fnv1a-10000-v1 must reproduce EVERY golden vector shared with the
 * Go reference implementation (P4 退出验收: 跨语言用例同值).
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { REPO_ROOT } from "./helpers.ts";
import { bucket, hash64Hex } from "../src/bucketing.ts";

interface Vector {
  desc: string;
  workspace: string;
  environment: string;
  key: string;
  epoch: number;
  salt: string;
  unitValue: string;
  purpose: string;
  hash64Hex: string;
  bucket: number;
}

const doc = JSON.parse(readFileSync(
  join(REPO_ROOT, "pkg", "bucketing", "testdata", "golden_vectors.json"), "utf8"));

test("golden vectors match the Go reference", () => {
  assert.equal(doc.algorithm, "grayling-fnv1a-10000-v1");
  assert.equal(doc.modulus, 10000);
  assert.ok(doc.vectors.length >= 20);
  for (const v of doc.vectors as Vector[]) {
    const input = {
      workspace: v.workspace, environment: v.environment, key: v.key,
      epoch: v.epoch, salt: v.salt, unitValue: v.unitValue, purpose: v.purpose,
    };
    assert.equal(hash64Hex(input), v.hash64Hex, `hash mismatch: ${v.desc}`);
    assert.equal(bucket(input), v.bucket, `bucket mismatch: ${v.desc}`);
  }
});

test("purpose coordinates are disjoint", () => {
  const base = {
    workspace: "ws", environment: "prod", key: "flag", epoch: 1,
    salt: "salt", unitValue: "user-1",
  };
  const coords = new Set([
    bucket({ ...base, purpose: "eligibility" }),
    bucket({ ...base, purpose: "variant" }),
    bucket({ ...base, purpose: "layer" }),
  ]);
  assert.equal(coords.size, 3);
});

test("input validation", () => {
  const base = {
    workspace: "ws", environment: "prod", key: "flag", epoch: 0,
    salt: "salt", unitValue: "u", purpose: "variant",
  };
  assert.throws(() => bucket({ ...base, epoch: -1 }));
  assert.throws(() => bucket({ ...base, purpose: "bad:purpose" }));
});
