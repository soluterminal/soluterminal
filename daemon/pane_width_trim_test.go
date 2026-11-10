package main

import (
	"bytes"
	"testing"

	"soluterminal/grid"
)

// TestSubscribeTrimsStaleWidthScrollback verifies the P3 stale-width trim:
// when the retained replay tail contains bytes written at a now-stale (narrow)
// PTY width, a normal-buffer full replay drops that hard-wrapped prefix and
// keeps only same-width scrollback, while the grid repaint still makes the
// visible screen authoritative. This is the daemon-side half of the
// "narrow scrollback above, correct screen below" fix.
func TestSubscribeTrimsStaleWidthScrollback(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      120,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(120, 24),
	}
	// Output streamed while the PTY was NARROW — hard-wrapped, unreflowable.
	p.buffer.Write([]byte("NARROW-STALE-LINE\n"))
	p.buffer.Write([]byte("NARROW-STALE-LINE-2\n"))
	// The PTY then widened to 120: record the width-change mark at this offset
	// exactly as Pane.Resize would.
	p.lastWidthOff = p.buffer.TotalWritten()
	p.lastWidthCols = 120
	// Fresh content at the current (120) width. Two lines so trimReplayHead's
	// partial-leading-line drop still leaves a stable marker.
	p.buffer.Write([]byte("WIDEHEAD\nWIDEMARK_RAW\n"))
	// The visible screen the grid repaint reconstructs, at the current width.
	// CURSORROW sits on the cursor row and is intentionally skipped by the
	// non-codex paint (input-row drift guard), so assert on WIDEGRIDMARK.
	p.replayEmu.Feed([]byte("WIDEGRIDMARK\r\nCURSORROW"))

	_, snap, _, _, _, _, _, cancel := p.Subscribe(120, 24)
	defer cancel()

	if bytes.Contains(snap, []byte("NARROW-STALE")) {
		t.Fatalf("stale-width scrollback was not trimmed; snap=%q", snap)
	}
	if !bytes.Contains(snap, []byte("WIDEMARK_RAW")) {
		t.Fatalf("same-width scrollback should be preserved; snap=%q", snap)
	}
	if !bytes.Contains(snap, []byte("WIDEGRIDMARK")) {
		t.Fatalf("grid repaint (visible screen) missing; snap=%q", snap)
	}
}

// TestSubscribeKeepsTailWhenWidthUnchanged guards the common no-resize
// reattach: with no width-change recorded, currentWidthRunStart returns -1 and
// the raw tail is shipped untouched (the trim is a strict no-op).
func TestSubscribeKeepsTailWhenWidthUnchanged(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      120,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(120, 24),
	}
	p.buffer.Write([]byte("KEEPHEAD\nKEEP_RAW_LINE\n"))
	p.replayEmu.Feed([]byte("GRIDMARK\r\nCURSORROW"))
	// No width mark recorded (lastWidthCols == 0) -> no trim.

	_, snap, _, _, _, _, _, cancel := p.Subscribe(120, 24)
	defer cancel()

	if !bytes.Contains(snap, []byte("KEEP_RAW_LINE")) {
		t.Fatalf("tail wrongly trimmed when width never changed; snap=%q", snap)
	}
}

// TestFullReplayReportsItsWidth guards the replay-start `cols` the client
// writes the replay at: the PTY's width, not the narrower terminal the client
// measured mid-layout and announced (term_cols).
func TestFullReplayReportsItsWidth(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      220,
		Rows:      30,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(220, 30),
	}
	p.buffer.Write([]byte("HEAD\nWIDE_RAW_LINE\n"))
	p.replayEmu.Feed([]byte("GRIDMARK\r\nCURSORROW"))

	s := p.subscribeStream(220, 30, subModeFull, 0, subOptions{termCols: 40, termRows: 30})
	defer s.cancel()

	if s.replayCols != 220 {
		t.Fatalf("replayCols = %d, want the PTY width 220", s.replayCols)
	}
}

// TestCurrentWidthRunStart exercises the width-mark lookup directly.
func TestCurrentWidthRunStart(t *testing.T) {
	t.Parallel()
	p := &Pane{ID: "pane-test"}
	// No mark recorded yet.
	if got := p.currentWidthRunStart(120); got != -1 {
		t.Fatalf("no-mark: got %d, want -1", got)
	}
	// Latest width change is to 120 at offset 4096.
	p.lastWidthOff = 4096
	p.lastWidthCols = 120
	if got := p.currentWidthRunStart(120); got != 4096 {
		t.Fatalf("matching width: got %d, want 4096", got)
	}
	// Paint width disagrees with the last recorded width (resize in flight) ->
	// don't trim.
	if got := p.currentWidthRunStart(80); got != -1 {
		t.Fatalf("mismatched width: got %d, want -1", got)
	}
}
