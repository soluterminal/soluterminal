
import { ContextPill } from './ContextPill';
import { OpenShinoque } from './OpenShinoque';
import { showEmbedChrome } from '../embedChrome';
import { InstallButton } from './InstallButton';
import { GridLayoutPicker } from './GridLayoutPicker';
import { AlertsMenu } from './AlertsMenu';
import { AttentionBell } from './AttentionBell';
import { RefreshViewButton } from './RefreshViewButton';
import { TopbarToast } from './TopbarToast';
import { useIsMobileView } from './useIsMobileView';
import { useAttentionBadge } from './AttentionInbox';
import { HelpMenu } from './HelpMenu';
import { IconNewWindow, IconPanelLeft, IconMessage, IconSettings, IconGlobe } from './ui/icons';
import { useStore } from '../store';
import { openInbox } from './inbox/InboxPanel';
import { inboxCountLabel, inboxFeed } from '../lib/inboxFeed';
import { useFeedbackInbox } from '../lib/useFeedbackInbox';
import { slotList, useInboxReports } from '../extensions';
import { useSettings } from '../settings';
import { multiWindowAvailable, openWorkspaceWindow } from '../tauri-browser';
import { mountedAssetURL } from '../previewBoot';
import { WindowControls, isTauriShell } from './WindowControls';
import { useOperatorFeatures } from '../entitlements';
import { PersonalViewPill } from './PersonalViewPill';

// Topbar doubles as the custom window title bar in Tauri (decorations
// disabled in lib.rs to avoid a glaring white OS bar). The outer div
// is a Tauri drag region so the user can drag the window by it; the
// right side hosts min/max/close. In a browser tab the window
// controls are hidden — the OS chrome handles that.
//
// Update affordance: when an update is available a LOUD accent pill appears
// here (always visible, no layout shift) AND the bottom statusbar chip lights
// up — two prominent places so it's not missed. The old "cross-screen jump"
// bug is gone because the popover anchors to whichever trigger opened it (this
// pill drops the details DOWN; the bottom chip opens them UP). An escalation
// banner (UpdateBanner) is the third, loudest tier if both get ignored.

// The message-bubble button — the inbox's front door. Badge = unread count
// (accent), warn-tinted when something needs the user. Rendered in the
// desktop toolbar AND standalone on mobile (the toolbar is desktop-only,
// but phones need an inbox entry that isn't buried in the drawer).
function InboxBubble() {
  const schedules = useStore((s) => s.schedules);
  const feedback = useFeedbackInbox();
  const agentReports = useInboxReports();
  const items = inboxFeed(schedules, feedback, agentReports);
  const unread = items.filter((i) => i.unread);
  const needs = unread.some((i) => i.needsYou);
  const title = unread.length > 0
    ? `Inbox — ${inboxCountLabel(unread.length)} unread${needs ? ', some need you' : ''}`
    : 'Inbox — reports from scheduled tasks, feedback updates, send feedback';
  return (
    <button
      type="button"
      className="chrome-btn topbar-inbox-btn"
      onClick={() => openInbox()}
      title={title}
      aria-label={title}
      data-tauri-drag-region={false}
    >
      <IconMessage />
      {unread.length > 0 && (
        <span className={`topbar-inbox-badge${needs ? ' is-needs' : ''}`} aria-hidden="true">
          {unread.length > 9 ? '9+' : unread.length}
        </span>
      )}
    </button>
  );
}

