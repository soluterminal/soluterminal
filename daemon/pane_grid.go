package main

import (
	"strings"
	"sync/atomic"
	"time"

	"soluterminal/grid"
)

// pane_grid.go isolates the grid-EMULATOR additions to Pane so the surgical
// footprint in pane.go is minimal. The emulator is a server-side VT model
// (charmbracelet/vt) used by ARIA auto-mode prompt/choice detection and the
// debug-grid endpoint; it is lazily created (ensureGrid) only for panes that
// need it, so plain bytes-mode panes pay zero cost.

// ensureGrid lazily creates the per-pane emulator and starts the reply pump.
// Safe to call repeatedly; idempotent. Caller must NOT hold subsMu.
func (p *Pane) ensureGrid() {
	p.subsMu.Lock()
	if p.gridEmu != nil {
		p.subsMu.Unlock()
		return
	}
	cols := int(p.Cols)
	rows := int(p.Rows)
	if safeCols, safeRows, ok := saneTerminalDimensions(cols, rows); ok {
		cols, rows = int(safeCols), int(safeRows)
	} else {
		cols, rows = defaultCols, defaultRows
	}
	em := grid.New(cols, rows)
	// Grid seeding replays scrollback and deliberately waits for the emulator
	// to settle. Do not observe intermediate replay frames: they are history,
	// not live prompt-instance edges. The final snapshot is installed as the
	// lifecycle baseline below, after replay has settled.
	p.resetGridPromptLifecycleLocked(nil)
	if raw := p.buffer.Snapshot(); len(raw) > 0 {
		em.Feed(raw)
		// Discard replies the seed feed produced. The ring buffer
		// snapshot is HISTORICAL output: any OSC 10/11/12 color
		// query in it (foreground / background / cursor probe)
		// was answered (or abandoned) the first time around. Re-
		// replying now would inject e.g.
		// "\x1b]11;rgb:0000/0000/0000\x1b\\" into PTY input via
		// the pump below, where whatever's currently foreground
		// (often a bash prompt that took over after a TUI exited)
		// would echo it as visible gibberish in the prompt line.
		//
		// Why the sleep: emulator.drainReplies() runs as a goroutine
		// that copies bytes from vt's io.Pipe into e.out. Feed()
		// returns once the parser has advanced through every byte,
		// but vt's reply for the LAST OSC may still be in the pipe-
		// to-e.out hop when Feed returns. Without the wait, that
		// reply slips past our drain and reaches the pump on the
		// next 10 ms tick, landing on the PTY anyway. 30 ms is well
		// above the per-reply transfer cost (~µs) and is paid only
		// once per pane lifetime.
		time.Sleep(30 * time.Millisecond)
		_ = em.DrainReplies()
	}
	p.resetGridPromptLifecycleLocked(em)
	p.gridEmu = em
	p.gridReplyStop = make(chan struct{})
	stop := p.gridReplyStop
	p.subsMu.Unlock()

	// OSC-reply drain pump. The grid emulator answers terminal queries it
	// sees in the PTY stream (CPR / DA / OSC 10 color queries / keyboard
	// enhancement state) into an internal reply pipe. Nothing consumes those
	// replies any more — the grid renderer that used to forward them to the
	// PTY is gone — but the pipe still has to be drained so it can't
	// accumulate unbounded across a long-lived ARIA auto-mode pane.
	//
	// CRITICAL: we drain and DISCARD; we MUST NOT forward these replies back
	// to the PTY. The grid emulator is spun up for ARIA auto-mode panes that
	// have a live xterm.js client (which answers DA queries itself) and an
	// idle bash foreground after a TUI exits. Forwarding our reply there
	// lands as `ESC[?62;1;6;22c` phantom input and bash echoes the visible
	// `62;1;6;22c` gibberish — the v0.0.551 DA-echo bug. Draining-without-
	// forwarding is what keeps that leak closed.
	//
	// Polling at 250ms (replies are produced synchronously from Feed, so the
	// pipe never fills between drains) keeps idle wakeups cheap.
	go func() {
		const idleTick = 250 * time.Millisecond
		tick := time.NewTicker(idleTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-p.done:
				return
			case <-tick.C:
				_ = em.DrainReplies()
			}
		}
	}()
}

