package main

import (
	"reflect"
	"testing"
	"time"
)

func TestManagerListStableCreationOrder(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := &Manager{panes: map[string]*Pane{
		"late":   {ID: "late", CreatedAt: now.Add(time.Second)},
		"b":      {ID: "b", CreatedAt: now},
		"a":      {ID: "a", CreatedAt: now},
		"legacy": {ID: "legacy"},
	}}
	want := []string{"legacy", "a", "b", "late"}
	for i := 0; i < 100; i++ {
		got := []string{}
		for _, p := range m.List() {
			got = append(got, p.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("List() call %d = %v, want %v", i, got, want)
		}
	}
	// Sorting a returned slice must not change subsequent calls.
	list := m.List()
	list[0], list[3] = list[3], list[0]
	if m.List()[0].ID != "legacy" {
		t.Fatal("List leaked its ordering storage")
	}
}

// A resumed pane must retain its sort key before Spawn publishes it, rather
// than racing List while post-spawn state is restored.
func TestResumeCreationTimeCarriedBeforePublication(t *testing.T) {
	t.Parallel()
	created := time.Unix(1234, 0).UTC()
	snap := PaneSnapshot{ID: "resumed", CreatedAt: created}
	opts := resumeSpawnOpts(snap, 0, 0)
	if !opts.CreatedAt.Equal(created) {
		t.Fatalf("resume timestamp = %v, want %v", opts.CreatedAt, created)
	}
	for _, target := range []string{"", "native"} {
		switched := switchSpawnOpts(snap, target, 0, 0)
		if !switched.CreatedAt.Equal(created) {
			t.Fatalf("switch to %q lost creation timestamp", target)
		}
	}
	p := &Pane{CreatedAt: created}
	restoreResumedState(p, PaneSnapshot{CreatedAt: created.Add(time.Hour)})
	if !p.CreatedAt.Equal(created) {
		t.Fatal("post-publication restoration changed immutable sort key")
	}
}
