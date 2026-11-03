package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// emptyCodexBox shows the put-back message gone from the input box with no
// input SoluTerminal saw: delivery reads the screen as drawn, the draft guard
// with the faint placeholder blanked.
func emptyCodexBox(t *testing.T, p *Pane, screen interface{ Store(any) }) {
	t.Helper()
	idle := codex0166Screen(t, "idle")
	screen.Store(idle)
	setPutBackBox(t, p, putBackBox{plain: idle, typed: strings.Replace(idle, "› Ask Codex to do anything", "›", 1), ok: true})
}

func actOnPutBack(t *testing.T, p *Pane, action string) *httptest.ResponseRecorder {
	t.Helper()
	return actOnPutBackID(t, p, p.Snapshot().UnsentMessage.ID, action)
}

// actOnPutBackID is actOnPutBack on the receipt with that ID, which may be
// gone by then.
func actOnPutBackID(t *testing.T, p *Pane, id, action string) *httptest.ResponseRecorder {
	t.Helper()
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	t.Cleanup(func() { identifyCallerHook = prev })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/"+p.ID+"/unsent-message", strings.NewReader(`{"id":"`+id+`","action":"`+action+`"}`))
	req.SetPathValue("id", p.ID)
	a.actOnUnsentMessage(rec, req)
	return rec
}

