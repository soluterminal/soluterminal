package main

import (
	"encoding/json"

	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// nativeSSEHeartbeat is how often the live SSE loop writes a `: ping` comment
	// frame. Cheap keepalive that (a) keeps proxies/load-balancers from idle-
	// reaping a quiet stream and (b) — paired with the write deadline below —
	// detects a half-open socket (dead cellular) within ~one interval instead of
	// waiting out the OS TCP timeout, so the goroutine + subscription don't leak.
	nativeSSEHeartbeat = 15 * time.Second

	// nativeSSEWriteTimeout bounds a single SSE write. A wedged client whose TCP
	// window never drains makes the write block forever without this; with it the
	// write errors, the loop returns, and the deferred cancel() drains the
	// subscription. Comfortably larger than the heartbeat so a slow-but-live
	// client isn't killed.
	nativeSSEWriteTimeout = 30 * time.Second
)

// nativeEventFrame returns the JSON wire frame for a native event, reusing the
// fanout-cached bytes (set in emitNativeEvent when subscribers exist) so a live
// delta is serialized once across all subscribers. Ring/snapshot entries carry
// no cache (kept lean), so reconnect replay marshals them on demand here.
func nativeEventFrame(ev NativeEvent) ([]byte, error) {
	if ev.marshaled != nil {
		return ev.marshaled, nil
	}
	return json.Marshal(ev)
}

// paneNativeStream is the long-lived SSE subscriber for a native pane. On connect
// it flushes a `: connected` comment frame, replays the native-ring snapshot
// (oldest-first, each event Seq'd) for backfill, then forwards live deltas from
// the pane's native subscription until the client disconnects.
func (a *apiServer) paneNativeStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

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

	// Subscribe BEFORE replaying the snapshot so no event slips through the gap
	// between snapshot copy and live-loop entry: the ring snapshot and the live
	// channel both come from SubscribeNative under one nativeMu, and any event
	// that races in after the snapshot is buffered on the channel. The client
	// dedups by Seq (snapshot tail may overlap the first live deltas).
	_, snapshot, ch, cancel, isNative := p.SubscribeNative()
	if !isNative {
		// Not a native pane (or parser torn down): 409 rather than streaming an
		// empty bus, so terminal panes never accidentally hold an SSE connection.
		http.Error(w, "pane is not native", 409)
		return
	}
	defer cancel()

	// ResponseController lets us put a deadline on each write so a wedged client
	// (half-open socket, dead cellular) can't block the loop forever. Best-effort:
	// if the underlying conn doesn't support deadlines SetWriteDeadline returns
	// http.ErrNotSupported, which we tolerate (the loop still exits on the request
	// context being cancelled).
	rc := http.NewResponseController(w)

	// writeFrame applies a fresh write deadline, writes one SSE frame, and flushes.
	// Any write/flush error (deadline exceeded on a stuck client, broken pipe)
	// returns false so the caller exits the loop and the deferred cancel() drains
	// this subscription — no leaked goroutine + socket.
	writeFrame := func(frame string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(nativeSSEWriteTimeout))
		if _, err := fmt.Fprint(w, frame); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Comment frame so the client sees a successful connect immediately.
	if !writeFrame(": connected\n\n") {
		return
	}

	// Announce this pane's nativeEpoch BEFORE the snapshot so a reconnecting
	// client resets its fold cache before folding the fresh ring. A daemon
	// auto-update swap reattaches the SAME agent but rebuilds the ring from seq 1
	// under a NEW epoch; the browser's EventSource auto-reconnects silently, so
	// without this announcement the client keeps its stale seen={1..N}/lastSeq and
	// silently dedups the swap's low-seq tail — the "missing latest messages until
	// a Term toggle" bug. The poll-driven DTO epoch catches the same change but
	// only ~3s later, losing the race to the instant reconnect. Named SSE event so
	// it never reaches the client's onmessage NativeEvent parser. Mirrors the DTO
	// epoch fallback (api.go): a pane not yet ensureNative'd reads 0 → daemonEpoch.
	ep := p.nativeEpoch.Load()
	if ep == 0 {
		ep = daemonEpoch
	}
	if !writeFrame(fmt.Sprintf("event: epoch\ndata: {\"nativeEpoch\":%d}\n\n", ep)) {
		return
	}

	// Replay the ring snapshot (backfill) before live deltas.
	for _, ev := range snapshot {
		data, err := nativeEventFrame(ev)
		if err != nil {
			continue
		}
		if !writeFrame(fmt.Sprintf("data: %s\n\n", data)) {
			return
		}
	}

	// Heartbeat: a `: ping` comment frame every nativeSSEHeartbeat keeps the
	// stream warm through idle periods and — via the write deadline above — turns
	// a half-open connection into a write error so we clean up promptly.
	ping := time.NewTicker(nativeSSEHeartbeat)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if !writeFrame(": ping\n\n") {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				// Pane exited / parser torn down: subscriber channel closed.
				return
			}
			data, err := nativeEventFrame(ev)
			if err != nil {
				continue
			}
			if !writeFrame(fmt.Sprintf("data: %s\n\n", data)) {
				return
			}
		}
	}
}

