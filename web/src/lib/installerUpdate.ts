import type { UpdateStatus } from '../types';
import { isCloudLaunchMode, isVersionNewer } from './useCloudShellUpdate';
import { installerNeedsLocalMode } from './shellUpdateRoute';

// One answer to "is there a desktop-installer update this click can install?"
// shared by the banner, the version popover and the status-bar chip, so they
// cannot disagree (the banner used to read only the daemon's shellAvailable
// while the popover pinged the app itself).
//
// newer: the installer is newer than the installed desktop app. A known app
//   version (the app's own ping) wins over the daemon's shellAvailable, which
//   can be stale (a WSL daemon that outlived an installer update still reports
//   the older shell).
// blockedByDaemon: the installer carries an older daemon than the one already
//   installed (most releases are daemon-only and carry the last installer
//   forward, publish.sh). Every launch path refuses it as a downgrade
//   (applyAllUpdates, daemon ApplyShell, the NSIS hook), so offering it only
//   brought the button back after the daemon update and refused the click.
//   In cloud launch mode the status belongs to a remote daemon, so it says
//   nothing about this computer; the app's own updater decides there.
// localModeOnly: the desktop app is showing another computer's team daemon
//   (shellUpdateRoute.ts installerNeedsLocalMode) and its installer is newer
//   than this app. That installer is the other computer's, so no button here
//   installs it; the surfaces offer the switch to Local mode instead.
// actionable: newer, not held by a rollback, not blocked, and installable
//   from this window.
export interface InstallerUpdate {
  latest: string | null;
  current: string | null;
  newer: boolean;
  held: boolean;
  blockedByDaemon: boolean;
  localModeOnly: boolean;
  actionable: boolean;
}

export function installerUpdate(
  status: UpdateStatus | null | undefined,
  appShellVersion?: string | null,
): InstallerUpdate {
  const latest = status?.shellLatest || null;
  // Without a known app version (a browser tab, or before the app answered its
  // ping) the daemon's verdict stands: it also checks the manifest carries an
  // installer for this platform.
  const current = appShellVersion || null;
  const newer = !!latest && (current ? isVersionNewer(latest, current) : !!status?.shellAvailable);
  const held = !!latest && status?.holdVersion === latest;
  const installedDaemon = isCloudLaunchMode() ? '' : (status?.current ?? '');
  const blockedByDaemon = newer && !!latest && !!installedDaemon && isVersionNewer(installedDaemon, latest);
  const localModeOnly = newer && installerNeedsLocalMode(status);
  return {
    latest,
    current,
    newer,
    held,
    blockedByDaemon,
    localModeOnly,
    actionable: newer && !held && !blockedByDaemon && !localModeOnly,
  };
}
