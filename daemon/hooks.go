package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Claude Code hook integration — the real source of truth for "what
// did Claude actually do?" The activity feed previously saw only
// pane-state transitions + our own MCP server's tool calls (today just
// preview_file). Claude's built-in tools (Edit, Write, Read, Bash,
// Grep, Glob, Task, TodoWrite, WebFetch, …) run inside Claude's
// process and never touch our daemon — so the feed missed the actual
// work.
//
// Claude Code's PostToolUse hook fires for EVERY tool invocation,
// including built-ins. We auto-register an HTTP hook pointing at
// POST /api/hooks/tool-call, and attribute the call to a pane via a
// SOLU_PANE_ID env var we inject at pane-spawn time.
//
// Threat model: loopback-only. The handler is reached exclusively from
// a Claude Code process running on the same machine as the daemon.
// isLoopback() guards the route; rejection for anything else.
//
// Privacy: we redact arguments aggressively before publishing to the
// uiBus. Tool name + basename-only hint. Commands (Bash) get the first
// ~60 chars and NOTHING if they look like they might be exporting
// secrets. No stdout, no stderr, no diffs — those live in the
// scrollback already and aren't duplicated into the event stream.

// hookPayload is the JSON Claude Code POSTs to our endpoint. Fields
// beyond these exist (session_id, cwd, transcript_path, etc.) but
// we ignore them for privacy-by-default — only the minimum needed to
// attribute + render makes it into the event.
type hookPayload struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	SessionID     string         `json:"session_id"`
	CWD           string         `json:"cwd"`
	// TranscriptPath is the .jsonl Claude Code is writing for THIS event.
	// We use only its shape (top-level conversation vs nested subagent/
	// workflow) to decide whether the event may (re)pin the pane's
	// conversation — never its contents. See isMainTranscriptPath.
	TranscriptPath string `json:"transcript_path"`
	// ToolResponse carries the tool's exit/result for PostToolUse. We
	// intentionally don't log its contents; only use it to note success
	// vs failure where trivially available.
	ToolResponse map[string]any `json:"tool_response"`
}

// isMainTranscriptPath reports whether tp points at a pane's MAIN conversation
// transcript ("<...>/projects/<enc>/<sessionId>.jsonl") rather than a subagent
// or Workflow transcript (nested under "<sessionId>/subagents/...").
//
// Why it matters: subagents and Workflow agents inherit the parent's
// SOLU_PANE_ID and ~/.claude/settings.json, so they fire this same hook with
// the parent pane's header but THEIR own session_id + a nested transcript_path.
// Letting those (re)pin the pane strands it on a subagent/workflow session id
// that has no top-level "<sid>.jsonl" — an "orphan" — so the conversation
// endpoint finds 0 turns. Gating on the path shape keeps only the real
// conversation pinned.
//
// Empty tp returns true: older Claude Code versions omit transcript_path, and
// we must not regress their (permissive) behavior.
func isMainTranscriptPath(tp string) bool {
	tp = strings.TrimSpace(tp)
	if tp == "" {
		return true
	}
	tp = filepath.ToSlash(tp)
	if !strings.HasSuffix(tp, ".jsonl") {
		return false
	}
	// A main transcript's grandparent dir is "projects":
	//   .../projects/<enc>/<sid>.jsonl  → Dir=.../projects/<enc>, gp="projects"
	// A subagent/workflow transcript nests one level deeper, so its grandparent
	// is "<sid>" (or "subagents"), never "projects".
	return filepath.Base(filepath.Dir(filepath.Dir(tp))) == "projects"
}

