package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushRelay is a fake cloud that counts the pushes and reminder syncs it gets.
type pushRelay struct {
	mu        sync.Mutex
	pushes    []map[string]string
	reminders int
	srv       *httptest.Server
}

func newPushRelay(t *testing.T) *pushRelay {
	t.Helper()
	r := &pushRelay{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.URL.Path == "/api/internal/reminders" {
			r.reminders++
			_, _ = w.Write([]byte(`{"stored":1}`))
			return
		}
		var m map[string]string
		_ = json.NewDecoder(req.Body).Decode(&m)
		r.pushes = append(r.pushes, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sent":1}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *pushRelay) counts() (pushes, reminders int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pushes), r.reminders
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testSpawnWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPromptScheduleAutoPausesWhenTargetPaneGone(t *testing.T) {
	relay := newPushRelay(t)
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	s.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))
	sched, err := s.Create(ScheduleInput{Name: "Nightly nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	if s.NextFire(sched.ID).IsZero() {
		t.Fatal("control: the schedule should be armed before it fires")
	}

	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	if got.Enabled || got.PausedReason != targetPaneGoneReason {
		t.Fatalf("after a fire into a closed pane: enabled=%v reason=%q, want paused with a reason", got.Enabled, got.PausedReason)
	}
	if len(got.Runs) != 1 || got.Runs[0].Status != "error" || !strings.Contains(got.Runs[0].Detail, "paused") {
		t.Fatalf("runs = %+v, want one error run saying it paused", got.Runs)
	}
	if !s.NextFire(sched.ID).IsZero() {
		t.Fatal("a paused schedule must not stay armed")
	}
	waitUntil(t, "the one schedule-error push", func() bool { n, _ := relay.counts(); return n == 1 })
	relay.mu.Lock()
	if relay.pushes[0]["state"] != "schedule-error" || !strings.Contains(relay.pushes[0]["summary"], "paused") {
		t.Errorf("push = %+v, want a schedule-error saying it paused", relay.pushes[0])
	}
	relay.mu.Unlock()

	// Re-enabling (after the user picks a pane) clears the daemon's reason.
	if _, err := s.SetEnabled(sched.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(sched.ID); got.PausedReason != "" {
		t.Fatalf("PausedReason = %q after re-enable, want cleared", got.PausedReason)
	}
}

// fakeClaude installs a `claude` on PATH that runs body. It counts its runs
// in the returned file and, like the real CLI running the daemon's
// SessionStart hook, marks the attempt as started. A body can mark a tool
// call with: echo x >> "$SOLU_TASK_MARKERS/tool"
func fakeClaude(t *testing.T, body string) (counter string) {
	t.Helper()
	return fakeClaudeHooks(t, true, body)
}

func fakeClaudeHooks(t *testing.T, hooks bool, body string) (counter string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix shell test")
	}
	bin := t.TempDir()
	counter = filepath.Join(bin, "runs")
	start := ""
	if hooks {
		start = "[ -n \"$SOLU_TASK_MARKERS\" ] && echo x >> \"$SOLU_TASK_MARKERS/started\"\n"
	}
	script := "#!/bin/sh\ncat >/dev/null\n" + start + "echo x >> \"" + counter + "\"\nn=$(wc -l < \"" + counter + "\")\n" + body
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func runsOf(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

func waitTaskIdle(t *testing.T, s *ScheduleStore, id string, runs int) Schedule {
	t.Helper()
	var got Schedule
	waitUntil(t, "the task run to land", func() bool {
		got, _ = s.Get(id)
		return len(got.Runs) >= runs && !s.taskRunningFor(id)
	})
	return got
}

func shrinkRetryDelay(t *testing.T) {
	prev := taskRetryDelay
	taskRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { taskRetryDelay = prev })
}

// Feedback #54: a one-time task used to be lost for good on one 529.
func TestOneTimeTaskRetriesOnceAfterTemporaryAPIError(t *testing.T) {
	shrinkRetryDelay(t)
	counter := fakeClaude(t, "if [ \"$n\" -eq 1 ]; then echo 'API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}'; exit 1; fi\necho 'Nothing new needs you.'\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Kind: "task", RunAt: &at, Prompt: "digest", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "digest", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if n := runsOf(t, counter); n != 2 {
		t.Fatalf("claude ran %d times, want 2 (one retry)", n)
	}
	if len(got.Runs) != 1 || got.Runs[0].Status != "fired" || !strings.Contains(got.Runs[0].Detail, "retried once after a temporary error") {
		t.Fatalf("runs = %+v, want ONE fired run that says it was retried", got.Runs)
	}
}

func TestTaskRealFailureIsNotRetried(t *testing.T) {
	shrinkRetryDelay(t)
	counter := fakeClaude(t, "echo 'Error: the build is broken'; exit 1\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "build", Cwd: work, Permission: "plan", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "build", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if n := runsOf(t, counter); n != 1 || got.Runs[0].Status != "error" {
		t.Fatalf("claude ran %d times, run=%+v; want one failed run and no retry", n, got.Runs[0])
	}
}

func TestTaskRunsHeldWhileUsageLimitIsSpent(t *testing.T) {
	counter := fakeClaude(t, "echo \"You've hit your weekly limit · resets 11:59pm\"; exit 1\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "*/30 * * * *", Prompt: "sweep", Cwd: work, Permission: "plan", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}

	s.fireTask(sched.ID, work, "sweep", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if got.Runs[0].Status != "error" || !strings.Contains(got.Runs[0].Detail, "usage limit reached; task runs resume after") {
		t.Fatalf("first run = %+v, want an error naming the resume time", got.Runs[0])
	}
	until, held := s.heldByUsageLimit(time.Now())
	if !held || until.Hour() != 23 || until.Minute() != 59 {
		t.Fatalf("hold = %v %v, want held until 23:59", until, held)
	}

	// The next fire skips without spawning anything (and skips do not page).
	s.fireTask(sched.ID, work, "sweep", "plan", "", 0)
	got, _ = s.Get(sched.ID)
	if n := runsOf(t, counter); n != 1 {
		t.Fatalf("claude ran %d times while the limit was spent, want 1", n)
	}
	if got.Runs[0].Status != "skipped" || !strings.Contains(got.Runs[0].Detail, "Claude usage limit reached — runs resume after") {
		t.Fatalf("held run = %+v, want a skip naming the resume time", got.Runs[0])
	}

	// "Run now" is the user's probe past the hold.
	if _, err := s.FireNow(sched.ID); err != nil {
		t.Fatal(err)
	}
	waitTaskIdle(t, s, sched.ID, 3)
	if n := runsOf(t, counter); n != 2 {
		t.Fatalf("claude ran %d times after Run now, want 2", n)
	}

	// After the reset passes, runs go through again.
	s.mu.Lock()
	s.usageLimitUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, held := s.heldByUsageLimit(time.Now()); held {
		t.Fatal("hold should lift once the reset passes")
	}
}

func TestOneTimeTaskMovesPastTheUsageLimitReset(t *testing.T) {
	counter := fakeClaude(t, "echo 'Claude AI usage limit reached|4102444800'; exit 1\n") // 2100-01-01
	s := newTestScheduleStore(t)
	work := t.TempDir()
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Kind: "task", RunAt: &at, Prompt: "once", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "once", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	want := time.Unix(4102444800, 0).Add(time.Minute)
	if !got.Enabled || got.RunAt == nil || !got.RunAt.Equal(want) {
		t.Fatalf("one-time after hitting the limit: enabled=%v runAt=%v, want re-armed for %v", got.Enabled, got.RunAt, want)
	}

	// A one-time whose turn comes while the hold is on moves too, without a run.
	other := time.Now().Add(2 * time.Hour)
	second, err := s.Create(ScheduleInput{Kind: "task", RunAt: &other, Prompt: "twice", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(second.ID, work, "twice", "plan", "", 0)
	got2, _ := s.Get(second.ID)
	if len(got2.Runs) != 0 || got2.RunAt == nil || !got2.RunAt.Equal(want) || runsOf(t, counter) != 1 {
		t.Fatalf("held one-time: runs=%+v runAt=%v spawns=%d, want moved with no run", got2.Runs, got2.RunAt, runsOf(t, counter))
	}
}

func TestClassifyTaskFailure(t *testing.T) {
	for out, want := range map[string]string{
		"API Error: 529 {\"type\":\"overloaded_error\"}":                     taskFailTransient,
		"API Error: 500 Internal server error":                               taskFailTransient,
		"Connection error.":                                                  taskFailTransient,
		"You've hit your weekly limit · resets Oct 30, 9am (Asia/Singapore)": taskFailQuota,
		"5-hour limit reached ∙ resets 3pm":                                  taskFailQuota,
		"Error: tests failed":                                                "",
		"":                                                                   "",
		// Only the tail counts: a report that DISCUSSES an outage early on
		// and then fails for its own reason is a real failure.
		"The status page said the API was overloaded yesterday.\n1\n2\n3\n4\n5\nError: lint failed": "",
	} {
		if got := classifyTaskFailure([]byte(out)); got != want {
			t.Errorf("classifyTaskFailure(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestTaskLimitResetAt(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 0, 0, 0, time.Local)
	for out, want := range map[string]time.Time{
		"Claude AI usage limit reached|1793500000":            time.Unix(1793500000, 0),
		"You've hit your weekly limit · resets Oct 30, 9am":   time.Date(2026, 10, 30, 9, 0, 0, 0, time.Local),
		"You've hit your weekly limit · resets Jan 2, 1:30am": time.Date(2027, 1, 2, 1, 30, 0, 0, time.Local),
		"5-hour limit reached ∙ resets 10:40pm":               time.Date(2026, 10, 28, 22, 40, 0, 0, time.Local),
		"5-hour limit reached ∙ resets 9am":                   time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local),
		"usage limit reached":                                 now.Add(taskLimitFallbackHold),
	} {
		if got := taskLimitResetAt([]byte(out), now); !got.Equal(want) {
			t.Errorf("taskLimitResetAt(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestReminderSyncSkipsAnUnchangedSet(t *testing.T) {
	prev := reminderSyncDebounce
	reminderSyncDebounce = 5 * time.Millisecond
	t.Cleanup(func() { reminderSyncDebounce = prev })
	relay := newPushRelay(t)
	s := newTestScheduleStore(t)
	s.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))
	waitUntil(t, "the connect-time sync", func() bool { _, n := relay.counts(); return n == 1 })

	// Runs and other changes that leave the reminder set alone send nothing.
	for i := 0; i < 5; i++ {
		s.syncRemindersNow()
	}
	if _, n := relay.counts(); n != 1 {
		t.Fatalf("reminder syncs = %d after 5 unchanged syncs, want still 1", n)
	}

	// A new reminder changes the set → one more sync.
	at := time.Now().Add(time.Hour)
	if _, err := s.Create(ScheduleInput{RunAt: &at, PaneID: "p1", Prompt: "call", NotifyOnRun: true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the changed-set sync", func() bool { _, n := relay.counts(); return n == 2 })

	// An unchanged set is still re-sent after reminderResyncEvery.
	prevEvery := reminderResyncEvery
	reminderResyncEvery = 0
	t.Cleanup(func() { reminderResyncEvery = prevEvery })
	s.syncRemindersNow()
	if _, n := relay.counts(); n != 3 {
		t.Fatalf("reminder syncs = %d, want a periodic re-send", n)
	}
}

func TestScheduleRunEventsNameTheSchedule(t *testing.T) {
	bus := newUIBus()
	s, err := NewScheduleStore(t.TempDir(), nil, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	events := bus.subscribe()
	defer bus.unsubscribe(events)
	off := false
	sched, err := s.Create(ScheduleInput{Name: "x", PaneID: "p1", Prompt: "hi", Cron: "0 9 * * *", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	s.recordRun(sched.ID, ScheduleRun{At: time.Now(), Status: "fired"})

	var texts []string
	for len(texts) < 2 {
		select {
		case e := <-events:
			if e.Type == "schedules-changed" {
				texts = append(texts, e.Text)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("schedules-changed events = %q, want two", texts)
		}
	}
	// A create changes the list; a run changes one row.
	if texts[0] != "" || texts[1] != sched.ID {
		t.Fatalf("schedules-changed texts = %q, want [\"\", %q]", texts, sched.ID)
	}
}

func TestGetScheduleAnswersOneVisibleRow(t *testing.T) {
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}
	off := false
	sched, err := s.Create(ScheduleInput{Name: "visible", PaneID: "p1", Prompt: "hi", Cron: "0 9 * * *", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	hidden := &Schedule{ID: "sched_hidden", Name: "agent trigger", Kind: agentKindSchedule, AgentID: "agt_1"}
	s.mu.Lock()
	s.items[hidden.ID] = hidden
	s.order = append(s.order, hidden.ID)
	s.mu.Unlock()

	get := func(id string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/schedules/{id}", a.getSchedule)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/schedules/"+id, nil))
		return rec
	}
	rec := get(sched.ID)
	var dto scheduleDTO
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &dto) != nil || dto.ID != sched.ID {
		t.Fatalf("GET visible = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(hidden.ID); rec.Code != 404 {
		t.Fatalf("GET hidden agent trigger = %d, want 404", rec.Code)
	}
	if rec := get("sched_nope"); rec.Code != 404 {
		t.Fatalf("GET unknown = %d, want 404", rec.Code)
	}
}

const overloadedThenOK = "if [ \"$n\" -eq 1 ]; then echo 'API Error: 529 overloaded_error'; exit 1; fi\necho 'Nothing new needs you.'\n"

// The retry asks again whether a run may start: a pause, an edit or the
// owning agent's pause during the wait cancels it.
func TestTaskRetryRespectsChangesDuringTheWait(t *testing.T) {
	prev := taskRetryDelay
	taskRetryDelay = 400 * time.Millisecond
	t.Cleanup(func() { taskRetryDelay = prev })

	cases := map[string]func(s *ScheduleStore, id string, paused *bool){
		"paused by the user": func(s *ScheduleStore, id string, _ *bool) {
			if _, err := s.SetEnabled(id, false); err != nil {
				t.Fatal(err)
			}
		},
		"owning agent paused": func(_ *ScheduleStore, _ string, paused *bool) { *paused = true },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			counter := fakeClaude(t, overloadedThenOK)
			s := newTestScheduleStore(t)
			var mu sync.Mutex
			agentPaused := false
			s.SetOwnedRunGate(func(string) (bool, string) {
				mu.Lock()
				defer mu.Unlock()
				return agentPaused, "agent paused"
			})
			work := t.TempDir()
			sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "digest", Cwd: work, Permission: "plan"})
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.items[sched.ID].OwnedByAgentID = "agt_1"
			s.mu.Unlock()

			s.fireTask(sched.ID, work, "digest", "plan", "", 0)
			waitUntil(t, "the first attempt", func() bool { return runsOf(t, counter) == 1 })
			mu.Lock()
			change(s, sched.ID, &agentPaused)
			mu.Unlock()
			got := waitTaskIdle(t, s, sched.ID, 1)
			if n := runsOf(t, counter); n != 1 {
				t.Fatalf("claude ran %d times, want 1: the retry should have been cancelled", n)
			}
			if got.Runs[0].Status != "error" || strings.Contains(got.Runs[0].Detail, "retried") {
				t.Fatalf("run = %+v, want the first failure recorded as-is", got.Runs[0])
			}
		})
	}
}

// A one-time task the user paused while it ran stays paused even if the run
// then hits the usage limit.
func TestUsageLimitDoesNotUndoAPauseMadeDuringTheRun(t *testing.T) {
	fakeClaude(t, "sleep 0.6; echo \"You've hit your weekly limit · resets 11:59pm\"; exit 1\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Kind: "task", RunAt: &at, Prompt: "once", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "once", "plan", "", 0)
	if _, err := s.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	got := waitTaskIdle(t, s, sched.ID, 1)
	if got.Enabled || got.RunAt == nil || !got.RunAt.Equal(at) {
		t.Fatalf("enabled=%v runAt=%v, want still paused at the original time %v", got.Enabled, got.RunAt, at)
	}
}

// The re-arm decision and a user's pause are serialised: a pause that lands
// after the daemon decided to re-arm wins.
func TestDeferOneTimeNeverUndoesALaterPause(t *testing.T) {
	s := newTestScheduleStore(t)
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Kind: "task", RunAt: &at, Prompt: "once", Cwd: t.TempDir(), Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	on, seq, _ := s.editStamp(sched.ID)
	if !on {
		t.Fatal("control: a new one-time schedule is on")
	}
	if _, err := s.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	if s.deferOneTime(sched.ID, at.Add(time.Hour), seq) {
		t.Fatal("deferOneTime re-armed a schedule paused after its stamp")
	}
	if got, _ := s.Get(sched.ID); got.Enabled || !got.RunAt.Equal(at) {
		t.Fatalf("enabled=%v runAt=%v, want the pause and the original time kept", got.Enabled, got.RunAt)
	}
	// With a current stamp it does move.
	_, seq, _ = s.editStamp(sched.ID)
	if !s.deferOneTime(sched.ID, at.Add(time.Hour), seq) {
		t.Fatal("deferOneTime with a current stamp should re-arm")
	}
}

// A retry re-runs the whole prompt, so an attempt that already started a tool
// (and may have posted, pushed or sent something) is never retried.
func TestTaskNotRetriedOnceItStartedATool(t *testing.T) {
	shrinkRetryDelay(t)
	counter := fakeClaude(t, "echo x >> \"$SOLU_TASK_MARKERS/tool\"\nif [ \"$n\" -eq 1 ]; then echo 'API Error: 529 overloaded_error'; exit 1; fi\necho ok\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "post the digest", Cwd: work, Permission: "edit", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "post the digest", "edit", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if n := runsOf(t, counter); n != 1 {
		t.Fatalf("claude ran %d times, want 1: a run that reached a tool must not be retried", n)
	}
	if got.Runs[0].Status != "error" || !strings.Contains(got.Runs[0].Detail, "not retried: it had already started using tools") {
		t.Fatalf("run = %+v, want the error recorded with why it was not retried", got.Runs[0])
	}
	if _, err := os.Stat(filepath.Join(s.taskRunDir(sched.ID), ".attempt")); !os.IsNotExist(err) {
		t.Errorf("marker folder left behind: %v", err)
	}
}

// Without proof the hooks ran (disabled hooks, managed policy, a wrapper),
// "no tool marker" proves nothing, so the run is not retried either.
func TestTaskNotRetriedWhenToolUseCannotBeRuledOut(t *testing.T) {
	shrinkRetryDelay(t)
	counter := fakeClaudeHooks(t, false, "if [ \"$n\" -eq 1 ]; then echo 'API Error: 529 overloaded_error'; exit 1; fi\necho ok\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "digest", Cwd: work, Permission: "plan", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "digest", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if n := runsOf(t, counter); n != 1 || !strings.Contains(got.Runs[0].Detail, "not retried: could not confirm") {
		t.Fatalf("claude ran %d times, run=%+v; want one run, not retried", n, got.Runs[0])
	}
}

func TestTaskMarkerHooksSettings(t *testing.T) {
	s := newTestScheduleStore(t)
	dir, settings := s.prepareTaskMarkers("sched_x")
	if dir == "" {
		t.Fatal("no marker folder")
	}
	var parsed struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		t.Fatalf("settings is not JSON: %v (%s)", err, settings)
	}
	for event, marker := range map[string]string{"SessionStart": taskMarkerStarted, "PreToolUse": taskMarkerTool} {
		groups := parsed.Hooks[event]
		if len(groups) != 1 || groups[0].Matcher != "" || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s hooks = %+v, want one match-all command", event, groups)
		}
		want := `echo x >> "` + filepath.ToSlash(filepath.Join(dir, marker)) + `"`
		if h := groups[0].Hooks[0]; h.Type != "command" || h.Command != want {
			t.Errorf("%s hook = %+v, want command %q", event, h, want)
		}
	}
	if tr := readTaskMarkers(dir); tr.hooksRan || tr.toolStarted || tr.safeToRetry() {
		t.Errorf("empty markers = %+v: nothing proven, so not safe to retry", tr)
	}
}
