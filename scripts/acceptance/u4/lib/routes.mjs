// U4 acceptance harness — route table resolution.
// routes.json uses ${PLACEHOLDER}s; resolveRoutes binds them to a seed summary.
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));

export function resolveRoutes(seed) {
  const table = JSON.parse(readFileSync(path.join(HERE, '..', 'routes.json'), 'utf8'));
  const vars = {
    WS: seed.ws,
    ENV: seed.env,
    FLAG: seed.flagKey,
    FLAG_DRAFT: seed.flagDraftOnly,
    SEG: seed.segKey,
    EXP: seed.expId,
    APPROVAL: seed.approvalId,
    METRIC_KEY: seed.metricKey,
    METRIC_VER: String(seed.metricVersion),
    AI_TYPE: seed.aiType,
    AI_ID: seed.aiId,
    AUDIT_OBJECT: seed.auditObject,
  };
  const expand = p =>
    p.replace(/\$\{(\w+)\}/g, (m, key) => {
      const v = vars[key];
      if (v === undefined || v === null || v === '') throw new Error(`route placeholder ${key} is empty (seed did not produce it)`);
      return encodeURIComponent(v);
    });
  const routes = table.routes.map(r => ({ ...r, url: expand(r.path), hash: '#' + expand(r.path) }));
  // expDetailZones: the four zone tabs + settings, for "experiment detail with
  // the zone tabs" coverage inside the key6 extra passes.
  const expZones = routes.filter(r => r.expZone);
  const key6 = routes.filter(r => r.key6);
  return { routes, key6, expZones };
}
