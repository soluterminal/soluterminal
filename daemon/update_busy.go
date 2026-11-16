package main

import "time"

// updateBusy protects daemon-local state, not terminal turns: their PTYs live
// in the agent process and survive the swap. Locks match the delivery drain.
func (a *apiServer) updateBusy() *updateBusyError {
	var panes []*Pane
	if a.mgr != nil {
		panes = a.mgr.List()
	}
	// Held messages live only in memory. While the person's pause keeps them
	// (they never expire then), no unattended update may drop them: this
	// cause is not subject to the collaboration grace.
	if agentMessagingPaused() {
		for _, p := range panes {
			p.mu.Lock()
			held := p.Status == PaneRunning && len(p.heldDeliveries) > 0
			p.mu.Unlock()
			if held {
				return &updateBusyError{cause: updateBusyCauseMessagingPaused, reason: "agent messaging is paused and messages are held on pane " + p.ID + "; resume, or send or discard them, to update", paneID: p.ID}
			}
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
				if h.resolution == "" && h.connection == p.agentConn && now.Sub(heldClockStart(h.At)) < heldDeliveryLimit {
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
