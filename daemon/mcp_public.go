package main

import "encoding/json"

// The public beta's MCP surface. Installs without the team entitlement see
// only the tools that work on the terminal app itself; the workflow
// orchestrator, task owners and managed agents are team-only, and so is
// the long delegation brief in mcpServerInstructions.

var mcpPublicToolNames = map[string]bool{
	"list_panes":           true,
	"read_pane_tail":       true,
	"ask_pane":             true,
	"wait_for_pane":        true,
	"get_pane_state":       true,
	"get_pane_livecwd":     true,
	"send_input":           true,
	"answer_prompt":        true,
	"spawn_pane":           true,
	"kill_pane":            true,
	"rename_pane":          true,
	"set_pane_info":        true,
	"set_pane_flag":        true,
	"list_workspaces":      true,
	"switch_workspace":     true,
	"list_ports":           true,
	"open_browser":         true,
	"close_browser":        true,
	"preview_file":         true,
	"read_file":            true,
	"search_files":         true,
	"git_status":           true,
	"write_file_line":      true,
	"read_activity":        true,
	"list_schedules":       true,
	"create_schedule":      true,
	"set_schedule_enabled": true,
	"cancel_schedule":      true,
}

const mcpPublicServerInstructions = "soluterminal is the user's multi-pane terminal workspace. " +
	"Panes are numbered #1, #2, … per workspace and may have an @handle (set_pane_info); either works as pane_id. list_panes shows them, states and which pane is you. " +
	"To work with the agent in another pane (for example \"ask Codex in pane 2 to review this\"), use ask_pane: it sends the message, waits for that agent to finish and returns only its reply. " +
	"wait_for_pane waits for a pane you already gave work to; read_pane_tail shows an agent's latest reply (or a shell's screen) without waiting. " +
	"If ask_pane reports needs_approval, the other agent is on a permission menu: answer it with answer_prompt only if the user asked you to, otherwise tell the user. " +
	"spawn_pane opens a new pane (pass prompt to give a new claude/codex agent its first message, then wait_for_pane); send_input types raw keys; rename_pane and kill_pane manage sessions; list_workspaces and switch_workspace for saved layouts. " +
	"preview_file opens a file in a viewer tile for the user; list_ports and open_browser show a dev server in a browser tile. " +
	"list_schedules and create_schedule manage scheduled prompts. " +
	"Your own pane id is in the SOLU_PANE_ID environment variable."

// publicMCPTool adapts a tool's schema for the public surface. spawn_pane's
// "role" parameter only creates team-only Orchestrator panes, so it is not
// advertised.
func publicMCPTool(t mcpTool) mcpTool {
	if t.Name != "spawn_pane" {
		return t
	}
	var schema map[string]any
	if json.Unmarshal(t.InputSchema, &schema) != nil {
		return t
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		delete(props, "role")
	}
	if b, err := json.Marshal(schema); err == nil {
		t.InputSchema = b
	}
	return t
}
