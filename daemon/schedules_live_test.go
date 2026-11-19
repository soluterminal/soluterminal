package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Automations stage 2 (ST-299 item 12): a run in flight can be watched, and
// the person can stop it.

func TestLiveShowsTheAssistantsTextNotToolOutputAndStopEndsTheRun(t *testing.T) {
	old := taskStreamPoll
	taskStreamPoll = 20 * time.Millisecond
	t.Cleanup(func() { taskStreamPoll = old })
	counter := fakeClaude(t, `printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at the build now."}]}}' '{"type":"user","message":{"content":[{"type":"tool_result","content":"TOOL-OUTPUT-SECRET"}]}}'
sleep 30
`)
	s := newTestScheduleStore(t)
	sched := taskSchedule(t, s, ScheduleInput{})
	s.fireTask(sched.ID, sched.Cwd, "digest", "plan", "", 0)
	var v liveView
	waitUntil(t, "the live text", func() bool {
		v = s.Live(sched.ID)
		return v.Running && strings.Contains(v.Text, "Looking at the build now.")
	})
	if strings.Contains(v.Text, "TOOL-OUTPUT-SECRET") {
		t.Fatalf("live text holds a tool's output: %q", v.Text)
	}
	if v.RunID == "" || v.StartedAt == nil {
		t.Errorf("live view = %+v, want its run id and start", v)
	}
	begin := time.Now()
	if _, err := s.StopRun(sched.ID, "run_not_this_one", ""); !errors.Is(err, errStaleStop) {
		t.Fatalf("Stop of another run = %v, want errStaleStop", err)
	}
	if out, err := s.StopRun(sched.ID, v.RunID, ""); err != nil || out != "stopped" {
		t.Fatalf("Stop = %q %v", out, err)
	}
	got := waitTaskIdle(t, s, sched.ID, 1)
	if time.Since(begin) > 10*time.Second {
		t.Fatal("Stop did not end the run")
	}
	r := got.Runs[0]
	if r.Cause != runCauseStopped || r.Status != "error" || !strings.HasPrefix(r.Detail, "stopped by you") {
		t.Fatalf("run = %+v, want stopped by you", r)
	}
	if n := runsOf(t, counter); n != 1 {
		t.Errorf("claude ran %d times, want 1: a stopped run is never retried", n)
	}
	if after := s.Live(sched.ID); after.Running {
		t.Error("still live after it ended")
	}
}

func TestStopEndsAPromptsWaitForItsPane(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "waiting")
	s.mu.Lock()
	s.paneWaits = map[string]*paneWait{sched.ID: {since: time.Now(), until: time.Now().Add(time.Hour), why: "the pane is busy"}}
	s.mu.Unlock()
	v := s.Live(sched.ID)
	if v.Running || v.Waiting != "the pane is busy" || v.WaitID == "" {
		t.Fatalf("live = %+v, want the wait and its id", v)
	}
	if _, err := s.StopRun(sched.ID, "", "123"); !errors.Is(err, errStaleStop) {
		t.Fatalf("Stop of another wait = %v, want errStaleStop", err)
	}
	// A prompt being typed this moment cannot be taken back: Stop says so.
	s.mu.Lock()
	s.promptRunning = map[string]bool{sched.ID: true}
	s.mu.Unlock()
	if _, err := s.StopRun(sched.ID, "", v.WaitID); !errors.Is(err, errBeingSent) {
		t.Fatalf("Stop while it is typed = %v, want errBeingSent", err)
	}
	s.mu.Lock()
	s.promptRunning = nil
	s.mu.Unlock()
	if _, err := s.StopRun(sched.ID, "", v.WaitID); err != nil {
		t.Fatal(err)
	}
	if s.waitingForPane(sched.ID) {
		t.Error("still waiting after Stop")
	}
	got, _ := s.Get(sched.ID)
	if r := got.Runs[0]; r.Cause != runCauseStopped || r.Status != "skipped" {
		t.Fatalf("run = %+v, want a stopped skip", r)
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "idle")
	if _, err := s.StopRun(sched.ID, "", ""); !errors.Is(err, errNothingToStop) {
		t.Fatalf("Stop = %v, want errNothingToStop", err)
	}
}

// A failed stop leaves nothing marked: a later Stop tries again, and a run
// that ends by itself is not recorded as stopped by the person.
func TestAFailedStopIsNotAStop(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "stuck")
	calls := 0
	r := &liveRun{runID: "r1", started: time.Now(), stop: func() error {
		calls++
		if calls == 1 {
			return errors.New("no permission")
		}
		return nil
	}}
	s.setLive(sched.ID, r)
	if _, err := s.StopRun(sched.ID, "r1", ""); err == nil {
		t.Fatal("a failed kill reported success")
	}
	if s.endLive(sched.ID, r) {
		t.Fatal("a failed stop marked the run stopped")
	}
	s.setLive(sched.ID, r)
	if out, err := s.StopRun(sched.ID, "r1", ""); err != nil || out != "stopped" || calls != 2 {
		t.Fatalf("second Stop = %q %v after %d calls, want stopped on a second try", out, err, calls)
	}
}

// The live text keeps moving past the report buffer's limit.
func TestLiveTextKeepsMovingPastTheReportBuffer(t *testing.T) {
	st := newTaskStream("unused")
	chunk := strings.Repeat("a", 32<<10)
	for i := 0; i < 12; i++ { // 384 KiB of text: past the 256 KiB report buffer
		st.mu.Lock()
		st.feedLocked([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + chunk + `"}]}}`))
		st.mu.Unlock()
	}
	st.mu.Lock()
	st.feedLocked([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"THE NEWEST WORDS"}]}}`))
	st.mu.Unlock()
	text, clipped, _ := st.liveText(liveTextMax)
	if !strings.HasSuffix(text, "THE NEWEST WORDS") || !clipped || len(text) > liveTextMax {
		t.Fatalf("live tail (len %d, clipped %v) does not end with the newest words", len(text), clipped)
	}
}

// A one-time reminder stopped while it waited is done: its attempt was that.
func TestAStoppedOneTimeReminderIsDone(t *testing.T) {
	s := newTestScheduleStore(t)
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Name: "remind", PaneID: "p1", Prompt: "x", RunAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	s.mu.Lock()
	s.paneWaits = map[string]*paneWait{sched.ID: {since: since, until: since.Add(time.Hour), why: "the pane is busy"}}
	s.mu.Unlock()
	if _, err := s.StopRun(sched.ID, "", s.Live(sched.ID).WaitID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(sched.ID); got.Enabled {
		t.Error("a stopped one-time reminder is still on")
	}
}
