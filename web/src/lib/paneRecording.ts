// Pane output recording (daemon/pane_recorder*.go), the app's side: which
// panes can be recorded, the person's switch and delete, and the wording.
//
// The marks come from daemon state only (PaneInfo.recording). The wording is
// limited to what the security owner cleared: "saved on this computer only",
// "deleted after 7 days", "never sent to the cloud".
import { api, type RoutingScope } from '../api';
import { useStore } from '../store';
import { confirmDialog } from './dialog';
import { effectivePaneAgent } from './agentKind';
import type { PaneInfo } from '../types';

// The daemon records only running Claude and Codex panes, and a Windows-side
// daemon refuses the switch; the menu offers it only where it can work.
export function paneRecordable(pane: PaneInfo): boolean {
  const agent = effectivePaneAgent(pane);
  return pane.status === 'running' && pane.backend !== 'win' && (agent === 'claude' || agent === 'codex');
}

export function paneRecordingOn(pane: PaneInfo | undefined): boolean {
  return pane?.recording?.on === true;
}

export const RECORDING_TOOLTIP =
  "Recording this pane's output. Saved on this computer only, deleted after 7 days, never sent to the cloud. Right-click the pane to stop.";

export function recordingStoppedText(reason: string): string {
  return `recording stopped: ${reason}`;
}

function report(e: unknown, fallback: string) {
  useStore.getState().toast(e instanceof Error && e.message ? e.message : fallback, { kind: 'error' });
}

// startPaneRecording asks first: the person should know what is kept.
// stillValid is checked after the person's answer: the pane they agreed to
// record must still be the one shown.
export async function startPaneRecording(pane: PaneInfo, scope?: RoutingScope, stillValid: () => boolean = () => true): Promise<void> {
  const ok = await confirmDialog({
    title: 'Record this pane?',
    message:
      `Saves everything "${pane.title || 'this pane'}" shows to a file. Saved on this computer only, ` +
      'deleted after 7 days, never sent to the cloud. Typed input is not recorded, but text the pane ' +
      'echoes back is. Switch recording off before a login, sudo or anything that prints a secret. ' +
      'It goes off by itself after 8 hours, and when the pane closes or restarts.',
    confirmLabel: 'Start recording',
  });
  if (!ok) return;
  if (!stillValid()) {
    useStore.getState().toast('The pane changed while you were deciding; recording was not started.', { kind: 'warn' });
    return;
  }
  try {
    const rec = await api.setPaneRecording(pane.id, true, scope);
    useStore.getState().applyPaneRecording(pane.id, rec);
  } catch (e) {
    report(e, 'Could not start recording');
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
