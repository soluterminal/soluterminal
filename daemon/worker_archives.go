package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// An archive is independent of the ten-entry recently-closed ring. It retains
// a provider-resumable snapshot and a private transcript copy until its TTL.
type workerArchiveEntry struct {
	ID         string       `json:"id"`
	Snap       PaneSnapshot `json:"snap"`
	Workspace  string       `json:"workspace"`
	Reply      string       `json:"reply"`
	ArchivedAt int64        `json:"archivedAt"`
	Pinned     bool         `json:"pinned"`
	Ready      bool         `json:"ready"`
}
type workerReadReceipt struct {
	Pane  *Pane
	Turn  string
	Input uint64
	At    time.Time
}
type workerArchiveState struct {
	mu           sync.Mutex
	operation    sync.Mutex
	loaded       bool
	err          error
	entries      []workerArchiveEntry
	reads        map[string]workerReadReceipt
	checked      map[string]time.Time
	flagged      map[string]string // pane id → the retention already flagged to its lead
	beforeCommit func()            // test-only race seam; nil in production
	teardown     func(*Pane) error // test-only failure seam; nil in production
	context      context.Context
}

var workerArchiveSlot extSlot[workerArchiveState]

func (a *apiServer) workerArchives() *workerArchiveState { return workerArchiveSlot.ensure(&a.ext) }
func (a *apiServer) archiveRoot() string                 { return filepath.Join(a.dataDir, "worker-archives") }
func archiveIDValid(id string) bool {
	if len(id) != 21 || !strings.HasPrefix(id, "pane_") {
		return false
	}
	_, err := hex.DecodeString(id[5:])
	return err == nil
}
func (a *apiServer) loadWorkerArchivesLocked(s *workerArchiveState) {
	if s.loaded {
		return
	}
	s.loaded = true
	s.reads = map[string]workerReadReceipt{}
	s.checked = map[string]time.Time{}
	s.flagged = map[string]string{}
	if a.dataDir == "" {
		s.err = errors.New("archive storage unavailable")
		return
	}
	raw, err := os.ReadFile(filepath.Join(a.archiveRoot(), "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.err = err
		return
	}
	if err = json.Unmarshal(raw, &s.entries); err != nil {
		s.err = err
		return
	}
	for _, e := range s.entries {
		if !archiveIDValid(e.ID) {
			s.err = errors.New("invalid worker archive index")
			return
		}
	}
}
func (a *apiServer) saveWorkerArchivesLocked(s *workerArchiveState, next []workerArchiveEntry) error {
	if s.err != nil {
		return s.err
	}
	if err := os.MkdirAll(a.archiveRoot(), 0700); err != nil {
		return err
	}
	if err := syncDir(a.dataDir); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err = writeFileAtomicSynced(filepath.Join(a.archiveRoot(), "index.json"), raw, 0600); err != nil {
		return err
	}
	s.entries = next
	return nil
}
func workerReplyKey(reply paneReply) string {
	if reply.Source != "transcript" || !reply.Complete || reply.CompletedAt.IsZero() || strings.TrimSpace(reply.Reply) == "" {
		return ""
	}
	raw := reply.Path + "\x00" + reply.PromptAt.UTC().Format(time.RFC3339Nano) + "\x00" + reply.CompletedAt.UTC().Format(time.RFC3339Nano) + "\x00" + reply.Reply
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
func (a *apiServer) noteWorkerReplyRead(caller string, p *Pane, reply paneReply) {
	snap := p.Snapshot()
	// The lead reading the result: the worker's starter when that pane leads
	// workers, or the lead recorded at spawn (pane_team.go workerLeadFor).
	if snap.Origin == nil || snap.Origin.Worker == nil || caller == "" || (caller != snap.Origin.LeadPaneID && caller != a.workerLeadFor(snap.Origin)) {
		return
	}
	key := workerReplyKey(reply)
	if key == "" || (atomic.LoadInt64(&p.LastInputMs) > 0 && atomic.LoadInt64(&p.LastInputMs) >= reply.CompletedAt.UnixMilli()) {
		return
	}
	s := a.workerArchives()
	s.mu.Lock()
	defer s.mu.Unlock()
	a.loadWorkerArchivesLocked(s)
	s.reads[p.ID] = workerReadReceipt{Pane: p, Turn: key, Input: p.inputRevision.Load(), At: time.Now()}
}

type workerGitProof struct{ Head, Upstream string }

func workerGitCleanPushed(ctx context.Context, cwd string) (workerGitProof, string) {
	// Do not inherit injected git config, alternate indexes or external diff.
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	options := []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "core.untrackedCache=false"}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, options...), args...)...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	heads, err := run("rev-parse", "HEAD", "@{upstream}")
	parts := strings.Fields(heads)
	if err != nil || len(parts) != 2 {
		return workerGitProof{}, "work has no pushed upstream"
	}
	paths, err := run("rev-parse", "--path-format=absolute", "--git-path", "objects", "--git-path", "index", "--show-toplevel")
	locations := strings.Split(paths, "\n")
	if err != nil || len(locations) != 3 {
		return workerGitProof{}, "worktree paths cannot be verified"
	}
	index, err := run("ls-files", "--stage")
	if err != nil {
		return workerGitProof{}, "worktree index cannot be verified"
	}
	for _, line := range strings.Split(index, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return workerGitProof{}, "submodule work requires manual review"
		}
	}
	// Status may invoke clean/process filters. Use a private Git directory
	// with no driver definitions, even if the worker concurrently edits its
	// config or attributes. Metadata-only commands above/below never run drivers.
	// Local info/exclude rules are deliberately omitted (conservative retention).
	view, err := os.MkdirTemp("", "solu-worker-git-*")
	if err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	defer os.RemoveAll(view)
	for _, dir := range []string{"objects", "refs"} {
		if err := os.Mkdir(filepath.Join(view, dir), 0700); err != nil {
			return workerGitProof{}, "worktree status cannot be verified"
		}
	}
	if err := os.WriteFile(filepath.Join(view, "HEAD"), []byte(parts[0]+"\n"), 0600); err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	cmd := exec.CommandContext(ctx, "git", append(options, "--git-dir="+view, "--work-tree="+locations[2], "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=all")...)
	cmd.Dir = locations[2]
	cmd.Env = append(env, "GIT_OBJECT_DIRECTORY="+locations[0], "GIT_INDEX_FILE="+locations[1])
	out, err := cmd.Output()
	status := strings.TrimSpace(string(out))
	if err != nil {
		return workerGitProof{}, "worktree status cannot be verified"
	}
	if status != "" {
		return workerGitProof{}, "uncommitted work"
	}
	ahead, err := run("rev-list", "--count", "@{upstream}..HEAD")
	if err != nil || ahead != "0" {
		return workerGitProof{}, "unpushed work"
	}
	return workerGitProof{parts[0], parts[1]}, ""
}
func workerArchiveAllowed(finished, final, read, noInput, cleanPushed bool) (bool, bool) {
	if !finished || !final || !noInput {
		return false, false
	}
	return read && cleanPushed, !read || !cleanPushed
}
func (a *apiServer) flagWorkerLead(lead, worker, reason string) {
	p, ok := a.mgr.Get(lead)
	if !ok {
		return
	}
	memo := clipRunes("Worker "+worker+" retained: "+reason, flagMemoMaxRunes)
	_ = a.mgr.changeCurrentFlag(p, a.panes, func(prev PaneSnapshot) error {
		if prev.Flagged && (prev.FlagOwner != "worker-manager" || prev.FlagMemo == memo) {
			return errors.New("preserve existing flag")
		}
		return nil
	}, true, memo, "worker-manager", "")
}
func (a *apiServer) archiveFinishedWorkers(now time.Time) {
	q := a.workers()
	q.mu.Lock()
	enabled := q.settings.WorkerAutoArchive
	var ids []string
	if enabled {
		for _, r := range q.requests {
			if r.Info.State == "finished" && r.PaneID != "" {
				ids = append(ids, r.PaneID)
			}
		}
	}
	q.mu.Unlock()
	s := a.workerArchives()
	if !s.operation.TryLock() {
		return
	}
	go func() {
		defer s.operation.Unlock()
		a.expireWorkerArchivesLocked(now)
		if !enabled {
			return
		}
		for _, id := range ids {
			s.mu.Lock()
			a.loadWorkerArchivesLocked(s)
			if s.context != nil && s.context.Err() != nil {
				s.mu.Unlock()
				return
			}
			due := now.Sub(s.checked[id]) >= 30*time.Second
			if due {
				s.checked[id] = now
			}
			s.mu.Unlock()
			if !due {
				continue
			}
			if p, ok := a.mgr.Get(id); ok {
				a.tryArchiveWorker(p, now)
			}
			break
		}
	}()
}
func (a *apiServer) tryArchiveWorker(p *Pane, now time.Time) {
	snap := p.Snapshot()
	// Only workers are archived: never a lead or the user's own pane.
	if a.ws == nil || a.panes == nil || snap.Origin == nil || snap.Origin.Worker == nil || snap.Origin.Worker.State != "finished" || paneTeamKind(snap.Origin) != paneTeamWorker {
		return
	}
	reply := a.paneLatestReply(p)
	key := workerReplyKey(reply)
	q := a.workers()
	q.mu.Lock()
	a.loadWorkerSettingsLocked(q)
	enabled := q.settings.WorkerAutoArchive
	started := q.startedAt
	q.mu.Unlock()
	// Restored panes need a newly observed final turn. Input before a daemon
	// restart cannot be reconstructed from its ephemeral writer counter.
	final := key != "" && !reply.CompletedAt.Before(started) && !reply.CompletedAt.After(now)
	noInput := atomic.LoadInt64(&p.LastInputMs) == 0 || atomic.LoadInt64(&p.LastInputMs) < reply.CompletedAt.UnixMilli()
	if !enabled || !final || !noInput || p.computeStateCached() != StateIdle {
		return
	}
	s := a.workerArchives()
	s.mu.Lock()
	a.loadWorkerArchivesLocked(s)
	receipt := s.reads[p.ID]
	storageErr := s.err
	baseContext := s.context
	pending := false
	for _, e := range s.entries {
		if e.Snap.ID == p.ID {
			pending = true
			break
		}
	}
	s.mu.Unlock()
	if storageErr != nil || pending {
		return
	}
	if baseContext == nil {
		baseContext = context.Background()
	}
	if baseContext.Err() != nil {
		return
	}
	read := receipt.Pane == p && receipt.Turn == key && receipt.Input == p.inputRevision.Load() && !receipt.At.Before(reply.CompletedAt)
	cwd := snap.Cwd // pane's assigned worktree, not a task subprocess's temporary cwd
	ctx, cancel := context.WithTimeout(baseContext, 4*time.Second)
	proof, reason := workerGitCleanPushed(ctx, cwd)
	cancel()
	allowed, flag := workerArchiveAllowed(true, final, read, noInput, reason == "")
	if !allowed {
		if flag {
			if !read {
				if reason != "" {
					reason += "; "
				}
				reason += "final reply unread by lead"
			}
			// Tell the lead once per final reply and reason: a person who
			// clears the flag isn't flagged again until something changes.
			lead := a.workerLeadFor(snap.Origin)
			note := lead + "\x00" + key + "\x00" + reason
			s.mu.Lock()
			again := s.flagged[p.ID] == note
			s.flagged[p.ID] = note
			s.mu.Unlock()
			if !again {
				a.flagWorkerLead(lead, snap.ID, reason)
			}
		}
		return
	}
	workspace := ""
	if a.ws != nil {
		workspace = a.ws.NameForPane(p.ID)
	}
	if workspace == "" {
		workspace = snap.Origin.Workspace
	}
	archiveID, err := randomID()
	if err != nil {
		return
	}
	dir := filepath.Join(a.archiveRoot(), archiveID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	source, _, err := openUserFile(reply.Path, 128<<20)
	if err != nil {
		return
	}
	defer source.Close()
	dest, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	n, copyErr := io.Copy(dest, io.LimitReader(source, 128<<20+1))
	syncErr := dest.Sync()
	closeErr := dest.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || n > 128<<20 || syncDir(dir) != nil {
		return
	}
	// Commit under the same writer lock as terminal/native/conditional input.
	// Once committed, all input paths refuse the old process; reopening has a new id.
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	var entry workerArchiveEntry
	committed := false
	a.mgr.withCurrentPane(p, func() {
		p.agentWriteMu.Lock()
		defer p.agentWriteMu.Unlock()
		if baseContext.Err() != nil || p.workerArchiving.Load() || p.inputRevision.Load() != receipt.Input || (atomic.LoadInt64(&p.LastInputMs) > 0 && atomic.LoadInt64(&p.LastInputMs) >= reply.CompletedAt.UnixMilli()) {
			return
		}
		latest := workerReplyFromFile(reply.Path, reply.Agent)
		if workerReplyKey(latest) != key || p.computeStateCached() != StateIdle {
			return
		}
		if !q.archiveEnabled.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(baseContext, 4*time.Second)
		fresh, why := workerGitCleanPushed(ctx, cwd)
		cancel()
		if why != "" || fresh != proof {
			return
		}
		current := p.Snapshot()
		if current.Origin == nil || current.Origin.Worker == nil || current.Origin.Worker.State != "finished" || paneTeamKind(current.Origin) != paneTeamWorker {
			return
		}
		q.archivePolicyMu.RLock()
		defer q.archivePolicyMu.RUnlock()
		if baseContext.Err() != nil || !q.archiveEnabled.Load() {
			return
		}
		w := *current.Origin.Worker
		w.State = "archived"
		w.ArchiveID = archiveID
		w.ArchivedAt = now.UnixMilli()
		current.Origin = current.Origin.clone()
		current.Origin.Worker = &w
		entry = workerArchiveEntry{ID: archiveID, Snap: current, Workspace: workspace, Reply: reply.Reply, ArchivedAt: now.UnixMilli(), Pinned: w.Pinned}
		s.mu.Lock()
		err = a.saveWorkerArchivesLocked(s, append(append([]workerArchiveEntry(nil), s.entries...), entry))
		s.mu.Unlock()
		if err != nil {
			return
		}
		p.workerArchiving.Store(true)
		committed = true
		keep = true
	})
	if !committed {
		return
	}
	if a.ws == nil {
		p.workerArchiving.Store(false)
		return
	}
	// RemovePane creates the server-owned tombstone before the process exits.
	if err = a.ws.RemovePane(p.ID); err != nil {
		if a.discardWorkerArchive(archiveID) == nil {
			keep = false
		}
		p.workerArchiving.Store(false)
		a.flagWorkerLead(a.workerLeadFor(snap.Origin), p.ID, "archive layout persistence failed")
		return
	}
	teardown := a.detachArchivedWorker
	if s.teardown != nil {
		teardown = s.teardown
	}
	if err = teardown(p); err != nil {
		p.workerArchiving.Store(false)
		restored := false
		a.mgr.withCurrentPane(p, func() { restored = a.ws.restoreWorkerArchivePane(entry.Workspace, entry.Snap) == nil })
		if restored && a.discardWorkerArchive(archiveID) == nil {
			keep = false
		}
		a.flagWorkerLead(a.workerLeadFor(snap.Origin), p.ID, "archive process shutdown failed")
		return
	}
	if a.panes != nil {
		if err = a.panes.Remove(p.ID); err != nil {
			a.flagWorkerLead(a.workerLeadFor(snap.Origin), p.ID, "archive pane-store cleanup failed")
			return
		}
	}
	s.mu.Lock()
	next := append([]workerArchiveEntry(nil), s.entries...)
	for i := range next {
		if next[i].ID == archiveID {
			next[i].Ready = true
		}
	}
	err = a.saveWorkerArchivesLocked(s, next)
	s.mu.Unlock()
	if err != nil {
		return
	}
	var release func()
	q.mu.Lock()
	for _, r := range q.requests {
		if r.PaneID == p.ID {
			r.Info = *entry.Snap.Origin.Worker
			release = r.release
			r.release = nil
			break
		}
	}
	q.mu.Unlock()
	if release != nil {
		release()
	}

	if a.uiBus != nil {
		a.uiBus.publish(uiEvent{Type: "pane.archived", PaneID: p.ID, Workspace: entry.Workspace, Text: "Worker archived; reopen from its lead"})
	}
}
func (a *apiServer) detachArchivedWorker(p *Pane) error {
	// A process-group signal cannot reach a setsid child after reparenting.
	// Kill uses the persisted scope to stop *all* descendants and waits for
	// kernel emptiness. Legacy panes lack that proof; retain their lease/layout.
	p.mu.Lock()
	legacy := p.Pid > 0 && p.processScope == nil
	p.mu.Unlock()
	if legacy {
		return errors.New("legacy worker has no process scope; reopen it before automatic archiving")
	}
	// Remove this exact incarnation before teardown so a concurrent respawn can
	// never make Kill(id) target a replacement process.
	a.mgr.mu.Lock()
	if a.mgr.panes[p.ID] != p {
		a.mgr.mu.Unlock()
		return errors.New("worker changed")
	}
	delete(a.mgr.panes, p.ID)
	removed := a.mgr.onPaneRemoved
	a.mgr.mu.Unlock()
	err := p.Kill()
	if err == nil {
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			err = errors.New("worker shutdown did not settle")
		}
	}
	if err != nil {
		a.mgr.mu.Lock()
		if _, exists := a.mgr.panes[p.ID]; !exists {
			a.mgr.panes[p.ID] = p
		}
		a.mgr.mu.Unlock()
		return err
	}
	p.detachSubscribers()
	if removed != nil {
		removed(p.ID)
	}
	return nil
}
func (a *apiServer) expireWorkerArchives(now time.Time) {
	s := a.workerArchives()
	if !s.operation.TryLock() {
		return
	}
	defer s.operation.Unlock()
	a.expireWorkerArchivesLocked(now)
}

