package main

import (
	"sync/atomic"
	"time"
)

const (
	// ariaAutoTypingSettle is how long after the user's last keystroke a direct
	// auto-fire holds off. Short on purpose: it only has to cover the window
	// between "the watcher saw a choice prompt" and "we write Enter", where the
	// menu can be answered (by the human, an allowlist, or a mode switch) and
	// the pane fall back to an ordinary composer the user is already typing
	// into. A bare Enter landing there submits their half-written message.
	ariaAutoTypingSettle = 3 * time.Second

	// ariaAutoDraftStaleAfter bounds the composer-has-text hold. The recorder
	// buffer is a heuristic reconstruction of typing, and the TUI can consume a
	// line by a path it never sees (Esc, a history recall, a mode switch), so a
	// phantom draft is possible. Deliberately much tighter than the delegation
	// nudge's 15m equivalent: a nudge can wait, a pending approval sitting on a
	// pane the user has walked away from cannot. Someone actually composing
	// touches a key well inside this, so the real case is unaffected and a
	// phantom costs at most one window plus a sweeper tick.
	ariaAutoDraftStaleAfter = 2 * time.Minute
)

// autoFireUserHold reports whether the human is mid-input in this TERMINAL
// pane, so a direct auto-fire must not write into it yet.
//
// Terminal-only by design. The defect is a PTY-level collision: auto-approve
// writes a bare Enter, and if the menu has already gone the Enter lands in an
// ordinary composer and submits whatever was half-written there. Native panes
// answer approvals with a structured control_response frame keyed by RequestID
// (aria_native_auto.go), never a keystroke, so a resolve cannot take a draft
// the user is holding. Holding those would cost auto-approval for nothing.
//
// Both signals come from the seam the delegation nudge already holds delivery
// on (delegationNudgeSoftHold): LastInputMs recency, and the prompt recorder's
// unsubmitted buffer. Draft freshness uses the recorder's own activity time so
// ARIA control writes cannot renew a stale buffer. The auto-approval path never
// consulted either signal, so an approval fire and a human keystroke could race
// for the same Enter.
//
// Holds are deliberately NOT declines: callers return without bumping the
// sweeper-suspend streak, so the 5s recovery sweeper retries the still-pending
// menu once the pane goes quiet.
func autoFireUserHold(p *Pane, now time.Time) (bool, string) {
	if p == nil {
		return false, ""
	}
	lastInput := atomic.LoadInt64(&p.LastInputMs)
	// Auto-approval sends reach the PTY through the same Pane.write path as
	// human keystrokes, so they bump LastInputMs too. Without this, one fire
	// would hold off the next one for the whole settle window and back-to-back
	// permission modals would stall. AutoModeLastFiredMs is stamped once the
	// send has landed, so a keystroke is the human's only if it is newer.
	//
	// Recency only. Draft freshness below comes from recorded input instead of
	// LastInputMs because auto-approval writes deliberately update LastInputMs.
	//
	// Only a person's input holds: an agent's message delivery (send_input,
	// ask_pane) stamps lastAgentInputMs too, and #896's lastHumanInputMs
	// (person sockets only) catches a person typing just before it.
	if lastInput > 0 && lastInput > p.Snapshot().AutoModeLastFiredMs && lastInput != p.lastAgentInputMs.Load() && now.Sub(time.UnixMilli(lastInput)) < ariaAutoTypingSettle {
		return true, "user typing"
	}
	if human := p.lastHumanInputMs.Load(); human > 0 && now.Sub(time.UnixMilli(human)) < ariaAutoTypingSettle {
		return true, "user typing"
	}
	// A draft an agent's delivery left (Codex keeps a queued message's text
	// in the recorder: Tab is not Enter) is no one's typing (2026-11-02:
	// @rest-gate's approval waited two minutes on @flag-guard's message).
	pending, lastRecordedInput := p.humanComposerPendingSnapshot()
	if pending > 0 && lastRecordedInput > 0 && now.Sub(time.UnixMilli(lastRecordedInput)) < ariaAutoDraftStaleAfter {
		// Name when the hold lifts. The deadline only moves when new input
		// renews the draft, so a renewed hold records a new decision line
		// instead of being collapsed into the first one.
		until := time.UnixMilli(lastRecordedInput).Add(ariaAutoDraftStaleAfter).Local().Format("15:04:05")
		return true, "composer has text (unsent input in the pane; auto-approval waits until " + until + ")"
	}
	return false, ""
}
