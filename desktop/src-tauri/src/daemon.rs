// Sidecar management — spawn the solud daemon when the Tauri app
// launches (unless it's already running), kill it when the app exits.
//
// Two backends:
//   - host-native: launch the bundled solud/solud.exe from next to the
//     Tauri exe. Works on Linux / macOS / Windows.
//   - wsl (Windows host only): launch the bundled Linux solud binary
//     INSIDE WSL via `wsl.exe -e`. Gives the user the full-featured
//     Linux daemon (state detection, port panel) without a separate
//     install.
//
// WSL-specific plumbing:
//   WSL2's built-in localhost forwarding (wslhost/wslrelay) is flaky —
//   breaks on sleep/hibernate, WSL version bumps, firewall changes.
//   Instead of relying on it, we:
//     1. Bind solud inside WSL to 0.0.0.0:8787 (reachable on WSL's eth0).
//     2. Resolve the WSL2 VM's IP (`wsl hostname -I`).
//     3. Run our own Rust TCP byte-proxy listening on Windows
//        127.0.0.1:18787 and forwarding each connection to the first
//        Windows-reachable WSL daemon address (<wsl_ip>:8787 in NAT
//        mode, 127.0.0.1:8787 in mirrored mode).
//   Result: the webview gets a stable loopback origin independent of
//   Microsoft's forwarding proxy health.

use std::io::{self, Read, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

#[cfg(windows)]
use std::net::TcpListener;
#[cfg(windows)]
use std::os::windows::process::CommandExt;
#[cfg(windows)]
use tauri::{AppHandle, Manager};

/// Windows CreateProcess flag to suppress the console window a child
/// process would otherwise pop up. Each `wsl.exe` invocation without
/// this flag flashes (short-lived probes) or leaves behind an empty
/// black box (long-lived daemon). Applied to every wsl.exe spawn.
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x08000000;

/// Cross-module entry point for the Job Object cleanup mechanism so
/// pair.rs (and any other future spawn site) can assign their child
/// processes without needing direct access to the private `job`
/// module. See `mod job` below for the full rationale.
#[cfg(windows)]
pub fn assign_child_to_job(child: &std::process::Child) {
    job::assign(child);
}

/// Windows Job Object that owns every child process we spawn (daemons,
/// router, claim subprocess, TCP-proxy listeners-by-proxy via guards).
/// Created lazily on first use, kept alive for the lifetime of the
/// Tauri process. When the Tauri process exits (normal quit, crash,
/// task-manager kill, installer file-in-use kill), the OS closes
/// every handle this process held — including this job — and
/// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE causes the kernel to terminate
/// every process still assigned to the job. This is what we rely on
/// instead of SidecarGuard::drop, which is NOT guaranteed to run:
/// Tauri's app.exit(0) uses std::process::exit, which skips Drop on
/// stack values. Without the job, tray-quit leaked orphan daemons.
#[cfg(windows)]
mod job;

/// `--relay <url>` for every spawned daemon so it can proxy voice
/// transcription (and push notifications / relay features) through the
/// cloud using the user's claim.json. Paired with --relay-token pulled
/// from claim.json automatically by the daemon when --relay-token is
/// omitted. Safe to pass even when the user hasn't paired yet — the
/// daemon downgrades to "cloud disabled" in that case (see
/// daemon/main.go's non-fatal branch). Empty in a build without a hosted
/// cloud (hosted_cloud.rs): the daemon then never dials a relay.
fn relay_args() -> Vec<&'static str> {
    match crate::hosted_cloud::relay_url() {
        Some(url) => vec!["--relay", url],
        None => Vec::new(),
    }
}

/// Backend identifiers passed to `solud --backend-id <id>` so the
/// cloud can distinguish multiple daemons sharing one claim. When both
/// a WSL and a Windows daemon are up (Backend::Dual), each registers
/// with the cloud under its own ID so /m/<mid>/b/wsl/api/* and
/// /m/<mid>/b/win/api/* route to the right one. Single-daemon hosts
/// register as "host" — mobile clients see exactly one backend and
/// don't render the switcher UI.
// Not #[cfg(windows)] like its sibling below: `wsl_start_script_body` bakes
// this into the generated launcher and is deliberately cfg-free so the
// template can be unit-tested (and `bash -n`'d) on Linux CI. Gating it would
// make the crate fail to build anywhere but Windows.
#[allow(dead_code)]
pub const BACKEND_ID_WSL: &str = "wsl";
#[cfg(windows)]
pub const BACKEND_ID_WIN: &str = "win";
pub const BACKEND_ID_HOST: &str = "host";

/// Where the Windows-native daemon listens in Dual mode. WSL's localhost
/// forwarding mirrors the WSL daemon's `0.0.0.0:8787` onto Windows'
/// `127.0.0.1:8787`, and the WSL daemon deliberately outlives app restarts —
/// so on every launch after the first it already owned :8787 and the Windows
/// backend lost the race, leaving "Dual" silently running WSL-only (observed
/// 2026-09-24/25 on every launch). The Windows backend takes its own port
/// instead; Windows-only (HostNative) mode keeps :8787. The Go daemon treats
/// this port as canonical too (daemon/claude_mcp_register.go).
pub const DUAL_WIN_ADDR: &str = "127.0.0.1:38787";

/// The single-backend daemon address (Windows-only / host-native modes).
const DEFAULT_DAEMON_ADDR: &str = "127.0.0.1:8787";

#[cfg_attr(not(windows), allow(dead_code))]
const GIB: u64 = 1024 * 1024 * 1024;

#[cfg_attr(not(windows), allow(dead_code))]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HostDiskPolicy {
    pub min_free_bytes: u64,
    pub min_free_pct: u8,
}

impl Default for HostDiskPolicy {
    fn default() -> Self {
        Self {
            min_free_bytes: 16 * GIB,
            min_free_pct: 3,
        }
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum HostDiskVerdict {
    Ok,
    Warn {
        free: u64,
        total: Option<u64>,
        floor: u64,
    },
    Block {
        free: u64,
        total: Option<u64>,
        floor: u64,
    },
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn host_disk_verdict(
    free: u64,
    total: Option<u64>,
    policy: &HostDiskPolicy,
) -> HostDiskVerdict {
    let pct_floor = total
        .map(|t| t.saturating_mul(policy.min_free_pct as u64) / 100)
        .unwrap_or(0);
    let floor = policy.min_free_bytes.max(pct_floor);
    if floor > 0 && free < floor {
        HostDiskVerdict::Block { free, total, floor }
    } else if floor > 0 && free < floor.saturating_mul(2) {
        HostDiskVerdict::Warn { free, total, floor }
    } else {
        HostDiskVerdict::Ok
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn host_disk_policy_from_sources(
    env_mb: Option<&str>,
    env_pct: Option<&str>,
    policy_json: Option<&str>,
) -> HostDiskPolicy {
    let defaults = HostDiskPolicy::default();
    let disk = policy_json.and_then(|raw| serde_json::from_str::<serde_json::Value>(raw).ok());
    let json_mb = disk
        .as_ref()
        .and_then(|v| v.get("minFreeMB"))
        .and_then(|v| v.as_u64())
        .filter(|v| *v > 0);
    let json_pct = disk
        .as_ref()
        .and_then(|v| v.get("minFreePct"))
        .and_then(|v| v.as_u64())
        .filter(|v| *v > 0);
    let mb = env_mb
        .and_then(|v| v.trim().parse::<u64>().ok())
        .or(json_mb)
        .unwrap_or(defaults.min_free_bytes / (1024 * 1024));
    let pct = env_pct
        .and_then(|v| v.trim().parse::<u64>().ok())
        .or(json_pct)
        .unwrap_or(defaults.min_free_pct as u64)
        .min(50);
    HostDiskPolicy {
        min_free_bytes: mb.saturating_mul(1024 * 1024),
        min_free_pct: pct as u8,
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn host_disk_verdict_from_sources(
    free: u64,
    total: Option<u64>,
    env_mb: Option<&str>,
    env_pct: Option<&str>,
    policy_json: Option<&str>,
) -> HostDiskVerdict {
    let policy = host_disk_policy_from_sources(env_mb, env_pct, policy_json);
    host_disk_verdict(free, total, &policy)
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn host_disk_block_message(v: &HostDiskVerdict) -> String {
    match v {
        HostDiskVerdict::Block { free, floor, .. } => format!(
            "host disk critically low ({:.1} GB free on C:, {:.1} GB needed) — WSL starts and restarts are paused; existing sessions may still be reachable. Free host space or run scripts\\wsl-vhdx-compact.ps1; see docs/RUNBOOK-wsl-host-disk.md.",
            *free as f64 / GIB as f64, *floor as f64 / GIB as f64),
        HostDiskVerdict::Warn { free, floor, .. } => format!(
            "host disk low ({:.1} GB free on C:, warning floor {:.1} GB) — WSL will start, but host headroom is limited. Free host space or see docs/RUNBOOK-wsl-host-disk.md.",
            *free as f64 / GIB as f64, *floor as f64 / GIB as f64),
        HostDiskVerdict::Ok => "host disk is healthy".into(),
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn recovery_should_spawn_wsl(v: &HostDiskVerdict) -> bool {
    !matches!(v, HostDiskVerdict::Block { .. })
}

// `--verbose --running` does NOT reliably filter stopped distributions.
// Intersect the default marked in the verbose listing with a separate quiet
// running-only listing. Neither list operation starts a distribution, and we
// do not depend on localized column headings or state names.
#[cfg_attr(not(windows), allow(dead_code))]
fn default_wsl_is_running(verbose: &str, running: &str) -> bool {
    let Some(row) = verbose.lines().find_map(|line| line.trim_start().strip_prefix("* ")) else {
        return false;
    };
    running.lines().map(str::trim).filter(|name| !name.is_empty()).any(|name| {
        let Some(rest) = row.trim().strip_prefix(name) else { return false; };
        // WSL pads columns with multiple spaces. A single space may be part
        // of a longer distro name (Ubuntu vs Ubuntu Dev), not the state column.
        if rest.chars().take_while(|c| c.is_whitespace()).count() < 2 {
            return false;
        }
        let Some((state, version)) = rest.trim().rsplit_once(char::is_whitespace) else {
            return false;
        };
        !state.trim().is_empty() && matches!(version, "1" | "2")
    })
}

#[cfg_attr(not(windows), allow(dead_code))]
fn decode_wsl_listing(raw: &[u8]) -> String {
    if raw.starts_with(&[0xff, 0xfe]) || raw.contains(&0) {
        let units: Vec<_> = raw
            .chunks_exact(2)
            .map(|pair| u16::from_le_bytes([pair[0], pair[1]]))
            .collect();
        String::from_utf16_lossy(&units)
            .trim_start_matches('\u{feff}')
            .to_string()
    } else {
        String::from_utf8_lossy(raw).into_owned()
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
fn allow_existing_wsl_probe(
    verdict: &HostDiskVerdict,
    running: impl FnOnce() -> Result<bool, String>,
) -> Result<(), String> {
    if recovery_should_spawn_wsl(verdict) {
        return Ok(());
    }
    match running() {
        Ok(true) => Ok(()),
        Ok(false) => Err(host_disk_block_message(verdict)),
        Err(e) => Err(format!(
            "{}; cannot verify running WSL: {e}",
            host_disk_block_message(verdict)
        )),
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
pub fn append_bounded_jsonl(path: &Path, line: &str, max_bytes: u64) -> io::Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let needed = line.len() as u64 + 1;
    if std::fs::metadata(path)
        .map(|m| m.len().saturating_add(needed) > max_bytes)
        .unwrap_or(false)
    {
        let p = path.to_string_lossy();
        let gen1 = PathBuf::from(format!("{p}.1"));
        let gen2 = PathBuf::from(format!("{p}.2"));
        let _ = std::fs::remove_file(&gen2);
        let _ = std::fs::rename(&gen1, &gen2);
        std::fs::rename(path, &gen1)?;
    }
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)?;
    writeln!(f, "{}", line.trim_end_matches('\n'))
}

pub struct SidecarGuard {
    child: Option<Child>,
    /// Watchdogs for host-native daemons we started. The watchdog owns
    /// the daemon process (it has to, to reap and respawn it), so a
    /// supervised guard carries `child: None` and stops the daemon
    /// through here instead. A Vec because Backend::Dual merges two
    /// guards into one and either side may be supervised.
    supervisors: Vec<SupervisorHandle>,
    /// Windows-only TCP byte-proxy handle (127.0.0.1:18787 → WSL daemon).
    /// None when not using the WSL backend. Drops stop the proxy listener.
    #[cfg(windows)]
    proxy: Option<ProxyHandle>,
    /// Dual/router stashes: hold the additional daemon + router child
    /// processes + proxy handles that come alive in Backend::Dual.
    /// Empty for single-backend runs. Drop order iterates in-order so
    /// the last-spawned goes down first.
    #[cfg(windows)]
    extras: Vec<Child>,
    #[cfg(windows)]
    extra_proxies: Vec<ProxyHandle>,
}

/// Result of bringing up a daemon: the lifetime guard plus the initial
/// URL the webview should load. The URL carries a `?token=` query when
/// the daemon is reached via a non-loopback path (WSL backend forwarded
/// through our TCP proxy), so the daemon's auth gate sees an authenticated
/// first request and sets its session cookie for everything after.
pub struct DaemonStart {
    pub guard: SidecarGuard,
    pub initial_url: String,
    /// Optional secondary daemon URL — set when Backend::Dual is active
    /// so the frontend can find the other backend. None otherwise.
    pub alt_url: Option<String>,
}

impl DaemonStart {
    pub fn single(guard: SidecarGuard, initial_url: String) -> Self {
        Self {
            guard,
            initial_url,
            alt_url: None,
        }
    }
}

/// Merge two SidecarGuards into one, so Backend::Dual can manage both
/// children+proxies through the single Mutex<SidecarGuard> in lib.rs.
/// Drop order is primary first, then secondary — matches the user's
/// active-backend priority so the visible daemon exits before its
/// sibling.
#[cfg(windows)]
pub fn merge_guards(mut primary: SidecarGuard, mut secondary: SidecarGuard) -> SidecarGuard {
    if let Some(c) = secondary.child.take() {
        primary.extras.push(c);
    }
    // Also absorb the secondary's own extras in case it's a merged
    // guard already (e.g. merging in the router).
    primary.extras.extend(secondary.extras.drain(..));
    primary.supervisors.extend(secondary.supervisors.drain(..));
    if let Some(p) = secondary.proxy.take() {
        primary.extra_proxies.push(p);
    }
    primary
        .extra_proxies
        .extend(secondary.extra_proxies.drain(..));
    primary
}

impl SidecarGuard {
    pub fn empty() -> Self {
        SidecarGuard {
            child: None,
            supervisors: Vec::new(),
            #[cfg(windows)]
            proxy: None,
            #[cfg(windows)]
            extras: Vec::new(),
            #[cfg(windows)]
            extra_proxies: Vec::new(),
        }
    }
}

impl Drop for SidecarGuard {
    fn drop(&mut self) {
        // Stop accepting new client connections first so outstanding
        // forwarded streams get cleanly EOF'd rather than half-open.
        #[cfg(windows)]
        {
            self.proxy.take();
            self.extra_proxies.clear();
        }
        // Before killing anything: tell the watchdogs to stand down and
        // let them take their own daemons with them. A watchdog still
        // running here would see the daemon die and dutifully respawn
        // the process we are quitting.
        for s in &self.supervisors {
            s.stop();
        }
        if let Some(mut c) = self.child.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
        #[cfg(windows)]
        {
            // LIFO: last-spawned (e.g. router) goes down first, then
            // the dual daemon, then anything else stashed.
            while let Some(mut c) = self.extras.pop() {
                let _ = c.kill();
                let _ = c.wait();
            }
        }
    }
}

/// Host-native daemon. Binds 127.0.0.1:8787 so the daemon sees loopback
/// traffic and doesn't require a token. Webview loads http://127.0.0.1:8787.
///
/// `backend_id` is forwarded as `--backend-id <id>` so the cloud's
/// per-backend routing can key this daemon's tunnel. Callers pass
/// "host" for single-backend setups, "win" when this daemon is the
/// Windows-native half of Backend::Dual.
/// Manifest platform key for the *shell* (Tauri exe) running on this
/// host. Distinct from the daemon's runtime platform under WSL: the
/// daemon is linux-amd64 but the shell is windows-amd64. Used for
/// --shell-platform when spawning solud.
pub fn host_shell_platform() -> &'static str {
    #[cfg(all(target_os = "windows", target_arch = "x86_64"))]
    {
        "windows-amd64"
    }
    #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
    {
        "linux-amd64"
    }
    #[cfg(all(target_os = "macos", target_arch = "x86_64"))]
    {
        "darwin-amd64"
    }
    #[cfg(all(target_os = "macos", target_arch = "aarch64"))]
    {
        "darwin-arm64"
    }
    #[cfg(not(any(
        all(target_os = "windows", target_arch = "x86_64"),
        all(target_os = "linux", target_arch = "x86_64"),
        all(target_os = "macos", target_arch = "x86_64"),
        all(target_os = "macos", target_arch = "aarch64"),
    )))]
    {
        ""
    }
}

// ---------------------------------------------------------------------
// Daemon log + watchdog (host-native backend)
//
// Two gaps this closes, both found the hard way. A Tauri release build
// on Windows discards the sidecar's stdout/stderr, so a daemon that died
// left no trace at all — the only evidence was the absence of a daemon.
// And nothing ever watched the sidecar: it was spawned once, and if it
// died it stayed dead until the user noticed the "Daemon unreachable"
// banner and restarted the whole app. The WSL backend has had a
// supervisor loop in its start script since day one; host-native never
// did, so the same crash is invisible on one backend and a hard outage
// on the other.
//
// So: every daemon we start writes to a real file, and a watchdog brings
// it back when it dies.
// ---------------------------------------------------------------------

/// Rotate the daemon log once it passes this; TWO generations are kept, as
/// `daemon.log.1` and `daemon.log.2`. Rotate rather than truncate — the bytes
/// worth having are almost always the tail of the run that just crashed, and
/// the second generation is what keeps a crash LOOP from overwriting the first
/// failure (the only one that usually explains the rest) with the tenth.
///
/// The same cap and the same two-generation bound apply everywhere the daemon's
/// output is kept: this supervisor's `daemon.log`, the WSL launcher's
/// `daemon.log`, and the daemon's own `solud.log` (durableLogMaxBytes). Anything
/// that collects or prunes crash logs has to know about `.2`.
const DAEMON_LOG_MAX_BYTES: u64 = 8 * 1024 * 1024;

/// How often the watchdog looks at the daemon it started.
const SUPERVISOR_POLL: Duration = Duration::from_millis(500);

/// Respawn backoff bounds. The cap matters more than the floor: a daemon
/// that cannot boot at all (torn state file, port squatter) has to keep
/// retrying slowly *forever* rather than either hammering the machine or
/// giving up permanently the way a tripped systemd StartLimit does.
const SUPERVISOR_BACKOFF_MIN: Duration = Duration::from_millis(500);
const SUPERVISOR_BACKOFF_MAX: Duration = Duration::from_secs(30);

/// A daemon that stays up this long has recovered — reset the backoff.
const SUPERVISOR_STABLE_AFTER: Duration = Duration::from_secs(60);

/// Grace for a daemon that exited *cleanly*. A self-update spawns its own
/// successor, waits for it to report a good boot, then exits 0
/// (ExitCodeRestartForUpdate). Respawning into that handoff would race the
/// successor for :8787 and one of us would lose, so give the successor
/// time to show up before assuming nobody is coming. Comfortably over the
/// daemon's own 25s successorBootTimeout.
const SUPERVISOR_HANDOFF_GRACE: Duration = Duration::from_secs(35);

/// Where shell-side artifacts live. Windows: `%LOCALAPPDATA%\soluterminal`,
/// already home to launch.log and start-wsl.sh. Unix: the same
/// `~/.local/share/soluterminal` the WSL start script writes its daemon.log
/// into — so "where are the daemon logs?" has one answer per platform
/// rather than one per backend.
fn solu_data_dir() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        return std::env::var_os("LOCALAPPDATA").map(|d| PathBuf::from(d).join("soluterminal"));
    }
    #[cfg(not(windows))]
    {
        if let Some(x) = std::env::var_os("XDG_DATA_HOME") {
            return Some(PathBuf::from(x).join("soluterminal"));
        }
        std::env::var_os("HOME")
            .map(|h| PathBuf::from(h).join(".local").join("share").join("soluterminal"))
    }
}

pub fn daemon_log_path() -> Option<PathBuf> {
    Some(solu_data_dir()?.join("daemon.log"))
}

/// Open the daemon log for appending, rotating first if it has grown past
/// the cap. Append (not truncate) so a crash and the restart that follows
/// it land in the same file, in order — the WSL script used to truncate on
/// every launch, which is exactly why its logs never survived to explain
/// anything; `wsl_start_script_body` now rotates and appends the same way.
/// Best-effort: `None` means the caller falls back to inherit.
fn open_daemon_log() -> Option<std::fs::File> {
    let path = daemon_log_path()?;
    std::fs::create_dir_all(path.parent()?).ok()?;
    rotate_daemon_log_file(&path, DAEMON_LOG_MAX_BYTES);
    std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&path)
        .ok()
}

/// Shift daemon.log generations once the live file has outgrown `max_bytes`:
/// `.1` -> `.2`, live -> `.1`. Two archived generations, the same bound the
/// WSL launcher and the daemon's own solud.log keep — a single generation used
/// to mean two crashes in a row overwrote the first one's tail, and a crash
/// loop is exactly when you need the FIRST failure, not the latest.
///
/// Every step is best-effort: losing an archive is worth far less than losing
/// the ability to keep logging at all.
fn rotate_daemon_log_file(path: &std::path::Path, max_bytes: u64) {
    let oversized = std::fs::metadata(path)
        .map(|m| m.len() > max_bytes)
        .unwrap_or(false);
    if !oversized {
        return;
    }
    let gen1 = path.with_extension("log.1");
    let gen2 = path.with_extension("log.2");
    let _ = std::fs::remove_file(&gen2);
    let _ = std::fs::rename(&gen1, &gen2);
    let _ = std::fs::rename(path, &gen1);
}

/// Write one supervisor breadcrumb into the same log the daemon writes to,
/// so a crash and the watchdog's reaction to it read as one story.
fn daemon_log_line(msg: &str) {
    if let Some(mut f) = open_daemon_log() {
        let _ = writeln!(f, "[{}] {msg}", chrono_like_now());
    }
}

/// Build the daemon command. Every spawn — first boot and every respawn —
/// goes through here so a supervised daemon can never drift from the one
/// the app started with.
///
/// `respawn_reason` is set only when the watchdog is bringing the daemon back
/// after an unexpected death. It rides in as env so the new daemon can report
/// its predecessor's demise to the cloud — the dead daemon obviously couldn't,
/// and fatal deaths were previously invisible fleet-wide (the crash sink only
/// ever heard about panics a daemon SURVIVED). The daemon unsets both vars
/// after reading them so pane agents and update successors don't inherit a
/// stale confession.
fn spawn_solud(
    bin: &Path,
    backend_id: &str,
    addr: Option<&str>,
    respawn_reason: Option<&str>,
) -> std::io::Result<Child> {
    let mut cmd = Command::new(bin);
    // None keeps the daemon's built-in default (127.0.0.1:8787).
    if let Some(addr) = addr {
        cmd.args(["--addr", addr]);
    }
    if let Some(reason) = respawn_reason {
        cmd.env("SOLU_RESPAWNED_AFTER", reason);
        if let Some(log) = daemon_log_path() {
            // The predecessor's dying words (panic traceback, fatal log) are
            // in this file; the successor attaches its tail to the report.
            cmd.env("SOLU_PREV_LOG", log);
        }
    }
    // Pass --relay so the daemon auto-loads the user's claim.json and
    // enables cloud-proxied voice / push. Safe without a claim: daemon
    // logs + continues with cloud features disabled.
    // Pass --backend-id so if a sibling daemon also registers (Dual
    // mode), the cloud keys each tunnel separately.
    cmd.args(relay_args());
    cmd.args(["--backend-id", backend_id]);
    // Plumb our own (Tauri-shell) version into solud so its updater
    // can detect when a new NSIS installer ships and surface the
    // shell-update badge in the web UI. Baked at compile time by
    // build.rs from tauri.conf.json (Cargo.toml's version is stale).
    cmd.args(["--shell-version", env!("SOLU_SHELL_VERSION")]);
    // Shell PLATFORM is host-native here — the shell and daemon both
    // run on the same OS. The flag exists primarily for the WSL spawn
    // path where shell≠daemon platform; we still pass it for clarity
    // and future cross-arch work.
    cmd.args(["--shell-platform", host_shell_platform()]);
    match open_daemon_log().and_then(|f| f.try_clone().ok().map(|g| (f, g))) {
        Some((out, err)) => {
            cmd.stdout(Stdio::from(out)).stderr(Stdio::from(err));
        }
        // No writable log dir. Inherit — worthless in a release build,
        // but it is what we did before and it costs nothing.
        None => {
            cmd.stdout(Stdio::inherit()).stderr(Stdio::inherit());
        }
    }
    // Suppress the console window the daemon would otherwise pop up
    // on Windows — same CREATE_NO_WINDOW treatment as the WSL-path
    // wsl.exe spawns. Unix ignores this call (cfg-gated).
    #[cfg(windows)]
    {
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let child = cmd.spawn()?;
    #[cfg(windows)]
    job::assign(&child);
    Ok(child)
}

/// What the watchdog needs from the outside world. Injected rather than
/// hardcoded so the loop can be tested against a throwaway process and a
/// fake health probe instead of a real daemon on a real port — this code
/// only earns its keep if it is known to work, and "spawn solud and hope"
/// is how we got here.
struct SupervisorCfg {
    /// Spawn the daemon. The argument is the respawn reason — `Some` with a
    /// description of the death when the watchdog is reviving after a crash
    /// (so the new daemon can report its predecessor), `None` otherwise.
    spawn: Box<dyn Fn(Option<&str>) -> std::io::Result<Child> + Send>,
    healthy: Box<dyn Fn() -> bool + Send>,
}

/// Live handle on a watchdog thread. Dropping the `SidecarGuard` that
/// owns this stops the watchdog and takes the daemon down with it.
pub struct SupervisorHandle {
    shutdown: Arc<AtomicBool>,
    child: Arc<Mutex<Option<Child>>>,
}

impl SupervisorHandle {
    /// Stop watching, then kill the daemon we're currently running.
    /// Order is load-bearing: set the flag *before* the kill, or the
    /// watchdog races us, sees the exit, and resurrects the very daemon
    /// we are trying to shut down.
    fn stop(&self) {
        self.shutdown.store(true, Ordering::SeqCst);
        if let Ok(mut slot) = self.child.lock() {
            if let Some(mut c) = slot.take() {
                let _ = c.kill();
                let _ = c.wait();
            }
        }
    }
}

enum DaemonState {
    Alive,
    JustExited(std::process::ExitStatus),
    Gone,
}

fn poll_child(slot: &Arc<Mutex<Option<Child>>>) -> DaemonState {
    let Ok(mut g) = slot.lock() else {
        return DaemonState::Gone;
    };
    let Some(c) = g.as_mut() else {
        return DaemonState::Gone;
    };
    match c.try_wait() {
        Ok(None) => DaemonState::Alive,
        Ok(Some(st)) => {
            g.take();
            DaemonState::JustExited(st)
        }
        // Can't wait on it — treat as gone rather than watch a process
        // we've lost track of.
        Err(_) => {
            g.take();
            DaemonState::Gone
        }
    }
}

/// Sleep in slices so a quitting app doesn't have to wait out a 30s
/// backoff before the watchdog notices. Returns false once shutdown is
/// signalled, at which point the caller must return immediately.
fn nap(total: Duration, shutdown: &AtomicBool) -> bool {
    const SLICE: Duration = Duration::from_millis(100);
    let mut left = total;
    loop {
        if shutdown.load(Ordering::SeqCst) {
            return false;
        }
        if left.is_zero() {
            return true;
        }
        let step = SLICE.min(left);
        thread::sleep(step);
        left -= step;
    }
}

fn start_supervisor(cfg: SupervisorCfg, child: Child) -> SupervisorHandle {
    let shutdown = Arc::new(AtomicBool::new(false));
    let slot = Arc::new(Mutex::new(Some(child)));
    let handle = SupervisorHandle {
        shutdown: shutdown.clone(),
        child: slot.clone(),
    };
    thread::spawn(move || supervise(cfg, shutdown, slot));
    handle
}

/// The production wiring: respawn the same binary with the same argv, and
/// treat "our backend answers on its address" as health.
fn solud_supervisor(bin: PathBuf, backend_id: String, addr: String, explicit_addr: bool) -> SupervisorCfg {
    let health_backend = backend_id.clone();
    let health_addr = addr.clone();
    SupervisorCfg {
        spawn: Box::new(move |reason| {
            spawn_solud(&bin, &backend_id, explicit_addr.then_some(addr.as_str()), reason)
        }),
        healthy: Box::new(move || require_backend(&health_addr, &health_backend).is_ok()),
    }
}

/// Watch the daemon and restart it when it dies.
///
/// Deliberately health-driven rather than purely exit-driven, because
/// "our child exited" and "there is no daemon" are different questions
/// and only the second one is ours to fix:
///
///   - child alive          → nothing to do. A daemon that is up but
///                            wedged is the health banner's problem;
///                            killing it could destroy in-flight work.
///   - child gone, port up  → something else is serving (an update
///                            successor, or a daemon the user started by
///                            hand). Leave it alone and keep watching.
///   - child gone, port down → respawn, with backoff.
fn supervise(cfg: SupervisorCfg, shutdown: Arc<AtomicBool>, slot: Arc<Mutex<Option<Child>>>) {
    let mut backoff = SUPERVISOR_BACKOFF_MIN;
    let mut running_since = Some(Instant::now());
    // Set when our daemon exits cleanly: until this deadline we assume a
    // successor it spawned is on its way and refuse to compete for the port.
    let mut handoff_until: Option<Instant> = None;
    // What we last saw happen to the daemon, phrased for the crash report the
    // replacement will file. Cleared when a successor takes the port (that
    // death was an orderly handoff, not a casualty).
    let mut death: Option<String> = None;

    loop {
        if !nap(SUPERVISOR_POLL, &shutdown) {
            return;
        }

        match poll_child(&slot) {
            DaemonState::Alive => {
                if running_since.is_some_and(|t| t.elapsed() >= SUPERVISOR_STABLE_AFTER) {
                    backoff = SUPERVISOR_BACKOFF_MIN;
                    running_since = None;
                }
                continue;
            }
            DaemonState::JustExited(st) => {
                daemon_log_line(&format!("supervisor: daemon exited ({st})"));
                death = Some(format!("daemon exited ({st})"));
                handoff_until = st
                    .success()
                    .then(|| Instant::now() + SUPERVISOR_HANDOFF_GRACE);
            }
            DaemonState::Gone => {
                if death.is_none() {
                    death = Some("daemon process lost (could not be waited on)".into());
                }
            }
        }

        if (cfg.healthy)() {
            if handoff_until.take().is_some() {
                daemon_log_line("supervisor: the daemon port was picked up by a successor — standing by");
            }
            death = None;
            backoff = SUPERVISOR_BACKOFF_MIN;
            continue;
        }

        if let Some(until) = handoff_until {
            if Instant::now() < until {
                continue;
            }
            daemon_log_line("supervisor: nothing took the daemon port within the handoff grace — respawning");
            handoff_until = None;
        }

        if !nap(backoff, &shutdown) {
            return;
        }
        match (cfg.spawn)(death.as_deref()) {
            Ok(child) => {
                daemon_log_line(&format!("supervisor: respawned daemon (pid {})", child.id()));
                if let Ok(mut g) = slot.lock() {
                    *g = Some(child);
                }
                death = None;
                running_since = Some(Instant::now());
            }
            Err(e) => daemon_log_line(&format!("supervisor: respawn failed: {e}")),
        }
        backoff = (backoff * 2).min(SUPERVISOR_BACKOFF_MAX);
    }
}

pub fn ensure_daemon_running(backend_id: &str) -> Result<DaemonStart, String> {
    ensure_daemon_running_on(backend_id, DEFAULT_DAEMON_ADDR, false)
}

/// ensure_daemon_running for a daemon on a specific loopback address. With
/// `explicit_addr` the address is passed as `--addr` (Dual's Windows backend
/// on DUAL_WIN_ADDR); otherwise the daemon's own default is used.
pub fn ensure_daemon_running_on(
    backend_id: &str,
    addr: &str,
    explicit_addr: bool,
) -> Result<DaemonStart, String> {
    let local_url = format!("http://{addr}");

    if port_alive(addr) {
        require_backend(addr, backend_id)?;
        eprintln!("desktop: daemon already running on {addr} — not spawning a sidecar");
        return Ok(DaemonStart {
            guard: SidecarGuard::empty(),
            initial_url: local_url,
            alt_url: None,
        });
    }

    let bin = match find_solud() {
        Some(p) => p,
        None => {
            return Err("solud binary not found. Looked: next to the Tauri exe, \
             ~/wsl-projects/soluterminal/solud, and $PATH. Install via \
             `curl -fsSL https://github.com/soluterminal/soluterminal-releases/releases/latest/download/install.sh | sh`"
                .into())
        }
    };

    eprintln!("desktop: spawning sidecar {}", bin.display());
    daemon_log_line(&format!(
        "supervisor: starting {} (backend {backend_id})",
        bin.display()
    ));
    let child =
        spawn_solud(&bin, backend_id, explicit_addr.then_some(addr), None).map_err(|e| format!("spawn {}: {}", bin.display(), e))?;

    let deadline = Instant::now() + Duration::from_secs(5);
    let mut ready = false;
    while Instant::now() < deadline {
        if require_backend(addr, backend_id).is_ok() {
            ready = true;
            break;
        }
        thread::sleep(Duration::from_millis(100));
    }
    if ready {
        eprintln!("desktop: sidecar ready on {addr}");
    } else {
        // Not fatal, and no longer permanent: hand it to the watchdog
        // either way. If it never binds, the watchdog keeps retrying
        // instead of leaving the user with a dead daemon and a banner.
        eprintln!(
            "desktop: sidecar didn't bind within 5s; window may show a connection error briefly"
        );
        daemon_log_line("supervisor: daemon didn't bind within 5s — watching anyway");
    }

    Ok(DaemonStart {
        guard: SidecarGuard {
            child: None,
            supervisors: vec![start_supervisor(
                solud_supervisor(bin, backend_id.to_string(), addr.to_string(), explicit_addr),
                child,
            )],
            #[cfg(windows)]
            proxy: None,
            #[cfg(windows)]
            extras: Vec::new(),
            #[cfg(windows)]
            extra_proxies: Vec::new(),
        },
        initial_url: local_url,
        alt_url: None,
    })
}

fn port_alive(addr: &str) -> bool {
    let Ok(sa) = addr.parse::<SocketAddr>() else {
        return false;
    };
    TcpStream::connect_timeout(&sa, Duration::from_millis(250)).is_ok()
}

/// What actually answers on a loopback port, judged from one raw exchange.
///
/// `port_alive` only proves a connect succeeded. Under WSL's Consomme
/// networking that is not proof of a listener: after a Linux listener
/// closes, Windows keeps its localhost forwarding reservation for a while,
/// so a connect succeeds and is then reset or closed with no reply. Treating
/// that as "our proxy is already running" sent the shell down the identity
/// check, which failed, and dropped the user to Windows-only.
#[cfg(any(windows, test))]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PortOccupant {
    /// Connection refused: nothing holds the port.
    Free,
    /// Bytes came back: a real listener answers (ours or someone else's).
    Answers,
    /// Connected, then reset or closed with no reply: a forwarding
    /// reservation with nothing behind it.
    Reservation,
    /// Connected, no reply within the budget: busy or wedged, not judged.
    Silent,
}

#[cfg(any(windows, test))]
fn classify_port_occupant(addr: &str, reply_budget: Duration) -> PortOccupant {
    let Ok(sa) = addr.parse::<SocketAddr>() else {
        return PortOccupant::Free;
    };
    let mut stream = match TcpStream::connect_timeout(&sa, Duration::from_millis(250)) {
        Ok(stream) => stream,
        Err(e) if e.kind() == io::ErrorKind::ConnectionRefused => return PortOccupant::Free,
        // A loopback connect that neither succeeds nor is refused is not a
        // listener we can talk to yet.
        Err(_) => return PortOccupant::Silent,
    };
    let _ = stream.set_read_timeout(Some(reply_budget));
    let _ = stream.set_write_timeout(Some(reply_budget));
    // No token: this may be a stranger's listener, and a reply of any kind
    // is all we need to tell a listener from a reservation.
    let request = b"GET /api/health?identity=1 HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n";
    if let Err(e) = stream.write_all(request) {
        return match e.kind() {
            io::ErrorKind::ConnectionReset
            | io::ErrorKind::ConnectionAborted
            | io::ErrorKind::BrokenPipe => PortOccupant::Reservation,
            _ => PortOccupant::Silent,
        };
    }
    let mut first = [0u8; 1];
    match stream.read(&mut first) {
        Ok(0) => PortOccupant::Reservation,
        Ok(_) => PortOccupant::Answers,
        Err(e) => match e.kind() {
            io::ErrorKind::ConnectionReset | io::ErrorKind::ConnectionAborted => {
                PortOccupant::Reservation
            }
            _ => PortOccupant::Silent,
        },
    }
}

#[cfg(any(windows, test))]
#[derive(Debug, PartialEq, Eq)]
enum ReservationWait {
    /// The port is free now; start normally and bind it.
    Freed,
    /// A real listener answers now; judge it with the identity check.
    Answers,
    /// Still a reservation (or silent) at the deadline.
    Persisting,
}

/// Wait, within `deadline`, for a suspected forwarding reservation to resolve
/// into either a free port or a real listener. Returns at once when the first
/// look already shows a listener, so a genuine foreign occupant is never
/// waited on.
#[cfg(any(windows, test))]
fn wait_out_forwarding_reservation(
    mut classify: impl FnMut() -> PortOccupant,
    mut pause: impl FnMut(Duration),
    deadline: Instant,
) -> ReservationWait {
    loop {
        match classify() {
            PortOccupant::Free => return ReservationWait::Freed,
            PortOccupant::Answers => return ReservationWait::Answers,
            PortOccupant::Reservation | PortOccupant::Silent => {}
        }
        if Instant::now() >= deadline {
            return ReservationWait::Persisting;
        }
        pause(Duration::from_millis(500));
    }
}

/// How long startup waits for a Consomme reservation on the proxy port to
/// clear. Measured clears were a few seconds; the WSL start budget is far
/// larger, and a false Windows-only fallback costs the user their panes.
#[cfg(any(windows, test))]
const FORWARDING_RESERVATION_WAIT: Duration = Duration::from_secs(20);

fn daemon_health_with_token(addr: &str, token: Option<&str>) -> Result<serde_json::Value, String> {
    crate::daemon_health::probe(addr, token)
}

fn require_backend(addr: &str, expected: &str) -> Result<(), String> {
    require_backend_with_token(addr, expected, None)
}

fn require_backend_health_with_token(
    addr: &str,
    expected: &str,
    token: Option<&str>,
) -> Result<serde_json::Value, String> {
    check_backend_identity(daemon_health_with_token(addr, token)?, addr, expected)
}

/// Budget for every WSL identity probe. These probes decide whether the shell
/// reuses the running WSL daemon or runs start-wsl.sh to replace it, so they
/// must outlast a slow-but-healthy daemon: /api/health took 2-6s on a loaded
/// host, and the 800ms default turned that into "unreachable" (27 of the 28
/// probe failures in one launch.log), which is how a healthy daemon got
/// replaced dozens of times a day.
#[cfg(any(windows, test))]
const WSL_IDENTITY_PROBE_TIMEOUT: Duration = Duration::from_secs(10);

/// Windows-side loopback addresses that can reach the WSL daemon without the
/// VM IP. Mirrored networking mirrors 127.0.0.1; in NAT mode WSL's localhost
/// forwarding (wslrelay) may bind only the IPv6 side — on the machine this was
/// diagnosed on, [::1]:8787 reached the WSL daemon and 127.0.0.1:8787 did not.
/// Neither needs a wsl.exe call, which is what the VM-IP lookup is, and that
/// lookup timed out at 5s under load and left the shell with no route to a
/// healthy daemon, so it ran start-wsl.sh against it. A Windows-native daemon
/// on either address is rejected by the backendID identity check.
#[cfg(any(windows, test))]
const WSL_LOOPBACK_CANDIDATES: [&str; 2] = ["127.0.0.1:8787", "[::1]:8787"];

/// Loopback reaches the WSL daemon through WSL's localhost forwarding, which
/// carries every proxied connection over its own Hyper-V socket (hv_sock)
/// channel. With ~70 panes that is hundreds of long-lived channels, and under
/// reconnect churn the host ran out of them (2026-09-24: ~1,600 channels, new
/// WSL terminals failed until a reboot). The VM IP goes over the virtual NIC
/// and costs none, so startup keeps waiting this long for the VM IP before it
/// settles for a loopback route that answered first.
#[cfg(any(windows, test))]
const WSL_VM_ROUTE_GRACE: Duration = Duration::from_secs(10);

/// How often a proxy that did settle for loopback re-probes the VM IP.
#[cfg(any(windows, test))]
const WSL_VM_ROUTE_REHOME_EVERY: Duration = Duration::from_secs(60);

#[cfg(any(windows, test))]
fn is_wsl_loopback_route(addr: &str) -> bool {
    WSL_LOOPBACK_CANDIDATES.contains(&addr)
}

/// Choose the proxy upstream from the candidates that answered one probe
/// pass, in preference order. A direct (VM-IP) route always wins. A loopback
/// route is taken only when there is no VM IP to prefer, or when it has been
/// the only answer for WSL_VM_ROUTE_GRACE (e.g. mirrored networking, where the
/// VM IP does not loop back). `None` means keep probing. Before this, the
/// first route to answer won for the whole session, so a VM-IP probe that
/// failed once mid-restart pinned every connection to localhost forwarding.
#[cfg(any(windows, test))]
fn choose_wsl_route(
    answered: &[String],
    vm_ip_known: bool,
    loopback_only_for: Option<Duration>,
) -> Option<String> {
    if let Some(direct) = answered.iter().find(|a| !is_wsl_loopback_route(a)) {
        return Some(direct.clone());
    }
    let loopback = answered.first()?;
    if !vm_ip_known || matches!(loopback_only_for, Some(waited) if waited >= WSL_VM_ROUTE_GRACE) {
        return Some(loopback.clone());
    }
    None
}

/// The startup reuse loop: probe every WSL candidate until one answers as a
/// current WSL daemon on a route `choose_wsl_route` accepts, and return that
/// route. `None` sends the caller down the path that runs start-wsl.sh, which
/// kills and replaces whatever holds :8787 — so it is returned only when no
/// candidate answered at all or one answered as a stale daemon. A slow probe
/// that still answers correctly is an answer, not an outage. The probe, pause
/// and log are injected so tests can drive this exact loop off Windows.
#[cfg(any(windows, test))]
fn find_reusable_wsl_daemon(
    candidates: &[String],
    vm_ip_known: bool,
    mut probe: impl FnMut(&str) -> Result<(), String>,
    mut pause: impl FnMut(Duration),
    mut log: impl FnMut(&str),
) -> Option<String> {
    let mut loopback_since: Option<Instant> = None;
    let mut first_pass = true;
    loop {
        let mut answered = Vec::new();
        for candidate in candidates {
            match probe(candidate) {
                Ok(()) => answered.push(candidate.clone()),
                Err(e) if e.starts_with("stale WSL daemon") => {
                    log(&format!("not reusing WSL daemon at {candidate}: {e}"));
                    return None;
                }
                Err(e) if first_pass => {
                    log(&format!("WSL health probe at {candidate} failed: {e}"))
                }
                Err(_) => {}
            }
        }
        first_pass = false;
        if answered.is_empty() {
            return None; // nothing to reuse: the caller spawns
        }
        if answered.iter().all(|a| is_wsl_loopback_route(a)) {
            loopback_since.get_or_insert_with(Instant::now);
        }
        if let Some(route) =
            choose_wsl_route(&answered, vm_ip_known, loopback_since.map(|t| t.elapsed()))
        {
            log(&format!("reusing existing WSL daemon at {route}"));
            return Some(route);
        }
        pause(Duration::from_millis(500));
    }
}

/// The VM-IP route a proxy currently on loopback should move back to.
#[cfg(any(windows, test))]
fn wsl_rehome_target<'a>(current: &str, vm_route: Option<&'a str>) -> Option<&'a str> {
    let vm = vm_route?;
    (is_wsl_loopback_route(current) && !is_wsl_loopback_route(vm) && vm != current).then_some(vm)
}

fn check_backend_identity(
    health: serde_json::Value,
    addr: &str,
    expected: &str,
) -> Result<serde_json::Value, String> {
    let actual = health
        .get("backendID")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    if actual == expected {
        return Ok(health);
    }
    let pid = health
        .get("pid")
        .map(|v| v.to_string())
        .unwrap_or_else(|| "unknown".into());
    let version = health
        .get("version")
        .and_then(|v| v.as_str())
        .unwrap_or("unknown");
    Err(format!(
        "daemon at {addr} is backend {actual:?}, expected {expected:?} (pid={pid}, version={version})"
    ))
}

fn require_backend_with_token(
    addr: &str,
    expected: &str,
    token: Option<&str>,
) -> Result<(), String> {
    require_backend_health_with_token(addr, expected, token).map(|_| ())
}

#[cfg(any(windows, test))]
fn require_current_wsl_backend_with_token(
    addr: &str,
    token: Option<&str>,
) -> Result<(), String> {
    let health = crate::daemon_health::probe_with_timeout(addr, token, WSL_IDENTITY_PROBE_TIMEOUT)?;
    let health = check_backend_identity(health, addr, BACKEND_ID_WSL)?;
    if let Some(reason) = stale_wsl_daemon_reason(&health) {
        return Err(format!("stale WSL daemon at {addr}: {reason}"));
    }
    Ok(())
}

#[cfg(any(windows, test))]
fn is_stale_wsl_error(err: &str) -> bool {
    err.starts_with("stale WSL daemon")
}

/// Accept an authenticated WSL daemon that is merely older than this shell.
///
/// After an installer update the shell is new and the WSL daemon (with every
/// running pane) is still the previous version. Treating that as an identity
/// failure dropped users to Windows-only with a warning — their WSL panes
/// "disappeared" until the daemon caught up and they restarted. Spawning a
/// replacement is no better: start-wsl.sh would fight the running daemon.
/// So keep it connected and let `on_stale` ask its own updater to catch up.
#[cfg(any(windows, test))]
fn require_reusable_wsl_backend_with(
    addr: &str,
    token: Option<&str>,
    on_stale: &dyn Fn(&str, Option<&str>, &str),
) -> Result<(), String> {
    match require_current_wsl_backend_with_token(addr, token) {
        Err(e) if is_stale_wsl_error(&e) => {
            on_stale(addr, token, &e);
            Ok(())
        }
        other => other,
    }
}

/// Ask a stale WSL daemon to update through its normal, guarded apply path,
/// at most once per launch and off the startup path. Its own admission rules
/// (busy panes, workflow drain, rollback holds) decide when; we never force,
/// kill or replace it. Failure only means it updates later on its own.
#[cfg(windows)]
fn request_wsl_upgrade_once(addr: &str, token: Option<&str>, reason: &str) {
    use std::sync::atomic::{AtomicBool, Ordering};
    static REQUESTED: AtomicBool = AtomicBool::new(false);
    if REQUESTED.swap(true, Ordering::SeqCst) {
        return;
    }
    launch_log(&format!(
        "keeping WSL daemon at {addr} connected ({reason}); asking it to update"
    ));
    let addr = addr.to_string();
    let token = token.map(str::to_string);
    thread::spawn(move || {
        match crate::wsl_update::request_guarded_upgrade(&addr, token.as_deref()) {
            Ok(outcome) => launch_log(&format!(
                "WSL backend upgrade at {addr}: {outcome:?}; preserving existing sessions"
            )),
            Err(e) => launch_log(&format!(
                "WSL backend upgrade at {addr} not started ({e}); it stays connected and updates on its own schedule"
            )),
        }
    });
}

/// Identity-probe one WSL candidate address. A 401 is proof of life, not of
/// absence: the daemon answered and we presented no token (the wsl.exe read
/// timed out on a busy host) or a stale one. Re-read the token patiently once
/// before that answer counts as a failure, because a failure here is what
/// sends the shell down the path that runs start-wsl.sh against a daemon that
/// was fine.
#[cfg(windows)]
fn probe_wsl_candidate(addr: &str) -> Result<(), String> {
    let token = read_wsl_token().ok();
    match require_reusable_wsl_backend_with(addr, token.as_deref(), &request_wsl_upgrade_once) {
        Err(e) if e.contains("health returned HTTP 401") => {
            launch_log(&format!(
                "{addr} answered 401; re-reading the WSL token before judging it"
            ));
            let token = read_wsl_token_patiently();
            if let Err(te) = &token {
                launch_log(&format!("patient WSL token read failed: {te}"));
            }
            require_reusable_wsl_backend_with(
                addr,
                token.ok().as_deref(),
                &request_wsl_upgrade_once,
            )
        }
        other => other,
    }
}

/// Read-only check used while running Windows-only after a WSL failure: is a
/// genuine WSL daemon answering on the loopback forward again? Never starts
/// the WSL VM (wsl.exe is only invoked for the token once the port is open)
/// and never asks the daemon to update.
#[cfg(windows)]
fn wsl_backend_answers_now() -> bool {
    WSL_LOOPBACK_CANDIDATES.iter().any(|addr| {
        if !port_alive(addr) {
            return false;
        }
        let token = read_wsl_token().ok();
        wsl_identity_usable(&require_current_wsl_backend_with_token(addr, token.as_deref()))
    })
}

/// A current or merely older WSL daemon is usable; anything else is not.
#[cfg(any(windows, test))]
fn wsl_identity_usable(result: &Result<(), String>) -> bool {
    match result {
        Ok(()) => true,
        Err(e) => is_stale_wsl_error(e),
    }
}

#[cfg(any(windows, test))]
const WSL_RECOVERY_POLL: Duration = Duration::from_secs(20);
#[cfg(any(windows, test))]
const WSL_RECOVERY_WATCH: Duration = Duration::from_secs(20 * 60);

/// Poll until `answers` reports WSL is back or the watch window closes.
/// Returns true when WSL came back.
#[cfg(any(windows, test))]
fn wait_for_wsl_recovery(
    window: Duration,
    poll: Duration,
    mut answers: impl FnMut() -> bool,
    mut pause: impl FnMut(Duration),
) -> bool {
    let mut waited = Duration::ZERO;
    while waited < window {
        pause(poll);
        waited += poll;
        if answers() {
            return true;
        }
    }
    false
}

/// After a Dual launch fell back to Windows-only, keep watching for WSL in
/// the background and call `on_ready` once it answers again, so the user can
/// get their WSL panes back without diagnosing anything.
#[cfg(windows)]
pub fn watch_for_wsl_recovery(on_ready: impl FnOnce() + Send + 'static) {
    thread::spawn(move || {
        if wait_for_wsl_recovery(
            WSL_RECOVERY_WATCH,
            WSL_RECOVERY_POLL,
            wsl_backend_answers_now,
            thread::sleep,
        ) {
            launch_log("WSL backend is answering again; offering to reconnect");
            on_ready();
        } else {
            launch_log("WSL recovery watch ended without WSL answering");
        }
    });
}

#[cfg(any(windows, test))]
fn stale_wsl_daemon_reason(health: &serde_json::Value) -> Option<String> {
    let version = health.get("version").and_then(|v| v.as_str())?;
    let required = env!("SOLU_SHELL_VERSION");
    if version_less_than(version, required) {
        if health
            .get("updateHoldVersion")
            .and_then(|v| v.as_str())
            .is_some_and(|held| held == required)
        {
            return None;
        }
        Some(format!("version {version} is older than bundled {required}"))
    } else {
        None
    }
}

#[cfg(any(windows, test))]
fn version_less_than(actual: &str, required: &str) -> bool {
    numeric_version_cmp(actual, required).is_some_and(|ordering| ordering.is_lt())
}

#[cfg(any(windows, test))]
pub(crate) fn numeric_version_cmp(actual: &str, required: &str) -> Option<std::cmp::Ordering> {
    let actual_parts: Vec<_> = actual.split('.').collect();
    let required_parts: Vec<_> = required.split('.').collect();
    let max = actual_parts.len().max(required_parts.len());
    let mut parsed_any = false;

    for idx in 0..max {
        let actual_part = numeric_version_part(actual_parts.get(idx).copied().unwrap_or("0"))?;
        let required_part = numeric_version_part(required_parts.get(idx).copied().unwrap_or("0"))?;
        parsed_any = true;
        match actual_part.cmp(&required_part) {
            std::cmp::Ordering::Equal => {}
            ordering => return Some(ordering),
        }
    }

    parsed_any.then_some(std::cmp::Ordering::Equal)
}

#[cfg(any(windows, test))]
pub(crate) fn numeric_version_part(part: &str) -> Option<u64> {
    let digits: String = part.chars().take_while(|c| c.is_ascii_digit()).collect();
    if digits.is_empty() {
        return Some(0);
    }
    digits.parse().ok()
}

/// What start-wsl.sh reported when the WSL port is held by something it will
/// not replace (report_port_blocked in the script). Every field comes from
/// inside WSL, mostly from the occupant's own /api/health, so it is display
/// data: the suggested stop command is built only from validated pieces.
#[cfg(any(windows, test))]
#[derive(Debug, Clone, PartialEq, Eq)]
struct WslPortBlock {
    /// A SoluTerminal daemon identified itself on the port ("port-held"), as
    /// opposed to our daemon failing to bind with nothing identifying itself
    /// ("bind-failed").
    identified: bool,
    reason: String,
    addr: String,
    backend: String,
    version: String,
    pid: Option<u32>,
    binary: String,
    /// The occupant runs in the distro start-wsl.sh ran in.
    visible: bool,
    cgroup: String,
    /// The distro start-wsl.sh ran in (the default distro).
    distro: String,
}

#[cfg(any(windows, test))]
fn status_text(value: &str) -> String {
    value.chars().filter(|c| !c.is_control()).take(300).collect()
}

#[cfg(any(windows, test))]
fn is_version_like(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 40
        && value.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '+'))
}

