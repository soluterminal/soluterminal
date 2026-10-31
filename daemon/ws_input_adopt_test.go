package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Keystroke-driven adoption of an UNOWNED pane (AdoptOnInput via handleWS).
// After a relayed reconnect every pane is unowned; a visible human viewer that
// types must become the owner and have its bytes written, while every other
// ownership rule (live owners, agents, the prompt gate) stays as it was.

type adoptTestPane struct {
	p      *Pane
	srv    *httptest.Server
	frames chan []byte // payloads the pane wrote to its agent (the PTY side)
}

// newAdoptTestPane wires a pane to a net.Pipe agent so tests observe exactly
// the bytes Pane.Write delivers. The socket's caller class comes from the
// test-only X-Test-Caller header: the production policy derives it from
// identifyCaller, never from anything the client controls.
func newAdoptTestPane(t *testing.T, promptHeld bool) *adoptTestPane {
	t.Helper()
	p := &Pane{
		ID:     "p1",
		Title:  "bash",
		Cols:   80,
		Rows:   24,
		Status: PaneRunning,
		buffer: NewRingBuffer(1 << 16),
		subs:   map[int64]*subEntry{},
		done:   make(chan struct{}),
	}
	paneEnd, agentEnd := net.Pipe()
	p.agentConn = paneEnd
	frames := make(chan []byte, 32)
	go func() {
		for {
			_, payload, err := ReadFrame(agentEnd)
			if err != nil {
				return
			}
			frames <- payload
		}
	}()
	m := &Manager{panes: map[string]*Pane{"p1": p}}
	policy := func(r *http.Request) wsInputPolicy {
		kind := callerKind(r.Header.Get("X-Test-Caller"))
		allowed := func(*Pane) bool { return true }
		if kind == callerPane && promptHeld {
			allowed = func(*Pane) bool { return false }
		}
		return wsInputPolicy{allowed: allowed, human: kind == callerHuman, kind: string(kind)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/panes/{id}/io", handleWS(m, policy))
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		paneEnd.Close()
		agentEnd.Close()
		close(p.done)
		p.shutdownGrid()
	})
	return &adoptTestPane{p: p, srv: srv, frames: frames}
}

type adoptTestConn struct {
	conn      *websocket.Conn
	subID     int64
	ownership chan map[string]any
}

