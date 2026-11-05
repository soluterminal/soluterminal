package main

import (
	"context"
	"sync/atomic"
	"time"
)

// Confirm plumbing shared by ARIA's autonomous action tools
// (send_input for terminal panes, resolve_permission for native
// panes). A mutating tool funnels through awaitConfirm so the user
// gets an SSE-driven cancel window (autoEmitter re-emits the
// needs_confirm as an auto-countdown card) before the action fires.

// confirmOpts feeds the awaitConfirm helper. Tool drives the SSE event
// type the UI switches on; Summary is the human-line preview. Extra
// adds tool-specific fields (pane id/title, input preview); leave it
// nil if Summary alone is enough.
type confirmOpts struct {
	Tool           string
	Summary        string
	Risk           string
	Extra          map[string]any
	TurnApproveAll *atomic.Bool
	AutoConfirm    bool
}

// awaitConfirm registers a needs_confirm dialog and blocks for the user
// decision (or the per-turn auto-approve flag).
//
// Returns:
//   - confirmID — opaque id; the caller must emit confirm_resolved with
//     this id once the action completes (success OR error path).
//   - approved — true if the user said yes, OR if turnApproveAll is set.
//     False covers an explicit reject.
//   - auto — true when approval came from the auto-countdown, not a
//     click; callers stamp this on confirm_resolved so the UI can render
//     an "auto-approved" label instead of "sent".
//   - terminal — true if the request was cancelled (ctx.Done) or timed
//     out. The helper has already emitted confirm_resolved so the caller
//     should return early without firing its own confirm_resolved.
func (a *Aria) awaitConfirm(
	ctx context.Context,
	toolUseID string,
	opts confirmOpts,
	emit func(map[string]any),
) (confirmID string, approved bool, auto bool, terminal bool) {
	confirmID, approved, auto, terminal, _ = a.awaitConfirmDecision(ctx, toolUseID, opts, emit)
	return
}

func (a *Aria) awaitConfirmDecision(
	ctx context.Context,
	toolUseID string,
	opts confirmOpts,
	emit func(map[string]any),
) (confirmID string, approved bool, auto bool, terminal bool, explicitReject bool) {
	confirmID = newConfirmID()
	payload := map[string]any{
		"type":        "needs_confirm",
		"id":          confirmID,
		"tool_use_id": toolUseID,
		"tool":        opts.Tool,
		"summary":     opts.Summary,
		"risk":        confirmRiskLevel(opts),
	}
	for k, v := range opts.Extra {
		payload[k] = v
	}

	// Activity-feed mirror so the pending action shows up even without
	// a dedicated ARIA surface mounted.
	a.uiBus.publish(uiEvent{
		Type: "aria-needs-confirm",
		Tool: opts.Tool,
		Ts:   time.Now().UnixMilli(),
	})

	// Auto-approve fast path. Emit needs_confirm anyway so the UI can
	// render the action, then immediately return approved=true. The
	// caller still emits confirm_resolved AFTER doing the work.
	if opts.TurnApproveAll != nil && opts.TurnApproveAll.Load() {
		emit(payload)
		return confirmID, true, true, false, false
	}

	paneID, _ := opts.Extra["pane_id"].(string)
	pc := &ariaPendingConfirm{
		ch:             make(chan bool, 1),
		turnApproveAll: opts.TurnApproveAll,
		autoConfirm:    opts.AutoConfirm,
		paneID:         paneID,
	}
	a.mu.Lock()
	a.confirms[confirmID] = pc
	a.mu.Unlock()
	emit(payload)

	select {
	case ok := <-pc.ch:
		return confirmID, ok, opts.AutoConfirm && ok, false, pc.explicitReject
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.confirms, confirmID)
		a.mu.Unlock()
		return confirmID, false, false, true, false
	case <-time.After(ariaConfirmTimeout):
		a.handleConfirmTimeout(confirmID, emit)
		return confirmID, false, false, true, false
	}
}

// handleConfirmTimeout fires when awaitConfirm's deadline elapses with
// no decision: resolve as "not approved" and purge the entry. (The old
// chat panel had a retry/re-arm flow here; autonomous confirms are
// answered by the server-side countdown long before the 3-min ceiling,
// so a timeout now just means nothing could resolve it.)
func (a *Aria) handleConfirmTimeout(confirmID string, emit func(map[string]any)) {
	a.mu.Lock()
	_, ok := a.confirms[confirmID]
	if ok {
		delete(a.confirms, confirmID)
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	emit(map[string]any{"type": "confirm_resolved", "id": confirmID, "approved": false, "timeout": true})
}

func confirmRiskLevel(opts confirmOpts) string {
	if opts.Risk != "" {
		return opts.Risk
	}
	return "medium"
}
