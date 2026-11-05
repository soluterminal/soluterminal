package main

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal/grid"
)

const testRingBytes = 256 * 1024

func newTestRing() *RingBuffer { return NewRingBuffer(testRingBytes) }

func TestProductionRingBufferCapacity(t *testing.T) {
	t.Parallel()
	const want = 8 * 1024 * 1024
	if ringBufferBytes != want {
		t.Fatalf("ringBufferBytes = %d, want %d; production replay capacity is load-bearing", ringBufferBytes, want)
	}
}

// connectedPane returns a Pane wired to an in-memory agent connection
// whose read side is drained, so Write/WriteNoPromptRecord don't block on
// WriteFrame. Used to exercise the prompt recorder end-to-end.
func connectedPane(t *testing.T) *Pane {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := c2.Read(buf); err != nil {
				return
			}
		}
	}()
	return &Pane{agentConn: c1}
}

// A menu digit answered via WriteNoPromptRecord must not linger in the
// recorder buffer: the auto-approval body is written before the
// submit-Enter, and when that Enter is skipped (prompt changed mid-write)
// the digit used to prepend onto the next real prompt — the "2/usage"
// carry-over.
func TestWriteNoPromptRecordPreventsMenuDigitCarryover(t *testing.T) {
	t.Parallel()
	p := connectedPane(t)
	if _, err := p.WriteNoPromptRecord([]byte("2")); err != nil { // menu body, Enter then aborted
		t.Fatalf("WriteNoPromptRecord: %v", err)
	}
	if _, err := p.Write([]byte("/usage\r")); err != nil { // next real prompt
		t.Fatalf("Write: %v", err)
	}
	if got := p.LastPrompts; len(got) != 1 || got[0] != "/usage" {
		t.Fatalf("LastPrompts = %q, want [\"/usage\"] — menu digit carried over", got)
	}
}

// Control: the recording path DOES carry the digit over, which is exactly
// why menu replies must use WriteNoPromptRecord.
func TestWriteRecordingPathCarriesMenuDigitOver(t *testing.T) {
	t.Parallel()
	p := connectedPane(t)
	if _, err := p.Write([]byte("2")); err != nil { // recorded → "2" left in buffer
		t.Fatalf("Write: %v", err)
	}
	if _, err := p.Write([]byte("/usage\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := p.LastPrompts; len(got) != 1 || got[0] != "2/usage" {
		t.Fatalf("LastPrompts = %q, want [\"2/usage\"] (carry-over via recorded path)", got)
	}
}

// WriteNoPromptRecord keeps menu replies out of the strip entirely, while
// Write still records real prompts.
func TestWriteNoPromptRecordSkipsStrip(t *testing.T) {
	t.Parallel()
	p := connectedPane(t)
	if _, err := p.WriteNoPromptRecord([]byte("11\r")); err != nil {
		t.Fatalf("WriteNoPromptRecord: %v", err)
	}
	if len(p.LastPrompts) != 0 {
		t.Fatalf("LastPrompts = %q, want empty — menu reply must not record", p.LastPrompts)
	}
	if _, err := p.Write([]byte("fix the login bug\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := p.LastPrompts; len(got) != 1 || got[0] != "fix the login bug" {
		t.Fatalf("LastPrompts = %q, want [\"fix the login bug\"]", got)
	}
}

func TestWriteNoPromptRecordDoesNotRefreshDraftActivity(t *testing.T) {
	t.Parallel()
	p := connectedPane(t)
	if _, err := p.Write([]byte("unsent draft")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	pending, recordedAt := p.composerPendingSnapshot()
	if pending == 0 || recordedAt <= 0 {
		t.Fatalf("recorded composer snapshot = (%d, %d), want nonempty with activity", pending, recordedAt)
	}

	// Make the second successful write observably later and at a distinct
	// stripped-output position. Ordinary input bookkeeping must advance, while
	// the prompt-record-bypassing write leaves the draft snapshot untouched.
	for time.Now().UnixMilli() <= recordedAt {
		time.Sleep(time.Millisecond)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, 42)
	if _, err := p.WriteNoPromptRecord([]byte("\r")); err != nil {
		t.Fatalf("WriteNoPromptRecord: %v", err)
	}
	if got := atomic.LoadInt64(&p.LastInputMs); got <= recordedAt {
		t.Fatalf("LastInputMs = %d after control write, want > recorded activity %d", got, recordedAt)
	}
	if got := atomic.LoadInt64(&p.LastInputStrippedPos); got != 42 {
		t.Fatalf("LastInputStrippedPos = %d, want 42", got)
	}
	gotPending, gotRecordedAt := p.composerPendingSnapshot()
	if gotPending != pending || gotRecordedAt != recordedAt {
		t.Fatalf("composer snapshot after control write = (%d, %d), want (%d, %d)", gotPending, gotRecordedAt, pending, recordedAt)
	}
}

func TestTrackModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		chunks []string // fed in order; simulates readLoop payload boundaries
		want   bool     // expected altScreen after all chunks
	}{
		{
			name:   "plain 1049h",
			chunks: []string{"\x1b[?1049h"},
			want:   true,
		},
		{
			name:   "plain 1049l",
			chunks: []string{"\x1b[?1049h", "\x1b[?1049l"},
			want:   false,
		},
		{
			name:   "combined 25;1049h",
			chunks: []string{"\x1b[?25;1049h"},
			want:   true,
		},
		{
			name:   "combined 1049;25h",
			chunks: []string{"\x1b[?1049;25h"},
			want:   true,
		},
		{
			name:   "combined 25;1049l exits alt",
			chunks: []string{"\x1b[?1049h", "\x1b[?25;1049l"},
			want:   false,
		},
		{
			name:   "unrelated mode ignored",
			chunks: []string{"\x1b[?25h", "\x1b[?2004h"},
			want:   false,
		},
		{
			name:   "1049 embedded in real output",
			chunks: []string{"hello\r\n\x1b[?1049h\x1b[2J\x1b[H"},
			want:   true,
		},
		{
			name:   "sequence split across boundary",
			chunks: []string{"hello\x1b[?10", "49h world"},
			want:   true,
		},
		{
			name:   "sequence split at every byte",
			chunks: []string{"\x1b", "[", "?", "1", "0", "4", "9", "h"},
			want:   true,
		},
		{
			name:   "bogus char bails parser without hanging",
			chunks: []string{"\x1b[?1049@notamode", "\x1b[?1049h"},
			want:   true,
		},
		{
			name:   "nested ESC inside partial doesn't trip",
			chunks: []string{"\x1b[?10", "\x1b[?1049h"},
			want:   true,
		},
		{
			name:   "long CSI with many params before 1049",
			chunks: []string{"\x1b[?1;2;3;4;5;1049h"},
			want:   true,
		},
		{
			name:   "1490 must not match 1049",
			chunks: []string{"\x1b[?1490h"},
			want:   false,
		},
		{
			name:   "empty chunks",
			chunks: []string{"", ""},
			want:   false,
		},
	}

	for _, c := range cases {
		p := &Pane{}
		for _, chunk := range c.chunks {
			p.trackModes([]byte(chunk))
		}
		if p.altScreen != c.want {
			t.Errorf("%s: altScreen = %v, want %v (modeTail = %q)",
				c.name, p.altScreen, c.want, string(p.modeTail))
		}
	}
}

func TestTrackModesRecordsTrackedModeState(t *testing.T) {
	t.Parallel()
	p := &Pane{}
	p.trackModes([]byte("\x1b[?2004h\x1b[?1002h\x1b[?1006h"))
	p.trackModes([]byte("\x1b[?25l"))
	p.trackModes([]byte("\x1b[?1002l"))
	wantPrefix := "\x1b[?25l\x1b[?1002l\x1b[?1006h\x1b[?2004h"
	if got := string(p.replayModePrefix()); got != wantPrefix {
		t.Fatalf("replayModePrefix() = %q, want %q", got, wantPrefix)
	}
	// Untracked / unseen modes must not appear.
	p2 := &Pane{}
	p2.trackModes([]byte("\x1b[?1049h\x1b[?9999h"))
	if got := p2.replayModePrefix(); len(got) != 0 {
		t.Fatalf("replayModePrefix() for untracked modes = %q, want empty", string(got))
	}
	if !p2.altScreen {
		t.Fatal("altScreen not set by 1049h")
	}
	// Combined param lists update every tracked mode in the list.
	p3 := &Pane{}
	p3.trackModes([]byte("\x1b[?1;2004;1049h"))
	if !p3.altScreen {
		t.Fatal("altScreen not set from combined list")
	}
	if got := string(p3.replayModePrefix()); got != "\x1b[?1h\x1b[?2004h" {
		t.Fatalf("combined-list prefix = %q, want %q", got, "\x1b[?1h\x1b[?2004h")
	}
}

func TestTrimReplayHead(t *testing.T) {
	t.Parallel()
	// ESC before newline → start at the ESC.
	in := []byte("garbage\x1b[1mstyled\nrest")
	if got := string(trimReplayHead(in)); got != "\x1b[1mstyled\nrest" {
		t.Fatalf("trimReplayHead = %q", got)
	}
	// Newline before ESC → start after the newline.
	in = []byte("partial line\nclean line\x1b[0m")
	if got := string(trimReplayHead(in)); got != "clean line\x1b[0m" {
		t.Fatalf("trimReplayHead = %q", got)
	}
	// Neither in the first 4 KiB → unchanged.
	blob := make([]byte, 8192)
	for i := range blob {
		blob[i] = 'x'
	}
	if got := trimReplayHead(blob); len(got) != len(blob) {
		t.Fatalf("trimReplayHead trimmed a boundary-free blob: %d → %d", len(blob), len(got))
	}
}

func TestSubscribeReplayTailCapped(t *testing.T) {
	t.Parallel()
	// Keep the 8 MiB production ring: the 2 MiB replay cap exceeds testRingBytes.
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(ringBufferBytes),
		subs:   map[int64]*subEntry{},
	}
	// Mark bracketed paste so a truncated replay must re-assert it.
	p.trackModes([]byte("\x1b[?2004h"))
	// Fill well past the normal-buffer cap with newline-terminated lines.
	line := make([]byte, 1023)
	for i := range line {
		line[i] = 'a'
	}
	line = append(line, '\n')
	for written := 0; written < replayTailCapNormal+1024*1024; written += len(line) {
		_, _ = p.buffer.Write(line)
	}

	_, snap, _, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()
	if len(snap) > replayTailCapNormal+64 {
		t.Fatalf("replay snapshot = %d bytes, want ≤ cap (%d) + prefix", len(snap), replayTailCapNormal)
	}
	wantPrefix := "\x1b[?2004h"
	if string(snap[:len(wantPrefix)]) != wantPrefix {
		t.Fatalf("truncated replay missing mode prefix; head = %q", string(snap[:24]))
	}
}

