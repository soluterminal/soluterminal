package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errLaunchNotObserved = errors.New("installer launch not observed")
	errLaunchInvisible   = errors.New("installer launched in non-interactive session 0")
)

type launchResult struct {
	Method  string
	PID     string
	Session string
	// Attempt identifies a silent courier's lines in the launch log.
	Attempt string
}

const shellDowngradeTokenSuffix = ".allow-daemon-downgrade"

// shellDaemonDowngradeError is returned when a shell installer would replace
// either the running daemon or the installed shell with an older bundled
// daemon. Keeping this typed lets the HTTP API return a stable, actionable 409
// without conflating the safety policy with download or launch failures.
type shellDaemonDowngradeError struct {
	InstalledVersion     string
	InstallerVersion     string
	CurrentDaemonVersion string
	CurrentShellVersion  string
}

func (e *shellDaemonDowngradeError) Error() string {
	return fmt.Sprintf(
		"refusing shell installer downgrade: installed version %s (daemon %s, shell %s) is newer than bundled daemon %s; retry explicitly with ?force=1 to allow the downgrade",
		e.InstalledVersion, versionOrUnknown(e.CurrentDaemonVersion), versionOrUnknown(e.CurrentShellVersion), e.InstallerVersion,
	)
}

func versionOrUnknown(version string) string {
	if strings.TrimSpace(version) == "" {
		return "not installed"
	}
	return version
}

func shellDowngradeAdmission(target, currentDaemon, currentShell string, forceDowngrade bool) error {
	if forceDowngrade {
		return nil
	}
	installed := ""
	for _, version := range []string{currentDaemon, currentShell} {
		version = strings.TrimSpace(version)
		// Development daemons do not have an ordered release version. Treat them
		// like the existing version-floor and external-downgrade exemptions do;
		// an installed release shell still remains a usable admission candidate.
		if version == "" || version == "dev" {
			continue
		}
		if installed == "" || isNewer(version, installed) {
			installed = version
		}
	}
	if installed == "" || !isNewer(installed, target) {
		return nil
	}
	return &shellDaemonDowngradeError{
		InstalledVersion:     installed,
		InstallerVersion:     target,
		CurrentDaemonVersion: currentDaemon,
		CurrentShellVersion:  currentShell,
	}
}

// ApplyShell downloads the platform's shell installer (NSIS .exe on
// Windows; future MSI / DMG / AppImage), verifies its SHA-256, then
// launches it detached. The installer takes over from there: NSIS's
// Tauri template detects the running app, prompts to close, installs
// over the existing binaries, and (by default) relaunches the app.
//
// We do NOT spawn a successor or os.Exit here — unlike the daemon
// auto-update, the installer needs the original shell + daemon to
// be the things it replaces. NSIS handles the close-running-app
// dance via window message; the running solud will die when its
// parent Tauri shell does (Job Object on Windows).
//
// The download lands in <dataDir>/shell-update/, which persists across
// daemon restarts so a partial download is resumed-or-re-fetched
// rather than re-downloaded every time the user opens the installer
// dialog. We don't currently resume — sha mismatch = re-download —
// but the path is stable for future improvement.
func (u *Updater) ApplyShell(ctx context.Context, dataDir string, forceDowngrade bool) (string, error) {
	return u.applyShell(ctx, dataDir, forceDowngrade, false)
}

