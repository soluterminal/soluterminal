package main

import (
	"strings"
	"testing"
)

// A prompt schedule fired without a pane manager records an error run instead
// of panicking or silently doing nothing (schedules_runner.go).
func TestScheduleFiredWithoutManagerRecordsErrorRun(t *testing.T) {
	store, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	enabled := false
	sched, err := store.Create(ScheduleInput{
		Name: "Prompt without manager", Kind: "prompt", Cron: "* * * * *",
		PaneID: "missing-pane", Prompt: "inspect the failing test", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	store.fire(sched.ID)

	listed := store.List()
	if len(listed) != 1 || len(listed[0].Runs) != 1 {
		t.Fatalf("schedule runs=%+v, want exactly one", listed)
	}
	run := listed[0].Runs[0]
	if run.Status != "error" || !strings.Contains(run.Detail, "pane manager unavailable") {
		t.Fatalf("run=%+v, want pane manager unavailable error", run)
	}
	if run.PaneID != "missing-pane" {
		t.Fatalf("run pane=%q, want the schedule's target pane", run.PaneID)
	}
}
