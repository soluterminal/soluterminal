// The dismissed-notice keying that keeps the "interrupted — Continue" marker
// from reappearing for the SAME interruption.

import { describe, it, expect } from 'vitest';
import { fromKindLabel, switchNoticeKey, shouldShowSwitchNotice } from './SwitchNoticeBanner';

describe('fromKindLabel', () => {
  it('labels the kind switched away from (switchNotice.fromKind spelling)', () => {
    expect(fromKindLabel('native')).toBe('Focus');
    expect(fromKindLabel('terminal')).toBe('Terminal');
  });
});

describe('switchNoticeKey + shouldShowSwitchNotice', () => {
  const notice = { interrupted: true as const, fromKind: 'terminal' as const, at: 1717000000000 };

  it('keys a notice by its `at` timestamp', () => {
    expect(switchNoticeKey(notice)).toBe(1717000000000);
    expect(switchNoticeKey(null)).toBeNull();
    expect(switchNoticeKey(undefined)).toBeNull();
  });

  it('shows a live notice that has not been dismissed', () => {
    expect(shouldShowSwitchNotice(notice, new Set())).toBe(true);
  });

  it('hides a notice once its key is dismissed', () => {
    expect(shouldShowSwitchNotice(notice, new Set([notice.at]))).toBe(false);
  });

  it('shows a FRESH interruption (new `at`) even if an older one was dismissed', () => {
    const older = new Set([1717000000000]);
    const fresh = { ...notice, at: 1717000009999 };
    expect(shouldShowSwitchNotice(fresh, older)).toBe(true);
  });

  it('shows nothing when there is no notice', () => {
    expect(shouldShowSwitchNotice(null, new Set())).toBe(false);
    expect(shouldShowSwitchNotice(undefined, new Set())).toBe(false);
  });
});

describe('switchNotice reasons (resume-repair / fresh-start)', () => {
  it('a fresh-start (informational, interrupted:false) notice still keys and shows', () => {
    const freshStart = {
      interrupted: false,
      fromKind: 'native' as const,
      at: 1717000000123,
      reason: 'fresh-start' as const,
    };
    expect(switchNoticeKey(freshStart)).toBe(1717000000123);
    expect(shouldShowSwitchNotice(freshStart, new Set())).toBe(true);
    expect(shouldShowSwitchNotice(freshStart, new Set([freshStart.at]))).toBe(false);
  });

  it('a resume-repair notice behaves like an interrupted notice', () => {
    const repaired = {
      interrupted: true,
      fromKind: 'native' as const,
      at: 1717000000456,
      reason: 'resume-repair' as const,
    };
    expect(shouldShowSwitchNotice(repaired, new Set())).toBe(true);
  });
});
