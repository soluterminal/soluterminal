import './loadExtensions';
import { Suspense, lazy, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useAttentionOutsideBadges } from './lib/attentionBadge';
import { Panel, PanelGroup, PanelResizeHandle, type ImperativePanelHandle } from 'react-resizable-panels';
import { noteUserActivity, useStore, paneIdsInOrder } from './store';
import { isRelayedSession, useCloud } from './cloud';
import { api, currentRoutingScope, noteUiEventsSseFailure } from './api';
import { serviceTarget } from './lib/portBrowser';
import { isTauri, playVoice, type VoiceEvent } from './voice';
import { getEmbedTheme, isEmbedded, sendEmbedNotification, sendEmbedReady } from './embed';
import { showEmbedChrome } from './embedChrome';
import { READLINE_ALT_COMBOS, comboFromEvent, isTerminalCollidingCombo, resolveCombo } from './keybindings';
import { isRouterMode } from './dualBackend';
import { lifecycleLog } from './debug';
import { isMobileLandscapeViewport, isMobileViewport, syncTextareaPinPref, syncVisualViewportCssVars } from './lib/mobile';
import { installPortraitOrientationLock } from './lib/orientation';
import { useZoomHotkeyWarning } from './lib/useZoomHotkeyWarning';

import { splitDaemonToastGlyph } from './lib/toastGlyph';
import { confirmClosePane } from './components/usePaneContextMenu';
import type { Workspace } from './types';
import { SupportBundleDialog } from './components/SupportBundleDialog';

// Find the nearest pane in the given direction from the focused pane.
// Uses each pane's on-screen rect (via data-pane-id markers in Pane.tsx)
// so the choice respects live split ratios, not the abstract layout tree.
// Returns null if no pane lies in that direction.
type Dir = 'left' | 'right' | 'up' | 'down';
function pickDirectionalPane(focusedId: string, dir: Dir): string | null {
  const els = document.querySelectorAll<HTMLElement>('[data-pane-id]');
  const rects: Array<{ id: string; r: DOMRect }> = [];
  let focused: DOMRect | null = null;
  for (const el of Array.from(els)) {
    const id = el.dataset.paneId!;
    const r = el.getBoundingClientRect();
    if (id === focusedId) focused = r;
    else rects.push({ id, r });
  }
  if (!focused) return null;
  const fx = focused.left + focused.width / 2;
  const fy = focused.top + focused.height / 2;

  // Score each candidate by (primary distance along the direction) +
  // (perpendicular offset penalty). Reject candidates that aren't
  // actually in the requested direction by rect overlap rather than
  // center — otherwise a pane immediately to the right whose center is
  // slightly up-and-over loses to one further but centered.
  let best: { id: string; score: number } | null = null;
  for (const { id, r } of rects) {
    let inDirection = false;
    let primary = 0;
    let perpendicular = 0;
    switch (dir) {
      case 'right':
        inDirection = r.left >= focused.right - 2;
        primary = r.left - focused.right;
        perpendicular = Math.abs((r.top + r.height / 2) - fy);
        break;
      case 'left':
        inDirection = r.right <= focused.left + 2;
        primary = focused.left - r.right;
        perpendicular = Math.abs((r.top + r.height / 2) - fy);
        break;
      case 'down':
        inDirection = r.top >= focused.bottom - 2;
        primary = r.top - focused.bottom;
        perpendicular = Math.abs((r.left + r.width / 2) - fx);
        break;
      case 'up':
        inDirection = r.bottom <= focused.top + 2;
        primary = focused.top - r.bottom;
        perpendicular = Math.abs((r.left + r.width / 2) - fx);
        break;
    }
    if (!inDirection) continue;
    const score = primary + perpendicular * 2;
    if (!best || score < best.score) best = { id, score };
  }
  return best?.id ?? null;
}

// Move native DOM focus to a pane's xterm helper textarea synchronously.
// Pane.tsx has a useEffect that does this after the store's focusedPaneId
// changes, but effects run post-commit — if the user mashes Alt+1 Alt+2
// and types a letter in quick succession, the letter's keydown can fire
// before React commits and runs the focus effect, landing the input on
// the OLD pane. Calling this in the same tick as setFocus moves browser
// focus before the next event dispatches.
function focusPaneTextarea(paneId: string): void {
  const paneEl = document.querySelector<HTMLElement>(`[data-pane-id="${paneId}"]`);
  const ta = paneEl?.querySelector<HTMLTextAreaElement>('.xterm-helper-textarea');
  if (ta) {
    try { ta.focus({ preventScroll: true }); } catch { ta.focus(); }
  }
}

import { Topbar } from './components/Topbar';
import { ScheduleHistoryDeepLink } from './components/ScheduleHistoryDialog';
import { routeNotificationTap } from './lib/notificationTap';
import { InboxHost } from './components/inbox/InboxPanel';
import { ScheduleDialogHost } from './components/ScheduleDialogHost';
import { UpdateOverlay } from './components/UpdateOverlay';
import { RequiredUpdateModal } from './components/RequiredUpdateModal';
import { DaemonHealthBanner } from './components/DaemonHealthBanner';
import { DegradedBanner } from './components/DegradedBanner';
import { UpdateBanner } from './components/UpdateBanner';
import { PhoneAccessBanner } from './components/PhoneAccessBanner';
import { ShellUpdateBanner } from './components/ShellUpdateBanner';
import { ClaudeInstallBanner } from './components/ClaudeInstallBanner';
import { MuseInstallBanner } from './components/MuseInstallBanner';
import { PhoneAccessPairPanelGate } from './components/PhoneAccessPairPanel';
import { Sidebar } from './components/Sidebar';
import { PaneGrid } from './components/PaneGrid';
import { WarmPaneLayer } from './components/WarmPaneLayer';
import { WorkspaceHandoffBoundary } from './components/WorkspaceHandoffBoundary';
import { workspaceMovedTo, workspaceTransferring } from './lib/workspaceHandoff';
import { ErrorBoundary } from './components/ErrorBoundary';
import { NewPaneDialog } from './components/NewPaneDialog';
import { MobileKeyBar } from './components/MobileKeyBar';
import { MobileDrawer } from './components/MobileDrawer';
import { MobileQuickReply } from './components/MobileQuickReply';
import { MobileTabBar } from './components/MobileTabBar';
import { useBottomBarsMeasure } from './components/useBottomBarsMeasure';
import { StatusBar } from './components/StatusBar';
import { ToastHost } from './components/ToastHost';
import { DialogHost } from './components/DialogHost';
import { HotkeyHelp } from './components/HotkeyHelp';
import { agentLabel, waitingAgentKind } from './lib/agentKind';
import { paneRolesAvailable, roleHostSupport, slotList, uiEventHandler, type GlobalKeys } from './extensions';
import type { OrchestratorHostInfo } from './types';
import { WelcomeOverlay } from './components/WelcomeOverlay';
import { useSettings } from './settings';
import { SetupChecklist } from './components/SetupChecklist';
import { WhatsNewToast } from './components/WhatsNewToast';
import { BannerSetupModal } from './components/BannerSetupModal';
import { BroadcastDialog } from './components/BroadcastDialog';
import { ScrollDiagOverlay } from './components/ScrollDiagOverlay';
import { ImeTraceOverlay } from './components/ImeTraceOverlay';
import { BootstrapErrorState } from './components/BootstrapErrorState';
import { DeviceApprovalPendingState } from './components/DeviceApprovalPendingState';
import { MachineAccessDeniedState } from './components/MachineAccessDeniedState';
import { useMachineAccess } from './lib/useMachineAccess';
import { useEntitlements, useLoadEntitlementsOnMount, useOperatorFeatures } from './entitlements';
import { useAskToConnectOnSignIn } from './accountConnect';
import { useAgentIntegrationPrompt } from './agentIntegration';
import { startAgent } from './agentStart';
import { AgentInstallPrompt } from './components/AgentInstallPrompt';
import { DeviceApprovalPrompt } from './components/DeviceApprovalPrompt';
import { remoteDevicePendingText } from './components/settings/RemoteDevicesSetting';
import { PrivateRelayIdentityWarning } from './components/PrivateRelayIdentityWarning';
import { PrivateRelayStatus } from './components/PrivateRelayStatus';
import { RebuildBackoff } from './lib/rebuildBackoff';
import { PRIVATE_RELAY_STATUS_EVENT } from './lib/privateRelay';
import { installerUpdate } from './lib/installerUpdate';

const ActivityDialog = lazy(() =>
  import('./components/ActivityDialog').then((mod) => ({ default: mod.ActivityDialog })),
);
const SystemPage = lazy(() =>
  import('./components/system/SystemPage').then((mod) => ({ default: mod.SystemPage })),
);
const SettingsModal = lazy(() =>
  import('./components/SettingsModal').then((mod) => ({ default: mod.SettingsModal })),
);
const CommandPalette = lazy(() =>
  import('./components/CommandPalette').then((mod) => ({ default: mod.CommandPalette })),
);

// Sidebar width in PIXELS. We compute the percentage at mount time from
// these + window.innerWidth because react-resizable-panels (v2.1.9) only
// exposes percentage-based sizing. Percentage-only meant the sidebar
// ballooned on wide monitors: 12% on a 2560px screen is 307px, which is
// way more than the workspace list / short labels need. These pixel
// targets give a content-driven width regardless of screen size.
// (User resizes of the WINDOW after mount still scale proportionally —
// that's a library limitation, not something we can avoid without
// rewriting the resize mechanism from scratch.)
// 260/220: at the old 160/120 section headers truncated ("WORKSPA…") and
// todo text wrapped one word per line.
const SIDEBAR_DEFAULT_PX = 260;
const SIDEBAR_MIN_PX = 220;
const SIDEBAR_MAX_PX = 480;

function workspaceKey(w: Workspace): string {
  return w.backend ? `${w.backend}:${w.name}` : w.name;
}

// Cycle through saved workspaces in the same order the Sidebar renders
// them (wsOrder custom order first, unknown names appended alphabetically).
// Returns true if a switch was initiated. Shared by Alt+Shift+↑/↓ and
// Alt+wheel so both entry points stay consistent.
function cycleWorkspace(delta: 1 | -1, source = 'unknown'): boolean {
  const s = useStore.getState();
  const filtered = s.workspaces.filter((w) => w.name !== '_current');
  if (filtered.length === 0) return false;
  const byKey = Object.fromEntries(filtered.map((w) => [workspaceKey(w), w]));
  const byLegacyName = Object.fromEntries(filtered.map((w) => [w.name, w]));
  const ordered: Workspace[] = [];
  for (const key of s.wsOrder) {
    const w = byKey[key] ?? byLegacyName[key];
    if (w) {
      ordered.push(w);
      delete byKey[workspaceKey(w)];
    }
  }
  const leftovers = Object.values(byKey)
    .sort((a, b) => workspaceKey(a).localeCompare(workspaceKey(b), undefined, { sensitivity: 'base' }));
  const workspaces = [...ordered, ...leftovers];
  const currentKey = s.activeBackend ? `${s.activeBackend}:${s.activeWorkspace}` : s.activeWorkspace;
  const cur = s.activeWorkspace === '_current'
    ? -1
    : workspaces.findIndex((w) => workspaceKey(w) === currentKey || (!s.activeBackend && w.name === s.activeWorkspace));
  const nextIdx = cur < 0
    ? (delta === 1 ? 0 : workspaces.length - 1)
    : (cur + delta + workspaces.length) % workspaces.length;
  const target = workspaces[nextIdx];
  if (target) {
    lifecycleLog('app.workspace-cycle', {
      source,
      delta,
      from: s.activeWorkspace,
      to: target.name,
      activeBackend: s.activeBackend,
      targetBackend: target.backend ?? null,
      workspaces: workspaces.map((w) => workspaceKey(w)),
    });
    if (isRouterMode() && target.backend && target.backend !== s.activeBackend) {
      void s.switchBackend(target.backend, target.name).catch(() => {});
    } else if (target.name !== s.activeWorkspace || (target.backend && target.backend !== s.activeBackend)) {
      void s.loadWorkspace(target.name).catch(() => {});
    }
  }
  return true;
}

// Shared mobile breakpoint. Everything keyed off "am I on a phone" reads
// this instead of rewriting its own matchMedia / innerWidth check.
// One-time cleanup of localStorage keys left behind by the removed ARIA
// panel (chat transcript, collapsed-zone flags, mobile section pick). Runs
// at module load so it can never race a render; try/catch covers private
// browsing modes where localStorage throws.
try {
  for (const k of Object.keys(localStorage)) {
    if (k.startsWith('solu.aria.')) localStorage.removeItem(k);
  }
} catch { /* localStorage unavailable; nothing to clean */ }

function useIsMobile() {
  const [mobile, setMobile] = useState(
    isMobileViewport(),
  );
  useEffect(() => {
    const on = () => setMobile(isMobileViewport());
    window.addEventListener('resize', on);
    window.visualViewport?.addEventListener('resize', on);
    return () => {
      window.removeEventListener('resize', on);
      window.visualViewport?.removeEventListener('resize', on);
    };
  }, []);
  return mobile;
}

