package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// list_schedules paging, filters, aliases and opt-in run history (moved here
// from the removed managed-agent CRUD tests).
func TestMCPListSchedulesPagesAndFilters(t *testing.T) {
	scheds, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheds.Stop)
	api := &apiServer{scheds: scheds}
	var alpha1, alpha5 *Schedule
	wantIDs := map[string]bool{}
	for i := 1; i <= 5; i++ {
		schedule, err := scheds.Create(ScheduleInput{
			Name: fmt.Sprintf("alpha %d", i), Kind: "prompt", Cron: "0 9 * * *", PaneID: "pane-1", Prompt: "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs[schedule.ID] = true
		if i == 1 {
			alpha1 = schedule
		}
		if i == 5 {
			alpha5 = schedule
		}
	}
	beta, err := scheds.Create(ScheduleInput{
		Name: "beta", Kind: "prompt", Cron: "0 9 * * *", PaneID: "pane-1", Prompt: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs[beta.ID] = true
	if _, err := scheds.SetEnabled(alpha5.ID, false); err != nil {
		t.Fatal(err)
	}
	scheds.recordRun(alpha1.ID, ScheduleRun{At: time.Now().UTC(), Status: "ok", Detail: "finished"})
	// A hidden agent-run schedule (what a managed-agent card left behind) is
	// never listed.
	hidden, err := scheds.createAgentRunSchedule("agt_repair", "Repair Medic", "*/30 * * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	hiddenID := hidden.ID

	type schedulePage struct {
		Schedules []scheduleDTO `json:"schedules"`
		Page      mcpPageInfo   `json:"page"`
	}
	call := func(args map[string]any) (schedulePage, string, bool) {
		t.Helper()
		text, isErr := mcpResultText(t, api.mcpToolListSchedules(args))
		var page schedulePage
		if !isErr {
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatalf("decode schedules page: %v (text=%q)", err, text)
			}
		}
		return page, text, isErr
	}
	ids := func(page schedulePage) []string {
		out := make([]string, 0, len(page.Schedules))
		for _, schedule := range page.Schedules {
			out = append(out, schedule.ID)
		}
		return out
	}

	all, raw, isErr := call(nil)
	if isErr || all.Page.Total != 6 || all.Page.Truncated || len(all.Schedules) != 6 || strings.Contains(raw, hiddenID) || strings.Contains(raw, agentKindSchedule) {
		t.Fatalf("all schedules = %+v raw=%q err=%v", all, raw, isErr)
	}
	if len(all.Schedules[0].Runs) != 0 || all.Schedules[0].ID != alpha1.ID {
		t.Fatalf("default schedule runs/order = %+v (runs are opt-in)", all.Schedules)
	}

	seen := map[string]bool{}
	var walked []string
	cursor := ""
	for {
		args := map[string]any{"limit": float64(2)}
		if cursor != "" {
			args["cursor"] = cursor
		}
		page, _, pageErr := call(args)
		if pageErr || len(page.Schedules) > 2 {
			t.Fatalf("schedule page cursor %q = %+v err=%v", cursor, page, pageErr)
		}
		if cursor == "" && (len(page.Schedules) != 2 || !page.Page.Truncated || page.Page.NextCursor != "2" || page.Page.Remaining != 4) {
			t.Fatalf("first schedule page = %+v", page)
		}
		for _, id := range ids(page) {
			if seen[id] {
				t.Fatalf("schedule walk returned %s twice", id)
			}
			seen[id] = true
			walked = append(walked, id)
		}
		if !page.Page.Truncated {
			break
		}
		cursor = page.Page.NextCursor
	}
	if len(seen) != len(wantIDs) {
		t.Fatalf("schedule walk saw %v, want %v", seen, wantIDs)
	}
	for i, schedule := range all.Schedules {
		if walked[i] != schedule.ID {
			t.Fatalf("schedule walk order = %v, want %v", walked, ids(all))
		}
	}

	alpha, _, _ := call(map[string]any{"nameContains": "ALPHA"})
	if alpha.Page.Total != 5 {
		t.Fatalf("alpha filter total = %d, want 5", alpha.Page.Total)
	}
	enabled, _, _ := call(map[string]any{"enabledOnly": true})
	if enabled.Page.Total != 5 {
		t.Fatalf("enabled filter total = %d, want 5", enabled.Page.Total)
	}
	for _, schedule := range enabled.Schedules {
		if schedule.ID == alpha5.ID {
			t.Fatal("enabled filter returned disabled alpha 5")
		}
	}
	alphaEnabled, _, _ := call(map[string]any{"nameContains": "alpha", "enabledOnly": true})
	alias, _, _ := call(map[string]any{"name_contains": "alpha", "enabled_only": true})
	if alphaEnabled.Page.Total != 4 || alias.Page.Total != 4 || strings.Join(ids(alphaEnabled), ",") != strings.Join(ids(alias), ",") {
		t.Fatalf("combined filters camel=%+v snake=%+v", alphaEnabled, alias)
	}

	withoutRuns, raw, isErr := call(map[string]any{"includeRuns": false})
	if isErr || strings.Contains(raw, `"runs"`) {
		t.Fatalf("includeRuns=false = %q err=%v", raw, isErr)
	}
	for _, schedule := range withoutRuns.Schedules {
		if schedule.ID == alpha1.ID && len(schedule.Runs) != 0 {
			t.Fatalf("includeRuns=false retained runs: %+v", schedule)
		}
	}
	withRuns, _, _ := call(map[string]any{"includeRuns": true})
	if len(withRuns.Schedules) == 0 || withRuns.Schedules[0].ID != alpha1.ID || len(withRuns.Schedules[0].Runs) == 0 {
		t.Fatalf("includeRuns=true dropped runs: %+v", withRuns.Schedules)
	}
	_, text, isErr := call(map[string]any{"cursor": "bad"})
	if !isErr || !strings.Contains(text, "invalid cursor") {
		t.Fatalf("invalid schedule cursor = %q err=%v", text, isErr)
	}
}
