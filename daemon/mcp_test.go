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
		"send_input", "spawn_pane", "create_schedule", "cancel_schedule", "write_file_line",
		// Gated in the Warden hardening pass — before it, `deny` still left
		// these callable (including kill_pane).
		"kill_pane", "rename_pane", "set_pane_flag", "switch_workspace", "preview_file", "answer_prompt",
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

func TestMCPReadFileReturnsMtimeForWriteGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{aria: &Aria{}}

	text, isErr := mcpResultText(t, a.mcpToolReadFile(map[string]any{
		"path":      path,
		"max_lines": float64(10),
	}))
	if isErr {
		t.Fatalf("read_file returned error: %q", text)
	}
	var parsed struct {
		Mtime int64 `json:"mtime"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("read_file result is not JSON: %v (%q)", err, text)
	}
	if parsed.Mtime == 0 {
		t.Fatalf("mtime = 0, want nonzero")
	}
}

func TestMCPWriteFileLineRequiresNonzeroExpectedMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{}

	text, isErr := mcpResultText(t, a.mcpToolWriteFileLine(map[string]any{
		"path":           path,
		"line_number":    float64(1),
		"new_text":       "ONE",
		"expected_mtime": float64(0),
	}))
	if !isErr {
		t.Fatalf("write_file_line succeeded without mtime guard: %q", text)
	}
	if !strings.Contains(text, "expected_mtime must be nonzero") {
		t.Fatalf("error = %q, want nonzero expected_mtime refusal", text)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("file changed despite refusal: %q", data)
	}
}

func TestMCPWriteFileLineRefusesSensitiveFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".env", "service.env", "deploy.key", "id_rsa", "api_token.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		text, isErr := mcpResultText(t, (&apiServer{}).mcpToolWriteFileLine(map[string]any{
			"path":           path,
			"line_number":    float64(1),
			"new_text":       "changed",
			"expected_mtime": float64(info.ModTime().UnixMilli()),
		}))
		if !isErr {
			t.Fatalf("%s write succeeded, want refusal: %q", name, text)
		}
		if !strings.Contains(text, "refusing to access likely") {
			t.Fatalf("%s error = %q, want sensitive-file refusal", name, text)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "secret\n" {
			t.Fatalf("%s changed despite refusal: %q", name, data)
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
