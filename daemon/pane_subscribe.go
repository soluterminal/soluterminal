package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"soluterminal/grid"
)

// sizingActiveSeq is a process-wide monotonic counter stamped onto a
// subEntry whenever that subscriber becomes the actively-viewed client
// (foregrounded, freshly opened on mobile, or claimed). desiredSizeLocked
// drives the shared PTY size from the VISIBLE sub with the highest stamp
// — i.e. the device the user is actually looking at — so the phone gets
// its narrow width when in use and the desktop its wide width when in use,
// without a passive/backgrounded viewer (or a desktop WebSocket reconnect)
// being able to steal the size. See the active-device sizing model.
var sizingActiveSeq atomic.Int64

// Replay tail caps for new subscribers. The ring holds 8 MiB; replaying
// all of it was the dominant workspace-switch cost (multi-MiB ANSI parse
// per pane on the browser main thread). Alt-screen TUIs repaint every
// cell each frame cycle, so anything beyond the last few repaints is
// pure waste — 256 KiB comfortably covers many full frames at 200×50.
// Normal-buffer panes keep real scrollback, so they get a larger cap
// (~tens of thousands of plain-text lines). Truncated replays get a
// trimReplayHead + replayModePrefix repair, and normal-buffer ones a
// trailing grid repaint of the visible screen; see subscribe().
const (
	replayTailCapAltScreen = 256 * 1024
	replayTailCapNormal    = 2 * 1024 * 1024
)

// trimReplayHead drops the partial leading line of a tail-capped
// snapshot. A cap can cut mid-UTF-8-rune or mid-escape-sequence; xterm
// resynchronises quickly, but the torn head can render a few garbage
// cells before it does. Starting at the first ESC (clean sequence
// boundary) or just after the first newline (clean line boundary) —
// whichever comes first — avoids that. If neither appears in the first
// 4 KiB the snapshot is left as-is.
func trimReplayHead(b []byte) []byte {
	limit := 4096
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		switch b[i] {
		case 0x1b:
			return b[i:]
		case '\n':
			return b[i+1:]
		}
	}
	return b
}

// tailBytes returns at most the last n bytes of b without copying.
func tailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// subEntry tracks one WS subscriber: its byte-fanout channel,
// a control-message channel (for server→client out-of-band
// notifications like ownership changes), the viewport size it last
// reported, and whether the tab is currently visible (foreground).
//
// Sizing rule (recomputeSize / desiredSizeLocked): the PTY follows the
// ACTIVE device — the VISIBLE sub with the highest lastActiveSeq (most
// recently foregrounded / mobile-opened / claimed). So the phone gets
// its narrow width while you're looking at the phone, the desktop its
// wide width while you're on the desktop, and a backgrounded viewer (or
// a desktop WS reconnect) can't steal the size. Ownership (ownerSubID)
// governs only who may WRITE input — it does not decide size.
type subEntry struct {
	ch      chan []byte
	ctrl    chan []byte // text frames — control messages to client
	cols    uint16
	rows    uint16
	visible bool
	// lastActiveSeq stamps when this sub last became the actively-viewed
	// client (foreground transition, mobile cold-open, or claim). 0 =
	// never actively viewed (e.g. a passive desktop sub that only ever
	// reconnected). The visible sub with the max stamp drives PTY size.
	lastActiveSeq int64
	// lagged marks that a PTY payload could not be enqueued because ch
	// was full (slow WS writer — e.g. the cloud relay during an output
	// burst). While lagged, the sub receives no further payloads;
	// laggedFrom records the ring offset where its stream stopped, and
	// once ch drains, fanout sends the exact missed ring bytes from
	// that offset (see fanoutPayloadLocked).
	lagged     bool
	laggedFrom int64
	// repairPending dedupes scheduleSubRepair: true while a repair
	// goroutine for this sub is in flight. Guarded by subsMu.
	repairPending bool
	// isMobile marks a sub that subscribed with mobile=1. It is about sizing:
	// a fresh mobile subscribe is the actively viewed device, so its narrow
	// width should drive the shared PTY.
	isMobile bool
	// skipSyntheticPaint marks native/mobile terminal clients that render the
	// RAW PTY stream themselves and build their own scrollback. They must NOT
	// receive daemon CUP grid-paints (reattach repair / RepaintGrid /
	// sub-repair): those xterm.js repair paints carry no scrollback and can pin
	// a native terminal to one stale frame. Web/PWA phones still need the paints
	// for normal-buffer diff-replay repair; they identify as client=web on the
	// WS URL and keep isMobile=true for sizing but skipSyntheticPaint=false.
	skipSyntheticPaint bool
	// createdAt is when this sub (this WS connection) registered. A claim
	// arriving within claimConnectGrace of it is a connect-time auto-reclaim
	// (every WebView2 reconnect fires one), NOT a deliberate user takeover —
	// so it must not restamp the sizing seq and steal width from an active
	// device. See Claim.
	createdAt time.Time
	// lastSeenAt is bumped on every inbound WS frame AND on every WS pong (see
	// ws.go). desiredSizeLocked skips subs unseen for longer than
	// staleSubWindow so a half-open connection — over the cloud relay the WS
	// ping/pong is stripped, so a vanished phone/desktop lingers until the 90s
	// read deadline — stops driving the shared PTY width while it's effectively
	// dead. Bumped under subsMu, same as the other fields.
	lastSeenAt time.Time
	// deviceID is the client's persistent per-device identity (?device= on the
	// WS URL; localStorage-backed in the web client). Ownership disruption is
	// overwhelmingly a device racing its OWN dead sockets: the desktop reaches
	// the daemon through a multi-hop proxy chain (WebView2 → router → shell →
	// daemon), so an abandoned connection lingers half-open server-side for up
	// to 30–90s while its replacement is denied adoption by "someone else" —
	// which is the same physical device. AdoptIfUnowned uses deviceID to let a
	// device displace its own ghost instantly. Empty for old bundles/probes;
	// empty never matches empty.
	deviceID                string
	retirementViewerID      string
	ownerViewerCleanClosing bool
	// lastInputAt is when this sub last delivered REAL user keystrokes to the
	// PTY (ws.go markInput; ESC-prefixed terminal auto-replies don't count).
	// Zero for pure viewers. Feeds the claim-arbitration guard in
	// ClaimWithReason via the device-level deviceLastInput map.
	lastInputAt time.Time
	// automation marks a sub whose client self-identified as a driven browser
	// (?automation=1, set when navigator.webdriver is true — Playwright,
	// Selenium, headless E2E). Automation subs never drive the shared PTY size
	// (desiredSizeLocked skips them): a 1280×720 test viewport must not shrink
	// the terminal a human is using. Their claims still go through the normal
	// arbitration guard like everyone else's.
	automation bool
}

// deviceLastInput maps deviceID → last time ANY pane received real user
// keystrokes from that device. Device-level (not per-sub, not per-pane) on
// purpose: "is the user at that machine right now?" is the question the
// claim-arbitration guard asks, and a user typing in pane 3 proves they're
// at the keyboard for pane 7 too. Bounded by the number of distinct devices
// that ever connect (a handful), so it never needs eviction.
var deviceLastInput sync.Map

// deviceInputWithin reports whether the given device delivered user input
// anywhere within window. Empty deviceID (old bundles, probes) is never
// considered active — those clients can't be tracked, and the guard treats
// them by visibility alone.
func deviceInputWithin(deviceID string, window time.Duration) bool {
	if deviceID == "" {
		return false
	}
	v, ok := deviceLastInput.Load(deviceID)
	if !ok {
		return false
	}
	t, ok := v.(time.Time)
	return ok && time.Since(t) <= window
}

// claimConnectGrace: a {type:"claim"} this soon after a sub's (re)subscribe
// is treated as a connect-time auto-reclaim — input ownership transfers but
// the sizing seq is NOT restamped, so a reconnecting desktop can't steal the
// PTY width from a phone the user is actively viewing. A genuine "Take over"
// click lands much later (after the user reads the overlay), so it still
// restamps. This guards against old/cached client bundles that don't send
// reason:"reconnect"; the daemon infers it from timing.
const claimConnectGrace = 3 * time.Second

