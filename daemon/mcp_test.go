package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPCallerPaneHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "present", header: "pane-1", want: "pane-1"},
		{name: "trimmed", header: " pane-1 ", want: "pane-1"},
		{name: "absent"},
		{name: "unexpanded", header: "${SOLU_PANE_ID}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.header != "" {
				req.Header.Set("X-Soluterminal-Pane", tt.header)
			}
			if got := mcpCallerPane(req); got != tt.want {
				t.Fatalf("mcpCallerPane() = %q, want %q", got, tt.want)
			}
		})
	}
}

func mcpCallForTest(t *testing.T, name string, args map[string]any) any {
	t.Helper()
	b, err := json.Marshal(toolCallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{mgr: NewManager(""), uiBus: newUIBus()}
	return a.mcpCallTool(b, nil, "")
}

func mcpResultText(t *testing.T, got any) (string, bool) {
	t.Helper()
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", got)
	}
	isErr, _ := m["isError"].(bool)
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %#v, want non-empty slice", m["content"])
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] = %#v, want map", content[0])
	}
	text, _ := first["text"].(string)
	return text, isErr
}

func TestMCPMutationPolicyDeniesConfiguredMutators(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "deny")

	for _, tool := range []string{
		"send_input", "spawn_pane", "create_schedule", "cancel_schedule",
		// Gated in the MCP hardening pass — before it, `deny` still left
		// these callable (including kill_pane).
		"kill_pane", "rename_pane", "set_pane_flag", "create_workspace", "switch_workspace", "preview_file", "answer_prompt",
	} {
		text, isErr := mcpResultText(t, mcpCallForTest(t, tool, map[string]any{}))
		if !isErr {
			t.Fatalf("%s was not returned as an error: %q", tool, text)
		}
		if !strings.Contains(text, "disabled by "+mcpMutationPolicyEnv) || !strings.Contains(text, "refused "+tool) {
			t.Fatalf("%s policy error = %q, want policy refusal", tool, text)
		}
	}
}

func TestMCPMutationPolicyDoesNotDenyReadOnlyTools(t *testing.T) {
	t.Setenv(mcpMutationPolicyEnv, "readonly")

	text, isErr := mcpResultText(t, mcpCallForTest(t, "list_panes", map[string]any{}))
	if isErr {
		t.Fatalf("list_panes returned error under readonly policy: %q", text)
	}
	if strings.Contains(text, mcpMutationPolicyEnv) {
		t.Fatalf("list_panes was policy-denied: %q", text)
	}
}

