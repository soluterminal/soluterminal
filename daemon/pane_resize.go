package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"soluterminal/grid"
)

func (p *Pane) Resize(cols, rows uint16) error {
	var ok bool
	cols, rows, ok = saneTerminalDimensions(int(cols), int(rows))
	if !ok {
		return nil
	}
	// Serialize the whole body (dims commit + emulator updates + frame
	// write). Without this, two racing Resize calls could commit dims in
	// order A,B but write FrameResize in order B,A — leaving p.Cols/Rows
	// saying B while the PTY sits at A, and the dedupe early-return below
	// then blocks the corrective resize forever (TUI wraps at the wrong
	// width until the user happens to resize again).
	p.resizeMu.Lock()
	defer p.resizeMu.Unlock()
	p.mu.Lock()
	oldCols, oldRows := p.Cols, p.Rows
	if p.Cols == cols && p.Rows == rows {
		// An applied resize equal to the current dims still proves them: a
		// live client measured its host and landed here, so post-restart
		// persisted dims that happen to match reality are verified too.
		p.dimsVerified = true
		p.mu.Unlock()
		return nil
	}
	p.Cols = cols
	p.Rows = rows
	// A client-driven resize is ground truth — it reflects a real,
	// measured viewport that is about to become the PTY size.
	p.dimsVerified = true
	conn := p.agentConn
	p.mu.Unlock()
	// Persist (debounced) so panes.json tracks the live size; stale
	// persisted dims after a daemon restart were the root of the
	// every-update pane garble.
	if p.onDimsCommit != nil {
		p.onDimsCommit(p)
	}
	if conn == nil {
		return nil
	}

	p.subsMu.Lock()
	p.recordResize(cols, rows)
	// Ghost-cell clear on alt-screen resize, with two audiences:
	//
	//   - The OWNER's xterm runs fit.fit() locally, which resizes its own
	//     cell grid and discards vacated cells — so the owner needs no
	//     clear for small steps, and broadcasting one just flashes their
	//     screen (the visible "text disturbed" artifact). The owner gets a
	//     clear only on a discontinuous jump, where a repaint is expected.
	//
	//   - NON-OWNER viewers render the owner-width TUI into their OWN xterm
	//     grid; on a shrink the TUI repaints fewer columns/rows and nothing
	//     ever overwrites the stale cells in their margin (no SIGWINCH
	//     reaches them at their geometry). They need the clear on EVERY
	//     alt-screen geometry change, or a slow drag of sub-threshold steps
	//     leaves them with permanent ghost cells.
	if p.altScreen {
		clearBytes := []byte("\x1b[2J\x1b[H")
		// resizeClearBytes is the owner/ring decision: non-empty only on a
		// discontinuous jump. Small drag steps return nil here so the owner
		// doesn't flash and the ring isn't polluted.
		ownerClear := len(resizeClearBytes(p.altScreen, oldCols, oldRows, cols, rows)) > 0
		owner := p.ownerSubID
		for id, e := range p.subs {
			if id == owner && !ownerClear {
				continue
			}
			select {
			case e.ch <- clearBytes:
			default:
			}
		}
		if ownerClear {
			p.buffer.Write(clearBytes)
			if vtPaintEnabled && p.vt != nil {
				_, _ = p.vt.Write(clearBytes)
			}
			if p.replayEmu != nil {
				p.replayEmu.Feed(clearBytes)
			}
		}
	}
	if vtPaintEnabled && p.vt != nil {
		p.vt.Resize(int(cols), int(rows))
	}
	if p.replayEmu != nil {
		p.replayEmu.Resize(int(cols), int(rows))
		_ = p.replayEmu.DrainReplies()
	}
	p.subsMu.Unlock()
	// Record where the new width takes effect in the ring so a later reattach
	// replay can trim scrollback that was hard-wrapped at the prior width (see
	// currentWidthRunStart + the stale-width trim in subscribeStream). Only a
	// COLS change matters — row-only changes don't reflow text. The offset is a
	// slight underestimate (a few in-flight old-width bytes may still land after
	// it), which is harmless: the trim re-runs trimReplayHead on the cut.
	if cols != oldCols {
		off := p.buffer.TotalWritten()
		p.widthMarkMu.Lock()
		p.lastWidthOff = off
		p.lastWidthCols = cols
		p.widthMarkMu.Unlock()
	}
	payload, _ := json.Marshal(map[string]uint16{"cols": cols, "rows": rows})
	p.agentWriteMu.Lock()
	err := WriteFrame(conn, FrameResize, payload)
	p.agentWriteMu.Unlock()

	p.scheduleVTPaint(250 * time.Millisecond)
	return err
}

