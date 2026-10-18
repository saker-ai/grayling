// Pagination is the legacy flags-list pager: disabled states at the ends and
// focus placement after a page turn (clicked button keeps focus; if it became
// disabled, focus moves to the opposite button).
import { useState } from 'react';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Pagination } from '@saker/grayling-pages';
import '../i18n';

afterEach(cleanup);

function Pager({ total, pageSize }: { total: number; pageSize: number }): ReactElement {
  const [page, setPage] = useState(0);
  return <Pagination page={page} pageSize={pageSize} total={total} onPage={setPage} />;
}

function buttons() {
  return {
    prev: screen.getByRole('button', { name: 'Previous' }) as HTMLButtonElement,
    next: screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement,
  };
}

describe('Pagination', () => {
  beforeEach(() => localStorage.clear());

  it('renders the page label and end-of-list disabled states', () => {
    render(<Pager total={95} pageSize={10} />);
    const { prev, next } = buttons();
    expect(screen.getByRole('status').textContent).toBe('Page 1 of 10 · 95 matches');
    expect(prev.disabled).toBe(true);
    expect(next.disabled).toBe(false);
  });

  it('keeps focus on the clicked button while it stays enabled', async () => {
    render(<Pager total={95} pageSize={10} />);
    const { next } = buttons();
    await userEvent.click(next);
    expect(screen.getByRole('status').textContent).toBe('Page 2 of 10 · 95 matches');
    expect(document.activeElement).toBe(buttons().next);
  });

  it('moves focus to the opposite button when the clicked one becomes disabled', async () => {
    render(<Pager total={20} pageSize={10} />);
    const { next } = buttons();
    await userEvent.click(next);
    // Landing on the last page disabled Next; focus moves to Previous.
    expect(screen.getByRole('status').textContent).toBe('Page 2 of 2 · 20 matches');
    const after = buttons();
    expect(after.next.disabled).toBe(true);
    expect(document.activeElement).toBe(after.prev);

    await userEvent.click(after.prev);
    // Back on page 1, Previous is now disabled; focus moves to Next.
    expect(screen.getByRole('status').textContent).toBe('Page 1 of 2 · 20 matches');
    expect(document.activeElement).toBe(buttons().next);
  });

  it('clamps the page count to at least one for an empty list', () => {
    render(<Pager total={0} pageSize={10} />);
    const { prev, next } = buttons();
    expect(screen.getByRole('status').textContent).toBe('Page 1 of 1 · 0 matches');
    expect(prev.disabled).toBe(true);
    expect(next.disabled).toBe(true);
  });
});
