//! Settings uses a signed web action. Windows WSL tray recovery has a separate
//! native confirmation and ownership proof, with no webview invoke surface.
use std::time::Duration;
use tauri::menu::{Menu, MenuItem};
#[cfg(any(windows, test))]
use tauri::Manager;

#[cfg(any(windows, test))]
pub struct RestartMenuItem(pub MenuItem<tauri::Wry>);

fn capability_url(window_url: &str) -> Option<(String, Option<String>)> {
    let parsed = tauri::Url::parse(window_url).ok()?;
    if parsed.scheme() != "http" || !matches!(parsed.host_str(), Some("127.0.0.1" | "localhost")) {
        return None;
    }
    let port = parsed.port()?;
    let token = parsed
        .query_pairs()
        .find(|(k, _)| k == "token")
        .map(|(_, v)| v.into_owned());
    let backend = if port == 28787 { "/b/wsl" } else { "" };
    Some((
        format!("http://127.0.0.1:{port}{backend}/api/service/restart"),
        token,
    ))
}

pub fn discover(app: &tauri::AppHandle, menu: &Menu<tauri::Wry>, window_url: &str) {
    let Some((url, token)) = capability_url(window_url) else {
        return;
    };
    let app = app.clone();
    let menu = menu.clone();
    std::thread::spawn(move || {
        let Ok(client) = reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(20))
            .build()
        else {
            return;
        };
        let mut request = client.get(url);
        if let Some(token) = token {
            request = request.bearer_auth(token);
        }
        let supported = request
            .send()
            .ok()
            .filter(|r| r.status().is_success())
            .and_then(|r| r.text().ok())
            .and_then(|s| serde_json::from_str::<serde_json::Value>(&s).ok())
            .and_then(|s| s.get("supported").and_then(|v| v.as_bool()))
            == Some(true);
        if !supported {
            return;
        }
        let main = app.clone();
        let _ = app.run_on_main_thread(move || {
            if let Ok(item) = MenuItem::with_id(
                &main,
                "restart_background_service",
                "Restart background service",
                true,
                None::<&str>,
            ) {
                let _ = menu.insert(&item, 2);
            }
        });
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn discovery_is_local_and_dual_always_targets_wsl() {
        assert_eq!(
            capability_url("http://127.0.0.1:28787/?solu_router=1")
                .unwrap()
                .0,
            "http://127.0.0.1:28787/b/wsl/api/service/restart"
        );
        assert_eq!(
            capability_url("http://127.0.0.1:18787/?token=abc%2Bd")
                .unwrap()
                .1
                .as_deref(),
            Some("abc+d")
        );
        assert!(capability_url("https://example.com/").is_none());
        assert!(capability_url("http://192.168.1.5:8787/").is_none());
    }
}

// Deliberately NOT a Tauri command: only the native tray callback can enter
// this recovery path. A page cannot restart the service when it lacks proof.
#[cfg(any(windows, test))]
pub fn native_restart(app: &tauri::AppHandle, window_url: &str) {
    use std::sync::atomic::{AtomicBool, Ordering};
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
    static PENDING: AtomicBool = AtomicBool::new(false);
    if PENDING.swap(true, Ordering::SeqCst) {
        return;
    }
    let app = app.clone();
    let url = window_url.to_owned();
    std::thread::spawn(move || {
        native_log("native tray background service restart requested");
        let result = native_attempt(&app, &url);
        native_log(if result.is_ok() {
            "native tray background service restart completed or cancelled"
        } else {
            "native tray background service restart refused or unconfirmed"
        });
        if let Some(tray) = app.tray_by_id("main") {
            let _ = tray.set_tooltip(Some("SoluTerminal"));
        }
        native_progress(&app, false);
        PENDING.store(false, Ordering::SeqCst);
        match result {
            Ok(Some(message)) | Err(message) => {
                app.dialog()
                    .message(message)
                    .title("Background service")
                    .buttons(MessageDialogButtons::Ok)
                    .blocking_show();
            }
            Ok(None) => {}
        }
    });
}

#[cfg(any(windows, test))]
fn native_attempt(app: &tauri::AppHandle, window_url: &str) -> Result<Option<String>, String> {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
    const SCRIPT: &str = include_str!("../../../daemon/background_service_wsl.sh");
    let inspect = run_wsl_restart(
        &[
            "-e",
            "/usr/bin/env",
            "-i",
            "PATH=/usr/bin:/bin",
            "LC_ALL=C",
            "/bin/bash",
            "--noprofile",
            "--norc",
            "-p",
            "-s",
            "--",
            "inspect",
        ],
        Some(SCRIPT.as_bytes()),
        Duration::from_secs(15),
    )?;
    if !inspect.status.success() {
        return Err("Cannot prove ownership of the WSL daemon and its existing supervisor. Nothing was stopped. For manual recovery, save other WSL work, then as a last resort run wsl --shutdown in Windows and reopen SoluTerminal (stops everything in WSL).".into());
    }
    let proof = String::from_utf8_lossy(&inspect.stdout);
    let parts: Vec<&str> = proof.split_whitespace().collect();
    if parts.len() != 2
        || parts[0].parse::<u32>().ok().filter(|p| *p > 1).is_none()
        || parts[1].len() != 64
        || !parts[1].bytes().all(|c| c.is_ascii_hexdigit())
    {
        return Err("Invalid WSL ownership proof. Nothing was stopped.".into());
    }
    let old_pid: u64 = parts[0].parse().map_err(|_| "Invalid PID")?;
    let (cap_url, token) = capability_url(window_url).ok_or("No local daemon route")?;
    let base = cap_url
        .strip_suffix("/api/service/restart")
        .ok_or("Invalid local route")?;
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(20))
        .build()
        .map_err(|e| e.to_string())?;
    let get_json = |path: &str| -> Result<serde_json::Value, String> {
        let mut req = client.get(format!("{base}{path}"));
        if let Some(t) = &token {
            req = req.bearer_auth(t);
        }
        let text = req
            .send()
            .and_then(|r| r.error_for_status())
            .and_then(|r| r.text())
            .map_err(|e| e.to_string())?;
        serde_json::from_str(&text).map_err(|e| e.to_string())
    };
    let expected = get_json("/api/service/restart")?;
    let daemon_identity = expected["daemonIdentity"]
        .as_str()
        .filter(|s| !s.is_empty())
        .ok_or("Daemon identity unavailable; nothing was stopped")?
        .to_owned();
    let check = || -> Result<(), String> {
        let identity = get_json("/api/health?identity=1")?;
        if identity["pid"].as_u64() != Some(old_pid)
            || identity["backendID"] != "wsl"
            || identity["daemonIdentity"].as_str() != Some(daemon_identity.as_str())
        {
            return Err("WSL daemon identity changed. Nothing was stopped.".into());
        }
        let capability = get_json("/api/service/restart")?;
        if capability["busy"].as_bool() == Some(true)
            || capability["supported"].as_bool() != Some(true)
            || capability["ownership"].as_str() != Some(proof.trim())
        {
            return Err("A restart/update is in progress or ownership is no longer proven. Nothing was stopped.".into());
        }
        let update = get_json("/api/update/status")?;
        if update["applying"].as_bool() == Some(true)
            || update["shellApplying"].as_bool() == Some(true)
        {
            return Err("An update is in progress. Nothing was stopped.".into());
        }
        Ok(())
    };
    check()?;
    let losses = get_json("/api/service/restart")?;
    let held = losses["heldDeliveries"].as_u64().unwrap_or(0);
    let receipts = losses["putBackReceipts"].as_u64().unwrap_or(0);
    let confirmed = app.dialog().message(format!(
        "Restart only the WSL background service (PID {old_pid})?\n\nPanes keep running and reconnect. {held} held deliveries and {receipts} put-back receipts may be lost (counts at confirmation). The service will read your changes to ~/.solu/env; this action does not edit that file."
    )).title("Restart background service").buttons(MessageDialogButtons::OkCancel).blocking_show();
    if !confirmed {
        return Ok(None);
    }
    check()?;
    native_log(
        "native tray background service restart confirmed; stopping only the proven WSL daemon",
    );
    native_progress(app, true);
    if let Some(tray) = app.tray_by_id("main") {
        let _ = tray.set_tooltip(Some("SoluTerminal — restarting background service…"));
    }
    let stopped = run_wsl_restart(
        &[
            "-e",
            "/usr/bin/env",
            "-i",
            "PATH=/usr/bin:/bin",
            "LC_ALL=C",
            "/bin/bash",
            "--noprofile",
            "--norc",
            "-p",
            "-s",
            "--",
            "stop",
            parts[0],
            parts[1],
        ],
        Some(SCRIPT.as_bytes()),
        Duration::from_secs(15),
    )?;
    if !stopped.status.success() {
        return Err("WSL ownership changed or the stop was refused. No retry was sent.".into());
    }
    let deadline = std::time::Instant::now() + Duration::from_secs(90);
    while std::time::Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(1500));
        if let Ok(identity) = get_json("/api/health?identity=1") {
            if identity["backendID"] == "wsl"
                && identity["daemonIdentity"].as_str() == Some(daemon_identity.as_str())
            {
                if let Some(pid) = identity["pid"].as_u64().filter(|p| *p > 0 && *p != old_pid) {
                    return Ok(Some(format!("Background service restarted. PID {old_pid} → {pid}. Panes are reconnecting.")));
                }
            }
        }
    }
    Err("The WSL daemon has not returned with a new PID. No automatic retry was sent. Check the daemon log.".into())
}

#[cfg(windows)]
fn run_wsl_restart(
    args: &[&str],
    stdin: Option<&[u8]>,
    timeout: Duration,
) -> Result<std::process::Output, String> {
    crate::daemon::run_wsl_bounded_with_stdin(args, stdin, timeout)
}

// Compile the native Windows flow in Linux desktop tests as well; executing a
// WSL command still requires Windows. There is no webview command registration.
#[cfg(all(test, not(windows)))]
fn run_wsl_restart(
    _: &[&str],
    _: Option<&[u8]>,
    _: Duration,
) -> Result<std::process::Output, String> {
    Err("WSL restart requires Windows".into())
}

#[cfg(any(windows, test))]
fn native_progress(app: &tauri::AppHandle, pending: bool) {
    if let Some(item) = app.try_state::<RestartMenuItem>() {
        let _ = item.0.set_enabled(!pending);
        let _ = item.0.set_text(if pending {
            "Restarting background service…"
        } else {
            "Restart background service"
        });
    }
}

#[cfg(any(windows, test))]
fn native_log(message: &str) {
    #[cfg(windows)]
    crate::daemon::launch_log(message);
    #[cfg(not(windows))]
    eprintln!("{message}");
}
