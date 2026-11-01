import {createRequire} from 'node:module';
import assert from 'node:assert/strict';
const require=createRequire(new URL('../web/package.json',import.meta.url));const {chromium}=require('@playwright/test');
const [origin]=process.argv.slice(2);assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/);
const scenario=JSON.parse(process.env.GRAYLING_GROWTH_LAB_SCENARIO);
const browser=await chromium.launch({headless:true});
try{
 const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));await page.goto(origin);
 await page.locator('#token').fill(process.env.GRAYLING_GROWTH_BROWSER_TOKEN);
 await page.locator('#goal').fill(JSON.stringify({goal:scenario.goal,baseline:scenario.baseline}));await page.locator('#key').fill('browser-design-synthetic');await page.locator('#create').click();await page.locator('#state').filter({hasText:'DIAGNOSED'}).waitFor();
 await page.getByText('Select a hypothesis and design the experiment',{exact:true}).click();
 const design={suggestionIndex:0,flagKey:scenario.flag,unit:'user_id',audience:'synthetic new users',exposureTrigger:'visible sample',controlAllocationPercent:50,implementation:[],rollbackConditions:['HOLD unhealthy data']};
 await page.locator('#design-input').fill(JSON.stringify(design));await page.locator('#design').click();await page.waitForFunction(()=>JSON.parse(document.querySelector('#proposal').value).hypothesis.includes('Synthetic clearer'));
 await page.locator('#sample-plan').click();await page.waitForFunction(()=>JSON.parse(document.querySelector('#proposal').value).samplePlan.minSamplePerArm>100);
 await page.locator('#rationale').fill('Synthetic browser request for implementation evidence');await page.locator('#changes').click();await page.locator('#state').filter({hasText:'CHANGES_REQUESTED'}).waitFor();
 await page.getByText('Review bundle and measurement preparation',{exact:true}).click();await page.locator('#study-export').click();await page.locator('#study-view').filter({hasText:'growth.study.v1'}).waitFor();
 assert.equal(JSON.parse(await page.locator('#study-view').textContent()).independentHumanVerified,false);assert.deepEqual(errors,[]);
 console.log('Workbench: finite baseline goal, actual diagnosis, hypothesis selection, deterministic sample plan, request changes, blind bundle: PASS');
}finally{await browser.close();}
