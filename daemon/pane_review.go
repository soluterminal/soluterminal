package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// PaneReview is the latest independent review (scripts/solu-review) of the
// work done in a pane, as that script reported it. It lets the person see on
// the pane that the work was reviewed, by which model, and what is still open,
// instead of finding out only from the agent's own report. Informational: the
// report comes from the pane's own session over loopback, so it is a record
// of what the review script said, not a merge gate (GitHub's solu/review
// status is the gate).
type PaneReview struct {
	// Verdict is approve, request-changes, comment (on hold: questions for
	// the person) or error (the review could not run).
	Verdict string `json:"verdict"`
	// Reviewer and Author are agent kinds (claude, codex); a cross-model
	// review has them differ.
	Reviewer  string              `json:"reviewer"`
	Model     string              `json:"model,omitempty"`
	Author    string              `json:"author,omitempty"`
	Fallback  string              `json:"fallback,omitempty"`
	Round     int                 `json:"round"`
	MustFix   int                 `json:"mustFix"`
	Notes     int                 `json:"notes"`
	Title     string              `json:"title,omitempty"`
	Branch    string              `json:"branch,omitempty"`
	Head      string              `json:"head,omitempty"`
	Summary   string              `json:"summary,omitempty"`
	Findings  []PaneReviewFinding `json:"findings,omitempty"`
	Questions []string            `json:"questions,omitempty"`
	Error     string              `json:"error,omitempty"`
	At        int64               `json:"at"`
	// Followup is the author's note that the must-fix items were fixed after
	// this review (scripts/solu-review --fixed). It is not a re-review: the
	// protocol stops at two rounds.
	Followup *PaneReviewFollowup `json:"followup,omitempty"`
}

type PaneReviewFollowup struct {
	Note string `json:"note"`
	Head string `json:"head,omitempty"`
	At   int64  `json:"at"`
}

type PaneReviewFinding struct {
	Severity string `json:"severity"`
	Where    string `json:"where,omitempty"`
	What     string `json:"what"`
	Blocking bool   `json:"blocking"`
}

const (
	paneReviewMaxFindings  = 12
	paneReviewMaxQuestions = 8
)

var paneReviewVerdicts = map[string]bool{"approve": true, "request-changes": true, "comment": true, "error": true}

func (r *PaneReview) clone() *PaneReview {
	if r == nil {
		return nil
	}
	c := *r
	c.Findings = append([]PaneReviewFinding(nil), r.Findings...)
	c.Questions = append([]string(nil), r.Questions...)
	if r.Followup != nil {
		f := *r.Followup
		c.Followup = &f
	}
	return &c
}

// normalize clips every field to a size the UI can show and rejects an
// unknown verdict.
func (r *PaneReview) normalize(now time.Time) error {
	r.Verdict = strings.ToLower(strings.TrimSpace(r.Verdict))
	if !paneReviewVerdicts[r.Verdict] {
		return fmt.Errorf("verdict must be approve, request-changes, comment or error")
	}
	r.Reviewer = clipRunes(oneLine(r.Reviewer), 20)
	r.Model = clipRunes(oneLine(r.Model), 60)
	r.Author = clipRunes(oneLine(r.Author), 20)
	r.Fallback = clipRunes(oneLine(r.Fallback), 200)
	r.Title = clipRunes(oneLine(r.Title), 200)
	r.Branch = clipRunes(oneLine(r.Branch), 120)
	r.Head = clipRunes(oneLine(r.Head), 40)
	r.Summary = clipRunes(strings.TrimSpace(r.Summary), 600)
	r.Error = clipRunes(oneLine(r.Error), 300)
	if len(r.Findings) > paneReviewMaxFindings {
		r.Findings = r.Findings[:paneReviewMaxFindings]
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Severity = clipRunes(oneLine(f.Severity), 12)
		f.Where = clipRunes(oneLine(f.Where), 200)
		f.What = clipRunes(strings.TrimSpace(f.What), 400)
	}
	if len(r.Questions) > paneReviewMaxQuestions {
		r.Questions = r.Questions[:paneReviewMaxQuestions]
	}
	for i := range r.Questions {
		r.Questions[i] = clipRunes(strings.TrimSpace(r.Questions[i]), 300)
	}
	r.At = now.UnixMilli()
	// A follow-up belongs to an earlier result; a new review starts clean.
	r.Followup = nil
	return nil
}