// Subscribe registers a new WS subscriber with its initial viewport
// size. Returns:
//   - snapshot: the current replay buffer for the new client to redraw
//   - ch:       fanout channel receiving subsequent PTY bytes
//   - updateSize: call when the client's viewport changes (e.g. xterm
//     fit on window resize). Triggers a PTY size recompute.
//   - cancel:   removes this subscriber; also recomputes PTY size so
//     the PTY can shrink back when a big client disconnects.
//
// Initial cols/rows may be 0 if the caller doesn't have them yet —
// they simply won't contribute to the PTY size until updateSize.
//
// Ownership: the pane has a single "owner" sub that dictates PTY
// size and can write keystrokes. New subs auto-claim if the pane is
// currently unowned; otherwise they start as passive viewers. Any
// sub can become owner later via Claim(). The returned isOwner()
// closure lets the handler check ownership on each keystroke.
//
// ctrl channel delivers server→client text frames (JSON control
// messages like ownership updates). The WS writer selects on both
// ch (binary bytes) and ctrl (text) and forwards appropriately.
// subMode selects what a new subscriber receives as its initial
// snapshot.
type subMode int

const (
	// subModeFull: full ring replay (tail-capped). Fresh terminals.
	subModeFull subMode = iota
	// subModeNone: no snapshot at all. Legacy skip-replay clients
	// (pre-offset web bundles); they get a deferred quiet-time grid
	// repaint as a best-effort gap repair.
	subModeNone
	// subModeResume: exact resume. The client reports the ring offset
	// it had received up to (`since`); the snapshot is precisely the
	// bytes it missed, straight from the ring — no grid-model paint,
	// no drift exposure, scrollback included. Falls back to
	// subModeFull when the ring no longer holds the gap.
	subModeResume
)

// subStream bundles everything a transport needs to serve one
// subscriber. cutOffset is the ring's TotalWritten at the snapshot
// cut — the client adopts it as its offset baseline so the NEXT
// reconnect can resume exactly. resumed reports whether snapshot is
// an exact-resume slice (apply in place, no terminal reset) rather
// than a full replay (reset + replace).
type subStream struct {
	id               int64
	snapshot         []byte
	resumed          bool
	cutOffset        int64
	ch               <-chan []byte
	ctrl             <-chan []byte
	updateSize       func(cols, rows uint16)
	updateVisibility func(visible bool)
	markSeen         func()
	markActive       func()
	markInput        func()
	isOwner          func() bool
	cancel           func()
}

func (p *Pane) Subscribe(cols, rows uint16) (
	subID int64,
	snapshot []byte,
	ch <-chan []byte,
	ctrl <-chan []byte,
	updateSize func(cols, rows uint16),
	updateVisibility func(visible bool),
	isOwner func() bool,
	cancel func(),
) {
	s := p.SubscribeStream(cols, rows, subModeFull, 0, false)
	return s.id, s.snapshot, s.ch, s.ctrl, s.updateSize, s.updateVisibility, s.isOwner, s.cancel
}

func (p *Pane) SubscribeNoReplay(cols, rows uint16) (
	subID int64,
	snapshot []byte,
	ch <-chan []byte,
	ctrl <-chan []byte,
	updateSize func(cols, rows uint16),
	updateVisibility func(visible bool),
	isOwner func() bool,
	cancel func(),
) {
	s := p.SubscribeStream(cols, rows, subModeNone, 0, false)
	return s.id, s.snapshot, s.ch, s.ctrl, s.updateSize, s.updateVisibility, s.isOwner, s.cancel
}

func (p *Pane) SubscribeStream(cols, rows uint16, mode subMode, since int64, mobile bool) *subStream {
	return p.subscribeStream(cols, rows, mode, since, subOptions{
		mobile:             mobile,
		skipSyntheticPaint: mobile,
	})
}

type subOptions struct {
	initiallyHidden    bool // presentation-only: no sizing vote before attach visibility
	mobile             bool
	skipSyntheticPaint bool
	// deviceID: the client's persistent per-device identity (see
	// subEntry.deviceID). Empty when the client doesn't send one.
	deviceID           string
	retirementViewerID string
	// automation: client self-identified as a driven browser (see
	// subEntry.automation).
	automation bool
	// termCols/termRows: the size of the client's terminal when the replay
	// arrives. It can differ from the cols/rows the client seeds the PTY
	// with (the daemon's own size, to keep attach a sizing no-op). Zero when
	// the client doesn't send it (older clients): the reattach paint then
	// stays unshifted.
	termCols, termRows uint16
}

// detachSubscribers releases every subscriber parked on this pane object and
// refuses future registrations (subsDetached). Called by Manager.Kill after
// the object leaves the manager: from that point no byte can ever reach it,
// so any WS still bound here must be torn down NOW — its client reconnects
// and resolves whatever lives under the pane id next (the respawned pane, or
// a 404 for a genuine close). Without this, a client watching an EXITED or
// SUSPENDED pane through a resume-in-place kept a silently-open socket on the
// orphan while the fresh claude painted to the successor's empty sub map —
// the "blank pane until a keystroke" of feedback #18 (the keystroke's write
// error was what finally closed the zombie).
//
// Idempotent, and safe against markExited (both reset the map under subsMu;
// the second caller sees it empty). ctrl channels are deliberately left
// open-but-silent, mirroring markExited — the WS loop exits on ch close.
func (p *Pane) detachSubscribers() {
	p.subsMu.Lock()
	p.subsDetached = true
	for _, e := range p.subs {
		close(e.ch)
	}
	p.subs = map[int64]*subEntry{}
	p.subsMu.Unlock()
}

