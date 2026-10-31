package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceStaleSavePreservesUnseenPanesAfterRestart(t *testing.T) {
	// Reproduce a window switching away long after an agent added a pane.
	// Its snapshot knows p1, but has never seen p2; a fresh daemon has no TTL map.
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed-set", true: "legacy-client"}[legacy], func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewWorkspaceStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Put(&Workspace{Name: "Desk", PaneIds: []string{"p1"}, Panes: []PaneDef{{ID: "p1"}}, Layout: LayoutNode{"type": "pane", "paneId": "p1"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AddPane("Desk", PaneDef{ID: "p2", Title: "worker", Origin: &PaneOrigin{Source: "agent", ByPaneID: "p1"}}, "horizontal", "p1"); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			store.recentAdds["Desk"]["p2"] = time.Now().Add(-time.Hour)
			store.mu.Unlock()
			store, err = NewWorkspaceStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			api := &apiServer{ws: store}
			body := `{"name":"Desk","layout":{"type":"pane","paneId":"p1"},"paneIds":["p1"],"panes":[{"id":"p1"}]`
			if !legacy {
				body += `,"knownPaneIds":["p1"]`
			}
			response := httptest.NewRecorder()
			api.putWorkspace(response, httptest.NewRequest("POST", "/api/workspaces", strings.NewReader(body+"}")))
			if response.Code != 200 {
				t.Fatalf("stale save: %d %s", response.Code, response.Body.String())
			}
			requireMCPPaneInWorkspace(t, store, "Desk", "p2")
			if strings.Contains(response.Body.String(), "knownPaneIds") {
				t.Fatal("observation leaked into response")
			}
			disk, err := os.ReadFile(store.path)
			if err != nil || strings.Contains(string(disk), "knownPaneIds") {
				t.Fatalf("observation persisted: %s, %v", disk, err)
			}
			// A view that HAS observed p2 may deliberately remove it immediately.
			known := []string{"p1", "p2"}
			next := &Workspace{Name: "Desk", Layout: LayoutNode{"type": "pane", "paneId": "p1"}, PaneIds: []string{"p1"}, Panes: []PaneDef{{ID: "p1"}}, KnownPaneIDs: &known}
			if err := store.Put(next); err != nil {
				t.Fatal(err)
			}
			got, _ := store.Get("Desk")
			if mcpWorkspaceHasPaneID(got.PaneIds, "p2") || mcpLayoutHasPane(got.Layout, "p2") {
				t.Fatal("explicit removal was undone")
			}
		})
	}
}

func TestWorkspaceUnseenMergeKeepsLocalLayoutAndOtherTiles(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(0)
	original := &Workspace{Name: "Desk", LayoutMode: "free", LayoutBaseVersion: &base, PaneIds: []string{"p1", "p2"}, Panes: []PaneDef{{ID: "p1"}, {ID: "p2"}}}
	original.Layout = insertPaneIntoLayout(LayoutNode{"type": "pane", "paneId": "p1"}, "p2", "horizontal", "")
	if err := store.Put(original); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPane("Desk", PaneDef{ID: "worker", Origin: &PaneOrigin{Source: "agent", ByPaneID: "p1"}}, "horizontal", "p1"); err != nil {
		t.Fatal(err)
	}
	known := []string{"p1", "p2"}
	// Locally removed p2 and added a browser before the stale save.
	layout := LayoutNode{"type": "split", "direction": "vertical", "ratio": 0.3, "a": LayoutNode{"type": "pane", "paneId": "p1"}, "b": LayoutNode{"type": "browser", "browserId": "browser"}}
	next := &Workspace{Name: "Desk", LayoutMode: "free", LayoutBaseVersion: &base, KnownPaneIDs: &known, PaneIds: []string{"p1"}, Panes: []PaneDef{{ID: "p1"}}, Layout: layout, Browsers: []BrowserInfoDef{{ID: "browser", URL: "https://example.com"}}}
	if err := store.Put(next); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("Desk")
	if got.Layout["ratio"] != 0.3 || got.Layout["direction"] != "vertical" || len(got.Browsers) != 1 {
		t.Fatalf("local edits lost: %+v", got)
	}
	if mcpWorkspaceHasPaneID(got.PaneIds, "p2") {
		t.Fatal("observed removal resurrected")
	}
	requireMCPPaneInWorkspace(t, store, "Desk", "worker")
}