func (h *adoptTestPane) dial(t *testing.T, kind callerKind, query string) *adoptTestConn {
	t.Helper()
	before := map[int64]bool{}
	h.p.subsMu.RLock()
	for id := range h.p.subs {
		before[id] = true
	}
	h.p.subsMu.RUnlock()
	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/panes/p1/io" + query
	header := http.Header{"Origin": []string{h.srv.URL}, "X-Test-Caller": []string{string(kind)}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &adoptTestConn{conn: conn, ownership: make(chan map[string]any, 32)}
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(data, &msg) == nil && msg["type"] == "ownership" {
				c.ownership <- msg
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for c.subID == 0 {
		h.p.subsMu.RLock()
		for id := range h.p.subs {
			if !before[id] {
				c.subID = id
			}
		}
		h.p.subsMu.RUnlock()
		if c.subID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new subscriber never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

func (c *adoptTestConn) send(t *testing.T, mt int, data string) {
	t.Helper()
	if err := c.conn.WriteMessage(mt, []byte(data)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *adoptTestConn) expectOwnership(t *testing.T, owned bool, owner string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-c.ownership:
			if msg["owned"] == owned && msg["owner"] == owner {
				return
			}
		case <-deadline:
			t.Fatalf("sub %d never got ownership owned=%v owner=%q", c.subID, owned, owner)
		}
	}
}

func (h *adoptTestPane) owner() int64 {
	h.p.subsMu.RLock()
	defer h.p.subsMu.RUnlock()
	return h.p.ownerSubID
}

func (h *adoptTestPane) expectWrite(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.frames:
		if string(got) != want {
			t.Fatalf("pane wrote %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pane never wrote %q", want)
	}
}

func (h *adoptTestPane) expectNoWrite(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.frames:
		t.Fatalf("pane unexpectedly wrote %q", got)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestWSHumanTypingAdoptsUnownedPane(t *testing.T) {
	h := newAdoptTestPane(t, false)
	viewer := h.dial(t, callerHuman, "")
	if h.owner() != 0 {
		t.Fatalf("pane owned before any claim: %d", h.owner())
	}
	viewer.send(t, websocket.BinaryMessage, "ls\r")
	h.expectWrite(t, "ls\r")
	if got := h.owner(); got != viewer.subID {
		t.Fatalf("owner = %d, want typing viewer %d", got, viewer.subID)
	}
	viewer.expectOwnership(t, true, "you")
	// The JSON "input" path adopts the same way once owned — and keeps writing.
	viewer.send(t, websocket.TextMessage, `{"type":"input","data":"pwd\r"}`)
	h.expectWrite(t, "pwd\r")
}

func TestWSHumanTypingNeverStealsFromLiveOwner(t *testing.T) {
	logs := captureLog(t)
	h := newAdoptTestPane(t, false)
	owner := h.dial(t, callerHuman, "")
	owner.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	owner.expectOwnership(t, true, "you")
	other := h.dial(t, callerHuman, "")
	other.send(t, websocket.BinaryMessage, "hunter2\r")
	other.send(t, websocket.TextMessage, `{"type":"input","data":"hunter2\r"}`)
	h.expectNoWrite(t)
	if got := h.owner(); got != owner.subID {
		t.Fatalf("owner = %d, want original owner %d", got, owner.subID)
	}
	other.expectOwnership(t, false, "other")
	out := logs.String()
	if !strings.Contains(out, "input: dropped keystrokes on pane p1") || !strings.Contains(out, "reason=owned-by-other") {
		t.Fatalf("drop not logged: %q", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("drop log leaked keystroke content: %q", out)
	}
	if n := strings.Count(out, "input: dropped keystrokes"); n != 1 {
		t.Fatalf("drop logged %d times for two frames; want rate-limited to 1", n)
	}
}

// A takeover owner that is hidden and idle still holds the pane: the
// keystroke adoption only ever fills an empty owner slot.
func TestWSHumanTypingKeepsHiddenIdleOwnersHold(t *testing.T) {
	h := newAdoptTestPane(t, false)
	owner := h.dial(t, callerHuman, "")
	owner.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	owner.expectOwnership(t, true, "you")
	owner.send(t, websocket.TextMessage, `{"type":"visibility","visible":false}`)
	other := h.dial(t, callerHuman, "")
	other.send(t, websocket.BinaryMessage, "x")
	h.expectNoWrite(t)
	if got := h.owner(); got != owner.subID {
		t.Fatalf("owner = %d, want hidden owner %d kept", got, owner.subID)
	}
}

func TestWSHiddenHumanViewerDoesNotAdopt(t *testing.T) {
	h := newAdoptTestPane(t, false)
	viewer := h.dial(t, callerHuman, "?visible=0")
	viewer.send(t, websocket.BinaryMessage, "x")
	h.expectNoWrite(t)
	if got := h.owner(); got != 0 {
		t.Fatalf("hidden viewer adopted the pane (owner %d)", got)
	}
	viewer.expectOwnership(t, false, "none")
}

func TestWSNonHumanSocketsNeverAdopt(t *testing.T) {
	for _, kind := range []callerKind{callerPane, callerWorker, callerUnverified, ""} {
		t.Run(string(kind), func(t *testing.T) {
			h := newAdoptTestPane(t, false)
			c := h.dial(t, kind, "")
			c.send(t, websocket.BinaryMessage, "rm -rf x\r")
			c.send(t, websocket.TextMessage, `{"type":"input","data":"y"}`)
			h.expectNoWrite(t)
			if got := h.owner(); got != 0 {
				t.Fatalf("%q socket adopted the pane (owner %d)", kind, got)
			}
		})
	}
}

// An agent's socket is still governed only by the explicit claim plus
// promptInputGate: typing never adopts, and a held prompt still holds.
func TestWSAgentSocketStillGovernedByPromptGate(t *testing.T) {
	h := newAdoptTestPane(t, true)
	agent := h.dial(t, callerPane, "")
	agent.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	agent.expectOwnership(t, true, "you")
	agent.send(t, websocket.BinaryMessage, "1")
	h.expectNoWrite(t)

	h2 := newAdoptTestPane(t, false)
	agent2 := h2.dial(t, callerPane, "")
	agent2.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	agent2.expectOwnership(t, true, "you")
	agent2.send(t, websocket.BinaryMessage, "ok")
	h2.expectWrite(t, "ok")
}

// Two humans typing into the same unowned pane at once: exactly one adopts
// it and only that one's bytes reach the PTY; the other is told "other".
func TestWSTwoHumansTypingRaceOneWins(t *testing.T) {
	for i := 0; i < 10; i++ {
		h := newAdoptTestPane(t, false)
		a := h.dial(t, callerHuman, "")
		b := h.dial(t, callerHuman, "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = a.conn.WriteMessage(websocket.BinaryMessage, []byte("A")) }()
		go func() { defer wg.Done(); _ = b.conn.WriteMessage(websocket.BinaryMessage, []byte("B")) }()
		wg.Wait()
		var got []string
		timeout := time.After(250 * time.Millisecond)
	collect:
		for {
			select {
			case f := <-h.frames:
				got = append(got, string(f))
			case <-timeout:
				break collect
			}
		}
		owner := h.owner()
		var winner, loser *adoptTestConn
		switch owner {
		case a.subID:
			winner, loser = a, b
		case b.subID:
			winner, loser = b, a
		default:
			t.Fatalf("iteration %d: owner %d is neither viewer (%d, %d)", i, owner, a.subID, b.subID)
		}
		want := "A"
		if winner == b {
			want = "B"
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("iteration %d: pane wrote %q, want only the winner's %q", i, got, want)
		}
		loser.expectOwnership(t, false, "other")
	}
}

// Terminal machinery (a DA reply, a focus report) is not a person typing:
// it never adopts an unowned pane, and the next real keypress does.
func TestWSTerminalReplyDoesNotAdoptUnownedPane(t *testing.T) {
	h := newAdoptTestPane(t, false)
	viewer := h.dial(t, callerHuman, "")
	viewer.send(t, websocket.BinaryMessage, "\x1b[?1;2c")
	viewer.send(t, websocket.BinaryMessage, "\x1b[I")
	h.expectNoWrite(t)
	if got := h.owner(); got != 0 {
		t.Fatalf("terminal reply adopted the pane (owner %d)", got)
	}
	viewer.send(t, websocket.BinaryMessage, "\x1b")
	h.expectWrite(t, "\x1b")
	if got := h.owner(); got != viewer.subID {
		t.Fatalf("lone ESC keypress did not adopt (owner %d)", got)
	}
}

// After a reconnect the first input may well be an arrow key, an Alt combo or
// a whole bracketed paste: each adopts the unowned pane and is written.
func TestWSUserEscapeSequencesAdoptUnownedPane(t *testing.T) {
	for name, input := range map[string]string{
		"arrow":           "\x1b[A",
		"ss3 arrow":       "\x1bOB",
		"alt-b":           "\x1bb",
		"bracketed paste": "\x1b[200~echo pasted\r\x1b[201~",
		"f5":              "\x1b[15~",
	} {
		t.Run(name, func(t *testing.T) {
			h := newAdoptTestPane(t, false)
			viewer := h.dial(t, callerHuman, "")
			viewer.send(t, websocket.BinaryMessage, input)
			h.expectWrite(t, input)
			if got := h.owner(); got != viewer.subID {
				t.Fatalf("%s did not adopt (owner %d)", name, got)
			}
		})
	}
}

func TestIsTerminalReply(t *testing.T) {
	replies := []string{"\x1b[?1;2c", "\x1b[>0;276;0c", "\x1b[12;40R", "\x1b[0n", "\x1b[I", "\x1b[O",
		"\x1b[<0;10;5M", "\x1b[M !!", "\x1b]11;rgb:0000/0000/0000\x1b\\", "\x1bP1$r0m\x1b\\", "\x1b[?2004;1$y", "\x1b[8;24;80t"}
	for _, r := range replies {
		if !isTerminalReply([]byte(r)) {
			t.Errorf("%q should be a terminal reply", r)
		}
	}
	keys := []string{"a", "\x1b", "\x1b[A", "\x1bOA", "\x1bx", "\x1b[200~x\x1b[201~", "\x1b[3~", "\x1b[1;5C", "\x1b[Z", "\x1b[c"}
	for _, k := range keys {
		if isTerminalReply([]byte(k)) {
			t.Errorf("%q should be a user key", k)
		}
	}
}
