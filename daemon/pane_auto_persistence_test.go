package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func autoSettingsServer(t *testing.T, a *apiServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/panes/{id}/auto-mode", a.setPaneAutoMode)
	mux.HandleFunc("POST /api/panes/{id}/never-auto-answer", a.setPaneNeverAutoAnswer)
	mux.HandleFunc("POST /api/panes/{id}/restart", a.handleRestartPane)
	mux.HandleFunc("POST /api/panes/{id}/resume", a.handleResumePane)
	mux.HandleFunc("POST /api/panes/{id}/refresh", a.handleRefresh)
	mux.HandleFunc("POST /api/panes/closed/{id}/reopen", a.reopenClosedPane)
	mux.HandleFunc("POST /api/panes/{id}/migrate-clean", a.migratePaneClean)
	mux.HandleFunc("POST /api/panes", a.createPane)
	mux.HandleFunc("GET /api/panes", a.listPanes)
	s := httptest.NewServer(mux) // random side port, never the installed daemon
	t.Cleanup(s.Close)
	return s
}

func autoSettingsPost(t *testing.T, s *httptest.Server, path, body string, want int) []byte {
	t.Helper()
	resp, err := s.Client().Post(s.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("POST %s: %d, want %d: %s", path, resp.StatusCode, want, b)
	}
	return b
}

func assertAutoSettings(t *testing.T, p *Pane, locked bool) {
	t.Helper()
	if p == nil {
		t.Fatal("pane missing")
	}
	snap := p.Snapshot()
	if snap.AutoMode != "" || snap.NeverAutoAnswer != locked {
		t.Fatalf("policy reverted: mode=%q locked=%v, want off/%v", snap.AutoMode, snap.NeverAutoAnswer, locked)
	}
}

// All HTTP lifecycle handlers run on a side port with a scratch HOME/store.
// Process hooks isolate provider/network requirements while exercising each
// real recipe, replacement handler, settings API and durable store.
func TestAutoOffAcrossLifecyclePaths(t *testing.T) {
	for _, path := range []string{"restart", "restart-fresh", "resume", "refresh", "migrate-inplace", "migrate-sibling", "workspace-restore", "reopen-agent", "reopen-shell"} {
		for _, locked := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/off", true: "/locked"}[locked], func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("SOLU_NATIVE_RENDERER", "1")
				p := switchTestPane("policy-pane", "", []string{"claude"}, "session-policy")
				p.ClaudeTranscriptPath = writeTempTranscript(t)
				if path == "reopen-shell" {
					p.ClaudeSessionID = ""
					p.Cmd = []string{"bash"}
				}
				if path == "migrate-inplace" {
					close(p.done)
				}
				if path == "refresh" {
					p.Kind = "native"
				}
				if path == "resume" {
					p.Status = PaneExited
				}
				a, _, _ := resumeTestServer(t, p)
				store, err := NewPaneStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				a.panes = store
				// Migrations allocate a new id; model actual pre-publication state.
				spawn := a.spawnPane
				a.spawnPane = func(o SpawnOpts) (*Pane, error) {
					if o.ID == "" {
						o.ID = "policy-replacement"
					}
					return spawn(o)
				}
				s := autoSettingsServer(t, a)
				base := "/api/panes/" + p.ID
				autoSettingsPost(t, s, base+"/auto-mode", `{"autoMode":"always"}`, 200)
				autoSettingsPost(t, s, base+"/auto-mode", `{"autoMode":""}`, 200)
				if locked {
					autoSettingsPost(t, s, base+"/never-auto-answer", `{"neverAutoAnswer":true}`, 200)
				}
				endpoint, body := base+"/"+path, ""
				switch path {
				case "restart":
					endpoint += "?force=1"
				case "restart-fresh":
					endpoint = base + "/restart?fresh=1&force=1"
				case "migrate-inplace":
					endpoint = base + "/migrate-clean"
					body = `{"mode":"inplace","confirm":true}`
				case "migrate-sibling":
					endpoint = base + "/migrate-clean"
					body = `{"mode":"sibling"}`
				case "reopen-shell", "reopen-agent":
					a.closed = newClosedPaneStore(t.TempDir())
					a.closed.add(closedPaneEntry{Snap: p.Snapshot()})
					endpoint = "/api/panes/closed/" + p.ID + "/reopen"
				case "workspace-restore":
					endpoint = "/api/panes"
					def := PaneDef{ID: p.ID, Cwd: p.Cwd, Cmd: p.Cmd, ClaudeSessionID: p.ClaudeSessionID, NeverAutoAnswer: locked}
					data, _ := json.Marshal(def)
					var req map[string]any
					_ = json.Unmarshal(data, &req)
					req["restore"] = true
					data, _ = json.Marshal(req)
					body = string(data)
				}
				want := 200
				if path == "workspace-restore" || path == "migrate-sibling" || strings.HasPrefix(path, "reopen-") {
					want = 201
				}
				result := autoSettingsPost(t, s, endpoint, body, want)
				var dto struct {
					ID   string `json:"id"`
					Pane struct {
						ID string `json:"id"`
					} `json:"pane"`
				}
				if err := json.Unmarshal(result, &dto); err != nil {
					t.Fatal(err)
				}
				id := dto.ID
				if id == "" {
					id = dto.Pane.ID
				}
				np, ok := a.mgr.Get(id)
				if !ok {
					t.Fatalf("replacement missing: %s", result)
				}
				assertAutoSettings(t, np, locked)
				// Reload panes.json, then exercise boot's suspended reconstruction.
				reloaded, err := NewPaneStore(filepath.Dir(store.path))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, info := range reloaded.List() {
					if info.ID == id {
						found = true
						if persistedPaneConversationID(info) == "" {
							info.RecoveryError = missingPaneIdentityMessage(info)
						}
						restored := NewManager(t.TempDir()).SuspendExisting(&info)
						assertAutoSettings(t, restored, locked)
					}
				}
				if !found {
					t.Fatalf("replacement %s not durable", id)
				}
			})
		}
	}
}

