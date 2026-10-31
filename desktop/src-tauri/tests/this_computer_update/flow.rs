// Tests for this_computer_update.rs. A throwaway in-process HTTP server stands
// in for this computer's daemon, so no real daemon, data dir or HOME is used.

use super::*;
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

/// Canned answers, by request line ("GET /api/update/status").
#[derive(Clone)]
struct Answers {
    status: &'static str,
    check: Option<&'static str>,
    apply: (u16, &'static str),
}

const UPDATE_READY: &str = r#"{"shellCurrent":"0.0.1225","shellLatest":"0.0.1229","shellAvailable":true,"shellApplying":false,"updateChannel":"team","lastSuccessMs":1}"#;
const UP_TO_DATE: &str = r#"{"shellCurrent":"0.0.1229","shellLatest":"0.0.1229","shellAvailable":false,"shellApplying":false,"updateChannel":"team","lastSuccessMs":1}"#;
const LAUNCHED: (u16, &str) = (200, r#"{"applying":true,"installerPath":"C:\\x\\setup.exe","silent":false}"#);

struct StubDaemon {
    addr: String,
    requests: Arc<Mutex<Vec<String>>>,
    stopped: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl StubDaemon {
    fn start(addr: &str, backend_id: &'static str, answers: Answers) -> Self {
        Self::start_inner(addr, backend_id, answers, None)
    }

    /// Answers 401 to any request without `Bearer <token>`.
    fn start_requiring_token(addr: &str, backend_id: &'static str, answers: Answers, token: &'static str) -> Self {
        Self::start_inner(addr, backend_id, answers, Some(token))
    }

    /// Reports `updateOnly` with `updaterReady: false` for the first
    /// `not_ready` health probes, then ready.
    fn start_with_not_ready(addr: &str, backend_id: &'static str, answers: Answers, not_ready: usize) -> Self {
        Self::start_full(addr, backend_id, answers, None, Some(not_ready))
    }

    fn start_inner(addr: &str, backend_id: &'static str, answers: Answers, required: Option<&'static str>) -> Self {
        Self::start_full(addr, backend_id, answers, required, None)
    }

    fn start_full(
        addr: &str,
        backend_id: &'static str,
        answers: Answers,
        required: Option<&'static str>,
        not_ready: Option<usize>,
    ) -> Self {
        let mut health_polls = 0usize;
        let listener = TcpListener::bind(addr).unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let stopped = Arc::new(AtomicBool::new(false));
        let (log, stop) = (requests.clone(), stopped.clone());
        let thread = std::thread::spawn(move || {
            for socket in listener.incoming() {
                if stop.load(Ordering::SeqCst) {
                    return;
                }
                let Ok(mut socket) = socket else { continue };
                let mut reader = BufReader::new(socket.try_clone().unwrap());
                let mut head = String::new();
                loop {
                    let mut line = String::new();
                    if reader.read_line(&mut line).unwrap_or(0) == 0 || line == "\r\n" {
                        break;
                    }
                    head.push_str(&line);
                }
                let first = head.lines().next().unwrap_or("").to_string();
                let request = first.rsplit_once(' ').map(|(r, _)| r.to_string()).unwrap_or_default();
                let auth = head
                    .lines()
                    .find(|l| l.to_lowercase().starts_with("authorization:"))
                    .map(|l| l.splitn(2, ':').nth(1).unwrap().trim().to_string())
                    .unwrap_or_default();
                log.lock().unwrap().push(format!("{request} [{auth}]"));
                let authorized = required.is_none_or(|t| auth == format!("Bearer {t}"));
                let (code, body): (u16, String) = if !authorized {
                    (401, "unauthorized".to_string())
                } else { match request.as_str() {
                    "GET /api/health?identity=1" => match not_ready {
                        None => (200, format!(r#"{{"backendID":"{backend_id}"}}"#)),
                        Some(n) => {
                            health_polls += 1;
                            let ready = health_polls > n;
                            (200, format!(r#"{{"backendID":"{backend_id}","updateOnly":true,"updaterReady":{ready}}}"#))
                        }
                    },
                    "GET /api/update/status" => (200, answers.status.to_string()),
                    "POST /api/update/check" => match answers.check {
                        Some(body) => (200, body.to_string()),
                        None => (503, "updater disabled".to_string()),
                    },
                    "POST /api/update/apply-shell" => (answers.apply.0, answers.apply.1.to_string()),
                    _ => (404, "not found".to_string()),
                } };
                let _ = socket.write_all(
                    format!(
                        "HTTP/1.1 {code} X\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                        body.len()
                    )
                    .as_bytes(),
                );
            }
        });
        Self { addr, requests, stopped, thread: Some(thread) }
    }

    fn stop(&mut self) {
        self.stopped.store(true, Ordering::SeqCst);
        let _ = TcpStream::connect(&self.addr);
        if let Some(thread) = self.thread.take() {
            thread.join().unwrap();
        }
    }

    fn requests(&self) -> Vec<String> {
        self.requests.lock().unwrap().clone()
    }
}

impl Drop for StubDaemon {
    fn drop(&mut self) {
        self.stop();
    }
}

const QUICK: Timeouts = Timeouts {
    ready: Duration::from_secs(5),
    check: Duration::from_secs(5),
    apply: Duration::from_secs(5),
};

fn no_progress(_: &str) {}

/// A start that brings a stub daemon up on the address the flow picks, and
/// whose stop the test can observe.
fn spawning(
    answers: Answers,
    slot: Arc<Mutex<Option<StubDaemon>>>,
    stops: Arc<Mutex<u32>>,
) -> impl FnOnce(&str) -> Result<(String, StopFn), String> {
    move |addr: &str| {
        let stub = StubDaemon::start(addr, "host", answers);
        let url = format!("http://{}", stub.addr);
        *slot.lock().unwrap() = Some(stub);
        let stop_slot = slot.clone();
        Ok((
            url,
            Box::new(move || {
                *stops.lock().unwrap() += 1;
                if let Some(stub) = stop_slot.lock().unwrap().as_mut() {
                    stub.stop();
                }
                Ok(())
            }) as StopFn,
        ))
    }
}

// An HTTP answer, not just a connect: under WSL's localhost forwarding a
// connect to a closed port can succeed and then be reset.
fn answers_http(addr: &str) -> bool {
    let endpoint = DaemonEndpoint { origin: format!("http://{addr}"), token: None };
    call(&endpoint, false, "/api/update/status", Duration::from_secs(2)).is_ok()
}

fn closed_addr() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    drop(listener);
    addr
}

#[test]
fn only_the_main_window_may_update_this_computer() {
    assert!(require_main_webview("main").is_ok());
    for label in ["workspace_2", "browser_1", "bpane_main", "directbrowser_1", ""] {
        assert!(require_main_webview(label).is_err(), "{label}");
    }
}

#[test]
fn the_endpoint_is_loopback_only_and_keeps_the_token_native() {
    let ep = DaemonEndpoint::from_url("http://127.0.0.1:18787/?token=abc", Some("other".into())).unwrap();
    assert_eq!(ep, DaemonEndpoint { origin: "http://127.0.0.1:18787".into(), token: Some("abc".into()) });
    let ep = DaemonEndpoint::from_url("http://127.0.0.1:41234", Some("tok".into())).unwrap();
    assert_eq!(ep.token.as_deref(), Some("tok"));
    for remote in [
        "https://soluterminal-cloud.onrender.com/",
        "http://10.0.0.5:8787",
        "http://example.com:8787",
        "https://127.0.0.1:8787",
    ] {
        assert!(DaemonEndpoint::from_url(remote, None).is_none(), "{remote}");
    }
}

// Starts a daemon when none answers, drives check + apply with its token, and
// stops it again because this flow started it.
#[test]
fn a_started_daemon_installs_the_update_and_is_stopped_afterwards() {
    let answers = Answers { status: UPDATE_READY, check: Some(UPDATE_READY), apply: LAUNCHED };
    let slot = Arc::new(Mutex::new(None));
    let stops = Arc::new(Mutex::new(0));
    let phases = Mutex::new(Vec::new());
    let unused = closed_addr();
    let outcome = run_update(
        || true,
        || (),
        || {
            start_host_daemon(
                &[(unused.as_str(), "host")],
                crate::daemon::daemon_answers_as,
                spawning(answers, slot.clone(), stops.clone()),
                || Some("secret".into()),
            )
        },
        &|phase: &str| phases.lock().unwrap().push(phase.to_string()),
        &QUICK,
    );
    assert_eq!(outcome.state, "launched", "{outcome:?}");
    assert_eq!(outcome.installer_version.as_deref(), Some("0.0.1229"));
    assert_eq!(outcome.current_version.as_deref(), Some("0.0.1225"));
    assert_eq!(*phases.lock().unwrap(), [PHASE_STARTING, PHASE_CHECKING, PHASE_INSTALLING]);
    assert_eq!(*stops.lock().unwrap(), 1, "the daemon this flow started is stopped");

    let guard = slot.lock().unwrap();
    let stub = guard.as_ref().unwrap();
    let requests = stub.requests();
    assert_eq!(
        requests
            .iter()
            .filter(|r| !r.starts_with("GET /api/update/status") && !r.starts_with("GET /api/health"))
            .cloned()
            .collect::<Vec<_>>(),
        ["POST /api/update/check [Bearer secret]", "POST /api/update/apply-shell [Bearer secret]"],
        "{requests:?}"
    );
    // Attended, never silent or forced: the exact query-less request.
    assert!(requests.iter().all(|r| !r.contains('?') || r.starts_with("GET /api/health")), "{requests:?}");
    assert!(!answers_http(&stub.addr), "the stub daemon is down");
}

// A daemon already running (Dual or Local-mode leftovers) is used and left
// running; no second daemon starts.
#[test]
fn a_running_daemon_is_reused_and_left_running() {
    let answers = Answers { status: UPDATE_READY, check: Some(UPDATE_READY), apply: LAUNCHED };
    let running = StubDaemon::start("127.0.0.1:0", "win", answers.clone());
    let unused = closed_addr();
    let mut spawned = false;
    let outcome = run_update(
        || true,
        || (),
        || {
            start_host_daemon(
                &[(unused.as_str(), "host"), (running.addr.as_str(), "win")],
                crate::daemon::daemon_answers_as,
                |_addr: &str| {
                    spawned = true;
                    Err("must not start a second daemon".to_string())
                },
                || None,
            )
        },
        &no_progress,
        &QUICK,
    );
    assert!(!spawned);
    assert_eq!(outcome.state, "launched", "{outcome:?}");
    assert!(running.requests().contains(&"POST /api/update/apply-shell []".to_string()));
    assert!(answers_http(&running.addr), "a reused daemon keeps running");
}

// A daemon that requires the token even on loopback (SOLU_LOOPBACK_REQUIRE_AUTH)
// is found with the token, not mistaken for a stranger.
#[test]
fn a_running_daemon_that_requires_the_token_is_reused() {
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: LAUNCHED };
    let running = StubDaemon::start_requiring_token("127.0.0.1:0", "host", answers, "secret");
    let daemon = start_host_daemon(
        &[(running.addr.as_str(), "host")],
        crate::daemon::daemon_answers_as,
        |_addr: &str| Err("must not start a second daemon".to_string()),
        || Some("secret".into()),
    )
    .unwrap();
    assert!(daemon.stop.is_none());
    assert_eq!(daemon.endpoint.token.as_deref(), Some("secret"));
    assert!(running.requests().contains(&"GET /api/health?identity=1 [Bearer secret]".to_string()));
}

// A daemon of another backend on a known port (WSL's forwarding on 8787) is
// not taken for this computer's Windows daemon.
#[test]
fn a_daemon_of_another_backend_is_not_reused() {
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: LAUNCHED };
    let wsl = StubDaemon::start("127.0.0.1:0", "wsl", answers.clone());
    let slot = Arc::new(Mutex::new(None));
    let stops = Arc::new(Mutex::new(0));
    let daemon = start_host_daemon(
        &[(wsl.addr.as_str(), "host")],
        crate::daemon::daemon_answers_as,
        spawning(answers, slot.clone(), stops.clone()),
        || None,
    )
    .unwrap();
    assert_ne!(daemon.endpoint.origin, format!("http://{}", wsl.addr));
    assert!(daemon.stop.is_some());
    daemon.finish().unwrap();
    assert_eq!(*stops.lock().unwrap(), 1);
}

#[test]
fn no_update_means_nothing_is_installed_and_the_daemon_stops() {
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: (500, "must not be called") };
    let slot = Arc::new(Mutex::new(None));
    let stops = Arc::new(Mutex::new(0));
    let unused = closed_addr();
    let outcome = run_update(
        || true,
        || (),
        || start_host_daemon(&[(unused.as_str(), "host")], |_, _, _| false, spawning(answers, slot.clone(), stops.clone()), || None),
        &no_progress,
        &QUICK,
    );
    assert_eq!(outcome.state, "up-to-date", "{outcome:?}");
    assert_eq!(outcome.current_version.as_deref(), Some("0.0.1229"));
    assert_eq!(*stops.lock().unwrap(), 1);
    let guard = slot.lock().unwrap();
    assert!(!guard.as_ref().unwrap().requests().iter().any(|r| r.contains("apply-shell")));
}

fn outcome_for(answers: Answers) -> ThisComputerUpdate {
    let running = StubDaemon::start("127.0.0.1:0", "host", answers);
    let addr = running.addr.clone();
    run_update(
        || true,
        || (),
        || start_host_daemon(&[(addr.as_str(), "host")], |_, _, _| true, |_: &str| Err("unused".into()), || None),
        &no_progress,
        &QUICK,
    )
}

#[test]
fn daemon_refusals_and_failures_come_back_as_messages() {
    let downgrade = outcome_for(Answers {
        status: UPDATE_READY,
        check: Some(UPDATE_READY),
        apply: (409, r#"{"code":"shell_daemon_downgrade_refused","message":"refusing"}"#),
    });
    assert_eq!((downgrade.state, downgrade.code.as_deref()), ("refused", Some("shell_daemon_downgrade_refused")));
    assert!(downgrade.message.unwrap().contains("nothing was installed"));

    let held = outcome_for(Answers {
        status: UPDATE_READY,
        check: Some(UPDATE_READY),
        apply: (409, r#"{"code":"update_held","heldVersion":"0.0.1229"}"#),
    });
    assert_eq!(held.state, "refused");

    let broken = outcome_for(Answers { status: UPDATE_READY, check: Some(UPDATE_READY), apply: (500, "download installer: sha256 mismatch") });
    assert_eq!(broken.state, "failed");
    assert!(broken.message.unwrap().contains("sha256 mismatch"));

    let busy = outcome_for(Answers {
        status: r#"{"shellAvailable":true,"shellApplying":true,"lastSuccessMs":1}"#,
        check: None,
        apply: (500, "must not be called"),
    });
    assert_eq!(busy.state, "busy");

    // A check that never succeeded is not "up to date".
    let unchecked = outcome_for(Answers { status: r#"{"shellAvailable":false}"#, check: None, apply: (500, "x") });
    assert_eq!(unchecked.state, "failed", "{unchecked:?}");
    let erred = outcome_for(Answers {
        status: r#"{"shellAvailable":false,"lastError":"fetch manifest: 503","lastSuccessMs":1}"#,
        check: None,
        apply: (500, "x"),
    });
    assert!(erred.message.unwrap().contains("fetch manifest: 503"));
}

#[test]
fn a_daemon_that_never_answers_fails_and_is_stopped() {
    let stops = Arc::new(Mutex::new(0));
    let counted = stops.clone();
    let dead = closed_addr();
    let outcome = run_update(
        || true,
        || (),
        || {
            start_host_daemon(&[], |_, _, _| false, move |_: &str| {
                Ok((format!("http://{dead}"), Box::new(move || {
                    *counted.lock().unwrap() += 1;
                    Ok(())
                }) as StopFn))
            }, || None)
        },
        &no_progress,
        &Timeouts { ready: Duration::from_millis(600), ..QUICK },
    );
    assert_eq!(outcome.state, "failed");
    assert!(outcome.message.unwrap().contains("nothing was installed"));
    assert_eq!(*stops.lock().unwrap(), 1);

    let failed_start = run_update(|| true, || (), || Err("solud binary not found".into()), &no_progress, &QUICK);
    assert_eq!(failed_start.state, "failed");
    assert!(failed_start.message.unwrap().contains("solud binary not found"));
}

#[test]
fn the_outcome_is_camel_case_for_the_page() {
    let json = serde_json::to_value(ThisComputerUpdate {
        installer_version: Some("1".into()),
        ..ThisComputerUpdate::new("launched")
    })
    .unwrap();
    assert_eq!(json["state"], "launched");
    assert_eq!(json["installerVersion"], "1");
    assert!(json.get("currentVersion").is_some());
}

// Cancel in the native confirmation: nothing starts, nothing is reported as
// progress, and the page gets "cancelled".
#[test]
fn cancelling_the_confirmation_starts_nothing() {
    let mut started = false;
    let phases = Mutex::new(Vec::<String>::new());
    let mut accepted = false;
    let outcome = run_update(
        || false,
        || accepted = true,
        || {
            started = true;
            Err("must not start".into())
        },
        &|phase: &str| phases.lock().unwrap().push(phase.to_string()),
        &QUICK,
    );
    assert_eq!(outcome.state, "cancelled");
    assert!(!started);
    assert!(!accepted, "nothing accepts an update-only daemon before the person confirms");
    assert!(phases.lock().unwrap().is_empty());
    assert_eq!(serde_json::to_value(&outcome).unwrap()["state"], "cancelled");
}

#[test]
fn the_confirmation_says_what_will_happen() {
    assert_eq!(CONFIRM_TITLE, "Update this computer's desktop app");
    assert_eq!(
        CONFIRM_MESSAGE,
        "Update this computer's desktop app?\n\nSoluTerminal will briefly start its background service on this computer to download and check the installer."
    );
    assert_eq!((CONFIRM_OK, CONFIRM_CANCEL), ("Update", "Cancel"));
}

// A daemon that could not be confirmed stopped is reported, not hidden.
#[test]
fn a_failed_stop_is_reported() {
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: (500, "x") };
    let running = StubDaemon::start("127.0.0.1:0", "host", answers);
    let url = format!("http://{}", running.addr);
    let outcome = run_update(
        || true,
        || (),
        || {
            start_host_daemon(&[], |_, _, _| false, move |_: &str| {
                Ok((url, Box::new(|| Err("daemon pid 42 is still running".to_string())) as StopFn))
            }, || None)
        },
        &no_progress,
        &QUICK,
    );
    assert_eq!(outcome.state, "up-to-date");
    assert_eq!(outcome.cleanup_error.as_deref(), Some("daemon pid 42 is still running"));
}

// The WSL stop, run for real with sh on stand-ins: it stops only processes
// whose own environment carries the marker. The flow's supervisor gets
// SIGKILL (its TERM trap, which here would kill the normal daemon, never
// runs), its daemon is stopped, and a normal daemon and supervisor that were
// already running are left alone.
#[cfg(target_os = "linux")]
#[test]
fn the_wsl_stop_stops_only_what_carries_the_marker() {
    use std::process::{Child, Command, Stdio};
    let dir = std::env::temp_dir().join(format!("solu-wsl-stop-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let solud = dir.join("solud");
    std::fs::copy("/bin/sh", &solud).unwrap();
    let daemon = |addr: &str, marked: bool| -> Child {
        let mut cmd = Command::new(&solud);
        cmd.args(["-c", "sleep 60; :", "--addr", addr]);
        if marked {
            cmd.env("SOLU_UPDATE_ONLY", "1");
        }
        cmd.spawn().unwrap()
    };
    let mut normal = daemon("normal", false);
    let mut ours = daemon("ours", true);
    let supervisor = |marked: bool, trap_kills: u32| -> Child {
        let script = dir.join(format!("start-wsl-{marked}.sh"));
        std::fs::write(
            &script,
            format!("trap 'kill {trap_kills}; exit 143' TERM\nwhile true; do sleep 0.1; done\n"),
        )
        .unwrap();
        let named = dir.join(format!("start-wsl.sh-{marked}"));
        std::fs::rename(&script, &named).unwrap();
        let mut cmd = Command::new("sh");
        cmd.arg(&named);
        if marked {
            cmd.env("SOLU_UPDATE_ONLY", "1");
        }
        cmd.spawn().unwrap()
    };
    let mut our_supervisor = supervisor(true, normal.id());
    let mut normal_supervisor = supervisor(false, normal.id());
    std::thread::sleep(Duration::from_millis(300));

    let run = |script: &str| {
        let mut child = Command::new("sh").arg("-s").stdin(Stdio::piped()).spawn().unwrap();
        child.stdin.take().unwrap().write_all(script.as_bytes()).unwrap();
        child.wait().unwrap()
    };
    let gone = crate::daemon::wsl_flow_gone_script();
    assert!(!run(&gone).success(), "the flow's daemon is still running before the stop");

    // Reap the flow's stand-ins as they die, so they are not left as zombies.
    let ours_pid = ours.id();
    let reaper = std::thread::spawn(move || {
        let _ = ours.wait();
        let _ = our_supervisor.wait();
    });
    assert!(run(&crate::daemon::wsl_stop_script()).success());
    reaper.join().unwrap();
    assert!(run(&gone).success(), "nothing of the flow is left");
    assert!(!std::path::Path::new(&format!("/proc/{ours_pid}/environ")).exists() || {
        std::fs::read(format!("/proc/{ours_pid}/environ")).map(|b| b.is_empty()).unwrap_or(true)
    });

    assert!(normal.try_wait().unwrap().is_none(), "a normal daemon must never be stopped");
    assert!(normal_supervisor.try_wait().unwrap().is_none(), "a normal supervisor must never be stopped");
    for mut child in [normal, normal_supervisor] {
        let _ = child.kill();
        let _ = child.wait();
    }
    let _ = std::fs::remove_dir_all(&dir);
}

// An update-only daemon whose updater is not ready yet is waited for before
// the first update check.
#[test]
fn the_first_check_waits_for_the_updater() {
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: (500, "x") };
    let running = StubDaemon::start_with_not_ready("127.0.0.1:0", "wsl", answers, 3);
    let addr = running.addr.clone();
    let outcome = run_update(
        || true,
        || (),
        || start_host_daemon(&[(addr.as_str(), "wsl")], |_, _, _| true, |_: &str| Err("unused".into()), || None),
        &no_progress,
        &QUICK,
    );
    assert_eq!(outcome.state, "up-to-date", "{outcome:?}");
    let requests = running.requests();
    let check_at = requests.iter().position(|r| r.starts_with("POST /api/update/check")).unwrap();
    let health_before = requests[..check_at].iter().filter(|r| r.starts_with("GET /api/health")).count();
    assert!(health_before >= 4, "checked after {health_before} health polls: {requests:?}");
}

// The hold that lets the flow accept an update-only daemon is taken only after
// the person confirmed, before anything starts, and released only after the
// flow stopped what it started.
#[test]
fn update_only_daemons_are_accepted_only_after_confirmation() {
    let events = std::rc::Rc::new(std::cell::RefCell::new(Vec::<&'static str>::new()));
    struct Hold(std::rc::Rc<std::cell::RefCell<Vec<&'static str>>>);
    impl Drop for Hold {
        fn drop(&mut self) {
            self.0.borrow_mut().push("released");
        }
    }
    let answers = Answers { status: UP_TO_DATE, check: Some(UP_TO_DATE), apply: (500, "x") };
    let running = StubDaemon::start("127.0.0.1:0", "host", answers);
    let url = format!("http://{}", running.addr);
    let (e1, e2, e3) = (events.clone(), events.clone(), events.clone());
    let outcome = run_update(
        move || {
            e1.borrow_mut().push("confirmed");
            true
        },
        move || {
            e2.borrow_mut().push("accepting");
            Hold(e2.clone())
        },
        move || {
            e3.borrow_mut().push("started");
            start_host_daemon(&[], |_, _, _| false, move |_: &str| {
                Ok((url, Box::new(|| Ok(())) as StopFn))
            }, || None)
        },
        &no_progress,
        &QUICK,
    );
    assert_eq!(outcome.state, "up-to-date", "{outcome:?}");
    assert_eq!(*events.borrow(), ["confirmed", "accepting", "started", "released"]);
}
