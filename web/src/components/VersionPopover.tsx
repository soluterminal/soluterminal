import { PreviewChannelSection } from './PreviewChannelSection';
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from './ui/icons';
import { createPortal } from 'react-dom';
import { shellAutoApplyWaitReason, shellAutoWaitLine, useStore } from '../store';
import { api, ApiError, apiErrorCode } from '../api';
import { deriveRollbackPresentation, deriveUpdateHoldPresentation, type RollbackTarget, type UpdateStatus } from '../types';
import { userFacingNotes } from '../lib/releaseNotes';
import { useOperatorFeatures } from '../entitlements';
import { isPreviewBuild } from '../previewBoot';
import { installerUpdate } from '../lib/installerUpdate';
import { applyShellUpdateNative, browserPanePing, isDesktopApp, revealShellInstallerNative } from '../tauri-browser';
import { fallBackToDownloadPage } from '../lib/shellUpdateFallback';
import { daemonOnThisComputer, installerNeedsLocalMode, shellAutoApplyEnabled, shellUpdateNeedsDaemon } from '../lib/shellUpdateRoute';
import { switchDesktopLaunchMode } from '../lib/launchModeSwitch';
import { UPDATE_CHECK_FAILED } from '../lib/updateErrors';
import { useIsMobileDevice } from './useIsMobileView';
import { useDismiss } from './useDismiss';

function formatRelative(ms: number | undefined): string {
  if (!ms) return 'never';
  const delta = Date.now() - ms;
  if (delta < 0) return 'just now';
  if (delta < 5_000) return 'just now';
  if (delta < 60_000) return `${Math.floor(delta / 1000)}s ago`;
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m ago`;
  if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h ago`;
  return `${Math.floor(delta / 86_400_000)}d ago`;
}

const TOP_N = 3;

interface Props {
  anchor: HTMLElement;
  status: UpdateStatus | null;
  currentFallback: string;
  onClose: () => void;
  // 'up' (default) opens above the anchor for the bottom StatusBar chip;
  // 'down' opens below it for the top-bar update pill.
  placement?: 'up' | 'down';
}

