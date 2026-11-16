package main

import (
	"testing"
	"time"

	"soluterminal/grid"
)

func mkSizingSub(cols, rows uint16, visible, mobile bool, seq int64) *subEntry {
	return &subEntry{
		cols:          cols,
		rows:          rows,
		visible:       visible,
		isMobile:      mobile,
		lastActiveSeq: seq,
		lastSeenAt:    time.Now(),
	}
}

func paneWithSizingSubs(subs ...*subEntry) *Pane {
	p := &Pane{subs: map[int64]*subEntry{}}
	for i, s := range subs {
		p.subs[int64(i+1)] = s
	}
	return p
}

// A background grid tile (hidden, narrow, even with a NEWER active stamp) must
// not pin the shared PTY narrow while a visible wide desktop is the on-screen
// viewer. This is the core "full-screen pane stuck at 65" defect.
func TestDesiredSizeHiddenNarrowDoesNotBeatVisibleWide(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(190, 50, true, false, 1), // visible wide desktop, older stamp
		mkSizingSub(65, 29, false, false, 9), // hidden narrow tile, newer stamp
	)
	if c, r := p.desiredSizeLocked(); c != 190 || r != 50 {
		t.Fatalf("got %dx%d, want 190x50 (visible wide must beat hidden narrow)", c, r)
	}
}

// When the only visible viewer is mid-layout (cols==0) and a hidden narrow sub
// has dims, HOLD (0,0 = leave PTY unchanged) rather than adopting the hidden
// narrow width — that adoption is how a parked tile shrank an active pane.
func TestDesiredSizeHoldsWhenVisibleHasNoDims(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(0, 0, true, false, 1),    // visible, dims pending
		mkSizingSub(65, 29, false, false, 9), // hidden narrow with dims
	)
	if c, r := p.desiredSizeLocked(); c != 0 || r != 0 {
		t.Fatalf("got %dx%d, want 0x0 (hold, not adopt hidden narrow)", c, r)
	}
}

// A stale sub (no inbound frame/pong past staleSubWindow) stops voting even with
// a higher active stamp, so a half-open narrow connection can't hold the width.
func TestDesiredSizeSkipsStaleSub(t *testing.T) {
	t.Parallel()
	visWide := mkSizingSub(190, 50, true, false, 5)
	staleNarrow := mkSizingSub(65, 29, true, false, 9) // higher seq but stale
	staleNarrow.lastSeenAt = time.Now().Add(-2 * staleSubWindow)
	p := paneWithSizingSubs(visWide, staleNarrow)
	if c, r := p.desiredSizeLocked(); c != 190 || r != 50 {
		t.Fatalf("got %dx%d, want 190x50 (stale narrow must be skipped)", c, r)
	}
}

// With NO visible sub at all, fall back to the most-recently-active hidden sub
// so a brief all-hidden blip doesn't spuriously resize.
func TestDesiredSizeFallsBackToHiddenWhenNoVisible(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(120, 40, false, false, 3),
		mkSizingSub(190, 50, false, false, 7), // newer hidden → wins fallback
	)
	if c, r := p.desiredSizeLocked(); c != 190 || r != 50 {
		t.Fatalf("got %dx%d, want 190x50 (most-active hidden fallback)", c, r)
	}
}

// Regression guard: a phone actively viewing still wins its narrow width
// (phone-first precedence must survive the hardening).
func TestDesiredSizeVisibleMobileStillWins(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(190, 50, true, false, 1), // visible desktop
		mkSizingSub(40, 90, true, true, 9),   // visible phone, newer → wins
	)
	if c, r := p.desiredSizeLocked(); c != 40 || r != 90 {
		t.Fatalf("got %dx%d, want 40x90 (visible mobile wins)", c, r)
	}
}

// updateSize's active-widen reclaim: a visible desktop that grows materially
// wider (a maximize) re-stamps itself active; sub-threshold jitter does not.
func TestUpdateSizeWidenReclaimStampsActive(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	subID, _, _, _, updateSize, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()

	seqOf := func() int64 {
		p.subsMu.Lock()
		defer p.subsMu.Unlock()
		return p.subs[subID].lastActiveSeq
	}

	before := seqOf()
	updateSize(190, 50) // 80 -> 190: a real maximize (>= widenReclaimCols)
	if after := seqOf(); after <= before {
		t.Fatalf("widen-reclaim did not stamp active: before=%d after=%d", before, after)
	}
	mid := seqOf()
	updateSize(191, 50) // +1 col: below threshold, must not restamp
	if after := seqOf(); after != mid {
		t.Fatalf("sub-threshold jitter wrongly restamped: %d -> %d", mid, after)
	}
}