/// WSL distro names are letters, digits, '.', '_' and '-'. Nothing else is
/// ever put into a command we suggest.
#[cfg(any(windows, test))]
fn is_plain_distro_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
}

/// Parse start-wsl.sh's report. Only the two blocked states count; anything
/// else (missing, "starting", garbage) means keep waiting as before.
#[cfg(any(windows, test))]
fn parse_wsl_start_status(text: &str) -> Option<WslPortBlock> {
    let mut fields = std::collections::HashMap::new();
    for line in text.lines().take(32) {
        if let Some((key, value)) = line.split_once('=') {
            fields.insert(key.trim(), value.trim_end_matches('\r'));
        }
    }
    let identified = match *fields.get("state")? {
        "port-held" => true,
        "bind-failed" => false,
        _ => return None,
    };
    let text = |key: &str| fields.get(key).map(|v| status_text(v)).unwrap_or_default();
    Some(WslPortBlock {
        identified,
        reason: text("reason"),
        addr: text("addr"),
        backend: text("backend"),
        version: fields
            .get("version")
            .filter(|v| is_version_like(v))
            .map(|v| v.to_string())
            .unwrap_or_default(),
        pid: fields
            .get("pid")
            .and_then(|v| v.trim().parse::<u32>().ok())
            .filter(|pid| *pid > 0),
        binary: text("binary"),
        visible: fields.get("visible").map(|v| v.trim()) == Some("1"),
        cgroup: text("cgroup"),
        distro: fields
            .get("distro")
            .map(|d| d.trim())
            .filter(|d| is_plain_distro_name(d))
            .map(str::to_string)
            .unwrap_or_default(),
    })
}

/// Where the occupant runs, as far as the shell could tell.
#[cfg(any(windows, test))]
#[derive(Debug, Clone, PartialEq, Eq)]
struct WslPortHolder {
    distro: String,
    cgroup: String,
}

/// Find the distro the reported daemon runs in. WSL2 distros share one
/// network namespace but not a process table, so the launcher in the default
/// distro sees the port taken and no owner. `inspect(distro, pid)` returns
/// that pid's (owns a LISTEN socket on the port, exe, cmdline, cgroup) inside
/// one running distro. A distro counts only when the pid there owns the
/// listening socket AND is the same daemon binary, or at least a solud: pids
/// collide across distros, and a wrong match would point the user's stop
/// command at the wrong distro.
#[cfg(any(windows, test))]
fn find_wsl_port_holder(
    block: &WslPortBlock,
    running: &[String],
    mut inspect: impl FnMut(&str, u32) -> Option<(bool, String, String, String)>,
) -> Option<WslPortHolder> {
    if block.visible {
        return Some(WslPortHolder {
            distro: block.distro.clone(),
            cgroup: block.cgroup.clone(),
        });
    }
    let pid = block.pid?;
    running
        .iter()
        .map(|name| name.trim())
        .filter(|name| is_plain_distro_name(name) && !name.eq_ignore_ascii_case(&block.distro))
        .take(6)
        .find_map(|name| {
            let (listens, exe, cmdline, cgroup) = inspect(name, pid)?;
            let same_binary = !block.binary.is_empty() && exe.trim() == block.binary;
            (listens && (same_binary || cmdline.contains("solud"))).then(|| WslPortHolder {
                distro: name.to_string(),
                cgroup: status_text(cgroup.trim()),
            })
        })
}

/// A one-line PowerShell command that stops the occupant and nothing else we
/// can avoid. SIGTERM is the daemon's graceful shutdown: its terminal sessions
/// keep running in that distro. A systemd unit would restart a killed daemon,
/// so the installer's user unit is disabled instead, and any other unit
/// (whose restart policy is unknown) gets its whole distro stopped.
#[cfg(any(windows, test))]
fn wsl_stop_command(distro: &str, pid: Option<u32>, cgroup: &str) -> Option<String> {
    if !is_plain_distro_name(distro) {
        return None;
    }
    if cgroup.contains("/user@") && cgroup.ends_with("/soluterminal.service") {
        return Some(format!(
            "wsl -d {distro} -- systemctl --user disable --now soluterminal.service"
        ));
    }
    if cgroup.contains(".service") {
        return Some(format!("wsl --terminate {distro}"));
    }
    pid.map(|pid| format!("wsl -d {distro} -- kill {pid}"))
}

/// The error ensure_daemon_running_wsl returns for a held port. It starts
/// with "WSL port held:" (classify_wsl_fallback keys on it) and says which
/// distro holds the port and how to free it.
#[cfg(any(windows, test))]
fn describe_wsl_port_block(
    block: &WslPortBlock,
    holder: Option<&WslPortHolder>,
    bundled: &str,
) -> String {
    let port = block
        .addr
        .rsplit(':')
        .next()
        .filter(|p| !p.is_empty() && p.chars().all(|c| c.is_ascii_digit()))
        .unwrap_or("8787");
    let ours = if block.distro.is_empty() {
        "the default WSL distro".to_string()
    } else {
        format!("\"{}\"", block.distro)
    };
    if !block.identified {
        let reason = if block.reason.is_empty() { "it could not bind the port" } else { &block.reason };
        return format!(
            "WSL port held: port {port} is in use by another program, so the WSL background \
             service in {ours} cannot start ({reason})."
        );
    }
    let older = !block.version.is_empty() && version_less_than(&block.version, bundled);
    let mut facts = Vec::new();
    if !block.version.is_empty() {
        facts.push(format!("version {}", block.version));
    }
    facts.push(if block.backend.is_empty() {
        "no backend id".to_string()
    } else {
        format!("backend \"{}\"", block.backend)
    });
    if let Some(pid) = block.pid {
        facts.push(format!("pid {pid}"));
    }
    let detail = format!(
        "{} SoluTerminal service ({})",
        if older { "an older" } else { "another" },
        facts.join(", ")
    );
    let elsewhere = !matches!(holder, Some(h) if h.distro.is_empty() || h.distro.eq_ignore_ascii_case(&block.distro));
    let location = match holder {
        Some(_) if !elsewhere => " in the same WSL distro".to_string(),
        Some(h) => format!(" in the WSL distro \"{}\"", h.distro),
        None => " outside this WSL distro (most likely in another running WSL distro)".to_string(),
    };
    let mut msg = format!(
        "WSL port held: port {port} is in use by {detail}{location}, so the WSL background \
         service in {ours} cannot start."
    );
    if elsewhere {
        msg.push_str(&format!(
            " WSL distros share one network, so only one of them can serve port {port}."
        ));
    }
    match holder.and_then(|h| wsl_stop_command(&h.distro, block.pid, &h.cgroup)) {
        Some(cmd) => msg.push_str(&format!(" To stop it, run in PowerShell: {cmd}")),
        None => msg.push_str(
            " Run `wsl -l -v` to see which distros are running, then stop the old \
             SoluTerminal service in that distro.",
        ),
    }
    msg
}

/// How the wait for a freshly spawned WSL daemon ended.
#[cfg(any(windows, test))]
#[derive(Debug)]
enum SpawnedWslWait {
    Reachable(String),
    Blocked(WslPortBlock),
    GaveUp(String),
}

/// One probe pass over the candidates, run off the waiting thread.
#[cfg(any(windows, test))]
struct WslWaitPass {
    answered: Vec<String>,
    last_err: Option<String>,
    candidates: Vec<String>,
    have_vm_ip: bool,
}

