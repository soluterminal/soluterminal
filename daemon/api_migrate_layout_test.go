package main

import (
	"reflect"
	"testing"
)

// In-place migration through the HTTP handler keeps the exact slot, carries the
// human approval lock, and tombstones the retired pane so a stale legacy save
// cannot resurrect it.
func TestMigrateInplaceKeepsSlotAndTombstonesRetiredPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOLU_NATIVE_RENDERER", "1")
	p := switchTestPane("old", "", []string{"claude"}, "session-migrate")
	p.ClaudeTranscriptPath = writeTempTranscript(t)
	p.NeverAutoAnswer = true
	close(p.done)
	a, _, _ := resumeTestServer(t, p)
	panes, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = panes
	ws, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.ws = ws
	spawn := a.spawnPane
	a.spawnPane = func(o SpawnOpts) (*Pane, error) {
		if o.ID == "" {
			o.ID = "new"
		}
		return spawn(o)
	}
	leaf := func(id string) LayoutNode { return LayoutNode{"type": "pane", "paneId": id} }
	layout := LayoutNode{"type": "split", "direction": "horizontal", "ratio": .3, "a": leaf("old"), "b": leaf("other")}
	if err := ws.Put(&Workspace{Name: "Desk", Layout: layout, PaneIds: []string{"old", "other"}, Panes: []PaneDef{{ID: "old"}, {ID: "other"}}}); err != nil {
		t.Fatal(err)
	}
	stale, _ := ws.Get("Desk")

	s := autoSettingsServer(t, a)
	autoSettingsPost(t, s, "/api/panes/old/migrate-clean", `{"mode":"inplace","confirm":true}`, 200)

	got, _ := ws.Get("Desk")
	expected := cloneLayoutDeep(layout)
	expected["a"] = leaf("new")
	if !reflect.DeepEqual(got.Layout, expected) || !reflect.DeepEqual(got.PaneIds, []string{"new", "other"}) {
		t.Fatalf("migration moved its slot: layout=%v ids=%v", got.Layout, got.PaneIds)
	}
	if !reflect.DeepEqual(got.RemovedPaneIDs, []string{"old"}) {
		t.Fatalf("retired pane not tombstoned: %v", got.RemovedPaneIDs)
	}
	var def *PaneDef
	for i := range got.Panes {
		if got.Panes[i].ID == "new" {
			def = &got.Panes[i]
		}
	}
	if def == nil || !def.NeverAutoAnswer {
		t.Fatalf("replacement def lost the approval lock: %+v", got.Panes)
	}

	// A view that never saw the migration saves its old layout.
	known := []string{"old", "other"}
	stale.KnownPaneIDs = &known
	if err := ws.Put(stale); err != nil {
		t.Fatal(err)
	}
	after, _ := ws.Get("Desk")
	for _, id := range append(collectLayoutPaneIDsForTest(after.Layout), after.PaneIds...) {
		if id == "old" {
			t.Fatalf("stale save resurrected the retired pane: layout=%v ids=%v", after.Layout, after.PaneIds)
		}
	}
}

func collectLayoutPaneIDsForTest(n LayoutNode) []string {
	if n == nil {
		return nil
	}
	if n["type"] == "pane" {
		id, _ := n["paneId"].(string)
		return []string{id}
	}
	a, _ := layoutChild(n["a"])
	b, _ := layoutChild(n["b"])
	return append(collectLayoutPaneIDsForTest(a), collectLayoutPaneIDsForTest(b)...)
}
