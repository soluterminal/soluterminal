// Tauri pop-out browser helper. In the desktop app, spawns a real
// WebviewWindow (full Chromium/WebView2) so the user can browse URLs
// blocked by X-Frame-Options / frame-ancestors inside an iframe
// (Google, GitHub, banks, Slack, etc). In the browser/PWA build this
// falls back to `window.open`, which either hits a popup blocker or
// opens the user's default browser — either way, outside the app.
//
// Feature-detected via try/catch on the invoke call rather than a
// version check, so an older Tauri shell (MSI built before this
// command landed) silently falls back to window.open instead of
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

// Minimum shell version where the popout window hosts its own child
// webview AND injects window.__SOLU_POPOUT__ via init_script. 0.0.277
// shipped the parent_label arg but relied on URL query params for the
// React side to detect popout mode — those got eaten by the daemon's
// auth redirect on fresh cookie jars, so the popout rendered the main
// App instead of PopoutBrowser. 0.0.284 moved detection to the
// init-script global and is the first version where popouts are
// actually usable end to end. Shells below this fall back to a
// plain "open the target URL directly" popout (no address bar, but
// everything else works and the user can close it normally).
const POPOUT_SHELL_MIN_VERSION = '0.0.284';

// Cache the shell version; first call awaits the ping, subsequent
// calls are synchronous. Ping is fast (one-off IPC) but we still
// don't want to re-fetch on every popout click.
let cachedShellVersion: Promise<string | null> | null = null;
export function getShellVersion(): Promise<string | null> {
  if (cachedShellVersion) return cachedShellVersion;
  cachedShellVersion = browserPanePing();
  return cachedShellVersion;
}

