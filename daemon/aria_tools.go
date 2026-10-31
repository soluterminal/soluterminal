package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// ARIA tool surface. The autonomous evaluator gets exactly one
// mutating tool per turn (send_input for terminal panes, or
// resolve_permission for native panes — see aria_native_auto.go);
// it goes through the confirm dance in aria_confirm.go. The
// read-only helpers below (search_files, read_file, list_ports,
// git_status) have no LLM caller anymore but ARE the implementations
// behind the daemon's MCP server tools (mcp.go) — keep them in sync
// with the MCP schemas.

// ariaConfirmTimeout is how long send_input waits for the user. Three
// minutes is long enough the user can walk away briefly, short enough
// the cloud-side 10 min Anthropic ceiling doesn't kill the whole turn
// before we can return a "timed out" tool_result and let ARIA react.
const ariaConfirmTimeout = 3 * time.Minute

// ariaToolDef is the JSON shape Anthropic's Messages API expects for
// the "tools" field. We build a slice of these once at startup.
type ariaToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ariaAutonomousTools is the tool surface a terminal-pane autonomous
// turn is offered: just send_input. (Native turns get
// ariaNativeAutonomousTools instead — see aria_native_auto.go.)
func ariaAutonomousTools() []ariaToolDef {
	return []ariaToolDef{
		{
			Name:        "send_input",
			Description: "Send text to a pane's stdin. Every call pops a confirmation dialog — the user sees the exact bytes and clicks approve or cancel before anything is written. Include a trailing newline (either a real newline in the JSON string, or the two-character escape sequence backslash+n — both are accepted and normalized to one LF byte) when you want the pane to treat the input as a submitted line; most prompts won't advance without it. Common C-style escapes are recognized: \\n \\r \\t \\\\ \\\" \\xNN \\uNNNN. Examples: input=\"y\\n\" to reply yes + Enter, input=\"\\n\" to press Enter on a waiting prompt, input=\"\\x03\" to send Ctrl-C, input=\"ls -la\\n\" to run a short command. Don't use this to write code or multi-line scripts.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pane_id": map[string]any{
						"type":        "string",
						"description": "Target pane id.",
					},
					"input": map[string]any{
						"type":        "string",
						"description": "Text to send. Include a trailing \"\\n\" to submit. Control characters are allowed (e.g. \"\\u0003\" for Ctrl-C).",
					},
				},
				"required": []string{"pane_id", "input"},
			},
		},
	}
}

// ariaToolsWithCacheBreakpoint returns the tools slice with a
// cache_control: ephemeral marker on the last entry so Anthropic
// caches the system+tools prefix. Earlier tools serialize through
// their normal struct tags; only the trailing entry needs the field
// since Anthropic caches everything BEFORE the marker.
func ariaToolsWithCacheBreakpoint(tools []ariaToolDef) []any {
	out := make([]any, len(tools))
	for i, t := range tools {
		if i == len(tools)-1 {
			out[i] = map[string]any{
				"name":          t.Name,
				"description":   t.Description,
				"input_schema":  t.InputSchema,
				"cache_control": map[string]any{"type": "ephemeral"},
			}
		} else {
			out[i] = t
		}
	}
	return out
}

// executeTool runs a tool by name with parsed input. Returns a string
// result that gets passed back to the model as the tool_result. If
// the tool errors (bad args, unknown pane, etc.) we return the error
// as the result text rather than failing the whole turn — the model
// is better at recovering from a visible error message than from a
// missing response block.
//
// Every turn is autonomous now, so the surface is exactly the two
// action tools; the daemon-bound fields on req (pane id, prompt
// identity, request id) are enforced here, never trusted from the
// model.
func (a *Aria) executeTool(ctx context.Context, toolUseID, name string, input json.RawMessage, req ariaQueryReq, emit func(map[string]any)) string {
	switch name {
	case "send_input":
		var args struct {
			PaneID string `json:"pane_id"`
			Input  string `json:"input"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return fmt.Sprintf("error: invalid args: %v", err)
		}
		if strings.TrimSpace(args.PaneID) != strings.TrimSpace(req.AutonomousPaneID) {
			return fmt.Sprintf("error: autonomous send_input may only target pane %q", req.AutonomousPaneID)
		}
		return a.toolSendInput(ctx, toolUseID, args.PaneID, args.Input, req.AutonomousPromptPos, req.AutonomousPromptHash, req.AutonomousCautious, emit)
	case "resolve_permission":
		// Native-pane approval. The model calls this with NO meaningful args to
		// APPROVE the single pending request bound to this turn; it declines by
		// not calling the tool at all (responding with a sentence). The daemon
		// binds the pane + RequestID server-side (AutonomousPaneID /
		// AutonomousRequestID) so the model can never pick which parallel
		// approval to resolve — the wrong-tool safety hole.
		if !req.AutonomousNative {
			return "error: resolve_permission is only available on native-pane turns"
		}
		return a.toolResolvePermission(ctx, toolUseID, req.AutonomousPaneID, req.AutonomousRequestID, req.AutonomousCautious, emit)
	default:
		return fmt.Sprintf("error: autonomous ARIA turns may only call send_input or resolve_permission, not %q", name)
	}
}

type ariaPortSummary struct {
	Port      uint16 `json:"port"`
	Addr      string `json:"addr"`
	Proto     string `json:"proto"`
	Process   string `json:"process"`
	Cmdline   string `json:"cmdline,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	PaneTitle string `json:"pane_title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	URL       string `json:"url,omitempty"`
	FirstAtMs int64  `json:"first_at_ms"`
	Scheme    string `json:"scheme,omitempty"`
	WebStatus string `json:"web_status,omitempty"`
}

func (a *Aria) toolListPorts(paneID string, onlyPanes bool) string {
	if a.ports == nil {
		return "error: port registry unavailable"
	}
	paneID = strings.TrimSpace(paneID)
	ports := a.ports.List()
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		if ports[i].Addr != ports[j].Addr {
			return ports[i].Addr < ports[j].Addr
		}
		return ports[i].Proto < ports[j].Proto
	})
	out := make([]ariaPortSummary, 0, len(ports))
	for _, p := range ports {
		if p == nil {
			continue
		}
		if paneID != "" && p.PaneID != paneID {
			continue
		}
		if onlyPanes && p.PaneID == "" {
			continue
		}
		item := ariaPortSummary{
			Port:      p.Port,
			Addr:      p.Addr,
			Proto:     p.Proto,
			Process:   p.Process,
			Cmdline:   p.Cmdline,
			Cwd:       p.Cwd,
			PaneID:    p.PaneID,
			URL:       localhostURLForPort(p),
			FirstAtMs: p.FirstAt.UnixMilli(),
			Scheme:    p.Scheme,
			WebStatus: p.WebStatus,
		}
		if p.PaneID != "" {
			if a.mgr != nil {
				if pane, ok := a.mgr.Get(p.PaneID); ok {
					item.PaneTitle = pane.Title
				}
			}
			if a.ws != nil {
				item.Workspace = a.ws.NameForPane(p.PaneID)
			}
		}
		out = append(out, item)
	}
	b, _ := json.Marshal(map[string]any{"ports": out})
	return string(b)
}