// hookToolCall handles POST /api/hooks/tool-call. Claude Code calls this
// synchronously; we want to return fast (<10ms on happy path) so the
// agent isn't stalled.
func (a *apiServer) hookToolCall(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "loopback-only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload hookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		// Malformed hook — still return 200 with empty body so Claude
		// doesn't treat us as a blocking failure. Log for diagnosis.
		log.Printf("hook: invalid payload: %v", err)
		writeJSON(w, 200, map[string]any{})
		return
	}

	// Pane attribution with a two-tier fallback:
	//
	//   1. X-Soluterminal-Pane header — set when Claude Code forwards
	//      our SOLU_PANE_ID env var via allowedEnvVars. Ideal path.
	//   2. cwd from the payload → match the most-recently-active
	//      running pane whose spawn cwd (or live cwd) equals it. Robust
	//      fallback for Claude Code versions where the `$VAR` header
	//      interpolation doesn't work as documented, or sessions that
	//      were started before SOLU_PANE_ID was injected into their env.
	//
	// Logging once per session (gated by hookLogged cache) gives us a
	// trail of what attribution signals are actually arriving without
	// spamming every tool call.
	// The pane header is THE in-app marker: in-app sessions carry our
	// SOLU_PANE_ID (forwarded as X-Soluterminal-Pane); out-app `claude`
	// runs in a bare terminal don't. Capture it before the cwd fallback
	// overwrites paneID — the cc-usage ledger keys on header-present, not
	// on whether attribution ultimately resolved a pane.
	paneHeader := r.Header.Get("X-Soluterminal-Pane")
	paneID := paneHeader
	if paneID == "" {
		paneID = attributeByCwd(a.mgr, payload.CWD)
	}
	// Record the session's in-app flag for the transcript scanner. Every
	// hook event (in-app or out-app) fires this; first-seen wins, in_app
	// upgrades only. Guarded for tests that build apiServer without a ledger.
	if a.ccLedger != nil && payload.SessionID != "" {
		a.ccLedger.Record(payload.SessionID, paneHeader != "")
		// Attribute the session to the currently-active Claude account on
		// SessionStart — it fires once at the start of every session, when the
		// active-cred pointer already reflects the account that will produce
		// the turns. RecordAccount is first-non-empty-wins, so this only fills
		// in. Guarded for tests/local-only daemons without a CredStore.
		// UserPromptSubmit (once per user turn, unlike the per-tool events)
		// also fills a session whose SessionStart missed; RecordAccount never
		// overwrites a recorded or authoritative value, and the lookup — which
		// may read the large .claude.json — is only paid while unattributed.
		if a.claudeLogin != nil && (payload.HookEventName == "SessionStart" ||
			(payload.HookEventName == "UserPromptSubmit" && a.ccLedger.LookupAccount(payload.SessionID) == "" &&
				hookAccountRetryDue(payload.SessionID, time.Now()))) {
			if acct := a.claudeLogin.UsageAccountLabel(); acct != "" {
				a.ccLedger.RecordAccount(payload.SessionID, acct)
			}
		}
	}
	var paneTitle string
	if paneID != "" {
		if pane, ok := a.mgr.Get(paneID); ok {
			paneTitle = pane.Title
			// Only a MAIN-session event may (re)pin this pane's conversation.
			// Subagent/Workflow events carry the parent's pane header but a
			// nested transcript_path; pinning on those would strand the pane on
			// an orphan session id with no top-level transcript (0 turns). See
			// isMainTranscriptPath.
			if payload.SessionID != "" && isMainTranscriptPath(payload.TranscriptPath) {
				// An authoritative in-pane Claude hook is also provider handoff
				// evidence for a shell previously owned by Codex. Clear the hidden
				// old Claude/Codex identities first, then pin THIS hook's current
				// session below in the same request. Cwd fallback is not strong
				// enough to change providers.
				identityChanged := false
				if paneHeader != "" {
					identityChanged = pane.SetForegroundClaudeIdentity()
				}
				// Guard cwd-fallback MISATTRIBUTION. With the header present the hook
				// came from THIS pane's own claude (authoritative) → always (re)pin.
				// With it absent we resolved the pane by cwd — the most-recently-active
				// pane sharing the dir — so a stray out-app `claude` (or one whose
				// SOLU_PANE_ID didn't propagate) in a shared cwd must NOT clobber an
				// in-app pane's resume identity. A cwd-resolved hook may only SEED an
				// id on a pane that has none (or re-affirm the same id), never replace
				// a different existing one. (The SessionStart hook now fires for EVERY
				// session start, so without this an out-app launch could overwrite an
				// in-app pane's id before running a single tool.)
				priorID := strings.TrimSpace(pane.Snapshot().ClaudeSessionID)
				if paneHeader != "" || priorID == "" || priorID == strings.TrimSpace(payload.SessionID) {
					rotated, _ := pane.setClaudeSessionIDReportingPrior(payload.SessionID)
					changed := rotated || identityChanged
					// transcript_path is the authoritative, rotation-proof anchor
					// the conversation endpoint prefers. Empty on legacy clients —
					// SetClaudeTranscriptPath no-ops, leaving id-based resolution.
					if pane.SetClaudeTranscriptPath(payload.TranscriptPath) {
						changed = true
					}
					if changed && a.panes != nil {
						var persistErr error
						a.mgr.withCurrentPane(pane, func() { persistErr = a.panes.Put(pane) })
						if persistErr != nil {
							log.Printf("hook: persist claude session pane=%s session=%s: %v", paneID, shortSessionID(payload.SessionID), persistErr)
						}
					}
					// A session id that ROTATED to a different value (prior
					// non-empty) is the /clear, respawn or resume-elsewhere
					// signature: a brand-new conversation that knows nothing
					// about the work this pane already delegated. It earns a
					// one-shot digest of that work. The FIRST id a pane ever
					// learns is NOT a rotation — that is the same conversation
					// finally being recognized — and neither is a re-observation
					// of the id the pane already holds, which is what keeps a
					// reattach, a UI reload and a daemon restart silent.
					//
					// Not gated on HookEventName: SessionStart is the normal
					// carrier, but a rotation first seen on PreToolUse is the
					// same event and must not be missed. The re-pin gate above
					// already ensures only a header-attributed hook can get
					// here with a changed id, so a cwd-resolved stray `claude`
					// can never trigger it.
				}
			}
		}
	}
	// One log line per session so a bad attribution is traceable.
	logHookOnce(payload.SessionID, paneID, payload.CWD, paneHeader)

	// PreToolUse and SessionStart exist only to pin the pane's session id +
	// transcript path (done above) — neither is a completed tool invocation, so
	// they must NOT publish a tool-call Activity row or record an edited file.
	// SessionStart is what lets the daemon learn a freshly-started session's id
	// before its first tool call (see ensureClaudeHooksRegistered).
	if payload.HookEventName == "PreToolUse" || payload.HookEventName == "SessionStart" {
		// A permission menu, if this tool needs one, paints right after
		// PreToolUse returns. Arm the fast prompt wake for auto-mode panes.
		if payload.HookEventName == "PreToolUse" && paneID != "" {
			if pane, ok := a.mgr.Get(paneID); ok && pane.Snapshot().AutoMode != "" {
				pane.expectAutoPrompt()
			}
		}
		writeJSON(w, 200, map[string]any{})
		return
	}

	hint := redactHookHint(payload.ToolName, payload.ToolInput)

	a.uiBus.publish(uiEvent{
		Type:      "tool-call",
		Ts:        time.Now().UnixMilli(),
		Tool:      payload.ToolName,
		Hint:      hint,
		PaneID:    paneID,
		PaneTitle: paneTitle,
		// Cwd is carried forward so the UI can render it as a fallback
		// pane-location hint when attribution failed (header missing +
		// no pane cwd matched). Without this, "—" rows had no way to
		// diagnose why.
		Cwd: payload.CWD,
	})

	// Record file-editing tool calls (full local path) for the sidebar's
	// "agent just touched" list. Kept separate from the basename-redacted
	// activity feed above; the serving endpoint transport-gates secrets.
	if a.recentFiles != nil {
		if path, ok := hookEditedPath(payload.ToolName, payload.ToolInput, payload.CWD); ok {
			a.recentFiles.record(recentFileTouch{
				Path:      path,
				Name:      filepath.Base(path),
				Dir:       filepath.Dir(path),
				Tool:      payload.ToolName,
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
			})
		}
	}

	// Empty JSON = "allow the tool call, no further action". Claude
	// Code's hook protocol treats {} as a successful allow.
	writeJSON(w, 200, map[string]any{})
}

