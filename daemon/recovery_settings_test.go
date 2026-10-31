package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverySettingsDefaultsToManual(t *testing.T) {
	t.Setenv("SOLU_AUTO_RESUME", "")
	st, err := newRecoverySettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newRecoverySettingsStore: %v", err)
	}
	if got := st.Snapshot().ResumeAfterRestart; got != recoveryResumeManual {
		t.Fatalf("default resume mode = %q, want %q", got, recoveryResumeManual)
	}
	if st.AutoResumeAfterRestart() {
		t.Fatal("AutoResumeAfterRestart true for manual default")
	}
}

func TestRecoverySettingsEnvSeedsAutoWhenNoFile(t *testing.T) {
	t.Setenv("SOLU_AUTO_RESUME", "1")
	st, err := newRecoverySettingsStore(t.TempDir())
	if err != nil {
		t.Fatalf("newRecoverySettingsStore: %v", err)
	}
	if got := st.Snapshot().ResumeAfterRestart; got != recoveryResumeAuto {
		t.Fatalf("default resume mode = %q, want %q", got, recoveryResumeAuto)
	}
	if !st.AutoResumeAfterRestart() {
		t.Fatal("AutoResumeAfterRestart false for auto default")
	}
}

func TestRecoverySettingsFileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOLU_AUTO_RESUME", "1")
	st, err := newRecoverySettingsStore(dir)
	if err != nil {
		t.Fatalf("newRecoverySettingsStore: %v", err)
	}
	if _, err := st.Set(recoverySettings{ResumeAfterRestart: recoveryResumePaused}); err != nil {
		t.Fatalf("Set paused: %v", err)
	}

	t.Setenv("SOLU_AUTO_RESUME", "")
	reloaded, err := newRecoverySettingsStore(dir)
	if err != nil {
		t.Fatalf("reload recovery settings: %v", err)
	}
	if got := reloaded.Snapshot().ResumeAfterRestart; got != recoveryResumePaused {
		t.Fatalf("reloaded resume mode = %q, want %q", got, recoveryResumePaused)
	}
}

func TestRecoverySettingsRejectsInvalidMode(t *testing.T) {
	st := defaultRecoverySettingsStore(t.TempDir())
	if _, err := st.Set(recoverySettings{ResumeAfterRestart: "soon"}); !errors.Is(err, errInvalidRecoverySettings) {
		t.Fatalf("Set invalid error = %v, want errInvalidRecoverySettings", err)
	}
}

func TestRecoverySettingsRejectsInvalidFileMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recovery_settings.json"), []byte(`{"resumeAfterRestart":"soon"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if _, err := newRecoverySettingsStore(dir); !errors.Is(err, errInvalidRecoverySettings) {
		t.Fatalf("load invalid error = %v, want errInvalidRecoverySettings", err)
	}
}

func TestRecoverySettingsModeReportsEachChoice(t *testing.T) {
	st := defaultRecoverySettingsStore(t.TempDir())
	for _, mode := range []recoveryResumeMode{recoveryResumeManual, recoveryResumeAuto, recoveryResumePaused} {
		if _, err := st.Set(recoverySettings{ResumeAfterRestart: mode}); err != nil {
			t.Fatalf("Set %q: %v", mode, err)
		}
		if got := st.Mode(); got != mode {
			t.Fatalf("Mode() = %q, want %q", got, mode)
		}
		if got, want := st.AutoResumeAfterRestart(), mode == recoveryResumeAuto; got != want {
			t.Fatalf("AutoResumeAfterRestart() under %q = %v, want %v", mode, got, want)
		}
	}
}

// "Don't resume" parks the pane exactly like "Ask me" (exited, resumable, no
// process) but marks it RestartPaused so the client makes no resume offer.
func TestSuspendAfterRestartMarksOnlyDontResume(t *testing.T) {
	for _, tc := range []struct {
		mode       recoveryResumeMode
		wantPaused bool
	}{
		{recoveryResumeManual, false},
		{recoveryResumePaused, true},
	} {
		mgr := NewManager(t.TempDir())
		p := suspendAfterRestart(mgr, &PersistedPane{ID: "pane_r", ClaudeSessionID: "sess-r", AgentPid: 999999}, tc.mode)
		if p == nil {
			t.Fatalf("%s: suspendAfterRestart returned nil", tc.mode)
		}
		snap := p.Snapshot()
		if snap.Status != PaneExited || !snap.Resumable {
			t.Fatalf("%s: status=%q resumable=%v, want exited+resumable (the person can still resume)", tc.mode, snap.Status, snap.Resumable)
		}
		if snap.RestartPaused != tc.wantPaused {
			t.Fatalf("%s: RestartPaused = %v, want %v", tc.mode, snap.RestartPaused, tc.wantPaused)
		}
		if got, ok := mgr.Get("pane_r"); !ok || got != p {
			t.Fatalf("%s: pane not registered", tc.mode)
		}
	}
}

// A pane that needs recovery keeps its recovery card; RestartPaused is only for
// a cleanly suspended conversation.
func TestSuspendAfterRestartLeavesRecoveryErrorPanesAlone(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := suspendAfterRestart(mgr, &PersistedPane{ID: "pane_e", ClaudeSessionID: "sess-e", RecoveryError: "working directory does not exist"}, recoveryResumePaused)
	if p == nil {
		t.Fatal("suspendAfterRestart returned nil")
	}
	if p.Snapshot().RestartPaused {
		t.Fatal("RestartPaused set on a pane with a recovery error")
	}
}

func TestRestartPausedRidesPaneJSON(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := suspendAfterRestart(mgr, &PersistedPane{ID: "pane_j", ClaudeSessionID: "sess-j"}, recoveryResumePaused)
	b, err := json.Marshal(p.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"restartPaused":true`) {
		t.Fatalf("snapshot JSON lacks restartPaused: %s", b)
	}
}

// Harness panes that exit on their own resume automatically only under
// "Resume automatically"; "Don't resume" and "Ask me" leave them alone.
func TestHarnessAutoResumeOnlyUnderAuto(t *testing.T) {
	for _, mode := range []recoveryResumeMode{recoveryResumeManual, recoveryResumePaused} {
		st := defaultRecoverySettingsStore(t.TempDir())
		if _, err := st.Set(recoverySettings{ResumeAfterRestart: mode}); err != nil {
			t.Fatal(err)
		}
		mgr := NewManager(t.TempDir())
		p := mgr.SuspendExisting(&PersistedPane{ID: "pane_h", HarnessProvider: "deepseek", HarnessSessionID: "hs-1", Cmd: []string{"solu-harness"}})
		if p == nil {
			t.Fatal("SuspendExisting returned nil")
		}
		a := &apiServer{mgr: mgr, recoverySettings: st, dataDir: t.TempDir()}
		a.autoResumeHarnessPane(p)
		if got, _ := mgr.Get("pane_h"); got != p || got.Snapshot().Status != PaneExited {
			t.Fatalf("%s: harness pane was resumed", mode)
		}
	}
}
