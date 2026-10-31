package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pane confirmations: the person's OK for an agent to close or change a pane
// the agent did not start.
//
// kill_pane, set_pane_info and rename_pane take user_requested: true from an
// agent that says the person asked. That claim is the agent's word alone, and
// an agent that is confused, or that read instructions planted in a file or
// web page, can make it. So the claim no longer acts by itself: it raises a request the
// person answers in the app ("@cmo wants to close pane #4 “Social loop”"),
// and the action happens only once they allow it. The tool call waits for the
// answer for a while (paneConfirmWait) and otherwise tells the agent it is
// waiting; the request stays open for paneConfirmTTL and, when allowed later,
// the daemon carries it out then.
//
// Answering is a person's act (requireHuman). An agent may act on its own
// pane and the panes it started (or leads) without asking, and while the
// person has turned on Settings → Agent permissions, user_requested is taken
// as acting for them, as answer_prompt is.

var (
	// paneConfirmWait is how long the tool call waits for the answer. MCP
	// clients give up on a tool call after about a minute (Codex: 60 s).
	paneConfirmWait = 45 * time.Second
	// paneConfirmTTL is how long an unanswered request stays open.
	paneConfirmTTL = 10 * time.Minute
)

const (
	paneConfirmClose  = "close"
	paneConfirmChange = "change"
	paneConfirmRename = "rename"

	// paneConfirmMaxPerCaller and paneConfirmMaxOpen stop one agent from
	// burying the person in requests.
	paneConfirmMaxPerCaller = 3
	paneConfirmMaxOpen      = 20
)

// paneAgentDecision is what happens to an agent's kill_pane, set_pane_info or
// rename_pane on a pane.
type paneAgentDecision int

const (
	paneAgentAllowed paneAgentDecision = iota
	paneAgentRefused
	paneAgentNeedsOK
)

// paneAgentAuthority decides an agent caller's kill_pane, set_pane_info or
// rename_pane.
// callerPane is the agent's own pane ("" for an agent outside SoluTerminal's
// panes, which owns none). The browser UI never comes through here: it closes
// and renames panes over the REST API.
func (a *apiServer) paneAgentAuthority(target *PaneOrigin, paneID, callerPane string, userRequested bool) paneAgentDecision {
	caller := strings.TrimSpace(callerPane)
	if caller != "" && (caller == paneID || paneStartedByCaller(target, caller)) {
		return paneAgentAllowed
	}
	// Anyone else's pane, including one without an origin record (opened
	// before origins were recorded, or by an older path): the person's.
	if !userRequested {
		return paneAgentRefused
	}
	if caller != "" && a.agentAuthority.active(time.Now()) {
		return paneAgentAllowed
	}
	return paneAgentNeedsOK
}

// paneConfirmView is a request as the app shows it.
type paneConfirmView struct {
	ID           string `json:"id"`
	Action       string `json:"action"`
	CallerPaneID string `json:"callerPaneId,omitempty"`
	Caller       string `json:"caller"`
	PaneID       string `json:"paneId"`
	Pane         string `json:"pane"`
	Title        string `json:"title"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`
}

type paneConfirmRequest struct {
	view paneConfirmView
	key  string
	// bucket is the asking caller for the per-caller limit (paneCaller.bucket).
	bucket string
	// run carries the action out once the person allows it; it returns the
	// tool result the agent sees.
	run   func() map[string]any
	done  chan struct{}
	timer *time.Timer
	// result is set before done closes.
	result map[string]any
}

type paneConfirms struct {
	mu   sync.Mutex
	byID map[string]*paneConfirmRequest
}

func newPaneConfirmID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// paneCaller is the agent making a pane tool call. ID is the pane it traces
// to: the one whose ownership it has ("" for none). Claimed is a pane it only
// says it is (SOLU_PANE_ID in the environment of a process with no pane above
// it, mcpTracedCallerPane): never used for ownership, only to word the prompt.
type paneCaller struct {
	ID      string
	Claimed string
}

// paneConfirmCaller names the asking agent for the person: its @handle, else
// its pane label ("#2 Claude"), with a warning when it couldn't be verified.
func (a *apiServer) paneConfirmCaller(c paneCaller) string {
	if p, ok := a.mgr.Get(strings.TrimSpace(c.ID)); ok {
		return a.paneCallerLabel(p)
	}
	if p, ok := a.mgr.Get(strings.TrimSpace(c.Claimed)); ok {
		return "An agent claiming to be " + a.paneCallerLabel(p) + " (couldn't be verified)"
	}
	return "An agent outside your panes"
}