func (p *Pane) subscribeStream(cols, rows uint16, mode subMode, since int64, opts subOptions) *subStream {
	hasCodex := mode != subModeNone && (p.HasCodexDescendant() || p.HasMuseDescendant())

	p.subsMu.Lock()
	// A detached object (Manager.Kill already dropped it) can never produce
	// another byte: hand back an already-closed stream so the caller's WS
	// ends immediately and the client's reconnect resolves the SUCCESSOR
	// pane under this id. Registering here instead would park the client on
	// the orphan forever (the resume-blank-pane zombie, feedback #18).
	if p.subsDetached {
		p.subsMu.Unlock()
		closed := make(chan []byte)
		close(closed)
		noop := func() {}
		return &subStream{
			ch:               closed,
			ctrl:             make(chan []byte),
			updateSize:       func(uint16, uint16) {},
			updateVisibility: func(bool) {},
			markSeen:         noop,
			markActive:       noop,
			markInput:        noop,
			isOwner:          func() bool { return false },
			cancel:           noop,
		}
	}
	hadSubs := len(p.subs) > 0
	p.nextSub++
	p.ownerViewerRevision++
	id := p.nextSub
	c := make(chan []byte, 64)
	ctl := make(chan []byte, 8)
	p.subs[id] = &subEntry{
		ch:                 c,
		ctrl:               ctl,
		cols:               cols,
		rows:               rows,
		visible:            !opts.initiallyHidden,
		createdAt:          time.Now(),
		lastSeenAt:         time.Now(),
		isMobile:           opts.mobile,
		skipSyntheticPaint: opts.skipSyntheticPaint,
		deviceID:           opts.deviceID,
		retirementViewerID: opts.retirementViewerID,
		automation:         opts.automation,
	}
	ownerViewerNeedsPersist := p.Role == paneRoleTaskOwner
	if ownerViewerNeedsPersist {
		p.noteOwnerViewerPendingLocked(id, p.subs[id])
	}
	if opts.mobile {
		// A mobile client only subscribes to the single pane it is
		// foregrounding, so a fresh mobile subscribe IS an active view:
		// stamp it so the phone's narrow width wins immediately on cold
		// open. Desktop subscribes (including the frequent Tauri WebView2
		// reconnects) are deliberately NOT stamped here, so a reconnecting
		// desktop can't steal the size back from a phone the user is
		// actively looking at.
		p.subs[id].lastActiveSeq = sizingActiveSeq.Add(1)
	}
	// NO server-side auto-claim. The client decides whether to
	// claim via an explicit {type:"claim"} message after subscribe.
	// Silently auto-claiming on the server meant that when a mobile
	// user switched to a pane the PC wasn't subscribed to, mobile
	// would silently become owner — user saw no overlay and no
	// consent step. With auto-claim gone, panes only change owner
	// when a client explicitly asks.
	currentOwner := p.ownerSubID
	// Snapshot + altScreen captured under the SAME subsMu.Lock that
	// readLoop uses to sequence each payload's (Write, trackModes,
	// fanout). This is the critical invariant: whichever inter-
	// payload boundary Subscribe lands on, the snapshot bytes and
	// the altScreen bit come from the same boundary. Without this,
	// a snapshot that includes a 1049l could pair with a stale
	// altScreen=true (or vice versa), and the prefix decision
	// below would be wrong — exactly the race Codex flagged.
	//
	// Fanout can't interleave either: readLoop's fanout is inside
	// this same lock, so no bytes go to our freshly-inserted sub
	// channel until we release (and those bytes are strictly AFTER
	// our snapshot cut, so no duplication).
	altScreenAtCut := p.altScreen
	cutOffset := p.buffer.TotalWritten()
	var rawSnapshot []byte
	var modePrefix []byte
	var vtPaint []byte
	var gridPaint []byte
	var resumeSlice []byte
	resumed := false
	replayTruncated := false
	if mode == subModeResume {
		// Exact resume: serve precisely the bytes this client missed
		// since `since` (its last known offset). Captured under the
		// same lock as the sub registration, so the slice and the
		// fanout stream are gap- and overlap-free. If the ring no
		// longer holds the gap (long disconnect), fall back to a full
		// replay — the client sees resume=false and resets.
		if since == cutOffset {
			resumed = true // nothing missed; pure reattach
		} else if slice, ok := p.buffer.SnapshotSince(since); ok {
			resumed = true
			resumeSlice = slice
		} else {
			mode = subModeFull
		}
	}
	includeReplay := mode == subModeFull
	if includeReplay {
		// Tail-capped replay. The full ring is 8 MiB; shipping all of
		// it made every pane attach (= every workspace switch-in) copy
		// the ring twice in the daemon and parse multi-MiB of ANSI on
		// the browser main thread per pane — the dominant cost behind
		// slow workspace switches. A bounded tail is enough: for
		// alt-screen TUIs everything before the last full repaint is
		// repainted over anyway, and for normal-buffer panes the cap
		// still preserves thousands of scrollback lines. When the cap
		// actually truncates, trimReplayHead drops the partial leading
		// line/sequence and replayModePrefix re-asserts DEC private
		// modes whose set-sequences may have been cut off.
		capBytes := replayTailCapNormal
		if altScreenAtCut {
			capBytes = replayTailCapAltScreen
		}
		rawSnapshot = p.buffer.SnapshotTail(capBytes)
		if len(rawSnapshot) >= capBytes {
			replayTruncated = true
			rawSnapshot = trimReplayHead(rawSnapshot)
			modePrefix = p.replayModePrefix()
		}
	}
	if includeReplay && vtPaintEnabled && altScreenAtCut && p.vt != nil {
		// VT-based reattach paint. Serialize our cell-grid model of
		// what the terminal currently looks like and ship THAT
		// instead of the raw ringbuf replay. Motivation: the ringbuf
		// is a byte log, and the tail of a long-running Ratatui
		// session is mostly diffs against Codex's own back-buffer —
		// replaying those bytes into a fresh xterm doesn't
		// reconstitute the display because the cells they're
		// diffing against never arrived. VT walks every cell and
		// emits CUP + SGR + char for every position, so the client
		// renders exactly what Codex believes the screen looks like
		// right now. Captured under the same subsMu that readLoop
		// uses when writing to vt, so the paint is consistent with
		// the (buffer, altScreen) pair we just read.
		vtPaint = serializeVT(p.vt, true /*includeAltScreenEnter*/)
	}
	// The grid paint is only ever consumed by the normal-buffer
	// branches below (codex repair / blank-replay fallback) — the
	// alt-screen branch takes precedence and discards it. Skip the
	// O(cols×rows) snapshot + serialization for alt-screen panes; it
	// all runs under subsMu, which stalls readLoop fanout.
	//
	// NOTE: this paint comes from the long-lived replayEmu grid MODEL,
	// which accumulates drift (the recurring "ghost sentence in the
	// input row" was phantom rows in this model being faithfully
	// repainted — v0.0.510 diagnosis). It is used ONLY where the
	// client terminal is fresh and the alternative is worse (truncated
	// replay → jumbled screen). It must NEVER be painted onto a live,
	// healthy surface — that's what exact resume (subModeResume) is
	// for.
	//
	// LOCK DISCIPLINE: replayEmu.Snapshot() (a cols×rows cell-grid walk) is
	// taken HERE, under subsMu, because it is the consistency anchor — it
	// must reflect the terminal at EXACTLY cutOffset / this sub's fanout cut.
	// readLoop advances replayEmu via Feed() only while holding subsMu, so
	// while we hold it no payload can move the grid model past cutOffset;
	// the snapshot and the (cutOffset, fanout-start) boundary are pinned to
	// the same inter-payload point. Taking it after Unlock would let readLoop
	// Feed new bytes between Unlock and Snapshot — those same bytes also go to
	// our just-registered fanout channel, so the paint (at cutOffset+N) plus
	// the deltas (cutOffset+1..) would DOUBLE-APPLY N bytes (overlap). Taking
	// it before registration would instead lose those bytes' visible effect
	// (gap). So Snapshot() stays inside the lock.
	//
	// The EXPENSIVE part — serializeGridSnapshotClamped (per-cell SGR diffing,
	// hex-color formatting, bytes.Buffer growth, strconv) and the
	// gridSnapshotHasText scan — is moved BELOW the Unlock. Snapshot() returns
	// a self-contained value (a copied cell slice + cursor pos), so serializing
	// it later is data-race-free and produces identical bytes; meanwhile
	// readLoop's per-payload fanout is no longer stalled by that work.
	var gridSnap grid.Snapshot
	var haveGridSnap bool
	gridSkipRow := -1
	gridMaxCols, gridMaxRows := 0, 0
	// Skip the synthetic grid paint only for native/mobile terminal subs that
	// render raw PTY themselves. Web/PWA phones still need the paint to repair
	// normal-buffer diff replays on cold-open / full-replay fallback.
	if includeReplay && !altScreenAtCut && p.replayEmu != nil && !opts.skipSyntheticPaint {
		gridSnap = p.replayEmu.Snapshot()
		haveGridSnap = true
		if !hasCodex {
			// The raw tail's own bytes own the input row; the model
			// must not overwrite it (phantom-prompt drift — see
			// serializeGridSnapshot). Captured from the snapshot taken
			// at the cut so it pins to the same cursor position.
			gridSkipRow = gridSnap.CursorY
		}
		// Desktop (non-mobile) subs get the full-model paint (maxCols=0).
		// Mobile subs never reach here (guarded above) — they render the raw
		// stream natively and build their own scrollback.
	}
	p.subsMu.Unlock()

	// Serialize the grid paint OUTSIDE subsMu (see lock-discipline note above).
	// gridSnap was captured at the fanout cut, so the resulting bytes are
	// identical to serializing under the lock — only the readLoop stall is gone.
	if haveGridSnap && gridSnapshotHasText(gridSnap) {
		gridPaint = serializeGridSnapshotClamped(gridSnap, gridSkipRow, gridMaxCols, gridMaxRows)
	}

	// Stale-width scrollback trim. The raw tail is a verbatim byte log with no
	// width metadata: any bytes written while the PTY was a DIFFERENT width are
	// hard-wrapped at that width and can never reflow, so replayed verbatim they
	// become the "narrow scrollback above, correct screen below" artifact (and
	// re-duplicate the bottom screen, which gridPaint repaints anyway). When we
	// have a gridPaint to make the visible screen authoritative, drop the
	// leading run of the tail that predates the current width, keeping only
	// same-width scrollback. Gated to normal-buffer full replays that carry a
	// real paint; a no-op unless a width change actually sits inside the
	// retained tail (currentWidthRunStart > tailStart), so the common
	// no-resize reattach is untouched.
	if includeReplay && !altScreenAtCut && len(gridPaint) > 0 && len(rawSnapshot) > 0 {
		if runStart := p.currentWidthRunStart(uint16(gridSnap.Cols)); runStart >= 0 {
			tailStart := cutOffset - int64(len(rawSnapshot))
			if runStart > tailStart {
				drop := int(runStart - tailStart)
				if drop >= len(rawSnapshot) {
					rawSnapshot = nil
				} else {
					rawSnapshot = trimReplayHead(rawSnapshot[drop:])
				}
				// The tail no longer starts at a self-contained boundary; re-assert
				// DEC private modes the dropped prefix may have set.
				if len(modePrefix) == 0 {
					modePrefix = p.replayModePrefix()
				}
			}
		}
	}

	// Height-mismatch alignment. The paint below is CUP-addressed in the
	// model's rows, but the client's xterm may be taller or shorter than the
	// model (a first attach before this client has resized the PTY, or a
	// viewer that never drives the size). The raw tail then leaves the client's
	// cursor on a different row than the model's, and a top-anchored paint left
	// the extra bottom rows holding a second copy of the last lines. Shift the
	// paint so the model's cursor row lands on the client's. Only the
	// skip-row (non-Codex) paint is shifted; equal heights are untouched.
	if len(gridPaint) > 0 && gridSkipRow >= 0 && len(rawSnapshot) > 0 {
		replay := make([]byte, 0, len(modePrefix)+len(rawSnapshot))
		replay = append(replay, modePrefix...)
		replay = append(replay, rawSnapshot...)
		if off := reattachPaintRowOffset(int(opts.termCols), int(opts.termRows), replay, gridSnap); off != 0 {
			gridPaint = serializeGridSnapshotAt(gridSnap, gridSkipRow, gridMaxCols, gridMaxRows, off, int(opts.termRows))
		}
	}

	// Reattach snapshot selection:
	//   - alt-screen, VT available → synthetic full paint from VT.
	//   - alt-screen, VT missing → raw replay with 1049h prefix
	//     (prior behavior; kept as a safety fallback).
	//   - normal mode → raw ringbuf replay, no prefix (preserves
	//     shell scrollback).
	var snapshot []byte
	if resumed {
		// Exact resume: the slice (possibly empty) is byte-for-byte
		// what the client missed. Applied onto its kept surface, the
		// result is identical to never having disconnected.
		snapshot = resumeSlice
	} else if mode == subModeNone {
		// Legacy skip-replay (client sent no offset): the client keeps
		// its surface and the gap bytes are unrecoverable for it. A
		// quiet-time grid repaint is scheduled below as best effort.
		snapshot = nil
	} else if len(vtPaint) > 0 {
		snapshot = vtPaint
	} else if altScreenAtCut {
		const enterAlt = "\x1b[?1049h"
		snapshot = make([]byte, 0, len(enterAlt)+len(modePrefix)+len(rawSnapshot))
		snapshot = append(snapshot, enterAlt...)
		snapshot = append(snapshot, modePrefix...)
		snapshot = append(snapshot, rawSnapshot...)
	} else if len(gridPaint) > 0 {
		// Raw tail for scrollback, then a grid repaint to repair the
		// visible screen. EVERY normal-buffer replay gets this now, not
		// just truncated ones (v0.0.503 fixed only truncated; the
		// untruncated path still shipped raw-only and jumbled the same
		// way for animation-heavy panes — worst during multi-subagent
		// runs whose tail is almost all relative-cursor diff frames with
		// no self-contained anchor frame). The grid repaint makes the
		// visible screen authoritative; the raw tail still seeds
		// scrollback; skipRow keeps the model off the TUI's input row
		// (grid-emulator drift guard). Codex needs it for the same reason
		// (its byte stream diffs against cells the fresh xterm never
		// received). A blank pane's whitespace raw tail is harmless ahead
		// of the paint.
		snapshot = make([]byte, 0, len(modePrefix)+len(rawSnapshot)+len(gridPaint))
		snapshot = append(snapshot, modePrefix...)
		snapshot = append(snapshot, rawSnapshot...)
		snapshot = append(snapshot, gridPaint...)
	} else if len(modePrefix) > 0 {
		snapshot = make([]byte, 0, len(modePrefix)+len(rawSnapshot))
		snapshot = append(snapshot, modePrefix...)
		snapshot = append(snapshot, rawSnapshot...)
	} else {
		snapshot = rawSnapshot
	}

	// Quiet-time grid repaint ONLY for legacy skip-replay clients that
	// can't do exact resume — best-effort gap repair, model-drift risk
	// accepted because the alternative is a permanently stale surface.
	// The v0.0.509 "post-replay settle" paint is gone: re-delivering
	// model content after a full replay added nothing the inline paint
	// hadn't already provided, and doubled the ghost exposure.
	if mode == subModeNone {
		p.scheduleSubRepair(id, "legacy skip-replay reconnect")
	}

	// Tell this sub immediately whether they're the owner — the
	// initial control frame lands right after the replay snapshot.
	p.sendOwnership(id, currentOwner == id)

	// Recompute the PTY size for the new subscriber set. The size follows
	// the active device (desiredSizeLocked): a mobile cold-open was just
	// stamped active above, so the phone's width takes effect immediately;
	// a desktop (re)subscribe is NOT stamped, so it can't shrink/steal the
	// size from a phone the user is actively viewing.
	p.recomputeSize()

	// Fresh-subscribe SIGWINCH bump for alt-screen TUIs whose diff-only
	// ringbuf tail can't reconstruct the screen on replay alone: a small
	// size bump (cols-1 → cols) forces SIGWINCH, which alt-screen TUIs
	// usually answer with a full repaint.
	//
	// Do NOT run the bump for normal-buffer Codex. Closing an overall
	// browser tile changes the root layout shape and can remount pane
	// components; the old unconditional bump then hit Linux Codex's
	// inline DECSTBM/sync-update renderer and produced duplicate input
	// widgets. Normal-buffer panes now rely on raw replay + the real
	// client resize event instead of an artificial subscribe-time bump.
	//
	// (The old unowned size-adoption that lived here — p.Resize with THIS
	// sub's own dims — is gone: recomputeSize above sizes the PTY to the
	// active device, so an unowned pane is sized correctly without letting
	// a passive reattacher silently drive the size.)
	if cols > 1 && rows > 0 {
		p.mu.Lock()
		curCols, curRows := p.Cols, p.Rows
		dimsOK := p.dimsVerified
		p.mu.Unlock()
		// The bump (cols-1 → cols SIGWINCH jiggle) coaxes a full repaint
		// out of an alt-screen TUI whose replay alone can't reconstruct
		// the screen. Fire it when EITHER:
		//   - the pane was previously unwatched (hadSubs false): a freshly
		//     resumed pane's ring tail may be stale diffs; or
		//   - the replay was tail-truncated: the diffs the last 256 KiB
		//     reference were cut off, so the new client painted only a
		//     partial frame and needs a SIGWINCH repaint to fill the rest.
		// We deliberately do NOT bump on the common case — a warm
		// workspace switch reattaching a pane whose full current frame
		// fits in the (untruncated) tail — which is what made every
		// switch-in double-repaint every Claude pane.
		// dimsOK gate: after a daemon restart with an old (pre-FrameDims)
		// agent, curCols is the stale persisted width — bumping would resize
		// the REAL PTY to it (the same disease as the client-seed garble).
		// The subscriber's own resize lands moments later and alt-screen
		// TUIs repaint on that SIGWINCH anyway.
		if altScreenAtCut && !resumed && (!hadSubs || replayTruncated) && curCols > 1 && curRows > 0 && dimsOK {
			_ = p.Resize(curCols-1, curRows)
			_ = p.Resize(curCols, curRows)
		}
	}

	updateSize := func(newCols, newRows uint16) {
		if newCols == 0 || newRows == 0 {
			return
		}
		p.subsMu.Lock()
		if e, ok := p.subs[id]; ok {
			prevCols := e.cols
			e.cols = newCols
			e.rows = newRows
			e.lastSeenAt = time.Now()
			// Active-widen reclaim. A VISIBLE, non-mobile sub that grows
			// materially WIDER is the user maximizing / expanding the window
			// they're actively looking at — re-stamp it active so it overtakes a
			// stale or background-tile sub that stamped earlier and is pinning
			// the shared PTY narrow (the "full-screen pane stuck at 65" bug).
			// Mobile is excluded: a phone widening is a keyboard close, not a
			// maximize, and it already has active precedence below. The
			// widenReclaimCols threshold ignores 1-col fit jitter and the
			// connect-seed no-op.
			if e.visible && !e.isMobile && prevCols != 0 && newCols >= prevCols+widenReclaimCols {
				e.lastActiveSeq = sizingActiveSeq.Add(1)
			}
		}
		p.subsMu.Unlock()
		p.recomputeSize()
	}

	updateVisibility := func(visible bool) {
		p.subsMu.Lock()
		if e, ok := p.subs[id]; ok {
			if visible && !e.visible {
				// Foreground transition (hidden → visible): the user just
				// brought this view to the front, so it becomes the active
				// device that drives the PTY size. A bare reconnect that
				// was already visible does NOT re-stamp (no transition).
				e.lastActiveSeq = sizingActiveSeq.Add(1)
			}
			e.visible = visible
		}
		p.subsMu.Unlock()
		// Visibility + active-stamp feed the sizing rule (desiredSizeLocked
		// picks the most-recently-active VISIBLE sub). Recompute so the
		// width follows the device the user is now looking at: foreground
		// the phone → narrow; background it → the desktop takes width back.
		p.recomputeSize()
	}

	// markSeen bumps this sub's liveness on any inbound WS frame or pong, so
	// desiredSizeLocked's stale-skip only drops genuinely silent (half-open)
	// connections. Cheap leaf-lock write.
	markSeen := func() {
		p.subsMu.Lock()
		if e, ok := p.subs[id]; ok {
			e.lastSeenAt = time.Now()
		}
		p.subsMu.Unlock()
	}

	// markActive re-stamps this sub as the actively-viewed device for SIZING
	// (it wins desiredSizeLocked) without transferring input ownership. The web
	// client fires it when this pane becomes the focused, on-screen pane
	// (window/tab focus) so the view the user is actually looking at reclaims
	// the shared PTY width from a background-tile or stale sub. Distinct from
	// {type:"claim"} (ownership) and from the foreground-transition restamp in
	// updateVisibility (which only fires on a hidden→visible edge).
	markActive := func() {
		p.subsMu.Lock()
		if e, ok := p.subs[id]; ok {
			e.lastSeenAt = time.Now()
			e.lastActiveSeq = sizingActiveSeq.Add(1)
		}
		p.subsMu.Unlock()
		p.recomputeSize()
	}

	// markInput records that this sub delivered real user keystrokes to the
	// PTY — per-sub and per-device (deviceLastInput). The claim-arbitration
	// guard in ClaimWithReason reads the device-level stamp to answer "is the
	// owner's user at their keyboard?", so an automatic claim from another
	// device can't silently steal a pane from someone actively working —
	// even when they're typing in a DIFFERENT pane of the same workspace.
	markInput := func() {
		p.subsMu.Lock()
		e, ok := p.subs[id]
		var device string
		if ok {
			e.lastInputAt = time.Now()
			device = e.deviceID
		}
		p.subsMu.Unlock()
		if device != "" {
			deviceLastInput.Store(device, time.Now())
		}
	}

	isOwner := func() bool {
		p.subsMu.RLock()
		defer p.subsMu.RUnlock()
		return p.ownerSubID == id
	}

	cancel := func() {
		cleanViewerRemoved := false
		p.subsMu.Lock()
		if existing, ok := p.subs[id]; ok {
			delete(p.subs, id)
			if existing.ownerViewerCleanClosing {
				key := ownerViewerDeviceKey(id, existing)
				remaining := false
				for otherID, other := range p.subs {
					if ownerViewerDeviceKey(otherID, other) == key {
						remaining = true
					}
				}
				if !remaining {
					delete(p.ownerViewerPendingDevices, key)
					cleanViewerRemoved = true
				}
			}
			p.ownerViewerRevision++
			close(existing.ch)
			close(existing.ctrl)
		}
		// If the departing sub was owner, the pane becomes unowned.
		// We deliberately do NOT auto-transfer to the next-oldest
		// sub — that was another silent-takeover path. The pane's
		// PTY keeps its last-known size; any remaining client can
		// explicitly claim via {type:"claim"} when they want to
		// interact.
		var orphaned []int64
		if p.ownerSubID == id {
			p.ownerSubID = 0
			for sid := range p.subs {
				orphaned = append(orphaned, sid)
			}
			log.Printf("ownership: pane=%s owner sub=%d departed — pane unowned (%d viewers notified)",
				p.ID, id, len(orphaned))
		}
		p.subsMu.Unlock()
		if cleanViewerRemoved && p.onOwnerViewersCommit != nil {
			if err := p.onOwnerViewersCommit(p); err != nil {
				p.subsMu.Lock()
				p.ownerViewerKnowledgeKnown = false
				p.subsMu.Unlock()
			}
		}
		// Tell the remaining viewers the pane is now UNOWNED. Their owned
		// bool doesn't change, but the owner:"none" state lets their lock
		// overlay say "no device has control" instead of the false "in use
		// somewhere else", and marks the pane adoptable (AdoptIfUnowned).
		for _, sid := range orphaned {
			p.sendOwnership(sid, false)
		}
		p.recomputeSize()
	}
	// Persist a possible unsent draft before this new socket can receive input
	// or acknowledge empty state. Failure keeps lifecycle uncertainty visible.
	if ownerViewerNeedsPersist && (p.onOwnerViewersCommit == nil || p.onOwnerViewersCommit(p) != nil) {
		p.subsMu.Lock()
		p.ownerViewerKnowledgeKnown = false
		p.subsMu.Unlock()
	}
	return &subStream{
		id:               id,
		snapshot:         snapshot,
		resumed:          resumed,
		cutOffset:        cutOffset,
		ch:               c,
		ctrl:             ctl,
		updateSize:       updateSize,
		updateVisibility: updateVisibility,
		markSeen:         markSeen,
		markActive:       markActive,
		markInput:        markInput,
		isOwner:          isOwner,
		cancel:           cancel,
	}
}

