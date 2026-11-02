//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The tests in this file exercise the real daemon-owned launch seams that were
// previously unguarded: the supervised `solud agent` wrapper, the agent's own
// pipe/PTY child, the managed Node harness installer, a native muse turn, a
// scheduled headless task run and an aria headless run. Each one
// must set only the soft RLIMIT_CORE to 1 across the fork/exec window and leave
// the hard limit untouched. Helpers (coreDumpGuardPreconditions, coreLimitLine,
// procLimitValue, requireLimitUnchanged, snapshotCrashDir,
// requireCrashDirUnchanged) live in coredump_guard_linux_test.go.

// TestSupervisedAgentChildSuppressesCoreDumps proves the single production
// spawner for every daemon-owned `solud agent` process applies the guard, so
// the wrapper and its whole descendant tree inherit soft=1.
func TestSupervisedAgentChildSuppressesCoreDumps(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	crashDir, beforeEntries := snapshotCrashDir(t)

	cmd := exec.Command("sh", "-c", `grep 'Max core' /proc/self/limits; sh -c "grep 'Max core' /proc/self/limits"`)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	applyDetached(cmd)
	reaped, err := startSupervisedAgentChild(cmd)
	if err != nil {
		t.Fatalf("startSupervisedAgentChild: %v", err)
	}
	<-reaped

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

	// Crash arm: the child and a grandchild SIGSEGV must not dump a core (WSL
	// pipes the dump to a host file under the kernel core_pattern).
	var crashOut, crashErr bytes.Buffer
	crash := exec.Command("sh", "-c", `sh -c 'kill -SEGV $$'; echo grandchild=$?`)
	crash.Stdout = &crashOut
	crash.Stderr = &crashErr
	reaped, err = startSupervisedAgentChild(crash)
	if err != nil {
		t.Fatalf("startSupervisedAgentChild crash: %v", err)
	}
	<-reaped
	if !strings.Contains(crashOut.String(), "grandchild=139") {
		t.Errorf("guarded crash stdout = %q, want grandchild=139", crashOut.String())
	}
	if strings.Contains(strings.ToLower(crashErr.String()), "core dumped") {
		t.Errorf("guarded crash reported a core dump: %q", crashErr.String())
	}
	requireCrashDirUnchanged(t, crashDir, beforeEntries)
	requireLimitUnchanged(t, before)
}

// TestAgentGuardHelperProcess is the helper-process entry point reached through
// the test binary in TestAgentChildStartSuppressesCoreDumps. It re-runs the
// binary as the `solud agent` wrapper so the real runAgent spawn block (pipe
// and PTY) executes under the guard. When invoked as an ordinary test (no `--`)
// it is a no-op.
func TestAgentGuardHelperProcess(t *testing.T) {
	sep := -1
	for i, arg := range os.Args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep >= 0 && sep+1 < len(os.Args) && os.Args[sep+1] == "codex-terminal" {
		os.Args = append([]string{"solud"}, os.Args[sep+1:]...)
		os.Exit(runCodexTerminal())
	}
	if sep < 0 || sep+1 >= len(os.Args) || os.Args[sep+1] != "agent" {
		return
	}
	os.Args = append([]string{"solud"}, os.Args[sep+1:]...)
	os.Exit(runAgent())
}

// runAgentHelper re-runs the test binary as the agent wrapper. script runs as
// `sh -c script` under runAgent; the caller may have it write to $GUARD_OUT.
func runAgentHelper(t *testing.T, noPty bool, script string) (out string, exit int, stderr string) {
	t.Helper()
	// t.TempDir() embeds the subtest name, which overflows the unix socket
	// path limit under a long TMPDIR (publisher scratch dirs).
	socket := shortSocketPath(t)
	cwd := t.TempDir()
	dataDir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "limits.txt")

	argv := []string{
		"-test.run=^TestAgentGuardHelperProcess$", "--",
		"agent", "--socket", socket, "--cwd", cwd, "--data-dir", dataDir,
	}
	if noPty {
		argv = append(argv, "--no-pty")
	}
	argv = append(argv, "--", "sh", "-c", script)

	cmd := exec.Command(os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "GUARD_OUT="+outPath)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if raw, rerr := os.ReadFile(outPath); rerr == nil {
		out = string(raw)
	}
	if runErr != nil {
		exit = 1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
	}
	return out, exit, errBuf.String()
}

