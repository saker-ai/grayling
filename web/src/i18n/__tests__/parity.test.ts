/// <reference types="node" />
// i18n parity contract for the React console, mirroring the static parts of
// web/legacy/i18n_contract.mjs:
//   1. en/zh catalog key-set parity;
//   2. every catalog key resolves through t() in BOTH languages with zero
//      recorded misses (missingKeys stays empty);
//   3. every statically extracted t('…') key in web/src and web-pages/src
//      exists in the en catalog;
//   4. zero CJK code points in web/src (this i18n directory exempt),
//      web-pages/src and web/index.html, with JS/HTML comments stripped —
//      the regex is copied from the legacy contract.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import i18n, { missingKeys } from '../index';
import en from '../en';
import zh from '../zh';

const here = path.dirname(fileURLToPath(import.meta.url));
const webSrc = path.resolve(here, '../..');
const pagesSrc = path.resolve(here, '../../../../web-pages/src');
const indexHTML = path.resolve(here, '../../../index.html');

describe('catalog parity', () => {
  it('en and zh carry identical key sets', () => {
    const enKeys = Object.keys(en).sort();
    const zhKeys = Object.keys(zh).sort();
    expect(zhKeys).toEqual(enKeys);
    expect(enKeys.length).toBeGreaterThan(1000);
  });
});

describe('t() resolution', () => {
  it('renders every key in both languages with zero misses', async () => {
    missingKeys.length = 0;
    for (const lang of ['en', 'zh'] as const) {
      await i18n.changeLanguage(lang);
      for (const key of Object.keys(en)) {
        const value = i18n.t(key);
        expect(typeof value).toBe('string');
        expect(value.length).toBeGreaterThan(0);
      }
    }
    await i18n.changeLanguage('en');
    expect(missingKeys).toEqual([]);
  });
});

function walkSources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walkSources(p));
    else if (/\.(ts|tsx)$/.test(entry.name)) out.push(p);
  }
  return out;
}

describe('static extraction', () => {
  const T_CALL_RE = /\bt\(\s*'([^']+)'/g;
  it('every extracted t() key exists in the en catalog', () => {
    const used = new Set<string>();
    for (const dir of [webSrc, pagesSrc]) {
      for (const file of walkSources(dir)) {
        // The catalogs and this contract live in web/src/i18n — exempt.
        if (file.includes(path.join('src', 'i18n') + path.sep)) continue;
        const src = fs.readFileSync(file, 'utf8');
        for (const m of src.matchAll(T_CALL_RE)) used.add(m[1]);
      }
    }
    expect(used.size).toBeGreaterThan(20);
    for (const key of used) {
      expect(en, `missing catalog key: ${key}`).toHaveProperty(key);
    }
  });
});

describe('CJK scan', () => {
  // Copied from web/legacy/i18n_contract.mjs.
  const CJK_RE = /[　-〿㐀-䶿一-鿿豈-﫿︰-﹏＀-￯]/;
  const stripJsComments = (src: string): string =>
    src
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .split('\n')
      .map(l => {
        const i = l.indexOf('//');
        return i < 0 ? l : l.slice(0, i);
      })
      .join('\n');

  it('no CJK literals outside the catalogs', () => {
    const offenders: string[] = [];
    for (const file of walkSources(webSrc)) {
      // The catalogs and this contract live in web/src/i18n — exempt.
      if (file.includes(path.join('src', 'i18n') + path.sep)) continue;
      if (CJK_RE.test(stripJsComments(fs.readFileSync(file, 'utf8')))) offenders.push(file);
    }
    for (const file of walkSources(pagesSrc)) {
      if (CJK_RE.test(stripJsComments(fs.readFileSync(file, 'utf8')))) offenders.push(file);
    }
    const html = fs.readFileSync(indexHTML, 'utf8').replace(/<!--[\s\S]*?-->/g, '');
    if (CJK_RE.test(html)) offenders.push(indexHTML);
    expect(offenders).toEqual([]);
  });
});