/// Wait for the daemon start-wsl.sh is starting to answer on a route
/// `choose_wsl_route` accepts. The probes run on their own thread, because one
/// pass over three candidates can take 20s when two of them time out, and
/// `port_block` (start-wsl.sh's report) is read every 200ms meanwhile: a port
/// the new daemon can never bind ends the wait within a second of the report
/// instead of after `timeout`. An answer from a usable daemon always wins over
/// a report seen in the same tick. Returns the outcome and the final candidate
/// list (a VM IP resolved during the wait is prepended).
#[cfg(any(windows, test))]
#[allow(clippy::too_many_arguments)]
fn wait_for_spawned_wsl_daemon<P, R, L>(
    candidates: Vec<String>,
    have_vm_ip: bool,
    timeout: Duration,
    stale_bail: Duration,
    probe: P,
    mut resolve_vm_ip: R,
    mut port_block: impl FnMut() -> Option<WslPortBlock>,
    log: L,
) -> (SpawnedWslWait, Vec<String>)
where
    P: Fn(&str) -> Result<(), String> + Send + 'static,
    R: FnMut() -> Option<String> + Send + 'static,
    L: Fn(&str) + Send + Clone + 'static,
{
    use std::sync::mpsc;
    let stop = Arc::new(AtomicBool::new(false));
    let (tx, rx) = mpsc::channel::<WslWaitPass>();
    {
        let stop = stop.clone();
        let log = log.clone();
        let mut candidates = candidates.clone();
        let mut have_vm_ip = have_vm_ip;
        thread::spawn(move || {
            while !stop.load(Ordering::SeqCst) {
                // The VM-IP lookup is a wsl.exe call that times out on a
                // loaded host; keep trying rather than settle for loopback.
                if !have_vm_ip {
                    if let Some(ip) = resolve_vm_ip() {
                        log(&format!("wsl_ip = {ip} (resolved during the wait)"));
                        candidates.insert(0, format!("{ip}:8787"));
                        have_vm_ip = true;
                    }
                }
                let mut answered = Vec::new();
                let mut last_err = None;
                for candidate in &candidates {
                    if stop.load(Ordering::SeqCst) {
                        return;
                    }
                    match probe(candidate) {
                        Ok(()) => {
                            answered.push(candidate.clone());
                            if !is_wsl_loopback_route(candidate) {
                                break; // the preferred route answered
                            }
                        }
                        Err(e) => last_err = Some(e),
                    }
                }
                let pass = WslWaitPass {
                    answered,
                    last_err,
                    candidates: candidates.clone(),
                    have_vm_ip,
                };
                if tx.send(pass).is_err() {
                    return;
                }
                thread::sleep(Duration::from_millis(150));
            }
        });
    }

    let started = Instant::now();
    let deadline = started + timeout;
    let mut candidates = candidates;
    let mut last_err = String::from("not attempted");
    let mut loopback_since: Option<Instant> = None;
    let finish = |outcome: SpawnedWslWait, candidates: Vec<String>| {
        stop.store(true, Ordering::SeqCst);
        (outcome, candidates)
    };
    loop {
        match rx.recv_timeout(Duration::from_millis(200)) {
            Ok(pass) => {
                candidates = pass.candidates;
                if let Some(e) = pass.last_err {
                    last_err = e;
                }
                if !pass.answered.is_empty() && pass.answered.iter().all(|a| is_wsl_loopback_route(a)) {
                    loopback_since.get_or_insert_with(Instant::now);
                }
                if let Some(route) =
                    choose_wsl_route(&pass.answered, pass.have_vm_ip, loopback_since.map(|t| t.elapsed()))
                {
                    return finish(SpawnedWslWait::Reachable(route), candidates);
                }
                // A *stale* daemon owning :8787 is a different failure from a
                // merely slow one: our spawn can never win that bind.
                if started.elapsed() >= stale_bail && is_stale_wsl_error(&last_err) {
                    log(&format!(
                        "bailing early after {}s: :8787 is held by a daemon our spawn cannot replace ({last_err})",
                        started.elapsed().as_secs()
                    ));
                    break;
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
        if let Some(block) = port_block() {
            log(&format!(
                "start-wsl.sh reports the WSL port held after {}ms: {} backend={:?} version={} pid={} visible_in_{}={} ({})",
                started.elapsed().as_millis(),
                if block.identified { "port-held" } else { "bind-failed" },
                block.backend,
                if block.version.is_empty() { "unknown" } else { &block.version },
                block.pid.map(|p| p.to_string()).unwrap_or_else(|| "unknown".into()),
                if block.distro.is_empty() { "default" } else { &block.distro },
                block.visible,
                block.reason,
            ));
            return finish(SpawnedWslWait::Blocked(block), candidates);
        }
        if Instant::now() >= deadline {
            break;
        }
    }
    // Keep the "not reachable within" wording — classify_wsl_fallback in
    // lib.rs keys on it to pick the user-facing explanation.
    let msg = format!(
        "WSL daemon was not reachable within {}s via {:?}: {last_err}",
        started.elapsed().as_secs(),
        candidates
    );
    finish(SpawnedWslWait::GaveUp(msg), candidates)
}

/// Locate the solud binary. Tries, in order:
///   1. Next to the Tauri exe (the bundled-sidecar path once we add it)
///   2. The dev checkout path (~/wsl-projects/soluterminal/solud)
///   3. Anything on $PATH called `solud`
pub fn find_solud() -> Option<PathBuf> {
    let candidates = [
        adjacent_to_self(),
        dev_checkout_path(),
        which_in_path("solud"),
    ];
    for c in candidates.into_iter().flatten() {
        if c.is_file() {
            return Some(c);
        }
    }
    None
}

fn adjacent_to_self() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    let dir = exe.parent()?;
    let name = if cfg!(windows) { "solud.exe" } else { "solud" };
    Some(dir.join(name))
}

fn dev_checkout_path() -> Option<PathBuf> {
    // dirs::home_dir picks the right env var per platform (HOME on Unix,
    // USERPROFILE on Windows). The dev checkout layout is Unix-shaped —
    // on Windows this just won't find anything and we fall through to
    // PATH, which is fine; a bundled Windows app ships solud.exe
    // adjacent to the Tauri exe anyway.
    let home = dirs::home_dir()?;
    let name = if cfg!(windows) { "solud.exe" } else { "solud" };
    Some(home.join("wsl-projects/soluterminal").join(name))
}

fn which_in_path(name: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    for dir in std::env::split_paths(&path) {
        let full = dir.join(name);
        if full.is_file() {
            return Some(full);
        }
    }
    None
}

/// Launch the bundled Linux solud INSIDE WSL and run a Windows-side
/// TCP proxy so the Tauri webview's `http://127.0.0.1:8787` always
/// reaches it regardless of WSL2 localhost-forwarding health.
#[cfg(windows)]
/// Free bytes on the host system drive, via kernel32 directly (no crate).
#[cfg(windows)]
fn host_disk_space() -> Option<(u64, u64)> {
    use std::os::windows::ffi::OsStrExt;
    #[link(name = "kernel32")]
    extern "system" {
        fn GetDiskFreeSpaceExW(
            dir: *const u16,
            free_caller: *mut u64,
            total: *mut u64,
            free_total: *mut u64,
        ) -> i32;
    }
    let path: Vec<u16> = std::ffi::OsStr::new("C:\\")
        .encode_wide()
        .chain(std::iter::once(0))
        .collect();
    let (mut fc, mut t, mut ft) = (0u64, 0u64, 0u64);
    // SAFETY: valid NUL-terminated wide string, valid out pointers.
    unsafe {
        if GetDiskFreeSpaceExW(path.as_ptr(), &mut fc, &mut t, &mut ft) != 0 {
            Some((fc, t))
        } else {
            None
        }
    }
}

#[cfg(windows)]
fn append_host_disk_launcher_event(
    kind: &str,
    verdict: &HostDiskVerdict,
    measured: Option<(u64, u64, u64)>,
) {
    let (name, free, total, floor) = match verdict {
        HostDiskVerdict::Ok => {
            let (free, total, floor) = measured.unwrap_or((0, 0, 0));
            ("ok", free, Some(total), floor)
        }
        HostDiskVerdict::Warn { free, total, floor } => ("warn", *free, *total, *floor),
        HostDiskVerdict::Block { free, total, floor } => ("block", *free, *total, *floor),
    };
    let Some(path) = solu_data_dir().map(|d| d.join("host-disk.jsonl")) else {
        return;
    };
    let line = serde_json::json!({"at": iso_now(), "kind": kind, "verdict": name, "freeBytes": free, "totalBytes": total, "floorBytes": floor}).to_string();
    let _ = append_bounded_jsonl(&path, &line, 1024 * 1024);
}

#[cfg(windows)]
static HOST_DISK_GATE: Mutex<Option<(HostDiskVerdict, Instant)>> = Mutex::new(None);

#[cfg(windows)]
static HOST_DISK_BLOCK_REPORTED: AtomicBool = AtomicBool::new(false);

#[cfg(windows)]
fn host_disk_gate() -> HostDiskVerdict {
    host_disk_gate_with_refresh(false)
}

#[cfg(windows)]
fn host_disk_gate_with_refresh(refresh: bool) -> HostDiskVerdict {
    let mut cached = HOST_DISK_GATE.lock().unwrap();
    if let Some((v, at)) = cached.as_ref() {
        if !refresh && at.elapsed() < Duration::from_secs(30) {
            return v.clone();
        }
    }
    let verdict = match host_disk_space() {
        Some((free, total)) => {
            let json = solu_data_dir()
                .and_then(|d| std::fs::read_to_string(d.join("host-disk-policy.json")).ok());
            let mb = std::env::var("SOLU_HOST_DISK_MIN_FREE_MB").ok();
            let pct = std::env::var("SOLU_HOST_DISK_MIN_FREE_PCT").ok();
            host_disk_verdict_from_sources(
                free,
                Some(total),
                mb.as_deref(),
                pct.as_deref(),
                json.as_deref(),
            )
        }
        None => HostDiskVerdict::Ok,
    };
    *cached = Some((verdict.clone(), Instant::now()));
    verdict
}

#[cfg(windows)]
pub fn wsl_start_allowed() -> bool {
    recovery_should_spawn_wsl(&host_disk_gate_with_refresh(true))
}

/// Permit only bounded discovery/token reads below the floor, and only when
/// the default distribution is already running. Fail closed on list errors.
/// Never use this for a startup script, install, update or recovery restart.
#[cfg(windows)]
pub(crate) fn run_wsl_probe_bounded(args: &[&str], timeout: Duration) -> Result<std::process::Output, String> {
    run_wsl_probe_bounded_with_stdin(args, None, timeout)
}

/// run_wsl_probe_bounded with optional stdin for the command itself.
#[cfg(windows)]
fn run_wsl_probe_bounded_with_stdin(
    args: &[&str],
    stdin: Option<&[u8]>,
    timeout: Duration,
) -> Result<std::process::Output, String> {
    let verdict = host_disk_gate();
    allow_existing_wsl_probe(&verdict, || {
        // The listing gets the caller's budget (never less than 3s): a patient
        // caller is no better off if the preflight in front of it times out.
        let list_budget = timeout.max(Duration::from_secs(3));
        let verbose = run_wsl_bounded(&["--list", "--verbose"], list_budget)?;
        let running = run_wsl_bounded(&["--list", "--running", "--quiet"], list_budget)?;
        if !verbose.status.success() || !running.status.success() {
            return Err("WSL distribution listing failed".into());
        }
        Ok(default_wsl_is_running(
            &decode_wsl_listing(&verbose.stdout),
            &decode_wsl_listing(&running.stdout),
        ))
    })?;
    run_wsl_bounded_with_stdin(args, stdin, timeout)
}

#[cfg(windows)]
fn report_recovery_disk_gate(verdict: &HostDiskVerdict) {
    if matches!(verdict, HostDiskVerdict::Block { .. }) {
        if !HOST_DISK_BLOCK_REPORTED.swap(true, Ordering::Relaxed) {
            launch_log(&format!(
                "RECOVERY BLOCKED: {}",
                host_disk_block_message(verdict)
            ));
            append_host_disk_launcher_event("recovery_blocked", verdict, None);
        }
    } else {
        HOST_DISK_BLOCK_REPORTED.store(false, Ordering::Relaxed);
    }
}

#[cfg(windows)]
pub fn ensure_daemon_running_wsl(app: &AppHandle) -> Result<DaemonStart, String> {
    launch_log("ensure_daemon_running_wsl: start");

    // Refresh the WSL launcher on EVERY app start, before any early return.
    //
    // A shell upgrade ships a new start-wsl.sh, but every path below that
    // finds an already-running daemon returns before the spawn path used to
    // write it. So a launcher fix — for example the stop_port_holder sweep
    // that clears an orphan squatting on :8787 — could be installed and still
    // never take effect: the user upgrades, the daemon is already healthy, we
    // proxy to it, and the OLD launcher stays on disk to run the next start.
    // That is exactly how the orphan fix failed to reach a machine that had
    // installed it.
    //
    // Writing the script is a pure file write; it starts nothing and races no
    // systemd unit. It is deliberately best-effort: a missing or corrupt
    // bundle must not take a healthy, already-running daemon away from the
    // user. The spawn path below still resolves the bundles for real and
    // fails loudly if they are unusable.
    let wsl_bundles = resolve_wsl_linux_bundles(app);
    let refreshed_launcher: Option<PathBuf> = match wsl_bundles.as_ref() {
        Ok((amd64, arm64)) => match write_wsl_start_script(amd64, arm64) {
            Ok(path) => {
                launch_log(&format!("start script refreshed = {}", path.display()));
                Some(path)
            }
            Err(e) => {
                launch_log(&format!("start script refresh failed: {e}"));
                None
            }
        },
        Err(e) => {
            launch_log(&format!("start script refresh skipped: {e}"));
            None
        }
    };

    // Measure and report pressure now, but try authenticated reuse before
    // rejecting a new start. The disk floor must not disconnect healthy panes.
    if let Some((free, total)) = host_disk_space() {
        let policy_json = solu_data_dir()
            .and_then(|d| std::fs::read_to_string(d.join("host-disk-policy.json")).ok());
        let mb = std::env::var("SOLU_HOST_DISK_MIN_FREE_MB").ok();
        let pct = std::env::var("SOLU_HOST_DISK_MIN_FREE_PCT").ok();
        let policy =
            host_disk_policy_from_sources(mb.as_deref(), pct.as_deref(), policy_json.as_deref());
        let verdict = host_disk_verdict_from_sources(
            free,
            Some(total),
            mb.as_deref(),
            pct.as_deref(),
            policy_json.as_deref(),
        );
        let floor = policy
            .min_free_bytes
            .max(total.saturating_mul(policy.min_free_pct as u64) / 100);
        append_host_disk_launcher_event("preflight", &verdict, Some((free, total, floor)));
        *HOST_DISK_GATE.lock().unwrap() = Some((verdict.clone(), Instant::now()));
        if let HostDiskVerdict::Block { .. } = verdict {
            let msg = host_disk_block_message(&verdict);
            launch_log(&format!(
                "PREFLIGHT: {msg}; checking for an existing backend"
            ));
        } else if let HostDiskVerdict::Warn { .. } = verdict {
            launch_log(&format!(
                "PREFLIGHT WARNING: {}",
                host_disk_block_message(&verdict)
            ));
        }
    }

    // Our Windows-side proxy listens on 127.0.0.1:18787, not 8787.
    // Why: 8787 may already have a service worker registered in
    // WebView2's storage from a previous session where the webview
    // loaded the Windows-native daemon there. That SW intercepts
    // navigations and breaks the WSL-auth cookie-redirect flow with
    // "a redirected response was used for a request whose redirect
    // mode is not 'follow'". Serving the WSL backend from a different
    // origin gives the webview a fresh SW slate, no cache conflict,
    // and no state clobbering required.
    let already = port_alive("127.0.0.1:18787");
    launch_log(&format!("port_alive(127.0.0.1:18787) = {already}"));
    if already {
        // Token-aware, exactly like the reuse/spawn probes below. The
        // existing proxy forwards to the WSL daemon's address, which in
        // NAT mode is the non-loopback eth0 IP — where the daemon
        // ENFORCES the bearer token (only true-loopback is token-exempt).
        // A token-LESS probe here gets 401 and wrongly trips the
        // host-native fallback, dropping the user onto an empty/wrong
        // backend that reads as "daemon unreachable". Present the token.
        //
        // A failed check is not always a foreign occupant. Under Consomme
        // networking a closed WSL listener leaves a Windows forwarding
        // reservation that accepts and then resets; it clears on its own.
        // Wait that out: a freed port continues to the normal start below,
        // which binds our proxy there.
        let reuse = match probe_wsl_candidate("127.0.0.1:18787") {
            Ok(()) => true,
            Err(e) => {
                let outcome = wait_out_forwarding_reservation(
                    || classify_port_occupant("127.0.0.1:18787", Duration::from_secs(2)),
                    thread::sleep,
                    Instant::now() + FORWARDING_RESERVATION_WAIT,
                );
                launch_log(&format!(
                    "existing WSL proxy check failed ({e}); port 18787 after waiting: {outcome:?}"
                ));
                match outcome {
                    ReservationWait::Freed => false,
                    ReservationWait::Answers => {
                        probe_wsl_candidate("127.0.0.1:18787").map_err(|e| {
                            format!("existing WSL proxy identity check failed: {e}")
                        })?;
                        true
                    }
                    ReservationWait::Persisting => {
                        return Err(format!("existing WSL proxy identity check failed: {e}"));
                    }
                }
            }
        };
        if reuse {
            launch_log("something on 18787 — returning empty guard");
            return Ok(DaemonStart {
                guard: SidecarGuard::empty(),
                initial_url: wsl_initial_url(),
                alt_url: None,
            });
        }
        launch_log("18787 was a stale forwarding reservation and is free now; starting normally");
    }

    // Resolve the WSL-side daemon addresses once. Prefer the direct WSL
    // VM IP in standard NAT mode, but fall back to Windows loopback —
    // with WSL mirrored networking the `hostname -I` IP may not loop
    // back from the Windows side, while 127.0.0.1 is mirrored. These
    // serve double duty: detecting an already-running daemon (just
    // below) and as the post-spawn reachability-wait targets.
    let mut upstream_candidates = Vec::new();
    match resolve_wsl_ip() {
        Ok(wsl_ip) => {
            launch_log(&format!("wsl_ip = {wsl_ip}"));
            upstream_candidates.push(format!("{wsl_ip}:8787"));
        }
        Err(e) => {
            launch_log(&format!("resolve_wsl_ip FAILED: {e}"));
        }
    }
    for addr in WSL_LOOPBACK_CANDIDATES {
        upstream_candidates.push(addr.into());
    }
    upstream_candidates.dedup();

    // Reuse an already-running WSL daemon instead of spawning our own.
    // The spawn path runs a startup script that kills any process whose
    // command line matches `--addr 0.0.0.0:8787 ... --backend-id wsl
    // ... --shell-platform windows-amd64` — which is exactly the systemd
    // `soluterminal.service` daemon. Spawning therefore starts a
    // kill/restart war with systemd (309784 restarts observed once).
    // If a healthy WSL daemon already answers, skip the spawn entirely
    // and just proxy to it.
    // The route the proxy should prefer (and move back to): the VM IP.
    let vm_route = upstream_candidates
        .first()
        .filter(|c| !is_wsl_loopback_route(c))
        .cloned();
    if let Some(route) = find_reusable_wsl_daemon(
        &upstream_candidates,
        vm_route.is_some(),
        probe_wsl_candidate,
        thread::sleep,
        launch_log,
    ) {
        return finish_wsl_proxy(route, None, vm_route);
    }

    // Only the path that actually starts/replaces a daemon is blocked. Recheck
    // after the probes: free space may have changed while they were running.
    let verdict = host_disk_gate_with_refresh(true);
    report_recovery_disk_gate(&verdict);
    if !recovery_should_spawn_wsl(&verdict) {
        return Err(host_disk_block_message(&verdict));
    }

    // Both bundled Linux daemons were already resolved for the launcher
    // refresh at the top of this function. Reuse that result; only this
    // spawn path, which actually execs them, treats a missing bundle as fatal.
    let (wsl_amd64, wsl_arm64) = wsl_bundles.clone()?;
    launch_log(&format!("wsl bundles: amd64={wsl_amd64} arm64={wsl_arm64}"));

    // Bind to 0.0.0.0 so the daemon is reachable on WSL's eth0 from
    // Windows (our TCP proxy connects via the VM IP, not localhost).
    //
    // We can't chmod the /mnt/c/Program Files copy — that directory is
    // NTFS admin-owned and WSL isn't elevated. Instead, copy the
    // binary to $HOME/.local/share/soluterminal/solud (WSL ext4,
    // user-writable), chmod there, and exec from the local copy.
    //
    // The script is kept deliberately flat: no `$(...)`, no DEST
    // variable, no nested double-quotes. That combo gets mangled
    // somewhere in the Windows CRT→wsl.exe→bash interop path. Spelling
    // the path out explicitly is ugly but reliable.
    // Write the startup script to a file and execute via
    // `wsl.exe -e bash <path>`. Inline `-c` scripts get their quoting
    // mangled somewhere in the Windows→wsl.exe→bash interop chain and
    // the daemon fails to start silently. A script file has zero
    // quoting ambiguity and also gives us a stable daemon log for
    // diagnostics.
    // Reuse the launcher written at the top of this function when that write
    // succeeded; otherwise write it now, and fail loudly if we cannot. The
    // refresh is best-effort, but actually spawning without a launcher is not.
    let script_win_path = match refreshed_launcher {
        Some(path) => path,
        None => write_wsl_start_script(&wsl_amd64, &wsl_arm64)
            .map_err(|e| format!("write startup script: {e}"))?,
    };
    let script_wsl_path = windows_path_to_wsl(&script_win_path)
        .ok_or_else(|| format!("translate {}", script_win_path.display()))?;
    launch_log(&format!("start script = {}", script_win_path.display()));
    launch_log(&format!("start script (wsl path) = {script_wsl_path}"));

    // start-wsl.sh reports a port it can never get (report_port_blocked) in
    // this file, next to itself. Clear the previous launch's report first so
    // only this launch's can end the wait.
    let status_path = script_win_path.with_file_name(WSL_START_STATUS_FILE);
    match std::fs::remove_file(&status_path) {
        Ok(()) => launch_log("cleared the previous start-wsl.sh port report"),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => launch_log(&format!("could not clear {}: {e}", status_path.display())),
    }

    crate::startup_screen::step("Starting the WSL background service");
    let child = Command::new("wsl.exe")
        .args(["-e", "bash", &script_wsl_path])
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .map_err(|e| format!("spawn wsl.exe: {e} (is WSL installed?)"))?;
    job::assign(&child);
    launch_log(&format!("spawned wsl.exe, child pid = {}", child.id()));

    // Wait for the daemon to bind on a Windows-reachable address. Startup can
    // take longer when a large WSL session is reattaching many panes: a
    // 73-pane session at load 70-100 took ~80s from launch to a healthy
    // /api/health. Giving up earlier drops the user onto the Windows-only
    // backend for the whole session (WSL gets one attempt per launch), with a
    // healthy WSL daemon they can no longer see.
    //
    // A port the new daemon can never bind is a different failure from a slow
    // start, and start-wsl.sh reports it within seconds: an older daemon with
    // no backend id in a second distro (WSL2 distros share one network) held
    // :8787 on 2026-10-28 and every launch waited the full 120s for nothing.
    let startup_timeout = Duration::from_secs(120);
    // A *stale* WSL daemon answering for this long also ends the wait early.
    let stale_bail = Duration::from_secs(10);
    let have_vm_ip = upstream_candidates.len() > WSL_LOOPBACK_CANDIDATES.len();
    let (outcome, upstream_candidates) = wait_for_spawned_wsl_daemon(
        upstream_candidates,
        have_vm_ip,
        startup_timeout,
        stale_bail,
        probe_wsl_candidate,
        || resolve_wsl_ip().ok(),
        || {
            std::fs::read_to_string(&status_path)
                .ok()
                .and_then(|text| parse_wsl_start_status(&text))
        },
        launch_log,
    );
    let wsl_addr = match outcome {
        SpawnedWslWait::Reachable(route) => route,
        SpawnedWslWait::GaveUp(msg) => return Err(msg),
        SpawnedWslWait::Blocked(block) => {
            crate::startup_screen::step("Checking which WSL distro is using the port");
            let holder = locate_wsl_port_holder(&block);
            let msg = describe_wsl_port_block(&block, holder.as_ref(), env!("SOLU_SHELL_VERSION"));
            launch_log(&format!(
                "not waiting for the WSL daemon: {msg} (start-wsl.sh keeps waiting for the port and starts the daemon once it is free)"
            ));
            // The wsl.exe child stays alive inside the app's job: start-wsl.sh
            // starts our daemon by itself once the port is released, and the
            // recovery watch then offers to reconnect.
            drop(child);
            return Err(msg);
        }
    };
    launch_log(&format!("daemon reachable at {wsl_addr}"));

    let vm_route = upstream_candidates
        .first()
        .filter(|c| !is_wsl_loopback_route(c))
        .cloned();
    finish_wsl_proxy(wsl_addr, Some(child), vm_route)
}

/// Ask every other running distro whether the reported pid is the daemon
/// holding the port. Read-only and bounded: `wsl --list --running` never
/// starts a distro, and only distros it lists are asked.
#[cfg(windows)]
fn locate_wsl_port_holder(block: &WslPortBlock) -> Option<WslPortHolder> {
    let running = if block.visible {
        Vec::new()
    } else {
        match run_wsl_bounded(&["--list", "--running", "--quiet"], Duration::from_secs(5)) {
            Ok(out) if out.status.success() => decode_wsl_listing(&out.stdout)
                .lines()
                .map(|l| l.trim().to_string())
                .filter(|l| !l.is_empty())
                .collect(),
            Ok(out) => {
                launch_log(&format!("wsl --list --running failed: {:?}", out.status));
                Vec::new()
            }
            Err(e) => {
                launch_log(&format!("wsl --list --running failed: {e}"));
                Vec::new()
            }
        }
    };
    let inspect_script = wsl_holder_inspect_script();
    let port = block
        .addr
        .rsplit(':')
        .next()
        .filter(|p| !p.is_empty() && p.chars().all(|c| c.is_ascii_digit()))
        .unwrap_or("8787")
        .to_string();
    let holder = find_wsl_port_holder(block, &running, |distro, pid| {
        let pid = pid.to_string();
        let out = run_wsl_bounded(
            &["-d", distro, "-e", "sh", "-c", &inspect_script, "inspect", &pid, &port],
            Duration::from_secs(5),
        )
        .ok()?;
        let text = String::from_utf8_lossy(&out.stdout).into_owned();
        let field = |key: &str| {
            text.lines()
                .find_map(|l| l.strip_prefix(key))
                .unwrap_or("")
                .to_string()
        };
        Some((field("listen=") == "1", field("exe="), field("cmd="), field("cgroup=")))
    });
    match &holder {
        Some(h) => launch_log(&format!(
            "the WSL port holder runs in distro {:?} (cgroup {:?})",
            h.distro, h.cgroup
        )),
        None => launch_log(&format!(
            "could not tell which WSL distro runs the port holder (running distros: {running:?})"
        )),
    }
    holder
}

/// Start the Windows-side TCP proxy (127.0.0.1:18787 → the WSL daemon)
/// and build the DaemonStart result. `child` is the spawned wsl.exe
/// process when we launched the daemon ourselves, or None when reusing
/// a daemon that was already running (e.g. the systemd service).
#[cfg(windows)]
fn finish_wsl_proxy(
    wsl_addr: String,
    child: Option<Child>,
    vm_route: Option<String>,
) -> Result<DaemonStart, String> {
    // Start our own Windows-side TCP proxy: accept on 127.0.0.1:18787,
    // forward each connection to the WSL daemon address. Protocol-
    // agnostic — WebSockets / HTTP / push / anything TCP works as-is.
    // Port 18787 (not 8787) gives the webview a fresh origin so we
    // don't inherit a stale service worker from any past Windows-native
    // session (see auth-gate + SW interaction note above).
    if let Some(vm) = wsl_rehome_target(&wsl_addr, vm_route.as_deref()) {
        launch_log(&format!(
            "WSL proxy settling for loopback {wsl_addr}: the VM IP {vm} did not answer within {}s; \
             every connection will use a WSL localhost-forwarding channel until it re-homes",
            WSL_VM_ROUTE_GRACE.as_secs()
        ));
    }
    let proxy = match start_tcp_proxy("127.0.0.1:18787", wsl_addr.clone(), vm_route) {
        Ok(p) => p,
        Err(e) if e.kind() == io::ErrorKind::AddrInUse => {
            // Almost certainly our own twin: another window of this app, or
            // the instance we're replacing across an app.restart(), bound
            // 18787 in the gap between the port_alive precheck at the top of
            // ensure_daemon_running_wsl and this bind — a gap that contains
            // resolve_wsl_ip, a wsl.exe spawn, and a multi-second
            // reachability wait, i.e. a TOCTOU window big enough to lose
            // reliably (observed in a real launch.log: two setups one second
            // apart, loser demoted). Losing this race used to be treated as
            // a fatal WSL-backend failure: the error was flattened into an
            // opaque string, classify_wsl_fallback had no arm for it, and
            // the user was dropped to host-native with a dialog blaming WSL
            // — while WSL was demonstrably fine, since the twin was serving
            // through it at that very moment. Binding our own port is
            // success wearing an error code: verify the listener really is
            // our proxy chain (same token-aware identity probe as the fast
            // path) and use it.
            probe_wsl_candidate("127.0.0.1:18787").map_err(
                |probe| {
                    launch_log(&format!(
                        "start_tcp_proxy: 18787 in use and its occupant failed the identity probe: {probe}"
                    ));
                    format!("start TCP proxy: {e} (18787 is held by something that is not our daemon: {probe})")
                },
            )?;
            launch_log("start_tcp_proxy: 18787 already bound by our own twin — using its proxy");
            return Ok(DaemonStart {
                guard: SidecarGuard {
                    // Keep any wsl.exe wrapper we spawned so its lifecycle
                    // stays managed; there is just no proxy of ours to hold.
                    child,
                    supervisors: Vec::new(),
                    proxy: None,
                    extras: Vec::new(),
                    extra_proxies: Vec::new(),
                },
                initial_url: wsl_initial_url(),
                alt_url: None,
            });
        }
        Err(e) => {
            launch_log(&format!("start_tcp_proxy FAILED: {e}"));
            return Err(format!("start TCP proxy: {e}"));
        }
    };
    launch_log(&format!("TCP proxy up: 127.0.0.1:18787 → {wsl_addr}"));

    Ok(DaemonStart {
        guard: SidecarGuard {
            child,
            // The WSL backend has its own supervisor: the start script's
            // wait/PIDFILE loop restarts the daemon inside WSL. Nothing
            // for us to watch from this side.
            supervisors: Vec::new(),
            proxy: Some(proxy),
            extras: Vec::new(),
            extra_proxies: Vec::new(),
        },
        initial_url: wsl_initial_url(),
        alt_url: None,
    })
}

/// The URL the webview should load for the WSL backend's proxy. The WSL
/// daemon treats only true-loopback connections as no-token; depending on
/// the selected upstream it may see the proxy as remote and require a
/// token. Read the token file the daemon wrote at startup and append it —
/// the frontend persists it locally and carries it automatically after.
#[cfg(windows)]
fn wsl_initial_url() -> String {
    match read_wsl_token() {
        Ok(token) => {
            launch_log("read WSL token OK");
            format!("http://127.0.0.1:18787/?token={token}")
        }
        Err(e) => {
            launch_log(&format!(
                "read_wsl_token FAILED: {e} — loading without token"
            ));
            "http://127.0.0.1:18787".into()
        }
    }
}

/// Generate the bash startup script used to launch the bundled Linux
/// daemon inside WSL. Writes to `%LOCALAPPDATA%\soluterminal\start-wsl.sh`
/// (accessible from WSL via /mnt/c/...) and returns the Windows path.
/// The script copies the bundled binary into WSL ext4, chmods, and execs
/// with its stdout/stderr redirected to a daemon.log we can inspect
/// after the fact.
#[cfg(windows)]
fn write_wsl_start_script(bundled_amd64_wsl: &str, bundled_arm64_wsl: &str) -> io::Result<PathBuf> {
    let mut dir = PathBuf::from(
        std::env::var_os("LOCALAPPDATA")
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "no LOCALAPPDATA"))?,
    );
    dir.push("soluterminal");
    std::fs::create_dir_all(&dir)?;
    let script_path = dir.join("start-wsl.sh");
    let body = wsl_start_script_body(bundled_amd64_wsl, bundled_arm64_wsl);
    std::fs::write(&script_path, body.as_bytes())?;
    Ok(script_path)
}

/// Resolve both bundled Linux daemons and translate them to `/mnt/...` paths
/// for the launcher.
///
/// Split out of `ensure_daemon_running_wsl` so the launcher can be refreshed on
/// every app start: the launcher embeds these paths, and the reuse paths that
/// never spawn used to skip writing it entirely, so a launcher shipped in a new
/// shell could sit unused on disk while an already-running daemon was proxied
/// to. ARM64 Windows (Snapdragon) runs aarch64 WSL, where an amd64 ELF cannot
/// exec, so both are resolved and the script picks by `uname -m`. Keep the
/// "bundled Linux solud not found / is empty" phrasing — classify_wsl_fallback
/// keys on it.
#[cfg(windows)]
fn resolve_wsl_linux_bundles(app: &AppHandle) -> Result<(String, String), String> {
    let resolve_gz = |name: &str| -> Result<String, String> {
        let p = app
            .path()
            .resolve(name, tauri::path::BaseDirectory::Resource)
            .map_err(|e| format!("resolve resource path {name}: {e}"))?;
        if !p.is_file() {
            return Err(format!("bundled Linux solud not found at {}", p.display()));
        }
        let meta = std::fs::metadata(&p)
            .map_err(|e| format!("stat bundled Linux solud {}: {e}", p.display()))?;
        if meta.len() == 0 {
            return Err(format!(
                "bundled Linux solud at {} is empty; rebuild the WSL sidecar",
                p.display()
            ));
        }
        windows_path_to_wsl(&p)
            .ok_or_else(|| format!("couldn't translate {} to /mnt/...", p.display()))
    };
    let amd64 = resolve_gz("resources/solud-linux-amd64.gz")?;
    let arm64 = resolve_gz("resources/solud-linux-arm64.gz")?;
    Ok((amd64, arm64))
}

/// Shell (POSIX sh, so it also runs through `wsl.exe -e sh -c`): does pid $1
/// own a LISTEN socket on TCP port $2? WSL2 distros share one network
/// namespace, so /proc/net/tcp lists the socket in every distro, but only the
/// distro that runs the owner can match its inode in /proc/$1/fd. A pid that
/// merely exists, or is an unrelated solud, does not pass.
const PID_LISTENS_ON_PORT_FN: &str = r#"pid_listens_on_port() {
  _ph="$(printf '%04X' "$2" 2>/dev/null || true)"
  [ -n "$_ph" ] || return 1
  for _inode in $(awk -v ph=":$_ph" '$4 == "0A" && substr($2, length($2) - 4) == ph { print $10 }' /proc/net/tcp /proc/net/tcp6 2>/dev/null || true); do
    if ls -l "/proc/$1/fd" 2>/dev/null | grep -qF "socket:[$_inode]"; then return 0; fi
  done
  return 1
}
"#;

/// Run in each other running distro: `$1` is the reported pid, `$2` the
/// port. Prints what `find_wsl_port_holder` needs; read-only.
#[cfg_attr(not(windows), allow(dead_code))]
fn wsl_holder_inspect_script() -> String {
    format!(
        "{PID_LISTENS_ON_PORT_FN}p=\"$1\"\n\
         if pid_listens_on_port \"$p\" \"$2\"; then echo listen=1; else echo listen=0; fi\n\
         printf 'exe=%s\\n' \"$(readlink \"/proc/$p/exe\" 2>/dev/null)\"\n\
         printf 'cmd=%s\\n' \"$(tr '\\0' ' ' < \"/proc/$p/cmdline\" 2>/dev/null)\"\n\
         printf 'cgroup=%s\\n' \"$(tail -n1 \"/proc/$p/cgroup\" 2>/dev/null)\"\n"
    )
}

/// The report start-wsl.sh writes next to itself (see report_port_blocked in
/// the script) when the WSL port is held by a daemon it will not replace.
const WSL_START_STATUS_FILE: &str = "wsl-start-status";