func TestSubscribeTruncatedNormalReplayAppendsGridPaint(t *testing.T) {
	t.Parallel()
	// Keep the 8 MiB production ring: the 2 MiB replay cap exceeds testRingBytes.
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    NewRingBuffer(ringBufferBytes),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	// The emulator's screen holds the marker; the ring tail (diff-style
	// churn from a TUI like Claude Code) does not — so the marker can
	// only reach the client via the appended grid repaint.
	p.replayEmu.Feed([]byte("SCREENMARKER\r\nPHANTOM-ON-CURSOR-ROW"))
	// Park the emulator cursor on the PHANTOM row: non-codex paints
	// must exclude exactly that row (input-row drift protection).
	line := append(bytes.Repeat([]byte{'a'}, 1023), '\n')
	for written := 0; written < replayTailCapNormal+1024*1024; written += len(line) {
		_, _ = p.buffer.Write(line)
	}

	_, snap, _, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()
	paintStart := bytes.Index(snap, []byte("\x1b[?2026l\x1b[r"))
	if paintStart < 0 {
		t.Fatal("truncated normal-buffer replay missing trailing grid repaint")
	}
	if !bytes.Contains(snap[paintStart:], []byte("SCREENMARKER")) {
		t.Fatal("grid repaint does not contain the emulator screen content")
	}
	if paintStart == 0 {
		t.Fatal("grid repaint replaced the raw tail instead of following it")
	}
	if bytes.Contains(snap[paintStart:], []byte("PHANTOM-ON-CURSOR-ROW")) {
		t.Fatal("non-codex paint must skip the cursor row — model drift there resurrects phantom prompts")
	}
}

func TestSubscribeUntruncatedNormalReplayAppendsGridPaint(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	// Even when the full byte log since spawn fits in the cap (untruncated),
	// a normal-buffer pane still gets the grid repaint: a multi-subagent
	// run's raw tail is relative-cursor diff frames that don't reconstruct
	// the visible screen on their own, so an untruncated raw-only replay
	// jumbled the same way a truncated one did. The marker lives only on
	// the emulator screen; the phantom row must still be skipped.
	p.replayEmu.Feed([]byte("SCREENMARKER\r\nPHANTOM-ON-CURSOR-ROW"))
	_, _ = p.buffer.Write([]byte("complete log since spawn\n"))

	_, snap, _, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()
	paintStart := bytes.Index(snap, []byte("\x1b[?2026l\x1b[r"))
	if paintStart < 0 {
		t.Fatal("untruncated normal-buffer replay missing trailing grid repaint")
	}
	if !bytes.Contains(snap[paintStart:], []byte("SCREENMARKER")) {
		t.Fatal("grid repaint does not contain the emulator screen content")
	}
	if paintStart == 0 {
		t.Fatal("grid repaint replaced the raw tail instead of following it")
	}
	if !bytes.Contains(snap[:paintStart], []byte("complete log since spawn")) {
		t.Fatal("raw tail (scrollback seed) must precede the repaint")
	}
	if bytes.Contains(snap[paintStart:], []byte("PHANTOM-ON-CURSOR-ROW")) {
		t.Fatal("non-codex paint must skip the cursor row — model drift there resurrects phantom prompts")
	}
}

