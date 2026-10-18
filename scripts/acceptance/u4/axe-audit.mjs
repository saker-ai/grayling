// U4 / R5A.1 — axe accessibility audit.
//   Full pass: every route in routes.json × desktop 1440×1000 × zh × light.
//   Extra passes: the six key pages (experiment detail expands to its five
//   zone tabs) × zh/dark, en/light, en/dark, mobile 390×844 zh/light.
//
// No axe rule is disabled anywhere. Known false positives live in
// axe-exceptions.json (+ docs/acceptance/web-2026-10-18-redesign/axe-exceptions.md)
// and are printed loudly when matched; any other violation exits 1.
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { attachOrStart } from './lib/server.mjs';
import { newContext, closeBrowser, drainProbes, AxeBuilder, AXE_TAGS } from './lib/browser.mjs';
import { seedAll } from './lib/seed.mjs';
import { resolveRoutes } from './lib/routes.mjs';
import { writeJSON, writeText } from './lib/evidence.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const exceptions = JSON.parse(readFileSync(path.join(HERE, 'axe-exceptions.json'), 'utf8'));

const isExcepted = (routeName, combo, ruleId, nodeHtml) =>
  exceptions.some(
    e =>
      e.ruleId === ruleId &&
      (e.route === '*' || e.route === routeName) &&
      (!e.combo || e.combo === combo) &&
      (!e.nodeIncludes || (nodeHtml || '').includes(e.nodeIncludes)),
  );

async function auditRoute(page, url, label, probeLog) {
  await page.goto(url, { waitUntil: 'networkidle' });
  // Let lazy chunks render past the Skeleton fallback.
  await page.waitForSelector('#main h1, #main h2, #main .card, #main table', { timeout: 10_000 }).catch(() => {});
  await page.waitForTimeout(150);
  const results = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
  const probes = await drainProbes(page);
  probeLog.push(...probes.dialogs.map(d => ({ route: label, ...d })));
  probeLog.push(...probes.pageErrors.map(e => ({ route: label, pageError: e })));
  return results;
}

function summarize(allRuns) {
  // rule × route × impact summary rows
  const rows = [];
  for (const run of allRuns) {
    for (const v of run.violations) {
      rows.push({
        rule: v.id,
        impact: v.impact || 'unknown',
        route: run.label,
        combo: run.combo,
        nodes: v.nodes.length,
        excepted: v.excepted || false,
      });
    }
  }
  return rows;
}

function summaryMarkdown(rows, exceptedRows) {
  const lines = [
    '# axe summary (raw rows: axe-results.json)',
    '',
    '| rule | impact | route | combo | nodes |',
    '| --- | --- | --- | --- | --- |',
  ];
  for (const r of rows) lines.push(`| ${r.rule} | ${r.impact} | ${r.route} | ${r.combo} | ${r.nodes} |`);
  if (!rows.length) lines.push('| (none) |  |  |  |  |');
  lines.push('', '## Excepted (documented in axe-exceptions.md)', '');
  lines.push('| rule | impact | route | combo | nodes |');
  lines.push('| --- | --- | --- | --- | --- |');
  for (const r of exceptedRows) lines.push(`| ${r.rule} | ${r.impact} | ${r.route} | ${r.combo} | ${r.nodes} |`);
  if (!exceptedRows.length) lines.push('| (none) |  |  |  |  |');
  lines.push('');
  return lines.join('\n');
}

const main = async () => {
  const server = await attachOrStart();
  const seed = await seedAll(server.baseURL);
  const { routes, key6, expZones } = resolveRoutes(seed);

  const combos = [
    { name: 'desktop-zh-light', viewport: 'desktop', lang: 'zh', theme: 'light' },
    { name: 'desktop-zh-dark', viewport: 'desktop', lang: 'zh', theme: 'dark' },
    { name: 'desktop-en-light', viewport: 'desktop', lang: 'en', theme: 'light' },
    { name: 'desktop-en-dark', viewport: 'desktop', lang: 'en', theme: 'dark' },
    { name: 'mobile-zh-light', viewport: 'mobile', lang: 'zh', theme: 'light' },
  ];
  // Full pass visits every route in the first combo; the key6 set (with the
  // experiment zones expanded) is re-visited under the remaining combos.
  const key6Expanded = [];
  for (const r of key6) {
    if (r.expZone) key6Expanded.push(...expZones);
    else key6Expanded.push(r);
  }
  const plan = [
    ...routes.map(r => ({ route: r, combo: combos[0] })),
    ...combos.slice(1).flatMap(c => key6Expanded.map(r => ({ route: r, combo: c }))),
  ];

  const probeLog = [];
  const allRuns = [];
  const rawResults = [];
  try {
    for (const { route, combo } of plan) {
      const { context } = await newContext({ ...combo, baseURL: server.baseURL });
      const page = await context.newPage();
      const label = route.name;
      const comboName = combo.name;
      const results = await auditRoute(page, server.baseURL + '/' + route.hash, `${label}`, probeLog);
      const violations = results.violations.map(v => ({
        ...v,
        nodes: v.nodes.map(n => ({ html: n.html.slice(0, 300), target: n.target, failureSummary: n.failureSummary })),
      }));
      for (const v of violations) {
        v.excepted = v.nodes.some(n => isExcepted(route.name, comboName, v.id, n.html));
      }
      allRuns.push({ label, combo: comboName, violations: violations.filter(v => !v.excepted) });
      rawResults.push({
        route: label,
        combo: comboName,
        url: route.hash,
        violations,
        passes: results.passes.length,
        incomplete: results.incomplete.map(v => ({ id: v.id, nodes: v.nodes.length })),
      });
      const open = violations.filter(v => !v.excepted).length;
      console.log(`[axe] ${comboName} ${label}: ${open} violation(s)` + (violations.length !== open ? ` (${violations.length - open} excepted)` : ''));
      await context.close();
    }
  } finally {
    await closeBrowser();
    await server.stop();
  }

  // Native dialogs / page errors are hard failures regardless of axe.
  const probeFailures = probeLog;
  const flat = [];
  const exceptedFlat = [];
  for (const run of allRuns) {
    for (const v of run.violations) {
      flat.push({ rule: v.id, impact: v.impact || 'unknown', route: run.label, combo: run.combo, nodes: v.nodes.length });
    }
  }
  for (const run of rawResults) {
    for (const v of run.violations.filter(v => v.excepted)) {
      exceptedFlat.push({ rule: v.id, impact: v.impact || 'unknown', route: run.route, combo: run.combo, nodes: v.nodes.length });
    }
  }

  writeJSON('axe-results.json', {
    generatedAt: new Date().toISOString(),
    tags: AXE_TAGS,
    runs: rawResults,
    probeFailures,
  });
  writeText('axe-summary.md', summaryMarkdown(flat, exceptedFlat));
  writeJSON('axe-summary.json', { open: flat, excepted: exceptedFlat, probeFailures });

  if (exceptedFlat.length) {
    console.log('[axe] excepted violations (documented):');
    for (const e of exceptedFlat) console.log(`  - ${e.rule} @ ${e.route} (${e.combo})`);
  }
  if (probeFailures.length) {
    console.error('[axe] native dialog / page error probes fired:');
    for (const p of probeFailures) console.error('  -', JSON.stringify(p));
  }
  if (flat.length || probeFailures.length) {
    console.error(`[axe] FAIL: ${flat.length} open violation row(s), ${probeFailures.length} probe failure(s)`);
    process.exit(1);
  }
  console.log('[axe] PASS: zero open violations, zero native dialogs, zero page errors');
};

main().catch(e => {
  console.error(e);
  process.exit(1);
});