/// The start-wsl.sh text itself, split out from the Windows-only writer above
/// so the template can be unit-tested (and `bash -n`'d) on any host. This is
/// the launcher that generations of daemon crashes have to survive, and the
/// only way to keep it honest is to assert on it.
#[allow(dead_code)] // called only from the #[cfg(windows)] writer above.
fn wsl_start_script_body(bundled_amd64_wsl: &str, bundled_arm64_wsl: &str) -> String {
    let daemon_log_name = "daemon.log";
    // Single-quote the /mnt/c path inside the script so spaces don't
    // need escaping. No shell expansion needed inside ' '. The $HOME
    // references are in double quotes; bash expands them at runtime.
    format!(
        concat!(
            "#!/bin/bash\n",
            "set -e\n",
            "# Windows environment variables can bleed into WSL under some\n",
            "# launches. Trust passwd for the Linux home, not inherited $HOME.\n",
            "uid=\"$(id -u)\"\n",
            "WSL_HOME=\"$(getent passwd \"$uid\" | cut -d: -f6 || true)\"\n",
            "if [ -z \"$WSL_HOME\" ]; then\n",
            "  user=\"$(id -un 2>/dev/null || echo \"${{USER:-user}}\")\"\n",
            "  WSL_HOME=\"/home/$user\"\n",
            "fi\n",
            "export HOME=\"$WSL_HOME\"\n",
            "# The daemon and every worker it spawns inherit this environment. A\n",
            "# Windows desktop launch hands us a minimal PATH with no nvm/asdf/Volta\n",
            "# node, which made node-based provider CLIs fail with 'env: node: No such\n",
            "# file or directory'. Establish the user's toolchain here so every\n",
            "# initial launch and relaunch of this supervisor yields the same\n",
            "# environment, and keep the prelude best-effort: this script runs under\n",
            "# set -e, so a missing manager or unset alias must never sink the boot.\n",
            "BASE_PATH=\"$PATH\"\n",
            "export PATH=\"$HOME/.local/bin:$HOME/.volta/bin:$BASE_PATH\"\n",
            "if [ -s \"$HOME/.asdf/asdf.sh\" ]; then\n",
            "  . \"$HOME/.asdf/asdf.sh\" || true\n",
            "fi\n",
            "if [ -s \"$HOME/.nvm/nvm.sh\" ]; then\n",
            "  . \"$HOME/.nvm/nvm.sh\" || true\n",
            "  if command -v nvm >/dev/null 2>&1; then\n",
            "    nvm use default >/dev/null 2>&1 || true\n",
            "  fi\n",
            "fi\n",
            "DAEMON_DIR=\"$HOME/.local/share/soluterminal\"\n",
            "BUNDLE_AMD64='{src_amd64}'\n",
            "BUNDLE_ARM64='{src_arm64}'\n",
            "case \"$(uname -m)\" in\n",
            "  aarch64|arm64) BUNDLE=\"$BUNDLE_ARM64\" ;;\n",
            "  *) BUNDLE=\"$BUNDLE_AMD64\" ;;\n",
            "esac\n",
            "BUNDLE_VERSION='{daemonver}'\n",
            "LOCAL=\"$DAEMON_DIR/solud\"\n",
            "LOCAL_VERSION_FILE=\"$LOCAL.version\"\n",
            "PIDFILE=\"$DAEMON_DIR/solud-wsl.pid\"\n",
            "# One supervisor at a time (see claim_supervision): the lock is held on\n",
            "# fd 9 for the supervisor's whole life; the pidfile only names the holder\n",
            "# for the log and for a takeover. $0 is how the Windows shell ran us, which\n",
            "# is how another copy of this script is recognised.\n",
            "SUPERVISOR_SCRIPT=\"$0\"\n",
            "SUPERVISOR_LOCK=\"$DAEMON_DIR/start-wsl.lock\"\n",
            "SUPERVISOR_PIDFILE=\"$DAEMON_DIR/start-wsl.pid\"\n",
            "# The Windows shell reads this report, next to this script on its own\n",
            "# drive, while it waits for the daemon. A port this launcher can never\n",
            "# get is reported in seconds instead of after the shell's whole wait.\n",
            "case \"$SUPERVISOR_SCRIPT\" in\n",
            "  */*) START_STATUS=\"${{SUPERVISOR_SCRIPT%/*}}/{status}\" ;;\n",
            "  *) START_STATUS=\"\" ;;\n",
            "esac\n",
            "# The one place the listen address is written. launch_daemon and the\n",
            "# stop_port_holder ownership check both read it, so they cannot drift\n",
            "# apart and leave the cleanup looking for a port we never bind.\n",
            "ADDR=\"0.0.0.0:8787\"\n",
            "mkdir -p \"$DAEMON_DIR\"\n",
            "# Retire a conflicting systemd --user unit, if one is installed.\n",
            "#\n",
            "# SoluTerminal has two install paths and they both want :8787. This\n",
            "# desktop app spawns and supervises the WSL daemon itself; the Linux\n",
            "# one-liner (`curl … | sh`) instead offers to install\n",
            "# ~/.config/systemd/user/soluterminal.service so the daemon starts on\n",
            "# login. Install both (or upgrade across the change) and the app kills\n",
            "# the unit's daemon to take the port while systemd's Restart=always\n",
            "# resurrects it — a kill/restart war (309784 restarts observed once)\n",
            "# that reads to the user as panes that never stop reconnecting.\n",
            "#\n",
            "# On a desktop install this app owns the daemon, so retire the unit's\n",
            "# AUTOSTART — and only that exact unit. `systemctl --user disable` is\n",
            "# preferred when a user bus exists, but dropping the\n",
            "# `default.target.wants` symlink achieves the same with NO systemd\n",
            "# dependency at all, which is the property that matters: a desktop\n",
            "# install must never require systemd. Best-effort throughout — this\n",
            "# runs under `set -e`, and a failure here must cost a warning, never\n",
            "# the backend.\n",
            "retire_conflicting_systemd_unit() {{\n",
            "  unit=\"$HOME/.config/systemd/user/soluterminal.service\"\n",
            "  wants=\"$HOME/.config/systemd/user/default.target.wants/soluterminal.service\"\n",
            "  [ -e \"$unit\" ] || return 0\n",
            "  if [ -L \"$wants\" ]; then\n",
            "    rm -f \"$wants\" 2>/dev/null || true\n",
            "    echo \"conflict: disabled systemd soluterminal.service autostart (the desktop app manages this daemon)\"\n",
            "  fi\n",
            "  # Only with a user bus. `wsl.exe -e` often runs without a systemd\n",
            "  # user session ('Failed to start the systemd user session'); then\n",
            "  # systemctl --user can only fail, and on some hosts it waits first.\n",
            "  # Nothing else in this script needs systemd.\n",
            "  bus_dir=\"${{XDG_RUNTIME_DIR:-/run/user/$uid}}\"\n",
            "  if command -v systemctl >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1 && [ -S \"$bus_dir/bus\" ]; then\n",
            "    XDG_RUNTIME_DIR=\"$bus_dir\" timeout 10 systemctl --user disable --now soluterminal.service >/dev/null 2>&1 || true\n",
            "  fi\n",
            "  return 0\n",
            "}}\n",
            "retire_conflicting_systemd_unit\n",
            "# Rotate the diagnostics log; never truncate it. This script used to\n",
            "# open the log with a truncating exec, so every supervisor session\n",
            "# destroyed the record of the one before it and the only run you\n",
            "# could ever read about was the one that had not gone wrong yet.\n",
            "# Three generations, same rationale as the host-native side: the\n",
            "# bytes worth having are the tail of the run that just died.\n",
            "# Plain `if` blocks, not `[ ] && ...` chains -- under `set -e` a\n",
            "# false test as the last command of the script would abort the boot.\n",
            "# Every step is `|| true` and the function returns 0 for the same\n",
            "# reason: this runs under `set -e`, and a read-only or full\n",
            "# $DAEMON_DIR must cost us a rotation, never the whole backend.\n",
            "rotate_daemon_log() {{\n",
            "  rm -f \"$DAEMON_DIR/{log}.2\" || true\n",
            "  if [ -f \"$DAEMON_DIR/{log}.1\" ]; then mv -f \"$DAEMON_DIR/{log}.1\" \"$DAEMON_DIR/{log}.2\" || true; fi\n",
            "  if [ -s \"$DAEMON_DIR/{log}\" ]; then mv -f \"$DAEMON_DIR/{log}\" \"$DAEMON_DIR/{log}.1\" || true; fi\n",
            "  return 0\n",
            "}}\n",
            "# Size-gated rotation, used between crash relaunches. Rotating on every\n",
            "# relaunch would let a boot-crash loop (10 attempts, 2s apart) burn all\n",
            "# three generations in seconds and delete the part that explains it:\n",
            "# the staging decisions, the stale-daemon cleanup and the first crash.\n",
            "# Appending keeps one supervisor session in one file, in order, and the\n",
            "# cap is all that ever moves it aside.\n",
            "rotate_daemon_log_if_big() {{\n",
            "  size=\"$(wc -c < \"$DAEMON_DIR/{log}\" 2>/dev/null | tr -dc '0-9' || true)\"\n",
            "  [ -n \"$size\" ] || size=0\n",
            "  if [ \"$size\" -ge {logmax} ]; then rotate_daemon_log; fi\n",
            "  return 0\n",
            "}}\n",
            "rotate_daemon_log\n",
            "exec >> \"$DAEMON_DIR/{log}\" 2>&1\n",
            "echo \"=== $(date -Is) starting soluterminal WSL daemon ===\"\n",
            "ver_gt() {{\n",
            "  a=\"$1\"\n",
            "  b=\"$2\"\n",
            "  [ -n \"$a\" ] && [ -n \"$b\" ] || return 1\n",
            "  [ \"$a\" = \"$b\" ] && return 1\n",
            "  IFS=. read -ra ap <<< \"$a\"\n",
            "  IFS=. read -ra bp <<< \"$b\"\n",
            "  max=${{#ap[@]}}\n",
            "  [ ${{#bp[@]}} -gt \"$max\" ] && max=${{#bp[@]}}\n",
            "  for ((i=0; i<max; i++)); do\n",
            "    av=\"${{ap[$i]:-0}}\"\n",
            "    bv=\"${{bp[$i]:-0}}\"\n",
            "    av=\"${{av%%[^0-9]*}}\"\n",
            "    bv=\"${{bv%%[^0-9]*}}\"\n",
            "    [ -z \"$av\" ] && av=0\n",
            "    [ -z \"$bv\" ] && bv=0\n",
            "    if ((10#$av > 10#$bv)); then return 0; fi\n",
            "    if ((10#$av < 10#$bv)); then return 1; fi\n",
            "  done\n",
            "  return 1\n",
            "}}\n",
            "rollback_hold_active() {{\n",
            "  candidate=\"$1\"\n",
            "  state_file=\"$HOME/.solu/update_rollback.json\"\n",
            "  [ -r \"$state_file\" ] || return 1\n",
            "  state=\"$(tr -d '\\r\\n\\t ' < \"$state_file\" 2>/dev/null || true)\"\n",
            "  held=\"$(printf '%s' \"$state\" | sed -n 's/.*\"hold\":{{\"version\":\"\\([0-9][0-9.]*\\)\".*/\\1/p')\"\n",
            "  expires=\"$(printf '%s' \"$state\" | sed -n 's/.*\"expiresAtMs\":\\([0-9][0-9]*\\).*/\\1/p')\"\n",
            "  [ \"$held\" = \"$candidate\" ] || return 1\n",
            "  case \"$expires\" in ''|*[!0-9]*) return 1 ;; esac\n",
            "  now_s=\"$(date +%s 2>/dev/null || true)\"\n",
            "  case \"$now_s\" in ''|*[!0-9]*) return 1 ;; esac\n",
            "  [ $((now_s * 1000)) -lt \"$expires\" ]\n",
            "}}\n",
            "stop_pid() {{\n",
            "  pid=\"$1\"\n",
            "  [ -n \"$pid\" ] || return 0\n",
            "  [ \"$pid\" = \"$$\" ] && return 0\n",
            "  kill \"$pid\" 2>/dev/null || true\n",
            "  sleep 0.5\n",
            "  kill -9 \"$pid\" 2>/dev/null || true\n",
            "}}\n",
            "# Kill whatever actually holds our listen port, even when it is not the\n",
            "# pid we think we manage. A self-update successor is forked by the\n",
            "# outgoing daemon, so it is never this shell's child; if the supervisor\n",
            "# is torn down mid-handoff (app quit, Windows killing the WSL relay)\n",
            "# that successor survives as an orphan and keeps the port. Every later\n",
            "# launch then dies with 'port ... held by another soluterminal daemon',\n",
            "# the supervisor burns its 10 restarts, and the UI shows panes that\n",
            "# reset on every keystroke. Cmdline-pattern matching cannot find it:\n",
            "# the survivor may carry different --shell-version / relay flags than\n",
            "# this build emits. The port itself is the identity we can trust.\n",
            "#\n",
            "# Identity check is deliberately narrow: only same-user processes whose\n",
            "# own --addr is the port we are about to bind. That cannot match an\n",
            "# unrelated `--addr 0.0.0.0:8787` grader/backend (e.g. a pytest server)\n",
            "# that also happens to be listening on it. Best-effort throughout: this\n",
            "# runs under `set -e` and must never abort the supervisor.\n",
            "#\n",
            "# Candidates come from ONE grep over every /proc/<pid>/cmdline, not a `tr`\n",
            "# fork per process: with ~1100 processes at load 100 the per-process scan\n",
            "# took ~29s, startup ran three of them, and the Windows shell gave up on\n",
            "# the daemon before this script had even launched it. The grep only\n",
            "# narrows; the identity checks below are unchanged.\n",
            "stop_port_holder() {{\n",
            "  port=\"${{ADDR##*:}}\"\n",
            "  case \"$port\" in ''|*[!0-9]*) return 0 ;; esac\n",
            "  for proc in $(grep -laFs -- \"$ADDR\" /proc/[0-9]*/cmdline 2>/dev/null || true); do\n",
            "    proc=\"${{proc%/cmdline}}\"\n",
            "    pid=\"${{proc##*/}}\"\n",
            "    [ \"$pid\" = \"$$\" ] && continue\n",
            "    [ -r \"$proc/cmdline\" ] || continue\n",
            "    cmdline=\"$(tr '\\0' ' ' 2>/dev/null < \"$proc/cmdline\" || true)\"\n",
            "    case \"$cmdline\" in *\" --addr $ADDR\"*|*\" --addr $ADDR \"*) ;; *) continue ;; esac\n",
            "    case \"$cmdline\" in *soluterminal*) ;; *) continue ;; esac\n",
            "    echo \"cleanup: stopping port holder pid=$pid on $ADDR\"\n",
            "    stop_pid \"$pid\"\n",
            "  done\n",
            "  return 0\n",
            "}}\n",
            "cleanup_managed_daemon() {{\n",
            "  pid=\"$(cat \"$PIDFILE\" 2>/dev/null || true)\"\n",
            "  if [ -n \"$pid\" ] && [ -r \"/proc/$pid/cmdline\" ]; then\n",
            "    cmdline=\"$(tr '\\0' ' ' < \"/proc/$pid/cmdline\" 2>/dev/null || true)\"\n",
            "    case \"$cmdline\" in\n",
            "      *\"$LOCAL\"*\" --addr 0.0.0.0:8787\"*|*\" --addr 0.0.0.0:8787\"*\" --backend-id wsl\"*\" --shell-platform windows-amd64\"*)\n",
            "        echo \"cleanup: stopping managed daemon pid=$pid\"\n",
            "        stop_pid \"$pid\"\n",
            "        ;;\n",
            "    esac\n",
            "  fi\n",
            "}}\n",
            "# Print one TOP-LEVEL value of the JSON object on stdin: a string's\n",
            "# contents, or a number/bool literal as written. Nested objects and arrays\n",
            "# are skipped whole. /api/health repeats \"version\" and \"pid\" inside every\n",
            "# fleet component (74 times on one real machine), so a grep would read a\n",
            "# pane agent's version as the daemon's. Plain awk (mawk/gawk/busybox), and\n",
            "# no backslashes in the program: quote, backslash and whitespace come from\n",
            "# sprintf so the Rust -> shell -> awk escaping cannot drift.\n",
            "json_top_value() {{\n",
            "  awk -v want=\"$1\" '\n",
            "BEGIN {{ RS = sprintf(\"%c\", 1); q = sprintf(\"%c\", 34); bs = sprintf(\"%c\", 92); ws = sprintf(\" %c%c%c\", 9, 10, 13) }}\n",
            "{{\n",
            "  s = $0; n = length(s); depth = 0; instr = 0; esc = 0\n",
            "  tok = \"\"; key = \"\"; inval = 0; raw = \"\"\n",
            "  for (i = 1; i <= n; i++) {{\n",
            "    c = substr(s, i, 1)\n",
            "    if (instr) {{\n",
            "      if (esc) {{ esc = 0; tok = tok c }}\n",
            "      else if (c == bs) {{ esc = 1 }}\n",
            "      else if (c == q) {{\n",
            "        instr = 0\n",
            "        if (depth == 1) {{\n",
            "          if (inval) {{ if (key == want) {{ print tok; exit }} }}\n",
            "          else {{ key = tok }}\n",
            "        }}\n",
            "      }}\n",
            "      else {{ tok = tok c }}\n",
            "      continue\n",
            "    }}\n",
            "    if (c == q) {{ instr = 1; tok = \"\"; continue }}\n",
            "    if (c == \"{{\" || c == \"[\") {{ depth++; continue }}\n",
            "    if (c == \"}}\" || c == \"]\") {{\n",
            "      if (depth == 1 && inval && key == want && raw != \"\") {{ print raw; exit }}\n",
            "      depth--\n",
            "      continue\n",
            "    }}\n",
            "    if (depth != 1) {{ continue }}\n",
            "    if (c == \":\") {{ inval = 1; raw = \"\"; continue }}\n",
            "    if (c == \",\") {{\n",
            "      if (inval && key == want && raw != \"\") {{ print raw; exit }}\n",
            "      inval = 0; raw = \"\"\n",
            "      continue\n",
            "    }}\n",
            "    if (inval && index(ws, c) == 0) {{ raw = raw c }}\n",
            "  }}\n",
            "}}'\n",
            "}}\n",
            "# Ask whatever answers on our port who it is, from inside WSL. That is\n",
            "# loopback, which the daemon trusts without a token (the owner token is\n",
            "# still sent when readable, for hosts that set SOLU_LOOPBACK_REQUIRE_AUTH;\n",
            "# it travels in the environment, never argv, which any user can read).\n",
            "# The 20s budget is deliberate: /api/health measured 2-6s on a loaded\n",
            "# host, and a probe that gives up early is exactly the bug this guards.\n",
            "# Sets LIVE_PID / LIVE_BACKEND / LIVE_VERSION / LIVE_BINARY; returns 0\n",
            "# only on HTTP 200 from something that identifies as a SoluTerminal\n",
            "# daemon. A daemon started without --backend-id (the Linux installer's\n",
            "# systemd unit, or a hand-started solud) reports an empty backend but\n",
            "# still names its version, and it holds the port just the same.\n",
            "LIVE_PID=\"\"\n",
            "LIVE_BACKEND=\"\"\n",
            "LIVE_VERSION=\"\"\n",
            "LIVE_BINARY=\"\"\n",
            "probe_live_daemon() {{\n",
            "  local port resp body tok\n",
            "  LIVE_PID=\"\"\n",
            "  LIVE_BACKEND=\"\"\n",
            "  LIVE_VERSION=\"\"\n",
            "  LIVE_BINARY=\"\"\n",
            "  port=\"${{ADDR##*:}}\"\n",
            "  case \"$port\" in ''|*[!0-9]*) return 1 ;; esac\n",
            "  command -v timeout >/dev/null 2>&1 || return 1\n",
            "  tok=\"$(tr -d '[:space:]' 2>/dev/null < \"$HOME/.solu/token\" || true)\"\n",
            "  resp=\"$(SOLU_PROBE_TOKEN=\"$tok\" timeout 20 bash -c 'exec 3<>\"/dev/tcp/127.0.0.1/$1\" || exit 1; if [ -n \"$SOLU_PROBE_TOKEN\" ]; then printf \"GET /api/health HTTP/1.0\\r\\nHost: 127.0.0.1\\r\\nAuthorization: Bearer %s\\r\\nConnection: close\\r\\n\\r\\n\" \"$SOLU_PROBE_TOKEN\" >&3; else printf \"GET /api/health HTTP/1.0\\r\\nHost: 127.0.0.1\\r\\nConnection: close\\r\\n\\r\\n\" >&3; fi; cat <&3' probe \"$port\" 2>/dev/null | tr -d '\\r' || true)\"\n",
            "  case \"$resp\" in \"HTTP/1.\"?\" 200 \"*) ;; *) return 1 ;; esac\n",
            "  body=\"$(printf '%s\\n' \"$resp\" | sed '1,/^$/d')\"\n",
            "  LIVE_PID=\"$(printf '%s' \"$body\" | json_top_value pid || true)\"\n",
            "  LIVE_BACKEND=\"$(printf '%s' \"$body\" | json_top_value backendID || true)\"\n",
            "  LIVE_VERSION=\"$(printf '%s' \"$body\" | json_top_value version || true)\"\n",
            "  LIVE_BINARY=\"$(printf '%s' \"$body\" | json_top_value binaryPath || true)\"\n",
            "  [ -n \"$LIVE_BACKEND\" ] || [ -n \"$LIVE_VERSION\" ]\n",
            "}}\n",
            "# A live daemon is worth keeping unless it is a different backend or older\n",
            "# than the daemon this shell bundles (an active rollback hold on the bundle\n",
            "# makes the older one intentional). Unknown version keeps it: replacing a\n",
            "# healthy daemon interrupts every pane and workflow run, and the Windows\n",
            "# side still enforces staleness with its own identity check.\n",
            "live_daemon_is_current() {{\n",
            "  [ \"$LIVE_BACKEND\" = \"{backend}\" ] || return 1\n",
            "  [ -n \"$LIVE_VERSION\" ] || return 0\n",
            "  if ver_gt \"$BUNDLE_VERSION\" \"$LIVE_VERSION\"; then\n",
            "    if rollback_hold_active \"$BUNDLE_VERSION\"; then return 0; fi\n",
            "    return 1\n",
            "  fi\n",
            "  return 0\n",
            "}}\n",
            "# Returns 0 when, after every cleanup this launcher may run, the port still\n",
            "# answers as a SoluTerminal daemon we must not keep: a different backend,\n",
            "# or older than the bundle. A healthy current daemon is never reported.\n",
            "# The usual cause is a daemon in ANOTHER WSL distro. WSL2 distros share one\n",
            "# network namespace, so its 127.0.0.1:8787 blocks our bind while its\n",
            "# processes are invisible from here. It is never stopped from here: its\n",
            "# panes live in that distro's ~/.solu, which our daemon cannot reattach.\n",
            "port_held_by_other_daemon() {{\n",
            "  probe_live_daemon || return 1\n",
            "  if live_daemon_is_current; then return 1; fi\n",
            "  return 0\n",
            "}}\n",
            "{pid_listens}",
            "# Whether LIVE_PID is the reported daemon running in this distro: it\n",
            "# owns the listening socket, and it is that binary (or at least a solud).\n",
            "live_daemon_visible_here() {{\n",
            "  local exe cmdline\n",
            "  case \"$LIVE_PID\" in ''|*[!0-9]*) return 1 ;; esac\n",
            "  [ -r \"/proc/$LIVE_PID/cmdline\" ] || return 1\n",
            "  pid_listens_on_port \"$LIVE_PID\" \"${{ADDR##*:}}\" || return 1\n",
            "  exe=\"$(readlink \"/proc/$LIVE_PID/exe\" 2>/dev/null || true)\"\n",
            "  if [ -n \"$LIVE_BINARY\" ] && [ \"$exe\" = \"$LIVE_BINARY\" ]; then return 0; fi\n",
            "  cmdline=\"$(tr '\\0' ' ' 2>/dev/null < \"/proc/$LIVE_PID/cmdline\" || true)\"\n",
            "  case \"$cmdline\" in *solud*) return 0 ;; esac\n",
            "  return 1\n",
            "}}\n",
            "# One key=value per line, each value one bounded line, written to a temp\n",
            "# file and renamed so the Windows shell never reads half a report.\n",
            "status_value() {{\n",
            "  printf '%s' \"$1\" | tr -d '\\r\\n' | cut -c1-300\n",
            "}}\n",
            "write_start_status() {{\n",
            "  [ -n \"$START_STATUS\" ] || return 0\n",
            "  {{\n",
            "    echo \"state=$(status_value \"$1\")\"\n",
            "    echo \"reason=$(status_value \"$2\")\"\n",
            "    echo \"visible=$(status_value \"${{3:-0}}\")\"\n",
            "    echo \"cgroup=$(status_value \"${{4:-}}\")\"\n",
            "    echo \"addr=$ADDR\"\n",
            "    echo \"backend=$(status_value \"$LIVE_BACKEND\")\"\n",
            "    echo \"version=$(status_value \"$LIVE_VERSION\")\"\n",
            "    echo \"pid=$(status_value \"$LIVE_PID\")\"\n",
            "    echo \"binary=$(status_value \"$LIVE_BINARY\")\"\n",
            "    echo \"distro=$(status_value \"${{WSL_DISTRO_NAME:-}}\")\"\n",
            "    echo \"bundle=$BUNDLE_VERSION\"\n",
            "  }} > \"$START_STATUS.tmp\" 2>/dev/null && mv -f \"$START_STATUS.tmp\" \"$START_STATUS\" 2>/dev/null || true\n",
            "  return 0\n",
            "}}\n",
            "# STATE is port-held (a SoluTerminal daemon identified itself on the port)\n",
            "# or bind-failed (our daemon could not bind and nothing identified itself).\n",
            "report_port_blocked() {{\n",
            "  local where visible cgroup\n",
            "  visible=0\n",
            "  cgroup=\"\"\n",
            "  where=\"not in this distro, so most likely in another WSL distro\"\n",
            "  if live_daemon_visible_here; then\n",
            "    visible=1\n",
            "    where=\"in this distro\"\n",
            "    cgroup=\"$(tail -n1 \"/proc/$LIVE_PID/cgroup\" 2>/dev/null || true)\"\n",
            "  fi\n",
            "  if [ \"$1\" = \"port-held\" ]; then\n",
            "    echo \"port $ADDR is held by a SoluTerminal daemon this launcher will not replace: backend ${{LIVE_BACKEND:-none}} version ${{LIVE_VERSION:-unknown}} pid ${{LIVE_PID:-unknown}} ($where); $2\"\n",
            "  else\n",
            "    echo \"port $ADDR is held by another program; $2\"\n",
            "  fi\n",
            "  write_start_status \"$1\" \"$2\" \"$visible\" \"$cgroup\"\n",
            "}}\n",
            "port_is_open() {{\n",
            "  local port\n",
            "  port=\"${{ADDR##*:}}\"\n",
            "  case \"$port\" in ''|*[!0-9]*) return 1 ;; esac\n",
            "  command -v timeout >/dev/null 2>&1 || return 1\n",
            "  timeout 5 bash -c 'exec 3<>\"/dev/tcp/127.0.0.1/$1\"' probe \"$port\" 2>/dev/null\n",
            "}}\n",
            "# Wait, outside the restart budget, until the port is released (the user\n",
            "# stopped that daemon), then return so the caller starts ours. Leaves if a\n",
            "# current daemon has taken the port meanwhile. Never stops anything.\n",
            "await_port_release() {{\n",
            "  echo \"waiting for $ADDR to be released before starting the daemon\"\n",
            "  while :; do\n",
            "    sleep \"${{PORT_RELEASE_POLL:-5}}\"\n",
            "    if ! port_is_open; then\n",
            "      echo \"$ADDR is free now; starting the daemon\"\n",
            "      write_start_status \"starting\" \"port released\"\n",
            "      return 0\n",
            "    fi\n",
            "    if probe_live_daemon && live_daemon_is_current; then\n",
            "      echo \"a current daemon pid=${{LIVE_PID:-unknown}} now serves $ADDR; leaving it alone\"\n",
            "      trap - EXIT\n",
            "      exit 0\n",
            "    fi\n",
            "  done\n",
            "}}\n",
            "# The daemon's own verdict on a failed bind (describeListenConflict in\n",
            "# daemon/updater_apply.go), from what it logged since launch_daemon.\n",
            "BIND_FAILURE=\"\"\n",
            "LAUNCH_LOG_MARK=0\n",
            "daemon_bind_failed() {{\n",
            "  local mark\n",
            "  mark=\"$LAUNCH_LOG_MARK\"\n",
            "  case \"$mark\" in ''|*[!0-9]*) mark=0 ;; esac\n",
            "  BIND_FAILURE=\"$(tail -c +\"$((mark + 1))\" \"$DAEMON_DIR/{log}\" 2>/dev/null | grep -m1 -E 'listen: (port .* is held by another soluterminal daemon|cannot bind)' | cut -c1-300 || true)\"\n",
            "  [ -n \"$BIND_FAILURE\" ]\n",
            "}}\n",
            "# Another copy of this very script, as the Windows shell launches it\n",
            "# (`bash <path>`). Our own subshells share that cmdline, so children of\n",
            "# this shell are excluded; so is anything that merely mentions the path.\n",
            "is_supervisor_pid() {{\n",
            "  local pid cmdline ppid\n",
            "  pid=\"$1\"\n",
            "  [ -n \"$pid\" ] || return 1\n",
            "  [ \"$pid\" = \"$$\" ] && return 1\n",
            "  [ -r \"/proc/$pid/cmdline\" ] || return 1\n",
            "  cmdline=\"$(tr '\\0' ' ' 2>/dev/null < \"/proc/$pid/cmdline\" || true)\"\n",
            "  case \"$cmdline\" in \"bash $SUPERVISOR_SCRIPT \"|\"/bin/bash $SUPERVISOR_SCRIPT \"|\"/usr/bin/bash $SUPERVISOR_SCRIPT \") ;; *) return 1 ;; esac\n",
            "  ppid=\"$(cut -d' ' -f4 < \"/proc/$pid/stat\" 2>/dev/null || true)\"\n",
            "  [ \"$ppid\" = \"$$\" ] && return 1\n",
            "  return 0\n",
            "}}\n",
            "# Supervisors from launchers that predate the lock never exit on their\n",
            "# own: their trap stops the daemon on TERM and then keeps looping, so each\n",
            "# one relaunches and clears the port of whatever the others started. Four\n",
            "# of them took turns killing each other's daemons for a whole morning. They\n",
            "# get SIGKILL, which no trap sees, so the daemon they supervised survives\n",
            "# for us to adopt or replace deliberately.\n",
            "retire_stray_supervisors() {{\n",
            "  local proc pid\n",
            "  for proc in $(grep -laFs -- \"$SUPERVISOR_SCRIPT\" /proc/[0-9]*/cmdline 2>/dev/null || true); do\n",
            "    proc=\"${{proc%/cmdline}}\"\n",
            "    pid=\"${{proc##*/}}\"\n",
            "    is_supervisor_pid \"$pid\" || continue\n",
            "    echo \"retiring stray supervisor pid=$pid (SIGKILL, so its trap cannot take the daemon down with it)\"\n",
            "    kill -9 \"$pid\" 2>/dev/null || true\n",
            "  done\n",
            "  return 0\n",
            "}}\n",
            "# Decide, BEFORE anything is killed, whether this run should start a\n",
            "# daemon at all.\n",
            "#\n",
            "# The Windows shell runs this script whenever ITS probe of the WSL daemon\n",
            "# fails, and that probe crosses the NAT boundary on a short budget with a\n",
            "# token it reads through wsl.exe. On a loaded host a perfectly healthy\n",
            "# daemon misses it, so the shell concludes WSL is down and runs us, and the\n",
            "# sweep below used to kill that healthy daemon on sight. Each app start\n",
            "# also left one more supervisor behind. The result read as a crash loop:\n",
            "# 30 forced restarts in one morning, every pane reattaching and every\n",
            "# workflow run interrupted each time, the pending self-update never\n",
            "# landing, and the UI parked on an empty Windows-only backend.\n",
            "#\n",
            "# So: one supervisor at a time (flock on fd 9, held for our whole life). If\n",
            "# another holds it and its daemon is healthy and current, leave both alone.\n",
            "# If we hold it and find a healthy, current daemon with no supervisor,\n",
            "# ADOPT it instead of restarting it. Only a missing, unresponsive, foreign\n",
            "# or older daemon is replaced. Exits here happen before the EXIT trap is\n",
            "# installed, so leaving never takes the daemon down.\n",
            "ADOPT_PID=\"\"\n",
            "claim_supervision() {{\n",
            "  local holder started\n",
            "  ADOPT_PID=\"\"\n",
            "  if command -v flock >/dev/null 2>&1 && exec 9>>\"$SUPERVISOR_LOCK\"; then\n",
            "    if ! flock -n 9; then\n",
            "      holder=\"$(cat \"$SUPERVISOR_PIDFILE\" 2>/dev/null || true)\"\n",
            "      echo \"supervisor pid=${{holder:-unknown}} already manages the WSL daemon\"\n",
            "      started=\"$SECONDS\"\n",
            "      while :; do\n",
            "        if probe_live_daemon; then\n",
            "          if live_daemon_is_current; then\n",
            "            echo \"its daemon pid=${{LIVE_PID:-unknown}} is healthy (version ${{LIVE_VERSION:-unknown}}); leaving it alone\"\n",
            "            exit 0\n",
            "          fi\n",
            "          echo \"its port answers as backend ${{LIVE_BACKEND:-none}} version ${{LIVE_VERSION:-unknown}} (bundle $BUNDLE_VERSION); taking over to replace it\"\n",
            "          break\n",
            "        fi\n",
            "        if flock -n 9; then break; fi\n",
            "        # Not answering yet. A big session reattaching dozens of panes on a\n",
            "        # busy host has taken over a minute to serve, so wait before\n",
            "        # deciding it is wedged.\n",
            "        if [ \"$((SECONDS - started))\" -ge 90 ]; then\n",
            "          echo \"its daemon has not answered for $((SECONDS - started))s; taking over\"\n",
            "          break\n",
            "        fi\n",
            "        sleep 5\n",
            "      done\n",
            "      if ! flock -n 9; then\n",
            "        if is_supervisor_pid \"$holder\"; then\n",
            "          echo \"stopping supervisor pid=$holder\"\n",
            "          kill \"$holder\" 2>/dev/null || true\n",
            "        fi\n",
            "        if ! flock -w 20 9; then\n",
            "          if is_supervisor_pid \"$holder\"; then kill -9 \"$holder\" 2>/dev/null || true; fi\n",
            "          if ! flock -w 5 9; then\n",
            "            echo \"could not take over from supervisor pid=${{holder:-unknown}}; leaving the daemon alone\"\n",
            "            exit 1\n",
            "          fi\n",
            "        fi\n",
            "      fi\n",
            "    fi\n",
            "  else\n",
            "    echo \"no flock(1); running without the single-supervisor guard\"\n",
            "  fi\n",
            "  echo \"$$\" > \"$SUPERVISOR_PIDFILE\" 2>/dev/null || true\n",
            "  retire_stray_supervisors\n",
            "  if probe_live_daemon; then\n",
            "    if ! live_daemon_is_current; then\n",
            "      echo \"daemon on $ADDR is backend ${{LIVE_BACKEND:-unknown}} version ${{LIVE_VERSION:-unknown}} (bundle $BUNDLE_VERSION); replacing it\"\n",
            "      return 0\n",
            "    fi\n",
            "    if [ -z \"$LIVE_VERSION\" ]; then echo \"daemon on $ADDR reported no version; treating it as current\"; fi\n",
            "    if [ -n \"$LIVE_PID\" ] && [ -r \"/proc/$LIVE_PID/cmdline\" ]; then\n",
            "      ADOPT_PID=\"$LIVE_PID\"\n",
            "      echo \"daemon pid=$ADOPT_PID on $ADDR is healthy (version ${{LIVE_VERSION:-unknown}}); adopting it instead of restarting it\"\n",
            "      return 0\n",
            "    fi\n",
            "    echo \"a healthy daemon answers on $ADDR but pid ${{LIVE_PID:-unknown}} is not visible from here; leaving it alone\"\n",
            "    exit 0\n",
            "  fi\n",
            "  return 0\n",
            "}}\n",
            "# Stop only the WSL daemon this Windows shell manages. Match both the\n",
            "# current staged binary path and the launch flags older installers used\n",
            "# so upgrades can replace stale dev-checkout daemons on the same port.\n",
            "# Runs only when claim_supervision found nothing worth keeping.\n",
            "stop_stale_managed_daemons() {{\n",
            "  if [ -s \"$PIDFILE\" ]; then\n",
            "    oldpid=\"$(cat \"$PIDFILE\" 2>/dev/null || true)\"\n",
            "    if [ -n \"$oldpid\" ] && [ -r \"/proc/$oldpid/cmdline\" ]; then\n",
            "      oldcmd=\"$(tr '\\0' ' ' < \"/proc/$oldpid/cmdline\" 2>/dev/null || true)\"\n",
            "      case \"$oldcmd\" in\n",
            "        *\"$LOCAL\"*\" --addr 0.0.0.0:8787\"*|*\" --addr 0.0.0.0:8787\"*\" --backend-id wsl\"*\" --shell-platform windows-amd64\"*)\n",
            "          echo \"stopping previous managed daemon pid=$oldpid\"\n",
            "          kill \"$oldpid\" 2>/dev/null || true\n",
            "          sleep 0.5\n",
            "          kill -9 \"$oldpid\" 2>/dev/null || true\n",
            "          ;;\n",
            "      esac\n",
            "    fi\n",
            "  fi\n",
            "  # Older launchers did not always leave a useful PID file, especially\n",
            "  # across installer upgrades. If a managed Windows-shell WSL daemon is\n",
            "  # still bound to the WSL port, stop it before execing this shell's\n",
            "  # daemon so the Windows proxy cannot attach to a stale shell-version.\n",
            "  # One grep finds the candidates (see stop_port_holder), and the second\n",
            "  # pass SIGKILLs only what the first pass asked to stop; stop_port_holder\n",
            "  # still clears the real port owner right before the bind. A process can\n",
            "  # exit between the -r test and the read, so the read is guarded: under\n",
            "  # set -e an unguarded one would abort the supervisor.\n",
            "  stale=\"\"\n",
            "  for proc in $(grep -laFs -- \"$ADDR\" /proc/[0-9]*/cmdline 2>/dev/null || true); do\n",
            "    proc=\"${{proc%/cmdline}}\"\n",
            "    pid=\"${{proc##*/}}\"\n",
            "    [ \"$pid\" = \"$$\" ] && continue\n",
            "    [ -r \"$proc/cmdline\" ] || continue\n",
            "    oldcmd=\"$(tr '\\0' ' ' 2>/dev/null < \"$proc/cmdline\" || true)\"\n",
            "    case \"$oldcmd\" in\n",
            "      *\"$LOCAL\"*\" --addr 0.0.0.0:8787\"*|*\" --addr 0.0.0.0:8787\"*\" --backend-id wsl\"*\" --shell-platform windows-amd64\"*)\n",
            "        echo \"stopping stale managed daemon pid=$pid\"\n",
            "        kill \"$pid\" 2>/dev/null || true\n",
            "        stale=\"$stale $pid\"\n",
            "        ;;\n",
            "    esac\n",
            "  done\n",
            "  if [ -n \"$stale\" ]; then\n",
            "    sleep 0.5\n",
            "    for pid in $stale; do\n",
            "      if kill -0 \"$pid\" 2>/dev/null; then\n",
            "        echo \"force-stopping stale managed daemon pid=$pid\"\n",
            "        kill -9 \"$pid\" 2>/dev/null || true\n",
            "      fi\n",
            "    done\n",
            "  fi\n",
            "  # Give the kernel a beat to release the port before we bind.\n",
            "  sleep 0.3\n",
            "  return 0\n",
            "}}\n",
            "claim_supervision\n",
            "if [ -z \"$ADOPT_PID\" ]; then\n",
            "  stop_stale_managed_daemons\n",
            "fi\n",
            "# Stage the daemon binary in a user-writable WSL ext4 location.\n",
            "# `cp -f` doesn't work here: previous copies can inherit mode 555\n",
            "# from /mnt/c (NTFS gives us no write bit), and some coreutils builds\n",
            "# refuse to open that target for writing even with -f.\n",
            "#\n",
            "# Logic:\n",
            "#   - LOCAL missing or empty                 → copy from BUNDLE\n",
            "#   - LOCAL has no version marker            → keep LOCAL to avoid downgrades\n",
            "#   - active hold names BUNDLE_VERSION       → keep rolled-back LOCAL\n",
            "#   - BUNDLE_VERSION > local version marker  → copy from BUNDLE\n",
            "#   - otherwise                              → keep LOCAL\n",
            "#\n",
            "# The updater refreshes LOCAL.version after a successful self-update;\n",
            "# comparing that marker avoids mtime-based downgrades when an MSI's\n",
            "# bundled daemon is older than the WSL daemon's auto-updated copy.\n",
            "stage_reason=\"\"\n",
            "local_version=\"\"\n",
            "[ -s \"$LOCAL_VERSION_FILE\" ] && local_version=\"$(head -n1 \"$LOCAL_VERSION_FILE\" | tr -d '[:space:]')\"\n",
            "# Self-heal: only trust a staged copy that is a runnable ELF. A\n",
            "# stale/corrupt/wrong-arch $LOCAL (e.g. an amd64 daemon a prior\n",
            "# install left on this now-arm64 box) must be re-staged regardless\n",
            "# of the version marker, or every launch dies with Exec format error.\n",
            "local_ok=0\n",
            "if [ -s \"$LOCAL\" ] && [ \"$(head -c4 \"$LOCAL\" | od -An -tx1 | tr -dc 0-9a-f)\" = \"7f454c46\" ]; then local_ok=1; fi\n",
            "if [ \"$local_ok\" != \"1\" ]; then\n",
            "  stage_reason=\"local missing/corrupt (not a runnable ELF)\"\n",
            "elif [ -z \"$local_version\" ]; then\n",
            "  echo \"staging daemon: keeping local (missing version marker; avoiding downgrade)\"\n",
            "elif rollback_hold_active \"$BUNDLE_VERSION\"; then\n",
            "  echo \"staging daemon: keeping rolled-back local version $local_version (bundle $BUNDLE_VERSION is held)\"\n",
            "elif ver_gt \"$BUNDLE_VERSION\" \"$local_version\"; then\n",
            "  stage_reason=\"bundle version $BUNDLE_VERSION > local version $local_version\"\n",
            "else\n",
            "  echo \"staging daemon: keeping local version $local_version (bundle $BUNDLE_VERSION)\"\n",
            "fi\n",
            "if [ -n \"$stage_reason\" ]; then\n",
            "  echo \"staging daemon: decompressing from bundle ($stage_reason)\"\n",
            "  rm -f \"$LOCAL\"\n",
            "  gunzip -c \"$BUNDLE\" > \"$LOCAL\"\n",
            "  chmod 755 \"$LOCAL\"\n",
            "  if [ \"$(head -c4 \"$LOCAL\" | od -An -tx1 | tr -dc 0-9a-f)\" != \"7f454c46\" ]; then echo \"staged daemon not a valid ELF after decompress\" >&2; exit 1; fi\n",
            "  printf '%s\\n' \"$BUNDLE_VERSION\" > \"$LOCAL_VERSION_FILE\"\n",
            "fi\n",
            "trap cleanup_managed_daemon EXIT\n",
            "# INT/TERM are a deliberate stop, and a stop has to STOP the supervisor.\n",
            "# The handler used to be the cleanup alone, so bash ran it and then\n",
            "# resumed the loop, whose `wait` had just returned: a supervisor told to\n",
            "# stop killed its daemon and immediately relaunched it.\n",
            "on_stop_signal() {{\n",
            "  cleanup_managed_daemon\n",
            "  trap - EXIT\n",
            "  exit 143\n",
            "}}\n",
            "trap on_stop_signal INT TERM\n",
            "# SIGHUP is not a stop request. It is what this shell gets when the\n",
            "# Windows side goes away (the app quits, restarts or updates and its\n",
            "# wsl.exe is torn down); the daemon, our background child, gets nothing.\n",
            "# Treating it as a stop killed the daemon on every app restart, and the\n",
            "# supervisor, reparented to init, relaunched it and lingered, one more\n",
            "# per restart. Keep supervising: the next app start finds this healthy\n",
            "# daemon and its supervisor and reuses them without restarting anything.\n",
            "on_hangup() {{\n",
            "  echo \"windows side disconnected (SIGHUP); daemon pid=${{daemon_pid:-none}} keeps running\"\n",
            "}}\n",
            "trap on_hangup HUP\n",
            // --relay makes the daemon pick up ~/.solu/claim.json
            // automatically and enable cloud-proxied voice / push.
            // If the user hasn't paired yet the daemon logs a warning
            // and continues with cloud features disabled — safe to
            // always pass. --backend-id wsl identifies this daemon to
            // the cloud when Tauri Dual spawns both WSL+Windows.
            "# `9>&-`: the daemon must not inherit the supervisor lock. flock locks\n",
            "# belong to the open file, so a daemon holding fd 9 would keep the lock\n",
            "# after its supervisor died and turn away every later supervisor.\n",
            "launch_daemon() {{\n",
            "  LAUNCH_LOG_MARK=\"$(wc -c < \"$DAEMON_DIR/{log}\" 2>/dev/null | tr -dc '0-9' || true)\"\n",
            "  \"$LOCAL\" --addr \"$ADDR\" {relay}--backend-id {backend} --shell-version {shellver} --shell-platform {shellplat} 9>&- &\n",
            "  daemon_pid=\"$!\"\n",
            "  daemon_is_child=1\n",
            "  echo \"$daemon_pid\" > \"$PIDFILE\"\n",
            "}}\n",
            "# Block until $daemon_pid exits, leaving its code in $status.\n",
            "#\n",
            "# `wait` only works on this shell's own children. A self-update\n",
            "# successor is forked by the outgoing daemon, not by us, so waiting\n",
            "# on one fails instantly with \"not a child of this shell\" (status\n",
            "# 127) instead of blocking -- which used to fall through to the\n",
            "# `exit` below and tear down the whole backend mid-update. Poll\n",
            "# those with kill -0 instead, following any newer successor.\n",
            "await_daemon() {{\n",
            "  if [ \"$daemon_is_child\" = \"1\" ]; then\n",
            "    set +e\n",
            "    wait \"$daemon_pid\"\n",
            "    status=\"$?\"\n",
            "    # A trapped signal (SIGHUP above) cuts `wait` short with 128+n while\n",
            "    # the daemon runs on. Read as an exit, that relaunched the daemon and\n",
            "    # swept the port first, killing the live one. Keep waiting instead;\n",
            "    # a dead daemon is still reaped here with its real status.\n",
            "    while [ \"$status\" -gt 128 ] && kill -0 \"$daemon_pid\" 2>/dev/null; do\n",
            "      wait \"$daemon_pid\"\n",
            "      status=\"$?\"\n",
            "    done\n",
            "    set -e\n",
            "    return 0\n",
            "  fi\n",
            "  while kill -0 \"$daemon_pid\" 2>/dev/null; do\n",
            "    sleep 0.5\n",
            "    next_pid=\"$(cat \"$PIDFILE\" 2>/dev/null || true)\"\n",
            "    if [ -n \"$next_pid\" ] && [ \"$next_pid\" != \"$daemon_pid\" ]; then\n",
            "      echo \"supervising newer update successor pid=$next_pid\"\n",
            "      daemon_pid=\"$next_pid\"\n",
            "    fi\n",
            "  done\n",
            "  status=0\n",
            "}}\n",
            "if [ -n \"$ADOPT_PID\" ]; then\n",
            "  # Supervise the healthy daemon claim_supervision found, exactly like an\n",
            "  # update successor: poll it, follow the pidfile, relaunch if it dies.\n",
            "  echo \"=== supervising adopted daemon pid=$ADOPT_PID (not restarted) ===\"\n",
            "  daemon_pid=\"$ADOPT_PID\"\n",
            "  daemon_is_child=0\n",
            "  echo \"$daemon_pid\" > \"$PIDFILE\"\n",
            "else\n",
            "  # Final ownership check before the first bind, in case the pidfile and\n",
            "  # /proc scans above raced a successor that appeared between passes.\n",
            "  stop_port_holder\n",
            "  # Whatever still answers here survived every cleanup we may run; our\n",
            "  # daemon would only load its state and fail to bind, ten times over.\n",
            "  if port_held_by_other_daemon; then\n",
            "    report_port_blocked port-held \"it still answers after the stale-daemon cleanup\"\n",
            "    await_port_release\n",
            "  fi\n",
            "  echo \"=== start daemon under WSL supervisor ===\"\n",
            "  launch_daemon\n",
            "fi\n",
            "restarts=0\n",
            "while true; do\n",
            "  started_at=\"$SECONDS\"\n",
            "  await_daemon\n",
            "  # Re-read the marker after the wait: a successor can appear in the\n",
            "  # window between our last poll and the old daemon exiting.\n",
            "  next_pid=\"$(cat \"$PIDFILE\" 2>/dev/null || true)\"\n",
            "  if [ \"$status\" = \"0\" ] && [ -n \"$next_pid\" ] && [ \"$next_pid\" != \"$daemon_pid\" ] && kill -0 \"$next_pid\" 2>/dev/null; then\n",
            "    echo \"supervising update successor pid=$next_pid\"\n",
            "    daemon_pid=\"$next_pid\"\n",
            "    daemon_is_child=0\n",
            "    continue\n",
            "  fi\n",
            "  # The daemon is gone for good: a crash, an OOM kill, or a successor\n",
            "  # that exited without handing off. Relaunch rather than exit --\n",
            "  # exiting leaves the Windows shell with no WSL backend and every\n",
            "  # pane dark until the user restarts the app by hand. A deliberate\n",
            "  # stop still arrives as INT/TERM/HUP and unwinds via the trap.\n",
            "  # A daemon that stayed up a while earned a fresh budget, so a\n",
            "  # long-lived backend is never capped by restarts from days ago.\n",
            "  # Status 73: another daemon already serves this data directory (the\n",
            "  # daemon's own log line names it). Relaunching cannot succeed until\n",
            "  # that one stops, and ten attempts would only bury the reason.\n",
            "  if [ \"$status\" = \"73\" ]; then\n",
            "    echo \"daemon exited (status=73): another soluterminal daemon serves this data directory; not relaunching\"\n",
            "    exit 73\n",
            "  fi\n",
            "  # A bind failure the daemon itself reported. If the port is still held\n",
            "  # a moment later, relaunching cannot succeed: report it to the Windows\n",
            "  # shell and wait for the port instead of burning the restart budget.\n",
            "  if [ \"$status\" != \"0\" ] && daemon_bind_failed; then\n",
            "    sleep 2\n",
            "    if port_held_by_other_daemon; then\n",
            "      report_port_blocked port-held \"$BIND_FAILURE\"\n",
            "      await_port_release\n",
            "      restarts=0\n",
            "    elif ! probe_live_daemon && port_is_open; then\n",
            "      report_port_blocked bind-failed \"$BIND_FAILURE\"\n",
            "      await_port_release\n",
            "      restarts=0\n",
            "    fi\n",
            "  fi\n",
            "  if [ \"$SECONDS\" -ge \"$((started_at + 120))\" ]; then restarts=0; fi\n",
            "  if [ \"$restarts\" -ge 10 ]; then\n",
            "    echo \"daemon exited (status=$status); restart cap reached, giving up\"\n",
            "    exit \"$status\"\n",
            "  fi\n",
            "  restarts=\"$((restarts + 1))\"\n",
            "  echo \"daemon exited (status=$status); relaunching in 2s (attempt $restarts)\"\n",
            "  # The death notice above is now the LAST line of this run's log.\n",
            "  # Rotate only if the file has outgrown the cap: an unbroken session\n",
            "  # log -- staging decisions, start banner, every crash in order -- is\n",
            "  # worth more than a fresh file per attempt, and a crash loop would\n",
            "  # otherwise evict the first failure before anyone reads it. Re-issue\n",
            "  # the redirect either way; after a rotation our fd would still point\n",
            "  # at the renamed daemon.log.1.\n",
            "  rotate_daemon_log_if_big\n",
            "  exec >> \"$DAEMON_DIR/{log}\" 2>&1\n",
            "  echo \"=== $(date -Is) relaunching daemon (attempt $restarts) ===\"\n",
            "  sleep 2\n",
            "  # The usual reason a relaunch cannot bind is an orphan successor that\n",
            "  # outlived the handoff. Clear the actual port owner here, not just in the\n",
            "  # one-shot startup scan, or every attempt fails identically and the\n",
            "  # supervisor reaches its cap with the orphan still serving.\n",
            "  stop_port_holder\n",
            "  launch_daemon\n",
            "done\n",
        ),
        log = daemon_log_name,
        status = WSL_START_STATUS_FILE,
        pid_listens = PID_LISTENS_ON_PORT_FN,
        // Same cap the host-native supervisor rotates its daemon.log at, so
        // one platform's logs never age out faster than the other's.
        logmax = DAEMON_LOG_MAX_BYTES,
        src_amd64 = bundled_amd64_wsl,
        src_arm64 = bundled_arm64_wsl,
        relay = relay_args().iter().map(|arg| format!("{arg} ")).collect::<String>(),
        backend = BACKEND_ID_WSL,
        // Plumb our (Tauri-shell) version into the WSL solud so the
        // shell-update channel is consistent across both backends.
        shellver = env!("SOLU_SHELL_VERSION"),
        // build-sidecar-binaries.sh bakes the same tauri.conf.json
        // version into solud-linux via -ldflags, so this is also the
        // bundled daemon version for marker comparisons.
        daemonver = env!("SOLU_SHELL_VERSION"),
        // The WSL spawn path is Windows-only (the file lives behind
        // #[cfg(windows)]). The shell is always windows-amd64 here,
        // even though the daemon runs on linux-amd64 — they're
        // different binaries, and the manifest is keyed on the SHELL.
        shellplat = "windows-amd64",
    )
}

