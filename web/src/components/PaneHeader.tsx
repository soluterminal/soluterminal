import { isTemporaryWorkspaceView } from '../lib/workspaceView';
import { useEffect, useLayoutEffect, useRef, useState, type Dispatch, type DragEventHandler, type MouseEvent as ReactMouseEvent, type SetStateAction, type ReactNode } from 'react';
import { IconMaximize, IconMinimize } from './ui/icons';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { confirmClosePane } from './usePaneContextMenu';
import { useIsMobileView } from './useIsMobileView';
import { PaneHeaderOverflow, PaneDetail, usePaneDetailVisible, DETAIL_PRIORITY } from './PaneHeaderOverflow';
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
import { useFocusEnabled } from '../lib/focusAvailability';
import { PaneTurnChip } from './PaneTurnChip';
import { turnChipModel } from '../lib/turnChip';
import {
  shouldConfirmModeSwitch,
  switchRequestForChoice,
  pendingSwitchKind,
  toggleVisual,
  focusHintDismissed,
  markFocusUsed,
} from '../native/modeSwitch';
import { ModeSwitchConfirm, PaneModeToggle } from './native/NativeEventViews';
import { Tooltip } from './Tooltip';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { paneAgentRssMB } from '../lib/agentMemory';

// Shared explainer copy for the renderer toggle. Surfaced as the group
// tooltip in the header AND mirrored in the command-palette entry so the
// "what is native mode" answer is the same wherever the user first meets it.
const MODE_TOGGLE_TITLE =
  'Renderer · Focus = structured chat that reflows at any width · Terminal = raw TUI (xterm)';

// One-time "Try Focus" discovery hint. Shown ONCE (per install) under the
// mode toggle of the first Terminal-mode Claude pane the user sees; dismissed
// forever by the ✕, by touching the toggle, or by EVER having used Focus —
// NativePane calls markFocusUsed() on mount (see modeSwitch.ts), so existing
// Focus users (and a pane just switched AWAY from Focus) never see it.
// Module-level claim so multiple visible Claude panes don't each pop the hint.
let focusHintClaimedBy: string | null = null;

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
  // Flip this pane between the xterm terminal renderer and the native
  // conversation renderer. Only rendered (as a Terminal/Native segmented
  // toggle) when the pane is Claude-capable. Absent ⇒ no toggle.
  onSwitchMode?: (kind: '' | 'native') => void;
  // Open the pane context menu — the ⋯ button and right-click share this. Owned
  // by the parent (Pane.tsx), which holds the menu state. Absent ⇒ no ⋯ button.
  onOpenMenu?: (e: ReactMouseEvent) => void;
}

// A direct/foreground Codex pane can enter Focus before its first message:
// the daemon distinguishes a verified virgin process (thread/start) from an
// unreadable or ambiguous identity (identityBlocked). A known thread remains
// the continuity signal for shell-launched panes between process scans.
function isFocusCapable(pane: PaneInfo) {
  const agent = effectivePaneAgent(pane);
  if (agent === 'codex') return true;
  // Muse: a terminal muse session's identity isn't discoverable, so a Focus
  // flip would silently start a fresh conversation — the daemon refuses it
  // (native_switch.go). Only offer the toggle when the id is known.
  if (agent === 'muse') return pane.kind === 'native' || !!pane.museSessionId;
  return pane.hasClaude === true || (agent === 'claude' && (
    pane.cmd[0] === 'claude' ||
    pane.cmd[0]?.endsWith('/claude') === true ||
    pane.kind === 'native'
  ));
}

