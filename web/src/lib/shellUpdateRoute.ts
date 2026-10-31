import { isRelayedSession, useCloud } from '../cloud';
import type { UpdateStatus } from '../types';
import { isDesktopApp } from '../tauri-browser';
import { isCloudLaunchMode } from './useCloudShellUpdate';

// Which updater installs the desktop app's installer.
//
// The local daemon can download, verify and launch it (daemon/updater_shell.go
// ApplyShell). Only that daemon may be asked: in a relayed session (the app
// loaded from the cloud, or the desktop app in cloud launch mode) /api/* is a
// daemon on another computer, which would launch the installer there.
//
// useCloud's mode is not that test. It is 'cloud' whenever the daemon serves
// /api/config, and every local daemon built with sign-in does (with isLocal).
// Testing mode alone turned the daemon path off on ordinary local installs.
export function daemonOnThisComputer(): boolean {
  return !isCloudLaunchMode() && !isRelayedSession(useCloud.getState());
}

// The installer belongs to the team build, so only the local daemon can get
// it: it follows the team channel only on a positive team verdict, fetches the
// installer through the cloud with this computer's machine token, and accepts
// it only from a manifest signed "flavor": "team". The app's own updater reads
// the public releases only, and a public app refuses any other flavour, so on
// this channel it is not used at all.
// The unattended installer restart is allowed only when the daemon says so
// explicitly (daemon/updater_control.go: on for team, off for public unless a
// control file turns it on). A status without the field counts as paused.
export function shellAutoApplyEnabled(status: UpdateStatus | null | undefined): boolean {
  return status?.shellAutoApplyPaused === false;
}

export function shellUpdateNeedsDaemon(status: UpdateStatus | null | undefined): boolean {
  return status?.updateChannel === 'team' && daemonOnThisComputer();
}

// The desktop app is showing another computer's team daemon (cloud launch
// mode, or any relayed session), so the installer that daemon reports is the
// team build for THAT computer. Nothing in this window can install it here:
// - the remote daemon would launch it on the other computer (never asked);
// - the app's own updater reads the public releases only, and a public app
//   refuses any other flavour (shell_update_manifest.rs);
// - this computer's own daemon is not reachable from this page. It is not
//   running at all in cloud launch mode (lib.rs DaemonStart::single), and when
//   it is, it rejects cross-origin requests (daemon/auth.go originProtected)
//   and this page holds no token for it.
// So the remote status is not this computer's installer status. This
// computer's own daemon has to fetch and verify the team installer: in Local
// mode (shellUpdateNeedsDaemon), or, with a shell that can, from Cloud mode
// through the shell itself (lib/updateThisComputer.ts), which starts that
// daemon natively. The download page would hand a team computer the public
// installer, so it is never the answer here either.
// A browser tab has no desktop app on this computer and is not affected.
//
// The same holds whatever computer the window shows when this app is itself a
// team build (window.__SOLU_BACKEND__.shellFlavor, from shells that report
// it): its installer comes only through this computer's daemon, and the app's
// own updater refuses it (shell_update_manifest.rs TEAM_SHELL_UPDATE_UNAVAILABLE).
export function installerNeedsLocalMode(status: UpdateStatus | null | undefined): boolean {
  if (!isDesktopApp() || daemonOnThisComputer()) return false;
  return status?.updateChannel === 'team' || thisAppIsTeamBuild();
}

export function thisAppIsTeamBuild(): boolean {
  const g = (window as unknown as { __SOLU_BACKEND__?: { shellFlavor?: unknown } }).__SOLU_BACKEND__;
  return g?.shellFlavor === 'team';
}
