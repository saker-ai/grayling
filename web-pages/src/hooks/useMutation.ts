import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { errMsg } from '@saker/grayling-client';
import type { ApiError } from '@saker/grayling-client';
import { useToast } from '../components/Toast';
import { useHost } from '../host';

// useMutation ports the legacy run() helper: success → ok toast + onSuccess
// (the caller's refresh/navigation); failure → err toast via errMsg, and a
// 403 additionally refreshes capabilities so gated controls hide.
export function useMutation(): <T>(
  fn: () => Promise<T>,
  okMsg: string | ((result: T) => string),
  onSuccess?: (result: T) => void,
) => Promise<T | undefined> {
  const { toast } = useToast();
  const { t } = useTranslation();
  const { refreshCapabilities } = useHost();

  return useCallback(
    async <T,>(
      fn: () => Promise<T>,
      okMsg: string | ((result: T) => string),
      onSuccess?: (result: T) => void,
    ): Promise<T | undefined> => {
      try {
        const result = await fn();
        toast(typeof okMsg === 'function' ? okMsg(result) : okMsg, true);
        onSuccess?.(result);
        return result;
      } catch (e) {
        toast(errMsg(e as ApiError, t), false);
        if ((e as ApiError).status === 403) await refreshCapabilities();
        return undefined;
      }
    },
    [toast, t, refreshCapabilities],
  );
}
