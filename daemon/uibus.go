package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// uiBus is an in-memory SSE broadcaster for daemon → web-UI events.
// The MCP layer publishes to it (e.g. "Claude just asked to preview
// a file — UI, please open it in a tile"); the web subscribes via
// /api/ui/events with a native EventSource. Fan-out to all connected
// subscribers. Events are best-effort: a slow subscriber gets its
// event dropped rather than blocking the publisher.
//
// Also keeps a ring buffer of recent events so late subscribers (or
// freshly-opened activity viewers) can see what happened before they
// connected. Capped so a long-running daemon doesn't grow unbounded.
type uiBus struct {
	mu          sync.RWMutex
	subscribers map[chan uiEvent]struct{}

	// coalesce configures minimum delivery intervals by event type. State is
	// kept separately per type so a flood of a pure invalidation signal cannot
	// starve unrelated UI events. Suppressed publishes consume neither ring
	// entries nor sequence numbers.
	coalesceMu    sync.Mutex
	coalesce      map[string]time.Duration
	coalesceState map[string]*uiBusCoalesceState

	logMu  sync.Mutex
	log    []uiEvent
	logCap int
	// seq is a per-boot monotonic id stamped on every published event, so
	// pollers (MCP read_activity) can cursor past what they've seen instead
	// of re-reading overlapping windows. Guarded by logMu with the ring.
	seq int64
}

type uiBusCoalesceState struct {
	lastSentAt   time.Time
	pending      bool
	timer        *time.Timer
	pendingEvent uiEvent
}