func (a *apiServer) paneCallerLabel(p *Pane) string {
	if id := p.Snapshot().Identity; id != nil && id.Handle != "" {
		return "@" + id.Handle
	}
	return a.paneLabelFor(p)
}

// paneConfirmBucket groups a caller's open requests for the per-caller limit.
func (c paneCaller) bucket() string {
	if id := strings.TrimSpace(c.ID); id != "" {
		return id
	}
	if claimed := strings.TrimSpace(c.Claimed); claimed != "" {
		return "claimed:" + claimed
	}
	return ""
}

// paneConfirmTarget names the pane the agent wants to act on:
// `pane #4 “Social loop”`.
func (a *apiServer) paneConfirmTarget(p *Pane) string {
	out := "pane"
	if slot := paneSlotsByID(a.ws)[p.ID]; slot.Number > 0 {
		out += " #" + strconv.Itoa(slot.Number)
	}
	if t := strings.TrimSpace(displayTitle(p)); t != "" {
		out += " “" + clipRunes(oneLine(t), spawnTitleMaxRunes) + "”"
	}
	return out
}

// requestPaneConfirm opens a request, or returns the open one for the same
// caller, pane and change so asking again never stacks a second prompt.
func (a *apiServer) requestPaneConfirm(key string, view paneConfirmView, run func() map[string]any) (*paneConfirmRequest, error) {
	return a.requestPaneConfirmFor(key, view.CallerPaneID, view, run)
}

func (a *apiServer) requestPaneConfirmFor(key, bucket string, view paneConfirmView, run func() map[string]any) (*paneConfirmRequest, error) {
	s := &a.paneConfirms
	s.mu.Lock()
	if s.byID == nil {
		s.byID = map[string]*paneConfirmRequest{}
	}
	mine := 0
	for _, req := range s.byID {
		if req.key == key {
			s.mu.Unlock()
			return req, nil
		}
		if req.bucket == bucket {
			mine++
		}
	}
	if mine >= paneConfirmMaxPerCaller {
		s.mu.Unlock()
		return nil, fmt.Errorf("%d of your requests are already waiting for the person's OK in the app; wait for their answer before asking again", mine)
	}
	if len(s.byID) >= paneConfirmMaxOpen {
		s.mu.Unlock()
		return nil, fmt.Errorf("%d requests from agents are already waiting for the person's OK in the app; wait for them to be answered before asking again", len(s.byID))
	}
	now := time.Now()
	view.ID = newPaneConfirmID()
	view.CreatedAt = now.UnixMilli()
	view.ExpiresAt = now.Add(paneConfirmTTL).UnixMilli()
	req := &paneConfirmRequest{view: view, key: key, bucket: bucket, run: run, done: make(chan struct{})}
	id := view.ID
	req.timer = time.AfterFunc(paneConfirmTTL, func() { a.expirePaneConfirm(id) })
	s.byID[id] = req
	s.mu.Unlock()
	log.Printf("pane-confirm: %s asks to %s %s (request %s)", view.Caller, view.Action, view.PaneID, id)
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "pane.confirm", Text: id, Hint: view.Message, PaneID: view.PaneID, Ts: now.UnixMilli()})
	}
	return req, nil
}

// takePaneConfirm removes an open request; the first answer wins.
func (a *apiServer) takePaneConfirm(id string) *paneConfirmRequest {
	s := &a.paneConfirms
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.byID[id]
	if req != nil {
		delete(s.byID, id)
		req.timer.Stop()
	}
	return req
}

func (a *apiServer) settlePaneConfirm(req *paneConfirmRequest, result map[string]any, outcome string) {
	req.result = result
	close(req.done)
	log.Printf("pane-confirm: request %s %s", req.view.ID, outcome)
	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "pane.confirm.resolved", Text: req.view.ID, Reason: outcome, PaneID: req.view.PaneID, Ts: time.Now().UnixMilli()})
	}
}

