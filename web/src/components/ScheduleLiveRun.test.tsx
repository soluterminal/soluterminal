import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { ScheduleLiveRun } from './ScheduleLiveRun';
import type { Schedule } from '../types';

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 's1', name: 'Digest', kind: 'task', cron: '0 9 * * *', cwd: '/p', permission: 'plan', prompt: 'p',
    enabled: true, createdAt: '2026-11-01T00:00:00Z', fireCount: 0, skipCount: 0, errorCount: 0, ...over,
  } as Schedule;
}

afterEach(() => vi.restoreAllMocks());

describe('ScheduleLiveRun', () => {
  it('shows nothing when no run is going', () => {
    const spy = vi.spyOn(api, 'scheduleLive');
    const { container } = render(<ScheduleLiveRun schedule={sched()} />);
    expect(container).toBeEmptyDOMElement();
    expect(spy).not.toHaveBeenCalled();
  });

  it('shows a task run live and stops it', async () => {
    vi.spyOn(api, 'scheduleLive').mockResolvedValue({
      running: true, runId: 'r', startedAt: new Date(Date.now() - 65000).toISOString(), model: 'claude-sonnet-4-5', text: 'Looking at the build now.',
    });
    const stop = vi.spyOn(api, 'stopSchedule').mockResolvedValue({ outcome: 'stopped' });
    render(<ScheduleLiveRun schedule={sched({ agentRunning: true })} />);
    expect(await screen.findByText('Looking at the build now.')).toBeInTheDocument();
    expect(screen.getByText(/^Running now · 1 min 5 s · claude-sonnet-4-5$/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Stop' }));
    await waitFor(() => expect(stop).toHaveBeenCalledWith('s1', { runId: 'r' }));
  });

  it('a prompt waiting for its pane can be stopped too', async () => {
    vi.spyOn(api, 'scheduleLive').mockResolvedValue({ running: false, waiting: 'the pane is busy' });
    render(<ScheduleLiveRun schedule={sched({ kind: 'prompt', waitingForPane: true, waitingReason: 'the pane is busy' })} />);
    expect(await screen.findByText('Waiting: the pane is busy')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
  });
});
