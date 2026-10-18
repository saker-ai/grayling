// U4 acceptance harness — evidence writer.
// All artifacts land in docs/acceptance/web-2026-10-18-redesign/ so R5B can
// assemble the final report.md from raw JSON + PNG without rerunning anything.
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const EVIDENCE_DIR = path.resolve(HERE, '..', '..', '..', '..', 'docs', 'acceptance', 'web-2026-10-18-redesign');

let ensured = false;
function ensureDir() {
  if (!ensured) {
    mkdirSync(EVIDENCE_DIR, { recursive: true });
    ensured = true;
  }
}

export function writeJSON(name, data) {
  ensureDir();
  const file = path.join(EVIDENCE_DIR, name);
  writeFileSync(file, JSON.stringify(data, null, 2) + '\n');
  return file;
}

export function writeText(name, text) {
  ensureDir();
  const file = path.join(EVIDENCE_DIR, name);
  writeFileSync(file, text);
  return file;
}

export async function screenshot(page, name, opts = {}) {
  ensureDir();
  const file = path.join(EVIDENCE_DIR, name);
  await page.screenshot({ path: file, fullPage: false, ...opts });
  return file;
}

// median of a numeric array (used by the perf baseline).
export function median(values) {
  if (!values.length) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}
