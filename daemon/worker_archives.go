package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
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

// workerRetention is a retained worker's notice record. Pending: its ask is
// not saved yet (a full or unwritable asks store); reconcile retries it.
type workerRetention struct {
	Lead, Reason    string
	Pending         bool   `json:",omitempty"`
	Since           int64  `json:",omitempty"` // first observed unpublished work; zero when not observed
	Escalated       bool   `json:",omitempty"` // set only after a fresh publication check
	MovedFrom       string `json:",omitempty"` // human-authorized notification-recipient migration
	LeadAbsentSince int64  `json:",omitempty"` // first observed absence; reset when present
}

// Routine retention is the lead's work. Only prolonged, known unpublished
// work asks the person to decide how to preserve/recover it. Generic Git or
// cleanup errors do not prove that only the person can help.
const workerUnpublishedEscalation = 72 * time.Hour
const workerLeadAbsenceGrace = 5 * time.Second

func workerRetentionNeedsUser(note workerRetention, now time.Time) bool {
	if note.Since <= 0 || now.Sub(time.UnixMilli(note.Since)) < workerUnpublishedEscalation {
		return false
	}
	return workerUnpublishedReason(note.Reason)
}

func workerUnpublishedReason(text string) bool {
	for _, reason := range []string{"uncommitted work", "unpushed work"} {
		if text == reason || strings.HasPrefix(text, reason+" (") || strings.HasPrefix(text, reason+";") {
			return true
		}
	}
	return false
}

// Failure to inspect work cannot retract earlier publication evidence. It
// cannot start or promote an escalation either; that still needs fresh proof.
func workerPublicationUncertain(text string) bool {
	if strings.HasPrefix(text, workerGitUnreadable) {
		return true
	}
	for _, reason := range []string{"work has no pushed upstream", "worktree list cannot be verified", "worktree paths cannot be verified", "worktree index cannot be verified", "worktree status cannot be verified", "submodule work requires manual review"} {
		if text == reason || strings.HasPrefix(text, reason+" (") || strings.HasPrefix(text, reason+";") {
			return true
		}
	}
	return false
}

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
	merged           map[string]workerMergedSeen
	beforeCommit     func()             // test-only race seam; nil in production
	beforeGitRecheck func()             // test-only slow-proof seam; nil in production
	teardown         func(*Pane) error  // test-only failure seam; nil in production
	liveCwd          func(*Pane) string // test-only process-cwd seam; nil in production
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
	s.merged = map[string]workerMergedSeen{}
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
	if snap.Origin == nil || snap.Origin.Worker == nil || caller == "" || (caller != snap.Origin.LeadPaneID && caller != a.workerNoticeLead(snap.ID, snap.Origin)) {
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

// workerMergedSeen is when a worker's final reply and Git proof were first
// seen merged. A changed reply or proof restarts the grace; so does a restart.
type workerMergedSeen struct {
	Mark  string
	Since time.Time
}

func (a *apiServer) workerMergedDue(id, mark string, landed bool, grace time.Duration, now time.Time) bool {
	s := a.workerArchives()
	s.mu.Lock()
	defer s.mu.Unlock()
	a.loadWorkerArchivesLocked(s)
	if !landed {
		delete(s.merged, id)
		return false
	}
	seen, ok := s.merged[id]
	if !ok || seen.Mark != mark {
		seen = workerMergedSeen{Mark: mark, Since: now}
		s.merged[id] = seen
	}
	return now.Sub(seen.Since) >= grace
}

func (a *apiServer) workerLiveCwd(p *Pane) string {
	if s := a.workerArchives(); s.liveCwd != nil {
		return s.liveCwd(p)
	}
	return liveCwdOfPane(p)
}

// workerOwnWorktree is the linked worktree a merged worker was working in
// (its transcript's cwd, else its process cwd), with the commit it started
// at, when it is part of the archive proof and the worker provably created
// it (workerCreatedWorktree). Anything else is kept.
func workerOwnWorktree(ctx context.Context, snap PaneSnapshot, cwd, checkouts string, calls []workerToolCall) (string, string) {
	if cwd == "" {
		return "", ""
	}
	root := workerGitRoot(ctx, cwd)
	var proofs map[string]workerGitProof
	if root == "" || json.Unmarshal([]byte(checkouts), &proofs) != nil {
		return "", ""
	}
	if _, ok := proofs[root]; !ok {
		return "", ""
	}
	base := workerCreatedWorktree(ctx, snap, root, calls)
	if base == "" {
		return "", ""
	}
	return root, base
}

// transcriptCwd is the directory an agent session last worked in, as its own
// transcript records it: Claude stamps the session's current directory on
// every entry and Codex on each turn_context. Process cwds are a weak signal
// for agents that run each command in a short-lived shell.
func transcriptCwd(path string) string {
	if path == "" {
		return ""
	}
	buf, err := tailReadBounded(path, paneReplyTailBytes)
	if err != nil {
		return ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type, Cwd string
			Payload   struct{ Cwd string } `json:"payload"`
		}
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if e.Cwd != "" && filepath.IsAbs(e.Cwd) {
			return filepath.Clean(e.Cwd)
		}
		if e.Type == "turn_context" && filepath.IsAbs(e.Payload.Cwd) {
			return filepath.Clean(e.Payload.Cwd)
		}
	}
	return ""
}

