package main

import (
	"testing"
	"time"
)

func crashExit(code int) *int { return &code }

func TestShouldAutoResumeHarnessOnExit(t *testing.T) {
	t.Parallel()
	managedCmd := []string{"node", "/l/launch.mjs", "deepseek", "--data-dir", "/d"}
	base := PaneSnapshot{
		ID: "pane_ds", Status: PaneExited, Resumable: true,
		HarnessProvider: "deepseek", HarnessSessionID: "sess-1",
		Cmd: managedCmd, ExitCode: crashExit(137),
	}
	cases := []struct {
		name       string
		mutate     func(s PaneSnapshot) PaneSnapshot
		autoResume bool
		want       bool
	}{
		{"auto off", func(s PaneSnapshot) PaneSnapshot { return s }, false, false},
		{"not exited", func(s PaneSnapshot) PaneSnapshot { s.Status = PaneRunning; return s }, true, false},
		{"not resumable", func(s PaneSnapshot) PaneSnapshot { s.Resumable = false; return s }, true, false},
		{"plain shell", func(s PaneSnapshot) PaneSnapshot {
			s.HarnessProvider, s.HarnessSessionID, s.Cmd = "", "", []string{"/bin/bash"}
			return s
		}, true, false},
		{"no durable session", func(s PaneSnapshot) PaneSnapshot { s.HarnessSessionID = ""; return s }, true, false},
		{"clean exit is a deliberate close", func(s PaneSnapshot) PaneSnapshot { s.ExitCode = crashExit(0); return s }, true, false},
		{"nil exit code (signal) resumes", func(s PaneSnapshot) PaneSnapshot { s.ExitCode = nil; return s }, true, true},
		{"crash resumes", func(s PaneSnapshot) PaneSnapshot { return s }, true, true},
		{"managed argv without provider field", func(s PaneSnapshot) PaneSnapshot { s.HarnessProvider = ""; return s }, true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldAutoResumeHarnessOnExit(tc.mutate(base), tc.autoResume)
			if got != tc.want {
				t.Fatalf("shouldAutoResumeHarnessOnExit(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestHarnessAutoResumeBudgetRollingWindow(t *testing.T) {
	t.Parallel()
	b := newHarnessAutoResumeBudget(10*time.Minute, 3)
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for i, at := range []time.Duration{0, time.Minute, 2 * time.Minute} {
		now := start.Add(at)
		if !b.allow("pane_ds", now) {
			t.Fatalf("attempt %d: allow = false, want true", i)
		}
		b.record("pane_ds", now)
	}
	if b.allow("pane_ds", start.Add(3*time.Minute)) {
		t.Fatal("fourth attempt inside the window was allowed; a crash loop would be unbounded")
	}
	// Another pane is unaffected.
	if !b.allow("pane_other", start.Add(3*time.Minute)) {
		t.Fatal("budget leaked across pane ids")
	}
	// After the window rolls past the first attempts, room opens again. At
	// +11m the cutoff is +1m, so the 0m and exactly-1m attempts are pruned.
	if !b.allow("pane_ds", start.Add(11*time.Minute)) {
		t.Fatal("budget did not roll off with the window")
	}
}

func TestHarnessAutoResumeBudgetZeroValueSafe(t *testing.T) {
	t.Parallel()
	var b *harnessAutoResumeBudget
	if b.allow("pane", time.Now()) {
		t.Fatal("nil budget allowed an attempt")
	}
	b.record("pane", time.Now()) // must not panic
}
