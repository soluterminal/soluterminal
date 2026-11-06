package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Terminal-text auto-approval classifier.
//
// Auto-approve answers a focused menu by sending a bare Enter, which selects
// the FOCUSED row (option 1 in practice). That is only safe when the menu is a
// tool/command permission gate AND the focused row grants that one action (or
// a session-scoped allow for the same tool/command — the normal Claude/Codex
// permission UX). Every other focused menu — plan exit, model-authored
// questions, billing/credits/reset confirmations, login/account menus, mode
// switches — must be left to the human. The native path enforces the same
// rule by tool name (isNonAutoApprovableNativeTool); this is the terminal
// equivalent, keyed on menu STRUCTURE (header, option rows, footer) rather
// than a growing allowlist of question phrasings.
//
// Order in autoApprovalDeclineReason:
//  1. deny classifier (plan / question / billing / login) — runs FIRST, so no
//     approve rule, including the Codex header-only fast path, can override it;
//  2. structural approve rules (Codex modal fast path, Codex MCP modal);
//  3. focused row must be an accept-once / session-allow row;
//  4. at least one sibling option must be visible, including scroll-marker rows;
//  5. the menu must offer an explicit escape option ("No…", "Cancel…",
//     "Esc to cancel").
//
// Both directAutoFire (via currentAutoApprovalPromptText) and the cautious
// ARIA path (cautiousAutoDenyReason) reach this through
// isAutoApprovalPromptText — there is no second pre-filter to keep in sync.

var (
	// A numbered menu row, optionally focus-marked. Group 1 = number,
	// group 2 = label. Same trailer grammar as choicePromptRE /
	// menuSiblingOptionRE ("1." / "1)" / "1  " after an ANSI-collapse glitch).
	menuOptionRowRE = regexp.MustCompile(`^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?([0-9]{1,2})(?:\.|\)|[\s\p{Zs}]{2,})[\s\p{Zs}]*(.*)$`)

	// Claude's Select draws "↓" (more rows below) on the last visible row and
	// "↑" (more above) on the first visible row, in the pointer column of an
	// unfocused row; the focused row always keeps its pointer. Strict on
	// purpose: the "N." trailer only, so counters like "↓  12  tokens" never
	// read as a row. Same groups as menuOptionRowRE.
	menuScrollMarkerRowRE = regexp.MustCompile(`^[\s\p{Zs}]*[↑↓][\s\p{Zs}]*([0-9]{1,2})\.[\s\p{Zs}]*(.*)$`)

	// Latest Codex command/edit modal header, used to decide whether the
	// header-only fast path describes the NEWEST menu on screen or a stale
	// modal left in scrollback above a later focused menu.
	codexApprovalHeaderRE = regexp.MustCompile(`Would\s+you\s+like\s+to\s+(?:run\s+the\s+following\s+command|make\s+the\s+following\s+edits)\?`)

	// Trailing key hint / description parentheticals on an option label:
	// "(y)", "(shift+tab)", "(esc)".
	optionHintSuffixRE = regexp.MustCompile(`\s*\([^()]{0,40}\)\s*$`)

	// Focused rows that grant exactly the pending action, or a session /
	// command-scoped allow for the same tool. Matched against the
	// normalized (lowercased, whitespace-collapsed, hint-stripped) label.
	acceptOnceFocusedRowREs = []*regexp.Regexp{
		regexp.MustCompile(`^yes$`),
		regexp.MustCompile(`^yes, proceed\b`),                               // Codex / Muse / legacy trust
		regexp.MustCompile(`^yes, allow\b`),                                 // DeepSeek / Pi "allow once", Claude "allow all edits during this session"
		regexp.MustCompile(`^yes, (?:and )?don'?t ask again\b`),             // Claude / Codex / Muse session allow
		regexp.MustCompile(`^yes, and allow\b`),                             // Claude settings-write, Codex host/permissions for session
		regexp.MustCompile(`^yes, just this once\b`),                        // Codex network
		regexp.MustCompile(`^yes, grant these permissions for this turn\b`), // Codex grant-permissions
		regexp.MustCompile(`^yes, provide the requested info\b`),            // Codex MCP info request
		regexp.MustCompile(`^yes, for this session\b`),                      // Claude add-directory
		regexp.MustCompile(`^yes, run it$`),                                 // Claude dynamic-workflow tool gate
	}

	// Even an accept-prefixed focused row must not switch session modes.
	focusedModeSwitchRE = regexp.MustCompile(`bypass permissions|auto mode|auto-accept|clear context|plan mode|full access|switch to\b`)

	// An escape option row: "No", "No, and tell Claude…", "Cancel this tool
	// call", "Reject". Word-anchored so "know"/"not"/"cancelled" never count.
	escapeOptionRE = regexp.MustCompile(`^(?:no|cancel|reject|deny)\b|\bcancel\b`)
	escapeFooterRE = regexp.MustCompile(`\besc to (?:cancel|reject|deny)\b`)
)

