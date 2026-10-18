// i18next setup for the grayling console. Ported from web/legacy/i18n.js:
// flat keys (keySeparator: false), legacy {name} interpolation delimiters,
// language resolution localStorage grayling_lang > navigator.language
// (zh* → zh) > en, and every miss recorded into the exported missingKeys
// array + console.warn so contract tests can assert zero missing keys.
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import en from './en';
import zh from './zh';

export const missingKeys: string[] = [];

export type ConsoleLang = 'en' | 'zh';

export function detectLang(): ConsoleLang {
  try {
    const stored = typeof localStorage !== 'undefined' && localStorage.getItem('grayling_lang');
    if (stored === 'en' || stored === 'zh') return stored;
  } catch {
    // storage unavailable (private mode)
  }
  const nav = (typeof navigator !== 'undefined' && navigator.language) || '';
  return String(nav).toLowerCase().startsWith('zh') ? 'zh' : 'en';
}

void i18n.use(initReactI18next).init({
  resources: {
    en: { translation: en },
    zh: { translation: zh },
  },
  lng: detectLang(),
  fallbackLng: 'en',
  keySeparator: false,
  interpolation: {
    prefix: '{',
    suffix: '}',
    escapeValue: false,
  },
  saveMissing: true,
  missingKeyHandler: (_lngs, _ns, key) => {
    if (!missingKeys.includes(key)) missingKeys.push(key);
    console.warn('[i18n] missing key: ' + key);
  },
});

// setLang switches the UI language and persists the choice; react-i18next
// re-renders subscribed components in place (no page reload).
export function setLang(lang: ConsoleLang): void {
  if (lang !== 'en' && lang !== 'zh') return;
  try {
    if (typeof localStorage !== 'undefined') localStorage.setItem('grayling_lang', lang);
  } catch {
    // storage unavailable
  }
  void i18n.changeLanguage(lang);
}

// localeFor maps the active language to a BCP-47 locale for date formatting.
export function localeFor(): string {
  return i18n.language === 'zh' ? 'zh-CN' : 'en-US';
}

export default i18n;
