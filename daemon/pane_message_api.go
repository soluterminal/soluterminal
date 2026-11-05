package main

import (
	"errors"
	"net/http"
	"strings"
)

// Broadcast uses verified agent delivery. Keep /inject raw: voice and ordinary
// keystrokes rely on its byte-for-byte behavior.
func (a *apiServer) submitPaneMessage(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "submit a pane message", "ask_pane", "send_input") {
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	body := strings.TrimRight(req.Text, "\r\n")
	if strings.TrimSpace(body) == "" {
		http.Error(w, "text required", http.StatusBadRequest)
		return
	}
	// This endpoint accepts message bodies, never raw control sequences.
	for _, ch := range body {
		if ch < 0x20 && ch != '\n' && ch != '\t' {
			http.Error(w, "message contains control characters", http.StatusBadRequest)
			return
		}
	}
	if p.Snapshot().Status != PaneRunning {
		http.Error(w, "pane has exited", http.StatusConflict)
		return
	}
	agent, claude := paneAgentProvider(p)
	if !agent {
		http.Error(w, "verified delivery requires a terminal Claude or Codex pane", http.StatusConflict)
		return
	}
	if !a.promptInputGate(r)(p) {
		http.Error(w, "human_required", http.StatusForbidden)
		return
	}
	if reason := sendInputMenuCollision(p, body+"\n"); reason != "" {
		http.Error(w, reason, http.StatusConflict)
		return
	}
	// Only the person's own message box may type onto a put-back message
	// they kept as their draft; any other caller of this route may not.
	submit := submitAgentMessage
	if a.identifyCaller(r).Kind == callerHuman {
		submit = submitPersonMessage
	}
	bytes, outcome, err := submit(p, body, claude, "you")
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errDeliveryChoicePrompt) {
			status = http.StatusConflict
		}
		if errors.Is(err, errPutBackInBox) {
			// Said to the person, whose message box this is.
			sender := "an agent"
			if msg := p.Snapshot().UnsentMessage; msg != nil {
				sender = msg.Sender
			}
			http.Error(w, "Nothing was sent: your input box holds an unsent message from "+sender+". Send or Clear it on the pane first, or remove it from the input box yourself.", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes": bytes, "outcome": outcome})
}
