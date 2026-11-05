package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const ariaNativeObservationTemplate = `[AUTONOMOUS NATIVE-PANE EVALUATION — you were invoked by the pane-state watcher, not by the user.]

Pane %q (id: %s) runs in native mode and is asking permission to use ONE tool. The exact request:

Tool: %s
Input:
<pane_output>
%s
</pane_output>

Policy for this pane:
"%s"

Decide:
1. If the policy clearly permits THIS tool call, call resolve_permission (it takes no arguments) to approve it. The user will see a %s before it actually fires.
2. If the policy does NOT clearly permit it (out of scope, risky, ambiguous), DO NOT call any tool — reply with ONE short sentence saying why. The request stays pending for the user to handle.

There is exactly one pending request bound to this turn; resolve_permission approves THAT request and no other. Do not call read_pane_tail. Be decisive; don't narrate.`

// aria_native_auto.go wires ARIA auto-mode (AutoMode "always" / "cautious" and
// per-pane standing orders) into NATIVE-renderer panes.
//
// A native pane runs the agent headless via stream-json; a tool-permission
// request arrives as a control_request[can_use_tool] and is parked in
// Pane.nativePendingApprovals (see approvals.go). It is answered NOT by typing
// into a TUI (the terminal directAutoFire / send_input path) but by writing a
// structured control_response allow/deny frame via Pane.resolveApproval. This
// file is the bridge: evaluateObservation (aria_auto.go) detects a native pane
// and delegates here.
//
// Two safety rules run through everything below:
//  1. Resolve by the EXACT RequestID ARIA evaluated — never an arbitrary
//     pending one. Parallel tool calls (Read/Grep/Bash at once) leave several
//     approvals open; the daemon binds the reqID server-side so the model can
//     never approve a safe Read and accidentally allow a dangerous Bash.
//  2. NEVER auto-answer AskUserQuestion / ExitPlanMode. Those are genuine user
//     questions, not safety gates — see isNonAutoApprovableNativeTool.
//
// Budget: native auto-resolution reuses the SAME fire-budget machinery as the
// terminal path (tryReserveAutoFireSlot reserves; the autoEmitter's
// confirm_resolved branch records via autoRecordFire). The only swap vs.
// directAutoFire is the write primitive — resolveApproval instead of a PTY
// write — so native does not become an un-capped path.

// ariaNativeAutoModeCautiousInstruction is the synthesised policy a native
// "cautious" turn runs with. Phrased for the structured resolve_permission
// tool (no TUI menu, no send_input) — the model sees the tool name + args
// directly, which is a cleaner signal than scraping a rendered menu.
const ariaNativeAutoModeCautiousInstruction = `Approve this native-pane permission request ONLY if the tool call is clearly low-risk.

LOW-RISK = read-only operations (reading files, listing, searching, status/inspection), formatting / linting, and simple edits to files INSIDE the pane's own project that continue work the user clearly initiated.

DO NOT approve (leave it pending for the user) if the tool would:
  (a) write, modify, or delete files OUTSIDE the current repo (system paths, dotfiles, other projects).
  (b) run shell commands that change system state, install packages, modify config, or hit network endpoints.
  (c) push to git, force-push, rewrite history, merge to main/master, delete branches, or touch remote repos.
  (d) read, write, or transmit secrets, .env files, credentials, tokens, API keys, or auth config.
  (e) anything you are not sure about — when in doubt, leave it for the user.

To approve, call resolve_permission (it takes no arguments). To decline, do NOT call any tool — reply with one short sentence naming the rule that applies (e.g. "Deferring — writes to ~/.ssh, rule (d).").`

