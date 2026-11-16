package main

import (
	"errors"
	"testing"
	"time"
)

// Tracker #35 (ST-119): a prompt schedule typed into a pane while the person
// was typing there, and its Enter submitted their half-written prompt with
// it. The daemon's own deliveries (schedules, ARIA mentions) now refuse,
// nothing typed, while the person is typing or their input box has text.

func TestDaemonDeliveryIsNotTypedWhileThePersonIsTyping(t *testing.T) {
	p, frames, _ := newDraftGuardPane(t, "daemon-typing")
	p.noteHumanInput(time.Now())
	_, err := submitLineToPane(p, "scheduled check-in", false)
	if !errors.Is(err, errDraftGuardHeld) || draftHeldReason(err) != "the person is typing" {
		t.Fatalf("err = %v, want held: the person is typing", err)
	}
	expectNoFrame(t, frames, "nothing typed while the person types")
	if held := p.Snapshot().HeldMessages; len(held) != 0 {
		t.Fatalf("a daemon delivery is not queued on the pane: %+v", held)
	}
}

func TestDaemonDeliveryIsNotTypedOntoADraft(t *testing.T) {
	p, frames, screen := newDraftGuardPane(t, "daemon-draft")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	screen.Store(deliveryCodexScreen("I want the release updates and inf"))
	_, err := submitLineToPane(p, "scheduled check-in", false)
	if !errors.Is(err, errDraftGuardHeld) || draftHeldReason(err) != heldBoxTextReason {
		t.Fatalf("err = %v, want held: %s", err, heldBoxTextReason)
	}
	expectNoFrame(t, frames, "nothing typed onto the draft")
}

func TestDaemonDeliveryGoesInWhenThePersonIsIdle(t *testing.T) {
	p, frames, _ := newDraftGuardPane(t, "daemon-idle")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	done := make(chan error, 1)
	go func() {
		_, err := submitLineToPane(p, "scheduled check-in", false)
		done <- err
	}()
	expectFrame(t, frames, "body", "\x1b[200~scheduled check-in\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
}

// A shell (or any program whose input line is not read off the screen):
// keystroke recency and the prompt recorder's draft decide.
func TestDaemonDeliveryHoldsInAShellWhileThePersonTypes(t *testing.T) {
	p, frames := newTypingHoldPane(t, "daemon-shell")
	p.forceCodexForTest = false
	if agent, _ := paneAgentProvider(p); agent {
		t.Fatal("test pane must be a plain terminal")
	}
	if _, err := p.Write([]byte("git commit -m 'half")); err != nil {
		t.Fatal(err)
	}
	<-frames
	p.noteHumanInput(time.Now())
	if _, err := submitLineToPane(p, "echo scheduled", false); !errors.Is(err, errDraftGuardHeld) || draftHeldReason(err) != "the person is typing" {
		t.Fatalf("typing: err = %v", err)
	}
	expectNoFrame(t, frames, "nothing typed while the person types")
	// Quiet, but the line they typed is still unsent.
	p.noteHumanInput(time.Now().Add(-time.Minute))
	if _, err := submitLineToPane(p, "echo scheduled", false); !errors.Is(err, errDraftGuardHeld) || draftHeldReason(err) != heldBoxTextReason {
		t.Fatalf("draft: err = %v", err)
	}
	expectNoFrame(t, frames, "nothing typed onto the unsent line")
}

// A shell has no screen to check: a line the person left unsent long ago
// still holds (review: the recorder's 2-minute staleness matched the wait,
// so a waiting fire typed onto it in the end).
func TestDaemonDeliveryHoldsOnAnOldShellDraft(t *testing.T) {
	p, frames := newTypingHoldPane(t, "daemon-shell-old")
	p.forceCodexForTest = false
	if _, err := p.Write([]byte("rm -rf build/")); err != nil {
		t.Fatal(err)
	}
	<-frames
	p.agentWriteMu.Lock()
	p.promptRec.lastRecordedInputMs = time.Now().Add(-time.Hour).UnixMilli()
	p.agentWriteMu.Unlock()
	p.noteHumanInput(time.Now().Add(-time.Hour))
	if _, err := submitLineToPane(p, "echo scheduled", false); !errors.Is(err, errDraftGuardHeld) || draftHeldReason(err) != heldBoxTextReason {
		t.Fatalf("err = %v, want held on the old draft", err)
	}
	expectNoFrame(t, frames, "nothing typed onto the unsent line")
}

// In a shell, input that arrives between the paste and Enter keeps the
// Enter from being pressed: it would run the person's keys with the line.
func TestDaemonDeliveryInAShellPressesNoEnterAfterInput(t *testing.T) {
	p, frames := newTypingHoldPane(t, "daemon-shell-enter")
	p.forceCodexForTest = false
	prev := enterGapBracketedPaste
	enterGapBracketedPaste = 300 * time.Millisecond
	t.Cleanup(func() { enterGapBracketedPaste = prev })
	done := make(chan error, 1)
	go func() {
		_, err := submitLineToPane(p, "echo scheduled", false)
		done <- err
	}()
	expectFrame(t, frames, "paste", "\x1b[200~echo scheduled\x1b[201~")
	p.inputRevision.Add(1) // a keystroke lands in the gap
	if err := <-done; !errors.Is(err, errDeliveryLeftInComposer) {
		t.Fatalf("err = %v, want left unsubmitted", err)
	}
	expectNoFrame(t, frames, "no Enter")
}