// applyShell is ApplyShell with an unattended mode. silent stages and verifies
// the installer exactly as the attended path does (same manifest, channel,
// hold and downgrade checks), then hands it to a courier that installs it with
// no installer window once the desktop app exits, and relaunches the app (see
// updater_shell_silent.go). The two modes never combine with a forced
// downgrade: an unattended update must not override that refusal.
func (u *Updater) applyShell(ctx context.Context, dataDir string, forceDowngrade, silent bool) (string, error) {
	if silent && forceDowngrade {
		return "", errors.New("a silent installer update never forces a downgrade")
	}
	u.mu.Lock()
	if u.shellApplying {
		u.mu.Unlock()
		return "", errors.New("shell update already in progress")
	}
	if u.manifest == nil || u.manifest.Shell == nil {
		u.mu.Unlock()
		return "", errors.New("no shell update available; run /api/update/check first")
	}
	u.recomputeShellLocked()
	targetVersion := u.manifest.Shell.Version
	// The installer belongs to the channel whose manifest named it. A check
	// that switches channels while it downloads must not let it launch.
	shellChannel := u.channel
	if u.rollback != nil {
		if hold, active := u.rollback.ActiveHold(time.Now()); active && hold.Version == targetVersion {
			u.status.ShellLaunch = nil
			u.mu.Unlock()
			u.shellLogf("update: shell installer refused because version %s is held until %s", targetVersion, time.UnixMilli(hold.ExpiresAtMs).Format(time.RFC3339))
			return "", &updateHeldError{version: hold.Version, expiresAtMs: hold.ExpiresAtMs, reason: hold.Reason}
		}
	}
	admissionErr := shellDowngradeAdmission(targetVersion, u.current, u.shellCurrent, forceDowngrade)
	platform, ok := u.manifest.Shell.Platforms[u.shellPlatform]
	if !ok {
		u.mu.Unlock()
		return "", fmt.Errorf("no installer in manifest for shell platform %s", u.shellPlatform)
	}
	if silent {
		if !silentShellSupported(u.shellPlatform) {
			u.mu.Unlock()
			return "", errSilentShellUnsupported
		}
		if paused, reason := u.ctrlEffective.shellAutoApplyPaused(u.channel); paused {
			u.mu.Unlock()
			return "", &silentShellPausedError{reason: reason}
		}
		// Every app window runs its own countdown. The first one to finish
		// starts the courier; a second request inside the courier's wait window
		// gets the same answer instead of a second courier and a second install.
		if path, reuse, err := u.recentSilentShellLaunchLocked(targetVersion, time.Now()); reuse || err != nil {
			u.mu.Unlock()
			return path, err
		}
	}
	// Capture the installed versions while holding the updater lock so the log
	// describes the exact decision. The admission check intentionally uses the
	// versions actually installed, not manifest.version: daemon-only publishes
	// legitimately leave shell.version pointing at an older carried installer.
	currentDaemon := u.current
	currentShell := u.shellCurrent
	shellPlatform := u.shellPlatform
	if admissionErr == nil {
		u.shellApplying = true
	}
	// ShellLaunch is cleared on EVERY attempt that passes the precondition checks
	// — admitted, policy-refused and token-cleanup-failure paths alike — so a
	// refused attempt can never leave an older launch-failed path
	// revealable/runnable; only the admitted path repopulates it via
	// setShellLaunchLocked (staged → launching → launched | launch-failed).
	u.status.ShellLaunch = nil
	u.mu.Unlock()
	if admissionErr == nil {
		// Reset shellApplying after launch (or on error). We don't want it stuck
		// true forever — the installer is a separate process and might be canceled
		// at the UAC prompt.
		defer func() {
			u.mu.Lock()
			u.shellApplying = false
			u.mu.Unlock()
		}()
	}

	dlDir := filepath.Join(dataDir, "shell-update")
	name := filepath.Base(platform.URL)
	if name == "" || name == "/" || name == "." {
		name = fmt.Sprintf("soluterminal-shell-%s", targetVersion)
	}
	dstPath := filepath.Join(dlDir, name)
	tokenPath := dstPath + shellDowngradeTokenSuffix
	var tokenCleanupErr error
	if !forceDowngrade {
		// A token authorizes exactly one installer invocation. A later normal
		// attempt must never inherit authorization from an abandoned forced run.
		if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
			tokenCleanupErr = fmt.Errorf("remove stale shell downgrade token %s: %w", tokenPath, err)
		}
	}
	if tokenCleanupErr != nil {
		// The adjacent token could still authorize this staged executable when it
		// is opened manually. Remove the executable before returning so a cleanup
		// failure cannot become a fail-open downgrade path. The token may remain
		// locked, but without its exact adjacent payload it grants nothing.
		installerCleanupErr := quarantineAuthorizedShellInstaller(dstPath)
		if installerCleanupErr != nil {
			u.shellLogf("update: shell installer blocked because stale downgrade authorization could not be revoked; token=%s installer=%s tokenErr=%v installerErr=%v",
				tokenPath, dstPath, tokenCleanupErr, installerCleanupErr)
			return "", fmt.Errorf("stale shell downgrade token could not be revoked and authorized installer %s could not be quarantined: %w",
				dstPath, errors.Join(tokenCleanupErr, installerCleanupErr))
		}
		u.shellLogf("update: shell installer quarantined because stale downgrade authorization could not be revoked; token=%s installer=%s err=%v",
			tokenPath, dstPath, tokenCleanupErr)
		return "", tokenCleanupErr
	}
	if admissionErr != nil {
		u.shellLogf("update: shell installer refused (installed daemon %s, installed shell %s, bundled daemon %s); pass ?force=1 only to explicitly allow the downgrade",
			versionOrUnknown(currentDaemon), versionOrUnknown(currentShell), targetVersion)
		return "", admissionErr
	}

	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}
	if err := u.download(ctx, platform.URL, platform.SHA256, dstPath); err != nil {
		return "", fmt.Errorf("download installer: %w", err)
	}
	if forceDowngrade && strings.HasPrefix(shellPlatform, "windows-") {
		// Created only after SHA verification. The NSIS payload consumes and
		// deletes this adjacent token before it permits the destructive copy.
		if err := os.WriteFile(tokenPath, []byte(targetVersion+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("authorize forced shell downgrade: %w", err)
		}
	}
	if err := pruneShellInstallers(dlDir, dstPath); err != nil {
		log.Printf("update: prune old shell installers: %v", err)
	}

	lg := newShellLaunchLog(dlDir)
	displayPath, displayErr := u.shellDisplayPath(dstPath, shellPlatform)
	if displayErr != nil {
		// The Linux path is still better recovery information than an empty path
		// if WSL interop is too broken even for wslpath.
		displayPath = dstPath
	}
	size := ""
	if info, err := os.Stat(dstPath); err == nil {
		size = strconv.FormatInt(info.Size(), 10)
	}
	lg.event("staged", map[string]string{"path": displayPath, "size": size})
	u.mu.Lock()
	u.shellLaunchLog = lg
	u.setShellLaunchLocked(ShellLaunchState{State: "staged", StagedPath: displayPath})
	u.mu.Unlock()

	if displayErr != nil {
		launchErr := fmt.Errorf("prepare Windows installer path: %w", displayErr)
		return "", u.finishShellLaunchFailed(displayPath, launchErr, lg)
	}

	u.mu.Lock()
	if u.channel != shellChannel {
		u.status.ShellLaunch = nil
		u.mu.Unlock()
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("update channel changed to %s while the installer downloaded; not launching the %s installer", u.channelSnapshot(), shellChannel)
	}
	if silent {
		// The operator may have paused it while the installer downloaded.
		if paused, reason := u.ctrlEffective.shellAutoApplyPaused(u.channel); paused {
			u.status.ShellLaunch = nil
			u.mu.Unlock()
			return "", &silentShellPausedError{reason: reason}
		}
	}
	u.setShellLaunchLocked(ShellLaunchState{State: "launching", StagedPath: displayPath})
	u.mu.Unlock()
	var result launchResult
	var launchErr error
	if silent {
		// The courier checks this digest again right before it installs.
		result, launchErr = u.launchShellSilent(displayPath, shellPlatform, platform.SHA256, lg)
	} else {
		result, launchErr = u.launchShell(displayPath, shellPlatform, lg)
	}
	if launchErr != nil {
		return "", u.finishShellLaunchFailed(displayPath, launchErr, lg)
	}
	lg.event("launched", map[string]string{
		"path": displayPath, "method": result.Method, "pid": result.PID, "session": result.Session,
	})
	u.mu.Lock()
	u.setShellLaunchLocked(ShellLaunchState{
		State: "launched", StagedPath: displayPath, Method: result.Method,
		PID: result.PID, Session: result.Session,
	})
	// Under the same lock as "launched", so the courier's later verdict
	// (observeSilentCourierLocked) can only ever replace it, never be replaced.
	if silent {
		u.noteSilentShellLaunchLocked(targetVersion, displayPath, result.Attempt, time.Now())
	}
	u.mu.Unlock()
	if u.bus != nil {
		u.bus.publish(uiEvent{Type: "update-available"})
	}
	log.Printf("update: launched shell installer %s (target shell and bundled daemon version %s)", displayPath, targetVersion)
	return displayPath, nil
}

