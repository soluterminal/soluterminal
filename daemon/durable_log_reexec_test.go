//go:build !windows

package main

// The durable log has to survive the one place in boot where the daemon
// replaces its own process image: maybeReexecCleanEnv. main.go attaches the log
// BEFORE that re-exec on purpose, so the decision to re-exec is itself on the
// record — which only pays off if the process on the other side of the exec
// re-attaches and re-arms rather than inheriting a stale fd.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDurableLogSurvivesCleanEnvReexec drives attachDurableLog ->
// maybeReexecCleanEnv -> attachDurableLog in one real process lineage.
//
// syscall.Exec keeps the pid but throws away the address space and (because Go
// opens files O_CLOEXEC) the fd table, so everything the first generation armed
// is gone. What must be true afterwards:
//
//   - the successor appends to the SAME solud.log rather than truncating it, so
//     the pre-exec records are still there to explain why the exec happened;
//   - it writes its own boot banner, so the file reads as two honest runs and
//     not one confusing one;
//   - and its crash output is armed on its own handle, so a panic AFTER the
//     re-exec still lands on disk. That last one is the whole point: an
//     unrecovered panic in a re-exec'd daemon was invisible before this change,
//     and would be invisible again if attachDurableLog ever moved below the
//     re-exec in main().
func TestDurableLogSurvivesCleanEnvReexec(t *testing.T) {
	if os.Getenv("SOLU_DURABLE_REEXEC_CHILD") == "1" {
		durableLogReexecChild()
		return
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestDurableLogSurvivesCleanEnvReexec", "-test.v")
	// The sentinel must start ABSENT, not empty: maybeReexecCleanEnv appends
	// its own SOLU_ENV_CLEANED=1 for the successor, and os.Getenv answers with
	// the first match, so an empty one carried in from here would mask it and
	// the successor would never reach the branch this test asserts on.
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envCleanedSentinel+"=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"SOLU_DURABLE_REEXEC_CHILD=1",
		"SOLU_DURABLE_LOG_DIR="+dir,
		// maybeReexecCleanEnv only fires for a daemon launched from inside a
		// Claude Code session; this is the marker it looks for.
		"CLAUDECODE=1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child was supposed to die of its post-re-exec panic; it exited 0:\n%s", out)
	}

	body, readErr := os.ReadFile(filepath.Join(dir, durableLogName))
	if readErr != nil {
		t.Fatalf("read durable log after re-exec: %v (child output:\n%s)", readErr, out)
	}
	got := string(body)

	if n := strings.Count(got, "solud boot"); n != 2 {
		t.Errorf("durable log holds %d boot banners, want 2 (one per process image); got:\n%s", n, got)
	}

	first := strings.Index(got, "before-clean-env-reexec")
	second := strings.Index(got, "after-clean-env-reexec")
	switch {
	case first < 0:
		t.Errorf("the pre-exec record is gone — the successor truncated the log instead of appending; got:\n%s", got)
	case second < 0:
		t.Errorf("the successor never logged; got:\n%s", got)
	case first > second:
		t.Errorf("records are out of order: the successor's record precedes its predecessor's; got:\n%s", got)
	}

	for _, needle := range []string{"panic:", "dying-words-after-reexec", "goroutine "} {
		if !strings.Contains(got, needle) {
			t.Errorf("durable log is missing %q — the successor's crash output was never armed; got:\n%s", needle, got)
		}
	}
	if idx := strings.Index(got, "panic:"); idx >= 0 && second >= 0 && idx < second {
		t.Errorf("the traceback precedes the successor's own record, so it came from the wrong generation; got:\n%s", got)
	}
}

// durableLogReexecChild is one process image of the lineage: attach, then
// either re-exec (first generation) or die loudly (second).
func durableLogReexecChild() {
	attachDurableLog()
	if durableLog == nil {
		fmt.Fprintln(os.Stderr, "durable log never attached")
		os.Exit(3)
	}

	if os.Getenv(envCleanedSentinel) == "1" {
		slog.Info("after-clean-env-reexec")
		panic("dying-words-after-reexec")
	}

	slog.Info("before-clean-env-reexec")
	maybeReexecCleanEnv() // replaces this process image; only returns on failure
	fmt.Fprintln(os.Stderr, "maybeReexecCleanEnv returned without exec'ing")
	os.Exit(3)
}

// TestCleanEnvSuccessorDetachesFromClaudeSession (ST-118): a primary daemon
// started inside a claude session re-execs clean in place, then, after flag
// parsing, continues as a copy of itself in a session of its own, so the
// claude session ending (its process group hung up) does not take it along.
func TestCleanEnvSuccessorDetachesFromClaudeSession(t *testing.T) {
	if os.Getenv("SOLU_DETACH_REEXEC_CHILD") == "1" {
		detachReexecChild()
		return
	}
	dir := t.TempDir()
	cmd := detachLineageCmd(t, dir, "TestCleanEnvSuccessorDetachesFromClaudeSession")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the starter should hand over and exit 0: %v\n%s", err, out)
	}
	starter := cmd.Process.Pid
	// The claude session ends: hang up its whole process group.
	_ = syscall.Kill(-starter, syscall.SIGHUP)

	got := waitDurableLog(t, dir, "detached-successor-survived")
	if !strings.Contains(got, "continuing as pid") {
		t.Errorf("the hand-over was not logged; log:\n%s", got)
	}
	if strings.Contains(got, "pending="+"1") {
		t.Errorf("the detached daemon inherited the one-time marker; log:\n%s", got)
	}
	var pid, sid, pgid int
	for _, line := range strings.Split(got, "\n") {
		if i := strings.Index(line, "detached-successor-started"); i >= 0 {
			for _, f := range strings.Fields(line[i:]) {
				k, v, ok := strings.Cut(strings.Trim(f, `"`), "=")
				if !ok {
					continue
				}
				n, _ := strconv.Atoi(v)
				switch k {
				case "pid":
					pid = n
				case "sid":
					sid = n
				case "pgid":
					pgid = n
				}
			}
		}
	}
	if pid == 0 || sid != pid || pgid != pid || pgid == starter {
		t.Fatalf("successor pid=%d sid=%d pgid=%d (starter group %d): want a session and group of its own; log:\n%s", pid, sid, pgid, starter, got)
	}
}

