package main

import (
	"context"
	"time"
)

// startActivityWatcher publishes "pane-state" uiBus events when a pane
// transitions between meaningful runtime states. "Meaningful" is tighter
// than "any transition" — the raw state machine (busy / recent / idle /
// awaiting-input / choice-prompt / exited) produces a lot of
// uninteresting noise (every short burst cycles busy → recent → idle in
// 4s), and the v1 feed drowned the signal in that noise.
//
// Filter policy:
//   - SKIP any transition where either side is `recent`. It's a 2-second
//     transient the state machine uses to bridge busy → idle; the user
//     just wants to see "busy → idle", not the intermediate step.
//   - SKIP transitions held < 1s — the ticker cadence is 2s so anything
//     at the 1s floor is almost certainly us catching a bounce rather
//     than a real moment of signal.
//   - KEEP: idle → busy, busy → idle (with duration), busy/idle →
//     awaiting-input / choice-prompt, and the reverse. These are the
//     orchestration-relevant edges.
//
// The Pusher keeps its own independent state tracking for push
// notifications (rate-limited, attention-only); this watcher is just
// for the in-app feed.
// onReplyComplete, when non-nil, is invoked with the pane each time a Claude
// turn finishes (the same edge that emits pane.reply_complete). main.go wires it
// to refreshPaneAITitleCache so the pane's generated title re-warms once per
// completed turn — off the request path, naturally rate-limited.
func startActivityWatcher(ctx context.Context, mgr *Manager, bus *uiBus, onReplyComplete func(*Pane)) {
	requireNormalBootEngine("activity.startActivityWatcher")
	last := map[string]PaneRuntimeState{}
	enteredAt := map[string]time.Time{}
	// Separate from enteredAt: busyEnteredAt is pinned at the moment
	// the pane FIRST became busy and held across the busy→recent
	// transient — so when recent→idle finally fires, we can emit a
	// synthetic "busy → idle (held N)" with the proper duration of
	// the full busy run, not just the 2s recent window. Cleared on
	// idle, overwritten when busy is re-entered.
	busyEnteredAt := map[string]time.Time{}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			for _, pane := range mgr.List() {
				cur := pane.computeStateCached()
				prev, seen := last[pane.ID]
				last[pane.ID] = cur

				if !seen {
					enteredAt[pane.ID] = now
					if cur == StateBusy {
						busyEnteredAt[pane.ID] = now
					}
					continue
				}

				// Track busy start, irrespective of whether we emit
				// anything — needed so a later recent→idle can compute
				// the true busy duration.
				if prev != StateBusy && cur == StateBusy {
					busyEnteredAt[pane.ID] = now
				}

				if cur == prev {
					continue
				}

				duration := now.Sub(enteredAt[pane.ID])
				enteredAt[pane.ID] = now

				// Synthetic "busy → idle" edge. The state machine goes
				// busy → recent → idle; the `recent` intermediate is
				// noise for the user, but the recent → idle moment IS
				// the meaningful "task finished" signal. Rewrite it
				// into a busy→idle event using the full busy duration.
				if prev == StateRecent && cur == StateIdle {
					busyStart, hadBusy := busyEnteredAt[pane.ID]
					delete(busyEnteredAt, pane.ID)
					if !hadBusy {
						// Never saw busy for this pane (e.g. we started
						// tracking mid-recent). Nothing to rewrite; drop.
						continue
					}
					busyDur := now.Sub(busyStart)
					// Only emit for meaningful busy periods. Sub-5s
					// "tasks" are usually just a prompt echo or a
					// single quick tool.
					const busyFinishThreshold = 5 * time.Second
					if busyDur < busyFinishThreshold {
						continue
					}
					// Scope to Claude panes. Prefer the ClaudeSessionID
					// marker (set by Claude's hook payloads) over the
					// process-descendant probe; fall back if absent.
					snap := pane.Snapshot()
					isClaude := snap.ClaudeSessionID != "" || pane.HasClaudeDescendant()
					if !isClaude {
						continue
					}
					amt := pane.agentMidTurnForState(StateIdle)
					bus.publish(uiEvent{
						Type:         "pane-state",
						PaneID:       snap.ID,
						PaneTitle:    snap.Title,
						Ts:           now.UnixMilli(),
						FromState:    string(StateBusy),
						ToState:      string(StateIdle),
						DurationMs:   busyDur.Milliseconds(),
						AgentMidTurn: &amt,
					})
					bus.publish(uiEvent{
						Type:       "pane.reply_complete",
						PaneID:     snap.ID,
						PaneTitle:  snap.Title,
						Ts:         now.UnixMilli(),
						DurationMs: busyDur.Milliseconds(),
					})
					if onReplyComplete != nil {
						onReplyComplete(pane)
					}
					continue
				}

				// Everything else: drop by default. Only attention-
				// state transitions survive — "task started" (idle→busy)
				// is noisy and doesn't add information (you can see the
				// pane go busy in the UI). What users care about is
				// "Claude needs me" and "Claude finished."
				if !involvesAttention(prev, cur) {
					continue
				}
				// Minimum-hold guard for attention-state transitions
				// too — prevents sub-1s bounces mid-prompt-rendering
				// from multiplying into spam.
				if duration < 1*time.Second {
					continue
				}
				// Attention on a non-Claude pane is an edge case
				// (something else matched the prompt regex); allow.

				snap := pane.Snapshot()
				amt := pane.agentMidTurnForState(cur)
				bus.publish(uiEvent{
					Type:         "pane-state",
					PaneID:       snap.ID,
					PaneTitle:    snap.Title,
					Ts:           now.UnixMilli(),
					FromState:    string(prev),
					ToState:      string(cur),
					DurationMs:   duration.Milliseconds(),
					AgentMidTurn: &amt,
				})
			}

			// Drop tracking for panes that went away.
			for id := range last {
				if _, ok := mgr.Get(id); !ok {
					delete(last, id)
					delete(enteredAt, id)
					delete(busyEnteredAt, id)
				}
			}
		}
	}
}

// isMeaningfulTransition — retained for legacy callers; the watcher
// inlines its own tighter decision now. The two things the watcher
// guards with this helper aren't applicable under the new flow, so
// this is effectively unused. Kept for now to avoid an unrelated
// refactor; can be deleted in a follow-up.
func isMeaningfulTransition(prev, cur PaneRuntimeState, held time.Duration) bool {
	_ = prev
	_ = cur
	_ = held
	return true
}

// involvesAttention returns true if either side of the transition is
// an attention-requiring state. Lets us still log the "shell just
// flipped to awaiting-input" edge for non-Claude panes — the rare case
// where a bash readline or some other TUI happens to match Claude's
// prompt regex.
func involvesAttention(prev, cur PaneRuntimeState) bool {
	for _, s := range []PaneRuntimeState{prev, cur} {
		if s == StateAwaitingInput || s == StateChoicePrompt {
			return true
		}
	}
	return false
}