// A put-back message that left the input box with no input SoluTerminal saw
// blocks nothing: the box is read, not the receipt. Clear then drops the
// stale receipt and its chip; Send says the message is gone.
func TestAPutBackMessageGoneFromTheBoxBlocksNothing(t *testing.T) {
	t.Run("agent keys", func(t *testing.T) {
		p, frames, screen := putBackPaneScreen(t, "pane_put_back_gone_keys")
		emptyCodexBox(t, p, screen)
		if p.Snapshot().UnsentMessage == nil {
			t.Fatal("precondition: the receipt is still current")
		}
		if err := p.putBackInputCheck(); err != nil {
			t.Fatalf("putBackInputCheck = %v, want nil once the box is empty", err)
		}
		a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
		text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "y"}, "pane_caller"))
		if isErr {
			t.Fatalf("send_input = %q, want the key typed", text)
		}
		expectFrame(t, frames, "the agent's key", "y")
	})
	t.Run("clear", func(t *testing.T) {
		p, frames, screen := putBackPaneScreen(t, "pane_put_back_gone_clear")
		id := p.Snapshot().UnsentMessage.ID
		emptyCodexBox(t, p, screen)
		// Clear finds it gone; or the put-back watch dropped it first.
		rec := actOnPutBackID(t, p, id, "clear")
		if !(rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"cleared"`)) && !droppedFirst(rec) {
			t.Fatalf("clear = %d %s", rec.Code, rec.Body)
		}
		if p.Snapshot().UnsentMessage != nil {
			t.Fatal("the stale receipt was kept")
		}
		expectNoFrame(t, frames, "nothing typed")
	})
	t.Run("send", func(t *testing.T) {
		p, frames, screen := putBackPaneScreen(t, "pane_put_back_gone_send")
		id := p.Snapshot().UnsentMessage.ID
		emptyCodexBox(t, p, screen)
		rec := actOnPutBackID(t, p, id, "send")
		if !(rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "no longer in the input box")) && !droppedFirst(rec) {
			t.Fatalf("send = %d %s", rec.Code, rec.Body)
		}
		if p.Snapshot().UnsentMessage != nil {
			t.Fatal("the stale receipt was kept")
		}
		expectNoFrame(t, frames, "nothing typed")
	})
}

// A key answering a menu goes to the menu while a put-back message is in the
// box. The message is still there afterwards, so its chip stays and an
// agent's Enter is still refused.
func TestAMenuAnswerLeavesAPutBackMessageAndItsGuard(t *testing.T) {
	p, frames, screen := putBackPaneScreen(t, "pane_put_back_menu")
	approval := codex0166Screen(t, "approval")
	seedGridMenu(t, p, approval)
	if p.computeState() != StateChoicePrompt {
		t.Fatal("fixture is not a menu on screen")
	}
	screen.Store(approval)
	id := p.Snapshot().UnsentMessage.ID
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "1"}, "pane_caller"))
	if isErr {
		t.Fatalf("an agent's menu answer was refused: %q", text)
	}
	expectFrame(t, frames, "the menu answer", "1")
	if msg := p.Snapshot().UnsentMessage; msg == nil || msg.ID != id {
		t.Fatalf("the menu answer superseded the put-back message: %+v", msg)
	}
	// The menu closes; the message is still in the box.
	restored := codex0166Screen(t, "restored")
	seedGridMenu(t, p, restored)
	screen.Store(restored)
	text, isErr = mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "only the person sends or clears it") {
		t.Fatalf("send_input Enter after the menu = %q (error %v)", text, isErr)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Senders are told what holds the pane now: once the put-back message is out
// of the box, a message queued behind others is not said to wait for it.
func TestHeldReasonIsRefreshedWhenThePutBackMessageGoes(t *testing.T) {
	p, _, screen := putBackPaneScreen(t, "pane_put_back_reason_refresh")
	// Keep the queue: the drain never gets a turn.
	block := make(chan struct{})
	prevSleep := draftGuardSleep
	draftGuardSleep = func(time.Duration) { <-block }
	t.Cleanup(func() {
		// The drain reads draftGuardSleep: it must have ended before the
		// sleep is put back.
		close(block)
		stopHeldDrain(t, p)
		draftGuardSleep = prevSleep
	})
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, _ := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "first\n"}))
	if !strings.HasPrefix(text, "held: an unsent message from @flag-guard") {
		t.Fatalf("first = %q", text)
	}
	emptyCodexBox(t, p, screen)
	text, _ = mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "second\n"}))
	if strings.Contains(text, "unsent message") || !strings.HasPrefix(text, "queued: other held messages go into pane "+p.ID+" first") {
		t.Fatalf("second = %q, want a queued notice that names no unsent message", text)
	}
	if r := p.heldDeliveryReason(); r != "" {
		t.Fatalf("held reason = %q, want it refreshed", r)
	}
}

// A schedule that fires onto a put-back message is skipped (pane not ready),
// not recorded as an error, and types nothing.
func TestScheduleOntoAPutBackMessageIsSkipped(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_schedule")
	store, err := NewScheduleStore(t.TempDir(), testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	enabled := false
	sched, err := store.Create(ScheduleInput{Name: "check-in", Kind: "prompt", Cron: "*/5 * * * *", PaneID: p.ID, Prompt: "status?", Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	runs := store.List()[0].Runs
	if len(runs) != 1 || runs[0].Status != "skipped" || !strings.Contains(runs[0].Detail, "unsent message from @flag-guard") {
		t.Fatalf("runs = %+v", runs)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// An agent's socket never takes the pane from the person, whatever reason
// its claim gives; the person's takeover still works.
func TestAgentSocketClaimNeverTakesThePaneFromThePerson(t *testing.T) {
	for _, reason := range []string{"takeover", "", "first-open"} {
		t.Run("reason="+reason, func(t *testing.T) {
			logs := captureLog(t)
			h := newAdoptTestPane(t, false)
			person := h.dial(t, callerHuman, "")
			person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			person.expectOwnership(t, true, "you")
			agent := h.dial(t, callerPane, "")
			agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"`+reason+`"}`)
			// Every claim is logged once decided: wait for the agent's.
			deadline := time.Now().Add(2 * time.Second)
			for strings.Count(logs.String(), "ownership: pane=p1 claim reason=") < 2 {
				if time.Now().After(deadline) {
					t.Fatalf("the agent's claim was never decided: %q", logs.String())
				}
				time.Sleep(time.Millisecond)
			}
			if got := h.owner(); got != person.subID {
				t.Fatalf("owner = %d, want the person's %d", got, person.subID)
			}
		})
	}
}

