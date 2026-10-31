package main

import (
	"net/http"
	"os"
	"path/filepath"
)

// acquirePaneSettings serializes settings with replacement of the pane object.
// A success response must refer to the current pane, never a retired instance.
func (a *apiServer) acquirePaneSettings(w http.ResponseWriter, p *Pane) bool {
	if !p.tryAcquireRespawn() {
		http.Error(w, "pane recovery or settings change already in progress", http.StatusConflict)
		return false
	}
	current, ok := a.mgr.Get(p.ID)
	if !ok || current != p {
		p.releaseRespawn()
		http.Error(w, "pane changed; retry settings on the current pane", http.StatusConflict)
		return false
	}
	return true
}

// setAutomaticAnswerSettings revokes in-flight decisions before returning.
// The settings/respawn lease is held by callers changing a published pane.
func (p *Pane) setAutomaticAnswerSettings(mode string, locked bool) {
	p.autoModeMu.Lock()
	defer p.autoModeMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if locked {
		mode = ""
	}
	if p.AutoMode != mode || p.NeverAutoAnswer != locked {
		p.autoModeGeneration++
	}
	p.AutoMode, p.NeverAutoAnswer = mode, locked
	if locked {
		p.autoPromptExpectUntilMs.Store(0)
	}
}

// This is deliberately requireHuman, even when delegated agent authority is on.
// Unlocking leaves auto mode Off; enabling it remains a separate explicit action.
func (a *apiServer) setPaneNeverAutoAnswer(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "change pane automatic-answer lock") {
		return
	}
	var req struct {
		NeverAutoAnswer *bool `json:"neverAutoAnswer"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if req.NeverAutoAnswer == nil {
		http.Error(w, "neverAutoAnswer is required", http.StatusBadRequest)
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	if !a.acquirePaneSettings(w, p) {
		return
	}
	defer p.releaseRespawn()
	prev := p.Snapshot()
	mode := ""
	if !*req.NeverAutoAnswer && !prev.NeverAutoAnswer {
		mode = prev.AutoMode // repeating an already-clear lock is a no-op
	}
	p.setAutomaticAnswerSettings(mode, *req.NeverAutoAnswer)
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.setAutomaticAnswerSettings(prev.AutoMode, prev.NeverAutoAnswer)
			http.Error(w, "persist pane: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if a.aria != nil && (prev.AutoMode != mode || prev.NeverAutoAnswer != *req.NeverAutoAnswer) {
		a.aria.resetAutoModeEvaluation(p)
	}
	if a.attention != nil {
		a.attention.Refresh(p)
	}
	writeJSON(w, http.StatusOK, makePaneDTO(p, a.aria, a.attention))
}

const paneNoAutoAnswerMarker = ".solu-no-autoanswer"

func tightenPaneAnswerSettings(cwd, mode string, locked bool) (string, bool) {
	if !locked && cwd != "" {
		_, err := os.Lstat(filepath.Join(expandHome(cwd), paneNoAutoAnswerMarker))
		locked = err == nil
	}
	if locked {
		return "", true
	}
	return mode, false
}
