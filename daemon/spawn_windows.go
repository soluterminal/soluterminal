//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process-management helpers. Together they provide the
// equivalent of Unix's `Setsid` (agent outlives daemon) plus
// process-group kill semantics for the agent's PTY child tree.
//
// Model:
//   - Daemon spawns agent with CREATE_BREAKAWAY_FROM_JOB so the
//     agent is NOT in the daemon's job. Agent survives daemon death.
//   - Agent on startup creates its OWN Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assigns itself to it.
//     Every child the agent spawns (the PTY command) inherits this
//     job, so when the agent dies the Job closes and all descendants
//     (shells, Claude, editors, whatever) die cleanly.
//   - Single-process kill works via TerminateProcess; tree kill
//     happens automatically via the Job Object teardown.

const (
	// CreateProcess flags. Defined in processthreadsapi.h; naming them
	// inline so we don't pull the x/sys constants just for these.
	detachedProcess        uint32 = 0x00000008
	createNewProcessGroup  uint32 = 0x00000200
	createNoWindow         uint32 = 0x08000000
	createBreakawayFromJob uint32 = 0x01000000
)

// detachedSysProcAttr: DETACHED_PROCESS (no inherited console) +
// CREATE_NEW_PROCESS_GROUP (so Ctrl+C to daemon doesn't cascade) +
// CREATE_BREAKAWAY_FROM_JOB (so the spawned agent is NOT in the
// daemon's Job Object — it creates its own, and outlives us).
//
// If the daemon's parent Job Object doesn't allow breakaway
// (JOB_OBJECT_LIMIT_BREAKAWAY_OK not set on it), CreateProcess
// returns ERROR_ACCESS_DENIED. That's rare in practice; if a user
// hits it we'd log + retry without the flag (agent would then die
// with daemon, but spawn would still succeed).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
	}
}

// successorSysProcAttr returns CreateProcess flags for the self-update
// successor process. Crucially we do NOT set CREATE_BREAKAWAY_FROM_JOB
// here — we WANT the successor to inherit Tauri's Job Object (via
// process tree inheritance from us, the outgoing daemon, which is
// already in the job) so it dies cleanly when the user quits Tauri.
// DETACHED_PROCESS still suppresses the console window flash.
// CREATE_NEW_PROCESS_GROUP still isolates Ctrl+C handling.
func successorSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

// shellInstallerSysProcAttr returns CreateProcess flags for launching
// a Tauri/NSIS shell installer. Unlike daemon self-update successors,
// the installer must outlive the Tauri Job Object: NSIS closes the app
// during upgrade, and if the installer remains in that job Windows can
// terminate it after the old app is uninstalled but before install runs.
func shellInstallerSysProcAttr() *syscall.SysProcAttr {
	return detachedSysProcAttr()
}

func applyDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = detachedSysProcAttr()
}

// applyNoWindow suppresses console flashes for background commands that still
// need stdout/stderr pipes, such as `claude --version`. It keeps any attributes
// already set; Windows ignores CREATE_NO_WINDOW alongside DETACHED_PROCESS.
func applyNoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// applyDetachedNoBreakaway is the fallback for applyDetached when the
// daemon's parent Job Object forbids breakaway. It keeps DETACHED_PROCESS
// + CREATE_NEW_PROCESS_GROUP but drops CREATE_BREAKAWAY_FROM_JOB, so the
// agent is created INSIDE the parent job (it dies when the app quits)
// instead of CreateProcess failing with ERROR_ACCESS_DENIED. A pane that
// only lives as long as the app beats a pane that won't open at all.
func applyDetachedNoBreakaway(cmd *exec.Cmd) {
	cmd.SysProcAttr = successorSysProcAttr()
}

// isJobBreakawayDenied reports whether a spawn error is the
// ERROR_ACCESS_DENIED that CreateProcess returns when
// CREATE_BREAKAWAY_FROM_JOB is set but the parent Job Object lacks
// JOB_OBJECT_LIMIT_BREAKAWAY_OK — the case when the Tauri shell launched
// the daemon. When true, the caller retries via applyDetachedNoBreakaway.
func isJobBreakawayDenied(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}

