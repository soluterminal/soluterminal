package main

import (
	"errors"
	"strings"
)

// Focus (the native renderer) was removed. A user who updates from an older
// release can still have a saved Focus pane: an entry in panes.json, or a pane
// definition in a saved workspace, with kind "native". This file is the only
// code that still understands that shape. It turns such a pane into a normal
// terminal pane that reopens the same Claude Code, Codex or Muse conversation,
// so an upgrade never drops the conversation and never crashes.

// errFocusRemoved answers a request for a NEW Focus pane (an old phone build or
// a stale client). Restores of saved Focus panes are converted instead.
var errFocusRemoved = errors.New("Focus view was removed: open a terminal pane instead")

// isRetiredNativePane reports whether a saved pane was a Focus pane: kind
// "native", or the older corruption where the kind was lost but the command is
// still the headless `claude -p … stream-json` argv only Focus ever used.
func isRetiredNativePane(kind string, cmd []string) bool {
	return strings.TrimSpace(kind) == "native" || isHeadlessClaudeCmd(cmd)
}

// retiredNativeAgent works out which CLI a saved Focus pane ran BEFORE its
// command is replaced: Focus panes ran `codex app-server`, the Muse driver or
// headless Claude, and older saves may omit the agent field. The command and
// the saved conversation ids decide; an explicit agent always wins.
func retiredNativeAgent(agent string, cmd []string, claudeSessionID, codexThreadID, museSessionID string) string {
	if a, ok := normalizeAgentKind(agent, cmd, codexThreadID); !ok || a != "" {
		if !ok {
			return agent // unknown agent: createPane rejects it later
		}
		return a
	}
	if strings.TrimSpace(agent) != "" || isHeadlessClaudeCmd(cmd) || strings.TrimSpace(claudeSessionID) != "" {
		return agentClaude
	}
	switch {
	case strings.TrimSpace(codexThreadID) != "" && strings.TrimSpace(museSessionID) == "":
		return agentCodex
	case strings.TrimSpace(museSessionID) != "" && strings.TrimSpace(codexThreadID) == "":
		return agentMuse
	case len(cmd) > 1 && cmd[1] == "muse-driver":
		return agentMuse
	}
	return agent
}

// terminalResumeCmd is the interactive command that reopens a conversation in a
// terminal pane, or starts the bare CLI when no conversation id was saved.
func terminalResumeCmd(agent, conversationID string) []string {
	id := strings.TrimSpace(conversationID)
	switch effectiveAgentKind(agent) {
	case agentCodex:
		if id != "" {
			return []string{"codex", "resume", id}
		}
		return []string{"codex"}
	case agentMuse:
		if id != "" {
			return []string{"muse", "resume", id}
		}
		return []string{"muse"}
	default:
		if id != "" {
			return []string{"claude", "--resume", id}
		}
		return []string{"claude"}
	}
}

// retireNativePersistedPane converts a saved Focus pane (panes.json) into a
// terminal pane before the boot restore decides how to bring it back. A Focus
// pane's live agent runs a headless stream-json process that nothing in this
// daemon can drive any more (no parser, no approval answers), so it is stopped
// (only when its pid still provably serves this pane's socket) and the pane
// goes down the normal resume path. The outgoing release's update gate refuses
// a non-forced swap while a Focus pane is mid-turn, so in practice the agent
// is idle here; a forced update or crash would have cut the turn anyway, and
// `--resume` reopens the conversation from its transcript. auto-resume reopens the
// conversation, otherwise the pane waits as a resumable tile. A pane whose
// conversation id was never saved becomes the usual "choose the original
// conversation" recovery tile. Returns ok=false, unchanged, for any other pane.
func retireNativePersistedPane(info PersistedPane) (PersistedPane, bool) {
	if !isRetiredNativePane(info.Kind, info.Cmd) {
		return info, false
	}
	info.Agent = retiredNativeAgent(info.Agent, info.Cmd, info.ClaudeSessionID, info.CodexThreadID, info.MuseSessionID)
	if effectiveAgentKind(info.Agent) == agentClaude && strings.TrimSpace(info.ClaudeSessionID) == "" {
		info.ClaudeSessionID = resumeSessionIDFromCmd(info.Cmd)
	}
	if info.AgentPid > 0 && agentPidOwnsSocket(info.AgentPid, info.SocketPath) {
		killAgentTree(info.AgentPid)
	}
	info.AgentPid = 0
	info.SocketPath = ""
	info.Kind = ""
	info.Cmd = terminalResumeCmd(info.Agent, conversationIDForAgent(info.Agent, info.ClaudeSessionID, info.CodexThreadID, info.MuseSessionID))
	return info, true
}

// retireNativeCreateRequest handles kind "native" on POST /api/panes. A restore
// of a saved Focus pane (a workspace switch or a recovery tile) is converted to
// a terminal restore of the same conversation; a request for a new Focus pane
// is refused with errFocusRemoved.
func retireNativeCreateRequest(req *createPaneReq) error {
	if req == nil {
		return nil
	}
	if !req.Restore {
		if strings.TrimSpace(req.Kind) == "native" {
			return errFocusRemoved
		}
		return nil
	}
	if !isRetiredNativePane(req.Kind, req.Cmd) {
		return nil
	}
	req.Agent = retiredNativeAgent(req.Agent, req.Cmd, req.ClaudeSessionID, req.CodexThreadID, req.MuseSessionID)
	if effectiveAgentKind(req.Agent) == agentClaude && strings.TrimSpace(req.ClaudeSessionID) == "" {
		req.ClaudeSessionID = resumeSessionIDFromCmd(req.Cmd)
	}
	req.Kind = ""
	req.Cmd = terminalResumeCmd(req.Agent, conversationIDForAgent(req.Agent, req.ClaudeSessionID, req.CodexThreadID, req.MuseSessionID))
	return nil
}
