package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
)

// pane_role_set.go implements POST /api/panes/{id}/role: set or remove a
// running pane's product role. The orchestrator brief is part of the selected
// host's durable instruction transport, so changing the role necessarily
// respawns the pane in place. Existing Claude and Codex conversations resume;
// a session-less Claude or shell pane starts a fresh default Claude session.

// roleChangeEnabled is the kill-switch for the role endpoint: DEFAULT-ON, set
// SOLU_PANE_ROLE to "0"/"false"/"off"/"no" to disable globally. It mirrors
// effortChangeEnabled because both controls respawn a live pane.
func roleChangeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_PANE_ROLE"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

type setRoleReq struct {
	Role string `json:"role"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (a *apiServer) handleSetRole(w http.ResponseWriter, r *http.Request) {
	if !roleChangeEnabled() {
		http.Error(w, "role change is disabled via SOLU_PANE_ROLE", http.StatusBadRequest)
		return
	}

	var req setRoleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	role := strings.TrimSpace(req.Role)
	if !validPaneRole(role) {
		http.Error(w, "unknown role: "+role, http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	snap := p.Snapshot()
	healedSnap, identityNotes := healPaneSnapshot(snap)
	for _, note := range identityNotes {
		log.Printf("set role %s: identity heal: %s", id, note)
	}
	if snap.Role == paneRoleTaskOwner && role != paneRoleTaskOwner {
		http.Error(w, "This pane is bound to a task owner; open a separate orchestrator for other tasks.", http.StatusConflict)
		return
	}
	agent := healedSnap.Agent
	if strings.TrimSpace(agent) != "" && !orchestratorHostSupported(agent) {
		http.Error(w, "orchestrator role is not available for "+orchestratorHostLabel(agent)+": "+orchestratorHostUnsupportedReason(agent), http.StatusConflict)
		return
	}
	host, _ := orchestratorHostFor(agent)
	if strings.TrimSpace(snap.Role) == role {
		writeJSON(w, http.StatusOK, makePaneDTO(p, a.aria, a.attention))
		return
	}
	if role == paneRoleOrchestrator && effectiveAgentKind(agent) == agentCodex {
		if avail := a.orchestratorHostAvailability(agent); !avail.Available {
			http.Error(w, "orchestrator role unavailable for Codex: "+avail.Reason, http.StatusConflict)
			return
		}
	}
	if snap.Status == PaneExited {
		http.Error(w, "pane has exited — use resume", http.StatusConflict)
		return
	}

	if !p.tryAcquireRespawn() {
		http.Error(w, "respawn already in progress", http.StatusConflict)
		return
	}
	defer p.releaseRespawn()

	if p.IsAgentWorking() {
		http.Error(w, "agent is busy — finish the current turn (or answer the pending approval) first", http.StatusConflict)
		return
	}

	// Re-snapshot after the single-flight and settle gates so a just-rotated
	// provider conversation id is the one resumed.
	snap = p.Snapshot()
	snap, _ = healPaneSnapshot(snap)
	opts := resumeSpawnOpts(snap, req.Cols, req.Rows)
	opts, _ = healSpawnOpts(opts)
	opts.Role = role
	// A managed terminal harness resumes through its own launch command
	// (resumeSpawnOpts already built it), and must not be gated on — or
	// hijacked by — a stale Claude/Codex id. Skip the provider continuity
	// checks entirely for it.
	if strings.TrimSpace(snap.HarnessProvider) == "" {
		conversationID := conversationIDForAgent(opts.Agent, opts.ClaudeSessionID, opts.CodexThreadID, opts.MuseSessionID)
		if conversationID != "" {
			switch effectiveAgentKind(opts.Agent) {
			case agentCodex:
				if !codexRolloutOnDisk(conversationID) {
					http.Error(w, "no on-disk Codex rollout to resume — cannot change role yet", http.StatusConflict)
					return
				}
			default:
				if !claudeTranscriptResumable(snap.Cwd, conversationID, strings.TrimSpace(opts.ClaudeTranscriptPath)) {
					http.Error(w, "no on-disk transcript to resume — cannot change role yet", http.StatusConflict)
					return
				}
			}
		} else {
			// There is no conversation to preserve. Drop every stale continuity hint
			// and start the selected host fresh; native Spawn builds its own argv.
			opts.ClaudeSessionID = ""
			opts.ClaudeTranscriptPath = ""
			opts.CodexThreadID = ""
			opts.MuseSessionID = ""
			opts.CodexTurnInFlight = false
			opts.Cmd = append([]string(nil), host.bareCmd...)
		}
	}

	newPane, err := a.respawnInPlace(snap, opts)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNativeSessionDied) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, makePaneDTO(newPane, a.aria, a.attention))
}
