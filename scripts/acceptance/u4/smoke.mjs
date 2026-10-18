// Internal smoke check: server + seed + one page + one axe run. Not part of
// the acceptance gate; used while developing the harness.
import { attachOrStart } from './lib/server.mjs';
import { newContext, closeBrowser, AxeBuilder, AXE_TAGS, drainProbes } from './lib/browser.mjs';
import { seedAll } from './lib/seed.mjs';
import { resolveRoutes } from './lib/routes.mjs';

const main = async () => {
  const server = await attachOrStart();
  try {
    const seed = await seedAll(server.baseURL);
    console.log('SEED OK', JSON.stringify(seed));
    const { routes } = resolveRoutes(seed);
    console.log('ROUTES', routes.length);
    const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
    const page = await context.newPage();
    await page.goto(server.baseURL + '/' + routes[0].hash, { waitUntil: 'networkidle' });
    await page.waitForTimeout(400);
    console.log('TITLE', await page.title());
    console.log('H1/H2', await page.locator('#main h1, #main h2').allTextContents());
    const results = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
    console.log('AXE violations', results.violations.length, results.violations.map(v => v.id).join(','));
    console.log('PROBES', JSON.stringify(await drainProbes(page)));
    await context.close();
  } finally {
    await closeBrowser();
    await server.stop();
  }
};

main().catch(e => {
  console.error(e);
  process.exit(1);
});