// ariaNativeAutonomousTools is the tool surface an autonomous turn against a
// native pane is given: a single structured-approval tool. The model cannot
// choose which request to resolve — the daemon binds the exact RequestID
// (executeTool passes autonomousRequestID), so resolve_permission needs no args.
func ariaNativeAutonomousTools() []ariaToolDef {
	return []ariaToolDef{
		{
			Name:        "resolve_permission",
			Description: "Approve the SINGLE pending permission request described in this turn's message (a native-renderer pane asking to use one tool). Call it with no arguments ONLY when the request clearly satisfies the pane's policy AND is safe. To DECLINE, do NOT call this tool — reply with one short sentence explaining why; the request stays pending for the user. You cannot choose which request to resolve: the daemon binds this to the exact request shown above.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// nativeNeverAutoReason is the plain-English explanation shown in the ARIA
// decision log when always/cautious mode declines to auto-answer a tool it is
// structurally not allowed to. Without this the prompt just sits there and the
// user (esp. on mobile, where "auto-approval on" is showing) has no idea why.
func nativeNeverAutoReason(toolName string) string {
	switch toolName {
	case "ExitPlanMode":
		return "ExitPlanMode needs your review — ARIA never auto-approves leaving plan mode"
	default:
		return "AskUserQuestion needs your answer — ARIA never auto-answers questions"
	}
}

// isNonAutoApprovableNativeTool reports tool names ARIA must NEVER auto-answer.
// AskUserQuestion and ExitPlanMode arrive on the same can_use_tool channel as
// real permission gates and report StateChoicePrompt, but they are genuine
// user-facing questions — auto-"allowing" them silently steers the work (and
// resolveApproval is allow-only, which is structurally wrong for a question
// whose answer is a chosen option). Always leave these for the human.
func isNonAutoApprovableNativeTool(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "AskUserQuestion", "ExitPlanMode":
		return true
	}
	return false
}

// nativeApprovalSummary returns a short tool title + a human-readable input
// preview for an approval, used in confirm summaries, observation text, and
// eval-log snapshots.
func nativeApprovalSummary(ap *NativeApproval) (title, preview string) {
	if ap == nil {
		return "tool", ""
	}
	title = strings.TrimSpace(ap.ToolName)
	if title == "" {
		title = "tool"
	}
	return title, nativeApprovalInputPreview(ap.RawInput)
}

// nativeApprovalInputPreview decodes the most meaningful field out of a tool's
// raw input (the Bash command, the edited file path, the fetched URL, …) and
// falls back to a truncated JSON dump.
func nativeApprovalInputPreview(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, k := range []string{"command", "file_path", "path", "url", "pattern", "query", "prompt"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return truncateStr(strings.TrimSpace(v), 200)
			}
		}
	}
	return truncateStr(strings.TrimSpace(string(raw)), 200)
}

// nativeObservationText builds a structured, human-readable observation from a
// native pane's open approvals — one line per pending request. Used for the
// aria-observation UI event so the panel shows "Permission request: Bash — git
// status" instead of the raw JSONL stream-json that lives in the ring buffer.
func nativeObservationText(p *Pane) string {
	if p == nil {
		return ""
	}
	pending := p.listNativePending()
	if len(pending) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ap := range pending {
		if i > 0 {
			b.WriteByte('\n')
		}
		title, preview := nativeApprovalSummary(ap)
		if preview != "" {
			fmt.Fprintf(&b, "Permission request: %s — %s", title, preview)
		} else {
			fmt.Fprintf(&b, "Permission request: %s", title)
		}
	}
	return b.String()
}

// nativeCautiousDenyNeedles is the deterministic safety net for native cautious
// mode — a belt-and-suspenders check on top of the model's own judgment. A hit
// leaves the request pending for the user (never an auto-allow). This list
// applies to EVERY tool: secrets and sensitive paths matter whether they appear
// in a Bash command, a file path, or an MCP tool's arguments.
var nativeCautiousDenyNeedles = []struct {
	needle string
	reason string
}{
	{".env", "touches environment/secret files"},
	{"api key", "mentions credentials or secrets"},
	{"apikey", "mentions credentials or secrets"},
	{"secret", "mentions credentials or secrets"},
	{"credential", "mentions credentials or secrets"},
	{"~/.ssh", "touches SSH credentials"},
	{"/.ssh", "touches SSH credentials"},
	{"/etc/", "touches system configuration"},
	{"/usr/", "touches system paths"},
	{"/var/", "touches system paths"},
	{"/root/", "touches system paths"},
}

