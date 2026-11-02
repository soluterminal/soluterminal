package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// A person's draft always wins over an agent's message.
//
// 2026-11-02: two agents' ask_pane messages were typed into @release-lead's
// pane while Benjamin was halfway through a prompt there, and Enter sent his
// "…updates and inf", @cmo's whole message and his "or" as one prompt. The
// pane was idle at its prompt, and busy-aware delivery only guarded busy
// panes.
//
// So before an agent's message (deliveryNoSteer: send_input and ask_pane) is
// typed into a Claude or Codex input box, the box must be empty on screen and
// the person must not have typed in the pane for draftGuardQuiet. Otherwise
// nothing is typed: the message is held, shown on the pane with its sender
// (Send now / Discard), and typed by the daemon once the box is empty and the
// person has been quiet, usually right after they submit their own prompt.
// Held messages go in in the order they arrived, and a new one waits behind
// any already held. Nothing is ever added to, or removed from, the person's
// draft. The person's own message box and schedules (deliveryPlain) are not
// held.

// draftGuardQuiet is how long after the person's last keystroke in a pane an
// agent's message waits. It also covers a keystroke the screen has not
// echoed yet.
const draftGuardQuiet = 5 * time.Second

// heldPerSenderMax bounds one sender's held messages in one pane. A sender
// over it is refused (nothing typed, nothing kept) and told so; a person's
// typing must not let an agent pile up messages that all go in afterwards.
const heldPerSenderMax = 5

// heldPerPaneMax bounds the complete queue, across verified and unverified
// senders. All unverified identities share one per-sender allowance.
const heldPerPaneMax = 20

// heldDeliveryMaxBytes bounds the text a held message shows on the pane; the
// whole message is still what is typed.
const heldDeliveryMaxBytes = 4096

var (
	draftGuardPoll  = 500 * time.Millisecond
	draftGuardSleep = time.Sleep
	// heldDeliveryLimit: a message held longer than this is dropped and its
	// sender told. It matches how long a queued ask is waited on.
	heldDeliveryLimit = queueWatchLimit
	// draftGuardScreen reads the screen and, from the same frame, a copy with
	// faint text blanked (see grid.VisibleTextFaintBlanked).
	draftGuardScreen = paneScreenFaintBlanked
)

var (
	errDraftGuardHeld = errors.New("the person is typing in that pane")
	// Queue limits refuse the new message without typing or keeping it.
	errHeldPaneFull  = fmt.Errorf("%d messages already wait in that pane, so this one was NOT sent and NOT kept. Wait for them to go in (wait_for_pane) or tell the user, then send it again", heldPerPaneMax)
	errHeldQueueFull = fmt.Errorf("the person is typing in that pane and %d of your messages already wait there, so this one was NOT sent and NOT kept. Wait for those to go in (wait_for_pane) or tell the user, then send it again", heldPerSenderMax)
	// errDraftGuardTypedAfterPaste: input reached the pane between the paste
	// and its submit key, so the key was not pressed.
	errDraftGuardTypedAfterPaste = errors.New("input arrived after the message was typed")
)

// deliveryHeld: nothing was typed; the message waits on the pane until the
// person's input box is empty and they have stopped typing.
const deliveryHeld deliveryOutcome = "held"

type heldDelivery struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
	// Text is the message as the pane shows it (without the sender label,
	// bounded); body is what is typed.
	Text string `json:"text"`
	At   int64  `json:"at"`

	// senderKey groups held messages independently of their display label.
	senderKey  string
	body       string
	claude     bool
	mode       deliveryMode
	connection net.Conn
	// delivered runs once the message has been typed, with its outcome.
	delivered func(deliveryOutcome)
	// Set once it leaves the queue, under Pane.mu.
	resolution string // delivered | discarded | sent | gone | expired | failed
	err        error
}

// heldResolvedKeep bounds the resolved held messages a pane remembers for
// ask_pane and wait_for_pane.
const heldResolvedKeep = 16

// paneScreenFaintBlanked is the grid's screen and its faint-blanked copy.
func paneScreenFaintBlanked(p *Pane) (screen, blanked string, ok bool) {
	p.subsMu.RLock()
	em := p.gridEmu
	if em == nil {
		em = p.replayEmu
	}
	p.subsMu.RUnlock()
	if em == nil {
		return "", "", false
	}
	return em.VisibleTextFaintBlanked()
}

