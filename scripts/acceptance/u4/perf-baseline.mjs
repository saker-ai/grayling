// U4 / R5A.2 — performance baseline + budget assertions.
//
// Measurements (methodology: docs/acceptance/web-2026-10-18-redesign/perf-methodology.md):
//   * FCP / LCP / DOMContentLoaded / load / TTI-proxy on the six key pages,
//     cold (fresh context per iteration) and warm (primed context), 5
//     iterations each, median reported.
//   * 1000-flag list: filter input event → list DOM update completion, 5
//     iterations, median.
//   * Experiment report page: longtask entries (>50ms) count + total duration.
//
// Budget assertions are HARD: any breach exits non-zero.
//   * web/static/assets/index-*.js  ≤ 460 KiB raw
//   * web/static/assets/index-*.css ≤ 32 KiB raw
//   * web-pages/src/viz/ sources    ≤ 15 KiB total
//   * pnpm-lock.yaml importers.web.dependencies unchanged vs HEAD~
//     (devDependencies may gain @playwright/test + @axe-core/playwright)
import { execFileSync } from 'node:child_process';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { attachOrStart } from './lib/server.mjs';
import { newContext, closeBrowser } from './lib/browser.mjs';
import { seedAll, bulkFlags, SEED } from './lib/seed.mjs';
import { resolveRoutes } from './lib/routes.mjs';
import { writeJSON, writeText, median, EVIDENCE_DIR } from './lib/evidence.mjs';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, '..', '..', '..');

const KIB = 1024;
const BUDGETS = {
  mainJsBytes: 460 * KIB,
  cssBytes: 32 * KIB,
  vizSrcBytes: 15 * KIB,
};

const ITERATIONS = 5;

// Collector injected before page scripts: buffers paint/LCP entries.
const PERF_INIT = `
(() => {
  window.__u4Perf = { fcp: null, lcp: null };
  try {
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) if (e.name === 'first-contentful-paint') window.__u4Perf.fcp = e.startTime;
    }).observe({ type: 'paint', buffered: true });
    new PerformanceObserver(list => {
      const entries = list.getEntries();
      if (entries.length) window.__u4Perf.lcp = entries[entries.length - 1].startTime;
    }).observe({ type: 'largest-contentful-paint', buffered: true });
  } catch { /* observer unsupported */ }
})();
`;

async function measurePage(context, url) {
  const page = await context.newPage();
  await page.goto(url, { waitUntil: 'load' });
  // TTI proxy: load event + main-chunk settled, then two rAFs — see
  // perf-methodology.md. LCP needs a beat past load to flush.
  const timing = await page.evaluate(
    () =>
      new Promise(resolve => {
        requestAnimationFrame(() =>
          requestAnimationFrame(() => {
            const nav = performance.getEntriesByType('navigation')[0] || {};
            resolve({
              dcl: nav.domContentLoadedEventEnd ?? null,
              load: nav.loadEventEnd ?? null,
              ttiProxy: performance.now(),
              fcp: window.__u4Perf?.fcp ?? null,
              lcp: window.__u4Perf?.lcp ?? null,
            });
          }),
        );
      }),
  );
  // LCP keeps updating until input; give it one more beat and re-read. FCP is
  // re-read too: the paint observer callback may land after the rAF pair.
  await page.waitForTimeout(400);
  const paint = await page.evaluate(() => ({ fcp: window.__u4Perf?.fcp ?? null, lcp: window.__u4Perf?.lcp ?? null }));
  await page.close();
  return { ...timing, fcp: timing.fcp ?? paint.fcp, lcp: paint.lcp };
}

