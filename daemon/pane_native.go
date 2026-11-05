package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

// pane_native.go isolates the native-renderer parser machinery to Pane so the
// surgical footprint in pane.go stays minimal (one Kind-guarded, non-blocking
// tee in the readLoop hot path). Everything here is gated on Kind=="native":
// terminal panes never call ensureNative, so the tee channel stays nil and the
// hot path pays a single pointer compare — the byte-identical fallback guarantee.
//
// Lifecycle mirrors the grid cluster (ensureGrid/shutdownGrid): ensureNative is
// called on native spawn + AttachExisting/ResumeClaudePane restore; shutdownNative
// is called from markExited (the single teardown choke point reached on clean
// FrameExit, EOF, and frame-read error). No leak across exitLingerDuration.
//
// The parser goroutine OWNS:
//   - cross-chunk line carry (the tee delivers raw PTY payload slices that do
//     NOT respect JSONL line boundaries; we buffer + split on '\n' + carry the
//     trailing partial fragment to the next chunk),
//   - Seq assignment (monotonic per-pane), PaneID + FromReplay stamping,
//   - the bounded native ring (drop-oldest by count AND bytes),
//   - fan-out to native subscribers under nativeMu,
//   - the pending-approval pointer (Store on permission_request, clear on
//     turn_end / control_cancel_request).
//
// adapter.Feed stays a PURE (line)->[]NativeEvent transform; all stateful work
// (carry, seq, ring, subs, approval) lives here.

const (
	// nativeRingMaxEvents bounds the ring by event count. Covers a long turn's
	// worth of deltas/tool-calls so a late SSE joiner sees recent history
	// without re-parsing the transcript; ?since= backfills below this.
	nativeRingMaxEvents = 4000

	// nativeRingMaxBytes bounds the ring by approximate marshaled size so a
	// burst of large tool_result payloads can't balloon memory. ~4 MiB.
	nativeRingMaxBytes = 4 * 1024 * 1024

	// nativeTeeBuf is the tee channel buffer depth. The readLoop backpressures on
	// full because these are machine-protocol bytes: dropping even one chunk can
	// lose an approval or turn boundary. The buffer absorbs ordinary parser
	// scheduling jitter so backpressure remains an exceptional path. (Was 256.)
	nativeTeeBuf = 1024

	// nativeSubBuf is each native subscriber's buffer depth. Drop-on-full: a
	// slow SSE client misses events and resyncs via ?since=, never backpressures
	// the parser. Raised so a client whose uplink briefly stalls (cellular / the
	// cloud relay during a burst) has ~seconds of headroom at streaming cadence
	// (~60 events/s) before it drops and pays a resync. (Was 64.)
	nativeSubBuf = 256
)

// nativeChunk is one tee delivery from the readLoop to the parser goroutine.
// replay=true marks bytes from the agent's stdout-ring BACKLOG replay
// (FrameReplay — every (re)attach starts with one): the parser processes them
// as live state so a daemon restart reconstructs still-open approvals and the
// in-turn flag from the backlog, but child-writing side effects (allowlist
// auto-allow) are suppressed for them — the original live pass already
// answered, and a duplicate control_response corrupts claude's stdin protocol.
type nativeChunk struct {
	data      []byte
	replay    bool
	replayEnd bool
}

// ensureNative lazily wires up the native adapter, tee channel, subscriber map,
// ring, and starts the parser goroutine. Idempotent — safe to call repeatedly
// (spawn + a later AttachExisting both call it). Caller must NOT hold nativeMu
// or subsMu.
//
// The tee channel is published into the readLoop hot path under subsMu (the ONE
// place native state touches that lock) so the readLoop's `p.nativeTeeCh != nil`
// guard and the parser-goroutine teardown can't race a half-initialized channel.
func (p *Pane) ensureNative() {
	p.nativeMu.Lock()
	if p.nativeAdapter != nil {
		p.nativeMu.Unlock()
		return
	}
	switch effectiveAgentKind(p.Agent) {
	case agentCodex:
		p.nativeAdapter = NewCodexAdapter()
	case agentMuse:
		p.nativeAdapter = NewMuseAdapter()
	default:
		p.nativeAdapter = NewClaudeAdapter()
	}
	// Stamp this spawn's unique NativeEpoch (once per *Pane, under the same guard
	// as the adapter init). A mode-switch respawn builds a new *Pane → new epoch →
	// the client re-seeds instead of rendering blank. See nextNativeEpoch.
	p.nativeEpoch.Store(nextNativeEpoch())
	p.nativeSubs = map[int64]chan NativeEvent{}
	tee := make(chan nativeChunk, nativeTeeBuf)
	stop := make(chan struct{})
	p.nativeStop = stop
	p.nativeMu.Unlock()

	// Publish the tee channel into the hot path under subsMu — the same lock
	// the readLoop holds when it reads p.nativeTeeCh — so the readLoop never
	// observes a torn pointer and the field flips atomically w.r.t. the fanout.
	p.subsMu.Lock()
	p.nativeTeeCh = tee
	p.subsMu.Unlock()

	go p.nativeParseLoop(tee, stop)
}

// bootstrapNativeCodex starts the response-driven app-server handshake for a
// freshly spawned Codex Focus pane. Reattached app-server processes are seeded
// in ensureNative from the persisted thread id and must never be initialized a
// second time.
func (p *Pane) bootstrapNativeCodex() error {
	snap := p.Snapshot()
	adapter, ok := p.NativeAdapterRef()
	if !ok || effectiveAgentKind(snap.Agent) != agentCodex {
		return nil
	}
	driver, ok := adapter.(*CodexAdapter)
	if !ok {
		return errors.New("Codex Focus adapter unavailable")
	}
	frame := driver.Bootstrap(snap.Cwd, snap.CodexThreadID, snap.Model, snap.Effort, orchestratorDeveloperInstructions(snap.Role))
	if len(frame) == 0 {
		return errors.New("Codex Focus bootstrap produced no initialize request")
	}
	_, err := p.WriteNoPromptRecord(frame)
	return err
}

// nativeParseLoop is the per-pane parser goroutine. It drains the tee, performs
// cross-chunk JSONL line carry, runs the (pure) adapter per complete line, then
// stamps + rings + fans out each resulting event. Exits on stop close, p.done,
// or tee close.
func (p *Pane) nativeParseLoop(tee <-chan nativeChunk, stop <-chan struct{}) {
	var carry []byte
	for {
		select {
		case <-stop:
			return
		case <-p.done:
			return
		case chunk, ok := <-tee:
			if !ok {
				return
			}
			if chunk.replayEnd {
				p.requestCodexThreadHistory()
				continue
			}
			carry = p.consumeNativeChunk(carry, chunk.data, chunk.replay)
		}
	}
}

