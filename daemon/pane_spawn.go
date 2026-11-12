package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hinshun/vt10x"

	"soluterminal/grid"
)

type SpawnOpts struct {
	workerAccount string // internal auto-worker account pin; never accepted from HTTP
	workerTeamID  string
	// Internal resume timestamp, installed before the pane is published.
	CreatedAt time.Time
	// Restored before publication: security settings must never lag the live pane.
	AutoMode        string
	NeverAutoAnswer bool

	launchRouteTeamID string // nonsecret scope captured with the minted route
	launchRouteLabel  string
	// The request capability is minted for this spawn. It is
	// never persisted; Spawn revokes it if the pane does not start.
	launchRouteCapability string
	// Internal provenance only; never accepted from a pane creation request.
	AutomationOrigin string

	ID                   string
	Cwd                  string
	Cmd                  []string
	Title                string
	Cols                 uint16
	Rows                 uint16
	Env                  []string
	ClaudeSessionID      string
	ClaudeTranscriptPath string
	TitleOverride        string
	// Origin is who or what started the pane and why (pane_origin.go).
	Origin *PaneOrigin
	// Review carries the latest solu-review result across a respawn.
	Review *PaneReview
	// Identity carries the pane's handle and working-on line across a respawn.
	Identity *PaneIdentity
	// Kind selects the pane's rendering model: "" (terminal, default) or
	// "native". When "native", the pane runs a headless agent emitting
	// stream-json and the daemon starts a native parser goroutine. The
	// native headless-argv rewrite of opts.Cmd is the spawn component's job;
	// this field only gates ensureNative + p.Kind.
	Kind string
	// Agent identifies the agent protocol independently from the renderer Kind.
	// "" / "claude" is the legacy Claude default; "codex" selects the durable
	// app-server protocol identity for Terminal and Focus panes.
	Agent string
	// CodexThreadID is the durable Codex app-server thread identifier. It is
	// independent from ClaudeSessionID so Claude transcript/resume guards never
	// inspect a Codex rollout as if it were a Claude transcript.
	CodexThreadID string
	// MuseSessionID is the durable Muse session UUID, independent from both
	// ids above for the same reason. Native Muse panes always carry one (the
	// daemon mints it); terminal panes only via a `muse resume <id>` argv.
	MuseSessionID string
	// HarnessSessionID/HarnessProvider carry a managed terminal harness's
	// durable conversation pointer into a resume spawn (see Pane fields). Spawn
	// copies them onto the pane; the resume paths put the id on the child env
	// as SOLU_HARNESS_RESUME_SESSION.
	HarnessSessionID string
	HarnessProvider  string
	// CodexTurnInFlight carries the dead pane's durable "a turn was underway"
	// record into its RESUME (see Pane.codexTurnInFlight), which Spawn turns
	// into the "a restart interrupted the previous response" notice. Set only
	// by the resume paths — a mode switch settles or interrupts its own turn
	// and stamps its own, more accurate notice.
	CodexTurnInFlight bool
	// NativeAllowlist seeds the pane's "always allow this tool" set on spawn.
	// Used by switch-mode + rollback to carry the user's opt-ins across a
	// kill+respawn that continues the SAME conversation (claude --resume), so
	// a Terminal↔Native flip doesn't silently drop the allowlist. Empty for a
	// brand-new pane (a genuinely new session starts with no opt-ins).
	NativeAllowlist []string
	// Provider routes the spawned `claude` to an LLM backend: "" / "anthropic"
	// = Anthropic (default, byte-identical to today); "glm" = Z.ai. Carries the
	// NAME, not a key. When "glm", the daemon passes `--provider glm` to the
	// agent (which resolves the token from its universal --data-dir flag onto the
	// child's c.Env) and injects a NON-SECRET unset+export prefix into the
	// login-shell wrap. The token NEVER rides this struct, opts.Env, the agent
	// argv, or any log.
	Provider string
	// Effort is the reasoning-effort level passed to `claude` as --effort
	// (one of "" | "low" | "medium" | "high" | "xhigh" | "max"). "" means
	// "don't pass the flag" — claude uses its built-in default. It is
	// PROVIDER-AGNOSTIC: the flag rides the API request, so Anthropic uses its
	// native levels and a GLM (Z.ai) backend maps them to its own tiers
	// (high-and-below → GLM High, xhigh/max → GLM Max). For native/Focus panes
	// this is the ONLY way to set effort (the interactive /effort slash command
	// is TUI-only and never reaches a headless `claude -p`). Baked into the argv
	// by nativeClaudeArgv, so it is re-derived on every (re)spawn — must survive
	// in the snapshot/persisted shape across switch/resume/restart.
	Effort string
	// Model is the Claude model id passed to `claude` as --model (e.g.
	// "claude-fable-5-1"). "" means "don't pass the flag". CRITICAL RESUME
	// SEMANTICS (verified empirically on claude 2.1.205, 2026-07-09): `--resume`
	// carries the RESUMED SESSION'S stored model, overriding the settings.json
	// default — so a bare respawn/switch/restart perpetuates whatever model the
	// session last ran, and a settings change is never picked up. Passing --model
	// once REWRITES the session's stored model (subsequent bare resumes keep it).
	// Same TUI-only story as Effort: headless `claude -p` has no /model command,
	// so this is the ONLY model control for Focus panes (POST /api/panes/{id}/model).
	Model string
	// Role is the pane's product role: "" (plain) or paneRoleOrchestrator (the
	// chief-of-staff pane that delegates buildable work to workflow tasks and
	// schedules). Only meaningful for a supported orchestrator host. Its adapter
	// re-derives the provider-specific instruction transport on every spawn, so
	// it must round-trip across switch/resume/restart.
	Role string
	// ContextPack is the resolved absolute path of the local briefing pack for
	// this pane, or "" when detached. Like Role, its argv/env adapter is
	// re-derived on EVERY (re)spawn, so it must round-trip through pane state and
	// workspace inline definitions.
	ContextPack string
	// DimsUnverified, when true, spawns the pane with dimsVerified=false: the
	// PTY IS sized to opts.Cols/Rows, but those dims are a persisted/guessed
	// width, not a live client measurement, so PaneSnapshot flags them and the
	// first client attach re-measures and drives the real width (mirrors
	// AttachExisting). The resume paths set this when they fall back to
	// panes.json dims — otherwise Spawn would launder a stale width into
	// "verified" and reproduce the every-update garble on any pane whose live
	// window differs from its last-persisted size. Default false: a normal
	// new-pane Spawn sized the PTY from the live client, so its dims are real.
	DimsUnverified bool
	// Fleet replacement pins the installed solud instead of reusing a stale
	// daemon inode, and refuses any fallback to a fresh conversation.
	AgentExecutable              string
	RequireResumableConversation bool
	// Keep a suspended reboot tile published until its replacement is ready.
	ReplaceSuspended *Pane
}

// SpawnError marks user-facing validation errors (wrong cwd, bad command).
// The HTTP layer maps these to 400 instead of 500.
type SpawnError struct{ msg string }

func (e *SpawnError) Error() string { return e.msg }

// nativeClaudeArgv builds the headless Claude argv for a native-renderer
// pane. The flags are byte-grounded in the Phase-0 capture (PHASE0-REPORT):
// the agent runs `-p` (headless / non-interactive), emits stream-json on
// stdout, accepts stream-json on stdin, and answers permission prompts over
// stdio. BOTH `--permission-mode default` AND `--permission-prompt-tool
// stdio` are load-bearing: `default` alone auto-executes tools without ever
// surfacing a can_use_tool control_request. `--include-partial-messages`
// yields the streaming content_block_delta frames, `--replay-user-messages`
// echoes user turns (deduped client-side), and `--verbose` is required for
// the full event vocabulary. `--effort` bakes the reasoning-effort level into
// the argv (headless `claude -p` has no /effort command; re-derived on every
// respawn from the pane's persisted Effort, provider-agnostic). `--prompt-
// suggestions` (when promptSuggestions is true) makes Claude emit its own
// model-generated next-prompt suggestions as `prompt_suggestion` lines (a
// background call ~10-20s after `result`, best-effort — see EvPromptSuggestion),
// gated on the per-machine native setting (Manager.promptSuggestionsEnabled)
// read fresh at every (re)spawn, so OFF by default. `--resume <id>` is appended
// on a session switch/restore so the same conversation continues.
//
// This is the role-less wrapper kept for the many callers that never carry one;
// nativeClaudeArgvRole is the full form.
func nativeClaudeArgv(sessionID string, narrate bool, effort string, promptSuggestions bool, model string) []string {
	return nativeClaudeArgvRole(sessionID, narrate, effort, promptSuggestions, model, "")
}

// nativeClaudeArgvRole is nativeClaudeArgv plus the pane's product role: an
// orchestrator pane's brief is appended to the SAME --append-system-prompt value
// as the narration prompt (claude takes one flag — see systemPromptAppend).
func nativeClaudeArgvRole(sessionID string, narrate bool, effort string, promptSuggestions bool, model string, role string) []string {
	argv := []string{
		"claude", "-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-mode", "default",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--replay-user-messages",
		"--verbose",
	}
	// Narration (the "vibe coding" generation lever): APPEND a "narrate your
	// decisions" instruction to Claude's built-in system prompt. --append-system-
	// prompt is additive — tools and built-in behavior are preserved (verified
	// against claude 2.1.185 in this exact -p stream-json mode). Off by default;
	// gated on the per-machine native setting, read fresh at every (re)spawn.
	// An orchestrator pane's chief-of-staff brief rides the SAME flag, appended
	// after the narration prompt (claude accepts one --append-system-prompt).
	if sp := systemPromptAppend(narrate, role); sp != "" {
		argv = append(argv, "--append-system-prompt", sp)
	}
	// Reasoning effort. Headless `claude -p` has no /effort command, so the
	// level MUST be baked into the argv here; re-derived on every respawn from
	// the pane's persisted Effort. Provider-agnostic: the flag rides the API
	// request to whichever backend (Anthropic or a GLM endpoint). "" → omit so
	// claude keeps its built-in default. Validated at the API boundary
	// (createPane / handleSetEffort), so this just trusts a non-empty value.
	if e := strings.TrimSpace(effort); e != "" {
		argv = append(argv, "--effort", e)
	}
	if promptSuggestions {
		argv = append(argv, "--prompt-suggestions")
	}
	// Model. Like effort, headless claude has no /model command so the choice is
	// argv-only. Note --resume (below) makes the session's stored model sticky:
	// omitting --model resumes on whatever the session last ran (NOT the settings
	// default), and passing it rewrites the stored model for future resumes.
	if m := strings.TrimSpace(model); m != "" {
		argv = append(argv, "--model", m)
	}
	if s := strings.TrimSpace(sessionID); s != "" {
		argv = append(argv, "--resume", s)
	}
	return argv
}

// nativeClaudeSpawnArgv is the Spawn-side seam over nativeClaudeArgvRole: it
// reads session / effort / model / ROLE off the SpawnOpts the caller already
// built, so a test can pin that a Focus orchestrator pane's argv really carries
// the brief (and that a role-less one is byte-identical to nativeClaudeArgv)
// without launching a process. narrate and promptSuggestions stay explicit —
// Spawn derives them from Manager state, not from opts.
func nativeClaudeSpawnArgv(opts SpawnOpts, narrate, promptSuggestions bool) []string {
	return nativeClaudeArgvRole(opts.ClaudeSessionID, narrate, opts.Effort, promptSuggestions, opts.Model, opts.Role)
}

// nativeCodexArgv starts Codex's stateful JSON-RPC app-server over stdio. The
// daemon owns initialize/thread/turn messages through CodexAdapter; using
// `codex exec --json` here would create a one-turn process and cannot support
// interactive approvals, interrupts, steering, or a persistent Focus pane.
func nativeCodexArgv() []string {
	return []string{"codex", "app-server", "--stdio"}
}

