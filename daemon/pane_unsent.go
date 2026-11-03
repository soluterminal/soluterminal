package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type unsentPaneMessage struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
	// NotTyped: the pane was busy, so nothing was typed (a shell or an agent
	// with no queue SoluTerminal knows). Send types it; Clear discards it.
	// Text is the whole message, so the person reads exactly what Send
	// would type (a typed receipt's text is on screen already).
	NotTyped bool `json:"notTyped,omitempty"`
	// PutBack: a queued message the agent put back into its input box,
	// usually after the person stopped the turn. Only the person sends or
	// clears it.
	PutBack    bool   `json:"putBack,omitempty"`
	// Edited: the person typed in the box since it was put back, and part of
	// it still shows. It is still not theirs alone: agents stay refused.
	Edited bool `json:"edited,omitempty"`
	// kept: the person chose "Keep as my draft". The chip goes and their own
	// message box may type again; agents, schedules and ARIA stay refused
	// while any part of the message shows (putBackInputCheck).
	kept bool
	Text       string `json:"text,omitempty"`
	body       string
	claude     bool
	revision   uint64
	connection net.Conn
}

var errUnsentChanged = errors.New("input changed or cannot be verified; the pane text was preserved")

// errPutBackGone: the input box can be read and no longer holds the
// put-back message.
var errPutBackGone = fmt.Errorf("%w: the message is no longer in the input box", errUnsentChanged)

// Manual input supersedes a receipt without altering any terminal text. This
// also removes the chip after the user submits or edits the draft themselves.
// Caller holds mu; receipts are immutable, so sharing the pointer is safe.
func (p *Pane) currentUnsentMessageLocked() *unsentPaneMessage {
	if p.unsentMessage != nil && !p.unsentMessage.kept && p.Status == PaneRunning && p.Kind != "native" && !p.respawning.Load() && p.unsentMessage.connection == p.agentConn && (p.unsentMessage.NotTyped || p.unsentMessage.revision == p.inputRevision.Load()) {
		return p.unsentMessage
	}
	return nil
}

// Exact deletion requires the whole literal draft to be visible. A collapsed
// paste, wrapped/multiline text, trailing spaces or unknown chrome cannot prove
// equality. Leave those for the user to edit in the terminal.
func composerShowsExactLine(screen string, codex bool, body string) bool {
	if body == "" || strings.ContainsAny(body, "\r\n") || strings.TrimRight(body, " \t") != body || strings.Contains(body, "[Pasted") {
		return false
	}
	line, ok := composerOneLine(screen, codex)
	return ok && line == body
}

