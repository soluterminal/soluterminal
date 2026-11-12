import { api, ApiError, type RoutingScope } from '../api';
import { useStore } from '../store';
import { confirmDialog } from './dialog';

export interface PaneActionContext { scope: RoutingScope; valid: () => boolean }
async function restart(id:string, fresh:boolean, force:boolean, action?:PaneActionContext) {
 if (!action) return force ? useStore.getState().restartPane(id,fresh,true) : fresh ? useStore.getState().restartPane(id,true) : useStore.getState().restartPane(id);
 if (!action.valid()) return;
 const pane = await api.restartPane(id,fresh,force,action.scope);
 if(action.valid()) useStore.setState(s=>({panes:{...s.panes,[id]:pane}}));
}

type RestartFailure = { status?: number; needsFresh: boolean; reason: string };

function parseRestartFailure(error: unknown): RestartFailure {
  const err = error as ApiError;
  const body = (err && typeof err === 'object' ? err.body : undefined) as { needsFresh?: boolean; error?: string } | undefined;
  return {
    status: err?.status,
    needsFresh: Boolean(body?.needsFresh),
    reason: body?.error || err?.message || 'try again',
  };
}

function toastRestartFailure(reason: string): void {
  useStore.getState().toast(
    `Couldn't restart pane: ${reason}`,
    { kind: 'error', icon: '⚠', ttlMs: 5000 },
  );
}

// Shared "Restart pane" flow, used by the context menu, the command palette and
// the per-pane stale-agent dot. It restarts the pane's agent in place on the
// installed binary, resuming its conversation.
//
//   - 409 + body.needsFresh → offer a fresh restart with an explicit confirm.
//   - any other 409 (busy)  → offer a confirmed "restart anyway" that forces
//     past the settle gate, so a pane wedged busy by a silent/logged-out
//     provider is not permanently un-restartable.
//   - anything else         → toast.
export async function restartPaneFlow(id: string, action?: PaneActionContext): Promise<void> {
  try {
    if(action && !action.valid()) return;
    await restart(id,false,false,action);
    return;
  } catch (error) {
    if(action && !action.valid())return;
    const failure = parseRestartFailure(error);
    if (failure.status === 409 && failure.needsFresh) {
      await restartPaneFreshFlow(id, failure.reason,action);
      return;
    }
    if (failure.status === 409) {
      await restartPaneForceFlow(id, failure.reason,action);
      return;
    }
    toastRestartFailure(failure.reason);
  }
}

// Confirm-and-force: the user explicitly accepts interrupting whatever the pane
// is doing. Used when a plain restart is refused as busy.
export async function restartPaneForceFlow(id: string, reason?: string, action?: PaneActionContext): Promise<void> {
  const detail = reason && reason !== 'try again' ? ` (${reason})` : '';
  const ok = await confirmDialog({
    title: 'Restart this pane anyway?',
    message: `This pane looks busy${detail}. Restarting will interrupt its current turn or parked prompt. Continue?`,
    confirmLabel: 'Restart anyway',
    danger: true,
  });
  if (!ok || action && !action.valid()) return;
  try {
    await restart(id,false,true,action);
  } catch (error) {
    if(action && !action.valid())return;
    const failure = parseRestartFailure(error);
    if (failure.status === 409 && failure.needsFresh) {
      await restartPaneFreshFlow(id, failure.reason,action);
      return;
    }
    toastRestartFailure(failure.reason);
  }
}

// Confirm-and-restart-fresh, the escape hatch for a pane whose conversation is
// not durably resumable (or whose live provider identity cannot be verified).
// Used by restartPaneFlow's 409 handling, restartPaneForceFlow's needsFresh
// fallback. Always forced:
// the user already confirmed an explicit restart.
export async function restartPaneFreshFlow(id: string, reason?: string, action?: PaneActionContext): Promise<void> {
  const detail = reason && reason !== 'try again' ? ` (${reason})` : '';
  const ok = await confirmDialog({
    title: 'Restart pane with a new session?',
    message: `This pane can't resume its conversation${detail}. Restarting fresh starts a new session in the same pane; the old transcript stays on disk and may still be recoverable manually.`,
    confirmLabel: 'Restart fresh',
    danger: true,
  });
  if (!ok || action && !action.valid()) return;
  try {
    await restart(id,true,true,action);
  } catch (freshError) {
    if(action && !action.valid())return;
    toastRestartFailure((freshError as Error)?.message || 'try again');
  }
}

// Reopen a terminal pane's own shell. Always fresh: a plain restart resumes an
// agent conversation the terminal had adopted, in place of the shell. No
// extra confirm unless the pane is busy; the caller has already said what a
// restart ends.
export async function restartTerminalFlow(id: string): Promise<void> {
  try {
    await restart(id, true, false);
    return;
  } catch (error) {
    const failure = parseRestartFailure(error);
    if (failure.status !== 409) {
      toastRestartFailure(failure.reason);
      return;
    }
    const detail = failure.reason && failure.reason !== 'try again' ? ` (${failure.reason})` : '';
    const ok = await confirmDialog({
      title: 'Restart this terminal anyway?',
      message: `This terminal looks busy${detail}. Restarting ends whatever is running in it.`,
      confirmLabel: 'Restart anyway',
      danger: true,
    });
    if (!ok) return;
  }
  try {
    await restart(id, true, true);
  } catch (error) {
    toastRestartFailure(parseRestartFailure(error).reason);
  }
}
