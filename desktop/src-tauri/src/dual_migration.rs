//! Durable, host-user-wide migration fence (ST-253 ruling 9e).
//! Callers supply an explicit host data directory and authenticated retirement
//! evidence. This module never probes a claim, cloud, process or live profile.

use std::{
    fs, io,
    path::Path,
    sync::atomic::{AtomicU64, Ordering},
};

const INTENT: &str = "dual-migration.json";
const RETIRED: &str = "dual-retired.json";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum State {
    Ready,
    Interrupted,
    Retired,
}

/// Proof must be collected by the local coordinator after joining the mirror.
/// Before requires positive evidence, not merely absence of a response.
#[derive(Clone, Copy, Debug)]
pub(crate) enum Evidence {
    DefinitelyBefore,
    SharedClaimRenamed,
    DistinctClaimRevoked,
    Unknown,
}

pub(crate) struct Record<'a> {
    pub time: u64,
    pub machine_id: &'a str,
}

fn present(path: &Path) -> io::Result<bool> {
    match fs::symlink_metadata(path) {
        Ok(_) => Ok(true), // Presence decides, even malformed content or a link.
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e),
    }
}

pub(crate) fn state(root: &Path) -> io::Result<State> {
    if present(&root.join(RETIRED))? {
        return Ok(State::Retired);
    }
    if present(&root.join(INTENT))?
        || present(&root.join("dual-migration-sending.json"))?
        || present(&root.join("dual-migration-confirmed.json"))?
    {
        // An orphan phase can mean an interrupted/ambiguous publication.
        // Missing intent is not positive evidence that revoke never started.
        return Ok(State::Interrupted);
    }
    Ok(State::Ready)
}

/// For stateful Linux daemon, mirror and claim paths only. Stateless helper
/// callers must not use this gate. I/O uncertainty refuses, never assumes Ready.
pub(crate) fn require_stateful_start(root: &Path) -> io::Result<()> {
    match state(root)? {
        State::Ready => Ok(()),
        State::Interrupted => Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "migration interrupted: retry or contact support",
        )),
        State::Retired => Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Dual was retired by migration",
        )),
    }
}

#[cfg(windows)]
fn host_root() -> io::Result<std::path::PathBuf> {
    let profile = std::env::var_os("USERPROFILE").ok_or_else(|| {
        io::Error::new(
            io::ErrorKind::NotFound,
            "host profile unavailable; migration state unproven",
        )
    })?;
    Ok(Path::new(&profile).join(".solu"))
}

#[cfg(windows)]
pub(crate) fn host_state() -> io::Result<State> {
    state(&host_root()?)
}

#[cfg(windows)]
pub(crate) fn require_host_stateful_start() -> io::Result<()> {
    require_stateful_start(&host_root()?)
}

/// The stop callback must perform the real bounded mirror join. No claim
/// operation can happen inside this preparation step. A failed join therefore
/// proves a pre-retirement abort; the retained in-process worker still needs
/// explicit abort recovery before it can resume.
pub(crate) fn prepare(root: &Path, stop: impl FnOnce() -> io::Result<()>) -> io::Result<()> {
    begin(root)?;
    if let Err(error) = stop() {
        remove_intent(root)?;
        return Err(error);
    }
    Ok(())
}

/// Create intent before cancelling/joining the mirror. An existing intent or
/// retirement is never replaced. The coordinator serializes local operations.
pub(crate) fn begin(root: &Path) -> io::Result<()> {
    require_stateful_start(root)?;
    publish(root, INTENT, b"{\"schema\":1,\"state\":\"in-progress\"}\n")
}

