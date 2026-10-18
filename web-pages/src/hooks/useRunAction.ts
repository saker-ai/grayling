// useRunAction ports legacy run() (web/legacy/app.js 239-251): the standard
// mutation wrapper for the detail views. Semantics preserved:
//   * unless savedEditor is given, confirmLeave() gates the action;
//   * success → optional savedEditor re-baseline, toast (string or result
//     mapper), reload of the view data (legacy route());
//   * failure → toast; a 403 additionally refreshes capabilities and reloads
//     (legacy refreshed and re-routed).
import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import { useToast } from '../components/Toast';
import { useHost } from '../host';
import { useEditorDirty } from '../editors/EditorDirtyContext';

export interface RunActionOptions {
  // savedEditor marks the action as editor-saving (legacy seg save): the
  // confirm gate is skipped and the callback re-baselines the dirty snapshot
  // after success instead.
  savedEditor?: () => void;
  // reload overrides the post-success reload (e.g. actions that navigate).
  reload?: () => void;
}

export function useRunAction(): <T>(
  fn: () => Promise<T>,
  okMsg: string | ((result: T) => string),
  opts?: RunActionOptions,
) => Promise<T | undefined> {
  const { t } = useTranslation();
  const { toast } = useToast();
  const { refreshCapabilities } = useHost();
  const { confirmLeave } = useEditorDirty();

  return useCallback(
    async <T,>(fn: () => Promise<T>, okMsg: string | ((result: T) => string), opts?: RunActionOptions): Promise<T | undefined> => {
      if (!opts?.savedEditor && !(await confirmLeave())) return undefined;
      try {
        const result = await fn();
        opts?.savedEditor?.();
        toast(typeof okMsg === 'function' ? okMsg(result) : okMsg);
        opts?.reload?.();
        return result;
      } catch (error) {
        toast(errMsg(error as Parameters<typeof errMsg>[0], t), false);
        if ((error as { status?: number }).status === 403) {
          try {
            await refreshCapabilities();
          } catch {
            // Keep the current view; the capability context retries lazily.
          }
          opts?.reload?.();
        }
        return undefined;
      }
    },
    [confirmLeave, refreshCapabilities, t, toast],
  );
}
