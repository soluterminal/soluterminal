// Pane output recording (daemon/pane_recorder*.go), the app's side: whether
// the menu can switch a pane on and why not, the person's start dialog (time
// limit and per-pane overrides), "record every pane in this workspace", stop,
// delete and the wording.
//
// The marks come from daemon state only (PaneInfo.recording). The wording is
// limited to what the security owner cleared: "saved on this computer only",
// "deleted after N days", "never sent to the cloud".
import { api, type RoutingScope } from '../api';
import { useStore } from '../store';
import { confirmDialog, recordingDialog } from './dialog';
import { effectivePaneAgent } from './agentKind';
import type { RecordingDialogPane } from '../components/RecordingDialogBody';
import type { PaneInfo, RecordingCheck } from '../types';

export const RECORDING_DEFAULT_DELETE_DAYS = 7;
export const RECORDING_DEFAULT_HOURS = 8;
/** The time-limit choices; the daemon refuses anything outside 1 to 24 hours. */
export const RECORDING_HOUR_CHOICES = [1, 2, 4, 8, 12, 24];

// A pane-recording.v1 daemon records only running Claude and Codex panes, and
// a Windows-side daemon refuses the switch.
export function paneRecordable(pane: PaneInfo): boolean {
  const agent = effectivePaneAgent(pane);
  return pane.status === 'running' && pane.backend !== 'win' && (agent === 'claude' || agent === 'codex');
}

export function paneRecordingOn(pane: PaneInfo | undefined): boolean {
  return pane?.recording?.on === true;
}

export function recordingTooltip(pane?: PaneInfo): string {
  // A pane-recording.v2 daemon reports the deletion age in force; an older
  // one always keeps 7 days.
  const deleteAfterDays = pane?.recording?.deleteAfterDays ?? RECORDING_DEFAULT_DELETE_DAYS;
  const hours = pane?.recording?.hours;
  return `Recording this pane's output. Saved on this computer only, deleted after ${deleteAfterDays} days, never sent to the cloud.` +
    (hours ? ` Goes off by itself after ${hours === 1 ? '1 hour' : `${hours} hours`}.` : '') +
    ' Right-click the pane to stop.';
}

export const RECORDING_TOOLTIP = recordingTooltip();

export function recordingStoppedText(reason: string): string {
  return `recording stopped: ${reason}`;
}

/** What the daemon behind a pane's scope offers: nothing yet (asking), no
 *  recording, the first version (agent panes, no choices) or the second. */
export type RecordingServed = 'checking' | 'unreachable' | 'none' | 'v1' | 'v2';

/** Why a pane can't be switched on, and what to do, by daemon code. */
const UNAVAILABLE_TEXT: Record<string, string> = {
  windows: "Recording isn't available for Windows panes yet. Use a WSL or Linux pane.",
  not_running: "The pane isn't running. Restart it, then record.",
  no_data_dir: 'SoluTerminal has no folder to save recordings in. Restart SoluTerminal.',
  hard_folder: 'This pane works in a folder that is never recorded (keys and tokens live there). Change to another folder, or open a new pane elsewhere.',
  cwd_unknown: "SoluTerminal can't read this pane's folder, so it can't check it. Recording isn't available here yet.",
};

/** The skip reasons the workspace switch answers with, in plain words. */
const SKIP_TEXT: Record<string, string> = {
  ...UNAVAILABLE_TEXT,
  handle: 'its handle is on the do-not-record list',
  folder: 'it moved into a folder on the do-not-record list',
  'folder:launch_security': 'it works in launch/roles/security',
  'folder:launch_deploy': 'it works in launch/deploy',
  'folder:config': 'it works in ~/.config',
  'folder:etc': 'it works in /etc',
  shell: "it's a plain shell",
  not_confirmed: 'it opened after you confirmed',
  pane_changed: 'it restarted after you confirmed',
  already_recording: 'it is already recording',
  not_in_workspace: "it's no longer in this workspace",
  disk_cap: 'the recordings cap is full',
  recordings_folder: "the recordings folder isn't safe to write to",
};

export function recordingSkipText(code: string): string {
  return SKIP_TEXT[code] ?? 'it could not be started';
}

/** The menu item's state: on, startable, or greyed with the reason. */
export type RecordingMenuState =
  | { kind: 'on' }
  | { kind: 'start' }
  | { kind: 'greyed'; reason: string };

export function recordingMenuState(pane: PaneInfo, served: RecordingServed, check?: RecordingCheck | null): RecordingMenuState {
  if (pane.recording?.on) return { kind: 'on' };
  switch (served) {
    case 'checking':
      return { kind: 'greyed', reason: 'Checking whether this pane can be recorded…' };
    case 'unreachable':
      return { kind: 'greyed', reason: "Couldn't reach SoluTerminal to check. Close this menu and open it again." };
    case 'none':
      return { kind: 'greyed', reason: 'Update SoluTerminal to record panes.' };
    case 'v1':
      return paneRecordable(pane)
        ? { kind: 'start' }
        : { kind: 'greyed', reason: 'Update SoluTerminal to record this pane. This version records Claude and Codex panes only.' };
  }
  if (!check) return { kind: 'greyed', reason: 'Checking whether this pane can be recorded…' };
  const code = check.unavailable || check.hard;
  if (code) return { kind: 'greyed', reason: UNAVAILABLE_TEXT[code] ?? "This pane can't be recorded." };
  return { kind: 'start' };
}

