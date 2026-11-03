package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// testdata/menu_screens holds raw PTY recordings of real Claude Code 2.1.310
// and Codex 0.166 sessions (scripts/capture-tui-screens.py, paths scrubbed at
// equal length). Each step's offset ends the stream at a moment the session
// was settled on that screen, so a prefix is exactly what a pane had received.
type menuScreenSession struct {
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
	Steps []struct {
		Name  string `json:"name"`
		Bytes int    `json:"bytes"`
	} `json:"steps"`
}

func loadMenuScreenSession(t *testing.T, name string) (menuScreenSession, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "menu_screens", name+".raw"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("testdata", "menu_screens", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var s menuScreenSession
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s, raw
}

func stateTailOf(p *Pane) string {
	s, _ := p.stateTail()
	return s
}

func menuScreenPane(t *testing.T, s menuScreenSession, raw []byte, agent string) *Pane {
	t.Helper()
	p := testPaneWithTail(t, "menu-screen", "menu screen", string(raw))
	p.Agent = agent
	p.Cols, p.Rows = s.Cols, s.Rows
	return p
}

func TestRealMenuScreens(t *testing.T) {
	// Which recorded steps show a live menu. Everything else is an idle or
	// answered screen whose transcript still holds a "❯ 1." / "› 1." row:
	// the "1. yes it works in desktop" prompt echo under a reply that ends
	// in a numbered list (seen on a lead pane on 2026-11-03), a draft
	// starting "1.", or the echo left above a cancelled dialog.
	live := map[string]bool{
		"permission":       true,
		"permission_amend": true,
		"ask_step1":        true,
		"ask_step1_moved":  true,
		"ask_step2":        true,
		"ask_step2_typed":  true,
		"approval":         true,
		"boot":             true, // Codex folder-trust menu
	}
	sessions := []struct {
		name  string
		agent string
		codex bool
	}{
		{"claude_2_1_310_100x40", "claude", false},
		{"claude_2_1_310_49x40", "claude", false},
		{"codex_0166_100x40", "codex", true},
	}
	seen := map[string]bool{}
	for _, sess := range sessions {
		s, raw := loadMenuScreenSession(t, sess.name)
		for _, step := range s.Steps {
			step := step
			seen[step.Name] = true
			t.Run(sess.name+"/"+step.Name, func(t *testing.T) {
				prefix := raw[:step.Bytes]
				want := live[step.Name]

				// Raw-tail path: no grid emulator, as for an unwatched pane.
				p := menuScreenPane(t, s, prefix, sess.agent)
				rawState := p.computeStateWithHasCodex(sess.codex)
				// Codex paints its trust menu as cell diffs only the screen
				// fallback reads, and a cancelled Codex approval keeps its
				// stale "Would you like to run" header in the raw tail until
				// the Esc input acks it (TestRealMenuScreensAnswered).
				rawExempt := sess.codex && (step.Name == "boot" || step.Name == "approval_cancelled")
				// Claude redraws only changed cells: its raw tail never ends in a
				// complete input box (the bottom rule is not repainted, and a
				// draft is written after the footer rows), so the veto, which
				// needs the box to be the final structure, cannot fire there.
				// The rendered screen clears those rows (claudeMenuOffScreen,
				// grid path below). A live menu must still match on the raw path.
				rawFalsePositiveKept := !sess.codex && !want
				if rawFalsePositiveKept {
					if rawState != StateChoicePrompt && len(choicePromptMatches(stateTailOf(p))) > 0 {
						t.Errorf("raw-tail state=%s despite a kept match", rawState)
					}
				} else if !rawExempt && (rawState == StateChoicePrompt) != want {
					t.Errorf("raw-tail state=%s, live menu=%v", rawState, want)
				}

				// Grid path: the rendered screen.
				g := menuScreenPane(t, s, prefix, sess.agent)
				g.ensureGrid()
				defer g.shutdownGrid()
				grid, ok := g.gridVisibleText()
				if !ok {
					t.Fatal("no rendered grid")
				}
				if got := isGridChoicePromptText(grid, sess.codex); got != want {
					t.Errorf("grid choice prompt=%v, want %v\n%s", got, want, grid)
				}
				if got := g.computeStateWithHasCodex(sess.codex); !rawExempt && (got == StateChoicePrompt) != want {
					t.Errorf("grid state=%s, live menu=%v", got, want)
				}
				if !want {
					return
				}
				// The live menu keeps its own focused row as the newest match,
				// so identity, auto-approve and answer_prompt read this menu.
				if strings.HasPrefix(step.Name, "ask_") || step.Name == "boot" {
					// AskUserQuestion keeps its footer through every step, which is
					// what menuInputDrawn needs once the highlight moves.
					if step.Name != "boot" && !menuDialogFooterOnScreen(grid) {
						t.Errorf("dialog footer missing from screen\n%s", grid)
					}
					return
				}
				matches := approvalPromptMatches(grid, sess.codex)
				if len(matches) == 0 {
					t.Fatalf("no approval match on grid\n%s", grid)
				}
				row := grid[matches[len(matches)-1][0]:]
				if i := strings.IndexByte(strings.TrimLeft(row, "\n"), '\n'); i > 0 {
					row = strings.TrimLeft(row, "\n")[:i]
				}
				if !strings.Contains(row, "Yes") {
					t.Errorf("newest match is not the dialog's focused row: %q", row)
				}
			})
		}
	}
	for name := range live {
		if !seen[name] {
			t.Errorf("no recorded step %q", name)
		}
	}
}

// An answered menu stops reading as a prompt once the answering key is
// recorded, on Claude (whose input box returns) and on Codex (whose stale
// raw-tail header is acked by the Esc input).
func TestRealMenuScreensAnswered(t *testing.T) {
	cases := []struct {
		session, menu, answered, agent string
		codex                          bool
	}{
		{"claude_2_1_310_100x40", "permission", "permission_cancelled", "claude", false},
		{"claude_2_1_310_49x40", "permission_amend", "permission_cancelled", "claude", false},
		{"claude_2_1_310_49x40", "ask_step2_typed", "ask_cancelled", "claude", false},
		{"codex_0166_100x40", "approval", "approval_cancelled", "codex", true},
	}
	for _, tc := range cases {
		t.Run(tc.session+"/"+tc.answered, func(t *testing.T) {
			s, raw := loadMenuScreenSession(t, tc.session)
			at := map[string]int{}
			for _, step := range s.Steps {
				at[step.Name] = step.Bytes
			}
			for _, grid := range []bool{false, true} {
				p := menuScreenPane(t, s, raw[:at[tc.menu]], tc.agent)
				if grid {
					p.ensureGrid()
				}
				if got := p.computeStateWithHasCodex(tc.codex); got != StateChoicePrompt {
					t.Fatalf("grid=%v: live menu state=%s", grid, got)
				}
				// The key that answered it, then everything the TUI drew after.
				atomic.StoreInt64(&p.LastInputStrippedPos, atomic.LoadInt64(&p.TotalStrippedChars))
				rest := raw[at[tc.menu]:at[tc.answered]]
				if _, err := p.buffer.Write(rest); err != nil {
					t.Fatal(err)
				}
				atomic.AddInt64(&p.TotalStrippedChars, int64(len(stripANSI(rest))))
				if grid {
					p.subsMu.Lock()
					p.feedGridPayloadLocked(rest, true)
					_ = p.gridEmu.DrainReplies()
					p.subsMu.Unlock()
				}
				if got := p.computeStateWithHasCodex(tc.codex); got == StateChoicePrompt {
					t.Errorf("grid=%v: answered menu still reads as choice-prompt", grid)
				}
				if grid {
					p.shutdownGrid()
				}
			}
		})
	}
}

func TestDropIdleComposerMatches(t *testing.T) {
	rule := strings.Repeat("─", 49)
	short := strings.Repeat("─", inputBoxMinRule-1)
	claudeIdle := rule + "\n❯ \n" + rule + "\n  ~/repo [Opus | ctx: 4%]\n  ⏸ manual mode on\n"
	codexIdle := "› Ask Codex to do anything\n\n  Context 1% used · GPT-6.1-Sol high · ~/repo\n"
	listReply := "● 1. #931 merged the lead notices fix\n  2. The release notes need a line\n  3. The release steps.\n\n✻ Cooked for 40s · done 4:30 PM\n\n"
	claudeMenu := " Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and switch to auto mode\n   3. No\n\n Esc to cancel · Tab to amend\n"
	askMenu := rule + "\n←  ☐ Colour  ☐ Size  ✔ Submit  →\nWhich colour?\n❯ 1. Red\n  2. Blue\n  3. Type something.\n" + rule + "\n  4. Chat about this\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel\n"
	codexMenu := "  Would you like to run the following command?\n\n  $ echo hi > a.txt\n\n› 1. Yes, proceed (y)\n  2. No, and tell Codex what to do differently (esc)\n\n  Press enter to confirm or esc to cancel\n"

	cases := []struct {
		name string
		s    string
		menu bool
	}{
		// False positives: transcript or composer text above/in the input box.
		{"claude echo under list reply", listReply + "❯ 1. yes it works in desktop\n\n● ok\n\n" + claudeIdle, false},
		{"claude echo, nbsp padding", "  ❯ 1. yes\n" + claudeIdle, false},
		{"claude draft in input box", listReply + rule + "\n❯ 1. draft that looks like a list\n" + rule + "\n", false},
		{"claude wrapped draft in input box", rule + "\n❯ 1. a draft that wraps\n  onto a second row\n" + rule + "\n", false},
		{"claude rows joined", "   ❯  1. yes it works      ✻ Brewed for 2s   " + rule + "  ❯     " + rule + "   ctx: 4%", false},
		{"codex echo above composer", "› 1. yes it works in desktop\n\n• ok\n\n  Worked for 8s\n\n" + codexIdle, false},
		{"codex queued draft footer", "› 1. queued\n\n  tab to queue message          98% context left\n", false},

		// Real menus stay detected.
		{"claude permission", listReply + "❯ some prompt\n● Bash(touch a)\n" + rule + "\n Bash command\n" + claudeMenu, true},
		{"claude permission after echo", "❯ 1. yes it works\n● Bash(touch a)\n" + rule + "\n" + claudeMenu, true},
		{"claude stale box above menu", claudeIdle + "● Bash(touch a)\n" + rule + "\n" + claudeMenu, true},
		{"ask user question", "❯ ask me\n" + askMenu, true},
		{"codex approval", codexIdle + "• Running echo hi\n" + codexMenu, true},
		{"codex approval after echo", "› 1. yes\n\n• ok\n" + codexMenu, true},
		// Agent-controlled text after a real menu's focused row can draw a
		// box or composer shape: a directory in "don't ask again for …",
		// an AskUserQuestion label or description, a command prefix. The
		// dialog's remaining rows and footer always follow, so it stays a menu.
		{"claude permission, box in directory", " Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for ls commands in /home/u/" + rule + "❯" + rule + "\n   3. No\n\n Esc to cancel · Tab to amend\n", true},
		{"claude permission, wrapped box in directory", " Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for ls commands in\n      " + rule + "\n❯ \n" + rule + "\n   3. No\n\n Esc to cancel · Tab to amend\n", true},
		{"claude permission, box and footer in directory", " Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again in\n" + rule + "\n❯ \n" + rule + "\n  ~/repo [Opus | ctx: 4%]\n   3. No\n\n Esc to cancel\n", true},
		{"ask user question, box in label", rule + "\nWhich colour?\n❯ 1. " + rule + "❯" + rule + "\n  2. Blue\n  3. Type something.\n" + rule + "\n  4. Chat about this\nEnter to select · Esc to cancel\n", true},
		{"ask user question, box in description", rule + "\nWhich colour?\n❯ 1. Red\n     " + rule + "\n❯ \n" + rule + "\n  2. Blue\n  3. Type something.\n" + rule + "\n  4. Chat about this\nEnter to select · Esc to cancel\n", true},
		{"codex approval, footer in command", "  Would you like to run the following command?\n\n  $ echo '› x Context 5% used'\n\n› 1. Yes, proceed (y)\n  2. No, and tell Codex what to do differently (esc)\n\n  Press enter to confirm or esc to cancel\n", true},
		{"codex approval, composer in prefix", "  Would you like to run the following command?\n\n  $ ls\n\n› 1. Yes, proceed (y)\n  2. Yes, and don't ask again for commands that start with `ls\n› Ask Codex\n  Context 5% used` (p)\n  3. No, and tell Codex what to do differently (esc)\n\n  Press enter to confirm or esc to cancel\n", true},
		// A cancelled menu's rows above the returned box are sibling options:
		// the veto keeps the match, and the answering key clears the state.
		{"claude cancelled menu, box back", claudeMenu + "  ⎿  Interrupted\n" + claudeIdle, true},
		// Without a complete input-box signature below it, a "❯ 1." row keeps
		// matching: an unproven veto would hide a real prompt.
		{"lone rule below match", "❯ 1. yes\n  2. no\n" + rule + "\n", true},
		{"echo, then a numbered reply", "❯ 1. yes\n● 1. a\n  2. b\n" + claudeIdle, true},
		{"box narrower than the minimum", "❯ 1. yes\n" + short + "\n❯ \n" + short + "\n", true},
		{"box not last", "❯ 1. yes\n" + claudeIdle + "● more output\n", true},
		{"footer too long", "❯ 1. yes\n" + claudeIdle + strings.Repeat("  status\n", idleTrailerMaxLines), true},
		{"box with oversized draft", rule + "\n❯ 1. " + strings.Repeat("x", inputBoxDraftBytes+10) + "\n" + rule + "\n", true},
		{"codex footer too far", "› 1. yes\n" + strings.Repeat(" ", codexComposerBytes) + "Context 1% used\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isChoicePromptText(tc.s); got != tc.menu {
				t.Fatalf("isChoicePromptText=%v, want %v\n%s", got, tc.menu, tc.s)
			}
		})
	}
}
