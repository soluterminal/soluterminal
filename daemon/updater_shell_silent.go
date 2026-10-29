package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"
)

// The unattended ("when idle") installer update for an install whose
// installer only this daemon can fetch: the team channel, whose build comes
// through the cloud from a manifest signed "flavor": "team". The desktop app's
// own silent updater (desktop/src-tauri/src/shell_update.rs) reads the public
// releases only, so on that channel the app asks this daemon instead
// (POST /api/update/apply-shell?silent=1).
//
// The daemon stages and verifies the installer exactly as for the attended
// update (applyShell), then starts a small PowerShell courier outside the
// app's process tree. The courier waits for the desktop app to exit (the web
// page asks it to right after this returns), runs the installer with /S (no
// window), and starts the app again. If the app does not exit within a minute
// (the page saw new activity and stayed open) the courier gives up without
// installing anything. Whatever happens, every app it saw exit is started
// again, so the user is never left without it.

var errSilentShellUnsupported = errors.New("the silent installer update is only available for the Windows desktop app")

// A started courier is alive until the app exits or its wait runs out.
// Within silentCourierReuse of its start a repeated silent request (another
// window's countdown) can safely reuse it: the requester exits well inside the
// courier's remaining wait. After that and until the wait is over the courier
// may give up before the app closes, and a second courier would install a
// second time, so the request is refused; past silentCourierWindow a new
// courier starts.
const (
	silentCourierAppExitWait = 60 * time.Second
	silentCourierReuse       = 40 * time.Second
	silentCourierWindow      = silentCourierAppExitWait + 15*time.Second
	silentCourierStartWait   = 10 * time.Second
	silentCourierObserveFor  = 15 * time.Minute
	silentAppProcessName     = "soluterminal"
)

// silentShellPausedError: the signed control file (or the channel's default)
// has the unattended installer restart off. A state, not a failure.
type silentShellPausedError struct{ reason string }

func (e *silentShellPausedError) Error() string {
	return "automatic desktop restarts are paused: " + e.reason
}

var errSilentCourierWaiting = errors.New("an earlier silent installer update is still waiting for the app to exit; try again in a minute")

// silentShellHost reports whether this daemon runs where it can start a
// courier on the Windows desktop: natively on Windows, or inside WSL. A test
// seam, since a WSL runner and a Linux CI box answer differently.
var silentShellHost = func() bool {
	return runtime.GOOS == "windows" || (runtime.GOOS == "linux" && isInsideWSL())
}

func silentShellSupported(shellPlatform string) bool {
	return strings.HasPrefix(shellPlatform, "windows-") && silentShellHost()
}

// recentSilentShellLaunchLocked reports whether the last courier for version
// can be reused (reuse true) or is still waiting but too close to giving up
// (err set). Neither means a new courier may start.
func (u *Updater) recentSilentShellLaunchLocked(version string, now time.Time) (path string, reuse bool, err error) {
	if u.shellSilentVersion == "" || u.shellSilentVersion != version {
		return "", false, nil
	}
	age := now.Sub(u.shellSilentAt)
	switch {
	case age < silentCourierReuse:
		return u.shellSilentPath, true, nil
	case age < silentCourierWindow:
		return "", false, errSilentCourierWaiting
	default:
		return "", false, nil
	}
}

func (u *Updater) noteSilentShellLaunchLocked(version, path, attempt string, now time.Time) {
	u.shellSilentVersion = version
	u.shellSilentPath = path
	u.shellSilentAt = now
	u.shellSilentAttempt = attempt
}

