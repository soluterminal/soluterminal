package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Autonomous evaluator. When the pane-state watcher fires an
// observation for a pane with a standing order, we build a one-shot
// ARIA turn (no browser initiation) that gives the model the
// observation text + the order instruction and asks it to decide:
// either call send_input with the appropriate reply, or respond with
// text (meaning "no action — ping the user"). The model's tool_use
// flows through the normal toolSendInput → needs_confirm path, so
// the UI sees a confirm card with an auto-countdown and the user
// can cancel within the window.
//
// Separation of concerns: this file owns the "when to fire" and
// "how to frame the prompt" logic. The countdown itself lives on
// the confirm plumbing (see emitNeedsConfirm in aria_tools.go) so
// the UI doesn't care whether a confirm came from a manual chat
// turn or an autonomous trigger; only the auto_countdown_ms hint
// is different.

// ariaAutoCountdownMs is how long the UI shows the confirm card
// before auto-approving it. 5 seconds is the sweet spot from the
// scoping conversation: long enough to register + hit cancel, short
// enough that the pane isn't obviously sitting idle.
const ariaAutoCountdownMs = 5000

// ariaAutoEvalMaxDuration bounds how long a single autonomous
// evaluation can hold the per-pane in-flight slot. Sized to cover a
// realistic worst case (slow first-token + tool loop + 5s confirm
// countdown ≈ 60-90s) while still releasing the slot when the cloud
// stalls for minutes — a stuck eval used to lock the pane out of
// future evals indefinitely. Anthropic's own request ceiling is
// ~10min, so 90s is the "we know this isn't progressing" bar.
const ariaAutoEvalMaxDuration = 90 * time.Second

// ariaAutoModeAlwaysInstruction is the synthesised standing order
// the evaluator runs with when a pane is set to AutoMode "always".
// Phrased to encourage decisive action — the user has explicitly
// opted into "approve everything", so equivocation isn't useful.
//
// IMPORTANT: only answer real TUI option prompts. The upstream watcher
// already restricts us to StateChoicePrompt (numbered menu shown in
// the TUI), but belt-and-suspenders: if the tail shows Claude finishing
// a reply with a prose question ("Would you like me to continue?") and
// no numbered menu, DO NOT auto-send — respond with a one-line "no
// prompt to answer" instead.
const ariaAutoModeAlwaysInstruction = `Auto-approve the numbered option menu shown in this pane. Call send_input with: '1' (or the option the TUI has highlighted with ❯/›).

Disambiguation rules — these are the cases that have stuck the autoresponder before, so apply them rather than declining:
  • If multiple identical-looking menus are stacked (e.g. two "Do you want to proceed?" blocks), the BOTTOM-MOST one is the active prompt; earlier copies are scrollback. Answer the bottom one.
  • If an option label is visually mangled (Claude Code occasionally interleaves the long command text through the option-2 label, or scrollback noise like stray digits / partial words appears between options), trust the ❯/› cursor placement over label readability. The cursor position is reliable; the rendering is not.
  • If the cursor is unambiguously on an option (❯/› visible on exactly one line), send that option's number even if the surrounding text is noisy.

Do NOT call send_input if the tail is just Claude finishing a reply with a prose question like "Would you like me to continue?", "Should I proceed with X?", or an unfocused y/n phrase — those are not the numbered TUI option menus this mode currently handles. In that case, reply with one short sentence ("No actionable numbered prompt.") and do not call any tools.

The user has opted into always-approve mode for real TUI prompts — be decisive there, but never invent a prompt that isn't on screen.`

// ariaAutoModeCautiousInstruction is the synthesised standing order
// for AutoMode "cautious". Spells out a concrete risk taxonomy so
// the model isn't doing vibes-based classification — anything matching
// the DO NOT list bounces back to the user with a one-line reason.
const ariaAutoModeCautiousInstruction = `Approve this prompt ONLY if (1) the tail shows an actual TUI option menu (numbered list with ❯/› focus marker), AND (2) the prompt is clearly low-risk.

Prose questions or unfocused y/n text embedded in Claude's reply ("Would you like me to continue?", "Should I proceed?") are NOT the numbered prompts auto mode currently handles — ignore them and respond with one line ("No actionable numbered prompt.").

Disambiguation: if multiple identical menus are stacked, treat the BOTTOM-MOST one as active (earlier copies are scrollback). If option labels are mangled by Claude Code interleaving command text, trust the ❯/› cursor position — but still apply the risk taxonomy below to the underlying command before approving.

LOW-RISK = read-only operations (file reads, status checks, listing, inspecting state), formatting / linting, displaying information, accepting suggestions for documentation or code comments, and simple continuations of work the user clearly initiated AS A NUMBERED TUI PROMPT.

DO NOT auto-approve if the prompt asks to:
  (a) write, modify, or delete files OUTSIDE the current repo (system paths, dotfiles, other projects).
  (b) run shell commands that change system state, install packages, modify config, or hit network endpoints not previously used in this session.
  (c) push to git, force-push, rewrite history, merge to main/master, delete branches, or interact with remote repos.
  (d) read, write, or transmit secrets, .env files, credentials, tokens, API keys, or auth config.
  (e) anything you are not sure about — when in doubt, leave it for the user.

Decision: if low-risk AND a real TUI prompt, call send_input with the appropriate reply. Otherwise, respond with ONE short sentence stating why (e.g. "Deferring — this writes to ~/.ssh which is rule (d)." or "No actionable prompt — prose question only."). The user will handle it manually.`

type ariaAutoModePolicyKey struct{}

type ariaAutoModePolicy struct {
	pane   *Pane
	policy paneAutoModePolicy
}

func withAriaAutoModePolicy(ctx context.Context, p *Pane) (context.Context, ariaAutoModePolicy) {
	if policy, ok := ctx.Value(ariaAutoModePolicyKey{}).(ariaAutoModePolicy); ok {
		return ctx, policy
	}
	policy := ariaAutoModePolicy{pane: p, policy: p.autoModePolicy()}
	return context.WithValue(ctx, ariaAutoModePolicyKey{}, policy), policy
}

func (policy ariaAutoModePolicy) matches(p *Pane) bool {
	return policy.pane == p && policy.policy.matches(p)
}

// Caller holds p.mu, as part of the final guarded Jev write.
func ariaAutoModePolicyMatchesLocked(ctx context.Context, p *Pane) bool {
	policy, ok := ctx.Value(ariaAutoModePolicyKey{}).(ariaAutoModePolicy)
	return !ok || (policy.pane == p && policy.policy.matchesLocked(p))
}

