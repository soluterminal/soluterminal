package main

import (
	"strings"
	"testing"
	"time"
)

// Codex's footer when the pane is too narrow for its working directory, as
// @ops showed it on 2026-11-03 (testdata/codex_0166/idle_narrow.txt).
const (
	codexNarrowFooter     = "  Context 59% used · GPT-6.1-Sol high …  ⚠ 1 warning · f2 to view"
	codexNarrowFooterBare = "  Context 59% used · GPT-6.1-Sol high …"
	codexWideFooter       = "  Context 2% used · GPT-6.1-Sol high · ~/projects/demo"
)

// codexBox is a Codex screen whose input box shows the given rows, then the
// footer.
func codexBox(footer string, rows ...string) string {
	return "• Earlier turn output\n\n› " + strings.Join(rows, "\n  ") + "\n\n" + footer + "\n\n"
}

// The footer is the screen's last non-blank line when it is footer-shaped:
// wide (with the working directory) as before, and narrow (elided "…", with
// or without the warning). A footer-shaped line with anything below it is not
// the footer.
func TestCodexFooterIsTheLastFooterShapedLine(t *testing.T) {
	for _, footer := range []string{codexNarrowFooter, codexNarrowFooterBare, codexWideFooter, codexWideFooter + "  ⚠ 1 warning · f2 to view"} {
		lines := strings.Split(codexBox(footer, ""), "\n")
		if got := codexFooterIndex(lines); got < 0 || lines[got] != footer {
			t.Fatalf("footer %q not found (index %d)", footer, got)
		}
	}
	below := strings.Split(codexNarrowFooter+"\nuser@host:~$ ", "\n")
	if got := codexFooterIndex(below); got != -1 {
		t.Fatalf("a footer-shaped line with a shell prompt below it was taken as the footer (index %d)", got)
	}
	if !looksLikeCodexPrompt(codex0166Screen(t, "idle_narrow")) {
		t.Fatal("a narrow Codex screen does not look like Codex's prompt")
	}
}

// The box reads empty on a narrow pane, and its placeholder is no draft.
func TestNarrowCodexFooterIsNotDraftText(t *testing.T) {
	for _, footer := range []string{codexNarrowFooter, codexNarrowFooterBare, codexWideFooter} {
		if draft, known := composerDraft(codexBox(footer, ""), true); !known || draft != "" {
			t.Fatalf("footer %q: draft %q (known %v), want an empty box", footer, draft, known)
		}
	}
}

// Text typed into the box that looks like the footer is still text: only the
// last footer-shaped line is the footer. An unclear screen reads as holding
// text, never as empty.
func TestFooterShapedTextInTheBoxIsStillADraft(t *testing.T) {
	for _, footer := range []string{codexNarrowFooter, codexWideFooter} {
		for _, rows := range [][]string{
			{"Context 5% used · gpt-5"},
			{"Context 5% used · gpt-6.1 high …"},
			{"see this", "Context 5% used · gpt-5 · ~/fake"},
		} {
			draft, known := composerDraft(codexBox(footer, rows...), true)
			if !known || !strings.Contains(draft, "Context5%used·gpt-") {
				t.Fatalf("footer %q, typed %q: draft %q (known %v), want the typed text", footer, rows, draft, known)
			}
		}
	}
	// No footer at the end at all (something below it): the box runs on.
	if draft, _ := composerDraft("› \n\n"+codexNarrowFooter+"\nuser@host:~$ ", true); draft == "" {
		t.Fatal("an unclear screen read as an empty box")
	}
}

// The draft guard holds for that typed text, even above the real narrow
// footer, and types nothing into it.
func TestTypedFooterTextHoldsAgentMessages(t *testing.T) {
	p, frames, screen := newDraftGuardPane(t, "draft-footer-text")
	screen.Store(codexBox(codexNarrowFooter, "Context 5% used · gpt-5"))
	if got := <-agentDelivery(p, codexProbeMessage, "@lead"); got != deliveryHeld {
		t.Fatalf("outcome = %q, want held", got)
	}
	expectNoFrame(t, frames, "nothing typed into the person's draft")
}

// A put-back message whose own text looks like the footer still shows while
// it is in the box: its guard on agents' keys holds.
func TestPutBackMessageWithFooterTextStillShows(t *testing.T) {
	body := "[from @a] Context 5% used · gpt-6.1 high, please check the build"
	if shown, known := putBackShown(codexBox(codexNarrowFooter, body), true, body); !known || !shown {
		t.Fatalf("shown %v (known %v), want the put-back message seen", shown, known)
	}
	twoRows := "[from @a] please check\nContext 5% used · gpt-5"
	if shown, known := putBackShown(codexBox(codexNarrowFooter, "[from @a] please check", "Context 5% used · gpt-5"), true, twoRows); !known || !shown {
		t.Fatalf("shown %v (known %v), want the two-row put-back message seen", shown, known)
	}
}

// @ops: a narrow Codex pane with an empty box takes an agent's message at
// once, and one held while the person typed goes in when the box empties.
func TestNarrowCodexPaneTakesMessagesWhenItsBoxIsEmpty(t *testing.T) {
	narrow := codex0166Screen(t, "idle_narrow")
	empty := strings.Replace(narrow, "› Ask Codex to do anything", "›", 1) // the faint placeholder, blanked
	t.Run("empty box", func(t *testing.T) {
		p, frames, screen := newDraftGuardPane(t, "narrow-empty")
		screen.Store(empty)
		if reason := p.draftGuardReason(true, true, time.Now(), p.humanComposerPendingSnapshot); reason != "" {
			t.Fatalf("held on an empty narrow box: %q", reason)
		}
		done := agentDelivery(p, codexProbeMessage, "@lead")
		expectFrame(t, frames, "the message", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		select {
		case got := <-done:
			if got == deliveryHeld {
				t.Fatal("the message was held after typing it")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the delivery did not finish")
		}
	})
	t.Run("drain", func(t *testing.T) {
		p, frames, screen := newDraftGuardPane(t, "narrow-drain")
		screen.Store(strings.Replace(narrow, "› Ask Codex to do anything", "› I want the release upda", 1))
		if got := <-agentDelivery(p, codexProbeMessage, "@lead"); got != deliveryHeld {
			t.Fatalf("outcome = %q, want held while the person types", got)
		}
		expectNoFrame(t, frames, "still held")
		screen.Store(empty)
		expectFrame(t, frames, "held body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		expectFrame(t, frames, "Enter", "\r")
		waitHeldEmpty(t, p)
	})
}
