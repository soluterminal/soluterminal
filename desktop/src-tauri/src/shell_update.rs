// One-click installer self-update for CLOUD launch mode.
//
// In cloud mode the app runs no local daemon, so the daemon's ApplyShell
// (daemon/updater_shell.go) — which normally downloads + launches the NSIS
// installer — isn't available. This command replicates it natively so a cloud
// desktop user can update the shell in one click instead of hand-downloading.
//
// PowerShell fetches the release manifest and its detached signature over
// HTTPS; Rust verifies the Ed25519 signature with the compiled-in key and takes
// the windows-amd64 installer URL and SHA-256 from the verified bytes only
// (shell_update_manifest.rs). PowerShell then downloads the installer, checks
// its SHA-256 and launches it via explorer.exe. The explorer hand-off is load-bearing: a
// direct child spawn would sit inside the Tauri shell's kill-on-close Job Object
// and NSIS would tear the installer down mid-run when it closes the app (see the
// long note in daemon/updater_shell.go launchWindowsInstaller). explorer hands
// the file to the already-running user shell, so the installer runs in the
// interactive session (visible + UAC) and OUTSIDE our job.
//
// Integrity: the same trust chain as the daemon updater. A manifest that is
// unsigned, signed by another key, edited after signing, or that names an
// installer outside the releases repo stops the update before any download.
// A validly signed but OLDER release (an old tag re-marked "latest") is
// downloaded but never launched: admit_installer refuses anything older than
// the running shell or the installed daemon unless forceDowngrade is set.

#[cfg(any(windows, test))]
use crate::shell_update_manifest::VerifiedShellAsset;

