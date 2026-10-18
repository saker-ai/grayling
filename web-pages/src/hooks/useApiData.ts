import { useCallback, useEffect, useRef, useState } from 'react';

// useApiData is the anti-staleness hook: the fetch runs under an
// AbortController owned by the effect, cleanup aborts it, and no setState
// happens after abort — a slow response can never overwrite a newer view
// (replaces the legacy routeEpoch/S.pollCleanup machinery).
export interface ApiDataState<T> {
  data: T | null;
  loading: boolean;
  error: unknown;
  reload: () => void;
}

export function useApiData<T>(fn: (signal: AbortSignal) => Promise<T>, deps: readonly unknown[]): ApiDataState<T> {
  const [state, setState] = useState<{ data: T | null; loading: boolean; error: unknown }>({
    data: null,
    loading: true,
    error: null,
  });
  const [epoch, setEpoch] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;

  useEffect(() => {
    const controller = new AbortController();
    setState(s => ({ ...s, loading: true, error: null }));
    fnRef.current(controller.signal).then(
      data => {
        if (!controller.signal.aborted) setState({ data, loading: false, error: null });
      },
      error => {
        if (!controller.signal.aborted) setState(s => ({ ...s, loading: false, error }));
      },
    );
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, epoch]);

  const reload = useCallback(() => setEpoch(e => e + 1), []);
  return { ...state, reload };
}
