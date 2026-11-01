package main

import (
	"regexp"
	"strings"
)

// Claude Code 2.1 queues a message submitted mid-turn instead of starting a
// turn. The queue is listed above the input box, one "❯ " entry per message
// (wrapped and multi-line rows indented two spaces, entries separated by a
// blank line), the last entry followed by the "<keys> to send now" hint, and
// the empty box reads "Press up to edit queued messages". Claude sends a
// queued message with the next tool result of the running turn (a
// queued_command attachment in the transcript), or as its own turn
// (promptSource "queued") when the turn ends or Esc interrupts it. Up moves
// every queued message back into the input box, where nothing sends it.
var (
	claudeQueueSendNowRE = regexp.MustCompile(`(?i)^\s+\S.* to send now\s*$`)
	claudeQueueEditRE    = regexp.MustCompile(`(?i)press up to edit queued messages`)
	// The live spinner row ("✶ Whirring… (1s · thinking)"); a finished turn
	// leaves "✻ Brewed for 7s", without the ellipsis.
	claudeSpinnerRE   = regexp.MustCompile(`(?m)^\s*[·✢✳✶✻✽*]\s+\S[^\n]*…\s*\(`)
	claudeInterruptRE = regexp.MustCompile(`(?i)esc to interrupt`)
)

// claudeComposerLine is the index of Claude's input box line (the last "❯"
// row), or -1.
func claudeComposerLine(lines []string) int {
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			at = i
		}
	}
	return at
}

// claudeQueuedMessages returns the messages Claude lists as queued above its
// input box, each without the sender label or whitespace. The entry above
// the queue can be the running turn's own prompt (it is drawn the same way
// before any output), so callers match a whole message, never "any entry".
func claudeQueuedMessages(screen string) []string {
	lines := strings.Split(screen, "\n")
	composer := claudeComposerLine(lines)
	hint := -1
	for i := 0; i < composer; i++ {
		if claudeQueueSendNowRE.MatchString(lines[i]) {
			hint = i
		}
	}
	if hint < 0 {
		return nil
	}
	var msgs []string
	var rows []string // the entry being read, bottom up
	for i := hint - 1; i >= 0 && len(msgs) < 32; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			if len(rows) > 0 {
				break // rows with no "❯" start: not a queue entry
			}
			continue
		}
		rows = append(rows, t)
		if strings.HasPrefix(t, "❯") {
			entry := make([]string, 0, len(rows))
			for j := len(rows) - 1; j >= 0; j-- {
				entry = append(entry, rows[j])
			}
			entry[0] = strings.TrimSpace(strings.TrimPrefix(entry[0], "❯"))
			msgs = append(msgs, conditionalDraftText(stripSenderLabel(strings.Join(entry, " "))))
			rows = rows[:0]
		}
	}
	return msgs
}

// claudeQueueHolds reports whether Claude lists body as queued: the whole
// text, or the "[Pasted text …]" stand-in Claude shows for a long paste.
func claudeQueueHolds(screen, body string) bool {
	want := conditionalDraftText(stripSenderLabel(body))
	if want == "" {
		return false
	}
	pasted := len(body) > 200 || strings.Contains(body, "\n")
	for _, m := range claudeQueuedMessages(screen) {
		if m == want || (pasted && strings.HasPrefix(m, "[Pastedtext")) {
			return true
		}
	}
	return false
}

// claudeTurnRunningText reports a live Claude turn on its rendered screen:
// the queue, the spinner row, or "esc to interrupt" under the input box.
func claudeTurnRunningText(screen string) bool {
	lines := strings.Split(screen, "\n")
	composer := claudeComposerLine(lines)
	if composer < 0 {
		return false
	}
	if claudeQueueEditRE.MatchString(lines[composer]) || claudeSpinnerRE.MatchString(strings.Join(lines[:composer], "\n")) {
		return true
	}
	for _, line := range lines[:composer] {
		if claudeQueueSendNowRE.MatchString(line) {
			return true
		}
	}
	return claudeInterruptRE.MatchString(strings.Join(lines[composer:], "\n"))
}

// claudePlainComposer reads Claude's input box when conditionalClaudeComposer
// cannot (it keys on a status line naming the model, which a narrow pane cuts
// to "[Opus…" or drops): the last "❯" row, right under the box's top rule, and
// its continuation rows down to the closing rule, with only Claude's footer
// below. Claude pads every footer row (status line, mode line, notices), so a
// row starting at the left edge (a shell prompt after Claude exits) is not one.
func claudePlainComposer(screen string) (string, bool) {
	lines := strings.Split(screen, "\n")
	at := claudeComposerLine(lines)
	if at < 1 || !claudeBoxRule(lines[at-1]) {
		return "", false
	}
	parts := []string{strings.TrimPrefix(strings.TrimSpace(lines[at]), "❯")}
	for i := at + 1; i < len(lines); i++ {
		if !claudeBoxRule(lines[i]) {
			parts = append(parts, lines[i])
			continue
		}
		footer := 0
		for _, below := range lines[i+1:] {
			if strings.TrimSpace(below) == "" {
				continue
			}
			if footer++; footer > claudeFooterRows || !(strings.HasPrefix(below, "  ") || claudeChromeLineRE.MatchString(below)) {
				return "", false
			}
		}
		return conditionalDraftText(strings.Join(parts, "")), true
	}
	return "", false
}

// claudeFooterRows bounds Claude's footer under the input box: a status line
// (which may print several rows), the mode line and a notice.
const claudeFooterRows = 6

func claudeBoxRule(line string) bool {
	t := strings.TrimSpace(line)
	return strings.Count(t, "─") >= 4 && strings.Trim(t, "─") == ""
}
