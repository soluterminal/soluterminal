import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { api } from '../api';
import { ScheduleHistoryDialog, ScheduleHistoryDeepLink } from './ScheduleHistoryDialog';
import type { Schedule } from '../types';

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 's1',
    name: 'Morning digest',
    kind: 'task',
    cron: '0 9 * * 1-5',
    cwd: '/home/me/project',
    permission: 'plan',
    prompt: 'Summarize overnight changes',
    enabled: true,
    createdAt: '2026-07-01T00:00:00Z',
    fireCount: 2,
    skipCount: 0,
    errorCount: 0,
    runs: [
      { at: '2026-07-10T09:00:00Z', status: 'fired', detail: 'plan · finished in 42s', logFile: 'run-100.log' },
      { at: '2026-07-09T09:00:00Z', status: 'fired', detail: 'plan · finished in 40s', logFile: 'run-99.log' },
    ],
    ...over,
  } as Schedule;
}

const TASK_LOG = [
  '# scheduled task · permission=plan · cwd=/home/me/project · started=2026-07-10T09:00:00Z',
  '# prompt: Summarize overnight changes',
  '',
  '# Overnight digest',
  'Nothing merged. CI green. Issue #12 needs your call.',
].join('\n');

afterEach(() => {
  useStore.setState({ schedules: [] });
  vi.restoreAllMocks();
});

describe('TaskReportReader', () => {
  // The whole point of tap-to-open: the newest report is readable
  // immediately, with nothing left to hunt for.
  it('loads the newest report on open, renders it, hides meta behind ⓘ', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);

    await waitFor(() => expect(logSpy).toHaveBeenCalledWith('s1', 'run-100.log'));
    // Only the newest — older runs load when paged to.
    expect(logSpy).toHaveBeenCalledTimes(1);

    // Report markdown renders as a document (heading, no literal '#').
    const heading = await screen.findByText('Overnight digest');
    expect(heading.className).toContain('report-h');
    expect(screen.getByText(/Issue #12 needs your call/)).toBeInTheDocument();

    // The daemon's technical header is NOT in the reading flow…
    expect(screen.queryByText(/permission=plan · cwd=/)).toBeNull();
    // …until the reader asks for it.
    fireEvent.click(screen.getByLabelText('Run details'));
    expect(screen.getByText(/permission=plan · cwd=/)).toBeInTheDocument();
    expect(screen.getByText(/Lifetime: ran 2×/)).toBeInTheDocument();
  });

  it('pages to older runs and fetches their reports lazily', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);
    await waitFor(() => expect(logSpy).toHaveBeenCalledTimes(1));

    expect(screen.getByText('1 of 2')).toBeInTheDocument();
    fireEvent.click(screen.getByText('‹ Older'));
    await waitFor(() => expect(logSpy).toHaveBeenCalledWith('s1', 'run-99.log'));
    expect(screen.getByText('2 of 2')).toBeInTheDocument();
    // At the oldest run the Older control is disabled, Newer is not.
    expect(screen.getByText('‹ Older')).toBeDisabled();
    expect(screen.getByText('Newer ›')).not.toBeDisabled();
  });

  it('marks the viewed run read, once, and never regresses the marker', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    const readSpy = vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    const { rerender } = render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);
    await waitFor(() => expect(readSpy).toHaveBeenCalledWith('s1', '2026-07-10T09:00:00Z'));
    expect(readSpy).toHaveBeenCalledTimes(1);
    // Poll reflects the marker → no duplicate POST.
    rerender(<ScheduleHistoryDialog schedule={sched({ lastReadAt: '2026-07-10T09:00:00Z' })} onClose={() => {}} />);
    // Paging back to an OLDER run must not POST (marker is already newer).
    fireEvent.click(screen.getByText('‹ Older'));
    await waitFor(() => expect(screen.getByText('2 of 2')).toBeInTheDocument());
    expect(readSpy).toHaveBeenCalledTimes(1);
  });

  it('"Act on this" spawns a seeded Claude pane in the schedule cwd', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    const createPane = vi.fn().mockResolvedValue({ id: 'p9' });
    useStore.setState({ createPane } as never);
    const onClose = vi.fn();
    render(<ScheduleHistoryDialog schedule={sched()} onClose={onClose} />);

    const btn = await screen.findByText('Act on this ↗');
    fireEvent.click(btn);
    await waitFor(() => expect(createPane).toHaveBeenCalledTimes(1));
    const opts = createPane.mock.calls[0][0];
    expect(opts.cwd).toBe('/home/me/project');
    expect(opts.cmd[0]).toBe('claude');
    // Seed carries the report body but NOT the daemon's technical header.
    expect(opts.cmd[1]).toContain('Issue #12 needs your call');
    expect(opts.cmd[1]).toContain('"Morning digest"');
    expect(opts.cmd[1]).not.toContain('permission=plan');
    expect(onClose).toHaveBeenCalled();
  });

  // Feed rows target specific runs — the reader must open ON that run, not
  // the newest, and unknown timestamps must fall back to the newest.
  it('opens at initialRunAt when given, falling back to newest for unknowns', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    const { unmount } = render(
      <ScheduleHistoryDialog schedule={sched()} onClose={() => {}} initialRunAt="2026-07-09T09:00:00Z" />,
    );
    await waitFor(() => expect(logSpy).toHaveBeenCalledWith('s1', 'run-99.log'));
    expect(screen.getByText('2 of 2')).toBeInTheDocument();
    unmount();

    render(
      <ScheduleHistoryDialog schedule={sched()} onClose={() => {}} initialRunAt="1999-01-01T00:00:00Z" />,
    );
    await waitFor(() => expect(logSpy).toHaveBeenCalledWith('s1', 'run-100.log'));
    expect(screen.getByText('1 of 2')).toBeInTheDocument();
  });

  it('shows a failure callout for error runs', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('partial output');
    render(
      <ScheduleHistoryDialog
        schedule={sched({
          runs: [{ at: '2026-07-10T09:00:00Z', status: 'error', detail: 'plan · exit 1 in 9s', logFile: 'run-100.log' }],
        })}
        onClose={() => {}}
      />,
    );
    await screen.findByText(/This run failed — plan · exit 1 in 9s/);
  });
});