// adoptAgentDims applies the PTY dimensions reported by the long-lived agent
// at (re)attach (FrameDims — the first frame on every new agent connection)
// as this pane's authoritative size.
//
// Why: after a daemon restart (auto-update), AttachExisting rebuilds the pane
// from panes.json, whose dims are only written at spawn/resume-style events
// and drift arbitrarily far from the live PTY (observed live: 283×62
// persisted vs 67×29 real). Until this frame existed, the daemon then LIED
// about the pane size on /api/panes; the post-update page reload made every
// client seed its attach resize with that lie (the anti-transient-shrink seed
// from v0.0.796), blowing the REAL PTY out to the stale width — Claude
// repainted at ~283 cols into ~67-col surfaces and every pane came back
// garbled until a resize jiggle forced a fresh repaint. The agent owns the
// PTY and survives the restart, so its report wins.
//
// Mirrors Resize's bookkeeping except: no FrameResize back to the agent (the
// dims CAME from it), and no width mark (the ring is empty at adoption time —
// the replay about to be consumed carries multi-width history whose
// boundaries we cannot know).
func (p *Pane) adoptAgentDims(cols, rows uint16) {
	cols, rows, ok := saneTerminalDimensions(int(cols), int(rows))
	if !ok {
		return
	}
	p.resizeMu.Lock()
	defer p.resizeMu.Unlock()
	p.mu.Lock()
	oldCols, oldRows := p.Cols, p.Rows
	changed := oldCols != cols || oldRows != rows
	p.Cols = cols
	p.Rows = rows
	p.dimsVerified = true
	p.mu.Unlock()
	if !changed {
		return
	}
	p.subsMu.Lock()
	p.recordResize(cols, rows)
	if vtPaintEnabled && p.vt != nil {
		p.vt.Resize(int(cols), int(rows))
	}
	if p.replayEmu != nil {
		// Size the replay emulator BEFORE the agent's replay bytes feed it
		// (FrameDims precedes FrameReplay on the socket), so the grid paint
		// appended to client replays reflects the real geometry instead of
		// wrapping live-width content at the stale persisted width.
		p.replayEmu.Resize(int(cols), int(rows))
		_ = p.replayEmu.DrainReplies()
	}
	p.subsMu.Unlock()
	log.Printf("pane %s: adopted agent dims %dx%d (had %dx%d)", p.ID, cols, rows, oldCols, oldRows)
	if p.onDimsCommit != nil {
		p.onDimsCommit(p)
	}
}

// currentWidthRunStart returns the ring offset at which the PTY's CURRENT
// width (w) most recently took effect — the start of the trailing run of
// width-w output. Bytes before it were written at a different, now-stale
// width. Because a mark is recorded only when cols actually CHANGES, the last
// recorded mark IS the start of the current width's run, and the existence of
// a mark with cols==w proves there is stale-width content before it. Returns
// -1 when no width change was ever recorded (uniform width — nothing to trim)
// or when the last recorded width disagrees with w (a resize in flight); the
// caller then ships the full raw tail unchanged.
func (p *Pane) currentWidthRunStart(w uint16) int64 {
	p.widthMarkMu.Lock()
	defer p.widthMarkMu.Unlock()
	if p.lastWidthCols == 0 || p.lastWidthCols != w {
		return -1
	}
	return p.lastWidthOff
}

// resizeClearBytes decides whether a resize warrants broadcasting a
// screen clear to subscribers ahead of the TUI's SIGWINCH repaint.
//
// The clear is ghost-cell protection (v0.0.172): after a large geometry
// jump, cells the repainted frame doesn't cover can keep stale content.
// But it is also the single most visible "text disturbed" artifact —
// every applied resize used to blank the pane until the TUI's repaint
// round-tripped. Small continuous steps (splitter drags arrive as ≤
// a-few-cells deltas through the client's 100ms debounce, and the
// subscribe-time bump is a 1-col jiggle) repaint over themselves fine,
// so only discontinuous jumps (≥8 cols / ≥3 rows — same thresholds the
// client uses for codex repair) pay the clear.
func resizeClearBytes(altScreen bool, oldCols, oldRows, cols, rows uint16) []byte {
	if !altScreen {
		return nil
	}
	if !isDiscontinuousResize(oldCols, oldRows, cols, rows) {
		return nil
	}
	return []byte("\x1b[2J\x1b[H")
}