/// Spawn `solud.exe router` to multiplex both daemons behind a single
/// origin on `listen_addr`. Called in Backend::Dual after both
/// daemons are up. Returns the SidecarGuard holding the router child
/// process; caller chains it into the overall merged guard.
#[cfg(windows)]
pub fn ensure_router_running(
    listen_addr: &str,
    wsl_upstream: &str,
    win_upstream: &str,
    wsl_token: &str,
    win_token: &str,
) -> Result<SidecarGuard, String> {
    launch_log(&format!(
        "router: spawning on {listen_addr} (wsl={wsl_upstream} win={win_upstream})"
    ));
    let bin = adjacent_to_self()
        .filter(|p| p.is_file())
        .or_else(|| which_in_path("solud.exe"))
        .ok_or_else(|| "router: solud.exe not found next to Tauri exe".to_string())?;

    let mut cmd = Command::new(&bin);
    cmd.args([
        "router",
        "--addr",
        listen_addr,
        "--wsl",
        wsl_upstream,
        "--win",
        win_upstream,
        "--wsl-token",
        wsl_token,
        "--win-token",
        win_token,
    ])
    .stdout(Stdio::inherit())
    .stderr(Stdio::inherit())
    .creation_flags(CREATE_NO_WINDOW);
    let child = cmd
        .spawn()
        .map_err(|e| format!("router: spawn {}: {}", bin.display(), e))?;
    job::assign(&child);

    // Wait briefly for the router to bind.
    let deadline = Instant::now() + Duration::from_secs(3);
    while Instant::now() < deadline {
        if port_alive(listen_addr) {
            launch_log(&format!("router: ready on {listen_addr}"));
            return Ok(SidecarGuard {
                child: Some(child),
                supervisors: Vec::new(),
                proxy: None,
                extras: Vec::new(),
                extra_proxies: Vec::new(),
            });
        }
        thread::sleep(Duration::from_millis(80));
    }
    launch_log(&format!(
        "router: didn't bind {listen_addr} within 3s — proceeding anyway"
    ));
    Ok(SidecarGuard {
        child: Some(child),
        supervisors: Vec::new(),
        proxy: None,
        extras: Vec::new(),
        extra_proxies: Vec::new(),
    })
}

/// Read the Windows-native daemon's token from %USERPROFILE%\.solu\token.
/// Same session-token pattern as the Linux daemon's ~/.solu/token; the
/// daemon writes it with 0600 on first run. Used by the router to
/// authenticate cross-daemon requests via Authorization: Bearer.
#[cfg(windows)]
pub fn read_win_token() -> Result<String, String> {
    let Some(home) = dirs::home_dir() else {
        return Err("no user home".into());
    };
    let path = home.join(".solu").join("token");
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut last_err = String::from("not attempted");
    while Instant::now() < deadline {
        match std::fs::read_to_string(&path) {
            Ok(s) => {
                let tok = s.trim().to_string();
                if !tok.is_empty() {
                    return Ok(tok);
                }
                last_err = "empty token".into();
            }
            Err(e) => last_err = format!("read {}: {e}", path.display()),
        }
        thread::sleep(Duration::from_millis(150));
    }
    Err(last_err)
}

/// Read the daemon's auth token from `~/.solu/token` inside WSL.
/// Wait briefly if the file hasn't been written yet — the daemon writes
/// it on startup and we're racing that.
#[cfg(windows)]
pub fn read_wsl_token() -> Result<String, String> {
    read_wsl_token_with(Duration::from_secs(3), false)
}

/// Re-read the token past the cache, with a budget sized for a loaded host.
/// Only for the startup identity decision after the daemon answered 401: a
/// 401 proves the daemon is alive, so the wrong response to it is the one we
/// used to give (a missing token read as "WSL is down", followed by a respawn
/// that killed it). Three bounded wsl.exe calls at 3s each were routinely too
/// short at load 70+.
#[cfg(windows)]
fn read_wsl_token_patiently() -> Result<String, String> {
    read_wsl_token_with(Duration::from_secs(10), true)
}

#[cfg(windows)]
fn read_wsl_token_with(timeout: Duration, bypass_cache: bool) -> Result<String, String> {
    // Single-flight + cache. The token is stable for the life of a daemon
    // install, and this used to be a 150ms retry loop of unbounded wsl.exe
    // spawns called PER PROXIED CONNECTION during outages — a major
    // contributor to the 2026-08-12 wsl.exe storm. Success is cached for
    // 60s; failure for 3s so a dead WSL doesn't get hammered.
    struct TokenCache {
        ok: Option<(String, Instant)>,
        err: Option<(String, Instant)>,
    }
    static TOKEN: Mutex<TokenCache> = Mutex::new(TokenCache { ok: None, err: None });

    let mut st = TOKEN.lock().unwrap();
    let now = Instant::now();
    if !bypass_cache {
        if let Some((tok, at)) = &st.ok {
            if now.duration_since(*at) < Duration::from_secs(60) {
                return Ok(tok.clone());
            }
        }
        if let Some((e, at)) = &st.err {
            if now.duration_since(*at) < Duration::from_secs(3) {
                return Err(e.clone());
            }
        }
    }
    let cmd = concat!(
        "uid=\"$(id -u)\"; ",
        "home=\"$(getent passwd \"$uid\" | cut -d: -f6 || true)\"; ",
        "if [ -z \"$home\" ]; then user=\"$(id -un 2>/dev/null || echo \"${USER:-user}\")\"; home=\"/home/$user\"; fi; ",
        "cat \"$home/.solu/token\" 2>/dev/null",
    );
    let res = match run_wsl_probe_bounded(&["-e", "bash", "-c", cmd], timeout) {
        Ok(o) if o.status.success() => {
            let tok = String::from_utf8_lossy(&o.stdout).trim().to_string();
            if tok.is_empty() {
                Err("empty token".to_string())
            } else {
                Ok(tok)
            }
        }
        Ok(o) => Err(format!(
            "cat exit {:?}: {}",
            o.status,
            String::from_utf8_lossy(&o.stderr).trim()
        )),
        Err(e) => Err(e),
    };
    match &res {
        Ok(tok) => {
            st.ok = Some((tok.clone(), Instant::now()));
            st.err = None;
        }
        Err(e) => st.err = Some((e.clone(), Instant::now())),
    }
    res
}

// --- Dual-mode claim mirroring ----------------------------------------
//
// In Dual mode two independent daemons run on one PC — the WSL daemon
// (backend=wsl) and the Windows-native daemon (backend=win) — and each
// reads its cloud claim.json from its OWN data dir on its OWN filesystem
// (WSL `$HOME/.solu` vs Windows `%USERPROFILE%\.solu`). Web-UI pairing
// only writes the claim to whichever daemon served the request, so the
// other daemon stays UNPAIRED: it never dials the relay, never pulls the
// team-credential set, and so team-account auto-switch silently never
// fires on that backend. (Confirmed in the wild: a member's relay logs
// showed only backend=wsl ever connecting.)
//
// The documented Dual design is "one shared claim, two backend IDs", so
// the fix is to keep both daemons' claim.json in sync. A background loop
// mirrors the claim between the two filesystems:
//   - exactly one side has a claim  → copy it to the other (the pairing
//     case: the unpaired daemon gets paired and connects within its ~5s
//     claim poll).
//   - both sides, same machineId, tokens differ → the NEWER (by mtime)
//     wins. The cloud keeps both the old and pending token valid during
//     a rotation grace window, so copying the freshly-rotated token over
//     a lagging one converges them safely. Newer-wins guarantees we never
//     push a post-grace-dead token over a live one.
//   - both sides, DIFFERENT machineIds → leave them alone. They are two
//     independent pairings under the same cloud user; both pull the same
//     team set on their own, so nothing is broken.
//   - identical bytes → no-op (steady state; no write, no relay churn).
//
// Writes are atomic on both sides (temp + rename) so a daemon never reads
// a torn claim mid-mirror. The loop lives for the Tauri process lifetime;
// it dies with the process (no explicit shutdown needed).

/// Path to the Windows-native daemon's claim.json — `%USERPROFILE%\.solu\
/// claim.json`. Mirrors the daemon's own `defaultDataDir()` (UserHomeDir
/// + ".solu") and pair::native_claim_path().
#[cfg(windows)]
fn win_claim_path() -> Option<PathBuf> {
    Some(dirs::home_dir()?.join(".solu").join("claim.json"))
}

/// Read the Windows-native claim bytes, or None when absent/empty.
#[cfg(windows)]
fn read_win_claim_bytes() -> Option<Vec<u8>> {
    let p = win_claim_path()?;
    std::fs::read(&p).ok().filter(|b| !b.is_empty())
}

/// Windows claim mtime as epoch seconds (for the rotation newer-wins tie-break).
#[cfg(windows)]
fn win_claim_mtime() -> Option<i64> {
    let p = win_claim_path()?;
    let meta = std::fs::metadata(&p).ok()?;
    let secs = meta
        .modified()
        .ok()?
        .duration_since(std::time::UNIX_EPOCH)
        .ok()?
        .as_secs();
    Some(secs as i64)
}

/// Write the Windows-native claim atomically (temp + rename).
#[cfg(windows)]
fn write_win_claim_bytes(bytes: &[u8]) -> Result<(), String> {
    let p = win_claim_path().ok_or_else(|| "no user home".to_string())?;
    if let Some(dir) = p.parent() {
        std::fs::create_dir_all(dir).map_err(|e| format!("mkdir {}: {e}", dir.display()))?;
    }
    let tmp = p.with_extension("json.tmp");
    std::fs::write(&tmp, bytes).map_err(|e| format!("write {}: {e}", tmp.display()))?;
    std::fs::rename(&tmp, &p).map_err(|e| format!("rename {}: {e}", p.display()))?;
    Ok(())
}

/// Bash prefix that resolves the WSL home into `$home` via the passwd db, NOT
/// the inherited `$HOME` — Windows env can bleed into a `wsl.exe` shell, so a
/// raw `$HOME` may point at a Windows path. This MUST match read_wsl_token and
/// the daemon's own start-wsl.sh launcher so the mirror targets the EXACT
/// claim.json the WSL daemon reads/writes (otherwise it would silently sync a
/// file the daemon never looks at). Every WSL claim helper below prepends this
/// and then uses `"$home/.solu/..."`.
#[cfg(windows)]
const WSL_HOME_PREFIX: &str = "uid=\"$(id -u)\"; home=\"$(getent passwd \"$uid\" | cut -d: -f6 || true)\"; if [ -z \"$home\" ]; then user=\"$(id -un 2>/dev/null || echo \"${USER:-user}\")\"; home=\"/home/$user\"; fi; ";

/// Budget for one claim-mirror wsl.exe call. Generous enough for a loaded host,
/// bounded so a hung VM can never park the mirror (or its session) forever.
#[cfg(windows)]
const CLAIM_MIRROR_WSL_TIMEOUT: Duration = Duration::from_secs(10);

/// Exit status the claim read uses to say "no claim.json", so an absent file
/// is never confused with a wsl.exe call that failed.
#[cfg(windows)]
const WSL_CLAIM_ABSENT_EXIT: i32 = 3;

