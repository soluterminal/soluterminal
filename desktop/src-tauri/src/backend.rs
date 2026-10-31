// First-run backend selector and persistence.
//
// On Windows, soluterminal can run the daemon in one of two backends:
//   - WSL: launches the bundled Linux solud inside WSL via our TCP
//     proxy. Full feature parity (state detection, port panel) because
//     the daemon has access to Linux /proc.
//   - Windows-native: launches the bundled solud.exe directly. Some
//     features are stubbed until Task #7 lands.
//
// On macOS / Linux there's no choice — always host-native.
//
// On first launch a PC whose WSL already works is asked once which to
// use. A PC without a working WSL starts on Windows PowerShell silently:
// a setup prompt before the window opens is the scariest moment to ask a
// newcomer for administrator rights and a restart. WSL setup stays
// reachable from the tray "Repair setup" item and the
// `desktop_set_up_wsl` command. The choice is saved at
// %LOCALAPPDATA%\soluterminal\backend.txt.

#[cfg(windows)]
use std::fs;
#[cfg(windows)]
use std::path::PathBuf;

#[cfg(windows)]
use std::os::windows::process::CommandExt;
#[cfg(windows)]
use std::process::Command;
#[cfg(windows)]
use std::thread;
#[cfg(windows)]
use std::time::{Duration, Instant};

#[cfg(windows)]
use tauri::AppHandle;
#[cfg(windows)]
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind};

// `Wsl` / `Dual` are only constructed on Windows; on other hosts we
// always pick `HostNative`. Suppress the "never constructed" warning
// on those builds.
#[allow(dead_code)]
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Backend {
    /// WSL-bundled Linux daemon (Windows only). Full features.
    Wsl,
    /// Host-native daemon (Windows .exe, macOS, Linux).
    HostNative,
    /// Both WSL and Windows-native daemons running simultaneously.
    /// Each has its own workspaces and panes; the frontend pulls from
    /// both and lets the user pick which side they're looking at.
    /// Windows only. Phase 1: plumbing to spawn both; frontend still
    /// primarily uses one of them.
    Dual,
}

/// An unattended launch must never stop on a native setup/repair dialog.
/// Host-native is the session-only safe fallback; the saved WSL intent stays
/// untouched so a later interactive launch can repair or resume it.
#[cfg(any(windows, test))]
fn unattended_backend(interactive: bool) -> Option<Backend> {
    if interactive {
        None
    } else {
        Some(Backend::HostNative)
    }
}

/// Resolve which backend to use this session. On Windows: reads the
/// saved preference, runs the first-run wizard if none, falls back to
/// HostNative for any issue. On non-Windows: always HostNative.
#[cfg(windows)]
pub fn resolve_backend(app: &AppHandle, interactive: bool) -> Backend {
    // Consume the one-shot Repair setup request on every path so it can't
    // leak into a later in-app restart.
    let offer_setup = take_wsl_setup_offer();
    if let Some(saved) = read_pref() {
        // A saved WSL/Dual preference is RE-VALIDATED every launch: the
        // distro can be removed, WSL can need updating, or the machine
        // can otherwise make WSL unusable. Returning the stale pref
        // blindly is what silently stranded a user on PowerShell with no
        // explanation. HostNative needs no validation.
        if matches!(saved, Backend::Wsl | Backend::Dual) {
            // Let the launcher attempt authenticated reuse and report disk
            // pressure. The status probe executes in the distro and could
            // wake it before admission or mistake pressure for a broken
            // installation requiring the repair wizard.
            if !crate::daemon::wsl_start_allowed() {
                return saved;
            }
            let st = wsl_status();
            if st != WslStatus::Ready {
                if let Some(fallback) = unattended_backend(interactive) {
                    return fallback;
                }
                return repair_or_fallback(app, saved, st);
            }
        }
        return saved;
    }
    if let Some(fallback) = unattended_backend(interactive) {
        return fallback;
    }
    // First run — the wizard persists the chosen pref itself (so an
    // install-needs-reboot case can keep pref=Wsl for the next launch
    // while running HostNative this session).
    let start_allowed = crate::daemon::wsl_start_allowed();
    run_first_run_wizard(app, wsl_status(), offer_setup, start_allowed)
}

