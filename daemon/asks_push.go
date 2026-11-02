package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// New asks reach the phone as one push per burst: the first new ask starts a
// short window, and every ask posted in it goes out together ("@secure,
// @release-lead are waiting on you"). Updates to an ask never push, and
// pushes are at least askPushGap apart. With only approved devices allowed,
// deliver replaces the title and text with neutral words (minimisePushBody).
const (
	askPushWindow = 30 * time.Second
	askPushGap    = 5 * time.Minute
)

type askPushBatch struct {
	mu      sync.Mutex
	pending []string // ask ids
	// busy: a flush is scheduled or delivering. New asks only join pending
	// meanwhile; the flush schedules the next one after it delivers, so a
	// burst during delivery still waits out askPushGap.
	busy bool
	last time.Time
	// Test seams; zero values mean askPushWindow, askPushGap and the live
	// pusher.
	window, gap time.Duration
	fire        func(pane, title, summary string) bool
}

func (b *askPushBatch) spacing() (time.Duration, time.Duration) {
	window, gap := b.window, b.gap
	if window == 0 {
		window = askPushWindow
	}
	if gap == 0 {
		gap = askPushGap
	}
	return window, gap
}

// scheduleLocked starts the next flush: after the window, and no sooner than
// gap after the last push. Caller holds b.mu.
func (a *apiServer) scheduleAskPushLocked(b *askPushBatch) {
	window, gap := b.spacing()
	wait := window
	if next := time.Until(b.last.Add(gap)); next > wait {
		wait = next
	}
	b.busy = true
	time.AfterFunc(wait, a.flushAskPush)
}

// queueAskPush is the store's onCreated hook.
func (a *apiServer) queueAskPush(x Ask) {
	b := &a.askPush
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, x.ID)
	if !b.busy {
		a.scheduleAskPushLocked(b)
	}
}

func (a *apiServer) flushAskPush() {
	b := &a.askPush
	b.mu.Lock()
	ids := b.pending
	b.pending = nil
	fire := b.fire
	b.mu.Unlock()
	if fire == nil {
		fire = func(pane, title, summary string) bool {
			p := a.getPusher()
			return p != nil && p.FireAsks(pane, title, summary)
		}
	}
	title, summary, pane := a.askPushText(ids)
	sent := pane != "" && fire(pane, title, summary)
	b.mu.Lock()
	defer b.mu.Unlock()
	if sent {
		b.last = time.Now()
	}
	b.busy = false
	if len(b.pending) > 0 {
		a.scheduleAskPushLocked(b)
	}
}

// askPushText words a push for the asks in ids that are still open: who is
// waiting (daemon labels, never agent text) as the title, the ask itself when
// there is one, else how many. pane is the newest ask's pane, which a tap
// opens; "" when none is still open.
func (a *apiServer) askPushText(ids []string) (title, summary, pane string) {
	if a.asks == nil || len(ids) == 0 {
		return "", "", ""
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var open []Ask
	for _, x := range a.asks.list() {
		if x.State == askOpen && want[x.ID] {
			open = append(open, x) // newest first
		}
	}
	if len(open) == 0 {
		return "", "", ""
	}
	var sources []string
	seen := map[string]bool{}
	for _, x := range open {
		label := x.Source
		if p, ok := a.mgr.Get(x.SourcePane); ok {
			label = a.paneCallerLabel(p)
		}
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		sources = append(sources, label)
	}
	switch {
	case len(sources) > 3:
		title = strings.Join(sources[:3], ", ") + fmt.Sprintf(" +%d", len(sources)-3)
	default:
		title = strings.Join(sources, ", ")
	}
	if len(open) == 1 {
		summary = open[0].Text
	} else {
		summary = fmt.Sprintf("%d new asks", len(open))
	}
	return title, summary, open[0].SourcePane
}