func (a *apiServer) paneTranscriptCwd(p *Pane) string {
	snap := p.Snapshot()
	switch paneSummaryAgent(p, snap) {
	case agentClaude:
		if path := a.transcriptForPane(p); path != "" && claudeTranscriptBelongsToPane(path, snap) {
			return transcriptCwd(path)
		}
	case agentCodex:
		return transcriptCwd(codexRolloutPath(snap.CodexThreadID))
	}
	return ""
}

// removeArchivedWorktree runs after the worker's process is gone. Failure
// keeps the folder: the archive stands and the folder stays for a person.
func (a *apiServer) removeArchivedWorktree(ctx context.Context, worker, path string) {
	if path == "" {
		return
	}
	for _, other := range a.mgr.List() {
		if other.ID == worker {
			continue
		}
		for _, cwd := range []string{other.Snapshot().Cwd, a.workerLiveCwd(other), a.paneTranscriptCwd(other)} {
			if workerPathInside(path, cwd) {
				log.Printf("worker archive %s: kept worktree %s: pane %s is using it", worker, path, other.ID)
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := workerGitRemoveWorktree(ctx, path); err != nil {
		log.Printf("worker archive %s: kept worktree: %v", worker, err)
	}
}

// workerPathInside reports whether cwd is root or below it, comparing the
// resolved paths when they exist (a symlinked pane cwd still counts).
func workerPathInside(root, cwd string) bool {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return false
	}
	for _, p := range []*string{&root, &cwd} {
		if real, err := filepath.EvalSymlinks(*p); err == nil {
			*p = real
		}
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(cwd))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
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
					s.retained[worker] = workerRetention{Lead: snap.ID, Reason: reason}
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

// noticeMu serializes notice updates, independently of archive/storage locks.
func (a *apiServer) flagWorkerLead(lead, worker, reason string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	old := s.retained[worker]
	if (lead == old.MovedFrom || !a.workerLeadPresent(lead)) && a.workerLeadPresent(old.Lead) {
		lead = old.Lead
	}
	since := old.Since
	if !workerUnpublishedReason(reason) && !workerPublicationUncertain(reason) {
		since = 0
	} else if workerUnpublishedReason(reason) && since == 0 {
		since = a.asksNow().UnixMilli()
	}
	note := workerRetention{Lead: lead, Reason: reason, Pending: true, Since: since, MovedFrom: old.MovedFrom, LeadAbsentSince: old.LeadAbsentSince}
	if lead != old.Lead {
		note.LeadAbsentSince = 0
	}
	leadGone := a.observeWorkerLeadAbsence(&note, a.asksNow())
	note.Escalated = workerRetentionNeedsUser(note, a.asksNow()) || (workerUnpublishedReason(reason) && leadGone)
	if workerPublicationUncertain(reason) {
		note.Escalated = old.Escalated
	}
	s.retained[worker] = note
	a.saveWorkerRetentionsLocked(s)
	if old.Lead != "" && old.Lead != lead {
		a.syncWorkerLeadAsksLocked(s, old.Lead, "")
	}
	a.syncWorkerLeadAsksLocked(s, lead, worker)
}

// Worker notices are asks on the lead (asks.go, kind worker), one per
// retained worker and keyed by its pane id. The retention record above stays
// the lifecycle's source of truth: it keeps the exact reason and remembers
// notices the person dismissed.
func workerAskKey(worker string) string { return "worker-" + strings.ToLower(worker) }

func (a *apiServer) workerAskText(worker, reason string) string {
	label := worker
	if p, ok := a.mgr.Get(worker); ok {
		label = a.paneCallerLabel(p)
	}
	return "Worker " + label + " kept open: " + oneLine(reason)
}

func (a *apiServer) asksNow() time.Time {
	if a.asks != nil {
		return a.asks.now()
	}
	return time.Now()
}

// A suspended lead still owns its workers across shutdown/reboot. Only a
// missing lead or a non-resumable exit starts the absence grace window.
func (a *apiServer) workerLeadPresent(lead string) bool {
	if a.paneRespawnInProgress(lead) {
		return true
	}
	p, ok := a.mgr.Get(lead)
	if !ok {
		// The replacement can begin after our first presence check.
		return a.paneRespawnInProgress(lead)
	}
	snap := p.Snapshot()
	return p.respawning.Load() || snap.Status == PaneRunning || (snap.Status == PaneExited && snap.Resumable)
}

// Called only with a fresh worker observation. Reconciliation alone cannot
// promote a saved Git reason merely because an absence timer aged.
func (a *apiServer) observeWorkerLeadAbsence(note *workerRetention, now time.Time) bool {
	if a.workerLeadPresent(note.Lead) {
		note.LeadAbsentSince = 0
		return false
	}
	if note.LeadAbsentSince == 0 {
		note.LeadAbsentSince = now.UnixMilli()
		return false
	}
	return now.Sub(time.UnixMilli(note.LeadAbsentSince)) >= workerLeadAbsenceGrace
}

// A human-authorized lead migration updates the notification recipient. Do
// not mistake its old spawn-time pane id for an abandoned lead. This changes
// only who is notified/records a read, not spending or pane-close authority.
func (a *apiServer) workerNoticeLead(worker string, origin *PaneOrigin) string {
	lead := a.workerLeadFor(origin)
	if note, ok := a.retainedWorker(worker); ok && (lead == note.MovedFrom || !a.workerLeadPresent(lead)) && a.workerLeadPresent(note.Lead) {
		return note.Lead
	}
	return lead
}

func (a *apiServer) movePaneAsks(from, to, label string) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	a.asks.movePane(from, to, label)
	changed := false
	for worker, note := range s.retained {
		if note.Lead == from {
			if note.MovedFrom == "" {
				note.MovedFrom = from
			}
			note.Lead = to
			note.LeadAbsentSince = 0
			s.retained[worker] = note
			changed = true
		}
	}
	if changed {
		a.saveWorkerRetentionsLocked(s)
		a.syncWorkerLeadAsksLocked(s, to, "")
	}
}

// observeWorkerPublication runs after Git inspection, outside the pane and
// archive locks. Merely aging a saved reason is not fresh evidence of risk.
func (a *apiServer) observeWorkerPublication(worker, reason string, now time.Time) {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	note, ok := s.retained[worker]
	if !ok {
		return
	}
	before := note
	if workerUnpublishedReason(reason) {
		if note.Since == 0 {
			note.Since = now.UnixMilli()
		}
	} else if !workerPublicationUncertain(reason) {
		note.Since = 0
		if reason == "" && workerUnpublishedReason(note.Reason) && strings.Contains(note.Reason, "final reply unread by lead") {
			note.Reason = "final reply unread by lead"
		}
	}
	verified := note
	verified.Reason = reason
	leadGone := a.observeWorkerLeadAbsence(&note, now)
	if !workerPublicationUncertain(reason) {
		note.Escalated = workerRetentionNeedsUser(verified, now) || (workerUnpublishedReason(reason) && leadGone)
	}
	if note != before {
		s.retained[worker] = note
		a.saveWorkerRetentionsLocked(s)
		a.syncWorkerLeadAsksLocked(s, note.Lead, "")
	}
}

// workerAsksAll, as syncWorkerLeadAsksLocked's create, opens an ask for every
// retained worker of the lead (the flag migration).
const workerAsksAll = "*"

// syncWorkerLeadAsksLocked makes the lead's worker asks match its retained
// workers: it updates their text and closes those no longer retained. It
// opens an ask only for create, the worker just retained, so a notice the
// person dismissed stays dismissed until the worker is retained again.
// It clears Pending for each saved notice independently, and reports the
// first ask it could not save. A failure for another worker must not leave a
// saved notice pending: retrying it could undo the person’s dismissal.
func (a *apiServer) syncWorkerLeadAsksLocked(s *workerArchiveState, lead, create string) error {
	if a.asks == nil {
		return nil
	}
	var failed error
	retentionsChanged := false
	leadPresent := a.workerLeadPresent(lead)
	open := map[string]Ask{}
	for _, x := range a.asks.list() {
		if x.Kind != askKindWorker || x.State != askOpen {
			continue
		}
		owner, retained := s.retained[x.WorkerPane]
		if (retained && owner.Lead == lead) || (!retained && x.SourcePane == lead) {
			open[x.WorkerPane] = x
		}
	}
	for worker, note := range s.retained {
		if note.Lead != lead {
			continue
		}
		if leadPresent && note.LeadAbsentSince != 0 {
			note.LeadAbsentSince = 0
			s.retained[worker] = note
			retentionsChanged = true
		}
		previous, has := open[worker]
		delete(open, worker)
		if !has && create != worker && create != workerAsksAll {
			continue
		}
		target := lead
		leadGone := !leadPresent && note.LeadAbsentSince > 0 && a.asksNow().Sub(time.UnixMilli(note.LeadAbsentSince)) >= workerLeadAbsenceGrace
		if leadGone {
			// Keep the work actionable when its lead is gone without Resume.
			target = worker
		}
		p, live := a.mgr.Get(target)
		if !live {
			continue
		}
		// Refresh the exact ask we saw, without recreating it if the person
		// dismissed it after workerAsks returned (or a new ask replaced it).
		needsUser := note.Escalated && (leadGone || (note.Since > 0 && a.asksNow().Sub(time.UnixMilli(note.Since)) >= workerUnpublishedEscalation))
		text := a.workerAskText(worker, note.Reason)
		if needsUser {
			if leadGone {
				text = "The lead is no longer running. Decide how to preserve unpublished work. " + text
			} else {
				text = "Decide how to preserve unpublished work kept for at least 72 hours. " + text
			}
		}
		_, _, err := a.asks.post(askPost{Pane: target, Source: a.paneCallerLabel(p), Kind: askKindWorker, Key: workerAskKey(worker), Text: text, WorkerPane: worker, NeedsUser: needsUser, UpdateID: previous.ID})
		if err != nil && !errors.Is(err, errAskNotFound) {
			log.Printf("worker notice for %s on %s: %v", worker, lead, err)
			if failed == nil {
				failed = err
			}
			continue
		}
		if note.Pending {
			note.Pending = false
			s.retained[worker] = note
			retentionsChanged = true
		}
	}
	if retentionsChanged {
		a.saveWorkerRetentionsLocked(s)
	}
	for _, x := range open {
		if err := a.asks.closeWorkerKey(x.SourcePane, x.Key, "worker_cleared"); err != nil {
			log.Printf("worker notice %s on %s: %v", x.Key, lead, err)
		}
	}
	return failed
}

// migrateWorkerManagerFlags turns the worker manager's old flags into asks:
// their memos seed the retention record (loadWorkerRetentionsLocked), each
// retained worker gets an ask on its lead, and the flag is cleared. Run at
// boot; once no worker-manager flag is left it does nothing.
func (a *apiServer) migrateWorkerManagerFlags() {
	s := a.workerArchives()
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	a.loadWorkerRetentionsLocked(s)
	var leads []*Pane
	for _, p := range a.mgr.List() {
		if snap := p.Snapshot(); snap.Flagged && snap.FlagOwner == "worker-manager" {
			leads = append(leads, p)
		}
	}
	if len(leads) == 0 {
		return
	}
	a.saveWorkerRetentionsLocked(s)
	for _, p := range leads {
		// The flag stays until every notice it carried is saved as an ask, so
		// a failed save is retried at the next start.
		if err := a.syncWorkerLeadAsksLocked(s, p.ID, workerAsksAll); err != nil {
			continue
		}
		_ = a.mgr.changeCurrentFlag(p, a.panes, func(prev PaneSnapshot) error {
			if !prev.Flagged || prev.FlagOwner != "worker-manager" {
				return errors.New("not the worker manager's flag")
			}
			return nil
		}, false, "", "")
	}
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
		if a.asks != nil {
			a.asks.closeWorkerNotice(worker)
		}
		a.syncWorkerLeadAsksLocked(s, note.Lead, "")
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
	if a.asks != nil {
		for _, lead := range a.asks.workerAskLeads() {
			leads[lead] = true
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
			if a.asks != nil {
				a.asks.closeWorkerNotice(id)
			}
			changed = true
			s.mu.Lock()
			delete(s.flagged, id)
			s.mu.Unlock()
			continue
		}
		snap := p.Snapshot()
		if !enabled || paneTeamKind(snap.Origin) != paneTeamWorker || snap.Origin.Worker == nil || snap.Origin.Worker.State != "finished" {
			delete(s.retained, id)
			if a.asks != nil {
				a.asks.closeWorkerNotice(id)
			}
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
		a.syncWorkerLeadAsksLocked(s, lead, "")
	}
	// Retry notices whose ask could not be saved when the worker was retained.
	for id, note := range s.retained {
		if note.Pending {
			a.syncWorkerLeadAsksLocked(s, note.Lead, id)
		}
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
	grace := time.Duration(q.settings.WorkerMergedGraceMinutes) * time.Minute
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
	if p.computeStateCached() != StateIdle && snap.Status != PaneExited {
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
	inputRev := p.inputRevision.Load()
	read := receipt.Pane == p && receipt.Turn == key && receipt.Input == inputRev && !receipt.At.Before(reply.CompletedAt)
	live := a.workerLiveCwd(p)
	ctx, cancel := context.WithTimeout(baseContext, 4*time.Second)
	proof, reason := workerGitForArchive(ctx, snap, live)
	cancel()
	a.observeWorkerPublication(p.ID, reason, now)
	if snap.Status == PaneExited {
		if prior, known := a.retainedWorker(p.ID); reason == "" && known && !strings.Contains(prior.Reason, "final reply unread by lead") && !strings.HasPrefix(prior.Reason, "archive ") {
			a.clearWorkerRetention(p.ID)
		}
		return // observe risk, but do not change the existing archive gate
	}
	// Merged = done: once every owned checkout is clean and has landed on the
	// remote default branch, the worker closes after a grace without a read
	// receipt. Its reply stays readable from the archive. Like the read
	// rule, it needs a reply finished since the daemon started: input or a
	// draft typed before a restart cannot be seen. It needs no publication
	// proof: a squash-merged branch whose remote ref was pruned has no
	// containing ref, yet its content is on the branch.
	merged, ownWorktree, checkouts := false, "", ""
	var calls []workerToolCall // the worker's own tool calls, read once per check
	worked := transcriptCwd(reply.Path)
	if snap.Origin.WorkerGit != nil && final {
		ctx, cancel := context.WithTimeout(baseContext, 8*time.Second)
		calls = workerTranscriptCalls(ctx, reply.Path)
		checkouts = workerGitCheckouts(ctx, snap, calls, live, worked)
		landed := checkouts != "" && workerGitMerged(ctx, snap, checkouts, calls, worked, live)
		if landed {
			ownWorktree, _ = workerOwnWorktree(ctx, snap, worked, checkouts, calls)
			if ownWorktree == "" {
				ownWorktree, _ = workerOwnWorktree(ctx, snap, live, checkouts, calls)
			}
		}
		cancel()
		merged = a.workerMergedDue(p.ID, key+"\x00"+checkouts, landed, grace, now)
	} else {
		a.workerMergedDue(p.ID, "", false, grace, now)
	}
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
		if known && prior.Lead == a.workerNoticeLead(snap.ID, snap.Origin) && (prior.Reason == reason || (!read && prior.Reason == reason+"; final reply unread by lead")) {
			return
		}
		a.flagWorkerLead(a.workerNoticeLead(snap.ID, snap.Origin), p.ID, reason)
		return
	}
	allowed, flag := workerArchiveAllowed(true, key != "", read || merged, noInput, reason == "" || merged)
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
			lead := a.workerNoticeLead(snap.ID, snap.Origin)
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
	if merged {
		ctx, cancel = context.WithTimeout(baseContext, 8*time.Second)
		fresh := workerGitCheckouts(ctx, p.Snapshot(), calls, a.workerLiveCwd(p), transcriptCwd(reply.Path))
		cancel()
		if fresh == "" || fresh != checkouts {
			return
		}
	} else {
		ctx, cancel = context.WithTimeout(baseContext, 4*time.Second)
		fresh, why := workerGitForArchive(ctx, p.Snapshot(), a.workerLiveCwd(p))
		cancel()
		if why != "" || fresh != proof {
			return
		}
	}
	var entry workerArchiveEntry
	committed := false
	// Once committed under the writer fence, input refuses the old process;
	// reopening has a new ID. Recheck identity/input after the slow proof.
	a.mgr.withCurrentPane(p, func() {
		p.agentWriteMu.Lock()
		defer p.agentWriteMu.Unlock()
		if baseContext.Err() != nil || p.workerArchiving.Load() || p.inputRevision.Load() != inputRev || (atomic.LoadInt64(&p.LastInputMs) > 0 && atomic.LoadInt64(&p.LastInputMs) >= reply.CompletedAt.UnixMilli()) {
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
		a.flagWorkerLead(a.workerNoticeLead(snap.ID, snap.Origin), p.ID, "archive layout persistence failed")
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
		a.flagWorkerLead(a.workerNoticeLead(snap.ID, snap.Origin), p.ID, "archive process shutdown failed")
		return
	}
	if a.panes != nil {
		if err = a.panes.Remove(p.ID); err != nil {
			a.flagWorkerLead(a.workerNoticeLead(snap.ID, snap.Origin), p.ID, "archive pane-store cleanup failed")
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
	s.mu.Lock()
	delete(s.merged, p.ID)
	s.mu.Unlock()
	if merged {
		a.removeArchivedWorktree(baseContext, p.ID, ownWorktree)
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
