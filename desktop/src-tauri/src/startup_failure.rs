//! What the app shows when its background service can't start.
//!
//! Startup used to carry on regardless: the main window was built anyway and
//! pointed at an address nothing served. That window has no title bar of its
//! own (the page draws it), so the person was left with the browser's "can't
//! reach this page" in a frame they could not move or close, and no reason.
//! Startup now stops instead and says what went wrong in a native dialog, with
//! Retry, Open logs and Quit. Nothing here needs a daemon or a web page.

use std::net::TcpStream;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use tauri::{AppHandle, Manager};
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind, MessageDialogResult};
use tauri_plugin_opener::OpenerExt;

pub const TITLE: &str = "SoluTerminal couldn't start";
const RETRY: &str = "Retry";
const OPEN_LOGS: &str = "Open logs";
const QUIT: &str = "Quit";

/// How long startup waits, after the service was started, for something to
/// accept connections where the main window is about to load.
pub const FIRST_ANSWER_PATIENCE: Duration = Duration::from_secs(25);

/// What the person chose in the dialog.
pub enum Choice {
    Retry,
    Quit,
}

#[derive(Debug, PartialEq, Eq)]
enum Button {
    Retry,
    OpenLogs,
    Quit,
}

fn button(result: &MessageDialogResult) -> Button {
    match result {
        MessageDialogResult::Custom(label) if label == RETRY => Button::Retry,
        MessageDialogResult::Custom(label) if label == OPEN_LOGS => Button::OpenLogs,
        // A platform that reports the button's position instead of its label.
        MessageDialogResult::Yes => Button::Retry,
        MessageDialogResult::No => Button::OpenLogs,
        // Quit, Esc, or the dialog's own close button. Nothing else of the
        // app is on screen, so closing the dialog must not leave it running.
        _ => Button::Quit,
    }
}

/// Tell the person startup failed and wait for their choice. "Open logs"
/// opens the folder and asks again. Blocks, so call it off the main thread.
pub fn ask(app: &AppHandle, err: &str) -> Choice {
    let message = message(err);
    loop {
        let dialog = app
            .dialog()
            .message(message.clone())
            .title(TITLE)
            .kind(MessageDialogKind::Error)
            .buttons(MessageDialogButtons::YesNoCancelCustom(
                RETRY.into(),
                OPEN_LOGS.into(),
                QUIT.into(),
            ));
        // The startup window, when it is up, owns the dialog so the dialog
        // opens in front of it. A launch at login has no startup window.
        let dialog = match app.get_webview_window(crate::startup_screen::LABEL) {
            Some(startup) => dialog.parent(&startup),
            None => dialog,
        };
        match button(&dialog.blocking_show_with_result()) {
            Button::Retry => return Choice::Retry,
            Button::OpenLogs => open_logs(app),
            Button::Quit => return Choice::Quit,
        }
    }
}

/// The backends are up but the main window could not be built. There is
/// nothing to retry: say so, then exit. Never blocks; safe on the main thread.
/// The caller leaves the startup window open until then: it owns the dialog,
/// and closing the app's last window would end the app before anyone read it.
pub fn window_failed(app: &AppHandle, err: &str) {
    let exiting = app.clone();
    let dialog = app
        .dialog()
        .message(format!(
            "SoluTerminal's background service started, but its window could not open.\n\n\
             Restart SoluTerminal. If this keeps happening, restart this computer.\n\n\
             Details: {}",
            one_line(err)
        ))
        .title(TITLE)
        .kind(MessageDialogKind::Error)
        .buttons(MessageDialogButtons::OkCustom(QUIT.into()));
    let dialog = match app.get_webview_window(crate::startup_screen::LABEL) {
        Some(startup) => dialog.parent(&startup),
        None => dialog,
    };
    dialog.show(move |_| exiting.exit(1));
}

/// A startup error in plain words: what happened, and what to do about it.
#[derive(Debug, PartialEq, Eq)]
struct Explanation {
    summary: String,
    advice: &'static str,
}

const ADVICE_RETRY: &str =
    "Choose Retry. If this keeps happening, choose Open logs and include those files when you report the problem.";

