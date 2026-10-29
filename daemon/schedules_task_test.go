package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// isToolErr reports whether an MCP tool result is the error shape (toolErr).
func isToolErr(res any) bool {
	m, ok := res.(map[string]any)
	if !ok {
		return false
	}
	e, _ := m["isError"].(bool)
	return e
}

func TestValidateTaskKind(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func() ScheduleInput {
		return ScheduleInput{
			Kind: "task", Cron: "0 9 * * *",
			Prompt: "summarize the repo", Cwd: dir, Permission: "plan",
		}
	}
	tests := []struct {
		name    string
		mutate  func(*ScheduleInput)
		wantErr string
	}{
		{"valid plan", func(*ScheduleInput) {}, ""},
		{"valid edit", func(in *ScheduleInput) { in.Permission = "edit" }, ""},
		{"valid timeout", func(in *ScheduleInput) { in.TimeoutMin = 60 }, ""},
		{"no prompt", func(in *ScheduleInput) { in.Prompt = "" }, "prompt is required"},
		{"no cwd", func(in *ScheduleInput) { in.Cwd = "" }, "cwd is required"},
		{"relative cwd", func(in *ScheduleInput) { in.Cwd = "relative/path" }, "absolute"},
		{"missing cwd", func(in *ScheduleInput) { in.Cwd = filepath.Join(dir, "nope") }, "existing directory"},
		{"cwd is a file", func(in *ScheduleInput) { in.Cwd = file }, "existing directory"},
		{"bad permission", func(in *ScheduleInput) { in.Permission = "bypassPermissions" }, `"plan" or "edit"`},
		{"empty permission", func(in *ScheduleInput) { in.Permission = "" }, `"plan" or "edit"`},
		{"paneId rejected", func(in *ScheduleInput) { in.PaneID = "p1" }, "no paneId"},
		{"negative timeout", func(in *ScheduleInput) { in.TimeoutMin = -1 }, "timeoutMin"},
		{"timeout over ceiling", func(in *ScheduleInput) { in.TimeoutMin = 60*4 + 1 }, "timeoutMin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			err := in.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// bypassPermissions must be unreachable: the preset map is the only path from
// a client string to a --permission-mode flag.
func TestTaskPermissionPresetsAreClosed(t *testing.T) {
	for _, bad := range []string{"bypassPermissions", "default", "acceptEdits", "", "PLAN"} {
		if validTaskPermission(bad) {
			t.Errorf("validTaskPermission(%q) = true, want false", bad)
		}
	}
	for preset, mode := range map[string]string{"plan": "plan", "edit": "acceptEdits"} {
		if !validTaskPermission(preset) {
			t.Errorf("validTaskPermission(%q) = false", preset)
		}
		if taskPermissionModes[preset] != mode {
			t.Errorf("preset %q maps to %q, want %q", preset, taskPermissionModes[preset], mode)
		}
	}
}

func TestResolveClaudeBinMinimalPATHFindsNvmInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME and nvm layout are Unix-only")
	}
	home := t.TempDir()
	claude := filepath.Join(home, ".nvm", "versions", "node", "v9.9.9", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	got, err := resolveClaudeBin()
	if err != nil || got != claude {
		t.Fatalf("resolveClaudeBin = %q, %v; want %q", got, err, claude)
	}
}

func prepareWorkerBinLoginShellFallback(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("login-shell resolution is Unix-only")
	}
	originalGOOS := loginShellGOOS
	originalLookPath := loginShellLookPath
	originalOutput := loginShellCommandOutput
	t.Cleanup(func() {
		loginShellGOOS = originalGOOS
		loginShellLookPath = originalLookPath
		loginShellCommandOutput = originalOutput
	})
	loginShellGOOS = "linux"
	loginShellLookPath = func(string) (string, error) { return "/bin/bash", nil }
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
}

func TestResolveClaudeBinLoginShellFallback(t *testing.T) {
	prepareWorkerBinLoginShellFallback(t)
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	loginShellCommandOutput = func(_ context.Context, argv []string) ([]byte, error) {
		if !strings.Contains(argv[2], "command -v -- 'claude'") {
			t.Fatalf("resolver script = %q", argv[2])
		}
		return []byte(bin + "\n"), nil
	}
	got, err := resolveClaudeBin()
	if err != nil || got != bin {
		t.Fatalf("resolveClaudeBin = %q, %v; want %q", got, err, bin)
	}

	loginShellCommandOutput = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("shell failed")
	}
	if _, err := resolveClaudeBin(); err == nil || !strings.Contains(err.Error(), "login shell") {
		t.Fatalf("resolveClaudeBin error = %v, want login-shell hint", err)
	}
}

