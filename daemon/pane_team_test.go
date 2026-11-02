package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spawnedOrigin(t *testing.T, a *apiServer, args map[string]any) (*PaneOrigin, string) {
	t.Helper()
	base := map[string]any{"workspace": "Work", "cwd": t.TempDir(), "wait_ready": false}
	for k, v := range args {
		base[k] = v
	}
	text, isErr := callMCPSpawnPane(t, a, "lead", base)
	if isErr {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(text)
	}
	p, ok := a.mgr.Get(out["id"].(string))
	if !ok {
		t.Fatalf("spawned pane missing: %s", text)
	}
	return p.Snapshot().Origin, ""
}

func TestSpawnPaneStartsLeadsAndDefaultsToWorkers(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	o, errText := spawnedOrigin(t, a, map[string]any{"cmd": "claude"})
	if errText != "" || o.TeamKind != paneTeamWorker || o.Worker == nil || !o.Worker.Plain || o.Worker.State != "running" || o.Worker.Provider != agentClaude || workerHoldsCredentials(o.Worker) {
		t.Fatalf("plain claude spawn is not a lifecycle worker: %+v %s", o, errText)
	}
	if o, _ = spawnedOrigin(t, a, map[string]any{"cmd": "codex"}); o.Worker == nil || o.Worker.Provider != agentCodex {
		t.Fatalf("codex worker record: %+v", o)
	}
	// A shell is a worker for the tree, but has no transcript to finish with.
	if o, _ = spawnedOrigin(t, a, map[string]any{"cmd": "bash"}); o.TeamKind != paneTeamWorker || o.Worker != nil {
		t.Fatalf("shell worker: %+v", o)
	}
	o, errText = spawnedOrigin(t, a, map[string]any{"cmd": "claude", "lead": " @Social "})
	if errText != "" || o.TeamKind != paneTeamLead || o.LeadRole != "Social" || o.Worker != nil || o.ByPaneID != "lead" {
		t.Fatalf("lead spawn: %+v %s", o, errText)
	}
	if _, errText = spawnedOrigin(t, a, map[string]any{"cmd": "claude", "lead": strings.Repeat("x", 33)}); !strings.Contains(errText, "lead:") {
		t.Fatalf("overlong role accepted: %q", errText)
	}
	if _, errText = spawnedOrigin(t, a, map[string]any{"cmd": "auto", "lead": "qa"}); !strings.Contains(errText, "auto worker is a worker") {
		t.Fatalf("auto lead accepted: %q", errText)
	}
	// Plain workers join the worker tick without any credential recovery.
	old := recoverQuotaWorker
	recoverQuotaWorker = func(*apiServer, *Pane, workerInfo) (func(), error) {
		t.Fatal("plain worker took the credential recovery path")
		return nil, nil
	}
	t.Cleanup(func() { recoverQuotaWorker = old })
	a.tickWorkers(time.Now())
}