describe('TaskReportReader load errors', () => {
  // A failed load used to leave "Loading…" up for good, with only a toast.
  it('says the report could not be loaded and loads it on Retry', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockRejectedValueOnce(new Error('HTTP 502'));
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);

    const failure = await screen.findByRole('alert');
    expect(failure).toHaveTextContent("Couldn't load this report: HTTP 502");
    expect(screen.queryByText('Loading the report…')).not.toBeInTheDocument();

    logSpy.mockResolvedValueOnce(TASK_LOG);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Overnight digest');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(logSpy).toHaveBeenCalledTimes(2);
  });

  it('shows loading while the report is on its way', async () => {
    let answer: (text: string) => void = () => {};
    vi.spyOn(api, 'scheduleRunLog').mockReturnValue(new Promise<string>((resolve) => { answer = resolve; }));
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);
    expect(await screen.findByRole('status')).toHaveTextContent('Loading the report…');
    await act(async () => { answer(TASK_LOG); });
    await screen.findByText('Overnight digest');
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('a failed report does not block paging to another run', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockRejectedValueOnce(new Error('HTTP 502')).mockResolvedValue(TASK_LOG);
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined);
    render(<ScheduleHistoryDialog schedule={sched()} onClose={() => {}} />);
    await screen.findByRole('alert');
    fireEvent.click(screen.getByText('‹ Older'));
    await screen.findByText('Overnight digest');
    expect(logSpy).toHaveBeenLastCalledWith('s1', 'run-99.log');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

describe('RunListDialog', () => {
  it('keeps a failed log on its row with Retry', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockRejectedValueOnce(new Error('HTTP 502'));
    render(
      <ScheduleHistoryDialog
        schedule={sched({ kind: 'agent', name: 'Fix agent' })}
        onClose={() => {}}
      />,
    );
    const failure = await screen.findByRole('alert');
    expect(failure).toHaveTextContent("Couldn't load this log: HTTP 502");

    logSpy.mockResolvedValueOnce('event=selftest status=green');
    fireEvent.click(within(failure).getByRole('button', { name: 'Retry' }));
    await screen.findByText(/event=selftest status=green/);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('says an automation has not run yet', () => {
    render(
      <ScheduleHistoryDialog
        schedule={sched({ kind: 'prompt', name: 'Morning prompt', runs: [] })}
        onClose={() => {}}
      />,
    );
    expect(screen.getByText("No runs yet: this automation hasn't run.")).toBeInTheDocument();
  });

  it('keeps the raw log list for non-task schedules', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('event=selftest status=green');
    render(
      <ScheduleHistoryDialog
        schedule={sched({ kind: 'agent', name: 'Fix agent' })}
        onClose={() => {}}
      />,
    );
    expect(screen.getByText(/Runs — Fix agent/)).toBeInTheDocument();
    await screen.findByText(/event=selftest status=green/);
  });
});

describe('ScheduleHistoryDeepLink', () => {
  // A notification tap can arrive before bootstrap has loaded schedules.
  // The host must wait for the store instead of dropping the intent.
  it('opens the reader when the schedule appears after the event', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue(TASK_LOG);
    render(<ScheduleHistoryDeepLink />);

    act(() => {
      window.dispatchEvent(new CustomEvent('solu:open-schedule-history', { detail: { id: 's1' } }));
    });
    // Schedule not loaded yet → nothing rendered, intent retained.
    expect(screen.queryByText(/Morning digest/)).toBeNull();

    act(() => {
      useStore.setState({ schedules: [sched()] });
    });
    await screen.findByText(/Report · Morning digest/);
  });
});
