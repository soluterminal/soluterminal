// "Update this computer" from Cloud launch mode (desktop_update_this_computer).
//
// In Cloud launch mode the window shows the cloud-served page and the shell
// starts no local daemon (lib.rs start_backends). On the team channel only
// this computer's own daemon can fetch and verify the desktop installer: it
// downloads it through the cloud with this computer's machine token and
// accepts it only from a manifest signed "flavor": "team"
// (daemon/updater_shell.go ApplyShell). The app's own updater
// (shell_update.rs) reads the public releases only. So a team computer used to
// have to switch to Local mode just to update (#750).
//
// This command does what Local mode would do, without leaving Cloud mode:
//
//   0. Ask first, in a native dialog the page cannot draw or answer
//      (CONFIRM_TITLE / CONFIRM_MESSAGE). On Cancel nothing starts and the
//      page gets "cancelled".
//   1. Start this computer's own installed daemon headless, the way Local mode
//      starts it (the saved backend: WSL, with Windows as its fallback, or
//      Windows), with its normal data dir. A daemon already answering is used
//      as is; a Windows daemon this flow starts gets a free loopback port.
//      A daemon this flow starts runs its update-only boot
//      (daemon::HEADLESS_UPDATE_ENV, SOLU_UPDATE_ONLY=1;
//      daemon/update_only_boot.go): the updater, its machine token and one
//      entitlements check, the auth gate and four update routes, nothing
//      else. No relay, pairing, check-in, panes, scheduler, registrations or
//      self-update, and saved state is left untouched.
//   2. Wait until it answers, then ask it for a fresh update check
//      (POST /api/update/check) with its own token, read here and never given
//      to the page.
//   3. When it reports a desktop installer update, ask it to install it
//      (POST /api/update/apply-shell, attended: the installer window and UAC
//      prompt show, as for a click in Local mode). The daemon applies every
//      check it applies in Local mode: channel, signed manifest, flavour,
//      SHA-256, rollback hold, downgrade refusal.
//   4. Stop everything this flow started, whatever the outcome, right after
//      apply-shell answers (not at app exit): a Windows daemon with its
//      watchdog, or the WSL daemon and its start-wsl.sh supervisor, found by
//      the marker in their own environment, stopped by a script that runs
//      detached inside WSL (so the installer closing the app cannot cut it
//      short) and confirmed gone. A daemon that was already running is never
//      signalled. A leftover update-only daemon is never reused as a normal
//      one: it reports updateOnly in /api/health and every reuse probe
//      refuses it (daemon.rs check_backend_identity, start-wsl.sh). The installer runs on
//      its own (handed to explorer.exe, outside the daemon's process tree),
//      closes the app, installs, and the app comes back in Cloud mode: the
//      launch mode lives in desktop-mode.txt, which nothing here writes and
//      the installer does not touch.
//
// What the page can do with it: the command takes no argument at all, so no
// URL, path, version or flag reaches the daemon from the page. The worst a
// hostile cloud page could do is make this computer install the latest
// installer its own daemon already accepts (signed, for its channel, not
// older than what is installed), with the installer window showing. It never
// talks to a remote daemon, and the daemon refuses installer requests that
// arrive over the relay anyway (daemon/update_api.go refuseInstallerOverRelay).
//
// Only the main window may call it (capabilities/default.json, and checked
// again here), and only in Cloud launch mode (CloudLaunch).

use std::io::Read;
use std::time::{Duration, Instant};

/// Managed only when this launch runs in Cloud launch mode (lib.rs).
pub struct CloudLaunch;

/// A loopback daemon and the token this shell read for it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct DaemonEndpoint {
    origin: String,
    token: Option<String>,
}

impl DaemonEndpoint {
    /// From a daemon URL as the start code returns it: plain-http loopback
    /// only, with its `token` query parameter when it carries one, else
    /// `fallback_token`.
    fn from_url(url: &str, fallback_token: Option<String>) -> Option<Self> {
        let parsed = tauri::Url::parse(url).ok()?;
        if parsed.scheme() != "http" {
            return None;
        }
        let host = parsed.host_str()?;
        if host != "127.0.0.1" && host != "localhost" {
            return None;
        }
        let port = parsed.port()?;
        let token = parsed
            .query_pairs()
            .find(|(key, _)| key == "token")
            .map(|(_, value)| value.into_owned())
            .filter(|value| !value.is_empty())
            .or(fallback_token.filter(|value| !value.is_empty()));
        Some(Self {
            origin: format!("http://127.0.0.1:{port}"),
            token,
        })
    }
}

