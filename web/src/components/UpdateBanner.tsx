import { useEffect, useRef, useState } from 'react';
import { shellAutoApplyNeedsManual, useStore } from '../store';
import { firstHighlight } from '../lib/updateNotes';
import { useIsMobileDevice, useIsMobileView } from './useIsMobileView';
import { applyShellUpdateNative, isDesktopApp, revealShellInstallerNative } from '../tauri-browser';
import { apiErrorCode } from '../api';
import { fallBackToDownloadPage } from '../lib/shellUpdateFallback';
import { daemonOnThisComputer, installerNeedsLocalMode, shellAutoApplyEnabled, shellUpdateNeedsDaemon } from '../lib/shellUpdateRoute';
import { useShellUpdate } from '../lib/useCloudShellUpdate';
import { installerUpdate } from '../lib/installerUpdate';
import { VersionPopover } from './VersionPopover';

// THE update signal. It shows IMMEDIATELY when an update lands (the old
// design staged a top-bar pill first and escalated to this banner 30s later —
// two indications for one fact) and fires at most once per version: dismiss/
// snooze/update all silence it for that version, and the quiet status-bar
// chip remains the persistent entry point afterwards. It only reappears when
// a newer version ships. Loud (full-width, pushes layout) but never naggy.

const FIRST_SEEN_KEY = 'solu.update.banner.firstSeen';
const DISMISSED_KEY = 'solu.update.banner.dismissed';
const SNOOZE_KEY = 'solu.update.banner.snoozeUntil';
const SNOOZE_MS = 8 * 60 * 60 * 1000; // "Tonight"

interface VersionStamp {
  version: string;
  at: number;
}
interface SnoozeStamp {
  version: string;
  until: number;
}

function readJSON<T>(key: string): T | null {
  try {
    const v = localStorage.getItem(key);
    return v ? (JSON.parse(v) as T) : null;
  } catch {
    return null;
  }
}
function writeJSON(key: string, val: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(val));
  } catch {
    /* ignore */
  }
}

