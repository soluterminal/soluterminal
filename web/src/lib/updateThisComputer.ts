import { useSyncExternalStore } from 'react';
import { tauriInvoke } from '../tauri-browser';
import { isCloudLaunchMode } from './useCloudShellUpdate';

// "Update this computer's desktop app" in Cloud launch mode.
//
// On the team channel only this computer's own daemon can fetch and verify the
// desktop installer (shellUpdateRoute.ts installerNeedsLocalMode), and in
// Cloud mode that daemon is not running and this page cannot reach it. The
// desktop app does it natively instead (desktop/src-tauri/src/
// this_computer_update.rs): it starts this computer's daemon headless, has it
// check and launch this computer's installer, and reports back. The page
// passes nothing: no URL, path or version.
//
// A shell that has the command and runs in Cloud mode says so in
// window.__SOLU_BACKEND__.updateThisComputer; older shells keep #750's
// "Switch to Local", which also stays as the fallback when this fails.

type Toast = (
  message: string,
  opts?: { kind?: 'info' | 'success' | 'warn' | 'error'; icon?: string; ttlMs?: number },
) => void;

export type ThisComputerPhase = 'confirming' | 'starting' | 'checking' | 'installing';

export interface ThisComputerUpdateResult {
  state: 'cancelled' | 'launched' | 'up-to-date' | 'busy' | 'refused' | 'failed' | 'unavailable';
  installerVersion?: string;
  currentVersion?: string;
  code?: string;
  message?: string;
}

export const THIS_COMPUTER_PHASE_LABEL: Record<ThisComputerPhase, string> = {
  // The desktop app asks in its own dialog before anything starts.
  confirming: 'Confirm in the dialog…',
  starting: "Starting this computer's SoluTerminal…",
  checking: 'Checking this computer for updates…',
  installing: 'Downloading the installer…',
};

export const THIS_COMPUTER_FAILED_FALLBACK = "Couldn't update this computer from here.";

export function canUpdateThisComputerHere(): boolean {
  if (!tauriInvoke() || !isCloudLaunchMode()) return false;
  const g = (window as unknown as { __SOLU_BACKEND__?: { updateThisComputer?: unknown } }).__SOLU_BACKEND__;
  return g?.updateThisComputer === true;
}

interface UpdateThisComputerState {
  running: boolean;
  phase: ThisComputerPhase | null;
  // Set when the last try did not install anything for a reason other than
  // "up to date"; the surfaces then offer Switch to Local as the fallback.
  failure: string | null;
}

let state: UpdateThisComputerState = { running: false, phase: null, failure: null };
const listeners = new Set<() => void>();

function setState(next: Partial<UpdateThisComputerState>): void {
  state = { ...state, ...next };
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

const PHASES: readonly string[] = ['starting', 'checking', 'installing'];

if (typeof window !== 'undefined') {
  window.addEventListener('solu:update-this-computer', (event) => {
    const phase = (event as CustomEvent<{ phase?: unknown }>).detail?.phase;
    if (state.running && typeof phase === 'string' && PHASES.includes(phase)) {
      setState({ phase: phase as ThisComputerPhase });
    }
  });
}

export function useUpdateThisComputer(): UpdateThisComputerState {
  return useSyncExternalStore(subscribe, () => state);
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value ? value : undefined;
}

function normalize(raw: unknown): ThisComputerUpdateResult {
  const r = raw && typeof raw === 'object' ? raw as Record<string, unknown> : {};
  const known = ['cancelled', 'launched', 'up-to-date', 'busy', 'refused', 'failed'] as const;
  const stateValue = known.find((s) => s === r.state) ?? 'failed';
  return {
    state: stateValue,
    installerVersion: text(r.installerVersion),
    currentVersion: text(r.currentVersion),
    code: text(r.code),
    message: text(r.message),
  };
}

// Runs the native flow once (a second click while it runs is ignored) and
// tells the user what happened.
export async function updateThisComputer(toast: Toast): Promise<ThisComputerUpdateResult> {
  if (state.running) return { state: 'busy' };
  const invoke = tauriInvoke();
  if (!invoke || !canUpdateThisComputerHere()) return { state: 'unavailable' };
  setState({ running: true, phase: 'confirming', failure: null });
  let result: ThisComputerUpdateResult;
  try {
    result = normalize(await invoke('desktop_update_this_computer'));
  } catch (err) {
    console.warn('desktop_update_this_computer invoke failed:', err);
    result = { state: 'unavailable' };
  }
  let failure: string | null = null;
  switch (result.state) {
    case 'cancelled':
      // The user said no in the desktop app's own dialog: nothing started.
      break;
    case 'launched':
      toast('Installer opened. Follow it to finish; the app reopens in Cloud mode.', { kind: 'success', icon: '⬆', ttlMs: 9000 });
      break;
    case 'up-to-date':
      toast(
        `This computer's desktop app is up to date${result.currentVersion ? ` (v${result.currentVersion})` : ''}.`,
        { kind: 'info' },
      );
      break;
    case 'busy':
      toast(result.message ?? "This computer's desktop app is already being updated.", { kind: 'info' });
      break;
    default:
      failure = result.message ?? THIS_COMPUTER_FAILED_FALLBACK;
  }
  setState({ running: false, phase: null, failure });
  return result;
}

// Tests only.
export function resetUpdateThisComputerForTest(): void {
  state = { running: false, phase: null, failure: null };
  listeners.forEach((listener) => listener());
}
