package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFleetRespawnRequiresDurableIdentityAndPostSubmitEvidence(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := fleetTestPane(mgr, "pane-a", "", 101)
	if got := fleetRespawnEligibility(p); got.Blocker == "" {
		t.Fatalf("missing conversation identity was accepted: %+v", got)
	}
	p.MuseSessionID = "muse-conversation-a"
	if got := fleetRespawnEligibility(p); got.Blocker != "" || got.Busy {
		t.Fatalf("idle Muse conversation was not resumable: %+v", got)
	}
	p.Kind = ""
	p.fleetTurnOutputWatermark.Store(atomic.LoadInt64(&p.TotalStrippedChars))
	p.fleetTurnAdmitted.Store(true)
	if !fleetPaneBusyForRestart(p) {
		t.Fatal("just-submitted terminal turn was treated as settled before output")
	}
	atomic.AddInt64(&p.TotalStrippedChars, 1)
	if !fleetPaneBusyForRestart(p) {
		t.Fatal("output without a terminal hand-back prompt was treated as settled")
	}
}

func TestFleetRestartSeesNativeSubmissionBeforePromptPersistenceFinishes(t *testing.T) {
	daemonConn, agentConn := net.Pipe()
	p := &Pane{
		ID: "native-submit-race", Kind: "native", Agent: agentClaude, Status: PaneRunning,
		ClaudeSessionID: "session-a", agentConn: daemonConn, done: make(chan struct{}),
		buffer: NewRingBuffer(1024), nativeAdapter: NewClaudeAdapter(),
		nativeSubs: map[int64]chan NativeEvent{}, nativeStop: make(chan struct{}),
	}
	persisting := make(chan struct{})
	releasePersistence := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePersistence) }) }
	p.onPromptsCommit = func(*Pane) {
		close(persisting)
		<-releasePersistence
	}
	t.Cleanup(func() {
		release()
		p.shutdownNative()
		_ = daemonConn.Close()
		_ = agentConn.Close()
	})
	submitDone := make(chan error, 1)
	go func() {
		_, err := p.submitNativeInput("start a provider turn before persistence")
		submitDone <- err
	}()
	if _, _, err := ReadFrame(agentConn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-persisting:
	case <-time.After(time.Second):
		t.Fatal("submission did not reach prompt persistence")
	}
	if !fleetPaneBusyForRestart(p) {
		t.Fatal("transport-committed native turn was invisible during prompt persistence")
	}
	release()
	if err := <-submitDone; err != nil {
		t.Fatal(err)
	}
}

func TestFleetRestartNativeTurnLatchSurvivesDisplaySilence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		silence   time.Duration
		afterTool bool
	}{
		{name: "plain silence beyond display settle", silence: nativeSettleTimeout + time.Second},
		{name: "post-tool silence beyond gap allowance", silence: nativeGapAllowance + time.Second, afterTool: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestNativePane()
			p.MuseSessionID = "muse-active"
			p.nativeInTurn.Store(true)
			p.fleetTurnAdmitted.Store(true)
			p.nativeLastWasToolResult.Store(tt.afterTool)
			p.nativeLastEventNano.Store(time.Now().Add(-tt.silence).UnixNano())
			if !fleetPaneBusyForRestart(p) {
				t.Fatal("silent provider turn was treated as safe to terminate")
			}
			p.emitNativeEvent(NativeEvent{Type: EvTurnEnd, Subtype: "success"})
			if fleetPaneBusyForRestart(p) {
				t.Fatal("authoritative turn completion did not release restart latch")
			}
		})
	}
}