func TestSubLeadsStayInTheirChainAndWorkersReportToTheNearestLead(t *testing.T) {
	now := time.UnixMilli(1_000)
	cmo := &Pane{ID: "cmo", Title: "CMO", Agent: agentClaude, buffer: NewRingBuffer(64), Origin: &PaneOrigin{Source: paneOriginUser}}
	sub := &Pane{ID: "social", Title: "Social", Agent: agentClaude, buffer: NewRingBuffer(64)}
	sub.Origin = agentSpawnOrigin(cmo, "", "W", now)
	sub.Origin.TeamKind, sub.Origin.LeadRole = paneTeamLead, "social"
	worker := &Pane{ID: "w", Title: "w", Agent: agentClaude, buffer: NewRingBuffer(64)}
	worker.Origin = agentSpawnOrigin(sub, "", "W", now)
	worker.Origin.TeamKind = paneTeamWorker
	// A sub-lead never starts a chain of its own: spend caps and close
	// authority stay with the pane at the top.
	if worker.Origin.ByPaneID != "social" || worker.Origin.LeadPaneID != "cmo" {
		t.Fatalf("a sub-lead's worker must stay in the chain: %+v", worker.Origin)
	}
	if o := agentSpawnOrigin(worker, "", "W", now); o.ByPaneID != "w" || o.LeadPaneID != "cmo" {
		t.Fatalf("nested worker: %+v", o)
	}
	// Reading and flagging go to the nearest running pane that leads workers.
	a := &apiServer{mgr: NewManager(t.TempDir())}
	for _, p := range []*Pane{transactionTestPane("cmo"), transactionTestPane("social"), transactionTestPane("w")} {
		a.mgr.panes[p.ID] = p
	}
	a.mgr.panes["cmo"].Origin = cmo.Origin
	a.mgr.panes["social"].Origin = sub.Origin
	a.mgr.panes["w"].Origin = worker.Origin
	subWorker := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "w", LeadPaneID: "cmo"}
	if got := a.workerLeadFor(worker.Origin); got != "social" {
		t.Fatalf("a sub-lead's worker reports to %q", got)
	}
	if got := a.workerLeadFor(subWorker); got != "social" {
		t.Fatalf("a worker's worker reports to %q, want its nearest lead", got)
	}
	a.mgr.panes["social"].Status = PaneExited
	if got := a.workerLeadFor(subWorker); got != "cmo" {
		t.Fatalf("with the sub-lead gone, report to the top: %q", got)
	}
	a.mgr.panes["social"].Status = PaneRunning
	a.mgr.panes["social"].Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "cmo", LeadPaneID: "cmo"}
	if got := a.workerLeadFor(worker.Origin); got != "cmo" {
		t.Fatalf("a pane with no kind passes its workers on (as before kinds): %q", got)
	}
}

func TestSubLeadsDoNotMultiplyTheWorkerCap(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	q := a.workers()
	q.mu.Lock()
	a.loadWorkerSettingsLocked(q)
	limit := q.settings.MaxRunning
	q.mu.Unlock()
	// Leads started from the user's pane, each starting its own workers.
	var leads []string
	for i := 0; i < 3; i++ {
		o, errText := spawnedOrigin(t, a, map[string]any{"cmd": "claude", "lead": fmt.Sprintf("role-%d", i)})
		if errText != "" || o.LeadPaneID != "lead" {
			t.Fatalf("sub-lead: %+v %s", o, errText)
		}
	}
	for _, p := range a.mgr.List() {
		if o := p.Snapshot().Origin; o != nil && paneTeamKind(o) == paneTeamLead {
			leads = append(leads, p.ID)
		}
	}
	for _, sub := range leads {
		for i := 0; i < limit; i++ {
			text, isErr := callMCPSpawnPane(t, a, sub, map[string]any{"cmd": "claude", "workspace": "Work", "cwd": t.TempDir(), "wait_ready": false})
			if isErr {
				t.Fatal(text)
			}
		}
	}
	for _, p := range a.mgr.List() {
		if o := p.Snapshot().Origin; o != nil && o.Source == paneOriginAgent && o.LeadPaneID != "lead" {
			t.Fatalf("%s left the chain: %+v", p.ID, o)
		}
	}
	q.mu.Lock()
	running := a.workerRunningLocked(q, "lead")
	q.mu.Unlock()
	if running < limit {
		t.Fatalf("sub-lead workers are not counted against the top lead: %d", running)
	}
}

