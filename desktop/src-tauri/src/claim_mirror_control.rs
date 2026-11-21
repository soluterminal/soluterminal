//! In-process lifetime control for the desktop's Dual claim mirror (ST-253).
//!
//! A stop acknowledgement means the worker was JOINED, not merely cancelled.
//! A timed-out stop retains the handle and keeps starts paused. Durable
//! migration/retirement policy belongs to the caller; this module touches no
//! claims, preferences, files, processes, network or external IPC.

use std::sync::{mpsc, Arc, Condvar, Mutex, TryLockError};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

#[derive(Clone, Default)]
pub(crate) struct CancelToken(Arc<(Mutex<bool>, Condvar)>);

impl CancelToken {
    fn cancel(&self) {
        let (lock, wake) = &*self.0;
        if let Ok(mut cancelled) = lock.lock() {
            *cancelled = true;
        }
        wake.notify_all();
    }

    pub(crate) fn is_cancelled(&self) -> bool {
        self.0 .0.lock().map(|value| *value).unwrap_or(true)
    }

    /// Wait for the next tick; true means cancellation, including poison.
    pub(crate) fn wait(&self, duration: Duration) -> bool {
        let (lock, wake) = &*self.0;
        let Ok(cancelled) = lock.lock() else {
            return true;
        };
        match wake.wait_timeout_while(cancelled, duration, |value| !*value) {
            Ok((cancelled, _)) => *cancelled,
            Err(_) => true,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ControlError {
    LockPoisoned,
    Busy,
    SpawnFailed,
    Paused,
    StillRunning,
    StopTimedOut,
    WorkerPanicked,
    WorkerFailed,
}

struct Worker {
    cancel: CancelToken,
    joined: mpsc::Receiver<Result<(), ControlError>>,
    handle: Arc<Mutex<Option<JoinHandle<Result<(), ()>>>>>,
    joiner: Option<JoinHandle<()>>,
}

#[derive(Default)]
struct State {
    paused: bool,
    worker: Option<Worker>,
    failure: Option<ControlError>,
    resume_on_abort: bool,
}

#[derive(Default)]
pub(crate) struct MirrorControl(Mutex<State>);

impl MirrorControl {
    /// Start at most one worker. A stop pauses starts until explicit abort
    /// recovery; another startup call cannot race past a successful join.
    pub(crate) fn start<F>(&self, run: F) -> Result<bool, ControlError>
    where
        F: FnOnce(CancelToken) -> Result<(), ()> + Send + 'static,
    {
        let mut state = self.0.lock().map_err(|_| ControlError::LockPoisoned)?;
        if state.paused {
            return Err(ControlError::Paused);
        }
        if state.worker.is_some() {
            return Ok(false);
        }
        let cancel = CancelToken::default();
        let token = cancel.clone();
        let handle = thread::Builder::new()
            .name("solu-dual-claim-mirror".into())
            .spawn(move || run(token))
            .map_err(|_| ControlError::SpawnFailed)?;
        state.resume_on_abort = true;
        let handle = Arc::new(Mutex::new(Some(handle)));
        let (send, joined) = mpsc::channel();
        let joining = handle.clone();
        // Only this monitor calls join, so the caller's deadline also bounds
        // arbitrary thread-local cleanup in the actual mirror worker.
        let joiner = thread::Builder::new()
            .name("solu-dual-mirror-join".into())
            .spawn(move || {
                let worker = joining.lock().unwrap().take().unwrap();
                let result = match worker.join() {
                    Ok(Ok(())) => Ok(()),
                    Ok(Err(())) => Err(ControlError::WorkerFailed),
                    Err(_) => Err(ControlError::WorkerPanicked),
                };
                let _ = send.send(result);
            });
        let failed = joiner.is_err();
        state.worker = Some(Worker {
            cancel,
            joined,
            handle,
            joiner: joiner.ok(),
        });
        if failed {
            // Spawn failure drops the monitor closure, not the worker handle.
            // Cancel and retain it; never let another startup duplicate it.
            state.paused = true;
            state.worker.as_ref().unwrap().cancel.cancel();
            return Err(ControlError::SpawnFailed);
        }
        Ok(true)
    }

    /// Blocking seam for the future local migration coordinator. On any
    /// error it MUST abort before claim rename/revoke. Timeout leaves the
    /// worker tracked; callers may retry joining, never assume it stopped.
    pub(crate) fn stop_and_join(&self, timeout: Duration) -> Result<(), ControlError> {
        let began = Instant::now();
        let mut state = self.0.try_lock().map_err(|error| match error {
            TryLockError::WouldBlock => ControlError::Busy,
            TryLockError::Poisoned(_) => ControlError::LockPoisoned,
        })?;
        state.paused = true;
        if let Some(worker) = state.worker.as_ref() {
            worker.cancel.cancel();
            if worker.joiner.is_none() {
                return Err(ControlError::SpawnFailed);
            }
            match worker
                .joined
                .recv_timeout(timeout.saturating_sub(began.elapsed()))
            {
                Ok(result) => {
                    state.failure = result.err();
                    state.worker = None;
                }
                Err(mpsc::RecvTimeoutError::Timeout) => return Err(ControlError::StopTimedOut),
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    return Err(ControlError::WorkerPanicked);
                }
            }
        }
        state.failure.map_or(Ok(()), Err)
    }

    /// Only for a coordinator that proved it aborted BEFORE retirement and
    /// released its durable intent gate. This clears in-process pause only;
    /// it is not an API to clear any durable migration/retirement state.
    /// The result says whether a worker existed and should be resumed.
    pub(crate) fn allow_after_abort(&self) -> Result<bool, ControlError> {
        let mut state = self.0.try_lock().map_err(|error| match error {
            TryLockError::WouldBlock => ControlError::Busy,
            TryLockError::Poisoned(_) => ControlError::LockPoisoned,
        })?;
        if let Some(worker) = state.worker.as_ref() {
            if worker.joiner.is_none() {
                let mut slot = worker
                    .handle
                    .lock()
                    .map_err(|_| ControlError::LockPoisoned)?;
                // No monitor could start. A non-blocking abort must not join
                // even an is_finished worker: TLS cleanup can still be live.
                if slot.is_some() {
                    return Err(ControlError::StillRunning);
                }
                slot.take();
            } else {
                match worker.joined.try_recv() {
                    Ok(_) => {}
                    Err(mpsc::TryRecvError::Empty) => return Err(ControlError::StillRunning),
                    Err(mpsc::TryRecvError::Disconnected) => {
                        return Err(ControlError::WorkerPanicked)
                    }
                }
            }
            state.worker = None;
        }
        state.failure = None;
        state.paused = false;
        Ok(std::mem::take(&mut state.resume_on_abort))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    const LIMIT: Duration = Duration::from_secs(5);

    #[test]
    fn cancelling_wakes_idle_wait_and_joins_before_acknowledging() {
        let control = MirrorControl::default();
        let (entered, ready) = mpsc::channel();
        let (exited, done) = mpsc::channel();
        control
            .start(move |cancel| {
                entered.send(()).unwrap();
                assert!(cancel.wait(Duration::from_secs(60)));
                exited.send(()).unwrap();
                Ok(())
            })
            .unwrap();
        ready.recv_timeout(LIMIT).unwrap();
        control.stop_and_join(LIMIT).unwrap();
        done.try_recv().unwrap();
        control.stop_and_join(Duration::ZERO).unwrap();
        assert_eq!(control.start(|_| Ok(())), Err(ControlError::Paused));
    }

    #[test]
    fn in_flight_copy_must_finish_before_the_stop_ack() {
        let control = Arc::new(MirrorControl::default());
        let (entered, ready) = mpsc::channel();
        let (release, copy_wait) = mpsc::channel();
        let writes = Arc::new(AtomicUsize::new(0));
        let observed = writes.clone();
        control
            .start(move |cancel| {
                entered.send(cancel.clone()).unwrap();
                copy_wait.recv_timeout(LIMIT).unwrap();
                assert!(cancel.is_cancelled());
                observed.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
            .unwrap();
        let cancellation = ready.recv_timeout(LIMIT).unwrap();
        let (ack, acknowledgement) = mpsc::channel();
        let stopping = control.clone();
        let stopper = thread::spawn(move || ack.send(stopping.stop_and_join(LIMIT)).unwrap());
        assert!(cancellation.wait(LIMIT));
        assert_eq!(
            acknowledgement.recv_timeout(Duration::from_millis(20)),
            Err(mpsc::RecvTimeoutError::Timeout)
        );
        assert_eq!(writes.load(Ordering::SeqCst), 0);
        release.send(()).unwrap();
        assert_eq!(acknowledgement.recv_timeout(LIMIT).unwrap(), Ok(()));
        stopper.join().unwrap();
        assert_eq!(writes.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn timeout_retains_worker_blocks_restart_and_can_be_joined_later() {
        let control = MirrorControl::default();
        let (entered, ready) = mpsc::channel();
        let (release, copy_wait) = mpsc::channel();
        control
            .start(move |_| {
                entered.send(()).unwrap();
                copy_wait.recv_timeout(LIMIT).unwrap();
                Ok(())
            })
            .unwrap();
        ready.recv_timeout(LIMIT).unwrap();
        assert_eq!(
            control.stop_and_join(Duration::ZERO),
            Err(ControlError::StopTimedOut)
        );
        assert_eq!(
            control.start(|_| panic!("must not start a second mirror")),
            Err(ControlError::Paused)
        );
        assert_eq!(control.allow_after_abort(), Err(ControlError::StillRunning));
        release.send(()).unwrap();
        control.stop_and_join(LIMIT).unwrap();
        assert_eq!(control.start(|_| Ok(())), Err(ControlError::Paused));
        control.allow_after_abort().unwrap();
        control
            .start(|cancel| {
                cancel.wait(LIMIT);
                Ok(())
            })
            .unwrap();
        control.stop_and_join(LIMIT).unwrap();
    }

    #[test]
    fn duplicate_starts_never_spawn_a_second_worker() {
        let control = MirrorControl::default();
        let (entered, ready) = mpsc::channel();
        control
            .start(move |cancel| {
                entered.send(()).unwrap();
                cancel.wait(LIMIT);
                Ok(())
            })
            .unwrap();
        ready.recv_timeout(LIMIT).unwrap();
        assert_eq!(control.start(|_| panic!("duplicate worker")), Ok(false));
        control.stop_and_join(LIMIT).unwrap();
    }

    #[test]
    fn panic_and_copy_failure_are_not_quiescence_acknowledgements() {
        for panics in [false, true] {
            let control = MirrorControl::default();
            control
                .start(move |_| {
                    if panics {
                        panic!("fixture worker panic");
                    }
                    Err(())
                })
                .unwrap();
            let expected = if panics {
                ControlError::WorkerPanicked
            } else {
                ControlError::WorkerFailed
            };
            assert_eq!(control.stop_and_join(LIMIT), Err(expected));
            assert_eq!(control.stop_and_join(LIMIT), Err(expected));
            assert_eq!(control.start(|_| Ok(())), Err(ControlError::Paused));
        }
    }

    #[test]
    fn cancellation_before_copy_never_enters_the_copy() {
        let control = MirrorControl::default();
        let (entered, ready) = mpsc::channel();
        let (release, wait) = mpsc::channel();
        let writes = Arc::new(AtomicUsize::new(0));
        let observed = writes.clone();
        control
            .start(move |cancel| {
                entered.send(()).unwrap();
                wait.recv_timeout(LIMIT).unwrap();
                if !cancel.is_cancelled() {
                    observed.fetch_add(1, Ordering::SeqCst);
                }
                Ok(())
            })
            .unwrap();
        ready.recv_timeout(LIMIT).unwrap();
        assert_eq!(
            control.stop_and_join(Duration::ZERO),
            Err(ControlError::StopTimedOut)
        );
        release.send(()).unwrap();
        control.stop_and_join(LIMIT).unwrap();
        assert_eq!(writes.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn callback_completion_is_not_a_join_if_thread_cleanup_is_still_running() {
        use std::cell::RefCell;
        struct ExitGate(mpsc::Sender<()>, mpsc::Receiver<()>);
        impl Drop for ExitGate {
            fn drop(&mut self) {
                self.0.send(()).unwrap();
                self.1.recv_timeout(LIMIT).unwrap();
            }
        }
        thread_local! { static EXIT: RefCell<Option<ExitGate>> = const { RefCell::new(None) }; }
        let control = MirrorControl::default();
        let (entered, cleanup_started) = mpsc::channel();
        let (release, cleanup_wait) = mpsc::channel();
        control
            .start(move |_| {
                EXIT.with(|slot| *slot.borrow_mut() = Some(ExitGate(entered, cleanup_wait)));
                Ok(())
            })
            .unwrap();
        cleanup_started.recv_timeout(LIMIT).unwrap();
        assert_eq!(
            control.stop_and_join(Duration::ZERO),
            Err(ControlError::StopTimedOut)
        );
        release.send(()).unwrap();
        control.stop_and_join(LIMIT).unwrap();
    }

    #[test]
    fn production_adapter_joins_and_propagates_failed_copy_without_wsl_or_claims() {
        use std::fs;
        use std::process::Command;
        static SEQ: AtomicUsize = AtomicUsize::new(0);
        let module = fs::canonicalize(file!()).unwrap();
        let daemon = fs::read_to_string(module.with_file_name("daemon.rs"))
            .unwrap()
            .replace("\r\n", "\n");
        let start = daemon
            .find("#[cfg(windows)]\nstatic DUAL_CLAIM_MIRROR:")
            .unwrap();
        let end = start
            + daemon[start..]
                .find("/// How often the mirror looks")
                .unwrap();
        let adapter = daemon[start..end].replace("#[cfg(windows)]\n", "");
        let root = std::env::temp_dir().join(format!(
            "solu-mirror-adapter-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&root).unwrap();
        struct Cleanup(std::path::PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.0);
            }
        }
        let _cleanup = Cleanup(root.clone());
        let harness = root.join("adapter.rs");
        let binary = root.join(if cfg!(windows) {
            "adapter.exe"
        } else {
            "adapter"
        });
        let stubs = r#"
use std::sync::{mpsc, Mutex, OnceLock};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::{Duration, Instant};
mod dual_migration { pub fn require_host_stateful_start() -> std::io::Result<()> {
    if super::BLOCKED.load(super::Ordering::SeqCst) { Err(std::io::Error::new(std::io::ErrorKind::PermissionDenied, "fixture fence")) } else { Ok(()) }
} }
const CLAIM_MIRROR_TICK: Duration = Duration::from_secs(60);
static CALLS: AtomicUsize = AtomicUsize::new(0);
static FAIL: AtomicBool = AtomicBool::new(false);
static BLOCKED: AtomicBool = AtomicBool::new(false);
static ENTERED: OnceLock<Mutex<mpsc::Sender<()>>> = OnceLock::new();
fn launch_log(_: &str) {}
fn read_win_claim_bytes() -> Option<Vec<u8>> { None }
fn sync_dual_claim_once(_: Option<Vec<u8>>) -> Result<(), String> {
    CALLS.fetch_add(1, Ordering::SeqCst);
    ENTERED.get().unwrap().lock().unwrap().send(()).unwrap();
    if FAIL.load(Ordering::SeqCst) { Err("fixture copy failure".into()) } else { Ok(()) }
}
struct ClaimMirrorSchedule;
impl ClaimMirrorSchedule {
    fn new(_: Instant) -> Self { Self }
    fn due(&mut self, _: Instant, _: Option<&[u8]>) -> bool { true }
    fn succeeded(&mut self, _: Instant, _: Option<Vec<u8>>) {}
    fn failed(&mut self, _: Instant) -> Duration { Duration::ZERO }
    fn should_log_failure(&self) -> bool { false }
    fn fail_streak(&self) -> u32 { 1 }
}
fn main() {
    let (send, receive) = mpsc::channel();
    ENTERED.set(Mutex::new(send)).unwrap();
    BLOCKED.store(true, Ordering::SeqCst);
    assert!(start_dual_claim_mirror_worker().is_err());
    assert_eq!(CALLS.load(Ordering::SeqCst), 0);
    BLOCKED.store(false, Ordering::SeqCst);
    stop_dual_claim_mirror(Duration::ZERO).unwrap();
    resume_dual_claim_mirror_after_abort().unwrap();
    assert_eq!(CALLS.load(Ordering::SeqCst), 0);
    start_dual_claim_mirror();
    receive.recv_timeout(Duration::from_secs(5)).unwrap();
    start_dual_claim_mirror(); // duplicate startup is inert
    stop_dual_claim_mirror(Duration::from_secs(5)).unwrap();
    assert_eq!(CALLS.load(Ordering::SeqCst), 1);
    start_dual_claim_mirror(); // cannot race past the joined barrier
    assert!(receive.recv_timeout(Duration::from_millis(20)).is_err());
    FAIL.store(true, Ordering::SeqCst);
    resume_dual_claim_mirror_after_abort().unwrap();
    receive.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(stop_dual_claim_mirror(Duration::from_secs(5)).is_err());
    assert!(stop_dual_claim_mirror(Duration::from_secs(5)).is_err());
    assert_eq!(CALLS.load(Ordering::SeqCst), 2);
}
"#;
        fs::write(
            &harness,
            format!("#[path = {module:?}] mod claim_mirror_control;\n{stubs}\n{adapter}"),
        )
        .unwrap();
        let rustc = std::env::var_os("RUSTC").unwrap_or_else(|| "rustc".into());
        let output = Command::new(rustc)
            .args(["--edition=2021", "-o"])
            .arg(&binary)
            .arg(&harness)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "adapter compile: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let output = Command::new(&binary).output().unwrap();
        assert!(
            output.status.success(),
            "adapter fixture: {}",
            String::from_utf8_lossy(&output.stderr)
        );
    }
}
