package main

import (
	"bytes"
	"testing"

	"soluterminal/grid"
)

// TestAdoptAgentDimsRepairsStalePersistedSize is the daemon half of the
// "every app update garbles all panes" fix: a pane rebuilt from panes.json
// carries stale dims (observed live: 283×62 persisted vs 67×29 real PTY).
// The agent's FrameDims report must replace them, resize the replay
// emulator BEFORE the reattach replay feeds it, mark the dims verified,
// and fire the persist hook so the next restart starts truthful.
func TestAdoptAgentDimsRepairsStalePersistedSize(t *testing.T) {
	t.Parallel()
	committed := 0
	p := &Pane{
		ID:        "pane-test",
		Cols:      283, // stale persisted copy
		Rows:      62,
		buffer:    NewRingBuffer(1024),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(283, 62),
	}
	p.onDimsCommit = func(*Pane) { committed++ }

	p.adoptAgentDims(67, 29)

	p.mu.Lock()
	cols, rows, verified := p.Cols, p.Rows, p.dimsVerified
	p.mu.Unlock()
	if cols != 67 || rows != 29 {
		t.Fatalf("dims after adopt = %dx%d, want 67x29", cols, rows)
	}
	if !verified {
		t.Fatal("adoptAgentDims must mark dims verified")
	}
	if committed != 1 {
		t.Fatalf("onDimsCommit fired %d times, want 1", committed)
	}
	snap := p.replayEmu.Snapshot()
	if snap.Cols != 67 || snap.Rows != 29 {
		t.Fatalf("replayEmu = %dx%d, want 67x29 (must be resized before replay feed)", snap.Cols, snap.Rows)
	}
	if p.Snapshot().DimsUnverified {
		t.Fatal("PaneSnapshot must report dims verified after adoption")
	}
}

// TestAdoptAgentDimsEqualStillVerifies: when the agent's report happens to
// match the persisted copy, the dims are proven right — verify without a
// spurious persist.
func TestAdoptAgentDimsEqualStillVerifies(t *testing.T) {
	t.Parallel()
	committed := 0
	p := &Pane{
		ID:        "pane-test",
		Cols:      120,
		Rows:      32,
		buffer:    NewRingBuffer(1024),
		subs:      map[int64]*subEntry{},
		replayEmu: grid.New(120, 32),
	}
	p.onDimsCommit = func(*Pane) { committed++ }

	p.adoptAgentDims(120, 32)

	p.mu.Lock()
	verified := p.dimsVerified
	p.mu.Unlock()
	if !verified {
		t.Fatal("equal agent dims must still verify")
	}
	if committed != 0 {
		t.Fatalf("onDimsCommit fired %d times on a no-op adopt, want 0", committed)
	}
}

