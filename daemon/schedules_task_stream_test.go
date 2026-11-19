package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Automations stage 2 (ST-299 PR 2): task runs stream into a file; the model,
// tokens and cost come from the result; a run SoluTerminal stopped during is
// finished at the next start.

const streamOK = `{"type":"system","subtype":"init","model":"claude-sonnet-4-5","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"Nothing new needs you.\n\n## Digest\n- two PRs merged","total_cost_usd":0.0421,"usage":{"input_tokens":10,"output_tokens":20},"modelUsage":{"claude-sonnet-4-5":{"inputTokens":1200,"outputTokens":340,"cacheReadInputTokens":5000,"cacheCreationInputTokens":800},"claude-haiku-4-5":{"inputTokens":100,"outputTokens":12}}}
`

// fakeClaudeStream is a claude on PATH that prints events (single quotes
// are not allowed in them).
func fakeClaudeStream(t *testing.T, events string, exit int) {
	t.Helper()
	if strings.Contains(events, "'") {
		t.Fatal("events must not hold a single quote")
	}
	fakeClaude(t, "printf '%s' '"+events+"'\nexit "+strconv.Itoa(exit)+"\n")
}

func taskSchedule(t *testing.T, s *ScheduleStore, in ScheduleInput) *Schedule {
	t.Helper()
	off := false
	in.Kind, in.Permission = "task", "plan"
	if in.Cwd == "" {
		in.Cwd = t.TempDir()
	}
	if in.Prompt == "" {
		in.Prompt = "digest"
	}
	if in.Cron == "" && in.RunAt == nil {
		in.Cron = "0 9 * * *"
	}
	if in.Enabled == nil {
		in.Enabled = &off
	}
	sched, err := s.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	return sched
}

