//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

func coreDumpGuardPreconditions(t *testing.T) syscall.Rlimit {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatalf("core-dump guard test requires sh: %v", err)
	}
	// The production default is WSL-only (see coreDumpGuardActive). Force the
	// guard on here so these seam tests exercise it deterministically on every
	// Linux host; TestCoreDumpGuardActive* pins the production default itself.
	t.Setenv(coreDumpGuardEnv, "1")
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		t.Fatalf("get RLIMIT_CORE: %v", err)
	}
	if limit.Max == 0 {
		t.Fatal("core-dump guard test requires a non-zero RLIMIT_CORE hard limit")
	}
	return limit
}

// raiseCoreSoftLimitForTest selects a non-1 starting policy inside an isolated
// test process, so the helper's Setrlimit path is exercised even when the test
// runner starts with soft=1. These processes never deliberately crash: for a
// piped core_pattern, soft=4 does NOT bound the size of a crash dump.
func raiseCoreSoftLimitForTest(t *testing.T) (orig, raised syscall.Rlimit) {
	t.Helper()
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &orig); err != nil {
		t.Fatalf("get RLIMIT_CORE: %v", err)
	}
	if orig.Max == 0 {
		t.Fatal("core-dump guard test requires a non-zero RLIMIT_CORE hard limit")
	}
	raised = syscall.Rlimit{Cur: 4, Max: orig.Max}
	if orig.Cur == raised.Cur {
		return orig, raised
	}
	if orig.Max < raised.Cur {
		t.Fatalf("RLIMIT_CORE hard limit %d is below the test soft limit %d", orig.Max, raised.Cur)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &raised); err != nil {
		t.Fatalf("cannot raise soft RLIMIT_CORE for the guard test: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_CORE, &orig) })
	return orig, raised
}

// TestCoreDumpGuardActiveHonorsExplicitOverride pins the WSL/debug-policy gate:
// an explicit override wins in both directions, and the default tracks
// isInsideWSL so native non-WSL Linux keeps its own core-dump policy.
func TestCoreDumpGuardActiveHonorsExplicitOverride(t *testing.T) {
	t.Setenv(coreDumpGuardEnv, "0")
	if coreDumpGuardActive() {
		t.Fatalf("guard active with explicit %s=0", coreDumpGuardEnv)
	}
	t.Setenv(coreDumpGuardEnv, "1")
	if !coreDumpGuardActive() {
		t.Fatalf("guard inactive with explicit %s=1", coreDumpGuardEnv)
	}
	t.Setenv(coreDumpGuardEnv, "")
	if got, want := coreDumpGuardActive(), isInsideWSL(); got != want {
		t.Fatalf("default guard active=%v, want WSL autodetection %v", got, want)
	}
}

// TestCoreDumpGuardOptOutLeavesInheritedLimit proves the debug-policy opt-out
// is a real no-op: with the guard disabled the child inherits the daemon's own
// soft limit instead of 1, and the daemon limit is untouched.
func TestCoreDumpGuardOptOutLeavesInheritedLimit(t *testing.T) {
	if isolateCoreLimitTest(t) {
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatalf("core-dump guard test requires sh: %v", err)
	}
	_, raised := raiseCoreSoftLimitForTest(t)
	t.Setenv(coreDumpGuardEnv, "0")

	cmd := exec.Command("sh", "-c", "grep 'Max core' /proc/self/limits")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatalf("start opted-out child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for opted-out child: %v", err)
	}
	soft, hard := coreLimitLine(t, stdout.String())
	if soft != procLimitValue(raised.Cur) || hard != procLimitValue(raised.Max) {
		t.Fatalf("opted-out child soft=%q hard=%q, want inherited soft=%q hard=%q", soft, hard, procLimitValue(raised.Cur), procLimitValue(raised.Max))
	}
	requireLimitUnchanged(t, raised)
}