/// Environment flag the tray "Repair setup" item sets before restarting.
/// Tauri restarts inherit the process environment, so the relaunched
/// shell's first-run wizard knows the user explicitly asked to re-detect
/// and should still be offered WSL setup rather than defaulting silently.
#[cfg(any(windows, test))]
const OFFER_WSL_SETUP_ENV: &str = "SOLU_OFFER_WSL_SETUP";

#[cfg(any(windows, test))]
fn request_wsl_setup_offer() {
    std::env::set_var(OFFER_WSL_SETUP_ENV, "1");
}

#[cfg(any(windows, test))]
fn take_wsl_setup_offer() -> bool {
    let requested = std::env::var_os(OFFER_WSL_SETUP_ENV).is_some();
    std::env::remove_var(OFFER_WSL_SETUP_ENV);
    requested
}

/// What the first launch (no saved preference) does for a given WSL state.
#[cfg(any(windows, test))]
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
enum FirstRunPlan {
    /// WSL works: ask which one the user prefers.
    AskPreference,
    /// A distro exists but WSL needs updating: offer the update.
    OfferUpdate,
    /// No usable WSL and the user asked via Repair setup: offer install.
    OfferInstall,
    /// No usable WSL: run on Windows without a dialog. `persist` is false
    /// when the probe couldn't be trusted (WSL start blocked by low disk
    /// space), so the next launch detects again instead of locking a WSL
    /// user onto Windows.
    WindowsSilently { persist: bool },
}

#[cfg(any(windows, test))]
fn first_run_plan(status: WslStatus, offer_setup: bool, start_allowed: bool) -> FirstRunPlan {
    match status {
        WslStatus::Ready => FirstRunPlan::AskPreference,
        WslStatus::NeedsUpdate => FirstRunPlan::OfferUpdate,
        WslStatus::NoDistro | WslStatus::Unavailable if offer_setup && start_allowed => {
            FirstRunPlan::OfferInstall
        }
        WslStatus::NoDistro | WslStatus::Unavailable => FirstRunPlan::WindowsSilently {
            persist: start_allowed,
        },
    }
}

#[cfg(not(windows))]
pub fn resolve_backend(_app: &tauri::AppHandle, _interactive: bool) -> Backend {
    Backend::HostNative
}

/// What kind of WSL situation this machine is in. The remediation
/// differs per case and conflating them is a real bug we hit: a missing
/// distro needs `wsl --install -d Ubuntu`; a stale install needs
/// `wsl --update`; neither should be treated as the other, and none
/// should silently drop the user onto PowerShell.
#[cfg(any(windows, test))]
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum WslStatus {
    /// `wsl -e true` succeeds — a default distro exists and can run.
    Ready,
    /// Platform present but no distro installed. Fix: install a distro.
    NoDistro,
    /// WSL signalled it needs updating. Fix: `wsl --update`.
    NeedsUpdate,
    /// wsl.exe not callable / platform not enabled / unknown failure.
    Unavailable,
}

/// Probe what WSL can actually do. We deliberately do NOT trust
/// `wsl --status` (it exits 0 even with ZERO distros installed). The
/// ground truth is "can I run a command in the default distro", i.e.
/// `wsl -e true`. When that fails we classify WHY (no distro vs needs
/// update vs unavailable) so the caller routes to the right fix instead
/// of silently falling back to host-native — the exact failure that
/// stranded a user on PowerShell with no explanation.
#[cfg(windows)]
fn wsl_status() -> WslStatus {
    if !crate::daemon::wsl_start_allowed() {
        return WslStatus::Unavailable;
    }
    const CREATE_NO_WINDOW: u32 = 0x08000000;
    let run = Command::new("wsl.exe")
        .args(["-e", "true"])
        .creation_flags(CREATE_NO_WINDOW)
        .output();
    let out = match run {
        Ok(o) if o.status.success() => return WslStatus::Ready,
        Ok(o) => o,                                   // ran but failed — classify
        Err(_) => return WslStatus::Unavailable,      // wsl.exe not callable
    };

    // wsl.exe emits UTF-16-ish text; from_utf8_lossy + lowercase is good
    // enough for substring classification. Combine both streams.
    let mut blob = String::from_utf8_lossy(&out.stdout).into_owned();
    blob.push_str(&String::from_utf8_lossy(&out.stderr));
    let blob = blob.to_lowercase();
    if blob.contains("wsl --update") || blob.contains("please update") || blob.contains("needs to be updated") {
        return WslStatus::NeedsUpdate;
    }
    if blob.contains("no installed distribution") || blob.contains("has no installed") {
        return WslStatus::NoDistro;
    }

    // Secondary signal: enumerate distros. `wsl -l -q` succeeds with at
    // least one name when a distro exists. Empty/failed ⇒ no distro.
    let list = Command::new("wsl.exe")
        .args(["-l", "-q"])
        .creation_flags(CREATE_NO_WINDOW)
        .output();
    match list {
        Ok(o) if o.status.success() => {
            // UTF-16LE bytes: a real name has non-NUL ASCII letters.
            let has_distro = o
                .stdout
                .iter()
                .any(|&b| b.is_ascii_alphanumeric());
            if has_distro {
                // A distro is registered but `wsl -e true` still failed —
                // most often a stale install; update is the safe move and
                // self-corrects to install if needed.
                WslStatus::NeedsUpdate
            } else {
                WslStatus::NoDistro
            }
        }
        _ => WslStatus::NoDistro,
    }
}

