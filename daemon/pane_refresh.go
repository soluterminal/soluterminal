package main

import (
	"errors"
	"net/http"
	"os"
	"strings"
)

// pane_refresh.go implements POST /api/panes/{id}/refresh: a "restart the agent,
// keep the conversation" action for NATIVE (Focus) panes. It kills the pane's
// long-lived provider process and respawns it under the SAME id, resuming the
// exact Claude session or Codex thread. The new process releases the old heap
// while preserving the conversation.
//
// It deliberately reuses the proven resume recipe (respawnInPlace) rather than
// performSwitchMode's core, because performSwitchMode no-ops on a same-Kind
// respawn (snap.Kind==target) and never restores AutoMode/Flag/prompts. refresh
// is native-only, settle-gated (never interrupts a turn or a parked approval just
// to reclaim RAM), and shares the pane's respawn single-flight with resume +
// switch-mode so concurrent respawns can't bind two agents to one socket.

// refreshEnabled is the refresh feature's kill-switch: DEFAULT-ON, set
// SOLU_PANE_REFRESH to "0"/"false"/"off"/"no" to disable the endpoint globally
// (mirrors nativeRendererEnabled — a safety valve if a field issue surfaces).
func refreshEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_PANE_REFRESH"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func (a *apiServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if !refreshEnabled() {
		http.Error(w, "pane refresh is disabled via SOLU_PANE_REFRESH", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	snap := p.Snapshot()

	// Native-only. A terminal pane already has Ctrl+C + `exec bash -i`; refresh
	// only makes sense for the headless native agent. Rejecting terminal panes
	// here also keeps every
	// terminal-respawn failure mode (lost hook-captured session id, the
	// `; exec bash -i` masquerade) out of the refresh blast radius.
	if snap.Kind != "native" {
		http.Error(w, "refresh is only available for native (Focus) panes", http.StatusBadRequest)
		return
	}

	// An exited-but-resumable pane is the resume path's job (it reaps the orphan
	// agent and re-arms the badge); don't duplicate that here.
	if snap.Status == PaneExited {
		http.Error(w, "pane has exited — use resume", http.StatusConflict)
		return
	}

	// Single-flight shared with resume + switch-mode. A double-click, phone+desktop
	// double-fire, or a refresh racing a switch must not double-kill the pane.
	if !p.tryAcquireRespawn() {
		http.Error(w, "respawn already in progress", http.StatusConflict)
		return
	}
	defer p.releaseRespawn()

	// Settle gate: never interrupt an in-flight turn or a parked tool approval
	// just to reclaim RAM. Refuse (don't defer) — simplest + safest for Phase 1.
	// Because we refuse while working, the snapshot we read below is post-settle,
	// so the provider's conversation id is the real, rotation-final id.
	if p.IsAgentWorking() || p.hasNativePending() {
		http.Error(w, "agent is busy — finish the current turn (or answer the pending approval) first", http.StatusConflict)
		return
	}

	// Re-snapshot AFTER acquiring the lock + confirming settle: the provider's
	// conversation id may have rotated since the first read.
	snap = p.Snapshot()
	agent := effectiveAgentKind(snap.Agent)
	conversationID := conversationIDForAgent(snap.Agent, snap.ClaudeSessionID, snap.CodexThreadID, snap.MuseSessionID)
	if conversationID == "" {
		// Brand-new pane mid-first-turn (or a pane that never started a session):
		// nothing to reclaim or resume, and respawning fresh would silently start
		// a DIFFERENT session and lose the in-flight turn. Mirrors resume's 400.
		http.Error(w, "no "+agent+" conversation yet — nothing to refresh", http.StatusConflict)
		return
	}
	// Claude's CLI may silently fall through to a fresh session if its transcript
	// disappeared, so retain the strict on-disk guard. Codex app-server validates
	// the durable thread during thread/resume and reports a failure instead.
	if agent == agentClaude && !claudeTranscriptResumable(snap.Cwd, conversationID, strings.TrimSpace(snap.ClaudeTranscriptPath)) {
		http.Error(w, "no on-disk transcript to resume — nothing to refresh", http.StatusConflict)
		return
	}

	// Recreate the pane under the SAME native kind, resuming the same conversation.
	// Cmd stays nil: Spawn selects headless Claude resume or Codex app-server
	// thread/resume from Agent + the provider-specific durable id.
	opts := SpawnOpts{
		AutoMode:             snap.AutoMode,
		NeverAutoAnswer:      snap.NeverAutoAnswer,
		ID:                   snap.ID,
		Cwd:                  snap.Cwd,
		Title:                snap.Title,
		Cols:                 snap.Cols,
		Rows:                 snap.Rows,
		ClaudeSessionID:      snap.ClaudeSessionID,
		ClaudeTranscriptPath: strings.TrimSpace(snap.ClaudeTranscriptPath),
		TitleOverride:        snap.TitleOverride,
		Origin:               snap.Origin.clone(),
		Review:               snap.Review.clone(),
		Identity:             snap.Identity.clone(),
		Kind:                 "native",
		Agent:                snap.Agent,
		CodexThreadID:        snap.CodexThreadID,
		Provider:             snap.Provider,
		Effort:               snap.Effort,
		Model:                snap.Model,
		Role:                 snap.Role,
		ContextPack:          snap.ContextPack,
		NativeAllowlist:      snap.NativeAllowlist,
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
