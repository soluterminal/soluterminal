import { describe, expect, it } from 'vitest';
import { runNowToast } from './runNowToast';
import type { RunNowOutcome } from '../types';

const sched = { name: 'weekly review', paneId: 'pane_1' };

describe('runNowToast', () => {
  it('says Started, and where a prompt went', () => {
    expect(runNowToast(sched, { outcome: 'started', detail: 'delivered to @mobile' })).toEqual({
      message: 'Started: weekly review · delivered to @mobile',
      opts: { kind: 'success', icon: '⏰', paneId: 'pane_1' },
    });
    expect(runNowToast({ name: 'digest' }, { outcome: 'started' })?.message).toBe('Started: digest');
  });

  it('says Skipped and why, as a warning', () => {
    const said = runNowToast(sched, { outcome: 'skipped', detail: 'the pane is busy' });
    expect(said?.message).toBe('Skipped: weekly review · the pane is busy');
    expect(said?.opts.kind).toBe('warn');
  });

  it('says a run failed, as an error', () => {
    const said = runNowToast(sched, { outcome: 'failed', detail: 'claude not found' });
    expect(said?.message).toBe('Run failed: weekly review · claude not found');
    expect(said?.opts.kind).toBe('error');
  });

  it('never leaves the reason blank, and claims nothing for an unknown outcome', () => {
    expect(runNowToast(sched, { outcome: 'skipped' })?.message).toBe('Skipped: weekly review · the run did not start');
    expect(runNowToast(sched, { outcome: 'queued' } as unknown as RunNowOutcome)).toBeNull();
  });
});