func TestPaneKindAuthority(t *testing.T) {
	a := &apiServer{mgr: NewManager(t.TempDir())}
	agentPane := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "starter", LeadPaneID: "lead"}
	cases := []struct {
		name   string
		origin *PaneOrigin
		pane   string
		caller string
		asked  bool
		want   paneAgentDecision
	}{
		{"starter", agentPane, "w", "starter", false, paneAgentAllowed},
		{"lead", agentPane, "w", "lead", false, paneAgentAllowed},
		{"self", agentPane, "w", "w", false, paneAgentRefused},
		{"self asked", agentPane, "w", "w", true, paneAgentNeedsOK},
		{"stranger", agentPane, "w", "x", false, paneAgentRefused},
		{"stranger asked", agentPane, "w", "x", true, paneAgentNeedsOK},
		{"user pane", &PaneOrigin{Source: paneOriginUser}, "cmo", "x", true, paneAgentNeedsOK},
		{"user pane self", &PaneOrigin{Source: paneOriginUser}, "cmo", "cmo", true, paneAgentNeedsOK},
		{"no origin", nil, "old", "x", true, paneAgentNeedsOK},
		{"schedule", &PaneOrigin{Source: paneOriginSchedule}, "s", "x", true, paneAgentNeedsOK},
	}
	for _, c := range cases {
		if got, _ := a.paneTeamAgentAuthority(c.origin, c.pane, c.caller, paneTeamLead, c.asked); got != c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
		if c.origin != nil && c.origin.Source == paneOriginAgent {
			if got, _ := a.paneTeamAgentAuthority(c.origin, c.pane, c.caller, paneTeamWorker, c.asked); got != c.want {
				t.Errorf("%s worker: %v want %v", c.name, got, c.want)
			}
		}
	}
	// Standing agent authority does not skip the person's OK for a kind change.
	a.agentAuthority = grantedAgentAuthority(t)
	if got, _ := a.paneTeamAgentAuthority(agentPane, "w", "x", paneTeamLead, true); got != paneAgentNeedsOK {
		t.Fatalf("agent authority bypassed the kind confirmation: %v", got)
	}
}

func TestUserPaneKindAuthority(t *testing.T) {
	a := &apiServer{agentAuthority: grantedAgentAuthority(t)}
	for _, origin := range []*PaneOrigin{nil, {}, {Source: paneOriginUser}, {Source: paneOriginSchedule}, {Source: paneOriginAPI}} {
		for _, caller := range []string{"", "self", "other"} {
			for _, asked := range []bool{false, true} {
				for _, kind := range []string{paneTeamLead, paneTeamWorker, ""} {
					want := paneAgentRefused
					if asked && kind == paneTeamLead {
						want = paneAgentNeedsOK
					}
					got, why := a.paneTeamAgentAuthority(origin, "self", caller, kind, asked)
					if got != want {
						t.Errorf("origin=%+v caller=%q asked=%v kind=%q: %v want %v", origin, caller, asked, kind, got, want)
					}
					if kind == paneTeamLead && !asked && !strings.Contains(why, "user_requested: true") {
						t.Errorf("missing promotion hint: %q", why)
					}
				}
			}
		}
	}
}