func TestFleetRespawnUsesProviderSpecificDurabilityProof(t *testing.T) {
	mgr := NewManager(t.TempDir())
	claude := fleetTestPane(mgr, "claude-pane", "", 101)
	claude.Agent = agentClaude
	claude.MuseSessionID = ""
	claude.ClaudeSessionID = "claude-conversation"
	claude.ClaudeTranscriptPath = writeTempTranscript(t)
	if got := fleetRespawnEligibility(claude); got.Blocker != "" {
		t.Fatalf("durable Claude transcript rejected: %+v", got)
	}
	claude.ClaudeTranscriptPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if got := fleetRespawnEligibility(claude); !strings.Contains(got.Blocker, "transcript") {
		t.Fatalf("missing Claude transcript blocker = %+v", got)
	}

	const threadID = "019f0000-0000-7000-8000-000000000123"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	codex := fleetTestPane(mgr, "codex-pane", "", 102)
	codex.Agent = agentCodex
	codex.MuseSessionID = ""
	codex.CodexThreadID = threadID
	if got := fleetRespawnEligibility(codex); !strings.Contains(got.Blocker, "rollout") {
		t.Fatalf("missing Codex rollout blocker = %+v", got)
	}
	day := filepath.Join(codexHome, "sessions", "2026", "09", "11")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-09-11T00-00-00-"+threadID+".jsonl"), validCodexRolloutMeta(threadID), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fleetRespawnEligibility(codex); got.Blocker != "" {
		t.Fatalf("durable Codex rollout rejected: %+v", got)
	}
}

// resumeTestServer wires an apiServer with spawn/kill hooks (so no real
// processes are launched) around a manager holding `exited`, and returns the
// server plus capture slices for the spawn opts and killed ids.
func resumeTestServer(t *testing.T, exited *Pane) (*apiServer, *[]SpawnOpts, *[]string) {
	t.Helper()
	mgr := NewManager(t.TempDir())
	mgr.panes[exited.ID] = exited
	spawnOpts := &[]SpawnOpts{}
	killed := &[]string{}
	api := &apiServer{
		mgr:   mgr,
		panes: &failingPaneStore{},
		spawnPane: func(o SpawnOpts) (*Pane, error) {
			*spawnOpts = append(*spawnOpts, o)
			np := switchTestPane(o.ID, o.Kind, o.Cmd, o.ClaudeSessionID)
			np.AutoMode = o.AutoMode
			np.NeverAutoAnswer = o.NeverAutoAnswer
			np.Agent = o.Agent
			np.Effort = o.Effort
			np.CodexThreadID = o.CodexThreadID
			np.Role = o.Role
			np.ContextPack = o.ContextPack
			if o.ContextPack != "" {
				np.ContextPackStatus = contextPackInjected
			}
			if o.Kind == "native" && effectiveAgentKind(o.Agent) == agentCodex {
				// The fake spawn has no app-server to emit thread/start|resume.
				// Model the positive readiness signal a healthy real spawn produces.
				np.nativeGotInit.Store(true)
			}
			mgr.panes[np.ID] = np
			return np, nil
		},
		killPane: func(id string) error {
			*killed = append(*killed, id)
			delete(mgr.panes, id)
			return nil
		},
	}
	return api, spawnOpts, killed
}

func postResume(api *apiServer, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/panes/"+id+"/resume", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	api.handleResumePane(rec, req)
	return rec
}

// A native exited-but-resumable pane respawns under the SAME id + kind, resuming
// the same session (nil cmd → Spawn rewrites the headless --resume argv), and the
// id is freed (killed) before the respawn so Spawn's duplicate-id guard passes.
func TestResumePaneNativeRespawns(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	exited := switchTestPane("pane_resume", "native", nil, "sess-xyz")
	exited.Status = PaneExited
	exited.Resumable = true
	exited.Flagged = true
	exited.FlagMemo = "keep me"
	api, spawnOpts, killed := resumeTestServer(t, exited)

	rec := postResume(api, "pane_resume")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if len(*killed) != 1 || (*killed)[0] != "pane_resume" {
		t.Fatalf("killed = %#v, want [pane_resume] (id freed before respawn)", *killed)
	}
	if len(*spawnOpts) != 1 {
		t.Fatalf("spawnOpts len = %d, want 1: %#v", len(*spawnOpts), *spawnOpts)
	}
	o := (*spawnOpts)[0]
	if o.ID != "pane_resume" || o.Kind != "native" || o.ClaudeSessionID != "sess-xyz" {
		t.Fatalf("respawn opts = %+v, want id=pane_resume kind=native session=sess-xyz", o)
	}
	if o.Cmd != nil {
		t.Fatalf("native respawn cmd = %#v, want nil (Spawn owns the headless --resume argv)", o.Cmd)
	}
	// Flag + memo (which SpawnOpts doesn't carry) are restored onto the new pane.
	np, ok := api.mgr.Get("pane_resume")
	if !ok {
		t.Fatal("resumed pane missing from manager")
	}
	if snap := np.Snapshot(); !snap.Flagged || snap.FlagMemo != "keep me" {
		t.Fatalf("restored state flagged=%v memo=%q, want true/\"keep me\"", snap.Flagged, snap.FlagMemo)
	}
}

