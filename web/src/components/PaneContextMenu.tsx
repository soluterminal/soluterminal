import type { RefObject } from 'react';
import type { PaneEffort, PaneInfo } from '../types';
import { AUTO_MODES, autoModeMenuLabel } from '../lib/autoMode';
import { EFFORTS, effortMenuLabel } from '../lib/effort';
import { useClampedMenuPosition } from './useClampedMenuPosition';
import { agentFromCommand, effectivePaneAgent } from '../lib/agentKind';
import { SmartApprovalsHint } from './SmartApprovalsHint';

type PaneAutoMode = NonNullable<PaneInfo['autoMode']>;

interface PaneContextMenuProps {
  pane: PaneInfo;
  managedOwner?:boolean;
  x: number;
  y: number;
  menuRef: RefObject<HTMLDivElement>;
  isClaudePane: boolean;
  // Native-renderer pane: hide terminal-only items (Force redraw — native has
  // no xterm grid to repaint). Defaults to false (terminal pane).
  isNative?: boolean;
  onSplitRight: () => void;
  onSplitRightShellHere: () => void;
  onSplitRightClaudeHere: () => void;
  onSplitDown: () => void;
  onClonePane: () => void;
  onMoveToWorkspace?: () => void;
  onNewSession: () => void;
  onCreateSchedule: () => void;
  onSetAutoMode: (mode: PaneAutoMode) => void;
  // Supported orchestrator hosts only: toggle the delegated-work status line
  // soluterminal types into this session when work it delegated moves. Absent
  // ⇒ the item is hidden.
  onToggleAnnounceDelegation?: () => void;
  // Native (Focus) panes only: change the reasoning-effort level. Changing it
  // respawns the headless agent (`claude --resume` + new --effort). Absent ⇒
  // the effort section is hidden (terminal panes use the /effort TUI command).
  onSetEffort?: (effort: PaneEffort) => void;
  // Claude/Codex panes: attach/detach a local context pack. Both operations
  // restart the agent in place.
  onChangeContextPack?: () => void;
  onDetachContextPack?: () => void;
  onEditFlagMemo: () => void;
  onUnflag: () => void;
  onFlagForAttention: () => void;
  onRename: () => void;
  // Name, @handle and working-on line in one dialog. Absent ⇒ hidden.
  onEditIdentity?: () => void;
  // Supported hosts only: set or remove the orchestrator role. Respawns the
  // pane's command; absent means the item is hidden.
  onConvertRole?: () => void;
  onRedraw?: () => void;
  // Terminal panes only: re-measure and re-assert this device's width so the
  // shared PTY resizes back to match — the fix for gibberish after an app
  // update or a takeover from another device. Absent ⇒ hidden.
  onRealign?: () => void;
  // Any running pane: restart its agent in place on the installed binary,
  // resuming the conversation (or offering a fresh restart when there is none).
  // Absent ⇒ hidden.
  onRestartPane?: () => void;
  // An exited pane whose conversation can be resumed in place. The quiet
  // route for a pane paused by "Don't resume", whose body offers nothing.
  // Absent ⇒ hidden.
  onResumePane?: () => void;
  onClosePane: () => void;
  // Terminal selection actions (shown only when text is selected): paste the
  // selection into another pane's input (no Enter), or search every pane's
  // scrollback for it.
  selectionText?: string;
  sendTargets?: { id: string; title: string; pid?: number }[];
  onSendSelection?: (targetPaneId: string) => void;
  onSearchSelection?: () => void;
  // Change review: the last agent turn's diff (when there is one) and the
  // repo's live uncommitted diff for the pane's folder.
  onLastTurnDiff?: () => void;
  onReviewChanges?: () => void;
}

