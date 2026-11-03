package main

import "encoding/json"

// The public beta's MCP surface. Installs without the team entitlement see
// only the tools that work on the terminal app itself; the workflow
// orchestrator, task owners and managed agents are team-only, and so is
// the long delegation brief in mcpServerInstructions.

var mcpPublicToolNames = map[string]bool{
	"list_panes":             true,
	"read_pane_tail":         true,
	"ask_pane":               true,
	"wait_for_pane":          true,
	"get_pane_state":         true,
	"get_pane_livecwd":       true,
	"send_input":             true,
	"answer_prompt":          true,
	"spawn_pane":             true,
	"cancel_queued_worker":   true,
	"kill_pane":              true,
	"rename_pane":            true,
	"set_pane_info":          true,
	"set_pane_flag":          true,
	"list_workspaces":        true,
	"move_pane_to_workspace": true,
	"rename_workspace":       true,
	"delete_workspace":       true,
	"switch_workspace":       true,
	"create_workspace":       true,
	"list_ports":             true,
	"open_browser":           true,
	"close_browser":          true,
	"preview_file":           true,
	"read_file":              true,
	"search_files":           true,
	"git_status":             true,
	"write_file_line":        true,
	"read_activity":          true,
	"post_user_ask":          true,
	"resolve_user_ask":       true,
	"list_schedules":         true,
	"create_schedule":        true,
	"set_schedule_enabled":   true,
	"cancel_schedule":        true,
}

const mcpPublicServerInstructions = "The user's multi-pane terminal workspace. " +
	"list_panes: states, workspace/#numbers, open_asks person count, flagOwner bookmark owner; worker_notices marks escalations. ids/#numbers/@handles address panes. " +
	"Message agents with ask_pane, never send_input; wait_for_pane waits, read_pane_tail reads. send_input/ask_pane messages wait while the person types, until the box is empty and typing stops; do not resend. Put-back messages need their Send/Clear; held messages wait behind them. " +
	"post_user_ask: one decision/action each, your own only; resolve_user_ask when done. Do not repeat open asks. Flags are user bookmarks. " +
	"needs_approval: answer_prompt only when the user asked. spawn_pane opens sessions. Never close leads. Workers archive once finished/read/committed/pushed; close plain finished panes you started unless asked to keep. " +
	"Pane/workspace writes: MCP, not REST. create_workspace: empty, no overwrite, 5/hour/verified pane, 20/hour total. Move to existing layouts only. Moving others, rename or delete (delete: empty only): user_requested + Allow even with Agent permissions. No _current. " +
	"Unverified: explicit targets + user request + Allow; no cmd:auto/starter/auto-archive."

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
