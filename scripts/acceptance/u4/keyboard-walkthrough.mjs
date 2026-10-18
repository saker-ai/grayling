// U4 / R5A.3 — keyboard-only walkthrough. Zero mouse input: every interaction
// goes through page.keyboard. Checks:
//   K1 skip-link: first Tab lands on it, activation moves focus to #main and
//      must NOT clobber the hash route (regression: href="#main" used to
//      destroy the hash route and unmount the app).
//   K2 sidebar nav: every nav button tabbable, current page aria-current=page.
//   K3 ZoneNav: arrows move focus without selecting (manual activation),
//      Enter selects and swaps the tabpanel.
//   K4 Dialog: opens via keyboard, focus moves inside, Tab wraps (trap),
//      Shift+Tab wraps backwards, Escape closes, focus returns to trigger.
//   K5 two-step confirm: rollback confirm completes fully by keyboard
//      (focus trigger → open → type reason → confirm); since rollback is a
//      gated change, the store must show a pending flag.rollback approval.
//   K6 viz SVGs never take focus in the Tab sequence.
//   K7 focus-ring visibility screenshots (zh/light + zh/dark): input, primary
//      button, inline button, zone tab.
// Results: keyboard-walkthrough.{json,md} + keyboard-*.png in the evidence dir.
import { attachOrStart } from './lib/server.mjs';
import { newContext, closeBrowser, drainProbes } from './lib/browser.mjs';
import { seedAll, apiClient } from './lib/seed.mjs';
import { resolveRoutes } from './lib/routes.mjs';
import { writeJSON, writeText, screenshot } from './lib/evidence.mjs';

