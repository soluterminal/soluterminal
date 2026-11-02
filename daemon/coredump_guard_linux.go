//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	gopty "github.com/aymanbagabas/go-pty"
)

const coreDumpGuardEnv = "SOLU_CORE_DUMP_GUARD"
const coreDumpGuardExecArg = "__solu_internal_core_dump_guard_exec_v1"

// coreDumpGuardExecError carries the helper's execve errno back to the daemon.
// The report pipe exists because the guard moves execve into the /proc/self/exe
// helper child; without it the kernel cause would be swallowed and an invalid
// target image could look like a successful launch. Unwrap exposes both the
// bare errno (so errors.Is(err, syscall.ENOEXEC) holds) and the helper's
// *exec.ExitError.
type coreDumpGuardExecError struct {
	Errno syscall.Errno
	Exit  *exec.ExitError
}

func (e *coreDumpGuardExecError) Error() string {
	return fmt.Sprintf("execve failed: %s", e.Errno)
}

// Unwrap omits a nil Exit: a typed-nil *exec.ExitError would satisfy errors.As
// and hand callers a nil pointer.
func (e *coreDumpGuardExecError) Unwrap() []error {
	if e.Exit == nil {
		return []error{e.Errno}
	}
	return []error{e.Errno, e.Exit}
}

func init() {
	if len(os.Args) >= 4 && os.Args[1] == coreDumpGuardExecArg {
		reportFD, err := strconv.Atoi(os.Args[2])
		if err != nil {
			reportFD = -1
		}
		os.Exit(runCoreDumpGuardExec(os.Args[3], os.Args[4:], reportFD))
	}
}

// runCoreDumpGuardExec runs inside the /proc/self/exe helper child. The parent
// passes the write end of a private pipe on reportFD; CloseOnExec makes a
// successful execve close it (the parent's read then returns EOF) while a
// failed execve that is not a valid image reports ENOEXEC so the daemon can
// surface the kernel cause. Other errnos keep the established child-setup
// contract where the helper's exit-125 diagnostic is the observable surface.
func runCoreDumpGuardExec(target string, argv []string, reportFD int) int {
	execTarget := func(target string, argv, env []string) error {
		if reportFD >= 0 {
			syscall.CloseOnExec(reportFD)
		}
		err := syscall.Exec(target, argv, env)
		if err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) && errno == syscall.ENOEXEC && reportFD >= 0 {
				_, _ = syscall.Write(reportFD, []byte(strconv.FormatUint(uint64(errno), 10)))
			}
		}
		return err
	}
	return coreDumpGuardExec(target, argv, os.Environ(), os.Stderr, syscall.Getrlimit, syscall.Setrlimit, execTarget)
}

// Dependencies are passed as values for deterministic failure-path tests; no
// mutable hooks or fault-injection environment variables enter the daemon.
func coreDumpGuardExec(target string, argv, env []string, stderr io.Writer,
	getLimit func(int, *syscall.Rlimit) error, setLimit func(int, *syscall.Rlimit) error,
	execTarget func(string, []string, []string) error) int {
	fail := func(op string, err error) int {
		fmt.Fprintln(stderr, &coreDumpGuardError{Executable: target, Op: op, Err: err})
		return coreDumpGuardExitCode
	}
	if len(argv) == 0 {
		argv = []string{target}
	}
	var orig syscall.Rlimit
	if err := getLimit(syscall.RLIMIT_CORE, &orig); err != nil {
		return fail("getrlimit RLIMIT_CORE", err)
	}
	if orig.Max < 1 {
		return fail("RLIMIT_CORE", fmt.Errorf("hard limit %d cannot support soft=1; disable %s only for intentional debugging", orig.Max, coreDumpGuardEnv))
	}
	if orig.Cur != 1 {
		guard := syscall.Rlimit{Cur: 1, Max: orig.Max}
		if err := setLimit(syscall.RLIMIT_CORE, &guard); err != nil {
			return fail("setrlimit RLIMIT_CORE soft=1", err)
		}
	}
	if err := execTarget(target, argv, env); err != nil {
		return fail("exec", err)
	}
	return 0
}

