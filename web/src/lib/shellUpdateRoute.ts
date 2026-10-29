import { isRelayedSession, useCloud } from '../cloud';
import type { UpdateStatus } from '../types';
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