// reviewHeadline is the one-line result for the notice and the header chip's
// tooltip, e.g. "Codex reviewed: 2 to fix".
func reviewHeadline(r *PaneReview) string {
	who := agentDisplayName(r.Reviewer)
	if who == "" {
		who = "A reviewer"
	}
	switch r.Verdict {
	case "approve":
		return who + " reviewed: approved"
	case "request-changes":
		return fmt.Sprintf("%s reviewed: %d to fix", who, r.MustFix)
	case "comment":
		return who + " reviewed: has questions"
	default:
		return "The review could not run"
	}
}

func (p *Pane) SetReview(r *PaneReview) {
	p.mu.Lock()
	p.Review = r
	p.mu.Unlock()
}

// replaceReviewIf swaps in next only while the pane still holds expected (by
// identity), so a follow-up or a revert can't overwrite a review that landed
// in between.
func (p *Pane) replaceReviewIf(expected, next *PaneReview) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Review != expected {
		return false
	}
	p.Review = next
	return true
}

// reportPaneReview handles POST /api/panes/{id}/review from scripts/solu-review
// running inside that pane. Loopback only: the report describes local work.
func (a *apiServer) reportPaneReview(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	var review PaneReview
	if !decodeJSON(w, r, &review, defaultJSONBodyBytes) {
		return
	}
	if err := review.normalize(time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prev := p.Snapshot().Review
	p.SetReview(&review)
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.SetReview(prev)
			http.Error(w, "persist pane: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if a.uiBus != nil {
		snap := p.Snapshot()
		label := paneLabel(paneSlotsByID(a.ws)[snap.ID], snap.Title, paneSummaryAgent(p, snap))
		a.uiBus.publish(uiEvent{
			Type:      "pane.reviewed",
			PaneID:    snap.ID,
			PaneTitle: label,
			Text:      label + " — " + reviewHeadline(&review),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reportPaneReviewFollowup handles POST /api/panes/{id}/review/followup from
// `scripts/solu-review --fixed "<what changed>"`: the author fixed the review's
// must-fix items after the last round. Loopback only, like the review itself.
func (a *apiServer) reportPaneReviewFollowup(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	var body PaneReviewFollowup
	if !decodeJSON(w, r, &body, defaultJSONBodyBytes) {
		return
	}
	body.Note = clipRunes(strings.TrimSpace(body.Note), 400)
	body.Head = clipRunes(oneLine(body.Head), 40)
	if body.Note == "" {
		http.Error(w, "note is required: say what you fixed", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	prev := p.Review
	p.mu.Unlock()
	if prev == nil {
		http.Error(w, "this pane has no review to follow up", http.StatusConflict)
		return
	}
	next := prev.clone()
	body.At = time.Now().UnixMilli()
	next.Followup = &body
	if !p.replaceReviewIf(prev, next) {
		http.Error(w, "the pane's review changed meanwhile; nothing recorded", http.StatusConflict)
		return
	}
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.replaceReviewIf(next, prev)
			http.Error(w, "persist pane: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clearPaneReview handles DELETE /api/panes/{id}/review: the person dismisses
// a result they have dealt with. Only the display record goes; the review
// ledger and GitHub's checks are untouched.
func (a *apiServer) clearPaneReview(w http.ResponseWriter, r *http.Request) {
	p, ok := a.mgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	p.mu.Lock()
	prev := p.Review
	p.Review = nil
	p.mu.Unlock()
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			p.replaceReviewIf(nil, prev)
			http.Error(w, "persist pane: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