// Claim transfers ownership to the given sub. Broadcasts ownership
// change notifications to every sub so their UIs can update (the
// newly-unclaimed side shows the "take over" overlay, the newly-
// claimed side unlocks input). Also triggers a size recompute —
// the new owner's cols become the PTY dims.
// restampSize controls whether the claim also makes the claimer the
// active-sizing device. A genuine user takeover stamps (its geometry wins
// the PTY size); a desktop reconnect reclaim does NOT — otherwise every
// WebView2 reconnect would steal the width back from a phone the user is
// actively viewing (the 84↔49 thrash).
func (p *Pane) Claim(subID int64, restampSize bool) bool {
	return p.ClaimWithReason(subID, restampSize, "")
}

func (p *Pane) ClaimWithReason(subID int64, restampSize bool, reason string) bool {
	p.subsMu.Lock()
	claimer, ok := p.subs[subID]
	if !ok {
		p.subsMu.Unlock()
		return false
	}
	previousOwner := p.ownerSubID
	if previousOwner == subID {
		p.subsMu.Unlock()
		return true // already owner — no-op but success
	}
	// ── Claim arbitration ─────────────────────────────────────────────
	// Not every claim is a user saying "I'm driving now". The web client
	// auto-claims on first open, desktops re-assert on reconnect, the
	// Flutter app re-sends a bare claim on every reconnect, and driven
	// browsers (E2E suites, MCP Playwright) fire the same first-open
	// auto-claim as a real user — each with a fresh deviceID. Granting all
	// of those unconditionally was the silent-steal class behind "in use
	// somewhere else" overlays with no second device in sight. Every
	// transport funnels through here, so one gate covers them all: an
	// AUTOMATIC claim is denied while the current owner is live on ANOTHER
	// device and its user is plausibly at that device — the pane is on its
	// screen, or that device typed anywhere recently. Explicit human
	// takeovers still always win: reason:"takeover" (the overlay button),
	// or a bare/legacy claim arriving well after connect (the Flutter
	// Take-over tap and old web bundles' button — automatic re-claims fire
	// AT connect, human taps land later; same inference claimConnectGrace
	// already relies on for sizing).
	explicitClaim := reason == "takeover" ||
		(reason == "" && time.Since(claimer.createdAt) >= claimConnectGrace)
	if !explicitClaim && previousOwner != 0 {
		if owner, ownerAlive := p.subs[previousOwner]; ownerAlive &&
			time.Since(owner.lastSeenAt) <= staleOwnerThreshold &&
			!(owner.deviceID != "" && owner.deviceID == claimer.deviceID) &&
			(owner.visible || deviceInputWithin(owner.deviceID, ownerDeviceActiveWindow)) {
			p.subsMu.Unlock()
			log.Printf("ownership: pane=%s claim reason=%q DENIED sub=%d device=%q — live owner sub=%d device=%q visible=%v seen=%s ago",
				p.ID, reason, subID, claimer.deviceID, previousOwner, owner.deviceID,
				owner.visible, time.Since(owner.lastSeenAt).Round(time.Millisecond))
			p.sendOwnership(subID, false) // truthful denial ack (owner:"other")
			return false
		}
	}
	p.ownerSubID = subID
	// Belt-and-suspenders against width theft: old/cached bundles that don't
	// send reason:"reconnect" arrive as bare/ambiguous claims. If one lands
	// within claimConnectGrace of connect, treat it as a connect-time
	// auto-reclaim: grant input ownership but do NOT restamp sizing, so a
	// reconnecting desktop can't steal the PTY width from an active device.
	//
	// New browser clients mark legitimate sizing intent explicitly
	// ("first-open" for desktop first attach, "takeover" for the overlay
	// button). Those must bypass the grace window; otherwise two desktop
	// clients on the same account can both remain seq=0 and the shared PTY
	// falls back to "widest visible subscriber" instead of the device that
	// actually took over.
	explicitSizingClaim := reason == "first-open" || reason == "takeover"
	if restampSize && !explicitSizingClaim && time.Since(claimer.createdAt) < claimConnectGrace {
		restampSize = false
	}
	// A user takeover is an explicit "I'm driving this now" — stamp the
	// claimer active so its geometry wins the size (e.g. a phone "Take over"
	// goes narrow). A reconnect reclaim skips the stamp so it can't steal the
	// size from the active device.
	if restampSize {
		claimer.lastActiveSeq = sizingActiveSeq.Add(1)
	}
	// Snapshot sub IDs to notify outside the lock so sendOwnership
	// doesn't reenter.
	allIDs := make([]int64, 0, len(p.subs))
	for sid := range p.subs {
		allIDs = append(allIDs, sid)
	}
	deviceID := claimer.deviceID
	p.subsMu.Unlock()

	log.Printf("ownership: pane=%s claim reason=%q owner %d -> %d device=%q",
		p.ID, reason, previousOwner, subID, deviceID)
	for _, sid := range allIDs {
		p.sendOwnership(sid, sid == subID)
	}
	p.recomputeSize()
	return true
}

