import { describe, expect, it } from 'vitest';
import { fmtDuration, fmtUsage, outcomeLabel, runCause, runOutcome } from './runCauses';
import type { ScheduleRun } from '../types';

const r = (over: Partial<ScheduleRun>): ScheduleRun => ({ at: '2026-11-18T09:00:00Z', status: 'fired', ...over });

describe('runCauses', () => {
  it('reads the outcome, falling back to status for older daemons', () => {
    expect(runOutcome(r({ outcome: 'nothing' }))).toBe('nothing');
    expect(runOutcome(r({ status: 'error' }))).toBe('failed');
    expect(runOutcome(r({ status: 'skipped' }))).toBe('skipped');
    expect(runOutcome(r({}))).toBe('worked');
    // A task that ran to its end but reported it could not finish.
    expect(runOutcome(r({ status: 'fired', outcome: 'failed' }))).toBe('failed');
  });

  it('labels a prompt that worked as Sent, anything else as Worked', () => {
    expect(outcomeLabel(r({}), { kind: 'prompt' })).toBe('Sent');
    expect(outcomeLabel(r({}), { kind: undefined })).toBe('Sent');
    expect(outcomeLabel(r({}), { kind: 'task' })).toBe('Worked');
    expect(outcomeLabel(r({ outcome: 'nothing' }), { kind: 'task' })).toBe('Nothing to do');
  });

  it('gives each failure plain words and at most one button', () => {
    expect(runCause(r({ status: 'error', cause: 'claude-missing' }))?.action).toBe('install-claude');
    expect(runCause(r({ status: 'error', cause: 'folder-gone' }))?.action).toBe('pick-folder');
    expect(runCause(r({ status: 'error', cause: 'pane-gone' }))?.action).toBe('pick-pane');
    expect(runCause(r({ status: 'skipped', cause: 'usage-limit' }))?.action).toBeUndefined();
    // A failure whose cause offers nothing better offers its log.
    expect(runCause(r({ status: 'error', cause: 'already-running', logFile: 'run-1.log' }))?.action).toBe('open-log');
    // An unknown cause still says something.
    expect(runCause(r({ status: 'error', cause: 'something-new' }))).toEqual({ words: 'The run failed.', action: 'retry' });
    expect(runCause(r({}))).toBeUndefined();
    expect(runCause(r({ outcome: 'nothing' }))).toBeUndefined();
  });

  it('formats durations and usage', () => {
    expect(fmtDuration(42000)).toBe('42 s');
    expect(fmtDuration(185000)).toBe('3 min 5 s');
    expect(fmtDuration(3840000)).toBe('1 h 4 min');
    expect(fmtDuration(undefined)).toBe('');
    expect(fmtDuration(300)).toBe('');
    expect(fmtUsage(r({ usage: { inputTokens: 900, outputTokens: 50 } }))).toBe('950 tokens');
    expect(fmtUsage(r({ usage: { inputTokens: 0, outputTokens: 0, costUsd: 0.004 } }))).toBe('<$0.01');
    expect(fmtUsage(r({}))).toBe('');
  });
});
