package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortenPaneConfirmWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := paneConfirmWait
	paneConfirmWait = d
	t.Cleanup(func() { paneConfirmWait = prev })
}

// confirmTestServer has the person's pane (no origin: opened before origins
// were recorded), an agent lead and the lead's worker.
func confirmTestServer(t *testing.T) *apiServer {
	t.Helper()
	dir := t.TempDir()
	a := &apiServer{mgr: NewManager(dir), agentAuthority: newAgentAuthority(dir), uiBus: newUIBus()}
	mk := func(id, title string, origin *PaneOrigin) *Pane {
		p := testPaneWithTail(t, id, title, "")
		p.Origin = origin
		p.done = make(chan struct{})
		close(p.done) // no process: Kill returns at once
		a.mgr.panes[id] = p
		return p
	}
	mk("lead", "Orchestrator", &PaneOrigin{Source: paneOriginUser})
	mk("worker", "Worker", &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead"})
	mk("social", "Social loop", nil)
	a.ws = &WorkspaceStore{
		path:       filepath.Join(dir, "workspaces.json"),
		data:       map[string]*Workspace{"w": {Name: "w", PaneIds: []string{"lead", "worker", "social"}}},
		recentAdds: map[string]map[string]time.Time{},
	}
	return a
}

func answerPaneConfirm(t *testing.T, a *apiServer, id, body string, who callerKind) *httptest.ResponseRecorder {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: who, PaneID: "lead"} }
	t.Cleanup(func() { identifyCallerHook = prev })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/pane-confirmations/{id}", a.handleAnswerPaneConfirm)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/pane-confirmations/"+id, strings.NewReader(body)))
	return rec
}

func onlyPendingConfirm(t *testing.T, a *apiServer) paneConfirmView {
	t.Helper()
	pending := a.pendingPaneConfirms()
	if len(pending) != 1 {
		t.Fatalf("want one open request, got %+v", pending)
	}
	return pending[0]
}

func TestKillPaneUserRequestedWaitsForThePersonsOK(t *testing.T) {
	a := confirmTestServer(t)
	events := a.uiBus.subscribe()
	done := make(chan any, 1)
	go func() {
		done <- a.mcpToolKillPane(map[string]any{"pane_id": "social", "user_requested": true}, "lead")
	}()
	var view paneConfirmView
	deadline := time.Now().Add(5 * time.Second)
	for len(a.pendingPaneConfirms()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no confirmation was raised")
		}
		time.Sleep(5 * time.Millisecond)
	}
	view = onlyPendingConfirm(t, a)
	if view.Message != "#1 Orchestrator wants to close pane #3 “Social loop”. Closing stops what is running in it; files are not deleted." || view.Title != "Close pane #3 “Social loop”?" {
		t.Fatalf("request = %+v", view)
	}
	if _, ok := a.mgr.Get("social"); !ok {
		t.Fatal("closed before the person answered")
	}
	select {
	case ev := <-events:
		if ev.Type != "pane.confirm" || ev.Text != view.ID || ev.Hint != view.Message {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("the app was not told")
	}
	// The agent cannot answer its own request.
	if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerPane); rec.Code != http.StatusForbidden {
		t.Fatalf("agent allowed its own request: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := a.mgr.Get("social"); !ok {
		t.Fatal("an agent's answer closed the pane")
	}
	// The person allows it.
	rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerHuman)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Killed pane social") {
		t.Fatalf("allow = %d %s", rec.Code, rec.Body.String())
	}
	res := <-done
	if text, failed := mcpResultText(t, res); failed || text != "Killed pane social (you allowed it in the app)." {
		t.Fatalf("tool result = %q", text)
	}
	if _, ok := a.mgr.Get("social"); ok {
		t.Fatal("still open after the person allowed it")
	}
	if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerHuman); rec.Code != http.StatusNotFound {
		t.Fatalf("answered twice: %d", rec.Code)
	}
}