// paneNativeHistory backfills native events with Seq > since. Plain JSON (not
// SSE). The common case (a brief SSE disconnect / resync) is served straight
// from the ring; when ?since= predates the ring floor — events have aged out —
// the on-disk transcript is re-parsed to reconstruct the full prior
// conversation so the client never loses early history. See historyBackfill.
func (a *apiServer) paneNativeHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if p.Snapshot().Kind != "native" {
		http.Error(w, "pane is not native", 409)
		return
	}

	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = parsed
		}
	}

	events, truncated := historyBackfill(p, since)
	writeJSON(w, 200, map[string]any{
		"paneId":    p.ID,
		"since":     since,
		"events":    events,
		"truncated": truncated,
	})
}

// historyBackfill assembles the /native/history payload for a native pane: the
// ring events with Seq>since, plus — when the client needs a FULL history
// backfill — the prior conversation re-derived from the on-disk transcript,
// prepended in a disjoint Seq block. Returns the events and whether the ring
// portion was truncated.
//
// `truncated` reports whether the requested `since` predates the oldest event
// still in the ring — i.e. some events between `since` and the ring floor aged
// out and are NOT in the ring portion of this response. The client uses it to
// decide it must REBUILD its cache from this response (drop its stale,
// now-non-contiguous live events) rather than fold incrementally.
//
// The transcript seed runs in TWO cases, both of which need the complete past
// conversation:
//   - since==0: a fresh seed/mount. `claude --resume` restores CONTEXT
//     server-side but replays NO events (Phase-0 Q3), so a resumed/flipped
//     native pane is blank until the next message — the transcript is the only
//     backfill source for those past turns.
//   - truncated: the live ring has rotated past `since`, so the ring alone
//     can't heal the gap. The transcript is the complete authoritative record,
//     so we serve it and let the client rebuild. (This was previously a stub:
//     truncated responses returned only the ring tail, so early history
//     vanished once a long conversation rolled the ring.)
//
// Seeded events are marked FromReplay=true and stamped into a high, live-disjoint
// Seq block (seedSeqBase) so they (a) never collide with the live ring's 1..N
// seqs — the client dedups by seq, so a collision would silently drop a real
// event — and (b) render ahead of the live events because the client folds in
// array order. mergeSeedWithRing dedups the user_text echo overlap (the live
// ring's replay-echo user_text vs. the seeded user_text) so the last prompt
// isn't shown twice. A since>0 request that is NOT truncated keeps the pure-ring
// behavior (the client already holds the older history).
func historyBackfill(p *Pane, since uint64) (events []NativeEvent, truncated bool) {
	events = p.NativeSince(since)
	// truncated := the client's lowest expected Seq (since+1) predates the oldest
	// Seq the ring can still serve (NativeRingFloor) — i.e. events between `since`
	// and the floor aged out of the ring. Computed from the ring FLOOR, not from
	// events[0], so it ALSO catches the full-rollover case where the ring has
	// rotated entirely past `since` and NativeSince returns NOTHING: the old
	// `events[0].Seq>since+1` test silently missed that (empty slice => false =>
	// the seed never ran => every event between `since` and now was lost).
	//
	// No `since>0` guard: a since==0 mount whose ring has already evicted seq 1
	// IS truncated relative to the start of the conversation (floor>1), and the
	// floor is exactly 1 (=> not truncated) for a fresh pane that hasn't evicted
	// anything, so the fresh-mount case still reads as not-truncated naturally.
	if floor := p.NativeRingFloor(); since+1 < floor {
		truncated = true
	}
	if since == 0 || truncated {
		snap := p.Snapshot()
		// Claude and Codex have separate conversation stores. A reused pane can
		// legitimately retain a historical ClaudeSessionID while its current
		// foreground/native agent is Codex; seeding that Claude transcript here
		// would inject a replayed session_init (for example Opus) into the Codex
		// Focus footer. Codex history is supplied by app-server thread/read, so the
		// on-disk Claude seed must be strictly provider-gated.
		if effectiveAgentKind(snap.Agent) == agentClaude && snap.ClaudeSessionID != "" {
			if seeded, err := seedFromTranscript(snap.Cwd, snap.ClaudeSessionID, snap.Model); err == nil && len(seeded) > 0 {
				events = mergeSeedWithRing(seeded, events)
			}
		}
	}
	return dropResolvedApprovalCards(p, events), truncated
}

