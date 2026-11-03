package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTypingHoldPane builds a codex terminal pane wired to a net.Pipe agent, so
// the test observes exactly the frames directAutoFire writes to the PTY.
func newTypingHoldPane(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p := &Pane{
		ID:                id,
		Title:             "codex",
		Cols:              80,
		Rows:              24,
		Status:            PaneRunning,
		AutoMode:          "always",
		buffer:            NewRingBuffer(1 << 16),
		subs:              map[int64]*subEntry{},
		done:              make(chan struct{}),
		forceCodexForTest: true,
	}
	t.Cleanup(func() {
		close(p.done)
		// A put-back watch reads the test hooks: it must have ended before
		// a later test changes them.
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mu.Lock()
			watching := p.putBackWatching
			p.mu.Unlock()
			if !watching {
				break
			}
			if time.Now().After(deadline) {
				t.Error("put-back watch did not end")
				break
			}
			time.Sleep(time.Millisecond)
		}
		p.shutdownGrid()
	})

	raw := strings.Repeat("   •      \n", 12)
	if _, err := p.buffer.Write([]byte(raw)); err != nil {
		t.Fatalf("buffer.Write: %v", err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(raw)))

	paneEnd, agentEnd := net.Pipe()
	p.agentConn = paneEnd
	t.Cleanup(func() { paneEnd.Close(); agentEnd.Close() })
	frames := make(chan directAutoFrameRead, 8)
	go func() {
		for {
			type_, payload, err := ReadFrame(agentEnd)
			frames <- directAutoFrameRead{type_: type_, payload: payload, err: err}
			if err != nil {
				return
			}
		}
	}()
	return p, frames
}