async function measureKey6(baseURL, routes, log) {
  const pages = [
    { name: 'overview', hash: routes.find(r => r.name === 'overview').hash },
    { name: 'flags', hash: routes.find(r => r.name === 'flags').hash },
    { name: 'flag-detail', hash: routes.find(r => r.name === 'flag-detail').hash },
    { name: 'experiments', hash: routes.find(r => r.name === 'experiments').hash },
    { name: 'exp-detail', hash: routes.find(r => r.name === 'exp-summary').hash },
    { name: 'ai', hash: routes.find(r => r.name === 'ai').hash },
  ];
  const out = {};
  for (const p of pages) {
    const url = baseURL + '/' + p.hash;
    const cold = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL });
      await context.addInitScript(PERF_INIT);
      cold.push(await measurePage(context, url));
      await context.close();
    }
    const warm = [];
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL });
      await context.addInitScript(PERF_INIT);
      await measurePage(context, url); // priming visit (not measured)
      for (let i = 0; i < ITERATIONS; i++) {
        const page = await context.newPage();
        await page.goto('about:blank');
        await page.close();
        warm.push(await measurePage(context, url));
      }
      await context.close();
    }
    out[p.name] = {
      cold: Object.fromEntries(['fcp', 'lcp', 'dcl', 'load', 'ttiProxy'].map(k => [k, median(cold.map(r => r[k]).filter(v => v !== null))])),
      warm: Object.fromEntries(['fcp', 'lcp', 'dcl', 'load', 'ttiProxy'].map(k => [k, median(warm.map(r => r[k]).filter(v => v !== null))])),
      coldRuns: cold,
      warmRuns: warm,
    };
    log(`[perf] ${p.name}: cold fcp=${out[p.name].cold.fcp?.toFixed(0)}ms lcp=${out[p.name].cold.lcp?.toFixed(0)}ms warm fcp=${out[p.name].warm.fcp?.toFixed(0)}ms lcp=${out[p.name].warm.lcp?.toFixed(0)}ms`);
  }
  return out;
}

// Filter latency: input event → list DOM update completion (double-rAF quiet
// window after the last mutation under #main).
async function measureFilterLatency(context, url, query, log) {
  const page = await context.newPage();
  await page.goto(url, { waitUntil: 'networkidle' });
  await page.waitForSelector('input.list-search', { timeout: 15_000 });
  const result = await page.evaluate(async q => {
    const input = document.querySelector('input.list-search');
    if (!input) return { error: 'no input.list-search' };
    const rowCount = () => document.querySelectorAll('#main table tbody tr, #main [data-mobile-cards] > *').length;
    const t0 = performance.now();
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    const done = new Promise(resolve => {
      let lastMutation = 0;
      const mo = new MutationObserver(() => {
        lastMutation = performance.now();
      });
      mo.observe(document.getElementById('main'), { childList: true, subtree: true });
      const settle = () => {
        requestAnimationFrame(() =>
          requestAnimationFrame(() => {
            // Quiet check: no mutation during the last two frames.
            if (performance.now() - lastMutation < 32 && lastMutation !== 0) {
              mo.disconnect();
              resolve(performance.now());
            } else {
              settle();
            }
          }),
        );
      };
      settle();
      setTimeout(() => {
        mo.disconnect();
        resolve(performance.now());
      }, 5000);
    });
    setter.call(input, q);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    const t1 = await done;
    return { latencyMs: t1 - t0, rows: rowCount() };
  }, query);
  await page.close();
  return result;
}

async function measureLongtasks(baseURL, hash, log) {
  const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL });
  await context.addInitScript(`
    window.__u4Longtasks = [];
    try {
      new PerformanceObserver(list => {
        for (const e of list.getEntries()) window.__u4Longtasks.push({ duration: e.duration, startTime: e.startTime });
      }).observe({ type: 'longtask', buffered: true });
    } catch { /* unsupported */ }
  `);
  const page = await context.newPage();
  await page.goto(baseURL + '/' + hash, { waitUntil: 'networkidle' });
  await page.waitForTimeout(3000);
  const tasks = await page.evaluate(() => window.__u4Longtasks || []);
  await context.close();
  const over50 = tasks.filter(t => t.duration > 50);
  const total = over50.reduce((s, t) => s + t.duration, 0);
  log(`[perf] longtasks on report page: ${over50.length} entries, ${total.toFixed(1)}ms total`);
  return { count: over50.length, totalMs: total, entries: over50 };
}

