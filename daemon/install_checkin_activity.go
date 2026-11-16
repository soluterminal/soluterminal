package main

import (
	"strings"
	"time"
)

// The check-in's two activation flags. Each is a yes/no for "in the last
// 24 hours"; no counts, pane names, commands or content are kept or sent.
//
//   - agentPaneDay: a Claude Code or Codex pane was started in SoluTerminal
//     (spawnPaneForRequest: the UI, an agent's spawn_pane, a reopen, resume or
//     switch). The daemon's own boot recovery of existing panes is not a start.
//   - agentsTogetherDay: one pane's agent used SoluTerminal's tools on another
//     pane, and the call succeeded (noteCrossPaneTool).
//
// Nothing is recorded while check-ins are off, so turning them off also stops
// this bookkeeping.

const (
	checkinActivityWindow = 24 * time.Hour
	// Persisting at most hourly keeps checkin.json writes off the hot path;
	// the window is then exact to within an hour.
	checkinActivityPersistEvery = time.Hour
)

// noteActivity moves one timestamp forward and persists it, unless check-ins
// are off or it was already noted within the last hour.
func (c *installCheckin) noteActivity(field func(*checkinSettings) *int64) {
	if c == nil || !c.enabled() {
		return
	}
	now := c.now().Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	at := field(&c.settings)
	if *at != 0 && now-*at < int64(checkinActivityPersistEvery/time.Second) {
		return
	}
	*at = now
	_ = c.persistLocked() // best effort: the in-memory value still counts
}

func (c *installCheckin) noteAgentPane() {
	c.noteActivity(func(s *checkinSettings) *int64 { return &s.AgentPaneAt })
}

func (c *installCheckin) noteAgentsTogether() {
	c.noteActivity(func(s *checkinSettings) *int64 { return &s.AgentsTogetherAt })
}

func (c *installCheckin) activityFlags() (agentPane, together bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	recent := func(at int64) bool {
		if at == 0 {
			return false
		}
		age := now.Sub(time.Unix(at, 0))
		return age >= 0 && age < checkinActivityWindow
	}
	return recent(c.settings.AgentPaneAt), recent(c.settings.AgentsTogetherAt)
}

// checkinAgentPane reports whether a freshly spawned pane runs Claude Code or
// Codex. A plain shell has no agent, so a pane is judged by its command.
func checkinAgentPane(snap PaneSnapshot) bool {
	if strings.TrimSpace(snap.Agent) == agentCodex {
		return true
	}
	switch agentFromCmd(snap.Cmd) {
	case agentClaude, agentCodex:
		return true
	}
	return false
}

// crossPaneTarget names the other pane an MCP call acts on, or "" when the
// call is not one pane's agent working with another: no caller pane, the
// caller acting on itself, or a tool that doesn't drive another pane. A
// spawn_pane that hands the new pane a prompt counts; its target is the
// pane it creates.
func (a *apiServer) crossPaneTarget(tool string, args map[string]any, callerPane string) string {
	if a == nil || a.checkin == nil || a.mgr == nil || callerPane == "" {
		return ""
	}
	if _, ok := a.mgr.Get(callerPane); !ok {
		return ""
	}
	switch tool {
	case "ask_pane", "send_input", "answer_prompt", "read_pane_tail", "wait_for_pane":
		ref, _ := args["pane_id"].(string)
		target := a.resolvePaneRef(ref, callerPane)
		if target == "" || target == callerPane {
			return ""
		}
		return target
	case "spawn_pane":
		if prompt, _ := args["prompt"].(string); strings.TrimSpace(prompt) != "" {
			return "(spawned)"
		}
	}
	return ""
}

// noteCrossPaneTool records agentsTogetherDay when a cross-pane call (see
// crossPaneTarget) succeeded, and passes its result through unchanged.
func (a *apiServer) noteCrossPaneTool(target string, result any) any {
	if target == "" {
		return result
	}
	m, ok := result.(map[string]any)
	if ok && m["isError"] == true {
		return result
	}
	// spawn_pane succeeds even when its prompt could not be delivered.
	if target == "(spawned)" && ok && strings.Contains(fmtToolText(m), `"prompt_sent":false`) {
		return result
	}
	a.checkin.noteAgentsTogether()
	return result
}

func fmtToolText(m map[string]any) string {
	var b strings.Builder
	content, _ := m["content"].([]any)
	for _, part := range content {
		if p, ok := part.(map[string]any); ok {
			if text, ok := p["text"].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}