func expectFrame(t *testing.T, frames <-chan directAutoFrameRead, step, want string) {
	t.Helper()
	select {
	case frame := <-frames:
		if frame.err != nil {
			t.Fatalf("%s frame read: %v", step, frame.err)
		}
		if got := string(frame.payload); got != want {
			t.Fatalf("%s payload = %q, want %q", step, got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s timed out waiting for %q", step, want)
	}
}

func expectNoFrame(t *testing.T, frames <-chan directAutoFrameRead, step string) {
	t.Helper()
	select {
	case frame := <-frames:
		t.Fatalf("%s unexpected frame: type=%v payload=%q err=%v", step, frame.type_, frame.payload, frame.err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDirectAutoFireHoldsWhileUserIsTyping is the regression for the live
// "auto-approve submitted my half-typed message" report: the user is part-way
// through a line in the pane when an approval menu is admitted, and the bare
// Enter directAutoFire writes lands in their composer.
//
// The typing goes through the real Pane.Write, so LastInputMs and the prompt
// recorder are populated by production bookkeeping, not by the test.
func TestDirectAutoFireHoldsWhileUserIsTyping(t *testing.T) {
	t.Parallel()
	p, frames := newTypingHoldPane(t, "pane-typing-hold")
	a := testAriaWithPane(t, p)
	a.sweepStreaks = map[string]*sweepStreak{}
	a.evalLog = newAriaEvalLog()

	if _, err := p.Write([]byte("half typed reply")); err != nil {
		t.Fatalf("Pane.Write typing: %v", err)
	}
	expectFrame(t, frames, "user keystrokes", "half typed reply")
	if p.composerPendingLen() == 0 {
		t.Fatal("prompt recorder buffer empty after typing; the composer signal would be vacuous")
	}

	// Menu arrives AFTER the keystrokes, so it is genuinely unanswered: the
	// pane really is in choice-prompt state and every pre-existing guard in
	// directAutoFire passes. Only the typing hold can stop the write.
	seedGridMenu(t, p, codexMcpPermissionPromptFor("get_workflow_task", "wft_typing01"))
	if got := p.computeState(); got != StateChoicePrompt {
		t.Fatalf("computeState = %q, want %q", got, StateChoicePrompt)
	}
	if _, _, _, ok := currentChoicePromptIdentity(p, ""); !ok {
		t.Fatal("no stable prompt identity; the test would pass for the wrong reason")
	}

	a.directAutoFire(context.Background(), p.ID, p.Title)
	expectNoFrame(t, frames, "auto-fire while user is typing")

	// A hold is not a decline: parking the pane would strand a menu that is
	// still pending once the user stops typing.
	if got := a.BackoffStreak(p.ID); got != 0 {
		t.Fatalf("BackoffStreak = %d after a hold, want 0 (hold must not bump the suspend streak)", got)
	}
}

// TestDirectAutoFireFiresOnceTypingSettles pins the other half: the hold
// releases, so auto-approve is deferred, not disabled.
func TestDirectAutoFireFiresOnceTypingSettles(t *testing.T) {
	t.Parallel()
	p, frames := newTypingHoldPane(t, "pane-typing-settled")
	a := testAriaWithPane(t, p)
	a.sweepStreaks = map[string]*sweepStreak{}
	a.evalLog = newAriaEvalLog()

	// The user typed, then submitted: the recorder buffer drains on Enter.
	if _, err := p.Write([]byte("an earlier message\r")); err != nil {
		t.Fatalf("Pane.Write typing: %v", err)
	}
	expectFrame(t, frames, "user keystrokes", "an earlier message\r")
	if p.composerPendingLen() != 0 {
		t.Fatalf("composerPendingLen = %d after submit, want 0", p.composerPendingLen())
	}
	// ...and then went quiet, past the settle window.
	atomic.StoreInt64(&p.LastInputMs, time.Now().Add(-10*ariaAutoTypingSettle).UnixMilli())

	seedGridMenu(t, p, codexMcpPermissionPromptFor("get_workflow_task", "wft_typing02"))
	if got := p.computeState(); got != StateChoicePrompt {
		t.Fatalf("computeState = %q, want %q", got, StateChoicePrompt)
	}

	a.directAutoFire(context.Background(), p.ID, p.Title)
	expectFrame(t, frames, "auto-fire after typing settled", "\r")
}

func TestAutoFireUserHold(t *testing.T) {
	t.Parallel()
	now := time.Now()

	if hold, _ := autoFireUserHold(nil, now); hold {
		t.Fatal("nil pane must not hold")
	}

	quiet := &Pane{ID: "quiet", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&quiet.LastInputMs, now.Add(-time.Hour).UnixMilli())
	if hold, reason := autoFireUserHold(quiet, now); hold {
		t.Fatalf("quiet terminal pane held: %q", reason)
	}

	typing := &Pane{ID: "typing", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&typing.LastInputMs, now.Add(-time.Second).UnixMilli())
	hold, reason := autoFireUserHold(typing, now)
	if !hold || reason != "user typing" {
		t.Fatalf("recent keystroke: hold=%v reason=%q, want true/\"user typing\"", hold, reason)
	}

	// The daemon's own auto-fire also lands on LastInputMs. It must not hold
	// off the next fire, or back-to-back permission modals stall.
	ownFire := &Pane{ID: "own-fire", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&ownFire.LastInputMs, now.Add(-10*time.Millisecond).UnixMilli())
	ownFire.AutoModeLastFiredMs = now.Add(-9 * time.Millisecond).UnixMilli()
	if hold, reason := autoFireUserHold(ownFire, now); hold {
		t.Fatalf("auto-fire's own write held the next fire: %q", reason)
	}

	// Stale keystroke, but the line was never submitted.
	draft := &Pane{ID: "draft", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&draft.LastInputMs, now.Add(-time.Minute).UnixMilli())
	draft.promptRec.buf = []byte("unsent draft")
	draft.promptRec.lastRecordedInputMs = now.Add(-time.Minute).UnixMilli()
	hold, reason = autoFireUserHold(draft, now)
	if !hold || !strings.HasPrefix(reason, "composer has text (") {
		t.Fatalf("stale draft: hold=%v reason=%q, want true/\"composer has text\"", hold, reason)
	}

	// Abandoned long enough that the draft can no longer hold auto-approval off.
	// A phantom draft (Esc, history recall: the recorder cannot see those) must
	// not park an approval indefinitely.
	abandoned := &Pane{ID: "abandoned", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&abandoned.LastInputMs, now.Add(-10*time.Millisecond).UnixMilli())
	abandoned.AutoModeLastFiredMs = now.Add(-9 * time.Millisecond).UnixMilli()
	abandoned.promptRec.buf = []byte("forgotten draft")
	abandoned.promptRec.lastRecordedInputMs = now.Add(-2 * ariaAutoDraftStaleAfter).UnixMilli()
	if hold, reason := autoFireUserHold(abandoned, now); hold {
		t.Fatalf("abandoned draft held: %q", reason)
	}

	// A draft typed before the daemon's own last fire is still the user's: the
	// recorder buffer is filled only by keystrokes, so the ours-vs-theirs test
	// must not suppress the draft check.
	draftBeforeFire := &Pane{ID: "draft-before-fire", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&draftBeforeFire.LastInputMs, now.Add(-30*time.Second).UnixMilli())
	draftBeforeFire.AutoModeLastFiredMs = now.Add(-10 * time.Second).UnixMilli()
	draftBeforeFire.promptRec.buf = []byte("unsent draft")
	draftBeforeFire.promptRec.lastRecordedInputMs = now.Add(-30 * time.Second).UnixMilli()
	hold, reason = autoFireUserHold(draftBeforeFire, now)
	if !hold || !strings.HasPrefix(reason, "composer has text (") {
		t.Fatalf("draft older than the last auto-fire: hold=%v reason=%q, want true/\"composer has text\"", hold, reason)
	}
}

// TestDirectAutoFireNativeDoesNotHoldOnComposer pins the deliberate asymmetry:
// a native resolve is a control_response frame bound to a RequestID, so it
// cannot take a draft the user is holding, and must not be held for one.
func TestAutoFireUserHoldIgnoresNativeComposerLease(t *testing.T) {
	t.Parallel()
	now := time.Now()
	native := &Pane{ID: "native", Kind: "native", subs: map[int64]*subEntry{}}
	atomic.StoreInt64(&native.LastInputMs, now.Add(-time.Hour).UnixMilli())
	native.SetComposerNonEmpty(true, now)
	if hold, reason := autoFireUserHold(native, now); hold {
		t.Fatalf("native composer lease held a resolve that cannot steal a draft: %q", reason)
	}
}

// A split terminal reply used to leak its printable tail into promptRec.buf,
// holding auto-approval for two minutes despite no user draft. Exercise the
// actual successful-write bookkeeping and keep the real-draft protection.
func TestAutoFireSplitTerminalRepliesDoNotCreateDraft(t *testing.T) {
	for _, reply := range []string{"\x1b]10;rgb:aaaa/bbbb/cccc\x1b\\", "\x1b[12;34R", "\x1b[?1;2c"} {
		for split := 1; split < len(reply); split++ {
			for _, draft := range []string{"", "unsent real draft"} {
				p := &Pane{ID: "split-reply", subs: map[int64]*subEntry{}}
				p.agentWriteMu.Lock()
				if draft != "" {
					p.observeSuccessfulInputLocked([]byte(draft), true, menuInputNone)
				}
				p.observeSuccessfulInputLocked([]byte(reply[:split]), true, menuInputNone)
				p.observeSuccessfulInputLocked([]byte(reply[split:]), true, menuInputNone)
				p.agentWriteMu.Unlock()
				// Fragmented protocol bytes conservatively count as recent input; the
				// short typing-settle protection is intentionally unchanged.
				now := time.Now().Add(ariaAutoTypingSettle + time.Second)
				hold, reason := autoFireUserHold(p, now)
				if hold != (draft != "") {
					t.Fatalf("reply %q split %d draft=%q: held=%v reason=%q", reply, split, draft, hold, reason)
				}
			}
		}
	}
}
