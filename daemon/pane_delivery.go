package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Delivering a message into another agent's input box.
//
// Writing the body and a separate Enter (submitLineToPane) is necessary but
// not sufficient: a Codex paste burst can still be absorbing the body when the
// Enter arrives (the Enter becomes a newline inside the paste), and a message
// longer than one read used to sit in the composer as "[Pasted Content …]"
// with nobody told. So an agent message is delivered as:
//
//  1. paste the body (bracketed paste for non-Claude TUIs);
//  2. wait until the input box shows it, then press Enter as its own write;
//  3. confirm the text left the input box; if it is still there, press Enter
//     once more — never retype the body;
//  4. report what happened, so callers can say "not submitted" instead of
//     pretending the message was sent.
//
// "Left the input box" is the signal, not pane busyness: our own paste echo
// makes the pane look busy before any turn starts.
//
// A Codex pane that is mid-turn is the exception to step 2. Codex 0.166 reads
// Enter during a turn as a steer and Tab (or the key it was rebound to) as
// "queue until the turn ends", and its footer says so ("tab to queue
// message", "^q to queue message") once the box holds a draft. A message to a
// working Codex is queued with that key, the sender is told it is queued, and
// the queue is watched: Codex sends it when the turn ends, but an
// interrupted turn puts queued messages back into the input box, which then
// becomes the unsent-message receipt instead of silent leftover text.

// errDeliveryLeftInComposer marks a message that reached the input box but
// was not submitted. The text is still there, so callers must not retype or
// re-deliver it elsewhere (that duplicates the message); they surface it.
var errDeliveryLeftInComposer = errors.New("message left in the input box, not submitted")
var errDeliveryChoicePrompt = errors.New("pane is showing a choice menu; message was not sent")

// errDeliveryWouldSteer: deliveryNoSteer stopped before Enter, which would
// have steered a running Codex turn that offers no queue key it can press.
var errDeliveryWouldSteer = errors.New("that Codex agent is mid-turn and its footer offers no queue key SoluTerminal can press, so the message was not sent (Enter would steer the running turn)")

// deliveryStopError is a delivery that stopped after the paste, before a
// submit key. Its text says where the pasted message is, so the caller knows
// whether sending it again would type it twice.
type deliveryStopError struct {
	kind error // errDeliveryChoicePrompt or errDeliveryWouldSteer
	msg  string
}

func (e deliveryStopError) Error() string { return e.msg }
func (e deliveryStopError) Unwrap() error { return e.kind }

// deliveryMode: deliveryNoSteer (agents: ask_pane and send_input over MCP)
// never presses Enter into a running Codex turn that offers no queue key;
// deliveryPlain keeps Enter there (the person's own message box, schedules).
type deliveryMode int

const (
	deliveryPlain deliveryMode = iota
	deliveryNoSteer
)

type deliveryOutcome string

const (
	// deliverySubmitted: the text left the input box after Enter.
	deliverySubmitted deliveryOutcome = "submitted"
	// deliveryUnconfirmed: Enter was sent but the input box could not be
	// read, so delivery could not be checked (unknown layout).
	deliveryUnconfirmed deliveryOutcome = "sent_unconfirmed"
	// deliveryLeftInComposer: the text is still in the input box after a
	// second Enter; the user (or caller) has to act on it.
	deliveryLeftInComposer deliveryOutcome = "left_in_composer"
	// deliveryQueued: Codex was mid-turn, so the message went into its own
	// follow-up queue; Codex sends it when the current turn ends.
	deliveryQueued deliveryOutcome = "queued"
)

// Timing and screen access are variables so tests can drive the loop.
var (
	deliveryVisibleTimeout = 3 * time.Second
	deliverySubmitTimeout  = 3 * time.Second
	deliveryRetryAfter     = 1200 * time.Millisecond
	deliveryPollInterval   = 150 * time.Millisecond
	// deliveryQueueHintWait is how long a running Codex turn is given to draw
	// its "tab to queue" footer before deliveryNoSteer stops.
	deliveryQueueHintWait = time.Second
	// deliveryQueueListPolls: reads of Codex's queue list after the queue
	// key, deliveryPollInterval apart, before the message counts as sent.
	deliveryQueueListPolls = 5
	// deliveryCodexTurnOpen reports a Codex turn still open in the rollout.
	deliveryCodexTurnOpen = codexRolloutTurnOpen
	deliverySleep         = time.Sleep
	deliveryScreenText    = paneVisibleScreen
	// deliveryMenuLive confirms a menu on screen is still unanswered.
	deliveryMenuLive = func(p *Pane) bool { return p.computeStateCached() == StateChoicePrompt }
	// A queued Codex message is followed until it leaves the queue. Turns
	// can run for hours; the watch ends with the pane or after the limit.
	queueWatchInterval = time.Second
	queueWatchLimit    = 4 * time.Hour
	queueWatchSleep    = time.Sleep
	queueWatchEnded    = func(*Pane, string) {}
)

