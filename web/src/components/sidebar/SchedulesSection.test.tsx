import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useStore } from '../../store';
import { api } from '../../api';
import type { Schedule } from '../../types';
import { SchedulesSection } from './SchedulesSection';
import { useEntitlements } from '../../entitlements';

const mocks = vi.hoisted(() => ({ confirm: vi.fn() }));
vi.mock('../../lib/dialog', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/dialog')>()),
  confirmDialog: mocks.confirm,
}));

const iso = (ms: number) => new Date(ms).toISOString();

function schedule(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 'sched-agent',
    name: 'Fix agent cadence',
    cron: '*/15 * * * *',
    kind: 'agent',
    agentId: 'fix-agent',
    enabled: true,
    createdAt: iso(Date.now() - 3_600_000),
    nextFire: iso(Date.now() + 10 * 60_000),
    fireCount: 0,
    skipCount: 0,
    errorCount: 0,
    ...over,
  };
}

const agent = schedule();
const prompt = schedule({
  id: 'sched-prompt',
  name: 'Morning prompt',
  kind: 'prompt',
  agentId: undefined,
  paneId: 'missing-pane',
  prompt: 'summarize',
});
const task = schedule({
  id: 'sched-task',
  name: 'Nightly task',
  kind: 'task',
  agentId: undefined,
  cwd: '/tmp/project',
  permission: 'plan',
  prompt: 'review the project',
});

function rowFor(name: string): HTMLElement {
  const row = screen.getByText(name).closest<HTMLElement>('.sched-row');
  if (!row) throw new Error(`schedule row for ${name} not found`);
  return row;
}

function renderSection() {
  useStore.setState({
    schedules: [agent, prompt, task],
    panes: {},
    focusedPaneId: null,
    agentsPanelOpen: false,
    setScheduleEnabled: vi.fn().mockResolvedValue(undefined),
    fireSchedule: vi.fn().mockResolvedValue(undefined),
    snoozeSchedule: vi.fn().mockResolvedValue(undefined),
    deleteSchedule: vi.fn().mockResolvedValue(undefined),
    toast: vi.fn(),
  });
  return render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
}

describe('SchedulesSection agent cockpit links', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, tier: 'team', team: true, operator: true, account: true });
    useStore.setState({ schedules: [], panes: {}, agentsPanelOpen: false });
  });

  afterEach(() => {
    cleanup();
    useStore.setState({ schedules: [], panes: {}, agentsPanelOpen: false });
    vi.restoreAllMocks();
  });

  it('opens the Agents cockpit from an agent target chip only', () => {
    renderSection();

    const target = within(rowFor(agent.name)).getByTitle('Open the Agents cockpit');
    expect(target.tagName).toBe('BUTTON');
    fireEvent.click(target);
    expect(useStore.getState().agentsPanelOpen).toBe(true);

    const promptTarget = rowFor(prompt.name).querySelector('.sched-target');
    expect(promptTarget?.tagName).toBe('SPAN');
    expect(within(rowFor(prompt.name)).queryByTitle('Open the Agents cockpit')).not.toBeInTheDocument();
    expect(rowFor(task.name).querySelector('.sched-target')?.tagName).toBe('SPAN');
    expect(within(rowFor(task.name)).queryByTitle('Open the Agents cockpit')).not.toBeInTheDocument();
  });

  it('offers the Agents cockpit action only in agent schedule menus', () => {
    renderSection();

    fireEvent.click(within(rowFor(agent.name)).getByRole('button', { name: 'Automation actions' }));
    const cockpit = screen.getByRole('menuitem', { name: 'Agents cockpit…' });
    expect(cockpit).toHaveAttribute('title', 'Fleet health and PR queue — the ◇ popover');
    fireEvent.click(cockpit);
    expect(useStore.getState().agentsPanelOpen).toBe(true);

    useStore.setState({ agentsPanelOpen: false });
    fireEvent.click(within(rowFor(prompt.name)).getByRole('button', { name: 'Automation actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Agents cockpit…' })).not.toBeInTheDocument();
    expect(useStore.getState().agentsPanelOpen).toBe(false);

    fireEvent.click(within(rowFor(task.name)).getByRole('button', { name: 'Automation actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Agents cockpit…' })).not.toBeInTheDocument();
  });

  it('keeps row clicks opening runs and reports', () => {
    renderSection();

    fireEvent.click(screen.getByText(agent.name));
    expect(screen.getByRole('heading', { name: `Runs — ${agent.name}` })).toBeInTheDocument();
  });
});