/// Stops what the flow started and says whether it is gone.
type StopFn = Box<dyn FnOnce() -> Result<(), String> + Send>;

/// This computer's daemon for the length of one update.
pub(crate) struct HeadlessDaemon {
    endpoint: DaemonEndpoint,
    /// Stops what this flow started. None when it used a running daemon,
    /// which is left exactly as it was.
    stop: Option<StopFn>,
}

impl HeadlessDaemon {
    fn finish(self) -> Result<(), String> {
        match self.stop {
            Some(stop) => stop(),
            None => Ok(()),
        }
    }
}

/// What the page gets back. `state` is one of "cancelled" (the user said no
/// in the confirmation), "launched" (the installer is running), "up-to-date",
/// "busy", "refused" (the daemon declined on policy) or "failed". `message`
/// is ready to show. `cleanup_error` says a daemon this flow started could
/// not be confirmed stopped.
#[derive(serde::Serialize, Debug, Clone, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ThisComputerUpdate {
    pub state: &'static str,
    pub installer_version: Option<String>,
    pub current_version: Option<String>,
    pub code: Option<String>,
    pub message: Option<String>,
    pub cleanup_error: Option<String>,
}

impl ThisComputerUpdate {
    fn new(state: &'static str) -> Self {
        Self {
            state,
            installer_version: None,
            current_version: None,
            code: None,
            message: None,
            cleanup_error: None,
        }
    }

    fn failed(message: String) -> Self {
        Self {
            message: Some(message),
            ..Self::new("failed")
        }
    }
}

/// How long each step may take.
pub(crate) struct Timeouts {
    /// For the daemon to answer once started (a WSL start can be slow).
    ready: Duration,
    /// For the update check, which fetches the signed manifest.
    check: Duration,
    /// For apply-shell, which downloads the installer before it launches it.
    apply: Duration,
}

const PRODUCTION_TIMEOUTS: Timeouts = Timeouts {
    ready: Duration::from_secs(45),
    check: Duration::from_secs(60),
    apply: Duration::from_secs(15 * 60),
};

/// The progress phases the page may show, in order.
pub(crate) const PHASE_STARTING: &str = "starting";
pub(crate) const PHASE_CHECKING: &str = "checking";
pub(crate) const PHASE_INSTALLING: &str = "installing";

const MAX_BODY: u64 = 1024 * 1024;

