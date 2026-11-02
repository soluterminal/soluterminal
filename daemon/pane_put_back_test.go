package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// putBackPane has @flag-guard's queued message put back into a Codex
// pane's input box by an interrupted turn (testdata/codex_0166/restored.txt).
func putBackPane(t *testing.T, id string) *Pane {
	t.Helper()
	p, _ := putBackPaneFrames(t, id)
	return p
}

func putBackPaneFrames(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames, screen := queueAgentMessageScreen(t, id)
	restored := codex0166Screen(t, "restored")
	screen.Store(restored)
	deadline := time.Now().Add(2 * time.Second)
	for p.Snapshot().UnsentMessage == nil {
		if time.Now().After(deadline) {
			t.Fatal("the put-back message was not recorded as unsent")
		}
		time.Sleep(time.Millisecond)
	}
	// The draft guard now reads the put-back message in the box.
	draftGuardScreen = func(*Pane) (string, string, bool) { return restored, restored, true }
	return p, frames
}

func TestPutBackMessageIsMarkedAndHoldsLaterMessagesBehindIt(t *testing.T) {
	p := putBackPane(t, "pane_put_back")
	msg := p.Snapshot().UnsentMessage
	if !msg.PutBack || msg.Sender != "@flag-guard" || msg.NotTyped {
		t.Fatalf("receipt = %+v, want a put-back message from @flag-guard", msg)
	}
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_put_back", "text": "please review\n"}))
	if !isErr || !strings.HasPrefix(text, "held: an unsent message from @flag-guard is in pane pane_put_back's input box") || strings.Contains(text, "typing") {
		t.Fatalf("send_input = %q (error %v)", text, isErr)
	}
	text, isErr = mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_put_back", "text": "and a question", "timeout_seconds": 0.3}, "pane_caller"))
	if isErr {
		t.Fatalf("ask_pane refused: %s", text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatal(err)
	}
	if res.Delivery != "held" || !strings.HasPrefix(res.Hint, "held: an unsent message from @flag-guard is in that pane's input box") {
		t.Fatalf("ask_pane = %+v", res)
	}
	if n := len(p.Snapshot().HeldMessages); n != 2 {
		t.Fatalf("held %d messages behind the put-back one, want 2", n)
	}
	// Never cleared or changed.
	if p.Snapshot().UnsentMessage != msg {
		t.Fatal("the put-back message changed")
	}
}

func TestOnlyThePersonSendsOrClearsAPutBackMessage(t *testing.T) {
	p := putBackPane(t, "pane_put_back_act")
	id := p.Snapshot().UnsentMessage.ID
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	act := func(action string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/panes/pane_put_back_act/unsent-message", strings.NewReader(`{"id":"`+id+`","action":"`+action+`"}`))
		req.SetPathValue("id", "pane_put_back_act")
		a.actOnUnsentMessage(rec, req)
		return rec
	}
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "other-pane"} }
	t.Cleanup(func() { identifyCallerHook = prev })
	for _, action := range []string{"send", "clear"} {
		if rec := act(action); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Fatalf("%s by an agent = %d %s", action, rec.Code, rec.Body)
		}
	}
	if got := p.Snapshot().UnsentMessage; got == nil || got.ID != id {
		t.Fatal("an agent's refused request changed the put-back message")
	}
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if rec := act("clear"); strings.Contains(rec.Body.String(), "human_required") {
		t.Fatalf("the person's clear was refused as an agent's: %d %s", rec.Code, rec.Body)
	}
}

// An agent's raw keys cannot send (Enter) or clear (Ctrl+A Ctrl+K) a
// put-back message: only the person can.
func TestAgentKeysNeverActOnAPutBackMessage(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_keys")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	for _, keys := range []string{"\n", "\x01\x0b", "y"} {
		text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_put_back_keys", "text": keys}, "pane_caller"))
		if !isErr || !strings.Contains(text, "only the person sends or clears it") {
			t.Fatalf("send_input(%q) = %q (error %v)", keys, text, isErr)
		}
	}
	expectNoFrame(t, frames, "nothing typed")
}

