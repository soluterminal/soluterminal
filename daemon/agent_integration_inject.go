package main

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-process tools and hooks for SoluTerminal's own panes. When the global
// registration is not on (agent_integration.go), panes still need the
// soluterminal MCP server and the session hooks, so they are passed to the
// one process instead of written into the user's config:
//
//   - Claude Code: --mcp-config <json> (adds to the user's servers; no
//     --strict-mcp-config) and --settings <json> carrying our hooks. Windows
//     .cmd launches materialize those values as user-private JSON files at
//     the child boundary (claudeConfigFileArgs).
//   - Codex: -c mcp_servers.soluterminal.url=… and its pane header, plus the
//     context-pack note as developer_instructions when a pack is attached.
//
// Applied where SoluTerminal starts the CLI: the pane spawn for a direct
// `claude`/`codex` pane, and the pane-local shims that `claude`/`codex` typed
// in a Unix pane shell resolve to. A CLI started outside SoluTerminal (another
// terminal, a Windows PowerShell pane's typed `codex`) gets nothing — that is
// the point of the opt-in.

// claudeSkipInjectSubcommands are management subcommands that start no
// session and must not receive session flags.
var claudeSkipInjectSubcommands = map[string]bool{
	"mcp": true, "config": true, "update": true, "doctor": true, "install": true,
	"migrate-installer": true, "setup-token": true, "plugin": true, "plugins": true,
	"auth": true, "login": true, "logout": true,
}

// soluReadOnlyMCPTools are the soluterminal tools that only read. Agents use
// them without a permission prompt: Claude Code through a --settings allow
// rule, Codex through per-tool approval_mode. Tools that act (send_input,
// ask_pane, answer_prompt, spawn_pane, kill_pane, …) still ask.
var soluReadOnlyMCPTools = []string{
	"list_panes", "get_pane_state", "read_pane_tail", "wait_for_pane", "get_pane_livecwd",
	"list_workspaces", "list_ports", "read_file", "search_files", "git_status",
	"read_activity", "list_schedules",
}

// codexSoluToolTimeoutSec lifts Codex's 60 s default MCP tool timeout so an
// ask_pane can wait for the other agent's whole turn.
const codexSoluToolTimeoutSec = 900

func claudeSoluReadOnlyAllowRules() []any {
	out := make([]any, 0, len(soluReadOnlyMCPTools))
	for _, t := range soluReadOnlyMCPTools {
		out = append(out, "mcp__soluterminal__"+t)
	}
	return out
}

var codexSkipInjectSubcommands = map[string]bool{
	"login": true, "logout": true, "mcp": true, "mcp-server": true, "completion": true,
	"apply": true, "debug": true, "app-server": true, "help": true,
}

func cliArgsHasAny(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// agentInjectionRecord loads the choice for a data dir and reports whether
// per-process injection applies. A missing file (a daemon that has not booted
// this version yet) means no injection: the global entries, if any, still
// serve, and adding a second "soluterminal" server could collide with them.
func agentInjectionRecord(dataDir string) (agentIntegrationRecord, bool) {
	if dataDir == "" {
		return agentIntegrationRecord{}, false
	}
	rec := readAgentIntegrationRecord(filepath.Join(dataDir, agentIntegrationFile))
	if rec.MCPURL == "" || rec.HookURL == "" {
		return rec, false
	}
	return rec, rec.effective() != agentIntegrationOn
}

// claudeInjectArgs appends the per-process MCP server and hooks to a Claude
// Code argv (without the binary).
func claudeInjectArgs(rec agentIntegrationRecord, paneID string, args []string) []string {
	if len(args) > 0 && claudeSkipInjectSubcommands[args[0]] {
		return args
	}
	if cliArgsHasAny(args, "-v", "--version", "-h", "--help", "--strict-mcp-config") {
		return args
	}
	headerValue, commandValue := "${SOLU_PANE_ID}", "${SOLU_PANE_ID}"
	if paneID != "" && !strings.ContainsAny(paneID, "\"'$`\\ ") {
		headerValue, commandValue = paneID, paneID
	}
	out := append([]string(nil), args...)
	if !cliArgsHasAny(args, "--mcp-config") {
		cfg := map[string]any{"mcpServers": map[string]any{"soluterminal": map[string]any{
			"type":    "http",
			"url":     rec.MCPURL,
			"headers": map[string]any{claudeMcpPaneHeaderName: headerValue},
		}}}
		b, _ := json.Marshal(cfg)
		out = append(out, "--mcp-config", string(b))
	}
	if !cliArgsHasAny(args, "--settings") {
		specs := claudeHookSpecs(rec.HookURL, headerValue, commandValue)
		hooks := map[string]any{}
		for _, event := range claudeHookEvents {
			hooks[event] = []any{specs[event]}
		}
		b, _ := json.Marshal(map[string]any{
			"hooks":       hooks,
			"permissions": map[string]any{"allow": claudeSoluReadOnlyAllowRules()},
		})
		out = append(out, "--settings", string(b))
	}
	return out
}

// codexInjectArgs appends the per-process MCP server (and the context-pack
// note when a pack is attached) to a Codex argv (without the binary).
func codexInjectArgs(rec agentIntegrationRecord, contextPack string, args []string) []string {
	if len(args) > 0 && codexSkipInjectSubcommands[args[0]] {
		return args
	}
	if cliArgsHasAny(args, "-V", "--version", "-h", "--help") {
		return args
	}
	for _, a := range args {
		if strings.HasPrefix(a, "mcp_servers.soluterminal.") {
			return args
		}
	}
	out := append([]string(nil), args...)
	out = append(out,
		"-c", "mcp_servers.soluterminal.url="+strconv.Quote(rec.MCPURL),
		"-c", `mcp_servers.soluterminal.env_http_headers={"`+claudeMcpPaneHeaderName+`"="SOLU_PANE_ID"}`,
		"-c", "mcp_servers.soluterminal.tool_timeout_sec="+strconv.Itoa(codexSoluToolTimeoutSec),
	)
	for _, t := range soluReadOnlyMCPTools {
		out = append(out, "-c", "mcp_servers.soluterminal.tools."+t+`.approval_mode="approve"`)
	}
	if strings.TrimSpace(contextPack) != "" {
		out = append(out, "-c", "developer_instructions="+strconv.Quote(strings.TrimSpace(codexAgentsContextPackLine)))
	}
	return out
}

// injectAgentArgsForPane rewrites a full pane command (binary first) when it
// starts Claude Code or Codex and the global registration is not on.
func injectAgentArgsForPane(dataDir, paneID string, env []string, cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}
	rec, ok := agentInjectionRecord(dataDir)
	if !ok {
		return cmd
	}
	switch {
	case cmdStartsClaude(cmd):
		return append([]string{cmd[0]}, claudeInjectArgs(rec, paneID, cmd[1:])...)
	case cmdStartsCodex(cmd):
		return append([]string{cmd[0]}, codexInjectArgs(rec, envValueFromList(env, "SOLU_CONTEXT_PACK"), cmd[1:])...)
	}
	return cmd
}