// toolOpenBrowser publishes the open-browser UI event — every connected
// client opens (or retargets) its in-app browser tile at the URL. This is
// the "show the user" half of list_ports: discovery names the server, this
// puts it on their screen. No confirm dialog: it writes nothing, runs
// nothing, and the result is fully visible and closable in one click.
func (a *Aria) toolOpenBrowser(rawURL string) string {
	if a.uiBus == nil {
		return "error: ui bus unavailable"
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "error: url must be absolute http(s), e.g. http://localhost:5173"
	}
	a.uiBus.publish(uiEvent{Type: "open-browser", URL: parsed.String(), Ts: time.Now().UnixMilli()})
	b, _ := json.Marshal(map[string]any{"ok": true, "opened": parsed.String()})
	return string(b)
}

// toolCloseBrowser is open_browser's inverse: publish the close-browser UI
// event and every connected client dismisses its (single-instance) browser
// tile. Idempotent — closing when nothing is open is a quiet no-op on the
// client, so the tool always reports ok.
func (a *Aria) toolCloseBrowser() string {
	if a.uiBus == nil {
		return "error: ui bus unavailable"
	}
	a.uiBus.publish(uiEvent{Type: "close-browser", Ts: time.Now().UnixMilli()})
	return `{"ok":true,"closed":true}`
}

func localhostURLForPort(p *Port) string {
	if p == nil || p.Port == 0 {
		return ""
	}
	if p.WebStatus != "" && p.WebStatus != "ready" {
		return ""
	}
	if p.Scheme != "" {
		return p.Scheme + "://" + portHost(p)
	}
	if p.Addr == "::1" || (strings.Contains(p.Addr, ":") && p.Addr != "::") {
		scheme := p.Scheme
		if scheme == "" {
			scheme = "http"
		}
		return scheme + "://" + portHost(p)
	}
	addr := strings.TrimSpace(p.Addr)
	if addr == "" || addr == "0.0.0.0" || addr == "::" || addr == "127.0.0.1" || addr == "::1" {
		return fmt.Sprintf("http://localhost:%d", p.Port)
	}
	if strings.Contains(addr, ":") {
		return fmt.Sprintf("http://[%s]:%d", addr, p.Port)
	}
	return fmt.Sprintf("http://%s:%d", addr, p.Port)
}

