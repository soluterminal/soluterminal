import { describe, expect, it } from 'vitest';
import { isRestartPaused } from './restartPaused';

describe('isRestartPaused', () => {
  it('is true only for an exited, resumable pane the daemon marked paused', () => {
    expect(isRestartPaused({ status: 'exited', resumable: true, restartPaused: true })).toBe(true);
    // "Ask me": suspended the same way but not marked → the pane offers Resume.
    expect(isRestartPaused({ status: 'exited', resumable: true })).toBe(false);
    expect(isRestartPaused({ status: 'exited', resumable: false, restartPaused: true })).toBe(false);
    expect(isRestartPaused({ status: 'running', resumable: false, restartPaused: true })).toBe(false);
  });
});
