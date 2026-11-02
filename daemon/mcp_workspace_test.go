package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func workspaceToolFixture(t *testing.T) *apiServer {
	t.Helper()
	a := confirmTestServer(t)
	a.ws = paneMoveFixture(t)
	p := testPaneWithTail(t, "p", "Work pane", "")
	p.Status = PaneRunning
	p.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead"}
	a.mgr.panes[p.ID] = p
	for _, name := range []string{"Empty", "_current"} {
		if err := a.ws.Put(&Workspace{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	shortenPaneConfirmWait(t, time.Millisecond)
	return a
}

func workspaceToolCall(a *apiServer, tool string, args map[string]any, c paneCaller) any {
	if tool == "move_pane_to_workspace" {
		return a.mcpToolMovePaneToWorkspace(args, c)
	}
	return a.mcpToolChangeWorkspace(tool, args, c)
}

func TestMCPWorkspaceMoveOwnedPaneWithoutOK(t *testing.T) {
	for _, caller := range []string{"lead", "starter", "p"} {
		t.Run(caller, func(t *testing.T) {
			a := workspaceToolFixture(t)
			p, _ := a.mgr.Get("p")
			p.Origin.ByPaneID = "starter" // lead and creator are independently allowed
			before := p.Snapshot()
			text, failed := mcpResultText(t, a.mcpToolMovePaneToWorkspace(map[string]any{"pane_id": "p", "workspace": "b"}, paneCaller{ID: caller}))
			if failed {
				t.Fatal(text)
			}
			if len(a.pendingPaneConfirms()) != 0 || a.ws.NameForPane("p") != "B" {
				t.Fatal("owned move required OK or did not move")
			}
			after := p.Snapshot()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("move changed pane identity/process")
			}
			reloaded, err := NewWorkspaceStore(filepath.Dir(a.ws.path))
			if err != nil || reloaded.NameForPane("p") != "B" {
				t.Fatalf("move not durable: %v", err)
			}
			events := a.uiBus.log
			if len(events) != 1 || events[0].Type != "workspace.pane-moved" || events[0].Workspace != "A" || events[0].Text != "B" {
				t.Fatalf("clients not updated: %+v", events)
			}
		})
	}
}

func TestMCPWorkspaceUserPaneNeedsRequestAndHumanOK(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, grant := range []bool{false, true} {
			t.Run(workspaceBool(allow)+"/grant="+workspaceBool(grant), func(t *testing.T) {
				a := workspaceToolFixture(t)
				p, _ := a.mgr.Get("p")
				p.Origin = nil
				if grant {
					if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
						t.Fatal(err)
					}
				}
				args := map[string]any{"pane_id": "p", "workspace": "B"}
				if text, failed := mcpResultText(t, a.mcpToolMovePaneToWorkspace(args, paneCaller{ID: "lead"})); !failed || !strings.Contains(text, "user_requested") {
					t.Fatalf("unrequested move: %s", text)
				}
				if len(a.pendingPaneConfirms()) != 0 {
					t.Fatal("unrequested move prompted")
				}
				args["user_requested"] = true
				a.mcpToolMovePaneToWorkspace(args, paneCaller{ID: "lead"})
				view := onlyPendingConfirm(t, a)
				if view.Message != `Orchestrator wants to move pane #1 “Work pane” from workspace "A" to workspace "B". Its process keeps running. Choose Allow in the app.` {
					t.Fatal(view.Message)
				}
				if a.ws.NameForPane("p") != "A" {
					t.Fatal("moved before Allow")
				}
				if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerPane); rec.Code != 403 {
					t.Fatalf("agent answered despite grant: %d", rec.Code)
				}
				body := `{"allow":false}`
				if allow {
					body = `{"allow":true}`
				}
				rec := answerPaneConfirm(t, a, view.ID, body, callerHuman)
				if rec.Code != 200 {
					t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
				}
				want := "A"
				if allow {
					want = "B"
				}
				if a.ws.NameForPane("p") != want {
					t.Fatalf("move after allow=%v", allow)
				}
			})
		}
	}
}

func workspaceBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestMCPWorkspaceChangesAlwaysNeedHumanOK(t *testing.T) {
	for _, tool := range []string{"rename_workspace", "delete_workspace"} {
		for _, allow := range []bool{false, true} {
			t.Run(tool+"/allow="+workspaceBool(allow), func(t *testing.T) {
				a := workspaceToolFixture(t)
				if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"workspace": "empty", "new_name": "Renamed"}
				c := paneCaller{ID: "lead"}
				if text, failed := mcpResultText(t, workspaceToolCall(a, tool, args, c)); !failed || !strings.Contains(text, "user_requested") {
					t.Fatalf("unrequested: %s", text)
				}
				args["user_requested"] = true
				workspaceToolCall(a, tool, args, c)
				view := onlyPendingConfirm(t, a)
				expectedMessage := `Orchestrator wants to rename workspace "Empty" → "Renamed". Choose Allow in the app.`
				if tool == "delete_workspace" {
					expectedMessage = `Orchestrator wants to delete empty workspace "Empty". Its saved layout, browser/file tiles and context-pack default will be removed; files are not deleted. Choose Allow in the app.`
				}
				if view.Message != expectedMessage {
					t.Fatalf("approval names: %s", view.Message)
				}
				if _, ok := a.ws.Get("Empty"); !ok {
					t.Fatal("changed before Allow")
				}
				if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerPane); rec.Code != 403 {
					t.Fatal("agent allowed workspace change")
				}
				body := `{"allow":false}`
				if allow {
					body = `{"allow":true}`
				}
				rec := answerPaneConfirm(t, a, view.ID, body, callerHuman)
				if rec.Code != 200 || (allow && strings.Contains(rec.Body.String(), `"failed":true`)) {
					t.Fatalf("answer: %s", rec.Body.String())
				}
				_, exists := a.ws.Get("Empty")
				if exists == allow {
					t.Fatal("wrong final existence")
				}
				if tool == "rename_workspace" && allow {
					if _, ok := a.ws.Get("Renamed"); !ok {
						t.Fatal("not renamed")
					}
				}
				if allow {
					events := a.uiBus.log
					expected := "workspace.deleted"
					if tool == "rename_workspace" {
						expected = "workspace.renamed"
					}
					if events[len(events)-2].Type != expected {
						t.Fatalf("missing event: %+v", events)
					}
				}
			})
		}
	}
}

func TestMCPWorkspaceInvalidOperationsNeverPrompt(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"rename_workspace", map[string]any{"workspace": " ", "new_name": "New"}, "workspace is required"},
		{"rename_workspace", map[string]any{"workspace": "Empty", "new_name": " "}, "new_name"},
		{"rename_workspace", map[string]any{"workspace": "Empty", "new_name": "Bad\u202eName"}, "new_name"},
		{"delete_workspace", map[string]any{"workspace": ""}, "workspace is required"},
		{"move_pane_to_workspace", map[string]any{"pane_id": "p", "workspace": ""}, "workspace is required"},
		{"rename_workspace", map[string]any{"workspace": "A", "new_name": "b"}, "already exists"},
		{"delete_workspace", map[string]any{"workspace": "A"}, "move or close its panes first"},
		{"delete_workspace", map[string]any{"workspace": "B"}, "protected"},
		{"rename_workspace", map[string]any{"workspace": "_CURRENT", "new_name": "Other"}, "_current"},
		{"rename_workspace", map[string]any{"workspace": "Empty", "new_name": "_CURRENT"}, "_current"},
		{"delete_workspace", map[string]any{"workspace": "_current"}, "_current"},
		{"move_pane_to_workspace", map[string]any{"pane_id": "p", "workspace": "_current"}, "_current"},
		{"move_pane_to_workspace", map[string]any{"pane_id": "p", "workspace": "Missing"}, "no workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+tc.want, func(t *testing.T) {
			a := workspaceToolFixture(t)
			tc.args["user_requested"] = true
			before := a.ws.List()
			text, failed := mcpResultText(t, workspaceToolCall(a, tc.tool, tc.args, paneCaller{ID: "lead"}))
			if !failed || !strings.Contains(text, tc.want) {
				t.Fatalf("result: %s", text)
			}
			if len(a.pendingPaneConfirms()) != 0 {
				t.Fatal("invalid operation prompted")
			}
			for _, old := range before {
				got, _ := a.ws.Get(old.Name)
				if !reflect.DeepEqual(got, old) {
					t.Fatal("invalid operation mutated workspace")
				}
			}
		})
	}
}

