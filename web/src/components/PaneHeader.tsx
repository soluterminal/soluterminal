import { isTemporaryWorkspaceView } from '../lib/workspaceView';
import { useRef, type Dispatch, type DragEventHandler, type MouseEvent as ReactMouseEvent, type SetStateAction, type ReactNode } from 'react';
import { IconMinimize } from './ui/icons';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { confirmClosePane } from './usePaneContextMenu';
import { useIsMobileView } from './useIsMobileView';
import { PaneHeaderOverflow, PaneDetail, DETAIL_PRIORITY } from './PaneHeaderOverflow';
import { PaneHeaderBadges } from './PaneHeaderBadges';
import { UnsentMessageChip } from './UnsentMessageChip';
import { PaneAsksChip } from './PaneAsksChip';
import { PaneNeedsYouChip } from './PaneNeedsYouChip';
import { WorkerNoticesChip } from './WorkerNoticesChip';
import { PaneOriginButton } from './PaneOriginButton';
import { messageSharesOrigin } from '../lib/paneHeaderDetails';
import { PaneIdentityChip } from './PaneIdentityChip';
import { PaneReviewChip } from './PaneReviewChip';
import { PaneMessageChip } from './PaneMessageChip';
import { PaneTurnChip } from './PaneTurnChip';
import { turnChipModel } from '../lib/turnChip';
import { Tooltip } from './Tooltip';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { useAgentMemoryPill } from '../lib/useAgentMemoryPill';
import { AgentMemoryPill } from './AgentMemoryChip';
import { paneTitleHint } from '../lib/altHeld';

type ConnStatus = 'connecting' | 'open' | 'reconnecting' | 'closed';

interface PaneHeaderProps {
  pane: PaneInfo;
  ownerPresentation?: boolean;
  actionGuard?:()=>boolean;
  paneIndex: number;
  activityClass: string;
  editingTitle: boolean;
  titleDraft: string;
  unread: boolean;
  // Which kind of unread (lib/turnChip.ts paneUnread): 'turn' highlights the
  // turn chip, 'output' shows the dot.
  unreadKind?: 'turn' | 'output' | null;
  connStatus: ConnStatus;
  isZoomed: boolean;
  canZoom: boolean;
  setTitleDraft: Dispatch<SetStateAction<string>>;
  setEditingTitle: Dispatch<SetStateAction<boolean>>;
  onDragStart: DragEventHandler<HTMLDivElement>;
  onDragEnd: () => void;
  onRename: (title: string) => Promise<unknown>;
  onClose: () => void;
  onToggleZoom: () => void;
  // Open the pane context menu — the ⋯ button and right-click share this. Owned
  // by the parent (Pane.tsx), which holds the menu state. Absent ⇒ no ⋯ button.
  onOpenMenu?: (e: ReactMouseEvent) => void;
  // Restart the agent in place; offered by the memory pill when memory is unusual.
  onRestartPane?: () => void;
}

// A Claude, Codex or Muse agent pane (by its agent label or detected CLI).
function isAgentPane(pane: PaneInfo) {
  const agent = effectivePaneAgent(pane);
  if (agent === 'codex') return true;
  if (agent === 'muse') return !!pane.museSessionId;
  return pane.hasClaude === true || (agent === 'claude' && (
    pane.cmd[0] === 'claude' ||
    pane.cmd[0]?.endsWith('/claude') === true
  ));
}

// Auto-approval is broader than Claude panes: the daemon detects and
// auto-answers Codex's approval modals too (daemon/state.go codexApprovalPromptRE
// + aria_auto.go passes hasCodex through), so the badge shows for Codex panes as
// well — not just Claude. A managed Pi pane loads the SoluTerminal approval
// extension, which renders the same numbered menu shape, so it is capable too
// (hasPi is set only for managed launches). The DeepSeek terminal client renders
// the same shape (hasDeepseek is detected from its client argv), so it is
// capable too. Plain shells still have no approval prompts to answer.
function isAutoApproveCapable(pane: PaneInfo) {
  return isAgentPane(pane) || pane.hasCodex === true || pane.hasMuse === true || pane.hasPi === true || pane.hasDeepseek === true;
}

function activityTooltip(c: string, label: string) {
  switch (c) {
    case 'busy': return 'streaming output';
    case 'awaiting-input': return `${label} is waiting for your input`;
    case 'choice-prompt': return `${label} needs your approval`;
    case 'recent': return 'recent activity';
    case 'idle': return 'idle';
    case 'exited': return 'session exited';
    default: return '';
  }
}

