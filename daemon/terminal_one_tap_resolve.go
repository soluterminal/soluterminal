package main

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

// choicePromptResolveReq is the body for
// POST /api/panes/{id}/choice-prompt/resolve (ST-265).
type choicePromptResolveReq struct {
	PromptID       string `json:"promptId"`
	ApprovalDigest string `json:"approvalDigest"`
	Allow          bool   `json:"allow"`
}

// errTermInputChanged: input reached the pane after the one-tap's checks.
var errTermInputChanged = errors.New("input reached the pane before the one-tap write")

// termResolveBeforeWrite runs just before the final input check; tests use
// it to land input in that window.
var termResolveBeforeWrite = func(*Pane) {}

// livePromptID is the pane's current choice-prompt id, "" when none.
func livePromptID(p *Pane) string {
	p.ensureGrid()
	_, pos, fp, ok := currentChoicePromptIdentity(p, "")
	if !ok || fp == 0 {
		return ""
	}
	return fmt.Sprintf("term:%d:%016x", pos, fp)
}

// handlePaneChoicePromptResolve answers a terminal Claude Code pane's
// permission menu from a phone one-tap. Every check runs against the live
// screen, in the order @secure set: the digest is mandatory (no unsigned
// one-tap), the caller is the person on an approved device, the live prompt
// and its signed digest match, the menu is a kind one-tap may answer and
// doesn't widen permissions, then the normal prompt-answer admission. Just
// before the write the prompt identity and the focus on option 1 are checked
// again. Allow is Enter on option 1 ("Yes", allow once); deny is Esc.
func (a *apiServer) handlePaneChoicePromptResolve(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	var req choicePromptResolveReq
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.PromptID == "" || req.ApprovalDigest == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "digest_required", "message": "A one-tap answer needs the signed prompt it answers."})
		return
	}
	caller := a.identifyCaller(r)
	device := remoteVerifiedDevice(r)
	if device == "" {
		device = caller.Device
	}
	if caller.Kind != callerHuman || device == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "device_required", "message": "A one-tap answer must come from an approved device."})
		return
	}
	changed := func() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_changed", "message": "This request changed or was already answered. Open the pane to review it."})
	}
	review := func(why string) {
		log.Printf("terminal one-tap: pane=%s refused prompt=%q device=%s: %s", p.ID, req.PromptID, device, why)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "review_required", "message": "Open the pane to review this request."})
	}
	// One resolve at a time per pane, and a prompt is answered once.
	lock := termResolveLock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	if v, ok := termAnswered.Load(p.ID); ok && v.(string) == req.PromptID {
		changed()
		return
	}
	if livePromptID(p) != req.PromptID {
		changed()
		return
	}
	if live := liveTerminalDigest(p.ID, req.PromptID); live == "" || !hmac.Equal([]byte(live), []byte(req.ApprovalDigest)) {
		log.Printf("terminal one-tap: pane=%s refused prompt=%q device=%s digest=%q (does not match the live prompt)", p.ID, req.PromptID, device, truncateStr(req.ApprovalDigest, 64))
		changed()
		return
	}
	// Any input to the pane after this point (a desktop arrow key moving
	// the focus to "always allow", typed before the grid redraws) cancels.
	// inputRevision is bumped under the pane's write lock by every real
	// input, so the compare inside writeWithPrecondition is atomic with
	// the write itself.
	revBefore := p.inputRevision.Load()
	t, why := evaluateTerminalOneTap(p)
	if why != "" {
		review(why)
		return
	}
	if t.PromptID != req.PromptID {
		changed()
		return
	}
	// The signature the digest came from must cover what is on screen now.
	if s := liveTerminalSigned(p.ID, req.PromptID); s == nil || s.tool != t.Tool || s.summary != t.Summary || s.widens != t.Widens {
		changed()
		return
	}
	if req.Allow && t.Widens {
		review("widens the agent's permissions")
		return
	}
	action := "deny a pane's permission request"
	if req.Allow {
		action = "allow a pane's permission request"
	}
	note, ok := a.admitPromptAnswer(w, r, p, action)
	if !ok {
		return
	}
	// Last look before the write: the same live prompt, the same dialog the
	// signature covers, focus still on option 1.
	if !choicePromptIdentityMatches(p, t.pos, t.fp) || livePromptID(p) != req.PromptID {
		changed()
		return
	}
	if grid, visible := p.gridVisibleText(); !visible {
		changed()
		return
	} else if view, why := extractClaudeApprovalDialog(grid); why != "" {
		review(why)
		return
	} else if view.Summary != t.Summary {
		changed()
		return
	}
	termResolveBeforeWrite(p)
	key := []byte("\x1b")
	if req.Allow {
		key = []byte("\r")
	}
	if _, err := p.writeWithPrecondition(key, false, false, func() error {
		if p.inputRevision.Load() != revBefore {
			return errTermInputChanged
		}
		return nil
	}); err != nil {
		if errors.Is(err, errTermInputChanged) {
			changed()
			return
		}
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	termAnswered.Store(p.ID, req.PromptID)
	note(req.PromptID)
	verb := "deny"
	if req.Allow {
		verb = "allow"
	}
	log.Printf("approval: source=%q pane=%s prompt=%q action=%s device=%s digest=%q", "phone one-tap", p.ID, req.PromptID, verb, device, req.ApprovalDigest)
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true, "allow": req.Allow})
}