// sendOwnership delivers a {type:"ownership", owned, owner} control
// frame to the given sub. owner distinguishes the two owned:false worlds:
// "other" (a peer sub really holds input) vs "none" (nobody does — the
// daemon restarted or the owner disconnected). The distinction lets
// clients render truthful lock copy and lets a pane's previous owner
// re-adopt it safely (AdoptIfUnowned) instead of showing "in use
// somewhere else" when no one is using it. Old clients ignore the extra
// field. Non-blocking: if the sub's ctrl channel is full (client slow),
// drop silently — they'll get the next one. The read lock is held across
// the (non-blocking) send: a departing sub's cancel closes e.ctrl under the
// write lock, so sending after releasing it raced that close.
func (p *Pane) sendOwnership(subID int64, owned bool) {
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	e, ok := p.subs[subID]
	hasOwner := p.ownerSubID != 0
	if !ok {
		return
	}
	var payload []byte
	switch {
	case owned:
		payload = []byte(`{"type":"ownership","owned":true,"owner":"you"}`)
	case hasOwner:
		payload = []byte(`{"type":"ownership","owned":false,"owner":"other"}`)
	default:
		payload = []byte(`{"type":"ownership","owned":false,"owner":"none"}`)
	}
	select {
	case e.ctrl <- payload:
	default:
	}
}

