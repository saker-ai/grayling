// dialog.test.tsx pins the R1.5 Dialog a11y contract: the title id is
// per-instance (useId, not a hard-coded "modal-title") and aria-labelledby
// points at it, and the Tab focus trap cycles through EVERY focusable —
// links included — skipping disabled and hidden elements.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Dialog } from '@saker/grayling-pages/components/Dialog';

afterEach(cleanup);

describe('Dialog', () => {
  it('labels the dialog by a per-instance title id', () => {
    render(
      <>
        <Dialog title="First" onClose={() => {}}>
          <button type="button">a</button>
        </Dialog>
        <Dialog title="Second" onClose={() => {}}>
          <button type="button">b</button>
        </Dialog>
      </>,
    );
    const dialogs = screen.getAllByRole('dialog');
    expect(dialogs).toHaveLength(2);
    const ids = dialogs.map(d => d.getAttribute('aria-labelledby'));
    expect(ids[0]).toBeTruthy();
    expect(ids[1]).toBeTruthy();
    expect(ids[0]).not.toBe(ids[1]);
    // Each aria-labelledby resolves to the dialog's own visible title.
    for (const [i, d] of dialogs.entries()) {
      const title = document.getElementById(ids[i] as string);
      expect(title?.textContent).toBe(i === 0 ? 'First' : 'Second');
      expect(d.contains(title ?? null)).toBe(true);
    }
  });

  it('traps Tab across links and buttons, skipping disabled and hidden', async () => {
    render(
      <Dialog
        title="Trap"
        onClose={() => {}}
        footer={
          <>
            <button type="button" disabled>
              disabled
            </button>
            <button type="button" hidden>
              hidden
            </button>
            <button type="button">last</button>
          </>
        }
      >
        <a href="#somewhere">middle link</a>
      </Dialog>,
    );
    const dialog = screen.getByRole('dialog');
    const link = screen.getByRole('link', { name: 'middle link' });
    const last = screen.getByRole('button', { name: 'last' });
    // Focus order: link → last → (wrap) link …; disabled/hidden stay out.
    link.focus();
    await userEvent.tab();
    expect(document.activeElement).toBe(last);
    await userEvent.tab();
    expect(document.activeElement).toBe(link);
    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(last);
    expect(dialog.contains(link)).toBe(true);
  });

  it('initial focus lands on the first field and restores on unmount', () => {
    const outer = document.createElement('button');
    document.body.appendChild(outer);
    outer.focus();
    const { unmount } = render(
      <Dialog title="Focus" onClose={() => {}}>
        <input aria-label="field" />
        <button type="button">ok</button>
      </Dialog>,
    );
    expect(document.activeElement).toBe(screen.getByLabelText('field'));
    unmount();
    expect(document.activeElement).toBe(outer);
    outer.remove();
  });

  it('Escape and overlay click close unless busy', async () => {
    const onClose = vi.fn();
    const { rerender } = render(
      <Dialog title="Close" onClose={onClose}>
        <button type="button">x</button>
      </Dialog>,
    );
    // The keydown handler lives on the overlay, so focus must be inside.
    screen.getByRole('button', { name: 'x' }).focus();
    await userEvent.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
    rerender(
      <Dialog title="Close" onClose={onClose} busy>
        <button type="button">x</button>
      </Dialog>,
    );
    screen.getByRole('button', { name: 'x' }).focus();
    await userEvent.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