// claudeTranscriptOnDisk reports whether sessionID's main transcript exists on
// disk, via a single os.Stat — NOT a listTranscripts directory walk. It runs on
// the /api/panes poll path (resumeReady) and at every Spawn, so it must stay
// O(1): walking ~/.claude/projects (thousands of files) per pane per poll would
// recreate the computeState GC death-spiral. Prefers the captured transcript path
// when present; otherwise stats the conventional
// <configDir>/projects/<enc-cwd>/<id>.jsonl.
//
// A negative from this is NOT proof of absence — see claudeTranscriptResumable
// for that, and use it instead anywhere a wrong "no" costs the user something.
// The cwd encoding is imitation (claudeProjectDirName), so a miss can equally
// mean "not there" or "not where we guessed".
func claudeTranscriptOnDisk(cwd, sessionID, knownPath string) bool {
	if p := strings.TrimSpace(knownPath); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return true
		}
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	enc := claudeProjectDirName(expandHome(strings.TrimSpace(cwd)))
	for _, dir := range claudeConfigDirs() {
		if fi, err := os.Stat(filepath.Join(dir, "projects", enc, sessionID+".jsonl")); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// claudeTranscriptResumable answers the question claudeTranscriptOnDisk only
// approximates: is this conversation really gone? Use it wherever a wrong "no"
// costs the user something — Spawn erases the session id on a "no", and the
// model/effort/refresh handlers refuse the request outright.
//
// Verdict semantics are deliberately asymmetric, the same contract
// codexRolloutOnDisk spells out and for the same reason. "No" has to mean "we
// enumerated the transcripts and it genuinely isn't there", never "we couldn't
// find it where we guessed" — that distinction is the whole function. A tree
// that exists but can't be read fails OPEN and lets the resume proceed; at worst
// claude rejects the id and says so, which beats silently starting over on top
// of a long conversation. A tree that isn't there at all is a different answer:
// nothing was ever written, so absence is real.
//
// The imitated encoding is why this exists. On Windows claudeProjectDirName was
// POSIX-only, so the cwd lookup could never match: every native pane whose
// ClaudeTranscriptPath was unset (i.e. whose hooks never reached us) was judged
// unresumable, which silently reset a user's ~736k-token session and made
// "change model"/"change effort" a permanent 409. The encoder is fixed, but it
// is still a guess — so the destructive paths no longer trust it alone.
//
// Costs a directory walk on the miss path, which is why the /api/panes poll
// (the resumeReady badge) must keep calling the cheap stat above instead: a
// walk per pane per poll is the computeState GC death-spiral.
func claudeTranscriptResumable(cwd, sessionID, knownPath string) bool {
	return claudeTranscriptResumableVia(cwd, sessionID, knownPath, claudeTranscriptByID)
}

func claudeTranscriptResumableVia(cwd, sessionID, knownPath string, byID func(dirs []string, sessionID string) (found, enumerated bool)) bool {
	if claudeTranscriptOnDisk(cwd, sessionID, knownPath) {
		return true
	}
	if strings.TrimSpace(sessionID) == "" {
		// No id to resume: nothing to abandon, and nothing a search could find.
		return false
	}
	dirs := claudeConfigDirs()
	if len(dirs) == 0 {
		// Not blindness — absence. claudeConfigDirs only returns config dirs
		// that HAVE a projects tree, so an empty result means claude has never
		// written a transcript on this machine at all. That's the virgin-session
		// case (a Focus pane flipped before its first turn), which must start
		// fresh rather than fake a resume against a session that never existed.
		return false
	}
	// Session ids are UUIDs and unique across project dirs, so look the file up
	// by NAME rather than reproducing Claude Code's cwd encoding.
	found, enumerated := byID(dirs, strings.TrimSpace(sessionID))
	if found {
		return true
	}
	return !enumerated
}

// claudeTranscriptByID scans the projects trees for "<sessionID>.jsonl".
// Reports whether it found the file, and whether it managed a clean enumeration
// of at least one projects root — the caller needs the second to tell "isn't
// there" apart from "couldn't look".
func claudeTranscriptByID(dirs []string, sessionID string) (found, enumerated bool) {
	target := sessionID + ".jsonl"
	for _, d := range dirs {
		root := filepath.Join(d, "projects")
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		clean := true
		_ = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable subtree means this enumeration is no longer
				// exhaustive, so it can't be used to prove absence.
				clean = false
				if de != nil && de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if de.IsDir() {
				return nil
			}
			if de.Name() == target && !isSubagentTranscript(path) {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if found {
			return true, true
		}
		if clean {
			enumerated = true
		}
	}
	return false, enumerated
}

// spawnResumeID returns the provider-specific conversation id this Spawn would
// resume. Native panes carry the id explicitly; terminal panes may carry it in
// their interactive CLI resume argv.
func spawnResumeID(opts SpawnOpts) string {
	switch effectiveAgentKind(opts.Agent) {
	case agentCodex:
		if id := strings.TrimSpace(opts.CodexThreadID); id != "" {
			return id
		}
		return resumeThreadIDFromCodexCmd(opts.Cmd)
	case agentMuse:
		if id := strings.TrimSpace(opts.MuseSessionID); id != "" {
			return id
		}
		return resumeSessionIDFromMuseCmd(opts.Cmd)
	}
	if opts.Kind == "native" {
		return strings.TrimSpace(opts.ClaudeSessionID)
	}
	return resumeSessionIDFromCmd(opts.Cmd)
}

// runningSessionOwner returns the id of a RUNNING pane (other than excludeID)
// whose provider-specific conversation id equals sid, or "" when no live pane owns it.
// Suspended/exited panes never count as owners: a session is only "taken" while
// a live agent process is attached to it, so resuming one of two suspended
// twins stays legitimate.
func (m *Manager) runningSessionOwner(agent, sid, excludeID string) string {
	agent = effectiveAgentKind(agent)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return ""
	}
	for _, p := range m.List() {
		if p.ID == excludeID {
			continue
		}
		snap := p.Snapshot()
		if snap.Status == PaneRunning && effectiveAgentKind(snap.Agent) == agent &&
			conversationIDForAgent(snap.Agent, snap.ClaudeSessionID, snap.CodexThreadID, snap.MuseSessionID) == sid {
			return p.ID
		}
	}
	return ""
}

// resumeThreadIDFromCodexCmd extracts the thread id from the stable interactive
// `codex resume <thread-id>` shape — BOTH the bare argv and the login-shell-
// wrapped single-quoted form (`… exec 'codex' 'resume' '<id>'` inside one
// bash -lc string), mirroring resumeSessionIDFromCmd for Claude. The wrapped
// hit is accepted only when it parses as a canonical thread id, keeping the
// argv path as conservative as /proc discovery. Native app-server panes carry
// CodexThreadID directly and do not depend on argv parsing.
func resumeThreadIDFromCodexCmd(cmd []string) string {
	if len(cmd) >= 3 && cmdStartsCodex(cmd) && strings.EqualFold(strings.TrimSpace(cmd[1]), "resume") {
		return strings.TrimSpace(cmd[2])
	}
	joined := strings.Join(cmd, " ")
	const marker = "'codex' 'resume' '"
	if idx := strings.Index(joined, marker); idx >= 0 {
		rest := joined[idx+len(marker):]
		if end := strings.IndexByte(rest, '\''); end >= 0 {
			if id, ok := canonicalCodexThreadID(rest[:end]); ok {
				return id
			}
		}
	}
	return ""
}

// resumeSessionIDFromMuseCmd extracts the session id from the interactive
// `muse resume <session-uuid>` argv shape, mirroring resumeThreadIDFromCodexCmd.
// Only a canonical UUID counts — `muse resume` (the picker) and `muse resume
// --last` carry no durable identity. Native Muse panes carry MuseSessionID
// directly and do not depend on argv parsing.
func resumeSessionIDFromMuseCmd(cmd []string) string {
	if len(cmd) >= 3 && cmdStartsMuse(cmd) && strings.EqualFold(strings.TrimSpace(cmd[1]), "resume") {
		if id, ok := canonicalMuseSessionID(cmd[2]); ok {
			return id
		}
	}
	joined := strings.Join(cmd, " ")
	const marker = "'muse' 'resume' '"
	if idx := strings.Index(joined, marker); idx >= 0 {
		rest := joined[idx+len(marker):]
		if end := strings.IndexByte(rest, '\''); end >= 0 {
			if id, ok := canonicalMuseSessionID(rest[:end]); ok {
				return id
			}
		}
	}
	return ""
}

// clearSpawnResume drops a provider-specific resume target and turns the
// request into a fresh invocation. Called after the duplicate-owner guard has
// positively matched another live pane, and after a resumability gate
// (transcript / rollout on disk) has ruled the target unloadable.
func clearSpawnResume(opts *SpawnOpts) {
	if effectiveAgentKind(opts.Agent) == agentCodex {
		opts.CodexThreadID = ""
		if opts.Kind != "native" {
			opts.Cmd = stripCodexResumeArgv(opts.Cmd)
		}
		return
	}
	if effectiveAgentKind(opts.Agent) == agentMuse {
		opts.MuseSessionID = ""
		if opts.Kind != "native" {
			opts.Cmd = stripMuseResumeArgv(opts.Cmd)
		}
		return
	}
	opts.ClaudeSessionID = ""
	opts.ClaudeTranscriptPath = ""
	if opts.Kind != "native" {
		opts.Cmd = stripResumeArgv(opts.Cmd)
	}
}

// stripResumeArgv removes the `--resume <id>` pair from a command, leaving a
// fresh invocation — BOTH the bare argv form (["claude","--resume",id] →
// ["claude"]) AND the login-shell-wrapped form, where the pair lives INSIDE one
// bash -lc string element (`… 'claude' '--resume' '<id>'; exec bash -i`).
// Workspace inline defs and persisted panes carry the WRAPPED form; stripping
// only the bare form let a fresh-start downgrade launch the doomed `--resume`
// anyway, falling through to the dead-bash masquerade the transcript guard
// exists to prevent.
func stripResumeArgv(cmd []string) []string {
	out := make([]string, 0, len(cmd))
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == "--resume" && i+1 < len(cmd) {
			i++ // skip the id too
			continue
		}
		out = append(out, stripWrappedQuotedPair(cmd[i], "'--resume'"))
	}
	return out
}

// stripCodexResumeArgv is stripResumeArgv's Codex counterpart: it removes the
// interactive resume target from BOTH the bare argv (["codex","resume",id] →
// ["codex"]) AND the login-shell-wrapped form (`… exec 'codex' 'resume'
// '<id>'` → `… exec 'codex'`), leaving a fresh TUI invocation.
func stripCodexResumeArgv(cmd []string) []string {
	if len(cmd) >= 3 && cmdStartsCodex(cmd) && strings.EqualFold(strings.TrimSpace(cmd[1]), "resume") {
		return cloneStringSlice(cmd[:1])
	}
	out := make([]string, 0, len(cmd))
	for _, el := range cmd {
		if strings.Contains(el, "'codex'") {
			el = stripWrappedQuotedPair(el, "'resume'")
		}
		out = append(out, el)
	}
	return out
}

// stripMuseResumeArgv is the Muse counterpart: `muse resume <id>` → `muse`.
func stripMuseResumeArgv(cmd []string) []string {
	if len(cmd) >= 3 && cmdStartsMuse(cmd) && strings.EqualFold(strings.TrimSpace(cmd[1]), "resume") {
		return cloneStringSlice(cmd[:1])
	}
	out := make([]string, 0, len(cmd))
	for _, el := range cmd {
		if strings.Contains(el, "'muse'") {
			el = stripWrappedQuotedPair(el, "'resume'")
		}
		out = append(out, el)
	}
	return out
}

// stripWrappedQuotedPair removes one ` <flag> '<value>'` occurrence from a
// login-shell-wrapped command string element (the single-quoted shape
// tryLoginShellWrap builds), returning the element unchanged when the flag or
// its closing quote is absent.
func stripWrappedQuotedPair(el, flag string) string {
	marker := flag + " '"
	idx := strings.Index(el, marker)
	if idx < 0 {
		return el
	}
	rest := el[idx+len(marker):]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		return el
	}
	return strings.TrimRight(el[:idx], " ") + rest[end+1:]
}

