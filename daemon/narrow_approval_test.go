package main

import (
	"fmt"
	"strings"
	"testing"
)

// The TUI reserves horizontal space for modal padding, so it can word-wrap
// before the terminal's last column. Keep physical rows within each width.
func narrowApprovalHeader(cols uint16, edit bool) string {
	if cols == 40 {
		if edit {
			return "  Would you like to make the\n  following\n  edits?"
		}
		return "  Would you like to run the\n  following\n  command?"
	}
	if edit {
		return "  Would you like to make the following\n  edits?"
	}
	return "  Would you like to run the following\n  command?"
}

func narrowApprovalGrid(t *testing.T, cols, rows uint16, text string) (*Pane, string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > int(cols) {
			t.Fatalf("fixture row exceeds %d columns: %q", cols, line)
		}
	}
	p := testPaneWithTail(t, "narrow-approval", "Narrow approval", "render residue\n")
	t.Cleanup(p.shutdownGrid)
	p.Cols, p.Rows = cols, rows
	seedGridMenu(t, p, text)
	grid, ok := p.gridVisibleText()
	if !ok {
		t.Fatal("missing rendered grid")
	}
	return p, grid
}

func TestNarrowCodexApprovalHeaders(t *testing.T) {
	for _, cols := range []uint16{40, 53} {
		for _, edit := range []bool{false, true} {
			for _, focused := range []bool{false, true} {
				t.Run(fmt.Sprintf("cols=%d/edit=%v/focused=%v", cols, edit, focused), func(t *testing.T) {
					body := "$ pwd"
					if edit {
						body = "  notes.txt (+1 -1)\n  1 -old\n  1 +new"
					}
					text := narrowApprovalHeader(cols, edit) + "\n\n" + body + "\n\n"
					if focused {
						text += "› 1. Yes, proceed (y)\n  2. No (esc)\n"
					}
					// Edit fallback needs a footer; command fallback also has $.
					text += "Press enter to\n  confirm or esc to cancel"
					p, grid := narrowApprovalGrid(t, cols, 32, text)
					if len(approvalPromptMatches(grid, true)) == 0 || !isGridChoicePromptText(grid, true) {
						t.Fatal("wrapped modal was not detected")
					}
					if reason := autoApprovalDeclineReason(grid, true); reason != "" {
						t.Fatalf("wrapped modal declined: %s", reason)
					}
					if got := p.computeStateWithHasCodex(true); got != StateChoicePrompt {
						t.Fatalf("state=%s, want choice-prompt", got)
					}
					if !focused && isAutoApprovalPromptText(grid, false) {
						t.Fatal("header-only fallback must require Codex")
					}
					if !focused {
						// Both supported footer anchors must tolerate wrapping.
						grid = strings.Replace(grid, "Press enter to", "Esc to", 1)
						grid = strings.Replace(grid, "confirm or esc to cancel", "cancel", 1)
						if !isAutoApprovalPromptText(grid, true) {
							t.Fatal("wrapped Esc-to-cancel fallback declined")
						}
					}
					if !edit && !focused {
						if got := codexCommandApprovalIdentity(grid); got == "" {
							t.Fatal("wrapped command lost its prompt identity")
						}
					}
				})
			}
		}
	}
}

func TestNarrowClaudeApprovalPhrase(t *testing.T) {
	for _, cols := range []uint16{40, 53} {
		t.Run(fmt.Sprint(cols), func(t *testing.T) {
			// No "Yes, <qualifier>" row or tool title: only the permission
			// phrase can establish the gate. The renderer wrapped that phrase.
			question := " Confirm this change: do you want to\n  make this edit to notes.txt?"
			if cols == 53 {
				question = " Confirm this pending change: do you want to make\n  this edit to notes.txt?"
			}
			_, grid := narrowApprovalGrid(t, cols, 32, question+"\n❯ 1. Yes\n  2. No (esc)")
			if reason := autoApprovalDeclineReason(grid, false); reason != "" {
				t.Fatalf("wrapped permission phrase declined: %s", reason)
			}
		})
	}
}

