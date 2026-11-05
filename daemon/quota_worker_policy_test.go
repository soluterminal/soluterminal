package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotaWorkerLeadReservePolicy(t *testing.T) {
	now := time.Unix(10000, 0)
	settings := defaultWorkerSettings()
	shared := workerAccount{Provider: "claude", Account: "lead", Headroom: 60, Capabilities: map[string]bool{"role-guard": true}}
	other := workerAccount{Provider: "codex", Account: "other", Headroom: 40}
	for _, tc := range []struct {
		name                    string
		leads                   int
		other                   int
		prefer                  string
		guarded                 bool
		provider, account, rule string
	}{
		{"no lead selects headroom", 0, 40, "", false, "claude", "lead", "headroom"},
		{"one lead protects fifteen percent", 1, 50, "claude", false, "codex", "other", "reserve"},
		{"reserve stacks for each lead", 2, 40, "claude", false, "codex", "other", "reserve"},
		{"prefer is only a usable-headroom tie", 1, 45, "codex", false, "codex", "other", "reserve"},
		{"unreserved account beats protected slice", 4, 1, "claude", false, "codex", "other", "reserve"},
		{"reserve fallback when all others are exhausted", 4, 0, "", false, "claude", "lead", "reserve"},
		{"guarded route takes precedence over reserve", 4, 40, "codex", true, "claude", "lead", "guarded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alternative := other
			alternative.Headroom = tc.other
			got, err := pickWorkerWithPolicy([]workerAccount{shared, alternative}, tc.prefer, tc.guarded, map[workerAccountKey]int{shared.key(): tc.leads}, settings, now)
			if err != nil || got.Provider != tc.provider || got.Account != tc.account || !strings.HasPrefix(got.Reason, tc.rule+":") {
				t.Fatalf("got %+v, %v", got, err)
			}
			if tc.leads == 4 && got.Account == "lead" && !strings.Contains(got.Reason, "using lead reserve") {
				t.Fatal(got.Reason)
			}
		})
	}
	// Reserve falls back only when *all* eligible accounts have none left. It
	// chooses the largest remaining allowance, not the preferred task provider.
	got, err := pickWorkerWithPolicy([]workerAccount{{Provider: "claude", Account: "c", Headroom: 10}, {Provider: "codex", Account: "x", Headroom: 12}}, "claude", false, map[workerAccountKey]int{{Provider: "claude", Account: "c"}: 1, {Provider: "codex", Account: "x"}: 1}, settings, now)
	if err != nil || got.Account != "x" || !strings.Contains(got.Reason, "using lead reserve") {
		t.Fatalf("reserve fallback: %+v, %v", got, err)
	}
	// Reserve can be explicitly disabled; a same-named lead on another provider
	// or team must not spend this account's reserve.
	settings.WorkerLeadReservePct = 0
	got, err = pickWorkerWithPolicy([]workerAccount{shared, other}, "", false, map[workerAccountKey]int{shared.key(): 8}, settings, now)
	if err != nil || got.Account != "lead" {
		t.Fatalf("zero reserve: %+v, %v", got, err)
	}
}

