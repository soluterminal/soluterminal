package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCodexGuard = `[features]
hooks = true
[[hooks.PreToolUse]]
[[hooks.PreToolUse.hooks]]
type = "command"
command = '''python3 guard.py role'''
`
const testClaudeGuard = `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"python3 guard.py"}]}]}}`

func TestWorkerCapabilitiesCodexGuardWithEmptyClaudeSettings(t *testing.T) {
	_, cwd := capabilityFixture(t)
	putCapabilityFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"hooks":{}}`)
	putCapabilityFile(t, filepath.Join(cwd, ".codex", "config.toml"), testCodexGuard)
	needs, caps, err := workerCapabilityRequirements(map[string]any{"cwd": cwd})
	if err != nil || len(needs) != 1 || needs[0] != "role-guard" || !caps["codex"]["role-guard"] {
		t.Fatalf("Codex guard ignored: needs=%v caps=%v err=%v", needs, caps, err)
	}
}

func capabilityFixture(t *testing.T) (string, string) {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	return home, cwd
}
func putCapabilityFile(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestWorkerProviderCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, global, project, claude, mcp                   string
		codexBrowser, codexGuard, claudeBrowser, claudeGuard bool
	}{
		{name: "both browser transports", global: "[mcp_servers.solubrowse]\ncommand = 'node'", mcp: `{"mcpServers":{"solubrowse":{"command":"node"}}}`, codexBrowser: true, claudeBrowser: true},
		{name: "ported Codex role", project: testCodexGuard, codexGuard: true},
		{name: "Claude role", claude: testClaudeGuard, claudeGuard: true},
		{name: "disabled global browser", global: "[mcp_servers.solubrowse]\nenabled=false\ncommand='node'"},
		{name: "project disables browser", global: "[mcp_servers.solubrowse]\ncommand='node'", project: "[mcp_servers.solubrowse]\nenabled=false\ncommand='node'"},
		{name: "subtable is not a server", global: "[mcp_servers.solubrowse.env]\nurl='http://127.0.0.1'"},
		{name: "guard must be enabled", project: strings.Replace(testCodexGuard, "hooks = true", "hooks = false", 1)},
		{name: "narrow hook cannot guard all tools", project: strings.Replace(testCodexGuard, "[[hooks.PreToolUse]]", "[[hooks.PreToolUse]]\nmatcher='Bash'", 1)},
		{name: "malformed guard fails closed", project: testCodexGuard + "[broken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := capabilityFixture(t)
			if tc.global != "" {
				putCapabilityFile(t, filepath.Join(home, ".codex", "config.toml"), tc.global)
			}
			if tc.project != "" {
				putCapabilityFile(t, filepath.Join(cwd, ".codex", "config.toml"), tc.project)
			}
			if tc.claude != "" {
				putCapabilityFile(t, filepath.Join(cwd, ".claude", "settings.json"), tc.claude)
			}
			if tc.mcp != "" {
				putCapabilityFile(t, filepath.Join(cwd, ".mcp.json"), tc.mcp)
			}
			got := workerProviderCapabilities(cwd)
			if got["codex"]["browser"] != tc.codexBrowser || got["codex"]["role-guard"] != tc.codexGuard || got["claude"]["browser"] != tc.claudeBrowser || got["claude"]["role-guard"] != tc.claudeGuard {
				t.Fatalf("capabilities=%+v", got)
			}
		})
	}
}
func TestWorkerCapabilitiesRouteAcrossProviders(t *testing.T) {
	home, cwd := capabilityFixture(t)
	putCapabilityFile(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.solubrowse]\ncommand='node'")
	putCapabilityFile(t, filepath.Join(cwd, ".codex", "config.toml"), testCodexGuard)
	a, calls := newQuotaWorkerAPI(t)
	// The guarded folder has a Codex guard only, so it must no longer force Claude.
	text, isErr := callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "needs": []string{"browser"}, "prefer": "claude", "cwd": cwd, "workspace": "Work", "wait_ready": false})
	if isErr || len(*calls) != 1 || (*calls)[0].Cmd[0] != "codex" {
		t.Fatalf("ported guard did not qualify Codex: %s", text)
	}
	// Both providers qualify for browser, and Claude has quota when Codex has none.
	putCapabilityFile(t, filepath.Join(cwd, ".claude", "settings.json"), testClaudeGuard)
	putCapabilityFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"solubrowse":{"command":"node"}}}`)
	quotaWorkerAccounts = func(*apiServer, time.Time) []workerAccount {
		return []workerAccount{{Provider: "codex", Account: "dry", Headroom: 0}, {Provider: "claude", Account: "ready", Headroom: 30}}
	}
	text, isErr = callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "needs": []string{"browser"}, "cwd": cwd, "workspace": "Work", "wait_ready": false})
	if isErr || len(*calls) != 2 || (*calls)[1].Cmd[0] != "claude" {
		t.Fatalf("quota did not fall through to qualifying provider: %s", text)
	}
}
func TestWorkerMissingCapabilityAndQuotaQueue(t *testing.T) {
	home, cwd := capabilityFixture(t)
	a, calls := newQuotaWorkerAPI(t)
	text, isErr := callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "needs": []string{"browser"}, "cwd": cwd, "workspace": "Work", "wait_ready": false})
	if !isErr || !strings.Contains(text, "browser") || len(*calls) != 0 || len(a.workers().requests) != 0 {
		t.Fatalf("missing capability should error before queue: %s", text)
	}
	putCapabilityFile(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.solubrowse]\ncommand='node'")
	quotaWorkerAccounts = func(*apiServer, time.Time) []workerAccount {
		return []workerAccount{{Provider: "codex", Account: "dry", Headroom: 0}, {Provider: "claude", Account: "ineligible", Headroom: 100}}
	}
	text, isErr = callMCPSpawnPane(t, a, "lead", map[string]any{"cmd": "auto", "needs": []string{"browser"}, "cwd": cwd, "workspace": "Work", "wait_ready": false})
	if isErr || !strings.Contains(text, `"queued":true`) || len(*calls) != 0 {
		t.Fatalf("qualifying quota exhaustion should queue: %s", text)
	}
	quotaWorkerAccounts = func(*apiServer, time.Time) []workerAccount {
		return []workerAccount{{Provider: "codex", Account: "refilled", Headroom: 20}}
	}
	q := a.workers()
	q.mu.Lock()
	r := q.requests[0]
	r.Info.State = "running"
	q.mu.Unlock()
	result := a.startAutoWorker(r)
	text, isErr = mcpResultText(t, result)
	if isErr || len(*calls) != 1 || (*calls)[0].Cmd[0] != "codex" {
		t.Fatalf("queued quota did not recover: %s", text)
	}
}
func TestWorkerNeedsSchemaAndValidation(t *testing.T) {
	for _, tool := range mcpCoreTools() {
		if tool.Name != "spawn_pane" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if len(schema.Properties["needs"]) == 0 {
			t.Fatal("needs missing from MCP schema")
		}
	}
	_, cwd := capabilityFixture(t)
	putCapabilityFile(t, filepath.Join(cwd, ".solu-no-autoanswer"), "")
	needs, err := workerNeeds(map[string]any{"needs": []any{"browser", "browser"}}, cwd)
	if err != nil || strings.Join(needs, ",") != "browser,role-guard" {
		t.Fatalf("needs=%v err=%v", needs, err)
	}
	if _, err := workerNeeds(map[string]any{"needs": "browser"}, cwd); err == nil {
		t.Fatal("non-array accepted")
	}
	if _, _, err := workerCapabilityRequirements(map[string]any{"cwd": cwd}); err == nil || !strings.Contains(err.Error(), "role-guard") {
		t.Fatal("marker without guard config did not fail closed")
	}
}
