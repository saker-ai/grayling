import { useEffect, useId, useRef } from 'react';
import type { KeyboardEvent, ReactElement, ReactNode } from 'react';
import { createPortal } from 'react-dom';

// Dialog is the legacy overlay/modal (promptForm, actionDialog in
// web/legacy/app.js) as a component: role=dialog + aria-modal, a Tab focus
// trap, Escape and overlay-click close (both suppressed while busy), initial
// focus on the first field, and focus restored to the previously focused
// element on unmount.
export interface DialogProps {
  title: string;
  onClose: () => void;
  busy?: boolean;
  children: ReactNode;
  footer?: ReactNode;
  describedBy?: string;
}

// Everything keyboard-focusable, so the trap covers links and custom
// tabindex targets as well as form fields; disabled and hidden elements are
// filtered out of the cycle.
const FOCUSABLE = 'a[href], button, input, select, textarea, summary, [tabindex]:not([tabindex="-1"])';

export function Dialog({ title, onClose, busy = false, children, footer, describedBy }: DialogProps): ReactElement {
  const overlayRef = useRef<HTMLDivElement>(null);
  // The title id must be per-instance (two stacked dialogs share no id).
  const titleId = useId();
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const busyRef = useRef(busy);
  busyRef.current = busy;

  useEffect(() => {
    const previousFocus = document.activeElement as HTMLElement | null;
    const overlay = overlayRef.current;
    const first =
      overlay?.querySelector<HTMLElement>('input, select, textarea') ??
      overlay?.querySelector<HTMLElement>('[data-x="cancel"]');
    first?.focus();
    return () => {
      previousFocus?.focus?.();
    };
  }, []);

  const onKeyDown = (ev: KeyboardEvent<HTMLDivElement>) => {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      if (!busyRef.current) onCloseRef.current();
      return;
    }
    if (ev.key !== 'Tab') return;
    const focusables = Array.from(overlayRef.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []).filter(
      el => !el.hasAttribute('disabled') && !el.hidden,
    );
    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (ev.shiftKey && document.activeElement === first) {
      ev.preventDefault();
      last.focus();
    } else if (!ev.shiftKey && document.activeElement === last) {
      ev.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div
      className="overlay"
      ref={overlayRef}
      onKeyDown={onKeyDown}
      onClick={ev => {
        if (ev.target === ev.currentTarget && !busyRef.current) onCloseRef.current();
      }}
    >
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        {...(describedBy ? { 'aria-describedby': describedBy } : {})}
      >
        <h3 id={titleId}>{title}</h3>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-foot">{footer}</div> : null}
      </div>
    </div>,
    document.body,
  );
}
