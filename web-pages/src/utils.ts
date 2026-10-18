// View-level helpers ported from web/legacy/app.js: fmtTime, chip color maps,
// diagnosticLabel, resourceKeyError/resourceContent and the ws/scope path
// builders. All pure; localization arrives via the caller's t()/locale so the
// module itself never touches i18n state.
import type { Translate } from '@saker/grayling-client';

// fmtTime renders an API timestamp like the legacy console: an em dash for
// missing/invalid values, otherwise the locale date-time string.
export function fmtTime(ts: string | undefined, locale: string): string {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d.getTime()) ? '—' : d.toLocaleString(locale);
}

// fmtDurNs renders a time.Duration (int64 ns) like legacy app.js fmtDurNs:
// fractional hours, ≥24h switches to days (168h → "7d").
export function fmtDurNs(ns: number | undefined, t: Translate): string {
  if (typeof ns !== 'number' || !isFinite(ns)) return '—';
  const h = ns / 3.6e12;
  return h >= 24 ? t('unit.days', { n: h / 24 }) : t('unit.hours', { n: h });
}

// shortHash is legacy shortHash(): the first 12 chars of a revision/evidence
// hash, em dash when empty.
export function shortHash(hash: string | undefined): string {
  return hash ? hash.slice(0, 12) : '—';
}

// Chip color maps, copied from legacy app.js.
export const DRAFT_CHIP: Record<string, string> = { editing: 'blue', submitted: 'amber', approved: 'green' };
export const EXP_CHIP: Record<string, string> = {
  draft: 'gray',
  reviewed: 'blue',
  bound: 'teal',
  running: 'green',
  paused: 'amber',
  stopped: 'red',
  analyzed: 'blue',
  archived: 'gray',
};
export const VERDICT_CHIP: Record<string, string> = { ship: 'green', rollback: 'red', continue: 'blue', hold: 'amber' };

// diagnosticLabel maps known reason/action codes to localized labels and
// passes anything else through (em dash for empty), exactly like legacy. The
// map holds FULL catalog keys (not legacy's prefix+suffix concat) so the
// parity test's static call extractor never sees a partial key.
export function diagnosticLabel(value: string | undefined, t: Translate): string {
  const keys: Record<string, string> = {
    normal: 'diagnostic.normal',
    high: 'diagnostic.high',
    'flag.rollback': 'diagnostic.rollback',
    'flag.publish': 'diagnostic.publish',
    'flag.delete': 'diagnostic.delete',
    'flag.emergency_disable': 'diagnostic.disable',
    'segment.publish': 'diagnostic.segmentPublish',
    'segment.delete': 'diagnostic.segmentDelete',
    default: 'diagnostic.default',
    targeting: 'diagnostic.targeting',
    disabled: 'diagnostic.disabled',
    error: 'diagnostic.error',
  };
  const key = value ? keys[value] : undefined;
  return key ? t(key) : value || '—';
}

// resourceKeyError is the legacy key validator: 1–128 chars, starting with a
// letter or digit, then letters/digits/dots/underscores/hyphens. Accepts
// unknown like legacy (a non-string value is invalid, never a crash).
export function resourceKeyError(value: unknown, t: Translate): string {
  return typeof value === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.exec(value)?.[0] === value
    ? ''
    : t('key.formatHint');
}

// resourceContent picks the content the list should show: draft first, then
// the published current, then a bare content field.
export function resourceContent(resource: {
  draft?: { content?: Record<string, unknown> } | null;
  current?: { content?: Record<string, unknown> } | null;
  content?: Record<string, unknown>;
}): Record<string, unknown> {
  return resource?.draft?.content || resource?.current?.content || resource?.content || {};
}

// wsPath/scopePath build the admin API prefixes for a workspace and a
// workspace+environment scope (legacy wsPath()/scopePath()).
export function wsPath(ws: string): string {
  return '/api/v1/admin/workspaces/' + encodeURIComponent(ws);
}

export function scopePath(ws: string, env: string): string {
  return wsPath(ws) + '/environments/' + encodeURIComponent(env);
}

// consoleViewPath builds the in-hash route for a console view, matching
// web/src/routes.tsx buildConsoleHash for the simple view/+rest shapes the
// phase-1 views navigate to (`~` encodes an empty scope segment).
export function consoleViewPath(ws: string, env: string, view: string, ...rest: string[]): string {
  return (
    '/' +
    ['console', ws || '~', env || '~', view, ...rest].map(encodeURIComponent).join('/')
  );
}
