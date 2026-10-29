import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from './api';
import { useStore } from './store';
import type { Schedule } from './types';

function sched(id: string, over: Partial<Schedule> = {}): Schedule {
  return {
    id, name: id, kind: 'task', enabled: true, createdAt: '2026-07-01T00:00:00Z',
    fireCount: 0, skipCount: 0, errorCount: 0, ...over,
  } as Schedule;
}

afterEach(() => {
  useStore.setState({ schedules: [] });
  vi.restoreAllMocks();
});

describe('refreshSchedule (one row per run event)', () => {
  it('patches just that row in place and never reloads the full list', async () => {
    const list = vi.spyOn(api, 'listSchedules');
    const other = sched('b');
    useStore.setState({ schedules: [sched('a'), other] });
    vi.spyOn(api, 'getSchedule').mockResolvedValue(sched('a', { fireCount: 1 }));

    await useStore.getState().refreshSchedule('a');

    const after = useStore.getState().schedules;
    expect(after.map((s) => s.id)).toEqual(['a', 'b']);
    expect(after[0].fireCount).toBe(1);
    expect(after[1]).toBe(other); // untouched rows keep their identity
    expect(list).not.toHaveBeenCalled();
  });

  it('leaves the store alone when the row did not change', async () => {
    const before = [sched('a')];
    useStore.setState({ schedules: before });
    vi.spyOn(api, 'getSchedule').mockResolvedValue(sched('a'));
    await useStore.getState().refreshSchedule('a');
    expect(useStore.getState().schedules).toBe(before);
  });

  it('drops a row the daemon no longer shows (404), and adds one it has not seen', async () => {
    useStore.setState({ schedules: [sched('a'), sched('gone')] });
    vi.spyOn(api, 'getSchedule').mockRejectedValueOnce(new ApiError(404, 'schedule not found'));
    await useStore.getState().refreshSchedule('gone');
    expect(useStore.getState().schedules.map((s) => s.id)).toEqual(['a']);

    vi.spyOn(api, 'getSchedule').mockResolvedValueOnce(sched('new'));
    await useStore.getState().refreshSchedule('new');
    expect(useStore.getState().schedules.map((s) => s.id)).toEqual(['a', 'new']);
  });

  it('coalesces a burst into one fetch plus ONE trailing fetch, so the last change always lands', async () => {
    useStore.setState({ schedules: [sched('a')] });
    // The first response was taken while the run was still going.
    const get = vi.spyOn(api, 'getSchedule')
      .mockResolvedValueOnce(sched('a', { agentRunning: true }))
      .mockResolvedValue(sched('a', { agentRunning: false, fireCount: 1 }));
    await Promise.all([
      useStore.getState().refreshSchedule('a'),
      useStore.getState().refreshSchedule('a'), // the run ended mid-fetch
      useStore.getState().refreshSchedule('a'),
    ]);
    await vi.waitFor(() => expect(useStore.getState().schedules[0].agentRunning).toBe(false));
    expect(get).toHaveBeenCalledTimes(2);
  });

  it('an event during a full-list refresh still gets its row refetched after it', async () => {
    useStore.setState({ schedules: [sched('a')] });
    let release!: (v: Schedule[]) => void;
    vi.spyOn(api, 'listSchedules').mockReturnValue(new Promise((r) => { release = r; }));
    const get = vi.spyOn(api, 'getSchedule').mockResolvedValue(sched('a', { fireCount: 5 }));
    const full = useStore.getState().refreshSchedules();
    const row = useStore.getState().refreshSchedule('a');
    release([sched('a', { agentRunning: true })]);
    await Promise.all([full, row]);
    await vi.waitFor(() => expect(useStore.getState().schedules[0].fireCount).toBe(5));
    expect(get).toHaveBeenCalledTimes(1);
  });

  it('falls back to the full list on a daemon without the single-row route', async () => {
    useStore.setState({ schedules: [sched('a')] });
    vi.spyOn(api, 'getSchedule').mockRejectedValue(new ApiError(405, 'method not allowed'));
    const list = vi.spyOn(api, 'listSchedules').mockResolvedValue([sched('a', { fireCount: 3 })]);
    await useStore.getState().refreshSchedule('a');
    expect(list).toHaveBeenCalledTimes(1);
    expect(useStore.getState().schedules[0].fireCount).toBe(3);
  });
});
