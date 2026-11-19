package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Automations stage 2 (ST-299 item 12; secure's condition 2): one switch holds
// every automation, and Pause all autonomy sets it too.

func TestPauseAllHoldsARunAndResumeRunsItOnce(t *testing.T) {
	s := newTestScheduleStore(t)
	on := true
	sched, err := s.Create(ScheduleInput{Name: "standup", PaneID: "pane_gone", Prompt: "x", Cron: "0 9 * * *", Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	if got.Enabled || got.PausedReason != allPausedReason || len(got.Runs) != 0 {
		t.Fatalf("while paused: enabled %v reason %q runs %d, want held and not run", got.Enabled, got.PausedReason, len(got.Runs))
	}
	if err := s.SetAllPaused(false, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the held run", func() bool { g, _ := s.Get(sched.ID); return len(g.Runs) == 1 })
	got, _ = s.Get(sched.ID)
	if !got.Enabled || got.PausedReason != "" {
		t.Errorf("after resume: enabled %v reason %q, want armed again", got.Enabled, got.PausedReason)
	}
}

func TestPauseAllKeepsAPromptWaitingButTypesNothing(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "waiting")
	s.mu.Lock()
	s.items[sched.ID].Enabled = true
	s.paneWaits = map[string]*paneWait{sched.ID: {since: time.Now(), until: time.Now().Add(time.Hour), why: "the pane is busy"}}
	s.mu.Unlock()
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	// Its wait (and the pane-identity check it carries) stays; a retry
	// while paused types nothing and records nothing.
	if !s.waitingForPane(sched.ID) {
		t.Fatal("the wait was dropped")
	}
	s.fireAs(sched.ID, fireRetry)
	if got, _ := s.Get(sched.ID); len(got.Runs) != 0 {
		t.Fatalf("a retry while paused did something: %+v", got.Runs)
	}
}

// A task that passed fireAs before the pause is still held at its run slot.
func TestATaskIsHeldAtItsRunSlotWhenPausedMeanwhile(t *testing.T) {
	fakeClaude(t, "exit 0\n")
	s := newTestScheduleStore(t)
	sched := taskSchedule(t, s, ScheduleInput{})
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	s.fireTaskAs(runTriggerScheduled, sched.ID, sched.Cwd, "p", "plan", "", 0)
	got, _ := s.Get(sched.ID)
	if got.PausedReason != allPausedReason || len(got.Runs) != 0 || s.taskRunningFor(sched.ID) {
		t.Fatalf("reason %q runs %d running %v, want held at the slot", got.PausedReason, len(got.Runs), s.taskRunningFor(sched.ID))
	}
}

// Holds a stop left behind after the switch was turned off are released at
// the next start, and their owed run goes.
func TestHoldsLeftByAStopAreReleasedAtStart(t *testing.T) {
	old := catchUpDelay
	catchUpDelay = 10 * time.Millisecond
	t.Cleanup(func() { catchUpDelay = old })
	dir := t.TempDir()
	s := reopen(t, dir)
	on := true
	sched, err := s.Create(ScheduleInput{Name: "held", PaneID: "pane_gone", Prompt: "x", Cron: "0 9 * * *", Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.holdForPauseLocked(s.items[sched.ID])
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop() // the switch is off; the row is still held
	s2 := reopen(t, dir)
	waitUntil(t, "the left-behind hold to be released and run", func() bool {
		g, _ := s2.Get(sched.ID)
		return g.PausedReason == "" && len(g.Runs) == 1
	})
}

func TestRunNowStillRunsWhilePaused(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "by hand")
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RunNow(sched.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(sched.ID); len(got.Runs) != 1 {
		t.Fatalf("runs = %+v, want the person's Run now", got.Runs)
	}
}

func TestPauseAllHoldsAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if p := reopen(t, dir).AllPaused(); !p.Paused || p.By != schedulePersonName || p.Since == nil {
		t.Fatalf("after restart = %+v, want still paused, by the person", p)
	}
}

// A missing "paused" changes nothing (it used to read as false: resume all).
func TestPauseAllNeedsAnExplicitValue(t *testing.T) {
	s := newTestScheduleStore(t)
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{scheds: s}
	for _, body := range []string{`{}`, `{"paused":null}`} {
		rec := httptest.NewRecorder()
		a.setPauseAll(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/pause-all", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, rec.Code)
		}
	}
	if !s.AllPaused().Paused {
		t.Fatal("a request with no value resumed all automations")
	}
}

// A one-time reminder that came due while the app was off, with all
// automations paused, is held for the resume, not recorded as missed.
func TestAOneTimeDueWhilePausedAndOffIsHeld(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	at := time.Now().Add(time.Hour)
	sched, err := s.Create(ScheduleInput{Name: "remind", PaneID: "pane_gone", Prompt: "x", RunAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	s.mu.Lock()
	s.items[sched.ID].RunAt = &past
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop()
	s2 := reopen(t, dir)
	got, _ := s2.Get(sched.ID)
	if got.PausedReason != allPausedReason || len(got.Runs) != 0 {
		t.Fatalf("after restart: reason %q runs %+v, want held, not missed", got.PausedReason, got.Runs)
	}
	if err := s2.SetAllPaused(false, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the held reminder to run", func() bool { g, _ := s2.Get(sched.ID); return len(g.Runs) == 1 })
}

// Switching off a held automation drops its hold: Resume does not turn it
// back on or run it.
func TestSwitchingOffAHeldAutomationDropsTheHold(t *testing.T) {
	s := newTestScheduleStore(t)
	on := true
	sched, err := s.Create(ScheduleInput{Name: "x", PaneID: "pane_gone", Prompt: "x", Cron: "0 9 * * *", Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllPaused(true, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	s.fire(sched.ID)
	if _, err := s.SetEnabledBy(sched.ID, false, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllPaused(false, scheduleActorPerson, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := s.Get(sched.ID); got.Enabled || len(got.Runs) != 0 {
		t.Fatalf("enabled %v runs %d, want it left off and not run", got.Enabled, len(got.Runs))
	}
}

// A pause switch that cannot be read fails closed: paused.
func TestAnUnreadablePauseSwitchStartsPaused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, automationsPausedFile), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !reopen(t, dir).AllPaused().Paused {
		t.Fatal("a torn pause switch started unpaused")
	}
}
