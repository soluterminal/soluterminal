import { createRef } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { PaneContextMenu } from './PaneContextMenu';

vi.mock('./SmartApprovalsHint', () => ({
  SmartApprovalsHint: () => <div role="note">Smart approvals test hint</div>,
}));

// The delegation toggle is the one control the pane menu gained for the
// delegation feedback loop. These cases pin what a user can tell from looking
// at it, and what pressing it sends.

function renderMenu(pane: Partial<PaneInfo>, props: Partial<Parameters<typeof PaneContextMenu>[0]> = {}) {
  const noop = () => {};
  return render(
    <PaneContextMenu
      pane={{ id: 'pane-1', title: 'Orchestrator', status: 'running', cmd: ['claude'], ...pane } as PaneInfo}
      x={0}
      y={0}
      menuRef={createRef<HTMLDivElement>()}
      isClaudePane
      onSplitRight={noop}
      onSplitRightShellHere={noop}
      onSplitRightClaudeHere={noop}
      onSplitDown={noop}
      onClonePane={noop}
      onNewSession={noop}
      onCreateSchedule={noop}
      onSetAutoMode={noop}
      onEditFlagMemo={noop}
      onUnflag={noop}
      onFlagForAttention={noop}
      onRename={noop}
      onClosePane={noop}
      {...props}
    />,
  );
}

const ITEM = 'Announce delegated-work updates';

describe('PaneContextMenu workspace moves', () => {
  it('offers moving an ordinary running pane', () => {
    const move = vi.fn();
    renderMenu({}, { onMoveToWorkspace: move });
    fireEvent.click(screen.getByRole('menuitem', { name: 'Move to workspace…' }));
    expect(move).toHaveBeenCalledOnce();
  });
  it('hides moving for managed task owners', () => {
    renderMenu({ role: 'task_owner' }, { managedOwner: true, onMoveToWorkspace: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: 'Move to workspace…' })).toBeNull();
  });
});

describe('PaneContextMenu items moved out of the header', () => {
  it('copies the session id of a pane that can be resumed', () => {
    const copy = vi.fn();
    renderMenu({ claudeSessionId: 'session-1', resumeReady: true }, { onCopySessionId: copy });
    const item = screen.getByRole('menuitem', { name: 'Copy session ID' });
    expect(item).toHaveAttribute('title', expect.stringContaining('session-1'));
    fireEvent.click(item);
    expect(copy).toHaveBeenCalledOnce();
  });
  it('calls a Codex conversation a thread', () => {
    renderMenu({ cmd: ['codex'], agent: 'codex', codexThreadId: 'thread-1', resumeReady: true }, { isClaudePane: false, onCopySessionId: vi.fn() });
    expect(screen.getByRole('menuitem', { name: 'Copy thread ID' })).toHaveAttribute('title', expect.stringContaining('thread-1'));
  });
  it('offers no copy until the conversation can be resumed', () => {
    renderMenu({ claudeSessionId: 'session-1' }, { onCopySessionId: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: /Copy (session|thread) ID/ })).toBeNull();
  });
  it('maximises, and restores while maximised', () => {
    const zoom = vi.fn();
    const { unmount } = renderMenu({}, { onToggleZoom: zoom });
    fireEvent.click(screen.getByRole('menuitem', { name: /^Maximise pane/ }));
    expect(zoom).toHaveBeenCalledOnce();
    unmount();
    renderMenu({}, { onToggleZoom: zoom, isZoomed: true });
    expect(screen.getByRole('menuitem', { name: /^Restore layout/ })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /^Maximise pane/ })).toBeNull();
  });
  it('offers no Maximise where there is nothing to maximise over', () => {
    renderMenu({});
    expect(screen.queryByRole('menuitem', { name: /Maximise pane|Restore layout/ })).toBeNull();
  });
  it('keeps Maximise for a managed task owner', () => {
    renderMenu({ role: 'task_owner' }, { managedOwner: true, onToggleZoom: vi.fn() });
    expect(screen.getByRole('menuitem', { name: /^Maximise pane/ })).toBeInTheDocument();
  });
});

