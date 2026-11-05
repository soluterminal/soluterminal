package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Per-turn change tracking for agent panes in TERMINAL mode.
//
// Production terminal turns follow owned provider transcript boundaries. PTY
// output, redraws, silence and process exits never prove successful completion.
// Approval/tool activity remains inside the provider turn. Legacy heuristic
// history stays readable, but cannot supply a current successful 'done' chip.
//
// At both edges the tracker snapshots the pane's git repository WITHOUT
// touching anything the user owns: the real index is copied to a temp file,
// `git add` runs against that copy (GIT_INDEX_FILE), and `git write-tree`
// returns a tree id. No commit, no ref, no change to the branch, the index or
// `git log --all`. The trees are ordinary unreachable objects, which git keeps
// for gc.pruneExpire (two weeks by default) — far longer than anyone reviews a
// turn. A pruned tree just makes that old turn's diff unavailable.
//
// The diff of a turn is base-tree → end-tree, i.e. everything that changed in
// the repo while the turn ran. When another agent pane was working in the same
// repo during the same window the record names it (OverlapPanes) so the UI can
// say "some changes may be from <pane>" instead of over-claiming attribution.

const (
	turnTickInterval      = time.Second
	turnSettleQuiet       = 3 * time.Second
	turnMinDuration       = 5 * time.Second
	turnSelfStartTicks    = 3 // busy ticks without user input before a turn opens
	turnInputLookback     = 15 * time.Second
	turnHistoryCap        = 50
	turnSnapshotTimeout   = 20 * time.Second
	turnUntrackedMaxBytes = 1 << 20
	// Changed tracked files above this are recorded as unchanged (skipped):
	// hashing a multi-GB database every turn would fill .git/objects.
	turnTrackedMaxBytes = 10 << 20
	// Total bytes one snapshot may read and hash.
	turnSnapshotMaxBytes = 128 << 20
	// Cap on any single git command's output held in memory.
	turnGitMaxOutput = 64 << 20
	// Snapshots run at most this many at once across all panes.
	turnSnapshotConcurrency = 2
	turnUntrackedMaxFiles   = 2000
	turnFileMaxAge          = 14 * 24 * time.Hour
)

// turnRecord is one completed turn. Persisted per pane under
// <dataDir>/turns/<paneID>.json and served by /api/panes/{id}/turns.
type turnRecord struct {
	Source       string `json:"source,omitempty"`
	SourceTurnID string `json:"sourceTurnId,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	Historical   bool   `json:"historical,omitempty"`
	N            int    `json:"n"`
	Agent        string `json:"agent,omitempty"`
	StartedMs    int64  `json:"startedMs"`
	EndedMs      int64  `json:"endedMs"`
	Repo         string `json:"repo,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseTree     string `json:"baseTree,omitempty"`
	EndTree      string `json:"endTree,omitempty"`
	Files        int    `json:"files"`
	Added        int    `json:"added"`
	Removed      int    `json:"removed"`
	// OverlapPanes names other agent panes that were working in the same repo
	// while this turn ran — their edits are in this diff too.
	OverlapPanes []string `json:"overlapPanes,omitempty"`
	// SkippedUntracked counts new files left out of the snapshot (too large,
	// secret-looking, or over the per-snapshot file cap).
	SkippedUntracked int `json:"skippedUntracked,omitempty"`
	// Note explains a turn without a diff ("not a git repository", a timeout).
	Note string `json:"note,omitempty"`
}

func (r turnRecord) hasDiff() bool { return r.BaseTree != "" && r.EndTree != "" }

// paneTurnDTO is the compact per-pane summary carried on every /api/panes row.
type paneTurnDTO struct {
	HistoryRevision uint64      `json:"historyRevision,omitempty"`
	Uncertain       bool        `json:"uncertain,omitempty"`
	Active          bool        `json:"active"`
	StartedMs       int64       `json:"startedMs,omitempty"`
	Count           int         `json:"count"`
	Last            *turnRecord `json:"last,omitempty"`
}

type turnPersisted struct {
	Active   *turnActiveCheckpoint `json:"active,omitempty"`
	Reader   *turnReaderCheckpoint `json:"reader,omitempty"`
	Revision uint64                `json:"revision,omitempty"`
	Count    int                   `json:"count"`
	History  []turnRecord          `json:"history"`
}

type activeTurn struct {
	paneID    string
	provider  *providerTurn
	agent     string
	startedMs int64
	repo      string // set once the base snapshot lands
	baseTree  string
	branch    string
	skipped   int
	note      string
	baseDone  chan struct{}
}

type paneTurnState struct {
	providerCheckpoint *turnReaderCheckpoint
	providerSource     string
	historyRevision    uint64
	providerReader     turnProviderReader
	providerUncertain  bool
	providerCompleted  string
	title              string
	count              int
	history            []turnRecord // oldest first
	active             *activeTurn
	busyTicks          int
	firstBusyMs        int64
	quietSinceMs       int64
	lastEndMs          int64
}

