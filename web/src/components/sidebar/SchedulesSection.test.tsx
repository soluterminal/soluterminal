import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useStore } from '../../store';
import type { Schedule } from '../../types';
import { SchedulesSection } from './SchedulesSection';
import { useEntitlements } from '../../entitlements';

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

    fireEvent.click(within(rowFor(agent.name)).getByRole('button', { name: 'Schedule actions' }));
    const cockpit = screen.getByRole('menuitem', { name: 'Agents cockpit…' });
    expect(cockpit).toHaveAttribute('title', 'Fleet health and PR queue — the ◇ popover');
    fireEvent.click(cockpit);
    expect(useStore.getState().agentsPanelOpen).toBe(true);

    useStore.setState({ agentsPanelOpen: false });
    fireEvent.click(within(rowFor(prompt.name)).getByRole('button', { name: 'Schedule actions' }));
    expect(screen.queryByRole('menuitem', { name: 'Agents cockpit…' })).not.toBeInTheDocument();
    expect(useStore.getState().agentsPanelOpen).toBe(false);

    fireEvent.click(within(rowFor(task.name)).getByRole('button', { name: 'Schedule actions' }));
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

  function renderWith(schedules: Schedule[]) {
    useStore.setState({ schedules, panes: {}, focusedPaneId: null, toast: vi.fn() });
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
});

// docs/DESIGN-agents-surface.md §7.2's board-steward row serves two shapes from
// one id, and the section only decides WHERE each one goes: the empty-state
// line inside the "nothing scheduled yet" block, the honesty line under the one
// row it is about.
