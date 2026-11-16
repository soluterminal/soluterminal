import { describe, it, expect } from 'vitest';
import type { Schedule, ScheduleChange } from '../types';
import { changeActor, describeChange, pausedChange, pausedSince } from './scheduleChanges';

const base: Schedule = {
  id: 's1', name: 'Nightly', cron: '0 9 * * *', enabled: false,
  createdAt: '2026-11-01T00:00:00Z', fireCount: 0, skipCount: 0, errorCount: 0,
};
const byAgent: ScheduleChange = { at: '2026-11-14T10:20:00Z', action: 'disabled', by: '@cmo', paneId: 'p7', source: 'mcp' };
const byPerson: ScheduleChange = { at: '2026-11-14T10:25:00Z', action: 'enabled', by: 'person', source: 'ui' };
const byDaemon: ScheduleChange = { at: '2026-11-14T10:30:00Z', action: 'disabled', by: 'SoluTerminal', source: 'daemon', reason: 'its target pane is gone' };

describe('scheduleChanges', () => {
  it('names the actor by where the change came from', () => {
    expect(changeActor(byAgent)).toBe('the agent in @cmo');
    expect(changeActor(byPerson)).toBe('you, in the app');
    expect(changeActor(byDaemon)).toBe('SoluTerminal (its target pane is gone)');
    // A pane cannot pass for the person by its name: only the app's own
    // record reads as "you".
    expect(changeActor({ ...byAgent, by: 'person' })).toBe('the agent in person');
  });

  it('describes a change in one line', () => {
    expect(describeChange(byAgent)).toMatch(/^Paused .+ by the agent in @cmo$/);
    expect(describeChange(byPerson)).toMatch(/^Turned on .+ by you, in the app$/);
  });

  it('finds the current pause only while the automation is off', () => {
    expect(pausedChange({ ...base, changes: [byAgent, byPerson] })).toBe(byAgent);
    expect(pausedSince({ ...base, changes: [byAgent] })).toMatch(/^since .+ by the agent in @cmo$/);
    expect(pausedChange({ ...base, enabled: true, changes: [byAgent] })).toBeNull();
    expect(pausedChange({ ...base, changes: [byPerson] })).toBeNull();
    expect(pausedChange(base)).toBeNull();
    expect(pausedSince(base)).toBe('');
  });
});