function useIsMobileLandscape() {
  const [landscape, setLandscape] = useState(isMobileLandscapeViewport());
  useEffect(() => {
    const on = () => setLandscape(isMobileLandscapeViewport());
    window.addEventListener('resize', on);
    window.addEventListener('orientationchange', on);
    window.visualViewport?.addEventListener('resize', on);
    return () => {
      window.removeEventListener('resize', on);
      window.removeEventListener('orientationchange', on);
      window.visualViewport?.removeEventListener('resize', on);
    };
  }, []);
  return landscape;
}

export function WorkspaceLoadingState() {
  return (
    <div className="workspace-state workspace-state--loading" role="status" aria-live="polite" aria-busy="true">
      <div className="workspace-state-spinner" aria-hidden />
      <div className="workspace-state-title">Loading sessions</div>
      <div className="workspace-state-copy">Connecting to the daemon and restoring your workspace.</div>
    </div>
  );
}

type EmptySessionKind = 'shell' | 'claude' | 'codex' | 'orchestrator' | 'orchestrator-codex';

export const EMPTY_WORKSPACE_COPY = 'Start an agent, or open a terminal. Sessions keep running when you close the window.';

type EmptyWorkspaceStateProps = {
  creating: boolean;
  // Which start button is busy, so only that one says "Starting…".
  creatingKind?: EmptySessionKind | null;
  hosts: OrchestratorHostInfo[] | null;
  onCreateShell: () => void;
  onCreateClaude: () => void;
  onCreateCodex?: () => void;
  onCreateOrchestrator: () => void;
  onCreateOrchestratorCodex: () => void;
  onOpenDialog: () => void;
};

export function EmptyWorkspaceState({
  creating,
  creatingKind = null,
  hosts,
  onCreateShell,
  onCreateClaude,
  onCreateCodex,
  onCreateOrchestrator,
  onCreateOrchestratorCodex,
  onOpenDialog,
}: EmptyWorkspaceStateProps) {
  const codex = roleHostSupport('codex', hosts);
  // The Orchestrator is an operator pane role for coordinating direct work.
  const orchestrator = useOperatorFeatures() && paneRolesAvailable();
  const starting = (kind: EmptySessionKind, label: string) =>
    creating && (creatingKind ?? 'claude') === kind ? 'Starting…' : label;
  return (
    <div className="workspace-state workspace-state--empty">
      <div className="workspace-state-title">No sessions here yet</div>
      <div className="workspace-state-copy">{EMPTY_WORKSPACE_COPY}</div>
      <div className="workspace-state-actions">
        <button type="button" className="primary" onClick={onCreateClaude} disabled={creating}>
          {starting('claude', 'Start Claude Code')}
        </button>
        {onCreateCodex && (
          <button type="button" className="primary" onClick={onCreateCodex} disabled={creating}>
            {starting('codex', 'Start Codex')}
          </button>
        )}
        <button type="button" onClick={onCreateShell} disabled={creating}>
          Open a terminal
        </button>
        {/* The coordinating pane role is offered as a peer of the shell. */}
        {orchestrator && (
          <button type="button" className="primary" onClick={onCreateOrchestrator} disabled={creating}>
            {creating ? 'Starting...' : 'Start Orchestrator'}
          </button>
        )}
        {orchestrator && codex.supported && (
          <button
            type="button"
            className="workspace-state-quiet-action"
            onClick={onCreateOrchestratorCodex}
            disabled={creating || !codex.available}
            title={!codex.available ? codex.reason : undefined}
          >
            Start Orchestrator with Codex
          </button>
        )}
        <button type="button" className="workspace-state-quiet-action" onClick={onOpenDialog} disabled={creating}>
          Choose command
        </button>
      </div>
      {orchestrator && (
        <div className="workspace-state-copy">
          One pane that investigates, builds, and coordinates other panes; you decide what merges.
        </div>
      )}
    </div>
  );
}