func TestResolveNodeBinLoginShellFallback(t *testing.T) {
	prepareWorkerBinLoginShellFallback(t)
	bin := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(bin, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	loginShellCommandOutput = func(_ context.Context, argv []string) ([]byte, error) {
		if !strings.Contains(argv[2], "command -v -- 'node'") {
			t.Fatalf("resolver script = %q", argv[2])
		}
		return []byte(bin + "\n"), nil
	}
	got, err := resolveNodeBin()
	if err != nil || got != bin {
		t.Fatalf("resolveNodeBin = %q, %v; want %q", got, err, bin)
	}
}

func TestResolveCodexBinLoginShellFallback(t *testing.T) {
	prepareWorkerBinLoginShellFallback(t)
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	loginShellCommandOutput = func(_ context.Context, argv []string) ([]byte, error) {
		if !strings.Contains(argv[2], "command -v -- 'codex'") {
			t.Fatalf("resolver script = %q", argv[2])
		}
		return []byte(bin + "\n"), nil
	}
	got, err := resolveCodexBin()
	if err != nil || got != bin {
		t.Fatalf("resolveCodexBin = %q, %v; want %q", got, err, bin)
	}

	loginShellCommandOutput = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("shell failed")
	}
	if _, err := resolveCodexBin(); err == nil || !strings.Contains(err.Error(), "login shell") {
		t.Fatalf("resolveCodexBin error = %v, want login-shell hint", err)
	}
}