func (a *Aria) toolGitStatus(root string, includeDiffStat bool, maxCommits int) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "error: root required"
	}
	abs, err := canonicalizeDirPath(root)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if reason := credentialPathRefusal(abs, a.dataDir); reason != "" {
		return "error: " + reason
	}
	if info, err := os.Stat(abs); err != nil {
		return fmt.Sprintf("error: root not found: %v", err)
	} else if !info.IsDir() {
		return "error: root is not a directory"
	}
	if maxCommits <= 0 {
		maxCommits = 5
	}
	if maxCommits > 20 {
		maxCommits = 20
	}
	out, err := hardenedGitStatus(abs, includeDiffStat, maxCommits)
	if err != nil {
		return "error: " + err.Error()
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// hardenedGitStatus reports `git status --short --branch`-style lines, recent
// commits and diff stats WITHOUT letting git read the working tree. Agents
// call this tool on repositories they control, and the daemon runs it outside
// any sandbox: plain `git status` / `git diff --stat` would run the repo's
// clean filters, fsmonitor hook and (for partial clones) a fetch transport.
// Staged changes compare HEAD with the index; unstaged changes compare the
// index with a daemon-built snapshot of the working tree (turns.go — files
// read by the daemon itself, never by git); untracked files come from a plain
// directory listing. All git calls go through the hardened turnGit runner.
func hardenedGitStatus(dir string, includeDiffStat bool, maxCommits int) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), turnSnapshotTimeout)
	defer cancel()
	release, err := acquireDiffEndpointSlot(ctx)
	defer release()
	if err != nil {
		return nil, err
	}
	repoRoot, err := turnResolveRepo(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repo: %v", err)
	}

	branch := "## " + gitStatusBranchLine(ctx, repoRoot)
	nameStatus := func(rev ...string) map[string]string {
		args := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--submodule=short", "--name-status", "-z"}, rev...)
		raw, err := turnGit(ctx, repoRoot, nil, nil, args...)
		m := map[string]string{}
		if err != nil {
			return m
		}
		f := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")
		for i := 0; i+1 < len(f); i += 2 {
			m[f[i+1]] = f[i][:1]
		}
		return m
	}
	staged := nameStatus("--cached")
	tracked := map[string]bool{}
	if raw, err := turnGit(ctx, repoRoot, nil, nil, "ls-files", "-z"); err == nil {
		for _, p := range strings.Split(strings.TrimRight(raw, "\x00"), "\x00") {
			if p != "" {
				tracked[p] = true
			}
		}
	}
	unstaged := map[string]string{}
	var snapTree, unknown string
	if snap, err := (gitTurnSnapshotter{}).snapshot(ctx, repoRoot); err != nil {
		unknown = "working-tree changes could not be read: " + err.Error()
	} else {
		if snap.trackedUnread > 0 {
			unknown = fmt.Sprintf("%d tracked path(s) (large, unreadable or submodules) were not compared and may have unstaged changes", snap.trackedUnread)
		}
		snapTree = snap.tree
		for path, st := range nameStatus("-R", "--cached", snap.tree) {
			if tracked[path] { // untracked files are listed separately below
				unstaged[path] = st
			}
		}
	}
	lines := []string{branch}
	paths := map[string]bool{}
	for p := range staged {
		paths[p] = true
	}
	for p := range unstaged {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		x, y := staged[p], unstaged[p]
		if x == "" {
			x = " "
		}
		if y == "" {
			y = " "
		}
		lines = append(lines, x+y+" "+p)
	}
	if raw, err := turnGit(ctx, repoRoot, nil, nil, "ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory", "-z"); err == nil {
		for _, p := range strings.Split(strings.TrimRight(raw, "\x00"), "\x00") {
			if p != "" {
				lines = append(lines, "?? "+p)
			}
		}
	}
	logOut, _ := turnGit(ctx, repoRoot, nil, nil, "log", "--no-show-signature", "--oneline", "--no-decorate", fmt.Sprintf("-%d", maxCommits))
	out := map[string]any{
		"root":    repoRoot,
		"status":  lines,
		"commits": splitNonEmptyLines(logOut),
	}
	if unknown != "" {
		out["unstaged_unknown"] = unknown
	}
	if includeDiffStat {
		stat := func(rev ...string) []string {
			args := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--submodule=short", "--stat"}, rev...)
			raw, _ := turnGit(ctx, repoRoot, nil, nil, args...)
			return splitNonEmptyLines(raw)
		}
		// Unstaged stat covers tracked paths only (as `git diff --stat`); the
		// snapshot tree also holds untracked files.
		if snapTree != "" && len(unstaged) > 0 && len(unstaged) <= 1000 {
			args := []string{"-R", "--cached", snapTree, "--"}
			for p := range unstaged {
				args = append(args, ":(literal)"+p)
			}
			out["diff_stat"] = stat(args...)
		} else if snapTree != "" && len(unstaged) > 1000 {
			out["diff_stat"] = stat("-R", "--cached", snapTree)
		} else {
			out["diff_stat"] = []string{}
		}
		out["staged_diff_stat"] = stat("--cached")
	}
	return out, nil
}

// gitStatusBranchLine renders the "## branch...upstream [ahead N, behind M]"
// header of `git status --short --branch` from ref queries alone.
func gitStatusBranchLine(ctx context.Context, repo string) string {
	head, err := turnGit(ctx, repo, nil, nil, "rev-parse", "--abbrev-ref", "HEAD")
	head = strings.TrimSpace(head)
	if err != nil || head == "" {
		return "No commits yet"
	}
	if head == "HEAD" {
		return "HEAD (no branch)"
	}
	up, err := turnGit(ctx, repo, nil, nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	up = strings.TrimSpace(up)
	if err != nil || up == "" {
		return head
	}
	line := head + "..." + up
	if counts, err := turnGit(ctx, repo, nil, nil, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); err == nil {
		var ahead, behind int
		fmt.Sscanf(strings.TrimSpace(counts), "%d %d", &ahead, &behind)
		var parts []string
		if ahead > 0 {
			parts = append(parts, fmt.Sprintf("ahead %d", ahead))
		}
		if behind > 0 {
			parts = append(parts, fmt.Sprintf("behind %d", behind))
		}
		if len(parts) > 0 {
			line += " [" + strings.Join(parts, ", ") + "]"
		}
	}
	return line
}

func splitNonEmptyLines(s string) []string {
	raw := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func (a *Aria) toolSearchFiles(root, query, kind string, limit int) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "error: root required"
	}
	abs, err := canonicalizeDirPath(root)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if reason := credentialPathRefusal(abs, a.dataDir); reason != "" {
		return "error: " + reason
	}
	if info, err := os.Stat(abs); err != nil {
		return fmt.Sprintf("error: root not found: %v", err)
	} else if !info.IsDir() {
		return "error: root is not a directory"
	}
	if limit <= 0 {
		limit = defaultSearchHits
	}
	if limit > maxSearchHits {
		limit = maxSearchHits
	}
	kind = strings.TrimSpace(kind)
	includeFiles := kind == "" || kind == "files" || kind == "all"
	includeDirs := kind == "dirs" || kind == "all"
	if !includeFiles && !includeDirs {
		return `error: kind must be "files", "dirs", or "all"`
	}
	// Agents never see secret/credential files, regardless of transport.
	hits, walked, capped := walkFileSearch(abs, query, limit, includeFiles, includeDirs, true, a.dataDir)
	b, _ := json.Marshal(map[string]any{
		"root":   abs,
		"hits":   hits,
		"walked": walked,
		"capped": capped,
	})
	return string(b)
}