func TestTaskRunReadsModelTokensAndCostFromItsStream(t *testing.T) {
	fakeClaudeStream(t, streamOK, 0)
	s := newTestScheduleStore(t)
	sched := taskSchedule(t, s, ScheduleInput{Model: "sonnet"})
	s.fireTask(sched.ID, sched.Cwd, "digest", "plan", "sonnet", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	r := got.Runs[0]
	if r.Status != "fired" || r.Outcome != runOutcomeWorked {
		t.Fatalf("run = %s/%s %q, want a worked run", r.Status, r.Outcome, r.Detail)
	}
	if r.Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want the one that wrote most", r.Model)
	}
	u := r.Usage
	if u == nil || u.InputTokens != 1300 || u.OutputTokens != 352 || u.CacheReadTokens != 5000 || u.CacheWriteTokens != 800 || u.Turns != 3 {
		t.Fatalf("usage = %+v, want the modelUsage totals", u)
	}
	if u.CostUSD == nil || *u.CostUSD != 0.0421 {
		t.Errorf("cost = %v, want 0.0421", u.CostUSD)
	}
	if !strings.HasPrefix(r.Summary, "Nothing new needs you.") {
		t.Errorf("summary = %q, want the result text", r.Summary)
	}
	logData, err := os.ReadFile(filepath.Join(s.taskRunDir(sched.ID), r.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(logData); !strings.Contains(body, "## Digest") || strings.Contains(body, `"type":"system"`) {
		t.Errorf("log = %q, want the readable report, not the events", body)
	}
	// Recorded: no events file or in-flight record is left.
	ents, _ := os.ReadDir(s.taskRunDir(sched.ID))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), taskEventsSuffix) || strings.HasSuffix(e.Name(), taskInflightSuffix) {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	// And the history line carries the usage.
	runs := historyRuns(historyOf(t, s, sched.ID, historyQuery{}))
	if len(runs) != 1 || runs[0].Usage == nil || runs[0].Usage.OutputTokens != 352 {
		t.Errorf("history run = %+v", runs)
	}
}

func TestTaskRunWithAnErrorResultFails(t *testing.T) {
	fakeClaudeStream(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["Reached maximum number of turns (15)"],"num_turns":15}
`, 0)
	s := newTestScheduleStore(t)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.fireTask(sched.ID, sched.Cwd, "digest", "plan", "", 0)
	r := waitTaskIdle(t, s, sched.ID, 1).Runs[0]
	if r.Status != "error" || r.Cause != runCauseRunFailed || !strings.Contains(r.Summary, "Reached maximum number of turns") {
		t.Fatalf("run = %+v, want a failure naming the CLI's reason", r)
	}
}

// oneTimeDueTask is a one-time task whose time came while its run was going:
// still on, its moment past.
func oneTimeDueTask(t *testing.T, s *ScheduleStore) *Schedule {
	t.Helper()
	future := time.Now().Add(time.Hour)
	sched := taskSchedule(t, s, ScheduleInput{RunAt: &future})
	past := time.Now().Add(-time.Minute)
	s.mu.Lock()
	s.items[sched.ID].RunAt = &past
	s.items[sched.ID].Enabled = true
	_ = s.saveUnlocked()
	s.mu.Unlock()
	return sched
}

// writeInflight leaves a run as a SoluTerminal that stopped mid-run would.
func writeInflight(t *testing.T, dataDir, schedID string, rec taskInflight, events string) taskRunFiles {
	t.Helper()
	dir := filepath.Join(dataDir, "schedule-runs", schedID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := taskRunFiles{dir: dir, stamp: rec.Started.UnixMilli()}
	if err := os.WriteFile(files.events(), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.SchedID = schedID
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(files.inflight(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return files
}

// deadPID is a pid of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no `true` to run")
	}
	return cmd.Process.Pid
}

func reopen(t *testing.T, dir string) *ScheduleStore {
	t.Helper()
	old := taskRecoverDelay
	taskRecoverDelay = 0
	t.Cleanup(func() { taskRecoverDelay = old })
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

func TestStartFinishesARunThatEndedWhileTheAppWasStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := oneTimeDueTask(t, s)
	s.Stop()
	started := time.Now().Add(-30 * time.Second)
	files := writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_before", PID: deadPID(t), Started: started, TimeoutMs: 60000, Permission: "plan", Trigger: runTriggerScheduled, Header: "# header\n\n"}, streamOK)

	s2 := reopen(t, dir)
	got := waitRecovered(t, s2, sched.ID)
	if len(got.Runs) == 0 {
		t.Fatal("no run recorded")
	}
	r := got.Runs[0]
	if r.ID != "run_before" || r.Status != "fired" || r.Usage == nil || r.Cause == runCauseMissedAppOff {
		t.Fatalf("run = %+v, want its real result from the events file", r)
	}
	for _, other := range got.Runs[1:] {
		if other.Cause == runCauseMissedAppOff {
			t.Errorf("also recorded as missed: %+v", other)
		}
	}
	if got.Enabled {
		t.Error("the one-time task is still on after its run was recorded")
	}
	if _, err := os.Stat(files.inflight()); !os.IsNotExist(err) {
		t.Error("in-flight record left behind")
	}
	if r.LogFile == "" {
		t.Error("no log written")
	}
}

func TestStartRecordsARunWithNoResultAsInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_cut", PID: deadPID(t), Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"},
		`{"type":"system","subtype":"init","model":"m"}`+"\n")
	s2 := reopen(t, dir)
	got := waitRecovered(t, s2, sched.ID)
	if r := got.Runs[0]; r.Status != "error" || r.Cause != runCauseInterrupted || r.Outcome != runOutcomeFailed {
		t.Fatalf("run = %+v, want an interrupted failure", r)
	}
}

func TestStartFollowsARunStillGoing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	old := taskFollowPoll
	taskFollowPoll = 20 * time.Millisecond
	t.Cleanup(func() { taskFollowPoll = old })
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := oneTimeDueTask(t, s)
	s.Stop()

	proc := exec.Command("sleep", "1")
	applyDetached(proc)
	if err := proc.Start(); err != nil {
		t.Skip("no sleep")
	}
	exited := make(chan struct{})
	go func() { _ = proc.Wait(); close(exited) }()
	rec := taskInflight{RunID: "run_live", PID: proc.Process.Pid, Started: time.Now(), TimeoutMs: 60000, Permission: "plan"}
	rec.StartTicks = startTicksOf(t, proc.Process.Pid)
	writeInflight(t, dir, sched.ID, rec, streamOK)

	s2 := reopen(t, dir)
	if !s2.taskRunningFor(sched.ID) {
		t.Fatal("the run still going does not hold its slot")
	}
	if got, _ := s2.Get(sched.ID); len(got.Runs) != 0 {
		t.Fatalf("recorded before it ended (missed?): %+v", got.Runs)
	}
	<-exited
	got := waitTaskIdle(t, s2, sched.ID, 1)
	if r := got.Runs[0]; r.ID != "run_live" || r.Status != "fired" {
		t.Fatalf("followed run = %+v", r)
	}
	if got.Enabled {
		t.Error("the one-time task is still on")
	}
}

func TestStartKeepsTheTimeLimitOfARunItFollows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	old := taskFollowPoll
	taskFollowPoll = 20 * time.Millisecond
	t.Cleanup(func() { taskFollowPoll = old })
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	proc := exec.Command("sleep", "30")
	applyDetached(proc)
	if err := proc.Start(); err != nil {
		t.Skip("no sleep")
	}
	exited := make(chan struct{})
	go func() { _ = proc.Wait(); close(exited) }()
	t.Cleanup(func() { _ = killChildTree(proc.Process) })
	rec := taskInflight{RunID: "run_long", PID: proc.Process.Pid, Started: time.Now().Add(-time.Hour), TimeoutMs: 1000, Permission: "plan"}
	rec.StartTicks = startTicksOf(t, proc.Process.Pid)
	writeInflight(t, dir, sched.ID, rec, "")
	s2 := reopen(t, dir)
	got := waitTaskIdle(t, s2, sched.ID, 1)
	if r := got.Runs[0]; r.Cause != runCauseTimeout {
		t.Fatalf("run = %+v, want timed out", r)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the run past its time limit was not stopped")
	}
}

func TestTaskStreamReportFallsBackToPlainOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e")
	if err := os.WriteFile(path, []byte("Error: claude crashed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newTaskStream(path)
	st.finish()
	if got := string(st.report()); got != "Error: claude crashed" {
		t.Errorf("report = %q", got)
	}
	if o := st.outcome(); o.hasResult || o.usage != nil {
		t.Errorf("outcome = %+v, want none", o)
	}
}

func TestTaskRunPastItsOutputLimitIsStopped(t *testing.T) {
	oldLimit, oldPoll := taskEventsLimit, taskStreamPoll
	taskEventsLimit, taskStreamPoll = 4096, 10*time.Millisecond
	t.Cleanup(func() { taskEventsLimit, taskStreamPoll = oldLimit, oldPoll })
	fakeClaude(t, "i=0; while [ $i -lt 200 ]; do echo 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; i=$((i+1)); done\nsleep 30\n")
	s := newTestScheduleStore(t)
	sched := taskSchedule(t, s, ScheduleInput{})
	begin := time.Now()
	s.fireTask(sched.ID, sched.Cwd, "digest", "plan", "", 0)
	r := waitTaskIdle(t, s, sched.ID, 1).Runs[0]
	if r.Status != "error" || !strings.Contains(r.Detail, "output passed") {
		t.Fatalf("run = %+v, want stopped for too much output", r)
	}
	if time.Since(begin) > 20*time.Second {
		t.Error("the run was not stopped when its output passed the limit")
	}
}

// waitRecovered waits until the runs from before the restart are recorded
// (they hold the automation's slot until then) and returns the automation.
func waitRecovered(t *testing.T, s *ScheduleStore, id string) Schedule {
	t.Helper()
	waitUntil(t, "the recovered run to be recorded", func() bool { return !s.taskRunningFor(id) })
	got, _ := s.Get(id)
	return got
}

// startTicksOf is a process's start time, or skips where it cannot be read
// (only then is a process followed or stopped).
func startTicksOf(t *testing.T, pid int) uint64 {
	t.Helper()
	_, ticks, ok := readProcCPUTicks(pid)
	if !ok {
		t.Skip("process start times are not readable here")
	}
	return ticks
}

// A live process whose start time does not match (a pid used again) is not
// taken for the run: it is neither followed nor stopped.
func TestStartNeverTakesAnotherProcessForTheRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	old := taskFollowPoll
	taskFollowPoll = 10 * time.Millisecond
	t.Cleanup(func() { taskFollowPoll = old })
	proc := exec.Command("sleep", "30")
	applyDetached(proc)
	if err := proc.Start(); err != nil {
		t.Skip("no sleep")
	}
	t.Cleanup(func() { _ = killChildTree(proc.Process); _ = proc.Wait() })
	ticks := startTicksOf(t, proc.Process.Pid)
	for _, tc := range []struct {
		ticks uint64
		want  string
	}{
		// No start time (macOS, Windows): waited for up to its time limit,
		// never signalled, then recorded with its pid named.
		{0, "may still be running"},
		// The wrong start time (a pid used again): not the run at all.
		{ticks + 1, "ended without a result"},
	} {
		dir := t.TempDir()
		s := reopen(t, dir)
		sched := taskSchedule(t, s, ScheduleInput{})
		s.Stop()
		writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_other", PID: proc.Process.Pid, StartTicks: tc.ticks, Started: time.Now().Add(-time.Hour), TimeoutMs: 1000, Permission: "plan"}, "")
		got := waitRecovered(t, reopen(t, dir), sched.ID)
		if r := got.Runs[0]; !strings.Contains(r.Detail, tc.want) || r.Cause != runCauseInterrupted {
			t.Errorf("start ticks %d: run = %+v, want %q", tc.ticks, r, tc.want)
		}
		if !processAlive(proc.Process.Pid) {
			t.Fatalf("start ticks %d: another process was stopped", tc.ticks)
		}
	}
}

// A run saved just before SoluTerminal stopped (its files not yet removed)
// is not recorded a second time.
func TestStartDoesNotRecordASavedRunTwice(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.recordRun(sched.ID, ScheduleRun{ID: "run_saved", Status: "fired", Detail: "done"})
	s.Stop()
	files := writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_saved", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"}, streamOK)
	got := waitRecovered(t, reopen(t, dir), sched.ID)
	if len(got.Runs) != 1 || got.FireCount != 1 {
		t.Fatalf("runs = %d, fireCount = %d, want the one saved run", len(got.Runs), got.FireCount)
	}
	if _, err := os.Stat(files.inflight()); !os.IsNotExist(err) {
		t.Error("files of the saved run left behind")
	}
}

// A run that hit the usage limit while SoluTerminal was stopped is handled
// as one this daemon saw: a one-time task moves past the reset, not lost.
func TestStartAppliesTheUsageLimitToARecoveredRun(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := oneTimeDueTask(t, s)
	s.Stop()
	limit := `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your weekly limit · resets 11:59pm","num_turns":1}` + "\n"
	if strings.Contains(limit, "'") {
		limit = strings.ReplaceAll(limit, "'", "")
	}
	writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_limit", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"}, limit)
	s2 := reopen(t, dir)
	got := waitRecovered(t, s2, sched.ID)
	if got.Runs[0].Cause != runCauseUsageLimit {
		t.Fatalf("run = %+v, want a usage-limit cause", got.Runs[0])
	}
	if !got.Enabled || got.RunAt == nil || !got.RunAt.After(time.Now()) {
		t.Errorf("one-time task: enabled %v runAt %v, want it moved past the reset", got.Enabled, got.RunAt)
	}
	if _, held := s2.heldByUsageLimit(time.Now()); !held {
		t.Error("task runs are not held after a recovered usage-limit failure")
	}
}