func TestWebMobileNormalReplayAppendsGridPaint(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(50, 20),
	}
	p.replayEmu.Feed([]byte("MOBILE-WEB-SCREEN\r\n"))
	_, _ = p.buffer.Write([]byte("raw tail\n"))

	s := p.subscribeStream(50, 20, subModeFull, 0, subOptions{
		mobile:             true,
		skipSyntheticPaint: false,
	})
	defer s.cancel()
	if !bytes.Contains(s.snapshot, []byte("MOBILE-WEB-SCREEN")) {
		t.Fatal("web mobile replay did not include grid paint repair")
	}
	if !bytes.Contains(s.snapshot, []byte("raw tail")) {
		t.Fatal("web mobile replay must keep raw tail before repair paint")
	}
}

func TestLegacyNativeMobileNormalReplaySkipsGridPaint(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(50, 20),
	}
	p.replayEmu.Feed([]byte("NATIVE-MOBILE-SCREEN\r\n"))
	_, _ = p.buffer.Write([]byte("raw tail\n"))

	s := p.SubscribeStream(50, 20, subModeFull, 0, true)
	defer s.cancel()
	if bytes.Contains(s.snapshot, []byte("NATIVE-MOBILE-SCREEN")) {
		t.Fatal("legacy/native mobile replay should skip synthetic grid paint")
	}
	if !bytes.Contains(s.snapshot, []byte("raw tail")) {
		t.Fatal("legacy/native mobile replay must still include raw tail")
	}
}

// TestSubscribeGridPaintPinnedToCut guards the lock-discipline invariant of
// the out-of-subsMu serialization change: the grid repaint must reflect the
// replayEmu state at the SUBSCRIBE CUT, not any later state. The expensive
// serializeGridSnapshotClamped now runs after subsMu.Unlock(), but the
// underlying Snapshot() is still taken under the lock at the cut, so bytes
// that reach the emulator only AFTER subscribe (i.e. via live fanout) must
// never appear in the returned grid paint — otherwise the paint would
// double-apply them on top of the deltas (overlap).
func TestSubscribeGridPaintPinnedToCut(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	// Pre-subscribe screen content — must be IN the paint.
	p.replayEmu.Feed([]byte("BEFORECUT\r\n"))
	_, _ = p.buffer.Write([]byte("ring before cut\n"))

	_, snap, _, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()

	// Post-subscribe screen content — arrives via live fanout, must NOT be in
	// the paint that was serialized for this subscribe.
	p.replayEmu.Feed([]byte("AFTERCUT"))

	paintStart := bytes.Index(snap, []byte("\x1b[?2026l\x1b[r"))
	if paintStart < 0 {
		t.Fatal("normal-buffer replay missing trailing grid repaint")
	}
	if !bytes.Contains(snap[paintStart:], []byte("BEFORECUT")) {
		t.Fatal("grid repaint must contain screen content present at the cut")
	}
	if bytes.Contains(snap, []byte("AFTERCUT")) {
		t.Fatal("grid repaint leaked post-cut bytes — overlap with live fanout deltas")
	}
}

func TestNarrowSecondSubscriberDoesNotShrink(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	p.trackModes([]byte("\x1b[?1049h"))
	// First subscriber on an unwatched alt-screen pane: the bump runs
	// (Resize twice). agentConn is nil so Resize only flips dims; the
	// observable end state is unchanged at 80x24.
	_, _, _, _, _, _, _, cancel1 := p.Subscribe(80, 24)
	defer cancel1()
	p.mu.Lock()
	cols1 := p.Cols
	p.mu.Unlock()
	if cols1 != 80 {
		t.Fatalf("after first subscribe Cols = %d, want 80", cols1)
	}
	p.Claim(1, true)
	// A NARROWER, passive (non-mobile, unclaimed) second subscriber must
	// NOT shrink the pane: the size follows the ACTIVE device, which here
	// is the first viewer (it claimed → stamped active). A bare desktop
	// reattach is never stamped active, so it can't drive the size. (The
	// narrow self-adoption that used to live in Subscribe is gone.)
	_, _, _, _, _, _, _, cancel2 := p.Subscribe(50, 20)
	defer cancel2()
	p.mu.Lock()
	cols2, rows2 := p.Cols, p.Rows
	p.mu.Unlock()
	if cols2 != 80 || rows2 != 24 {
		t.Fatalf("passive narrow subscribe shrank pane to %dx%d, want 80x24", cols2, rows2)
	}
}

