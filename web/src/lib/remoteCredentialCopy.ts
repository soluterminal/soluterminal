// What the Credentials section says when it is viewed through the cloud
// relay (Cloud mode, a phone, or the desktop's Cloud mode on the same
// computer). The daemon lets an approved device switch accounts and turn
// auto-switch on or off from there; every other account change stays on the
// computer itself (daemon/claudecreds_relay_switch.go).

/**
 * Whether this view can switch accounts (and which device rules apply).
 * canSwitchTeam: whether team rows may be chosen too (never over the relay).
 */
export type RemoteCredentialAccess = { relayed: boolean; canSwitch: boolean; canSwitchTeam: boolean };

export const LOCAL_CREDENTIAL_ACCESS: RemoteCredentialAccess = { relayed: false, canSwitch: true, canSwitchTeam: true };

/** Tooltip on every account control that stays local-only. */
export const REMOTE_VIEW_NOTE = 'Viewing remotely. Account changes are made on the computer itself.';

/** Tooltip on a team row when this device may switch its own accounts only. */
export const REMOTE_TEAM_NOTE = 'Viewing remotely. Team accounts are chosen on the computer itself.';

/** Whether this view may not choose a row (team rows need canSwitchTeam too). */
export function remoteSwitchLocked(remote: RemoteCredentialAccess, team: boolean): boolean {
  return remote.relayed && (!remote.canSwitch || (team && !remote.canSwitchTeam));
}

/** The tooltip for a row remoteSwitchLocked locks. */
export function remoteSwitchNote(remote: RemoteCredentialAccess): string {
  return remote.canSwitch ? REMOTE_TEAM_NOTE : REMOTE_VIEW_NOTE;
}

/** Section note when this device may switch accounts from here. */
export const REMOTE_SWITCH_NOTE =
  'Viewing remotely. You can switch accounts and turn auto-switch on or off here; other account changes are made on the computer itself.';

/** Added to the section note when this device may not switch yet. */
export const REMOTE_SWITCH_SETUP =
  'To switch accounts from here, choose "Only devices I approve" in Settings › Devices on the computer and approve this device.';

/** Shown instead of the daemon's raw refusal. */
export const REMOTE_REFUSED = "Can't change this from a remote session. Do it on the computer itself.";

/** The message to show for a failed account change. */
export function credentialErrorMessage(error: unknown, remote: RemoteCredentialAccess): string {
  const message = error instanceof Error ? error.message : String(error);
  if (remote.relayed && /local-only/i.test(message)) return REMOTE_REFUSED;
  return message;
}