/// Read the WSL daemon's claim bytes. `Ok(None)` means the file is verifiably
/// absent or empty; `Err` means WSL could not answer (timeout, interop
/// failure, distro not running). Callers must never treat `Err` as absent: the
/// mirror used to, and under load it rewrote a claim that was there hundreds
/// of times, each rewrite another wsl.exe session. Bounded, and it never wakes
/// a stopped distro (run_wsl_probe_bounded). Home resolved via WSL_HOME_PREFIX
/// (passwd db), matching the WSL daemon's own claim location.
#[cfg(windows)]
fn read_wsl_claim_bytes() -> Result<Option<Vec<u8>>, String> {
    let cmd = format!(
        "{WSL_HOME_PREFIX}f=\"$home/.solu/claim.json\"; [ -s \"$f\" ] || exit {WSL_CLAIM_ABSENT_EXIT}; cat \"$f\""
    );
    let out = run_wsl_probe_bounded(&["-e", "bash", "-c", &cmd], CLAIM_MIRROR_WSL_TIMEOUT)?;
    match out.status.code() {
        Some(0) if !out.stdout.is_empty() => Ok(Some(out.stdout)),
        Some(WSL_CLAIM_ABSENT_EXIT) => Ok(None),
        _ => Err(format!(
            "wsl claim read exit {:?}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        )),
    }
}

/// WSL claim mtime as epoch seconds (`stat -c %Y`). Bounded; `Err` when WSL
/// could not answer.
#[cfg(windows)]
fn wsl_claim_mtime() -> Result<i64, String> {
    let cmd = format!("{WSL_HOME_PREFIX}stat -c %Y \"$home/.solu/claim.json\"");
    let out = run_wsl_probe_bounded(&["-e", "bash", "-c", &cmd], CLAIM_MIRROR_WSL_TIMEOUT)?;
    if !out.status.success() {
        return Err(format!("wsl claim stat exit {:?}", out.status));
    }
    String::from_utf8_lossy(&out.stdout)
        .trim()
        .parse::<i64>()
        .map_err(|e| format!("wsl claim stat output: {e}"))
}

/// Write the WSL claim atomically (temp + mv) by piping the bytes to a
/// `cat`/`mv` one-liner over stdin — avoids quoting the JSON through the
/// Windows→wsl.exe→bash interop chain. umask 077 keeps it 0600. Home resolved
/// via WSL_HOME_PREFIX so it lands where the WSL daemon reads it. Bounded like
/// every other mirror call.
#[cfg(windows)]
fn write_wsl_claim_bytes(bytes: &[u8]) -> Result<(), String> {
    let cmd = format!(
        "{WSL_HOME_PREFIX}mkdir -p \"$home/.solu\" && umask 077 && cat > \"$home/.solu/claim.json.tmp\" && mv -f \"$home/.solu/claim.json.tmp\" \"$home/.solu/claim.json\""
    );
    let out = run_wsl_probe_bounded_with_stdin(
        &["-e", "bash", "-c", &cmd],
        Some(bytes),
        CLAIM_MIRROR_WSL_TIMEOUT,
    )?;
    if !out.status.success() {
        return Err(format!(
            "wsl claim write exit {:?}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(())
}

/// Extract `machineId` from claim.json bytes so we only auto-converge a
/// rotation drift WITHIN one machine identity (never across two genuinely
/// separate pairings).
#[cfg(windows)]
fn claim_machine_id(bytes: &[u8]) -> Option<String> {
    let v: serde_json::Value = serde_json::from_slice(bytes).ok()?;
    v.get("machineId")?.as_str().map(|s| s.to_string())
}

/// One mirror pass between the WSL and Windows claim files. See the module
/// note above for the rules. `Err` means WSL could not be read or written; the
/// loop backs off rather than retrying every tick (see ClaimMirrorSchedule).
#[cfg(windows)]
fn sync_dual_claim_once(win: Option<Vec<u8>>) -> Result<(), String> {
    // Claim maintenance is optional. Do not let this background loop wake a
    // stopped distro (or add writes) while WSL starts are blocked by pressure.
    if !wsl_start_allowed() {
        return Ok(());
    }
    match (read_wsl_claim_bytes()?, win) {
        (Some(wsl), None) => {
            write_win_claim_bytes(&wsl).map_err(|e| format!("WSL→Win: {e}"))?;
            launch_log("claim-mirror: copied WSL claim → Windows (Windows had none)");
        }
        (None, Some(win)) => {
            write_wsl_claim_bytes(&win).map_err(|e| format!("Win→WSL: {e}"))?;
            launch_log("claim-mirror: copied Windows claim → WSL (WSL had none)");
        }
        (Some(wsl), Some(win)) => {
            if wsl == win {
                return Ok(()); // steady state
            }
            // Only converge a token drift within the SAME machine identity.
            // Different machineIds = two independent pairings; both work on
            // their own (same user → same team set), so don't fight them.
            let same_machine = matches!(
                (claim_machine_id(&wsl), claim_machine_id(&win)),
                (Some(a), Some(b)) if a == b
            );
            if !same_machine {
                return Ok(());
            }
            // Newer (by mtime) wins: it carries the freshly-rotated token.
            if wsl_claim_mtime()? >= win_claim_mtime().unwrap_or(0) {
                write_win_claim_bytes(&wsl).map_err(|e| format!("WSL→Win (rotation): {e}"))?;
                launch_log("claim-mirror: synced rotated token WSL→Windows");
            } else {
                write_wsl_claim_bytes(&win).map_err(|e| format!("Win→WSL (rotation): {e}"))?;
                launch_log("claim-mirror: synced rotated token Windows→WSL");
            }
        }
        (None, None) => {} // neither paired yet — nothing to mirror
    }
    Ok(())
}

/// Start the Dual-mode claim mirror loop for the process lifetime. Call once,
/// only in Dual mode with both daemons running. The first pass runs
/// immediately so a pre-existing pairing fixes the unpaired side at launch.
///
/// Each pass costs at least one wsl.exe session (a new WSL init plus its
/// Hyper-V socket channels). This loop used to run one every 10s, unbounded,
/// forever — ~8,600 sessions a day at steady state, and under load hundreds of
/// failed calls whose half-built sessions leaked hv_sock channels until new
/// WSL terminals could not start (2026-09-24: ~1,600 leaked channels, cleared
/// only by a reboot). The Windows side is watched locally every tick for free;
/// WSL is consulted only when the Windows claim changed, on a slow steady
/// cadence, and with exponential backoff while it is failing.
#[cfg(windows)]
pub fn start_dual_claim_mirror() {
    launch_log("claim-mirror: starting Dual claim sync loop");
    thread::spawn(|| {
        let mut schedule = ClaimMirrorSchedule::new(Instant::now());
        loop {
            let win = read_win_claim_bytes();
            let now = Instant::now();
            if schedule.due(now, win.as_deref()) {
                match sync_dual_claim_once(win.clone()) {
                    // Re-read: a WSL→Windows copy just changed it, and our own
                    // write must not look like a fresh Windows-side change.
                    Ok(()) => schedule.succeeded(now, read_win_claim_bytes()),
                    Err(e) => {
                        let backoff = schedule.failed(now);
                        if schedule.should_log_failure() {
                            launch_log(&format!(
                                "claim-mirror failed ({} in a row; next WSL check in {}s): {e}",
                                schedule.fail_streak(),
                                backoff.as_secs()
                            ));
                        }
                    }
                }
            }
            thread::sleep(CLAIM_MIRROR_TICK);
        }
    });
}

/// How often the mirror looks at the (local, free) Windows claim.
const CLAIM_MIRROR_TICK: Duration = Duration::from_secs(10);
/// How often WSL is consulted when nothing changed on the Windows side. The
/// WSL daemon rotating its own token is the only change this cadence catches.
const CLAIM_MIRROR_STEADY: Duration = Duration::from_secs(60);
/// Ceiling for the failure backoff while WSL cannot answer.
const CLAIM_MIRROR_MAX_BACKOFF: Duration = Duration::from_secs(600);

/// Backoff after `streak` consecutive failed passes: 20s, 40s, 80s ... capped
/// at CLAIM_MIRROR_MAX_BACKOFF.
#[cfg_attr(not(windows), allow(dead_code))]
fn claim_mirror_backoff(streak: u32) -> Duration {
    let factor = 1u32 << streak.clamp(1, 16);
    CLAIM_MIRROR_TICK
        .checked_mul(factor)
        .unwrap_or(CLAIM_MIRROR_MAX_BACKOFF)
        .min(CLAIM_MIRROR_MAX_BACKOFF)
}

/// Decides when the claim mirror may spend a wsl.exe session. Platform-neutral
/// so its policy is unit-tested on every CI host.
#[cfg_attr(not(windows), allow(dead_code))]
struct ClaimMirrorSchedule {
    next_wsl_check: Instant,
    /// Windows claim bytes as of the last successful pass; None until then.
    last_win: Option<Option<Vec<u8>>>,
    fail_streak: u32,
    backoff_until: Option<Instant>,
}

#[cfg_attr(not(windows), allow(dead_code))]
impl ClaimMirrorSchedule {
    fn new(now: Instant) -> Self {
        Self { next_wsl_check: now, last_win: None, fail_streak: 0, backoff_until: None }
    }

    /// Whether this tick may consult WSL. Never inside a failure backoff; else
    /// on the steady cadence, or at once when the Windows claim changed.
    fn due(&self, now: Instant, win: Option<&[u8]>) -> bool {
        if matches!(self.backoff_until, Some(until) if now < until) {
            return false;
        }
        if now >= self.next_wsl_check {
            return true;
        }
        match &self.last_win {
            Some(last) => last.as_deref() != win,
            None => true,
        }
    }

    fn succeeded(&mut self, now: Instant, win: Option<Vec<u8>>) {
        self.fail_streak = 0;
        self.backoff_until = None;
        self.next_wsl_check = now + CLAIM_MIRROR_STEADY;
        self.last_win = Some(win);
    }

    /// Record a failed pass and return the backoff before the next attempt.
    fn failed(&mut self, now: Instant) -> Duration {
        self.fail_streak = self.fail_streak.saturating_add(1);
        let backoff = claim_mirror_backoff(self.fail_streak);
        self.backoff_until = Some(now + backoff);
        self.next_wsl_check = now + backoff;
        backoff
    }

    /// Log the 1st, 2nd, 4th, 8th ... failure of a streak, not every one.
    fn should_log_failure(&self) -> bool {
        self.fail_streak.is_power_of_two()
    }

    fn fail_streak(&self) -> u32 {
        self.fail_streak
    }
}

#[cfg(test)]
mod claim_mirror_schedule_tests {
    use super::*;

    #[test]
    fn first_pass_is_due_immediately() {
        let now = Instant::now();
        assert!(ClaimMirrorSchedule::new(now).due(now, None));
    }

    #[test]
    fn steady_state_consults_wsl_once_per_minute_not_every_tick() {
        let t0 = Instant::now();
        let mut s = ClaimMirrorSchedule::new(t0);
        s.succeeded(t0, Some(b"claim".to_vec()));
        let mut wsl_calls = 0;
        // Ten minutes of 10s ticks with an unchanged Windows claim.
        for tick in 1..=60u64 {
            let now = t0 + CLAIM_MIRROR_TICK * tick as u32;
            if s.due(now, Some(b"claim")) {
                wsl_calls += 1;
                s.succeeded(now, Some(b"claim".to_vec()));
            }
        }
        assert_eq!(wsl_calls, 10, "one WSL check per minute, not one per tick");
    }

    #[test]
    fn windows_side_change_is_mirrored_on_the_next_tick() {
        let t0 = Instant::now();
        let mut s = ClaimMirrorSchedule::new(t0);
        s.succeeded(t0, Some(b"old".to_vec()));
        let next_tick = t0 + CLAIM_MIRROR_TICK;
        assert!(!s.due(next_tick, Some(b"old")));
        assert!(s.due(next_tick, Some(b"new")), "a Windows web-pair must not wait a minute");
        assert!(s.due(next_tick, None), "a removed Windows claim is a change too");
    }

    #[test]
    fn failures_back_off_exponentially_and_block_change_triggers() {
        let t0 = Instant::now();
        let mut s = ClaimMirrorSchedule::new(t0);
        s.succeeded(t0, Some(b"a".to_vec()));
        let mut now = t0 + CLAIM_MIRROR_STEADY;
        let mut backoffs = Vec::new();
        for _ in 0..8 {
            assert!(s.due(now, Some(b"a")));
            let b = s.failed(now);
            // Even a Windows-side change must not retry inside the backoff.
            assert!(!s.due(now + b - Duration::from_secs(1), Some(b"changed")));
            backoffs.push(b.as_secs());
            now += b;
        }
        assert_eq!(backoffs, vec![20, 40, 80, 160, 320, 600, 600, 600]);
        s.succeeded(now, Some(b"a".to_vec()));
        assert_eq!(s.fail_streak(), 0);
        assert!(!s.due(now + CLAIM_MIRROR_TICK, Some(b"a")), "recovery returns to the steady cadence");
    }

    #[test]
    fn failure_logging_is_throttled_to_powers_of_two() {
        let t0 = Instant::now();
        let mut s = ClaimMirrorSchedule::new(t0);
        let logged: Vec<u32> = (1..=20)
            .filter_map(|_| {
                s.failed(t0);
                s.should_log_failure().then(|| s.fail_streak())
            })
            .collect();
        assert_eq!(logged, vec![1, 2, 4, 8, 16]);
    }
}

/// Run wsl.exe with a hard deadline, killing its whole process tree on
/// overrun. `.output()` alone blocks forever when the WSL VM is hung —
/// which is exactly when these probes get called — and with no bound the
/// callers pile fresh wsl.exe children on top of stuck ones (observed
/// 2026-08-12: hundreds of concurrent wsl.exe during a VM death, enough
/// Windows-side memory pressure to worsen the outage it was probing).
#[cfg(windows)]
fn run_wsl_bounded(args: &[&str], timeout: Duration) -> Result<std::process::Output, String> {
    run_wsl_bounded_with_stdin(args, None, timeout)
}

/// run_wsl_bounded, optionally feeding `stdin` to the child. The deadline
/// covers the stdin write too: a hung VM that never reads must not block us.
#[cfg(windows)]
fn run_wsl_bounded_with_stdin(
    args: &[&str],
    stdin: Option<&[u8]>,
    timeout: Duration,
) -> Result<std::process::Output, String> {
    let mut child = Command::new("wsl.exe")
        .args(args)
        .creation_flags(CREATE_NO_WINDOW)
        .stdin(if stdin.is_some() { Stdio::piped() } else { Stdio::null() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("wsl.exe spawn: {e}"))?;
    let pid = child.id();
    let (done_tx, done_rx) = std::sync::mpsc::channel::<()>();
    let watchdog = thread::spawn(move || {
        if done_rx.recv_timeout(timeout).is_err() {
            // taskkill /T takes the whole tree: wsl.exe plus the relay
            // children it parents. A plain child.kill() leaves those behind.
            let _ = Command::new("taskkill.exe")
                .args(["/T", "/F", "/PID", &pid.to_string()])
                .creation_flags(CREATE_NO_WINDOW)
                .output();
            return true;
        }
        false
    });
    if let (Some(bytes), Some(mut pipe)) = (stdin, child.stdin.take()) {
        // A failed write surfaces as the command's own failure below; the
        // pipe drops here either way so the child sees EOF.
        let _ = pipe.write_all(bytes);
    }
    let out = child.wait_with_output();
    let _ = done_tx.send(());
    let timed_out = watchdog.join().unwrap_or(false);
    match out {
        Ok(_) if timed_out => Err(format!(
            "wsl.exe {:?} timed out after {}s; process tree killed",
            args.first().copied().unwrap_or(""),
            timeout.as_secs()
        )),
        Ok(o) => Ok(o),
        Err(e) => Err(format!("wsl.exe wait: {e}")),
    }
}

/// Shared governor for WSL liveness probes: an IP cache with TTL, plus a
/// failure circuit with exponential backoff. One probe in flight at a time
/// (callers serialize on the mutex and then read the fresh cache), and
/// while the circuit is open every caller fails fast instead of spawning
/// its own wsl.exe. Success closes the circuit immediately.
#[cfg(windows)]
struct WslProbeState {
    ip: Option<String>,
    ip_at: Option<Instant>,
    fail_streak: u32,
    next_attempt_at: Option<Instant>,
}

#[cfg(windows)]
static WSL_PROBE: Mutex<WslProbeState> = Mutex::new(WslProbeState {
    ip: None,
    ip_at: None,
    fail_streak: 0,
    next_attempt_at: None,
});

#[cfg(windows)]
const WSL_IP_TTL: Duration = Duration::from_secs(15);

#[cfg(windows)]
fn wsl_probe_failed(st: &mut WslProbeState) {
    st.fail_streak = st.fail_streak.saturating_add(1);
    // 2s, 4s, 8s ... capped at 60s between attempts.
    let backoff = Duration::from_secs((2u64 << st.fail_streak.min(5).saturating_sub(1)).min(60));
    st.next_attempt_at = Some(Instant::now() + backoff);
}

/// Query `wsl.exe -e hostname -I` for the WSL2 VM's IPv4 address —
/// single-flighted, TTL-cached, and circuit-broken (see WslProbeState).
#[cfg(windows)]
fn resolve_wsl_ip() -> Result<String, String> {
    let mut st = WSL_PROBE.lock().unwrap();
    let now = Instant::now();
    if let (Some(ip), Some(at)) = (st.ip.clone(), st.ip_at) {
        if now.duration_since(at) < WSL_IP_TTL {
            return Ok(ip);
        }
    }
    if let Some(next) = st.next_attempt_at {
        if now < next {
            return Err(format!(
                "wsl probe circuit open ({} consecutive failures; next attempt in {}s)",
                st.fail_streak,
                next.duration_since(now).as_secs().max(1)
            ));
        }
    }
    // Holding the lock through the bounded probe IS the single-flight:
    // concurrent callers block briefly, then hit the fresh cache or the
    // just-opened circuit instead of spawning their own wsl.exe.
    match run_wsl_probe_bounded(&["-e", "bash", "-c", "hostname -I"], Duration::from_secs(5)) {
        Ok(out) if out.status.success() => {
            let stdout = String::from_utf8_lossy(&out.stdout);
            match pick_wsl_ipv4(&stdout) {
                Some(ip) => {
                    st.ip = Some(ip.clone());
                    st.ip_at = Some(Instant::now());
                    st.fail_streak = 0;
                    st.next_attempt_at = None;
                    Ok(ip)
                }
                None => {
                    wsl_probe_failed(&mut st);
                    Err(format!("unexpected hostname output: {stdout:?}"))
                }
            }
        }
        Ok(out) => {
            wsl_probe_failed(&mut st);
            Err(format!(
                "wsl.exe exit {:?}: {}",
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
        Err(e) => {
            wsl_probe_failed(&mut st);
            Err(e)
        }
    }
}

/// Pick the WSL VM's IPv4 from `hostname -I` output. The output lists EVERY
/// interface address, and the first token is not always eth0: Tailscale inside
/// WSL puts its CGNAT address (100.64.0.0/10) first, and IPv6 tokens can
/// appear too. Picking a CGNAT/unreachable address makes the daemon probes
/// hang and burns the 45s post-spawn reachability wait (observed in the field:
/// candidates ["100.64.12.34:8787", "127.0.0.1:8787"]). Prefer the first
/// plain IPv4 that isn't CGNAT/loopback/link-local; fall back to the first
/// IPv4 at all (the 127.0.0.1 fallback candidate still exists upstream).
#[cfg_attr(not(windows), allow(dead_code))]
fn pick_wsl_ipv4(hostname_output: &str) -> Option<String> {
    let mut first_v4: Option<&str> = None;
    for tok in hostname_output.split_whitespace() {
        let octets: Vec<u8> = match tok.split('.').map(|p| p.parse::<u8>()).collect() {
            Ok(o) => o,
            Err(_) => continue, // IPv6 / junk token — skip, don't abort
        };
        if octets.len() != 4 {
            continue;
        }
        if first_v4.is_none() {
            first_v4 = Some(tok);
        }
        let cgnat = octets[0] == 100 && (64..=127).contains(&octets[1]);
        let loopback = octets[0] == 127;
        let link_local = octets[0] == 169 && octets[1] == 254;
        if !cgnat && !loopback && !link_local {
            return Some(tok.to_string());
        }
    }
    first_v4.map(str::to_string)
}

/// Plain TCP byte-proxy: accepts on `listen_addr`, dials `forward_addr`
/// for each connection, runs bidirectional `io::copy`. Protocol-agnostic.
/// Returns a handle whose Drop stops accepting new connections; existing
/// forwarded streams drain briefly after either side closes.
#[cfg(windows)]
fn start_tcp_proxy(
    listen_addr: &str,
    forward_addr: String,
    vm_route: Option<String>,
) -> io::Result<ProxyHandle> {
    let listener = TcpListener::bind(listen_addr)?;
    listener.set_nonblocking(true)?;
    let shutdown = Arc::new(AtomicBool::new(false));
    let shutdown_t = shutdown.clone();

    // The upstream address lives behind a shared cell so a connection
    // that finds it dead — WSL2 reassigns the VM IP on every restart /
    // sleep-resume — can re-resolve and overwrite it. Subsequent
    // connections then dial the new IP directly instead of flooding the
    // log with "target machine actively refused it" forever against a
    // stale address (the recurring daemon-unreachable churn).
    let target = Arc::new(Mutex::new(forward_addr));

    // A proxy that had to settle for loopback moves new connections back to
    // the VM IP once it answers. The token is read once here (cached), so the
    // re-probe is one plain HTTP request per interval and never a wsl.exe.
    if let Some(vm_route) = vm_route {
        let target = target.clone();
        let shutdown = shutdown.clone();
        let token = read_wsl_token().ok();
        thread::spawn(move || {
            let mut last_check = Instant::now();
            while !shutdown.load(Ordering::SeqCst) {
                thread::sleep(Duration::from_millis(500));
                if last_check.elapsed() < WSL_VM_ROUTE_REHOME_EVERY {
                    continue;
                }
                last_check = Instant::now();
                let current = target.lock().unwrap().clone();
                let Some(vm) = wsl_rehome_target(&current, Some(&vm_route)) else {
                    continue;
                };
                if require_backend_with_token(vm, BACKEND_ID_WSL, token.as_deref()).is_ok() {
                    *target.lock().unwrap() = vm.to_string();
                    launch_log(&format!(
                        "WSL proxy re-homed {current} → {vm}: new connections use the VM IP again"
                    ));
                }
            }
        });
    }

    thread::spawn(move || {
        while !shutdown_t.load(Ordering::SeqCst) {
            match listener.accept() {
                Ok((client, _)) => {
                    // CRITICAL on Windows: accepted streams inherit the
                    // listener's nonblocking flag. io::copy doesn't handle
                    // WouldBlock and returns an error partway through large
                    // responses — manifests as ERR_CONTENT_LENGTH_MISMATCH
                    // in the browser. Force back to blocking.
                    if let Err(e) = client.set_nonblocking(false) {
                        launch_log(&format!("proxy set_nonblocking(false) failed: {e}"));
                        continue;
                    }
                    let target = target.clone();
                    thread::spawn(move || {
                        if let Err(e) = forward_conn(client, &target) {
                            launch_log(&format!("proxy conn error: {e}"));
                        }
                    });
                }
                Err(ref e) if e.kind() == io::ErrorKind::WouldBlock => {
                    // Poll cadence. 50ms is cheap — this thread is
                    // usually idle between connections.
                    thread::sleep(Duration::from_millis(50));
                }
                Err(e) => {
                    launch_log(&format!("proxy accept error: {e}"));
                    break;
                }
            }
        }
        launch_log("proxy accept loop stopped");
    });

    Ok(ProxyHandle { shutdown })
}

/// Dial `addr` with a bounded connect timeout. Our WSL upstreams are
/// always numeric `host:port`, so they parse to a SocketAddr and we can
/// cap the connect — a stale-but-routable IP otherwise blocks ~21s on
/// Windows before failing.
#[cfg(windows)]
fn dial_upstream(addr: &str, timeout: Duration) -> io::Result<TcpStream> {
    match addr.parse::<SocketAddr>() {
        Ok(sa) => TcpStream::connect_timeout(&sa, timeout),
        Err(_) => TcpStream::connect(addr),
    }
}

/// WSL daemon address candidates, freshest first: the current WSL VM IP
/// (NAT mode) then Windows loopback, IPv4 (mirrored mode) and IPv6 (NAT-mode
/// localhost forwarding); see WSL_LOOPBACK_CANDIDATES.
#[cfg(windows)]
fn wsl_upstream_candidates() -> Vec<String> {
    let mut v = Vec::new();
    if let Ok(ip) = resolve_wsl_ip() {
        v.push(format!("{ip}:8787"));
    }
    for addr in WSL_LOOPBACK_CANDIDATES {
        v.push(addr.into());
    }
    v.dedup();
    v
}

/// Connect to the WSL daemon for one proxied client, healing a stale
/// upstream IP transparently. Happy path: dial the cached address and
/// return. On failure (connection refused / timeout — the classic
/// symptom of WSL having restarted with a new VM IP), re-resolve the
/// WSL IP, verify the candidate actually answers as the WSL backend (so
/// we never latch onto a Windows-native daemon squatting :8787), and on
/// success overwrite the shared target so later connections skip the
/// re-resolve and go straight to the working address.
#[cfg(windows)]
fn connect_upstream(target: &Arc<Mutex<String>>) -> io::Result<TcpStream> {
    // This address was identity-checked when the proxy was established (or
    // last healed). Connecting to it does not start WSL or grow its disk.
    // Keep existing sessions reachable even when new starts are disallowed.
    let current = target.lock().unwrap().clone();
    let (stream, address) = connect_existing_wsl_upstream(
        &current,
        |addr| dial_upstream(addr, Duration::from_secs(3)),
        || {
            launch_log(&format!("upstream {current} unreachable; re-resolving WSL IP"));
            wsl_upstream_candidates()
        },
        |candidate| {
            let token = read_wsl_token().ok();
            require_backend_with_token(candidate, BACKEND_ID_WSL, token.as_deref()).is_ok()
        },
    )?;
    if address != current {
        launch_log(&format!("upstream re-resolved {current} → {address}"));
        *target.lock().unwrap() = address;
    }
    Ok(stream)
}

// The fast path must not depend on disk admission or launching wsl.exe. Only
// failed connections invoke discovery; every replacement address is verified.
#[cfg_attr(not(windows), allow(dead_code))]
fn connect_existing_wsl_upstream<T>(
    current: &str,
    dial: impl Fn(&str) -> io::Result<T>,
    discover: impl FnOnce() -> Vec<String>,
    verify: impl Fn(&str) -> bool,
) -> io::Result<(T, String)> {
    let error = match dial(current) {
        Ok(stream) => return Ok((stream, current.to_string())),
        Err(error) => error,
    };
    for candidate in discover() {
        if candidate == current || !verify(&candidate) {
            continue;
        }
        if let Ok(stream) = dial(&candidate) {
            return Ok((stream, candidate));
        }
    }
    Err(error)
}

#[cfg(windows)]
/// Page served from Windows when the WSL daemon can't be reached. It polls
/// the proxy and reloads itself the instant WSL answers again, so the outage
/// ends without the user touching anything.
#[cfg(windows)]
const BACKEND_UNAVAILABLE_HTML: &str = r#"<!doctype html><meta charset="utf-8">
<title>soluterminal — reconnecting</title>
<style>body{font:14px system-ui,-apple-system,sans-serif;background:#0b0f14;
color:#e6edf3;display:flex;align-items:center;justify-content:center;
height:100vh;margin:0}div{text-align:center;max-width:34em;padding:0 24px}
h1{font-size:16px;font-weight:600;margin:0 0 8px}p{color:#8b949e;margin:0 0 6px}
button{font:inherit;margin-top:16px;padding:6px 14px;border-radius:6px;
border:1px solid #30363d;background:#161b22;color:#e6edf3;cursor:pointer}</style>
<div><h1>Reconnecting to the WSL backend</h1>
<p>WSL stopped responding. Recovery is running automatically.</p>
<p>This page returns to your session as soon as it's back.</p>
<button onclick="location.reload()">Retry now</button></div>
<script>setInterval(function(){fetch('/api/health',{cache:'no-store'})
.then(function(r){if(r.ok)location.reload()}).catch(function(){})},2000)</script>"#;

#[cfg(windows)]
const BACKEND_HOST_DISK_HTML: &str = r#"<!doctype html><meta charset="utf-8">
<title>soluterminal — Windows drive nearly full</title>
<style>body{font:14px system-ui,-apple-system,sans-serif;background:#0b0f14;color:#e6edf3;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}div{text-align:center;max-width:38em;padding:0 24px}h1{font-size:16px}p{color:#8b949e}</style>
<div><h1>Windows drive C: is nearly full</h1><p>WSL will not be restarted until at least {{FLOOR_GB}} GB is free (currently {{FREE_GB}} GB).</p><p>Free space or run scripts\wsl-vhdx-compact.ps1; see docs/RUNBOOK-wsl-host-disk.md.</p></div>
<script>setInterval(function(){fetch('/api/health',{cache:'no-store'}).then(function(r){if(r.ok)location.reload()}).catch(function(){})},2000)</script>"#;

/// Answer a client from Windows when the WSL upstream is down, instead of
/// dropping the socket.
///
/// Dropping it produced the worst user-visible failure we've seen: the page
/// died with "127.0.0.1 unable to connect", and the browser's own retry then
/// tripped the daemon's origin gate and left a bare "cross-origin request
/// rejected" string on screen — escapable only by quitting the whole app. A
/// real 503 that re-checks on a timer means the same outage clears itself the
/// moment WSL recovers.
#[cfg(windows)]
fn serve_backend_unavailable(mut client: TcpStream) {
    // Best-effort peek at the request head so we answer in the right shape.
    // A slow/absent request body must never hold this thread open.
    let _ = client.set_read_timeout(Some(Duration::from_millis(250)));
    let mut head = [0u8; 4096];
    let n = client.read(&mut head).unwrap_or(0);
    let req = String::from_utf8_lossy(&head[..n]).to_ascii_lowercase();
    let first_line = req.lines().next().unwrap_or("").to_string();

    // WebSocket upgrades and API calls want a status code, not markup — the
    // web UI already renders its own reconnecting state for those.
    let wants_html = !req.contains("upgrade: websocket")
        && !first_line.contains(" /api/")
        && req.contains("text/html");

    let blocked = host_disk_gate();
    let host_body = match blocked {
        HostDiskVerdict::Block { free, floor, .. } => Some(
            BACKEND_HOST_DISK_HTML
                .replace("{{FREE_GB}}", &format!("{:.1}", free as f64 / GIB as f64))
                .replace("{{FLOOR_GB}}", &format!("{:.1}", floor as f64 / GIB as f64)),
        ),
        _ => None,
    };
    let (ctype, body) = if wants_html {
        (
            "text/html; charset=utf-8",
            host_body.as_deref().unwrap_or(BACKEND_UNAVAILABLE_HTML),
        )
    } else {
        ("application/json", r#"{"error":"wsl backend unreachable"}"#)
    };
    let resp = format!(
        "HTTP/1.1 503 Service Unavailable\r\n\
         Content-Type: {ctype}\r\n\
         Content-Length: {}\r\n\
         Cache-Control: no-store\r\n\
         Retry-After: 2\r\n\
         Connection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = client.write_all(resp.as_bytes());
    let _ = client.flush();
    let _ = client.shutdown(std::net::Shutdown::Both);
}

#[cfg(windows)]
fn forward_conn(client: TcpStream, target: &Arc<Mutex<String>>) -> io::Result<()> {
    let server = match connect_upstream(target) {
        Ok(s) => s,
        Err(_) => {
            // connect_upstream already logged the unreachable address and its
            // re-resolve attempt; don't log the same outage twice per request
            // (this path fires once per asset on a page load).
            serve_backend_unavailable(client);
            return Ok(());
        }
    };
    let _ = client.set_nodelay(true);
    let _ = server.set_nodelay(true);

    relay_tcp(client, server, PROXY_DRAIN_GRACE)
}

// Allow a half-closed HTTP request to receive its response, but do not keep
// an upstream socket and two threads forever if the peer stays quiet after
// the other end has gone away. WebSocket/SSE sessions remain open while both
// copy directions are active.
#[cfg(windows)]
const PROXY_DRAIN_GRACE: Duration = Duration::from_secs(30);

// On Windows, shutting down one TcpStream clone does not reliably wake a
// blocking read on another clone. Poll I/O so cancellation can release both
// relay threads after the drain grace without closing an idle live stream.
const PROXY_IO_POLL: Duration = Duration::from_millis(250);

fn relay_copy(
    reader: &mut TcpStream,
    writer: &mut TcpStream,
    cancelled: &AtomicBool,
) -> io::Result<()> {
    let mut buffer = [0u8; 16 * 1024];
    loop {
        if cancelled.load(Ordering::SeqCst) {
            return Ok(());
        }
        let len = match reader.read(&mut buffer) {
            Ok(0) => return Ok(()),
            Ok(len) => len,
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::Interrupted | io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut
                ) =>
            {
                continue;
            }
            Err(e) => return Err(e),
        };
        let mut written = 0;
        while written < len {
            if cancelled.load(Ordering::SeqCst) {
                return Ok(());
            }
            match writer.write(&buffer[written..len]) {
                Ok(0) => return Err(io::ErrorKind::WriteZero.into()),
                Ok(n) => written += n,
                Err(e)
                    if matches!(
                        e.kind(),
                        io::ErrorKind::Interrupted
                            | io::ErrorKind::WouldBlock
                            | io::ErrorKind::TimedOut
                    ) => {}
                Err(e) => return Err(e),
            }
        }
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
fn relay_tcp(client: TcpStream, server: TcpStream, drain_grace: Duration) -> io::Result<()> {
    client.set_read_timeout(Some(PROXY_IO_POLL))?;
    client.set_write_timeout(Some(PROXY_IO_POLL))?;
    server.set_read_timeout(Some(PROXY_IO_POLL))?;
    server.set_write_timeout(Some(PROXY_IO_POLL))?;
    let client_control = client.try_clone()?;
    let server_control = server.try_clone()?;

    let client_read = client.try_clone()?;
    let server_write = server.try_clone()?;
    let client_write = client;
    let server_read = server;

    // One thread per direction. Half-shutdown preserves a final response
    // after a request EOF; the owner forces both sockets closed if the
    // reverse copy cannot finish within the drain grace.
    let (done_tx, done_rx) = std::sync::mpsc::channel();
    let cancelled = Arc::new(AtomicBool::new(false));
    let up_done = done_tx.clone();
    let up_cancelled = cancelled.clone();
    let t_up = thread::spawn(move || {
        let mut r = client_read;
        let mut w = server_write;
        let _ = relay_copy(&mut r, &mut w, &up_cancelled);
        let _ = w.shutdown(std::net::Shutdown::Write);
        let _ = up_done.send(());
    });
    let down_cancelled = cancelled.clone();
    let t_dn = thread::spawn(move || {
        let mut r = server_read;
        let mut w = client_write;
        let _ = relay_copy(&mut r, &mut w, &down_cancelled);
        let _ = w.shutdown(std::net::Shutdown::Write);
        let _ = done_tx.send(());
    });
    let _ = done_rx.recv();
    if done_rx.recv_timeout(drain_grace).is_err() {
        cancelled.store(true, Ordering::SeqCst);
        let _ = client_control.shutdown(std::net::Shutdown::Both);
        let _ = server_control.shutdown(std::net::Shutdown::Both);
    }
    let _ = t_up.join();
    let _ = t_dn.join();
    Ok(())
}

#[cfg(test)]
mod proxy_relay_tests {
    use super::*;
    use std::net::{Shutdown, TcpListener};
    use std::sync::mpsc;

    fn socket_pair() -> (TcpStream, TcpStream) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (relay, _) = listener.accept().unwrap();
        (peer, relay)
    }

    #[test]
    fn disconnected_client_does_not_leave_a_quiet_upstream_open() {
        let (client, relay_client) = socket_pair();
        let (mut upstream, relay_server) = socket_pair();
        let (done_tx, done_rx) = mpsc::channel();
        thread::spawn(move || {
            done_tx
                .send(relay_tcp(
                    relay_client,
                    relay_server,
                    Duration::from_millis(100),
                ))
                .unwrap();
        });

        drop(client);
        assert!(done_rx.recv_timeout(Duration::from_secs(2)).unwrap().is_ok());
        upstream.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
        assert_eq!(upstream.read(&mut [0u8; 1]).unwrap(), 0);
    }

    #[test]
    fn half_closed_request_can_receive_its_final_response() {
        let (mut client, relay_client) = socket_pair();
        let (mut upstream, relay_server) = socket_pair();
        let (done_tx, done_rx) = mpsc::channel();
        thread::spawn(move || {
            done_tx
                .send(relay_tcp(
                    relay_client,
                    relay_server,
                    Duration::from_secs(1),
                ))
                .unwrap();
        });

        client.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        upstream.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        client.write_all(b"ping").unwrap();
        client.shutdown(Shutdown::Write).unwrap();
        let mut request = [0u8; 4];
        upstream.read_exact(&mut request).unwrap();
        assert_eq!(&request, b"ping");
        upstream.write_all(b"pong").unwrap();
        upstream.shutdown(Shutdown::Write).unwrap();
        let mut response = [0u8; 4];
        client.read_exact(&mut response).unwrap();
        assert_eq!(&response, b"pong");
        assert!(done_rx.recv_timeout(Duration::from_secs(2)).unwrap().is_ok());
    }

    #[test]
    fn idle_live_connection_is_not_closed_by_drain_grace() {
        let (mut client, relay_client) = socket_pair();
        let (mut upstream, relay_server) = socket_pair();
        let (done_tx, done_rx) = mpsc::channel();
        thread::spawn(move || {
            done_tx
                .send(relay_tcp(
                    relay_client,
                    relay_server,
                    Duration::from_millis(100),
                ))
                .unwrap();
        });

        assert!(matches!(
            done_rx.recv_timeout(Duration::from_millis(650)),
            Err(mpsc::RecvTimeoutError::Timeout)
        ));
        client.write_all(b"live").unwrap();
        upstream.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
        let mut message = [0u8; 4];
        upstream.read_exact(&mut message).unwrap();
        assert_eq!(&message, b"live");
        drop(client);
        drop(upstream);
        assert!(done_rx.recv_timeout(Duration::from_secs(2)).unwrap().is_ok());
    }
}

#[cfg(windows)]
pub struct ProxyHandle {
    shutdown: Arc<AtomicBool>,
}

#[cfg(windows)]
impl Drop for ProxyHandle {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::SeqCst);
        // The accept loop wakes at the next poll tick (≤50ms). The
        // listener drops when this function returns; any spawned
        // per-connection forwarder threads finish on their own once
        // their peer closes.
    }
}

/// Append a line to %LOCALAPPDATA%\soluterminal\launch.log so we can
/// see what happened during startup when the Tauri release build
/// suppresses stdout/stderr on Windows. Best-effort — silently no-ops
/// if we can't write.
#[cfg(windows)]
pub(crate) fn launch_log(line: &str) {
    use std::fs::OpenOptions;
    use std::io::Write;
    // Serialize writers: concurrent proxy threads logging at once used to
    // interleave bytes mid-line, garbling the log exactly when it mattered
    // (many threads log during a WSL outage).
    static LOG_LOCK: Mutex<()> = Mutex::new(());
    let _guard = LOG_LOCK.lock().unwrap();
    let Some(mut dir) = std::env::var_os("LOCALAPPDATA").map(PathBuf::from) else {
        return;
    };
    dir.push("soluterminal");
    if std::fs::create_dir_all(&dir).is_err() {
        return;
    }
    dir.push("launch.log");
    let Ok(mut f) = OpenOptions::new().create(true).append(true).open(&dir) else {
        return;
    };
    let ts = chrono_like_now();
    let _ = writeln!(f, "[{ts}] {line}");
}

fn chrono_like_now() -> String {
    // Minimal ISO-ish timestamp without pulling chrono into the tree.
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    format!("{secs}")
}

#[cfg_attr(not(windows), allow(dead_code))]
fn iso_now() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let days = (secs / 86400) as i64;
    let z = days + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let mut year = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = mp + if mp < 10 { 3 } else { -9 };
    year += if month <= 2 { 1 } else { 0 };
    let sod = secs % 86400;
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        sod / 3600,
        (sod % 3600) / 60,
        sod % 60
    )
}

#[cfg(test)]
mod host_disk_tests {
    use super::*;

    #[test]
    fn low_disk_allows_discovery_only_in_an_already_running_default_distro() {
        let blocked = HostDiskVerdict::Block {
            free: GIB,
            total: None,
            floor: 60 * GIB,
        };
        assert!(allow_existing_wsl_probe(&blocked, || Ok(true)).is_ok());
        assert!(allow_existing_wsl_probe(&blocked, || Ok(false)).is_err());
        assert!(allow_existing_wsl_probe(&blocked, || Err("timed out".into())).is_err());
        assert!(!recovery_should_spawn_wsl(&blocked));
        assert!(
            allow_existing_wsl_probe(&HostDiskVerdict::Ok, || panic!("unnecessary listing"))
                .is_ok()
        );
    }

    #[test]
    fn running_listing_checks_the_default_without_localized_state_names() {
        let verbose = "  NAME             STATE           VERSION\r\n* Ubuntu Dev       Running         2\r\n  Debian           Stopped         2\r\n";
        assert!(default_wsl_is_running(verbose, "Ubuntu Dev\r\n"));
        assert!(!default_wsl_is_running(verbose, "Debian\r\n"));
        assert!(!default_wsl_is_running(verbose, "Ubuntu\r\n"));
        assert!(!default_wsl_is_running(verbose, ""));
        assert!(!default_wsl_is_running("WSL unavailable", "Ubuntu Dev"));
        assert!(default_wsl_is_running(
            &verbose.replace("Running", "En cours"),
            "Ubuntu Dev"
        ));
        let encoded: Vec<u8> = verbose.encode_utf16().flat_map(u16::to_le_bytes).collect();
        assert_eq!(decode_wsl_listing(&encoded), verbose);
        assert_eq!(decode_wsl_listing(verbose.as_bytes()), verbose);
    }

    #[test]
    fn existing_proxy_connection_survives_unavailable_discovery() {
        let (stream, address) = connect_existing_wsl_upstream(
            "172.22.1.2:8787",
            |_| Ok(42),
            || panic!("a healthy connection must not invoke disk-gated WSL discovery"),
            |_| panic!("the established address was already verified"),
        )
        .unwrap();
        assert_eq!(stream, 42);
        assert_eq!(address, "172.22.1.2:8787");
    }

    #[test]
    fn dead_proxy_rejects_wrong_backend_and_accepts_verified_changed_ip() {
        let result = connect_existing_wsl_upstream(
            "old",
            |addr| match addr {
                "old" => Err(io::Error::new(io::ErrorKind::ConnectionRefused, "offline")),
                "windows" => panic!("must not forward to the Windows backend"),
                _ => Ok(42),
            },
            || vec!["old".into(), "windows".into(), "new-wsl".into()],
            |addr| addr == "new-wsl",
        )
        .unwrap();
        assert_eq!(result, (42, "new-wsl".into()));
        let unavailable = connect_existing_wsl_upstream::<()>(
            "old",
            |_| Err(io::Error::new(io::ErrorKind::ConnectionRefused, "offline")),
            Vec::new,
            |_| false,
        );
        assert_eq!(
            unavailable.unwrap_err().kind(),
            io::ErrorKind::ConnectionRefused
        );
    }

    #[test]
    fn verdict_boundaries_use_larger_absolute_or_percentage_floor() {
        let p = HostDiskPolicy {
            min_free_bytes: 16 * GIB,
            min_free_pct: 3,
        };
        assert!(matches!(
            host_disk_verdict(40 * GIB, Some(2_050 * GIB), &p),
            HostDiskVerdict::Block { .. }
        ));
        let floor = 60 * GIB;
        assert_eq!(
            host_disk_verdict(floor, Some(2_000 * GIB), &p),
            HostDiskVerdict::Warn {
                free: floor,
                total: Some(2_000 * GIB),
                floor
            }
        );
        assert_eq!(
            host_disk_verdict(2 * floor, Some(2_000 * GIB), &p),
            HostDiskVerdict::Ok
        );
        assert!(
            matches!(host_disk_verdict(15*GIB, None, &p), HostDiskVerdict::Block { floor, .. } if floor == 16*GIB)
        );
    }

    #[test]
    fn policy_precedence_and_invalid_values_fall_through() {
        let json = r#"{"minFreeMB":32768,"minFreePct":7}"#;
        let p = host_disk_policy_from_sources(Some("65536"), Some("9"), Some(json));
        assert_eq!(
            p,
            HostDiskPolicy {
                min_free_bytes: 65536 * 1024 * 1024,
                min_free_pct: 9
            }
        );
        let p = host_disk_policy_from_sources(Some("tow"), Some("nope"), Some(json));
        assert_eq!(
            p,
            HostDiskPolicy {
                min_free_bytes: 32768 * 1024 * 1024,
                min_free_pct: 7
            }
        );
        assert_eq!(
            host_disk_policy_from_sources(Some("tow"), None, None),
            HostDiskPolicy::default()
        );
        assert_eq!(
            host_disk_policy_from_sources(None, Some("99"), None).min_free_pct,
            50
        );
    }

    #[test]
    fn policy_file_zeroes_cannot_disable_the_builtin_preflight() {
        let json = r#"{"minFreeMB":0,"minFreePct":0}"#;
        assert_eq!(
            host_disk_policy_from_sources(None, None, Some(json)),
            HostDiskPolicy::default()
        );
        assert!(matches!(
            host_disk_verdict_from_sources(8 * GIB, None, None, None, Some(json)),
            HostDiskVerdict::Block { floor, .. } if floor == 16 * GIB
        ));
        assert!(matches!(
            host_disk_verdict_from_sources(
                8 * GIB,
                Some(2_050 * GIB),
                None,
                None,
                Some(json),
            ),
            HostDiskVerdict::Block { floor, .. }
                if floor == 2_050 * GIB * 3 / 100 && floor >= 16 * GIB
        ));
    }

    #[test]
    fn launcher_environment_zero_remains_an_explicit_override() {
        assert_eq!(
            host_disk_policy_from_sources(
                Some("2048"),
                Some("0"),
                Some(r#"{"minFreeMB":0,"minFreePct":0}"#),
            ),
            HostDiskPolicy {
                min_free_bytes: 2 * GIB,
                min_free_pct: 0,
            }
        );
    }

    #[test]
    fn policy_file_zero_falls_back_per_component() {
        assert_eq!(
            host_disk_policy_from_sources(None, None, Some(r#"{"minFreeMB":0,"minFreePct":7}"#)),
            HostDiskPolicy {
                min_free_bytes: 16 * GIB,
                min_free_pct: 7,
            }
        );
        assert_eq!(
            host_disk_policy_from_sources(
                None,
                None,
                Some(r#"{"minFreeMB":32768,"minFreePct":0}"#),
            ),
            HostDiskPolicy {
                min_free_bytes: 32 * GIB,
                min_free_pct: 3,
            }
        );
    }

    #[test]
    fn launcher_runtime_sources_drive_the_preflight_verdict() {
        let blocked = host_disk_verdict_from_sources(40 * GIB, Some(2_050 * GIB), None, None, None);
        assert!(matches!(blocked, HostDiskVerdict::Block { .. }));
        let allowed = host_disk_verdict_from_sources(
            40 * GIB,
            Some(2_050 * GIB),
            Some("0"),
            Some("0"),
            Some(r#"{"minFreeMB":32768,"minFreePct":7}"#),
        );
        assert_eq!(allowed, HostDiskVerdict::Ok);
    }

    #[test]
    fn block_message_and_recovery_gate_are_actionable() {
        let v = HostDiskVerdict::Block {
            free: 4 * GIB,
            total: Some(100 * GIB),
            floor: 16 * GIB,
        };
        let msg = host_disk_block_message(&v);
        assert!(msg.starts_with("host disk critically low"));
        assert!(
            msg.contains("4.0 GB")
                && msg.contains("16.0 GB")
                && msg.contains("RUNBOOK-wsl-host-disk.md")
        );
        assert!(!recovery_should_spawn_wsl(&v));
        assert!(recovery_should_spawn_wsl(&HostDiskVerdict::Ok));
        assert!(recovery_should_spawn_wsl(&HostDiskVerdict::Warn {
            free: 20 * GIB,
            total: None,
            floor: 16 * GIB
        }));
        let warning = host_disk_block_message(&HostDiskVerdict::Warn {
            free: 100 * GIB,
            total: Some(2_050 * GIB),
            floor: 61 * GIB,
        });
        assert!(warning.starts_with("host disk low"), "{warning}");
        assert!(
            !warning.contains("critically low") && !warning.contains("cannot run safely"),
            "{warning}"
        );
    }

    #[test]
    fn bounded_jsonl_keeps_two_generations() {
        let dir = std::env::temp_dir().join(format!(
            "solu-host-disk-{}-{}",
            std::process::id(),
            chrono_like_now()
        ));
        let path = dir.join("host-disk.jsonl");
        append_bounded_jsonl(&path, "123456789", 10).unwrap();
        append_bounded_jsonl(&path, "abcdefghij", 10).unwrap();
        append_bounded_jsonl(&path, "ABCDEFGHIJ", 10).unwrap();
        assert!(path.exists());
        assert!(PathBuf::from(format!("{}.1", path.display())).exists());
        assert!(PathBuf::from(format!("{}.2", path.display())).exists());
        std::fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn iso_timestamp_is_json_time_compatible() {
        assert!(iso_now().ends_with('Z') && iso_now().contains('T'));
    }
}

/// Translate a Windows path like `C:\Users\benja\file` to `/mnt/c/Users/benja/file`.
/// Strips the `\\?\` extended-length prefix that `canonicalize` / some Tauri
/// path APIs produce on Windows. Returns None for UNC `\\server\share` paths.
#[cfg(windows)]
fn windows_path_to_wsl(p: &Path) -> Option<String> {
    let s = p.to_str()?;
    // Drop `\\?\` extended-length prefix if present. UNC form `\\?\UNC\...`
    // we deliberately don't translate — let the caller see None.
    let s = s.strip_prefix(r"\\?\").unwrap_or(s);
    if s.starts_with(r"UNC\") {
        return None;
    }
    let bytes = s.as_bytes();
    if bytes.len() < 3 || bytes[1] != b':' || (bytes[2] != b'\\' && bytes[2] != b'/') {
        return None;
    }
    let drive = (bytes[0] as char).to_ascii_lowercase();
    let rest = &s[2..].replace('\\', "/");
    Some(format!("/mnt/{drive}{rest}"))
}

/// The generated WSL launcher is the only witness to a daemon death on the
/// Windows/WSL backend, and it spent a long time destroying its own evidence:
/// a truncating `exec >` at script start meant each supervisor session wiped
/// the previous one's log. These tests hold the template to the fix, and hold
/// the update-handoff plumbing next to it byte-still.
#[cfg(test)]
mod wsl_start_script_tests {
    use super::{stale_wsl_daemon_reason, wsl_start_script_body};

    fn body() -> String {
        wsl_start_script_body("/mnt/c/a/solud-linux-amd64.gz", "/mnt/c/b/solud-linux-arm64.gz")
    }

    #[test]
    fn health_accepts_older_wsl_daemon_when_bundle_version_is_held() {
        let required = env!("SOLU_SHELL_VERSION");
        let health = serde_json::json!({
            "version": "0.0.1",
            "updateHoldVersion": required,
        });
        assert_eq!(stale_wsl_daemon_reason(&health), None);

        let unheld = serde_json::json!({ "version": "0.0.1" });
        assert!(stale_wsl_daemon_reason(&unheld).is_some());
    }

    /// The regression itself. `exec > "` truncates; `exec >> "` appends. The
    /// two differ by one character, which is exactly how this got shipped.
    #[test]
    fn never_truncates_the_daemon_log() {
        let b = body();
        assert!(
            !b.contains("exec > \""),
            "start-wsl.sh still opens the log with a TRUNCATING redirect"
        );
        assert!(
            b.contains("exec >> \"$DAEMON_DIR/daemon.log\" 2>&1"),
            "start-wsl.sh must append to daemon.log"
        );
    }

    /// Rotation must be defined once and actually called: unconditionally
    /// before the initial redirect, size-gated when relaunching a crashed
    /// daemon.
    #[test]
    fn rotates_three_generations_at_start_and_on_relaunch() {
        let b = body();
        assert_eq!(
            b.matches("rotate_daemon_log() {\n").count(),
            1,
            "rotate_daemon_log should be defined exactly once"
        );
        let calls = b
            .lines()
            .filter(|l| l.trim() == "rotate_daemon_log")
            .count();
        assert_eq!(
            calls, 1,
            "rotate_daemon_log is called unconditionally exactly once, at session start; \
             the relaunch path goes through rotate_daemon_log_if_big"
        );
        let gated = b
            .lines()
            .filter(|l| l.trim() == "rotate_daemon_log_if_big")
            .count();
        assert_eq!(
            gated, 1,
            "the crash-relaunch path must rotate through the size gate exactly once"
        );
        assert!(
            b.contains("mv -f \"$DAEMON_DIR/daemon.log.1\" \"$DAEMON_DIR/daemon.log.2\""),
            "missing daemon.log.1 -> daemon.log.2 generation shift"
        );
        assert!(
            b.contains("mv -f \"$DAEMON_DIR/daemon.log\" \"$DAEMON_DIR/daemon.log.1\""),
            "missing daemon.log -> daemon.log.1 generation shift"
        );
        assert!(
            b.contains("rm -f \"$DAEMON_DIR/daemon.log.2\""),
            "generations must be bounded: the oldest has to be dropped"
        );
        // The relaunch must re-point stdout at the fresh file, or the
        // supervisor keeps writing into the renamed .1 through its old fd.
        assert!(
            b.contains("=== $(date -Is) relaunching daemon (attempt $restarts) ==="),
            "relaunch should stamp a dated banner into the fresh log"
        );
        assert!(
            b.contains("=== $(date -Is) starting soluterminal WSL daemon ==="),
            "the original start banner must survive"
        );
    }

    /// Relaunching must not evict the session's own preamble. A boot-crash
    /// loop runs 10 attempts 2s apart; rotating on each one would delete the
    /// staging decisions, the start banner and the FIRST failure inside six
    /// seconds, which is the only part anyone reads a crash log for.
    #[test]
    fn relaunch_rotation_is_size_gated() {
        let b = body();
        let branch_start = b
            .find("daemon exited (status=$status); relaunching in 2s")
            .expect("crash-relaunch branch missing");
        let rest = &b[branch_start..];
        let branch_end = rest.find("  launch_daemon\n").expect("relaunch never relaunches");
        let branch = &rest[..branch_end];
        assert!(
            branch.contains("rotate_daemon_log_if_big"),
            "the relaunch must rotate through the size gate: {branch}"
        );
        assert!(
            !branch.lines().any(|l| l.trim() == "rotate_daemon_log"),
            "the relaunch must NOT rotate unconditionally: {branch}"
        );
        assert!(
            branch.contains("exec >> \"$DAEMON_DIR/daemon.log\" 2>&1"),
            "the relaunch must re-point stdout at the live file (append): {branch}"
        );
        // The gate compares against the same cap the host-native supervisor
        // rotates at; a hard-coded mismatch here would age one platform's logs
        // out faster than the other's for no stated reason.
        assert!(
            b.contains(&format!("-ge {} ]; then rotate_daemon_log; fi", super::DAEMON_LOG_MAX_BYTES)),
            "the size gate must compare against DAEMON_LOG_MAX_BYTES"
        );
    }

    /// Rotation is best-effort or it is a liability: the script runs under
    /// `set -e`, so an `rm`/`mv` that fails on a read-only, full, or otherwise
    /// hostile $DAEMON_DIR would abort the supervisor itself — the WSL backend
    /// goes dark, and the only evidence lost is the failure that caused it.
    #[test]
    fn rotation_steps_are_best_effort() {
        let b = body();
        let rotate = extract_fn(&b, "rotate_daemon_log");
        for line in rotate.lines() {
            let t = line.trim();
            if t.starts_with("rm ") || t.contains("mv -f") {
                assert!(
                    t.ends_with("|| true") || t.ends_with("|| true; fi"),
                    "rotation step can abort the supervisor under set -e: {t}"
                );
            }
        }
        assert!(
            rotate.contains("  return 0\n"),
            "rotate_daemon_log must return 0 so a false test cannot trip set -e"
        );
    }

    /// The same claim, executed: run the generated rotate function against a
    /// $DAEMON_DIR it cannot fix. Directories where the log files belong make
    /// `rm -f` fail with EISDIR (even as root) and `mv -f` fail onto a
    /// non-empty target — stand-ins for the read-only/quota failures we cannot
    /// portably simulate. Before the `|| true` fix this aborted the script.
    #[cfg(unix)]
    #[test]
    fn rotation_survives_a_hostile_daemon_dir() {
        let b = body();
        let rotate = extract_fn(&b, "rotate_daemon_log");

        // Scenario A: only the oldest generation is un-removable.
        // Scenario B: every generation is, so the `mv` steps fail too.
        for (tag, poison) in [
            ("oldest-only", vec!["daemon.log.2"]),
            ("all", vec!["daemon.log", "daemon.log.1", "daemon.log.2"]),
        ] {
            let dir = match scratch_dir(&format!("rotate-hostile-{tag}")) {
                Some(d) => d,
                None => return,
            };
            for name in ["daemon.log", "daemon.log.1", "daemon.log.2"] {
                let p = dir.join(name);
                if poison.contains(&name) {
                    std::fs::create_dir_all(p.join("blocker")).expect("poison dir");
                } else {
                    std::fs::write(&p, "some log bytes\n").expect("seed log");
                }
            }
            let script = format!(
                "set -e\nDAEMON_DIR='{}'\n{rotate}\nrotate_daemon_log\necho SURVIVED\n",
                dir.display()
            );
            match run_bash(&script) {
                Some(out) => {
                    let stdout = String::from_utf8_lossy(&out.stdout).into_owned();
                    let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
                    assert!(
                        out.status.success() && stdout.contains("SURVIVED"),
                        "rotation aborted the script on a hostile DAEMON_DIR ({tag}); \
                         status={:?} stdout={stdout} stderr={stderr}",
                        out.status.code()
                    );
                }
                None => {
                    let _ = std::fs::remove_dir_all(&dir);
                    return; // no bash on this host; nothing to prove here.
                }
            }
            let _ = std::fs::remove_dir_all(&dir);
        }
    }

    /// A self-update successor is the SAME run continuing under a new pid, not
    /// a relaunch. Rotating there would split one daemon's life across two
    /// files for no reason, and touching this block at all risks the handoff.
    #[test]
    fn successor_handoff_is_rotation_free() {
        let b = body();
        let start = b
            .find("supervising update successor pid=")
            .expect("successor handoff block missing");
        let rest = &b[start..];
        let end = rest.find("continue").expect("successor handoff has no continue");
        let block = &rest[..end];
        assert!(
            !block.contains("rotate_daemon_log"),
            "the successor handoff must not rotate: {block}"
        );
    }

    /// The orphaned-successor bind war. A self-update successor is forked by the
    /// outgoing daemon, so it is never the supervisor's child; if the supervisor
    /// dies mid-handoff (app quit, Windows tearing down the WSL relay) that
    /// successor survives holding :8787. Every later launch then fails with
    /// "port ... held by another soluterminal daemon" for all ten restarts while
    /// the orphan serves, which the UI shows as panes that reset on each
    /// keystroke.
    ///
    /// The fix is a port-ownership sweep, not another cmdline pattern: the
    /// survivor can carry different --shell-version / relay flags than this
    /// build emits, so pattern matching misses it. The sweep must run on the
    /// relaunch path (where the failure actually happens), not only once at
    /// startup.
    #[test]
    fn relaunch_clears_an_orphaned_port_holder() {
        let b = body();

        // Defined exactly once, and it keys identity off the port we bind.
        assert_eq!(
            b.matches("stop_port_holder() {\n").count(),
            1,
            "stop_port_holder must be defined exactly once"
        );
        let sweep = extract_fn(&b, "stop_port_holder");
        assert!(
            sweep.contains("--addr $ADDR"),
            "the sweep must identify the holder by our own --addr, not by flags: {sweep}"
        );
        assert!(
            sweep.contains("/proc/[0-9]*"),
            "the sweep must scan same-user processes for the port owner: {sweep}"
        );

        // ADDR is the single source of truth, and the holder check must read it.
        assert!(
            b.contains("ADDR=\"0.0.0.0:8787\"\n"),
            "the listen address must be defined once as ADDR"
        );
        assert!(
            b.contains("\"$LOCAL\" --addr \"$ADDR\""),
            "launch_daemon must bind through $ADDR so it cannot drift from the sweep"
        );

        // The load-bearing part: a sweep on the relaunch path, before rebinding.
        let relaunch = b
            .find("daemon exited (status=$status); relaunching in 2s")
            .expect("crash-relaunch branch missing");
        let rest = &b[relaunch..];
        let end = rest
            .find("  launch_daemon\n")
            .expect("relaunch never relaunches");
        let branch = &rest[..end];
        assert!(
            branch.contains("stop_port_holder\n"),
            "the relaunch must clear the real port holder before rebinding, \
             or every attempt fails identically and the cap is reached with the \
             orphan still serving: {branch}"
        );
    }

    /// The sweep runs under `set -e` and must never abort the supervisor: a
    /// read-only /proc entry, a vanished pid, or a port with no solud on it all
    /// have to be survivable. Mirrors rotation_steps_are_best_effort.
    #[cfg(unix)]
    #[test]
    fn port_holder_sweep_is_best_effort() {
        let b = body();
        let sweep = extract_fn(&b, "stop_port_holder");

        // Abstract: every dangerous command tolerates failure.
        for line in sweep.lines() {
            let t = line.trim();
            if t.starts_with("kill ") || t.starts_with("tr ") {
                assert!(
                    t.contains("|| true"),
                    "sweep step can abort the supervisor under set -e: {t}"
                );
            }
        }
        assert!(
            sweep.contains("  return 0\n"),
            "stop_port_holder must return 0 so a false test cannot trip set -e"
        );

        // Executed: with a port nobody holds, the sweep is a silent no-op and
        // the caller continues.
        let script = format!(
            "set -e\nADDR='127.0.0.1:65281'\n{}{}{}\nstop_port_holder\necho SURVIVED\n",
            "stop_pid() { pid=\"$1\"; [ -n \"$pid\" ] || return 0; kill \"$pid\" 2>/dev/null || true; sleep 0.5; kill -9 \"$pid\" 2>/dev/null || true; }\n",
            "",
            sweep,
        );
        match run_bash(&script) {
            Some(out) => {
                let stdout = String::from_utf8_lossy(&out.stdout);
                assert!(
                    out.status.success() && stdout.contains("SURVIVED"),
                    "the sweep aborted the supervisor on an unheld port; \
                     status={:?} stdout={stdout} stderr={}",
                    out.status.code(),
                    String::from_utf8_lossy(&out.stderr)
                );
            }
            None => return, // no bash on this host.
        }
    }

    /// The sweep, executed against a live impostor. A process whose own
    /// `--addr` names the port we are about to bind must die, and one that
    /// merely mentions the port in an argument must not. This is the whole
    /// point of the fix, so it is proven rather than asserted.
    #[cfg(unix)]
    #[test]
    fn port_holder_sweep_kills_the_owner_and_spares_bystanders() {
        use std::process::{Command, Stdio};

        let sweep = extract_fn(&body(), "stop_port_holder");
        let dir = match scratch_dir("port-holder-sweep") {
            Some(d) => d,
            None => return,
        };
        let script_path = dir.join("sweep.sh");
        let script = format!(
            "set -e\nDAEMON_DIR='{}'\nADDR='127.0.0.1:65280'\n\
             stop_pid() {{ local p=\"$1\"; [ -n \"$p\" ] || return 0; \
             kill \"$p\" 2>/dev/null || true; sleep 0.5; \
             kill -9 \"$p\" 2>/dev/null || true; }}\n{sweep}\nstop_port_holder\n",
            dir.display()
        );
        std::fs::write(&script_path, script.as_bytes()).expect("write sweep script");

        // Impostor: a real orphan carries our --addr (and lives under a
        // soluterminal path, which is what distinguishes it from a stranger
        // merely holding the same port). python3 is used because `sleep`
        // rejects unknown flags and would exit on its own, which would make
        // the liveness assertions below meaningless either way.
        let mut owner = match Command::new("python3")
            .args([
                "-c",
                "import time; time.sleep(600)",
                "--solu-bin=/home/u/.local/share/soluterminal/solud",
                "--addr",
                "127.0.0.1:65280",
            ])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
        {
            Ok(c) => c,
            Err(_) => {
                let _ = std::fs::remove_dir_all(&dir);
                return; // no python3 on this host.
            }
        };
        // Bystander: mentions the port but its own --addr is elsewhere, so it
        // must survive (guards against a sweep that kills by mere substring).
        let mut bystander = match Command::new("python3")
            .args([
                "-c",
                "import time; time.sleep(600)",
                "--solu-bin=/home/u/.local/share/soluterminal/solud",
                "--addr",
                "127.0.0.1:1",
                "--note",
                "127.0.0.1:65280",
            ])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
        {
            Ok(c) => c,
            Err(_) => {
                let _ = owner.kill();
                let _ = std::fs::remove_dir_all(&dir);
                return;
            }
        };

        std::thread::sleep(std::time::Duration::from_millis(300));
        let out = Command::new("bash").arg(&script_path).output();
        std::thread::sleep(std::time::Duration::from_millis(300));

        let owner_alive = owner.try_wait().ok().flatten().is_none();
        let bystander_alive = bystander.try_wait().ok().flatten().is_none();
        let _ = owner.kill();
        let _ = bystander.kill();
        let _ = owner.wait();
        let _ = bystander.wait();
        let _ = std::fs::remove_dir_all(&dir);

        if let Ok(o) = &out {
            assert!(
                o.status.success(),
                "sweep aborted under set -e: {}",
                String::from_utf8_lossy(&o.stderr)
            );
        } else {
            return; // no bash on this host.
        }
        assert!(
            !owner_alive,
            "sweep left the port owner (--addr 127.0.0.1:65280) alive; \
             the relaunch would fail to bind exactly as before"
        );
        assert!(
            bystander_alive,
            "sweep killed a process that only MENTIONED the port in another \
             argument; identity must key off --addr, not a substring"
        );
    }

    /// The launch line carries `--relay <url>` exactly when the build has a
    /// hosted cloud, and nothing in its place otherwise.
    #[test]
    fn launch_line_passes_relay_only_with_a_hosted_cloud() {
        let b = body();
        let line = b
            .lines()
            .find(|l| l.contains("\"$LOCAL\" --addr \"$ADDR\""))
            .expect("launch line");
        match crate::hosted_cloud::relay_url() {
            Some(url) => assert!(line.contains(&format!(" --relay {url} --backend-id wsl ")), "{line}"),
            None => {
                assert!(!line.contains("--relay"), "{line}");
                assert!(line.contains("--addr \"$ADDR\" --backend-id wsl "), "{line}");
            }
        }
    }

    /// The pidfile handoff is explicitly frozen — these are the primitives the
    /// update flow depends on, asserted so a future log tweak cannot quietly
    /// take them out.
    #[test]
    fn pidfile_handoff_primitives_intact() {
        let b = body();
        for needle in [
            "PIDFILE=\"$DAEMON_DIR/solud-wsl.pid\"",
            "await_daemon() {",
            "launch_daemon() {",
            "while kill -0 \"$daemon_pid\" 2>/dev/null; do",
            "supervising newer update successor pid=",
            "daemon_is_child=0",
        ] {
            assert!(b.contains(needle), "start-wsl.sh lost handoff primitive: {needle}");
        }
    }

    /// The generated launcher must export the user's node toolchain before any
    /// daemon launch: a Windows desktop start hands WSL a minimal PATH with no
    /// nvm/asdf/Volta node, which made node-based provider CLIs fail with
    /// "env: node: No such file or directory". The Go launcher test executes
    /// this prelude; this is the template-shape guard beside it.
    #[test]
    fn establishes_node_toolchain_path_before_launch() {
        let b = body();
        let home_at = b
            .find("export HOME=\"$WSL_HOME\"")
            .expect("HOME export missing");
        let path_at = b
            .find("BASE_PATH=\"$PATH\"")
            .expect("node-toolchain PATH prelude missing");
        let daemon_dir_at = b.find("DAEMON_DIR=").expect("DAEMON_DIR missing");
        assert!(
            home_at < path_at && path_at < daemon_dir_at,
            "PATH must be established after HOME and before any daemon launch"
        );
        for needle in [
            "export PATH=\"$HOME/.local/bin:$HOME/.volta/bin:$BASE_PATH\"",
            "$HOME/.asdf/asdf.sh",
            "$HOME/.nvm/nvm.sh",
            "nvm use default",
        ] {
            assert!(
                b.contains(needle),
                "start-wsl.sh lost node-toolchain setup: {needle}"
            );
        }
    }

    /// A syntax error here bricks the whole WSL backend, and nothing else in
    /// the build would catch it. Skips (rather than fails) where bash is not
    /// available — a Windows dev box has no business failing on this.
    #[cfg(unix)]
    #[test]
    fn generated_script_parses_under_bash() {
        use std::process::Command;

        let dir = match scratch_dir("start-wsl-syntax") {
            Some(d) => d,
            None => return,
        };
        let path = dir.join("start-wsl.sh");
        std::fs::write(&path, body().as_bytes()).expect("write temp script");

        let out = match Command::new("bash").arg("-n").arg(&path).output() {
            Ok(o) => o,
            Err(e) => {
                eprintln!("skipping bash -n check: bash unavailable ({e})");
                let _ = std::fs::remove_dir_all(&dir);
                return;
            }
        };
        let _ = std::fs::remove_dir_all(&dir);
        assert!(
            out.status.success(),
            "generated start-wsl.sh is not valid bash:\n{}",
            String::from_utf8_lossy(&out.stderr)
        );
    }

    #[cfg(unix)]
    #[test]
    fn generated_staging_keeps_rolled_back_daemon_while_bundle_is_held() {
        let b = body();
        let hold_check = extract_fn(&b, "rollback_hold_active");
        let version_compare = extract_fn(&b, "ver_gt");
        let staging_start = b
            .find("stage_reason=\"\"\n")
            .expect("generated staging decision missing");
        let staging_end = b[staging_start..]
            .find("trap cleanup_managed_daemon")
            .map(|offset| staging_start + offset)
            .expect("generated staging decision has no boundary");
        let staging = &b[staging_start..staging_end];
        let dir = match scratch_dir("rollback-hold") {
            Some(d) => d,
            None => return,
        };
        let state_dir = dir.join(".solu");
        std::fs::create_dir_all(&state_dir).expect("create rollback state dir");
        std::fs::write(
            state_dir.join("update_rollback.json"),
            r#"{"schema":1,"hold":{"version":"2.0.0","fromVersion":"1.0.0","reason":"regression","createdAtMs":1000,"expiresAtMs":4102444800000}}"#,
        )
        .expect("write rollback hold");
        let local = dir.join("solud");
        let marker = dir.join("solud.version");
        let rolled_back = b"\x7fELFrolled-back";
        std::fs::write(&local, rolled_back).expect("write rolled-back daemon fixture");
        std::fs::write(&marker, "1.0.0\n").expect("write rolled-back version marker");
        let script = format!(
            "set -e\nHOME='{}'\nLOCAL='{}'\nLOCAL_VERSION_FILE='{}'\nBUNDLE='/does/not/exist'\nBUNDLE_VERSION='2.0.0'\n{hold_check}\n{version_compare}\n{staging}\necho HELD\n",
            dir.display(),
            local.display(),
            marker.display(),
        );
        let out = run_bash(&script).expect("bash available");
        assert!(
            out.status.success() && String::from_utf8_lossy(&out.stdout).contains("HELD"),
            "generated launcher did not honor active rollback hold: status={:?} stderr={}",
            out.status.code(),
            String::from_utf8_lossy(&out.stderr),
        );
        assert_eq!(
            std::fs::read(&local).expect("read local daemon after staging"),
            rolled_back,
            "launcher replaced the rolled-back daemon with the held bundle",
        );
        assert_eq!(
            std::fs::read_to_string(&marker).expect("read local marker after staging"),
            "1.0.0\n",
            "launcher raised the marker to the held bundle version",
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The kill loop of 2026-09-24: the shell's short-budget probe missed a
    /// slow-but-healthy daemon, ran this script, and the script's sweep killed
    /// that daemon on sight (30 forced restarts in one morning). Every sweep
    /// must now sit behind claim_supervision, and a healthy current daemon
    /// must be adopted, never swept or relaunched.
    #[test]
    fn nothing_is_killed_before_supervision_is_claimed() {
        let b = body();
        let claim = b
            .find("\nclaim_supervision\n")
            .expect("claim_supervision is never called");
        let gated = "if [ -z \"$ADOPT_PID\" ]; then\n  stop_stale_managed_daemons\nfi\n";
        let gate = b.find(gated).expect("the stale-daemon sweep must be gated on ADOPT_PID");
        assert!(claim < gate, "the sweep runs before supervision is claimed");
        // No top-level kill runs before the claim: everything above it is a
        // function definition or setup, so a stray `kill` line there would be
        // an unconditional sweep again.
        for line in b[..claim].lines() {
            assert!(
                !(line.starts_with("kill ")
                    || line == "stop_port_holder"
                    || line == "stop_stale_managed_daemons"),
                "top-level kill before claim_supervision: {line}"
            );
        }
        // The first launch is skipped for an adopted daemon, and so is the
        // port-holder sweep in front of it (which would kill that daemon).
        let adopt = b
            .find("if [ -n \"$ADOPT_PID\" ]; then\n")
            .expect("adoption branch missing");
        let branch = &b[adopt..b[adopt..].find("\nfi\n").map(|e| adopt + e).unwrap()];
        let (adopted, fresh) = branch.split_once("\nelse\n").expect("adoption branch has no else");
        assert!(adopted.contains("daemon_is_child=0"), "an adopted daemon is not our child: {adopted}");
        assert!(!adopted.contains("launch_daemon") && !adopted.contains("stop_port_holder"),
            "adopting must not relaunch or sweep: {adopted}");
        assert!(fresh.contains("  stop_port_holder\n") && fresh.trim_end().ends_with("launch_daemon"),
            "a fresh start must still clear the port and launch: {fresh}");
    }

    /// A supervisor told to stop must stop. The old `trap cleanup INT TERM HUP`
    /// ran the cleanup and then resumed the loop, so it killed its daemon and
    /// relaunched it; stray supervisors piled up and took turns doing that.
    #[test]
    fn stop_signals_end_the_supervisor() {
        let b = body();
        let handler = extract_fn(&b, "on_stop_signal");
        assert!(handler.contains("  exit "), "on_stop_signal must exit: {handler}");
        assert!(b.contains("trap on_stop_signal INT TERM\n"));
        assert!(b.contains("trap on_hangup HUP\n"), "SIGHUP must not stop the backend");
        for line in b.lines().filter(|l| !l.trim_start().starts_with('#')) {
            assert!(
                !(line.starts_with("trap cleanup_managed_daemon ") && line != "trap cleanup_managed_daemon EXIT"),
                "a bare cleanup trap on a signal resumes the supervisor loop: {line}"
            );
        }
    }

    /// Killing the Windows-side wsl.exe (every app quit, restart or update)
    /// delivers SIGHUP to the supervisor, measured, and nothing to the daemon.
    /// The supervisor must neither stop nor relaunch: the interrupted `wait`
    /// must not be read as the daemon exiting.
    #[cfg(unix)]
    #[test]
    fn hangup_leaves_the_daemon_running() {
        let b = body();
        let hangup = extract_fn(&b, "on_hangup");
        let await_fn = extract_fn(&b, "await_daemon");
        let script = format!(
            "set -e\nPIDFILE=/dev/null\n{hangup}\ntrap on_hangup HUP\n{await_fn}\n\
             sleep 2 & daemon_pid=$!; daemon_is_child=1\n\
             ( sleep 0.3; kill -HUP $$ ) &\n\
             start=$(date +%s%N)\n\
             await_daemon\n\
             echo \"status=$status elapsed_ms=$(( ($(date +%s%N) - start) / 1000000 ))\"\n"
        );
        let Some(out) = run_bash(&script) else { return };
        let stdout = String::from_utf8_lossy(&out.stdout);
        assert!(stdout.contains("keeps running"), "hangup was not handled: {stdout}");
        assert!(stdout.contains("status=0 "), "the daemon's real exit status was lost: {stdout}");
        let elapsed: u64 = stdout
            .split("elapsed_ms=")
            .nth(1)
            .and_then(|v| v.trim().parse().ok())
            .unwrap_or(0);
        assert!(elapsed >= 1500, "await_daemon returned on the HUP, not the exit: {stdout}");
    }

    /// Executed version: a supervisor-shaped loop with the generated handler
    /// must exit on TERM instead of reaching its relaunch line.
    #[cfg(unix)]
    #[test]
    fn stop_signal_is_not_followed_by_a_relaunch() {
        let handler = extract_fn(&body(), "on_stop_signal");
        let script = format!(
            "set -e\ncleanup_managed_daemon() {{ kill \"$daemon_pid\" 2>/dev/null; echo CLEANED; }}\n{handler}\n\
             trap on_stop_signal INT TERM HUP\n\
             sleep 30 & daemon_pid=$!\n\
             ( sleep 0.3; kill -TERM $$ ) &\n\
             set +e; wait \"$daemon_pid\"; set -e\n\
             kill \"$daemon_pid\" 2>/dev/null || true\n\
             echo RELAUNCHED\n"
        );
        let Some(out) = run_bash(&script) else { return };
        let stdout = String::from_utf8_lossy(&out.stdout);
        assert!(stdout.contains("CLEANED"), "cleanup did not run: {stdout}");
        assert!(!stdout.contains("RELAUNCHED"), "supervisor resumed after TERM: {stdout}");
        assert_eq!(out.status.code(), Some(143), "stop must exit 143: {stdout}");
    }

    /// flock locks belong to the open file; a daemon that inherited fd 9 would
    /// keep the supervisor lock after its supervisor died.
    #[test]
    fn launched_daemon_does_not_inherit_the_supervisor_lock() {
        let launch = extract_fn(&body(), "launch_daemon");
        assert!(launch.contains(" 9>&- &\n"), "daemon launch must close fd 9: {launch}");
    }

    /// The probe asks the daemon on loopback (token-exempt) with a budget that
    /// outlasts a loaded host, and never puts the token on a command line.
    #[test]
    fn live_probe_is_loopback_patient_and_keeps_the_token_out_of_argv() {
        let probe = extract_fn(&body(), "probe_live_daemon");
        assert!(probe.contains("/dev/tcp/127.0.0.1/"), "probe must use loopback: {probe}");
        assert!(probe.contains("timeout 20 bash -c"), "probe budget changed: {probe}");
        assert!(probe.contains("SOLU_PROBE_TOKEN=\"$tok\" timeout"), "token must travel in env: {probe}");
        assert!(probe.contains("' probe \"$port\" 2>/dev/null"), "only the port may be an argument: {probe}");
    }

    /// /api/health repeats "version" and "pid" in every fleet component; only
    /// the daemon's own, top-level values may be read.
    #[cfg(unix)]
    #[test]
    fn json_reader_sees_only_top_level_keys() {
        let reader = extract_fn(&body(), "json_top_value");
        let json = r#"{"addr":"0.0.0.0:8787","backendID":"wsl","fleet":{"components":[{"pid":11,"version":"0.0.1059","note":"has \"quotes\", {braces} and [brackets]"},{"version":"9.9.9"}]},"panes":72,"pid":377206,"system":{"version":"8.8.8"},"version":"0.0.1140","webSourceSha":"abc","webVersion":"x"}"#;
        for (key, want) in [
            ("version", "0.0.1140"),
            ("pid", "377206"),
            ("backendID", "wsl"),
            ("panes", "72"),
            ("webVersion", "x"),
            ("missing", ""),
        ] {
            let script = format!("{reader}\nprintf '%s' '{json}' | json_top_value {key}\n");
            let Some(out) = run_bash(&script) else { return };
            assert_eq!(
                String::from_utf8_lossy(&out.stdout).trim(),
                want,
                "json_top_value {key}; stderr={}",
                String::from_utf8_lossy(&out.stderr)
            );
        }
    }

    /// Under `set -e`, a process exiting between `[ -r ]` and the read of its
    /// cmdline must not abort the supervisor, so every such read is guarded.
    #[test]
    fn every_proc_cmdline_read_is_guarded() {
        for line in body().lines() {
            if line.contains("/cmdline\"") && line.contains("tr '\\0' ' '") {
                assert!(line.contains("|| true"), "unguarded /proc read: {line}");
            }
        }
    }

    /// Each sweep of the process table is one grep, not a fork per process:
    /// the per-process form took ~29s a pass at load 100.
    #[test]
    fn process_sweeps_prefilter_with_one_grep() {
        let b = body();
        for name in ["stop_port_holder", "stop_stale_managed_daemons", "retire_stray_supervisors"] {
            let f = extract_fn(&b, name);
            assert!(
                f.contains("$(grep -laFs -- ") && !f.contains("for proc in /proc/[0-9]*; do"),
                "{name} must prefilter candidates with a single grep: {f}"
            );
        }
    }

    /// The shell must be able to find the WSL daemon without the VM-IP
    /// lookup (a wsl.exe call that times out under load): both loopback
    /// families are candidates, and every candidate is a dialable address.
    #[test]
    fn wsl_loopback_candidates_cover_both_address_families() {
        let c = super::WSL_LOOPBACK_CANDIDATES;
        assert!(c.contains(&"127.0.0.1:8787") && c.contains(&"[::1]:8787"), "{c:?}");
        for addr in c {
            assert!(addr.parse::<std::net::SocketAddr>().is_ok(), "{addr} is not a socket address");
        }
    }

    #[test]
    fn wsl_identity_probe_outlasts_a_loaded_daemon() {
        assert!(super::WSL_IDENTITY_PROBE_TIMEOUT >= std::time::Duration::from_secs(5));
    }

    /// Lift one shell function out of the template so it can be run on its own.
    fn extract_fn(body: &str, name: &str) -> String {
        let head = format!("{name}() {{\n");
        let start = body
            .find(&head)
            .unwrap_or_else(|| panic!("{name} is not defined in start-wsl.sh"));
        let rest = &body[start..];
        let end = rest
            .find("\n}\n")
            .unwrap_or_else(|| panic!("{name} has no closing brace"));
        rest[..end + 3].to_string()
    }

    /// A unique scratch directory; `None` if temp is unusable, which is a skip
    /// rather than a failure (these tests check the template, not the host).
    #[cfg(unix)]
    fn scratch_dir(tag: &str) -> Option<std::path::PathBuf> {
        use std::time::{SystemTime, UNIX_EPOCH};

        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let dir = std::env::temp_dir().join(format!("solu-{tag}-{}-{nanos}", std::process::id()));
        match std::fs::create_dir_all(&dir) {
            Ok(()) => Some(dir),
            Err(e) => {
                eprintln!("skipping: no usable temp dir ({e})");
                None
            }
        }
    }

    /// Run a snippet through bash; `None` means bash is unavailable (skip).
    #[cfg(unix)]
    fn run_bash(script: &str) -> Option<std::process::Output> {
        match std::process::Command::new("bash").arg("-c").arg(script).output() {
            Ok(o) => Some(o),
            Err(e) => {
                eprintln!("skipping: bash unavailable ({e})");
                None
            }
        }
    }
}

/// The host-native supervisor keeps its own daemon.log, and it has to age out
/// on the same terms as the WSL launcher's and the daemon's own solud.log —
/// two archived generations, so two crashes in a row cannot overwrite the
/// first one's tail.
#[cfg(test)]
mod daemon_log_rotation_tests {
    use super::rotate_daemon_log_file;

    fn scratch_dir(tag: &str) -> Option<std::path::PathBuf> {
        use std::time::{SystemTime, UNIX_EPOCH};

        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let dir = std::env::temp_dir().join(format!("solu-{tag}-{}-{nanos}", std::process::id()));
        match std::fs::create_dir_all(&dir) {
            Ok(()) => Some(dir),
            Err(e) => {
                eprintln!("skipping: no usable temp dir ({e})");
                None
            }
        }
    }

    #[test]
    fn keeps_two_generations_and_drops_the_oldest() {
        let dir = match scratch_dir("host-log-rotate") {
            Some(d) => d,
            None => return,
        };
        let live = dir.join("daemon.log");
        std::fs::write(&live, "the run that just died\n").expect("seed live log");
        std::fs::write(dir.join("daemon.log.1"), "previous run\n").expect("seed .1");
        std::fs::write(dir.join("daemon.log.2"), "ancient run\n").expect("seed .2");

        rotate_daemon_log_file(&live, 4);

        assert_eq!(
            std::fs::read_to_string(dir.join("daemon.log.1")).expect("read .1"),
            "the run that just died\n",
            "the dead run must become the first archived generation"
        );
        assert_eq!(
            std::fs::read_to_string(dir.join("daemon.log.2")).expect("read .2"),
            "previous run\n",
            "the previous run must shift down instead of being overwritten"
        );
        assert!(
            !dir.join("daemon.log.3").exists(),
            "generations must stay bounded at .2"
        );
        assert!(!live.exists(), "the live file is renamed, not copied");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn leaves_a_log_under_the_cap_alone() {
        let dir = match scratch_dir("host-log-keep") {
            Some(d) => d,
            None => return,
        };
        let live = dir.join("daemon.log");
        std::fs::write(&live, "still small\n").expect("seed live log");

        rotate_daemon_log_file(&live, 1024);

        assert_eq!(
            std::fs::read_to_string(&live).expect("read live"),
            "still small\n",
            "a log under the cap must be appended to, never rotated"
        );
        assert!(!dir.join("daemon.log.1").exists());
        let _ = std::fs::remove_dir_all(&dir);
    }
}

#[cfg(test)]
mod pick_wsl_ipv4_tests {
    use super::pick_wsl_ipv4;

    #[test]
    fn plain_eth0_first() {
        assert_eq!(pick_wsl_ipv4("172.29.132.65 \n"), Some("172.29.132.65".into()));
    }

    #[test]
    fn skips_tailscale_cgnat_before_eth0() {
        // The field case: Tailscale-in-WSL lists its CGNAT address first.
        assert_eq!(
            pick_wsl_ipv4("100.64.12.34 172.29.132.65"),
            Some("172.29.132.65".into())
        );
    }

    #[test]
    fn skips_ipv6_and_junk_tokens() {
        assert_eq!(
            pick_wsl_ipv4("fe80::215:5dff:fe00:1 100.64.12.34 10.0.0.7"),
            Some("10.0.0.7".into())
        );
    }

    #[test]
    fn falls_back_to_cgnat_when_nothing_else() {
        // Better a maybe-reachable CGNAT than nothing — 127.0.0.1 is still a
        // separate upstream candidate at the call site.
        assert_eq!(pick_wsl_ipv4("100.64.12.34"), Some("100.64.12.34".into()));
    }

    #[test]
    fn skips_loopback_and_link_local() {
        assert_eq!(
            pick_wsl_ipv4("127.0.0.1 169.254.10.5 192.168.4.20"),
            Some("192.168.4.20".into())
        );
    }

    #[test]
    fn empty_output_is_none() {
        assert_eq!(pick_wsl_ipv4("  \n"), None);
    }
}

#[cfg(test)]
mod supervisor_tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    /// A child that exits immediately with the given code.
    fn dead_child(code: i32) -> std::io::Result<Child> {
        Command::new("sh")
            .args(["-c", &format!("exit {code}")])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
    }

    /// A child that stays up until it's killed.
    fn live_child() -> std::io::Result<Child> {
        Command::new("sh")
            .args(["-c", "sleep 300"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
    }

    fn wait_until(what: &str, mut done: impl FnMut() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while Instant::now() < deadline {
            if done() {
                return;
            }
            thread::sleep(Duration::from_millis(50));
        }
        panic!("timed out waiting for {what}");
    }

    /// The bug this whole thing exists to fix: the daemon dies and nobody
    /// brings it back. The respawn must also carry a description of the death
    /// so the replacement daemon can report its predecessor to the cloud —
    /// fatal deaths used to be invisible fleet-wide.
    #[test]
    fn respawns_a_daemon_that_crashed() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let reasons: Arc<Mutex<Vec<Option<String>>>> = Arc::new(Mutex::new(Vec::new()));
        let s = spawns.clone();
        let r = reasons.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |reason| {
                s.fetch_add(1, Ordering::SeqCst);
                r.lock().unwrap().push(reason.map(str::to_owned));
                dead_child(1)
            }),
            healthy: Box::new(|| false),
        };
        let h = start_supervisor(cfg, dead_child(1).unwrap());
        wait_until("respawn", || spawns.load(Ordering::SeqCst) >= 2);
        h.stop();
        let seen = reasons.lock().unwrap();
        let first = seen.first().cloned().flatten();
        match first {
            Some(reason) => assert!(
                reason.contains("exited"),
                "crash respawn reason should describe the exit, got {reason:?}"
            ),
            None => panic!("a crash respawn must carry a death reason for the crash report"),
        }
    }

    /// A daemon that is up must be left alone — a watchdog that restarts a
    /// healthy daemon is worse than no watchdog.
    #[test]
    fn leaves_a_live_daemon_alone() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let s = spawns.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |_reason| {
                s.fetch_add(1, Ordering::SeqCst);
                live_child()
            }),
            healthy: Box::new(|| true),
        };
        let h = start_supervisor(cfg, live_child().unwrap());
        thread::sleep(Duration::from_secs(2));
        assert_eq!(spawns.load(Ordering::SeqCst), 0, "respawned a healthy daemon");
        h.stop();
    }

    /// A self-update exits 0 and hands :8787 to a successor it spawned
    /// itself. Respawning into that would fight the successor for the port.
    #[test]
    fn stands_down_when_a_successor_takes_the_port() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let s = spawns.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |_reason| {
                s.fetch_add(1, Ordering::SeqCst);
                live_child()
            }),
            // Successor is already serving by the time we look.
            healthy: Box::new(|| true),
        };
        // Clean exit == the update handoff signal.
        let h = start_supervisor(cfg, dead_child(0).unwrap());
        thread::sleep(Duration::from_secs(2));
        assert_eq!(
            spawns.load(Ordering::SeqCst),
            0,
            "competed with the update successor for the port"
        );
        h.stop();
    }

    /// The handoff grace, isolated: with the port DOWN (successor still
    /// booting — the daemon allows it up to 25s), a clean exit must wait
    /// rather than race the successor for :8787...
    #[test]
    fn clean_exit_waits_for_a_booting_successor() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let s = spawns.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |_reason| {
                s.fetch_add(1, Ordering::SeqCst);
                live_child()
            }),
            healthy: Box::new(|| false),
        };
        let h = start_supervisor(cfg, dead_child(0).unwrap());
        thread::sleep(Duration::from_secs(3));
        assert_eq!(
            spawns.load(Ordering::SeqCst),
            0,
            "respawned during the update handoff grace"
        );
        h.stop();
    }

    /// ...but a CRASH gets no such grace: nobody is coming, so come back now.
    #[test]
    fn crash_respawns_without_waiting() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let s = spawns.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |_reason| {
                s.fetch_add(1, Ordering::SeqCst);
                live_child()
            }),
            healthy: Box::new(|| false),
        };
        let h = start_supervisor(cfg, dead_child(1).unwrap());
        wait_until("prompt respawn after a crash", || {
            spawns.load(Ordering::SeqCst) >= 1
        });
        h.stop();
    }

    /// Quitting the app must not be answered by resurrecting the daemon.
    #[test]
    fn stop_does_not_resurrect() {
        let spawns = Arc::new(AtomicUsize::new(0));
        let s = spawns.clone();
        let cfg = SupervisorCfg {
            spawn: Box::new(move |_reason| {
                s.fetch_add(1, Ordering::SeqCst);
                live_child()
            }),
            healthy: Box::new(|| false),
        };
        let h = start_supervisor(cfg, live_child().unwrap());
        h.stop();
        let after_stop = spawns.load(Ordering::SeqCst);
        thread::sleep(Duration::from_secs(2));
        assert_eq!(
            spawns.load(Ordering::SeqCst),
            after_stop,
            "watchdog respawned the daemon we were shutting down"
        );
    }

    /// stop() must actually kill the running daemon, not just stop watching.
    #[test]
    fn stop_kills_the_running_daemon() {
        let cfg = SupervisorCfg {
            spawn: Box::new(|_reason| live_child()),
            healthy: Box::new(|| true),
        };
        let child = live_child().unwrap();
        let pid = child.id();
        let h = start_supervisor(cfg, child);
        h.stop();
        wait_until("daemon to die", || {
            !Path::new(&format!("/proc/{pid}")).exists()
        });
    }

    /// A long backoff must not stall app shutdown.
    #[test]
    fn nap_returns_promptly_on_shutdown() {
        let flag = Arc::new(AtomicBool::new(false));
        let f = flag.clone();
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(150));
            f.store(true, Ordering::SeqCst);
        });
        let start = Instant::now();
        assert!(!nap(Duration::from_secs(30), &flag));
        assert!(
            start.elapsed() < Duration::from_secs(2),
            "nap ignored shutdown for {:?}",
            start.elapsed()
        );
    }
}

#[cfg(test)]
mod dual_windows_port_tests {
    use super::*;

    #[test]
    fn dual_windows_backend_avoids_the_wsl_forwarded_port() {
        assert_ne!(DUAL_WIN_ADDR, DEFAULT_DAEMON_ADDR);
        assert!(DUAL_WIN_ADDR.starts_with("127.0.0.1:"), "loopback only: {DUAL_WIN_ADDR}");
    }

    /// The Go daemon must treat the same port as canonical, or the Windows
    /// backend would never own its Claude/Codex MCP registration.
    #[test]
    fn dual_windows_port_matches_the_go_daemon() {
        let go = std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../daemon/claude_mcp_register.go"
        ))
        .expect("read daemon/claude_mcp_register.go");
        let port = DUAL_WIN_ADDR.rsplit(':').next().unwrap();
        let decl = format!("const dualWindowsDaemonPort = \"{port}\"");
        assert!(go.contains(&decl), "Go daemon is missing `{decl}`");
    }
}

#[cfg(test)]
mod wsl_existing_daemon_tests {
    use super::{
        find_reusable_wsl_daemon, require_current_wsl_backend_with_token,
        WSL_IDENTITY_PROBE_TIMEOUT,
    };
    use std::io::{BufRead, BufReader, Write};
    use std::net::TcpListener;
    use std::thread;
    use std::time::{Duration, Instant};

    /// Answer one health request after `response_delay`, optionally requiring a
    /// bearer token. `required_token` of `None` answers anything; a mismatch is
    /// answered as 401. Returns the address and the request text the client
    /// actually sent.
    fn serve_health(
        body: Vec<u8>,
        response_delay: Duration,
        required_token: Option<&str>,
    ) -> (String, thread::JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let required_token = required_token.map(str::to_owned);
        let server = thread::spawn(move || {
            let (mut socket, _) = listener.accept().unwrap();
            socket.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
            let mut reader = BufReader::new(socket.try_clone().unwrap());
            let mut request = String::new();
            loop {
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                request.push_str(&line);
                if line == "\r\n" || line.is_empty() {
                    break;
                }
            }
            thread::sleep(response_delay);
            let authorized = match required_token.as_ref() {
                None => true,
                Some(token) => request.to_ascii_lowercase().contains(&format!(
                    "authorization: bearer {}\r\n",
                    token.to_ascii_lowercase()
                )),
            };
            let response = if authorized {
                let mut response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                )
                .into_bytes();
                response.extend_from_slice(&body);
                response
            } else {
                b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                    .to_vec()
            };
            let _ = socket.write_all(&response);
            request
        });
        (addr, server)
    }

    fn identity(backend: &str, version: &str, hold: Option<&str>) -> Vec<u8> {
        let mut value = serde_json::json!({
            "status": "ok", "backendID": backend, "version": version, "pid": 4242
        });
        if let Some(hold) = hold {
            value["updateHoldVersion"] = serde_json::Value::String(hold.to_owned());
        }
        serde_json::to_vec(&value).unwrap()
    }

    /// The reuse loop in `ensure_daemon_running_wsl` keys on exactly this
    /// predicate: an `Ok` here skips the `start-wsl.sh` spawn path entirely.
    #[test]
    fn delayed_authenticated_wsl_identity_is_reused_without_spawn() {
        let (addr, server) = serve_health(
            identity("wsl", "999.0.0", None),
            Duration::from_millis(1500),
            Some("desktop-secret"),
        );
        let started = Instant::now();
        let result = require_current_wsl_backend_with_token(&addr, Some("desktop-secret"));
        assert!(result.is_ok(), "{result:?}");
        assert!(started.elapsed() >= Duration::from_millis(1400));
        let request = server.join().unwrap().to_ascii_lowercase();
        assert!(request.starts_with("get /api/health?identity=1 http/1.1\r\n"));
        assert!(request.contains("authorization: bearer desktop-secret\r\n"));
    }

    /// Drive the real startup reuse loop (`find_reusable_wsl_daemon`, the loop
    /// `ensure_daemon_running_wsl` runs before it would spawn start-wsl.sh) with
    /// a daemon that answers the authenticated identity probe after 1.5s. The
    /// loop must return that daemon's route on its first pass: `Some(route)` is
    /// the branch that proxies to the running daemon (`finish_wsl_proxy(route,
    /// None, ..)`, no child process), and it is the ONLY way out of the loop that
    /// skips the disk gate and the start-wsl.sh spawn, whose script kills and
    /// replaces whatever daemon holds :8787. The loopback candidates are
    /// answered by the fake probe without touching the host's real :8787.
    #[test]
    fn startup_reuse_loop_keeps_a_slow_but_healthy_wsl_daemon() {
        let (vm_addr, server) = serve_health(
            identity("wsl", "999.0.0", None),
            Duration::from_millis(1500),
            Some("desktop-secret"),
        );
        let candidates = vec![
            vm_addr.clone(),
            "127.0.0.1:8787".to_owned(),
            "[::1]:8787".to_owned(),
        ];
        let mut probes = Vec::new();
        let mut pauses = 0;
        let mut logs = Vec::new();
        let started = Instant::now();
        let reused = find_reusable_wsl_daemon(
            &candidates,
            true,
            |addr| {
                probes.push(addr.to_owned());
                if addr == vm_addr {
                    require_current_wsl_backend_with_token(addr, Some("desktop-secret"))
                } else {
                    Err(format!("request daemon health {addr} failed or timed out"))
                }
            },
            |_| pauses += 1,
            |line| logs.push(line.to_owned()),
        );
        let elapsed = started.elapsed();

        // Reused on the first pass: no retry pause, one probe per candidate,
        // and the route handed to the proxy is the slow daemon itself. A `None`
        // here is what would have spawned start-wsl.sh and replaced it.
        assert_eq!(reused.as_deref(), Some(vm_addr.as_str()), "{logs:?}");
        assert_eq!(pauses, 0, "{logs:?}");
        assert_eq!(probes, candidates);
        assert!(elapsed >= Duration::from_millis(1400), "{elapsed:?}");
        assert!(elapsed < WSL_IDENTITY_PROBE_TIMEOUT, "{elapsed:?}");
        assert!(
            !logs
                .iter()
                .any(|l| l.contains(&vm_addr) && l.contains("failed")),
            "{logs:?}"
        );
        assert!(
            logs.iter()
                .any(|l| l == &format!("reusing existing WSL daemon at {vm_addr}")),
            "{logs:?}"
        );
        let request = server.join().unwrap().to_ascii_lowercase();
        assert!(request.starts_with("get /api/health?identity=1 http/1.1\r\n"));
        assert!(request.contains("authorization: bearer desktop-secret\r\n"));
    }

    /// The same loop and the same 1.5s daemon, probed on the old 800ms default
    /// budget, falls through to the spawn path. This pins that the test above
    /// discriminates: the reuse only holds because the WSL identity probe has
    /// its own budget.
    #[test]
    fn startup_reuse_loop_on_the_old_800ms_budget_would_spawn() {
        let (vm_addr, server) = serve_health(
            identity("wsl", "999.0.0", None),
            Duration::from_millis(1500),
            Some("desktop-secret"),
        );
        let candidates = vec![vm_addr.clone()];
        let mut pauses = 0;
        let reused = find_reusable_wsl_daemon(
            &candidates,
            true,
            |addr| {
                crate::daemon_health::probe(addr, Some("desktop-secret"))
                    .and_then(|h| super::check_backend_identity(h, addr, super::BACKEND_ID_WSL))
                    .map(|_| ())
            },
            |_| pauses += 1,
            |_| {},
        );
        assert_eq!(reused, None);
        assert_eq!(pauses, 0);
        server.join().unwrap();
    }

    /// A stale daemon is still replaced: the loop stops at once and hands the
    /// caller the spawn path, without pausing to retry.
    #[test]
    fn startup_reuse_loop_hands_a_stale_daemon_to_the_spawn_path() {
        let (vm_addr, server) = serve_health(
            identity("wsl", "0.0.1", None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let candidates = vec![vm_addr.clone(), "127.0.0.1:8787".to_owned()];
        let mut probes = 0;
        let mut pauses = 0;
        let mut logs = Vec::new();
        let reused = find_reusable_wsl_daemon(
            &candidates,
            true,
            |addr| {
                probes += 1;
                assert_eq!(addr, vm_addr, "loop kept probing after a stale answer");
                require_current_wsl_backend_with_token(addr, Some("desktop-secret"))
            },
            |_| pauses += 1,
            |line| logs.push(line.to_owned()),
        );
        assert_eq!(reused, None);
        assert_eq!((probes, pauses), (1, 0));
        assert!(logs[0].starts_with(&format!(
            "not reusing WSL daemon at {vm_addr}: stale WSL daemon"
        )));
        server.join().unwrap();
    }

    #[test]
    fn invalid_identity_responses_are_not_reused() {
        let cases = [
            (
                identity("win", "999.0.0", None),
                Some("desktop-secret"),
                Some("desktop-secret"),
            ),
            (
                b"not-json".to_vec(),
                Some("desktop-secret"),
                Some("desktop-secret"),
            ),
            (
                identity("wsl", "999.0.0", None),
                Some("wrong-secret"),
                Some("desktop-secret"),
            ),
            (
                identity("wsl", "0.0.1", None),
                Some("desktop-secret"),
                Some("desktop-secret"),
            ),
        ];
        for (body, sent_token, required_token) in cases {
            let (addr, server) = serve_health(body, Duration::ZERO, required_token);
            let result = require_current_wsl_backend_with_token(&addr, sent_token);
            assert!(result.is_err(), "expected Err for {result:?}");
            server.join().unwrap();
        }
    }

    #[test]
    fn stale_version_is_rejected_without_a_matching_hold() {
        let (addr, server) = serve_health(
            identity("wsl", "0.0.1", None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let err =
            require_current_wsl_backend_with_token(&addr, Some("desktop-secret")).unwrap_err();
        assert!(err.starts_with("stale WSL daemon"), "{err}");
        server.join().unwrap();
    }

    /// A daemon that accepts the connection but never answers still yields a
    /// verdict within the WSL identity budget, so startup reaches its bounded
    /// spawn/fallback path instead of hanging on a dead backend. The server
    /// stays silent past the upper bound, so only the budget can end it.
    #[test]
    fn stalled_wsl_identity_is_bounded_by_the_identity_budget() {
        let (addr, server) = serve_health(
            identity("wsl", "999.0.0", None),
            WSL_IDENTITY_PROBE_TIMEOUT + Duration::from_millis(1500),
            Some("desktop-secret"),
        );
        let started = Instant::now();
        let err =
            require_current_wsl_backend_with_token(&addr, Some("desktop-secret")).unwrap_err();
        let elapsed = started.elapsed();
        assert!(err.contains("failed or timed out"), "{err}");
        assert!(!err.contains("desktop-secret"), "{err}");
        assert!(
            elapsed >= WSL_IDENTITY_PROBE_TIMEOUT - Duration::from_secs(1),
            "{elapsed:?}"
        );
        assert!(
            elapsed < WSL_IDENTITY_PROBE_TIMEOUT + Duration::from_secs(1),
            "{elapsed:?}"
        );
        server.join().unwrap();
    }

    #[test]
    fn rollback_hold_allows_intentionally_older_daemon() {
        let bundled = env!("SOLU_SHELL_VERSION");
        let (addr, server) = serve_health(
            identity("wsl", "0.0.1", Some(bundled)),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        assert!(require_current_wsl_backend_with_token(&addr, Some("desktop-secret")).is_ok());
        server.join().unwrap();
    }

    /// The 0.0.1199 → 0.0.1202 shell update: the WSL daemon is older than
    /// the new shell. It must stay connected (no Windows-only fallback) and
    /// be asked to update exactly once, with the token it authenticated with.
    #[test]
    fn older_wsl_daemon_stays_connected_and_is_asked_to_update() {
        use super::require_reusable_wsl_backend_with;
        use std::sync::Mutex;
        let (addr, server) = serve_health(
            identity("wsl", "0.0.1", None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let calls = Mutex::new(Vec::new());
        let on_stale = |a: &str, t: Option<&str>, reason: &str| {
            calls
                .lock()
                .unwrap()
                .push((a.to_string(), t.map(str::to_string), reason.to_string()));
        };
        assert!(require_reusable_wsl_backend_with(&addr, Some("desktop-secret"), &on_stale).is_ok());
        server.join().unwrap();
        let calls = calls.into_inner().unwrap();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].0, addr);
        assert_eq!(calls[0].1.as_deref(), Some("desktop-secret"));
        assert!(calls[0].2.starts_with("stale WSL daemon"), "{}", calls[0].2);
    }

    #[test]
    fn current_wsl_daemon_is_not_asked_to_update() {
        use super::require_reusable_wsl_backend_with;
        let (addr, server) = serve_health(
            identity("wsl", env!("SOLU_SHELL_VERSION"), None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let on_stale = |_: &str, _: Option<&str>, _: &str| panic!("current daemon flagged stale");
        assert!(require_reusable_wsl_backend_with(&addr, Some("desktop-secret"), &on_stale).is_ok());
        server.join().unwrap();
    }

    /// A newer daemon than the shell (daemon self-updated first) is fine too.
    #[test]
    fn newer_wsl_daemon_is_accepted() {
        use super::require_reusable_wsl_backend_with;
        let (addr, server) = serve_health(
            identity("wsl", "999.0.0", None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let on_stale = |_: &str, _: Option<&str>, _: &str| panic!("newer daemon flagged stale");
        assert!(require_reusable_wsl_backend_with(&addr, Some("desktop-secret"), &on_stale).is_ok());
        server.join().unwrap();
    }

    /// Accepting older versions must not accept something that isn't the WSL
    /// daemon at all.
    #[test]
    fn wrong_backend_is_still_rejected() {
        use super::require_reusable_wsl_backend_with;
        let (addr, server) = serve_health(
            identity("host", "0.0.1", None),
            Duration::ZERO,
            Some("desktop-secret"),
        );
        let on_stale = |_: &str, _: Option<&str>, _: &str| panic!("wrong backend treated as stale");
        let err =
            require_reusable_wsl_backend_with(&addr, Some("desktop-secret"), &on_stale).unwrap_err();
        assert!(err.contains("expected \"wsl\""), "{err}");
        server.join().unwrap();
    }

    #[test]
    fn usable_identity_covers_current_and_older_only() {
        use super::wsl_identity_usable;
        assert!(wsl_identity_usable(&Ok(())));
        assert!(wsl_identity_usable(&Err(
            "stale WSL daemon at 127.0.0.1:8787: version 0.0.1201 is older than bundled 0.0.1202".into()
        )));
        assert!(!wsl_identity_usable(&Err("health returned HTTP 401".into())));
        assert!(!wsl_identity_usable(&Err(
            "daemon at 127.0.0.1:8787 is backend \"host\", expected \"wsl\"".into()
        )));
    }

    #[test]
    fn recovery_watch_reports_wsl_coming_back() {
        use super::wait_for_wsl_recovery;
        let mut probes = 0;
        let mut paused = Duration::ZERO;
        let came_back = wait_for_wsl_recovery(
            Duration::from_secs(600),
            Duration::from_secs(20),
            || {
                probes += 1;
                probes == 3
            },
            |d| paused += d,
        );
        assert!(came_back);
        assert_eq!(probes, 3);
        assert_eq!(paused, Duration::from_secs(60), "waits before every probe");
    }

    #[test]
    fn recovery_watch_gives_up_after_its_window() {
        use super::wait_for_wsl_recovery;
        let mut probes = 0;
        let came_back = wait_for_wsl_recovery(
            Duration::from_secs(60),
            Duration::from_secs(20),
            || {
                probes += 1;
                false
            },
            |_| {},
        );
        assert!(!came_back);
        assert_eq!(probes, 3);
    }

    #[test]
    fn recovery_watch_is_bounded_in_production() {
        use super::{WSL_RECOVERY_POLL, WSL_RECOVERY_WATCH};
        assert!(WSL_RECOVERY_POLL >= Duration::from_secs(10), "too chatty");
        assert!(WSL_RECOVERY_WATCH <= Duration::from_secs(30 * 60), "watches forever");
    }
}

#[cfg(test)]
mod wsl_route_tests {
    use super::*;

    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    #[test]
    fn vm_ip_route_wins_even_when_loopback_also_answers() {
        let answered = s(&["172.22.149.245:8787", "127.0.0.1:8787"]);
        assert_eq!(choose_wsl_route(&answered, true, None).as_deref(), Some("172.22.149.245:8787"));
        // Order of answers must not matter.
        let answered = s(&["[::1]:8787", "172.22.149.245:8787"]);
        assert_eq!(choose_wsl_route(&answered, true, None).as_deref(), Some("172.22.149.245:8787"));
    }

    #[test]
    fn loopback_alone_waits_for_the_vm_ip_during_the_grace() {
        let answered = s(&["127.0.0.1:8787"]);
        assert_eq!(choose_wsl_route(&answered, true, None), None);
        assert_eq!(choose_wsl_route(&answered, true, Some(Duration::from_secs(3))), None);
        assert_eq!(
            choose_wsl_route(&answered, true, Some(WSL_VM_ROUTE_GRACE)).as_deref(),
            Some("127.0.0.1:8787"),
            "after the grace, loopback beats having no route at all"
        );
    }

    #[test]
    fn loopback_is_used_at_once_when_there_is_no_vm_ip() {
        let answered = s(&["[::1]:8787"]);
        assert_eq!(choose_wsl_route(&answered, false, None).as_deref(), Some("[::1]:8787"));
    }

    #[test]
    fn nothing_answered_keeps_probing() {
        assert_eq!(choose_wsl_route(&[], true, Some(Duration::from_secs(60))), None);
        assert_eq!(choose_wsl_route(&[], false, None), None);
    }

    #[test]
    fn rehome_only_moves_a_loopback_proxy_to_a_known_vm_ip() {
        assert_eq!(wsl_rehome_target("127.0.0.1:8787", Some("172.22.149.245:8787")), Some("172.22.149.245:8787"));
        assert_eq!(wsl_rehome_target("[::1]:8787", Some("172.22.149.245:8787")), Some("172.22.149.245:8787"));
        assert_eq!(wsl_rehome_target("172.22.149.245:8787", Some("172.22.149.245:8787")), None);
        assert_eq!(wsl_rehome_target("127.0.0.1:8787", None), None);
        assert_eq!(wsl_rehome_target("127.0.0.1:8787", Some("[::1]:8787")), None, "never re-home onto loopback");
    }
}

#[cfg(test)]
mod forwarding_reservation_tests {
    use super::{
        classify_port_occupant, wait_out_forwarding_reservation, PortOccupant, ReservationWait,
    };
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::thread;
    use std::time::{Duration, Instant};

    const BUDGET: Duration = Duration::from_millis(300);

    // A port that was never bound in this run. A port freed a moment ago is
    // not good enough: under WSL Consomme networking it stays a forwarding
    // reservation for a while (connect, then reset), which is exactly what
    // the classifier exists to recognise.
    fn never_bound_refused_addr() -> Option<String> {
        let seed = std::process::id() as u64 ^ Instant::now().elapsed().as_nanos() as u64;
        for i in 0..64u64 {
            let port = 49152 + ((seed.wrapping_mul(2654435761).wrapping_add(i * 7919)) % 16000) as u16;
            let addr = format!("127.0.0.1:{port}");
            let sa: std::net::SocketAddr = addr.parse().unwrap();
            match std::net::TcpStream::connect_timeout(&sa, Duration::from_millis(200)) {
                Err(e) if e.kind() == std::io::ErrorKind::ConnectionRefused => return Some(addr),
                _ => continue,
            }
        }
        None
    }

    #[test]
    fn a_refused_port_is_free() {
        let Some(addr) = never_bound_refused_addr() else {
            eprintln!("no refused loopback port found; skipping");
            return;
        };
        assert_eq!(classify_port_occupant(&addr, BUDGET), PortOccupant::Free);
    }

    #[test]
    fn a_listener_that_replies_answers() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut buf = [0u8; 256];
            let _ = stream.read(&mut buf);
            let _ = stream.write_all(b"HTTP/1.0 401 Unauthorized\r\n\r\n");
        });
        assert_eq!(classify_port_occupant(&addr, BUDGET), PortOccupant::Answers);
        server.join().unwrap();
    }

    // The Consomme symptom: the connect succeeds, then the connection is
    // closed or reset without a single byte of reply.
    #[test]
    fn accept_then_close_without_reply_is_a_reservation() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let server = thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            drop(stream);
        });
        assert_eq!(classify_port_occupant(&addr, BUDGET), PortOccupant::Reservation);
        server.join().unwrap();
    }

    #[test]
    fn a_listener_that_never_replies_is_silent_not_free() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let server = thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            thread::sleep(Duration::from_millis(600));
            drop(stream);
        });
        assert_eq!(classify_port_occupant(&addr, BUDGET), PortOccupant::Silent);
        server.join().unwrap();
    }

    #[test]
    fn a_reservation_that_clears_frees_the_port() {
        let mut looks = vec![
            PortOccupant::Reservation,
            PortOccupant::Reservation,
            PortOccupant::Free,
        ]
        .into_iter();
        let mut pauses = 0;
        let outcome = wait_out_forwarding_reservation(
            || looks.next().unwrap(),
            |_| pauses += 1,
            Instant::now() + Duration::from_secs(60),
        );
        assert_eq!(outcome, ReservationWait::Freed);
        assert_eq!(pauses, 2);
    }

    #[test]
    fn a_real_listener_is_never_waited_on() {
        let mut pauses = 0;
        let outcome = wait_out_forwarding_reservation(
            || PortOccupant::Answers,
            |_| pauses += 1,
            Instant::now() + Duration::from_secs(60),
        );
        assert_eq!(outcome, ReservationWait::Answers);
        assert_eq!(pauses, 0, "a foreign listener must fail fast, as before");
    }

    #[test]
    fn a_reservation_that_persists_gives_up_at_the_deadline() {
        let outcome = wait_out_forwarding_reservation(
            || PortOccupant::Reservation,
            |_| {},
            Instant::now(),
        );
        assert_eq!(outcome, ReservationWait::Persisting);
    }

    #[test]
    fn a_reservation_that_becomes_a_listener_is_judged_by_identity() {
        let mut looks = vec![PortOccupant::Silent, PortOccupant::Answers].into_iter();
        let outcome = wait_out_forwarding_reservation(
            || looks.next().unwrap(),
            |_| {},
            Instant::now() + Duration::from_secs(60),
        );
        assert_eq!(outcome, ReservationWait::Answers);
    }
}