func (a *Aria) toolReadFile(path string, startLine, maxLines int) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "error: path required"
	}
	abs, err := canonicalizeFilePath(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	// Agents get the full secret/credential denylist, not just .env detection
	// — an agent reading id_rsa / .aws/credentials / .solu/token would funnel
	// it straight into the model's context. This is strictly stronger than
	// the old looksLikeEnvFile check.
	if reason := credentialPathRefusal(abs, a.dataDir); reason != "" {
		return "error: " + reason
	}
	if reason := secretPathRefusal(abs); reason != "" {
		return "error: " + reason
	}
	data, _, err := readUserFile(abs, maxFileBytes)
	if err != nil {
		msg, _ := userFileReadError(err)
		return "error: " + msg
	}
	if containsNullByte(data) {
		return "error: file appears binary"
	}
	if startLine <= 0 {
		startLine = 1
	}
	if maxLines <= 0 {
		maxLines = 120
	}
	if maxLines > 200 {
		maxLines = 200
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start := startLine - 1
	if start >= len(lines) {
		return fmt.Sprintf("error: start_line %d is past end of file (%d lines)", startLine, len(lines))
	}
	end := start + maxLines
	if end > len(lines) {
		end = len(lines)
	}
	type outLine struct {
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	outLines := make([]outLine, 0, end-start)
	for i := start; i < end; i++ {
		outLines = append(outLines, outLine{Line: i + 1, Text: lines[i]})
	}
	b, _ := json.Marshal(map[string]any{
		"path":        abs,
		"start_line":  startLine,
		"end_line":    end,
		"total_lines": len(lines),
		"truncated":   end < len(lines),
		"lines":       outLines,
	})
	return string(b)
}

// toolSendInput parks the tool loop on the auto-countdown confirm,
// then (if approved) writes the bytes to the pane's PTY. The result
// string is what the model sees on the next turn — so rejections
// and errors must read like prose a model can reason about, not
// stack traces.
func (a *Aria) toolSendInput(ctx context.Context, toolUseID, paneID, input string, autonomousPromptPos int64, autonomousPromptHash uint64, autonomousCautious bool, emit func(map[string]any)) string {
	return a.toolSendInputWithJevProof(ctx, toolUseID, paneID, input, autonomousPromptPos, autonomousPromptHash, autonomousCautious, nil, emit)
}

func (a *Aria) toolSendInputWithJevProof(ctx context.Context, toolUseID, paneID, input string, autonomousPromptPos int64, autonomousPromptHash uint64, autonomousCautious bool, proof *jevAmbiguousProof, emit func(map[string]any)) string {
	if strings.TrimSpace(paneID) == "" {
		return "error: pane_id required"
	}
	if input == "" {
		return `error: input is empty. Include at least one character — use "\n" to send a bare Enter.`
	}
	// Normalize literal escape sequences to their real byte values.
	// The model routinely emits `\\n` in JSON (two bytes: backslash +
	// n) instead of an actual newline — doesn't matter how carefully
	// the prompt spells it out. Rather than fight the model, unescape
	// here so both encodings mean the same thing at the PTY.
	input = unescapeAriaInput(input)
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return fmt.Sprintf("error: no pane with id %q", paneID)
	}
	var policy ariaAutoModePolicy
	if autonomousCautious {
		ctx, policy = withAriaAutoModePolicy(ctx, p)
		if policy.policy.mode != "cautious" || !policy.matches(p) {
			emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": "approval policy changed"})
			return "skipped: auto-approval policy changed during evaluation"
		}
	}
	if proof != nil && (input != "1\n" || !proof.valid(p) || !shouldSubmitFocusedChoiceWithEnter(p, input)) {
		return "skipped: Jev permission proof does not bind focused option 1 to Enter"
	}
	if autonomousCautious {
		tail, rendered := cautiousPromptSnapshot(p, tailOfPane(p, 30))
		// A short screen may show the approval rows after the command has
		// scrolled away. Neither evaluator can authorize that missing action.
		if rendered && !cautiousRenderedHasActionContext(tail) {
			return "skipped: cautious auto-approval denied by daemon safety gate: current action is not visible"
		}
		if reason := cautiousAutoDenyReasonWithJevProof(tail, input, proof != nil && proof.valid(p)); reason != "" {
			log.Printf("aria: cautious auto-send pane=%s denied: %s", paneID, reason)
			return "skipped: cautious auto-approval denied by daemon safety gate: " + reason
		}
	}

	preview := truncateStr(strings.ReplaceAll(strings.ReplaceAll(input, "\n", "↵"), "\r", "␍"), 80)
	confirmID, approved, auto, terminal, explicitReject := a.awaitConfirmDecision(ctx, toolUseID, confirmOpts{
		Tool:        "send_input",
		Summary:     fmt.Sprintf("Send to %s: %s", p.Title, preview),
		Risk:        riskForSendInput(input),
		Extra:       map[string]any{"pane_id": paneID, "pane_title": p.Title, "input": input},
		AutoConfirm: true,
	}, emit)
	if terminal {
		return "error: send_input did not run (cancelled, timed out, or stream closed)"
	}
	if !approved {
		if autonomousCautious && !explicitReject {
			emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": "confirmation blocked by pause or limits"})
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false})
			return "skipped: autonomous confirmation was blocked by pause or limits"
		}
		if autonomousCautious {
			if policy.matches(p) {
				a.rememberJevCautiousForPolicy(policy, autonomousPromptPos, autonomousPromptHash, "cancelled")
			} else {
				emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": "approval policy changed"})
			}
		}
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false})
		return "user rejected the send_input request. Do not retry the same input — ask what they'd prefer to send instead."
	}
	// Stale-write guard for the autonomous path. Between the LLM
	// producing tool_use("send_input", ...) and this code writing to
	// the PTY, several seconds can pass (LLM call + tool dispatch).
	// During that window, Claude may have processed something else
	// and moved past the menu we were answering — so the "1\n" would
	// type "1" into an empty prompt and Enter, submitting "1" as a
	// free-form message. Re-check the pane state right before the
	// write; if the menu is gone, abort.
	if auto && p.computeState() != StateChoicePrompt {
		log.Printf("aria: send_input pane=%s aborted (state=%s, no longer choice-prompt) — menu advanced before write",
			paneID, p.computeState())
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "pane state advanced past the menu"})
		return "skipped: pane state advanced past the menu before the input could land. The menu was likely answered already; no action needed."
	}
	if auto && !choicePromptIdentityMatches(p, autonomousPromptPos, autonomousPromptHash) {
		log.Printf("aria: send_input pane=%s aborted (prompt changed: was=%d now=%d)",
			paneID, autonomousPromptPos, atomic.LoadInt64(&p.LastAckedChoicePromptPos))
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "pane prompt changed before write"})
		return "skipped: pane prompt changed before the input could land. The menu was likely replaced by a newer prompt."
	}
	if auto && proof != nil && !proof.valid(p) {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "Jev permission menu changed before write"})
		return "skipped: Jev permission menu changed before input could land."
	}
	// User-input hold, the LLM-path twin of directAutoFire's (autoFireUserHold).
	// The two guards above prove the menu is still there; neither proves the
	// human is not typing into the pane right now. If the menu has just been
	// answered by hand, the bytes below land in an ordinary composer and submit
	// whatever is half-written there.
	//
	// This is the RACE BACKSTOP, not the main gate: evaluateObservation checks
	// the same hold before spending a turn, and only typing that starts during
	// the turn reaches here. Keep both.
	//
	// The `auto` guard is belt-and-braces and currently always true (executeTool
	// serves autonomous turns only, and awaitConfirm's AutoConfirm is hardcoded
	// for send_input). It stays as the marker for where a non-autonomous caller
	// would have to opt out, since deferring a send a human explicitly asked
	// for would just make it vanish: nothing re-drives that path.
	//
	// Emitted as auto-hold rather than recorded here so evaluateObservation can
	// route it away from the sweeper-suspend streak. A hold is not a decline:
	// the menu is still pending and the next sweep should retry it.
	if auto {
		if hold, holdReason := autoFireUserHold(p, time.Now()); hold {
			log.Printf("aria: send_input pane=%s held (%s)", paneID, holdReason)
			emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": holdReason})
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "held: " + holdReason})
			return "skipped: the user is typing in this pane, so sending now would submit their half-written message. Do not retry. The pane is re-evaluated automatically once they stop."
		}
	}

	// Defensive auto-submit on obvious menu replies. Reasoning models
	// (DeepSeek V4) sometimes drop the trailing newline on short
	// answers like "1" / "y" / "no" — the prompt explicitly says to
	// include "\n" but the model occasionally forgets, leaving "1"
	// typed but unsubmitted in the input box. Without this backstop,
	// the next eval iteration sees the pane still at choice-prompt
	// and types another "1" — accumulating "111" in the buffer until
	// some iteration finally adds "\n", at which point Claude submits
	// the wrong-looking but functionally-correct "111\r".
	//
	// Heuristic: single-digit numbers, single-letter y/n, "yes"/"no"
	// (case-insensitive). Anything longer/different is left alone so
	// genuine multi-character text without an Enter (rare but possible)
	// still works as the user typed it.
	if !strings.ContainsAny(input, "\r\n") && isObviousMenuReply(input) {
		log.Printf("aria: send_input pane=%s auto-appending newline to short reply %q", paneID, input)
		input = input + "\n"
	}
	if proof != nil {
		if !proof.valid(p) || !shouldSubmitFocusedChoiceWithEnter(p, input) {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "Jev focused option changed before Enter"})
			return "skipped: Jev focused option changed before Enter"
		}
		input = "\n"
	} else if auto && shouldSubmitFocusedChoiceWithEnter(p, input) {
		log.Printf("aria: send_input pane=%s using Enter-only submit for focused option 1", paneID)
		input = "\n"
	}
	// Split the input into body + "submit" flag. A single trailing
	// \n / \r / \r\n is treated as "press Enter after" — we write
	// it as a separate PTY frame after a short delay. Anything else
	// (embedded newlines, multi-line text, raw control chars) is
	// written verbatim in one frame — the user explicitly asked for
	// those bytes.
	//
	// WHY the split exists: see pty_submit.go (EnterGap) for the
	// rationale and the timing numbers. Mirrored across daemon
	// (this file + aria_auto.go) and web (ptySubmit.ts).
	body, submit := splitTrailingEnter(input)
	hasClaude := p.HasClaudeDescendant()
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()

	// Menu-choice / approval replies (a single digit / y / n, or a
	// focused-option bare Enter) are control answers to a TUI menu, not
	// user prompts. Write them without feeding the prompt recorder so
	// they neither appear in the last-prompts strip nor — when the
	// submit-Enter is skipped because the prompt changed before the
	// write (the abort branches below) — leave the body digit lingering
	// in the recorder buffer to prepend onto the next real prompt.
	// Free-form text still records as before.
	recordPrompt := !(isObviousMenuReply(body) || (submit && body == ""))
	writeInput := p.Write
	if !recordPrompt {
		writeInput = p.WriteNoPromptRecord
	}
	if proof != nil {
		writeInput = func(b []byte) (int, error) {
			if string(b) != "\r" {
				return 0, fmt.Errorf("Jev may only submit bare Enter")
			}
			return a.writeJevEnter(ctx, p, proof)
		}
	}
	if auto && autonomousCautious {
		write := writeInput
		writeInput = func(b []byte) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if !policy.matches(p) {
				emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": "approval policy changed"})
				return 0, errAutoModePolicyChanged
			}
			var n int
			var err error
			if proof != nil {
				// The Jev transaction checks the captured policy under p.mu.
				n, err = write(b)
			} else {
				deadline := time.Now().Add(ariaJevTimeout)
				if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
					deadline = requestDeadline
				}
				n, err = p.writeWithAutoModePolicy(b, recordPrompt, policy.policy, deadline)
			}
			if err == errAutoModePolicyChanged {
				emit(map[string]any{"type": "auto-hold", "pane_id": paneID, "reason": "approval policy changed"})
			}
			return n, err
		}
	}

	var wrote int
	if body != "" {
		// Non-Claude submits: wrap body in bracketed paste so
		// multi-word text isn't sliced per-word by TUI paste
		// detectors. Kept out of the Claude path because Bubble
		// Tea's bracketed-paste support is version-dependent and
		// v0.0.135 regressed Claude when we forced it there. Also
		// keep obvious Codex approval replies raw because Ratatui
		// modals expect shortcut keys (y / p / esc / enter), not
		// paste payloads.
		payload := body
		if submit && !hasClaude && !(hasCodex && isObviousMenuReply(body)) {
			payload = "\x1b[200~" + body + "\x1b[201~"
		}
		n, err := writeInput([]byte(payload))
		if err != nil {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": err.Error()})
			return fmt.Sprintf("error: write to pane %q failed: %v", paneID, err)
		}
		wrote += n
	}
	if submit {
		gap := time.Duration(0)
		if body != "" {
			gap = EnterGap(hasClaude)
		}
		log.Printf("aria: send_input pane=%s hasClaude=%v hasCodex=%v body=%q submit=%v gap=%v auto=%v",
			paneID, hasClaude, hasCodex, body, submit, gap, auto)
		if gap > 0 {
			select {
			case <-ctx.Done():
				emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "cancelled before Enter"})
				return fmt.Sprintf("partial: wrote %d bytes then request cancelled before Enter was sent", wrote)
			case <-time.After(gap):
			}
		}
		if auto && !choicePromptIdentityMatches(p, autonomousPromptPos, autonomousPromptHash) {
			log.Printf("aria: send_input pane=%s aborted before Enter (prompt changed: was=%d now=%d)",
				paneID, autonomousPromptPos, atomic.LoadInt64(&p.LastAckedChoicePromptPos))
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "pane prompt changed before Enter"})
			return fmt.Sprintf("partial: wrote %d bytes then skipped Enter because the pane prompt changed", wrote)
		}
		if proof != nil {
			tail := tailOfPane(p, 30)
			if autonomousCautious {
				tail = cautiousPromptText(p, tail)
			}
			if !proof.valid(p) || !ariaJevFocusedFirstOption(tail) {
				emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": "Jev permission proof changed before Enter"})
				return fmt.Sprintf("partial: wrote %d bytes then skipped Enter because Jev permission proof changed", wrote)
			}
		}
		n, err := writeInput([]byte("\r"))
		if err != nil {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto, "error": err.Error()})
			return fmt.Sprintf("error: wrote %d bytes but submit-Enter failed: %v", wrote, err)
		}
		wrote += n
		if auto {
			a.verifyAutoFireLanded(paneID, p.Title, atomic.LoadInt64(&p.LastInputMs))
		}
	} else {
		log.Printf("aria: send_input pane=%s hasClaude=%v hasCodex=%v input=%q submit=false auto=%v",
			paneID, hasClaude, hasCodex, input, auto)
	}
	emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "auto": auto})
	b, _ := json.Marshal(map[string]any{
		"ok":            true,
		"pane_id":       paneID,
		"bytes_written": wrote,
		"submitted":     submit,
	})
	return string(b)
}