// Question-region phrases (the last few lines above the focused row).
// Kept specific: this region also holds Bash command bodies, so a loose word
// would needlessly decline real permission prompts.
var autoApprovalDenyHeaderPhrases = []struct{ needle, reason string }{
	// Plan-exit / plan-enter (Claude + Codex).
	{"ready to code?", "plan-exit menu"},
	{"exit plan mode?", "plan-exit menu"},
	{"wants to exit plan mode", "plan-exit menu"},
	{"enter plan mode?", "plan-enter menu"},
	{"wants to enter plan mode", "plan-enter menu"},
	{"written up a plan", "plan-exit menu"},
	{"implement this plan?", "plan-exit menu"},
	// Billing / credits / resets.
	{"use your reset?", "billing/reset confirmation"},
	{"use this reset?", "billing/reset confirmation"},
	{"usage credits", "billing/credits menu"},
	{"extra usage", "billing/credits menu"},
	// Login / account.
	{"use this api key", "login/account menu"},
	{"login method", "login/account menu"},
	{"sign in with", "login/account menu"},
	{"log in with", "login/account menu"},
}

// Options + footer phrases (everything from the focused row down).
var autoApprovalDenyMenuPhrases = []struct{ needle, reason string }{
	// Plan-exit / plan-enter option labels (Claude + Codex).
	{"clear context", "plan-exit menu"},
	{"auto-accept edits", "plan-exit menu"},
	{"manually approve edits", "plan-exit menu"},
	{"keep planning", "plan-exit menu"},
	{"ultraplan", "plan-exit menu"},
	{"tell claude what to change", "plan-exit menu"},
	{"implement this plan", "plan-exit menu"},
	{"stay in plan mode", "plan-exit menu"},
	{"continue planning", "plan-exit menu"},
	{"start coding", "plan-exit menu"},
	{"fresh thread with this plan", "plan-exit menu"},
	{"enter plan mode", "plan-enter menu"},
	{"start implementing", "plan-enter menu"},
	// Billing / credits / resets / model or access upsell.
	{"use my reset", "billing/reset confirmation"},
	{"use this reset", "billing/reset confirmation"},
	{"usage credits", "billing/credits menu"},
	{"re-enable and continue", "billing/credits menu"},
	{"upgrade to", "billing/upgrade menu"},
	{"keep my current model", "model-switch menu"},
	{"full access", "access-mode menu"},
	// Question menus: Claude AskUserQuestion and Codex request_user_input.
	{"enter to sel", "question menu"}, // "Enter to select", also when truncated
	{"↑/↓ to navigate", "question menu"},
	{"up/down to navigate", "question menu"},
	{"arrow keys to navigate", "question menu"},
	{"chat about this", "question menu"},
	{"type something", "question menu"},
	{"to add notes", "question menu"},
	{"submit answer", "question menu"},
	{"to submit all", "question menu"},
	{"navigate questions", "question menu"},
	{"none of the above", "question menu"},
	{"type your answer", "question menu"},
	{"unanswered", "question menu"},
}

