import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useStore } from '../../store';
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