// The operation lease serializes reconciliation with teardown/compensation.
func (a *apiServer) expireWorkerArchivesLocked(now time.Time) {
	s := a.workerArchives()
	live := map[string]bool{}
	for _, p := range a.mgr.List() {
		live[p.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a.loadWorkerArchivesLocked(s)
	if s.err != nil {
		return
	}
	next := make([]workerArchiveEntry, 0, len(s.entries))
	changed := false
	for _, e := range s.entries {
		if !e.Ready && !live[e.Snap.ID] {
			if a.panes == nil || a.panes.Remove(e.Snap.ID) == nil {
				e.Ready = true
				changed = true
			}
		}
		if !e.Ready || e.Pinned || now.UnixMilli()-e.ArchivedAt < int64((24*time.Hour)/time.Millisecond) {
			next = append(next, e)
			continue
		}
		if err := os.RemoveAll(filepath.Join(a.archiveRoot(), e.ID)); err != nil {
			next = append(next, e)
			continue
		}
		changed = true
	}
	if changed {
		_ = a.saveWorkerArchivesLocked(s, next)
	}
}
func (a *apiServer) archivedWorkerSummaries() []mcpPaneSummary {
	a.expireWorkerArchives(time.Now())
	s := a.workerArchives()
	s.mu.Lock()
	a.loadWorkerArchivesLocked(s)
	entries := append([]workerArchiveEntry(nil), s.entries...)
	s.mu.Unlock()
	out := []mcpPaneSummary{}
	for _, e := range entries {
		if !e.Ready {
			continue
		}
		w := visibleWorkerInfo(originWorker(e.Snap.Origin))
		if w == nil {
			continue
		}
		w.Pinned = e.Pinned
		origin := visibleWorkerOrigin(a.originAliases.resolveOrigin(e.Snap.Origin))
		if origin != nil && origin.Worker != nil {
			origin.Worker.Pinned = e.Pinned
		}
		out = append(out, mcpPaneSummary{ID: e.Snap.ID, Title: e.Snap.Title, Status: "archived", Cwd: e.Snap.Cwd, Kind: "terminal", Agent: e.Snap.Agent, Workspace: e.Workspace, Origin: origin, Worker: w})
	}
	return out
}
func (a *apiServer) archivedWorkerReply(id string) (string, bool) {
	a.expireWorkerArchives(time.Now())
	s := a.workerArchives()
	s.mu.Lock()
	defer s.mu.Unlock()
	a.loadWorkerArchivesLocked(s)
	for _, e := range s.entries {
		if e.Snap.ID == id || e.ID == id {
			return redactTerminalTail(e.Reply), true
		}
	}
	return "", false
}
func (a *apiServer) reopenWorkerArchive(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "reopen an archived worker") {
		return
	}
	s := a.workerArchives()
	s.operation.Lock()
	defer s.operation.Unlock()
	a.expireWorkerArchivesLocked(time.Now())
	s.mu.Lock()
	a.loadWorkerArchivesLocked(s)
	var entry workerArchiveEntry
	for _, e := range s.entries {
		if e.ID == r.PathValue("id") {
			entry = e
			break
		}
	}
	s.mu.Unlock()
	if entry.ID == "" {
		http.Error(w, "archive not found", 404)
		return
	}
	if _, live := a.mgr.Get(entry.Snap.ID); live {
		http.Error(w, "worker is still live; inspect its existing pane", 409)
		return
	}
	lead := ""
	if o := a.originAliases.resolveOrigin(entry.Snap.Origin); o != nil {
		lead = o.LeadPaneID
	}
	workspace, err := a.resolveMCPSpawnWorkspace(entry.Workspace, lead)
	if err != nil {
		workspace, err = a.resolveMCPSpawnWorkspace("", lead)
	}
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	opts := reopenSpawnOpts(closedPaneEntry{Snap: entry.Snap}, 0, 0)
	opts.Origin = a.originAliases.resolveOrigin(entry.Snap.Origin.clone())
	opts.Title = entry.Snap.Title
	if opts.Origin != nil && opts.Origin.Worker != nil {
		opts.Origin.Worker.State = "running"
		opts.Origin.Worker.ArchiveID = ""
		opts.Origin.Worker.ArchivedAt = 0
		opts.Origin.Worker.Pinned = entry.Pinned
	}
	p, err := a.spawnPaneForRequest(opts)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if a.panes != nil {
		if err = a.panes.Put(p); err != nil {
			a.rollbackSpawnedPane(p)
			http.Error(w, "persist reopened worker", 500)
			return
		}
	}
	snap := p.Snapshot()
	def := PaneDef{ID: snap.ID, Cwd: snap.Cwd, Cmd: snap.Cmd, Title: snap.Title, Agent: snap.Agent, Kind: snap.Kind, ClaudeSessionID: snap.ClaudeSessionID, CodexThreadID: snap.CodexThreadID, Origin: snap.Origin.clone(), NeverAutoAnswer: snap.NeverAutoAnswer}
	if a.ws == nil {
		a.rollbackSpawnedPane(p)
		http.Error(w, "workspace unavailable", 500)
		return
	}
	if _, err = a.ws.AddPane(workspace, def, "horizontal", lead); err != nil {
		a.rollbackSpawnedPane(p)
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Lock()
	next := []workerArchiveEntry{}
	for _, e := range s.entries {
		if e.ID != entry.ID {
			next = append(next, e)
		}
	}
	err = a.saveWorkerArchivesLocked(s, next)
	s.mu.Unlock()
	if err != nil {
		a.rollbackSpawnedPane(p)
		http.Error(w, "persist archive reopen", 500)
		return
	}
	_ = os.RemoveAll(filepath.Join(a.archiveRoot(), entry.ID))
	writeJSON(w, 201, makePaneDTO(p, a.aria, a.attention))
}
func init() { registerAPIRoutes((*apiServer).registerWorkerArchiveRoutes) }
func (a *apiServer) registerWorkerArchiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/worker-archives", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.archivedWorkerSummaries()) })
	mux.HandleFunc("POST /api/worker-archives/{id}/reopen", a.reopenWorkerArchive)
	mux.HandleFunc("PATCH /api/worker-archives/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !a.requireHuman(w, r, "change worker archive retention") {
			return
		}
		var body struct {
			Pinned *bool `json:"pinned"`
		}
		if !decodeJSON(w, r, &body, 1024) {
			return
		}
		if body.Pinned == nil {
			http.Error(w, "pinned is required", 400)
			return
		}
		s := a.workerArchives()
		s.mu.Lock()
		defer s.mu.Unlock()
		a.loadWorkerArchivesLocked(s)
		next := append([]workerArchiveEntry(nil), s.entries...)
		for i := range next {
			if next[i].ID == r.PathValue("id") {
				next[i].Pinned = *body.Pinned
				if err := a.saveWorkerArchivesLocked(s, next); err != nil {
					http.Error(w, "persist archive pin", 500)
					return
				}
				writeJSON(w, 200, map[string]any{"pinned": *body.Pinned})
				return
			}
		}
		http.Error(w, "archive not found", 404)
	})
}

