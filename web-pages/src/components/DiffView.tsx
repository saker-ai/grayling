import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { flagDiffValue } from '../lint';

// DiffView ports legacy renderFlagDiff (web/legacy/app.js): the
// draft-vs-published field-by-field comparison used on the flag detail page,
// inside the publish review dialog and (twice) in the 409 save-conflict box.
// Only fields whose canonical serialization changed are listed; values are
// rendered with flagDiffValue ('—' for absent, canonical JSON otherwise).
export interface DiffSide {
  content?: Record<string, unknown>;
  deleted?: boolean;
}

export interface DiffViewProps {
  current: DiffSide | null | undefined;
  draft: DiffSide | null | undefined;
  // Base revision for the "compared against rN" line (legacy current.revision).
  baseRevision?: number;
  title?: string;
  beforeLabel?: string;
  afterLabel?: string;
  emptyLabel?: string;
  hideBase?: boolean;
}

// The legacy label map for the diffable fields (resource metadata plus the
// flag document's top-level sections).
const FIELD_LABEL_KEYS: Record<string, string> = {
  displayName: 'resource.displayName',
  description: 'resource.description',
  type: 'flags.diffType',
  disable: 'flags.diffDisable',
  bucketing: 'flags.diffBucketing',
  fields: 'flags.diffFields',
  variations: 'flags.diffVariations',
  defaultRule: 'flags.diffDefaultRule',
  targeting: 'flags.diffTargeting',
  experiment: 'flags.diffExperiment',
};

export function DiffView({ current, draft, baseRevision, title, beforeLabel, afterLabel, emptyLabel, hideBase }: DiffViewProps): ReactElement {
  const { t } = useTranslation();
  const heading = title || t('flags.diffTitle');
  if (!draft) {
    return (
      <section className="card">
        <h3>{heading}</h3>
        <p className="muted">{t('flags.diffNoDraft')}</p>
      </section>
    );
  }
  if (draft.deleted) {
    return (
      <section className="card">
        <h3>{heading}</h3>
        <div className="banner warn">{t('flags.diffDeleted')}</div>
      </section>
    );
  }
  const before = current?.content || {};
  const after = draft.content || {};
  const changed = Object.keys(FIELD_LABEL_KEYS).filter(field => flagDiffValue(before[field]) !== flagDiffValue(after[field]));
  return (
    <section className="card">
      <h3>{heading}</h3>
      {hideBase ? null : <p className="muted">{t('flags.diffBase', { revision: baseRevision ?? 0 })}</p>}
      {changed.length ? (
        <div className="flag-diff-list">
          {changed.map(field => (
            <details className="flag-diff-item" key={field}>
              <summary>{t(FIELD_LABEL_KEYS[field])}</summary>
              <div className="flag-diff-values">
                <div>
                  <strong>{beforeLabel || t('flags.diffBefore')}</strong>
                  <pre className="pre small">{flagDiffValue(before[field])}</pre>
                </div>
                <div>
                  <strong>{afterLabel || t('flags.diffAfter')}</strong>
                  <pre className="pre small">{flagDiffValue(after[field])}</pre>
                </div>
              </div>
            </details>
          ))}
        </div>
      ) : (
        <p>{emptyLabel || t('flags.diffNoChanges')}</p>
      )}
    </section>
  );
}
