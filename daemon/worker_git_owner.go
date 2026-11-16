package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ST-251: a new linked worktree that is dirty or unpushed holds a finished
// worker open only when it can be the worker's own. Git cannot say who made a
// worktree, so the archive check excuses one only on positive evidence that it
// is not the worker's; anything it cannot determine still holds the worker.

// workerGitOwnership is what the archive check knows about who else may own a
// new worktree. The zero value excuses nothing.
type workerGitOwnership struct {
	// FinalAt is when the worker's final reply completed, set only when that
	// reply is final and the pane has had no input since it (an idle,
	// finished worker cannot have made a worktree after it). Zero otherwise.
	FinalAt time.Time
	// Peers lists the start checkouts of other live panes. It is called at
	// most once per check (each check works on its own copy), only when a new
	// worktree would hold the worker.
	Peers func() []workerGitPeer
	// Excused, when not nil, receives each unsaved new worktree the check
	// excused: path → its state and the rule that excused it. The archive
	// tells the lead about them (workerNoteExcusedWorktrees).
	Excused map[string]string

	peers     []workerGitPeer
	peersRead bool
}

// workerGitPeer is the start checkout of another live pane.
type workerGitPeer struct {
	Dir string
	// Worker is set for a pane that can close by itself (a worker), and
	// Start is its start snapshot (nil when it has none). A pane that never
	// closes by itself (a lead, the person's own pane) answers for its
	// worktree unconditionally; a worker only while its own archive check
	// is certain to hold it for the same state (workerGitPeerHolds).
	Worker bool
	Start  *workerGitStart
}

// workerGitCreatedSlack is how much later than the final reply a worktree must
// have been created to count as made after it: timestamps come from the
// agent's transcript and the file system, which may round differently.
const workerGitCreatedSlack = 2 * time.Second

// The two rules, as the lead's notice names them.
const (
	workerGitExcusedPeer  = "a: another live pane started there"
	workerGitExcusedLater = "b: made after its final reply"
)

// Seams for tests; the real birth time and file-system check otherwise.
var (
	workerGitBirth      = workerGitBirthTime
	workerGitClockLocal = workerGitLocalClock
)

// workerGitNotOwn reports whether the new worktree at path, whose inspection
// gave proof and reason, provably is not this worker's: it was created after
// the worker's final reply with no input since, or another live pane works in
// it and will itself stay open for this state. paths are all the repository's
// worktrees, from workerWorktrees.
func workerGitNotOwn(ctx context.Context, snap PaneSnapshot, start *workerGitStart, path string, paths []string, proof workerGitProof, reason string, own *workerGitOwnership) bool {
	// Excuse only on positive evidence, for plain dirt or plain unpushed
	// work only; any other reason (no pushed upstream, submodule work, a
	// status, index or path that cannot be verified, an unreadable
	// repository) holds the worker under either rule.
	state := workerGitPlainState(reason)
	if state == "" {
		return false
	}
	excuse := func(rule string) bool {
		if own.Excused != nil {
			own.Excused[path] = state + "; " + rule
		}
		return true
	}
	if !own.FinalAt.IsZero() {
		if created, ok := workerGitWorktreeCreated(ctx, path, snap.Cwd); ok && created.After(own.FinalAt.Add(workerGitCreatedSlack)) {
			return excuse(workerGitExcusedLater)
		}
	}
	if own.Peers == nil {
		return false
	}
	if !own.peersRead {
		own.peers, own.peersRead = own.Peers(), true
	}
	for _, peer := range own.peers {
		if workerGitWorktreeOf(peer.Dir, paths) == path && workerGitPeerHolds(ctx, path, peer, start, proof, reason) {
			return excuse(workerGitExcusedPeer)
		}
	}
	return false
}

