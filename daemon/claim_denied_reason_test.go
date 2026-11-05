package main

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// expectDenied waits for the refusal ack: owned:false from owner, with the
// reason the screen shows its person.
func (c *adoptTestConn) expectDenied(t *testing.T, owner, denied string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-c.ownership:
			if msg["owned"] == false && msg["denied"] != nil {
				if msg["owner"] != owner || msg["denied"] != denied {
					t.Fatalf("sub %d denial = %v, want owner=%q denied=%q", c.subID, msg, owner, denied)
				}
				return
			}
		case <-deadline:
			t.Fatalf("sub %d never got a denial (owner=%q denied=%q)", c.subID, owner, denied)
		}
	}
}

// A Type here from a socket that isn't the person's is refused as before,
// and its ack now says so: the screen can tell its person to approve it
// instead of doing nothing (ST-54). Who may claim is unchanged.
func TestClaimDenialSaysTheScreenIsNotThePersons(t *testing.T) {
	for _, kind := range []callerKind{callerUnverified, callerPane} {
		t.Run(string(kind), func(t *testing.T) {
			h := newAdoptTestPane(t, false)
			person := h.dial(t, callerHuman, "?device=dev-person")
			person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			person.expectOwnership(t, true, "you")
			screen := h.dial(t, kind, "?device=dev-screen")
			screen.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			screen.expectDenied(t, "other", claimDeniedNotPerson)
			if got := h.owner(); got != person.subID {
				t.Fatalf("owner = %d, want the person's %d", got, person.subID)
			}
		})
	}
}

// The person's own Type here still wins, with no denial on anyone's ack.
func TestPersonTakeoverIsNeverDenied(t *testing.T) {
	h := newAdoptTestPane(t, false)
	first := h.dial(t, callerHuman, "?device=dev-a")
	first.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	first.expectOwnership(t, true, "you")
	second := h.dial(t, callerHuman, "?device=dev-b")
	second.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	second.expectOwnership(t, true, "you")
	for {
		select {
		case msg := <-first.ownership:
			if msg["denied"] != nil {
				t.Fatalf("the displaced owner's frame carries a denial: %v", msg)
			}
			if msg["owned"] == false {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the displaced owner was never told")
		}
	}
}

// A refused adopt-unowned says another device is typing.
func TestAdoptDenialSaysTheOwnerIsActive(t *testing.T) {
	h := newAdoptTestPane(t, false)
	person := h.dial(t, callerHuman, "?device=dev-a")
	person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
	person.expectOwnership(t, true, "you")
	other := h.dial(t, callerHuman, "?device=dev-b")
	other.send(t, websocket.TextMessage, `{"type":"claim","reason":"adopt-unowned"}`)
	other.expectDenied(t, "other", claimDeniedOwnerActive)
	if got := h.owner(); got != person.subID {
		t.Fatalf("owner = %d, want %d", got, person.subID)
	}
}

// A view-only socket's claim is refused in every mode, and says so.
func TestViewOnlyClaimDenialSaysViewOnly(t *testing.T) {
	for _, mode := range humanProofModes {
		t.Run(mode, func(t *testing.T) {
			h0 := newAdoptTestPane(t, false)
			a := viewOnlyServer(t, mode)
			h := socketForCallers(t, a, h0.p, h0.m)
			conn := h.dial(t, callerKind(viewOnlyCallers[0].name), "")
			conn.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			conn.expectDenied(t, "none", claimDeniedViewOnly)
			if h0.owner() == conn.subID {
				t.Fatal("a view-only socket owns the pane")
			}
		})
	}
}

// The ownership frames are byte-identical to main's; a denial only adds its
// fixed reason before the closing brace (@secure's condition on ST-54).
func TestOwnershipFramesAreUnchangedApartFromTheReason(t *testing.T) {
	p := &Pane{ID: "p1", Cols: 80, Rows: 24, buffer: NewRingBuffer(1024), subs: map[int64]*subEntry{}}
	stream := p.subscribeStream(80, 24, subModeFull, 0, subOptions{})
	defer stream.cancel()
	recvCtrl(t, stream.ctrl) // initial frame
	cases := []struct {
		owned  bool
		owner  int64
		denied string
		want   string
	}{
		{true, stream.id, "", `{"type":"ownership","owned":true,"owner":"you"}`},
		{false, 99, "", `{"type":"ownership","owned":false,"owner":"other"}`},
		{false, 0, "", `{"type":"ownership","owned":false,"owner":"none"}`},
		{false, 99, claimDeniedNotPerson, `{"type":"ownership","owned":false,"owner":"other","denied":"not-person"}`},
		{false, 99, claimDeniedOwnerActive, `{"type":"ownership","owned":false,"owner":"other","denied":"owner-active"}`},
		{false, 0, claimDeniedViewOnly, `{"type":"ownership","owned":false,"owner":"none","denied":"view-only"}`},
	}
	for _, c := range cases {
		p.subsMu.Lock()
		p.ownerSubID = c.owner
		p.subsMu.Unlock()
		if c.denied == "" {
			p.sendOwnership(stream.id, c.owned)
		} else {
			p.sendOwnershipDenied(stream.id, c.denied)
		}
		if got := recvCtrl(t, stream.ctrl); got != c.want {
			t.Fatalf("frame = %s, want %s", got, c.want)
		}
	}
}
