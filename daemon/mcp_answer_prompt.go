package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// answer_prompt is the semantically-safe alternative to send_input for the
// one job an orchestrating/caretaker agent actually needs: unblocking a pane
// that is sitting on a permission/choice prompt. Where send_input can type
// ANY bytes into another session's PTY (a prompt-injection escalation
// primitive), answer_prompt can only pick one of the options the pane is
// ALREADY presenting, validated against the live prompt:
//
//   - terminal panes: valid ONLY when the pane is in choice-prompt state;
//     `option` must be one of the numbered options currently on the menu.
//     The chosen digit is written to the PTY (Claude/Codex menus action a
//     bare digit keypress — the ARIA auto-mode convention, no Enter).
//   - native (Focus) panes: `option` must be "allow" or "deny"; resolves the
//     single pending NativeApproval through the same resolveApproval path the
//     UI and lock-screen use (idempotent, confirm-registry bridged).
//
// It cannot free-type, cannot answer a prose "should I continue?" question
// (that is not a choice-prompt), and cannot act on a pane with no live
// prompt. Mutation-gated; scoped callers need it in scope.

// menuOptionRE captures a numbered menu option row: an optional focus arrow,
// the option number, a "." / ")" / 2+-space trailer (mirroring choicePromptRE's
// tolerance for Claude's TUI repaint glitches), then the label.
var menuOptionRE = regexp.MustCompile(`(?m)^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?([0-9]+)(?:[.)]|[\s\p{Zs}]{2,})[\s\p{Zs}]*(\S.*)$`)

// parseMenuOptions extracts option-number → label from a choice-prompt tail.
// When a menu is stacked (an earlier copy scrolled up, the active one at the
// bottom), the LAST occurrence of each number wins — matching the daemon's
// existing "bottom-most menu is active" rule.
func parseMenuOptions(tail string) map[string]string {
	out := map[string]string{}
	for _, m := range menuOptionRE.FindAllStringSubmatch(tail, -1) {
		num := m[1]
		label := strings.TrimRight(strings.TrimSpace(m[2]), " \t\r")
		out[num] = label
	}
	return out
}

func sortedOptionKeys(opts map[string]string) []string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		// Numeric where possible so "10" sorts after "2".
		return atoiSafe(keys[i]) < atoiSafe(keys[j])
	})
	return keys
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 1 << 30
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// mcpToolAnswerPrompt also returns the prompt it answered, read before the
// answer changes it: the approval it resolved, or the menu's input revision.
func (a *apiServer) mcpToolAnswerPrompt(args map[string]any) (any, string) {
	paneID, _ := args["pane_id"].(string)
	option, _ := args["option"].(string)
	option = strings.TrimSpace(option)
	if strings.TrimSpace(paneID) == "" {
		return toolErr("pane_id is required"), ""
	}
	if option == "" {
		return toolErr(`option is required — a menu number ("1") for a terminal pane, or "allow"/"deny" for a native pane`), ""
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID + " (call list_panes for ids)"), ""
	}
	snap := p.Snapshot()
	if snap.Status == PaneExited {
		return toolErr("pane " + paneID + " has exited — nothing to answer"), ""
	}

	if snap.Kind == "native" {
		return a.answerNativePrompt(p, option)
	}
	return a.answerTerminalPrompt(p, paneID, option)
}

// answerNativePrompt resolves a Focus pane's single pending approval.
func (a *apiServer) answerNativePrompt(p *Pane, option string) (any, string) {
	var allow bool
	switch strings.ToLower(option) {
	case "allow", "yes", "approve", "y":
		allow = true
	case "deny", "no", "reject", "n":
		allow = false
	default:
		return toolErr(`native pane: option must be "allow" or "deny" (got ` + option + ")"), ""
	}
	pending := p.peekNativePending("")
	if pending == nil {
		return toolErr("pane " + p.ID + " has no pending approval to answer"), ""
	}
	denyReason := ""
	if !allow {
		denyReason = "declined via answer_prompt"
	}
	policy := p.autoModePolicy()
	resolved, err := p.resolveApprovalWithPolicy(pending.RequestID, allow, denyReason, &policy)
	if err != nil {
		if err == errNoPendingApproval {
			return toolErr("the approval was already resolved (someone else answered it)"), ""
		}
		return toolErr("resolve failed: " + err.Error()), ""
	}
	// Converge with the in-app ConfirmCard / lock-screen paths, exactly like
	// handlePaneApprovalResolve does.
	if a.aria != nil && resolved != nil && resolved.ConfirmID != "" {
		a.aria.signalConfirm(resolved.ConfirmID, allow, false)
	}
	verb := "Allowed"
	if !allow {
		verb = "Denied"
	}
	return toolOK(fmt.Sprintf("%s the pending %s approval on pane %s ('%s').", verb, resolved.ToolName, p.ID, p.Title)), approvalPromptID(pending.RequestID)
}

