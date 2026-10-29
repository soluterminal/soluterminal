// lib.rs pattern matches the `create-tauri-app` template so future
// mobile (iOS / Android) targets can reuse the same entry point.

mod autostart;
mod backend;
mod browser;
mod browser_pane;
mod daemon;
mod daemon_health;
#[cfg(any(windows, test))]
mod wsl_update;
mod hosted_cloud;
mod hotkey_probe;
mod launch_mode;
mod nav_guard;
#[cfg(test)]
mod capability_tests;
mod pair;
mod shell_info;
mod shell_update;
#[cfg(any(windows, test))]
mod shell_update_manifest;
#[cfg(desktop)]
mod single_instance;
mod startup_screen;
mod tray_icon;
mod tray_notice;
mod window_geometry;
mod workspace_window;

use tauri::{
    menu::{Menu, MenuItem, PredefinedMenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    Manager, WebviewUrl, WebviewWindowBuilder, WindowEvent,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct StartupWindowPlan {
    /// The main window is ALWAYS built hidden. Building it visible let the
    /// user focus or minimize it while WebView2 was still being created; the
    /// focus change re-entered the window procedure from WebView2's nested
    /// message pump and deadlocked the main thread (Alt+Tab or Win+D in the
    /// first seconds of launch hung the window with nothing loaded, on 0.0.1213
    /// and 0.0.1214 alike; shown by the Windows install smoke probe).
    start_hidden: bool,
    /// Show the window once `build()` has returned, i.e. after the webview
    /// exists. False for autostart, which stays in the tray.
    show_after_build: bool,
    maximize_on_first_show: bool,
}

fn startup_window_plan(
    has_restore: bool,
    saved_maximized: bool,
    started_by_autostart: bool,
) -> StartupWindowPlan {
    StartupWindowPlan {
        start_hidden: true,
        show_after_build: !started_by_autostart,
        maximize_on_first_show: has_restore && saved_maximized && started_by_autostart,
    }
}

fn show_main_window(
    app: &tauri::AppHandle,
    maximize_on_first_show: &std::sync::atomic::AtomicBool,
) {
    use std::sync::atomic::Ordering;

    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        if maximize_on_first_show.swap(false, Ordering::AcqRel) {
            let _ = window.maximize();
        }
        let _ = window.set_focus();
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // Which source this shell was built from, so a hand-installed build is
    // visible in launch.log (see docs/RUNBOOK-hand-install.md).
    #[cfg(windows)]
    daemon::launch_log(&format!(
        "shell {} source {} dirty={}",
        env!("SOLU_SHELL_VERSION"),
        env!("SOLU_SOURCE_SHA"),
        env!("SOLU_SOURCE_DIRTY")
    ));
    let builder = tauri::Builder::default();
    // First plugin: a second launch must hand off and exit before anything
    // else initializes.
    #[cfg(desktop)]
    let builder = builder.plugin(single_instance::plugin());
    builder
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_opener::init())
        // The startup window's page (startup_screen.rs), served by the shell
        // itself so it shows before any daemon is up.
        .register_uri_scheme_protocol(startup_screen::SCHEME, |_ctx, request| {
            startup_screen::respond(request.uri().path())
        })
        .invoke_handler(tauri::generate_handler![
            autostart::desktop_get_autostart,
            autostart::desktop_set_autostart,
            desktop_set_backend_mode,
            desktop_set_up_wsl,
            desktop_set_launch_mode,
            exit_for_update,
            hotkey_probe::global_hotkey_owned,
            shell_update::apply_shell_update,
            shell_update::apply_shell_update_silent,
            shell_update::app_process_id,
            shell_update::reveal_shell_installer,
            browser::open_external_window,
            workspace_window::open_workspace_window,
            browser_pane::browser_pane_ping,
            browser_pane::browser_pane_create,
            browser_pane::browser_pane_set_bounds,
            browser_pane::browser_pane_navigate,
            browser_pane::browser_pane_close,
            browser_pane::browser_pane_set_visible,
        ])
        .setup(|app| {
            let started_by_autostart = autostart::consume_autostart_arg(
                std::env::args(),
                std::env::var_os(autostart::AUTOSTART_CONSUMED_ENV).is_some(),
            );
            if started_by_autostart {
                // Tauri restarts inherit both argv and this process environment.
                // Mark the boot-only argument consumed so an explicit in-app
                // restart returns visibly instead of hiding to the tray again.
                std::env::set_var(autostart::AUTOSTART_CONSUMED_ENV, "1");
            }
            let interactive_startup = !started_by_autostart;

            // If the app version changed since last launch, wipe the
            // WebView2 cache. Protects against bad service worker state
            // locking users out across upgrades (we hit this on v7→v8
            // sw.js: the cached v7 was buggy and refused to release,
            // deadlocking all subsequent launches until cache was
            // manually nuked). The only user-visible cost is re-auth —
            // but we re-inject the token via ?token= URL on every
            // launch anyway, so the transition is transparent.
            #[cfg(windows)]
            {
                let version = app.package_info().version.to_string();
                reset_webview_cache_on_version_change(&version);
            }

            // Desktop launch mode is independent from daemon backend:
            // Local starts solud and loads localhost; Cloud skips solud
            // and loads the hosted account UI directly.
            let app_handle = app.handle().clone();

            // No first-run Local/Cloud chooser: connecting to remote machines
            // through the cloud relay is not part of the public beta, so a
            // fresh install always runs projects on this computer
            // (launch_mode::resolve defaults to Local). An install already
            // saved in Cloud mode keeps it and keeps a tray item back to Local.
            let desktop_mode = launch_mode::resolve();
            let chosen = if matches!(desktop_mode, launch_mode::LaunchMode::Local) {
                Some(backend::resolve_backend(&app_handle, interactive_startup))
            } else {
                None
            };
            // Best-effort and intentionally before daemon startup: an upgrade
            // may have removed the Run value while leaving the preference.
            autostart::reconcile();

            // Start the backends off the main thread. Setup used to block on
            // them, so nothing was on screen until they were up: two minutes
            // and more when the WSL daemon could not start (2026-10-28, an
            // older daemon in a second distro held its port). The startup
            // window says what is happening meanwhile; the main window is
            // built, exactly as before, once the backends are known.
            if interactive_startup && matches!(desktop_mode, launch_mode::LaunchMode::Local) {
                startup_screen::open(&app_handle);
            }
            let worker = app_handle.clone();
            std::thread::Builder::new()
                .name("backend-startup".into())
                .spawn(move || {
                    let started = start_backends(&worker, desktop_mode, chosen, interactive_startup);
                    if startup_screen::has_notice()
                        && worker.get_webview_window(startup_screen::LABEL).is_some()
                    {
                        // Leave the fallback notice readable before the
                        // workspace replaces it.
                        std::thread::sleep(std::time::Duration::from_secs(3));
                    }
                    startup_screen::step("Opening your workspace");
                    let main = worker.clone();
                    let posted = worker.run_on_main_thread(move || {
                        if let Err(e) =
                            finish_startup(&main, started, desktop_mode, chosen, started_by_autostart)
                        {
                            eprintln!("desktop: startup failed: {e}");
                            #[cfg(windows)]
                            daemon::launch_log(&format!("startup failed: {e}"));
                            main.exit(1);
                        }
                        startup_screen::close(&main);
                    });
                    if let Err(e) = posted {
                        eprintln!("desktop: could not finish startup on the main thread: {e}");
                    }
                })?;
            Ok(())
        })
        .on_window_event(|window, event| {
            if window.label() == "main" {
                match event {
                    WindowEvent::Focused(true) => {
                        dispatch_desktop_restored_event(window, "focused");
                    }
                    WindowEvent::Resized(_) => {
                        dispatch_desktop_restored_event(window, "resized");
                    }
                    WindowEvent::ScaleFactorChanged { .. } => {
                        dispatch_desktop_restored_event(window, "scale-factor-changed");
                    }
                    WindowEvent::Focused(false) => {
                        // Persist geometry when the user leaves the window
                        // (alt-tab away, tray "Hide") — captures the final
                        // position after a move/resize without per-pixel writes.
                        if let Some(w) = window.app_handle().get_webview_window("main") {
                            if let Some(g) = window_geometry::capture(&w) {
                                window_geometry::save(&g);
                            }
                        }
                    }
                    _ => {}
                }
            }

            // Intercept the red-X close ONLY on the main window: hide
            // to tray instead of exiting. User can fully quit via the
            // tray menu. Mirrors Slack / Discord / Tailscale.
            //
            // Secondary windows (pop-out browser windows labelled
            // `browser_*`) must close normally — otherwise clicking
            // their X leaves a hidden zombie window the user can't
            // dismiss.
            if let WindowEvent::CloseRequested { api, .. } = event {
                if window.label() == "main" {
                    // Persist geometry before hiding to tray.
                    if let Some(w) = window.app_handle().get_webview_window("main") {
                        if let Some(g) = window_geometry::capture(&w) {
                            window_geometry::save(&g);
                        }
                        dispatch_desktop_will_background_event(&w, "close-to-tray");
                    }
                    let _ = window.hide();
                    api.prevent_close();
                    tray_notice::after_close_to_tray(window.app_handle());
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

/// Start the daemon(s) for this launch: the slow part of startup, run on its
/// own thread while the startup window is up. Never fails: a backend that
/// cannot start falls back as before, ending at a URL the window can load.
fn start_backends(
    app_handle: &tauri::AppHandle,
    desktop_mode: launch_mode::LaunchMode,
    chosen: Option<backend::Backend>,
    interactive_startup: bool,
) -> daemon::DaemonStart {
    match desktop_mode {
        launch_mode::LaunchMode::Cloud => daemon::DaemonStart::single(
            daemon::SidecarGuard::empty(),
            launch_mode::cloud_web_url()
                .expect("cloud launch mode resolves only in a build with a hosted cloud")
                .into(),
        ),
        launch_mode::LaunchMode::Local => {
            let chosen = chosen.expect("local desktop mode should resolve a backend");
            // Drop ~/.solu/shell-info.json so a daemon launched
            // outside of Tauri can still discover the installed
            // shell version. Best-effort — failures are logged
            // and ignored.
            let shell_ver = env!("SOLU_SHELL_VERSION");
            let shell_plat = daemon::host_shell_platform();
            #[cfg(windows)]
            {
                match chosen {
                    backend::Backend::Wsl => {
                        shell_info::write_wsl(shell_ver, "windows-amd64");
                    }
                    backend::Backend::HostNative => {
                        shell_info::write_host(shell_ver, shell_plat);
                    }
                    backend::Backend::Dual => {
                        shell_info::write_host(shell_ver, shell_plat);
                        shell_info::write_wsl(shell_ver, "windows-amd64");
                    }
                }
            }
            #[cfg(not(windows))]
            {
                let _ = chosen;
                shell_info::write_host(shell_ver, shell_plat);
            }

            {
                #[cfg(windows)]
                {
                    match chosen {
                        backend::Backend::Wsl => {
                            startup_screen::step("Connecting to the WSL background service");
                            daemon::ensure_daemon_running_wsl(app_handle)
                            .or_else(|err| {
                                daemon::launch_log(&format!("WSL backend FAILED: {err}"));
                                daemon::launch_log("falling back to host-native");
                                startup_screen::wsl_fell_back(&err);
                                startup_screen::step("Starting the Windows background service");
                                // Surface the fallback with cause-specific copy. Without
                                // this the user picked the WSL backend but silently gets
                                // PowerShell panes and no WSL — with zero indication WHY,
                                // so they assume the app is broken (and generic "fix WSL"
                                // advice is wrong/misleading for an incomplete install or
                                // a port already held). Non-blocking so startup isn't
                                // stalled; launch.log still holds the full error.
                                if interactive_startup {
                                    use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
                                    let (title, body) = classify_wsl_fallback(&err);
                                    app_handle
                                        .dialog()
                                        .message(body)
                                        .title(title)
                                        .kind(MessageDialogKind::Warning)
                                        .show(|_| {});
                                }
                                daemon::ensure_daemon_running(daemon::BACKEND_ID_HOST)
                            })
                        }
                        backend::Backend::HostNative => {
                            startup_screen::step("Starting the Windows background service");
                            daemon::ensure_daemon_running(daemon::BACKEND_ID_HOST)
                        }
                        backend::Backend::Dual => {
                            // Spawn BOTH backends. Then spawn a Go
                            // `solud router` that multiplexes both
                            // behind 127.0.0.1:28787.
                            daemon::launch_log("Dual: spawning WSL + Windows-native + router");
                            // Circuit-breaker: WSL is attempted EXACTLY ONCE per launch.
                            // On failure we fall through to Windows-only below — we never
                            // retry WSL in a loop within a single launch (a flapping WSL
                            // VM otherwise stalled startup retrying the 45s spawn-wait).
                            startup_screen::step("Connecting to the WSL background service");
                            let wsl = daemon::ensure_daemon_running_wsl(app_handle);
                            if let Err(e) = &wsl {
                                startup_screen::wsl_fell_back(e);
                            }
                            startup_screen::step("Starting the Windows background service");
                            // The Windows backend takes its own port: WSL's localhost
                            // forwarding already owns Windows' 127.0.0.1:8787 (see
                            // daemon::DUAL_WIN_ADDR).
                            let host = daemon::ensure_daemon_running_on(
                                daemon::BACKEND_ID_WIN,
                                daemon::DUAL_WIN_ADDR,
                                true,
                            );
                            match (wsl, host) {
                                (Ok(w), Ok(h)) => {
                                    // Both daemons are up but each has its
                                    // OWN claim.json on its OWN filesystem.
                                    // Web-UI pairing only pairs the one that
                                    // served the request, leaving the other
                                    // daemon unpaired → it never pulls the
                                    // team-credential set → team auto-switch
                                    // never fires on that backend. Mirror the
                                    // claim between both so both connect and
                                    // provision. See daemon::start_dual_claim_mirror.
                                    daemon::start_dual_claim_mirror();
                                    startup_screen::step("Connecting the WSL and Windows services");
                                    const ROUTER_ADDR: &str = "127.0.0.1:28787";
                                    const ROUTER_URL: &str = "http://127.0.0.1:28787";
                                    const WSL_UPSTREAM: &str = "http://127.0.0.1:18787";
                                    let win_upstream = format!("http://{}", daemon::DUAL_WIN_ADDR);
                                    let wsl_tok = daemon::read_wsl_token()
                                        .unwrap_or_else(|e| {
                                            daemon::launch_log(&format!("router: read WSL token err: {e}"));
                                            String::new()
                                        });
                                    let win_tok = daemon::read_win_token()
                                        .unwrap_or_else(|e| {
                                            daemon::launch_log(&format!("router: read Win token err: {e}"));
                                            String::new()
                                        });
                                    match daemon::ensure_router_running(
                                        ROUTER_ADDR, WSL_UPSTREAM, &win_upstream,
                                        &wsl_tok, &win_tok,
                                    ) {
                                        Ok(router_guard) => {
                                            let merged = daemon::merge_guards(
                                                daemon::merge_guards(w.guard, h.guard),
                                                router_guard,
                                            );
                                            let initial_url =
                                                format!("{ROUTER_URL}/?solu_router=1");
                                            Ok(daemon::DaemonStart {
                                                guard: merged,
                                                initial_url,
                                                alt_url: None,
                                            })
                                        }
                                        Err(e) => {
                                            daemon::launch_log(&format!(
                                                "Dual: router failed ({e}); falling back to switcher-pill mode"
                                            ));
                                            let merged = daemon::merge_guards(w.guard, h.guard);
                                            Ok(daemon::DaemonStart {
                                                guard: merged,
                                                initial_url: w.initial_url,
                                                alt_url: Some(h.initial_url),
                                            })
                                        }
                                    }
                                }
                                (Ok(w), Err(e)) => {
                                    daemon::launch_log(&format!(
                                        "Dual: Windows-native failed ({e}), falling to WSL-only"
                                    ));
                                    Ok(w)
                                }
                                (Err(e), Ok(h)) => {
                                    daemon::launch_log(&format!(
                                        "Dual: WSL failed ({e}), falling to Windows-only"
                                    ));
                                    if interactive_startup {
                                        use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
                                        let (title, body) = classify_wsl_fallback(&e);
                                        app_handle.dialog().message(body).title(title)
                                            .kind(MessageDialogKind::Warning).show(|_| {});
                                    }
                                    // WSL is often only late (a busy or
                                    // just-booted VM). Keep watching so the
                                    // user can reconnect in one click.
                                    let watcher = app_handle.clone();
                                    daemon::watch_for_wsl_recovery(move || {
                                        offer_wsl_reconnect(&watcher)
                                    });
                                    Ok(h)
                                }
                                (Err(e1), Err(e2)) => Err(format!(
                                    "both backends failed: WSL={e1}; Windows={e2}"
                                )),
                            }
                        }
                    }
                }
                #[cfg(not(windows))]
                {
                    let _ = (app_handle, interactive_startup);
                    startup_screen::step("Starting the background service");
                    daemon::ensure_daemon_running(daemon::BACKEND_ID_HOST)
                }
            }
            .unwrap_or_else(|err| {
                eprintln!("desktop: {err}");
                eprintln!("desktop: continuing without sidecar — window will fail to load until you start solud manually");
                daemon::DaemonStart::single(
                    daemon::SidecarGuard::empty(),
                    "http://127.0.0.1:8787".into(),
                )
            })
        }
    }
}

/// Everything setup did after the backends were up: managed state, the main
/// window and the tray. Runs on the main thread.
fn finish_startup(
    app: &tauri::AppHandle,
    started: daemon::DaemonStart,
    desktop_mode: launch_mode::LaunchMode,
    chosen: Option<backend::Backend>,
    started_by_autostart: bool,
) -> tauri::Result<()> {
    #[cfg(not(windows))]
    let _ = chosen;
    app.manage(std::sync::Mutex::new(started.guard));

    // Create the main window programmatically AFTER the daemon
    // is up, with the correct URL (including ?token= for the
    // WSL backend). A static window config would start loading
    // http://127.0.0.1:8787 before our proxy is listening and
    // land the webview in an error state Chromium can't cleanly
    // recover from via navigate(). This way the webview's
    // first-ever load points at a working daemon.
    //
    // Dual mode: embed BOTH backend URLs (with their tokens) as
    // query params on the initial load. The daemon's auth gate
    // strips `token=` on its redirect but leaves other query
    // params alone, so `solu_primary` and `solu_alt` survive to
    // the frontend. Frontend uses them to switch between
    // backends without losing the ability to come back.
    let url_string = match &started.alt_url {
        Some(alt) => {
            let sep = if started.initial_url.contains('?') { "&" } else { "?" };
            format!(
                "{}{}solu_primary={}&solu_alt={}",
                started.initial_url,
                sep,
                urlencode(&started.initial_url),
                urlencode(alt),
            )
        }
        None => started.initial_url.clone(),
    };
    let url = match url_string.parse() {
        Ok(u) => WebviewUrl::External(u),
        Err(_) => WebviewUrl::External("http://127.0.0.1:8787".parse().unwrap()),
    };
    // The daemon origins the app's windows may load (nav_guard.rs).
    let mut daemon_urls = vec![started.initial_url.clone()];
    daemon_urls.extend(started.alt_url.clone());
    if url_string.parse::<tauri::Url>().is_err() {
        daemon_urls.push("http://127.0.0.1:8787".to_string()); // the fallback above
    }
    app.manage(nav_guard::AllowedLocalOrigins::from_urls(daemon_urls.iter().map(String::as_str)));

    // Backend signal for the web (status chip + fallback banner +
    // sidebar mode switcher).
    // SHARED CONTRACT: window.__SOLU_BACKEND__ = { requested,
    // actual, fallback, hostPlatform, launchMode }. "requested"
    // is the backend the user wanted (resolved from their
    // saved/first-run choice); "actual" is what actually started
    // this session. fallback is true when the user wanted wsl/dual
    // but we ended up on host. hostPlatform/launchMode let the web
    // hide Windows-only local controls in Cloud mode and on
    // macOS/Linux desktop shells.
    // "requested" comes from the SAVED pref, not from `chosen`
    // (resolve_backend's output): when a saved WSL/Dual pref fails
    // validation, resolve folds it into HostNative, and deriving
    // "requested" from that made "user chose Windows" and "user
    // chose WSL but WSL is broken" indistinguishable — fallback
    // under-reported exactly when the UI most needed to say "you
    // are not on the backend you picked". No saved pref (first
    // run) falls back to what the wizard resolved this session.
    let requested_backend = if matches!(desktop_mode, launch_mode::LaunchMode::Cloud) {
        // Cloud mode runs no local daemon; the chip is moot, so
        // report a neutral host/host with no fallback.
        "host"
    } else {
        #[cfg(windows)]
        {
            match backend::saved_pref() {
                Some(backend::Backend::Wsl) => "wsl",
                Some(backend::Backend::Dual) => "dual",
                Some(backend::Backend::HostNative) => "host",
                None => match chosen {
                    Some(backend::Backend::Wsl) => "wsl",
                    Some(backend::Backend::Dual) => "dual",
                    _ => "host",
                },
            }
        }
        #[cfg(not(windows))]
        {
            "host"
        }
    };
    // Detect what actually came up from the initial URL: the WSL
    // proxy serves :18787, the Dual router :28787, host-native
    // :8787. A wsl/dual request landing on :8787 means we fell
    // back to host-native (the WSL .or_else branch above).
    let actual_backend = if matches!(desktop_mode, launch_mode::LaunchMode::Cloud) {
        "host"
    } else if started.initial_url.contains(":18787") {
        "wsl"
    } else if started.initial_url.contains(":28787") {
        "dual"
    } else {
        "host"
    };
    let fallback = matches!(requested_backend, "wsl" | "dual") && actual_backend == "host";
    let host_platform = if cfg!(windows) {
        "windows"
    } else if cfg!(target_os = "macos") {
        "macos"
    } else {
        "linux"
    };
    let launch_mode_value = match desktop_mode {
        launch_mode::LaunchMode::Local => "local",
        launch_mode::LaunchMode::Cloud => "cloud",
    };
    // `version` is the shell/installer version (tauri.conf.json), so the
    // web UI can tell an old installer apart from a packaging bug without
    // waiting on the async browser_pane_ping round-trip.
    let shell_version = app.package_info().version.to_string();
    let backend_script = format!(
        "window.__SOLU_BACKEND__ = {{ requested: \"{requested_backend}\", \
         actual: \"{actual_backend}\", fallback: {fallback}, \
         hostPlatform: \"{host_platform}\", launchMode: \"{launch_mode_value}\", \
         version: \"{shell_version}\" }};"
    );

    // Restore the main window's saved geometry (position, size,
    // maximized) — but only if it still lands on a live monitor, so an
    // unplugged display falls back to the default instead of stranding
    // the window off-screen.
    let monitors = app.available_monitors().unwrap_or_default();
    let restore = window_geometry::load()
        .filter(|g| window_geometry::visible_on_monitors(g, &monitors));
    // A maximized restore builds at the DEFAULT size so a later
    // restore-down isn't monitor-filling; the maximize() below
    // re-establishes the maximized state.
    let (init_w, init_h) = match restore {
        Some(g) if !g.maximized => (g.width as f64, g.height as f64),
        _ => (1280.0, 800.0),
    };
    let window_plan = startup_window_plan(
        restore.is_some(),
        restore.is_some_and(|geometry| geometry.maximized),
        started_by_autostart,
    );

    let mut main_builder = WebviewWindowBuilder::new(app, "main", url)
        .title("soluterminal")
        .inner_size(init_w, init_h)
        .min_inner_size(600.0, 400.0)
        .resizable(true)
        // Custom title bar lives inside the React app (Topbar.tsx)
        // and is themed to match the rest of the chrome. The OS
        // bar would be a glaring white strip. Frontend renders
        // its own min/max/close on Windows/Linux; macOS keeps
        // the native traffic lights via title_bar_style below.
        .decorations(false)
        // Dedicated Tauri-webview flag for the web code. Our
        // frontend is served from the daemon's HTTP server rather
        // than bundled Tauri assets, so Tauri's default
        // `window.__TAURI__` injection isn't always present. This
        // runs before any page script on every load, so
        // `window.__SOLU_TAURI__` is a reliable signal that the
        // web is inside the desktop app (vs. a browser tab).
        .initialization_script("window.__SOLU_TAURI__ = true;")
        // Backend signal (requested/actual/fallback) per the
        // SHARED CONTRACT. Separate script so it runs before any
        // page code, same as __SOLU_TAURI__.
        .initialization_script(&backend_script)
        // Per-window identity (multi-monitor). The main window is
        // the primary; secondary windows (workspace_*) inject their
        // own label in workspace_window.rs. The frontend keys
        // per-window storage off this so each window restores its
        // own workspace across reloads. Primary keeps the legacy
        // un-suffixed storage key (backward compat).
        .initialization_script(
            "window.__SOLU_WINDOW__ = { label: \"main\", role: \"primary\" };",
        )
        .devtools(cfg!(debug_assertions))
        // WebView2's native drag/drop handler swallows HTML5 DnD
        // events inside the page (e.g. sidebar workspace reorder).
        // Disabling the native handler restores in-page DnD.
        .disable_drag_drop_handler()
        .on_navigation(nav_guard::guard(app.clone()));

    // Build hidden, then show once the webview exists (see
    // StartupWindowPlan::start_hidden for the deadlock this avoids).
    // When restoring, set the PHYSICAL position before showing — the
    // builder's .position() is logical and would misplace across
    // mixed-DPI monitors (same pattern as workspace_window.rs).
    // Maximize AFTER show (maximizing a hidden window is unreliable).
    if window_plan.start_hidden {
        main_builder = main_builder.visible(false);
    }
    let main_window = main_builder.build()?;
    if let Some(g) = restore {
        let _ = main_window.set_position(tauri::PhysicalPosition::new(g.x, g.y));
    }
    if window_plan.show_after_build {
        let _ = main_window.show(); // always show, even if positioning errored
        if restore.is_some_and(|g| g.maximized) {
            let _ = main_window.maximize();
        }
        let _ = main_window.set_focus();
    }
    let maximize_on_first_show = std::sync::Arc::new(
        std::sync::atomic::AtomicBool::new(window_plan.maximize_on_first_show),
    );
    #[cfg(desktop)]
    app.manage(single_instance::MainWindowReveal(maximize_on_first_show.clone()));

    // Capture the composed URL + backend signal so the
    // `open_workspace_window` command can spawn additional full-app
    // windows that load identically (same daemon URL, token, and
    // backend) without re-running launch-mode resolution. Read-only
    // managed state — never mutated, so no Mutex. Multi-window is
    // gated to Local launch mode (Cloud needs a per-machine prefix +
    // JWT we don't compose for MVP).
    app.manage(workspace_window::WorkspaceSpawnTemplate {
        url_string: url_string.clone(),
        backend_script: backend_script.clone(),
        supported: matches!(desktop_mode, launch_mode::LaunchMode::Local),
    });

    // Tray icon + right-click menu. Gives the user a way to
    // re-open the window after closing it and a real "Quit" that
    // actually exits (vs. hiding).
    //
    // Public beta: no "Pair with cloud…" item (phone access/pairing
    // is team-only and lives in the web UI, which gates it on the
    // daemon's entitlements) and no "Switch to connecting to
    // computers you set up" in Local mode. A Cloud-mode install
    // still gets the way back to running projects on this PC.
    let show = MenuItem::with_id(app, "show", "Show soluterminal", true, None::<&str>)?;
    let hide = MenuItem::with_id(app, "hide", "Hide window", true, None::<&str>)?;
    let switch_to_local = MenuItem::with_id(
        app,
        "switch_launch_mode",
        format!(
            "Switch to running projects on this computer ({})",
            launch_mode::label(launch_mode::LaunchMode::Local)
        ),
        true,
        None::<&str>,
    )?;

    // "Switch backend" tray entry (Windows only). Label shows
    // what you'd switch TO — "Switch to Windows-native" if
    // currently on WSL, and vice versa. Clicking saves the new
    // preference and restarts the app so the new backend comes
    // up cleanly (rather than trying to swap daemons live,
    // which would orphan the webview's state).
    #[cfg(windows)]
    let switch = {
        let target = backend::other(chosen.unwrap_or(backend::Backend::HostNative));
        let label = format!("Switch to {}", backend::label(target));
        MenuItem::with_id(app, "switch_backend", &label, true, None::<&str>)?
    };

    // "Repair setup" (Windows, Local only): clears the saved
    // backend choice + transient WSL artifacts and restarts so
    // detection + the first-run wizard re-fire. The in-app escape
    // hatch for a botched WSL setup that a reinstall wouldn't fix
    // (prefs live in %LOCALAPPDATA%, untouched by the uninstaller).
    #[cfg(windows)]
    let repair =
        MenuItem::with_id(app, "repair_reset", "Repair setup (re-detect Linux/Windows)", true, None::<&str>)?;

    let sep = PredefinedMenuItem::separator(app)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;

    #[cfg(windows)]
    let menu = if matches!(desktop_mode, launch_mode::LaunchMode::Local) {
        Menu::with_items(app, &[&show, &hide, &switch, &repair, &sep, &quit])?
    } else {
        Menu::with_items(app, &[&show, &hide, &switch_to_local, &sep, &quit])?
    };
    #[cfg(not(windows))]
    let menu = if matches!(desktop_mode, launch_mode::LaunchMode::Local) {
        Menu::with_items(app, &[&show, &hide, &sep, &quit])?
    } else {
        Menu::with_items(app, &[&show, &hide, &switch_to_local, &sep, &quit])?
    };

    let maximize_from_menu = maximize_on_first_show.clone();
    let maximize_from_tray = maximize_on_first_show.clone();
    let _tray = TrayIconBuilder::with_id("main")
        .tooltip("soluterminal")
        .icon(
            tray_icon::image()
                .unwrap_or_else(|| app.default_window_icon().unwrap().clone()),
        )
        .menu(&menu)
        .show_menu_on_left_click(false) // left-click toggles, right-click opens menu
        .on_menu_event(move |app, event| match event.id.as_ref() {
            "show" => {
                show_main_window(app, &maximize_from_menu);
            }
            "hide" => {
                if let Some(w) = app.get_webview_window("main") {
                    dispatch_desktop_will_background_event(&w, "tray-hide");
                    let _ = w.hide();
                }
            }
            "switch_launch_mode" => {
                // Only offered in Cloud mode now, and always back to
                // Local: never flip a Local install into Cloud.
                if let Err(e) = launch_mode::set_pref(launch_mode::LaunchMode::Local) {
                    eprintln!("desktop: set launch mode failed: {e}");
                }
                restart_from_main_thread(app);
            }
            #[cfg(windows)]
            "switch_backend" => {
                // Flip to the opposite of the currently-SAVED
                // backend, persist, and restart. We read the saved
                // pref (not resolve_backend) so this never triggers
                // the WSL re-validation/repair dialog — switching is
                // an explicit user action, not a health check.
                // Restart cleanly stops the current daemon/proxy via
                // SidecarGuard drop and re-runs .setup().
                let target = backend::other(backend::saved_or_default());
                if let Err(e) = backend::set_pref(target) {
                    eprintln!("desktop: set_pref failed: {e}");
                }
                restart_from_main_thread(app);
            }
            #[cfg(windows)]
            "repair_reset" => {
                use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind};
                let ok = app
                    .dialog()
                    .message(
                        "Reset soluterminal's Windows/Linux setup and re-detect from \
                         scratch? The app will restart and ask how you want it to run \
                         (and offer to set up Linux/WSL if needed).",
                    )
                    .title("Repair setup")
                    .kind(MessageDialogKind::Warning)
                    .buttons(MessageDialogButtons::OkCancel)
                    .blocking_show();
                if ok {
                    backend::reset_setup();
                    restart_from_main_thread(app);
                }
            }
            "quit" => {
                // Persist main-window geometry before a hard exit —
                // the blur/close flushes don't fire on app.exit(0).
                persist_main_window_geometry(app);
                app.exit(0)
            }
            _ => {}
        })
        .on_tray_icon_event(move |tray, event| {
            // Left-click on the icon = always bring main forward.
            // Previously this toggled visibility, but that broke
            // the common "close app, re-open from tray" flow
            // when the main window was already visible behind
            // something else (e.g. a browser popout took focus):
            // is_visible() returned true, tray-click would then
            // HIDE main, and the user saw nothing. Toggle via
            // tray also fought with native child webviews that
            // refuse to unhide cleanly in some Windows DPI /
            // compositor states. Hide is still available via
            // the tray context menu's "Hide window" item.
            // Only act on the Up edge so a click isn't double-
            // handled across press and release.
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                let app = tray.app_handle();
                show_main_window(app, &maximize_from_tray);
            }
        })
        .build(app)?;

    // First-launch: NO auto-fired native pair dialog anymore.
    //
    // Pairing now lives inside the embedded web UI: a dismissible
    // "Set up phone access" banner driven by live cloud status
    // (POST /api/cloud/pair/start + GET /api/cloud/status). First
    // launch should open straight to a working local terminal, not
    // a modal wall. The old 800ms auto-dialog stacked an OS modal
    // over a half-painted webview and dead-ended on dismiss
    // (confusion points #12 / #13 in PLAN-onboarding-ux-2026-06-24).
    //
    // The native pair wizard (pair::run_pair_wizard) is KEPT as a
    // fallback but is no longer auto-invoked, and the tray no longer
    // carries a pairing item (public beta: pairing is team-only and
    // lives in the web UI). `pair::any_claim_exists` is likewise no longer
    // called here; both remain public helpers (see #[allow(dead_code)]
    // notes in pair.rs) so the native flow can be re-wired if needed.
    Ok(())
}

/// `app.restart()` from a tray handler (main thread). Frees the
/// single-instance lock first so the relaunched shell isn't mistaken for a
/// second instance; see single_instance::release_before_restart.
fn restart_from_main_thread(app: &tauri::AppHandle) -> ! {
    #[cfg(desktop)]
    single_instance::release_before_restart(app);
    app.restart()
}

/// WSL answered again after this launch fell back to Windows-only. A restart
/// re-runs Dual startup, which now attaches to it (even if it is older).
#[cfg(windows)]
fn offer_wsl_reconnect(app: &tauri::AppHandle) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogKind};
    let restart_handle = app.clone();
    app.dialog()
        .message(
            "Linux (WSL) is running again. Restart SoluTerminal to bring your WSL panes back.",
        )
        .title("WSL is back")
        .kind(MessageDialogKind::Info)
        .buttons(MessageDialogButtons::OkCancelCustom(
            "Restart now".into(),
            "Later".into(),
        ))
        .show(move |restart| {
            if restart {
                persist_main_window_geometry(&restart_handle);
                let app = restart_handle.clone();
                let _ = restart_handle.run_on_main_thread(move || {
                    restart_from_main_thread(&app);
                });
            }
        });
}

fn persist_main_window_geometry(app: &tauri::AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        if let Some(g) = window_geometry::capture(&w) {
            window_geometry::save(&g);
        }
    }
}

/// Flush geometry and leave cleanly once the out-of-process silent-update
/// courier has proved it is running. The courier performs the relaunch.
#[tauri::command]
async fn exit_for_update(app: tauri::AppHandle) -> Result<(), String> {
    let main_thread_app = app.clone();
    app.run_on_main_thread(move || {
        persist_main_window_geometry(&main_thread_app);
        main_thread_app.exit(0);
    })
    .map_err(|e| format!("run_on_main_thread failed: {e}"))
}

#[tauri::command]
async fn desktop_set_backend_mode(app: tauri::AppHandle, mode: String) -> Result<(), String> {
    #[cfg(windows)]
    {
        let backend = match mode.as_str() {
            "wsl" => backend::Backend::Wsl,
            "host" | "win" | "windows" => backend::Backend::HostNative,
            "dual" => backend::Backend::Dual,
            _ => return Err(format!("unknown backend mode: {mode}")),
        };
        backend::set_pref(backend).map_err(|e| format!("set backend mode: {e}"))?;
        app.restart()
    }
    #[cfg(not(windows))]
    {
        let _ = app;
        let _ = mode;
        Err("backend mode switching is only available on Windows".to_string())
    }
}

/// In-app "Set up Linux (WSL)" entry point for the setup checklist and
/// settings. First launch no longer prompts for WSL, so this is where a
/// Windows PowerShell user is offered it later. It shows the same guided
/// confirm / install / verify dialogs as tray "Repair setup", then returns
/// "switching" (WSL works; the app restarts into it — when WSL was already
/// installed this happens with no dialog, so the caller should confirm
/// first because a restart closes open panes), "restart-pc", "declined",
/// "failed" (the user saw why), "low-disk", or "busy" (already running).
/// Older shells reject the invoke; callers point to the tray instead.
#[tauri::command]
async fn desktop_set_up_wsl(app: tauri::AppHandle) -> Result<String, String> {
    #[cfg(windows)]
    {
        use std::sync::atomic::{AtomicBool, Ordering};
        static RUNNING: AtomicBool = AtomicBool::new(false);
        if RUNNING.swap(true, Ordering::AcqRel) {
            return Ok("busy".into());
        }
        let worker = app.clone();
        let result = tauri::async_runtime::spawn_blocking(move || {
            backend::set_up_wsl_on_request(&worker)
        })
        .await;
        RUNNING.store(false, Ordering::Release);
        let outcome = match result.map_err(|e| format!("WSL setup failed: {e}"))? {
            backend::WslSetupRequest::SwitchNow => {
                persist_main_window_geometry(&app);
                let main = app.clone();
                app.run_on_main_thread(move || {
                    restart_from_main_thread(&main);
                })
                .map_err(|e| format!("run_on_main_thread failed: {e}"))?;
                "switching"
            }
            backend::WslSetupRequest::RestartPcToFinish => "restart-pc",
            backend::WslSetupRequest::Declined => "declined",
            backend::WslSetupRequest::Failed => "failed",
            backend::WslSetupRequest::LowDisk => "low-disk",
        };
        Ok(outcome.into())
    }
    #[cfg(not(windows))]
    {
        let _ = app;
        Err("Linux (WSL) setup is only available on Windows".to_string())
    }
}

/// Switch the desktop between Local daemon and Cloud account hosting and
/// restart. Same behaviour as the tray "Switch to …" item, exposed as a
/// command so the sidebar's connection switcher can drive it. Web callers
/// feature-detect: on shells that predate this command the invoke rejects
/// and the UI points at the tray menu instead.
#[tauri::command]
async fn desktop_set_launch_mode(app: tauri::AppHandle, mode: String) -> Result<(), String> {
    let target = match mode.as_str() {
        "local" | "daemon" => launch_mode::LaunchMode::Local,
        "cloud" | "account" => launch_mode::LaunchMode::Cloud,
        _ => return Err(format!("unknown launch mode: {mode}")),
    };
    launch_mode::check_available(target)?;
    launch_mode::set_pref(target).map_err(|e| format!("set launch mode: {e}"))?;
    app.restart()
}

fn dispatch_desktop_restored_event(window: &tauri::Window, reason: &str) {
    let Some(webview) = window.app_handle().get_webview_window("main") else {
        return;
    };

    // hideTracked: this shell announces its own hides (tray Hide, close to
    // tray) with solu:desktop-will-background, so the page may treat a bare
    // focus return as focus only. Older shells omit it.
    let detail = serde_json::json!({ "reason": reason, "hideTracked": true });
    let script = format!(
        "window.dispatchEvent(new CustomEvent('solu:desktop-restored', {{ detail: {detail} }}));"
    );

    if let Err(err) = webview.eval(script) {
        eprintln!("desktop: dispatch solu:desktop-restored failed: {err}");
    }
}

// Native hides bypass the page's titlebar buttons, so announce them the same
// way: the page invalidates in-flight gestures across a real hide-and-return.
fn dispatch_desktop_will_background_event(webview: &tauri::WebviewWindow, reason: &str) {
    let detail = serde_json::json!({ "reason": reason });
    let script = format!(
        "window.dispatchEvent(new CustomEvent('solu:desktop-will-background', {{ detail: {detail} }}));"
    );
    if let Err(err) = webview.eval(script) {
        eprintln!("desktop: dispatch solu:desktop-will-background failed: {err}");
    }
}

/// Windows-only: wipe the WebView2 cache directory if the app version
/// changed since last launch. Protects against stale service workers,
/// bad cached bundles, or any other WebView2 storage that got into a
/// broken state under a previous version. Safe because we re-inject
/// the daemon's session token on every launch via ?token= URL.
///
/// Stores the "last seen version" at %LOCALAPPDATA%\soluterminal\webview-version
/// next to the existing launch.log.
#[cfg(windows)]
fn reset_webview_cache_on_version_change(current_version: &str) {
    use std::fs;
    use std::path::PathBuf;

    let Some(local) = std::env::var_os("LOCALAPPDATA").map(PathBuf::from) else {
        return;
    };
    let version_file = local.join("soluterminal").join("webview-version");

    let last_version = fs::read_to_string(&version_file).ok();
    let needs_reset = last_version.as_deref().map(str::trim) != Some(current_version);
    if !needs_reset {
        return;
    }

    // Tauri 2 places WebView2 data under LOCALAPPDATA keyed by the
    // Tauri identifier (`app.soluterminal.desktop` per tauri.conf.json).
    // The EBWebView subdir holds profiles, cache, service workers, etc.
    //
    // Previous implementation removed the entire EBWebView directory,
    // which wiped localStorage, IndexedDB, cookies, and Preferences
    // along with the cache — meaning every version bump reset the
    // user's voice-alert mode, Whisper language, terminal theme, and
    // notification permission. Only the HTTP cache and the service
    // worker are actually problematic across upgrades (the incident
    // that motivated this reset was a buggy sw.js locking clients
    // out); everything else should survive.
    //
    // Surgical wipe: for each profile directory under EBWebView/,
    // remove only Cache / Code Cache / Service Worker. Keep Local
    // Storage / IndexedDB / Cookies / Preferences intact.
    let webview_dir = local.join("app.soluterminal.desktop").join("EBWebView");
    if webview_dir.exists() {
        daemon::launch_log(&format!(
            "webview cache reset: {:?} -> {} (surgical wipe in {})",
            last_version.as_deref().map(str::trim).unwrap_or("<none>"),
            current_version,
            webview_dir.display()
        ));
        // Subfolders to wipe on version change. The trailing spaces in
        // "Service Worker" and "Code Cache" are part of the literal
        // directory names as WebView2 writes them.
        const WIPE: &[&str] = &["Cache", "Code Cache", "Service Worker"];
        // Any direct child of EBWebView that contains a profile — in
        // practice this is just "Default", but older WebView2 installs
        // occasionally create "Profile 1" and similar. Iterate so we
        // catch them all.
        if let Ok(entries) = fs::read_dir(&webview_dir) {
            for entry in entries.flatten() {
                let profile = entry.path();
                if !profile.is_dir() {
                    continue;
                }
                for name in WIPE {
                    let target = profile.join(name);
                    if target.exists() {
                        let _ = fs::remove_dir_all(&target);
                    }
                }
            }
        }
    }

    // Record the version even if there was nothing to delete, so the
    // next launch with the same version skips the reset.
    if let Some(parent) = version_file.parent() {
        let _ = fs::create_dir_all(parent);
    }
    let _ = fs::write(&version_file, current_version);
}

#[cfg(test)]
mod autostart_window_tests {
    use super::startup_window_plan;

    #[test]
    fn autostart_preserves_maximized_restore_for_first_tray_show() {
        let plan = startup_window_plan(true, true, true);
        assert!(plan.start_hidden);
        assert!(plan.maximize_on_first_show);

        let normal = startup_window_plan(true, true, false);
        assert!(normal.start_hidden);
        assert!(!normal.maximize_on_first_show);

        assert!(startup_window_plan(false, false, true).start_hidden);
        assert!(startup_window_plan(true, false, false).start_hidden);
        let restored_but_not_maximized = startup_window_plan(true, false, true);
        assert!(restored_but_not_maximized.start_hidden);
        assert!(!restored_but_not_maximized.maximize_on_first_show);
    }

    // A visible window during WebView2 creation can be focused or minimized
    // by the user, which deadlocked the main thread. Every launch builds the
    // window hidden; only an interactive launch shows it afterwards.
    #[test]
    fn main_window_is_built_hidden_and_shown_after_build_unless_autostart() {
        for has_restore in [false, true] {
            for saved_maximized in [false, true] {
                let interactive = startup_window_plan(has_restore, saved_maximized, false);
                assert!(interactive.start_hidden);
                assert!(interactive.show_after_build);
                assert!(!interactive.maximize_on_first_show);

                let autostart = startup_window_plan(has_restore, saved_maximized, true);
                assert!(autostart.start_hidden);
                assert!(!autostart.show_after_build);
            }
        }
    }
}

/// Classify the error string returned by daemon::ensure_daemon_running_wsl
/// into cause-specific (title, body) fallback copy. The WSL backend failed
/// and we're about to run host-native instead; generic "fix WSL" advice is
/// actively wrong for some causes (an incomplete download needs a
/// re-install, not `wsl --shutdown`; a held port needs the other daemon
/// quit). Substrings match the error sites in daemon.rs. Windows-only:
/// the WSL backend (and thus its fallback) only exists there.
#[cfg(windows)]
fn classify_wsl_fallback(err: &str) -> (&'static str, String) {
    if err.contains("host disk critically low") {
        return (
            "Windows drive is nearly full",
            format!("soluterminal could not verify a reachable Linux (WSL) backend. Starting or restarting it is paused because Windows drive space is below the configured floor ({err}). Free up space, then reopen soluterminal.\n\nYou are viewing the Windows workspace. Your WSL panes have not been deleted."),
        );
    }
    // Incomplete install: the bundled Linux solud is missing or empty.
    // `ensure_daemon_running_wsl` emits "bundled Linux solud not found …"
    // / "bundled Linux solud at … is empty". This is NOT a WSL problem.
    if err.contains("bundled Linux solud") {
        return (
            "soluterminal is missing some files",
            "Your install is incomplete — re-download the latest installer \
             from soluterminal and run it again to repair soluterminal.\n\n\
             For now it's running directly on Windows so you can keep working."
                .to_string(),
        );
    }
    // WSL networking: couldn't resolve the WSL VM IP, or the daemon never
    // became reachable on a Windows-routable address within the timeout.
    if err.contains("resolve WSL IP") || err.contains("not reachable within") {
        return (
            "Couldn't reach the Linux (WSL) backend",
            format!(
                "soluterminal couldn't reach Windows' Linux (WSL) over the network, \
                 so it's running directly on Windows for now — panes open in \
                 PowerShell and WSL-only features are unavailable.\n\n\
                 Try `wsl --shutdown` in a terminal then reopen soluterminal, or use \
                 \"Repair setup\" in the tray menu.\n\n\
                 Reason: {err}"
            ),
        );
    }
    // The WSL port is held by a SoluTerminal daemon start-wsl.sh will not
    // replace — typically an older one in another WSL distro, which shares
    // the network. The message names the distro and a command to stop it.
    if let Some(detail) = err.strip_prefix("WSL port held: ") {
        return (
            "WSL didn't start: port 8787 is in use",
            format!(
                "{detail}\n\n\
                 SoluTerminal is using Windows only for now. Once the port is free, the WSL \
                 background service starts by itself; restart SoluTerminal, or accept its \
                 offer to reconnect, to get your WSL panes back."
            ),
        );
    }
    // Port already held: an identity check found a different backend (e.g.
    // a Windows-native daemon) squatting the port. The reuse/probe paths
    // surface this as a backend mismatch ("backend \"host\"", …).
    if err.contains("backend \"host\"") || err.contains("port held") {
        return (
            "Port 8787 is already in use",
            "Another soluterminal daemon is already using port 8787 — quit it \
             (or use the tray to switch backend) and reopen soluterminal.\n\n\
             For now it's running directly on Windows so you can keep working."
                .to_string(),
        );
    }
    // Generic fallback — unknown cause; keep the original WSL advice.
    (
        "Running on Windows directly (WSL unavailable)",
        format!(
            "Couldn't start the Linux (WSL) backend, so soluterminal is running \
             directly on Windows for now — panes open in PowerShell and WSL-only \
             features are unavailable.\n\n\
             Reason: {err}\n\n\
             Fix WSL (try `wsl --shutdown` then reopen, or `wsl --install` if it's \
             missing) and relaunch. You can also switch backends from the tray menu."
        ),
    )
}

/// Minimal percent-encoder for values destined for URL query strings.
/// We only need to escape the characters that would break parsing of
/// `?foo=bar&solu_alt=<value>` — primarily `&`, `=`, `?`, `#`, and
/// whitespace. Produced URLs already have the "ascii-only localhost"
/// shape so we don't need full RFC 3986 handling.
fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' | b':' | b'/' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{:02X}", b)),
        }
    }
    out
}