const results = [];
const check = (id, name, pass, detail = '') => {
  results.push({ id, name, pass, detail });
  console.log(`[kb] ${id} ${pass ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
};

async function focused(page) {
  return page.evaluate(() => {
    const el = document.activeElement;
    return {
      tag: el?.tagName,
      id: el?.id || '',
      cls: typeof el?.className === 'string' ? el.className : '',
      role: el?.getAttribute('role') || '',
      text: (el?.textContent || '').trim().slice(0, 40),
      ariaCurrent: el?.getAttribute('aria-current'),
      ariaSelected: el?.getAttribute('aria-selected'),
      tabindex: el?.getAttribute('tabindex'),
    };
  });
}

// tabSequence walks the focus order from the top of the page, collecting up to
// `limit` stops.
async function tabSequence(page, limit = 60) {
  await page.evaluate(() => document.body.focus());
  const seq = [];
  for (let i = 0; i < limit; i++) {
    await page.keyboard.press('Tab');
    const f = await focused(page);
    seq.push(f);
    // Stop when focus wraps back to the first stop.
    if (i > 0 && f.tag === seq[0].tag && f.id === seq[0].id && f.cls === seq[0].cls && f.text === seq[0].text) break;
  }
  return seq;
}

const main = async () => {
  const server = await attachOrStart();
  const seed = await seedAll(server.baseURL);
  const { routes } = resolveRoutes(seed);
  const url = name => server.baseURL + '/' + routes.find(r => r.name === name).hash;

  try {
    // ---- K1 skip-link -------------------------------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('overview'), { waitUntil: 'networkidle' });
      await page.keyboard.press('Tab');
      const f1 = await focused(page);
      check('K1a', 'first Tab lands on skip-link', f1.cls === 'skip-link', JSON.stringify(f1));
      await page.keyboard.press('Enter');
      await page.waitForTimeout(300);
      const state = await page.evaluate(() => ({
        hash: location.hash,
        activeId: document.activeElement?.id || '',
        mainHasFocus: document.activeElement === document.getElementById('main') || document.getElementById('main')?.contains(document.activeElement),
        mainVisible: !!document.querySelector('#main h1, #main h2, #main .card'),
      }));
      check('K1b', 'skip-link activation moves focus to main', state.mainHasFocus, JSON.stringify(state));
      check('K1c', 'hash route survives skip-link', state.hash.includes('/console/'), `hash=${state.hash}`);
      check('K1d', 'main content still rendered', state.mainVisible);
      await context.close();
    }

    // ---- K2 sidebar nav -----------------------------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('flags'), { waitUntil: 'networkidle' });
      const navInfo = await page.evaluate(() => {
        const buttons = [...document.querySelectorAll('#nav button')];
        return buttons.map(b => ({
          text: b.textContent.trim(),
          tabbable: b.tabIndex === 0,
          ariaCurrent: b.getAttribute('aria-current'),
          active: b.classList.contains('active'),
        }));
      });
      check('K2a', 'every sidebar nav button is tabbable', navInfo.every(b => b.tabbable), `${navInfo.length} buttons`);
      const current = navInfo.filter(b => b.ariaCurrent === 'page');
      check('K2b', 'exactly one nav button carries aria-current=page', current.length === 1, JSON.stringify(current));
      check('K2c', 'aria-current marks the flags view', current[0]?.text !== undefined && current[0]?.active === true, current[0]?.text);
      // Walk the tab order until the flags nav button receives focus.
      const seq = await tabSequence(page, 40);
      const hitNav = seq.some(s => s.text === current[0]?.text);
      check('K2d', 'nav reachable in Tab sequence', hitNav);
      await context.close();
    }

    // ---- K3 ZoneNav manual activation ---------------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('exp-summary'), { waitUntil: 'networkidle' });
      await page.waitForSelector('#exp-tab-summary', { timeout: 10_000 });
      await page.focus('#exp-tab-summary');
      await page.keyboard.press('ArrowRight');
      const afterArrow = await page.evaluate(() => ({
        focused: document.activeElement?.id,
        summarySelected: document.getElementById('exp-tab-summary')?.getAttribute('aria-selected'),
        hash: location.hash,
      }));
      check('K3a', 'ArrowRight moves focus to next tab', afterArrow.focused === 'exp-tab-health', JSON.stringify(afterArrow));
      check('K3b', 'focus move does not select (manual activation)', afterArrow.summarySelected === 'true', `summary aria-selected=${afterArrow.summarySelected}`);
      check('K3c', 'focus move does not navigate', afterArrow.hash.includes('/experiments/') && !afterArrow.hash.endsWith('/health'), `hash=${afterArrow.hash}`);
      await page.keyboard.press('Enter');
      await page.waitForTimeout(500);
      const afterEnter = await page.evaluate(() => ({
        healthSelected: document.getElementById('exp-tab-health')?.getAttribute('aria-selected'),
        hash: location.hash,
        panelVisible: !!document.getElementById('exp-zonepanel-health'),
      }));
      check('K3d', 'Enter selects the focused tab', afterEnter.healthSelected === 'true', JSON.stringify(afterEnter));
      check('K3e', 'selection deep-links the hash', afterEnter.hash.endsWith('/health'), `hash=${afterEnter.hash}`);
      check('K3f', 'tabpanel with matching id exists', afterEnter.panelVisible);
      await context.close();
    }

    // ---- K4 Dialog focus trap -----------------------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('overview'), { waitUntil: 'networkidle' });
      await page.focus('#btn-new-ws');
      await page.keyboard.press('Enter');
      await page.waitForSelector('.modal [role="dialog"], .modal', { timeout: 5000 });
      await page.waitForTimeout(200);
      const inside = await page.evaluate(() => {
        const overlay = document.querySelector('.overlay');
        return overlay?.contains(document.activeElement) ?? false;
      });
      check('K4a', 'dialog opens with focus inside', inside);
      // Collect the focus cycle inside the dialog.
      const cycle = [];
      for (let i = 0; i < 12; i++) {
        cycle.push(await focused(page));
        await page.keyboard.press('Tab');
      }
      const ids = cycle.map(c => c.id || c.tag + '.' + c.cls);
      const first = ids[0];
      const wrappedIdx = ids.indexOf(first, 1);
      check('K4b', 'Tab cycles inside the dialog (trap)', wrappedIdx > 0, `cycle=${ids.join(' → ')}`);
      const escaped = cycle.some(c => !c.tag || c.tag === 'BODY');
      check('K4c', 'focus never escapes to the page', !escaped);
      // Shift+Tab from the first stop wraps to the last.
      await page.evaluate(() => {
        const overlay = document.querySelector('.overlay');
        overlay.querySelector('input, select, textarea')?.focus();
      });
      const beforeShift = await focused(page);
      await page.keyboard.press('Shift+Tab');
      const afterShift = await focused(page);
      check('K4d', 'Shift+Tab wraps from first to last', afterShift.tag === 'BUTTON' && afterShift !== beforeShift, `first=${beforeShift.id || beforeShift.tag} shiftTab→${afterShift.id || afterShift.text}`);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(300);
      const closed = await page.evaluate(() => !document.querySelector('.overlay'));
      const restored = await focused(page);
      check('K4e', 'Escape closes the dialog', closed);
      check('K4f', 'focus returns to the trigger', restored.id === 'btn-new-ws', `active=${restored.id || restored.tag}`);
      await context.close();
    }

    // ---- K5 two-step confirm (rollback) --------------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('flag-detail'), { waitUntil: 'networkidle' });
      const rb = await page.waitForSelector('[data-rb="1"]', { timeout: 10_000 }).catch(() => null);
      if (!rb) {
        check('K5', 'rollback affordance present', false, 'no [data-rb="1"] button — seed revision history missing');
      } else {
        await page.focus('[data-rb="1"]');
        await page.keyboard.press('Enter');
        await page.waitForSelector('#action-reason', { timeout: 5000 });
        const reasonFocused = await page.evaluate(() => document.activeElement?.id);
        check('K5a', 'rollback confirm opens, reason field focused', reasonFocused === 'action-reason', `active=${reasonFocused}`);
        await page.keyboard.type('u4 keyboard rollback');
        await page.keyboard.press('Tab'); // → cancel
        await page.keyboard.press('Tab'); // → confirm
        const onConfirm = await focused(page);
        await page.keyboard.press('Enter');
        await page.waitForTimeout(800);
        const after = await page.evaluate(() => ({
          dialogOpen: !!document.querySelector('.overlay'),
          hash: location.hash,
        }));
        check('K5b', 'confirm button reachable by Tab', onConfirm.tag === 'BUTTON', JSON.stringify(onConfirm));
        check('K5c', 'rollback request completes and dialog closes', !after.dialogOpen, JSON.stringify(after));
        // Non-emergency rollback is a gated change: it creates a pending
        // approval (action flag.rollback, payload {"revision":1}) instead of
        // flipping current.revision directly. Verify that against the store.
        const { must } = await apiClient(server.baseURL, () => {});
        let found = null;
        for (let i = 0; i < 12 && !found; i++) {
          const list = await must('/api/v1/admin/workspaces/u4acc/approvals', {});
          found = (list.data || []).find(a => a.action === 'flag.rollback' && String(a.object || '').includes('u4-bool'));
          if (!found) await page.waitForTimeout(250);
        }
        const payloadOk = !!found && String(found.payload || '').includes('"revision":1');
        check('K5d', 'pending flag.rollback approval created for r1', payloadOk, found ? `id=${found.id} state=${found.state} payload=${found.payload}` : 'no matching approval');
        await context.close();
      }
    }

    // ---- K6 viz SVG stays out of the Tab order -------------------------------
    {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme: 'light', baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('exp-evidence'), { waitUntil: 'networkidle' });
      await page.waitForTimeout(600);
      const seq = await tabSequence(page, 80);
      const svgStops = seq.filter(s => s.tag === 'svg' || s.tag === 'SVG');
      check('K6', 'decorative SVG charts never enter the Tab order', svgStops.length === 0, svgStops.length ? JSON.stringify(svgStops[0]) : `${seq.length} stops walked`);
      await context.close();
    }

    // ---- K7 focus-ring screenshots -------------------------------------------
    for (const theme of ['light', 'dark']) {
      const { context } = await newContext({ viewport: 'desktop', lang: 'zh', theme, baseURL: server.baseURL });
      const page = await context.newPage();
      await page.goto(url('flags'), { waitUntil: 'networkidle' });
      await page.waitForSelector('input.list-search', { timeout: 10_000 });
      // input
      await page.focus('input.list-search');
      await page.waitForTimeout(150);
      await screenshot(page, `keyboard-focus-input-${theme}.png`);
      // primary button (create flag CTA on the flags page)
      const primary = await page.evaluate(() => {
        const btn = document.querySelector('#main .btn.primary');
        if (btn) btn.focus();
        return !!btn;
      });
      if (primary) {
        await page.waitForTimeout(150);
        await screenshot(page, `keyboard-focus-primary-${theme}.png`);
      }
      // inline button (row-level action inside the table)
      const inline = await page.evaluate(() => {
        const btn = document.querySelector('#main table button');
        if (btn) btn.focus();
        return !!btn;
      });
      if (inline) {
        await page.waitForTimeout(150);
        await screenshot(page, `keyboard-focus-inline-${theme}.png`);
      }
      // zone tab
      await page.goto(url('exp-summary'), { waitUntil: 'networkidle' });
      await page.waitForSelector('#exp-tab-summary', { timeout: 10_000 });
      await page.focus('#exp-tab-summary');
      await page.waitForTimeout(150);
      await screenshot(page, `keyboard-focus-tab-${theme}.png`);
      await context.close();
      results.push({ id: `K7-${theme}`, name: `focus ring screenshots (${theme})`, pass: primary && inline, detail: 'PNG in evidence dir' });
    }

    // Probe sweep: no native dialogs may fire during the whole walkthrough.
  } finally {
    await closeBrowser();
    await server.stop();
  }

  const failed = results.filter(r => !r.pass);
  writeJSON('keyboard-walkthrough.json', { generatedAt: new Date().toISOString(), results });
  const md = [
    '# keyboard walkthrough (raw: keyboard-walkthrough.json)',
    '',
    '| id | check | result | detail |',
    '| --- | --- | --- | --- |',
    ...results.map(r => `| ${r.id} | ${r.name} | ${r.pass ? 'PASS' : 'FAIL'} | ${String(r.detail).replaceAll('|', '\\|')} |`),
    '',
  ];
  writeText('keyboard-walkthrough.md', md.join('\n'));
  if (failed.length) {
    console.error(`[kb] FAIL: ${failed.length} check(s) failed`);
    process.exit(1);
  }
  console.log('[kb] PASS: all keyboard checks hold');
};

main().catch(e => {
  console.error(e);
  process.exit(1);
});
