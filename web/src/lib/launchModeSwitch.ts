import { confirmDialog } from './dialog';
import { tauriInvoke } from '../tauri-browser';

type Toast = (
  message: string,
  opts?: { kind?: 'info' | 'success' | 'warn' | 'error'; icon?: string; ttlMs?: number },
) => void;

export type LaunchModeTarget = 'local' | 'cloud';

// The desktop app's Local/Cloud hosting switch: the same restart-level action
// as the tray "Switch to …" item (desktop_set_launch_mode). The hosting row in
// the environment controls and the installer prompts in cloud launch mode
// (VersionPopover, ShellUpdateBanner) all go through here, so they confirm
// and fail the same way.
//
// Resolves true once the shell accepted the switch (it restarts the app),
// false when the user cancelled, there is no desktop shell, or the shell
// refused (each refusal is toasted). onConfirmed runs after the user agreed
// and before the shell is asked, for a pending indicator.
export async function switchDesktopLaunchMode(
  target: LaunchModeTarget,
  toast: Toast,
  onConfirmed?: () => void,
): Promise<boolean> {
  const invoke = tauriInvoke();
  if (!invoke) return false;
  const ok = await confirmDialog({
    title: target === 'cloud' ? 'Switch to Cloud' : 'Switch to Local',
    message: target === 'cloud'
      ? 'Reconnect through your soluterminal cloud account?\n\nThe app restarts and shows your cloud machines (sign-in required).'
      : 'Connect directly to the daemon on this computer?\n\nThe app restarts.',
    confirmLabel: 'Restart',
  });
  if (!ok) return false;
  onConfirmed?.();
  try {
    await invoke('desktop_set_launch_mode', { mode: target });
    return true;
  } catch (err) {
    // The invoke can fail for two reasons: a genuinely old shell without
    // the command, or a shell whose capability allowlist predates the
    // ACL grant (installers up to at least 0.0.888 shipped the command
    // but denied it). Either way the tray menu performs the same switch
    // and an app update fixes it for good — say that, and keep the raw
    // error in the console for support.
    console.warn('desktop_set_launch_mode invoke failed:', err);
    // A shell built without a hosted cloud refuses Cloud and says why.
    if (/no hosted cloud/i.test(String(err))) {
      toast('This build has no hosted cloud configured, so it only runs on this computer.', { kind: 'info', ttlMs: 9000 });
      return false;
    }
    toast(
      "This version of the desktop app can't switch hosting in-app. Right-click the soluterminal tray icon and pick 'Switch to Cloud account / Local daemon' — or update the desktop app to enable this button.",
      { kind: 'error', icon: '!', ttlMs: 9000 },
    );
    return false;
  }
}
