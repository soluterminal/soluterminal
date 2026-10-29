
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useFocusEnabled } from '../lib/focusAvailability';
import { closedPaneHint, closedPaneLabel } from '../lib/closedPanes';
import { paneUnread } from '../lib/turnChip';
import { api } from '../api';
import { paneIdsInOrder, useStore } from '../store';
import { confirmDialog } from '../lib/dialog';
import { multiWindowAvailable, openWorkspaceWindow } from '../tauri-browser';
import { shouldConfirmModeSwitch } from '../native/modeSwitch';
import { autoModeMenuLabel, nextAutoMode } from '../lib/autoMode';
import { REBINDABLE_ACTIONS } from '../keybindings';
import type { PaneAutoMode } from '../types';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { roleConvertAllowed, slotList, type PaletteAction } from '../extensions';
import { operatorFeaturesAllowed, useEntitlements } from '../entitlements';
import { confirmConvertPaneRole } from './usePaneContextMenu';
import { restartPaneFlow } from '../lib/paneRestart';
import { useSettings } from '../settings';
import { useCloud } from '../cloud';
import { usePointerMoved } from './usePointerMoved';
import { AGENT_NAMES, startAgent, type StartableAgent } from '../agentStart';

// One-stop palette. Three modes share the same modal shell:
//
//   actions — fuzzy-pick any app action (new pane, split, kill, zoom,
//             toggle sidebar, switch workspace, …). Default for Ctrl+K.
//   files   — fuzzy-walk filenames/dirs under a chosen root (root chips,
//             dirs/files toggle, recent-dirs menu). Default for Ctrl+P.
//   search  — find text across every running pane's scrollback. Reached
//             via `@` prefix or the "Search across panes…" action.
//
// Mode switching without reopening:
//   actions → files:  `/` as first char. (`/` reads as a path sigil.)
//   actions → search: `@` as first char. (`@` = at/across panes.)
//   anywhere → actions: `>` as first char. Matches VS Code.
//   Backspace on an empty query in files/search → actions (back out).
//
// One door, two (then three) rooms: users only have one first guess for
// "I want to do something" — Ctrl+K is always the right reflex and the
// mode handles the rest.

type Mode = 'actions' | 'files' | 'search';
type Kind = 'files' | 'dirs' | 'all';

interface Props {
  open: boolean;
  // Seeds the initial mode when the palette opens. Ctrl+P sets 'files';
  // Ctrl+K (and the default) sets 'actions'. Changes to this prop while
  // the palette is already open are ignored — mode only resets on (re)open.
  initialMode?: Mode;
  // Seeds the query on (re)open — "Search selection in all panes" opens the
  // palette in search mode with the selected text already typed.
  initialQuery?: string;
  onClose: () => void;
  onOpenHelp: () => void;
  onOpenNewPane: (split: 'horizontal' | 'vertical' | null) => void;
  onOpenBroadcast: () => void;
  onOpenActivity: () => void;
  onOpenFeedback: () => void;
}

type Action = PaletteAction;

interface Hit { path: string; name: string; rel: string; isDir?: boolean }

// Flat search hit — one per match, so ↑↓ cycles through every match
// across every pane. The alternative (grouped by pane) feels nice
// visually but adds a mental hop when skimming with the keyboard.
interface SearchHit {
  paneId: string;
  paneLabel: string;
  line: number;
  text: string;
  start: number;
  end: number;
}

// Token subsequence score for actions: lower is better. Returns null if
// the query can't match at all.
function scoreAction(q: string, a: Action): number | null {
  if (!q) return 0;
  const query = q.toLowerCase();
  const label = a.label.toLowerCase();
  const category = a.category.toLowerCase();
  const keywords = (a.keywords || '').toLowerCase();
  const haystack = `${label} ${category} ${keywords}`;

  if (label.startsWith(query)) return 0;
  // A match at the start of any word ("set" → "Open Settings") ranks next.
  if (label.includes(` ${query}`)) return 0.5;
  const idxLabel = label.indexOf(query);
  if (idxLabel >= 0) return 1 + idxLabel * 0.01;
  const idxCat = category.indexOf(query);
  if (idxCat >= 0) return 10 + idxCat * 0.01;
  if (keywords.indexOf(query) >= 0) return 12;

  let hi = 0;
  for (let qi = 0; qi < query.length; qi++) {
    const ch = query[qi];
    if (ch === ' ') continue;
    const found = haystack.indexOf(ch, hi);
    if (found < 0) return null;
    hi = found + 1;
  }
  return 30 + (hi - query.length);
}

// Abbreviates /home/<user> → ~ to match the rest of the app.
function labelRoot(r: string): string {
  if (!r) return '';
  if (r === '/') return '/';
  return r.replace(/^\/home\/[^/]+/, '~');
}

// Palette actions that open team-only surfaces (see entitlements.ts).
const TEAM_ONLY_ACTIONS = new Set(['pane.role.orchestrator']);

