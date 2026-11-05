import { fireEvent, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { AttentionBell } from './AttentionBell';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

// The bell reads the real store through useAttentionBadge/useAttentionItems —
// seed panes directly and assert the badge count + the opened list rows
// (including its plain reason line) render from the same data.

function needsUserPane(id: string, over: Partial<PaneInfo> = {}): PaneInfo {
  return {
    id,
    title: `pane ${id}`,
    cmd: ['claude'],
    cwd: '/tmp',
    status: 'running',
    state: 'choice-prompt',
    attention: 'needs-user',
    lastActivityMs: Date.now(),
    ...over,
  } as PaneInfo;
}

describe('AttentionBell', () => {
  beforeEach(() => {
    useStore.setState({
      panes: {},
      workspaces: [],
      paneTails: {},
      ariaDismissedFingerprints: {},
    });
  });

  it('shows no badge when nothing needs the user', () => {
    render(<AttentionBell />);
    const btn = screen.getByRole('button', { name: /nothing is waiting/i });
    expect(btn.querySelector('.attention-bell-count')).toBeNull();
  });

  it('keeps routine worker notes out of the count, but includes an escalated person ask', () => {
    const note = { id: 'n1', text: 'Worker @x kept open: uncommitted work', source: '@lead', createdAt: 1 };
    useStore.setState({ panes: { lead: needsUserPane('lead', { state: 'idle', attention: 'none', workerNotices: [note] }) } });
    const { rerender } = render(<AttentionBell />);
    expect(screen.getByRole('button', { name: /nothing is waiting/i }).querySelector('.attention-bell-count')).toBeNull();
    useStore.setState({ panes: { lead: needsUserPane('lead', { state: 'idle', attention: 'none', asks: [{ ...note, text: 'Decide how to preserve unpublished work' }] }) } });
    rerender(<AttentionBell />);
    fireEvent.click(screen.getByRole('button', { name: /1 item is waiting/i }));
    expect(screen.getByText('Decide how to preserve unpublished work')).toBeTruthy();
  });

  it('uses plain reasons and ignores model fields from older daemons', () => {
    const legacy = { attentionSummary: 'old model summary', attentionRisk: 'high', attentionAction: 'old model action' };
    useStore.setState({
      panes: {
        p1: needsUserPane('p1', { ...legacy, attentionReason: 'This prompt needs your decision' }),
        p2: needsUserPane('p2', { state: 'awaiting-input' }),
        // auto-pending panes are auto-approval's problem, never the bell's.
        p3: needsUserPane('p3', { attention: 'auto-pending' }),
      },
    });
    render(<AttentionBell />);
    const btn = screen.getByRole('button', { name: /2 items are waiting/i });
    expect(btn.querySelector('.attention-bell-count')?.textContent).toBe('2');

    fireEvent.click(btn);
    expect(screen.getByText('Waiting on you')).toBeTruthy();
    expect(screen.getByText('pane p1')).toBeTruthy();
    expect(screen.getByText('pane p2')).toBeTruthy();
    expect(screen.queryByText('pane p3')).toBeNull();
    expect(screen.getByText('This prompt needs your decision')).toBeTruthy();
    expect(screen.queryByText('old model summary')).toBeNull();
    expect(screen.queryByText('old model action')).toBeNull();
    expect(screen.queryByText('high')).toBeNull();
  });

  it("lists a message stuck in an idle pane's input box until it is sent or cleared", () => {
    useStore.setState({
      panes: {
        w1: needsUserPane('w1', { state: 'idle', attention: 'none', unsentMessage: { id: 'm', sender: '#1 Claude' } }),
        // A pane already listed for a prompt is not listed twice.
        w2: needsUserPane('w2', { unsentMessage: { id: 'n', sender: '#1 Claude' } }),
      },
    });
    render(<AttentionBell />);
    const btn = screen.getByRole('button', { name: /2 items are waiting/i });
    fireEvent.click(btn);
    expect(screen.getByText('Message from #1 Claude not sent — Send or Clear it on the pane')).toBeTruthy();
    expect(screen.getAllByText(/pane w/).length).toBe(2);
  });

  it('still lists a stuck message when its pane\'s prompt row is dismissed', () => {
    useStore.setState({
      panes: { w3: needsUserPane('w3', { state: 'awaiting-input', unsentMessage: { id: 'm', sender: '#2 Codex' } }) },
      paneTails: { w3: { text: 'plain prose, no prompt here', ts: Date.now() } as never },
    });
    render(<AttentionBell />);
    fireEvent.click(screen.getByRole('button', { name: /1 item is waiting/i }));
    expect(screen.getByText('Message from #2 Codex not sent — Send or Clear it on the pane')).toBeTruthy();
  });

  it("lists each agent ask under its pane's daemon label, after prompts, and counts it", () => {
    const now = Date.now();
    useStore.setState({
      panes: {
        p1: needsUserPane('p1'),
        s1: needsUserPane('s1', {
          state: 'busy', attention: 'none',
          asks: [
            { id: 'a1', text: 'renew the bot token by 18:00', source: '@secure', createdAt: now - 120_000 },
            { id: 'a2', text: 'restart your PC', source: '@secure', createdAt: now - 60_000 },
          ],
        }),
      },
    });
    render(<AttentionBell />);
    const btn = screen.getByRole('button', { name: /3 items are waiting/i });
    expect(btn.querySelector('.attention-bell-count')?.textContent).toBe('3');
    fireEvent.click(btn);
    const rows = document.querySelectorAll('.attention-row');
    expect(rows.length).toBe(3);
    // The prompt first, then asks newest first.
    expect(rows[0].classList.contains('attention-choice')).toBe(true);
    expect(rows[1].textContent).toContain('restart your PC');
    expect(rows[2].textContent).toContain('renew the bot token by 18:00');
    expect(screen.getAllByText('@secure').length).toBe(2);
    expect(screen.getAllByRole('button', { name: 'Dismiss ask from @secure' }).length).toBe(2);
  });

  it('shows ask text as plain text, never markup', () => {
    useStore.setState({
      panes: {
        s1: needsUserPane('s1', {
          state: 'idle', attention: 'none',
          asks: [{ id: 'a1', text: '<b>paste your token</b> [here](http://x)', source: '#2 Claude', createdAt: Date.now() }],
        }),
      },
    });
    render(<AttentionBell />);
    fireEvent.click(screen.getByRole('button', { name: /1 item is waiting/i }));
    expect(screen.getByText('<b>paste your token</b> [here](http://x)')).toBeTruthy();
    expect(document.querySelector('.attention-row b, .attention-row a')).toBeNull();
  });
});
