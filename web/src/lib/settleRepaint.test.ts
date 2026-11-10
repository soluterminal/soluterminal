import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createSettleRepaint, SETTLE_REPAINT_DELAY_MS } from './settleRepaint';

describe('settle repaint', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('repaints once after activity goes quiet', () => {
    const repaint = vi.fn();
    const settle = createSettleRepaint(repaint);
    settle.poke();
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS - 1);
    expect(repaint).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(repaint).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS * 4);
    expect(repaint).toHaveBeenCalledTimes(1);
  });

  it('does not repaint during a burst of output', () => {
    const repaint = vi.fn();
    const settle = createSettleRepaint(repaint);
    for (let i = 0; i < 40; i++) {
      settle.poke();
      vi.advanceTimersByTime(50);
    }
    expect(repaint).not.toHaveBeenCalled();
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS);
    expect(repaint).toHaveBeenCalledTimes(1);
  });

  it('repaints again after a later burst settles', () => {
    const repaint = vi.fn();
    const settle = createSettleRepaint(repaint);
    settle.poke();
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS);
    settle.poke();
    settle.poke();
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS);
    expect(repaint).toHaveBeenCalledTimes(2);
  });

  it('cancels a pending repaint and ignores pokes once disposed', () => {
    const repaint = vi.fn();
    const settle = createSettleRepaint(repaint);
    settle.poke();
    settle.dispose();
    settle.poke();
    vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS * 2);
    expect(repaint).not.toHaveBeenCalled();
  });

  it('swallows a repaint that throws', () => {
    const settle = createSettleRepaint(() => { throw new Error('disposed renderer'); });
    settle.poke();
    expect(() => vi.advanceTimersByTime(SETTLE_REPAINT_DELAY_MS)).not.toThrow();
  });
});
