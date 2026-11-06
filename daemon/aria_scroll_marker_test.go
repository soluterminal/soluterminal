package main

import (
	"strings"
	"testing"
)

// Claude's Select replaces the pointer of an unfocused first/last visible row
// with "↑"/"↓" when rows are scrolled out of a short pane. The focused row
// keeps "❯". These fixtures are that shape (command text is synthetic).
const scrollMarkerHead = "Bash command\n\n  git status && cat \"a b\"\n  Show status\n\nDo you want to proceed?\n"

func TestMenuOptionRowAcceptsScrollMarkers(t *testing.T) {
	for _, tc := range []struct {
		line, num, label string
	}{
		{"↓ 2. Yes, and switch to auto mode", "2", "Yes, and switch to auto mode"},
		{"  ↓ 2. Yes, and don't ask again", "2", "Yes, and don't ask again"},
		{"↓2. Yes", "2", "Yes"},
		{"  ↑ 3. Yes, and allow", "3", "Yes, and allow"},
		{"❯ 1. Yes", "1", "Yes"},
		{"  3. No", "3", "No"},
	} {
		row := menuOptionRow(tc.line)
		if row == nil || row[1] != tc.num || row[2] != tc.label {
			t.Errorf("menuOptionRow(%q) = %q, want %s/%q", tc.line, row, tc.num, tc.label)
		}
	}
	// Not rows: counters, prose, arrows without a "N." trailer, a marker
	// before a 3-digit number, a trailing arrow, and the 2-space trailer
	// that only the pointer grammar tolerates.
	for _, line := range []string{
		"↓  12  tokens",
		"↓ 2 files changed",
		"↓ 2  Yes",
		"↓ 123. Yes",
		"↓ Yes",
		"Yes ↓ 2. Yes",
		"(17s · ↓ 2. tokens)",
		"→ 2. Yes",
		"",
	} {
		if row := menuOptionRow(line); row != nil {
			t.Errorf("menuOptionRow(%q) = %q, want no row", line, row)
		}
	}
}

func TestScopeDeclineAllowsOnceRowWithScrollMarkerSibling(t *testing.T) {
	for name, tail := range map[string]string{
		"arrow row":     scrollMarkerHead + "❯ 1. Yes\n↓ 2. Yes, and switch to auto mode · auto mode\n     handles these prompts for you",
		"indented":      scrollMarkerHead + " ❯ 1. Yes\n ↓ 2. Yes, and don't ask again for git commands\n",
		"wrapped arrow": scrollMarkerHead + "❯ 1. Yes\n↓ 2. Yes, and allow Claude to edit its own\n     settings for this session\n",
	} {
		t.Run(name, func(t *testing.T) {
			if reason := ariaJevScopeDecline(tail); reason != "" {
				t.Fatalf("once permission declined: %s", reason)
			}
			evidence := ariaJevMenuEvidence(tail, true)
			if !strings.Contains(evidence, "↓ 2.") {
				t.Fatalf("evidence dropped the second row: %q", evidence)
			}
		})
	}
}

func TestScopeDeclineStillRefusesNonOnceFocus(t *testing.T) {
	for name, tc := range map[string]struct{ tail, want string }{
		"focused auto-mode switch": {
			scrollMarkerHead + "❯ 1. Yes, and switch to auto mode · auto mode\n     handles these prompts for you\n↓ 2. Yes, and don't ask again",
			"focused option is not an explicit once/turn permission",
		},
		"focused session grant": {
			scrollMarkerHead + "❯ 1. Yes, and don't ask again for git commands\n↓ 2. Yes",
			"focused option is not an explicit once/turn permission",
		},
		"focus on row two, arrow above": {
			scrollMarkerHead + "↑ 1. Yes\n❯ 2. Yes, and switch to auto mode\n  3. No",
			"focused option is not the first once/turn permission",
		},
		"focus on row three, arrow above": {
			scrollMarkerHead + "↑ 2. Yes\n❯ 3. No",
			"focused option is not the first once/turn permission",
		},
		"deny reason still wins": {
			"Ready to code?\n\nHere is Claude's plan:\n  do things\n\nWould you like to proceed?\n❯ 1. Yes\n↓ 2. Yes, auto-accept edits",
			"plan-exit menu",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ariaJevScopeDecline(tc.tail); got != tc.want {
				t.Fatalf("ariaJevScopeDecline = %q, want %q", got, tc.want)
			}
		})
	}
}

