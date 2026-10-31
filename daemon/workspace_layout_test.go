package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWorkspaceAutoGridAndCustomGeometry(t *testing.T) {
	expected := [][]int{{1}, {2}, {3}, {2, 2}, {3, 2}, {3, 3}, {4, 3}, {4, 4}, {3, 3, 3}, {4, 3, 3}, {4, 4, 3}, {4, 4, 4}}
	for i, rows := range expected {
		w := &Workspace{}
		for n := 0; n <= i; n++ {
			addWorkspaceLayoutPane(w, string(rune('a'+n)), "horizontal", "")
		}
		var actual []int
		var walk func(LayoutNode)
		walk = func(node LayoutNode) {
			if node["type"] == "split" && node["direction"] == "vertical" {
				a, _ := layoutChild(node["a"])
				b, _ := layoutChild(node["b"])
				walk(a)
				walk(b)
			} else {
				actual = append(actual, len(layoutLeaves(node)))
			}
		}
		walk(w.Layout)
		if !reflect.DeepEqual(actual, rows) {
			t.Fatalf("%d panes rows %v want %v", i+1, actual, rows)
		}
	}
	leaf := func(id string) LayoutNode { return LayoutNode{"type": "pane", "paneId": id} }
	w := &Workspace{LayoutMode: "free", Layout: LayoutNode{"type": "split", "direction": "horizontal", "ratio": .2, "a": leaf("small"), "b": leaf("large")}}
	addWorkspaceLayoutPane(w, "new", "vertical", "small")
	b, _ := layoutChild(w.Layout["b"])
	if b["type"] != "split" || b["direction"] != "horizontal" || w.Layout["ratio"] != .2 {
		t.Fatalf("not largest: %+v", w.Layout)
	}
	row := LayoutNode{"type": "split", "direction": "horizontal", "ratio": .2, "a": leaf("a"), "b": LayoutNode{"type": "split", "direction": "horizontal", "ratio": .375, "a": leaf("b"), "b": leaf("c")}}
	next, removed := removeCustomLayoutPane(row, "b")
	if !removed || math.Abs(next["ratio"].(float64)-.35) > .000001 {
		t.Fatalf("did not share released area: %+v", next)
	}
}
func TestWorkspaceLayoutRevisionRejectsStaleRetileAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWorkspaceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(0)
	w := &Workspace{Name: "Desk", LayoutVersion: 999, LayoutMode: "auto", LayoutBaseVersion: &base, PaneIds: []string{"a", "b"}, Layout: LayoutNode{"type": "split", "direction": "horizontal", "ratio": .5, "a": LayoutNode{"type": "pane", "paneId": "a"}, "b": LayoutNode{"type": "pane", "paneId": "b"}}}
	if err := store.Put(w); err != nil {
		t.Fatal(err)
	}
	if w.LayoutVersion != 0 {
		t.Fatal("new workspace revision is not daemon-owned")
	}
	stale := cloneWorkspace(w)
	changed := cloneWorkspace(w)
	changed.LayoutMode = "free"
	changed.Layout["ratio"] = .3
	changed.LayoutBaseVersion = &base
	if err := store.Put(changed); err != nil {
		t.Fatal(err)
	}
	if changed.LayoutVersion != 1 {
		t.Fatalf("revision %d", changed.LayoutVersion)
	}
	store, err = NewWorkspaceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	stale.LayoutBaseVersion = &base
	if err := store.Put(stale); !errors.Is(err, errWorkspaceLayoutConflict) {
		t.Fatalf("stale retile: %v", err)
	}
	// An old tab without a base cannot undo a modern client's saved decision.
	stale.LayoutBaseVersion = nil
	if err := store.Put(stale); !errors.Is(err, errWorkspaceLayoutConflict) {
		t.Fatalf("legacy overwrite: %v", err)
	}
	got, _ := store.Get("Desk")
	if got.Layout["ratio"] != .3 || got.LayoutMode != "free" {
		t.Fatal("stale overwrite changed committed geometry")
	}
}

