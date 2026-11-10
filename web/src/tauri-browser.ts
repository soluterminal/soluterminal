// Desktop-shell helpers. Each wraps one Tauri command or plugin call and
// degrades quietly outside the desktop app (or on an older installed shell
// that predates the command): feature-detected via try/catch on the invoke
// rather than a version check, so an old shell falls back instead of
// throwing at the user.

import { isEmbedded, openEmbedUrl } from './embed';

type InvokeFn = (cmd: string, args?: Record<string, unknown>) => Promise<unknown>;

export function tauriInvoke(): InvokeFn | null {
  if (isEmbedded()) return null;
  // Tauri 2 exposes invoke via both __TAURI_INTERNALS__ (runtime) and
  // the @tauri-apps/api/core module (import). We use the runtime form
  // to avoid adding a hard build dependency on @tauri-apps/api that
  // would bloat the cloud/PWA bundle for a feature only used in the
  // desktop app.
  const w = window as unknown as {
    __SOLU_TAURI__?: boolean;
    __TAURI_INTERNALS__?: { invoke?: InvokeFn };
  };
  if (!w.__SOLU_TAURI__) return null;
  const inv = w.__TAURI_INTERNALS__?.invoke;
  return typeof inv === 'function' ? inv : null;
}

export function isDesktopApp(): boolean {
  return tauriInvoke() !== null;
}

export type AutostartStatus = { supported: boolean; enabled: boolean };

export async function getAutostartStatus(): Promise<AutostartStatus | null> {
  const invoke = tauriInvoke();
  if (!invoke) return null;
  try {
    return await invoke('desktop_get_autostart') as AutostartStatus;
  } catch {
    // The daemon-served web bundle can be newer than the installed shell.
    return null;
  }
}

export async function setAutostart(enabled: boolean): Promise<AutostartStatus> {
  const invoke = tauriInvoke();
  if (!invoke) throw new Error('Not running in the desktop app');
  return await invoke('desktop_set_autostart', { enabled }) as AutostartStatus;
}

// Multi-monitor (Phase 1a): spawn a second full-app window the user can
// drag to another monitor. The new window loads the same daemon URL +
// backend/token as the main window and boots with its own per-window
// identity (see workspace_window.rs). Returns false (no-op) outside the
// desktop app or if the shell predates the command.
export async function openWorkspaceWindow(): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  try {
    await invoke('open_workspace_window');
    return true;
  } catch (err) {
    // Older shell without the command, or cloud mode (Rust rejects it).
    console.warn('open_workspace_window invoke failed:', err);
    return false;
  }
}

// One-click installer self-update (cloud mode — see shell_update.rs). Downloads
// + SHA-verifies + launches the latest Windows shell installer natively, since
// cloud mode has no local daemon to do it. Returns { ok } — false when the
// command is unavailable (older shell that predates it, or non-Windows), so the
// caller can fall back to opening the release page for a manual download. On a
// real launch the returned promise resolves after the installer is spawned; the
// installer then prompts to close + relaunch the app.
export interface ShellUpdateNativeResult {
  ok: boolean;
  staged?: string;
  error?: string;
  downgradeRefused?: boolean;
  updateHeld?: boolean;
  installedVersion?: string;
  installerVersion?: string;
}

export async function applyShellUpdateNative(forceDowngrade = false): Promise<ShellUpdateNativeResult> {
  const invoke = tauriInvoke();
  if (!invoke) return { ok: false };
  try {
    const result = await invoke('apply_shell_update', { forceDowngrade });
    // A newer web bundle can run inside an older desktop shell. Older commands
    // resolve to a plain string (or undefined); those remain successful.
    if (result && typeof result === 'object') {
      const outcome = result as {
        launched?: boolean;
        path?: string;
        error?: string | null;
        downgradeRefused?: boolean;
        updateHeld?: boolean;
        installedVersion?: string | null;
        installerVersion?: string | null;
      };
      if (outcome.launched === false) {
        return {
          ok: false,
          staged: outcome.path,
          error: outcome.error ?? undefined,
          downgradeRefused: outcome.downgradeRefused ?? false,
          ...(outcome.updateHeld === true ? { updateHeld: true } : {}),
          installedVersion: outcome.installedVersion ?? undefined,
          installerVersion: outcome.installerVersion ?? undefined,
        };
      }
    }
    return { ok: true };
  } catch (err) {
    // Unknown command (old shell) OR a real failure (download/sha/launch). Both
    // resolve to ok:false so the banner degrades to the manual release-page
    // path; the message is surfaced for the failure case.
    return { ok: false, error: String(err) };
  }
}

// Zero-click desktop update path. This deliberately invokes a NEW command:
// older shells reject it and return ok:false, so they fall back to the attended
// banner without accidentally launching an installer that ignores new args.
export async function applyShellUpdateSilentNative(forceDowngrade = false): Promise<ShellUpdateNativeResult> {
  const invoke = tauriInvoke();
  if (!invoke) return { ok: false };
  try {
    const result = await invoke('apply_shell_update_silent', { forceDowngrade });
    if (result && typeof result === 'object') {
      const outcome = result as {
        launched?: boolean;
        path?: string;
        error?: string | null;
        downgradeRefused?: boolean;
        updateHeld?: boolean;
        installedVersion?: string | null;
        installerVersion?: string | null;
      };
      if (outcome.launched === false) {
        return {
          ok: false,
          staged: outcome.path,
          error: outcome.error ?? undefined,
          downgradeRefused: outcome.downgradeRefused ?? false,
          ...(outcome.updateHeld === true ? { updateHeld: true } : {}),
          installedVersion: outcome.installedVersion ?? undefined,
          installerVersion: outcome.installerVersion ?? undefined,
        };
      }
    }
    return { ok: true };
  } catch (err) {
    return { ok: false, error: String(err) };
  }
}

