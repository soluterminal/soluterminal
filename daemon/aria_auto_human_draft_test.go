package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// queueAgentMessage has an agent's ask_pane message queued into a working
// Codex (bracketed paste, then Tab), as @flag-guard's was into @rest-gate's
// pane on 2026-11-02. Codex keeps queued text out of its input box, but the
// prompt recorder still holds it: Tab is not Enter.
func queueAgentMessage(t *testing.T, id string) *Pane {
	t.Helper()
	p, _ := queueAgentMessageFrames(t, id)
	return p
}

func queueAgentMessageFrames(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames, _ := queueAgentMessageScreen(t, id)
	return p, frames
}

func queueAgentMessageScreen(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead, *atomic.Value) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	p.Cols, p.Rows = 160, 48
	var screen, guard atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	// The draft guard reads the box with Codex's faint placeholder blanked.
	guard.Store(strings.Replace(codex0166Screen(t, "working"), "› Ask Codex to do anything", "›", 1))
	stubDraftGuard(t, p, &guard)
	ended := stubQueueWatch(t)
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := deliverAgentMessage(p, codexProbeMessage, false, deliveryNoSteer, "@flag-guard")
		done <- outcome
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	screen.Store(codex0166Screen(t, "queued"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	t.Cleanup(func() {
		// Sent, or put back into the box: either way the watch ends.
		screen.Store(codex0166Screen(t, "idle"))
		select {
		case <-ended:
		case <-time.After(2 * time.Second):
			t.Error("the queued-message watch did not end")
		}
	})
	if n, _ := p.composerPendingSnapshot(); n == 0 {
		t.Fatal("precondition: the recorder should still hold the queued message's text")
	}
	return p, frames, &screen
}

func TestAgentQueuedTextDoesNotHoldAutoApproval(t *testing.T) {
	p := queueAgentMessage(t, "auto-agent-queued")
	// Right after the delivery: its keystrokes are not a person typing.
	if hold, reason := autoFireUserHold(p, time.Now()); hold {
		t.Fatalf("held right after an agent's delivery: %q", reason)
	}
	if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); hold {
		t.Fatalf("held for an agent's queued message: %q", reason)
	}
}

func TestHumanDraftOverAgentTextHoldsAutoApproval(t *testing.T) {
	p := queueAgentMessage(t, "auto-agent-then-human")
	if _, err := p.Write([]byte("and also")); err != nil {
		t.Fatal(err)
	}
	if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); !hold || !strings.Contains(reason, "composer has text") {
		t.Fatalf("a person's typing after an agent's text = %v %q, want a draft hold", hold, reason)
	}
}

func TestHumanTypedDraftHoldsAutoApproval(t *testing.T) {
	p, _ := newTypingHoldPane(t, "auto-human-draft")
	if _, err := p.Write([]byte("half typed reply")); err != nil {
		t.Fatal(err)
	}
	if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); !hold || !strings.Contains(reason, "composer has text") {
		t.Fatalf("hold = %v %q, want a draft hold", hold, reason)
	}
}

func TestHumanTypingWithinTheSettleHoldsAutoApproval(t *testing.T) {
	p, _ := newTypingHoldPane(t, "auto-human-typing")
	now := time.Now()
	p.noteHumanInput(now.Add(-2 * time.Second))
	if hold, reason := autoFireUserHold(p, now); !hold || reason != "user typing" {
		t.Fatalf("hold = %v %q, want user typing", hold, reason)
	}
	if hold, reason := autoFireUserHold(p, now.Add(2*time.Second)); hold {
		t.Fatalf("still held after the settle with no draft: %q", reason)
	}
}

// A person answering a menu by hand does not make an agent's queued text
// their draft: menu keys leave the composer draft, and its owner, as it was.
func TestMenuAnsweredByHandKeepsAgentTextAgentOwned(t *testing.T) {
	for name, steps := range map[string][]menuStep{
		"digit":            {{"1", ""}},
		"arrow then enter": {{"\x1b[B", questionFocused(2, "")}, {"\r", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			p, frames := queueAgentMessageFrames(t, "auto-agent-menu-"+strings.ReplaceAll(name, " ", "-"))
			seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
			if p.computeState() != StateChoicePrompt {
				t.Fatal("fixture is not a menu on screen")
			}
			typeMenuSteps(t, p, frames, steps)
			if n, _ := p.composerPendingSnapshot(); n == 0 {
				t.Fatal("precondition: the agent's queued text should still be in the recorder")
			}
			if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); hold {
				t.Fatalf("held after a hand-answered menu: %q", reason)
			}
		})
	}
}

// An interrupted Codex turn puts the queued agent message back into the
// input box (testdata/codex_0166/restored.txt). It is shown as an unsent
// message and left there, but it is not a person's draft: it does not hold
// auto-approval.
func TestCodexPutBackOfAQueuedAgentMessageDoesNotHoldAutoApproval(t *testing.T) {
	p, frames, screen := queueAgentMessageScreen(t, "auto-agent-put-back")
	// The person presses Esc to stop the turn; Codex puts the queued
	// message back into the box.
	if _, err := p.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "the person's Esc", "\x1b")
	screen.Store(codex0166Screen(t, "restored"))
	deadline := time.Now().Add(2 * time.Second)
	for p.Snapshot().UnsentMessage == nil {
		if time.Now().After(deadline) {
			t.Fatal("the put-back message was not recorded as unsent")
		}
		time.Sleep(time.Millisecond)
	}
	if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); hold {
		t.Fatalf("held for an agent's put-back message: %q", reason)
	}
	if n, _ := p.composerPendingSnapshot(); n == 0 {
		t.Fatal("the put-back text must be left as it is")
	}
}

// Keys that leave the draft as it is (arrows, page keys, Esc) keep an
// agent's text agent-owned; typing a character does not.
func TestKeysThatLeaveTheDraftKeepAgentOwnership(t *testing.T) {
	p, frames := queueAgentMessageFrames(t, "auto-agent-arrows")
	for _, key := range []string{"\x1b[D", "\x1b[A", "\x1b[5~", "\x1b"} {
		if _, err := p.Write([]byte(key)); err != nil {
			t.Fatal(err)
		}
		expectFrame(t, frames, "the person's key", key)
		if hold, reason := autoFireUserHold(p, time.Now().Add(10*time.Second)); hold {
			t.Fatalf("held after %q over an agent's queued text: %q", key, reason)
		}
	}
	if _, err := p.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "the person's character", "x")
	if hold, _ := autoFireUserHold(p, time.Now().Add(10*time.Second)); !hold {
		t.Fatal("a character the person typed must make the draft theirs")
	}
}