func cautiousAutoDenyReason(tail, input string) string {
	return cautiousAutoDenyReasonWithJevProof(tail, input, false)
}

// Team evaluation supports more permission types than Jev's deliberately
// narrow admission check. Use the full visible preamble, including long
// diffs, but still require an action payload in the current dialog.
func cautiousRenderedHasActionContext(tail string) bool {
	if cautiousDialogHasElision(tail) {
		return false
	}
	focus := ariaJevFocusedOptionRE.FindAllStringIndex(tail, -1)
	if len(focus) == 0 {
		return false
	}
	lines := strings.Split(tail[:focus[len(focus)-1][0]], "\n")
	at := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if len(choicePromptMatches(line)) > 0 || escapeFooterRE.MatchString(normalizePromptText(line)) {
			break
		}
		header := ariaJevActionHeaderRE.MatchString(line) || ariaJevToolActionHeaderRE.MatchString(line) || ariaJevFullActionHeader(line) || line == "Would you like to grant these permissions?"
		for _, title := range []string{"bash command", "edit file", "create file", "read file", "write file", "tool use", "fetch"} {
			lower := strings.ToLower(line)
			header = header || lower == title || strings.HasPrefix(lower, title+" ·")
		}
		if header {
			at = i
			break
		}
	}
	if at < 0 {
		return false
	}
	for _, payload := range lines[at+1:] {
		payload = strings.TrimSpace(payload)
		lower := strings.ToLower(payload)
		if payload != "" && strings.Trim(payload, "─╌━-│ ") != "" && !strings.HasSuffix(payload, "?") &&
			!strings.HasPrefix(lower, "do you want") && !strings.HasPrefix(lower, "would you like") &&
			!strings.HasPrefix(lower, "reason:") && !strings.HasPrefix(lower, "environment:") {
			return true
		}
	}
	return false
}