// composerOneLine is the input box's text when the box is one readable line:
// its prompt row, then only blank rows down to the box's end. Ctrl+A Ctrl+K
// clears exactly that; a wrapped or multi-line box it cannot.
func composerOneLine(screen string, codex bool) (string, bool) {
	if _, known := composerDraft(screen, codex); !known {
		return "", false
	}
	lines := strings.Split(screen, "\n")
	start := -1
	for i, line := range lines {
		t := strings.TrimLeft(line, " \t")
		if (codex && codexComposerLineRE.MatchString(line)) || (!codex && strings.HasPrefix(t, "❯")) {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	line := strings.TrimLeft(lines[start], " \t")
	_, size := utf8.DecodeRuneInString(line)
	line = strings.TrimRight(line[size:], " \r")
	// Current Claude draws its prompt separator with NBSP.
	if strings.HasPrefix(line, "\u00a0") {
		line = strings.TrimPrefix(line, "\u00a0")
	} else {
		line = strings.TrimPrefix(line, " ")
	}
	footer := codexFooterIndex(lines)
	for j := start + 1; j < len(lines); j++ {
		rest := lines[j]
		// Claude closes its box with a rule; older layouts end at the footer.
		if (codex && j == footer) || (!codex && (newPromptFooterRE.MatchString(rest) || claudeBoxRule(rest))) {
			return line, true
		}
		if strings.Trim(strings.TrimSpace(rest), "─") != "" {
			return "", false
		}
	}
	return "", false
}

const untypedSendAction = "send a message a busy pane was not given"

const putBackAction = "send or clear a message put back in a pane's input box"

const putBackKeepAction = "keep a put-back message you edited as your own draft"

// errPutBackMultiLine: Clear's keys clear one line; the box has more.
var errPutBackMultiLine = errors.New("Nothing was cleared: the input box has more than one line, and Clear can only empty a single line. Remove the message in the pane yourself, or choose Keep as my draft")

// errPutBackEditedSend: Enter would send the person's edit with it.
var errPutBackEditedSend = errors.New("Nothing was sent: you edited that message in the input box. Press Enter in the pane to send it as it is now, or Clear it")

func (a *apiServer) actOnUnsentMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if req.ID == "" || (req.Action != "send" && req.Action != "clear" && req.Action != "keep") {
		http.Error(w, "id and send/clear/keep action required", http.StatusBadRequest)
		return
	}
	if !a.promptInputGate(r)(p) {
		http.Error(w, "human_required", http.StatusForbidden)
		return
	}
	if held := p.heldDeliveryByID(req.ID); held != nil {
		// Sending one early overrides the person's typing hold, and
		// discarding one drops another agent's message: only the person may.
		if !a.requireHuman(w, r, heldActionName) {
			return
		}
		if req.Action == "keep" {
			http.Error(w, "keep applies only to a put-back message", http.StatusBadRequest)
			return
		}
		a.actOnHeldDelivery(w, p, held, req.Action)
		return
	}
	// Sending a message the pane was never given types an agent's text: only
	// the person, who was shown it, may do that.
	if msg := p.Snapshot().UnsentMessage; msg != nil && msg.ID == req.ID && msg.NotTyped && req.Action == "send" && !a.requireHuman(w, r, untypedSendAction) {
		return
	}
	if req.Action == "keep" && !a.requireHuman(w, r, putBackKeepAction) {
		return
	}
	if msg := p.Snapshot().UnsentMessage; msg != nil && msg.ID == req.ID && msg.PutBack && !a.requireHuman(w, r, putBackAction) {
		return
	}
	// The provider check can scan the process tree, so it runs before the
	// delivery and writer locks.
	agent, claude := paneAgentProvider(p)
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	msg := p.Snapshot().UnsentMessage
	if msg == nil || msg.ID != req.ID {
		http.Error(w, "unsent message changed", http.StatusConflict)
		return
	}
	// The receipt can change between the check above and the lock: check the
	// one Send would act on.
	if msg.NotTyped && req.Action == "send" && !a.requireHuman(w, r, untypedSendAction) {
		return
	}
	if msg.PutBack && !a.requireHuman(w, r, putBackAction) {
		return
	}
	if req.Action == "keep" {
		// Keep is the person's alone (checked above, and again here on the
		// receipt under the lock), and only for a put-back message they
		// edited: then the text is partly theirs.
		if !msg.PutBack || !msg.Edited {
			http.Error(w, "keep applies only to a put-back message you edited", http.StatusConflict)
			return
		}
		if !a.requireHuman(w, r, putBackKeepAction) {
			return
		}
		p.keepPutBack(msg)
		coordLog.record(coordinationEvent{Kind: "unsent", Pane: p.ID, By: msg.Sender, Outcome: "kept"})
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "kept"})
		return
	}
	if msg.NotTyped {
		a.actOnUntypedMessage(w, p, msg, req.Action, agent)
		return
	}
	if msg.Edited && req.Action == "send" {
		http.Error(w, errPutBackEditedSend.Error(), http.StatusConflict)
		return
	}
	keys := "\r"
	if req.Action == "clear" {
		keys = "\x01\x0b"
	} // start of line, kill to end
	revision := msg.revision + 1
	// The screen and receipt are rechecked under the writer lock.
	if !agent || claude != msg.claude {
		http.Error(w, errUnsentChanged.Error(), http.StatusConflict)
		return
	}
	// Clear's control bytes are not a prompt; keep them out of prompt history.
	_, err := p.writeWithPrecondition([]byte(keys), req.Action == "send", false, func() error {
		snap := p.Snapshot()
		if snap.Status != PaneRunning || snap.Kind == "native" || snap.UnsentMessage != msg || p.inputRevision.Load() != msg.revision {
			return errUnsentChanged
		}
		screen := deliveryScreenText(p)
		if isGridChoicePromptText(screen, !msg.claude) {
			return errUnsentChanged
		}
		if msg.PutBack {
			// Gone only when no part of it shows: dropping the receipt
			// lifts the guard on agents' keys.
			if shown, known := putBackShown(screen, !msg.claude, msg.body); known && !shown {
				return errPutBackGone
			}
		}
		if msg.Edited {
			// The box is partly the person's now: Clear empties it, edit
			// and all, but only a single line, which Ctrl+A Ctrl+K clears
			// whole.
			if line, one := composerOneLine(screen, !msg.claude); !one || line == "" || strings.Contains(line, "[Pasted") {
				return errPutBackMultiLine
			}
			return nil
		}
		holds, known := composerHolds(screen, !msg.claude, msg.body)
		if !known || !holds {
			return errUnsentChanged
		}
		if req.Action == "clear" && !composerShowsExactLine(screen, !msg.claude, msg.body) {
			return errUnsentChanged
		}
		return nil
	})
	if errors.Is(err, errPutBackGone) {
		// Sent, cleared or edited with no input SoluTerminal saw: the receipt
		// is stale. Drop it, so it blocks nothing and its chip goes; Clear
		// has nothing left to do.
		p.dropUnsentMessage(msg)
		coordLog.record(coordinationEvent{Kind: "unsent", Pane: p.ID, By: msg.Sender, Outcome: req.Action + "_gone"})
		if req.Action == "clear" {
			writeJSON(w, http.StatusOK, map[string]any{"outcome": "cleared"})
			return
		}
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnsentChanged) || errors.Is(err, errPutBackMultiLine) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	var holds, known bool
	if msg.PutBack {
		// Resolved only when no part of it shows: the receipt keeps the
		// guard on agents' keys until then.
		holds, known = waitPutBackGone(p, msg, deliveryRetryAfter)
	} else {
		holds, known = waitComposer(p, !msg.claude, msg.body, false, deliveryRetryAfter)
	}
	// Resolved only when the input box is confirmed to have changed.
	resolved := req.Action
	if !known || holds {
		resolved += "_unresolved"
	}
	coordLog.record(coordinationEvent{Kind: "unsent", Pane: p.ID, By: msg.Sender, Outcome: resolved})
	outcome := string(deliveryUnconfirmed)
	if known {
		if holds {
			outcome = string(deliveryLeftInComposer)
		} else {
			outcome = string(deliverySubmitted)
			if req.Action == "clear" {
				outcome = "cleared"
			}
		}
	}
	p.mu.Lock()
	if p.unsentMessage == msg {
		if known && !holds {
			p.unsentMessage = nil
		} else {
			next := *msg
			next.revision = revision
			p.unsentMessage = &next
		}
	}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"outcome": outcome})
}

