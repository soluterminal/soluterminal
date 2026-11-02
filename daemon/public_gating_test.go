package main

import (
	"encoding/json"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeEntitlementsFile(t *testing.T, dir string, rec entitlementRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, entitlementsFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBootTeamVerdict(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	now := time.Now()

	fresh := t.TempDir()
	if team, src := bootTeamVerdict(fresh); team || src != "none" {
		t.Fatalf("fresh install = %t/%s, want off", team, src)
	}

	cachedTeam := t.TempDir()
	writeEntitlementsFile(t, cachedTeam, entitlementRecord{Team: true, Source: "machine", CheckedAt: now})
	if team, _ := bootTeamVerdict(cachedTeam); !team {
		t.Fatal("a cached team verdict (older record without tier) must start the team engines")
	}

	cachedAccount := t.TempDir()
	writeEntitlementsFile(t, cachedAccount, entitlementRecord{Team: false, Tier: tierAccount, Source: "account", CheckedAt: now})
	if err := os.MkdirAll(filepath.Join(cachedAccount, "workflow", "tasks", "wft_1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if team, _ := bootTeamVerdict(cachedAccount); team {
		t.Fatal("a cached free-account verdict wins over old workflow tasks")
	}

	existing := t.TempDir()
	if err := os.MkdirAll(filepath.Join(existing, "workflow", "tasks", "wft_1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if team, src := bootTeamVerdict(existing); !team || src != "existing-workflow" {
		t.Fatalf("install with workflow tasks and no verdict = %t/%s, want on", team, src)
	}

	emptyTasks := t.TempDir()
	if err := os.MkdirAll(filepath.Join(emptyTasks, "workflow", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if team, _ := bootTeamVerdict(emptyTasks); team {
		t.Fatal("an empty task store is not an existing workflow user")
	}

	t.Setenv("SOLU_TEAM_FEATURES", "1")
	if team, src := bootTeamVerdict(fresh); !team || src != "env" {
		t.Fatalf("env override = %t/%s", team, src)
	}
	t.Setenv("SOLU_TEAM_FEATURES", "0")
	if team, _ := bootTeamVerdict(cachedTeam); team {
		t.Fatal("env 0 must switch the team engines off")
	}
}

func publicAPIServer(t *testing.T) *apiServer {
	t.Helper()
	t.Setenv("SOLU_TEAM_FEATURES", "")
	return &apiServer{entitlements: newTeamEntitlements(t.TempDir(), nil)}
}

func mcpToolNames(t *testing.T, resp rpcResp) []string {
	t.Helper()
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T", resp.Result)
	}
	tools, ok := result["tools"].([]mcpTool)
	if !ok {
		t.Fatalf("tools = %T", result["tools"])
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestPublicMCPSurface(t *testing.T) {
	a := publicAPIServer(t)

	init := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"}, nil, "")
	instructions, _ := init.Result.(map[string]any)["instructions"].(string)
	if instructions != mcpPublicServerInstructions {
		t.Fatalf("public instructions = %q", instructions)
	}
	if strings.Contains(instructions, "workflow") || len(instructions) > 1200 {
		t.Fatalf("public instructions must be short and pipeline-free, got %d chars", len(instructions))
	}

	names := mcpToolNames(t, a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"}, nil, ""))
	if len(names) == 0 {
		t.Fatal("public tool list is empty")
	}
	for _, name := range names {
		if !mcpPublicToolNames[name] {
			t.Fatalf("tools/list leaked team tool %s", name)
		}
	}
	for _, want := range []string{"list_panes", "send_input", "preview_file", "create_schedule", "move_pane_to_workspace", "rename_workspace", "delete_workspace"} {
		if !strings.Contains(","+strings.Join(names, ",")+",", ","+want+",") {
			t.Fatalf("public tool list is missing %s: %v", want, names)
		}
	}

	params, _ := json.Marshal(map[string]any{"name": "create_workflow_task", "arguments": map[string]any{"prompt": "x"}})
	call := a.mcpDispatch(rpcReq{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "tools/call", Params: params}, nil, "")
	body, _ := json.Marshal(call.Result)
	if !strings.Contains(string(body), "not available in this edition") {
		t.Fatalf("create_workflow_task must be refused on a public install: %s", body)
	}

	spawn := a.mcpToolSpawnPane(map[string]any{"cmd": "claude", "role": "orchestrator"}, "")
	spawnBody, _ := json.Marshal(spawn)
	if !strings.Contains(string(spawnBody), "not available in this edition") {
		t.Fatalf("orchestrator role must be refused on a public install: %s", spawnBody)
	}
}

func TestPublicSpawnPaneSchemaHasNoRole(t *testing.T) {
	for _, tool := range mcpTools() {
		if tool.Name != "spawn_pane" {
			continue
		}
		if !strings.Contains(string(tool.InputSchema), `"role"`) {
			t.Fatal("team spawn_pane schema should still advertise role")
		}
		pub := publicMCPTool(tool)
		if strings.Contains(string(pub.InputSchema), `"role"`) {
			t.Fatalf("public spawn_pane schema still has role: %s", pub.InputSchema)
		}
		return
	}
	t.Fatal("spawn_pane not found")
}

func TestTeamCloudLLMAllowed(t *testing.T) {
	t.Setenv("SOLU_TEAM_FEATURES", "")
	prev := jevEntitlementsRef.Load()
	t.Cleanup(func() { jevEntitlementsRef.Store(prev) })

	jevEntitlementsRef.Store(newTeamEntitlements(t.TempDir(), nil))
	if teamCloudLLMAllowed() {
		t.Fatal("a public install must not call the team-only cloud LLM")
	}
	teamDir := t.TempDir()
	writeEntitlementsFile(t, teamDir, entitlementRecord{Team: true, Tier: tierTeam, Source: "machine", CheckedAt: time.Now()})
	jevEntitlementsRef.Store(newTeamEntitlements(teamDir, nil))
	if !teamCloudLLMAllowed() {
		t.Fatal("a team install keeps the cloud LLM")
	}
}

func TestCodexAgentsPublicBlock(t *testing.T) {
	prev := codexAgentsPublicPolicy.Load()
	t.Cleanup(func() { codexAgentsPublicPolicy.Store(prev) })

	codexAgentsPublicPolicy.Store(false)
	teamBlock := codexAgentsManagedBlock()
	existing := "user notes\n\n" + teamBlock + "\n"

	codexAgentsPublicPolicy.Store(true)
	got, changed, ok := codexAgentsApply(existing)
	if !ok || !changed {
		t.Fatalf("public sync must replace a team block: changed=%t ok=%t", changed, ok)
	}
	if strings.Contains(got, "create_workflow_task") || strings.Contains(got, "workflow") {
		t.Fatalf("public block still mentions the workflow pipeline:\n%s", got)
	}
	if !strings.Contains(got, "SOLU_CONTEXT_PACK") || !strings.HasPrefix(got, "user notes\n") {
		t.Fatalf("public block must keep context packs and the user's text:\n%s", got)
	}
}

func TestCodexAgentsRemove(t *testing.T) {
	block := codexAgentsManagedBlock()
	got, changed, ok := codexAgentsRemove("user notes\n\n" + block + "\nmore\n")
	if !ok || !changed || got != "user notes\nmore\n" {
		t.Fatalf("remove = %q changed=%t ok=%t", got, changed, ok)
	}
	if got, changed, ok := codexAgentsRemove("plain\n"); !ok || changed || got != "plain\n" {
		t.Fatal("a file without our block must be untouched")
	}
	if _, _, ok := codexAgentsRemove(codexAgentsBeginMarker + "\n" + codexAgentsBeginMarker); ok {
		t.Fatal("an ambiguous marker shape must be left alone")
	}
}

func TestCodexConfigRemoveServer(t *testing.T) {
	body := "model = \"x\"\n\n[mcp_servers.soluterminal]\nurl = \"http://127.0.0.1:8787/mcp\"\n\n[mcp_servers.soluterminal.env_http_headers]\nX = \"Y\"\n\n[mcp_servers.other]\nurl = \"http://o\"\n"
	got, changed := codexConfigRemoveServer(body, "soluterminal")
	if !changed {
		t.Fatal("expected a change")
	}
	if strings.Contains(got, "soluterminal") || !strings.Contains(got, "[mcp_servers.other]") || !strings.Contains(got, `model = "x"`) {
		t.Fatalf("unexpected result:\n%s", got)
	}
	if _, changed := codexConfigRemoveServer("[mcp_servers.other]\n", "soluterminal"); changed {
		t.Fatal("no soluterminal table means no change")
	}
}
