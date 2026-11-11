package main

import "time"

// updateBusy protects daemon-local state, not terminal turns: their PTYs live
// in the agent process and survive the swap. Locks match the delivery drain.
func (a *apiServer) updateBusy() *updateBusyError {
	var panes []*Pane
	if a.mgr != nil {
		panes = a.mgr.List()
	}
	// TEMPORARY FOCUS COMPATIBILITY: the Focus removal deletes this block.
	// Focus still exists on main; its in-process turns must finish, even if
	// the collaboration grace is exhausted. Give this priority over asks.
	for _, p := range panes {
		p.mu.Lock()
		native := p.Kind == "native"
		p.mu.Unlock()
		if native && p.IsAgentWorking() {
			return &updateBusyError{cause: updateBusyCauseFocus, reason: "a Focus pane is mid-turn (pane " + p.ID + ")", paneID: p.ID}
		}
	}
	if a.collabInflight.Load() > 0 {
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "an ask_pane or wait_for_pane call is in flight"}
	}
	now := time.Now()
	for _, p := range panes {
		p.mu.Lock()
		putBack := p.putBackReceiptLocked() != nil
		held := false
		if p.Status == PaneRunning && p.agentConn != nil && !p.respawning.Load() {
			for _, h := range p.heldDeliveries {
				if h.resolution == "" && h.connection == p.agentConn && now.Sub(time.UnixMilli(h.At)) < heldDeliveryLimit {
					held = true
					break
				}
			}
		}
		p.mu.Unlock()
		if putBack {
			return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "a pane has a put-back receipt (pane " + p.ID + ")", paneID: p.ID}
		}
		if held {
			return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "a pane has a held delivery (pane " + p.ID + ")", paneID: p.ID}
		}
	}
	return nil
}
