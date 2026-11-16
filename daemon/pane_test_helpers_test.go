package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// switchTestPane builds a live (running) in-memory pane for the switch-mode
// tests. Mirrors transactionTestPane but lets the caller set Kind/Cmd/session so
// we can drive the kill+respawn transitions without real processes.
func switchTestPane(id, kind string, cmd []string, sessionID string) *Pane {
	done := make(chan struct{})
	return &Pane{
		ID:              id,
		Cwd:             "/tmp",
		Cmd:             cmd,
		Title:           "agent",
		Status:          PaneRunning,
		CreatedAt:       time.Now().UTC(),
		Cols:            80,
		Rows:            24,
		Kind:            kind,
		ClaudeSessionID: sessionID,
		done:            done,
		buffer:          NewRingBuffer(1024),
		subs:            map[int64]*subEntry{},
	}
}

// deferredTestPane builds a live terminal/native pane with a ring buffer for the
// deferred-switch unit tests. Like switchTestPane but with a real buffer so the
// terminal settle predicate (computeStateCached) has something to scan.
func deferredTestPane(id, kind string) *Pane {
	return &Pane{
		ID:        id,
		Cwd:       "/tmp",
		Cmd:       []string{"bash"},
		Title:     "agent",
		Status:    PaneRunning,
		CreatedAt: time.Now().UTC(),
		Cols:      80,
		Rows:      24,
		Kind:      kind,
		done:      make(chan struct{}),
		buffer:    NewRingBuffer(1 << 20),
		subs:      map[int64]*subEntry{},
	}
}

// writeTempTranscript drops an on-disk transcript file so claudeTranscriptOnDisk
// passes for a keep-conversation respawn, and returns its absolute path.
func writeTempTranscript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConfirmSwitchHealthyEarlyExit locks in the latency win: a respawn that
// has painted its first PTY output confirms at ~switchModeHealthFloor instead
// of always burning the full switchModeHealthGrace — but never BEFORE the
// floor, and retries (fullGrace) still wait out the whole window.
func TestConfirmSwitchHealthyEarlyExit(t *testing.T) {
	api := &apiServer{}

	t.Run("first output exits at floor", func(t *testing.T) {
		p := switchTestPane("pane_h1", "", []string{"claude"}, "s")
		p.buffer.Write([]byte("first paint"))
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, false); err != nil {
			t.Fatalf("healthy pane errored: %v", err)
		}
		elapsed := time.Since(start)
		if elapsed < switchModeHealthFloor {
			t.Fatalf("returned before the floor (%v < %v) — quick-exit detection window violated", elapsed, switchModeHealthFloor)
		}
		if elapsed >= switchModeHealthGrace {
			t.Fatalf("no early exit: took %v, want < full grace %v", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("no output waits out the full grace", func(t *testing.T) {
		p := switchTestPane("pane_h2", "", []string{"claude"}, "s")
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, false); err != nil {
			t.Fatalf("running pane errored: %v", err)
		}
		if elapsed := time.Since(start); elapsed < switchModeHealthGrace {
			t.Fatalf("returned at %v without a health signal, want the full %v", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("fullGrace disables the early exit on retries", func(t *testing.T) {
		p := switchTestPane("pane_h3", "", []string{"claude"}, "s")
		p.buffer.Write([]byte("first paint"))
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, true); err != nil {
			t.Fatalf("healthy pane errored: %v", err)
		}
		if elapsed := time.Since(start); elapsed < switchModeHealthGrace {
			t.Fatalf("retry attempt returned at %v, want the full grace %v (races can die late)", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("quick exit still fails despite output", func(t *testing.T) {
		p := switchTestPane("pane_h4", "", []string{"claude"}, "s")
		p.buffer.Write([]byte("error: no conversation"))
		p.Status = PaneExited
		if err := api.confirmSwitchHealthy(p, false); err == nil {
			t.Fatal("exited pane confirmed healthy; want errNativeSessionDied")
		}
	})

	t.Run("nil pane is a failure", func(t *testing.T) {
		if err := api.confirmSwitchHealthy(nil, false); err != errPaneRespawnNil {
			t.Fatalf("nil pane err = %v, want errPaneRespawnNil", err)
		}
	})
}

// markPaneWorking makes a terminal test pane read as mid-turn: fresh output
// with no prompt box on screen (computeState → busy, IsAgentWorking → true).
func markPaneWorking(p *Pane) {
	p.buffer.Write([]byte("✻ Working… (esc to interrupt)\n"))
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
}