// Actual Manager/agent transport, not a mocked restore: boot queues Always,
// HTTP successfully saves Off on the suspended pane, then the queue resumes.
// Before the fix ResumeAgentPane reinstalled the old Always value.
func TestAutoOffSurvivesStaleBootResumeQueue(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "locked"}[locked], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", "/bin/sh")
			dir := t.TempDir()
			mgr := NewManager(dir)
			store, err := NewPaneStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			stale := PersistedPane{ID: "queued-shell", Cwd: t.TempDir(), Cmd: []string{"/bin/sh"}, AutoMode: "always", RecoveryError: "resume shell", Cols: 80, Rows: 24}
			p := mgr.SuspendExisting(&stale)
			if err := store.Put(p); err != nil {
				t.Fatal(err)
			}
			a := &apiServer{mgr: mgr, panes: store}
			s := autoSettingsServer(t, a)
			autoSettingsPost(t, s, "/api/panes/"+p.ID+"/auto-mode", `{"autoMode":""}`, 200)
			if locked {
				autoSettingsPost(t, s, "/api/panes/"+p.ID+"/never-auto-answer", `{"neverAutoAnswer":true}`, 200)
			}
			np, err := mgr.ResumeAgentPane(stale)
			if err != nil {
				t.Fatal(err)
			}
			if np != nil {
				t.Cleanup(func() { _ = mgr.Kill(np.ID) })
			}
			assertAutoSettings(t, np, locked)
			if err := store.Put(np); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewPaneStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			info := reloaded.List()[0]
			// A live agent reconnect after daemon restart reads the durable record.
			nextManager := NewManager(dir)
			attached, err := nextManager.AttachExisting(&info)
			if err != nil {
				t.Fatal(err)
			}
			assertAutoSettings(t, attached, locked)
			t.Cleanup(func() { _ = nextManager.Kill(attached.ID) })
			info.RecoveryError = missingPaneIdentityMessage(info)
			assertAutoSettings(t, NewManager(t.TempDir()).SuspendExisting(&info), locked)

		})
	}
}