// TestCoreDumpGuardSetsAndRestoresSoftLimit retains the original regression
// name, but now proves the parent stays at its chosen limit throughout the
// child-only helper's Setrlimit path.
func TestCoreDumpGuardSetsAndRestoresSoftLimit(t *testing.T) {
	if isolateCoreLimitTest(t) {
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Fatalf("core-dump guard test requires sh: %v", err)
	}
	_, raised := raiseCoreSoftLimitForTest(t)
	t.Setenv(coreDumpGuardEnv, "1")

	cmd := exec.Command("sh", "-c", "grep 'Max core' /proc/self/limits")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatalf("start guarded child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for guarded child: %v", err)
	}
	soft, hard := coreLimitLine(t, stdout.String())
	if soft != "1" || hard != procLimitValue(raised.Max) {
		t.Fatalf("guarded child soft=%q hard=%q, want soft=1 hard=%q", soft, hard, procLimitValue(raised.Max))
	}
	requireLimitUnchanged(t, raised)
}

type coreLimitObservingStarter struct {
	observed syscall.Rlimit
}

func (s *coreLimitObservingStarter) Start() error {
	return syscall.Getrlimit(syscall.RLIMIT_CORE, &s.observed)
}

// TestStartWithoutCoreDumpsDoesNotMutateParentAcrossStart pins the concurrency
// boundary: preparing a guarded child must never temporarily change the
// daemon's process-wide RLIMIT_CORE while Start is running. Any unrelated fork
// may execute concurrently, so changing the parent's limit even for the
// fork/exec window lets that unrelated child inherit the guard by accident.
func TestStartWithoutCoreDumpsDoesNotMutateParentAcrossStart(t *testing.T) {
	if isolateCoreLimitTest(t) {
		return
	}
	_, raised := raiseCoreSoftLimitForTest(t)
	t.Setenv(coreDumpGuardEnv, "1")

	starter := &coreLimitObservingStarter{}
	if err := startWithoutCoreDumps(starter); err == nil || !strings.Contains(err.Error(), coreDumpGuardDiagnostic) {
		t.Fatalf("unsupported command must fail closed: %v", err)
	}
	if starter.observed != (syscall.Rlimit{}) {
		t.Fatal("unsupported target was started")
	}
	requireLimitUnchanged(t, raised)
}

func parseCoreLimitLine(out string) (soft, hard string, err error) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Max core file size") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return "", "", fmt.Errorf("malformed Max core file size line %q", line)
		}
		return fields[4], fields[5], nil
	}
	return "", "", fmt.Errorf("no Max core file size line in %q", out)
}

func coreLimitLine(t *testing.T, out string) (soft, hard string) {
	t.Helper()
	soft, hard, err := parseCoreLimitLine(out)
	if err != nil {
		t.Fatal(err)
	}
	return soft, hard
}

func coreLimitLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Max core file size") {
			lines = append(lines, line)
		}
	}
	return lines
}

func procLimitValue(value uint64) string {
	if value == ^uint64(0) {
		return "unlimited"
	}
	return strconv.FormatUint(value, 10)
}

func requireLimitUnchanged(t *testing.T, before syscall.Rlimit) {
	t.Helper()
	var after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &after); err != nil {
		t.Fatalf("get restored RLIMIT_CORE: %v", err)
	}
	if after != before {
		t.Fatalf("RLIMIT_CORE after guarded spawn = %+v, want original %+v", after, before)
	}
}

func TestStartWithoutCoreDumpsInheritedByChildAndGrandchild(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	cmd := exec.Command("sh", "-c", `grep 'Max core' /proc/self/limits; sh -c "grep 'Max core' /proc/self/limits"`)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	applyDetached(cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatalf("start guarded child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for guarded child: %v", err)
	}

	lines := coreLimitLines(stdout.String())
	if len(lines) != 2 {
		t.Fatalf("Max core file size lines = %d, want child and grandchild: %q", len(lines), stdout.String())
	}
	wantHard := procLimitValue(before.Max)
	for i, line := range lines {
		soft, hard := coreLimitLine(t, line)
		if soft != "1" || hard != wantHard {
			t.Errorf("process %d limits soft=%q hard=%q, want soft=1 hard=%q", i, soft, hard, wantHard)
		}
	}
	requireLimitUnchanged(t, before)
}

