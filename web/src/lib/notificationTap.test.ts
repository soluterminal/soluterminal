import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import { isInboxTapTarget, onServiceWorkerTapMessage, routeNotificationTap } from './notificationTap';
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
  vi.restoreAllMocks();
});

describe('routeNotificationTap', () => {
  it('leaves pane ids to the caller', () => {
    expect(routeNotificationTap('pane-123')).toBe(false);
    expect(inbox).toEqual([]);
    expect(history).toEqual([]);
  });

  it("opens a feedback tap on that report's thread and marks it read", () => {
    const read = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    expect(routeNotificationTap('feedback:42')).toBe(true);
    expect(inbox).toEqual([{ view: 'feed', thread: 42 }]);
    expect(read).toHaveBeenCalledWith(42);
  });

  it.each(['feedback:0', 'feedback:', 'feedback:abc', 'feedback:-3', 'feedback:4.5'])(
    'opens the Inbox feed for %s, which names no report',
    (target) => {
      const read = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
      expect(routeNotificationTap(target)).toBe(true);
      expect(inbox).toEqual([{ view: undefined, thread: undefined }]);
      expect(read).not.toHaveBeenCalled();
    },
  );

  it('names the targets it routes, so the arrival effect never polls for a pane called feedback:42', () => {
    expect(isInboxTapTarget('feedback:42')).toBe(true);
    expect(isInboxTapTarget('sched:s1')).toBe(true);
    expect(isInboxTapTarget('agent:agt_7')).toBe(true);
    expect(isInboxTapTarget('pane-123')).toBe(false);
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

describe('onServiceWorkerTapMessage', () => {
  // The worker hands a tap only to a window that answers this (sw.js tapReady).
  it('answers the service worker that the app is listening here', async () => {
    const { port1, port2 } = new MessageChannel();
    const answered = new Promise((resolve) => { port1.onmessage = (e) => resolve(e.data); });
    onServiceWorkerTapMessage(new MessageEvent('message', { data: { type: 'solu:notification-tap-ready' }, ports: [port2] }));
    expect(await answered).toBe('ready');
    port1.close();
    expect(inbox).toEqual([]);
  });

  it('opens the report a tapped feedback notification names', () => {
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    onServiceWorkerTapMessage(new MessageEvent('message', { data: { type: 'solu:notification-tap', paneId: 'feedback:42' } }));
    expect(inbox).toEqual([{ view: 'feed', thread: 42 }]);
  });

  it('ignores other messages', () => {
    onServiceWorkerTapMessage(new MessageEvent('message', { data: { type: 'solu:sw-activated' } }));
    onServiceWorkerTapMessage(new MessageEvent('message', { data: null }));
    expect(inbox).toEqual([]);
  });
});