// turnPane is what the tracker needs from a pane; *Pane satisfies it through
// turnPaneAdapter, tests use fakes.
type turnPane interface {
	id() string
	title() string
	running() bool
	native() bool
	state() PaneRuntimeState
	agent() string // "claude" | "codex" | "muse" | ""
	cwd() string
	lastInputMs() int64
	lastActivityMs() int64
}

type turnSnapshotter interface {
	snapshot(ctx context.Context, dir string) (turnSnapshot, error)
	stat(ctx context.Context, repo, base, end string) (files, added, removed int, err error)
}

type turnSnapshot struct {
	repo, branch, tree string
	skipped            int
	secretSkipped      int // untracked secret-looking files left out
	// trackedUnread counts tracked paths recorded as unchanged without being
	// read (too large, unreadable, submodules): their real state is unknown.
	trackedUnread int
}

type turnTracker struct {
	providerLifecycle bool
	lifecycleCutoffMs int64
	dir               string
	snap              turnSnapshotter
	panes             func() []turnPane
	now               func() time.Time

	mu    sync.Mutex
	state map[string]*paneTurnState
	// pending counts in-flight snapshot goroutines (tests wait on it).
	pending sync.WaitGroup
}

// activeTurnTracker is consulted by the pane DTO builder; nil-safe so tests
// and builds without the tracker simply omit the field.
var activeTurnTracker atomic.Pointer[turnTracker]

func newTurnTracker(dataDir string, mgr *Manager) *turnTracker {
	t := &turnTracker{
		providerLifecycle: true,
		lifecycleCutoffMs: time.Now().UnixMilli(),
		dir:               filepath.Join(dataDir, "turns"),
		snap:              gitTurnSnapshotter{},
		now:               time.Now,
		state:             map[string]*paneTurnState{},
	}
	t.panes = func() []turnPane {
		list := mgr.List()
		out := make([]turnPane, 0, len(list))
		for _, p := range list {
			out = append(out, turnPaneAdapter{p})
		}
		return out
	}
	return t
}

func (t *turnTracker) run(ctx context.Context) {
	t.pruneOldFiles()
	tick := time.NewTicker(turnTickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.tick()
		}
	}
}

func (t *turnTracker) tick() {
	panes := t.panes()
	seen := make(map[string]bool, len(panes))
	for _, p := range panes {
		seen[p.id()] = true
		t.observe(p)
	}
	t.mu.Lock()
	var gone []string
	for id := range t.state {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		delete(t.state, id)
	}
	t.mu.Unlock()
	for _, id := range gone {
		_ = os.Remove(t.file(id))
	}
}

func (t *turnTracker) observe(p turnPane) {
	if p.native() {
		return
	}
	id := p.id()
	nowMs := t.now().UnixMilli()
	st := t.load(id)
	if t.providerLifecycle {
		t.observeProvider(p, st, nowMs)
		return
	}

	t.mu.Lock()
	st.title = p.title()
	running := p.running()
	var cur PaneRuntimeState = StateExited
	if running {
		cur = p.state()
	}
	if st.active == nil {
		if cur != StateBusy {
			st.busyTicks, st.firstBusyMs = 0, 0
			t.mu.Unlock()
			return
		}
		if st.busyTicks == 0 {
			st.firstBusyMs = nowMs
		}
		st.busyTicks++
		input := p.lastInputMs()
		userSubmitted := input > 0 && nowMs-input <= turnInputLookback.Milliseconds() && input > st.lastEndMs
		if !userSubmitted && st.busyTicks < turnSelfStartTicks {
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		// Probe the process tree outside the lock: only at this edge, never per tick.
		agent := p.agent()
		if agent == "" {
			return
		}
		cwd := p.cwd()
		t.mu.Lock()
		if st.active != nil { // raced with another tick
			t.mu.Unlock()
			return
		}
		started := st.firstBusyMs
		if userSubmitted && input < started {
			started = input
		}
		at := &activeTurn{agent: agent, startedMs: started, baseDone: make(chan struct{})}
		st.active = at
		st.quietSinceMs = 0
		t.mu.Unlock()
		t.startBase(at, cwd)
		return
	}

	switch cur {
	case StateBusy, StateRecent, StateChoicePrompt:
		st.quietSinceMs = 0
		t.mu.Unlock()
		return
	case StateExited:
		// fall through to close immediately
	default: // idle, awaiting-input
		if st.quietSinceMs == 0 {
			st.quietSinceMs = nowMs
		}
		if nowMs-st.quietSinceMs < turnSettleQuiet.Milliseconds() {
			t.mu.Unlock()
			return
		}
	}
	at := st.active
	st.active = nil
	st.busyTicks, st.firstBusyMs, st.quietSinceMs = 0, 0, 0
	ended := p.lastActivityMs()
	if ended < at.startedMs || ended > nowMs {
		ended = nowMs
	}
	st.lastEndMs = nowMs
	t.mu.Unlock()
	t.finish(id, at, ended)
}

func (t *turnTracker) acquireSnapshotSlot() func() {
	release, _ := acquireTurnGitSlot(context.Background())
	return release
}

func (t *turnTracker) startBase(at *activeTurn, cwd string) {
	t.pending.Add(1)
	go func() {
		defer t.pending.Done()
		defer close(at.baseDone)
		if cwd == "" {
			t.setBase(at, turnSnapshot{}, errors.New("unknown working directory"))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), turnSnapshotTimeout)
		defer cancel()
		release, err := acquireTurnGitSlot(ctx)
		defer release()
		if err != nil {
			t.setBase(at, turnSnapshot{}, err)
			return
		}
		snap, err := t.snap.snapshot(ctx, cwd)
		t.setBase(at, snap, err)
	}()
}