// A terminal (kind "") resume rebuilds the interactive `claude --resume <id>` TUI.
func TestResumePaneTerminalBuildsResumeArgv(t *testing.T) {
	exited := switchTestPane("pane_t", "", []string{"claude"}, "sess-term")
	exited.Status = PaneExited
	exited.Resumable = true
	api, spawnOpts, _ := resumeTestServer(t, exited)

	rec := postResume(api, "pane_t")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	o := (*spawnOpts)[0]
	want := []string{"claude", "--resume", "sess-term"}
	if len(o.Cmd) != 3 || o.Cmd[0] != want[0] || o.Cmd[1] != want[1] || o.Cmd[2] != want[2] {
		t.Fatalf("terminal respawn cmd = %#v, want %#v", o.Cmd, want)
	}
}

func TestResumePaneCodexUsesThreadIdentity(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "native app server", kind: "native"},
		{name: "terminal TUI", kind: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exited := switchTestPane("pane_codex", tt.kind, []string{"codex"}, "")
			exited.Agent = agentCodex
			exited.CodexThreadID = "thread-123"
			exited.Status = PaneExited
			exited.Resumable = true
			api, spawnOpts, _ := resumeTestServer(t, exited)

			rec := postResume(api, exited.ID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if len(*spawnOpts) != 1 {
				t.Fatalf("spawn count = %d, want 1", len(*spawnOpts))
			}
			o := (*spawnOpts)[0]
			if o.Agent != agentCodex || o.CodexThreadID != "thread-123" || o.ClaudeSessionID != "" {
				t.Fatalf("Codex identity crossed providers: %+v", o)
			}
			if tt.kind == "native" {
				if o.Cmd != nil {
					t.Fatalf("native Codex cmd = %#v, want nil for Spawn app-server rewrite", o.Cmd)
				}
			} else if len(o.Cmd) != 3 || o.Cmd[0] != "codex" || o.Cmd[1] != "resume" || o.Cmd[2] != "thread-123" {
				t.Fatalf("terminal Codex cmd = %#v", o.Cmd)
			}
		})
	}
}

// Resuming a running pane is an idempotent no-op (200, no spawn).
func TestResumePaneRunningIsNoop(t *testing.T) {
	running := switchTestPane("pane_run", "native", nil, "sess-1") // Status=PaneRunning
	api, spawnOpts, killed := resumeTestServer(t, running)
	rec := postResume(api, "pane_run")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(*spawnOpts) != 0 || len(*killed) != 0 {
		t.Fatalf("running pane should not respawn: spawns=%d kills=%d", len(*spawnOpts), len(*killed))
	}
}

// A session-less exited pane (plain shell) is not resumable → 400, no spawn.
func TestResumePaneNoSessionRejected(t *testing.T) {
	exited := switchTestPane("pane_sh", "", []string{"bash"}, "")
	exited.Status = PaneExited
	api, spawnOpts, _ := resumeTestServer(t, exited)
	rec := postResume(api, "pane_sh")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(*spawnOpts) != 0 {
		t.Fatalf("session-less pane should not respawn, got %d spawns", len(*spawnOpts))
	}
}

