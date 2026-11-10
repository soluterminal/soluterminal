package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newUntypedReceiptPane is a ready shell pane holding a not-typed receipt
// for text.
func newUntypedReceiptPane(t *testing.T, id, text string) (*Pane, <-chan directAutoFrameRead, *unsentPaneMessage) {
	t.Helper()
	stubSendInputBusy(t, false)
	p, frames := newTypingHoldPane(t, id)
	p.Agent, p.forceCodexForTest = "", false
	var screen atomic.Value
	screen.Store("~/demo$ ")
	stubDeliveryScreen(t, &screen)
	if !p.recordUntypedMessage("#1 Claude", text) {
		t.Fatal("receipt not kept")
	}
	return p, frames, p.Snapshot().UnsentMessage
}

func untypedSendBody(id, hash string) string {
	b, _ := json.Marshal(map[string]string{"id": id, "action": "send", "textSha256": hash})
	return string(b)
}

// refusedForReview: a 409 with the code, the review message and the
// receipt kept; nothing typed.
func refusedForReview(t *testing.T, step string, rec *httptest.ResponseRecorder, p *Pane, frames <-chan directAutoFrameRead, msg *unsentPaneMessage, code string) map[string]any {
	t.Helper()
	var got map[string]any
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["code"] != code || !strings.HasPrefix(fmt.Sprint(got["error"]), "The message changed or wasn't shown; review it again") {
		t.Fatalf("%s: %d %s", step, rec.Code, rec.Body.String())
	}
	expectNoFrame(t, frames, step+": nothing typed")
	if msg != nil && p.Snapshot().UnsentMessage != msg {
		t.Fatalf("%s: the receipt was dropped", step)
	}
	return got
}

// Send of a not-typed message types only the text the person's client showed:
// it carries that text's SHA-256, and the daemon checks it against the text
// Send would type.
func TestUntypedSendRequiresTheShownTextsHash(t *testing.T) {
	t.Run("missing hash: an older client is told to update", func(t *testing.T) {
		p, frames, msg := newUntypedReceiptPane(t, "pane_hash_missing", "make test")
		got := refusedForReview(t, "no field", postUnsentBody(t, p, `{"id":"`+msg.ID+`","action":"send"}`), p, frames, msg, "unsent_text_unconfirmed")
		if got["update"] != true || !strings.Contains(fmt.Sprint(got["error"]), "Update SoluTerminal") {
			t.Fatalf("no update hint: %v", got)
		}
		refusedForReview(t, "empty field", postUnsentBody(t, p, untypedSendBody(msg.ID, "")), p, frames, msg, "unsent_text_unconfirmed")
	})
	t.Run("wrong hash", func(t *testing.T) {
		p, frames, msg := newUntypedReceiptPane(t, "pane_hash_wrong", "make test")
		for name, hash := range map[string]string{
			"other text": unsentTextSha256("make clean"),
			"uppercase":  strings.ToUpper(unsentTextSha256("make test")),
			"trailing":   unsentTextSha256("make test "),
			"not hex":    "make test",
		} {
			got := refusedForReview(t, name, postUnsentBody(t, p, untypedSendBody(msg.ID, hash)), p, frames, msg, "unsent_text_changed")
			if _, hint := got["update"]; hint {
				t.Fatalf("%s: a current client was told to update: %v", name, got)
			}
		}
	})
	t.Run("text changed after the client fetched it", func(t *testing.T) {
		p, frames, msg := newUntypedReceiptPane(t, "pane_hash_changed", "make test")
		shown := unsentTextSha256(msg.Text)
		// The same receipt id now holds other text.
		next := *msg
		next.Text, next.body = "make clean", "make clean"
		p.mu.Lock()
		p.unsentMessage = &next
		p.mu.Unlock()
		refusedForReview(t, "same id", postUnsentBody(t, p, untypedSendBody(msg.ID, shown)), p, frames, &next, "unsent_text_changed")
		// A new receipt replaced the one that was shown.
		p.dropUnsentMessage(&next)
		if !p.recordUntypedMessage("#2 Codex", "make clean") {
			t.Fatal("second receipt not kept")
		}
		refusedForReview(t, "new receipt", postUnsentBody(t, p, untypedSendBody(msg.ID, shown)), p, frames, p.Snapshot().UnsentMessage, "unsent_text_changed")
	})
	t.Run("happy path", func(t *testing.T) {
		p, frames, msg := newUntypedReceiptPane(t, "pane_hash_ok", "make test")
		// sha256("make test"), computed independently: lowercase hex.
		if got := unsentTextSha256(msg.Text); got != "22cc66aa7d2624b4eb4d5b61658614cd6c32477b123afbf7dbc7fc3d7d0f3713" {
			t.Fatalf("sha256 = %s", got)
		}
		if rec := postUnsentBody(t, p, untypedSendBody(msg.ID, unsentTextSha256(msg.Text))); rec.Code != 200 || !strings.Contains(rec.Body.String(), "submitted") {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
		expectFrame(t, frames, "line", "make test")
		expectFrame(t, frames, "Enter", "\r")
		if p.Snapshot().UnsentMessage != nil {
			t.Fatal("receipt kept after Send")
		}
	})
	t.Run("non-ASCII text hashes its UTF-8 bytes", func(t *testing.T) {
		const text = "echo café ✓ 日本 🚀"
		p, frames, msg := newUntypedReceiptPane(t, "pane_hash_utf8", text)
		// The hash the web client's TextEncoder + SubtleCrypto produce for
		// the same text (UnsentMessageChip.test.tsx pins the same value).
		const want = "aa51c94bc53b3c46dcc127e263bca4254b8c3d2ad45654bca9094494dff6642d"
		if got := unsentTextSha256(text); got != want {
			t.Fatalf("sha256(%q) = %s, want %s", text, got, want)
		}
		if rec := postUnsentBody(t, p, untypedSendBody(msg.ID, want)); rec.Code != 200 {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
		expectFrame(t, frames, "line", text)
	})
}

// Agents never reach the hash check: Send of a not-typed message stays the
// person's, with or without a correct hash.
func TestUntypedSendHashDoesNotAdmitAgents(t *testing.T) {
	p, frames, msg := newUntypedReceiptPane(t, "pane_hash_agent", "make test")
	for _, who := range []callerKind{callerUnverified, callerWorker, callerPane} {
		for _, body := range []string{untypedSendBody(msg.ID, unsentTextSha256(msg.Text)), `{"id":"` + msg.ID + `","action":"send"}`} {
			rec := postUnsentBody(t, p, body, who)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: %d %s", who, rec.Code, rec.Body.String())
			}
		}
	}
	expectNoFrame(t, frames, "nothing typed")
	if p.Snapshot().UnsentMessage != msg {
		t.Fatal("the receipt was dropped")
	}
}

// Only Send of a not-typed message needs the hash: Discard does not.
func TestUntypedDiscardNeedsNoHash(t *testing.T) {
	p, frames, msg := newUntypedReceiptPane(t, "pane_hash_discard", "make test")
	if rec := postUnsentBody(t, p, `{"id":"`+msg.ID+`","action":"clear"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "cleared") {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
	}
	expectNoFrame(t, frames, "nothing typed")
}