func (t *turnTracker) setBase(at *activeTurn, snap turnSnapshot, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at.repo, at.branch, at.baseTree, at.skipped = snap.repo, snap.branch, snap.tree, snap.skipped
	if at.provider != nil && at.provider.EndedMs > 0 {
		if at.note != "" {
			at.note += "; "
		}
		at.note += "Base snapshot completed after provider completion; diff may omit earlier changes"
	}
	if err != nil {
		if at.note != "" {
			at.note += "; "
		}
		at.note += err.Error()
	}
	if at.paneID != "" {
		if st := t.state[at.paneID]; st != nil && st.active == at {
			t.persistLocked(at.paneID, st)
		}
	}
}

func (t *turnTracker) finish(paneID string, at *activeTurn, endedMs int64) {
	t.pending.Add(1)
	go func() {
		defer t.pending.Done()
		<-at.baseDone
		t.mu.Lock()
		rec := turnRecord{
			Agent: at.agent, StartedMs: at.startedMs, EndedMs: endedMs,
			Repo: at.repo, Branch: at.branch, BaseTree: at.baseTree,
			SkippedUntracked: at.skipped, Note: at.note,
		}
		if at.provider != nil {
			rec.Source = at.provider.Agent + ":" + at.provider.Session
			rec.SourceTurnID = at.provider.ID
			rec.Outcome = at.provider.Outcome
		}
		t.mu.Unlock()
		if rec.BaseTree != "" {
			ctx, cancel := context.WithTimeout(context.Background(), turnSnapshotTimeout)
			release, err := acquireTurnGitSlot(ctx)
			var snap turnSnapshot
			if err == nil {
				snap, err = t.snap.snapshot(ctx, rec.Repo)
			}
			if err == nil {
				rec.EndTree = snap.tree
				if snap.skipped > rec.SkippedUntracked {
					rec.SkippedUntracked = snap.skipped
				}
				rec.Files, rec.Added, rec.Removed, err = t.snap.stat(ctx, rec.Repo, rec.BaseTree, rec.EndTree)
			}
			cancel()
			release()
			if err != nil {
				rec.EndTree = ""
				rec.Note = err.Error()
			}
		}
		if rec.Source != "" {
			t.updateProviderCapture(paneID, rec)
			return
		}
		if rec.Source == "" && time.Duration(rec.EndedMs-rec.StartedMs)*time.Millisecond < turnMinDuration && rec.Files == 0 {
			return // a blip: prompt echo, a redraw burst, a one-liner answer
		}
		t.record(paneID, rec)
	}()
}

func (t *turnTracker) record(paneID string, rec turnRecord) {
	t.mu.Lock()
	st, ok := t.state[paneID]
	if !ok {
		t.mu.Unlock()
		return // pane closed while the snapshot ran
	}
	if rec.Repo != "" {
		rec.OverlapPanes = t.overlapLocked(paneID, rec)
	}
	st.count++
	rec.N = st.count
	st.history = append(st.history, rec)
	sort.SliceStable(st.history, func(i, j int) bool { return st.history[i].StartedMs < st.history[j].StartedMs })
	if len(st.history) > turnHistoryCap {
		st.history = append([]turnRecord(nil), st.history[len(st.history)-turnHistoryCap:]...)
	}
	st.historyRevision++
	t.persistLocked(paneID, st)
	t.mu.Unlock()
}