type uiEvent struct {
	// Seq is the per-boot monotonic publish id (1, 2, …). A poller passes
	// the highest Seq it has seen back as `since` to read only newer events.
	// Resets on daemon restart — cursors are only meaningful within a boot.
	Seq  int64  `json:"seq,omitempty"`
	Type string `json:"type"` // "open-file" | "open-browser" | "close-browser" | "port-up" | "toast" | "tool-call" | "pane-state" | "cmd.exit" | "cmd.long_running" | "build.failed" | "pane.awaiting_approval" | "pane.reply_complete" | "workflow.fallback" | "workflow.fallback_decision" | "workflow.health" | "workflow.budget" | "workflow.autopilot" | "workflow.delegation" | …
	//
	// "workflow.delegation" (workflow_delegation.go) reuses three general
	// fields rather than growing the struct: PaneID/PaneTitle name the ORIGIN
	// pane, Text is the task id (as on every other workflow.* event), Hint is
	// the task title, and Reason is the transition key (awaiting_approval,
	// plan_gate, decision, failed, budget, completed, cancelled, land_failed,
	// land_reconciled, fallback, auto_resumed, autopilot).
	// claudeAutoSelfEmit is local routing metadata, never serialized. It marks
	// a credential invalidation produced by its originating controller so
	// the daemon subscriber can swallow that feedback edge without counters
	// that can be orphaned by coalescing or drop-on-full fan-out.
	claudeAutoSelfEmit bool
	// codexAutoSelfEmit is the Codex controller's twin of claudeAutoSelfEmit.
	codexAutoSelfEmit bool
	Path              string `json:"path,omitempty"`
	URL               string `json:"url,omitempty"`
	Port              *Port  `json:"port,omitempty"`
	Text              string `json:"text,omitempty"`
	// SkipRecent (toast events only): show the toast transiently but do NOT
	// persist it in the Alerts "Recent" notification log. Set for agent/task
	// run-results, which already surface in the Inbox — logging them to Recent
	// too just duplicates that surface (and used to inflate the unread badge).
	SkipRecent bool `json:"skipRecent,omitempty"`

	// Activity fields. Populated on the new "tool-call" and
	// "pane-state" events; ignored otherwise. PaneID + PaneTitle let
	// the UI render "focus this pane" without a follow-up API call.
	// Ts is Unix millis at publish time (server clock).
	PaneID    string `json:"paneId,omitempty"`
	PaneTitle string `json:"paneTitle,omitempty"`
	// Workspace is the named workspace the pane belongs to ("" for the
	// _current scratchpad / unattached). Populated on pane.awaiting_approval
	// so the aria-watcher's "Approval needed — X" title can show the
	// workspace name (recognizable) instead of a generic pane title.
	Show       bool   `json:"show,omitempty"` // pane.spawned: scroll only; never keyboard focus
	Workspace  string `json:"workspace,omitempty"`
	Ts         int64  `json:"ts,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Hint       string `json:"hint,omitempty"`
	FromState  string `json:"fromState,omitempty"`
	ToState    string `json:"toState,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	// AgentMidTurn carries the daemon's live working-flag on "pane-state" events so
	// the client spinner flips with the transition instead of lagging up to 3s for
	// the next panes poll. Pointer so false is distinguishable from absent (omitted
	// on non-pane-state events). Nil elsewhere.
	AgentMidTurn *bool `json:"agentMidTurn,omitempty"`

	// Attention fields. Populated on "pane-attention" events (attention.go):
	// Attention is "none" | "auto-pending" | "needs-user"; AttentionReason is
	// the short human explanation when needs-user has one (never-auto tool,
	// decline, escalation). Empty elsewhere.
	Attention       string `json:"attention,omitempty"`
	AttentionReason string `json:"attentionReason,omitempty"`

	// Terminal-signal fields. Populated on the "cmd.exit",
	// "cmd.long_running", and "build.failed" events emitted for the
	// aria-watcher; ignored otherwise. ExitCode is a pointer so a
	// genuine exit 0 is distinguishable from "not applicable".
	// StderrTail carries the last ~20 lines of the pane buffer on
	// build.failed. ElapsedMs is how long the pane had been busy when
	// cmd.long_running fired.
	ExitCode   *int   `json:"exitCode,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
	ElapsedMs  int64  `json:"elapsedMs,omitempty"`

	// Prompts carries the pane's full recent-prompt history (newest
	// first) on "pane-prompt" events so clients can replace their copy
	// wholesale instead of merging deltas.
	Prompts []string `json:"prompts,omitempty"`
	// Cmd is the pane's command line on terminal-signal events.
	Cmd string `json:"cmd,omitempty"`

	// Cwd reported by Claude Code's hook payload. Only populated on
	// "tool-call" events; provides a debug anchor when our pane
	// attribution fails — the UI renders it so the user can see
	// which directory Claude was operating from, and tell at a
	// glance whether our pane-matching logic is off.
	Cwd string `json:"cwd,omitempty"`

	// Credential-change event fields; ignored on unrelated events.
	// TOKEN-FREE: From/To are display names, Reason a short human string,
	// FromPct5h the previous credential's 5h utilization, and Trigger one of
	// "auto"|"manual"|"reactive". No credential value ever rides this event.
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Degraded is populated only on workflow.fallback when the promoted entry
	// sits below the role's configured model floor.
	Degraded  bool   `json:"degraded,omitempty"`
	Policy    string `json:"policy,omitempty"`
	FromPct5h int    `json:"fromPct5h,omitempty"`
	Trigger   string `json:"trigger,omitempty"`

	// Lease is set on "alerts-handler" events only (notifications_handler.go).
	Lease *alertsLeaseEvent `json:"lease,omitempty"`
}

func newUIBus() *uiBus {
	return &uiBus{
		subscribers: map[chan uiEvent]struct{}{},
		// This event is a pure "re-fetch accounts" invalidation. Coalescing is
		// lossless and bounds any future producer feedback bug to one delivery
		// per second plus a trailing invalidation.
		coalesce: map[string]time.Duration{
			"claude-accounts-changed": time.Second,
		},
		coalesceState: map[string]*uiBusCoalesceState{},
		// 200 events covers "what's my orchestration been doing in the
		// last ~hour at typical pane activity"; enough for the UI feed
		// without letting the daemon balloon on a long-running session.
		logCap: 200,
	}
}

func (b *uiBus) subscribe() chan uiEvent {
	// Buffered — enough headroom that a normal dispatch cycle doesn't
	// drop events, but not so big that a disconnected client accumulates
	// thousands of pending messages.
	c := make(chan uiEvent, 16)
	b.mu.Lock()
	b.subscribers[c] = struct{}{}
	b.mu.Unlock()
	return c
}

func (b *uiBus) unsubscribe(c chan uiEvent) {
	b.mu.Lock()
	_, ok := b.subscribers[c]
	if ok {
		delete(b.subscribers, c)
	}
	b.mu.Unlock()
	if ok {
		close(c)
	}
}

func (b *uiBus) publish(e uiEvent) {
	if b.publishCoalesced(e) {
		return
	}
	b.deliver(e)
}

// publishCoalesced handles configured event types and reports whether e was
// either delivered or retained as the one trailing delivery for its window.
// A suppressed burst always leaves one pending event, whose timestamp is
// refreshed when the timer actually sends it.
func (b *uiBus) publishCoalesced(e uiEvent) bool {
	b.coalesceMu.Lock()
	interval, ok := b.coalesce[e.Type]
	if !ok || interval <= 0 {
		b.coalesceMu.Unlock()
		return false
	}
	state := b.coalesceState[e.Type]
	if state == nil {
		state = &uiBusCoalesceState{}
		b.coalesceState[e.Type] = state
	}
	now := time.Now()
	if !state.pending && (state.lastSentAt.IsZero() || now.Sub(state.lastSentAt) >= interval) {
		state.lastSentAt = now
		b.coalesceMu.Unlock()
		b.deliver(e)
		return true
	}

	if state.pending {
		// A coalesced delivery is considered self-originated only when every
		// invalidation folded into it came from the controller. If any API,
		// refresher, or other external emitter participates, preserve its wake.
		e.claudeAutoSelfEmit = state.pendingEvent.claudeAutoSelfEmit && e.claudeAutoSelfEmit
		e.codexAutoSelfEmit = state.pendingEvent.codexAutoSelfEmit && e.codexAutoSelfEmit
	}
	state.pendingEvent = e
	if !state.pending {
		remaining := interval - now.Sub(state.lastSentAt)
		if remaining < 0 {
			remaining = 0
		}
		state.pending = true
		state.timer = time.AfterFunc(remaining, func() {
			b.flushCoalesced(e.Type)
		})
	}
	b.coalesceMu.Unlock()
	return true
}

func (b *uiBus) flushCoalesced(eventType string) {
	b.coalesceMu.Lock()
	state := b.coalesceState[eventType]
	if state == nil || !state.pending {
		b.coalesceMu.Unlock()
		return
	}
	e := state.pendingEvent
	now := time.Now()
	e.Ts = now.UnixMilli()
	state.pending = false
	state.timer = nil
	state.lastSentAt = now
	b.coalesceMu.Unlock()

	b.deliver(e)
}

// deliver is the unchanged ring-buffer + subscriber fan-out path. Coalescing
// happens before this point so only delivered events receive sequence ids.
func (b *uiBus) deliver(e uiEvent) {
	// Ring buffer — store EVERY event so /api/activity can replay the
	// full recent timeline. Activity UI can still filter display-side;
	// keeping the buffer unfiltered means we can add new event types
	// without touching the storage path.
	b.logMu.Lock()
	b.seq++
	e.Seq = b.seq
	b.log = append(b.log, e)
	if len(b.log) > b.logCap {
		// Trim from the front. O(n) per trim, but n ≤ logCap so it's
		// bounded; simpler than a proper circular buffer and the call
		// rate is well below anywhere this matters.
		b.log = b.log[len(b.log)-b.logCap:]
	}
	b.logMu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for c := range b.subscribers {
		select {
		case c <- e:
		default:
			// Drop rather than block. UI events are ephemeral — if the
			// client missed one, the ring buffer snapshot fills it in
			// on the next /api/activity poll.
		}
	}
}

// Recent returns a copy of the ring-buffer contents, oldest first. Safe
// to call concurrently with publish.
func (b *uiBus) Recent() []uiEvent {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	out := make([]uiEvent, len(b.log))
	copy(out, b.log)
	return out
}

// RecentSince returns the retained events with Seq > since (all of them for
// since ≤ 0) plus the bus's current cursor — the Seq of the newest event
// ever published, which the caller hands back as its next `since` even when
// the ring has already trimmed past it.
func (b *uiBus) RecentSince(since int64) ([]uiEvent, int64) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	start := 0
	for start < len(b.log) && b.log[start].Seq <= since {
		start++
	}
	out := make([]uiEvent, len(b.log)-start)
	copy(out, b.log[start:])
	return out, b.seq
}

// uiEvents is the SSE endpoint the web client long-polls for UI events.
// Not loopback-locked: cloud-relay access is fine (the tunnel is already
// authenticated) so mobile/remote UIs still receive events.
func (a *apiServer) uiEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Block buffering proxies (nginx, Cloudflare) from holding bytes.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	ch := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(ch)

	// Comment frame so the client sees a successful connect immediately.
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Keepalive: without periodic traffic a half-open socket (phone slept,
	// NAT rebound, relay hiccup) is indistinguishable from a quiet daemon —
	// the client waits forever on a dead stream and its pane lists go stale.
	// A comment frame every 25s gives clients a liveness signal to watchdog
	// (silence ⇒ reconnect ⇒ refetch) and lets this handler notice a dead
	// peer via the failed write.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