// --- budget assertions -------------------------------------------------------

function checkBundleBudgets(log) {
  const failures = [];
  const assetsDir = path.join(REPO_ROOT, 'web', 'static', 'assets');
  const assets = readdirSync(assetsDir);
  const mainJs = assets.filter(f => /^index-.*\.js$/.test(f));
  const css = assets.filter(f => /^index-.*\.css$/.test(f));
  const readings = {};
  for (const f of mainJs) {
    const size = statSync(path.join(assetsDir, f)).size;
    readings[f] = size;
    if (size > BUDGETS.mainJsBytes) failures.push(`${f}: ${size} > ${BUDGETS.mainJsBytes}`);
  }
  for (const f of css) {
    const size = statSync(path.join(assetsDir, f)).size;
    readings[f] = size;
    if (size > BUDGETS.cssBytes) failures.push(`${f}: ${size} > ${BUDGETS.cssBytes}`);
  }
  // viz sources: exact sum of file bytes.
  const vizDir = path.join(REPO_ROOT, 'web-pages', 'src', 'viz');
  let vizBytes = 0;
  const walk = d => {
    for (const e of readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else vizBytes += statSync(p).size;
    }
  };
  walk(vizDir);
  readings['web-pages/src/viz/**'] = vizBytes;
  if (vizBytes > BUDGETS.vizSrcBytes) failures.push(`viz sources: ${vizBytes} > ${BUDGETS.vizSrcBytes}`);
  for (const [k, v] of Object.entries(readings)) log(`[budget] ${k}: ${v} bytes`);
  return { readings, failures };
}

// Lockfile dependency guard: the web importer's runtime dependencies must be
// identical to HEAD~ (devDependencies may grow by exactly the two harness
// packages). Parsed with indentation rules — enough for pnpm-lock's layout.
function lockfileWebDeps(text) {
  const lines = text.split('\n');
  let inWeb = false;
  let inDeps = false;
  const deps = [];
  for (const line of lines) {
    if (/^ {2}web:/.test(line)) {
      inWeb = true;
      inDeps = false;
      continue;
    }
    if (inWeb && /^ {2}\S/.test(line)) break; // next importer
    if (inWeb && /^ {4}dependencies:/.test(line)) {
      inDeps = true;
      continue;
    }
    if (inWeb && inDeps && /^ {4}\S/.test(line)) break; // devDependencies etc.
    if (inWeb && inDeps && /^ {6}'?([^\']+?)'?:/.test(line)) {
      deps.push(line.trim().replace(/:$/, '').replace(/^'|'$/g, ''));
    }
  }
  return deps.sort();
}

function checkLockfileBudget(log) {
  let before;
  try {
    before = execFileSync('git', ['show', 'HEAD~:pnpm-lock.yaml'], { cwd: REPO_ROOT, encoding: 'utf8' });
  } catch {
    before = execFileSync('git', ['show', 'HEAD:pnpm-lock.yaml'], { cwd: REPO_ROOT, encoding: 'utf8' });
  }
  const now = readFileSync(path.join(REPO_ROOT, 'pnpm-lock.yaml'), 'utf8');
  const a = lockfileWebDeps(before);
  const b = lockfileWebDeps(now);
  const added = b.filter(d => !a.includes(d));
  const removed = a.filter(d => !b.includes(d));
  log(`[budget] lockfile web runtime deps: ${b.length} (added: ${added.length}, removed: ${removed.length})`);
  const failures = [];
  if (added.length) failures.push(`lockfile runtime deps added vs HEAD~: ${added.join(', ')}`);
  if (removed.length) failures.push(`lockfile runtime deps removed vs HEAD~: ${removed.join(', ')}`);
  return { before: a, after: b, added, removed, failures };
}