// attributeByCwd finds the running pane most likely to have produced
// this hook call, based on the `cwd` field in the payload. Matches
// against spawn Cwd first (deterministic, set at pane creation), then
// falls back to liveCwdOfPane (captures `cd` after spawn). If multiple
// panes match, picks the one with the most recent PTY activity — in
// practice the one the user just interacted with.
//
// Returns "" when no match — the Activity feed renders un-attributed
// tool calls as "—" rather than lying about which pane they came from.
func attributeByCwd(mgr *Manager, cwd string) string {
	if cwd == "" {
		return ""
	}
	want := canonCwd(cwd)

	// Two tiers. Exact match wins over prefix — if two panes could
	// both claim the call but one spawned in exactly Claude's cwd and
	// the other's cwd is a parent, the exact match is more specific.
	var exactBest *Pane
	var exactActivity int64
	// Prefix match: pane cwd is a parent of Claude's cwd. Keep the
	// LONGEST matching prefix (deepest pane), breaking ties by most-
	// recent PTY activity.
	var prefixBest *Pane
	var prefixLen int
	var prefixActivity int64

	for _, p := range mgr.List() {
		if p.Status != PaneRunning {
			continue
		}
		for _, paneCwd := range []string{canonCwd(p.Cwd), canonCwd(liveCwdOfPane(p))} {
			if paneCwd == "" {
				continue
			}
			if paneCwd == want {
				if exactBest == nil || p.LastActivityMs > exactActivity {
					exactBest = p
					exactActivity = p.LastActivityMs
				}
				continue
			}
			// Prefix check: Claude's cwd starts with pane cwd + "/".
			// Avoids "/foo/bar" being considered a parent of "/foo/barbaz".
			if strings.HasPrefix(want, paneCwd+"/") {
				if len(paneCwd) > prefixLen ||
					(len(paneCwd) == prefixLen && p.LastActivityMs > prefixActivity) {
					prefixBest = p
					prefixLen = len(paneCwd)
					prefixActivity = p.LastActivityMs
				}
			}
		}
	}

	if exactBest != nil {
		return exactBest.ID
	}
	if prefixBest != nil {
		return prefixBest.ID
	}
	return ""
}