// A task run captures the child's output into the per-schedule runs dir and
// records the log name on the run. We stand in a fake `claude` on PATH.
func TestTaskRunCapturesLog(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	// Echo argv, then whatever arrived on stdin — the prompt travels there.
	script := "#!/bin/sh\necho \"args: $*\"\necho \"stdin: $(cat)\"\necho task-output\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	s := newTestScheduleStore(t)
	enabled := false
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "check the build",
		Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	s.fireTask(sched.ID, work, "check the build", "plan", "", 0)
	run := waitForRun(t, s, sched.ID)
	if run.Status != "fired" {
		t.Fatalf("status = %q (%s), want fired", run.Status, run.Detail)
	}
	if run.LogFile == "" {
		t.Fatal("run.LogFile empty")
	}
	data, err := os.ReadFile(filepath.Join(s.taskRunDir(sched.ID), run.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "task-output") {
		t.Errorf("log missing child output: %q", got)
	}
	// The permission preset must reach claude as its mapped flag...
	if !strings.Contains(got, "--permission-mode plan") {
		t.Errorf("permission flag missing from argv: %q", got)
	}
	// ...and the prompt must arrive on STDIN, never as an argv element.
	if !strings.Contains(got, "stdin: check the build") {
		t.Errorf("prompt did not arrive on stdin: %q", got)
	}
	// The report-format contract rides every task prompt at run time.
	if !strings.Contains(got, "## Needs you") {
		t.Errorf("report style hint missing from stdin: %q", got)
	}
	if strings.Contains(got, "args: -p check the build") {
		t.Errorf("prompt leaked into argv: %q", got)
	}
	// Tasks use the "new since your last report" contract, and the first run
	// has no previous report to compare against.
	if !strings.Contains(got, "Nothing new needs you.") || strings.Contains(got, "Your last report") {
		t.Errorf("first run stdin should carry the task contract and no previous report: %q", got)
	}
	// The next run is handed the opening of this one.
	for deadline := time.Now().Add(testSpawnWait); s.taskRunningFor(sched.ID); {
		if time.Now().After(deadline) {
			t.Fatal("first run never released its slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.fireTask(sched.ID, work, "check the build", "plan", "", 0)
	var second ScheduleRun
	for deadline := time.Now().Add(testSpawnWait); ; time.Sleep(20 * time.Millisecond) {
		if got, _ := s.Get(sched.ID); len(got.Runs) >= 2 {
			second = got.Runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second run never recorded")
		}
	}
	if second.Status != "fired" {
		t.Fatalf("second run = %+v, want fired", second)
	}
	data2, err := os.ReadFile(filepath.Join(s.taskRunDir(sched.ID), second.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data2), "(Your last report, from ") {
		t.Errorf("second run stdin missing the previous report lede: %q", data2)
	}
	// Deleting the schedule takes its run logs with it.
	if err := s.Delete(sched.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.taskRunDir(sched.ID)); !os.IsNotExist(err) {
		t.Errorf("task run dir survived delete: %v", err)
	}
}

// Regression: `claude -p` takes an OPTIONAL value, so its parser rejects a
// value starting with a dash ("error: unknown option"). A prompt like
// "--force a rebuild and report" must still reach the model intact.
func TestTaskPromptStartingWithDashReachesChild(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	// Fail loudly if any argv element looks like the prompt.
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --force*) echo PROMPT_IN_ARGV; exit 3;; esac; done\necho \"stdin: $(cat)\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	s := newTestScheduleStore(t)
	enabled := false
	prompt := "--force a rebuild and report what changed"
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: prompt,
		Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, prompt, "plan", "", 0)
	run := waitForRun(t, s, sched.ID)
	if run.Status != "fired" {
		t.Fatalf("status = %q (%s), want fired", run.Status, run.Detail)
	}
	data, err := os.ReadFile(filepath.Join(s.taskRunDir(sched.ID), run.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "stdin: "+prompt) {
		t.Errorf("dash-prefixed prompt did not reach the child on stdin: %q", data)
	}
}

func TestTaskRunErrorsOnMissingCwd(t *testing.T) {
	s := newTestScheduleStore(t)
	gone := filepath.Join(t.TempDir(), "deleted")
	s.fireTask("sched_x", gone, "hi", "plan", "", 0)
	// fireTask records synchronously on this path (no goroutine), but the
	// schedule doesn't exist so recordRun is a no-op — assert it didn't panic
	// and that a real schedule gets the error recorded.
	work := t.TempDir()
	enabled := false
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "x", Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "x", "plan", "", 0)
	got, _ := s.Get(sched.ID)
	if len(got.Runs) == 0 || got.Runs[0].Status != "error" ||
		!strings.Contains(got.Runs[0].Detail, "working directory is gone") {
		t.Fatalf("expected missing-cwd error run, got %+v", got.Runs)
	}
}

func TestTaskSingleFlightAndConcurrencyCap(t *testing.T) {
	// fireTask resolves the claude binary BEFORE the single-flight/cap
	// checks, so a fake must be on PATH even though nothing spawns here —
	// CI runners have no real claude (this exact gap kept CI red for a day).
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := newTestScheduleStore(t)
	work := t.TempDir()
	enabled := false
	mk := func() *Schedule {
		sched, err := s.Create(ScheduleInput{
			Kind: "task", Cron: "0 9 * * *", Prompt: "x", Cwd: work, Permission: "plan", Enabled: &enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		return sched
	}
	a, b, c := mk(), mk(), mk()

	// Occupy all agentRunsDefault general slots with a and b.
	s.mu.Lock()
	s.taskRunning[a.ID] = true
	s.taskRunning[b.ID] = true
	s.mu.Unlock()

	// Same-schedule re-fire → single-flight skip.
	s.fireTask(a.ID, work, "x", "plan", "", 0)
	gotA, _ := s.Get(a.ID)
	if len(gotA.Runs) == 0 || !strings.Contains(gotA.Runs[0].Detail, "previous run still going") {
		t.Errorf("expected single-flight skip, got %+v", gotA.Runs)
	}

	// Third schedule → global cap skip.
	s.fireTask(c.ID, work, "x", "plan", "", 0)
	gotC, _ := s.Get(c.ID)
	if len(gotC.Runs) == 0 || !strings.Contains(gotC.Runs[0].Detail, "already in flight") {
		t.Errorf("expected concurrency-cap skip, got %+v", gotC.Runs)
	}
	if gotC.Runs[0].Status != "skipped" {
		t.Errorf("cap skip status = %q, want skipped", gotC.Runs[0].Status)
	}
}

func TestTaskRunCapsApplyLiveAndReserveIsInvisibleToTasks(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := newTestScheduleStore(t)
	work := t.TempDir()
	enabled := false
	mk := func() *Schedule {
		sched, err := s.Create(ScheduleInput{
			Kind: "task", Cron: "0 9 * * *", Prompt: "x", Cwd: work, Permission: "plan", Enabled: &enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		return sched
	}
	a, b := mk(), mk()
	s.SetRunCapsResolver(func() agentRunCaps { return testAgentRunCaps(1, 0) })
	s.mu.Lock()
	s.taskRunning["general-holder"] = true
	s.mu.Unlock()
	s.fireTask(a.ID, work, "x", "plan", "", 0)
	gotA, _ := s.Get(a.ID)
	if len(gotA.Runs) == 0 || gotA.Runs[0].Detail != "busy — 1 task runs already in flight" {
		t.Fatalf("initial cap was not applied: %+v", gotA.Runs)
	}

	// Re-tune without replacing the store. One general holder plus one reserved
	// bookkeeping holder leaves a general slot for a task schedule even though
	// len(taskRunning) already equals the configured general cap.
	s.SetRunCapsResolver(func() agentRunCaps { return testAgentRunCaps(2, 1) })
	s.mu.Lock()
	s.taskRunning["bookkeeping-holder"] = true
	s.taskReserved["bookkeeping-holder"] = true
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		releaseRunSlot(s.taskRunning, s.taskReserved, "general-holder")
		releaseRunSlot(s.taskRunning, s.taskReserved, "bookkeeping-holder")
		s.mu.Unlock()
	})
	s.fireTask(b.ID, work, "x", "plan", "", 0)
	deadline := time.Now().Add(10 * time.Second)
	finished := false
	for time.Now().Before(deadline) {
		gotB, _ := s.Get(b.ID)
		if len(gotB.Runs) > 0 {
			if gotB.Runs[0].Status == "skipped" {
				t.Fatalf("task counted the bookkeeping reserve against its general pool: %+v", gotB.Runs[0])
			}
			finished = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !finished {
		t.Fatal("retuned task run did not finish")
	}
	for s.taskRunningFor(b.ID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.taskRunningFor(b.ID) {
		t.Fatal("retuned task run did not release its slot")
	}
}

func TestTaskRunTimeoutKillsChild(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	s := newTestScheduleStore(t)
	enabled := false
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "x", Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sub-minute timeouts aren't expressible through TimeoutMin, so drive
	// runTaskProcess directly with a tiny ceiling via the exported path:
	// fireTask(…, timeoutMin) floors at 1 minute, which is too slow for a
	// unit test — instead assert the timeout plumbing on the default path by
	// checking that a killed child is reported as an error.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runTaskProcessWithTimeout(sched.ID, fake, work, "x", "plan", "", 120*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTaskProcess did not return after timeout")
	}
	got, _ := s.Get(sched.ID)
	if len(got.Runs) == 0 || got.Runs[0].Status != "error" ||
		!strings.Contains(got.Runs[0].Detail, "timed out") {
		t.Fatalf("expected timeout error run, got %+v", got.Runs)
	}
}

// MCP create_schedule must keep working for pre-task callers that pass
// pane_id + prompt with no `kind` at all.
func TestMcpCreateScheduleDefaultsToPromptKind(t *testing.T) {
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}
	res := a.mcpToolCreateSchedule(map[string]any{
		"pane_id": "p1", "prompt": "old caller", "cron": "0 9 * * *",
	}, "")
	if isToolErr(res) {
		t.Fatalf("legacy call failed: %v", res)
	}
	list := s.List()
	if len(list) != 1 || list[0].Kind != "" && list[0].Kind != "prompt" {
		t.Fatalf("expected a prompt-kind schedule, got %+v", list)
	}
	if list[0].PaneID != "p1" {
		t.Errorf("paneId = %q, want p1", list[0].PaneID)
	}
}

func TestMcpCreateScheduleTaskKind(t *testing.T) {
	work := t.TempDir()
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}

	// Missing cwd is rejected.
	if res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "x", "cron": "0 9 * * *",
	}, ""); !isToolErr(res) {
		t.Error("kind=task without cwd should error")
	}
	// Permission defaults to plan (read-only) when unspecified.
	res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "audit deps", "cron": "0 9 * * 1", "cwd": work,
	}, "")
	if isToolErr(res) {
		t.Fatalf("task create failed: %v", res)
	}
	list := s.List()
	if len(list) != 1 || list[0].Kind != "task" {
		t.Fatalf("expected a task schedule, got %+v", list)
	}
	if list[0].Permission != "plan" {
		t.Errorf("permission = %q, want plan (safe default)", list[0].Permission)
	}
	// The dangerous mode is unreachable through MCP too.
	if res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "x", "cron": "0 9 * * *", "cwd": work,
		"permission": "bypassPermissions",
	}, ""); !isToolErr(res) {
		t.Error("permission=bypassPermissions should be rejected via MCP")
	}
}

