package main

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPSpawnPaneShowRateLimit(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "allow")
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Desk", "Other"} {
		putMCPSpawnWorkspace(t, store, &Workspace{Name: name}, time.Now())
	}
	api, calls := newMCPSpawnWorkspaceAPI(t, store)
	api.uiBus = newUIBus()
	for _, id := range []string{"caller", "other"} {
		api.mgr.panes[id] = transactionTestPane(id)
	}
	for _, step := range []struct {
		caller, workspace     string
		show, reveal, skipped bool
	}{
		{"caller", "Desk", false, false, false}, // Silent spawns do not consume the cooldown.
		{"caller", "Desk", true, true, false},
		{"caller", "Desk", true, false, true},
		{"caller", "Other", true, false, true}, // Changing workspace cannot bypass it.
		{"other", "Desk", true, true, false},   // Each caller has an independent cooldown.
		{"caller", "Desk", false, false, false},
		{"nonexistent-one", "Desk", true, true, false},
		{"nonexistent-two", "Desk", true, false, true}, // Forged IDs share the unattributed bucket.
	} {
		text, isErr := callMCPSpawnPane(t, api, step.caller, map[string]any{
			"cmd": "bash", "workspace": step.workspace, "show": step.show,
		})
		if isErr {
			t.Fatalf("rate limit prevented spawn: %s", text)
		}
		var result struct {
			ID      string `json:"id"`
			Show    *bool  `json:"show"`
			Skipped string `json:"show_skipped"`
		}
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatal(err)
		}
		requireMCPPaneInWorkspace(t, store, step.workspace, result.ID)
		if step.show && (result.Show == nil || *result.Show != step.reveal) {
			t.Fatalf("unexpected reveal status: %s", text)
		}
		if (result.Skipped != "") != step.skipped || (step.skipped && !strings.Contains(result.Skipped, "10 seconds")) {
			t.Fatalf("missing or unexpected skip explanation: %s", text)
		}
		api.uiBus.logMu.Lock()
		event := api.uiBus.log[len(api.uiBus.log)-1]
		api.uiBus.logMu.Unlock()
		if event.Type != "pane.spawned" || event.PaneID != result.ID || event.Show != step.reveal {
			t.Fatalf("unexpected spawn event: %+v", event)
		}
	}
	if *calls != 8 {
		t.Fatalf("spawned %d panes, want all 8", *calls)
	}
}

func TestSpawnRevealLimiterInterval(t *testing.T) {
	var limiter spawnRevealLimiter
	now := time.Unix(1000, 0)
	for _, step := range []struct {
		at   time.Duration
		want bool
	}{
		{0, true},
		{spawnRevealInterval - time.Nanosecond, false},
		{spawnRevealInterval, true},
		{2*spawnRevealInterval - time.Nanosecond, false},
		{2 * spawnRevealInterval, true},
	} {
		if got := limiter.allow("caller", now.Add(step.at)); got != step.want {
			t.Fatalf("at %s: reveal = %v, want %v", step.at, got, step.want)
		}
	}
	if !limiter.allow("", now.Add(2*spawnRevealInterval)) || limiter.allow("  ", now.Add(2*spawnRevealInterval)) {
		t.Fatal("unattributed callers did not share a cooldown")
	}
	limiter.allow("next", now.Add(3*spawnRevealInterval))
	if len(limiter.last) != 1 {
		t.Fatalf("expired callers retained: %v", limiter.last)
	}
}

func TestSpawnRevealLimiterConcurrent(t *testing.T) {
	var limiter spawnRevealLimiter
	var accepted atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.allow("caller", now) {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("concurrent requests revealed %d panes, want 1", accepted.Load())
	}
}
