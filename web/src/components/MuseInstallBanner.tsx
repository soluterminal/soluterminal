import { useEffect, useState } from 'react';
import { IconAlertTriangle, IconKey } from './ui/icons';
import { api } from '../api';
import { useDaemonHealth, refreshDaemonHealth } from '../lib/useDaemonHealth';
import { isDesktopShell } from '../cloudPair';
import { useMuseInstall, MuseInstallConfirmCard } from '../museBootstrap';
import { activeBackendFromUrl, isRouterMode } from '../dualBackend';
import { setupBannerHidden, hideSetupBanner } from '../lib/setupBannerPrefs';
import { useStore } from '../store';

// Always-on Muse-Code readiness banner — the parallel of AgentSetupBanner.
// Surfaces the two states that dead-end Muse usage: not installed, and
// installed-but-not-signed-in. Same scope rules: desktop-shell only
// (install/login act on the LOCAL daemon — 403 over the relay); in cloud
// launch mode the webview's /api/health has no muse* fields → museChecked is
// undefined → the banner stays hidden.

type Need = 'install' | 'login' | null;

type BackendId = 'wsl' | 'win';

function activeBackendForBanner(storeBackend: BackendId | null): BackendId | null {
  if (!isRouterMode()) return null;
  return storeBackend ?? activeBackendFromUrl();
}

function activeBackendLabel(activeBackend: BackendId | null): string {
  if (!isRouterMode()) return 'this machine';
  return activeBackend === 'win' ? 'PowerShell' : 'WSL';
}

function healthBelongsToActiveBackend(
  health: ReturnType<typeof useDaemonHealth>['health'],
  activeBackend: BackendId | null,
): boolean {
  if (!isRouterMode() || !activeBackend || !health) return true;
  return health.backendID === activeBackend;
}

export function MuseInstallBanner() {
  const { health } = useDaemonHealth();
  const storeActiveBackend = useStore((s) => s.activeBackend);
  const activeBackend = activeBackendForBanner(storeActiveBackend);
  const activeKey = activeBackend ?? 'local';
  // Session-scoped dismissal (NOT persisted), keyed to the NEED that was
  // dismissed — same contract as the Claude banner.
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);
  const { installing, pendingInstall, startInstall, confirmInstall, cancelInstall, loginMuse } =
    useMuseInstall({ onSpawn: (kind) => setDismissedKey(`${activeKey}:${kind}`) });

  const desktop = isDesktopShell();

  // Force ONE fresh probe on mount so a brand-new launch reflects ground truth
  // promptly (the cached health probe can be up to ~5 min stale; the boot
  // probe may also still be in flight). Skip it only when muse is fully ready.
  useEffect(() => {
    if (!desktop) return;
    if (
      healthBelongsToActiveBackend(health, activeBackend) &&
      health?.museInstalled === true &&
      health?.museAuthed === true
    ) return;
    let alive = true;
    void api
      .museRecheck()
      .then(() => { if (alive) refreshDaemonHealth(); })
      .catch(() => { /* keep the polled health value */ });
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [desktop, activeBackend]);

  if (!desktop) return null;

  // In Dual/router mode don't render a Muse prompt from WSL while the UI
  // label says PowerShell (or vice versa).
  if (!healthBelongsToActiveBackend(health, activeBackend)) return null;

  // Only act once the daemon has actually probed (museChecked === true) —
  // never a premature "not installed" during the boot-probe window. Order:
  // install first (the harder blocker), then sign-in.
  const need: Need =
    health?.museChecked !== true
      ? null
      : health?.museInstalled === false
        ? 'install'
        : health?.museAuthed === false
          ? 'login'
          : null;
  const needKey = need ? `${activeKey}:${need}` : null;
  // Durable opt-out ("Don't show again") outranks every need. The Credentials
  // CLI row always shows install status, so hiding never strands reinstall —
  // and the row offers "Remind me" to bring the banner back.
  if (!need || dismissedKey === needKey || setupBannerHidden('muse')) return null;

  const dismiss = () => setDismissedKey(needKey);
  const dontShowAgain = () => { hideSetupBanner('muse'); setDismissedKey(needKey); };
  const backendLabel = activeBackendLabel(activeBackend);
  // This daemon runs Windows: the installer can never succeed here (Muse Code
  // ships macOS/Linux binaries only), so say where to go instead of offering
  // a doomed install. The button still opens the spec — the hook's toast
  // repeats the routing.
  const windowsHost = (health?.shellPlatform ?? '').startsWith('win');

  return (
    <div className="phone-access-banner" role="status">
      <div style={{ display: 'flex', flexDirection: 'column', gap: pendingInstall ? 6 : 0, flex: '1 1 auto', minWidth: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
          {need === 'install' ? (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconAlertTriangle size={13} style={{ verticalAlign: '-2px' }} /></span> Muse Code isn’t installed for {backendLabel}
              <span className="phone-access-banner-sub">{windowsHost
                ? ' — Muse Code is macOS/Linux-only; switch to the WSL side to install it there'
                : ' — Muse workflow runs need the `muse` CLI on this side'}</span>
            </span>
          ) : (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconKey size={13} style={{ verticalAlign: '-2px' }} /></span> Muse Code isn’t signed in for {backendLabel}
              <span className="phone-access-banner-sub"> — sign in with your browser or paste an API key</span>
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
                {installing ? 'Preparing…' : 'Review install'}
              </button>
            ) : (
              <button
                type="button"
                className="phone-access-banner-btn primary"
                onClick={loginMuse}
              >
                Sign in
              </button>
            )}
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Dismiss (returns next launch if Muse Code still isn’t ready)"
              aria-label="Dismiss"
              onClick={dismiss}
            >
              ×
            </button>
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Don't remind me again (install status stays in Credentials)"
              aria-label="Don’t show again"
              onClick={dontShowAgain}
            >
              Don’t show again
            </button>
          </span>
        </div>
        {pendingInstall && (
          <MuseInstallConfirmCard
            spec={pendingInstall}
            onConfirm={confirmInstall}
            onCancel={cancelInstall}
          />
        )}
      </div>
    </div>
  );
}
