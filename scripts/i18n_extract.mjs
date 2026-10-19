// One-shot generator for the React migration: loads web/legacy/i18n.js in a
// node:vm sandbox (the file only needs localStorage/navigator stubs) and
// writes the en/zh catalogs as TS modules at web/src/i18n/{en,zh}.ts.
//
// Re-run after any legacy catalog change:
//   node scripts/i18n_extract.mjs
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const code = fs.readFileSync(path.join(root, 'web/legacy/i18n.js'), 'utf8');

const sandbox = {
  console,
  localStorage: { getItem: () => null, setItem: () => {} },
  navigator: { language: 'en' },
};
vm.createContext(sandbox);
vm.runInContext(code + '\n;globalThis.__catalogs = { en: I18N_EN, zh: I18N_ZH };', sandbox);
const catalogs = sandbox.__catalogs;
if (!catalogs || !catalogs.en || !catalogs.zh) {
  throw new Error('could not read I18N_EN/I18N_ZH from web/legacy/i18n.js');
}

const header =
  '// Generated from web/legacy/i18n.js by scripts/i18n_extract.mjs — do not edit by hand.\n' +
  '// Values keep the legacy {name} interpolation placeholders verbatim.\n';

const outDir = path.join(root, 'web/src/i18n');
fs.mkdirSync(outDir, { recursive: true });
for (const lang of ['en', 'zh']) {
  const body = JSON.stringify(catalogs[lang], null, 2);
  fs.writeFileSync(path.join(outDir, lang + '.ts'), header + 'export default ' + body + ';\n');
  console.log('wrote web/src/i18n/' + lang + '.ts:', Object.keys(catalogs[lang]).length, 'keys');
}