// nativeNarrationPrompt is appended to the headless agent's system prompt when
// native narration is enabled. It asks the agent to surface the why/decisions a
// "vibe coder" cares about while staying TERSE: HARD rules that apply to EVERY
// reply (including direct answers like "summarize the session"). It controls SHAPE
// + ORDER — lead lean, plain formatting, no bold-label bullets, most-important-
// first — and crucially does NOT tell the agent to WITHHOLD detail: the client
// Focus prose fold (splitLedeBlocks / FoldableProse) collapses the lower-priority
// TAIL behind a one-click, lossless "show more", so the agent writes the COMPLETE
// answer front-loaded and the UI (not a re-ask) is the expansion path. An earlier
// version told the agent to offer "want the full rundown?" instead of answering —
// that conflicts with the fold (a stub with nothing behind it), so it's removed.
// NOTE: a --resume'd pane carries in-context style momentum, so a tightened prompt
// lands cleanest on a FRESH native pane; the fold works regardless of pane age.
const nativeNarrationPrompt = `You are working with a "vibe coder" who wants the decision and the outcome fast — not a write-up. They care about the why, the plan, and the trade-offs, and treat code, diffs, and command output as supporting detail. Optimize every reply to be skimmed in seconds.

Hard rules — these override your usual formatting and length instincts:
- NEVER use markdown headings, section titles, horizontal rules, or bold "Label:" lead-ins, and NEVER structure a reply like a document, report, or multi-section "summary". This applies to EVERY reply, including direct answers to questions like "what did you do?" or "summarize the session" — answer those the same terse way, never as an essay.
- Do NOT bold words for emphasis, and do NOT open a bullet with a bolded term followed by a dash. Write each bullet as a plain sentence — not "- **Go ~72k** — the backend", but "- Go is ~72k lines, the backend and the riskiest layer". Plain prose and plain bullets only.
- Lead with the decision or outcome in the FIRST sentence. No preamble, no restating the request, no "Great question".
- Order by importance and keep the TOP lean: the decision plus the one or two facts that matter most come first, in at most 3 short sentences OR a tight list of at most 5 one-line bullets. Any genuinely secondary detail (caveats, lesser risks, supporting numbers) goes AFTER that lean top.
- Give the COMPLETE answer — do NOT withhold substance and tell the user to ask again. The interface folds your lower-priority tail behind a one-click "show more", so write the full answer; just lead with what matters most. Only offer to expand when it would be genuinely NEW work (e.g. "want a full file-by-file walkthrough?"), never for detail you could simply include.
- Narrate intent and trade-offs, not keystrokes or individual tool calls. Skip routine narration ("now I'll read the file", "next I'll run the tests") — just do it. Don't restate what the code, diff, or command output already shows.
- Prefer few, decisive actions. Batch related shell commands into one call ("a && b && c") instead of many sequential ones, and don't spin up long chains of thinking when a quick decision will do.

This is in addition to your normal work: keep the engineering correct and complete. Only the prose around it gets terse — lead with what matters, plain formatting, no document scaffolding.`

// isHeadlessClaudeCmd reports whether cmd is (or wraps) the native headless
// Claude invocation — `claude -p --output-format stream-json --input-format
// stream-json …`. It must recognize BOTH the bare argv AND the login-shell-
// wrapped form (`bash -lc '… claude … stream-json …'`, built by
// tryLoginShellWrap when claude isn't on the daemon's PATH), so it scans the
// joined command for the three markers unique to the native stream-json mode
// (input-format, output-format, stream-json). A terminal `claude` /
// `claude --resume <id>` TUI carries none of them, and no ordinary interactive
// command carries all three, so this never false-positives a real shell/TUI
// command into pipe mode. Used to (a) force the agent's --no-pty flag and
// (b) heal a native pane whose Kind was lost — so a native pane can never run
// claude -p under a PTY, which crashes with "Input must be provided … --print".
func isHeadlessClaudeCmd(cmd []string) bool {
	if len(cmd) == 0 {
		return false
	}
	joined := strings.Join(cmd, " ")
	return strings.Contains(joined, "input-format") &&
		strings.Contains(joined, "output-format") &&
		strings.Contains(joined, "stream-json")
}

// healLostNativeKind repairs a persisted pane whose native Kind was lost: Kind
// reads "" (terminal) but its Cmd is the headless `claude -p … stream-json`
// argv. This corruption was written by an older daemon's resume path that reused
// the persisted native command without carrying Kind through; on the next
// restart such a pane would respawn as a terminal pane, run claude -p under a
// PTY, crash with "Input must be provided … --print", and drop the Focus pane to
// a dead bash shell. Returns the pane with Kind forced back to "native" and
// ok=true when it healed (so the restart loop can resume it as a proper Focus
// pane); a pane already tagged native, or any non-native command, is returned
// unchanged with ok=false.
func healLostNativeKind(info PersistedPane) (PersistedPane, bool) {
	if info.Kind == "" && isHeadlessClaudeCmd(info.Cmd) {
		info.Kind = "native"
		return info, true
	}
	return info, false
}