fn as_str(value: &serde_json::Value, key: &str) -> Option<String> {
    value
        .get(key)
        .and_then(serde_json::Value::as_str)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

/// One request to the daemon, with its token. Blocking: call it off the
/// async runtime (the command runs the flow on its own thread).
fn call(
    endpoint: &DaemonEndpoint,
    post: bool,
    path: &str,
    timeout: Duration,
) -> Result<(u16, serde_json::Value), String> {
    let client = reqwest::blocking::Client::builder()
        .http1_only()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .retry(reqwest::retry::never())
        .connect_timeout(Duration::from_secs(2))
        .timeout(timeout)
        .build()
        .map_err(|_| "could not create the daemon client".to_string())?;
    let url = format!("{}{path}", endpoint.origin);
    let mut request = if post {
        client.post(&url).header(reqwest::header::CONTENT_LENGTH, "0")
    } else {
        client.get(&url)
    };
    if let Some(token) = &endpoint.token {
        let mut header = reqwest::header::HeaderValue::from_str(&format!("Bearer {token}"))
            .map_err(|_| "invalid daemon authorization".to_string())?;
        header.set_sensitive(true);
        request = request.header(reqwest::header::AUTHORIZATION, header);
    }
    // No library error text: it can carry the request's credentials.
    let response = request
        .send()
        .map_err(|_| format!("this computer's SoluTerminal service did not answer {path}"))?;
    let status = response.status().as_u16();
    let mut body = Vec::new();
    response
        .take(MAX_BODY + 1)
        .read_to_end(&mut body)
        .map_err(|_| format!("could not read the answer to {path}"))?;
    if body.len() as u64 > MAX_BODY {
        return Err(format!("the answer to {path} is too large"));
    }
    let value = serde_json::from_slice(&body).unwrap_or_else(|_| {
        let text = String::from_utf8_lossy(&body);
        serde_json::Value::String(text.trim().chars().take(300).collect())
    });
    Ok((status, value))
}

fn wait_until_ready(endpoint: &DaemonEndpoint, within: Duration) -> Result<serde_json::Value, String> {
    let deadline = Instant::now() + within;
    loop {
        let last = match call(endpoint, false, "/api/update/status", Duration::from_secs(10)) {
            Ok((200, status)) if status.is_object() => return Ok(status),
            Ok((code, _)) => format!("its update status returned HTTP {code}"),
            Err(e) => e,
        };
        if Instant::now() >= deadline {
            return Err(last);
        }
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// An update-only daemon (relay off) says when its updater has the cloud
/// credentials it needs (health `updaterReady`); wait for that so the first
/// check does not fail. A daemon that does not report it is ready.
fn wait_updater_ready(endpoint: &DaemonEndpoint, within: Duration) -> Result<(), String> {
    let deadline = Instant::now() + within;
    loop {
        let ready = match call(endpoint, false, "/api/health?identity=1", Duration::from_secs(10)) {
            Ok((200, health)) => health.get("updaterReady").and_then(serde_json::Value::as_bool) != Some(false),
            _ => true,
        };
        if ready {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err("its updater did not get ready".to_string());
        }
        std::thread::sleep(Duration::from_millis(250));
    }
}

/// Check and install with a daemon that is up. Never stops it.
fn drive(endpoint: &DaemonEndpoint, progress: &dyn Fn(&str), timeouts: &Timeouts) -> ThisComputerUpdate {
    let cached = match wait_until_ready(endpoint, timeouts.ready) {
        Ok(status) => status,
        Err(e) => return ThisComputerUpdate::failed(format!(
            "This computer's SoluTerminal service did not start ({e}); nothing was installed."
        )),
    };
    if let Err(e) = wait_updater_ready(endpoint, timeouts.ready) {
        return ThisComputerUpdate::failed(format!(
            "This computer's SoluTerminal service did not start ({e}); nothing was installed."
        ));
    }
    progress(PHASE_CHECKING);
    // A fresh check, so a daemon that just started knows its manifest. If the
    // check itself fails, the daemon still answers with its status.
    let status = match call(endpoint, true, "/api/update/check", timeouts.check) {
        Ok((200, status)) if status.is_object() => status,
        _ => cached,
    };
    let latest = as_str(&status, "shellLatest");
    let current = as_str(&status, "shellCurrent");
    let with_versions = |mut outcome: ThisComputerUpdate| {
        outcome.installer_version = latest.clone();
        outcome.current_version = current.clone();
        outcome
    };
    if status.get("shellApplying").and_then(serde_json::Value::as_bool) == Some(true) {
        return with_versions(ThisComputerUpdate {
            message: Some("This computer's desktop app is already being updated.".into()),
            ..ThisComputerUpdate::new("busy")
        });
    }
    if status.get("shellAvailable").and_then(serde_json::Value::as_bool) != Some(true) {
        // No update is "up to date" only after a check that succeeded.
        if let Some(error) = as_str(&status, "lastError") {
            return with_versions(ThisComputerUpdate::failed(format!(
                "This computer's update check failed: {error}"
            )));
        }
        let checked = status
            .get("lastSuccessMs")
            .and_then(serde_json::Value::as_i64)
            .is_some_and(|ms| ms > 0);
        if !checked {
            return with_versions(ThisComputerUpdate::failed(
                "This computer's update check did not finish. Try again in a minute.".into(),
            ));
        }
        return with_versions(ThisComputerUpdate::new("up-to-date"));
    }

    progress(PHASE_INSTALLING);
    let (code, body) = match call(endpoint, true, "/api/update/apply-shell", timeouts.apply) {
        Ok(answer) => answer,
        Err(e) => return with_versions(ThisComputerUpdate::failed(format!(
            "The installer update did not finish: {e}."
        ))),
    };
    let typed = as_str(&body, "code");
    let daemon_message = as_str(&body, "message").or_else(|| body.as_str().map(str::to_string));
    if code == 200 && body.get("applying").and_then(serde_json::Value::as_bool) == Some(true) {
        return with_versions(ThisComputerUpdate::new("launched"));
    }
    let refused = |message: String| {
        with_versions(ThisComputerUpdate {
            code: typed.clone(),
            message: Some(message),
            ..ThisComputerUpdate::new("refused")
        })
    };
    match typed.as_deref() {
        Some("shell_daemon_downgrade_refused") => refused(
            "This computer's installer is older than the app already installed here, so nothing was installed.".into(),
        ),
        Some("update_held") => refused(
            "This version is on hold on this computer after a rollback, so nothing was installed.".into(),
        ),
        _ => {
            let detail = daemon_message.unwrap_or_else(|| format!("HTTP {code}"));
            let mut outcome = with_versions(ThisComputerUpdate::failed(format!(
                "The installer update failed: {detail}"
            )));
            outcome.code = typed;
            outcome
        }
    }
}

/// The native confirmation, shown before anything starts.
pub(crate) const CONFIRM_TITLE: &str = "Update this computer's desktop app";
pub(crate) const CONFIRM_MESSAGE: &str = "Update this computer's desktop app?\n\n\
SoluTerminal will briefly start its background service on this computer to download and check the installer.";
pub(crate) const CONFIRM_OK: &str = "Update";
pub(crate) const CONFIRM_CANCEL: &str = "Cancel";

/// The whole flow: ask, start (or find) this computer's daemon, check,
/// install, and stop whatever this flow started. `accept` runs only after the
/// person confirmed, and what it returns (in production, the hold that lets
/// the flow's own checks accept an update-only daemon) lives until the flow
/// has stopped what it started.
pub(crate) fn run_update<G>(
    confirm: impl FnOnce() -> bool,
    accept: impl FnOnce() -> G,
    start: impl FnOnce() -> Result<HeadlessDaemon, String>,
    progress: &dyn Fn(&str),
    timeouts: &Timeouts,
) -> ThisComputerUpdate {
    if !confirm() {
        return ThisComputerUpdate::new("cancelled");
    }
    let _accepting = accept();
    progress(PHASE_STARTING);
    let daemon = match start() {
        Ok(daemon) => daemon,
        Err(e) => {
            return ThisComputerUpdate::failed(format!(
                "Couldn't start this computer's SoluTerminal service: {e}"
            ))
        }
    };
    let mut outcome = drive(&daemon.endpoint, progress, timeouts);
    // Once apply-shell has answered, the installer (or nothing) runs on its
    // own; the daemon has no part left. A daemon that was already running is
    // left alone.
    if let Err(e) = daemon.finish() {
        outcome.cleanup_error = Some(e);
    }
    outcome
}

/// A loopback address nothing listens on right now.
fn free_loopback_addr() -> Result<String, String> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")
        .map_err(|e| format!("no free local port: {e}"))?;
    let port = listener
        .local_addr()
        .map_err(|e| format!("no free local port: {e}"))?
        .port();
    Ok(format!("127.0.0.1:{port}"))
}

/// This computer's host-native daemon: one already answering on a known
/// address (`reuse`, as (address, backend id)) is used as is; otherwise
/// `spawn` starts one on a free loopback port and returns its URL and the
/// way to stop it.
fn start_host_daemon(
    reuse: &[(&str, &str)],
    answers: impl Fn(&str, &str, Option<&str>) -> bool,
    spawn: impl FnOnce(&str) -> Result<(String, StopFn), String>,
    token: impl Fn() -> Option<String>,
) -> Result<HeadlessDaemon, String> {
    // Read before discovery: the identity probe needs it on a daemon that
    // requires the token on loopback too.
    let known_token = token();
    for (addr, backend) in reuse {
        if answers(addr, backend, known_token.as_deref()) {
            let endpoint = DaemonEndpoint::from_url(&format!("http://{addr}"), known_token)
                .ok_or_else(|| format!("unusable daemon address {addr}"))?;
            return Ok(HeadlessDaemon { endpoint, stop: None });
        }
    }
    let addr = free_loopback_addr()?;
    let (url, stop) = spawn(&addr)?;
    // A daemon starting for the first time writes its token as it starts.
    match DaemonEndpoint::from_url(&url, known_token.or_else(&token)) {
        Some(endpoint) => Ok(HeadlessDaemon { endpoint, stop: Some(stop) }),
        None => {
            let _ = stop();
            Err(format!("the daemon started at an unusable address {url}"))
        }
    }
}

/// The host-native daemons a running app may have left (Local mode on
/// Windows only, or Dual's Windows half).
#[cfg(windows)]
const HOST_REUSE: &[(&str, &str)] = &[
    ("127.0.0.1:8787", crate::daemon::BACKEND_ID_HOST),
    (crate::daemon::DUAL_WIN_ADDR, crate::daemon::BACKEND_ID_WIN),
];
#[cfg(not(windows))]
const HOST_REUSE: &[(&str, &str)] = &[("127.0.0.1:8787", crate::daemon::BACKEND_ID_HOST)];

#[cfg(windows)]
fn host_token() -> Option<String> {
    crate::daemon::read_win_token().ok()
}

/// Loopback is token-exempt on a host-native daemon unless the operator set
/// SOLU_LOOPBACK_REQUIRE_AUTH; send the token when it can be read.
#[cfg(not(windows))]
fn host_token() -> Option<String> {
    let home = dirs::home_dir()?;
    std::fs::read_to_string(home.join(".solu").join("token"))
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

fn start_this_computer_host_daemon() -> Result<HeadlessDaemon, String> {
    crate::shell_info::write_host(
        env!("SOLU_SHELL_VERSION"),
        crate::daemon::host_shell_platform(),
    );
    start_host_daemon(
        HOST_REUSE,
        crate::daemon::daemon_answers_as,
        |addr| {
            let started = crate::daemon::ensure_daemon_running_on_with(
                crate::daemon::BACKEND_ID_HOST,
                addr,
                true,
                true,
            )?;
            let guard = started.guard;
            // Dropping the guard stops the watchdog first, then kills and
            // reaps the daemon.
            Ok((
                started.initial_url,
                Box::new(move || {
                    drop(guard);
                    Ok(())
                }) as StopFn,
            ))
        },
        host_token,
    )
}

/// Start (or find) the daemon Local mode would use, by the saved backend.
fn start_this_computer_daemon(app: &tauri::AppHandle) -> Result<HeadlessDaemon, String> {
    #[cfg(windows)]
    {
        use crate::backend::Backend;
        // Dual's router sends the installer update to its WSL daemon, and a
        // failed WSL start falls back to Windows, as in Local mode.
        if matches!(crate::backend::saved_or_default(), Backend::Wsl | Backend::Dual) {
            crate::shell_info::write_wsl(env!("SOLU_SHELL_VERSION"), "windows-amd64");
            match crate::daemon::ensure_daemon_running_wsl_with(app, true) {
                Ok(started) => {
                    let guard = started.guard;
                    let endpoint = DaemonEndpoint::from_url(
                        &started.initial_url,
                        crate::daemon::read_wsl_token().ok(),
                    );
                    // A leftover update-only daemon from an earlier, cut-short
                    // run is ours too (its environ carries the marker): stop it.
                    let leftover = endpoint.as_ref().is_some_and(is_update_only);
                    let stop: Option<StopFn> = if guard.owns_process() || leftover {
                        // This flow started the WSL daemon: record its pid
                        // and its supervisor's now, to stop both afterwards.
                        let daemon_pid = endpoint.as_ref().and_then(daemon_pid);
                        let supervisor_pid = crate::daemon::read_wsl_supervisor_pid();
                        crate::daemon::launch_log(&format!(
                            "update this computer: started the WSL daemon (pid {daemon_pid:?}, supervisor {supervisor_pid:?})"
                        ));
                        Some(Box::new(move || {
                            drop(guard);
                            crate::daemon::stop_wsl_daemon(supervisor_pid, daemon_pid)
                        }))
                    } else if guard.is_empty() {
                        None
                    } else {
                        // A WSL daemon that was already running: stop only
                        // the Windows-side proxy this flow opened.
                        Some(Box::new(move || {
                            drop(guard);
                            Ok(())
                        }))
                    };
                    let Some(endpoint) = endpoint else {
                        if let Some(stop) = stop {
                            let _ = stop();
                        }
                        return Err("the WSL service is at an unusable address".to_string());
                    };
                    return Ok(HeadlessDaemon { endpoint, stop });
                }
                Err(e) => crate::daemon::launch_log(&format!(
                    "update this computer: WSL backend failed ({e}); using Windows"
                )),
            }
        }
    }
    #[cfg(not(windows))]
    let _ = app;
    start_this_computer_host_daemon()
}

/// Whether the daemon says it is an update-only one (health `updateOnly`).
#[cfg_attr(not(windows), allow(dead_code))]
fn is_update_only(endpoint: &DaemonEndpoint) -> bool {
    matches!(
        call(endpoint, false, "/api/health?identity=1", Duration::from_secs(10)),
        Ok((200, health)) if health.get("updateOnly").and_then(serde_json::Value::as_bool) == Some(true)
    )
}

/// The pid a daemon reports for itself.
#[cfg_attr(not(windows), allow(dead_code))]
fn daemon_pid(endpoint: &DaemonEndpoint) -> Option<u32> {
    let (code, health) = call(endpoint, false, "/api/health?identity=1", Duration::from_secs(10)).ok()?;
    if code != 200 {
        return None;
    }
    health
        .get("pid")
        .and_then(serde_json::Value::as_u64)
        .and_then(|pid| u32::try_from(pid).ok())
        .filter(|pid| *pid > 1)
}

/// Asks the user natively, on top of the main window. Blocking: call it off
/// the main thread (the flow runs on its own).
fn confirm_natively(app: &tauri::AppHandle) -> bool {
    use tauri::Manager;
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind};
    let mut dialog = app
        .dialog()
        .message(CONFIRM_MESSAGE)
        .title(CONFIRM_TITLE)
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            CONFIRM_OK.into(),
            CONFIRM_CANCEL.into(),
        ));
    if let Some(main) = app.get_webview_window("main") {
        dialog = dialog.parent(&main);
    }
    dialog.blocking_show()
}