// typedComposerScreen is screen with the input box's rows taken from blanked,
// so a faint placeholder or suggested prompt reads as an empty box. The rest
// of the screen keeps its faint text: the layout is recognised by it (Claude
// draws its model status line faint).
func typedComposerScreen(screen, blanked string, codex bool) string {
	lines, solid := strings.Split(screen, "\n"), strings.Split(blanked, "\n")
	if len(lines) != len(solid) {
		return screen
	}
	start := -1
	for i, line := range lines {
		if codex && codexComposerLineRE.MatchString(line) || !codex && claudeComposerRow(line) {
			start = i
		}
	}
	if start < 0 {
		return screen
	}
	for i := start; i < len(lines); i++ {
		if i > start && (codex && codexFooterLineRE.MatchString(lines[i]) || !codex && (claudeBoxRule(lines[i]) || boxBottomRE.MatchString(lines[i]) || newPromptFooterRE.MatchString(lines[i]))) {
			break
		}
		lines[i] = solid[i]
	}
	return strings.Join(lines, "\n")
}

// claudeComposerRow: the row conditionalClaudeComposer takes as the input
// box's first row.
func claudeComposerRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "❯") || strings.HasPrefix(t, "❭") || strings.HasPrefix(t, "›") || strings.HasPrefix(t, "▶") || inputBoxLineRE.MatchString(line)
}

// noteHumanInput records a person's keystroke in the pane (not terminal
// protocol replies). The pane WebSocket calls it before the write, so a
// delivery checking under the writer lock sees a keystroke already waiting
// for that lock.
func (p *Pane) noteHumanInput(now time.Time) {
	p.lastHumanInputMs.Store(now.UnixMilli())
}

// draftGuardReason says why an agent's message must not be typed now, or ""
// when it may be. typing=false ignores keystroke recency (the person pressed
// Send now). recorder reads the prompt recorder's draft a person typed (text
// an agent's delivery left there does not count) and when it was last typed;
// it is only consulted when the input box cannot be read.
func (p *Pane) draftGuardReason(codex, typing bool, now time.Time, recorder func() (int, int64)) string {
	if last := p.lastHumanInputMs.Load(); typing && last > 0 && now.Sub(time.UnixMilli(last)) < draftGuardQuiet {
		return "the person is typing"
	}
	if screen, blanked, ok := draftGuardScreen(p); ok {
		if draft, known := composerDraft(typedComposerScreen(screen, blanked, codex), codex); known {
			if draft != "" {
				if who := p.unsentMessageInBox(typedComposerScreen(screen, blanked, codex), codex); who != "" {
					return unsentInBoxReason(who)
				}
				return "the person's input box has text"
			}
			return ""
		}
	}
	// The box cannot be read: an agent's unsent message there still holds
	// (typing would merge into it), as does a person's recorded draft.
	p.mu.Lock()
	msg := p.currentUnsentMessageLocked()
	p.mu.Unlock()
	if msg != nil && !msg.NotTyped {
		return unsentInBoxReason(msg.Sender)
	}
	if n, at := recorder(); n > 0 && at > 0 && now.Sub(time.UnixMilli(at)) < ariaAutoDraftStaleAfter {
		return "the person's input box has text"
	}
	return ""
}

// holdDelivery queues an agent's message instead of typing it, when the
// person is typing or messages are already held. Caller holds deliveryMu.
// It reports whether the message was held; errHeldQueueFull when it would
// have been, but the sender is over heldPerSenderMax.
func (p *Pane) holdDelivery(body string, claude bool, mode deliveryMode, sender string, delivered func(deliveryOutcome), senderKeys ...string) (bool, error) {
	p.mu.Lock()
	queued := len(p.heldDeliveries) > 0
	p.mu.Unlock()
	// The reason is read again even behind other held messages, so senders
	// are told what holds the pane now.
	reason := p.draftGuardReason(!claude, true, time.Now(), p.humanComposerPendingSnapshot)
	if !queued && reason == "" {
		return false, nil
	}
	if reason != "" {
		p.setHeldReason(reason)
	}
	return true, p.enqueueHeldDelivery(body, claude, mode, sender, delivered, senderKeys...)
}