// resumeSessionIDFromCmd extracts the <id> from a `--resume <id>` in cmd. Used
// to recover a native pane's Claude session when its persisted ClaudeSessionID
// field was cleared by the corruption but the id is still baked into the resume
// argv, so the conversation can still resume instead of the pane being dropped.
// Handles BOTH the bare argv (["…","--resume","<id>",…]) and the login-shell-
// wrapped single-quoted form (`… '--resume' '<id>' …` inside one bash -lc
// string). Returns "" when there is no --resume.
func resumeSessionIDFromCmd(cmd []string) string {
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == "--resume" {
			return strings.TrimSpace(cmd[i+1])
		}
	}
	joined := strings.Join(cmd, " ")
	const marker = "'--resume' '"
	if idx := strings.Index(joined, marker); idx >= 0 {
		rest := joined[idx+len(marker):]
		if end := strings.IndexByte(rest, '\''); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

// isStaleNonPtyNativeAgent reports whether pid is OUR live agent for socketPath
// but was spawned WITHOUT --no-pty — the broken pre-fix native agent (it ran the
// headless claude -p under a PTY, which crashes with "Input must be provided …
// --print" and falls back to a zombie `bash -i`). It verifies identity from
// /proc/<pid>/cmdline — the process must be a `solud agent` bound to THIS pane's
// socket — so a recycled or unrelated pid is never matched, and the restart
// loop's terminatePid can never kill the wrong process. Linux-only; elsewhere it
// returns false (we can't introspect /proc, so leave the agent alone and let the
// normal socket-dial reattach decide).
func isStaleNonPtyNativeAgent(pid int, socketPath string) bool {
	if pid <= 0 || socketPath == "" || runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := bytes.Split(raw, []byte{0})
	var isAgent, ownsSocket, hasNoPty bool
	for i, a := range args {
		switch string(a) {
		case "agent":
			isAgent = true
		case "--no-pty":
			hasNoPty = true
		case "--socket":
			if i+1 < len(args) && string(args[i+1]) == socketPath {
				ownsSocket = true
			}
		}
	}
	return isAgent && ownsSocket && !hasNoPty
}

// startSupervisedAgentChild starts one daemon-owned `solud agent` and keeps its
// exec.Cmd alive until the child terminates. Setsid detaches the agent from the
// daemon's session and process group, but it does not change parentage: while
// this daemon is alive it must still wait on the child or the kernel retains a
// zombie process-table entry. A dedicated Cmd.Wait is deliberately narrower
// than a process-wide wait4(-1), which could steal the exit status from an
// unrelated exec.Cmd elsewhere in the daemon.
//
// The wrapper and every descendant inherit the child-only soft=1 guard;
// neither the daemon's limit nor the hard limit is changed.
//
// The returned channel closes after Wait has reaped the child. Spawn watches it
// to fail fast when an agent dies before listening; tests use it to verify the
// boot-recovery kill path.
func startSupervisedAgentChild(cmd *exec.Cmd) (<-chan struct{}, error) {
	if err := startWithoutCoreDumps(cmd); err != nil {
		return nil, err
	}
	reaped := make(chan struct{})
	go func() {
		_ = waitOwnedCommand(cmd)
		close(reaped)
	}()
	return reaped, nil
}

// Spawn forks a new agent process and attaches to it.
func (m *Manager) Spawn(opts SpawnOpts) (*Pane, error) {
	var release func()
	if opts.Origin != nil && workerHoldsCredentials(opts.Origin.Worker) && opts.workerAccount == "" {
		prepare := workerCredentialSlot.ensure(&m.ext).prepare
		if prepare == nil {
			return nil, &SpawnError{msg: "worker account recovery is not ready; resume after daemon startup"}
		}
		opts.Origin = opts.Origin.clone()
		var err error
		release, err = prepare(&opts)
		if err != nil {
			return nil, err
		}
	}
	p, err := m.spawnPreparedPane(opts)
	if release != nil {
		if err != nil {
			release()
		} else {
			p.mu.Lock()
			p.quotaWorkerCleanupOwned = true
			done := p.done
			p.mu.Unlock()
			go func() { <-done; release() }()
		}
	}
	return p, err
}

func (m *Manager) spawnPreparedPane(opts SpawnOpts) (*Pane, error) {
	_, ok := normalizeAgentKind(opts.Agent, opts.Cmd, opts.CodexThreadID)
	if !ok {
		return nil, &SpawnError{msg: "unknown agent: " + strings.TrimSpace(opts.Agent)}
	}
	var notes []string
	opts, notes = healSpawnOpts(opts)
	for _, note := range notes {
		identity := opts.ID
		if identity == "" {
			identity = opts.Title
		}
		log.Printf("spawn %s: identity heal: %s", identity, note)
	}
	if !validPaneRole(opts.Role) {
		opts.Role = ""
	}
	opts.Cwd = expandHome(opts.Cwd)
	if opts.Cwd == "" {
		home, _ := os.UserHomeDir()
		opts.Cwd = home
	}
	opts.AutoMode, opts.NeverAutoAnswer = tightenPaneAnswerSettings(opts.Cwd, opts.AutoMode, opts.NeverAutoAnswer)
	contextPack := resolveContextPack(opts.ContextPack, opts.Cwd, opts.Kind, opts.Agent, opts.Cmd)

	// Resume-target validation (BOTH kinds). Never hand `claude --resume <id>` a
	// session whose transcript doesn't exist on disk: the headless native agent
	// exits with error_during_execution (dead Focus pane), and a terminal
	// `claude --resume` either exits 1 → re-armed Resumable → crash-loop, or (when
	// claude isn't on the daemon's PATH) falls through the `; exec bash -i` wrap to
	// a dead bash shell that masquerades as a healthy resume. A brand-new session
	// (transcript not written until the first message) and a phantom/expired id
	// both hit this. When the id isn't resumable, start FRESH (drop --resume) and
	// clear the dead id+path so the pane stops carrying / re-offering it.
	// noticeReason accumulates the discontinuity (if any) this spawn inflicts on
	// the conversation the caller asked to continue; stamped as a switchNotice on
	// the fresh pane so the client can TELL the user instead of silently starting
	// over ("resume was iffy" almost always traces to one of these silent drops).
	noticeReason := ""
	if rid := spawnResumeID(opts); effectiveAgentKind(opts.Agent) == agentClaude && rid != "" && !claudeTranscriptResumable(opts.Cwd, rid, opts.ClaudeTranscriptPath) {
		if opts.RequireResumableConversation {
			return nil, &SpawnError{msg: "required Claude transcript is no longer resumable"}
		}
		log.Printf("spawn: session %s has no on-disk transcript — starting fresh instead of a doomed --resume", shortSessionID(rid))
		opts.ClaudeSessionID = ""
		opts.ClaudeTranscriptPath = ""
		if opts.Kind != "native" {
			opts.Cmd = stripResumeArgv(opts.Cmd)
		}
		noticeReason = "fresh-start"
	}

	// The Codex twin of the transcript guard: never hand `codex resume <id>` /
	// `thread/resume` a thread whose rollout doesn't exist on disk. A virgin
	// Focus thread (rollout not materialized until the first turn) that died
	// pre-turn, and a deleted/expired rollout, both hit this: the TUI errors
	// out (or falls through a shell wrap to a dead bash), and the app-server
	// rejects thread/resume, exhausting the Focus health retries. Start FRESH
	// instead and say so. codexRolloutOnDisk fails OPEN when the sessions tree
	// can't be positively enumerated, so an unusual CODEX_HOME never silently
	// abandons a resumable conversation.
	if rid := spawnResumeID(opts); effectiveAgentKind(opts.Agent) == agentCodex && rid != "" && ((opts.RequireResumableConversation && !codexRolloutVerifiedOnDisk(rid)) || (!opts.RequireResumableConversation && !codexRolloutOnDisk(rid))) {
		if opts.RequireResumableConversation {
			return nil, &SpawnError{msg: "required Codex rollout is no longer resumable"}
		}
		log.Printf("spawn: codex thread %s has no on-disk rollout — starting fresh instead of a doomed resume", shortSessionID(rid))
		clearSpawnResume(&opts)
		noticeReason = "fresh-start"
	}

	// Duplicate-resume guard (BOTH kinds): never start a SECOND live
	// `claude --resume <id>` for a session some OTHER running pane already owns —
	// twin panes of one conversation confuse the user and race each other's
	// transcript writes. Seen after a Windows reboot: stale workspace defs in two
	// windows each carried the same baked `--resume <id>` and both respawned it.
	// Start FRESH instead (drop --resume, clear the id), exactly like the
	// transcript guard above. Same-pane respawns (resume click, native switch,
	// effort change) pass their own id in opts.ID and are excluded.
	if rid := spawnResumeID(opts); rid != "" {
		if owner := m.runningSessionOwner(opts.Agent, rid, strings.TrimSpace(opts.ID)); owner != "" {
			if opts.RequireResumableConversation {
				return nil, &SpawnError{msg: "conversation is already running in pane " + owner}
			}
			log.Printf("spawn: session %s is already live in pane %s — starting fresh instead of a duplicate --resume", shortSessionID(rid), owner)
			clearSpawnResume(&opts)
			noticeReason = "fresh-start"
		}
	}

	// A terminal pane spawned with `--resume <id>` owns that session from t=0:
	// seed ClaudeSessionID from the argv instead of waiting for the first
	// SessionStart hook, so the duplicate-resume guard above catches a second
	// spawn arriving milliseconds later and the resume badge is accurate
	// immediately. Header-attributed hooks stay authoritative and update it later.
	if opts.Kind != "native" {
		if rid := spawnResumeID(opts); rid != "" {
			switch effectiveAgentKind(opts.Agent) {
			case agentCodex:
				if strings.TrimSpace(opts.CodexThreadID) == "" {
					opts.CodexThreadID = rid
				}
			case agentMuse:
				if strings.TrimSpace(opts.MuseSessionID) == "" {
					opts.MuseSessionID = rid
				}
			default:
				if strings.TrimSpace(opts.ClaudeSessionID) == "" {
					opts.ClaudeSessionID = rid
				}
			}
		}
	}

	// Native-renderer panes run a headless Claude emitting stream-json. We rewrite
	// opts.Cmd to the verified headless argv here, so the rest of Spawn (binary
	// resolution, the agent/PTY transport, the readLoop) is identical to a terminal
	// pane — only the bytes on the wire differ (JSONL not VT). The terminal spawn
	// path is untouched (Kind=="" skips this).
	narrate := false
	if opts.Kind == "native" {
		switch effectiveAgentKind(opts.Agent) {
		case agentCodex:
			opts.Cmd = quotaNativeCodexArgv(opts)
		case agentMuse:
			// Muse has no long-lived interactive protocol, so the pane's
			// child is our own muse-driver subcommand bridging one-shot
			// `muse exec --json --session-id` turns (see muse_driver.go).
			// The session id is minted HERE so it persists before the first
			// event ever streams — resume and Terminal-switch need it.
			if strings.TrimSpace(opts.MuseSessionID) == "" {
				opts.MuseSessionID = newMuseSessionID()
			}
			opts.Cmd = nativeMuseArgv(opts.MuseSessionID, opts.Model, opts.Effort)
		default:
			// Narration is the default Claude Focus voice — always on (the
			// per-machine toggle was removed).
			narrate = true
			prompts := m.promptSuggestionsEnabled != nil && m.promptSuggestionsEnabled()
			opts.Cmd = nativeClaudeSpawnArgv(opts, narrate, prompts)
		}
	}
	if len(opts.Cmd) == 0 {
		opts.Cmd = []string{defaultShell()}
	}
	// Terminal orchestrator panes get the same brief the native argv above bakes
	// in. It goes on HERE — before the login-shell wrap — so the wrap quotes the
	// brief into the bash -lc string and Pane.Cmd persists it, exactly as a native
	// pane's Cmd persists the narration prompt. spawnCmdForRole is the pure seam
	// that decides (native / role-less / non-claude argvs come back untouched), so
	// this wiring is table-testable without a process.
	if opts.Role == paneRoleTaskOwner {
		return nil, &SpawnError{msg: "task engine retired; retained owner history remains available"}
	}
	opts.Cmd = spawnCmdForRole(opts.Kind, opts.Role, opts.Agent, opts.Cmd)
	opts.Cmd = spawnCmdForContextPack(opts.Cmd, contextPack)
	// A new pane's ID is chosen here rather than below so the request
	// capability can record which pane holds it.
	if strings.TrimSpace(opts.ID) == "" {
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		opts.ID = id
	}
	launchRouteOverrides, releaseRoute, err := paneRoutePrepare(m, &opts)
	if err != nil {
		return nil, &SpawnError{msg: err.Error()}
	}
	started := false
	if releaseRoute != nil {
		defer func() {
			if !started {
				releaseRoute()
			}
		}()
	}
	launchCredential := m.paneLaunchCredential(opts)
	// Launch terminal Codex in the normal buffer. Keep opts.Cmd canonical for
	// session detection, persistence, and resume; only the child argv gets the
	// UI flag. Native Codex app-server panes never enter this path.
	var codexInlineCmd []string
	if opts.Kind == "" {
		codexInlineCmd = codexInlineTerminalCmd(opts.Cmd)
	}
	// Keep the canonical command in pane persistence. Only the actual child
	// launch is bridged, so resume/provider attribution continue to read Codex.
	var codexLiveCmd []string
	if opts.Kind == "" && codexLiveEnabled(m.dataDir) && codexLiveLaunchReady(opts.Cmd, opts.Env) {
		codexLiveCmd = append([]string(nil), opts.Cmd...)
		if len(codexInlineCmd) > 0 {
			codexLiveCmd = codexInlineCmd
		}
	}
	if opts.Cols == 0 {
		opts.Cols = defaultCols
	}
	if opts.Rows == 0 {
		opts.Rows = defaultRows
	}
	if opts.Title == "" {
		opts.Title = opts.Cmd[0]
	}

	// Native resume self-heal: if the on-disk transcript ends with an unanswered
	// tool_use (the previous agent died mid-approval — e.g. an AskUserQuestion
	// whose answer was lost when the connection dropped), append a synthetic
	// tool_result so the resumed `claude --resume` loads a VALID sequence. Without
	// this the next user turn 400s and poisons the session forever, and the
	// interrupted turn spins on "thinking" with no recovery. Best-effort: a repair
	// failure never blocks the spawn (a poisoned resume is still better than no
	// pane, and the failure is surfaced for diagnosis).
	// Codex twin of the resume-repair notice below. Codex needs no transcript
	// repair (thread/resume + excludeTurns always loads), but it also cannot
	// TELL the user why their response stopped: measured against codex-cli
	// 0.144.4, a turn orphaned by a crash and a turn the user ESC'd both come
	// back as status:"interrupted" with identical fields, and both rollouts end
	// with the same turn_aborted{reason:"interrupted"} record. So the daemon's
	// own durable flag is the only evidence — and because it is disarmed on
	// every reported turn end (including a user interrupt), it surviving as
	// true means precisely "the process died before the turn reported an end".
	// Reuses the resume-repair reason: the client copy ("A restart interrupted
	// the previous response." + Continue) is agent-agnostic and exactly right.
	if opts.Kind == "native" && effectiveAgentKind(opts.Agent) == agentCodex &&
		opts.CodexTurnInFlight && strings.TrimSpace(opts.CodexThreadID) != "" {
		log.Printf("native resume: codex thread %s died mid-turn — surfacing the interrupted-turn notice", shortSessionID(opts.CodexThreadID))
		noticeReason = "resume-repair"
	}

	if opts.Kind == "native" && effectiveAgentKind(opts.Agent) == agentClaude {
		if sid := strings.TrimSpace(opts.ClaudeSessionID); sid != "" {
			// ONE transcript pass repairs a dangling tool_use AND reports whether
			// the session died owing a reply (they can be 58 MB — see
			// prepareClaudeResume). A fresh-start guard above wins no conflict
			// here — it cleared the session id, so this branch (sid != "") never
			// runs after one.
			st, err := prepareClaudeResume(opts.Cwd, sid)
			switch {
			case err != nil:
				log.Printf("native resume: transcript inspect failed for %s: %v", sid, err)
			case st.repairedToolUse:
				log.Printf("native resume: repaired an unanswered tool_use in session %s", sid)
				// The previous response was cut off mid-tool (daemon restart /
				// crash, NOT a mode switch) and we synthetically answered the
				// dangling tool_use so --resume loads. Surface it: without a
				// notice the pane just sits idle and the user can't tell why
				// their in-flight work stopped.
				noticeReason = "resume-repair"
			case st.awaitsReply:
				// The mid-TEXT twin of the repair: killed while streaming prose
				// or thinking, Claude writes NO assistant record, so there is no
				// dangling tool_use and the transcript simply stops after the
				// user's prompt. Nothing needs repairing — the session resumes
				// fine — but the user's answer silently evaporated, which is
				// exactly what the notice exists to say. A DELIBERATE stop always
				// writes an interrupt marker, so it never reaches this branch.
				log.Printf("native resume: session %s died before answering the last turn — surfacing the interrupted-turn notice", shortSessionID(sid))
				noticeReason = "resume-repair"
			}
		}
	}

	if info, err := os.Stat(opts.Cwd); err != nil {
		return nil, &SpawnError{msg: "working directory does not exist: " + opts.Cwd}
	} else if !info.IsDir() {
		return nil, &SpawnError{msg: "working directory is not a directory: " + opts.Cwd}
	}

	// Pre-check the command is resolvable, AND normalize to its absolute
	// path. go-pty's Windows (ConPTY) backend doesn't do PATH lookup the
	// way exec.Command does on Unix — bare names like "powershell.exe"
	// spawn but the ConPTY attachment fails and the child exits
	// immediately. Resolving here means an explicit "powershell.exe"
	// and the empty-default both end up as absolute paths, matching
	// what platformDefaultShell() already returns.
	resolved, err := exec.LookPath(opts.Cmd[0])
	loginWrapped := false
	if err != nil {
		if paneRouteActive(opts.Env) {
			return nil, &SpawnError{msg: "team CLI executable is unavailable on the daemon PATH"}
		}
		// Login-shell fallback (Unix only). The daemon inherits the
		// process-launcher's PATH — when launched by Tauri or systemd
		// that's the bare system PATH, missing nvm / pyenv / cargo bin
		// dirs the user lives in. Bash panes still work because the
		// shell sources ~/.bashrc on first prompt; but a non-shell
		// command like `claude`, `codex`, `bun` fails LookPath here
		// and a SpawnError 400 was the only signal back. Worse, the
		// ARIA spawn tools called this in a loop and reported a
		// confidently-empty workspace.
		//
		// Fix: when LookPath fails on Unix and the cmd is a bare name
		// (no slashes), retry by wrapping in `bash -lc 'exec <cmd>'`.
		// A login shell loads the user's full env (nvm, pyenv, asdf,
		// cargo, ~/.local/bin, …) and resolves the binary the same
		// way the user's interactive shell would. Title falls through
		// unchanged so the pane still reads as "claude" / "codex" /
		// whatever the caller asked for.
		if shell, looked := tryLoginShellWrap(opts.Cmd, opts.Provider, m.glmSettings); looked {
			opts.Cmd = shell
			resolved = opts.Cmd[0]
			loginWrapped = true
		} else if win, found := findOnWindowsPath(opts.Cmd[0]); found {
			// Windows analogue of the Unix login-shell fallback: the daemon's
			// inherited PATH can be a stale snapshot missing a dir added to the
			// User PATH after launch (e.g. a Claude Code install), so LookPath
			// misses what a fresh shell would resolve. Search the live registry
			// PATH before failing. (Returns false off Windows — see winpath_*.go.)
			resolved = win
		} else {
			return nil, &SpawnError{msg: "command not found: " + opts.Cmd[0] +
				" (check the command name and that it's on the daemon's $PATH)"}
		}
	}
	opts.Cmd[0] = resolved
	launchCmd := opts.Cmd
	if len(codexInlineCmd) > 0 {
		if loginWrapped {
			// A direct Codex command missing from the daemon's PATH is run
			// through a login shell. Rebuild only the launch wrapper with the
			// inline flag; the persisted wrapper still parses for resume.
			wrapped, ok := tryLoginShellWrap(codexInlineCmd, opts.Provider, m.glmSettings)
			if !ok {
				return nil, &SpawnError{msg: "could not launch Codex inline through the login shell"}
			}
			launchCmd = wrapped
		} else {
			// Resolution can change `codex` to codex.exe/codex.cmd on Windows.
			// Retain the already-classified inline argv and swap only its binary.
			launchCmd = append([]string{opts.Cmd[0]}, codexInlineCmd[1:]...)
		}
	}
	shellDaemonURL := ""
	if opts.Kind == "" {
		if codexUnixShellNeedsDaemonAddress(opts.Cmd) && !paneRouteActive(opts.Env) && m.codexDaemonURL != "" {
			// Non-secret address only; generic Unix shells obtain account and
			// capability afresh through their Codex shim. Managed CLI/login
			// wrappers retain their existing already-prepared route plan.
			opts.Env = append(filterOutEnvKeys(opts.Env, []string{paneDaemonURLEnv}), paneDaemonURLEnv+"="+m.codexDaemonURL)
		}
		if shellCmd := codexInlinePowerShellCmd(opts.Cmd); len(shellCmd) > 0 {
			launchCmd = shellCmd
			// Non-secret address only; selected account/capability are resolved
			// afresh inside each typed Codex invocation, never in the shell env.
			if daemonURL := paneShellDaemonURL(m.codexDaemonURL); daemonURL != "" {
				opts.Env = append(opts.Env, paneDaemonURLEnv+"="+daemonURL)
				shellDaemonURL = daemonURL
			}
		}
	}

	id := strings.TrimSpace(opts.ID)
	if id == "" {
		var err error
		id, err = randomID()
		if err != nil {
			return nil, err
		}
	}
	// Without the machine-wide registration (agent_integration.go), a pane
	// that starts Claude Code or Codex directly still gets SoluTerminal's tools
	// and hooks for this process only. A Codex pane usually runs through the
	// live bridge (codex-terminal), which forwards these -c flags to both its
	// app-server and its TUI, so the bridge command gets them too. Without
	// them Codex fell back to the user's own config: no tools at all on a
	// fresh install, or another SoluTerminal's tools on a machine that has one.
	if len(codexLiveCmd) == 0 {
		launchCmd = injectAgentArgsForPane(m.dataDir, id, opts.Env, launchCmd)
	} else {
		codexLiveCmd = injectAgentArgsForPane(m.dataDir, id, opts.Env, codexLiveCmd)
	}
	m.mu.RLock()
	existing := m.panes[id]
	idInUse := existing != nil && existing != opts.ReplaceSuspended
	m.mu.RUnlock()
	if idInUse {
		return nil, fmt.Errorf("pane id already exists: %s", id)
	}
	socketPath, socketDir, err := agentSocketPath(m.dataDir, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir agents: %w", err)
	}

	// Attribute tool-call hooks to this pane. Claude Code's HTTP hooks
	// can forward specific env vars as request headers (see
	// ~/.claude/settings.json's allowedEnvVars). Setting SOLU_PANE_ID
	// here means every tool Claude runs inside THIS pane carries the
	// pane id when it POSTs to /api/hooks/tool-call — turning the
	// Activity feed into per-pane attribution for free.
	opts.Env = append(opts.Env, "SOLU_PANE_ID="+id)
	opts.Env = append(opts.Env, contextPackEnv(contextPack)...)
	if contextPack.Status == contextPackMissing {
		log.Printf("spawn: context pack %s not found — pane %s starts unbriefed", contextPack.Dir, id)
	}

	// Pin Claude Code to its CLASSIC renderer. Since ~v2.1.150 Claude
	// defaults to "fullscreen" (alternate-screen) rendering, which has ZERO
	// scrollback — every Term-pane feature built on normal-buffer output
	// (local scrolling on mobile and desktop, reflow, replay, grid reads)
	// degrades to page-jump round-trips, and under ConPTY even mouse wheel
	// dies (conhost eats the reports; see input_wheel.go). The env var is
	// Claude's documented opt-out and beats the user's saved /tui setting;
	// it's inert for every other command a pane might run. Panes where the
	// user WANTS fullscreen can still run /tui fullscreen per-session.
	opts.Env = append(opts.Env, "CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1")
	if opts.Kind == "" && runtime.GOOS != "windows" && len(codexInlinePowerShellCmd(opts.Cmd)) == 0 {
		// Bash imports this into terminal panes, including a shell opened
		// after an agent exits. A plain `codex` typed at that prompt then
		// gets inline scrollback without editing the user's shell files.
		opts.Env = append(append(opts.Env, codexInlineBashEnv), paneShellBashEnv...)
	}

	selfExe := strings.TrimSpace(opts.AgentExecutable)
	if selfExe == "" {
		selfExe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("os.Executable: %w", err)
		}
	}

	// Build the agent argv: `solud agent --socket ... --cwd ... -- <cmd...>`
	argv := []string{"agent",
		"--socket", socketPath,
		"--cwd", opts.Cwd,
		"--cols", strconv.Itoa(int(opts.Cols)),
		"--rows", strconv.Itoa(int(opts.Rows)),
	}
	if opts.Role == paneRoleTaskOwner && opts.Kind == "" {
		argv = append(argv, "--owner-retirement-pane", opts.ID)
	}
	if opts.Kind == "native" || isHeadlessClaudeCmd(opts.Cmd) {
		// Headless stream-json agents (claude -p) refuse a TTY stdin, so the
		// agent wrapper must wire the child to pipes, not a PTY. We gate on the
		// COMMAND as well as Kind (defense-in-depth): if any stale/older-daemon
		// resume path ever hands us the headless `claude -p` argv with an empty
		// Kind, claude -p still gets pipes instead of crashing with "Input must
		// be provided … --print" and dropping the pane to a bash shell.
		argv = append(argv, "--no-pty")
	}
	if opts.Provider == "glm" {
		// Pass only the NON-SECRET provider name. The agent reads
		// glm_settings.json from --data-dir below and resolves the token onto the
		// child's c.Env itself — the token NEVER transits this argv.
		argv = append(argv, "--provider", "glm")
	}
	// Every new agent needs the daemon data dir as its diagnostics destination.
	// Agents already running an older argv retain it until their next respawn.
	if m.dataDir != "" {
		argv = append(argv, "--data-dir", m.dataDir)
	}
	if len(opts.Env) > 0 {
		argv = append(argv, "--env", joinEnv(opts.Env))
	}
	argv = append(argv, "--")
	if len(codexLiveCmd) > 0 {
		childCmd, bridgeErr := codexLiveSpawnCommand(selfExe, m.dataDir, m.codexDaemonURL, codexLiveCmd, launchRouteOverrides)
		if bridgeErr != nil {
			return nil, bridgeErr
		}
		argv = append(argv, childCmd...)
	} else {
		childCmd := launchCmd
		if opts.Kind == "" {
			childCmd = codexPaneLaunchCommand(childCmd)
		}
		childCmd = routeChildCommand(childCmd, launchRouteOverrides)
		// Keep the driver command visible to provider/broker planning above;
		// only adapt argv0 at the final process launch boundary.
		if opts.Kind == "native" && len(childCmd) > 1 && childCmd[1] == "muse-driver" {
			childCmd = daemonCommandArgv(childCmd[0], childCmd[1:]...)
		}
		argv = append(argv, childCmd...)
	}

	// Keep pre-wire startup failures (including guard diagnostics) visible
	// in the daemon diagnostic stream. Normal pane output uses its protocol.
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	// buildAgentCmd makes a FRESH *exec.Cmd each call: a Cmd whose Start()
	// failed can't be re-Start()'d, so the breakaway-fallback retry below
	// needs to rebuild it.
	buildAgentCmd := func(breakaway bool) *exec.Cmd {
		c := daemonCommand(selfExe, argv...)
		// Setsid (Unix) / CREATE_NEW_PROCESS_GROUP+DETACHED (Windows)
		// makes the agent survive daemon death. See spawn_*.go.
		if breakaway {
			applyDetached(c)
		} else {
			applyDetachedNoBreakaway(c)
		}
		c.Env = os.Environ()
		c.Stdout = devNull
		c.Stderr = os.Stderr
		return c
	}

	agentCmd := buildAgentCmd(true)
	agentExited, err := startSupervisedAgentChild(agentCmd)
	if err != nil {
		// Windows: when the Tauri shell launched the daemon, the daemon
		// lives in a Job Object lacking JOB_OBJECT_LIMIT_BREAKAWAY_OK, so
		// CREATE_BREAKAWAY_FROM_JOB makes CreateProcess fail with
		// ERROR_ACCESS_DENIED. Retry without breakaway: the agent then
		// shares the parent job (it dies when the app quits) but the pane
		// opens instead of failing outright. See spawn_windows.go.
		if isJobBreakawayDenied(err) {
			log.Printf("pane %s: agent spawn denied job breakaway (%v); retrying without CREATE_BREAKAWAY_FROM_JOB", id, err)
			agentCmd = buildAgentCmd(false)
			agentExited, err = startSupervisedAgentChild(agentCmd)
		}
		if err != nil {
			return nil, fmt.Errorf("start agent: %w", err)
		}
	}
	agentPid := agentCmd.Process.Pid

	// Connect to the agent's socket, retrying while it binds.
	conn, err := dialStartingAgent(socketPath, agentExited, agentStartupDialTimeout)
	if err != nil {
		// Clean up the agent we just spawned so we don't leak it.
		_ = killChildTree(agentCmd.Process)
		return nil, fmt.Errorf("connect agent: %w", err)
	}

	now := time.Now()
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		createdAt = now.UTC()
	}
	if paneRoute.shellStarted != nil {
		// Every (re)spawn reports: an empty address clears what an earlier
		// PowerShell terminal under this pane id was given.
		paneRoute.shellStarted(id, shellDaemonURL)
	}
	p := &Pane{
		Credential:              launchCredential,
		credentialSelection:     m.selectedCredentialAccount,
		credentialTeamSelection: m.selectedCredentialTeam,
		credentialLiveStatus:    m.liveCredentialStatus,
		codexLiveDataDir:        m.dataDir,
		AutoMode:                opts.AutoMode,
		NeverAutoAnswer:         opts.NeverAutoAnswer,
		ID:                      id,
		Cwd:                     opts.Cwd,
		Cmd:                     opts.Cmd,
		Title:                   opts.Title,
		Status:                  PaneRunning,
		CreatedAt:               createdAt,
		Cols:                    opts.Cols,
		Rows:                    opts.Rows,
		Pid:                     agentPid,
		processScope:            ownedScopeForProcess(agentCmd.Process),
		LastActivityMs:          now.UnixMilli(),
		SocketPath:              socketPath,
		ClaudeSessionID:         strings.TrimSpace(opts.ClaudeSessionID),
		CodexThreadID:           strings.TrimSpace(opts.CodexThreadID),
		MuseSessionID:           strings.TrimSpace(opts.MuseSessionID),
		HarnessSessionID:        strings.TrimSpace(opts.HarnessSessionID),
		HarnessProvider:         strings.TrimSpace(opts.HarnessProvider),
		ClaudeTranscriptPath:    strings.TrimSpace(opts.ClaudeTranscriptPath),
		TitleOverride:           strings.TrimSpace(opts.TitleOverride),
		Origin:                  opts.Origin.clone(),
		Review:                  opts.Review.clone(),
		Identity:                opts.Identity.clone(),
		harnessSessionDirs:      harnessSessionDirs(m.dataDir),
		// We just sized this PTY, so its dims are real — UNLESS the caller
		// spawned from a persisted/guessed width (resume paths) and asked us
		// to leave them unverified, so the first client attach re-measures.
		dimsVerified: !opts.DimsUnverified,

		agentConn: conn,
		buffer:    NewRingBuffer(ringBufferBytes),
		vt:        vt10x.New(vt10x.WithSize(int(opts.Cols), int(opts.Rows))),
		replayEmu: grid.New(int(opts.Cols), int(opts.Rows)),
		subs:      map[int64]*subEntry{},
		done:      make(chan struct{}),
	}

	p.onPromptsCommit = m.onPromptsCommit
	p.onClaudeSessionRotated = m.onClaudeSessionRotated
	p.onTurnStateEdge = m.onTurnStateEdge
	p.onCodexTurnFlagCommit = m.onCodexTurnFlagCommit
	p.onDimsCommit = m.onDimsCommit
	p.onOwnerViewersCommit = m.onOwnerViewersCommit
	p.Kind = opts.Kind
	p.Agent = opts.Agent
	p.Provider = opts.Provider
	p.Effort = opts.Effort
	p.Model = opts.Model
	p.Role = opts.Role
	p.ownerViewerKnowledgeKnown = opts.Role == paneRoleTaskOwner
	p.ContextPack = opts.ContextPack
	p.ContextPackStatus = contextPack.Status
	// Stamp the process's actual narration state (false for terminal panes), so
	// the per-pane "narrating" badge reflects what this agent was spawned with.
	p.Narrating = narrate

	m.mu.Lock()
	if m.panes[id] != opts.ReplaceSuspended {
		m.mu.Unlock()
		_ = conn.Close()
		_ = terminatePid(agentPid)
		return nil, fmt.Errorf("pane changed while restoring: %s", id)
	}
	m.panes[id] = p
	m.mu.Unlock()

	if p.Kind == "native" {
		p.ensureNative()
		// Seed the "always allow" set from opts (switch-mode / rollback carry
		// the user's opt-ins across a same-conversation respawn). A brand-new
		// pane passes nil here, so it correctly starts with an empty allowlist.
		p.setNativeAllowlist(opts.NativeAllowlist)
		if effectiveAgentKind(p.Agent) == agentCodex {
			if err := p.bootstrapNativeCodex(); err != nil {
				m.mu.Lock()
				if m.panes[id] == p {
					if opts.ReplaceSuspended != nil {
						m.panes[id] = opts.ReplaceSuspended
					} else {
						delete(m.panes, id)
					}
				}
				m.mu.Unlock()
				_ = conn.Close()
				_ = terminatePid(agentPid)
				return nil, fmt.Errorf("bootstrap Codex Focus: %w", err)
			}
		}
	}

	// Surface any resume discontinuity the guards above inflicted (see
	// noticeReason). fromKind is this pane's own kind; interrupted=true only
	// for resume-repair (there IS a cut-off response to continue) — a
	// fresh-start has nothing to continue, its banner is informational.
	if noticeReason != "" {
		p.setSwitchNoticeReason(kindOrTerminal(p.Kind), noticeReason, noticeReason == "resume-repair")
	}

	if p.Snapshot().AutoMode != "" {
		p.ensureGrid()
	}
	started = true
	go p.readLoop(m)
	return p, nil
}

