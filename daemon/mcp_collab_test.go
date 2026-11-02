package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPaneSlotsFollowTheUILayoutOrder(t *testing.T) {
	ws := &WorkspaceStore{data: map[string]*Workspace{
		"api": {
			Name: "api",
			Layout: LayoutNode{"type": "split", "a": LayoutNode{"type": "pane", "paneId": "p_claude"},
				"b": map[string]any{"type": "split",
					"a": map[string]any{"type": "browser", "browserId": "b1"},
					"b": map[string]any{"type": "pane", "paneId": "p_codex"}}},
			PaneIds: []string{"p_codex", "p_claude", "p_orphan"},
		},
		"_current": {Name: "_current", PaneIds: []string{"p_claude"}},
	}}
	slots := paneSlotsByID(ws)
	if slots["p_claude"] != (paneSlot{"api", 1}) || slots["p_codex"] != (paneSlot{"api", 2}) {
		t.Fatalf("layout order: %+v", slots)
	}
	if slots["p_orphan"] != (paneSlot{"api", 3}) {
		t.Fatalf("panes missing from the layout come after it: %+v", slots["p_orphan"])
	}
	if got := paneLabel(slots["p_codex"], "/bin/bash", agentCodex); got != "#2 Codex" {
		t.Fatalf("label = %q", got)
	}
	if got := paneLabel(slots["p_claude"], "api-errors", agentClaude); got != "#1 api-errors" {
		t.Fatalf("a renamed pane keeps its title: %q", got)
	}
}

func TestListPanesReportsNumberStateAndCaller(t *testing.T) {
	a := &apiServer{mgr: NewManager(""), uiBus: newUIBus()}
	for _, id := range []string{"pane_one", "pane_two"} {
		p := testPaneWithTail(t, id, "bash", "$ ")
		a.mgr.panes[id] = p
	}
	a.ws = &WorkspaceStore{data: map[string]*Workspace{"w": {
		Name:    "w",
		Layout:  LayoutNode{"type": "split", "a": LayoutNode{"type": "pane", "paneId": "pane_two"}, "b": LayoutNode{"type": "pane", "paneId": "pane_one"}},
		PaneIds: []string{"pane_one", "pane_two"},
	}}}
	text, isErr := mcpResultText(t, a.mcpToolListPanes(nil, "pane_one"))
	if isErr {
		t.Fatal(text)
	}
	var rows []mcpPaneSummary
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "pane_two" || rows[0].Number != 1 || rows[1].Number != 2 {
		t.Fatalf("rows not in UI order: %+v", rows)
	}
	if !rows[1].You || rows[0].You {
		t.Fatalf("you flag: %+v", rows)
	}
	if rows[0].Workspace != "w" || rows[0].State == "" || rows[0].Label != "#1 bash" {
		t.Fatalf("workspace/state/label: %+v", rows[0])
	}
	if got := a.resolvePaneRef("#2", "pane_two"); got != "pane_one" {
		t.Fatalf("#2 resolves to %q", got)
	}
}

func claudeLine(typ string, ts time.Time, extra string) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":%q,"sessionId":"s1",%s}`, typ, ts.UTC().Format(time.RFC3339Nano), extra)
}

func TestClaudeLatestReplyTakesOnlyTheLastTurn(t *testing.T) {
	t0 := time.Now().Add(-time.Minute)
	lines := []string{
		claudeLine("user", t0, `"promptSource":"typed","message":{"role":"user","content":"old question"}`),
		claudeLine("assistant", t0, `"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"old answer"}]}`),
		claudeLine("user", t0.Add(time.Second), `"promptSource":"typed","message":{"role":"user","content":"review my last change"}`),
		claudeLine("assistant", t0.Add(2*time.Second), `"message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{}}]}`),
		claudeLine("user", t0.Add(3*time.Second), `"message":{"role":"user","content":[{"type":"tool_result","content":"diff"}]}`),
	}
	r := claudeLatestReply([]byte(strings.Join(lines, "\n")))
	if r.Prompt != "review my last change" || r.Complete || strings.Contains(r.Reply, "old answer") {
		t.Fatalf("mid-turn: %+v", r)
	}
	lines = append(lines, claudeLine("assistant", t0.Add(4*time.Second), `"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Three issues: a loop, no test, a typo."}]}`))
	r = claudeLatestReply([]byte(strings.Join(lines, "\n")))
	if !r.Complete || r.Reply != "Looking.\n\nThree issues: a loop, no test, a typo." {
		t.Fatalf("finished turn: %+v", r)
	}
}