// actOnUntypedMessage resolves a message a busy pane was never given: Send
// types it now, the way send_input would have (line, gap, Enter), and Clear
// discards it. Nothing is in an input box to check, so the outcome is what
// was done. Send refuses, typing nothing, when the pane is locked against
// automatic answers (the text is an agent's), is busy again, shows a menu or
// a password prompt, or has started an agent since; the receipt stays.
func (a *apiServer) actOnUntypedMessage(w http.ResponseWriter, p *Pane, msg *unsentPaneMessage, action string, agent bool) {
	same := func() error {
		snap := p.Snapshot()
		if snap.Status != PaneRunning || snap.UnsentMessage != msg {
			return errUnsentChanged
		}
		return nil
	}
	ready := func() error {
		if err := same(); err != nil {
			return err
		}
		switch screen := deliveryScreenText(p); {
		case p.Snapshot().NeverAutoAnswer:
			return errUntypedLocked
		case agent:
			return errUntypedAgent
		case sendInputPaneBusy(p):
			return errUntypedBusy
		case p.computeStateCached() == StateChoicePrompt || isGridChoicePromptText(screen, true) || isGridChoicePromptText(screen, false) || passwordPromptRE.MatchString(lastScreenLine(screen)):
			return errUntypedPrompt
		}
		return nil
	}
	// Enter follows the line: the receipt and the lock are checked again, not
	// busyness (the line's own echo is output).
	unlocked := func() error {
		if err := same(); err != nil {
			return err
		}
		if p.Snapshot().NeverAutoAnswer {
			return errUntypedLocked
		}
		return nil
	}
	outcome := "cleared"
	if action == "send" {
		for i, step := range []struct {
			b     string
			check func() error
		}{{msg.body, ready}, {"\r", unlocked}} {
			if i > 0 {
				time.Sleep(EnterGap(false))
			}
			if _, err := p.writeWithPrecondition([]byte(step.b), true, false, step.check); err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, errUnsentChanged) || errors.Is(err, errUntypedRefused) {
					status = http.StatusConflict
				}
				if i > 0 {
					err = fmt.Errorf("the line was typed but Enter was not pressed: %w", err)
				}
				http.Error(w, err.Error(), status)
				return
			}
		}
		outcome = string(deliverySubmitted)
	} else if err := same(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	p.dropUnsentMessage(msg)
	coordLog.record(coordinationEvent{Kind: "unsent", Pane: p.ID, By: msg.Sender, Outcome: action})
	writeJSON(w, http.StatusOK, map[string]any{"outcome": outcome})
}