func TestWorkspaceLayoutConflictIsHTTP409(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(0)
	current := &Workspace{Name: "Desk", LayoutMode: "auto", LayoutBaseVersion: &base, Layout: LayoutNode{"type": "pane", "paneId": "p"}, PaneIds: []string{"p"}}
	if err := store.Put(current); err != nil {
		t.Fatal(err)
	}
	stale := cloneWorkspace(current)
	current.LayoutMode = "free"
	current.LayoutBaseVersion = &base
	if err := store.Put(current); err != nil {
		t.Fatal(err)
	}
	stale.LayoutBaseVersion = &base
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	(&apiServer{ws: store}).putWorkspace(recorder, httptest.NewRequest("POST", "/api/workspaces", bytes.NewReader(body)))
	if recorder.Code != 409 {
		t.Fatalf("stale layout response %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestMCPSpawnPaneJoinsAutoGrid(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &Workspace{Name: "Desk", LayoutMode: "auto", LayoutColumns: 4, LayoutAspect: 1.6}
	for _, id := range []string{"caller", "p2", "p3", "p4", "p5", "p6", "p7", "p8"} {
		addWorkspaceLayoutPane(w, id, "horizontal", "")
		w.PaneIds = append(w.PaneIds, id)
		w.Panes = append(w.Panes, PaneDef{ID: id})
	}
	putMCPSpawnWorkspace(t, store, w, time.Now())
	api, _ := newMCPSpawnWorkspaceAPI(t, store)
	metadata := requireMCPSpawnSuccess(t, api, "caller", map[string]any{"cmd": "bash"})
	got, _ := store.Get("Desk")
	var order []string
	layoutPaneOrder(got.Layout, &order)
	if len(order) != 9 || order[8] != metadata.ID {
		t.Fatalf("agent pane did not append in number order: %v", order)
	}
	var rows []int
	var walk func(LayoutNode)
	walk = func(n LayoutNode) {
		if n["type"] == "split" && n["direction"] == "vertical" {
			a, _ := layoutChild(n["a"])
			b, _ := layoutChild(n["b"])
			walk(a)
			walk(b)
		} else {
			rows = append(rows, len(layoutLeaves(n)))
		}
	}
	walk(got.Layout)
	if !reflect.DeepEqual(rows, []int{3, 3, 3}) {
		t.Fatalf("agent auto grid rows %v", rows)
	}
}

func TestWorkspaceStaleSaveCannotUndoCloseRetile(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(0)
	w := &Workspace{Name: "Desk", LayoutMode: "auto", LayoutColumns: 4, LayoutAspect: 1.6, LayoutBaseVersion: &base}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		addWorkspaceLayoutPane(w, id, "horizontal", "")
		w.PaneIds = append(w.PaneIds, id)
		w.Panes = append(w.Panes, PaneDef{ID: id})
	}
	if err := store.Put(w); err != nil {
		t.Fatal(err)
	}
	stale := cloneWorkspace(w)
	stale.LayoutBaseVersion = &base
	if err := store.RemovePane("c"); err != nil {
		t.Fatal(err)
	}
	closed, _ := store.Get("Desk")
	if closed.LayoutVersion <= stale.LayoutVersion {
		t.Fatal("close re-tile did not advance the layout revision")
	}
	if err := store.Put(stale); !errors.Is(err, errWorkspaceLayoutConflict) {
		t.Fatalf("stale close overwrite: %v", err)
	}
	// The caller that already removed the tile may acknowledge the exact saved
	// result without a false conflict caused by its own DELETE request.
	localClose := cloneWorkspace(closed)
	localClose.LayoutBaseVersion = &base
	localClose.LayoutVersion = base
	if err := store.Put(localClose); err != nil {
		t.Fatalf("matching close acknowledgement: %v", err)
	}
	if localClose.LayoutVersion != closed.LayoutVersion {
		t.Fatal("a matching acknowledgement changed the revision")
	}
}

func TestWorkspaceMigrationReplacementPreservesExactSlot(t *testing.T) {
	for _, mode := range []string{"auto", "free"} {
		t.Run(mode, func(t *testing.T) {
			store, err := NewWorkspaceStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			leaf := func(id string) LayoutNode { return LayoutNode{"type": "pane", "paneId": id} }
			layout := LayoutNode{"type": "split", "direction": "horizontal", "ratio": .27, "a": leaf("other"), "b": LayoutNode{"type": "split", "direction": "vertical", "ratio": .37, "a": leaf("old"), "b": leaf("last")}}
			base := uint64(0)
			if err := store.Put(&Workspace{Name: "Desk", LayoutMode: mode, LayoutBaseVersion: &base, Layout: layout, PaneIds: []string{"other", "old", "last"}, Panes: []PaneDef{{ID: "other"}, {ID: "old"}, {ID: "last"}}}); err != nil {
				t.Fatal(err)
			}
			got, err := store.ReplacePane("Desk", "old", PaneDef{ID: "new", Title: "Replacement"})
			if err != nil {
				t.Fatal(err)
			}
			expected := cloneLayoutDeep(layout)
			right, _ := layoutChild(expected["b"])
			right["a"] = leaf("new")
			if !reflect.DeepEqual(got.Layout, expected) || !reflect.DeepEqual(got.PaneIds, []string{"other", "new", "last"}) {
				t.Fatalf("migration moved its slot: %+v", got)
			}
			if got.LayoutVersion != 1 || got.LayoutMode != mode {
				t.Fatalf("replacement metadata: %+v", got)
			}
			if !reflect.DeepEqual(got.RemovedPaneIDs, []string{"old"}) {
				t.Fatalf("replacement did not tombstone the retired pane: %+v", got.RemovedPaneIDs)
			}
			before, _ := store.Get("Desk")
			store.path = filepath.Join(store.path, "blocked.json")
			if _, err := store.ReplacePane("Desk", "new", PaneDef{ID: "failed"}); !errors.Is(err, errWorkspacePersistence) {
				t.Fatalf("replacement write failure: %v", err)
			}
			after, _ := store.Get("Desk")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed replacement changed the committed layout")
			}
		})
	}
}