// consumeNativeChunk appends chunk to the carried partial line, splits on '\n',
// processes every COMPLETE line, and returns the trailing incomplete fragment to
// carry forward. The carry buffer is copied (never aliases the tee slice) so a
// retained fragment can't be clobbered by buffer reuse.
//
// Split this way it is invariant under arbitrary chunk boundaries: feeding the
// same bytes one-at-a-time, all-at-once, or split at any offset yields the same
// line stream (the chunked-stream-stripper carry contract).
//
// fromBacklog tags lines from an agent stdout-ring replay chunk (FrameReplay):
// processed as live (state reconstruction) with child-writing side effects
// suppressed. A partial line spanning the backlog→live boundary classifies as
// live (it completes in a live chunk) — acceptable: it IS the freshest line.
func (p *Pane) consumeNativeChunk(carry, chunk []byte, fromBacklog bool) []byte {
	carry = append(carry, chunk...)
	for {
		nl := bytes.IndexByte(carry, '\n')
		if nl < 0 {
			break
		}
		line := carry[:nl]
		// Trim a trailing CR so CRLF transports don't corrupt the JSON tail.
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		if len(bytes.TrimSpace(line)) > 0 {
			// Copy: the adapter may retain sub-slices (json.RawMessage), and we
			// reslice carry below, so the line must not alias the carry buffer.
			p.processNativeLine(append([]byte(nil), line...), false, fromBacklog)
		}
		carry = carry[nl+1:]
	}
	// Compact the carried remainder into a fresh backing array so the buffer
	// doesn't grow unbounded across many splits.
	if len(carry) == 0 {
		return carry[:0]
	}
	rest := make([]byte, len(carry))
	copy(rest, carry)
	return rest
}

// processNativeLine runs the adapter on one complete JSONL line, stamps the
// envelope, and emits each event. fromReplay marks transcript-seeded events;
// fromBacklog marks agent stdout-ring replay lines (see nativeChunk).
func (p *Pane) processNativeLine(line []byte, fromReplay, fromBacklog bool) {
	p.nativeMu.Lock()
	adapter := p.nativeAdapter
	p.nativeMu.Unlock()
	if adapter == nil {
		return
	}
	// Idle-clock reset for the stuck-turn settle (nativeWorkingNow). ANY live line
	// from the agent — INCLUDING ones that map to no NativeEvent (message_start,
	// ping, content_block_start/stop, system heartbeats, rate_limit_event, mid-turn
	// message_delta) — is proof the process is alive and mid-turn. Captured claude
	// stream-json emits SOME line every ~1.3s during generation (max ~7s gap on a
	// small context; it scales with context size), but only a handful of event types
	// were stamped in emitNativeEvent — so the clock measured "mapped-event silence",
	// not true process silence, and a long time-to-first-token / inter-block pause on
	// a big context aged it past the backstop and DROPPED the spinner mid-turn (the
	// 20.4s idle-settle observed on the aria repo). Stamping every live line here makes
	// the clock measure TRUE silence, so the backstop only fires on a genuinely dead
	// turn. Replayed transcript seeds (fromReplay) must NOT light it, or a fresh
	// reattach would read "working" forever.
	if !fromReplay {
		p.nativeLastEventNano.Store(time.Now().UnixNano())
	}
	sawSessionInit := false
	for _, ev := range adapter.Feed(line) {
		// Preserve semantic replay emitted by an adapter (for example Codex
		// thread/read history) in addition to raw transport replay provenance.
		ev.FromReplay = ev.FromReplay || fromReplay
		ev.fromBacklog = fromBacklog
		// Drop the CLI's `--replay-user-messages` echo of a prompt the daemon
		// already emitted at input time (so the prompt's seq precedes the reply —
		// the echo can otherwise arrive AFTER the reply starts and render the prompt
		// below the answer). Only LIVE (non-seed) user_text echoes are candidates.
		// Suppression is now armed for BOTH fresh AND steer submits (native_api.go),
		// so a steer echo is dropped here too; a transcript SEED (fromReplay) never
		// consumes an entry — it is prior history, not the input-time echo.
		if !fromReplay && ev.Type == EvUserText && p.consumeEchoSuppress(ev.Text) {
			log.Printf("[native-turn] pane=%s echo-suppressed user_text textLen=%d", p.ID, len(ev.Text))
			continue
		}
		if !ev.FromReplay && ev.Type == EvSessionInit {
			sawSessionInit = true
		}
		p.emitNativeEvent(ev)
	}
	// Codex initialization is response-driven: after Feed handles initialize,
	// it queues initialized + thread/start|resume. Drain and write on the parser
	// goroutine so ordering matches the app-server response stream exactly.
	if driver, ok := adapter.(*CodexAdapter); ok {
		if outbound := driver.DrainOutbound(); len(outbound) > 0 {
			if _, err := p.WriteNoPromptRecord(outbound); err != nil {
				p.emitNativeEvent(NativeEvent{Type: EvError, Message: "Codex protocol write failed", Detail: err.Error(), Code: "protocol_write_failed"})
			}
		}
		// A freshly bootstrapped resume excludes inline turns; request its
		// canonical bounded history only after the thread response establishes
		// readiness. Fresh threads return an empty seed, harmlessly.
		if sawSessionInit {
			p.writeCodexProtocolFrame(driver.ReadThread(), "thread history request")
		}
	}
	// control_cancel_request (the agent acking an interrupt by cancelling its
	// OWN pending approval) emits nothing through the adapter, so peek the raw
	// type here to clear a stale pending approval — otherwise computeState would
	// keep reporting StateChoicePrompt after an interrupt.
	if isNativeCancelLine(line) {
		for _, ap := range p.drainNativePending() {
			p.emitNativeEvent(NativeEvent{
				Type: EvPermissionResolved, RequestID: ap.RequestID,
				CallID: ap.ToolUseID, ToolName: ap.ToolName,
			})
		}
	}
	// A message_delta carrying stop_reason=end_turn is the model's positive "done"
	// signal (it lands ~ms before the result line). Settle nativeInTurn on it so the
	// spinner clears deterministically on the done-signal rather than waiting on the
	// idle-timeout net; mid-turn deltas carry stop_reason=tool_use and are ignored.
	// turn_end's later Swap(false) is then a silent no-op (one settle log, not two).
	if !fromReplay && nativeEndTurnLine(line) {
		p.setFleetTurnAdmission(false)
		if p.nativeInTurn.CompareAndSwap(true, false) {
			p.nativeOpenTools.Store(0)
			log.Printf("[native-turn] pane=%s set=false trigger=stop_reason", p.ID)
			p.publishTurnEdge()
		}
	}
}

// requestCodexThreadHistory runs at the ordered agent FrameReplayEnd boundary.
// A surviving app-server was primed from its persisted thread id in
// AttachExisting, so this fetch reconstructs semantic history that may predate
// the raw 8 MiB transport ring. A fresh spawn is not ready at this boundary and
// ReadThread correctly returns nil; its thread response requests history later.
func (p *Pane) requestCodexThreadHistory() {
	adapter, ok := p.NativeAdapterRef()
	if !ok {
		return
	}
	driver, ok := adapter.(*CodexAdapter)
	if !ok {
		return
	}
	p.writeCodexProtocolFrame(driver.ReadThread(), "thread history request")
}

