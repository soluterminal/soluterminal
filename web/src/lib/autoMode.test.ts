import { describe, expect, it, vi } from 'vitest';
import { AUTO_MODES, autoModeBadgeLabel, nextAutoMode } from './autoMode';

vi.mock('./dialog', () => ({ confirmDialog: vi.fn().mockResolvedValue(false) }));
import { confirmDialog } from './dialog';
import { confirmAutoModeChange } from './autoModeConfirm';

describe('autoMode', () => {
  it('gives each mode its own chip label, and off never says auto', () => {
    const labels = AUTO_MODES.map(autoModeBadgeLabel);
    expect(new Set(labels).size).toBe(3);
    expect(autoModeBadgeLabel('').toLowerCase()).not.toContain('auto');
  });

  it('the one-step toggle never steps into Always approve', () => {
    for (const mode of AUTO_MODES) expect(nextAutoMode(mode)).not.toBe('always');
    expect(nextAutoMode('')).toBe('cautious');
    expect(nextAutoMode('cautious')).toBe('');
    expect(nextAutoMode('always')).toBe('');
    // Repeated activation stays out of Always.
    let m = nextAutoMode('');
    for (let i = 0; i < 6; i++) { m = nextAutoMode(m); expect(m).not.toBe('always'); }
  });
});

describe('confirmAutoModeChange', () => {
  it('asks only when moving into Always approve', async () => {
    vi.mocked(confirmDialog).mockClear();
    expect(await confirmAutoModeChange('', 'cautious')).toBe(true);
    expect(await confirmAutoModeChange('always', '')).toBe(true);
    expect(await confirmAutoModeChange('always', 'always')).toBe(true);
    expect(confirmDialog).not.toHaveBeenCalled();
    expect(await confirmAutoModeChange('', 'always', 'Build')).toBe(false);
    expect(await confirmAutoModeChange('cautious', 'always')).toBe(false);
    expect(confirmDialog).toHaveBeenCalledTimes(2);
  });
});
