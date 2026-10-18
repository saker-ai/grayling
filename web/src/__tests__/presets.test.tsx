// presets.test.ts is the vitest successor of web/legacy/preset_contract.mjs.
// It asserts the same contract against the React port's pure preset catalog
// (web-pages/src/presets): catalog shape, console-linter acceptance, fresh
// documents per build, apply-time time windows that never rot, i18n coverage
// in BOTH catalogs, the picker wiring (confirm dialog, source replacement,
// metadata carry-over, select reset) — and it prints one machine-readable
// line, `preset_contract.presets {…}`, that web/legacy/render_contract_test.go
// parses out of stdout to re-validate every document with the real server
// validators (model.ValidateFlag / model.ValidateSegment). The marker format
// is byte-compatible with the legacy contract.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ToastProvider, ConfirmProvider } from '@saker/grayling-pages';
import { PresetPicker } from '@saker/grayling-pages/editors/PresetPicker';
import { lintFlag, lintSegment } from '@saker/grayling-pages/lint';
import {
  FLAG_PRESETS, SEGMENT_PRESETS, presetFlagContent, presetSegmentContent,
} from '@saker/grayling-pages/presets';
import i18n from '../i18n';
import en from '../i18n/en';
import zh from '../i18n/zh';

const t = i18n.t.bind(i18n);

interface BuiltDocument {
  id: string;
  content: Record<string, unknown>;
}

const KEY = (id: string): string => 'preset-' + id;

function buildAll(): { flags: BuiltDocument[]; segments: BuiltDocument[] } {
  return {
    flags: FLAG_PRESETS.map(p => ({ id: p.id, content: p.build(KEY(p.id)) as unknown as Record<string, unknown> })),
    segments: SEGMENT_PRESETS.map(p => ({ id: p.id, content: p.build(KEY(p.id)) as unknown as Record<string, unknown> })),
  };
}

