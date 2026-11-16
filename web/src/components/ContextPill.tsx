import { useStore } from '../store';
import { TopbarPopover } from './TopbarPopover';
import { BackendControls, useBackendEnv } from './BackendSwitcher';
import { IconLayers, IconChevronDown, IconBolt, IconCloud } from './ui/icons';
import { isDesktopApp } from '../tauri-browser';
import { showEmbedChrome } from '../embedChrome';

// The top-left context cluster — two adjacent-but-distinct elements, NOT one
// crammed pill. Ordered hosting-LEFT, workspace-RIGHT on purpose:
//   1. an actionable HOSTING button ([⚡/☁] Local/Cloud · side ▾) that opens the
//      hosting/backend/side switch. Anchored on the LEFT so it NEVER shifts
//      position — a control you reach for repeatedly wants a stable, muscle-memory
//      spot, and it's fixed-width while the workspace name is not. Leads with the
//      hosting glyph (the thing the click changes) and is color-coded: Cloud gets
//      the calm violet --host-cloud so "your commands run on a remote box" reads
//      at a glance. The primary label stays short (hosting + active side); the
//      OS/backend the commands run on (Linux/Windows/Dual/macOS) rides along as a
//      quiet secondary badge — visible at a glance but clearly subordinate — with
//      the full detail still in the popover + aria-label.
//   2. a quiet WORKSPACE IDENTITY label ([layers] name) on the RIGHT — the
//      "which layout am I in" readout. Not a button; workspace SWITCHING lives in
//      the sidebar. Its width varies with the name, so it sits on the right and
//      grows into the free center space: its own movement when YOU switch is
//      expected and attributable, while the hosting button (left) stays steady
//      instead of jittering every time the name length changes.
//
// In a browser tab there's nothing to switch (the page origin fixes hosting), so
// the hosting element renders as a read-only static chip with no chevron/hover.
// While cloud detection is pending on desktop the hosting slot reserves its width
// so its later appearance doesn't shove the workspace label.

export function ContextPill() {
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  const env = useBackendEnv();

  const workspaceLabel = activeWorkspace === '_current'
    ? 'Current layout'
    : activeWorkspace || 'Workspace';

  const isCloud = env.connActive === 'cloud';
  const HostGlyph = isCloud ? IconCloud : IconBolt;
  const hostingWord = isCloud
    ? `Cloud${env.machineName ? ` · ${env.machineName}` : ''}`
    : 'Local';
  const sideWord = env.showRouterSide
    ? (env.activeBackend === 'win' ? 'PowerShell' : 'WSL')
    : null;
  // The OS/backend the commands actually run on (Linux/Windows/Dual/macOS),
  // resolved for desktop, cloud, and native-Linux/macOS alike (see useBackendEnv).
  const osWord = env.osWord;
  // The accessible name keeps the full environment so screen-reader users don't
  // lose any detail even though the visible primary text stays terse.
  const envAria = [hostingWord, osWord, sideWord].filter(Boolean).join(' · ');

  // Hosting shows once cloud detection settles (actionable in the desktop shell,
  // read-only in a browser tab). While detecting, only the workspace shows.
  const showEnv = showEmbedChrome('hostingSwitch') && (env.actionable || env.showConnDisplay);
  // Hosting is anchored on the left, so while detection is still pending on
  // desktop (where hosting WILL appear) reserve its slot width — otherwise the
  // control fading in would shove the workspace label to its right. Not reserved
  // in a browser tab, where hosting may never appear (no empty left indent).
  const reserveEnv = !showEnv && isDesktopApp();

  const envInner = (
    <>
      <HostGlyph size={13} className="context-env-glyph" />
      <span className="context-env-text">
        {hostingWord}{sideWord ? ` · ${sideWord}` : ''}
      </span>
      {osWord && (
        <span
          className={`context-env-os${env.modeMismatch ? ' mismatch' : ''}`}
          title={env.modeMismatch ? 'The saved mode didn’t start. Open to retry it or choose another.' : undefined}
        >
          {osWord}
        </span>
      )}
    </>
  );

  return (
    <div className="context-cluster">
      {/* Hosting control — anchored LEFT so it stays put as the variable-width
          workspace name (right) changes. The slot reserves width while cloud
          detection is pending so hosting appearing doesn't shove the workspace. */}
      <span className={`context-env-slot${reserveEnv ? ' reserve' : ''}`}>
        {showEnv && (
          env.actionable ? (
            <TopbarPopover
              align="left"
              minWidth={220}
              trigger={({ open, toggle }) => (
                <button
                  type="button"
                  className={`context-env${isCloud ? ' is-cloud' : ''}${open ? ' open' : ''}`}
                  aria-label={`Environment: ${envAria} — change hosting or backend`}
                  aria-expanded={open}
                  title="Hosting & backend"
                  onClick={toggle}
                >
                  {envInner}
                  <IconChevronDown size={13} className="context-env-caret" />
                </button>
              )}
            >
              <BackendControls />
            </TopbarPopover>
          ) : (
            <div
              className={`context-env context-env-static${isCloud ? ' is-cloud' : ''}`}
              title={`Environment: ${envAria}`}
            >
              {envInner}
            </div>
          )
        )}
      </span>

      {showEnv && <span className="context-divider" aria-hidden="true" />}

      <span className="context-ws" title={`Workspace: ${workspaceLabel}`}>
        <IconLayers size={14} className="context-ws-icon" />
        <span className="context-ws-name">{workspaceLabel}</span>
      </span>
    </div>
  );
}
