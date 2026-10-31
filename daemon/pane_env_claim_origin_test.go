package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A caller placed only by SOLU_PANE_ID in its own environment (FromEnv:
// setsid, a detached tmux or screen server) owns no panes. That must also
// hold for what it leaves behind: a pane it spawns, or a schedule it files,
// must not record the pane it claims (or the "\x00claimed:" marker that
// carries the claim) as its starter, so no real pane can later own the pane
// through it, in paneStartedByCaller or anywhere else ownership is decided.

func envClaimSpawnServer(t *testing.T) (*apiServer, *[]SpawnOpts) {
	t.Helper()
	a := traceTestServer(t)
	a.panes = &failingPaneStore{}
	var spawned []SpawnOpts
	a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
		spawned = append(spawned, opts)
		p := transactionTestPane("pane_spawned_by_claim")
		p.Origin = opts.Origin.clone()
		a.mgr.mu.Lock()
		a.mgr.panes[p.ID] = p
		a.mgr.mu.Unlock()
		return p, nil
	}
	return a, &spawned
}

func originFieldsCarryClaim(o *PaneOrigin) bool {
	if o == nil {
		return false
	}
	for _, v := range []string{o.ByPaneID, o.ByPaneTitle, o.LeadPaneID, o.LeadPaneTitle} {
		if strings.Contains(v, "\x00") || strings.Contains(v, "claimed:") {
			return true
		}
	}
	return false
}

func TestEnvClaimSpawnOriginNeverMatchesARealPane(t *testing.T) {
	a, spawned := envClaimSpawnServer(t)
	claimsLead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, FromEnv: true}
	for _, header := range []string{"lead", ""} {
		*spawned = nil
		text, failed := mcpAs(t, a, claimsLead, header, "spawn_pane", map[string]any{"cmd": "bash", "title": "Spawned by a claim", "wait_ready": false})
		if failed {
			t.Fatalf("spawn_pane (header %q): %q", header, text)
		}
		if len(*spawned) != 1 {
			t.Fatalf("spawned %d panes", len(*spawned))
		}
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(text), &meta); err != nil || meta.ID == "" {
			t.Fatalf("spawn_pane result %q: %v", text, err)
		}
		p, ok := a.mgr.Get(meta.ID)
		if !ok {
			t.Fatalf("spawned pane %s missing", meta.ID)
		}
		origin := p.Snapshot().Origin
		// Stored as an unattributed ("api") pane: no starter, no lead, and no
		// trace of the claim.
		if origin == nil || origin.Source != paneOriginAPI || origin.ByPaneID != "" || origin.LeadPaneID != "" || originFieldsCarryClaim(origin) {
			t.Fatalf("origin from a FromEnv caller (header %q) = %+v", header, origin)
		}
		// No caller id owns it: every real pane, the claimed one, an empty
		// caller, and the raw marker itself.
		for _, caller := range []string{"lead", "worker", "rogue", "kid", "social", "", mcpClaimedCallerPrefix + "lead", mcpClaimedCallerPrefix} {
			if paneStartedByCaller(origin, caller) {
				t.Fatalf("paneStartedByCaller(%+v, %q) = true", origin, caller)
			}
			if caller != "" && caller != p.ID && a.paneAgentAuthority(origin, p.ID, caller, false) != paneAgentRefused {
				t.Fatalf("paneAgentAuthority lets %q act on a pane spawned by a claim", caller)
			}
		}
		// Through the tools: the claimed pane, traced for real, can't close
		// or rename it without the person's OK...
		lead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"kill_pane", map[string]any{"pane_id": p.ID}},
			{"rename_pane", map[string]any{"pane_id": p.ID, "title": "mine now"}},
			{"set_pane_info", map[string]any{"pane_id": p.ID, "working_on": "mine now"}},
		} {
			if text, failed := mcpAs(t, a, lead, "lead", tc.tool, tc.args); !failed || !strings.Contains(text, "was not started by you") {
				t.Fatalf("real lead %s on a pane spawned by its claimant: %q", tc.tool, text)
			}
			// ...and neither can the claimant itself.
			if text, failed := mcpAs(t, a, claimsLead, "lead", tc.tool, tc.args); !failed || !strings.Contains(text, "was not started by you") {
				t.Fatalf("claimant %s on the pane it spawned: %q", tc.tool, text)
			}
		}
		if _, ok := a.mgr.Get(p.ID); !ok {
			t.Fatal("the spawned pane was closed")
		}
		a.mgr.mu.Lock()
		delete(a.mgr.panes, p.ID)
		a.mgr.mu.Unlock()
	}
}

// Even an origin that somehow stored the marker (none is written today)
// matches no caller: a real pane's id differs from it, and a FromEnv caller
// reaches the tools with no pane id at all.
func TestOriginCarryingTheClaimMarkerOwnsNothing(t *testing.T) {
	a := traceTestServer(t)
	marked := &PaneOrigin{Source: paneOriginAgent, ByPaneID: mcpClaimedCallerPrefix + "lead", LeadPaneID: mcpClaimedCallerPrefix + "lead"}
	p := testPaneWithTail(t, "marked", "Marked", "")
	p.Origin = marked
	p.done = make(chan struct{})
	close(p.done)
	a.mgr.panes[p.ID] = p
	for _, caller := range []string{"lead", "worker", "rogue", ""} {
		if paneStartedByCaller(marked, caller) {
			t.Fatalf("paneStartedByCaller(marked, %q) = true", caller)
		}
	}
	if text, failed := mcpAs(t, a, &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "lead", "kill_pane", map[string]any{"pane_id": "marked"}); !failed {
		t.Fatalf("real lead closed a marker-origin pane: %q", text)
	}
	if text, failed := mcpAs(t, a, &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, FromEnv: true}, "lead", "kill_pane", map[string]any{"pane_id": "marked"}); !failed {
		t.Fatalf("claimant closed a marker-origin pane: %q", text)
	}
	if _, ok := a.mgr.Get("marked"); !ok {
		t.Fatal("marker-origin pane was closed")
	}
}

func TestEnvClaimScheduleOriginRecordsNoPane(t *testing.T) {
	a := traceTestServer(t)
	a.scheds = newTestScheduleStore(t)
	claimsLead := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, FromEnv: true}
	if text, failed := mcpAs(t, a, claimsLead, "lead", "create_schedule", map[string]any{"pane_id": "social", "prompt": "nightly", "cron": "0 9 * * *"}); failed {
		t.Fatalf("create_schedule: %q", text)
	}
	list := a.scheds.List()
	if len(list) != 1 {
		t.Fatalf("schedules = %+v", list)
	}
	if o := list[0].CreatedBy; o == nil || o.PaneID != "" || o.PaneTitle != "" || strings.Contains(o.PaneID, "claimed:") {
		t.Fatalf("createdBy from a FromEnv caller = %+v, want no pane", o)
	}
	// A real traced caller is still recorded, for contrast.
	if _, failed := mcpAs(t, a, &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 7}, "", "create_schedule", map[string]any{"pane_id": "social", "prompt": "daily", "cron": "0 10 * * *"}); failed {
		t.Fatal("create_schedule from the real lead failed")
	}
	for _, s := range a.scheds.List() {
		if s.Prompt == "daily" && (s.CreatedBy == nil || s.CreatedBy.PaneID != "lead") {
			t.Fatalf("real caller's createdBy = %+v", s.CreatedBy)
		}
	}
}
