import type { HTMLAttributes, ReactElement, ReactNode } from 'react';

// DataTable is the single-render successor of the legacy dual markup
// (desktop table.grid + .mobile-record-cards): ONE table, wrapped in a
// horizontally scrolling container so narrow screens pan instead of needing
// a second DOM tree. Column-definition driven; class names stay on the
// global.css contract (table.grid, .muted empty row).
export interface Column<T> {
  key: string;
  header: ReactNode;
  className?: string;
  cell: (row: T) => ReactNode;
}

export interface DataTableProps<T> {
  columns: Array<Column<T>>;
  rows: T[];
  rowKey: (row: T) => string;
  emptyText: string;
  // rowProps supplies per-row attributes (e.g. data-list-record for the list
  // search filter, or a click handler for drill-down rows).
  rowProps?: (row: T) => HTMLAttributes<HTMLTableRowElement> & Record<`data-${string}`, string>;
  ariaLabel?: string;
  // id lands on the <table> (legacy views addressed tables by id).
  id?: string;
}

export function DataTable<T>({ columns, rows, rowKey, emptyText, rowProps, ariaLabel, id }: DataTableProps<T>): ReactElement {
  return (
    <div className="table-scroll">
      <table className="grid" aria-label={ariaLabel} id={id}>
        <thead>
          <tr>
            {columns.map(col => (
              <th key={col.key} className={col.className}>
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
}