func TestAutoSettingsRefuseRetiringPaneAndRevokeAutomaticWrites(t *testing.T) {
	a, p, frames, _ := newDirectAutoRepeatHarness(t, "locked-writer")
	server := &apiServer{mgr: a.mgr}
	s := autoSettingsServer(t, server)
	if !p.tryAcquireRespawn() {
		t.Fatal("lease")
	}
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/auto-mode", `{"autoMode":""}`, 409)
	p.releaseRespawn()
	old := p.autoModePolicy()
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/never-auto-answer", `{"neverAutoAnswer":true}`, 200)
	assertAutoSettings(t, p, true)
	for _, mode := range []string{"always", "cautious"} {
		autoSettingsPost(t, s, "/api/panes/"+p.ID+"/auto-mode", `{"autoMode":"`+mode+`"}`, 409)
	}
	if _, err := p.writeWithAutoModePolicy([]byte("\r"), false, old, time.Now().Add(time.Second)); !errors.Is(err, errAutoModePolicyChanged) {
		t.Fatalf("old decision wrote: %v", err)
	}
	// Even a newly captured standing-order decision cannot write when locked.
	if _, err := p.writeWithAutoModePolicy([]byte("\r"), false, p.autoModePolicy(), time.Now().Add(time.Second)); !errors.Is(err, errAutoModePolicyChanged) {
		t.Fatalf("locked decision wrote: %v", err)
	}
	p.expectAutoPrompt()
	if p.autoPromptExpected() {
		t.Fatal("locked hook armed auto prompt")
	}
	a.evaluateObservation(context.Background(), p.ID, p.Title, "")
	a.directAutoFire(context.Background(), p.ID, p.Title)
	if got := a.toolSendInput(context.Background(), "locked-send", p.ID, "1\n", 0, 0, false, func(map[string]any) {}); !strings.Contains(got, "locked") {
		t.Fatalf("standing-order send: %s", got)
	}
	expectNoFrame(t, frames, "locked auto paths")
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/never-auto-answer", `{"neverAutoAnswer":false}`, 200)
	assertAutoSettings(t, p, false)
}