func snapshotCrashDir(t *testing.T) (string, map[string]struct{}) {
	t.Helper()
	dir := os.Getenv("SOLU_TEST_WSL_CRASH_DIR")
	if dir == "" {
		return "", nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("SOLU_TEST_WSL_CRASH_DIR is not an existing directory: %q", dir)
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read WSL crash directory %q: %v", dir, err)
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = struct{}{}
	}
	return dir, names
}

func requireCrashDirUnchanged(t *testing.T, dir string, before map[string]struct{}) {
	t.Helper()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read WSL crash directory %q after crashes: %v", dir, err)
	}
	for _, entry := range entries {
		if _, ok := before[entry.Name()]; !ok {
			t.Errorf("guarded crash created %q in %s", entry.Name(), dir)
		}
	}
}

func TestStartWithoutCoreDumpsCrashDoesNotDumpCore(t *testing.T) {
	beforeLimit := coreDumpGuardPreconditions(t)
	if pattern, err := os.ReadFile("/proc/sys/kernel/core_pattern"); err == nil {
		t.Logf("kernel.core_pattern=%q", strings.TrimSpace(string(pattern)))
	} else {
		t.Logf("read kernel.core_pattern: %v", err)
	}
	crashDir, beforeEntries := snapshotCrashDir(t)

	crashWork := t.TempDir()
	cmd := exec.Command("sh", "-c", "kill -SEGV $$")
	cmd.Dir = crashWork
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatalf("start guarded crash: %v", err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("guarded crash error = %v, want *exec.ExitError", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("guarded crash status type = %T, want syscall.WaitStatus", exitErr.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGSEGV || status.CoreDump() {
		t.Fatalf("guarded crash status=%v signaled=%v signal=%v coreDump=%v", status, status.Signaled(), status.Signal(), status.CoreDump())
	}

	grandchild := exec.Command("sh", "-c", `sh -c 'kill -SEGV $$'; echo grandchild=$?`)
	grandchild.Dir = crashWork
	var stdout, stderr bytes.Buffer
	grandchild.Stdout = &stdout
	grandchild.Stderr = &stderr
	if err := startWithoutCoreDumps(grandchild); err != nil {
		t.Fatalf("start guarded grandchild crash: %v", err)
	}
	if err := grandchild.Wait(); err != nil {
		t.Fatalf("outer shell after grandchild crash = %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "grandchild=139") {
		t.Errorf("grandchild status output = %q, want grandchild=139", stdout.String())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "core dumped") {
		t.Errorf("grandchild crash unexpectedly reported a core dump: %q", stderr.String())
	}

	if entries, err := os.ReadDir(crashWork); err != nil || len(entries) != 0 {
		t.Fatalf("crash artifacts: %v, %v", entries, err)
	}
	requireCrashDirUnchanged(t, crashDir, beforeEntries)
	requireLimitUnchanged(t, beforeLimit)
}

func TestStartWithoutCoreDumpsConcurrentSpawnsAllGuarded(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	const workers = 8
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("sh", "-c", "grep 'Max core' /proc/self/limits")
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if err := startWithoutCoreDumps(cmd); err != nil {
				results <- fmt.Errorf("start: %w", err)
				return
			}
			if err := cmd.Wait(); err != nil {
				results <- fmt.Errorf("wait: %w", err)
				return
			}
			soft, hard, err := parseCoreLimitLine(stdout.String())
			if err != nil {
				results <- err
				return
			}
			if soft != "1" || hard != procLimitValue(before.Max) {
				results <- fmt.Errorf("limits soft=%q hard=%q", soft, hard)
				return
			}
			results <- nil
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	requireLimitUnchanged(t, before)
}

func TestWfRunCaptureCmdSuppressesCoreDumps(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	cmd := exec.Command("sh", "-c", coreTreeReportScript)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := runCaptureCmd(context.Background(), cmd); err != nil {
		t.Fatalf("capture runner: %v", err)
	}
	requireGuardedTree(t, stdout.String(), before)
	soft, hard := coreLimitLine(t, stdout.String())
	if soft != "1" || hard != procLimitValue(before.Max) {
		t.Fatalf("capture limits soft=%q hard=%q, want soft=1 hard=%q", soft, hard, procLimitValue(before.Max))
	}
	requireLimitUnchanged(t, before)
}

// Limit changes in this fixture happen in an isolated test process. The test
// runner (and concurrent daemon-like launches) never inherits a temporary limit.
func TestCoreDumpGuardIsolatedInheritance(t *testing.T) {
	if value := os.Getenv("SOLU_TEST_CORE_LIMIT"); value != "" {
		cur, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		before := coreDumpGuardPreconditions(t)
		before.Cur = cur
		if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &before); err != nil {
			t.Fatal(err)
		}
		const n = 12
		results := make(chan error, n)
		for i := 0; i < n; i++ {
			go func() {
				cmd := exec.Command("sh", "-c", `grep 'Max core' /proc/self/limits; sh -c "grep 'Max core' /proc/self/limits"; exit 23`)
				out, err := guardedCombinedOutput(cmd)
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 23 {
					results <- fmt.Errorf("target exit: %v %s", err, out)
					return
				}
				lines := coreLimitLines(string(out))
				if len(lines) != 2 {
					results <- fmt.Errorf("child/grandchild: %s", out)
					return
				}
				for _, line := range lines {
					soft, hard, err := parseCoreLimitLine(line)
					if err != nil || soft != "1" || hard != procLimitValue(before.Max) {
						results <- fmt.Errorf("limits: %s (%v)", line, err)
						return
					}
				}
				results <- nil
			}()
		}
		remaining := n
		for remaining > 0 {
			requireLimitUnchanged(t, before)
			select {
			case err := <-results:
				remaining--
				if err != nil {
					t.Error(err)
				}
			default:
				time.Sleep(time.Millisecond)
			}
		}
		requireLimitUnchanged(t, before)
		for _, value := range []string{"0", "false", "off", "no"} {
			t.Setenv(coreDumpGuardEnv, value)
			out, err := guardedOutput(exec.Command("sh", "-c", "grep 'Max core' /proc/self/limits"))
			if err != nil {
				t.Fatal(err)
			}
			soft, hard := coreLimitLine(t, string(out))
			if soft != procLimitValue(cur) || hard != procLimitValue(before.Max) {
				t.Fatalf("opt out %s: %s", value, out)
			}
		}
		return
	}
	for _, cur := range []string{"0", "4"} {
		t.Run(cur, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCoreDumpGuardIsolatedInheritance$")
			cmd.Env = append(os.Environ(), "SOLU_TEST_CORE_LIMIT="+cur)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated soft=%s: %v\n%s", cur, err, out)
			}
		})
	}
}

func TestCoreDumpGuardPolicyMatrix(t *testing.T) {
	for _, wsl := range []bool{false, true} {
		for _, value := range []string{"", "unknown", "1", "true", "on", "yes", "0", "false", "off", "no", " TRUE "} {
			want := wsl
			switch strings.TrimSpace(strings.ToLower(value)) {
			case "1", "true", "on", "yes":
				want = true
			case "0", "false", "off", "no":
				want = false
			}
			if got := coreDumpGuardPolicy(value, func() bool { return wsl }); got != want {
				t.Errorf("override=%q wsl=%v: %v", value, wsl, got)
			}
		}
	}
}

func TestCoreDumpGuardParentFailures(t *testing.T) {
	for _, failure := range []string{"getrlimit", "hard-limit", "LookPath", "adaptation", "missing"} {
		t.Run(failure, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			cmd := exec.Command("sh", "-c", `echo ran > "$1"`, "sh", marker)
			var starter interface{ Start() error } = cmd
			get := func(_ int, r *syscall.Rlimit) error { *r = syscall.Rlimit{Cur: 0, Max: 42}; return nil }
			look := exec.LookPath
			switch failure {
			case "getrlimit":
				get = func(int, *syscall.Rlimit) error { return syscall.EPERM }
			case "hard-limit":
				get = func(_ int, r *syscall.Rlimit) error { *r = syscall.Rlimit{}; return nil }
			case "LookPath":
				look = func(string) (string, error) { return "", exec.ErrNotFound }
			case "adaptation":
				starter = &coreLimitObservingStarter{}
			case "missing":
				cmd.Path = filepath.Join(t.TempDir(), "not-present")
			}
			err := startWithCoreDumpGuard(starter, true, get, look)
			var guardErr *coreDumpGuardError
			if !errors.As(err, &guardErr) || !strings.Contains(err.Error(), coreDumpGuardDiagnostic) || guardErr.Executable == "" {
				t.Fatalf("guard error: %v", err)
			}
			if cmd.Process != nil {
				t.Fatal("target started")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("target marker exists: %v", err)
			}
		})
	}
}

