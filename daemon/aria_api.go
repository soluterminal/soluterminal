package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ariaQueryReq is the internal request shape for one autonomous
// evaluation turn (built by aria_auto.go / aria_native_auto.go and fed
// to runTurn — it no longer arrives over HTTP; the interactive chat
// endpoint was removed with the ARIA panel).
type ariaQueryReq struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	// Model override; the cloud force-routes regardless (see cloud/aria.go).
	Model string `json:"model,omitempty"`
	// Autonomous is true for daemon-initiated standing-order / cautious
	// auto-mode turns (always true today; kept for the safety guards
	// that key off it).
	Autonomous bool `json:"-"`
	// AutonomousPaneID is the only pane an autonomous turn may affect.
	// Browser callers cannot set it; aria_auto.go stamps it for
	// server-side tool enforcement.
	AutonomousPaneID string `json:"-"`
	// AutonomousPromptPos is the LastAckedChoicePromptPos observed when
	// the watcher fired. Autonomous writes must still be targeting that
	// same prompt immediately before bytes hit the PTY.
	AutonomousPromptPos int64 `json:"-"`
	// AutonomousPromptHash fingerprints the visible choice prompt text
	// observed by the watcher. It complements AutonomousPromptPos for
	// grid-backed prompts where the position token is synthetic.
	AutonomousPromptHash uint64 `json:"-"`
	// AutonomousCautious means this autonomous turn is the built-in
	// "approve with caution" mode rather than a user-authored standing
	// order. Mutating tools get an extra deterministic deny gate before
	// any auto-confirm can be armed.
	AutonomousCautious bool `json:"-"`
	// AutonomousNative marks an autonomous turn that targets a native-renderer
	// pane. The turn is offered the structured resolve_permission tool instead
	// of the byte-scraping send_input tool (native approvals are answered with
	// a control_response frame, not PTY keystrokes). Browser callers cannot set
	// it; aria_auto.go stamps it for native panes.
	AutonomousNative bool `json:"-"`
	// AutonomousRequestID is the EXACT native approval RequestID this turn is
	// evaluating. The daemon binds resolve_permission to this id server-side so
	// the model never picks which parallel approval to resolve (the wrong-tool
	// hazard — approve a safe Read, accidentally allow a dangerous Bash). Empty
	// for terminal turns.
	AutonomousRequestID string `json:"-"`
}

type ariaConfirmReq struct {
	ID      string `json:"id"`
	Approve bool   `json:"approve"`
	// ApproveRest is set by the ConfirmCard's "approve rest of turn"
	// checkbox. Honoured only when Approve=true. Flips a per-turn atomic
	// so subsequent mutating tools in the same turn skip their dialogs
	// and execute immediately. Resets when the turn ends (loop exits).
	ApproveRest bool `json:"approve_rest,omitempty"`
}

type ariaPauseReq struct {
	Paused bool `json:"paused"`
}

type ariaAutoPendingConfirm struct {
	ID              string `json:"id"`
	PaneID          string `json:"pane_id"`
	PaneTitle       string `json:"pane_title"`
	Input           string `json:"input"`
	OrderID         string `json:"order_id"`
	AutoCountdownMs int    `json:"auto_countdown_ms"`
	DeadlineAtMs    int64  `json:"deadline_at_ms"`
}

// handleAriaPause toggles the autoresponder kill switch. Pause persists
// across restarts via aria_pause.json; on the next observation after a
// restart the panel surfaces a Resume?/Stay paused banner so the user
// makes the call rather than ARIA silently resuming.
func (a *apiServer) handleAriaPause(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	var req ariaPauseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !req.Paused && !a.requireHumanOrDelegated(w, r, "resume Aria") {
		return
	}
	a.aria.SetPaused(req.Paused)
	a.handleAriaStatus(w, r)
}

// handleAriaResetCap is the user's escape hatch when the rolling-hour
// auto-fire budget hits 100/100 and they want to keep going. Clears
// the fire log + sweeper-backoff streaks; the rolling-hour bound
// re-applies to fires from this moment forward.
func (a *apiServer) handleAriaResetCap(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	a.aria.ResetAutoFireCap()
	a.handleAriaStatus(w, r)
}

