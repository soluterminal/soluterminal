package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// promptRecorder reconstructs submitted input lines ("prompts") from the
// raw client→PTY byte stream flowing through Pane.Write. This is the
// daemon-side successor of the web's observePaneInput (removed in the
// same change): recording at the PTY choke point sees EVERY transport —
// typed keystrokes over the pane WS, /api/panes/{id}/inject (voice,
// ARIA quick-reply), ARIA send_input / auto-approve, schedules,
// broadcast — where the old client-side recorder only saw bytes leaving
// its own page. It also makes the history a property of the pane, so
// all clients (desktop, phone via cloud relay) render the same strip
// and it survives reloads and daemon restarts.
//
// Heuristic, by design: it reconstructs what the user TYPED, not what
// the foreground program ended up with. Tab-completions, history
// recall (↑ + Enter) and TUI-side edits aren't keystrokes and are
// invisible here; recovering those would mean parsing PTY output /
// the grid, which is exactly the TUI-coupled fragility this project
// keeps deliberately avoiding.
//
// Not safe for concurrent use — Pane.Write already serializes callers
// under agentWriteMu, so the recorder piggybacks on that.
type promptRecorder struct {
	buf []byte
	// lastRecordedInputMs is the Unix-millisecond time of the most recent
	// successful recorded client write. It deliberately lives with buf under
	// Pane.agentWriteMu so autonomous writes that bypass the recorder cannot
	// make an old draft appear fresh.
	lastRecordedInputMs int64
	// inStringControl carries OSC/DCS/APC/PM/SOS swallowing across
	// Write calls: xterm's auto-replies to terminal queries (OSC color
	// reports, DCS responses) can be split across frames, and without
	// the carry the payload tail would leak into the prompt buffer.
	inStringControl bool
	// Carry framing, including the two bytes of ST, across client writes.
	// A chunk boundary is not the end of an escape sequence.
	escapePending bool
	inCSI         bool
	stringEscape  bool
	// While a choice menu is on screen, keystrokes answer the menu (a digit,
	// arrows, a question's free text), not the composer. They accumulate after
	// menuStart and never count as a draft; leaving the menu discards them.
	// Claude's question menus take a digit without Enter, so recording it as
	// composer text held the next approval for two minutes (2026-10-26).
	inMenu    bool
	menuStart int
	// draftRecordedMs is lastRecordedInputMs as of entering the menu: the
	// freshness of the composer draft, which menu keys must not renew.
	draftRecordedMs int64
	// agentOwned: everything in the draft was typed by an agent's message
	// delivery (pane_delivery.go) into an empty box, such as a message Codex
	// moved to its queue on Tab. A person's recorded input clears it.
	// Auto-approval holds only for a person's draft (humanDraft).
	agentOwned bool
}

// enterMenu starts a menu segment; a no-op while one is open, so a menu that
// follows another without a composer keystroke keeps the original draft.
func (r *promptRecorder) enterMenu() {
	if r.inMenu {
		return
	}
	r.inMenu = true
	r.menuStart = len(r.buf)
	r.draftRecordedMs = r.lastRecordedInputMs
}

// leaveMenu discards what was typed into the menu and restores the draft.
func (r *promptRecorder) leaveMenu() {
	if !r.inMenu {
		return
	}
	r.buf = r.buf[:r.draftLen()]
	r.lastRecordedInputMs = r.draftRecordedMs
	r.inMenu = false
	r.menuStart = 0
	r.draftRecordedMs = 0
}

// discardDraft forgets the composer draft and any menu segment. Byte framing
// (a split escape sequence) is kept: it belongs to the stream, not the draft.
func (r *promptRecorder) discardDraft() {
	r.buf = r.buf[:0]
	r.agentOwned = false
	r.inMenu = false
	r.menuStart = 0
	r.draftRecordedMs = 0
}

func (r *promptRecorder) draftLen() int {
	if r.inMenu && r.menuStart < len(r.buf) {
		return r.menuStart
	}
	return len(r.buf)
}

// draft reports the unsent composer text's length and when it was last typed.
// Menu keys are excluded: they went to the menu, whatever happens to it next.
// Caller holds Pane.agentWriteMu.
func (r *promptRecorder) draft() (int, int64) {
	if r.inMenu {
		return r.draftLen(), r.draftRecordedMs
	}
	return len(r.buf), r.lastRecordedInputMs
}

// humanDraft is draft for a draft a person typed: an agent-owned draft
// reads as none. Caller holds Pane.agentWriteMu.
func (r *promptRecorder) humanDraft() (int, int64) {
	if r.agentOwned {
		return 0, 0
	}
	return r.draft()
}

const (
	// lastPromptsMax is how many committed prompts a pane retains,
	// newest first — a bounded recall buffer held in memory, persisted to
	// panes.json, and broadcast on the pane-prompt SSE event. The web
	// strip shows the newest one collapsed and the full buffer in a
	// scrollable list (~3 rows visible) when expanded. Bounded rather than
	// unlimited so the snapshot/SSE payload can't grow without limit.
	lastPromptsMax = 50
	// Commit gates: a submitted line shorter than promptMinRunes is
	// menu-choice noise ("1", "y"); longer than promptMaxRunes is a
	// paste blob, not a prompt.
	promptMinRunes = 2
	promptMaxRunes = 2000
	// promptBufCap bounds the unsubmitted-typing buffer (tail kept).
	promptBufCap = 4000
)

