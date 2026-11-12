package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
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

// heldStuckAfter is when messages held behind text in the input box become
// the person's to deal with: the oldest has waited this long and nobody has
// typed in the pane for this long. Before that they are probably mid-prompt.
const heldStuckAfter = 60 * time.Second

// heldDeliveryMaxBytes bounds the text a held message shows on the pane; the
// whole message is still what is typed.
const heldDeliveryMaxBytes = 4096

var (
	draftGuardPoll  = 500 * time.Millisecond
	draftGuardSleep = time.Sleep
	// putBackWatchPoll and putBackWatchSleep pace watchPutBack: its own, so
	// a test can hold the held-message drain without touching the watch.
	putBackWatchPoll  = 500 * time.Millisecond
	putBackWatchSleep = time.Sleep
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
	errHeldQueueFull = fmt.Errorf("%d of your messages already wait in that pane, so this one was NOT sent and NOT kept. Wait for those to go in (wait_for_pane) or tell the user, then send it again", heldPerSenderMax)
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
	footer := -1
	if codex {
		footer = codexFooterIndex(lines)
	}
	for i := start; i < len(lines); i++ {
		if i > start && (codex && i == footer || !codex && (claudeBoxRule(lines[i]) || boxBottomRE.MatchString(lines[i]) || newPromptFooterRE.MatchString(lines[i]))) {
			break
		}
		lines[i] = solid[i]
	}
	return strings.Join(lines, "\n")
}

