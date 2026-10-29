import { useState } from 'react';
import { useCloudShellUpdate } from '../lib/useCloudShellUpdate';
import { useStore } from '../store';
import { applyShellUpdateNative, revealShellInstallerNative } from '../tauri-browser';
import { fallBackToDownloadPage } from '../lib/shellUpdateFallback';
import { installerNeedsLocalMode } from '../lib/shellUpdateRoute';
import { switchDesktopLaunchMode } from '../lib/launchModeSwitch';

// Cloud-mode installer-update nudge. The daemon-driven update chip can't see a
// stale LOCAL shell in cloud mode (no local daemon — see useCloudShellUpdate),
// so this banner is the only surface that tells a cloud desktop user their app
// is behind. Dismissible per target version (a newer shell re-nags).
//
// "Update now" tries the native one-click path (apply_shell_update — download +
// SHA-verify + launch the installer, since there's no local daemon to do it),
// and falls back to opening the download page for a manual install on shells
// that predate that command (which is how a currently-stranded shell bootstraps
// onto the first shell that HAS one-click).
//
// On a team machine the reported installer is that computer's team build,
// which nothing in this window can install here (shellUpdateRoute.ts
// installerNeedsLocalMode), and the download page would hand this computer
// the public one. The banner then offers the switch to Local mode, where this
// computer's own daemon fetches and verifies its installer.

function dismissKey(version: string): string {
  return `solu.shellUpdateBannerDismissed.${version}`;
}

export function ShellUpdateBanner() {
  const { available, current, latest, releaseUrl } = useCloudShellUpdate();
  const [sessionDismissed, setSessionDismissed] = useState<string | null>(null);
  const [applying, setApplying] = useState(false);
  const [launchFailure, setLaunchFailure] = useState<{ path: string; error?: string } | null>(null);
  const [downgradeRefusal, setDowngradeRefusal] = useState<{ installed?: string; installer?: string; message?: string } | null>(null);
  const [updateHoldRefusal, setUpdateHoldRefusal] = useState<string | null>(null);
  const [switchingToLocal, setSwitchingToLocal] = useState(false);
  const toast = useStore((s) => s.toast);
  const applyAllUpdates = useStore((s) => s.applyAllUpdates);
  const localModeOnly = useStore((s) => installerNeedsLocalMode(s.updateStatus));

  if (!available || !latest || !releaseUrl) return null;

  const persistedDismissed = (() => {
    try {
      return localStorage.getItem(dismissKey(latest)) === '1';
    } catch {
      return false;
    }
  })();
  if (persistedDismissed || sessionDismissed === latest) return null;

  const dismiss = () => {
    try {
      localStorage.setItem(dismissKey(latest), '1');
    } catch {
      /* ignore — session dismissal still applies via state */
    }
    setSessionDismissed(latest);
  };

  const switchToLocal = async () => {
    if (switchingToLocal) return;
    const switched = await switchDesktopLaunchMode('local', toast, () => setSwitchingToLocal(true));
    if (!switched) setSwitchingToLocal(false);
  };

  const update = async () => {
    if (applying || localModeOnly) return;
    setApplying(true);
    const applyNative = async () => {
      // Try the native one-click first; fall back to the manual release page
      // when the command is missing (older shell) or the download/launch failed.
      const res = await applyShellUpdateNative(false);
      if (res.ok) {
        toast('Installer launched — follow the prompts to finish updating.', { kind: 'success', icon: '⬆' });
        return;
      }
      if (res.downgradeRefused) {
        setDowngradeRefusal({
          installed: res.installedVersion,
          installer: res.installerVersion,
          message: res.error,
        });
        return;
      }
      if (res.updateHeld) {
        setUpdateHoldRefusal(res.error ?? 'This installer version is paused by a rollback hold.');
        return;
      }
      if (res.staged) {
        setLaunchFailure({ path: res.staged, error: res.error });
        return;
      }
      await fallBackToDownloadPage(toast, res.error);
    };
    const result = await applyAllUpdates({ applyShell: applyNative, shellActionable: true });
    if (result?.downgradeRefused) {
      setDowngradeRefusal({
        installed: result.installedVersion,
        installer: result.installerVersion,
      });
    }
    setApplying(false);
  };

  const reveal = async () => {
    if (!launchFailure) return;
    if (!await revealShellInstallerNative(launchFailure.path)) {
      toast(`Couldn't open the folder. Installer path: ${launchFailure.path}`, {
        kind: 'error', icon: '⚠', ttlMs: 15000,
      });
    }
  };

  return (
    <div className="shell-update-banner" role="status">
      <span className="shell-update-banner-text">
        {updateHoldRefusal ? (
          <>
            <span aria-hidden>⚠</span> Installer paused by rollback hold — {updateHoldRefusal}
          </>
        ) : downgradeRefusal ? (
          <>
            <span aria-hidden>⚠</span> Installer held for safety — installed daemon v{downgradeRefusal.installed ?? 'unknown'} is newer than bundled v{downgradeRefusal.installer ?? 'unknown'}
          </>
        ) : launchFailure ? (
          <>
            <span aria-hidden>⚠</span> Installer downloaded but didn't open — open it manually
            <code className="shell-update-banner-path">{launchFailure.path}</code>
          </>
        ) : localModeOnly ? (
          <>
            <span aria-hidden>⬆</span> Update this computer's desktop app in Local mode
            <span className="shell-update-banner-sub"> · the installer shown here belongs to the computer you're viewing</span>
          </>
        ) : (
          <>
            <span aria-hidden>⬆</span> A newer desktop app (v{latest}) is available
            {current ? ` — you have v${current}` : ''}
            <span className="shell-update-banner-sub"> · restores the in-app Local/Cloud switch + latest fixes</span>
          </>
        )}
      </span>
      <span className="shell-update-banner-actions">
        {updateHoldRefusal || downgradeRefusal ? null : localModeOnly ? (
          <button
            type="button"
            className="shell-update-banner-btn primary"
            disabled={switchingToLocal}
            onClick={() => void switchToLocal()}
          >
            {switchingToLocal ? 'Switching…' : 'Switch to Local'}
          </button>
        ) : launchFailure ? (
          <button type="button" className="shell-update-banner-btn primary" onClick={() => void reveal()}>
            Show in folder
          </button>
        ) : (
          <button
            type="button"
            className="shell-update-banner-btn primary"
            disabled={applying}
            onClick={() => void update()}
          >
            {applying ? 'Updating…' : 'Update now'}
          </button>
        )}
        <button
          type="button"
          className="shell-update-banner-btn ghost"
          title="Dismiss"
          aria-label="Dismiss"
          onClick={dismiss}
        >
          ×
        </button>
      </span>
    </div>
  );
}