// Resuming an unknown pane → 404.
func TestResumePaneNotFound(t *testing.T) {
	exited := switchTestPane("pane_real", "native", nil, "s")
	exited.Status = PaneExited
	exited.Resumable = true
	api, _, _ := resumeTestServer(t, exited)
	rec := postResume(api, "pane_ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// markExited flags a pane Resumable only when its claude process ended on its
// own (not an explicit user kill) AND a session id was captured. This is the
// signal the client keys the "Resume session" button off of.
func TestMarkExitedSetsResumable(t *testing.T) {
	cases := []struct {
		name        string
		sessionID   string
		userClosing bool
		want        bool
	}{
		{"claude pane, natural exit", "abc-123", false, true},
		{"claude pane, user killed", "abc-123", true, false},
		{"plain shell, no session", "", false, false},
		{"session-less, user killed", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pane{ClaudeSessionID: tc.sessionID, userClosing: tc.userClosing}
			p.markExited(nil)
			if p.Status != PaneExited {
				t.Fatalf("status = %q, want exited", p.Status)
			}
			snap := p.Snapshot()
			if snap.Resumable != tc.want {
				t.Fatalf("Resumable = %v, want %v", snap.Resumable, tc.want)
			}
			// A whitespace-only session id must not count as resumable.
		})
	}
}

// A whitespace-only session id is treated as no session.
func TestMarkExitedResumableTrimsSession(t *testing.T) {
	p := &Pane{ClaudeSessionID: "   "}
	p.markExited(nil)
	if p.Snapshot().Resumable {
		t.Fatal("whitespace-only session id must not be resumable")
	}
}

func TestMarkExitedCodexUsesThreadNotClaudeSession(t *testing.T) {
	withThread := &Pane{Agent: agentCodex, CodexThreadID: "thread-live"}
	withThread.markExited(nil)
	if !withThread.Snapshot().Resumable {
		t.Fatal("natural Codex exit with a thread id must be resumable")
	}

	wrongProviderID := &Pane{Agent: agentCodex, ClaudeSessionID: "claude-id-must-not-count"}
	wrongProviderID.markExited(nil)
	if wrongProviderID.Snapshot().Resumable {
		t.Fatal("Codex pane must not become resumable from ClaudeSessionID")
	}
}

// SuspendExisting materializes a dead-but-resumable pane (agent died across a
// daemon restart) as an EXITED, Resumable pane in the manager WITHOUT spawning
// a process — the default restart behavior, so the user opts into resuming via
// the same "Resume session" affordance instead of every session auto-respawning.
func TestSuspendExistingMaterializesResumablePane(t *testing.T) {
	mgr := NewManager(t.TempDir())
	info := &PersistedPane{
		ID:              "pane_susp",
		Kind:            "native",
		ClaudeSessionID: "sess-susp",
		Title:           "claude",
		AgentPid:        999999, // stale: the real agent died across the restart
		Flagged:         true,
		FlagMemo:        "keep",
		NativeAllowlist: []string{"Bash"},
	}
	p := mgr.SuspendExisting(info)
	if p == nil {
		t.Fatal("SuspendExisting returned nil for a pane with a session id")
	}
	if got, ok := mgr.Get("pane_susp"); !ok || got != p {
		t.Fatal("suspended pane not registered in the manager")
	}
	snap := p.Snapshot()
	if snap.Status != PaneExited {
		t.Fatalf("status = %q, want exited", snap.Status)
	}
	if !snap.Resumable {
		t.Fatal("suspended pane must be Resumable so the UI offers Resume")
	}
	if snap.ClaudeSessionID != "sess-susp" {
		t.Fatalf("session id = %q, want sess-susp", snap.ClaudeSessionID)
	}
	if !snap.Flagged || snap.FlagMemo != "keep" {
		t.Fatalf("flag state lost: flagged=%v memo=%q", snap.Flagged, snap.FlagMemo)
	}
	// The native allowlist must survive so a later resume keeps the user's opt-ins.
	if len(snap.NativeAllowlist) != 1 || snap.NativeAllowlist[0] != "Bash" {
		t.Fatalf("native allowlist not carried forward: %#v", snap.NativeAllowlist)
	}
}

func TestSuspendExistingCodexPreservesThread(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := mgr.SuspendExisting(&PersistedPane{
		ID: "pane_codex_susp", Kind: "native", Agent: agentCodex,
		CodexThreadID: "thread-susp", ClaudeSessionID: "",
	})
	if p == nil {
		t.Fatal("SuspendExisting returned nil for Codex thread")
	}
	snap := p.Snapshot()
	if !snap.Resumable || snap.Agent != agentCodex || snap.CodexThreadID != "thread-susp" || snap.ClaudeSessionID != "" {
		t.Fatalf("suspended Codex identity = %+v", snap)
	}
}

// Killing a suspended (or exited) pane must release subscribers bound to the
// dropped object — the respawn-in-place path (click Resume after a reboot)
// replaces the manager entry with a fresh pane under the SAME id, and a
// subscriber left parked on the orphan is a zombie: the resumed claude paints
// to the successor's empty sub map while the client's socket stays silently
// open (feedback #18's blank pane; the user's Ctrl+C "fix" was just the
// keystroke's write error finally tearing the zombie down). The channel close
// is what tells the client to reconnect and resolve the successor.
func TestKillSuspendedPaneReleasesSubscribers(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := mgr.SuspendExisting(&PersistedPane{ID: "pane_z", ClaudeSessionID: "sess-z"})
	if p == nil {
		t.Fatal("SuspendExisting returned nil")
	}
	s := p.SubscribeStream(80, 24, subModeFull, 0, false)
	if err := mgr.Kill("pane_z"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case _, ok := <-s.ch:
		if ok {
			t.Fatal("expected the subscriber channel to close, got a payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel not closed by Manager.Kill — a client WS would stay parked on the orphaned pane object while the respawned pane paints to nobody")
	}
	// A late subscriber racing the kill (a client reconnect that resolved the
	// old object just before it left the manager) must get an already-closed
	// stream — bounced to reconnect, never parked on the orphan.
	late := p.SubscribeStream(80, 24, subModeFull, 0, false)
	if _, ok := <-late.ch; ok {
		t.Fatal("subscribe on a detached pane must return an already-closed stream")
	}
}

// A pane with no captured session id can't be resumed exactly, so SuspendExisting
// declines it (returns nil, leaves it unregistered) — the caller drops it.
func TestSuspendExistingNoSessionReturnsNil(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if p := mgr.SuspendExisting(&PersistedPane{ID: "x", ClaudeSessionID: "  "}); p != nil {
		t.Fatal("a pane with only a whitespace session id must not be suspended")
	}
	if _, ok := mgr.Get("x"); ok {
		t.Fatal("non-resumable pane must not be registered in the manager")
	}
}

// Closing a suspended pane must not hang: it has no process or readLoop, so its
// done channel is pre-closed and Kill returns immediately.
func TestSuspendedPaneKillDoesNotBlock(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if p := mgr.SuspendExisting(&PersistedPane{ID: "pane_k", ClaudeSessionID: "s"}); p == nil {
		t.Fatal("expected a suspended pane")
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Kill("pane_k") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Kill returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill on a suspended pane blocked — done channel not pre-closed?")
	}
	if _, ok := mgr.Get("pane_k"); ok {
		t.Fatal("killed pane should be removed from the manager")
	}
}

// Kill() marks the pane as user-closing so a subsequent markExited (fired when
// the child dies) does not flag it Resumable.
func TestKillMarksUserClosing(t *testing.T) {
	p := &Pane{ClaudeSessionID: "abc-123", done: make(chan struct{})}
	close(p.done) // so Kill's wait returns immediately instead of the 3s timeout
	_ = p.Kill()
	if !p.userClosing {
		t.Fatal("Kill should set userClosing")
	}
	p.markExited(nil)
	if p.Snapshot().Resumable {
		t.Fatal("a user-killed claude pane must not be resumable")
	}
}