// Codex marks a header whose command/patch body does not fit in the picker.
// Its title may still be visible, but the action is incomplete.
func cautiousDialogHasElision(tail string) bool {
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[… ") {
			continue
		}
		if end := strings.Index(line, " lines]"); end > len("[… ") {
			count := line[len("[… "):end]
			if strings.Trim(count, "0123456789") == "" {
				return true
			}
		}
	}
	return false
}

func cautiousAutoDenyReasonWithJevProof(tail, input string, allowAmbiguous bool) string {
	body, submit := splitTrailingEnter(input)
	if submit {
		input = body
	}
	if !isObviousMenuReply(input) {
		return "reply is not a simple menu choice"
	}
	if !choicePromptRE.MatchString(tail) {
		return "no focused numbered option menu is visible"
	}
	if !isAutoApprovalPromptText(tail, false) && !(allowAmbiguous && autoApprovalDeclineReason(tail, false) == "choice menu, not a permission prompt") {
		return "prompt is a choice menu, not an approval prompt"
	}
	lowerTail := strings.ToLower(tail)
	selected := strings.ToLower(choiceLineForInput(tail, input))
	wholePromptDeny := []struct {
		needle string
		reason string
	}{
		{".env", "prompt mentions environment/secret files"},
		{"api key", "prompt mentions credentials or secrets"},
		{"apikey", "prompt mentions credentials or secrets"},
		{"secret", "prompt mentions credentials or secrets"},
		{"token", "prompt mentions credentials or secrets"},
		{"credential", "prompt mentions credentials or secrets"},
		{"~/.ssh", "prompt touches SSH credentials"},
		{"/.ssh", "prompt touches SSH credentials"},
		{"/etc/", "prompt touches system configuration"},
		{"/usr/", "prompt touches system paths"},
		{"/var/", "prompt touches system paths"},
		{"/root/", "prompt touches system paths"},
		{"sudo ", "prompt would run privileged command"},
		{"rm -rf", "prompt would run a destructive command"},
		{"shred ", "prompt would run a destructive command"},
		{"dd if=", "prompt would run a raw-disk command"},
		{"mkfs", "prompt would format a filesystem"},
		{"truncate -s", "prompt would truncate files"},
		{"del /f", "prompt would run a destructive command"},
		{"rd /s", "prompt would run a destructive command"},
		{"rmdir /s", "prompt would run a destructive command"},
		{"chmod ", "prompt would change permissions"},
		{"chown ", "prompt would change ownership"},
		{"killall ", "prompt would kill processes"},
		{"pkill ", "prompt would kill processes"},
		{"shutdown ", "prompt would shut the machine down"},
		{"systemctl ", "prompt would touch system services"},
		{"crontab ", "prompt would change scheduled jobs"},
		{"git push", "prompt would mutate a remote repository"},
		{"force-push", "prompt would rewrite remote git history"},
		{"git reset --hard", "prompt would rewrite local git state"},
		{"git clean", "prompt would delete untracked files"},
		{"git checkout -- ", "prompt would discard local changes"},
		{"git stash drop", "prompt would discard stashed changes"},
		{"npm install", "prompt would install packages"},
		{"pnpm install", "prompt would install packages"},
		{"yarn install", "prompt would install packages"},
		{"pip install", "prompt would install packages"},
		{"go install", "prompt would install packages"},
		{"cargo install", "prompt would install packages"},
		{"npx ", "prompt would run an arbitrary package"},
		{"pipx ", "prompt would run an arbitrary package"},
		{"apt install", "prompt would install packages"},
		{"apt-get install", "prompt would install packages"},
		{"brew install", "prompt would install packages"},
		{"dnf install", "prompt would install packages"},
		{"yum install", "prompt would install packages"},
		{"kubectl delete", "prompt would delete cluster resources"},
		{"terraform apply", "prompt would change infrastructure"},
		{"terraform destroy", "prompt would destroy infrastructure"},
		{"drop table", "prompt would drop database objects"},
		{"drop database", "prompt would drop database objects"},
		{"curl ", "prompt would hit the network"},
		{"wget ", "prompt would hit the network"},
		{"http://", "prompt mentions a network endpoint"},
		{"https://", "prompt mentions a network endpoint"},
	}
	for _, rule := range wholePromptDeny {
		if cautiousContainsWrappedRisk(lowerTail, rule.needle) {
			return rule.reason
		}
	}
	selectedDeny := []struct {
		needle string
		reason string
	}{
		{"don't ask again", "selected option would persist future approvals"},
		{"dont ask again", "selected option would persist future approvals"},
		{"allow all", "selected option would persist broad approvals"},
		{"always allow", "selected option would persist broad approvals"},
		{"trust", "selected option would persist broad trust"},
	}
	for _, rule := range selectedDeny {
		if cautiousContainsWrappedRisk(selected, rule.needle) {
			return rule.reason
		}
	}
	return ""
}

