package main

import (
	"strings"
	"testing"
	"time"
)

// ST-287 (tracker #71): on Codex 0.168 and 0.169, ask_pane to an idle pane
// showing only its faint placeholder was held ("that pane's input box has
// text") and never typed. The placeholder was still faint and read as empty;
// the footer under the box was not found. Codex 0.169 draws a hint row under
// its footer ("← for agents · ? for shortcuts", with "⚠ 1 warning · f2 to
// view" on the right), and once text is typed it wraps a long working
// directory onto a second footer row. A model whose display name does not
// start with GPT ("Daybreak Red") never matched the footer at all. With no
// footer the box ran to the bottom of the screen, so the footer was "text in
// the box", and spawn_pane never saw the pane's input box either.
//
// Each session is a real recording of an official Codex build
// (testdata/menu_screens/README.md): idle at an empty box, then "hello draft
// text" typed into it, then Ctrl+C clearing it. Nothing was submitted.
func TestCodexEmptyBoxAcrossVersions(t *testing.T) {
	for _, session := range []string{
		"codex_0166_empty_100x40", "codex_0166_warning_60x40",
		"codex_0167_empty_100x40", "codex_0167_warning_60x40",
		"codex_0168_empty_100x40", "codex_0168_warning_60x40", "codex_0168_daybreak_100x40",
		"codex_0169_empty_100x40", "codex_0169_warning_60x40", "codex_0169_daybreak_100x40",
		// ST-297 (tracker #72): 0.169.1 with the default status line, then
		// the reporter's configured one: no working directory and the
		// context item as "Context 100% left", wide and narrow with the
		// warning chip.
		"codex_01691_empty_100x40", "codex_01691_left_100x40", "codex_01691_leftwarn_60x40",
	} {
		t.Run(session, func(t *testing.T) {
			s, raw := loadMenuScreenSession(t, session)
			p := recordedPane(t, s, agentCodex)
			none := func() (int, int64) { return 0, 0 }
			fed := 0
			for _, st := range s.Steps {
				for fed < st.Bytes {
					n := min(512, st.Bytes-fed)
					feedRecorded(p, raw[fed:fed+n])
					fed += n
					p.computeStateWithHasCodex(true)
				}
				screen, blanked, ok := paneScreenFaintBlanked(p)
				if !ok {
					t.Fatal("no screen")
				}
				if !agentInputBoxVisible(screen, agentCodex) {
					t.Errorf("%s: Codex's input box not recognised\n%s", st.Name, screen)
				}
				if state := p.computeStateWithHasCodex(true); state == StateChoicePrompt || state == StateBusy {
					t.Errorf("%s: state=%s at the input box", st.Name, state)
				}
				draft, known := composerDraft(typedComposerScreen(screen, blanked, true), true)
				reason := p.draftGuardReason(true, false, time.Now(), none)
				if st.Name == "typed" {
					// The box reads exactly what was typed: the footer, a
					// wrapped footer row or the hint row are not part of it.
					if !known || draft != conditionalDraftText("hello draft text") {
						t.Errorf("typed: draft=%q known=%v\n%s", draft, known, screen)
					}
					if reason != heldBoxTextReason {
						t.Errorf("typed: reason=%q, want held behind the typed text", reason)
					}
					continue
				}
				if !known || draft != "" {
					t.Errorf("%s: draft=%q known=%v with an empty box\n%s", st.Name, draft, known, screen)
				}
				if reason != "" {
					t.Errorf("%s: message held (%s) with an empty box\n%s", st.Name, reason, screen)
				}
				// A screen source without styles shows the placeholder as
				// plain text; the guard still knows it (ST-217's fallback).
				if !codexComposerPlaceholder(screen) {
					t.Errorf("%s: placeholder fallback missed the empty box\n%s", st.Name, screen)
				}
			}
		})
	}
}