func TestCodexLatestReplyUsesTheFinalMessage(t *testing.T) {
	ts := `"timestamp":"2026-10-23T01:00:00Z"`
	lines := []string{
		`{` + ts + `,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /x"}]}}`,
		`{` + ts + `,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"review the last commit"}]}}`,
		`{` + ts + `,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reading the diff."}]}}`,
	}
	r := codexLatestReply([]byte(strings.Join(lines, "\n")))
	if r.Prompt != "review the last commit" || r.Complete {
		t.Fatalf("mid-turn: %+v", r)
	}
	lines = append(lines, `{`+ts+`,"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Found 3 issues."}}`)
	r = codexLatestReply([]byte(strings.Join(lines, "\n")))
	if !r.Complete || r.Reply != "Found 3 issues." {
		t.Fatalf("finished: %+v", r)
	}
}

func TestAskPaneSendsWaitsAndReturnsOnlyTheReply(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(transcript, []byte(claudeLine("user", time.Now().Add(-time.Hour), `"promptSource":"typed","message":{"role":"user","content":"earlier"}`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := testPaneWithTail(t, "pane_claude", "claude", "❯ \n")
	p.Cmd = []string{"claude"}
	p.ClaudeTranscriptPath = transcript
	// The fake agent answers the message once it has been submitted, as a
	// real one does. (It used to answer on a 300 ms timer: under -race and
	// load, ask_pane could take its before-sending baseline after that
	// reply was written, and then the reply read as the previous turn's.)
	replyAfterSubmit(t, p, "review my change", func() {
		now := time.Now()
		f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		defer f.Close()
		fmt.Fprintln(f, claudeLine("user", now, `"promptSource":"typed","message":{"role":"user","content":"review my change"}`))
		fmt.Fprintln(f, claudeLine("assistant", now, `"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Looks good apart from the loop."}]}`))
	})
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_claude", "text": "review my change", "timeout_seconds": 10.0}, "pane_caller"))
	if isErr {
		t.Fatal(text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "replied" || res.Reply != "Looks good apart from the loop." || res.Source != "transcript" {
		t.Fatalf("result: %+v", res)
	}
}

// replyAfterSubmit connects p to a fake agent that reads the pane's input
// and runs reply once, after the Enter that follows message.
func replyAfterSubmit(t *testing.T, p *Pane, message string, reply func()) {
	t.Helper()
	paneEnd, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = paneEnd.Close(); _ = agentEnd.Close() })
	go func() {
		var typed strings.Builder
		replied := false
		for {
			_, payload, err := ReadFrame(agentEnd)
			if err != nil {
				return
			}
			typed.Write(payload)
			if !replied && strings.Contains(typed.String(), message) && strings.HasSuffix(typed.String(), "\r") {
				replied = true
				reply()
			}
		}
	}()
	p.mu.Lock()
	p.agentConn = paneEnd
	p.mu.Unlock()
}

// A new Claude pane in a folder that already holds an older session must not
// report that session's reply: the folder guess is ignored when the file is
// older than the pane, and a pinned transcript still counts.
func TestClaudeTranscriptFromAnEarlierSessionIsNotThePanesReply(t *testing.T) {
	old := filepath.Join(t.TempDir(), "old-session.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	snap := PaneSnapshot{CreatedAt: time.Now()}
	if claudeTranscriptBelongsToPane(old, snap) {
		t.Fatal("an older session's transcript was taken as the new pane's")
	}
	snap.ClaudeSessionID = "old-session"
	if !claudeTranscriptBelongsToPane(old, snap) {
		t.Fatal("the session the hooks pinned must count")
	}
	snap = PaneSnapshot{CreatedAt: time.Now(), ClaudeTranscriptPath: old}
	if !claudeTranscriptBelongsToPane(old, snap) {
		t.Fatal("the pinned transcript path must count")
	}
}

// ask_pane's baseline: a finished turn already in the transcript before the
// message was sent is not the answer, even when its timestamps are close.
func TestAskPaneBaselineIgnoresTheTurnBeforeTheAsk(t *testing.T) {
	sent := time.Now()
	base := paneTurnBaseline{Since: sent, Path: "/t/s1.jsonl", PromptAt: sent.Add(-time.Second)}
	before := paneReply{Path: "/t/s1.jsonl", PromptAt: sent.Add(-time.Second), Complete: true}
	if base.freshTurn(before) {
		t.Fatal("the previous turn counted as the reply")
	}
	if !base.freshTurn(paneReply{Path: "/t/s1.jsonl", PromptAt: sent.Add(time.Second), Complete: true}) {
		t.Fatal("the asked turn did not count")
	}
	if !base.freshTurn(paneReply{Path: "/t/s2.jsonl", PromptAt: sent, Complete: true}) {
		t.Fatal("a new session file for this pane did not count")
	}
	if base.freshTurn(paneReply{Path: "/t/s2.jsonl", PromptAt: sent.Add(-time.Hour), Complete: true}) {
		t.Fatal("an old turn in another file counted")
	}
}

func TestAskPaneRefusesSelfBusyAndMenus(t *testing.T) {
	p := choicePromptPane(t, "pane_menu")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	if text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_menu", "text": "hi"}, "pane_menu")); !isErr || !strings.Contains(text, "your own pane") {
		t.Fatalf("self: %q", text)
	}
	if text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_menu", "text": "hi"}, "other")); !isErr || !strings.Contains(text, "approval menu") {
		t.Fatalf("menu: %q", text)
	}
}

func TestWaitForPaneTimesOutWithAHint(t *testing.T) {
	p := testPaneWithTail(t, "pane_quiet", "bash", "$ make\nbuilding…\n")
	p.LastActivityMs = time.Now().UnixMilli()
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	res := a.waitForPaneTurn(p, paneTurnBaseline{Since: time.Now()}, 600*time.Millisecond)
	if res.Status != "still_working" || res.Hint == "" {
		t.Fatalf("timeout result: %+v", res)
	}
}

func TestCodexConfigEnsureDefaultsKeepsUserChoices(t *testing.T) {
	body := "[mcp_servers.soluterminal]\nurl = \"http://127.0.0.1:8787/mcp\"\n\n[mcp_servers.soluterminal.tools.read_pane_tail]\napproval_mode = \"prompt\"\n"
	next, changed := codexConfigEnsureDefaults(body)
	if !changed || !strings.Contains(next, "tool_timeout_sec = 900") {
		t.Fatalf("timeout not added:\n%s", next)
	}
	if strings.Count(next, "[mcp_servers.soluterminal.tools.read_pane_tail]") != 1 || !strings.Contains(next, `approval_mode = "prompt"`) {
		t.Fatalf("user's own approval_mode must stay:\n%s", next)
	}
	if !strings.Contains(next, "[mcp_servers.soluterminal.tools.list_panes]\napproval_mode = \"approve\"") {
		t.Fatalf("read-only tool not pre-approved:\n%s", next)
	}
	if again, changed := codexConfigEnsureDefaults(next); changed || again != next {
		t.Fatal("second pass must be a no-op")
	}
	if _, changed := codexConfigEnsureDefaults("[mcp_servers.other]\nurl = \"x\"\n"); changed {
		t.Fatal("other servers are untouched")
	}
	stripped, _ := codexConfigRemoveServer(next, "soluterminal")
	if strings.Contains(stripped, "soluterminal") {
		t.Fatalf("removal must take the tool tables too:\n%s", stripped)
	}
}

func TestClaudeInjectSettingsAllowReadOnlyToolsOnly(t *testing.T) {
	settings := injectFlagValue(claudeInjectArgs(testInjectionRecord(), "p1", nil), "--settings")
	var cfg struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(settings), &cfg); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Permissions.Allow, " ")
	for _, want := range []string{"mcp__soluterminal__list_panes", "mcp__soluterminal__wait_for_pane"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, cfg.Permissions.Allow)
		}
	}
	for _, acting := range []string{"send_input", "ask_pane", "answer_prompt", "spawn_pane", "kill_pane"} {
		if strings.Contains(joined, "__"+acting) {
			t.Fatalf("%s must still ask", acting)
		}
	}
}

func TestMenuRegionAndFocusForCodexSubmitMenus(t *testing.T) {
	menu := menuRegion(codexMcpPermissionObservedPrompt)
	if !menuNeedsSubmit(menu) || focusedMenuOption(menu) != "1" {
		t.Fatalf("focus/submit: %q", menu)
	}
	moved := strings.Replace(strings.Replace(codexMcpPermissionObservedPrompt, "› 1.", "  1.", 1), "  2. Allow for this session", "› 2. Allow for this session", 1)
	if focusedMenuOption(menuRegion(moved)) != "2" {
		t.Fatal("focus did not follow the marker")
	}
	stale := "› 1. Old\n  2. Older\nenter to submit\n...\n" + codexMcpPermissionObservedPrompt
	if opts := parseMenuOptions(menuRegion(stale)); !strings.HasPrefix(opts["2"], "Allow for this session") {
		t.Fatalf("the bottom menu wins: %+v", opts)
	}
}
