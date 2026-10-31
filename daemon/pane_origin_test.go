package main

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpawnPaneTitleNeverUsesPromptBoilerplate(t *testing.T) {
	origin := &PaneOrigin{Source: paneOriginAgent, ByPaneTitle: "#5 Claude"}
	if got := spawnPaneTitle("  Engine   deletion ", agentCodex, origin); got != "Engine deletion" {
		t.Fatalf("explicit title = %q", got)
	}
	if got := spawnPaneTitle("", agentCodex, origin); got != "Codex · from #5 Claude" {
		t.Fatalf("fallback title = %q, want the agent and its starter", got)
	}
	if got := spawnPaneTitle("", "", nil); got != "Terminal" {
		t.Fatalf("shell without a starter = %q", got)
	}
	long := strings.Repeat("x", 80)
	if got := spawnPaneTitle(long, agentCodex, origin); len([]rune(got)) > spawnTitleMaxRunes {
		t.Fatalf("title not clipped: %d runes", len([]rune(got)))
	}
}

func TestSpawnPurposeFallsBackToFirstPromptLine(t *testing.T) {
	if got := spawnPurpose("Delete the engine", "ignored"); got != "Delete the engine" {
		t.Fatalf("explicit purpose = %q", got)
	}
	if got := spawnPurpose("", "\n\n  Review the diff on branch x\nmore detail"); got != "Review the diff on branch x" {
		t.Fatalf("fallback purpose = %q", got)
	}
}

func TestAgentSpawnOriginFollowsTheLeadThroughNestedSpawns(t *testing.T) {
	now := time.UnixMilli(1_000)
	// An unattributed caller is an API pane, never presented as the user's.
	if o := agentSpawnOrigin(nil, "p", "Personal", now); o.Source != paneOriginAPI || o.ByPaneID != "" {
		t.Fatalf("unattributed origin = %+v", o)
	}
	lead := &Pane{ID: "pane-lead", Title: "claude", Agent: agentClaude, buffer: NewRingBuffer(64)}
	worker := &Pane{ID: "pane-worker", Title: "codex", Agent: agentCodex, buffer: NewRingBuffer(64)}
	worker.Origin = agentSpawnOrigin(lead, "Engine deletion", "Personal", now)
	if worker.Origin.Source != paneOriginAgent || worker.Origin.ByPaneID != "pane-lead" || worker.Origin.LeadPaneID != "pane-lead" {
		t.Fatalf("worker origin = %+v", worker.Origin)
	}
	sub := agentSpawnOrigin(worker, "Sub-task", "Personal", now)
	if sub.ByPaneID != "pane-worker" || sub.LeadPaneID != "pane-lead" {
		t.Fatalf("nested spawn must keep the original lead: %+v", sub)
	}
	if got := paneStartedBy(sub); !strings.HasPrefix(got, "agent: ") || !strings.Contains(got, "(lead ") || !strings.Contains(got, "Sub-task") {
		t.Fatalf("list_panes provenance = %q", got)
	}
	if paneStartedBy(&PaneOrigin{Source: paneOriginUser}) != "" || paneStartedBy(nil) != "" {
		t.Fatal("user-opened and legacy panes carry no provenance line")
	}
}

func TestCreatePaneOriginDistinguishesUserAPIAndRestore(t *testing.T) {
	now := time.UnixMilli(2_000)
	if o, _ := createPaneOrigin(createPaneReq{StartedBy: "user"}, "Personal", now); o.Source != paneOriginUser {
		t.Fatalf("app-opened pane = %+v", o)
	}
	if o, _ := createPaneOrigin(createPaneReq{}, "", now); o.Source != paneOriginAPI {
		t.Fatalf("unattributed HTTP pane must not be presented as the user's: %+v", o)
	}
	if o, _ := createPaneOrigin(createPaneReq{StartedBy: "agent"}, "", now); o.Source != paneOriginAPI {
		t.Fatalf("a client cannot claim agent origin over HTTP: %+v", o)
	}
	saved := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-lead", Purpose: "Engine deletion", At: 1}
	o, title := createPaneOrigin(createPaneReq{Restore: true, Origin: saved, TitleOverride: " Engine deletion "}, "", now)
	if o == nil || o.ByPaneID != "pane-lead" || o == saved || title != "Engine deletion" {
		t.Fatalf("restore must bring back a copy of the saved origin and name: %+v %q", o, title)
	}
}

func TestKillPaneLimitsAgentsToPanesTheyStarted(t *testing.T) {
	stranger := &Pane{ID: "pane-user", Title: "bash", buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginUser}}
	a := &apiServer{mgr: testManagerWithPane(stranger)}
	res := a.mcpToolKillPane(map[string]any{"pane_id": "pane-user"}, "pane-lead")
	if m, _ := res.(map[string]any); m["isError"] != true {
		t.Fatalf("an agent closed a pane it did not start: %v", res)
	}
	worker := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-worker", LeadPaneID: "pane-lead"}
	if !paneStartedByCaller(worker, "pane-worker") || !paneStartedByCaller(worker, "pane-lead") {
		t.Fatal("the starter and its lead may close the pane")
	}
	if paneStartedByCaller(worker, "pane-other") || paneStartedByCaller(&PaneOrigin{Source: paneOriginUser, ByPaneID: "x"}, "x") || paneStartedByCaller(nil, "pane-lead") {
		t.Fatal("only agent-started panes count, and only for their starter or lead")
	}
}

