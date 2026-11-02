package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// respawnAskServer is resumeTestServer with the production kill path: the
// real Manager.Kill and its onPaneRemoved hook, which respawns use to free
// the pane id they reuse.
func respawnAskServer(t *testing.T, p *Pane) *apiServer {
	t.Helper()
	close(p.done)
	api, _, _ := resumeTestServer(t, p)
	api.killPane = nil
	api.mgr.SetOnPaneRemoved(api.paneRemoved)
	api.asks = newAskStore(t.TempDir(), nil)
	return api
}

// Model, effort, role, refresh, resume, restart and context-pack changes all
// respawn through respawnInPlace; the pane keeps its asks.
func TestAsksSurviveRespawnInPlace(t *testing.T) {
	p := switchTestPane("pane_r", "", []string{"claude"}, "sess-r")
	p.Status, p.Resumable = PaneExited, true
	api := respawnAskServer(t, p)
	mustPost(t, api.asks, "pane_r", "deploy", "OK to deploy?")
	if _, err := api.respawnInPlace(p.Snapshot(), SpawnOpts{ID: "pane_r", Cwd: "/tmp", Cmd: []string{"claude"}, ClaudeSessionID: "sess-r"}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if _, ok := api.mgr.Get("pane_r"); !ok {
		t.Fatal("respawned pane missing")
	}
	if open := api.asks.openFor("pane_r"); len(open) != 1 {
		t.Fatalf("asks after respawn = %+v, want the ask kept", open)
	}
	// The resume route (a suspended pane after a reboot) too.
	np := api.mgr.panes["pane_r"]
	np.Status, np.Resumable = PaneExited, true
	close(np.done)
	if rec := postResume(api, "pane_r"); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if open := api.asks.openFor("pane_r"); len(open) != 1 {
		t.Fatalf("asks after resume = %+v, want the ask kept", open)
	}
}

// Switching a pane between terminal and native respawns it under the same id.
func TestAsksSurviveModeSwitch(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	p := switchTestPane("pane_switch", "", []string{"bash"}, "sess-123")
	p.LastPrompts = []string{"hi"}
	api := respawnAskServer(t, p)
	mustPost(t, api.asks, "pane_switch", "deploy", "OK to deploy?")
	req := httptest.NewRequest(http.MethodPost, "/api/panes/pane_switch/switch-mode", strings.NewReader(`{"kind":"native"}`))
	req.SetPathValue("id", "pane_switch")
	rec := httptest.NewRecorder()
	api.handleSwitchMode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body.String())
	}
	if open := api.asks.openFor("pane_switch"); len(open) != 1 {
		t.Fatalf("asks after switch = %+v, want the ask kept", open)
	}
}

// Closing a pane for good closes its asks: from the app, and by kill_pane.
func TestAsksCloseWhenThePaneIsClosed(t *testing.T) {
	a := confirmTestServer(t)
	a.asks = newAskStore(t.TempDir(), nil)
	a.mgr.SetOnPaneRemoved(a.paneRemoved)
	mustPost(t, a.asks, "social", "x", "closed from the app")
	mustPost(t, a.asks, "worker", "y", "closed by its lead")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/panes/social", nil)
	req.SetPathValue("id", "social")
	a.deletePane(rec, req)
	if len(a.asks.openFor("social")) != 0 {
		t.Fatalf("app close kept asks (%d)", rec.Code)
	}
	if res := a.killPaneNow("worker", "lead", ""); res["isError"] == true {
		t.Fatalf("kill_pane: %v", res)
	}
	if len(a.asks.openFor("worker")) != 0 {
		t.Fatal("kill_pane kept asks")
	}
}

// migrate-clean moves the agent to a new pane id; its asks go with it.
func TestAsksMoveWithMigrateClean(t *testing.T) {
	s, _ := newTestAskStore(t)
	mustPost(t, s, "old", "deploy", "OK to deploy?")
	if _, _, err := s.post(askPost{Pane: "lead", Kind: askKindWorker, Key: "worker-old", Text: "kept", WorkerPane: "old"}); err != nil {
		t.Fatal(err)
	}
	s.movePane("old", "new", "#3 Claude")
	open := s.openFor("new")
	if len(open) != 1 || open[0].Source != "#3 Claude" || len(s.openFor("old")) != 0 {
		t.Fatalf("after move: new=%+v old=%+v", open, s.openFor("old"))
	}
	s.paneGone("new")
	if len(s.openFor("lead")) != 0 {
		t.Fatal("worker ask did not follow the moved worker")
	}
}

// A dismissed text stays dismissed under a new key too.
func TestAskDismissMemoryFollowsTheText(t *testing.T) {
	s, _ := newTestAskStore(t)
	a := mustPost(t, s, "p1", "deploy", "OK to deploy?")
	if err := s.dismiss(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.post(askPost{Pane: "p1", Key: "deploy-2", Text: "ok to  deploy"}); err == nil || !strings.Contains(err.Error(), "dismissed") {
		t.Fatalf("same text under a new key: %v", err)
	}
	// Another pane may ask the same thing.
	mustPost(t, s, "p2", "deploy", "OK to deploy?")
}

// Enter that answers a menu (a permission prompt) is not a reply to the agent.
func TestAskReplyIgnoresEnterOnAMenu(t *testing.T) {
	prev := paneShowsMenu
	atMenu := true
	paneShowsMenu = func(*Pane) bool { return atMenu }
	t.Cleanup(func() { paneShowsMenu = prev })
	h := newAdoptTestPane(t, false)
	submits := make(chan string, 4)
	h.m.SetOnHumanSubmit(func(id string, _ time.Time) { submits <- id })
	person := h.dial(t, callerHuman, "")
	person.send(t, websocket.BinaryMessage, "\r")
	h.expectWrite(t, "\r")
	select {
	case <-submits:
		t.Fatal("Enter on a menu counted as a reply")
	case <-time.After(200 * time.Millisecond):
	}
	atMenu = false
	person.send(t, websocket.BinaryMessage, "\r")
	h.expectWrite(t, "\r")
	select {
	case <-submits:
	case <-time.After(2 * time.Second):
		t.Fatal("Enter at the input box was not seen")
	}
}