// Permit physical row breaks within a deny phrase without deleting ordinary
// spaces. In particular, "rd /s" must not match "dashboard/src", and the
// required separator after "sudo " must not disappear inside "pseudo".
func cautiousContainsWrappedRisk(text, needle string) bool {
	if strings.Contains(text, needle) {
		return true
	}
	if needle == "" || !strings.Contains(text, "\n") {
		return false
	}
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	for start := 0; start < len(text); start++ {
		if text[start] != needle[0] {
			continue
		}
		pos, matched := start, true
		for i := 0; i < len(needle); i++ {
			if needle[i] == ' ' {
				if pos >= len(text) || !space(text[pos]) {
					matched = false
					break
				}
				for pos < len(text) && space(text[pos]) {
					// A blank row separates UI regions, rather than wrapping
					// a word in one physical terminal row.
					if text[pos] == '\n' && pos+1 < len(text) {
						next := pos + 1
						for next < len(text) && (text[next] == ' ' || text[next] == '\t' || text[next] == '\r') {
							next++
						}
						if next < len(text) && text[next] == '\n' {
							matched = false
							break
						}
					}
					pos++
				}
				if !matched {
					break
				}
				continue
			}
			// Only a row boundary permits skipping presentation indentation.
			if pos < len(text) && text[pos] == '\r' {
				pos++
			}
			if pos < len(text) && text[pos] == '\n' {
				pos++
				for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t') {
					pos++
				}
			}
			if pos >= len(text) || text[pos] != needle[i] {
				matched = false
				break
			}
			pos++
		}
		if matched {
			return true
		}
	}
	return false
}