// A held message that waits only for a menu says so, not that an unsent
// message or other messages hold it.
func TestHeldReasonNamesAMenuThatBlocksTheQueue(t *testing.T) {
	p, frames, screen := putBackPaneScreen(t, "pane_put_back_reason_menu")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	if text, _ := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "first\n"})); !strings.HasPrefix(text, "held: an unsent message") {
		t.Fatalf("first = %q", text)
	}
	emptyCodexBox(t, p, screen)
	screen.Store(codex0166Screen(t, "approval"))
	deadline := time.Now().Add(2 * time.Second)
	for p.heldDeliveryReason() != heldMenuReason {
		if time.Now().After(deadline) {
			t.Fatalf("held reason = %q, want %q", p.heldDeliveryReason(), heldMenuReason)
		}
		time.Sleep(time.Millisecond)
	}
	if got := heldNotice(p.ID, p.heldDeliveryReason()); !strings.HasPrefix(got, "queued: a menu is open in pane "+p.ID) {
		t.Fatalf("notice = %q", got)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Nor by naming the person's device: the same-device exemption is the
// person's alone, on a takeover claim and on adopt-unowned.
func TestAgentSocketNamingThePersonsDeviceCannotTakeThePane(t *testing.T) {
	for _, reason := range []string{"takeover", "adopt-unowned"} {
		t.Run(reason, func(t *testing.T) {
			logs := captureLog(t)
			h := newAdoptTestPane(t, false)
			person := h.dial(t, callerHuman, "?device=dev-person")
			person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			person.expectOwnership(t, true, "you")
			agent := h.dial(t, callerPane, "?device=dev-person")
			agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"`+reason+`"}`)
			deadline := time.Now().Add(2 * time.Second)
			for !strings.Contains(logs.String(), "DENIED sub="+strconv.FormatInt(agent.subID, 10)) {
				if time.Now().After(deadline) {
					t.Fatalf("the agent's claim was not denied: %q", logs.String())
				}
				time.Sleep(time.Millisecond)
			}
			if got := h.owner(); got != person.subID {
				t.Fatalf("owner = %d, want the person's %d", got, person.subID)
			}
		})
	}
}

// putBackInBox gives the pane's put-back message body and an input box that
// shows box (drawn from the restored Codex screen, or a Claude one). Both
// change under deliveryMu, which each put-back watch pass takes: a pass in
// between would see the new body against the old box and drop the message.
func putBackInBox(t *testing.T, p *Pane, body string, claude bool, box string) string {
	t.Helper()
	p.deliveryMu.Lock()
	defer p.deliveryMu.Unlock()
	p.mu.Lock()
	next := *p.unsentMessage
	next.body, next.claude = body, claude
	p.unsentMessage = &next
	p.mu.Unlock()
	var screen string
	if claude {
		rule := strings.Repeat("─", 60)
		screen = "● Thanks.\n\n" + rule + "\n" + box + "\n" + rule + "\n  ? for shortcuts"
	} else {
		restored := codex0166Screen(t, "restored")
		at := strings.LastIndex(restored, "› [from #3 Claude] queued probe: say CHERRY")
		if at < 0 {
			t.Fatal("fixture: no composer line")
		}
		screen = restored[:at] + box + restored[at+len("› [from #3 Claude] queued probe: say CHERRY"):]
	}
	setPutBackBox(t, p, putBackBox{plain: screen, typed: screen, ok: true})
	return screen
}

// longPutBack is an n-line put-back body and its first and last two lines as
// a Codex box draws them.
func longPutBack(n int) (body, firstLines, lastLines, middleLines string) {
	var lines []string
	for i := 1; i <= n; i++ {
		lines = append(lines, "line "+strconv.Itoa(i)+" of a long queued message from @flag-guard, with enough words to fill a row")
	}
	lines[0] = "[from @flag-guard] " + lines[0]
	box := func(a, b string) string { return "› " + a + "\n  " + b }
	return strings.Join(lines, "\n"), box(lines[0], lines[1]), box(lines[n-2], lines[n-1]), box(lines[n/2], lines[n/2+1])
}

// The refusal lifts only when the box is readable and positively without the
// message: a partial view, extra text or a paste placeholder still refuses.
func TestAPutBackMessageStillInTheBoxIsRefusedHoweverItShows(t *testing.T) {
	longBody, firstLines, lastLines, middleLines := longPutBack(40)
	// Over 8 KB: the screen tail can hold only its end.
	hugeBody, _, hugeEnd, _ := longPutBack(120)
	if len(hugeBody) <= 8<<10 {
		t.Fatalf("fixture: %d bytes, want over 8 KB", len(hugeBody))
	}
	short := "[from @flag-guard] please rerun the flaky test"
	for _, tc := range []struct {
		name, body string
		claude     bool
		box        string
		refused    bool
		edited     bool // the person typed since: the receipt is superseded
	}{
		{"(a) taller than the box, first lines shown", longBody, false, firstLines, true, false},
		{"(e) taller than the box, last lines shown", longBody, false, lastLines, true, false},
		{"(f) over 8 KB, only its end on screen", hugeBody, false, hugeEnd, true, false},
		{"taller than the box, middle lines shown", longBody, false, middleLines, true, false},
		{"(b) message plus extra text", short, false, "› " + short + " and also this", true, false},
		{"(b) message plus the person's edit", short, false, "› " + short + " and also this", true, true},
		{"(d) codex paste placeholder", longBody, false, "› [Pasted text #1 +40 lines]", true, false},
		{"(d) claude typed text and a paste placeholder", longBody, true, "❯ see this [Pasted text #1 +40 lines]", true, false},
		{"(c) empty box", short, false, "›", false, false},
		{"(c) other text without the message", short, false, "› something else entirely", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := putBackPane(t, "pane_put_back_lenient")
			putBackInBox(t, p, tc.body, tc.claude, tc.box)
			if tc.edited {
				p.inputRevision.Add(1)
				if p.Snapshot().UnsentMessage != nil {
					t.Fatal("precondition: the edit supersedes the receipt")
				}
			}
			err := p.putBackInputCheck()
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("putBackInputCheck = %v, want refused %v", err, tc.refused)
			}
		})
	}
}

// End to end: an agent's bare Enter does not send a put-back message taller
// than the box.
func TestAgentEnterNeverSendsATallPutBackMessage(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_tall")
	body, _, lastLines, _ := longPutBack(40)
	putBackInBox(t, p, body, false, lastLines)
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "only the person sends or clears it") {
		t.Fatalf("send_input Enter = %q (error %v)", text, isErr)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Clear treats a put-back message as gone only when no part of it shows: with
// just its end on screen the receipt, and the guard, stay.
func TestClearKeepsAPutBackMessageWhoseEndStillShows(t *testing.T) {
	p, frames, screen := putBackPaneScreen(t, "pane_put_back_end_clear")
	body, _, lastLines, _ := longPutBack(40)
	screen.Store(putBackInBox(t, p, body, false, lastLines))
	if rec := actOnPutBack(t, p, "clear"); rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "no longer in the input box") {
		t.Fatalf("clear = %d %s, want a 409 that keeps the message", rec.Code, rec.Body)
	}
	if p.Snapshot().UnsentMessage == nil {
		t.Fatal("the receipt was dropped while the message's end still shows")
	}
	if err := p.putBackInputCheck(); err == nil {
		t.Fatal("the guard lifted while the message's end still shows")
	}
	expectNoFrame(t, frames, "nothing typed")
}

// droppedFirst: the put-back watch dropped the stale receipt before the
// request reached it, so the request found it changed.
func droppedFirst(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "unsent message changed")
}