// withoutInvisibleFormat removes format characters (Unicode category Cf)
// from an agent's message: Unicode tag characters (U+E0000–E007F) can carry
// text the receiving agent reads but the person cannot see, and bidi
// controls (also Cf) reorder what the person reads. The joiners (ZWJ, ZWNJ)
// and variation selectors stay: emoji and scripts need them, and they hide
// no text. Applied before a message is typed or shown, so what the person
// sees is what the agent receives.
func withoutInvisibleFormat(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\u200c', r == '\u200d', r >= '\ufe00' && r <= '\ufe0f', r >= 0xe0100 && r <= 0xe01ef:
			return r
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

const unsentInBoxPrefix = "an unsent message from "

// errPutBackInBox: an agent's keys would act on a put-back message.
var errPutBackInBox = errors.New("nothing was typed into that pane")

// putBackKeysCheck is putBackInputCheck for raw keys, except keys answering
// a menu live on screen at the moment of the write (they go to the menu).
// The screen is read under the writer lock, with the write.
func (p *Pane) putBackKeysCheck(codex bool) func() error {
	return func() error {
		if menuBeforeKey(p, codex) {
			return nil
		}
		return p.putBackInputCheck()
	}
}

// putBackInputCheck refuses an agent's raw input while the input box holds a
// put-back message: Enter would send it and Ctrl+A Ctrl+K clear it, which
// only the person does. A write precondition, run under the writer lock;
// callers skip it for keys that answer a live menu.
func (p *Pane) putBackInputCheck() error {
	p.mu.Lock()
	msg := p.currentUnsentMessageLocked()
	p.mu.Unlock()
	if msg != nil && msg.PutBack {
		return fmt.Errorf("%w: its input box holds an unsent message from %s that was put back after the person stopped the turn, and only the person sends or clears it", errPutBackInBox, msg.Sender)
	}
	return nil
}

// unsentInBoxReason: the input box holds an agent's message that was not
// submitted (one Codex put back after the person stopped its turn, or one
// Enter did not send), not a person's draft. Held messages wait behind it:
// typing would merge into it, and sending it would override the person.
func unsentInBoxReason(sender string) string {
	return unsentInBoxPrefix + sender + " is in that pane's input box"
}

// unsentMessageInBox is the sender of the pane's unsent message when the
// input box (on typed, the faint-blanked screen) holds exactly that message.
func (p *Pane) unsentMessageInBox(typed string, codex bool) string {
	p.mu.Lock()
	msg := p.currentUnsentMessageLocked()
	p.mu.Unlock()
	if msg == nil || msg.NotTyped || !composerHoldsExactly(typed, codex, msg.body) {
		return ""
	}
	return msg.Sender
}

// setHeldReason records why the pane's messages are held, for their
// senders' notices.
func (p *Pane) setHeldReason(reason string) {
	p.mu.Lock()
	p.heldReason = reason
	p.mu.Unlock()
}

func (p *Pane) heldDeliveryReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heldReason
}

// heldText is a message as it is held, shown and typed: control characters
// other than newline and tab (an Escape or carriage return would act on the
// agent's input box), bidirectional controls (which make the text the
// person reads differ from its order) and other invisible format characters
// (withoutInvisibleFormat) are removed; CRLF becomes a newline.
func heldText(s string) string {
	s = withoutInvisibleFormat(strings.ReplaceAll(s, "\r\n", "\n"))
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, s)
}

func (p *Pane) enqueueHeldDelivery(body string, claude bool, mode deliveryMode, sender string, delivered func(deliveryOutcome), senderKeys ...string) error {
	senderKey := heldSenderKey(sender, senderKeys)
	quotaKey := heldSenderQuotaKey(senderKey)
	body = heldText(body)
	text := stripSenderLabel(body)
	if len(text) > heldDeliveryMaxBytes {
		text = strings.ToValidUTF8(text[:heldDeliveryMaxBytes], "") + "…"
	}
	p.mu.Lock()
	// The same message from the same sender again (a resend) waits once.
	mine := 0
	for _, h := range p.heldDeliveries {
		if h.senderKey == senderKey && stripSenderLabel(h.body) == stripSenderLabel(body) {
			p.mu.Unlock()
			return nil
		}
		if heldSenderQuotaKey(h.senderKey) == quotaKey {
			mine++
		}
	}
	if mine >= heldPerSenderMax {
		p.mu.Unlock()
		coordLog.record(coordinationEvent{Kind: "deliver", Pane: p.ID, By: sender, Outcome: "held_full"})
		return errHeldQueueFull
	}
	if len(p.heldDeliveries) >= heldPerPaneMax {
		p.mu.Unlock()
		coordLog.record(coordinationEvent{Kind: "deliver", Pane: p.ID, By: sender, Outcome: "held_full"})
		return errHeldPaneFull
	}
	now := time.Now()
	p.heldSeq++
	h := &heldDelivery{ID: fmt.Sprintf("held-%d-%d", now.UnixNano(), p.heldSeq), Sender: sender, senderKey: senderKey, Text: text, At: now.UnixMilli(), body: body, claude: claude, mode: mode, connection: p.agentConn, delivered: delivered}
	p.heldDeliveries = append(p.heldDeliveries, h)
	start := !p.heldDraining
	p.heldDraining = true
	p.mu.Unlock()
	coordLog.record(coordinationEvent{Kind: "deliver", Pane: p.ID, By: sender, Outcome: string(deliveryHeld)})
	if start {
		go p.drainHeldDeliveries()
	}
	return nil
}