// GridSnapshot returns the current emulator snapshot. Returns zero value if
// the grid emulator hasn't been initialised.
func (p *Pane) GridSnapshot() (grid.Snapshot, bool) {
	p.subsMu.RLock()
	em := p.gridEmu
	p.subsMu.RUnlock()
	if em == nil {
		return grid.Snapshot{}, false
	}
	return em.Snapshot(), true
}

// ScreenSnapshot returns the ALWAYS-ON replay grid's current snapshot — the
// rendered screen every terminal pane maintains for reconnect repaints
// (p.replayEmu), unlike GridSnapshot's ARIA grid (p.gridEmu), which only
// exists once auto-mode is enabled. Used by claudePromptVisible: the raw ring
// is a DELTA stream (a TUI animates its spinner via cursor-move fragments), so
// only the assembled screen reliably shows the CURRENT frame's spinner/prompt.
func (p *Pane) ScreenSnapshot() (grid.Snapshot, bool) {
	p.subsMu.RLock()
	em := p.replayEmu
	p.subsMu.RUnlock()
	if em == nil {
		return grid.Snapshot{}, false
	}
	return em.Snapshot(), true
}

// snapshotVisibleText flattens a grid snapshot into newline-joined visible
// rows (right-trimmed) — the shape the prompt/spinner/choice regexes scan.
func snapshotVisibleText(snap grid.Snapshot) (string, bool) {
	if snap.Cols <= 0 || snap.Rows <= 0 || len(snap.Cells) == 0 {
		return "", false
	}
	var b strings.Builder
	for y := 0; y < snap.Rows; y++ {
		start := y * snap.Cols
		if start >= len(snap.Cells) {
			break
		}
		end := start + snap.Cols
		if end > len(snap.Cells) {
			end = len(snap.Cells)
		}
		line := make([]string, 0, end-start)
		for _, c := range snap.Cells[start:end] {
			if c.Content == "" {
				line = append(line, " ")
			} else {
				line = append(line, c.Content)
			}
		}
		b.WriteString(strings.TrimRight(strings.Join(line, ""), " \t"))
		b.WriteByte('\n')
	}
	return b.String(), true
}

// gridSyncUpdateEnd is ratatui's synchronized-output commit marker. Codex
// paints its normal-buffer UI between DEC mode 2026 begin/end sequences; only
// the END is a stable frame boundary. Observing the screen there avoids
// mistaking an in-progress clear-and-repaint for a prompt disappearance.
const gridSyncUpdateEnd = "\x1b[?2026l"

// feedGridPayloadLocked feeds one live PTY payload and observes every complete
// synchronized-output frame, including multiple frames coalesced into one
// socket read and a commit marker split across reads. Caller holds subsMu.
// This is deliberately in the feed path rather than computeState: the latter
// polls every 2s/5s and routinely misses the short gap between back-to-back
// local MCP permission modals.
func (p *Pane) feedGridPayloadLocked(payload []byte, observeLifecycle bool) {
	if p.gridEmu == nil || len(payload) == 0 {
		return
	}
	if !observeLifecycle {
		p.gridEmu.Feed(payload)
		return
	}

	start := 0
	for i, b := range payload {
		if b == gridSyncUpdateEnd[p.gridSyncEndMatch] {
			p.gridSyncEndMatch++
		} else if b == gridSyncUpdateEnd[0] {
			p.gridSyncEndMatch = 1
		} else {
			p.gridSyncEndMatch = 0
		}
		if p.gridSyncEndMatch != len(gridSyncUpdateEnd) {
			continue
		}

		// Feed through the commit marker before taking the snapshot. If the
		// marker began in the previous payload, those prefix bytes have already
		// reached the emulator; feeding this payload's suffix completes it.
		p.gridEmu.Feed(payload[start : i+1])
		start = i + 1
		p.gridSyncEndMatch = 0
		p.observeGridChoicePromptFrameLocked()
	}
	if start < len(payload) {
		p.gridEmu.Feed(payload[start:])
	}
}