func coreDumpGuardActive() bool {
	return coreDumpGuardPolicy(os.Getenv(coreDumpGuardEnv), isInsideWSL)
}

func coreDumpGuardPolicy(override string, insideWSL func() bool) bool {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return insideWSL()
}

// startWithoutCoreDumps starts cmd with a soft RLIMIT_CORE of exactly 1.
//
// Linux rlimits are per-process (shared by all threads), inherited across fork,
// and preserved across execve. The guarded limit therefore reaches every
// descendant, including the node -> Playwright -> browser grandchild tree.
// core(5) notes that RLIMIT_CORE is normally ignored for piped core patterns,
// but fs/coredump.c's do_coredump path has one special escape hatch: a limit of
// exactly 1 aborts a pipe dump. The kernel gives its own pipe helper that value
// to prevent recursive dumps. A limit of 1 is also below binfmt min_coredump,
// so file-pattern hosts write nothing. Zero does not suppress a piped dump, and
// PR_SET_DUMPABLE is unsuitable because execve resets it.
//
// The daemon's own limits never change. The command starts through the private
// /proc/self/exe helper mode. That child sets only its own soft limit
// (Cur:1, Max:orig.Max) and then
// execs the requested command, so concurrent unguarded forks cannot inherit a
// temporary process-wide daemon limit. The hard limit is never lowered, and
// the child can raise its own soft limit back to that untouched hard limit.
// PR_SET_DUMPABLE is deliberately not used. This was also verified empirically
// on WSL2 kernel 6.18 on 2026-08-27: guarded child and grandchild SIGSEGVs had
// WCOREDUMP=false and created no wsl-crashes file.
//
// The guard is active only inside WSL by default; on native non-WSL Linux it is
// a no-op unless coreDumpGuardEnv forces it on, so extending it to the seams
// below does not change native-Linux or user-chosen debugging behavior.
//
// Guarded daemon-owned launch paths:
//   - workflow browser/Playwright/preview/verify workers (workflow_screens.go,
//     preview_spawn.go, workflow_verify.go)
//   - workflow agent runtime clients (workflow_runtime_{claude,codex,muse,gemini,
//     deepseek_harness}.go, agents_run.go, schedules_agent.go)
//   - the interactive/native-harness `solud agent` wrapper and every descendant
//     (pane_spawn.go), including the real agent PTY/pipe child (agent.go)
//   - the managed Node harness installer (harness_api.go)
//   - native muse `muse exec` turns (muse_driver.go)
//   - scheduled headless task runs (schedules_task.go)
//   - optional features' headless runs
//   - workflow/delegated capability probes, npm-global Playwright discovery,
//     and optional features' Claude MCP registration commands
//
// Only Path and Args are adapted, on the original command object. This keeps
// its context, cancellation, WaitDelay, descriptors and process attributes.
func startWithoutCoreDumps(cmd interface{ Start() error }) error {
	if command, ok := cmd.(*exec.Cmd); ok {
		return startInOwnedProcessScope(command, func() error {
			return startWithCoreDumpGuard(cmd, coreDumpGuardActive(), syscall.Getrlimit, exec.LookPath)
		})
	}
	return startWithCoreDumpGuard(cmd, coreDumpGuardActive(), syscall.Getrlimit, exec.LookPath)
}

