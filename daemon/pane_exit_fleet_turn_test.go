package main

import (
	"sync/atomic"
	"testing"
)

// A pane that dies mid-turn must not stay "busy" for restart safety: exiting
// clears the fleet-turn latch, so a non-forced restart is not refused.
func TestMarkExitedClearsFleetTurnAdmission(t *testing.T) {
	t.Parallel()
	for _, agent := range []string{agentClaude, agentCodex, agentMuse} {
		p := &Pane{Agent: agent, done: make(chan struct{})}
		p.setFleetTurnAdmission(true)
		atomic.StoreInt64(&p.TotalStrippedChars, 0)
		p.fleetTurnOutputWatermark.Store(0)
		if !p.fleetTurnAdmitted.Load() {
			t.Fatalf("%s: fixture did not admit a turn", agent)
		}
		p.markExited(nil)
		if p.fleetTurnAdmitted.Load() {
			t.Fatalf("%s: exited pane still holds the fleet-turn latch", agent)
		}
		if restartPaneBusyBlocked(p, false) {
			t.Fatalf("%s: restart of an exited pane refused as busy", agent)
		}
	}
}
