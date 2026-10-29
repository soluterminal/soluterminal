// One desktop shell per user session.
//
// Relaunching the exe (Start menu, desktop shortcut, taskbar pin) while the
// app is already running — often hidden in the tray after the user closed
// the window — used to start a second shell that raced the first for the
// daemon port and tray. tauri-plugin-single-instance holds a per-identifier
// named mutex (Windows) / D-Bus name (Linux) / lock socket (macOS); a second
// launch forwards its argv to the running shell and exits before .setup()
// runs, so it never spawns a daemon or shows first-run dialogs.

use std::sync::atomic::AtomicBool;
use std::sync::Arc;

use tauri::{AppHandle, Manager, Runtime};

use crate::autostart::AUTOSTART_ARG;

/// Shared "maximize on the first reveal" flag (autostart restored a
/// maximized window hidden). Managed so the relaunch callback can reveal the
/// window exactly like the tray does.
pub struct MainWindowReveal(pub Arc<AtomicBool>);

/// The plugin. Must be the FIRST plugin registered so a second instance
/// exits before any other plugin initializes.
pub fn plugin<R: Runtime>() -> tauri::plugin::TauriPlugin<R> {
    tauri_plugin_single_instance::init(|app, argv, _cwd| {
        if !should_reveal(&argv) {
            return;
        }
        reveal_main_window(app);
    })
}

/// A second launch that came from the login Run key (`--autostart`) must
/// not pop the window open: the user didn't ask to see it.
fn should_reveal<S: AsRef<str>>(argv: &[S]) -> bool {
    !argv.iter().any(|a| a.as_ref() == AUTOSTART_ARG)
}

fn reveal_main_window<R: Runtime>(app: &AppHandle<R>) {
    use std::sync::atomic::Ordering;

    // The first instance may still be starting its backends (or in the
    // first-run wizard) — no main window yet. Bring the startup window forward
    // if it is showing; the main window shows itself when startup finishes.
    let Some(window) = app.get_webview_window("main") else {
        if let Some(startup) = app.get_webview_window(crate::startup_screen::LABEL) {
            let _ = startup.unminimize();
            let _ = startup.set_focus();
        }
        return;
    };
    let maximize = app
        .try_state::<MainWindowReveal>()
        .map(|r| r.0.swap(false, Ordering::AcqRel))
        .unwrap_or(false);
    let _ = window.show();
    let _ = window.unminimize();
    if maximize {
        let _ = window.maximize();
    }
    let _ = window.set_focus();
}

/// Release the single-instance lock before a main-thread `app.restart()`.
///
/// Tauri's main-thread restart path spawns the new process and calls
/// `exit(0)` WITHOUT emitting `RunEvent::Exit`, so the plugin never frees
/// its mutex: the relaunched shell would see a "running" instance, hand
/// its argv to the dying one and exit — the app would just vanish. Only
/// call this from main-thread handlers (tray menu); async commands restart
/// through `request_exit`, which emits `RunEvent::Exit` and frees the lock
/// itself (freeing it twice would close a stale handle).
pub fn release_before_restart<R: Runtime>(app: &AppHandle<R>) {
    tauri_plugin_single_instance::destroy(app);
}

#[cfg(test)]
mod tests {
    use super::should_reveal;

    #[test]
    fn plain_relaunch_reveals_the_window() {
        assert!(should_reveal(&[
            r"C:\Program Files\soluterminal\soluterminal.exe"
        ]));
        assert!(should_reveal::<&str>(&[]));
    }

    #[test]
    fn login_autostart_relaunch_stays_in_the_tray() {
        assert!(!should_reveal(&[
            r"C:\Program Files\soluterminal\soluterminal.exe",
            "--autostart",
        ]));
    }
}
