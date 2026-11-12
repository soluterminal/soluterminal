//! Where Claude Code and Codex are installed on a Windows PC: on Windows, in
//! WSL, or both. The first launch uses this to pick where to run instead of
//! asking (backend.rs).
//!
//! Two rules shape it. It is bounded: the Windows side only looks at files,
//! and every wsl.exe call has a short deadline. And it never starts WSL: a
//! stopped distro is reported as asleep and left alone, because waking the
//! WSL VM takes up to a minute and gigabytes of memory, for a person who may
//! never use it.
//!
//! The places searched mirror the daemon's own lookup (daemon/provider_bin.go,
//! shared/providers, daemon/claude_bootstrap.go `loginShellInit`), so the
//! shell and the setup checklist agree about what is installed.

// Only the Windows shell calls this; other hosts compile it for the tests.
#![cfg_attr(not(windows), allow(dead_code))]

use std::path::{Path, PathBuf};

/// Which agents were found on one side.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
pub struct Agents {
    pub claude: bool,
    pub codex: bool,
}

impl Agents {
    pub fn count(self) -> u8 {
        u8::from(self.claude) + u8::from(self.codex)
    }

    pub fn any(self) -> bool {
        self.count() > 0
    }
}

/// What could be learned about agents inside WSL without starting it.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum WslAgents {
    /// No WSL, or no distro to run anything in.
    NoDistro,
    /// The default distro exists but is not running. Not looked into.
    Asleep,
    /// The default distro is running and answered.
    Running(Agents),
    /// Couldn't tell: a listing or the lookup failed or timed out, or low
    /// disk space blocks WSL. Nothing may be concluded, or saved, from this.
    Unknown,
}

/// The folders and extensions a Windows lookup uses, taken from the
/// environment in the app and built by hand in tests.
#[derive(Clone, Debug, Default)]
pub struct WindowsPlaces {
    /// `%PATH%`, in order.
    pub path: Vec<PathBuf>,
    /// `%PATHEXT%` entries, each with its leading dot.
    pub pathext: Vec<String>,
    /// `%USERPROFILE%`.
    pub home: Option<PathBuf>,
    /// `%LOCALAPPDATA%`.
    pub local_app_data: Option<PathBuf>,
}

/// What Windows uses when `%PATHEXT%` is unset (daemon/winpath_windows.go).
const DEFAULT_PATHEXT: &str = ".COM;.EXE;.BAT;.CMD";

impl WindowsPlaces {
    pub fn from_env() -> Self {
        let pathext = std::env::var("PATHEXT")
            .ok()
            .filter(|value| !value.trim().is_empty())
            .unwrap_or_else(|| DEFAULT_PATHEXT.to_string());
        let home = std::env::var_os("USERPROFILE").map(PathBuf::from);
        let local_app_data = std::env::var_os("LOCALAPPDATA")
            .map(PathBuf::from)
            .or_else(|| home.as_ref().map(|home| home.join("AppData").join("Local")));
        Self {
            path: std::env::var_os("PATH")
                .map(|path| std::env::split_paths(&path).collect())
                .unwrap_or_default(),
            pathext: pathext
                .split(';')
                .map(str::trim)
                .filter(|ext| ext.starts_with('.'))
                .map(str::to_string)
                .collect(),
            home,
            local_app_data,
        }
    }
}

/// Folders under the home folder where the installers and version managers
/// put an agent (shared/providers/claude.go, codex.go).
const HOME_DIRS: [&str; 3] = [".local/bin", ".asdf/shims", ".volta/bin"];
/// (parent, child) pairs: `<home>/<parent>/<any version>/<child>/<tool>`.
const HOME_VERSIONED_DIRS: [(&str, &str); 2] =
    [(".nvm/versions/node", "bin"), (".asdf/installs/nodejs", "bin")];
/// Where OpenAI's install.ps1 puts Codex, under `%LOCALAPPDATA%`.
const CODEX_LOCAL_APP_DIR: &str = "Programs/OpenAI/Codex/bin";