// TestAgentChildStartSuppressesCoreDumps drives the real runAgent seam in both
// pipe (--no-pty) and PTY modes and proves the agent child and its grandchild
// inherit soft=1 with the hard limit unchanged. A SIGSEGV child must not create
// a WSL crash-dir file.
func TestAgentChildStartSuppressesCoreDumps(t *testing.T) {
	if isolateCoreLimitTest(t) {
		return
	}
	_ = coreDumpGuardPreconditions(t)
	// Exercise the child-only Setrlimit path from a parent soft limit above 1.
	// The PTY implementation adapts argv separately from exec.Cmd.
	_, before := raiseCoreSoftLimitForTest(t)
	crashDir, beforeEntries := snapshotCrashDir(t)
	script := `grep 'Max core' /proc/self/limits > "$GUARD_OUT"; sh -c "grep 'Max core' /proc/self/limits" >> "$GUARD_OUT"`

	for _, noPty := range []bool{true, false} {
		name := "pty"
		if noPty {
			name = "no-pty"
		}
		t.Run(name, func(t *testing.T) {
			raw, exit, stderr := runAgentHelper(t, noPty, script)
			if exit != 0 {
				t.Fatalf("agent helper (no-pty=%v) exit=%d stderr=%q", noPty, exit, stderr)
			}
			lines := coreLimitLines(raw)
			if len(lines) != 2 {
				t.Fatalf("Max core lines = %d, want 2: %q (stderr=%q)", len(lines), raw, stderr)
			}
			wantHard := procLimitValue(before.Max)
			for i, line := range lines {
				soft, hard := coreLimitLine(t, line)
				if soft != "1" || hard != wantHard {
					t.Errorf("process %d limits soft=%q hard=%q, want soft=1 hard=%q", i, soft, hard, wantHard)
				}
			}
		})
	}

	// Failure path: a SIGSEGV agent child must be reaped cleanly with no dump.
	if _, exit, stderr := runAgentHelper(t, true, `kill -SEGV $$`); exit != 0 {
		t.Errorf("agent crash helper exit=%d stderr=%q, want 0", exit, stderr)
	}
	requireCrashDirUnchanged(t, crashDir, beforeEntries)
	requireLimitUnchanged(t, before)
}

// TestHarnessInstallerSuppressesCoreDumps proves the managed Node harness
// installer launched by ensureHarnessRuntime inherits the guard. A fake `node`
// earlier on PATH runs the shortcut's Node gate and reports its own limits.
func TestHarnessInstallerSuppressesCoreDumps(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	t.Setenv("HOME", t.TempDir())

	binDir := t.TempDir()
	fake := filepath.Join(binDir, "node")
	// Report limits, then exit non-zero on purpose: a failed install skips the
	// post-install ensureHarnessCommands() self-heal, whose asynchronous writes
	// are outside this test's scope (and could outlive the restored HOME/PATH).
	nodeScript := "#!/bin/sh\nif [ \"$1\" = \"-e\" ]; then exit 0; fi\n" + coreTreeReportScript + "\nexit 7\n"
	if err := os.WriteFile(fake, []byte(nodeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dataDir := t.TempDir()
	api := &apiServer{dataDir: dataDir, ver: "test"}
	api.ensureHarnessRuntime("deepseek")

	deadline := time.Now().Add(30 * time.Second)
	for {
		installing, _ := api.harnessInstallState("deepseek")
		if !installing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness install did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}

	raw, err := os.ReadFile(filepath.Join(dataDir, "harness-install-deepseek.log"))
	if err != nil {
		t.Fatalf("read harness install log: %v", err)
	}
	requireGuardedTree(t, string(raw), before)
	soft, hard := coreLimitLine(t, string(raw))
	if soft != "1" || hard != procLimitValue(before.Max) {
		t.Fatalf("harness installer limits soft=%q hard=%q, want soft=1 hard=%q", soft, hard, procLimitValue(before.Max))
	}
	requireLimitUnchanged(t, before)
}

// TestMuseTurnSuppressesCoreDumps proves the native pane's per-turn `muse exec`
// child is guarded; the fake muse reports its limits on stderr, which lands in
// the bounded turn_exit tail.
func TestMuseTurnSuppressesCoreDumps(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "muse")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n("+coreTreeReportScript+") >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, interrupted, tail := runMuseTurn(fake, "sid", "", "", "", "prompt", make(chan struct{}))
	if code != 0 || interrupted {
		t.Fatalf("runMuseTurn code=%d interrupted=%v tail=%q", code, interrupted, tail)
	}
	requireGuardedTree(t, tail, before)
	soft, hard := coreLimitLine(t, tail)
	if soft != "1" || hard != procLimitValue(before.Max) {
		t.Fatalf("muse turn limits soft=%q hard=%q, want soft=1 hard=%q", soft, hard, procLimitValue(before.Max))
	}
	requireLimitUnchanged(t, before)
}

// TestScheduleTaskSpawnSuppressesCoreDumps mirrors the guarded agent-schedule
// seam for the task spawner: a fake claude reports its limits and the recorded
// run log must show soft=1.
func TestScheduleTaskSpawnSuppressesCoreDumps(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+coreTreeReportScript+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	s := newTestScheduleStore(t)
	enabled := false
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "x", Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.runTaskProcessWithTimeout(sched.ID, fake, work, "x", "plan", "", 30*time.Second)

	got, ok := s.Get(sched.ID)
	if !ok || len(got.Runs) == 0 || got.Runs[0].LogFile == "" {
		t.Fatalf("no recorded task run: ok=%v runs=%+v", ok, got.Runs)
	}
	raw, err := os.ReadFile(filepath.Join(s.taskRunDir(sched.ID), got.Runs[0].LogFile))
	if err != nil {
		t.Fatalf("read task run log: %v", err)
	}
	requireGuardedTree(t, string(raw), before)
	soft, hard := coreLimitLine(t, string(raw))
	if soft != "1" || hard != procLimitValue(before.Max) {
		t.Fatalf("task run limits soft=%q hard=%q, want soft=1 hard=%q", soft, hard, procLimitValue(before.Max))
	}
	requireLimitUnchanged(t, before)
}

const coreTreeReportScript = `grep 'Max core' /proc/self/limits; sh -c "grep 'Max core' /proc/self/limits"`

func requireGuardedTree(t *testing.T, raw string, before syscall.Rlimit) {
	t.Helper()
	lines := coreLimitLines(raw)
	if len(lines) < 2 || len(lines)%2 != 0 {
		t.Fatalf("missing child/grandchild pair: %q", raw)
	}
	for _, line := range lines {
		soft, hard := coreLimitLine(t, line)
		if soft != "1" || hard != procLimitValue(before.Max) {
			t.Fatalf("unguarded tree: %s", line)
		}
	}
	requireLimitUnchanged(t, before)
}

// Embed the report path in a fake executable so workerEnv's credential
// allowlist is exercised unchanged. Every invocation records its argv and two
// generations of limits without disturbing the runtime's response parser.
func coreGuardFake(t *testing.T, binDir, name, report, body string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + report + ".args'\n" +
		"grep 'Max core' /proc/self/limits >> '" + report + "'\n" +
		"sh -c \"grep 'Max core' /proc/self/limits\" >> '" + report + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Fake Codex app-server speaks only the two boot RPCs required to start the
// terminal client. Both processes report inherited limits and fork a child.
func TestCoreDumpGuardCodexCLIHelper(t *testing.T) {
	report := os.Getenv("SOLU_TEST_GUARD_REPORT")
	if report == "" {
		return
	}
	raw, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		t.Fatal(err)
	}
	child, err := exec.Command("sh", "-c", "cat /proc/self/limits").Output()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(report, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(append(raw, child...))
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	args := os.Args
	serverPath := ""
	for i, arg := range args {
		if arg == "--listen" && i+1 < len(args) {
			serverPath = strings.TrimPrefix(args[i+1], "unix://")
		}
	}
	if serverPath == "" {
		os.Exit(17)
	}
	listener, err := net.Listen("unix", serverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request map[string]json.RawMessage
			if conn.ReadJSON(&request) != nil {
				return
			}
			if id := request["id"]; id != nil {
				if conn.WriteJSON(map[string]any{"id": id, "result": map[string]any{}}) != nil {
					return
				}
			}
		}
	})}
	_ = server.Serve(listener)
}