func (p *Pane) writeCodexProtocolFrame(frame []byte, what string) {
	if len(frame) == 0 {
		return
	}
	if _, err := p.WriteNoPromptRecord(frame); err != nil {
		// A history read is continuity enrichment, not a failed model turn.
		// Logging preserves diagnostics without injecting EvError, which generic
		// native bookkeeping interprets as a live-turn settle signal.
		log.Printf("native(codex): pane=%s %s failed errorType=%T", p.ID, what, err)
	}
}

// emitNativeEvent stamps Seq + PaneID onto an adapter event, appends it to the
// bounded ring, fans it out to subscribers, and maintains the pending-approval
// pointer for the computeState short-circuit. All under nativeMu except the
// atomic approval Store (lock-free).
func (p *Pane) emitNativeEvent(ev NativeEvent) uint64 {
	// Stamp the auto-allow verdict BEFORE the ring append + fanout below: the
	// post-fanout switch calls maybeAutoAllowFromList, so by the time the daemon
	// resolves the request the client has ALREADY been handed the event and drawn
	// a decision card — which then vanishes a few ms later. That teardown is the
	// "full bash/error details show, then it auto-approves" flash. The client used
	// to approximate this with a whole-tool-name match, which silently missed
	// every argument-scoped rule ("Bash(git status)"); now it just reads the flag.
	//
	// Computed OUTSIDE nativeMu (allowlistAutoAllows takes the allowlist + pane
	// locks), matching the lock discipline maybeAutoAllowFromList documents.
	// Skipped for the same cases the auto-allow itself skips — a backlog replay
	// (already answered by the previous daemon) and codex (its own approval path)
	// — so neither can be hidden from the user on a false promise.
	autoAllowHow := ""
	if ev.Type == EvPermissionRequest && ev.RequestID != "" && !ev.fromBacklog &&
		effectiveAgentKind(p.Agent) != agentCodex {
		how, denyReason := p.allowlistAutoAllows(ev.ToolName, ev.Args)
		if denyReason != "" {
			log.Printf("approvals: allowlist auto-allow WITHHELD by pane policy (pane=%s tool=%s reqID=%s): %s",
				p.ID, ev.ToolName, ev.RequestID, denyReason)
		} else {
			autoAllowHow = how
		}
		// THE decision — stamped for the client here and handed to the resolve in
		// the post-fanout switch below, so the two can never disagree.
		ev.WouldAutoAllow = autoAllowHow != ""
	}
	p.nativeMu.Lock()
	p.nativeSeq++
	ev.Seq = p.nativeSeq
	ev.PaneID = p.ID
	seq := ev.Seq          // captured under the lock for the caller (e.g. the input-time emit log)
	p.ringAppendLocked(ev) // ring keeps the lean struct (marshaled stays nil)
	// Marshal ONCE for the live fanout when anyone is listening, so N subscribers
	// write the same frame instead of each re-marshaling in their SSE loop. Set
	// AFTER ringAppendLocked (which copied ev by value), so the ring entry stays
	// lean — reconnect replay re-marshals on demand via nativeEventFrame.
	if len(p.nativeSubs) > 0 {
		if data, err := json.Marshal(ev); err == nil {
			ev.marshaled = data
		}
		// Snapshot subscriber channels under the lock; send below (sends are
		// non-blocking, so holding nativeMu during fanout is cheap and keeps Seq
		// ordering consistent with ring order).
		for _, ch := range p.nativeSubs {
			select {
			case ch <- ev:
			default:
				// Subscriber channel full: this frame is DROPPED for that client (it
				// resyncs via /native/history?since=). Log so a vanished prompt that
				// traces to a dropped user_text frame self-reports — correlate seq
				// against the [native-input] userSeq. On the drop path (rare), so the
				// cost is negligible; log.Printf locks internally, not nativeMu.
				log.Printf("[native-turn] pane=%s DROP frame on full subscriber type=%s seq=%d", p.ID, ev.Type, ev.Seq)
			}
		}
	}
	p.nativeMu.Unlock()

	// Maintain the explicit in-turn flag for IsAgentWorking()'s graceful-switch
	// busy guard. Live (non-replay) assistant streaming — a delta, finalized
	// text/thinking, a tool call, or a status:requesting spinner tick — means a
	// turn is in flight; turn_end / error close it. Replayed transcript-seed
	// events (FromReplay) must NOT light the flag, or a fresh reattach would look
	// "working" forever. The user-turn-write side of the flag is set in the input
	// path (paneNativeInput), so the agent reads as working the instant we submit,
	// before any byte streams back.
	if !ev.FromReplay {
		// Idle clock for the stuck-turn settle (nativeWorkingNow): any live event
		// is activity that resets it.
		p.nativeLastEventNano.Store(time.Now().UnixNano())
		// Track whether the LAST live event was a tool_result: the model then sits
		// in the (event-less) gap before it resumes streaming its next message, so
		// nativeWorkingNow gives that gap a longer settle allowance than a plain
		// post-response idle — letting the normal settle be snappy without the gap
		// causing a spinner flicker.
		p.nativeLastWasToolResult.Store(ev.Type == EvToolResult)
		switch ev.Type {
		case EvTextDelta, EvText, EvThinkingDelta, EvThinking, EvToolCall, EvStatus:
			p.setFleetTurnAdmission(true)
			// Swap so we log only the false→true edge (one line per turn start),
			// not every streaming delta. See "[native-turn]" diagnostics below.
			if !p.nativeInTurn.Swap(true) {
				// A new turn START clears any background-workflow latch: the
				// workflow's completion notification fires exactly such a turn
				// (the agent reading its results), and a user steer is an explicit
				// move-on. The launching turn's OWN Workflow tool_call lands while
				// nativeInTurn is already true (no edge), so this never clears the
				// latch we're about to set below.
				p.nativeWorkflowActive.Store(false)
				// Arm the durable Codex twin on the SAME edge (no-op for Claude).
				p.markCodexTurnInFlight(true)
				log.Printf("[native-turn] pane=%s set=true trigger=%s", p.ID, ev.Type)
				p.publishTurnEdge()
			}
		case EvTurnEnd, EvError:
			p.setFleetTurnAdmission(false)
			// The settle edge. If this never fires for a turn (e.g. the
			// deny-with-reason AskUserQuestion path is suspected to emit no
			// turn_end), the client spinner stays stuck on agentMidTurn — this log
			// is how we confirm whether a settle event actually arrived.
			if p.nativeInTurn.Swap(false) {
				// Codex reported an END for this turn — including status
				// "interrupted" when the USER hit ESC. Disarming here is what
				// makes the durable flag mean "died mid-turn" and not merely
				// "the last turn was interrupted": a deliberate interrupt
				// clears it, a crash never gets the chance.
				p.markCodexTurnInFlight(false)
				log.Printf("[native-turn] pane=%s set=false trigger=%s subtype=%s", p.ID, ev.Type, ev.Subtype)
				p.publishTurnEdge()
			}
		}
		// In-flight tool count: a tool_call awaiting its tool_result means the agent
		// is working on a tool even while silent (a long bash run streams nothing),
		// so nativeWorkingNow must NOT settle then. Reset on a turn boundary so a
		// dropped / parallel / unbalanced result can't drift the count forever.
		switch ev.Type {
		case EvToolCall:
			p.nativeOpenTools.Add(1)
			// A Workflow tool_call launches background subagents that outlive this
			// turn — latch so the DTO carries WorkflowStatus once nativeInTurn
			// settles. Set AFTER the Swap(true) above so a (rare) turn that opens
			// with the Workflow call still ends up latched. Cleared on the next turn.
			if ev.Name == "Workflow" {
				p.nativeWorkflowActive.Store(true)
				log.Printf("[native-turn] pane=%s workflow launched (callId=%s)", p.ID, ev.CallID)
			}
		case EvToolResult:
			if p.nativeOpenTools.Add(-1) < 0 {
				p.nativeOpenTools.Store(0)
			}
		case EvTurnEnd, EvError:
			p.nativeOpenTools.Store(0)
		}
	}

	// Maintain the lock-free pending-approval pointer that computeState reads.
	// A live permission_request parks the approval (=> StateChoicePrompt); a
	// turn_end means the turn is over, so any unresolved approval is moot.
	switch ev.Type {
	case EvPermissionRequest:
		// Replayed (transcript-seeded) permission_requests must NOT re-park —
		// they were already resolved during the original live run, and the drain
		// edges (turn_end / cancel / teardown) don't re-fire after a post-settle
		// reconnect, so a re-park sticks as a GHOST: phantom choice-prompt state +
		// a false "waiting on approval" alert long after the turn finished. A
		// genuinely-still-pending approval survives a client reconnect via its
		// ORIGINAL live park (the map is process state, untouched by replay); after
		// a daemon restart the re-spawned --resume agent re-emits it LIVE. So this
		// guard loses nothing. Symmetric with the EvSessionInit !FromReplay guard
		// below — replayed events reconstruct DISPLAY, only live events drive STATE.
		if ev.FromReplay {
			break
		}
		// Park the request FIRST so resolveApproval (keyed by RequestID) has a
		// pending entry to claim. This runs OUTSIDE nativeMu (the post-fanout
		// switch), and setNativePending/resolveApproval take nativeApprovalMu +
		// agentWriteMu — never nativeMu — so there is no deadlock with the
		// parser fanout.
		p.setNativePending(&NativeApproval{
			RequestID: ev.RequestID,
			ToolUseID: ev.CallID,
			ToolName:  ev.ToolName,
			RawInput:  ev.Args,
		})
		// Per-pane "always allow this tool" opt-in: if the user has allowlisted
		// this exact tool, resolve it as ALLOW immediately (silently — the
		// tool_call event still rendered above) and DON'T leave a pending card.
		// This deliberate user opt-in bypasses the ARIA fire-budget cap; the
		// tool_call already fanned out, so the auto-allow is invisible like
		// ARIA auto-approve. If not allowlisted (or the resolve failed) the
		// request stays parked for the user — computeState already saw it via
		// nativePendingCount.
		//
		// SKIPPED for backlog-replayed requests (daemon restart / update
		// handoff re-reading the agent's stdout ring): the original live pass
		// already wrote the control_response, and writing a SECOND one for the
		// same request_id on every reattach is a stdin protocol error. The
		// re-park above still happens — if the request is genuinely still open
		// (claude is blocked waiting on the restarted daemon), the user answers
		// it; if it was already resolved, the replayed tool_result that follows
		// in the same backlog clears the ghost via takeNativePendingByToolUse.
		// `autoAllowHow` is the verdict computed ONCE at the top of this function
		// (and stamped on the event as WouldAutoAllow for the client). It is ""
		// unless every gate passed — non-empty RequestID, not a backlog replay,
		// not codex, a matching rule, no cautious veto — so this needs no gate of
		// its own, and the resolve can no longer disagree with what the client was
		// told.
		autoAllowed := p.maybeAutoAllowFromList(ev.RequestID, ev.ToolName, autoAllowHow)
		// Diagnostic: confirms the daemon DID emit/park this approval. If the
		// client still shows a dead/unclickable question for this callId, the drop
		// is client/transport-side; if autoAllowed=false for a tool you expected to
		// auto-approve, the allowlist desynced (see the existing "approvals:" logs).
		log.Printf("[native-turn] pane=%s permission_request callId=%s reqId=%s tool=%s autoAllowed=%v inTurn=%v",
			p.ID, ev.CallID, ev.RequestID, ev.ToolName, autoAllowed, p.nativeInTurn.Load())
	case EvUsage:
		// Trailing usage with inTurn=true and NO following set=false is the
		// signature of the stuck-spinner bug (the turn produced usage but never a
		// settling turn_end).
		log.Printf("[native-turn] pane=%s usage inTurn=%v", p.ID, p.nativeInTurn.Load())
	case EvTurnEnd, EvError:
		p.drainNativePending()
	case EvToolResult:
		// A tool_result is authoritative proof its approval was answered — by
		// this daemon, a predecessor daemon (update-handoff backlog replay), a
		// concurrent client, ARIA, or a deny that produced an error result. If
		// an entry for its tool_use is still parked, it's a ghost: clear it so
		// a reattach can't leave a phantom approval card whose click would
		// write a stale-RequestID control_response. Gated on the lock-free
		// count so the streaming hot path pays one atomic load.
		if p.hasNativePending() {
			if ap := p.takeNativePendingByToolUse(ev.CallID); ap != nil {
				log.Printf("approvals: cleared pending approval on its tool_result (pane=%s reqID=%s tool=%s callId=%s) — already answered elsewhere",
					p.ID, ap.RequestID, ap.ToolName, ev.CallID)
			}
		}
	case EvSessionInit:
		// Positive health signal for confirmSwitchHealthy: the agent came up and
		// spoke stream-json. Set regardless of FromReplay — the flag is per-Pane
		// (fresh struct per respawn), so any init on THIS pane means THIS process
		// is alive.
		p.nativeGotInit.Store(true)
		// The live stream's system/init is the FIRST place the daemon learns a
		// fresh native pane's claude session id (its hooks may never reach us).
		// Persist it so switch-to-terminal can `--resume`, a daemon restart can
		// reattach the conversation, and historyBackfill's transcript seed (which
		// gates on a non-empty ClaudeSessionID) can run. Seeded/replayed events
		// must NOT clobber the live id, so guard on !FromReplay.
		if !ev.FromReplay {
			p.persistNativeSessionID(ev.SessionID)
		}
	}
	return seq
}

