package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ST-299 item 13: an unattended update waits for an automation's run.
func TestUpdateWaitsForAnAutomationRun(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "nightly")
	a := &apiServer{scheds: s}
	if busy := a.updateBusy(); busy != nil {
		t.Fatalf("idle: busy = %+v", busy)
	}
	s.mu.Lock()
	s.taskRunning[sched.ID] = true
	s.mu.Unlock()
	busy := a.updateBusy()
	if busy == nil || busy.cause != updateBusyCauseAutomation {
		t.Fatalf("a run in flight: busy = %+v, want the automation cause", busy)
	}
	s.mu.Lock()
	delete(s.taskRunning, sched.ID)
	s.promptRunning = map[string]bool{sched.ID: true}
	s.mu.Unlock()
	if busy := a.updateBusy(); busy == nil || busy.cause != updateBusyCauseAutomation {
		t.Fatalf("a prompt being typed: busy = %+v", busy)
	}
}

// The wait is bounded: past updateAutomationGrace the update goes ahead (the
// run is followed after the swap).
func TestAnAutomationRunHoldsAnUpdateOnlyForItsGrace(t *testing.T) {
	u := &Updater{busyCheck: func() *updateBusyError {
		return &updateBusyError{cause: updateBusyCauseAutomation, reason: "running"}
	}}
	start := time.Now()
	step := updateBusyObservationGap / 2 // observed steadily, as the poll does
	for at := start; !at.After(start.Add(updateAutomationGrace + step)); at = at.Add(step) {
		busy := u.checkBusyAt(at)
		elapsed := at.Sub(start)
		switch {
		case elapsed < updateAutomationGrace && busy == nil:
			t.Fatalf("released after %s, before the automation grace (%s)", elapsed, updateAutomationGrace)
		case elapsed >= updateAutomationGrace && busy != nil:
			t.Fatalf("still held after %s, past the automation grace", elapsed)
		}
	}
}

// A run whose automation was deleted while it ran still holds the update.
func TestARunOfADeletedAutomationStillHoldsTheUpdate(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "gone")
	s.mu.Lock()
	s.taskRunning[sched.ID] = true
	s.mu.Unlock()
	if err := s.Delete(sched.ID); err != nil {
		t.Fatal(err)
	}
	if name, ok := s.RunInFlight(); !ok || name != "a deleted automation" {
		t.Fatalf("RunInFlight = %q %v, want the deleted automation's run", name, ok)
	}
}

// A different cause starts its own grace: a collaboration hold after a long
// automation hold still gets its own ten minutes.
func TestADifferentBusyCauseStartsItsOwnGrace(t *testing.T) {
	cause := updateBusyCauseAutomation
	u := &Updater{busyCheck: func() *updateBusyError { return &updateBusyError{cause: cause, reason: "busy"} }}
	start := time.Now()
	step := updateBusyObservationGap / 2
	at := start
	for ; at.Before(start.Add(updateAutomationGrace + step)); at = at.Add(step) {
		u.checkBusyAt(at)
	}
	cause = updateBusyCauseCollaboration
	if busy := u.checkBusyAt(at); busy == nil {
		t.Fatal("a fresh collaboration hold inherited the spent automation grace")
	}
}

// While an automation runs, the status says the installer restart is paused
// (the native desktop updater reads it), and the daemon-routed silent
// installer answers "paused" (409), not a failure.
func TestTheDesktopInstallerWaitsForAnAutomationRun(t *testing.T) {
	allowed := false
	u := &Updater{
		automationRunning: func() (string, bool) { return "nightly", true },
		// The channel's control lets the installer restart the app: only the
		// automation stops it here.
		ctrlEffective: &UpdateControl{ShellAutoApplyPaused: &allowed},
	}
	st := u.Status()
	if !strings.Contains(st.ShellWaitingForAutomation, "nightly") || st.ShellAutoApplyPaused {
		t.Fatalf("status = waiting %q paused %v, want waiting for the automation, not paused", st.ShellWaitingForAutomation, st.ShellAutoApplyPaused)
	}
	_, err := u.applyShell(context.Background(), t.TempDir(), false, true, 0)
	var paused *silentShellPausedError
	if !errors.As(err, &paused) {
		t.Fatalf("silent installer = %v, want silentShellPausedError", err)
	}
}

// The installer's wait is bounded as the daemon's is.
func TestTheInstallersWaitIsBounded(t *testing.T) {
	u := &Updater{automationRunning: func() (string, bool) { return "nightly", true }}
	if _, busy := u.automationRunningNow(); !busy {
		t.Fatal("not waiting at first")
	}
	u.mu.Lock()
	u.automationWaitSince = time.Now().Add(-updateAutomationGrace - time.Second)
	u.mu.Unlock()
	if _, busy := u.automationRunningNow(); busy {
		t.Fatal("still waiting past the grace")
	}
}