/// Substrings match the error sites in daemon.rs and lib.rs.
fn explain(err: &str) -> Explanation {
    // A WSL or Dual start that failed on both sides. The Windows half is the
    // one that had to work, so explain that.
    if let Some((_, windows)) = err.split_once("; Windows=") {
        let windows = explain(windows);
        return Explanation {
            summary: format!(
                "Linux (WSL) didn't start, and the Windows background service couldn't start in its place. {}",
                windows.summary
            ),
            advice: windows.advice,
        };
    }
    if err.contains("solud binary not found") || err.contains("bundled Linux solud") {
        return Explanation {
            summary: "SoluTerminal is missing its background service file.".into(),
            advice: "Download the latest installer and run it again to repair SoluTerminal.",
        };
    }
    if err.contains("is backend \"wsl\"") {
        return Explanation {
            summary: "SoluTerminal's Linux (WSL) background service is already using the port the Windows one needs."
                .into(),
            advice: "To free the port, save your Linux work and run `wsl --shutdown` in PowerShell (this closes everything running in WSL). Then choose Retry.",
        };
    }
    if err.contains(" is backend ") {
        return Explanation {
            summary: "Another SoluTerminal background service is already using the port this one needs.".into(),
            advice: "Quit the other copy of SoluTerminal (look in the notification area, and in other user accounts on this computer). Then choose Retry.",
        };
    }
    if err.contains("update-only daemon") {
        return Explanation {
            summary: "SoluTerminal is still finishing an update.".into(),
            advice: "Wait a minute, then choose Retry.",
        };
    }
    if err.contains("daemon health") || err.contains("health returned HTTP") || err.contains("health body") {
        return Explanation {
            summary: "Something is already using the port SoluTerminal needs, and it isn't SoluTerminal.".into(),
            advice: "Close the program using that port, or restart this computer. Then choose Retry.",
        };
    }
    if err.starts_with("spawn ") {
        return Explanation {
            summary: "This computer wouldn't run SoluTerminal's background service.".into(),
            advice: "Security software sometimes blocks it. Allow SoluTerminal there, or run the installer again. Then choose Retry.",
        };
    }
    if err.contains("did not answer") {
        return Explanation {
            summary: "SoluTerminal's background service started but did not answer.".into(),
            advice: ADVICE_RETRY,
        };
    }
    Explanation {
        summary: "SoluTerminal's background service did not start.".into(),
        advice: ADVICE_RETRY,
    }
}

/// The dialog's text: the explanation, then the error itself for whoever
/// reads a screenshot of it.
fn message(err: &str) -> String {
    let Explanation { summary, advice } = explain(err);
    format!("{summary}\n\n{advice}\n\nDetails: {}", one_line(err))
}

/// One bounded line, so a long error can't push the buttons off the screen.
fn one_line(err: &str) -> String {
    const MAX_CHARS: usize = 400;
    let err = err.split_whitespace().collect::<Vec<_>>().join(" ");
    if err.is_empty() {
        return "no error message".to_string();
    }
    let mut line: String = err.chars().take(MAX_CHARS).collect();
    if err.chars().count() > MAX_CHARS {
        line.push('…');
    }
    line
}

/// The file to point at in the logs folder. On Windows launch.log is the
/// app's own account of this start and sits beside the service's daemon.log;
/// elsewhere there is only daemon.log. With neither, the folder itself.
fn log_to_show(dir: &Path) -> PathBuf {
    ["launch.log", "daemon.log"]
        .iter()
        .map(|name| dir.join(name))
        .find(|path| path.is_file())
        .unwrap_or_else(|| dir.to_path_buf())
}

fn open_logs(app: &AppHandle) {
    let Some(dir) = crate::daemon::daemon_log_path().and_then(|log| log.parent().map(Path::to_path_buf))
    else {
        eprintln!("desktop: no logs folder on this computer");
        return;
    };
    let target = log_to_show(&dir);
    let opened = if target.is_file() {
        app.opener().reveal_item_in_dir(&target)
    } else {
        app.opener()
            .open_path(target.to_string_lossy().into_owned(), None::<&str>)
    };
    if let Err(e) = opened {
        eprintln!("desktop: could not open {}: {e}", target.display());
        #[cfg(windows)]
        crate::daemon::launch_log(&format!("could not open {}: {e}", target.display()));
    }
}

