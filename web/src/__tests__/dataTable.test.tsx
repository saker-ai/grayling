// dataTable.test.tsx pins the R2 record-card mode: mobileCards renders a
// second tree (.dt-cards) from the SAME column declarations as the desktop
// table, the 700px media query in global.css swaps which tree is visible.
// jsdom does not evaluate media queries, so both trees are always in the DOM
// here — these tests assert the dual-tree structure and the column metadata
// behavior (mobileLabel / mobilePrimary / mobileHidden), never the visual
// switch itself.
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { HTMLAttributes } from 'react';
import { fireEvent, render, within } from '@testing-library/react';
import { DataTable } from '@saker/grayling-pages/components/DataTable';
import type { Column } from '@saker/grayling-pages/components/DataTable';
import { expectNoA11yViolations } from './helpers';

interface Row {
  id: string;
  name: string;
  qty: number;
  note: string;
  secret: string;
}

type RowProps = (row: Row) => HTMLAttributes<HTMLTableRowElement> & Record<`data-${string}`, string>;

const ROWS: Row[] = [
  { id: 'r1', name: 'alpha', qty: 3, note: 'first note', secret: 'sec-1' },
  { id: 'r2', name: 'beta', qty: 5, note: 'second note', secret: 'sec-2' },
];

// Columns exercise every metadata branch: a primary title column, a
// string-header fallback label, an explicit mobileLabel over a non-string
// header, a hidden column and an unlabeled (empty-header) action column.
const COLUMNS: Array<Column<Row>> = [
  { key: 'name', header: 'Name', mobilePrimary: true, cell: r => r.name },
  { key: 'qty', header: 'Quantity', cell: r => r.qty },
  { key: 'note', header: <em>note</em>, mobileLabel: 'Note', cell: r => r.note },
  { key: 'secret', header: 'Secret', mobileHidden: true, cell: r => r.secret },
  { key: 'act', header: '', cell: r => <button type="button">{'act-' + r.id}</button> },
];

function renderCards(rows: Row[] = ROWS, rowProps?: RowProps) {
  return render(
    <DataTable columns={COLUMNS} rows={rows} rowKey={r => r.id} emptyText="nothing here" mobileCards rowProps={rowProps} />,
  );
}

describe('DataTable mobileCards', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('renders both trees from the same column declarations', () => {
    const { container } = renderCards();
    expect(container.querySelector('.dt-responsive')).toBeTruthy();
    const table = container.querySelector('table.grid') as HTMLElement;
    const cards = container.querySelector('.dt-cards') as HTMLElement;
    expect(table).toBeTruthy();
    expect(cards).toBeTruthy();
    // Table: header + two data rows; cards: one article per row.
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    expect(cards.querySelectorAll('.dt-card')).toHaveLength(2);
    // Both trees carry the same cell data.
    expect(within(table).getByText('alpha')).toBeTruthy();
    expect(within(cards).getByText('alpha')).toBeTruthy();
    expect(within(table).getByText('5')).toBeTruthy();
    expect(within(cards).getByText('5')).toBeTruthy();
  });

  it('promotes mobilePrimary columns into the card title row and out of the field list', () => {
    const { container } = renderCards();
    const card = container.querySelector('.dt-card') as HTMLElement;
    const primary = card.querySelector('.dt-card-primary') as HTMLElement;
    expect(primary).toBeTruthy();
    expect(within(primary).getByText('alpha')).toBeTruthy();
    // The promoted column is not repeated as a labeled field.
    const labels = Array.from(card.querySelectorAll('.dt-card-field dt')).map(dt => dt.textContent);
    expect(labels).not.toContain('Name');
  });

  it('resolves field labels: explicit mobileLabel beats the header, string headers are the fallback', () => {
    const { container } = renderCards();
    const card = container.querySelector('.dt-card') as HTMLElement;
    const labels = Array.from(card.querySelectorAll('.dt-card-field dt')).map(dt => dt.textContent);
    expect(labels).toContain('Quantity'); // fallback from the string header
    expect(labels).toContain('Note'); // explicit mobileLabel over a non-string header
    expect(labels).not.toContain('note');
  });

  it('renders a field without a term when the label resolves to empty', () => {
    const { container } = renderCards();
    const card = container.querySelector('.dt-card') as HTMLElement;
    const actionField = Array.from(card.querySelectorAll('.dt-card-field')).find(f => f.querySelector('button'));
    expect(actionField).toBeTruthy();
    expect(actionField?.querySelector('dt')).toBeNull();
    expect(within(actionField as HTMLElement).getByRole('button', { name: 'act-r1' })).toBeTruthy();
  });

  it('omits mobileHidden columns from cards but keeps them in the table', () => {
    const { container } = renderCards();
    const cards = container.querySelector('.dt-cards') as HTMLElement;
    const table = container.querySelector('table.grid') as HTMLElement;
    expect(within(cards).queryByText('sec-1')).toBeNull();
    const labels = Array.from(cards.querySelectorAll('.dt-card-field dt')).map(dt => dt.textContent);
    expect(labels).not.toContain('Secret');
    expect(within(table).getByText('sec-1')).toBeTruthy();
    expect(within(table).getByText('Secret')).toBeTruthy();
  });

  it('renders the empty state in both trees', () => {
    const { container } = renderCards([]);
    const table = container.querySelector('table.grid') as HTMLElement;
    expect(within(table).getByText('nothing here')).toBeTruthy();
    const cards = container.querySelectorAll('.dt-card');
    expect(cards).toHaveLength(1);
    const muted = cards[0].querySelector('.muted');
    expect(muted?.textContent).toBe('nothing here');
  });

  it('mirrors row props (data-*, className, onClick) onto the card', () => {
    const clicks: string[] = [];
    const { container } = renderCards(ROWS, r => ({
      'data-row-id': r.id,
      className: 'clickable',
      onClick: () => clicks.push(r.id),
    }));
    // The table rows keep the contract.
    expect(container.querySelectorAll('tr[data-row-id]')).toHaveLength(2);
    const cards = Array.from(container.querySelectorAll<HTMLElement>('.dt-card'));
    expect(cards.map(c => c.getAttribute('data-row-id'))).toEqual(['r1', 'r2']);
    expect(cards[0].className).toContain('dt-card');
    expect(cards[0].className).toContain('clickable');
    fireEvent.click(cards[1]);
    expect(clicks).toEqual(['r2']);
  });

  it('leaves the DOM unchanged when mobileCards is off', () => {
    const { container } = render(
      <DataTable columns={COLUMNS} rows={ROWS} rowKey={r => r.id} emptyText="nothing here" />,
    );
    expect(container.querySelector('.dt-responsive')).toBeNull();
    expect(container.querySelector('.dt-cards')).toBeNull();
    expect(container.querySelector('.table-scroll table.grid')).toBeTruthy();
  });

  it('has no axe violations in the dual-tree state', async () => {
    const { container } = renderCards();
    await expectNoA11yViolations(container);
  });
});
