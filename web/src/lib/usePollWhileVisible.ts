import { useEffect, useRef } from 'react';

/**
 * Calls `run` every `intervalMs` while the page is visible, and once more
 * straight away when it becomes visible or regains focus (a phone coming
 * back from the lock screen or another app). Never overlaps two calls:
 * a tick that lands while the previous call is still running is skipped.
 */
export function usePollWhileVisible(run: () => Promise<unknown> | void, intervalMs: number, enabled = true): void {
  const runRef = useRef(run);
  runRef.current = run;
  useEffect(() => {
    if (!enabled) return;
    let inflight = false;
    const tick = () => {
      if (inflight || document.visibilityState === 'hidden') return;
      inflight = true;
      Promise.resolve()
        .then(() => runRef.current())
        .catch(() => undefined)
        .finally(() => { inflight = false; });
    };
    const timer = window.setInterval(tick, intervalMs);
    const onVisible = () => { if (document.visibilityState === 'visible') tick(); };
    document.addEventListener('visibilitychange', onVisible);
    window.addEventListener('focus', tick);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('focus', tick);
    };
  }, [intervalMs, enabled]);
}