// evaluateObservation is invoked asynchronously by the watcher. Preserve the
// policy captured here through provider calls and the confirmation countdown.
func (a *Aria) evaluateObservation(ctx context.Context, paneID, paneTitle, observationText string) {
	start := time.Now()
	// Per-pane in-flight lock. The watcher fires evals on transitions,
	// AND the 5s recovery sweeper fires on stuck choice-prompts —
	// without dedup, both can race. Two parallel evals on the same
	// menu would write "1\n" twice; the second lands after Claude has
	// processed the first and would submit "1" as a free-form message
	// in the next idle prompt. Skip-and-log is preferable to queueing
	// — by the time the in-flight eval finishes, the pane state may
	// have advanced anyway, so a second eval would be evaluating stale
	// context.
	if !a.tryClaimEval(paneID) {
		if a.metrics != nil {
			a.metrics.AriaEvalSkippedTotal.Add(1)
			a.metrics.IncLabelled("solud_aria_eval_outcome_total", map[string]string{"outcome": "skipped"})
		}
		log.Printf("aria-auto: pane=%s skipping — eval already in-flight", paneID)
		return
	}
	defer a.releaseEval(paneID)
	if a.metrics != nil {
		a.metrics.AriaEvalsTotal.Add(1)
	}

	// Hard ceiling on this eval's duration. If the cloud stalls or the
	// model loops, we want the per-pane in-flight slot back so the next
	// state transition can re-evaluate instead of being silently
	// dropped at tryClaimEval. Cancel on the way out either way.
	ctx, cancel := context.WithTimeout(ctx, ariaAutoEvalMaxDuration)
	defer cancel()

	var (
		instruction string
		orderID     string
		countdownMs = ariaAutoCountdownMs
	)
	// Native-renderer panes answer permission requests with a structured
	// control_response frame, not PTY keystrokes — they need a dedicated
	// decision path (resolve_permission + the parked NativeApproval). The
	// aria-watcher fires the same for native and terminal panes (both report
	// StateChoicePrompt), but the terminal grid-scrape machinery
	// (directAutoFire / send_input) cannot act on a native pane, so detect the
	// kind up front and branch below.
	p, found := a.mgr.Get(paneID)
	if !found {
		return
	}
	if p.Snapshot().NeverAutoAnswer {
		return
	}
	ctx, policy := withAriaAutoModePolicy(ctx, p)
	isNative := p.Snapshot().Kind == "native"
	if !isNative && p.Snapshot().AutoMode == "cautious" {
		observationText = cautiousPromptText(p, observationText)
	}
	if !isNative && p.Snapshot().AutoMode == "cautious" && a.cautiousPromptCancelled(p) {
		return
	}
	if isNative && !nativeAriaAutoEnabled() {
		// Kill-switch off: leave native panes for the human (pre-wiring
		// behaviour). Returning here also keeps a native pane from falling
		// through to the terminal grid/PTY path, which can't act on it.
		return
	}
	if !isNative && a.tryJevAmbiguousTerminal(ctx, p, paneID, paneTitle, start) {
		return
	}
	switch p.Snapshot().AutoMode {
	case "always":
		// Jev gets the first decision on eligible terminal permissions above.
		// Recognized prompts still progress when Jev abstains, is unavailable,
		// or cannot receive the screen text. Native requests stay structured.
		if isNative {
			a.directAutoFireNative(ctx, paneID, paneTitle)
		} else {
			a.directAutoFire(ctx, paneID, paneTitle)
		}
		return
	case "cautious":
		instruction = ariaAutoModeCautiousInstruction
		orderID = "automode_cautious"
	default:
		return
	}

	// Native LLM path (cautious mode): a dedicated handler evaluates
	// each parked approval by its exact RequestID and resolves via
	// resolve_permission. The terminal grid/PTY machinery below does not
	// apply to native panes. Non-team installs can't reach that model, so
	// their native permissions stay with the human.
	if isNative && a.cautiousJevOnly() {
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "declined", Reason: jevCautiousReason("native_fallback"), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	if isNative {
		a.evaluateNativeObservation(ctx, paneID, paneTitle, p, instruction, orderID, countdownMs, orderID == "automode_cautious")
		return
	}

	// Safety gates: paused flag + per-second burst cap. autoAllowed is a
	// pure check — a fire is only logged when a send actually lands (see
	// autoRecordFire in autoEmitter's confirm_resolved handler).
	allowed, capReason := a.autoAllowed()
	if !allowed {
		if a.metrics != nil {
			a.metrics.AriaEvalSkippedTotal.Add(1)
			a.metrics.IncLabelled("solud_aria_eval_outcome_total", map[string]string{"outcome": "skipped"})
		}
		reason := autoSuppressReason(capReason)
		switch capReason {
		case "paused":
			log.Printf("aria-auto: paused, skipping eval for pane=%s", paneID)
		case "burst-cap":
			log.Printf("aria-auto: burst-limited (>=%d/sec), skipping eval for pane=%s", ariaAutoMaxPerSecond, paneID)
		default:
			log.Printf("aria-auto: gate %q, skipping eval for pane=%s", capReason, paneID)
		}
		a.recordEvalOutcome(AriaEvalOutcome{
			PaneID:       paneID,
			Kind:         "skipped_backoff",
			Reason:       reason,
			TailSnapshot: observationText,
			LatencyMs:    time.Since(start).Milliseconds(),
		})
		// Surface to the UI so the user knows an auto-eval was
		// suppressed. Quiet — no modal, just a line in the panel. Reason
		// rides along so the panel can show why (burst vs cap vs pause).
		a.uiBus.publish(uiEvent{
			Type:      "aria-auto-suppressed",
			PaneID:    paneID,
			PaneTitle: paneTitle,
			Ts:        time.Now().UnixMilli(),
			Reason:    reason,
		})
		return
	}

	// Build the message ARIA sees. The system prompt stays the same
	// (she already knows her tools); what we inject here is the
	// situation + the standing order + explicit guidance to either
	// act via send_input or explain the non-action. The countdown
	// phrasing is dynamic because "always" mode skips the cancel
	// window entirely — don't promise a 5s grace that won't exist.
	// User-input hold, checked BEFORE the turn. toolSendInput holds too, but
	// that is the race backstop and it fires only after the model has already
	// answered: a held pane would otherwise cost one sweeper LLM turn every 5s
	// for as long as the user keeps typing. Nothing bounds that, either: the
	// burst cap counts fires, not evaluations.
	if hold, holdReason := autoFireUserHold(p, time.Now()); hold {
		log.Printf("aria-auto: pane=%s held before eval (%s)", paneID, holdReason)
		// No tail: recordDedup keys on Kind + TailSnapshot, and the tail here
		// contains the line the user is actively retyping, so a per-tick tail
		// would defeat the dedup and evict real history from the 5-slot ring.
		a.recordEvalOutcomeDedup(AriaEvalOutcome{
			PaneID:    paneID,
			Kind:      "held",
			Reason:    holdReason,
			Model:     "direct",
			LatencyMs: time.Since(start).Milliseconds(),
		})
		// Streak untouched: the menu is real and still pending.
		return
	}
	// Non-team installs: Jev is the only cautious evaluator, and anything
	// it doesn't approve stays with the human (never the team ARIA model).
	if a.cautiousJevOnly() {
		a.evaluateCautiousWithJev(ctx, p, paneID, paneTitle, observationText, start)
		return
	}
	// The opt-in Jev fast path handles only high-confidence, low-risk
	// terminal permission menus. All other cases continue through the
	// incumbent ARIA turn below.
	if a.tryJevCautiousTerminal(ctx, p, paneID, paneTitle, observationText, start) {
		return
	}

	countdownPhrase := fmt.Sprintf("%d-second cancel window", countdownMs/1000)
	if countdownMs <= 0 {
		countdownPhrase = "no cancel window (always-approve mode)"
	}
	// Prevent pane-controlled output from closing the wrapper tag and
	// injecting instructions outside the <pane_output> boundary.
	safeObservation := sanitizePromptWrapper(observationText)
	msg := fmt.Sprintf(`[AUTONOMOUS EVALUATION — you were invoked by the pane-state watcher, not by the user.]

Pane %q (id: %s) just entered awaiting-input. Here is its current tail:

<pane_output>
%s
</pane_output>

The user has set this standing order for this pane:
"%s"

Decide:
1. If the standing order clearly covers THIS prompt, call send_input with the appropriate reply. The user will see a %s before it actually fires.
2. If the order does NOT cover this prompt (wrong type, outside scope, ambiguous), respond with a single short sentence explaining why, and DO NOT call any tools. The user still has the observation card and will handle it manually.

CRITICAL — send_input newline:
- The 'input' arg MUST end with '\n' to actually press Enter and submit. Bare '1' types the digit but Claude Code never sees it because Enter wasn't pressed.
- For a numbered menu, pass input='1\n' (or whatever the choice is). For y/n, pass input='y\n'. The trailing newline is mandatory for ANY reply you intend to submit.

Be decisive. Don't narrate. Don't call read_pane_tail — the full tail is already above.`,
		paneTitle, paneID, safeObservation, instruction, countdownPhrase)

	// Run the turn via the same machinery browser-initiated turns use.
	// Conversation id is dedicated per-pane and reset each eval so
	// context doesn't grow; each firing is an independent decision.
	convID := fmt.Sprintf("auto_pane_%s", paneID)
	innerEmit := a.autoEmitter(paneID, paneTitle, orderID, countdownMs)
	promptPos := int64(0)
	promptHash := choicePromptFingerprint(observationText)
	if p, ok := a.mgr.Get(paneID); ok {
		if _, pos, fp, ok := currentChoicePromptIdentity(p, observationText); ok {
			promptPos = pos
			if fp != 0 {
				promptHash = fp
			}
		} else {
			promptPos = atomic.LoadInt64(&p.LastAckedChoicePromptPos)
		}
	}

	// Wrap the emitter so we can detect whether a send_input actually
	// landed during this eval. confirm_resolved with approved=true and
	// no error is the only signal that ARIA acted — anything else
	// (model declined, tool errored, runTurn failed) counts as
	// "no action" toward the sweeper-backoff streak.
	var (
		fired         bool
		held          bool
		heldReason    string
		firedInput    string
		assistantText strings.Builder
		toolCalls     int
	)
	emit := func(evt map[string]any) {
		switch t, _ := evt["type"].(string); t {
		case "auto-hold":
			// toolSendInput declined to write because the user is mid-input.
			// Not a policy decline: the menu is still pending, so this must
			// not reach the sweeper-suspend streak (see the routing below).
			held = true
			heldReason, _ = evt["reason"].(string)
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
			if name, _ := evt["name"].(string); name == "send_input" {
				if raw, ok := evt["args"].(json.RawMessage); ok {
					var args struct {
						Input string `json:"input"`
					}
					if err := json.Unmarshal(raw, &args); err == nil {
						firedInput = renderControlChars(args.Input)
					}
				}
			}
		}
		innerEmit(evt)
	}

	// Stamp start so the UI can show "ARIA is evaluating…".
	emit(map[string]any{
		"type":        "auto-eval-start",
		"pane_id":     paneID,
		"pane_title":  paneTitle,
		"order_id":    orderID,
		"instruction": instruction,
	})

	req := ariaQueryReq{
		ConversationID:       convID,
		Message:              msg,
		Model:                a.settings.SweeperModel(),
		Autonomous:           true,
		AutonomousPaneID:     paneID,
		AutonomousPromptPos:  promptPos,
		AutonomousPromptHash: promptHash,
		AutonomousCautious:   orderID == "automode_cautious",
	}
	turnErr := a.runTurn(ctx, req, emit)
	current, currentExists := a.mgr.Get(paneID)
	if !fired && (!currentExists || current != p || !policy.matches(p)) {
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		a.recordEvalOutcomeDedup(AriaEvalOutcome{PaneID: paneID, Kind: "held", Reason: "approval policy changed", Model: req.Model, LatencyMs: time.Since(start).Milliseconds(), ToolCalls: toolCalls})
		return
	}
	if err := turnErr; err != nil {
		if a.metrics != nil {
			a.metrics.AriaEvalErrorsTotal.Add(1)
			a.metrics.IncLabelled("solud_aria_eval_outcome_total", map[string]string{"outcome": "error"})
		}
		log.Printf("aria-auto: eval failed pane=%s: %v", paneID, err)
		emit(map[string]any{
			"type":       "auto-eval-error",
			"pane_id":    paneID,
			"pane_title": paneTitle,
			"error":      err.Error(),
		})
		// Record `fired` honestly — when iteration 1 of the tool loop
		// already executed send_input (confirm_resolved → fired=true)
		// and a later iteration errors (e.g. provider rejects the
		// follow-up turn), the user-visible action DID happen. Counting
		// it as a non-fire here would back off the sweeper for a pane
		// that's been advancing fine.
		a.recordAutoEvalOutcome(paneID, fired)
		kind := "error"
		if fired {
			kind = "fired"
		}
		a.recordEvalOutcome(AriaEvalOutcome{
			PaneID:       paneID,
			Kind:         kind,
			Reason:       err.Error(),
			TailSnapshot: observationText,
			Model:        req.Model,
			LatencyMs:    time.Since(start).Milliseconds(),
			ToolCalls:    toolCalls,
			SendInput:    firedInput,
		})
		return
	}
	emit(map[string]any{
		"type":       "auto-eval-done",
		"pane_id":    paneID,
		"pane_title": paneTitle,
	})
	// Route the outcome toward the sweeper-suspend streak. A non-fire
	// normally counts as a "no action" and bumps the streak — but if the
	// menu the watcher detected wasn't even present in the tail we handed
	// the model, the decline is a detection/eval-tail mismatch on our side
	// (a multi-subagent burst buried the menu past the eval window), not a
	// policy decision. Counting it would let a transient output flood park
	// the pane after 3 misses; instead record it neutrally and retry.
	switch {
	case fired:
		a.recordAutoEvalOutcome(paneID, true)
	case held:
		// Streak untouched, same as a tail-window miss: the menu is real and
		// still pending, so 3 holds in a row must not park the pane.
	case a.evalMenuWasSurfaced(paneID, observationText):
		a.recordAutoEvalOutcome(paneID, false)
	default:
		a.recordAutoEvalNoSurface(paneID)
	}
	kind := "declined"
	reason := strings.TrimSpace(assistantText.String())
	if fired {
		kind = "fired"
		reason = ""
	} else if held {
		// Deduped and tail-less, matching the pre-turn hold above: the tail is
		// the line being retyped, so keeping it would defeat the dedup.
		a.recordEvalOutcomeDedup(AriaEvalOutcome{
			PaneID:    paneID,
			Kind:      "held",
			Reason:    heldReason,
			Model:     req.Model,
			LatencyMs: time.Since(start).Milliseconds(),
			ToolCalls: toolCalls,
		})
		return
	}
	a.recordEvalOutcome(AriaEvalOutcome{
		PaneID:       paneID,
		Kind:         kind,
		Reason:       reason,
		TailSnapshot: observationText,
		Model:        req.Model,
		LatencyMs:    time.Since(start).Milliseconds(),
		ToolCalls:    toolCalls,
		SendInput:    firedInput,
	})
}

// evalMenuWasSurfaced reports whether the approval menu the watcher
// detected (the StateChoicePrompt that triggered this eval) was actually
// present in the tail handed to the model. It re-runs the same matcher
// computeState uses (approvalPromptMatches) over the observation text. A
// false result means a non-fire is a tail-window miss on our side, not a
// policy decline — see the outcome routing in evaluateObservation and
// recordAutoEvalNoSurface.
func (a *Aria) evalMenuWasSurfaced(paneID, observationText string) bool {
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return false
	}
	return len(approvalPromptMatches(observationText, p.HasCodexDescendant() || p.HasMuseDescendant())) > 0
}

