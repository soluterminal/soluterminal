package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// AriaEvalOutcome is one entry in the per-pane autonomous-eval ring
// buffer. Surfaced through GET /api/aria/panes/{id}/evals so the user
// can see WHY ARIA declined / errored / fired on a given pane instead
// of only seeing a "backed off" badge with no reasoning.
type AriaEvalOutcome struct {
	At           time.Time `json:"at"`
	PaneID       string    `json:"pane_id"`
	Kind         string    `json:"kind"`
	Reason       string    `json:"reason,omitempty"`
	TailSnapshot string    `json:"tail_snapshot,omitempty"`
	Model        string    `json:"model,omitempty"`
	LatencyMs    int64     `json:"latency_ms"`
	ToolCalls    int       `json:"tool_calls"`
	SendInput    string    `json:"send_input,omitempty"`
}

const (
	ariaEvalLogPerPane      = 5
	ariaEvalLogMaxPanes     = 200
	ariaEvalLogTailMax      = 4096
	ariaEvalLogReasonMax    = 240
	ariaEvalLogSendInputMax = 200
)

type ariaEvalRing struct {
	entries [ariaEvalLogPerPane]AriaEvalOutcome
	count   int
	next    int
	touched time.Time
}

type ariaEvalLog struct {
	mu    sync.Mutex
	panes map[string]*ariaEvalRing
}

func newAriaEvalLog() *ariaEvalLog {
	return &ariaEvalLog{panes: map[string]*ariaEvalRing{}}
}

func (l *ariaEvalLog) record(o AriaEvalOutcome) { l.recordOpt(o, false) }

// recordDedup records o only if the pane's newest entry isn't already an
// identical (same Kind + TailSnapshot) record. The 5s recovery sweeper
// re-runs the native always-mode fire path for a pane parked on the same
// question every tick; without dedup that floods the 5-slot ring with
// repeat "skipped: AskUserQuestion" entries and evicts real fires.
func (l *ariaEvalLog) recordDedup(o AriaEvalOutcome) { l.recordOpt(o, true) }

