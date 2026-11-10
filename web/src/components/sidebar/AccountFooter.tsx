import { useState } from 'react';
import { useCloud } from '../../cloud';
import { usePersonalView } from '../../entitlements';
import { usePresentationMode } from '../../lib/hideIdentities';
import { useStore } from '../../store';
import { AlertsMenu } from '../AlertsMenu';
import { useIsMobileView } from '../useIsMobileView';
import { useSettings } from '../../settings';
import { api } from '../../api';
import { AriaHeaderStatus } from './AriaHeaderStatus';
import { confirmSignOut } from '../../lib/confirmSignOut';
import { IconClock, IconGauge, IconHelp, IconLayers, IconMessage, IconSettings, IconUser } from '../ui/icons';
import { openFeedback } from '../../lib/feedbackReport';

interface AccountFooterProps {
  onOpenHelp?: () => void;
  // Opens the full-screen Usage page. Usage is account-level data (cloud
  // aggregated, refreshed on open — not a live ticker), so its entry point
  // lives here with the identity rather than as a sidebar section.
  onOpenUsage?: () => void;
  onOpenSystem?: () => void;
  // Mobile: reveals the Schedules section in the drawer (un-hides + scrolls).
  // Phones have no right-click path to re-show a hidden section, so Schedules
  // needs a first-class entry point that works regardless of sidebar prefs.
  onOpenSchedules?: () => void;
}

export function AccountFooter({ onOpenHelp, onOpenUsage, onOpenSystem, onOpenSchedules }: AccountFooterProps) {
  const isMobile = useIsMobileView();
  const session = useCloud((s) => s.session);
  const signOut = useCloud((s) => s.signOut);
  const [busy, setBusy] = useState(false);
  // Presentation mode (screenshots, videos) and the personal view keep the
  // account email off screen; the row says "Signed in", so it does not jump.
  const presentation = usePresentationMode();
  const personal = usePersonalView();
  const hideEmail = presentation || personal;
  const email = hideEmail ? 'Signed in' : session?.user?.email || '';
  // The auto-approver's at-a-glance status (paused / fires in the last
  // hour + the reset action). Only shown when auto-approve is actually in
  // play — no "0/hr" noise for users who never touch it.
  const ariaActive = useStore((s) => s.ariaAutoFiresHr > 0 || s.ariaPaused
    || Object.values(s.panes).some((p) => p.autoMode === 'always' || p.autoMode === 'cautious'));
  // Same confirmation as Settings › Account: signing out stops phone access,
  // push and smart approvals, so a stray click here must not do it silently.
  const handle = async () => {
    if (busy) return;
    if (!(await confirmSignOut())) return;
    setBusy(true);
    void signOut();
  };
  const warmUsage = () => api.ccUsageSummaryPrefetch('30d');

  // Desktop: the global controls (grid, voice, alerts, prefs, help) now live in
  // the topbar utility toolbar, so the sidebar footer is account-only. Mobile:
  // the topbar toolbar is hidden, so keep the attention controls here — matching
  // the pre-uplift mobile footer, which surfaced help/prefs/alerts and left
  // voice + grid to the composer / single-pane layout.
  if (!isMobile && !session && !onOpenSystem) return null;

  return (
    <div
      className="sidebar-section sidebar-footer"
      title={session ? (hideEmail ? 'Signed in' : `Signed in as ${email}`) : undefined}
    >
      {isMobile && (
        <div className="sidebar-footer-chrome">
          <button
            type="button"
            className="chrome-btn sidebar-footer-help"
            onClick={onOpenHelp}
            title="Show keyboard shortcuts (?)"
            aria-label="Keyboard shortcuts"
          >
            <IconHelp size={16} />
          </button>
          {/* Settings dialog (z 100) overlays the drawer (z 80), so this can
              open it in place — no need to close the drawer first. */}
          <button
            type="button"
            className="chrome-btn"
            onClick={() => useSettings.getState().openSettings()}
            title="Settings"
            aria-label="Settings"
            aria-haspopup="dialog"
          >
            <IconSettings size={16} />
          </button>
          {onOpenSchedules && (
            <button
              type="button"
              className="chrome-btn"
              onClick={onOpenSchedules}
              title="Schedules — scheduled runs and their reports"
              aria-label="Schedules"
            >
              <IconClock size={16} />
            </button>
          )}
          {session && onOpenUsage && (
            <button
              type="button"
              className="chrome-btn"
              onClick={onOpenUsage}
              onPointerEnter={warmUsage}
              onFocus={warmUsage}
              onPointerDown={warmUsage}
              title="Usage — spend, turns and sessions for this account"
              aria-label="Open usage"
            >
              <IconGauge size={16} />
            </button>
          )}
          {onOpenSystem && (
            <button type="button" className="chrome-btn" onClick={onOpenSystem} title="System — resources and operational status" aria-label="Open System">
              <IconLayers size={16} />
            </button>
          )}
          {/* Phones have no Help menu or status bar: the form's own entry. */}
          <button
            type="button"
            className="chrome-btn"
            onClick={() => openFeedback()}
            title="Send feedback"
            aria-label="Send feedback"
          >
            <IconMessage size={16} />
          </button>
          <AlertsMenu />
        </div>
      )}
      {!isMobile && !session && onOpenSystem && (
        <div className="sidebar-footer-system-row">
          <button type="button" onClick={onOpenSystem} title="System — resources and operational status" aria-label="Open System"><IconLayers size={14} strokeWidth={2.25} /><span>System</span></button>
        </div>
      )}
      {session && (
        <div className="sidebar-footer-identity-row">
          {/* 16px: centers on a whole pixel in the 36px row, and at this size
              the avatar's knockout reads as a person — at 14 the shoulder
              cutout sat close enough to the rim to scan as a missing slice.
              See IconUser for the shape rationale. */}
          <IconUser size={16} className="sidebar-footer-identity-icon" />
          <span className="sidebar-footer-identity">{email}</span>
          {ariaActive && (
            <span className="sidebar-footer-aria">
              <AriaHeaderStatus />
            </span>
          )}
          {/* On mobile the gauge lives in the chrome row above (bigger touch
              target); keep the identity-row gauge desktop-only to avoid two
              adjacent entry points. */}
          {onOpenUsage && !isMobile && (
            <button
              type="button"
              className="iconbtn sidebar-footer-usage"
              onClick={onOpenUsage}
              onPointerEnter={warmUsage}
              onFocus={warmUsage}
              onPointerDown={warmUsage}
              title="Usage — spend, turns and sessions for this account"
              aria-label="Open usage"
            ><IconGauge size={14} strokeWidth={2.25} /></button>
          )}
          <button
            type="button"
            className="iconbtn sidebar-footer-signout"
            onClick={handle}
            disabled={busy}
            title="Sign out — returns to the login screen. Panes keep running in the daemon."
          >{busy ? '…' : 'Sign out'}</button>
        </div>
      )}
    </div>
  );
}