/// The saved backend pref WITHOUT any re-validation or dialogs — for the
/// tray label/handler, which must read the current choice without
/// triggering the repair flow that resolve_backend now performs.
#[cfg(windows)]
pub fn saved_or_default() -> Backend {
    read_pref().unwrap_or(Backend::HostNative)
}

/// Clear the saved backend choice + transient WSL artifacts so the next
/// launch re-runs detection and the first-run wizard. Backs the tray
/// "Repair setup" item (and is the in-app counterpart to a reinstall
/// reset). The relaunch still offers WSL setup when it's missing, because
/// the user asked for it. Best-effort — missing files are fine.
#[cfg(windows)]
pub fn reset_setup() {
    request_wsl_setup_offer();
    delete_pref();
    if let Some(dir) = std::env::var_os("LOCALAPPDATA").map(PathBuf::from) {
        let base = dir.join("soluterminal");
        let _ = fs::remove_file(base.join("start-wsl.sh"));
        let _ = fs::remove_file(base.join("launch.log"));
    }
}

/// First-run backend wizard. WSL-ready machines get the plain-language
/// preference dialog; a WSL that needs updating gets the guided update.
/// Without WSL the app opens on Windows PowerShell with no dialog, unless
/// the user came here through Repair setup, which still offers the guided
/// install. Persists the right pref so the next launch lands correctly
/// even when setup needs a reboot.
#[cfg(windows)]
fn run_first_run_wizard(
    app: &AppHandle,
    status: WslStatus,
    offer_setup: bool,
    start_allowed: bool,
) -> Backend {
    match first_run_plan(status, offer_setup, start_allowed) {
        FirstRunPlan::AskPreference => {} // fall through to the preference dialog
        FirstRunPlan::OfferUpdate => return persist_first_run_outcome(run_update_wsl_wizard(app)),
        FirstRunPlan::OfferInstall => {
            return persist_first_run_outcome(run_install_wsl_wizard(app));
        }
        FirstRunPlan::WindowsSilently { persist } => {
            crate::daemon::launch_log(&format!(
                "first run: WSL not ready ({status:?}); using Windows PowerShell without a setup prompt (saved={persist})"
            ));
            if persist {
                let _ = write_pref(Backend::HostNative);
            }
            return Backend::HostNative;
        }
    }

    // WSL is ready — ask the user which they prefer. Plain-language copy:
    // lead with what each choice means, not "WSL"/"native" jargon.
    let picked_wsl = app
        .dialog()
        .message(
            "How should soluterminal run on this PC?\n\n\
             • Run inside Windows (recommended) — uses Windows' built-in \
             Linux (WSL) for the full experience: reliable alerts when \
             Claude needs you, the Ports panel, and smarter pane grouping.\n\n\
             • Run on Windows directly — simpler, no extra setup, but a few \
             features are limited.\n\n\
             You can change this any time from the tray menu (\"Switch to …\").",
        )
        .title("Choose how soluterminal runs")
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            "Run inside Windows (recommended)".into(),
            "Run on Windows directly".into(),
        ))
        .blocking_show();

    let choice = if picked_wsl { Backend::Wsl } else { Backend::HostNative };
    let _ = write_pref(choice);
    choice
}