// dropResolvedApprovalCards removes replayed permission_request events whose
// approval is no longer live-pending. Fixes the "dead question after respawn"
// bug: the live ring retains the original permission_request event, so a full
// remount (since==0: renderer switch, daemon restart, auto-update, fresh mount)
// re-renders an ALREADY-RESOLVED approval as an actionable card. Clicking it
// resolves nothing — resolveApproval finds no pending entry (takeNativePending
// returns nil → errNoPendingApproval), which the HTTP layer maps to a no-op 200,
// so the answer is silently dropped.
//
// The fix is authoritative, not heuristic: keep an approval card ONLY if its
// RequestID is still in the live pending set (p.nativePendingApprovals) — those
// are exactly the requests a click CAN resolve, and a genuinely-pending one is
// backed by its original live park so its buttons work. A resolved/stale request
// is dropped from the replay entirely (its tool_call/tool_result still convey the
// outcome). This is the DISPLAY complement to the EvPermissionRequest FromReplay
// STATE guard in pane_native.go (which already prevents a replayed request from
// re-parking a ghost choice-prompt). No-op when the replay set carries no
// permission_request, so the common path allocates nothing.
func dropResolvedApprovalCards(p *Pane, events []NativeEvent) []NativeEvent {
	hasApproval := false
	for i := range events {
		if events[i].Type == EvPermissionRequest {
			hasApproval = true
			break
		}
	}
	if !hasApproval {
		return events
	}
	live := make(map[string]bool)
	for _, ap := range p.listNativePending() {
		if ap.RequestID != "" {
			live[ap.RequestID] = true
		}
	}
	out := make([]NativeEvent, 0, len(events))
	for _, ev := range events {
		if ev.Type == EvPermissionRequest && !live[ev.RequestID] {
			continue
		}
		out = append(out, ev)
	}
	return out
}

type nativeInputReq struct {
	Text string `json:"text"`
	// LocalMsgID is an optional client-supplied id echoed back so the client can
	// dedup its optimistic local turn against the --replay-user-messages echo.
	LocalMsgID string `json:"localMsgId,omitempty"`
}

type composerStateReq struct {
	NonEmpty bool `json:"nonEmpty"`
}

