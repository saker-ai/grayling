// polling.test.tsx pins usePolling (web-pages/src/hooks/usePolling.ts), the
// port of the legacy #exp-auto loop: immediate pull on enable, the legacy
// failure backoff (the first failure waits 10s * 2**1 = 20s, then 40s, then
// the failures cap of 3 pins min(60s, 10s * 2**3) = 60s), the reset to the
// 10s base on success, hidden-tab pause, immediate pull when the tab becomes
// visible again, and full cleanup on unmount/disable.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, render } from '@testing-library/react';
import { usePolling } from '@saker/grayling-pages/hooks/usePolling';
import type { ReactElement } from 'react';

function Probe({ enabled, task }: { enabled: boolean; task: () => Promise<boolean> }): ReactElement {
  usePolling({ enabled, task });
  return <div />;
}

function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true });
  Object.defineProperty(document, 'hidden', { value: state === 'hidden', configurable: true });
}

function visibilitychange() {
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'));
  });
}

// flush lets the in-flight poll() promise settle (and the follow-up timer be
// scheduled) without advancing the clock.
async function flush() {
  await act(async () => {
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(0);
  });
}

describe('usePolling', () => {
  afterEach(() => {
    vi.useRealTimers();
    setVisibility('visible');
  });

  it('polls immediately when enabled and backs off 20s/40s/60s on failure', async () => {
    vi.useFakeTimers();
    const task = vi.fn(() => Promise.resolve(false));
    render(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(1);

    // The legacy backoff increments before scheduling: the first failure
    // waits 10s * 2**1 = 20s, then 40s, then the failures cap of 3 pins
    // min(60s, 10s * 2**3) = 60s.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(19999);
    });
    expect(task).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(task).toHaveBeenCalledTimes(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(40000);
    });
    expect(task).toHaveBeenCalledTimes(3);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60000);
    });
    expect(task).toHaveBeenCalledTimes(4);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60000);
    });
    expect(task).toHaveBeenCalledTimes(5);
  });

  it('resets the backoff to 10s after a success', async () => {
    vi.useFakeTimers();
    let ok = false;
    const task = vi.fn(() => Promise.resolve(ok));
    render(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(1);
    ok = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20000);
    });
    expect(task).toHaveBeenCalledTimes(2);
    // After the success the next tick is back at the 10s base.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(9999);
    });
    expect(task).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(task).toHaveBeenCalledTimes(3);
  });

  it('stays quiet while hidden and pulls immediately on visible', async () => {
    vi.useFakeTimers();
    const task = vi.fn(() => Promise.resolve(true));
    render(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(1);

    setVisibility('hidden');
    visibilitychange(); // stops the pending timer
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120000);
    });
    expect(task).toHaveBeenCalledTimes(1);

    setVisibility('visible');
    visibilitychange(); // immediate pull
    await flush();
    expect(task).toHaveBeenCalledTimes(2);
  });

  it('stops after disable and cleans up on unmount', async () => {
    vi.useFakeTimers();
    const task = vi.fn(() => Promise.resolve(true));
    const view = render(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(1);

    view.rerender(<Probe enabled={false} task={task} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120000);
    });
    expect(task).toHaveBeenCalledTimes(1);

    view.rerender(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(2); // re-enable pulls at once
    view.unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120000);
    });
    expect(task).toHaveBeenCalledTimes(2);
  });

  it('treats a throwing task as a failure (never crashes the loop)', async () => {
    vi.useFakeTimers();
    const task = vi.fn(() => Promise.reject(new Error('boom')));
    render(<Probe enabled={true} task={task} />);
    await flush();
    expect(task).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20000);
    });
    expect(task).toHaveBeenCalledTimes(2);
  });
});