// A run that ends the moment it is followed is recorded after the store is
// set up (run with -race).
func TestStartFollowsARunThatEndsAtOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	old := taskFollowPoll
	taskFollowPoll = time.Millisecond
	t.Cleanup(func() { taskFollowPoll = old })
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	proc := exec.Command("sleep", "0.05")
	applyDetached(proc)
	if err := proc.Start(); err != nil {
		t.Skip("no sleep")
	}
	go func() { _ = proc.Wait() }()
	rec := taskInflight{RunID: "run_quick", PID: proc.Process.Pid, Started: time.Now(), TimeoutMs: 60000, Permission: "plan"}
	rec.StartTicks = startTicksOf(t, proc.Process.Pid)
	writeInflight(t, dir, sched.ID, rec, streamOK)
	s2 := reopen(t, dir)
	got := waitTaskIdle(t, s2, sched.ID, 1)
	if got.Runs[0].ID != "run_quick" {
		t.Fatalf("runs = %+v", got.Runs)
	}
}

func TestTaskStreamBoundsAnUnendedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e")
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), taskStreamLineMax+(200<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newTaskStream(path)
	st.poll()
	st.mu.Lock()
	rem := len(st.rem)
	st.mu.Unlock()
	if rem > taskStreamLineMax {
		t.Errorf("an unended line holds %d bytes, want at most %d", rem, taskStreamLineMax)
	}
}

