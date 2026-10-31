// NativePane.tsx — the pane body for a kind==='native' pane. Mirrors
// BrowserPane.tsx's non-terminal structure (own header + .pane-body, focus on
// mousedown, drag-drop reorder) but renders a native Claude or Codex conversation
// (markdown / tool cards / structured approvals) at the browser's own width
// instead of an xterm grid. Terminal panes never reach this component — the
// dispatch is gated on pane.kind==='native' at the two pane-render funnels
// (WarmPaneLayer.WarmPanePortal + PaneGrid.renderPaneLeaf), so the xterm path
// stays byte-identical.
//
// The conversation state + SSE plumbing lives in useNativePane(paneId); this
// component is the view + scroll/auto-scroll + composer + interrupt.

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { IconMaximize, IconMinimize } from './ui/icons';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { api } from '../api';
import { firstImageFile, insertAttachToken, insertPlainPathToken, hasImagePayload } from '../native/composerAttach';
import { DRAG_MIME, computeZone, type DropZone } from '../lib/paneDnd';
import { confirmClosePane, usePaneContextMenu } from './usePaneContextMenu';
import { confirmDialog } from '../lib/dialog';
import { restartPaneFlow } from '../lib/paneRestart';
import { isRestartPaused } from '../lib/restartPaused';
import { PaneContextMenu } from './PaneContextMenu';
import { PaneFlagMemo } from './PaneFlagMemo';
import { PaneLastPrompt } from './PaneLastPrompt';
import { PaneHeaderBadges } from './PaneHeaderBadges';
import { PaneOriginButton } from './PaneOriginButton';
import { PaneIdentityChip } from './PaneIdentityChip';
import { PaneReviewChip } from './PaneReviewChip';
import { PaneMessageChip } from './PaneMessageChip';
import { ScrollToLatestButton, STICK_BOTTOM_SLOP } from './ScrollToLatestButton';
import { PaneFindBar } from './PaneFindBar';
import { useNativeFind } from './useNativeFind';
import { NativeFindContext } from '../native/nativeFindContext';
import { collectLeaves, collectPaneIds } from '../lib/layout';
import { isTypingElsewhere } from '../lib/focusGuard';
import { isMobileViewport } from '../lib/mobile';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { roleConvertAllowed } from '../extensions';
import { operatorFeaturesAllowed } from '../entitlements';
import { AgentMemoryChip } from './AgentMemoryChip';
import { ContextPackDialog } from './ContextPackDialog';
import { PaneIdentityDialog } from './PaneIdentityDialog';
import {
  useNativePane,
  getDraft,
  setDraft as persistDraft,
  subscribeDraft,
  getQueued,
  setQueued as persistQueued,
  getClearedSeqs,
  setClearedSeqs,
  getRealClear,
  persistRealClear,
} from '../native/useNativePane';
import type { NativeEvent } from '../native/nativeEvents';
import { buildTranscript, groupRows, relativeTime, computeNativeWorking, lastTurnDuration, idleBackgroundShells, idleBackgroundAgents } from '../native/nativeTranscript';
import { prettyModelName, MODEL_OPTIONS, sameModelId, modelIdForAgent } from '../native/modelInfo';
import type { ModelOption } from '../native/modelInfo';
import { contextUsageFraction } from '../native/footerVitals';
import { getVitalsSnapshot, saveVitalsSnapshot, clearVitalsContextPct } from '../native/vitalsSnapshot';
import { getKnownSlashCommands, rememberSlashCommands } from '../native/slashCommandsCache';
import { Composer } from './composer/Composer';
import {
  TextView,
  SlashResultView,
  UserMessageView,
  ThinkingView,
  ToolCallView,
  ToolGroupView,
  StepGroupView,
  EditGroupView,
  TaskView,
  ThinkingGroupView,
  TodoView,
  ApprovalView,
  DockedApprovalBar,
  AllowlistStrip,
  ErrorView,
  NativeMetaBar,
  TurnSeparator,
  SessionEndedCard,
  SlashMenu,
  AtMentionMenu,
  ModelMenu,
  type AtMentionHit,
  Cheatsheet,
  ModeSwitchConfirm,
  PaneModeToggle,
  SwitchInterruptedBanner,
  classifyApprovalRisk,
  isReadLikeApproval,
  parseAskQuestions,
  buildAnswerReason,
} from './native/NativeEventViews';
import {
  shouldConfirmModeSwitch,
  shouldShowSwitchNotice,
  switchNoticeKey,
  switchRequestForChoice,
  pendingSwitchKind,
  toggleVisual,
  markFocusUsed,
} from '../native/modeSwitch';
import {
  derivePromptHistory,
  recallOlder,
  recallNewer,
  HISTORY_IDLE,
  type HistoryNav,
  filterSlashCommands,
  stepSlashSelection,
  applyQuestionKey,
  composeSlashCommands,
  isCompactCommand,
  findAtMention,
  applyAtMention,
  KEYBOARD_SHORTCUTS,
  type SlashCommand,
} from '../native/nativeComposer';

function shortCwd(cwd: string): string {
  if (!cwd) return '';
  const parts = cwd.split('/').filter(Boolean);
  return parts.length > 2 ? '…/' + parts.slice(-2).join('/') : cwd;
}

// Per-pane scroll-position memory, keyed by pane id. Module-level so it survives
// BOTH a component remount AND the warm-pane park/unpark — switching workspace
// moves a pane's host into a display:none parking lot, which resets the scroll
// body's scrollTop to 0. Without this, coming back to a pane you'd scrolled up in
// stranded you at the very top. `atBottom` defaults to "follow latest"; we only
// persist while the body is actually visible (clientHeight > 0) so the forced-0
// on hide never overwrites the real position.
const scrollMemByPane = new Map<string, { top: number; atBottom: boolean }>();