func (l *ariaEvalLog) recordOpt(o AriaEvalOutcome, dedup bool) {
	if l == nil || o.PaneID == "" {
		return
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	if len(o.TailSnapshot) > ariaEvalLogTailMax {
		o.TailSnapshot = o.TailSnapshot[:ariaEvalLogTailMax]
	}
	if len(o.Reason) > ariaEvalLogReasonMax {
		o.Reason = o.Reason[:ariaEvalLogReasonMax]
	}
	if len(o.SendInput) > ariaEvalLogSendInputMax {
		o.SendInput = o.SendInput[:ariaEvalLogSendInputMax]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.panes[o.PaneID]
	if dedup && ok && r.count > 0 {
		newest := r.entries[(r.next-1+ariaEvalLogPerPane)%ariaEvalLogPerPane]
		if newest.Kind == o.Kind && newest.TailSnapshot == o.TailSnapshot {
			return
		}
	}
	if !ok {
		r = &ariaEvalRing{}
		l.panes[o.PaneID] = r
		l.evictIfNeededLocked(o.PaneID)
	}
	r.entries[r.next] = o
	r.next = (r.next + 1) % ariaEvalLogPerPane
	if r.count < ariaEvalLogPerPane {
		r.count++
	}
	r.touched = o.At
}

// recentAll merges every pane's ring into one newest-first slice, capped at
// limit. Powers GET /api/aria/recent — a single cross-pane "what did ARIA
// just decide, and why" view so the user never has to know a pane id to
// debug a missed auto-approval.
func (l *ariaEvalLog) recentAll(limit int) []AriaEvalOutcome {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	var all []AriaEvalOutcome
	for _, r := range l.panes {
		for i := 0; i < r.count; i++ {
			idx := (r.next - 1 - i + ariaEvalLogPerPane) % ariaEvalLogPerPane
			all = append(all, r.entries[idx])
		}
	}
	l.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// evictIfNeededLocked drops the least-recently-touched pane ring once
// we exceed the per-process pane cap. Caller holds l.mu.
func (l *ariaEvalLog) evictIfNeededLocked(justAddedPaneID string) {
	if len(l.panes) <= ariaEvalLogMaxPanes {
		return
	}
	var oldestID string
	var oldestAt time.Time
	for id, r := range l.panes {
		if id == justAddedPaneID {
			continue
		}
		if oldestID == "" || r.touched.Before(oldestAt) {
			oldestID = id
			oldestAt = r.touched
		}
	}
	if oldestID != "" {
		delete(l.panes, oldestID)
	}
}

func (l *ariaEvalLog) recent(paneID string) []AriaEvalOutcome {
	if l == nil || paneID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.panes[paneID]
	if !ok || r.count == 0 {
		return nil
	}
	out := make([]AriaEvalOutcome, 0, r.count)
	// Walk newest → oldest. The slot before `next` holds the newest.
	for i := 0; i < r.count; i++ {
		idx := (r.next - 1 - i + ariaEvalLogPerPane) % ariaEvalLogPerPane
		out = append(out, r.entries[idx])
	}
	return out
}

func (a *Aria) recordEvalOutcome(o AriaEvalOutcome) {
	if a == nil {
		return
	}
	a.evalLog.record(o)
	a.noteAttentionOutcome(o)
}

const ariaModelDeclineReason = "Auto-approve left this request for your review."

// Model explanations belong in the bounded evaluation record, never in the
// attention reason consumed by the inbox, pane DTO and phone push payload.
func (a *Aria) recordModelEvalOutcome(o AriaEvalOutcome) {
	if a == nil {
		return
	}
	a.evalLog.record(o)
	if o.Kind == "declined" {
		o.Reason = ariaModelDeclineReason
	}
	a.noteAttentionOutcome(o)
}

// recordEvalOutcomeDedup is recordEvalOutcome but collapses back-to-back
// identical entries for a pane (used for the per-sweep "never auto-answered"
// skip so it logs once per distinct question, not every 5s).
func (a *Aria) recordEvalOutcomeDedup(o AriaEvalOutcome) {
	if a == nil {
		return
	}
	a.evalLog.recordDedup(o)
	a.noteAttentionOutcome(o)
}

// noteAttentionOutcome flips the pane's attention signal when an auto-eval
// outcome means the human is now required. "declined" (the model or the
// direct-fire recheck left the prompt for the user) and "skipped" (a tool
// ARIA never auto-answers) both carry a reason worth surfacing in the alert.
// Everything else — "fired" (the prompt is clearing), "error" (the sweeper
// retries within seconds; the attentionAutoGrace escalation backstops a
// persistent failure), "skipped_backoff" (pause/caps, classified statically
// by the engine's gate check) — is left to the engine's normal evaluation.
func (a *Aria) noteAttentionOutcome(o AriaEvalOutcome) {
	if o.PaneID == "" || a.mgr == nil {
		return
	}
	switch o.Kind {
	case "declined", "skipped":
	default:
		return
	}
	p, ok := a.mgr.Get(o.PaneID)
	if !ok {
		return
	}
	p.SetAutoDeclined(o.Reason)
	if a.attention != nil {
		a.attention.Refresh(p)
	}
}

func (a *Aria) recentDecisionsAll(limit int) []AriaEvalOutcome {
	if a == nil {
		return nil
	}
	return a.evalLog.recentAll(limit)
}

func (a *Aria) recentEvalOutcomes(paneID string) []AriaEvalOutcome {
	if a == nil {
		return nil
	}
	return a.evalLog.recent(paneID)
}

func renderControlChars(s string) string {
	var out []byte
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			out = append(out, fmt.Sprintf("\\x%02x", s[0])...)
			s = s[1:]
			continue
		}
		switch r {
		case '\n':
			out = append(out, "\\n"...)
		case '\r':
			out = append(out, "\\r"...)
		case '\t':
			out = append(out, "\\t"...)
		default:
			if r < 0x20 || r == 0x7f {
				out = append(out, fmt.Sprintf("\\x%02x", r)...)
			} else {
				out = append(out, s[:size]...)
			}
		}
		s = s[size:]
	}
	return string(out)
}
