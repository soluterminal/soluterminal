import { describe, expect, it } from 'vitest';
import {
  duplicateNames,
  lastSeenText,
  machineBlocked,
  platformLabel,
  policyLabel,
  refusalText,
  REFUSED_TEXT,
  WOULD_REFUSE_TEXT,
} from './machinePolicy';

describe('machinePolicy', () => {
  it('labels the platform from the report, then the backend', () => {
    expect(platformLabel({ backend: '', platform: 'linux-wsl' })).toBe('Linux (WSL)');
    expect(platformLabel({ backend: 'win', platform: 'windows' })).toBe('Windows');
    expect(platformLabel({ backend: 'wsl' })).toBe('Linux (WSL)');
    expect(platformLabel({ backend: '' })).toBe('');
  });

  it('labels each policy, unknown as an older version', () => {
    expect(policyLabel('approved')).toBe('Only approved devices');
    expect(policyLabel('any')).toBe('Any device');
    expect(policyLabel('unknown')).toMatch(/older version/);
    expect(policyLabel('any', false)).toBe('Any device (last reported)');
    expect(policyLabel('unknown', false)).toMatch(/older version/);
    // A value from a newer cloud isn't claimed to be an older daemon.
    expect(policyLabel('locked' as never)).toBe('Policy unknown');
  });

  it('says blocked now, blocked soon, or nothing', () => {
    expect(refusalText({ backend: '', policy: 'any', refused: true, wouldRefuse: true })).toBe(REFUSED_TEXT);
    expect(refusalText({ backend: '', policy: 'unknown', wouldRefuse: true })).toBe(WOULD_REFUSE_TEXT);
    expect(refusalText({ backend: '', policy: 'approved' })).toBeNull();
    expect(REFUSED_TEXT).toMatch(/Only devices I approve/);
  });

  it('blocks a machine only when every daemon is refused', () => {
    expect(machineBlocked(undefined)).toBe(false);
    expect(machineBlocked([])).toBe(false);
    expect(machineBlocked([{ backend: 'wsl', policy: 'any', refused: true }])).toBe(true);
    expect(machineBlocked([
      { backend: 'wsl', policy: 'any', refused: true },
      { backend: 'win', policy: 'approved' },
    ])).toBe(false);
  });

  it('finds names shared by two or more machines', () => {
    expect(duplicateNames([{ name: 'PC' }, { name: 'pc ' }, { name: 'Laptop' }])).toEqual(['PC']);
    expect(duplicateNames([{ name: 'A' }, { name: 'B' }])).toEqual([]);
    expect(duplicateNames([{ name: '' }, { name: ' ' }])).toEqual([]);
  });

  it('renders last seen', () => {
    const now = Date.parse('2026-11-14T12:00:00Z');
    expect(lastSeenText(undefined, now)).toBeNull();
    expect(lastSeenText('nope', now)).toBeNull();
    expect(lastSeenText('2026-11-14T11:59:50Z', now)).toBe('last seen just now');
    expect(lastSeenText('2026-11-14T11:55:00Z', now)).toBe('last seen 5 min ago');
    expect(lastSeenText('2026-11-14T09:00:00Z', now)).toBe('last seen 3 h ago');
    expect(lastSeenText('2026-11-10T12:00:00Z', now)).toBe('last seen 4 days ago');
  });
});