// pipeChildSysProcAttr is a no-op on Windows: a --no-pty child inherits
// the agent's Job Object (KILL_ON_JOB_CLOSE) for tree cleanup and is
// killed directly by terminateChildTree's proc.Kill(). No process-group
// attribute is needed.
func pipeChildSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// terminatePid falls through to hardKillPid on Windows. There's no
// SIGTERM equivalent for arbitrary processes — the graceful path
// requires ConsoleCtrlEvent which needs a shared console handle.
// For the "clean up a PID we just spawned that didn't respond on
// its socket" use case (the pane.go code path), TerminateProcess
// is the right answer.
func terminatePid(pid int) error {
	return hardKillPid(pid)
}

func hardKillPid(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// processAlive: os.FindProcess on Windows always succeeds (returns
// a handle wrapper regardless of whether the pid exists), so we need
// a real existence probe. PROCESS_QUERY_LIMITED_INFORMATION is the
// cheapest handle that works without elevated privileges.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(h)
	return true
}

// initAgentProcess puts the agent (us) into its own Job Object with
// KILL_ON_JOB_CLOSE. This means:
//   - Every process the agent spawns (starting with the PTY child)
//     inherits this Job Object unless it explicitly breaks away.
//   - When the agent process ends for any reason — clean exit, crash,
//     TerminateProcess from outside — Windows closes the Job Object,
//     which triggers termination of every process still in it.
//
// This is our stand-in for Unix `Setsid`: on Unix the daemon Setsid's
// the agent, giving it its own session so it outlives daemon death;
// on Windows the daemon already used CREATE_BREAKAWAY_FROM_JOB to
// spawn us, so we're free-standing, and the Job Object here gives
// us the "my subtree dies with me" semantics.
//
// Failure here is non-fatal — without the Job Object the agent still
// works, we just lose guaranteed cleanup of grandchildren if the
// agent is killed abruptly. Log + continue.
func initAgentProcess() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}

	self, err := windows.GetCurrentProcess()
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("GetCurrentProcess: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, self); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	// Keep `job` handle alive for the lifetime of the agent process
	// by stashing it in a global. Closing the handle here is safe —
	// the Job Object persists as long as any process is assigned —
	// but retaining the handle lets a future enhancement inspect /
	// re-set limits. For v0, leak it; exit frees it anyway.
	agentJobHandle = job
	return nil
}

// agentJobHandle keeps the agent's Job Object handle alive. Set
// once by initAgentProcess; never closed explicitly (process exit
// releases it).
var agentJobHandle windows.Handle

// terminateChildTree asks the child tree to shut down. No graceful
// signal on Windows; this just kills the main child. The agent's
// Job Object cascades to grandchildren on agent exit, but for a
// targeted "FrameKillRequest, shut this pane down" we only hit the
// direct child — Windows doesn't provide a clean way to signal a
// process tree without complicated console setup.
func terminateChildTree(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	return proc.Kill()
}

// killChildTree force-terminates a fresh snapshot of the complete descendant
// tree, deepest children first, then the group leader. CREATE_NEW_PROCESS_GROUP
// has no Windows equivalent of kill(-pgid), so Toolhelp enumeration provides
// the bounded workflow timeout path with the same no-surviving-hooks property.
func killChildTree(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	pids := freshProcessTreeSnapshot().descendantPids(proc.Pid)
	var firstErr error
	for i := len(pids) - 1; i >= 0; i-- {
		if err := hardKillPid(pids[i]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// platformDefaultShell: Windows has no $SHELL convention. Try the
// modern PowerShell (pwsh.exe, installed by default on Win11) then
// the built-in PowerShell 5.x, then cmd.exe as the universal
// fallback. PATH lookup so we don't hardcode install paths.
func platformDefaultShell() string {
	for _, candidate := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p
		}
	}
	// Last resort — System32 always has cmd.exe even if PATH is nuked.
	return `C:\Windows\System32\cmd.exe`
}
