package main

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartSpawnOptsResumeVsFresh(t *testing.T) {
	t.Parallel()
	terminal := PaneSnapshot{
		ID: "pane-t", Cwd: "/repo", Cmd: []string{"claude"},
		ClaudeSessionID: "sess-1", ClaudeTranscriptPath: "/t.jsonl",
	}
	resume := restartSpawnOpts(terminal, false)
	if !slices.Equal(resume.Cmd, []string{"claude", "--resume", "sess-1"}) {
		t.Fatalf("resume restart cmd = %v", resume.Cmd)
	}
	fresh := restartSpawnOpts(terminal, true)
	if fresh.ClaudeSessionID != "" || fresh.ClaudeTranscriptPath != "" {
		t.Fatalf("fresh restart kept a conversation pointer: %+v", fresh)
	}
	if slices.Contains(fresh.Cmd, "--resume") || !slices.Equal(fresh.Cmd, []string{"claude"}) {
		t.Fatalf("fresh restart cmd = %v, want the pane's own command", fresh.Cmd)
	}

	// A managed harness resume carries the sidecar session on the env and the
	// fresh variant drops it.
	harness := PaneSnapshot{
		ID: "pane-h", Cwd: "/repo", Cmd: []string{"node", "/l/launch.mjs", "deepseek"},
		HarnessProvider: "deepseek", HarnessSessionID: "sess-h", ClaudeSessionID: "stale",
	}
	hr := restartSpawnOpts(harness, false)
	if !slices.Contains(hr.Env, "SOLU_HARNESS_RESUME_SESSION=sess-h") {
		t.Fatalf("harness resume env = %v", hr.Env)
	}
	hf := restartSpawnOpts(harness, true)
	if slices.Contains(hf.Env, "SOLU_HARNESS_RESUME_SESSION=sess-h") || hf.HarnessProvider != "" || hf.HarnessSessionID != "" {
		t.Fatalf("fresh harness restart kept resume state: %+v", hf)
	}
	if !slices.Equal(hf.Cmd, harness.Cmd) {
		t.Fatalf("fresh harness restart cmd = %v, want the launch command", hf.Cmd)
	}
}

func TestFleetPaneBusyClearsForQuietTerminalWithNoAgentProcess(t *testing.T) {
	t.Parallel()
	// DeepSeek/Pi harnesses and plain shells have no Claude/Codex prompt to
	// detect. Once output has advanced and the pane has been quiet for a full
	// settle window, the admitted turn must be treated as finished or the
	// restart verdict wedges "busy" forever (the user's old-build badge click).
	p := deferredTestPane("pane-quiet", "")
	p.Status = PaneRunning
	atomic.StoreInt64(&p.TotalStrippedChars, 100)
	p.fleetTurnOutputWatermark.Store(10)
	p.fleetTurnAdmitted.Store(true)
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-terminalRestartQuietWindow-time.Second).UnixMilli())
	if fleetPaneBusyForRestart(p) {
		t.Fatal("quiet harness/shell pane stayed busy")
	}
	if p.fleetTurnAdmitted.Load() {
		t.Fatal("admission latch was not cleared")
	}
}

func TestFleetPaneBusyStaysTrueWhileTerminalIsActive(t *testing.T) {
	t.Parallel()
	p := deferredTestPane("pane-active", "")
	p.Status = PaneRunning
	atomic.StoreInt64(&p.TotalStrippedChars, 100)
	p.fleetTurnOutputWatermark.Store(10)
	p.fleetTurnAdmitted.Store(true)
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
	if !fleetPaneBusyForRestart(p) {
		t.Fatal("actively producing terminal pane was not busy")
	}
}

func TestFleetPaneBusyStaysTrueBeforeFirstOutput(t *testing.T) {
	t.Parallel()
	p := deferredTestPane("pane-no-output", "")
	p.Status = PaneRunning
	atomic.StoreInt64(&p.TotalStrippedChars, 10)
	p.fleetTurnOutputWatermark.Store(10) // admitted, nothing emitted yet
	p.fleetTurnAdmitted.Store(true)
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Hour).UnixMilli())
	if !fleetPaneBusyForRestart(p) {
		t.Fatal("pane with no output since admission was not busy")
	}
}

func TestRestartPaneForceBypassesBusyGate(t *testing.T) {
	t.Parallel()
	p := deferredTestPane("pane-busy", "")
	p.Status = PaneRunning
	atomic.StoreInt64(&p.TotalStrippedChars, 10)
	p.fleetTurnOutputWatermark.Store(10) // admitted, nothing emitted yet
	p.fleetTurnAdmitted.Store(true)
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Hour).UnixMilli())
	if !restartPaneBusyBlocked(p, false) {
		t.Fatal("busy pane was not gated without the user override")
	}
	if restartPaneBusyBlocked(p, true) {
		t.Fatal("confirmed force did not bypass the busy gate")
	}
}