const main = async () => {
  const log = console.log;
  const budgetResults = [];
  const bundle = checkBundleBudgets(log);
  const lock = checkLockfileBudget(log);
  budgetResults.push(...bundle.failures, ...lock.failures);

  const server = await attachOrStart();
  let perf = {};
  let filterLatency = null;
  let longtasks = null;
  try {
    const seed = await seedAll(server.baseURL);
    const { routes } = resolveRoutes(seed);
    perf = await measureKey6(server.baseURL, routes, log);

    // 1000-flag list scenario — seeded AFTER the six-page medians so those
    // numbers reflect a typical scope, not the stress scope.
    await bulkFlags(server.baseURL, 1000, log);
    const flagsHash = routes.find(r => r.name === 'flags').hash;
    const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
    await measurePage(context, server.baseURL + '/' + flagsHash); // prime
    const latencies = [];
    const queries = ['u4-bulk-077', 'u4-bulk-0999', 'u4-bulk-1', 'u4-json', 'u4-bulk-05'];
    for (const q of queries) {
      const r = await measureFilterLatency(context, server.baseURL + '/' + flagsHash, q, log);
      log(`[perf] filter '${q}': ${r.latencyMs?.toFixed(1)}ms → ${r.rows} row(s)`);
      latencies.push({ query: q, ...r });
    }
    await context.close();
    filterLatency = { iterations: latencies, medianMs: median(latencies.map(l => l.latencyMs)) };

    longtasks = await measureLongtasks(server.baseURL, routes.find(r => r.name === 'exp-evidence').hash, log);
  } finally {
    await closeBrowser();
    await server.stop();
  }

  writeJSON('perf-results.json', {
    generatedAt: new Date().toISOString(),
    iterations: ITERATIONS,
    key6: perf,
    filterLatency1000: filterLatency,
    reportLongtasks: longtasks,
    budgets: {
      limits: BUDGETS,
      readings: bundle.readings,
      lockfile: { added: lock.added, removed: lock.removed, runtimeDeps: lock.after },
      failures: budgetResults,
    },
  });

  const md = ['# perf summary (raw: perf-results.json)', ''];
  md.push('| page | cold FCP | cold LCP | cold load | cold TTI-proxy | warm FCP | warm LCP | warm load | warm TTI-proxy |');
  md.push('| --- | --- | --- | --- | --- | --- | --- | --- | --- |');
  for (const [name, m] of Object.entries(perf)) {
    const f = v => (v === null || v === undefined ? '—' : v.toFixed(0) + 'ms');
    md.push(`| ${name} | ${f(m.cold.fcp)} | ${f(m.cold.lcp)} | ${f(m.cold.load)} | ${f(m.cold.ttiProxy)} | ${f(m.warm.fcp)} | ${f(m.warm.lcp)} | ${f(m.warm.load)} | ${f(m.warm.ttiProxy)} |`);
  }
  md.push('');
  md.push(`- 1000-flag filter latency median: ${filterLatency?.medianMs?.toFixed(1)}ms (n=${filterLatency?.iterations.length})`);
  md.push(`- experiment report longtasks: ${longtasks?.count} entries >50ms, total ${longtasks?.totalMs.toFixed(1)}ms`);
  md.push('');
  md.push('## budgets');
  md.push('');
  for (const [k, v] of Object.entries(bundle.readings)) {
    const limit = k.endsWith('.js') ? BUDGETS.mainJsBytes : k.endsWith('.css') ? BUDGETS.cssBytes : BUDGETS.vizSrcBytes;
    md.push(`- ${k}: ${v} / ${limit} bytes ${v <= limit ? 'PASS' : 'FAIL'}`);
  }
  md.push(`- lockfile runtime deps: added=${lock.added.length} removed=${lock.removed.length} ${lock.failures.length ? 'FAIL' : 'PASS'}`);
  md.push('');
  writeText('perf-summary.md', md.join('\n'));

  if (budgetResults.length) {
    console.error('[budget] FAIL:');
    for (const f of budgetResults) console.error('  - ' + f);
    process.exit(1);
  }
  console.log('[perf] PASS: budgets hold');
};

main().catch(e => {
  console.error(e);
  process.exit(1);
});
