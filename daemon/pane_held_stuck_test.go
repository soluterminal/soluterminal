package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const heldStuckSecret = "do-not-show-this-message-text"

// holdStuck leaves one held message per sender on a pane whose input box has
// text, the oldest already past heldStuckAfter.
func holdStuck(t *testing.T, id string, senders ...string) (*Pane, *atomic.Value) {
	t.Helper()
	p, _, screen := newDraftGuardPane(t, id)
	screen.Store(deliveryCodexScreen("halfway through"))
	for _, sender := range senders {
		if got := <-agentDelivery(p, "[from "+sender+"] "+heldStuckSecret+" "+sender, sender); got != deliveryHeld {
			t.Fatalf("delivery = %q, want held", got)
		}
	}
	p.mu.Lock()
	p.heldDeliveries[0].At = time.Now().Add(-heldStuckAfter - time.Second).UnixMilli()
	p.heldReason = heldBoxTextReason
	p.mu.Unlock()
	return p, screen
}

// stuckPaneNoDrain is a pane with held messages and no drain goroutine, so a
// test alone sets its reason.
func stuckPaneNoDrain(id string, senders ...string) *Pane {
	p := &Pane{ID: id, Status: PaneRunning, heldReason: heldBoxTextReason}
	at := time.Now().Add(-heldStuckAfter - time.Second).UnixMilli()
	for _, sender := range senders {
		p.heldDeliveries = append(p.heldDeliveries, &heldDelivery{ID: "held-" + sender, Sender: sender, Text: heldStuckSecret, body: heldStuckSecret, At: at})
	}
	return p
}

func TestHeldMessagesAreStuckOnlyBehindUnattendedText(t *testing.T) {
	p := stuckPaneNoDrain("pane_stuck", "@a · #1 Claude")
	stuck := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.heldStuckLocked(time.Now())
	}
	setReason := func(reason string) {
		p.mu.Lock()
		p.heldReason = reason
		p.mu.Unlock()
	}
	if !stuck() {
		t.Fatal("text in the box with nobody typing should be stuck, receipt or not")
	}
	setReason(unsentInBoxReason("@a"))
	if !stuck() {
		t.Fatal("an unsent message in the box should be stuck")
	}
	for _, reason := range []string{heldMenuReason, "the person is typing", ""} {
		setReason(reason)
		if stuck() {
			t.Fatalf("reason %q is not stuck", reason)
		}
	}
	setReason(heldBoxTextReason)
	p.noteHumanInput(time.Now())
	if stuck() {
		t.Fatal("someone typing within heldStuckAfter is working on the text")
	}
	p.noteHumanInput(time.Now().Add(-heldStuckAfter - time.Second))
	if !stuck() {
		t.Fatal("old typing should not keep it from being stuck")
	}
	p.mu.Lock()
	p.heldDeliveries[0].At = time.Now().UnixMilli()
	p.mu.Unlock()
	if stuck() {
		t.Fatal("a message that only just arrived is not stuck yet")
	}
}

