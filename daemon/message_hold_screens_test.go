package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal/grid"
)

// recordedPane is a pane without auto-approval, as most panes are: it has the
// ring buffer and the replay screen every terminal pane keeps, and no grid
// emulator. feed gives it recorded bytes the way readLoop does.
func recordedPane(t *testing.T, s menuScreenSession, agent string) *Pane {
	t.Helper()
	p := menuScreenPane(t, s, nil, agent)
	p.replayEmu = grid.New(int(s.Cols), int(s.Rows))
	t.Cleanup(func() { _ = p.replayEmu.Close() })
	return p
}

func feedRecorded(p *Pane, raw []byte) {
	_, _ = p.buffer.Write(raw)
	atomic.AddInt64(&p.TotalStrippedChars, int64(countStrippedChars(raw)))
	p.replayEmu.Feed(raw)
	_ = p.replayEmu.DrainReplies()
}

// Two reports had the same shape: an idle agent pane refused other agents'
// messages because the daemon misread what its screen showed.
//
//   - F-019 (ST-217): Codex's placeholder "Ask Codex to do anything" was read
//     as text the person had typed, so messages were held behind it.
//   - F-024 (ST-263): a Claude Code repaint was read as an approval menu, so
//     ask_pane refused the pane ("waiting on an approval menu") until someone
//     typed. Claude writes only the cells that changed: when a background
//     shell ended, the "1" of "1 shell still running" followed a repaint of
//     the input box's ❯ in the byte stream, which is the shape of a focused
//     "❯ 1" option row.
//
// Each session is a real recording (testdata/menu_screens). The pane is
// polled while the bytes arrive, as the state pollers do, and no input is
// ever recorded for it: nothing but the screen can clear a misreading. At
// every recorded step the pane must report a menu exactly when one is on
// screen, and an idle pane must take a message.
func TestMisreadScreensDoNotHoldAgentMessages(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.322 (Claude Code)", "codex-cli 0.166.0")
	type step struct {
		name string
		menu bool
	}
	for _, tc := range []struct {
		name, session, agent string
		codex                bool
		steps                []step
	}{
		{"F-024 claude shell count repaint", "claude_2_1_322_100x40", agentClaude, false,
			[]step{{name: "warm"}, {name: "two_shells"}, {name: "one_shell"}, {name: "no_shells"}}},
		// Claude draws this dialog below its input box. It is a real menu
		// (typed text and Enter answer it), so the pane stays held while it
		// shows.
		{"claude dialog below the input box", "claude_2_1_322_auto_120x40", agentClaude, false,
			[]step{{name: "idle"}, {name: "teach_dialog", menu: true}}},
		{"F-019 codex placeholder", "codex_0166_100x40", agentCodex, true,
			[]step{{name: "list_idle"}, {name: "echo_idle"}, {name: "approval", menu: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, raw := loadMenuScreenSession(t, tc.session)
			at := map[string]int{}
			for _, st := range s.Steps {
				at[st.Name] = st.Bytes
			}
			p := recordedPane(t, s, tc.agent)
			fed := 0
			for _, st := range tc.steps {
				end, ok := at[st.name]
				if !ok || end < fed {
					t.Fatalf("no recorded step %q after byte %d", st.name, fed)
				}
				const chunk = 512
				for fed < end {
					n := min(chunk, end-fed)
					feedRecorded(p, raw[fed:fed+n])
					fed += n
					p.computeStateWithHasCodex(tc.codex)
				}
				screen, _ := p.screenVisibleText()
				state := p.computeStateWithHasCodex(tc.codex)
				if st.menu {
					if state != StateChoicePrompt {
						t.Errorf("%s: state=%s with a menu on screen\n%s", st.name, state, screen)
					}
					continue
				}
				if state == StateChoicePrompt {
					t.Errorf("%s: choice-prompt with no menu on screen\n%s", st.name, screen)
				}
				none := func() (int, int64) { return 0, 0 }
				if reason := p.draftGuardReason(tc.codex, false, time.Now(), none); reason != "" {
					t.Errorf("%s: message held (%s) with an empty input box\n%s", st.name, reason, screen)
				}
				if !tc.codex {
					continue
				}
				// A screen source that lost its styles shows the placeholder
				// as plain text (the F-019 reading).
				prev := draftGuardScreen
				draftGuardScreen = func(p *Pane) (string, string, bool) {
					text, _, ok := paneScreenFaintBlanked(p)
					return text, text, ok
				}
				reason := p.draftGuardReason(true, false, time.Now(), none)
				draftGuardScreen = prev
				if reason != "" {
					t.Errorf("%s: message held (%s) behind the placeholder\n%s", st.name, reason, screen)
				}
			}
		})
	}
}

// ST-263b: the recorder ran inside another Claude session, so this recording
// has Claude's "Transcript saving is off" notice between the input box's
// closing rule and the status line. In Claude's auto mode the box reader took
// that row for text typed in the box, and every agent message to the pane was
// held. The notice is not the draft; text really typed in the box still is.
func TestClaudeNoticeRowUnderTheBoxIsNotADraft(t *testing.T) {
	s, raw := loadMenuScreenSession(t, "claude_2_1_322_auto_120x40")
	idle := 0
	for _, st := range s.Steps {
		if st.Name == "idle" {
			idle = st.Bytes
		}
	}
	p := recordedPane(t, s, agentClaude)
	feedRecorded(p, raw[:idle])
	none := func() (int, int64) { return 0, 0 }
	read := func() (lines []string, draft string, known bool) {
		screen, blanked, ok := paneScreenFaintBlanked(p)
		if !ok {
			t.Fatal("no screen")
		}
		typed := typedComposerScreen(screen, blanked, false)
		_, draft, known = conditionalClaudeComposer(typed)
		return strings.Split(screen, "\n"), draft, known
	}

	lines, draft, known := read()
	box := claudeComposerLine(lines)
	if box < 1 || box+3 >= len(lines) || !claudeBoxRule(lines[box-1]) || !claudeBoxRule(lines[box+1]) ||
		!strings.Contains(lines[box+2], "Transcript saving is off") || !newPromptFooterRE.MatchString(lines[box+3]) {
		t.Fatalf("recording: want box, notice row, status line\n%s", strings.Join(lines, "\n"))
	}
	if !known || draft != "" {
		t.Errorf("empty box under a notice row: draft=%q known=%v", draft, known)
	}
	if reason := p.draftGuardReason(false, false, time.Now(), none); reason != "" {
		t.Errorf("message held (%s) with an empty input box", reason)
	}

	// The person types in the box: Claude replaces the faint suggestion with
	// normal text on the same row.
	const typedText = "I want the release notes"
	feedRecorded(p, []byte(fmt.Sprintf("\x1b[%d;1H\x1b[0m\x1b[2K❯\u00a0%s", box+1, typedText)))
	if _, draft, known := read(); !known || draft != conditionalDraftText(typedText) {
		t.Errorf("typed draft: draft=%q known=%v", draft, known)
	}
	if reason := p.draftGuardReason(false, false, time.Now(), none); reason != heldBoxTextReason {
		t.Errorf("reason=%q with text typed in the box, want it held", reason)
	}
}