// nativeCautiousExecDenyNeedles are command/network patterns that only signal
// risk when the tool can EXECUTE or FETCH — Bash, WebFetch/WebSearch, any
// mcp__* tool, and anything unrecognized (unknown ⇒ assume it can act).
// Inside a Read/Edit/Write/Grep call these strings are just text — an edit to
// a README that mentions `curl` or a script that contains `rm -rf` must not
// park the request (the cautious LLM still reviews it) — so file-scoped tools
// skip this list. That asymmetry is what stopped the old list's biggest noise
// source: `https://` deferring every edit that mentioned a URL.
var nativeCautiousExecDenyNeedles = []struct {
	needle string
	reason string
}{
	{"token", "mentions credentials or secrets"},
	{"sudo ", "would run a privileged command"},
	{"rm -rf", "would run a destructive command"},
	{"shred ", "would run a destructive command"},
	{"dd if=", "would run a raw-disk command"},
	{"mkfs", "would format a filesystem"},
	{"truncate -s", "would truncate files"},
	{"del /f", "would run a destructive command"},
	{"rd /s", "would run a destructive command"},
	{"rmdir /s", "would run a destructive command"},
	{"chmod ", "would change permissions"},
	{"chown ", "would change ownership"},
	{"killall ", "would kill processes"},
	{"pkill ", "would kill processes"},
	{"shutdown ", "would shut the machine down"},
	{"systemctl ", "would touch system services"},
	{"crontab ", "would change scheduled jobs"},
	{"git push", "would mutate a remote repository"},
	{"force-push", "would rewrite remote git history"},
	{"git reset --hard", "would rewrite local git state"},
	{"git clean", "would delete untracked files"},
	{"git checkout -- ", "would discard local changes"},
	{"git stash drop", "would discard stashed changes"},
	{"npm install", "would install packages"},
	{"pnpm install", "would install packages"},
	{"yarn install", "would install packages"},
	{"pip install", "would install packages"},
	{"go install", "would install packages"},
	{"cargo install", "would install packages"},
	{"npx ", "would run an arbitrary package"},
	{"pipx ", "would run an arbitrary package"},
	{"apt install", "would install packages"},
	{"apt-get install", "would install packages"},
	{"brew install", "would install packages"},
	{"dnf install", "would install packages"},
	{"yum install", "would install packages"},
	{"kubectl delete", "would delete cluster resources"},
	{"terraform apply", "would change infrastructure"},
	{"terraform destroy", "would destroy infrastructure"},
	{"drop table", "would drop database objects"},
	{"drop database", "would drop database objects"},
	{"curl ", "would hit the network"},
	{"wget ", "would hit the network"},
	{"http://", "mentions a network endpoint"},
	{"https://", "mentions a network endpoint"},
}

// nativeFileScopedTools are Claude's in-repo file/search tools: their args are
// paths, patterns and file CONTENT, never something the tool itself executes.
// Everything not in this set — Bash, WebFetch, mcp__* servers, tools added in
// future claude versions — gets the exec/network needle check too, because
// unknown means "can possibly act on the world".
var nativeFileScopedTools = map[string]bool{
	"Read":         true,
	"Edit":         true,
	"MultiEdit":    true,
	"Write":        true,
	"ApplyPatch":   true, // Codex app-server's structured fileChange approval
	"NotebookEdit": true,
	"Grep":         true,
	"Glob":         true,
	"LS":           true,
}

// bashCommandFromInput extracts the "command" string from a Bash tool input.
func bashCommandFromInput(raw json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	return in.Command
}

// SQL phrases the shell parser can't see through — they live inside a db
// client's quoted argument (`psql -c "drop table x"`), so a parsed-clean
// command still gets this scan.
var bashSQLDenyNeedles = []struct {
	needle string
	reason string
}{
	{"drop table", "would drop database objects"},
	{"drop database", "would drop database objects"},
}

// nativeCautiousAutoDenyReason returns a non-empty reason when an approval
// should be left for the user under cautious mode, or "" if it's safe to
// consider for auto-allow.
func nativeCautiousAutoDenyReason(ap *NativeApproval) string {
	if ap == nil {
		return ""
	}
	hay := strings.ToLower(ap.ToolName + " " + string(ap.RawInput))
	for _, rule := range nativeCautiousDenyNeedles {
		if strings.Contains(hay, rule.needle) {
			return rule.reason
		}
	}
	if !nativeFileScopedTools[ap.ToolName] {
		// Bash gets a REAL parse (shellrisk.go) instead of the substring
		// needles: `git push` parks while `git log --grep push` doesn't, and
		// composition the needles never saw (`ls; rm -rf /`, `echo x | xargs
		// rm -rf`, `bash -c "curl ..."`) parks too. The needles remain the
		// fallback for a command that doesn't parse — an unparseable command
		// must never come out MORE trusted than before. Deliberately dropped
		// for parsed commands: the bare "token" needle (it parked every
		// mention of tokenizer/tokens.ts; secrets exfil needs network or
		// file access, which park on their own).
		if ap.ToolName == "Bash" {
			if cmd := bashCommandFromInput(ap.RawInput); cmd != "" {
				if reason, parsed := shellRiskReason(cmd); parsed {
					if reason != "" {
						return reason
					}
					low := strings.ToLower(cmd)
					for _, rule := range bashSQLDenyNeedles {
						if strings.Contains(low, rule.needle) {
							return rule.reason
						}
					}
					return ""
				}
			}
		}
		for _, rule := range nativeCautiousExecDenyNeedles {
			if strings.Contains(hay, rule.needle) {
				return rule.reason
			}
		}
	}
	return ""
}