func TestQuotaWorkerResetAwarePolicy(t *testing.T) {
	now := time.Unix(10000, 0)
	settings := defaultWorkerSettings()
	window := func(remaining int, after time.Duration) workerQuotaWindow {
		return workerQuotaWindow{Remaining: remaining, ResetEpoch: now.Add(after).Unix()}
	}
	for _, tc := range []struct {
		name     string
		windows  []workerQuotaWindow
		expected int
		provider string
		reason   string
	}{
		{"twenty percent resetting soon beats forty percent in four days", []workerQuotaWindow{window(20, 10*time.Minute)}, 45, "claude", "resets within"},
		{"nonresetting weekly window still constrains the choice", []workerQuotaWindow{window(20, 10*time.Minute), window(30, 4*24*time.Hour)}, 45, "codex", "headroom:"},
		{"both constraining windows reset during the run", []workerQuotaWindow{window(20, 10*time.Minute), window(30, 20*time.Minute)}, 45, "claude", "resets within"},
		{"expected run setting changes horizon", []workerQuotaWindow{window(20, 10*time.Minute)}, 5, "codex", "headroom:"},
		{"reset at horizon counts", []workerQuotaWindow{window(20, 45*time.Minute)}, 45, "claude", "resets within"},
		{"reset beyond horizon does not count", []workerQuotaWindow{window(20, 45*time.Minute+time.Second)}, 45, "codex", "headroom:"},
		{"unknown reset is not a refill", []workerQuotaWindow{{Remaining: 20}}, 45, "codex", "headroom:"},
		{"elapsed reset is not predicted again", []workerQuotaWindow{window(20, -time.Second)}, 45, "codex", "headroom:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := settings
			s.WorkerExpectedRunMinutes = tc.expected
			got, err := pickWorkerWithPolicy([]workerAccount{{Provider: "claude", Account: "soon", Headroom: 20, Windows: tc.windows}, {Provider: "codex", Account: "later", Headroom: 40, Windows: []workerQuotaWindow{window(40, 4*24*time.Hour)}}}, "codex", false, nil, s, now)
			if err != nil || got.Provider != tc.provider || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	// Predictions do not loosen existing quota-exhaustion or guarded-provider rules.
	exhausted := workerAccount{Provider: "claude", Account: "dry", Headroom: 0, Windows: []workerQuotaWindow{window(0, time.Minute)}}
	if _, err := pickWorkerWithPolicy([]workerAccount{exhausted}, "", false, nil, settings, now); err == nil {
		t.Fatal("future reset admitted an exhausted account")
	}
	got, err := pickWorkerWithPolicy([]workerAccount{{Provider: "claude", Account: "guard", Headroom: 10, Capabilities: map[string]bool{"role-guard": true}}, {Provider: "codex", Account: "soon", Headroom: 20, Windows: []workerQuotaWindow{window(20, time.Minute)}}}, "codex", true, nil, settings, now)
	if err != nil || got.Provider != "claude" || !strings.HasPrefix(got.Reason, "guarded:") {
		t.Fatalf("guard relaxed: %+v, %v", got, err)
	}
	// A reset prediction must not reduce a genuinely full measured window.
	score := scoreWorkerQuota(workerAccount{Headroom: 100, Windows: []workerQuotaWindow{window(100, time.Minute)}}, 0, settings, now)
	if score.usable != 100 {
		t.Fatalf("full window reduced: %+v", score)
	}
}

func TestQuotaWorkerResetReserveAndTieBreak(t *testing.T) {
	now := time.Unix(10000, 0)
	settings := defaultWorkerSettings()
	soon := workerAccount{Provider: "claude", Account: "soon", Headroom: 20, Windows: []workerQuotaWindow{{Remaining: 20, ResetEpoch: now.Add(10 * time.Minute).Unix()}}}
	other := workerAccount{Provider: "codex", Account: "other", Headroom: 90}
	got, err := pickWorkerWithPolicy([]workerAccount{soon, other}, "claude", false, map[workerAccountKey]int{soon.key(): 1}, settings, now)
	if err != nil || got.Account != "other" || !strings.HasPrefix(got.Reason, "reserve:") {
		t.Fatalf("reset-aware reserve: %+v, %v", got, err)
	}
	for _, prefer := range []string{"claude", "codex"} {
		got, err = pickWorkerWithPolicy([]workerAccount{{Provider: "claude", Account: "c", Headroom: 40}, {Provider: "codex", Account: "x", Headroom: 40}}, prefer, false, nil, settings, now)
		if err != nil || got.Provider != prefer || !strings.HasPrefix(got.Reason, "tie-break:") || !strings.Contains(got.Reason, "prefer "+prefer) {
			t.Fatalf("tie: %+v, %v", got, err)
		}
	}
}

func TestQuotaWorkerLeadIdentityCounts(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	add := func(id, provider, account, team string, status PaneStatus, origin *PaneOrigin, role string) {
		p := transactionTestPane(id)
		p.Cmd = []string{provider}
		p.Agent = provider
		p.Status = status
		p.Origin = origin
		p.Role = role
		p.Credential = &paneCredentialInfo{Provider: provider, Account: account, TeamID: team}
		a.mgr.panes[id] = p
	}
	add("claude-1", "claude", "same", "", PaneRunning, nil, "")
	add("claude-2", "claude", "same", "", PaneRunning, nil, "")
	add("codex", "codex", "same", "", PaneRunning, nil, "")
	add("exited", "claude", "same", "", PaneExited, nil, "")
	add("worker", "claude", "same", "", PaneRunning, &PaneOrigin{Source: paneOriginAgent, LeadPaneID: "claude-1"}, "")
	add("orchestrator", "claude", "same", "team-a", PaneRunning, &PaneOrigin{Source: paneOriginAgent, LeadPaneID: "claude-1"}, paneRoleOrchestrator)
	add("other-team", "claude", "same", "team-b", PaneRunning, nil, "")
	add("pinned", "codex", "global", "", PaneRunning, &PaneOrigin{Source: paneOriginAgent, Worker: &workerInfo{Provider: "codex", Account: "isolated"}}, paneRoleOrchestrator)
	got := a.workerLeadCounts()
	for key, want := range map[workerAccountKey]int{{Provider: "claude", Account: "same"}: 2, {Provider: "codex", Account: "same"}: 1, {Provider: "claude", Account: "team__same", TeamID: "team-a"}: 1, {Provider: "claude", Account: "team__same", TeamID: "team-b"}: 1, {Provider: "codex", Account: "isolated"}: 1} {
		if got[key] != want {
			t.Fatalf("%+v: got %d want %d; %+v", key, got[key], want, got)
		}
	}
	if len(got) != 5 {
		t.Fatalf("unexpected lead accounts: %+v", got)
	}
}

func TestQuotaWorkerPolicySettingsMigrationAndPatch(t *testing.T) {
	a := &apiServer{dataDir: t.TempDir()}
	path := filepath.Join(a.dataDir, "worker-settings.json")
	if err := os.WriteFile(path, []byte(`{"maxRunning":3,"finishAfterMinutes":8}`), 0600); err != nil {
		t.Fatal(err)
	}
	q := a.workers()
	q.mu.Lock()
	a.loadWorkerSettingsLocked(q)
	s := q.settings
	q.mu.Unlock()
	if s.MaxRunning != 3 || s.FinishAfterMinutes != 8 || s.WorkerLeadReservePct != 15 || s.WorkerExpectedRunMinutes != 45 || s.WorkerAccounts != "active" {
		t.Fatalf("migration lost defaults/settings: %+v", s)
	}
	if s.WorkerMergedGraceMinutes != 10 {
		t.Fatalf("merged-close grace default lost: %+v", s)
	}
	if shorter := (workerSettings{WorkerMergedGraceMinutes: 5}); !shorter.widens(workerSettings{WorkerMergedGraceMinutes: 10}) {
		t.Fatal("a shorter merged-close grace must need the person")
	}
	reserve, minutes := 0, 90
	changed, ok := (workerSettingsPatch{WorkerLeadReservePct: &reserve, WorkerExpectedRunMinutes: &minutes}).apply(s)
	if !ok || !changed.valid() || changed.MaxRunning != 3 || changed.FinishAfterMinutes != 8 || changed.WorkerLeadReservePct != 0 || changed.WorkerExpectedRunMinutes != 90 {
		t.Fatalf("partial settings update: %+v", changed)
	}
	raw, _ := json.Marshal(changed)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := &apiServer{dataDir: a.dataDir}
	q2 := reloaded.workers()
	q2.mu.Lock()
	reloaded.loadWorkerSettingsLocked(q2)
	got := q2.settings
	q2.mu.Unlock()
	if got != changed {
		t.Fatalf("explicit zero reserve lost: %+v", got)
	}
	for _, bad := range []workerSettings{{MaxRunning: 4, FinishAfterMinutes: 5, WorkerLeadReservePct: 101, WorkerExpectedRunMinutes: 45}, {MaxRunning: 4, FinishAfterMinutes: 5, WorkerLeadReservePct: -1, WorkerExpectedRunMinutes: 45}, {MaxRunning: 4, FinishAfterMinutes: 5, WorkerLeadReservePct: 15, WorkerExpectedRunMinutes: 0}} {
		if bad.valid() {
			t.Fatalf("invalid settings accepted: %+v", bad)
		}
	}
}

func TestQuotaWorkerSpawnAppliesLeadReserve(t *testing.T) {
	a, calls := newQuotaWorkerAPI(t)
	lead, _ := a.mgr.Get("lead")
	lead.Cmd = []string{"codex"}
	lead.Agent = "codex"
	lead.Credential = &paneCredentialInfo{Provider: "codex", Account: "test"}
	// The fixture offers Codex 90% and Claude 80%. Protecting this lead's
	// fifteen percent must route the worker to Claude despite prefer=codex.
	text, isErr := callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "prefer": "codex", "workspace": "Work", "cwd": t.TempDir(), "wait_ready": false})
	if isErr {
		t.Fatal(text)
	}
	if len(*calls) != 1 || (*calls)[0].Cmd[0] != "claude" || !strings.Contains(text, "reserve:") {
		t.Fatalf("lead reserve was not used at spawn: %s, %+v", text, *calls)
	}
}

func TestQuotaWorkerPredictedResetCannotSpendLeadReserve(t *testing.T) {
	now := time.Unix(10000, 0)
	settings := defaultWorkerSettings()
	other := workerAccount{Provider: "codex", Account: "other", Headroom: 40}
	for _, remaining := range []int{20, 15, 10} {
		t.Run(fmt.Sprint(remaining), func(t *testing.T) {
			lead := workerAccount{Provider: "claude", Account: "lead", Headroom: remaining, Windows: []workerQuotaWindow{{Remaining: remaining, ResetEpoch: now.Add(30 * time.Minute).Unix()}}}
			score := scoreWorkerQuota(lead, 1, settings, now)
			if score.usable != max(0, remaining-15) {
				t.Fatalf("predicted refill spent protected quota: %+v", score)
			}
			got, err := pickWorkerWithPolicy([]workerAccount{lead, other}, "claude", false, map[workerAccountKey]int{lead.key(): 1}, settings, now)
			if err != nil || got.Account != "other" || !strings.HasPrefix(got.Reason, "reserve:") {
				t.Fatalf("lead reserve defeated by forecast: %+v, %v", got, err)
			}
		})
	}
}