// observeGridChoicePromptFrameLocked advances the live frame generation when
// a stable choice-prompt identity appears after an absent frame, or when the
// identity itself changes. The safety decision is intentionally elsewhere:
// generic choice menus can participate in lifecycle tracking but still fail
// isAutoApprovalPromptText before directAutoFire can write anything.
func (p *Pane) observeGridChoicePromptFrameLocked() {
	fp := gridChoicePromptFrameFingerprint(p.gridEmu)
	if fp != 0 && fp != p.gridChoicePromptObservedHash {
		atomic.AddUint64(&p.GridChoicePromptGeneration, 1)
	}
	p.gridChoicePromptObservedHash = fp
}

func gridChoicePromptFrameFingerprint(em *grid.Emulator) uint64 {
	if em == nil {
		return 0
	}
	text, ok := em.VisibleText()
	if !ok || !isGridChoicePromptText(text, true) {
		return 0
	}
	return approvalPromptFingerprint(text, true)
}

// resetGridPromptLifecycleLocked installs em's final screen as a baseline
// without manufacturing a live edge. This is used for ensureGrid scrollback
// replay and agent replay completion. Caller holds subsMu.
func (p *Pane) resetGridPromptLifecycleLocked(em *grid.Emulator) {
	p.gridChoicePromptObservedHash = gridChoicePromptFrameFingerprint(em)
	p.gridSyncEndMatch = 0
	atomic.StoreUint64(&p.GridChoicePromptGeneration, 0)
	atomic.StoreUint64(&p.LastAckedGridChoicePromptGeneration, 0)
}

// resizeGridEmu keeps the grid emulator's dimensions in lockstep with the
// PTY so the server-side cell state ARIA detection reads (GridSnapshot →
// gridVisibleText) and the debug-grid endpoint stay correct after a resize.
// Called from recomputeSize on an actual dimension change. No-op when the
// emulator hasn't been initialised (terminal panes pay nothing).
//
// vt preserves cells in the overlap region on resize, matching what codex /
// ratatui believe the terminal is in after their own SIGWINCH-driven
// partial redraw — so the emulator ends up consistent with the live screen.
func (p *Pane) resizeGridEmu(cols, rows int) {
	safeCols, safeRows, ok := saneTerminalDimensions(cols, rows)
	if !ok {
		return
	}
	p.subsMu.Lock()
	em := p.gridEmu
	if em != nil {
		em.Resize(int(safeCols), int(safeRows))
	}
	p.subsMu.Unlock()
}

// SetAutoMode updates the per-pane ARIA auto-approval policy. Empty
// string clears it (manual approval). Other values: "always", "cautious".
// Validation is the caller's job; we store whatever string we're given so
// future modes can be added without changing this method.
func (p *Pane) SetAutoMode(mode string) {
	p.autoModeMu.Lock()
	p.mu.Lock()
	if p.AutoMode != mode {
		p.autoModeGeneration++
	}
	if p.NeverAutoAnswer {
		mode = ""
	}
	p.AutoMode = mode
	p.mu.Unlock()
	p.autoModeMu.Unlock()
	if mode != "" {
		p.ensureGrid()
	}
}

// MarkAutoModeFired stamps "an auto-mode send just landed". Called
// from autoEmitter on confirm_resolved when the approval was approved
// AND no error came back — i.e. the send actually reached the PTY.
// Drives the "fired Xs ago" label on the auto-mode strip.
func (p *Pane) MarkAutoModeFired() {
	p.mu.Lock()
	p.AutoModeLastFiredMs = time.Now().UnixMilli()
	p.mu.Unlock()
	// Queued menus (parallel subagents) paint the next one as soon as this
	// answer lands, with no new PreToolUse to announce it.
	p.expectAutoPrompt()
}

// shutdownGrid stops the reply pump and closes the emulator. Called from
// Pane teardown paths.
func (p *Pane) shutdownGrid() {
	p.subsMu.Lock()
	stop := p.gridReplyStop
	em := p.gridEmu
	replayEmu := p.replayEmu
	p.gridReplyStop = nil
	p.gridEmu = nil
	p.replayEmu = nil
	p.resetGridPromptLifecycleLocked(nil)
	p.subsMu.Unlock()
	if stop != nil {
		close(stop)
	}
	if em != nil {
		_ = em.Close()
	}
	if replayEmu != nil {
		_ = replayEmu.Close()
	}
}
