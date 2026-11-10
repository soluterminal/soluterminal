// Capability scoping, checked through Tauri's own ACL with the real
// capabilities/*.json. A capability's remote.urls names the exact origins the
// app's windows load (daemon::APP_ADDRS and the hosted cloud); any other page,
// such as a preview of a pane's dev server on its own origin, gets no
// commands, even inside the app's window and with the window's invoke key.

use tauri::ipc::{CallbackFn, InvokeBody};
use tauri::test::{get_ipc_response, mock_builder, INVOKE_KEY};
use tauri::webview::InvokeRequest;
use tauri::{WebviewUrl, WebviewWindowBuilder};

struct AsWebview<'a>(&'a tauri::Webview<tauri::test::MockRuntime>);
impl AsRef<tauri::Webview<tauri::test::MockRuntime>> for AsWebview<'_> {
    fn as_ref(&self) -> &tauri::Webview<tauri::test::MockRuntime> {
        self.0
    }
}

fn ask_cmd(webview: &tauri::Webview<tauri::test::MockRuntime>, origin: &str, cmd: &str) -> bool {
    get_ipc_response(
        &AsWebview(webview),
        InvokeRequest {
            cmd: cmd.into(),
            callback: CallbackFn(0),
            error: CallbackFn(1),
            url: origin.parse().unwrap(),
            body: InvokeBody::default(),
            headers: Default::default(),
            invoke_key: INVOKE_KEY.to_string(),
        },
    )
    .is_ok()
}

// One expansion of generate_context!() for the tests (run() has the other).
fn app() -> tauri::App<tauri::test::MockRuntime> {
    mock_builder()
        .invoke_handler(tauri::generate_handler![
            desktop_update_this_computer,
            apply_shell_update,
            desktop_set_backend_mode,
            browser_pane_ping
        ])
        .build(tauri::generate_context!())
        .unwrap()
}

// Stands in for this_computer_update::desktop_update_this_computer under the
// same name, so the ACL decision is tested without starting a daemon.
#[tauri::command]
fn desktop_update_this_computer() -> &'static str {
    "ok"
}

// Stand-ins for more desktop commands under their real names: installing an
// update, switching the backend (restarts the shell) and the version ping.
#[tauri::command]
fn apply_shell_update() -> &'static str {
    "ok"
}
#[tauri::command]
fn desktop_set_backend_mode() -> &'static str {
    "ok"
}
#[tauri::command]
fn browser_pane_ping() -> &'static str {
    "ok"
}

const DESKTOP_COMMANDS: [&str; 5] = [
    "plugin:app|version",
    "browser_pane_ping",
    "apply_shell_update",
    "desktop_set_backend_mode",
    "desktop_update_this_computer",
];

// Only the main window may ask this computer's daemon to install its update.
// Workspace windows and any other window are refused, from the cloud page's
// origin as from a local one.
#[test]
fn only_the_main_window_may_update_this_computer() {
    let app = app();
    let cloud = "https://soluterminal-cloud.onrender.com/";
    for (label, allowed) in [("main", true), ("workspace_2", false), ("browser_1", false)] {
        let window =
            WebviewWindowBuilder::new(&app, label, WebviewUrl::External(cloud.parse().unwrap()))
                .build()
                .unwrap();
        assert_eq!(
            ask_cmd(window.as_ref(), cloud, "desktop_update_this_computer"),
            allowed,
            "{label}"
        );
        // Any other origin, even in the main window, gets nothing.
        assert!(!ask_cmd(
            window.as_ref(),
            "https://evil.example/",
            "desktop_update_this_computer"
        ));
    }
}