func isDiscontinuousResize(oldCols, oldRows, cols, rows uint16) bool {
	colDelta := absInt(int(cols) - int(oldCols))
	rowDelta := absInt(int(rows) - int(oldRows))
	return colDelta >= 8 || rowDelta >= 3
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (p *Pane) scheduleRedrawSettle(after time.Duration) {
	p.resizeSettleMu.Lock()
	if p.resizeSettleTimer != nil {
		p.resizeSettleTimer.Stop()
	}
	p.resizeSettleTimer = time.AfterFunc(after, func() {
		if p.redrawInProgress.Load() {
			return
		}
		p.Redraw()
	})
	p.resizeSettleMu.Unlock()
}

func (p *Pane) Redraw() {
	if !p.redrawInProgress.CompareAndSwap(false, true) {
		log.Printf("pane %s: Redraw skipped (already in progress)", p.ID)
		return
	}

	p.subsMu.Lock()
	altScreen := p.altScreen
	subCount := len(p.subs)
	var targetCols, targetRows uint16
	if owner, ok := p.subs[p.ownerSubID]; ok && owner.cols > 0 && owner.rows > 0 {
		targetCols = owner.cols
		targetRows = owner.rows
	}
	p.subsMu.Unlock()

	if targetCols == 0 || targetRows == 0 {
		p.mu.Lock()
		targetCols, targetRows = p.Cols, p.Rows
		p.mu.Unlock()
	}
	cols, rows := targetCols, targetRows

	log.Printf("pane %s: Redraw altScreen=%v subs=%d cols=%d rows=%d",
		p.ID, altScreen, subCount, cols, rows)

	ack := fmt.Sprintf(`{"type":"redraw-ack","cols":%d,"rows":%d,"altScreen":%t,"subs":%d}`,
		cols, rows, altScreen, subCount)
	p.subsMu.RLock()
	for _, e := range p.subs {
		select {
		case e.ctrl <- []byte(ack):
		default:
		}
	}
	p.subsMu.RUnlock()

	if !altScreen {
		if p.HasCodexDescendant() || p.HasMuseDescendant() {
			p.RepaintGrid()
		}
		p.redrawInProgress.Store(false)
		return
	}
	if rows < 6 || cols < 2 {
		p.redrawInProgress.Store(false)
		return
	}

	p.subsMu.Lock()
	// Scroll-away + one explicit clear. The per-step resize clears are
	// now gated to discontinuous jumps (resizeClearBytes), and the bump
	// deltas below are all small — so manual redraw injects its own
	// clear once up front to keep its ghost-busting strength.
	scrollSeq := fmt.Sprintf("\x1b[%d;1H%s\x1b[H\x1b[2J\x1b[H",
		rows, strings.Repeat("\n", int(rows)))
	scrollBytes := []byte(scrollSeq)
	p.buffer.Write(scrollBytes)
	for _, e := range p.subs {
		select {
		case e.ch <- scrollBytes:
		default:
		}
	}
	p.subsMu.Unlock()

	go func() {
		defer p.redrawInProgress.Store(false)
		rowsInt := int(rows)
		phaseA := []int{-2, -3, -1, -2, -1}
		for _, d := range phaseA {
			target := rowsInt + d
			if target < 3 {
				target = 3
			}
			_ = p.Resize(cols, uint16(target))
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
		// Re-read the restore target. The bump runs ~1s; if the owner
		// resized the window mid-bump, restoring the dims captured at
		// Redraw start would overwrite their new size with a stale one
		// — and the client (which only sends resize on viewport change)
		// would never resend it.
		cols, rows = p.currentRedrawTarget(cols, rows)
		_ = p.Resize(cols, rows-1)
		time.Sleep(150 * time.Millisecond)
		cols, rows = p.currentRedrawTarget(cols, rows)
		_ = p.Resize(cols, rows)
	}()
}

// currentRedrawTarget returns the owner sub's current dims if an owner
// with sane dims exists, else the provided fallback.
func (p *Pane) currentRedrawTarget(fallbackCols, fallbackRows uint16) (uint16, uint16) {
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	if owner, ok := p.subs[p.ownerSubID]; ok && owner.cols > 1 && owner.rows > 0 {
		return owner.cols, owner.rows
	}
	return fallbackCols, fallbackRows
}

// repaintGridSnapshot returns the current normal-buffer grid snapshot when
// it is paintable: not alt-screen (those TUIs repaint on SIGWINCH), a replay
// emulator exists, and the grid has text. Caller holds subsMu.
func (p *Pane) repaintGridSnapshot() (grid.Snapshot, bool) {
	if p.altScreen || p.replayEmu == nil {
		return grid.Snapshot{}, false
	}
	snap := p.replayEmu.Snapshot()
	if !gridSnapshotHasText(snap) {
		return grid.Snapshot{}, false
	}
	return snap, true
}

// gridPaintForSub builds a synthetic full-screen paint from the replay
// emulator's cell grid for normal-buffer (codex/ratatui) panes, CLAMPED to
// ONE subscriber's geometry. CUP-addressed, so it overwrites in place — no
// WS churn, no ring replay, no SIGWINCH.
//
// The clamp is the fix for the "Codex boxes shatter on a phone" bug: the
// long-lived replayEmu model is at the owner's (possibly wide) cols, so
// emitting its raw CUP coords into a narrower non-owner viewer's xterm wraps
// every over-width box-drawing row and shatters the borders. Clamping each
// row to the sub's cols keeps every CUP coord inside that client's width —
// the same fix the reattach path (pane_subscribe.go) and runSubRepair apply.
// A sub at owner width clamps to a no-op, so desktop viewers are unaffected.
//
// Clamp COLS ONLY (maxRows=0): clamping rows down to the sub's reported
// height dropped the bottom rows — Codex's input box — on a phone whose row
// count lags the live grid (virtual keyboard). Painting all rows is safe;
// xterm clamps any CUP past its own grid. Caller holds subsMu.
func (p *Pane) gridPaintForSub(snap grid.Snapshot, e *subEntry) []byte {
	// Native/mobile terminal subs render the raw stream and keep their own
	// scrollback — a synthetic CUP paint clears that scrollback and pins one
	// frame, so never send them one. Web/PWA mobile clients keep this false
	// because their xterm.js cold replays still need the paint repair.
	if e.skipSyntheticPaint {
		return nil
	}
	skipRow := -1
	if !p.HasCodexDescendant() && !p.HasMuseDescendant() {
		// Live surface: keep model drift away from the TUI's input row
		// (see serializeGridSnapshot).
		skipRow = snap.CursorY
	}
	return serializeGridSnapshotClamped(snap, skipRow, int(e.cols), 0)
}

// RepaintGridFor sends the grid repaint to ONE subscriber. Used by the
// client-driven resize-repair ("repaint" WS message): only the client
// that resized needs its screen rebuilt, and that client may be a
// passive (non-owner) viewer whose resize didn't change PTY size — so
// broadcasting the daemon's emulator reconstruction to every client
// would clobber the owner's already-correct codex screen with a
// possibly-diverged model. Targeting the requester avoids that.
func (p *Pane) RepaintGridFor(subID int64) {
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	snap, ok := p.repaintGridSnapshot()
	if !ok {
		return
	}
	e, ok := p.subs[subID]
	if !ok {
		return
	}
	paint := p.gridPaintForSub(snap, e)
	if len(paint) == 0 {
		return
	}
	select {
	case e.ch <- paint:
	default:
	}
}

// RepaintGrid broadcasts the synthetic paint to every bytes-mode
// subscriber. Used by the user-initiated full Redraw path, where a
// global repaint is the intent. Client-driven resize repair uses
// RepaintGridFor instead. Each sub gets a paint clamped to its OWN
// geometry (see gridPaintForSub) so a narrow phone viewer doesn't get the
// owner-width paint that would shatter its box-drawing.
func (p *Pane) RepaintGrid() {
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	snap, ok := p.repaintGridSnapshot()
	if !ok {
		return
	}
	for _, e := range p.subs {
		paint := p.gridPaintForSub(snap, e)
		if len(paint) == 0 {
			continue
		}
		select {
		case e.ch <- paint:
		default:
		}
	}
}

func (p *Pane) scheduleVTPaint(after time.Duration) {
	if !vtPaintEnabled {
		return
	}
	p.mu.Lock()
	if p.vtPaintTimer != nil {
		p.vtPaintTimer.Stop()
	}
	p.vtPaintTimer = time.AfterFunc(after, p.emitVTPaint)
	p.mu.Unlock()
}

func (p *Pane) emitVTPaint() {
	if !vtPaintEnabled {
		return
	}
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	if !p.altScreen || p.vt == nil {
		return
	}
	payload := serializeVT(p.vt, false)
	if len(payload) == 0 {
		return
	}
	p.buffer.Write(payload)
	for _, e := range p.subs {
		select {
		case e.ch <- payload:
		default:
		}
	}
}
