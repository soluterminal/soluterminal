import type { PaneAutoMode, PaneInfo } from '../types';
import { currentRoutingScope } from '../api';
import { useStore } from '../store';
import { confirmDialog } from './dialog';

// Turning on Always approve is the one auto-approve change that needs an
// explicit yes, on every surface (header chip menu, pane menu, palette).
// Turning auto-approve down, or off, never asks.

export function alwaysApproveConfirmMessage(paneTitle?: string): string {
  const where = paneTitle?.trim() ? `“${paneTitle.trim()}”` : 'this pane';
  return `Every tool permission prompt in ${where} that SoluTerminal recognizes will be approved automatically, `
    + 'with no cancel window, so the agent can run commands and change files without asking you each time. '
    + 'Questions and plan changes still wait for you. It stays on until you change it.';
}

export function confirmAlwaysApprove(paneTitle?: string): Promise<boolean> {
  return confirmDialog({
    title: 'Turn on Always approve?',
    message: alwaysApproveConfirmMessage(paneTitle),
    confirmLabel: 'Always approve',
    cancelLabel: 'Cancel',
    danger: true,
  });
}

// Resolves true when the change may go ahead: only a move INTO Always approve
// asks first.
export async function confirmAutoModeChange(from: PaneAutoMode, to: PaneAutoMode, paneTitle?: string): Promise<boolean> {
  if (to !== 'always' || from === 'always') return true;
  return confirmAlwaysApprove(paneTitle);
}

// What a person is authorizing when they pick a mode: this pane, on this
// backend, in this view. Captured at the click; stillValid() is checked again
// after the confirmation, so a backend switch, navigation or a different pane
// taking the same id while the dialog is open can't receive the change.
export function captureAutoModeTarget(pane: Pick<PaneInfo, 'id' | 'createdAt'>): () => boolean {
  const scopeKey = currentRoutingScope().key;
  const generation = useStore.getState().navigationGeneration;
  const { id, createdAt } = pane;
  return () => {
    const st = useStore.getState();
    const now = st.panes[id];
    return currentRoutingScope().key === scopeKey
      && st.navigationGeneration === generation
      && !!now && now.createdAt === createdAt;
  };
}

// The store-backed apply used by the header chip and the palette: confirm a
// move into Always approve, then apply only if the captured target still holds.
export async function applyAutoModeChange(pane: Pick<PaneInfo, 'id' | 'createdAt' | 'title' | 'autoMode'>, to: PaneAutoMode): Promise<boolean> {
  const from = (pane.autoMode ?? '') as PaneAutoMode;
  if (from === to) return false;
  const stillValid = captureAutoModeTarget(pane);
  if (!(await confirmAutoModeChange(from, to, pane.title))) return false;
  if (!stillValid()) {
    if (to === 'always') useStore.getState().toast('The pane changed while you were confirming, so Always approve was not turned on.', { kind: 'warn' });
    return false;
  }
  void useStore.getState().setPaneAutoMode(pane.id, to).catch(() => {});
  return true;
}
