package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkspaceStoreSetContextPack(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Workspace{Name: "strategy", PaneIds: []string{}}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.SetContextPack("strategy", "/packs/strategy")
	if err != nil || updated.ContextPack != "/packs/strategy" {
		t.Fatalf("set = %+v, %v", updated, err)
	}
	updated, err = store.SetContextPack("strategy", "")
	if err != nil || updated.ContextPack != "" {
		t.Fatalf("clear = %+v, %v", updated, err)
	}
	if _, err := store.SetContextPack("missing", "/pack"); err == nil {
		t.Fatal("unknown workspace unexpectedly succeeded")
	}
}

func TestWorkspaceStorePutPreservesContextPack(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Workspace{Name: "strategy", PaneIds: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetContextPack("strategy", "/packs/strategy"); err != nil {
		t.Fatal(err)
	}
	autosave := &Workspace{Name: "strategy", PaneIds: []string{}}
	if err := store.Put(autosave); err != nil {
		t.Fatal(err)
	}
	if autosave.ContextPack != "/packs/strategy" {
		t.Fatalf("autosave response context pack = %q", autosave.ContextPack)
	}
	got, _ := store.Get("strategy")
	if got.ContextPack != "/packs/strategy" {
		t.Fatalf("context pack = %q", got.ContextPack)
	}
}

func TestWorkspaceStoreRenamePreservesContextPack(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Workspace{Name: "before", PaneIds: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetContextPack("before", "/packs/strategy"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rename("before", "after"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("after")
	if got.ContextPack != "/packs/strategy" {
		t.Fatalf("context pack = %q", got.ContextPack)
	}
}

func TestPaneDefContextPackJSON(t *testing.T) {
	empty, err := json.Marshal(PaneDef{ID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "contextPack") {
		t.Fatalf("empty contextPack was not omitted: %s", empty)
	}
	want := "/packs/strategy"
	b, err := json.Marshal(PaneDef{ID: "p", ContextPack: want})
	if err != nil {
		t.Fatal(err)
	}
	var got PaneDef
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ContextPack != want {
		t.Fatalf("round trip = %q", got.ContextPack)
	}
}

func TestWorkspaceStoreAddPaneAppendsAtomically(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name:    "Soluterminal",
		Layout:  LayoutNode{"type": "pane", "paneId": "p1"},
		PaneIds: []string{"p1"},
		Panes:   []PaneDef{{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "shell"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := ws.AddPane(
		"Soluterminal",
		PaneDef{ID: "p2", Cwd: "/repo", Cmd: []string{"bash"}, Title: "shell"},
		"horizontal",
		"p1",
	)
	if err != nil {
		t.Fatalf("AddPane: %v", err)
	}
	if len(got.PaneIds) != 2 || got.PaneIds[1] != "p2" {
		t.Fatalf("PaneIds = %#v, want p2 appended", got.PaneIds)
	}
	if len(got.Panes) != 2 || got.Panes[1].ID != "p2" {
		t.Fatalf("Panes = %#v, want p2 appended", got.Panes)
	}
	if got.Layout["type"] != "split" || got.Layout["direction"] != "horizontal" {
		t.Fatalf("Layout = %#v, want horizontal split", got.Layout)
	}
}

func TestWorkspaceStoreAddPaneFallsBackWhenTargetMissing(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name:    "Soluterminal",
		Layout:  LayoutNode{"type": "pane", "paneId": "p1"},
		PaneIds: []string{"p1"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := ws.AddPane(
		"Soluterminal",
		PaneDef{ID: "p2", Cwd: "/repo", Cmd: []string{"bash"}, Title: "shell"},
		"horizontal",
		"missing",
	)
	if err != nil {
		t.Fatalf("AddPane: %v", err)
	}
	if got.Layout["type"] != "split" {
		t.Fatalf("Layout = %#v, want fallback split", got.Layout)
	}
	if got.Layout["a"] == nil || got.Layout["b"] == nil {
		t.Fatalf("Layout = %#v, want existing layout plus new leaf", got.Layout)
	}
}

func TestWorkspaceStoreMovePaneIsAtomicAndPreservesLayouts(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	p1 := PaneDef{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"}
	p2 := PaneDef{ID: "p2", Cwd: "/repo", Cmd: []string{"bash"}, Title: "two"}
	if err := ws.Put(&Workspace{
		Name: "source", Layout: LayoutNode{"type": "pane", "paneId": "p1"}, PaneIds: []string{"p1"}, Panes: []PaneDef{p1}, ContextPack: "/packs/source",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ws.Put(&Workspace{
		Name: "destination", Layout: LayoutNode{"type": "pane", "paneId": "p2"}, PaneIds: []string{"p2"}, Panes: []PaneDef{p2}, ContextPack: "/packs/destination",
	}); err != nil {
		t.Fatal(err)
	}

	source, destination, err := ws.MovePane("source", "destination", "p1", "vertical", "p2", 0)
	if err != nil {
		t.Fatalf("MovePane: %v", err)
	}
	if len(source.PaneIds) != 0 || source.Layout != nil || source.ContextPack != "/packs/source" {
		t.Fatalf("source = %#v, want empty layout with metadata preserved", source)
	}
	if len(destination.PaneIds) != 2 || destination.PaneIds[1] != "p1" || len(destination.Panes) != 2 {
		t.Fatalf("destination = %#v, want p1 appended", destination)
	}
	if destination.Layout["type"] != "split" || destination.Layout["direction"] != "horizontal" {
		t.Fatalf("destination layout = %#v, want balanced auto row", destination.Layout)
	}
	if destination.ContextPack != "/packs/destination" {
		t.Fatalf("destination context pack = %q", destination.ContextPack)
	}

	if _, _, err := ws.MovePane("source", "destination", "missing", "horizontal", "", source.PaneMoveVersion); err == nil {
		t.Fatal("missing pane unexpectedly moved")
	}
	gotSource, _ := ws.Get("source")
	gotDestination, _ := ws.Get("destination")
	if len(gotSource.PaneIds) != 0 || len(gotDestination.PaneIds) != 2 {
		t.Fatalf("failed move changed state: source=%#v destination=%#v", gotSource, gotDestination)
	}
}

func TestWorkspaceStoreRemovePaneRemovesWorkspaceReferences(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name: "Soluterminal",
		Layout: LayoutNode{
			"type":      "split",
			"direction": "horizontal",
			"ratio":     0.5,
			"a":         LayoutNode{"type": "pane", "paneId": "p1"},
			"b":         LayoutNode{"type": "pane", "paneId": "p2"},
		},
		PaneIds: []string{"p1", "p2"},
		Panes: []PaneDef{
			{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"},
			{ID: "p2", Cwd: "/repo", Cmd: []string{"bash"}, Title: "two"},
		},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := ws.RemovePane("p2"); err != nil {
		t.Fatalf("RemovePane: %v", err)
	}
	got, ok := ws.Get("Soluterminal")
	if !ok {
		t.Fatal("workspace missing")
	}
	if len(got.PaneIds) != 1 || got.PaneIds[0] != "p1" {
		t.Fatalf("PaneIds = %#v, want only p1", got.PaneIds)
	}
	if len(got.Panes) != 1 || got.Panes[0].ID != "p1" {
		t.Fatalf("Panes = %#v, want only p1", got.Panes)
	}
	if got.Layout["type"] != "pane" || got.Layout["paneId"] != "p1" {
		t.Fatalf("Layout = %#v, want p1 leaf", got.Layout)
	}
}

func TestWorkspaceStorePutPreservesRecentlyAddedPane(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name:    "Soluterminal",
		Layout:  LayoutNode{"type": "pane", "paneId": "p1"},
		PaneIds: []string{"p1"},
		Panes:   []PaneDef{{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := ws.AddPane(
		"Soluterminal",
		PaneDef{ID: "p2", Cwd: "/repo", Cmd: []string{"bash"}, Title: "two"},
		"horizontal",
		"p1",
	); err != nil {
		t.Fatalf("AddPane: %v", err)
	}

	if err := ws.Put(&Workspace{
		Name:    "Soluterminal",
		Layout:  LayoutNode{"type": "pane", "paneId": "p1"},
		PaneIds: []string{"p1"},
		Panes:   []PaneDef{{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"}},
	}); err != nil {
		t.Fatalf("stale Put: %v", err)
	}

	got, ok := ws.Get("Soluterminal")
	if !ok {
		t.Fatal("workspace missing")
	}
	if len(got.PaneIds) != 2 || got.PaneIds[1] != "p2" {
		t.Fatalf("PaneIds = %#v, want recently added p2 preserved", got.PaneIds)
	}
	if len(got.Panes) != 2 || got.Panes[1].ID != "p2" {
		t.Fatalf("Panes = %#v, want recently added p2 def preserved", got.Panes)
	}
}

func TestWorkspaceServiceTargetSurvivesReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewWorkspaceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := &BrowserServiceTargetDef{URL: "http://127.0.0.1:5173", PreviewPath: "/api/proxy/5173~b2xk/", Backend: "win", MachinePrefix: "/m/host", PaneID: "app", Port: 5173, Addr: "127.0.0.1", Proto: "tcp"}
	if err := store.Put(&Workspace{Name: "Services", Browsers: []BrowserInfoDef{{ID: "preview", URL: target.URL, Service: target}}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewWorkspaceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("Services")
	if !ok || len(got.Browsers) != 1 || got.Browsers[0].Service == nil || *got.Browsers[0].Service != *target {
		t.Fatalf("service recovery identity did not survive workspace reload: %+v", got)
	}
}

func TestWorkspaceStoreGetReturnsDeepCopy(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name: "Soluterminal",
		Layout: LayoutNode{
			"type":      "split",
			"direction": "horizontal",
			"a":         LayoutNode{"type": "pane", "paneId": "p1"},
			"b":         LayoutNode{"type": "pane", "paneId": "p2"},
			"meta":      []any{LayoutNode{"paneId": "p1"}},
		},
		PaneIds: []string{"p1", "p2"},
		Panes: []PaneDef{
			{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"},
			{ID: "p2", Cwd: "/repo", Cmd: []string{"zsh"}, Title: "two"},
		},
		Browsers: []BrowserInfoDef{{ID: "b1", URL: "https://example.test", CreatedAt: "now", Service: &BrowserServiceTargetDef{URL: "http://127.0.0.1:5173", Backend: "win", PreviewPath: "/api/proxy/5173~YWJj/", MachinePrefix: "/m/host"}}},
		Files:    []FileTileInfoDef{{ID: "f1", Path: "/repo/README.md", CreatedAt: "now"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := ws.Get("Soluterminal")
	if !ok {
		t.Fatal("workspace missing")
	}
	got.PaneIds[0] = "mutated"
	got.Panes[0].Cmd[0] = "mutated"
	got.Panes[0].Title = "mutated"
	got.Browsers[0].URL = "https://mutated.test"
	got.Browsers[0].Service.Backend = "mutated"
	got.Files[0].Path = "/mutated"
	got.Layout["type"] = "mutated"
	if child, ok := got.Layout["a"].(LayoutNode); ok {
		child["paneId"] = "mutated"
	} else {
		t.Fatalf("Layout[a] = %#v, want LayoutNode", got.Layout["a"])
	}
	meta := got.Layout["meta"].([]any)
	meta[0].(LayoutNode)["paneId"] = "mutated"

	again, ok := ws.Get("Soluterminal")
	if !ok {
		t.Fatal("workspace missing after mutation")
	}
	if again.PaneIds[0] != "p1" {
		t.Fatalf("PaneIds = %#v, want original", again.PaneIds)
	}
	if again.Panes[0].Cmd[0] != "bash" || again.Panes[0].Title != "one" {
		t.Fatalf("Panes = %#v, want original", again.Panes)
	}
	if again.Browsers[0].URL != "https://example.test" {
		t.Fatalf("Browsers = %#v, want original URL", again.Browsers)
	}
	if again.Browsers[0].Service.Backend != "win" {
		t.Fatal("service routing snapshot was mutated")
	}
	if again.Files[0].Path != "/repo/README.md" {
		t.Fatalf("Files = %#v, want original path", again.Files)
	}
	if again.Layout["type"] != "split" {
		t.Fatalf("Layout[type] = %#v, want split", again.Layout["type"])
	}
	child, ok := again.Layout["a"].(LayoutNode)
	if !ok {
		t.Fatalf("Layout[a] = %#v, want LayoutNode", again.Layout["a"])
	}
	if child["paneId"] != "p1" {
		t.Fatalf("Layout[a] = %#v, want p1", child)
	}
	meta = again.Layout["meta"].([]any)
	if meta[0].(LayoutNode)["paneId"] != "p1" {
		t.Fatalf("Layout[meta] = %#v, want p1", meta)
	}
}

func TestWorkspaceStoreListReturnsDeepCopies(t *testing.T) {
	t.Parallel()
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	if err := ws.Put(&Workspace{
		Name:    "Soluterminal",
		Layout:  LayoutNode{"type": "pane", "paneId": "p1"},
		PaneIds: []string{"p1"},
		Panes:   []PaneDef{{ID: "p1", Cwd: "/repo", Cmd: []string{"bash"}, Title: "one"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	list := ws.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	list[0].PaneIds[0] = "mutated"
	list[0].Panes[0].Cmd[0] = "mutated"
	list[0].Layout["paneId"] = "mutated"
	list[0] = nil

	again := ws.List()
	if len(again) != 1 {
		t.Fatalf("List len after mutation = %d, want 1", len(again))
	}
	if again[0] == nil {
		t.Fatal("List returned nil workspace after caller mutated prior list")
	}
	if again[0].PaneIds[0] != "p1" {
		t.Fatalf("PaneIds = %#v, want original", again[0].PaneIds)
	}
	if again[0].Panes[0].Cmd[0] != "bash" {
		t.Fatalf("Panes = %#v, want original command", again[0].Panes)
	}
	if again[0].Layout["paneId"] != "p1" {
		t.Fatalf("Layout = %#v, want original pane id", again[0].Layout)
	}
}

func TestOwnerTerminalMigratesHistoricalDashboardInPlace(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := LayoutNode{"type": "split", "direction": "horizontal", "ratio": 0.7, "a": LayoutNode{"type": "pane", "paneId": "existing"}, "b": LayoutNode{"type": "tasks", "tasksId": "owner_wft_test", "ownerTaskId": "wft_test"}}
	if err := store.Put(&Workspace{Name: "Workspace", Layout: old, PaneIds: []string{"existing"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := store.AddPane("Workspace", PaneDef{ID: "owner_wft_test", Role: paneRoleTaskOwner}, "horizontal", "")
		if err != nil {
			t.Fatal(err)
		}
		child, _ := layoutChild(got.Layout["b"])
		if len(got.PaneIds) != 2 || child["type"] != "pane" || child["paneId"] != "owner_wft_test" || got.Layout["ratio"] != 0.7 {
			t.Fatalf("owner not migrated in place: %+v", got)
		}
	}
}
