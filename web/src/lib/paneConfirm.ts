// An agent asked to close or change a pane it did not start, saying the
// person asked for it (kill_pane / set_pane_info with user_requested). The
// daemon holds the action until the person allows it here
// (daemon/pane_confirm.go).
//
// Open requests stay on screen in their own strip (PaneConfirmHost) until
// they are answered or expire, apart from the capped toast queue so other
// notifications can never push one out of reach. Review opens the Allow /
// Deny dialog. It never opens the dialog by itself: a dialog that pops up
// while the person is typing in a terminal could take a stray Enter as
// "Allow".
import { create } from 'zustand';
import { api, ApiError, type PaneConfirmRequest } from '../api';
import { useStore } from '../store';
import { confirmDialog } from './dialog';

export interface PendingPaneConfirm {
  id: string;
  message: string;
}

export const usePaneConfirms = create<{ pending: PendingPaneConfirm[] }>(() => ({ pending: [] }));

/** Show an open request once. */
export function notePaneConfirm(id: string, message: string): void {
  if (!id) return;
  usePaneConfirms.setState((s) => (s.pending.some((p) => p.id === id) ? s : { pending: [...s.pending, { id, message }] }));
}

/** The request was answered (here or in another window) or expired. */
export function forgetPaneConfirm(id: string): void {
  usePaneConfirms.setState((s) => ({ pending: s.pending.filter((p) => p.id !== id) }));
}

/** Show exactly the daemon's open requests, after a (re)connect: ones raised
 *  while this window was closed appear, ones answered meanwhile go. */
export async function resyncPaneConfirms(): Promise<void> {
  try {
    const { pending } = await api.paneConfirmations();
    usePaneConfirms.setState({ pending: (pending ?? []).map((p) => ({ id: p.id, message: p.message })) });
  } catch {
    // An older daemon without the endpoint: nothing to show.
  }
}

/** Open the Allow / Deny dialog for a request and send the answer. */
export async function reviewPaneConfirm(id: string): Promise<void> {
  const st = useStore.getState();
  let req: PaneConfirmRequest | undefined;
  try {
    req = (await api.paneConfirmations()).pending.find((p) => p.id === id);
  } catch (error) {
    st.toast(`Couldn't load the request: ${String(error)}`, { kind: 'error' });
    return;
  }
  if (!req) {
    forgetPaneConfirm(id);
    st.toast('That request was already answered or has expired.', { kind: 'info' });
    return;
  }
  const allow = await confirmDialog({
    title: req.title,
    message: `${req.message} Only allow it if you asked for it.`,
    confirmLabel: 'Allow',
    cancelLabel: 'Deny',
    danger: req.action === 'close',
  });
  let answer: Awaited<ReturnType<typeof api.answerPaneConfirmation>>;
  try {
    answer = await api.answerPaneConfirmation(id, allow);
  } catch (error) {
    // Still open (refused, or the daemon was unreachable): it stays listed.
    st.toast(`Couldn't send your answer: ${answerErrorText(error)}`, { kind: 'error' });
    return;
  }
  forgetPaneConfirm(id);
  if (!answer) {
    st.toast('That request was already answered or has expired.', { kind: 'info' });
  } else if (!allow) {
    st.toast(`Denied: ${req.pane} was left as it is.`, { kind: 'info' });
  } else if (answer.failed) {
    st.toast(answer.result || 'The change could not be made.', { kind: 'error' });
  } else {
    const done = req.action === 'close' ? 'Closed' : req.action === 'rename' ? 'Renamed' : 'Changed';
    st.toast(`${done} ${req.pane}.`, { kind: 'success' });
  }
}

/** The daemon's own sentence for a refusal (caller_authority.go), else the error. */
function answerErrorText(error: unknown): string {
  if (error instanceof ApiError && error.body && typeof error.body === 'object') {
    const message = (error.body as { message?: unknown }).message;
    if (typeof message === 'string' && message) return message;
  }
  return error instanceof Error ? error.message : String(error);
}

/** Test hook. */
export function resetPaneConfirmsForTest(): void {
  usePaneConfirms.setState({ pending: [] });
}