func TestNeverAutoAnswerRequiresHumanEvenWithDelegation(t *testing.T) {
	a := authorityTestServer(t)
	p := switchTestPane("guarded", "", []string{"bash"}, "")
	a.mgr.panes[p.ID] = p
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	previous := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = previous })
	for _, who := range []callerKind{callerPane, callerWorker, callerUnverified, callerHuman} {
		identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: who, PaneID: "lead"} }
		for _, locked := range []bool{true, false} {
			rec := httptest.NewRecorder()
			b, _ := json.Marshal(map[string]bool{"neverAutoAnswer": locked})
			req := httptest.NewRequest("POST", "/api/panes/guarded/never-auto-answer", bytes.NewReader(b))
			req.SetPathValue("id", p.ID)
			a.setPaneNeverAutoAnswer(rec, req)
			want := 403
			if who == callerHuman {
				want = 200
			}
			if rec.Code != want {
				t.Fatalf("caller %s lock %v: %d %s", who, locked, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestNeverAutoAnswerBlocksNativeAllowlistAndAria(t *testing.T) {
	p := newTestNativePane()
	p.setNativeAllowlist([]string{"Bash"})
	p.setAutomaticAnswerSettings("", true)
	if how, _ := p.allowlistAutoAllows("Bash", json.RawMessage(`{"command":"git status"}`)); how != "" {
		t.Fatal("locked allowlist opted in")
	}
	if _, err := p.resolveAutomaticApproval("request"); !errors.Is(err, errAutoModePolicyChanged) {
		t.Fatalf("native auto resolve: %v", err)
	}
	if p.maybeAutoAllowFromList("request", "Bash", "tool") {
		t.Fatal("allowlist resolved")
	}
}

func TestAutoSettingsPersistenceFailureRollsBack(t *testing.T) {
	p := switchTestPane("failed-lock", "", []string{"bash"}, "")
	p.SetAutoMode("always")
	a, _, _ := resumeTestServer(t, p)
	a.panes = &failingPaneStore{putErr: errors.New("disk full")}
	s := autoSettingsServer(t, a)
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/never-auto-answer", `{"neverAutoAnswer":true}`, 500)
	if snap := p.Snapshot(); snap.AutoMode != "always" || snap.NeverAutoAnswer {
		t.Fatalf("failed lock changed policy: %+v", snap)
	}
}

// A restart that has captured Always must not acknowledge Off on its old
// object and then install Always on the replacement.
func TestAutoOffCannotSucceedOnPaneBeingRestarted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := switchTestPane("restarting", "", []string{"bash"}, "")
	p.SetAutoMode("always")
	a, _, _ := resumeTestServer(t, p)
	entered, release := make(chan struct{}), make(chan struct{})
	oldKill := a.killPane
	a.killPane = func(id string) error { close(entered); <-release; return oldKill(id) }
	s := autoSettingsServer(t, a)
	done := make(chan error, 1)
	go func() {
		resp, err := s.Client().Post(s.URL+"/api/panes/"+p.ID+"/restart?fresh=1&force=1", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				err = errors.New(resp.Status)
			}
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not reach snapshot boundary")
	}
	// The old implementation returned 200 here and reinstalled Always afterward.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/"+p.ID+"/auto-mode", strings.NewReader(`{"autoMode":""}`))
	req.SetPathValue("id", p.ID)
	a.setPaneAutoMode(rec, req)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rec.Code != 409 {
		t.Fatalf("Off acknowledged on retiring pane: %d %s", rec.Code, rec.Body.String())
	}
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/auto-mode", `{"autoMode":""}`, 200)
	np, _ := a.mgr.Get(p.ID)
	assertAutoSettings(t, np, false)
}

func TestNeverAutoAnswerRevokesQueuedNativeWriteButAllowsHuman(t *testing.T) {
	p, agentEnd := nativePaneForApproval(t, "locked-native-write")
	p.processNativeLine([]byte(`{"type":"control_request","request_id":"req-lock","request":{"subtype":"can_use_tool","tool_name":"Read","tool_use_id":"tool-lock","input":{"file_path":"/tmp/example"}}}`), false, false)
	p.agentWriteMu.Lock()
	done := make(chan error, 1)
	go func() { _, err := p.resolveAutomaticApproval("req-lock"); done <- err }()
	deadline := time.Now().Add(time.Second)
	for p.peekNativePending("req-lock") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// The resolver has taken the request and is queued at the transport lock.
	queued := p.peekNativePending("req-lock") == nil
	p.setAutomaticAnswerSettings("", true)
	p.agentWriteMu.Unlock()
	if !queued {
		t.Fatal("automatic resolver did not queue")
	}
	if err := <-done; !errors.Is(err, errAutoModePolicyChanged) {
		t.Fatalf("automatic write was not revoked: %v", err)
	}
	if p.peekNativePending("req-lock") == nil {
		t.Fatal("revoked approval was lost")
	}
	frames := make(chan error, 1)
	go func() {
		_ = agentEnd.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err := ReadFrame(agentEnd)
		frames <- err
	}()
	if _, err := p.resolveApproval("req-lock", true, ""); err != nil {
		t.Fatalf("human approval refused: %v", err)
	}
	if err := <-frames; err != nil {
		t.Fatal(err)
	}
}

func TestNeverAutoAnswerReopenNeverEnablesAutoMode(t *testing.T) {
	for _, session := range []string{"", "closed-session"} {
		opts := reopenSpawnOpts(closedPaneEntry{Snap: PaneSnapshot{Cmd: []string{"bash"}, ClaudeSessionID: session, AutoMode: "always", NeverAutoAnswer: true}}, 0, 0)
		if opts.AutoMode != "" || !opts.NeverAutoAnswer {
			t.Fatalf("reopen policy: auto=%q locked=%v", opts.AutoMode, opts.NeverAutoAnswer)
		}
	}
}

func TestNeverAutoAnswerAlreadyUnlockedIsNoOp(t *testing.T) {
	p := switchTestPane("already-unlocked", "", []string{"bash"}, "")
	p.SetAutoMode("always")
	a, _, _ := resumeTestServer(t, p)
	s := autoSettingsServer(t, a)
	autoSettingsPost(t, s, "/api/panes/"+p.ID+"/never-auto-answer", `{"neverAutoAnswer":false}`, 200)
	if p.Snapshot().AutoMode != "always" {
		t.Fatal("already-clear lock changed the mode")
	}
}

func TestAutoOffRevokesNativeDecisionContext(t *testing.T) {
	p, agentEnd := nativePaneForApproval(t, "native-old-decision")
	p.SetAutoMode("always")
	p.processNativeLine([]byte(nativeCanUseToolLine("old-decision", "old-tool", "Read", `{"file_path":"/tmp/example"}`)), false, false)
	a := testNativeAutoAria(t, p)
	ctx, _ := withAriaAutoModePolicy(context.Background(), p)
	p.SetAutoMode("")
	a.directAutoFireNative(ctx, p.ID, p.Title)
	got := a.toolResolvePermission(ctx, "old", p.ID, "old-decision", false, func(map[string]any) {})
	if !strings.Contains(got, "policy changed") {
		t.Fatalf("old native decision: %s", got)
	}
	if p.peekNativePending("old-decision") == nil {
		t.Fatal("old decision resolved the pending request")
	}
	_ = agentEnd.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := ReadFrame(agentEnd); err == nil {
		t.Fatal("revoked native decision wrote a frame")
	}
}
