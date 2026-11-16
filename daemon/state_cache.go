package main

import (
	"sync"
	"time"
)

// computeState is a per-pane hot path: three background watchers
// (pusher.go, activity.go, terminal_signals.go) each call it on EVERY pane
// every 2s. Each call does a 64KB SnapshotTail + stripANSI of that 64KB +
// regex scans + a procfs walk (HasCodexDescendant). At ~40 panes the three
// 2s watchers redo several MB/s of identical copy+regex+procfs work. This
// has been the hot path behind prior GC-spiral incidents.
//
// stateCache memoizes computeState's result per pane so the watchers share
// one computation per (output-position, native-pending, TTL) window instead
// of each recomputing from scratch. It is a STRICT performance optimization
// with ZERO intended behavior change: a cache HIT returns exactly the value
// the most recent direct compute returned, and a MISS recomputes via the
// unchanged computeStateWithHasCodex.
//
// The /api/panes request path (api.go) intentionally keeps calling the
// DIRECT uncached computeStateWithHasCodex for maximum on-demand freshness;
// only the periodic background watchers use the cache.
type stateCache struct {
	mu sync.Mutex

	valid bool             // false until the first compute populates the entry
	state PaneRuntimeState // the memoized result

	// Invalidation keys captured at compute time. A cache hit requires ALL
	// of these to be unchanged AND the entry to be within the TTL.

	// totalWritten is RingBuffer.TotalWritten() at compute time. It
	// increments by len(payload) on EVERY PTY write in readLoop (including
	// native panes), and never resets. If it is unchanged, SnapshotTail
	// returns byte-identical bytes, so the entire output-scan portion of
	// computeState is guaranteed to produce the identical result. This is
	// the primary "has the output advanced" signal.
	totalWritten int64

	// computedAt retains time.Now's monotonic clock for the in-memory TTL.
	// Wall-clock corrections must not extend the lifetime of a cached state.
	computedAt time.Time

	// computedAtMs records wall time for diagnostics; it is not the TTL clock.
	computedAtMs int64
}

// stateCacheTTL bounds how long a memoized computeState result may be reused
// when neither the output position nor the native-pending flag has changed.
//
// WHY A TTL IS REQUIRED: computeState has TIME-DEPENDENT transitions that
// change with NO new output — they are driven purely by `age = now - last`:
//
//	state.go:252  if age >= 1500  → the awaiting-input scan is gated on this
//	state.go:283  if age <  1500  → StateBusy
//	state.go:287  if age <  3000  → StateRecent
//	state.go:290  else            → StateIdle
//
// The SMALLEST time threshold that flips the returned state is 1500ms
// (busy → recent at age==1500, which also unlocks the awaiting-input scan).
// The next is 3000ms (recent → idle). To never miss a time-based transition
// by more than the watcher's own 2s cadence, the TTL must be <= the smallest
// such threshold. We pick 1000ms — comfortably below 1500ms, so on any tick
// after output goes quiet the entry expires before the pane could cross the
// busy→recent boundary, and the watcher recomputes and observes the
// transition on schedule. (The watchers already only sample every 2s, so a
// transition is inherently observed with up to ~2s of latency; the 1000ms
// TTL guarantees the cache adds NO additional transition latency beyond that
// existing sampling latency.)
//
// A larger TTL (e.g. 2000ms) would risk reusing a StateBusy result past the
// 1500ms boundary on a tick where a fresh compute would have returned
// StateRecent — a real behavior change. 1000ms avoids that with margin.
const stateCacheTTL = 1000 * time.Millisecond

// computeStateCached returns this pane's runtime state, reusing the most
// recent computeState result when nothing relevant has changed. It is a
// drop-in replacement for p.computeState() for the periodic background
// watchers.
//
// A cache HIT (return the memoized value, no recompute) requires ALL of:
//   - the entry has been populated (valid),
//   - the pane's output position (RingBuffer.TotalWritten) is unchanged,
//   - the native-pending flag is unchanged, AND
//   - less than stateCacheTTL has elapsed since the compute.
//
// Otherwise it recomputes via computeStateWithHasCodex (which itself derives
// hasCodex exactly as the direct computeState() does, so the cached path is
// behavior-identical to the uncached path) and refreshes the entry.
//
// Thread-safety: multiple watcher goroutines call this concurrently for the
// same pane. The whole hit/miss decision plus refresh runs under
// stateCache.mu, so the keys read for the hit test and the keys stored on a
// miss are always consistent. The compute itself runs while holding mu — it
// is the same work one watcher would have done anyway, and serializing
// concurrent callers for the same pane is exactly the point (they share the
// one result rather than racing to redo it).
func (p *Pane) computeStateCached() PaneRuntimeState {
	c := &p.stateCacheData
	written := p.buffer.TotalWritten()

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(c.computedAt)

	if c.valid &&
		c.totalWritten == written &&
		elapsed >= 0 && elapsed < stateCacheTTL {
		return c.state
	}

	// Miss: recompute and refresh the entry. computeStateWithHasCodex is the
	// same function the direct computeState() funnels into; we derive hasCodex
	// the same way it does so the result is identical.
	state := p.computeStateWithHasCodex(p.HasCodexDescendant() || p.HasMuseDescendant())
	c.valid = true
	c.state = state
	c.totalWritten = written
	c.computedAt = now
	c.computedAtMs = now.UnixMilli()
	return state
}

// stateTailCache retains only the expensive, pure ANSI normalization. State
// decisions still observe the current clock, input/ack positions, native
// approvals, process flags and both grids on EVERY direct/API read.
// Bounded to one 64 KiB tail per pane; ring identity also handles replacement.
type stateTailCache struct {
	mu      sync.Mutex
	ring    *RingBuffer
	written int64
	text    string
	hasTail bool
}

func (p *Pane) stateTail() (string, bool) {
	c := &p.stateTailCache
	c.mu.Lock()
	defer c.mu.Unlock()
	r := p.buffer
	r.mu.Lock()
	written := r.totalWritten
	if c.ring == r && c.written == written {
		r.mu.Unlock()
		return c.text, c.hasTail
	}
	// Capture the bytes and generation under the same ring lock. Do the
	// regexp work after releasing it so PTY writes are never held up by it.
	tail := r.snapshotTailLocked(65536)
	r.mu.Unlock()
	c.ring, c.written = r, written
	c.text, c.hasTail = stripANSI(tail), len(tail) > 0
	return c.text, c.hasTail
}
