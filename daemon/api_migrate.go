package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errMigrateInProgress = errors.New("pane recovery already in progress")

// migrateReq is the body of POST /api/panes/{id}/migrate-clean.
type migrateReq struct {
	// Mode selects how to replace the degraded pane's nested-child `claude`
	// with a clean top-level one:
	//   "sibling" (default) — spawn a fresh clean pane next to this one and
	//     LEAVE this one running. Non-destructive: nothing is lost; the user
	//     closes the old pane when ready (its scrollback stays readable).
	//   "inplace" — replace the `claude` in THIS pane (same id/position) by
	//     respawning its agent with a clean env. Destructive: the running
	//     claude (whose conversation was never written to disk) is killed and
	//     cannot be resumed. Requires Confirm.
	Mode string `json:"mode"`
	// Confirm must be true for a destructive "inplace" migration when a live
	// claude would be killed. Ignored for "sibling".
	Confirm bool `json:"confirm"`
}

// migratePaneClean handles POST /api/panes/{id}/migrate-clean.
func (a *apiServer) migratePaneClean(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req migrateReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // empty body → defaults
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "sibling"
	}

	switch mode {
	case "sibling":
		np, err := a.doMigrateSibling(p)
		if err != nil {
			a.writeMigrateErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{
			"status": "sibling-created", "method": "sibling", "pane": makePaneDTO(np, a.aria, a.attention),
		})
	case "inplace":
		if p.HasClaudeDescendant() && !req.Confirm {
			writeJSON(w, 409, map[string]any{
				"needsConfirm": true,
				"paneId":       p.ID,
				"willLose":     "the current Claude conversation (it was never written to disk and cannot be resumed)",
				"willKeep":     []string{"cwd", "title", "pane position", "pane id"},
				"hint":         "set confirm:true to proceed, or use mode:sibling to keep this pane and open a clean one beside it",
			})
			return
		}
		np, res, err := a.doMigrateInplace(p)
		if err != nil {
			a.writeMigrateErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"status":       "respawned",
			"method":       "inplace",
			"resumed":      res.resumed,
			"archivedPath": res.archivedPath,
			"pane":         makePaneDTO(np, a.aria, a.attention),
		})
	default:
		http.Error(w, "unknown mode (want sibling|inplace): "+mode, 400)
	}
}

func (a *apiServer) writeMigrateErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errMigrateInProgress) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if _, bad := err.(*SpawnError); bad {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Error(w, err.Error(), 500)
}

// doMigrateSibling spawns a fresh clean `claude` pane next to p, in the same
// workspace + (live) cwd, and leaves p untouched. Returns the new pane.
func (a *apiServer) doMigrateSibling(p *Pane) (*Pane, error) {
	snap := p.Snapshot()
	cwd := snap.Cwd
	if live := snapshotPaneProcesses(p).liveCwd; live != "" {
		cwd = live
	}
	title := strings.TrimSpace(snap.Title)
	if title == "" {
		title = "claude"
	}
	np, err := a.spawnPaneForRequest(SpawnOpts{
		NeverAutoAnswer: snap.NeverAutoAnswer,
		Cwd:             cwd, Cmd: []string{"claude"}, Title: title, Cols: snap.Cols, Rows: snap.Rows,
		Provider:    snap.Provider,
		Effort:      snap.Effort,
		Model:       snap.Model,
		Role:        snap.Role,
		ContextPack: snap.ContextPack,
	})
	if err != nil {
		return nil, err
	}
	if a.panes != nil {
		if err := a.panes.Put(np); err != nil {
			a.rollbackSpawnedPane(np)
			return nil, err
		}
	}
	// Place the sibling immediately right of the degraded pane, same workspace.
	if a.ws != nil {
		if wsName := a.ws.NameForPane(p.ID); wsName != "" {
			ns := np.Snapshot()
			if _, err := a.ws.AddPane(wsName, PaneDef{
				ID: ns.ID, Cwd: ns.Cwd, Cmd: ns.Cmd, Title: ns.Title, Cols: ns.Cols, Rows: ns.Rows, Kind: ns.Kind, Agent: ns.Agent, CodexThreadID: ns.CodexThreadID, Provider: ns.Provider, Effort: ns.Effort, Model: ns.Model, Role: ns.Role, ContextPack: ns.ContextPack, NeverAutoAnswer: ns.NeverAutoAnswer,
			}, "right", p.ID); err != nil {
				a.rollbackSpawnedPane(np)
				return nil, err
			}
		}
	}
	return np, nil
}

// inplaceResult reports what a respawn did, for the caller's response/log.
type inplaceResult struct {
	resumed      bool
	archivedPath string
}