func workerReplyFromFile(path, agent string) paneReply {
	raw, err := tailReadBounded(path, paneReplyTailBytes)
	if err != nil {
		return paneReply{}
	}
	var reply paneReply
	if agent == agentClaude {
		reply = claudeLatestReply(raw)
	} else if agent == agentCodex {
		reply = codexLatestReply(raw)
	}
	reply.Path = path
	reply.Reply = redactTerminalTail(reply.Reply)
	return reply
}

// Only compensation for a failed archive of the same still-live pane may
// clear this tombstone. Successful archives always reopen under a new id.
func (s *WorkspaceStore) restoreWorkerArchivePane(name string, snap PaneSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.data[name]
	if !ok {
		return errors.New("workspace unavailable")
	}
	next := s.candidate()
	w := cloneWorkspace(current)
	w.RemovedPaneIDs = removePaneID(w.RemovedPaneIDs, snap.ID)
	present := false
	for _, id := range w.PaneIds {
		if id == snap.ID {
			present = true
		}
	}
	if !present {
		w.PaneIds = append(w.PaneIds, snap.ID)
		w.Panes = appendPaneDef(w.Panes, PaneDef{ID: snap.ID, Cwd: snap.Cwd, Cmd: snap.Cmd, Title: snap.Title, Agent: snap.Agent, Kind: snap.Kind, Origin: snap.Origin.clone(), ClaudeSessionID: snap.ClaudeSessionID, CodexThreadID: snap.CodexThreadID, NeverAutoAnswer: snap.NeverAutoAnswer})
		w.Layout = insertPaneIntoLayout(w.Layout, snap.ID, "horizontal", "")
	}
	w.ActiveAt = time.Now().UTC()
	next[name] = w
	return s.commit(next)
}
func (a *apiServer) discardWorkerArchive(id string) error {
	s := a.workerArchives()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := []workerArchiveEntry{}
	for _, e := range s.entries {
		if e.ID != id {
			next = append(next, e)
		}
	}
	return a.saveWorkerArchivesLocked(s, next)
}