// staleOwnerThreshold: an owner sub with no inbound frame for this long is
// treated as a ghost for adoption purposes. Every first-party client
// heartbeats well inside it — the web bundle sends an app-level
// {type:"ping"} every 25s, the Flutter app every 30s, and local browsers
// additionally auto-pong the server's 30s WS pings — so a LIVE owner's
// lastSeenAt can never age past ~35s. Anything older is a connection whose
// client is gone but whose TCP lingers half-open (the desktop reaches the
// daemon through WebView2 → router → shell proxy hops, which can absorb a
// close for the full 90s read deadline).
const staleOwnerThreshold = 45 * time.Second

// ownerDeviceActiveWindow: an automatic claim from another device is denied
// while the current owner's DEVICE delivered user keystrokes (to any pane)
// this recently — the user is at that keyboard even if this particular pane
// is on a background workspace. Generous on purpose: the failure mode of
// too-short is a pocketed phone or test browser silently stealing a pane
// mid-work-session; the failure mode of too-long is one truthful "Take
// over" click when deliberately switching devices after a long idle.
const ownerDeviceActiveWindow = 10 * time.Minute

// AdoptIfUnowned transfers ownership to subID when the pane is safely
// adoptable. This is the conditional reclaim for connection drops the
// restore path never sees — above all the daemon-update restart, which
// wipes every pane's in-memory ownerSubID while the PTYs (detached agent
// procs) live on. Granted when:
//   - nobody owns the pane (the post-restart state), or
//   - the current owner is a sub from the SAME device as the requester —
//     that owner is by definition the requester's own abandoned socket
//     (page reload, WebView2 reconnect, proxy half-open); a device racing
//     its own ghost must never lock itself out, or
//   - the current owner has been silent past staleOwnerThreshold — a ghost
//     from any device; live clients heartbeat every 25–30s.
//
// Unlike reason:"reconnect" (unconditional), adoption can never undo a
// takeover by a LIVE peer on another device: in that case the adopt is
// denied and the requester gets a truthful owner:"other" frame as its ack
// (its claim-pending UI state settles into the locked overlay). Never
// restamps the sizing seq — adoption is continuity repair, not a user
// gesture.
func (p *Pane) AdoptIfUnowned(subID int64) bool {
	p.subsMu.Lock()
	requester, ok := p.subs[subID]
	if !ok {
		p.subsMu.Unlock()
		return false
	}
	if p.ownerSubID == subID {
		p.subsMu.Unlock()
		return true // already owner — no-op but success
	}
	previousOwner := p.ownerSubID
	grant := "unowned"
	if previousOwner != 0 {
		owner, ownerAlive := p.subs[previousOwner]
		switch {
		case !ownerAlive:
			// Defensive: ownerSubID should be cleared by cancel(), but if it
			// ever dangles, the pane is de-facto unowned.
			grant = "owner-gone"
		case owner.deviceID != "" && owner.deviceID == requester.deviceID:
			grant = "same-device"
		case time.Since(owner.lastSeenAt) > staleOwnerThreshold:
			grant = "stale-owner"
		default:
			p.subsMu.Unlock()
			log.Printf("ownership: pane=%s adopt DENIED sub=%d device=%q — live owner sub=%d device=%q seen=%s ago",
				p.ID, subID, requester.deviceID, previousOwner, owner.deviceID,
				time.Since(owner.lastSeenAt).Round(time.Millisecond))
			p.sendOwnership(subID, false) // denial ack (owner:"other")
			return false
		}
	}
	p.ownerSubID = subID
	allIDs := make([]int64, 0, len(p.subs))
	for sid := range p.subs {
		allIDs = append(allIDs, sid)
	}
	p.subsMu.Unlock()

	log.Printf("ownership: pane=%s adopt granted (%s) owner %d -> %d device=%q",
		p.ID, grant, previousOwner, subID, requester.deviceID)
	for _, sid := range allIDs {
		p.sendOwnership(sid, sid == subID)
	}
	p.recomputeSize()
	return true
}

