import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { FIX_DISPLAY_REDRAW_DELAY_MS, useStore } from './store';

beforeEach(() => {
  vi.useFakeTimers();
  useStore.setState({ realigners: {}, redrawers: {} });
});
afterEach(() => {
  useStore.setState({ realigners: {}, redrawers: {} });
  vi.clearAllTimers();
  vi.useRealTimers();
});

it('Fix display realigns first, then forces a redraw once the resize settles', () => {
  const calls: string[] = [];
  useStore.getState().registerRealigner('k1', () => calls.push('realign'));
  useStore.getState().registerRedrawer('k1', () => calls.push('redraw'));

  useStore.getState().fixPaneDisplay('k1');
  expect(calls).toEqual(['realign']);
  vi.advanceTimersByTime(FIX_DISPLAY_REDRAW_DELAY_MS - 1);
  expect(calls).toEqual(['realign']);
  vi.advanceTimersByTime(1);
  expect(calls).toEqual(['realign', 'redraw']);
});

it('Fix display only touches the pane it was asked for', () => {
  const other = vi.fn();
  useStore.getState().registerRealigner('k2', other);
  useStore.getState().registerRedrawer('k2', other);
  useStore.getState().fixPaneDisplay('k1');
  vi.advanceTimersByTime(FIX_DISPLAY_REDRAW_DELAY_MS);
  expect(other).not.toHaveBeenCalled();
});