func TestSetPaneInfoUserPaneLeadNeedsApproval(t *testing.T) {
	shortenPaneConfirmWait(t, time.Millisecond)
	for _, source := range []string{"legacy", paneOriginUser} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allow=%v", source, allow), func(t *testing.T) {
				a := confirmTestServer(t)
				a.agentAuthority = grantedAgentAuthority(t)
				p, _ := a.mgr.Get("social")
				if source != "legacy" {
					p.Origin = &PaneOrigin{Source: source}
				}
				// Invalid roles and requests outside the narrow promotion path
				// neither mutate the pane nor raise a confirmation.
				for _, args := range []map[string]any{
					{"kind": "lead", "lead_role": "security"},
					{"kind": "worker", "user_requested": true},
					{"kind": "", "user_requested": true},
					{"kind": "lead", "user_requested": true},
					{"kind": "lead", "lead_role": strings.Repeat("x", 33), "user_requested": true},
				} {
					args["pane_id"] = p.ID
					text, bad := mcpResultText(t, a.mcpToolSetPaneInfo(args, "lead"))
					if !bad || len(a.pendingPaneConfirms()) != 0 || paneTeamKind(p.Snapshot().Origin) != "" {
						t.Fatalf("invalid promotion %v: %s", args, text)
					}
					if args["lead_role"] == "security" && !strings.Contains(text, "user_requested: true") {
						t.Fatalf("missing hint: %s", text)
					}
				}
				text, bad := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{
					"pane_id": p.ID, "kind": "lead", "lead_role": " @security ", "user_requested": true,
				}, "lead"))
				if bad || !strings.Contains(text, "waiting for your OK") || paneTeamKind(p.Snapshot().Origin) != "" {
					t.Fatalf("promotion did not wait for approval: %s", text)
				}
				view := onlyPendingConfirm(t, a)
				if view.PaneID != p.ID || view.Title != "Change pane #3 “Social loop”?" || view.Message != "#1 Orchestrator wants to make it a lead (role “security”): pane #3 “Social loop”." {
					t.Fatalf("approval must identify pane and role: %+v", view)
				}
				if rec := answerPaneConfirm(t, a, view.ID, `{"allow":true}`, callerPane); rec.Code != http.StatusForbidden {
					t.Fatalf("agent approved its own promotion: %d", rec.Code)
				}
				if rec := answerPaneConfirm(t, a, view.ID, fmt.Sprintf(`{"allow":%t}`, allow), callerHuman); rec.Code != http.StatusOK {
					t.Fatalf("person's answer: %d %s", rec.Code, rec.Body.String())
				}
				origin := p.Snapshot().Origin
				if !allow {
					if paneTeamKind(origin) != "" {
						t.Fatalf("denied promotion applied: %+v", origin)
					}
					return
				}
				if origin.TeamKind != paneTeamLead || origin.LeadRole != "security" || origin.Source == paneOriginAgent || origin.Worker != nil {
					t.Fatalf("approved promotion: %+v", origin)
				}
				// Promotion preserves the user's ownership and its restrictions.
				for _, kind := range []string{paneTeamWorker, ""} {
					text, bad = mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": p.ID, "kind": kind, "user_requested": true}, "lead"))
					if !bad || len(a.pendingPaneConfirms()) != 0 || paneTeamKind(p.Snapshot().Origin) != paneTeamLead {
						t.Fatalf("user lead changed to %q: %s", kind, text)
					}
				}
			})
		}
	}
}

func TestSetPaneInfoKindByStarter(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	if _, errText := spawnedOrigin(t, a, map[string]any{"cmd": "claude"}); errText != "" {
		t.Fatal(errText)
	}
	a.tickWorkers(time.Now())
	var child *Pane
	for _, p := range a.mgr.List() {
		if p.ID != "lead" {
			child = p
		}
	}
	workerID := child.Snapshot().Origin.Worker.ID
	text, bad := mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": child.ID, "kind": "lead", "lead_role": "qa"}, "lead"))
	if bad || !strings.Contains(text, "is now a lead") {
		t.Fatal(text)
	}
	o := child.Snapshot().Origin
	if o.TeamKind != paneTeamLead || o.LeadRole != "qa" || o.Worker != nil {
		t.Fatalf("promotion: %+v", o)
	}
	// The dropped plain record leaves the queue and the tick can't restore it.
	a.tickWorkers(time.Now())
	if child.Snapshot().Origin.Worker != nil {
		t.Fatal("tick restored the plain worker record of a lead")
	}
	q := a.workers()
	q.mu.Lock()
	for _, r := range q.requests {
		if r.Info.ID == workerID && r.Info.State != "closed" {
			t.Errorf("queue entry still %s", r.Info.State)
		}
	}
	q.mu.Unlock()
	// The pane itself can't demote itself without the person.
	text, bad = mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"kind": "worker"}, child.ID))
	if !bad || !strings.Contains(text, "can't change its own kind") {
		t.Fatal(text)
	}
	text, bad = mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": child.ID, "kind": "worker"}, "lead"))
	if bad {
		t.Fatal(text)
	}
	if o = child.Snapshot().Origin; o.TeamKind != paneTeamWorker || o.LeadRole != "" || o.Worker == nil || !o.Worker.Plain {
		t.Fatalf("demotion: %+v", o)
	}
	for _, args := range []map[string]any{
		{"pane_id": child.ID, "kind": "lead"},
		{"pane_id": child.ID, "kind": "boss"},
		{"pane_id": child.ID, "kind": "worker", "lead_role": "x"},
		{"pane_id": child.ID, "kind": "lead", "lead_role": "x", "handle": "x"},
		{"pane_id": child.ID, "lead_role": "x"},
	} {
		if text, bad = mcpResultText(t, a.mcpToolSetPaneInfo(args, "lead")); !bad {
			t.Errorf("%v accepted: %s", args, text)
		}
	}
	// Without a user request, an agent may only explain how to ask for promotion.
	text, bad = mcpResultText(t, a.mcpToolSetPaneInfo(map[string]any{"pane_id": "lead", "kind": "lead", "lead_role": "cmo"}, child.ID))
	if !bad || !strings.Contains(text, "user_requested: true") {
		t.Fatal(text)
	}
}