func (p *Pane) dropUnsentMessage(msg *unsentPaneMessage) {
	p.mu.Lock()
	if p.unsentMessage == msg {
		p.unsentMessage = nil
	}
	p.mu.Unlock()
}

var (
	errUntypedRefused = errors.New("not typed")
	errUntypedLocked  = fmt.Errorf("%w: the pane is locked against automatic answers", errUntypedRefused)
	errUntypedAgent   = fmt.Errorf("%w: an agent has started in the pane since", errUntypedRefused)
	errUntypedBusy    = fmt.Errorf("%w: the pane is busy again; send it once it settles", errUntypedRefused)
	errUntypedPrompt  = fmt.Errorf("%w: the pane is asking a question (a menu or a password)", errUntypedRefused)
)

// passwordPromptRE: a shell waiting for a password or passphrase (sudo, ssh).
var passwordPromptRE = regexp.MustCompile(`(?i)\b(password|passphrase|pin)\b[^:\n]{0,40}:\s*$`)

func lastScreenLine(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	return lines[len(lines)-1]
}

// untypedReceiptMaxBytes bounds a not-typed receipt: the whole text is shown
// to the person, and pane snapshots carry it.
const untypedReceiptMaxBytes = 4096

// hiddenOrControlRune: a control character (C0, DEL or C1; Send would type
// it raw) or an invisible format character (bidi overrides and isolates,
// zero-width characters), which can make text a person approves read
// differently from the bytes that are typed. #878's normalizeLeadRole uses
// the same rule.
func hiddenOrControlRune(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

// recordUntypedMessage keeps a message a busy pane was not given as its
// unsent receipt, and reports whether it did. It never replaces another
// sender's receipt (that sender was told the pane shows it), keeps nothing
// with control or invisible format characters (the Send… dialog must show
// exactly what is typed) or over the size bound, and keeps nothing for a
// pane locked against automatic answers.
func (p *Pane) recordUntypedMessage(sender, body string) bool {
	if body == "" || len(body) > untypedReceiptMaxBytes || strings.IndexFunc(body, hiddenOrControlRune) >= 0 {
		return false
	}
	keep := p.keptPutBack()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.NeverAutoAnswer {
		return false
	}
	if keep != nil && p.unsentMessage != nil && p.unsentMessage.ID == keep.ID {
		return false // a put-back message's receipt stays while its text may show
	}
	if cur := p.currentUnsentMessageLocked(); cur != nil {
		return cur.NotTyped && cur.body == body
	}
	p.unsentMessage = &unsentPaneMessage{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Sender: sender, NotTyped: true, Text: body, body: body, connection: p.agentConn}
	return true
}

// dropUntypedMessageFor clears the not-typed receipt for body once the same
// message has been typed after all, so the person cannot send it twice.
func (p *Pane) dropUntypedMessageFor(body string) {
	p.mu.Lock()
	if m := p.unsentMessage; m != nil && m.NotTyped && m.body == body {
		p.unsentMessage = nil
	}
	p.mu.Unlock()
}
