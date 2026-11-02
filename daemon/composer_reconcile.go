package main

import (
	"strings"
	"sync/atomic"
	"time"
)

const (
	// composerReconcileSettle is how long the last recorded keystroke must be
	// in the past before an empty input box on screen counts as proof. The
	// agent has to echo a keystroke before the screen can show it.
	composerReconcileSettle = ariaAutoTypingSettle
	// composerReconcileRecheck spaces out screen reads while the box shows
	// text, so a draft the person keeps for hours costs one read per interval.
	composerReconcileRecheck = 10 * time.Second
)

// reconcileComposerDraft drops a prompt-recorder draft that the agent's
// visible input box shows is gone. The recorder reconstructs typing from input
// bytes. It cannot see the agent clear or consume a line (Esc, a history
// recall, a key that went to a dialog the daemon had not drawn yet), and every
// consumer of the draft (the auto-approval hold, delegation nudges,
// conditional submit) would trust that phantom. The screen is the only proof.
// So the draft is dropped only when the box is readable and empty, no menu
// covers it, and no input arrived since it was read. Anything else keeps it.
// It reports whether a draft was dropped.
func (p *Pane) reconcileComposerDraft(now time.Time) bool {
	if p == nil {
		return false
	}
	pending, recorded := p.composerPendingSnapshot()
	if pending == 0 || recorded <= 0 || now.Sub(time.UnixMilli(recorded)) < composerReconcileSettle {
		return false
	}
	if atomic.LoadInt64(&p.composerReconcileAfterMs) > now.UnixMilli() {
		return false
	}
	if p.Snapshot().Kind == "native" || p.computeStateCached() == StateChoicePrompt {
		return false
	}
	// Claude only: its input box has a layout that proves emptiness (below).
	// Other agents keep the recorder's draft.
	if agent, claude := paneAgentProvider(p); !agent || !claude {
		return false
	}
	screen := deliveryScreenText(p)
	if menuDialogFooterOnScreen(screen) {
		return false
	}
	if empty, known := claudeComposerProvablyEmpty(screen); !known || !empty {
		atomic.StoreInt64(&p.composerReconcileAfterMs, now.Add(composerReconcileRecheck).UnixMilli())
		return false
	}
	p.agentWriteMu.Lock()
	defer p.agentWriteMu.Unlock()
	// Input since the snapshot may not have been echoed when the screen was
	// read; that draft is unproven either way, so keep it.
	if n, at := p.promptRec.draft(); n == 0 || at != recorded || p.promptRec.inMenu {
		return false
	}
	p.promptRec.discardDraft()
	return true
}

// claudeComposerProvablyEmpty reports whether the screen shows Claude's input
// box as one empty row: a full rule, the prompt glyph with nothing after it,
// and a full rule, as the last such row on screen. A multi-line draft breaks
// that shape. Its first row sits under the top rule and has text, and a
// later row that merely starts with the glyph has no rule directly above it.
// So text the box still holds can never read as empty. known is false when
// no box of that shape is on screen.
func claudeComposerProvablyEmpty(screen string) (empty, known bool) {
	lines := strings.Split(screen, "\n")
	row := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			row = i
		}
	}
	if row < 1 || row+1 >= len(lines) {
		return false, false
	}
	if !composerRuleLine(lines[row-1]) || !composerRuleLine(lines[row+1]) {
		return false, false
	}
	rest := strings.TrimPrefix(strings.TrimSpace(lines[row]), "❯")
	return strings.TrimSpace(strings.ReplaceAll(rest, "\u00a0", " ")) == "", true
}

// composerRuleLine is one of the full-width rules Claude draws around its
// input box.
func composerRuleLine(line string) bool {
	line = strings.TrimSpace(line)
	return len([]rune(line)) >= 8 && strings.Trim(line, "─") == ""
}