// AdoptOnInput makes subID the owner when it types into a pane NOBODY owns
// while it is visible. It is the daemon-side backstop for clients that lost
// their claim across a reconnect (every relayed socket drops on a cloud
// deploy, leaving each pane unowned) and kept typing into the void.
//
// Stricter than AdoptIfUnowned on purpose: it never displaces an owner — not
// a same-device ghost, not a stale one — so it can never undo a takeover.
// The caller (handleWS) only calls it for sockets the daemon classified as a
// human (callerHuman); the visibility flag only chooses WHICH human socket
// adopts. Check and set happen under one subsMu critical section, so when
// two viewers type at once exactly one wins; the other gets ("owned-by-other")
// and its keystrokes are dropped. Never restamps the sizing seq.
// Returns whether subID now owns the pane, and the drop reason when not.
func (p *Pane) AdoptOnInput(subID int64) (bool, string) {
	p.subsMu.Lock()
	requester, ok := p.subs[subID]
	switch {
	case !ok:
		p.subsMu.Unlock()
		return false, "viewer-gone"
	case p.ownerSubID == subID:
		p.subsMu.Unlock()
		return true, ""
	case p.ownerSubID != 0:
		p.subsMu.Unlock()
		return false, "owned-by-other"
	case !requester.visible:
		p.subsMu.Unlock()
		return false, "unowned-viewer-hidden"
	}
	p.ownerSubID = subID
	allIDs := make([]int64, 0, len(p.subs))
	for sid := range p.subs {
		allIDs = append(allIDs, sid)
	}
	deviceID := requester.deviceID
	p.subsMu.Unlock()

	log.Printf("ownership: pane=%s adopt granted (unowned, on input) owner 0 -> %d device=%q",
		p.ID, subID, deviceID)
	for _, sid := range allIDs {
		p.sendOwnership(sid, sid == subID)
	}
	p.recomputeSize()
	return true, ""
}

// desiredSizeLocked returns the PTY geometry for the current subscriber
// set: the dims of the ACTIVE device — the VISIBLE sub with the highest
// lastActiveSeq (most recently foregrounded / mobile-opened / claimed).
//
// Rationale: a shared PTY has ONE width, and a phone and a desktop want
// different widths, so only one can win at a time. Picking the device
// the user is actively looking at means the phone gets its narrow width
// while it's in use (readable on mobile) and the desktop its wide width
// while it's in use. Two passive guards keep it stable: only VISIBLE
// subs count (a backgrounded phone can't shrink a desktop you're using),
// and a desktop WS reconnect is never auto-stamped active (it can't
// steal the size back from a phone you're actively viewing). The cost is
// the inherent one: switching phone→desktop leaves a narrow patch in the
// desktop's scrollback (hard-wrapped TUI bytes can't reflow) until it
// ages out.
//
// Ties (multiple visible subs with the same stamp, e.g. two desktop
// windows that never transitioned) break toward the wider sub. When no
// visible sub reports dims, fall back to the most-recently-active sub
// among ALL subs so we don't spuriously resize a pane whose viewers are
// momentarily hidden. Returns (0,0) when no sub has dims → caller leaves
// the PTY unchanged. Caller holds subsMu.
func (p *Pane) desiredSizeLocked() (uint16, uint16) {
	var visBest, anyBest *subEntry
	mobileVisible := false
	hasVisibleSub := false
	now := time.Now()
	for _, e := range p.subs {
		// Skip subs we haven't heard from in a while. A half-open WS lingers
		// until the 90s read deadline — and over the cloud relay the WS
		// ping/pong is stripped, so a vanished phone/desktop can linger even
		// longer — during which its (often narrow) dims would keep driving the
		// shared PTY. lastSeenAt is bumped on every inbound frame and pong.
		if !e.lastSeenAt.IsZero() && now.Sub(e.lastSeenAt) > staleSubWindow {
			continue
		}
		// Driven browsers (Playwright/E2E, navigator.webdriver clients) never
		// vote on the shared PTY size: a default 1280×720 test viewport must
		// not shrink the terminal a human is looking at.
		if e.automation {
			continue
		}
		if e.visible {
			hasVisibleSub = true
		}
		if e.cols == 0 || e.rows == 0 {
			continue
		}
		if e.visible {
			if visBest == nil || moreActiveForSizing(e, visBest) {
				visBest = e
			}
			if e.isMobile {
				mobileVisible = true
			}
		} else if anyBest == nil || moreActiveForSizing(e, anyBest) {
			// anyBest considers ONLY hidden subs now; it is consulted solely as
			// the no-visible-sub fallback below.
			anyBest = e
		}
	}

	p.mobileSizeMu.Lock()
	defer p.mobileSizeMu.Unlock()
	p.lastSizeHeld = false
	if visBest != nil {
		if visBest.isMobile {
			// A phone is the active viewer: it wins, and we remember its
			// width to bridge its own reconnect gaps (below).
			p.lastMobileCols, p.lastMobileRows = visBest.cols, visBest.rows
			p.lastMobileAt = time.Now()
			return visBest.cols, visBest.rows
		}
		// The winner is a non-mobile (desktop) sub. If a phone was the active
		// sizer moments ago and no phone is visible right now, this is almost
		// certainly a reconnect GAP, not a deliberate switch to the desktop —
		// hold the phone width so churn can't flip Codex desktop-wide on the
		// phone. The hold ages out (mobileSizeHoldWindow), so a genuine
		// phone→desktop handoff still settles to the desktop width shortly.
		if !mobileVisible && p.lastMobileCols != 0 &&
			time.Since(p.lastMobileAt) < mobileSizeHoldWindow {
			p.lastSizeHeld = true
			return p.lastMobileCols, p.lastMobileRows
		}
		return visBest.cols, visBest.rows
	}
	// A visible viewer exists but none reports usable dims yet (warm-host
	// re-adopt / fit pending / mid-layout). HOLD the current PTY size (return
	// 0,0 → caller leaves it unchanged) rather than adopting a hidden or
	// background sub's narrow width — that adoption is exactly how a parked grid
	// tile shrank an actively-viewed pane to 65 cols.
	if hasVisibleSub {
		return 0, 0
	}
	// No visible sub at all — every viewer is momentarily hidden. Fall back to
	// the most-recently-active hidden sub so a brief all-hidden blip doesn't
	// spuriously resize the PTY.
	if anyBest != nil {
		return anyBest.cols, anyBest.rows
	}
	return 0, 0
}

// mobileSizeHoldWindow is how long a phone's last applied width survives a
// gap with no visible mobile sub before desiredSizeLocked falls back to a
// desktop sub. Comfortably longer than the native client's reconnect backoff
// (500ms→5s) so ordinary churn never escapes it, short enough that a real
// phone→desktop switch settles within a few seconds.
const mobileSizeHoldWindow = 8 * time.Second

