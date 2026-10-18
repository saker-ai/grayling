import { useEffect, useRef } from 'react';

// usePolling ports the legacy report auto-refresh loop (exp_workbench.js
// ~180-205): sequential ticks (the next tick is scheduled only after the
// current task settles), a 10s base interval doubling per failure
// (Math.min(60000, 10000 * 2 ** failures)), failures capped at 3 and reset
// on success, a hidden tab pauses polling, becoming visible triggers an
// immediate pull, and toggling `enabled` resets the backoff and pulls at
// once. Cleanup on unmount or disable clears the timer and detaches the
// visibility listener.
export interface PollingOptions {
  // enabled is the master switch (the legacy #exp-auto checkbox).
  enabled: boolean;
  // task runs one poll cycle and resolves true on success, false on
  // failure; it must never throw (legacy loadReport caught internally).
  task: () => Promise<boolean>;
}

export function usePolling({ enabled, task }: PollingOptions): void {
  const taskRef = useRef(task);
  taskRef.current = task;

  useEffect(() => {
    if (!enabled) return undefined;
    let active = true;
    let failures = 0;
    let polling = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const stopTimer = () => {
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }
    };

    const poll = async () => {
      if (!active || polling || document.visibilityState === 'hidden') return;
      polling = true;
      let ok = false;
      try {
        ok = await taskRef.current();
      } catch {
        ok = false;
      }
      polling = false;
      failures = ok ? 0 : Math.min(failures + 1, 3);
      // document.hidden re-reads the visibility without the TS narrowing the
      // early-return above put on document.visibilityState.
      if (active && !document.hidden) {
        timer = setTimeout(() => void poll(), Math.min(60000, 10000 * 2 ** failures));
      }
    };

    const onVisibility = () => {
      stopTimer();
      if (document.visibilityState !== 'hidden') void poll();
    };

    document.addEventListener('visibilitychange', onVisibility);
    void poll();
    return () => {
      active = false;
      stopTimer();
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [enabled]);
}