/// No evidence is inferred here. Unknown retains the intent across restarts.
/// The retired file is monotonic: there is deliberately no removal operation.
pub(crate) fn recover(root: &Path, evidence: Evidence, record: Record<'_>) -> io::Result<State> {
    let current = state(root)?;
    if current == State::Ready {
        return Ok(current);
    }
    if current == State::Retired {
        remove_intent(root)?;
        return Ok(State::Retired);
    }
    match evidence {
        Evidence::Unknown => Ok(State::Interrupted),
        Evidence::DefinitelyBefore => {
            if present(&root.join("dual-migration-sending.json"))?
                || present(&root.join("dual-migration-confirmed.json"))?
            {
                // Preserve the bound intent needed to finish an ambiguous or
                // confirmed revoke. Neither phase permits abort back to Dual.
                return Ok(State::Interrupted);
            }
            remove_intent(root)?;
            state(root)
        }
        Evidence::SharedClaimRenamed | Evidence::DistinctClaimRevoked => {
            let id = record.machine_id;
            if id.is_empty()
                || id.len() > 128
                || !id
                    .bytes()
                    .all(|c| c.is_ascii_alphanumeric() || b"-_:".contains(&c))
            {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "invalid informational machine id",
                ));
            }
            let mode = if matches!(evidence, Evidence::SharedClaimRenamed) {
                "shared"
            } else {
                "distinct"
            };
            let data = format!(
                "{{\"schema\":1,\"time\":{},\"mode\":\"{}\",\"machineId\":\"{}\"}}\n",
                record.time, mode, id
            );
            match publish(root, RETIRED, data.as_bytes()) {
                Ok(()) => {}
                Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
                Err(e) => return Err(e),
            }
            remove_intent(root)?;
            Ok(State::Retired)
        }
    }
}

fn remove_intent(root: &Path) -> io::Result<()> {
    match fs::remove_file(root.join(INTENT)) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e),
    }
}

fn publish(root: &Path, name: &str, data: &[u8]) -> io::Result<()> {
    use std::io::Write;
    static SEQ: AtomicU64 = AtomicU64::new(0);
    // Never adopt a temp file left by a previous process.
    let temp = root.join(format!(
        ".dual-migration-{}-{}.tmp",
        std::process::id(),
        SEQ.fetch_add(1, Ordering::Relaxed)
    ));
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(&temp)?;
    let result = (|| {
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        rename_new(&temp, &root.join(name))
    })();
    let _ = fs::remove_file(&temp);
    result
}

