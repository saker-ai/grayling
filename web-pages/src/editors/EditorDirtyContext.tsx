import { createContext, useCallback, useContext, useEffect, useRef } from 'react';
import type { ReactElement, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useBlocker } from 'react-router-dom';
import { useConfirm } from '../components/ConfirmDialog';

// EditorDirtyContext is the React port of legacy confirmLeaveEditor()
// (web/legacy/app.js): the flag/segment detail pages publish their dirty flag
// through this context; route navigations are intercepted with useBlocker
// and in-page lifecycle actions (submit/approve/publish/back/…, where legacy
// called confirmLeaveEditor()) call confirmLeave() before acting.
//
// Semantics preserved from legacy:
//   * clean editor → confirmLeave() resolves true without any dialog;
//   * dirty editor → one confirm dialog (legacy window.confirm); dismissing
//     it (cancel/Escape/overlay) resolves false and NOTHING changes — neither
//     the URL nor the editor content;
//   * confirming resolves true and lets the caller proceed.
//
// useBlocker needs a data router; the shell uses createHashRouter and the
// test harness (web/src/__tests__/helpers.tsx) createMemoryRouter.
export interface EditorDirty {
  dirty: boolean;
  confirmLeave: () => Promise<boolean>;
}

const EditorDirtyContext = createContext<EditorDirty>({
  dirty: false,
  confirmLeave: () => Promise.resolve(true),
});

export function useEditorDirty(): EditorDirty {
  return useContext(EditorDirtyContext);
}

export function EditorDirtyProvider({
  dirty,
  message,
  children,
}: {
  dirty: boolean;
  // message overrides the generic confirm.unsaved title (the experiment
  // wizard carries the dedicated exp.wizardDiscard copy).
  message?: string;
  children: ReactNode;
}): ReactElement {
  const { t } = useTranslation();
  const confirm = useConfirm();
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;

  const confirmLeave = useCallback(async (): Promise<boolean> => {
    if (!dirtyRef.current) return true;
    const accepted = await confirm({ title: message || t('confirm.unsaved'), confirmLabel: t('btn.ok') });
    return accepted !== null;
  }, [confirm, message, t]);

  // Route guard: while dirty, any router navigation (sidebar, back,
  // deep-link edits of the hash) is held; the same dialog then proceeds or
  // resets. The ref indirection keeps the effect from re-opening the dialog
  // on unrelated re-renders of the blocked state.
  const blocker = useBlocker(dirty);
  const confirmLeaveRef = useRef(confirmLeave);
  confirmLeaveRef.current = confirmLeave;
  useEffect(() => {
    if (blocker.state !== 'blocked') return;
    let stale = false;
    void confirmLeaveRef.current().then(ok => {
      if (stale) return;
      if (ok) blocker.proceed?.();
      else blocker.reset?.();
    });
    return () => {
      stale = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [blocker.state]);

  return <EditorDirtyContext.Provider value={{ dirty, confirmLeave }}>{children}</EditorDirtyContext.Provider>;
}