func TestWorkspaceStaleSaveCannotRestoreKilledPane(t *testing.T) {
	for _, name := range []string{"Desk", "_current"} {
		for _, legacy := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/observed", true: "/legacy"}[legacy], func(t *testing.T) {
				a := confirmTestServer(t)
				worker, _ := a.mgr.Get("worker")
				layout := insertPaneIntoLayout(LayoutNode{"type": "pane", "paneId": "lead"}, "worker", "horizontal", "lead")
				stale := &Workspace{Name: name, Layout: layout, PaneIds: []string{"lead", "worker"}, Panes: []PaneDef{{ID: "lead"}, {ID: "worker", Cmd: []string{"bash"}, Origin: worker.Origin}}}
				if err := a.ws.Put(stale); err != nil {
					t.Fatal(err)
				}
				known := []string{"lead", "worker"}
				if !legacy {
					stale.KnownPaneIDs = &known
				}
				if text, failed := mcpResultText(t, a.mcpToolKillPane(map[string]any{"pane_id": "worker"}, "lead")); failed {
					t.Fatalf("starter closing worker: %s", text)
				}
				if _, ok := a.mgr.Get("worker"); ok {
					t.Fatal("killed worker still live")
				}
				// Tombstones must survive restart and repeated stale saves, whether
				// the window still has a launch recipe or only a missing-pane leaf.
				store, err := NewWorkspaceStore(filepath.Dir(a.ws.path))
				if err != nil {
					t.Fatal(err)
				}
				for _, withDef := range []bool{true, false} {
					request := cloneWorkspace(stale)
					if !withDef {
						request.Panes = nil
					}
					// A client cannot erase the server's removal record.
					request.RemovedPaneIDs = nil
					if err := store.Put(request); err != nil {
						t.Fatal(err)
					}
					for _, got := range []*Workspace{request, func() *Workspace { w, _ := store.Get(name); return w }()} {
						if mcpWorkspaceHasPaneID(got.PaneIds, "worker") || mcpLayoutHasPane(got.Layout, "worker") {
							t.Fatalf("closed pane returned as a recovery tile: %+v", got)
						}
						for _, def := range got.Panes {
							if def.ID == "worker" {
								t.Fatal("closed pane launch recipe was restored")
							}
						}
						if !mcpWorkspaceHasPaneID(got.RemovedPaneIDs, "worker") {
							t.Fatal("closed pane tombstone lost")
						}
					}
				}
				api := &apiServer{ws: store}
				for _, summary := range api.listWorkspaceSummaries() {
					if summary.Name == name && !mcpWorkspaceHasPaneID(summary.RemovedPaneIDs, "worker") {
						t.Fatal("summary poll cannot discover closure")
					}
				}
			})
		}
	}
}

func TestWorkspaceSaveAllowsLocallyCreatedObservedPane(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Workspace{Name: "Desk", Layout: LayoutNode{"type": "pane", "paneId": "p1"}, PaneIds: []string{"p1"}}); err != nil {
		t.Fatal(err)
	}
	known := []string{"p1", "local"}
	next := &Workspace{Name: "Desk", KnownPaneIDs: &known, PaneIds: known, Panes: []PaneDef{{ID: "local"}}, Layout: insertPaneIntoLayout(LayoutNode{"type": "pane", "paneId": "p1"}, "local", "horizontal", "p1")}
	if err := store.Put(next); err != nil {
		t.Fatal(err)
	}
	if !mcpWorkspaceHasPaneID(next.PaneIds, "local") || !mcpLayoutHasPane(next.Layout, "local") {
		t.Fatal("new local pane mistaken for a server removal")
	}
}

func TestMCPSpawnPanePlacementAndShow(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := insertPaneIntoLayout(LayoutNode{"type": "pane", "paneId": "caller"}, "other", "horizontal", "")
	putMCPSpawnWorkspace(t, store, &Workspace{Name: "Desk", LayoutMode: "free", PaneIds: []string{"caller", "other"}, Panes: []PaneDef{{ID: "caller"}, {ID: "other"}}, Layout: layout}, time.Now())
	api, _ := newMCPSpawnWorkspaceAPI(t, store)
	api.uiBus = newUIBus()
	metadata := requireMCPSpawnSuccess(t, api, "caller", map[string]any{"cmd": "bash", "show": true})
	got, _ := store.Get("Desk")
	left, _ := layoutChild(got.Layout["a"])
	if got.Layout["ratio"] != 0.5 || !mcpLayoutHasPane(left, metadata.ID) || !mcpLayoutHasPane(left, "caller") || mcpLayoutHasPane(left, "other") {
		t.Fatalf("new pane not beside caller: %+v", got.Layout)
	}
	api.uiBus.logMu.Lock()
	defer api.uiBus.logMu.Unlock()
	event := api.uiBus.log[len(api.uiBus.log)-1]
	if event.Type != "pane.spawned" || !event.Show || event.Workspace != "Desk" || event.PaneID != metadata.ID {
		t.Fatalf("spawn event: %+v", event)
	}
	for _, tool := range mcpTools() {
		if tool.Name == "spawn_pane" {
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			if schema.Properties["show"].Type != "boolean" {
				t.Fatal("show missing from schema")
			}
		}
	}
}

func TestAppendPaneUsesProportionalSpace(t *testing.T) {
	layout := LayoutNode{"type": "pane", "paneId": "one"}
	for _, id := range []string{"two", "three", "four"} {
		layout = insertPaneIntoLayout(layout, id, "horizontal", "")
	}
	if layout["ratio"] != 0.75 {
		t.Fatalf("fourth pane takes more than a quarter: %+v", layout)
	}
}

func TestWorkspaceScratchpadCanBeClearedWithoutObservingOldPanes(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Workspace{Name: "_current", PaneIds: []string{"old"}, Panes: []PaneDef{{ID: "old", Origin: &PaneOrigin{Source: "agent"}}}, Layout: LayoutNode{"type": "pane", "paneId": "old"}}); err != nil {
		t.Fatal(err)
	}
	known := []string{}
	cleared := &Workspace{Name: "_current", KnownPaneIDs: &known}
	if err := store.Put(cleared); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("_current")
	if got.Layout != nil || len(got.PaneIds) != 0 || len(got.Panes) != 0 {
		t.Fatalf("scratchpad clear revived old panes: %+v", got)
	}
	reloaded, err := NewWorkspaceStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reloaded.Get("_current")
	if got.Layout != nil || len(got.PaneIds) != 0 {
		t.Fatalf("scratchpad revived after restart: %+v", got)
	}
}