#[cfg(windows)]
fn rename_new(from: &Path, to: &Path) -> io::Result<()> {
    use std::os::windows::ffi::OsStrExt;
    #[link(name = "kernel32")]
    extern "system" {
        fn MoveFileExW(from: *const u16, to: *const u16, flags: u32) -> i32;
    }
    let from: Vec<u16> = from.as_os_str().encode_wide().chain(Some(0)).collect();
    let to: Vec<u16> = to.as_os_str().encode_wide().chain(Some(0)).collect();
    // WRITE_THROUGH, deliberately WITHOUT REPLACE_EXISTING. std::fs::rename
    // replaces an existing target and cannot implement this monotonic marker.
    if unsafe { MoveFileExW(from.as_ptr(), to.as_ptr(), 0x8) } == 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

#[cfg(not(windows))]
fn rename_new(from: &Path, to: &Path) -> io::Result<()> {
    // Portable fixture adapter: hard-link publication is atomic and no-clobber.
    fs::hard_link(from, to)?;
    fs::File::open(to.parent().unwrap())?.sync_all()
}

#[cfg(test)]
mod tests {
    use super::*;
    struct Fixture(std::path::PathBuf);
    impl Fixture {
        fn new() -> Self {
            static SEQ: AtomicU64 = AtomicU64::new(0);
            let root = std::env::temp_dir().join(format!(
                "solu-retirement-fixture-{}-{}",
                std::process::id(),
                SEQ.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&root).unwrap();
            Self(root)
        }
        fn record(&self) -> Record<'static> {
            Record {
                time: 1,
                machine_id: "fixture-id",
            }
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    #[test]
    fn join_timeout_aborts_before_any_retirement() {
        let f = Fixture::new();
        let result = prepare(&f.0, || {
            assert_eq!(state(&f.0).unwrap(), State::Interrupted);
            Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "fixture join timeout",
            ))
        });
        assert_eq!(result.unwrap_err().kind(), io::ErrorKind::TimedOut);
        assert_eq!(state(&f.0).unwrap(), State::Ready);
        assert!(!f.0.join(RETIRED).exists());
    }

    #[test]
    fn crash_before_retirement_resumes_only_with_positive_proof() {
        let f = Fixture::new();
        begin(&f.0).unwrap();
        assert!(require_stateful_start(&f.0).is_err());
        assert_eq!(
            recover(&f.0, Evidence::DefinitelyBefore, f.record()).unwrap(),
            State::Ready
        );
        require_stateful_start(&f.0).unwrap();
    }
    #[test]
    fn unreachable_cloud_or_partial_rename_stays_blocked() {
        let f = Fixture::new();
        begin(&f.0).unwrap();
        assert_eq!(
            recover(&f.0, Evidence::Unknown, f.record()).unwrap(),
            State::Interrupted
        );
        assert!(f.0.join(INTENT).exists());
        assert!(!f.0.join(RETIRED).exists());
        assert!(require_stateful_start(&f.0)
            .unwrap_err()
            .to_string()
            .contains("migration interrupted"));
    }
    #[test]
    fn crash_after_shared_rename_or_distinct_revoke_finishes_retirement() {
        for proof in [Evidence::SharedClaimRenamed, Evidence::DistinctClaimRevoked] {
            let f = Fixture::new();
            begin(&f.0).unwrap();
            assert_eq!(recover(&f.0, proof, f.record()).unwrap(), State::Retired);
            assert!(!f.0.join(INTENT).exists());
            assert!(require_stateful_start(&f.0).is_err());
            let before = fs::read(f.0.join(RETIRED)).unwrap();
            recover(&f.0, Evidence::DefinitelyBefore, f.record()).unwrap();
            assert_eq!(fs::read(f.0.join(RETIRED)).unwrap(), before);
            assert!(begin(&f.0).is_err());
        }
    }
    #[test]
    fn retired_presence_wins_over_legacy_repair_repairing_and_corrupt_content() {
        let f = Fixture::new();
        fs::write(f.0.join("legacy-dual.txt"), "existing-dual-v1\n").unwrap();
        fs::write(f.0.join(RETIRED), b"").unwrap();
        assert_eq!(state(&f.0).unwrap(), State::Retired);
        assert!(require_stateful_start(&f.0).is_err());
        assert!(publish(&f.0, RETIRED, b"replacement").is_err());
        assert_eq!(fs::read(f.0.join(RETIRED)).unwrap(), b"");
    }
    #[test]
    fn crash_after_retired_write_before_intent_removal_keeps_retired() {
        let f = Fixture::new();
        begin(&f.0).unwrap();
        publish(&f.0, RETIRED, b"{\"schema\":1}").unwrap();
        assert_eq!(
            recover(&f.0, Evidence::Unknown, f.record()).unwrap(),
            State::Retired
        );
        assert!(!f.0.join(INTENT).exists());
    }
    #[test]
    fn orphan_phase_presence_never_resumes_dual_even_without_intent() {
        for name in ["dual-migration-sending.json", "dual-migration-confirmed.json"] {
            let f = Fixture::new();
            fs::write(f.0.join(name), b"").unwrap();
            assert_eq!(state(&f.0).unwrap(), State::Interrupted);
            assert!(require_stateful_start(&f.0).is_err());
            assert_eq!(recover(&f.0, Evidence::DefinitelyBefore, f.record()).unwrap(), State::Interrupted);
            assert!(f.0.join(name).exists());
        }
    }

    #[test]
    fn before_evidence_preserves_bound_intent_when_revoke_phase_exists() {
        for name in ["dual-migration-sending.json", "dual-migration-confirmed.json"] {
            let f = Fixture::new();
            begin(&f.0).unwrap();
            let intent = fs::read(f.0.join(INTENT)).unwrap();
            fs::write(f.0.join(name), b"fixture phase").unwrap();
            assert_eq!(recover(&f.0, Evidence::DefinitelyBefore, f.record()).unwrap(), State::Interrupted);
            assert_eq!(fs::read(f.0.join(INTENT)).unwrap(), intent);
        }
    }

}