/// Agents installed on the Windows side. Looks at files only; starts nothing.
pub fn windows_agents(places: &WindowsPlaces) -> Agents {
    Agents {
        claude: tool_on_windows(places, "claude", None),
        codex: tool_on_windows(places, "codex", Some(CODEX_LOCAL_APP_DIR)),
    }
}

fn tool_on_windows(places: &WindowsPlaces, tool: &str, local_app_dir: Option<&str>) -> bool {
    // On PATH only a name with a PATHEXT extension counts. npm also drops an
    // extensionless `claude` next to `claude.cmd`; that one is a shell script
    // for Git Bash and Windows cannot run it.
    let on_path = places.path.iter().any(|dir| {
        places
            .pathext
            .iter()
            .any(|ext| is_file(&dir.join(format!("{tool}{}", ext.to_ascii_lowercase()))))
    });
    if on_path {
        return true;
    }
    let names = [format!("{tool}.exe"), format!("{tool}.cmd"), tool.to_string()];
    let in_dir = |dir: &Path| names.iter().any(|name| is_file(&dir.join(name)));
    if let Some(home) = &places.home {
        if HOME_DIRS.iter().any(|dir| in_dir(&home.join(dir))) {
            return true;
        }
        for (parent, child) in HOME_VERSIONED_DIRS {
            let Ok(versions) = std::fs::read_dir(home.join(parent)) else {
                continue;
            };
            if versions
                .flatten()
                .any(|version| is_file(&version.path().join(child).join(tool)))
            {
                return true;
            }
        }
    }
    match (&places.local_app_data, local_app_dir) {
        (Some(local), Some(dir)) => in_dir(&local.join(dir)),
        _ => false,
    }
}

fn is_file(path: &Path) -> bool {
    path.is_file()
}

/// The default distro, judged from the two listings that never start one.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
enum DefaultDistro {
    Missing,
    Stopped,
    Running,
}

/// `verbose` is `wsl --list --verbose` and `running` is
/// `wsl --list --running --quiet`, both decoded. The default distro is the
/// row marked `*`; column headings and state names are localized, so nothing
/// else in the verbose listing is read here.
fn default_distro(verbose: &str, running: &str) -> DefaultDistro {
    let has_default = verbose
        .lines()
        .any(|line| line.trim_start().starts_with("* "));
    if !has_default {
        DefaultDistro::Missing
    } else if crate::daemon::default_wsl_is_running(verbose, running) {
        DefaultDistro::Running
    } else {
        DefaultDistro::Stopped
    }
}

/// Where WSL mounts the Windows drives.
const WINDOWS_MOUNT: &str = "/mnt/";

/// Run inside the default distro with `bash -l -s`, fed on stdin so nothing
/// has to survive wsl.exe's argument quoting. The PATH setup is the daemon's
/// `loginShellInit`. Anything under `windows_mount` is a Windows install seen
/// through WSL's shared PATH, not one that can run in WSL, so every match is
/// checked and only a Linux path counts. Matches are compared one whole line
/// at a time: Windows paths have spaces ("/mnt/c/Program Files/nodejs/codex"),
/// and splitting one on them would leave a piece that no longer starts with
/// the mount. The last line proves the script finished: without it, "absent"
/// could just mean the shell died early.
fn wsl_lookup_script(windows_mount: &str) -> String {
    WSL_LOOKUP_SCRIPT.replace("{{WINDOWS_MOUNT}}", windows_mount)
}