// findSchedule returns the first stored schedule matching pred.
func findSchedule(s *ScheduleStore, pred func(Schedule) bool) (Schedule, bool) {
	for _, sched := range s.List() {
		if pred(sched) {
			return sched, true
		}
	}
	return Schedule{}, false
}

// The caller pane is CREATOR provenance and nothing else: it never becomes the
// prompt target, it survives a store reload and an Update, and it stays nil for
// anything created through the plain (HTTP) path.
func TestMcpCreateScheduleStampsCreatedBy(t *testing.T) {
	work := t.TempDir()
	dir := t.TempDir()
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := transactionTestPane("pane-1")
	p.Title = "Orchestrator"
	a := &apiServer{scheds: s, mgr: testManagerWithPane(p)}
	wantOrigin := &ScheduleOrigin{Source: "mcp", PaneID: "pane-1", PaneTitle: "Orchestrator"}

	// kind=task forbids a TARGET pane (validate rejects paneId), so this case
	// also proves creator provenance is a separate field and not smuggled in.
	if res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "nightly audit", "cron": "0 9 * * 1", "cwd": work,
	}, "pane-1"); isToolErr(res) {
		t.Fatalf("task create failed: %v", res)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("schedules = %d, want 1", len(list))
	}
	taskSched := list[0]
	if !reflect.DeepEqual(taskSched.CreatedBy, wantOrigin) {
		t.Fatalf("createdBy = %+v, want %+v", taskSched.CreatedBy, wantOrigin)
	}
	if taskSched.PaneID != "" {
		t.Errorf("task schedule paneId = %q, want empty — creator origin is not a target", taskSched.PaneID)
	}

	// kind=prompt: the target pane comes from args, the creator from the header.
	if res := a.mcpToolCreateSchedule(map[string]any{
		"pane_id": "p-target", "prompt": "stand up", "cron": "0 9 * * *",
	}, "pane-1"); isToolErr(res) {
		t.Fatalf("prompt create failed: %v", res)
	}
	promptSched, found := findSchedule(s, func(sched Schedule) bool { return sched.PaneID == "p-target" })
	if !found {
		t.Fatal("prompt schedule did not keep its target pane")
	}
	if !reflect.DeepEqual(promptSched.CreatedBy, wantOrigin) {
		t.Errorf("prompt schedule createdBy = %+v, want %+v", promptSched.CreatedBy, wantOrigin)
	}

	// No caller pane (an older client, or a non-pane transport): the surface is
	// still worth recording; the pane simply isn't known.
	if res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "no pane", "cron": "0 10 * * 1", "cwd": work,
	}, ""); isToolErr(res) {
		t.Fatalf("headerless create failed: %v", res)
	}
	headerless, found := findSchedule(s, func(sched Schedule) bool { return sched.Prompt == "no pane" })
	if !found {
		t.Fatal("headerless schedule not found")
	}
	if !reflect.DeepEqual(headerless.CreatedBy, &ScheduleOrigin{Source: "mcp"}) {
		t.Errorf("headerless createdBy = %+v, want {source:mcp}", headerless.CreatedBy)
	}

	// Update must preserve it — no ScheduleInput field can carry it back.
	if _, err := s.Update(taskSched.ID, ScheduleInput{
		Kind: "task", Prompt: "nightly audit v2", Cron: "0 9 * * 1", Cwd: work, Permission: "plan",
	}); err != nil {
		t.Fatal(err)
	}
	if updated, _ := s.Get(taskSched.ID); !reflect.DeepEqual(updated.CreatedBy, wantOrigin) {
		t.Errorf("createdBy after Update = %+v, want %+v", updated.CreatedBy, wantOrigin)
	}

	// The plain Create (POST /api/schedules) has no pane to name.
	plain, err := s.Create(ScheduleInput{Cron: "0 11 * * *", PaneID: "p2", Prompt: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.CreatedBy != nil {
		t.Errorf("HTTP-created schedule createdBy = %+v, want nil", plain.CreatedBy)
	}

	// Round-trip through the store file.
	s.Stop()
	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	reloaded, ok := s2.Get(taskSched.ID)
	if !ok {
		t.Fatal("schedule missing after reload")
	}
	if !reflect.DeepEqual(reloaded.CreatedBy, wantOrigin) {
		t.Errorf("createdBy after reload = %+v, want %+v", reloaded.CreatedBy, wantOrigin)
	}
	if replain, ok := s2.Get(plain.ID); !ok || replain.CreatedBy != nil {
		t.Errorf("HTTP-created schedule gained provenance across a reload: %+v", replain)
	}
}

func TestMcpSetScheduleEnabled(t *testing.T) {
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}
	sched, err := s.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: "p1", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res := a.mcpToolSetScheduleEnabled(map[string]any{"id": sched.ID, "enabled": false}); isToolErr(res) {
		t.Fatalf("pause failed: %v", res)
	}
	if got, _ := s.Get(sched.ID); got.Enabled {
		t.Error("schedule still enabled after pause")
	}
	if res := a.mcpToolSetScheduleEnabled(map[string]any{"id": sched.ID, "enabled": true}); isToolErr(res) {
		t.Fatalf("resume failed: %v", res)
	}
	if got, _ := s.Get(sched.ID); !got.Enabled {
		t.Error("schedule still disabled after resume")
	}
	// Missing/!bool enabled is an error, not a silent false.
	if res := a.mcpToolSetScheduleEnabled(map[string]any{"id": sched.ID}); !isToolErr(res) {
		t.Error("missing enabled should error")
	}
}