// ST-258: a visible view that has never been active (seq 0, e.g. the
// standalone desktop window left open behind Shinoque) must not take the size
// from a hidden desktop view that was active. HOLD (0,0 = leave PTY as is).
func TestDesiredSizeNeverActiveVisibleHoldsForHiddenActiveDesktop(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(58, 28, true, false, 0),     // idle window, never active
		mkSizingSub(49, 22, false, false, 1668), // owner, just hidden
	)
	if c, r := p.desiredSizeLocked(); c != 0 || r != 0 {
		t.Fatalf("got %dx%d, want 0x0 (hold the hidden owner's size)", c, r)
	}
}

// A visible view that WAS active at some point still takes the size when the
// more recent view hides: only never-active views are held back.
func TestDesiredSizeOnceActiveVisibleTakesOverFromHidden(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(58, 28, true, false, 3),
		mkSizingSub(49, 22, false, false, 9),
	)
	if c, r := p.desiredSizeLocked(); c != 58 || r != 28 {
		t.Fatalf("got %dx%d, want 58x28 (an once-active visible view takes over)", c, r)
	}
}

// Pocketing the phone still hands the width back to a visible desktop even
// when the desktop's (reconnected) socket was never stamped active.
func TestDesiredSizeHiddenPhoneHandsBackToNeverActiveDesktop(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(190, 50, true, false, 0), // desktop, unstamped reconnect
		mkSizingSub(40, 90, false, true, 9),  // phone, backgrounded
	)
	if c, r := p.desiredSizeLocked(); c != 190 || r != 50 {
		t.Fatalf("got %dx%d, want 190x50 (hidden phone hands width back)", c, r)
	}
}

// With no active view anywhere (every stamp 0, e.g. right after a daemon
// restart), the visible view still sizes the pane.
func TestDesiredSizeNeverActiveVisibleBeatsNeverActiveHidden(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(58, 28, true, false, 0),
		mkSizingSub(49, 22, false, false, 0),
	)
	if c, r := p.desiredSizeLocked(); c != 58 || r != 28 {
		t.Fatalf("got %dx%d, want 58x28 (visible view sizes when none is active)", c, r)
	}
}

// ST-258 end to end: the owner hiding (workspace switch) and showing again
// leaves the PTY at the owner's size the whole time, so a shell gets no
// SIGWINCH and draws no extra prompt row. The idle window takes the size only
// once it is actually used.
func TestOwnerHideDoesNotHandPTYToNeverActiveWindow(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      49,
		Rows:      22,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(49, 22),
	}
	idle := p.SubscribeStream(58, 28, subModeFull, 0, false)
	defer idle.cancel()
	owner := p.SubscribeStream(49, 22, subModeFull, 0, false)
	defer owner.cancel()
	owner.markActive()
	waitPaneSize(t, p, 49, 22)

	assertSizeHeld := func(what string) {
		t.Helper()
		deadline := time.Now().Add(3 * resizeCoalesceDelay)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			c, r := p.Cols, p.Rows
			p.mu.Unlock()
			if c != 49 || r != 22 {
				t.Fatalf("%s: pane resized to %dx%d, want it held at 49x22", what, c, r)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for i := 0; i < 3; i++ {
		owner.updateVisibility(false)
		assertSizeHeld("owner hidden")
		owner.updateVisibility(true)
		assertSizeHeld("owner shown")
	}

	// The idle window is used (focused or typed in): it takes the size.
	owner.updateVisibility(false)
	idle.markActive()
	waitPaneSize(t, p, 58, 28)
}

// Owning the pane is not being active: an idle window that adopted the pane
// when the person's socket dropped (seen 12:02–14:10 on 14 Nov) is still held
// against the person's hidden view. A reconnected view the person is using
// takes the size at its next focus or keystroke, which stamp it active.
func TestDesiredSizeNeverActiveOwnerStillHolds(t *testing.T) {
	t.Parallel()
	p := paneWithSizingSubs(
		mkSizingSub(58, 28, true, false, 0),   // idle window, adopted owner
		mkSizingSub(49, 22, false, false, 40), // the person's view, hidden
	)
	p.ownerSubID = 1
	if c, r := p.desiredSizeLocked(); c != 0 || r != 0 {
		t.Fatalf("got %dx%d, want 0x0 (an unstamped owner holds too)", c, r)
	}
}