// Shared PowerShell prefix for download + SHA-256 verification. Both the
// attended and silent commands use this exact staging path so the security and
// exit-code behavior cannot drift.
#[cfg(any(windows, test))]
fn download_script_prefix(force_downgrade: bool, asset: &VerifiedShellAsset) -> String {
    // The logger is defined before any network or file work so every failure
    // leaves a "stage-failed" line in launch.log. A 0.0.1199 shell once failed
    // here twice after a complete, correct download and left no trace at all.
    format!(
        r#"$ErrorActionPreference='Stop'
$forceDowngrade = {FORCE_DOWNGRADE}
$dir = Join-Path $env:LOCALAPPDATA 'soluterminal\shell-update'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$logPath = Join-Path $dir 'launch.log'
$dst = ''
function ConvertTo-LaunchJsonString([string]$v) {{ if ($null -eq $v) {{ return '' }}; return $v.Replace('\','\\').Replace('"','\"').Replace("`r",'\r').Replace("`n",'\n') }}
function Write-LaunchLog([string]$event,[string]$method,[string]$processId,[string]$session,[string]$detail,[string]$err) {{
  try {{
    $line = '{{"ts":"' + (Get-Date).ToUniversalTime().ToString('o') + '","event":"' + (ConvertTo-LaunchJsonString $event) + '","path":"' + (ConvertTo-LaunchJsonString $dst) + '","method":"' + (ConvertTo-LaunchJsonString $method) + '","pid":"' + (ConvertTo-LaunchJsonString $processId) + '","session":"' + (ConvertTo-LaunchJsonString $session) + '","detail":"' + (ConvertTo-LaunchJsonString $detail) + '","err":"' + (ConvertTo-LaunchJsonString $err) + '"}}'
    Add-Content -LiteralPath $logPath -Value $line -Encoding UTF8
  }} catch {{}}
}}
function Stop-Stage([int]$code,[string]$message) {{
  Write-LaunchLog 'stage-failed' '' '' '' ('exit=' + $code) $message
  [Console]::Error.WriteLine($message)
  exit $code
}}
trap {{ Stop-Stage 9 ('installer staging failed: ' + $_.Exception.Message) }}
# URL, digest and version come from the manifest Rust already verified.
$target = {TARGET}
$p = [pscustomobject]@{{ url = {URL}; sha256 = {SHA256} }}
$dst = Join-Path $dir (Split-Path $p.url -Leaf)
if (-not $forceDowngrade -and (Test-Path -LiteralPath ($dst + '.allow-daemon-downgrade'))) {{ try {{ Remove-Item -Force -LiteralPath ($dst + '.allow-daemon-downgrade') -ErrorAction Stop }} catch {{ Stop-Stage 8 ('remove stale downgrade token failed: ' + $_.Exception.Message) }} }}
try {{ $ProgressPreference='SilentlyContinue'; Invoke-WebRequest -UseBasicParsing -Uri $p.url -OutFile $dst }} catch {{ Stop-Stage 4 ('download failed: ' + $_.Exception.Message) }}
# Antivirus can hold a freshly written installer for a few seconds, so reading
# it for the hash gets a short retry instead of failing the whole update.
$got = $null
$hashErr = ''
for ($i = 0; $i -lt 10 -and -not $got; $i++) {{
  try {{ $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $dst -ErrorAction Stop).Hash.ToLower() }} catch {{ $hashErr = $_.Exception.Message; Start-Sleep -Milliseconds 1000 }}
}}
if (-not $got) {{ Stop-Stage 10 ('could not read the downloaded installer: ' + $hashErr) }}
if ($got -ne $p.sha256) {{ Remove-Item -Force -LiteralPath $dst -ErrorAction SilentlyContinue; Stop-Stage 5 ('sha256 mismatch: got ' + $got + ' want ' + $p.sha256) }}
Write-LaunchLog 'staged' '' '' '' ('size=' + (Get-Item -LiteralPath $dst).Length) ''"#,
        TARGET = ps_single_quoted(&asset.version),
        URL = ps_single_quoted(&asset.url),
        SHA256 = ps_single_quoted(&asset.sha256),
        FORCE_DOWNGRADE = if force_downgrade { "$true" } else { "$false" },
    )
}

#[cfg(windows)]
fn stage_script(force_downgrade: bool, asset: &VerifiedShellAsset) -> String {
    format!(
        "{}\n{}",
        download_script_prefix(force_downgrade, asset),
        r#"[Console]::Out.WriteLine('staged=' + $dst)
[Console]::Out.WriteLine('installer-version=' + $target)"#,
    )
}

// PowerShell for the attended launch phase. Downloading and admission happen
// before this script is built, so no installer process can start until Rust has
// compared the verified target against the installed daemon marker.
#[cfg(windows)]
fn launch_script(installer: &str) -> String {
    r#"$ErrorActionPreference='Stop'
$dst = __INSTALLER__
$dir = Split-Path $dst -Parent
$logPath = Join-Path $dir 'launch.log'
function ConvertTo-LaunchJsonString([string]$v) {{ if ($null -eq $v) {{ return '' }}; return $v.Replace('\','\\').Replace('"','\"').Replace("`r",'\r').Replace("`n",'\n') }}
function Write-LaunchLog([string]$event,[string]$method,[string]$processId,[string]$session,[string]$detail,[string]$err) {{
  try {{
    $line = '{{"ts":"' + (Get-Date).ToUniversalTime().ToString('o') + '","event":"' + (ConvertTo-LaunchJsonString $event) + '","path":"' + (ConvertTo-LaunchJsonString $dst) + '","method":"' + (ConvertTo-LaunchJsonString $method) + '","pid":"' + (ConvertTo-LaunchJsonString $processId) + '","session":"' + (ConvertTo-LaunchJsonString $session) + '","detail":"' + (ConvertTo-LaunchJsonString $detail) + '","err":"' + (ConvertTo-LaunchJsonString $err) + '"}}'
    Add-Content -LiteralPath $logPath -Value $line -Encoding UTF8
  }} catch {{}}
}}
Write-LaunchLog 'dispatch' 'explorer' '' '' '' ''
try { Start-Process -FilePath 'explorer.exe' -ArgumentList ('"' + $dst + '"') } catch {
  Write-LaunchLog 'verify' 'explorer' '' '' 'dispatch failed' $_.Exception.Message
  [Console]::Error.WriteLine('staged=' + $dst)
  [Console]::Error.WriteLine('installer staged at ' + $dst + ' but explorer dispatch failed: ' + $_.Exception.Message)
  exit 6
}
$leaf = (Split-Path $dst -Leaf).Replace("'", "''")
$seen0 = $false
for ($i = 0; $i -lt 20; $i++) {
  $all = @(Get-CimInstance Win32_Process -Filter ("Name='" + $leaf + "'") -ErrorAction SilentlyContinue)
  $live = $all | Where-Object { $_.SessionId -ne 0 } | Select-Object -First 1
  if ($live) {
    Write-LaunchLog 'verify' 'explorer' ([string]$live.ProcessId) ([string]$live.SessionId) '' ''
    [Console]::Out.WriteLine('launched ' + $dst + ' pid=' + $live.ProcessId + ' session=' + $live.SessionId)
    exit 0
  }
  $consent = Get-CimInstance Win32_Process -Filter "Name='consent.exe'" -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($consent) {
    Write-LaunchLog 'verify' 'explorer' ([string]$consent.ProcessId) 'uac' '' ''
    [Console]::Out.WriteLine('launched ' + $dst + ' pid=' + $consent.ProcessId + ' session=uac')
    exit 0
  }
  if ($all | Where-Object { $_.SessionId -eq 0 }) { $seen0 = $true }
  Start-Sleep -Milliseconds 500
}
[Console]::Error.WriteLine('staged=' + $dst)
if ($seen0) {
  Write-LaunchLog 'verify' 'explorer' '' '0' 'installer observed only in SessionId 0' 'invisible'
  [Console]::Error.WriteLine('installer staged at ' + $dst + ' but was observed only in SessionId 0')
  exit 7
}
Write-LaunchLog 'verify' 'explorer' '' '' 'installer process was not observed' 'not observed'
[Console]::Error.WriteLine('installer staged at ' + $dst + ' but was not observed running')
exit 6"#
		.replace("__INSTALLER__", &ps_single_quoted(installer))
		.replace("{{", "{")
		.replace("}}", "}")
}

#[cfg(any(windows, test))]
fn ps_single_quoted(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

// The courier is launched by explorer.exe, not as our child. That escape from
// the shell's kill-on-close Job Object is what lets it wait for this process to
// exit, install silently, and relaunch the same executable.
#[cfg(any(windows, test))]
fn silent_courier_script(
    app_pid: u32,
    installer: &str,
    app_exe: &str,
    log_path: &str,
    attempt: &str,
) -> String {
    format!(
        r#"$ErrorActionPreference='Stop'
$appPid = {app_pid}
$installer = {installer}
$appExe = {app_exe}
$logPath = {log_path}
$attempt = {attempt}
function ConvertTo-LaunchJsonString([string]$v) {{ if ($null -eq $v) {{ return '' }}; return $v.Replace('\','\\').Replace('"','\"').Replace("`r",'\r').Replace("`n",'\n') }}
function Write-LaunchLog([string]$event,[string]$method,[string]$processId,[string]$session,[string]$detail,[string]$err) {{
  try {{
    $line = '{{"ts":"' + (Get-Date).ToUniversalTime().ToString('o') + '","event":"' + (ConvertTo-LaunchJsonString $event) + '","path":"' + (ConvertTo-LaunchJsonString $installer) + '","method":"' + (ConvertTo-LaunchJsonString $method) + '","pid":"' + (ConvertTo-LaunchJsonString $processId) + '","session":"' + (ConvertTo-LaunchJsonString $session) + '","detail":"' + (ConvertTo-LaunchJsonString $detail) + '","err":"' + (ConvertTo-LaunchJsonString $err) + '"}}'
    Add-Content -LiteralPath $logPath -Value $line -Encoding UTF8
  }} catch {{}}
}}
Write-LaunchLog 'courier-started' 'silent-courier' ([string]$appPid) '' $attempt ''
try {{
  $deadline = (Get-Date).AddSeconds(60)
  while (Get-Process -Id $appPid -ErrorAction SilentlyContinue) {{
    if ((Get-Date) -ge $deadline) {{ throw 'timed out waiting for the app to exit' }}
    Start-Sleep -Milliseconds 250
  }}
  Write-LaunchLog 'app-exited' 'silent-courier' ([string]$appPid) '' $attempt ''
  $install = Start-Process -FilePath $installer -ArgumentList '/S' -Wait -PassThru
  if ($install.ExitCode -ne 0) {{ throw ('installer exited with code ' + $install.ExitCode) }}
  Write-LaunchLog 'installer-done' 'silent-courier' ([string]$install.Id) '' $attempt ''
  $relaunched = Start-Process -FilePath $appExe -PassThru
  Write-LaunchLog 'relaunched' 'silent-courier' ([string]$relaunched.Id) '' $attempt ''
}} catch {{
  Write-LaunchLog 'failed' 'silent-courier' ([string]$appPid) '' $attempt $_.Exception.Message
  exit 1
}}"#,
        app_pid = app_pid,
        installer = ps_single_quoted(installer),
        app_exe = ps_single_quoted(app_exe),
        log_path = ps_single_quoted(log_path),
        attempt = ps_single_quoted(attempt),
    )
}

#[cfg(any(windows, test))]
fn silent_cmd_script(courier_path: &str) -> String {
    // -Command lets PowerShell own the single-quoted path, including spaces and
    // apostrophes. `start` hands PowerShell off and returns immediately.
    format!(
        "start \"\" /min powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -Command \"& {}\"\r\n",
        ps_single_quoted(courier_path),
    )
}

#[cfg(any(windows, test))]
fn explorer_visible_path(path: &str) -> String {
    if let Some(rest) = path.strip_prefix(r"\\?\UNC\") {
        return format!(r"\\{rest}");
    }
    path.strip_prefix(r"\\?\").unwrap_or(path).to_string()
}

#[cfg(any(windows, test))]
fn explorer_select_argument(path: &str) -> String {
    format!(r#"/select,"{}""#, explorer_visible_path(path))
}

#[cfg(any(windows, test))]
fn is_path_within_windows_dir(target: &str, allowed: &str) -> bool {
    let target = explorer_visible_path(target)
        .replace('/', "\\")
        .to_lowercase();
    let allowed = explorer_visible_path(allowed)
        .replace('/', "\\")
        .trim_end_matches('\\')
        .to_lowercase();
    target
        .strip_prefix(&allowed)
        .is_some_and(|rest| rest.starts_with('\\') && rest.len() > 1)
}

// Resolve powershell.exe, falling back to its canonical System32 path when PATH
// is stripped (locked-down machines) — mirrors the daemon's resolvePowerShell.
#[cfg(windows)]
fn resolve_powershell() -> String {
    const CANON: &str = r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe";
    if std::path::Path::new(CANON).exists() {
        CANON.to_string()
    } else {
        "powershell.exe".to_string()
    }
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ShellUpdateOutcome {
    pub launched: bool,
    pub path: String,
    pub error: Option<String>,
    pub downgrade_refused: bool,
    pub update_held: bool,
    pub installed_version: Option<String>,
    pub installer_version: Option<String>,
}

#[cfg(windows)]
struct StagedShellInstaller {
    path: std::path::PathBuf,
    version: String,
}

#[cfg(any(windows, test))]
#[derive(Debug, Clone, PartialEq, Eq)]
struct DowngradeRefusal {
    installed_version: String,
    installer_version: String,
    message: String,
}

#[cfg(any(windows, test))]
#[derive(Debug, Clone, PartialEq, Eq)]
struct UpdateHoldRefusal {
    installer_version: String,
    message: String,
}

#[cfg(any(windows, test))]
#[derive(serde::Deserialize)]
struct PersistedRollbackState {
    hold: Option<PersistedRollbackHold>,
}

#[cfg(any(windows, test))]
#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct PersistedRollbackHold {
    version: String,
    expires_at_ms: i64,
}

#[cfg(any(windows, test))]
fn rollback_hold_refusal_from_documents<I, S>(
    installer_version: &str,
    documents: I,
    now_ms: i64,
) -> Option<UpdateHoldRefusal>
where
    I: IntoIterator<Item = S>,
    S: AsRef<str>,
{
    documents.into_iter().find_map(|raw| {
        let state: PersistedRollbackState = serde_json::from_str(raw.as_ref()).ok()?;
        let hold = state.hold?;
        if hold.version != installer_version || now_ms >= hold.expires_at_ms {
            return None;
        }
        Some(UpdateHoldRefusal {
            installer_version: installer_version.to_string(),
            message: format!(
                "installer version {installer_version} is held after rollback until epoch-ms {}; resume updates before installing it",
                hold.expires_at_ms
            ),
        })
    })
}

#[cfg(windows)]
fn active_rollback_hold_refusal(installer_version: &str) -> Option<UpdateHoldRefusal> {
    use std::time::{SystemTime, UNIX_EPOCH};

    let mut documents = Vec::new();
    if let Some(home) = std::env::var_os("USERPROFILE") {
        if let Ok(raw) = std::fs::read_to_string(
            std::path::PathBuf::from(home)
                .join(".solu")
                .join("update_rollback.json"),
        ) {
            documents.push(raw);
        }
    }
    // The primary Windows topology runs solud inside the default WSL distro;
    // inspect its daemon-local hold as well. This is read-only and never starts
    // an installer or mutates either filesystem.
    // Bounded, and skipped unless the distro is already running: an install
    // check must neither hang on a stuck VM nor start one.
    if let Ok(out) = crate::daemon::run_wsl_probe_bounded(
        &[
            "-e",
            "sh",
            "-lc",
            "cat \"$HOME/.solu/update_rollback.json\" 2>/dev/null || true",
        ],
        std::time::Duration::from_secs(10),
    ) {
        if out.status.success() && !out.stdout.is_empty() {
            documents.push(String::from_utf8_lossy(&out.stdout).into_owned());
        }
    }
    let now_ms = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(i64::MAX as u128) as i64;
    rollback_hold_refusal_from_documents(installer_version, &documents, now_ms)
}

#[cfg(any(windows, test))]
#[derive(Debug, Clone, PartialEq, Eq)]
enum InstalledDaemon {
    Absent,
    Known {
        version: String,
        source: &'static str,
    },
    Unknown(String),
}

#[cfg(any(windows, test))]
fn usable_installed_daemon_version(version: &str) -> bool {
    let version = version.trim();
    !version.is_empty()
        && version
            .split('.')
            .all(|part| !part.is_empty() && part.chars().all(|ch| ch.is_ascii_digit()))
}

// `running_shell` is this shell's own version. Like the daemon's
// shellDowngradeAdmission, the floor is the newer of the installed daemon and
// the running shell, so a replayed older (still validly signed) release cannot
// roll the app back even when no daemon is installed (cloud mode). A shell
// without an ordered release version (empty, "dev") adds no floor.
#[cfg(any(windows, test))]
fn admit_installer(
    installed: &InstalledDaemon,
    running_shell: &str,
    installer_version: &str,
    force_downgrade: bool,
) -> Result<(), DowngradeRefusal> {
    if force_downgrade {
        return Ok(());
    }
    let running_shell = running_shell.trim();
    if usable_installed_daemon_version(running_shell)
        && crate::daemon::numeric_version_cmp(running_shell, installer_version)
            .is_some_and(|ordering| ordering.is_gt())
    {
        return Err(DowngradeRefusal {
            installed_version: running_shell.to_string(),
            installer_version: installer_version.to_string(),
            message: format!(
                "refusing shell installer downgrade: running shell {running_shell} is newer than installer {installer_version}; retry only with forceDowngrade:true (or /SOLU_FORCE_DAEMON_DOWNGRADE) to allow it explicitly"
            ),
        });
    }
    match installed {
        InstalledDaemon::Absent => Ok(()),
        InstalledDaemon::Known { version, source } => {
            if !crate::daemon::numeric_version_cmp(version, installer_version)
                .is_some_and(|ordering| ordering.is_gt())
            {
                return Ok(());
            }
            Err(DowngradeRefusal {
                installed_version: version.clone(),
                installer_version: installer_version.to_string(),
                message: format!(
                    "refusing shell installer downgrade: installed daemon {version} ({source}) is newer than bundled daemon {installer_version}; retry only with forceDowngrade:true (or /SOLU_FORCE_DAEMON_DOWNGRADE) to allow it explicitly"
                ),
            })
        }
        InstalledDaemon::Unknown(reason) => Err(DowngradeRefusal {
            installed_version: reason.clone(),
            installer_version: installer_version.to_string(),
            message: format!(
                "refusing shell installer replacement: installed daemon version is unknown ({reason}); bundled daemon is {installer_version}; retry only with forceDowngrade:true (or /SOLU_FORCE_DAEMON_DOWNGRADE) to allow it explicitly"
            ),
        }),
    }
}

#[cfg(windows)]
fn stage_shell_installer(ps: &str, force_downgrade: bool) -> Result<StagedShellInstaller, String> {
    use std::process::Command;

    // Nothing is downloaded until the manifest's signature checks out.
    let asset = crate::shell_update_manifest::fetch_verified_shell_asset(ps)?;
    let staged = Command::new(ps)
        .args([
            "-NoProfile",
            "-NonInteractive",
            "-ExecutionPolicy",
            "Bypass",
            "-Command",
            &stage_script(force_downgrade, &asset),
        ])
        .output()
        .map_err(|e| format!("could not run powershell ({ps}): {e}"))?;
    if !staged.status.success() {
        let code = staged.status.code().unwrap_or(-1);
        return Err(format!(
            "installer update failed (exit {code}): {}",
            String::from_utf8_lossy(&staged.stderr).trim()
        ));
    }
    let stdout = String::from_utf8_lossy(&staged.stdout);
    let path = stdout
        .lines()
        .find_map(|line| line.strip_prefix("staged="))
        .map(str::trim)
        .filter(|path| !path.is_empty())
        .ok_or_else(|| "installer staged but PowerShell did not report its path".to_string())?;
    let version = stdout
        .lines()
        .find_map(|line| line.strip_prefix("installer-version="))
        .map(str::trim)
        .filter(|version| !version.is_empty())
        .ok_or_else(|| {
            "installer staged but manifest shell.version was not reported".to_string()
        })?;
    Ok(StagedShellInstaller {
        path: std::path::PathBuf::from(path),
        version: version.to_string(),
    })
}

#[cfg(any(windows, test))]
fn embedded_daemon_versions(bytes: &[u8]) -> Vec<String> {
    const NEEDLE: &[u8] = b"main.version=";
    let mut versions = Vec::new();
    let mut cursor = 0;
    while cursor + NEEDLE.len() <= bytes.len() {
        let Some(relative) = bytes[cursor..]
            .windows(NEEDLE.len())
            .position(|window| window == NEEDLE)
        else {
            break;
        };
        let value_start = cursor + relative + NEEDLE.len();
        let mut value_end = value_start;
        while value_end < bytes.len()
            && (bytes[value_end].is_ascii_digit() || bytes[value_end] == b'.')
        {
            value_end += 1;
        }
        let value = String::from_utf8_lossy(&bytes[value_start..value_end]).into_owned();
        if usable_installed_daemon_version(&value) && !versions.contains(&value) {
            versions.push(value);
        }
        cursor = value_start;
    }
    versions
}

#[cfg(any(windows, test))]
fn embedded_daemon_version(path: &std::path::Path) -> Result<Option<String>, String> {
    let bytes = std::fs::read(path).map_err(|err| err.to_string())?;
    let versions = embedded_daemon_versions(&bytes);
    Ok((versions.len() == 1).then(|| versions[0].clone()))
}

#[cfg(any(windows, test))]
fn installed_daemon_version(app_exe: &std::path::Path) -> Result<InstalledDaemon, String> {
    let dir = app_exe
        .parent()
        .ok_or_else(|| format!("installed app path has no parent: {}", app_exe.display()))?;
    let marker = dir.join("solud.exe.version");
    let daemon = dir.join("solud.exe");
    let daemon_exists = daemon.try_exists().map_err(|err| {
        format!(
            "could not inspect installed daemon {}: {err}",
            daemon.display()
        )
    })?;
    if !daemon_exists {
        return Ok(InstalledDaemon::Absent);
    }
    if let Ok(value) = std::fs::read_to_string(&marker) {
        let value = value.trim();
        if usable_installed_daemon_version(value) {
            return Ok(InstalledDaemon::Known {
                version: value.to_string(),
                source: "marker",
            });
        }
    }

    if let Some(version) = embedded_daemon_version(&daemon).map_err(|err| {
        format!(
            "could not inspect installed daemon {}: {err}",
            daemon.display()
        )
    })? {
        return Ok(InstalledDaemon::Known {
            version,
            source: "binary",
        });
    }

    // The single-value helper deliberately returns None for both zero and
    // ambiguous values. Re-scan only to make the fail-closed reason actionable.
    let bytes = std::fs::read(&daemon).map_err(|err| {
        format!(
            "could not inspect installed daemon {}: {err}",
            daemon.display()
        )
    })?;
    let versions = embedded_daemon_versions(&bytes);
    let reason = if versions.len() > 1 {
        format!(
            "existing solud.exe has ambiguous embedded build versions: {}",
            versions.join(", ")
        )
    } else {
        "existing solud.exe has no usable version marker and no embedded build version".to_string()
    };
    Ok(InstalledDaemon::Unknown(reason))
}

#[cfg(any(windows, test))]
fn installed_daemon_outcome_version(installed: &InstalledDaemon) -> Option<String> {
    match installed {
        InstalledDaemon::Absent => None,
        InstalledDaemon::Known { version, .. } => Some(version.clone()),
        InstalledDaemon::Unknown(reason) => Some(reason.clone()),
    }
}

#[cfg(any(windows, test))]
fn downgrade_token_path(installer: &std::path::Path) -> std::path::PathBuf {
    let mut token = installer.as_os_str().to_os_string();
    token.push(".allow-daemon-downgrade");
    std::path::PathBuf::from(token)
}

#[cfg(any(windows, test))]
fn configure_downgrade_token(installer: &std::path::Path, force: bool) -> Result<(), String> {
    let token = downgrade_token_path(installer);
    if force {
        std::fs::write(&token, b"single-use\n").map_err(|err| {
            format!(
                "could not create forced-downgrade token {}: {err}",
                token.display()
            )
        })
    } else {
        match std::fs::remove_file(&token) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(err) => Err(format!(
                "could not remove stale downgrade token {}: {err}",
                token.display()
            )),
        }
    }
}

#[cfg(windows)]
fn refusal_outcome(staged: &StagedShellInstaller, refusal: DowngradeRefusal) -> ShellUpdateOutcome {
    use std::io::Write;

    crate::daemon::launch_log(&refusal.message);
    if let Some(dir) = staged.path.parent() {
        let log_path = dir.join("launch.log");
        if let Ok(mut log) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(log_path)
        {
            let event = serde_json::json!({
                "event": "downgrade-refused",
                "path": staged.path.to_string_lossy(),
                "installedVersion": &refusal.installed_version,
                "installerVersion": &refusal.installer_version,
                "detail": &refusal.message,
            });
            let _ = writeln!(log, "{event}");
        }
    }
    ShellUpdateOutcome {
        launched: false,
        path: staged.path.to_string_lossy().into_owned(),
        error: Some(refusal.message),
        downgrade_refused: true,
        update_held: false,
        installed_version: Some(refusal.installed_version),
        installer_version: Some(refusal.installer_version),
    }
}

#[cfg(windows)]
fn hold_refusal_outcome(
    staged: &StagedShellInstaller,
    refusal: UpdateHoldRefusal,
) -> ShellUpdateOutcome {
    crate::daemon::launch_log(&refusal.message);
    ShellUpdateOutcome {
        launched: false,
        path: staged.path.to_string_lossy().into_owned(),
        error: Some(refusal.message),
        downgrade_refused: false,
        update_held: true,
        installed_version: None,
        installer_version: Some(refusal.installer_version),
    }
}

/// The reason an installer update did not launch, if it failed. Policy
/// refusals carry their own message and are logged where they are decided.
fn apply_failure_reason(result: &Result<ShellUpdateOutcome, String>) -> Option<String> {
    match result {
        Err(err) => Some(err.clone()),
        Ok(outcome) if !outcome.launched && !outcome.downgrade_refused && !outcome.update_held => {
            outcome.error.clone()
        }
        Ok(_) => None,
    }
}

// The web layer used to swallow these errors and open a download page, so a
// failed update left no explanation anywhere. Record it in both logs.
fn record_apply_failure(kind: &str, result: &Result<ShellUpdateOutcome, String>) {
    let Some(reason) = apply_failure_reason(result) else {
        return;
    };
    #[cfg(windows)]
    {
        use std::io::Write;
        use std::time::{SystemTime, UNIX_EPOCH};

        crate::daemon::launch_log(&format!("shell update ({kind}) failed: {reason}"));
        if let Some(local) = std::env::var_os("LOCALAPPDATA") {
            let dir = std::path::PathBuf::from(local)
                .join("soluterminal")
                .join("shell-update");
            if std::fs::create_dir_all(&dir).is_ok() {
                if let Ok(mut log) = std::fs::OpenOptions::new()
                    .create(true)
                    .append(true)
                    .open(dir.join("launch.log"))
                {
                    let ts_ms = SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis();
                    let event = serde_json::json!({
                        "event": "apply-failed",
                        "method": kind,
                        "err": reason,
                        "tsMs": ts_ms.to_string(),
                    });
                    let _ = writeln!(log, "{event}");
                }
            }
        }
    }
    #[cfg(not(windows))]
    let _ = (kind, reason);
}

#[cfg(windows)]
static SILENT_APPLY_IN_FLIGHT: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

/// Download + verify + launch the latest Windows shell installer. Returns the
/// verified launch outcome and staged path. The installer (running outside our Job
/// Object) prompts to close the app, installs over it, and relaunches — so we do
/// NOT exit here, matching the daemon's ApplyShell.
#[tauri::command]
pub async fn apply_shell_update(
    force_downgrade: Option<bool>,
) -> Result<ShellUpdateOutcome, String> {
    let result = apply_shell_update_inner(force_downgrade).await;
    record_apply_failure("attended", &result);
    result
}

async fn apply_shell_update_inner(
    force_downgrade: Option<bool>,
) -> Result<ShellUpdateOutcome, String> {
    #[cfg(windows)]
    {
        use std::process::Command;
        let force_downgrade = force_downgrade.unwrap_or(false);
        let ps = resolve_powershell();
        let staged = stage_shell_installer(&ps, force_downgrade)?;
        if let Some(refusal) = active_rollback_hold_refusal(&staged.version) {
            return Ok(hold_refusal_outcome(&staged, refusal));
        }
        let app_exe = std::env::current_exe()
            .map_err(|e| format!("could not resolve current app executable: {e}"))?;
        let installed = installed_daemon_version(&app_exe)?;
        let installed_version = installed_daemon_outcome_version(&installed);
        if let Err(refusal) = admit_installer(
            &installed,
            env!("SOLU_SHELL_VERSION"),
            &staged.version,
            force_downgrade,
        ) {
            return Ok(refusal_outcome(&staged, refusal));
        }
        configure_downgrade_token(&staged.path, force_downgrade)?;
        let out = Command::new(&ps)
            .args([
                "-NoProfile",
                "-NonInteractive",
                "-ExecutionPolicy",
                "Bypass",
                "-Command",
                &launch_script(&staged.path.to_string_lossy()),
            ])
            .output()
            .map_err(|e| format!("could not run powershell ({ps}): {e}"))?;
        if out.status.success() {
            let stdout = String::from_utf8_lossy(&out.stdout);
            let line = stdout
                .lines()
                .find(|line| line.starts_with("launched "))
                .unwrap_or("launched ")
                .trim();
            let path = line
                .strip_prefix("launched ")
                .unwrap_or(line)
                .rsplit_once(" pid=")
                .map(|(path, _)| path)
                .unwrap_or("")
                .to_string();
            Ok(ShellUpdateOutcome {
                launched: true,
                path,
                error: None,
                downgrade_refused: false,
                update_held: false,
                installed_version: installed_version.clone(),
                installer_version: Some(staged.version),
            })
        } else {
            let stderr = String::from_utf8_lossy(&out.stderr);
            let code = out.status.code().unwrap_or(-1);
            if code == 6 || code == 7 {
                let path = stderr
                    .lines()
                    .find_map(|line| line.strip_prefix("staged="))
                    .unwrap_or("")
                    .trim()
                    .to_string();
                return Ok(ShellUpdateOutcome {
                    launched: false,
                    path,
                    error: Some(
                        "installer downloaded but did not start — open it manually".to_string(),
                    ),
                    downgrade_refused: false,
                    update_held: false,
                    installed_version,
                    installer_version: Some(staged.version),
                });
            }
            Err(format!(
                "installer update failed (exit {code}): {}",
                stderr.trim()
            ))
        }
    }
    #[cfg(not(windows))]
    {
        let _ = force_downgrade;
        Err("one-click installer update is only available on Windows".to_string())
    }
}

/// This app's own process id. The page passes it with the daemon's unattended
/// installer update (POST /api/update/apply-shell?silent=1), so the daemon's
/// courier waits on exactly this process instead of matching by name (#739:
/// the name it matched, "soluterminal", was not this exe's).
#[tauri::command]
pub fn app_process_id() -> u32 {
    std::process::id()
}

/// Stage the verified current-user NSIS installer, launch an out-of-job courier
/// through explorer.exe, and wait until its first log event proves it survived.
/// The web layer exits only after this command succeeds.
#[tauri::command]
pub async fn apply_shell_update_silent(
    force_downgrade: Option<bool>,
) -> Result<ShellUpdateOutcome, String> {
    let result = apply_shell_update_silent_inner(force_downgrade).await;
    record_apply_failure("silent", &result);
    result
}

async fn apply_shell_update_silent_inner(
    force_downgrade: Option<bool>,
) -> Result<ShellUpdateOutcome, String> {
    #[cfg(windows)]
    {
        use std::process::Command;
        use std::sync::atomic::Ordering;
        use std::time::{Duration, SystemTime, UNIX_EPOCH};

        let force_downgrade = force_downgrade.unwrap_or(false);
        if SILENT_APPLY_IN_FLIGHT.swap(true, Ordering::SeqCst) {
            return Ok(ShellUpdateOutcome {
                launched: true,
                path: String::new(),
                error: None,
                downgrade_refused: false,
                update_held: false,
                installed_version: None,
                installer_version: None,
            });
        }
        struct ResetInFlight(bool);
        impl Drop for ResetInFlight {
            fn drop(&mut self) {
                if self.0 {
                    SILENT_APPLY_IN_FLIGHT.store(false, Ordering::SeqCst);
                }
            }
        }
        let mut reset = ResetInFlight(true);

        let ps = resolve_powershell();
        let staged = stage_shell_installer(&ps, force_downgrade)?;
        if let Some(refusal) = active_rollback_hold_refusal(&staged.version) {
            return Ok(hold_refusal_outcome(&staged, refusal));
        }
        let installer_path = staged.path.clone();
        let installer = installer_path.to_string_lossy().into_owned();
        let dir = installer_path
            .parent()
            .ok_or_else(|| format!("invalid staged installer path: {installer}"))?;
        let app_exe = std::env::current_exe()
            .map_err(|e| format!("could not resolve current app executable: {e}"))?;
        let installed = installed_daemon_version(&app_exe)?;
        let installed_version = installed_daemon_outcome_version(&installed);
        if let Err(refusal) = admit_installer(
            &installed,
            env!("SOLU_SHELL_VERSION"),
            &staged.version,
            force_downgrade,
        ) {
            return Ok(refusal_outcome(&staged, refusal));
        }
        configure_downgrade_token(&staged.path, force_downgrade)?;
        let courier_path = dir.join("silent-update.ps1");
        let cmd_path = dir.join("silent-update.cmd");
        let log_path = dir.join("launch.log");
        let attempt = format!(
            "{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        );
        let courier = silent_courier_script(
            std::process::id(),
            &explorer_visible_path(&installer_path.to_string_lossy()),
            &explorer_visible_path(&app_exe.to_string_lossy()),
            &explorer_visible_path(&log_path.to_string_lossy()),
            &attempt,
        );
        std::fs::write(&courier_path, courier)
            .map_err(|e| format!("could not write silent-update.ps1: {e}"))?;
        std::fs::write(
            &cmd_path,
            silent_cmd_script(&explorer_visible_path(&courier_path.to_string_lossy())),
        )
        .map_err(|e| format!("could not write silent-update.cmd: {e}"))?;

        let cmd_visible = explorer_visible_path(&cmd_path.to_string_lossy());
        let dispatch_script = format!(
            "$ErrorActionPreference='Stop'; try {{ Start-Process -FilePath 'explorer.exe' -ArgumentList ('\"' + {} + '\"') }} catch {{ [Console]::Error.WriteLine($_.Exception.Message); exit 8 }}",
            ps_single_quoted(&cmd_visible),
        );
        let dispatched = Command::new(&ps)
            .args([
                "-NoProfile",
                "-NonInteractive",
                "-ExecutionPolicy",
                "Bypass",
                "-Command",
                &dispatch_script,
            ])
            .output()
            .map_err(|e| format!("could not dispatch silent update courier: {e}"))?;
        if !dispatched.status.success() {
            return Err(format!(
                "silent update courier dispatch failed (exit {}): {}",
                dispatched.status.code().unwrap_or(-1),
                String::from_utf8_lossy(&dispatched.stderr).trim()
            ));
        }

        let marker = r#""event":"courier-started""#;
        let mut started = false;
        for _ in 0..20 {
            if std::fs::read_to_string(&log_path)
                .map(|log| log.contains(&marker) && log.contains(&attempt))
                .unwrap_or(false)
            {
                started = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(500));
        }
        if !started {
            return Ok(ShellUpdateOutcome {
                launched: false,
                path: installer,
                error: Some("silent update courier did not start within 10 seconds".to_string()),
                downgrade_refused: false,
                update_held: false,
                installed_version: installed_version.clone(),
                installer_version: Some(staged.version),
            });
        }

        // Keep the guard raised until this process exits. A second window that
        // races the countdown gets a successful no-op and cannot stage twice.
        // If the web layer discovers a last-moment safety hold and deliberately
        // stays alive, the courier times out after 60s; release shortly after so
        // a later quiet-window attempt stages a fresh courier instead of treating
        // the stale invocation as successful forever.
        std::thread::spawn(|| {
            std::thread::sleep(Duration::from_secs(75));
            SILENT_APPLY_IN_FLIGHT.store(false, Ordering::SeqCst);
        });
        reset.0 = false;
        Ok(ShellUpdateOutcome {
            launched: true,
            path: installer,
            error: None,
            downgrade_refused: false,
            update_held: false,
            installed_version,
            installer_version: Some(staged.version),
        })
    }
    #[cfg(not(windows))]
    {
        let _ = force_downgrade;
        Err("silent installer update is only available on Windows".to_string())
    }
}

#[tauri::command]
pub fn reveal_shell_installer(path: String) -> Result<(), String> {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        use std::path::PathBuf;
        use std::process::Command;

        let local = std::env::var_os("LOCALAPPDATA")
            .ok_or_else(|| "LOCALAPPDATA is unavailable".to_string())?;
        let allowed = std::fs::canonicalize(
            PathBuf::from(local)
                .join("soluterminal")
                .join("shell-update"),
        )
        .map_err(|e| format!("could not resolve shell-update directory: {e}"))?;
        let target = std::fs::canonicalize(&path)
            .map_err(|e| format!("could not resolve staged installer {path}: {e}"))?;
        if !is_path_within_windows_dir(&target.to_string_lossy(), &allowed.to_string_lossy()) {
            return Err("refusing to reveal a path outside the shell-update directory".to_string());
        }
        const DETACHED_PROCESS: u32 = 0x0000_0008;
        Command::new("explorer.exe")
            .raw_arg(explorer_select_argument(&target.to_string_lossy()))
            .creation_flags(DETACHED_PROCESS)
            .spawn()
            .map_err(|e| format!("could not open installer folder: {e}"))?;
        Ok(())
    }
    #[cfg(not(windows))]
    {
        let _ = path;
        Err("showing a shell installer is only available on Windows".to_string())
    }
}

#[cfg(test)]
#[path = "../tests/shell_update/missing_marker.rs"]
mod missing_marker_regression_tests;

#[cfg(test)]
mod tests {
    use super::{
        admit_installer, configure_downgrade_token, downgrade_token_path, explorer_select_argument,
        installed_daemon_version, is_path_within_windows_dir, rollback_hold_refusal_from_documents,
        silent_cmd_script, silent_courier_script, InstalledDaemon,
    };
    use super::{apply_failure_reason, download_script_prefix, ShellUpdateOutcome};
    use crate::shell_update_manifest::VerifiedShellAsset;

    fn verified_asset() -> VerifiedShellAsset {
        VerifiedShellAsset {
            url: "https://github.com/o/r/releases/download/v1.2.3/soluterminal_1.2.3_x64-setup.exe".into(),
            sha256: "ab".repeat(32),
            version: "1.2.3".into(),
        }
    }

    fn outcome(launched: bool, error: Option<&str>) -> ShellUpdateOutcome {
        ShellUpdateOutcome {
            launched,
            path: String::new(),
            error: error.map(str::to_string),
            downgrade_refused: false,
            update_held: false,
            installed_version: None,
            installer_version: None,
        }
    }

    #[test]
    fn apply_failures_are_recorded_with_their_reason() {
        let err: Result<ShellUpdateOutcome, String> =
            Err("installer update failed (exit 10): could not read".to_string());
        assert_eq!(
            apply_failure_reason(&err).as_deref(),
            Some("installer update failed (exit 10): could not read")
        );
        let not_started = Ok(outcome(
            false,
            Some("installer downloaded but did not start"),
        ));
        assert_eq!(
            apply_failure_reason(&not_started).as_deref(),
            Some("installer downloaded but did not start")
        );
        assert_eq!(apply_failure_reason(&Ok(outcome(true, None))), None);
        let mut refused = outcome(false, Some("refusing shell installer downgrade"));
        refused.downgrade_refused = true;
        assert_eq!(apply_failure_reason(&Ok(refused)), None);
        let mut held = outcome(false, Some("held after rollback"));
        held.update_held = true;
        assert_eq!(apply_failure_reason(&Ok(held)), None);
    }

    #[test]
    fn staging_logs_every_failure_before_doing_any_work() {
        let script = download_script_prefix(false, &verified_asset());
        let logger = script.find("function Stop-Stage").expect("failure logger");
        let download = script.find("Invoke-WebRequest").expect("download");
        assert!(logger < download);
        assert!(
            script.contains("trap {"),
            "unexpected errors must be logged too"
        );
        assert!(script.contains("Write-LaunchLog 'stage-failed'"));
        for code in [
            "Stop-Stage 4",
            "Stop-Stage 5",
            "Stop-Stage 8",
            "Stop-Stage 10",
        ] {
            assert!(script.contains(code), "missing {code}");
        }
        // Every raw `exit N` goes through Stop-Stage, so none can skip the log.
        assert_eq!(script.matches("exit ").count(), 1, "{script}");
    }

    #[test]
    fn staging_downloads_only_what_the_verified_manifest_named() {
        let script = download_script_prefix(false, &verified_asset());
        // The staging script never reads a manifest itself: URL, digest and
        // version are the verified values, single-quoted.
        assert!(!script.contains("Invoke-RestMethod"), "{script}");
        assert!(!script.contains("update.json"), "{script}");
        assert!(script.contains("$target = '1.2.3'"));
        assert!(script.contains(
            "url = 'https://github.com/o/r/releases/download/v1.2.3/soluterminal_1.2.3_x64-setup.exe'"
        ));
        assert!(script.contains(&format!("sha256 = '{}'", "ab".repeat(32))));
        let mut hostile = verified_asset();
        hostile.version = "1'; Remove-Item x; '".into();
        let script = download_script_prefix(false, &hostile);
        assert!(script.contains("$target = '1''; Remove-Item x; '''"));
    }

    #[test]
    fn staging_retries_reading_a_freshly_downloaded_installer() {
        let script = download_script_prefix(false, &verified_asset());
        assert!(script.contains("Get-FileHash -Algorithm SHA256 -LiteralPath $dst"));
        assert!(script.contains("$i -lt 10"));
        assert!(script.contains("Start-Sleep -Milliseconds 1000"));
        assert!(!script.contains("{{"), "format! braces must be unescaped");
    }

    fn fixture_dir(label: &str) -> std::path::PathBuf {
        let unique = format!(
            "solu-shell-{label}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .expect("clock")
                .as_nanos()
        );
        let dir = std::env::temp_dir().join(unique);
        std::fs::create_dir_all(&dir).expect("create fixture");
        dir
    }

    #[test]
    fn shell_installer_admission_is_monotonic_by_default() {
        for (name, installed, installer, force, want_refusal) in [
            (
                "fresh install",
                InstalledDaemon::Absent,
                "0.0.916",
                false,
                false,
            ),
            (
                "same-version reinstall",
                InstalledDaemon::Known {
                    version: "0.0.916".to_string(),
                    source: "marker",
                },
                "0.0.916",
                false,
                false,
            ),
            (
                "newer-available upgrade",
                InstalledDaemon::Known {
                    version: "0.0.900".to_string(),
                    source: "marker",
                },
                "0.0.916",
                false,
                false,
            ),
            (
                "incident downgrade",
                InstalledDaemon::Known {
                    version: "0.0.932".to_string(),
                    source: "binary",
                },
                "0.0.916",
                false,
                true,
            ),
            (
                "forced downgrade",
                InstalledDaemon::Known {
                    version: "0.0.932".to_string(),
                    source: "binary",
                },
                "0.0.916",
                true,
                false,
            ),
        ] {
            let result = admit_installer(&installed, "", installer, force);
            assert_eq!(result.is_err(), want_refusal, "{name}: {result:?}");
            if let Err(refusal) = result {
                assert!(refusal.message.contains("0.0.932"));
                assert!(refusal.message.contains("0.0.916"));
                assert!(refusal.message.contains("forceDowngrade:true"));
            }
        }
    }

    #[test]
    fn running_shell_version_is_a_downgrade_floor_without_a_daemon() {
        // Cloud mode installs no daemon, so an older signed release replayed as
        // "latest" must still be refused by the shell's own version.
        let refusal = admit_installer(&InstalledDaemon::Absent, "0.0.1214", "0.0.1100", false)
            .expect_err("older installer than the running shell must be refused");
        assert_eq!(refusal.installed_version, "0.0.1214");
        assert_eq!(refusal.installer_version, "0.0.1100");
        assert!(refusal.message.contains("running shell 0.0.1214"));
        assert!(refusal.message.contains("forceDowngrade:true"));

        admit_installer(&InstalledDaemon::Absent, "0.0.1214", "0.0.1100", true)
            .expect("an explicit forced downgrade is still allowed");
        admit_installer(&InstalledDaemon::Absent, "0.0.1214", "0.0.1214", false)
            .expect("reinstalling the same version is allowed");
        admit_installer(&InstalledDaemon::Absent, "0.0.1214", "0.0.1215", false)
            .expect("upgrade is allowed");
        for unordered in ["", "dev", " "] {
            admit_installer(&InstalledDaemon::Absent, unordered, "0.0.1100", false)
                .expect("a shell without a release version adds no floor");
        }

        // The newer of daemon and shell wins, as in the daemon's admission.
        let daemon = InstalledDaemon::Known {
            version: "0.0.1100".to_string(),
            source: "marker",
        };
        admit_installer(&daemon, "0.0.1214", "0.0.1150", false)
            .expect_err("a newer running shell refuses even when the daemon is older");
    }

    #[test]
    fn active_rollback_hold_refuses_native_manual_and_silent_installer_admission() {
        let raw = r#"{"schema":1,"hold":{"version":"2.0.0","fromVersion":"1.0.0","reason":"regression","createdAtMs":1000,"expiresAtMs":4102444800000}}"#;
        for path in ["manual", "silent"] {
            let refusal = rollback_hold_refusal_from_documents("2.0.0", [raw], 2_000)
                .unwrap_or_else(|| panic!("{path} installer path bypassed active hold"));
            assert_eq!(refusal.installer_version, "2.0.0");
            assert!(refusal.message.contains("held"));
        }
        assert!(rollback_hold_refusal_from_documents("2.0.1", [raw], 2_000).is_none());
        assert!(rollback_hold_refusal_from_documents("2.0.0", [raw], 4_102_444_800_000).is_none());
    }

    #[test]
    fn native_marker_and_single_use_token_are_managed_next_to_payloads() {
        let dir = fixture_dir("downgrade-token");
        let app = dir.join("soluterminal.exe");
        let installer = dir.join("setup.exe");

        assert_eq!(
            installed_daemon_version(&app).expect("missing marker"),
            InstalledDaemon::Absent
        );
        std::fs::write(dir.join("solud.exe"), b"installed daemon").expect("write daemon");
        std::fs::write(dir.join("solud.exe.version"), " 0.0.932\r\n").expect("write marker");
        assert_eq!(
            installed_daemon_version(&app).expect("read marker"),
            InstalledDaemon::Known {
                version: "0.0.932".to_string(),
                source: "marker"
            }
        );

        configure_downgrade_token(&installer, true).expect("create forced token");
        let token = downgrade_token_path(&installer);
        assert!(token.exists());
        configure_downgrade_token(&installer, false).expect("remove stale token");
        assert!(!token.exists());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn existing_daemon_without_usable_marker_is_refused_by_default() {
        let dir = fixture_dir("unversioned-daemon");
        let app = dir.join("soluterminal.exe");
        std::fs::write(dir.join("solud.exe"), b"pre-marker installed daemon")
            .expect("write installed daemon fixture");

        let installed = installed_daemon_version(&app).expect("inspect installed daemon");
        assert!(matches!(installed, InstalledDaemon::Unknown(_)));
        let refusal = admit_installer(&installed, "", "0.0.916", false)
            .expect_err("an existing daemon with no trustworthy version must fail closed");
        assert!(refusal.message.contains("0.0.916"));
        assert!(refusal.message.contains("version is unknown"));
        assert!(refusal.message.contains("forceDowngrade:true"));
        admit_installer(&installed, "", "0.0.916", true)
            .expect("an explicit forced downgrade may replace an unversioned daemon");

        std::fs::write(dir.join("solud.exe.version"), b"not-a-version\r\n")
            .expect("write malformed marker fixture");
        let malformed = installed_daemon_version(&app).expect("inspect malformed marker");
        assert!(matches!(malformed, InstalledDaemon::Unknown(_)));
        admit_installer(&malformed, "", "0.0.916", false)
            .expect_err("an existing daemon with a malformed marker must also fail closed");

        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn embedded_build_version_is_derived_when_marker_is_missing() {
        let dir = fixture_dir("embedded-version");
        let app = dir.join("soluterminal.exe");
        std::fs::write(
            dir.join("solud.exe"),
            b"\x00build\t-ldflags=\"-X main.version=0.0.932 -X main.requireSignedManifest=1\"\x00",
        )
        .expect("write embedded build info");
        let installed = installed_daemon_version(&app).expect("derive embedded version");
        assert_eq!(
            installed,
            InstalledDaemon::Known {
                version: "0.0.932".to_string(),
                source: "binary"
            }
        );
        admit_installer(&installed, "", "0.0.916", false).expect_err("downgrade must be refused");
        admit_installer(&installed, "", "0.0.932", false).expect("same version is monotonic");
        admit_installer(&installed, "", "0.0.940", false).expect("upgrade is monotonic");
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn malformed_marker_falls_back_to_embedded_version() {
        let dir = fixture_dir("malformed-marker-embedded-version");
        let app = dir.join("soluterminal.exe");
        std::fs::write(dir.join("solud.exe.version"), b"not-a-version\n")
            .expect("write malformed marker");
        std::fs::write(
            dir.join("solud.exe"),
            b"-X main.version=0.0.932 -X main.other=1",
        )
        .expect("write embedded build info");
        assert_eq!(
            installed_daemon_version(&app).expect("derive embedded version"),
            InstalledDaemon::Known {
                version: "0.0.932".to_string(),
                source: "binary"
            }
        );
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn ambiguous_embedded_versions_fail_closed() {
        let dir = fixture_dir("ambiguous-embedded-version");
        let app = dir.join("soluterminal.exe");
        std::fs::write(
            dir.join("solud.exe"),
            b"main.version=0.0.932\x00main.version=0.0.940\x00",
        )
        .expect("write ambiguous build info");
        let installed = installed_daemon_version(&app).expect("inspect ambiguous version");
        assert!(matches!(installed, InstalledDaemon::Unknown(_)));
        let refusal = admit_installer(&installed, "", "0.0.916", false)
            .expect_err("ambiguous versions must fail closed");
        assert!(refusal.message.contains("version is unknown"));
        assert!(refusal.message.contains("0.0.932"));
        assert!(refusal.message.contains("0.0.940"));
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn marker_wins_over_embedded_version() {
        let dir = fixture_dir("marker-wins");
        let app = dir.join("soluterminal.exe");
        std::fs::write(dir.join("solud.exe"), b"main.version=0.0.932\x00")
            .expect("write embedded build info");
        std::fs::write(dir.join("solud.exe.version"), b"0.0.940\r\n").expect("write marker");
        assert_eq!(
            installed_daemon_version(&app).expect("read marker"),
            InstalledDaemon::Known {
                version: "0.0.940".to_string(),
                source: "marker"
            }
        );
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn nsis_payload_guard_precedes_kill_and_records_successful_version() {
        let hooks = include_str!("../installer/hooks.nsh");
        let preinstall = hooks
            .split("!macro NSIS_HOOK_PREINSTALL")
            .nth(1)
            .and_then(|tail| tail.split("!macroend").next())
            .expect("preinstall hook");
        let existing_daemon = preinstall
            .find(r#"IfFileExists "$INSTDIR\solud.exe""#)
            .expect("existing daemon probe");
        let marker = preinstall
            .find("solud.exe.version")
            .expect("version marker read");
        let embedded = preinstall
            .find(r"main\.version=")
            .expect("embedded build-version probe");
        let unknown_refusal = preinstall
            .find("solu_downgrade_version_unknown:")
            .expect("unknown-version refusal");
        let kill = preinstall
            .find("!insertmacro _SOLU_KILL_RUNNING_PROCESSES")
            .expect("process kill");
        assert!(
            existing_daemon < marker
                && marker < embedded
                && embedded < unknown_refusal
                && unknown_refusal < kill,
            "installed-version derivation and refusals must precede process kill"
        );
        assert!(preinstall.contains("/SOLU_FORCE_DAEMON_DOWNGRADE"));
        assert!(preinstall.contains("$EXEPATH.allow-daemon-downgrade"));
        assert!(preinstall.contains("SetErrorLevel 65"));
        assert!(preinstall.contains("SetErrorLevel 67"));
        let postinstall = hooks
            .split("!macro NSIS_HOOK_POSTINSTALL")
            .nth(1)
            .and_then(|tail| tail.split("!macroend").next())
            .expect("postinstall hook");
        assert!(postinstall.contains(r#"$INSTDIR\solud.exe.version"#));
        assert!(postinstall.contains("${VERSION}"));
    }

    #[test]
    fn launch_logger_does_not_bind_read_only_powershell_variables() {
        let source = include_str!("shell_update.rs");
        for reserved in ["$pid", "$args", "$input", "$this"] {
            assert!(
                !source.contains(&format!("[string]{reserved}")),
                "PowerShell logger parameter {reserved} is automatic/read-only and prevents the logger body from running"
            );
        }
    }

    #[test]
    fn silent_courier_does_not_bind_read_only_powershell_variables() {
        let script =
            silent_courier_script(123, "setup.exe", "soluterminal.exe", "launch.log", "try-1");
        let lower = script.to_lowercase();
        for reserved in ["$pid", "$args", "$input", "$this"] {
            assert!(
                !lower.contains(&format!("[string]{reserved}")),
                "courier binds automatic/read-only PowerShell variable {reserved}"
            );
        }
        assert!(script.contains("$appPid = 123"));
        assert!(script.contains("-ArgumentList '/S'"));
    }

    #[test]
    fn silent_generated_files_single_quote_paths_with_spaces_and_apostrophes() {
        let courier_path = r"C:\Users\O'Brien\App Data\silent-update.ps1";
        let courier = silent_courier_script(
            7,
            r"C:\Users\O'Brien\App Data\setup.exe",
            r"C:\Program Files\O'Brien\soluterminal.exe",
            r"C:\Users\O'Brien\App Data\launch.log",
            "attempt's id",
        );
        assert!(courier.contains(r"$installer = 'C:\Users\O''Brien\App Data\setup.exe'"));
        assert!(courier.contains(r"$appExe = 'C:\Program Files\O''Brien\soluterminal.exe'"));
        assert!(courier.contains(r"$attempt = 'attempt''s id'"));

        let cmd = silent_cmd_script(courier_path);
        assert!(cmd.contains(r"& 'C:\Users\O''Brien\App Data\silent-update.ps1'"));
        assert_eq!(cmd.lines().count(), 1);
    }

    #[cfg(windows)]
    #[test]
    fn launch_logger_writes_jsonl_under_windows_powershell() {
        use std::process::Command;
        use std::time::{SystemTime, UNIX_EPOCH};

        let generated = super::launch_script(r"C:\Users\x\setup.exe");
        let start = generated.find("$logPath =").expect("logger start");
        let end = generated
            .find("Write-LaunchLog 'dispatch'")
            .expect("first logger call");
        let logger = &generated[start..end];
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let dir = std::env::temp_dir().join(format!("solu-shell-launch-log-{unique}"));
        std::fs::create_dir_all(&dir).expect("create smoke directory");
        let dst = dir.join("setup.exe");
        std::fs::write(&dst, b"smoke").expect("create staged smoke file");
        let ps_quote = |value: &std::path::Path| value.to_string_lossy().replace('\'', "''");
        let command = format!(
            "$dir='{}'; $dst='{}'; {logger}; Write-LaunchLog 'smoke' 'explorer' '123' '1' '' ''",
            ps_quote(&dir),
            ps_quote(&dst),
        );
        let output = Command::new(super::resolve_powershell())
            .args(["-NoProfile", "-NonInteractive", "-Command", &command])
            .output()
            .expect("run PowerShell logger smoke test");
        assert!(
            output.status.success(),
            "PowerShell logger failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let log = std::fs::read_to_string(dir.join("launch.log")).expect("read launch.log");
        assert!(log.contains(r#""event":"smoke""#), "launch.log = {log:?}");
        assert!(log.contains(r#""pid":"123""#), "launch.log = {log:?}");
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn reveal_uses_non_verbatim_path_and_one_raw_explorer_argument() {
        assert_eq!(
            explorer_select_argument(
                r"\\?\C:\Users\John Doe\AppData\Local\soluterminal\shell-update\setup.exe"
            ),
            r#"/select,"C:\Users\John Doe\AppData\Local\soluterminal\shell-update\setup.exe""#,
        );
        assert_eq!(
            explorer_select_argument(r"\\?\UNC\server\share\shell-update\setup.exe"),
            r#"/select,"\\server\share\shell-update\setup.exe""#,
        );
    }

    #[test]
    fn reveal_containment_is_windows_case_insensitive_and_boundary_aware() {
        let allowed = r"C:\Users\x\AppData\Local\soluterminal\shell-update";
        assert!(is_path_within_windows_dir(
            r"c:\USERS\X\APPDATA\LOCAL\SOLUTERMINAL\SHELL-UPDATE\setup.exe",
            allowed,
        ));
        assert!(is_path_within_windows_dir(
            r"C:\Users\ÉLODIE\AppData\Local\soluterminal\shell-update\setup.exe",
            r"c:\users\élodie\appdata\local\soluterminal\shell-update",
        ));
        assert!(!is_path_within_windows_dir(
            r"C:\Users\x\AppData\Local\soluterminal\shell-update-evil\setup.exe",
            allowed,
        ));
        assert!(!is_path_within_windows_dir(allowed, allowed));
    }
}
