import { useStore } from './store';
import { confirmDialog } from './lib/dialog';
import { isEmbedded } from './embed';

// "Set up Linux (WSL)" from the setup checklist. The desktop shell's
// desktop_set_up_wsl command runs the guided confirm / install / verify
// dialogs and, when WSL works, restarts the app into it. When WSL is already
// installed that restart happens with no dialog of its own, so we always ask
// first: a restart closes open panes.

type InvokeFn = (cmd: string, args?: Record<string, unknown>) => Promise<unknown>;

export interface SoluBackendGlobal {
  requested?: 'wsl' | 'host' | 'dual';
  actual?: 'wsl' | 'host' | 'dual';
  fallback?: boolean;
  hostPlatform?: 'windows' | 'macos' | 'linux';
  launchMode?: 'local' | 'cloud';
  // Whether the desktop app saw a WSL distro on this PC at this launch. Null
  // or absent when it did not look (a mode was already saved, or an older app).
  wslDistro?: boolean | null;
}

export function readBackendGlobal(): SoluBackendGlobal | null {
  if (typeof window === 'undefined') return null;
  const g = (window as unknown as { __SOLU_BACKEND__?: SoluBackendGlobal }).__SOLU_BACKEND__;
  return g && typeof g === 'object' ? g : null;
}

function desktopInvoke(): InvokeFn | null {
  if (typeof window === 'undefined' || isEmbedded()) return null;
  const w = window as unknown as { __SOLU_TAURI__?: boolean; __TAURI_INTERNALS__?: { invoke?: InvokeFn } };
  return w.__SOLU_TAURI__ === true && typeof w.__TAURI_INTERNALS__?.invoke === 'function'
    ? w.__TAURI_INTERNALS__.invoke
    : null;
}

// Only the Windows desktop app, running projects on this PC, can set up WSL.
// A browser tab shows the status only.
export function canSetUpWslHere(): boolean {
  const bg = readBackendGlobal();
  return !!desktopInvoke() && bg?.hostPlatform === 'windows' && bg.launchMode !== 'cloud';
}

export type WslSetupOutcome = 'switching' | 'restart-pc' | 'declined' | 'failed' | 'low-disk' | 'busy';

// Plain-words follow-up for each outcome. "declined" and "failed" need none:
// the user chose, or the shell's own dialog already said why.
export function wslSetupOutcomeMessage(outcome: string): string | null {
  switch (outcome) {
    case 'switching':
      return 'Restarting SoluTerminal in Linux (WSL)…';
    case 'restart-pc':
      return 'Linux (WSL) is installed. Restart your PC, then open SoluTerminal again to finish.';
    case 'low-disk':
      return 'There isn’t enough free disk space to set up Linux (WSL). Free up some space and try again.';
    case 'busy':
      return 'Linux (WSL) setup is already running.';
    default:
      return null;
  }
}

export async function requestWslSetup(): Promise<void> {
  const invoke = desktopInvoke();
  const toast = useStore.getState().toast;
  if (!invoke) return;
  const ok = await confirmDialog({
    title: 'Switch to Linux (WSL)?',
    message:
      'SoluTerminal will set up Linux (WSL) if it isn’t ready yet. Windows may ask for permission, and setup can need a PC restart.\n\n' +
      'SoluTerminal then restarts in Linux (WSL), and open panes will close.',
    confirmLabel: 'Switch to Linux (WSL)',
    cancelLabel: 'Not now',
  });
  if (!ok) return;
  try {
    const outcome = String(await invoke('desktop_set_up_wsl'));
    const message = wslSetupOutcomeMessage(outcome);
    if (message) toast(message, { kind: 'info', ttlMs: 10000 });
  } catch {
    // Older desktop apps don't have the command: point at the tray item.
    toast('Right-click the SoluTerminal icon in the taskbar tray, then choose “Repair setup”.', {
      kind: 'info',
      icon: '!',
      ttlMs: 10000,
    });
  }
}
