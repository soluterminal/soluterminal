package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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

// TestSwitchModeToNativeRespawnsWithResume verifies the happy path: a terminal
// pane is killed and respawned with Kind=="native" + the same ClaudeSessionID
// (so Spawn would append --resume), preserving the pane id, and the kind change
// is persisted.
func TestSwitchModeToNativeRespawnsWithResume(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	mgr := NewManager(t.TempDir())
	store := &failingPaneStore{}
	orig := switchTestPane("pane_switch", "", []string{"bash"}, "sess-123")
	// A real conversation (recorded prompts): resume continuity applies. A pane
	// with NO prompts would correctly drop the session id instead (see
	// TestSwitchModeVirginSessionSkipsWaitAndResume).
	orig.LastPrompts = []string{"hi"}
	mgr.panes[orig.ID] = orig

	var spawnOpts []SpawnOpts
	var killed []string
	api := &apiServer{
		mgr:   mgr,
		panes: store,
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			spawnOpts = append(spawnOpts, o)
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error {
			killed = append(killed, id)
			delete(mgr.panes, id)
			return nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_switch/switch-mode", strings.NewReader(`{"kind":"native"}`))
	req.SetPathValue("id", "pane_switch")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if len(killed) != 1 || killed[0] != "pane_switch" {
		t.Fatalf("killed = %#v, want [pane_switch]", killed)
	}
	if len(spawnOpts) != 1 {
		t.Fatalf("spawnOpts len = %d, want 1: %#v", len(spawnOpts), spawnOpts)
	}
	o := spawnOpts[0]
	if o.ID != "pane_switch" {
		t.Fatalf("respawn id = %q, want pane_switch (identity preserved)", o.ID)
	}
	if o.Kind != "native" {
		t.Fatalf("respawn kind = %q, want native", o.Kind)
	}
	if o.ClaudeSessionID != "sess-123" {
		t.Fatalf("respawn sessionID = %q, want sess-123 (resume continuity)", o.ClaudeSessionID)
	}
	if o.Cmd != nil {
		t.Fatalf("respawn Cmd = %#v, want nil (Spawn rewrites to headless argv for native)", o.Cmd)
	}
	if np, ok := mgr.Get("pane_switch"); !ok || np.Kind != "native" {
		t.Fatalf("live pane kind = %v ok=%v, want native pane", np, ok)
	}
	if ids := store.putIDsSnapshot(); len(ids) != 1 || ids[0] != "pane_switch" {
		t.Fatalf("persisted = %#v, want [pane_switch]", ids)
	}
}

// TestSwitchModeToTerminalRebuildsInteractiveResume is the regression for the
// "clicking Term errors" bug: a native pane's Cmd is the HEADLESS stream-json
// argv, which is broken in a terminal. Switching to term must spawn the
// INTERACTIVE `claude --resume <session>` TUI, not restore the headless argv.
func TestSwitchModeToTerminalRebuildsInteractiveResume(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	mgr := NewManager(t.TempDir())
	store := &failingPaneStore{}
	headless := []string{"claude", "-p", "--output-format", "stream-json", "--resume", "sess-xyz"}
	orig := switchTestPane("pane_switch", "native", headless, "sess-xyz")
	orig.LastPrompts = []string{"hi"} // real conversation → resume continuity applies
	mgr.panes[orig.ID] = orig

	var spawnOpts []SpawnOpts
	api := &apiServer{
		mgr:   mgr,
		panes: store,
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			spawnOpts = append(spawnOpts, o)
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error { delete(mgr.panes, id); return nil },
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_switch/switch-mode", strings.NewReader(`{"kind":""}`))
	req.SetPathValue("id", "pane_switch")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if len(spawnOpts) != 1 {
		t.Fatalf("spawnOpts len = %d, want 1", len(spawnOpts))
	}
	o := spawnOpts[0]
	if o.Kind != "" {
		t.Fatalf("respawn kind = %q, want \"\" (terminal)", o.Kind)
	}
	want := []string{"claude", "--resume", "sess-xyz"}
	if len(o.Cmd) != 3 || o.Cmd[0] != want[0] || o.Cmd[1] != want[1] || o.Cmd[2] != want[2] {
		t.Fatalf("respawn Cmd = %#v, want %#v (interactive TUI, NOT the headless argv)", o.Cmd, want)
	}
}

// TestSwitchModeRollsBackWhenNativeSessionDies is the load-bearing rollback
// test: the native respawn comes up but its agent exits immediately (the
// --resume failure mode). The handler must roll back to the ORIGINAL terminal
// pane and 500 — never leave a dead Kind=="native" pane parked.
func TestSwitchModeRollsBackWhenNativeSessionDies(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	mgr := NewManager(t.TempDir())
	store := &failingPaneStore{}
	orig := switchTestPane("pane_resume_fail", "", []string{"bash"}, "stale-sess")
	mgr.panes[orig.ID] = orig

	var spawnKinds []string
	api := &apiServer{
		mgr:   mgr,
		panes: store,
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			spawnKinds = append(spawnKinds, o.Kind)
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			// The native respawn's agent dies instantly (bad --resume): mark it
			// exited so the health check trips. The rollback (terminal) spawn
			// stays running.
			if o.Kind == "native" {
				code := 1
				np.Status = PaneExited
				np.ExitCode = &code
			}
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error {
			delete(mgr.panes, id)
			return nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_resume_fail/switch-mode", strings.NewReader(`{"kind":"native"}`))
	req.SetPathValue("id", "pane_resume_fail")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("body = %q, want rollback message", rec.Body.String())
	}
	// Spawn attempts: the native respawn is tried THREE times (2 retries on the
	// quick-exit / session-handoff race), then the rollback terminal one.
	if len(spawnKinds) != 4 || spawnKinds[0] != "native" || spawnKinds[1] != "native" || spawnKinds[2] != "native" || spawnKinds[3] != "" {
		t.Fatalf("spawnKinds = %#v, want [native, native, native, ''] (native + 2 retries, then rollback)", spawnKinds)
	}
	// The surviving pane must be the ORIGINAL terminal kind, alive — never a
	// dead native pane.
	got, ok := mgr.Get("pane_resume_fail")
	if !ok {
		t.Fatal("pane gone after rollback; want original restored")
	}
	if got.Kind != "" {
		t.Fatalf("restored pane kind = %q, want '' (terminal); a dead native pane must never be left parked", got.Kind)
	}
	if got.Status == PaneExited {
		t.Fatal("restored pane is exited; rollback must leave a LIVE pane")
	}
}

// TestSwitchModeRollsBackWhenRespawnErrors covers the other native failure
// branch: Spawn itself errors (e.g. claude not on PATH). Same invariant — roll
// back to the original, 500.
func TestSwitchModeRollsBackWhenRespawnErrors(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	mgr := NewManager(t.TempDir())
	store := &failingPaneStore{}
	orig := switchTestPane("pane_spawn_err", "", []string{"zsh"}, "")
	mgr.panes[orig.ID] = orig

	var spawnKinds []string
	api := &apiServer{
		mgr:   mgr,
		panes: store,
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			spawnKinds = append(spawnKinds, o.Kind)
			if o.Kind == "native" {
				return nil, errors.New("command not found: claude")
			}
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error {
			delete(mgr.panes, id)
			return nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_spawn_err/switch-mode", strings.NewReader(`{"kind":"native"}`))
	req.SetPathValue("id", "pane_spawn_err")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	if len(spawnKinds) != 4 || spawnKinds[0] != "native" || spawnKinds[1] != "native" || spawnKinds[2] != "native" || spawnKinds[3] != "" {
		t.Fatalf("spawnKinds = %#v, want [native, native, native, ''] (native errored + 2 retries, then rollback)", spawnKinds)
	}
	got, ok := mgr.Get("pane_spawn_err")
	if !ok || got.Kind != "" || got.Status == PaneExited {
		t.Fatalf("restored pane = %#v ok=%v, want live terminal pane", got, ok)
	}
	// Original command preserved on the terminal rollback respawn.
	if len(got.Cmd) != 1 || got.Cmd[0] != "zsh" {
		t.Fatalf("restored cmd = %#v, want [zsh]", got.Cmd)
	}
}

// TestSwitchModeNativeGatedByEnv: a "native" target is rejected 400 when the
// kill-switch SOLU_NATIVE_RENDERER=0 is set, and the pane is NOT killed. (Native
// is ON by default now; the only way to a 400 here is the explicit opt-out.)
func TestSwitchModeNativeGatedByEnv(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "0")
	mgr := NewManager(t.TempDir())
	orig := switchTestPane("pane_gated", "", []string{"bash"}, "")
	mgr.panes[orig.ID] = orig

	killed := false
	api := &apiServer{
		mgr: mgr,
		killPane: func(id string) error {
			killed = true
			return nil
		},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			t.Fatal("spawn must not be called when native is gated off")
			return nil, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_gated/switch-mode", strings.NewReader(`{"kind":"native"}`))
	req.SetPathValue("id", "pane_gated")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if killed {
		t.Fatal("pane was killed before the env gate rejected the switch")
	}
}

// TestSwitchModeNoOpWhenAlreadyTargetKind: switching to the current kind is an
// idempotent 200 with no kill/respawn.
func TestSwitchModeNoOpWhenAlreadyTargetKind(t *testing.T) {
	mgr := NewManager(t.TempDir())
	orig := switchTestPane("pane_noop", "", []string{"bash"}, "")
	mgr.panes[orig.ID] = orig

	api := &apiServer{
		mgr: mgr,
		killPane: func(id string) error {
			t.Fatal("kill must not be called for a no-op switch")
			return nil
		},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			t.Fatal("spawn must not be called for a no-op switch")
			return nil, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_noop/switch-mode", strings.NewReader(`{"kind":""}`))
	req.SetPathValue("id", "pane_noop")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (idempotent no-op); body=%q", rec.Code, rec.Body.String())
	}
}

// TestNativeRendererEnabledDefaultOn locks in the shipping semantics: native
// mode is AVAILABLE by default (zero config — no env var needed to use the
// per-pane Native toggle), and SOLU_NATIVE_RENDERER acts only as a kill-switch.
func TestNativeRendererEnabledDefaultOff(t *testing.T) {
	// Focus was retired (2026-10): off unless explicitly re-enabled.
	cases := []struct {
		env  string
		want bool
	}{
		{"", false},        // unset → retired
		{"1", true},        // explicit re-enable
		{"true", true},     //
		{"on", true},       //
		{" yes ", true},    // trimmed
		{"0", false},       //
		{"off", false},     //
		{"garbage", false}, // unknown value → stays retired
	}
	for _, c := range cases {
		t.Setenv("SOLU_NATIVE_RENDERER", c.env)
		if got := nativeRendererEnabled(); got != c.want {
			t.Errorf("nativeRendererEnabled() with env=%q = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestSwitchSpawnOptsMirrorsResume locks in switchSpawnOpts' parity with
// resumeSpawnOpts: resume-continuity fields (session id, transcript path,
// title override) carried in both directions, the interactive-TUI Cmd rebuild
// for a native→terminal flip, and the dims decision — live client dims when
// provided, else persisted dims marked DimsUnverified so the first attach
// re-measures (the mis-wrapped-after-switch fix).
func TestSwitchSpawnOptsMirrorsResume(t *testing.T) {
	base := PaneSnapshot{
		ID:                   "pane_s",
		Cwd:                  "/tmp",
		Title:                "agent",
		Cols:                 283, // deliberately stale-looking persisted dims
		Rows:                 62,
		ClaudeSessionID:      " sess-1 ", // trimmed on the way through
		ClaudeTranscriptPath: " /tr/sess-1.jsonl ",
		TitleOverride:        "my custom title",
		Provider:             "zai",
		Effort:               "high",
		Model:                "claude-fable-5",
		NativeAllowlist:      []string{"Read"},
	}

	t.Run("native→terminal without live dims marks DimsUnverified", func(t *testing.T) {
		snap := base
		snap.Kind = "native"
		o := switchSpawnOpts(snap, "", 0, 0)
		if !o.DimsUnverified {
			t.Fatal("no live dims → DimsUnverified must be true (stale persisted dims must not be trusted)")
		}
		if o.Cols != 283 || o.Rows != 62 {
			t.Fatalf("fallback dims = %dx%d, want persisted 283x62", o.Cols, o.Rows)
		}
		want := []string{"claude", "--resume", "sess-1"}
		if len(o.Cmd) != 3 || o.Cmd[0] != want[0] || o.Cmd[1] != want[1] || o.Cmd[2] != want[2] {
			t.Fatalf("Cmd = %#v, want %#v", o.Cmd, want)
		}
		if o.ClaudeSessionID != "sess-1" {
			t.Fatalf("session id = %q, want trimmed sess-1", o.ClaudeSessionID)
		}
		if o.ClaudeTranscriptPath != "/tr/sess-1.jsonl" {
			t.Fatalf("transcript path = %q, want trimmed /tr/sess-1.jsonl (resume-guard parity)", o.ClaudeTranscriptPath)
		}
		if o.TitleOverride != "my custom title" {
			t.Fatalf("title override = %q dropped across the flip", o.TitleOverride)
		}
		if o.Provider != "zai" || o.Effort != "high" {
			t.Fatalf("provider/effort = %q/%q, want zai/high", o.Provider, o.Effort)
		}
		if o.Model != "claude-fable-5" {
			t.Fatalf("model = %q dropped across the flip (resume would silently keep the session-stored model)", o.Model)
		}
	})

	t.Run("native→terminal with live dims uses them verified", func(t *testing.T) {
		snap := base
		snap.Kind = "native"
		o := switchSpawnOpts(snap, "", 142, 38)
		if o.DimsUnverified {
			t.Fatal("live dims provided → DimsUnverified must be false")
		}
		if o.Cols != 142 || o.Rows != 38 {
			t.Fatalf("dims = %dx%d, want live 142x38", o.Cols, o.Rows)
		}
	})

	t.Run("native→terminal without session id spawns fresh claude", func(t *testing.T) {
		snap := base
		snap.Kind = "native"
		snap.ClaudeSessionID = "  "
		o := switchSpawnOpts(snap, "", 0, 0)
		if len(o.Cmd) != 1 || o.Cmd[0] != "claude" {
			t.Fatalf("Cmd = %#v, want [claude]", o.Cmd)
		}
	})

	t.Run("terminal→native leaves Cmd nil and dims untouched", func(t *testing.T) {
		snap := base
		snap.Kind = ""
		snap.Cmd = []string{"bash"}
		o := switchSpawnOpts(snap, "native", 0, 0)
		if o.Cmd != nil {
			t.Fatalf("Cmd = %#v, want nil (Spawn rewrites the headless argv)", o.Cmd)
		}
		if o.DimsUnverified {
			t.Fatal("native target must not set DimsUnverified (no PTY width to mis-trust)")
		}
		if o.Kind != "native" {
			t.Fatalf("kind = %q, want native", o.Kind)
		}
	})

	t.Run("terminal→terminal restores original cmd", func(t *testing.T) {
		snap := base
		snap.Kind = ""
		snap.Cmd = []string{"htop"}
		o := switchSpawnOpts(snap, "", 0, 0)
		if len(o.Cmd) != 1 || o.Cmd[0] != "htop" {
			t.Fatalf("Cmd = %#v, want [htop]", o.Cmd)
		}
	})
}

// TestSwitchModeThreadsLiveDims proves the wire: a switch-mode POST carrying
// cols/rows lands them on the spawn opts (verified), and one without leaves
// the daemon on the DimsUnverified fallback.
func TestSwitchModeThreadsLiveDims(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	run := func(t *testing.T, body string) SpawnOpts {
		mgr := NewManager(t.TempDir())
		orig := switchTestPane("pane_dims", "native", []string{"claude", "-p"}, "sess-d")
		mgr.panes[orig.ID] = orig
		var got []SpawnOpts
		api := &apiServer{
			mgr:   mgr,
			panes: &failingPaneStore{},
			spawnPane: func(o SpawnOpts) (*Pane, error) {
				got = append(got, o)
				np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
				np.buffer.Write([]byte("painted")) // health signal → fast confirm
				mgr.panes[np.ID] = np
				return np, nil
			},
			killPane: func(id string) error { delete(mgr.panes, id); return nil },
		}
		req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_dims/switch-mode", strings.NewReader(body))
		req.SetPathValue("id", "pane_dims")
		rec := httptest.NewRecorder()
		api.handleSwitchMode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if len(got) != 1 {
			t.Fatalf("spawns = %d, want 1", len(got))
		}
		return got[0]
	}

	t.Run("with live dims", func(t *testing.T) {
		o := run(t, `{"kind":"","cols":142,"rows":38}`)
		if o.Cols != 142 || o.Rows != 38 || o.DimsUnverified {
			t.Fatalf("opts dims = %dx%d unverified=%v, want live 142x38 verified", o.Cols, o.Rows, o.DimsUnverified)
		}
	})
	t.Run("without live dims", func(t *testing.T) {
		o := run(t, `{"kind":""}`)
		if !o.DimsUnverified {
			t.Fatal("no cols/rows in request → DimsUnverified fallback expected")
		}
	})
}

// TestConfirmSwitchHealthyEarlyExit locks in the latency win: a respawn showing
// a positive health signal (native: first system/init; terminal: first PTY
// output) confirms at ~switchModeHealthFloor instead of always burning the full
// switchModeHealthGrace — but never BEFORE the floor, and retries (fullGrace)
// still wait out the whole window.
func TestConfirmSwitchHealthyEarlyExit(t *testing.T) {
	api := &apiServer{}

	t.Run("native init signal exits at floor", func(t *testing.T) {
		p := switchTestPane("pane_h1", "native", nil, "s")
		p.nativeGotInit.Store(true)
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, "native", false); err != nil {
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

	t.Run("terminal output signal exits at floor", func(t *testing.T) {
		p := switchTestPane("pane_h2", "", []string{"claude"}, "s")
		p.buffer.Write([]byte("first paint"))
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, "", false); err != nil {
			t.Fatalf("healthy pane errored: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= switchModeHealthGrace {
			t.Fatalf("no early exit: took %v, want < full grace %v", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("no signal waits out the full grace", func(t *testing.T) {
		p := switchTestPane("pane_h3", "native", nil, "s")
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, "native", false); err != nil {
			t.Fatalf("running pane errored: %v", err)
		}
		if elapsed := time.Since(start); elapsed < switchModeHealthGrace {
			t.Fatalf("returned at %v without a health signal, want the full %v (no-signal fallback unchanged)", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("fullGrace disables the early exit on retries", func(t *testing.T) {
		p := switchTestPane("pane_h4", "native", nil, "s")
		p.nativeGotInit.Store(true)
		start := time.Now()
		if err := api.confirmSwitchHealthy(p, "native", true); err != nil {
			t.Fatalf("healthy pane errored: %v", err)
		}
		if elapsed := time.Since(start); elapsed < switchModeHealthGrace {
			t.Fatalf("retry attempt returned at %v, want the full grace %v (races can die late)", elapsed, switchModeHealthGrace)
		}
	})

	t.Run("quick exit still fails despite a signal", func(t *testing.T) {
		p := switchTestPane("pane_h5", "native", nil, "s")
		p.nativeGotInit.Store(true)
		p.Status = PaneExited
		if err := api.confirmSwitchHealthy(p, "native", false); err == nil {
			t.Fatal("exited pane confirmed healthy; want errNativeSessionDied")
		}
	})
}

// TestConfirmSwitchHealthyAllowsCodexHandshakePastClaudeGrace is the regression
// for Terminal -> Focus returning 500 on a healthy Codex thread. Codex has to
// complete initialize AND thread/resume; that routinely crosses Claude's 600ms
// quick-exit window. A readiness signal arriving after that old deadline must
// still win without waiting out the full Codex ceiling.
func TestConfirmSwitchHealthyAllowsCodexHandshakePastClaudeGrace(t *testing.T) {
	api := &apiServer{}
	p := switchTestPane("pane_codex_slow_init", "native", nil, "")
	p.Agent = agentCodex

	signalAfter := switchModeHealthGrace + 100*time.Millisecond
	go func() {
		time.Sleep(signalAfter)
		p.nativeGotInit.Store(true)
	}()

	start := time.Now()
	if err := api.confirmSwitchHealthy(p, "native", false); err != nil {
		t.Fatalf("healthy Codex pane errored after delayed handshake: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < signalAfter {
		t.Fatalf("returned before Codex readiness signal: %v < %v", elapsed, signalAfter)
	}
	if elapsed >= codexSwitchModeHealthGrace {
		t.Fatalf("did not exit on Codex readiness: %v >= %v", elapsed, codexSwitchModeHealthGrace)
	}
}

// TestMain keeps the package hermetic and shortens the transcript-flush guard.
// Fixture session ids never have on-disk transcripts, so every switch test
// would otherwise idle out the full production wait before the kill.
//
// Daemon-spawned shells (panes, steward sessions, and publish.sh's test gate)
// export SOLU_RELEASE_REPO, SOLU_RELEASES_REPO, and the workflow resource-cap
// variables. The source-repo resolvers (releaseCutRepoPath, releaseLedgerRepoKey,
// declareReleaseCut, releaseReadinessProber.RepoPath, and compute) and the
// resolve…Override cap resolvers read those variables first, so host values silently
// replace test fixture repos and caps. On 2026-08-15 that made publish.sh refuse
// v0.0.1001 after release tests inspected the real repository.
//
// This cannot live in per-test t.Setenv calls: the affected
// TestReleaseCutAPI* and TestReleaseReadiness* tests use t.Parallel, where
// t.Setenv panics. Tests that deliberately exercise these overrides are
// non-parallel and set them explicitly after this package-wide scrub.
//
// The suite also protects the enclosing repository's administrative link. On
// 2026-08-23, agent-generated test code running in a review worktree overwrote
// its .git gitdir pointer with /tmp/fake_git. A package-wide snapshot catches
// and restores that mutation even when the responsible test was generated at
// runtime rather than present in tracked source.
var (
	daemonPackageRequestedTimeout string
	daemonPackageActiveTimeout    string
)

// daemonPackageDefaultParallelism mirrors scripts/daemon-test-gate.sh's
// -parallel policy for trees that do not carry the script.
const daemonPackageDefaultParallelism = 8

func daemonPackageCanonicalParallelism() (int, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(sourceFile) {
		return 0, fmt.Errorf("resolve canonical daemon test gate from source path %q", sourceFile)
	}
	gatePath := filepath.Join(filepath.Dir(sourceFile), "..", "scripts", "daemon-test-gate.sh")
	data, err := os.ReadFile(gatePath)
	if errors.Is(err, os.ErrNotExist) {
		// A source tree without the repository's test gate (the public export)
		// uses the gate's policy as of this writing.
		return daemonPackageDefaultParallelism, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read canonical daemon test gate: %w", err)
	}
	match := regexp.MustCompile(`-parallel=([0-9]+)`).FindSubmatch(data)
	if match == nil {
		return 0, errors.New("canonical daemon test gate has no -parallel policy")
	}
	parallelism, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, fmt.Errorf("parse canonical daemon test parallelism %q: %w", match[1], err)
	}
	if parallelism < 1 {
		return 0, fmt.Errorf("canonical daemon test parallelism must be positive, got %d", parallelism)
	}
	return parallelism, nil
}

func daemonPackageParallelismExplicit(args []string) bool {
	for _, arg := range args {
		if arg == "-parallel" || arg == "-test.parallel" ||
			strings.HasPrefix(arg, "-parallel=") || strings.HasPrefix(arg, "-test.parallel=") {
			return true
		}
	}
	return false
}

func TestDaemonPackagePreservesRequestedTimeout(t *testing.T) {
	timeoutFlag := flag.Lookup("test.timeout")
	if timeoutFlag == nil {
		t.Fatal("test.timeout flag is unavailable")
	}
	if _, err := time.ParseDuration(timeoutFlag.Value.String()); err != nil {
		t.Fatalf("parse active timeout: %v", err)
	}
	if timeoutFlag.Value.String() != daemonPackageActiveTimeout {
		t.Fatalf("active timeout changed after TestMain from %s to %s", daemonPackageActiveTimeout, timeoutFlag.Value.String())
	}
	requested, err := time.ParseDuration(daemonPackageRequestedTimeout)
	if err != nil {
		t.Fatalf("parse requested timeout: %v", err)
	}
	if requested == 10*time.Minute {
		if timeoutFlag.Value.String() != (15 * time.Minute).String() {
			t.Fatalf("implicit Go timeout was not extended: requested=%s active=%s", daemonPackageRequestedTimeout, timeoutFlag.Value.String())
		}
	} else if timeoutFlag.Value.String() != daemonPackageRequestedTimeout {
		t.Fatalf("TestMain changed explicit timeout from %s to %s", daemonPackageRequestedTimeout, timeoutFlag.Value.String())
	}
}

func TestDaemonPackageUsesControlledParallelismHeadroom(t *testing.T) {
	if daemonPackageParallelismExplicit(os.Args[1:]) {
		t.Skip("explicit -parallel override is authoritative; gate default not asserted")
	}
	parallelFlag := flag.Lookup("test.parallel")
	if parallelFlag == nil {
		t.Fatal("test.parallel flag is unavailable")
	}
	var got int
	if _, err := fmt.Sscanf(parallelFlag.Value.String(), "%d", &got); err != nil {
		t.Fatalf("parse active test parallelism: %v", err)
	}
	canonical, err := daemonPackageCanonicalParallelism()
	if err != nil {
		t.Fatal(err)
	}
	if got < canonical {
		t.Fatalf("daemon package parallelism = %d, want canonical gate default %d", got, canonical)
	}
}

func TestDaemonPackageExplicitLowerParallelismOverrideIsValid(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestDaemonPackageUsesControlledParallelismHeadroom$",
		"-test.parallel=2",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("explicit lower test parallelism was rejected: %v\n%s", err, output)
	}
}

func TestDaemonPackageCanonicalParallelismIsWorkingDirectoryIndependent(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestDaemonPackageUsesControlledParallelismHeadroom$",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("canonical daemon parallelism depends on the process working directory: %v\n%s", err, output)
	}
}

func TestDaemonPackageParallelismExplicitOverrideDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "default invocation", args: []string{"-test.timeout=25m"}},
		{name: "go test equals", args: []string{"-test.parallel=3"}, want: true},
		{name: "go test split", args: []string{"-test.parallel", "3"}, want: true},
		{name: "source spelling", args: []string{"-parallel=3"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonPackageParallelismExplicit(tc.args); got != tc.want {
				t.Fatalf("explicit = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSwitchModeWaitsForTranscriptFlush proves the pre-kill flush guard: a
// switch racing claude's FIRST transcript flush waits (bounded) for the file
// to appear instead of killing immediately — so the respawn's `--resume` finds
// the conversation instead of silently starting fresh.
func TestSwitchModeWaitsForTranscriptFlush(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	prev := switchModeTranscriptFlushWait
	switchModeTranscriptFlushWait = 600 * time.Millisecond
	t.Cleanup(func() { switchModeTranscriptFlushWait = prev })

	const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0001"
	cwd := "/tmp"
	projDir := filepath.Join(cfg, "projects", claudeProjectDirName(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projDir, sid+".jsonl")

	mgr := NewManager(t.TempDir())
	orig := switchTestPane("pane_flush", "native", []string{"claude", "-p"}, sid)
	orig.Cwd = cwd
	orig.LastPrompts = []string{"do the thing"} // a REAL conversation is racing its first flush
	mgr.panes[orig.ID] = orig

	var killedAt time.Time
	api := &apiServer{
		mgr:   mgr,
		panes: &failingPaneStore{},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			np.buffer.Write([]byte("painted"))
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error {
			killedAt = time.Now()
			delete(mgr.panes, id)
			return nil
		},
	}

	// Simulate claude's initial flush landing ~120ms after the switch begins.
	start := time.Now()
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = os.WriteFile(transcript, []byte(`{"type":"summary"}`+"\n"), 0o644)
	}()

	if _, err := api.performSwitchMode(orig, "", false, 0, 0); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	if killedAt.IsZero() {
		t.Fatal("pane was never killed")
	}
	waited := killedAt.Sub(start)
	if waited < 100*time.Millisecond {
		t.Fatalf("kill happened at +%v — the flush guard did not wait for the transcript", waited)
	}
	if waited >= switchModeTranscriptFlushWait+200*time.Millisecond {
		t.Fatalf("kill happened at +%v — waited past the bounded window", waited)
	}
}

// TestSwitchModeProceedsWhenTranscriptNeverFlushes: the guard is bounded — a
// real conversation whose transcript never appears delays the switch by at
// most the window, then the switch proceeds (Spawn's fresh-start notice
// surfaces the consequence).
func TestSwitchModeProceedsWhenTranscriptNeverFlushes(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	prev := switchModeTranscriptFlushWait
	switchModeTranscriptFlushWait = 150 * time.Millisecond
	t.Cleanup(func() { switchModeTranscriptFlushWait = prev })

	mgr := NewManager(t.TempDir())
	orig := switchTestPane("pane_noflush", "native", []string{"claude", "-p"}, "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0002")
	orig.LastPrompts = []string{"hello"} // real conversation → the bounded wait applies
	mgr.panes[orig.ID] = orig

	api := &apiServer{
		mgr:   mgr,
		panes: &failingPaneStore{},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			np.buffer.Write([]byte("painted"))
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error { delete(mgr.panes, id); return nil },
	}

	start := time.Now()
	if _, err := api.performSwitchMode(orig, "", false, 0, 0); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("switch took %v — the flush guard is not bounded", elapsed)
	}
}

// TestSwitchModeVirginSessionSkipsWaitAndResume: a pane whose session NEVER had
// a user turn (no recorded prompts, no transcript) has nothing to resume — the
// switch must not burn the flush wait, must spawn WITHOUT --resume (a doomed
// resume id would just trip Spawn's guard + a misleading "couldn't resume"
// banner on a pane that had no conversation), and the rollback snapshot stays
// untouched.
func TestSwitchModeVirginSessionSkipsWaitAndResume(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	prev := switchModeTranscriptFlushWait
	switchModeTranscriptFlushWait = 5 * time.Second // would be VERY visible if waited
	t.Cleanup(func() { switchModeTranscriptFlushWait = prev })

	mgr := NewManager(t.TempDir())
	orig := switchTestPane("pane_virgin", "native", []string{"claude", "-p"}, "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0003")
	// No LastPrompts: a freshly-opened Focus pane the user flips before typing.
	mgr.panes[orig.ID] = orig

	var got []SpawnOpts
	api := &apiServer{
		mgr:   mgr,
		panes: &failingPaneStore{},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			got = append(got, o)
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			np.buffer.Write([]byte("painted"))
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error { delete(mgr.panes, id); return nil },
	}

	start := time.Now()
	if _, err := api.performSwitchMode(orig, "", false, 0, 0); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("virgin switch took %v — the flush wait must be skipped when there is nothing to resume", elapsed)
	}
	if len(got) != 1 {
		t.Fatalf("spawns = %d, want 1", len(got))
	}
	if got[0].ClaudeSessionID != "" {
		t.Fatalf("spawn carried session id %q, want empty (nothing to resume)", got[0].ClaudeSessionID)
	}
	if len(got[0].Cmd) != 1 || got[0].Cmd[0] != "claude" {
		t.Fatalf("spawn Cmd = %#v, want a fresh [claude] with no --resume", got[0].Cmd)
	}
}

func TestMain(m *testing.M) {
	// Exercise the production fixed-recipe helper in subprocess fixtures.
	if len(os.Args) >= 2 && os.Args[1] == capabilityCommand {
		os.Exit(runCapabilityCommand(os.Args[2:], os.Stdout))
	}
	// Handler tests act as the person at the app; caller_authority_test.go
	// switches the real identification back on.
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	// Task schedules are created all over the suite on hosts with no Claude
	// CLI; schedules_task_claude_test.go exercises the real check.
	scheduleTaskRunnable = func() error { return nil }
	// Generated-storage producers must never allocate fixture roots beneath the
	// invoking user's real cache. Keep one process-stable cache base so every
	// managed fixture is bounded to this test process and discarded afterward.
	generatedCacheRoot, generatedCacheErr := os.MkdirTemp("", "soluterminal-daemon-test-cache-")
	if generatedCacheErr != nil {
		fmt.Fprintln(os.Stderr, "FAIL: create generated-storage test cache: "+generatedCacheErr.Error())
		os.Exit(2)
	}
	if err := os.Setenv("XDG_CACHE_HOME", generatedCacheRoot); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: configure generated-storage test cache: "+err.Error())
		_ = os.RemoveAll(generatedCacheRoot)
		os.Exit(2)
	}
	// Fixtures can enable browser acceptance and construct an engine, which
	// launches boot cleanup. Never let ordinary verification discover/delete
	// the real daemon's live acceptance sandboxes. Tests supply fake endpoints;
	// only the explicitly opted-in live-browser test keeps host discovery.
	os.Unsetenv("SOLU_BROWSER_ACCEPT")
	if os.Getenv("SOLU_BROWSER_ACCEPT_LIVE") != "1" {
		os.Unsetenv("SOLU_SOLUBROWSE_MCP_URL")
		os.Unsetenv("SOLU_SOLUBROWSE_MCP_TOKEN")
	}
	os.Unsetenv("SOLU_RELEASE_REPO")
	// Fixtures assert an immediate Enter on the first evaluation. Pacing
	// tests opt back in per Aria via productionAutoFirePacing.
	productionAutoFirePacing = autoFirePacing{
		minAge:     ariaAutoFireMinAge,
		minGap:     ariaAutoFireMinGap,
		settle:     ariaAutoRepeatSettle,
		unrendered: ariaAutoUnrenderedHold,
		verify:     ariaAutoFireVerifyAfter,
	}
	ariaAutoFireMinAge, ariaAutoFireMinGap, ariaAutoRepeatSettle, ariaAutoUnrenderedHold = 0, 0, 0, 0
	ariaAutoFireVerifyAfter = nil
	os.Unsetenv("SOLU_RELEASES_REPO")
	os.Unsetenv("SOLU_WORKFLOW_MAX_HEAVY")
	os.Unsetenv("SOLU_WORKFLOW_MAX_CONCURRENT")
	os.Unsetenv("SOLU_WORKFLOW_MIN_FREE_MB")
	os.Unsetenv("SOLU_WORKFLOW_ADAPTIVE_HEAVY")
	os.Unsetenv("SOLU_WORKFLOW_HOST_PROBE")
	if !flag.Parsed() {
		flag.Parse()
	}
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		daemonPackageRequestedTimeout = timeoutFlag.Value.String()
		// Go supplies its implicit default to the test binary as though it were
		// an explicit flag. Keep the package-level safety margin here as well as
		// in workflow.json so a frozen verifier command from before the config
		// update cannot still trip the observed 10-minute alarm. Deliberately
		// preserve every non-default timeout chosen by a caller.
		if requested, err := time.ParseDuration(daemonPackageRequestedTimeout); err == nil && requested == 10*time.Minute {
			_ = flag.Set("test.timeout", (15 * time.Minute).String())
		}
		daemonPackageActiveTimeout = timeoutFlag.Value.String()
	}
	// Follow the canonical gate's test-parallelism default for direct package
	// invocations too. The gate is the sole owner of that policy and its 25m
	// package alarm; an explicit caller override remains authoritative.
	if parallelFlag := flag.Lookup("test.parallel"); parallelFlag != nil && !daemonPackageParallelismExplicit(os.Args[1:]) {
		var current int
		canonical, err := daemonPackageCanonicalParallelism()
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAIL: configure daemon test parallelism: "+err.Error())
			os.Exit(2)
		}
		if _, err := fmt.Sscanf(parallelFlag.Value.String(), "%d", &current); err == nil && current < canonical {
			if err := parallelFlag.Value.Set(fmt.Sprint(canonical)); err != nil {
				fmt.Fprintln(os.Stderr, "FAIL: configure daemon test parallelism: "+err.Error())
				os.Exit(2)
			}
		}
	}
	switchModeTranscriptFlushWait = 20 * time.Millisecond
	// Low-disk relief (reached from any fixture whose disk probe reports free
	// space below the floor) trims the engine's shared Go build cache and reaps
	// managed dirs under its temp root. Keep every engine this binary builds
	// off the host's real shared cache, which concurrent verification rows are
	// compiling against, and off the real /tmp, whose dirs other processes own.
	// Tests that exercise relief inject their own cache resolver and temp root.
	hermeticTmpRoot, err := os.MkdirTemp("", "soluterminal-daemon-test-tmproot-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: create hermetic engine temp root: "+err.Error())
		os.Exit(2)
	}
	guard := snapshotEnclosingGitLink()
	code := m.Run()
	_ = os.RemoveAll(hermeticTmpRoot)
	if guard != nil {
		if msg := guard.check(); msg != "" {
			fmt.Fprintln(os.Stderr, "FAIL: test suite mutated the enclosing repo's .git link: "+msg)
			code = 1
		}
	}
	// Engine cleanup can finish a cache write just as the suite returns. Retry
	// this test-owned root briefly so a concurrent final mkdir does not make
	// the whole package fail after every test has passed.
	var cacheCleanupErr error
	for attempt := 0; attempt < 5; attempt++ {
		cacheCleanupErr = os.RemoveAll(generatedCacheRoot)
		if cacheCleanupErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cacheCleanupErr; err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: remove generated-storage test cache: "+err.Error())
		code = 1
	}
	os.Exit(code)
}

// The fallback must track the gate wherever the gate exists.
func TestDaemonPackageDefaultParallelismMatchesGate(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "scripts", "daemon-test-gate.sh"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no repository test gate in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`-parallel=([0-9]+)`).FindSubmatch(data)
	if match == nil || string(match[1]) != strconv.Itoa(daemonPackageDefaultParallelism) {
		t.Fatalf("gate parallelism %q, fallback %d: update daemonPackageDefaultParallelism", match, daemonPackageDefaultParallelism)
	}
}
