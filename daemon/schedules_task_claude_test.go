package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// useRealScheduleTaskCheck swaps TestMain's always-ok seam back to the real
// Claude lookup, with no claude anywhere it looks (PATH, HOME, login shell).
func useRealScheduleTaskCheck(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("login-shell resolution is Unix-only")
	}
	prev := scheduleTaskRunnable
	t.Cleanup(func() { scheduleTaskRunnable = prev })
	scheduleTaskRunnable = scheduleTaskClaudeResolves
	prepareWorkerBinLoginShellFallback(t)
	loginShellCommandOutput = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("claude: not found")
	}
}

// A Codex-only machine gets a clear refusal when a Claude task is saved, over
// both the HTTP route (the dialog) and the MCP tool, instead of a schedule
// that errors and pushes on every fire. Prompt schedules still save.
func TestScheduleTaskRefusedWithoutClaude(t *testing.T) {
	useRealScheduleTaskCheck(t)
	work := t.TempDir()
	s, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{scheds: s, mgr: testManagerWithPane(transactionTestPane("pane-1"))}

	body := `{"kind":"task","prompt":"nightly","cron":"0 9 * * *","permission":"plan","cwd":` + strconv.Quote(work) + `}`
	rec := httptest.NewRecorder()
	a.createSchedule(rec, httptest.NewRequest(http.MethodPost, "/api/schedules", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Claude Code isn't installed on this computer") {
		t.Fatalf("HTTP create = %d %q, want 400 naming the missing Claude Code", rec.Code, rec.Body.String())
	}

	res := a.mcpToolCreateSchedule(map[string]any{
		"kind": "task", "prompt": "nightly", "cron": "0 9 * * *", "cwd": work,
	}, "pane-1")
	text, isErr := mcpResultText(t, res)
	if !isErr || !strings.Contains(text, `kind "prompt"`) {
		t.Fatalf("MCP create = %q (err=%v), want the refusal pointing at kind \"prompt\"", text, isErr)
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("schedules = %d, want none saved", n)
	}

	prompt, err := s.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: "pane-1", Prompt: "stand up"})
	if err != nil {
		t.Fatalf("prompt schedule refused without Claude: %v", err)
	}
	// Turning a prompt schedule into a task is refused the same way...
	if _, err := s.Update(prompt.ID, ScheduleInput{
		Kind: "task", Prompt: "nightly", Cron: "0 9 * * *", Cwd: work, Permission: "plan",
	}); !errors.Is(err, errScheduleTaskNeedsClaude) {
		t.Fatalf("prompt→task update err = %v, want errScheduleTaskNeedsClaude", err)
	}
}

// ...but a task saved while Claude was installed stays editable after it goes
// away (it may come back; the run reports the missing binary meanwhile).
func TestScheduleExistingTaskEditableWithoutClaude(t *testing.T) {
	work := t.TempDir()
	s, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Create(ScheduleInput{Kind: "task", Prompt: "nightly", Cron: "0 9 * * *", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	useRealScheduleTaskCheck(t)
	if _, err := s.Update(task.ID, ScheduleInput{
		Kind: "task", Prompt: "nightly v2", Cron: "0 10 * * *", Cwd: work, Permission: "plan",
	}); err != nil {
		t.Fatalf("editing an existing task without Claude: %v", err)
	}
}
