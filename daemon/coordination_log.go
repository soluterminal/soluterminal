package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// coordinationLog records pane-to-pane coordination as it happens (workers
// started and closed, messages delivered or left unsent) so the workflow
// scorecard (scripts/workflow-scorecard.mjs) can say whether coordination is
// used and works. Append-only JSON lines under <data>/coordination/; best
// effort — a write failure never affects the action being recorded.
type coordinationLog struct {
	mu  sync.Mutex
	dir string
	now func() time.Time
}

type coordinationEvent struct {
	At   int64  `json:"at"`
	Kind string `json:"kind"` // spawn | close | deliver | queued | unsent
	Pane string `json:"pane"`
	// By: the starting or closing pane id, "app" for the app's own close, or
	// the sender label of a delivered message.
	By    string `json:"by,omitempty"`
	Lead  string `json:"lead,omitempty"`
	Agent string `json:"agent,omitempty"`
	// Started: how the pane was started (pane_origin.go source), on close.
	Started string `json:"started,omitempty"`
	// Own: a close by the pane's starter or lead.
	Own bool `json:"own,omitempty"`
	// Outcome: deliver → submitted | sent_unconfirmed | left_in_composer |
	// queued | menu | would_steer | busy (not typed) | error; queued (a
	// queued Codex or Claude message leaving the queue) → submitted |
	// left_in_composer | gone; unsent → send | clear (confirmed) or
	// send_unresolved | clear_unresolved.
	Outcome string `json:"outcome,omitempty"`
	AgeSec  int64  `json:"ageSec,omitempty"`
}

const (
	coordinationLogFile     = "events.jsonl"
	coordinationLogMaxBytes = 4 << 20
)

var coordLog = &coordinationLog{}

func (l *coordinationLog) setDir(dataDir string) {
	l.mu.Lock()
	l.dir = filepath.Join(dataDir, "coordination")
	l.mu.Unlock()
}

func (l *coordinationLog) record(ev coordinationEvent) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dir == "" {
		return
	}
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	if ev.At == 0 {
		ev.At = now().UnixMilli()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(l.dir, coordinationLogFile)
	if fi, err := os.Stat(path); err == nil && fi.Size() > coordinationLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// recordPaneClose notes a pane closing; origin is the pane's (snapshotted
// before the kill), by the closer.
func recordPaneClose(paneID string, origin *PaneOrigin, by string, own bool) {
	ev := coordinationEvent{Kind: "close", Pane: paneID, By: by, Own: own}
	if origin != nil { // panes from before origins were recorded have none
		ev.Started = origin.Source
		ev.Lead = origin.LeadPaneID
		if origin.At > 0 {
			ev.AgeSec = (time.Now().UnixMilli() - origin.At) / 1000
		}
	}
	coordLog.record(ev)
}