// persistNativeSessionID records a claude session id observed on the live native
// stream and persists the pane if it changed. It mirrors hooks.go's writer path
// (SetClaudeSessionID + a pane persist) — the only OTHER place this id is set —
// but reaches the store through the pane's onPromptsCommit hook (the single
// persist callback the Manager wires onto a pane; emitNativeEvent runs in the
// parser goroutine, which holds no store reference of its own).
//
// SetClaudeSessionID is idempotent: it trims, ignores empty, and returns false
// when the id is unchanged, so this no-ops on every redundant init line and the
// persist (and its piggybacked pane-prompt publish) fires at most once per id.
func (p *Pane) persistNativeSessionID(sessionID string) {
	if effectiveAgentKind(p.Agent) == agentCodex {
		if p.SetCodexThreadID(sessionID) && p.onPromptsCommit != nil {
			p.onPromptsCommit(p)
		}
		return
	}
	if effectiveAgentKind(p.Agent) == agentMuse {
		if p.SetMuseSessionID(sessionID) && p.onPromptsCommit != nil {
			p.onPromptsCommit(p)
		}
		return
	}
	changed, prior := p.setClaudeSessionIDReportingPrior(sessionID)
	if !changed {
		return
	}
	// Reset semantics for the per-pane "always allow" set:
	//   - prior == "": the FIRST session id this pane has learned. That's a
	//     restore/resume (or the initial spawn) continuing the SAME
	//     conversation — KEEP any allowlist restored from disk.
	//   - prior != "": the session id ROTATED to a genuinely different value,
	//     i.e. a brand-NEW conversation started in this pane. CLEAR the
	//     allowlist so the new conversation doesn't inherit the old one's
	//     auto-allow opt-ins.
	// (Switch-mode Terminal↔Native keeps the same id via --resume, so it falls
	// in the "keep" branch — the user's opt-ins survive a flip, as intended.)
	if prior != "" {
		p.clearNativeAllowlist()
		log.Printf("approvals: cleared allowlist on session rotation (pane=%s %s→%s)", p.ID, prior, sessionID)
		// Same edge, second consumer: a brand-new conversation gets the
		// one-shot digest of work this pane already delegated. Belt-and-braces
		// alongside the SessionStart hook (hooks.go) — a native pane whose
		// hooks are unregistered still has its own init line. The nudger keys
		// on the session id, so both firing produces one digest.
		if p.onClaudeSessionRotated != nil {
			p.onClaudeSessionRotated(p.ID, sessionID)
		}
	}
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
}