// Auto-approval is broader than the renderer toggle: the daemon detects and
// auto-answers Codex's approval modals too (daemon/state.go codexApprovalPromptRE
// + aria_auto.go passes hasCodex through), so the badge shows for Codex panes as
// well — not just Claude. A managed Pi pane loads the SoluTerminal approval
// extension, which renders the same numbered menu shape, so it is capable too
// (hasPi is set only for managed launches). The DeepSeek terminal client renders
// the same shape (hasDeepseek is detected from its client argv), so it is
// capable too. Plain shells still have no approval prompts to answer.
function isAutoApproveCapable(pane: PaneInfo) {
  return isFocusCapable(pane) || pane.hasCodex === true || pane.hasMuse === true || pane.hasPi === true || pane.hasDeepseek === true;
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

// memLabel formats sampled agent RSS (MB) for the memory chip: GB above 1024.
function memLabel(mb: number) {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
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
  onSwitchMode,
  onOpenMenu,
}: PaneHeaderProps) {
  const paneMovesAllowed = useStore(s => !isTemporaryWorkspaceView(s.workspaceView));
  const isMobileView = useIsMobileView();
  const warningReview = !isMobileView && (pane.review?.verdict === 'request-changes' || pane.review?.verdict === 'comment');
  const headerRef = useRef<HTMLDivElement>(null);
  const combineMessage = useStore(s => !isMobileView && messageSharesOrigin(pane, s.panes));
  const paneAgentLabel = agentLabel(effectivePaneAgent(pane));
  const agentMemMB = paneAgentRssMB(pane);

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
        <Tooltip label="Double-click to rename">
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
          The steady-state "running" was permanent header noise (and Focus
          panes never had it), so the healthy state now shows nothing. */}
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
        <PaneDetail priority={DETAIL_PRIORITY.mode}><PaneModeControl pane={pane} onSwitchMode={onSwitchMode} /></PaneDetail>
        {/* Agent memory readout (display-only on Term): the selected provider's
            resident RAM, sampled ~60s. No tap-to-restart here — Term reclaims
            via Ctrl+C + resume or reopen
            (one-tap restart lives in Focus mode). Amber when heavy. */}
        {agentMemMB > 0 && (
          <PaneDetail priority={DETAIL_PRIORITY.memory}><Tooltip label={`Agent memory ${memLabel(agentMemMB)}. To reclaim: Ctrl+C to exit the agent then resume, or use Restart pane in the ⋯ menu.`}>
            <span
              className={`iconbtn pane-mem-chip readonly${agentMemMB >= 500 ? ' heavy' : ''}`}
              aria-label={`Agent memory ${memLabel(agentMemMB)}`}
            >
              {memLabel(agentMemMB)}
            </span>
          </Tooltip></PaneDetail>
        )}
        {canZoom && (
          <PaneDetail priority={DETAIL_PRIORITY.maximise}><Tooltip label={isZoomed ? 'Restore layout (Alt+Z)' : 'Maximise pane (Alt+Z)'}>
            <button
              className="iconbtn pane-zoom-btn"
              aria-label={isZoomed ? 'Restore pane' : 'Maximise pane'}
              onClick={(e) => {
                e.stopPropagation();
                onToggleZoom();
              }}
              onDoubleClick={(e) => e.stopPropagation()}
            >{isZoomed ? <IconMinimize size={13} /> : <IconMaximize size={13} />}<span className="pane-zoom-label">{isZoomed ? 'Restore layout' : 'Maximise pane'}</span></button>
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

// Each inline/popover copy owns its confirmation. Hiding an inline detail or
// unmounting its popover cancels that copy instead of retaining an armed dialog.
function PaneModeControl({ pane, onSwitchMode }: Pick<PaneHeaderProps, 'pane' | 'onSwitchMode'>) {
  const detailVisible = usePaneDetailVisible();
  const paneAgentLabel = agentLabel(effectivePaneAgent(pane));
  const isNative = pane.kind === 'native';
  const modeSwitchReady = isFocusCapable(pane) && !pane.identityBlocked;
  const codexIdentityPending = !isNative && (pane.hasCodex === true || pane.hasMuse === true) && !modeSwitchReady;
  // Focus retired: the toggle only remains on a pane that is STILL native, as
  // its way back to Terminal.
  const focusEnabled = useFocusEnabled();
  const showModeToggle = !!onSwitchMode && (focusEnabled ? (modeSwitchReady || codexIdentityPending) : isNative);
  const modeDisabledReason = codexIdentityPending
    ? 'Focus is temporarily locked while conversation ownership is ambiguous. Leave one agent in the foreground and try again.'
    : undefined;
  // Per-pane "switch in flight" flag (set by store.switchPaneMode for the
  // duration of the daemon kill+respawn grace). Subscribed here — not passed
  // as a prop — so the header can show a disabled/spinner state without
  // touching the native-pane component files. Selector is a primitive, so it
  // only re-renders this header when THIS pane's flag flips.
  const switching = useStore((s) => !!s.paneModeSwitching[pane.id]);
  // The store's switchPaneMode, for the deferred/cancel/override paths that the
  // (kind-only) onSwitchMode prop can't express. The immediate idle switch still
  // routes through onSwitchMode for back-compat.
  const switchPaneMode = useStore((s) => s.switchPaneMode);
  // A renderer switch awaiting confirmation because the pane is WORKING (a turn
  // is in flight / an approval is pending). Holds the target kind. null = no
  // confirm open ⇒ idle switches stay one-click. The three-way ModeSwitchConfirm
  // popover renders below the toggle while this is set. Named confirmTarget (not
  // pendingSwitch) so it doesn't collide with the DTO's pane.pendingSwitch.
  const [confirmTarget, setConfirmTarget] = useState<'' | 'native' | null>(null);

  // One-time Focus discovery hint (see the note above focusHintClaimedBy).
  const [showFocusHint, setShowFocusHint] = useState(false);
  useEffect(() => {
    if (!focusEnabled || !showModeToggle || !modeSwitchReady || isNative) return;
    if (focusHintDismissed()) return;
    if (focusHintClaimedBy && focusHintClaimedBy !== pane.id) return;
    focusHintClaimedBy = pane.id;
    setShowFocusHint(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusEnabled, isNative, modeSwitchReady, showModeToggle, pane.id]);
  const closeFocusHint = () => {
    markFocusUsed();
    setShowFocusHint(false);
  };

  // The armed-but-waiting deferred switch (server state) and the precedence-
  // resolved toggle visual (flipping > pending > idle).
  const pendingSwitch = pane.pendingSwitch ?? null;
  const visual = toggleVisual(switching, pendingSwitch);
  const stillWorking = shouldConfirmModeSwitch(pane.state, pane.agentMidTurn);

  // Click handler for a toggle segment: switch immediately when idle, else open
  // the three-way confirm. `to` is the renderer being switched TO.
  const requestSwitch = (to: '' | 'native') => {
    if (switching || !modeSwitchReady) return;
    if (stillWorking) {
      setConfirmTarget(to);
      return;
    }
    onSwitchMode?.(to);
  };

  // Cancel an armed pending switch (plain-click while pending, or its ✕).
  const cancelPending = () => {
    const kind = pendingSwitchKind(pendingSwitch);
    if (kind == null) return;
    void switchPaneMode(pane.id, kind, { cancel: true });
  };
  // Override an armed pending switch → switch now (Shift-click the target).
  const switchNow = () => {
    const kind = pendingSwitchKind(pendingSwitch);
    if (kind == null) return;
    void switchPaneMode(pane.id, kind, { when: 'now' });
  };

  useLayoutEffect(() => {
    if (!detailVisible) setConfirmTarget(null);
  }, [detailVisible]);

  if (!showModeToggle) return null;
  return (
    <div className="pane-mode-toggle-wrap" onClickCapture={() => { if (showFocusHint) closeFocusHint(); }}>
      {showFocusHint && confirmTarget === null && visual === 'idle' && (
        <div className="pane-focus-hint" role="note">
          <span className="pane-focus-hint-text">
            Try <strong>Focus</strong> — this {paneAgentLabel} session as a structured chat
            (rich diffs, one-tap approvals, reflows on any screen). Switch back anytime.
          </span>
          <button
            type="button"
            className="pane-focus-hint-x"
            aria-label="Dismiss tip"
            title="Dismiss"
            onClick={(e) => {
              e.stopPropagation();
              closeFocusHint();
            }}
          >
            ✕
          </button>
        </div>
      )}
      <PaneModeToggle
        isNative={isNative}
        visual={visual}
        pending={pendingSwitch}
        onSeg={requestSwitch}
        onCancelPending={cancelPending}
        onSwitchNow={switchNow}
        termTitle="Terminal renderer — raw TUI (xterm). Shared PTY width."
        nativeTitle="Focus renderer — structured chat that reflows at any width."
        idleTitle={MODE_TOGGLE_TITLE}
        agentLabel={paneAgentLabel}
        disabledReason={modeDisabledReason}
      />
      {detailVisible && confirmTarget !== null && (
        <ModeSwitchConfirm
          target={confirmTarget}
          working={stillWorking}
          agentLabel={paneAgentLabel}
          onConfirm={(choice) => {
            const to = confirmTarget;
            setConfirmTarget(null);
            if (!stillWorking) {
              // Settled mid-confirm → plain immediate switch (back-compat).
              onSwitchMode?.(to);
              return;
            }
            void switchPaneMode(pane.id, to, switchRequestForChoice(choice, to));
          }}
          onCancel={() => setConfirmTarget(null)}
        />
      )}
    </div>
  );
}