describe('SchedulesSection housekeeping', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
  });
  afterEach(() => {
    cleanup();
    useStore.setState({ schedules: [], panes: {} });
  });

  function renderWith(schedules: Schedule[], deleteSchedule = vi.fn().mockResolvedValue(undefined)) {
    useStore.setState({ schedules, panes: {}, focusedPaneId: null, toast: vi.fn(), deleteSchedule });
    return render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
  }

  it('"N new" counts only enabled schedules; a paused one keeps its own dot', () => {
    const run = [{ at: iso(Date.now() - 60_000), status: 'fired' as const, summary: 'report' }];
    renderWith([
      { ...task, id: 't-on', name: 'Enabled digest', runs: run },
      { ...task, id: 't-off', name: 'Paused digest', enabled: false, runs: run },
    ]);
    expect(screen.getByRole('button', { name: '1 new' })).toBeInTheDocument();
    expect(within(rowFor('Paused digest')).getByLabelText('New report')).toBeInTheDocument();
  });

  it('shows why the daemon paused a schedule', () => {
    renderWith([{ ...prompt, enabled: false, pausedReason: 'its target pane is gone' }]);
    const tag = within(rowFor(prompt.name)).getByText(/paused: its target pane is gone/);
    expect(tag.getAttribute('title')).toMatch(/Paused automatically/);
  });

  it('a paused built-in agent shows a standing warning naming when and who', () => {
    const agent: Schedule = {
      ...prompt, id: 'pr', name: 'PR babysitter', kind: 'agent', agentId: 'pr-agent', paneId: undefined, prompt: undefined,
      enabled: false,
      changes: [{ at: '2026-11-14T10:20:00Z', action: 'disabled', by: '@cmo', paneId: 'p7', source: 'mcp' }],
    };
    renderWith([agent]);
    const tag = within(rowFor(agent.name)).getByText(/paused since .+ by the agent in @cmo/);
    expect(tag).toHaveClass('sched-warn');
    expect(tag.getAttribute('title')).toMatch(/built-in agent is paused since .+ It does nothing until you turn it back on/);
    expect(rowFor(agent.name).getAttribute('title')).toMatch(/Changes: Paused .+ by the agent in @cmo/);
  });

  it('a paused built-in agent with no record still warns', () => {
    renderWith([{ ...prompt, id: 'fx', name: 'Fix agent', kind: 'agent', agentId: 'fix-agent', enabled: false }]);
    const tag = rowFor('Fix agent').querySelector('.sched-paused-tag');
    expect(tag).toHaveClass('sched-warn');
    expect(tag?.textContent).toMatch(/paused$/);
  });

  it('an ordinary paused schedule names who paused it in its details only', () => {
    renderWith([{ ...prompt, enabled: false, changes: [{ at: '2026-11-14T10:20:00Z', action: 'disabled', by: 'person', source: 'ui' }] }]);
    const tag = within(rowFor(prompt.name)).getByText('· paused');
    expect(tag).not.toHaveClass('sched-warn');
    expect(tag.getAttribute('title')).toMatch(/Paused since .+ by you, in the app/);
  });

  it('a user-paused schedule still just says paused', () => {
    renderWith([{ ...prompt, enabled: false }]);
    expect(within(rowFor(prompt.name)).getByText('· paused')).toBeInTheDocument();
  });

  // A one-time automation the daemon switched off after its run cannot be
  // switched back on. It used to read "paused" with a checkbox that failed.
  const fired = [{ at: iso(Date.now() - 60_000), status: 'fired' as const }];
  const reminder: Schedule = {
    ...prompt, id: 'once-done', name: 'Call the dentist', cron: undefined,
    runAt: iso(Date.now() - 60_000), enabled: false, fireCount: 1, runs: fired,
  };
  const missed: Schedule = {
    ...prompt, id: 'once-missed', name: 'Water the plants', cron: undefined,
    runAt: iso(Date.now() - 3_600_000), enabled: false, fireCount: 0, skipCount: 1,
    runs: [{ at: iso(Date.now() - 60_000), status: 'skipped' as const, detail: 'missed while daemon was down' }],
  };

  it('shows a finished automation as done, below the live ones, with no checkbox and out of the header count', () => {
    renderWith([reminder, task, missed]);
    const rows = [...document.querySelectorAll('.sched-row')].map((r) => r.querySelector('.sched-name')?.textContent?.trim());
    expect(rows).toEqual(['Nightly task', 'Call the dentist · done', 'Water the plants · missed']);
    expect(within(rowFor(reminder.name)).queryByRole('checkbox')).toBeNull();
    expect(within(rowFor(task.name)).getByRole('checkbox')).toBeChecked();
    expect(rowFor(reminder.name).querySelector('.sched-next')).toHaveTextContent('done');
    expect(document.body.textContent).not.toMatch(/paused/);
    // One live automation: the header counts that one only.
    expect(document.querySelector('.sidebar-section')?.textContent).toMatch(/Automations\s*1/);
    expect(screen.getAllByText('Finished')).toHaveLength(1);
  });

  it('offers no Resume on a finished automation, but keeps Run now, Edit and Delete', () => {
    renderWith([reminder]);
    fireEvent.click(within(rowFor(reminder.name)).getByRole('button', { name: 'Automation actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Resume' })).toBeNull();
    expect(screen.queryByRole('menuitem', { name: 'Pause' })).toBeNull();
    for (const name of ['Run now', 'Edit…', 'History…', 'Delete']) {
      expect(screen.getByRole('menuitem', { name })).toBeInTheDocument();
    }
  });

  it('Clear finished removes every finished automation after one confirm, and leaves the rest', async () => {
    mocks.confirm.mockResolvedValueOnce(true);
    const deleteSchedule = vi.fn().mockResolvedValue(undefined);
    renderWith([reminder, task, missed, { ...prompt, id: 'user-paused', name: 'Paused by me', enabled: false }], deleteSchedule);
    fireEvent.click(screen.getByRole('button', { name: 'Clear finished' }));
    await vi.waitFor(() => expect(deleteSchedule).toHaveBeenCalledTimes(2));
    expect(deleteSchedule.mock.calls.map((c) => c[0])).toEqual(['once-done', 'once-missed']);
    expect(mocks.confirm).toHaveBeenCalledWith(expect.objectContaining({
      message: 'Clear 2 finished automations?\n\nTheir run history and reports are removed too.',
    }));
  });

  it('Clear finished does nothing when the confirm is declined', async () => {
    mocks.confirm.mockResolvedValueOnce(false);
    const deleteSchedule = vi.fn().mockResolvedValue(undefined);
    renderWith([reminder], deleteSchedule);
    fireEvent.click(screen.getByRole('button', { name: 'Clear finished' }));
    await vi.waitFor(() => expect(mocks.confirm).toHaveBeenCalled());
    await Promise.resolve();
    expect(deleteSchedule).not.toHaveBeenCalled();
  });

  it('a repeat that reached its run limit or end date is finished; one the person paused is not', () => {
    renderWith([
      { ...task, id: 'max', name: 'Three runs', enabled: false, maxFires: 3, fireCount: 3, runs: fired },
      { ...task, id: 'until', name: 'Ended last week', enabled: false, until: iso(Date.now() - 7 * 86_400_000), fireCount: 4, runs: fired },
      { ...task, id: 'paused', name: 'Paused by me', enabled: false, maxFires: 3, fireCount: 1 },
      { ...prompt, id: 'future', name: 'Tomorrow, paused', cron: undefined, runAt: iso(Date.now() + 86_400_000), enabled: false },
    ]);
    expect(within(rowFor('Three runs')).getByText('· done')).toBeInTheDocument();
    expect(within(rowFor('Ended last week')).getByText('· done')).toBeInTheDocument();
    expect(within(rowFor('Paused by me')).getByText('· paused')).toBeInTheDocument();
    expect(within(rowFor('Tomorrow, paused')).getByText('· paused')).toBeInTheDocument();
  });

  // The daemon switches a one-time automation off the moment it starts, so
  // off + past its time + still running is the normal state mid-run. Clearing
  // it then would delete the run's files under the running process.
  it('a one-time automation whose run is still going is not finished and cannot be cleared', () => {
    const deleteSchedule = vi.fn().mockResolvedValue(undefined);
    renderWith([{ ...reminder, id: 'once-going', name: 'Deploy check', fireCount: 0, runs: [], agentRunning: true }], deleteSchedule);
    const row = rowFor('Deploy check');
    expect(row.classList.contains('finished')).toBe(false);
    expect(row.querySelector('.sched-next')).toHaveTextContent('running…');
    expect(row.querySelector('.sched-name')?.textContent?.trim()).toBe('Deploy check');
    expect(screen.queryByRole('button', { name: 'Clear finished' })).toBeNull();
    expect(screen.queryByText('Finished')).toBeNull();
    // It still counts as one thing running here on its own.
    expect(document.querySelector('.sidebar-section')?.textContent).toMatch(/Automations\s*1/);
    // Nothing to resume: the daemon refuses to switch a spent one-time back on.
    expect(within(row).queryByRole('checkbox')).toBeNull();
    fireEvent.click(within(row).getByRole('button', { name: 'Automation actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Resume' })).toBeNull();
  });

  it('Delete says the automation will not run again, and that a run going now is not stopped', async () => {
    mocks.confirm.mockResolvedValue(false);
    renderWith([task, { ...task, id: 'going', name: 'Long audit', agentRunning: true }]);
    fireEvent.click(within(rowFor(task.name)).getByRole('button', { name: 'Automation actions' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await vi.waitFor(() => expect(mocks.confirm).toHaveBeenLastCalledWith(expect.objectContaining({
      message: `Delete automation "${task.name}"?\n\nIt will not run again, and its run history and reports are removed.`,
    })));
    fireEvent.click(within(rowFor('Long audit')).getByRole('button', { name: 'Automation actions' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await vi.waitFor(() => expect(mocks.confirm).toHaveBeenLastCalledWith(expect.objectContaining({
      message: 'Delete automation "Long audit"?\n\nIt will not run again, and its run history and reports are removed. The run going now is not stopped.',
    })));
  });

  it('a one-time automation paused until its time passed reads missed, with no run recorded', () => {
    renderWith([{ ...reminder, id: 'once-never', name: 'Renew the domain', fireCount: 0, runs: [] }]);
    expect(within(rowFor('Renew the domain')).getByText('· missed')).toBeInTheDocument();
    expect(rowFor('Renew the domain').querySelector('.sched-next')).toHaveTextContent('missed');
  });
});

// docs/DESIGN-agents-surface.md §7.2's board-steward row serves two shapes from
// one id, and the section only decides WHERE each one goes: the empty-state
// line inside the "nothing scheduled yet" block, the honesty line under the one
// row it is about.

// An empty list used to read "No automations yet" while the first load was
// still running and after it failed. Each of the three states now says what
// it is, and a failed load can be retried from the list.
describe('SchedulesSection loading and load errors', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    useStore.setState({ schedules: [], schedulesLoaded: false, schedulesError: null, panes: {}, focusedPaneId: null });
  });
  afterEach(() => {
    cleanup();
    useStore.setState({ schedules: [], schedulesLoaded: false, schedulesError: null, panes: {} });
    vi.restoreAllMocks();
  });

  it('says it is loading until the first load answers, then shows the empty state', async () => {
    let answer: (rows: Schedule[]) => void = () => {};
    vi.spyOn(api, 'listSchedules').mockReturnValue(new Promise<Schedule[]>((resolve) => { answer = resolve; }));
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    let load: Promise<void> = Promise.resolve();
    act(() => { load = useStore.getState().refreshSchedules(); });

    expect(screen.getByRole('status')).toHaveTextContent('Loading automations…');
    expect(screen.queryByText(/No automations yet/)).not.toBeInTheDocument();

    await act(async () => { answer([]); await load; });
    expect(screen.getByText(/No automations yet/)).toBeInTheDocument();
    expect(screen.queryByText('Loading automations…')).not.toBeInTheDocument();
  });

  it('says the load failed instead of "No automations yet", and Retry loads the list', async () => {
    const list = vi.spyOn(api, 'listSchedules').mockRejectedValueOnce(new Error('daemon unreachable'));
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    await act(async () => { await useStore.getState().refreshSchedules(); });

    expect(screen.getByRole('alert')).toHaveTextContent('Couldn’t load automations.');
    expect(screen.queryByText(/No automations yet/)).not.toBeInTheDocument();
    expect(screen.queryByText('Loading automations…')).not.toBeInTheDocument();

    list.mockResolvedValueOnce([task]);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.getByText('Nightly task')).toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('keeps the rows it has when a later refresh fails, and says they may be out of date', async () => {
    vi.spyOn(api, 'listSchedules').mockResolvedValueOnce([task]).mockRejectedValueOnce(new Error('timeout'));
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    await act(async () => { await useStore.getState().refreshSchedules(); });
    expect(screen.getByText('Nightly task')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    await act(async () => { await useStore.getState().refreshSchedules(); });
    expect(screen.getByText('Nightly task')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('Couldn’t refresh automations. This list may be out of date.');
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('calls a prompt automation whose pane is gone "(closed pane)"', () => {
    useStore.setState({ schedules: [prompt], schedulesLoaded: true });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    expect(rowFor('Morning prompt')).toHaveTextContent('(closed pane)');
  });

  it('says why a prompt automation is waiting for its pane', () => {
    useStore.setState({
      schedules: [
        schedule({ ...prompt, waitingForPane: true, waitingReason: 'you are typing in the pane' }),
        schedule({ ...prompt, id: 'sched-older', name: 'Older daemon', waitingForPane: true }),
      ],
      schedulesLoaded: true,
    });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    expect(rowFor('Morning prompt').querySelector('.sched-next')).toHaveTextContent('waiting: you are typing in the pane');
    expect(rowFor('Older daemon').querySelector('.sched-next')).toHaveTextContent('waiting for pane');
  });
});

describe('SchedulesSection kept schedules from removed agents', () => {
  const kept = schedule({
    id: 'sched-kept',
    name: 'workflow steward',
    kind: 'task',
    agentId: undefined,
    ownedByAgentId: 'agt_steward',
    cwd: '/tmp/project',
    permission: 'plan',
    prompt: 'steward the board',
  });
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    mocks.confirm.mockReset();
  });
  afterEach(() => {
    cleanup();
    useStore.setState({ schedules: [], panes: {} });
  });

  function renderKept(deleteSchedule = vi.fn().mockResolvedValue(undefined)) {
    useStore.setState({ schedules: [kept, task], panes: {}, focusedPaneId: null, toast: vi.fn(), deleteSchedule });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    return deleteSchedule;
  }

  it('marks the row, offers no switch, and leaves it out of the count', () => {
    renderKept();
    const row = rowFor(kept.name);
    expect(within(row).getByText(/kept from a removed agent/)).toBeInTheDocument();
    expect(within(row).getByText("doesn't run")).toBeInTheDocument();
    expect(row.querySelector('input[type="checkbox"]')).toBeNull();
    expect(rowFor(task.name).querySelector('input[type="checkbox"]')).not.toBeNull();
    expect(screen.getByText('1')).toBeInTheDocument();
  });

  it('offers only History and Delete, and says the prompt is saved first', async () => {
    const del = renderKept();
    fireEvent.click(within(rowFor(kept.name)).getByRole('button', { name: 'Automation actions' }));
    const items = screen.getAllByRole('menuitem').map((b) => b.textContent);
    expect(items).toEqual(['History…', 'Delete']);

    mocks.confirm.mockResolvedValueOnce(true);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await waitFor(() => expect(del).toHaveBeenCalledWith(kept.id));
    expect(mocks.confirm.mock.calls[0][0].message).toContain('agents-export-kept.md');
  });

  it('deletes nothing when the person cancels', async () => {
    const del = renderKept();
    fireEvent.click(within(rowFor(kept.name)).getByRole('button', { name: 'Automation actions' }));
    mocks.confirm.mockResolvedValueOnce(false);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await waitFor(() => expect(mocks.confirm).toHaveBeenCalled());
    expect(del).not.toHaveBeenCalled();
  });
});

describe('SchedulesSection limits (ST-299)', () => {
  afterEach(() => {
    useStore.setState({ schedules: [], panes: {} });
  });

  it('marks an automation from before the limits that runs more often than allowed', () => {
    useStore.setState({
      schedules: [schedule({ id: 'fast', name: 'Every minute', kind: 'task', agentId: undefined, cron: '* * * * *', cwd: '/p', permission: 'plan', tooFrequent: true })],
      panes: {},
    });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    expect(within(rowFor('Every minute')).getByText(/runs more often than allowed/)).toBeInTheDocument();
  });
});

describe('SchedulesSection pause all (ST-299)', () => {
  afterEach(() => {
    useStore.setState({ schedules: [], panes: {} });
    vi.restoreAllMocks();
  });

  it('pauses all automations from the header, and the banner resumes them', async () => {
    vi.spyOn(api, 'getPauseAll').mockResolvedValue({ paused: false });
    const set = vi.spyOn(api, 'setPauseAll')
      .mockResolvedValueOnce({ paused: true, since: new Date().toISOString(), by: 'person' })
      .mockResolvedValueOnce({ paused: false });
    useStore.setState({ schedules: [schedule({ id: 'a', name: 'Digest', kind: 'task', agentId: undefined, cwd: '/p', permission: 'plan' })], panes: {}, toast: vi.fn() });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Pause all automations' }));
    await waitFor(() => expect(set).toHaveBeenCalledWith(true));
    expect(await screen.findByText(/All automations are paused/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
    await waitFor(() => expect(set).toHaveBeenCalledWith(false));
  });

  it('marks a held automation', () => {
    vi.spyOn(api, 'getPauseAll').mockResolvedValue({ paused: true });
    useStore.setState({
      schedules: [schedule({ id: 'h', name: 'Held one', kind: 'task', agentId: undefined, cwd: '/p', permission: 'plan', enabled: false, pausedReason: 'all automations are paused; runs once when they resume' })],
      panes: {},
    });
    render(<SchedulesSection collapsed={false} onToggle={() => {}} />);
    expect(within(rowFor('Held one')).getByText(/· held/)).toBeInTheDocument();
  });
});