// waitPaneSize polls until the pane reaches the expected dims, or fails.
// recomputeSize now applies PTY resizes through a trailing-debounce coalescer
// (scheduleCoalescedResize), so size changes are eventually-consistent rather
// than synchronous — tests that expect a size change must wait for it.
func waitPaneSize(t *testing.T, p *Pane, wantCols, wantRows uint16) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		c, r := p.Cols, p.Rows
		p.mu.Unlock()
		if c == wantCols && r == wantRows {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane size = %dx%d, want %dx%d", c, r, wantCols, wantRows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The width follows the ACTIVE device. The desktop claims (active) and
// drives the width; a passive phone doesn't shrink it. When the desktop
// backgrounds, the still-visible phone becomes the only candidate and
// takes the width; when the desktop foregrounds again (a hidden→visible
// transition re-stamps it active) it takes the width back.
func TestSizeFollowsActiveDeviceAcrossVisibility(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	// Wide desktop subscribes and claims ownership → it is the active
	// device.
	deskID, _, _, _, _, deskVisible, _, deskCancel := p.Subscribe(200, 50)
	defer deskCancel()
	p.Claim(deskID, true)
	// Passive phone subscribes. It is not active, so it must NOT shrink
	// the pane the desktop is driving.
	_, _, _, _, _, _, _, phoneCancel := p.Subscribe(50, 20)
	defer phoneCancel()
	// Desktop claimed (active) → its 200x50 drives the PTY; the passive phone
	// must NOT shrink it.
	waitPaneSize(t, p, 200, 50)
	// Desktop backgrounds → only the phone is visible → width hands over
	// to the phone.
	deskVisible(false)
	waitPaneSize(t, p, 50, 20)
	// Desktop foregrounds again → hidden→visible re-stamps it active →
	// width returns to desktop.
	deskVisible(true)
	waitPaneSize(t, p, 200, 50)
}

// The core mobile fix: when the user opens the app on their phone (a
// mobile subscribe is stamped active), the phone's narrow width drives
// the PTY even though a wider desktop owner is still visible — so mobile
// is readable. (Putting the phone away — visibility false — hands the
// width back; covered by TestSizeFollowsActiveDeviceAcrossVisibility.)
func TestMobileColdOpenDrivesNarrowOverVisibleDesktop(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   200,
		Rows:   50,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	// Wide desktop owns and is the active device.
	deskID, _, _, _, _, _, _, deskCancel := p.Subscribe(200, 50)
	defer deskCancel()
	p.Claim(deskID, true)
	waitPaneSize(t, p, 200, 50)
	// Phone cold-opens (mobile subscribe) on the same pane → stamped
	// active → its narrow width wins over the still-visible desktop.
	ms := p.SubscribeStream(50, 20, subModeFull, 0, true /*mobile*/)
	defer ms.cancel()
	waitPaneSize(t, p, 50, 20)
}

// A desktop that subscribes and immediately auto-reclaims ownership (every
// WebView2 reconnect does this) must NOT steal the PTY width from a phone the
// user is actively viewing — even when the (old/cached) client asks to
// restamp. The daemon infers "connect-time auto-reclaim" from timing
// (claimConnectGrace) and grants input ownership without restamping the
// sizing seq.
func TestDesktopConnectClaimDoesNotStealWidthFromActivePhone(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   200,
		Rows:   50,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	// Phone is actively viewing (mobile cold-open stamps it the active device).
	ph := p.SubscribeStream(50, 20, subModeFull, 0, true /*mobile*/)
	defer ph.cancel()
	waitPaneSize(t, p, 50, 20)
	// Desktop connects and immediately claims with restamp requested — the
	// connect-time grace must suppress the restamp so the width stays the
	// phone's; the desktop only takes input ownership.
	desk := p.SubscribeStream(200, 50, subModeFull, 0, false /*not mobile*/)
	defer desk.cancel()
	if !p.Claim(desk.id, true /*restamp requested → ignored within grace*/) {
		t.Fatalf("desktop claim should succeed")
	}
	waitPaneSize(t, p, 50, 20)
	if !desk.isOwner() {
		t.Fatalf("desktop should hold input ownership after claiming")
	}
}

// Two desktop clients on the same account can both be visible at once (local
// host app plus a cloud browser on another PC). Under claim arbitration the
// second browser's automatic first-open claim is DENIED — the local owner is
// live and on screen — and displacing it takes the explicit Take-over
// gesture, which must still stamp the active-sizing seq even though it can
// arrive early in the connection. Otherwise every desktop sub can stay seq=0
// and desiredSizeLocked falls back to "widest visible subscriber" rather
// than the device that actually took over, which churns the shared PTY width.
func TestExplicitDesktopClaimRestampsWithinConnectGrace(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	local := p.subscribeStream(120, 40, subModeFull, 0, subOptions{deviceID: "dev-local"})
	defer local.cancel()
	if !p.ClaimWithReason(local.id, true, "first-open") {
		t.Fatalf("local first-open claim should succeed")
	}
	waitPaneSize(t, p, 120, 40)

	cloud := p.subscribeStream(200, 50, subModeFull, 0, subOptions{deviceID: "dev-cloud"})
	defer cloud.cancel()
	waitPaneSize(t, p, 120, 40)

	if p.ClaimWithReason(cloud.id, true, "first-open") {
		t.Fatalf("cloud first-open claim must be denied against a live visible owner")
	}
	if !p.ClaimWithReason(cloud.id, true, "takeover") {
		t.Fatalf("cloud takeover claim should succeed")
	}
	waitPaneSize(t, p, 200, 50)
	if !cloud.isOwner() {
		t.Fatalf("cloud desktop should hold input ownership after taking over")
	}
}

// ── Claim arbitration ──────────────────────────────────────────────────
// Automatic claims (first-open, reconnect, the Flutter app's bare re-claim
// at connect) must not displace a live owner whose user is plausibly at
// their device. Explicit takeovers always win. This is the guard against
// the silent-steal class: headless E2E browsers with fresh deviceIDs,
// pocketed phones re-claiming on relay socket cycles, and idle laptops
// re-asserting stale claim intent.

func TestFirstOpenDeniedAgainstLiveVisibleOwner(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-owner"})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	recvCtrl(t, owner.ctrl) // initial owner:none
	recvCtrl(t, owner.ctrl) // claim ack owner:you

	intruder := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-e2e-fresh"})
	defer intruder.cancel()
	recvCtrl(t, intruder.ctrl) // initial owner:other
	if p.ClaimWithReason(intruder.id, true, "first-open") {
		t.Fatalf("first-open must be denied against a live visible owner on another device")
	}
	if !owner.isOwner() || intruder.isOwner() {
		t.Fatalf("ownership must be unchanged after a denied first-open")
	}
	if got := recvCtrl(t, intruder.ctrl); got != `{"type":"ownership","owned":false,"owner":"other","denied":"owner-active"}` {
		t.Fatalf("denial ack = %s, want owner:other", got)
	}
}

func TestFirstOpenDeniedWhenOwnerDeviceRecentlyTyped(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-typing"})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	// The owned pane sits on a background workspace (hidden) — but the
	// user just typed on that device (in this pane or any other).
	owner.updateVisibility(false)
	owner.markInput()

	intruder := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-phone"})
	defer intruder.cancel()
	if p.ClaimWithReason(intruder.id, true, "first-open") {
		t.Fatalf("first-open must be denied while the owner's device is actively typing")
	}
	if !owner.isOwner() {
		t.Fatalf("ownership must be unchanged")
	}
}

func TestFirstOpenGrantedWhenOwnerHiddenAndIdle(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-idle"})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	// Owner's screen is off / app backgrounded, no input anywhere from that
	// device — the user walked away. A fresh open elsewhere should win
	// without a manual Take over (the morning walk-to-the-laptop flow).
	owner.updateVisibility(false)

	newcomer := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-laptop"})
	defer newcomer.cancel()
	if !p.ClaimWithReason(newcomer.id, true, "first-open") {
		t.Fatalf("first-open should be granted when the owner is hidden and idle")
	}
	if !newcomer.isOwner() {
		t.Fatalf("newcomer should hold input ownership")
	}
}

func TestFirstOpenGrantedAgainstSameDeviceGhost(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	ghost := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-A"})
	defer ghost.cancel()
	if !p.ClaimWithReason(ghost.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	// Page reload: the old socket lingers half-open (visible, "live" for up
	// to the read deadline) while the same device's new page auto-claims.
	successor := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-A"})
	defer successor.cancel()
	if !p.ClaimWithReason(successor.id, true, "first-open") {
		t.Fatalf("first-open must displace the same device's own ghost")
	}
	if !successor.isOwner() {
		t.Fatalf("successor should hold input ownership")
	}
}

// The Flutter app re-sends a bare {type:"claim"} the moment its socket
// reconnects (pane_socket.dart) — a background phone cycling on relay idle
// timeouts must not silently re-take a pane from the desktop the user is
// looking at. But the SAME bare message sent later in a connection is the
// app's explicit Take-over tap and must win. The daemon tells them apart by
// timing, exactly like the sizing-restamp grace.
func TestBareClaimArbitratedAtConnectExplicitAfterGrace(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-desktop"})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	phone := p.subscribeStream(50, 20, subModeFull, 0, subOptions{mobile: true, deviceID: "dev-phone"})
	defer phone.cancel()
	// Reconnect-time bare claim → automatic → denied against the live
	// visible desktop owner.
	if p.ClaimWithReason(phone.id, true, "") {
		t.Fatalf("bare claim at connect must be arbitrated and denied")
	}
	if !owner.isOwner() {
		t.Fatalf("ownership must be unchanged after the denied bare claim")
	}
	// The user taps Take over later in the same connection — explicit.
	p.subsMu.Lock()
	p.subs[phone.id].createdAt = time.Now().Add(-claimConnectGrace - time.Second)
	p.subsMu.Unlock()
	if !p.ClaimWithReason(phone.id, true, "") {
		t.Fatalf("bare claim after the connect grace is an explicit takeover and must win")
	}
	if !phone.isOwner() {
		t.Fatalf("phone should hold input ownership after its explicit claim")
	}
}

func TestTakeoverAlwaysWinsOverActiveOwner(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-busy"})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim of an unowned pane should succeed")
	}
	owner.markInput() // visible AND typing — maximally protected

	taker := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-other"})
	defer taker.cancel()
	if !p.ClaimWithReason(taker.id, true, "takeover") {
		t.Fatalf("an explicit takeover must always be granted")
	}
	if !taker.isOwner() {
		t.Fatalf("taker should hold input ownership")
	}
}

