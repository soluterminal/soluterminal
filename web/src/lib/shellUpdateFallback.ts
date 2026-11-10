import { openInSystemBrowser } from '../tauri-browser';

// The public download page. Users are never sent to the release repository:
// its URL carries a personal account name, and its asset list is confusing.
export const DESKTOP_DOWNLOAD_URL = 'https://soluterminal.com/download/';

type Toast = (
  message: string,
  opts?: { kind?: 'info' | 'success' | 'warn' | 'error'; icon?: string; ttlMs?: number },
) => void;

// One plain line from a native updater error such as
// "installer update failed (exit 10): could not read the downloaded installer: …".
export function describeShellUpdateFailure(error?: string): string {
  if (!error) return '';
  const first = error.split(/\r?\n/)[0]?.trim() ?? '';
  const reason = first
    .replace(/^Error:\s*/i, '')
    .replace(/^installer update failed \(exit -?\d+\):\s*/i, '')
    .trim();
  return reason.length > 160 ? `${reason.slice(0, 157)}…` : reason;
}

export async function openDesktopDownloadPage(): Promise<void> {
  await openInSystemBrowser(DESKTOP_DOWNLOAD_URL);
}

// Last resort after every automatic path failed: say why, then open the
// download page so the user can run the installer themselves.
export async function fallBackToDownloadPage(toast: Toast, error?: string): Promise<void> {
  const reason = describeShellUpdateFailure(error);
  toast(
    reason
      ? `Couldn't start the installer (${reason}). Opening the download page: run the installer from there.`
      : "Couldn't start the installer. Opening the download page: run the installer from there.",
    { kind: 'warn', icon: '⬆', ttlMs: 12000 },
  );
  await openDesktopDownloadPage();
}