func TestCodexLiveGuardedDescendants(t *testing.T) {
	before := coreDumpGuardPreconditions(t)
	dir := t.TempDir()
	data := t.TempDir()
	home := t.TempDir()
	report := filepath.Join(dir, "limits")
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"access_token":"fixture","account_id":"fixture-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nexec " + shellSingleQuote(os.Args[0]) + " -test.run=^TestCoreDumpGuardCodexCLIHelper$ -- \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	// The supervised wrapper executes runAgent's no-PTY path, which in turn
	// starts the real Codex Live bridge and its fake server/client descendants.
	argv := []string{"-test.run=^TestAgentGuardHelperProcess$", "--", "agent", "--socket", filepath.Join(dir, "agent.sock"), "--cwd", dir, "--data-dir", data, "--no-pty", "--", os.Args[0], "-test.run=^TestAgentGuardHelperProcess$", "--", "codex-terminal", "--data-dir", data, "--", fake}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "SOLU_PANE_ID=guard-fixture", "SOLU_TEST_GUARD_REPORT="+report)
	applyDetached(cmd)
	cmd.Cancel = func() error { return killChildTree(cmd.Process) }
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	reaped, err := startSupervisedAgentChild(cmd)
	if err != nil {
		t.Fatal(err)
	}
	<-reaped
	if !cmd.ProcessState.Success() {
		t.Fatalf("wrapper exited: %s %s", cmd.ProcessState, &stderr)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read descendants: %v; %s", err, &stderr)
	}
	if len(coreLimitLines(string(raw))) != 4 {
		t.Fatalf("server/client plus grandchildren: %q", raw)
	}
	requireGuardedTree(t, string(raw), before)
	status, err := os.ReadFile(codexLiveStatusPath(data, "guard-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var st codexLiveStatus
	if err := json.Unmarshal(status, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "stopped" || st.ClientPID == 0 || st.ServerPID == 0 {
		t.Fatalf("Codex Live lifecycle: %s", status)
	}
	for _, pid := range []int{st.ClientPID, st.ServerPID} {
		if fields, ok := statFieldsAfterComm(pid); ok && len(fields) > 0 && fields[0] != "Z" {
			t.Errorf("Codex Live descendant %d survived cleanup", pid)
		}
	}
}