// The desktop app's own process id (app_process_id, #739), which the page
// passes with the daemon's silent installer update so its courier waits on
// exactly this process. null outside the app and on an app built before the
// command, which then falls back to the courier's name match.
export async function getAppProcessId(): Promise<number | null> {
  const invoke = tauriInvoke();
  if (!invoke) return null;
  try {
    const pid = await invoke('app_process_id');
    return typeof pid === 'number' && Number.isInteger(pid) && pid > 0 && pid <= 2_147_483_647 ? pid : null;
  } catch {
    return null;
  }
}

export async function exitForUpdateNative(): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  try {
    await invoke('exit_for_update');
    return true;
  } catch {
    return false;
  }
}

export async function revealShellInstallerNative(path: string): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  try {
    await invoke('reveal_shell_installer', { path });
    return true;
  } catch {
    // Older shells do not have the reveal command. The persistent UI keeps the
    // selectable path visible so manual navigation is still possible.
    return false;
  }
}

// Whether the "New Window" affordance should be offered. Desktop app only,
// and not in Cloud launch mode (a cloud window needs a per-machine prefix +
// JWT we don't compose for MVP; the Rust command also rejects cloud).
export function multiWindowAvailable(): boolean {
  if (!isDesktopApp()) return false;
  const g = (window as unknown as { __SOLU_BACKEND__?: { launchMode?: string } }).__SOLU_BACKEND__;
  return g?.launchMode !== 'cloud';
}

// Cache the shell version; first call awaits the ping, subsequent
// calls reuse it. Ping is fast (one-off IPC) but the version check and
// the update banner both ask.
let cachedShellVersion: Promise<string | null> | null = null;
export function getShellVersion(): Promise<string | null> {
  if (cachedShellVersion) return cachedShellVersion;
  cachedShellVersion = browserPanePing();
  return cachedShellVersion;
}

// Open a URL in the user's OWN default browser (Chrome/Edge/Safari…), NOT in
// an in-app window. This is the right call for anything the user must SIGN IN
// to — OAuth consent, account authorization — because their real browser
// already holds their logged-in session. It is also where every page the
// preview tile can't show goes (public sites, pages that refuse framing).
//
// Uses the opener plugin (registered in lib.rs; `opener:default` grants
// allow-open-url + allow-default-urls, which scopes http/https). Omitting the
// `with` arg means "default application" = the system browser. Invoked via the
// runtime `__TAURI_INTERNALS__` form (not @tauri-apps/plugin-opener) to avoid a
// hard build dep that would bloat the cloud/PWA bundle — same reasoning as the
// rest of this module.
//
// Do NOT use <a target="_blank"> for this: nothing in the shell intercepts
// _blank navigation from the main window, so anchors silently no-op in the
// desktop app.
//
// Returns true when handed to the system browser; false when it fell back to
// window.open (browser/PWA build, where that IS the user's real browser).
export async function openInSystemBrowser(url: string, triggerEvent?: Event): Promise<boolean> {
  if (isEmbedded()) return openEmbedUrl(url, triggerEvent);
  const invoke = tauriInvoke();
  if (invoke) {
    try {
      await invoke('plugin:opener|open_url', { url });
      return true;
    } catch (err) {
      // Old shell without the plugin, or ACL denied. window.open is unlikely to
      // work in WebView2 either, which is why callers must keep a "Copy link"
      // escape hatch alongside.
      console.warn('plugin:opener|open_url failed, falling back to window.open:', err);
    }
  }
  window.open(url, '_blank', 'noopener,noreferrer');
  return false;
}

// The shell-version sentinel (desktop/src-tauri/src/shell_ping.rs). Named for
// the retired in-pane browser; the command name stays for every shell.
export async function browserPanePing(): Promise<string | null> {
  const invoke = tauriInvoke();
  if (!invoke) return null;
  try {
    const v = await invoke('browser_pane_ping');
    return typeof v === 'string' ? v : String(v);
  } catch (err) {
    console.warn('browser_pane_ping failed:', err);
    return null;
  }
}

// Hotkey-conflict probe (desktop only). True when another app owns
// `combo` (the keybindings.ts serialization, e.g. "alt+z") as an
// EXCLUSIVE system-wide hotkey — NVIDIA and AMD overlays both grab
// Alt+Z this way, and the chord then dies before the webview sees a
// keydown, with nothing visible to blame. Old shells without the
// command and the browser/PWA build report false: no warning, never a
// false alarm.
export async function globalHotkeyOwned(combo: string): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  try {
    return (await invoke('global_hotkey_owned', { combo })) === true;
  } catch {
    return false;
  }
}