/// Map a first-run setup outcome to the session backend AND persist the
/// pref. ReadyNow → WSL. RebootRequired → keep pref=WSL so the post-
/// reboot launch auto-restores it, but run HostNative this session.
/// Declined/Failed → the user is on Windows now; persist HostNative so we
/// don't re-nag the guided setup every launch (they can switch later).
#[cfg(windows)]
fn persist_first_run_outcome(outcome: SetupOutcome) -> Backend {
    match outcome {
        SetupOutcome::ReadyNow => {
            let _ = write_pref(Backend::Wsl);
            Backend::Wsl
        }
        SetupOutcome::RebootRequired => {
            let _ = write_pref(Backend::Wsl);
            Backend::HostNative
        }
        SetupOutcome::Declined | SetupOutcome::Failed => {
            let _ = write_pref(Backend::HostNative);
            Backend::HostNative
        }
    }
}

/// A saved WSL/Dual preference is no longer usable. Guide repair instead
/// of silently dropping to PowerShell. Returns the backend to use THIS
/// session. We never overwrite the saved pref to HostNative here: keeping
/// it WSL means the next launch re-checks and auto-restores WSL once
/// repaired (e.g. after the reboot that finishes `wsl --install`).
#[cfg(windows)]
fn repair_or_fallback(app: &AppHandle, saved: Backend, status: WslStatus) -> Backend {
    let (title, body, fix_label) = match status {
        WslStatus::NoDistro => (
            "Linux (WSL) needs a distribution",
            "SoluTerminal is set to run inside Windows' Linux (WSL), but no Linux \
             distribution is installed, so the full experience can't start. Set one \
             up now? Windows will install Ubuntu (it may need a restart). You can \
             also run on Windows directly for this session.",
            "Set up Linux now",
        ),
        WslStatus::NeedsUpdate => (
            "Linux (WSL) needs an update",
            "SoluTerminal is set to run inside Windows' Linux (WSL), but WSL needs a \
             quick update before it can start. Update now? You can also run on \
             Windows directly for this session.",
            "Update WSL now",
        ),
        _ => (
            "Linux (WSL) is unavailable",
            "SoluTerminal is set to run inside Windows' Linux (WSL), but WSL isn't \
             available right now. Set it up? You can also run on Windows directly \
             for this session.",
            "Set it up",
        ),
    };
    let do_fix = app
        .dialog()
        .message(body)
        .title(title)
        .kind(MessageDialogKind::Warning)
        .buttons(MessageDialogButtons::OkCancelCustom(
            fix_label.into(),
            "Run on Windows for now".into(),
        ))
        .blocking_show();
    if !do_fix {
        // Session-only fallback; saved pref stays WSL so the next launch
        // re-checks and restores it automatically once WSL is repaired.
        return Backend::HostNative;
    }
    let outcome = match status {
        WslStatus::NeedsUpdate => do_update_and_verify(app),
        _ => do_install_and_verify(app),
    };
    match outcome {
        // Verified working now — honor the original (Wsl or Dual) intent.
        SetupOutcome::ReadyNow => saved,
        // Reboot/failed — Windows for this session, saved pref unchanged.
        _ => Backend::HostNative,
    }
}

/// How an in-app request to set up Linux (WSL) ended.
#[cfg(windows)]
pub enum WslSetupRequest {
    /// WSL works now and the pref is saved: restart to switch to it.
    SwitchNow,
    /// Setup ran; the PC must restart to finish. Pref is saved as WSL so the
    /// next launch after the restart comes up in it.
    RestartPcToFinish,
    /// The user said no to the confirm dialog. Nothing changed.
    Declined,
    /// Setup couldn't start; the user has already seen why.
    Failed,
    /// Low disk space blocks starting WSL, so a probe can't be trusted.
    LowDisk,
}