// directAutoFireNative is the no-LLM fast path for native AutoMode "always"
// panes — the structured-resolve mirror of directAutoFire. It resolves EVERY
// eligible pending approval in one pass (parallel tool calls leave several open
// at once; answering them one-per-sweeper-tick would be ~5s each). Each
// resolution reserves a budget slot, writes the allow frame, and routes
// confirm_resolved through the autoEmitter so the fire is recorded exactly once.
//
// Caller holds the per-pane in-flight eval lock and has confirmed AutoMode is
// "always" with no standing order.
func (a *Aria) directAutoFireNative(ctx context.Context, paneID, paneTitle string) {
	const orderID = "automode_always_direct"
	start := time.Now()
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return
	}
	ctx, policy := withAriaAutoModePolicy(ctx, p)
	if !policy.matches(p) {
		return
	}
	emit := a.autoEmitter(paneID, paneTitle, orderID, 0)
	emit(map[string]any{
		"type":        "auto-eval-start",
		"pane_id":     paneID,
		"pane_title":  paneTitle,
		"order_id":    orderID,
		"instruction": "automode-always native direct fire (no LLM)",
	})

	// No user-input hold here, deliberately. The terminal path holds because it
	// writes a bare Enter that can land in a composer the user is typing into
	// (autoFireUserHold). A native resolve is a control_response frame bound to
	// a RequestID; it cannot touch the user's draft, so holding it would stall
	// auto-approval for no safety gain.

	pending := p.listNativePending()
	firedAny := false
	eligibleAny := false
	for _, ap := range pending {
		if ap == nil || ap.RequestID == "" {
			continue
		}
		// Never auto-answer genuine user questions. Record it (deduped) so the
		// decision log explains why a pane with "auto-approval on" is still
		// sitting on a prompt — the single most confusing case on mobile.
		if isNonAutoApprovableNativeTool(ap.ToolName) {
			a.recordEvalOutcomeDedup(AriaEvalOutcome{
				PaneID:       paneID,
				Kind:         "skipped",
				Reason:       nativeNeverAutoReason(ap.ToolName),
				TailSnapshot: strings.TrimSpace(ap.ToolName + " " + nativeApprovalInputPreview(ap.RawInput)),
				Model:        "direct-native",
			})
			continue
		}
		eligibleAny = true
		// Cap / pause gate, re-checked per fire — each approval costs a slot and
		// the 5/sec burst cap can trip mid-batch. On suppression, stop; the
		// recovery sweeper retries the rest.
		if allowed, capReason := a.autoAllowed(); !allowed {
			a.uiBus.publish(uiEvent{
				Type: "aria-auto-suppressed", PaneID: paneID, PaneTitle: paneTitle,
				Ts: time.Now().UnixMilli(), Reason: autoSuppressReason(capReason),
			})
			break
		}
		title, preview := nativeApprovalSummary(ap)
		confirmID := fmt.Sprintf("auto_native_direct_%s_%s", paneID, ap.RequestID)
		if !a.tryReserveAutoFireSlot(confirmID) {
			a.uiBus.publish(uiEvent{
				Type: "aria-auto-suppressed", PaneID: paneID, PaneTitle: paneTitle,
				Ts: time.Now().UnixMilli(), Reason: "rate-limited",
			})
			break
		}
		emit(map[string]any{
			"type":        "needs_confirm",
			"id":          confirmID,
			"tool":        "resolve_permission",
			"summary":     fmt.Sprintf("Approve %s in %s", title, paneTitle),
			"pane_id":     paneID,
			"pane_title":  paneTitle,
			"request_id":  ap.RequestID,
			"tool_name":   ap.ToolName,
			"input":       preview,
			"tool_use_id": confirmID,
		})
		// Staleness: the approval may have been drained (turn_end) or resolved
		// (allowlist / manual) since the snapshot. resolveApproval is idempotent
		// by RequestID, but re-checking lets us emit a clean error so the
		// autoEmitter releases the reservation instead of recording a fire.
		if p.peekNativePending(ap.RequestID) == nil {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": ap.RequestID, "approved": true, "error": "approval no longer pending"})
			continue
		}
		resolved, err := p.resolveAutomaticApprovalForPolicy(ap.RequestID, policy.policy)
		if err != nil || resolved == nil {
			errMsg := "approval already resolved"
			if err != nil {
				errMsg = err.Error()
			}
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": ap.RequestID, "approved": true, "error": errMsg})
			continue
		}
		// Allow frame landed. confirm_resolved with no error makes the
		// autoEmitter record the fire against the budget + stamp the pane.
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": ap.RequestID, "approved": true})
		a.recordEvalOutcome(AriaEvalOutcome{
			PaneID:       paneID,
			Kind:         "fired",
			TailSnapshot: strings.TrimSpace(ap.ToolName + " " + preview),
			Model:        "direct-native",
			LatencyMs:    time.Since(start).Milliseconds(),
			SendInput:    "approve " + ap.ToolName,
		})
		log.Printf("aria-auto-native-direct: pane=%s approved tool=%s req=%s", paneID, ap.ToolName, ap.RequestID)
		firedAny = true
	}
	emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})

	// Sweeper-backoff streak: a fire means the pane is advancing; an eligible
	// approval that didn't fire (stale, or write failed) is a "no action" so a
	// pane ARIA can never act on eventually backs off. A batch whose ONLY
	// pending entries are user-questions is left untouched — the human must
	// answer those, so backing off ARIA over them would be wrong (matches
	// evaluateNativeObservation, which returns without recording in that case).
	if firedAny {
		a.recordAutoEvalOutcome(paneID, true)
	} else if eligibleAny {
		a.recordAutoEvalOutcome(paneID, false)
	}
}