// A detached daemon that fails at once (another daemon owns the data
// directory) must not look like a successful start.
func TestDetachReportsAnImmediateFailure(t *testing.T) {
	if os.Getenv("SOLU_DETACH_REEXEC_CHILD") == "1" {
		detachReexecChild()
		return
	}
	dir := t.TempDir()
	cmd := detachLineageCmd(t, dir, "TestDetachReportsAnImmediateFailure")
	cmd.Env = append(cmd.Env, "SOLU_DETACH_TEST_FAIL=1")
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 73 {
		t.Fatalf("starter exit = %v, want status 73 from the detached daemon\n%s", err, out)
	}
	if !strings.Contains(string(out), "exited at once (status 73)") {
		t.Fatalf("the starter must say the daemon failed; output:\n%s", out)
	}
}

func detachLineageCmd(t *testing.T, dir, test string) *exec.Cmd {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envCleanedSentinel+"=") || strings.HasPrefix(kv, "INVOCATION_ID=") || strings.HasPrefix(kv, detachPendingEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(env, "SOLU_DETACH_REEXEC_CHILD=1", "SOLU_DURABLE_LOG_DIR="+dir, "CLAUDECODE=1")
	// The starter gets a process group of its own, standing in for the claude
	// session's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func waitDurableLog(t *testing.T, dir, needle string) string {
	t.Helper()
	var got string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(filepath.Join(dir, durableLogName))
		got = string(body)
		if strings.Contains(got, needle) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("durable log never showed %q; log:\n%s", needle, got)
	return got
}

// detachReexecChild is one process image of the lineage:
//  1. the starter (inside the "claude session") re-execs clean in place;
//  2. the clean image, marker set, detaches the daemon (as main does after
//     flag parsing);
//  3. the detached daemon, marker consumed, reports where it runs.
func detachReexecChild() {
	attachDurableLog()
	if durableLog == nil {
		fmt.Fprintln(os.Stderr, "durable log never attached")
		os.Exit(3)
	}
	switch {
	case os.Getenv(envCleanedSentinel) != "1":
		maybeReexecCleanEnv() // replaces this process image
		fmt.Fprintln(os.Stderr, "maybeReexecCleanEnv returned without re-exec")
		os.Exit(3)
	case os.Getenv(detachPendingEnv) == "1":
		detachFromClaudeSessionFn = func(func(string) string, string, string, string, string, string) bool { return true }
		detachEarlyExitWait = 300 * time.Millisecond
		maybeDetachFromClaudeSession(defaultDaemonAddr, defaultDataDir(), "", "") // exits
		fmt.Fprintln(os.Stderr, "maybeDetachFromClaudeSession returned without detaching")
		os.Exit(3)
	default:
		if os.Getenv("SOLU_DETACH_TEST_FAIL") == "1" {
			os.Exit(73)
		}
		sid, _ := unix.Getsid(0)
		pgid, _ := syscall.Getpgid(0)
		slog.Info("detached-successor-started pid="+strconv.Itoa(os.Getpid())+" sid="+strconv.Itoa(sid)+" pgid="+strconv.Itoa(pgid),
			"pending", os.Getenv(detachPendingEnv), "claudeEnv", os.Getenv("CLAUDECODE"))
		// Outlive the starter's hang-up by a margin before reporting.
		time.Sleep(time.Second)
		slog.Info("detached-successor-survived")
		os.Exit(0)
	}
}

func TestDetachFromClaudeSessionOnlyForThePrimary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	none := func(string) string { return "" }
	data := defaultDataDir()
	if !detachFromClaudeSession(none, "linux", defaultDaemonAddr, data, "", "") {
		t.Fatal("the primary daemon (default port and data) must detach")
	}
	for _, c := range []struct{ addr, data, backend, shell string }{
		{"127.0.0.1:9791", data, "", ""},
		{defaultDaemonAddr, filepath.Join(home, "side-data"), "", ""},
		{defaultDaemonAddr, data, "wsl", ""},
		{defaultDaemonAddr, data, "", "0.0.1243"},
	} {
		if detachFromClaudeSession(none, "linux", c.addr, c.data, c.backend, c.shell) {
			t.Fatalf("%+v must stay in place: a side daemon, or one the desktop app supervises", c)
		}
	}
	systemd := func(k string) string {
		if k == "INVOCATION_ID" {
			return "abc"
		}
		return ""
	}
	if detachFromClaudeSession(systemd, "linux", defaultDaemonAddr, data, "", "") {
		t.Fatal("a systemd service must stay its unit's main process")
	}
}