// kindOrTerminal normalizes the empty terminal Kind for the client contract.
func kindOrTerminal(kind string) string {
	if kind == "" {
		return "terminal"
	}
	return kind
}

// AttachExisting reattaches to an agent that's already running (e.g. after
// a daemon restart). Returns nil, nil if the agent is dead; callers retain
// the pane as a suspended recovery record.
func (m *Manager) AttachExisting(info *PersistedPane) (*Pane, error) {
	copy := *info
	info = &copy
	info.AutoMode, info.NeverAutoAnswer = tightenPaneAnswerSettings(info.Cwd, info.AutoMode, info.NeverAutoAnswer)
	// Is the agent process alive?
	if !pidAlive(info.AgentPid) {
		return nil, nil
	}
	// Can we connect? Short timeout — if not, consider it dead.
	conn, err := dialAgent(info.SocketPath, 1*time.Second)
	if err != nil {
		return nil, nil
	}

	// PersistedPane has no proven activity epoch. Terminal replay must not
	// make reattach time look like new output. Native behavior is unchanged.
	activityMs := int64(0)
	if info.Kind == "native" {
		activityMs = time.Now().UnixMilli()
	}
	cols, rows := info.Cols, info.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	agent, _ := normalizeAgentKind(info.Agent, info.Cmd, info.CodexThreadID)
	p := &Pane{
		Credential:               clonePaneCredential(info.Credential),
		credentialSelection:      m.selectedCredentialAccount,
		credentialTeamSelection:  m.selectedCredentialTeam,
		credentialLiveStatus:     m.liveCredentialStatus,
		codexLiveDataDir:         m.dataDir,
		ID:                       info.ID,
		Cwd:                      info.Cwd,
		Cmd:                      info.Cmd,
		Title:                    info.Title,
		Status:                   PaneRunning,
		CreatedAt:                info.CreatedAt,
		Cols:                     cols,
		Rows:                     rows,
		Pid:                      info.AgentPid,
		processScope:             cloneValuePtr(info.ProcessScope),
		LastActivityMs:           activityMs,
		SocketPath:               info.SocketPath,
		Kind:                     info.Kind,
		Agent:                    agent,
		Provider:                 info.Provider,
		Effort:                   info.Effort,
		Model:                    info.Model,
		Role:                     info.Role,
		ContextPack:              info.ContextPack,
		ContextPackStatus:        info.ContextPackStatus,
		AnnounceDelegationEvents: info.AnnounceDelegationEvents,
		AutoMode:                 info.AutoMode,
		NeverAutoAnswer:          info.NeverAutoAnswer,
		Narrating:                info.Narrating,
		Flagged:                  info.Flagged,
		FlagMemo:                 info.FlagMemo,
		FlagOwner:                info.FlagOwner,
		ClaudeSessionID:          info.ClaudeSessionID,
		CodexThreadID:            info.CodexThreadID,
		MuseSessionID:            info.MuseSessionID,
		HarnessSessionID:         info.HarnessSessionID,
		HarnessProvider:          info.HarnessProvider,
		ClaudeTranscriptPath:     info.ClaudeTranscriptPath,
		TitleOverride:            info.TitleOverride,
		Origin:                   info.Origin.clone(),
		Review:                   info.Review.clone(),
		Identity:                 info.Identity.clone(),
		LastPrompts:              cloneStringSlice(info.LastPrompts),
		harnessSessionDirs:       harnessSessionDirs(m.dataDir),

		agentConn: conn,
		buffer:    NewRingBuffer(ringBufferBytes),
		vt:        vt10x.New(vt10x.WithSize(int(cols), int(rows))),
		replayEmu: grid.New(int(cols), int(rows)),
		subs:      map[int64]*subEntry{},
		done:      make(chan struct{}),
		// dimsVerified stays false: these dims are the persisted copy, not
		// the live PTY's. The agent's FrameDims (first frame on the socket
		// we just dialed) or the first client resize verifies them. Old
		// agents never send FrameDims — the PaneSnapshot.DimsUnverified
		// flag then keeps clients from seeding resizes with the stale value.
	}
	p.onPromptsCommit = m.onPromptsCommit
	p.onClaudeSessionRotated = m.onClaudeSessionRotated
	p.onTurnStateEdge = m.onTurnStateEdge
	p.onCodexTurnFlagCommit = m.onCodexTurnFlagCommit
	p.onDimsCommit = m.onDimsCommit
	p.onOwnerViewersCommit = m.onOwnerViewersCommit
	p.ownerRetirementRestoreDisabled.Store(info.OwnerRetirementRestoreDisabled)
	p.ownerViewerKnowledgeKnown = info.OwnerRetirementViewersKnown
	p.ownerViewerPendingDevices = make(map[string]bool, len(info.OwnerRetirementPendingDevices))
	for _, key := range info.OwnerRetirementPendingDevices {
		p.ownerViewerPendingDevices[key] = true
	}

	p.codexIdentityBlocked.Store(info.IdentityBlocked)
	m.mu.Lock()
	m.panes[info.ID] = p
	m.mu.Unlock()

	if p.Snapshot().AutoMode != "" {
		p.ensureGrid()
	}
	// Restore an armed deferred switch ("switch when Claude finishes") — the
	// restart that interrupted it is most often the update flow itself, exactly
	// when users arm one. The caller (main.go boot loop) re-creates the watcher
	// via ensureSwitchWatcher once the API server exists; RequestedAt is the
	// ORIGINAL arm time so the max-wait cap spans the restart.
	if info.PendingSwitch != nil {
		p.setPendingSwitchAt(info.PendingSwitch.Target, info.PendingSwitch.RequestedAt)
	}

	if p.Kind == "native" {
		p.ensureNative()
		// This path reattaches to the SAME surviving app-server process, which
		// was initialized by the predecessor daemon. Prime only this adapter
		// instance from the durable thread id; a fresh Spawn must complete the
		// initialize + thread/resume handshake before it becomes ready.
		if effectiveAgentKind(p.Agent) == agentCodex {
			if adapter, ok := p.NativeAdapterRef(); ok {
				if driver, ok := adapter.(*CodexAdapter); ok {
					if threadID := strings.TrimSpace(p.CodexThreadID); threadID != "" {
						driver.RestoreThread(threadID, p.Cwd, p.Model)
						p.nativeGotInit.Store(true)
					}
				}
			}
		}
		// Restore the persisted "always allow" set. Reattach is the SAME
		// agent + same conversation, so the user's prior opt-ins still apply.
		p.setNativeAllowlist(info.NativeAllowlist)
		// Restore the durable Codex turn flag. The agent SURVIVED here (this is
		// the auto-update / daemon-restart path), so a turn may still be genuinely
		// in flight: the agent's stdout backlog replays as live state and will
		// re-drive both edges, but seeding it keeps the record accurate in the gap
		// before that replay lands — and correct if the agent dies during it.
		p.codexTurnInFlight.Store(info.CodexTurnInFlight)
	}

	go p.readLoop(m)
	go p.pokeBlankReattachAfter(1200 * time.Millisecond)
	return p, nil
}

