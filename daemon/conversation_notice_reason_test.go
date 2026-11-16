package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConversationNoticeReasonRoundTrip locks in the conversation-notice
// contract terminal panes use when a resume could not continue the saved
// conversation (pane_spawn.go: resume-repair / fresh-start). The reason rides
// the snapshot and the wire; a notice with no reason omits the field, so older
// clients see the same JSON as before; fresh-start is informational
// (interrupted=false); an empty source kind reads as "terminal".
func TestConversationNoticeReasonRoundTrip(t *testing.T) {
	t.Parallel()
	p := &Pane{}

	p.setSwitchNoticeReason("terminal", "", true)
	n := p.switchNoticeSnapshot()
	if n == nil || !n.Interrupted || n.FromKind != "terminal" || n.Reason != "" {
		t.Fatalf("reasonless notice = %+v, want interrupted terminal with empty reason", n)
	}
	if b, _ := json.Marshal(n); strings.Contains(string(b), "reason") {
		t.Fatalf("reasonless notice JSON leaks a reason field: %s (wire back-compat broken)", b)
	}

	p.setSwitchNoticeReason(kindOrTerminal(""), "resume-repair", true)
	n = p.switchNoticeSnapshot()
	if n == nil || !n.Interrupted || n.FromKind != "terminal" || n.Reason != "resume-repair" {
		t.Fatalf("resume-repair notice = %+v", n)
	}
	if b, _ := json.Marshal(n); !strings.Contains(string(b), `"reason":"resume-repair"`) {
		t.Fatalf("resume-repair JSON = %s, want reason on the wire", b)
	}

	p.setSwitchNoticeReason("", "fresh-start", false)
	n = p.switchNoticeSnapshot()
	if n == nil || n.Interrupted || n.FromKind != "terminal" || n.Reason != "fresh-start" {
		t.Fatalf("fresh-start notice = %+v, want informational terminal fresh-start", n)
	}

	// The snapshot is a copy: changing it must not change the pane's notice.
	n.Reason = "mutated"
	if got := p.switchNoticeSnapshot(); got == nil || got.Reason != "fresh-start" {
		t.Fatalf("snapshot aliases the pending notice: %+v", got)
	}

	p.clearSwitchNotice()
	if p.switchNoticeSnapshot() != nil {
		t.Fatal("clear did not drop the notice")
	}
}
