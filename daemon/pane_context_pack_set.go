package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

// contextPackChangeEnabled is the kill switch for the respawning setter.
func contextPackChangeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_PANE_CONTEXT_PACK"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

type setContextPackReq struct {
	ContextPack string `json:"contextPack"`
	Cols        uint16 `json:"cols"`
	Rows        uint16 `json:"rows"`
}

// handleSetContextPack attaches or detaches a pack by respawning the selected
// agent in place. Existing provider conversations are preserved exactly like a
// role/effort respawn; session-less Claude starts fresh.
func (a *apiServer) handleSetContextPack(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "change a pane context pack") {
		return
	}
	if !contextPackChangeEnabled() {
		http.Error(w, "context pack change is disabled via SOLU_PANE_CONTEXT_PACK", http.StatusBadRequest)
		return
	}

	var req setContextPackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := validContextPackDir(req.ContextPack)
	if err != nil {
		http.Error(w, "invalid context pack: "+err.Error(), http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	snap := p.Snapshot()
	if effectiveAgentKind(snap.Agent) == agentMuse {
		http.Error(w, "context packs are not available for Muse panes", http.StatusConflict)
		return
	}
	if snap.Kind != "native" && !cmdRunsClaude(snap.Cmd) && !cmdStartsCodex(snap.Cmd) {
		http.Error(w, "context packs are not available for this command", http.StatusConflict)
		return
	}
	if strings.TrimSpace(snap.ContextPack) == abs {
		writeJSON(w, http.StatusOK, makePaneDTO(p, a.aria, a.attention))
		return
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

	if p.IsAgentWorking() || p.hasNativePending() {
		http.Error(w, "agent is busy — finish the current turn (or answer the pending approval) first", http.StatusConflict)
		return
	}

	snap = p.Snapshot()
	opts := resumeSpawnOpts(snap, req.Cols, req.Rows)
	opts.ContextPack = abs
	if effectiveAgentKind(snap.Agent) == agentClaude {
		sessionID := strings.TrimSpace(snap.ClaudeSessionID)
		if sessionID != "" {
			if !claudeTranscriptResumable(snap.Cwd, sessionID, strings.TrimSpace(snap.ClaudeTranscriptPath)) {
				http.Error(w, "no on-disk transcript to resume — cannot change context pack yet", http.StatusConflict)
				return
			}
		} else {
			opts.ClaudeSessionID = ""
			opts.ClaudeTranscriptPath = ""
			opts.CodexTurnInFlight = false
			if snap.Kind == "native" {
				opts.Cmd = nil
			} else {
				opts.Cmd = []string{"claude"}
			}
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