func TestKillPaneUserRequestedDenied(t *testing.T) {
	a := confirmTestServer(t)
	done := make(chan any, 1)
	go func() { done <- a.mcpToolKillPane(map[string]any{"pane_id": "lead", "user_requested": true}, "worker") }()
	for len(a.pendingPaneConfirms()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	view := onlyPendingConfirm(t, a)
	if rec := answerPaneConfirm(t, a, view.ID, `{"allow":false}`, callerHuman); rec.Code != http.StatusOK {
		t.Fatalf("deny = %d", rec.Code)
	}
	text, failed := mcpResultText(t, <-done)
	if !failed || text != "The person said no in the app: pane #1 “Orchestrator” was left as it is. Don't ask again unless they tell you to." {
		t.Fatalf("denied result = %q", text)
	}
	if _, ok := a.mgr.Get("lead"); !ok {
		t.Fatal("closed after the person said no")
	}
}

func TestKillPaneConfirmOutlivesTheToolCall(t *testing.T) {
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	a := confirmTestServer(t)
	text, failed := mcpResultText(t, a.mcpToolKillPane(map[string]any{"pane_id": "social", "user_requested": true}, "lead"))
	if failed || text != "pane #3 “Social loop” is not closed yet: waiting for your OK in the app. The person has been asked to allow it; if they do within 10 minutes, it happens then. Don't call again: tell them it is waiting for their OK, and check list_panes later." {
		t.Fatalf("waiting result = %q", text)
	}
	// Asking again does not raise a second prompt.
	a.mcpToolKillPane(map[string]any{"pane_id": "social", "user_requested": true}, "lead")
	view := onlyPendingConfirm(t, a)
	if _, ok := a.decidePaneConfirm(view.ID, true); !ok {
		t.Fatal("request gone")
	}
	if _, ok := a.mgr.Get("social"); ok {
		t.Fatal("allowing later did not close the pane")
	}
}

func TestPaneConfirmExpires(t *testing.T) {
	a := confirmTestServer(t)
	req, err := a.requestPaneConfirm("k", paneConfirmView{Action: paneConfirmClose, Pane: "pane #3", PaneID: "social"}, func() map[string]any { return toolOK("ran") })
	if err != nil {
		t.Fatal(err)
	}
	a.expirePaneConfirm(req.view.ID)
	<-req.done
	if text, failed := mcpResultText(t, req.result); !failed || !strings.Contains(text, "Nobody answered") {
		t.Fatalf("expired = %q", text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("expired request still open")
	}
}

func TestPaneConfirmLimitsOpenRequestsPerAgent(t *testing.T) {
	a := confirmTestServer(t)
	for i := 0; i < paneConfirmMaxPerCaller; i++ {
		if _, err := a.requestPaneConfirm(string(rune('a'+i)), paneConfirmView{CallerPaneID: "lead"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.requestPaneConfirm("more", paneConfirmView{CallerPaneID: "lead"}, nil); err == nil {
		t.Fatal("an agent may stack up prompts")
	}
}

func TestKillPaneAgentRules(t *testing.T) {
	a := confirmTestServer(t)
	// Without user_requested, the person's pane (no origin) is refused.
	text, failed := mcpResultText(t, a.mcpToolKillPane(map[string]any{"pane_id": "social"}, "worker"))
	if !failed || text != "pane #3 “Social loop” was not started by you, so you can't close it. If the user asked you to close it, call kill_pane again with user_requested: true and they will be asked to allow it in the SoluTerminal app. Otherwise leave it: they can close it themselves." {
		t.Fatalf("nil-origin pane = %q", text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("a refusal raised a prompt")
	}
	// A lead closes its own worker without asking.
	if text, failed := mcpResultText(t, a.mcpToolKillPane(map[string]any{"pane_id": "worker"}, "lead")); failed {
		t.Fatalf("own worker: %q", text)
	}
	if _, ok := a.mgr.Get("worker"); ok {
		t.Fatal("own worker still open")
	}
	// With Settings → Agent permissions on, the agent acts for the person.
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpResultText(t, a.mcpToolKillPane(map[string]any{"pane_id": "social", "user_requested": true}, "lead")); failed {
		t.Fatalf("delegated: %q", text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("delegated close still asked")
	}
}

// The app closes panes over the REST API, which agents' ownership rules do
// not touch.
func TestPersonClosesAnyPaneFromTheApp(t *testing.T) {
	a := confirmTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/panes/{id}", a.deletePane)
	for _, id := range []string{"social", "lead"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/panes/"+id, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("close %s from the app = %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

func TestSetPaneInfoNilOriginRefused(t *testing.T) {
	a := confirmTestServer(t)
	text, failed := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "social", "handle": "mine"}, "lead"))
	if !failed || !strings.HasPrefix(text, "pane #3 “Social loop” was not started by you, so you can't change its handle or working_on.") {
		t.Fatalf("nil-origin rename = %q", text)
	}
}

func TestRenamePaneAgentRules(t *testing.T) {
	a := confirmTestServer(t)
	social, _ := a.mgr.Get("social")
	lead, _ := a.mgr.Get("lead")
	// The person's pane (no origin): refused without user_requested.
	text, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": "safe to close"}, "worker"))
	if !failed || text != "pane #3 “Social loop” was not started by you, so you can't rename it. If the user asked you to rename it, call rename_pane again with user_requested: true and they will be asked to allow it in the SoluTerminal app. Otherwise leave it: they can rename it themselves." {
		t.Fatalf("nil-origin rename = %q", text)
	}
	if got := social.Snapshot().Title; got != "Social loop" {
		t.Fatalf("title changed to %q", got)
	}
	// Another agent's lead pane (origin user): refused too.
	if _, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "lead", "title": "x"}, "worker")); !failed {
		t.Fatal("a worker renamed its lead")
	}
	// Its own pane, and the panes it started or leads: allowed.
	if text, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "worker", "title": "API fix"}, "lead")); failed {
		t.Fatalf("own worker: %q", text)
	}
	if text, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "lead", "title": "Lead"}, "lead")); failed || lead.Snapshot().Title != "Lead" {
		t.Fatalf("own pane: %q", text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("allowed renames raised a prompt")
	}
}