// Hammer the store from many goroutines: concurrent fires, reads, DTO builds,
// enable/disable, and pusher swaps. Run under -race to catch lock-discipline
// slips (e.g. reading s.pusher or s.taskRunning outside the mutex).
func TestScheduleStoreConcurrentAccess(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := t.TempDir()
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}
	enabled := false
	var ids []string
	for i := 0; i < 4; i++ {
		sched, err := s.Create(ScheduleInput{
			Kind: "task", Cron: "0 9 * * *", Prompt: "x",
			Cwd: work, Permission: "plan", Enabled: &enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sched.ID)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				s.fireTask(id, work, "x", "plan", "", 0)
				s.taskRunningFor(id)
				if sched, ok := s.Get(id); ok {
					a.makeScheduleDTO(sched)
				}
				_, _ = s.SetEnabled(id, true)
				_, _ = s.SetEnabled(id, false)
				s.List()
			}(id)
		}
	}
	// Concurrently swap the pusher in and out, as cloud connect/disconnect does.
	// NewPusher (not &Pusher{}) so any push this provokes hits its initialized
	// rate-limit map rather than nil-map-panicking on a landmine of our own making.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p := NewPusher("http://127.0.0.1:1", "tok", nil, nil, nil)
		for i := 0; i < 50; i++ {
			s.SetPusher(nil)
			s.SetPusher(p)
		}
	}()
	wg.Wait()
	s.SetPusher(nil) // don't let a late goroutine POST after the test ends

	// Every in-flight slot must have been released once the runs settle.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.taskRunning)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	leaked := len(s.taskRunning)
	s.mu.Unlock()
	t.Fatalf("%d task slots leaked after all runs finished", leaked)
}