// ringAppendLocked appends ev to the bounded native ring, dropping the oldest
// entries until both the event-count and byte budgets are satisfied. Caller
// holds nativeMu.
func (p *Pane) ringAppendLocked(ev NativeEvent) {
	sz := approxEventBytes(ev)
	p.nativeRing = append(p.nativeRing, ev)
	p.nativeRingBytes += sz
	for len(p.nativeRing) > nativeRingMaxEvents ||
		(p.nativeRingBytes > nativeRingMaxBytes && len(p.nativeRing) > 1) {
		drop := p.nativeRing[0]
		p.nativeRingBytes -= approxEventBytes(drop)
		if p.nativeRingBytes < 0 {
			p.nativeRingBytes = 0
		}
		p.nativeRing = p.nativeRing[1:]
	}
	// Periodically re-slice into a fresh array so the dropped prefix's backing
	// memory is released rather than pinned by the growing slice header.
	if cap(p.nativeRing) > 2*nativeRingMaxEvents && len(p.nativeRing) <= nativeRingMaxEvents {
		compact := make([]NativeEvent, len(p.nativeRing))
		copy(compact, p.nativeRing)
		p.nativeRing = compact
	}
}

// approxEventBytes estimates a NativeEvent's marshaled size for ring byte
// accounting. The raw payload fields (Args/RawEvent/Output) dominate; a small
// flat overhead covers the rest of the envelope.
func approxEventBytes(ev NativeEvent) int {
	n := 64 + len(ev.Text) + len(ev.Content) + len(ev.Args) + len(ev.RawEvent) + len(ev.Output)
	return n
}

// SubscribeNative registers a native-event subscriber and returns the live
// channel plus a cancel func. The caller (the SSE handler) first drains the
// returned ring snapshot for backfill, then ranges the channel for live deltas.
// Returns ok=false if the pane is not native (no parser running).
func (p *Pane) SubscribeNative() (id int64, snapshot []NativeEvent, ch <-chan NativeEvent, cancel func(), ok bool) {
	p.nativeMu.Lock()
	if p.nativeSubs == nil {
		p.nativeMu.Unlock()
		return 0, nil, nil, nil, false
	}
	c := make(chan NativeEvent, nativeSubBuf)
	p.nextNativeSub++
	id = p.nextNativeSub
	p.nativeSubs[id] = c
	snap := make([]NativeEvent, len(p.nativeRing))
	copy(snap, p.nativeRing)
	p.nativeMu.Unlock()

	// A WS reconnect / browser refresh replays this ring snapshot, and the ring
	// still holds already-resolved permission_requests — without this they
	// re-render as fresh, actionable approval cards on every refresh (the
	// "approval spam on refresh" bug). Keep only requests still in the live
	// pending set, exactly like historyBackfill's replay. This is the WS/SSE
	// backfill complement to that HTTP-history fix — both transports replay the
	// same ring, so both must drop resolved cards. Called AFTER releasing
	// nativeMu because dropResolvedApprovalCards → listNativePending takes
	// nativeApprovalMu; reading it here keeps the two native locks from nesting.
	snap = dropResolvedApprovalCards(p, snap)

	cancel = func() {
		p.nativeMu.Lock()
		if existing, ok := p.nativeSubs[id]; ok {
			delete(p.nativeSubs, id)
			close(existing)
		}
		p.nativeMu.Unlock()
	}
	return id, snap, c, cancel, true
}

