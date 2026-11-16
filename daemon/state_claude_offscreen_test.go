package main

import (
	"strings"
	"sync/atomic"
	"testing"

	"soluterminal/grid"
)

const claudeOffscreenMenu = "Bash command\n  printf 1 > /tmp/f1\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again for printf commands\n  3. No\n\nEsc to cancel · Tab to amend"

// After an answered Claude dialog, Claude repaints the rows it occupied and
// the raw tail matches the menu again at a newer position. With the rendered
// screen showing only the composer, that is not a pending prompt.
func TestComputeStateIgnoresClaudeMenuOnlyInScrollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claude bool
		screen string
		want   PaneRuntimeState
	}{
		{"claude, menu cleared from screen", true, "● Done.\n\n❯ \n  ? for shortcuts", ""},
		{"claude, menu on screen", true, claudeOffscreenMenu, StateChoicePrompt},
		{"other TUI keeps the raw scan", false, "● Done.\n\n❯ \n  ? for shortcuts", StateChoicePrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaneWithTail(t, "pane-offscreen", "Offscreen", "● Running\n"+claudeOffscreenMenu+"\n")
			if tc.claude {
				p.ClaudeSessionID = "session-offscreen"
			}
			seedGridMenu(t, p, tc.screen)
			got := p.computeStateWithHasCodex(false)
			if tc.want == StateChoicePrompt && got != StateChoicePrompt {
				t.Fatalf("state = %s, want choice-prompt", got)
			}
			if tc.want != StateChoicePrompt && got == StateChoicePrompt {
				t.Fatal("raw-only Claude menu reported as a pending choice prompt")
			}
		})
	}
}

// The menu reappears on screen later (a genuinely new dialog): the raw match
// acknowledged while off screen must not suppress it.
func TestClaudeOffscreenMenuDoesNotSuppressLaterRealMenu(t *testing.T) {
	p := testPaneWithTail(t, "pane-offscreen-later", "Offscreen later", "● Running\n"+claudeOffscreenMenu+"\n")
	p.ClaudeSessionID = "session-offscreen-later"
	seedGridMenu(t, p, "● Done.\n\n❯ ")
	if p.computeStateWithHasCodex(false) == StateChoicePrompt {
		t.Fatal("off-screen menu reported")
	}
	next := "\n● Next\n" + claudeOffscreenMenu + "\n"
	if _, err := p.buffer.Write([]byte(next)); err != nil {
		t.Fatal(err)
	}
	atomic.AddInt64(&p.TotalStrippedChars, int64(len(stripANSI([]byte(next)))))
	seedGridMenu(t, p, claudeOffscreenMenu)
	if got := p.computeStateWithHasCodex(false); got != StateChoicePrompt {
		t.Fatalf("real menu state = %s, want choice-prompt", got)
	}
}

// seedReplayScreen paints text on the pane's replay screen, the only rendered
// screen a pane without auto-approval has.
func seedReplayScreen(t *testing.T, p *Pane, text string) {
	t.Helper()
	p.replayEmu = grid.New(int(p.Cols), int(p.Rows))
	t.Cleanup(func() { _ = p.replayEmu.Close() })
	p.replayEmu.Feed([]byte("\x1b[2J\x1b[H" + strings.ReplaceAll(text, "\n", "\r\n")))
	_ = p.replayEmu.DrainReplies()
}

// Without a grid emulator, the replay screen sets a raw-only Claude match
// aside only when it shows Claude's input box with no dialog below it. Any
// other screen keeps the match: a missed menu lets a message be typed into a
// permission prompt.
func TestClaudeRawMenuMatchAgainstReplayScreen(t *testing.T) {
	rule := strings.Repeat("─", 60)
	box := rule + "\n❯ \n" + rule + "\n  ~/repo [Opus | ctx: 4%]\n  ⏸ manual mode on · 1 shell\n"
	narrow := strings.Repeat("─", inputBoxMinRule-1)
	teach := rule + "\n  Teach auto mode about your environment?\n\n%s\n\n  Enter to confirm · Esc to cancel\n"
	for _, tc := range []struct {
		name   string
		claude bool
		screen string
		prompt bool
	}{
		{"input box on screen", true, "● Done.\n\n" + box, false},
		{"working above the input box", true, "✽ Brewing… (4s · ↓ 336 tokens)\n\n" + box, false},
		{"menu on screen", true, claudeOffscreenMenu, true},
		{"dialog below the input box", true, box + strings.Replace(teach, "%s", "  ❯ 1. Yes\n    2. Not now\n    3. Don't show again", 1), true},
		{"dialog below the input box, highlight moved", true, box + strings.Replace(teach, "%s", "    1. Yes\n  ❯ 2. Not now\n    3. Don't show again", 1), true},
		{"blank screen", true, "", true},
		{"no input box on screen", true, "● Done.\n\nsome output\n", true},
		{"input box narrower than the minimum", true, "● Done.\n\n" + narrow + "\n❯ \n" + narrow + "\n", true},
		{"other TUI keeps the raw scan", false, "● Done.\n\n" + box, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaneWithTail(t, "pane-replay-screen", "Replay screen", "● Running\n"+claudeOffscreenMenu+"\n")
			if tc.claude {
				p.ClaudeSessionID = "session-replay-screen"
			}
			seedReplayScreen(t, p, tc.screen)
			// Twice: the first poll acknowledges the match, the second is the
			// reading that used to stay latched with no input after it.
			for poll := 1; poll <= 2; poll++ {
				if got := p.computeStateWithHasCodex(false) == StateChoicePrompt; got != tc.prompt {
					t.Fatalf("poll %d: choice-prompt=%v, want %v", poll, got, tc.prompt)
				}
			}
		})
	}
}

// A dialog that closes with no input (Claude starts a turn when a background
// shell ends) releases the pane: the state follows the screen.
func TestClaudeMenuClosedWithoutInputReleasesThePane(t *testing.T) {
	rule := strings.Repeat("─", 60)
	p := testPaneWithTail(t, "pane-closed-dialog", "Closed dialog", "● Running\n"+claudeOffscreenMenu+"\n")
	p.ClaudeSessionID = "session-closed-dialog"
	seedReplayScreen(t, p, claudeOffscreenMenu)
	if got := p.computeStateWithHasCodex(false); got != StateChoicePrompt {
		t.Fatalf("menu on screen: state=%s", got)
	}
	p.replayEmu.Feed([]byte("\x1b[2J\x1b[H" + strings.ReplaceAll("● Done.\n\n"+rule+"\n❯ \n"+rule+"\n  ? for shortcuts\n", "\n", "\r\n")))
	_ = p.replayEmu.DrainReplies()
	if got := p.computeStateWithHasCodex(false); got == StateChoicePrompt {
		t.Fatal("still choice-prompt after the dialog left the screen")
	}
}
