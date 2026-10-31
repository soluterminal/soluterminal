package main

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// PaneOrigin records how a pane came to exist, for the people looking at it:
// who or what started it, which lead it works for, and what it was started
// for. It answers "what is this pane?" when the user comes back to panes an
// agent opened, and is snapshotted at creation so it survives the creator
// closing. kill_pane and set_pane_info read it to tell an agent's own panes
// (which it may close and rename) from the person's, which need the person's
// OK in the app (pane_confirm.go). The caller header is self-declared, so this
// guards against a confused or misled agent, not a hostile program.
type PaneOrigin struct {
	// Source is "user" (opened from the app), "agent" (spawn_pane from
	// another pane), "schedule", or "api" (any other caller). A pane without
	// an origin predates this record: show nothing rather than guess.
	Source string `json:"source"`
	// ByPaneID/ByPaneTitle/ByAgent name the pane whose agent started this
	// one, as it was labelled at that moment.
	ByPaneID    string `json:"byPaneId,omitempty"`
	ByPaneTitle string `json:"byPaneTitle,omitempty"`
	ByAgent     string `json:"byAgent,omitempty"`
	// LeadPaneID/LeadPaneTitle name the top of a chain of spawns: the
	// creator itself, or the creator's own lead when it was agent-started.
	LeadPaneID    string `json:"leadPaneId,omitempty"`
	LeadPaneTitle string `json:"leadPaneTitle,omitempty"`
	// Purpose is what the pane was started for, as given at start. It is
	// historical: the pane's current work can differ.
	Purpose   string `json:"purpose,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	At        int64  `json:"at"`
}

const (
	paneOriginUser     = "user"
	paneOriginAgent    = "agent"
	paneOriginSchedule = "schedule"
	paneOriginAPI      = "api"

	spawnTitleMaxRunes   = 48
	spawnPurposeMaxRunes = 200
)

func (o *PaneOrigin) clone() *PaneOrigin {
	if o == nil {
		return nil
	}
	c := *o
	return &c
}

// agentSpawnOrigin builds the origin of a pane that the agent in creator
// started. creator may be nil (an unattributed MCP caller): the pane is then
// an "api" pane, never presented as the user's.
func agentSpawnOrigin(creator *Pane, purpose, workspace string, now time.Time) *PaneOrigin {
	o := &PaneOrigin{Source: paneOriginAPI, Purpose: clipRunes(oneLine(purpose), spawnPurposeMaxRunes), Workspace: workspace, At: now.UnixMilli()}
	if creator == nil {
		return o
	}
	snap := creator.Snapshot()
	o.Source = paneOriginAgent
	o.ByPaneID = snap.ID
	o.ByPaneTitle = displayTitle(creator)
	// Name the starter's agent only when one is actually running there.
	switch {
	case creator.HasClaudeDescendant():
		o.ByAgent = agentClaude
	case creator.HasCodexDescendant() || snap.Agent == agentCodex:
		o.ByAgent = agentCodex
	}
	o.LeadPaneID, o.LeadPaneTitle = o.ByPaneID, o.ByPaneTitle
	if parent := snap.Origin; parent != nil && parent.Source == paneOriginAgent && parent.LeadPaneID != "" {
		o.LeadPaneID, o.LeadPaneTitle = parent.LeadPaneID, parent.LeadPaneTitle
	}
	return o
}

// spawnPaneTitle is the label an agent-started pane is shown with: the
// caller's title when given, else "<Agent> · from <creator>". It never uses
// the first words of the prompt: boilerplate such as "You are an independent
// reviewer…" is not a name.
func spawnPaneTitle(title, agent string, origin *PaneOrigin) string {
	if t := clipRunes(oneLine(title), spawnTitleMaxRunes); t != "" {
		return t
	}
	name := agentDisplayName(agent)
	if name == "" {
		name = "Terminal"
	}
	if origin == nil || origin.ByPaneTitle == "" {
		return name
	}
	return clipRunes(name+" · from "+origin.ByPaneTitle, spawnTitleMaxRunes)
}

// spawnPurpose is the stated purpose, or else the first line of the starting
// prompt, so the details card can show what the pane was asked to do.
func spawnPurpose(purpose, prompt string) string {
	if p := strings.TrimSpace(purpose); p != "" {
		return p
	}
	for _, line := range strings.Split(prompt, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}

// paneStartedBy is the one-line provenance agents see in list_panes:
// "agent: #5 Claude (lead #2 Claude) — Engine deletion", "schedule", "api".
func paneStartedBy(o *PaneOrigin) string {
	if o == nil || o.Source == paneOriginUser {
		return ""
	}
	out := o.Source
	if o.ByPaneTitle != "" {
		out += ": " + o.ByPaneTitle
		if o.LeadPaneID != "" && o.LeadPaneID != o.ByPaneID {
			out += " (lead " + o.LeadPaneTitle + ")"
		}
	}
	if o.Purpose != "" {
		out += " — " + clipRunes(o.Purpose, 80)
	}
	return out
}

// paneStartedByCaller reports whether caller started the pane or leads the
// chain that did: the panes an agent may close or rename without the person's
// OK.
func paneStartedByCaller(o *PaneOrigin, caller string) bool {
	return o != nil && o.Source == paneOriginAgent && caller != "" && (o.ByPaneID == caller || o.LeadPaneID == caller)
}

// remapOrigin rewrites the starter and lead ids after a workspace restore,
// which brings panes back under new ids. Reports whether anything changed.
func (p *Pane) remapOrigin(newIDs map[string]string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Origin == nil {
		return false
	}
	o := p.Origin.clone()
	changed := false
	if id, ok := newIDs[o.ByPaneID]; ok && id != "" && o.ByPaneID != "" {
		o.ByPaneID, changed = id, true
	}
	if id, ok := newIDs[o.LeadPaneID]; ok && id != "" && o.LeadPaneID != "" {
		o.LeadPaneID, changed = id, true
	}
	if changed {
		p.Origin = o
	}
	return changed
}

// remapPaneOrigins handles POST /api/pane-origins/remap. After the app
// restores a workspace it sends {remap: {oldID: newID}} so restored panes'
// "started by" and lead links (and the kill_pane guard that reads them) point
// at the panes as they now are. Only old ids that are no longer running are
// rewritten; the pairs are also remembered (paneOriginAliases) for panes
// restored later.
func (a *apiServer) remapPaneOrigins(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Remap map[string]string `json:"remap"`
	}
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	remap := map[string]string{}
	for oldID, newID := range req.Remap {
		if oldID == "" || newID == "" {
			continue
		}
		// A running pane keeps its id; an exited one was replaced.
		if old, ok := a.mgr.Get(oldID); ok && old.Snapshot().Status == PaneRunning {
			continue
		}
		if _, ok := a.mgr.Get(newID); !ok {
			continue
		}
		remap[oldID] = newID
	}
	// Remembered for panes restored later, from other workspaces.
	a.originAliases.add(remap, time.Now())
	updated := 0
	for _, p := range a.mgr.List() {
		if p.remapOrigin(remap) {
			updated++
			if a.panes != nil {
				_ = a.panes.Put(p)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}