func TestPaneKindHTTPIsForPeople(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/panes/{id}/team", a.setPaneTeamHTTP)
	post := func(id, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/panes/"+id+"/team", strings.NewReader(body)))
		return w
	}
	old := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = old })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "lead"} }
	if w := post("lead", `{"kind":"lead","role":"cmo"}`); w.Code != http.StatusForbidden {
		t.Fatalf("agent used the person's route: %d %s", w.Code, w.Body.String())
	}
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if w := post("lead", `{"kind":"lead","role":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("lead without role: %d", w.Code)
	}
	if w := post("lead", `{"kind":"worker"}`); w.Code != http.StatusConflict {
		t.Fatalf("user pane made a worker: %d %s", w.Code, w.Body.String())
	}
	if w := post("lead", `{"kind":"lead","role":"cmo"}`); w.Code != http.StatusOK {
		t.Fatalf("make lead: %d %s", w.Code, w.Body.String())
	}
	lead, _ := a.mgr.Get("lead")
	if o := lead.Snapshot().Origin; paneTeamKind(o) != paneTeamLead || o.LeadRole != "cmo" {
		t.Fatalf("user pane lead: %+v", o)
	}
	if w := post("lead", `{"kind":"none"}`); w.Code != http.StatusOK || paneTeamKind(lead.Snapshot().Origin) != "" {
		t.Fatalf("remove lead: %d %+v", w.Code, lead.Snapshot().Origin)
	}
	// A legacy agent pane with no kind becomes a worker with a lifecycle record.
	legacy := transactionTestPane("legacy")
	legacy.Cmd, legacy.Agent = []string{"claude"}, agentClaude
	legacy.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead"}
	a.mgr.panes[legacy.ID] = legacy
	if w := post("legacy", `{"kind":"worker"}`); w.Code != http.StatusOK {
		t.Fatalf("make worker: %d %s", w.Code, w.Body.String())
	}
	if o := legacy.Snapshot().Origin; o.TeamKind != paneTeamWorker || o.Worker == nil || !o.Worker.Plain || o.Worker.Provider != agentClaude {
		t.Fatalf("legacy worker: %+v", o)
	}
	if w := post("legacy", `{"kind":"none"}`); w.Code != http.StatusConflict {
		t.Fatalf("agent pane lost its kind: %d", w.Code)
	}
	// A failed write keeps the previous kind.
	a.panes.(*failingPaneStore).putErr = errors.New("disk full")
	if w := post("legacy", `{"kind":"lead","role":"qa"}`); w.Code != http.StatusInternalServerError || paneTeamKind(legacy.Snapshot().Origin) != paneTeamWorker {
		t.Fatalf("unpersisted change applied: %d %+v", w.Code, legacy.Snapshot().Origin)
	}
}

func TestRestoredKindComesOnlyFromTheDaemonStore(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	forged := &PaneOrigin{Source: paneOriginAgent, ByPaneID: "x", TeamKind: paneTeamLead, LeadRole: "root", Worker: &workerInfo{ID: "w1", Provider: "claude", Plain: true}}
	for _, restore := range []bool{true, false} {
		got := a.validatedWorkerOrigin(forged, createPaneReq{Restore: restore, ClaudeSessionID: "s1"})
		if got.TeamKind != "" || got.LeadRole != "" || got.Worker != nil {
			t.Fatalf("client-supplied kind survived (restore=%v): %+v", restore, got)
		}
	}
	if forged.TeamKind != paneTeamLead {
		t.Fatal("mutated supplied metadata")
	}
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	older := transactionTestPane("older")
	older.ClaudeSessionID, older.CreatedAt = "s1", time.Now().Add(-time.Hour)
	older.Origin = &PaneOrigin{Source: paneOriginAgent}
	newer := transactionTestPane("newer")
	newer.ClaudeSessionID = "s1"
	newer.Origin = &PaneOrigin{Source: paneOriginAgent, TeamKind: paneTeamWorker, Worker: &workerInfo{ID: "w-old", Provider: "claude", Plain: true, State: "finished"}}
	for _, p := range []*Pane{older, newer} {
		if err := store.Put(p); err != nil {
			t.Fatal(err)
		}
	}
	got := a.validatedWorkerOrigin(&PaneOrigin{Source: paneOriginAgent, TeamKind: paneTeamLead, LeadRole: "root"}, createPaneReq{Restore: true, ClaudeSessionID: "s1"})
	if got.TeamKind != paneTeamWorker || got.LeadRole != "" || got.Worker == nil || got.Worker.ID == "w-old" || got.Worker.State != "running" || workerHoldsCredentials(got.Worker) {
		t.Fatalf("restore did not take the newest daemon kind: %+v %+v", got, got.Worker)
	}
	if got = a.validatedWorkerOrigin(forged, createPaneReq{Restore: true, ClaudeSessionID: "other"}); got.TeamKind != "" || got.Worker != nil {
		t.Fatalf("another conversation's kind: %+v", got)
	}
	// A claimed plain worker without a saved record restores as a plain pane;
	// a claimed account worker still needs its saved record.
	raw, _ := json.Marshal(createPaneReq{Restore: true, Cmd: []string{"claude"}, ClaudeSessionID: "gone", Origin: forged, Workspace: "Work", Cwd: t.TempDir()})
	w := httptest.NewRecorder()
	a.createPane(w, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(string(raw))))
	if strings.Contains(w.Body.String(), "matching daemon-saved conversation") || strings.Contains(w.Body.String(), "human_required") {
		t.Fatalf("plain worker restore gated as an account worker: %d %s", w.Code, w.Body.String())
	}
	for _, w := range []*workerInfo{
		{Plain: true, Account: "a"}, {Plain: true, ConfigDir: "/x"}, {Plain: true, TeamID: "t"}, {Plain: true, LeaseID: "l"}, {Plain: true, CredentialRevision: "r"}, {Account: ""},
	} {
		if !workerHoldsCredentials(w) {
			t.Errorf("%+v treated as plain", w)
		}
	}
}

func TestOnlyWorkersAreArchived(t *testing.T) {
	for _, kind := range []string{"plain-worker", "legacy-auto-worker", "lead", "user"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			p.mu.Lock()
			o := p.Origin.clone()
			switch kind {
			case "plain-worker":
				o.TeamKind = paneTeamWorker
				o.Worker = &workerInfo{ID: o.Worker.ID, State: "finished", Provider: "claude", Plain: true}
			case "legacy-auto-worker":
				o.TeamKind = ""
			case "lead":
				o.TeamKind, o.LeadRole = paneTeamLead, "qa"
			case "user":
				o.Source, o.TeamKind = paneOriginUser, ""
			}
			p.Origin = o
			p.mu.Unlock()
			a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			a.tryArchiveWorker(p, now)
			_, live := a.mgr.Get(p.ID)
			wantArchived := kind == "plain-worker" || kind == "legacy-auto-worker"
			if live == wantArchived {
				t.Fatalf("live=%v, want archived=%v", live, wantArchived)
			}
			if lead, _ := a.mgr.Get("lead"); !wantArchived && lead.Snapshot().Flagged {
				t.Fatal("a pane that is never archived flagged its lead")
			}
		})
	}
}

func TestKindChangesTrackWorkersAfreshAndRaceArchivingSafely(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	if _, errText := spawnedOrigin(t, a, map[string]any{"cmd": "claude"}); errText != "" {
		t.Fatal(errText)
	}
	a.tickWorkers(time.Now())
	var child *Pane
	for _, p := range a.mgr.List() {
		if p.ID != "lead" {
			child = p
		}
	}
	tracked := func() []string {
		q := a.workers()
		q.mu.Lock()
		defer q.mu.Unlock()
		var out []string
		for _, r := range q.requests {
			if r.PaneID == child.ID {
				out = append(out, r.Info.ID+":"+r.Info.State)
			}
		}
		return out
	}
	first := child.Snapshot().Origin.Worker.ID
	if got := tracked(); len(got) != 1 || got[0] != first+":running" {
		t.Fatalf("plain worker not tracked: %v", got)
	}
	if _, err := a.setPaneTeam(child, paneTeamLead, "qa", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := tracked(); len(got) != 0 {
		t.Fatalf("a lead kept its queue entry: %v", got)
	}
	if _, err := a.setPaneTeam(child, paneTeamWorker, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	a.tickWorkers(time.Now())
	second := child.Snapshot().Origin.Worker.ID
	if got := tracked(); second == first || len(got) != 1 || got[0] != second+":running" {
		t.Fatalf("a worker again is not tracked afresh: %v (first %s, second %s)", got, first, second)
	}
	child.workerArchiving.Store(true)
	if _, err := a.setPaneTeam(child, paneTeamLead, "qa", time.Now()); err == nil || paneTeamKind(child.Snapshot().Origin) != paneTeamWorker {
		t.Fatal("a worker being archived changed kind")
	}
	child.workerArchiving.Store(false)
}

func TestPromotionDuringArchivePreparationKeepsTheLead(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.workerArchives().beforeCommit = func() {
		if _, err := a.setPaneTeam(p, paneTeamLead, "qa", now); err != nil {
			t.Errorf("promotion before commit: %v", err)
		}
	}
	a.tryArchiveWorker(p, now)
	if _, live := a.mgr.Get(p.ID); !live || len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("a pane promoted to lead mid-archive was archived")
	}
}

func TestRestoredPlainWorkerNeverSharesItsRecord(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	saved := transactionTestPane("saved")
	saved.ClaudeSessionID = "s1"
	saved.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead", TeamKind: paneTeamWorker, Worker: &workerInfo{ID: "w1", Provider: "claude", Plain: true, State: "finished"}}
	if err := store.Put(saved); err != nil {
		t.Fatal(err)
	}
	got := a.validatedWorkerOrigin(saved.Origin, createPaneReq{Restore: true, ClaudeSessionID: "s1"})
	if got.TeamKind != paneTeamWorker || got.Worker == nil || got.Worker.ID == "w1" || got.Worker.State != "running" || !got.Worker.Plain || got.LeadPaneID != "lead" {
		t.Fatalf("restored plain worker: %+v %+v", got, got.Worker)
	}
}

func TestKeptWorkerFlagsItsLeadOncePerReason(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.tryArchiveWorker(p, now) // unread
	lead, _ := a.mgr.Get("lead")
	if !lead.Snapshot().Flagged {
		t.Fatal("unread result did not flag the lead")
	}
	// The person clears the flag; nothing about the worker changed.
	lead.mu.Lock()
	lead.Flagged, lead.FlagMemo, lead.FlagOwner = false, "", ""
	lead.mu.Unlock()
	s := a.workerArchives()
	s.mu.Lock()
	s.checked = map[string]time.Time{}
	s.mu.Unlock()
	a.tryArchiveWorker(p, now)
	if lead.Snapshot().Flagged {
		t.Fatal("the lead was flagged again for the same unread result")
	}
	// A new reason flags again.
	putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "changed")
	a.tryArchiveWorker(p, now)
	if !lead.Snapshot().Flagged || !strings.Contains(lead.Snapshot().FlagMemo, "uncommitted") {
		t.Fatalf("a new reason did not flag the lead: %+v", lead.Snapshot().FlagMemo)
	}
}

func TestLeadRolesRejectInvisibleCharacters(t *testing.T) {
	for _, bad := range []string{"se​curity", "qa‮", "x⁦y", "a\tb\x00"} {
		if _, err := normalizeLeadRole(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if r, err := normalizeLeadRole("Sécurité 2"); err != nil || r != "Sécurité 2" {
		t.Fatalf("plain unicode role: %q %v", r, err)
	}
}

func TestRestoreFindsTheKindWithoutAnySavedKindOrOrigin(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	saved := transactionTestPane("saved")
	saved.ClaudeSessionID = "s1"
	saved.Origin = &PaneOrigin{Source: paneOriginUser, TeamKind: paneTeamLead, LeadRole: "cmo", At: 5}
	if err := store.Put(saved); err != nil {
		t.Fatal(err)
	}
	for _, supplied := range []*PaneOrigin{nil, {Source: paneOriginUser, At: 5}} {
		got := a.validatedWorkerOrigin(supplied, createPaneReq{Restore: true, ClaudeSessionID: "s1"})
		if got == nil || got.TeamKind != paneTeamLead || got.LeadRole != "cmo" || got.Source != paneOriginUser {
			t.Fatalf("restore from %+v lost the kind: %+v", supplied, got)
		}
	}
	if got := a.validatedWorkerOrigin(nil, createPaneReq{ClaudeSessionID: "s1"}); got != nil {
		t.Fatalf("a non-restore gained an origin: %+v", got)
	}
}

func TestWorkerTickPrunesOldFinishedEntries(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	q := a.workers()
	q.mu.Lock()
	old := time.Now().Add(-25 * time.Hour)
	q.requests = append(q.requests,
		&workerRequest{Info: workerInfo{ID: "old-closed", State: "closed", Plain: true}, Created: old},
		&workerRequest{Info: workerInfo{ID: "new-closed", State: "closed", Plain: true}, Created: time.Now()},
		&workerRequest{Info: workerInfo{ID: "old-queued", State: "queued"}, Created: old})
	q.mu.Unlock()
	a.tickWorkers(time.Now())
	q.mu.Lock()
	var ids []string
	for _, r := range q.requests {
		ids = append(ids, r.Info.ID)
	}
	q.mu.Unlock()
	if strings.Contains(strings.Join(ids, ","), "old-closed") || !strings.Contains(strings.Join(ids, ","), "new-closed") || !strings.Contains(strings.Join(ids, ","), "old-queued") {
		t.Fatalf("queue after tick: %v", ids)
	}
}

func TestKindEdgesFromTheThirdBotReview(t *testing.T) {
	// An auto worker started after its caller closed is an api pane: still a worker.
	if got := paneTeamKind(&PaneOrigin{Source: paneOriginAPI, Worker: &workerInfo{ID: "w"}}); got != paneTeamWorker {
		t.Fatalf("api auto worker kind = %q", got)
	}
	if got := paneTeamKind(&PaneOrigin{Source: paneOriginUser, Worker: &workerInfo{ID: "w"}}); got != "" {
		t.Fatalf("a user pane is never a worker: %q", got)
	}
	// A change to a pane that closed meanwhile is an error, not a silent success.
	a, _ := newQuotaWorkerAPI(t)
	gone := transactionTestPane("gone")
	gone.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", LeadPaneID: "lead"}
	if _, err := a.setPaneTeam(gone, paneTeamLead, "qa", time.Now()); err == nil {
		t.Fatal("changed a pane the manager no longer has")
	}
	// A saved worker kind never makes a user-sourced restore archivable.
	store, err := NewPaneStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.panes = store
	saved := transactionTestPane("saved")
	saved.ClaudeSessionID = "s1"
	saved.Origin = &PaneOrigin{Source: paneOriginAgent, ByPaneID: "lead", TeamKind: paneTeamWorker, Worker: &workerInfo{ID: "w1", Provider: "claude", Plain: true}}
	if err := store.Put(saved); err != nil {
		t.Fatal(err)
	}
	got := a.validatedWorkerOrigin(&PaneOrigin{Source: paneOriginUser}, createPaneReq{Restore: true, ClaudeSessionID: "s1"})
	if paneTeamKind(got) != "" || got.Worker != nil {
		t.Fatalf("user-sourced restore became a worker: %+v", got)
	}
}
