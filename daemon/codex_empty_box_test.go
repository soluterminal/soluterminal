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
		{"0.169 wrapped Windows directory with a space", []string{"› hello", "  Context 3% used · Daybreak Red high · C:\\Users\\Jo", "  Smith\\repo"}, 1},
	} {
		if got := codexFooterIndex(tc.lines); got != tc.want {
			t.Errorf("%s: codexFooterIndex=%d, want %d\n%s", tc.name, got, tc.want, strings.Join(tc.lines, "\n"))
		}
	}
}
