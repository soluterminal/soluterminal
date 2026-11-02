package main

import (
	"sync"
	"time"
)

// Agent-originated layout autosaves are expected refusals. Emit at most one
// diagnostic per caller per minute while preserving the ordinary 403 response.
type workspaceSaveLogCaller struct {
	kind callerKind
	pane string
	pid  int
}

type workspaceSaveLogLimiter struct {
	mu   sync.Mutex
	last map[workspaceSaveLogCaller]time.Time
}

func (l *workspaceSaveLogLimiter) allow(id callerIdentity, now time.Time) bool {
	key := workspaceSaveLogCaller{kind: id.Kind, pane: id.PaneID}
	if key.pane == "" {
		key.pid = id.PID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = make(map[workspaceSaveLogCaller]time.Time)
	}
	for caller, ts := range l.last {
		if now.Sub(ts) >= time.Minute {
			delete(l.last, caller)
		}
	}
	if _, exists := l.last[key]; exists {
		return false
	}
	l.last[key] = now
	return true
}