// observe consumes one client→PTY write and returns any prompts
// committed by Enter bytes inside it (usually zero or one).
func (r *promptRecorder) observe(data []byte) []string {
	var commits []string
	// In a menu, Enter, Ctrl+U and Backspace act on the menu segment only; a
	// draft typed before the menu stays exactly as it was.
	floor := 0
	if r.inMenu {
		floor = r.draftLen()
	}
	i := 0
	for i < len(data) {
		b := data[i]
		if r.inStringControl {
			if b == 0x07 || r.stringEscape && b == '\\' {
				r.inStringControl = false
				r.stringEscape = false
			} else {
				r.stringEscape = b == 0x1b
			}
			i++
			continue
		}
		if r.escapePending {
			r.escapePending = false
			switch b {
			case ']', 'P', '^', '_', 'X':
				r.inStringControl = true
				i++
				continue
			case '[', 'O':
				r.inCSI = true
				i++
				continue
			}
			// A standalone Escape followed by ordinary typing must not
			// discard that typing or its Enter; process it below.
		}
		if b == 0x1b {
			r.inCSI = false
			r.escapePending = true
			i++
			continue
		}
		if r.inCSI {
			// CSI/SS3, including bracketed-paste wrappers. Their payload
			// ends at the final byte; pasted text afterwards is ordinary input.
			if b >= 0x40 && b <= 0x7e {
				r.inCSI = false
			}
			i++
			continue
		}
		if b == '\r' || b == '\n' {
			text := collapseSpaces(string(r.buf[floor:]))
			r.buf = r.buf[:floor]
			if n := utf8.RuneCountInString(text); n >= promptMinRunes && n <= promptMaxRunes && !isMenuChoiceNoise(text) {
				commits = append(commits, text)
			}
			i++
			continue
		}
		if b == 0x7f || b == 0x08 {
			if len(r.buf) > floor {
				_, size := utf8.DecodeLastRune(r.buf[floor:])
				r.buf = r.buf[:len(r.buf)-size]
			}
			i++
			continue
		}
		// Ctrl+C / Ctrl+U / Ctrl+W ≈ "abandon current input".
		if b == 0x03 || b == 0x15 || b == 0x17 {
			r.buf = r.buf[:floor]
			i++
			continue
		}
		// Printable ASCII, all UTF-8 lead/continuation bytes (>= 0x80)
		// and tabs accumulate (collapseSpaces turns tab runs into one
		// space — dropping tabs outright fused pasted words together);
		// remaining C0 controls are dropped.
		if b >= 0x20 || b == 0x09 {
			r.buf = append(r.buf, b)
			i++
			continue
		}
		i++
	}
	// Menu input is capped on its own, after the draft: a long paste into a
	// question must not push the composer draft out of the buffer.
	if keep := floor; len(r.buf)-keep > promptBufCap {
		tail := r.buf[len(r.buf)-promptBufCap:]
		// Don't start the kept tail mid-rune.
		for len(tail) > 0 && tail[0]&0xC0 == 0x80 {
			tail = tail[1:]
		}
		r.buf = append(r.buf[:keep], tail...)
	}
	return commits
}

// collapseSpaces trims and collapses all whitespace runs to single
// spaces — cursor-move escapes already became nothing here (input side
// drops them outright), but pasted text carries tabs/newlines-adjacent
// spacing worth normalizing for a one-line strip.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// isMenuChoiceNoise reports whether a committed line is an
// approval/menu-choice reply rather than a typed prompt — a run of
// digits ("1", "11", "111", "11111": the auto-approval loop answering a
// choice prompt, which accumulates digits in the buffer when a reply
// lands without a committing Enter and the next iteration types another
// one), or a bare yes/no/y/n. The promptMinRunes gate already drops the
// single-character "1"/"y" case; this extends the same "menu-choice
// noise" intent to the accumulated repeats and the spelled-out yes/no
// so auto-approval never pollutes the last-prompts strip.
//
// Conservative on purpose: any other character keeps the line. A real
// prompt that merely starts with a digit ("2 reasons this fails") is
// preserved; only all-digit / pure-yes-no lines are dropped.
func isMenuChoiceNoise(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no":
		return true
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == ' ': // tolerate whitespace between accumulated digits
		default:
			return false
		}
	}
	return hasDigit
}

// sanitizePrompts drops menu-choice noise and repairs native protocol envelopes
// persisted by older daemons. Native input used to feed the encoded Codex
// JSON-RPC / Claude stream-json frame to the terminal recorder, putting the
// whole envelope in the prompt strip. Recover its original text when possible.
// Returns a new slice; never mutates the input.
func sanitizePrompts(history []string) []string {
	var out []string
	for _, h := range history {
		if text, ok := nativeEnvelopePrompt(h); ok {
			h = text
		}
		if !isMenuChoiceNoise(h) {
			out = append(out, h)
		}
	}
	return out
}

func nativeEnvelopePrompt(raw string) (string, bool) {
	var envelope struct {
		Type    string `json:"type"`
		Method  string `json:"method"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Params struct {
			Input []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return "", false
	}
	if envelope.Type == "user" && envelope.Message.Role == "user" && envelope.Message.Content != "" {
		return collapseSpaces(envelope.Message.Content), true
	}
	if envelope.Method != "turn/start" && envelope.Method != "turn/steer" {
		return "", false
	}
	var texts []string
	for _, input := range envelope.Params.Input {
		if input.Type == "text" && input.Text != "" {
			texts = append(texts, input.Text)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return collapseSpaces(strings.Join(texts, " ")), true
}

// pushPrompt prepends a committed prompt to a newest-first history,
// capped at lastPromptsMax. Returns the new slice.
func pushPrompt(history []string, prompt string) []string {
	next := make([]string, 0, lastPromptsMax)
	next = append(next, prompt)
	for _, h := range history {
		if len(next) >= lastPromptsMax {
			break
		}
		next = append(next, h)
	}
	return next
}
