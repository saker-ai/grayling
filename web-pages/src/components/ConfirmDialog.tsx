import { createContext, useCallback, useContext, useRef, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, errMsg } from '@saker/grayling-client';
import { Button } from './Button';
import { Dialog } from './Dialog';

// ConfirmDialog is the legacy actionDialog as a Promise API: confirmAction()
// resolves with { reason } on confirm and null on cancel. It carries the
// legacy behaviors: optional reason field (required by default when a label
// is given), busy lock with disabled confirm, inline error feedback with
// role=alert, and a 401 from onConfirm settling as cancel (the session layer
// takes over).
export interface ConfirmOptions {
  title: string;
  // Optional body text (legacy actionDialog always had one; the dirty-editor
  // guard carries only a title, like the legacy window.confirm it replaces).
  summary?: string;
  preview?: ReactNode;
  reasonLabel?: string;
  reasonRequired?: boolean;
  confirmLabel?: string;
  onConfirm?: (args: { reason: string }) => Promise<void>;
}

export interface ConfirmResult {
  reason: string;
}

type Resolver = (value: ConfirmResult | null) => void;

interface PendingConfirm {
  options: ConfirmOptions;
  resolve: Resolver;
}

const ConfirmContext = createContext<(options: ConfirmOptions) => Promise<ConfirmResult | null>>(() =>
  Promise.resolve(null),
);

export function useConfirm(): (options: ConfirmOptions) => Promise<ConfirmResult | null> {
  return useContext(ConfirmContext);
}

export function ConfirmProvider({ children }: { children: ReactNode }): ReactElement {
  const [pending, setPending] = useState<PendingConfirm | null>(null);
  const resolverRef = useRef<Resolver | null>(null);

  const confirmAction = useCallback((options: ConfirmOptions) => {
    // Replacing a still-open dialog settles it as cancelled — a stacked
    // navigation guard must not leak its promise (or block forever).
    resolverRef.current?.(null);
    return new Promise<ConfirmResult | null>(resolve => {
      resolverRef.current = resolve;
      setPending({ options, resolve });
    });
  }, []);

  const settle = useCallback(
    (value: ConfirmResult | null) => {
      resolverRef.current = null;
      setPending(current => {
        current?.resolve(value);
        return null;
      });
    },
    [setPending],
  );

  return (
    <ConfirmContext.Provider value={confirmAction}>
      {children}
      {pending ? <ConfirmDialog options={pending.options} settle={settle} /> : null}
    </ConfirmContext.Provider>
  );
}

function ConfirmDialog({ options, settle }: { options: ConfirmOptions; settle: (value: ConfirmResult | null) => void }): ReactElement {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState('');
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const reasonRequired = options.reasonRequired ?? true;

  const confirm = async () => {
    const reason = reasonRef.current?.value.trim() || '';
    if (options.reasonLabel && reasonRequired && !reason) {
      reasonRef.current?.focus();
      return;
    }
    if (options.onConfirm) {
      setBusy(true);
      try {
        await options.onConfirm({ reason });
      } catch (e) {
        if ((e as ApiError).status === 401) {
          settle(null);
          return;
        }
        setFeedback(errMsg(e as Parameters<typeof errMsg>[0], t));
        setBusy(false);
        return;
      }
    }
    settle({ reason });
  };

  return (
    <Dialog
      title={options.title}
      describedBy="action-summary"
      busy={busy}
      onClose={() => settle(null)}
      footer={
        <>
          <Button data-x="cancel" onClick={() => settle(null)} disabled={busy}>
            {t('btn.cancel')}
          </Button>
          <Button variant="danger" onClick={() => void confirm()} disabled={busy}>
            {options.confirmLabel || t('btn.ok')}
          </Button>
        </>
      }
    >
      <p id="action-summary">{options.summary ?? ''}</p>
      {options.preview}
      {options.reasonLabel ? (
        <label className="field">
          <span>{options.reasonLabel}</span>
          <textarea ref={reasonRef} id="action-reason" rows={3} required={reasonRequired} autoFocus />
        </label>
      ) : null}
      <div id="action-feedback" role="alert">
        {feedback}
      </div>
    </Dialog>
  );
}