func TestTaskStreamReadsNoFurtherThanTheLimit(t *testing.T) {
	old := taskEventsLimit
	taskEventsLimit = 1000
	t.Cleanup(func() { taskEventsLimit = old })
	path := filepath.Join(t.TempDir(), "e")
	if err := os.WriteFile(path, bytes.Repeat([]byte("line\n"), 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newTaskStream(path)
	if size := st.poll(); size != 10000 {
		t.Errorf("poll size = %d, want the file's 10000", size)
	}
	st.finish()
	if st.off > taskEventsLimit {
		t.Errorf("read %d bytes, limit %d", st.off, taskEventsLimit)
	}
}

// A task run whose record does not reach disk keeps its files, and the next
// start records it once, with one history line.
func TestATaskRunWhoseSaveFailedIsRecordedOnceAtTheNextStart(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot write")
	}
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	files := writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_unsaved", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"}, streamOK)
	// schedules.json cannot be replaced: the save fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	run := ScheduleRun{ID: "run_unsaved", At: time.Now().Add(-time.Minute), Status: "fired", Detail: "done"}
	s.recordTaskResult(sched.ID, run, []byte("done"), files)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files.inflight()); err != nil {
		t.Fatalf("recovery files removed although the record was not saved: %v", err)
	}
	s.Stop()
	s2 := reopen(t, dir)
	got := waitRecovered(t, s2, sched.ID)
	if len(got.Runs) != 1 || got.Runs[0].ID != "run_unsaved" {
		t.Fatalf("runs after the next start = %+v, want the run once", got.Runs)
	}
	if runs := historyRuns(historyOf(t, s2, sched.ID, historyQuery{})); len(runs) != 1 {
		t.Fatalf("history has %d lines for the run, want 1", len(runs))
	}
	if _, err := os.Stat(files.inflight()); !os.IsNotExist(err) {
		t.Error("recovery files left after the run was recorded")
	}
}

