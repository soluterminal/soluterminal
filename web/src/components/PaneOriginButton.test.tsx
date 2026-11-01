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

  it('counts the workers its agent started on a pane you opened, and lists them', () => {
    const mine = pane({ id: 'mine', title: 'Planner', origin: { source: 'user', at: 1 } });
    const w1 = pane({ id: 'w1', title: 'Fix login', state: 'busy', origin: { source: 'agent', byPaneId: 'mine', leadPaneId: 'mine', at: 2 } });
    const w2 = pane({ id: 'w2', title: 'Write tests', origin: { source: 'agent', byPaneId: 'w1', leadPaneId: 'mine', at: 3 } });
    useStore.setState({ panes: { mine, w1, w2 }, layout: null, revealPane } as never);
    expect(workersOf('mine', { mine, w1, w2 }).map((p) => p.id)).toEqual(['w1', 'w2']);
    render(<PaneOriginButton pane={mine} />);
    // Having workers doesn't make a pane a lead: a lead is a role.
    const btn = screen.getByLabelText('2 workers. Show details');
    fireEvent.click(btn);
    expect(screen.getByText('Its workers')).toBeTruthy();
    expect(screen.getByText('working')).toBeTruthy();
    fireEvent.click(screen.getAllByText('Go to')[1]);
    expect(revealPane).toHaveBeenCalledWith('w2');
  });

  it('counts a worker\'s own workers for its lead, and labels nothing on a plain pane of yours', () => {
    const lead = pane({ id: 'lead', title: 'Claude' });
    const w = pane({ id: 'w', title: 'Fix', origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } });
    const sub = pane({ id: 's', title: 'Sub', origin: { source: 'agent', byPaneId: 'w', leadPaneId: 'w', at: 2 } });
    const panes = { lead, w, sub };
    // A worker is a task, not a lead: its sub-worker works for the nearest lead.
    expect(roleLabel(w, panes, ['lead', 'w', 's'])).toBe('Worker · lead #1 Claude');
    expect(roleLabel(sub, panes, ['lead', 'w', 's'])).toBe('Worker · lead #1 Claude');
    expect(workersOf('lead', panes).map(p => p.id)).toEqual(['w', 's']);
    expect(roleLabel(lead, { lead }, [])).toBe('');
    expect(roleLabel(pane({ origin: { source: 'api', at: 1 } }), {}, [])).toBe('Started by a program');
    expect(roleShortLabel(w, panes)).toBe('Worker');
    expect(roleShortLabel(lead, { lead, w })).toBe('1 worker');
  });

  it('names a lead by its role, with or without workers', () => {
    const cmo = pane({ id: 'cmo', title: 'CMO', origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'cmo' } });
    const social = pane({ id: 'social', title: 'Social', origin: { source: 'agent', byPaneId: 'cmo', leadPaneId: 'cmo', at: 2, teamKind: 'lead', leadRole: 'social' } });
    // leadPaneId stays the top of the chain; the worker's lead is its nearest one.
    const w = pane({ id: 'w', title: 'Post', origin: { source: 'agent', byPaneId: 'social', leadPaneId: 'cmo', at: 3, teamKind: 'worker' } });
    const sw = pane({ id: 'sw', title: 'Sub-step', origin: { source: 'agent', byPaneId: 'w', leadPaneId: 'cmo', at: 4, teamKind: 'worker' } });
    expect(workersOf('social', { cmo, social, w, sw }).map(p => p.id)).toEqual(['w', 'sw']);
    expect(roleLabel(sw, { cmo, social, w, sw }, ['cmo', 'social', 'w', 'sw'])).toBe('Worker · lead #2 Social');
    // A closed sub-lead no longer reads results: the top of the chain does, as in the daemon.
    const gone = { ...social, status: 'exited' as const };
    expect(roleLabel(sw, { cmo, social: gone, w, sw }, ['cmo', 'social', 'w', 'sw'])).toBe('Worker · lead #1 CMO');
    expect(roleLabel(cmo, { cmo }, [])).toBe('Lead · cmo');
    // A sub-lead is not counted as one of its parent's workers.
    expect(roleLabel(cmo, { cmo, social, w }, [])).toBe('Lead · cmo');
    expect(roleLabel(social, { cmo, social, w }, [])).toBe('Lead · social · 1 worker');
    expect(roleShortLabel(social, { cmo, social, w })).toBe('Lead · 1');
    expect(roleLabel(w, { cmo, social, w }, ['cmo', 'social', 'w'])).toBe('Worker · lead #2 Social');
    render(<PaneOriginButton pane={cmo} />);
    fireEvent.click(screen.getByLabelText('Lead · cmo. Show details'));
    expect(screen.getByText('Lead for cmo')).toBeTruthy();
  });
});