// A preview page framed in the app's window (a pane's dev server, the router's
// or the daemon's preview origin on a random port) asks with the window's own
// invoke key and is refused every command; the app's own origins keep them.
#[test]
fn a_framed_preview_page_gets_no_commands() {
    let app = app();
    let daemon = "http://127.0.0.1:8787/";
    let window =
        WebviewWindowBuilder::new(&app, "main", WebviewUrl::External(daemon.parse().unwrap()))
            .build()
            .unwrap();
    for addr in crate::daemon::APP_ADDRS {
        let origin = format!("http://{addr}/");
        for cmd in DESKTOP_COMMANDS {
            assert!(ask_cmd(window.as_ref(), &origin, cmd), "{origin} lost {cmd}");
        }
    }
    for preview in [
        "http://127.0.0.1:49152/",
        "http://127.0.0.1:5173/",
        "http://localhost:5173/",
        "http://localhost:8787/",
        "http://[::1]:5173/",
        "http://127.0.0.1:8788/",
    ] {
        for cmd in DESKTOP_COMMANDS {
            assert!(!ask_cmd(window.as_ref(), preview, cmd), "{preview} reached {cmd}");
        }
    }
}

// remote.urls lists exact origins only: no wildcard host or port, nothing but
// the app's own addresses and the hosted cloud.
#[test]
fn remote_urls_are_the_exact_app_origins() {
    let mut want: Vec<String> = crate::daemon::APP_ADDRS
        .iter()
        .map(|addr| format!("http://{addr}"))
        .collect();
    want.push("https://soluterminal-cloud.onrender.com".to_string());
    want.sort();
    let directory = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("capabilities");
    let mut checked = 0;
    for entry in std::fs::read_dir(directory).unwrap() {
        let path = entry.unwrap().path();
        let json: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let Some(urls) = json.pointer("/remote/urls").and_then(|v| v.as_array()) else {
            continue;
        };
        let mut got: Vec<String> = urls
            .iter()
            .map(|u| u.as_str().unwrap().to_string())
            .collect();
        for url in &got {
            assert!(!url.contains('*'), "{}: wildcard in {url}", path.display());
            let parsed: tauri::Url = url.parse().unwrap();
            assert!(
                parsed.path() == "/" && parsed.query().is_none(),
                "{}: {url} is not a bare origin",
                path.display()
            );
        }
        got.sort();
        assert_eq!(got, want, "{}", path.display());
        checked += 1;
    }
    assert!(checked >= 2, "expected the main and workspace capabilities");
}

// Removed with the native and pop-out browsers: no capability may name a
// browser window or webview any more.
#[test]
fn no_capability_names_a_browser_window() {
    let directory = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("capabilities");
    for entry in std::fs::read_dir(directory).unwrap() {
        let path = entry.unwrap().path();
        let text = std::fs::read_to_string(&path).unwrap();
        for retired in ["browser_*", "bpane_", "browser-pane-create", "open-external-window"] {
            assert!(!text.contains(retired), "{}: {retired}", path.display());
        }
    }
}

// Recovery bypasses webview device proof only for a real native tray click.
// Guard both registration and every shipped capability, so an accidental
// invoke surface cannot turn that recovery action into a page API.
#[test]
fn native_service_recovery_is_absent_from_webview_commands_and_capabilities() {
    let source = include_str!("lib.rs");
    let start = source
        .find(".invoke_handler(tauri::generate_handler![")
        .unwrap();
    let end = source[start..].find("])").unwrap();
    let handler = &source[start..start + end];
    let recovery = include_str!("restart_service.rs");
    assert!(!recovery.contains("#[tauri::command]"));
    let directory = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("capabilities");
    let capabilities: Vec<String> = std::fs::read_dir(directory)
        .unwrap()
        .map(|entry| std::fs::read_to_string(entry.unwrap().path()).unwrap())
        .collect();
    assert!(!capabilities.is_empty());
    for forbidden in [
        "native_restart",
        "restart_background_service",
        "restart_service::",
        "native_attempt",
    ] {
        assert!(
            !handler.contains(forbidden),
            "native recovery registered as a webview command: {forbidden}"
        );
        for capability in &capabilities {
            assert!(
                !capability.contains(forbidden),
                "native recovery granted to a webview: {forbidden}"
            );
        }
    }
}
