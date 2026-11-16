import { describe, it, expect } from 'vitest';
import { relativeTime } from './relativeTime';

describe('relativeTime', () => {
  const now = 1_000_000;
  it('reads "just now" within 5s', () => {
    expect(relativeTime(now - 1000, now)).toBe('just now');
    expect(relativeTime(now, now)).toBe('just now');
  });
  it('formats seconds / minutes / hours / days', () => {
    expect(relativeTime(now - 42_000, now)).toBe('42s ago');
    expect(relativeTime(now - 5 * 60_000, now)).toBe('5m ago');
    expect(relativeTime(now - 3 * 3_600_000, now)).toBe('3h ago');
    expect(relativeTime(now - 2 * 86_400_000, now)).toBe('2d ago');
  });
  it('falls back to "earlier" for undefined (replayed history)', () => {
    expect(relativeTime(undefined, now)).toBe('earlier');
  });
});
