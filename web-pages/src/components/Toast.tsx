import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

// Toasts, ported from legacy toast() (web/legacy/app.js): ok toasts fade at
// 3.8s and vanish at 4.3s; error toasts stay until dismissed and are capped
// at 5 (oldest dropped). The viewport container is #toasts with
// role=status aria-live=polite, exactly like the legacy markup.
interface ToastItem {
  id: number;
  msg: string;
  ok: boolean;
}

interface ToastContextValue {
  toast: (msg: string, ok?: boolean) => void;
}

const ToastContext = createContext<ToastContextValue>({ toast: () => {} });

export function useToast(): ToastContextValue {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: ReactNode }): ReactElement {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextId = useRef(0);

  const dismiss = useCallback((id: number) => {
    setItems(prev => prev.filter(item => item.id !== id));
  }, []);

  const toast = useCallback((msg: string, ok = true) => {
    const id = ++nextId.current;
    setItems(prev => {
      const next = [...prev, { id, msg, ok }];
      const errors = next.filter(item => !item.ok);
      if (errors.length > 5) {
        const drop = new Set(errors.slice(0, errors.length - 5).map(item => item.id));
        return next.filter(item => item.ok || !drop.has(item.id));
      }
      return next;
    });
  }, []);

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <ToastViewport items={items} onDismiss={dismiss} />
    </ToastContext.Provider>
  );
}

function ToastViewport({ items, onDismiss }: { items: ToastItem[]; onDismiss: (id: number) => void }): ReactElement {
  return (
    <div id="toasts" role="status" aria-live="polite" aria-atomic="false">
      {items.map(item => (
        <ToastMessage key={item.id} item={item} onDismiss={onDismiss} />
      ))}
    </div>
  );
}

function ToastMessage({ item, onDismiss }: { item: ToastItem; onDismiss: (id: number) => void }): ReactElement {
  const { t } = useTranslation();
  const [fading, setFading] = useState(false);

  useEffect(() => {
    if (!item.ok) return;
    const fade = setTimeout(() => setFading(true), 3800);
    const remove = setTimeout(() => onDismiss(item.id), 4300);
    return () => {
      clearTimeout(fade);
      clearTimeout(remove);
    };
  }, [item, onDismiss]);

  return (
    <div className={'toast ' + (item.ok ? 'ok' : 'err') + (fading ? ' fade' : '')}>
      {item.msg}
      {!item.ok && (
        <button type="button" className="btn sm toast-close" onClick={() => onDismiss(item.id)}>
          {t('btn.dismissError')}
        </button>
      )}
    </div>
  );
}