func choiceLineForInput(tail, input string) string {
	choice := strings.TrimSpace(unescapeAriaInput(input))
	choice = strings.Trim(choice, "\r\n\t ")
	if choice == "" {
		choice = "1"
	}
	lines := strings.Split(tail, "\n")
	withContinuation := func(i int) string {
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" && !menuOptionRowRE.MatchString(lines[end]) {
			end++
		}
		return strings.Join(lines[i:end], "\n")
	}
	if len(choice) > 0 && choice[0] >= '0' && choice[0] <= '9' {
		// Earlier permission menus remain in scrollback. The newest matching
		// row belongs to the menu currently awaiting input; choosing the first
		// row could miss a persistent grant in the active menu.
		for i := len(lines) - 1; i >= 0; i-- {
			if row := menuOptionRowRE.FindStringSubmatch(lines[i]); row != nil && row[1] == choice {
				return withContinuation(i)
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, "❭") || strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, "▶") {
			return withContinuation(i)
		}
	}
	return ""
}

func riskForSendInput(input string) string {
	trimmed := strings.TrimSpace(unescapeAriaInput(input))
	if isObviousMenuReply(trimmed) {
		return "low"
	}
	lower := strings.ToLower(trimmed)
	// Defeat trivial evasion: case, whitespace runs, and escaping —
	// `RM  -RF`, `rm\t-rf`, `rm -rf  /` all collapse to the same form.
	normalized := strings.Join(strings.Fields(lower), " ")
	for _, marker := range []string{
		"rm -rf", "sudo ", "chmod ", "chown ", "git push", "git reset --hard", "git clean",
		"npm install", "pnpm install", "yarn install", "pip install", "curl ", "wget ",
		".env", "api key", "secret", "token", "password",
		"iptables", "kubectl delete", "dd ", "mkfs", "> /dev/", "chmod 777", ":(){:|:&};:",
	} {
		if strings.Contains(lower, marker) || strings.Contains(normalized, marker) {
			return "high"
		}
	}
	if strings.ContainsAny(trimmed, "\r\n") || len(trimmed) > 200 {
		return "medium"
	}
	return "medium"
}
