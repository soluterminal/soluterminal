//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// envCleanedSentinel marks an environment that has already been through the
// self-clean re-exec, so maybeReexecCleanEnv runs at most once per lineage.
const envCleanedSentinel = "SOLU_ENV_CLEANED"

// maybeReexecCleanEnv re-execs the daemon ONCE with the inherited Claude Code
// session env stripped, when the daemon was launched from inside a `claude`
// session (so its own environ carries CLAUDE_CODE_CHILD_SESSION / SESSION_ID).
// The clean image then decides, after flag parsing, whether to leave the
// claude session too (maybeDetachFromClaudeSession).
//
// Why a re-exec rather than only fixing spawn sites: the dirty env would
// otherwise (1) be a foot-gun for any future code path that execs `claude`
// before the agent-level strip, and (2) propagate to every successor daemon
// across auto-updates (spawnSuccessor inherits os.Environ()). Re-exec'ing the
// current process clean kills both at the source.
//
// Safety: this runs in main() AFTER subcommand dispatch (so `solud agent` /
// `solud router`, which legitimately run under the daemon env and strip
// per-command, are never re-exec'd) and BEFORE any port bind or state load, so
// there is no listener handoff and no in-memory state to lose. Pane agents are
// detached (Setsid) and survive the exec, reattaching exactly as they do after
// an auto-update. syscall.Exec replaces the process image in place (same pid,
// same args). It only returns on failure, in which case we log and continue
// running with the dirty env (fail-open: a degraded daemon beats no daemon).
func maybeReexecCleanEnv() {
	// Nothing to clean, or we already cleaned this lineage.
	if os.Getenv(childSessionEnvKey) == "" && os.Getenv("CLAUDECODE") == "" {
		return
	}
	if os.Getenv(envCleanedSentinel) == "1" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		log.Printf("self-clean: os.Executable failed, continuing with inherited env: %v", err)
		return
	}
	// The clean image decides, once its flags are parsed, whether to leave the
	// claude session (maybeDetachFromClaudeSession). Detaching here, before
	// flag parsing, would swallow the output of one-shot runs (--claim, -h,
	// a bad flag or a mistyped subcommand) and exit 0.
	env := append(stripInheritedClaudeEnv(os.Environ()), envCleanedSentinel+"=1", detachPendingEnv+"=1")
	log.Printf("self-clean: re-exec to drop inherited CLAUDE_CODE_* env (was launched inside a claude session)")
	if err := syscall.Exec(self, os.Args, env); err != nil {
		log.Printf("self-clean: re-exec failed, continuing with inherited env: %v", err)
	}
}

// detachPendingEnv marks the clean image of a daemon that was started inside a
// claude session. maybeDetachFromClaudeSession consumes it before anything
// can spawn a child, so no pane or update successor inherits it.
const detachPendingEnv = "SOLU_DETACH_FROM_CLAUDE_SESSION"

// detachEarlyExitWait is how long the starter waits for the detached daemon
// to fail at once (another daemon already owns the data directory, or the
// port is taken) before it reports success.
var detachEarlyExitWait = 3 * time.Second

// maybeDetachFromClaudeSession runs after flag parsing and the one-shot paths
// (--claim, usage errors), just before the daemon claims its data directory.
// A daemon started inside a claude session shares that session, process group
// and terminal, so when the claude session ends (its terminal hangs up, or it
// kills its process group) the daemon dies with it and nothing restarts it
// (ST-118). The primary daemon continues instead as a copy of itself in a
// session of its own; this process waits briefly for an early failure, then
// exits. Anything else stays in place (detachFromClaudeSession).
func maybeDetachFromClaudeSession(addr, dataDir, backendID, shellVersion string) {
	if os.Getenv(detachPendingEnv) != "1" {
		return
	}
	_ = os.Unsetenv(detachPendingEnv)
	if !detachFromClaudeSessionFn(os.Getenv, runtime.GOOS, addr, dataDir, backendID, shellVersion) {
		return
	}
	self, err := os.Executable()
	if err != nil {
		log.Printf("detach: os.Executable failed, staying in the claude session: %v", err)
		return
	}
	cmd, err := startDetachedCleanSuccessor(self, os.Args[1:], os.Environ())
	if err != nil {
		log.Printf("detach: could not start the detached daemon, staying in the claude session: %v", err)
		return
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 1
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() > 0 {
			code = exitErr.ExitCode()
		} else if err == nil {
			code = 0
		}
		log.Printf("detach: the detached daemon (pid %d) exited at once (status %d); see solud.log", pid, code)
		fmt.Fprintf(os.Stderr, "solud: the daemon exited at once (status %d); see solud.log for why\n", code)
		os.Exit(code)
	case <-time.After(detachEarlyExitWait):
	}
	log.Printf("detach: launched inside a claude session; continuing as pid %d in a session of its own, so the daemon outlives the claude session", pid)
	os.Exit(0)
}

// detachFromClaudeSessionFn is a test seam.
var detachFromClaudeSessionFn = detachFromClaudeSession

// detachFromClaudeSession decides whether the daemon leaves the claude session
// it was started in. Only the PRIMARY daemon (canonical port, default data
// dir: the one the app and every pane rely on) detaches. A side daemon
// started inside a claude session is almost always a test or dev daemon whose
// starter tracks its pid and stops it; detaching that would orphan it. A
// systemd service (INVOCATION_ID) stays put: systemd tracks its main pid, and
// so does a desktop-started daemon (--backend-id or --shell-version).
func detachFromClaudeSession(getenv func(string) string, goos, addr, dataDir, backendID, shellVersion string) bool {
	if getenv("INVOCATION_ID") != "" || backendID != "" || shellVersion != "" {
		return false
	}
	return decideDaemonRole(goos, addr, dataDir, defaultDataDir()).Primary
}

// startDetachedCleanSuccessor starts the daemon in a new session with no
// controlling terminal and its stdio off the claude session's terminal (it
// writes its durable log itself). Pane agents are detached already and
// reattach as after an auto-update.
func startDetachedCleanSuccessor(self string, args, env []string) (*exec.Cmd, error) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()
	cmd := exec.Command(self, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	cmd.SysProcAttr = successorSysProcAttr() // Setsid: new session, no controlling terminal
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