// observeSilentCourierLocked surfaces what the last courier reported after the
// daemon answered. Only one outcome changes the status: the courier found the
// installer changed since it was verified, installed nothing and removed it.
// The daemon then reports that as a rejected launch with no staged path, so
// nothing offers to reveal or run that file.
func (u *Updater) observeSilentCourierLocked() {
	if u.shellSilentAttempt == "" || u.shellLaunchLog == nil {
		return
	}
	// A courier that never wrote a verdict (killed, user signed out) is not
	// polled for ever: well past its wait and an install, stop looking.
	if time.Since(u.shellSilentAt) > silentCourierObserveFor {
		u.shellSilentAttempt = ""
		return
	}
	event, detail := silentCourierFinal(u.shellLaunchLog.path, u.shellSilentAttempt)
	switch event {
	case "":
		return
	case "sha-mismatch":
		u.setShellLaunchLocked(ShellLaunchState{
			State: "rejected",
			Error: "The downloaded desktop installer changed after it was verified, so nothing was installed. " +
				"It was removed; the next update downloads it again. " + detail,
		})
	}
	// That courier is finished, so a later request may start a new one.
	u.shellSilentAttempt = ""
	u.shellSilentVersion = ""
}

// ApplyShellSilent stages the verified installer and starts the courier.
// It returns once the courier has reported that it is running; the caller then
// lets the desktop app exit.
func (u *Updater) ApplyShellSilent(ctx context.Context, dataDir string) (string, error) {
	return u.applyShell(ctx, dataDir, false, true)
}

// psSingleQuoted renders s as a PowerShell single-quoted literal.
func psSingleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// windowsDir returns the directory of a Windows-spelled path.
func windowsDir(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i > 0 {
		return path[:i]
	}
	return path
}