// staleSubWindow is how long a sub may go with NO inbound frame or pong before
// desiredSizeLocked stops counting its vote. Comfortably longer than the 30s WS
// ping cadence and the web client's ~25s app-level keepalive, so a live-but-idle
// viewer is never dropped; shorter than the 90s read deadline, so a half-open
// connection (e.g. a vanished phone behind the relay) stops driving the shared
// PTY width well before it is finally reaped.
const staleSubWindow = 75 * time.Second

// widenReclaimCols is the minimum cols increase that counts as a deliberate
// maximize/widen (rather than 1-col fit jitter or the connect-seed no-op) for
// the active-widen reclaim in updateSize. Mirrors the discontinuous-resize
// threshold used elsewhere.
const widenReclaimCols = 8

// sizingMobileLog returns a compact one-line dump of the sizing decision,
// but ONLY when a phone is in the picture (a visible mobile sub, or the
// reconnect-gap hold is currently active). Returns "" otherwise so
// non-mobile panes stay silent. The tag reveals the decision path:
// "active" (a phone won outright), "HELD-gap" (a phone reconnect gap is
// being bridged — the fingerprint of the churn the hold fixes), or
// "desktop" (fell through to a desktop sub). Caller holds subsMu (read);
// the hold flag is read under mobileSizeMu (a leaf lock).
func (p *Pane) sizingMobileLog(cols, rows uint16) string {
	mobileVisible := false
	for _, e := range p.subs {
		if e.isMobile && e.visible && e.cols != 0 && e.rows != 0 {
			mobileVisible = true
			break
		}
	}
	p.mobileSizeMu.Lock()
	held := p.lastSizeHeld
	p.mobileSizeMu.Unlock()
	if !mobileVisible && !held {
		return ""
	}
	tag := "active"
	if held {
		tag = "HELD-gap"
	} else if !mobileVisible {
		tag = "desktop"
	}
	parts := make([]string, 0, len(p.subs)+1)
	parts = append(parts, fmt.Sprintf("chose %dx%d (%s)", cols, rows, tag))
	for id, e := range p.subs {
		kind := "desk"
		if e.isMobile {
			kind = "phone"
		}
		vis := ""
		if !e.visible {
			vis = " hidden"
		}
		owner := ""
		if id == p.ownerSubID {
			owner = "*"
		}
		parts = append(parts, fmt.Sprintf("s%d%s{%s %dx%d seq=%d%s}",
			id, owner, kind, e.cols, e.rows, e.lastActiveSeq, vis))
	}
	return strings.Join(parts, " ")
}

// moreActiveForSizing reports whether sub a should win the PTY size over
// sub b: more recently active, or — on an equal stamp — wider.
func moreActiveForSizing(a, b *subEntry) bool {
	if a.lastActiveSeq != b.lastActiveSeq {
		return a.lastActiveSeq > b.lastActiveSeq
	}
	return a.cols > b.cols
}

// recomputeSize resizes the PTY — and, for grid-mode (Codex) panes, the
// grid emulator — to the active device's geometry (see desiredSizeLocked).
// Called on: subscribe, any sub's resize, claim, disconnect, visibility
// change. If no sub has dims, the PTY is left unchanged.
func (p *Pane) recomputeSize() {
	p.subsMu.RLock()
	cols, rows := p.desiredSizeLocked()
	hasGrid := p.gridEmu != nil
	dbg := p.sizingContentionLog()
	mdbg := p.sizingMobileLog(cols, rows)
	p.subsMu.RUnlock()
	if dbg != "" {
		log.Printf("pane %s: sizing chose %dx%d | %s", p.ID, cols, rows, dbg)
	}
	// Mobile-size diagnostic: log only on a TRANSITION so a stable phone
	// session stays quiet, but reconnect-gap oscillation (the 84⟷62 flip)
	// or the hold engaging shows up as a clear burst. Proves on the next
	// repro whether the churn theory — and the hold fix — are real.
	if mdbg != "" {
		p.mobileSizeMu.Lock()
		changed := mdbg != p.lastSizeLog
		p.lastSizeLog = mdbg
		p.mobileSizeMu.Unlock()
		if changed {
			log.Printf("pane %s: mobile-size %s", p.ID, mdbg)
		}
	}
	if cols == 0 || rows == 0 {
		return
	}
	// Coalesce: a mobile multi-pass fit fires several differing sizes within
	// a few hundred ms; collapse the burst into one Resize after it settles
	// so a normal-buffer Codex pane isn't churned by a SIGWINCH storm.
	p.scheduleCoalescedResize(cols, rows, hasGrid)
}

// resizeCoalesceDelay is the trailing-debounce window for recomputeSize-driven
// resizes. Long enough to swallow a mobile fit storm (font-load + keyboard +
// bottom-bar settle), short enough to feel responsive on a deliberate resize.
const resizeCoalesceDelay = 140 * time.Millisecond

// scheduleCoalescedResize applies the latest desired geometry once the
// caller stops requesting new sizes for resizeCoalesceDelay. Trailing
// debounce: each call records the target and resets the timer, so a burst
// collapses to a single Resize at the final dims. Uses its own mutex (Resize
// takes resizeMu/subsMu, so this lock must never wrap those).
func (p *Pane) scheduleCoalescedResize(cols, rows uint16, hasGrid bool) {
	p.resizeCoalesceMu.Lock()
	p.pendingResizeCols = cols
	p.pendingResizeRows = rows
	if hasGrid {
		p.pendingResizeGrid = true
	}
	if p.resizeCoalesceTimer != nil {
		p.resizeCoalesceTimer.Stop()
	}
	p.resizeCoalesceTimer = time.AfterFunc(resizeCoalesceDelay, p.applyCoalescedResize)
	p.resizeCoalesceMu.Unlock()
}

// applyCoalescedResize commits the most recent pending geometry. Runs off the
// debounce timer; reads the pending dims under resizeCoalesceMu, then releases
// before Resize (which takes resizeMu/subsMu).
func (p *Pane) applyCoalescedResize() {
	p.resizeCoalesceMu.Lock()
	cols, rows := p.pendingResizeCols, p.pendingResizeRows
	hasGrid := p.pendingResizeGrid
	p.pendingResizeGrid = false
	p.resizeCoalesceTimer = nil
	p.resizeCoalesceMu.Unlock()
	if cols == 0 || rows == 0 {
		return
	}
	p.mu.Lock()
	changed := p.Cols != cols || p.Rows != rows
	p.mu.Unlock()
	_ = p.Resize(cols, rows)
	// Keep the grid emulator in lockstep with the PTY size so ARIA
	// detection / debug-grid can't read a desynced grid (emulator narrow,
	// PTY wide). Only on an actual dimension change.
	if hasGrid && changed {
		p.resizeGridEmu(int(cols), int(rows))
	}
}

// sizingContentionLog returns a one-line dump of the subscriber set, but
// ONLY when the subs DISAGREE on width (e.g. a phone + a desktop both
// attached) — the exact case the shared-PTY sizing rule has to arbitrate
// and the one worth diagnosing (why does the phone's narrow width fail to
// win?). Returns "" otherwise so the common single-viewer path stays
// silent. Each entry shows the sub's reported geometry, visibility, and
// lastActiveSeq — the three inputs to desiredSizeLocked — plus which sub is
// the input owner. Caller holds subsMu (read).
func (p *Pane) sizingContentionLog() string {
	withDims := 0
	firstCols := -1
	contend := false
	for _, e := range p.subs {
		if e.cols == 0 || e.rows == 0 {
			continue
		}
		withDims++
		if firstCols == -1 {
			firstCols = int(e.cols)
		} else if int(e.cols) != firstCols {
			contend = true
		}
	}
	if withDims < 2 || !contend {
		return ""
	}
	parts := make([]string, 0, len(p.subs))
	for id, e := range p.subs {
		owner := ""
		if id == p.ownerSubID {
			owner = "*owner"
		}
		parts = append(parts, fmt.Sprintf("sub%d%s{%dx%d vis=%v seq=%d}",
			id, owner, e.cols, e.rows, e.visible, e.lastActiveSeq))
	}
	return strings.Join(parts, " ")
}
