package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testInjectionRecord() agentIntegrationRecord {
	return agentIntegrationRecord{
		State:   agentIntegrationOff,
		MCPURL:  "http://127.0.0.1:8787/mcp",
		HookURL: "http://127.0.0.1:8787/api/hooks/tool-call",
	}
}

func injectFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestClaudeInjectArgsAddsToolsAndHooksForThisProcess(t *testing.T) {
	got := claudeInjectArgs(testInjectionRecord(), "pane_abc123", []string{"--resume", "s1"})
	if got[0] != "--resume" || got[1] != "s1" {
		t.Fatalf("user args must stay first: %v", got)
	}
	var mcp struct {
		McpServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(injectFlagValue(got, "--mcp-config")), &mcp); err != nil {
		t.Fatal(err)
	}
	s := mcp.McpServers["soluterminal"]
	if s.URL != "http://127.0.0.1:8787/mcp" || s.Headers[claudeMcpPaneHeaderName] != "pane_abc123" || s.Type != "http" {
		t.Fatalf("mcp config: %+v", mcp)
	}
	if cliArgsHasAny(got, "--strict-mcp-config") {
		t.Fatal("must add to the user's servers, not replace them")
	}
	var settings struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(injectFlagValue(got, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range claudeHookEvents {
		if len(settings.Hooks[event]) != 1 || !claudeHookIsOurEntry(settings.Hooks[event][0]) {
			t.Fatalf("hook %s: %v", event, settings.Hooks[event])
		}
	}
	if !strings.Contains(injectFlagValue(got, "--settings"), "pane_abc123") {
		t.Fatal("hooks should carry the literal pane id")
	}
}

func TestClaudeInjectArgsSkipsManagementAndOwnConfig(t *testing.T) {
	rec := testInjectionRecord()
	for _, args := range [][]string{
		{"mcp", "list"}, {"--version"}, {"-v"}, {"update"}, {"--help"},
		{"-p", "x", "--strict-mcp-config", "--mcp-config", "{}"},
	} {
		if got := claudeInjectArgs(rec, "p1", args); !reflect.DeepEqual(got, args) {
			t.Errorf("%v must be untouched, got %v", args, got)
		}
	}
	got := claudeInjectArgs(rec, "p1", []string{"--mcp-config", "mine.json"})
	if strings.Count(strings.Join(got, " "), "--mcp-config") != 1 || injectFlagValue(got, "--settings") == "" {
		t.Fatalf("own --mcp-config: keep it, still add hooks: %v", got)
	}
	// An unsafe pane id falls back to the environment variable.
	if v := injectFlagValue(claudeInjectArgs(rec, `p"; rm`, nil), "--mcp-config"); !strings.Contains(v, "${SOLU_PANE_ID}") {
		t.Fatalf("unsafe pane id must not be embedded: %s", v)
	}
}

func TestCodexInjectArgs(t *testing.T) {
	rec := testInjectionRecord()
	got := codexInjectArgs(rec, []string{"resume", "t1"})
	want := []string{"resume", "t1",
		"-c", `mcp_servers.soluterminal.url="http://127.0.0.1:8787/mcp"`,
		"-c", `mcp_servers.soluterminal.env_http_headers={"X-Soluterminal-Pane"="SOLU_PANE_ID"}`,
		"-c", "mcp_servers.soluterminal.tool_timeout_sec=900",
	}
	for _, tool := range soluReadOnlyMCPTools {
		want = append(want, "-c", "mcp_servers.soluterminal.tools."+tool+`.approval_mode="approve"`)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, args := range [][]string{
		{"login"}, {"--version"}, {"app-server"},
		{"-c", `mcp_servers.soluterminal.url="http://127.0.0.1:9/mcp"`},
	} {
		if got := codexInjectArgs(rec, args); !reflect.DeepEqual(got, args) {
			t.Errorf("%v must be untouched, got %v", args, got)
		}
	}
}

func TestInjectAgentArgsForPaneFollowsTheChoice(t *testing.T) {
	_, dataDir, store := isolateAgentHome(t)
	cmd := []string{"claude", "--resume", "s1"}

	// No record yet (a daemon that has not booted this version): untouched.
	if got := injectAgentArgsForPane(dataDir, "p1", nil, cmd); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("no record: %v", got)
	}
	bootAgentIntegration(store, "127.0.0.1:8787", false, true, func() bool { return false })
	if got := injectAgentArgsForPane(dataDir, "p1", nil, cmd); injectFlagValue(got, "--mcp-config") == "" {
		t.Fatalf("ask state must inject: %v", got)
	}
	if _, err := store.update(func(r *agentIntegrationRecord) { r.State = agentIntegrationOn }); err != nil {
		t.Fatal(err)
	}
	if got := injectAgentArgsForPane(dataDir, "p1", nil, cmd); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("global registration on: no per-process duplicate, got %v", got)
	}
	if _, err := store.update(func(r *agentIntegrationRecord) { r.State = agentIntegrationOff }); err != nil {
		t.Fatal(err)
	}
	codex := injectAgentArgsForPane(dataDir, "p1", []string{"SOLU_CONTEXT_PACK=/x"}, []string{"codex"})
	if len(codex) != 7+2*len(soluReadOnlyMCPTools) || strings.Contains(strings.Join(codex, " "), "developer_instructions=") {
		t.Fatalf("codex off: %v", codex)
	}
	if got := injectAgentArgsForPane(dataDir, "p1", nil, []string{"bash"}); !reflect.DeepEqual(got, []string{"bash"}) {
		t.Fatalf("shells untouched: %v", got)
	}
	if _, ok := agentInjectionRecord(filepath.Join(dataDir, "missing")); ok {
		t.Fatal("missing data dir must not inject")
	}
}

// A Codex pane started by SoluTerminal runs through the live bridge with the
// inline argv ["codex", "--no-alt-screen"]; it must get the same per-process
// server, or Codex falls back to the user's own config (no tools, or another
// install's).
func TestInjectAgentArgsCoversTheCodexLiveBridgeArgv(t *testing.T) {
	_, dataDir, store := isolateAgentHome(t)
	bootAgentIntegration(store, "127.0.0.1:47621", false, true, func() bool { return false })
	got := injectAgentArgsForPane(dataDir, "p1", nil, []string{"codex", "--no-alt-screen"})
	joined := strings.Join(got, " ")
	if got[0] != "codex" || got[1] != "--no-alt-screen" || !strings.Contains(joined, `mcp_servers.soluterminal.url="http://127.0.0.1:47621/mcp"`) {
		t.Fatalf("live bridge argv not injected: %v", got)
	}
}