// answerTerminalPrompt validates the pane is on a menu and the option exists,
// then writes the digit to the PTY.
func (a *apiServer) answerTerminalPrompt(p *Pane, paneID, option string) (any, string) {
	if state := p.computeStateCached(); state != StateChoicePrompt {
		return toolErr("pane " + paneID + " is not on a choice prompt (state=" + string(state) +
			"). answer_prompt only acts on a live numbered menu; it will not type into a running or idle pane."), ""
	}
	// The rendered screen is the live menu. Raw scrollback can hold stale
	// menus, and Codex's inline frames collapse rows onto one line once the
	// cursor moves are stripped, so it is only the fallback.
	screen := ""
	if snap, ok := p.ScreenSnapshot(); ok {
		screen, _ = snapshotVisibleText(snap)
	}
	opts := parseMenuOptions(menuRegion(screen))
	if len(opts) == 0 {
		opts = parseMenuOptions(approvalScanTail(p))
	}
	if len(opts) == 0 {
		// State said choice-prompt but no options parsed — a transient
		// surfacing miss (the menu scrolled behind a spinner burst). Refuse
		// rather than blind-fire a digit.
		return toolErr("pane " + paneID + " looks like a menu but its options could not be read right now — try again in a moment"), ""
	}
	label, present := opts[option]
	if !present {
		return toolErr(fmt.Sprintf("option %q is not on pane %s's menu. Presented options: %s",
			option, paneID, describeOptions(opts))), ""
	}
	prompt := promptRevID(p)
	policy := p.autoModePolicy()
	writeAnswer := func(b []byte) (int, error) {
		return p.writeWithAutoModePolicy(b, false, policy, time.Now().Add(ariaJevTimeout))
	}
	// Write the bare digit — Claude/Codex numbered menus action the keypress
	// immediately (ARIA auto-mode convention: no Enter). WriteNoPromptRecord,
	// like every menu-choice reply: the digit is a control answer, not a user
	// prompt, and must stay out of the prompt-recorder strip.
	if _, err := writeAnswer([]byte(option)); err != nil {
		return toolErr("write to pane " + paneID + " failed: " + err.Error()), ""
	}
	// Codex's submit-style menus ("enter to submit") can treat the digit as
	// "move here" rather than "choose". Confirm with Enter only when the menu
	// is still up and its focus marker now sits on the chosen option.
	if menuNeedsSubmit(screen) {
		answerPromptSleep(answerPromptSettle)
		if after, ok := p.ScreenSnapshot(); ok {
			if text, ok := snapshotVisibleText(after); ok && menuNeedsSubmit(text) {
				if focusedMenuOption(menuRegion(text)) != option {
					return toolErr(fmt.Sprintf("pressed %s on pane %s but the menu's selection did not move to it; nothing was confirmed. Read the pane and try again, or tell the user.", option, paneID)), ""
				}
				if _, err := writeAnswer([]byte("\r")); err != nil {
					return toolErr("confirm on pane " + paneID + " failed: " + err.Error()), ""
				}
			}
		}
	}
	return toolOK(fmt.Sprintf("Selected option %s (%q) on pane %s ('%s').", option, label, paneID, p.Title)), prompt
}

const answerPromptSettle = 300 * time.Millisecond

// answerPromptSleep is swapped by tests.
var answerPromptSleep = time.Sleep

var (
	menuSubmitFooterRE = regexp.MustCompile(`(?i)enter to submit`)
	menuFocusRowRE     = regexp.MustCompile(`(?m)^[\s\p{Zs}]*[❯❭›▶][\s\p{Zs}]*([0-9]+)(?:[.)]|[\s\p{Zs}]{2,})`)
)

// menuRegion keeps the bottom-most menu on a screen: from the last line that
// starts the option list ("1." with or without a focus marker) to the end.
func menuRegion(screen string) string {
	if screen == "" {
		return ""
	}
	lines := strings.Split(screen, "\n")
	start := -1
	for i, line := range lines {
		if m := menuOptionRE.FindStringSubmatch(line); m != nil && m[1] == "1" {
			start = i
		}
	}
	if start < 0 {
		return screen
	}
	// Include the question above the options for context.
	if start >= 6 {
		start -= 6
	} else {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
}

func menuNeedsSubmit(screen string) bool {
	return menuSubmitFooterRE.MatchString(screen)
}

func focusedMenuOption(menu string) string {
	all := menuFocusRowRE.FindAllStringSubmatch(menu, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

func describeOptions(opts map[string]string) string {
	var b strings.Builder
	for i, k := range sortedOptionKeys(opts) {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s=%s", k, opts[k])
	}
	return b.String()
}