// codexComposerPlaceholder is the delivery guard's fallback when faint styling
// does not identify Codex's known empty-box hint. Match the entire box above a
// recognised footer, so "Ask ..." drafts, suffixes and continuation text hold.
// Do not use this for Clear verification: only its version-pinned faint hint
// can prove that clearing succeeded.
func codexComposerPlaceholder(screen string) bool {
	lines := strings.Split(screen, "\n")
	start := -1
	for i, line := range lines {
		if codexComposerLineRE.MatchString(line) {
			start = i
		}
	}
	footer := codexFooterIndex(lines)
	if start < 0 || footer <= start {
		return false
	}
	box := strings.TrimSpace(strings.Join(lines[start:footer], "\n"))
	return strings.TrimSpace(strings.TrimPrefix(box, "›")) == "Ask Codex to do anything"
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
		typed := typedComposerScreen(screen, blanked, codex)
		if draft, known := composerDraft(typed, codex); known {
			if draft != "" {
				if who := p.unsentMessageInBox(typed, codex); who != "" {
					return unsentInBoxReason(who)
				}
				if codex && codexComposerPlaceholder(typed) {
					return ""
				}
				return heldBoxTextReason
			}
			return ""
		}
	}
	// The box cannot be read: an agent's unsent message there still holds
	// (typing would merge into it), as does a person's recorded draft. A
	// put-back message holds whatever input came since, kept as the person's
	// draft or not: only a readable box without it releases it.
	p.mu.Lock()
	msg := p.currentUnsentMessageLocked()
	p.mu.Unlock()
	if msg != nil && !msg.NotTyped {
		return unsentInBoxReason(msg.Sender)
	}
	if msg := p.putBackReceipt(); msg != nil {
		return unsentInBoxReason(msg.Sender)
	}
	if n, at := recorder(); n > 0 && at > 0 && now.Sub(time.UnixMilli(at)) < ariaAutoDraftStaleAfter {
		return heldBoxTextReason
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
	// are told what holds the pane now, not what held it before.
	reason := p.draftGuardReason(!claude, true, time.Now(), p.humanComposerPendingSnapshot)
	if !queued && reason == "" {
		return false, nil
	}
	return true, p.enqueueHeldDelivery(body, claude, mode, sender, delivered, reason, senderKeys...)
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

// heldMenuReason: the input box is free, but a menu is open in the pane.
const heldMenuReason = "a menu is open in that pane"

// heldBoxTextReason: the input box has text that is not a known unsent
// message. Usually the person's draft, but it can be a message the daemon lost
// track of, so it does not say the person is typing.
const heldBoxTextReason = "the person's input box has text"

// errPutBackInBox: an agent's keys would act on a put-back message.
var errPutBackInBox = errors.New("nothing was typed into that pane")

// putBackKeysCheck is putBackInputCheck for raw keys, except keys answering
// a menu live on screen at the moment of the write (they go to the menu).
// The screen is read under the writer lock, with the write, and only when
// there is a put-back message: other keys pay for one receipt read.
func (p *Pane) putBackKeysCheck() func() error {
	return func() error {
		msg := p.putBackReceipt()
		if msg == nil || menuBeforeKey(p, !msg.claude) {
			return nil
		}
		return p.putBackRefusal(msg)
	}
}

// putBackInputCheck refuses an agent's raw input while the input box holds a
// put-back message: Enter would send it and Ctrl+A Ctrl+K clear it, which
// only the person does. A write precondition, run under the writer lock;
// callers skip it for keys that answer a live menu.
func (p *Pane) putBackInputCheck() error {
	msg := p.putBackReceipt()
	if msg == nil {
		return nil
	}
	return p.putBackRefusal(msg)
}

// putBackPersonCheck is putBackInputCheck for the person's own message box:
// a put-back message they kept as their draft does not refuse it.
func (p *Pane) putBackPersonCheck() error {
	msg := p.putBackReceipt()
	if msg == nil || msg.kept {
		return nil
	}
	return p.putBackRefusal(msg)
}

// keepPutBack is the person's "Keep as my draft" on an edited put-back
// message: its chip goes and their own message box may type again. The
// receipt stays, kept, so agents, schedules and ARIA are refused until the
// box shows no part of it (watchPutBack drops it then).
func (p *Pane) keepPutBack(msg *unsentPaneMessage) {
	p.mu.Lock()
	if p.unsentMessage == msg {
		next := *msg
		next.kept = true
		p.unsentMessage = &next
	}
	p.mu.Unlock()
}

// watchPutBack follows the pane's put-back message while there is one. It
// drops the receipt once the readable box shows no part of it (sent, cleared
// or deleted, by anyone), and after the person types in the box while part
// of it still shows, keeps it current, marked edited, so its chip stays and
// the guard holds. Each pass runs under deliveryMu, so it never interleaves
// with Send, Clear or Keep acting on the receipt.
func (p *Pane) watchPutBack() {
	for {
		putBackWatchSleep(putBackWatchPoll)
		select {
		case <-p.done:
			p.mu.Lock()
			p.putBackWatching = false
			p.mu.Unlock()
			return
		default:
		}
		p.deliveryMu.Lock()
		more := p.checkPutBack()
		p.deliveryMu.Unlock()
		if !more {
			return
		}
	}
}

// checkPutBack is one watchPutBack pass; false ends the watch. Caller holds
// deliveryMu.
func (p *Pane) checkPutBack() bool {
	msg := p.putBackReceipt()
	var shown, known bool
	if msg != nil {
		if screen, typed, ok := draftGuardScreen(p); ok {
			shown, known = p.putBackShownInTypedBox(msg, screen, typed)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg == nil || p.unsentMessage != msg {
		// Gone, or replaced: a newer put-back receipt is watched on.
		if cur := p.unsentMessage; msg == nil || cur == nil || !cur.PutBack {
			p.putBackWatching = false
			return false
		}
		return true
	}
	switch rev := p.inputRevision.Load(); {
	case known && !shown:
		p.unsentMessage = nil
		p.putBackWatching = false
		return false
	case known && msg.revision != rev:
		next := *msg
		next.revision = rev
		next.Edited = true
		p.unsentMessage = &next
	}
	return true
}

// putBackReceipt is the pane's put-back message for its current agent, also
// after input that did not edit it (an answer to a menu, a refused write):
// the input box, not the receipt, says whether it is still there.
func (p *Pane) putBackReceipt() *unsentPaneMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.putBackReceiptLocked()
}

// Caller holds mu. Kept receipts still protect input while their text remains.
func (p *Pane) putBackReceiptLocked() *unsentPaneMessage {
	msg := p.unsentMessage
	if msg == nil || !msg.PutBack || p.Status != PaneRunning || p.Kind == "native" || p.respawning.Load() || msg.connection != p.agentConn {
		return nil
	}
	return msg
}

// keptPutBack is the pane's put-back receipt while the input box may still
// show any part of its text (it does, or the box can't be read), kept as the
// person's draft or not. A newer unsent receipt must not replace it then:
// that would lift the guard on agents' keys with the agent's text still in
// the box.
func (p *Pane) keptPutBack() *unsentPaneMessage {
	msg := p.putBackReceipt()
	if msg == nil {
		return nil
	}
	if screen, typed, ok := draftGuardScreen(p); ok {
		if shown, known := p.putBackShownInTypedBox(msg, screen, typed); known && !shown {
			return nil
		}
	}
	return msg
}

// putBackRefusal refuses while the input box shows msg at all
// (putBackShown), so a box showing only part of it, or it with more text,
// still refuses. Only a readable box positively without it lifts the
// refusal. When the box cannot be read, the receipt refuses, the person's
// typing in the box since included: their edit does not make the agent's
// text safe to send.
func (p *Pane) putBackRefusal(msg *unsentPaneMessage) error {
	if screen, typed, ok := draftGuardScreen(p); ok {
		if shown, known := p.putBackShownInTypedBox(msg, screen, typed); known && !shown {
			return nil
		}
	}
	return putBackError(msg)
}

// putBackMarkerRunes is how much of a message must show to count as it,
// as deliveryMarker.
const putBackMarkerRunes = 24

// putBackShown reports whether the input box shows any of body: its start or
// its end (a box taller than the screen, or the screen tail, can drop
// either), a stretch of it at the box's own start or end (only the middle
// shown), or, for a long or multi-line body, the placeholder a paste
// collapses into. known is false when the box cannot be read.
func putBackShown(screen string, codex bool, body string) (shown, known bool) {
	draft, known := composerDraft(screen, codex)
	if !known || draft == "" {
		return false, known
	}
	if strings.Contains(draft, "[Pasted") && (len(body) > 200 || strings.Contains(body, "\n")) {
		return true, true
	}
	msg := conditionalDraftText(body)
	m := []rune(msg)
	n := min(putBackMarkerRunes, len(m))
	if n == 0 {
		return false, true
	}
	if strings.Contains(draft, string(m[:n])) || strings.Contains(draft, string(m[len(m)-n:])) {
		return true, true
	}
	d := []rune(draft)
	return len(d) >= n && (strings.Contains(msg, string(d[:n])) || strings.Contains(msg, string(d[len(d)-n:]))), true
}

func putBackError(msg *unsentPaneMessage) error {
	return fmt.Errorf("%w: its input box holds an unsent message from %s, and only the person sends or clears it", errPutBackInBox, msg.Sender)
}

// carryPutBackPastMenuKeyLocked keeps a put-back receipt current across a
// key that answered a menu (rev is that key's input revision): the key went
// to the menu, not into the input box, so the message is still there and its
// chip stays. Caller holds agentWriteMu (so rev is its own key's) and mu.
func (p *Pane) carryPutBackPastMenuKeyLocked(rev uint64) {
	if msg := p.unsentMessage; msg != nil && msg.PutBack && msg.revision+1 == rev {
		next := *msg
		next.revision = rev
		p.unsentMessage = &next
	}
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

// reason is why it waits (draftGuardReason, "" behind other held messages
// alone); it becomes the pane's held reason with the message, under one lock,
// so the drain cannot clear it in between.
func (p *Pane) enqueueHeldDelivery(body string, claude bool, mode deliveryMode, sender string, delivered func(deliveryOutcome), reason string, senderKeys ...string) error {
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
			p.heldReason = reason
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
	p.heldReason = reason
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

// heldStuckLocked reports held messages waiting on text in the input box that
// nobody is working on (heldStuckAfter). A menu or a person typing is not
// stuck. It needs no unsent receipt: text the daemon cannot name holds the
// queue just the same. Caller holds mu.
func (p *Pane) heldStuckLocked(now time.Time) bool {
	if len(p.heldDeliveries) == 0 || p.Status != PaneRunning {
		return false
	}
	if p.heldReason != heldBoxTextReason && !strings.HasPrefix(p.heldReason, unsentInBoxPrefix) {
		return false
	}
	if now.Sub(time.UnixMilli(p.heldDeliveries[0].At)) < heldStuckAfter {
		return false
	}
	last := p.lastHumanInputMs.Load()
	return last == 0 || now.Sub(time.UnixMilli(last)) >= heldStuckAfter
}

// paneHeldSummary is what agents are told about a pane's held messages:
// how many, whose and whether they are stuck. Never their text.
type paneHeldSummary struct {
	Count int `json:"count"`
	// Senders: omitted for an unverified caller, which gets counts only.
	Senders []string `json:"senders,omitempty"`
	Stuck   bool     `json:"stuck,omitempty"`
}

// heldSummaryFor summarises snap's held messages for an agent; nil when there
// are none. verified: the caller was traced to a pane.
func heldSummaryFor(snap PaneSnapshot, verified bool) *paneHeldSummary {
	if len(snap.HeldMessages) == 0 {
		return nil
	}
	s := &paneHeldSummary{Count: len(snap.HeldMessages), Stuck: snap.HeldStuck}
	if verified {
		s.Senders = heldSenderLabels(snap.HeldMessages)
	}
	return s
}

// heldSenderLabels is the distinct senders of held, oldest first.
func heldSenderLabels(held []heldDelivery) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range held {
		if !seen[h.Sender] {
			seen[h.Sender] = true
			out = append(out, h.Sender)
		}
	}
	return out
}

// heldStuckPush hands a stuck pane's notice to the cloud push channel; nil
// while the machine is not paired.
var heldStuckPush atomic.Pointer[func(paneID, title, summary string)]

func init() {
	registerCloudPusherHook(func(_ *apiServer, pusher *Pusher) {
		if pusher == nil {
			heldStuckPush.Store(nil)
			return
		}
		fire := func(paneID, title, summary string) { pusher.FireAsks(paneID, title, summary) }
		heldStuckPush.Store(&fire)
	})
}

// noteHeldStuck records the start of a stuck episode, once until the queue
// empties: a log line and a push, with sender labels and no message text.
func (p *Pane) noteHeldStuck(now time.Time) {
	p.mu.Lock()
	if p.heldStuckNoted || !p.heldStuckLocked(now) {
		p.mu.Unlock()
		return
	}
	p.heldStuckNoted = true
	held := p.heldDeliveriesLocked()
	title := p.Title
	p.mu.Unlock()
	coordLog.record(coordinationEvent{Kind: "held", Pane: p.ID, By: held[0].Sender, Outcome: "stuck"})
	if fire := heldStuckPush.Load(); fire != nil {
		pushTitle, summary := heldStuckPushText(title, held)
		go (*fire)(p.ID, pushTitle, summary)
	}
}

// heldShortSender is a sender label without its pane number. An unverified
// mark stays: "@lead · #1 Claude (unverified)" must not read as the real @lead.
func heldShortSender(label string) string {
	name, _, _ := strings.Cut(label, " · ")
	if mark := " (unverified)"; strings.HasSuffix(label, mark) && !strings.HasSuffix(name, mark) {
		name += mark
	}
	return name
}

// heldStuckPushText words the push for a stuck pane.
func heldStuckPushText(paneTitle string, held []heldDelivery) (title, summary string) {
	var names []string
	for _, label := range heldSenderLabels(held) {
		if name := heldShortSender(label); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) > 3 {
		names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
	}
	title = "Messages waiting"
	if paneTitle != "" {
		title += " · " + paneTitle
	}
	return title, fmt.Sprintf("%d from %s, behind the text in the input box", len(held), strings.Join(names, ", "))
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
			p.heldStuckNoted = false
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
		reason := p.draftGuardReason(!head.claude, true, time.Now(), p.humanComposerPendingSnapshot)
		// The menu first: Codex's "› 1. Yes, proceed" row reads as text in the
		// box, and a menu is not text anyone can send or clear.
		if menuBeforeKey(p, !head.claude) {
			reason = heldMenuReason
		}
		// Refreshed every pass, cleared too: a message sent or cleared since
		// must not still be named to senders.
		p.setHeldReason(reason)
		p.noteHeldStuck(time.Now())
		if reason != "" {
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
	if reason == heldMenuReason {
		return fmt.Sprintf("queued: a menu is open in pane %s, so nothing was typed yet. Your message is held on that pane with your name and goes in by itself once the menu is answered (the person can also send it now or discard it). Do not resend it; use ask_pane to wait for a reply.", paneID)
	}
	if reason == heldBoxTextReason {
		return fmt.Sprintf("held: pane %s's input box has text, so nothing was typed. Your message is held on that pane with your name and goes in by itself once the box is empty and nobody is typing (the person can also discard it). Do not resend it; use ask_pane to wait for a reply.", paneID)
	}
	if reason == "" {
		return fmt.Sprintf("queued: other held messages go into pane %s first, so nothing was typed yet. Your message is held on that pane with your name and goes in by itself after them (the person can also send it now or discard it). Do not resend it; use ask_pane to wait for a reply.", paneID)
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