var (
	reachedLimitRE      = regexp.MustCompile(`reached your .{0,40}limit`)
	switchAndContinueRE = regexp.MustCompile(`switch to \S.{0,40} and continue`)
)

// normalizePromptText lowercases, folds curly apostrophes, and collapses every
// whitespace run (including newlines and NBSP) to one space, so a footer the
// pane wrapped ("Enter to\nselect") matches exactly like the unwrapped one.
func normalizePromptText(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("’", "'", "‘", "'").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// menuOptionRow parses one screen line as a numbered menu row: group 1 = the
// number, group 2 = the label. It accepts everything menuOptionRowRE does plus
// a row carrying a scroll marker in place of its pointer. It never reports a
// row as focused: focus detection stays on the pointer-only regexes.
func menuOptionRow(line string) []string {
	if row := menuOptionRowRE.FindStringSubmatch(line); row != nil {
		return row
	}
	return menuScrollMarkerRowRE.FindStringSubmatch(line)
}

func isMenuOptionRow(line string) bool { return menuOptionRow(line) != nil }

// autoApprovalMenu is the parsed shape of the focused menu about to be
// answered.
type autoApprovalMenu struct {
	header  string   // normalized last few non-empty lines above the focused row
	options []string // normalized, hint-stripped labels; options[0] is focused
	menu    string   // normalized focused row .. footer (rows + footer)
}

const (
	autoApprovalHeaderLines = 12
	// Lines kept below the focused row: enough for nine options with wrapped
	// labels plus a footer wrapped over several narrow-pane lines.
	autoApprovalMenuLines = 30
)

// parseAutoApprovalMenu splits s at focus, the offset of its focused
// option-1 row. s must be UNTRIMMED: trimTrailingStatusJunk drops short
// trailing lines, which is exactly what a footer wrapped in a narrow pane
// ("Enter to" / "select") looks like.
func parseAutoApprovalMenu(s string, focus int) autoApprovalMenu {
	var m autoApprovalMenu
	m.header = autoApprovalQuestionRegion(s[:focus])
	lines := strings.Split(s[focus:], "\n")
	if len(lines) > autoApprovalMenuLines {
		lines = lines[:autoApprovalMenuLines]
	}
	m.menu = normalizePromptText(strings.Join(lines, "\n"))

	// Option labels, each extended by wrapped continuation lines up to the
	// next row or a blank line. Rows must number consecutively from 1, so
	// later numbered output is not mistaken for an option. The last row gets
	// no continuation: what follows it is footer, not label.
	var labels []string
	lastRow := ""
	cont := false
	for _, line := range lines {
		if row := menuOptionRow(line); row != nil {
			if row[1] != strconv.Itoa(len(labels)+1) {
				break
			}
			labels = append(labels, row[2])
			lastRow = row[2]
			cont = true
			continue
		}
		if strings.TrimSpace(line) == "" {
			cont = false
			continue
		}
		if cont && len(labels) > 0 {
			labels[len(labels)-1] += " " + line
		}
	}
	if len(labels) > 0 {
		labels[len(labels)-1] = lastRow
	}
	for _, l := range labels {
		n := normalizePromptText(l)
		for {
			stripped := optionHintSuffixRE.ReplaceAllString(n, "")
			if stripped == n {
				break
			}
			n = stripped
		}
		m.options = append(m.options, n)
	}
	return m
}

// Permission dialogs display untrusted command/patch content next to their
// question. Do not treat words in that content as the dialog's own question.
// Claude puts its question AFTER the payload; Codex puts it BEFORE the payload.
// Unknown menus retain the conservative bounded header scan.
func autoApprovalQuestionRegion(before string) string {
	lines := strings.Split(before, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !isPermissionQuestionLine(line) {
			break
		}
		at := permissionPayloadIndex(lines[:i])
		if at < 0 {
			break
		}
		// Only the dialog's own separator may sit between its payload and the
		// question. Anything else there (a login or billing decision drawn
		// below stale payload-looking scrollback) stays in the question.
		region := []string{line}
		for _, between := range lines[at+1 : i] {
			between = strings.TrimSpace(between)
			if between != "" && strings.Trim(between, "─╌━-") != "" {
				region = append(region, between)
			}
		}
		return normalizePromptText(strings.Join(region, "\n"))
	}
	// Codex's header precedes its payload. Remove only explicit renderer
	// payload rows, retaining later plain-text login/question headings even
	// when an old command modal remains above them.
	var header []string
	headerLines := strings.Split(lastNonEmptyLines(before, autoApprovalHeaderLines), "\n")
	payload := permissionPayloadIndex(headerLines) >= 0
	for _, line := range headerLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "$ ") || strings.HasPrefix(trimmed, "Reason:") ||
			(payload && strings.HasPrefix(trimmed, "│")) || approvalDiffPayloadRE.MatchString(line) {
			continue
		}
		header = append(header, line)
	}
	return normalizePromptText(strings.Join(header, "\n"))
}