// canonCwd normalizes a cwd for equality comparison: expands leading
// `~` to $HOME, strips trailing slashes. Does NOT symlink-resolve —
// that's a syscall per pane per hook and likely unnecessary for the
// common case where Claude reports the same style of path the pane
// was spawned with.
func canonCwd(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			p = home + p[1:]
		}
	}
	// Strip trailing slash (but keep root "/").
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

// logHookOnce — one log line per Claude session per attribution
// outcome, so we can tell after the fact whether the header path or
// the cwd fallback is doing the work (or neither). Guards against
// spamming the log on every tool call from a chatty session.
var hookLogged = struct {
	mu   sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

func logHookOnce(sessionID, resolvedPaneID, cwd, headerVal string) {
	if sessionID == "" {
		return
	}
	// HTTP hook handlers run concurrently, so seen[] is touched from
	// multiple goroutines — guard it. Hold the lock across the
	// check-then-set so two simultaneous first-calls for the same
	// session don't both log.
	hookLogged.mu.Lock()
	if _, ok := hookLogged.seen[sessionID]; ok {
		hookLogged.mu.Unlock()
		return
	}
	hookLogged.seen[sessionID] = struct{}{}
	hookLogged.mu.Unlock()
	source := "unresolved"
	if headerVal != "" {
		source = "header"
	} else if resolvedPaneID != "" {
		source = "cwd"
	}
	log.Printf("hook: session=%s attribution=%s pane=%s cwd=%s", shortSessionID(sessionID), source, resolvedPaneID, cwd)
}

func shortSessionID(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8]
}