export function NativePane({ pane }: { pane: PaneInfo }) {
  const paneAgent = effectivePaneAgent(pane);
  const paneAgentLabel = agentLabel(paneAgent);
  const setFocus = useStore((s) => s.setFocus);
  const focused = useStore((s) => s.focusedPaneId === pane.id);
  const movePane = useStore((s) => s.movePaneInLayout);
  const setDraggingPane = useStore((s) => s.setDraggingPane);
  // Per-machine opt-in for Claude's "next-prompt" suggestions (default OFF).
  // Gates both the composer ghost text AND the daemon --prompt-suggestions argv.
  const promptSuggestionsOn = useStore((s) => s.nativeSettings?.promptSuggestions === true);
  const killPane = useStore((s) => s.killPane);
  const resumePane = useStore((s) => s.resumePane);
  // "Don't resume": no Resume offer on the pane body (⋯ menu still has it).
  const restartPaused = isRestartPaused(pane);
  const refreshPane = useStore((s) => s.refreshPane);
  const setPaneEffort = useStore((s) => s.setPaneEffort);
  const setPaneModel = useStore((s) => s.setPaneModel);
  const setPaneContextPack = useStore((s) => s.setPaneContextPack);
  const switchPaneMode = useStore((s) => s.switchPaneMode);
  // Header parity with the terminal pane: rename, zoom, split-here, #index, and
  // the right-click / ⋯ context menu. All derived from the store so the two
  // NativePane mount points (PaneGrid + WarmPaneLayer, both pass only `pane`)
  // need no extra props.
  const renamePane = useStore((s) => s.renamePane);
  const toggleZoom = useStore((s) => s.toggleZoom);
  const launchShellHere = useStore((s) => s.launchShellHere);
  const launchClaudeHere = useStore((s) => s.launchClaudeHere);
  const isZoomed = useStore((s) => s.zoomedPaneId === pane.id);
  const layout = useStore((s) => s.layout);
  const paneIndex = useMemo(() => {
    const i = collectLeaves(layout).findIndex((l) => l.type === 'pane' && l.paneId === pane.id);
    return i + 1;
  }, [layout, pane.id]);
  const paneCount = useMemo(() => collectPaneIds(layout).length, [layout]);
  const canZoom = paneCount > 1 || isZoomed;
  // Whether this pane is in the ACTIVE workspace layout (vs parked/warm in a
  // hidden host). Drives whether useNativePane holds its SSE open: a parked
  // native pane keeps its DOM warm but drops the held-open HTTP/1.1 SSE so it
  // doesn't consume one of the browser's 6 connections to the local daemon
  // (see the SSE effect in useNativePane). Re-subscribes when shown again.
  const inActiveLayout = useMemo(() => collectPaneIds(layout).includes(pane.id), [layout, pane.id]);

  // Font size / Line height (Preferences) drive BOTH renderers so Terminal and
  // Focus stay consistent. The terminal applies the px / ratio directly to xterm;
  // the Focus renderer is markdown prose with its own carefully-tuned type ramp
  // (15px prose base, 1.55 leading, em-stepped headings/code/cards), so applying
  // the raw px would flatten the ramp. Instead we apply a SCALE to the whole ramp.
  //
  // Font: anchored on 14 (NOT the store default 13). The native prose base is
  // 15px, so --native-font-scale = size/14 makes Focus prose render at
  // 15*(size/14) — i.e. ~1px above the terminal px (15px vs 14px at size 14). That
  // ~1px is deliberate: the terminal's JetBrains Mono has a larger x-height than
  // the Focus sans, so equal px would read as Focus being SMALLER; +1px makes them
  // look visually ALIGNED. (Earlier we anchored on 13, which left Focus ~15%
  // bigger — Benjamin flagged it as too large.) Line-height stays anchored on its
  // own default (1.2) — it's a separate knob the user didn't want changed.
  // styles.css derives every native size/leading token from these two vars (scoped
  // to .native-pane, so app chrome is untouched).
  const terminalFontSize = useStore((s) => s.terminalFontSize);
  const terminalLineHeight = useStore((s) => s.terminalLineHeight);
  const focusFontOffset = useStore((s) => s.focusFontOffset);
  const nativeTypeScaleStyle = useMemo(
    () =>
      ({
        '--native-font-scale': terminalFontSize / 14,
        '--native-line-height-scale': terminalLineHeight / 1.2,
        // The ~1px Focus-over-Terminal gap above is now user-tunable via the
        // "Focus text offset" pref. Focus prose targets (term + offset)px; the
        // prose base is --text-lead (15px), so scaling by (term+offset)/15 lands
        // lead at exactly term+offset. CSS swaps --native-font-scale to this inside
        // the focus subtree (styles.css). Default offset 0.4 keeps Focus prose a
        // touch larger than the Terminal; 0 = Focus prose matches the Terminal px.
        '--native-focus-scale': (terminalFontSize + focusFontOffset) / 15,
      }) as React.CSSProperties,
    [terminalFontSize, terminalLineHeight, focusFontOffset],
  );

  const {
    events,
    pending,
    sessionInit,
    pendingApprovals,
    pendingApproval,
    connected,
    sendOptimistic,
    resend,
    retryPending,
    resolve,
    interrupt,
    allowlist,
    addToAllowlist,
    removeFromAllowlist,
    promptSuggestion,
    consumeSuggestion,
    hydrated,
  } = useNativePane(pane.id, pane.pid, pane.nativeEpoch, inActiveLayout);

  // Fast lookups for rendering: which permission_request rows are still open
  // (by seq), and which tool callIds have an open approval (to suppress the
  // duplicate AskUserQuestion tool_call card while its question awaits an answer).
  const pendingApprovalSeqs = useMemo(
    () => new Set(pendingApprovals.map((a) => a.seq)),
    [pendingApprovals],
  );
  const pendingApprovalCallIds = useMemo(
    () => new Set(pendingApprovals.map((a) => a.callId).filter(Boolean)),
    [pendingApprovals],
  );
  const hasApproval = pendingApprovals.length > 0;

  // (The dead-AskUserQuestion / approval + turn-lifecycle diagnostic now lives
  // DAEMON-side — grep the daemon log for "[native-turn]". Moved off the client
  // console because devtools is awkward to reach in the Tauri shell.)

  // The keyboard-active approval: the first OPEN tool approval that ISN'T an
  // AskUserQuestion (those are answered by tapping an option, not Enter/Esc).
  // Enter approves it (ONLY if non-destructive), Esc denies it (wired in the
  // keydown effect below); its card shows the risk-aware Enter/Esc hint.
  const kbApproval = useMemo(
    () => pendingApprovals.find((a) => a.toolName !== 'AskUserQuestion' && a.requestId) ?? null,
    [pendingApprovals],
  );
  // Keyboard approval is deliberately narrower than click approval: only tools
  // positively classified as read-only may be approved with Enter. Unknown
  // provider tools fail closed and require an explicit click.
  const kbReadOnly = useMemo(
    () => (kbApproval ? classifyApprovalRisk(kbApproval.toolName, kbApproval.args).readOnly : false),
    [kbApproval],
  );

  // Tool approvals only (exclude AskUserQuestion, which is answered by tapping an
  // option). Drives the docked off-screen bar + batch affordances.
  const toolApprovals = useMemo(
    () => pendingApprovals.filter((a) => a.toolName !== 'AskUserQuestion' && a.requestId),
    [pendingApprovals],
  );

  // Batch resolves: fire the existing resolve() in a loop. The daemon is
  // idempotent and resolve() optimistically dismisses each, so a partial failure
  // just leaves the unresolved ones on screen for a retry. "reads" = read-like
  // tools only (Read/Grep/Glob/LS/WebFetch) — never a Write/Bash/Edit.
  const approveAllReads = useCallback(() => {
    for (const a of toolApprovals) if (a.requestId && isReadLikeApproval(a)) void resolve(a.requestId, true);
  }, [toolApprovals, resolve]);

  // The keyboard-active AskUserQuestion: the first OPEN AskUserQuestion approval.
  // Answered by digits 1–9 (select) + Enter (confirm multi) + Esc (skip), gated
  // exactly like the tool-approval keys. Excluded from kbApproval above (those
  // are Enter/Esc approve/deny), so the two handlers never double-fire.
  const kbQuestion = useMemo(
    () => pendingApprovals.find((a) => a.toolName === 'AskUserQuestion' && a.requestId) ?? null,
    [pendingApprovals],
  );
  // Parsed shape of the active question (for the keyboard selection machine +
  // controlled card). null when there's no active AskUserQuestion.
  const kbQuestions = useMemo(
    () => (kbQuestion ? parseAskQuestions(kbQuestion.args) : null),
    [kbQuestion],
  );

  // The Focus slash menu is composed from the session's ADVERTISED command set
  // (sessionInit.slashCommands — built-in + custom + skills + MCP, the same set
  // Term mode's interactive `/` menu shows), merged with the purely-client
  // actions (/clearview view-hide, /help cheatsheet) and the confirm-guarded
  // /clear. A DORMANT pane (transcript seed only, no live session_init since the
  // last daemon restart / reload) has empty sessionInit.slashCommands; in that
  // case fall back to the GLOBAL last-known set (slashCommandsCache — skills are
  // installation-wide), and only when THAT is empty too (truly first run) to the
  // curated list. See composeSlashCommands + slashCommandsCache. NOTE: the dep is
  // `sessionSlashCommands` (a stable ref — the array on the event object, or
  // undefined), NOT a `?? []` fallback which would mint a new ref every render
  // and recompute this memo + fire the effect each frame.
  const sessionSlashCommands = sessionInit?.slashCommands;
  const composedCommands = useMemo(
    () =>
      composeSlashCommands(
        sessionSlashCommands && sessionSlashCommands.length ? sessionSlashCommands : getKnownSlashCommands(paneAgent ?? 'claude'),
        paneAgent ?? 'claude',
      ),
    [sessionSlashCommands, paneAgent],
  );
  // Remember every live command set globally so dormant panes (and post-reload
  // panes) can populate their menu from it. Order-independent idempotency guard
  // inside rememberSlashCommands keeps this from writing localStorage every turn.
  useEffect(() => {
    if (sessionSlashCommands && sessionSlashCommands.length) rememberSlashCommands(sessionSlashCommands, paneAgent ?? 'claude');
  }, [sessionSlashCommands, paneAgent]);
  // The forward/clear-real command names in the composed set — excluded from
  // prompt recall (↑/↓) since they're invocations, not prompts to re-send.
  const forwardCommandNames = useMemo(
    () =>
      new Set(
        composedCommands
          .filter((c) => c.action === 'forward' || c.action === 'clear-real')
          .map((c) => c.name),
      ),
    [composedCommands],
  );

  // Prompt history for THIS pane (oldest→newest), for ↑/↓ recall.
  const promptHistory = useMemo(
    () => derivePromptHistory(events, forwardCommandNames),
    [events, forwardCommandNames],
  );
  // Same history, newest-first, for the click-to-expand strip below the header
  // (mirrors terminal mode's lastPrompts row). We feed it from the client-derived
  // promptHistory rather than the store's daemon `lastPrompts` map so the strip,
  // the ↑/↓ composer recall, and the per-message "edit & resend" all recall from
  // ONE source — and it stays in lockstep with the transcript (both are driven by
  // the same user_text events, so a new turn shows here the instant it renders).
  const lastPromptsNewestFirst = useMemo(
    () => [...promptHistory].reverse(),
    [promptHistory],
  );

  const [dropZone, setDropZone] = useState<DropZone | null>(null);
  const [beingDragged, setBeingDragged] = useState(false);
  const [interrupting, setInterrupting] = useState(false);
  const [atBottom, setAtBottom] = useState(() => scrollMemByPane.get(pane.id)?.atBottom ?? true);
  // Count of new transcript rows that landed below the fold while the user is
  // scrolled up — surfaced as "N new" on the jump-to-latest pill (parity with
  // the terminal reader chip). Reset to 0 whenever the user is at the bottom.
  const [newCount, setNewCount] = useState(0);
  // In-pane renderer toggle: a switch awaiting CONFIRMATION because a turn is in
  // flight. Holds the target kind ('' = terminal). null = no confirm open ⇒ idle
  // switches stay one-click. (Only the Term segment is actionable in a native
  // pane — the Native segment is already active.) Named confirmTarget (not
  // pendingSwitch) so it doesn't collide with the DTO's pane.pendingSwitch.
  const [confirmTarget, setConfirmTarget] = useState<'' | 'native' | null>(null);
  // Resume-session UI state for the exited-pane card.
  const [resuming, setResuming] = useState(false);
  const [resumeFailed, setResumeFailed] = useState(false);

  // Inline title rename + flag-memo editor + the shared context menu, mirroring
  // Pane.tsx. Local state is lost on a warm-pane remount (rare mid-edit edge),
  // exactly as for terminal panes — the saved title/flag live in the store.
  const [editingTitle, setEditingTitle] = useState(false);
  const [contextPackDialogOpen, setContextPackDialogOpen] = useState(false);
  const [identityDialogOpen, setIdentityDialogOpen] = useState(false);
  const [titleDraft, setTitleDraft] = useState('');
  const [editingFlagMemo, setEditingFlagMemo] = useState(false);
  const [flagMemoDraft, setFlagMemoDraft] = useState('');
  const [lastPromptExpanded, setLastPromptExpanded] = useState(false);
  const {
    ctxMenu,
    ctxMenuRef,
    openContextMenu,
    closeContextMenu,
    actions: ctxMenuActions,
  } = usePaneContextMenu({
    pane,
    killPane,
    setFocus,
    setTitleDraft,
    setEditingTitle,
    setFlagMemoDraft,
    setEditingFlagMemo,
  });
  // Per-pane set of dismissed switchNotice keys (keyed by switchNotice.at) so the
  // "interrupted — Continue" banner doesn't reappear for the SAME interruption.
  // A FRESH interruption has a new `at`, so it shows again. Cleared on pane swap.
  const [dismissedNotices, setDismissedNotices] = useState<Set<number>>(() => new Set());

  // --- keyboard-first composer state (power-user surface) ---
  // Prompt-history recall cursor (↑/↓ over THIS pane's prior prompts).
  const [historyNav, setHistoryNav] = useState<HistoryNav>(HISTORY_IDLE);
  // Mid-turn steering: follow-up messages queued to auto-send (FIFO, one per
  // settle) when the current turn ends. Empty = nothing queued. Mirrors the
  // per-pane module map (getQueued/persistQueued) so a warm-pane remount / reload
  // / pane-swap doesn't drop a queued steer — exactly like the draft. A chip per
  // queued message shows near the composer, each with a ✕ to cancel.
  const [queued, setQueuedState] = useState<string[]>(() => getQueued(pane.id));
  const setQueued = useCallback(
    (next: string[] | ((prev: string[]) => string[])) => {
      setQueuedState((prev) => {
        const v = typeof next === 'function' ? (next as (p: string[]) => string[])(prev) : next;
        persistQueued(pane.id, v); // write through so it survives remount
        return v;
      });
    },
    [pane.id],
  );
  // Client /clear: the set of event seqs hidden from the rendered transcript at
  // the moment /clear ran. New events (new seqs) render normally; the session is
  // untouched. Survives within this mount; a warm-pane remount re-derives from a
  // module map so the local clear persists like the draft.
  const [clearedSeqs, setClearedSeqsState] = useState<Set<number>>(() => getClearedSeqs(pane.id));
  // Was the most recent clear a REAL context reset (forwarded /clear) vs a view-only
  // hide? Drives the cleared-notice copy + whether "undo" is offered (a real clear
  // can't be undone — the agent's memory is actually gone).
  const [realClear, setRealClear] = useState(() => getRealClear(pane.id));
  // A respawn (Term↔Focus switch / daemon swap, signalled by a NEW nativeEpoch)
  // re-seqs the whole conversation. The /clear view-floor is a snapshot of the
  // seqs that existed at clear time; after a respawn the daemon re-seeds the
  // transcript with those SAME deterministic seqs, so the floor would hide EVERY
  // re-seeded row → a blank pane. Drop the floor on any epoch change. A plain warm
  // remount (same epoch) keeps it, so a deliberate /clear still survives a zoom/drag.
  const lastNativeEpochRef = useRef(pane.nativeEpoch);
  useEffect(() => {
    if (lastNativeEpochRef.current === pane.nativeEpoch) return;
    lastNativeEpochRef.current = pane.nativeEpoch;
    // A respawn re-seqs the whole transcript, so any saved scroll offset is now
    // meaningless — drop it so the pane re-follows the latest.
    scrollMemByPane.delete(pane.id);
    if (getClearedSeqs(pane.id).size) {
      setClearedSeqs(pane.id, new Set());
      setClearedSeqsState(new Set());
    }
  }, [pane.nativeEpoch, pane.id]);
  // /help cheatsheet overlay.
  const [showCheatsheet, setShowCheatsheet] = useState(false);
  // Slash-command menu highlighted row (↑/↓ navigate, Enter runs).
  const [slashIndex, setSlashIndex] = useState(0);
  // /model picker — client-side model menu (headless claude has no /model; a
  // pick respawns via POST /api/panes/{id}/model). Opened by the /model slash
  // command; ↑/↓/Enter/Esc handled in the composer keydown like the other menus.
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [modelIndex, setModelIndex] = useState(0);
  // The model just chosen via /model, shown in the footer immediately (see
  // modelLabel). Reset to null once a live session_init confirms it, or on
  // failure — so it's a short-lived optimistic override, not a second source
  // of truth.
  const [pendingModel, setPendingModel] = useState<string | null>(null);
  // CONTROLLED multi-select state for the active AskUserQuestion card, so the
  // keyboard handler (digits 1–9 toggle, Enter confirms) and the on-card clicks
  // share one source of truth. Re-seeded when the active question changes.
  const [questionSel, setQuestionSel] = useState<string[][]>([]);

  // The composer's editable text. Mirrors the per-pane module draft (getDraft/
  // persistDraft/subscribeDraft) so it survives warm-pane remounts, and is the
  // controlled source for the keyboard power (slash menu, ↑/↓ history, steer/
  // queue). `sending` flags an in-flight optimistic send. (These parent-level
  // declarations are load-bearing — setDraftValue, the keyboard handlers, the
  // slash filter, and the <Composer> value all read them.)
  const [draft, setDraft] = useState<string>(() => getDraft(pane.id));
  const [sending, setSending] = useState(false);

  // Slash menu: the filtered command list for the current draft (empty unless the
  // WHOLE draft is a "/query" token). Drives the popover above the composer.
  const slashCommands = useMemo(() => filterSlashCommands(draft, composedCommands), [draft, composedCommands]);
  const slashOpen = slashCommands.length > 0;

  const bodyRef = useRef<HTMLDivElement | null>(null);
  const transcriptRef = useRef<HTMLDivElement | null>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);

  // ── @-file mention picker ────────────────────────────────────────────────
  // Typing "@" (start-of-draft or after whitespace, caret inside the token)
  // opens a fuzzy file picker over the pane's cwd. Caret position is tracked
  // explicitly (findAtMention keys on the token UNDER THE CARET, so mentions
  // completed earlier in the draft don't re-open the menu). atDismissedStart
  // remembers an Esc-closed token by its "@" index until the user moves on.
  const [caretPos, setCaretPos] = useState(0);
  const [atIndex, setAtIndex] = useState(0);
  const [atHits, setAtHits] = useState<AtMentionHit[]>([]);
  const [atDismissedStart, setAtDismissedStart] = useState<number | null>(null);
  const atSearchSeq = useRef(0);
  const atMention = useMemo(
    () => (slashOpen ? null : findAtMention(draft, caretPos)),
    [draft, caretPos, slashOpen],
  );
  const atOpen =
    !!atMention && atMention.start !== atDismissedStart && atHits.length > 0 && !showCheatsheet;

  // Track caret movement that doesn't change the draft (arrows, clicks). The
  // typing path updates via onDraftChange's rAF below.
  useEffect(() => {
    const el = taRef.current;
    if (!el) return;
    const update = () => setCaretPos(el.selectionStart ?? 0);
    el.addEventListener('keyup', update);
    el.addEventListener('click', update);
    el.addEventListener('select', update);
    return () => {
      el.removeEventListener('keyup', update);
      el.removeEventListener('click', update);
      el.removeEventListener('select', update);
    };
  }, [pane.id]);

  // Debounced cwd file search for the active token. Stale responses are
  // dropped by sequence; an empty query ("@" alone) still searches so @+↓
  // browses the tree immediately.
  useEffect(() => {
    if (!atMention) {
      setAtHits([]);
      return;
    }
    const seq = ++atSearchSeq.current;
    const q = atMention.query;
    const root = pane.liveCwd || pane.cwd;
    const t = window.setTimeout(() => {
      api
        .searchFiles(root, q, 12, 'all')
        .then((res) => {
          if (atSearchSeq.current !== seq) return;
          setAtHits(res.hits.map((h) => ({ rel: h.rel, name: h.name, isDir: h.isDir })));
          setAtIndex(0);
        })
        .catch(() => {
          if (atSearchSeq.current === seq) setAtHits([]);
        });
    }, 150);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- key on the token's identity, not the memo object
  }, [atMention?.query, atMention?.start, pane.id]);

  // IME composition guard: Tab-accept must NOT fire mid-composition (it would
  // commit an incomplete IME string). Flipped by onCompositionStart/End below.
  const composingRef = useRef(false);
  // Auto-scroll: stick to bottom unless the user scrolled up. A small slop so a
  // near-bottom position still counts as "at bottom". Seeded from the saved
  // memory so a remount (warm panes off) lands where the user left off.
  const stickRef = useRef(scrollMemByPane.get(pane.id)?.atBottom ?? true);
  // Explicit user-scroll intent, so a reflow-induced scroll (content-visibility
  // size corrections, browser scroll anchoring) can't latch the reader away from
  // the tail. Only a wheel notch, a touch drag, or a press on the scrollbar
  // counts — a click on a message does not.
  const scrollIntentUntilRef = useRef(0);
  const markScrollIntent = useCallback(() => { scrollIntentUntilRef.current = performance.now() + 400; }, []);
  const hasScrollIntent = useCallback(() => performance.now() <= scrollIntentUntilRef.current, []);
  // Last clientHeight seen by the ResizeObserver — a 0 → >0 transition means the
  // pane was just un-parked (re-shown), the cue to restore the saved position.
  const lastBodyHeightRef = useRef(0);
  // Last-seen event count, to derive how many NEW rows arrived while scrolled up.
  const prevEventsLenRef = useRef(0);
  // DOM nodes of the rendered approval cards, keyed by permission_request seq.
  // Used to (a) detect when a pending approval is scrolled out of view (→ dock a
  // bar above the composer) and (b) scroll a card into view on "jump to".
  const approvalCardRefs = useRef<Map<number, HTMLDivElement | null>>(new Map());
  // Seq of the first tool approval that is currently OFF-SCREEN (above/below the
  // viewport fold). null when the active approval is visible (don't duplicate it
  // in the docked bar). Recomputed on scroll/resize/transcript-grow.
  const [offscreenApprovalSeq, setOffscreenApprovalSeq] = useState<number | null>(null);

  const { rows, resultByCallId, childrenByParent, usage, rateLimit, cost, contextCompacted } = useMemo(
    () => buildTranscript(events, pane.agentMidTurn),
    [events, pane.agentMidTurn],
  );

  // Bottom-bar "vitals" — read-only session indicators (model / context% / tokens /
  // cost / cache / resets / cwd) that moved OUT of the crowded header and into the
  // NativeMetaBar's right cluster. Which ones show is a global user pref; the values
  // are all derived from data already on the client (no daemon round-trip).
  const metaBarFields = useStore((s) => s.nativeMetaBarFields);
  // Last-known vitals snapshot for THIS pane, read ONCE on mount (synchronous
  // module-map / localStorage read). It rehydrates the bottom bar instantly when
  // a pane (re-)enters Focus mode, before the async seed (session_init/usage)
  // lands — killing the ~100-300ms blank flash of model/context/cost. Read into a
  // ref so it's a stable per-mount fallback that the live derives below override
  // the moment real data arrives; it is never re-read mid-mount (the live values
  // take over). Keyed by pane.id via a guarded re-read so a warm-pane remount onto
  // a different pane id picks up that pane's snapshot.
  const vitalsSnapRef = useRef<{ id: string; agent: typeof paneAgent; snap: ReturnType<typeof getVitalsSnapshot> }>({
    id: pane.id,
    agent: paneAgent,
    snap: getVitalsSnapshot(pane.id, paneAgent),
  });
  if (vitalsSnapRef.current.id !== pane.id || vitalsSnapRef.current.agent !== paneAgent) {
    vitalsSnapRef.current = {
      id: pane.id,
      agent: paneAgent,
      snap: getVitalsSnapshot(pane.id, paneAgent),
    };
  }
  const vitalsSnap = vitalsSnapRef.current.snap;

  const liveModelId = useMemo(
    () => modelIdForAgent(sessionInit?.model, paneAgent),
    [sessionInit?.model, paneAgent],
  );
  const liveModelLabel = useMemo(() => prettyModelName(liveModelId), [liveModelId]);
  // The pane DTO carries an explicitly-selected durable model synchronously.
  // It is the correct bridge after daemon reattach, when Codex's surviving
  // app-server does not emit another session_init. Provider validation keeps a
  // stale cross-agent pane field from entering the footer.
  const paneModelLabel = useMemo(
    () => prettyModelName(modelIdForAgent(pane.model, paneAgent)),
    [pane.model, paneAgent],
  );
  // A just-picked model, shown in the footer IMMEDIATELY so /model gives instant
  // feedback. Without it the footer keeps showing the OLD model until the next
  // turn: /model respawns the pane, and the post-respawn seed session_init is a
  // replay carrying the LAST assistant turn's model (i.e. the old one). The new
  // model only rides in on the next LIVE session_init.
  const pendingModelLabel = prettyModelName(modelIdForAgent(pendingModel ?? undefined, paneAgent));
  // Effective model: the just-picked model first (instant), then the LIVE label
  // once session_init has loaded, else the snapshot's last-known label so the
  // bar shows the right model on re-entry instead of blank.
  const modelLabel = pendingModelLabel || (
    sessionInit?.fromReplay
      ? (paneModelLabel || liveModelLabel)
      : (liveModelLabel || paneModelLabel)
  ) || vitalsSnap.model || '';
  // Drop the pending override once the live (non-replay) session_init confirms
  // the new model actually took — from then on the authoritative value drives.
  // Only a MATCHING live init clears it, so the stale old-model init still
  // present during the respawn window can't prematurely revert the footer.
  useEffect(() => {
    if (
      pendingModel &&
      sessionInit &&
      !sessionInit.fromReplay &&
      sameModelId(sessionInit.model, pendingModel)
    ) {
      setPendingModel(null);
    }
  }, [pendingModel, sessionInit]);
  // Context-window used %. Claude derives this from its split prompt/cache counts;
  // Codex uses its authoritative totalTokens/modelContextWindow pair (its input
  // count already includes cache reads). Undefined until the provider supplies
  // enough real data, so the footer hides instead of guessing or showing zero.
  //
  // contextCompacted short-circuits to undefined: /clear and older /compact
  // boundaries provide no post-reset reading, so `usage` is the STALE pre-reset
  // value. Current compact boundaries append postTokens usage and clear this
  // marker immediately. See buildTranscript's contextCompacted.
  const liveContextPct = useMemo(() => {
    if (contextCompacted) return undefined;
    return contextUsageFraction(usage, paneAgent, liveModelId);
  }, [usage, contextCompacted, liveModelId, paneAgent]);
  // Post-compact: skip the snapshot fallback too (it holds the pre-compact %), so the
  // chip stays hidden until a real post-compact usage lands.
  const contextPct = contextCompacted ? undefined : (liveContextPct ?? vitalsSnap.contextPct);
  // Cumulative cost: live value from the transcript, else the snapshot's last
  // known so a re-entered pane doesn't briefly read $0 / blank before usage folds.
  const effCost = cost ?? vitalsSnap.cost;
  // Folder path for the bottom-bar vital. Display = "…/<project folder>" so the
  // project name is ALWAYS whole, even on a narrow pane (the old 16ch char-clip
  // chopped it to "…/wsl-projects/s…"). The full absolute path rides the hover
  // title via metaCwdFull, so nothing is lost.
  //
  // cwd is SYNCHRONOUS on entry (pane.liveCwd/pane.cwd ride the pane DTO, present
  // before any native event), so it should never blank — but PREFER those pane
  // fields over sessionInit (which is async) so the folder shows on the very first
  // paint, and fall back to the snapshot only if even those are somehow empty.
  const metaCwdFull = pane.liveCwd || pane.cwd || sessionInit?.cwd || vitalsSnap.cwdFull || '';
  const metaCwd = useMemo(() => {
    if (!metaCwdFull) return vitalsSnap.cwd || '';
    const parts = metaCwdFull.split('/').filter(Boolean);
    return parts.length > 1 ? '…/' + parts[parts.length - 1] : metaCwdFull;
  }, [metaCwdFull, vitalsSnap.cwd]);

  // Persist the snapshot whenever a live vital becomes known so the NEXT entry to
  // Focus mode (a Terminal↔Focus flip, a warm remount, or a full reload) rehydrates
  // instantly. saveVitalsSnapshot merges + ignores undefined/empty, so a transient
  // blank derive never wipes a good snapshot, and it no-ops when nothing changed
  // (so this per-render effect isn't a localStorage write every frame).
  useEffect(() => {
    saveVitalsSnapshot(pane.id, {
      // Persist only provider-authoritative values (never the snapshot fallback)
      // so the snapshot reflects truth, never its own stale echo. The durable
      // pane model bridges a Codex daemon reattach that emits no new session_init.
      model: (sessionInit?.fromReplay ? paneModelLabel || liveModelLabel : liveModelLabel || paneModelLabel) || undefined,
      contextPct: liveContextPct,
      cost: cost ?? undefined,
      cwd: metaCwd || undefined,
      cwdFull: metaCwdFull || undefined,
    }, paneAgent);
  }, [pane.id, paneAgent, sessionInit?.fromReplay, liveModelLabel, paneModelLabel, liveContextPct, cost, metaCwd, metaCwdFull]);

  // On a /compact, the saved snapshot's contextPct is now stale (the window was
  // freed) and saveVitalsSnapshot above can't clear it (it ignores undefined, by
  // design). Drop just that field so a later Focus re-entry — which has no live
  // usage to fall back FROM — doesn't resurrect the pre-compact %. Model/cost/cwd
  // stay. The next post-compact turn re-derives and re-persists the real value.
  useEffect(() => {
    if (contextCompacted) clearVitalsContextPct(pane.id, paneAgent);
  }, [pane.id, paneAgent, contextCompacted]);

  // Apply a local /clear: hide every row whose underlying event seq was present
  // when /clear ran (the session + daemon history are untouched — this is a
  // view-only reset). New events (new seqs) render normally. Drop a now-leading
  // turn separator so the visible transcript doesn't open with a dangling divider.
  const visibleRows = useMemo(() => {
    if (clearedSeqs.size === 0) return rows;
    const kept = rows.filter((r) => r.kind !== 'event' || !clearedSeqs.has(r.event.seq));
    while (kept.length && kept[0].kind === 'turn-sep') kept.shift();
    return kept;
  }, [rows, clearedSeqs]);
  // How many rows the local clear is currently hiding (for the undo chip).
  const hiddenCount = clearedSeqs.size > 0 ? rows.length - visibleRows.length : 0;

  // Focus-mode terse pass: fold runs of adjacent tool calls / thinking blocks
  // into single expandable group rows. Runs AFTER the /clear filter so a group
  // only ever holds visible events.
  const groupedRows = useMemo(
    () => groupRows(visibleRows),
    [visibleRows],
  );

  // Coarse ticker so the turn-separator relative times ("2m ago") re-render as
  // the conversation sits idle. 30s cadence — cheap, and the labels are coarse
  // (seconds→minutes→hours) so finer wouldn't read differently.
  const [nowTick, setNowTick] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNowTick(Date.now()), 30000);
    return () => clearInterval(id);
  }, []);

  // The session has ended when the daemon reports the pane process exited. Mirror
  // the terminal pane's dignity: a terminal "Session ended" card + a disabled
  // composer. (status is 'running' | 'exited'.)
  const sessionEnded = pane.status === 'exited';

  // Is the latest assistant/thinking block still streaming? Drives the SINGLE
  // busy indicator: while text streams the caret IS the activity, so we suppress
  // the redundant spinner; the spinner only shows when a turn is in flight but
  // nothing has started streaming yet.
  const streaming = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) {
      const e = events[i];
      if (e.streaming) return true;
      if (e.type === 'user_text' || e.type === 'turn_end' || e.type === 'error') break;
    }
    return false;
  }, [events]);

  // The keyboard-active tool approval to surface in the docked bar — only when
  // it's actually off-screen (so a visible card never gets a redundant dock).
  const dockedTarget = useMemo(
    () =>
      offscreenApprovalSeq != null
        ? toolApprovals.find((a) => a.seq === offscreenApprovalSeq) ?? null
        : null,
    [offscreenApprovalSeq, toolApprovals],
  );

  // Recompute whether the FIRST open tool approval's card is off-screen relative
  // to the scroll body. Cheap (a couple of getBoundingClientRect on the body +
  // the one target card); called on scroll, resize, and after the transcript
  // changes. A card with no DOM node yet (just rendered) counts as off-screen so
  // the dock appears immediately rather than after the next scroll tick.
  const recomputeOffscreen = useCallback(() => {
    const body = bodyRef.current;
    const first = toolApprovals[0];
    if (!body || !first || first.seq == null) {
      setOffscreenApprovalSeq(null);
      return;
    }
    const card = approvalCardRefs.current.get(first.seq);
    if (!card) {
      setOffscreenApprovalSeq(first.seq);
      return;
    }
    const b = body.getBoundingClientRect();
    const c = card.getBoundingClientRect();
    // Off-screen if the card is entirely above the top or below the bottom of
    // the visible body (a small slop so a barely-peeking card still counts as
    // visible and doesn't flicker the dock).
    const offscreen = c.bottom < b.top + 8 || c.top > b.bottom - 8;
    setOffscreenApprovalSeq(offscreen ? first.seq : null);
  }, [toolApprovals]);

  const onScroll = () => {
    const el = bodyRef.current;
    if (!el) return;
    const stuck = el.scrollHeight - el.scrollTop - el.clientHeight < STICK_BOTTOM_SLOP;
    // While we're following, an off-bottom position with no user gesture behind
    // it is a reflow artifact (content-visibility row sizing, scroll anchoring),
    // not a scroll-away: re-pin instead of latching the "N new" chip on.
    if (!stuck && stickRef.current && !hasScrollIntent()) {
      el.scrollTop = el.scrollHeight;
      recomputeOffscreen();
      return;
    }
    stickRef.current = stuck;
    setAtBottom(stuck);
    if (stuck) setNewCount(0);
    // Remember the position — but only while the body is actually on screen.
    // When the pane is parked (display:none) the browser fires a scroll to 0;
    // persisting that would clobber the real spot we want to return to.
    if (el.clientHeight > 0) scrollMemByPane.set(pane.id, { top: el.scrollTop, atBottom: stuck });
    recomputeOffscreen();
  };

  // Restore the saved scroll position (called on mount and whenever the pane is
  // un-parked). At-bottom / first-open → snap to the latest; otherwise return to
  // the remembered offset, clamped in case the transcript grew while away.
  const restoreScrollPosition = useCallback(() => {
    const el = bodyRef.current;
    if (!el || el.clientHeight === 0) return;
    const mem = scrollMemByPane.get(pane.id);
    if (!mem || mem.atBottom) {
      el.scrollTop = el.scrollHeight;
      stickRef.current = true;
      setAtBottom(true);
    } else {
      el.scrollTop = Math.min(mem.top, el.scrollHeight - el.clientHeight);
      stickRef.current = false;
      setAtBottom(false);
    }
  }, [pane.id]);

  const setDraftValue = useCallback(
    (v: string) => {
      setDraft(v);
      persistDraft(pane.id, v);
    },
    [pane.id],
  );

  // Splice the picked @-mention file in as "@<rel> " and park the caret after
  // it. The trailing space ends the token, so the menu self-closes
  // (findAtMention → null on the next derivation).
  const pickAtMention = useCallback(
    (hit: AtMentionHit) => {
      const m = findAtMention(draft, caretPos);
      if (!m) return;
      const res = applyAtMention(draft, m.start, caretPos, hit.rel);
      setDraftValue(res.draft);
      setCaretPos(res.caret);
      requestAnimationFrame(() => {
        const el = taRef.current;
        if (el) {
          el.focus();
          el.selectionStart = el.selectionEnd = res.caret;
        }
      });
    },
    [draft, caretPos, setDraftValue],
  );

  // Image attach (paste / drop / 📎). Upload the bytes to the pane's paste dir
  // and splice the returned path into the draft — headless `claude` reads the
  // `@`-mention inline (verified in stream-json mode); native Muse panes get
  // the plain path instead (muse has no @-mention syntax — parity with the
  // terminal Muse panes) and the daemon forwards the file via --image. Same
  // flow the terminal panes and MobileQuickReply use. Functional setDraft +
  // persist so a resolve that lands after further typing appends rather than
  // clobbering, then refocus the composer so the user can add a caption and send.
  const attachImage = useCallback(
    async (file: File) => {
      if (!file.type.startsWith('image/')) {
        useStore.getState().toast('Only images can be attached', { kind: 'warn', ttlMs: 3000 });
        return;
      }
      try {
        const { path } = await api.pasteImage(pane.id, file);
        setDraft((cur) => {
          // Muse has no `@`-mention syntax (and its TUI fails attaching it):
          // splice the plain path, which the daemon forwards via --image.
          const next = paneAgent === 'muse' ? insertPlainPathToken(cur, path) : insertAttachToken(cur, path);
          persistDraft(pane.id, next);
          return next;
        });
        useStore
          .getState()
          .toast(`Attached ${path.split(/[\\/]/).pop()}`, { kind: 'info', ttlMs: 2500 });
        requestAnimationFrame(() => {
          const el = taRef.current;
          if (el) {
            // The upload is async; if the user moved to another composer while
            // it was in flight, don't yank the caret back to this pane.
            if (isTypingElsewhere(el)) return;
            el.focus();
            el.selectionStart = el.selectionEnd = el.value.length;
          }
        });
      } catch (err) {
        useStore
          .getState()
          .toast(`Attach failed: ${(err as Error).message}`, { kind: 'error', ttlMs: 4000 });
      }
    },
    [pane.id, paneAgent],
  );

  // Composer paste: intercept an image on the clipboard and attach it; let plain
  // text paste fall through to the textarea's default handling.
  const onComposerPaste = useCallback(
    (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
      const cd = e.clipboardData;
      const img = firstImageFile(cd?.files, cd?.items);
      if (img) {
        e.preventDefault();
        void attachImage(img);
      }
    },
    [attachImage],
  );

  // Drag-and-drop an image file onto the pane body (parity with terminal panes).
  const onBodyDragOver = useCallback((e: React.DragEvent) => {
    if (!hasImagePayload(e.dataTransfer)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  }, []);
  const onBodyDrop = useCallback(
    (e: React.DragEvent) => {
      const img = firstImageFile(e.dataTransfer?.files, e.dataTransfer?.items);
      if (!img) return;
      e.preventDefault();
      e.stopPropagation();
      void attachImage(img);
    },
    [attachImage],
  );

  // Clicking anywhere in the transcript focuses the composer — parity with
  // Term panes, where a body click focuses the terminal (typing "just works"
  // after any click into the pane). Skipped when the click finished a text
  // selection (copying transcript text must not yank focus), when it landed
  // on an interactive element (buttons/links/inputs own their focus), and
  // while an approval is pending (the approval bar owns the keyboard).
  const onBodyClick = useCallback(
    (e: React.MouseEvent) => {
      if (window.getSelection()?.toString()) return;
      const t = e.target as HTMLElement | null;
      if (t?.closest('button, a, input, textarea, select, [contenteditable="true"]')) return;
      if (pendingApproval) return;
      taRef.current?.focus();
    },
    [pendingApproval],
  );

  // Subscribe this mounted composer to external draft pushes (the message-action
  // "edit & resend" calls persistDraft from outside the component). Mirror the
  // pushed value into local state so the textarea updates. Guard against echoing
  // our OWN writes: setDraftValue already set both, so only react when the module
  // value diverges from what we're holding. Re-subscribes on pane change.
  useEffect(() => {
    return subscribeDraft(pane.id, (v) => {
      setDraft((cur) => (cur === v ? cur : v));
    });
  }, [pane.id]);

  // Pin the scroll body to the bottom, but only if the user is "stuck" there
  // (hasn't scrolled up). The single chokepoint every growth/resize path calls so
  // "follow the latest" behaves identically everywhere.
  const scrollToBottomIfStuck = useCallback(() => {
    const el = bodyRef.current;
    if (el && stickRef.current) el.scrollTop = el.scrollHeight;
  }, []);

  // Auto-scroll: stick to bottom (if the user hasn't scrolled up) as the
  // transcript grows. Split into two paths instead of one useLayoutEffect keyed
  // on the whole `events` array (the old version): the array gets a NEW identity
  // every streamed token, so it fired + did a synchronous scrollHeight read
  // (forced reflow) per token — the felt jank.
  //
  // STRUCTURAL growth (a NEW event: tool card, user turn, first delta of a block;
  // or an optimistic pending row) is low-frequency, so keep it pre-paint
  // (useLayoutEffect) to avoid any flash of unscrolled content on mount / new
  // row. Keyed off the scalar counts, not the array identity.
  useLayoutEffect(() => {
    const el = bodyRef.current;
    if (!el || !stickRef.current) return;
    el.scrollTop = el.scrollHeight;
    // content-visibility rows report an estimated height until the browser
    // renders them, so scrollHeight can grow after this write and leave us just
    // above the true bottom. Re-pin once the corrected layout has settled.
    const id = requestAnimationFrame(() => {
      const node = bodyRef.current;
      if (node && stickRef.current) node.scrollTop = node.scrollHeight;
    });
    return () => cancelAnimationFrame(id);
  }, [events.length, pending.length]);

  // STREAMING growth (in-place delta merge into the tail block → events.length
  // unchanged, only the tail event's text length grows ~60×/sec) is high-
  // frequency, so rAF-coalesce: at most one scrollHeight read + write per frame
  // regardless of token rate. A ≤1-frame lag in the snap is imperceptible while
  // text streams. The signal is the last event's text length (the streaming
  // block merges deltas in place, so events.length is unchanged but its .text
  // grows), so this fires per frame only while the tail block is streaming.
  const lastEvent = events.length ? events[events.length - 1] : undefined;
  const tailLen = lastEvent?.text?.length ?? 0;

  // In-pane find (Ctrl+F / cross-pane reveal). Client-side over the rendered
  // transcript — the Focus-mode counterpart to usePaneFind's xterm search, so
  // both renderers search the same way. The hook re-walks on its own via a
  // MutationObserver while active, so it stays live as the conversation streams.
  const find = useNativeFind({
    paneId: pane.id,
    transcriptRef,
    composerRef: taRef,
  });
  // Stable provider value so the memoized foldable views only re-render when the
  // find-active flag actually flips — NOT on every streamed token (NativePane
  // re-renders per delta; a fresh {active} object each time would defeat their memo).
  const findCtx = useMemo(() => ({ active: find.active }), [find.active]);

  useEffect(() => {
    if (!stickRef.current) return;
    const id = requestAnimationFrame(() => {
      const el = bodyRef.current;
      if (el && stickRef.current) el.scrollTop = el.scrollHeight;
    });
    return () => cancelAnimationFrame(id);
  }, [tailLen]);

  // Tally NEW rows that land while the user is scrolled up, so the jump pill can
  // read "N new" (cleared the moment they're back at the bottom — see onScroll).
  // Only STRUCTURAL growth counts (a genuinely new row); in-place token streaming
  // doesn't change events.length, so it never inflates the count.
  useEffect(() => {
    const prev = prevEventsLenRef.current;
    prevEventsLenRef.current = events.length;
    // First run (incl. a remount with the transcript already seeded) just records
    // the baseline so a restored, scrolled-up pane shows "0 new" — not the whole
    // history miscounted as unread.
    if (prev === 0) return;
    if (!stickRef.current && events.length > prev) {
      setNewCount((c) => Math.min(999, c + (events.length - prev)));
    }
  }, [events.length]);

  // Recompute the off-screen-approval dock after the transcript / approval set
  // changes (a new approval card may land below the fold) and on viewport resize.
  // Runs post-DOM so the just-rendered card's rect is measurable.
  useLayoutEffect(() => {
    recomputeOffscreen();
  }, [recomputeOffscreen, rows.length, events]);

  useEffect(() => {
    if (typeof ResizeObserver === 'undefined') return;
    const el = bodyRef.current;
    if (!el) return;
    // The body's height shrinks/grows when the docked metabar toggles between its
    // working / done / idle states (and on pane resize). Re-pin to the bottom so
    // a stuck reader stays glued to the latest instead of drifting up a few px.
    const ro = new ResizeObserver(() => {
      const node = bodyRef.current;
      const h = node?.clientHeight ?? 0;
      // A 0 → >0 height transition is the pane being un-parked from the warm
      // hidden host (or the first mount): restore the remembered scroll spot.
      // Any other resize just keeps a stuck reader pinned to the latest.
      if (lastBodyHeightRef.current === 0 && h > 0) {
        restoreScrollPosition();
      } else {
        scrollToBottomIfStuck();
      }
      lastBodyHeightRef.current = h;
      recomputeOffscreen();
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [recomputeOffscreen, scrollToBottomIfStuck, restoreScrollPosition]);

  // Scroll the active off-screen approval card into view (docked-bar "jump to").
  const jumpToApproval = useCallback(() => {
    const seq = dockedTarget?.seq;
    if (seq == null) return;
    const card = approvalCardRefs.current.get(seq);
    card?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }, [dockedTarget]);

  // The daemon's AUTHORITATIVE in-turn flag (nativeInTurn). True for the WHOLE
  // turn — including the event-less gap after a tool_result while the model
  // generates, and the usage BURSTS Claude emits after every tool round (which
  // the event-scan alone mistook for a settle). Passed RAW (boolean | undefined)
  // so computeNativeWorking can fall back to the old heuristic for a daemon that
  // predates the field; a current daemon always reports a real boolean.
  const agentMidTurn = pane.agentMidTurn;

  // Working = a turn is in flight (drives the spinner + busy dot + steer/queue).
  // The settle rules + the daemon-flag authority live in the pure
  // computeNativeWorking helper (unit-tested): turn_end/error hard-settle; a bare
  // usage never settles a turn the daemon says is live; otherwise agentMidTurn is
  // the ground truth.
  const working = useMemo<NativeEvent | null>(
    () => computeNativeWorking(events, pending.length, agentMidTurn, pane.id),
    [events, pending.length, agentMidTurn, pane.id],
  );

  const inFlight = working != null || sending;
  // A turn is actively streaming/working (and no approval is blocking) — the
  // window where a typed message must QUEUE (steer) rather than send immediately.
  const turnBusy = inFlight && !hasApproval;
  // Prompt-suggestion ghost text (Claude's own "next prompt"). Shown only over
  // an empty composer when idle (no queued msg, no turn in flight) and the
  // per-machine setting is on. Tab (keydown step 0) accepts it.
  const ghostText =
    promptSuggestionsOn && !!promptSuggestion?.text && !draft && pending.length === 0 && !turnBusy
      ? promptSuggestion.text
      : undefined;

  // --- dedicated status line (the metabar) ---
  // Phase + label for the always-present status row docked under the composer.
  // working → spinner + label + live elapsed; done → "✓ worked for Xs" from the
  // last turn_end's duration (the terminal-style "Claude finished" cue); idle →
  // blank (reserved). turnStartedAt anchors the elapsed timer; set on the
  // not-working → working edge so it measures THIS turn.
  const isWorking = working != null;
  const [turnStartedAt, setTurnStartedAt] = useState<number | null>(null);
  const wasWorkingRef = useRef(false);
  useEffect(() => {
    if (isWorking && !wasWorkingRef.current) setTurnStartedAt(Date.now());
    wasWorkingRef.current = isWorking;
  }, [isWorking]);
  // /compact runs THROUGH the model (~1 min) with no inherent "compacting" signal,
  // so the generic "Claude is working…" reads as stuck/broken until it silently
  // finishes (the recurring "/compact doesn't work" report). Track that the live
  // turn is a /compact — set when we send it (immediate or from the steer queue),
  // cleared the moment the turn settles — to label the metabar honestly.
  const [compactingSince, setCompactingSince] = useState<number | null>(null);
  const compacting = compactingSince != null && isWorking;
  useEffect(() => {
    if (!isWorking && compactingSince != null) setCompactingSince(null);
  }, [isWorking, compactingSince]);
  // Duration of the most recently COMPLETED turn (latest turn_end). Survives into
  // the idle "done" state and is present on mount from seeded history.
  const doneDurationMs = useMemo(() => lastTurnDuration(events), [events]);
  // Once the turn has settled, how many run_in_background shells launched but haven't
  // reported completion. >0 ⇒ the agent is PARKED waiting on them (resumes when one
  // finishes via a <task-notification>, or you message it). Surfaced in the metabar so
  // a bare "✓ worked for Xs" doesn't read as "maybe still going" — the exact "running
  // or stuck?" ambiguity. 0 while working.
  const idleBgShells = useMemo(
    () => idleBackgroundShells(events, resultByCallId, isWorking),
    [events, resultByCallId, isWorking],
  );
  // Same signal for background AGENTS (the Agent tool): between their launch
  // turn settling and the <task-notification> re-prompt, a bare "✓ worked for Xs"
  // reads as stale — this is the parked half of the 2026-07-09 report.
  const idleBgAgents = useMemo(
    () => idleBackgroundAgents(events, resultByCallId, isWorking),
    [events, resultByCallId, isWorking],
  );
  const statusPhase: 'working' | 'done' | 'idle' = isWorking
    ? 'working'
    : doneDurationMs != null
      ? 'done'
      : 'idle';
  const statusLabel = !connected
    ? 'Reconnecting…'
    : compacting
      ? 'Compacting conversation…'
      : streaming
        ? 'Responding…'
        : working?.detail || 'Thinking…';
  // The metabar reserves a fixed height so toggling phases never shifts the
  // transcript, but its first appearance (and the done→idle settle) can still nudge
  // the body height by a hair — re-pin a stuck reader to the bottom on each change.
  useLayoutEffect(() => {
    scrollToBottomIfStuck();
  }, [statusPhase, scrollToBottomIfStuck]);

  // sendText — fire one optimistic message. Shared by the composer submit, the
  // queue-flush, and any client command that needs to send. Returns the promise
  // so callers can sequence / re-focus.
  const sendText = useCallback(
    (text: string) => {
      const t = text.trim();
      if (!t) return Promise.resolve();
      stickRef.current = true;
      setAtBottom(true);
      setSending(true);
      return sendOptimistic(t)
        .catch(() => {
          // Restore into the draft so the user can edit + resend — but only if the
          // composer is still empty (don't clobber something they've started
          // typing). Write through the per-pane map so the restore survives remount.
          setDraft((cur) => {
            const next = cur ? cur : t;
            persistDraft(pane.id, next);
            return next;
          });
        })
        .finally(() => {
          setSending(false);
          // Async refocus of THIS pane's composer after the send resolves. Skip
          // it if the user has since moved the caret into another editable (e.g.
          // a queued-steer auto-flush settling while they type elsewhere) — never
          // yank focus back on a delayed promise.
          if (focused && !pendingApproval && !isTypingElsewhere(taRef.current)) taRef.current?.focus();
        });
    },
    [sendOptimistic, focused, pendingApproval, pane.id],
  );

  // resendPrompt — the message-action "resend" / "retry": send the prompt verbatim
  // as a NEW optimistic user turn (no composer round-trip). Snap to bottom so the
  // new turn is visible.
  const resendPrompt = useCallback(
    (text: string) => {
      const t = text.trim();
      if (!t) return;
      stickRef.current = true;
      setAtBottom(true);
      void resend(t);
    },
    [resend],
  );

  // The last user prompt text (newest user_text), for the error-card "retry".
  // null when there's no prior prompt to replay.
  const lastUserPrompt = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) {
      if (events[i].type === 'user_text' && events[i].text) return events[i].text as string;
    }
    return null;
  }, [events]);

  // submit — the composer Enter / Send action. While a turn is busy, QUEUE the
  // message as a follow-up (steering) instead of sending now; it auto-sends when
  // the turn settles (effect below). Slash commands are handled before this is
  // reached (the form's onKeyDown intercepts Enter when the menu is open).
  const submit = () => {
    const text = draft.trim();
    if (!text || sending) return;
    setHistoryNav(HISTORY_IDLE);
    if (turnBusy) {
      // Steer: append as a queued follow-up (FIFO — earlier ones aren't lost; a
      // long turn can accumulate several, each flushed in order on settle) and
      // clear the composer. A chip per message shows near the composer + a ✕.
      setQueued((q) => [...q, text]);
      setDraftValue('');
      return;
    }
    setDraftValue(''); // clear synchronously — instant, not gated on the POST
    if (isCompactCommand(text)) setCompactingSince(Date.now());
    void sendText(text);
  };

  // Auto-send the queued steer messages once the current turn settles (no longer
  // working + no approval blocking). FIFO, one per settle: dequeue the head and
  // send it; sending flips `sending` synchronously so turnBusy is true again on
  // the next render, gating the effect until THAT turn settles — so each queued
  // message gets its own turn, in order. Dequeuing before the send prevents a
  // re-entrant double-send.
  useEffect(() => {
    if (queued.length === 0) return;
    if (turnBusy) return; // still busy — wait
    if (hasApproval) return; // an approval is blocking; don't barge past it
    const [msg, ...rest] = queued;
    setQueued(rest);
    if (isCompactCommand(msg)) setCompactingSince(Date.now());
    void sendText(msg);
  }, [queued, turnBusy, hasApproval, sendText, setQueued]);

  const stop = async () => {
    if (interrupting) return;
    // Stopping the turn also cancels any queued follow-up so it can't fire into
    // the just-interrupted session (the user hit Stop — they don't expect a
    // steer to land seconds later). Fold the queued text back into the composer
    // below any in-progress draft so NOTHING is lost. Read LIVE from the module
    // maps — stop() is also invoked from the window ESC listener, whose closure
    // can be a render behind.
    const q = getQueued(pane.id);
    if (q.length > 0) {
      const merged = [getDraft(pane.id), ...q].map((s) => s.trim()).filter(Boolean).join('\n\n');
      setDraftValue(merged);
      setQueued([]);
    }
    setInterrupting(true);
    try {
      await interrupt();
    } catch {
      // benign — daemon is idempotent
    } finally {
      setInterrupting(false);
    }
  };

  // In-pane renderer toggle (the Term segment). Switch immediately when idle,
  // else open the three-way confirm so an in-flight turn isn't silently aborted.
  const switching = useStore((s) => !!s.paneModeSwitching[pane.id]);
  const stillWorking = shouldConfirmModeSwitch(pane.state, pane.agentMidTurn);
  // The armed-but-waiting deferred switch (server state) + precedence-resolved
  // toggle visual (flipping > pending > idle).
  const paneePendingSwitch = pane.pendingSwitch ?? null;
  const toggleVisualState = toggleVisual(switching, paneePendingSwitch);
  const requestModeSwitch = useCallback(
    (to: '' | 'native') => {
      if (switching) return;
      if (shouldConfirmModeSwitch(pane.state, pane.agentMidTurn)) {
        setConfirmTarget(to);
        return;
      }
      void switchPaneMode(pane.id, to);
    },
    [switching, pane.state, pane.agentMidTurn, pane.id, switchPaneMode],
  );
  // Cancel an armed pending switch (plain-click while pending, or its ✕).
  const cancelPendingSwitch = useCallback(() => {
    const kind = pendingSwitchKind(paneePendingSwitch);
    if (kind == null) return;
    void switchPaneMode(pane.id, kind, { cancel: true });
  }, [paneePendingSwitch, pane.id, switchPaneMode]);
  // Override an armed pending switch → switch now (Shift-click the target).
  const switchNowFromPending = useCallback(() => {
    const kind = pendingSwitchKind(paneePendingSwitch);
    if (kind == null) return;
    void switchPaneMode(pane.id, kind, { when: 'now' });
  }, [paneePendingSwitch, pane.id, switchPaneMode]);

  // The mid-turn interruption marker: render only a LIVE notice the user hasn't
  // dismissed in this session (keyed by switchNotice.at).
  const showInterrupted = shouldShowSwitchNotice(pane.switchNotice, dismissedNotices);
  // Continue after an interruption: send a short resume turn (which clears the
  // daemon's notice on the next input). Also drop the local dismiss key so the
  // banner clears immediately rather than waiting for the switchNotice to null.
  const continueAfterInterrupt = useCallback(() => {
    const key = switchNoticeKey(pane.switchNotice);
    if (key != null) setDismissedNotices((s) => new Set(s).add(key));
    void sendText('Continue where you left off.');
  }, [pane.switchNotice, sendText]);
  // Dismiss the interruption marker locally (the daemon also clears the notice
  // on the next user turn). Keyed by switchNotice.at so it stays dismissed for
  // THIS interruption only.
  const dismissNotice = useCallback(() => {
    const key = switchNoticeKey(pane.switchNotice);
    if (key != null) setDismissedNotices((s) => new Set(s).add(key));
  }, [pane.switchNotice]);

  // ---- client slash commands (/clear, /help) ----
  const runSlashCommand = useCallback(
    (cmd: SlashCommand) => {
      setDraftValue(''); // consume the "/cmd" text
      setSlashIndex(0);
      if (cmd.action === 'clear' || cmd.action === 'clear-real') {
        // Hide everything currently rendered (snapshot ALL current event seqs —
        // replayed history + live alike). New events render normally.
        const snapshot = () => {
          const seqs = new Set<number>();
          for (const e of events) if (typeof e.seq === 'number' && e.seq >= 0) seqs.add(e.seq);
          setClearedSeqsState(seqs);
          setClearedSeqs(pane.id, seqs);
        };
        if (cmd.action === 'clear-real') {
          // REAL reset: forward "/clear" to wipe the agent's context (it rotates the
          // session) AND reset the view. Destructive → confirm first. Guard like a
          // steer so it never barges an in-flight turn / parked approval.
          if (turnBusy || hasApproval) {
            useStore.getState().toast('Wait for the current turn to finish');
          } else {
            void confirmDialog({
              message:
                "Reset Claude's memory and start fresh? This clears the conversation context for this pane. Your transcript stays saved to disk.",
              confirmLabel: 'Reset',
              danger: true,
            }).then((ok) => {
              if (!ok) return;
              void sendText('/clear');
              snapshot();
              setRealClear(true);
              persistRealClear(pane.id, true);
            });
          }
        } else {
          // View-only: hide rows; the session + memory are untouched, undo available.
          snapshot();
          setRealClear(false);
          persistRealClear(pane.id, false);
        }
        setShowCheatsheet(false);
      } else if (cmd.action === 'forward') {
        // Forward verbatim to the agent (/context, /cost, /config, /compact run
        // headless). Steer exactly like submit(): if a turn is in flight, QUEUE it
        // so it auto-sends when the turn settles (effect above) — NEVER silently
        // drop it. Previously this toasted-and-discarded on turnBusy, so a /compact
        // fired mid-turn vanished entirely (the bug). Only a live approval truly
        // blocks — you can't barge a permission fork.
        if (hasApproval) {
          useStore.getState().toast('Resolve the pending approval first');
        } else if (turnBusy) {
          setQueued((q) => [...q, cmd.name]);
        } else {
          if (isCompactCommand(cmd.name)) setCompactingSince(Date.now());
          void sendText(cmd.name);
        }
        setShowCheatsheet(false);
      } else if (cmd.action === 'help') {
        setShowCheatsheet(true);
      } else if (cmd.action === 'model') {
        // Prefer the optimistic pick and durable pane flag over a replay init:
        // after /model that replay describes the last assistant turn (old model).
        const currentId = modelIdForAgent(pendingModel ?? pane.model ?? sessionInit?.model, paneAgent);
        const cur = MODEL_OPTIONS.findIndex((o) => sameModelId(currentId, o.id));
        setModelIndex(cur >= 0 ? cur : 0);
        setModelPickerOpen(true);
      }
      // Refocus this pane's composer after running a slash command — but not if
      // the user has moved the caret into another editable in the meantime.
      if (focused && !pendingApproval && !isTypingElsewhere(taRef.current)) taRef.current?.focus();
    },
    [events, pane.id, pane.model, paneAgent, pendingModel, focused, pendingApproval, setDraftValue, setQueued, setCompactingSince, turnBusy, hasApproval, sendText, sessionInit?.model],
  );

  // completeSlashCommand — Tab / click on a slash-menu item AUTOCOMPLETES it into
  // the composer ("/cmd " + trailing space) so the user can add arguments (e.g.
  // "/review 123", "/compact focus on the API", "/config key=value") and press
  // Enter to send — matching Term mode's slash menu. Without this, picking any
  // forwarded command SENT it immediately, so arg-taking skills (/review, /compact
  // …) were unusable from the menu. Client-only commands (/clearview view-hide,
  // /help cheatsheet, /clear confirm-reset) aren't text to send, so they RUN
  // immediately via runSlashCommand as before. Enter still runs/sends directly
  // for the quick no-argument path.
  const completeSlashCommand = useCallback(
    (cmd: SlashCommand) => {
      if (cmd.action === 'forward') {
        setDraftValue(cmd.name + ' '); // "/cmd " — trailing space parks the caret for args
        setSlashIndex(0);
        // The trailing space makes isSlashTrigger false, so the menu closes. Focus
        // the composer + put the caret at the end so the user can type args at once
        // (guarded like runSlashCommand so a delayed rAF never yanks focus into
        // another editable the user has since moved to).
        requestAnimationFrame(() => {
          const el = taRef.current;
          if (el && focused && !pendingApproval && !isTypingElsewhere(el)) {
            el.focus();
            el.selectionStart = el.selectionEnd = el.value.length;
          }
        });
        return;
      }
      runSlashCommand(cmd); // clear / clear-real / help / model → run the client action now
    },
    [runSlashCommand, setDraftValue, focused, pendingApproval],
  );

  // Pick a model from the /model menu: respawn-with---model via the store
  // (optimistic flip + new-pid reconcile). Busy panes are refused client-side
  // with the same copy the daemon's 409 would give — no half-open picker state.
  const pickModel = useCallback(
    (opt: ModelOption) => {
      setModelPickerOpen(false);
      if (turnBusy || hasApproval) {
        useStore.getState().toast('Wait for the current turn to finish before changing model');
        return;
      }
      // Reflect the pick in the footer right away (pendingModel); the store's
      // respawn + toast follow. Cleared here if the change fails, or by the
      // effect above once the live session_init confirms it.
      setPendingModel(opt.id);
      void setPaneModel(pane.id, opt.id).catch(() => setPendingModel(null));
    },
    [pane.id, turnBusy, hasApproval, setPaneModel],
  );

  // Undo a local /clear (re-show the hidden history). Surfaced as a small chip.
  const undoClear = useCallback(() => {
    setClearedSeqsState(new Set());
    setClearedSeqs(pane.id, new Set());
    setRealClear(false);
    persistRealClear(pane.id, false);
  }, [pane.id]);

  // Auto-grow the textarea to fit its content (capped). Runs on every draft
  // change (including the synchronous clear after submit, which resets to min).
  useLayoutEffect(() => {
    const el = taRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 200) + 'px';
  }, [draft]);

  // Re-seed the draft + cleared-view floor + queued steer from the per-pane maps
  // when the pane changes (warm-pane remount / pane swap). NOT destructive —
  // preserves an unsent draft, a local /clear, AND any queued follow-up (which
  // used to be reset to null here, silently dropping a steer on every remount).
  // Reset only the truly transient pane-scoped keyboard state (history cursor,
  // slash highlight).
  // A Focus pane existing IS Focus usage — permanently suppress the
  // "Try Focus" discovery hint (PaneHeader) for this install.
  useEffect(() => {
    markFocusUsed();
  }, []);

  useEffect(() => {
    setDraft(getDraft(pane.id));
    setClearedSeqsState(getClearedSeqs(pane.id));
    setRealClear(getRealClear(pane.id));
    setHistoryNav(HISTORY_IDLE);
    setQueuedState(getQueued(pane.id));
    setSlashIndex(0);
    setShowCheatsheet(false);
    setConfirmTarget(null);
    setDismissedNotices(new Set());
    setCaretPos(0);
    setAtIndex(0);
    setAtHits([]);
    setAtDismissedStart(null);
    setModelPickerOpen(false);
    setModelIndex(0);
  }, [pane.id]);

  // Keep the slash highlight in range as the filter narrows the list.
  useEffect(() => {
    setSlashIndex((i) => (slashCommands.length === 0 ? 0 : Math.min(i, slashCommands.length - 1)));
  }, [slashCommands.length]);

  // Re-seed the controlled question selection whenever the active question
  // changes (new requestId) so a fresh card starts with empty picks.
  useEffect(() => {
    setQuestionSel(kbQuestions ? kbQuestions.map(() => []) : []);
  }, [kbQuestion?.requestId, kbQuestions]);

  // Auto-focus the composer when this pane is focused (and not mid-approval, so
  // we don't steal focus from the approval buttons, and not while dragging). When
  // an approval appears WHILE the composer holds focus, blur it so Enter/Esc go
  // to the approval keyboard handler instead of being swallowed by the textarea's
  // own Enter-to-send (which preventDefaults). The user can still click back into
  // the composer to type a message instead of approving.
  useEffect(() => {
    // Grab the caret only when the user DELIBERATELY selected this pane —
    // setFocus / explicit spawn stamps lastUserFocusId (click, keyboard-nav,
    // palette, reveal, split). Spend that marker UP-FRONT, before any early
    // return, so it is truly one-shot in TIME: if this run can't act on it
    // (mobile, the mid-approval blur branch, not-yet-focused), the privilege is
    // consumed anyway. Otherwise a later BACKGROUND re-run — an ARIA auto-resolve
    // clearing pendingApproval, a drag ending, a reconnect — would still see
    // userPicked=true and bypass the typing guard, stealing the caret. (Audit
    // 2026-06-26: this was the recurring steal.)
    const userPicked = useStore.getState().lastUserFocusId === pane.id;
    if (userPicked) useStore.getState().consumeUserFocus();
    if (focused && !pendingApproval && !beingDragged) {
      // Mobile: never programmatically focus the composer. xterm/native focus
      // on mobile summons the soft keyboard AND pans the viewport to chase the
      // textarea (matches Pane.tsx). Mobile users focus by tapping the composer
      // directly (native focus); we must not auto-steal on every selection.
      if (isMobileViewport()) return;
      // Without a fresh deliberate selection, fall through to safe-by-default:
      // never steal from a composer the user is typing in (background MCP/ARIA/
      // workspace switch can't pull the caret either).
      if (!userPicked && isTypingElsewhere(taRef.current)) {
        // Opt-in instrumentation: set window.SOLU_FOCUS_DEBUG=true in devtools to
        // confirm steals are being blocked rather than firing.
        if ((window as unknown as { SOLU_FOCUS_DEBUG?: boolean }).SOLU_FOCUS_DEBUG) {
          // eslint-disable-next-line no-console
          console.debug('[focus-guard] blocked steal →', pane.id, 'caret in',
            (document.activeElement as HTMLElement | null)?.tagName);
        }
        return;
      }
      taRef.current?.focus();
    } else if (pendingApproval && document.activeElement === taRef.current) taRef.current?.blur();
  }, [focused, pane.id, pendingApproval, beingDragged]);

  // Keyboard on the active tool approval. Window-level so it works regardless of
  // which element holds focus, but gated to THIS pane being focused so a
  // background pane never grabs the keystroke. The daemon resolve is idempotent.
  //   • Esc always denies (any tool, destructive or not).
  //   • Enter approves ONLY when the active approval is explicitly read-only.
  //     Unknown provider tools, writes, and commands require an explicit click.
  //     Enter on any other approval does nothing (falls through), preserving
  //     the composer's normal Enter-to-send only if no approval consumes it.
  //   • Enter always yields to a non-empty composer draft (so the user can still
  //     send a message mid-approval instead of approving).
  //   • Shift+A approves only pending read requests, never writes/commands.
  useEffect(() => {
    if (!focused || !kbApproval?.requestId) return;
    const reqId = kbApproval.requestId;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      // Don't answer THIS pane's approval when the caret is actually in another
      // editable (another pane's composer / a field): focusedPaneId can drift to
      // this pane via background activity (MCP open / ARIA spawn) while the user
      // keeps typing elsewhere — their Esc/Enter belongs where the caret is.
      if (isTypingElsewhere(taRef.current)) return;
      const ae = document.activeElement as HTMLElement | null;
      const composerBusy =
        ae?.tagName === 'TEXTAREA' && (ae as HTMLTextAreaElement).value.trim() !== '';
      if (e.key === 'Escape') {
        e.preventDefault();
        void resolve(reqId, false);
        return;
      }
      // Shift+A: batch-approve read-only requests. Unknown and mutating tools
      // are intentionally excluded.
      if (
        (e.key === 'A' || e.key === 'a') &&
        e.shiftKey &&
        !e.metaKey &&
        !e.ctrlKey &&
        !e.altKey &&
        toolApprovals.filter(isReadLikeApproval).length >= 2 &&
        ae?.tagName !== 'TEXTAREA'
      ) {
        e.preventDefault();
        approveAllReads();
        return;
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey) {
        if (composerBusy) return; // yield to a non-empty composer draft
        if (!kbReadOnly) return;
        e.preventDefault();
        void resolve(reqId, true);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [focused, kbApproval, kbReadOnly, toolApprovals, approveAllReads, resolve]);

  // Keyboard on the active AskUserQuestion. Window-level + gated EXACTLY like the
  // tool-approval handler (focused pane; composer not the active non-empty typing
  // target) and keyed off kbQuestion (which is excluded from kbApproval), so the
  // two never double-fire on the same Enter/Esc.
  //   • Digits 1–9 → select the matching option (single-select: answer now;
  //     multi-select: toggle, reflected in the controlled card via questionSel).
  //   • Enter → confirm a complete multi-select / multi-question answer (sends).
  //   • Esc → skip (decline) the question.
  //   • A digit/Enter always yields to a non-empty composer draft so the user can
  //     still type a free-text message instead of answering.
  useEffect(() => {
    if (!focused || !kbQuestion?.requestId || !kbQuestions) return;
    const reqId = kbQuestion.requestId;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      if (e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;
      // Same drift guard as the approval handler: don't answer while the user's
      // caret is in another editable (focusedPaneId may have drifted here).
      if (isTypingElsewhere(taRef.current)) return;
      const ae = document.activeElement as HTMLElement | null;
      const composerBusy =
        ae?.tagName === 'TEXTAREA' && (ae as HTMLTextAreaElement).value.trim() !== '';
      // The keyboard answers ONE question at a time: the first not-yet-answered
      // question (so a multi-question card advances as each gets a pick).
      const qi = Math.max(
        0,
        questionSel.findIndex((s) => s.length === 0),
      );
      const action = applyQuestionKey(e.key, kbQuestions, questionSel, qi);
      if (action.kind === 'none') return;
      // Esc always skips (even with a draft — it's the universal "decline"); a
      // digit/Enter yields to a non-empty composer draft.
      if (action.kind !== 'skip' && composerBusy) return;
      e.preventDefault();
      if (action.kind === 'skip') {
        void resolve(reqId, false, 'The user declined to answer the question.');
      } else if (action.kind === 'update') {
        setQuestionSel(action.sel);
      } else if (action.kind === 'answer') {
        void resolve(reqId, false, buildAnswerReason(kbQuestions, action.sel));
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [focused, kbQuestion, kbQuestions, questionSel, resolve]);

  // Esc → Stop/interrupt the turn. Routed by REAL DOM focus, exactly like Term
  // routes a keystroke through the pane whose terminal holds focus — NOT by the
  // global focusedPaneId. This fixes the cross-pane bug: background activity (an
  // MCP open / ARIA spawn) can silently move focusedPaneId to another pane while
  // your caret stays here, and the old `focused` gate then sent Esc to the WRONG
  // pane. Every warm-mounted pane attaches the listener; the ownership test below
  // guarantees exactly one acts. Skips when an approval/question owns Esc
  // (deny/skip) or the slash menu / cheatsheet is open (the composer's own
  // keydown preventDefaults Esc there, so e.defaultPrevented short-circuits us).
  useEffect(() => {
    if (pendingApproval) return; // this pane's approval/question owns Esc
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      if (e.key !== 'Escape' || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;
      // Ownership, DOM-first:
      //   • our composer holds the caret → we own Esc;
      //   • some OTHER editable holds it → that element/pane owns it, not us;
      //   • nothing editable is focused  → fall back to the SELECTED pane.
      const ta = taRef.current;
      const owns =
        ta && document.activeElement === ta ? true : isTypingElsewhere(ta) ? false : focused;
      if (!owns) return;
      if (!inFlight) return; // nothing to stop
      e.preventDefault();
      void stop();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focused, pendingApproval, inFlight]);

  // Coarse pointers (touch): Enter should newline, the Send button sends. Desktop
  // keeps Enter-to-send / Shift+Enter-newline.
  const coarsePointer =
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(pointer: coarse)').matches;

  return (
    <div
      className={`pane native-pane ${focused ? 'focused' : ''} ${beingDragged ? 'dragging' : ''}`}
      style={nativeTypeScaleStyle}
      data-pane-id={pane.id}
      // POINTERdown, not mousedown: the composer's voice gestures cancel
      // pointerdown to tell a tap from a hold (useComposerVoice's
      // onPointerDown/onTouchStart), and a cancelled pointerdown suppresses the
      // compatibility mousedown ENTIRELY. So a click on the empty text bar never
      // ran a mousedown-based activation — the caret moved (onUp re-focuses by
      // hand) while store.focusedPaneId silently stayed on the previous pane, and
      // Alt+Z zoomed THAT one. A cancelled pointerdown still fires, and capture
      // runs before the textarea's own handler, so this can't be suppressed.
      onPointerDownCapture={() => setFocus(pane.id)}
      onContextMenu={openContextMenu}
      onDragOver={(e) => {
        if (!e.dataTransfer.types.includes(DRAG_MIME)) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        setDropZone(computeZone(e));
      }}
      onDragLeave={(e) => {
        const related = e.relatedTarget as Node | null;
        if (!e.currentTarget.contains(related)) setDropZone(null);
      }}
      onDrop={(e) => {
        if (!e.dataTransfer.types.includes(DRAG_MIME)) return;
        e.preventDefault();
        const src = e.dataTransfer.getData(DRAG_MIME);
        const zone = computeZone(e);
        setDropZone(null);
        if (src && src !== pane.id) movePane(src, pane.id, zone);
      }}
    >
      <div
        className="pane-header native-header"
        draggable
        onContextMenu={openContextMenu}
        onDoubleClick={(e) => {
          if (!canZoom) return;
          e.stopPropagation();
          toggleZoom(pane.id);
        }}
        onDragStart={(e) => {
          e.dataTransfer.setData(DRAG_MIME, pane.id);
          e.dataTransfer.effectAllowed = 'move';
          setBeingDragged(true);
          setDraggingPane(true);
        }}
        onDragEnd={() => {
          setBeingDragged(false);
          setDraggingPane(false);
        }}
      >
        <span
          className={`activity-dot ${
            !connected
              ? 'reconnecting'
              : pendingApproval
                ? pendingApproval.toolName === 'AskUserQuestion'
                  ? 'awaiting-input'
                  : 'choice-prompt'
                : working
                  ? 'busy'
                  : 'idle'
          }`}
          title={
            !connected
              ? 'reconnecting…'
              : pendingApproval
                ? 'waiting for your input'
                : working
                  ? `${paneAgentLabel} is working…`
                  : 'Focus renderer connected'
          }
        />
        <PaneHeaderBadges pane={pane} paneIndex={paneIndex} />
        {editingTitle ? (
          <input
            className="pane-title-input"
            autoFocus
            value={titleDraft}
            onChange={(e) => setTitleDraft(e.target.value)}
            onBlur={async () => {
              setEditingTitle(false);
              const next = titleDraft.trim();
              if (next !== pane.title) {
                try { await renamePane(pane.id, next); } catch {}
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
          <span
            className="pane-title"
            title="Double-click to rename"
            onDoubleClick={(e) => {
              e.stopPropagation();
              setTitleDraft(pane.title);
              setEditingTitle(true);
            }}
          >
            {pane.title || pane.cmd.join(' ') || paneAgentLabel}
          </span>
        )}
        {/* Same agent-work markers as the terminal header (PaneHeader). */}
        <PaneIdentityChip pane={pane} />
        <PaneOriginButton pane={pane} />
        <PaneReviewChip pane={pane} />
        <PaneMessageChip pane={pane} />
        {/* model · cwd moved OUT of this header into the bottom NativeMetaBar
            vitals row — the header is now identity + actions only. */}
        <div className="pane-actions">
          <div className="pane-mode-toggle-wrap">
            <PaneModeToggle
              isNative
              visual={toggleVisualState}
              pending={paneePendingSwitch}
              onSeg={requestModeSwitch}
              onCancelPending={cancelPendingSwitch}
              onSwitchNow={switchNowFromPending}
              termTitle="Terminal renderer (xterm)"
              nativeTitle="Focus conversation renderer"
              idleTitle="Switch between the terminal and Focus renderer"
              agentLabel={paneAgentLabel}
            />
            {confirmTarget !== null && (
              <ModeSwitchConfirm
                target={confirmTarget}
                working={stillWorking}
                agentLabel={paneAgentLabel}
                onConfirm={(choice) => {
                  const to = confirmTarget;
                  setConfirmTarget(null);
                  if (!stillWorking) {
                    void switchPaneMode(pane.id, to);
                    return;
                  }
                  void switchPaneMode(pane.id, to, switchRequestForChoice(choice, to));
                }}
                onCancel={() => setConfirmTarget(null)}
              />
            )}
          </div>
          {/* Memory chip: the agent's resident RAM (sampled ~60s). Tap to restart
              the agent and reclaim it (conversation kept). Goes amber when heavy.
              Hidden until the first sample (agentRssMB 0/absent). */}
          <AgentMemoryChip pane={pane} onReclaim={() => { void refreshPane(pane.id); }} />
          {/* Maximise/restore — header parity with the Terminal pane
              (PaneHeader.tsx). Same classes/glyphs/titles so it looks and behaves
              identically; positioned AFTER the mode toggle and BEFORE the ⋯ menu to
              match the Terminal action order. canZoom mirrors the terminal gate
              (only meaningful with >1 pane, or while already zoomed). The
              @container collapse rule already hides .pane-zoom-btn on very narrow
              panes for both headers. */}
          {canZoom && (
            <button
              className="iconbtn pane-zoom-btn"
              title={isZoomed ? 'Restore layout (Alt+Z)' : 'Maximise pane (Alt+Z)'}
              aria-label={isZoomed ? 'Restore pane' : 'Maximise pane'}
              onClick={(e) => {
                e.stopPropagation();
                toggleZoom(pane.id);
              }}
              onDoubleClick={(e) => e.stopPropagation()}
            >{isZoomed ? <IconMinimize size={13} /> : <IconMaximize size={13} />}</button>
          )}
          <button
            className="iconbtn pane-menu-btn"
            title="Pane menu"
            aria-label="Pane menu"
            aria-haspopup="menu"
            onClick={(e) => {
              e.stopPropagation();
              openContextMenu(e);
            }}
            onDoubleClick={(e) => e.stopPropagation()}
          >⋯</button>
          <button
            className="iconbtn"
            title="Close session"
            onClick={async (e) => {
              e.stopPropagation();
              if (await confirmClosePane(pane)) void killPane(pane.id);
            }}
          >
            ×
          </button>
        </div>
      </div>
      {/* Flag-memo + last-prompt rows, mirroring terminal mode's chrome strip.
          Rendered as plain in-flow children (NOT inside terminal mode's
          .pane-chrome-rows wrapper) — that wrapper's mobile height:0 overlay is
          an xterm-only trick to dodge a ResizeObserver refit flash; the native
          transcript is a flex scroll body that wants these rows in normal flow. */}
      <PaneFlagMemo
        pane={pane}
        editing={editingFlagMemo}
        draft={flagMemoDraft}
        setEditing={setEditingFlagMemo}
        setDraft={setFlagMemoDraft}
      />
      <PaneLastPrompt
        prompts={lastPromptsNewestFirst}
        expanded={lastPromptExpanded}
        setExpanded={setLastPromptExpanded}
      />
      {ctxMenu && (
        <PaneContextMenu
          pane={pane}
          x={ctxMenu.x}
          y={ctxMenu.y}
          menuRef={ctxMenuRef}
          isClaudePane={paneAgent === 'claude'}
          isNative
          onSplitRight={ctxMenuActions.splitRight}
          onSplitRightShellHere={() => {
            closeContextMenu();
            void launchShellHere(pane.id, 'horizontal');
          }}
          onSplitRightClaudeHere={() => {
            closeContextMenu();
            void launchClaudeHere(pane.id, 'horizontal');
          }}
          onSplitDown={ctxMenuActions.splitDown}
          onClonePane={ctxMenuActions.clonePane}
          onMoveToWorkspace={pane.role !== 'task_owner' ? ctxMenuActions.moveToWorkspace : undefined}
          onNewSession={ctxMenuActions.newSession}
          onCreateSchedule={ctxMenuActions.createSchedule}
          onSetAutoMode={ctxMenuActions.setAutoMode}
          onToggleAnnounceDelegation={(paneAgent === 'claude' || paneAgent === 'codex') && operatorFeaturesAllowed() ? ctxMenuActions.toggleAnnounceDelegation : undefined}
          onEditFlagMemo={ctxMenuActions.editFlagMemo}
          onUnflag={ctxMenuActions.unflag}
          onFlagForAttention={ctxMenuActions.flagForAttention}
          onRename={ctxMenuActions.rename}
          onEditIdentity={() => {
            closeContextMenu();
            setIdentityDialogOpen(true);
          }}
          onConvertRole={operatorFeaturesAllowed() && roleConvertAllowed(pane, useStore.getState().orchestratorHosts) ? ctxMenuActions.convertRole : undefined}
          onChangeContextPack={() => {
            closeContextMenu();
            setContextPackDialogOpen(true);
          }}
          onDetachContextPack={() => {
            closeContextMenu();
            void setPaneContextPack(pane.id, '');
          }}
          onRestartPane={() => {
            closeContextMenu();
            void restartPaneFlow(pane.id);
          }}
          onResumePane={() => {
            closeContextMenu();
            // store.resumePane toasts a failure and restores the pane.
            resumePane(pane.id).catch(() => {});
          }}
          onSetEffort={paneAgent === 'claude' ? (effort) => {
            closeContextMenu();
            void setPaneEffort(pane.id, effort);
          } : undefined}
          onClosePane={ctxMenuActions.closePane}
        />
      )}
      {identityDialogOpen && (
        <PaneIdentityDialog pane={pane} onClose={() => setIdentityDialogOpen(false)} />
      )}
      {contextPackDialogOpen && (
        <ContextPackDialog
          title="Context pack — pane"
          value={pane.contextPack ?? ''}
          onSave={async (dir) => { await setPaneContextPack(pane.id, dir); }}
          onClose={() => setContextPackDialogOpen(false)}
        />
      )}

      {sessionInit?.permissionMode === 'auto' && (
        <div className="native-warn-banner">
          Permission mode is <strong>auto</strong> — approval prompts won't appear; tools run
          automatically.
        </div>
      )}

      {/* Relative frame for the floating scroll-to-bottom pill + drop overlay.
          They must be SIBLINGS of the scroller, not children: an absolutely
          positioned element inside an overflow:auto box scrolls WITH the content
          (it drifts off-screen) instead of pinning to the visible transcript —
          which is why the pill never appeared when scrolled up. */}
      <div className="native-body-wrap">
      {/* In-pane find overlay — same bar + keys (Enter next, Shift+Enter prev,
          Esc close) as the terminal renderer, anchored over the transcript body
          (the wrap is position:relative). */}
      {find.open && (
        <PaneFindBar
          inputRef={find.inputRef}
          query={find.query}
          setQuery={find.setQuery}
          results={find.results}
          onNext={find.next}
          onPrev={find.prev}
          onClose={find.close}
        />
      )}
      {/* Renderer-switch scrim: while the daemon kills+respawns the pane
          (paneModeSwitching), the freshly-mounted Focus body would otherwise
          sit blank. A labeled veil turns that dead beat into an intentional
          transition; it lifts the moment the switch resolves and the normal
          "Restoring conversation…" hydration state takes over. */}
      {switching && (
        <div className="pane-switching-scrim" role="status" aria-live="polite">
          <span className="pane-switching-spin" aria-hidden />
          <span className="pane-switching-label">
            Switching to Focus — restoring your conversation…
          </span>
        </div>
      )}
      <div
        className="pane-body native-body"
        ref={bodyRef}
        onScroll={onScroll}
        onWheel={markScrollIntent}
        onTouchMove={markScrollIntent}
        onPointerDown={(e) => { if (e.target === e.currentTarget) markScrollIntent(); }}
        onClick={onBodyClick}
        onDragOver={onBodyDragOver}
        onDrop={onBodyDrop}
      >
        {visibleRows.length === 0 && pending.length === 0 && !working && (
          <div className="native-empty">
            {sessionInit?.model && <div className="native-empty-model">{sessionInit.model}</div>}
            {(sessionInit?.cwd || pane.liveCwd || pane.cwd) && (
              <div className="native-empty-cwd">
                {shortCwd(sessionInit?.cwd || pane.liveCwd || pane.cwd)}
              </div>
            )}
            {/* Un-hydrated + a linked session = a Term→Focus switch (or respawn)
                is still restoring the prior conversation — saying "message
                Claude to start…" here misreads as a lost session. A genuinely
                fresh pane hydrates in one round-trip, so the restoring line is
                only ever visible when there is actually something to restore. */}
            <div className="native-empty-hint">
              {!hydrated && (paneAgent === 'codex' ? pane.codexThreadId : pane.claudeSessionId) ? (
                <span className="native-empty-restoring">Restoring conversation…</span>
              ) : hiddenCount > 0 ? (
                realClear
                  ? `Context cleared — message ${paneAgentLabel} to start fresh…`
                  : `View cleared — message ${paneAgentLabel} to continue…`
              ) : (
                `Message ${paneAgentLabel} to start…`
              )}
            </div>
          </div>
        )}
        {hiddenCount > 0 && (
          <div className="native-cleared-notice">
            {realClear ? (
              <span>✓ Context cleared — {paneAgentLabel} is starting fresh.</span>
            ) : (
              <>
                <span>Transcript view cleared ({hiddenCount} hidden) — the session is intact.</span>
                <button type="button" className="native-cleared-undo" onClick={undoClear}>
                  Show again
                </button>
              </>
            )}
          </div>
        )}
        {/* role="log": the standard live-chat pattern — screen readers announce
            ADDITIONS (implicit aria-live=polite, aria-relevant=additions), not
            the whole transcript on every render. */}
        <NativeFindContext.Provider value={findCtx}>
        <div
          className="native-transcript"
          data-display-mode="focus"
          role="log"
          aria-label="Conversation transcript"
          ref={transcriptRef}
        >
          {groupedRows.map((row) => {
            if (row.kind === 'turn-sep') {
              // A user-turn boundary: subtle divider + relative time. Replayed
              // (history) turns carry no client time, so they read "earlier".
              return (
                <TurnSeparator
                  key={row.key}
                  label={row.fromReplay ? 'earlier' : relativeTime(row.clientTs, nowTick)}
                />
              );
            }
            // Focus-mode terse groups (groupRows): a folded run of tool calls or
            // thinking blocks. Each reuses the per-item views internally, so
            // expanding loses nothing.
            if (row.kind === 'tool-group') {
              return (
                <ToolGroupView
                  key={row.key}
                  events={row.events}
                  resultByCallId={resultByCallId}
                />
              );
            }
            if (row.kind === 'step-group') {
              // An interleaved thinking + tool-call run (the think→tool ladder),
              // folded into one group with the reasoning one click away.
              return (
                <StepGroupView
                  key={row.key}
                  events={row.events}
                  resultByCallId={resultByCallId}
                />
              );
            }
            if (row.kind === 'edit-group') {
              // A run of ≥2 consecutive file edits, consolidated into one collapsed,
              // expandable "✎ N edits" group (a lone edit never reaches here).
              return (
                <EditGroupView
                  key={row.key}
                  events={row.events}
                  resultByCallId={resultByCallId}
                />
              );
            }
            if (row.kind === 'thinking-group') {
              return <ThinkingGroupView key={row.key} events={row.events} />;
            }
            const e = row.event;
            switch (e.type) {
              case 'text':
              case 'text_delta':
                return <TextView key={e.seq} ev={e} />;
              case 'slash_result':
                return <SlashResultView key={e.seq} ev={e} />;
              case 'user_text':
                return (
                  <UserMessageView key={e.seq} ev={e} />
                );
              case 'thinking':
              case 'thinking_delta':
                return <ThinkingView key={e.seq} ev={e} isStreaming={e.streaming} />;
              case 'tool_call': {
                // While an AskUserQuestion awaits the user, the interactive
                // approval card below renders the question + option buttons;
                // suppress the duplicate tool_call card. Once answered the
                // approval clears and this card returns with the answer folded in.
                if (
                  (e.name || e.toolName) === 'AskUserQuestion' &&
                  e.callId &&
                  pendingApprovalCallIds.has(e.callId)
                ) {
                  return null;
                }
                // A Task/Agent that spawned a sub-agent renders its captured nested
                // tool calls indented beneath it (TaskView). Degrades to a plain
                // ToolCallView when no children were captured.
                const childCalls = e.callId ? childrenByParent.get(e.callId) : undefined;
                if (childCalls && childCalls.length > 0) {
                  return (
                    <TaskView
                      key={e.seq}
                      ev={e}
                      result={e.callId ? resultByCallId.get(e.callId) : undefined}
                      childCalls={childCalls}
                      resultByCallId={resultByCallId}
                    />
                  );
                }
                return (
                  <ToolCallView
                    key={e.seq}
                    ev={e}
                    result={e.callId ? resultByCallId.get(e.callId) : undefined}
                  />
                );
              }
              case 'todo':
                return <TodoView key={e.seq} ev={e} />;
              case 'permission_request':
                // Render an approval card only for the live pending request;
                // resolved/superseded requests collapse to nothing. The wrapper
                // div holds a seq-keyed ref so we can detect when this card is
                // scrolled off-screen (→ docked bar) and scroll it back into view.
                return pendingApprovalSeqs.has(e.seq) ? (
                  <div
                    key={e.seq}
                    ref={(el) => {
                      if (el) approvalCardRefs.current.set(e.seq, el);
                      else approvalCardRefs.current.delete(e.seq);
                    }}
                  >
                    <ApprovalView
                      ev={e}
                      onResolve={resolve}
                      onAlwaysAllow={
                        paneAgent !== 'codex' && e.toolName && e.toolName !== 'AskUserQuestion'
                          ? addToAllowlist
                          : undefined
                      }
                      alreadyAllowed={!!e.toolName && allowlist.includes(e.toolName)}
                      allowlist={allowlist}
                      keyHint={kbApproval?.seq === e.seq}
                      batch={
                        toolApprovals[0]?.seq === e.seq && toolApprovals.length >= 2
                          ? {
                              count: toolApprovals.length,
                              readCount: toolApprovals.filter(isReadLikeApproval).length,
                              onApproveAllReads: approveAllReads,
                            }
                          : undefined
                      }
                      // Wire the CONTROLLED question selection to the keyboard-active
                      // AskUserQuestion (matched by seq) so digits 1–9 / Enter and the
                      // on-card clicks share one source of truth.
                      questionSel={kbQuestion?.seq === e.seq ? questionSel : undefined}
                      onQuestionSelChange={kbQuestion?.seq === e.seq ? setQuestionSel : undefined}
                    />
                  </div>
                ) : null;
              case 'error':
                return (
                  <ErrorView
                    key={e.seq}
                    ev={e}
                    onRetry={lastUserPrompt ? () => resendPrompt(lastUserPrompt) : undefined}
                  />
                );
              default:
                return null;
            }
          })}
          {/* Optimistic user rows: rendered from the pending list (kept OUT of
              the seq cache). Keyed by localMsgId, never a seq. Each disappears the
              instant its user_text replay echo reconciles it in the hook. */}
          {pending.map((p) => (
            <UserMessageView
              key={`pending-${p.localMsgId}`}
              ev={{ type: 'user_text', text: p.text, seq: -1, paneId: pane.id, fromReplay: false }}
              optimistic
              failed={p.failed}
              onRetry={p.failed ? () => void retryPending(p.localMsgId) : undefined}
            />
          ))}
          {/* The "Claude is working" indicator no longer lives in the scroll
              flow — it's the dedicated, always-visible metabar docked under the
              composer (so it's seen even when scrolled up, and never shifts the
              transcript as it toggles). */}
          {sessionEnded && <SessionEndedCard exitCode={pane.exitCode} resumable={pane.resumable} restartPaused={pane.restartPaused} agentLabel={paneAgentLabel} />}
        </div>
        </NativeFindContext.Provider>
      </div>
        <ScrollToLatestButton
          visible={!atBottom}
          count={newCount}
          onClick={() => {
            const el = bodyRef.current;
            if (el) {
              el.scrollTop = el.scrollHeight;
              stickRef.current = true;
              setAtBottom(true);
              setNewCount(0);
            }
          }}
        />
        {dropZone && !beingDragged && (
          <div className={`drop-zone-overlay zone-${dropZone}`} aria-hidden>
            <span className="drop-zone-hint">
              {dropZone === 'center' ? 'swap' : `insert ${dropZone}`}
            </span>
          </div>
        )}
      </div>

      {/* Auto-approve visibility strip: surfaces which tools are currently on the
          pane's "always allow" list so the auto-approval is never invisible, and
          lets the user revoke any of them inline. Renders nothing when empty. */}
      {paneAgent !== 'codex' && (
        <AllowlistStrip tools={allowlist} onRemove={(tool) => void removeFromAllowlist(tool)} />
      )}

      {/* Docked approval bar: surfaces a pending tool approval that's scrolled
          out of view, just above the composer, so a fired approval is never
          missed. Hidden when the active card is visible (no duplication). Also
          carries the batch affordances when ≥2 approvals are pending. */}
      {dockedTarget && (
        <DockedApprovalBar
          target={dockedTarget}
          pendingApprovals={toolApprovals}
          onResolve={resolve}
          onJump={jumpToApproval}
          onApproveAllReads={approveAllReads}
          onAlwaysAllow={
            paneAgent !== 'codex' && dockedTarget.toolName && dockedTarget.toolName !== 'AskUserQuestion'
              ? addToAllowlist
              : undefined
          }
          alreadyAllowed={!!dockedTarget.toolName && allowlist.includes(dockedTarget.toolName)}
          allowlist={allowlist}
        />
      )}

      {sessionEnded ? (
        // The agent process has exited — no more input is possible. Replace the
        // composer with a clear, dignified affordance (mirrors the terminal
        // pane's "Session closed" handling) instead of a dead text box.
        <div className="native-composer native-composer-ended" aria-disabled>
          <span className="native-composer-ended-text">
            {resumeFailed
              ? "Couldn't resume — the session may have expired. Try again or close the pane."
              : restartPaused
                ? 'Paused after a restart — not resumed.'
                : pane.resumable
                  ? 'Session ended — resume to continue, or close the pane.'
                  : 'Session ended — this pane no longer accepts input.'}
          </span>
          {pane.resumable && !restartPaused && (
            <button
              type="button"
              className="native-btn native-btn-send"
              title={`Resume this ${paneAgentLabel} conversation`}
              disabled={resuming}
              onClick={async (e) => {
                e.stopPropagation();
                setResumeFailed(false);
                setResuming(true);
                try {
                  await resumePane(pane.id);
                } catch {
                  setResumeFailed(true);
                } finally {
                  setResuming(false);
                }
              }}
            >
              {resuming ? 'Resuming…' : 'Resume session'}
            </button>
          )}
          <button
            type="button"
            className="native-btn"
            title="Close this pane"
            onClick={async (e) => {
              e.stopPropagation();
              if (await confirmClosePane(pane)) void killPane(pane.id);
            }}
          >
            Close pane
          </button>
        </div>
      ) : (
        <div className="native-composer-wrap">
          {/* Floating layers above the composer: the interrupted marker, the
              /help cheatsheet, the slash menu, the queued-steer chip — each
              rendered just above the input. */}
          {showInterrupted && pane.switchNotice && (
            <SwitchInterruptedBanner
              fromKind={pane.switchNotice.fromKind}
              reason={pane.switchNotice.reason}
              onContinue={continueAfterInterrupt}
              onDismiss={dismissNotice}
            />
          )}
          {showCheatsheet && (
            <Cheatsheet shortcuts={KEYBOARD_SHORTCUTS} onClose={() => setShowCheatsheet(false)} />
          )}
          {slashOpen && !showCheatsheet && (
            <SlashMenu
              commands={slashCommands}
              index={slashIndex}
              onPick={completeSlashCommand}
              onHover={setSlashIndex}
            />
          )}
          {atOpen && (
            <AtMentionMenu
              hits={atHits}
              index={atIndex}
              onPick={pickAtMention}
              onHover={setAtIndex}
            />
          )}
          {paneAgent === 'claude' && modelPickerOpen && (
            <ModelMenu
              options={MODEL_OPTIONS}
              currentId={modelIdForAgent(pendingModel ?? pane.model ?? sessionInit?.model, paneAgent)}
              index={modelIndex}
              onPick={pickModel}
              onHover={setModelIndex}
            />
          )}
          {queued.length > 0 && (
            <div className="native-queued-list">
              {queued.map((q, i) => (
                <div key={i} className="native-queued-chip" role="status">
                  <span className="native-queued-glyph" aria-hidden>
                    ⏎
                  </span>
                  <span className="native-queued-label" title={q}>
                    queued{queued.length > 1 ? ` ${i + 1}/${queued.length}` : ''}: {q}
                  </span>
                  <button
                    type="button"
                    className="native-queued-cancel"
                    aria-label="Cancel queued message"
                    title="Cancel this queued follow-up"
                    onClick={() => {
                      // Drop just this one (preserving FIFO order of the rest).
                      // Restore its text into the composer when empty so it's
                      // editable, not silently lost.
                      if (q && !draft.trim()) setDraftValue(q);
                      setQueued((cur) => cur.filter((_, idx) => idx !== i));
                    }}
                  >
                    ✕
                  </button>
                </div>
              ))}
            </div>
          )}

          {/* Voice-first composer (shared with MobileQuickReply). Empty box =
              hold-to-talk → dictates into the draft (auto-sends with 🚀, else
              review-then-send); type + ➤ sends; ⏹ interrupts while a turn is in
              flight. No PTY \r-split — sendOptimistic POSTs a single JSONL user
              turn. 📎 / paste / drop attach an image: upload → splice `@<path>`
              into the draft, which headless Claude reads inline (same as terminal
              panes). Our power-composer wiring rides on top: the keyboard handler
              still owns slash-menu nav,
              ↑/↓ history recall, 1–9 question keys, and Enter-send/steer; the
              floating layers (interrupted marker, cheatsheet, slash menu, queued
              chip) render above via the `banners` slot so they stay in this
              surface's DOM order. */}
          <Composer
            inputRef={taRef}
            draft={draft}
            placeholder={
              turnBusy
                ? `Steer ${pane.title || paneAgentLabel} (queues until the turn ends)…`
                : `Message ${pane.title || paneAgentLabel}…`
            }
            voiceHint={`Hold to talk · or message ${paneAgentLabel}…`}
            suggestion={ghostText}
            onCompositionStart={() => { composingRef.current = true; }}
            onCompositionEnd={() => { composingRef.current = false; }}
            onDraftChange={(v) => {
              setDraftValue(v);
              // Any edit exits history browsing (the text no longer matches a
              // recalled entry) and resets the slash highlight to the top.
              if (historyNav.index !== null) setHistoryNav(HISTORY_IDLE);
              setSlashIndex(0);
              // Track the caret for the @-mention token derivation (rAF: the
              // textarea's selection updates after the change event).
              requestAnimationFrame(() => {
                const el = taRef.current;
                if (el) setCaretPos(el.selectionStart ?? v.length);
              });
            }}
            onKeyDown={(e) => {
              // 0) Tab accepts the prompt-suggestion ghost (only when one is
              //    showing — ghostText already gates on empty/idle/setting-on —
              //    and NOT mid-IME-composition, and NOT while the slash menu or
              //    the @-mention picker is open (each owns its Tab below).
              if (e.key === 'Tab' && ghostText && !composingRef.current && !slashOpen && !atOpen) {
                e.preventDefault();
                consumeSuggestion();
                setDraftValue(ghostText);
                // Park the caret at the end so the user can keep editing.
                requestAnimationFrame(() => {
                  const el = taRef.current;
                  if (el) el.selectionStart = el.selectionEnd = el.value.length;
                });
                return;
              }
              // 1) Cheatsheet open → Esc closes it (and only that).
              if (showCheatsheet && e.key === 'Escape') {
                e.preventDefault();
                setShowCheatsheet(false);
                return;
              }
              // 2-pre) /model picker open → it owns ↑/↓/Enter/Esc. Checked
              // before the slash menu (the draft is empty while it's open, so
              // slashOpen is false anyway — this is belt-and-braces ordering).
              if (modelPickerOpen) {
                if (e.key === 'ArrowDown') {
                  e.preventDefault();
                  setModelIndex((i) => stepSlashSelection(i, MODEL_OPTIONS.length, 1));
                  return;
                }
                if (e.key === 'ArrowUp') {
                  e.preventDefault();
                  setModelIndex((i) => stepSlashSelection(i, MODEL_OPTIONS.length, -1));
                  return;
                }
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  const opt = MODEL_OPTIONS[modelIndex] ?? MODEL_OPTIONS[0];
                  if (opt) pickModel(opt);
                  return;
                }
                if (e.key === 'Escape') {
                  e.preventDefault();
                  setModelPickerOpen(false);
                  return;
                }
              }
              // 2) Slash menu open → it owns ↑/↓/Enter/Esc.
              if (slashOpen) {
                if (e.key === 'ArrowDown') {
                  e.preventDefault();
                  setSlashIndex((i) => stepSlashSelection(i, slashCommands.length, 1));
                  return;
                }
                if (e.key === 'ArrowUp') {
                  e.preventDefault();
                  setSlashIndex((i) => stepSlashSelection(i, slashCommands.length, -1));
                  return;
                }
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  const cmd = slashCommands[slashIndex] ?? slashCommands[0];
                  if (cmd) runSlashCommand(cmd);
                  return;
                }
                // Tab autocompletes the highlighted command into the composer
                // ("/cmd ") so the user can add arguments, then Enter sends.
                if (e.key === 'Tab') {
                  e.preventDefault();
                  const cmd = slashCommands[slashIndex] ?? slashCommands[0];
                  if (cmd) completeSlashCommand(cmd);
                  return;
                }
                if (e.key === 'Escape') {
                  e.preventDefault();
                  setDraftValue(''); // close the menu by clearing the "/" token
                  return;
                }
                // Other keys (typing to filter) fall through to onChange.
              }
              // 2b) @-mention picker open → it owns ↑/↓/Tab/Enter/Esc. Mutually
              //     exclusive with the slash menu (that one requires the WHOLE
              //     draft to be a "/token", which can't also be an @-token).
              if (atOpen) {
                if (e.key === 'ArrowDown') {
                  e.preventDefault();
                  setAtIndex((i) => stepSlashSelection(i, atHits.length, 1));
                  return;
                }
                if (e.key === 'ArrowUp') {
                  e.preventDefault();
                  setAtIndex((i) => stepSlashSelection(i, atHits.length, -1));
                  return;
                }
                if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
                  e.preventDefault();
                  const hit = atHits[atIndex] ?? atHits[0];
                  if (hit) pickAtMention(hit);
                  return;
                }
                if (e.key === 'Escape') {
                  e.preventDefault();
                  // Remember THIS token as dismissed (keyed by its "@" index);
                  // typing more of it keeps the menu closed, a new token reopens.
                  setAtDismissedStart(atMention?.start ?? null);
                  return;
                }
                // Other keys (typing to filter) fall through to onChange.
              }
              // 3) Prompt history: ↑/↓ recall, only at the very start of the
              //    composer (caret at 0) or while already browsing, so ↑/↓
              //    inside a multi-line draft still moves the caret normally.
              const ta = e.currentTarget;
              const atStart = ta.selectionStart === 0 && ta.selectionEnd === 0;
              const browsing = historyNav.index !== null;
              if (e.key === 'ArrowUp' && !e.shiftKey && (atStart || browsing)) {
                const res = recallOlder(historyNav, promptHistory, draft);
                if (res) {
                  e.preventDefault();
                  setHistoryNav(res.nav);
                  setDraftValue(res.text);
                  // Put the caret at the end of the recalled text next tick.
                  requestAnimationFrame(() => {
                    const el = taRef.current;
                    if (el) el.selectionStart = el.selectionEnd = el.value.length;
                  });
                  return;
                }
              }
              if (e.key === 'ArrowDown' && !e.shiftKey && browsing) {
                const res = recallNewer(historyNav, promptHistory);
                if (res) {
                  e.preventDefault();
                  setHistoryNav(res.nav);
                  setDraftValue(res.text);
                  return;
                }
              }
              // 4) Enter sends; Shift+Enter inserts a newline. No PTY \r-split —
              //    the daemon reads a JSONL user turn, so a single atomic POST is
              //    correct. On coarse (touch) pointers DON'T intercept Enter — let
              //    it newline; the visible ➤ button is the send affordance.
              if (e.key === 'Enter' && !e.shiftKey && !coarsePointer) {
                e.preventDefault();
                submit();
              }
            }}
            onSend={submit}
            onPaste={onComposerPaste}
            onPickFile={(f) => void attachImage(f)}
            sendLabel={turnBusy ? 'Queue' : 'Send'}
            sendDisabled={draft.trim() === ''}
            inFlight={inFlight}
            onStop={() => void stop()}
            stopping={interrupting || (!working && !hasApproval)}
            voice={{
              onSubmitText: submit,
              onError: (m, k) => useStore.getState().toast(m, { kind: k }),
              shortcutActive: focused,
              shortcutPaneId: pane.id,
            }}
          />

          {/* Dedicated status + usage line, docked under the composer. Left: the
              always-present "Claude is working… / ✓ worked for Xs" status. Right:
              compact session usage. Replaces the old fat usage-footer AND the
              verbose keyboard-hint row — one slim, reserved line. */}
          <NativeMetaBar
            phase={statusPhase}
            label={statusLabel}
            startedAt={turnStartedAt}
            doneDurationMs={doneDurationMs}
            usage={usage}
            rateLimit={rateLimit}
            fields={metaBarFields}
            model={modelLabel}
            contextPct={contextPct}
            contextMeasuring={contextCompacted}
            cost={effCost}
            cwd={metaCwd}
            cwdFull={metaCwdFull}
            idleBgShells={idleBgShells}
            idleBgAgents={idleBgAgents}
            workflow={pane.workflowStatus}
          />
        </div>
      )}
    </div>
  );
}
