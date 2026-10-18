import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import type { FlagRevision } from '@saker/grayling-client';
import { Button } from './Button';
import { fmtTime } from '../utils';

// RevisionsTable ports legacy revisionsTable (web/legacy/app.js): the flag
// revision history with short hash, publish metadata, note/displayName/
// description and the two rollback affordances (approval-routed rollback and
// emergency rollback). The click decisions stay with the caller — the legacy
// handlers confirm via actionDialog before any request.
export interface RevisionsTableProps {
  revisions: FlagRevision[] | undefined;
  locale: string;
  onRollback: (revision: number) => void;
  onEmergencyRollback: (revision: number) => void;
  // canRollback mirrors the legacy gateActions rule for [data-rb]/[data-er]
  // (flag.publish): without the permission the buttons hide, rows stay.
  canRollback?: boolean;
}

function shortHash(hash: string | undefined): string {
  return hash ? String(hash).slice(0, 12) : '—';
}

export function RevisionsTable({ revisions, locale, onRollback, onEmergencyRollback, canRollback = true }: RevisionsTableProps): ReactElement {
  const { t } = useTranslation();
  const revs = revisions || [];
  if (!revs.length) return <p className="muted">{t('flags.noRevs')}</p>;
  // .table-scroll keeps the 660px-min grid inside the viewport on narrow
  // screens (same pattern as DataTable): the wrapper scrolls, not the page.
  return (
    <div className="table-scroll">
      <table className="grid">
      <thead>
        <tr>
          <th>{t('th.rev')}</th>
          <th>{t('th.hash')}</th>
          <th>{t('th.published')}</th>
          <th>{t('th.by')}</th>
          <th>{t('th.note')}</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {revs.map(r => (
          <tr key={r.revision}>
            <td>
              r{r.revision}
              {r.rollbackOf ? <span className="muted"> {t('flags.rollbackOf', { n: r.rollbackOf })}</span> : null}
            </td>
            <td className="mono">{shortHash(r.hash)}</td>
            <td className="nowrap">{fmtTime(r.publishedAt, locale)}</td>
            <td>{r.publishedBy || '—'}</td>
            <td>
              {r.note || ''}
              {r.content?.displayName ? <div>{String(r.content.displayName)}</div> : null}
              {r.content?.description ? <div className="muted">{String(r.content.description)}</div> : null}
            </td>
            <td>
              {canRollback ? (
                <>
                  <Button size="sm" data-rb={r.revision} onClick={() => onRollback(r.revision)}>
                    {t('flags.rollbackTo', { n: r.revision })}
                  </Button>{' '}
                  <Button size="sm" variant="danger" data-er={r.revision} onClick={() => onEmergencyRollback(r.revision)}>
                    {t('approval.emergency')}
                  </Button>
                </>
              ) : null}
            </td>
          </tr>
        ))}
      </tbody>
      </table>
    </div>
  );
}
