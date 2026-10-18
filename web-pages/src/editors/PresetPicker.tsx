// PresetPicker ports legacy bindPresetPicker (web/legacy/app.js 583-607): on
// selection the picker immediately resets itself (never looking "applied"),
// builds a fresh document, carries displayName/description over from the
// current JSON when parseable, confirms with a preview dialog, then hands the
// merged document to onApply (which replaces the JSON source and redraws the
// structured form). Applying a preset is a draft edit — nothing is saved
// until the operator saves.
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useConfirm } from '../components/ConfirmDialog';
import { useToast } from '../components/Toast';

export interface PresetLike {
  id: string;
  nameKey: string;
  descKey: string;
  hintKey?: string;
}

function PresetPreview({ preset, content }: { preset: PresetLike; content: Record<string, unknown> }): ReactElement {
  const { t } = useTranslation();
  return (
    <>
      <p>{t(preset.descKey)}</p>
      {preset.hintKey ? <p className="muted">{t(preset.hintKey)}</p> : null}
      <pre className="pre small">{JSON.stringify(content, null, 2)}</pre>
    </>
  );
}

export function PresetPicker<P extends PresetLike>({ id, presets, build, currentSource, onApply }: {
  id: string;
  presets: P[];
  build: (id: string) => Record<string, unknown> | null;
  currentSource: () => string;
  onApply: (content: Record<string, unknown>) => void;
}): ReactElement {
  const { t } = useTranslation();
  const confirm = useConfirm();
  const { toast } = useToast();

  const onChange = async (selected: string, select: HTMLSelectElement): Promise<void> => {
    const preset = presets.find(item => item.id === selected);
    select.value = ''; // never look like a preset is still "applied"
    if (!preset) return;
    const next = build(preset.id);
    if (!next) return;
    let previous: Record<string, unknown> = {};
    try {
      previous = JSON.parse(currentSource());
    } catch {
      // The preset may repair invalid configuration; metadata carry-over is
      // skipped when the current source does not parse.
    }
    for (const field of ['displayName', 'description'] as const) {
      if (previous[field] !== undefined) next[field] = previous[field];
    }
    const accepted = await confirm({
      title: t('preset.applyTitle'),
      summary: t('preset.applySummary', { name: t(preset.nameKey) }),
      preview: <PresetPreview preset={preset} content={next} />,
      confirmLabel: t('preset.applyConfirm'),
    });
    if (!accepted) return;
    onApply(next);
    toast(t('preset.applied', { name: t(preset.nameKey) }));
  };

  return (
    <label className="inline">
      {t('preset.label')}{' '}
      <select id={id} defaultValue="" onChange={event => void onChange(event.target.value, event.target)}>
        <option value="">{t('preset.none')}</option>
        {presets.map(preset => <option key={preset.id} value={preset.id}>{t(preset.nameKey)}</option>)}
      </select>
    </label>
  );
}
