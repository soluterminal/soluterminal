package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const mcpProtocolVersion = "2024-11-05"

// Served verbatim in the MCP initialize response and embedded by reference in
// the daemon-managed ~/.codex/AGENTS.md block (codex_agents_md.go). Edit this
// constant and both surfaces update on the next daemon start. Sessions work
// directly and coordinate through panes; the delegate-first workflow contract
// lives only in the opt-in orchestrator role (orchestrator_policy.go).
const mcpServerInstructions = mcpDirectServerInstructions

// The engine-free brief is also used by the generated global Codex policy.
// Keep schedules and pane coordination available without
// advertising task lifecycle or repair capabilities that cannot run.
const mcpDirectServerInstructions = "soluterminal is the user's multi-pane terminal workspace. Work directly in your own session: investigate, implement and test your changes. Keep any git worktree or clone you create inside its repository (for example .claude/worktrees/<name>, after making sure .gitignore excludes that folder), never in a temporary folder such as /tmp that can be cleared on restart, and commit work in progress early. The workflow task engine is unavailable. Coordinate with other panes through list_panes, ask_pane, wait_for_pane and read_pane_tail. When you start a pane with spawn_pane, give it a title, purpose and handle so the user knows what it is, address it by its @handle afterwards, and close it with kill_pane once you have its result unless the user wants it kept. Pick each worker's agent for its task (cmd claude or codex, optionally with model and effort): work that writes code goes to a visible worker pane, and in-session subagents are for read-only research.Keep your own pane's working_on line current with set_pane_info as your work moves on. Before handing a change back for merge, follow the repository AGENTS.md independent-review procedure (in soluterminal run scripts/solu-review, at most two rounds), then report the change, evidence, review and open issues to the user, who merges. Preserve user authorization and required approval gates."

// SOLU_MCP_MUTATION_POLICY controls MCP tools that directly mutate local
// state without the ARIA confirmation channel. Default "allow" preserves
// existing loopback/local-dev behavior; set to "deny" (or "readonly") to
// expose the MCP server as read-only-ish while retaining inspector tools.
const mcpMutationPolicyEnv = "SOLU_MCP_MUTATION_POLICY"

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // can be string, number, or null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (a *apiServer) mcp(w http.ResponseWriter, r *http.Request) {
	// Scoped capability token? Resolved BEFORE the general auth gate: a
	// scoped token both authorizes the request AND restricts it to its
	// tool set — even from loopback, which is the whole point (a
	// daemon-spawned child is a loopback caller too, and must not fall
	// through to the tokenless-loopback full-access allowance).
	var scope *mcpScope
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if sc, ok := a.mcpScopes.Lookup(strings.TrimPrefix(h, "Bearer ")); ok {
			scope = sc
		}
	}
	// Defense in depth — /mcp answers loopback callers, plus non-loopback
	// callers that present the daemon token (Bearer / cookie). The cloud
	// relay bypasses authGate and carries no token, so relay-tunnelled
	// requests still fall through to the reject below.
	if scope == nil && (a.gate == nil || !a.gate.isAuthed(r)) {
		http.Error(w, "mcp endpoint requires loopback or a valid token", http.StatusForbidden)
		return
	}

	// Some clients do a GET first expecting an SSE stream. We don't need
	// server-initiated messages, so return a minimal open stream that
	// keeps the client happy — all real work happens on POST.
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		if f, ok := w.(http.Flusher); ok {
			_, _ = fmt.Fprintf(w, ": ok\n\n")
			f.Flush()
		}
		<-r.Context().Done()
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 200, rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	defer r.Body.Close()

	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 200, rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}

	// Notifications (no ID) are fire-and-forget; the spec says the server
	// MUST NOT reply. Accept and return 202.
	if strings.HasPrefix(req.Method, "notifications/") || len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Tracing reads the process table (tens of milliseconds), so only tool
	// calls pay for it; initialize and tools/list read the header, which
	// only shapes what is advertised (tools/call checks the surface again).
	callerPane := mcpCallerPane(r)
	if req.Method == "tools/call" {
		callerPane = a.mcpTracedCallerPane(r)
	}

	writeJSON(w, 200, a.mcpDispatchTransport(req, scope, callerPane))
}

// mcpTracedCallerPane is the pane an MCP call comes from, as far as the
// daemon can tell. It decides which panes the caller owns (kill_pane,
// rename_pane, set_pane_info), who a message is from (send_input, ask_pane),
// who started a pane or schedule, and which tool surface the caller gets.
//
// The X-Soluterminal-Pane header is the caller's own word: any agent can call
// /mcp on loopback and name another pane. So the connection is traced to its
// process first (caller_authority.go). A process inside a pane is that pane,
// whatever the header says. A process the daemon started, or any other traced
// program, is no pane and owns none. The header counts only when the
// connection cannot be traced (a Windows process reaching a WSL daemon, WSL's
// relayed loopback): the limit SECURITY.md documents. A process placed only
// by SOLU_PANE_ID in its environment is no pane either (see below).
func (a *apiServer) mcpTracedCallerPane(r *http.Request) string {
	header := mcpCallerPane(r)
	id := a.identifyCaller(r)
	switch {
	case id.Kind == callerPane && id.PaneID != "" && id.FromEnv:
		// No pane above the process (setsid, a detached tmux or screen
		// server): the pane id is only what its environment says, as
		// self-declared as the header. It owns no panes; the confirmation
		// prompt names the pane it claims, marked as unverified.
		return mcpClaimedCallerPrefix + id.PaneID
	case id.Kind == callerPane && id.PaneID != "":
		if header != "" && header != id.PaneID {
			noteMCPCallerMismatch(id.PaneID, header, id.PID)
		}
		return id.PaneID
	case id.Kind == callerWorker:
		return ""
	case id.PID > 0 && !peerIsRelayProxy(id.PID):
		// Traced to a program outside the panes (an agent in another
		// terminal, a script): it owns no panes, whatever it claims.
		return ""
	}
	return header
}

// mcpClaimedCallerPrefix marks a caller pane that could not be verified
// (mcpTracedCallerPane). It never reaches a tool: mcpCallToolTransport strips
// it, and a header can't carry a NUL byte.
const mcpClaimedCallerPrefix = "\x00claimed:"

// splitMCPCaller separates a verified caller pane from a claimed one.
func splitMCPCaller(callerPane string) paneCaller {
	if claimed, ok := strings.CutPrefix(callerPane, mcpClaimedCallerPrefix); ok {
		return paneCaller{Claimed: claimed}
	}
	return paneCaller{ID: callerPane}
}

// mcpCallerMismatches rate-limits the log line for a traced pane that names
// another pane in its header: one line per pair per interval.
var mcpCallerMismatches = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

const mcpCallerMismatchEvery = 10 * time.Minute

func noteMCPCallerMismatch(traced, header string, pid int) {
	key := traced + "\x00" + header
	now := time.Now()
	mcpCallerMismatches.Lock()
	if t, ok := mcpCallerMismatches.last[key]; ok && now.Sub(t) < mcpCallerMismatchEvery {
		mcpCallerMismatches.Unlock()
		return
	}
	if len(mcpCallerMismatches.last) >= 256 {
		mcpCallerMismatches.last = map[string]time.Time{}
	}
	mcpCallerMismatches.last[key] = now
	mcpCallerMismatches.Unlock()
	log.Printf("mcp: caller traced to pane %s (pid %d) named pane %q in X-Soluterminal-Pane; acting as %s", traced, pid, header, traced)
}

// mcpCallerPane reads the X-Soluterminal-Pane header the pane's MCP client
// sends. It is self-declared: mcpTracedCallerPane decides when it counts. It
// tolerates both missing pane headers from existing sessions and literal,
// unexpanded environment-variable headers.
func mcpCallerPane(r *http.Request) string {
	paneID := strings.TrimSpace(r.Header.Get("X-Soluterminal-Pane"))
	if strings.Contains(paneID, "$") {
		return ""
	}
	return paneID
}

func (a *apiServer) mcpDispatch(req rpcReq, scope *mcpScope, callerPane string) rpcResp {
	return a.mcpDispatchTransport(req, scope, callerPane)
}

func (a *apiServer) mcpDispatchTransport(req rpcReq, scope *mcpScope, callerPane string) rpcResp {
	base := rpcResp{JSONRPC: "2.0", ID: req.ID}
	// Public installs (not operator) get the terminal tools only: no
	// workflow, task-owner or managed-agent tools, and short neutral
	// instructions instead of the delegation brief. Scoped callers (managed
	// agents, the Warden) only exist on operator installs. The personal view
	// gives panes started while it is on the same public surface.
	public := scope == nil && (!a.operatorFeaturesOn() || a.personalViewForPane(splitMCPCaller(callerPane).ID))
	if public && req.Method == "tools/call" {
		var p toolCallParams
		if err := json.Unmarshal(req.Params, &p); err == nil && !mcpPublicToolNames[p.Name] {
			base.Result = toolErr("unknown tool: " + p.Name + " is not available in this edition of soluterminal")
			return base
		}
	}
	switch req.Method {
	case "initialize":
		instructions := mcpServerInstructions
		{
			instructions = mcpDirectServerInstructions
		}
		if public {
			instructions = mcpPublicServerInstructions
		}
		base.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"instructions":    instructions,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "soluterminal",
				"version": a.ver,
			},
		}
	case "tools/list":
		tools := mcpToolsForScope(scope)
		if public {
			filtered := tools[:0]
			for _, tool := range tools {
				if mcpPublicToolNames[tool.Name] {
					filtered = append(filtered, publicMCPTool(tool))
				}
			}
			tools = filtered
		}
		base.Result = map[string]any{
			"tools": tools,
		}
	case "tools/call":
		base.Result = a.mcpCallToolTransport(req.Params, scope, callerPane)
	case "ping":
		base.Result = map[string]any{}
	default:
		base.Error = &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
	}
	return base
}

