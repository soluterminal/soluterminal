import { useEffect, useState } from 'react';
import { IconAlertTriangle, IconKey } from './ui/icons';
import { api } from '../api';
import { useDaemonHealth, refreshDaemonHealth } from '../lib/useDaemonHealth';
import { isDesktopShell } from '../cloudPair';
import { useClaudeInstall, ClaudeInstallConfirmCard } from '../claudeBootstrap';
import { activeBackendFromUrl, isRouterMode } from '../dualBackend';
import { setupBannerHidden, hideSetupBanner } from '../lib/setupBannerPrefs';
import { useStore } from '../store';
import { useTeamFeatures } from '../entitlements';
import { sectionOffered } from './sidebar/sidebarSections';

// Always-on Claude-Code readiness banner — surfaces the two states that
// dead-end a Claude pane: not installed, and installed-but-not-signed-in.
//
// WHY THIS EXISTS: the setup checklist (which also surfaces this) only opens on
// FIRST RUN (localStorage `solu.seenChecklist`), and that flag survives a
// reinstall — localStorage at 127.0.0.1:8787 isn't cleared when the desktop app
// is uninstalled. So a returning user who lacks `claude` (or hasn't signed in)
// never re-saw the checklist and got NO prompt. This banner closes that gap: it
// watches the daemon's live claude status and surfaces the guided install / the
// sign-in nudge whenever they're needed — independent of first-run state.
//
// Scope: desktop-shell only (install/login act on the LOCAL daemon — 403 over
// the relay — and are never relevant on the phone/web). In cloud launch mode the
// webview's /api/health is the cloud server, which has no claude* fields →
// claudeChecked is undefined → the banner stays hidden. The "not signed in"
// state never fires on macOS: the daemon reports authed conservatively there
// (Keychain creds aren't file-readable), so we never false-nag a signed-in Mac.

type Need = 'install' | 'login' | null;

type BackendId = 'wsl' | 'win';

function activeBackendForBanner(storeBackend: BackendId | null): BackendId | null {
  if (!isRouterMode()) return null;
  return storeBackend ?? activeBackendFromUrl();
}

// Names the side only in Dual mode, where Linux (WSL) and Windows PowerShell
// each need their own install and sign-in; elsewhere there is one side.
function activeBackendLabel(activeBackend: BackendId | null): string | null {
  if (!isRouterMode()) return null;
  return activeBackend === 'win' ? 'Windows PowerShell' : 'Linux (WSL)';
}

function healthBelongsToActiveBackend(
  health: ReturnType<typeof useDaemonHealth>['health'],
  activeBackend: BackendId | null,
): boolean {
  if (!isRouterMode() || !activeBackend || !health) return true;
  return health.backendID === activeBackend;
}