// read_file, search_files and write_file_line duplicated what every pane
// agent already has (its own Read, Glob/Grep and Edit), and get_pane_state and
// get_pane_livecwd folded into list_panes. git_status, read_file and
// search_files stay for scoped managed agents only
// (TestMCPFileToolsOnlyForScopedCallers).
func TestMCPRemovedToolsAreGoneForPanes(t *testing.T) {
	removed := []string{"read_file", "search_files", "write_file_line", "get_pane_state", "get_pane_livecwd", "git_status"}
	advertised := map[string]bool{}
	for _, tool := range mcpToolsForScope(nil) {
		advertised[tool.Name] = true
	}
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{aria: &Aria{}, mgr: NewManager("")}
	for _, name := range removed {
		if advertised[name] {
			t.Errorf("%s is still advertised to panes", name)
		}
		if mcpPublicToolNames[name] {
			t.Errorf("%s is still on the public surface", name)
		}
		b, _ := json.Marshal(toolCallParams{Name: name, Arguments: map[string]any{
			"path": path, "line_number": float64(1), "new_text": "changed", "expected_mtime": float64(1), "query": "notes",
		}})
		text, isErr := mcpResultText(t, a.mcpCallTool(b, nil, ""))
		if !isErr || text != "unknown tool: "+name {
			t.Errorf("%s from a pane = %q (isError=%v), want unknown tool", name, text, isErr)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "one\n" {
		t.Fatalf("file changed: %q", data)
	}
	for _, name := range soluReadOnlyMCPTools {
		if !advertised[name] {
			t.Errorf("auto-approved read-only tool %s is not a tool", name)
		}
	}
	for _, tool := range mcpTools() {
		for _, name := range removed[:5] {
			if strings.Contains(tool.Description, name) {
				t.Errorf("%s description still points at %s", tool.Name, name)
			}
		}
	}
}

// git_status, read_file and search_files are for daemon-minted scopes only:
// a managed agent runs with no shell or file tools of its own, so they are
// how it reads its repository. A pane's agent has its own git, Read and Grep.
func TestMCPFileToolsOnlyForScopedCallers(t *testing.T) {
	// A scope that lists them (the managed-agent read set does; see
	// TestAgentMintedScopeExposesOnlyReadTools, private).
	agent := &mcpScope{Identity: "agent:fix", Tools: map[string]bool{"git_status": true, "read_file": true, "search_files": true, "list_panes": true}, Expires: farFuture()}
	listed := map[string]bool{}
	for _, tool := range mcpToolsForScope(agent) {
		listed[tool.Name] = true
	}
	for _, name := range []string{"git_status", "read_file", "search_files"} {
		if !listed[name] {
			t.Errorf("a scope that lists %s doesn't get it", name)
		}
	}
	for _, tool := range mcpTools() {
		if tool.Name == "git_status" || tool.Name == "read_file" || tool.Name == "search_files" {
			t.Errorf("%s is advertised to panes", tool.Name)
		}
	}
	repo := testGitRepo(t)
	a := &apiServer{aria: &Aria{}, mgr: NewManager("")}
	b, _ := json.Marshal(toolCallParams{Name: "git_status", Arguments: map[string]any{"path": repo}})
	if text, isErr := mcpResultText(t, a.mcpCallTool(b, agent, "")); isErr {
		t.Fatalf("scoped git_status = %q, want a summary", text)
	}
	other := &mcpScope{Identity: "x", Tools: map[string]bool{"list_panes": true}, Expires: farFuture()}
	for _, tool := range mcpToolsForScope(other) {
		if tool.Name == "git_status" || tool.Name == "read_file" || tool.Name == "search_files" {
			t.Fatalf("a scope without %s lists it", tool.Name)
		}
	}
}

func TestUIBusRecentSinceCursor(t *testing.T) {
	b := newUIBus()
	for i := 0; i < 5; i++ {
		b.publish(uiEvent{Type: "toast", Text: "e"})
	}
	all, cursor := b.RecentSince(0)
	if len(all) != 5 || cursor != 5 {
		t.Fatalf("full window: got %d events cursor %d", len(all), cursor)
	}
	if all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("seq stamping wrong: first %d last %d", all[0].Seq, all[4].Seq)
	}
	newer, cursor := b.RecentSince(3)
	if len(newer) != 2 || newer[0].Seq != 4 || cursor != 5 {
		t.Fatalf("since=3: got %d events first seq %d cursor %d", len(newer), newer[0].Seq, cursor)
	}
	// Cursor survives ring trims: blow past logCap and ask again.
	for i := 0; i < 210; i++ {
		b.publish(uiEvent{Type: "toast", Text: "x"})
	}
	trimmed, cursor := b.RecentSince(5)
	if cursor != 215 {
		t.Fatalf("cursor after trim = %d", cursor)
	}
	if len(trimmed) != 200 {
		t.Fatalf("retained window = %d", len(trimmed))
	}
	if trimmed[0].Seq != 16 {
		t.Fatalf("oldest retained seq = %d (want 16 after trimming to 200)", trimmed[0].Seq)
	}
	none, _ := b.RecentSince(999)
	if len(none) != 0 {
		t.Fatalf("since beyond cursor returned %d events", len(none))
	}
}

func TestRedactToolHintPaneTools(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"send_input", map[string]any{"pane_id": "pane_ab", "text": "SECRET prompt\n"}, "pane_ab"},
		{"kill_pane", map[string]any{"pane_id": "pane_cd"}, "pane_cd"},
		{"answer_prompt", map[string]any{"pane_id": "pane_ef", "option": "2"}, "2 → pane_ef"},
		{"spawn_pane", map[string]any{"cmd": "muse", "cwd": "/home/u/proj"}, "muse · proj"},
		{"spawn_pane", map[string]any{}, ""},
		{"switch_workspace", map[string]any{"workspace": "trading"}, "trading"},
	}
	for _, c := range cases {
		if got := redactToolHint(c.tool, c.args); got != c.want {
			t.Errorf("redactToolHint(%s) = %q want %q", c.tool, got, c.want)
		}
	}
	// The input text must never leak into the hint.
	if got := redactToolHint("send_input", map[string]any{"pane_id": "p", "text": "hunter2"}); strings.Contains(got, "hunter2") {
		t.Errorf("send_input hint leaked text: %q", got)
	}
}