// paneVisibleScreen prefers the grid model the state machine reads (menus,
// prompts); the replay emulator is not always attached.
func paneVisibleScreen(p *Pane) string {
	if screen, ok := p.gridVisibleText(); ok {
		return screen
	}
	if screen, ok := p.screenVisibleText(); ok {
		return screen
	}
	if p.buffer == nil {
		return ""
	}
	return stripANSI(p.buffer.SnapshotTail(8192))
}

// composerDraft returns the whitespace-free text of the agent's input box and
// whether the layout was recognised.
func composerDraft(screen string, codex bool) (string, bool) {
	if !codex {
		_, draft, known := conditionalClaudeComposer(screen)
		return draft, known
	}
	lines := strings.Split(screen, "\n")
	start := -1
	for i, line := range lines {
		if codexComposerLineRE.MatchString(line) {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	var parts []string
	for i, line := range lines[start:] {
		if codexFooterLineRE.MatchString(line) {
			break
		}
		if i == 0 {
			line = strings.TrimPrefix(strings.TrimSpace(line), "›")
		}
		parts = append(parts, line)
	}
	return conditionalDraftText(strings.Join(parts, "")), true
}

// composerHolds reports whether the input box still shows body (or the paste
// placeholder a long body collapses into), and whether the box was readable.
func composerHolds(screen string, codex bool, body string) (holds, known bool) {
	draft, known := composerDraft(screen, codex)
	if !known || draft == "" {
		return false, known
	}
	// Claude and Codex collapse a long or multi-line paste into a
	// "[Pasted …]" placeholder; a short single line never collapses, so a
	// placeholder then is someone else's paste, not ours.
	if strings.Contains(draft, "[Pasted") && (len(body) > 200 || strings.Contains(body, "\n")) {
		return true, true
	}
	return strings.Contains(draft, deliveryMarker(body)), true
}

// deliveryMarker is the start of the body as the composer renders it: no
// whitespace, at most 24 runes (wrapping and collapsing keep the start).
func deliveryMarker(body string) string {
	m := conditionalDraftText(body)
	if utf8.RuneCountInString(m) <= 24 {
		return m
	}
	return string([]rune(m)[:24])
}

// waitComposer polls until composerHolds(...) == want or the timeout passes.
// It returns the last observation. An unreadable input box returns at once:
// waiting cannot observe anything, so the caller falls back to the plain
// gap-then-Enter timing instead of adding a dead delay.
func waitComposer(p *Pane, codex bool, body string, want bool, timeout time.Duration) (holds, known bool) {
	deadline := time.Now().Add(timeout)
	for {
		holds, known = composerHolds(deliveryScreenText(p), codex, body)
		if !known || holds == want || !time.Now().Before(deadline) {
			return holds, known
		}
		deliverySleep(deliveryPollInterval)
	}
}

// submitAgentMessage types body into a Claude or Codex pane and submits it,
// following the contract at the top of this file. It returns the bytes
// written and the outcome; an error means a PTY write failed or delivery
// stopped (a menu, or a turn deliveryNoSteer will not steer).
func submitAgentMessage(p *Pane, body string, hasClaude bool, senders ...string) (wrote int, outcome deliveryOutcome, err error) {
	return deliverAgentMessage(p, body, hasClaude, deliveryPlain, senders...)
}

func deliverAgentMessage(p *Pane, body string, hasClaude bool, mode deliveryMode, senders ...string) (wrote int, outcome deliveryOutcome, err error) {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	defer func() {
		ev := coordinationEvent{Kind: "deliver", Pane: p.ID, Outcome: string(outcome)}
		if len(senders) > 0 {
			ev.By = senders[0]
		}
		switch {
		case errors.Is(err, errDeliveryChoicePrompt):
			ev.Outcome = "menu"
		case errors.Is(err, errDeliveryWouldSteer):
			ev.Outcome = "would_steer"
		case err != nil:
			ev.Outcome = "error"
		}
		coordLog.record(ev)
	}()
	// A queued delivery may have waited behind a whole turn. Recheck the
	// current screen after acquiring the delivery lock, before typing anything.
	// The pane state confirms the menu is live: an answered menu can stay on
	// screen, and computeState tracks which ones were answered.
	if isGridChoicePromptText(deliveryScreenText(p), !hasClaude) && deliveryMenuLive(p) {
		return 0, "", errDeliveryChoicePrompt
	}
	body = strings.TrimRight(body, "\r\n")
	sender := "SoluTerminal"
	if len(senders) > 0 && senders[0] != "" {
		sender = senders[0]
	}
	var revision uint64
	defer func() {
		if outcome == deliveryLeftInComposer {
			p.recordUnsentMessage(sender, body, hasClaude, revision)
		}
	}()
	codex := !hasClaude
	first := []byte(body)
	if codex {
		first = []byte("\x1b[200~" + body + "\x1b[201~")
	}
	wrote, err = p.Write(first)
	if err != nil {
		return wrote, "", errors.New("write failed: " + err.Error())
	}
	// Enter must not land inside the paste: wait until the box shows it, and
	// never less than the established gap.
	deliverySleep(EnterGap(hasClaude))
	seen, _ := waitComposer(p, codex, body, true, deliveryVisibleTimeout)
	key, queue, stop := chooseSubmitKey(p, codex, mode)
	if stop != nil {
		return wrote, "", p.stopAfterPaste(stop, seen, sender, body, hasClaude)
	}
	n, err := p.Write(key)
	wrote += n
	if err != nil {
		return wrote, "", errors.New("submit key failed: " + err.Error())
	}
	holds, known := waitComposer(p, codex, body, false, deliveryRetryAfter)
	if !known {
		return wrote, deliveryUnconfirmed, nil
	}
	if !holds {
		return wrote, p.leftComposerOutcome(queue, body, senders...), nil
	}
	// Still in the box: the first key became a newline inside the paste.
	// The turn may have ended meanwhile, so the key is chosen again.
	if key, queue, stop = chooseSubmitKey(p, codex, mode); stop != nil {
		return wrote, "", p.stopAfterPaste(stop, true, sender, body, hasClaude)
	}
	revision = p.inputRevision.Load() + 1
	n, err = p.Write(key)
	wrote += n
	if err != nil {
		return wrote, "", errors.New("second submit key failed: " + err.Error())
	}
	holds, known = waitComposer(p, codex, body, false, deliverySubmitTimeout)
	switch {
	case !known:
		return wrote, deliveryUnconfirmed, nil
	case holds:
		return wrote, deliveryLeftInComposer, nil
	}
	return wrote, p.leftComposerOutcome(queue, body, senders...), nil
}

// chooseSubmitKey picks the key for the pasted draft, or says why none may
// be pressed. An approval can open while the paste lands, and Enter would
// answer it. In deliveryNoSteer mode a Codex turn that is running but offers
// no queue key it can press (an unbound key, one other than Tab or a Ctrl
// letter, an older Codex, the footer in its Esc or overlay mode) gets no
// Enter, which would steer it: the footer is given
// deliveryQueueHintWait to draw, then delivery stops.
func chooseSubmitKey(p *Pane, codex bool, mode deliveryMode) (key []byte, queue bool, stop error) {
	deadline := time.Now().Add(deliveryQueueHintWait)
	for {
		if menuBeforeKey(p, codex) {
			return nil, false, errDeliveryChoicePrompt
		}
		key, queue = deliverySubmitKey(p, codex)
		if !codex || queue || mode != deliveryNoSteer {
			return key, queue, nil
		}
		if !codexTurnRunningText(deliveryScreenText(p)) && !deliveryCodexTurnOpen(p) {
			return key, false, nil
		}
		if !time.Now().Before(deadline) {
			return nil, false, errDeliveryWouldSteer
		}
		deliverySleep(deliveryPollInterval)
	}
}

// stopAfterPaste ends a delivery that stopped after the paste. Text the box
// showed is still there: a Codex line the screen shows exactly is removed
// when no menu is open (the caller may ask again later), anything else is
// recorded as the unsent-message receipt. Either way the error says so.
func (p *Pane) stopAfterPaste(stop error, seen bool, sender, body string, claude bool) error {
	codex := !claude
	what := "a choice menu opened after the message was typed, so it was not submitted"
	if errors.Is(stop, errDeliveryWouldSteer) {
		what = errDeliveryWouldSteer.Error()
	}
	if !seen {
		if errors.Is(stop, errDeliveryChoicePrompt) {
			return errDeliveryChoicePrompt
		}
		return deliveryStopError{stop, what + "; the input box did not show it, so check the pane before sending it again"}
	}
	if errors.Is(stop, errDeliveryWouldSteer) && composerShowsExactLine(deliveryScreenText(p), codex, body) {
		if _, err := p.Write([]byte("\x01\x0b")); err == nil { // start of line, kill to end
			if holds, known := waitComposer(p, codex, body, false, deliveryRetryAfter); known && !holds {
				return deliveryStopError{stop, what + ", and it was removed from the input box; call wait_for_pane until the turn ends, then send it again"}
			}
		}
	}
	p.recordUnsentMessage(sender, body, claude, p.inputRevision.Load())
	return deliveryStopError{stop, what + "; it is still in the input box, and the pane shows it as an unsent message. Do not send it again (it would be typed twice); tell the user"}
}

// menuBeforeKey reports a live choice menu on screen just before a submit
// key is written.
func menuBeforeKey(p *Pane, codex bool) bool {
	return isGridChoicePromptText(deliveryScreenText(p), codex) && deliveryMenuLive(p)
}

// deliverySubmitKey is Enter, or the queue key for a Codex pane whose footer
// offers one (a turn is running and the draft is in the box).
func deliverySubmitKey(p *Pane, codex bool) (key []byte, queue bool) {
	if codex {
		if key, ok := codexQueueKey(deliveryScreenText(p)); ok {
			return key, true
		}
	}
	return []byte("\r"), false
}

// leftComposerOutcome classifies text that left the input box. After the
// queue key it is queued when Codex's queue shows it (the list can draw a
// frame after the box empties, so it is read a few times); otherwise the
// turn ended between reading the footer and the key, and Codex submitted it
// as a new turn.
func (p *Pane) leftComposerOutcome(queue bool, body string, senders ...string) deliveryOutcome {
	if !queue {
		return deliverySubmitted
	}
	for i := 0; i < deliveryQueueListPolls; i++ {
		if i > 0 {
			deliverySleep(deliveryPollInterval)
		}
		if codexQueueHolds(deliveryScreenText(p), body) {
			by := ""
			if len(senders) > 0 {
				by = senders[0]
			}
			go p.watchQueuedMessage(body, by)
			return deliveryQueued
		}
	}
	return deliverySubmitted
}

func (p *Pane) recordUnsentMessage(sender, body string, claude bool, revision uint64) {
	p.mu.Lock()
	p.unsentMessage = &unsentPaneMessage{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Sender: sender, body: body, claude: claude, revision: revision, connection: p.agentConn}
	p.mu.Unlock()
}

// watchQueuedMessage follows a message in Codex's queue until it leaves it.
// Codex sends queued messages when the turn ends; an interrupted turn moves
// them back into the input box instead, and then the message is recorded as
// unsent so the pane shows its Send/Clear receipt.
func (p *Pane) watchQueuedMessage(body, by string) {
	sender := by
	if sender == "" {
		sender = "SoluTerminal"
	}
	p.mu.Lock()
	conn := p.agentConn
	p.mu.Unlock()
	same := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.Status == PaneRunning && p.agentConn == conn && !p.respawning.Load()
	}
	outcome := "gone"
	defer func() {
		coordLog.record(coordinationEvent{Kind: "queued", Pane: p.ID, By: by, Outcome: outcome})
		queueWatchEnded(p, outcome)
	}()
	for deadline := time.Now().Add(queueWatchLimit); time.Now().Before(deadline); {
		queueWatchSleep(queueWatchInterval)
		if !same() {
			return
		}
		screen := deliveryScreenText(p)
		// An approval modal covers the queue and the input box.
		if codexQueueHolds(screen, body) || isGridChoicePromptText(screen, true) {
			continue
		}
		p.deliveryMu.Lock()
		screen = deliveryScreenText(p)
		holds, known := composerHolds(screen, true, body)
		switch {
		case codexQueueHolds(screen, body) || isGridChoicePromptText(screen, true) || !known:
			p.deliveryMu.Unlock()
			continue
		case holds:
			p.recordUnsentMessage(sender, body, false, p.inputRevision.Load())
			outcome = string(deliveryLeftInComposer)
		default:
			outcome = string(deliverySubmitted)
		}
		p.deliveryMu.Unlock()
		return
	}
}

// paneAgentProvider reports whether p runs Claude or Codex in its terminal;
// other panes (shells, REPLs) keep raw keystroke semantics.
func paneAgentProvider(p *Pane) (agent bool, hasClaude bool) {
	snap := p.Snapshot()
	if snap.Kind == "native" {
		return false, false
	}
	// The recorded agent wins: a Codex pane whose tool spawned a claude child
	// still needs Codex delivery (bracketed paste).
	switch strings.TrimSpace(snap.Agent) {
	case agentCodex:
		return true, false
	case agentClaude:
		return true, true
	}
	if p.HasClaudeDescendant() {
		return true, true
	}
	if p.HasCodexDescendant() {
		return true, false
	}
	return false, false
}

// deliveryNotice is the caller-facing sentence for an outcome.
func deliveryNotice(o deliveryOutcome) string {
	switch o {
	case deliverySubmitted:
		return "the agent received it"
	case deliveryLeftInComposer:
		return "NOT submitted: the text is still in that pane's input box (pressing Enter twice did not send it); tell the user, do not resend"
	case deliveryUnconfirmed:
		return "Enter was sent but delivery could not be confirmed from the screen"
	case deliveryQueued:
		return "queued: that Codex agent is mid-turn, so the message is in its queue and goes in when the current turn ends; do not resend"
	}
	return string(o)
}