// Codex's approval menu draws "› 1. Yes, proceed" where the input box goes,
// which reads as text in the box. A menu is not stuck.
func TestCodexApprovalMenuIsNotStuck(t *testing.T) {
	p, frames, screen := newDraftGuardPane(t, "pane_stuck_menu")
	screen.Store(deliveryCodexScreen("halfway through"))
	if got := <-agentDelivery(p, "[from @a] "+heldStuckSecret, "@a · #1 Claude"); got != deliveryHeld {
		t.Fatalf("delivery = %q, want held", got)
	}
	p.mu.Lock()
	p.heldDeliveries[0].At = time.Now().Add(-heldStuckAfter - time.Second).UnixMilli()
	p.mu.Unlock()
	screen.Store(codex0166Screen(t, "approval"))
	deadline := time.Now().Add(2 * time.Second)
	for p.heldDeliveryReason() != heldMenuReason {
		if time.Now().After(deadline) {
			t.Fatalf("reason = %q, want the menu", p.heldDeliveryReason())
		}
		time.Sleep(time.Millisecond)
	}
	if p.Snapshot().HeldStuck {
		t.Fatal("a pane at an approval menu was reported stuck")
	}
	p.mu.Lock()
	noted := p.heldStuckNoted
	p.mu.Unlock()
	if noted {
		t.Fatal("a stuck episode was noted at an approval menu")
	}
	expectNoFrame(t, frames, "nothing goes into a menu")
	// The menu is answered and the text is still there: stuck again.
	screen.Store(deliveryCodexScreen("halfway through"))
	deadline = time.Now().Add(2 * time.Second)
	for !p.Snapshot().HeldStuck {
		if time.Now().After(deadline) {
			t.Fatalf("not stuck once the menu closed; reason = %q", p.heldDeliveryReason())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeldSummaryNeverCarriesTextAndUnverifiedGetsCounts(t *testing.T) {
	p, _ := holdStuck(t, "pane_stuck_summary", "@a · #1 Claude", "@b · #2 Claude")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	listed := func(caller string) string {
		text, isErr := mcpResultText(t, a.mcpToolListPanes(nil, caller))
		if isErr {
			t.Fatalf("list_panes: %s", text)
		}
		return text
	}
	verified := listed("pane_caller")
	if !strings.Contains(verified, `"held_messages":{"count":2,"senders":["@a · #1 Claude","@b · #2 Claude"],"stuck":true}`) {
		t.Fatalf("verified caller: %s", verified)
	}
	for name, caller := range map[string]string{"claimed": mcpClaimedCallerPrefix + "pane_claimed", "none": ""} {
		got := listed(caller)
		if !strings.Contains(got, `"held_messages":{"count":2,"stuck":true}`) || strings.Contains(got, "@a · #1") {
			t.Fatalf("%s caller must get counts only: %s", name, got)
		}
	}
	for _, out := range []string{verified, listed("")} {
		if strings.Contains(out, heldStuckSecret) {
			t.Fatal("list_panes carried message text")
		}
	}
	state, isErr := mcpResultText(t, a.mcpToolGetPaneState(map[string]any{"pane_id": p.ID}))
	if isErr || !strings.Contains(state, "2 held") || strings.Contains(state, "@a") || strings.Contains(state, heldStuckSecret) {
		t.Fatalf("get_pane_state = %q", state)
	}
}

func TestWaitResultReportsStuckPaneAsTheCallerMaySeeIt(t *testing.T) {
	p, _ := holdStuck(t, "pane_stuck_wait", "@a · #1 Claude")
	var res collabResult
	noteHeldOnPane(p, paneCaller{ID: "pane_caller"}, &res)
	if res.HeldMessages == nil || !res.HeldMessages.Stuck || len(res.HeldMessages.Senders) != 1 || !strings.Contains(res.Hint, "Do not resend") {
		t.Fatalf("verified = %+v", res)
	}
	res = collabResult{}
	noteHeldOnPane(p, paneCaller{Claimed: "pane_claimed"}, &res)
	if res.HeldMessages == nil || res.HeldMessages.Count != 1 || res.HeldMessages.Senders != nil {
		t.Fatalf("unverified = %+v", res.HeldMessages)
	}
	b, err := json.Marshal(res)
	if err != nil || strings.Contains(string(b), heldStuckSecret) {
		t.Fatalf("wait result carried message text: %s (%v)", b, err)
	}
}

func TestPushKeepsTheUnverifiedMark(t *testing.T) {
	held := []heldDelivery{{Sender: "@lead · #1 Claude"}, {Sender: "@lead · #1 Claude (unverified)"}, {Sender: "an unverified agent"}, {Sender: "@a · #2 Claude"}, {Sender: "@a · #3 Claude"}}
	_, summary := heldStuckPushText("", held)
	if want := "5 from @lead, @lead (unverified), an unverified agent, 1 more, behind the text in the input box"; summary != want {
		t.Fatalf("summary = %q, want %q", summary, want)
	}
}

func TestStuckEpisodeNotifiesOnceWithoutMessageText(t *testing.T) {
	p := stuckPaneNoDrain("pane_stuck_once", "@a · #1 Claude", "@b · #2 Claude")
	p.Title = "@ops"
	type pushed struct{ pane, title, summary string }
	fired := make(chan pushed, 4)
	fire := func(pane, title, summary string) { fired <- pushed{pane, title, summary} }
	heldStuckPush.Store(&fire)
	t.Cleanup(func() { heldStuckPush.Store(nil) })
	p.noteHeldStuck(time.Now())
	p.noteHeldStuck(time.Now())
	select {
	case got := <-fired:
		if got.pane != p.ID || got.title != "Messages waiting · @ops" || got.summary != "2 from @a, @b, behind the text in the input box" {
			t.Fatalf("push = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push for a stuck pane")
	}
	select {
	case got := <-fired:
		t.Fatalf("a second push for one episode: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
}

// startSentToWorking delivers an agent's message to a working pane with no
// queue showing it, so it leaves the box as "submitted" and is watched.
func startSentToWorking(t *testing.T, id string, screen *atomic.Value, claude bool) (*Pane, <-chan directAutoFrameRead, <-chan string) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	empty := deliveryCodexScreen("")
	pasted := "\x1b[200~" + codexProbeMessage + "\x1b[201~"
	if claude {
		p.Agent = agentClaude
		empty = deliveryClaudeScreen("")
		pasted = codexProbeMessage
	} else {
		p.Agent = agentCodex
	}
	screen.Store(empty)
	stubDeliveryScreen(t, screen)
	stubQueueWatch(t)
	prevBusy, prevGuard, prevGrace, prevEnded, prevOpen := deliveryPaneBusy, draftGuardScreen, returnWatchGrace, returnWatchEnded, deliveryCodexTurnOpen
	deliveryPaneBusy = func(*Pane) bool { return true }
	deliveryCodexTurnOpen = func(*Pane) bool { return false }
	draftGuardScreen = func(*Pane) (string, string, bool) { return empty, empty, true }
	returnWatchGrace = 20 * time.Millisecond
	ended := make(chan string, 2)
	returnWatchEnded = func(_ *Pane, outcome string) { ended <- outcome }
	t.Cleanup(func() {
		deliveryPaneBusy, draftGuardScreen, returnWatchGrace, returnWatchEnded, deliveryCodexTurnOpen = prevBusy, prevGuard, prevGrace, prevEnded, prevOpen
	})
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := deliverAgentMessage(p, codexProbeMessage, claude, deliveryNoSteer, "@blob · #10 Claude")
		done <- outcome
	}()
	expectFrame(t, frames, "body", pasted)
	expectFrame(t, frames, "Enter", "\r")
	if got := <-done; got != deliverySubmitted {
		t.Fatalf("outcome = %q, want submitted", got)
	}
	return p, frames, ended
}

func startSentToWorkingCodex(t *testing.T, id string, screen *atomic.Value) (*Pane, <-chan directAutoFrameRead, <-chan string) {
	t.Helper()
	return startSentToWorking(t, id, screen, false)
}

func waitReturnWatch(t *testing.T, ended <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ended:
		if got != want {
			t.Fatalf("watch ended %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not end (want %q)", want)
	}
}

func TestSubmittedMessageBackInTheBoxBecomesAReceipt(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorkingCodex(t, "pane_returned", &screen)
	t.Cleanup(func() { stopHeldDrain(t, p) })
	screen.Store(deliveryCodexScreen(codexProbeMessage))
	waitReturnWatch(t, ended, string(deliveryLeftInComposer))
	msg := p.Snapshot().UnsentMessage
	if msg == nil || !msg.PutBack || msg.Sender != "@blob · #10 Claude" || msg.body != codexProbeMessage {
		t.Fatalf("receipt = %+v", msg)
	}
	expectNoFrame(t, frames, "the watch never types")
}

func TestSubmittedMessageThatStaysGoneLeavesNoReceipt(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorkingCodex(t, "pane_taken", &screen)
	waitReturnWatch(t, ended, string(deliverySubmitted))
	if msg := p.Snapshot().UnsentMessage; msg != nil {
		t.Fatalf("receipt = %+v", msg)
	}
	expectNoFrame(t, frames, "the watch never types")
}

func TestReturnWatchLeavesOtherTextAlone(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorkingCodex(t, "pane_other_text", &screen)
	// The person's own draft, starting like the message: not exactly it.
	screen.Store(deliveryCodexScreen(codexProbeMessage + " and my own words"))
	waitReturnWatch(t, ended, string(deliverySubmitted))
	if msg := p.Snapshot().UnsentMessage; msg != nil {
		t.Fatalf("a person's draft became a receipt: %+v", msg)
	}
	expectNoFrame(t, frames, "the watch never types")
}

func TestReturnWatchWaitsOutAMenu(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorkingCodex(t, "pane_return_menu", &screen)
	t.Cleanup(func() { stopHeldDrain(t, p) })
	screen.Store(codex0166Screen(t, "approval"))
	time.Sleep(40 * time.Millisecond)
	select {
	case got := <-ended:
		t.Fatalf("a menu ended the watch: %q", got)
	default:
	}
	expectNoFrame(t, frames, "nothing goes into a menu")
	screen.Store(deliveryCodexScreen(codexProbeMessage))
	waitReturnWatch(t, ended, string(deliveryLeftInComposer))
}

func TestReturnWatchFollowsAWorkingClaude(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorking(t, "pane_return_claude", &screen, true)
	t.Cleanup(func() { stopHeldDrain(t, p) })
	// A running turn with an empty box: well past the grace, still watching.
	screen.Store(claude2110Screen(t, "queued_tool"))
	if !claudeTurnRunningText(claude2110Screen(t, "queued_tool")) {
		t.Fatal("fixture: want a running Claude turn")
	}
	time.Sleep(60 * time.Millisecond)
	select {
	case got := <-ended:
		t.Fatalf("a running turn ended the watch: %q", got)
	default:
	}
	screen.Store(deliveryClaudeScreen(codexProbeMessage))
	waitReturnWatch(t, ended, string(deliveryLeftInComposer))
	msg := p.Snapshot().UnsentMessage
	if msg == nil || !msg.PutBack || !msg.claude {
		t.Fatalf("receipt = %+v", msg)
	}
	expectNoFrame(t, frames, "the watch never types")
}

func TestReturnWatchEndsAfterAClaudeTurnThatTookIt(t *testing.T) {
	var screen atomic.Value
	p, frames, ended := startSentToWorking(t, "pane_taken_claude", &screen, true)
	waitReturnWatch(t, ended, string(deliverySubmitted))
	if msg := p.Snapshot().UnsentMessage; msg != nil {
		t.Fatalf("receipt = %+v", msg)
	}
	expectNoFrame(t, frames, "the watch never types")
}

func TestReturnWatchBaselineIsTheSubmitKey(t *testing.T) {
	body := strings.Repeat("a long message ", 30)
	for name, c := range map[string]struct {
		keyRev uint64
		want   string
	}{
		// A person's paste after the key and before the watch starts.
		"input after the key": {keyRev: 5, want: string(deliverySubmitted)},
		"no input since":      {keyRev: 6, want: string(deliveryLeftInComposer)},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newTypingHoldPane(t, "pane_return_baseline")
			var screen atomic.Value
			screen.Store(deliveryCodexScreen("[Pasted Content 450 chars]"))
			stubDeliveryScreen(t, &screen)
			stubQueueWatch(t)
			prevGrace, prevEnded, prevOpen := returnWatchGrace, returnWatchEnded, deliveryCodexTurnOpen
			returnWatchGrace = 20 * time.Millisecond
			deliveryCodexTurnOpen = func(*Pane) bool { return false }
			ended := make(chan string, 2)
			returnWatchEnded = func(_ *Pane, outcome string) { ended <- outcome }
			t.Cleanup(func() { returnWatchGrace, returnWatchEnded, deliveryCodexTurnOpen = prevGrace, prevEnded, prevOpen })
			stubDraftGuard(t, p, &screen)
			p.inputRevision.Store(6)
			if got := p.leftComposerOutcome(true, false, true, c.keyRev, body, "@a"); got != deliverySubmitted {
				t.Fatalf("outcome = %q", got)
			}
			waitReturnWatch(t, ended, c.want)
			if got := p.Snapshot().UnsentMessage != nil; got != (c.want == string(deliveryLeftInComposer)) {
				t.Fatalf("receipt present = %v", got)
			}
		})
	}
}

func TestAPastePlaceholderCountsOnlyWithNoInputSince(t *testing.T) {
	p, _ := newTypingHoldPane(t, "pane_return_paste")
	body := strings.Repeat("a long message ", 30)
	w := returnWatch{body: body, codex: true, revision: p.inputRevision.Load()}
	box := deliveryCodexScreen("[Pasted Content 450 chars]")
	if !p.boxHoldsReturned(box, w) {
		t.Fatal("no input since: the placeholder is this message")
	}
	w.revision++
	if p.boxHoldsReturned(box, w) {
		t.Fatal("input since: the placeholder may be the person's own paste")
	}
	if p.boxHoldsReturned(deliveryCodexScreen(""), w) {
		t.Fatal("an empty box holds nothing")
	}
}