/// The gentle, later WSL offer: the user asked for it from inside the app
/// (setup checklist / settings). Reuses the same guided dialogs as Repair
/// setup but never restarts the app or changes the saved pref unless WSL
/// is usable or will be after a PC restart. Blocks while the elevated
/// install runs, so call it off the main thread and off the async runtime.
#[cfg(windows)]
pub fn set_up_wsl_on_request(app: &AppHandle) -> WslSetupRequest {
    if !crate::daemon::wsl_start_allowed() {
        return WslSetupRequest::LowDisk;
    }
    // Keep a Dual user Dual; anyone else switches to WSL.
    let target = match read_pref() {
        Some(Backend::Dual) => Backend::Dual,
        _ => Backend::Wsl,
    };
    let outcome = match wsl_status() {
        WslStatus::Ready => SetupOutcome::ReadyNow,
        WslStatus::NeedsUpdate => run_update_wsl_wizard(app),
        WslStatus::NoDistro | WslStatus::Unavailable => run_install_wsl_wizard(app),
    };
    match outcome {
        SetupOutcome::ReadyNow => {
            let _ = write_pref(target);
            WslSetupRequest::SwitchNow
        }
        SetupOutcome::RebootRequired => {
            let _ = write_pref(target);
            WslSetupRequest::RestartPcToFinish
        }
        SetupOutcome::Declined => WslSetupRequest::Declined,
        SetupOutcome::Failed => WslSetupRequest::Failed,
    }
}

/// Outcome of a guided WSL setup step.
#[cfg(windows)]
enum SetupOutcome {
    /// WSL verified runnable this session.
    ReadyNow,
    /// Setup started but needs a PC restart to finish.
    RebootRequired,
    /// User backed out before anything ran.
    Declined,
    /// Setup failed to start / errored.
    Failed,
}

/// Confirm + guided distro install. Used on first run.
#[cfg(windows)]
fn run_install_wsl_wizard(app: &AppHandle) -> SetupOutcome {
    let install_requested = app
        .dialog()
        .message(
            "soluterminal runs best using Windows' built-in Linux (WSL). \
             Set it up now for the full experience — reliable alerts when \
             Claude needs you, the Ports panel, and smarter pane grouping?\n\n\
             Windows will ask for administrator permission and will need a \
             one-time PC restart to finish. You can skip this and run on \
             Windows directly instead — a few features will be limited, and \
             you can switch later from the tray menu.",
        )
        .title("Set up the full experience?")
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            "Set it up (recommended)".into(),
            "Run on Windows directly".into(),
        ))
        .blocking_show();
    if !install_requested {
        return SetupOutcome::Declined;
    }
    do_install_and_verify(app)
}

/// Confirm + guided WSL update. Used on first run when a distro exists
/// but WSL itself needs updating.
#[cfg(windows)]
fn run_update_wsl_wizard(app: &AppHandle) -> SetupOutcome {
    let update_requested = app
        .dialog()
        .message(
            "Windows' built-in Linux (WSL) needs a quick update before \
             soluterminal can use it for the full experience. Update now?\n\n\
             Windows will ask for administrator permission. You can skip this \
             and run on Windows directly instead.",
        )
        .title("Update WSL?")
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            "Update now (recommended)".into(),
            "Run on Windows directly".into(),
        ))
        .blocking_show();
    if !update_requested {
        return SetupOutcome::Declined;
    }
    do_update_and_verify(app)
}

/// Run `wsl --install -d Ubuntu` elevated, then VERIFY it actually
/// produced a working distro. The old code treated PowerShell exit 0 as
/// success, but `wsl --install` exits 0 even when it only queued the
/// distro for post-reboot setup — faking success and stranding the user.
/// We probe `wsl -e true` and report ready-now vs reboot-required.
#[cfg(windows)]
fn do_install_and_verify(app: &AppHandle) -> SetupOutcome {
    if let Err(e) = run_wsl_command_elevated("'--install','-d','Ubuntu'") {
        app.dialog()
            .message(format!(
                "Couldn't start the setup: {e}\n\n\
                 You can set it up manually later by running `wsl --install` \
                 in an Administrator PowerShell. For now soluterminal will run \
                 directly on Windows — you can switch later from the tray menu.",
            ))
            .title("Setup didn't start")
            .kind(MessageDialogKind::Warning)
            .blocking_show();
        return SetupOutcome::Failed;
    }
    finish_setup_with_verify(
        app,
        "Linux is ready — soluterminal will now switch to the full experience.",
    )
}