const WSL_LOOKUP_SCRIPT: &str = r#"export PATH="$HOME/.local/bin:$HOME/.volta/bin:$PATH"
[ -s "$HOME/.nvm/nvm.sh" ] && . "$HOME/.nvm/nvm.sh" >/dev/null 2>&1
[ -s "$HOME/.asdf/asdf.sh" ] && . "$HOME/.asdf/asdf.sh" >/dev/null 2>&1
for tool in claude codex; do
  verdict=absent
  while IFS= read -r found; do
    case "$found" in '{{WINDOWS_MOUNT}}'*) ;; /*) verdict=found ;; esac
  done < <(type -aP "$tool" 2>/dev/null)
  echo "solu-agent $tool $verdict"
done
echo "solu-agent done"
"#;

/// What a failed `wsl --list --verbose` means. Only WSL saying outright that
/// it has no distro counts as none. Anything else (a WSL service error, WSL
/// not installed, a message in another language) is unknown: nothing about
/// the PC's WSL may be concluded from a failure nobody recognised.
fn failed_listing(output: &str) -> WslAgents {
    let output = output.to_lowercase();
    if output.contains("no installed distribution") || output.contains("has no installed") {
        WslAgents::NoDistro
    } else {
        WslAgents::Unknown
    }
}

/// The lookup's answer, or `None` when it did not run to its last line.
fn parse_wsl_lookup(output: &str) -> Option<Agents> {
    let mut agents = Agents::default();
    let mut done = false;
    for line in output.lines() {
        match line.trim() {
            "solu-agent claude found" => agents.claude = true,
            "solu-agent codex found" => agents.codex = true,
            "solu-agent done" => done = true,
            _ => {}
        }
    }
    done.then_some(agents)
}

/// Agents installed in WSL's default distro, asked only if it is already
/// running. `wsl --list` never starts a distro; the lookup itself runs only
/// after the running-list named the default one.
#[cfg(windows)]
pub fn wsl_agents() -> WslAgents {
    use crate::daemon::{decode_wsl_listing, launch_log, run_wsl_bounded, run_wsl_bounded_with_stdin};
    use std::time::Duration;

    const LIST_TIMEOUT: Duration = Duration::from_secs(5);
    const LOOKUP_TIMEOUT: Duration = Duration::from_secs(10);

    // Low disk blocks WSL starts, and the lists say nothing reliable then.
    if !crate::daemon::wsl_start_allowed() {
        return WslAgents::Unknown;
    }
    let verbose = match run_wsl_bounded(&["--list", "--verbose"], LIST_TIMEOUT) {
        Ok(out) if out.status.success() => decode_wsl_listing(&out.stdout),
        Ok(out) => {
            let mut said = decode_wsl_listing(&out.stdout);
            said.push('\n');
            said.push_str(&decode_wsl_listing(&out.stderr));
            let verdict = failed_listing(&said);
            if verdict == WslAgents::Unknown {
                launch_log("agent detection: the WSL distro list failed for a reason not recognised");
            }
            return verdict;
        }
        // No wsl.exe on this PC at all.
        Err(e) if e.starts_with("wsl.exe spawn") => return WslAgents::NoDistro,
        Err(e) => {
            launch_log(&format!("agent detection: {e}"));
            return WslAgents::Unknown;
        }
    };
    // With nothing running, WSL prints a notice and exits non-zero; that is
    // an empty list, not a failure.
    let running = match run_wsl_bounded(&["--list", "--running", "--quiet"], LIST_TIMEOUT) {
        Ok(out) if out.status.success() => decode_wsl_listing(&out.stdout),
        Ok(_) => String::new(),
        Err(e) => {
            launch_log(&format!("agent detection: {e}"));
            return WslAgents::Unknown;
        }
    };
    match default_distro(&verbose, &running) {
        DefaultDistro::Missing => WslAgents::NoDistro,
        DefaultDistro::Stopped => WslAgents::Asleep,
        DefaultDistro::Running => {
            let lookup = run_wsl_bounded_with_stdin(
                &["-e", "bash", "-l", "-s"],
                Some(wsl_lookup_script(WINDOWS_MOUNT).as_bytes()),
                LOOKUP_TIMEOUT,
            );
            match lookup {
                Ok(out) => match parse_wsl_lookup(&decode_wsl_listing(&out.stdout)) {
                    Some(agents) => WslAgents::Running(agents),
                    None => {
                        launch_log("agent detection: the WSL lookup did not finish");
                        WslAgents::Unknown
                    }
                },
                Err(e) => {
                    launch_log(&format!("agent detection: {e}"));
                    WslAgents::Unknown
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A scratch folder that removes itself.
    struct Scratch(PathBuf);

    impl Scratch {
        fn new(name: &str) -> Self {
            let dir = std::env::temp_dir().join(format!("solu-agent-detect-{name}-{}", std::process::id()));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            Scratch(dir)
        }

        fn file(&self, relative: &str) -> PathBuf {
            let path = self.0.join(relative);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(&path, "x").unwrap();
            path
        }
    }

    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    fn places(scratch: &Scratch) -> WindowsPlaces {
        WindowsPlaces {
            path: vec![scratch.0.join("npm"), scratch.0.join("missing")],
            pathext: DEFAULT_PATHEXT.split(';').map(str::to_string).collect(),
            home: Some(scratch.0.join("home")),
            local_app_data: Some(scratch.0.join("local")),
        }
    }

    #[test]
    fn nothing_installed_finds_nothing() {
        let scratch = Scratch::new("none");
        assert_eq!(windows_agents(&places(&scratch)), Agents::default());
        assert_eq!(windows_agents(&WindowsPlaces::default()), Agents::default());
        assert!(!Agents::default().any());
    }

    #[test]
    fn an_npm_install_on_path_counts_only_by_its_cmd_file() {
        let scratch = Scratch::new("npm");
        // npm's shell-script twin, which Windows cannot run.
        scratch.file("npm/claude");
        assert!(!windows_agents(&places(&scratch)).claude);
        scratch.file("npm/claude.cmd");
        scratch.file("npm/codex.cmd");
        let found = windows_agents(&places(&scratch));
        assert_eq!(found, Agents { claude: true, codex: true });
        assert_eq!(found.count(), 2);
    }

    #[test]
    fn the_native_installers_are_found_off_path() {
        let scratch = Scratch::new("native");
        scratch.file("home/.local/bin/claude.exe");
        assert_eq!(windows_agents(&places(&scratch)), Agents { claude: true, codex: false });
        scratch.file("local/Programs/OpenAI/Codex/bin/codex.exe");
        assert_eq!(windows_agents(&places(&scratch)), Agents { claude: true, codex: true });
    }

    #[test]
    fn version_manager_folders_are_searched() {
        let scratch = Scratch::new("managers");
        scratch.file("home/.nvm/versions/node/v22.4.0/bin/codex");
        scratch.file("home/.volta/bin/claude.cmd");
        assert_eq!(windows_agents(&places(&scratch)), Agents { claude: true, codex: true });
    }

    #[test]
    fn the_codex_folder_does_not_make_claude_installed() {
        let scratch = Scratch::new("codex-dir");
        scratch.file("local/Programs/OpenAI/Codex/bin/claude.exe");
        assert_eq!(windows_agents(&places(&scratch)), Agents::default());
    }

    const VERBOSE: &str = "  NAME              STATE           VERSION\r\n\
                           * Ubuntu            Running         2\r\n\
                           \x20 Ubuntu-24.04      Stopped         2\r\n";

    #[test]
    fn the_default_distro_is_missing_stopped_or_running() {
        assert_eq!(default_distro("", ""), DefaultDistro::Missing);
        assert_eq!(
            default_distro("Windows Subsystem for Linux has no installed distributions.\r\n", ""),
            DefaultDistro::Missing
        );
        assert_eq!(default_distro(VERBOSE, ""), DefaultDistro::Stopped);
        // Another distro running does not make the default one awake.
        assert_eq!(default_distro(VERBOSE, "Ubuntu-24.04\r\n"), DefaultDistro::Stopped);
        assert_eq!(default_distro(VERBOSE, "Ubuntu\r\nUbuntu-24.04\r\n"), DefaultDistro::Running);
    }

    #[test]
    fn the_wsl_lookup_needs_its_last_line() {
        assert_eq!(
            parse_wsl_lookup("motd noise\nsolu-agent claude found\nsolu-agent codex absent\nsolu-agent done\n"),
            Some(Agents { claude: true, codex: false })
        );
        assert_eq!(
            parse_wsl_lookup("solu-agent claude absent\r\nsolu-agent codex found\r\nsolu-agent done\r\n"),
            Some(Agents { claude: false, codex: true })
        );
        // Killed at the deadline: no verdict, not "nothing installed".
        assert_eq!(parse_wsl_lookup("solu-agent claude found\n"), None);
        assert_eq!(parse_wsl_lookup(""), None);
    }

    #[test]
    fn only_wsl_saying_it_has_no_distro_counts_as_none() {
        assert_eq!(
            failed_listing("Windows Subsystem for Linux has no installed distributions.\r\n"),
            WslAgents::NoDistro
        );
        for other in [
            "Catastrophic failure\r\nError code: Wsl/Service/E_UNEXPECTED\r\n",
            "The Windows Subsystem for Linux is not installed.",
            "Windows-Subsystem für Linux verfügt über keine installierten Distributionen.",
            "",
        ] {
            assert_eq!(failed_listing(other), WslAgents::Unknown, "{other}");
        }
    }

    /// Runs the real script under bash with a PATH built for the case.
    /// `windows_mount` stands in for /mnt/, which a test cannot write to.
    fn run_lookup(home: &Path, path: &str, windows_mount: &str) -> Option<Agents> {
        let script = home.join("lookup.sh");
        std::fs::write(&script, wsl_lookup_script(windows_mount)).unwrap();
        let out = std::process::Command::new("bash")
            .arg(&script)
            .env_clear()
            .env("HOME", home)
            .env("PATH", path)
            .output()
            .ok()?;
        parse_wsl_lookup(&String::from_utf8_lossy(&out.stdout))
    }

    #[cfg(unix)]
    fn executable(path: &Path) {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755)).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn the_wsl_lookup_script_finds_linux_installs_and_ignores_windows_ones() {
        let scratch = Scratch::new("script");
        let home = scratch.0.join("home");
        std::fs::create_dir_all(&home).unwrap();
        let base = "/usr/bin:/bin";

        assert_eq!(run_lookup(&home, base, WINDOWS_MOUNT), Some(Agents::default()));

        // The installer's own folder is searched even when PATH lacks it.
        executable(&scratch.file("home/.local/bin/claude"));
        assert_eq!(
            run_lookup(&home, base, WINDOWS_MOUNT),
            Some(Agents { claude: true, codex: false })
        );

        // A Linux install in a folder with a space in its name still counts.
        executable(&scratch.file("my tools/codex"));
        let with_tools = format!("{}:{base}", scratch.0.join("my tools").display());
        assert_eq!(
            run_lookup(&home, &with_tools, WINDOWS_MOUNT),
            Some(Agents { claude: true, codex: true })
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_windows_install_seen_from_wsl_is_not_a_wsl_install() {
        // "/mnt/c/Program Files/nodejs/codex": under the Windows mount, with a
        // space. Split on the space, its second half would look like a Linux
        // path and send a Windows-only PC into WSL.
        let scratch = Scratch::new("mount");
        let home = scratch.0.join("home");
        std::fs::create_dir_all(&home).unwrap();
        let mount = format!("{}/", scratch.0.join("mnt").display());
        executable(&scratch.file("mnt/c/Program Files/nodejs/codex"));
        executable(&scratch.file("mnt/c/Users/me/AppData/Roaming/npm/claude"));
        let path = format!(
            "{}:{}:/usr/bin:/bin",
            scratch.0.join("mnt/c/Program Files/nodejs").display(),
            scratch.0.join("mnt/c/Users/me/AppData/Roaming/npm").display()
        );
        assert_eq!(run_lookup(&home, &path, &mount), Some(Agents::default()));

        // A real Linux install behind the Windows one on PATH is still found.
        executable(&scratch.file("linux/bin/codex"));
        let path = format!("{path}:{}", scratch.0.join("linux/bin").display());
        assert_eq!(
            run_lookup(&home, &path, &mount),
            Some(Agents { claude: false, codex: true })
        );
    }

    #[test]
    fn the_shipped_lookup_script_uses_the_real_mount_and_ends_with_its_marker() {
        let script = wsl_lookup_script(WINDOWS_MOUNT);
        assert!(script.contains("case \"$found\" in '/mnt/'*) ;; /*) verdict=found ;; esac"));
        assert!(script.contains("done < <(type -aP \"$tool\" 2>/dev/null)"));
        assert!(!script.contains("{{"));
        assert!(script.trim_end().ends_with("echo \"solu-agent done\""));
    }
}