func TestRenamePaneUserRequestedWaitsForThePersonsOK(t *testing.T) {
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	a := confirmTestServer(t)
	social, _ := a.mgr.Get("social")
	text, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": "safe to close", "user_requested": true}, "lead"))
	if failed || !strings.HasPrefix(text, "pane #3 “Social loop” is not renamed yet: waiting for your OK in the app.") {
		t.Fatalf("waiting result = %q", text)
	}
	if got := social.Snapshot().Title; got != "Social loop" {
		t.Fatalf("renamed before the person allowed it: %q", got)
	}
	view := onlyPendingConfirm(t, a)
	if view.Action != paneConfirmRename || view.Title != "Rename pane #3 “Social loop”?" || view.Message != "#1 Orchestrator wants to rename pane #3 “Social loop” to “safe to close”." {
		t.Fatalf("request = %+v", view)
	}
	// The agent can't allow it; the person can.
	if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerPane); rec.Code != http.StatusForbidden {
		t.Fatalf("agent allowed its own rename: %d", rec.Code)
	}
	if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerHuman); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Renamed pane social") {
		t.Fatalf("allow = %d %s", rec.Code, rec.Body.String())
	}
	if got := social.Snapshot().Title; got != "safe to close" {
		t.Fatalf("title after allow = %q", got)
	}
	// A denied rename leaves the title alone.
	a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": "again", "user_requested": true}, "worker")
	if _, ok := a.decidePaneConfirm(onlyPendingConfirm(t, a).ID, false); !ok {
		t.Fatal("no request to deny")
	}
	if got := social.Snapshot().Title; got != "safe to close" {
		t.Fatalf("title after deny = %q", got)
	}
}

func TestRenamePaneDelegatedAndUnattributed(t *testing.T) {
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	a := confirmTestServer(t)
	// No pane header: the caller owns nothing, not even agent-started panes.
	if _, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "worker", "title": "x"}, "")); !failed {
		t.Fatal("an unattributed caller renamed a pane")
	}
	// Agent permissions on: user_requested from a pane agent acts for the person.
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": "Renamed", "user_requested": true}, "lead")); failed {
		t.Fatalf("delegated: %q", text)
	}
	// ...but not for a caller outside the panes.
	text, _ := mcpResultText(t, a.mcpToolRenamePane(map[string]any{"pane_id": "lead", "title": "x", "user_requested": true}, ""))
	if !strings.Contains(text, "waiting for your OK in the app") {
		t.Fatalf("unattributed with the grant on = %q", text)
	}
}

