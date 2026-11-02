package main

import (
	"strings"
	"unicode/utf8"
)

// sendInputMenuAnswerMaxRunes: a deliberate menu answer ("1", "y", "2\n")
// is one or two keys. Anything longer typed into a pane that is showing an
// approval menu is read by the TUI as menu keystrokes — a message such as
// "[from Codex] please …" can select options, including Codex's "p" (don't
// ask again, a persistent grant) — and it also leaves "composer has text"
// behind, holding the pane's auto-approval.
const sendInputMenuAnswerMaxRunes = 2

// sendInputMenuCollision refuses MCP send_input text that is longer than a
// menu answer while the target pane sits at an approval/choice prompt. The
// caller can retry once the prompt resolves, or answer it explicitly with
// answer_prompt. Nothing is written when it refuses.
func sendInputMenuCollision(p *Pane, text string) string {
	if p == nil || p.Snapshot().Kind == "native" {
		return ""
	}
	if !sendInputLongerThanMenuAnswer(text) {
		return "" // a deliberate one/two-key answer: no state probe needed
	}
	return sendInputMenuCollisionFor(p.ID, p.computeStateCached(), text)
}

func sendInputLongerThanMenuAnswer(text string) bool {
	return utf8.RuneCountInString(strings.TrimRight(text, "\r\n")) > sendInputMenuAnswerMaxRunes
}

// sendInputMenuCollisionFor is the decision on an already-computed pane state.
func sendInputMenuCollisionFor(paneID string, state PaneRuntimeState, text string) string {
	if state != StateChoicePrompt || !sendInputLongerThanMenuAnswer(text) {
		return ""
	}
	return "pane " + paneID + " is showing an approval prompt — your text was NOT sent, because the menu would read it as keystrokes (it can select options, including persistent grants). " +
		"Retry after the prompt resolves, or answer the prompt itself with answer_prompt."
}

// sendInputUnsubmittedMessage refuses a message for a Claude or Codex pane
// that does not end in Enter. Only a line ending in Enter goes through verified
// delivery. Without one, the text is typed raw into the input box and
// left there: Claude collapses a long one into a "[Pasted text #N]" placeholder
// that nobody submits, and the prompt recorder counts it as the person's own
// draft, so the pane's auto-approval holds as "composer has text"
// (2026-11-02: @qa's review note in @lead's box). Keys keep raw
// semantics: control sequences and one- or two-key answers are not messages.
// Nothing is written when it refuses.
func sendInputUnsubmittedMessage(p *Pane, text string) string {
	if p == nil || strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r") {
		return ""
	}
	if _, message := agentMessageBody(text + "\n"); !message {
		return ""
	}
	if agent, _ := paneAgentProvider(p); !agent {
		return ""
	}
	return "pane " + p.ID + " runs an agent, and your text does not end with a newline, so it was NOT sent: " +
		"it would sit unsubmitted in that agent's input box and hold its auto-approval. " +
		"End the text with \"\\n\" to deliver and submit it, or use ask_pane."
}