// codexFooterIndex ends Codex's box at its footer block and nowhere else: a
// line that only looks like part of it, with no footer where Codex draws one,
// leaves the box running to the end of the screen (held, never empty).
func TestCodexFooterIndexBlock(t *testing.T) {
	const box = "› Ask Codex to do anything"
	for _, tc := range []struct {
		name  string
		lines []string
		want  int
	}{
		{"0.166 footer", []string{box, "", "  Context 2% used · GPT-6.1-Sol high · ~/projects/demo"}, 2},
		{"narrow footer with warning", []string{box, "", "  Context 59% used · GPT-6.1-Sol high …  ⚠ 1 warning · f2 to view"}, 2},
		{"no login", []string{box, "", "  gpt-5.1 default · /home/me/repo"}, 2},
		{"0.169 hint row", []string{box, "", "  Context 0% used · GPT-6.1-Sol low · ~/repo", "  ← for agents · ? for shortcuts", "", ""}, 2},
		{"0.169 hint row and warning", []string{box, "", "  gpt-5.1 default · /home/me/repo", "  ← for agents · ? for shortcuts      ⚠ 2 warnings · f2 to view"}, 2},
		{"0.169 wrapped directory", []string{"› hello", "", "  gpt-5.1 default · /home/me/projects/a-very-long-folder-", "  name/repo"}, 2},
		{"non-GPT model", []string{box, "", "  Context 12% used · Daybreak Red high · C:\\Users\\me\\repo"}, 2},
		{"non-GPT model, no login", []string{box, "", "  Daybreak Red high · C:\\Users\\me\\repo"}, 2},
		{"UNC directory", []string{box, "", "  gpt-5.1 default · \\\\server\\share\\repo"}, 2},
		// ST-297: the status line is the person's (`[tui] status_line`).
		{"context left, no directory", []string{box, "", "  Context 100% left · gpt-5.1 default", "  ← for agents · ? for shortcuts"}, 2},
		{"context left, no directory, typed", []string{"› hello", "", "  Context 100% left · gpt-5.1 default"}, 2},
		{"0.166 context left with the warning chip", []string{box, "", "  Context 96% left · GPT-6-Astra medium      ⚠ 2 warnings · f2 to view"}, 2},
		{"context item last", []string{box, "", "  GPT-6-Astra medium · Context 50% left"}, 2},
		{"context item alone", []string{box, "", "  Context 100% left"}, 2},
		{"context item alone with the warning chip on its row", []string{box, "", "  Context 96% left      ⚠ 2 warnings · f2 to view"}, 2},
		// Not a footer block.
		{"text last", []string{"› hello", "  world"}, -1},
		{"shell prompt under an old footer", []string{box, "", "  gpt-5.1 default · /home/me/repo", "me@pc:~/repo$"}, -1},
		{"queue hint under a footer-shaped draft line", []string{"› fix", "  gpt-5.1 default · /home/me/repo", "  tab to queue message"}, -1},
		{"hint row alone", []string{"› hello", "  ← for agents · ? for shortcuts"}, -1},
		{"three hint rows", []string{box, "  gpt-5.1 default · /home/me/repo", "  ? for shortcuts", "  ? for shortcuts", "  ? for shortcuts"}, -1},
		{"hint row with other text", []string{box, "  gpt-5.1 default · /home/me/repo", "  ? for shortcuts · and my notes"}, -1},
		{"continuation under an elided footer", []string{"› hello", "  gpt-5.1 default · /home/me/projects/a…", "  name/repo"}, -1},
		{"continuation with a separator", []string{"› hello", "  gpt-5.1 default · /home/me/projects/a-", "  name · more"}, -1},
		{"continuation not indented as the footer", []string{"› hello", "  gpt-5.1 default · /home/me/projects/a-", "name/repo"}, -1},
		{"words under a footer-shaped draft line", []string{"› fix", "  gpt-5.1 default · /home/me/repo", "  and then the rest of my note"}, -1},
		// A status line without the context item or the directory, or none
		// at all, is not told apart from lines of a draft, idle or not:
		// the box runs to the end of the screen (held, never empty).
		{"model alone above the key hints", []string{box, "", "  GPT-6-Astra medium", "  ← for agents · ? for shortcuts"}, -1},
		{"git branch and project above the key hints and chip", []string{box, "", "  main · soluterminal", "  ← for agents · ? for shortcuts   ⚠ 1 warning · f2 to view"}, -1},
		{"no status line", []string{box, "", "  ← for agents · ? for shortcuts                       100% context left"}, -1},
		{"model alone, typed", []string{"› hello", "", "  GPT-6-Astra medium"}, -1},
		{"no status line, typed: the gauge alone", []string{"› hello", "", "                                   100% context left"}, -1},
		{"running turn: a queue hint on any key, with the gauge", []string{"› hello", "", "  alt+q to queue message                 98% context left"}, -1},
		{"model alone under the chip, typed", []string{"› hello", "", "  GPT-6-Astra medium", "                 ⚠ 1 warning · f2 to view"}, -1},
		{"key hints under a draft line with no separator", []string{"› hello", "  my second line", "  ← for agents · ? for shortcuts"}, -1},
		{"hint row typed after a blank line", []string{"›", "", "  ? for shortcuts"}, -1},
		{"key hints under a status-shaped row below a draft", []string{"› hello", "", "  GPT-6-Astra medium", "  ← for agents · ? for shortcuts"}, -1},
		{"no status line, typed, with the warning chip", []string{"› hello", "", "                       ⚠ 1 warning · f2 to view"}, -1},
		{"key hints and gauge under a draft", []string{"› hello", "", "  ← for agents · ? for shortcuts                       100% context left"}, -1},
		{"key hints under the box's second draft line", []string{"›", "  my line", "", "  GPT-6-Astra medium", "  ← for agents · ? for shortcuts"}, -1},
		{"key hints under the box row's continuation at another indent", []string{box, "", "    indented note", "  ← for agents · ? for shortcuts"}, -1},
		{"context left in the middle of a sentence", []string{"› hello", "  the Context 5% left is wrong"}, -1},
		{"0.169 wrapped Windows directory with a space", []string{"› hello", "  Context 3% used · Daybreak Red high · C:\\Users\\Jo", "  Smith\\repo"}, 1},
	} {
		if got := codexFooterIndex(tc.lines); got != tc.want {
			t.Errorf("%s: codexFooterIndex=%d, want %d\n%s", tc.name, got, tc.want, strings.Join(tc.lines, "\n"))
		}
	}
}