// redactHookHint builds a short, display-safe label for the Activity
// feed from a tool_input object. Strategy per tool:
//
//   - file-path-bearing tools (Edit, Write, Read, NotebookEdit, Glob):
//     show basename of file_path / path
//   - Bash: show the first ~60 chars of the command UNLESS it looks
//     like a secret-leaking pattern (export, echo $, curl -H 'Auth…')
//   - Grep: show the pattern (capped)
//   - WebFetch / WebSearch: show the URL host or the query (capped)
//   - Task (sub-agent spawn): show the description
//   - TodoWrite: count of todos
//   - unknown: empty (safer default than dumping unknown shapes)
//
// The returned string is capped at 80 chars. Empty return is fine;
// the UI just renders `tool: <Name>` without a hint.
func redactHookHint(tool string, input map[string]any) string {
	get := func(k string) string { v, _ := input[k].(string); return v }
	basename := func(s string) string {
		if s == "" {
			return ""
		}
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == '/' || s[i] == '\\' {
				return s[i+1:]
			}
		}
		return s
	}
	truncate := func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	}

	var hint string
	switch tool {
	case "Edit", "MultiEdit", "Write", "Read", "NotebookEdit":
		// file_path is the canonical arg name Claude Code uses.
		hint = basename(get("file_path"))
	case "Glob":
		// Glob takes pattern + optional path. The pattern is more
		// useful than the directory ("**/*.ts" vs "/home/user").
		if pattern := get("pattern"); pattern != "" {
			hint = truncate(pattern, 60)
		} else {
			hint = basename(get("path"))
		}
	case "Grep":
		if pattern := get("pattern"); pattern != "" {
			hint = truncate(pattern, 60)
		}
	case "Bash":
		cmd := get("command")
		if looksLikeSecret(cmd) {
			// Don't echo commands that may be exporting / piping creds.
			// The event still fires — user sees "Bash" ran but not the
			// command line.
			hint = "(redacted)"
		} else if d := get("description"); d != "" {
			// Claude Code attaches a human one-line description to ~94% of
			// Bash calls ("Run the mobile build") — far more useful than the
			// raw, truncated command line (which often starts with VAR=val
			// scaffolding). Fall back to the command when absent.
			hint = truncate(d, 60)
		} else {
			hint = truncate(cmd, 60)
		}
	case "WebFetch":
		// URL first — path + query may encode request details we don't
		// want to log verbatim. Strip to host.
		if u := get("url"); u != "" {
			hint = hostOf(u)
		}
	case "WebSearch":
		if q := get("query"); q != "" {
			hint = truncate(q, 60)
		}
	case "Task", "Agent":
		// Sub-agent spawn. Claude Code's name for the tool varies by
		// version — older builds call it "Task", newer "Agent" — same
		// shape underneath. Prefer `description` (human-readable
		// purpose); fall back to `subagent_type` (which agent flavor)
		// or the first ~60 chars of `prompt` if present.
		if d := get("description"); d != "" {
			hint = truncate(d, 60)
		} else if a := get("subagent_type"); a != "" {
			hint = a
		} else if pr := get("prompt"); pr != "" {
			hint = truncate(pr, 60)
		}
	case "TodoWrite":
		if todos, ok := input["todos"].([]any); ok {
			hint = fmt.Sprintf("%d item%s", len(todos), plural(len(todos)))
		}
	}
	return truncate(hint, 80)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// looksLikeSecret tries to recognize command lines whose display would
// leak sensitive data. Conservative — prefer false-positives (redact
// more than needed) to false-negatives. Pattern-based, not exhaustive.
func looksLikeSecret(cmd string) bool {
	low := strings.ToLower(cmd)
	// `export FOO=bar`, `FOO=bar cmd`, `echo $SECRET`, curl with
	// Authorization header, base64 decoding of envs — all common
	// shapes for a credential interaction.
	patterns := []string{
		"export ",
		"api_key",
		"api-key",
		"secret",
		"password",
		"token=",
		"-h 'authorization",
		"--header 'authorization",
		"echo $",
		"printenv",
	}
	for _, pat := range patterns {
		if strings.Contains(low, pat) {
			return true
		}
	}
	return false
}

func hostOf(rawURL string) string {
	// Intentionally not importing net/url for this — we just want the
	// bit between `://` and the next `/`. Avoids allocator churn when
	// Claude fetches in a loop.
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return rawURL
	}
	host := rawURL[i+3:]
	if j := strings.IndexByte(host, '/'); j >= 0 {
		host = host[:j]
	}
	return host
}