func TestWorkspaceTimestampSaveDoesNotBroadcastFullLayout(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := uint64(0)
	current := &Workspace{Name: "Desk", LayoutMode: "auto", LayoutBaseVersion: &base, Layout: LayoutNode{"type": "pane", "paneId": "p"}, PaneIds: []string{"p"}}
	if err := store.Put(current); err != nil {
		t.Fatal(err)
	}
	api := &apiServer{ws: store, uiBus: newUIBus()}
	save := func(w *Workspace) int {
		body, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		api.putWorkspace(response, httptest.NewRequest("POST", "/api/workspaces", bytes.NewReader(body)))
		return response.Code
	}
	current.LayoutBaseVersion = &base
	if code := save(current); code != 200 {
		t.Fatalf("timestamp save: %d", code)
	}
	if len(api.uiBus.log) != 0 {
		t.Fatal("timestamp-only save broadcast a full layout invalidation")
	}
	current.LayoutMode = "free"
	if code := save(current); code != 200 {
		t.Fatalf("mode save: %d", code)
	}
	if len(api.uiBus.log) != 1 || api.uiBus.log[0].Type != "workspace.updated" {
		t.Fatal("mode change did not notify viewers")
	}
	if code := save(&Workspace{Name: "_current", LayoutMode: "auto", LayoutBaseVersion: &base}); code != 200 {
		t.Fatalf("scratch save: %d", code)
	}
	if len(api.uiBus.log) != 1 {
		t.Fatal("scratch save notified other windows")
	}
	if code := save(&Workspace{Name: "Invalid", LayoutMode: "wrong", LayoutBaseVersion: &base}); code != 400 {
		t.Fatalf("invalid mode response: %d", code)
	}
}