// evaluateNativeObservation is the LLM decision path for native panes in
// cautious mode or with a user standing order. It evaluates each eligible
// pending approval in its own turn (one bound RequestID per turn — the
// parallel-tool-call safety rule) and lets the model approve via
// resolve_permission or decline by replying.
//
// Caller holds the per-pane in-flight eval lock.
func (a *Aria) evaluateNativeObservation(ctx context.Context, paneID, paneTitle string, p *Pane, instruction, orderID string, countdownMs int, cautious bool) {
	if p == nil {
		return
	}
	if a.completionRefused(p) {
		return
	}
	ctx, _ = withAriaAutoModePolicy(ctx, p)
	// Cap / pause gate up front so a paused or capped ARIA never burns an LLM
	// call. (Per-turn budget is still enforced again at confirm time.)
	if allowed, capReason := a.autoAllowed(); !allowed {
		a.uiBus.publish(uiEvent{
			Type: "aria-auto-suppressed", PaneID: paneID, PaneTitle: paneTitle,
			Ts: time.Now().UnixMilli(), Reason: autoSuppressReason(capReason),
		})
		return
	}
	pending := p.listNativePending()
	firedAny := false
	evaluatedAny := false
	for _, ap := range pending {
		if ap == nil || ap.RequestID == "" {
			continue
		}
		if isNonAutoApprovableNativeTool(ap.ToolName) {
			a.recordEvalOutcomeDedup(AriaEvalOutcome{
				PaneID:       paneID,
				Kind:         "skipped",
				Reason:       nativeNeverAutoReason(ap.ToolName),
				TailSnapshot: strings.TrimSpace(ap.ToolName + " " + nativeApprovalInputPreview(ap.RawInput)),
				Model:        "cautious-native",
			})
			continue
		}
		// Re-check the budget before each turn — a prior turn in this batch may
		// have fired and consumed the burst budget.
		if allowed, _ := a.autoAllowed(); !allowed {
			break
		}
		fired, evaluated := a.runNativeApprovalTurn(ctx, paneID, paneTitle, ap, instruction, orderID, countdownMs, cautious)
		evaluatedAny = evaluatedAny || evaluated
		if fired {
			firedAny = true
		}
	}
	if !evaluatedAny {
		return
	}
	if firedAny {
		a.recordAutoEvalOutcome(paneID, true)
	} else {
		a.recordAutoEvalOutcome(paneID, false)
	}
}