func (t *turnTracker) persistLocked(paneID string, st *paneTurnState) {
	persisted := turnPersisted{Count: st.count, History: st.history, Revision: st.historyRevision}
	if at := st.active; at != nil && at.provider != nil {
		persisted.Active = &turnActiveCheckpoint{Turn: *at.provider, Repo: at.repo, BaseTree: at.baseTree, Branch: at.branch, Skipped: at.skipped, Note: at.note}
		persisted.Reader = st.providerCheckpoint
	}
	data, _ := json.Marshal(persisted)
	if err := os.MkdirAll(t.dir, 0o700); err == nil {
		_ = writeFileAtomic(t.file(paneID), data, 0o600)
	}
}
func (t *turnTracker) updateProviderCapture(paneID string, rec turnRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state[paneID]
	if st == nil {
		return
	}
	for i, previous := range st.history {
		if previous.Source == rec.Source && previous.SourceTurnID == rec.SourceTurnID {
			rec.N, rec.Historical = previous.N, previous.Historical
			overlaps := map[string]bool{}
			for _, name := range previous.OverlapPanes {
				overlaps[name] = true
			}
			for _, name := range t.overlapLocked(paneID, rec) {
				overlaps[name] = true
			}
			rec.OverlapPanes = nil
			for name := range overlaps {
				rec.OverlapPanes = append(rec.OverlapPanes, name)
			}
			sort.Strings(rec.OverlapPanes)
			st.history[i] = rec
			st.historyRevision++
			t.persistLocked(paneID, st)
			return
		}
	}
}