// Driven browsers (?automation=1 — Playwright/E2E) never vote on the shared
// PTY size: a default test viewport must not shrink the terminal a human is
// using, even while the automation sub is visible.
func TestAutomationSubNeverDrivesSizing(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   200,
		Rows:   50,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	desk := p.subscribeStream(200, 50, subModeFull, 0, subOptions{deviceID: "dev-human"})
	defer desk.cancel()
	if !p.ClaimWithReason(desk.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	waitPaneSize(t, p, 200, 50)

	bot := p.subscribeStream(66, 23, subModeFull, 0, subOptions{deviceID: "dev-bot", automation: true})
	defer bot.cancel()
	// Even if the bot somehow becomes the most-recently-active sub, the
	// election skips it entirely.
	bot.markActive()
	waitPaneSize(t, p, 200, 50)
}

// recvCtrl reads the next control frame from a sub's ctrl channel, failing
// the test if none arrives promptly.
func recvCtrl(t *testing.T, ctrl <-chan []byte) string {
	t.Helper()
	select {
	case b := <-ctrl:
		return string(b)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for ctrl frame")
		return ""
	}
}

// Ownership frames carry provenance: owner:"none" when nobody holds the pane
// (the state every pane is in right after a daemon-update restart), "you" to
// the owner, "other" to displaced viewers. Clients need the none/other split
// to render truthful lock copy and to know a pane is safely adoptable.
func TestOwnershipFrameCarriesOwnerProvenance(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	s1 := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer s1.cancel()
	if got := recvCtrl(t, s1.ctrl); got != `{"type":"ownership","owned":false,"owner":"none"}` {
		t.Fatalf("initial frame on unowned pane = %s, want owner:none", got)
	}
	s2 := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer s2.cancel()
	if got := recvCtrl(t, s2.ctrl); got != `{"type":"ownership","owned":false,"owner":"none"}` {
		t.Fatalf("second sub initial frame = %s, want owner:none", got)
	}
	if !p.ClaimWithReason(s1.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	if got := recvCtrl(t, s1.ctrl); got != `{"type":"ownership","owned":true,"owner":"you"}` {
		t.Fatalf("claimer frame = %s, want owner:you", got)
	}
	if got := recvCtrl(t, s2.ctrl); got != `{"type":"ownership","owned":false,"owner":"other"}` {
		t.Fatalf("displaced viewer frame = %s, want owner:other", got)
	}
}

// AdoptIfUnowned grants ownership when nobody holds the pane — the post-
// daemon-restart reclaim that keeps input working without a manual Take
// over — and broadcasts the change to every sub.
func TestAdoptIfUnownedGrantsWhenUnowned(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	s1 := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer s1.cancel()
	s2 := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer s2.cancel()
	recvCtrl(t, s1.ctrl) // drain initial owner:none frames
	recvCtrl(t, s2.ctrl)
	if !p.AdoptIfUnowned(s1.id) {
		t.Fatalf("adopt of an unowned pane should be granted")
	}
	if !s1.isOwner() {
		t.Fatalf("adopter should hold input ownership")
	}
	if got := recvCtrl(t, s1.ctrl); got != `{"type":"ownership","owned":true,"owner":"you"}` {
		t.Fatalf("adopter frame = %s, want owner:you", got)
	}
	if got := recvCtrl(t, s2.ctrl); got != `{"type":"ownership","owned":false,"owner":"other"}` {
		t.Fatalf("viewer frame = %s, want owner:other", got)
	}
}

// AdoptIfUnowned must NEVER displace a live owner — that's the property that
// makes it safe to fire on every reconnect. The denial acks the requester
// with a truthful owner:"other" frame so its pending-claim UI state settles
// into the locked overlay instead of waiting forever.
func TestAdoptIfUnownedDeniedWhenOwned(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	late := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer late.cancel()
	recvCtrl(t, late.ctrl) // drain initial owner:other frame
	if p.AdoptIfUnowned(late.id) {
		t.Fatalf("adopt must be denied while another sub owns the pane")
	}
	if !owner.isOwner() || late.isOwner() {
		t.Fatalf("ownership must be unchanged after a denied adopt")
	}
	if got := recvCtrl(t, late.ctrl); got != `{"type":"ownership","owned":false,"owner":"other","denied":"owner-active"}` {
		t.Fatalf("denial ack = %s, want owner:other", got)
	}
}

// A device racing its OWN dead socket must never lock itself out. The desktop
// reaches the daemon through a multi-hop proxy chain, so an abandoned
// connection (page reload, WebView2 churn) lingers half-open server-side —
// still registered, still the owner — while its replacement asks to adopt.
// Same deviceID identifies that owner as the requester's own ghost: displace
// it instantly instead of denying with a false "in use somewhere else".
func TestAdoptDisplacesSameDeviceGhostOwner(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	ghost := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-A"})
	defer ghost.cancel()
	if !p.ClaimWithReason(ghost.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	recvCtrl(t, ghost.ctrl) // initial owner:none
	recvCtrl(t, ghost.ctrl) // claim ack owner:you
	successor := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-A"})
	defer successor.cancel()
	recvCtrl(t, successor.ctrl) // initial owner:other
	if !p.AdoptIfUnowned(successor.id) {
		t.Fatalf("adopt must displace a same-device owner (own ghost)")
	}
	if !successor.isOwner() || ghost.isOwner() {
		t.Fatalf("ownership must transfer to the successor sub")
	}
	if got := recvCtrl(t, successor.ctrl); got != `{"type":"ownership","owned":true,"owner":"you"}` {
		t.Fatalf("successor frame = %s, want owner:you", got)
	}
	if got := recvCtrl(t, ghost.ctrl); got != `{"type":"ownership","owned":false,"owner":"other"}` {
		t.Fatalf("displaced ghost frame = %s, want owner:other", got)
	}
}

// A silent owner past staleOwnerThreshold is a ghost from ANY device (every
// first-party client heartbeats every 25–30s): adoption displaces it, so a
// half-open connection can't pin a pane locked for its full read-deadline
// lifetime. A FRESH owner on a different device still denies (next test and
// TestAdoptIfUnownedDeniedWhenOwned).
func TestAdoptDisplacesStaleOwner(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	ghost := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-A"})
	defer ghost.cancel()
	if !p.ClaimWithReason(ghost.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	late := p.subscribeStream(80, 24, subModeFull, 0, subOptions{deviceID: "dev-B"})
	defer late.cancel()
	// Fresh different-device owner → denied.
	if p.AdoptIfUnowned(late.id) {
		t.Fatalf("adopt must be denied while the other-device owner is fresh")
	}
	// Age the owner past the stale threshold — a live client's heartbeats
	// would have bumped lastSeenAt long before this.
	p.subsMu.Lock()
	p.subs[ghost.id].lastSeenAt = time.Now().Add(-staleOwnerThreshold - time.Second)
	p.subsMu.Unlock()
	if !p.AdoptIfUnowned(late.id) {
		t.Fatalf("adopt must displace an owner silent past staleOwnerThreshold")
	}
	if !late.isOwner() || ghost.isOwner() {
		t.Fatalf("ownership must transfer to the live sub")
	}
}

// Empty deviceIDs (old bundles, probes) must never match each other — the
// same-device fast path requires a real identity on both sides.
func TestAdoptEmptyDeviceIDsNeverMatch(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.subscribeStream(80, 24, subModeFull, 0, subOptions{})
	defer owner.cancel()
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	late := p.subscribeStream(80, 24, subModeFull, 0, subOptions{})
	defer late.cancel()
	if p.AdoptIfUnowned(late.id) {
		t.Fatalf("empty deviceIDs must not grant same-device adoption")
	}
	if !owner.isOwner() {
		t.Fatalf("ownership must be unchanged")
	}
}

// Adoption is continuity repair, not a user gesture: it must not restamp the
// active-sizing seq, so a desktop re-adopting after a daemon restart can't
// steal the shared PTY width from a phone the user is actively viewing.
func TestAdoptIfUnownedDoesNotStealWidthFromActivePhone(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   200,
		Rows:   50,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	ph := p.SubscribeStream(50, 20, subModeFull, 0, true /*mobile*/)
	defer ph.cancel()
	waitPaneSize(t, p, 50, 20)
	desk := p.SubscribeStream(200, 50, subModeFull, 0, false)
	defer desk.cancel()
	if !p.AdoptIfUnowned(desk.id) {
		t.Fatalf("adopt of an unowned pane should be granted")
	}
	if !desk.isOwner() {
		t.Fatalf("desktop should hold input ownership after adopting")
	}
	waitPaneSize(t, p, 50, 20)
}

// When the owner sub disconnects, the remaining viewers learn the pane is
// now UNOWNED (owner:"none") — so their overlays can stop claiming "in use
// somewhere else" about a pane nobody is using.
func TestOwnerDepartureBroadcastsUnowned(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	owner := p.SubscribeStream(80, 24, subModeFull, 0, false)
	if !p.ClaimWithReason(owner.id, true, "first-open") {
		t.Fatalf("claim should succeed")
	}
	viewer := p.SubscribeStream(80, 24, subModeFull, 0, false)
	defer viewer.cancel()
	recvCtrl(t, viewer.ctrl) // drain initial owner:other frame
	owner.cancel()
	if got := recvCtrl(t, viewer.ctrl); got != `{"type":"ownership","owned":false,"owner":"none"}` {
		t.Fatalf("post-departure frame = %s, want owner:none", got)
	}
}

// A phone the user is actively viewing shares the PTY with an always-mounted
// desktop sub (warm pane, visible=true, wide). The native app's WS connection
// churns — it drops and reconnects on backoff/app-lifecycle. During that gap
// the phone sub is gone and ONLY the wide desktop remains, so naive
// active-device sizing would flip the PTY desktop-wide for the few hundred ms
// until the phone reconnects — clipping Codex on the phone and snapping its
// scroll to the bottom on the resulting repaint. The reconnect-gap hold must
// keep the phone width through the gap instead of oscillating.
func TestMobileReconnectGapHoldsPhoneWidth(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   200,
		Rows:   50,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	// Wide desktop is mounted and visible (warm pane), never foregrounded.
	desk := p.SubscribeStream(200, 50, subModeFull, 0, false /*not mobile*/)
	defer desk.cancel()
	// Phone cold-opens (active) → its narrow width drives the PTY.
	ph := p.SubscribeStream(50, 20, subModeFull, 0, true /*mobile*/)
	waitPaneSize(t, p, 50, 20)
	// Phone's WS drops (reconnect gap): only the wide desktop is left.
	ph.cancel()
	// The hold must keep the phone width — assert it does NOT flip to the
	// desktop's 200x50 across the coalesce window + a margin.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		c, r := p.Cols, p.Rows
		p.mu.Unlock()
		if c != 50 || r != 20 {
			t.Fatalf("size flipped to %dx%d during reconnect gap; want held 50x20", c, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParamListContains(t *testing.T) {
	t.Parallel()
	cases := []struct {
		params string
		want   int
		found  bool
	}{
		{"1049", 1049, true},
		{"25;1049", 1049, true},
		{"1049;25", 1049, true},
		{"1;2;3;1049;4", 1049, true},
		{"1490", 1049, false},
		{"10490", 1049, false},
		{"", 1049, false},
		{"25", 1049, false},
		{";1049", 1049, true}, // leading empty → 0, then 1049
		{"1049;", 1049, true}, // 1049, trailing empty — matches on the ';' boundary
	}
	for _, c := range cases {
		got := paramListContains([]byte(c.params), c.want)
		if got != c.found {
			t.Errorf("paramListContains(%q, %d) = %v, want %v",
				c.params, c.want, got, c.found)
		}
	}
}

func TestSubscribeNoReplaySkipsSnapshot(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	_, _ = p.buffer.Write([]byte("hello replay"))

	id, snap, ch, ctrl, _, _, _, cancel := p.SubscribeNoReplay(80, 24)
	defer cancel()
	if id == 0 {
		t.Fatal("SubscribeNoReplay returned zero sub id")
	}
	if len(snap) != 0 {
		t.Fatalf("SubscribeNoReplay snapshot length = %d, want 0", len(snap))
	}
	if ch == nil || ctrl == nil {
		t.Fatal("SubscribeNoReplay returned nil channels")
	}

	_, normalSnap, _, _, _, _, _, normalCancel := p.Subscribe(80, 24)
	defer normalCancel()
	if string(normalSnap) != "hello replay" {
		t.Fatalf("Subscribe snapshot = %q, want replay bytes", string(normalSnap))
	}
}

// collectUntil drains ch, appending every received chunk to a buffer,
// until pred matches the accumulated bytes or the timeout elapses.
func collectUntil(t *testing.T, ch <-chan []byte, timeout time.Duration, pred func([]byte) bool) []byte {
	t.Helper()
	var got []byte
	deadline := time.After(timeout)
	for {
		if pred(got) {
			return got
		}
		select {
		case b, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, b...)
		case <-deadline:
			return got
		}
	}
}

func TestSubscribeNoReplaySendsGridPaintRepair(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	// Output emitted while the client was disconnected: in the ring and
	// on the emulator's screen, but the skip-replay client never gets
	// the ring bytes — only the repaint can carry GAPMARKER to it.
	p.replayEmu.Feed([]byte("GAPMARKER\r\n"))
	_, _ = p.buffer.Write([]byte("GAPMARKER\r\n"))

	_, snap, ch, _, _, _, _, cancel := p.SubscribeNoReplay(80, 24)
	defer cancel()
	if len(snap) != 0 {
		t.Fatalf("skip-replay snapshot length = %d, want 0 (repair is deferred)", len(snap))
	}
	// LastActivityMs is zero (idle pane) → the deferred repair fires on
	// its first poll. Wait for the paint on the fanout channel.
	got := collectUntil(t, ch, 2*time.Second, func(b []byte) bool {
		return bytes.Contains(b, []byte("GAPMARKER"))
	})
	if !bytes.Contains(got, []byte("\x1b[?2026l\x1b[r")) {
		t.Fatalf("skip-replay sub did not receive a grid repaint; got %d bytes", len(got))
	}
	if !bytes.Contains(got, []byte("GAPMARKER")) {
		t.Fatal("repaint missing the emulator screen content")
	}
}

func TestSubscribeNoReplayAltScreenSkipsGridPaint(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	p.replayEmu.Feed([]byte("GAPMARKER"))
	p.trackModes([]byte("\x1b[?1049h"))

	_, snap, ch, _, _, _, _, cancel := p.SubscribeNoReplay(80, 24)
	defer cancel()
	if len(snap) != 0 {
		t.Fatalf("alt-screen skip-replay snapshot length = %d, want 0 (bump repairs alt-screen)", len(snap))
	}
	select {
	case b := <-ch:
		t.Fatalf("alt-screen skip-replay sub received unexpected bytes: %q", string(b[:min(len(b), 40)]))
	case <-time.After(250 * time.Millisecond):
	}
}

func TestSubscribeNoReplayRepairWaitsForQuiet(t *testing.T) {
	t.Parallel()
	// The busy phase is a wall-clock simulation: this goroutine stamps
	// LastActivityMs to stand in for a live PTY. runSubRepair can only paint
	// once it observes subRepairQuiet of silence, so a paint that lands while
	// the stamps were dense is the ghost-text regression — but a paint that
	// lands after this goroutine lost the CPU for longer than the quiet window
	// proves nothing: from the pane's side the stream really did fall silent.
	// Under full-suite load that starvation happens, so measure the real gap
	// and retry the simulation rather than blame the pane for it.
	var starved time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		if starved = subRepairQuietAttempt(t); starved == 0 {
			return
		}
		t.Logf("attempt %d: the test goroutine stalled %v, past the %v quiet window — the pane saw a genuinely quiet stream; retrying", attempt, starved, subRepairQuiet)
	}
	t.Fatalf("the busy phase never kept the stream busy: this goroutine stalled %v every attempt (> the %v quiet window)", starved, subRepairQuiet)
}

func TestSubscribeNoReplayRepairObservesQuietAfterScheduling(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	p.replayEmu.Feed([]byte("GAPMARKER\r\n"))

	// LastActivityMs is a wall-clock value and can appear old after a clock
	// discontinuity. A repair must still observe a monotonic quiet window after
	// it is scheduled; trusting that apparent age paints immediately and can
	// splice a stale grid into an active PTY frame.
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Hour).UnixMilli())
	_, _, ch, _, _, _, _, cancel := p.SubscribeNoReplay(80, 24)
	defer cancel()

	select {
	case b := <-ch:
		t.Fatalf("repair trusted stale wall-clock activity before observing quiet: %q", string(b[:min(len(b), 40)]))
	case <-time.After(subRepairQuiet / 2):
	}

	got := collectUntil(t, ch, 2*time.Second, func(b []byte) bool {
		return bytes.Contains(b, []byte("GAPMARKER"))
	})
	if !bytes.Contains(got, []byte("GAPMARKER")) {
		t.Fatal("repair did not arrive after the observed quiet window")
	}
}

// subRepairQuietAttempt runs one busy-then-quiet simulation of the repair gate.
// It fails the test outright when a repair paint arrives even though the stamps
// never left a subRepairQuiet-sized hole for it — the pane painting mid-burst.
// When the paint arrives after such a hole it returns the hole, telling the
// caller the simulation, not the pane, is what went quiet.
func subRepairQuietAttempt(t *testing.T) time.Duration {
	t.Helper()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	p.replayEmu.Feed([]byte("GAPMARKER\r\n"))
	// Mark the stream as actively mid-burst: the repair must hold off
	// while bytes are flowing (a paint serialized mid-frame plants
	// ghost text — the v0.0.509 input-row ghost bug).
	stamped := time.Now()
	atomic.StoreInt64(&p.LastActivityMs, stamped.UnixMilli())

	_, _, ch, _, _, _, _, cancel := p.SubscribeNoReplay(80, 24)
	defer cancel()

	// widest is the largest silence this goroutine ever left in the stamps. It
	// is the bound on what runSubRepair could have observed, so it decides
	// whether an early paint is the pane's fault or the scheduler's.
	var widest time.Duration
	busyUntil := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(busyUntil) {
		select {
		case b := <-ch:
			if gap := time.Since(stamped); gap > widest {
				widest = gap
			}
			// One millisecond of slack: LastActivityMs is truncated to millis, so
			// the pane can measure up to that much MORE silence than the gap
			// observed here. A real regression paints within microseconds of the
			// last byte, nowhere near this edge.
			if widest < subRepairQuiet-time.Millisecond {
				t.Fatalf("repair fired while the stream was busy (widest stamp gap %v, quiet window %v): %q", widest, subRepairQuiet, string(b[:min(len(b), 40)]))
			}
			return widest
		case <-time.After(20 * time.Millisecond):
		}
		now := time.Now()
		if gap := now.Sub(stamped); gap > widest {
			widest = gap
		}
		stamped = now
		atomic.StoreInt64(&p.LastActivityMs, stamped.UnixMilli())
	}

	// Stream goes quiet → repair must arrive promptly.
	got := collectUntil(t, ch, 2*time.Second, func(b []byte) bool {
		return bytes.Contains(b, []byte("GAPMARKER"))
	})
	if !bytes.Contains(got, []byte("GAPMARKER")) {
		t.Fatal("repair did not arrive after the stream went quiet")
	}
	return 0
}

func TestFanoutLagRepair(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	_, _, ch, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()

	feed := func(b []byte) {
		p.subsMu.Lock()
		p.buffer.Write(b)
		p.replayEmu.Feed(b)
		p.fanoutPayloadLocked(b)
		p.subsMu.Unlock()
	}

	// Saturate the channel so the next payload is dropped.
	p.subsMu.Lock()
	e := p.subs[1]
	for len(e.ch) < cap(e.ch) {
		e.ch <- []byte("fill")
	}
	p.subsMu.Unlock()
	feed([]byte("DROPPEDMARKER"))
	p.subsMu.Lock()
	if !e.lagged {
		p.subsMu.Unlock()
		t.Fatal("full channel did not mark the sub lagged")
	}
	p.subsMu.Unlock()

	// While lagged, further payloads must NOT trickle through out of
	// order — the sub is paused until the catch-up slice.
	feed([]byte("MIDGAP"))

	// Drain (simulating the WS writer catching up), then feed again:
	// the sub must receive ONE exact catch-up slice covering every
	// byte since the drop — DROPPEDMARKER, MIDGAP, and the triggering
	// payload — in original order, byte-for-byte from the ring.
	for len(ch) > 0 {
		<-ch
	}
	feed([]byte("NEXT"))
	got := collectUntil(t, ch, 2*time.Second, func(b []byte) bool {
		return bytes.Contains(b, []byte("NEXT"))
	})
	want := "DROPPEDMARKERMIDGAPNEXT"
	if string(got) != want {
		t.Fatalf("catch-up slice = %q, want %q (exact ring bytes, no model paint)", string(got), want)
	}
	p.subsMu.Lock()
	if e.lagged {
		p.subsMu.Unlock()
		t.Fatal("lagged flag not cleared after catch-up")
	}
	p.subsMu.Unlock()
}

func TestSubscribeStreamResumeExact(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:        "pane-test",
		Cols:      80,
		Rows:      24,
		buffer:    newTestRing(),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(80, 24),
	}
	_, _ = p.buffer.Write([]byte("history before disconnect"))
	since := p.buffer.TotalWritten()
	_, _ = p.buffer.Write([]byte("MISSED-DURING-GAP"))

	s := p.SubscribeStream(80, 24, subModeResume, since, false)
	defer s.cancel()
	if !s.resumed {
		t.Fatal("resume with in-ring offset did not resume")
	}
	if string(s.snapshot) != "MISSED-DURING-GAP" {
		t.Fatalf("resume slice = %q, want exactly the missed bytes", string(s.snapshot))
	}
	if s.cutOffset != p.buffer.TotalWritten() {
		t.Fatalf("cutOffset = %d, want %d", s.cutOffset, p.buffer.TotalWritten())
	}
}

func TestSubscribeStreamResumeNothingMissed(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: newTestRing(),
		subs:   map[int64]*subEntry{},
	}
	_, _ = p.buffer.Write([]byte("steady state"))
	s := p.SubscribeStream(80, 24, subModeResume, p.buffer.TotalWritten(), false)
	defer s.cancel()
	if !s.resumed || len(s.snapshot) != 0 {
		t.Fatalf("clean reattach: resumed=%v snapshot=%d bytes, want resumed with empty slice",
			s.resumed, len(s.snapshot))
	}
}

func TestSubscribeStreamResumeFallsBackToFullReplay(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(64), // tiny ring so the gap is evicted
		subs:   map[int64]*subEntry{},
	}
	_, _ = p.buffer.Write([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	since := int64(1) // offset long evicted
	_, _ = p.buffer.Write(bytes.Repeat([]byte{'b'}, 64))

	s := p.SubscribeStream(80, 24, subModeResume, since, false)
	defer s.cancel()
	if s.resumed {
		t.Fatal("evicted offset must not resume — client needs a full replace")
	}
	if len(s.snapshot) == 0 {
		t.Fatal("fallback must carry a full replay snapshot")
	}
}

func TestRedrawNormalBufferDoesNotInjectScrollBytes(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	_, _, ch, _, _, _, _, _ := p.Subscribe(80, 24)

	p.Redraw()

	if got := p.buffer.Snapshot(); len(got) != 0 {
		t.Fatalf("buffer after normal-buffer Redraw has %d bytes, want 0", len(got))
	}
	select {
	case got := <-ch:
		t.Fatalf("normal-buffer Redraw emitted %q, want no PTY bytes", string(got))
	default:
	}
	if p.redrawInProgress.Load() {
		t.Fatal("redrawInProgress still set after normal-buffer Redraw")
	}
}

func TestResizeClearBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		alt     bool
		oldCols uint16
		oldRows uint16
		cols    uint16
		rows    uint16
		want    string
	}{
		{
			// Small continuous step (splitter drag, subscribe-time
			// 1-col bump): no clear — the TUI's repaint covers it and
			// the blank-flash was the visible "text disturbed" bug.
			name:    "alt screen small delta no clear",
			alt:     true,
			oldCols: 120, oldRows: 32, cols: 121, rows: 32,
			want: "",
		},
		{
			name:    "alt screen discontinuous width clear",
			alt:     true,
			oldCols: 120, oldRows: 32, cols: 160, rows: 32,
			want: "\x1b[2J\x1b[H",
		},
		{
			name:    "alt screen discontinuous height clear",
			alt:     true,
			oldCols: 120, oldRows: 32, cols: 120, rows: 40,
			want: "\x1b[2J\x1b[H",
		},
		{
			name:    "normal large width no daemon clear",
			oldCols: 120, oldRows: 32, cols: 160, rows: 32,
			want: "",
		},
		{
			name:    "normal large height no daemon clear",
			oldCols: 120, oldRows: 32, cols: 120, rows: 40,
			want: "",
		},
		{
			name:    "normal small drag no clear",
			oldCols: 120, oldRows: 32, cols: 123, rows: 33,
			want: "",
		},
		{
			name:    "normal shell no clear",
			oldCols: 120, oldRows: 32, cols: 160, rows: 40,
			want: "",
		},
	}
	for _, c := range cases {
		got := string(resizeClearBytes(c.alt, c.oldCols, c.oldRows, c.cols, c.rows))
		if got != c.want {
			t.Fatalf("%s: resizeClearBytes() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSaneTerminalDimensions(t *testing.T) {
	t.Parallel()
	if _, _, ok := saneTerminalDimensions(0, 24); ok {
		t.Fatal("zero cols accepted")
	}
	if _, _, ok := saneTerminalDimensions(80, -1); ok {
		t.Fatal("negative rows accepted")
	}
	cols, rows, ok := saneTerminalDimensions(maxTerminalCols+1, maxTerminalRows+1)
	if !ok {
		t.Fatal("large positive dimensions rejected instead of capped")
	}
	if cols != maxTerminalCols || rows != maxTerminalRows {
		t.Fatalf("capped dimensions = %dx%d, want %dx%d", cols, rows, maxTerminalCols, maxTerminalRows)
	}
	cols, rows, ok = saneTerminalDimensions(132, 43)
	if !ok || cols != 132 || rows != 43 {
		t.Fatalf("normal dimensions = %dx%d ok=%v, want 132x43 ok=true", cols, rows, ok)
	}
}

func TestResizeCapsPaneDimensions(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}

	if err := p.Resize(^uint16(0), ^uint16(0)); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	p.mu.Lock()
	cols, rows := p.Cols, p.Rows
	p.mu.Unlock()
	if cols != maxTerminalCols || rows != maxTerminalRows {
		t.Fatalf("pane size = %dx%d, want capped %dx%d", cols, rows, maxTerminalCols, maxTerminalRows)
	}
}