// When the input box cannot be read, a put-back message still holds later
// messages: typing would merge into it.
func TestUnreadableBoxWithAPutBackMessageStillHolds(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_unread")
	draftGuardScreen = func(*Pane) (string, string, bool) { return "", "", false }
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_put_back_unread", "text": "please review\n"}))
	if !isErr || !strings.HasPrefix(text, "held: an unsent message from @flag-guard") {
		t.Fatalf("send_input = %q (error %v)", text, isErr)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// The HTTP inject route is held to the same rule for agents; the person's
// own input still goes through.
func TestAgentInjectNeverActsOnAPutBackMessage(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_inject")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	inject := func(text string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"text": text})
		req := httptest.NewRequest("POST", "/api/panes/pane_put_back_inject/inject", strings.NewReader(string(body)))
		req.SetPathValue("id", "pane_put_back_inject")
		a.injectPane(rec, req)
		return rec
	}
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "other-pane"} }
	for _, keys := range []string{"\r", "\x01\x0b"} {
		if rec := inject(keys); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "only the person sends or clears it") {
			t.Fatalf("agent inject %q = %d %s", keys, rec.Code, rec.Body)
		}
	}
	expectNoFrame(t, frames, "nothing typed")
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if rec := inject("\r"); rec.Code != http.StatusOK {
		t.Fatalf("the person's inject = %d %s", rec.Code, rec.Body)
	}
	expectFrame(t, frames, "the person's Enter", "\r")
}

// Nor does an agent's pane socket that takes the pane over: its Enter and
// Ctrl+A Ctrl+K are dropped and its socket stays open. The person's socket
// still sends the message.
func TestAgentSocketNeverActsOnAPutBackMessage(t *testing.T) {
	p, frames := putBackPaneFrames(t, "p1")
	m := testManagerWithPane(p)
	policy := func(r *http.Request) wsInputPolicy {
		kind := callerKind(r.Header.Get("X-Test-Caller"))
		return wsInputPolicy{human: kind == callerHuman, kind: string(kind)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/panes/{id}/io", handleWS(m, policy))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &adoptTestPane{p: p, m: m, srv: srv}

	agent := h.dial(t, callerPane, "")
	agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	agent.expectOwnership(t, true, "you")
	agent.send(t, websocket.BinaryMessage, "\r")
	agent.send(t, websocket.TextMessage, `{"type":"input","data":"\u0001\u000b"}`)
	expectNoFrame(t, frames, "nothing typed")
	if p.Snapshot().UnsentMessage == nil {
		t.Fatal("the put-back message must stay")
	}
	p.subsMu.RLock()
	_, open := p.subs[agent.subID]
	p.subsMu.RUnlock()
	if !open {
		t.Fatal("the agent's socket was closed instead of its keys dropped")
	}

	person := h.dial(t, callerHuman, "")
	person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	person.expectOwnership(t, true, "you")
	person.send(t, websocket.BinaryMessage, "\r")
	expectFrame(t, frames, "the person's Enter", "\r")
}

// The message-box route (any caller) and schedules or ARIA mentions
// (submitLineToPane) never type onto a put-back message either: it would be
// submitted with theirs.
func TestNoTypedPathSubmitsAPutBackMessage(t *testing.T) {
	p, frames := putBackPaneFrames(t, "pane_put_back_submit")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/pane_put_back_submit/submit-message", strings.NewReader(`{"text":"my own words"}`))
	req.SetPathValue("id", "pane_put_back_submit")
	a.submitPaneMessage(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unsent message from @flag-guard") {
		t.Fatalf("submit-message = %d %s", rec.Code, rec.Body)
	}
	if _, err := submitLineToPane(p, "scheduled check-in", false); !errors.Is(err, errPutBackInBox) {
		t.Fatalf("submitLineToPane err = %v, want errPutBackInBox", err)
	}
	expectNoFrame(t, frames, "nothing typed")
	if p.Snapshot().UnsentMessage == nil {
		t.Fatal("the put-back message must stay")
	}
}

// heldReason ends with the queue: a later hold for typing is not reported
// as an unsent message.
func TestHeldReasonClearsWhenTheQueueEmpties(t *testing.T) {
	p := putBackPane(t, "pane_put_back_reason")
	<-agentDelivery(p, "[from @a] first", "@a")
	if r := p.heldDeliveryReason(); !strings.HasPrefix(r, unsentInBoxPrefix) {
		t.Fatalf("reason = %q", r)
	}
	p.mu.Lock()
	held := append([]*heldDelivery(nil), p.heldDeliveries...)
	p.mu.Unlock()
	for _, h := range held {
		p.resolveHeld(h, "discarded", nil)
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.heldDeliveryReason() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("reason kept after the queue emptied: %q", p.heldDeliveryReason())
		}
		time.Sleep(time.Millisecond)
	}
}

// The person's own message box is told plainly why nothing was sent.
func TestMessageBoxExplainsAPutBackMessageToThePerson(t *testing.T) {
	p := putBackPane(t, "pane_put_back_box")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/pane_put_back_box/submit-message", strings.NewReader(`{"text":"my own words"}`))
	req.SetPathValue("id", "pane_put_back_box")
	a.submitPaneMessage(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "your input box holds an unsent message from @flag-guard") || !strings.Contains(rec.Body.String(), "Send or Clear it on the pane first") {
		t.Fatalf("submit-message = %d %s", rec.Code, rec.Body)
	}
}
