// Owned synthetic local browser acceptance; no personal identities or payment.
import {createRequire} from 'node:module';
import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import path from 'node:path';
const require=createRequire(new URL('../web/package.json',import.meta.url));
const {chromium}=require('@playwright/test');
const [origin]=process.argv.slice(2);assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/);
const browser=await chromium.launch({headless:true});
try{
 const context=await browser.newContext();const page=await context.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const clickEvent=async(button,kind)=>{const [response]=await Promise.all([page.waitForResponse(r=>r.url()===origin+'/session/event'&&r.request().postDataJSON().kind===kind),page.locator(button).click()]);assert.equal(response.status(),200);return response.json();};
 await page.goto(origin);const [login]=await Promise.all([page.waitForResponse(r=>r.url()===origin+'/idp/session'),page.locator('#login').click()]);assert.equal(login.status(),200);
 for(const scenario of ['activation','upgrade']){
  await page.locator('#scenario').selectOption(scenario);const exposure=await clickEvent('#open','render');assert.equal(exposure.accepted,1,JSON.stringify(exposure));
  await page.locator('#experience').waitFor({state:'visible'});
  const conversion=await clickEvent('#complete','complete');assert.equal(conversion.accepted,1,JSON.stringify(conversion));
  const duplicate=await clickEvent('#complete','complete');assert.equal(duplicate.duplicates,1,JSON.stringify(duplicate));
  if(process.env.GRAYLING_GROWTH_LAB_EVIDENCE){const dir=process.env.GRAYLING_GROWTH_LAB_EVIDENCE;assert.ok(path.isAbsolute(dir));await mkdir(dir,{recursive:true,mode:0o700});await page.screenshot({path:path.join(dir,scenario+'-synthetic.png'),fullPage:true});}
 }
 assert.deepEqual(errors,[]);assert.equal(await page.evaluate(()=>localStorage.length+sessionStorage.length),0);
 const cookies=await context.cookies();assert.ok(cookies.some(c=>c.name==='growth_lab_session'&&c.httpOnly&&c.sameSite==='Strict'));
 console.log('Synthetic browser: visible variants, actual onRender/collect, first task + simulated upgrade, conversion idempotency, opaque IdP cookie: PASS');
}finally{await browser.close();}