// CatchUp: a task schedule that missed recurring fires while the daemon was
// down runs ONCE shortly after startup. Opt-in, task-kind only.
func TestCatchUpRunsOnceAfterBoot(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho caught-up\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	prevDelay := catchUpDelay
	catchUpDelay = 50 * time.Millisecond
	t.Cleanup(func() { catchUpDelay = prevDelay })

	work := t.TempDir()
	dir := t.TempDir()
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 * * * *", Prompt: "nightly report",
		Cwd: work, Permission: "plan", CatchUp: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Backdate: last fired 3h ago, then "downtime" until this reload.
	s.mu.Lock()
	past := time.Now().Add(-3 * time.Hour)
	s.items[sched.ID].LastFiredAt = &past
	s.items[sched.ID].CreatedAt = past
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop()

	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()

	// The synthetic skip should announce the catch-up...
	got, _ := s2.Get(sched.ID)
	if len(got.Runs) == 0 || !strings.Contains(got.Runs[0].Detail, "catching up with one run") {
		t.Fatalf("synthetic run missing catch-up note: %+v", got.Runs)
	}
	// ...and one real fired run should land after the (shrunken) delay.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = s2.Get(sched.ID)
		if got.FireCount >= 1 {
			if got.FireCount > 1 {
				t.Fatalf("FireCount = %d, want exactly 1 (catch-up must not replay every miss)", got.FireCount)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("catch-up run never fired; runs=%+v", got.Runs)
}

// Without CatchUp the missed-run record must NOT trigger a boot fire — the
// original no-catch-up policy is the default.
func TestNoCatchUpWithoutOptIn(t *testing.T) {
	prevDelay := catchUpDelay
	catchUpDelay = 30 * time.Millisecond
	t.Cleanup(func() { catchUpDelay = prevDelay })

	work := t.TempDir()
	dir := t.TempDir()
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 * * * *", Prompt: "x",
		Cwd: work, Permission: "plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	past := time.Now().Add(-3 * time.Hour)
	s.items[sched.ID].LastFiredAt = &past
	s.items[sched.ID].CreatedAt = past
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop()

	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	time.Sleep(300 * time.Millisecond)
	got, _ := s2.Get(sched.ID)
	if got.FireCount != 0 {
		t.Fatalf("FireCount = %d, want 0 without opt-in", got.FireCount)
	}
}

func TestCatchUpValidation(t *testing.T) {
	work := t.TempDir()
	// prompt-kind + catchUp → rejected
	in := ScheduleInput{Cron: "0 * * * *", PaneID: "p1", Prompt: "x", CatchUp: true}
	if err := in.validate(); err == nil || !strings.Contains(err.Error(), "Claude-task") {
		t.Errorf("prompt+catchUp: %v", err)
	}
	// one-time task + catchUp → rejected
	at := time.Now().Add(time.Hour)
	in2 := ScheduleInput{Kind: "task", RunAt: &at, Prompt: "x", Cwd: work, Permission: "plan", CatchUp: true}
	if err := in2.validate(); err == nil || !strings.Contains(err.Error(), "recurring") {
		t.Errorf("one-time+catchUp: %v", err)
	}
}

// taskOutputPreview: the push body is the report's opening, capped without
// mid-word chops. This string IS what the user reads on their lockscreen.
func TestTaskOutputPreview(t *testing.T) {
	if got := taskOutputPreview([]byte("  Short report.\n")); got != "Short report." {
		t.Errorf("short: %q", got)
	}
	if got := taskOutputPreview(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	long := strings.Repeat("word ", 200) // 1000 chars
	got := taskOutputPreview([]byte(long))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long output not truncated with ellipsis: %q", got)
	}
	if n := len([]rune(got)); n > taskPreviewMaxRunes+1 {
		t.Errorf("preview too long: %d runes", n)
	}
	if strings.Contains(got, "wor…") || strings.Contains(strings.TrimSuffix(got, "…"), "wo\n") {
		t.Errorf("preview chopped mid-word: %q", got)
	}
	// One giant token: hard chop is the only option — must still cap.
	giant := strings.Repeat("x", 1000)
	if n := len([]rune(taskOutputPreview([]byte(giant)))); n > taskPreviewMaxRunes+1 {
		t.Errorf("giant token preview too long: %d runes", n)
	}
}

// A successful notify-on-run task push carries the run Summary (the report
// lede) as its body; runs without a summary fall back to the status Detail.
func TestRecordRunPushBodyPrefersSummary(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sent":1}`))
	}))
	defer srv.Close()

	work := t.TempDir()
	s, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.SetPusher(NewPusher(srv.URL, "tok", nil, nil, nil))
	defer s.SetPusher(nil)

	withSummary, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 * * * *", Prompt: "digest", Cwd: work,
		Permission: "plan", NotifyOnRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	noSummary, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 * * * *", Prompt: "digest2", Cwd: work,
		Permission: "plan", NotifyOnRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	failed, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 * * * *", Prompt: "digest3", Cwd: work,
		Permission: "plan",
	})
	if err != nil {
		t.Fatal(err)
	}

	s.recordRun(withSummary.ID, ScheduleRun{
		At: time.Now(), Status: "fired",
		Detail:  "plan · finished in 42s",
		Summary: "3 PRs merged overnight; CI green; issue #12 needs your call.",
	})
	s.recordRun(noSummary.ID, ScheduleRun{
		At: time.Now(), Status: "fired",
		Detail: "plan · finished in 9s",
	})
	s.recordRun(failed.ID, ScheduleRun{
		At: time.Now(), Status: "error",
		Detail:  "plan · exit 1 in 3s",
		Summary: "failed: error: build compilation failed",
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("deliveries = %d, want 3", len(bodies))
	}
	got := map[string]string{}
	for _, b := range bodies {
		got[b["paneId"]] = b["summary"]
	}
	if got["sched:"+withSummary.ID] != "3 PRs merged overnight; CI green; issue #12 needs your call." {
		t.Errorf("summary-run push body = %q, want the report lede", got["sched:"+withSummary.ID])
	}
	if got["sched:"+noSummary.ID] != "plan · finished in 9s" {
		t.Errorf("no-summary push body = %q, want the detail fallback", got["sched:"+noSummary.ID])
	}
	if want := "failed: error: build compilation failed (plan · exit 1 in 3s)"; got["sched:"+failed.ID] != want {
		t.Errorf("failed-run push body = %q, want %q", got["sched:"+failed.ID], want)
	}
}

func TestLastErrorLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"only whitespace", "   \n\t  \n  ", ""},
		{"single line", "error: command failed", "error: command failed"},
		{"multiple lines with trailing newlines", "step 1\nstep 2\nerror: out of memory\n\n\n", "error: out of memory"},
		{"trims spaces around line", "line 1\n   error: permission denied   \n", "error: permission denied"},
		{"runaway long line truncated", strings.Repeat("x", 300), strings.Repeat("x", taskPreviewMaxRunes) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastErrorLine([]byte(tc.in))
			if got != tc.want {
				t.Errorf("lastErrorLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTaskProcessRunSummaryOnFailure(t *testing.T) {
	bin := t.TempDir()
	fakeFail := filepath.Join(bin, "claude-fail")
	script := "#!/bin/sh\n" +
		"echo \"info: starting test suite\"\n" +
		"echo \"error: build compilation failed on step 3\"\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeFail, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	s := newTestScheduleStore(t)
	enabled := false
	sched, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "test fail", Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	s.runTaskProcessWithTimeout(sched.ID, fakeFail, work, "test fail", "plan", "", 5*time.Second)

	got, ok := s.Get(sched.ID)
	if !ok || len(got.Runs) == 0 {
		t.Fatalf("expected recorded run for %s", sched.ID)
	}
	run := got.Runs[0]
	if run.Status != "error" {
		t.Errorf("run.Status = %q, want error", run.Status)
	}
	wantSummary := "failed: error: build compilation failed on step 3"
	if run.Summary != wantSummary {
		t.Errorf("run.Summary = %q, want %q", run.Summary, wantSummary)
	}

	// Test case where failure produces zero output
	fakeSilentFail := filepath.Join(bin, "claude-silent-fail")
	if err := os.WriteFile(fakeSilentFail, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	schedSilent, err := s.Create(ScheduleInput{
		Kind: "task", Cron: "0 9 * * *", Prompt: "silent fail", Cwd: work, Permission: "plan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	s.runTaskProcessWithTimeout(schedSilent.ID, fakeSilentFail, work, "silent fail", "plan", "", 5*time.Second)

	gotSilent, ok := s.Get(schedSilent.ID)
	if !ok || len(gotSilent.Runs) == 0 {
		t.Fatalf("expected recorded run for %s", schedSilent.ID)
	}
	if gotSilent.Runs[0].Summary != "failed" {
		t.Errorf("run.Summary = %q, want failed", gotSilent.Runs[0].Summary)
	}
}

func TestTaskPromptStdinCarriesLastReportLede(t *testing.T) {
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.Local)
	runs := []ScheduleRun{
		{At: at.Add(2 * time.Hour), Status: "skipped", Detail: "busy"},
		{At: at.Add(time.Hour), Status: "error", Summary: "failed: exit 1"},
		{At: at, Status: "fired", Summary: "## Needs you\n- merge PR #7"},
	}
	prev := lastReportRun(runs)
	if prev == nil || !prev.At.Equal(at) {
		t.Fatalf("lastReportRun = %+v, want the newest fired run with a summary", prev)
	}
	got := taskPromptStdin("check the build", prev, true)
	for _, want := range []string{
		"check the build",
		"only for a decision or action the user must take that is new since your last report",
		"\"Nothing new needs you.\"",
		"(Your last report, from Tue 27 Oct 2026, began:\n## Needs you\n- merge PR #7\n)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdin missing %q:\n%s", want, got)
		}
	}
	if lastReportRun(runs[:2]) != nil {
		t.Error("errors and skips are not reports")
	}
	if strings.Contains(got, "has not been read") || strings.Contains(got, "not read that report") {
		t.Error("a read previous report needs no carry-forward note")
	}
	// An unread previous report: its open asks must be carried, not dropped.
	if unread := taskPromptStdin("check the build", prev, false); !strings.Contains(unread,
		"The user has not read that report yet: keep any of its asks that are still open under \"## Needs you\".") {
		t.Errorf("unread previous report must ask to carry open asks:\n%s", unread)
	}
	if strings.Contains(taskPromptStdin("p", nil, false), "Your last report") {
		t.Error("no previous report → no comparison block")
	}
	// Managed agents keep the original contract; only tasks changed.
	if !strings.Contains(reportStyleHint, "Nothing needs you today.") {
		t.Error("reportStyleHint (agents) must stay unchanged")
	}
}

func TestTaskReportSeenIgnoresAReadMarkerMovedByALaterFailure(t *testing.T) {
	mon := time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local)
	report := ScheduleRun{At: mon, Status: "fired", Summary: "## Needs you\n- approve PR #7"}
	failed := ScheduleRun{At: mon.Add(24 * time.Hour), Status: "error", Summary: "failed: exit 1"}
	skipped := ScheduleRun{At: mon.Add(25 * time.Hour), Status: "skipped"}
	at := func(t time.Time) *time.Time { return &t }

	read := Schedule{LastReadAt: at(mon), Runs: []ScheduleRun{skipped, report}}
	if !taskReportSeen(read, lastReportRun(read.Runs)) {
		t.Error("the report was opened and is still the newest run: seen")
	}
	// The user opened Tuesday's failure, never Monday's report.
	viaFailure := Schedule{LastReadAt: at(failed.At), Runs: []ScheduleRun{failed, report}}
	if taskReportSeen(viaFailure, lastReportRun(viaFailure.Runs)) {
		t.Error("a marker moved by a later failed run must not count the report as seen")
	}
	unread := Schedule{Runs: []ScheduleRun{report}}
	if taskReportSeen(unread, lastReportRun(unread.Runs)) {
		t.Error("never opened: not seen")
	}
}
