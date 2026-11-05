package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// A valid owner token authenticates a connection but never makes a pane,
// worker or script a person. Exercise real classification and route dispatch.
func TestPaneWorkspaceRESTCallerAuthority(t *testing.T) {
	for _, route := range []struct {
		name, pattern, path, body, tool string
		handler                         func(*apiServer) http.HandlerFunc
		status                          int
	}{
		{"migrate in place", "POST /api/panes/{id}/migrate-clean", "/api/panes/social/migrate-clean", `{"mode":"inplace","confirm":true}`, "", func(a *apiServer) http.HandlerFunc { return a.migratePaneClean }, 200},
		{"migrate all", "POST /api/panes/migrate-clean-all", "/api/panes/migrate-clean-all", `{}`, "", func(a *apiServer) http.HandlerFunc { return a.migrateCleanAll }, 200},
		{"workspace context pack", "POST /api/workspaces/{name}/context-pack", "/api/workspaces/w/context-pack", `{"contextPack":""}`, "", func(a *apiServer) http.HandlerFunc { return a.setWorkspaceContextPack }, 200},
		{"pane context pack", "POST /api/panes/{id}/context-pack", "/api/panes/social/context-pack", `{"contextPack":""}`, "", func(a *apiServer) http.HandlerFunc { return a.handleSetContextPack }, 200},
		// Existing broadcast tests exercise successful human delivery. Invalid
		// control input must still reach validation for human UI callers only.
		{"submit message", "POST /api/panes/{id}/submit-message", "/api/panes/social/submit-message", `{"text":"hello\u0003"}`, "ask_pane", func(a *apiServer) http.HandlerFunc { return a.submitPaneMessage }, 400},
		{"close pane", "DELETE /api/panes/{id}", "/api/panes/social", "", "kill_pane", func(a *apiServer) http.HandlerFunc { return a.deletePane }, 204},
		{"rename pane", "POST /api/panes/{id}/rename", "/api/panes/social/rename", `{"title":"Renamed"}`, "rename_pane", func(a *apiServer) http.HandlerFunc { return a.renamePane }, 200},
		{"pane title", "POST /api/panes/{id}/title", "/api/panes/social/title", `{"title":"Override"}`, "rename_pane", func(a *apiServer) http.HandlerFunc { return a.setPaneTitle }, 200},
		{"generate title", "POST /api/panes/{id}/title/generate", "/api/panes/social/title/generate", `{}`, "rename_pane", func(a *apiServer) http.HandlerFunc { return a.generatePaneTitle }, 200},
		{"pane flag", "POST /api/panes/{id}/flag", "/api/panes/social/flag", `{"flagged":true,"memo":"Later"}`, "set_pane_flag", func(a *apiServer) http.HandlerFunc { return a.setPaneFlag }, 200},
		{"pane identity", "POST /api/panes/{id}/identity", "/api/panes/social/identity", `{"handle":"changed","workingOn":"New work"}`, "set_pane_info", func(a *apiServer) http.HandlerFunc { return a.setPaneIdentity }, 200},
		{"create workspace", "POST /api/workspaces", "/api/workspaces", `{"name":"New","paneIds":[]}`, "create_workspace", func(a *apiServer) http.HandlerFunc { return a.putWorkspace }, 200},
		{"save workspace", "POST /api/workspaces", "/api/workspaces", `{"name":"w","paneIds":["lead","worker","social"],"layout":{"type":"pane","paneId":"social"}}`, "create_workspace", func(a *apiServer) http.HandlerFunc { return a.putWorkspace }, 200},
		{"move pane", "POST /api/workspaces/{name}/move-pane", "/api/workspaces/w/move-pane", `{"paneId":"social","destination":"Destination"}`, "move_pane_to_workspace", func(a *apiServer) http.HandlerFunc { return a.moveWorkspacePane }, 200},
		{"rename workspace", "POST /api/workspaces/{name}/rename", "/api/workspaces/w/rename", `{"newName":"Renamed"}`, "rename_workspace", func(a *apiServer) http.HandlerFunc { return a.renameWorkspace }, 204},
		{"delete workspace", "DELETE /api/workspaces/{name}", "/api/workspaces/w", "", "delete_workspace", func(a *apiServer) http.HandlerFunc { return a.deleteWorkspace }, 204},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, caller := range []string{"web", "desktop origin", "relay human", "pane", "delegated pane", "worker", "unverified", "unverified browser headers", "unverified relay header"} {
				t.Run(caller, func(t *testing.T) {
					a := confirmTestServer(t)
					prev := identifyCallerHook
					identifyCallerHook = nil
					t.Cleanup(func() { identifyCallerHook = prev })
					stubPeerPID(t, 0, false)
					p, _ := a.mgr.Get("social")
					p.Status = PaneRunning
					p.Cmd = []string{"claude"}
					a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
						replacement := switchTestPane("replacement", "", opts.Cmd, opts.ClaudeSessionID)
						a.mgr.panes[replacement.ID] = replacement
						return replacement, nil
					}
					transcript := filepath.Join(t.TempDir(), "title.jsonl")
					if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"role":"user","content":"A test title"}}`+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					p.ClaudeTranscriptPath = transcript
					oldRunner := titleRunner
					titleRunner = func(context.Context, string) (string, error) { return "Generated title", nil }
					t.Cleanup(func() { titleRunner = oldRunner })
					if err := a.ws.Put(&Workspace{Name: "Destination", PaneIds: []string{}}); err != nil {
						t.Fatal(err)
					}
					source, _ := a.ws.Get("w")
					source.Layout = LayoutNode{"type": "pane", "paneId": "social"}
					source.Panes = []PaneDef{{ID: "social", Cmd: []string{"bash"}}}
					if err := a.ws.Put(source); err != nil {
						t.Fatal(err)
					}
					before := a.ws.List()
					sort.Slice(before, func(i, j int) bool { return before[i].Name < before[j].Name })
					beforePane := p.Snapshot()
					method, _, _ := strings.Cut(route.pattern, " ")
					req := httptest.NewRequest(method, "http://localhost"+route.path, strings.NewReader(route.body))
					req.RemoteAddr = "127.0.0.1:12345"
					req.Header.Set("Authorization", "Bearer owner-token")
					wantKind := callerUnverified
					switch caller {
					case "web":
						req.Header.Set("Sec-Fetch-Site", "same-origin")
						wantKind = callerHuman
					case "desktop origin":
						req.Header.Set("Origin", "http://localhost")
						wantKind = callerHuman
					case "relay human":
						wantKind = callerHuman
					case "pane", "delegated pane":
						// A detached pane job: re-parented away from the
						// daemon, placed by its SOLU_PANE_ID.
						stubPeerPID(t, startDetachedSleeper(t, "SOLU_PANE_ID=lead"), true)
						req.Header.Set("Sec-Fetch-Site", "same-origin")
						wantKind = callerPane
						if caller == "delegated pane" {
							if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true, UpdatedAt: time.Now()}); err != nil {
								t.Fatal(err)
							}
						}
					case "worker":
						stubPeerPID(t, os.Getpid(), true)
						req.Header.Set("Sec-Fetch-Site", "same-origin")
						wantKind = callerWorker
					case "unverified browser headers":
						// Linux and Windows: a traced non-browser is unverified.
						useTracedOutsiderRule(t, false)
						stubPeerPID(t, 2147483647, true)
						req.Header.Set("Sec-Fetch-Site", "same-origin")
						oldBrowser, oldProxy := peerIsBrowser, peerIsRelayProxy
						peerIsBrowser = func(int) bool { return false }
						peerIsRelayProxy = func(int) bool { return false }
						t.Cleanup(func() { peerIsBrowser, peerIsRelayProxy = oldBrowser, oldProxy })
					case "unverified relay header":
						req.Header.Set("X-Forwarded-Prefix", "/m/test")
					}
					mux := http.NewServeMux()
					mux.HandleFunc(route.pattern, route.handler(a))
					var handler http.Handler = (&authGate{token: "owner-token"}).middleware(mux)
					if caller == "relay human" {
						handler = markRelayTransport(handler)
					}
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)
					if wantKind == callerHuman {
						if rec.Code != route.status {
							t.Fatalf("human: %d %s", rec.Code, rec.Body.String())
						}
						return
					}
					var denied struct{ Error, Caller, Message string }
					if rec.Code != http.StatusForbidden || json.Unmarshal(rec.Body.Bytes(), &denied) != nil || denied.Error != "human_required" || denied.Caller != string(wantKind) || !strings.Contains(denied.Message, route.tool) {
						t.Fatalf("want refusal for %s naming %s; got %d %s", wantKind, route.tool, rec.Code, rec.Body.String())
					}
					after := a.ws.List()
					sort.Slice(after, func(i, j int) bool { return after[i].Name < after[j].Name })
					got, exists := a.mgr.Get("social")
					if !exists || got != p || !reflect.DeepEqual(p.Snapshot(), beforePane) || !reflect.DeepEqual(after, before) {
						t.Fatal("refused caller changed pane or workspace")
					}
					if len(a.pendingPaneConfirms()) != 0 {
						t.Fatal("REST must direct to MCP without creating another confirmation path")
					}
				})
			}
		})
	}
}

func TestWorkspaceAutosaveRefusalLogPerCaller(t *testing.T) {
	a := confirmTestServer(t)
	previous := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = previous })
	var output bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(writer) })
	for _, pane := range []string{"lead", "lead", "worker", "worker"} {
		identifyCallerHook = func(*http.Request) callerIdentity {
			return callerIdentity{Kind: callerPane, PaneID: pane, PID: 123}
		}
		rec := httptest.NewRecorder()
		a.putWorkspace(rec, httptest.NewRequest("POST", "/api/workspaces", strings.NewReader(`{"name":"w"}`)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Fatalf("autosave response: %d %s", rec.Code, rec.Body.String())
		}
	}
	if got := strings.Count(output.String(), "authority: refused create or save a workspace"); got != 2 {
		t.Fatalf("want one log per caller; got %d: %s", got, output.String())
	}
	limiter := workspaceSaveLogLimiter{}
	now := time.Now()
	id := callerIdentity{Kind: callerPane, PaneID: "lead", PID: 123}
	if !limiter.allow(id, now) || limiter.allow(id, now.Add(time.Second)) {
		t.Fatal("initial call should log and repeat should be suppressed")
	}
	id.PID++ // Child processes within the same pane share its log budget.
	if limiter.allow(id, now.Add(59*time.Second)) || !limiter.allow(id, now.Add(time.Minute)) {
		t.Fatal("pane log budget should reset after one minute")
	}
}