// A marker line that is only output text, sitting under the focused row with
// no blank line, is still a row (as "2. text" would be); the label guard
// judges the focused row alone and the menu-level checks are unchanged.
func TestScopeDeclineMarkerOnlyChangesRowBoundary(t *testing.T) {
	tail := scrollMarkerHead + "❯ 1. Yes, proceed with everything\n↓ 2. No"
	if reason := ariaJevScopeDecline(tail); reason == "" {
		t.Fatalf("a label outside the once list was approved")
	}
}

func TestParseAutoApprovalMenuReadsScrollMarkerRows(t *testing.T) {
	s := scrollMarkerHead + "❯ 1. Yes\n↓ 2. Yes, and switch to auto mode · auto mode\n     handles these prompts for you\n"
	focus := strings.Index(s, "❯")
	m := parseAutoApprovalMenu(s, focus)
	// The last row takes no wrapped continuation (what follows it is footer).
	want := []string{"yes", "yes, and switch to auto mode · auto mode"}
	if len(m.options) != 2 || m.options[0] != want[0] || m.options[1] != want[1] {
		t.Fatalf("options = %q, want %q", m.options, want)
	}
	// Rows must still number consecutively from 1.
	s = scrollMarkerHead + "❯ 1. Yes\n↓ 3. No\n"
	if m := parseAutoApprovalMenu(s, strings.Index(s, "❯")); len(m.options) != 1 {
		t.Fatalf("non-consecutive marker row was taken as an option: %q", m.options)
	}
}

func TestAutoApprovalAcceptsVisibleScrollMarkerSiblings(t *testing.T) {
	for name, rows := range map[string]string{
		"down-marker sibling": "❯ 1. Yes\n↓ 2. Yes, and switch to auto mode · auto mode\n     handles these prompts for you",
		"down-marker third row": "❯ 1. Yes\n  2. Yes, and don't ask again\n↓ 3. No",
	} {
		t.Run(name, func(t *testing.T) {
			s := scrollMarkerHead + rows + "\n\n Esc to cancel · Tab to amend"
			if reason := autoApprovalDeclineReason(s, false); reason != "" {
				t.Fatalf("visible marker sibling declined: %s", reason)
			}
		})
	}
}

func TestPlainTextWithArrowIsNotAMenu(t *testing.T) {
	s := "● Ran the build\n  ↓ 2 files changed\n  ↑ 3 files added\n  ↓  12  tokens\n❯ \n"
	if reason := autoApprovalDeclineReason(s, false); reason != "no focused numbered menu" {
		t.Fatalf("reason = %q", reason)
	}
	if reason := ariaJevScopeDecline(s); reason != "" {
		t.Fatalf("no focused option row, scope guard must stay silent, got %q", reason)
	}
}

func TestScrollMarkerSiblingKeepsIdleHintFromVetoingMenu(t *testing.T) {
	hint := "new task? /clear to save 40k tokens\n"
	menu := "❯ 1. Yes\n↓ 2. Yes, and don't ask again\n" + hint
	if got := dropClaudeIdleFooterMatches(menu, choicePromptRE.FindAllStringIndex(menu, -1)); len(got) != 1 {
		t.Fatalf("a menu with a scroll-marker sibling was vetoed as idle chrome")
	}
	// A lone focused row above the idle hint is still idle chrome.
	lone := "❯ 1. fix the build\n" + hint
	if got := dropClaudeIdleFooterMatches(lone, choicePromptRE.FindAllStringIndex(lone, -1)); len(got) != 0 {
		t.Fatalf("lone focused row above the idle hint was kept")
	}
}

func TestCodexMenuParsingUnchangedByScrollMarkers(t *testing.T) {
	s := "Would you like to run the following command?\n\n  $ go test ./...\n\n› 1. Yes, proceed (y)\n  2. Yes, and don't ask again for commands that start with `go test` (p)\n  3. No, and tell Codex what to do differently (esc)\n"
	m := parseAutoApprovalMenu(s, strings.Index(s, "›"))
	if len(m.options) != 3 || m.options[0] != "yes, proceed" || !strings.HasPrefix(m.options[2], "no, and tell codex") {
		t.Fatalf("codex options = %q", m.options)
	}
	if reason := autoApprovalDeclineReason(s, true); reason != "" {
		t.Fatalf("codex modal declined: %q", reason)
	}
}