// mcpToolsForScope narrows the advertised tool list to the caller's scope.
// A nil scope (interactive panes, tokenless loopback) sees everything. A
// scoped caller must not even SEE out-of-scope tools — advertising a tool
// the call layer would refuse just burns the model's attempts.
func mcpToolsForScope(scope *mcpScope) []mcpTool {
	all := mcpTools()
	if scope == nil {
		return all
	}
	out := make([]mcpTool, 0, len(scope.Tools))
	for _, t := range all {
		if scope.allows(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// mcpTools defines the tools the server advertises. Descriptions are
// consumed directly by Claude — wording should guide the model toward
// *when* to reach for each one, not just what it does.
func mcpTools() []mcpTool {
	return withExtensionTools(mcpCoreTools())
}

func mcpCoreTools() []mcpTool {
	return append([]mcpTool{
		{
			Name: "preview_file",
			Description: "Open a file in the soluterminal preview tile for the user to view. " +
				"Prefer this over `cat`/`head`/`Read`-and-paste when the user asks you to " +
				"open, show, display, or look at a file — it gives them a line-by-line " +
				"viewer inside the UI, with automatic masking for .env-style values, " +
				"rather than dumping contents into the terminal. Only works for text " +
				"files under 512 KB.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Absolute path, or path starting with ~, to the file to preview. Must exist and be a text file."
					}
				},
				"required": ["path"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "list_panes",
			Description: "List every open soluterminal pane with its id, its #number and workspace " +
				"as the user sees them in the app, a label like \"#2 Codex\", the live state " +
				"(busy, idle, awaiting-input, choice-prompt, exited), agent identity (claude | " +
				"codex | muse, empty for a plain shell), title, cwd and flagged state. `you: true` " +
				"marks your own pane. When the user says \"pane 2\" they mean number 2 in their " +
				"workspace. The ids (or #numbers) are the inputs to ask_pane, wait_for_pane, " +
				"read_pane_tail, get_pane_state, send_input and kill_pane.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
		},
		{
			Name: "read_pane_tail",
			Description: "Read what a pane is showing. For a Claude Code or Codex pane the default " +
				"(mode \"reply\") returns that agent's latest reply and the prompt it answered, " +
				"taken from its own conversation log — no old sessions, no screen debris. For " +
				"other panes the default (mode \"screen\") is the current screen. mode \"raw\" " +
				"returns the ANSI-stripped scrollback (`lines`, default 80). To send a message and " +
				"wait for the answer in one step, use ask_pane instead.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"mode": {
						"type": "string",
						"enum": ["reply", "screen", "raw"],
						"description": "reply (agent panes' default), screen (other panes' default) or raw scrollback."
					},
					"lines": {
						"type": "number",
						"description": "raw mode only: number of trailing lines. Defaults to 80, capped at 500."
					}
				},
				"required": ["pane_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "get_pane_state",
			Description: "Return the runtime state of a pane — one of busy, idle, recent, " +
				"awaiting-input, choice-prompt, or exited. Use this for a quick check of " +
				"whether a pane is working, finished, or waiting for a human, without " +
				"pulling its full scrollback.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					}
				},
				"required": ["pane_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "ask_pane",
			Description: "Ask the agent in another pane something and get its answer: sends `text` " +
				"as a message, waits until that agent has finished its turn, and returns only its " +
				"reply (JSON: pane, status, reply). Use this instead of send_input + sleeping + " +
				"read_pane_tail. status is replied, needs_approval (the other agent is waiting on a " +
				"permission menu: answer_prompt it or tell the user, then wait_for_pane), " +
				"still_working (timed out: call wait_for_pane) or exited. Refused if the pane is " +
				"busy or on a menu, or is your own pane.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number (e.g. \"#2\") or @handle as returned by list_panes."
					},
					"text": {
						"type": "string",
						"description": "The message for the other agent, as you would type it."
					},
					"timeout_seconds": {
						"type": "number",
						"description": "How long to wait for the reply. Default 240, max 900."
					}
				},
				"required": ["pane_id", "text"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "wait_for_pane",
			Description: "Wait until another pane's agent finishes what it is doing, then return its " +
				"latest reply (JSON: pane, status, state, reply). status is idle, needs_approval, " +
				"still_working (timed out; call again) or exited. Use after spawn_pane with a prompt, " +
				"after ask_pane returned still_working, or after send_input.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"timeout_seconds": {
						"type": "number",
						"description": "How long to wait. Default 120, max 900."
					}
				},
				"required": ["pane_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "send_input",
			Description: "Write keystrokes to a pane's terminal (stdin). This is a mutating tool — " +
				"it types directly into a live session. Include a trailing newline in `text` " +
				"to submit a line (e.g. \"ls\\n\" to run a command). To answer an approval/choice " +
				"menu use answer_prompt. While a pane shows such a menu, only a one- or " +
				"two-key answer is accepted here; longer text is refused unsent (the menu " +
				"would read it as keystrokes) — retry after the prompt resolves. Refused if " +
				"the pane has already exited. The pane_id must come from list_panes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"text": {
						"type": "string",
						"description": "Text to send. Include a trailing \"\\n\" to submit the line."
					}
				},
				"required": ["pane_id", "text"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "answer_prompt",
			Description: "Answer a pane that is waiting on a permission or choice prompt — the " +
				"safe way to unblock another session. Prefer this over send_input for prompts: " +
				"it can ONLY pick an option the pane is already showing, never free-type. For a " +
				"terminal pane in choice-prompt state, `option` is the menu number (\"1\", \"2\", …); " +
				"the number must be one currently on the menu. For a native (Focus) pane, `option` " +
				"is \"allow\" or \"deny\". Refused if the pane is not actually on a prompt (it will " +
				"not act on a running/idle pane or a prose \"should I continue?\" question). Get " +
				"the state first with get_pane_state, and the menu text with read_pane_tail.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"option": {
						"type": "string",
						"description": "Terminal pane: the menu number to select (\"1\", \"2\", …). Native pane: \"allow\" or \"deny\"."
					}
				},
				"required": ["pane_id", "option"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "spawn_pane",
			Description: "Create a new soluterminal pane and return JSON metadata {id, workspace}. Use this to open a " +
				"fresh terminal session for the user — a shell, a Claude Code session, a " +
				"REPL. All arguments are optional: cwd defaults to the user's home, cmd " +
				"defaults to the user's shell, and workspace defaults to the calling pane's named " +
				"workspace (or the most recently active visible workspace when the caller is unknown). " +
				"For an agent (cmd \"claude\" or \"codex\") it waits until the agent shows its input " +
				"box (up to ~45 s). If folder trust is waiting, it returns status needs_user immediately, " +
				"leaves the prompt unsent, and asks the user to trust the folder; afterward use ask_pane to send the original prompt. " +
				"Otherwise it sends `prompt` as the first message when set; call wait_for_pane for the reply. " +
				"Set role to orchestrator to start Claude or Codex with " +
				"the chief-of-staff brief. Always pass title (a short name, e.g. \"Engine deletion\"), " +
				"purpose (one sentence) and handle (e.g. \"engine-delete\"): the user sees them on the pane and in " +
				"the notice that you started it, and the handle addresses the pane in later calls.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"cwd": {
						"type": "string",
						"description": "Working directory for the new pane. Absolute path, or path starting with ~. Optional — defaults to the user's home."
					},
					"cmd": {
						"type": "string",
						"description": "Command to run in the pane (e.g. \"claude\", \"codex\", \"bash\"). Optional — defaults to the user's shell. Parsed as a single argv0; arguments are not supported here (use model and effort for an agent)."
					},
					"model": {
						"type": "string",
						"description": "Optional model for a claude or codex pane, passed as the agent's own model flag (e.g. \"opus\" or \"sonnet\" for Claude, a Codex model id for Codex). Omit to use the agent's default."
					},
					"effort": {
						"type": "string",
						"enum": ["low", "medium", "high", "xhigh", "max"],
						"description": "Optional reasoning effort for a claude or codex pane. Codex takes low to xhigh, and max only on models that support it."
					},
					"workspace": {
						"type": "string",
						"description": "Optional visible named workspace to attach the new pane to, matched case-insensitively. Defaults to the calling pane's workspace, then the most recently active visible workspace. The response contains the canonical stored workspace name."
					},
					"title": {
						"type": "string",
						"description": "Short name shown on the new pane (≤ 48 characters), e.g. \"Engine deletion\". Without it the pane is named after its agent and the pane that started it."
					},
					"purpose": {
						"type": "string",
						"description": "One sentence on what the new pane is for. Shown in the pane's details; defaults to the first line of prompt."
					},
					"handle": {
						"type": "string",
						"description": "Short unique name for the new pane (e.g. \"api-fix\"): lowercase letters, digits, dashes. Use it as pane_id afterwards (\"@api-fix\"); it stays put when pane numbers change."
					},
					"role": {
						"type": "string",
						"description": "Optional pane role. \"orchestrator\" spawns the pane with the chief-of-staff orchestrator brief; requires cmd \"claude\" or \"codex\"."
					},
					"prompt": {
						"type": "string",
						"description": "Optional first message for the new agent, sent once it is ready for input."
					},
					"wait_ready": {
						"type": "boolean",
						"description": "Wait for the agent to be ready before returning. Defaults to true for claude/codex."
					}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name: "kill_pane",
			Description: "Terminate a pane and the agent process running inside it. Close the panes you " +
				"started once you have collected their result, unless the user asked to keep them. " +
				"Any other pane is the user's: closing it is refused, unless the user explicitly asked " +
				"for it in this conversation and you pass user_requested: true; the user is then asked " +
				"to allow it in the SoluTerminal app, and the pane closes only if they do. Closing " +
				"never deletes files. The pane_id must come from list_panes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"user_requested": {
						"type": "boolean",
						"description": "Set true only when the user explicitly asked you to close this pane. It does not close the pane by itself: the user is asked to allow it in the app. Not needed for panes you started."
					}
				},
				"required": ["pane_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "list_workspaces",
			Description: "List every named soluterminal workspace with its pane count and " +
				"last-active timestamp. A workspace is a saved layout of panes the user can " +
				"switch between. Use this to discover what workspaces exist before calling " +
				"switch_workspace or before spawning a pane into a specific one. The internal " +
				"'_current' autosave scratchpad is hidden.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
		},
		{
			Name: "switch_workspace",
			Description: "Make a named workspace the active one in the user's soluterminal UI — " +
				"loads its layout and mounts its panes. Use this when the user says \"switch " +
				"to <name>\" or \"open my <name> workspace\". The workspace name is matched " +
				"case-insensitively. Call list_workspaces first if you're unsure of the name.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace": {
						"type": "string",
						"description": "Workspace name to switch to. Matched case-insensitively against the names from list_workspaces."
					}
				},
				"required": ["workspace"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "list_ports",
			Description: "List local listening TCP ports the daemon can see — dev servers and the " +
				"like — with port, bind address, pid, command line, and the owning pane id " +
				"when one is known. Use this when the user asks what dev servers are running, " +
				"which port an app is on, or which pane owns a localhost port.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Optional. Restrict results to ports owned by this pane (id from list_panes)."
					}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name: "open_browser",
			Description: "Open a URL in the user's in-app browser tile — the way to SHOW the user a " +
				"running app instead of just naming it. Prefer the localhost URL from " +
				"list_ports (e.g. after starting a dev server, or when the user asks to see " +
				"what you built). http(s) only; the tile opens beside the user's focused " +
				"pane on every connected client. close_browser dismisses it.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"url": {
						"type": "string",
						"description": "Absolute http(s) URL to open, e.g. http://localhost:5173 from list_ports."
					}
				},
				"required": ["url"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "close_browser",
			Description: "Close the user's in-app browser tile (the one open_browser opens — it's " +
				"single-instance). Use when the user asks to dismiss the preview, or to tidy " +
				"up after showing something. No-op if the tile isn't open.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
		},
		{
			Name: "read_activity",
			Description: "Read the recent soluterminal activity feed — pane state changes, tool " +
				"calls, command exits, and build failures, newest last. Use this to answer " +
				"\"what's been happening\", to catch up on what panes have been doing, or to " +
				"see whether a recent action landed. Returns {events, cursor}: pass the " +
				"returned cursor back as `since` on the next call to read only what's new " +
				"(cursors reset on daemon restart).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": {
						"type": "number",
						"description": "Max number of trailing events to return. Defaults to 50, capped at 200."
					},
					"since": {
						"type": "number",
						"description": "Return only events with seq greater than this — use the cursor from the previous call. Omit or 0 for the full retained window."
					}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name: "read_file",
			Description: "Read a bounded slice of a text file and return its lines. ANSI-safe; " +
				"refuses likely secret/.env files, binary files, and oversized files. Returns " +
				"the file mtime as `mtime`; MCP write_file_line requires that nonzero value " +
				"as its optimistic-concurrency guard. Prefer " +
				"this over shell `cat`/`head` for inspecting source or config when you don't " +
				"need the user to see it in a UI tile (use preview_file for that).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Absolute path, or path starting with ~, to the file to read."
					},
					"start_line": {
						"type": "number",
						"description": "1-indexed line to start at. Defaults to 1."
					},
					"max_lines": {
						"type": "number",
						"description": "Number of lines to return. Defaults to 120, capped at 200."
					}
				},
				"required": ["path"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "get_pane_livecwd",
			Description: "Return the current working directory of the shell running inside a pane — " +
				"the directory the user has actually cd'd into, which may differ from the " +
				"pane's spawn cwd. Use this when you need to know where a shell pane \"is\" " +
				"right now, e.g. before running git_status or search_files for it. The " +
				"pane_id must come from list_panes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					}
				},
				"required": ["pane_id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "git_status",
			Description: "Read-only Git summary for a directory: repo root, branch/upstream line, " +
				"porcelain status, recent commits, and diff stats. Use this when the user asks " +
				"what changed, whether a repo is clean, or what branch they're on. Only " +
				"read-only git commands are run. Pass a directory inside the repo — pair it " +
				"with get_pane_livecwd to target the pane the user is working in.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "A directory inside a git repo. Optional — defaults to the user's home (rarely useful; prefer passing a real project dir)."
					}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name: "search_files",
			Description: "Read-only fuzzy file search under a directory. Returns matching paths " +
				"only, not contents; skips common generated/vendor directories. Use this to " +
				"locate a source, config, or document file before reading it.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {
						"type": "string",
						"description": "Fuzzy query to match against file names/paths."
					},
					"path": {
						"type": "string",
						"description": "Directory to search under. Optional — defaults to the user's home. Prefer a project directory or a pane's live cwd."
					}
				},
				"required": ["query"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "rename_pane",
			Description: "Change a pane's display title. Use when the user wants to relabel a pane " +
				"for clarity (e.g. \"rename this pane to backend\"). Does not affect the " +
				"process running inside it. You can rename your own pane and the panes you " +
				"started; any other pane is the user's: renaming it is refused, unless the user " +
				"asked for it and you pass user_requested: true; the user is then asked to allow " +
				"it in the SoluTerminal app. The pane_id must come from list_panes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"title": {
						"type": "string",
						"description": "New display title for the pane."
					},
					"user_requested": {
						"type": "boolean",
						"description": "Set true only when the user asked you to rename a pane you did not start. The rename waits for the user to allow it in the app."
					}
				},
				"required": ["pane_id", "title"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "set_pane_info",
			Description: "Set a pane's handle and/or its working-on line. The handle is a short unique " +
				"name (e.g. \"api-fix\", shown as @api-fix) that you and the user can use in place of a " +
				"pane id or #number; numbers change when panes close, handles don't. working_on is one " +
				"line on what the pane is doing now, shown on the pane and in list_panes; keep your own " +
				"current as your work moves on. Defaults to your own pane. Another pane needs to be one " +
				"you started; for anyone else's, pass user_requested: true only when the user asked, " +
				"and they are asked to allow it in the SoluTerminal app. An empty string clears a field.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle. Optional — defaults to your own pane."
					},
					"handle": {
						"type": "string",
						"description": "1–24 lowercase letters, digits or dashes with at least one letter, unique among panes. \"\" clears it."
					},
					"working_on": {
						"type": "string",
						"description": "One line (≤ 140 characters) on what the pane is doing now. \"\" clears it."
					},
					"user_requested": {
						"type": "boolean",
						"description": "Set true only when the user asked you to change a pane you did not start. The change waits for the user to allow it in the app."
					}
				},
				"additionalProperties": false
			}`),
		},
		{
			Name: "set_pane_flag",
			Description: "Flag (or unflag) a pane as \"come back to this\" — sets the sidebar " +
				"badge the user scans to find pending threads later, with an optional short " +
				"memo of why. Use when the user says \"flag this pane\", \"mark T2 so I don't " +
				"lose it\", or \"clear the flag on T2\". Set flagged=false to remove the flag " +
				"(this also clears any memo). The flag persists across daemon restarts. The " +
				"pane_id must come from list_panes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pane_id": {
						"type": "string",
						"description": "Pane id, #number or @handle as returned by list_panes."
					},
					"flagged": {
						"type": "boolean",
						"description": "True to flag the pane, false to clear the flag (and its memo)."
					},
					"memo": {
						"type": "string",
						"description": "Optional short note on why the pane is flagged (e.g. \"waiting on the migration to finish\"). Trimmed and capped at 500 chars. Ignored when flagged is false."
					}
				},
				"required": ["pane_id", "flagged"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "list_schedules",
			Description: "List scheduled prompt-injections — recurring (cron) and one-time " +
				"(run-at) jobs that fire a prompt into a pane on a timer. Each entry has its " +
				"id, name, target pane, prompt, trigger, enabled state, next-fire time, fire/" +
				"skip/error counts and last-fired time; pass includeRuns:true for recent run history. Use this to review what's " +
				"scheduled, or to find a schedule's id before calling cancel_schedule. The result " +
				"is {schedules, page}: schedules is this page and page.total counts every visible " +
				"schedule after filters. When page.truncated is true, pass page.nextCursor as cursor.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": {"type":"number","description":"Max schedules per page. Default 50, capped at 200."},
					"cursor": {"type":"string","description":"Opaque cursor from a previous response's page.nextCursor. Valid only with the same filters."},
					"nameContains": {"type":"string","description":"Optional. Case-insensitive substring match on the schedule name."},
					"enabledOnly": {"type":"boolean","description":"Optional, default false. Return only enabled schedules."},
					"includeRuns": {"type":"boolean","description":"Optional, default false. Pass true to include each schedule's recent run history (up to 20 runs each; the bulk of the payload)."}
				},
				"additionalProperties": false
			}`),
		},

		{
			Name: "create_schedule",
			Description: "Create a scheduled job. Two kinds: kind=\"task\" (RECOMMENDED) runs a " +
				"fresh headless Claude session on `prompt` inside `cwd` — no pane needed, works " +
				"while the user is away, output saved to the run history; kind=\"prompt\" types " +
				"`prompt` into an existing pane's terminal and presses Enter (needs pane_id from " +
				"list_panes, and is skipped if that pane is busy). Prefer \"task\" unless the " +
				"user explicitly wants the text to land in a pane they are watching. Pass exactly " +
				"one trigger: `cron` for a recurring job (standard 5-field cron, local timezone) " +
				"or `run_at` for a one-time fire (RFC3339 timestamp, must be in the future). " +
				"`until` and `max_fires` are recurring-only end conditions.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"kind": {
						"type": "string",
						"enum": ["task", "prompt"],
						"description": "\"task\" = fresh headless Claude session in cwd (requires cwd). \"prompt\" = type into an existing pane (requires pane_id). Defaults to \"prompt\" for backward compatibility."
					},
					"name": {
						"type": "string",
						"description": "Short human-readable name for the schedule. Optional — defaults to a name derived from the prompt."
					},
					"cwd": {
						"type": "string",
						"description": "kind=task only. Absolute path of the directory the Claude session runs in. Must already exist."
					},
					"permission": {
						"type": "string",
						"enum": ["plan", "edit"],
						"description": "kind=task only. \"plan\" (default) reads and plans but changes nothing; \"edit\" lets Claude modify files in cwd. Only pass \"edit\" when the user clearly asked for changes to be made unattended."
					},
					"timeout_min": {
						"type": "number",
						"description": "kind=task only. Give up after this many minutes (default 30, max 240)."
					},
					"pane_id": {
						"type": "string",
						"description": "kind=prompt only. Pane id (from list_panes) whose terminal the prompt is injected into."
					},
					"prompt": {
						"type": "string",
						"description": "kind=task: what Claude should do on each run (every run is a fresh session with no memory of the last, so say everything it needs). kind=prompt: the text typed into the pane."
					},
					"notify_on_run": {
						"type": "boolean",
						"description": "Push a phone notification on every successful run. Failures always notify regardless. A good default for one-time reminders."
					},
					"cron": {
						"type": "string",
						"description": "Standard 5-field cron expression for a recurring schedule (e.g. \"0 9 * * 1-5\"). Provide this OR run_at, not both."
					},
					"run_at": {
						"type": "string",
						"description": "RFC3339 timestamp for a one-time fire (e.g. \"2026-05-16T15:30:00Z\"). Must be in the future. Provide this OR cron, not both."
					},
					"until": {
						"type": "string",
						"description": "Optional RFC3339 timestamp; a recurring schedule stops firing after this instant. Recurring-only."
					},
					"max_fires": {
						"type": "number",
						"description": "Optional. A recurring schedule disables itself after this many successful fires (0 = unlimited). Recurring-only."
					}
				},
				"required": ["prompt"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "set_schedule_enabled",
			Description: "Pause or resume a schedule without deleting it — its config, counters, " +
				"and run history all survive. Use this for \"pause my nightly build check\" or " +
				"\"turn the morning digest back on\". The id must come from list_schedules.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {
						"type": "string",
						"description": "Schedule id as returned by list_schedules."
					},
					"enabled": {
						"type": "boolean",
						"description": "true resumes the schedule, false pauses it."
					}
				},
				"required": ["id", "enabled"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "cancel_schedule",
			Description: "Permanently delete a scheduled prompt-injection so it stops firing. This " +
				"is destructive — the schedule and its run history are removed (to merely " +
				"pause one, that's a separate UI action). The id must come from list_schedules.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {
						"type": "string",
						"description": "Schedule id as returned by list_schedules."
					}
				},
				"required": ["id"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "write_file_line",
			Description: "Replace exactly one line of a text file ON DISK, in place. This is a " +
				"mutating tool — it edits the real file (atomically) and there is no undo. It " +
				"replaces only the single line at `line_number`; other lines are untouched. " +
				"`expected_mtime` is an optimistic-concurrency guard: pass the file's mtime " +
				"from a prior read_file/preview, and the write is rejected if the file changed " +
				"since (re-read and retry); zero is refused for MCP writes. Intended for small, deliberate config-style edits, " +
				"not multi-line refactors — use a Claude Code pane for real code changes. " +
				"Refuses binary files, files over 512 KB, and likely secret/env/key files; new_text must not contain a newline.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Absolute path, or path starting with ~, to the text file to edit."
					},
					"line_number": {
						"type": "number",
						"description": "1-indexed line number to replace. Must exist in the file."
					},
					"new_text": {
						"type": "string",
						"description": "Replacement text for that line. Must not contain a newline."
					},
					"expected_mtime": {
						"type": "number",
						"description": "The file's last-modified time in unix milliseconds, as seen on a prior read. Must be nonzero; the write is rejected with a mismatch error if the file changed since."
					}
				},
				"required": ["path", "line_number", "new_text", "expected_mtime"],
				"additionalProperties": false
			}`),
		},
	}, removedTodoMCPTools()...)
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// MCP request metadata belongs to the transport envelope, not arguments.
	// Decode it as opaque data; never use it for caller or consent authority.
	Meta map[string]any `json:"_meta,omitempty"`
}

func (a *apiServer) mcpCallTool(params json.RawMessage, scope *mcpScope, callerPane string) any {
	return a.mcpCallToolTransport(params, scope, callerPane)
}

func (a *apiServer) mcpCallToolTransport(params json.RawMessage, scope *mcpScope, callerPane string) any {
	// An unverified caller acts as no pane; only the pane tools that ask the
	// person keep its claim, to say so in the prompt.
	caller := splitMCPCaller(callerPane)
	callerPane = caller.ID
	var p toolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return toolErr("invalid params: " + err.Error())
	}
	// Scope enforcement comes FIRST — a scoped caller's budget is a hard
	// daemon-side wall, checked before any policy or handler logic runs.
	if scope != nil && !scope.allows(p.Name) {
		return toolErr("tool " + p.Name + " is not in this session's scope")
	}
	// Todos was removed: its old tool names answer one fixed notice and do
	// nothing else, so no guard, activity row or handler runs for them.
	if removedTodoToolNames[p.Name] {
		return a.mcpToolTodosRemoved()
	}
	// "#3" and "@api-fix" become the pane's id before any guard reads it.
	a.resolvePaneIDArgument(p.Arguments, callerPane)
	if guarded, denied := a.runMCPCallGuards(p.Name, p.Arguments, scope); denied != nil {
		return denied
	} else {
		p.Arguments = guarded
	}
	if err := mcpCheckMutatorAllowed(p.Name); err != nil {
		return toolErr(err.Error())
	}
	// callerIdent is the provenance stamped on artifacts this call creates
	// (pane-flag owner). Full-access callers are "agent" — an interactive
	// pane session; the human distinction ("user") belongs to UI/HTTP paths.
	callerIdent := "agent"
	if scope != nil {
		callerIdent = scope.Identity
	}
	// Emit a redacted tool-call event on the uiBus so the Activity
	// feed can show "tool: preview_file(config.yaml)" style rows
	// without logging full arguments (paths may include sensitive
	// segments; bigger tools like Edit could ship file contents).
	// Hint captures just enough to render a human label — the basename
	// of `path` when present, truncated; fields beyond Tool + Hint are
	// intentionally not forwarded.
	if a.uiBus != nil {
		hint := redactToolHint(p.Name, p.Arguments)
		// Pane ids are opaque in the feed — upgrade to the pane's title so
		// the row reads "send_input(website/stripe)" instead of a hex id.
		if pid, ok := p.Arguments["pane_id"].(string); ok && pid != "" {
			if pane, found := a.mgr.Get(pid); found {
				if title := pane.Snapshot().Title; title != "" {
					hint = strings.Replace(hint, pid, title, 1)
				}
			}
		}
		a.uiBus.publish(uiEvent{
			Type: "tool-call",
			Ts:   time.Now().UnixMilli(),
			Tool: p.Name,
			Hint: hint,
		})
	}
	// The check-in's agentsTogetherDay yes/no (install_checkin_activity.go):
	// one pane's agent driving another pane through these tools.
	crossPane := a.crossPaneTarget(p.Name, p.Arguments, callerPane)
	switch p.Name {
	case "preview_file":
		return a.mcpToolPreviewFile(p.Arguments)
	case "list_panes":
		return a.mcpToolListPanes(p.Arguments, callerPane)
	case "read_pane_tail":
		return a.noteCrossPaneTool(crossPane, a.mcpToolReadPaneTail(p.Arguments, callerPane))
	case "ask_pane":
		return a.noteCrossPaneTool(crossPane, a.mcpToolAskPane(p.Arguments, callerPane))
	case "wait_for_pane":
		return a.noteCrossPaneTool(crossPane, a.mcpToolWaitForPane(p.Arguments, callerPane))
	case "get_pane_state":
		return a.mcpToolGetPaneState(p.Arguments)
	case "send_input":
		// Keys sent to a pane at a permission prompt answer it; an
		// interactive agent needs the person's grant for that.
		if scope == nil && !a.agentAuthority.active(time.Now()) {
			if target, ok := a.mgr.Get(mcpArgString(p.Arguments, "pane_id")); ok && target.Snapshot().Kind != "native" && target.computeStateCached() == StateChoicePrompt {
				return toolErr("pane " + target.ID + " is showing a permission prompt. Agents can't answer it on their own; the person can answer it in the SoluTerminal app, or turn on Settings → Devices → Agent permissions.")
			}
		}
		return a.noteCrossPaneTool(crossPane, a.mcpToolSendInput(p.Arguments, callerPane))
	case "answer_prompt":
		// Answering a prompt is acting for the person. Unscoped callers are
		// interactive agents and need the person's agent-permissions grant; a
		// managed agent's scope already carries the grant a person gave it.
		if scope == nil && !a.agentAuthority.active(time.Now()) {
			return toolErr("Agents can't answer prompts on their own. The person can answer it in the SoluTerminal app, or turn on Settings → Devices → Agent permissions to let agents in panes do it.")
		}
		return a.noteCrossPaneTool(crossPane, a.mcpToolAnswerPrompt(p.Arguments))
	case "spawn_pane":
		return a.noteCrossPaneTool(crossPane, a.mcpToolSpawnPane(p.Arguments, callerPane, false))
	case "kill_pane":
		return a.mcpToolKillPaneAs(p.Arguments, caller)
	case "list_workspaces":
		return a.mcpToolListWorkspaces(p.Arguments)
	case "switch_workspace":
		return a.mcpToolSwitchWorkspace(p.Arguments)
	case "list_ports":
		return a.mcpToolListPorts(p.Arguments)
	case "open_browser":
		return a.mcpToolOpenBrowser(p.Arguments)
	case "close_browser":
		return a.mcpToolCloseBrowser(p.Arguments)
	case "read_activity":
		return a.mcpToolReadActivity(p.Arguments)
	case "read_file":
		return a.mcpToolReadFile(p.Arguments)
	case "get_pane_livecwd":
		return a.mcpToolGetPaneLiveCwd(p.Arguments)
	case "git_status":
		return a.mcpToolGitStatus(p.Arguments)
	case "search_files":
		return a.mcpToolSearchFiles(p.Arguments)
	case "rename_pane":
		return a.mcpToolRenamePaneAs(p.Arguments, caller)
	case "set_pane_info":
		return a.mcpToolSetPaneInfoAs(p.Arguments, caller)
	case "set_pane_flag":
		return a.mcpToolSetPaneFlag(p.Arguments, callerIdent, scope)
	case "list_schedules":
		return a.mcpToolListSchedules(p.Arguments)

	case "create_schedule":
		// Caller-pane attribution identifies the terminal that made this request: the pane
		// is the CREATOR, stamped as provenance, never the prompt target.
		return a.mcpToolCreateSchedule(p.Arguments, callerPane)
	case "set_schedule_enabled":
		return a.mcpToolSetScheduleEnabled(p.Arguments)
	case "cancel_schedule":
		return a.mcpToolCancelSchedule(p.Arguments)
	case "write_file_line":
		return a.mcpToolWriteFileLine(p.Arguments)
	default:
		if call, ok := extensionMCPTool(p.Name); ok {
			return call(a, mcpToolCall{Args: p.Arguments, Scope: scope, CallerPane: callerPane, Ident: callerIdent})
		}
		return toolErr("unknown tool: " + p.Name)
	}
}

func mcpCheckMutatorAllowed(tool string) error {
	if !mcpToolRequiresMutationPolicy(tool) {
		return nil
	}
	allowed, reason := mcpMutationPolicyAllows()
	if allowed {
		return nil
	}
	if reason == "" {
		reason = "MCP mutating tools are disabled"
	}
	return errors.New(reason + "; refused " + tool)
}

func mcpToolRequiresMutationPolicy(tool string) bool {
	switch tool {

	case "send_input", "ask_pane", "spawn_pane", "create_schedule", "set_schedule_enabled", "cancel_schedule", "write_file_line", "open_browser", "close_browser",
		// Every remaining mutator joined the gate in the Warden hardening
		// pass — before it, `deny` still left kill_pane (!) and the
		// flag/workspace/preview mutators callable, which made the policy a
		// false promise of read-only-ness. The removed todo tools left the set:
		// their stubs change nothing.
		"kill_pane", "rename_pane", "set_pane_info", "set_pane_flag", "switch_workspace", "preview_file", "answer_prompt":
		return true
	default:
		return false
	}
}

func mcpMutationPolicyAllows() (bool, string) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(mcpMutationPolicyEnv)))
	switch raw {
	case "", "allow", "allowed", "1", "true", "yes", "on":
		return true, ""
	case "deny", "denied", "readonly", "read-only", "0", "false", "no", "off":
		return false, "MCP mutating tools are disabled by " + mcpMutationPolicyEnv + "=" + raw
	default:
		return false, "invalid " + mcpMutationPolicyEnv + "=" + raw + " (use allow or deny)"
	}
}

// redactToolHint produces a short, display-safe hint for a tool call.
// Strips full paths to basenames, truncates to 80 chars. Unknown tools
// get an empty hint rather than raw argument dump — defaults to safe.
func redactToolHint(tool string, args map[string]any) string {
	pick := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	basename := func(s string) string {
		if s == "" {
			return ""
		}
		// Last path component; OS-agnostic (works for both / and \\).
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == '/' || s[i] == '\\' {
				return s[i+1:]
			}
		}
		return s
	}
	var hint string
	switch tool {
	case "preview_file":
		hint = basename(pick("path"))
	case "send_input", "read_pane_tail", "get_pane_state", "kill_pane",
		"rename_pane", "set_pane_info", "set_pane_flag", "get_pane_livecwd", "ask_pane", "wait_for_pane":
		// Pane-targeting tools: the target id (the publish site upgrades
		// it to the pane's title when it can). Never the input text —
		// send_input bodies are prompts and may carry anything.
		hint = pick("pane_id")
	case "answer_prompt":
		hint = pick("option")
		if pid := pick("pane_id"); pid != "" {
			if hint != "" {
				hint += " → "
			}
			hint += pid
		}
	case "spawn_pane":
		hint = pick("cmd")
		if d := basename(pick("cwd")); d != "" {
			if hint != "" {
				hint += " · "
			}
			hint += d
		}
	case "switch_workspace":
		hint = pick("workspace")
	default:
		// For any future tool with a common "path" arg, surface just
		// the basename. Anything else → empty.
		if p := pick("path"); p != "" {
			hint = basename(p)
		}
	}
	if len(hint) > 80 {
		hint = hint[:80] + "…"
	}
	return hint
}

func (a *apiServer) mcpToolPreviewFile(args map[string]any) any {
	pathRaw, _ := args["path"].(string)
	if pathRaw == "" {
		return toolErr("path is required")
	}
	abs, err := canonicalizeFilePath(pathRaw)
	if err != nil {
		return toolErr(err.Error())
	}
	if credentialPathRefusal(abs, a.dataDir) != "" || secretPathRefusal(abs) != "" {
		return toolErr("refusing to access credential material")
	}
	// Same open-then-fstat check the tile's read will make, so the agent
	// gets the refusal instead of the person getting an error tile.
	f, info, err := openUserFile(abs, -1)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return toolErr("not found: " + err.Error())
		}
		if errors.Is(err, errUserFileIsDir) {
			return toolErr("path is a directory — preview_file expects a text file. To list a directory, use shell tools.")
		}
		if errors.Is(err, errUserFileNotRegular) {
			return toolErr(err.Error())
		}
		return toolErr("cannot open: " + err.Error())
	}
	_ = f.Close()
	if info.Size() > maxFileBytes {
		return toolErr(fmt.Sprintf("file too large (%d bytes > %d cap for preview)", info.Size(), maxFileBytes))
	}
	a.uiBus.publish(uiEvent{Type: "open-file", Path: abs})
	return toolOK(fmt.Sprintf("Opened %s in a soluterminal preview tile. The user can now see it in the UI.", abs))
}

// mcpPaneSummary is the per-pane shape list_panes returns over MCP.
type mcpPaneSummary struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Cwd            string `json:"cwd"`
	ExitCode       *int   `json:"exitCode,omitempty"`
	LastActivityMs int64  `json:"lastActivityMs"`
	// Agent is the live agent identity — "claude" | "codex" | "muse", or
	// "" for a plain shell/REPL. Kind is the renderer: "terminal" | "native"
	// (native = Focus). Orchestrators need both to pick a pane to drive.
	Agent string `json:"agent,omitempty"`
	Kind  string `json:"kind"`
	// Flagged/FlagMemo mirror the sidebar's "come back to this" marker.
	Flagged   bool   `json:"flagged,omitempty"`
	FlagMemo  string `json:"flagMemo,omitempty"`
	FlagOwner string `json:"flagOwner,omitempty"`
	// Number is the #n the UI shows on the pane in Workspace; Label is
	// "#n Name" for repeating back to the user. State is the live runtime
	// state (busy, idle, awaiting-input, choice-prompt, …). You marks the
	// calling pane, so an agent never asks itself.
	Number    int    `json:"number,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Label     string `json:"label,omitempty"`
	State     string `json:"state,omitempty"`
	You       bool   `json:"you,omitempty"`
	// StartedBy says who opened the pane and why, e.g. "#5 Claude — Engine deletion"
	// (pane_origin.go); empty for panes the user opened or that predate it.
	StartedBy string `json:"startedBy,omitempty"`
	// Handle (@api-fix) addresses the pane in any pane_id; WorkingOn is its
	// current one-line status (pane_identity.go).
	Handle    string `json:"handle,omitempty"`
	WorkingOn string `json:"workingOn,omitempty"`
}

