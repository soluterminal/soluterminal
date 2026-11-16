package main

import (
	"encoding/json"

	"strings"
	"testing"
)

func TestValidEffortLevel(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if !validEffortLevel(ok) {
			t.Errorf("validEffortLevel(%q) = false, want true", ok)
		}
	}
	// Note: validEffortLevel trims whitespace (the API trims too), so "high " is
	// valid — only genuinely-unknown tokens are rejected.
	for _, bad := range []string{"lowish", "ultra", "ultracode", "MAX", "0", "true"} {
		if validEffortLevel(bad) {
			t.Errorf("validEffortLevel(%q) = true, want false", bad)
		}
	}
}

// indexOfPair returns the index of the first element equal to flag whose next
// element equals val, or -1. Used to assert "--effort max" appears as an
// adjacent argv pair (not just both tokens somewhere).
func indexOfPair(argv []string, flag, val string) int {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return i
		}
	}
	return -1
}

func argvHas(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// TestPersistedPaneEffortRoundTrip mirrors TestPersistedPaneProviderRoundTrip:
// Effort must survive snapshot -> on-disk -> JSON -> back, and a legacy entry
// with no effort key must unmarshal to "".
func TestPersistedPaneEffortRoundTrip(t *testing.T) {
	t.Parallel()
	pp := persistedFromSnapshot(PaneSnapshot{ID: "p1", Effort: "xhigh"})
	if pp.Effort != "xhigh" {
		t.Fatalf("persistedFromSnapshot dropped Effort: got %q", pp.Effort)
	}

	b, err := json.Marshal(pp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PersistedPane
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Effort != "xhigh" {
		t.Fatalf("Effort did not round-trip: got %q", back.Effort)
	}

	// Legacy entry (no "effort" key) -> "".
	var legacy PersistedPane
	if err := json.Unmarshal([]byte(`{"id":"old","kind":"native"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.Effort != "" {
		t.Fatalf("legacy entry should have empty Effort, got %q", legacy.Effort)
	}

	// omitempty: an empty Effort must not appear in the serialized JSON (keeps
	// existing panes.json byte-identical).
	empty, _ := json.Marshal(persistedFromSnapshot(PaneSnapshot{ID: "p2"}))
	if strings.Contains(string(empty), `"effort"`) {
		t.Fatalf("empty Effort should be omitted from JSON, got %s", empty)
	}
}
