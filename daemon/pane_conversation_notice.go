package main

import "time"

// switchNotice is the transient "something discontinuous happened to this
// conversation" marker for a pane. It rides the pane DTO (switchNotice) so the
// client can show the right banner, and is cleared on the next user turn (or an
// explicit ack). Reason discriminates the copy:
//
//	"resume-repair" — a resume found (and synthetically answered) a dangling
//	                  tool_use: the previous response was cut off by a daemon
//	                  restart / crash.
//	"fresh-start"   — a requested `--resume` was DROPPED (no on-disk transcript
//	                  yet, or the session is live in another pane) and the pane
//	                  started a brand-new conversation. Interrupted=false: there
//	                  is nothing to continue, the banner is informational.
type switchNotice struct {
	Interrupted bool   `json:"interrupted"`
	FromKind    string `json:"fromKind"`         // always "terminal" since Focus was removed
	At          int64  `json:"at"`               // unix millis when the interrupt happened
	Reason      string `json:"reason,omitempty"` // "resume-repair" | "fresh-start"
}

// setSwitchNoticeReason records a conversation discontinuity (see switchNotice.Reason).
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

// clearSwitchNotice drops any outstanding notice — called when the next user
// turn is written (the "Continue" was effectively taken) or on explicit ack.
func (p *Pane) clearSwitchNotice() {
	p.switchNoticeMu.Lock()
	p.pendingSwitchNotice = nil
	p.switchNoticeMu.Unlock()
}

// switchNoticeSnapshot returns a copy of the pending notice (or nil) for the
// DTO. Copied under the lock so the DTO marshal never races a concurrent
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

// IsAgentWorking reports whether the pane's agent is mid-turn: computeState in
// {StateBusy, StateRecent} means recent PTY output, i.e. the TUI agent is
// generating. A respawn that sees true must wait for the turn to settle.
func (p *Pane) IsAgentWorking() bool {
	return p.agentMidTurnForState(p.computeState())
}

// agentMidTurnForState answers "would a respawn NOW interrupt live agent work?"
// given an ALREADY-COMPUTED runtime state, so the per-pane DTO path can reuse
// the state it already derived. StateBusy/StateRecent mean recent PTY output,
// but a visible prompt box means the turn was handed back (the redraws are the
// idle prompt, not generation), so it is NOT mid-turn. Every other state
// (choice-prompt, awaiting-input, idle, exited) is owned by the user.
func (p *Pane) agentMidTurnForState(state PaneRuntimeState) bool {
	switch state {
	case StateBusy, StateRecent:
		return !p.claudePromptVisible()
	default:
		return false
	}
}
