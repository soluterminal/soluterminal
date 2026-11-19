import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import { ScheduleRunHistory } from './ScheduleRunHistory';
import type { Schedule, ScheduleHistoryPage, ScheduleRun } from '../types';

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 's1',
    name: 'Standup',
    kind: 'prompt',
    cron: '0 9 * * 1-5',
    paneId: 'p1',
    prompt: 'standup',
    enabled: true,
    createdAt: '2026-07-01T00:00:00Z',
    fireCount: 1,
    skipCount: 0,
    errorCount: 0,
    runs: [],
    ...over,
  } as Schedule;
}

const today = new Date();
today.setHours(9, 0, 0, 0);
const yesterday = new Date(today.getTime() - 86400000);

function run(over: Partial<ScheduleRun>): ScheduleRun {
  return { at: today.toISOString(), status: 'fired', detail: 'delivered to pane', outcome: 'worked', ...over };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ScheduleRunHistory', () => {
  it('groups runs and changes by day, newest first, with each run\'s outcome', async () => {
    const page: ScheduleHistoryPage = {
      entries: [
        { type: 'run', at: today.toISOString(), run: run({ id: 'r2' }) },
        { type: 'change', at: yesterday.toISOString(), change: { at: yesterday.toISOString(), action: 'created', by: 'person', source: 'ui' } },
        { type: 'run', at: yesterday.toISOString(), run: run({ id: 'r1', at: yesterday.toISOString(), status: 'skipped', outcome: 'skipped', cause: 'snoozed', detail: 'snoozed' }) },
      ],
    };
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue(page);
    render(<ScheduleRunHistory schedule={sched()} />);
    const todayGroup = await screen.findByRole('region', { name: 'Today' });
    expect(within(todayGroup).getByText('Sent')).toBeInTheDocument();
    const yesterdayGroup = screen.getByRole('region', { name: 'Yesterday' });
    expect(within(yesterdayGroup).getByText('Skipped')).toBeInTheDocument();
    expect(within(yesterdayGroup).getByText('You snoozed this run.')).toBeInTheDocument();
    expect(within(yesterdayGroup).getByText(/^Created .* by you, in the app$/)).toBeInTheDocument();
  });

  it('shows the newest runs at once, before the first page arrives', () => {
    vi.spyOn(api, 'scheduleHistory').mockReturnValue(new Promise(() => {}));
    render(<ScheduleRunHistory schedule={sched({ runs: [run({ id: 'r1', detail: 'delivered to @work' })] })} />);
    expect(screen.getByText('delivered to @work')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Loading the history…');
  });

  it('pages with Load older and filters to failed runs', async () => {
    const spy = vi.spyOn(api, 'scheduleHistory')
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'new', detail: 'newest' }) }], next: '2026-07-10:3' })
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: yesterday.toISOString(), run: run({ id: 'old', at: yesterday.toISOString(), detail: 'older' }) }] })
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'bad', status: 'error', outcome: 'failed', cause: 'run-failed', detail: 'exit 1' }) }] });
    render(<ScheduleRunHistory schedule={sched()} />);
    await screen.findByText('newest');
    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    await screen.findByText('older');
    expect(spy).toHaveBeenNthCalledWith(2, 's1', { before: '2026-07-10:3', limit: 50, failedOnly: false });
    expect(screen.getByText('newest')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Load older' })).toBeNull();

    fireEvent.click(screen.getByRole('checkbox', { name: 'Failed only' }));
    await screen.findByText('exit 1');
    expect(spy).toHaveBeenLastCalledWith('s1', { before: undefined, limit: 50, failedOnly: true });
    expect(screen.queryByText('newest')).toBeNull();
  });

  it('says why a run failed and offers one button: Retry now runs it', async () => {
    const fireSchedule = vi.fn().mockResolvedValue(undefined);
    useStore.setState({ fireSchedule } as never);
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({
      entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'r', status: 'skipped', outcome: 'skipped', cause: 'pane-busy', detail: 'not sent' }) }],
    });
    render(<ScheduleRunHistory schedule={sched()} />);
    expect(await screen.findByText('The pane was busy.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Retry now' }));
    await waitFor(() => expect(fireSchedule).toHaveBeenCalledWith('s1'));
  });

  it('a closed pane offers Pick another pane, which opens the automation for editing', async () => {
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({
      entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'r', status: 'error', outcome: 'failed', cause: 'pane-gone', detail: 'paused' }) }],
    });
    const opened = vi.fn();
    window.addEventListener('solu:open-schedule-dialog', opened);
    const onClose = vi.fn();
    render(<ScheduleRunHistory schedule={sched()} onClose={onClose} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Pick another pane' }));
    expect(onClose).toHaveBeenCalled();
    expect((opened.mock.calls[0][0] as CustomEvent).detail).toEqual({ editId: 's1' });
    window.removeEventListener('solu:open-schedule-dialog', opened);
  });

  it('shows a task run\'s time, account, model and usage', async () => {
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({
      entries: [{
        type: 'run', at: today.toISOString(), run: run({
          id: 'r', detail: 'plan · finished in 42s', durationMs: 42000, trigger: 'run-now', account: 'work',
          model: 'claude-sonnet-4-5', usage: { inputTokens: 1200, outputTokens: 340, cacheReadTokens: 5000, costUsd: 0.0421 },
        }),
      }],
    });
    render(<ScheduleRunHistory schedule={sched({ kind: 'task', paneId: undefined, cwd: '/p', permission: 'plan' })} />);
    expect(await screen.findByText('42 s · Run now · work · claude-sonnet-4-5 · 6.5k tokens · $0.04')).toBeInTheDocument();
    expect(screen.getByText('Worked')).toBeInTheDocument();
  });

  it('keeps the newest runs and says so when the history cannot load', async () => {
    vi.spyOn(api, 'scheduleHistory').mockRejectedValue(new Error('HTTP 500'));
    render(<ScheduleRunHistory schedule={sched({ runs: [run({ id: 'r1', detail: 'delivered to @work' })] })} />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load the history: HTTP 500 (showing the newest runs)");
    expect(screen.getByText('delivered to @work')).toBeInTheDocument();
  });

  it('a task run opens its report in the reader', async () => {
    const r = run({ id: 'r', logFile: 'run-1.log', detail: 'plan · finished in 4s' });
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({ entries: [{ type: 'run', at: r.at, run: r }] });
    const onOpenReport = vi.fn();
    render(<ScheduleRunHistory schedule={sched({ kind: 'task' })} onOpenReport={onOpenReport} />);
    fireEvent.click(await screen.findByRole('button', { name: 'report' }));
    expect(onOpenReport).toHaveBeenCalledWith(r);
  });

  it('a new run is added at the top without dropping the older pages', async () => {
    const spy = vi.spyOn(api, 'scheduleHistory')
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'a', detail: 'first' }) }], next: 'cur:1' })
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: yesterday.toISOString(), run: run({ id: 'b', at: yesterday.toISOString(), detail: 'older page' }) }] })
      .mockResolvedValueOnce({
        entries: [
          { type: 'run', at: new Date(today.getTime() + 60000).toISOString(), run: run({ id: 'c', at: new Date(today.getTime() + 60000).toISOString(), detail: 'brand new' }) },
          { type: 'run', at: today.toISOString(), run: run({ id: 'a', detail: 'first' }) },
        ],
      });
    const s = sched({ runs: [run({ id: 'a', detail: 'first' })] });
    const view = render(<ScheduleRunHistory schedule={s} />);
    await screen.findByText('first');
    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    await screen.findByText('older page');
    view.rerender(<ScheduleRunHistory schedule={{ ...s, runs: [run({ id: 'c', detail: 'brand new' }), ...s.runs!] }} />);
    await screen.findByText('brand new');
    expect(screen.getByText('older page')).toBeInTheDocument();
    expect(screen.getAllByText('first')).toHaveLength(1);
    expect(spy).toHaveBeenCalledTimes(3);
  });

  it('groups by when each line was written: a run that crossed midnight is under the later day', async () => {
    const started = new Date(today.getTime() - 9.5 * 3600000); // 23:30 the day before
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({
      entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'x', at: started.toISOString(), detail: 'long run' }) }],
    });
    render(<ScheduleRunHistory schedule={sched()} />);
    const group = await screen.findByRole('region', { name: 'Today' });
    expect(within(group).getByText('long run')).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Yesterday' })).toBeNull();
  });

  it('a failed task run keeps its report button and offers no second way to the same log', async () => {
    const r = run({ id: 'f', status: 'fired', outcome: 'failed', cause: 'reported-failed', logFile: 'run-2.log', detail: 'could not finish' });
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({ entries: [{ type: 'run', at: r.at, run: r }] });
    const onOpenReport = vi.fn();
    render(<ScheduleRunHistory schedule={sched({ kind: 'task' })} onOpenReport={onOpenReport} />);
    fireEvent.click(await screen.findByRole('button', { name: 'report' }));
    expect(onOpenReport).toHaveBeenCalledWith(r);
    expect(screen.queryByRole('button', { name: 'Open log' })).toBeNull();
  });

  it('shows the row\'s on/off changes when the history cannot load', async () => {
    vi.spyOn(api, 'scheduleHistory').mockRejectedValue(new Error('HTTP 404'));
    render(<ScheduleRunHistory schedule={sched({ changes: [{ at: today.toISOString(), action: 'disabled', by: 'person', source: 'ui' }] })} />);
    await screen.findByRole('alert');
    expect(screen.getByText(/^Paused .* by you, in the app$/)).toBeInTheDocument();
    expect(screen.getByText("No runs yet: this automation hasn't run.")).toBeInTheDocument();
  });

  it('a refresh that lands after Failed only is ticked does not add other runs', async () => {
    let resolveRefresh: (p: ScheduleHistoryPage) => void = () => {};
    vi.spyOn(api, 'scheduleHistory')
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'a', detail: 'first' }) }] })
      .mockReturnValueOnce(new Promise<ScheduleHistoryPage>((res) => { resolveRefresh = res; }))
      .mockResolvedValueOnce({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'bad', status: 'error', outcome: 'failed', detail: 'exit 1' }) }] });
    const s = sched({ runs: [run({ id: 'a', detail: 'first' })] });
    const view = render(<ScheduleRunHistory schedule={s} />);
    await screen.findByText('first');
    view.rerender(<ScheduleRunHistory schedule={{ ...s, runs: [run({ id: 'ok', detail: 'worked fine' }), ...s.runs!] }} />);
    fireEvent.click(screen.getByRole('checkbox', { name: 'Failed only' }));
    await screen.findByText('exit 1');
    resolveRefresh({ entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'ok', detail: 'worked fine' }) }] });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText('worked fine')).toBeNull();
  });

  it('an automation kept from a removed managed agent offers its log, not Retry or Edit', async () => {
    vi.spyOn(api, 'scheduleHistory').mockResolvedValue({
      entries: [{ type: 'run', at: today.toISOString(), run: run({ id: 'k', status: 'error', outcome: 'failed', cause: 'pane-gone', detail: 'paused' }) }],
    });
    render(<ScheduleRunHistory schedule={sched({ ownedByAgentId: 'agt_1' } as Partial<Schedule>)} />);
    await screen.findByText('Its pane was closed, so the automation is paused.');
    expect(screen.queryByRole('button', { name: 'Pick another pane' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry now' })).toBeNull();
  });
});