function paneStatusLabel(pane: PaneInfo, connStatus: ConnStatus) {
  if (pane.status === 'exited') {
    return `exited${pane.exitCode != null ? ` (${pane.exitCode})` : ''}`;
  }
  if (connStatus === 'reconnecting') return 'reconnecting…';
  if (connStatus === 'connecting') return 'connecting…';
  return 'running';
}

function paneStatusClass(pane: PaneInfo, connStatus: ConnStatus) {
  if (pane.status === 'exited') return 'exited';
  if (connStatus === 'open') return 'running';
  return connStatus;
}

export function PaneHeader({
  pane, ownerPresentation = false, actionGuard = ()=>true,
  paneIndex,
  activityClass,
  editingTitle,
  titleDraft,
  unread,
  unreadKind,
  connStatus,
  isZoomed,
  canZoom,
  setTitleDraft,
  setEditingTitle,
  onDragStart,
  onDragEnd,
  onRename,
  onClose,
  onToggleZoom,
  onOpenMenu,
  onRestartPane,
}: PaneHeaderProps) {
  const paneMovesAllowed = useStore(s => !isTemporaryWorkspaceView(s.workspaceView));
  const isMobileView = useIsMobileView();
  const warningReview = !isMobileView && (pane.review?.verdict === 'request-changes' || pane.review?.verdict === 'comment');
  const headerRef = useRef<HTMLDivElement>(null);
  const combineMessage = useStore(s => !isMobileView && messageSharesOrigin(pane, s.panes));
  const paneAgentLabel = agentLabel(effectivePaneAgent(pane));
  const memory = useAgentMemoryPill(pane, isMobileView);

  return (
    <div
      ref={headerRef}
      className={`pane-header${isMobileView ? '' : ' pane-header-managed'}`}
      draggable={!ownerPresentation && paneMovesAllowed}
      onDragStart={e=>{if(actionGuard()&&!ownerPresentation&&paneMovesAllowed)onDragStart(e);else e.preventDefault();}}
      onDragEnd={onDragEnd}
      // Double-click empty header area toggles maximise — matches OS window
      // affordance. Title span, edit input, and action buttons stop
      // propagation so they don't trigger this.
      onDoubleClick={(e) => {
        if (!canZoom) return;
        e.stopPropagation();
        onToggleZoom();
      }}
    >
      <PaneHeaderOverflow headerRef={headerRef} enabled={!isMobileView}>
      <Tooltip label={activityTooltip(activityClass, paneAgentLabel)}>
        <span className={`activity-dot ${activityClass}`} />
      </Tooltip>
      <PaneHeaderBadges pane={pane} paneIndex={paneIndex} section={isMobileView ? undefined : "index"} showAutoMode={isAutoApproveCapable(pane)} />
      {!isMobileView && <PaneHeaderBadges pane={pane} paneIndex={paneIndex} section="details" showAutoMode={isAutoApproveCapable(pane)} />}
      {editingTitle ? (
        <input
          className="pane-title-input"
          autoFocus
          value={titleDraft}
          onChange={(e) => setTitleDraft(e.target.value)}
          onBlur={async () => {
            if(!actionGuard()) {setEditingTitle(false);return;}
            setEditingTitle(false);
            const next = titleDraft.trim();
            if (next !== pane.title) {
              try { await onRename(next); } catch {}
            }
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
            if (e.key === 'Escape') {
              setTitleDraft(pane.title);
              setEditingTitle(false);
            }
            e.stopPropagation();
          }}
          onMouseDownCapture={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
          onDoubleClick={(e) => e.stopPropagation()}
        />
      ) : (
        <Tooltip label={paneTitleHint(paneIndex)}>
          <span
            className="pane-title"
            onDoubleClick={(e) => {
              e.stopPropagation();
              setTitleDraft(pane.title);
              setEditingTitle(true);
            }}
          >
            {unread && (unreadKind !== 'turn' || !turnChipModel(pane, Date.now(), true)) && <span className="unread-marker" aria-label="new activity">●</span>}
            {pane.title || pane.cmd.join(' ')}
          </span>
        </Tooltip>
      )}
      <PaneDetail priority={DETAIL_PRIORITY.identity}><PaneIdentityChip pane={pane} deduplicateTitle={!isMobileView} /></PaneDetail>
      <PaneDetail priority={combineMessage ? DETAIL_PRIORITY.message : DETAIL_PRIORITY.origin}>{combineMessage ? <span className="pane-origin-message"><PaneOriginButton pane={pane} /><PaneMessageChip pane={pane} /></span> : <PaneOriginButton pane={pane} />}</PaneDetail>
      {!warningReview && <PaneDetail priority={DETAIL_PRIORITY.review}><PaneReviewChip pane={pane} /></PaneDetail>}
      {!combineMessage && <PaneDetail priority={DETAIL_PRIORITY.message}><PaneMessageChip pane={pane} /></PaneDetail>}
      <PaneDetail priority={DETAIL_PRIORITY.workerNotice}><WorkerNoticesChip pane={pane} /></PaneDetail>
      <HeaderAttention enabled={!isMobileView}>
      {warningReview && <PaneReviewChip pane={pane} />}
      {!isMobileView && <PaneHeaderBadges pane={pane} paneIndex={paneIndex} section="attention" showAutoMode={isAutoApproveCapable(pane)} />}
      {!isMobileView && <PaneNeedsYouChip pane={pane} />}
      {/* Status chip only when it carries SIGNAL — exited / (re)connecting.
          The steady-state "running" was permanent header noise, so the
          healthy state now shows nothing. */}
      {(pane.status === 'exited' || connStatus !== 'open') && (
        <span className={`pane-status ${paneStatusClass(pane, connStatus)}`}>
          {paneStatusLabel(pane, connStatus)}
        </span>
      )}
      <PaneTurnChip pane={pane} unseen={unreadKind === 'turn'} />
      <UnsentMessageChip pane={pane} actionGuard={actionGuard} />
      <PaneAsksChip pane={pane} />
      </HeaderAttention>
      <div className="pane-actions">
        {/* Agent memory: hidden until unusual, then "1.8 GB · Restart?"
            (AgentMemoryChip.tsx). An unusual reading ranks with auto-approve
            so it is the last detail to fold into +N. */}
        {memory.show && (
          <PaneDetail priority={memory.unusual ? DETAIL_PRIORITY.memoryUnusual : DETAIL_PRIORITY.memory}>
            <AgentMemoryPill
              memory={memory}
              onRestart={onRestartPane && pane.status === 'running' ? () => { if (actionGuard()) onRestartPane(); } : undefined}
            />
          </PaneDetail>
        )}
        {/* Maximise lives in the pane menu (and Alt+Z, and a double-click on
            the header). The header shows only the way back, while maximised. */}
        {canZoom && isZoomed && !isMobileView && (
          <PaneDetail priority={DETAIL_PRIORITY.maximise}><Tooltip label="Restore layout (Alt+Z)">
            <button
              className="iconbtn pane-zoom-btn"
              aria-label="Restore pane"
              onClick={(e) => {
                e.stopPropagation();
                onToggleZoom();
              }}
              onDoubleClick={(e) => e.stopPropagation()}
            ><IconMinimize size={13} /><span className="pane-zoom-label">Restore layout</span></button>
          </Tooltip></PaneDetail>
        )}
        {onOpenMenu && (
          <Tooltip label="Pane menu">
            <button
              className="iconbtn pane-menu-btn"
              aria-label="Pane menu"
              aria-haspopup="menu"
              onClick={(e) => {
                e.stopPropagation();
                onOpenMenu(e);
              }}
              onDoubleClick={(e) => e.stopPropagation()}
            >⋯</button>
          </Tooltip>
        )}
        <Tooltip label="Close session">
          <button
            className="iconbtn"
            aria-label={ownerPresentation ? 'Close owner view' : 'Close session'}
            onClick={async (e) => {
              e.stopPropagation();
              if(!actionGuard())return;
              if(ownerPresentation) {onClose();return;}
              if (await confirmClosePane(pane) && actionGuard()) onClose();
            }}
            onDoubleClick={(e) => e.stopPropagation()}
          >×</button>
        </Tooltip>
      </div>
      </PaneHeaderOverflow>
    </div>
  );
}

function HeaderAttention({ enabled, children }: { enabled: boolean; children: ReactNode }) {
  return enabled ? <div className="pane-header-attention">{children}</div> : <>{children}</>;
}