// paneSummaryAgent resolves a pane's live agent identity for list_panes:
// the persisted agent field when set, else the spawn argv, else one
// process-tree walk for a claude/codex/muse descendant ("" = plain shell).
func paneSummaryAgent(p *Pane, snap PaneSnapshot) string {
	if strings.TrimSpace(snap.Agent) != "" {
		return effectiveAgentKind(snap.Agent)
	}
	if snap.Kind == "native" {
		// Native panes with no explicit agent are the legacy Claude default.
		return agentClaude
	}
	if cmdStartsCodex(snap.Cmd) {
		return agentCodex
	}
	if cmdStartsMuse(snap.Cmd) {
		return agentMuse
	}
	if len(snap.Cmd) > 0 {
		name := strings.ToLower(filepath.Base(strings.ReplaceAll(snap.Cmd[0], `\`, "/")))
		if name == "claude" || name == "claude.exe" {
			return agentClaude
		}
	}
	if p.Pid > 0 {
		for _, pid := range descendantPids(p.Pid) {
			if pid == p.Pid {
				continue
			}
			switch readComm(pid) {
			case "claude":
				return agentClaude
			case "codex":
				return agentCodex
			case "muse":
				return agentMuse
			}
		}
	}
	return ""
}

func (a *apiServer) mcpToolListPanes(_ map[string]any, callerPane string) any {
	panes := a.mgr.List()
	slots := paneSlotsByID(a.ws)
	out := make([]mcpPaneSummary, 0, len(panes))
	for _, p := range panes {
		snap := p.Snapshot()
		kind := "terminal"
		if snap.Kind == "native" {
			kind = "native"
		}
		agent := paneSummaryAgent(p, snap)
		slot := slots[snap.ID]
		out = append(out, mcpPaneSummary{
			ID:             snap.ID,
			Title:          snap.Title,
			Status:         string(snap.Status),
			Cwd:            snap.Cwd,
			ExitCode:       snap.ExitCode,
			LastActivityMs: snap.LastActivityMs,
			Agent:          agent,
			Kind:           kind,
			Flagged:        snap.Flagged,
			FlagMemo:       snap.FlagMemo,
			FlagOwner:      snap.FlagOwner,
			Number:         slot.Number,
			Workspace:      slot.Workspace,
			Label:          paneLabel(slot, snap.Title, agent),
			StartedBy:      paneStartedBy(snap.Origin),
			State:          string(p.computeStateCached()),
			Handle:         identityHandle(snap.Identity),
			WorkingOn:      identityWorkingOn(snap.Identity),
			You:            callerPane != "" && snap.ID == callerPane,
		})
	}
	// The UI's order: by workspace, then #n; panes outside any workspace last.
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := out[i].Workspace, out[j].Workspace
		if (wi == "") != (wj == "") {
			return wi != ""
		}
		if wi != wj {
			return wi < wj
		}
		return out[i].Number < out[j].Number
	})
	b, err := json.Marshal(out)
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

func (a *apiServer) mcpToolReadPaneTail(args map[string]any, callerPane string) any {
	ref, _ := args["pane_id"].(string)
	if ref == "" {
		return toolErr("pane_id is required")
	}
	paneID := a.resolvePaneRef(ref, callerPane)
	if paneID == "" {
		paneID = ref
	}
	mode, _ := args["mode"].(string)
	if raw, _ := args["raw"].(bool); raw {
		mode = "raw"
	}
	if mode != "raw" {
		p, ok := a.mgr.Get(paneID)
		if !ok {
			return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
		}
		if mode == "screen" {
			return toolOK(paneScreenText(p))
		}
		reply := a.paneLatestReply(p)
		if mode == "" && reply.Source != "transcript" {
			return toolOK(reply.Reply)
		}
		return toolOK(formatPaneReply(a.collabPaneLabel(p), reply, p.computeStateCached()))
	}
	lines := 80
	if v, ok := args["lines"].(float64); ok && v > 0 {
		lines = int(v)
	}
	if lines > 500 {
		lines = 500
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	// Terminals routinely display credentials (`gh auth token`, printed env,
	// connection strings); scrub the shapes we can key on before the tail
	// leaves the daemon and lands in a model context or a report.
	return toolOK(redactTerminalTail(tailOfPane(p, lines)))
}

func (a *apiServer) mcpToolGetPaneState(args map[string]any) any {
	paneID, _ := args["pane_id"].(string)
	if paneID == "" {
		return toolErr("pane_id is required")
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	// Cached read, NOT the direct computeState(): the direct path advances
	// the fire-once ack positions (awaiting-input falls through to idle on
	// the next compute), so an MCP poll could consume a state edge before
	// the attention engine's own 2s sample saw it — suppressing a "waiting
	// for you" signal. The cache shares one compute with the watchers
	// (≤1s staleness), so polling here can no longer eat their edges.
	return toolOK(string(p.computeStateCached()))
}

func (a *apiServer) mcpToolSendInput(args map[string]any, callers ...string) any {
	paneID, _ := args["pane_id"].(string)
	text, _ := args["text"].(string)
	if paneID == "" {
		return toolErr("pane_id is required")
	}
	if text == "" {
		return toolErr(`text is empty — use "\n" to send a bare Enter`)
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	// Reject writes to a pane whose process has already exited — the
	// PTY is gone and the keystrokes would silently vanish.
	if p.Snapshot().Status == PaneExited {
		return toolErr("pane " + paneID + " has exited — cannot send input to it")
	}
	// Normalize literal escape sequences (the model often emits "\\n"
	// instead of a real newline) the same way the ARIA path does.
	text = unescapeAriaInput(text)
	if reason := sendInputMenuCollision(p, text); reason != "" {
		return toolErr(reason)
	}
	// A message to a Claude or Codex pane (text ending in Enter, multi-line
	// included) goes through the verified delivery: paste, wait until the
	// input box shows it, Enter, confirm it left the box (pane_delivery.go).
	if body, ok := agentMessageBody(text); ok {
		if agent, claude := paneAgentProvider(p); agent {
			sender := ""
			if len(callers) > 0 {
				body = a.labelPaneMessage(body, callers[0], paneID)
				sender = a.paneMessageSender(callers[0], paneID)
			}
			wrote, outcome, err := submitAgentMessage(p, body, claude, sender)
			if err != nil {
				return toolErr(fmt.Sprintf("wrote %d bytes to pane %s but %v", wrote, paneID, err))
			}
			if len(callers) > 0 {
				p.recordAgentMessage(callers[0], sender, outcome, time.Now())
			}
			if outcome == deliveryLeftInComposer {
				return toolErr(fmt.Sprintf("Sent %d bytes to pane %s: %s.", wrote, paneID, deliveryNotice(outcome)))
			}
			return toolOK(fmt.Sprintf("Sent %d bytes to pane %s; %s.", wrote, paneID, deliveryNotice(outcome)))
		}
	}
	// Split body + trailing-Enter so a submitted line is written as a
	// separate CR frame after the standard gap — see pty_submit.go.
	body, submit := splitTrailingEnter(text)
	hasClaude := p.HasClaudeDescendant()
	wrote := 0
	if body != "" {
		n, err := p.Write([]byte(body))
		if err != nil {
			return toolErr("write to pane " + paneID + " failed: " + err.Error())
		}
		wrote += n
	}
	if submit {
		if body != "" {
			time.Sleep(EnterGap(hasClaude))
		}
		n, err := p.Write([]byte("\r"))
		if err != nil {
			return toolErr(fmt.Sprintf("wrote %d bytes but submit-Enter failed: %v", wrote, err))
		}
		wrote += n
	}
	return toolOK(fmt.Sprintf("Sent %d bytes to pane %s.", wrote, paneID))
}

func (a *apiServer) mcpToolSpawnPane(args map[string]any, callerPane string, automated ...bool) any {
	cwd, _ := args["cwd"].(string)
	cmdStr, _ := args["cmd"].(string)
	workspace, _ := args["workspace"].(string)
	role, _ := args["role"].(string)
	var cmd []string
	if strings.TrimSpace(cmdStr) != "" {
		cmd = []string{strings.TrimSpace(cmdStr)}
	}
	modelArg, _ := args["model"].(string)
	effortArg, _ := args["effort"].(string)
	cmd, err := spawnAgentModelArgv(cmd, modelArg, effortArg)
	if err != nil {
		return toolErr(err.Error())
	}
	role = strings.TrimSpace(role)
	if !validPaneRole(role) {
		return toolErr("unknown role: " + role + " (only \"orchestrator\" is supported)")
	}
	if role != "" && !a.operatorFeaturesOn() {
		return toolErr("the orchestrator role is not available in this edition of soluterminal")
	}
	agentForRole := ""
	if role != "" {
		agentForRole = agentFromCmd(cmd)
		if agentForRole == "" {
			return toolErr("orchestrator role requires cmd \"claude\" or \"codex\"")
		}
		if !orchestratorHostSupported(agentForRole) {
			return toolErr("orchestrator role is not available for " + orchestratorHostLabel(agentForRole) + ": " + orchestratorHostUnsupportedReason(agentForRole))
		}
		if avail := a.orchestratorHostAvailability(agentForRole); !avail.Available {
			return toolErr("orchestrator role unavailable for " + orchestratorHostLabel(agentForRole) + ": " + avail.Reason)
		}
	}
	var workspaceErr error
	workspace, workspaceErr = resolveMCPSpawnWorkspace(a.ws, workspace, callerPane)
	if workspaceErr != nil {
		return toolErr(workspaceErr.Error())
	}
	agent := ""
	if agentForRole == agentCodex {
		agent = agentCodex
	}
	origin := ""
	if len(automated) > 0 && automated[0] {
		origin = "automated_spawn"
	}
	// Record who started the pane and why, and give it a real name instead of
	// "codex"/"bash" (pane_origin.go). Informational only.
	var creator *Pane
	if c, ok := a.mgr.Get(strings.TrimSpace(callerPane)); ok {
		creator = c
	}
	titleArg, _ := args["title"].(string)
	purposeArg, _ := args["purpose"].(string)
	var identity *PaneIdentity
	if raw, _ := args["handle"].(string); strings.TrimSpace(raw) != "" {
		h, err := normalizePaneHandle(raw)
		if err != nil {
			return toolErr(err.Error())
		}
		// Held until the pane exists, so a concurrent spawn, restore or edit
		// can't claim it meanwhile.
		release, err := a.reservePaneHandle(h)
		if err != nil {
			return toolErr(err.Error())
		}
		defer release()
		identity = &PaneIdentity{Handle: h}
	}
	promptArg, _ := args["prompt"].(string)
	paneOrigin := agentSpawnOrigin(creator, spawnPurpose(purposeArg, promptArg), workspace, time.Now())
	// Name the starter as agents see it in list_panes ("#5 Claude Code"), so
	// the user can find it on screen.
	if creator != nil {
		snap := creator.Snapshot()
		paneOrigin.ByPaneTitle = paneLabel(paneSlotsByID(a.ws)[snap.ID], snap.Title, paneSummaryAgent(creator, snap))
		if paneOrigin.LeadPaneID == paneOrigin.ByPaneID {
			paneOrigin.LeadPaneTitle = paneOrigin.ByPaneTitle
		}
	}
	spawnTitle := spawnPaneTitle(titleArg, agentFromCmd(cmd), paneOrigin)
	p, err := a.spawnPaneForRequest(SpawnOpts{Cwd: cwd, Cmd: cmd, Agent: agent, Role: role, AutomationOrigin: origin, Origin: paneOrigin, Title: spawnTitle, Identity: identity})
	if err != nil {
		return toolErr("spawn failed: " + err.Error())
	}
	// Persist for reattach on daemon restart — mirrors createPane.
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			a.rollbackSpawnedPane(p)
			return toolErr("persist pane failed: " + err.Error())
		}
	}
	snap := p.Snapshot()
	if _, err := a.ws.AddPane(workspace, PaneDef{
		ID:              snap.ID,
		Cwd:             snap.Cwd,
		Cmd:             snap.Cmd,
		Title:           snap.Title,
		Cols:            snap.Cols,
		Rows:            snap.Rows,
		Flagged:         snap.Flagged,
		FlagMemo:        snap.FlagMemo,
		ClaudeSessionID: snap.ClaudeSessionID,
		Kind:            snap.Kind,
		Agent:           snap.Agent,
		CodexThreadID:   snap.CodexThreadID,
		MuseSessionID:   snap.MuseSessionID,
		Provider:        snap.Provider,
		Effort:          snap.Effort,
		Role:            snap.Role,
		ContextPack:     snap.ContextPack,
		TitleOverride:   snap.TitleOverride,
		Origin:          snap.Origin.clone(),
		Identity:        snap.Identity.clone(),
	}, "", ""); err != nil {
		a.rollbackSpawnedPane(p)
		return toolErr("workspace attach failed: " + err.Error())
	}
	resp := map[string]any{"id": p.ID, "workspace": workspace, "title": spawnTitle}
	number := 0
	if slot, ok := paneSlotsByID(a.ws)[p.ID]; ok {
		resp["number"] = slot.Number
		number = slot.Number
	}
	a.announcePaneSpawn(p, paneOrigin, spawnTitle, workspace, number)
	for k, v := range a.spawnFollowUp(p, cmd, args) {
		resp[k] = v
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

// resolveMCPSpawnWorkspace guarantees that an MCP-spawned pane has a visible
// destination before the process is created. Explicit input wins, followed by
// caller attribution and finally the daemon's most recently active named
// workspace. The name tie-break keeps fallback deterministic despite the
// workspace store's map-backed iteration order.
func resolveMCPSpawnWorkspace(ws *WorkspaceStore, requested, callerPane string) (string, error) {
	if ws == nil {
		return "", errors.New("workspace store not initialized")
	}

	requested = strings.TrimSpace(requested)
	if requested != "" {
		resolved, ok := resolveWorkspaceName(ws, requested)
		if !ok {
			if strings.EqualFold(requested, "_current") {
				return "", errors.New("workspace _current is hidden — choose a visible named workspace")
			}
			return "", fmt.Errorf("no workspace named %s (case-insensitive) — call list_workspaces for valid names", requested)
		}
		if resolved == "_current" {
			return "", errors.New("workspace _current is hidden — choose a visible named workspace")
		}
		return resolved, nil
	}

	if callerPane = strings.TrimSpace(callerPane); callerPane != "" {
		if callerWorkspace := ws.NameForPane(callerPane); callerWorkspace != "" {
			return callerWorkspace, nil
		}
	}

	var newest *Workspace
	for _, candidate := range ws.List() {
		if candidate == nil || candidate.Name == "" || candidate.Name == "_current" {
			continue
		}
		if newest == nil || candidate.ActiveAt.After(newest.ActiveAt) ||
			(candidate.ActiveAt.Equal(newest.ActiveAt) && candidate.Name < newest.Name) {
			newest = candidate
		}
	}
	if newest == nil {
		return "", errors.New("no visible named workspace available")
	}
	return newest.Name, nil
}

func (a *apiServer) mcpToolKillPane(args map[string]any, callerPane string) any {
	return a.mcpToolKillPaneAs(args, paneCaller{ID: callerPane})
}

// mcpToolKillPaneAs is mcpToolKillPane for a caller that may only claim a pane
// (paneCaller.Claimed): it owns nothing, and the prompt says so.
func (a *apiServer) mcpToolKillPaneAs(args map[string]any, c paneCaller) any {
	callerPane := c.ID
	paneID, _ := args["pane_id"].(string)
	if paneID == "" {
		return toolErr("pane_id is required")
	}
	target, ok := a.mgr.Get(paneID)
	if !ok {
		return a.killPaneNow(paneID, callerPane, "")
	}
	asked, _ := args["user_requested"].(bool)
	switch a.paneAgentAuthority(target.Snapshot().Origin, paneID, callerPane, asked) {
	case paneAgentRefused:
		return toolErr(a.paneConfirmTarget(target) + " was not started by you, so you can't close it. If the user asked you to close it, call kill_pane again with user_requested: true and they will be asked to allow it in the SoluTerminal app. Otherwise leave it: they can close it themselves.")
	case paneAgentNeedsOK:
		name := a.paneConfirmTarget(target)
		caller := a.paneConfirmCaller(c)
		req, err := a.requestPaneConfirmFor(paneConfirmClose+"\x00"+c.bucket()+"\x00"+paneID, c.bucket(), paneConfirmView{
			Action:       paneConfirmClose,
			CallerPaneID: strings.TrimSpace(callerPane),
			Caller:       caller,
			PaneID:       paneID,
			Pane:         name,
			Title:        "Close " + name + "?",
			Message:      caller + " wants to close " + name + ". Closing stops what is running in it; files are not deleted.",
		}, func() map[string]any { return a.killPaneNow(paneID, callerPane, " (you allowed it in the app)") })
		if err != nil {
			return toolErr(err.Error())
		}
		return awaitPaneConfirm(req, paneConfirmWaitingText(name+" is not closed yet"))
	}
	return a.killPaneNow(paneID, callerPane, "")
}

// killPaneNow closes the pane for an agent once kill_pane is allowed.
func (a *apiServer) killPaneNow(paneID, callerPane, note string) map[string]any {
	var origin *PaneOrigin
	if target, ok := a.mgr.Get(paneID); ok {
		origin = target.Snapshot().Origin
	}
	if err := a.mgr.Kill(paneID); err != nil {
		return toolErr(err.Error())
	}
	closer := strings.TrimSpace(callerPane)
	if closer == "" {
		closer = "agent"
	}
	recordPaneClose(paneID, origin, closer, paneStartedByCaller(origin, callerPane))
	if a.panes != nil {
		_ = a.panes.Remove(paneID)
	}
	if a.ws != nil {
		_ = a.ws.RemovePane(paneID)
	}
	return toolOK("Killed pane " + paneID + note + ".")
}

// resolveWorkspaceName matches `name` against stored workspace names
// case-insensitively, returning the canonical stored name. An exact
// match wins over a case-folded one. Used so the model can pass
// workspace:"aria" for a workspace named "Aria".
func resolveWorkspaceName(ws *WorkspaceStore, name string) (string, bool) {
	if ws == nil || name == "" {
		return "", false
	}
	lower := strings.ToLower(name)
	var fold string
	for _, w := range ws.List() {
		if w.Name == name {
			return w.Name, true
		}
		if fold == "" && strings.ToLower(w.Name) == lower {
			fold = w.Name
		}
	}
	if fold != "" {
		return fold, true
	}
	return "", false
}

func (a *apiServer) mcpToolListWorkspaces(_ map[string]any) any {
	if a.ws == nil {
		return toolErr("workspace store not initialized")
	}
	type wsEntry struct {
		Name      string `json:"name"`
		PaneCount int    `json:"paneCount"`
		Protected bool   `json:"protected,omitempty"`
		ActiveAt  int64  `json:"activeAtMs"`
	}
	items := a.ws.List()
	out := make([]wsEntry, 0, len(items))
	for _, w := range items {
		// _current is the browser's autosave scratchpad — not a
		// user-visible workspace. Hide it, mirroring toolListWorkspaces.
		if w.Name == "_current" {
			continue
		}
		out = append(out, wsEntry{
			Name:      w.Name,
			PaneCount: len(w.PaneIds),
			Protected: w.Protected,
			ActiveAt:  w.ActiveAt.UnixMilli(),
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

func (a *apiServer) mcpToolSwitchWorkspace(args map[string]any) any {
	name, _ := args["workspace"].(string)
	if strings.TrimSpace(name) == "" {
		return toolErr("workspace is required")
	}
	if a.ws == nil {
		return toolErr("workspace store not initialized")
	}
	resolved, ok := resolveWorkspaceName(a.ws, name)
	if !ok {
		return toolErr("no workspace named " + name + " (case-insensitive) — call list_workspaces for valid names")
	}
	// Mirrors toolSwitchWorkspace: the browser subscribes to this event
	// and calls loadWorkspace.
	a.uiBus.publish(uiEvent{
		Type: "aria-switch-workspace",
		Text: resolved,
		Ts:   time.Now().UnixMilli(),
	})
	return toolOK("Requested connected soluterminal clients to switch to workspace '" + resolved + "'.")
}

func (a *apiServer) mcpToolListPorts(args map[string]any) any {
	if a.aria == nil {
		return toolErr("port discovery unavailable (aria not enabled)")
	}
	paneID, _ := args["pane_id"].(string)
	// Reuse the ARIA implementation verbatim — it already returns a JSON
	// string and enriches each port with pane title + workspace.
	return toolOK(a.aria.toolListPorts(paneID, false))
}

// mcpToolOpenBrowser is the "show the user" half of list_ports: discovery
// names the dev server, this puts it on their screen. Reuses the ARIA
// implementation, which validates http(s) and publishes the open-browser
// UI event every connected client acts on (store.openBrowserHere — the
// single browser tile navigates or opens). Mutation-policy-gated: it runs
// nothing and writes nothing, but an agent steering what's on the user's
// screen is still a capability read-only deployments should be able to
// switch off.
func (a *apiServer) mcpToolOpenBrowser(args map[string]any) any {
	if a.aria == nil {
		return toolErr("browser control unavailable (aria not enabled)")
	}
	rawURL, _ := args["url"].(string)
	res := a.aria.toolOpenBrowser(rawURL)
	if strings.HasPrefix(res, "error:") {
		return toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
	}
	return toolOK(res)
}

// mcpToolCloseBrowser — open_browser's inverse; same policy gate, same
// rationale (screen steering), same event bus.
func (a *apiServer) mcpToolCloseBrowser(_ map[string]any) any {
	if a.aria == nil {
		return toolErr("browser control unavailable (aria not enabled)")
	}
	res := a.aria.toolCloseBrowser()
	if strings.HasPrefix(res, "error:") {
		return toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
	}
	return toolOK(res)
}

func (a *apiServer) mcpToolReadActivity(args map[string]any) any {
	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if limit > 200 {
		limit = 200
	}
	since := int64(0)
	if v, ok := args["since"].(float64); ok && v > 0 {
		since = int64(v)
	}
	events, cursor := a.uiBus.RecentSince(since)
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	b, err := json.Marshal(map[string]any{
		"events": events,
		"cursor": cursor,
	})
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

func (a *apiServer) mcpToolReadFile(args map[string]any) any {
	if a.aria == nil {
		return toolErr("read_file unavailable (aria not enabled)")
	}
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return toolErr("path is required")
	}
	startLine := 0
	if v, ok := args["start_line"].(float64); ok {
		startLine = int(v)
	}
	maxLines := 0
	if v, ok := args["max_lines"].(float64); ok {
		maxLines = int(v)
	}
	// Reuse the ARIA implementation — it handles env-file refusal,
	// binary detection, size caps, and 1-indexed bounded slicing.
	result := a.aria.toolReadFile(path, startLine, maxLines)
	if strings.HasPrefix(result, "error:") {
		return toolErr(strings.TrimSpace(strings.TrimPrefix(result, "error:")))
	}
	abs, err := canonicalizeFilePath(path)
	if err != nil {
		return toolOK(result)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return toolOK(result)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		return toolOK(result)
	}
	obj["mtime"] = info.ModTime().UnixMilli()
	b, err := json.Marshal(obj)
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

func (a *apiServer) mcpToolGetPaneLiveCwd(args map[string]any) any {
	paneID, _ := args["pane_id"].(string)
	if strings.TrimSpace(paneID) == "" {
		return toolErr("pane_id is required")
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	cwd := liveCwdOfPane(p)
	fellBack := false
	if cwd == "" {
		cwd = p.Cwd // fall back to spawn-time cwd — mirrors paneLiveCwd
		fellBack = true
	}
	b, err := json.Marshal(map[string]any{
		"pane_id":      paneID,
		"cwd":          cwd,
		"spawn_cwd":    p.Cwd,
		"is_spawn_cwd": fellBack,
	})
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

func (a *apiServer) mcpToolGitStatus(args map[string]any) any {
	if a.aria == nil {
		return toolErr("git_status unavailable (aria not enabled)")
	}
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		// No path given — default to the user's home. Rarely a repo,
		// but toolGitStatus returns a clean "not a git repo" error the
		// model can act on.
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	// Reuse the ARIA implementation: read-only git only, with diff
	// stats and the default recent-commit count.
	res := a.aria.toolGitStatus(path, true, 0)
	if strings.HasPrefix(res, "error:") {
		return toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
	}
	for _, note := range gitStatusNoteHooks {
		res += note(a, path)
	}
	return toolOK(res)
}

func (a *apiServer) mcpToolSearchFiles(args map[string]any) any {
	if a.aria == nil {
		return toolErr("search_files unavailable (aria not enabled)")
	}
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return toolErr("query is required")
	}
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	// Reuse the ARIA implementation: skips vendor/generated dirs,
	// returns paths only, default kind=files and default hit cap.
	res := a.aria.toolSearchFiles(path, query, "files", 0)
	if strings.HasPrefix(res, "error:") {
		return toolErr(strings.TrimSpace(strings.TrimPrefix(res, "error:")))
	}
	return toolOK(res)
}

// mcpToolRenamePane changes a pane's title. Like kill_pane and set_pane_info,
// an agent renames its own pane and the panes it started (or leads); anyone
// else's, including a pane without an origin record, only with the person's
// OK in the app. That also keeps an agent from retitling someone else's pane
// to mislead the person in a later confirmation prompt.
func (a *apiServer) mcpToolRenamePane(args map[string]any, callerPane string) any {
	return a.mcpToolRenamePaneAs(args, paneCaller{ID: callerPane})
}

// mcpToolRenamePaneAs is mcpToolRenamePane for a caller that may only claim a pane
// (paneCaller.Claimed): it owns nothing, and the prompt says so.
func (a *apiServer) mcpToolRenamePaneAs(args map[string]any, c paneCaller) any {
	callerPane := c.ID
	paneID, _ := args["pane_id"].(string)
	// One line, bounded: what the person approves is exactly what is set.
	rawTitle, _ := args["title"].(string)
	title := clipRunes(oneLine(printableText(rawTitle)), paneTitleMaxRunes)
	if strings.TrimSpace(paneID) == "" {
		return toolErr("pane_id is required")
	}
	if strings.TrimSpace(title) == "" {
		return toolErr("title is required")
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	asked, _ := args["user_requested"].(bool)
	switch a.paneAgentAuthority(p.Snapshot().Origin, p.ID, callerPane, asked) {
	case paneAgentRefused:
		return toolErr(a.paneConfirmTarget(p) + " was not started by you, so you can't rename it. If the user asked you to rename it, call rename_pane again with user_requested: true and they will be asked to allow it in the SoluTerminal app. Otherwise leave it: they can rename it themselves.")
	case paneAgentNeedsOK:
		name := a.paneConfirmTarget(p)
		caller := a.paneConfirmCaller(c)
		req, err := a.requestPaneConfirmFor(paneConfirmRename+"\x00"+c.bucket()+"\x00"+p.ID+"\x00"+title, c.bucket(), paneConfirmView{
			Action:       paneConfirmRename,
			CallerPaneID: strings.TrimSpace(callerPane),
			Caller:       caller,
			PaneID:       p.ID,
			Pane:         name,
			Title:        "Rename " + name + "?",
			Message:      caller + " wants to rename " + name + " to “" + title + "”.",
		}, func() map[string]any {
			target, ok := a.mgr.Get(p.ID)
			if !ok {
				return toolErr(name + " has closed; nothing was renamed.")
			}
			return a.renamePaneNow(target, title)
		})
		if err != nil {
			return toolErr(err.Error())
		}
		return awaitPaneConfirm(req, paneConfirmWaitingText(name+" is not renamed yet"))
	}
	return a.renamePaneNow(p, title)
}

// printableText drops control and bidi-override characters, which could make
// a title read differently in the prompt than it is.
func printableText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F:
			return -1
		}
		return r
	}, s)
}

// paneTitleMaxRunes bounds a title an agent sets with rename_pane.
const paneTitleMaxRunes = 120

// renamePaneNow sets the title once rename_pane is allowed.
func (a *apiServer) renamePaneNow(p *Pane, title string) map[string]any {
	oldTitle := p.Snapshot().Title
	p.Rename(title)
	// Persist for reattach on restart — mirrors the ARIA rename path.
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.Rename(oldTitle)
			return toolErr("persist pane failed: " + err.Error())
		}
	}
	return toolOK("Renamed pane " + p.ID + " from '" + oldTitle + "' to '" + title + "'.")
}

func (a *apiServer) mcpToolSetPaneFlag(args map[string]any, ident string, scope *mcpScope) any {
	paneID, _ := args["pane_id"].(string)
	if strings.TrimSpace(paneID) == "" {
		return toolErr("pane_id is required")
	}
	flagged, ok := args["flagged"].(bool)
	if !ok {
		return toolErr("flagged is required (boolean)")
	}
	memo, _ := args["memo"].(string)
	p, found := a.mgr.Get(paneID)
	if !found {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)")
	}
	episode := ""
	if scope != nil && scope.Identity == wardenScopeIdentity {
		// Read the current option at call time: a token minted before opt-out
		// cannot keep creating flags. Only a live triage episode can write.
		if !flagged || a.wardenSettings == nil || !a.wardenSettings.Snapshot().AutoFlags {
			return toolErr("Warden automatic flags are disabled")
		}
		if scope.WardenPaneID != paneID || scope.WardenEpisode == "" {
			return toolErr("Warden flag capability is not for this pane and episode")
		}
		episode = scope.WardenEpisode
	}
	err := a.mgr.changeCurrentFlag(p, a.panes, func(prev PaneSnapshot) error {
		if scope != nil && prev.Flagged && prev.FlagOwner != "" && prev.FlagOwner != ident {
			return fmt.Errorf("pane %s is already flagged by '%s' — a scoped caller may not override it", paneID, prev.FlagOwner)
		}
		if episode != "" {
			if a.wardenSettings == nil || !a.wardenSettings.Snapshot().AutoFlags {
				return fmt.Errorf("Warden automatic flags are disabled")
			}
			state, _, current := p.attentionEpisodeNow()
			if state != AttentionNeedsUser || current != episode {
				return fmt.Errorf("Warden triage episode is no longer active")
			}
			if prev.Flagged {
				return fmt.Errorf("pane %s already has a flag", paneID)
			}
		}
		return nil
	}, flagged, memo, ident, episode)
	if err != nil {
		return toolErr(err.Error())
	}
	if flagged {
		if strings.TrimSpace(memo) != "" {
			return toolOK("Flagged pane " + paneID + " ('" + p.Title + "') with memo: " + strings.TrimSpace(memo))
		}
		return toolOK("Flagged pane " + paneID + " ('" + p.Title + "').")
	}
	return toolOK("Cleared the flag on pane " + paneID + " ('" + p.Title + "').")
}

func (a *apiServer) mcpToolListSchedules(args map[string]any) any {
	if a.scheds == nil {
		return toolErr("schedule store not initialized")
	}
	limit, offset, err := mcpPageArgs(args, 50, 200)
	if err != nil {
		return toolErr(err.Error())
	}
	nameContains := strings.ToLower(mcpArgString(args, "nameContains", "name_contains"))
	enabledOnly, _ := mcpPageArgBool(args, "enabledOnly", "enabled_only")
	// Run history is opt-in: with it, an unfiltered list was ~35 KB (~9k
	// tokens) on a busy machine, almost all of it runs a caller rarely needs.
	includeRuns, _ := mcpPageArgBool(args, "includeRuns", "include_runs")
	// Reuse makeScheduleDTO so the MCP shape matches the HTTP API's —
	// the persisted Schedule fields plus the computed nextFire time.
	//
	// Hidden agent triggers are ALWAYS filtered out here, with no escape hatch
	// (the HTTP list has ?includeAgentRuns=1; this one deliberately does not).
	// A model that could see them could pass their ids to set_schedule_enabled
	// and quietly disarm — or re-arm — an agent the user paused. Schedules
	// IMPORTED into an agent (docs/DESIGN-agents-surface.md §8.3) are hidden on
	// the same terms and for the same reason.
	out := []scheduleDTO{}
	for _, s := range a.scheds.List() {
		if s.isAgentRun() || s.OwnedByAgentID != "" {
			continue
		}
		if nameContains != "" && !strings.Contains(strings.ToLower(s.Name), nameContains) {
			continue
		}
		if enabledOnly && !s.Enabled {
			continue
		}
		dto := a.makeScheduleDTO(s)
		if !includeRuns {
			dto.Runs = nil
		}
		out = append(out, dto)
	}
	end := mcpPageBounds(len(out), offset, limit, func(i int) int {
		encoded, _ := json.Marshal(out[i])
		return len(encoded) + 1
	}, 256)
	pageOut := []scheduleDTO{}
	if offset < len(out) {
		pageOut = out[offset:end]
	}
	b, err := json.Marshal(map[string]any{
		"schedules": pageOut,
		"page":      mcpPageInfoFor(len(out), offset, end),
	})
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

// callerPane is the pane the agent is talking in (X-Soluterminal-Pane), stamped
// as CREATOR provenance. It is never confused with args["pane_id"], which for
// kind=prompt names the pane the prompt is delivered into.
func (a *apiServer) mcpToolCreateSchedule(args map[string]any, callerPane string) any {
	if a.scheds == nil {
		return toolErr("schedule store not initialized")
	}
	name, _ := args["name"].(string)
	paneID, _ := args["pane_id"].(string)
	prompt, _ := args["prompt"].(string)
	cronExpr, _ := args["cron"].(string)
	// kind defaults to "prompt": pre-task callers passed pane_id + prompt with
	// no kind at all, and must keep working unchanged.
	kindArg, _ := args["kind"].(string)
	kindArg = strings.TrimSpace(kindArg)
	if kindArg == "" {
		kindArg = "prompt"
	}
	if strings.TrimSpace(prompt) == "" {
		return toolErr("prompt is required")
	}
	in := ScheduleInput{
		Name:   name,
		Prompt: prompt,
		Cron:   cronExpr,
	}
	switch kindArg {
	case "task":
		in.Kind = "task"
		cwdArg, _ := args["cwd"].(string)
		in.Cwd = strings.TrimSpace(cwdArg)
		if in.Cwd == "" {
			return toolErr("cwd is required for kind=task")
		}
		permArg, _ := args["permission"].(string)
		in.Permission = strings.TrimSpace(permArg)
		if in.Permission == "" {
			in.Permission = "plan" // read-only unless the caller opts up
		}
		if v, ok := args["timeout_min"].(float64); ok {
			in.TimeoutMin = int(v)
		}
	case "prompt":
		if strings.TrimSpace(paneID) == "" {
			return toolErr("pane_id is required for kind=prompt")
		}
		in.PaneID = paneID
	default:
		return toolErr(`kind must be "task" or "prompt"`)
	}
	if v, ok := args["notify_on_run"].(bool); ok {
		in.NotifyOnRun = v
	}
	if v, ok := args["max_fires"].(float64); ok {
		in.MaxFires = int(v)
	}
	// Timestamp fields are RFC3339 strings — parse to *time.Time so
	// ScheduleStore.Create / validate see the same shape as the HTTP API.
	if v, ok := args["run_at"].(string); ok && strings.TrimSpace(v) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
		if err != nil {
			return toolErr("run_at must be an RFC3339 timestamp: " + err.Error())
		}
		in.RunAt = &t
	}
	if v, ok := args["until"].(string); ok && strings.TrimSpace(v) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
		if err != nil {
			return toolErr("until must be an RFC3339 timestamp: " + err.Error())
		}
		in.Until = &t
	}
	createdBy := &ScheduleOrigin{Source: "mcp", PaneID: strings.TrimSpace(callerPane)}
	if createdBy.PaneID != "" {
		createdBy.PaneTitle = a.mgr.DisplayTitle(createdBy.PaneID)
	}
	// ScheduleStore.CreateWithOrigin runs the same full validation Create does
	// (exactly-one trigger, future timestamps, valid cron, per-kind required
	// fields) and wires the cron; the origin is provenance only.
	sched, err := a.scheds.CreateWithOrigin(in, createdBy)
	if err != nil {
		return toolErr(err.Error())
	}
	trigger := "recurring (cron " + sched.Cron + ")"
	if sched.oneTime() {
		trigger = "one-time (run-at " + sched.RunAt.Format(time.RFC3339) + ")"
	}
	target := "targeting pane " + sched.PaneID
	if sched.isTask() {
		target = "a Claude task in " + sched.Cwd + " (" + sched.Permission + ")"
	}
	return toolOK("Created schedule " + sched.ID + " '" + sched.Name + "' — " + trigger + " — " + target + ".")
}

func (a *apiServer) mcpToolSetScheduleEnabled(args map[string]any) any {
	if a.scheds == nil {
		return toolErr("schedule store not initialized")
	}
	idArg, _ := args["id"].(string)
	id := strings.TrimSpace(idArg)
	if id == "" {
		return toolErr("id is required")
	}
	enabled, ok := args["enabled"].(bool)
	if !ok {
		return toolErr("enabled must be true or false")
	}
	if err := mcpRefuseAgentRunSchedule(a.scheds, id); err != nil {
		return toolErr(err.Error())
	}
	sched, err := a.scheds.SetEnabled(id, enabled)
	if err != nil {
		return toolErr(err.Error())
	}
	if enabled {
		return toolOK("Resumed schedule " + sched.ID + " '" + sched.Name + "'.")
	}
	return toolOK("Paused schedule " + sched.ID + " '" + sched.Name + "' — it will not fire until resumed.")
}

func (a *apiServer) mcpToolCancelSchedule(args map[string]any) any {
	if a.scheds == nil {
		return toolErr("schedule store not initialized")
	}
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return toolErr("id is required")
	}
	if err := mcpRefuseAgentRunSchedule(a.scheds, strings.TrimSpace(id)); err != nil {
		return toolErr(err.Error())
	}
	if err := a.scheds.Delete(id); err != nil {
		return toolErr(err.Error())
	}
	return toolOK("Deleted schedule " + id + " — it will no longer fire.")
}

// mcpRefuseAgentRunSchedule keeps the hidden triggers backing managed agents
// out of reach of the general schedule tools (docs/DESIGN-agents-surface.md
// §2.3): an agent must not be able to toggle or delete its own trigger, and no
// model-facing tool should be able to silently stop the user's agents. The ids
// are not even listed (see mcpToolListSchedules), so reaching one requires
// guessing it — this refuses the guess.
// It covers IMPORTED schedules too (§8.3): those are visible to the user as an
// agent card, so a model toggling one by id would silently change what that
// card describes.
func mcpRefuseAgentRunSchedule(store *ScheduleStore, id string) error {
	if store == nil {
		return nil
	}
	sched, ok := store.Get(id)
	if !ok {
		return nil
	}
	if sched.OwnedByAgentID != "" {
		return errors.New("that schedule is managed by agent " + sched.OwnedByAgentID +
			" and is controlled from its card, not this tool")
	}
	if !sched.isAgentRun() {
		return nil
	}
	return errors.New("that schedule belongs to an agent and is managed through the agents API, not this tool")
}

func (a *apiServer) mcpToolWriteFileLine(args map[string]any) any {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return toolErr("path is required")
	}
	newText, _ := args["new_text"].(string)
	lineFloat, ok := args["line_number"].(float64)
	if !ok {
		return toolErr("line_number is required (number)")
	}
	lineNumber := int(lineFloat)
	if lineNumber < 1 {
		return toolErr("line_number must be 1 or greater")
	}
	mtimeFloat, ok := args["expected_mtime"].(float64)
	if !ok {
		return toolErr("expected_mtime is required (number, unix millis from a prior read)")
	}
	expectedMtime := int64(mtimeFloat)
	if expectedMtime == 0 {
		return toolErr("expected_mtime must be nonzero for MCP file writes; call read_file first and pass its mtime")
	}
	abs, err := canonicalizeFilePath(path)
	if err != nil {
		return toolErr(err.Error())
	}
	if reason := mcpWriteFileLineRefusal(abs); reason != "" {
		return toolErr(reason)
	}
	// writeFileLineAt is 0-indexed; the tool's line_number is 1-indexed
	// to match read_file / preview_file output, so convert here. It is
	// the shared core behind the /api/files/write-line handler, so the
	// mtime guard, atomic write, and caps are identical to the UI path.
	abs, newMtime, _, err := writeFileLineAt(abs, lineNumber-1, newText, expectedMtime)
	if err != nil {
		return toolErr(err.Error())
	}
	b, mErr := json.Marshal(map[string]any{
		"path":        abs,
		"line_number": lineNumber,
		"mtime":       newMtime,
	})
	if mErr != nil {
		return toolErr("marshal: " + mErr.Error())
	}
	return toolOK("Replaced line " + fmt.Sprintf("%d", lineNumber) + " of " + abs + " on disk. " + string(b))
}

func toolOK(msg string) map[string]any {
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": msg},
		},
	}
}

func toolErr(msg string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{
			map[string]any{"type": "text", "text": msg},
		},
	}
}

// mcpArgString encodes the key-case skew in the server's own surfaces: agent
// verbs read taskId, workflow verbs read task_id/repo_path, and engine errors
// say repoPath. Incoming MCP arguments are never schema-validated despite
// additionalProperties:false, so a mis-cased key otherwise silently reads as
// "". Callers pass that verb's advertised key first (snake_case for workflow
// verbs, camelCase for agent verbs), followed by its alias; the first
// non-empty string wins when both spellings are present.
func mcpArgString(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}
