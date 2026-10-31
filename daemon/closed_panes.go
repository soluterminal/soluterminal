package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Recently closed panes, kept by the DAEMON that owned them so "reopen" uses
// that daemon's authoritative snapshot: the conversation identity it had at
// close time, role / provider / effort / model / context pack, managed-harness
// sessions, and the resume command built by resumeSpawnOpts — never a
// client-side replay of a possibly stale argv. Each backend runs its own
// daemon, so a pane can only ever be reopened on the backend it lived on.
//
// GET  /api/panes/closed              → newest first
// POST /api/panes/closed/{id}/reopen  → spawns a NEW pane resuming it

const (
	closedPanesCap    = 10
	closedPaneMaxAge  = 24 * time.Hour
	closedPanesFile   = "closed_panes.json"
	closedPaneMaxBody = 1 << 10
)

type closedPaneEntry struct {
	Snap       PaneSnapshot `json:"snap"`
	LiveCwd    string       `json:"liveCwd,omitempty"`
	ClosedAtMs int64        `json:"closedAtMs"`
}

type closedPaneStore struct {
	mu   sync.Mutex
	path string
	list []closedPaneEntry // newest first
	now  func() time.Time
}

func newClosedPaneStore(dataDir string) *closedPaneStore {
	s := &closedPaneStore{now: time.Now}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, closedPanesFile)
		if data, err := os.ReadFile(s.path); err == nil {
			_ = json.Unmarshal(data, &s.list)
		}
	}
	s.mu.Lock()
	s.pruneLocked()
	s.mu.Unlock()
	return s
}

func (s *closedPaneStore) pruneLocked() {
	cutoff := s.now().Add(-closedPaneMaxAge).UnixMilli()
	out := s.list[:0]
	for _, e := range s.list {
		if e.ClosedAtMs >= cutoff {
			out = append(out, e)
		}
	}
	s.list = out
	if len(s.list) > closedPanesCap {
		s.list = s.list[:closedPanesCap]
	}
}

func (s *closedPaneStore) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.list)
	if err == nil {
		_ = writeFileAtomic(s.path, data, 0o600)
	}
}

func (s *closedPaneStore) add(e closedPaneEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ClosedAtMs == 0 {
		e.ClosedAtMs = s.now().UnixMilli()
	}
	next := []closedPaneEntry{e}
	for _, old := range s.list {
		if old.Snap.ID != e.Snap.ID {
			next = append(next, old)
		}
	}
	s.list = next
	s.pruneLocked()
	s.saveLocked()
}

func (s *closedPaneStore) entries() []closedPaneEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return append([]closedPaneEntry(nil), s.list...)
}

// take removes and returns an entry (put it back with add on a failed reopen).
func (s *closedPaneStore) take(id string) (closedPaneEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for i, e := range s.list {
		if e.Snap.ID == id {
			s.list = append(s.list[:i:i], s.list[i+1:]...)
			s.saveLocked()
			return e, true
		}
	}
	return closedPaneEntry{}, false
}

// closedPaneKind summarizes what reopening will bring back.
func closedPaneKind(snap PaneSnapshot) string {
	switch {
	case strings.TrimSpace(snap.HarnessSessionID) != "" || cmdLaunchesManagedHarness(snap.Cmd):
		return "harness"
	case paneSnapshotConversationID(snap) == "":
		return "shell"
	default:
		return string(effectiveAgentKind(snap.Agent))
	}
}

// reopenSpawnOpts rebuilds a closed pane. A pane with a conversation (or a
// managed harness) goes through resumeSpawnOpts — the same path as resuming an
// exited pane. A pane without one comes back as a fresh shell in the folder it
// was in, with any stale resume arguments stripped from its command.
func reopenSpawnOpts(e closedPaneEntry, liveCols, liveRows uint16) SpawnOpts {
	snap, _ := healPaneSnapshot(e.Snap)
	var opts SpawnOpts
	if kind := closedPaneKind(snap); kind != "shell" {
		opts = resumeSpawnOpts(snap, liveCols, liveRows)
	} else {
		cwd := snap.Cwd
		if e.LiveCwd != "" {
			cwd = e.LiveCwd
		}
		opts = SpawnOpts{
			Cwd:         cwd,
			Cmd:         stripMuseResumeArgv(stripCodexResumeArgv(stripResumeArgv(snap.Cmd))),
			Title:       snap.Title,
			Cols:        snap.Cols,
			Rows:        snap.Rows,
			Provider:    snap.Provider,
			Effort:      snap.Effort,
			Model:       snap.Model,
			Role:        snap.Role,
			ContextPack: snap.ContextPack,
		}
		if liveCols > 0 && liveRows > 0 {
			opts.Cols, opts.Rows = liveCols, liveRows
		} else {
			opts.DimsUnverified = true
		}
	}
	// Reopen creates a new pane: retain the lock, but never enable auto-mode
	// as a side effect of this otherwise ungated lifecycle action.
	opts.AutoMode = ""
	opts.NeverAutoAnswer = snap.NeverAutoAnswer
	opts.ID = "" // a new pane; the closed id may still be referenced elsewhere
	return opts
}

func (a *apiServer) listClosedPanes(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Cwd        string `json:"cwd"`
		Kind       string `json:"kind"` // claude | codex | muse | harness | shell
		ClosedAtMs int64  `json:"closedAtMs"`
	}
	out := []row{}
	if a.closed != nil {
		for _, e := range a.closed.entries() {
			cwd := e.Snap.Cwd
			kind := closedPaneKind(e.Snap)
			if kind == "shell" && e.LiveCwd != "" {
				cwd = e.LiveCwd
			}
			title := e.Snap.Title
			if title == "" {
				title = strings.Join(e.Snap.Cmd, " ")
			}
			out = append(out, row{ID: e.Snap.ID, Title: title, Cwd: cwd, Kind: kind, ClosedAtMs: e.ClosedAtMs})
		}
	}
	writeJSON(w, 200, map[string]any{"panes": out})
}

func (a *apiServer) reopenClosedPane(w http.ResponseWriter, r *http.Request) {
	if a.closed == nil {
		http.Error(w, "no closed panes", 404)
		return
	}
	var live resizeReq
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, closedPaneMaxBody)).Decode(&live)
	}
	e, ok := a.closed.take(r.PathValue("id"))
	if !ok {
		http.Error(w, "that pane is no longer in the recently-closed list", 404)
		return
	}
	p, err := a.spawnPaneForRequest(reopenSpawnOpts(e, live.Cols, live.Rows))
	if err != nil {
		a.closed.add(e)
		http.Error(w, err.Error(), 500)
		return
	}
	if a.panes != nil {
		if err := a.panes.Put(p); err != nil {
			a.rollbackSpawnedPane(p)
			a.closed.add(e)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	writeJSON(w, 201, makePaneDTO(p, a.aria, a.attention))
}