// SuspendExisting materializes a dead-but-resumable agent pane (its agent
// died across a daemon restart) as an EXITED, Resumable pane WITHOUT spawning a
// process. The pane then appears in the UI exactly like a live-exit suspended
// pane — the "Resume session" affordance — so resuming is an explicit opt-in
// instead of the daemon respawning every session on restart. That auto-respawn
// felt aggressive: a WSL shutdown / reboot kills all agents at once, so the
// next daemon start used to resurrect dozens of `claude --resume` the user
// never asked for. Clicking Resume routes through the same
// handleResumePane/resumeExitedPane path a live-exit resume uses; the pane
// lingers in the store as a resumable entry until resumed or closed.
//
// An explicit recovery error also permits an unidentified session or shell
// placeholder. It stays visible until the user chooses a conversation or closes it.
func (m *Manager) SuspendExisting(info *PersistedPane) *Pane {
	copy := *info
	info = &copy
	info.AutoMode, info.NeverAutoAnswer = tightenPaneAnswerSettings(info.Cwd, info.AutoMode, info.NeverAutoAnswer)
	agent, _ := normalizeAgentKind(info.Agent, info.Cmd, info.CodexThreadID)
	// A managed terminal harness has its durable conversation in the client's
	// sidecar, adopted into HarnessSessionID; it is resumable even though there
	// is no Claude/Codex/Muse id.
	if strings.TrimSpace(info.HarnessSessionID) == "" &&
		conversationIDForAgent(agent, info.ClaudeSessionID, info.CodexThreadID, info.MuseSessionID) == "" && info.RecoveryError == "" {
		return nil
	}
	cols, rows := info.Cols, info.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	// done is created already-closed: the pane has no process or readLoop, so
	// Pane.Kill (close-or-timeout on done) and Manager.Kill (<-done) must not
	// block when the user closes a suspended pane.
	done := make(chan struct{})
	close(done)
	p := &Pane{
		Credential:               clonePaneCredential(info.Credential),
		credentialSelection:      m.selectedCredentialAccount,
		credentialTeamSelection:  m.selectedCredentialTeam,
		credentialLiveStatus:     m.liveCredentialStatus,
		codexLiveDataDir:         m.dataDir,
		ID:                       info.ID,
		Cwd:                      info.Cwd,
		Cmd:                      info.Cmd,
		Title:                    info.Title,
		Status:                   PaneExited,
		Resumable:                true,
		RecoveryError:            info.RecoveryError,
		CreatedAt:                info.CreatedAt,
		Cols:                     cols,
		Rows:                     rows,
		Pid:                      info.AgentPid,
		processScope:             cloneValuePtr(info.ProcessScope),
		LastActivityMs:           time.Now().UnixMilli(),
		SocketPath:               info.SocketPath,
		Kind:                     info.Kind,
		Agent:                    agent,
		Provider:                 info.Provider,
		Effort:                   info.Effort,
		Model:                    info.Model,
		Role:                     info.Role,
		ContextPack:              info.ContextPack,
		ContextPackStatus:        info.ContextPackStatus,
		AnnounceDelegationEvents: info.AnnounceDelegationEvents,
		AutoMode:                 info.AutoMode,
		NeverAutoAnswer:          info.NeverAutoAnswer,
		Narrating:                info.Narrating,
		Flagged:                  info.Flagged,
		FlagMemo:                 info.FlagMemo,
		FlagOwner:                info.FlagOwner,
		ClaudeSessionID:          info.ClaudeSessionID,
		CodexThreadID:            info.CodexThreadID,
		MuseSessionID:            info.MuseSessionID,
		HarnessSessionID:         info.HarnessSessionID,
		HarnessProvider:          info.HarnessProvider,
		ClaudeTranscriptPath:     info.ClaudeTranscriptPath,
		TitleOverride:            info.TitleOverride,
		Origin:                   info.Origin.clone(),
		Review:                   info.Review.clone(),
		Identity:                 info.Identity.clone(),
		LastPrompts:              cloneStringSlice(info.LastPrompts),
		harnessSessionDirs:       harnessSessionDirs(m.dataDir),

		buffer:    NewRingBuffer(ringBufferBytes),
		vt:        vt10x.New(vt10x.WithSize(int(cols), int(rows))),
		replayEmu: grid.New(int(cols), int(rows)),
		subs:      map[int64]*subEntry{},
		done:      done,
	}
	p.codexIdentityBlocked.Store(info.IdentityBlocked)
	p.onPromptsCommit = m.onPromptsCommit
	p.onClaudeSessionRotated = m.onClaudeSessionRotated
	// Carry the native "always allow" set forward so a later resume keeps the
	// user's per-pane opt-ins (mirrors AttachExisting). Only touches
	// nativeApprovalMu, so it's safe without a live native renderer; a no-op
	// for terminal panes.
	if p.Kind == "native" {
		p.setNativeAllowlist(info.NativeAllowlist)
		// Carry the durable Codex turn flag onto the suspended pane. The resume
		// path reads the PANE's snapshot (not the persisted record), so without
		// this a mid-turn death would resume with no notice. It is deliberately
		// NOT mirrored into nativeInTurn: this pane has no process, and lighting
		// that flag would render a dead pane as "working".
		p.codexTurnInFlight.Store(info.CodexTurnInFlight)
	}

	m.mu.Lock()
	if existing := m.panes[info.ID]; existing != nil {
		m.mu.Unlock()
		existing.mu.Lock()
		if existing.Status == PaneExited && info.RecoveryError != "" {
			existing.RecoveryError = info.RecoveryError
		}
		existing.mu.Unlock()
		return existing
	}
	m.panes[info.ID] = p
	m.mu.Unlock()
	return p
}