// autoEmitter returns an emit function for runTurn that routes events
// to the uiBus instead of an HTTP SSE stream. Browser clients pick
// them up via /api/ui/events and render them in the ARIA panel's
// "autoresponder" strip.
//
// Translation rules:
//   - text deltas → aria-auto-delta (accumulated client-side)
//   - needs_confirm → aria-auto-needs-confirm (tagged with auto-countdown)
//   - confirm_resolved → aria-auto-confirm-resolved
//   - tool_call / tool_result → dropped; the ARIA panel doesn't
//     surface tool chips for manual turns, no reason to for auto.
//   - error / done → aria-auto-error / aria-auto-done
func (a *Aria) autoEmitter(paneID, paneTitle, orderID string, countdownMs int) func(map[string]any) {
	return func(evt map[string]any) {
		t, _ := evt["type"].(string)
		switch t {
		case "delta":
			text, _ := evt["text"].(string)
			a.uiBus.publish(uiEvent{
				Type:      "aria-auto-delta",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
				Text:      text,
			})
		case "needs_confirm":
			tool, _ := evt["tool"].(string)
			evtPaneID, _ := evt["pane_id"].(string)
			// Native-renderer panes confirm via a structured "resolve_permission"
			// tool instead of the byte-scraped "send_input"; both are valid
			// autonomous confirms for the target pane.
			if (tool != "send_input" && tool != "resolve_permission") || evtPaneID != paneID {
				log.Printf("aria-auto: refusing auto-confirm for tool=%q pane=%q target=%q", tool, evtPaneID, paneID)
				if confirmID, _ := evt["id"].(string); confirmID != "" {
					_ = a.signalAutoConfirm(confirmID, false)
				}
				a.uiBus.publish(uiEvent{
					Type:      "aria-auto-error",
					PaneID:    paneID,
					PaneTitle: paneTitle,
					Ts:        time.Now().UnixMilli(),
					Text:      "refused unsafe autonomous tool request",
				})
				return
			}
			// Re-emit as aria-auto-needs-confirm so the UI routes it
			// to the autoresponder strip (countdown visible) instead
			// of the in-chat confirm-card slot. The id is the same
			// confirm id the daemon already registered, so clicking
			// cancel on the auto card still POSTs to /api/aria/confirm
			// and resolves the parked tool executor.
			//
			// countdownMs of 0 means "auto-fire immediately" — used by
			// AutoMode "always".
			out := map[string]any{
				"type":              "aria-auto-needs-confirm",
				"pane_id":           paneID,
				"pane_title":        paneTitle,
				"order_id":          orderID,
				"auto_countdown_ms": countdownMs,
				"ts":                time.Now().UnixMilli(),
			}
			for k, v := range evt {
				if k == "type" {
					continue
				}
				out[k] = v
			}
			// Server-side auto-approve timer. Auto-approval used to be
			// driven by a browser setInterval in AriaPanel.tsx, which
			// meant it silently stopped firing when the Tauri webview
			// was backgrounded / minimized / memory-trimmed by WebView2.
			// Run the timer in-daemon so approval is independent of the
			// window having focus. If the user hits cancel first,
			// signalConfirm consumes the pending entry and our later
			// AfterFunc call is a no-op (signalConfirm returns false
			// for an unknown id). once.Do on the pending entry makes
			// the race harmless either way.
			confirmID, _ := evt["id"].(string)
			delay := time.Duration(countdownMs) * time.Millisecond
			if confirmID != "" {
				deadline := time.Now().Add(delay)
				out["deadline_at_ms"] = deadline.UnixMilli()
				a.rememberAutoPendingConfirm(ariaAutoPendingConfirm{
					ID:              confirmID,
					PaneID:          paneID,
					PaneTitle:       paneTitle,
					Input:           stringFromAny(out["input"]),
					OrderID:         orderID,
					AutoCountdownMs: countdownMs,
					DeadlineAtMs:    deadline.UnixMilli(),
				})
			}
			payload, _ := json.Marshal(out)
			a.uiBus.publish(uiEvent{
				Type:      "aria-auto-needs-confirm",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
				Text:      string(payload),
			})
			if confirmID != "" {
				time.AfterFunc(delay, func() {
					if !a.tryReserveAutoFireForConfirm(confirmID) {
						_ = a.signalAutoConfirm(confirmID, false)
						return
					}
					if !a.signalConfirm(confirmID, true, false) {
						a.releaseAutoFireReservation(confirmID)
					}
				})
			}
		case "confirm_resolved":
			// Record the fire in the rolling-hour log only when
			// the send actually landed (approved AND no error). The
			// cap exists to bound real-world blast radius; cancels
			// and failures don't count. Also stamp the pane's
			// AutoModeLastFiredMs so the panel can render
			// "fired Xs ago" on the auto-mode strip.
			confirmID, _ := evt["id"].(string)
			a.forgetAutoPendingConfirm(confirmID)
			if approved, _ := evt["approved"].(bool); approved {
				if _, hadErr := evt["error"]; !hadErr {
					a.autoRecordFire(confirmID)
					if p, ok := a.mgr.Get(paneID); ok {
						p.MarkAutoModeFired()
					}
				} else {
					a.releaseAutoFireReservation(confirmID)
				}
			} else {
				a.releaseAutoFireReservation(confirmID)
			}
			payload, _ := json.Marshal(evt)
			a.uiBus.publish(uiEvent{
				Type:      "aria-auto-confirm-resolved",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
				Text:      string(payload),
			})
		case "error":
			msg, _ := evt["message"].(string)
			a.uiBus.publish(uiEvent{
				Type:      "aria-auto-error",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
				Text:      msg,
			})
		case "done":
			a.uiBus.publish(uiEvent{
				Type:      "aria-auto-done",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
			})
		case "auto-eval-start", "auto-eval-error", "auto-eval-done":
			// Pass these diagnostic events through with the payload
			// inlined as JSON so the UI can optionally show them.
			payload, _ := json.Marshal(evt)
			a.uiBus.publish(uiEvent{
				Type:      "aria-" + t,
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Ts:        time.Now().UnixMilli(),
				Text:      string(payload),
			})
			// tool_call / tool_result: intentionally dropped. Auto
			// turns don't need workflow chips in the UI.
		}
	}
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

// sanitizePromptWrapper neutralises any XML wrapper tag the autonomous
// prompt uses (currently <pane_output>), case-insensitively, so untrusted
// pane content can't break out of the boundary and inject instructions.
func sanitizePromptWrapper(s string) string {
	for _, tag := range []string{"</pane_output", "<pane_output"} {
		lowerTag := strings.ToLower(tag)
		var b strings.Builder
		b.Grow(len(s))
		i := 0
		for i < len(s) {
			if i+len(tag) <= len(s) && strings.EqualFold(s[i:i+len(tag)], lowerTag) {
				// Replace leading '<' with '<\' to break the tag while
				// keeping the text human-readable.
				b.WriteString("<\\")
				b.WriteString(s[i+1 : i+len(tag)])
				i += len(tag)
				continue
			}
			b.WriteByte(s[i])
			i++
		}
		s = b.String()
	}
	return s
}

// Use the same rendered menu as currentChoicePromptIdentity. Cell-diff TUIs
// can leave a fragmented or older menu in raw scrollback; classifying one
// source and confirming against the other leaves safe permissions stuck.
// A rendered choice question is authoritative too: the deny rules must see
// it instead of falling back to an older permission in the raw tail.
func cautiousPromptText(p *Pane, fallback string) string {
	text, _ := cautiousPromptSnapshot(p, fallback)
	return text
}

func cautiousPromptSnapshot(p *Pane, fallback string) (string, bool) {
	if p != nil {
		hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
		if text, visible := p.gridVisibleText(); visible && isGridChoicePromptText(text, hasCodex) {
			return cautiousCurrentDialogText(text, hasCodex), true
		}
	}
	return fallback, false
}

// Terminal TUIs leave earlier command results above their permission modal. Those
// results are not the action being granted. Trim them only when a complete,
// unambiguous permission heading identifies the current dialog; unknown,
// nested or clipped shapes retain the full screen and its conservative holds.
func cautiousCurrentDialogText(text string, hasCodex bool) string {
	if !isAutoApprovalPromptText(text, hasCodex) {
		return text
	}
	choices := choicePromptMatches(text)
	if len(choices) == 0 {
		return text
	}
	focus := choices[len(choices)-1][0]
	menu := parseAutoApprovalMenu(text, focus)
	if len(menu.options) == 0 {
		return text
	}
	start, contextStart, pendingStart, offset := -1, -1, -1, 0
	for _, line := range strings.Split(text[:focus], "\n") {
		trimmed := strings.TrimSpace(line)
		kind := ""
		switch trimmed {
		case "Would you like to run the following command?", "Would you like to make the following edits?":
			kind = "codex"
		case "Bash command", "Read file", "Edit file", "Write file", "Create file", "Tool use", "Fetch":
			kind = "claude"
		}
		if kind != "" {
			if start >= 0 {
				return text // A payload can quote another modal; never hide its prefix.
			}
			if kind == "codex" && (!hasCodex || menu.options[0] != "yes, proceed") {
				return text
			}
			contextStart = offset
			if kind == "codex" {
				indent := line[:strings.Index(line, trimmed)]
				if indent != "" && indent != "  " {
					return text
				}
				if strings.TrimSpace(text[:offset]) != "" {
					if pendingStart < 0 {
						return text // Continuation text cannot establish a new modal.
					}
					contextStart = pendingStart // Keep the actual pending command too.
				}
			}
			if kind == "claude" {
				indent := line[:strings.Index(line, trimmed)]
				if hasCodex || menu.options[0] != "yes" || (indent != "" && indent != " ") {
					return text // Payload indentation is not a top-level Claude heading.
				}
			}
			start = offset
		} else {
			if start < 0 {
				if strings.HasPrefix(line, "• Running ") {
					pendingStart = offset
				} else if trimmed != "" && !strings.HasPrefix(line, "  │ ") {
					pendingStart = -1
				}
			}
			if start < 0 && (strings.HasPrefix(trimmed, "$ ") || approvalDiffPayloadRE.MatchString(line) ||
				isPermissionQuestionLine(trimmed) || ariaJevActionHeaderRE.MatchString(trimmed) || ariaJevToolActionHeaderRE.MatchString(trimmed) ||
				ariaJevFullActionHeader(trimmed) || trimmed == "Would you like to grant these permissions?") {
				return text // The real opener may have scrolled off above this payload.
			}
		}
		offset += len(line) + 1
	}
	if start <= 0 || !cautiousRenderedHasActionContext(text[start:]) || cautiousSensitiveTail(text) {
		return text
	}
	// Retain the pending execution summary when present and everything from
	// the opener down: action, options, footers and later on-screen content.
	return text[contextStart:]
}

func currentAutoApprovalPromptText(p *Pane) (string, bool) {
	text, ok, _ := currentAutoApprovalPrompt(p)
	return text, ok
}

// currentAutoApprovalPrompt is currentAutoApprovalPromptText plus the source
// that supplied the menu: "grid" or "screen" (rendered), "tail" or "wide"
// (raw scrollback), or "" when none did.
func currentAutoApprovalPrompt(p *Pane) (string, bool, string) {
	if p == nil {
		return "", false, ""
	}
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
	p.ensureGrid()
	// A visible choice menu is authoritative even when it is NOT an
	// approvable permission. Falling through there would let an old permission
	// retained in scrollback authorize Enter on the current question instead.
	if gridText, ok := p.gridVisibleText(); ok && isGridChoicePromptText(gridText, hasCodex) {
		return gridText, isAutoApprovalPromptText(gridText, hasCodex), "grid"
	}
	// Replay-screen source: cell-diff TUIs (Muse) can leave the raw tail
	// unrecognizable while the always-on replay grid shows the menu
	// plainly — the same gap the computeState screen fallback closes.
	// Without this, admission (screen) and confirmation (tail) disagree
	// and directAutoFire declines "not an approval prompt".
	if screenText, ok := p.screenVisibleText(); ok && isGridChoicePromptText(screenText, hasCodex) {
		return screenText, isAutoApprovalPromptText(screenText, hasCodex), "screen"
	}
	tail := tailOfPaneAdaptive(p, 30)
	if isAutoApprovalPromptText(tail, hasCodex) {
		return tail, true, "tail"
	}
	// Wide-window fallback: match the approval-scan window computeState used
	// to ADMIT this pane to StateChoicePrompt. A multi-subagent burst can
	// stream enough post-menu progress lines to push the menu past the
	// 30-line adaptive tail above while the wide scan still finds it. Without
	// this, the admit gate (computeState, wide window) and this confirm gate
	// (narrow tail) disagree under a burst — the watcher fires but
	// directAutoFire declines "not an approval prompt", which used to bump
	// the suspend streak and park the pane. Keep the two gates in lockstep.
	if wide := approvalScanTail(p); wide != "" && isAutoApprovalPromptText(wide, hasCodex) {
		return wide, true, "wide"
	}
	return "", false, ""
}

// A failed recognition must explain the currently rendered menu. Raw tails
// can contain older prompts or cursor-motion fragments, so prefer a visible
// focused menu for diagnostics. This helper never authorizes a write.
func currentAutoApprovalDeclineReason(p *Pane) string {
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
	for _, snapshot := range []func() (string, bool){p.gridVisibleText, p.screenVisibleText} {
		if text, ok := snapshot(); ok && len(approvalPromptMatches(text, hasCodex)) > 0 {
			if reason := autoApprovalDeclineReason(text, hasCodex); reason != "" {
				return reason
			}
		}
	}
	if reason := autoApprovalDeclineReason(approvalScanTail(p), hasCodex); reason != "" {
		return reason
	}
	return "approval observation changed; retrying"
}

// approvalScanTail returns the same wide approval-scan window
// computeStateWithHasCodex uses to admit StateChoicePrompt: the trailing
// approvalScanChars of stripped, trailing-junk-trimmed output taken from the
// last 64 KB of the raw ring. Deliberately much wider than
// tailOfPaneAdaptive(p, 30) so directAutoFire's recheck sees the menu even
// when a subagent output flood has pushed it well behind the buffer tail.
func approvalScanTail(p *Pane) string {
	if p == nil {
		return ""
	}
	raw := p.buffer.SnapshotTail(65536)
	if len(raw) == 0 {
		return ""
	}
	s := stripANSI(raw)
	end := trimTrailingStatusJunk(s)
	start := 0
	if end > approvalScanChars {
		start = end - approvalScanChars
	}
	if start > end {
		return ""
	}
	return s[start:end]
}

// recordDirectDecline routes a directAutoFire "not an approval prompt"
// outcome toward the right bucket. If an approval-style menu IS still present
// in the wide scan window, this is a genuine generic-menu decline and bumps
// the sweeper-suspend streak (so we stop hammering a menu we'll never
// answer). If NO menu is present, the menu the watcher detected vanished or
// got buried by a subagent burst between admit and recheck — a transient
// surfacing miss, recorded neutrally so 3 of them in a row can't park a pane
// that has a real, still-pending menu. Mirrors evaluateObservation's
// surfaced-vs-not-surfaced split, which directAutoFire previously lacked.
func (a *Aria) recordDirectDecline(paneID string, p *Pane) {
	if a.evalMenuWasSurfaced(paneID, approvalScanTail(p)) {
		a.recordAutoEvalOutcome(paneID, false)
	} else {
		a.recordAutoEvalNoSurface(paneID)
	}
}

// Numeric chrome is ignored only in recognized status rows. Masking every
// digit also hid changes to command arguments, ports, and file names.
var autoFireNumericStatusRE = regexp.MustCompile(`(?i)^(?:context [0-9]+% used\b|[0-9]+% context left\b|[0-9]+(?:\.[0-9]+)?[km]? tokens\b)`)

// autoFireObservationIdentity hashes the normalized visible screen used by
// the direct auto-fire repeat guard. Prefer the always-on replay screen over
// the ring tail: a redrawing footer appends a fresh copy to the ring every
// frame, so the raw tail changes while the displayed screen does not — the
// exact loop this guard must stop. The ARIA grid and already-computed tail are
// fallbacks. Recognized spinner/status lines are excluded, while numbers in
// actions and paths remain significant: file-1 and file-2 are different work.
func autoFireObservationIdentity(p *Pane, fallback string) uint64 {
	observation := fallback
	if p != nil {
		if screen, ok := p.screenVisibleText(); ok {
			observation = screen
		} else if gridText, ok := p.gridVisibleText(); ok {
			observation = gridText
		}
	}

	var kept []string
	for _, raw := range strings.Split(observation, "\n") {
		line := strings.TrimSpace(raw)
		// The replay screen contains Claude's live spinner row. Its random verb
		// can change on every paint even though the permission menu has not;
		// exclude only the already-recognized spinner shape, preserving stable
		// menu and command lines as identity-bearing content.
		if !isMenuIdentityLine(line) && claudeActiveSpinnerRE.MatchString(line) {
			continue
		}
		if isStatusJunkLine(line) {
			continue
		}
		line = normalizeMenuLine(line)
		if autoFireNumericStatusRE.MatchString(line) {
			continue
		}
		if line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(kept, "\n")))
	return h.Sum64()
}