pub(crate) fn require_main_webview(label: &str) -> Result<(), String> {
    if label == "main" {
        Ok(())
    } else {
        Err("only the main window can update this computer's desktop app".to_string())
    }
}

fn dispatch_progress(app: &tauri::AppHandle, phase: &str) {
    use tauri::Manager;
    let Some(webview) = app.get_webview_window("main") else {
        return;
    };
    let detail = serde_json::json!({ "phase": phase });
    let _ = webview.eval(format!(
        "window.dispatchEvent(new CustomEvent('solu:update-this-computer', {{ detail: {detail} }}));"
    ));
}

static IN_FLIGHT: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// Install this computer's own desktop update from Cloud launch mode. No
/// arguments, by design (see the top of this file).
#[tauri::command]
pub async fn desktop_update_this_computer(
    app: tauri::AppHandle,
    webview: tauri::Webview,
) -> Result<ThisComputerUpdate, String> {
    use std::sync::atomic::Ordering;
    use tauri::Manager;

    require_main_webview(webview.label())?;
    if app.try_state::<CloudLaunch>().is_none() {
        return Err("updating this computer from here is only for Cloud launch mode".to_string());
    }
    if IN_FLIGHT.swap(true, Ordering::SeqCst) {
        return Ok(ThisComputerUpdate {
            message: Some("This computer's desktop app is already being updated.".into()),
            ..ThisComputerUpdate::new("busy")
        });
    }
    let worker = app.clone();
    // reqwest's blocking client and the daemon start code must not run on the
    // async runtime, so the flow gets a thread of its own.
    let joined = tauri::async_runtime::spawn_blocking(move || {
        std::thread::spawn(move || {
            let progress = |phase: &str| dispatch_progress(&worker, phase);
            run_update(
                || confirm_natively(&worker),
                // Only once the person clicked Update: the flow's own checks
                // accept the update-only daemon it starts (every other reuse
                // check keeps refusing one).
                crate::daemon::AcceptUpdateOnlyDaemons::hold,
                || start_this_computer_daemon(&worker),
                &progress,
                &PRODUCTION_TIMEOUTS,
            )
        })
        .join()
    })
    .await;
    IN_FLIGHT.store(false, Ordering::SeqCst);
    let outcome = match joined {
        Ok(Ok(outcome)) => outcome,
        _ => ThisComputerUpdate::failed("The update stopped unexpectedly; nothing was installed.".into()),
    };
    #[cfg(windows)]
    crate::daemon::launch_log(&format!(
        "update this computer: {} {} {}",
        outcome.state,
        outcome.message.as_deref().unwrap_or(""),
        outcome.cleanup_error.as_deref().unwrap_or("")
    ));
    Ok(outcome)
}

#[cfg(test)]
#[path = "../tests/this_computer_update/flow.rs"]
mod tests;