func TestKillPaneGuardrailExceptions(t *testing.T) {
	user := &PaneOrigin{Source: paneOriginUser}
	worker := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-lead", LeadPaneID: "pane-lead"}
	a := &apiServer{}
	for _, tc := range []struct {
		name         string
		target       *PaneOrigin
		paneID, call string
		asked        bool
		want         paneAgentDecision
	}{
		{"someone else's pane", user, "pane-x", "pane-lead", false, paneAgentRefused},
		{"user asked: the person confirms", user, "pane-x", "pane-lead", true, paneAgentNeedsOK},
		{"no origin (older pane) is the person's", nil, "pane-old", "pane-lead", false, paneAgentRefused},
		{"no origin, user asked", nil, "pane-old", "pane-lead", true, paneAgentNeedsOK},
		{"own worker", worker, "pane-w", "pane-lead", false, paneAgentAllowed},
		{"itself", user, "pane-x", "pane-x", false, paneAgentAllowed},
		{"unattributed caller owns nothing", user, "pane-x", "", false, paneAgentRefused},
		{"unattributed caller, user asked", worker, "pane-w", "", true, paneAgentNeedsOK},
	} {
		if got := a.paneAgentAuthority(tc.target, tc.paneID, tc.call, tc.asked); got != tc.want {
			t.Fatalf("%s: decision=%v, want %v", tc.name, got, tc.want)
		}
	}
	if o, _ := createPaneOrigin(createPaneReq{Origin: &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-lead"}, StartedBy: "user"}, "", time.Now()); o.Source != paneOriginAgent {
		t.Fatalf("a retried pane must keep its saved origin: %+v", o)
	}
}

func TestRestoreRemapsStarterAndLeadToTheRestoredPanes(t *testing.T) {
	lead := &Pane{ID: "pane-new-lead", Status: PaneRunning, buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginUser}}
	worker := &Pane{ID: "pane-new-worker", Status: PaneRunning, buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-old-lead", LeadPaneID: "pane-old-lead"}}
	other := &Pane{ID: "pane-live", Status: PaneRunning, buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-new-lead", LeadPaneID: "pane-new-lead"}}
	m := testManagerWithPane(lead)
	m.panes[worker.ID] = worker
	m.panes[other.ID] = other
	a := &apiServer{mgr: m}
	body := `{"remap":{"pane-old-lead":"pane-new-lead","pane-new-lead":"pane-new-worker","pane-gone":"pane-missing"}}`
	req := httptest.NewRequest("POST", "/api/pane-origins/remap", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.remapPaneOrigins(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"updated":1`) {
		t.Fatalf("remap = %d %s", rec.Code, rec.Body.String())
	}
	if o := worker.Snapshot().Origin; o.ByPaneID != "pane-new-lead" || o.LeadPaneID != "pane-new-lead" {
		t.Fatalf("worker origin = %+v", o)
	}
	// A live old id is never rewritten, so a running lead keeps its workers.
	if o := other.Snapshot().Origin; o.LeadPaneID != "pane-new-lead" {
		t.Fatalf("live lead was remapped: %+v", o)
	}
	if !paneStartedByCaller(worker.Snapshot().Origin, "pane-new-lead") {
		t.Fatal("after a restore the lead may close its workers again")
	}
}

func TestRestoreRemapsExitedStartersAndLaterRestores(t *testing.T) {
	exited := &Pane{ID: "pane-old-lead", Status: PaneExited, buffer: NewRingBuffer(64)}
	lead := &Pane{ID: "pane-new-lead", Status: PaneRunning, buffer: NewRingBuffer(64)}
	worker := &Pane{ID: "pane-w", Status: PaneRunning, buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-old-lead", LeadPaneID: "pane-old-lead"}}
	m := testManagerWithPane(exited)
	m.panes[lead.ID], m.panes[worker.ID] = lead, worker
	a := &apiServer{mgr: m, originAliases: newPaneOriginAliases(t.TempDir())}
	rec := httptest.NewRecorder()
	a.remapPaneOrigins(rec, httptest.NewRequest("POST", "/api/pane-origins/remap", strings.NewReader(`{"remap":{"pane-old-lead":"pane-new-lead"}}`)))
	if o := worker.Snapshot().Origin; o.LeadPaneID != "pane-new-lead" {
		t.Fatalf("an exited starter must be remapped: %+v (%s)", o, rec.Body.String())
	}
	// A worker restored later, from another workspace, still finds its lead.
	later := a.originAliases.resolveOrigin(&PaneOrigin{Source: paneOriginAgent, ByPaneID: "pane-old-lead", LeadPaneID: "pane-old-lead"})
	if later.ByPaneID != "pane-new-lead" || later.LeadPaneID != "pane-new-lead" {
		t.Fatalf("later restore = %+v", later)
	}
	// Restored twice: the newest id wins, and the map survives a restart.
	a.originAliases.add(map[string]string{"pane-new-lead": "pane-newer-lead"}, time.Now())
	reloaded := newPaneOriginAliases(filepath.Dir(a.originAliases.path))
	if got := reloaded.resolve("pane-old-lead"); got != "pane-newer-lead" {
		t.Fatalf("resolve after reload = %q", got)
	}
}

func TestOriginAliasesFollowManyRestoresAndStopOnLoops(t *testing.T) {
	s := newPaneOriginAliases(t.TempDir())
	prev := "pane-0"
	for i := 1; i <= 12; i++ {
		next := fmt.Sprintf("pane-%d", i)
		s.add(map[string]string{prev: next}, time.UnixMilli(int64(i)))
		prev = next
	}
	if got := s.resolve("pane-0"); got != "pane-12" {
		t.Fatalf("after 12 restores pane-0 resolves to %q", got)
	}
	s.m["loop-a"] = paneOriginAlias{To: "loop-b"}
	s.m["loop-b"] = paneOriginAlias{To: "loop-a"}
	if got := s.resolve("loop-a"); got != "loop-a" && got != "loop-b" {
		t.Fatalf("a loop must stop: %q", got)
	}
}
