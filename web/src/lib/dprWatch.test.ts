import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { watchDevicePixelRatio } from './dprWatch';

type Listener = () => void;

// Fake matchMedia: each query is its own list; `setDpr` fires `change` on the
// lists whose ratio stopped (or started) matching, as browsers do.
function installFakeMatchMedia() {
  const lists: { query: string; listeners: Set<Listener> }[] = [];
  const matchMedia = vi.fn((query: string) => {
    const entry = { query, listeners: new Set<Listener>() };
    lists.push(entry);
    return {
      media: query,
      matches: true,
      addEventListener: (_: string, fn: Listener) => entry.listeners.add(fn),
      removeEventListener: (_: string, fn: Listener) => entry.listeners.delete(fn),
    } as unknown as MediaQueryList;
  });
  const setDpr = (dpr: number) => {
    const old = window.devicePixelRatio;
    Object.defineProperty(window, 'devicePixelRatio', { configurable: true, value: dpr });
    for (const entry of lists.slice()) {
      if (entry.query === `(resolution: ${old}dppx)`) entry.listeners.forEach((fn) => fn());
    }
  };
  const activeListeners = () => lists.reduce((n, e) => n + e.listeners.size, 0);
  Object.defineProperty(window, 'matchMedia', { configurable: true, value: matchMedia });
  return { matchMedia, setDpr, activeListeners };
}

describe('watchDevicePixelRatio', () => {
  const originalMatchMedia = window.matchMedia;
  const originalDpr = window.devicePixelRatio;

  beforeEach(() => {
    Object.defineProperty(window, 'devicePixelRatio', { configurable: true, value: 1 });
  });
  afterEach(() => {
    Object.defineProperty(window, 'matchMedia', { configurable: true, value: originalMatchMedia });
    Object.defineProperty(window, 'devicePixelRatio', { configurable: true, value: originalDpr });
  });

  it('reports a scale change and re-arms for the next one', () => {
    const fake = installFakeMatchMedia();
    const onChange = vi.fn();
    watchDevicePixelRatio(onChange);
    expect(fake.matchMedia).toHaveBeenLastCalledWith('(resolution: 1dppx)');

    fake.setDpr(1.5);
    expect(onChange).toHaveBeenCalledWith(1.5);
    expect(fake.matchMedia).toHaveBeenLastCalledWith('(resolution: 1.5dppx)');

    fake.setDpr(2);
    expect(onChange).toHaveBeenCalledTimes(2);
    expect(onChange).toHaveBeenLastCalledWith(2);
    expect(fake.activeListeners()).toBe(1);
  });

  it('stops listening once disposed', () => {
    const fake = installFakeMatchMedia();
    const onChange = vi.fn();
    const dispose = watchDevicePixelRatio(onChange);
    dispose();
    fake.setDpr(2);
    expect(onChange).not.toHaveBeenCalled();
    expect(fake.activeListeners()).toBe(0);
  });

  it('is a no-op without matchMedia', () => {
    Object.defineProperty(window, 'matchMedia', { configurable: true, value: undefined });
    const dispose = watchDevicePixelRatio(vi.fn());
    expect(() => dispose()).not.toThrow();
  });
});
