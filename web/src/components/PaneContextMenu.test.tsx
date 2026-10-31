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
  it.each([true, false])('offers Create schedule for native=%s panes', (isNative) => {
    const onCreateSchedule = vi.fn();
    renderMenu({}, { isNative, onCreateSchedule });
    fireEvent.click(screen.getByRole('menuitem', { name: 'Create schedule…' }));
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