// ST-297: Codex status lines the footer shape does not cover, the model alone
// and none at all (real 0.169.1 recordings). Their rows are not told apart
// from lines of a draft, so the box is never read as empty: agent messages
// wait for the person, at the idle box as with text typed, and are never
// typed into a draft. Reading these idle boxes as empty needs evidence the
// text cannot fake, such as the cursor row.
func TestCodexUnreadStatusLinesNeverReadEmpty(t *testing.T) {
	for _, session := range []string{"codex_01691_model_100x40", "codex_01691_nostatus_100x40"} {
		t.Run(session, func(t *testing.T) {
			s, raw := loadMenuScreenSession(t, session)
			p := recordedPane(t, s, agentCodex)
			none := func() (int, int64) { return 0, 0 }
			fed := 0
			for _, st := range s.Steps {
				feedRecorded(p, raw[fed:st.Bytes])
				fed = st.Bytes
				screen, blanked, ok := paneScreenFaintBlanked(p)
				if !ok {
					t.Fatal("no screen")
				}
				if draft, known := composerDraft(typedComposerScreen(screen, blanked, true), true); known && draft == "" {
					t.Errorf("%s: box read as empty\n%s", st.Name, screen)
				}
				if reason := p.draftGuardReason(true, false, time.Now(), none); reason != heldBoxTextReason {
					t.Errorf("%s: reason=%q, want held\n%s", st.Name, reason, screen)
				}
			}
		})
	}
}