// decidePaneConfirm records the person's answer and, when they allow it,
// carries the action out. ok is false when no such request is open.
func (a *apiServer) decidePaneConfirm(id string, allow bool) (result map[string]any, ok bool) {
	req := a.takePaneConfirm(id)
	if req == nil {
		return nil, false
	}
	if !allow {
		a.settlePaneConfirm(req, toolErr("The person said no in the app: "+req.view.Pane+" was left as it is. Don't ask again unless they tell you to."), "denied")
		return req.result, true
	}
	a.settlePaneConfirm(req, req.run(), "allowed")
	return req.result, true
}

func (a *apiServer) expirePaneConfirm(id string) {
	if req := a.takePaneConfirm(id); req != nil {
		a.settlePaneConfirm(req, toolErr(fmt.Sprintf("Nobody answered in the app within %s: %s was left as it is.", paneConfirmTTLText(), req.view.Pane)), "expired")
	}
}

// dropPaneConfirmsFor ends the open requests about a pane that closed, or
// asked by an agent whose pane closed.
func (a *apiServer) dropPaneConfirmsFor(paneID string) {
	if paneID == "" {
		return
	}
	s := &a.paneConfirms
	s.mu.Lock()
	var gone []string
	for id, req := range s.byID {
		if req.view.PaneID == paneID || req.view.CallerPaneID == paneID {
			gone = append(gone, id)
		}
	}
	s.mu.Unlock()
	for _, id := range gone {
		if req := a.takePaneConfirm(id); req != nil {
			a.settlePaneConfirm(req, toolErr(req.view.Pane+" or the asking agent's pane closed, so the request was dropped; nothing was changed."), "closed")
		}
	}
}

// awaitPaneConfirm waits for the answer for up to paneConfirmWait.
func awaitPaneConfirm(req *paneConfirmRequest, waiting string) map[string]any {
	select {
	case <-req.done:
		return req.result
	case <-time.After(paneConfirmWait):
		return toolOK(waiting)
	}
}

func paneConfirmTTLText() string {
	if m := int(paneConfirmTTL / time.Minute); m > 1 {
		return strconv.Itoa(m) + " minutes"
	}
	return paneConfirmTTL.String()
}

// paneConfirmWaitingText is the tool result while the person has not
// answered yet.
func paneConfirmWaitingText(notYet string) string {
	return fmt.Sprintf("%s: waiting for your OK in the app. The person has been asked to allow it; if they do within %s, it happens then. Don't call again: tell them it is waiting for their OK, and check list_panes later.", notYet, paneConfirmTTLText())
}

func (a *apiServer) pendingPaneConfirms() []paneConfirmView {
	s := &a.paneConfirms
	s.mu.Lock()
	out := make([]paneConfirmView, 0, len(s.byID))
	for _, req := range s.byID {
		out = append(out, req.view)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// handleListPaneConfirms is GET /api/pane-confirmations: the open requests,
// so a window that opens (or reconnects) after one was raised still shows it.
func (a *apiServer) handleListPaneConfirms(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pending": a.pendingPaneConfirms()})
}

// handleAnswerPaneConfirm is POST /api/pane-confirmations/{id} {"allow": bool}.
// Only the person answers: an agent cannot allow its own request.
func (a *apiServer) handleAnswerPaneConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow *bool `json:"allow"`
	}
	if !decodeJSON(w, r, &body, defaultJSONBodyBytes) {
		return
	}
	if body.Allow == nil {
		http.Error(w, `body must set "allow" to true or false`, http.StatusBadRequest)
		return
	}
	if !a.requireHuman(w, r, "answer an agent's request to close or change a pane") {
		return
	}
	result, ok := a.decidePaneConfirm(r.PathValue("id"), *body.Allow)
	if !ok {
		http.Error(w, "this request was already answered or has expired", http.StatusNotFound)
		return
	}
	text, failed := paneConfirmResultText(result)
	writeJSON(w, http.StatusOK, map[string]any{"allowed": *body.Allow, "failed": failed, "result": text})
}

// paneConfirmResultText reads back the text and error flag of a toolOK/toolErr
// result.
func paneConfirmResultText(res map[string]any) (string, bool) {
	failed, _ := res["isError"].(bool)
	var parts []string
	if content, ok := res["content"].([]any); ok {
		for _, c := range content {
			if m, ok := c.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
	}
	return strings.Join(parts, "\n"), failed
}