export function Topbar({
  onOpenHelp,
  onOpenGettingStarted,
  sidebarCollapsed,
  onToggleSidebar,
}: {
  /** Opens the keyboard-shortcut sheet (the Help menu's "Keyboard shortcuts"). */
  onOpenHelp?: () => void;
  /** Re-opens the first-run welcome tour (the Help menu's "Getting started"). */
  onOpenGettingStarted?: () => void;
  sidebarCollapsed?: boolean;
  onToggleSidebar?: () => void;
} = {}) {
  const tauri = isTauriShell();
  const isMobile = useIsMobileView();
  // Task Owners are team-only on the public beta; dictation needs a free
  // account (cloud voice).
  const teamFeatures = useOperatorFeatures();

  // When the sidebar is hidden, surface any pane that needs attention (approval
  // / waiting for input) as a dot on the toggle — otherwise it's invisible.
  const { count: attnCount, hasChoice: attnHasChoice } = useAttentionBadge();
  const showToggleAttn = !!sidebarCollapsed && attnCount > 0;
  // Internal browser (single-instance): the topbar button opens it and, when
  // it's already open, closes it — the same toggle as Alt+B. `browserOpen` is a
  // primitive selector so it re-renders only when the browser opens/closes.
  const browserOpen = useStore((s) => Object.keys(s.browsers).length > 0);
  const toggleBrowser = useStore((s) => s.toggleBrowser);
  // The old top-bar update pill is gone — an available update now shows the
  // full-width UpdateBanner immediately (App.tsx) instead of pill-then-banner
  // double-signaling; the status-bar chip stays as the quiet persistent entry
  // point after a dismiss.

  return (
    <div className="topbar" data-tauri-drag-region>
      {/* Zone over the SIDEBAR column: app mark + sidebar toggle. When the
          sidebar is open (is-aligned) this zone spans its width (--content-left)
          so the context cluster that follows begins at the content column's left
          edge — above the first pane — and tracks the sidebar as it's dragged.
          The toggle itself stays pinned far-left so it survives the sidebar
          collapsing. Collapsed / mobile: natural width, normal spacing. */}
      <div
        className={`topbar-zone-sidebar${!isMobile && !sidebarCollapsed ? ' is-aligned' : ''}`}
        data-tauri-drag-region
      >
        {showEmbedChrome('appIcon') && <img
          className="topbar-app-icon"
          src={mountedAssetURL('icon-mark-192.png')}
          alt=""
          draggable={false}
          data-tauri-drag-region
        />}
        {!isMobile && onToggleSidebar && (
          <button
            type="button"
            className="chrome-btn topbar-sidebar-toggle"
            data-tauri-drag-region={false}
            onClick={onToggleSidebar}
            aria-label={sidebarCollapsed ? 'Show sidebar' : 'Hide sidebar'}
            aria-pressed={!sidebarCollapsed}
            title={sidebarCollapsed ? 'Show sidebar (Ctrl+Q)' : 'Hide sidebar (Ctrl+Q)'}
          >
            <IconPanelLeft size={17} />
            {showToggleAttn && (
              <span
                className={`topbar-sidebar-toggle-dot ${attnHasChoice ? 'is-choice' : 'is-awaiting'}`}
                aria-hidden="true"
              />
            )}
          </button>
        )}
      </div>
      {/* Context pill — merges workspace identity with the hosting (Local/Cloud)
          switch into one interactive control. Replaces the old passive
          workspace chip + standalone Environment chip, and the footer's `ws:`
          duplicate. Opts out of the drag region so it's clickable. */}
      <div className="topbar-context-slot" data-tauri-drag-region={false}>
        <ContextPill />
      </div>

      <PersonalViewPill />
      {showEmbedChrome('install') && <InstallButton />}
      {/* Center of the title bar: a drag region that doubles as the inline home
          for "top center" notifications (see TopbarToast). */}
      <TopbarToast />
      {/* Utility toolbar — the global app controls, unified into one consistent
          icon row (was three mismatched button styles in the sidebar footer).
          Desktop only; on mobile these live in the drawer's sidebar footer. */}
      {!isMobile && (
        <div className="topbar-toolbar" data-tauri-drag-region={false}>
          {/* Grouped into four clusters — View · Input · Comms · App — with
              hairline dividers, so the row reads as a handful of things to scan
              instead of one flat run of ten icons. */}

          {/* View — arrange / repair what's on screen. */}
          <div className="topbar-group">
            <GridLayoutPicker />
            {/* Refresh view (realign + reflow) is a repair tool, so it stays out
                of the public top bar; Alt+R, the palette and the pane menu
                still reach it. Team installs keep the button. */}
            {teamFeatures && <RefreshViewButton />}
            {/* Internal browser toggle — open a single localhost-peek tile, click
                again (or Alt+B) to close it. Single-instance by design; the
                `active` class lights it while the browser is open. */}
            <button
              type="button"
              className={`chrome-btn${browserOpen ? ' active' : ''}`}
              onClick={() => toggleBrowser()}
              aria-pressed={browserOpen}
              title={browserOpen ? 'Close browser (Alt+B)' : 'Open browser — peek at a localhost service (Alt+B)'}
              aria-label={browserOpen ? 'Close browser' : 'Open browser'}
            >
              <IconGlobe />
            </button>
          </div>

          {/* Items of optional features (extensions.ts) — each renders its own
              leading divider, or nothing. */}
          {slotList('topbarItems').map((Item, i) => <Item key={i} />)}

          <span className="topbar-divider" aria-hidden="true" />

          {/* Comms — the three "something wants you" surfaces, together so their
              (now unified) badges read as one family. Attention = live panes
              waiting on a human; Alerts = notification channels + toast history;
              Inbox = scheduled-task reports, feedback updates, compose. */}
          <div className="topbar-group">
            <AttentionBell />
            <AlertsMenu />
            <InboxBubble />
          </div>

          <span className="topbar-divider" aria-hidden="true" />

          {/* App — global controls about the app itself, pushed to the edge
              (next to the window buttons in Tauri). */}
          <div className="topbar-group">
            {/* Settings — opens the unified tabbed dialog (was a mega-dropdown;
                the controls now live in Settings › Appearance/Panes/Claude…). */}
            <button
              type="button"
              className="chrome-btn"
              onClick={() => useSettings.getState().openSettings()}
              title="Settings"
              aria-label="Settings"
              aria-haspopup="dialog"
            >
              <IconSettings />
            </button>
            <HelpMenu onOpenGettingStarted={onOpenGettingStarted} onOpenShortcuts={onOpenHelp} />
          </div>
        </div>
      )}
      {isMobile && (
        <div className="topbar-toolbar" data-tauri-drag-region={false}>
          <OpenShinoque />
          <InboxBubble />
        </div>
      )}
      {tauri && (
        <WindowControls>
          {/* Window-lifecycle sibling of min/max/close: spawn a second
              full-app window (multi-monitor). Lived in the Workspaces
              section header before — a window action buried in a layout
              section — so it moved up here with the other window controls.
              Also reachable via the command palette ("New window"). */}
          {multiWindowAvailable() && (
            <button
              type="button"
              className="window-control"
              onClick={() => { void openWorkspaceWindow(); }}
              aria-label="New window"
              title="New window — open a second window you can move to another monitor"
            >
              <IconNewWindow size={14} />
            </button>
          )}
        </WindowControls>
      )}
    </div>
  );
}