export default function App() {
  useAttentionOutsideBadges();
  const teamFeatures = useOperatorFeatures();
  const bootstrap = useStore((s) => s.bootstrap);
  const orchestratorHosts = useStore((s) => s.orchestratorHosts);
  // Boolean, not the layout object: App only branches on emptiness, and
  // subscribing to the object identity re-rendered the entire app shell
  // (topbar, sidebar, status bar) on every splitter-drag frame — setLayout
  // builds a fresh tree per onLayout tick.
  const hasLayout = useStore((s) => !!s.layout);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const killPane = useStore((s) => s.killPane);
  const setFocus = useStore((s) => s.setFocus);
  const loading = useStore((s) => s.loading);
  const bootstrapError = useStore((s) => s.bootstrapError);

  const machineAccess = useMachineAccess();
  const uiScale = useStore((s) => s.uiScale);
  const appTheme = useStore((s) => s.appTheme);
  const createPane = useStore((s) => s.createPane);
  const toast = useStore((s) => s.toast);
  const systemPageOpen = useStore((s) => s.systemPageOpen);
  const setSystemPageOpen = useStore((s) => s.setSystemPageOpen);
  // Warn when a GPU overlay owns the pane-zoom chord system-wide
  // (desktop only; no-op in browser/PWA and on old shells).
  useZoomHotkeyWarning();
  useEffect(() => {
    lifecycleLog('app.mount', {
      href: window.location.href,
      visibility: document.visibilityState,
    });
    return () => lifecycleLog('app.unmount', {
      visibility: document.visibilityState,
    });
  }, []);
  useEffect(() => installPortraitOrientationLock(), []);
  useLoadEntitlementsOnMount();
  // After a sign-in, ask once whether to connect this PC to the account.
  useAskToConnectOnSignIn();
  useEffect(() => {
    let raf = 0;
    const sync = () => {
      if (raf) return;
      raf = window.requestAnimationFrame(() => {
        raf = 0;
        syncVisualViewportCssVars();
      });
    };
    syncVisualViewportCssVars();
    // Apply the textarea-pin preference (default on; escape hatch via
    // ?textareapin=0 / localStorage). See syncTextareaPinPref.
    syncTextareaPinPref();
    window.addEventListener('resize', sync);
    window.visualViewport?.addEventListener('resize', sync);
    window.visualViewport?.addEventListener('scroll', sync);
    return () => {
      if (raf) window.cancelAnimationFrame(raf);
      window.removeEventListener('resize', sync);
      window.visualViewport?.removeEventListener('resize', sync);
      window.visualViewport?.removeEventListener('scroll', sync);
    };
  }, []);
  // Mirror uiScale onto <html> as a CSS variable so CSS can scale the
  // chrome (sidebar + topbar) via a single `zoom: var(--ui-scale)` rule.
  // Doesn't touch the panes — they have their own dedicated terminal
  // font-size setting and zooming them would fight xterm's cell-grid math.
  useEffect(() => {
    document.documentElement.style.setProperty('--ui-scale', String(uiScale));
  }, [uiScale]);
  // index.html owns the pre-paint theme; this effect owns live preference and
  // OS color-scheme changes after React mounts.
  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: light)');
    const apply = () => {
      const override = getEmbedTheme();
      const light = override ? override === 'light' : appTheme === 'light' || (appTheme === 'system' && mq.matches);
      useStore.getState().setAppThemeLight(light);
      const el = document.documentElement;
      if (light) el.setAttribute('data-theme', 'light');
      else el.removeAttribute('data-theme');
      document.querySelector('meta[name="theme-color"]')?.setAttribute('content', light ? '#f5f7fa' : '#0b0d10');
    };
    apply();
    if (appTheme !== 'system') return;
    mq.addEventListener('change', apply);
    return () => mq.removeEventListener('change', apply);
  }, [appTheme]);
  useEffect(() => { sendEmbedReady(typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : undefined); }, []);

  const [dialog, setDialog] = useState<{ open: boolean; split: 'horizontal' | 'vertical' | null }>({
    open: false,
    split: null,
  });
  const [creatingSession, setCreatingSession] = useState<EmptySessionKind | null>(null);
  const [helpOpen, setHelpOpen] = useState(false);
  const supportBundleOpen = useStore((state) => state.supportBundleOpen);
  const setSupportBundleOpen = useStore((state) => state.setSupportBundleOpen);
  const [welcomeOpen, setWelcomeOpen] = useState(false);
  // First-run setup checklist (distinct localStorage flag from the welcome
  // tour). Surfaced after the tour so new users land on a live "is my
  // install wired up?" card before touching anything.
  const [checklistOpen, setChecklistOpen] = useState(false);
  useEffect(() => {
    try {
      if (localStorage.getItem('solu.seenWelcome') !== '1') setWelcomeOpen(true);
      else if (localStorage.getItem('solu.seenChecklist') !== '1') setChecklistOpen(true);
    } catch { /* ignore */ }
  }, []);
  const dismissWelcome = () => {
    try { localStorage.setItem('solu.seenWelcome', '1'); } catch { /* ignore */ }
    setWelcomeOpen(false);
    // Chain into the setup checklist for first-time users. Guarded by its
    // own flag so a user who's already seen it doesn't get it again.
    try {
      if (localStorage.getItem('solu.seenChecklist') !== '1') setChecklistOpen(true);
    } catch { setChecklistOpen(true); }
  };
  // Help menu → "Getting started": the welcome tour again, or the setup
  // checklist where the tour isn't shown (embedded builds).
  const openGettingStarted = () => {
    if (showEmbedChrome('welcome')) setWelcomeOpen(true);
    else setChecklistOpen(true);
  };
  const dismissChecklist = () => {
    try { localStorage.setItem('solu.seenChecklist', '1'); } catch { /* ignore */ }
    setChecklistOpen(false);
  };
  // Live daemon reachability. Both first-run overlays gate on this so they
  // NEVER pop over a disconnected app — a brand-new install whose daemon
  // hasn't come up yet should see the connection/bootstrap state, not a
  // checklist floating over an unusable shell.
  const daemonOnline = useStore((s) => s.daemonOnline);
  const firstRunReady = daemonOnline && !loading && !bootstrapError;
  // The current load failed because the computer refused this browser, not
  // because the daemon is down.
  const awaitingApproval = useStore((s) => s.bootstrapNeedsApproval);
  // Ask about adding SoluTerminal to Claude Code / Codex only once the
  // first-run tour and checklist are out of the way (agentIntegration.ts).
  useAgentIntegrationPrompt(firstRunReady && !welcomeOpen && !checklistOpen && showEmbedChrome('welcome'));
  const createEmptyWorkspaceSession = async (kind: EmptySessionKind) => {
    if (creatingSession) return;
    setCreatingSession(kind);
    try {
      // Start buttons check first: a missing agent opens its install card.
      if (kind === 'claude' || kind === 'codex') {
        await startAgent(kind);
        return;
      }
      await createPane(
        kind === 'shell'
          ? { cwd: '', cmd: [], title: 'shell' }
          // Terminal renderer for both Claude CTAs — neither consults the
          // defaultClaudeMode Focus preference (that's the dialog's job).
          : kind === 'orchestrator'
            ? { cwd: '', cmd: ['claude'], title: 'Orchestrator', role: 'orchestrator' as const }
            : { cwd: '', cmd: ['codex'], agent: 'codex' as const, title: 'Orchestrator', role: 'orchestrator' as const },
      );
    } catch (e) {
      const label = kind === 'shell' ? '$SHELL' : kind === 'orchestrator' ? 'Orchestrator' : kind === 'orchestrator-codex' ? 'Orchestrator (Codex)' : kind === 'codex' ? 'Codex' : 'Claude';
      toast(`Could not start ${label}: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
      });
    } finally {
      setCreatingSession(null);
    }
  };
  // Palette opens in one of two modes. Ctrl+K → 'actions'; Ctrl+P →
  // 'files'. Inside the palette the user can flip between modes with
  // the `/` / `>` prefix or the mode chip — these keys just seed which
  // list they see first.
  const [palette, setPalette] = useState<{ open: boolean; mode: 'actions' | 'files' | 'search'; query?: string }>({
    open: false,
    mode: 'actions',
  });
  // Touch surfaces (the mobile bottombar's ⌘ button) open the palette through
  // this event — they can't reach the keyboard dispatcher that normally seeds
  // it, and the palette state is App-local by design.
  useEffect(() => {
    const onOpen = (e: Event) => {
      const detail = (e as CustomEvent).detail ?? {};
      const mode = detail.mode === 'files' ? 'files' : detail.mode === 'search' ? 'search' : 'actions';
      setPalette({ open: true, mode, query: typeof detail.query === 'string' ? detail.query : undefined });
    };
    window.addEventListener('solu:open-palette', onOpen);
    return () => window.removeEventListener('solu:open-palette', onOpen);
  }, []);
  const [broadcastOpen, setBroadcastOpen] = useState(false);
  const settingsOpen = useSettings((s) => s.open);
  const [activityOpen, setActivityOpen] = useState(false);
  const isMobile = useIsMobile();
  const isMobileLandscape = useIsMobileLandscape();
  // Measure the fixed bottom bars into --bottombar-h/--keybar-h/--quick-reply-h
  // so .main padding + the bars' own `bottom` track their real rendered height
  // (font scaling, notch, keybar wrap) instead of hard-coded 52/56/66px.
  useBottomBarsMeasure(isMobile);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const ownerVisible = false;
  // Keybar visibility lives at App level so the unified bottom bar's
  // toggle button and the keybar itself can share it. Previously
  // MobileKeyBar owned the state internally.
  const [keybarOpen, setKeybarOpen] = useState(false);
  // Hamburger/menu dot: lights up when any pane the user ISN'T currently
  // viewing needs attention (awaiting-input / choice-prompt).
  //
  // Perf note: this selector returns a BOOLEAN rather than subscribing
  // to `s.panes`. The panes map gets a new reference on every WebSocket
  // byte (lastActivityMs update), so subscribing to it re-renders App
  // constantly. Zustand re-renders only when the selector's output
  // changes (Object.is), so returning a bool keeps App stable — the
  // dominant source of perceived mobile lag before this fix.
  const attentionOffscreen = useStore((s) => {
    const focused = s.focusedPaneId;
    for (const id in s.panes) {
      const p = s.panes[id];
      if (
        p.status === 'running' &&
        p.id !== focused &&
        (p.state === 'awaiting-input' || p.state === 'choice-prompt')
      ) {
        return true;
      }
    }
    return false;
  });
  // Auto-close the mobile drawer whenever the user actually acts on
  // something inside it (workspace load, background-pane add). Otherwise
  // they'd have to manually dismiss, defeating the "I picked, now show me
  // the terminal" flow. We watch the top-level state that those actions
  // change and reset drawerOpen off of it.
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  useEffect(() => {
    if (drawerOpen) setDrawerOpen(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeWorkspace]);

  // Sidebar imperative handle — used by Ctrl+Q (toggle) and the collapsed-rail
  // click-to-expand. react-resizable-panels persists the panel size via the
  // PanelGroup's autoSaveId, so the user's last width restores on reload.
  const sidebarRef = useRef<ImperativePanelHandle>(null);
  // The .main row that wraps the horizontal PanelGroup — used to measure the
  // live sidebar column width and publish it as --content-left (see below).
  const mainRef = useRef<HTMLDivElement>(null);
  // Convert pixel targets → percentages based on current window width.
  // useMemo with [] pins this to mount time on purpose: the Panel only
  // reads defaultSize once, so reacting to window resize here would have
  // no effect. We accept that tradeoff in exchange for a sensible first-
  // open width that's driven by what the sidebar NEEDS to show rather
  // than what fraction of the monitor it happens to occupy.
  const sidebarDefaultPct = useMemo(() => {
    const w = typeof window !== 'undefined' && window.innerWidth > 0
      ? window.innerWidth
      : 1280;
    return (SIDEBAR_DEFAULT_PX / w) * 100;
  }, []);
  // Min/max DO track window resizes: the library re-validates sizes when
  // constraints change, so the sidebar can't shrink below 220px after the
  // window grows (it used to reopen at ~115px after a narrow→wide resize).
  const [windowWidth, setWindowWidth] = useState(() => (
    typeof window !== 'undefined' && window.innerWidth > 0 ? window.innerWidth : 1280
  ));
  useEffect(() => {
    let raf = 0;
    const onResize = () => {
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        if (window.innerWidth > 0) setWindowWidth(window.innerWidth);
      });
    };
    window.addEventListener('resize', onResize);
    return () => { if (raf) cancelAnimationFrame(raf); window.removeEventListener('resize', onResize); };
  }, []);
  const sidebarSizePct = {
    defaultSize: sidebarDefaultPct,
    minSize: Math.min(40, (SIDEBAR_MIN_PX / windowWidth) * 100),
    maxSize: Math.min(60, (SIDEBAR_MAX_PX / windowWidth) * 100),
  };
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const toggleSidebar = () => {
    const p = sidebarRef.current;
    if (!p) return;
    if (p.isCollapsed()) p.expand();
    else p.collapse();
  };

  // Publish the live width of the sidebar column (panel + resize handle) as the
  // CSS custom property --content-left. The Topbar's left "sidebar zone" reads
  // it so the workspace/hosting cluster that follows begins exactly at the
  // content column's left edge — visually above the first pane — and tracks the
  // sidebar as it's dragged, collapsed, or the window is resized. Measured (not
  // computed from PanelGroup percentages) so it stays correct across window
  // resizes too. Scoped to `.main-panels > …` to match the CSS that styles
  // this exact rail (the section handles inside the sidebar are the separate
  // .sidebar-section-resize class).
  useLayoutEffect(() => {
    const root = mainRef.current;
    if (!root) return;
    const panel = root.querySelector<HTMLElement>('.main-panels > .sidebar-panel');
    const handle = root.querySelector<HTMLElement>('.main-panels > .sidebar-resize-handle');
    if (!panel) return;
    let raf = 0;
    const publish = () => {
      raf = 0;
      const w = panel.offsetWidth + (handle?.offsetWidth ?? 0);
      document.documentElement.style.setProperty('--content-left', `${Math.round(w)}px`);
    };
    const schedule = () => { if (!raf) raf = requestAnimationFrame(publish); };
    publish();
    const ro = new ResizeObserver(schedule);
    ro.observe(panel);
    if (handle) ro.observe(handle);
    return () => { if (raf) cancelAnimationFrame(raf); ro.disconnect(); };
  }, []);

  // StrictMode replays mount effects in local development. A second restore
  // can overwrite panes opened after the first restore has finished.
  const initialBootstrapStarted = useRef(false);
  useEffect(() => {
    if (initialBootstrapStarted.current) return;
    initialBootstrapStarted.current = true;
    void bootstrap();
  }, [bootstrap]);

  // ?github=<connected|requested|error|claim> arrival from the GitHub App
  // install callback (cloud/github_app.go redirects here after GitHub's
  // install screen). connected/requested/error are toast-and-strip;
  // claim (an install started from github.com directly, no signed state)
  // asks the signed-in user to explicitly finish the connection — identity
  // must come from an in-app confirmation, never from the URL alone.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const params = new URLSearchParams(window.location.search);
    const gh = params.get('github');
    if (!gh) return;
    const installId = Number(params.get('installation_id'));
    try {
      params.delete('github');
      params.delete('installation_id');
      const qs = params.toString();
      // Strip first, always: a refresh must never re-fire a toast or prompt.
      window.history.replaceState(
        {},
        '',
        window.location.pathname + (qs ? '?' + qs : '') + window.location.hash,
      );
    } catch { /* ignore */ }
    const toast = useStore.getState().toast;
    if (gh === 'connected') {
      toast('GitHub connected — agents pick up your repos on their next run.', { kind: 'success' });
    } else if (gh === 'requested') {
      toast('Install request sent — waiting for your GitHub org admin to approve.', { kind: 'info', ttlMs: 10_000 });
    } else if (gh === 'error') {
      toast('GitHub connection failed — try Connect GitHub again.', { kind: 'error' });
    } else if (gh === 'claim' && Number.isInteger(installId) && installId > 0) {
      void (async () => {
        const { confirmDialog } = await import('./lib/dialog');
        const ok = await confirmDialog({
          title: 'Finish connecting GitHub?',
          message: 'A GitHub App install was started for this account. Connect it to SoluTerminal so agents can review the repos you picked?',
          confirmLabel: 'Connect',
        });
        if (!ok) return;
        try {
          const s = await api.githubAppClaim(installId);
          toast(
            `GitHub connected${typeof s.repoCount === 'number' ? ` — ${s.repoCount} repo${s.repoCount === 1 ? '' : 's'}` : ''}.`,
            { kind: 'success' },
          );
        } catch {
          toast('Could not finish the GitHub connection — try Connect GitHub again.', { kind: 'error' });
        }
      })();
    }
  }, []);

  // ?pane=<paneId> arrival from a tapped phone notification.
  //
  // The SW's notificationclick handler (sw.js) appends this query
  // param when routing, so a tapped alert lands directly on the
  // triggering pane — loading the right workspace, adopting from
  // Background if needed, and focusing it. We strip the param from
  // the URL after handling so a refresh doesn't re-fire.
  //
  // Gated on bootstrap having completed (panes map populated). We
  // poll every 200ms for up to 10s; the payload from a push is tiny
  // and bootstrap is usually done within a few hundred ms on repeat
  // visits, but first-time cold loads can run longer.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const params = new URLSearchParams(window.location.search);
    const paneId = params.get('pane');
    if (!paneId) return;
    // Automation and agent pushes ride the same slot as "sched:<id>" /
    // "agent:<id>" (pusher.go). A tap on "⏰ Morning digest" should land on
    // its Inbox report, not fizzle in the pane-poll below.
    // routeNotificationTap owns waiting for the item to load.
    if (paneId.startsWith('sched:') || paneId.startsWith('agent:')) {
      try {
        params.delete('pane');
        const qs = params.toString();
        window.history.replaceState({}, '', window.location.pathname + (qs ? '?' + qs : '') + window.location.hash);
      } catch { /* ignore */ }
      routeNotificationTap(paneId);
      return;
    }
    let cancelled = false;
    const start = Date.now();
    const tick = () => {
      if (cancelled) return;
      const st = useStore.getState();
      if (st.panes[paneId]) {
        void st.revealPane(paneId);
        // Strip the param so a subsequent refresh (or sharing the
        // URL) doesn't cause us to re-reveal.
        try {
          params.delete('pane');
          const qs = params.toString();
          const newUrl = window.location.pathname + (qs ? '?' + qs : '') + window.location.hash;
          window.history.replaceState({}, '', newUrl);
        } catch { /* ignore */ }
        return;
      }
      if (Date.now() - start > 10_000) return; // give up after 10s
      window.setTimeout(tick, 200);
    };
    // Wait one tick so the bootstrap effect above fires first.
    window.setTimeout(tick, 50);
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Notification tap while the app is ALREADY open at this machine. The SW
  // can't hand a running page a new query param — it posts a message
  // instead (sw v24). Same routing as the ?pane= arrival above: automation
  // and agent ids open their Inbox item, pane ids get revealed.
  useEffect(() => {
    if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) return;
    const onMsg = (e: MessageEvent) => {
      const d = e.data as { type?: string; paneId?: string } | null;
      if (!d || d.type !== 'solu:notification-tap' || !d.paneId) return;
      if (routeNotificationTap(d.paneId)) return;
      const st = useStore.getState();
      if (st.panes[d.paneId]) void st.revealPane(d.paneId);
    };
    navigator.serviceWorker.addEventListener('message', onMsg);
    return () => navigator.serviceWorker.removeEventListener('message', onMsg);
  }, []);

  // Push-subscription liveness ping.
  //
  // The cloud keeps subscriptions in memory (see cloud/push.go), so a
  // cloud restart, daemon reboot, or evicted PWA session silently kills
  // notification delivery until the bell is toggled off and on. This
  // effect re-POSTs the subscription whenever (a) we mount, (b) the
  // user selects a new machine, or (c) they toggle the bell on — any
  // of which can change the eligibility condition. The store action
  // is fully gated + idempotent, so firing it on non-cloud or pre-auth
  // states is a no-op.
  //
  // The original push.ts comment advertised this "cheap liveness ping"
  // but the wiring was missing — notifications could silently stop and
  // the UI wouldn't reflect it.
  const cloudMode = useCloud((s) => s.mode);
  const selectedMachineId = useCloud((s) => s.selectedMachineId);
  const cloudSessionToken = useCloud((s) => s.session?.access_token ?? null);
  const notifyEnabled = useStore((s) => s.notifyEnabled);
  useEffect(() => {
    if (cloudMode !== 'cloud' || !selectedMachineId || !cloudSessionToken || !notifyEnabled) {
      return;
    }
    void useStore.getState().resubscribePush();
  }, [cloudMode, selectedMachineId, cloudSessionToken, notifyEnabled]);

  // Banner-setup intro toast — one-time hint pointing at the 📤 button
  // next to the bell. Fires here on mount (not only inside
  // setNotifyEnabled) so users who subscribed before v0.0.118 shipped
  // still see the hint on their next visit. Guarded by a localStorage
  // flag so it only fires once per device across all code paths.
  // Tauri desktop skipped — OS-native notifications bypass the whole
  // channel-importance question this toast is about.
  useEffect(() => {
    if (isTauri()) return;
    if (cloudMode !== 'cloud') return;
    if (!notifyEnabled) return;
    let seen = false;
    try { seen = localStorage.getItem('solu.banner-info-toast-seen') === '1'; } catch { /* fall through */ }
    if (seen) return;
    // Small delay so it lands after bootstrap's own toasts (if any)
    // and doesn't compete visually with pane loading.
    const h = window.setTimeout(() => {
      useStore.getState().toast(
        'Notifications on. Want them as banners on your phone? Tap the Test button next to the bell.',
        { kind: 'info', icon: '🔔', ttlMs: 10_000 },
      );
      try { localStorage.setItem('solu.banner-info-toast-seen', '1'); } catch { /* ignore */ }
    }, 1500);
    return () => window.clearTimeout(h);
  }, [cloudMode, notifyEnabled]);

  // Ack the daemon's per-pane notification dedup as soon as the user
  // is demonstrably looking at a pane in an attention state. Without
  // this the daemon keeps re-firing pushes for the same prompt the
  // user has already seen; subsequent fires only happen after a real
  // new transition (user types → Claude shows a fresh prompt).
  const focusedPaneState = useStore((s) =>
    s.focusedPaneId ? s.panes[s.focusedPaneId]?.state ?? null : null,
  );
  useEffect(() => {
    if (!focusedPaneId) return;
    if (focusedPaneState !== 'awaiting-input' && focusedPaneState !== 'choice-prompt') return;
    const ackIfVisible = () => {
      if (document.visibilityState !== 'visible') return;
      void api.ackPaneNotification(focusedPaneId);
    };
    ackIfVisible();
    document.addEventListener('visibilitychange', ackIfVisible);
    return () => document.removeEventListener('visibilitychange', ackIfVisible);
  }, [focusedPaneId, focusedPaneState]);

  // First-launch notification opt-in.
  //
  // On a brand-new Tauri install the user has to hunt for the 🔔 icon
  // in the topbar to enable notifications — which is the whole point of
  // running soluterminal in the first place (approval prompts, schedule
  // fires). Auto-flip the bell ON at first launch so the user gets
  // Windows' one-time "allow notifications" dialog while they're still
  // oriented on the app, rather than minutes later from a buried toggle.
  //
  // Gated on localStorage: we ONLY run when 'solu.notifyEnabled' has
  // never been set. Once the user has made a decision (grant or deny),
  // we persist it here on behalf of setNotifyEnabled so subsequent
  // launches don't re-prompt. Manual click on 🔔 in the topbar still
  // works either way.
  useEffect(() => {
    if (!isTauri()) return;
    let cancelled = false;
    try {
      if (localStorage.getItem('solu.notifyEnabled') !== null) return;
    } catch { /* localStorage blocked → fall through and auto-prompt */ }
    (async () => {
      const ok = await useStore.getState().setNotifyEnabled(true);
      if (cancelled) return;
      if (!ok) {
        // Permission denied or plugin error. Persist '0' so we don't
        // auto-prompt on every launch forever; user can still re-enable
        // via the topbar bell.
        try { localStorage.setItem('solu.notifyEnabled', '0'); } catch { /* ignore */ }
      }
    })();
    return () => { cancelled = true; };
  }, []);

  // Desktop-shell updates may relaunch only after a genuine quiet window.
  // Capture reaches terminal/composer events before pane handlers consume them.
  useEffect(() => {
    const note = () => noteUserActivity();
    const options: AddEventListenerOptions = { capture: true, passive: true };
    window.addEventListener('keydown', note, options);
    window.addEventListener('pointerdown', note, options);
    return () => {
      window.removeEventListener('keydown', note, options);
      window.removeEventListener('pointerdown', note, options);
    };
  }, []);

  // Service-worker upgrade re-subscription handshake.
  //
  // When sw.js activates a new version (SW_VERSION bump after any
  // frontend deploy), any push subscription the prior SW held can be
  // invalidated by the browser. The new SW broadcasts a `solu:sw-
  // activated` message on activate; we catch it here and re-run the
  // subscription so the user keeps receiving notifications without a
  // manual bell re-toggle. No-ops outside cloud mode via resubscribe's
  // own gate.
  useEffect(() => {
    if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) return;
    const onMsg = (ev: MessageEvent) => {
      const data = ev.data as { type?: string } | null;
      if (data?.type === 'solu:sw-activated') {
        void useStore.getState().resubscribePush();
      }
    };
    navigator.serviceWorker.addEventListener('message', onMsg);
    return () => navigator.serviceWorker.removeEventListener('message', onMsg);
  }, []);

  // Subscribe to the daemon's UI-event SSE stream. This is how tool
  // calls from Claude Code (via our MCP server) reach the browser —
  // e.g. `preview_file(~/.env)` → daemon publishes `open-file` → we
  // dispatch to openFileHere. EventSource auto-reconnects on drop.
  //
  // Depends on `activeBackend` so that in dual-backend router mode
  // the SSE re-subscribes to the newly-active daemon after
  // switchBackend. Without this dep, a backend swap would leave the
  // SSE still pointing at the old daemon and the UI would miss
  // tool-call events from the new one.
  const activeBackend = useStore((s) => s.activeBackend);
  // Bumped to force the SSE effect to tear down and rebuild the
  // EventSource with a freshly-built URL. Needed because EventSource
  // bakes its URL (including the cloud relay ticket) at construction
  // and reuses it for every native auto-reconnect — so a fatal close
  // (e.g. 401 after the token expired) would otherwise loop forever on
  // the spent ticket with no recovery.
  const [sseEpoch, setSseEpoch] = useState(0);
  // Coalesce update toasts across the two update channels. A desktop
  // release bumps BOTH the daemon binary and the shell installer in the same
  // manifest, so the daemon fires `update-available` AND `shell-update-
  // available` on one 30s poll tick (updater.go:443-447). Each event used to
  // render its own near-identical "↑ New version/installer X" toast — the
  // bottom-right notification appearing to fire twice. The store's
  // consecutive-duplicate dedup (store.ts toast()) can't catch it because
  // the messages differ. This ref remembers the version we last toasted
  // (+when); a second event for the same version inside the window is
  // suppressed. Lives in the component body so it survives SSE reconnects;
  // different versions still each get a toast.
  const updateToastRef = useRef<{ version: string; at: number } | null>(null);
  const sseBackoffRef = useRef(new RebuildBackoff());
  const UPDATE_TOAST_COALESCE_MS = 30_000;
  useEffect(() => {
    if (machineAccess.blocked) return;
    const eventScope=currentRoutingScope();
    const eventActive=()=>eventScope.key===currentRoutingScope().key;
    const es = api.openUiEvents();
    // Rebuild on fatal closure. EventSource fires onerror on every
    // transient drop too (it then auto-reconnects with the SAME url,
    // which is fine — the token is still valid), so only act when the
    // browser has given up (readyState === CLOSED). The wait backs off
    // (2 s doubling to 30 s, lib/rebuildBackoff) so a computer that refuses
    // this device or is down isn't asked every 2 s.
    let rebuildTimer: number | undefined;
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED && rebuildTimer === undefined) {
        noteUiEventsSseFailure();
        rebuildTimer = window.setTimeout(() => setSseEpoch((n) => n + 1), sseBackoffRef.current.fail());
      }
    };
    // The private relay channel came back: don't sit out the rest of the wait.
    const onRelayStatus = (e: Event) => {
      if (!(e as CustomEvent<{ reachable: boolean }>).detail.reachable || rebuildTimer === undefined) return;
      window.clearTimeout(rebuildTimer);
      sseBackoffRef.current.reset();
      setSseEpoch((n) => n + 1);
    };
    window.addEventListener(PRIVATE_RELAY_STATUS_EVENT, onRelayStatus);
    // Every time the SSE stream (re)opens — fresh connect on mount
    // AND every auto-reconnect after a drop — pull the daemon's
    // cached /api/update/status. Reason: update-available is a
    // transition-only event (daemon/updater.go:197), and the SSE bus
    // doesn't replay events fired while we were disconnected. So if
    // the daemon detected a new version during any gap (its own
    // startup poll before EventSource connects, a network blip, or
    // the auto-updater's own restart) the UI would otherwise stay
    // stuck on the previous status until the next Ctrl+R bootstrap.
    // A single extra GET on every (re)connect closes that window.
    //
    // Also refresh schedules: another SSE-driven state that silently
    // drifts if schedules-changed fires during a disconnect.
    es.onopen = () => {
      sseBackoffRef.current.reset();
      if(!eventActive())return;
      const st = useStore.getState();
      // Pane runtime-state — especially the native `agentMidTurn` flag that
      // drives the Focus "Thinking…" spinner — rides the transition-only
      // `pane-state` SSE event, which is edge-fired (the daemon emits it on a
      // CHANGE, then never again). A settle (working→done) that fires while SSE
      // is briefly dropped (daemon auto-update restart, network blip, WebView
      // reconnect) is lost forever, leaving the spinner stuck on a turn the
      // daemon already finished. refreshPanes is a level resync (mergePaneInfos
      // replaces with the fresh DTO; samePaneInfo compares agentMidTurn) that
      // heals it on every (re)connect. See feedback_sse_transition_event_drift.
      void st.refreshPanes();
      void st.refreshUpdateStatus();
      void st.refreshSchedules();

      // ARIA cap pill resync: aria-status-changed is also a
      // transition-only event, so any fires that landed while SSE was
      // dropped (e.g. during the cloud-suspended outage on 2026-04-26)
      // never reach the store and the pill stays stuck at the
      // pre-drop value. One GET on every (re)connect closes that gap.
      void api.ariaStatus().then(st.setAriaStatusSnapshot).catch(() => {});
      void api.ariaAutoPending().then((res) => {
        for (const p of res.pending) {
          st.upsertAriaAutoPending({
            confirmId: p.id,
            paneId: p.pane_id,
            paneTitle: p.pane_title,
            input: p.input,
            orderId: p.order_id,
            startedAt: Date.now(),
            deadlineAt: p.deadline_at_ms,
            countdownMs: p.auto_countdown_ms,
            status: 'pending',
          });
        }
      }).catch(() => {});
      // Refresh the entitlement verdict on every (re)connect, and let optional
      // features resync their own transition-only events (extensions.ts).
      const entitlements = useEntitlements.getState().load();
      for (const resync of slotList('eventStreamOpened')) resync(entitlements);
    };
    // Belt-and-braces fallback poll. Defense in depth: even if the
    // onopen path regressed, or a malformed frame made us skip the
    // status SSE, this guarantees the badge is never stuck for more
    // than POLL_MS. It's a zero-cost local HTTP read of cached
    // daemon state; no external network. Deliberately rate-limited
    // beyond 60s is NOT the tradeoff we want — the whole point is
    // that a paid user should never have to know a refresh button
    // exists.
    const POLL_MS = 60_000;
    const fallback = window.setInterval(() => {
      const st = useStore.getState();
      // Belt-and-braces level resync for pane runtime-state (the edge-fired
      // `pane-state`/agentMidTurn flag has no other poll fallback) so a stuck
      // Focus spinner self-heals within POLL_MS even if the onopen resync above
      // was itself missed. See feedback_sse_transition_event_drift.
      void st.refreshPanes();
      void st.refreshUpdateStatus();
      void api.ariaStatus().then(st.setAriaStatusSnapshot).catch(() => {});
    }, POLL_MS);
    // Coalesce helper for the two update channels — see updateToastRef.
    const coalesceUpdateToast = (version: string): boolean => {
      const now = Date.now();
      const last = updateToastRef.current;
      if (last && last.version === version && now - last.at < UPDATE_TOAST_COALESCE_MS) {
        return false;
      }
      updateToastRef.current = { version, at: now };
      return true;
    };
    es.onmessage = (ev) => {
      if(!eventActive())return;
      try {
        const msg = JSON.parse(ev.data) as { type: string; path?: string; url?: string; text?: string; paneId?: string; paneTitle?: string; ts?: number; tool?: string; hint?: string; fromState?: string; toState?: string; durationMs?: number; agentMidTurn?: boolean; prompts?: string[]; degraded?: boolean; attention?: string; attentionReason?: string; skipRecent?: boolean };
        const st = useStore.getState();
        // Every daemon-published event also feeds the Activity log. We
        // push unconditionally so the ring buffer reflects the full
        // recent timeline (pane-state, tool-call, schedule fires,
        // toasts); the Activity dialog filters for display.
        st.pushActivity({ ...msg, ts: msg.ts ?? Date.now() });
        if (msg.type === 'aria-switch-workspace' && msg.text) {
          void st.loadWorkspace(msg.text).catch((error) => st.toast(String(error), { kind: 'error' }));
        } else if (msg.type === 'pane-state' && msg.paneId && typeof msg.toState === 'string') {
          // Real-time pane runtime-state from the daemon's 2s detection
          // tick (daemon/activity.go). pushActivity above already fed the
          // ring; here we ALSO apply it to panes[id].state so the quick-
          // reply bar (gated on state === 'choice-prompt') and other
          // state-gated UI flip immediately instead of lagging up to 3s
          // for the next refreshPanes poll. toState uses the same string
          // encoding as PaneRuntimeState (busy/awaiting-input/choice-
          // prompt/recent/idle/exited — daemon/state.go), so it's applied
          // directly with no mapping. The poll stays source-of-truth and
          // self-heals any missed/stale SSE (samePaneInfo keys on state).
          st.applyPaneStateFromSSE(
            msg.paneId,
            msg.toState as import('./types').PaneRuntimeState,
            typeof msg.agentMidTurn === 'boolean' ? msg.agentMidTurn : undefined,
          );
        } else if (msg.type === 'pane-attention' && msg.paneId && typeof msg.attention === 'string') {
          // Real-time "who needs to act" flip from the daemon's attention
          // engine (daemon/attention.go). Applied to panes[id].attention so
          // the notification tick + attention badge react immediately; the
          // 3s poll stays source-of-truth (samePaneInfo keys on attention).
          st.applyPaneAttentionFromSSE(
            msg.paneId,
            msg.attention as NonNullable<import('./types').PaneInfo['attention']>,
            msg.attentionReason,
          );
        } else if (msg.type === 'open-file' && msg.path) {
          st.openFileHere(msg.path);
        } else if (msg.type === 'open-browser' && msg.url) {
          st.openBrowserHere(msg.url);
        } else if (msg.type === 'close-browser') {
          // Agent's close_browser MCP tool — dismiss the single tile; quiet
          // no-op when nothing is open.
          st.closeBrowser();
        } else if (msg.type === 'entitlements-changed') {
          // A new cloud verdict or a personal-view switch (from this window,
          // another one or the phone): re-read it so gated surfaces follow
          // without a reload.
          void useEntitlements.getState().load();
        } else if (msg.type === 'port-up' && msg.url && 'port' in msg) {
          // A pane just brought up a dev server (daemon port scan). Offer
          // one-click preview — the toast itself is the button. '⚡' resolves
          // to the shared bolt icon in ToastHost's central glyph map.
          const port = (msg as typeof msg & {port: import('./types').Port}).port;
          if (port?.webStatus === 'ready') {
            const target = serviceTarget({...port,backend:eventScope.backendPrefix === '/b/win' ? 'win' : eventScope.backendPrefix === '/b/wsl' ? 'wsl' : undefined});
            st.toast(`Preview detected — ${msg.url.replace(/^https?:\/\//, '')}${msg.text ? ` (${msg.text})` : ''}`, {
              kind:'info',icon:'⚡',ttlMs:8000,
              action:{label:'Open preview',run:()=>{if(eventScope.key === currentRoutingScope().key) st.openBrowserHere(target.url,undefined,undefined,target);}},
            });
            void st.refreshPorts();
          }
        } else if (msg.type === 'pane.spawned' && msg.text && msg.paneId) {
          // An agent opened a pane. Announce it with a link to the new pane and
          // keep it in Alerts "Recent", so it is still there after the toast.
          st.toast(msg.text, { kind: 'info', ttlMs: 10_000, paneId: msg.paneId, logToRecent: true });
        } else if (msg.type === 'pane.reviewed' && msg.text && msg.paneId) {
          // An independent review of a pane's work finished (solu-review).
          // Kept in Alerts "Recent"; anything but an approval is a warning.
          const ok = / approved$/.test(msg.text);
          st.toast(msg.text, { kind: ok ? 'info' : 'warn', ttlMs: 10_000, paneId: msg.paneId, logToRecent: true });
        } else if (msg.type === 'remote-device.pending' && msg.text) {
          // A phone or browser asked to use this computer (daemon
          // trusted_devices.go). It is approved here, at the computer, after
          // comparing codes, so a relayed view gets no prompt.
          if (!isRelayedSession(useCloud.getState())) {
            st.toast(remoteDevicePendingText(msg.text), {
              kind: 'warn',
              ttlMs: 0,
              logToRecent: true,
              action: { label: 'Review', run: () => useSettings.getState().openSettings('devices') },
            });
          }
        } else if (msg.type === 'toast' && msg.text) {
          // Resolve daemon glyph prefixes to shared SVGs here so ToastHost,
          // TopbarToast, and Alerts Recent all receive glyph-free copy.
          // `skipRecent` marks results that already live in the Inbox.
          const glyph = splitDaemonToastGlyph(msg.text);
          st.toast(glyph.message, {
            kind: glyph.kind ?? 'info',
            icon: glyph.icon,
            logToRecent: !msg.skipRecent,
          });
        } else if (msg.type === 'pane-prompt' && msg.paneId && Array.isArray(msg.prompts)) {
          // Daemon committed a prompt to this pane's history (typed,
          // voice inject, ARIA, schedule — any transport). The event
          // carries the full newest-first list; refreshPanes also
          // resyncs it level-based from the pane DTO, so fires lost
          // during an SSE gap heal on the next poll.
          st.setPaneLastPrompts(msg.paneId, msg.prompts, eventScope);
        } else if (msg.type === 'aria-observation' && msg.paneId) {
          // Daemon watcher captured a pane's tail at the moment it
          // entered an attention state. Upsert it into paneTails —
          // both the bell and the panel observation cards derive from
          // this map (via useAttentionItems), so this single write
          // lights up both surfaces.
          st.upsertPaneTail({
            paneId: msg.paneId,
            paneTitle: msg.paneTitle ?? '',
            toState: msg.toState ?? 'awaiting-input',
            text: msg.text ?? '',
            ts: msg.ts ?? Date.now(),
          });
        } else if (msg.type === 'aria-auto-needs-confirm' && typeof msg.text === 'string') {
          // Payload is JSON-serialized in .text because uiEvent doesn't
          // carry confirm-specific fields. Parse + dispatch to the
          // autoresponder strip in the ARIA panel.
          try {
            const payload = JSON.parse(msg.text) as {
              id?: string;
              pane_id?: string;
              pane_title?: string;
              input?: string;
              order_id?: string;
              auto_countdown_ms?: number;
              deadline_at_ms?: number;
            };
            if (payload.id) {
              st.upsertAriaAutoPending({
                confirmId: payload.id,
                paneId: payload.pane_id ?? msg.paneId ?? '',
                paneTitle: payload.pane_title ?? msg.paneTitle ?? '',
                input: payload.input ?? '',
                orderId: payload.order_id ?? '',
                startedAt: Date.now(),
                deadlineAt: typeof payload.deadline_at_ms === 'number' ? payload.deadline_at_ms : undefined,
                countdownMs: payload.auto_countdown_ms ?? 5000,
                status: 'pending',
              });
            }
          } catch { /* malformed; ignore */ }
        } else if (msg.type === 'aria-auto-confirm-resolved' && typeof msg.text === 'string') {
          try {
            const payload = JSON.parse(msg.text) as {
              id?: string;
              approved?: boolean;
              error?: string;
              timeout?: boolean;
            };
            if (payload.id) {
              st.resolveAriaAutoPending(payload.id, {
                approved: Boolean(payload.approved),
                error: payload.error,
                timeout: payload.timeout,
              });
            }
            // Optimistically hide the quick-reply bar the instant the
            // approval lands, instead of waiting up to 3s for the next
            // refreshPanes poll to flip pane.state off 'choice-prompt'.
            // Only on a clean approval (approved && no error). msg.paneId
            // is the authoritative top-level SSE field — the daemon sets
            // uiEvent.PaneID for both the LLM-confirm and directAutoFire
            // paths (they share autoEmitter). The 3s poll reconciles to
            // ground truth (samePaneInfo keys on state, so it self-heals).
            if (payload.approved === true && !payload.error && msg.paneId) {
              st.clearChoicePromptOptimistic(msg.paneId);
            }
            // The footer count updates via the aria-status-changed event the
            // daemon now pushes from autoRecordFire — no refetch needed.
          } catch { /* malformed; ignore */ }
        } else if (msg.type === 'aria-status-changed' && typeof msg.text === 'string') {
          // Server-pushed snapshot. Avoids the stale-pill bug where the
          // footer froze while the server moved on — the old
          // refetch-on-confirm-resolved path missed bursts when SSE briefly
          // dropped or the tab was hibernated.
          try {
            const payload = JSON.parse(msg.text) as {
              paused: boolean;
              auto_fires_hr: number;
            };
            st.setAriaStatusSnapshot(payload);
          } catch { /* malformed; ignore */ }
        } else if (msg.type === 'pane-attention-summary') {
          // An LLM-graded attention summary landed for a needs-user pane.
          // Low frequency (one per prompt, capped hourly) — a level resync
          // of the pane list picks up the new DTO fields.
          void st.refreshPanes();
        } else if (msg.type === 'aria-auto-suppressed') {
          // Paused or burst-limited. Refresh status so the footer pill
          // reflects the current pause state.
          void api.ariaStatus().then((s) => st.setAriaStatusSnapshot(s)).catch(() => {});
        } else if (msg.type === 'schedules-changed') {
          // A run event names its schedule: refetch that row only. No name
          // means the list itself changed (create, edit, delete).
          if (msg.text) void st.refreshSchedule(msg.text);
          else void st.refreshSchedules();
        } else if (uiEventHandler(msg.type)) {
          // Events of optional features (extensions.ts).
          uiEventHandler(msg.type)!(msg);
        } else if (msg.type === 'schedule-firing-soon') {
          playVoice('schedule-soon');
        } else if (msg.type === 'schedule-fired') {
          playVoice('schedule-fired');
        } else if (msg.type === 'update-available') {
          // Daemon detected a newer version on its 30s poll. Refresh
          // the cached status so the topbar badge appears without the
          // user having to navigate or reload. Then surface a toast +
          // short voice cue ("update available") so the user notices
          // even if their eyes are on a terminal pane. Edge-fire logic
          // in the daemon ensures this only triggers on a genuinely new
          // version — see feedback_edge_fire_pitfall.md.
          void (async () => {
            await st.refreshUpdateStatus();
            const fresh = useStore.getState().updateStatus;
            if (fresh?.available && fresh.latest && coalesceUpdateToast(fresh.latest)) {
              useStore.getState().toast(
                `New version ${fresh.latest} available`,
                { kind: 'info', icon: '↑', ttlMs: 8000 },
              );
              playVoice('update-available');
            }
          })();
        } else if (msg.type === 'update-applying') {
          // Edge delivery is best-effort and replayable across local/cloud
          // relays. Confirm it with a fresh level read before entering the
          // shared bounded handoff controller.
          void st.confirmUpdateApplyingEvent(msg.text || undefined);
        } else if (msg.type === 'update-aborted') {
          // Apply failed on the daemon side (download / hash / swap
          // error). Clear the freeze so the user can interact again.
          st.abortUpdateRecovery();
          void st.refreshUpdateStatus();
          st.toast('Update aborted on the daemon side — check logs', { kind: 'error', icon: '⚠' });
        } else if (msg.type === 'shell-update-available') {
          // Manifest's shell section advertises a newer Tauri installer.
          // Same flow as update-available but for the NSIS / MSI / DMG
          // channel: refresh status so the badge appears, toast so the
          // user notices. We reuse the 'update-available' voice cue —
          // a separate cue would be over-engineering for what's a rare
          // event (shell installs are infrequent).
          void (async () => {
            await st.refreshUpdateStatus();
            const fresh = useStore.getState().updateStatus;
            // Not for an installer every launch path would refuse because it
            // carries an older daemon than this one (lib/installerUpdate.ts).
            if (fresh?.shellAvailable && fresh.shellLatest && !installerUpdate(fresh).blockedByDaemon
              && coalesceUpdateToast(fresh.shellLatest)) {
              useStore.getState().toast(
                `New installer ${fresh.shellLatest} available`,
                { kind: 'info', icon: '↑', ttlMs: 8000 },
              );
              playVoice('update-available');
            }
          })();
        }
      } catch {
        /* ignore malformed frames */
      }
    };
    return () => {
      clearInterval(fallback);
      if (rebuildTimer !== undefined) clearTimeout(rebuildTimer);
      window.removeEventListener(PRIVATE_RELAY_STATUS_EVENT, onRelayStatus);
      es.close();
    };
    // cloudSessionToken is a dep so the EventSource is rebuilt with a
    // ticket minted from the fresh JWT whenever Supabase rotates it (~hourly).
    // Without it, long phone/cloud sessions silently lost all
    // event-driven updates after the first post-expiry reconnect. See
    // the WS path (api.wsUrl() per reconnect) which was already immune.
  }, [activeBackend, cloudSessionToken, machineAccess.blocked, sseEpoch]);

  const previouslyBlocked = useRef(machineAccess.blocked);
  useEffect(() => {
    const wasBlocked = previouslyBlocked.current;
    previouslyBlocked.current = machineAccess.blocked;
    if (wasBlocked && !machineAccess.blocked && useStore.getState().bootstrapError) {
      void useStore.getState().bootstrap();
    }
  }, [machineAccess.blocked]);

  // Events ring buffer for debugging. Accessible via
  // `window.__soluDebug.events` or `window.__soluDebug.dumpEvents()`.
  // The ring buffer is always populated; live console logging only
  // happens when `localStorage.solu.debug === '1'` to avoid noise.
  const eventsRef = useRef<Array<{ t: number; paneId: string; title: string; kind: string; detail?: string }>>([]);
  function logEvent(kind: string, pane: { id: string; title: string }, detail?: string) {
    const row = { t: Date.now(), paneId: pane.id, title: pane.title, kind, detail };
    eventsRef.current.push(row);
    if (eventsRef.current.length > 200) eventsRef.current.shift();
    let debug = false;
    try { debug = localStorage.getItem('solu.debug') === '1'; } catch { /* ignore */ }
    if (debug) console.log(`[solu] ${kind} · ${pane.title}${detail ? ' · ' + detail : ''}`);
  }

  // Notification scheduler (attention-transition model).
  //
  // Alerts fire on the transition INTO `attention === 'needs-user'` — the
  // daemon-computed "a human is required" signal (daemon/attention.go) —
  // NOT on the raw runtime state. The distinction is auto-approval: a pane
  // in auto mode entering choice-prompt is usually about to be answered by
  // ARIA within seconds, and alerting on the raw state fired "Approval
  // needed" toasts the auto-approve then made moot (the race the attention
  // engine exists to fix). needs-user carries a reason when the daemon has
  // one (never-auto tool, cautious decline, escalation) and the message uses
  // it. Panes whose DTO has no `attention` yet (older daemon, or the
  // engine's first ~2s) fall back to the old state-transition alerts so
  // nothing is silently swallowed.
  //
  // Fallback: for commands that don't look like Claude, fire on busy → idle
  // after a 3s quiet window (the old behavior, scoped to non-Claude panes).
  //
  // Per-pane state:
  //   lastState     : the most recent server-reported state we've seen
  //   lastAttention : the most recent attention we've seen ('' = never seen)
  //   idleSince : when the pane first became idle after being busy (fallback path)
  //   fired     : whether we've already notified for the current idle run
  //   primed    : true after we've observed at least one state — prevents
  //               firing on the very first observation (e.g. after page reload
  //               the pane might already be awaiting-input from hours ago)
  type SchedEntry = {
    lastState: string;
    lastAttention: string;
    idleSince: number;
    fired: boolean;
    primed: boolean;
  };
  const schedRef = useRef<Record<string, SchedEntry>>({});
  useEffect(() => {
    const dbg = {
      store: useStore,
      sched: schedRef,
      events: eventsRef,
      tickCount: 0,
      dumpEvents: () => {
        console.table(eventsRef.current.map((e) => ({
          ...e,
          ago: ((Date.now() - e.t) / 1000).toFixed(1) + 's',
          when: new Date(e.t).toLocaleTimeString(),
        })));
        return eventsRef.current;
      },
      dumpPanes: () => {
        const s = useStore.getState();
        console.table(Object.values(s.panes).map((p) => ({
          id: p.id.slice(-6),
          title: p.title,
          status: p.status,
          state: p.state,
          activityAge: ((Date.now() - (p.lastActivityMs || 0)) / 1000).toFixed(1) + 's',
          focused: s.focusedPaneId === p.id,
        })));
      },
    };
    (window as unknown as { __soluDebug: typeof dbg }).__soluDebug = dbg;

    function fire(
      pane: { id: string; title: string; cmd: string[]; cwd: string; liveCwd?: string },
      title: string,
      body: string,
      opts: { alsoToast?: boolean; voice?: VoiceEvent } = {},
    ) {
      const s = useStore.getState();
      // When cloud push is active, the service worker's `push` handler is the
      // source of truth for OS notifications on this device. Skip the local
      // new-Notification / sendNotification path to avoid double-notifying
      // for the same daemon-side state transition (Tauri webview + cloud sub
      // = both layers fired, observed 2026-05-07).
      const cloudModeNow = useCloud.getState().mode;
      if (s.notifyEnabled && cloudModeNow === 'cloud' && !isEmbedded()) return;
      const focused = s.focusedPaneId === pane.id;
      const hidden = document.visibilityState === 'hidden';
      const notLooking = !focused || hidden;

      logEvent('fire', pane, `notLooking=${notLooking} focused=${focused} hidden=${hidden}`);

      if (!notLooking) return;

      // Voice line — INSIDE the guards. It used to play at the call sites,
      // which skipped both the cloud dedup and the "you're already looking
      // at this pane" check, so the focused pane spoke "approval needed" at
      // the user mid-click. playVoice keeps its own per-event cooldown.
      if (opts.voice) playVoice(opts.voice);

      // In-app toast — always fire. Cheap, dismissable, non-intrusive.
      if (opts.alsoToast) {
        s.toast(title + ' — ' + body, { kind: 'info', icon: '🔔', ttlMs: 6000 });
        logEvent('toast', pane, title);
      }

      // OS-level notification — requires the 🔔 toggle AND permission.
      // Split path: inside Tauri, the Web Notification API is silently
      // blocked by WebView2, so we route through the tauri-apps
      // plugin-notification which talks to Windows' Action Center
      // directly. Outside Tauri (browser / cloud PWA), use the standard
      // Web Notification API — Tauri plugin IPC isn't available there.
      if (s.notifyEnabled) {
        if (isEmbedded()) {
          sendEmbedNotification(title, body, pane.id);
          logEvent('notification', pane, title);
        } else if (isTauri()) {
          import('@tauri-apps/plugin-notification')
            .then(({ sendNotification }) => {
              sendNotification({ title, body });
              logEvent('notification', pane, title);
            })
            .catch((e) => {
              logEvent('notification-error', pane, String(e));
            });
        } else if (typeof Notification !== 'undefined' && Notification.permission === 'granted') {
          try {
            const n = new Notification(title, { body, tag: pane.id });
            n.onclick = () => {
              window.focus();
              useStore.getState().setFocus(pane.id);
              n.close();
            };
            logEvent('notification', pane, title);
          } catch (e) {
            logEvent('notification-error', pane, String(e));
          }
        }
      }
    }

    const h = setInterval(() => {
      // Skip the per-pane notification scan while the tab/window is
      // hidden — there's no UI to update and OS notifications for a
      // backgrounded tab ride on SSE/push, not this poll. Matches the 3s
      // store poll's visibility gate.
      if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return;

      const dbg = (window as unknown as { __soluDebug: { tickCount: number } }).__soluDebug;
      dbg.tickCount++;

      const s = useStore.getState();
      // No early-return on notifyEnabled anymore — in-app toasts always
      // fire for Claude-specific transitions (cheap, non-intrusive).
      // The notifyEnabled flag only gates the extra *browser* Notification
      // inside fire() — those need OS permission and can be annoying.

      const now = Date.now();
      // paneId → named-workspace map, rebuilt per tick (cheap: a few small
      // arrays). Notification titles lead with "<workspace> · <pane>" — the
      // shared location grammar every surface uses (sw.js push, FCM,
      // AttentionInbox) — so a user with many workspaces can navigate from
      // the alert alone.
      const wsByPane = new Map<string, string>();
      for (const w of s.workspaces) {
        if (w.name === '_current') continue;
        for (const id of w.paneIds ?? []) {
          if (!wsByPane.has(id)) wsByPane.set(id, w.name);
        }
      }
      for (const pane of Object.values(s.panes)) {
        if (pane.status !== 'running') continue;
        const state = pane.state || 'idle';
        const attention = pane.attention; // undefined = old daemon / not yet evaluated
        const st = schedRef.current[pane.id] ?? {
          lastState: state,
          lastAttention: attention ?? '',
          idleSince: 0,
          fired: false,
          primed: false,
        };

        if (!st.primed) {
          // First observation — remember state but don't fire, even if it's
          // already awaiting-input (pane may have been sitting there for hours).
          st.lastState = state;
          st.lastAttention = attention ?? '';
          st.primed = true;
          schedRef.current[pane.id] = st;
          continue;
        }

        const prev = st.lastState;
        const prevAttention = st.lastAttention;
        const label = pane.title || pane.cmd[0];
        const waitingAgent = agentLabel(waitingAgentKind(pane));
        // Shared location grammar: "<workspace> · <pane>", workspace omitted
        // when the pane isn't in a named one. Same string sw.js/FCM render.
        const ws = wsByPane.get(pane.id);
        const loc = ws ? `${ws} · ${label}` : label;
        // Approval detail for the body: WHAT is being asked. Prefer the
        // daemon's attention reason (why the human is needed), then the
        // concrete tool call, then the working directory.
        const hint = pane.pendingApproval;
        const toolDetail = hint ? (hint.summary ? `${hint.toolName}: ${hint.summary}` : hint.toolName) : '';
        const approvalBody = pane.attentionReason || toolDetail || pane.liveCwd || pane.cwd;

        // Log any state change so we can see what the daemon is reporting.
        if (state !== prev) {
          logEvent('state-change', pane, `${prev} → ${state}`);
        }
        if ((attention ?? '') !== prevAttention) {
          logEvent('attention-change', pane, `${prevAttention || '∅'} → ${attention ?? '∅'}`);
        }

        // Tier 1: Claude prompts. When the daemon reports the attention
        // signal, alert ONLY on the transition into needs-user — auto-pending
        // prompts are being handled by auto-approval and stay quiet (the
        // AttentionInbox shows them as "⚡ handling"). The daemon's reason
        // (never-auto tool, cautious decline, escalation) becomes the body so
        // the alert says WHY it needs the user, not just that it does.
        if (attention !== undefined) {
          if (attention === 'needs-user' && prevAttention !== 'needs-user') {
            logEvent('transition-needs-user', pane, `state=${state} reason=${pane.attentionReason ?? ''}`);
            if (state === 'awaiting-input') {
              fire(pane, `${waitingAgent} is waiting · ${loc}`, pane.liveCwd || pane.cwd, { alsoToast: true, voice: 'reply-ready' });
            } else {
              // choice-prompt, or a brief state/attention skew (the two ride
              // different SSE events) — either way a human is required.
              fire(pane, `Approval needed · ${loc}`, approvalBody, { alsoToast: true, voice: 'approval' });
            }
          }
        } else {
          // Legacy fallback (old daemon): fire on transition INTO the state.
          if (state === 'awaiting-input' && prev !== 'awaiting-input') {
            logEvent('transition-awaiting', pane, `prev=${prev}`);
            fire(pane, `${waitingAgent} is waiting · ${loc}`, pane.liveCwd || pane.cwd, { alsoToast: true, voice: 'reply-ready' });
          } else if (state === 'choice-prompt' && prev !== 'choice-prompt') {
            logEvent('transition-choice', pane, `prev=${prev}`);
            fire(pane, `Approval needed · ${loc}`, approvalBody, { alsoToast: true, voice: 'approval' });
          }
        }

        // Tier 2: busy → idle fallback, scoped to non-Claude commands.
        // `hasClaude` catches the case where the user started a shell and
        // ran claude inside — we still want Claude-state transitions to
        // take priority over the generic busy→idle fallback.
        const isClaude =
          pane.cmd[0] === 'claude' ||
          pane.cmd[0]?.endsWith('/claude') ||
          pane.hasClaude === true;
        if (!isClaude) {
          const isBusy = state === 'busy';
          const isIdleish = state === 'idle' || state === 'recent';
          if (isBusy) {
            st.idleSince = 0;
            st.fired = false;
          } else if (isIdleish) {
            if (prev === 'busy') st.idleSince = now;
            if (st.idleSince > 0 && now - st.idleSince >= 3000 && !st.fired) {
              fire(pane, `${loc} went idle`, pane.liveCwd || pane.cwd);
              st.fired = true;
            }
          }
        }

        st.lastState = state;
        st.lastAttention = attention ?? '';
        schedRef.current[pane.id] = st;
      }
    }, 1000);
    return () => clearInterval(h);
  }, []);

  // Global key handlers of optional features (extensions.ts), created once per
  // App so their state survives the hotkeys effect's re-mounts.
  const featureKeysRef = useRef<GlobalKeys[] | null>(null);
  if (featureKeysRef.current === null) featureKeysRef.current = slotList('globalKeys').map((create) => create());

  // Global hotkeys. We run in CAPTURE phase so we preempt xterm: the
  // terminal's keyboard handler would otherwise swallow Alt+letter as
  // a meta-prefixed escape sequence (e.g. Alt+B = word-back in bash).
  //
  // Target filtering: we only skip the shortcut when the event came from
  // one of OUR inputs (dialog fields, rename inputs, DirPicker) — NOT
  // when it came from xterm's hidden helper textarea. Otherwise the
  // shortcuts would stop working the moment the user clicks into a
  // terminal, which is exactly when you want to switch panes.
  useEffect(() => {
    const isOurOwnInput = (el: HTMLElement | null): boolean => {
      if (!el) return false;
      // xterm uses a textarea.xterm-helper-textarea for input capture.
      // Treat that as "terminal", not as an input field we should defer to.
      if (el.classList.contains('xterm-helper-textarea')) return false;
      const tag = el.tagName.toLowerCase();
      if (tag === 'input' || tag === 'textarea') return true;
      if (el.isContentEditable) return true;
      return false;
    };

    // Stricter variant that INCLUDES xterm. Used for the "?" help trigger
    // because `?` is a common character in shells (grep patterns,
    // `git commit` help, `-?` flags, man pages) so we must NOT intercept
    // it when the user is typing in a terminal. Users who want help
    // while focused in a terminal can click the "Press ? for shortcuts"
    // footer in the sidebar, or click anywhere outside the terminal and
    // then press "?".
    const isAnyTextTarget = (el: HTMLElement | null): boolean => {
      if (!el) return false;
      if (el.classList.contains('xterm-helper-textarea')) return true;
      const tag = el.tagName.toLowerCase();
      if (tag === 'input' || tag === 'textarea') return true;
      if (el.isContentEditable) return true;
      return false;
    };

    const featureKeys = featureKeysRef.current ?? [];

    const onKey = (e: KeyboardEvent) => {
      const state = useStore.getState();
      // The hidden grid retains its terminal and editor DOM. Its global
      // shortcuts must not send input or close panes after another view owns it.
      if (state.mainPresentation.kind === 'workspace' && (workspaceMovedTo(state.activeWorkspace) || workspaceTransferring(state.activeWorkspace))) return;
      const target = e.target as HTMLElement | null;
      const ourInput = isOurOwnInput(target);
      // Optional features see each key first (extensions.ts).
      for (const keys of featureKeys) if (keys.keydown(e)) return;

      // Ctrl+Shift+R — hard "refresh the app". Handled explicitly (not via the
      // rebindable dispatcher) because comboFromEvent drops Shift for letter
      // keys, so Ctrl+Shift+R serializes to "ctrl+r" and could never match a
      // "ctrl+shift+r" binding — the old pane.redraw shortcut was dead. Relying
      // on the WebView2 accelerator default is also unreliable: it behaves
      // inconsistently when a Focus pane's composer textarea has focus and can
      // soft-reload the cached shell. reloadApp does a deterministic, cache-
      // busting reload so refresh works the same on every workspace/pane.
      // `e.code === 'KeyR'` + shiftKey distinguishes it from Ctrl+R (shell
      // reverse-search), which we never intercept.
      if (e.ctrlKey && e.shiftKey && !e.altKey && !e.metaKey && e.code === 'KeyR') {
        e.preventDefault();
        e.stopImmediatePropagation();
        useStore.getState().reloadApp();
        return;
      }

      // Rebindable-action dispatcher. Runs BEFORE every hardcoded
      // default branch below so a user override wins. Built from the
      // small REBINDABLE_ACTIONS set in keybindings.ts; defaults route
      // through here too so there's one source of truth for these
      // shortcuts (help, sidebar toggle, new pane, pane zoom, palette
      // actions / files, find-in-pane). Everything else (vim nav,
      // Alt+digit focus, broadcast, workspace cycle) stays hardcoded
      // below. pane.zoom is rebindable for a concrete reason: GPU
      // overlays (NVIDIA and AMD both) grab Alt+Z as an exclusive
      // OS-level hotkey, killing the default with zero visible cause —
      // see useZoomHotkeyWarning for the detection side.
      {
        const combo = comboFromEvent(e);
        if (combo) {
          const custom = useStore.getState().customKeybindings;
          const actionId = resolveCombo(combo, custom);
          if (actionId) {
            // Per-action guards. `help.toggle` fires `?` which is also
            // a common shell character — suppress when ANY text target
            // (including xterm) has focus. `palette.findInPane` only
            // makes sense with a focused pane; `edit.commandLine` sends
            // PTY bytes, so both stay out of our own inputs. Everything
            // else fires from our inputs too AS LONG AS the combo holds a
            // real modifier (Ctrl/Alt/Meta): a chord is an app command,
            // not text entry — Alt+Z must zoom even while the Focus
            // composer has focus. Bare-character combos still defer to
            // inputs, and IME composition never triggers anything.
            const chorded = (e.ctrlKey || e.altKey || e.metaKey) && !e.isComposing;
            let canFire = true;
            if (actionId === 'help.toggle') canFire = !isAnyTextTarget(target);
            else if (actionId === 'palette.findInPane') canFire = !ourInput && !!focusedPaneId;
            else if (actionId === 'edit.commandLine') canFire = !ourInput && !!focusedPaneId; // fires inside xterm, not our dialogs
            else canFire = !ourInput || chorded;
            // "Terminal keys first": plain Ctrl+letter chords go to the
            // terminal (readline/TUI keys) while xterm has focus.
            if (
              canFire &&
              useStore.getState().terminalKeysFirst &&
              target?.classList?.contains('xterm-helper-textarea') &&
              isTerminalCollidingCombo(combo)
            ) canFire = false;
            // Readline word-editing keys (Alt+B/D/T/U) belong to the terminal
            // while it has focus, unless the user bound that combo on purpose.
            if (
              canFire &&
              READLINE_ALT_COMBOS.has(combo) &&
              custom[actionId] !== combo &&
              target?.classList?.contains('xterm-helper-textarea')
            ) canFire = false;

            if (useStore.getState().mainPresentation.kind === 'owner' && ['pane.new','browser.new','pane.zoom','pane.reopenClosed'].includes(actionId)) canFire = false;
            // Tasks is team-only: leave Alt+A to the terminal for everyone else.
            if (canFire) {
              e.preventDefault();
              e.stopImmediatePropagation();
              switch (actionId) {
                case 'help.toggle':
                  setHelpOpen((v) => !v);
                  break;
                case 'view.sidebar':
                  toggleSidebar();
                  break;
                case 'pane.new':
                  setDialog({ open: true, split: null });
                  break;
                case 'browser.new':
                  // Toggle: Alt+B opens the single internal browser, Alt+B
                  // again closes it (single-instance — see store.toggleBrowser).
                  useStore.getState().toggleBrowser();
                  break;
                case 'system.toggle':
                  useStore.getState().setSystemPageOpen(!useStore.getState().systemPageOpen);
                  break;
                case 'pane.realign':
                  // Re-fit + re-assert width on every visible pane — the
                  // programmatic open/close-browser trick that unblocks
                  // gibberish after an update or a cross-device takeover.
                  useStore.getState().realignAllPanes();
                  break;
                case 'pane.zoom':
                  useStore.getState().toggleZoom();
                  break;
                case 'palette.actions':
                  setPalette({ open: true, mode: 'actions' });
                  break;
                case 'palette.files':
                  setPalette({ open: true, mode: 'files' });
                  break;
                case 'palette.findInPane':
                  window.dispatchEvent(new CustomEvent('solu:pane-find-open', {
                    detail: { paneId: focusedPaneId },
                  }));
                  break;
                case 'pane.reopenClosed':
                  void useStore.getState().reopenClosedPane();
                  break;
                case 'pane.lastTurnDiff': {
                  const st = useStore.getState();
                  const fp = st.focusedPaneId ? st.panes[st.focusedPaneId] : undefined;
                  if (fp?.turn?.last) st.openTurnDiff(fp.id);
                  else st.toast('No finished agent turn in the focused pane yet', { kind: 'info', ttlMs: 2500 });
                  break;
                }
                case 'edit.commandLine':
                  // Ctrl+X Ctrl+E — readline/zsh "edit current command line
                  // in $EDITOR". Sent as raw bytes to the focused pane's PTY;
                  // the shell opens $EDITOR with the buffered line and runs it
                  // on save+exit. We capture Alt+G before xterm so it never
                  // reaches the shell as a meta-g prefix.
                  useStore.getState().sendToFocused('\x18\x05');
                  break;
              }
              return;
            }
          }
        }
      }

      // Alt+Shift+B — open Broadcast dialog (send one prompt to many
      // panes). Alt+Shift avoids collision with browser/OS Ctrl+B.
      // Captures when the user is NOT already typing somewhere so
      // xterm-side "B" in a terminal is never eaten.
      if (
        !ourInput &&
        e.altKey && e.shiftKey &&
        !e.ctrlKey && !e.metaKey &&
        (e.key === 'B' || e.key === 'b')
      ) {
        e.preventDefault();
        e.stopImmediatePropagation();
        setBroadcastOpen(true);
        return;
      }

      // Ctrl+K / Ctrl+P / Ctrl+F are routed through the rebindable-
      // action dispatcher above. Users can remap them via the
      // Keybindings dialog; the dispatcher routes both defaults and
      // overrides through one path.

      // Ctrl+Shift+Arrow — discoverable alias for Alt+H/J/K/L spatial nav.
      // Alt+letters work for vim users; arrows are what everyone else
      // reaches for. Shift is required so we don't clobber the shell's
      // Ctrl+Arrow (word-jump in readline).
      if (
        !ourInput &&
        (e.ctrlKey || e.metaKey) &&
        e.shiftKey &&
        !e.altKey &&
        (e.key === 'ArrowLeft' || e.key === 'ArrowRight' ||
         e.key === 'ArrowUp' || e.key === 'ArrowDown')
      ) {
        if (focusedPaneId) {
          const dir =
            e.key === 'ArrowLeft' ? 'left' :
            e.key === 'ArrowRight' ? 'right' :
            e.key === 'ArrowUp' ? 'up' : 'down';
          const next = pickDirectionalPane(focusedPaneId, dir);
          if (next) {
            e.preventDefault();
            e.stopImmediatePropagation();
            setFocus(next);
            focusPaneTextarea(next);
            return;
          }
        }
      }

      // Alt+Shift+↑/↓ — cycle through saved workspaces in the left
      // panel. Mirrors Ctrl+Shift+Arrow's directional semantics but
      // one hierarchy level up (workspaces, not panes). Checked BEFORE
      // the Alt-only branches below so that Alt+Shift+↓ doesn't fall
      // through to the Alt+↓ "split down" dialog — Shift is the
      // disambiguator. Same ordering logic as Sidebar.tsx: wsOrder
      // custom order first, unknown names appended alphabetically.
      if (
        e.altKey &&
        e.shiftKey &&
        !e.ctrlKey &&
        !e.metaKey &&
        !ourInput &&
        (e.key === 'ArrowUp' || e.key === 'ArrowDown')
      ) {
        if (cycleWorkspace(e.key === 'ArrowDown' ? 1 : -1, 'keyboard')) {
          e.preventDefault();
          e.stopImmediatePropagation();
          return;
        }
      }

      if (!e.altKey) return;
      if (ourInput) return;

      // Single `handled` flag so we can preventDefault + stop at the end
      // for ANY matched shortcut. Without stopImmediatePropagation xterm
      // would still receive the Alt+letter and send an escape sequence.
      // Alt+N, Alt+Z and Ctrl+Q are routed through the rebindable-action
      // dispatcher above (pane.new / pane.zoom / view.sidebar).
      // Deliberately excluded from this Alt-block so a user's override
      // wins and we don't double-fire when the defaults still match.
      let handled = false;
      const k = e.key.toLowerCase();
      if (e.key === 'ArrowRight' && !e.shiftKey) {
        setDialog({ open: true, split: 'horizontal' });
        handled = true;
      } else if (e.key === 'ArrowDown' && !e.shiftKey) {
        // Shift-held Alt+↓ is reserved for workspace-next (handled
        // above); guarding here in case that branch's early-return is
        // ever broken by refactors.
        setDialog({ open: true, split: 'vertical' });
        handled = true;
      } else if (k === 'w') {
        if (focusedPaneId) {
          const s = useStore.getState();
          handled = true;
          if (s.files[focusedPaneId]) {
            s.closeFileInLayout(focusedPaneId);
          } else if (s.browsers[focusedPaneId]) {
            s.closeBrowserInLayout(focusedPaneId);
          } else if (s.panes[focusedPaneId]) {
            void confirmClosePane(s.panes[focusedPaneId]).then((ok) => {
              if (ok) void killPane(focusedPaneId);
            });
          }
        }
      } else if (/^[1-9]$/.test(e.key)) {
        const idx = parseInt(e.key, 10) - 1;
        const order = paneIdsInOrder(useStore.getState().layout);
        if (order[idx]) {
          setFocus(order[idx]);
          focusPaneTextarea(order[idx]);
          handled = true;
        }
      } else if (e.key === '[' || e.key === ']') {
        const order = paneIdsInOrder(useStore.getState().layout);
        if (order.length > 0) {
          const cur = focusedPaneId ? order.indexOf(focusedPaneId) : -1;
          const delta = e.key === ']' ? 1 : -1;
          const next = order[(cur + delta + order.length) % order.length];
          setFocus(next);
          focusPaneTextarea(next);
          handled = true;
        }
      } else if (k === 'h' || k === 'j' || k === 'k' || k === 'l') {
        // Spatial focus — pick the nearest pane in the requested direction
        // by comparing the focused pane's rect to all others. Uses the DOM
        // as the source of truth so it respects split ratios and zoom.
        if (focusedPaneId) {
          const dir = k === 'h' ? 'left' : k === 'l' ? 'right' : k === 'k' ? 'up' : 'down';
          const next = pickDirectionalPane(focusedPaneId, dir);
          if (next) {
            setFocus(next);
            focusPaneTextarea(next);
            handled = true;
          }
        }
      } else if (e.key === 'Enter') {
        if (focusedPaneId) {
          void useStore.getState().cloneFocused('horizontal');
          handled = true;
        }
      }

      if (handled) {
        e.preventDefault();
        // Prevents xterm's keydown listener (same phase, later listener)
        // from ALSO reacting — otherwise Alt+L would move focus AND send
        // ESC+L to the shell. stopPropagation isn't enough because the
        // capture-phase listener on xterm's element may already have run.
        e.stopImmediatePropagation();
      }
    };
    const onKeyUp = (e: KeyboardEvent) => {
      for (const keys of featureKeys) keys.keyup(e);
    };
    const onBlur = () => {
      for (const keys of featureKeys) keys.blur();
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === 'hidden') onBlur();
    };

    // Capture phase so we beat xterm's keyboard handler, which is
    // attached to its own host element (bubble phase) but aggressively
    // claims Alt+letter as a meta escape sequence.
    window.addEventListener('keydown', onKey, { capture: true });
    window.addEventListener('keyup', onKeyUp, { capture: true });
    window.addEventListener('blur', onBlur);
    document.addEventListener('visibilitychange', onVisibilityChange);
    return () => {
      window.removeEventListener('keydown', onKey, { capture: true });
      window.removeEventListener('keyup', onKeyUp, { capture: true });
      window.removeEventListener('blur', onBlur);
      document.removeEventListener('visibilitychange', onVisibilityChange);
      for (const keys of featureKeys) keys.detach();
    };
  }, [focusedPaneId, killPane, setFocus]);

  // Bridge the Pane's right-click context menu to the NewPaneDialog.
  // The menu items (New session / Split right / Split down) fire a
  // CustomEvent with { split } in the detail; we pick it up here and
  // open the dialog. Avoids prop-drilling a setter down through
  // PaneGrid + every Split node to reach Pane.
  useEffect(() => {
    const onOpen = (e: Event) => {
      const detail = (e as CustomEvent<{ split: 'horizontal' | 'vertical' | null }>).detail;
      setDialog({ open: true, split: detail?.split ?? null });
    };
    window.addEventListener('solu:open-pane-dialog', onOpen as EventListener);
    return () => window.removeEventListener('solu:open-pane-dialog', onOpen as EventListener);
  }, []);

  // CommandPalette → App bridge for the sidebar toggle. toggleSidebar
  // closes over the sidebarRef created in App, so we can't expose it
  // through the store without re-plumbing. A CustomEvent keeps the
  // palette decoupled from App internals (same shape as the pane
  // dialog bridge above).
  useEffect(() => {
    const on = () => toggleSidebar();
    const showOwners = () => {sidebarRef.current?.expand(); setSidebarCollapsed(false); setDrawerOpen(true);};
    const selected = () => setDrawerOpen(false);
    window.addEventListener('solu:show-task-owners',showOwners);
    window.addEventListener('solu:owner-selected',selected);
    window.addEventListener('solu:toggle-sidebar', on);
    return () => {window.removeEventListener('solu:toggle-sidebar', on);window.removeEventListener('solu:show-task-owners',showOwners);window.removeEventListener('solu:owner-selected',selected);};
  }, []);

  // Alt + mouse wheel — cycle workspaces (same as Alt+Shift+↑/↓).
  // Capture + non-passive so we can preventDefault before the event
  // reaches Pane.tsx's own wheel handler (which forwards to xterm /
  // mouse-tracking CSI). Throttled because a single wheel notch emits
  // multiple events — 250ms matches a comfortable one-switch-per-flick
  // feel without feeling laggy.
  useEffect(() => {
    let lastAt = 0;
    const onWheel = (e: WheelEvent) => {
      if (!e.altKey) return;
      if (e.shiftKey || e.ctrlKey || e.metaKey) return;
      if (e.deltaY === 0) return;
      const now = Date.now();
      if (now - lastAt < 250) {
        e.preventDefault();
        e.stopImmediatePropagation();
        return;
      }
      if (cycleWorkspace(e.deltaY > 0 ? 1 : -1, 'alt-wheel')) {
        lastAt = now;
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    };
    window.addEventListener('wheel', onWheel, { capture: true, passive: false });
    return () => window.removeEventListener('wheel', onWheel, { capture: true });
  }, []);

  return (
    <div className="app">
      {/* Overlays of optional features (extensions.ts), first in the root. */}
      {slotList('rootOverlays').map((Overlay, i) => <Overlay key={i} />)}
      {/* RequiredUpdateModal before UpdateOverlay: same z-index, so if both
          ever render the later sibling (the applying overlay) paints on top —
          "Updating…" is the truthful state while a forced swap runs. */}
      <RequiredUpdateModal />
      <UpdateOverlay />
      <ScheduleHistoryDeepLink />
      {/* Inbox panel host — always mounted for the same reason as the deep
          link above: its entry points (topbar bubble, footer chip, sidebar
          chip, notification taps) live in chrome phones may not have. */}
      <InboxHost />
      {/* New-schedule dialog host — same always-mounted rationale; catches
          `solu:open-schedule-dialog` even when the sidebar section is hidden. */}
      <ScheduleDialogHost />
      {/* Hosts registered by optional features — same always-mounted
          rationale (see extensions.ts). */}
      {slotList('appHosts').map((Host, i) => <Host key={i} />)}
      {/* Row 1 (the .app grid's `auto` banner row). Both in-flow advisories
          share this single wrapper so neither becomes an extra grid child
          that would shift the Topbar/pane-grid into the wrong rows. */}
      <div className="app-banner-row">
        <DaemonHealthBanner />
        <DegradedBanner />
        <ClaudeInstallBanner hidden={firstRunReady && (welcomeOpen || checklistOpen)} />
        {teamFeatures && <MuseInstallBanner />}
        {showEmbedChrome('phonePairing') && <PhoneAccessBanner />}
        {showEmbedChrome('shellUpdate') && <ShellUpdateBanner />}
        <UpdateBanner />
      </div>
      <Topbar
        onOpenHelp={() => setHelpOpen(true)}
        onOpenGettingStarted={openGettingStarted}
        sidebarCollapsed={sidebarCollapsed}
        onToggleSidebar={toggleSidebar}
      />
      <div ref={mainRef} className={`main ${sidebarCollapsed ? 'sidebar-collapsed' : ''}`}>
        <PanelGroup
          direction="horizontal"
          // autoSaveId is bumped (v2 → v3) so widths saved under the old
          // 160px default reset once to the new 260px default. A user's
          // post-drag width saves normally under the new key afterwards.
          autoSaveId="solu.sidebar.v3"
          className="main-panels"
        >
          <Panel
            ref={sidebarRef}
            id="sidebar"
            order={1}
            // Pixel-driven: default=260px, min=220px, max=480px. Converted
            // to percentages against window.innerWidth at mount. See the
            // SIDEBAR_*_PX constants at the top of this file for rationale.
            defaultSize={sidebarSizePct.defaultSize}
            minSize={sidebarSizePct.minSize}
            maxSize={sidebarSizePct.maxSize}
            collapsible
            collapsedSize={0}
            onCollapse={() => setSidebarCollapsed(true)}
            onExpand={() => setSidebarCollapsed(false)}
            className="sidebar-panel"
          >
            <Sidebar onToggleCollapse={toggleSidebar} onOpenHelp={() => setHelpOpen(true)} />
          </Panel>
          {/* When collapsed this is just a quiet resize seam (double-click or
              drag to bring the sidebar back) — the loud accent ‹›› chip that
              used to live here is gone, since the always-visible topbar
              IconPanelLeft toggle is now the primary way back. */}
          <PanelResizeHandle
            className={`sidebar-resize-handle ${sidebarCollapsed ? 'collapsed' : ''}`}
            onDoubleClick={toggleSidebar}
            title={sidebarCollapsed ? 'Show sidebar — double-click or drag (Ctrl+Q)' : 'Drag to resize · double-click to collapse (Ctrl+Q)'}
          />
          <Panel id="grid" order={2} minSize={30} className="grid-panel">
            <WorkspaceHandoffBoundary><div className="grid-area" style={{position:'relative'}}>

              <div style={{width:'100%',height:'100%',display:ownerVisible ? 'none' : undefined}} {...(ownerVisible ? {inert:''} : {})} aria-hidden={ownerVisible || undefined}>
              {machineAccess.blocked ? (
                <MachineAccessDeniedState />
              ) : loading ? (
                <WorkspaceLoadingState />
              ) : bootstrapError && awaitingApproval ? (
                <DeviceApprovalPendingState onRetry={bootstrap} />
              ) : bootstrapError ? (
                <BootstrapErrorState error={bootstrapError} onRetry={bootstrap} />
              ) : !hasLayout ? (
                <EmptyWorkspaceState
                  creating={creatingSession !== null}
                  creatingKind={creatingSession}
                  hosts={orchestratorHosts}
                  onCreateShell={() => void createEmptyWorkspaceSession('shell')}
                  onCreateClaude={() => void createEmptyWorkspaceSession('claude')}
                  onCreateCodex={() => void createEmptyWorkspaceSession('codex')}
                  onCreateOrchestrator={() => void createEmptyWorkspaceSession('orchestrator')}
                  onCreateOrchestratorCodex={() => void createEmptyWorkspaceSession('orchestrator-codex')}
                  onOpenDialog={() => setDialog({ open: true, split: null })}
                />
              ) : (
                <ErrorBoundary label="PaneGrid">
                  <PaneGrid />
                </ErrorBoundary>
              )}
            </div></div>
            </WorkspaceHandoffBoundary>
            <StatusBar />
          </Panel>
        </PanelGroup>
      </div>

      {/* Owns every live terminal (active + parked) via portals into
          persistent hosts — must render unconditionally so parked panes
          survive blank phases / empty workspaces. See WarmPaneLayer. */}
      <ErrorBoundary label="WarmPaneLayer">
        <WarmPaneLayer />
      </ErrorBoundary>

      <NewPaneDialog
        open={dialog.open}
        splitDirection={dialog.split}
        onClose={() => setDialog({ open: false, split: null })}
      />

      <HotkeyHelp
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
      />
      <SupportBundleDialog open={supportBundleOpen} onClose={() => setSupportBundleOpen(false)} />
      {showEmbedChrome('welcome') && <WelcomeOverlay open={welcomeOpen && firstRunReady} onClose={dismissWelcome} />}
      <SetupChecklist open={checklistOpen && firstRunReady} onClose={dismissChecklist} />
      <AgentInstallPrompt />
      <DeviceApprovalPrompt />
      <PrivateRelayIdentityWarning />
      <PrivateRelayStatus />
      {showEmbedChrome('whatsNew') && <WhatsNewToast />}
      <Suspense fallback={null}>
        {settingsOpen && <SettingsModal />}
        {activityOpen && <ActivityDialog open onClose={() => setActivityOpen(false)} />}
        {systemPageOpen && <SystemPage onClose={() => setSystemPageOpen(false)} />}
        {palette.open && (
          <CommandPalette
            open
            initialMode={palette.mode}
            initialQuery={palette.query}
            onClose={() => setPalette((p) => ({ ...p, open: false }))}
            onOpenHelp={() => setHelpOpen(true)}
            onOpenNewPane={(split) => setDialog({ open: true, split })}
            onOpenBroadcast={() => setBroadcastOpen(true)}
            onOpenActivity={() => setActivityOpen(true)}
            onOpenFeedback={() => useStore.getState().setFeedbackOpen(true)}
          />
        )}
      </Suspense>
      {broadcastOpen && <BroadcastDialog onClose={() => setBroadcastOpen(false)} />}
      <ScrollDiagOverlay />
      <ImeTraceOverlay />

      {isMobile && (
        <>
          <MobileDrawer
            open={drawerOpen}
            onOpen={() => setDrawerOpen(true)}
            onClose={() => setDrawerOpen(false)}
            onOpenHelp={() => setHelpOpen(true)}
          />
          <MobileTabBar
            onOpenDrawer={() => setDrawerOpen(true)}
            drawerHasAttention={attentionOffscreen}
            keybarOpen={keybarOpen}
            onToggleKeybar={() => setKeybarOpen((v) => !v)}
          />
          <MobileQuickReply />
        </>
      )}
      <MobileKeyBar visible={isMobile && keybarOpen} onHide={() => setKeybarOpen(false)} />
      {isMobileLandscape && (
        <div className="portrait-lock-overlay" role="alert" aria-live="polite">
          <div className="portrait-lock-icon" aria-hidden>↻</div>
          <div className="portrait-lock-title">Rotate your phone upright</div>
        </div>
      )}
      <ToastHost />
      <DialogHost />
      <BannerSetupGate />
      {showEmbedChrome('phonePairing') && <PhoneAccessPairPanelGate />}
    </div>
  );
}

// Pulls the modal visibility off the store. Separate from App so
// App doesn't re-render every time the modal flips (would affect
// pane stability). Renders nothing when bannerSetupOpen is false,
// which is the default state for every page load.
function BannerSetupGate() {
  const open = useStore((s) => s.bannerSetupOpen);
  const setOpen = useStore((s) => s.setBannerSetupOpen);
  if (!open) return null;
  return <BannerSetupModal onClose={() => setOpen(false)} />;
}
