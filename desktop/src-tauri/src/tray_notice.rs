// One-time "still running in the tray" notice.
//
// Closing the main window (X) hides it to the tray instead of quitting. A
// new user who doesn't know that thinks the app quit — then wonders why a
// relaunch is instant, or why Claude alerts still arrive. The first time the
// window hides this way we show one OS notification (the notification
// plugin the shell already registers for web alerts) and drop a marker so it
// never repeats for this install's user profile.

use std::fs::OpenOptions;
use std::path::{Path, PathBuf};

use tauri::{AppHandle, Runtime};

const MARKER: &str = "tray-notice-shown";

/// Same per-user data dir as backend.txt / desktop-mode.txt.
fn marker_path() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        let mut p = PathBuf::from(std::env::var_os("LOCALAPPDATA")?);
        p.push("soluterminal");
        p.push(MARKER);
        Some(p)
    }

    #[cfg(not(windows))]
    {
        let mut p = dirs::config_dir()?;
        p.push("soluterminal");
        p.push(MARKER);
        Some(p)
    }
}

/// Atomically claim the one-time notice: true only for the call that
/// creates the marker. A missing/unwritable data dir returns false — better
/// to skip the hint than to repeat it on every close.
fn claim_once(path: &Path) -> bool {
    if let Some(dir) = path.parent() {
        if std::fs::create_dir_all(dir).is_err() {
            return false;
        }
    }
    OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .is_ok()
}

/// Call after the main window hid to the tray on a close request.
pub fn after_close_to_tray<R: Runtime>(app: &AppHandle<R>) {
    use tauri_plugin_notification::NotificationExt;

    let Some(path) = marker_path() else {
        return;
    };
    if !claim_once(&path) {
        return;
    }
    let body = if cfg!(target_os = "macos") {
        "Closing the window keeps SoluTerminal running in the menu bar. \
         Click the SoluTerminal icon to reopen it, or choose Quit there to exit."
    } else {
        "Closing the window keeps SoluTerminal running in the tray. \
         Click the SoluTerminal tray icon to reopen it, or right-click it and \
         choose Quit to exit."
    };
    if let Err(e) = app
        .notification()
        .builder()
        .title("SoluTerminal is still running")
        .body(body)
        .show()
    {
        eprintln!("desktop: tray notice failed: {e}");
    }
}

#[cfg(test)]
mod tests {
    use super::claim_once;
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn notice_is_claimed_exactly_once() {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let dir = std::env::temp_dir().join(format!("solu-tray-notice-{nonce}"));
        let marker = dir.join("nested").join("tray-notice-shown");

        assert!(claim_once(&marker), "first close shows the notice");
        assert!(marker.is_file(), "marker persists the flag");
        assert!(!claim_once(&marker), "later closes stay quiet");

        let _ = std::fs::remove_dir_all(&dir);
    }
}
