package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidModelID(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "claude-fable-5", "claude-opus-4-8", "sonnet", "claude-fable-5[1m]", "glm-5.2"} {
		if !validModelID(ok) {
			t.Errorf("validModelID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"model with spaces", "a;rm -rf", "x/../../etc", strings.Repeat("m", 65)} {
		if validModelID(bad) {
			t.Errorf("validModelID(%q) = true, want false", bad)
		}
	}
}

// TestPersistedPaneModelRoundTrip mirrors TestPersistedPaneEffortRoundTrip:
// Model must survive snapshot -> on-disk -> JSON -> back, legacy entries load
// as "", and empty stays omitted (panes.json byte-identical for old panes).
func TestPersistedPaneModelRoundTrip(t *testing.T) {
	t.Parallel()
	pp := persistedFromSnapshot(PaneSnapshot{ID: "p1", Model: "claude-fable-5"})
	if pp.Model != "claude-fable-5" {
		t.Fatalf("persistedFromSnapshot dropped Model: got %q", pp.Model)
	}

	b, err := json.Marshal(pp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PersistedPane
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Model != "claude-fable-5" {
		t.Fatalf("Model did not round-trip: got %q", back.Model)
	}

	var legacy PersistedPane
	if err := json.Unmarshal([]byte(`{"id":"old","kind":"native"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.Model != "" {
		t.Fatalf("legacy entry should have empty Model, got %q", legacy.Model)
	}

	empty, _ := json.Marshal(persistedFromSnapshot(PaneSnapshot{ID: "p2"}))
	if strings.Contains(string(empty), `"model"`) {
		t.Fatalf("empty Model should be omitted from JSON, got %s", empty)
	}
}