export function PaneContextMenu({
  pane, managedOwner=false,
  x,
  y,
  menuRef,
  isClaudePane,
  isNative = false,
  onSplitRight,
  onSplitRightShellHere,
  onSplitRightClaudeHere,
  onSplitDown,
  onClonePane,
  onMoveToWorkspace,
  onNewSession,
  onCreateSchedule,
  onSetAutoMode,
  onToggleAnnounceDelegation,
  onSetEffort,
  onChangeContextPack,
  onDetachContextPack,
  onEditFlagMemo,
  onUnflag,
  onFlagForAttention,
  onRename,
  onEditIdentity,
  onConvertRole,
  onRedraw,
  onRealign,
  onRestartPane,
  onResumePane,
  onClosePane,
  selectionText,
  sendTargets,
  onSendSelection,
  onSearchSelection,
  onLastTurnDiff,
  onReviewChanges,
}: PaneContextMenuProps) {
  const currentAutoMode = (pane.autoMode ?? '') as PaneAutoMode;
  const currentEffort = (pane.effort ?? '') as PaneEffort;
  const paneAgent = pane.kind === 'native' ? effectivePaneAgent(pane) : agentFromCommand(pane.cmd);
  const contextPackCapable = paneAgent === 'claude' || paneAgent === 'codex';
  const packBase = pane.contextPack?.replace(/\\/g, '/').split('/').filter(Boolean).pop() ?? 'none';
  const { left, top } = useClampedMenuPosition(menuRef, { x, y });

  return (
    <div
      ref={menuRef}
      className="pane-ctx-menu"
      role="menu"
      style={{ position: 'fixed', left, top, zIndex: 'var(--z-menu)' }}
    >
      {selectionText && (
        <>
          <div className="pane-ctx-menu-label" title={selectionText}>
            Selection · <span className="pane-ctx-menu-selection">{selectionPreview(selectionText)}</span>
          </div>
          {onSearchSelection && (
            <button role="menuitem" onClick={onSearchSelection}>Search all panes for selection</button>
          )}
          {onSendSelection && sendTargets && sendTargets.length > 0 && sendTargets.map((t) => (
            <button
              key={t.id}
              role="menuitem"
              title="Pastes a single line into that pane's input without submitting it. Multi-line selections are copied to the clipboard instead."
              onClick={() => onSendSelection(t.id)}
            >
              Send selection to {t.title}
            </button>
          ))}
          <div className="pane-ctx-menu-sep" />
        </>
      )}
      {(onLastTurnDiff || onReviewChanges) && (
        <>
          {onLastTurnDiff && (
            <button role="menuitem" onClick={onLastTurnDiff}>
              Last turn's changes <kbd>Alt+D</kbd>
            </button>
          )}
          {onReviewChanges && (
            <button role="menuitem" onClick={onReviewChanges}>Review uncommitted changes</button>
          )}
          <div className="pane-ctx-menu-sep" />
        </>
      )}
      {!managedOwner && <>
      <button role="menuitem" onClick={onSplitRight}>
        Split right <kbd>Alt+→</kbd>
      </button>
      {pane.status === 'running' && (
        <button role="menuitem" onClick={onSplitRightShellHere}>
          Split right with shell here
        </button>
      )}
      {!isClaudePane && pane.status === 'running' && (
        <button role="menuitem" onClick={onSplitRightClaudeHere}>
          Split right with Claude here
        </button>
      )}
      <button role="menuitem" onClick={onSplitDown}>
        Split down <kbd>Alt+↓</kbd>
      </button>
      <button role="menuitem" onClick={onClonePane}>
        Clone pane <kbd>Alt+Enter</kbd>
      </button>
      {onMoveToWorkspace && pane.role !== 'task_owner' && pane.status === 'running' && (
        <button role="menuitem" onClick={onMoveToWorkspace}>
          Move to workspace…
        </button>
      )}
      <button role="menuitem" onClick={onNewSession}>New session…</button>
      <button role="menuitem" onClick={onCreateSchedule}>Create schedule…</button>
      </>}
      <div className="pane-ctx-menu-sep" />
      <div className="pane-ctx-menu-label">Auto-approve</div>
      {AUTO_MODES.map((mode) => {
        const checked = currentAutoMode === mode;
        return (
          <button
            key={mode || 'off'}
            role="menuitemradio"
            aria-checked={checked}
            className={checked ? 'is-checked' : ''}
            onClick={() => onSetAutoMode(mode)}
          >
            <span className="pane-ctx-menu-check" aria-hidden>{checked ? '●' : '○'}</span>
            {autoModeMenuLabel(mode)}
          </button>
        );
      })}
      {/* Jev smart approvals classify terminal permission menus. Native panes
          use their structured approval path instead; non-team native cautious
          mode intentionally leaves requests for the user. */}
      {!isNative && <SmartApprovalsHint />}
      {onToggleAnnounceDelegation && (
        <>
          <div className="pane-ctx-menu-sep" />
          <button
            role="menuitemcheckbox"
            aria-checked={pane.announceDelegation === true}
            className={pane.announceDelegation === true ? 'is-checked' : ''}
            title="Types a one-line [solu] status into this session when work it delegated needs you, fails, or lands (only while the session is idle). Also gives a fresh session (after /clear or a restart) a one-time digest of what this pane still has in flight. Default on for orchestrator panes."
            onClick={onToggleAnnounceDelegation}
          >
            <span className="pane-ctx-menu-check" aria-hidden>{pane.announceDelegation === true ? '☑' : '☐'}</span>
            Announce delegated-work updates
          </button>
        </>
      )}
      {isNative && onSetEffort && pane.status === 'running' && (
        <>
          <div className="pane-ctx-menu-sep" />
          <div className="pane-ctx-menu-label">Reasoning effort (restarts agent)</div>
          {EFFORTS.map((level) => {
            const checked = currentEffort === level;
            return (
              <button
                key={level || 'default'}
                role="menuitemradio"
                aria-checked={checked}
                className={checked ? 'is-checked' : ''}
                onClick={() => onSetEffort(level)}
              >
                <span className="pane-ctx-menu-check" aria-hidden>{checked ? '●' : '○'}</span>
                {effortMenuLabel(level)}
              </button>
            );
          })}
        </>
      )}
      {contextPackCapable && onChangeContextPack && pane.status === 'running' && (
        <>
          <div className="pane-ctx-menu-sep" />
          <div className="pane-ctx-menu-label">Context pack (restarts agent)</div>
          <div className="pane-ctx-menu-label" title={pane.contextPack}>{packBase} · {pane.contextPackStatus ?? (pane.contextPack ? 'injected' : 'none')}</div>
          <button role="menuitem" onClick={onChangeContextPack}>Change…</button>
          {pane.contextPack && onDetachContextPack && (
            <button role="menuitem" onClick={onDetachContextPack}>Detach</button>
          )}
        </>
      )}
      <div className="pane-ctx-menu-sep" />
      {pane.flagged ? (
        <>
          <button role="menuitem" onClick={onEditFlagMemo}>Edit flag memo…</button>
          <button role="menuitem" onClick={onUnflag}>Unflag</button>
        </>
      ) : (
        <button role="menuitem" onClick={onFlagForAttention}>Flag for attention…</button>
      )}
      <div className="pane-ctx-menu-sep" />
      <button role="menuitem" onClick={onRename}>Rename</button>
      {onEditIdentity && (
        <button role="menuitem" onClick={onEditIdentity} title="A handle to address this pane by, and what it is working on">
          Handle and working on…
        </button>
      )}
      {!managedOwner && onConvertRole && pane.status === 'running' && pane.role !== 'task_owner' && (
        <button
          role="menuitem"
          onClick={onConvertRole}
          title="Restarts the pane so the orchestrator brief is injected/removed (Claude and Codex conversations are kept)"
        >
          {pane.role === 'orchestrator' ? 'Remove orchestrator role…' : 'Convert to orchestrator…'}
        </button>
      )}
      {!isNative && onRealign && pane.status === 'running' && (
        <button
          role="menuitem"
          onClick={onRealign}
          title="Fix garbled / misaligned text after an app update or a takeover from another device"
        >
          Realign display
        </button>
      )}
      {!isNative && onRedraw && pane.status === 'running' && (
        <button role="menuitem" onClick={onRedraw}>
          Force redraw
        </button>
      )}
      {onResumePane && pane.status === 'exited' && pane.resumable && (
        <button
          role="menuitem"
          onClick={onResumePane}
          title="Start this pane's conversation again where it left off."
        >
          Resume session
        </button>
      )}
      {onRestartPane && pane.status === 'running' && (
        <button
          role="menuitem"
          onClick={onRestartPane}
          title="Restart this pane's agent in place on the installed build, keeping the conversation. If it can't be resumed you'll be offered a fresh restart."
        >
          Restart pane
        </button>
      )}
      <button role="menuitem" className="danger" onClick={onClosePane}>
        Close <kbd>Alt+W</kbd>
      </button>
    </div>
  );
}

// One-line preview of a selection for the menu label.
export function selectionPreview(text: string, max = 32): string {
  const one = text.replace(/\s+/g, ' ').trim();
  return one.length > max ? one.slice(0, max - 1) + '…' : one;
}