// ResumeAgentPane recreates a dead pane with the provider-specific durable
// conversation id. Native panes let Spawn build the headless protocol argv;
// terminal panes resume through the corresponding interactive CLI.
func (m *Manager) ResumeAgentPane(info PersistedPane) (*Pane, error) {
	var suspended *Pane
	if existing, ok := m.Get(info.ID); ok {
		if existing.Snapshot().Status == PaneRunning {
			return existing, nil
		}
		if !existing.tryAcquireRespawn() {
			return nil, fmt.Errorf("pane recovery already in progress")
		}
		defer existing.releaseRespawn()
		suspended = existing
		// The boot queue can predate a successful settings change on this tile.
		// Take security policy from the current pane after acquiring its lease.
		live := existing.Snapshot()
		info.AutoMode = live.AutoMode
		info.NeverAutoAnswer = live.NeverAutoAnswer
	}
	if info.IdentityBlocked {
		return nil, &SpawnError{msg: "conversation identity is unverified; choose the original conversation"}
	}
	_, ok := normalizeAgentKind(info.Agent, info.Cmd, info.CodexThreadID)
	if !ok {
		return nil, &SpawnError{msg: "unknown agent: " + strings.TrimSpace(info.Agent)}
	}
	var notes []string
	info, notes = healPersistedPane(info)
	for _, note := range notes {
		log.Printf("resume pane %s: identity heal: %s", info.ID, note)
	}
	agent := info.Agent
	harnessProvider := strings.TrimSpace(info.HarnessProvider)
	harnessSessionID := strings.TrimSpace(info.HarnessSessionID)
	// The daemon may have died before its process poll adopted the client's
	// sidecar. Read it directly so a boot resume never relaunches Claude over a
	// live DeepSeek conversation left on a reused shell.
	if harnessProvider == "" {
		if provider, sessionID, ok := readHarnessSessionRecord(harnessSessionDirs(m.dataDir), info.ID); ok {
			harnessProvider, harnessSessionID = provider, sessionID
		}
	}
	if harnessProvider == "" {
		harnessProvider = harnessProviderFromCmd(info.Cmd)
	}
	conversationID := conversationIDForAgent(agent, info.ClaudeSessionID, info.CodexThreadID, info.MuseSessionID)
	// A managed terminal harness resumes by replaying the pane's own launch
	// command with the durable session on the child env (the client calls
	// session/resume); it has no Claude/Codex/Muse conversation id to require.
	harnessResume := harnessProvider != "" && info.Kind != "native"
	shellResume := !harnessResume && conversationID == "" && plainShellRecovery(info)
	if !harnessResume && conversationID == "" && !shellResume {
		return nil, nil
	}
	cols, rows := info.Cols, info.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	title := info.Title
	if strings.TrimSpace(title) == "" {
		if harnessProvider != "" {
			title = harnessProvider
		} else {
			title = effectiveAgentKind(agent)
		}
	}
	var cmd []string
	var extraEnv []string
	if shellResume {
		cmd = cloneStringSlice(info.Cmd)
	} else if harnessResume {
		cmd = append([]string(nil), info.Cmd...)
		// A hand-launched harness has no managed argv to replay; rebuild it so
		// the boot resume actually reopens the conversation (never a shell).
		if !cmdLaunchesManagedHarness(cmd) && managedHarnessLaunchCmd != nil {
			if rebuilt, ok := managedHarnessLaunchCmd(harnessProvider); ok {
				cmd = rebuilt
			}
		}
		if cmdLaunchesManagedHarness(cmd) && harnessSessionID != "" {
			extraEnv = harnessResumeEnv(harnessSessionID)
		}
	} else {
		cmd = []string{"claude", "--resume", conversationID}
		switch effectiveAgentKind(agent) {
		case agentCodex:
			cmd = []string{"codex", "resume", conversationID}
		case agentMuse:
			cmd = []string{"muse", "resume", conversationID}
		}
		if info.Kind == "native" {
			cmd = nil
		}
	}
	// Hold the suspended pane's respawn lease before touching its socket;
	// another client may already have resumed it while boot recovery queued.
	if agentPidOwnsSocket(info.AgentPid, info.SocketPath) {
		killAgentTree(info.AgentPid)
	}
	if info.SocketPath != "" {
		_ = os.Remove(info.SocketPath)
	}
	p, err := m.Spawn(SpawnOpts{
		ID:                           info.ID,
		CreatedAt:                    info.CreatedAt,
		ReplaceSuspended:             suspended,
		AutoMode:                     info.AutoMode,
		NeverAutoAnswer:              info.NeverAutoAnswer,
		RequireResumableConversation: !harnessResume && conversationID != "",
		MuseSessionID:                info.MuseSessionID,
		Cwd:                          info.Cwd,
		Cmd:                          cmd,
		Title:                        title,
		Cols:                         cols,
		Rows:                         rows,
		ClaudeSessionID:              strings.TrimSpace(info.ClaudeSessionID),
		CodexThreadID:                info.CodexThreadID,
		CodexTurnInFlight:            info.CodexTurnInFlight,
		ClaudeTranscriptPath:         strings.TrimSpace(info.ClaudeTranscriptPath),
		TitleOverride:                info.TitleOverride,
		Origin:                       info.Origin.clone(),
		Review:                       info.Review.clone(),
		Identity:                     info.Identity.clone(),
		Kind:                         info.Kind,
		Agent:                        agent,
		HarnessSessionID:             harnessSessionID,
		HarnessProvider:              harnessProvider,
		Env:                          extraEnv,
		Provider:                     info.Provider,
		Effort:                       info.Effort,
		Model:                        info.Model,
		// Role rides along so a restart-resume re-derives the brief. The bare host
		// resume argv above is decorated from opts.Role by Spawn.
		Role:        info.Role,
		ContextPack: info.ContextPack,
		// Boot-time resume: cols/rows are the persisted panes.json copy with no
		// live client attached yet. Leave a terminal PTY's dims unverified so the
		// first client to attach re-measures and drives the real width instead of
		// inheriting a stale size (every-update garble). Native panes render
		// headless (no shared-PTY xterm width), so keep their prior behavior.
		DimsUnverified: info.Kind != "native",
	})
	if err != nil || p == nil {
		return p, err
	}
	p.mu.Lock()
	p.Kind = info.Kind
	p.AnnounceDelegationEvents = info.AnnounceDelegationEvents
	p.Flagged = info.Flagged
	p.FlagMemo = info.FlagMemo
	p.FlagOwner = info.FlagOwner
	p.LastPrompts = cloneStringSlice(info.LastPrompts)
	p.mu.Unlock()
	if p.Snapshot().AutoMode != "" {
		p.ensureGrid()
	}
	if p.Kind == "native" {
		// Resume continues the SAME conversation (claude --resume <sessionID>),
		// so the user's "always allow" opt-ins still apply — restore them.
		p.setNativeAllowlist(info.NativeAllowlist)
	}
	return p, nil
}

// ResumeClaudePane is retained as a compatibility alias for older internal
// callers. Agent identity on info still controls the actual provider.
func (m *Manager) ResumeClaudePane(info PersistedPane) (*Pane, error) {
	return m.ResumeAgentPane(info)
}

