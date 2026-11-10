package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPaneReviewNormalizeClipsAndRejectsUnknownVerdicts(t *testing.T) {
	if err := (&PaneReview{Verdict: "lgtm"}).normalize(time.Now()); err == nil {
		t.Fatal("an unknown verdict must be rejected")
	}
	r := &PaneReview{Verdict: " Request-Changes ", Reviewer: "codex", Title: strings.Repeat("t", 500)}
	for i := 0; i < 20; i++ {
		r.Findings = append(r.Findings, PaneReviewFinding{Severity: "high", What: "x", Blocking: true})
	}
	if err := r.normalize(time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "request-changes" || len(r.Findings) != paneReviewMaxFindings || len([]rune(r.Title)) > 200 || r.At != 5 {
		t.Fatalf("normalized review = %+v", r)
	}
}

func TestReviewHeadlineNamesTheReviewerAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		r    PaneReview
		want string
	}{
		{PaneReview{Verdict: "approve", Reviewer: agentCodex}, "Codex reviewed: approved"},
		{PaneReview{Verdict: "request-changes", Reviewer: agentClaude, MustFix: 2}, "reviewed: 2 to fix"},
		{PaneReview{Verdict: "comment", Reviewer: agentCodex}, "has questions"},
		{PaneReview{Verdict: "error"}, "could not run"},
	} {
		if got := reviewHeadline(&tc.r); !strings.Contains(got, tc.want) {
			t.Fatalf("%s: headline %q, want %q", tc.r.Verdict, got, tc.want)
		}
	}
}

func TestReportPaneReviewStoresTheResultFromLoopbackOnly(t *testing.T) {
	p := &Pane{ID: "pane-r", Title: "claude", buffer: NewRingBuffer(64)}
	a := &apiServer{mgr: testManagerWithPane(p)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/panes/{id}/review", a.reportPaneReview)
	post := func(remote, body string) int {
		req := httptest.NewRequest("POST", "/api/panes/pane-r/review", strings.NewReader(body))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	body := `{"verdict":"approve","reviewer":"codex","author":"claude","round":1,"findings":[{"severity":"low","what":"nit","blocking":false}]}`
	if code := post("192.168.1.9:5000", body); code != http.StatusForbidden {
		t.Fatalf("remote report = %d, want 403", code)
	}
	if p.Snapshot().Review != nil {
		t.Fatal("a refused report must not be stored")
	}
	if code := post("127.0.0.1:5000", `{"verdict":"maybe"}`); code != http.StatusBadRequest {
		t.Fatalf("bad verdict = %d, want 400", code)
	}
	if code := post("127.0.0.1:5000", body); code != http.StatusOK {
		t.Fatalf("loopback report = %d", code)
	}
	got := p.Snapshot().Review
	if got == nil || got.Verdict != "approve" || got.Reviewer != "codex" || got.Author != "claude" || len(got.Findings) != 1 || got.At == 0 {
		t.Fatalf("stored review = %+v", got)
	}
	if persisted := persistedFromSnapshot(p.Snapshot()); persisted.Review == nil || persisted.Review == got {
		t.Fatalf("the review must be saved with the pane as a copy: %+v", persisted.Review)
	}
}

func TestReviewFollowupAndClear(t *testing.T) {
	p := &Pane{ID: "pane-r", Title: "claude", buffer: NewRingBuffer(64)}
	a := &apiServer{mgr: testManagerWithPane(p)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/panes/{id}/review", a.reportPaneReview)
	mux.HandleFunc("POST /api/panes/{id}/review/followup", a.reportPaneReviewFollowup)
	mux.HandleFunc("DELETE /api/panes/{id}/review", a.clearPaneReview)
	do := func(method, path, remote, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	const local = "127.0.0.1:5000"
	if code := do("POST", "/api/panes/pane-r/review/followup", local, `{"note":"fixed"}`); code != http.StatusConflict {
		t.Fatalf("follow-up with no review = %d, want 409", code)
	}
	do("POST", "/api/panes/pane-r/review", local, `{"verdict":"request-changes","mustFix":1,"round":2}`)
	if code := do("POST", "/api/panes/pane-r/review/followup", "10.0.0.2:5000", `{"note":"fixed"}`); code != http.StatusForbidden {
		t.Fatalf("remote follow-up = %d, want 403", code)
	}
	if code := do("POST", "/api/panes/pane-r/review/followup", local, `{"note":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("empty note = %d, want 400", code)
	}
	if code := do("POST", "/api/panes/pane-r/review/followup", local, `{"note":"Setting saves before it takes effect","head":"442818cbc"}`); code != http.StatusOK {
		t.Fatalf("follow-up = %d", code)
	}
	got := p.Snapshot().Review
	if got.Verdict != "request-changes" || got.Followup == nil || got.Followup.Note != "Setting saves before it takes effect" || got.Followup.At == 0 {
		t.Fatalf("review after follow-up = %+v", got)
	}
	// A new review replaces the follow-up.
	do("POST", "/api/panes/pane-r/review", local, `{"verdict":"approve","round":1}`)
	if p.Snapshot().Review.Followup != nil {
		t.Fatal("a new review kept the old follow-up")
	}
	if code := do("DELETE", "/api/panes/pane-r/review", local, ""); code != http.StatusNoContent || p.Snapshot().Review != nil {
		t.Fatalf("clear = %d, review %+v", code, p.Snapshot().Review)
	}
}

// A follow-up captured against one review never lands on a newer one.
func TestReviewFollowupNeverOverwritesANewerReview(t *testing.T) {
	p := &Pane{ID: "pane-r", Title: "claude", buffer: NewRingBuffer(64)}
	old := &PaneReview{Verdict: "request-changes", MustFix: 1}
	p.SetReview(old)
	newer := &PaneReview{Verdict: "approve"}
	p.SetReview(newer) // landed between the follow-up's read and its write
	withNote := old.clone()
	withNote.Followup = &PaneReviewFollowup{Note: "fixed"}
	if p.replaceReviewIf(old, withNote) {
		t.Fatal("follow-up replaced a newer review")
	}
	if got := p.Snapshot().Review; got.Verdict != "approve" || got.Followup != nil {
		t.Fatalf("review now %+v", got)
	}
	if !p.replaceReviewIf(newer, withNote) {
		t.Fatal("swap against the current review failed")
	}
}