function formatMinutesSeconds(totalSeconds: number): string {
  const seconds = Math.max(0, totalSeconds);
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

export function UpdateBanner() {
  const updateStatus = useStore((s) => s.updateStatus);
  const deferredUpdate = useStore((s) => s.deferredUpdate);
  const autoApply = useStore((s) => s.updateSettings?.autoApply);
  const shellAutoApply = useStore((s) => s.updateSettings?.shellAutoApply);
  const shellAutoRestart = useStore((s) => s.shellAutoRestart);
  const applyUpdate = useStore((s) => s.applyUpdate);
  const applyShellUpdate = useStore((s) => s.applyShellUpdate);
  const applyAllUpdates = useStore((s) => s.applyAllUpdates);
  const setVersionPopoverOpen = useStore((s) => s.setVersionPopoverOpen);
  const openVersionPopoverFrom = useStore((s) => s.openVersionPopoverFrom);
  const versionPopoverOpen = useStore((s) => s.versionPopoverOpen);
  const versionPopoverSource = useStore((s) => s.versionPopoverSource);
  const restartShellNowForUpdate = useStore((s) => s.restartShellNowForUpdate);
  const postponeShellAutoRestart = useStore((s) => s.postponeShellAutoRestart);
  const toast = useStore((s) => s.toast);
  const isMobile = useIsMobileDevice();
  const isNarrow = useIsMobileView();
  const clientShellUpdate = useShellUpdate();
  const detailsButtonRef = useRef<HTMLButtonElement | null>(null);
  const [tick, setTick] = useState(0);
  const bump = () => setTick((n) => n + 1);

  const available = !!updateStatus?.available;
  const workflowDraining = available && (
    (updateStatus?.workflowDeferred === true
      && updateStatus.workflowDeferState !== 'backoff'
      && updateStatus.workflowDeferState !== 'publish')
    || (deferredUpdate?.cause === 'workflow'
      && !deferredUpdate.workflowNextRetryMs
      && updateStatus?.workflowDeferState !== 'backoff'
      && updateStatus?.workflowDeferState !== 'publish')
  );
  // Same installer verdict as the version popover (lib/installerUpdate.ts):
  // the app's own version wins over the daemon's, and an installer that
  // carries an older daemon than the installed one is not offered at all.
  const installer = installerUpdate(updateStatus, clientShellUpdate.current);
  const shellNeedsManual =
    shellAutoApplyNeedsManual(updateStatus?.shellLatest) && installer.actionable;
  // Only an install with the desktop app has an installer to update. The
  // daemon also flags one when it has never seen a desktop shell (a plain
  // install.sh daemon, shellCurrent empty), so a browser tab on such an
  // install must not offer it. Another computer's team installer is not
  // actionable here (installer.localModeOnly): ShellUpdateBanner and the
  // version popover offer Local mode for it instead.
  const hasDesktopShell = isDesktopApp() || !!updateStatus?.shellCurrent;
  const shellActionable = installer.actionable && !isMobile && hasDesktopShell;
  const actionable = available || shellActionable || !!shellAutoRestart;
  const applying = (!!updateStatus?.applying && !updateStatus?.workflowDeferred) || !!updateStatus?.shellApplying;
  const targetVersion = shellAutoRestart?.version ?? (available ? updateStatus?.latest : updateStatus?.shellLatest) ?? null;

  // Stamp first-seen the moment an actionable update for a NEW version appears
  // (kept for telemetry-ish continuity; visibility no longer waits on it).
  useEffect(() => {
    if (!actionable || !targetVersion) return;
    const seen = readJSON<VersionStamp>(FIRST_SEEN_KEY);
    if (!seen || seen.version !== targetVersion) {
      writeJSON(FIRST_SEEN_KEY, { version: targetVersion, at: Date.now() });
      bump();
    }
  }, [actionable, targetVersion]);

  // Wake up exactly when an active snooze elapses, so the banner reappears
  // without needing an unrelated re-render to trigger it.
  useEffect(() => {
    if (!actionable || !targetVersion || applying) return;
    const now = Date.now();
    const snooze = readJSON<SnoozeStamp>(SNOOZE_KEY);
    if (!snooze || snooze.version !== targetVersion || snooze.until <= now) return;
    const t = window.setTimeout(bump, snooze.until - now + 50);
    return () => window.clearTimeout(t);
  }, [actionable, targetVersion, applying, tick]);

  useEffect(() => {
    if (!shellAutoRestart && !workflowDraining) return;
    const timer = window.setInterval(bump, 1000);
    return () => window.clearInterval(timer);
  }, [shellAutoRestart?.version, shellAutoRestart?.deadlineMs, workflowDraining,
    updateStatus?.workflowDeferDeadlineMs, deferredUpdate?.workflowDeferDeadlineMs]);

  function visible(): boolean {
    if (!actionable || !targetVersion || applying) return false;
    // An active daemon workflow drain is authoritative shared state. It stays
    // visible on remote/PWA clients regardless of this browser's dismissal,
    // snooze, auto-apply preference, or whether it originated the POST.
    if (workflowDraining) return true;
    if (deferredUpdate) return false;
    // An imminent relaunch must stay visible even if this version was dismissed
    // or snoozed before the automatic countdown armed.
    if (shellAutoRestart) return true;
    // Both update tracks suppress their loud banner while their when-idle path
    // owns the pending work. The quiet status chip remains persistent. A failed
    // silent-shell attempt restores the manual fallback banner.
    // A paused silent restart (control file / public default) owns nothing:
    // the ordinary installer banner shows.
    const shellOwned =
      shellAutoApply === 'when-idle' &&
      shellAutoApplyEnabled(updateStatus) &&
      shellActionable &&
      !shellNeedsManual;
    if (shellOwned && !available) return false;
    // A REQUIRED daemon update auto-applies the same way regardless of the
    // pref (and falls back to RequiredUpdateModal if stuck) — same suppression.
    if ((autoApply === 'when-idle' || updateStatus?.required) && available && (!shellActionable || shellOwned))
      return false;
    let dismissed: string | null = null;
    try {
      dismissed = localStorage.getItem(DISMISSED_KEY);
    } catch {
      /* ignore */
    }
    if (dismissed === targetVersion) return false;
    const snooze = readJSON<SnoozeStamp>(SNOOZE_KEY);
    if (snooze && snooze.version === targetVersion && snooze.until > Date.now()) return false;
    return true;
  }

  if (!visible() || !targetVersion) return null;

  // Shell-only (desktop installer) updates are a separate version track and
  // relaunch the app — so don't show the daemon version or claim it "survives
  // your session"; both would be wrong/confusing.
  const shellOnly = !!shellAutoRestart || (!available && shellActionable);
  // The notes describe the daemon release, not the installer, so an
  // installer-only banner shows no headline from them.
  const headline = shellOnly ? null : firstHighlight(updateStatus?.notes);
  const countdownSeconds = shellAutoRestart
    ? Math.max(0, Math.ceil((shellAutoRestart.deadlineMs - Date.now()) / 1000))
    : 0;
  const workflowDeadline = deferredUpdate?.workflowDeferDeadlineMs
    ?? updateStatus?.workflowDeferDeadlineMs;
  const workflowCountdownSeconds = workflowDeadline
    ? Math.max(0, Math.ceil((workflowDeadline - Date.now()) / 1000))
    : 0;
  const workflowInflight = deferredUpdate?.workflowInflight
    ?? updateStatus?.workflowInflight
    ?? 0;

  async function onUpdate() {
    // Silence the banner immediately — it's done its job. (applyUpdate usually
    // ends in a reload; if it's queued for a mid-turn pane it returns with its
    // own toast, and we don't want the banner lingering meanwhile.)
    try {
      localStorage.setItem(DISMISSED_KEY, targetVersion!);
    } catch {
      /* ignore */
    }
    bump();
    try {
      const applyManualShellFallback = async () => {
        // Another computer's team installer: nothing here installs it, and
        // the download page would hand this computer the public one.
        if (installerNeedsLocalMode(updateStatus)) return;
        // Team channel: only the local daemon can fetch and verify the team
        // installer; the app's own step reads public releases (shellUpdateRoute.ts).
        if (shellUpdateNeedsDaemon(updateStatus)) {
          try {
            await applyShellUpdate(); // toasts its own success or failure
          } catch (error) {
            if (apiErrorCode(error) === 'shell_daemon_downgrade_refused') return;
            await fallBackToDownloadPage(toast, error instanceof Error ? error.message : String(error));
          }
          return;
        }
        const result = await applyShellUpdateNative(false);
        if (result.ok) {
          toast('Installer launched — follow the prompts to finish updating.', { kind: 'success', icon: '⬆' });
          return;
        }
        if (result.downgradeRefused) {
          toast(
            result.error ?? `Installer held: installed daemon ${result.installedVersion ?? 'unknown'} is newer than bundled daemon ${result.installerVersion ?? 'unknown'}.`,
            { kind: 'warn', icon: '⚠', ttlMs: 12000 },
          );
          return;
        }
        if (result.updateHeld) {
          toast(result.error ?? 'Installer update is paused by the rollback hold. Resume updates before retrying.', {
            kind: 'warn', icon: '⚠', ttlMs: 12000,
          });
          return;
        }
        if (result.staged) {
          const shown = await revealShellInstallerNative(result.staged);
          toast(
            shown
              ? "The installer downloaded but didn't open. Double-click it in the folder that just opened."
              : `The installer downloaded but didn't open. Run it from: ${result.staged}`,
            { kind: 'warn', icon: '⬆', ttlMs: 15000 },
          );
          return;
        }
        // The app's own installer step failed before launching anything. The
        // local daemon has a separate download-and-launch path, so try it
        // before sending the user to the download page.
        if (updateStatus?.shellAvailable && daemonOnThisComputer()) {
          try {
            await applyShellUpdate(); // toasts its own success or failure
            return;
          } catch (error) {
            if (apiErrorCode(error) === 'shell_daemon_downgrade_refused') return;
          }
        }
        await fallBackToDownloadPage(toast, result.error);
      };
      // Every installer click takes the same route as the version popover's:
      // the app's own updater first (the daemon alone on the team channel),
      // then the local daemon, then the download page. The installer may be
      // known only from the app's own version (shellAvailable false), so the
      // daemon-only applyShellUpdate is never the whole answer here.
      if (shellActionable) await applyAllUpdates({ applyShell: applyManualShellFallback, shellActionable: true });
      else if (available) await applyUpdate();
    } catch (e) {
      toast(`Update failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  function onSnooze() {
    writeJSON(SNOOZE_KEY, { version: targetVersion, until: Date.now() + SNOOZE_MS });
    bump();
  }

  function onDismiss() {
    try {
      localStorage.setItem(DISMISSED_KEY, targetVersion!);
    } catch {
      /* ignore */
    }
    bump();
  }

  return (
    <>
      <div className={`update-banner${workflowDraining ? ' update-banner-draining' : ''}`} role="status" aria-live="polite">
        <span className="update-banner-arrow" aria-hidden="true">↑</span>
        <button
          ref={detailsButtonRef}
          type="button"
          className="update-banner-text"
          onClick={() => {
            if (isNarrow) openVersionPopoverFrom('topbar');
            else setVersionPopoverOpen(true);
          }}
          title="See what's new"
        >
          <span className="update-banner-title">
            {workflowDraining
              ? `Update ${targetVersion} ready`
              : shellAutoRestart
              ? 'Desktop app update ready'
              : shellOnly
                ? 'Desktop installer update ready'
                : `soluterminal v${targetVersion} is ready`}
          </span>
          <span className="update-banner-sub">
            {workflowDraining
              ? `Waiting for ${workflowInflight} running worker${workflowInflight === 1 ? '' : 's'} to finish · will postpone in ${formatMinutesSeconds(workflowCountdownSeconds)} if still busy`
              : shellAutoRestart
              ? `restarting in ${countdownSeconds}s`
              : <>{headline ? `${headline} · ` : ''}{shellOnly ? 'runs the desktop installer to finish' : 'updates in seconds, survives your session'}</>}
          </span>
        </button>
        {!workflowDraining && <span className="update-banner-actions">
          {shellAutoRestart ? (
            <>
              <button type="button" className="update-banner-apply" onClick={() => void restartShellNowForUpdate()}>
                Restart now
              </button>
              <button type="button" className="update-banner-snooze" onClick={postponeShellAutoRestart}>
                Hold 1 hour
              </button>
            </>
          ) : (
            <>
              <button type="button" className="update-banner-apply" onClick={onUpdate}>
                Update now
              </button>
              <button type="button" className="update-banner-snooze" onClick={onSnooze}>
                Tonight
              </button>
              <button
                type="button"
                className="update-banner-close"
                onClick={onDismiss}
                aria-label="Dismiss"
                title="Dismiss until the next version"
              >
                ×
              </button>
            </>
          )}
        </span>}
      </div>
      {isNarrow && versionPopoverOpen && versionPopoverSource === 'topbar' && detailsButtonRef.current && (
        <VersionPopover
          anchor={detailsButtonRef.current}
          status={updateStatus}
          currentFallback={updateStatus?.current || ''}
          placement="down"
          onClose={() => setVersionPopoverOpen(false)}
        />
      )}
    </>
  );
}