export function CommandPalette({
  open,
  initialMode = 'actions',
  initialQuery = '',
  onClose,
  onOpenHelp,
  onOpenNewPane,
  onOpenBroadcast,
  onOpenActivity,
  onOpenFeedback,
}: Props) {

  // Subscribe only to primitives. The store churns every WS byte
  // (lastActivityMs); binding maps here would rebuild the action list
  // mid-typing and reset selection.
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const workspaces = useStore((s) => s.workspaces);
  const wsOrder = useStore((s) => s.wsOrder);
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  const recentDirs = useStore((s) => s.recentDirs);
  const pushRecentDir = useStore((s) => s.pushRecentDir);
  const openFileHere = useStore((s) => s.openFileHere);
  const setFilesAnchor = useStore((s) => s.setFilesAnchor);
  const toast = useStore((s) => s.toast);
  const focusedPaneCwd = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    return p?.liveCwd || p?.cwd || '~';
  });
  // Renderer-mode facts for the focused pane, as subscribed primitives so the
  // "Switch to Focus / Terminal" action's label + enablement stay live while
  // the palette is open (the action list itself only re-snapshots on open).
  // isFocusedNative drives the opposite-of-current label; capable gates the
  // entry to Claude panes (mirrors PaneHeader.isClaudeCapable).
  const focusEnabled = useFocusEnabled();
  // Live, so the reopen entries appear once the daemon list arrives.
  const recentlyClosedPanes = useStore((s) => s.recentlyClosedPanes);
  const isFocusedNative = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    return p?.kind === 'native';
  });
  const isFocusedAgentCapable = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    if (!p) return false;
    if (p.identityBlocked) return false;
    const agent = effectivePaneAgent(p);
    if (agent === 'codex') return p.kind === 'native' || !!p.codexThreadId || !!p.hasCodex;
    if (agent === 'muse') return p.kind === 'native' || !!p.museSessionId || !!p.hasMuse;
    return p.hasClaude === true || agent === 'claude';
  });
  const focusedAgentLabel = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    return agentLabel(p ? effectivePaneAgent(p) : null);
  });
  // Auto-approve capability is broader than the renderer toggle:
  // the daemon auto-answers Codex approval modals too, so the auto-approve
  // action shows for Codex panes as well (mirrors PaneHeader.isAutoApproveCapable).
  const isFocusedAutoApproveCapable = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    if (!p) return false;
    return (
      p.cmd[0] === 'claude' ||
      p.cmd[0]?.endsWith('/claude') === true ||
      p.hasClaude === true ||
      p.kind === 'native' ||
      p.hasCodex === true ||
      p.hasMuse === true ||
      p.hasPi === true ||
      p.hasDeepseek === true
    );
  });
  // Focused pane's auto-approve mode, subscribed so the palette action's
  // label stays live (mirrors isFocusedNative for the renderer toggle).
  const focusedAutoMode = useStore((s) => {
    const p = s.focusedPaneId ? s.panes[s.focusedPaneId] : undefined;
    return (p?.autoMode ?? '') as PaneAutoMode;
  });

  const [mode, setMode] = useState<Mode>(initialMode);
  const [query, setQuery] = useState('');
  const [selIdx, setSelIdx] = useState(0);
  // Hover selects a row only on real pointer movement, so rows filtering
  // under a resting mouse can't steal Enter from the best match.
  const pointer = usePointerMoved();

  // Files-mode state. Unused outside files mode but kept hook-stable so
  // switching doesn't remount the whole modal (would steal focus).
  const [root, setRoot] = useState('');
  const [kind, setKind] = useState<Kind>('files');
  const [fileHits, setFileHits] = useState<Hit[]>([]);
  const [loading, setLoading] = useState(false);
  const [capped, setCapped] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [rootMenuOpen, setRootMenuOpen] = useState(false);

  // Search-mode state.
  const [searchHits, setSearchHits] = useState<SearchHit[]>([]);
  const [searchLoading, setSearchLoading] = useState(false);
  const [searchErr, setSearchErr] = useState<string | null>(null);
  const [searchCapped, setSearchCapped] = useState(false);
  const [searchTotal, setSearchTotal] = useState(0);
  // Distinct seq from files-mode so a slow search response can't stomp
  // a fast files response if the user flips modes mid-request.
  const searchSeq = useRef(0);

  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const rootMenuRef = useRef<HTMLDivElement | null>(null);
  // Guard against a stale slow response overwriting a newer fast one.
  const reqSeq = useRef(0);

  // Reset per-open.
  useEffect(() => {
    if (!open) return;
    setMode(initialMode);
    setQuery(initialQuery);
    setSelIdx(0);
    pointer.reset();
    setFileHits([]);
    setCapped(false);
    setErr(null);
    setKind('files');
    setRootMenuOpen(false);
    setRoot(initialMode === 'files' ? focusedPaneCwd : '');
    void useStore.getState().refreshClosedPanes();
    setSearchHits([]);
    setSearchErr(null);
    setSearchCapped(false);
    setSearchTotal(0);
    requestAnimationFrame(() => inputRef.current?.focus());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, initialMode, initialQuery]);

  // Concurrently chase the live cwd on files-mode open.
  useEffect(() => {
    if (!open || mode !== 'files' || !focusedPaneId) return;
    let cancelled = false;
    api.paneLiveCwd(focusedPaneId).then((cwd) => {
      if (!cancelled && cwd) setRoot(cwd);
    }).catch(() => { /* keep seeded root */ });
    return () => { cancelled = true; };
  }, [open, mode, focusedPaneId]);

  // Close root menu on outside click / Escape.
  useEffect(() => {
    if (!rootMenuOpen) return;
    const onAway = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent) {
        if (e.key === 'Escape') { setRootMenuOpen(false); e.stopPropagation(); }
        return;
      }
      const target = e.target as Node | null;
      if (target && rootMenuRef.current?.contains(target)) return;
      setRootMenuOpen(false);
    };
    window.addEventListener('mousedown', onAway, { capture: true });
    window.addEventListener('keydown', onAway as EventListener, { capture: true });
    return () => {
      window.removeEventListener('mousedown', onAway, { capture: true });
      window.removeEventListener('keydown', onAway as EventListener, { capture: true });
    };
  }, [rootMenuOpen]);

  // --- Actions mode -------------------------------------------------

  const actions = useMemo<Action[]>(() => {
    const st = useStore.getState();
    const cloud = useCloud.getState();
    const signedIn = !!cloud.session;
    const canSignIn = cloud.mode === 'cloud' && cloud.accountOptional && !signedIn;
    // First in the list: the two agents, with the same check-first start as
    // the empty window (a missing agent opens its install card).
    const startAgentAction = (agent: StartableAgent, keywords: string): Action => ({
      id: `agent.start.${agent}`,
      label: `Start ${AGENT_NAMES[agent]}`,
      hint: `Open ${AGENT_NAMES[agent]} in a new pane`,
      category: 'Agents',
      keywords,
      run: () => {
        onClose();
        startAgent(agent).catch((e: unknown) => {
          useStore.getState().toast(`Could not start ${AGENT_NAMES[agent]}: ${(e as Error).message}`, {
            kind: 'error',
            icon: '!',
          });
        });
      },
    });
    const list: Action[] = [
      startAgentAction('claude', 'claude code anthropic agent new session open launch run'),
      startAgentAction('codex', 'codex openai chatgpt agent new session open launch run'),
      {
        id: 'broadcast',
        label: 'Broadcast prompt to multiple panes',
        hint: 'Send one prompt to N selected panes at once',
        category: 'Terminal',
        keywords: 'fanout multicast send all claude',
        shortcut: 'Alt+Shift+B',
        run: () => onOpenBroadcast(),
      },
      {
        id: 'settings.open',
        label: 'Open Settings',
        hint: 'Appearance, panes, devices and updates',
        category: 'Settings',
        keywords: 'preferences options config settings',
        run: () => { onClose(); useSettings.getState().openSettings(); },
      },
      {
        id: 'view.theme',
        label: st.appThemeLight ? 'Switch to dark theme' : 'Switch to light theme',
        hint: 'Toggle the app theme',
        category: 'View',
        keywords: 'toggle theme dark light mode appearance colour color',
        run: () => { onClose(); useStore.getState().setAppTheme(useStore.getState().appThemeLight ? 'dark' : 'light'); },
      },
      ...(canSignIn ? [{
        id: 'account.signIn',
        label: 'Sign in or create free account',
        hint: 'Adds phone access, push alerts, voice and smart approvals',
        category: 'Account',
        keywords: 'login log in sign up register account',
        run: () => { onClose(); useCloud.getState().requestSignIn(); },
      }] : []),
      ...(signedIn ? [{
        id: 'account.signOut',
        label: 'Sign out',
        hint: cloud.session?.user?.email ? `Signed in as ${cloud.session.user.email}` : 'Signed in',
        category: 'Account',
        keywords: 'log out logout sign out account',
        run: async () => {
          onClose();
          const ok = await confirmDialog({
            message: 'Sign out? Your panes keep running. Phone access, push alerts, voice and smart approvals stop until you sign in again.',
            confirmLabel: 'Sign out',
          });
          if (ok) void useCloud.getState().signOut();
        },
      }] : []),
      {
        id: 'view.realign',
        label: 'Refresh view',
        hint: 'Redraw panes if text looks jumbled, for example after an update or after using them on another device',
        category: 'View',
        keywords: 'realign garbled redraw repaint refresh fix text misaligned',
        shortcut: 'Alt+R',
        run: () => { onClose(); useStore.getState().realignAllPanes(); },
      },
      {
        id: 'pane.new',
        label: 'New session',
        hint: 'Open the New Pane dialog',
        category: 'Terminal',
        keywords: 'create start claude code',
        shortcut: 'Alt+N',
        run: () => onOpenNewPane(null),
      },
      {
        id: 'pane.split.right',
        label: 'Split right',
        hint: 'Open New Pane dialog, horizontal split',
        category: 'Terminal',
        keywords: 'horizontal new',
        shortcut: 'Alt+→',
        run: () => onOpenNewPane('horizontal'),
      },
      {
        id: 'pane.split.down',
        label: 'Split down',
        hint: 'Open New Pane dialog, vertical split',
        category: 'Terminal',
        keywords: 'vertical new',
        shortcut: 'Alt+↓',
        run: () => onOpenNewPane('vertical'),
      },
      {
        id: 'pane.clone',
        label: 'Clone focused pane',
        hint: 'Duplicate the focused pane to the right',
        category: 'Terminal',
        keywords: 'copy duplicate split',
        shortcut: 'Alt+Enter',
        run: () => void st.cloneFocused('horizontal'),
      },
      {
        id: 'pane.kill',
        label: 'Kill focused session',
        hint: 'Terminates the focused pane in the daemon',
        category: 'Terminal',
        keywords: 'close remove delete',
        shortcut: 'Alt+W',
        run: async () => {
          if (!focusedPaneId) return;
          if (await confirmDialog({ message: 'Kill focused session?', confirmLabel: 'Kill', danger: true })) void st.killPane(focusedPaneId);
        },
      },
      {
        id: 'nav.next',
        label: 'Focus next pane',
        category: 'Navigation',
        keywords: 'cycle switch',
        shortcut: 'Alt+]',
        run: () => {
          const order = paneIdsInOrder(st.layout);
          if (!order.length) return;
          const cur = focusedPaneId ? order.indexOf(focusedPaneId) : -1;
          const next = order[(cur + 1 + order.length) % order.length];
          st.setFocus(next);
        },
      },
      {
        id: 'nav.prev',
        label: 'Focus previous pane',
        category: 'Navigation',
        keywords: 'cycle switch',
        shortcut: 'Alt+[',
        run: () => {
          const order = paneIdsInOrder(st.layout);
          if (!order.length) return;
          const cur = focusedPaneId ? order.indexOf(focusedPaneId) : -1;
          const prev = order[(cur - 1 + order.length) % order.length];
          st.setFocus(prev);
        },
      },
      {
        id: 'nav.zoom',
        label: 'Toggle zoom on focused pane',
        category: 'Navigation',
        keywords: 'maximize fullscreen',
        shortcut: 'Alt+Z',
        run: () => st.toggleZoom(),
      },
      {
        id: 'view.sidebar',
        label: 'Toggle sidebar',
        category: 'View',
        keywords: 'collapse expand hide show',
        shortcut: 'Ctrl+Q',
        run: () => {
          window.dispatchEvent(new CustomEvent('solu:toggle-sidebar'));
        },
      },
      // Desktop-only (hidden in cloud launch mode — the Rust command
      // rejects it there). Same action as the Topbar window-controls
      // button; the palette entry makes it findable by name.
      ...(multiWindowAvailable() ? [{
        id: 'window.new',
        label: 'New window',
        hint: 'Open a second soluterminal window — drag it to another monitor',
        category: 'View',
        keywords: 'monitor multi second screen display workspace window clone',
        run: () => { void openWorkspaceWindow(); },
      }] : []),
      {
        id: 'view.files',
        label: 'Find file…',
        hint: 'Switch this palette into file-finder mode',
        category: 'View',
        keywords: 'quickopen fuzzy search path / filename',
        shortcut: 'Ctrl+P',
        run: () => {
          setMode('files');
          setQuery('');
          setSelIdx(0);
          setRoot(focusedPaneCwd);
        },
      },
      {
        id: 'pane.export',
        label: 'Export focused pane transcript',
        hint: 'Download the focused pane\'s scrollback as plain text',
        category: 'Terminal',
        keywords: 'save download share session history transcript',
        run: async () => {
          const fid = useStore.getState().focusedPaneId;
          if (!fid) {
            useStore.getState().toast('No focused pane to export', { kind: 'error', icon: '⚠' });
            return;
          }
          try {
            const { filename, text } = await api.exportPaneTranscript(fid);
            // Blob + click-anchor is the standard "save as" path. No
            // dependency on the browser's default-download prompt since
            // we bake the filename in. Works identically in Tauri's
            // webview because its IPC layer doesn't intercept <a download>.
            const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = filename;
            document.body.appendChild(a);
            a.click();
            a.remove();
            // Defer revoking so Firefox's out-of-process download can
            // still resolve the URL. 1s is plenty.
            setTimeout(() => URL.revokeObjectURL(url), 1000);
            useStore.getState().toast(`Exported ${filename}`, { kind: 'success', icon: '📄', ttlMs: 2200 });
          } catch (e) {
            useStore.getState().toast(`Export failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
          }
        },
      },
      {
        id: 'help.supportBundle',
        label: 'Create support bundle…',
        hint: 'Collect redacted local diagnostics and save them to your computer',
        category: 'Help',
        keywords: 'support diagnostics bundle logs troubleshoot report',
        run: () => {
          onClose();
          useStore.getState().setSupportBundleOpen(true);
        },
      },
      {
        id: 'view.findInPane',
        label: 'Find in focused pane…',
        hint: 'Open the find overlay for the currently focused pane',
        category: 'View',
        keywords: 'ctrl f find grep in pane terminal',
        shortcut: 'Ctrl+F',
        run: () => {
          const fid = useStore.getState().focusedPaneId;
          if (!fid) {
            useStore.getState().toast('Focus a pane first', { kind: 'warn' });
            return;
          }
          window.dispatchEvent(new CustomEvent('solu:pane-find-open', {
            detail: { paneId: fid },
          }));
        },
      },
      {
        id: 'view.search',
        label: 'Search across panes…',
        hint: 'Find text in any running pane\'s scrollback',
        category: 'View',
        keywords: 'grep find text output claude @',
        shortcut: '@',
        run: () => {
          switchMode('search');
        },
      },
      {
        id: 'view.activity',
        label: 'Show activity…',
        hint: 'Cross-pane feed of state changes, tool calls, schedule fires',
        category: 'View',
        keywords: 'activity feed dashboard log history orchestration timeline',
        run: () => {
          onClose();
          onOpenActivity();
        },
      },
      {
        id: 'view.system',
        label: 'System',
        hint: 'Computer and app health: memory, disk, running sessions and updates',
        category: 'View',
        keywords: 'system cockpit resources memory ram cpu disk status health ops daemon',
        shortcut: 'Alt+O',
        run: () => {
          onClose();
          useStore.getState().setSystemPageOpen(true);
        },
      },
      {
        id: 'view.help',
        label: 'Show keyboard shortcuts',
        hint: `View all shortcuts — ${REBINDABLE_ACTIONS.length} of them rebindable in place`,
        category: 'View',
        keywords: 'hotkey bindings ? keybindings bind remap rebind customize edit',
        shortcut: '?',
        run: () => {
          onClose();
          onOpenHelp();
        },
      },
    ];

    const rolePane = focusedPaneId ? st.panes[focusedPaneId] : undefined;
    if (rolePane && roleConvertAllowed(rolePane, st.orchestratorHosts)) {
      const toOrchestrator = rolePane.role !== 'orchestrator';
      list.push({
        id: 'pane.role.orchestrator',
        label: toOrchestrator ? 'Convert pane to orchestrator' : 'Remove orchestrator role',
        hint: 'Restarts the pane to inject/remove the chief-of-staff brief (conversation kept for Claude and Codex sessions)',
        category: 'Terminal',
        keywords: 'orchestrator role convert chief of staff brief delegate',
        run: async () => {
          onClose();
          if (await confirmConvertPaneRole(rolePane, toOrchestrator)) {
            void useStore.getState().setPaneRole(rolePane.id, toOrchestrator ? 'orchestrator' : '');
          }
        },
      });
    }

    // Actions registered by optional features (extensions.ts); each builder
    // decides for itself whether it has anything to offer right now.
    for (const build of slotList('paletteActions')) list.push(...build({ close: onClose }));

    // Renderer toggle for the focused pane. Only meaningful for Claude
    // panes (the native renderer speaks Claude's stream-json), so it's
    // gated on the same isClaudeCapable heuristic the pane header uses.
    // The label shows the OPPOSITE of the current mode so it reads as a
    // one-tap destination ("Switch to Focus" when you're on Terminal).
    // Focus retired: only a still-native pane keeps "Switch to Terminal".
    if (isFocusedAgentCapable && focusedPaneId && (focusEnabled || isFocusedNative)) {
      const targetKind: '' | 'native' = isFocusedNative ? '' : 'native';
      list.push({
        id: 'pane.switchMode',
        label: isFocusedNative
          ? 'Switch focused pane to Terminal renderer'
          : 'Switch focused pane to Focus renderer',
        hint: isFocusedNative
          ? 'Back to the raw TUI (xterm). Width is shared across devices.'
          : `Render this ${focusedAgentLabel} pane as a structured chat that reflows at any width`,
        category: 'Terminal',
        keywords: 'focus native terminal renderer mode toggle reflow chat xterm tui switch',
        // The palette has no inline three-way confirm (it closes on run), so a
        // switch on a WORKING pane defaults to the LOSSLESS deferred mode (fires
        // when the turn finishes) rather than silently interrupting; an idle pane
        // switches immediately as before. A toast tells the user it was armed.
        run: () => {
          const st = useStore.getState();
          const p = st.panes[focusedPaneId];
          if (p && shouldConfirmModeSwitch(p.state, p.agentMidTurn)) {
            void st.switchPaneMode(focusedPaneId, targetKind, { when: 'deferred' });
            st.toast(
              `Will switch to ${targetKind === 'native' ? 'Focus' : 'Terminal'} when ${focusedAgentLabel} finishes`,
              { kind: 'info', icon: '⏳', ttlMs: 4000 },
            );
            return;
          }
          void st.switchPaneMode(focusedPaneId, targetKind);
        },
      });
    }

    // Restart the focused pane's agent in place on the installed build. Any
    // running pane; the daemon resumes the conversation, or offers a fresh
    // restart when there's nothing durable to resume.
    if (focusedPaneId && st.panes[focusedPaneId]?.status === 'running') {
      list.push({
        id: 'pane.restart',
        label: 'Restart focused pane',
        hint: 'Roll just this pane onto the installed build, keeping its conversation',
        category: 'Terminal',
        keywords: 'restart reload refresh stale old build pane agent',
        run: () => void restartPaneFlow(focusedPaneId),
      });
    }

    // Auto-approve cycle for the focused pane. Same source of truth as
    // the header badge + context-menu radio (api.setPaneAutoMode), so the
    // three surfaces never drift. Label shows the next mode in the cycle.
    // Gated on the broader auto-approve capability so Codex panes get it too
    // (the renderer toggle above stays Claude-only).
    if (isFocusedAutoApproveCapable && focusedPaneId) {
      const nextMode = nextAutoMode(focusedAutoMode);
      list.push({
        id: 'pane.autoMode',
        label: `Set focused pane auto-approval: ${autoModeMenuLabel(nextMode)}`,
        hint: `Auto-approve is "${autoModeMenuLabel(focusedAutoMode)}" — cycle to "${autoModeMenuLabel(nextMode)}"`,
        category: 'Terminal',
        keywords: 'aria auto approve approval cautious always off mode pane',
        run: () => void useStore.getState().setPaneAutoMode(focusedPaneId, nextMode).catch(() => {}),
      });
    }

    // Agent turns + recently closed panes.
    const focusedInfo = focusedPaneId ? st.panes[focusedPaneId] : undefined;
    if (focusedInfo?.turn?.last) {
      const t = focusedInfo.turn.last;
      list.push({
        id: 'pane.lastTurnDiff',
        label: "Show focused pane's last turn changes",
        hint: t.files > 0 ? `Turn ${t.n}: ${t.files} file(s) +${t.added} −${t.removed}` : `Turn ${t.n}: no file changes`,
        category: 'Pane',
        keywords: 'turn diff changes what changed agent claude codex review last',
        shortcut: 'Alt+D',
        run: () => st.openTurnDiff(focusedInfo.id),
      });
    }
    if (focusedInfo) {
      list.push({
        id: 'pane.reviewChanges',
        label: "Review uncommitted changes in focused pane's repo",
        hint: 'git diff of the working tree (or staged) for the folder this pane is in',
        category: 'Pane',
        keywords: 'git diff review changes working tree staged repo',
        run: () => st.openDiffHere(focusedInfo.liveCwd || focusedInfo.cwd, false, focusedInfo.id),
      });
    }
    recentlyClosedPanes.forEach((c, i) => {
      list.push({
        id: `pane.reopen.${c.id}`,
        label: `Reopen closed pane: ${closedPaneLabel(c)}`,
        hint: closedPaneHint(c),
        category: 'Pane',
        keywords: 'reopen undo closed restore pane session resume',
        shortcut: i === 0 ? 'Alt+U' : undefined,
        run: () => void st.reopenClosedPane(c.id),
      });
    });

    // Dynamic: pane switchers, sorted unread-first.
    //
    // We read the pane map non-subscribingly (useStore.getState()) so
    // this memo doesn't rebuild on every WebSocket byte. The `open` dep
    // below refreshes the snapshot when the palette re-opens, which is
    // when the user actually sees this list — stale-but-cheap is the
    // right tradeoff for an always-reachable command surface.
    const orderedIds = paneIdsInOrder(st.layout);
    const lastViewed = st.lastViewedMs || {};
    const clientActivity = st.clientActivityMs || {};
    type PaneEntry = { id: string; title: string; unread: boolean; activityAge: number };
    const paneEntries: PaneEntry[] = [];
    for (const id of orderedIds) {
      const p = st.panes[id];
      if (!p || p.status !== 'running') continue;
      const serverActivity = p.lastActivityMs ?? 0;
      const effectiveActivity = Math.max(serverActivity, clientActivity[id] ?? 0);
      const viewed = lastViewed[id] ?? 0;
      const unread = id !== focusedPaneId && paneUnread(p, viewed, effectiveActivity) !== null;
      paneEntries.push({
        id,
        title: p.title || (p.cmd && p.cmd[0]) || id,
        unread,
        activityAge: Date.now() - effectiveActivity,
      });
    }
    // Unread first (by freshness), then the rest in layout order.
    paneEntries.sort((a, b) => {
      if (a.unread !== b.unread) return a.unread ? -1 : 1;
      if (a.unread && b.unread) return a.activityAge - b.activityAge;
      return 0;
    });
    const firstUnread = paneEntries.find((p) => p.unread);
    if (firstUnread) {
      // Convenience action — appears near the top thanks to the sort
      // below. Jumps to the freshest unread pane, clearing its marker
      // as a side effect of revealPane → lastViewedMs stamp in store.
      list.push({
        id: 'pane.jumpUnread',
        label: 'Jump to next unread pane',
        hint: `${paneEntries.filter((p) => p.unread).length} pane(s) with new activity`,
        category: 'Navigation',
        keywords: 'unread activity new output next focus',
        run: () => void useStore.getState().revealPane(firstUnread.id),
      });
    }
    for (const e of paneEntries) {
      if (e.id === focusedPaneId) continue;
      const mark = e.unread ? '● ' : '';
      list.push({
        id: `pane.focus.${e.id}`,
        label: `${mark}Focus pane: ${e.title}`,
        hint: e.unread ? 'Unread — new activity since last viewed' : undefined,
        category: 'Pane',
        keywords: `${e.title} pane focus switch${e.unread ? ' unread new activity' : ''}`,
        run: () => void useStore.getState().revealPane(e.id),
      });
    }

    // Dynamic: workspace switchers. Mirrors Sidebar ordering.
    const filtered = workspaces.filter((w) => w.name !== '_current');
    if (filtered.length) {
      const byName = Object.fromEntries(filtered.map((w) => [w.name, w]));
      const ordered: string[] = [];
      for (const name of wsOrder) {
        if (byName[name]) {
          ordered.push(name);
          delete byName[name];
        }
      }
      const leftovers = Object.values(byName)
        .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }))
        .map((w) => w.name);
      const names = [...ordered, ...leftovers];
      for (const name of names) {
        if (name === activeWorkspace) continue;
        list.push({
          id: `ws.${name}`,
          label: `Switch to workspace: ${name}`,
          category: 'Workspace',
          keywords: `${name} ws`,
          run: () => void useStore.getState().loadWorkspace(name),
        });
      }
    }

    // Feedback sits last so an empty palette doesn't open on it.
    list.push(
      {
        id: 'feedback.report',
        label: 'Send feedback',
        hint: 'Report a bug or share an idea',
        category: 'Help',
        keywords: 'bug feedback issue report idea problem suggestion request',
        run: () => { onClose(); onOpenFeedback(); },
      }
    );

    // The workflow Tasks console and Orchestrator role are team-only on the
    // public beta. Read per open (the memo re-runs on `open`).
    const team = operatorFeaturesAllowed();
    const ent = useEntitlements.getState();
    if (ent.loaded && (ent.real.team || ent.real.operator)) {
      const personal = ent.viewPersonal;
      list.push({
        id: 'view.personal',
        label: personal ? 'Back to full view' : 'View as personal',
        hint: personal ? 'Show team features again' : 'See the app as a free account does',
        category: 'Account',
        keywords: 'personal view free account stranger public team operator preview',
        run: () => {
          onClose();
          void useEntitlements.getState().setPersonalView(!personal).catch(() => {
            useStore.getState().toast('Could not change the view. It can only be changed on this computer.', { kind: 'error', icon: '⚠' });
          });
        },
      });
    }
    return list.filter((a) => team || !TEAM_ONLY_ACTIONS.has(a.id));
    // `open` in the deps re-snapshots panes each time the palette opens
    // — see the unread-surface block above for why we don't subscribe.
  }, [open, recentlyClosedPanes, focusedPaneId, focusedPaneCwd, focusEnabled, isFocusedNative, isFocusedAgentCapable, focusedAgentLabel, isFocusedAutoApproveCapable, focusedAutoMode, workspaces, wsOrder, activeWorkspace, onClose, onOpenHelp, onOpenNewPane, onOpenBroadcast, onOpenFeedback]);

  const actionHits = useMemo(() => {
    if (mode !== 'actions') return [];
    const q = query.trim();
    return actions
      .map((a) => ({ a, s: scoreAction(q, a) }))
      .filter((x): x is { a: Action; s: number } => x.s !== null)
      .sort((x, y) => x.s - y.s)
      .map((x) => x.a);
  }, [mode, actions, query]);

  // --- Files mode ---------------------------------------------------

  const runFileSearch = useCallback(async (r: string, q: string, k: Kind) => {
    if (!r) return;
    const seq = ++reqSeq.current;
    setLoading(true);
    setErr(null);
    try {
      const res = await api.searchFiles(r, q, 30, k);
      if (seq !== reqSeq.current) return;
      setFileHits(res.hits);
      setCapped(res.capped);
      setSelIdx(0);
    } catch (e) {
      if (seq !== reqSeq.current) return;
      setErr((e as Error).message);
      setFileHits([]);
    } finally {
      if (seq === reqSeq.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!open || mode !== 'files' || !root) return;
    void runFileSearch(root, '', kind);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, mode, root, kind]);

  useEffect(() => {
    if (!open || mode !== 'files' || !root) return;
    const h = setTimeout(() => { void runFileSearch(root, query, kind); }, 150);
    return () => clearTimeout(h);
  }, [query, root, kind, open, mode, runFileSearch]);

  // --- Search mode --------------------------------------------------

  // Debounced cross-pane search. The daemon caps results (8 per pane,
  // 40 overall) so even a hot token like "error" doesn't stream
  // megabytes. Empty query short-circuits without a network call.
  useEffect(() => {
    if (!open || mode !== 'search') return;
    const trimmed = query.trim();
    if (!trimmed) {
      setSearchHits([]);
      setSearchTotal(0);
      setSearchCapped(false);
      setSearchErr(null);
      return;
    }
    const seq = ++searchSeq.current;
    const h = setTimeout(async () => {
      setSearchLoading(true);
      setSearchErr(null);
      try {
        const res = await api.searchPanes(trimmed);
        if (seq !== searchSeq.current) return;
        // Flatten matches so ↑↓ cycles through every hit individually.
        const flat: SearchHit[] = [];
        for (const r of res.results) {
          const label = r.paneTitle || (r.paneCmd && r.paneCmd[0]) || r.paneId;
          for (const m of r.matches) {
            flat.push({
              paneId: r.paneId,
              paneLabel: label,
              line: m.line,
              text: m.text,
              start: m.start,
              end: m.end,
            });
          }
        }
        setSearchHits(flat);
        setSearchTotal(res.totalMatches);
        setSearchCapped(res.capped);
        setSelIdx(0);
      } catch (e) {
        if (seq !== searchSeq.current) return;
        setSearchErr((e as Error).message);
        setSearchHits([]);
      } finally {
        if (seq === searchSeq.current) setSearchLoading(false);
      }
    }, 150);
    return () => clearTimeout(h);
  }, [query, open, mode]);

  // --- Shared keyboard + selection ----------------------------------

  const hitsLen = mode === 'actions'
    ? actionHits.length
    : mode === 'files'
      ? fileHits.length
      : searchHits.length;

  useEffect(() => {
    if (selIdx >= hitsLen) setSelIdx(Math.max(0, hitsLen - 1));
  }, [hitsLen, selIdx]);

  useEffect(() => {
    if (!listRef.current) return;
    const node = listRef.current.querySelector<HTMLElement>(`[data-idx="${selIdx}"]`);
    node?.scrollIntoView({ block: 'nearest' });
  }, [selIdx]);

  function switchMode(next: Mode) {
    setMode(next);
    setQuery('');
    setSelIdx(0);
    if (next === 'files') {
      setRoot(focusedPaneCwd);
      setKind('files');
    }
    if (next === 'search') {
      setSearchHits([]);
      setSearchErr(null);
      setSearchCapped(false);
      setSearchTotal(0);
    }
    requestAnimationFrame(() => inputRef.current?.focus());
  }

  function onQueryChange(v: string) {
    // Prefix mode-switch. Only fires when the previous query was empty.
    //   actions → files:  `/`
    //   actions → search: `@`
    //   anywhere → actions: `>`
    if (mode === 'actions' && !query && v === '/') {
      switchMode('files');
      return;
    }
    if (mode === 'actions' && !query && v === '@') {
      switchMode('search');
      return;
    }
    if (mode !== 'actions' && !query && v === '>') {
      switchMode('actions');
      return;
    }
    setQuery(v);
    // Typing re-ranks the list, so Enter should run the new best match.
    setSelIdx(0);
  }

  function runAction(a: Action) {
    if (a.id === 'view.files' || a.id === 'view.search') {
      void a.run();
      return;
    }
    onClose();
    // Defer one frame so the modal tears down before the action runs.
    requestAnimationFrame(() => { void a.run(); });
  }

  function pickDir(path: string) {
    setFilesAnchor(path);
    pushRecentDir(path);
    onClose();
    toast(`Pinned ${labelRoot(path)} in Files`, {
      kind: 'success',
      icon: '📁',
      ttlMs: 2200,
    });
  }

  function openFileHit(h: Hit) {
    if (h.isDir) {
      pickDir(h.path);
      return;
    }
    openFileHere(h.path);
    onClose();
    toast(`Opened ${h.name}`, { kind: 'success', icon: '📄', ttlMs: 1500 });
  }

  function openSearchHit(h: SearchHit) {
    // Capture the query before onClose tears the palette down (which
    // resets state). Prefer the user's typed query over the hit excerpt
    // — the excerpt may be surrounded by ellipses that wouldn't match
    // xterm's buffer. Fall back to the matched substring from the hit.
    const revealQuery = query.trim() || h.text.slice(h.start, h.end);
    // revealPane loads the right workspace / adopts from background /
    // focuses — same path used by tapped phone notifications. Then fire
    // the reveal event so Pane.tsx runs the SearchAddon against its
    // buffer and scrolls + highlights the match. Best-effort: if the
    // query isn't in xterm's scrollback (e.g. trimmed by the 5000-line
    // cap), you still land on the right pane.
    void useStore.getState().revealPane(h.paneId);
    window.dispatchEvent(new CustomEvent('solu:pane-search-reveal', {
      detail: { paneId: h.paneId, query: revealQuery },
    }));
    onClose();
  }

  function cycleKind() {
    setKind((k) => (k === 'files' ? 'dirs' : k === 'dirs' ? 'all' : 'files'));
  }

  function chooseRoot(next: string) {
    setRoot(next);
    setRootMenuOpen(false);
    requestAnimationFrame(() => inputRef.current?.focus());
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setSelIdx((i) => (hitsLen ? (i + 1) % hitsLen : 0));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      setSelIdx((i) => (hitsLen ? (i - 1 + hitsLen) % hitsLen : 0));
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (mode === 'actions') {
        if (actionHits[selIdx]) runAction(actionHits[selIdx]);
      } else if (mode === 'files') {
        if (fileHits[selIdx]) openFileHit(fileHits[selIdx]);
      } else {
        if (searchHits[selIdx]) openSearchHit(searchHits[selIdx]);
      }
      return;
    }
    if (mode === 'files' && e.key === 'd' && e.altKey) {
      e.preventDefault();
      cycleKind();
      return;
    }
    // Backspace on empty query in a non-actions mode backs out to
    // actions — feels like undoing the `/` or `@` prefix.
    if (mode !== 'actions' && e.key === 'Backspace' && !query) {
      e.preventDefault();
      switchMode('actions');
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      if (rootMenuOpen) { setRootMenuOpen(false); return; }
      onClose();
    }
  }

  if (!open) return null;

  const rootMenuItems: { path: string; label: string; hint?: string }[] = [];
  const seen = new Set<string>();
  const addItem = (path: string, label: string, hint?: string) => {
    if (!path || seen.has(path)) return;
    seen.add(path);
    rootMenuItems.push({ path, label, hint });
  };
  if (focusedPaneCwd) addItem(focusedPaneCwd, labelRoot(focusedPaneCwd), 'focused pane');
  addItem('~', '~', 'home');
  addItem('/', '/', 'filesystem root');
  for (const d of recentDirs) addItem(d, labelRoot(d), 'recent');

  const rootLabel = labelRoot(root);
  const kindLabel = kind === 'files' ? 'Files' : kind === 'dirs' ? 'Folders' : 'Files + Folders';
  const kindHint = kind === 'files'
    ? 'matching files'
    : kind === 'dirs'
      ? 'matching folders — Enter pins as Files-panel anchor'
      : 'files and folders';

  const placeholder = mode === 'actions'
    ? 'Type an action — e.g. new, split, kill, workspace… ("/" files, "@" search panes)'
    : mode === 'files'
      ? (kind === 'dirs'
        ? 'Type folder-name fragment — e.g. soluterm, wsl-pro'
        : 'Type name fragment — e.g. App.tsx, store, render.ya')
      : 'Search across all pane scrollbacks — substring, case-insensitive';

  // Mode chip — click cycles actions → files → search → actions. Shows
  // the current sigil so the `/` / `@` / `>` convention is visible.
  const modeChipLabel = mode === 'actions' ? '>' : mode === 'files' ? '/' : '@';
  const modeChipTitle = mode === 'actions'
    ? 'Actions mode · click to cycle · "/" files · "@" search panes'
    : mode === 'files'
      ? 'Files mode · click to cycle · ">" actions · "@" search panes'
      : 'Search mode · click to cycle · ">" actions · "/" files';
  const nextMode: Mode = mode === 'actions' ? 'files' : mode === 'files' ? 'search' : 'actions';

  return (
    <div className="dialog-backdrop" onMouseDown={onClose} onMouseMove={pointer.track}>
      <div
        className="dialog quickopen"
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 640, maxWidth: '92vw' }}
      >
        <div className="quickopen-header">
          <button
            type="button"
            className="iconbtn"
            onClick={() => switchMode(nextMode)}
            title={modeChipTitle}
            style={{
              padding: '2px 8px',
              fontFamily: "'SF Mono','Menlo',monospace",
              fontSize: 12,
            }}
          >{modeChipLabel}</button>
          <input
            ref={inputRef}
            className="quickopen-input"
            placeholder={placeholder}
            value={query}
            onChange={(e) => onQueryChange(e.target.value)}
            onKeyDown={onKeyDown}
            autoComplete="off"
            spellCheck={false}
          />
          <button
            className="iconbtn"
            onClick={onClose}
            aria-label="Close"
            title="Close (Esc)"
          >×</button>
        </div>

        {mode === 'files' && (
          <div
            className="quickopen-scope"
            style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}
          >
            <span>in</span>
            <div style={{ position: 'relative' }}>
              <button
                className="iconbtn"
                onClick={() => setRootMenuOpen((v) => !v)}
                title="Change search root"
                style={{
                  padding: '2px 8px',
                  fontFamily: "'SF Mono','Menlo',monospace",
                  fontSize: 11,
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 4,
                }}
              >
                <code style={{ background: 'transparent', padding: 0 }}>{rootLabel || '…'}</code>
                <span style={{ opacity: 0.7 }}>▾</span>
              </button>
              {rootMenuOpen && (
                <div
                  ref={rootMenuRef}
                  role="menu"
                  className="pane-ctx-menu"
                  style={{
                    position: 'absolute',
                    top: '100%',
                    left: 0,
                    marginTop: 4,
                    zIndex: 10,
                    minWidth: 220,
                    maxWidth: 380,
                  }}
                >
                  {rootMenuItems.map((it) => (
                    <button
                      key={it.path}
                      role="menuitem"
                      className={it.path === root ? 'active' : ''}
                      onClick={() => chooseRoot(it.path)}
                      title={it.path}
                      style={{
                        display: 'flex',
                        justifyContent: 'space-between',
                        gap: 10,
                        fontFamily: "'SF Mono','Menlo',monospace",
                        fontSize: 11,
                      }}
                    >
                      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {it.label}
                      </span>
                      {it.hint && (
                        <span style={{ opacity: 0.55, fontSize: 10 }}>{it.hint}</span>
                      )}
                    </button>
                  ))}
                </div>
              )}
            </div>
            <button
              className="iconbtn"
              onClick={cycleKind}
              title={`Search mode: ${kindLabel} — ${kindHint}. Alt+D to cycle.`}
              style={{ padding: '2px 8px', fontSize: 11 }}
            >
              {kindLabel}
            </button>
            {loading && <span className="quickopen-spin"> · searching…</span>}
            {capped && !loading && <span className="quickopen-capped"> · too many entries, narrowed to first matches</span>}
          </div>
        )}

        {mode === 'files' && err && (
          <div className="quickopen-err">Couldn't search: {err}</div>
        )}

        {mode === 'search' && (
          <div
            className="quickopen-scope"
            style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}
          >
            <span>across running panes</span>
            {searchLoading && <span className="quickopen-spin"> · searching…</span>}
            {!searchLoading && query.trim() && (
              <span style={{ opacity: 0.7 }}>
                · {searchTotal} match{searchTotal === 1 ? '' : 'es'}
                {searchCapped ? ' (capped)' : ''}
              </span>
            )}
          </div>
        )}

        {mode === 'search' && searchErr && (
          <div className="quickopen-err">Couldn't search: {searchErr}</div>
        )}

        <div className="quickopen-list" ref={listRef} role="listbox">
          {mode === 'actions' && (
            <>
              {actionHits.length === 0 && (
                <div className="quickopen-empty">No matching actions.</div>
              )}
              {actionHits.map((a, i) => (
                <div
                  key={a.id}
                  data-idx={i}
                  role="option"
                  aria-selected={i === selIdx}
                  className={`quickopen-row ${i === selIdx ? 'sel' : ''}`}
                  onMouseMove={(e) => { if (pointer.moved(e)) setSelIdx(i); }}
                  onMouseDown={(e) => { e.preventDefault(); runAction(a); }}
                  title={a.hint || a.label}
                >
                  <span className="quickopen-name">
                    <span style={{ opacity: 0.6, fontSize: 10, marginRight: 6 }}>{a.category}</span>
                    <span>{a.label}</span>
                  </span>
                  {a.shortcut && (
                    <span className="quickopen-dir" style={{ fontFamily: "'SF Mono','Menlo',monospace" }}>
                      {a.shortcut}
                    </span>
                  )}
                </div>
              ))}
            </>
          )}

          {mode === 'files' && (
            <>
              {fileHits.length === 0 && !loading && !err && (
                <div className="quickopen-empty">
                  {query ? 'No matches.' : 'Nothing to show in this scope.'}
                </div>
              )}
              {fileHits.map((h, i) => {
                const lastSlash = h.rel.lastIndexOf('/');
                const dirPart = lastSlash >= 0 ? h.rel.slice(0, lastSlash + 1) : '';
                const namePart = lastSlash >= 0 ? h.rel.slice(lastSlash + 1) : h.rel;
                return (
                  <div
                    key={h.path}
                    data-idx={i}
                    role="option"
                    aria-selected={i === selIdx}
                    className={`quickopen-row ${i === selIdx ? 'sel' : ''}`}
                    onMouseMove={(e) => { if (pointer.moved(e)) setSelIdx(i); }}
                    onMouseDown={(e) => { e.preventDefault(); openFileHit(h); }}
                    title={h.isDir ? `${h.path} — pin as Files-panel anchor` : h.path}
                  >
                    <span className="quickopen-name">
                      {h.isDir ? '▸ ' : ''}{namePart}{h.isDir ? '/' : ''}
                    </span>
                    {dirPart && <span className="quickopen-dir">{dirPart}</span>}
                  </div>
                );
              })}
            </>
          )}

          {mode === 'search' && (
            <>
              {searchHits.length === 0 && !searchLoading && !searchErr && (
                <div className="quickopen-empty">
                  {query.trim() ? 'No matches across pane scrollbacks.' : 'Type text to find it in any running pane.'}
                </div>
              )}
              {searchHits.map((h, i) => {
                const before = h.text.slice(0, h.start);
                const hit = h.text.slice(h.start, h.end);
                const after = h.text.slice(h.end);
                return (
                  <div
                    key={`${h.paneId}:${h.line}:${i}`}
                    data-idx={i}
                    role="option"
                    aria-selected={i === selIdx}
                    className={`quickopen-row ${i === selIdx ? 'sel' : ''}`}
                    onMouseMove={(e) => { if (pointer.moved(e)) setSelIdx(i); }}
                    onMouseDown={(e) => { e.preventDefault(); openSearchHit(h); }}
                    title={`${h.paneLabel} · line ${h.line} — Enter to reveal the pane`}
                  >
                    <span className="quickopen-name" style={{ minWidth: 0 }}>
                      <span style={{ opacity: 0.6, fontSize: 10, marginRight: 6 }}>
                        {h.paneLabel} · L{h.line}
                      </span>
                      <span style={{ fontFamily: "'SF Mono','Menlo',monospace", fontSize: 11 }}>
                        {before}
                        <mark style={{
                          background: 'transparent',
                          color: 'var(--accent, #6cf)',
                          fontWeight: 600,
                        }}>{hit}</mark>
                        {after}
                      </span>
                    </span>
                  </div>
                );
              })}
            </>
          )}
        </div>

        <div className="quickopen-footer">
          {mode === 'actions' && (
            <><kbd>↑↓</kbd> select · <kbd>Enter</kbd> run · <kbd>/</kbd> files · <kbd>@</kbd> search · <kbd>Esc</kbd> close</>
          )}
          {mode === 'files' && (
            <><kbd>↑↓</kbd> select · <kbd>Enter</kbd> {kind === 'dirs' ? 'pin' : 'open'} · <kbd>Alt+D</kbd> mode · <kbd>⌫</kbd> back · <kbd>Esc</kbd> close</>
          )}
          {mode === 'search' && (
            <><kbd>↑↓</kbd> select · <kbd>Enter</kbd> reveal pane · <kbd>⌫</kbd> back · <kbd>Esc</kbd> close</>
          )}
        </div>
      </div>
    </div>
  );
}
