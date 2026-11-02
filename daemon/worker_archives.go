package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
type workerRetention struct{ Lead, Reason string }

type workerArchiveState struct {
	noticeMu         sync.Mutex
	retained         map[string]workerRetention
	noticesLoaded    bool
	mu               sync.Mutex
	operation        sync.Mutex
	loaded           bool
	err              error
	entries          []workerArchiveEntry
	reads            map[string]workerReadReceipt
	checked          map[string]time.Time
	flagged          map[string]string // pane id → the retention already flagged to its lead
	beforeCommit     func()            // test-only race seam; nil in production
	beforeGitRecheck func()            // test-only slow-proof seam; nil in production
	teardown         func(*Pane) error // test-only failure seam; nil in production
	context          context.Context
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

func workerArchiveAllowed(finished, final, read, noInput, cleanPushed bool) (bool, bool) {
	if !finished || !final || !noInput {
		return false, false
	}
	return read && cleanPushed, !read || !cleanPushed
}

// Retentions survive restart, including entries hidden by the bounded memo.
// Existing single-worker notices seed the metadata on first use.
func (a *apiServer) loadWorkerRetentionsLocked(s *workerArchiveState) {
	if s.noticesLoaded {
		return
	}
	s.noticesLoaded = true
	s.retained = map[string]workerRetention{}
	if a.dataDir != "" {
		if raw, err := os.ReadFile(filepath.Join(a.dataDir, "worker-retentions.json")); err == nil {
			if err := json.Unmarshal(raw, &s.retained); err != nil {
				log.Printf("worker retention load: %v", err)
			}
		}
	}
	if s.retained == nil {
		s.retained = map[string]workerRetention{}
	}
	for _, p := range a.mgr.List() {
		snap := p.Snapshot()
		if !snap.Flagged || snap.FlagOwner != "worker-manager" {
			continue
		}
		for _, line := range strings.Split(snap.FlagMemo, "\n") {
			line, ok := strings.CutPrefix(line, "Worker ")
			if !ok {
				continue
			}
			worker, reason, ok := strings.Cut(line, " retained: ")
			if ok && worker != "" {
				if _, exists := s.retained[worker]; !exists {
					s.retained[worker] = workerRetention{snap.ID, reason}
				}
			}
		}
	}
}
func (a *apiServer) retainedWorker(id string) (workerRetention, bool) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	note, ok := s.retained[id]
	return note, ok
}

func (a *apiServer) saveWorkerRetentionsLocked(s *workerArchiveState) {
	if a.dataDir == "" {
		return
	}
	raw, err := json.Marshal(s.retained)
	if err == nil {
		err = writeFileAtomicSynced(filepath.Join(a.dataDir, "worker-retentions.json"), raw, 0600)
	}
	if err != nil {
		log.Printf("worker retention save: %v", err)
	}
}

