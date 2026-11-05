package main

import (
	"errors"
	"net/http"
	"strings"
)

// pane_restart.go implements POST /api/panes/{id}/restart: restart THIS pane's
// agent in place, on the currently installed binary, resuming the same
// conversation when the pane has a durable one. It backs the per-pane "older
// build" dot (paneDTO.agentStale) and the pane menu's restart.
//
// ?fresh=1 restarts the pane's own command WITHOUT resuming — a brand-new
// conversation. That is the escape hatch for a pane whose transcript/rollout is
// gone or was never captured: instead of forcing the user to close and recreate
// the pane, the restart closes and reopens it under the same pane id.
//
// ?force=1 is the user's explicit override of the busy settle gate. Automatic
// paths never set it. It exists so a pane wedged "busy" by a
// silent/logged-out provider, or parked on an approval the user chooses to
// abandon, is not permanently un-restartable. The UI confirms before sending it.
//
// Safety: the shared settle gate (never
// interrupt an in-flight turn or parked approval unless the user forces it) and
// the fleetRespawnEligibility policy (a pane whose conversation is not
// durably resumable is refused with needsFresh, never silently resumed into a
// different session).

func (a *apiServer) handleRestartPane(w http.ResponseWriter, r *http.Request) {
	// Restarting runs the pane's command again: the person's, like opening one.
	if !a.requireHuman(w, r, "restart a pane") {
		return
	}
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	fresh := r.URL.Query().Get("fresh") == "1"
	force := r.URL.Query().Get("force") == "1"

	// Single-flight shared with resume + refresh + switch-mode, so a
	// double-click or a restart racing another respawn cannot double-kill.
	if !p.tryAcquireRespawn() {
		http.Error(w, "respawn already in progress", http.StatusConflict)
		return
	}
	defer p.releaseRespawn()

	// Settle gate: a restart is a kill, so never interrupt a live turn or a
	// parked approval (same policy as refresh; the blocked
	// panes this feature exists for are idle). force is the confirmed user
	// override.
	if restartPaneBusyBlocked(p, force) {
		http.Error(w, "agent is busy — finish the current turn (or answer the pending approval) first", http.StatusConflict)
		return
	}

	// Adopt a harness sidecar written since the last poll before the identity is
	// judged: a reused shell can carry a stale Claude id, and resuming that
	// would relaunch Claude over the live DeepSeek conversation.
	p.refreshHarnessSession()

	// Re-snapshot after the gate so a conversation id rotated in the meantime is
	// the one judged/resumed.
	snap := p.Snapshot()
	if !fresh {
		blocker := ""
		if snap.Status == PaneRunning {
			blocker = fleetRespawnEligibility(p).Blocker
		} else if paneSnapshotConversationID(snap) == "" {
			blocker = "no durable conversation to resume"
		}
		if blocker != "" {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      blocker,
				"needsFresh": true,
			})
			return
		}
	}

	newPane, err := a.respawnInPlace(snap, restartSpawnOpts(snap, fresh))
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

// restartSpawnOpts builds the respawn opts for a per-pane restart. The resume
// case reuses resumeSpawnOpts; the fresh case clears every
// durable pointer and replays the pane's own command so Spawn cannot resume the
// old conversation.
func restartSpawnOpts(snap PaneSnapshot, fresh bool) SpawnOpts {
	opts := resumeSpawnOpts(snap, 0, 0)
	if !fresh {
		return opts
	}
	opts.ClaudeSessionID = ""
	opts.ClaudeTranscriptPath = ""
	opts.CodexThreadID = ""
	opts.MuseSessionID = ""
	opts.HarnessSessionID = ""
	opts.HarnessProvider = ""
	opts.CodexTurnInFlight = false
	opts.Env = nil
	if snap.Kind != "native" {
		cmd := append([]string(nil), snap.Cmd...)
		if len(cmd) == 0 {
			cmd = []string{defaultShell()}
		}
		opts.Cmd = cmd
	} else {
		// Native Spawn rebuilds a fresh headless argv from Agent when Cmd is nil
		// and no session id is set.
		opts.Cmd = nil
	}
	return opts
}

// restartNeedsFresh reports whether the pane has no durable conversation the
// daemon could resume, i.e. a plain restart would be refused with needsFresh.
// Exposed for tests; the handler mirrors it inline because it also distinguishes
// a not-running pane.
func restartNeedsFresh(p *Pane) bool {
	if p == nil {
		return true
	}
	snap := p.Snapshot()
	if snap.Status != PaneRunning {
		return paneSnapshotConversationID(snap) == ""
	}
	return strings.TrimSpace(fleetRespawnEligibility(p).Blocker) != ""
}

// restartPaneBusyBlocked is the per-pane settle gate. force is the user's
// confirmed override: the automatic paths never pass it, so a silent mid-turn
// provider is never killed without an explicit choice.
func restartPaneBusyBlocked(p *Pane, force bool) bool {
	return !force && fleetPaneBusyForRestart(p)
}