// TestCoreDumpGuardExecCausePreserved pins that the helper's private report
// pipe restores the kernel's execve cause instead of swallowing it. A target
// that passes LookPath but is not a valid image must surface ENOEXEC through
// the guard error, wrapped around the helper's exit-125 *exec.ExitError.
func TestCoreDumpGuardExecCausePreserved(t *testing.T) {
	coreDumpGuardPreconditions(t)
	bad := filepath.Join(t.TempDir(), "bad-executable")
	if err := os.WriteFile(bad, []byte("not an executable image\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := startWithoutCoreDumps(exec.Command(bad))
	var ge *coreDumpGuardError
	var ee *exec.ExitError
	if !errors.Is(err, syscall.ENOEXEC) ||
		!errors.As(err, &ge) ||
		!errors.As(err, &ee) ||
		ee.ExitCode() != coreDumpGuardExitCode ||
		!strings.Contains(err.Error(), coreDumpGuardDiagnostic) ||
		!strings.Contains(err.Error(), bad) {
		t.Fatalf("exec cause not preserved: %v", err)
	}
}

// TestCoreDumpGuardPTYInvalidImageExit125 pins the intended asymmetry with
// TestCoreDumpGuardExecCausePreserved: a target that passes LookPath but is
// not a valid image cannot report its execve errno through the go-pty seam
// (go-pty has no ExtraFiles report-pipe API, so reportFD stays -1). The
// /proc/self/exe helper itself is a valid image, so Start succeeds and
// startWithoutCoreDumps returns nil; the helper's ENOEXEC then surfaces as
// exit 125 with the guard diagnostic on the PTY. Callers must treat a nil
// Start as "helper launched", not "target exec succeeded".
func drainCoreDumpGuardPTY(t *testing.T, pty gopty.Pty, done <-chan struct{}) {
	t.Helper()
	// Wait reaps the child but go-pty retains the parent's slave descriptor.
	// Close that descriptor to produce EOF after pending output is drained.
	// Closing the master first discards output if the reader hasn't run yet.
	if err := pty.(gopty.UnixPty).Slave().Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		pty.Close()
		<-done
		t.Fatal("PTY reader did not drain after child exit")
	}
	pty.Close()
}

func TestCoreDumpGuardPTYInvalidImageExit125(t *testing.T) {
	coreDumpGuardPreconditions(t)
	bad := filepath.Join(t.TempDir(), "bad-executable")
	if err := os.WriteFile(bad, []byte("not an executable image\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pty, err := gopty.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	cmd := pty.Command(bad)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatalf("PTY Start: %v", err)
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, pty); close(done) }()
	waitErr := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) || ee.ExitCode() != coreDumpGuardExitCode {
		t.Fatalf("PTY Wait: %v", waitErr)
	}
	if errors.Is(waitErr, syscall.ENOEXEC) {
		t.Fatalf("PTY seam unexpectedly preserves ENOEXEC: %v", waitErr)
	}
	drainCoreDumpGuardPTY(t, pty, done)
	if !strings.Contains(out.String(), coreDumpGuardDiagnostic) {
		t.Fatalf("PTY output missing guard diagnostic: %q", out.String())
	}
}