func TestMCPWorkspaceRevalidatesAfterAllow(t *testing.T) {
	for _, tool := range []string{"delete_workspace", "rename_workspace", "move_pane_to_workspace"} {
		t.Run(tool, func(t *testing.T) {
			a := workspaceToolFixture(t)
			p, _ := a.mgr.Get("p")
			p.Origin = nil
			args := map[string]any{"workspace": "Empty", "new_name": "New", "pane_id": "p", "user_requested": true}
			workspaceToolCall(a, tool, args, paneCaller{ID: "lead"})
			view := onlyPendingConfirm(t, a)
			switch tool {
			case "delete_workspace":
				_, err := a.ws.AddPane("Empty", PaneDef{ID: "new"}, "horizontal", "")
				if err != nil {
					t.Fatal(err)
				}
			case "rename_workspace":
				if err := a.ws.Ensure("new"); err != nil {
					t.Fatal(err)
				}
			case "move_pane_to_workspace":
				if _, _, err := a.ws.MovePane("A", "B", "p", "horizontal", "", 0); err != nil {
					t.Fatal(err)
				}
			}
			rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerHuman)
			if !strings.Contains(rec.Body.String(), `"failed":true`) {
				t.Fatalf("stale request applied: %s", rec.Body.String())
			}
		})
	}
}

func TestMCPWorkspaceConfirmsShareCallerLimitAndRejectClaims(t *testing.T) {
	a := workspaceToolFixture(t)
	for i := 0; i < paneConfirmMaxPerCaller; i++ {
		name := []string{"One", "Two", "Three"}[i]
		a.mcpToolChangeWorkspace("rename_workspace", map[string]any{"workspace": "Empty", "new_name": name, "user_requested": true}, paneCaller{ID: "lead"})
	}
	text, failed := mcpResultText(t, a.mcpToolChangeWorkspace("delete_workspace", map[string]any{"workspace": "Empty", "user_requested": true}, paneCaller{ID: "lead"}))
	if !failed || !strings.Contains(text, "already waiting") {
		t.Fatal(text)
	}
	// A claimed lead never owns the worker.
	text, failed = mcpResultText(t, a.mcpToolMovePaneToWorkspace(map[string]any{"pane_id": "p", "workspace": "B"}, paneCaller{Claimed: "lead"}))
	if !failed || !strings.Contains(text, "user_requested") {
		t.Fatal(text)
	}
}