// mcpAs calls the real /mcp handler for a tool, from a caller the daemon
// traces as traced (nil: untraceable) while it names header in
// X-Soluterminal-Pane.
func mcpAs(t *testing.T, a *apiServer, traced *callerIdentity, header, tool string, args map[string]any) (string, bool) {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity {
		if traced == nil {
			return callerIdentity{Kind: callerUnverified}
		}
		return *traced
	}
	defer func() { identifyCallerHook = prev }()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-token")
	if header != "" {
		req.Header.Set("X-Soluterminal-Pane", header)
	}
	rec := httptest.NewRecorder()
	a.mcp(rec, req)
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Result == nil {
		t.Fatalf("%s: %d %s", tool, rec.Code, rec.Body.String())
	}
	return mcpResultText(t, resp.Result)
}

// traceTestServer adds a rogue pane with its own child to confirmTestServer:
// lead started worker; rogue started kid.
func traceTestServer(t *testing.T) *apiServer {
	t.Helper()
	a := confirmTestServer(t)
	a.gate = &authGate{token: "fixture-token"}
	for id, origin := range map[string]*PaneOrigin{
		"rogue": {Source: paneOriginUser},
		"kid":   {Source: paneOriginAgent, ByPaneID: "rogue", LeadPaneID: "rogue"},
	} {
		p := testPaneWithTail(t, id, id, "")
		p.Origin = origin
		p.done = make(chan struct{})
		close(p.done)
		a.mgr.panes[id] = p
	}
	return a
}

// An agent that names another pane in X-Soluterminal-Pane is still the pane
// its connection traces to: it can't act on that pane or its workers without
// the person's OK, and keeps acting on its own.
func TestMCPCallerHeaderCannotBorrowAnotherPanesOwnership(t *testing.T) {
	a := traceTestServer(t)
	rogue := &callerIdentity{Kind: callerPane, PaneID: "rogue", PID: 4242}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"kill_pane", map[string]any{"pane_id": "worker"}},
		{"kill_pane", map[string]any{"pane_id": "lead"}},
		{"rename_pane", map[string]any{"pane_id": "lead", "title": "hijacked"}},
		{"rename_pane", map[string]any{"pane_id": "worker", "title": "hijacked"}},
		{"set_pane_info", map[string]any{"pane_id": "lead", "working_on": "hijacked"}},
		{"set_pane_info", map[string]any{"pane_id": "worker", "handle": "hijacked"}},
	} {
		// Header = the victim itself, or the victim's starter.
		for _, header := range []string{"lead", "worker"} {
			text, failed := mcpAs(t, a, rogue, header, tc.tool, tc.args)
			if !failed || !strings.Contains(text, "was not started by you") {
				t.Fatalf("%s %v with header %s: %q", tc.tool, tc.args, header, text)
			}
		}
	}
	for _, id := range []string{"lead", "worker"} {
		p, ok := a.mgr.Get(id)
		if !ok {
			t.Fatalf("%s was closed", id)
		}
		if snap := p.Snapshot(); snap.Title == "hijacked" || (snap.Identity != nil && (snap.Identity.WorkingOn == "hijacked" || snap.Identity.Handle == "hijacked")) {
			t.Fatalf("%s was changed: %+v", id, snap.Identity)
		}
	}
	// user_requested from the traced pane goes to the person, attributed to it.
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	if text, _ := mcpAs(t, a, rogue, "lead", "kill_pane", map[string]any{"pane_id": "worker", "user_requested": true}); !strings.Contains(text, "waiting for your OK in the app") {
		t.Fatalf("user_requested = %q", text)
	}
	if view := onlyPendingConfirm(t, a); view.CallerPaneID != "rogue" {
		t.Fatalf("request attributed to %q, want rogue", view.CallerPaneID)
	}
	// Its own child: still allowed, whatever the header says.
	if text, failed := mcpAs(t, a, rogue, "lead", "rename_pane", map[string]any{"pane_id": "kid", "title": "Kid"}); failed {
		t.Fatalf("rename own child: %q", text)
	}
	if text, failed := mcpAs(t, a, rogue, "lead", "set_pane_info", map[string]any{"pane_id": "kid", "working_on": "helping"}); failed {
		t.Fatalf("set_pane_info own child: %q", text)
	}
	if text, failed := mcpAs(t, a, rogue, "lead", "kill_pane", map[string]any{"pane_id": "kid"}); failed {
		t.Fatalf("kill own child: %q", text)
	}
	if _, ok := a.mgr.Get("kid"); ok {
		t.Fatal("own child still open")
	}
}