// doMigrateInplace replaces the pane's nested-child claude with a clean one in
// the SAME layout slot. It spawns the replacement under a NEW pane id (placed
// at the old pane's position) and only then retires the old pane — reusing the
// old id with a Kill-then-Spawn raced the old pane's async exit cleanup against
// the new pane, leaving it mis-marked exited and dropped from panes.json.
// Resume-when-possible: if the pane has a real transcript on disk it restarts
// `claude --resume <id>`; degraded panes (no transcript) start fresh. The
// visible scrollback is archived to ~/.solu/archives first.
func (a *apiServer) doMigrateInplace(p *Pane) (*Pane, inplaceResult, error) {
	if !p.tryAcquireRespawn() {
		return nil, inplaceResult{}, errMigrateInProgress
	}
	defer p.releaseRespawn()
	snap := p.Snapshot()
	cwd := snap.Cwd
	if live := snapshotPaneProcesses(p).liveCwd; live != "" {
		cwd = live
	}
	title := strings.TrimSpace(snap.Title)
	if title == "" {
		title = "claude"
	}
	// Resume target + scrollback archive, read off the live pane before we touch
	// anything. Degraded panes resolve to "" → fresh start.
	resumeSID := a.resumableSessionID(p)
	archived := archivePaneScrollback(p)
	cmd := []string{"claude"}
	if resumeSID != "" {
		cmd = []string{"claude", "--resume", resumeSID}
	}

	// Spawn the clean replacement (NEW id), place it at the old pane's slot, and
	// only then remove the old pane. Spawning before the kill also means a spawn
	// failure leaves the original pane untouched.
	np, err := a.spawnPaneForRequest(SpawnOpts{
		AutoMode:        snap.AutoMode,
		NeverAutoAnswer: snap.NeverAutoAnswer,
		Cwd:             cwd, Cmd: cmd, Title: title, Cols: snap.Cols, Rows: snap.Rows,
		ClaudeSessionID: resumeSID,
		Provider:        snap.Provider,
		Effort:          snap.Effort,
		Model:           snap.Model,
		Role:            snap.Role,
		ContextPack:     snap.ContextPack,
	})
	if err != nil {
		return nil, inplaceResult{}, err
	}
	if a.panes != nil {
		if perr := a.panes.Put(np); perr != nil {
			a.rollbackSpawnedPane(np)
			return nil, inplaceResult{}, perr
		}
	}
	if a.ws != nil {
		if ws := a.ws.NameForPane(p.ID); ws != "" {
			ns := np.Snapshot()
			if _, aerr := a.ws.ReplacePane(ws, p.ID, PaneDef{
				ID: ns.ID, Cwd: ns.Cwd, Cmd: ns.Cmd, Title: ns.Title, Cols: ns.Cols, Rows: ns.Rows, Kind: ns.Kind, Agent: ns.Agent, CodexThreadID: ns.CodexThreadID, Provider: ns.Provider, Effort: ns.Effort, Model: ns.Model, Role: ns.Role, ContextPack: ns.ContextPack, NeverAutoAnswer: ns.NeverAutoAnswer,
			}); aerr != nil {
				a.rollbackSpawnedPane(np)
				return nil, inplaceResult{}, aerr
			}
			if a.uiBus != nil {
				a.uiBus.publish(uiEvent{Type: "workspace.updated", Workspace: ws})
			}
		}
	}
	// A clean replacement ends the old pane's needs-user episode. Carry
	// durable manual/historical flags with their owner, but not an automatic
	// Warden flag bound to the retired pane's episode.
	if snap.Flagged && !(snap.FlagOwner == wardenScopeIdentity && snap.FlagEpisode != "" && snap.FlagRevision != "") {
		np.SetFlagAs(true, snap.FlagMemo, snap.FlagOwner)
	}
	if a.panes != nil {
		_ = a.panes.Put(np)
	}

	// Replacement is live in the layout — retire the old pane.
	_ = a.mgr.Kill(p.ID)
	if a.panes != nil {
		_ = a.panes.Remove(p.ID)
	}
	if a.ws != nil {
		_ = a.ws.RemovePane(p.ID)
	}
	log.Printf("migrate-inplace pane=%s -> %s resumed=%v archived=%q", p.ID, np.ID, resumeSID != "", archived)
	return np, inplaceResult{resumed: resumeSID != "", archivedPath: archived}, nil
}

// resumableSessionID returns the session id to `claude --resume` for this pane
// IF it has a real top-level transcript on disk, else "". Degraded
// (nested-child) panes have no transcript → "".
func (a *apiServer) resumableSessionID(p *Pane) string {
	path := a.transcriptForPane(p)
	if path == "" || isSubagentTranscript(path) {
		return ""
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return ""
	}
	return sessionIDForTranscript(path)
}

