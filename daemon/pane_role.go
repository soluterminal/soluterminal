package main

import "strings"

// pane_role.go owns the host-neutral pane product role. The role is chosen at
// spawn or changed through the role lifecycle endpoint, persisted with the
// pane, and re-derived on every spawn. pane_role_host.go delivers the shared
// brief through each supported host's durable instruction transport.

const paneRoleOrchestrator = "orchestrator"
const paneRoleTaskOwner = "task_owner"

// validPaneRole reports whether s is an accepted pane role. "" is valid and
// means "no role" — a plain pane (mirrors validEffortLevel).
func validPaneRole(s string) bool {
	switch strings.TrimSpace(s) {
	case "", paneRoleOrchestrator, paneRoleTaskOwner:
		return true
	default:
		return false
	}
}

// SetAnnounceDelegationEvents records the user's explicit choice about the
// "[solu] delegated work" status line, or clears it back to the role default
// with nil. Mirrors SetAutoMode: one field, under p.mu, no side effects. The
// pointer is replaced wholesale so a concurrent Snapshot copy stays valid.
func (p *Pane) SetAnnounceDelegationEvents(v *bool) {
	p.mu.Lock()
	p.AnnounceDelegationEvents = v
	p.mu.Unlock()
}

// AnnounceDelegationEffective resolves the tri-state to the answer the nudger
// acts on. An explicit override always wins; otherwise an orchestrator pane is
// on and every other pane is off.
func (p *Pane) AnnounceDelegationEffective() bool {
	return p.Snapshot().announceDelegationEffective()
}

func (s PaneSnapshot) announceDelegationEffective() bool {
	if s.AnnounceDelegationEvents != nil {
		return *s.AnnounceDelegationEvents
	}
	return strings.TrimSpace(s.Role) == paneRoleOrchestrator || strings.TrimSpace(s.Role) == paneRoleTaskOwner
}

// orchestratorBriefMarker is the brief's FIRST sentence fragment. It is how an
// already-appended (possibly STALE, from an older release) copy is recognized in
// an argv so it can be replaced instead of duplicated — orchestratorBrief must
// therefore always start with it.
const orchestratorBriefMarker = "You are the orchestrator for this machine"

// orchestratorBrief is the chief-of-staff system prompt appended to a
// role=orchestrator pane (terminal AND native). It defines the product
// behavior: own buildable work, coordinate separate pane writers, report what
// needs a human decision, and never route around the approval gates.
//
// HARD constraints on this text (pinned by TestOrchestratorBriefTextConstraints):
//   - NO ASCII single quote — the terminal argv is single-quoted into a
//     bash -lc string (tryLoginShellWrap / shellSingleQuote), and a literal
//     apostrophe would litter the persisted cmd with escaped-quote sequences.
//     Use the typographic ’ instead.
//   - It must NOT contain all three of "input-format", "output-format" and
//     "stream-json", or isHeadlessClaudeCmd would misclassify a TERMINAL
//     orchestrator pane as the headless native invocation and spawn it --no-pty.
//   - It must NOT contain the wrapped-form marker `'--resume' '`, which
//     resumeSessionIDFromCmd / stripResumeArgv scan for.
//   - No backticks (this is a Go raw string).
const orchestratorBrief = `You are the orchestrator for this machine. Own the work and report progress.

Work directly in your session: investigate, implement, test and prepare changes for the user to merge. The workflow task engine is retired. Do not create workflow tasks. Use list_panes, ask_pane, wait_for_pane and read_pane_tail to coordinate separate writers when work splits cleanly. Give each worker a self-contained assignment and avoid conflicting edits. Use spawn_pane for user-driven interaction.

Keep shared plans and handoffs in tracked repository files. Use create_schedule only when the user requests recurring work. When a thread needs the user to come back to it, flag its pane with set_pane_flag and a short memo.

Follow the repository AGENTS.md before handing back: get one independent review in a fresh context, fix each must-fix finding or explain the disagreement, and stop after at most two review rounds. In soluterminal use scripts/solu-review. The user decides what merges. Never bypass human approval gates or claim approval, merge or release without authoritative evidence.`

// systemPromptAppend joins the native narration prompt and the role brief into
// ONE --append-system-prompt value: claude takes a single flag, so the two
// system-prompt additions are concatenated (narration first, blank line, then
// the brief) rather than passed twice. Returns "" when neither applies.
func systemPromptAppend(narrate bool, role string) string {
	if role == paneRoleTaskOwner {
		return ""
	}
	orchestrator := strings.TrimSpace(role) == paneRoleOrchestrator
	switch {
	case narrate && orchestrator:
		return nativeNarrationPrompt + "\n\n" + orchestratorBrief
	case narrate:
		return nativeNarrationPrompt
	case orchestrator:
		return orchestratorBrief
	}
	return ""
}

// withOrchestratorBrief appends `--append-system-prompt <orchestratorBrief>` to
// a BARE terminal claude argv (the native/headless argv gets it from
// nativeClaudeArgvRole instead).
//
// Idempotent and self-healing: any existing bare `--append-system-prompt <v>`
// pair whose value carries orchestratorBriefMarker is dropped first, so a stale
// brief from an older release is REPLACED rather than duplicated. A user-supplied
// --append-system-prompt without the marker is left untouched.
//
// A cmd that is not bare claude is returned unchanged. That is the login-shell-
// wrapped `bash -lc '…'` form which workspace def-restore and the native_switch
// rollback replay from a persisted snapshot: it already carries whatever brief it
// was spawned with, and re-quoting it here would be a text rewrite of a shell
// string. Every clean respawn path rebuilds a bare ["claude","--resume",id] and
// re-derives the brief from the pane's Role, so the wrapped form self-heals on
// the next resume/refresh/effort/model/switch.
func withOrchestratorBrief(cmd []string) []string {
	if !cmdStartsClaude(cmd) {
		return cmd
	}
	out := make([]string, 0, len(cmd)+2)
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == "--append-system-prompt" && i+1 < len(cmd) &&
			strings.Contains(cmd[i+1], orchestratorBriefMarker) {
			i++ // drop the stale brief's value too
			continue
		}
		out = append(out, cmd[i])
	}
	return append(out, "--append-system-prompt", orchestratorBrief)
}

// spawnCmdForRole is the seam Manager.Spawn calls to decorate a TERMINAL pane's
// argv for its role. Native panes are already done — nativeClaudeArgvRole baked
// the brief into the headless argv from opts.Role — and a pane without the
// orchestrator role is untouched.
//
// It exists as a pure function so the kind/role→argv wiring is table-testable
// without launching a process (review finding: the decoration itself was
// covered, but nothing pinned that Spawn actually applied it, and only for the
// right kind).
func spawnCmdForRole(kind, role, agent string, cmd []string) []string {
	if kind != "native" && role == paneRoleTaskOwner {
		return withTaskOwnerBrief(agent, cmd)
	}
	if kind == "native" || strings.TrimSpace(role) != paneRoleOrchestrator {
		return cmd
	}
	host, ok := orchestratorHostFor(agent)
	if !ok {
		return cmd
	}
	return host.decorateTerminalArgv(cmd)
}

func orchestratorDeveloperInstructions(role string) string {
	if role == paneRoleTaskOwner {
		return ""
	}
	if strings.TrimSpace(role) == paneRoleOrchestrator {
		return orchestratorBrief
	}
	return ""
}