// overlapLocked lists the titles of OTHER panes that had a turn in the same
// repository overlapping rec's window (finished or still running).
func (t *turnTracker) overlapLocked(paneID string, rec turnRecord) []string {
	var out []string
	for id, st := range t.state {
		if id == paneID {
			continue
		}
		hit := false
		if a := st.active; a != nil && a.repo == rec.Repo && a.startedMs < rec.EndedMs {
			hit = true
		}
		for i := len(st.history) - 1; i >= 0 && !hit; i-- {
			h := st.history[i]
			if h.EndedMs < rec.StartedMs {
				break
			}
			if h.Repo == rec.Repo && h.StartedMs < rec.EndedMs {
				hit = true
			}
		}
		if hit {
			name := st.title
			if name == "" {
				name = id
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (t *turnTracker) file(paneID string) string {
	return filepath.Join(t.dir, filepath.Base(paneID)+".json")
}

// load returns the pane's state, reading its persisted history the first time
// the pane is seen (so a daemon restart keeps "done 2m ago" and old diffs).
func (t *turnTracker) load(paneID string) *paneTurnState {
	t.mu.Lock()
	st, ok := t.state[paneID]
	t.mu.Unlock()
	if ok {
		return st
	}
	fresh := &paneTurnState{providerUncertain: t.providerLifecycle}
	if data, err := os.ReadFile(t.file(paneID)); err == nil {
		var p turnPersisted
		if json.Unmarshal(data, &p) == nil {
			fresh.count, fresh.history, fresh.historyRevision = p.Count, p.History, p.Revision
			if t.providerLifecycle && p.Active != nil && p.Active.Turn.StartedMs > 0 && p.Reader != nil && p.Reader.Offset >= 0 {
				saved := p.Active
				active := saved.Turn
				done := make(chan struct{})
				close(done)
				fresh.active = &activeTurn{paneID: paneID, provider: &active, agent: active.Agent, startedMs: active.StartedMs, repo: saved.Repo, baseTree: saved.BaseTree, branch: saved.Branch, skipped: saved.Skipped, note: saved.Note, baseDone: done}
				if saved.BaseTree == "" {
					fresh.active.note = "Recovered active turn; original base snapshot unavailable"
				}
				fresh.providerReader.restore = p.Reader
				fresh.providerCheckpoint = p.Reader
			}
			if n := len(p.History); n > 0 {
				fresh.lastEndMs = p.History[n-1].EndedMs
			}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.state[paneID]; ok {
		return st
	}
	t.state[paneID] = fresh
	return fresh
}

func (t *turnTracker) pruneOldFiles() {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return
	}
	cutoff := t.now().Add(-turnFileMaxAge)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(t.dir, e.Name()))
		}
	}
}

func (t *turnTracker) summary(paneID string) *paneTurnDTO {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.state[paneID]
	if !ok {
		return nil
	}
	d := &paneTurnDTO{Count: st.count, Uncertain: st.providerUncertain, HistoryRevision: st.historyRevision}
	if st.active != nil {
		d.Active, d.StartedMs = true, st.active.startedMs
	}
	for i := len(st.history) - 1; i >= 0; i-- {
		last := st.history[i]
		if t.providerLifecycle && (last.Source == "" || st.providerSource != "" && last.Source != st.providerSource) {
			continue
		}
		last.BaseTree, last.EndTree = "", ""
		d.Last = &last
		break
	}
	if !t.providerLifecycle && !d.Active && d.Last == nil {
		return nil
	}
	return d
}

// list returns the pane's turns newest first, plus the active turn's start.
func (t *turnTracker) list(paneID string) (turns []turnRecord, activeStartedMs int64) {
	st := t.load(paneID)
	t.mu.Lock()
	defer t.mu.Unlock()
	turns = make([]turnRecord, 0, len(st.history))
	for i := len(st.history) - 1; i >= 0; i-- {
		turns = append(turns, st.history[i])
	}
	if st.active != nil {
		activeStartedMs = st.active.startedMs
	}
	return turns, activeStartedMs
}

func (t *turnTracker) get(paneID string, n int) (turnRecord, bool) {
	st := t.load(paneID)
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range st.history {
		if r.N == n {
			return r, true
		}
	}
	return turnRecord{}, false
}

// ---- *Pane adapter ----

type turnPaneAdapter struct{ p *Pane }

func (a turnPaneAdapter) id() string { return a.p.ID }
func (a turnPaneAdapter) title() string {
	s := a.p.Snapshot()
	if s.Title != "" {
		return s.Title
	}
	return strings.Join(s.Cmd, " ")
}
func (a turnPaneAdapter) running() bool {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	return a.p.Status == PaneRunning
}
func (a turnPaneAdapter) native() bool            { return a.p.Snapshot().Kind == "native" }
func (a turnPaneAdapter) state() PaneRuntimeState { return a.p.computeStateCached() }
func (a turnPaneAdapter) agent() string {
	switch {
	case a.p.HasClaudeDescendant():
		return "claude"
	case a.p.HasCodexDescendant():
		return "codex"
	case a.p.HasMuseDescendant():
		return "muse"
	}
	return ""
}
func (a turnPaneAdapter) cwd() string {
	if c := liveCwdOfPane(a.p); c != "" {
		return c
	}
	return a.p.Snapshot().Cwd
}
func (a turnPaneAdapter) lastInputMs() int64    { return atomic.LoadInt64(&a.p.LastInputMs) }
func (a turnPaneAdapter) lastActivityMs() int64 { return atomic.LoadInt64(&a.p.LastActivityMs) }

// ---- git snapshotter ----

type gitTurnSnapshotter struct{}

var errTurnSparse = errors.New("sparse checkout — turn snapshots disabled")

// Every turn git runs through hardenedGitCommand (git_env.go). The snapshot
// runs as the DAEMON, outside any agent sandbox, so a repository (or an agent
// able to edit .git/config or .gitattributes) must never be able to make it
// execute something. Clean / smudge filters and textconv are avoided by
// construction (no `git add`, no worktree reads by git, --no-filters,
// --no-textconv); the shared options cover the rest: the fsmonitor hook, the
// untracked-cache, hooks, signature verification and transports.

// turnGitEnv is the fixed environment turn git adds to
// hardenedGitCommandWithExcludes'.
var turnGitEnv = []string{"GIT_PAGER=cat", "LC_ALL=C"}

// turnSafeRel accepts only structurally plain repo-relative paths: no
// absolute paths and no "", "." or ".." components (update-index would reject
// those and fail the whole snapshot). Odd characters are fine: the content is
// read by the daemon through the repo root and git only ever sees the path
// NUL-terminated in --index-info, never as a filesystem path to open.
func turnSafeRel(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." || part == "." || part == "" || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// turnPathWithin reports whether path is base or inside it, both resolved
// through symlinks.
func turnPathWithin(base, path string) bool {
	b, err1 := filepath.EvalSymlinks(base)
	p, err2 := filepath.EvalSymlinks(path)
	if err1 != nil || err2 != nil {
		return false
	}
	return p == b || strings.HasPrefix(p, b+string(filepath.Separator))
}

// turnParentInside reports whether rel's parent directory, with every symlink
// resolved, is still inside the repository — so a directory replaced by a
// symlink to elsewhere is never read through.
func turnParentInside(repoReal, rel string) bool {
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Join(repoReal, rel)))
	if err != nil {
		return false
	}
	return parent == repoReal || strings.HasPrefix(parent, repoReal+string(filepath.Separator))
}

// cappedBuffer collects command output up to a limit, so a huge listing or
// diff can't balloon daemon memory. The buffer is a named field, NOT embedded:
// an embedded bytes.Buffer would promote ReadFrom, and io.Copy (used by
// os/exec) would then bypass Write and the cap entirely.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

var errTurnOutputTooLarge = errors.New("git output exceeded the snapshot limit")

// Write keeps what fits and then fails, so the writer (git) gets EPIPE and
// stops instead of streaming the rest into a buffer nobody reads.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.overflow = true
		return 0, errTurnOutputTooLarge
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Len() int       { return c.buf.Len() }
func (c *cappedBuffer) String() string { return c.buf.String() }

var errTurnPartialClone = errors.New("partial clone — turn snapshots disabled (missing objects would be fetched)")

// turnRepoRefusal rejects repositories whose git operations could reach
// out: a partial clone fetches missing objects on demand (running the remote
// helper / ssh command the repo configures). Older git lacks
// GIT_NO_LAZY_FETCH, so refuse up front.
func turnRepoRefusal(ctx context.Context, repo string) error {
	if v, _ := turnGit(ctx, repo, nil, nil, "config", "--get", "extensions.partialClone"); strings.TrimSpace(v) != "" {
		return errTurnPartialClone
	}
	// ANY promisor key refuses, whatever its value: git accepts yes/on/1/true
	// spellings, and matching one of them would be a bypass waiting to happen.
	if v, _ := turnGit(ctx, repo, nil, nil, "config", "--name-only", "--get-regexp", `^remote\..*\.promisor$`); strings.TrimSpace(v) != "" {
		return errTurnPartialClone
	}
	return nil
}

// Git work is bounded by two small pools (each run uses its own temp index,
// so this is about load, not safety): one for the turn tracker's snapshots,
// one for the diff endpoints — so someone browsing diffs can never delay a
// turn's base snapshot past the agent's first edits.
var (
	turnGitSlots      = make(chan struct{}, turnSnapshotConcurrency)
	diffEndpointSlots = make(chan struct{}, turnSnapshotConcurrency)
)

func acquireSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

func acquireTurnGitSlot(ctx context.Context) (func(), error) { return acquireSlot(ctx, turnGitSlots) }

func acquireDiffEndpointSlot(ctx context.Context) (func(), error) {
	return acquireSlot(ctx, diffEndpointSlots)
}

// turnResolveRepo returns the repository toplevel for dir, refusing a
// worktree that core.worktree (or a crafted .git file) points somewhere
// else: the toplevel must contain dir, and resolving the git dir from the
// toplevel must find the SAME repository as resolving it from dir (else an
// ancestor repo such as a dotfiles ~/.git would be read and written).
func turnResolveRepo(ctx context.Context, dir string) (string, error) {
	top, err := turnGit(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	repo := strings.TrimSpace(top)
	if err != nil && !strings.Contains(err.Error(), "not a git repository") {
		return "", err // git's own reason, e.g. a repository owned by another user
	}
	if err != nil || repo == "" {
		return "", errors.New("not a git repository")
	}
	if !turnPathWithin(repo, dir) {
		return "", errTurnRedirected
	}
	fromDir, err1 := turnGit(ctx, dir, nil, nil, "rev-parse", "--absolute-git-dir")
	fromTop, err2 := turnGit(ctx, repo, nil, nil, "rev-parse", "--absolute-git-dir")
	if err1 != nil || err2 != nil || strings.TrimSpace(fromDir) != strings.TrimSpace(fromTop) {
		return "", errTurnRedirected
	}
	return repo, nil
}

var errTurnRedirected = errors.New("repository worktree is redirected outside this folder (core.worktree)")

func turnGit(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	out, overflow, err := turnGitLimited(ctx, dir, env, stdin, turnGitMaxOutput, args...)
	if overflow {
		return "", fmt.Errorf("git %s: %w", args[0], errTurnOutputTooLarge)
	}
	return out, err
}

// turnGitLimited runs a hardened git command keeping at most limit bytes of
// stdout; overflow reports that output was cut (the error is then nil when
// the cut was the only problem, so callers may show a truncated diff).
func turnGitLimited(ctx context.Context, dir string, env []string, stdin io.Reader, limit int, args ...string) (string, bool, error) {
	// Update successors run detached on Windows, with no console for Git to
	// inherit: the builder applies applyNoWindow, as pipes alone do not prevent
	// Windows from opening a new console for each background probe.
	cmd := hardenedGitCommandWithExcludes(ctx, dir, args...)
	cmd.Env = slices.Concat(cmd.Env, turnGitEnv, env)
	cmd.Stdin = stdin
	out := &cappedBuffer{limit: limit}
	errb := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", false, fmt.Errorf("git %s: timed out", args[0])
		}
		if out.overflow {
			return out.String(), true, nil
		}
		return "", false, fmt.Errorf("git %s: %s", args[0], gitFailureReason(errb.String()))
	}
	return out.String(), out.overflow, nil
}