func TestMCPWorkspaceMoveClaimedOwnerNeedsAllow(t *testing.T) {
	for _, claim := range []string{"p", "starter", "lead"} {
		t.Run(claim, func(t *testing.T) {
			a := workspaceToolFixture(t)
			p, _ := a.mgr.Get("p")
			p.Origin.ByPaneID = "starter"
			if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"pane_id": "p", "workspace": "B"}
			call := func() (string, bool) {
				t.Helper()
				params, err := json.Marshal(toolCallParams{Name: "move_pane_to_workspace", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				return mcpResultText(t, a.mcpCallTool(params, nil, mcpClaimedCallerPrefix+claim))
			}
			if text, failed := call(); !failed || !strings.Contains(text, "user_requested") {
				t.Fatalf("claimed owner moved without request: %s", text)
			}
			if len(a.pendingPaneConfirms()) != 0 || a.ws.NameForPane("p") != "A" {
				t.Fatal("unrequested move prompted or changed workspace")
			}
			args["user_requested"] = true
			call()
			view := onlyPendingConfirm(t, a)
			if view.CallerPaneID != "" || !strings.Contains(view.Caller, "unverified") || a.ws.NameForPane("p") != "A" {
				t.Fatalf("claim acquired ownership before Allow: %+v", view)
			}
			rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerHuman)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"failed":true`) || a.ws.NameForPane("p") != "B" {
				t.Fatalf("human Allow did not move pane: %s", rec.Body.String())
			}
		})
	}
}

func TestMCPWorkspaceToolsRespectMutationPolicyAndScope(t *testing.T) {
	a := workspaceToolFixture(t)
	t.Setenv(mcpMutationPolicyEnv, "deny")
	for _, tool := range []string{"move_pane_to_workspace", "rename_workspace", "delete_workspace"} {
		b, _ := json.Marshal(toolCallParams{Name: tool, Arguments: map[string]any{"workspace": "Empty", "pane_id": "p", "new_name": "New", "user_requested": true}})
		text, failed := mcpResultText(t, a.mcpCallTool(b, nil, "lead"))
		if !failed || !strings.Contains(text, "disabled") {
			t.Fatal(text)
		}
		text, failed = mcpResultText(t, scopedCallForTest(t, a, &mcpScope{}, tool, map[string]any{}))
		if !failed || !strings.Contains(text, "not in this session's scope") {
			t.Fatal(text)
		}
	}
	if len(a.pendingPaneConfirms()) != 0 || a.ws.NameForPane("p") != "A" {
		t.Fatal("denied dispatch changed state")
	}
}

func TestWorkspaceUIRenameAndDeleteSharePersistenceAndEvents(t *testing.T) {
	a := workspaceToolFixture(t)
	r := httptest.NewRequest("POST", "/api/workspaces/Empty/rename", strings.NewReader(`{"newName":"NEW"}`))
	r.SetPathValue("name", "Empty")
	w := httptest.NewRecorder()
	a.renameWorkspace(w, r)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	r = httptest.NewRequest("POST", "/api/workspaces/NEW/rename", strings.NewReader(`{"newName":"a"}`))
	r.SetPathValue("name", "NEW")
	w = httptest.NewRecorder()
	a.renameWorkspace(w, r)
	if w.Code != 400 {
		t.Fatal("UI case-insensitive collision accepted")
	}
	// UI deletion retains its existing non-empty detach semantics.
	r = httptest.NewRequest("DELETE", "/api/workspaces/A", nil)
	r.SetPathValue("name", "A")
	w = httptest.NewRecorder()
	a.deleteWorkspace(w, r)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if p, ok := a.mgr.Get("p"); !ok || p.Status != PaneRunning {
		t.Fatal("UI delete killed a pane")
	}
	if a.uiBus.log[0].Type != "workspace.renamed" || a.uiBus.log[1].Type != "workspace.deleted" {
		t.Fatalf("UI events: %+v", a.uiBus.log)
	}
}

// Only a verified person can answer, including through the trusted relay.
// Pane and worker callers remain refused even with standing Agent permissions.
func TestMCPWorkspaceRelayAnswersRequireHuman(t *testing.T) {
	for _, tool := range []string{"move_pane_to_workspace", "rename_workspace", "delete_workspace"} {
		t.Run(tool, func(t *testing.T) {
			a := workspaceToolFixture(t)
			p, _ := a.mgr.Get("p")
			p.Origin = nil
			if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"workspace": "Empty", "new_name": "Renamed", "pane_id": "p", "user_requested": true}
			workspaceToolCall(a, tool, args, paneCaller{ID: "lead"})
			view := onlyPendingConfirm(t, a)
			before := a.ws.List()
			previous := identifyCallerHook
			t.Cleanup(func() { identifyCallerHook = previous })
			for _, kind := range []callerKind{callerPane, callerWorker, callerUnverified} {
				identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: kind, PaneID: "lead", Relay: true} }
				mux := http.NewServeMux()
				mux.HandleFunc("POST /api/pane-confirmations/{id}", a.handleAnswerPaneConfirm)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/api/pane-confirmations/"+view.ID, strings.NewReader(`{"allow":true}`))
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s relay Allow = %d %s", kind, rec.Code, rec.Body.String())
				}
				if len(a.pendingPaneConfirms()) != 1 {
					t.Fatal("non-person consumed request")
				}
				for _, old := range before {
					got, _ := a.ws.Get(old.Name)
					if !reflect.DeepEqual(got, old) {
						t.Fatal("non-person relay approval changed workspace")
					}
				}
			}
			identifyCallerHook = nil
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/pane-confirmations/{id}", a.handleAnswerPaneConfirm)
			// Self-declared relay headers are not a verified person.
			spoof := httptest.NewRequest("POST", "/api/pane-confirmations/"+view.ID, strings.NewReader(`{"allow":true}`))
			spoof.Header.Set("X-Solu-Relay", "1")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, spoof)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("spoofed relay Allow = %d %s", rec.Code, rec.Body.String())
			}
			// The app's authenticated relay adapter supplies this private context
			// marker; the production classifier recognizes the person on it.
			human := httptest.NewRequest("POST", "/api/pane-confirmations/"+view.ID, strings.NewReader(`{"allow":true}`))
			human = human.WithContext(context.WithValue(human.Context(), relayTransportKey{}, true))
			if caller := a.identifyCaller(human); caller.Kind != callerHuman || !caller.Relay {
				t.Fatalf("trusted relay caller = %+v", caller)
			}
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, human)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"failed":true`) {
				t.Fatalf("trusted relay Allow = %d %s", rec.Code, rec.Body.String())
			}
			if len(a.pendingPaneConfirms()) != 0 {
				t.Fatal("human relay did not consume the request")
			}

			switch tool {
			case "move_pane_to_workspace":
				if a.ws.NameForPane("p") != "Empty" {
					t.Fatal("person relay move not applied")
				}
			case "rename_workspace":
				if _, ok := a.ws.Get("Renamed"); !ok {
					t.Fatal("person relay rename not applied")
				}
			case "delete_workspace":
				if _, ok := a.ws.Get("Empty"); ok {
					t.Fatal("person relay delete not applied")
				}
			}
		})
	}
}

