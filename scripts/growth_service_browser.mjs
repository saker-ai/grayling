// Local synthetic acceptance only. Run against an owned disposable service.
import { createRequire } from 'node:module';
import assert from 'node:assert/strict';
const require = createRequire(new URL('../web/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
const [origin, taskId] = process.argv.slice(2);
assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(process.env.GRAYLING_GROWTH_BROWSER_TOKEN);
const browser = await chromium.launch({ headless: true });
try {
 const page = await browser.newPage();
 const errors = []; page.on('pageerror', error => errors.push(error.message));
 await page.goto(origin);
 await page.locator('#token').fill(process.env.GRAYLING_GROWTH_BROWSER_TOKEN);
 await page.getByRole('button', {name:'Connect', exact:true}).click();
 await page.locator('#tasks button').first().waitFor();
 await page.locator('#tasks button').first().click();
 await page.locator('#work').waitFor({state:'visible'});
 await page.locator('#state').filter({hasText:'HOLD'}).waitFor();
 assert.match(await page.locator('#proposal').inputValue(), new RegExp(taskId));
 assert.equal(await page.locator('#task-title img').count(),0);
 assert.equal(await page.evaluate(() => window.injected),undefined);
 // Render malicious goal text as text, and reject through the real UI/API.
 await page.locator('#rationale').fill('Synthetic browser rejection: needs implementation evidence');
 await page.getByRole('button',{name:'Reject proposal',exact:true}).click();
 await page.locator('#state').filter({hasText:'REJECTED'}).waitFor();
 assert.match(await page.locator('#audit').textContent(), /reviewer/);
 assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length),0);
 assert.equal(await page.locator('#upstream').inputValue(),'');
 assert.deepEqual(errors,[]);
 console.log('Browser: real task loading, evidence/proposal display, authenticated rejection and audit, no token storage, no script errors: PASS');
} finally { await browser.close(); }