// runNativeApprovalTurn runs ONE LLM turn for ONE native approval, bound to its
// exact RequestID. Returns whether it fired and whether the provider evaluated
// it (local refusals are neutral). Mirrors
// evaluateObservation's emit-wrapper / outcome bookkeeping, but for a native
// pane: the model is offered only resolve_permission, and the daemon binds the
// reqID so the model never selects among parallel approvals.
func (a *Aria) runNativeApprovalTurn(ctx context.Context, paneID, paneTitle string, ap *NativeApproval, instruction, orderID string, countdownMs int, cautious bool) (bool, bool) {
	if ap == nil || ap.RequestID == "" {
		return false, false
	}
	start := time.Now()
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return false, false
	}
	ctx, approvalPolicy := withAriaAutoModePolicy(ctx, p)
	// Don't spend an LLM call on an approval that already vanished.
	if p.peekNativePending(ap.RequestID) == nil {
		return false, false
	}
	if a.nativeCompletionRefused(p, ap.RequestID) {
		return false, false
	}
	inputAllowed := completionStructuredInputAllowed(ap.RawInput)

	title, preview := nativeApprovalSummary(ap)
	policy := instruction
	if cautious {
		policy = ariaNativeAutoModeCautiousInstruction
	}
	countdownPhrase := fmt.Sprintf("%d-second cancel window", countdownMs/1000)
	if countdownMs <= 0 {
		countdownPhrase = "no cancel window"
	}
	// Wrap the (untrusted) tool args in the same <pane_output> boundary the
	// terminal path sanitises, so pane-controlled content can't break out and
	// inject instructions.
	var msg string
	if inputAllowed {
		safeInput := sanitizePromptWrapper(strings.TrimSpace(string(ap.RawInput)))
		msg = fmt.Sprintf(ariaNativeObservationTemplate,
			paneTitle, paneID, ap.ToolName, safeInput, policy, countdownPhrase)
	}

	convID := fmt.Sprintf("auto_native_%s", paneID)
	innerEmit := a.autoEmitter(paneID, paneTitle, orderID, countdownMs)
	var (
		fired         bool
		assistantText strings.Builder
		toolCalls     int
	)
	emit := func(evt map[string]any) {
		switch t, _ := evt["type"].(string); t {
		case "confirm_resolved":
			if approved, _ := evt["approved"].(bool); approved {
				if _, hadErr := evt["error"]; !hadErr {
					fired = true
				}
			}
		case "delta":
			if s, _ := evt["text"].(string); s != "" {
				assistantText.WriteString(s)
			}
		case "tool_call":
			toolCalls++
		}
		innerEmit(evt)
	}

	emit(map[string]any{
		"type":        "auto-eval-start",
		"pane_id":     paneID,
		"pane_title":  paneTitle,
		"order_id":    orderID,
		"instruction": "native approval: " + title,
	})

	req := ariaQueryReq{
		ConversationID:      convID,
		Message:             msg,
		Model:               a.settings.SweeperModel(),
		Autonomous:          true,
		AutonomousPaneID:    paneID,
		AutonomousCautious:  cautious,
		AutonomousNative:    true,
		AutonomousRequestID: ap.RequestID,
	}
	completionAllowed := teamCloudLLMAllowed()
	turnErr := errAriaCompletionInput
	if inputAllowed {
		turnErr = a.runTurn(ctx, req, emit)
	}
	if err := turnErr; err != nil {
		if errors.Is(err, errAriaCompletionInput) {
			a.rememberNativeCompletionRefusal(p, approvalPolicy, completionAllowed, ap.RequestID)
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: errAriaCompletionInput.Error(), Model: req.Model, LatencyMs: time.Since(start).Milliseconds()})
			return false, false
		}
		log.Printf("aria-auto-native: eval failed pane=%s req=%s: %v", paneID, ap.RequestID, err)
		emit(map[string]any{"type": "auto-eval-error", "pane_id": paneID, "pane_title": paneTitle, "error": err.Error()})
		kind := "error"
		if fired {
			kind = "fired"
		}
		a.recordEvalOutcome(AriaEvalOutcome{
			PaneID: paneID, Kind: kind, Reason: err.Error(),
			TailSnapshot: strings.TrimSpace(ap.ToolName + " " + preview),
			Model:        req.Model, LatencyMs: time.Since(start).Milliseconds(),
			ToolCalls: toolCalls, SendInput: "approve " + ap.ToolName,
		})
		return fired, true
	}
	emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})

	kind := "declined"
	reason := strings.TrimSpace(assistantText.String())
	sendInput := ""
	if fired {
		kind = "fired"
		reason = ""
		sendInput = "approve " + ap.ToolName
	}
	a.recordModelEvalOutcome(AriaEvalOutcome{
		PaneID: paneID, Kind: kind, Reason: reason,
		TailSnapshot: strings.TrimSpace(ap.ToolName + " " + preview),
		Model:        req.Model, LatencyMs: time.Since(start).Milliseconds(),
		ToolCalls: toolCalls, SendInput: sendInput,
	})
	return fired, true
}