// A run followed after a restart is held to the same output limit.
func TestStartKeepsTheOutputLimitOfARunItFollows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process test")
	}
	oldPoll, oldLimit := taskFollowPoll, taskEventsLimit
	taskFollowPoll, taskEventsLimit = 20*time.Millisecond, 1000
	t.Cleanup(func() { taskFollowPoll, taskEventsLimit = oldPoll, oldLimit })
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	proc := exec.Command("sleep", "30")
	applyDetached(proc)
	if err := proc.Start(); err != nil {
		t.Skip("no sleep")
	}
	exited := make(chan struct{})
	go func() { _ = proc.Wait(); close(exited) }()
	t.Cleanup(func() { _ = killChildTree(proc.Process) })
	rec := taskInflight{RunID: "run_big", PID: proc.Process.Pid, Started: time.Now(), TimeoutMs: 600000, Permission: "plan"}
	rec.StartTicks = startTicksOf(t, proc.Process.Pid)
	writeInflight(t, dir, sched.ID, rec, strings.Repeat("noise\n", 400))
	s2 := reopen(t, dir)
	got := waitTaskIdle(t, s2, sched.ID, 1)
	if r := got.Runs[0]; r.Status != "error" || !strings.Contains(r.Detail, "output passed") {
		t.Fatalf("run = %+v, want stopped for too much output", r)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the run past its output limit was not stopped")
	}
}

// A run that failed while the app was off is recorded once the store is
// wired, so its failure is pushed like any other.
func TestARecoveredFailedRunIsPushed(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_failed", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"},
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the tool crashed"]}`+"\n")
	relay := newPushRelay(t)
	old := taskRecoverDelay
	taskRecoverDelay = 200 * time.Millisecond // the boot wiring below lands first
	t.Cleanup(func() { taskRecoverDelay = old })
	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	s2.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))
	got := waitRecovered(t, s2, sched.ID)
	if r := got.Runs[0]; r.ID != "run_failed" || r.Status != "error" {
		t.Fatalf("run = %+v", r)
	}
	waitUntil(t, "the failure push", func() bool {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		for _, p := range relay.pushes {
			if p["state"] == "schedule-error" && strings.Contains(p["summary"], "the tool crashed") {
				return true
			}
		}
		return false
	})
}

