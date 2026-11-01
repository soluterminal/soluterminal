package main

import "time"

// agentMessageReceipt is the last message another agent pane delivered to
// this pane, so the person sees a pane is being talked to by an agent
// (ownership is unchanged: a message never makes a pane someone's worker).
// In memory only: it describes recent activity, not who the pane belongs to.
type agentMessageReceipt struct {
	FromPaneID string `json:"fromPaneId"`
	FromLabel  string `json:"fromLabel"` // "#3 Codex", as the "[from …]" prefix names it
	At         int64  `json:"at"`
	// Outcome: submitted | queued | sent_unconfirmed (left_in_composer is shown as
	// the unsent-message chip instead).
	Outcome string `json:"outcome"`
}

func (p *Pane) recordAgentMessage(fromPaneID, fromLabel string, outcome deliveryOutcome, now time.Time) {
	if fromLabel == "" || outcome == deliveryLeftInComposer {
		return
	}
	p.mu.Lock()
	p.lastAgentMessage = &agentMessageReceipt{FromPaneID: fromPaneID, FromLabel: fromLabel, At: now.UnixMilli(), Outcome: string(outcome)}
	p.mu.Unlock()
}

func (r *agentMessageReceipt) clone() *agentMessageReceipt {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