func TestNarrowApprovalSafetyChecks(t *testing.T) {
	for _, cols := range []uint16{40, 53} {
		for _, tc := range []struct{ name, text, reason string }{
			{"selection", "Choose how to display\n  the result:\n❯ 1. Short\n  2. Detailed", "choice menu, not a permission prompt"},
			{"question footer", "Do you want to\n  proceed?\n❯ 1. Yes\n  2. No\nEnter to\n  select", "question menu"},
			{"plan", "Would you like to\n  proceed?\n❯ 1. Yes\n  2. No, keep\n  planning", "plan-exit menu"},
			{"billing", "Use your\n  reset?\nDo you want to\n  proceed?\n❯ 1. Yes\n  2. No", "billing/reset confirmation"},
			{"login", "Sign in\n  with Google?\nDo you want to\n  proceed?\n❯ 1. Yes\n  2. No", "login/account menu"},
			{"focused mode switch", "Do you want to\n  proceed?\n❯ 1. Yes, and use auto\n  mode\n  2. No", "focused option is not an accept-once approval"},
			{"focused non-approval", "Do you want to\n  proceed?\n❯ 1. Explain\n  the change\n  2. No", "focused option is not an accept-once approval"},
			{"missing escape", "Do you want to\n  proceed?\n❯ 1. Yes\n  2. Explain", "menu offers no No/Cancel escape option"},
		} {
			t.Run(fmt.Sprintf("%d/%s", cols, tc.name), func(t *testing.T) {
				_, grid := narrowApprovalGrid(t, cols, 32, tc.text)
				for _, codex := range []bool{false, true} {
					if got := autoApprovalDeclineReason(grid, codex); got != tc.reason {
						t.Fatalf("Codex=%v: reason=%q, want %q", codex, got, tc.reason)
					}
				}
				// A stale wrapped Codex modal cannot override the current menu.
				stale := narrowApprovalHeader(cols, false) + "\n$ pwd\n› 1. Yes, proceed\n  2. No\n\n"
				if got := autoApprovalDeclineReason(stale+grid, true); got == "" {
					t.Fatal("stale Codex header approved the current decision")
				}
			})
		}
	}
}

func TestShortApprovalScreens(t *testing.T) {
	for _, cols := range []uint16{40, 53} {
		for _, rows := range []uint16{10, 12} {
			header := narrowApprovalHeader(cols, false)
			for _, tc := range []struct {
				name, text  string
				codex, want bool
			}{
				// Painting more than rows scrolls the question out of the real
				// emulator. A bare Yes/No menu then has no permission signal.
				{"question scrolled out, bare Yes", "Do you want to proceed?\n" + strings.Repeat("payload line\n", 14) + "❯ 1. Yes\n  2. No", false, false},
				// Always mode already recognizes Yes, proceed + No without the
				// question. Cautious mode still needs the missing action context.
				{"question scrolled out, qualified Yes", header + "\n" + strings.Repeat("payload line\n", 14) + "› 1. Yes, proceed\n  2. No", true, true},
				{"No clipped, no footer", header + "\n$ pwd\n› 1. Yes, proceed\n  2. Explain", true, false},
				{"No clipped, escape footer visible", header + "\n$ pwd\n› 1. Yes, proceed\n  2. Explain\nEsc to\n  cancel", true, true},
				{"focused row absent, Codex fallback", header + "\n$ pwd\nPress enter to\n  confirm", true, true},
				{"wrapped Yes No selection", "Choose whether to include\n  the summary:\n❯ 1. Yes\n  2. No", false, false},
			} {
				t.Run(fmt.Sprintf("%dx%d/%s", cols, rows, tc.name), func(t *testing.T) {
					p, grid := narrowApprovalGrid(t, cols, rows, tc.text)
					p.forceCodexForTest = tc.codex
					// Keep an intact old permission in the raw tail. A clipped
					// rendered choice must be authoritative over this fallback.
					if _, err := p.buffer.Write([]byte(codexCommandApprovalScreen)); err != nil {
						t.Fatal(err)
					}
					_, got, source := currentAutoApprovalPrompt(p)
					if got != tc.want || source != "grid" {
						t.Fatalf("approval=%v source=%q, want %v from grid; reason=%s; screen:\n%s", got, source, tc.want, autoApprovalDeclineReason(grid, tc.codex), grid)
					}
					if strings.HasPrefix(tc.name, "question scrolled out") {
						if strings.Contains(grid, "Do you want") || strings.Contains(grid, "Would you like") {
							t.Fatal("fixture did not scroll the question off screen")
						}
						if cautiousRenderedHasActionContext(grid) {
							t.Fatal("clipped action must stay with the human in cautious mode")
						}
					}
				})
			}
		}
	}
}
