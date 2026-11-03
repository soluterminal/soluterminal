package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const restoredComposer = "› " + codexProbeMessage

// putBackBoxShowing is the restored Codex screen with its input box drawn as
// box, set as both the delivery and the draft-guard screen.
func putBackBoxShowing(t *testing.T, p *Pane, screen *atomic.Value, box string) {
	t.Helper()
	restored := codex0166Screen(t, "restored")
	at := strings.LastIndex(restored, restoredComposer)
	if at < 0 {
		t.Fatal("fixture: no composer line")
	}
	s := restored[:at] + box + restored[at+len(restoredComposer):]
	setPutBackBox(t, p, putBackBox{plain: s, typed: s, ok: true})
	screen.Store(s)
}

// editedPutBack has the person type into the box holding @flag-guard's
// put-back message (box shows what they made of it), and waits until the
// watch marks the message edited.
func editedPutBack(t *testing.T, id, box string) (*Pane, <-chan directAutoFrameRead, *atomic.Value) {
	t.Helper()
	return editedPutBackWith(t, id, box, "x")
}

// editedPutBackWith is editedPutBack with the person's key given.
func editedPutBackWith(t *testing.T, id, box, key string) (*Pane, <-chan directAutoFrameRead, *atomic.Value) {
	t.Helper()
	p, frames, screen := putBackPaneScreen(t, id)
	putBackBoxShowing(t, p, screen, box)
	if _, err := p.Write([]byte(key)); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "the person's key", key)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if msg := p.Snapshot().UnsentMessage; msg != nil && msg.Edited {
			return p, frames, screen
		}
		if time.Now().After(deadline) {
			t.Fatalf("the put-back message was not marked edited: %+v", p.Snapshot().UnsentMessage)
		}
		time.Sleep(time.Millisecond)
	}
}

func asCaller(t *testing.T, kind callerKind) {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: kind, PaneID: "other-pane"} }
	t.Cleanup(func() { identifyCallerHook = prev })
}

