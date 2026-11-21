//! Cut a of ST-253: existing Dual PCs keep working; new PCs cannot select it.
//!
//! This is a compatibility preference, not an authority boundary against the
//! same OS user. Capture an existing saved Dual choice before replacing it or
//! clearing setup, so a repair or a temporary switch cannot strand that PC.

use std::fs;
use std::io;
use std::path::Path;

const MARKER: &str = "legacy-dual.txt";
const MARKER_VALUE: &str = "existing-dual-v1\n";

pub fn available(pref: &Path) -> bool {
    let saved_dual = fs::read_to_string(pref)
        .map(|s| matches!(s.trim(), "dual" | "both"))
        .unwrap_or(false);
    saved_dual
        || pref.parent().is_some_and(|dir| {
            fs::read_to_string(dir.join(MARKER))
                .map(|s| s == MARKER_VALUE)
                .unwrap_or(false)
        })
}

/// Called before a preference change. A failed marker write must preserve the
/// old preference, otherwise switching away could lose legacy compatibility.
pub fn preserve(pref: &Path) -> io::Result<()> {
    if fs::read_to_string(pref)
        .map(|s| matches!(s.trim(), "dual" | "both"))
        .unwrap_or(false)
    {
        let dir = pref.parent().ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "backend preference has no directory",
            )
        })?;
        fs::write(dir.join(MARKER), MARKER_VALUE)?;
    }
    Ok(())
}

/// Guard all preference writers, including direct desktop command invokes.
pub fn before_write(pref: &Path, selecting_dual: bool) -> io::Result<()> {
    if selecting_dual && !available(pref) {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "Dual is available only on PCs already configured for it. Choose Linux (WSL) or Windows.",
        ));
    }
    preserve(pref)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};

    struct Fixture(std::path::PathBuf);

    impl Fixture {
        fn new() -> Self {
            static SEQ: AtomicU64 = AtomicU64::new(0);
            let dir = std::env::temp_dir().join(format!(
                "solu-legacy-dual-{}-{}",
                std::process::id(),
                SEQ.fetch_add(1, Ordering::Relaxed),
            ));
            fs::create_dir(&dir).unwrap();
            Self(dir)
        }
        fn pref(&self) -> std::path::PathBuf {
            self.0.join("backend.txt")
        }
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn fresh_and_single_backend_profiles_cannot_create_dual() {
        let fixture = Fixture::new();
        for saved in [None, Some("wsl"), Some("host"), Some("invalid")] {
            if let Some(saved) = saved {
                fs::write(fixture.pref(), saved).unwrap();
            }
            assert!(!available(&fixture.pref()));
            assert_eq!(
                before_write(&fixture.pref(), true).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
            assert_eq!(fs::read_to_string(fixture.pref()).ok().as_deref(), saved);
            assert!(!fixture.0.join(MARKER).exists());
            before_write(&fixture.pref(), false).unwrap();
        }
    }

    #[test]
    fn existing_dual_survives_switching_away_and_repair_reset() {
        for value in ["dual", " both\n"] {
            let fixture = Fixture::new();
            fs::write(fixture.pref(), value).unwrap();
            assert!(available(&fixture.pref()));
            before_write(&fixture.pref(), false).unwrap();
            fs::write(fixture.pref(), "host").unwrap();
            assert!(available(&fixture.pref()));
            before_write(&fixture.pref(), true).unwrap();
            fs::remove_file(fixture.pref()).unwrap();
            assert!(available(&fixture.pref()));
            before_write(&fixture.pref(), true).unwrap();
        }
    }

    #[test]
    fn failed_preservation_leaves_the_saved_dual_choice_intact() {
        let fixture = Fixture::new();
        fs::write(fixture.pref(), "dual").unwrap();
        fs::create_dir(fixture.0.join(MARKER)).unwrap();
        assert!(before_write(&fixture.pref(), false).is_err());
        assert_eq!(fs::read_to_string(fixture.pref()).unwrap(), "dual");
    }

    #[test]
    fn an_empty_or_unrecognized_marker_does_not_offer_dual() {
        let fixture = Fixture::new();
        for value in ["", "1", "existing-dual-v2\n"] {
            fs::write(fixture.0.join(MARKER), value).unwrap();
            assert!(!available(&fixture.pref()));
        }
    }
}