// hidden: the setup checklist (or the welcome tour before it) is open and
// already shows the same install / sign-in step.
export function ClaudeInstallBanner({ hidden = false }: { hidden?: boolean } = {}) {
  const { health } = useDaemonHealth();
  const storeActiveBackend = useStore((s) => s.activeBackend);
  // An available Credentials section owns the install/status row.
  const team = useTeamFeatures();
  const activeBackend = activeBackendForBanner(storeActiveBackend);
  const activeKey = activeBackend ?? 'local';
  // Session-scoped dismissal (NOT persisted), keyed to the NEED that was
  // dismissed. Dismissing the install nudge suppresses only the install state;
  // when claude is then installed and the state flips to "login", the banner
  // returns for the new (un-dismissed) need. It self-clears once claude is
  // installed AND signed in. Re-shows next launch if still unresolved.
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);
  const { installing, pendingInstall, startInstall, confirmInstall, cancelInstall, loginClaude } =
    useClaudeInstall({ onSpawn: (kind) => setDismissedKey(`${activeKey}:${kind}`) });

  const desktop = isDesktopShell();

  // Force ONE fresh probe on mount so a brand-new launch reflects ground truth
  // promptly (the cached health probe can be up to ~5 min stale; the boot probe
  // may also still be in flight). Skip it only when claude is fully ready
  // (installed AND signed in) — otherwise a fresh read is load-bearing. Read at
  // mount only; we deliberately don't re-run on every 10s health poll. 403 over
  // relay / 404 on an older daemon is swallowed, leaving the polled value intact.
  useEffect(() => {
    if (!desktop) return;
    if (
      healthBelongsToActiveBackend(health, activeBackend) &&
      health?.claudeInstalled === true &&
      health?.claudeAuthed === true
    ) return;
    let alive = true;
    void api
      .claudeRecheck()
      .then(() => { if (alive) refreshDaemonHealth(); })
      .catch(() => { /* keep the polled health value */ });
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [desktop, activeBackend]);

  if (!desktop || hidden) return null;

  // In Dual/router mode a backend switch happens in-place. The shared health
  // singleton can still hold the previous backend's last value for a moment; do
  // not render a Claude prompt from WSL while the UI label says PowerShell (or
  // vice versa). The effect above forces a fresh recheck for the active side.
  if (!healthBelongsToActiveBackend(health, activeBackend)) return null;

  // Only act once the daemon has actually probed (claudeChecked === true) — never
  // a premature "not installed" during the boot-probe window. Order: install
  // first (the harder blocker), then sign-in.
  const need: Need =
    health?.claudeChecked !== true
      ? null
      : health?.claudeInstalled === false
        ? 'install'
        : health?.claudeAuthed === false
          ? 'login'
          : null;
  const needKey = need ? `${activeKey}:${need}` : null;
  // Durable opt-out ("Don't show again") outranks every need. The Credentials
  // CLI row always shows install status, so hiding never strands reinstall —
  // and the row offers "Remind me" to bring the banner back.
  if (!need || dismissedKey === needKey || setupBannerHidden('claude')) return null;

  const dismiss = () => setDismissedKey(needKey);
  const dontShowAgain = () => { hideSetupBanner('claude'); setDismissedKey(needKey); };
  const backendLabel = activeBackendLabel(activeBackend);

  return (
    <div className="phone-access-banner" role="status">
      <div style={{ display: 'flex', flexDirection: 'column', gap: pendingInstall ? 6 : 0, flex: '1 1 auto', minWidth: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
          {need === 'install' ? (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconAlertTriangle size={13} style={{ verticalAlign: '-2px' }} /></span> Claude Code isn’t installed yet{backendLabel ? ` in ${backendLabel}` : ''}.
            </span>
          ) : (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconKey size={13} style={{ verticalAlign: '-2px' }} /></span> Claude Code isn’t signed in yet{backendLabel ? ` in ${backendLabel}` : ''}.
              {backendLabel && (
                <span className="phone-access-banner-sub"> Linux (WSL) and Windows PowerShell each need their own sign-in.</span>
              )}
            </span>
          )}
          <span className="phone-access-banner-actions">
            {need === 'install' ? (
              <button
                type="button"
                className="phone-access-banner-btn primary"
                disabled={installing}
                onClick={startInstall}
              >
                {installing ? 'Preparing…' : 'Install'}
              </button>
            ) : (
              <button
                type="button"
                className="phone-access-banner-btn primary"
                onClick={loginClaude}
              >
                Log in
              </button>
            )}
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Dismiss (returns next launch if Claude Code still isn’t ready)"
              aria-label="Dismiss"
              onClick={dismiss}
            >
              ×
            </button>
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title={team && sectionOffered('credentials')
                ? "Don't remind me again (install status stays in Credentials)"
                : "Don't remind me again on this computer"}
              aria-label="Don’t show again"
              onClick={dontShowAgain}
            >
              Don’t show again
            </button>
          </span>
        </div>
        {pendingInstall && (
          <ClaudeInstallConfirmCard
            spec={pendingInstall}
            onConfirm={confirmInstall}
            onCancel={cancelInstall}
          />
        )}
      </div>
    </div>
  );
}