function versionGte(a: string, b: string): boolean {
  const pa = a.split('.').map((n) => parseInt(n, 10) || 0);
  const pb = b.split('.').map((n) => parseInt(n, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const ai = pa[i] ?? 0;
    const bi = pb[i] ?? 0;
    if (ai > bi) return true;
    if (ai < bi) return false;
  }
  return true;
}

export async function openExternalWindow(url: string, title?: string): Promise<boolean> {
  if (isEmbedded()) return openEmbedUrl(url);
  const invoke = tauriInvoke();
  if (invoke) {
    try {
      // Version-gate the popout-shell flow. On shells older than
      // POPOUT_SHELL_MIN_VERSION the `parent_label` arg is ignored, so
      // the child webview would end up attached to the main window
      // (empty popout, content obscuring the main UI). Fall back to
      // opening the target URL directly — same as the pre-address-bar
      // behaviour — so auto-update daemons on old shells still get a
      // working (if chrome-less) popout.
      const shellVersion = await getShellVersion();
      const canHostPopoutShell =
        shellVersion !== null && versionGte(shellVersion, POPOUT_SHELL_MIN_VERSION);
      if (!canHostPopoutShell) {
        await invoke('open_external_window', { url, title: title ?? null });
        return true;
      }

      // Load the popout window at the same daemon origin as the main
      // window — the React bundle served from there reads
      // `window.__SOLU_POPOUT__` (injected by Rust via init_script) and
      // renders a fullscreen PopoutBrowser component, which spawns a
      // native child webview for the target URL inside the popout
      // window itself. Reusing the bundle gives us address bar +
      // reload + close buttons for free, and keeps cross-origin
      // support via the native child webview (same trick as in-pane
      // browsers).
      //
      // Label is generated here so the popout's React code can pass
      // it to browser_pane_create as parent_label — matches the Tauri
      // window label created in Rust.
      const label = `browser_${Date.now().toString(36)}_${Math.floor(Math.random() * 1e6).toString(36)}`;
      const navigateUrl = new URL(window.location.href);
      navigateUrl.searchParams.delete('popout');
      navigateUrl.searchParams.delete('popout_url');
      navigateUrl.searchParams.delete('popout_label');
      navigateUrl.searchParams.delete('popout_title');
      // Encode popout config in the URL hash (fragment). Three reasons:
      //   - fragments survive every server-side redirect (daemon auth
      //     gate, router mode); the browser re-attaches them to the
      //     Location target, so the config is guaranteed to reach React
      //     even if the popout hits /login first.
      //   - fragments are never sent to the server, so no risk of
      //     leaking the target URL into daemon access logs.
      //   - the previous init-script approach (window.__SOLU_POPOUT__)
      //     was silently failing in some WebView2 configurations, with
      //     PopoutBrowser never mounting and the user seeing the raw
      //     target URL full-screen. Hash handshake is observable from
      //     JS and can't be dropped.
      const popoutParams = new URLSearchParams();
      popoutParams.set('popout', '1');
      popoutParams.set('popout_url', url);
      popoutParams.set('popout_label', label);
      if (title) popoutParams.set('popout_title', title);
      navigateUrl.hash = popoutParams.toString();
      await invoke('open_external_window', {
        url,                              // target URL (address bar + init_script fallback)
        title: title ?? null,
        label,
        // Tauri 2 commands auto-rename camelCase (JS) ↔ snake_case
        // (Rust), so this field MUST be camelCase to reach the Rust
        // `navigate_url: Option<String>` parameter. Sending snake_case
        // silently falls through to None and Rust opens the popout at
        // the target URL directly, bypassing our React bundle — which
        // is how the "no address bar" bug survived all earlier fixes.
        navigateUrl: navigateUrl.toString(),
      });
      return true;
    } catch (err) {
      // Old shell without the command, or ACL denied. Fall through to
      // window.open so the user still gets SOME pop-out behavior.
      console.warn('open_external_window invoke failed, falling back to window.open:', err);
    }
  }
  window.open(url, '_blank', 'noopener,noreferrer');
  return false;
}

// Open a URL in the user's OWN default browser (Chrome/Edge/Safari…), NOT in
// an in-app window. This is the right call for anything the user must SIGN IN
// to — OAuth consent, account authorization — because their real browser
// already holds their logged-in session, whereas openExternalWindow spawns a
// WebView2 with a FRESH profile (no cookies) and would make them re-enter
// credentials in a window that doesn't look like their browser.
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
export async function openInSystemBrowser(url: string): Promise<boolean> {
  if (isEmbedded()) return openEmbedUrl(url);
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

// In-pane native browser (child webview overlaid on a React pane).
// All five commands are no-ops in non-Tauri mode — callers use this
// behind `isDesktopApp()` and fall back to `<iframe>` rendering.

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

export async function browserPaneCreate(
  id: string,
  url: string,
  rect: { x: number; y: number; w: number; h: number },
  opts: { parentLabel?: string; timeoutMs?: number } = {},
): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  const timeoutMs = opts.timeoutMs ?? 4000;
  try {
    // parentLabel lets the popout window host its own child webview
    // (omit or leave undefined for the default "main" window). Rust
    // defaults to "main" when parent_label is None.
    //
    // Tauri 2 auto-renames camelCase (JS) ↔ snake_case (Rust), so this
    // MUST be sent as parentLabel to reach Rust's `parent_label: Option<String>`.
    // Sending snake_case silently drops it to None and the child webview
    // attaches to the main window instead of the popout.
    const args: Record<string, unknown> = { id, url, ...rect };
    if (opts.parentLabel) args.parentLabel = opts.parentLabel;
    await Promise.race([
      invoke('browser_pane_create', args),
      new Promise((_, reject) =>
        setTimeout(() => reject(new Error(`browser_pane_create timed out after ${timeoutMs}ms`)), timeoutMs),
      ),
    ]);
    return true;
  } catch (err) {
    console.warn('browser_pane_create failed:', err);
    return false;
  }
}

export async function browserPaneSetBounds(
  id: string,
  rect: { x: number; y: number; w: number; h: number },
): Promise<void> {
  const invoke = tauriInvoke();
  if (!invoke) return;
  try {
    await invoke('browser_pane_set_bounds', { id, ...rect });
  } catch (err) {
    // Bounds updates can race with close; don't log those.
    if (String(err).indexOf('not found') < 0) {
      console.warn('browser_pane_set_bounds failed:', err);
    }
  }
}

export async function browserPaneNavigate(id: string, url: string): Promise<void> {
  const invoke = tauriInvoke();
  if (!invoke) return;
  try {
    await invoke('browser_pane_navigate', { id, url });
  } catch (err) {
    // Expected on initial mount: the navigate effect fires before
    // browser_pane_create finishes, so get_webview returns None and
    // Rust returns "webview not found". The initial URL was already
    // passed to WebviewBuilder in create(), so navigation is correct
    // without this redundant call — just swallow the race.
    if (String(err).indexOf('not found') < 0) {
      console.warn('browser_pane_navigate failed:', err);
    }
  }
}

export async function browserPaneClose(id: string): Promise<void> {
  const invoke = tauriInvoke();
  if (!invoke) return;
  try {
    await invoke('browser_pane_close', { id });
  } catch {
    // already closed — ignore
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

// Toggle native child webview visibility. We hide all child webviews
// while a pane drag is in progress so the user can see the drop-zone
// overlay underneath (the native webview is an OS-level window painted
// on top of the React DOM and would otherwise obscure the overlay).
export async function browserPaneSetVisible(id: string, visible: boolean): Promise<void> {
  const invoke = tauriInvoke();
  if (!invoke) return;
  try {
    await invoke('browser_pane_set_visible', { id, visible });
  } catch (err) {
    if (String(err).indexOf('not found') < 0) {
      console.warn('browser_pane_set_visible failed:', err);
    }
  }
}