func actOnUnsent(t *testing.T, p *Pane, action string) *httptest.ResponseRecorder {
	t.Helper()
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	id := ""
	if msg := p.Snapshot().UnsentMessage; msg != nil {
		id = msg.ID
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/"+p.ID+"/unsent-message", strings.NewReader(`{"id":"`+id+`","action":"`+action+`"}`))
	req.SetPathValue("id", p.ID)
	a.actOnUnsentMessage(rec, req)
	return rec
}

func messageBox(t *testing.T, p *Pane, text string) *httptest.ResponseRecorder {
	t.Helper()
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	body, _ := json.Marshal(map[string]string{"text": text})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/"+p.ID+"/submit-message", strings.NewReader(string(body)))
	req.SetPathValue("id", p.ID)
	a.submitPaneMessage(rec, req)
	return rec
}

// After the person types in the box, the put-back message's chip stays,
// marked edited, while part of it shows; and agents stay refused.
func TestAnEditedPutBackMessageKeepsItsChipAndItsGuard(t *testing.T) {
	p, frames, _ := editedPutBack(t, "pane_put_back_edited", restoredComposer+" x")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "only the person sends or clears it") {
		t.Fatalf("an agent's Enter after the edit = %q (error %v)", text, isErr)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// The chip goes by itself once the box shows no part of the message, edited
// or not; the guard goes with it.
func TestAPutBackMessageChipGoesOnceTheBoxNoLongerShowsIt(t *testing.T) {
	p, _, screen := editedPutBack(t, "pane_put_back_edited_gone", restoredComposer+" x")
	putBackBoxShowing(t, p, screen, "›")
	deadline := time.Now().Add(2 * time.Second)
	for p.Snapshot().UnsentMessage != nil {
		if time.Now().After(deadline) {
			t.Fatal("the chip stayed after the box emptied")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.putBackInputCheck(); err != nil {
		t.Fatalf("guard after the box emptied: %v", err)
	}
}

// When the box cannot be read, an edit does not lift the guard.
func TestAnEditedPutBackMessageInAnUnreadableBoxStillRefusesAgents(t *testing.T) {
	p, _, _ := editedPutBack(t, "pane_put_back_edited_unreadable", restoredComposer+" x")
	setPutBackBox(t, p, putBackBox{})
	if _, err := p.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := p.putBackInputCheck(); err == nil {
		t.Fatal("the guard lifted on an unreadable box after an edit")
	}
}

// Clear on an edited message empties a single-line box (edit and all) and
// the chip goes once the box is empty.
func TestClearEmptiesASingleLineEditedPutBackMessage(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, screen := editedPutBack(t, "pane_put_back_edited_clear", restoredComposer+" x")
	go func() {
		<-frames // Ctrl+A Ctrl+K, then the box is empty.
		putBackBoxShowing(t, p, screen, "›")
	}()
	if rec := actOnUnsent(t, p, "clear"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cleared"`) {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}
	if p.Snapshot().UnsentMessage != nil {
		t.Fatal("the receipt stayed after the box emptied")
	}
}

// A multi-line box cannot be emptied by Clear's keys: nothing is typed, the
// person is told to remove it by hand or keep it, and the guard stays.
func TestClearRefusesAMultiLineEditedPutBackMessage(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, _ := editedPutBack(t, "pane_put_back_edited_multi", restoredComposer+" x\n  and a second line")
	rec := actOnUnsent(t, p, "clear")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Remove the message in the pane yourself") {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "nothing typed")
	if msg := p.Snapshot().UnsentMessage; msg == nil || !msg.Edited {
		t.Fatalf("the receipt changed: %+v", msg)
	}
}

// Send is not offered on an edited message: Enter would send the person's
// edit with it.
func TestSendRefusesAnEditedPutBackMessage(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, _ := editedPutBack(t, "pane_put_back_edited_send", restoredComposer+" x")
	if rec := actOnUnsent(t, p, "send"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "you edited that message") {
		t.Fatalf("send = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Keep is the person's alone, and only for a message they edited.
func TestOnlyThePersonKeepsAnEditedPutBackMessage(t *testing.T) {
	t.Run("agent", func(t *testing.T) {
		asCaller(t, callerPane)
		p, _, _ := editedPutBack(t, "pane_put_back_keep_agent", restoredComposer+" x")
		if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Fatalf("keep by an agent = %d %s", rec.Code, rec.Body)
		}
		if p.Snapshot().UnsentMessage == nil {
			t.Fatal("an agent's refused keep removed the chip")
		}
	})
	t.Run("unedited", func(t *testing.T) {
		asCaller(t, callerHuman)
		p := putBackPane(t, "pane_put_back_keep_unedited")
		if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusConflict {
			t.Fatalf("keep on an unedited message = %d %s", rec.Code, rec.Body)
		}
	})
}

// After Keep, while any part of the text shows: the chip is gone and the
// person's own message box types again, but an agent's send_input Enter, an
// agent socket's Enter, a schedule and an agent calling the message box
// route stay refused.
func TestKeptPutBackMessageFreesOnlyThePersonsMessageBox(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, _ := editedPutBack(t, "p1", restoredComposer+" x")
	if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kept"`) {
		t.Fatalf("keep = %d %s", rec.Code, rec.Body)
	}
	if p.Snapshot().UnsentMessage != nil {
		t.Fatal("the chip stayed after Keep")
	}

	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "only the person sends or clears it") {
		t.Fatalf("an agent's Enter after Keep = %q (error %v)", text, isErr)
	}

	policy := func(r *http.Request) wsInputPolicy {
		kind := callerKind(r.Header.Get("X-Test-Caller"))
		return wsInputPolicy{human: kind == callerHuman, kind: string(kind)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/panes/{id}/io", handleWS(a.mgr, policy))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &adoptTestPane{p: p, m: a.mgr, srv: srv}
	agent := h.dial(t, callerPane, "")
	agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	agent.expectOwnership(t, true, "you")
	agent.send(t, websocket.BinaryMessage, "\r")
	expectNoFrame(t, frames, "the agent socket's Enter")

	store, err := NewScheduleStore(t.TempDir(), a.mgr, nil)
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
	if runs := store.List()[0].Runs; len(runs) != 1 || runs[0].Status != "skipped" {
		t.Fatalf("schedule runs = %+v", runs)
	}
	expectNoFrame(t, frames, "the schedule's text")

	// The route itself is the person's (403 for agents); its delivery would
	// refuse them too.
	asCaller(t, callerPane)
	if rec := messageBox(t, p, "from an agent"); rec.Code == http.StatusOK {
		t.Fatalf("an agent on the message box route = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "the agent's message")

	asCaller(t, callerHuman)
	rec := messageBox(t, p, "my own words")
	if rec.Code != http.StatusOK {
		t.Fatalf("the person's message box = %d %s", rec.Code, rec.Body)
	}
	select {
	case f := <-frames:
		if !strings.Contains(string(f.payload), "my own words") {
			t.Fatalf("the person's message box typed %q", f.payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the person's message box typed nothing")
	}
}

// Send's result on any put-back message counts only a box that shows no
// part of it as sent: with its end still showing, the receipt and the guard
// stay.
func TestSendOnAPutBackMessageKeepsItWhileItsEndShows(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, screen := putBackPaneScreen(t, "pane_put_back_send_end")
	body, firstLines, lastLines, _ := longPutBack(40)
	// Body and box change together, under deliveryMu (see putBackInBox).
	p.deliveryMu.Lock()
	p.mu.Lock()
	next := *p.unsentMessage
	next.body = body
	p.unsentMessage = &next
	p.mu.Unlock()
	putBackBoxShowing(t, p, screen, firstLines)
	p.deliveryMu.Unlock()
	go func() {
		<-frames // Enter: the box now shows only the message's end.
		putBackBoxShowing(t, p, screen, lastLines)
	}()
	rec := actOnUnsent(t, p, "send")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "left_in_composer") {
		t.Fatalf("send = %d %s, want left_in_composer", rec.Code, rec.Body)
	}
	if err := p.putBackInputCheck(); err == nil {
		t.Fatal("the guard lifted while the message's end shows")
	}
}

// After Keep, an unreadable box still holds an agent's message, also with
// no recorded draft of the person's to hold it: it would be typed onto the
// kept text and sent with it.
func TestKeptPutBackMessageInAnUnreadableBoxHoldsAgentMessages(t *testing.T) {
	asCaller(t, callerHuman)
	// A cursor key: the box's text stays the agent's in the recorder, so no
	// recorded draft of the person's holds the message below.
	p, frames, _ := editedPutBackWith(t, "pane_put_back_kept_unreadable", restoredComposer+" x", "\x1b[D")
	if n, _ := p.humanComposerPendingSnapshot(); n != 0 {
		t.Fatalf("precondition: the recorder holds a draft of the person's (%d)", n)
	}
	if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusOK {
		t.Fatalf("keep = %d %s", rec.Code, rec.Body)
	}
	setPutBackBox(t, p, putBackBox{})
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "please review\n"}))
	if !isErr || !strings.HasPrefix(text, "held: an unsent message from @flag-guard") {
		t.Fatalf("send_input = %q (error %v), want it held", text, isErr)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// agentStillRefused checks, while part of the kept put-back text shows, that
// an agent's send_input Enter and an agent's unsent-message Send are refused
// and type nothing.
func agentStillRefused(t *testing.T, p *Pane, frames <-chan directAutoFrameRead) {
	t.Helper()
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": "\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "only the person sends or clears it") {
		t.Fatalf("an agent's Enter = %q (error %v)", text, isErr)
	}
	asCaller(t, callerPane)
	if rec := actOnUnsent(t, p, "send"); rec.Code == http.StatusOK {
		t.Fatalf("an agent's unsent Send = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// After Keep, a newer unsent receipt (the person's own message left in the
// box, a stop after the paste, or a busy pane's untyped message) never
// replaces the kept put-back receipt while its text shows: the guard holds.
func TestAKeptPutBackReceiptIsNeverReplacedWhileItsTextShows(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, _ := editedPutBack(t, "pane_put_back_kept_replace", restoredComposer+" x")
	if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusOK {
		t.Fatalf("keep = %d %s", rec.Code, rec.Body)
	}
	p.mu.Lock()
	kept := p.unsentMessage.ID
	p.mu.Unlock()
	// What the message box's left_in_composer and the stop after a paste
	// (pane_delivery.go) record, and a busy pane's untyped message.
	p.recordUnsentMessage("you", "my own words", false, p.inputRevision.Load())
	if p.recordUntypedMessage("@b", "hello") {
		t.Fatal("an untyped receipt replaced the kept put-back receipt")
	}
	p.mu.Lock()
	cur := p.unsentMessage
	p.mu.Unlock()
	if cur == nil || cur.ID != kept || !cur.PutBack {
		t.Fatalf("the kept put-back receipt was replaced: %+v", cur)
	}
	agentStillRefused(t, p, frames)
}

// The same through the person's own message box ending left_in_composer.
func TestThePersonsMessageLeftInTheBoxKeepsAKeptPutBackGuarded(t *testing.T) {
	asCaller(t, callerHuman)
	p, frames, screen := editedPutBack(t, "pane_put_back_kept_box", restoredComposer+" x")
	if rec := actOnUnsent(t, p, "keep"); rec.Code != http.StatusOK {
		t.Fatalf("keep = %d %s", rec.Code, rec.Body)
	}
	// The box keeps showing the agent's text with the person's message after
	// it, so their message box ends with it left in the box.
	putBackBoxShowing(t, p, screen, restoredComposer+" x my own words")
	rec := messageBox(t, p, "my own words")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), string(deliveryLeftInComposer)) {
		t.Fatalf("message box = %d %s, want left_in_composer", rec.Code, rec.Body)
	}
	// The delivery's own keys: the paste, its Enter, and any retried Enter.
	typed := 0
	for drained := false; !drained; {
		select {
		case f := <-frames:
			t.Logf("delivery frame %q", f.payload)
			typed++
		case <-time.After(300 * time.Millisecond):
			drained = true
		}
	}
	if typed == 0 {
		t.Fatal("the person's message was not typed")
	}
	agentStillRefused(t, p, frames)
}