// directAutoFire is the no-LLM fast path for AutoMode "always" panes.
// Presses Enter on the already-focused option-1 prompt, records the
// fire, and emits the same
// confirm-resolved UI event so the autoresponder strip animates
// identically to the LLM-driven path.
//
// Caller must already hold the per-pane in-flight eval lock and have
// confirmed AutoMode is "always" with no standing order.
func (a *Aria) directAutoFire(ctx context.Context, paneID, paneTitle string) {
	a.directAutoFireWithJevProof(ctx, paneID, paneTitle, nil)
}

func (a *Aria) directAutoFireWithJevProof(ctx context.Context, paneID, paneTitle string, proof *jevAmbiguousProof) {
	if ctx.Err() != nil {
		return
	}
	orderID := "automode_always_direct"
	instruction := "automode-always direct fire (no LLM)"
	start := time.Now()
	if proof != nil && !proof.started.IsZero() {
		start = proof.started
	}
	if proof != nil && proof.classified {
		orderID = "automode_always_jev"
		instruction = "Jev classified the focused tool permission"
	}
	var tailSnap string
	var obsHash, settleKey uint64
	var gridPromptGeneration uint64
	var observedPane *Pane
	var policy ariaAutoModePolicy
	if p, ok := a.mgr.Get(paneID); ok {
		ctx, policy = withAriaAutoModePolicy(ctx, p)
		observedPane = p
		tailSnap = tailOfPane(p, 30)
		obsHash = autoFireObservationIdentity(p, tailSnap)
		settleKey = autoFireSettleKey(p)
		gridPromptGeneration = atomic.LoadUint64(&p.LastAckedGridChoicePromptGeneration)
	}

	emit := a.autoEmitter(paneID, paneTitle, orderID, 0)
	emit(map[string]any{
		"type":        "auto-eval-start",
		"pane_id":     paneID,
		"pane_title":  paneTitle,
		"order_id":    orderID,
		"instruction": instruction,
	})
	if wait := a.autoFireHold(paneID, settleKey, time.Now()); wait > 0 {
		// Pacing hold (see autoFireHold): not a decline and not a repeat.
		// Re-evaluate when it ends instead of waiting for the sweeper.
		log.Printf("aria-auto-direct: pane=%s pacing hold %v", paneID, wait.Round(time.Millisecond))
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		a.retryAutoFireAfter(paneID, paneTitle, wait)
		return
	}
	if a.unchangedAutoFireExhausted(paneID, obsHash, gridPromptGeneration) {
		log.Printf("aria-auto-direct: pane=%s skipping — already auto-fired %d× on this unchanged screen", paneID, ariaAutoUnchangedFireLimit)
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		// This is a parked repeat, not a decline: emit no rate-cap suppression
		// event and leave the sweeper streak untouched. Rebind the human-attention
		// reason on every suppressed redraw because its raw prompt position can
		// advance; publish the explicit awaiting event only once per stable screen.
		const repeatReason = "unchanged observation already auto-answered"
		if observedPane != nil {
			observedPane.SetAutoDeclined(repeatReason)
			if a.attention != nil {
				a.attention.Refresh(observedPane)
			} else {
				observedPane.setAttention(AttentionNeedsUser, repeatReason)
			}
		}
		if a.markUnchangedAutoFireSurfaced(paneID, obsHash, gridPromptGeneration) {
			workspace := ""
			if a.ws != nil {
				workspace = a.ws.NameForPane(paneID)
			}
			a.uiBus.publish(uiEvent{
				Type:      "pane.awaiting_approval",
				PaneID:    paneID,
				PaneTitle: paneTitle,
				Workspace: workspace,
				Ts:        time.Now().UnixMilli(),
				Text:      tailSnap,
			})
		}
		a.recordEvalOutcomeDedup(AriaEvalOutcome{
			PaneID:    paneID,
			Kind:      "skipped_repeat",
			Reason:    repeatReason,
			Model:     "direct",
			LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}

	recordOutcome := func(kind, reason, sendInput string) {
		if proof != nil && proof.full && proof.classified {
			a.ariaJevFullAudit(kind, proof.known, time.Since(start).Milliseconds(), 0, 0)
		}
		model := "direct"
		if proof != nil && proof.classified {
			model = ariaJevModel
		}
		a.recordEvalOutcome(AriaEvalOutcome{
			PaneID:       paneID,
			Kind:         kind,
			Reason:       reason,
			TailSnapshot: tailSnap,
			Model:        model,
			LatencyMs:    time.Since(start).Milliseconds(),
			SendInput:    sendInput,
		})
	}

	// Cap + pause check. Same gate the LLM path uses, same UI signal.
	if allowed, capReason := a.autoAllowed(); !allowed {
		reason := autoSuppressReason(capReason)
		switch capReason {
		case "paused":
			log.Printf("aria-auto-direct: paused, skipping fire for pane=%s", paneID)
		case "burst-cap":
			log.Printf("aria-auto-direct: burst-limited (>=%d/sec), skipping fire for pane=%s", ariaAutoMaxPerSecond, paneID)
		default:
			log.Printf("aria-auto-direct: gate %q, skipping fire for pane=%s", capReason, paneID)
		}
		a.uiBus.publish(uiEvent{
			Type:      "aria-auto-suppressed",
			PaneID:    paneID,
			PaneTitle: paneTitle,
			Ts:        time.Now().UnixMilli(),
			Reason:    reason,
		})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("skipped_backoff", reason, "")
		// Don't bump backoff streak for cap/pause — those are user
		// intent, not "the model declined."
		return
	}

	p, found := a.mgr.Get(paneID)
	if !found {
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("error", "pane not found", "")
		return
	}

	// Stale-write guard. Same logic as toolSendInput's auto branch:
	// between the watcher firing and this write, Claude may have
	// processed something else and moved past the menu. Writing "1"
	// into an empty prompt would submit "1" as a free-form message.
	if p.computeState() != StateChoicePrompt {
		log.Printf("aria-auto-direct: pane=%s aborted (state=%s, no longer choice-prompt)", paneID, p.computeState())
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("declined", "no longer choice-prompt", "")
		return
	}
	if !currentAutoApprovalPromptMatchesProof(p, proof) {
		if proof != nil {
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordJevObservationHold(paneID, "approval observation changed; retrying")
			return
		}
		reason := currentAutoApprovalDeclineReason(p)
		log.Printf("aria-auto-direct: pane=%s declined: %s", paneID, reason)
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("declined", reason, "")
		a.recordDirectDecline(paneID, p)
		return
	}
	// User-input hold. The guards above prove a menu is on screen; none of
	// them prove the human is not typing into the pane right now. If the menu
	// has just been answered by hand, the Enter below lands in an ordinary
	// composer and submits whatever is half-written there. Hold, don't
	// decline: no streak bump, so the recovery sweeper retries once the pane
	// goes quiet.
	// No aria-auto-suppressed event: the sweeper retries every 5s, and every
	// publish makes each connected client refetch /api/aria/status. The deduped
	// decision-log entry below is the surface that already renders this, and
	// attention still escalates the pane on its own timer.
	if hold, holdReason := autoFireUserHold(p, time.Now()); hold {
		log.Printf("aria-auto-direct: pane=%s held (%s)", paneID, holdReason)
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		// Deduped, with its own Kind and no tail. A held pane is re-evaluated
		// every sweeper tick; recordDedup keys on Kind + TailSnapshot, so
		// sharing "skipped_backoff" would let a cap/pause entry swallow the
		// hold, and carrying the tail (which contains the line the user is
		// retyping) would defeat the dedup outright and evict real
		// fire/decline history from the 5-slot ring.
		a.recordEvalOutcomeDedup(AriaEvalOutcome{
			PaneID:    paneID,
			Kind:      "held",
			Reason:    holdReason,
			Model:     "direct",
			LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}

	_, promptPos, promptHash, ok := currentChoicePromptIdentity(p, "")
	if !ok {
		log.Printf("aria-auto-direct: pane=%s aborted (no stable prompt identity)", paneID)
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("declined", "no stable prompt identity", "")
		return
	}

	hasClaude := p.HasClaudeDescendant()
	hasCodex := p.HasCodexDescendant() || p.HasMuseDescendant()
	body := ""
	confirmID := fmt.Sprintf("auto_direct_%s_%d", paneID, time.Now().UnixMilli())
	if !a.tryReserveAutoFireSlot(confirmID) {
		log.Printf("aria-auto-direct: pane=%s aborted (pause/cap blocked final fire)", paneID)
		a.uiBus.publish(uiEvent{
			Type:      "aria-auto-suppressed",
			PaneID:    paneID,
			PaneTitle: paneTitle,
			Ts:        time.Now().UnixMilli(),
			Reason:    "rate-limited",
		})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("skipped_backoff", "pause/cap blocked final fire", "")
		return
	}
	displayInput := body + "\n"
	if body == "" {
		displayInput = "\n"
	}
	emit(map[string]any{
		"type":        "needs_confirm",
		"id":          confirmID,
		"tool":        "send_input",
		"summary":     fmt.Sprintf("Send to %s: %s", paneTitle, strings.ReplaceAll(displayInput, "\n", "↵")),
		"pane_id":     paneID,
		"pane_title":  paneTitle,
		"input":       displayInput,
		"tool_use_id": confirmID,
	})

	// Focused choice prompts already have option 1 selected, so a
	// bare Enter approves the same option without first placing "1"
	// in the TUI composer. That avoids a partial-write failure mode
	// where the prompt changes during the body/Enter timing gap and
	// leaves a stray digit behind.
	if body != "" {
		if !choicePromptIdentityMatches(p, promptPos, promptHash) {
			log.Printf("aria-auto-direct: pane=%s aborted (prompt changed before body write)", paneID)
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": "pane prompt changed before body write"})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordAutoEvalOutcome(paneID, false)
			recordOutcome("error", "pane prompt changed before body write", "")
			return
		}
		if _, err := p.writeWithAutoModePolicy([]byte(body), false, policy.policy, time.Now().Add(ariaJevTimeout)); err != nil {
			log.Printf("aria-auto-direct: pane=%s body write failed: %v", paneID, err)
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": err.Error()})
			emit(map[string]any{"type": "error", "message": err.Error()})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordAutoEvalOutcome(paneID, false)
			recordOutcome("error", "body write failed: "+err.Error(), "")
			return
		}
	}

	gap := time.Duration(0)
	if body != "" {
		gap = EnterGap(hasClaude)
	}
	if gap > 0 {
		select {
		case <-ctx.Done():
			log.Printf("aria-auto-direct: pane=%s cancelled before Enter", paneID)
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": "cancelled before Enter"})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordAutoEvalOutcome(paneID, false)
			recordOutcome("error", "cancelled before Enter", "")
			return
		case <-time.After(gap):
		}
	}
	if !choicePromptIdentityMatches(p, promptPos, promptHash) {
		log.Printf("aria-auto-direct: pane=%s aborted (prompt changed before Enter)", paneID)
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": "pane prompt changed before Enter"})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		a.recordAutoEvalOutcome(paneID, false)
		recordOutcome("error", "pane prompt changed before Enter", "")
		return
	}
	if !currentAutoApprovalPromptMatchesProof(p, proof) {
		if proof != nil {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false, "error": "approval observation changed; retrying"})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordJevObservationHold(paneID, "approval observation changed; retrying")
			return
		}
		log.Printf("aria-auto-direct: pane=%s aborted before Enter (prompt is no longer an approval prompt)", paneID)
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": "pane prompt is not an approval prompt"})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		a.recordDirectDecline(paneID, p)
		recordOutcome("declined", "not an approval prompt", "")
		return
	}

	if ctx.Err() != nil {
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false, "error": "cancelled before Enter"})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		recordOutcome("cancelled", "cancelled before Enter", "")
		return
	}
	writeEnter := p.WriteNoPromptRecord
	if proof != nil {
		writeEnter = func([]byte) (int, error) { return a.writeJevEnter(ctx, p, proof) }
	} else {
		writeEnter = func(b []byte) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if !policy.matches(p) {
				return 0, errAutoModePolicyChanged
			}
			deadline := time.Now().Add(ariaJevTimeout)
			if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
				deadline = requestDeadline
			}
			return p.writeWithAutoModePolicy(b, false, policy.policy, deadline)
		}
	}
	if _, err := writeEnter([]byte("\r")); err != nil {
		if errors.Is(err, errAutoModePolicyChanged) {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false, "error": err.Error()})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			recordOutcome("held", "approval policy changed", "")
			return
		}
		if proof != nil && errors.Is(err, errJevObservationChanged) {
			emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false, "error": "approval observation changed; retrying"})
			emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
			a.recordJevObservationHold(paneID, "approval observation changed; retrying")
			return
		}
		log.Printf("aria-auto-direct: pane=%s Enter write failed: %v", paneID, err)
		emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": true, "error": err.Error()})
		emit(map[string]any{"type": "error", "message": err.Error()})
		emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
		a.recordAutoEvalOutcome(paneID, false)
		recordOutcome("error", "Enter write failed: "+err.Error(), "")
		return
	}

	_, _, promptSource := currentAutoApprovalPrompt(p)
	log.Printf("aria-auto-direct: pane=%s hasClaude=%v hasCodex=%v body=%q gap=%v src=%s fired", paneID, hasClaude, hasCodex, body, gap, promptSource)

	// Fire-resolved emit BEFORE the cap bump so autoEmitter's
	// confirm_resolved branch is what calls autoRecordFire +
	// MarkAutoModeFired (keeps the bookkeeping in one place).
	emit(map[string]any{
		"type":     "confirm_resolved",
		"id":       confirmID,
		"approved": true,
	})
	emit(map[string]any{"type": "auto-eval-done", "pane_id": paneID, "pane_title": paneTitle})
	a.recordAutoEvalOutcome(paneID, true)
	a.recordUnchangedAutoFire(paneID, obsHash, gridPromptGeneration)
	a.markAutoFirePace(paneID, settleKey, time.Now())
	a.verifyAutoFireLanded(paneID, paneTitle, atomic.LoadInt64(&p.LastInputMs))
	recordOutcome("fired", "", renderControlChars(displayInput))
}

// Caller holds p.mu; Jev must retain its screen/input transaction through transport.
func (p *Pane) autoModePolicyLockedForAnswer(ctx context.Context) paneAutoModePolicy {
	if captured, ok := ctx.Value(ariaAutoModePolicyKey{}).(ariaAutoModePolicy); ok {
		return captured.policy
	}
	return paneAutoModePolicy{mode: p.AutoMode, generation: p.autoModeGeneration}
}