describe('PaneContextMenu delegation announcements', () => {
  it('reflects the daemon-computed effective value', () => {
    renderMenu({ announceDelegation: true }, { onToggleAnnounceDelegation: () => {} });
    const item = screen.getByRole('menuitemcheckbox', { name: new RegExp(ITEM) });
    expect(item).toHaveAttribute('aria-checked', 'true');
    expect(item).toHaveClass('is-checked');
  });

  it('renders unchecked for a pane that is not announcing', () => {
    renderMenu({ announceDelegation: false }, { onToggleAnnounceDelegation: () => {} });
    const item = screen.getByRole('menuitemcheckbox', { name: new RegExp(ITEM) });
    expect(item).toHaveAttribute('aria-checked', 'false');
    expect(item).not.toHaveClass('is-checked');
  });

  it('invokes the toggle when pressed', () => {
    const toggle = vi.fn();
    renderMenu({ announceDelegation: true }, { onToggleAnnounceDelegation: toggle });
    fireEvent.click(screen.getByRole('menuitemcheckbox', { name: new RegExp(ITEM) }));
    expect(toggle).toHaveBeenCalledTimes(1);
  });

  it('can be wired for a Codex pane', () => {
    renderMenu({ agent: 'codex', cmd: ['codex'], announceDelegation: false }, { onToggleAnnounceDelegation: () => {}, isClaudePane: false });
    expect(screen.getByRole('menuitemcheckbox', { name: new RegExp(ITEM) })).toBeInTheDocument();
  });

  it('is hidden when the host does not wire the action', () => {
    renderMenu({ announceDelegation: true });
    expect(screen.queryByRole('menuitemcheckbox', { name: new RegExp(ITEM) })).toBeNull();
  });
});

describe('PaneContextMenu scheduling', () => {
  it.each([true, false])('offers New automation for native=%s panes', (isNative) => {
    const onCreateSchedule = vi.fn();
    renderMenu({}, { isNative, onCreateSchedule });
    fireEvent.click(screen.getByRole('menuitem', { name: 'New automation…' }));
    expect(onCreateSchedule).toHaveBeenCalledTimes(1);
  });
});

describe('PaneContextMenu smart approvals', () => {
  it.each([false, true])('shows the terminal-only hint for native=%s', (isNative) => {
    renderMenu({}, { isNative });
    expect(!!screen.queryByRole('note')).toBe(!isNative);
    expect(screen.getByRole('menuitemradio', { name: 'Approve with caution' })).toBeInTheDocument();
  });
});

describe('PaneContextMenu role lifecycle', () => {
  it('offers conversion for an eligible plain running pane', () => {
    renderMenu({}, { onConvertRole: () => {} });
    expect(screen.getByRole('menuitem', { name: 'Convert to orchestrator…' })).toBeInTheDocument();
  });

  it('offers conversion for a running shell pane', () => {
    renderMenu({ cmd: ['bash'], status: 'running' }, { onConvertRole: () => {}, isClaudePane: false });
    expect(screen.getByRole('menuitem', { name: 'Convert to orchestrator…' })).toBeInTheDocument();
  });

  it('offers conversion when the container marks a running Codex pane eligible', () => {
    renderMenu({ agent: 'codex', cmd: ['codex'] }, { onConvertRole: () => {} });
    expect(screen.getByRole('menuitem', { name: 'Convert to orchestrator…' })).toBeInTheDocument();
  });

  it('offers removal for an orchestrator pane', () => {
    renderMenu({ role: 'orchestrator' }, { onConvertRole: () => {} });
    expect(screen.getByRole('menuitem', { name: 'Remove orchestrator role…' })).toBeInTheDocument();
  });

  it('hides the role action when the host marks the pane ineligible', () => {
    renderMenu({});
    expect(screen.queryByRole('menuitem', { name: /orchestrator role|convert to orchestrator/i })).toBeNull();
  });

  it('hides the role action for an exited pane', () => {
    renderMenu({ status: 'exited' }, { onConvertRole: () => {} });
    expect(screen.queryByRole('menuitem', { name: /orchestrator role|convert to orchestrator/i })).toBeNull();
  });

  it('invokes the role handler', () => {
    const convert = vi.fn();
    renderMenu({}, { onConvertRole: convert });
    fireEvent.click(screen.getByRole('menuitem', { name: 'Convert to orchestrator…' }));
    expect(convert).toHaveBeenCalledTimes(1);
  });
});

