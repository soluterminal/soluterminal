package main

import (
	"path/filepath"
	"strings"
)

func plainShellRecovery(info PersistedPane) bool {
	if info.Agent != "" || info.IdentityBlocked || info.HarnessSessionID != "" {
		return false
	}
	if len(info.Cmd) == 0 {
		return true
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(info.Cmd[0])), ".exe")
	switch name {
	case "bash", "sh", "zsh", "fish", "dash", "pwsh", "powershell", "cmd":
	default:
		return false
	}
	for _, arg := range info.Cmd[1:] {
		switch arg {
		case "-i", "-l", "-il", "-li", "--login":
		default:
			return false
		}
	}
	return true
}

// A shell command describes the process we launched, not necessarily the
// conversation the user subsequently opened inside it. Persist a resume recipe
// only when that pane's own conversation identity was verified.
func paneRecoveryCommand(snap PaneSnapshot) []string {
	if snap.IdentityBlocked {
		return cloneStringSlice(snap.Cmd)
	}
	switch {
	case snap.Agent == agentCodex && strings.TrimSpace(snap.CodexThreadID) != "" && !cmdRunsCodex(snap.Cmd):
		return []string{"codex", "resume", snap.CodexThreadID}
	case snap.Agent == agentMuse && strings.TrimSpace(snap.MuseSessionID) != "" && !cmdStartsMuse(snap.Cmd):
		return []string{"muse", "resume", snap.MuseSessionID}
	case effectiveAgentKind(snap.Agent) == agentClaude && strings.TrimSpace(snap.ClaudeSessionID) != "" && !cmdRunsClaude(snap.Cmd):
		return []string{"claude", "--resume", snap.ClaudeSessionID}
	default:
		return cloneStringSlice(snap.Cmd)
	}
}

func missingPaneIdentityMessage(info PersistedPane) string {
	if info.IdentityBlocked {
		return "The original conversation could not be verified. Choose the original conversation to reopen this pane."
	}
	if info.Agent == agentCodex || info.Agent == agentMuse || cmdRunsClaude(info.Cmd) || info.Agent == agentClaude {
		return "The conversation ID was not saved. Choose the original conversation to reopen this pane."
	}
	return "The previous shell ended during shutdown. Reopen the shell to continue; its process state cannot be restored."
}

// Restore requests are stricter than a deliberate fresh launch: they must not
// quietly replace the user's conversation when the saved identity is absent.
func preparePaneRestore(req *createPaneReq) error {
	if req.IdentityBlocked {
		return &SpawnError{msg: "saved conversation identity is unverified; choose the original conversation"}
	}
	agent, valid := normalizeAgentKind(req.Agent, req.Cmd, req.CodexThreadID)
	if !valid {
		return &SpawnError{msg: "unknown saved agent: " + req.Agent}
	}
	if req.CodexThreadID == "" {
		req.CodexThreadID = resumeThreadIDFromCodexCmd(req.Cmd)
	}
	if req.ClaudeSessionID == "" {
		req.ClaudeSessionID = resumeSessionIDFromCmd(req.Cmd)
	}
	if req.MuseSessionID == "" {
		req.MuseSessionID = resumeSessionIDFromMuseCmd(req.Cmd)
	}
	id := conversationIDForAgent(agent, req.ClaudeSessionID, req.CodexThreadID, req.MuseSessionID)
	if id == "" && (agent == agentCodex || agent == agentMuse || cmdRunsClaude(req.Cmd) || req.Agent == agentClaude) {
		return &SpawnError{msg: "saved conversation ID is missing; choose the original conversation"}
	}
	if id != "" {
		switch effectiveAgentKind(agent) {
		case agentCodex:
			req.Cmd = []string{"codex", "resume", id}
		case agentMuse:
			req.Cmd = []string{"muse", "resume", id}
		default:
			req.Cmd = []string{"claude", "--resume", id}
		}
	}
	return nil
}
