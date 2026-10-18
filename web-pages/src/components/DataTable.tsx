import type { HTMLAttributes, ReactElement, ReactNode } from 'react';

// DataTable renders ONE column declaration as two trees when mobileCards is
// on: the desktop table (in a horizontally scrolling wrapper) and a card
// tree for narrow screens. A (max-width: 700px) media query swaps which tree
// is visible; the hidden tree is display:none, which also drops it from the
// accessibility tree, so screen readers never meet duplicate content. Both
// trees run the same cell renderers — that single source is the whole point —
// which imposes one hard constraint: a cell renderer must never use id= (the
// two trees would duplicate it). The successor of the legacy dual markup
// (desktop table.grid + .mobile-record-cards), which drifted because the two
// templates were hand-written.
export interface Column<T> {
  key: string;
  header: ReactNode;
  className?: string;
  cell: (row: T) => ReactNode;
  // mobileLabel is the card field name; when omitted and header is a string,
  // the header text is used. A field whose label resolves to '' renders its
  // value without a term (e.g. an actions column with an empty header).
  mobileLabel?: string;
  // mobilePrimary promotes the column into the card title row (usually one
  // column; several render side by side in column order). Promoted columns
  // leave the field list.
  mobilePrimary?: boolean;
  // mobileHidden omits a low-value column from cards; it stays in the table.
  mobileHidden?: boolean;
}

export interface DataTableProps<T> {
  columns: Array<Column<T>>;
  rows: T[];
  rowKey: (row: T) => string;
  emptyText: string;
  // rowProps supplies per-row attributes (e.g. data-list-record for the list
  // search filter, or a click handler for drill-down rows). With mobileCards
  // the same props land on the card element (className merges onto .dt-card).
  rowProps?: (row: T) => HTMLAttributes<HTMLTableRowElement> & Record<`data-${string}`, string>;
  ariaLabel?: string;
  // id lands on the <table> (legacy views addressed tables by id).
  id?: string;
  // mobileCards opts the table into the dual-tree card mode for narrow
  // viewports (default off — each page enables it explicitly).
  mobileCards?: boolean;
}

// mobileFieldLabel resolves a card field label: the explicit mobileLabel,
// else a string header, else '' (no term rendered).
function mobileFieldLabel<T>(col: Column<T>): string {
  if (col.mobileLabel !== undefined) return col.mobileLabel;
  return typeof col.header === 'string' ? col.header : '';
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  emptyText,
  rowProps,
  ariaLabel,
  id,
  mobileCards,
}: DataTableProps<T>): ReactElement {
  const table = (
    <div className="table-scroll">
      <table className="grid" aria-label={ariaLabel} id={id}>
        <thead>
          <tr>
            {columns.map(col => (
              <th key={col.key} className={col.className} scope="col">
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="muted">
                {emptyText}
              </td>
            </tr>
          ) : (
            rows.map(row => (
              <tr key={rowKey(row)} {...rowProps?.(row)}>
                {columns.map(col => (
                  <td key={col.key} className={col.className}>
                    {col.cell(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
  if (!mobileCards) return table;

  const primary = columns.filter(col => col.mobilePrimary && !col.mobileHidden);
  const fields = columns.filter(col => !col.mobilePrimary && !col.mobileHidden);
  return (
    <div className="dt-responsive">
      {table}
      <div className="dt-cards">
        {rows.length === 0 ? (
          <article className="dt-card">
            <span className="muted">{emptyText}</span>
          </article>
        ) : (
          rows.map(row => {
            const { className: rowClass, ...rest } = rowProps?.(row) ?? {};
            return (
              <article key={rowKey(row)} className={'dt-card' + (rowClass ? ' ' + rowClass : '')} {...rest}>
                {primary.length ? (
                  <div className="dt-card-primary">
                    {primary.map(col => (
                      <span key={col.key} className={col.className}>
                        {col.cell(row)}
                      </span>
                    ))}
                  </div>
                ) : null}
                {fields.length ? (
                  <dl className="dt-card-fields">
                    {fields.map(col => {
                      const label = mobileFieldLabel(col);
                      return (
                        <div className="dt-card-field" key={col.key}>
                          {label ? <dt>{label}</dt> : null}
                          <dd className={col.className}>{col.cell(row)}</dd>
                        </div>
                      );
                    })}
                  </dl>
                ) : null}
              </article>
            );
          })
        )}
      </div>
    </div>
  );
}