function report(e: unknown, fallback: string) {
  useStore.getState().toast(e instanceof Error && e.message ? e.message : fallback, { kind: 'error' });
}

function recordingMessage(deleteAfterDays: number, workspace: boolean): string {
  return (workspace
    ? 'Saves what each pane below shows to its own file. Panes opened later are not added. '
    : 'Saves everything this pane shows to a file. ') +
    `Saved on this computer only, deleted after ${deleteAfterDays} days, never sent to the cloud. ` +
    'Typed input is not recorded, but text a pane echoes back is. Switch recording off before a login, ' +
    'sudo or anything that prints a secret. It goes off by itself at the time limit, and when a pane ' +
    'closes or restarts.';
}

// The deletion age the dialog states, read from the daemon. Without it the
// dialog can't say how long recordings are kept, so nothing starts.
async function deleteAfterDaysOf(scope?: RoutingScope): Promise<number | null> {
  try {
    return (await api.getRecordingSettings(scope)).deleteAfterDays;
  } catch {
    useStore.getState().toast("Couldn't read the recording settings, so recording was not started. Try again.", { kind: 'error' });
    return null;
  }
}

// The deletion age must still be the one the dialog stated when the person
// confirmed; if it changed meanwhile they are asked again.
async function sameDeleteAfterDays(stated: number, scope?: RoutingScope): Promise<boolean> {
  let now: number | null = null;
  try {
    now = (await api.getRecordingSettings(scope)).deleteAfterDays;
  } catch { /* unreadable: treated as changed */ }
  if (now === stated) return true;
  useStore.getState().toast('How long recordings are kept changed while you were deciding, so recording was not started. Try again to see the new setting.', { kind: 'warn' });
  return false;
}

function paneName(pane: PaneInfo): string {
  const handle = pane.identity?.handle;
  return pane.title || (handle ? `@${handle}` : 'this pane');
}

// startPaneRecording asks first: the person should know what is kept. With a
// v2 daemon the dialog also takes the time limit and names each default the
// pane would be refused by, to override for this switch-on only. stillValid
// is checked after the person's answer: the pane they agreed to record must
// still be the one shown.
export async function startPaneRecording(
  pane: PaneInfo,
  scope?: RoutingScope,
  stillValid: () => boolean = () => true,
  v2?: { check: RecordingCheck },
): Promise<void> {
  let terms: { hours: number; override: NonNullable<RecordingCheck['defaults']>; pid?: number } | undefined;
  if (v2) {
    const days = await deleteAfterDaysOf(scope);
    if (days === null) return;
    const choice = await recordingDialog({
      title: 'Record this pane?',
      message: recordingMessage(days, false),
      confirmLabel: 'Start recording',
      recording: {
        panes: [{ id: pane.id, name: paneName(pane), defaults: v2.check.defaults ?? [] }],
        hours: RECORDING_HOUR_CHOICES,
        defaultHours: RECORDING_DEFAULT_HOURS,
        deleteAfterDays: days,
        workspace: false,
      },
    });
    if (!choice) return;
    if (!(await sameDeleteAfterDays(days, scope))) return;
    // The daemon refuses if the pane restarted since (same id, new process).
    terms = { hours: choice.hours, override: choice.panes[0]?.override ?? [], ...(pane.pid ? { pid: pane.pid } : {}) };
  } else {
    const ok = await confirmDialog({
      title: 'Record this pane?',
      message:
        `Saves everything "${pane.title || 'this pane'}" shows to a file. Saved on this computer only, ` +
        `deleted after ${RECORDING_DEFAULT_DELETE_DAYS} days, never sent to the cloud. Typed input is not recorded, but text the pane ` +
        'echoes back is. Switch recording off before a login, sudo or anything that prints a secret. ' +
        'It goes off by itself after 8 hours, and when the pane closes or restarts.',
      confirmLabel: 'Start recording',
    });
    if (!ok) return;
  }
  if (!stillValid()) {
    useStore.getState().toast('The pane changed while you were deciding; recording was not started.', { kind: 'warn' });
    return;
  }
  try {
    const rec = await api.setPaneRecording(pane.id, true, scope, terms);
    useStore.getState().applyPaneRecording(pane.id, rec);
  } catch (e) {
    report(e, 'Could not start recording');
  }
}

/** One confirm for every pane in the workspace: it lists each pane with the
 *  defaults that apply to it (each overridable), and the panes that will be
 *  skipped and why. Panes opened after the confirm are never added. */