/// Run `wsl --update` elevated, then verify. If WSL still can't run a
/// command afterwards (e.g. there's no distro), chain into the distro
/// install so the user is never dead-ended.
#[cfg(windows)]
fn do_update_and_verify(app: &AppHandle) -> SetupOutcome {
    if let Err(e) = run_wsl_command_elevated("'--update'") {
        app.dialog()
            .message(format!(
                "Couldn't update WSL: {e}\n\n\
                 You can update it manually by running `wsl --update` in an \
                 Administrator PowerShell. For now soluterminal will run \
                 directly on Windows.",
            ))
            .title("Update didn't start")
            .kind(MessageDialogKind::Warning)
            .blocking_show();
        return SetupOutcome::Failed;
    }
    if wait_for_wsl_ready(Duration::from_secs(8)) {
        return finish_setup_with_verify(app, "WSL is updated and ready.");
    }
    // Updated but still not runnable — most likely no distro. Don't dead-
    // end: fall through to the install flow.
    do_install_and_verify(app)
}

/// Shared post-step verify + user-facing result. Polls `wsl -e true`
/// briefly: ready → success dialog + ReadyNow; else → restart dialog +
/// RebootRequired.
#[cfg(windows)]
fn finish_setup_with_verify(app: &AppHandle, ready_msg: &str) -> SetupOutcome {
    if wait_for_wsl_ready(Duration::from_secs(8)) {
        app.dialog()
            .message(ready_msg)
            .title("Setup complete")
            .kind(MessageDialogKind::Info)
            .blocking_show();
        SetupOutcome::ReadyNow
    } else {
        app.dialog()
            .message(
                "Setup is almost done. Please restart your PC to finish, then \
                 open soluterminal again — it will switch to the full \
                 experience automatically.\n\n\
                 Until you restart, this session runs directly on Windows.",
            )
            .title("Restart to finish setup")
            .kind(MessageDialogKind::Info)
            .blocking_show();
        SetupOutcome::RebootRequired
    }
}

/// Poll until WSL can run a command or the deadline passes.
#[cfg(windows)]
fn wait_for_wsl_ready(max: Duration) -> bool {
    let deadline = Instant::now() + max;
    loop {
        if wsl_status() == WslStatus::Ready {
            return true;
        }
        if Instant::now() >= deadline {
            return false;
        }
        thread::sleep(Duration::from_millis(800));
    }
}

/// Run `wsl.exe <args>` with UAC elevation, blocking until it exits.
/// `ps_argument_list` is the PowerShell -ArgumentList literal, e.g.
/// `'--install','-d','Ubuntu'`. We use `Start-Process -Verb RunAs -Wait`
/// rather than ShellExecuteW directly — one string, handles the UAC
/// prompt, blocks until done.
#[cfg(windows)]
fn run_wsl_command_elevated(ps_argument_list: &str) -> Result<(), String> {
    const CREATE_NO_WINDOW: u32 = 0x08000000;
    let cmd = format!(
        "Start-Process -FilePath wsl.exe -ArgumentList {ps_argument_list} -Verb RunAs -Wait"
    );
    let status = Command::new("powershell.exe")
        .args(["-NoProfile", "-NonInteractive", "-Command", &cmd])
        .creation_flags(CREATE_NO_WINDOW)
        .status()
        .map_err(|e| format!("spawn powershell: {e}"))?;
    if !status.success() {
        return Err(format!("powershell exit {status}"));
    }
    Ok(())
}

#[cfg(windows)]
fn delete_pref() {
    if let Some(p) = pref_path() {
        let _ = fs::remove_file(p);
    }
}

#[cfg(windows)]
fn pref_path() -> Option<PathBuf> {
    let mut p = PathBuf::from(std::env::var_os("LOCALAPPDATA")?);
    p.push("soluterminal");
    p.push("backend.txt");
    Some(p)
}

/// The user's SAVED backend choice, unfiltered by validation. Exists for the
/// window.__SOLU_BACKEND__ "requested" signal: resolve_backend's OUTPUT folds
/// a failed-validation WSL pref into HostNative, so deriving "requested" from
/// it made "user chose Windows" and "user chose WSL but WSL is broken"
/// indistinguishable — the fallback flag under-reported exactly when the UI
/// most needed to say "you are not on the backend you picked".
#[cfg(windows)]
pub fn saved_pref() -> Option<Backend> {
    read_pref()
}