/// The 2026-10-28 Surface case: an old solud with no backend id, in a second
/// WSL distro, held :8787 (WSL2 distros share one network namespace), so the
/// daemon start-wsl.sh launched in the default distro could never bind and
/// every launch waited the full 120s with nothing on screen. start-wsl.sh now
/// classifies the occupant, reports it, and the shell stops waiting.
#[cfg(test)]
mod wsl_port_block_tests {
    use super::*;

    fn body() -> String {
        wsl_start_script_body("/mnt/c/a/solud-linux-amd64.gz", "/mnt/c/b/solud-linux-arm64.gz")
    }

    fn extract_fn(body: &str, name: &str) -> String {
        let head = format!("{name}() {{\n");
        let start = body
            .find(&head)
            .unwrap_or_else(|| panic!("{name} is not defined in start-wsl.sh"));
        let rest = &body[start..];
        let end = rest
            .find("\n}\n")
            .unwrap_or_else(|| panic!("{name} has no closing brace"));
        rest[..end + 3].to_string()
    }

    #[cfg(unix)]
    fn scratch_dir(tag: &str) -> Option<std::path::PathBuf> {
        use std::time::{SystemTime, UNIX_EPOCH};
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let dir = std::env::temp_dir().join(format!("solu-{tag}-{}-{nanos}", std::process::id()));
        std::fs::create_dir_all(&dir).ok().map(|_| dir)
    }