func (a *apiServer) handleAriaAutoPending(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"pending": a.aria.listAutoPendingConfirms()})
}

// handleAriaStatus reports autoresponder state: paused flag plus how
// many autonomous fires landed in the last hour. The footer
// pill polls this so the pause toggle renders in the right position
// without waiting for the next SSE tick.
func (a *apiServer) handleAriaStatus(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.aria.autoStatus())
}

// handleAriaPaneEvals returns the last few autonomous-eval outcomes
// for a single pane (newest first) so the panel can show WHY ARIA
// declined / errored / fired instead of leaving the user with only a
// "backed off" badge. Empty array if the pane has no recorded outcomes
// yet — 200, not 404, since "never been evaluated" is normal.
func (a *apiServer) handleAriaPaneEvals(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	paneID := strings.TrimSpace(r.PathValue("id"))
	if paneID == "" {
		http.Error(w, "pane id required", http.StatusBadRequest)
		return
	}
	outcomes := a.aria.recentEvalOutcomes(paneID)
	if outcomes == nil {
		outcomes = []AriaEvalOutcome{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pane_id":  paneID,
		"outcomes": outcomes,
	})
}

// handleAriaRecent returns the most recent ARIA auto-approval decisions across
// ALL panes (newest first), each enriched with the pane's title/kind/auto-mode.
// This is the cross-pane answer to "auto-approval is on but this prompt didn't
// go through" — the user sees fired/declined/skipped + the reason without
// having to know (or query) a specific pane id. ?limit=N (default 100, max 500).
func (a *apiServer) handleAriaRecent(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if s := strings.TrimSpace(r.URL.Query().Get("limit")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}
	type decisionDTO struct {
		AriaEvalOutcome
		PaneTitle string `json:"pane_title,omitempty"`
		PaneKind  string `json:"pane_kind,omitempty"`
		AutoMode  string `json:"auto_mode,omitempty"`
	}
	raw := a.aria.recentDecisionsAll(limit)
	out := make([]decisionDTO, 0, len(raw))
	for _, o := range raw {
		d := decisionDTO{AriaEvalOutcome: o}
		if p, ok := a.mgr.Get(o.PaneID); ok {
			snap := p.Snapshot()
			d.PaneTitle = snap.Title
			d.PaneKind = snap.Kind
			d.AutoMode = snap.AutoMode
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

// handleAriaConfirm resolves a pending confirmation. The UI POSTs here
// when the user clicks Approve or Cancel on a confirmation card — which
// includes the CANCEL button on a cautious-mode auto-approve countdown
// (PaneHeaderBadges), so this endpoint must stay available even while
// chat is disabled: the confirm registry serves both chat confirms and
// the autonomous countdown confirms the daemon arms itself. Auth is the
// same bearer-token gate the rest of /api/* uses.
func (a *apiServer) handleAriaConfirm(w http.ResponseWriter, r *http.Request) {
	if a.aria == nil {
		http.Error(w, "ARIA is disabled", http.StatusServiceUnavailable)
		return
	}
	var req ariaConfirmReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	// Approving answers a pane's prompt (and "approve rest" skips the rest
	// of the turn's dialogs); cancelling stays open to anyone.
	// The confirmation's pane: an agent never approves its own.
	note := func(string) {}
	if req.Approve {
		var target *Pane
		if paneID := a.aria.confirmPane(req.ID); paneID != "" {
			target, _ = a.mgr.Get(paneID)
		}
		var ok bool
		if note, ok = a.admitPromptAnswer(w, r, target, "approve an Aria confirmation"); !ok {
			return
		}
	}
	if !a.aria.signalConfirm(req.ID, req.Approve, req.ApproveRest) {
		http.Error(w, "unknown or expired confirm id", http.StatusNotFound)
		return
	}
	note("aria-confirm:" + req.ID)
	w.WriteHeader(http.StatusNoContent)
}