// heldDeliveriesLocked is the pane's held messages, oldest first, for its
// snapshot. Caller holds mu.
func (p *Pane) heldDeliveriesLocked() []heldDelivery {
	if len(p.heldDeliveries) == 0 {
		return nil
	}
	out := make([]heldDelivery, 0, len(p.heldDeliveries))
	for _, h := range p.heldDeliveries {
		out = append(out, heldDelivery{ID: h.ID, Sender: h.Sender, Text: h.Text, At: h.At})
	}
	return out
}

// resolveHeldLocked takes h out of the queue with its resolution. It reports
// whether h was still queued. Caller holds mu.
func (p *Pane) resolveHeldLocked(h *heldDelivery, resolution string, err error) bool {
	for i, q := range p.heldDeliveries {
		if q == h {
			p.heldDeliveries = append(p.heldDeliveries[:i:i], p.heldDeliveries[i+1:]...)
			h.resolution, h.err = resolution, err
			p.heldResolved = append(p.heldResolved, h)
			if len(p.heldResolved) > heldResolvedKeep {
				p.heldResolved = p.heldResolved[len(p.heldResolved)-heldResolvedKeep:]
			}
			return true
		}
	}
	return false
}

func (p *Pane) resolveHeld(h *heldDelivery, resolution string, err error) bool {
	p.mu.Lock()
	ok := p.resolveHeldLocked(h, resolution, err)
	p.mu.Unlock()
	if ok && resolution != "delivered" && resolution != "sent" {
		coordLog.record(coordinationEvent{Kind: "held", Pane: p.ID, By: h.Sender, Outcome: resolution})
	}
	return ok
}

// heldDeliveryStatus reports what became of the held message with this id:
// "held" while it waits, its resolution once it left the queue, "" when the
// pane does not know it (or forgot it).
func (p *Pane) heldDeliveryStatus(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.heldDeliveries {
		if h.ID == id {
			return string(deliveryHeld), nil
		}
	}
	for _, h := range p.heldResolved {
		if h.ID == id {
			return h.resolution, h.err
		}
	}
	return "", nil
}

// pendingHeldID is the id of sender's held message whose text is msg (sender
// labels ignored), or "" when none waits: it was typed already, or never held.
func (p *Pane) pendingHeldID(sender, msg string, senderKeys ...string) string {
	senderKey := heldSenderKey(sender, senderKeys)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.heldDeliveries) - 1; i >= 0; i-- {
		if h := p.heldDeliveries[i]; h.senderKey == senderKey && queuedPromptMatches(h.body, heldText(msg)) {
			return h.ID
		}
	}
	return ""
}