    #[cfg(unix)]
    fn run_bash(script: &str) -> Option<std::process::Output> {
        std::process::Command::new("bash").arg("-c").arg(script).output().ok()
    }

    /// The launcher functions the classification uses, with the probe either
    /// real (talking to `port`) or replaced by `stub`.
    #[cfg(unix)]
    fn classifier(dir: &std::path::Path, port: u16, bundle: &str, stub: Option<&str>) -> String {
        let b = body();
        let mut fns = vec![
            extract_fn(&b, "ver_gt"),
            extract_fn(&b, "rollback_hold_active"),
            extract_fn(&b, "live_daemon_is_current"),
            extract_fn(&b, "port_held_by_other_daemon"),
            extract_fn(&b, "live_daemon_visible_here"),
            extract_fn(&b, "status_value"),
            extract_fn(&b, "write_start_status"),
            extract_fn(&b, "report_port_blocked"),
        ];
        match stub {
            Some(stub) => fns.push(stub.to_string()),
            None => {
                fns.push(extract_fn(&b, "json_top_value"));
                fns.push(extract_fn(&b, "probe_live_daemon"));
            }
        }
        format!(
            "set -e\nHOME='{home}'\nADDR='0.0.0.0:{port}'\nBUNDLE_VERSION='{bundle}'\n\
             START_STATUS='{home}/{status}'\nWSL_DISTRO_NAME='Ubuntu'\n\
             LIVE_PID=''\nLIVE_BACKEND=''\nLIVE_VERSION=''\nLIVE_BINARY=''\n{fns}\n",
            home = dir.display(),
            status = WSL_START_STATUS_FILE,
            fns = fns.join("\n"),
        )
    }

    /// Serve one canned /api/health to every connection until dropped.
    #[cfg(unix)]
    fn serve_health(json: &'static str) -> u16 {
        use std::io::{BufRead, BufReader};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        thread::spawn(move || {
            for stream in listener.incoming().flatten() {
                let mut stream = stream;
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                while reader.read_line(&mut line).map(|n| n > 0).unwrap_or(false) {
                    if line == "\r\n" {
                        break;
                    }
                    line.clear();
                }
                let _ = write!(
                    stream,
                    "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{json}",
                    json.len()
                );
            }
        });
        port
    }

    /// Probe classification, through the launcher's real probe: an older
    /// daemon without a backend id answering on the port is a held port, is
    /// reported (not replaced), and the report parses into the fast path.
    #[cfg(unix)]
    #[test]
    fn old_daemon_without_backend_id_is_reported_as_holding_the_port() {
        let port = serve_health(
            r#"{"status":"ok","version":"0.0.1189","pid":999999,"backendID":"","binaryPath":"/home/benja/.local/bin/solud","fleet":{"components":[{"pid":1,"version":"9.9.9"}]}}"#,
        );
        let Some(dir) = scratch_dir("port-held") else { return };
        let script = format!(
            "{}\nif port_held_by_other_daemon; then report_port_blocked port-held probe; echo BLOCKED; else echo FREE; fi\n",
            classifier(&dir, port, "0.0.1228", None)
        );
        let Some(out) = run_bash(&script) else { return };
        let stdout = String::from_utf8_lossy(&out.stdout);
        assert!(
            stdout.contains("BLOCKED"),
            "an old daemon with no backend id must count as holding the port: {stdout} {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert!(stdout.contains("backend none version 0.0.1189 pid 999999"), "{stdout}");
        assert!(stdout.contains("most likely in another WSL distro"), "{stdout}");
        let report = std::fs::read_to_string(dir.join(WSL_START_STATUS_FILE)).expect("report written");
        let _ = std::fs::remove_dir_all(&dir);
        let block = parse_wsl_start_status(&report).expect("a port-held report ends the wait");
        assert_eq!(
            block,
            WslPortBlock {
                identified: true,
                reason: "probe".into(),
                addr: format!("0.0.0.0:{port}"),
                backend: String::new(),
                version: "0.0.1189".into(),
                pid: Some(999999),
                binary: "/home/benja/.local/bin/solud".into(),
                visible: false,
                cgroup: String::new(),
                distro: "Ubuntu".into(),
            }
        );
    }

    /// Never act on a healthy daemon of the right backend: a current WSL
    /// daemon on the port is not a held port, and nothing is reported.
    #[cfg(unix)]
    #[test]
    fn healthy_current_wsl_daemon_is_left_alone() {
        let port = serve_health(
            r#"{"status":"ok","version":"0.0.1228","pid":4242,"backendID":"wsl","binaryPath":"/home/u/.local/share/soluterminal/solud"}"#,
        );
        let Some(dir) = scratch_dir("port-current") else { return };
        let script = format!(
            "{}\nif port_held_by_other_daemon; then echo BLOCKED; else echo FREE; fi\n",
            classifier(&dir, port, "0.0.1228", None)
        );
        let Some(out) = run_bash(&script) else { return };
        let stdout = String::from_utf8_lossy(&out.stdout);
        let reported = dir.join(WSL_START_STATUS_FILE).exists();
        let _ = std::fs::remove_dir_all(&dir);
        assert!(stdout.contains("FREE"), "a current WSL daemon was treated as a blocker: {stdout}");
        assert!(!reported, "nothing may be reported for a healthy daemon");
    }

    /// The same decision across the identities a probe can return.
    #[cfg(unix)]
    #[test]
    fn classification_blocks_only_foreign_or_older_daemons() {
        let cases = [
            ("", "0.0.1189", true),     // installer unit / hand-started: no backend id
            ("win", "0.0.1228", true),  // a different backend
            ("wsl", "0.0.1189", true),  // older, no rollback hold
            ("wsl", "0.0.1228", false), // current
            ("wsl", "0.0.1300", false), // newer
            ("wsl", "", false),         // version unknown: keep it
        ];
        for (backend, version, blocked) in cases {
            let Some(dir) = scratch_dir("port-cases") else { return };
            let stub = format!(
                "probe_live_daemon() {{\n  LIVE_PID=77\n  LIVE_BACKEND='{backend}'\n  LIVE_VERSION='{version}'\n  LIVE_BINARY=''\n  return 0\n}}\n"
            );
            let script = format!(
                "{}\nif port_held_by_other_daemon; then echo BLOCKED; else echo FREE; fi\n",
                classifier(&dir, 8787, "0.0.1228", Some(&stub))
            );
            let Some(out) = run_bash(&script) else { return };
            let _ = std::fs::remove_dir_all(&dir);
            let stdout = String::from_utf8_lossy(&out.stdout);
            assert_eq!(
                stdout.contains("BLOCKED"),
                blocked,
                "backend={backend:?} version={version:?}: {stdout}"
            );
        }
    }

    /// The daemon's own bind-failure line (describeListenConflict) is found
    /// only in what it logged since the last launch.
    #[cfg(unix)]
    #[test]
    fn bind_failure_is_read_from_the_current_launch_only() {
        let Some(dir) = scratch_dir("bind-fail") else { return };
        let log = dir.join("daemon.log");
        let old = "2026/10/28 09:00:00 listen: cannot bind 0.0.0.0:8787 — old run\n";
        let new = "2026/10/29 09:00:00 listen: port 0.0.0.0:8787 is held by another soluterminal daemon (backend=unknown version=0.0.1189 pid=600) — stop it or switch backend in the tray\n";
        std::fs::write(&log, old).unwrap();
        let func = extract_fn(&body(), "daemon_bind_failed");
        let script = format!(
            "DAEMON_DIR='{}'\nLAUNCH_LOG_MARK={}\n{func}\n\
             if daemon_bind_failed; then echo \"FOUND $BIND_FAILURE\"; else echo NONE; fi\n\
             printf '%s' '{new}' >> \"$DAEMON_DIR/daemon.log\"\n\
             if daemon_bind_failed; then echo \"FOUND $BIND_FAILURE\"; else echo NONE; fi\n",
            dir.display(),
            old.len(),
        );
        let Some(out) = run_bash(&script) else { return };
        let _ = std::fs::remove_dir_all(&dir);
        let stdout = String::from_utf8_lossy(&out.stdout);
        let mut lines = stdout.lines();
        assert_eq!(lines.next(), Some("NONE"), "an earlier run's line counted: {stdout}");
        let found = lines.next().unwrap_or("");
        assert!(
            found.starts_with("FOUND ") && found.contains("backend=unknown version=0.0.1189 pid=600"),
            "{stdout}"
        );
    }

    /// Where the checks sit in the launcher: after every sweep we may run and
    /// before the first launch, and in the relaunch loop before the restart
    /// budget is spent. Neither path stops anything.
    #[test]
    fn launcher_reports_a_held_port_instead_of_burning_restarts() {
        let b = body();
        let fresh = b
            .find("  stop_port_holder\n  # Whatever still answers here")
            .expect("fresh-start check missing");
        let check = b[fresh..].find("  if port_held_by_other_daemon; then\n").map(|i| fresh + i).unwrap();
        let launch = b[fresh..].find("  launch_daemon\nfi\n").map(|i| fresh + i).unwrap();
        assert!(check < launch, "the held-port check must precede the first launch");
        let relaunch = b.find("  if [ \"$status\" != \"0\" ] && daemon_bind_failed; then\n").expect("relaunch check");
        let cap = b.find("  if [ \"$restarts\" -ge 10 ]; then\n").unwrap();
        assert!(relaunch < cap, "a held port must be handled before the restart cap");
        for name in ["port_held_by_other_daemon", "report_port_blocked", "await_port_release", "write_start_status"] {
            let f = extract_fn(&b, name);
            assert!(!f.contains("kill ") && !f.contains("stop_pid"), "{name} must never stop a process: {f}");
        }
        // The report lands next to the script, where the Windows shell reads it.
        assert!(b.contains(&format!("START_STATUS=\"${{SUPERVISOR_SCRIPT%/*}}/{WSL_START_STATUS_FILE}\"")));
    }

    /// `wsl.exe -e` often has no systemd user session; the launcher must not
    /// depend on one.
    #[test]
    fn systemd_user_calls_need_a_user_bus() {
        let f = extract_fn(&body(), "retire_conflicting_systemd_unit");
        let call = f.lines().find(|l| l.contains("systemctl --user disable")).expect("systemctl call");
        assert!(call.contains("timeout 10 systemctl"), "{call}");
        assert!(f.contains("[ -S \"$bus_dir/bus\" ]"), "systemctl --user must be gated on a user bus: {f}");
    }

    #[test]
    fn report_parsing_ignores_other_states_and_untrusted_noise() {
        assert_eq!(parse_wsl_start_status(""), None);
        assert_eq!(parse_wsl_start_status("state=starting\nreason=port released\n"), None);
        assert_eq!(parse_wsl_start_status("garbage"), None);
        let block = parse_wsl_start_status(
            "state=bind-failed\r\nreason=listen: cannot bind\u{7}\r\npid=12x\r\ndistro=Ubuntu; rm -rf /\r\nversion=0.0.1189\"\r\n",
        )
        .unwrap();
        assert!(!block.identified);
        assert_eq!(block.reason, "listen: cannot bind");
        assert_eq!(block.pid, None);
        assert_eq!(block.distro, "", "a distro name that is not plain is dropped");
        assert_eq!(block.version, "");
    }

    #[test]
    fn holder_is_the_distro_where_that_pid_is_solud() {
        let block = parse_wsl_start_status(
            "state=port-held\npid=600\nversion=0.0.1189\nbinary=/home/benja/.local/bin/solud\ndistro=Ubuntu\nvisible=0\n",
        )
        .unwrap();
        let running: Vec<String> = ["Ubuntu", "docker-desktop", "Debian", "Ubuntu-24.04"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        let mut asked = Vec::new();
        let holder = find_wsl_port_holder(&block, &running, |distro, pid| {
            asked.push(distro.to_string());
            assert_eq!(pid, 600);
            match distro {
                // pid 600 exists here too, but it is not the daemon
                "docker-desktop" => Some((false, "/usr/bin/containerd".into(), "containerd ".into(), String::new())),
                // pid 600 is even a solud here, but it does not own the port
                "Debian" => Some((
                    false,
                    "/home/benja/.local/bin/solud".into(),
                    "/home/benja/.local/bin/solud --addr 127.0.0.1:9999 ".into(),
                    "0::/system.slice/other.service".into(),
                )),
                "Ubuntu-24.04" => Some((
                    true,
                    "/home/benja/.local/bin/solud".into(),
                    "/home/benja/.local/bin/solud --addr 127.0.0.1:8787 ".into(),
                    "0::/user.slice/user-1000.slice/user@1000.service/app.slice/soluterminal.service".into(),
                )),
                _ => None,
            }
        });
        assert_eq!(asked, vec!["docker-desktop", "Debian", "Ubuntu-24.04"], "the default distro already looked");
        let holder = holder.expect("found");
        assert_eq!(holder.distro, "Ubuntu-24.04");
        let msg = describe_wsl_port_block(&block, Some(&holder), "0.0.1228");
        assert!(msg.starts_with("WSL port held: port 8787 is in use by an older SoluTerminal service (version 0.0.1189, no backend id, pid 600) in the WSL distro \"Ubuntu-24.04\", so the WSL background service in \"Ubuntu\" cannot start."), "{msg}");
        assert!(msg.contains("WSL distros share one network"), "{msg}");
        assert!(msg.ends_with("To stop it, run in PowerShell: wsl -d Ubuntu-24.04 -- systemctl --user disable --now soluterminal.service"), "{msg}");
    }

    /// The ownership check itself, against a real listener: this test process
    /// owns the socket it bound, and no other pid passes for it.
    #[cfg(target_os = "linux")]
    #[test]
    fn only_the_socket_owner_passes_the_listen_check() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port().to_string();
        let me = std::process::id().to_string();
        let run = |pid: &str, port: &str| {
            std::process::Command::new("sh")
                .args(["-c", &wsl_holder_inspect_script(), "inspect", pid, port])
                .output()
                .ok()
                .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
        };
        let Some(out) = run(&me, &port) else { return };
        assert!(out.lines().any(|l| l == "listen=1"), "the owner must pass: {out}");
        assert!(out.lines().any(|l| l.starts_with("exe=/")), "{out}");
        let Some(init) = run("1", &port) else { return };
        assert!(init.lines().any(|l| l == "listen=0"), "a pid that does not own the socket passed: {init}");
        drop(listener);
        let Some(closed) = run(&me, &port) else { return };
        assert!(closed.lines().any(|l| l == "listen=0"), "a closed port passed: {closed}");
    }

    #[test]
    fn stop_command_matches_how_the_daemon_runs() {
        assert_eq!(
            wsl_stop_command("Ubuntu-24.04", Some(600), "0::/init.scope").as_deref(),
            Some("wsl -d Ubuntu-24.04 -- kill 600")
        );
        assert_eq!(
            wsl_stop_command("Ubuntu-24.04", Some(600), "0::/system.slice/solud.service").as_deref(),
            Some("wsl --terminate Ubuntu-24.04")
        );
        assert_eq!(wsl_stop_command("Ubuntu 24.04", Some(600), ""), None, "never quote untrusted names");
        assert_eq!(wsl_stop_command("Ubuntu", None, ""), None);
        let block = parse_wsl_start_status("state=port-held\npid=600\nversion=0.0.1189\ndistro=Ubuntu\n").unwrap();
        let msg = describe_wsl_port_block(&block, None, "0.0.1228");
        assert!(msg.contains("outside this WSL distro"), "{msg}");
        assert!(msg.contains("Run `wsl -l -v`"), "{msg}");
    }

    fn block() -> WslPortBlock {
        parse_wsl_start_status("state=port-held\npid=600\nversion=0.0.1189\ndistro=Ubuntu\n").unwrap()
    }

    fn vm_and_loopback() -> Vec<String> {
        vec!["192.168.255.176:8787".into(), "127.0.0.1:8787".into(), "[::1]:8787".into()]
    }

    /// The wait exits on start-wsl.sh's report even while every probe is
    /// stuck in a slow timeout (each took up to 10s on the Surface).
    #[test]
    fn wait_ends_within_a_second_of_a_port_report() {
        let calls = Arc::new(Mutex::new(0u32));
        let seen = calls.clone();
        let started = Instant::now();
        let (outcome, _) = wait_for_spawned_wsl_daemon(
            vm_and_loopback(),
            true,
            Duration::from_secs(60),
            Duration::from_secs(10),
            |_addr: &str| {
                thread::sleep(Duration::from_secs(3));
                Err("request daemon health failed or timed out".to_string())
            },
            || None,
            move || {
                let mut n = seen.lock().unwrap();
                *n += 1;
                (*n >= 2).then(block)
            },
            |_: &str| {},
        );
        assert!(matches!(outcome, SpawnedWslWait::Blocked(ref b) if b.pid == Some(600)), "{outcome:?}");
        assert!(started.elapsed() < Duration::from_secs(2), "took {:?}", started.elapsed());
        assert!(*calls.lock().unwrap() >= 2);
    }

    /// A usable daemon that answers wins: a report never overrides it.
    #[test]
    fn answering_daemon_wins_over_a_report() {
        let started = Instant::now();
        let (outcome, candidates) = wait_for_spawned_wsl_daemon(
            vm_and_loopback(),
            true,
            Duration::from_secs(60),
            Duration::from_secs(10),
            |addr: &str| {
                if addr.starts_with("192.168.") {
                    Ok(())
                } else {
                    Err("x".into())
                }
            },
            || None,
            move || (started.elapsed() > Duration::from_secs(1)).then(block),
            |_: &str| {},
        );
        assert!(matches!(outcome, SpawnedWslWait::Reachable(ref r) if r == "192.168.255.176:8787"), "{outcome:?}");
        assert_eq!(candidates[0], "192.168.255.176:8787");
    }

    /// Without a report the wait is unchanged: it gives up at the deadline
    /// with the wording classify_wsl_fallback keys on.
    #[test]
    fn without_a_report_the_wait_runs_to_its_deadline() {
        let started = Instant::now();
        let (outcome, candidates) = wait_for_spawned_wsl_daemon(
            vec!["127.0.0.1:8787".into()],
            false,
            Duration::from_millis(700),
            Duration::from_secs(10),
            |_: &str| Err("daemon at 127.0.0.1:8787 is backend \"\", expected \"wsl\"".to_string()),
            || Some("172.20.0.2".to_string()),
            || None,
            |_: &str| {},
        );
        assert!(started.elapsed() >= Duration::from_millis(700));
        match outcome {
            SpawnedWslWait::GaveUp(msg) => {
                assert!(msg.starts_with("WSL daemon was not reachable within"), "{msg}");
                assert!(msg.contains("expected \\\"wsl\\\"") || msg.contains("expected \"wsl\""), "{msg}");
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(candidates[0], "172.20.0.2:8787", "a VM IP resolved during the wait is kept");
    }
}
