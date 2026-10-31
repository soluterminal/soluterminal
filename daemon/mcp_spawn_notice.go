package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const spawnRevealInterval = 10 * time.Second

// spawnRevealLimiter bounds disruptive show requests without limiting spawns.
// The empty caller shares one bucket; missing attribution cannot bypass it.
type spawnRevealLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *spawnRevealLimiter) allow(caller string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Retain only active cooldowns, rather than every caller seen since boot.
	for id, last := range l.last {
		if now.Sub(last) >= spawnRevealInterval {
			delete(l.last, id)
		}
	}
	caller = strings.TrimSpace(caller)
	if _, cooling := l.last[caller]; cooling {
		return false
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	l.last[caller] = now
	return true
}

// announcePaneSpawn tells the people watching that an agent opened a pane:
// a toast with the starter, the new pane and its name, which the web client
// also keeps in the Alerts "Recent" list so it is still there when they come
// back. Published only after the pane is persisted and attached to its
// workspace, so the notice never names a pane that failed to start.
func (a *apiServer) announcePaneSpawn(p *Pane, origin *PaneOrigin, title, workspace string, number int, show ...bool) {
	if p == nil {
		return
	}
	ev := coordinationEvent{Kind: "spawn", Pane: p.ID, Agent: p.Snapshot().Agent}
	if origin != nil {
		ev.By, ev.Lead, ev.Started = origin.ByPaneID, origin.LeadPaneID, origin.Source
	}
	coordLog.record(ev)
	if a.uiBus == nil {
		return
	}
	who := "An agent"
	if origin != nil && origin.ByPaneTitle != "" {
		who = origin.ByPaneTitle
	}
	pane := "a pane"
	if number > 0 {
		pane = fmt.Sprintf("pane #%d", number)
	}
	where := ""
	if workspace != "" {
		where = " in " + workspace
	}
	reveal := len(show) > 0 && show[0]
	a.uiBus.publish(uiEvent{
		Type:      "pane.spawned",
		Show:      reveal,
		PaneID:    p.ID,
		PaneTitle: title,
		Workspace: workspace,
		Text:      fmt.Sprintf("%s started %s%s — %s", who, pane, where, title),
	})
}
