import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { routeNotificationTap } from './notificationTap';
import type { Schedule } from '../types';

function sched(id: string, kind: Schedule['kind']): Schedule {
  return {
    id, name: id, kind, enabled: true, createdAt: '2026-07-01T00:00:00Z',
    fireCount: 0, skipCount: 0, errorCount: 0,
  } as Schedule;
}

let inbox: unknown[];
let history: unknown[];
const onInbox = (e: Event) => inbox.push((e as CustomEvent).detail);
const onHistory = (e: Event) => history.push((e as CustomEvent).detail);

beforeEach(() => {
  inbox = [];
  history = [];
  window.addEventListener('solu:open-inbox', onInbox);
  window.addEventListener('solu:open-schedule-history', onHistory);
});

afterEach(() => {
  window.removeEventListener('solu:open-inbox', onInbox);
  window.removeEventListener('solu:open-schedule-history', onHistory);
  useStore.setState({ schedules: [] });
  vi.useRealTimers();
});

describe('routeNotificationTap', () => {
  it('leaves pane ids to the caller', () => {
    expect(routeNotificationTap('pane-123')).toBe(false);
    expect(inbox).toEqual([]);
    expect(history).toEqual([]);
  });

  it('opens an agent tap on its Inbox decision immediately, without waiting for a pane', () => {
    expect(routeNotificationTap('agent:agt_7')).toBe(true);
    expect(inbox).toEqual([{ target: { agent: 'agt_7' } }]);
  });

  it('opens a task automation on its Inbox report', () => {
    useStore.setState({ schedules: [sched('s1', 'task')] });
    expect(routeNotificationTap('sched:s1')).toBe(true);
    expect(inbox).toEqual([{ target: { schedule: 's1' } }]);
    expect(history).toEqual([]);
  });

  it('keeps a prompt automation on its run history (it has no Inbox row)', () => {
    useStore.setState({ schedules: [sched('p1', 'prompt')] });
    routeNotificationTap('sched:p1');
    expect(history).toEqual([{ id: 'p1' }]);
    expect(inbox).toEqual([]);
  });

  it('waits for the schedule list on a cold start, then routes', () => {
    routeNotificationTap('sched:s1');
    expect(inbox).toEqual([]);
    useStore.setState({ schedules: [sched('s1', 'task')] });
    expect(inbox).toEqual([{ target: { schedule: 's1' } }]);
    // Settled: later list changes do not re-open anything.
    useStore.setState({ schedules: [sched('s1', 'task'), sched('s2', 'task')] });
    expect(inbox).toHaveLength(1);
  });

  it('falls back to the history dialog when the schedule never appears', () => {
    vi.useFakeTimers();
    routeNotificationTap('sched:gone');
    vi.advanceTimersByTime(10_001);
    expect(history).toEqual([{ id: 'gone' }]);
    expect(inbox).toEqual([]);
  });
});