// turnIndexEntry is one stage-0 entry of the repository's real index, with
// the stat data git recorded for it (from `git ls-files -s --debug`).
type turnIndexEntry struct {
	mode, sha       string
	size            int64
	mtimeS, mtimeNs int64
}

// readTurnIndex lists the index's entries without touching the worktree.
// Conflicted paths (stage > 0) are returned with an empty sha so the caller
// hashes the worktree file instead.
func readTurnIndex(ctx context.Context, repo string) (map[string]turnIndexEntry, error) {
	out, err := turnGit(ctx, repo, nil, nil, "ls-files", "-s", "--debug", "-z")
	if err != nil {
		return nil, err
	}
	entries := map[string]turnIndexEntry{}
	var cur string
	var curEntry turnIndexEntry
	flush := func() {
		if cur != "" {
			if prev, ok := entries[cur]; ok && (prev.sha == "" || curEntry.sha == "") {
				curEntry.sha = ""
			}
			entries[cur] = curEntry
		}
		cur, curEntry = "", turnIndexEntry{}
	}
	// -z terminates each path with NUL; the --debug stat block follows as
	// lines starting with two spaces, then the next "<mode> <sha> <stage>\t<path>"
	// runs to the next NUL (the path itself may contain newlines).
	for _, chunk := range strings.Split(out, "\x00") {
		for strings.HasPrefix(chunk, "  ") {
			line, rest, _ := strings.Cut(chunk, "\n")
			chunk = rest
			f := strings.Fields(line)
			for i := 0; i+1 < len(f); i++ {
				switch f[i] {
				case "mtime:":
					sec, nsec, _ := strings.Cut(f[i+1], ":")
					curEntry.mtimeS, _ = strconv.ParseInt(sec, 10, 64)
					curEntry.mtimeNs, _ = strconv.ParseInt(nsec, 10, 64)
				case "size:":
					curEntry.size, _ = strconv.ParseInt(f[i+1], 10, 64)
				}
			}
		}
		if chunk == "" {
			continue
		}
		flush()
		head, path, ok := strings.Cut(chunk, "\t")
		hf := strings.Fields(head)
		if !ok || len(hf) != 3 {
			continue
		}
		cur = path
		curEntry = turnIndexEntry{mode: hf[0], sha: hf[1]}
		if hf[2] != "0" {
			curEntry.sha = "" // conflicted: take the worktree file
		}
	}
	flush()
	return entries, nil
}