// ensureClaudeHooksRegistered appends (or updates) SoluTerminal Claude Code
// hooks in ~/.claude/settings.json.
//
// PreToolUse lets us learn Claude's session id before the tool runs;
// PostToolUse keeps the Activity feed behavior that reports completed tool
// calls. Both POST to the local daemon's /api/hooks/tool-call. The
// `X-Soluterminal-Pane` header carries the SOLU_PANE_ID env var so the daemon
// can attribute the call to the right pane.
//
// SessionStart pins a session's id the instant it starts (not just on its first
// tool call), so a freshly (re)started interactive `claude` isn't invisible to
// the daemon — otherwise switching that pane to Focus resumes the STALE prior
// session. It MUST be a command hook (curl forwarding to the same endpoint):
// Claude Code does not fire http-type SessionStart hooks. See the inline note.
//
// Design constraints mirror ensureClaudeMcpRegistration's direct-write path:
//   - Idempotent — no-op if the registered entry already matches.
//   - Non-destructive — read, mutate, atomic-write; skip on parse error
//     rather than overwriting a corrupt file.
//   - Opt-out via SOLU_SKIP_HOOK_REGISTER=1.
//   - Creates ~/.claude/settings.json if missing (unlike the MCP flow
//     which requires an existing ~/.claude.json). settings.json is the
//     standard user config location and missing just means the user
//     hasn't customized anything yet — safe to create.
func ensureClaudeHooksRegistered(addr string) {
	requireNormalBootEngine("hooks.ensureClaudeHooksRegistered")
	if os.Getenv("SOLU_SKIP_HOOK_REGISTER") == "1" {
		return
	}
	// Same ownership rule as the MCP registration (shouldSelfRegister): the
	// hook URLs in ~/.claude/settings.json are machine-wide, so only the daemon
	// on a canonical port may write them. A side-port test/dev daemon used to
	// re-point every Claude session's hooks at itself — the production daemon
	// then stopped learning session ids until someone hand-edited the file.
	if !shouldSelfRegister(addr) {
		log.Printf("hooks: %s is not a canonical daemon port — leaving ~/.claude/settings.json hooks to the main daemon", addr)
		return
	}
	// Opt-in (agent_integration.go): without consent the hooks reach only
	// SoluTerminal's own panes, per process.
	if !agentIntegrationGlobalOn() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("hooks: mkdir %s: %v (skipping auto-register)", dir, err)
		return
	}
	path := filepath.Join(dir, "settings.json")

	// Read existing config if present; empty object otherwise.
	cfg := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Printf("hooks: could not parse %s: %v (skipping auto-register — not overwriting)", path, err)
			return
		}
	}

	// Build our hook definition. If the daemon listens on 0.0.0.0 or
	// [::], callbacks must still target an actual loopback name; the auth
	// gate intentionally rejects tokenless requests whose Host is a wildcard
	// bind address.
	hookURL := claudeHookURL(addr)
	specs := claudeHookSpecs(hookURL, "$SOLU_PANE_ID", "${SOLU_PANE_ID}")

	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		cfg["hooks"] = hooks
	}
	upsert := func(event string, want map[string]any) {
		entries, _ := hooks[event].([]any)
		replaced := false
		for i, entry := range entries {
			if claudeHookIsOurEntry(entry) {
				// Already registered. Update in place (URL drift across daemon
				// ports, or http→command migration for SessionStart).
				entries[i] = want
				replaced = true
				break
			}
		}
		if !replaced {
			entries = append(entries, want)
		}
		hooks[event] = entries
	}
	for _, event := range claudeHookEvents {
		upsert(event, specs[event])
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}

	// Atomic replace to avoid clobbering mid-write by another tool.
	tmp := path + ".solu-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		log.Printf("hooks: could not write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("hooks: could not replace %s: %v", path, err)
		return
	}
	log.Printf("hooks: registered Claude tool hooks → %s in %s", hookURL, path)
	log.Printf("hooks: set SOLU_SKIP_HOOK_REGISTER=1 to opt out")
}

// claudeHookEvents are the Claude Code events SoluTerminal hooks into.
var claudeHookEvents = []string{"PreToolUse", "PostToolUse", "SessionStart"}

func claudeHookURL(addr string) string {
	return fmt.Sprintf("http://%s/api/hooks/tool-call", claudeHookCallbackAddr(addr))
}