func (p *Pane) heldDeliveryByID(id string) *heldDelivery {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.heldDeliveries {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// drainHeldDeliveries types the held messages, oldest first, each once the
// person's input box is empty, they have been quiet for draftGuardQuiet and
// no menu is open. It ends when the queue is empty, and drops the queue when
// the pane exits or its agent connection changes.
func (p *Pane) drainHeldDeliveries() {
	for {
		draftGuardSleep(draftGuardPoll)
		p.mu.Lock()
		if len(p.heldDeliveries) == 0 {
			p.heldDraining = false
			p.heldReason = ""
			p.mu.Unlock()
			return
		}
		head := p.heldDeliveries[0]
		gone := p.Status != PaneRunning || p.agentConn != head.connection || p.respawning.Load()
		p.mu.Unlock()
		switch {
		case gone:
			p.deliveryMu.Lock()
			p.mu.Lock()
			held := append([]*heldDelivery(nil), p.heldDeliveries...)
			p.mu.Unlock()
			for _, h := range held {
				p.resolveHeld(h, "gone", nil)
			}
			p.deliveryMu.Unlock()
			continue
		case time.Since(time.UnixMilli(head.At)) >= heldDeliveryLimit:
			p.resolveHeld(head, "expired", nil)
			continue
		}
		if reason := p.draftGuardReason(!head.claude, true, time.Now(), p.humanComposerPendingSnapshot); reason != "" {
			p.setHeldReason(reason)
			continue
		}
		if menuBeforeKey(p, !head.claude) {
			continue
		}
		p.deliverHeld(head, true)
	}
}

// deliverHeld types h if it is still queued and the person's draft still
// allows it, rechecked under deliveryMu and again under the writer lock.
// typing=false is the person's Send now: keystroke recency is ignored, the
// input box must still be empty. It reports the outcome, or deliveryHeld
// when h stays queued.
func (p *Pane) deliverHeld(h *heldDelivery, typing bool) (deliveryOutcome, error) {
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	p.mu.Lock()
	queued := false
	for _, q := range p.heldDeliveries {
		queued = queued || q == h
	}
	// The agent this message was held for must still be the one running.
	gone := p.Status != PaneRunning || p.agentConn != h.connection || p.respawning.Load()
	p.mu.Unlock()
	if !queued {
		return "", errUnsentChanged
	}
	if gone {
		p.resolveHeld(h, "gone", nil)
		return "", errUnsentChanged
	}
	check := draftBoxOnly
	if typing {
		check = draftBoxAndTyping
	}
	wrote, outcome, err := typeAgentMessageLocked(p, h.body, h.claude, h.mode, check, h.Sender)
	if errors.Is(err, errDraftGuardHeld) || errors.Is(err, errDeliveryChoicePrompt) && wrote == 0 {
		// Nothing was typed: the person's draft, or a menu that opened.
		return deliveryHeld, err
	}
	resolution := "delivered"
	if !typing {
		resolution = "sent"
	}
	if err != nil {
		resolution = "failed"
	}
	if p.resolveHeld(h, resolution, err) && err == nil && h.delivered != nil {
		h.delivered(outcome)
	}
	return outcome, err
}

// heldNotice is send_input's answer for a held message.
func heldNotice(paneID, reason string) string {
	if strings.HasPrefix(reason, unsentInBoxPrefix) {
		return fmt.Sprintf("held: %s %s, so nothing was typed. Your message waits on that pane, queued behind it, and goes in once the person sends or clears that message. Do not resend it; use ask_pane to wait for a reply.", strings.TrimSuffix(reason, " is in that pane's input box"), "is in pane "+paneID+"'s input box")
	}
	return fmt.Sprintf("queued: the person is typing in pane %s, so nothing was typed. Your message is held on that pane with your name and goes in by itself once their input box is empty and they have stopped typing (they can also send it now or discard it). Do not resend it; use ask_pane to wait for a reply.", paneID)
}

const heldActionName = "send or discard a message held while the person types"

// actOnHeldDelivery is the pane's Send now / Discard on a held message. Send
// now types it at once, however recently the person typed, but never into an
// input box that has text: that would mix the message into their draft.
// Discard drops it untyped; its sender is told when it next waits on it.
func (a *apiServer) actOnHeldDelivery(w http.ResponseWriter, p *Pane, h *heldDelivery, action string) {
	if action == "clear" {
		// Under deliveryMu: a delivery of h in progress finishes first, and
		// then h is no longer queued, so a discard never reports success for
		// a message that is being typed.
		p.deliveryMu.Lock()
		ok := p.resolveHeld(h, "discarded", nil)
		p.deliveryMu.Unlock()
		if !ok {
			http.Error(w, errUnsentChanged.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "cleared"})
		return
	}
	outcome, err := p.deliverHeld(h, false)
	switch {
	case errors.Is(err, errDraftGuardHeld):
		http.Error(w, "your input box has text: send or clear it first, then send this message", http.StatusConflict)
		return
	case errors.Is(err, errDeliveryChoicePrompt) && outcome == deliveryHeld:
		http.Error(w, "the pane is showing a menu: answer it first", http.StatusConflict)
		return
	case errors.Is(err, errUnsentChanged):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcome": string(outcome)})
}

// Internal delivery paths use their display label; MCP supplies a stable
// caller key. Anonymous deliveries share the unverified quota.
func heldSenderKey(sender string, keys []string) string {
	if len(keys) > 0 && keys[0] != "" {
		return keys[0]
	}
	if sender == "SoluTerminal" {
		return mcpClaimedCallerPrefix
	}
	return sender
}

func heldSenderQuotaKey(key string) string {
	if strings.HasPrefix(key, mcpClaimedCallerPrefix) {
		return mcpClaimedCallerPrefix
	}
	return key
}