func TestMCPWorkspaceAllToolsShareExistingThreeRequestLimit(t *testing.T) {
	for _, tool := range []string{"move_pane_to_workspace", "rename_workspace", "delete_workspace"} {
		t.Run(tool, func(t *testing.T) {
			a := workspaceToolFixture(t)
			p, _ := a.mgr.Get("p")
			p.Origin = nil
			if paneConfirmMaxPerCaller != 3 {
				t.Fatal("review contract: three pending per agent")
			}
			for _, key := range []string{"legacy-one", "legacy-two"} {
				_, err := a.requestPaneConfirmFor(key, "lead", paneConfirmView{Action: paneConfirmClose, CallerPaneID: "lead", PaneID: "social"}, func() map[string]any { return toolOK("legacy action") })
				if err != nil {
					t.Fatal(err)
				}
			}
			workspaceToolCall(a, tool, map[string]any{"workspace": "Empty", "new_name": "Renamed", "pane_id": "p", "user_requested": true}, paneCaller{ID: "lead"})
			if len(a.pendingPaneConfirms()) != 3 {
				t.Fatal("workspace request did not share the existing queue")
			}
			_, err := a.requestPaneConfirmFor("fourth-legacy", "lead", paneConfirmView{CallerPaneID: "lead"}, func() map[string]any { return toolOK("unexpected") })
			if err == nil || !strings.Contains(err.Error(), "3 of your requests") {
				t.Fatalf("fourth request = %v", err)
			}
			t.Cleanup(func() {
				for _, pending := range a.pendingPaneConfirms() {
					a.decidePaneConfirm(pending.ID, false)
				}
			})
		})
	}
}