func TestMCPTracedCallerPane(t *testing.T) {
	a := traceTestServer(t)
	prevRelay := peerIsRelayProxy
	peerIsRelayProxy = func(pid int) bool { return pid == 1 }
	t.Cleanup(func() { peerIsRelayProxy = prevRelay })
	for _, tc := range []struct {
		name   string
		traced *callerIdentity
		header string
		want   string
	}{
		{"pane matches its header", &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "lead", "lead"},
		{"pane naming another pane", &callerIdentity{Kind: callerPane, PaneID: "rogue", PID: 7}, "lead", "rogue"},
		{"pane with no header", &callerIdentity{Kind: callerPane, PaneID: "rogue", PID: 7}, "", "rogue"},
		{"placed only by its environment: a claim", &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7, FromEnv: true}, "lead", mcpClaimedCallerPrefix + "lead"},
		{"environment claim ignores the header too", &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7, FromEnv: true}, "worker", mcpClaimedCallerPrefix + "lead"},
		{"worker owns no panes", &callerIdentity{Kind: callerWorker, PID: 7}, "lead", ""},
		{"traced program outside the panes", &callerIdentity{Kind: callerUnverified, PID: 7}, "lead", ""},
		{"relayed by WSL init: untraceable", &callerIdentity{Kind: callerUnverified, PID: 1}, "lead", "lead"},
		{"untraceable", nil, "lead", "lead"},
	} {
		prev := identifyCallerHook
		identifyCallerHook = func(*http.Request) callerIdentity {
			if tc.traced == nil {
				return callerIdentity{Kind: callerUnverified}
			}
			return *tc.traced
		}
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set("X-Soluterminal-Pane", tc.header)
		got := a.mcpTracedCallerPane(req)
		identifyCallerHook = prev
		if got != tc.want {
			t.Errorf("%s: caller = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A worker naming the lead can't close the lead's worker.
	if text, failed := mcpAs(t, a, &callerIdentity{Kind: callerWorker, PID: 7}, "lead", "kill_pane", map[string]any{"pane_id": "worker"}); !failed {
		t.Fatalf("worker closed a pane: %q", text)
	}
	// Untraceable: the header still counts (SECURITY.md's documented limit).
	if text, failed := mcpAs(t, a, nil, "lead", "kill_pane", map[string]any{"pane_id": "worker"}); failed {
		t.Fatalf("untraceable lead closing its worker: %q", text)
	}
}

// A caller placed only by SOLU_PANE_ID in its own environment (setsid, a
// detached tmux or screen server) owns no panes, not even the one it claims:
// it goes through the person's OK, and the prompt says it is unverified.
func TestMCPEnvOnlyCallerOwnsNothing(t *testing.T) {
	a := traceTestServer(t)
	claimsLead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, FromEnv: true}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"kill_pane", map[string]any{"pane_id": "worker"}},
		{"kill_pane", map[string]any{"pane_id": "lead"}},
		{"rename_pane", map[string]any{"pane_id": "lead", "title": "x"}},
		{"rename_pane", map[string]any{"pane_id": "worker", "title": "x"}},
		{"set_pane_info", map[string]any{"pane_id": "lead", "working_on": "x"}},
		{"set_pane_info", map[string]any{"pane_id": "worker", "working_on": "x"}},
	} {
		for _, header := range []string{"lead", ""} {
			if text, failed := mcpAs(t, a, claimsLead, header, tc.tool, tc.args); !failed || !strings.Contains(text, "was not started by you") {
				t.Fatalf("%s %v (header %q) = %q", tc.tool, tc.args, header, text)
			}
		}
	}
	// Even with Agent permissions on, its user_requested goes to the person.
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	if text, _ := mcpAs(t, a, claimsLead, "lead", "kill_pane", map[string]any{"pane_id": "worker", "user_requested": true}); !strings.Contains(text, "waiting for your OK in the app") {
		t.Fatalf("user_requested = %q", text)
	}
	view := onlyPendingConfirm(t, a)
	if view.CallerPaneID != "" || view.Message != "An agent claiming to be #1 Orchestrator (couldn't be verified) wants to close pane #2 “Worker”. Closing stops what is running in it; files are not deleted." {
		t.Fatalf("request = %+v", view)
	}
	if _, ok := a.mgr.Get("worker"); !ok {
		t.Fatal("closed without the person's OK")
	}
}