// pokeBlankReattachAfter repairs older/quiet agents that survive a daemon
// restart but do not send any replay bytes back to the new daemon. Without a
// fresh byte stream, browser clients mount a healthy running pane with an
// empty xterm surface until the process happens to write output. Ctrl+L is
// the standard readline/TUI repaint key: shells redraw the prompt without
// submitting input, and full-screen apps treat it as a refresh.
func (p *Pane) pokeBlankReattachAfter(after time.Duration) {
	// NEVER for native panes: their agent stdin is `claude -p --input-format
	// stream-json`, where a raw `\x0c` (Ctrl+L) is an invalid NDJSON line that
	// makes claude self-exit code 1. A native pane also doesn't need this repaint —
	// it rebuilds its surface from the event ring + the since=0 transcript backfill,
	// not from raw replay bytes. (The Ctrl+L poke is a terminal/Codex repair only.)
	if p.Kind == "native" {
		return
	}
	timer := time.NewTimer(after)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-p.done:
		return
	}
	// Nudge a repaint in two scenarios:
	//   1. Ringbuf is empty — agent never sent any replay (older quiet
	//      agent that survived the daemon swap without producing output).
	//   2. Ringbuf has bytes but the grid emulator extracts no readable
	//      text. This catches Codex normal-buffer panes whose raw replay
	//      is diff-only against evicted scrollback: the bytes fed
	//      replayEmu but produced no recognisable cells. Reattaching to
	//      an old-protocol agent that doesn't ship FrameSyntheticPaint
	//      lands here and Ctrl+L is the last-resort repaint trigger.
	blank := p.buffer.TotalWritten() == 0
	p.subsMu.Lock()
	if !blank && p.replayEmu != nil {
		if !gridSnapshotHasText(p.replayEmu.Snapshot()) {
			blank = true
		}
	}
	p.subsMu.Unlock()
	if !blank {
		return
	}
	if _, err := p.Write([]byte("\x0c")); err != nil {
		log.Printf("pane %s: blank reattach repaint failed: %v", p.ID, err)
		return
	}
	log.Printf("pane %s: blank reattach repaint requested", p.ID)
}

// maxUnixSocketLen is a conservative cap on a Unix-domain socket path.
// The kernel sockaddr_un.sun_path is 108 bytes on Linux/Windows and only
// 104 on macOS; overshooting surfaces as a cryptic "invalid argument"
// (EINVAL) at bind/connect time. We stay well under the strictest limit.
const maxUnixSocketLen = 100

// agentSocketPath returns the Unix-socket path for a pane plus the dir to
// create. Preferred location is <dataDir>/agents/<id>.sock; if that would
// overflow the sun_path limit (a deep custom -data dir on Windows), fall
// back to a short path under the OS temp dir. The returned path is what
// gets passed to the agent (--socket → bind), dialed, and persisted, so
// all four agree by construction.
func agentSocketPath(dataDir, id string) (sockPath, sockDir string, err error) {
	agentsDir := filepath.Join(dataDir, "agents")
	preferred := filepath.Join(agentsDir, id+".sock")
	if len(preferred) <= maxUnixSocketLen {
		return preferred, agentsDir, nil
	}
	// Overflow: relocate to a short dir under the OS temp dir. The pane id
	// is already globally-unique random hex, so <id>.sock can't collide
	// across data dirs sharing one temp dir.
	shortDir := filepath.Join(os.TempDir(), "solu-sock")
	shortPath := filepath.Join(shortDir, id+".sock")
	if len(shortPath) > maxUnixSocketLen {
		return "", "", fmt.Errorf("agent socket path too long even under temp dir (%d > %d): %s", len(shortPath), maxUnixSocketLen, shortPath)
	}
	return shortPath, shortDir, nil
}

// agentStartupDialTimeout bounds how long Spawn waits for a LIVE agent to bind
// its socket. An agent that dies first fails the spawn at once (see
// dialStartingAgent), so this only has to cover slow startup. The old flat 3s
// budget killed healthy agents on a loaded host (5s+ to bind observed on WSL)
// and turned every new pane into a 500.
const agentStartupDialTimeout = 15 * time.Second

// dialStartingAgent is dialAgent for an agent this daemon just started: it
// fails fast once the agent has exited instead of polling out the timeout on a
// socket that will never appear.
func dialStartingAgent(path string, exited <-chan struct{}, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("dial %s: agent did not listen within %s: %w", path, timeout, err)
		}
		select {
		case <-exited:
			return nil, fmt.Errorf("dial %s: agent exited before listening: %w", path, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// dialAgent connects to a Unix socket, retrying while the agent is starting.
func dialAgent(path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("dial %s: %w", path, lastErr)
}

// tryLoginShellWrap attempts to wrap `cmd` in a bash invocation that
// has the user's full env (nvm, asdf, pyenv) loaded, so a binary that
// lives outside the daemon's slim PATH (nvm-installed `claude` /
// `codex`, pyenv `python`, etc.) resolves the way the user's
// interactive shell would.
//
// Returns (wrapped, true) on success — wrapped[0] is bash's absolute
// path. Returns (nil, false) on Windows, when bash itself isn't on
// PATH, or when cmd[0] already contains a path separator (caller
// asked for a specific binary — don't second-guess them; let the
// original LookPath error stand).
//
// Implementation note — why we don't just use `bash -ilc`:
//
// `bash -lc` (login, non-interactive) sources /etc/profile and
// ~/.profile, but Ubuntu's default ~/.bashrc returns immediately on
// non-interactive shells (`case $- in *i*) ;; *) return;; esac` near
// the top of the file). nvm's setup lives BELOW that guard, so a
// login non-interactive shell never loads it. `bash -ilc` would work
// but emits MOTD ("Welcome to Ubuntu…") and a "no job control"
// warning into the pane on every spawn.
//
// Instead we run a non-interactive login shell and explicitly source
// the version managers ourselves. Each line is `[ -s file ] && . file`
// or `command -v X >/dev/null && eval "$(X init -)"` — silent no-ops
// if the manager isn't installed. nvm covers ~claude and the wider
// node ecosystem; asdf and pyenv are bonus for users on those.
func tryLoginShellWrap(cmd []string, provider string, glm *glmSettingsStore) ([]string, bool) {
	if runtime.GOOS == "windows" {
		return nil, false
	}
	if len(cmd) == 0 {
		return nil, false
	}
	if strings.ContainsAny(cmd[0], "/\\") {
		return nil, false
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return nil, false
	}
	parts := make([]string, len(cmd))
	for i, a := range cmd {
		parts[i] = shellSingleQuote(a)
	}
	// Shared with Claude detection (claude_bootstrap.go: loginShellInit) so a
	// binary we report "installed" is the SAME one panes can spawn. Sources
	// nvm/asdf/pyenv and prepends ~/.local/bin (the native Claude installer's
	// target, which the installer doesn't reliably persist onto PATH).
	init := loginShellInit()
	// GLM panes get a NON-SECRET prefix that runs after the login profile is
	// sourced (it's at the start of the -c string), so a profile-exported
	// ANTHROPIC_API_KEY/ANTHROPIC_BASE_URL can't shadow or redirect the GLM
	// routing. The token is NOT here — it rides the agent child's env.
	init = glmWrapPrefix(provider, glm) + init
	cmdline := strings.Join(parts, " ")
	// Codex app-server speaks a machine protocol on stdin immediately after
	// spawn. Do NOT use a login shell for it: arbitrary /etc/profile or
	// ~/.profile helpers run before the final exec and inherit that stdin. On
	// WSL, an interop self-heal launched cmd.exe probes from .profile; those
	// probes consumed the one-shot JSON initialize frame. The app-server stayed
	// alive but never became ready, so every Focus switch exhausted its health
	// retries and rolled back. loginShellInit already sources the version
	// managers needed to resolve nvm/asdf/pyenv installs, so a non-login shell
	// preserves discovery while giving the protocol an untouched stdin stream.
	shellFlag := "-lc"
	if len(cmd) >= 2 && cmd[0] == "codex" && cmd[1] == "app-server" {
		shellFlag = "-c"
	}
	// An INTERACTIVE agent TUI (claude, and the codex TUI incl. `codex resume`)
	// drops back to a shell when the agent exits, instead of `exec`-ing it
	// (which replaces bash, leaving NO shell underneath — so exiting the agent
	// would dead-end the pane). `; exec bash -i` runs the TUI in the foreground
	// and, whatever its exit code, hands the same PTY to an interactive shell.
	// `bash -i` inherits the env we just sourced, so the agent stays on PATH
	// for a manual resume; resuming is via the pane-header session-id badge (or
	// the agent's own printed resume hint). Codex was historically `exec`'d
	// directly, so quitting the TUI killed the whole pane while Claude fell to
	// a shell — the same UX now applies to both.
	//
	// A HEADLESS agent (native `claude -p … stream-json`, `codex app-server`)
	// is EXCLUDED: it's a non-interactive pipe agent, so it gets plain `exec`
	// like other wrapped commands. Appending `; exec bash -i` to a pipe agent
	// would, when it exits, leave the agent process running a zombie
	// interactive bash the native parser can't read — instead of the agent
	// exiting so the pane shows exited/resumable.
	interactiveTUI := (cmd[0] == "claude" && !isHeadlessClaudeCmd(cmd)) ||
		(cmd[0] == "codex" && !(len(cmd) >= 2 && cmd[1] == "app-server"))
	if interactiveTUI {
		return []string{bash, "-lc", init + "; " + cmdline + "; exec " + bash + " -i"}, true
	}
	return []string{bash, shellFlag, init + "; exec " + cmdline}, true
}

// glmWrapPrefix returns the NON-SECRET shell prefix prepended to a GLM pane's
// `bash -lc` command string: it unsets ANTHROPIC_API_KEY (which Claude Code would
// otherwise prefer over the GLM ANTHROPIC_AUTH_TOKEN that rides the child's env)
// and re-exports the base-URL + model + auto-compact-window vars. Because it sits
// at the START of the -c string it runs AFTER bash sources the login profile, so a
// profile that exports ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL can't shadow or
// redirect the routing. The token is deliberately ABSENT — it lives only on the
// agent child's c.Env (providerEnvVars), never in this argv (readable via
// /proc/<pid>/cmdline). Returns "" for non-GLM panes, leaving the wrap untouched.
func glmWrapPrefix(provider string, glm *glmSettingsStore) string {
	if provider != "glm" || glm == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("unset ANTHROPIC_API_KEY; export")
	for _, p := range glm.Snapshot().nonSecretEnvPairs() {
		b.WriteString(" ")
		b.WriteString(p[0])
		b.WriteString("=")
		b.WriteString(shellSingleQuote(p[1]))
	}
	b.WriteString("; ")
	return b.String()
}

// shellSingleQuote wraps s in POSIX single quotes safe for bash.
// Single quotes preserve every character except a literal `'`, which
// we escape by closing the quote, emitting an escaped quote, and reopening.
func shellSingleQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pidAlive(pid int) bool {
	return processAlive(pid)
}

func joinEnv(env []string) string {
	return strings.Join(env, ",")
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "pane_" + hex.EncodeToString(b[:]), nil
}

// defaultShell picks a shell when the spawn request doesn't specify
// one. Platform-split because Unix honors $SHELL while Windows has
// no such convention. See spawn_{unix,windows}.go.
func defaultShell() string { return platformDefaultShell() }

func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if len(p) >= 2 && p[1] == '/' {
		return home + p[1:]
	}
	return p
}

// starterWorkspace resolves a spawn's attribution before any active-workspace
// fallback. Live layout membership wins over historical origin. An agent's
// lead is consulted only when neither identifies a visible workspace. The
// visited set also bounds malformed or restored lead cycles.
func (a *apiServer) starterWorkspace(paneID string) string {
	seen := make(map[string]bool)
	for paneID = strings.TrimSpace(paneID); paneID != "" && !seen[paneID]; {
		seen[paneID] = true
		if name := a.ws.NameForPane(paneID); name != "" {
			return name
		}
		if a.mgr == nil {
			return ""
		}
		p, ok := a.mgr.Get(paneID)
		if !ok {
			return ""
		}
		origin := p.Snapshot().Origin
		if origin == nil {
			return ""
		}
		if name, ok := resolveWorkspaceName(a.ws, strings.TrimSpace(origin.Workspace)); ok && name != "_current" {
			return name
		}
		if origin.Source != paneOriginAgent {
			return ""
		}
		paneID = strings.TrimSpace(origin.LeadPaneID)
	}
	return ""
}