func TestCoreDumpGuardHelperFailures(t *testing.T) {
	for _, failure := range []string{"getrlimit", "hard-limit", "setrlimit", "exec", "success"} {
		t.Run(failure, func(t *testing.T) {
			get := func(_ int, r *syscall.Rlimit) error { *r = syscall.Rlimit{Cur: 0, Max: 1234}; return nil }
			setCalled, execCalled := false, false
			set := func(_ int, r *syscall.Rlimit) error {
				setCalled = true
				if *r != (syscall.Rlimit{Cur: 1, Max: 1234}) {
					t.Fatalf("changed hard limit: %+v", r)
				}
				if failure == "setrlimit" {
					return syscall.EPERM
				}
				return nil
			}
			run := func(target string, argv, env []string) error {
				execCalled = true
				if target != "/target" || !reflect.DeepEqual(argv, []string{"custom0", "arg"}) || !reflect.DeepEqual(env, []string{"X=y"}) {
					t.Fatal("exec arguments changed")
				}
				if failure == "exec" {
					return syscall.ENOEXEC
				}
				return nil
			}
			if failure == "getrlimit" {
				get = func(int, *syscall.Rlimit) error { return syscall.EPERM }
			}
			if failure == "hard-limit" {
				get = func(_ int, r *syscall.Rlimit) error { *r = syscall.Rlimit{}; return nil }
			}
			var diagnostic bytes.Buffer
			code := coreDumpGuardExec("/target", []string{"custom0", "arg"}, []string{"X=y"}, &diagnostic, get, set, run)
			if failure == "success" {
				if code != 0 || diagnostic.Len() != 0 || !execCalled {
					t.Fatalf("success: %d %s", code, &diagnostic)
				}
				return
			}
			if code != coreDumpGuardExitCode || !strings.Contains(diagnostic.String(), coreDumpGuardDiagnostic) || !strings.Contains(diagnostic.String(), "/target") {
				t.Fatalf("failure: %d %s", code, &diagnostic)
			}
			if failure != "exec" && execCalled {
				t.Fatal("target executed after setup failure")
			}
			if (failure == "getrlimit" || failure == "hard-limit") && setCalled {
				t.Fatal("set after failed preflight")
			}
		})
	}
}

