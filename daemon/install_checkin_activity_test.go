package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckinActivityFlagsLastADayAndSurviveRestarts(t *testing.T) {
	allowCheckinForTest(t)
	sink, srv := newCheckinSink(t)
	dir := t.TempDir()
	base := func() string { return srv.URL }
	now := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := newInstallCheckin(dir, "0.0.1215", base, nil)
	c.now = clock

	if sent, err := c.sendOnce(context.Background()); !sent || err != nil {
		t.Fatalf("first send: sent=%v err=%v", sent, err)
	}
	now = now.Add(time.Hour)
	c.noteAgentPane()
	if sent, _ := c.sendOnce(context.Background()); !sent {
		t.Fatal("a flag turning on the same day must be reported")
	}
	if b := sink.bodies[1]; b["agentPaneDay"] != true || b["agentsTogetherDay"] != false {
		t.Fatalf("second body = %v", b)
	}
	c.noteAgentsTogether()
	if sent, _ := c.sendOnce(context.Background()); !sent || sink.bodies[2]["agentsTogetherDay"] != true {
		t.Fatalf("agents-together not reported: %v", sink.bodies)
	}

	// Only timestamps are kept locally, and a restart remembers them.
	raw, err := os.ReadFile(filepath.Join(dir, checkinSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["agentPaneAt"] != float64(now.Unix()) || saved["agentsTogetherAt"] != float64(now.Unix()) {
		t.Fatalf("checkin.json = %s", raw)
	}
	restarted := newInstallCheckin(dir, "0.0.1215", base, nil)
	restarted.now = clock
	if pane, together := restarted.activityFlags(); !pane || !together {
		t.Fatalf("restart forgot the day's activity: %v %v", pane, together)
	}

	// Noting again within the hour does not rewrite the file.
	now = now.Add(30 * time.Minute)
	restarted.noteAgentPane()
	if restarted.settings.AgentPaneAt != now.Add(-30*time.Minute).Unix() {
		t.Fatal("a second note within the hour must not move the timestamp")
	}

	// 24 hours after the last activity both flags are no again.
	now = now.Add(24 * time.Hour)
	if pane, together := restarted.activityFlags(); pane || together {
		t.Fatalf("flags must expire after 24 h: %v %v", pane, together)
	}
	if sent, _ := restarted.sendOnce(context.Background()); !sent {
		t.Fatal("next day must send")
	}
	last := sink.bodies[len(sink.bodies)-1]
	if last["agentPaneDay"] != false || last["agentsTogetherDay"] != false {
		t.Fatalf("expired flags sent as yes: %v", last)
	}
}

func TestCheckinActivityIsNotRecordedWhenCheckinsAreOff(t *testing.T) {
	allowCheckinForTest(t)
	dir := t.TempDir()
	c := newInstallCheckin(dir, "0.0.1215", nil, nil)
	if err := c.setEnabled(false); err != nil {
		t.Fatal(err)
	}
	c.noteAgentPane()
	c.noteAgentsTogether()
	if c.settings.AgentPaneAt != 0 || c.settings.AgentsTogetherAt != 0 {
		t.Fatalf("opted-out install recorded activity: %+v", c.settings)
	}

	t.Setenv(checkinDisabledEnv, "1")
	on := newInstallCheckin(t.TempDir(), "0.0.1215", nil, nil)
	on.noteAgentPane()
	if on.settings.AgentPaneAt != 0 {
		t.Fatal("SOLU_DISABLE_CHECKIN=1 must also stop the bookkeeping")
	}

	// A nil check-in (tests, or a daemon without one) is a no-op.
	var none *installCheckin
	none.noteAgentPane()
	none.noteAgentsTogether()
}

func TestCheckinAgentPaneKinds(t *testing.T) {
	cases := []struct {
		name string
		snap PaneSnapshot
		want bool
	}{
		{"plain shell", PaneSnapshot{Cmd: []string{"bash"}}, false},
		{"empty terminal", PaneSnapshot{}, false},
		{"claude terminal", PaneSnapshot{Cmd: []string{"claude"}}, true},
		{"wrapped claude", PaneSnapshot{Cmd: []string{"bash", "-lc", "exec 'claude' --resume x"}}, true},
		{"codex terminal", PaneSnapshot{Cmd: []string{"codex"}}, true},
		{"codex agent", PaneSnapshot{Agent: agentCodex, Cmd: []string{"bash"}}, true},
	}
	for _, tc := range cases {
		if got := checkinAgentPane(tc.snap); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSpawnPaneForRequestNotesAgentPanes(t *testing.T) {
	allowCheckinForTest(t)
	a := &apiServer{checkin: newInstallCheckin(t.TempDir(), "0.0.1215", nil, nil)}
	a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
		return &Pane{ID: "p_new", Cmd: opts.Cmd, Kind: opts.Kind, Agent: opts.Agent}, nil
	}
	if _, err := a.spawnPaneForRequest(SpawnOpts{Cmd: []string{"bash"}}); err != nil {
		t.Fatal(err)
	}
	if pane, _ := a.checkin.activityFlags(); pane {
		t.Fatal("a plain shell is not an agent pane")
	}
	if _, err := a.spawnPaneForRequest(SpawnOpts{Cmd: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if pane, _ := a.checkin.activityFlags(); !pane {
		t.Fatal("a Codex pane start must set agentPaneDay")
	}
}

func crossPaneServer(t *testing.T) *apiServer {
	t.Helper()
	allowCheckinForTest(t)
	a := &apiServer{mgr: NewManager(""), uiBus: newUIBus()}
	for _, id := range []string{"pane_lead", "pane_helper"} {
		a.mgr.panes[id] = testPaneWithTail(t, id, "bash", "$ ready\n")
	}
	a.checkin = newInstallCheckin(t.TempDir(), "0.0.1215", nil, nil)
	return a
}

func callAs(t *testing.T, a *apiServer, caller, name string, args map[string]any) any {
	t.Helper()
	b, err := json.Marshal(toolCallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return a.mcpCallTool(b, nil, caller)
}

func TestCrossPaneToolSetsAgentsTogether(t *testing.T) {
	a := crossPaneServer(t)

	// Reading its own pane, or calling with no caller pane, is not teamwork.
	if _, isErr := mcpResultText(t, callAs(t, a, "pane_lead", "read_pane_tail", map[string]any{"pane_id": "pane_lead"})); isErr {
		t.Fatal("own read_pane_tail failed")
	}
	callAs(t, a, "", "read_pane_tail", map[string]any{"pane_id": "pane_helper"})
	// A failed call on another pane doesn't count either.
	callAs(t, a, "pane_lead", "send_input", map[string]any{"pane_id": "pane_helper", "text": ""})
	callAs(t, a, "pane_lead", "read_pane_tail", map[string]any{"pane_id": "pane_missing"})
	if _, together := a.checkin.activityFlags(); together {
		t.Fatal("no cross-pane call has succeeded yet")
	}

	if text, isErr := mcpResultText(t, callAs(t, a, "pane_lead", "read_pane_tail", map[string]any{"pane_id": "pane_helper"})); isErr {
		t.Fatal(text)
	}
	if _, together := a.checkin.activityFlags(); !together {
		t.Fatal("reading another pane's output must set agentsTogetherDay")
	}
}

func TestCrossPaneTargetCoversTheCollaborationTools(t *testing.T) {
	a := crossPaneServer(t)
	for _, tool := range []string{"ask_pane", "send_input", "answer_prompt", "read_pane_tail", "wait_for_pane"} {
		if got := a.crossPaneTarget(tool, map[string]any{"pane_id": "pane_helper"}, "pane_lead"); got != "pane_helper" {
			t.Errorf("%s on another pane: target %q", tool, got)
		}
		if got := a.crossPaneTarget(tool, map[string]any{"pane_id": "pane_lead"}, "pane_lead"); got != "" {
			t.Errorf("%s on its own pane must not count: %q", tool, got)
		}
		if got := a.crossPaneTarget(tool, map[string]any{"pane_id": "pane_helper"}, "pane_unknown"); got != "" {
			t.Errorf("%s from an unknown caller must not count: %q", tool, got)
		}
	}
	if got := a.crossPaneTarget("spawn_pane", map[string]any{"cmd": "claude", "prompt": "fix the test"}, "pane_lead"); got == "" {
		t.Error("spawn_pane with a task must count")
	}
	if got := a.crossPaneTarget("spawn_pane", map[string]any{"cmd": "claude"}, "pane_lead"); got != "" {
		t.Error("spawn_pane without a task must not count")
	}
	for _, tool := range []string{"list_panes", "kill_pane", "rename_pane"} {
		if got := a.crossPaneTarget(tool, map[string]any{"pane_id": "pane_helper"}, "pane_lead"); got != "" {
			t.Errorf("%s must not count: %q", tool, got)
		}
	}

	// A spawn whose prompt never reached the new pane doesn't count.
	a.noteCrossPaneTool("(spawned)", toolOK(`{"id":"p","prompt_sent":false}`))
	if _, together := a.checkin.activityFlags(); together {
		t.Fatal("an undelivered spawn prompt must not count")
	}
	a.noteCrossPaneTool("(spawned)", toolOK(`{"id":"p","ready":true}`))
	if _, together := a.checkin.activityFlags(); !together {
		t.Fatal("a delivered spawn prompt must count")
	}
}