func (a *apiServer) setPaneComposerState(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req composerStateReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	p.SetComposerNonEmpty(req.NonEmpty, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

// truncateForLog clips s to at most n bytes with an ellipsis marker, for bounded
// structured logging of user-supplied text (a full prompt can be large and isn't
// needed to correlate a repro). Byte-sliced — a split multi-byte rune is
// cosmetically harmless inside a %q debug log.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// nativeAllowlistReq is the body for POST /api/panes/{id}/native/allowlist.
type nativeAllowlistReq struct {
	Tool   string `json:"tool"`
	Action string `json:"action"` // "add" | "remove"
}

// paneNativeAllowlistGet returns the pane's per-pane tool-approval allowlist
// ("always allow this tool"). GET /api/panes/{id}/native/allowlist →
// 200 {"tools":["Read","Grep"]}. Returns an empty array (never null) for a
// pane with no opt-ins or a non-native pane, so the client always gets a stable
// shape. 404 when the pane id is unknown.
func (a *apiServer) paneNativeAllowlistGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	writeJSON(w, 200, map[string]any{"tools": allowlistOrEmpty(p.allowlistTools())})
}

// paneNativeAllowlistPost adds or removes a tool from the pane's allowlist.
// POST /api/panes/{id}/native/allowlist  body {"tool":"Read","action":"add"}.
// action ∈ {"add","remove"}. Returns 200 {"tools":[...]} (the updated list),
// 400 on a bad body (missing tool / unknown action), 404 if the pane is
// missing. Entries are either an EXACT tool name ("Bash") or a command-prefix
// rule ("Bash(git status)") — prefix rules are validated on add (Bash only,
// non-empty prefix) and matched soundly by allowlistPrefixMatch.
func (a *apiServer) paneNativeAllowlistPost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}
	if effectiveAgentKind(p.Snapshot().Agent) == agentCodex {
		http.Error(w, "persistent tool allowlisting is unavailable for Codex approvals", http.StatusConflict)
		return
	}
	if effectiveAgentKind(p.Snapshot().Agent) == agentMuse {
		http.Error(w, "Muse Focus runs sandboxed without interactive approvals — nothing to allowlist", http.StatusConflict)
		return
	}
	var req nativeAllowlistReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		http.Error(w, "tool is required", 400)
		return
	}
	var tools []string
	switch req.Action {
	case "add":
		// Prefix-rule entries must be well-formed on the way IN (remove stays
		// permissive so a bad legacy entry can always be deleted).
		if i := strings.IndexByte(tool, '('); i >= 0 {
			inner := ""
			if strings.HasSuffix(tool, ")") {
				inner = strings.TrimSpace(tool[i+1 : len(tool)-1])
			}
			if tool[:i] != "Bash" || inner == "" {
				http.Error(w, `prefix rules must look like Bash(git status)`, 400)
				return
			}
		}
		// An allowlist entry approves every future matching request without
		// the person, so adding one is the same act as turning on auto-approve.
		if !a.requirePromptAnswer(w, r, p, "add an auto-allow rule", "") {
			return
		}
		tools = p.allowlistAddTool(tool)
	case "remove":
		tools = p.allowlistRemoveTool(tool)
	default:
		http.Error(w, "action must be \"add\" or \"remove\"", 400)
		return
	}
	// Persist the change so it survives reconnect + daemon-restart-resume.
	// Non-fatal on failure: the in-memory allowlist is already updated and the
	// auto-allow path keys off that, so we still return the live list.
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			log.Printf("native allowlist: persist failed (pane=%s): %v", id, err)
		}
	}
	writeJSON(w, 200, map[string]any{"tools": allowlistOrEmpty(tools)})
}

// allowlistOrEmpty maps a nil allowlist to a non-nil empty slice so the JSON
// response is always `[]`, never `null` (the wire contract specifies an array).
func allowlistOrEmpty(tools []string) []string {
	if tools == nil {
		return []string{}
	}
	return tools
}