// TestResizeVerifiesDimsAndCommits: a client-driven resize is ground truth —
// it must verify the dims and fire the persist hook so panes.json tracks the
// live PTY size.
func TestResizeVerifiesDimsAndCommits(t *testing.T) {
	t.Parallel()
	committed := 0
	p := &Pane{
		ID:     "pane-test",
		Cols:   283, // stale persisted copy, unverified (reattach state)
		Rows:   62,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	p.onDimsCommit = func(*Pane) { committed++ }

	if err := p.Resize(67, 29); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	p.mu.Lock()
	verified := p.dimsVerified
	p.mu.Unlock()
	if !verified {
		t.Fatal("an applied Resize must verify the dims")
	}
	if committed != 1 {
		t.Fatalf("onDimsCommit fired %d times, want 1", committed)
	}

	// A resize equal to current dims (dedupe early-return) still proves them.
	p2 := &Pane{
		ID:     "pane-test-2",
		Cols:   67,
		Rows:   29,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	if err := p2.Resize(67, 29); err != nil {
		t.Fatalf("Resize (equal): %v", err)
	}
	p2.mu.Lock()
	verified2 := p2.dimsVerified
	p2.mu.Unlock()
	if !verified2 {
		t.Fatal("an equal applied Resize must also verify the dims")
	}
}

// TestSubscribeAltScreenBumpSkippedWhileDimsUnverified: the fresh-subscribe
// SIGWINCH bump resizes the REAL PTY to p.Cols — with stale persisted dims
// that is the same disease as the client-seed garble, so it must not run
// until the dims are verified.
func TestSubscribeAltScreenBumpSkippedWhileDimsUnverified(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   283, // stale persisted copy, unverified
		Rows:   62,
		buffer: NewRingBuffer(1024),
		subs:   map[int64]*subEntry{},
	}
	p.trackModes([]byte("\x1b[?1049h")) // alt-screen, unwatched → bump would run
	_, _, _, _, _, _, _, cancel := p.Subscribe(80, 24)
	defer cancel()
	// The bump does Resize(cols-1) → Resize(cols), and Resize marks dims
	// verified — so "still unverified" proves the bump was skipped.
	p.mu.Lock()
	verified := p.dimsVerified
	p.mu.Unlock()
	if verified {
		t.Fatal("alt-screen bump ran on unverified dims (would resize the PTY to a stale width)")
	}
}

// TestResumeSpawnOptsDimsDecision pins the crux of the post-shutdown /
// cross-window garble fix: a terminal resume must NOT launder persisted
// panes.json dims into a "verified" PTY size. With a live client measurement it
// spawns verified at that width; without one it falls back to the persisted
// dims but leaves them UNVERIFIED so the first attach re-measures. Native panes
// (headless, no shared-PTY xterm) keep their prior verified-persisted behavior.
func TestResumeSpawnOptsDimsDecision(t *testing.T) {
	t.Parallel()
	base := PaneSnapshot{
		ID:              "pane-test",
		ClaudeSessionID: "sess-123",
		Cols:            283, // stale persisted copy
		Rows:            62,
	}

	// Terminal + live measurement → spawn AT the live width, verified.
	term := base
	term.Kind = ""
	opts := resumeSpawnOpts(term, 80, 24)
	if opts.Cols != 80 || opts.Rows != 24 {
		t.Fatalf("live dims: opts = %dx%d, want 80x24 (use the client's viewport)", opts.Cols, opts.Rows)
	}
	if opts.DimsUnverified {
		t.Fatal("live dims: a real client measurement must spawn VERIFIED")
	}
	if len(opts.Cmd) == 0 || opts.Cmd[len(opts.Cmd)-1] != "sess-123" {
		t.Fatalf("terminal resume must run `claude --resume <id>`, got %v", opts.Cmd)
	}

	// Terminal + no measurement → persisted dims, but UNVERIFIED so the first
	// attach re-measures instead of blowing the PTY to a stale width.
	optsNoLive := resumeSpawnOpts(term, 0, 0)
	if optsNoLive.Cols != 283 || optsNoLive.Rows != 62 {
		t.Fatalf("no live dims: opts = %dx%d, want 283x62 (persisted fallback)", optsNoLive.Cols, optsNoLive.Rows)
	}
	if !optsNoLive.DimsUnverified {
		t.Fatal("no live dims: persisted fallback MUST be unverified (the every-update garble guard)")
	}
}

// TestSubscribeEmptyFullReplayStillAnnounced guards the ws.go contract: a
// full replay with an EMPTY snapshot must still be announced (bytes:0) so a
// reconnecting client resets its stale surface instead of letting the
// agent's fanned-out reattach replay jumble it. The subscribeStream side of
// that contract is "empty snapshot, resumed=false" — this test pins the
// shape ws.go keys on.
func TestSubscribeEmptyFullReplayStillAnnounced(t *testing.T) {
	t.Parallel()
	p := &Pane{
		ID:     "pane-test",
		Cols:   80,
		Rows:   24,
		buffer: NewRingBuffer(1024), // fresh post-restart ring: empty
		subs:   map[int64]*subEntry{},
	}
	s := p.subscribeStream(80, 24, subModeFull, 0, subOptions{})
	defer s.cancel()
	if s.resumed {
		t.Fatal("full replay must not report resumed")
	}
	if len(s.snapshot) != 0 {
		t.Fatalf("expected empty snapshot, got %d bytes", len(s.snapshot))
	}
	if s.cutOffset != 0 {
		t.Fatalf("cutOffset = %d, want 0 on a fresh ring", s.cutOffset)
	}
	// Bytes written AFTER the subscribe cut must reach the sub via fanout —
	// that's the stream the client's post-reset surface is rebuilt from.
	p.subsMu.Lock()
	p.buffer.Write([]byte("HISTORY"))
	p.fanoutPayloadLocked([]byte("HISTORY"))
	p.subsMu.Unlock()
	select {
	case b := <-s.ch:
		if !bytes.Contains(b, []byte("HISTORY")) {
			t.Fatalf("fanout payload = %q, want HISTORY", b)
		}
	default:
		t.Fatal("post-cut bytes did not reach the subscriber")
	}
}
