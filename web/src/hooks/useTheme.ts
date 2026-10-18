import { useCallback, useEffect, useState } from 'react';

// Theme handling, ported from legacy applyTheme/themePreference
// (web/legacy/app.js): grayling_theme stores system|light|dark; 'system'
// follows prefers-color-scheme live. index.html carries an inline twin of
// resolve() so the first paint is already themed (no FOUC).
export type ThemePreference = 'system' | 'light' | 'dark';

const THEME_KEY = 'grayling_theme';
const LIGHT_QUERY = '(prefers-color-scheme: light)';

export function themePreference(): ThemePreference {
  try {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved === 'light' || saved === 'dark') return saved;
  } catch {
    // storage unavailable
  }
  return 'system';
}

function resolve(preference: ThemePreference): 'light' | 'dark' {
  if (preference !== 'system') return preference;
  return window.matchMedia?.(LIGHT_QUERY).matches ? 'light' : 'dark';
}

export function useTheme(): {
  preference: ThemePreference;
  resolved: 'light' | 'dark';
  setPreference: (p: ThemePreference) => void;
} {
  const [preference, setPreferenceState] = useState<ThemePreference>(themePreference);

  useEffect(() => {
    document.documentElement.dataset.theme = resolve(preference);
    if (preference !== 'system') return;
    const mq = window.matchMedia?.(LIGHT_QUERY);
    const onChange = () => {
      document.documentElement.dataset.theme = resolve('system');
    };
    mq?.addEventListener?.('change', onChange);
    return () => mq?.removeEventListener?.('change', onChange);
  }, [preference]);

  const setPreference = useCallback((p: ThemePreference) => {
    try {
      localStorage.setItem(THEME_KEY, p);
    } catch {
      // storage unavailable
    }
    setPreferenceState(p);
  }, []);

  return { preference, resolved: resolve(preference), setPreference };
}