func startWithCoreDumpGuard(cmd interface{ Start() error }, active bool,
	getLimit func(int, *syscall.Rlimit) error, lookPath func(string) (string, error)) error {
	target, argv, dir, adaptErr := commandCoreDumpSpec(cmd)
	fail := func(op string, err error) error {
		guardErr := &coreDumpGuardError{Executable: target, Op: op, Err: err}
		// Preserve Start's cleanup of descriptors created by the pipe APIs.
		// A forced Err makes os/exec return before fork: the target cannot run.
		if c, ok := cmd.(*exec.Cmd); ok && c != nil && c.Process == nil {
			prior := c.Err
			c.Err = guardErr
			_ = c.Start()
			c.Err = prior
		}
		return guardErr
	}
	if adaptErr != nil {
		return fail("command adaptation", adaptErr)
	}
	if !active {
		return cmd.Start()
	}
	if c, ok := cmd.(*exec.Cmd); ok && c.Err != nil {
		return fail("LookPath", c.Err)
	}
	var orig syscall.Rlimit
	if err := getLimit(syscall.RLIMIT_CORE, &orig); err != nil {
		return fail("getrlimit RLIMIT_CORE", err)
	}
	if orig.Max < 1 {
		return fail("RLIMIT_CORE", fmt.Errorf("hard limit %d cannot support soft=1", orig.Max))
	}
	if len(argv) == 0 {
		argv = []string{target}
	}
	// exec.Cmd.Path is already resolved by exec.Command, and relative paths are
	// evaluated in Dir. go-pty instead calls exec.Command at Start, so resolve a
	// bare PTY name using the caller's PATH before installing the helper.
	resolved := target
	if _, pty := cmd.(*gopty.Cmd); pty && !strings.ContainsRune(target, '/') {
		var err error
		resolved, err = lookPath(target)
		if err != nil {
			return fail("LookPath", err)
		}
	} else {
		check := target
		if !filepath.IsAbs(check) {
			check = filepath.Join(dir, check)
		}
		// Prefix bare relative paths to retain exec.Cmd's Dir-relative semantics.
		if !strings.ContainsRune(check, '/') {
			check = "./" + check
		}
		if _, err := lookPath(check); err != nil {
			return fail("LookPath", err)
		}
	}
	// The helper execs the target, so its execve errno is otherwise invisible to
	// this process. A *exec.Cmd carries a private report pipe on the next
	// ExtraFiles descriptor (os/exec maps ExtraFiles[i] to fd 3+i); go-pty has no
	// such API, so it keeps reportFD = -1 and the exit-125 diagnostic surface.
	reportFD := -1
	var reportR, reportW *os.File
	var oldExtra []*os.File
	if c, ok := cmd.(*exec.Cmd); ok {
		oldExtra = c.ExtraFiles
		var err error
		reportR, reportW, err = os.Pipe()
		if err != nil {
			return fail("report pipe", err)
		}
		reportFD = 3 + len(oldExtra)
		c.ExtraFiles = append(append([]*os.File(nil), oldExtra...), reportW)
	}
	helpArgs := append([]string{argv[0], coreDumpGuardExecArg, strconv.Itoa(reportFD), resolved}, argv...)
	switch c := cmd.(type) {
	case *exec.Cmd:
		c.Path, c.Args = "/proc/self/exe", helpArgs
	case *gopty.Cmd:
		c.Path, c.Args = "/proc/self/exe", helpArgs
	}
	startErr := cmd.Start()
	if c, ok := cmd.(*exec.Cmd); ok {
		c.ExtraFiles = oldExtra
	}
	if reportW != nil {
		reportW.Close()
	}
	if startErr != nil {
		if reportR != nil {
			reportR.Close()
		}
		return fail("start", startErr)
	}
	c, ok := cmd.(*exec.Cmd)
	if !ok {
		return nil
	}
	// A successful exec closes the child's write end, so ReadAll returns EOF
	// with no bytes. A getrlimit/setrlimit failure produces no report either and
	// keeps the helper's exit-125 diagnostic surface.
	report, readErr := io.ReadAll(reportR)
	reportR.Close()
	// The caller never Waits after an error, so reap the started helper here.
	abandon := func(op string, err error) error {
		_ = c.Process.Kill()
		_ = c.Wait()
		return fail(op, err)
	}
	if readErr != nil {
		return abandon("report", readErr)
	}
	if len(report) == 0 {
		return nil
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(string(report)))
	if parseErr != nil {
		return abandon("report", parseErr)
	}
	errno := syscall.Errno(value)
	if errno != syscall.ENOEXEC {
		return nil
	}
	var exitErr *exec.ExitError
	if waitErr := c.Wait(); waitErr != nil {
		_ = errors.As(waitErr, &exitErr)
	}
	return &coreDumpGuardError{Executable: target, Op: "exec", Err: &coreDumpGuardExecError{Errno: errno, Exit: exitErr}}
}
