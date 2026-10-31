import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { usePollWhileVisible } from './usePollWhileVisible';

function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
  document.dispatchEvent(new Event('visibilitychange'));
}

describe('usePollWhileVisible', () => {
  beforeEach(() => { vi.useFakeTimers(); setVisibility('visible'); });
  afterEach(() => { vi.useRealTimers(); setVisibility('visible'); });

  it('polls on an interval while visible and stops on unmount', async () => {
    const run = vi.fn().mockResolvedValue(undefined);
    const { unmount } = renderHook(() => usePollWhileVisible(run, 5000));
    expect(run).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(5000);
    expect(run).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(5000);
    expect(run).toHaveBeenCalledTimes(2);
    unmount();
    await vi.advanceTimersByTimeAsync(20000);
    expect(run).toHaveBeenCalledTimes(2);
  });

  it('skips ticks while hidden and refreshes as soon as the page is visible again', async () => {
    const run = vi.fn().mockResolvedValue(undefined);
    renderHook(() => usePollWhileVisible(run, 5000));
    setVisibility('hidden');
    await vi.advanceTimersByTimeAsync(15000);
    expect(run).not.toHaveBeenCalled();
    setVisibility('visible');
    await vi.advanceTimersByTimeAsync(0);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('never overlaps a slow call', async () => {
    let finish: () => void = () => undefined;
    const run = vi.fn(() => new Promise<void>((r) => { finish = r; }));
    renderHook(() => usePollWhileVisible(run, 1000));
    await vi.advanceTimersByTimeAsync(3000);
    expect(run).toHaveBeenCalledTimes(1);
    finish();
    await vi.advanceTimersByTimeAsync(1000);
    expect(run).toHaveBeenCalledTimes(2);
  });

  it('does nothing when disabled', async () => {
    const run = vi.fn();
    renderHook(() => usePollWhileVisible(run, 1000, false));
    window.dispatchEvent(new Event('focus'));
    await vi.advanceTimersByTimeAsync(5000);
    expect(run).not.toHaveBeenCalled();
  });
});