// The rename the person approves is exactly the text they were shown.
func TestRenamePaneAppliesTheTitleThePromptShowed(t *testing.T) {
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	a := confirmTestServer(t)
	raw := "Build\nlogs \u202eevil\x1b[31m " + strings.Repeat("x", 200)
	a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": raw, "user_requested": true}, "lead")
	view := onlyPendingConfirm(t, a)
	shown := strings.TrimSuffix(strings.TrimPrefix(view.Message, "#1 Orchestrator wants to rename pane #3 “Social loop” to “"), "”.")
	if strings.ContainsAny(shown, "\n\x1b\u202e") || len([]rune(shown)) > paneTitleMaxRunes {
		t.Fatalf("prompt shows %q", shown)
	}
	if _, ok := a.decidePaneConfirm(view.ID, true); !ok {
		t.Fatal("request gone")
	}
	social, _ := a.mgr.Get("social")
	if got := social.Snapshot().Title; got != shown {
		t.Fatalf("set %q, prompt showed %q", got, shown)
	}
	// An own-pane rename is normalised the same way.
	a.mcpToolRenamePane(map[string]any{"pane_id": "lead", "title": "two\nlines"}, "lead")
	if lead, _ := a.mgr.Get("lead"); lead.Snapshot().Title != "two lines" {
		t.Fatalf("own rename = %q", lead.Snapshot().Title)
	}
}

func TestPaneConfirmGlobalCapMessage(t *testing.T) {
	a := confirmTestServer(t)
	for i := 0; i < paneConfirmMaxOpen; i++ {
		if _, err := a.requestPaneConfirmFor(string(rune('a'+i)), string(rune('A'+i)), paneConfirmView{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.requestPaneConfirmFor("more", "someone-new", paneConfirmView{}, nil)
	if err == nil || err.Error() != "20 requests from agents are already waiting for the person's OK in the app; wait for them to be answered before asking again" {
		t.Fatalf("global cap = %v", err)
	}
}

// A request ends when the pane it is about, or the asking agent's pane, closes.
func TestPaneConfirmDroppedWhenAPaneCloses(t *testing.T) {
	shortenPaneConfirmWait(t, 20*time.Millisecond)
	a := confirmTestServer(t)
	a.mgr.SetOnPaneRemoved(a.dropPaneConfirmsFor)
	a.mcpToolRenamePane(map[string]any{"pane_id": "social", "title": "x", "user_requested": true}, "lead")
	target := onlyPendingConfirm(t, a)
	req := a.paneConfirms.byID[target.ID]
	if err := a.mgr.Kill("social"); err != nil {
		t.Fatal(err)
	}
	<-req.done
	if text, failed := mcpResultText(t, req.result); !failed || !strings.Contains(text, "closed, so the request was dropped") {
		t.Fatalf("dropped = %q", text)
	}
	// The asking agent's pane closes.
	a.mcpToolKillPane(map[string]any{"pane_id": "lead", "user_requested": true}, "worker")
	if len(a.pendingPaneConfirms()) != 1 {
		t.Fatal("no request")
	}
	if err := a.mgr.Kill("worker"); err != nil {
		t.Fatal(err)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("request outlived the asking agent's pane")
	}
}