// toolResolvePermission is the executor for the resolve_permission tool (the
// native-pane mirror of toolSendInput). The model only ever approves with it;
// it declines by not calling the tool. The pane + reqID are daemon-bound, so
// this resolves exactly the approval ARIA evaluated. cautious adds the
// deterministic deny gate as a backstop to the model's judgment.
func (a *Aria) toolResolvePermission(ctx context.Context, toolUseID, paneID, reqID string, cautious bool, emit func(map[string]any)) string {
	if strings.TrimSpace(paneID) == "" {
		return "error: no native pane is bound to this autonomous turn"
	}
	if strings.TrimSpace(reqID) == "" {
		return "error: no pending approval is bound to this autonomous turn"
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return fmt.Sprintf("error: no pane with id %q", paneID)
	}
	ctx, policy := withAriaAutoModePolicy(ctx, p)
	if !policy.matches(p) {
		return "skipped: auto-approval policy changed during evaluation"
	}
	ap := p.peekNativePending(reqID)
	if ap == nil {
		return "skipped: that permission request is no longer pending (already resolved, cancelled, or the turn ended). No action needed."
	}
	// Never auto-answer genuine user questions — they are not safety gates.
	if isNonAutoApprovableNativeTool(ap.ToolName) {
		return fmt.Sprintf("skipped: %q is a question for the user, not a permission gate. Left for the human to answer.", ap.ToolName)
	}
	// Deterministic safety net for cautious mode (belt-and-suspenders).
	if cautious {
		if reason := nativeCautiousAutoDenyReason(ap); reason != "" {
			log.Printf("aria: native cautious auto-approve pane=%s tool=%s denied: %s", paneID, ap.ToolName, reason)
			return "skipped: cautious auto-approval denied by daemon safety gate: " + reason + ". Left for the user."
		}
	}

	title, preview := nativeApprovalSummary(ap)
	confirmID, approved, auto, terminal := a.awaitConfirm(ctx, toolUseID, confirmOpts{
		Tool:        "resolve_permission",
		Summary:     fmt.Sprintf("Approve %s in %s", title, p.Title),
		Risk:        "medium",
		Extra:       map[string]any{"pane_id": paneID, "pane_title": p.Title, "request_id": reqID, "tool_name": ap.ToolName, "input": preview},
		AutoConfirm: true,
	}, emit)
	if terminal {
		return "error: resolve_permission did not run (cancelled, timed out, or stream closed)"
	}
	if !approved {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": reqID, "approved": false})
		return "the approval was cancelled by the user during the confirm window. Do not retry — they will handle it manually."
	}
	// Staleness re-check before the write. resolveApproval is itself idempotent
	// (takeNativePending), but re-checking lets us emit a clean confirm_resolved
	// that releases the budget reservation rather than recording a phantom fire.
	if p.peekNativePending(reqID) == nil {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": reqID, "approved": true, "auto": auto, "error": "approval no longer pending"})
		return "skipped: the approval was resolved by something else before ARIA could act. No action needed."
	}
	resolved, err := p.resolveAutomaticApprovalForPolicy(reqID, policy.policy)
	if err != nil {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": reqID, "approved": true, "auto": auto, "error": err.Error()})
		if errors.Is(err, errNoPendingApproval) {
			return "skipped: the approval was already resolved. No action needed."
		}
		return "error: failed to write the approval to the native agent: " + err.Error()
	}
	if resolved == nil {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": reqID, "approved": true, "auto": auto, "error": "approval no longer pending"})
		return "skipped: the approval was already resolved. No action needed."
	}
	emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "request_id": reqID, "approved": true, "auto": auto})
	return fmt.Sprintf("approved: allowed %s to run in %s.", ap.ToolName, p.Title)
}