#[cfg(windows)]
fn read_pref() -> Option<Backend> {
    let p = pref_path()?;
    let s = fs::read_to_string(&p).ok()?;
    match s.trim() {
        "wsl" => Some(Backend::Wsl),
        "host" | "host-native" | "windows" => Some(Backend::HostNative),
        "dual" | "both" => Some(Backend::Dual),
        _ => None,
    }
}

#[cfg(windows)]
fn write_pref(b: Backend) -> std::io::Result<()> {
    let p = pref_path().ok_or_else(|| {
        std::io::Error::new(std::io::ErrorKind::NotFound, "no LOCALAPPDATA")
    })?;
    if let Some(dir) = p.parent() {
        fs::create_dir_all(dir)?;
    }
    let value = match b {
        Backend::Wsl => "wsl",
        Backend::HostNative => "host",
        Backend::Dual => "dual",
    };
    fs::write(&p, value)
}

/// Set the backend preference programmatically — e.g. from the tray
/// menu's "Switch backend" action. Callers typically follow up with
/// `app.restart()` to apply the change.
#[cfg(windows)]
pub fn set_pref(b: Backend) -> std::io::Result<()> {
    write_pref(b)
}

/// The next backend in the WSL → Windows → Dual → WSL cycle — used
/// to label the "Switch to X" tray menu entry at startup.
#[allow(dead_code)]
pub fn other(b: Backend) -> Backend {
    match b {
        Backend::Wsl => Backend::HostNative,
        Backend::HostNative => Backend::Dual,
        Backend::Dual => Backend::Wsl,
    }
}

/// Human-readable label for a backend, for menu text.
#[allow(dead_code)]
pub fn label(b: Backend) -> &'static str {
    match b {
        Backend::Wsl => "WSL",
        Backend::HostNative => "Windows-native",
        Backend::Dual => "Dual (WSL + Windows)",
    }
}

#[cfg(test)]
mod autostart_tests {
    use super::{unattended_backend, Backend};

    #[test]
    fn unattended_startup_falls_back_without_opening_backend_dialogs() {
        assert_eq!(unattended_backend(false), Some(Backend::HostNative));
        assert_eq!(unattended_backend(true), None);
    }
}

#[cfg(test)]
mod first_run_tests {
    use super::{
        first_run_plan, request_wsl_setup_offer, take_wsl_setup_offer, FirstRunPlan, WslStatus,
    };

    #[test]
    fn no_wsl_opens_on_windows_without_a_dialog() {
        for status in [WslStatus::NoDistro, WslStatus::Unavailable] {
            assert_eq!(
                first_run_plan(status, false, true),
                FirstRunPlan::WindowsSilently { persist: true }
            );
        }
    }

    #[test]
    fn working_wsl_keeps_the_preference_dialog() {
        assert_eq!(
            first_run_plan(WslStatus::Ready, false, true),
            FirstRunPlan::AskPreference
        );
        assert_eq!(
            first_run_plan(WslStatus::Ready, true, true),
            FirstRunPlan::AskPreference
        );
    }

    #[test]
    fn wsl_needing_an_update_keeps_the_update_offer() {
        assert_eq!(
            first_run_plan(WslStatus::NeedsUpdate, false, true),
            FirstRunPlan::OfferUpdate
        );
    }

    #[test]
    fn repair_setup_still_offers_the_install() {
        for status in [WslStatus::NoDistro, WslStatus::Unavailable] {
            assert_eq!(
                first_run_plan(status, true, true),
                FirstRunPlan::OfferInstall
            );
        }
    }

    #[test]
    fn low_disk_runs_windows_for_this_session_only() {
        // wsl_status reports Unavailable when the disk gate blocks WSL; that
        // says nothing about whether WSL is installed, so don't save Windows
        // and don't offer an install.
        for offer in [false, true] {
            assert_eq!(
                first_run_plan(WslStatus::Unavailable, offer, false),
                FirstRunPlan::WindowsSilently { persist: false }
            );
        }
    }

    #[test]
    fn repair_offer_is_one_shot() {
        request_wsl_setup_offer();
        assert!(take_wsl_setup_offer());
        assert!(!take_wsl_setup_offer());
    }
}
