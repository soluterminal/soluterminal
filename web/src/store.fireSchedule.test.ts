import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';
import { useStore } from './store';
import type { Schedule } from './types';

const sched: Schedule = {
  id: 'sch1', name: 'weekly review', cron: '0 9 * * 1', paneId: 'pane_1', prompt: 'review',
  enabled: true, createdAt: '2026-11-14T00:00:00Z', fireCount: 0, skipCount: 0, errorCount: 0,
} as Schedule;

describe('fireSchedule (Run now)', () => {
  const { toast } = useStore.getState();
  afterEach(() => {
    vi.restoreAllMocks();
    useStore.setState({ toast, schedules: [] });
  });

  it('says what the daemon answered and keeps the answer out of the stored row', async () => {
    const said = vi.fn();
    useStore.setState({ schedules: [sched], toast: said });
    vi.spyOn(api, 'fireSchedule').mockResolvedValue({
      ...sched, skipCount: 1, runNow: { outcome: 'skipped', detail: 'the pane is busy' },
    });

    await useStore.getState().fireSchedule('sch1');

    expect(said).toHaveBeenCalledWith('Skipped: weekly review · the pane is busy', expect.objectContaining({ kind: 'warn' }));
    const stored = useStore.getState().schedules[0];
    expect(stored.skipCount).toBe(1);
    expect(stored).not.toHaveProperty('runNow');
  });

  it('claims nothing when an older daemon sends no outcome', async () => {
    const said = vi.fn();
    useStore.setState({ schedules: [sched], toast: said });
    vi.spyOn(api, 'fireSchedule').mockResolvedValue({ ...sched });

    await useStore.getState().fireSchedule('sch1');

    expect(said).not.toHaveBeenCalled();
  });
});