func TestCoreDumpGuardExecSemantics(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "target")); err != nil {
		t.Fatal(err)
	}
	extra, err := os.CreateTemp(t.TempDir(), "extra")
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `read input; printf '%s:%s:%s\n' "$input" "$GUARD_ENV" "$PWD"; cat /proc/$$/cmdline; printf extra >&3; printf diagnostic >&2; exit 23`)
	cmd.Path = "./target"
	cmd.Args[0] = "custom-argv-zero"
	cmd.Env = []string{"PATH=/bin:/usr/bin", "GUARD_ENV=value"}
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("input\n")
	cmd.ExtraFiles = []*os.File{extra}
	attr := &syscall.SysProcAttr{Setsid: true}
	cmd.SysProcAttr = attr
	cmd.WaitDelay = 100 * time.Millisecond
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) || ee.ExitCode() != 23 {
		t.Fatalf("exit: %v", err)
	}
	if !strings.Contains(out.String(), "input:value:"+dir) || !strings.Contains(out.String(), "custom-argv-zero\x00") || stderr.String() != "diagnostic" {
		t.Fatalf("stdio/argv/env/dir: %q %q", out.String(), stderr.String())
	}
	if raw, _ := os.ReadFile(extra.Name()); string(raw) != "extra" {
		t.Fatalf("ExtraFiles=%q", raw)
	}
	if cmd.SysProcAttr != attr || cmd.WaitDelay != 100*time.Millisecond || cmd.Process.Pid != cmd.ProcessState.Pid() {
		t.Fatal("command lifecycle changed")
	}
	requireLimitUnchanged(t, before)
}

func TestCoreDumpGuardCancellationAndWaitDelay(t *testing.T) {
	coreDumpGuardPreconditions(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := cmd.Wait(); err == nil || !cmd.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatalf("cancel: %v", err)
	}
	cmd = exec.Command("sh", "-c", "sleep 1 & exit 0")
	cmd.WaitDelay = 20 * time.Millisecond
	if _, err := guardedOutput(cmd); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("WaitDelay: %v", err)
	}
}

func TestCoreDumpGuardPTYPathArgvAndResize(t *testing.T) {
	coreDumpGuardPreconditions(t)
	pty, err := gopty.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	if err := pty.Resize(97, 31); err != nil {
		t.Fatal(err)
	}
	cmd := pty.Command("sh", "-c", `stty size; cat /proc/$$/cmdline; echo; grep 'Max core' /proc/self/limits; sh -c "grep 'Max core' /proc/self/limits"; exit 17`)
	cmd.Args[0] = "custom-pty-zero"
	cmd.Dir = t.TempDir()
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, pty); close(done) }()
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) || ee.ExitCode() != 17 {
		t.Fatalf("PTY exit: %v", err)
	}
	drainCoreDumpGuardPTY(t, pty, done)
	if !strings.Contains(out.String(), "31 97") || !strings.Contains(out.String(), "custom-pty-zero\x00") || len(coreLimitLines(out.String())) != 2 {
		t.Fatalf("PTY output: %q", out.String())
	}
}

