import { fireEvent, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { insertPane } from '../lib/layout';
import { PaneOriginButton, livePaneLabel, originSummary, roleLabel, roleShortLabel, workersOf } from './PaneOriginButton';

function pane(extra: Partial<PaneInfo>): PaneInfo {
  return { id: 'w1', cwd: '/tmp', cmd: ['codex'], title: 'Engine deletion', status: 'running', createdAt: '', cols: 80, rows: 24, pid: 1, lastActivityMs: 0, state: 'idle', hasClaude: false, ...extra } as PaneInfo;
}

const revealPane = vi.fn(async () => {});

describe('PaneOriginButton', () => {
  beforeEach(() => {
    revealPane.mockClear();
    useStore.setState({ panes: { lead: pane({ id: 'lead', title: '#5 Claude' }) }, revealPane } as never);
  });

  it('shows nothing on panes the person opened or that predate origins', () => {
    const { container: a } = render(<PaneOriginButton pane={pane({ origin: { source: 'user', at: 1 } })} />);
    const { container: b } = render(<PaneOriginButton pane={pane({})} />);
    expect(a.innerHTML + b.innerHTML).toBe('');
  });

  it('opens a details card with the starter, purpose and a way back to the starter', () => {
    render(<PaneOriginButton pane={pane({ origin: { source: 'agent', byPaneId: 'lead', byPaneTitle: '#5 Claude', leadPaneId: 'lead', leadPaneTitle: '#5 Claude', purpose: 'Delete the engine', workspace: 'Personal', at: Date.now() } })} />);
    const btn = screen.getByLabelText('Worker · lead #5 Claude. Show details');
    expect(btn.querySelector('.pane-role-long')?.textContent).toBe('Worker · lead #5 Claude');
    expect(btn.querySelector('.pane-role-short')?.textContent).toBe('Worker');
    fireEvent.click(btn);
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByText('Delete the engine')).toBeTruthy();
    expect(screen.getByText('Personal')).toBeTruthy();
    fireEvent.click(screen.getByText('Go to #5 Claude'));
    expect(revealPane).toHaveBeenCalledWith('lead');
  });

  it('says when the starter has closed and closes on Escape', () => {
    render(<PaneOriginButton pane={pane({ origin: { source: 'agent', byPaneId: 'gone', byPaneTitle: '#9 Codex', leadPaneId: 'gone', at: 1 } })} />);
    fireEvent.click(screen.getByLabelText('Worker · lead closed. Show details'));
    expect(screen.getByText('closed')).toBeTruthy();
    expect(screen.queryByText('Go to #9 Codex')).toBeNull();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('names schedules and other programs honestly', () => {
    expect(originSummary({ source: 'schedule', at: 1 })).toBe('Started by a schedule');
    expect(originSummary({ source: 'api', at: 1 })).toBe('Started by another program');
  });

  it('names the starter as it is now: renamed, with its current number', () => {
    const lead = pane({ id: 'lead', title: 'Planner' });
    useStore.setState({ panes: { lead }, layout: insertPane(insertPane(null, 'x', null, null), 'lead', 'horizontal', 'x') } as never);
    render(<PaneOriginButton pane={pane({ origin: { source: 'agent', byPaneId: 'lead', byPaneTitle: '#1 Claude Code', leadPaneId: 'lead', at: 1 } })} />);
    expect(screen.getByLabelText('Worker · lead #2 Planner. Show details')).toBeTruthy();
    expect(livePaneLabel('gone', '#9 Codex', {}, [])).toBe('#9 Codex');
  });

  it('uses the stable handle in the existing starter chip and details', () => {
    const lead = pane({ id: 'lead', title: 'Planner', identity: { handle: 'release-lead' } });
    useStore.setState({ panes: { lead }, layout: { type: 'pane', paneId: 'lead' } } as never);
    render(<PaneOriginButton pane={pane({ origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } })} />);
    const chip = screen.getByLabelText('Worker · lead #1 @release-lead. Show details');
    expect(chip.title).toBe('Started by #1 @release-lead');
    fireEvent.click(chip);
    expect(screen.getByText('Go to #1 @release-lead')).toBeTruthy();
  });

  it('marks a pane you opened as the lead of the workers its agent started, and lists them', () => {
    const mine = pane({ id: 'mine', title: 'Planner', origin: { source: 'user', at: 1 } });
    const w1 = pane({ id: 'w1', title: 'Fix login', state: 'busy', origin: { source: 'agent', byPaneId: 'mine', leadPaneId: 'mine', at: 2 } });
    const w2 = pane({ id: 'w2', title: 'Write tests', origin: { source: 'agent', byPaneId: 'w1', leadPaneId: 'mine', at: 3 } });
    useStore.setState({ panes: { mine, w1, w2 }, layout: null, revealPane } as never);
    expect(workersOf('mine', { mine, w1, w2 }).map((p) => p.id)).toEqual(['w1', 'w2']);
    render(<PaneOriginButton pane={mine} />);
    const btn = screen.getByLabelText('Lead · 2 workers. Show details');
    fireEvent.click(btn);
    expect(screen.getByText('Its workers')).toBeTruthy();
    expect(screen.getByText('working')).toBeTruthy();
    fireEvent.click(screen.getAllByText('Go to')[1]);
    expect(revealPane).toHaveBeenCalledWith('w2');
  });

  it('labels a worker that leads others, and nothing on a plain pane of yours', () => {
    const lead = pane({ id: 'lead', title: 'Claude' });
    const w = pane({ id: 'w', title: 'Fix', origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } });
    const sub = pane({ id: 's', title: 'Sub', origin: { source: 'agent', byPaneId: 'w', leadPaneId: 'w', at: 2 } });
    const panes = { lead, w, sub };
    expect(roleLabel(w, panes, ['lead', 'w', 's'])).toBe('Worker · lead #1 Claude · 1 worker');
    expect(roleLabel(lead, { lead }, [])).toBe('');
    expect(roleLabel(pane({ origin: { source: 'api', at: 1 } }), {}, [])).toBe('Started by a program');
    expect(roleShortLabel(w, panes)).toBe('Worker · 1');
    expect(roleShortLabel(lead, { lead, w })).toBe('Lead · 1');
  });
});