// snapshot records the working tree as a git tree WITHOUT running anything
// the repository configures: it never calls `git add` (clean filters,
// fsmonitor) and git never reads the worktree. Unchanged tracked files reuse
// the blob id the index already holds (stat match, racy-safe); changed and new
// files are hashed raw with `hash-object --no-filters`; the tree is assembled
// in a temporary index via --index-info. The user's index, branch and refs
// are never written.
func (gitTurnSnapshotter) snapshot(ctx context.Context, dir string) (turnSnapshot, error) {
	var s turnSnapshot
	repo, err := turnResolveRepo(ctx, dir)
	if err != nil {
		return s, err
	}
	s.repo = repo
	if err := turnRepoRefusal(ctx, s.repo); err != nil {
		return s, err
	}
	if sparse, _ := turnGit(ctx, s.repo, nil, nil, "config", "--bool", "core.sparseCheckout"); strings.TrimSpace(sparse) == "true" {
		return s, errTurnSparse
	}
	if b, err := turnGit(ctx, s.repo, nil, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		s.branch = strings.TrimSpace(b)
	}
	idxPath, err := turnGit(ctx, s.repo, nil, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return s, err
	}
	idxPath = strings.TrimSpace(idxPath)
	if !filepath.IsAbs(idxPath) {
		idxPath = filepath.Join(s.repo, idxPath)
	}
	// Racy-git guard: a file modified in the same instant the index was
	// written can match its recorded stat with different content.
	var idxMtime time.Time
	if fi, err := os.Stat(idxPath); err == nil {
		idxMtime = fi.ModTime()
	}
	index, err := readTurnIndex(ctx, s.repo)
	if err != nil {
		return s, err
	}

	// The daemon reads every file itself — never git with a worktree path — so
	// it can refuse anything that would reach OUTSIDE the repository: a
	// symlinked parent directory, a quoted / ".." / control-character path
	// (git would decode "\057etc\057…" from --stdin-paths), or the final
	// component being a symlink (link text is stored, never followed). Content
	// is staged in a private temp dir and only those staged copies are hashed.
	repoReal, err := filepath.EvalSymlinks(s.repo)
	if err != nil {
		return s, err
	}
	// Every file is opened through an os.Root, which refuses atomically to
	// resolve any path component outside the repository, so a directory
	// swapped for a symlink between checks can never leak an outside file.
	root, err := os.OpenRoot(repoReal)
	if err != nil {
		return s, err
	}
	defer root.Close()
	stage, err := os.MkdirTemp("", "solu-turn-blobs-*")
	if err != nil {
		return s, err
	}
	defer os.RemoveAll(stage)

	type item struct{ mode, sha, path string }
	var items []item
	var stagedPaths, stagedModes, stagedRels []string
	var stagedBytes int64
	stageData := func(rel, mode string, data []byte) error {
		name := filepath.Join(stage, strconv.Itoa(len(stagedPaths)))
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return err
		}
		stagedPaths = append(stagedPaths, name)
		stagedModes = append(stagedModes, mode)
		stagedRels = append(stagedRels, rel)
		stagedBytes += int64(len(data))
		return nil
	}
	// keep falls back to what the index already recorded for a path we will
	// not (or cannot safely) read, so the snapshot shows it unchanged.
	keep := func(rel string, known *turnIndexEntry) {
		s.skipped++
		if known != nil {
			s.trackedUnread++
		}
		if known != nil && known.sha != "" {
			items = append(items, item{known.mode, known.sha, rel})
		}
	}

	addFile := func(rel string, known *turnIndexEntry, maxBytes int64) error {
		if !turnSafeRel(rel) {
			// Not representable safely (and update-index would reject it,
			// failing the whole snapshot): leave it out, count it skipped.
			s.skipped++
			return nil
		}
		abs := filepath.Join(repoReal, rel)
		info, err := root.Lstat(filepath.FromSlash(rel))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				keep(rel, known) // escapes the repo or unreadable: show as recorded
			}
			return nil // deleted in the worktree
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Only the link TEXT is stored. Readlink by path is safe because
			// the parent is re-checked; a race can at most leak link text.
			if !turnParentInside(repoReal, rel) {
				keep(rel, known)
				return nil
			}
			target, err := os.Readlink(abs)
			if err != nil {
				return nil
			}
			return stageData(rel, "120000", []byte(target))
		case info.Mode().IsRegular():
			mode := "100644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			if known != nil && known.sha != "" && known.mode == mode && known.size == info.Size() &&
				known.mtimeS == info.ModTime().Unix() && known.mtimeNs == int64(info.ModTime().Nanosecond()) &&
				info.ModTime().Before(idxMtime) {
				items = append(items, item{mode, known.sha, rel})
				return nil
			}
			if info.Size() > maxBytes || stagedBytes+info.Size() > turnSnapshotMaxBytes {
				keep(rel, known)
				return nil
			}
			data, err := turnReadInRoot(root, rel, maxBytes)
			if err != nil {
				keep(rel, known)
				return nil
			}
			return stageData(rel, mode, data)
		}
		return nil
	}

	for rel, e := range index {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		e := e
		if e.mode == "160000" { // submodule: keep the recorded commit
			s.trackedUnread++
			items = append(items, item{e.mode, e.sha, rel})
			continue
		}
		if err := addFile(rel, &e, turnTrackedMaxBytes); err != nil {
			return s, err
		}
	}
	untracked, err := turnGit(ctx, s.repo, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return s, err
	}
	n := 0
	for _, rel := range strings.Split(strings.TrimRight(untracked, "\x00"), "\x00") {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if rel == "" {
			continue
		}
		abs := filepath.Join(s.repo, rel)
		if secretPathRefusal(abs) != "" {
			s.skipped++
			s.secretSkipped++
			continue
		}
		if n >= turnUntrackedMaxFiles {
			s.skipped++
			continue
		}
		n++
		if err := addFile(rel, nil, turnUntrackedMaxBytes); err != nil {
			return s, err
		}
	}

	if len(stagedPaths) > 0 {
		// Only our own staged files (plain temp names) are handed to git.
		out, err := turnGit(ctx, s.repo, nil, strings.NewReader(strings.Join(stagedPaths, "\n")+"\n"),
			"hash-object", "-w", "--no-filters", "--stdin-paths")
		if err != nil {
			return s, err
		}
		shas := strings.Fields(out)
		if len(shas) != len(stagedPaths) {
			return s, fmt.Errorf("hash-object returned %d ids for %d files", len(shas), len(stagedPaths))
		}
		for i, rel := range stagedRels {
			items = append(items, item{stagedModes[i], shas[i], rel})
		}
	}

	tmp, err := os.CreateTemp("", "solu-turn-index-*")
	if err != nil {
		return s, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	os.Remove(tmpPath) // absent = empty index; git creates it
	defer os.Remove(tmpPath)
	defer os.Remove(tmpPath + ".lock")
	env := []string{"GIT_INDEX_FILE=" + tmpPath}
	var info bytes.Buffer
	for _, it := range items {
		fmt.Fprintf(&info, "%s %s\t%s\x00", it.mode, it.sha, it.path)
	}
	if info.Len() > 0 {
		if _, err := turnGit(ctx, s.repo, env, &info, "update-index", "-z", "--index-info"); err != nil {
			return s, err
		}
	}
	tree, err := turnGit(ctx, s.repo, env, nil, "write-tree")
	if err != nil {
		return s, err
	}
	s.tree = strings.TrimSpace(tree)
	return s, nil
}

func (gitTurnSnapshotter) stat(ctx context.Context, repo, base, end string) (files, added, removed int, err error) {
	if base == end {
		return 0, 0, 0, nil
	}
	out, err := turnGit(ctx, repo, nil, nil, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--submodule=short", "--numstat", base, end)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		files++
		if a, err := strconv.Atoi(parts[0]); err == nil {
			added += a
		}
		if r, err := strconv.Atoi(parts[1]); err == nil {
			removed += r
		}
	}
	return files, added, removed, nil
}

type turnActiveCheckpoint struct {
	Turn     providerTurn `json:"turn"`
	Repo     string       `json:"repo,omitempty"`
	BaseTree string       `json:"baseTree,omitempty"`
	Branch   string       `json:"branch,omitempty"`
	Skipped  int          `json:"skipped,omitempty"`
	Note     string       `json:"note,omitempty"`
}