export async function startWorkspaceRecording(
  workspace: string,
  panes: PaneInfo[],
  scope?: RoutingScope,
  stillValid: () => boolean = () => true,
): Promise<void> {
  const checks = await Promise.all(panes.map((p) => api.getPaneRecording(p.id, scope).then((r) => r.check).catch(() => undefined)));
  const rows: RecordingDialogPane[] = panes.map((p, i) => {
    const check = checks[i];
    const code = p.recording?.on ? 'on' : !check ? 'unknown' : check.unavailable || check.hard;
    const skip = code === 'on' ? 'already recording' : code === 'unknown' ? "couldn't check it" : code ? (UNAVAILABLE_TEXT[code] ?? "it can't be recorded") : undefined;
    return { id: p.id, name: paneName(p), defaults: skip ? [] : (check?.defaults ?? []), skip };
  });
  const days = await deleteAfterDaysOf(scope);
  if (days === null) return;
  const choice = await recordingDialog({
    title: 'Record every pane in this workspace?',
    message: recordingMessage(days, true),
    confirmLabel: 'Start recording',
    recording: { panes: rows, hours: RECORDING_HOUR_CHOICES, defaultHours: RECORDING_DEFAULT_HOURS, deleteAfterDays: days, workspace: true },
  });
  if (!choice) return;
  if (!stillValid()) {
    useStore.getState().toast('The workspace changed while you were deciding; recording was not started.', { kind: 'warn' });
    return;
  }
  // Each pane must still be the process the dialog showed: a pane restarted
  // or resumed meanwhile keeps its id but is not what the person agreed to.
  const shown = new Map(panes.map((p) => [p.id, p]));
  const now = useStore.getState().panes;
  const changed = choice.panes.filter((c) => {
    const was = shown.get(c.id);
    const is = now[c.id];
    return !was || !is || is.pid !== was.pid || is.status !== was.status;
  });
  if (changed.length > 0) {
    choice.panes = choice.panes.filter((c) => !changed.includes(c));
    useStore.getState().toast(
      `${changed.map((c) => paneName(shown.get(c.id) ?? ({ id: c.id, title: '' } as PaneInfo))).join(', ')} changed while you were deciding and won't be recorded.`,
      { kind: 'warn' },
    );
    if (choice.panes.length === 0) return;
  }
  // The marks follow from the daemon's pane-recording events, as for one pane.
  try {
    if (!(await sameDeleteAfterDays(days, scope))) return;
    // Each pane names the process the person saw; the daemon refuses one
    // restarted since, whatever this window has cached.
    const withPid = choice.panes.map((c) => ({ ...c, ...(shown.get(c.id)?.pid ? { pid: shown.get(c.id)!.pid } : {}) }));
    const res = await api.setWorkspaceRecording({ workspace, on: true, hours: choice.hours, panes: withPid }, scope);
    const chosen = new Set(choice.panes.map((p) => p.id));
    const name = (id: string) => paneName(useStore.getState().panes[id] ?? ({ id, title: '' } as PaneInfo));
    const notStarted = res.skipped.filter((s) => chosen.has(s.id)).map((s) => `${name(s.id)}: ${recordingSkipText(s.reason)}`);
    const n = res.recorded.length;
    useStore.getState().toast(
      (n === 1 ? 'Recording 1 pane.' : `Recording ${n} panes.`) + (notStarted.length ? ` Not started: ${notStarted.join('; ')}.` : ''),
      { kind: notStarted.length ? 'warn' : 'success' },
    );
  } catch (e) {
    report(e, 'Could not start recording');
  }
}

export async function stopWorkspaceRecording(workspace: string, scope?: RoutingScope): Promise<void> {
  try {
    await api.setWorkspaceRecording({ workspace, on: false }, scope);
  } catch (e) {
    report(e, 'Could not stop recording');
  }
}

export async function stopPaneRecording(pane: PaneInfo, scope?: RoutingScope): Promise<void> {
  try {
    const rec = await api.setPaneRecording(pane.id, false, scope);
    useStore.getState().applyPaneRecording(pane.id, rec);
  } catch (e) {
    report(e, 'Could not stop recording');
  }
}

export async function dismissPaneRecordingNotice(pane: PaneInfo, scope?: RoutingScope): Promise<void> {
  try {
    const rec = await api.dismissPaneRecordingNotice(pane.id, scope);
    useStore.getState().applyPaneRecording(pane.id, rec);
  } catch (e) {
    report(e, 'Could not dismiss the notice');
  }
}

export async function deletePaneRecordings(pane: PaneInfo, scope?: RoutingScope, stillValid: () => boolean = () => true): Promise<void> {
  const ok = await confirmDialog({
    title: "Delete this pane's recordings?",
    message: `Deletes every recording of "${pane.title || 'this pane'}" from this computer, and stops a recording that is running.`,
    confirmLabel: 'Delete recordings',
    danger: true,
    focusCancel: true,
  });
  if (!ok || !stillValid()) return;
  try {
    const { deleted } = await api.deletePaneRecordings(pane.id, scope);
    useStore.getState().applyPaneRecording(pane.id, undefined);
    useStore.getState().toast(deleted === 1 ? '1 recording deleted' : `${deleted} recordings deleted`, { kind: 'success', ttlMs: 2200 });
  } catch (e) {
    report(e, 'Could not delete the recordings');
  }
}