func TestMCPWorkspaceMoveRefusesHiddenSource(t *testing.T) {
	a := workspaceToolFixture(t)
	if err := a.ws.Delete("A"); err != nil {
		t.Fatal(err)
	}
	if err := a.ws.Put(&Workspace{Name: "_current", PaneIds: []string{"p"}, Panes: []PaneDef{{ID: "p"}}, Layout: LayoutNode{"type": "pane", "paneId": "p"}}); err != nil {
		t.Fatal(err)
	}
	text, failed := mcpResultText(t, a.mcpToolMovePaneToWorkspace(map[string]any{"pane_id": "p", "workspace": "B", "user_requested": true}, paneCaller{ID: "lead"}))
	if !failed || !strings.Contains(text, "visible named workspace") {
		t.Fatalf("hidden source = %s", text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("hidden source prompted")
	}
	scratch, _ := a.ws.Get("_current")
	destination, _ := a.ws.Get("B")
	if !reflect.DeepEqual(scratch.PaneIds, []string{"p"}) || len(destination.PaneIds) != 0 {
		t.Fatal("hidden source moved")
	}
}

// Either the delete wins and the move is refused, or the move wins and the
// delete is refused. No interleaving may detach a pane that just moved in.
func TestMCPWorkspaceDeleteEmptyConcurrentMoveNeverDetachesPane(t *testing.T) {
	for attempt := 0; attempt < 32; attempt++ {
		store := paneMoveFixture(t)
		if err := store.Ensure("Empty"); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		moveResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() {
			<-start
			_, _, err := store.MovePane("A", "Empty", "p", "horizontal", "", 0)
			moveResult <- err
		}()
		go func() { <-start; deleteResult <- store.DeleteEmpty("Empty") }()
		close(start)
		moveErr, deleteErr := <-moveResult, <-deleteResult
		if (moveErr == nil) == (deleteErr == nil) {
			t.Fatalf("exactly one operation must win: move=%v delete=%v", moveErr, deleteErr)
		}
		reloaded, err := NewWorkspaceStore(filepath.Dir(store.path))
		if err != nil {
			t.Fatal(err)
		}
		expected := "A"
		if moveErr == nil {
			expected = "Empty"
			if !strings.Contains(deleteErr.Error(), "move or close its panes first") {
				t.Fatal(deleteErr)
			}
		}
		if store.NameForPane("p") != expected || reloaded.NameForPane("p") != expected {
			t.Fatalf("concurrent deletion detached p; want %s", expected)
		}
	}
}

func TestMCPWorkspacePromotedUserPaneDoesNotBecomeAgentOwned(t *testing.T) {
	a := workspaceToolFixture(t)
	p, _ := a.mgr.Get("p")
	// A lead role and even stale linkage never override the recorded user origin.
	p.Origin = &PaneOrigin{Source: paneOriginUser, TeamKind: "lead", LeadRole: "operations", ByPaneID: "lead", LeadPaneID: "lead"}
	args := map[string]any{"pane_id": "p", "workspace": "B", "caller_pane_id": "lead", "lead_pane_id": "lead", "origin": map[string]any{"source": "agent", "byPaneId": "lead"}}
	for _, caller := range []paneCaller{{ID: "lead"}, {Claimed: "lead"}, {}} {
		text, failed := mcpResultText(t, a.mcpToolMovePaneToWorkspace(args, caller))
		if !failed || !strings.Contains(text, "user_requested") {
			t.Fatalf("promoted user pane/argument claim gained ownership: %s", text)
		}
	}
	args["user_requested"] = true
	a.mcpToolMovePaneToWorkspace(args, paneCaller{ID: "lead"})
	view := onlyPendingConfirm(t, a)
	if a.ws.NameForPane("p") != "A" {
		t.Fatal("promoted user pane moved before Allow")
	}
	answerPaneConfirm(t, a, view.ID, `{"allow":false}`, callerHuman)
	if a.ws.NameForPane("p") != "A" {
		t.Fatal("promoted user pane moved despite Deny")
	}
}

func TestMCPWorkspaceConfirmationNamesHaveNoControlOrBidiCharacters(t *testing.T) {
	a := workspaceToolFixture(t)
	p, _ := a.mgr.Get("p")
	p.Origin = nil
	p.Title = "Work\n\u202e\x1b pane"
	caller, _ := a.mgr.Get("lead")
	caller.Title = "Caller\n\u202e\x1b name"
	a.mcpToolMovePaneToWorkspace(map[string]any{"pane_id": "p", "workspace": "B", "user_requested": true}, paneCaller{ID: "lead"})
	view := onlyPendingConfirm(t, a)
	for _, text := range []string{view.Title, view.Message, view.Pane, view.Caller} {
		if text != printableText(text) || strings.ContainsAny(text, "\n\r\t") {
			t.Fatalf("unsafe confirmation text: %q", text)
		}
	}
	if !strings.Contains(view.Message, `from workspace "A" to workspace "B"`) {
		t.Fatal(view.Message)
	}
	answerPaneConfirm(t, a, view.ID, `{"allow":false}`, callerHuman)
	for _, tool := range []string{"move_pane_to_workspace", "rename_workspace", "delete_workspace"} {
		unsafe := "Unsafe\u202eName"
		if err := a.ws.Put(&Workspace{Name: unsafe}); err != nil {
			t.Fatal(err)
		}
		text, failed := mcpResultText(t, workspaceToolCall(a, tool, map[string]any{"pane_id": "p", "workspace": unsafe, "new_name": "New", "user_requested": true}, paneCaller{ID: "lead"}))
		if !failed || !strings.Contains(text, "control or bidi") {
			t.Fatalf("unsafe name in %s: %s", tool, text)
		}
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("unsafe names raised a prompt")
	}
}

func TestMCPWorkspaceMoveWithoutVisibleSourceIsRefused(t *testing.T) {
	a := workspaceToolFixture(t)
	if err := a.ws.Delete("A"); err != nil {
		t.Fatal(err)
	}
	text, failed := mcpResultText(t, a.mcpToolMovePaneToWorkspace(map[string]any{"pane_id": "p", "workspace": "B", "user_requested": true}, paneCaller{ID: "lead"}))
	if !failed || !strings.Contains(text, "visible named workspace") {
		t.Fatal(text)
	}
	if len(a.pendingPaneConfirms()) != 0 {
		t.Fatal("unassigned pane raised a move request")
	}
	b, _ := a.ws.Get("B")
	if len(b.PaneIds) != 0 {
		t.Fatal("unassigned pane moved")
	}
}
