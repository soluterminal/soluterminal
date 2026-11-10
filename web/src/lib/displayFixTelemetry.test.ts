import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  readDisplayFixTelemetry,
  recordDisplayFix,
  resetDisplayFixTelemetry,
} from './displayFixTelemetry';

const STORAGE_KEY = 'solu.displayFixes.v1';

describe('display fix telemetry', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    resetDisplayFixTelemetry();
    vi.setSystemTime(new Date('2026-11-08T01:00:00.000Z'));
  });

  afterEach(() => {
    resetDisplayFixTelemetry();
    vi.useRealTimers();
  });

  it('starts at zero', () => {
    expect(readDisplayFixTelemetry()).toEqual({
      version: 1,
      altR: 0,
      palette: 0,
      fixDisplay: 0,
      firstAt: '2026-11-08T01:00:00.000Z',
      lastAt: '2026-11-08T01:00:00.000Z',
    });
  });

  it('counts each source separately and updates lastAt', () => {
    recordDisplayFix('altR');
    recordDisplayFix('altR');
    recordDisplayFix('palette');
    vi.setSystemTime(new Date('2026-11-08T01:05:00.000Z'));
    recordDisplayFix('fixDisplay');
    expect(readDisplayFixTelemetry()).toMatchObject({
      altR: 2,
      palette: 1,
      fixDisplay: 1,
      firstAt: '2026-11-08T01:00:00.000Z',
      lastAt: '2026-11-08T01:05:00.000Z',
    });
  });

  it('persists to localStorage after a short delay', () => {
    recordDisplayFix('fixDisplay');
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
    vi.advanceTimersByTime(1000);
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '{}')).toMatchObject({ version: 1, fixDisplay: 1 });
  });

  it('reads back stored counts and clamps bad values', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ version: 1, altR: 3, palette: -4, fixDisplay: 'x', firstAt: '2026-11-01T00:00:00.000Z' }));
    expect(readDisplayFixTelemetry()).toMatchObject({ altR: 3, palette: 0, fixDisplay: 0, firstAt: '2026-11-01T00:00:00.000Z' });
  });

  it('starts fresh on corrupt storage', () => {
    localStorage.setItem(STORAGE_KEY, '{not json');
    expect(readDisplayFixTelemetry()).toMatchObject({ altR: 0, palette: 0, fixDisplay: 0 });
  });
});