// The popover is a DETAILS surface, not the primary path — the canonical
// "Update now" lives on the status-bar chip and the escalation banner. Layout
// is inverted vs the old version: the verdict + primary action sit in a fixed
// top zone that never scrolls, "what's new" shows a few cleaned highlights, and
// the version table + full changelog fold into collapsed disclosures. The apply
// button's position is a function of nothing variable, so the mid-turn warning
// (now an inline, height-reserved sub-line) can never push it down.
export function VersionPopover({ anchor, status, currentFallback, onClose, placement = 'up' }: Props) {
  const popRef = useRef<HTMLDivElement | null>(null);
  const isMobileDevice = useIsMobileDevice();
  const team = useOperatorFeatures();
  const applyUpdate = useStore((s) => s.applyUpdate);
  const applyShellUpdate = useStore((s) => s.applyShellUpdate);
  const applyAllUpdates = useStore((s) => s.applyAllUpdates);
  const checkForUpdate = useStore((s) => s.checkForUpdate);
  const deferredUpdate = useStore((s) => s.deferredUpdate);
  const shellAutoApply = useStore((s) => s.updateSettings?.shellAutoApply);
  const shellAutoRestart = useStore((s) => s.shellAutoRestart);
  const postponeShellAutoRestart = useStore((s) => s.postponeShellAutoRestart);
  const requestInFlight = useStore((s) => s.updateApplyRequestInFlight);
  const rollbackUpdate = useStore((s) => s.rollbackUpdate);
  const releaseUpdateHold = useStore((s) => s.releaseUpdateHold);
  const rollbackDecline = useStore((s) => s.rollbackDecline);
  const toast = useStore((s) => s.toast);
  // Panes whose agent speaks an older protocol than the daemon (see the
  // per-pane stale dot). A number, so the selector result stays stable.
  const stalePaneCount = useStore((s) => Object.values(s.panes).filter((p) => p.agentStale).length);
  const [showChangelog, setShowChangelog] = useState(false);
  const [showDetails, setShowDetails] = useState(false);
  const [checking, setChecking] = useState(false);
  const [confirmForce, setConfirmForce] = useState(false);
  const [rollbackTargets, setRollbackTargets] = useState<RollbackTarget[]>([]);
  const [rollbackConfirm, setRollbackConfirm] = useState<{ target: string; expiresMs: number } | null>(null);
  const [rollbackPending, setRollbackPending] = useState(false);
  const [, setCountdownTick] = useState(0);
  // The desktop-app (Tauri shell) version, pinged CLIENT-SIDE — works even when
  // the daemon can't report it: a Linux/WSL daemon under a Windows shell, or
  // cloud mode where the daemon is remote. It's a SEPARATE version track from
  // the daemon, so surfacing it here makes "app 0.0.846 vs installer 0.0.839"
  // legible instead of a mystery.
  const [desktopVersion, setDesktopVersion] = useState<string | null>(null);
  const [shellNativeApplying, setShellNativeApplying] = useState(false);
  const [nativeLaunchFailure, setNativeLaunchFailure] = useState<{ path: string; error?: string } | null>(null);
  const [downgradeRefusal, setDowngradeRefusal] = useState<{ installed?: string; installer?: string; message?: string } | null>(null);
  const [switchingToLocal, setSwitchingToLocal] = useState(false);
  useEffect(() => {
    if (!isDesktopApp()) return;
    let alive = true;
    void browserPanePing().then((v) => { if (alive && v) setDesktopVersion(v); });
    return () => { alive = false; };
  }, []);
  useEffect(() => {
    // Mounted preview builds have no daemon behind /api/*: skip the rollback
    // targets probe so a backend-less preview stays network-silent instead of
    // recording an expected 404 in acceptance.
    if (isPreviewBuild) {
      setRollbackTargets([]);
      return;
    }
    let alive = true;
    void api.getRollbackTargets().then((response) => {
      if (alive) setRollbackTargets(response.targets.filter((target) => target.eligible));
    }, () => {
      if (alive) setRollbackTargets([]);
    });
    return () => { alive = false; };
  }, [status?.current]);
  useEffect(() => {
    if (!shellAutoRestart) return;
    const timer = window.setInterval(() => setCountdownTick((tick) => tick + 1), 1000);
    return () => window.clearInterval(timer);
  }, [shellAutoRestart?.version, shellAutoRestart?.deadlineMs]);

  // The click-not-mousedown ordering guarantee is documented in useDismiss.ts.
  useDismiss(onClose, {
    inside: [popRef, anchor],
    escapeStopPropagation: true,
  });

  const rect = anchor.getBoundingClientRect();
  const POP_WIDTH = 320;
  const margin = 8;
  // 'down' (top pill) left-aligns to the anchor; 'up' (bottom chip)
  // right-aligns so it hugs the corner. Both clamp to the viewport.
  let left = placement === 'down' ? rect.left : rect.right - POP_WIDTH;
  if (left < margin) left = margin;
  if (left + POP_WIDTH > window.innerWidth - margin) {
    left = window.innerWidth - POP_WIDTH - margin;
  }
  const vstyle =
    placement === 'down'
      ? { top: rect.bottom + 6 }
      : { bottom: window.innerHeight - rect.top + 6 };

  const current = status?.current || currentFallback;
  const latest = status?.latest;
  const available = !!status?.available;
  const workflowDeferred = status?.workflowDeferred === true;
  const holdPresentation = deriveUpdateHoldPresentation(status, deferredUpdate);
  const rollbackPresentation = deriveRollbackPresentation(status);
  const applying = !!status?.applying && !workflowDeferred;
  const shellApplying = !!status?.shellApplying;
  const shellCurrent = status?.shellCurrent || '?';
  const shellLatest = status?.shellLatest;
  // Desktop-app (installer) track: prefer the client-pinged version, fall back
  // to the daemon's. Computed client-side so it's populated even when the daemon
  // can't see the shell (a WSL/Linux daemon under a Windows shell, or cloud).
  const shellCur = desktopVersion || (shellCurrent !== '?' ? shellCurrent : null);
  const installer = installerUpdate(status, shellCur);
  // Another computer's team installer (installer.localModeOnly) is not this
  // app's update: it gets no "Installer update available", no version pair
  // and no install button, only the way to update this computer's app. It
  // waits for the app's own version so the line never names the remote's.
  const shellUpd = !!(shellLatest && shellCur && installer.newer) && !installer.localModeOnly;
  const localModeUpdate = installer.localModeOnly && !!desktopVersion && !isMobileDevice;
  const shellHeld = installer.held;
  // The carried installer is older than the installed daemon, so every launch
  // path would refuse it. Say why instead of offering a button that comes back
  // after the daemon update and then refuses the click.
  const shellBlockedByDaemon = shellUpd && installer.blockedByDaemon;
  const lastError = status?.lastError;
  const lastChecked = status?.lastCheckedMs;
  const lastSuccess = status?.lastSuccessMs;
  const daemonLaunchFailure = status?.shellLaunch?.state === 'launch-failed'
    ? status.shellLaunch
    : null;
  const launchFailure = downgradeRefusal ? null : (nativeLaunchFailure ?? (daemonLaunchFailure
    ? { path: daemonLaunchFailure.stagedPath ?? '', error: daemonLaunchFailure.error }
    : null));

  // The installed desktop shell updates itself via the NATIVE one-click path
  // (download + SHA-verify + launch the installer, inside the Tauri process).
  // That works on ANY topology — a Windows daemon, a WSL/Linux daemon under a
  // Windows shell, or cloud where the daemon is remote — unlike the daemon-driven
  // apply, which needs daemon + shell on the same machine (and silently no-ops
  // from a Linux daemon). So detect client-side (shellUpd) and act natively.
  const shellUpdatable = isDesktopApp() && !isMobileDevice && shellUpd && !shellHeld && !shellBlockedByDaemon;
  const shellStatusOnly = shellUpd && isMobileDevice;
  const queuedRollbackTarget = deferredUpdate?.rollbackTarget;
  const anyApplying = requestInFlight || applying || shellApplying || shellNativeApplying || rollbackPending;
  const anyAvailable = available || shellUpdatable || !!queuedRollbackTarget;
  // Checks are FAILING and no cached update is actionable. "Up to date" would
  // be a lie here: latest/notes are last-known-good cache, and lastCheckedMs
  // advances on failed attempts too — a machine cut off from the update
  // server for days otherwise renders a green check. Say so instead, anchored
  // to the last time a check actually got through (lastSuccessMs).
  const unreachable = !!lastError && !anyAvailable;
  // Keep one queued surface mounted while the typed cause/details update.
  const workflowBackoff = holdPresentation.workflowBackoff;
  const queued = !!queuedRollbackTarget || ((!!deferredUpdate || workflowDeferred) && available);
  const queuedCause = holdPresentation.cause;

  const highlights = userFacingNotes(available && latest ? latest : current, status?.notes, team);
  const topHighlights = highlights.slice(0, TOP_N);
  // A daemon update and a desktop-installer (shell) update are SEPARATE version
  // tracks. When ONLY the installer update applies, the from→to pair must come
  // from the shell versions — pairing the daemon version with the shell version
  // renders a bogus "v0.0.742 → v0.0.741" (the installer usually lags the daemon).
  const shellOnly = !available && shellUpdatable;
  const fromVersion = shellOnly ? (shellCur ?? shellCurrent) : current;
  const toVersion = queuedRollbackTarget ?? (shellOnly ? shellLatest : latest);
  const summary = anyAvailable
    ? `v${fromVersion} → v${toVersion}${shellOnly ? ' · desktop installer' : ''} · checked ${formatRelative(lastChecked)}`
    : unreachable
      ? lastSuccess
        ? `v${current} · last reached ${formatRelative(lastSuccess)}`
        : `v${current} · update server not reached yet`
      : `v${current} · checked ${formatRelative(lastChecked)}`;

  let buttonLabel = 'Update now';
  if (requestInFlight || applying) buttonLabel = 'Updating…';
  else if (shellApplying) buttonLabel = 'Launching installer…';
  else if (queued && queuedRollbackTarget && queuedCause === 'focus') buttonLabel = 'Roll back now — interrupt Focus';
  else if (queued && queuedRollbackTarget && queuedCause === 'workflow') buttonLabel = 'Roll back now — force restart';
  else if (queued && queuedRollbackTarget) buttonLabel = 'Roll back now — interrupt active work';
  else if (queued && queuedCause === 'focus') buttonLabel = 'Update now — interrupt Focus';
  else if (queued && queuedCause === 'workflow') buttonLabel = 'Update now — force restart';
  else if (queued) buttonLabel = 'Update now — interrupt active work';
  else if (shellNativeApplying) buttonLabel = 'Launching installer…';
  else if (available && shellUpdatable) buttonLabel = 'Update now (app + installer)';
  else if (shellUpdatable && !available) buttonLabel = 'Update installer';

  // Reserved two-line sub-line under the button label — present at a fixed
  // height even when empty, so long real pane ids and workflow detail never
  // reflow the controls below it during an in-place hold transition.
  let buttonSub = '';
  if (applying) buttonSub = 'Swapping daemon — hang tight';
  else if (queued && queuedCause === 'focus') {
    buttonSub = deferredUpdate?.paneId
      ? `⏳ Applies when Focus pane ${deferredUpdate.paneId} finishes its turn`
      : '⏳ Applies when the active Focus turn finishes';
  } else if (queued && queuedCause === 'workflow') {
    const count = holdPresentation.workflowInflight;
    const deadline = holdPresentation.workflowDeferDeadlineMs;
    const remaining = deadline
      ? Math.ceil((deadline - Date.now()) / 60000)
      : null;
    const deadlineCopy = remaining === null
      ? ''
      : remaining > 0
        ? ` · deadline in ${remaining}m`
        : ' · drain deadline reached';
    const invocationCopy = count == null
      ? 'Critical workflow invocations'
      : `${count} workflow invocation${count === 1 ? '' : 's'}`;
    if (workflowBackoff && holdPresentation.workflowNextRetryMs) {
      const retryMinutes = Math.max(1, Math.ceil((holdPresentation.workflowNextRetryMs - Date.now()) / 60_000));
      buttonSub = `⏳ Update deferred · automatic retry in ${retryMinutes}m`;
    } else {
      buttonSub = `⏳ ${invocationCopy} draining${deadlineCopy}`;
    }
  } else if (queued) {
    buttonSub = '⏳ Applies when active work finishes';
  } else if (shellAutoRestart) {
    const seconds = Math.max(0, Math.ceil((shellAutoRestart.deadlineMs - Date.now()) / 1000));
    buttonSub = `↻ Restarts automatically in ${seconds}s`;
  } else if (available && shellUpdatable && latest) {
    // One click, two steps: the installer relaunches the app, and the new app
    // finishes the daemon update itself (store continueUpdateAfterInstaller).
    buttonSub = `Installer first · the app finishes v${latest} after it restarts`;
  } else if (shellAutoApply === 'when-idle' && shellUpdatable) {
    buttonSub = shellAutoWaitLine(shellAutoApplyWaitReason(), shellAutoApplyEnabled(status));
  }

  // The local daemon's ApplyShell: download + SHA-verify + launch (WMI/explorer
  // courier, WSL-interop aware). Resolves true when the attempt was handled
  // (launched, or refused by policy with its own message); false with the
  // reason when it failed and the manual download page is next.
  async function applyShellViaDaemon(): Promise<{ handled: true } | { handled: false; error?: string }> {
    try {
      await applyShellUpdate(); // toasts success/failure itself
      return { handled: true };
    } catch (error) {
      if (apiErrorCode(error) === 'shell_daemon_downgrade_refused') {
        const body = error instanceof ApiError && typeof error.body === 'object' && error.body !== null
          ? error.body as { installedVersion?: string; installerVersion?: string; message?: string }
          : {};
        setDowngradeRefusal({
          installed: body.installedVersion,
          installer: body.installerVersion,
          message: body.message,
        });
        return { handled: true };
      }
      return { handled: false, error: error instanceof Error ? error.message : String(error) };
    }
  }

  // One-click installer, trying every automatic path before the manual one:
  // 1. Native (download + SHA-verify + launch inside the Tauri process) —
  //    works on any topology, but shells ≤0.0.890 deny the command (ACL gap).
  // 2. Daemon ApplyShell — the same done by the LOCAL daemon. Only when the
  //    daemon itself reports shellAvailable (it verified it can see this
  //    machine's shell) and is on this computer: in a relayed session it is
  //    remote and would launch the installer on the wrong machine.
  // 3. The public download page for a manual install — last resort.
  // On the team channel the daemon goes alone (shellUpdateRoute.ts): the team
  // installer comes only through it, and the native step reads public releases.
  async function applyShellNative() {
    // Another computer's team installer: neither the app's updater, the
    // remote daemon nor the download page installs it on this computer.
    if (installerNeedsLocalMode(status)) return;
    setShellNativeApplying(true);
    setNativeLaunchFailure(null);
    setDowngradeRefusal(null);
    try {
      if (shellUpdateNeedsDaemon(status)) {
        const viaDaemon = await applyShellViaDaemon();
        if (!viaDaemon.handled) await fallBackToDownloadPage(toast, viaDaemon.error);
        return;
      }
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
        toast(
          res.error ?? `Installer refused: installed daemon ${res.installedVersion ?? 'unknown'} is newer than bundled daemon ${res.installerVersion ?? 'unknown'}.`,
          { kind: 'warn', icon: '⚠', ttlMs: 12000 },
        );
        return;
      }
      if (res.updateHeld) {
        toast(res.error ?? 'Installer update is paused by the rollback hold. Resume updates before retrying.', {
          kind: 'warn', icon: '⚠', ttlMs: 12000,
        });
        return;
      }
      if (res.staged) {
        setNativeLaunchFailure({ path: res.staged, error: res.error });
        toast(`Installer downloaded but didn't open — run it manually: ${res.staged}`, {
          kind: 'error', icon: '⚠', ttlMs: 15000,
        });
        return;
      }
      if (status?.shellAvailable && daemonOnThisComputer()) {
        // Non-policy failures fall through to the manual download page.
        if ((await applyShellViaDaemon()).handled) return;
      }
      await fallBackToDownloadPage(toast, res.error);
    } finally {
      setShellNativeApplying(false);
    }
  }

  async function revealLaunchFailure() {
    if (!launchFailure) return;
    const ok = nativeLaunchFailure
      ? await revealShellInstallerNative(launchFailure.path)
      : await api.revealShellInstaller().then(() => true, () => false);
    if (!ok) {
      toast(`Couldn't open the folder. Installer path: ${launchFailure.path}`, {
        kind: 'error', icon: '⚠', ttlMs: 15000,
      });
    }
  }

  async function onApply(forceConfirmed = false) {
    if (anyApplying) return;
    const workflowCount = holdPresentation.workflowInflight ?? 0;
    if (queued && workflowCount > 0 && !forceConfirmed) {
      setConfirmForce(true);
      return;
    }
    setConfirmForce(false);
    try {
      if (queued) {
        if (queuedRollbackTarget) {
          await rollbackUpdate({
            targetVersion: queuedRollbackTarget,
            holdHours: deferredUpdate?.rollbackHoldHours,
            force: true,
          });
        } else {
          await applyUpdate({ force: true }); // deliberate override; the label names what is skipped
        }
      } else if (available && shellUpdatable) {
        const result = await applyAllUpdates({ applyShell: applyShellNative, shellActionable: true });
        if (result?.downgradeRefused) {
          setDowngradeRefusal({
            installed: result.installedVersion,
            installer: result.installerVersion,
          });
        }
      } else if (available) {
        await applyUpdate();
      } else if (shellUpdatable) {
        const result = await applyAllUpdates({ applyShell: applyShellNative, shellActionable: true });
        if (result?.downgradeRefused) {
          setDowngradeRefusal({
            installed: result.installedVersion,
            installer: result.installerVersion,
          });
        }
      }
    } catch (e) {
      toast(`Update failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  async function onSwitchToLocal() {
    if (switchingToLocal) return;
    const switched = await switchDesktopLaunchMode('local', toast, () => setSwitchingToLocal(true));
    if (!switched) setSwitchingToLocal(false);
  }

  async function onSkipInstaller() {
    if (anyApplying) return;
    try {
      await applyUpdate();
    } catch (e) {
      toast(`Update failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  async function onCheck() {
    if (checking) return;
    setChecking(true);
    try {
      await checkForUpdate();
    } finally {
      setChecking(false);
    }
  }

  async function onRollback(targetVersion: string, force = false) {
    if (rollbackPending) return;
    setRollbackPending(true);
    try {
      await rollbackUpdate({ targetVersion, holdHours: 72, force });
      setRollbackConfirm(null);
    } catch (error) {
      toast(`Rollback failed: ${(error as Error).message}`, { kind: 'error', icon: '⚠' });
    } finally {
      setRollbackPending(false);
    }
  }

  return createPortal(
    <div
      ref={popRef}
      className="version-popover"
      style={{ left, ...vstyle, width: POP_WIDTH }}
      role="dialog"
      aria-label="Update status"
    >
      {/* Zone A — verdict + summary + primary action (fixed, never scrolls) */}
      <div className="vp-verdict">
        {queuedRollbackTarget ? (
          <>
            <span className="vp-verdict-dot" aria-hidden="true" />
            Rollback queued
          </>
        ) : anyAvailable ? (
          <>
            <span className="vp-verdict-dot" aria-hidden="true" />
            {shellOnly ? 'Installer update available' : 'Update available'}
          </>
        ) : unreachable ? (
          <>
            <span className="vp-verdict-warn" aria-hidden="true"><IconAlertTriangle size={12} style={{ verticalAlign: '-2px' }} /></span>
            Can't reach update server
          </>
        ) : (
          <>
            <span className="vp-verdict-check" aria-hidden="true">✓</span>
            Up to date
          </>
        )}
      </div>
      <div className="vp-summary">{summary}</div>
      <PreviewChannelSection />
      {stalePaneCount > 0 && (
        <div className="vp-summary">
          {`${stalePaneCount} ${stalePaneCount === 1 ? 'pane' : 'panes'} on older builds · update when restarted`}
        </div>
      )}
      {shellBlockedByDaemon && isDesktopApp() && !isMobileDevice && (
        <div className="vp-summary" data-testid="vp-installer-blocked">
          {`Desktop app v${shellCur} · installer v${shellLatest} carries an older daemon than v${current}, so it updates with the next desktop release`}
        </div>
      )}

      {anyAvailable ? (
        <button
          type="button"
          className="vp-apply"
          onClick={() => void onApply(false)}
          disabled={anyApplying}
        >
          <span className="vp-apply-label">{buttonLabel}</span>
          <span className="vp-apply-sub">{buttonSub}</span>
        </button>
      ) : (
        <button
          type="button"
          className="vp-check"
          onClick={onCheck}
          disabled={checking}
        >
          {checking ? 'Checking…' : 'Check now'}
        </button>
      )}

      {localModeUpdate && (
        <>
          <div className="vp-summary" data-testid="vp-installer-local-mode">
            {`Desktop app v${desktopVersion} · updates for this computer run in Local mode`}
          </div>
          <button
            type="button"
            className="vp-check"
            onClick={() => void onSwitchToLocal()}
            disabled={switchingToLocal}
          >
            {switchingToLocal ? 'Switching to Local…' : "Switch to Local to update this computer's desktop app"}
          </button>
        </>
      )}

      {confirmForce && (
        <div className="vp-force-confirm" role="alert">
          <strong>This will interrupt {holdPresentation.workflowInflight} running workflow invocation{holdPresentation.workflowInflight === 1 ? '' : 's'}.</strong>
          <span>Their work will be lost.</span>
          <div>
            <button type="button" onClick={() => void onApply(true)}>
              {queuedRollbackTarget ? 'Confirm force rollback' : 'Confirm force update'}
            </button>
            <button type="button" onClick={() => setConfirmForce(false)}>Cancel</button>
          </div>
        </div>
      )}

      {available && shellUpdatable && !anyApplying && (
        <button type="button" className="vp-skip" onClick={onSkipInstaller}>
          Skip installer for now
        </button>
      )}

      {shellAutoRestart && !anyApplying && (
        <button type="button" className="vp-skip" onClick={postponeShellAutoRestart}>
          Hold automatic restart for 1 hour
        </button>
      )}

      {lastError && <div className="vp-error">{UPDATE_CHECK_FAILED}</div>}
      {status?.shellLaunch?.state === 'rejected' && status.shellLaunch.error && (
        <div className="vp-error">{status.shellLaunch.error}</div>
      )}
      {shellStatusOnly && (
        <div className="vp-error">
          Installer updates must be started from the desktop app on that computer.
        </div>
      )}
      {downgradeRefusal && (
        <div className="vp-error">
          Installer held for safety: installed daemon v{downgradeRefusal.installed ?? 'unknown'} is newer than bundled daemon v{downgradeRefusal.installer ?? 'unknown'}. No downgrade was started.
        </div>
      )}
      {launchFailure && (
        <div className="vp-error vp-launch-failed">
          <div>Installer downloaded but failed to open — start it manually:</div>
          <code>{launchFailure.path}</code>
          <button type="button" onClick={() => void revealLaunchFailure()}>Show in folder</button>
        </div>
      )}
      {rollbackDecline && (
        <div className="vp-rollback-declined" role="alert">
          <strong>Rollback declined — nothing was changed.</strong>
          <span>{rollbackDecline.store} state requires reader {rollbackDecline.requiredReader}; v{rollbackDecline.targetVersion} supports {rollbackDecline.targetReader}.</span>
          <span>{rollbackDecline.message}</span>
        </div>
      )}
      {rollbackPresentation.rollbackFailed && (
        <div className="vp-rollback-failed" role="alert">
          <strong>Rollback to v{rollbackPresentation.rollbackTarget ?? 'the selected version'} failed.</strong>
          <span>{rollbackPresentation.rollbackError ?? 'The previous binary was restored. You can retry safely.'}</span>
          {rollbackPresentation.rollbackTarget && (
            <button type="button" disabled={anyApplying} onClick={() => void onRollback(rollbackPresentation.rollbackTarget!)}>Retry</button>
          )}
        </div>
      )}

      {rollbackPresentation.held && (
        <div className="vp-hold-banner">
          <span>Updates to v{rollbackPresentation.heldVersion} are paused{rollbackPresentation.holdExpiresMs ? ` until ${new Date(rollbackPresentation.holdExpiresMs).toLocaleString()}` : ''}.</span>
          <button type="button" onClick={() => void releaseUpdateHold()}>Resume updates now</button>
        </div>
      )}

      {/* Zone B — what's new (fixed; cleaned highlights, full log on demand) */}
      <div className="vp-section">
        <div className="vp-section-title">{anyAvailable ? "What's new" : 'Latest release'}</div>
        {topHighlights.length > 0 ? (
          <ul className="vp-highlights">
            {topHighlights.map((h, i) => (
              <li key={i} className={`vp-hl vp-hl-${h.kind}`} title={h.text}>
                {h.text}
              </li>
            ))}
          </ul>
        ) : (
          <div className="vp-empty">No release notes.</div>
        )}
        {highlights.length > TOP_N && (
          <button
            type="button"
            className="vp-disclosure"
            onClick={() => setShowChangelog((v) => !v)}
            aria-expanded={showChangelog}
          >
            {showChangelog ? 'Hide changelog' : `Show full changelog (${highlights.length}) ▸`}
          </button>
        )}
        {showChangelog && (
          <div className="vp-changelog">
            {highlights.map((h, i) => (
              <div key={i} className={`vp-cl-line vp-hl-${h.kind}`}>
                {h.text}
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Zone C — version details (collapsed by default) */}
      <button
        type="button"
        className="vp-disclosure vp-details-toggle"
        onClick={() => setShowDetails((v) => !v)}
        aria-expanded={showDetails}
      >
        {showDetails ? 'Hide details' : 'Details ▸'}
      </button>
      {showDetails && (
        <div className="vp-details">
          {/* Two SEPARATE version tracks. The daemon (the background service)
              updates continuously; the desktop app / installer (the Tauri shell
              that hosts the window) is rebuilt occasionally, so it usually
              trails. Showing both, always, makes the gap legible not mysterious. */}
          <div className="vp-row">
            <span className="vp-row-label">App (daemon)</span>
            <span className="vp-row-value">
              v{available && latest ? `${current} → v${latest}` : current}
              {available && <span className="vp-pill">update</span>}
            </span>
          </div>
          {shellCur && (
            <div className="vp-row">
              <span className="vp-row-label">Desktop app</span>
              <span className="vp-row-value">
                v{shellUpd && shellLatest ? `${shellCur} → v${shellLatest}` : shellCur}
                {shellUpd && <span className="vp-pill">update</span>}
              </span>
            </div>
          )}
          <div className="vp-row">
            <span className="vp-row-label">Last checked</span>
            <span className="vp-row-value">{formatRelative(lastChecked)}</span>
          </div>
          {lastError && (
            <div className="vp-row">
              <span className="vp-row-label">Last reached</span>
              <span className="vp-row-value">{formatRelative(lastSuccess)}</span>
            </div>
          )}
          {status?.workflowDeferState && (
            <>
              <div className="vp-row">
                <span className="vp-row-label">Workflow update</span>
                <span className="vp-row-value">{status.workflowDeferState === 'backoff' ? 'Deferred until retry' : 'Waiting for drain'}</span>
              </div>
              <div className="vp-row">
                <span className="vp-row-label">In flight</span>
                <span className="vp-row-value">{status.workflowInflight ?? 0}</span>
              </div>
              <div className="vp-row">
                <span className="vp-row-label">Drain attempts</span>
                <span className="vp-row-value">{status.workflowDeferAttempts ?? 0}</span>
              </div>
              {status.workflowNextRetryMs && (
                <div className="vp-row">
                  <span className="vp-row-label">Next retry</span>
                  <span className="vp-row-value">{new Date(status.workflowNextRetryMs).toLocaleTimeString()}</span>
                </div>
              )}
            </>
          )}
            {rollbackTargets.length > 0 && (
              <div className="vp-rollback">
                <button
                  type="button"
                  disabled={anyApplying}
                  onClick={() => setRollbackConfirm({ target: rollbackTargets[0].version, expiresMs: Date.now() + 72 * 60 * 60 * 1000 })}
                >
                  Roll back this update
                </button>
              </div>
            )}
        </div>
      )}
      {rollbackConfirm && (
        <div className="vp-rollback-confirm" role="alert">
          <strong>Roll back v{current} → v{rollbackConfirm.target}?</strong>
          <span>The daemon restarts briefly. Updates to v{current} will be paused until {new Date(rollbackConfirm.expiresMs).toLocaleString()}.</span>
          <span>{holdPresentation.workflowInflight
            ? `${holdPresentation.workflowInflight} running workflow invocation${holdPresentation.workflowInflight === 1 ? '' : 's'} must finish, unless you force the rollback.`
            : 'Running workflow invocations must drain before the restart.'}</span>
          <div>
            <button type="button" disabled={anyApplying} onClick={() => void onRollback(rollbackConfirm.target)}>Confirm rollback</button>
            <button type="button" disabled={anyApplying} onClick={() => setRollbackConfirm(null)}>Cancel</button>
          </div>
        </div>
      )}
    </div>,
    document.body,
  );
}