/// Wait until something accepts connections where `url` points, the address
/// the main window loads next. A service that was started but never listens
/// (it crashed at once, or the machine is very slow) would otherwise leave
/// that window on a dead page. Only a connection is checked: who answers was
/// already settled when the backend was started.
pub fn wait_until_listening(url: &str, patience: Duration) -> Result<(), String> {
    let addrs = url
        .parse::<tauri::Url>()
        .ok()
        .and_then(|url| url.socket_addrs(|| None).ok())
        .unwrap_or_default();
    if addrs.is_empty() {
        // Not an address this check understands; let the window try it.
        return Ok(());
    }
    let deadline = Instant::now() + patience;
    loop {
        if addrs
            .iter()
            .any(|addr| TcpStream::connect_timeout(addr, Duration::from_millis(250)).is_ok())
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "the background service did not answer at {} within {} seconds",
                addrs[0],
                patience.as_secs()
            ));
        }
        std::thread::sleep(Duration::from_millis(200));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn each_button_is_read_by_label_or_by_position() {
        assert_eq!(button(&MessageDialogResult::Custom(RETRY.into())), Button::Retry);
        assert_eq!(button(&MessageDialogResult::Custom(OPEN_LOGS.into())), Button::OpenLogs);
        assert_eq!(button(&MessageDialogResult::Custom(QUIT.into())), Button::Quit);
        assert_eq!(button(&MessageDialogResult::Yes), Button::Retry);
        assert_eq!(button(&MessageDialogResult::No), Button::OpenLogs);
        // Esc or the close button must never leave a windowless app running.
        assert_eq!(button(&MessageDialogResult::Cancel), Button::Quit);
        assert_eq!(button(&MessageDialogResult::Ok), Button::Quit);
    }

    #[test]
    fn a_missing_service_file_points_at_the_installer() {
        let found = explain("solud binary not found. Looked: next to the Tauri exe");
        assert_eq!(found.summary, "SoluTerminal is missing its background service file.");
        assert!(found.advice.contains("installer"));
    }

    #[test]
    fn a_wsl_service_on_the_windows_port_says_how_to_free_it() {
        let found = explain(
            "daemon at 127.0.0.1:8787 is backend \"wsl\", expected \"host\" (pid=412, version=0.0.1242)",
        );
        assert!(found.summary.contains("Linux (WSL) background service is already using the port"));
        assert!(found.advice.contains("wsl --shutdown"));
        assert!(found.advice.contains("save your Linux work"));
    }

    #[test]
    fn another_service_or_another_program_on_the_port_are_told_apart() {
        let service =
            explain("daemon at 127.0.0.1:8787 is backend \"win\", expected \"host\" (pid=9, version=0.0.1242)");
        assert!(service.summary.starts_with("Another SoluTerminal background service"));
        for err in [
            "request daemon health 127.0.0.1:8787 failed or timed out",
            "daemon 127.0.0.1:8787 health returned HTTP 404",
        ] {
            let program = explain(err);
            assert!(program.summary.contains("isn't SoluTerminal"), "{err}: {}", program.summary);
        }
    }

    #[test]
    fn a_blocked_or_silent_service_and_an_unknown_error_keep_retry_useful() {
        assert!(explain("spawn C:\\Program Files\\SoluTerminal\\solud.exe: Access is denied. (os error 5)")
            .advice
            .contains("Security software"));
        let silent = explain("the background service did not answer at 127.0.0.1:8787 within 25 seconds");
        assert!(silent.summary.contains("started but did not answer"));
        assert_eq!(silent.advice, ADVICE_RETRY);
        let unknown = explain("something nobody planned for");
        assert_eq!(unknown.summary, "SoluTerminal's background service did not start.");
        assert_eq!(unknown.advice, ADVICE_RETRY);
    }

    #[test]
    fn a_failed_fallback_explains_the_windows_side() {
        let found = explain(
            "both backends failed: WSL=WSL daemon was not reachable within 120s via []: x; \
             Windows=solud binary not found. Looked: next to the Tauri exe",
        );
        assert!(found.summary.starts_with("Linux (WSL) didn't start, and the Windows background service"));
        assert!(found.summary.ends_with("SoluTerminal is missing its background service file."));
        assert!(found.advice.contains("installer"));
    }

    #[test]
    fn the_message_ends_with_the_error_on_one_bounded_line() {
        let text = message("first line\r\n   second line");
        assert!(text.ends_with("Details: first line second line"), "{text}");
        assert!(text.contains("\n\n"));
        assert_eq!(one_line(" \n "), "no error message");
        let long = one_line(&"x".repeat(1000));
        assert_eq!(long.chars().count(), 401);
        assert!(long.ends_with('…'));
    }

    #[test]
    fn open_logs_points_at_the_launch_log_then_the_daemon_log_then_the_folder() {
        let dir = std::env::temp_dir().join(format!("solu-startup-failure-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        assert_eq!(log_to_show(&dir), dir);
        std::fs::write(dir.join("daemon.log"), "x").unwrap();
        assert_eq!(log_to_show(&dir), dir.join("daemon.log"));
        std::fs::write(dir.join("launch.log"), "x").unwrap();
        assert_eq!(log_to_show(&dir), dir.join("launch.log"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn startup_waits_for_a_listener_and_gives_up_with_the_address() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let url = format!("http://127.0.0.1:{port}/?token=abc");
        assert_eq!(wait_until_listening(&url, Duration::ZERO), Ok(()));
        drop(listener);
        let err = wait_until_listening(&url, Duration::ZERO).unwrap_err();
        assert_eq!(
            err,
            format!("the background service did not answer at 127.0.0.1:{port} within 0 seconds")
        );
        assert_eq!(explain(&err).summary, "SoluTerminal's background service started but did not answer.");
        // An address this check can't resolve is left to the window.
        assert_eq!(wait_until_listening("not a url", Duration::ZERO), Ok(()));
    }
}