// claudeHookSpecs builds SoluTerminal's hook entry for each event. The global
// ~/.claude/settings.json form takes the pane id from the environment
// ("$SOLU_PANE_ID" / "${SOLU_PANE_ID}"); the per-pane --settings form can pass
// the literal id instead.
//
// Tool events (PreToolUse/PostToolUse) are HTTP callbacks carrying the pane id
// as a header. SessionStart is a COMMAND hook, NOT http: Claude Code (verified
// on 2.1.186) silently does not run http-type SessionStart hooks. Without a
// working SessionStart capture the daemon only learns a session's id on its
// FIRST tool call, so a freshly (re)started interactive `claude` that hasn't
// run a tool yet is invisible: its pane still carries the PRIOR session's id,
// and switching to Focus (or back to Terminal) resumes that stale conversation.
// The command forwards the hook's stdin JSON (session_id + transcript_path) to
// the same endpoint with the pane header. `|| true` keeps it non-blocking, and
// a missing curl degrades to first-tool-call capture.
func claudeHookSpecs(hookURL, headerPaneValue, commandPaneValue string) map[string]map[string]any {
	httpHook := map[string]any{
		"type":    "http",
		"url":     hookURL,
		"timeout": 2,
		"headers": map[string]any{
			"X-Soluterminal-Pane": headerPaneValue,
		},
	}
	if strings.Contains(headerPaneValue, "SOLU_PANE_ID") {
		httpHook["allowedEnvVars"] = []any{"SOLU_PANE_ID"}
	}
	// Empty matcher means every tool.
	httpEntry := map[string]any{
		"matcher": "",
		"hooks":   []any{httpHook},
	}
	sessionStartCmd := fmt.Sprintf(
		`curl -sf -m 2 -X POST -H "X-Soluterminal-Pane: %s" -H "Content-Type: application/json" --data-binary @- %s >/dev/null 2>&1 || true`,
		commandPaneValue, hookURL,
	)
	commandEntry := map[string]any{
		"matcher": "",
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": sessionStartCmd,
		}},
	}
	return map[string]map[string]any{
		"PreToolUse":   httpEntry,
		"PostToolUse":  httpEntry,
		"SessionStart": commandEntry,
	}
}

// claudeHookIsOurEntry reports whether a hooks[event] entry is one SoluTerminal
// wrote — matched by EITHER an http url OR a command that references our
// endpoint path. Checking both lets re-registration replace a legacy http
// SessionStart entry (from older daemons) with the command form, and update
// the URL in place across daemon-port drift, without ever duplicating. Removal
// uses the same test, so it never touches another tool's hooks.
func claudeHookIsOurEntry(entry any) bool {
	em, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := em["hooks"].([]any)
	for _, h := range hs {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if u, _ := hm["url"].(string); strings.Contains(u, "/api/hooks/tool-call") {
			return true
		}
		if c, _ := hm["command"].(string); strings.Contains(c, "/api/hooks/tool-call") {
			return true
		}
	}
	return false
}

func claudeHookCallbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("127.0.0.1", port)
	default:
		return net.JoinHostPort(host, port)
	}
}

// hookAccountRetry rate-limits the UserPromptSubmit account fill for sessions
// that stay unattributable (no saved account, no OAuth email): without it every
// prompt would re-read the multi-megabyte .claude.json on the synchronous hook
// path. One attempt per session per hookAccountRetryInterval.
var hookAccountRetry = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

const hookAccountRetryInterval = 10 * time.Minute

func hookAccountRetryDue(sessionID string, now time.Time) bool {
	hookAccountRetry.Lock()
	defer hookAccountRetry.Unlock()
	if t, ok := hookAccountRetry.last[sessionID]; ok && now.Sub(t) < hookAccountRetryInterval {
		return false
	}
	if len(hookAccountRetry.last) >= 4096 {
		for k, t := range hookAccountRetry.last {
			if now.Sub(t) >= hookAccountRetryInterval {
				delete(hookAccountRetry.last, k)
			}
		}
		if len(hookAccountRetry.last) >= 4096 {
			hookAccountRetry.last = map[string]time.Time{}
		}
	}
	hookAccountRetry.last[sessionID] = now
	return true
}