// NativeSince returns ring events with Seq > since, oldest-first. Used by the
// /native/history?since= gap-fill for events still in the ring. Events below the
// ring floor are re-parsed from the transcript by the caller (historyBackfill),
// which detects the floor-passed case and prepends the transcript seed.
func (p *Pane) NativeSince(since uint64) []NativeEvent {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if p.nativeSubs == nil {
		return nil
	}
	out := make([]NativeEvent, 0, len(p.nativeRing))
	for _, ev := range p.nativeRing {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out
}

// NativeRingFloor returns the lowest Seq the ring can still serve — i.e. the
// minimum retained Seq. historyBackfill compares this against the client's
// `since` to detect that the ring has rolled PAST `since` (events aged out),
// which NativeSince alone can't reveal: once the whole ring has rotated beyond
// `since`, NativeSince returns an empty slice and the old `events[0].Seq>since+1`
// heuristic silently reports not-truncated, dropping all history in the gap.
//
// Derived from existing state under nativeMu (no separate floor field):
//   - ring non-empty  -> floor = oldest retained event's Seq.
//   - ring empty      -> nothing at or below nativeSeq is retained, so the floor
//     is nativeSeq+1. (nativeSeq==0 on a never-emitted pane yields floor 1, so a
//     since==0 request reads as not-truncated and takes the fresh-seed path.)
//
// This is exact: ringAppendLocked only ever drops from the front, so the retained
// Seqs are always the contiguous tail [floor .. nativeSeq].
func (p *Pane) NativeRingFloor() uint64 {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if len(p.nativeRing) > 0 {
		return p.nativeRing[0].Seq
	}
	return p.nativeSeq + 1
}

// NativeAdapterRef returns the pane's adapter (for the input/approval encoders)
// and whether the pane is native. Read under nativeMu.
func (p *Pane) NativeAdapterRef() (NativeAdapter, bool) {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if p.nativeAdapter == nil {
		return nil, false
	}
	return p.nativeAdapter, true
}

var errNativeAgentNotReady = errors.New("native agent is not ready to accept input")

type nativeSubmitResult struct {
	Bytes     int
	FreshTurn bool
	UserSeq   uint64
}

// submitNativeInput is the single semantic submission path for Focus panes.
// HTTP composer sends and scheduled prompts both use it, so Claude stream-json
// and Codex app-server panes never receive raw terminal keystrokes.
func (p *Pane) submitNativeInput(text string) (nativeSubmitResult, error) {
	return p.submitNativeInputWithContext("", text)
}

func (p *Pane) submitNativeInputWithContext(context, text string) (nativeSubmitResult, error) {
	adapter, ok := p.NativeAdapterRef()
	if !ok {
		return nativeSubmitResult{}, errNativeAgentNotReady
	}

	freshTurn := !p.nativeInTurnNow()
	envelope := []byte(nil)
	suppressEcho := true
	wire := text
	if context != "" {
		wire = context + "\n\n" + text
	}
	if driver, isCodex := adapter.(*CodexAdapter); context == "" && isCodex && isNativeCompactCommand(text) {
		// Codex app-server exposes compaction as a dedicated control RPC. Sending
		// the literal text as a user turn only prompts the model to talk about it.
		envelope = driver.EncodeCompact()
		suppressEcho = false // a control RPC has no user-message replay echo
	} else {
		envelope = adapter.EncodeUserTurn(wire)
	}
	if len(envelope) == 0 {
		return nativeSubmitResult{}, errNativeAgentNotReady
	}

	// Arm echo suppression before handing an ordinary user turn to the agent;
	// its replay echo can arrive on the parser goroutine as soon as the frame is
	// written. A compact control RPC has no echo, so arming one would leave a
	// stale entry capable of swallowing a later literal "/compact" user event.
	suppressedTexts := []string{wire}
	if context != "" {
		// ClaudeAdapter maps the replayed combined wire back into the same two
		// rows emitted below, so suppress those rows individually.
		suppressedTexts = []string{context, text}
	}
	if suppressEcho {
		for _, echoed := range suppressedTexts {
			p.expectEchoSuppress(echoed)
		}
	}
	if _, err := p.writeNativeTurn(envelope); err != nil {
		if suppressEcho {
			for _, echoed := range suppressedTexts {
				p.consumeEchoSuppress(echoed)
			}
		}
		return nativeSubmitResult{}, err
	}

	p.recordCommittedPrompts(text)
	p.markNativeUserTurn()
	p.clearSwitchNotice()
	p.SetComposerNonEmpty(false, time.Now())
	if context != "" {
		p.emitNativeEvent(NativeEvent{Type: EvUserText, Text: context})
	}
	userSeq := p.emitNativeEvent(NativeEvent{Type: EvUserText, Text: text})
	log.Printf("[native-input] pane=%s freshTurn=%v userSeq=%d textLen=%d",
		p.ID, freshTurn, userSeq, len(text))

	return nativeSubmitResult{Bytes: len(envelope), FreshTurn: freshTurn, UserSeq: userSeq}, nil
}

// splitAmbientDelegation recognizes the single-line delegation prefix used on
// the agent wire and keeps every byte of the user's message after the first
// blank-line separator.
func splitAmbientDelegation(s string) (context, user string, ok bool) {
	i := strings.Index(s, "\n\n")
	if i < 0 {
		return "", s, false
	}
	context = s[:i]
	if !isAmbientDelegationLine(context) {
		return "", s, false
	}
	return context, s[i+2:], true
}

// isAmbientDelegationLine recognizes the two exact nudge headers. A broad
// "[solu] delegated work" prefix is insufficient because a user may quote
// those words in an ordinary multi-paragraph prompt.
func isAmbientDelegationLine(line string) bool {
	if strings.ContainsAny(line, "\r\n") || !strings.Contains(line, " (wft_") {
		return false
	}
	entryTail := line
	if !strings.HasSuffix(entryTail, ")") {
		more := strings.LastIndex(entryTail, "; +")
		if more < 0 || !strings.HasSuffix(entryTail, " more") {
			return false
		}
		count := strings.TrimSuffix(entryTail[more+3:], " more")
		if count == "" {
			return false
		}
		for _, r := range count {
			if r < '0' || r > '9' {
				return false
			}
		}
		entryTail = entryTail[:more]
		if !strings.HasSuffix(entryTail, ")") {
			return false
		}
	}
	if strings.HasPrefix(line, "[solu] delegated work update: \"") {
		return entryTail == line // single-update lines never carry "+N more"
	}
	const prefix = "[solu] delegated work: "
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	rest := strings.TrimPrefix(line, prefix)
	sep := strings.Index(rest, " updates — \"")
	if sep <= 0 {
		return false
	}
	for _, r := range rest[:sep] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isNativeCompactCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && fields[0] == "/compact"
}

// shutdownNative tears down the parser goroutine, closes the tee + subscriber
// channels, frees the ring, and clears the pending approval. Called from
// markExited. Idempotent and leak-free: the goroutine exits on stop close (and
// also p.done), subscribers' SSE loops terminate on channel close.
func (p *Pane) shutdownNative() {
	// Detach the tee from the hot path FIRST, under subsMu, so the readLoop
	// stops sending before we close the channel (no send-on-closed panic).
	p.subsMu.Lock()
	tee := p.nativeTeeCh
	p.nativeTeeCh = nil
	p.subsMu.Unlock()

	p.nativeMu.Lock()
	stop := p.nativeStop
	p.nativeStop = nil
	subs := p.nativeSubs
	p.nativeSubs = nil
	p.nativeRing = nil
	p.nativeRingBytes = 0
	p.nativeMu.Unlock()

	if stop != nil {
		close(stop)
	}
	// Close the tee AFTER stop so the parser goroutine has already been told to
	// exit; a residual buffered chunk is harmless (goroutine returns on stop).
	if tee != nil {
		close(tee)
	}
	for _, ch := range subs {
		close(ch)
	}
	p.drainNativePending()
	// Clear the mid-turn flag on teardown so a later resume of this session
	// (a fresh pane reusing the id) can never inherit a stuck "working"
	// spinner — the v0.0.595 failure mode. The fresh pane gets a zero-valued
	// flag anyway; this is belt-and-suspenders for any reuse path.
	p.nativeInTurn.Store(false)
	p.setFleetTurnAdmission(false)
}

// markNativeUserTurn flags the native pane as mid-turn the instant a user turn
// is submitted (paneNativeInput), BEFORE any byte streams back — so a switch
// fired immediately after sending sees the pane as working. Lock-free.
func (p *Pane) markNativeUserTurn() {
	// Swap (not Store) so the real-time turn-state edge fires only on the
	// false→true transition (a mid-turn steer where it's already true publishes
	// nothing). The flag still ends true either way.
	edge := !p.nativeInTurn.Swap(true)
	// Reset the idle clock so the just-submitted turn (no events streamed back
	// yet) isn't immediately mistaken for a settled/stuck turn by nativeWorkingNow.
	p.nativeLastEventNano.Store(time.Now().UnixNano())
	// A user submit while a background workflow is shown is an explicit move-on —
	// drop the latch so the footer reverts to normal turn state for this new turn.
	p.nativeWorkflowActive.Store(false)
	// Arm the durable Codex flag from the SUBMIT side too. This is not
	// redundant with the streaming turn-start edge: markNativeUserTurn has
	// already set nativeInTurn, so that edge's Swap(true) reports no
	// transition and never runs — leaving a turn that dies before its first
	// streamed event (the widest crash window: model still thinking) with no
	// record that it was ever in flight. Idempotent for a mid-turn steer.
	p.markCodexTurnInFlight(true)
	if edge {
		p.publishTurnEdge()
	}
}

// nativeInTurnNow reports the lock-free in-turn flag (native panes only).
func (p *Pane) nativeInTurnNow() bool { return p.nativeInTurn.Load() }

// publishTurnEdge fires the Manager's real-time pane-state edge callback (wired at
// spawn/attach) on a nativeInTurn transition. Nil-guarded so tests / unwired paths
// are a no-op. Called ONLY from the edge-detecting Swap/CompareAndSwap blocks, so it
// fires at most once per real true↔false transition (never per delta). Runs on the
// parser / HTTP goroutine — never inside nativeWorkingNow — so the callback's
// computeState()/agentMidTurnForState() cannot re-enter the idle-settle path.
func (p *Pane) publishTurnEdge() {
	if p.onTurnStateEdge != nil {
		p.onTurnStateEdge(p)
	}
}

// markCodexTurnInFlight arms/disarms the DURABLE Codex turn flag and persists
// the change (see Pane.codexTurnInFlight). A no-op for Claude and terminal
// panes: codex-ness is decided by the live adapter's type, not by reading
// p.Agent, so it stays correct without touching p.mu on the turn path.
//
// It is deliberately wired ONLY to the two AUTHORITATIVE edges — a turn
// starting, and Codex reporting turn/completed (EvTurnEnd/EvError) — and NOT
// to publishTurnEdge, which also fires for approval park/resolve and for the
// idle-settle heuristic. Mirroring the idle settle would clear the flag on a
// merely-quiet turn (an agent silent past nativeSettleTimeout is still alive),
// and mirroring teardown would erase the crash evidence at exactly the moment
// a crash produces it.
//
// Persistence fires only on a real transition, so a turn costs at most two
// extra panes.json writes — the same order as the per-prompt write already
// happening on this path.
func (p *Pane) markCodexTurnInFlight(inFlight bool) {
	adapter, ok := p.NativeAdapterRef()
	if !ok {
		return
	}
	if _, isCodex := adapter.(*CodexAdapter); !isCodex {
		return
	}
	if p.codexTurnInFlight.Swap(inFlight) == inFlight {
		return // no transition — nothing to persist
	}
	if p.onCodexTurnFlagCommit != nil {
		p.onCodexTurnFlagCommit(p)
	}
}

const nativeEchoSuppressCap = 16 // safety bound (pathological no-echo) — see below

// expectEchoSuppress records that the daemon emitted `text` as a user_text at
// input time, so the matching `--replay-user-messages` echo should be DROPPED when
// it later arrives (we'd otherwise double-render the prompt). Armed on EVERY submit
// now — fresh turn AND mid-turn steer (paneNativeInput no longer gates on freshTurn,
// since a stale-true nativeInTurn used to drop the prompt entirely). Bounded: if the
// CLI ever stopped echoing, entries would accumulate; cap to the most recent N so a
// stuck stream can't grow this unbounded.
func (p *Pane) expectEchoSuppress(text string) {
	p.nativeEchoMu.Lock()
	p.nativeEchoSuppress = append(p.nativeEchoSuppress, text)
	if n := len(p.nativeEchoSuppress); n > nativeEchoSuppressCap {
		p.nativeEchoSuppress = p.nativeEchoSuppress[n-nativeEchoSuppressCap:]
	}
	p.nativeEchoMu.Unlock()
}

// consumeEchoSuppress drops ONE matching expectation and reports whether the
// caller should suppress this echo. Content-FIFO (first matching text), so two
// identical fresh prompts each suppress exactly one echo.
func (p *Pane) consumeEchoSuppress(text string) bool {
	p.nativeEchoMu.Lock()
	defer p.nativeEchoMu.Unlock()
	for i, t := range p.nativeEchoSuppress {
		if t == text {
			p.nativeEchoSuppress = append(p.nativeEchoSuppress[:i], p.nativeEchoSuppress[i+1:]...)
			return true
		}
	}
	return false
}

// nativeWorkingNow settle thresholds — a GENEROUS SAFETY NET, not a fast settle.
// A normal turn settles INSTANTLY on its deterministic edge (turn_end / error, or
// the stop_reason=end_turn raw peek in processNativeLine); this timeout only catches
// the rare case where nativeInTurn hangs true with NO such edge (e.g. a respawn
// mid-turn, or a turn that ends with no settle line). It must therefore be LONGER
// than any real mid-turn silence. processNativeLine now resets the idle clock on
// EVERY live line (not just stamped event types), so the clock tracks TRUE process
// silence — and claude emits some line every ~1–7s during generation. But the
// generation stream itself can pause (a 6.8s no-line gap was measured mid-thinking
// on a SMALL context; it scales with context size, and a 20.4s gap on the big aria
// repo fired the old 20s timeout and DROPPED the spinner mid-turn). So the backstop
// is set well above any plausible scaled thinking-pause: the deterministic edges
// handle every normal end instantly, so a long net costs nothing on a real turn and
// only delays recovery in the genuinely-stuck anomaly. Two tiers kept (post-tool_result
// gaps run a touch longer), both consulted only when no tool is in flight (a running
// tool is silent but genuinely working — guarded separately by nativeOpenTools).
const (
	nativeSettleTimeout = 45 * time.Second
	nativeGapAllowance  = 60 * time.Second
)

// nativeWorkingNow reports whether a native agent is genuinely mid-turn, applying
// the stuck-flag settle. Shared by IsAgentWorking (the graceful-switch busy
// guard) and agentMidTurnForState (the DTO agentMidTurn field) so both agree.
// Order matters: a tool in flight is work even while silent; only with NO open
// tool does a long silence count as a settled turn — and then we clear the stuck
// flag so subsequent reads are cheap and the spinner stops.
func (p *Pane) nativeWorkingNow() bool {
	if !p.nativeInTurnNow() {
		return false
	}
	if p.nativeOpenTools.Load() > 0 {
		return true
	}
	// Snappy normal settle, but give the post-tool_result gap (model generating
	// its next message, no events yet) the longer allowance so we don't flicker
	// the spinner off mid-gap on a normal turn.
	timeout := nativeSettleTimeout
	if p.nativeLastWasToolResult.Load() {
		timeout = nativeGapAllowance
	}
	if last := p.nativeLastEventNano.Load(); last > 0 && time.Since(time.Unix(0, last)) > timeout {
		// CompareAndSwap so exactly one concurrent caller logs the settle edge
		// (this runs on the ~100×/sec DTO path).
		if p.nativeInTurn.CompareAndSwap(true, false) {
			log.Printf("[native-turn] pane=%s set=false trigger=idle-settle silentMs=%d",
				p.ID, time.Since(time.Unix(0, last)).Milliseconds())
		}
		return false
	}
	return true
}

// IsAgentWorking reports whether the pane's agent is mid-turn, dispatching by
// Kind. This is the graceful-switch busy guard: a true result means the switch
// should interrupt + wait for the turn to settle before the kill→respawn so the
// in-flight turn isn't silently aborted with a half-written transcript.
//
//   - Native (Kind=="native"): the explicit nativeInTurn flag — set on a
//     submitted user turn / streaming assistant message, cleared on
//     turn_end/error. More precise than recency: a headless native pane emits
//     no PTY bytes, so recency would read "idle" the moment streaming pauses.
//   - Terminal (Kind==""): computeState ∈ {StateBusy, StateRecent} — recent PTY
//     output means the TUI agent is generating — UNLESS Claude's prompt box is on
//     screen, which means the turn was handed back (the recent bytes are the idle
//     prompt's cursor/footer redraws, not generation). StateChoicePrompt /
//     awaiting / idle are NOT "working" (the user, not the agent, owns the turn).
func (p *Pane) IsAgentWorking() bool {
	if p.Kind == "native" {
		return p.nativeWorkingNow()
	}
	return p.agentMidTurnForState(p.computeState())
}

// agentMidTurnForState answers "would switching the renderer NOW interrupt live
// agent work?" given an ALREADY-COMPUTED runtime state, so the per-pane DTO path
// can reuse the state it already derived instead of stripping the buffer twice.
//
//   - Native: the explicit in-turn flag (authoritative; no PTY recency).
//   - Terminal: StateBusy/StateRecent means recent PTY output, but a visible
//     Claude prompt box means the turn was handed back — the redraws are the idle
//     prompt, not generation — so it is NOT mid-turn. This is what stops the 3s
//     post-turn recency tail from reading as "working" (the bug where an idle
//     pane offered "switch when done" and a one-click switch stamped a bogus
//     "interrupted" notice). Every other state (choice-prompt, awaiting-input,
//     idle, exited) is owned by the user, not the agent.
func (p *Pane) agentMidTurnForState(state PaneRuntimeState) bool {
	if p.Kind == "native" {
		return p.nativeWorkingNow()
	}
	switch state {
	case StateBusy, StateRecent:
		return !p.claudePromptVisible()
	default:
		return false
	}
}

// switchNotice is the transient "something discontinuous happened to this
// conversation" marker for a pane. It rides the pane DTO (switchNotice) so the
// client can show the right banner, and is cleared on the next user turn (or an
// explicit ack). Reason discriminates the copy:
//
//	""             — legacy: a mode switch interrupted an in-flight turn.
//	"resume-repair" — a resume found (and synthetically answered) a dangling
//	                  tool_use: the previous response was cut off by a daemon
//	                  restart / crash, not by a switch.
//	"fresh-start"   — a requested `--resume` was DROPPED (no on-disk transcript
//	                  yet, or the session is live in another pane) and the pane
//	                  started a brand-new conversation. Interrupted=false: there
//	                  is nothing to continue, the banner is informational.
type switchNotice struct {
	Interrupted bool   `json:"interrupted"`
	FromKind    string `json:"fromKind"`         // the SOURCE kind that was interrupted: "native" | "terminal"
	At          int64  `json:"at"`               // unix millis when the interrupt happened
	Reason      string `json:"reason,omitempty"` // "" | "resume-repair" | "fresh-start"
}

// setSwitchNotice records that a mode switch interrupted an in-flight turn on
// this pane. fromKind is the SOURCE kind (the mode the interrupted turn was
// running in): "native" or "terminal" (the empty terminal Kind is normalized to
// "terminal" for the client contract).
func (p *Pane) setSwitchNotice(fromKind string) {
	p.setSwitchNoticeReason(fromKind, "", true)
}

// setSwitchNoticeReason is the generalized notice writer (see switchNotice.Reason).
func (p *Pane) setSwitchNoticeReason(fromKind, reason string, interrupted bool) {
	if fromKind == "" {
		fromKind = "terminal"
	}
	p.switchNoticeMu.Lock()
	p.pendingSwitchNotice = &switchNotice{
		Interrupted: interrupted,
		FromKind:    fromKind,
		At:          time.Now().UnixMilli(),
		Reason:      reason,
	}
	p.switchNoticeMu.Unlock()
}

// clearSwitchNotice drops any outstanding switch notice — called when the next
// user turn is written (the "Continue" was effectively taken) or on explicit ack.
func (p *Pane) clearSwitchNotice() {
	p.switchNoticeMu.Lock()
	p.pendingSwitchNotice = nil
	p.switchNoticeMu.Unlock()
}

// switchNoticeSnapshot returns a copy of the pending switch notice (or nil) for
// the DTO. Copied under the lock so the DTO marshal never races a concurrent
// set/clear.
func (p *Pane) switchNoticeSnapshot() *switchNotice {
	p.switchNoticeMu.Lock()
	defer p.switchNoticeMu.Unlock()
	if p.pendingSwitchNotice == nil {
		return nil
	}
	cp := *p.pendingSwitchNotice
	return &cp
}

// isNativeCancelLine reports whether a raw JSONL line is a top-level
// control_cancel_request (the agent's interrupt ack). Cheap top-level decode.
func isNativeCancelLine(line []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return false
	}
	return probe.Type == "control_cancel_request"
}