// silentCourierScript is the PowerShell the courier runs. Every value is a
// single-quoted literal; nothing in it comes from the network except the
// installer, which applyShell has already SHA-verified against the signed
// manifest.
func silentCourierScript(installer, logPath, attempt, sha256, appName string, appExitWait time.Duration) string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$installer = %s
$logPath = %s
$attempt = %s
$appName = %s
$expectedSha = %s
function ConvertTo-LaunchJsonString([string]$v) { if ($null -eq $v) { return '' }; return $v.Replace('\','\\').Replace('"','\"').Replace("`+"`r"+`",'\r').Replace("`+"`n"+`",'\n') }
function Write-LaunchLog([string]$ev,[string]$processId,[string]$detail,[string]$err) {
  try {
    $line = '{"ts":"' + (Get-Date).ToUniversalTime().ToString('o') + '","event":"' + (ConvertTo-LaunchJsonString $ev) + '","path":"' + (ConvertTo-LaunchJsonString $installer) + '","method":"silent-courier","pid":"' + (ConvertTo-LaunchJsonString $processId) + '","attempt":"' + (ConvertTo-LaunchJsonString $attempt) + '","detail":"' + (ConvertTo-LaunchJsonString $detail) + '","err":"' + (ConvertTo-LaunchJsonString $err) + '"}'
    Add-Content -LiteralPath $logPath -Value $line -Encoding UTF8
  } catch {}
}
$session = (Get-Process -Id $PID).SessionId
$apps = @(Get-Process -Name $appName -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -eq $session })
$appPaths = @($apps | Where-Object { $_.Path } | ForEach-Object { $_.Path } | Select-Object -Unique)
$appIds = @($apps | ForEach-Object { $_.Id })
if ($appPaths.Count -eq 0) { Write-LaunchLog 'failed' ([string]$PID) '' 'the desktop app is not running in this session'; exit 1 }
Write-LaunchLog 'courier-started' ([string]$PID) ($appPaths -join ';') ''
try {
  $deadline = (Get-Date).AddSeconds(%d)
  while (Get-Process -Id $appIds -ErrorAction SilentlyContinue) {
    if ((Get-Date) -ge $deadline) { throw 'the desktop app did not exit; nothing was installed' }
    Start-Sleep -Milliseconds 250
  }
  Write-LaunchLog 'app-exited' '' '' ''
  # applyShell verified these bytes against the signed digest; check again
  # in case the file was swapped while the courier waited.
  $actualSha = (Get-FileHash -Algorithm SHA256 -LiteralPath $installer -ErrorAction Stop).Hash
  if ([string]::IsNullOrEmpty($actualSha)) { throw 'could not read the installer to check it; nothing was installed' }
  if (-not [string]::Equals($actualSha, $expectedSha, [System.StringComparison]::OrdinalIgnoreCase)) {
    Write-LaunchLog 'sha-mismatch' '' ('expected ' + $expectedSha + ', found ' + $actualSha) 'installer changed after verification; nothing was installed'
    Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
    throw 'installer changed after verification; nothing was installed'
  }
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $installer
  $psi.Arguments = '/S'
  $psi.UseShellExecute = $false
  try { $install = [System.Diagnostics.Process]::Start($psi); $install.WaitForExit() }
  catch { $install = Start-Process -FilePath $installer -ArgumentList '/S' -Wait -PassThru }
  if ($install.ExitCode -ne 0) { throw ('installer exited with code ' + $install.ExitCode) }
  Write-LaunchLog 'installer-done' ([string]$install.Id) '' ''
} catch {
  Write-LaunchLog 'failed' '' '' $_.Exception.Message
}
# Start again every app that was running when the courier started and has
# since exited, installed or not. One that never exited (the page stayed open)
# is still running and is left alone.
foreach ($appExe in $appPaths) {
  if (Get-Process -Name $appName -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $appExe }) { continue }
  try {
    $relaunched = Start-Process -FilePath $appExe -PassThru
    Write-LaunchLog 'relaunched' ([string]$relaunched.Id) $appExe ''
  } catch {
    Write-LaunchLog 'failed' '' $appExe ('relaunch: ' + $_.Exception.Message)
  }
}
`, psSingleQuoted(installer), psSingleQuoted(logPath), psSingleQuoted(attempt), psSingleQuoted(appName),
		psSingleQuoted(sha256), int(appExitWait/time.Second))
}

// encodedPowerShell is the -EncodedCommand form of script (base64 UTF-16LE),
// so the courier needs no script file and no quoting on the command line.
func encodedPowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		b = append(b, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func silentCourierCommandLine(script string) string {
	return "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -EncodedCommand " + encodedPowerShell(script)
}

// silentCmdScript is the .cmd that explorer.exe runs on native Windows (the
// same courier hand-off the desktop app's own silent update uses): explorer
// starts it in the user's session and outside the app's Job Object, and
// `start` lets it return at once. The script goes in a file next to it, since
// a cmd.exe line is too short for the whole script in encoded form.
func silentCmdScript(courierPath string) string {
	// cmd.exe reads a .cmd in the OEM code page, so a non-ASCII profile path
	// cannot appear in it literally: the path travels inside -EncodedCommand.
	return "start \"\" /min " + silentCourierCommandLine("& "+psSingleQuoted(courierPath)) + "\r\n"
}

// launchSilentInstaller starts the courier for the staged installer at
// installerPath (its Windows spelling) and waits until the courier reports it
// is running. lg's file is the launch log the courier appends to.
func launchSilentInstaller(installerPath, shellPlatform, sha256 string, lg *shellLaunchLog) (launchResult, error) {
	if !silentShellSupported(shellPlatform) {
		return launchResult{}, errSilentShellUnsupported
	}
	if lg == nil || lg.path == "" {
		return launchResult{}, errors.New("silent installer update needs a launch log")
	}
	fromWSL := runtime.GOOS == "linux"
	attempt := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	winLog := windowsDir(installerPath) + `\` + filepath.Base(lg.path)
	if !fromWSL {
		winLog = lg.path
	}
	script := silentCourierScript(installerPath, winLog, attempt, sha256, silentAppProcessName, silentCourierAppExitWait)
	psExe, err := resolvePowerShell()
	if err != nil {
		return launchResult{}, err
	}

	method := ""
	if fromWSL {
		// WMI through the repaired interop socket, as for the attended
		// installer: it lands in the user's interactive session (wmiCreateCommand
		// refuses session 0) and outside every process tree we own.
		method = "silent-courier-wmi"
		lg.event("dispatch", map[string]string{"path": installerPath, "method": method})
		out, werr := runWMICourier(silentCourierCommandLine(script), true, psExe)
		if werr != nil {
			lg.event("verify", map[string]string{"path": installerPath, "method": method, "err": werr.Error(), "detail": strings.TrimSpace(out)})
			return launchResult{}, fmt.Errorf("could not start the silent update courier: %v: %s", werr, strings.TrimSpace(out))
		}
	} else {
		// Native Windows: WMI from this daemon can land in session 0, so hand a
		// .cmd to explorer.exe, which runs it in the user's session.
		method = "silent-courier-explorer"
		dir := filepath.Dir(lg.path)
		courierPath := filepath.Join(dir, "silent-update.ps1")
		cmdPath := filepath.Join(dir, "silent-update.cmd")
		// Windows PowerShell reads a BOM-less script in the ANSI code page, which
		// would garble a non-ASCII profile path; the BOM makes it UTF-8.
		if err := os.WriteFile(courierPath, append([]byte("\ufeff"), script...), 0o600); err != nil {
			return launchResult{}, fmt.Errorf("write silent update courier: %w", err)
		}
		if err := os.WriteFile(cmdPath, []byte(silentCmdScript(courierPath)), 0o600); err != nil {
			return launchResult{}, fmt.Errorf("write silent update courier: %w", err)
		}
		explorer, err := resolveExplorer(false)
		if err != nil {
			return launchResult{}, err
		}
		lg.event("dispatch", map[string]string{"path": installerPath, "method": method})
		cmd := exec.Command(explorer, cmdPath)
		cmd.SysProcAttr = shellInstallerSysProcAttr()
		if err := cmd.Start(); err != nil {
			return launchResult{}, fmt.Errorf("could not start the silent update courier: %w", err)
		}
		go func() { _ = waitOwnedCommand(cmd) }()
	}

	pid, err := waitForSilentCourier(lg.path, attempt, silentCourierStartWait)
	if err != nil {
		lg.event("verify", map[string]string{"path": installerPath, "method": method, "err": err.Error()})
		return launchResult{}, err
	}
	lg.event("verify", map[string]string{"path": installerPath, "method": method, "pid": pid})
	return launchResult{Method: method, PID: pid, Attempt: attempt}, nil
}

// waitForSilentCourier polls the launch log for this attempt's
// courier-started line (or its early failure) and returns the courier's pid.
func waitForSilentCourier(logPath, attempt string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		if pid, failure, found := silentCourierOutcome(logPath, attempt); found {
			if failure != "" {
				return "", fmt.Errorf("%w: silent update courier: %s", errLaunchNotObserved, failure)
			}
			return pid, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: silent update courier did not start within %s", errLaunchNotObserved, wait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// silentCourierFinal returns the courier's last outcome for attempt once it has
// one: installer-done, sha-mismatch or failed (with its detail), or "" while it
// is still waiting.
func silentCourierFinal(logPath, attempt string) (event, detail string) {
	for _, entry := range silentCourierEntries(logPath, attempt) {
		switch entry["event"] {
		case "sha-mismatch":
			return "sha-mismatch", entry["detail"]
		case "installer-done":
			event, detail = "installer-done", ""
		case "failed":
			if event == "" {
				event, detail = "failed", entry["err"]
			}
		}
	}
	return event, detail
}

func silentCourierEntries(logPath, attempt string) []map[string]string {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var out []map[string]string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "\ufeff")
		var entry map[string]string
		if line == "" || json.Unmarshal([]byte(line), &entry) != nil || entry["attempt"] != attempt {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func silentCourierOutcome(logPath, attempt string) (pid, failure string, found bool) {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return "", "", false
	}
	// The courier writes UTF-8 with a BOM on a new file; drop it line by line.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "\ufeff")
		var entry map[string]string
		if line == "" || json.Unmarshal([]byte(line), &entry) != nil || entry["attempt"] != attempt {
			continue
		}
		switch entry["event"] {
		case "courier-started":
			return entry["pid"], "", true
		case "failed":
			return "", entry["err"], true
		}
	}
	return "", "", false
}