// quarantineAuthorizedShellInstaller breaks adjacency between an executable
// and a token that could not be revoked. Removal is preferred; rename and
// truncation are fail-closed fallbacks for filesystems that deny one operation
// while still permitting another (notably antivirus races on Windows).
func quarantineAuthorizedShellInstaller(path string) error {
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return nil
	}
	blockedPath := path + ".blocked"
	_ = os.Remove(blockedPath)
	if err := os.Rename(path, blockedPath); err == nil || os.IsNotExist(err) {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	return file.Close()
}

func pruneShellInstallers(dlDir, protectedPath string) error {
	entries, err := os.ReadDir(dlDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type installer struct {
		path      string
		modified  time.Time
		protected bool
	}
	installers := make([]installer, 0, len(entries))
	protectedPath = filepath.Clean(protectedPath)
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "launch.log") || strings.HasSuffix(entry.Name(), shellDowngradeTokenSuffix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dlDir, entry.Name())
		installers = append(installers, installer{path: path, modified: info.ModTime(), protected: filepath.Clean(path) == protectedPath})
	}
	sort.Slice(installers, func(i, j int) bool {
		if installers[i].protected != installers[j].protected {
			return installers[i].protected
		}
		return installers[i].modified.After(installers[j].modified)
	})
	for index := 2; index < len(installers); index++ {
		item := installers[index]
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (u *Updater) setShellLaunchLocked(state ShellLaunchState) {
	state.UpdatedAtMs = time.Now().UnixMilli()
	// Always replace the pointer. Status() may have returned the previous one,
	// and mutating it here would create an unlocked alias/data race.
	next := state
	u.status.ShellLaunch = &next
}

func (u *Updater) finishShellLaunchFailed(stagedPath string, cause error, lg *shellLaunchLog) error {
	message := fmt.Sprintf("installer downloaded to %s but did not start — open it manually", stagedPath)
	lg.event("launch-failed", map[string]string{"path": stagedPath, "err": cause.Error(), "detail": message})
	u.mu.Lock()
	u.setShellLaunchLocked(ShellLaunchState{State: "launch-failed", StagedPath: stagedPath, Error: message})
	u.mu.Unlock()
	if u.bus != nil {
		u.bus.publish(uiEvent{Type: "update-available"})
	}
	return fmt.Errorf("%s: %w", message, cause)
}

func shellInstallerDisplayPath(path, shellPlatform string) (string, error) {
	if runtime.GOOS == "linux" && isInsideWSL() && strings.HasPrefix(shellPlatform, "windows-") {
		out, err := runWSLPath(context.Background(), "-w", path)
		if err != nil {
			return "", fmt.Errorf("wslpath %s: %w", path, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return path, nil
}

// Path conversion is a bounded probe, not an installer launch. A stalled WSL
// socket must return control to ApplyShell's existing manual-recovery path.
// WaitDelay also bounds inherited output pipes after the converter exits.
func runWSLPath(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wslpath", args...)
	applyDetachedNoBreakaway(cmd)
	cmd.Cancel = func() error { return killChildTree(cmd.Process) }
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		_ = killChildTree(cmd.Process)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return out, err
}

// launchInstaller spawns the downloaded installer detached so it
// survives our process exit. The right tool depends on the SHELL
// platform, NOT the daemon's runtime OS — under WSL the daemon is
// Linux but the installer is a Windows .exe, so we need Windows
// interop, not xdg-open.
//
// For a Windows installer — whether the daemon is a WSL-side Linux
// process or a native Windows process — we launch it via WMI
// Win32_Process.Create rather than `cmd /c start`. Two reasons:
//
//  1. The installer must outlive the Tauri shell's Job Object. That
//     job has KILL_ON_JOB_CLOSE and catches every descendant
//     (Tauri → … → solud → courier → installer). When NSIS detects the
//     running app and kills soluterminal.exe, the whole job tree comes
//     down — including the installer mid-run, leaving the user with the
//     old version uninstalled and the new one never started.
//  2. On native Windows we used to launch `cmd /c start` with
//     CREATE_BREAKAWAY_FROM_JOB (shellInstallerSysProcAttr) to escape
//     that job. But if the daemon's parent job forbids breakaway
//     (JOB_OBJECT_LIMIT_BREAKAWAY_OK unset), CreateProcess fails with
//     ERROR_ACCESS_DENIED — the literal "fork/exec …cmd.exe: Access is
//     denied" that blocked updates on locked-down machines.
//
// WMI's Win32_Process.Create spawns the new process under the WMI
// provider service (WmiPrvSE.exe), OUTSIDE any job our user-mode parent
// owns. The installer survives the soluterminal teardown AND we never
// request breakaway, so the access-denied path can't fire. The courier
// PowerShell that fires the WMI call is short-lived and does not need to
// break away — it must NOT, or it would hit the very error this avoids.
func launchInstaller(path, shellPlatform string, lg *shellLaunchLog) (launchResult, error) {
	switch {
	case runtime.GOOS == "linux" && isInsideWSL() && strings.HasPrefix(shellPlatform, "windows-"):
		// ApplyShell already converted this once so the same user-facing path is
		// used for launch state, diagnostics, dispatch, and manual recovery.
		return launchWindowsInstaller(path, true, lg)

	case runtime.GOOS == "windows":
		// Native Windows: path is already a Windows path (filepath.Join used
		// backslashes), so no conversion.
		return launchWindowsInstaller(path, false, lg)

	case runtime.GOOS == "darwin":
		cmd := exec.Command("open", path)
		lg.event("dispatch", map[string]string{"path": path, "method": "open"})
		cmd.SysProcAttr = shellInstallerSysProcAttr()
		if err := cmd.Start(); err != nil {
			return launchResult{}, err
		}
		go func() { _ = waitOwnedCommand(cmd) }()
		return launchResult{Method: "open"}, nil

	default: // linux (non-WSL): the installer matches this Linux host.
		cmd := exec.Command("xdg-open", path)
		lg.event("dispatch", map[string]string{"path": path, "method": "xdg-open"})
		cmd.SysProcAttr = shellInstallerSysProcAttr()
		if err := cmd.Start(); err != nil {
			return launchResult{}, err
		}
		go func() { _ = waitOwnedCommand(cmd) }()
		return launchResult{Method: "xdg-open"}, nil
	}
}

// launchWindowsInstaller launches the Windows installer at winPath, choosing the
// mechanism by daemon context (fromWSL) so the installer always appears in the
// user's INTERACTIVE session and survives the Tauri Job Object teardown.
//
// Native Windows daemon (fromWSL=false): explorer.exe FIRST. WMI Win32_Process
// .Create from the native daemon can place the installer on a non-interactive
// station/desktop (invisible) — the "Windows-only mode: installer never comes up"
// symptom — even when it reports ReturnValue 0 (Dual mode worked because there
// the WSL daemon launches via interop, which lands in the interactive session).
// explorer.exe hands the file to the user's already-running shell, which
// ShellExecutes it in the interactive session (visible window + UAC) and outside
// our job (the installer becomes the shell's child, not our descendant). WMI is
// the fallback for the rare case no interactive shell/explorer is available.
//
// WSL daemon (fromWSL=true): WMI FIRST, via the repaired WSL interop env — that
// courier lands the installer in the interactive session (proven) and gives a
// verified launch (PID + non-zero SessionId). explorer is the fallback, including
// when WMI "succeeds" into the invisible session 0 (wmiCreateCommand exit 12).
// This path is unchanged because Dual/WSL mode already works.
//
// If BOTH mechanisms fail (dead WSL interop, or no shell + no powershell on a
// locked-down box), we return an error NAMING the installer path so the UI can
// tell the user what to run manually — never a silent "launched" that leaves the
// update prompt stuck.
func launchWindowsInstaller(winPath string, fromWSL bool, lg *shellLaunchLog) (launchResult, error) {
	psExe, psErr := resolvePowerShell()

	if !fromWSL {
		// Native Windows: interactive explorer launch first (visible in the user
		// session); WMI only if explorer is unavailable.
		if result, ferr := launchViaExplorer(winPath, false, psExe, lg); ferr == nil {
			log.Printf("update: launched shell installer via explorer (native)")
			return result, nil
		} else {
			if psErr != nil {
				return launchResult{}, fmt.Errorf("could not launch installer %q — explorer launch could not be verified (%v) and powershell is unavailable (%v); open the file manually to finish updating", winPath, ferr, psErr)
			}
			log.Printf("update: explorer launch failed (%v) — falling back to WMI", ferr)
			lg.event("dispatch", map[string]string{"path": winPath, "method": "wmi"})
			out, werr := runWMICourier(winPath, false, psExe)
			if werr != nil {
				lg.event("verify", map[string]string{"path": winPath, "method": "wmi", "err": werr.Error(), "detail": strings.TrimSpace(out)})
				return launchResult{}, fmt.Errorf("could not launch installer %q — explorer failed (%v) and WMI failed (%v: %s); open the file manually to finish updating",
					winPath, ferr, werr, strings.TrimSpace(out))
			}
			result := parseLaunchResult("wmi", out)
			lg.event("verify", map[string]string{"path": winPath, "method": "wmi", "pid": result.PID, "session": result.Session})
			advisoryVerify(winPath, false, psExe, lg)
			log.Printf("update: WMI shell installer launch verified (%s)", strings.TrimSpace(out))
			return result, nil
		}
	}

	// WSL → Windows: WMI first (via interop), explorer fallback.
	if psErr != nil {
		return launchResult{}, psErr
	}
	lg.event("dispatch", map[string]string{"path": winPath, "method": "wmi"})
	out, werr := runWMICourier(winPath, true, psExe)
	if werr != nil {
		lg.event("verify", map[string]string{"path": winPath, "method": "wmi", "err": werr.Error(), "detail": strings.TrimSpace(out)})
		// werr covers a hard launch failure AND the invisible-session-0 case
		// (exit 12) — both mean the user would see nothing, so try explorer.
		log.Printf("update: WMI installer launch unusable (%v): %s — falling back to explorer",
			werr, strings.TrimSpace(out))
		if result, ferr := launchViaExplorer(winPath, true, psExe, lg); ferr != nil {
			return launchResult{}, fmt.Errorf("could not launch installer %q — WMI failed (%v) and explorer fallback failed (%v); open the file manually to finish updating",
				winPath, werr, ferr)
		} else {
			log.Printf("update: launched shell installer via explorer fallback")
			return result, nil
		}
	}
	result := parseLaunchResult("wmi", out)
	lg.event("verify", map[string]string{"path": winPath, "method": "wmi", "pid": result.PID, "session": result.Session})
	advisoryVerify(winPath, true, psExe, lg)
	log.Printf("update: WMI shell installer launch verified (%s)", strings.TrimSpace(out))
	return result, nil
}

func parseLaunchResult(method, out string) launchResult {
	r := launchResult{Method: method}
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "pid=") {
			r.PID = strings.TrimPrefix(field, "pid=")
		}
		if strings.HasPrefix(field, "session=") {
			r.Session = strings.TrimPrefix(field, "session=")
		}
	}
	return r
}

func advisoryVerify(winPath string, fromWSL bool, psExe string, lg *shellLaunchLog) {
	pid, session, err := verifyInstallerVisible(winPath, fromWSL, psExe)
	fields := map[string]string{"path": winPath, "method": "wmi-advisory", "pid": pid, "session": session}
	if err != nil {
		fields["err"] = err.Error()
	}
	lg.event("verify", fields)
}

// runWMICourier fires the verified WMI Win32_Process.Create courier (see
// wmiCreateCommand) and returns its combined output + error. fromWSL routes
// through the repaired WSL interop env and uses the breakaway courier attr; the
// native courier inherits the job (no breakaway). Bounded by a 30s context so a
// wedged interop call can't hang the apply request.
func runWMICourier(winPath string, fromWSL bool, psExe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wmi := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-Command", wmiCreateCommand(winPath))
	if fromWSL {
		// Repair a stale WSL_INTEROP before launching. The inherited socket dies
		// with the wsl.exe session that created it, so a long-lived/systemd daemon
		// holds a dead value and Windows launches fail with "accept4 failed 110".
		wmi.Env = wslInteropEnv(psExe)
		wmi.SysProcAttr = shellInstallerSysProcAttr()
	} else {
		wmi.SysProcAttr = successorSysProcAttr()
	}
	out, werr := wmi.CombinedOutput()
	return string(out), werr
}

// launchViaExplorer ShellExecutes winPath via explorer.exe — the user's shell —
// which runs the target interactively (visible window + UAC) and outside any Job
// Object we own (the installer becomes the shell's child, not our descendant).
// explorer's own exit code is unreliable (it hands the path to the running
// shell and exits), so success requires a separately observed installer/UAC
// process. Primary path on native Windows; fallback on WSL.
func launchViaExplorer(winPath string, fromWSL bool, psExe string, lg *shellLaunchLog) (launchResult, error) {
	explorer, err := resolveExplorer(fromWSL)
	if err != nil {
		return launchResult{}, err
	}
	lg.event("dispatch", map[string]string{"path": winPath, "method": "explorer"})
	cmd := exec.Command(explorer, winPath)
	if fromWSL {
		cmd.Env = wslInteropEnv(psExe)
	}
	cmd.SysProcAttr = shellInstallerSysProcAttr()
	if err := cmd.Start(); err != nil {
		lg.event("verify", map[string]string{"path": winPath, "method": "explorer", "err": err.Error(), "detail": "dispatch failed"})
		return launchResult{}, err
	}
	go func() { _ = waitOwnedCommand(cmd) }()
	pid, session, err := verifyInstallerVisible(winPath, fromWSL, psExe)
	if err != nil {
		lg.event("verify", map[string]string{"path": winPath, "method": "explorer", "err": err.Error()})
		return launchResult{}, err
	}
	lg.event("verify", map[string]string{"path": winPath, "method": "explorer", "pid": pid, "session": session})
	return launchResult{Method: "explorer", PID: pid, Session: session}, nil
}

func resolveExplorer(fromWSL bool) (string, error) {
	if fromWSL {
		if _, serr := os.Stat("/mnt/c/Windows/explorer.exe"); serr == nil {
			return "/mnt/c/Windows/explorer.exe", nil
		} else if p, lerr := exec.LookPath("explorer.exe"); lerr == nil {
			return p, nil
		} else {
			return "", fmt.Errorf("explorer.exe not found for launch")
		}
	}
	// Native Windows: prefer PATH, fall back to the canonical Windows path for
	// locked-down PATHs (mirrors resolvePowerShell's robustness).
	if p, lerr := exec.LookPath("explorer.exe"); lerr == nil {
		return p, nil
	}
	if _, serr := os.Stat(`C:\Windows\explorer.exe`); serr == nil {
		return `C:\Windows\explorer.exe`, nil
	}
	return "", fmt.Errorf("explorer.exe not found for launch")
}

// verifyInstallerVisible polls Windows process state in one bounded PowerShell
// invocation. Session 0 is explicitly rejected because its UI is invisible to
// the signed-in user. consent.exe counts as confirmation: the installer is at
// the UAC boundary and launching it again would create a duplicate prompt.
func verifyInstallerVisible(winPath string, fromWSL bool, psExe string) (pid, session string, err error) {
	// A full 20-pass verifier can take roughly 20s on Windows because each
	// pass performs two CIM queries in addition to its 500ms sleep. Keep the
	// outer guard comfortably beyond that real runtime so exit 20/21 remains
	// reachable instead of being replaced by a misleading context timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-Command", verifyInstallerCommand(winPath))
	if fromWSL {
		cmd.Env = wslInteropEnv(psExe)
		cmd.SysProcAttr = shellInstallerSysProcAttr()
	} else {
		cmd.SysProcAttr = successorSysProcAttr()
	}
	out, runErr := cmd.CombinedOutput()
	detail := strings.TrimSpace(string(out))
	if runErr == nil {
		result := parseLaunchResult("verify", detail)
		return result.PID, result.Session, nil
	}
	if ctx.Err() != nil {
		return "", "", fmt.Errorf("%w: verification timed out: %v", errLaunchNotObserved, ctx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		switch exitErr.ExitCode() {
		case 20:
			return "", "", fmt.Errorf("%w (exit 20): %s", errLaunchNotObserved, detail)
		case 21:
			return "", "0", fmt.Errorf("%w (exit 21): %s", errLaunchInvisible, detail)
		}
	}
	return "", "", fmt.Errorf("installer launch verification failed: %w (%s)", runErr, detail)
}

func verifyInstallerCommand(winPath string) string {
	processName := filepath.Base(strings.ReplaceAll(winPath, `\`, "/"))
	processName = strings.ReplaceAll(processName, "'", "''")
	return fmt.Sprintf(
		"$seen0=$false; for($i=0; $i -lt 20; $i++) { "+
			"$all=@(Get-CimInstance Win32_Process -Filter \"Name='%s'\" -ErrorAction SilentlyContinue); "+
			"$p=$all | Where-Object { $_.SessionId -ne 0 } | Select-Object -First 1; "+
			"if($p) { [Console]::Out.WriteLine('pid=' + $p.ProcessId + ' session=' + $p.SessionId); exit 0 }; "+
			"$consent=Get-CimInstance Win32_Process -Filter \"Name='consent.exe'\" -ErrorAction SilentlyContinue | Select-Object -First 1; "+
			"if($consent) { [Console]::Out.WriteLine('pid=' + $consent.ProcessId + ' session=uac'); exit 0 }; "+
			"if($all | Where-Object { $_.SessionId -eq 0 }) { $seen0=$true }; Start-Sleep -Milliseconds 500 }; "+
			"if($seen0) { [Console]::Error.WriteLine('installer observed only in SessionId 0'); exit 21 }; "+
			"[Console]::Error.WriteLine('installer process was not observed'); exit 20",
		processName,
	)
}

// revealStagedInstaller opens Explorer with the staged file selected. It never
// accepts a caller-provided path; only the last path produced by ApplyShell can
// be revealed.
func (u *Updater) revealStagedInstaller() (string, error) {
	u.mu.Lock()
	stagedPath := ""
	if u.status.ShellLaunch != nil {
		stagedPath = u.status.ShellLaunch.StagedPath
	}
	lg := u.shellLaunchLog
	shellPlatform := u.shellPlatform
	u.mu.Unlock()
	if stagedPath == "" {
		return "", errors.New("no staged installer path is available")
	}
	fromWSL := runtime.GOOS == "linux" && isInsideWSL() && strings.HasPrefix(shellPlatform, "windows-")
	explorer, err := resolveExplorer(fromWSL)
	if err != nil {
		lg.event("reveal", map[string]string{"path": stagedPath, "method": "explorer", "err": err.Error()})
		return stagedPath, fmt.Errorf("could not show staged installer %s: %w", stagedPath, err)
	}
	cmd := exec.Command(explorer, "/select,"+stagedPath)
	if fromWSL {
		psExe, psErr := resolvePowerShell()
		if psErr != nil {
			lg.event("reveal", map[string]string{"path": stagedPath, "method": "explorer", "err": psErr.Error()})
			return stagedPath, fmt.Errorf("could not show staged installer %s: %w", stagedPath, psErr)
		}
		cmd.Env = wslInteropEnv(psExe)
	}
	cmd.SysProcAttr = shellInstallerSysProcAttr()
	if err := cmd.Start(); err != nil {
		lg.event("reveal", map[string]string{"path": stagedPath, "method": "explorer", "err": err.Error()})
		return stagedPath, fmt.Errorf("could not show staged installer %s: %w", stagedPath, err)
	}
	go func() { _ = waitOwnedCommand(cmd) }()
	lg.event("reveal", map[string]string{"path": stagedPath, "method": "explorer"})
	return stagedPath, nil
}

// wmiCreateCommand builds the PowerShell that asks WMI to create the
// installer process outside our job. Single-quoted PS string: escape any
// embedded ' by doubling. Paths from our dataDir don't contain quotes in
// practice, but be defensive against weird usernames.
func wmiCreateCommand(winPath string) string {
	psPath := strings.ReplaceAll(winPath, "'", "''")
	// Create the installer process via WMI and VERIFY it both worked AND is
	// visible to the user:
	//   - Invoke-CimMethod returns ReturnValue 0 + a non-zero ProcessId only on a
	//     real launch (exit 10/11 otherwise). The old command ignored both, so a
	//     failed Create still exited 0 and left the user with a stuck "installer
	//     launched" prompt and no installer.
	//   - Win32_Process.Create can also "succeed" into the non-interactive
	//     services desktop (SessionId 0), where the installer's window + UAC are
	//     invisible to the logged-in user — the process exists (valid PID,
	//     ReturnValue 0) yet nothing ever appears. We detect that, kill the
	//     orphaned invisible process, and exit 12 so launchWindowsInstaller falls
	//     back to a visible explorer launch. A process we can no longer find
	//     (already elevated / re-spawned by the installer) is treated as a normal
	//     visible launch, so this never causes a false fallback.
	return fmt.Sprintf(
		"$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='%s'}; "+
			"if ($r.ReturnValue -ne 0) { [Console]::Error.WriteLine('Win32_Process.Create ReturnValue=' + $r.ReturnValue); exit 10 }; "+
			"if (-not $r.ProcessId) { [Console]::Error.WriteLine('Win32_Process.Create returned no ProcessId'); exit 11 }; "+
			"Start-Sleep -Milliseconds 350; "+
			"$p = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $r.ProcessId) -ErrorAction SilentlyContinue; "+
			"if ($p -and $p.SessionId -eq 0) { [Console]::Error.WriteLine('Win32_Process.Create spawned into non-interactive session 0 (invisible); pid=' + $r.ProcessId); Stop-Process -Id $r.ProcessId -Force -ErrorAction SilentlyContinue; exit 12 }; "+
			"$s = 'na'; if ($p) { $s = $p.SessionId }; "+
			"[Console]::Out.WriteLine('pid=' + $r.ProcessId + ' session=' + $s); exit 0",
		psPath,
	)
}

// resolvePowerShell finds powershell.exe, falling back to its canonical
// install path when PATH is stripped (locked-down or systemd-launched
// environments). Handles both the native-Windows path and the WSL /mnt/c
// view so the WSL and native callers share one lookup.
func resolvePowerShell() (string, error) {
	const psExe = "powershell.exe"
	if _, err := exec.LookPath(psExe); err == nil {
		return psExe, nil
	}
	for _, fb := range []string{
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe",
	} {
		if _, err := os.Stat(fb); err == nil {
			return fb, nil
		}
	}
	return "", fmt.Errorf("powershell.exe not found in PATH or canonical install location")
}

// wslInteropEnv returns the process environment with WSL_INTEROP pointed
// at a socket that can actually launch Windows processes. The inherited
// WSL_INTEROP goes stale when the wsl.exe session that created it ends
// (the socket file lingers but its server is gone), so a long-lived or
// systemd-started daemon launching a Windows .exe fails with
// "accept4 failed 110". If no live socket is found we return the
// environment unchanged — best effort, since the inherited one might
// still happen to be alive.
func wslInteropEnv(probeExe string) []string {
	if sock := liveWSLInterop(probeExe); sock != "" {
		return envWithInterop(os.Environ(), sock)
	}
	return os.Environ()
}

// liveWSLInterop returns the first WSL interop socket that can launch a
// Windows process, trying in order: the inherited WSL_INTEROP, the init's
// master socket (/run/WSL/1_interop, which outlives individual sessions),
// then every other /run/WSL/*_interop. Returns "" if none work.
func liveWSLInterop(probeExe string) string {
	var candidates []string
	if cur := os.Getenv("WSL_INTEROP"); cur != "" {
		candidates = append(candidates, cur)
	}
	candidates = append(candidates, "/run/WSL/1_interop")
	if entries, err := os.ReadDir("/run/WSL"); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), "_interop") {
				candidates = append(candidates, filepath.Join("/run/WSL", e.Name()))
			}
		}
	}
	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if interopWorks(probeExe, c) {
			return c
		}
	}
	return ""
}

// interopWorks reports whether launching a trivial Windows process via
// probeExe succeeds with WSL_INTEROP=sock. A dead socket fails fast.
func interopWorks(probeExe, sock string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probeExe, "-NoProfile", "-NonInteractive", "-Command", "exit 0")
	cmd.Env = envWithInterop(os.Environ(), sock)
	return cmd.Run() == nil
}

// envWithInterop returns env with WSL_INTEROP set to sock, replacing any
// existing entry (or appending if absent).
func envWithInterop(env []string, sock string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "WSL_INTEROP=") {
			out = append(out, "WSL_INTEROP="+sock)
			replaced = true
		} else {
			out = append(out, kv)
		}
	}
	if !replaced {
		out = append(out, "WSL_INTEROP="+sock)
	}
	return out
}