// archivePaneScrollback writes the pane's current scrollback (ANSI-stripped) to
// ~/.solu/archives so an in-place migration that can't resume still leaves a
// readable record. Best-effort: returns "" on any failure or empty buffer.
func archivePaneScrollback(p *Pane) string {
	body := stripANSI(p.buffer.Snapshot())
	if strings.TrimSpace(body) == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".solu", "archives")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	slug := slugifyTitle(p.Title)
	if slug == "" {
		slug = "pane"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.txt", slug, p.ID, time.Now().UTC().Unix()))
	var b strings.Builder
	b.WriteString("# solu pane scrollback archive (pre-migration)\n")
	b.WriteString("# title: " + p.Title + "\n# pane: " + p.ID + "\n# cwd: " + p.Cwd + "\n")
	b.WriteString("# archived: " + time.Now().UTC().Format(time.RFC3339) + "\n\n")
	b.WriteString(body)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return ""
	}
	return path
}

// ---- bulk: POST /api/panes/migrate-clean-all ----

type migrateAllReq struct {
	DryRun     *bool  `json:"dryRun"`     // default true — never mutates on first call
	Mode       string `json:"mode"`       // default "sibling"
	ConfirmAll bool   `json:"confirmAll"` // required for destructive "inplace"
	StaggerMs  int    `json:"staggerMs"`  // delay between spawns; default 1200
}

// migrateCleanAll handles POST /api/panes/migrate-clean-all: fan the per-pane
// migration over every degraded pane. dryRun (default) returns a preview; a
// real run executes in the background, staggered, and the client watches
// /api/panes (degraded flags clear, clean siblings appear) for progress.
func (a *apiServer) migrateCleanAll(w http.ResponseWriter, r *http.Request) {
	var req migrateAllReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "sibling"
	}
	if mode != "sibling" && mode != "inplace" {
		http.Error(w, "unknown mode (want sibling|inplace): "+mode, 400)
		return
	}

	// Collect degraded panes off one process-tree snapshot.
	tree := snapshotProcessTree()
	type row struct {
		PaneID      string `json:"paneId"`
		Title       string `json:"title"`
		Destructive bool   `json:"destructive"`
		Reason      string `json:"degradedReason"`
	}
	var affected []row
	var paneIDs []string
	for _, p := range a.mgr.List() {
		proc := snapshotPaneProcessesFromTree(p, tree)
		if proc.degraded.unknown || !proc.degraded.degraded {
			continue
		}
		snap := p.Snapshot()
		affected = append(affected, row{
			PaneID: snap.ID, Title: snap.Title,
			Destructive: mode == "inplace" && proc.hasClaude,
			Reason:      proc.degraded.reason,
		})
		paneIDs = append(paneIDs, snap.ID)
	}

	if dryRun {
		writeJSON(w, 200, map[string]any{
			"dryRun": true, "mode": mode, "affected": affected,
			"summary": map[string]any{"total": len(affected)},
		})
		return
	}
	if mode == "inplace" && !req.ConfirmAll {
		writeJSON(w, 409, map[string]any{
			"needsConfirm": true, "mode": "inplace", "affected": affected,
			"hint": "inplace kills each pane's live (unsaved) claude — set confirmAll:true, or use mode:sibling (non-destructive)",
		})
		return
	}
	if len(paneIDs) == 0 {
		writeJSON(w, 200, map[string]any{"status": "noop", "count": 0})
		return
	}

	stagger := time.Duration(req.StaggerMs) * time.Millisecond
	if req.StaggerMs <= 0 {
		stagger = 1200 * time.Millisecond
	}
	// Run in the background so the request doesn't hang for N*stagger; the
	// client polls /api/panes to watch siblings appear and degraded clear.
	go a.runMigrateAll(paneIDs, mode, stagger)

	writeJSON(w, 202, map[string]any{
		"status": "started", "mode": mode, "count": len(paneIDs),
	})
}

func (a *apiServer) runMigrateAll(paneIDs []string, mode string, stagger time.Duration) {
	ok, fail := 0, 0
	for i, id := range paneIDs {
		p, exists := a.mgr.Get(id)
		if !exists {
			continue
		}
		var err error
		if mode == "inplace" {
			_, _, err = a.doMigrateInplace(p)
		} else {
			_, err = a.doMigrateSibling(p)
		}
		if err != nil {
			fail++
			log.Printf("migrate-clean-all: pane=%s mode=%s failed: %v", id, mode, err)
		} else {
			ok++
		}
		if i < len(paneIDs)-1 {
			time.Sleep(stagger) // avoid a thundering herd of claude launches
		}
	}
	log.Printf("migrate-clean-all: done mode=%s ok=%d failed=%d", mode, ok, fail)
}