var approvalDiffPayloadRE = regexp.MustCompile(`^\s*[0-9]+\s+(?:[+-]|\s)`)

// A generic "proceed?" is also used by real user decisions. Only separate
// its preceding body when the current dialog actually displays a tool payload.
// A previous focused menu closes that context; stale permission chrome cannot
// hide the title of a new login/billing decision.
//
// permissionPayloadIndex returns the index of the payload row nearest the end
// of lines, or -1 when there is none.
func permissionPayloadIndex(lines []string) int {
	separator := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if len(choicePromptMatches(line)) > 0 || escapeFooterRE.MatchString(normalizePromptText(line)) {
			return -1
		}
		if len([]rune(line)) >= 8 && strings.Trim(line, "─╌━-") == "" {
			separator = true
		}
		if (separator && strings.HasPrefix(line, "│")) || strings.HasPrefix(line, "$ ") || approvalDiffPayloadRE.MatchString(line) {
			return i
		}
		lower := normalizePromptText(line)
		for _, title := range []string{"bash command", "edit file", "create file", "read file", "tool use"} {
			if lower == title || strings.HasPrefix(lower, title+" ·") {
				return i
			}
		}
	}
	return -1
}

func isPermissionQuestionLine(line string) bool {
	line = normalizePromptText(line)
	// Match renderer questions at the start of a line, never text within a
	// command, diff row, or a plan-exit explanation that also says "proceed".
	for _, prefix := range []string{
		"do you want to proceed?", "would you like to proceed?",
		"do you want to make this edit", "do you want to create ",
		"do you want to allow claude to fetch ",
		"would you like to run the following command?",
		"would you like to make the following edits?",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func lastNonEmptyLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append(kept, lines[i])
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// autoApprovalDenyReason is the explicit DENY classifier. header is the
// question region above the focused row; menu is the option rows + footer.
// Both must already be normalized. Returns "" when nothing denies.
func autoApprovalDenyReason(header, menu string) string {
	for _, p := range autoApprovalDenyHeaderPhrases {
		if strings.Contains(header, p.needle) {
			return p.reason
		}
	}
	if reachedLimitRE.MatchString(header) {
		return "billing/limit menu"
	}
	for _, p := range autoApprovalDenyMenuPhrases {
		if strings.Contains(menu, p.needle) {
			return p.reason
		}
	}
	if switchAndContinueRE.MatchString(menu) {
		return "model-switch menu"
	}
	return ""
}

// isAcceptOnceFocusedRow reports whether the focused option label grants only
// the pending action (or a session-scoped allow for it).
func isAcceptOnceFocusedRow(label string) bool {
	if focusedModeSwitchRE.MatchString(label) {
		return false
	}
	for _, re := range acceptOnceFocusedRowREs {
		if re.MatchString(label) {
			return true
		}
	}
	return false
}

// menuHasEscapeOption reports whether the menu offers an explicit way out:
// a non-focused option row that starts with "No"/"Cancel"/"Reject"/"Deny"
// (or names Cancel), or an "Esc to cancel" footer.
func menuHasEscapeOption(m autoApprovalMenu) bool {
	for i, opt := range m.options {
		if i == 0 {
			continue
		}
		if escapeOptionRE.MatchString(opt) {
			return true
		}
	}
	return escapeFooterRE.MatchString(m.menu)
}

// autoApprovalDeclineReason returns "" when s shows a prompt auto-approve may
// answer with a bare Enter, otherwise a short reason it must be left alone.
func autoApprovalDeclineReason(s string, hasCodex bool) string {
	if len(folderTrustPromptMatches(s)) > 0 {
		return "folder trust needs the user"
	}
	choices := choicePromptMatches(s)

	// Codex command/edit modal rendered without its focused option row. Only
	// trusted when that header is the newest menu on screen: a stale modal in
	// scrollback above a later focused menu must not approve the later one.
	if hasCodex {
		if locs := codexApprovalHeaderRE.FindAllStringIndex(s, -1); len(locs) > 0 {
			hdr := locs[len(locs)-1][0]
			if (len(choices) == 0 || hdr > choices[len(choices)-1][0]) && isCodexApprovalPromptText(s[hdr:]) {
				// No option rows to inspect; header phrases only, so a
				// command body mentioning e.g. "upgrade to" is not denied.
				return autoApprovalDenyReason(autoApprovalQuestionRegion(s[hdr:]), "")
			}
		}
	}
	if len(choices) == 0 {
		return "no focused numbered menu"
	}

	block := activeChoicePromptBlock(s)
	menu := parseAutoApprovalMenu(s, choices[len(choices)-1][0])
	// 1. Explicit deny, before any approve rule.
	if reason := autoApprovalDenyReason(menu.header, menu.menu); reason != "" {
		return reason
	}
	// 2. Codex MCP permission modal: its four structural anchors (question,
	// focused "Allow", session/persistent grant rows, and Cancel or a
	// validated clipped four-option footer) carry the safety burden.
	if hasCodex && isCodexMcpPermissionPromptText(block) {
		return ""
	}

	// Match phrases across physical wrap rows, as the deny checks already do.
	// Keep the numbered-row regex on the original text so its anchors survive.
	lower := normalizePromptText(block)
	// Permission-gate signal: the "Yes, <qualifier>" escalation row every
	// Claude/Codex permission prompt offers (approvalOptionRE), "ask again"
	// (apostrophe-glyph-proof), or a known permission question. Selection
	// menus never carry these. Don't bolt question phrasings on here — the
	// focused-row and deny checks carry the safety burden.
	hasApprovalLanguage := strings.Contains(lower, "ask again") || approvalOptionRE.MatchString(block)
	if !hasApprovalLanguage {
		for _, needle := range autoApprovalNeedles {
			if strings.Contains(lower, needle) {
				hasApprovalLanguage = true
				break
			}
		}
	}
	if !hasApprovalLanguage {
		return "choice menu, not a permission prompt"
	}

	// 3. Enter selects the focused row, so it must itself be accept-once.
	if len(menu.options) == 0 || !isAcceptOnceFocusedRow(menu.options[0]) {
		return "focused option is not an accept-once approval"
	}
	// 4. A lone focused row and generic footer cannot establish the menu's
	// scope when the other options are clipped. Scroll-marker siblings count
	// as visible options through parseAutoApprovalMenu.
	if len(menu.options) < 2 {
		return "menu has no visible sibling option"
	}
	// 5. A real permission prompt always offers an escape.
	if !menuHasEscapeOption(menu) {
		return "menu offers no No/Cancel escape option"
	}
	return ""
}

// Known permission-question phrasings. Secondary to the structural
// "Yes, <qualifier>" / "ask again" signal above.
var autoApprovalNeedles = []string{
	"do you want to proceed",
	"would you like to proceed",
	"do you want to make this edit",
	"do you want to apply",
	"do you want to accept",
	"would you like to run the following command",
	"would you like to make the following edits",
	"bash command",
	"edit file",
	"yes, proceed",
	"yes, allow",
	"allow all edits",
	"allow reading",
	"trust the files",
}