// workerGitPlainState names the two reasons a new worktree may be excused
// for, exactly as workerGitInspectWith reports them: "dirty" for plain
// uncommitted work, "unpushed" for plain unpushed work, "" for anything else.
func workerGitPlainState(reason string) string {
	switch reason {
	case "uncommitted work":
		return "dirty"
	case "unpushed work":
		return "unpushed"
	}
	return ""
}

// workerGitWorktreeCreated is the earliest of the birth times of the linked
// worktree at path and of its own admin entry under the repository's common
// dir (git worktree add makes both). Taking the earlier one means a moved or
// re-made folder can only make the worktree read older, which keeps holding.
// ok is false when either is unknown: not recorded, zero, later than now
// plus the slack, or on a file system whose clock may not be this machine's
// (workerGitLocalClock: a Windows drive under WSL, a network share).
func workerGitWorktreeCreated(ctx context.Context, path, cwd string) (time.Time, bool) {
	path = filepath.Clean(path)
	if st, err := os.Lstat(path); err != nil || !st.IsDir() {
		return time.Time{}, false
	}
	common := workerGitCommonDir(ctx, cwd)
	if common == "" {
		return time.Time{}, false
	}
	admin, err := workerGitAdminEntry(ctx, path, common)
	if err != nil {
		return time.Time{}, false
	}
	latest := time.Now().Add(workerGitCreatedSlack)
	var created time.Time
	for _, p := range []string{path, admin} {
		if !workerGitClockLocal(p) {
			return time.Time{}, false
		}
		born, ok := workerGitBirth(p)
		if !ok || born.IsZero() || born.Unix() <= 0 || born.After(latest) {
			return time.Time{}, false
		}
		if created.IsZero() || born.Before(created) {
			created = born
		}
	}
	return created, true
}

// workerGitWindowsMount reports whether path is under WSL's mount of a
// Windows drive, /mnt/<letter>, whatever file system serves it.
func workerGitWindowsMount(path string) bool {
	rest, ok := strings.CutPrefix(filepath.ToSlash(filepath.Clean(path)), "/mnt/")
	if !ok || rest == "" {
		return false
	}
	c := rest[0]
	return ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) && (len(rest) == 1 || rest[1] == '/')
}

// workerGitWorktreeOf is the worktree in paths that contains dir: the deepest
// one, so a worktree nested in another is its own. "" when none does.
func workerGitWorktreeOf(dir string, paths []string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	dir = filepath.Clean(dir)
	best := ""
	for _, p := range paths {
		p = filepath.Clean(p)
		rel, err := filepath.Rel(p, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if len(p) > len(best) {
			best = p
		}
	}
	return best
}

// workerGitPeerHolds reports whether peer, a live pane working in the new
// worktree at path, will itself stay open while the worktree is in the state
// this worker's inspection found (proof, reason), so it answers for it.
func workerGitPeerHolds(ctx context.Context, path string, peer workerGitPeer, start *workerGitStart, proof workerGitProof, reason string) bool {
	if !peer.Worker {
		return true
	}
	base := peer.Start
	if base == nil {
		// No start snapshot: its own check is strict clean-and-pushed.
		return true
	}
	// Its check honours info/exclude only while the file is unchanged since
	// its own spawn; it must never hide a file this worker's check sees.
	if base.ExcludeSHA != "" && base.ExcludeSHA != start.ExcludeSHA {
		return false
	}
	if base.Cwd != peer.Dir {
		// It excuses its start state only for its start checkout.
		return true
	}
	// Mirror workerGitForArchive's excuse of the start state: dirt that is
	// all within the start dirt, and an unpushed HEAD that is the start HEAD
	// or is contained in a remote ref. Only the two plain reasons get here
	// (workerGitNotOwn); anything else is not answered for.
	switch reason {
	case "uncommitted work":
		return base.Proof.Dirty == ""
	case "unpushed work":
		return proof.Head != "" && proof.Head != base.Proof.Head && !workerGitCachedPublishedHead(ctx, path, proof.Head)
	}
	return false
}