function timeOperands(content: Record<string, unknown>): { after?: { value: string }; before?: { value: string } } {
  const fromConstraints = Array.isArray(content.constraints) ? content.constraints : [];
  const fromTargeting = Array.isArray(content.targeting)
    ? (content.targeting as Array<{ constraints?: Array<{ op?: string; value?: string }> }>).flatMap(r => r.constraints || [])
    : [];
  const all = [...fromConstraints, ...fromTargeting] as Array<{ op?: string; value?: string }>;
  const timed = all.filter(c => c.op === 'time_after' || c.op === 'time_before');
  return {
    after: timed.find(c => c.op === 'time_after') as { value: string } | undefined,
    before: timed.find(c => c.op === 'time_before') as { value: string } | undefined,
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('preset catalog shape', () => {
  it('ships 6 flag + 5 segment presets with unique ids and build functions', () => {
    expect(FLAG_PRESETS.length).toBeGreaterThanOrEqual(6);
    expect(SEGMENT_PRESETS.length).toBeGreaterThanOrEqual(5);
    for (const [label, list] of [['flag', FLAG_PRESETS], ['segment', SEGMENT_PRESETS]] as const) {
      const seen = new Set<string>();
      for (const preset of list) {
        expect(preset.id, `${label} preset id`).toBeTruthy();
        expect(seen.has(preset.id), `${label} duplicate id ${preset.id}`).toBe(false);
        seen.add(preset.id);
        expect(typeof preset.build).toBe('function');
      }
    }
  });

  it('falls back to null for empty or unknown ids', () => {
    for (const lookup of [presetFlagContent, presetSegmentContent]) {
      expect(lookup('', 'fallback-probe')).toBeNull();
      expect(lookup('nope-not-a-preset', 'fallback-probe')).toBeNull();
    }
    expect(presetFlagContent('percent-rollout', 'fallback-probe')?.key).toBe('fallback-probe');
  });
});

describe('preset documents', () => {
  it('pass the console linters and carry schemaVersion 1', () => {
    const { flags, segments } = buildAll();
    for (const doc of flags) {
      expect(doc.content.key).toBe(KEY(doc.id));
      expect(doc.content.schemaVersion).toBe(1);
      expect(lintFlag(t, doc.content)).toEqual([]);
    }
    for (const doc of segments) {
      expect(doc.content.key).toBe(KEY(doc.id));
      expect(doc.content.schemaVersion).toBe(1);
      expect(lintSegment(t, doc.content)).toEqual([]);
    }
  });

  it('build a fresh, independent document on every call', () => {
    for (const preset of [...FLAG_PRESETS, ...SEGMENT_PRESETS]) {
      const probe = preset.build('probe-a') as unknown as Record<string, unknown>;
      probe.key = 'mutated-by-the-test';
      const again = preset.build('probe-a') as unknown as Record<string, unknown>;
      expect(again.key, `${preset.id} must not share a template`).toBe('probe-a');
    }
  });

  it('generates time windows at apply time that open in the future', () => {
    const flag = FLAG_PRESETS.find(p => p.id === 'time-window');
    const segment = SEGMENT_PRESETS.find(p => p.id === 'active-window');
    expect(flag).toBeTruthy();
    expect(segment).toBeTruthy();
    const pattern = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
    const t0 = Date.parse('2026-10-15T00:00:00Z');
    vi.useFakeTimers();
    try {
      vi.setSystemTime(t0);
      for (const preset of [flag!, segment!]) {
        const first = timeOperands(preset.build('clock-probe') as unknown as Record<string, unknown>);
        expect(first.after?.value, `${preset.id} time_after`).toMatch(pattern);
        expect(first.before?.value, `${preset.id} time_before`).toMatch(pattern);
        const afterMs = Date.parse(first.after!.value);
        const beforeMs = Date.parse(first.before!.value);
        expect(afterMs, `${preset.id} opens in the future`).toBeGreaterThan(t0);
        expect(afterMs - t0, `${preset.id} opens shortly after apply`).toBeLessThanOrEqual(2 * 3600e3);
        expect(beforeMs, `${preset.id} closes after it opens`).toBeGreaterThan(afterMs);
        expect(beforeMs - afterMs, `${preset.id} spans at least a day`).toBeGreaterThanOrEqual(24 * 3600e3);
        // Recomputed per apply: the same preset must move with the clock.
        vi.setSystemTime(t0 + 86400e3);
        const second = timeOperands(preset.build('clock-probe') as unknown as Record<string, unknown>);
        expect(second.after!.value, `${preset.id} recomputes on every apply`).not.toBe(first.after!.value);
        vi.setSystemTime(t0);
      }
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('preset i18n', () => {
  it('resolves every preset key in both catalogs', () => {
    const catalogs: Record<string, Record<string, string>> = { en, zh };
    const keys = [...FLAG_PRESETS, ...SEGMENT_PRESETS].flatMap(p =>
      [p.nameKey, p.descKey, (p as { hintKey?: string }).hintKey].filter((k): k is string => !!k));
    expect(new Set(keys).size).toBe(keys.length);
    for (const key of keys) {
      for (const [lang, catalog] of Object.entries(catalogs)) {
        expect(Object.hasOwn(catalog, key), `${lang} missing ${key}`).toBe(true);
        expect(catalog[key].trim().length, `${lang} empty ${key}`).toBeGreaterThan(0);
      }
    }
  });

  it('resolves the picker labels in both catalogs', () => {
    for (const key of ['preset.label', 'preset.none', 'preset.applyHint', 'preset.applyTitle',
      'preset.applySummary', 'preset.applyConfirm', 'preset.applied']) {
      expect(Object.hasOwn(en, key), `en missing ${key}`).toBe(true);
      expect(Object.hasOwn(zh, key), `zh missing ${key}`).toBe(true);
      expect(t(key)).not.toBe(key);
    }
  });
});

describe('PresetPicker wiring', () => {
  function renderPicker(currentSource: () => string, onApply: (doc: Record<string, unknown>) => void) {
    return render(
      <ToastProvider>
        <ConfirmProvider>
          <PresetPicker
            id="flag-preset"
            presets={FLAG_PRESETS}
            build={id => presetFlagContent(id, 'picker-probe') as unknown as Record<string, unknown> | null}
            currentSource={currentSource}
            onApply={onApply}
          />
        </ConfirmProvider>
      </ToastProvider>,
    );
  }

  it('lists every preset behind an empty default option', () => {
    renderPicker(() => '{}', () => {});
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    expect(select.value).toBe('');
    const values = Array.from(select.options).map(o => o.value);
    for (const preset of FLAG_PRESETS) expect(values).toContain(preset.id);
  });

  it('confirms, applies the fresh document with metadata carried over, and resets the select', async () => {
    const user = userEvent.setup();
    const applied: Array<Record<string, unknown>> = [];
    const source = JSON.stringify({ key: 'picker-probe', displayName: 'Keep me', description: 'And me' });
    renderPicker(() => source, doc => applied.push(doc));
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const killSwitch = FLAG_PRESETS.find(p => p.id === 'kill-switch')!;
    await user.selectOptions(select, 'kill-switch');
    // The confirmation names and describes the preset before applying it.
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain(t(killSwitch.nameKey));
    expect(dialog.textContent).toContain(t(killSwitch.descKey));
    expect(select.value).toBe('');
    await user.click(screen.getByRole('button', { name: t('preset.applyConfirm') }));
    await waitFor(() => expect(applied.length).toBe(1));
    const expected = killSwitch.build('picker-probe') as unknown as Record<string, unknown>;
    expect(applied[0]).toEqual({ ...expected, displayName: 'Keep me', description: 'And me' });
    expect(applied[0].key).toBe('picker-probe');
  });

  it('leaves the draft untouched when the dialog is cancelled', async () => {
    const user = userEvent.setup();
    const applied: Array<Record<string, unknown>> = [];
    renderPicker(() => '{"key":"picker-probe"}', doc => applied.push(doc));
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    await user.selectOptions(select, 'percent-rollout');
    await screen.findByRole('dialog');
    await user.click(screen.getByRole('button', { name: t('btn.cancel') }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(applied).toEqual([]);
    expect(select.value).toBe('');
  });
});

describe('machine-readable hand-off', () => {
  it('prints the preset_contract.presets marker for the Go validators', () => {
    const { flags, segments } = buildAll();
    expect(flags).toHaveLength(6);
    expect(segments).toHaveLength(5);
    // web/legacy/render_contract_test.go scans stdout for this exact line and
    // re-runs model.ValidateFlag/ValidateSegment over the printed documents.
    // process.stdout.write because vitest intercepts console.log and never
    // forwards it on a non-TTY run; the write lands on the real stdout.
    process.stdout.write('preset_contract.presets ' + JSON.stringify({ flags, segments }) + '\n');
  });
});