// noticeMu serializes aggregate memo updates, independently of archive/storage
// locks. The flag transaction still checks ownership against concurrent UI writes.
func (a *apiServer) flagWorkerLead(lead, worker, reason string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	old := s.retained[worker]
	s.retained[worker] = workerRetention{lead, reason}
	a.saveWorkerRetentionsLocked(s)
	if old.Lead != "" && old.Lead != lead {
		a.syncWorkerLeadFlagLocked(s, old.Lead, false)
	}
	a.syncWorkerLeadFlagLocked(s, lead, true)
}
func (a *apiServer) syncWorkerLeadFlagLocked(s *workerArchiveState, lead string, create bool) {
	p, ok := a.mgr.Get(lead)
	if !ok {
		return
	}
	var ids []string
	for id, note := range s.retained {
		if note.Lead == lead {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var lines []string
	for i, id := range ids {
		if i == 4 {
			lines = append(lines, fmt.Sprintf("+%d more retained workers", len(ids)-i))
			break
		}
		lines = append(lines, "Worker "+id+" retained: "+clipRunes(oneLine(s.retained[id].Reason), 70))
	}
	memo := strings.Join(lines, "\n")
	_ = a.mgr.changeCurrentFlag(p, a.panes, func(prev PaneSnapshot) error {
		if (prev.Flagged && prev.FlagOwner != "worker-manager") || (!prev.Flagged && !create) || (prev.Flagged && prev.FlagMemo == memo) {
			return errors.New("preserve existing flag")
		}
		return nil
	}, len(ids) > 0, memo, "worker-manager")
}
func (a *apiServer) clearWorkerRetention(worker string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	s.mu.Lock()
	delete(s.flagged, worker)
	s.mu.Unlock()
	if note, ok := s.retained[worker]; ok {
		delete(s.retained, worker)
		a.saveWorkerRetentionsLocked(s)
		a.syncWorkerLeadFlagLocked(s, note.Lead, false)
	}
}
func (a *apiServer) reconcileWorkerRetentions(enabled bool) {
	s := a.workerArchives()
	// A detached worker with an unfinished durable archive still has pending
	// cleanup. expireWorkerArchivesLocked retries that cleanup independently
	// of auto-archive policy; keep its notice until the retry is persisted.
	s.mu.Lock()
	a.loadWorkerArchivesLocked(s)
	pending := map[string]bool{}
	for _, entry := range s.entries {
		if !entry.Ready {
			pending[entry.Snap.ID] = true
		}
	}
	s.mu.Unlock()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	changed := false
	leads := map[string]bool{}
	for _, p := range a.mgr.List() {
		snap := p.Snapshot()
		if snap.Flagged && snap.FlagOwner == "worker-manager" {
			leads[p.ID] = true
		}
	}
	for id, note := range s.retained {
		leads[note.Lead] = true
		p, ok := a.mgr.Get(id)
		if !ok {
			if pending[id] {
				continue
			}
			delete(s.retained, id)
			changed = true
			s.mu.Lock()
			delete(s.flagged, id)
			s.mu.Unlock()
			continue
		}
		snap := p.Snapshot()
		if !enabled || paneTeamKind(snap.Origin) != paneTeamWorker || snap.Origin.Worker == nil || snap.Origin.Worker.State != "finished" {
			delete(s.retained, id)
			changed = true
			s.mu.Lock()
			delete(s.flagged, id)
			s.mu.Unlock()
		}
	}
	if changed {
		a.saveWorkerRetentionsLocked(s)
	}
	for lead := range leads {
		a.syncWorkerLeadFlagLocked(s, lead, false)
	}
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
		a.reconcileWorkerRetentions(enabled)
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
		a.clearWorkerRetention(p.ID)
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
	if !enabled || key == "" || !noInput {
		a.clearWorkerRetention(p.ID)
		return
	}
	// Transient non-idle observations do not mean retained work vanished.
	if p.computeStateCached() != StateIdle {
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
	live := liveCwdOfPane(p)
	ctx, cancel := context.WithTimeout(baseContext, 4*time.Second)
	proof, reason := workerGitForArchive(ctx, snap, live)
	cancel()
	if !final {
		// Losing ephemeral read receipts at restart is not evidence of a new
		// unread result. Preserve known notices, but do not re-flag a silent or
		// dismissed clean worker merely because its old final cannot archive.
		prior, known := a.retainedWorker(p.ID)
		if reason == "" {
			if read || (known && !strings.Contains(prior.Reason, "final reply unread by lead") && !strings.HasPrefix(prior.Reason, "archive ")) {
				a.clearWorkerRetention(p.ID)
			}
			return
		}
		if known && prior.Lead == a.workerLeadFor(snap.Origin) && (prior.Reason == reason || (!read && prior.Reason == reason+"; final reply unread by lead")) {
			return
		}
		a.flagWorkerLead(a.workerLeadFor(snap.Origin), p.ID, reason)
		return
	}
	allowed, flag := workerArchiveAllowed(true, key != "", read, noInput, reason == "")
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
	// Finish slow Git verification before taking manager/input locks. Input
	// arriving during the proof changes the revision and rejects commit below.
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if s.beforeGitRecheck != nil {
		s.beforeGitRecheck()
	}
	ctx, cancel = context.WithTimeout(baseContext, 4*time.Second)
	fresh, why := workerGitForArchive(ctx, p.Snapshot(), liveCwdOfPane(p))
	cancel()
	if why != "" || fresh != proof {
		return
	}
	var entry workerArchiveEntry
	committed := false
	// Once committed under the writer fence, input refuses the old process;
	// reopening has a new ID. Recheck identity/input after the slow proof.
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
	a.asksPaneClosed(p.ID)
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

	a.clearWorkerRetention(p.ID)
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