describe('PaneContextMenu Resume session', () => {
  it("resumes an exited resumable pane (the route for a pane paused by Don't resume)", () => {
    const resume = vi.fn();
    renderMenu({ status: 'exited', resumable: true, restartPaused: true }, { onResumePane: resume });
    fireEvent.click(screen.getByRole('menuitem', { name: 'Resume session' }));
    expect(resume).toHaveBeenCalledOnce();
  });
  it('is hidden on a running pane and on an exited pane that cannot resume', () => {
    const { unmount } = renderMenu({ status: 'running' }, { onResumePane: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: 'Resume session' })).toBeNull();
    unmount();
    renderMenu({ status: 'exited', resumable: false }, { onResumePane: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: 'Resume session' })).toBeNull();
  });
});

describe('PaneContextMenu automatic-answer lock', () => {
  it('disables every mode choice when the pane is locked', () => {
    const change = vi.fn();
    renderMenu({ neverAutoAnswer: true }, { onSetAutoMode: change });
    expect(screen.getByText('Auto-approve — locked')).toBeInTheDocument();
    for (const option of screen.getAllByRole('menuitemradio')) {
      expect(option).toBeDisabled();
      fireEvent.click(option);
    }
    expect(change).not.toHaveBeenCalled();
  });
});

describe('lead and worker items', () => {
  const names = () => screen.queryAllByRole('menuitem').map(b => b.textContent).filter(t => /lead|worker/i.test(t ?? ''));
  it('offers Make lead and Remove lead on your own pane, never Make worker', () => {
    const onSetTeamKind = vi.fn();
    const { unmount } = renderMenu({ origin: { source: 'user', at: 1 } }, { onSetTeamKind });
    expect(names()).toEqual(['Make lead…']);
    fireEvent.click(screen.getByText('Make lead…'));
    expect(onSetTeamKind).toHaveBeenCalledWith('lead');
    unmount();
    renderMenu({ origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'cmo' } }, { onSetTeamKind });
    expect(names()).toEqual(['Rename lead role…', 'Remove lead']);
    fireEvent.click(screen.getByText('Remove lead'));
    expect(onSetTeamKind).toHaveBeenLastCalledWith('none');
  });
  it('offers both on an agent pane from before kinds, and the other one after', () => {
    const agent = { source: 'agent' as const, byPaneId: 'cmo', leadPaneId: 'cmo', at: 1 };
    const { unmount } = renderMenu({ origin: agent }, { onSetTeamKind: vi.fn() });
    expect(names()).toEqual(['Make lead…', 'Make worker']);
    unmount();
    const second = renderMenu({ origin: { ...agent, teamKind: 'worker' } }, { onSetTeamKind: vi.fn() });
    expect(names()).toEqual(['Make lead…']);
    second.unmount();
    const third = renderMenu({ origin: { ...agent, teamKind: 'lead', leadRole: 'social' } }, { onSetTeamKind: vi.fn() });
    expect(names()).toEqual(['Rename lead role…', 'Make worker']);
    third.unmount();
    // An #850 auto worker is already a worker to the daemon.
    renderMenu({ origin: { ...agent, worker: { state: 'running' } } }, { onSetTeamKind: vi.fn() });
    expect(names()).toEqual(['Make lead…']);
  });
  it('hides them on an exited pane or without the action', () => {
    const { unmount } = renderMenu({ status: 'exited', origin: { source: 'user', at: 1 } }, { onSetTeamKind: vi.fn() });
    expect(names()).toEqual([]);
    unmount();
    renderMenu({ origin: { source: 'user', at: 1 } });
    expect(names()).toEqual([]);
  });
});

describe('PaneContextMenu Fix display', () => {
  it('offers one Fix display item in place of Realign display and Force redraw', () => {
    const fix = vi.fn();
    renderMenu({}, { onFixDisplay: fix });
    expect(screen.queryByRole('menuitem', { name: 'Realign display' })).toBeNull();
    expect(screen.queryByRole('menuitem', { name: 'Force redraw' })).toBeNull();
    fireEvent.click(screen.getByRole('menuitem', { name: 'Fix display' }));
    expect(fix).toHaveBeenCalledTimes(1);
  });
  it('hides it on native, exited or unwired panes', () => {
    const native = renderMenu({}, { isNative: true, onFixDisplay: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: 'Fix display' })).toBeNull();
    native.unmount();
    const exited = renderMenu({ status: 'exited' }, { onFixDisplay: vi.fn() });
    expect(screen.queryByRole('menuitem', { name: 'Fix display' })).toBeNull();
    exited.unmount();
    renderMenu({});
    expect(screen.queryByRole('menuitem', { name: 'Fix display' })).toBeNull();
  });
});