// An over-long event line (a tool's output: file contents, command output) is
// dropped whole. None of it reaches the run log, the summary or a push.
func TestACutToolOutputNeverLeavesTheEventsFile(t *testing.T) {
	oldMax := taskStreamLineMax
	taskStreamLineMax = 4096
	t.Cleanup(func() { taskStreamLineMax = oldMax })
	const secret = "SECRET-FILE-CONTENTS"
	long := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + secret + strings.Repeat("x", 3*4096) + secret + `"}]}}`

	// Followed by an error result, and at the very end with no newline.
	for _, events := range []string{
		long + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"]}` + "\n",
		`{"type":"system","subtype":"init","model":"m"}` + "\n" + long,
	} {
		dir := t.TempDir()
		s := reopen(t, dir)
		sched := taskSchedule(t, s, ScheduleInput{})
		s.Stop()
		writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_cut", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan", Header: "# h\n\n"}, events)
		relay := newPushRelay(t)
		old := taskRecoverDelay
		taskRecoverDelay = 100 * time.Millisecond
		s2, err := NewScheduleStore(dir, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s2.Stop)
		s2.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))
		got := waitRecovered(t, s2, sched.ID)
		taskRecoverDelay = old
		r := got.Runs[0]
		if strings.Contains(r.Summary, secret) || strings.Contains(r.Detail, secret) {
			t.Fatalf("the cut tool output reached the record: %+v", r)
		}
		logData, err := os.ReadFile(filepath.Join(s2.taskRunDir(sched.ID), r.LogFile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(logData), secret) || strings.Contains(string(logData), "xxxx") {
			t.Fatalf("the cut tool output reached the run log: %q", string(logData))
		}
		if !strings.Contains(string(logData), taskStreamDroppedNote) {
			t.Errorf("the log does not say an event was left out: %q", string(logData))
		}
		waitUntil(t, "the failure push", func() bool { n, _ := relay.counts(); return n > 0 })
		relay.mu.Lock()
		for _, p := range relay.pushes {
			for _, v := range p {
				if strings.Contains(v, secret) {
					relay.mu.Unlock()
					t.Fatalf("the cut tool output reached a push: %v", p)
				}
			}
		}
		relay.mu.Unlock()
	}
}

// A recurring task whose run was in flight when the app stopped: that run is
// its slot's run. The slot is not also counted as missed, and no catch-up
// run is added beside it.
func TestAnInFlightRunIsNotAlsoCountedAsMissed(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 * * * *", Prompt: "p", Cwd: t.TempDir(), Permission: "plan", CatchUp: true, Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	slot := now.Truncate(time.Hour) // this hour's run, the one in flight
	if now.Sub(slot) < time.Minute {
		t.Skip("too close to the hour")
	}
	prev := slot.Add(-time.Hour).Add(time.Second)
	s.mu.Lock()
	it := s.items[sched.ID]
	it.Enabled = true
	it.CreatedAt = prev.Add(-time.Hour)
	it.LastFiredAt = &prev
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop()
	writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_hour", Started: slot.Add(time.Second), TimeoutMs: 60000, Permission: "plan"}, streamOK)

	got := waitRecovered(t, reopen(t, dir), sched.ID)
	for _, r := range got.Runs {
		if r.Cause == runCauseMissedAppOff {
			t.Fatalf("the in-flight run's slot was counted as missed: %+v", r)
		}
	}
	if len(got.Runs) != 1 || got.Runs[0].ID != "run_hour" {
		t.Fatalf("runs = %+v, want only the recovered run", got.Runs)
	}
}

// During an update the daemon before this one can still record a run (and
// remove its files) after this one swept it. Then nothing is recorded again:
// no false "interrupted" failure, no push.
func TestARunTheOldDaemonRecordedIsNotRecordedAgain(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.Stop()
	files := writeInflight(t, dir, sched.ID, taskInflight{RunID: "run_old_daemon", Started: time.Now().Add(-time.Minute), TimeoutMs: 60000, Permission: "plan"}, streamOK)
	old := taskRecoverDelay
	taskRecoverDelay = 300 * time.Millisecond
	t.Cleanup(func() { taskRecoverDelay = old })
	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	// The old daemon records the run and removes its files.
	files.remove()
	got := waitRecovered(t, s2, sched.ID)
	if len(got.Runs) != 0 {
		t.Fatalf("recorded again: %+v", got.Runs)
	}
}