func TestGuardedCaptureContractsAndDiagnostics(t *testing.T) {
	coreDumpGuardPreconditions(t)
	for _, combined := range []bool{false, true} {
		t.Run(strconv.FormatBool(combined), func(t *testing.T) {
			run := guardedOutput
			if combined {
				run = guardedCombinedOutput
			}
			cmd := exec.Command("sh", "-c", "printf output; printf failure >&2; exit 9")
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			out, err := run(cmd)
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 9 {
				t.Fatalf("ordinary exit: %v", err)
			}
			if !combined && (string(out) != "output" || string(ee.Stderr) != "failure") {
				t.Fatalf("Output: %q %q", out, ee.Stderr)
			}
			if combined && string(out) != "outputfailure" {
				t.Fatalf("CombinedOutput: %q", out)
			}
			if logs.Len() != 0 {
				t.Fatalf("ordinary failure logged: %s", &logs)
			}
			bad := filepath.Join(t.TempDir(), "bad-executable")
			if err := os.WriteFile(bad, []byte("#!/missing-interpreter\n"), 0755); err != nil {
				t.Fatal(err)
			}
			_, err = run(exec.Command(bad))
			var ge *coreDumpGuardError
			if !errors.As(err, &ge) || !errors.As(err, &ee) || ee.ExitCode() != coreDumpGuardExitCode || !strings.Contains(logs.String(), coreDumpGuardDiagnostic) || !strings.Contains(logs.String(), bad) {
				t.Fatalf("helper diagnostic: %v; %s", err, &logs)
			}
			cmd = exec.Command("sh")
			cmd.Stdout = io.Discard
			if _, err := run(cmd); err == nil || err.Error() != "exec: Stdout already set" {
				t.Fatalf("stdout rejection: %v", err)
			}
			if combined {
				cmd = exec.Command("sh")
				cmd.Stderr = io.Discard
				if _, err := run(cmd); err == nil || err.Error() != "exec: Stderr already set" {
					t.Fatalf("stderr rejection: %v", err)
				}
			}
		})
	}
	// Compare the bounded stderr contract byte for byte with the standard library.
	script := `printf prefix; head -c 100000 /dev/zero; printf suffix >&2; exit 1`
	script = "(" + script + ") >&2"
	_, ordinaryErr := exec.Command("sh", "-c", script).Output()
	_, guardedErr := guardedOutput(exec.Command("sh", "-c", script))
	var ordinary, guarded *exec.ExitError
	if !errors.As(ordinaryErr, &ordinary) || !errors.As(guardedErr, &guarded) || !bytes.Equal(ordinary.Stderr, guarded.Stderr) {
		t.Fatal("bounded ExitError.Stderr differs from os/exec")
	}
}

// Run an existing limit-changing retained regression in its own process, so it
// cannot affect unrelated tests. The test name is an exact regex, not a skip.
func isolateCoreLimitTest(t *testing.T) bool {
	t.Helper()
	if os.Getenv("SOLU_TEST_ISOLATED_CORE_TEST") == t.Name() {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), "SOLU_TEST_ISOLATED_CORE_TEST="+t.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated regression: %v\n%s", err, out)
	}
	return true
}

func TestCoreDumpGuardPreflightClosesPipes(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo should-not-run")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	err = startWithCoreDumpGuard(cmd, true, func(int, *syscall.Rlimit) error { return syscall.EPERM }, exec.LookPath)
	var ge *coreDumpGuardError
	if !errors.As(err, &ge) || cmd.Process != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := in.Write([]byte("input")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdin not closed: %v", err)
	}
	for _, r := range []io.Reader{out, stderr} {
		if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("output pipe not closed: %v", err)
		}
	}
}
