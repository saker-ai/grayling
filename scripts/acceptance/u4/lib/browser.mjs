// U4 acceptance harness — Playwright browser/context factory.
// Every context carries the acceptance probes:
//   * window.__u4Dialogs    — native alert/confirm/prompt calls (any call = failure)
//   * window.__u4PageErrors — window.onerror / unhandledrejection messages
//   * localStorage grayling_lang / grayling_theme presets
// plus page-level 'pageerror' and console 'error' capture for belt and braces.
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = path.resolve(HERE, '..', '..', '..', '..');

// The harness is plain Node (no bundler); @playwright/test and
// @axe-core/playwright live in web/node_modules as devDependencies. Binding
// createRequire to web/package.json resolves them through the pnpm workspace
// without NODE_PATH.
const requireFromWeb = createRequire(path.join(REPO_ROOT, 'web', 'package.json'));
export const { chromium } = requireFromWeb('@playwright/test');
export const { AxeBuilder } = requireFromWeb('@axe-core/playwright');

export const VIEWPORTS = {
  desktop: { width: 1440, height: 1000 },
  mobile: { width: 390, height: 844 },
};

export const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

// Probe script installed with addInitScript: runs before any page script.
const PROBE_INIT = `
(() => {
  window.__u4Dialogs = [];
  window.__u4PageErrors = [];
  const record = kind => (...args) => {
    window.__u4Dialogs.push({ kind, text: String(args[0] ?? ''), at: Date.now() });
    return kind === 'confirm' ? false : undefined;
  };
  window.alert = record('alert');
  window.confirm = record('confirm');
  window.prompt = record('prompt');
  window.addEventListener('error', ev => {
    window.__u4PageErrors.push(String(ev.message || ev.error || 'error'));
  });
  window.addEventListener('unhandledrejection', ev => {
    window.__u4PageErrors.push('unhandledrejection: ' + String(ev.reason));
  });
})();
`;

let browserPromise = null;

export async function launchBrowser() {
  if (!browserPromise) browserPromise = chromium.launch();
  return browserPromise;
}

export async function closeBrowser() {
  if (browserPromise) {
    const b = await browserPromise;
    browserPromise = null;
    await b.close();
  }
}

// newContext builds a context for {viewport: 'desktop'|'mobile', lang:
// 'zh'|'en', theme: 'light'|'dark'|'system'}. Returns { context, page,
// errors } where errors collects console errors + pageerrors observed through
// the Playwright event channel (the in-page arrays are read per-assertion).
export async function newContext({ viewport = 'desktop', lang = 'zh', theme = 'light', baseURL } = {}) {
  const browser = await launchBrowser();
  const vp = VIEWPORTS[viewport] || VIEWPORTS.desktop;
  const context = await browser.newContext({
    viewport: vp,
    locale: lang === 'zh' ? 'zh-CN' : 'en-US',
    hasTouch: viewport === 'mobile',
    isMobile: viewport === 'mobile',
    baseURL,
  });
  await context.addInitScript(PROBE_INIT);
  await context.addInitScript(
    ([langValue, themeValue]) => {
      try {
        localStorage.setItem('grayling_lang', langValue);
        localStorage.setItem('grayling_theme', themeValue);
      } catch {
        /* storage unavailable */
      }
    },
    [lang, theme],
  );
  const errors = [];
  context.on('page', page => {
    page.on('pageerror', err => errors.push(`pageerror: ${err.message}`));
    page.on('console', msg => {
      if (msg.type() === 'error') errors.push(`console.error: ${msg.text()}`);
    });
  });
  return { context, errors };
}

// drainProbes reads the in-page probe arrays (and clears them) so each route
// visit reports its own native-dialog calls and page errors.
export async function drainProbes(page) {
  return page.evaluate(() => {
    const dialogs = window.__u4Dialogs || [];
    const pageErrors = window.__u4PageErrors || [];
    window.__u4Dialogs = [];
    window.__u4PageErrors = [];
    return { dialogs, pageErrors };
  });
}
