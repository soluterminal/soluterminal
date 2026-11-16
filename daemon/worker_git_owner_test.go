package main

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ST-251: a new worktree holds a finished worker only when it can be the
// worker's own.

// newDirtyWorktree adds a linked worktree on a branch tracking origin/main and
// leaves an uncommitted change in it.
func newDirtyWorktree(t *testing.T, cwd, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	archiveGit(t, cwd, "worktree", "add", "-b", name, path)
	archiveGit(t, path, "branch", "--set-upstream-to=origin/main")
	putCapabilityFile(t, filepath.Join(path, "tracked.txt"), name+" change\n")
	return path
}

func peersOf(peers ...workerGitPeer) workerGitOwnership {
	return workerGitOwnership{Peers: func() []workerGitPeer { return peers }}
}

func needBirthTimes(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if _, ok := workerGitBirthTime(dir); !ok {
		t.Skip("this file system records no birth times")
	}
	if !workerGitLocalClock(dir) {
		t.Skip("the temporary directory is not on a listed local-disk file system")
	}
}

func TestWorkerGitOtherPanesWorktreeDoesNotHoldWorker(t *testing.T) {
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	other := newDirtyWorktree(t, cwd, "other")
	want := "uncommitted work (new worktree: " + other + ")"
	ctx := context.Background()
	for name, c := range map[string]struct {
		own  workerGitOwnership
		hold bool
	}{
		"no ownership known":       {workerGitOwnership{}, true},
		"lead or person's pane":    {peersOf(workerGitPeer{Dir: other}), false},
		"pane in a subdirectory":   {peersOf(workerGitPeer{Dir: filepath.Join(other, "sub")}), false},
		"pane in another checkout": {peersOf(workerGitPeer{Dir: cwd}, workerGitPeer{Dir: t.TempDir()}), true},
		"relative directory":       {peersOf(workerGitPeer{Dir: "other"}), true},
		"worker without baseline":  {peersOf(workerGitPeer{Dir: other, Worker: true}), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, why := workerGitForArchiveOwned(ctx, snap, "", c.own)
			if c.hold && why != want {
				t.Fatalf("why=%q, want %q", why, want)
			}
			if !c.hold && why != "" {
				t.Fatalf("another pane's worktree held the worker: %q", why)
			}
		})
	}
}

// A worker peer answers only while its own archive check is certain to hold
// it for the same state: its start snapshot must not excuse that state.
func TestWorkerGitWorkerPeerAnswersOnlyForStateItWouldKeep(t *testing.T) {
	ctx := context.Background()
	t.Run("dirt", func(t *testing.T) {
		cwd := archiveRepo(t)
		snap := workerGitBaseline(t, cwd)
		other := filepath.Join(t.TempDir(), "other")
		archiveGit(t, cwd, "worktree", "add", "-b", "other", other)
		archiveGit(t, other, "branch", "--set-upstream-to=origin/main")
		clean := captureWorkerGitStart(ctx, other)
		putCapabilityFile(t, filepath.Join(other, "tracked.txt"), "dirt\n")
		dirty := captureWorkerGitStart(ctx, other)
		if clean == nil || dirty == nil {
			t.Fatal("baseline unavailable")
		}
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: clean})); why != "" {
			t.Fatal("a worker started clean in the worktree did not answer for new dirt:", why)
		}
		// Started with the same dirt: its own check excuses it, so it cannot answer.
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: dirty})); !strings.Contains(why, "new worktree: "+other) {
			t.Fatal("a worker whose start excuses the dirt answered for it:", why)
		}
		// Its exclude file is honoured where this worker's is not.
		odd := *clean
		odd.ExcludeSHA = "different"
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: &odd})); !strings.Contains(why, "new worktree: "+other) {
			t.Fatal("a worker with another exclude baseline answered:", why)
		}
		// A start snapshot of another checkout excuses nothing here.
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: snap.Origin.WorkerGit})); why != "" {
			t.Fatal("a worker strict about the worktree did not answer:", why)
		}
	})
	t.Run("unpushed", func(t *testing.T) {
		cwd := archiveRepo(t)
		snap := workerGitBaseline(t, cwd)
		other := filepath.Join(t.TempDir(), "other")
		archiveGit(t, cwd, "worktree", "add", "-b", "other", other)
		archiveGit(t, other, "branch", "--set-upstream-to=origin/main")
		before := captureWorkerGitStart(ctx, other)
		putCapabilityFile(t, filepath.Join(other, "tracked.txt"), "commit\n")
		archiveGit(t, other, "commit", "-am", "unpushed")
		after := captureWorkerGitStart(ctx, other)
		if before == nil || after == nil {
			t.Fatal("baseline unavailable")
		}
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: before})); why != "" {
			t.Fatal("a worker that started before the commit did not answer for it:", why)
		}
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: other, Worker: true, Start: after})); why != "unpushed work (new worktree: "+other+")" {
			t.Fatal("a worker whose start HEAD is the unpushed commit answered for it:", why)
		}
	})
}

func TestWorkerGitOwnNewWorktreeStillHoldsWorker(t *testing.T) {
	needBirthTimes(t)
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	own := newDirtyWorktree(t, cwd, "mine")
	// Made before the final reply, and no other pane works in it.
	ownership := peersOf(workerGitPeer{Dir: cwd}, workerGitPeer{Dir: t.TempDir()})
	ownership.FinalAt = time.Now().Add(time.Minute)
	if _, why := workerGitForArchiveOwned(context.Background(), snap, "", ownership); why != "uncommitted work (new worktree: "+own+")" {
		t.Fatal(why)
	}
	archiveGit(t, own, "commit", "-am", "mine")
	if _, why := workerGitForArchiveOwned(context.Background(), snap, "", ownership); why != "unpushed work (new worktree: "+own+")" {
		t.Fatal(why)
	}
}

func TestWorkerGitWorktreeMadeAfterFinalReplyDoesNotHoldWorker(t *testing.T) {
	needBirthTimes(t)
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	final := time.Now().Add(-time.Minute)
	later := newDirtyWorktree(t, cwd, "later")
	ctx := context.Background()
	if _, why := workerGitForArchiveOwned(ctx, snap, "", workerGitOwnership{FinalAt: final}); why != "" {
		t.Fatal("a worktree made after the final reply held the worker:", why)
	}
	// Within the slack of the final reply it may be the worker's own.
	if _, why := workerGitForArchiveOwned(ctx, snap, "", workerGitOwnership{FinalAt: time.Now()}); !strings.Contains(why, "new worktree: "+later) {
		t.Fatal("a worktree made at the final reply was excused:", why)
	}
	// No final reply known (input since, or not final): it still holds.
	if _, why := workerGitForArchiveOwned(ctx, snap, "", workerGitOwnership{}); !strings.Contains(why, "new worktree: "+later) {
		t.Fatal(why)
	}
}

func TestWorkerGitUndeterminedWorktreeStillHoldsWorker(t *testing.T) {
	needBirthTimes(t)
	ctx := context.Background()
	final := workerGitOwnership{FinalAt: time.Now().Add(-time.Minute)}
	t.Run("folder deleted with an unpushed commit", func(t *testing.T) {
		cwd := archiveRepo(t)
		snap := workerGitBaseline(t, cwd)
		gone := newDirtyWorktree(t, cwd, "gone")
		archiveGit(t, gone, "commit", "-am", "unpushed")
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		if _, why := workerGitForArchiveOwned(ctx, snap, "", final); !strings.Contains(why, "new worktree: "+gone) {
			t.Fatal("an unreadable worktree was excused:", why)
		}
		// Nor does a pane "in" it answer when its state could not be read.
		ownership := peersOf(workerGitPeer{Dir: gone, Worker: true, Start: &workerGitStart{Cwd: gone}})
		if _, why := workerGitForArchiveOwned(ctx, snap, "", ownership); !strings.Contains(why, "new worktree: "+gone) {
			t.Fatal("a worker peer answered for unread state:", why)
		}
	})
	t.Run("admin entry not its own", func(t *testing.T) {
		cwd := archiveRepo(t)
		snap := workerGitBaseline(t, cwd)
		wt := newDirtyWorktree(t, cwd, "crafted")
		common := workerGitCommonDir(ctx, cwd)
		putCapabilityFile(t, filepath.Join(common, "worktrees", "crafted", "gitdir"), filepath.Join(t.TempDir(), ".git")+"\n")
		if _, ok := workerGitWorktreeCreated(ctx, wt, cwd); ok {
			t.Fatal("creation time read through an entry that does not point back")
		}
		if _, ok := workerGitWorktreeCreated(ctx, filepath.Join(wt, "sub"), cwd); ok {
			t.Fatal("creation time read for a directory that is not a worktree root")
		}
		_ = snap
	})
	t.Run("nested worktree's pane does not answer for its parent", func(t *testing.T) {
		cwd := archiveRepo(t)
		snap := workerGitBaseline(t, cwd)
		parent := newDirtyWorktree(t, cwd, "parent")
		nested := filepath.Join(parent, "nested")
		archiveGit(t, cwd, "worktree", "add", "-b", "nested", nested)
		if _, why := workerGitForArchiveOwned(ctx, snap, "", peersOf(workerGitPeer{Dir: nested})); !strings.Contains(why, "new worktree: "+parent) {
			t.Fatal(why)
		}
	})
}

func TestWorkerGitWorktreeOf(t *testing.T) {
	paths := []string{"/r", "/r/.claude/worktrees/a", "/r/.claude/worktrees/ab"}
	for dir, want := range map[string]string{
		"/r/.claude/worktrees/a":       "/r/.claude/worktrees/a",
		"/r/.claude/worktrees/a/x/y":   "/r/.claude/worktrees/a",
		"/r/.claude/worktrees/ab/":     "/r/.claude/worktrees/ab",
		"/r/.claude/worktrees/abc":     "/r",
		"/r/src":                       "/r",
		"/elsewhere":                   "",
		"r/.claude/worktrees/a":        "",
		"":                             "",
		"/r/.claude/worktrees/a/../ab": "/r/.claude/worktrees/ab",
	} {
		if got := workerGitWorktreeOf(dir, paths); got != want {
			t.Errorf("%q: got %q, want %q", dir, got, want)
		}
	}
}

// End to end through tryArchiveWorker: the final reply dates the worker, and
// the pane list supplies peers (exited panes and process cwds do not answer).
func TestWorkerArchiveIgnoresWorktreesThatAreNotTheWorkers(t *testing.T) {
	needBirthTimes(t)
	setup := func(t *testing.T) (*apiServer, *Pane, time.Time) {
		a, p, now := archiveFixture(t)
		start := workerGitBaseline(t, p.Cwd).Origin.WorkerGit
		p.mu.Lock()
		o := p.Origin.clone()
		o.WorkerGit = start
		p.Origin = o
		p.mu.Unlock()
		a.workerArchives().liveCwd = func(*Pane) string { return "" }
		return a, p, now
	}
	// Move the final reply after the worktree was made, so only a peer can
	// excuse it, and before the lead's read receipt.
	replyAfterNow := func(t *testing.T, a *apiServer, p *Pane) time.Time {
		now := time.Now().Add(time.Minute)
		raw := fmt.Sprintf("{\"type\":\"user\",\"promptSource\":\"typed\",\"timestamp\":%q,\"message\":{\"content\":\"task\"}}\n{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"Final answer\"}]}}\n", time.Now().Add(-time.Second).Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano))
		putCapabilityFile(t, p.Snapshot().ClaudeTranscriptPath, raw)
		return now
	}
	peer := func(a *apiServer, id, cwd string, mutate func(*Pane)) {
		q := testPaneWithTail(t, id, id, "")
		q.Cwd = cwd
		q.Origin = &PaneOrigin{Source: paneOriginUser}
		if mutate != nil {
			mutate(q)
		}
		a.mgr.mu.Lock()
		a.mgr.panes[id] = q
		a.mgr.mu.Unlock()
	}
	archived := func(a *apiServer, p *Pane, now time.Time) bool {
		a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
		a.tryArchiveWorker(p, now)
		_, live := a.mgr.Get(p.ID)
		return !live
	}
	t.Run("made after the final reply", func(t *testing.T) {
		a, p, now := setup(t)
		newDirtyWorktree(t, p.Cwd, "later")
		if !archived(a, p, now) {
			t.Fatal("a worktree made after the final reply kept the worker open")
		}
	})
	t.Run("own worktree", func(t *testing.T) {
		a, p, _ := setup(t)
		newDirtyWorktree(t, p.Cwd, "mine")
		if archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("the worker's own dirty worktree did not keep it open")
		}
	})
	t.Run("person's pane works in it", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		peer(a, "person", other, nil)
		if !archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("another live pane's worktree kept the worker open")
		}
	})
	t.Run("person's shell only passes through it", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		peer(a, "person", t.TempDir(), nil)
		a.workerArchives().liveCwd = func(q *Pane) string {
			if q.ID == "person" {
				return other
			}
			return ""
		}
		if archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("a pane's passing process cwd answered for a worktree")
		}
	})
	for kind, origin := range map[string]*PaneOrigin{
		"an agent's helper pane": {Source: paneOriginAgent},
		"an API pane":            {Source: paneOriginAPI},
		"a pane with no origin":  nil,
	} {
		t.Run(kind, func(t *testing.T) {
			a, p, _ := setup(t)
			other := newDirtyWorktree(t, p.Cwd, "other")
			peer(a, "helper", other, func(q *Pane) { q.Origin = origin })
			if archived(a, p, replyAfterNow(t, a, p)) {
				t.Fatalf("%s answered for a worktree", kind)
			}
		})
	}
	t.Run("a lead's pane", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		peer(a, "sublead", other, func(q *Pane) { q.Origin = &PaneOrigin{Source: paneOriginAgent, TeamKind: paneTeamLead, LeadRole: "x"} })
		if !archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("a lead's pane did not answer for its worktree")
		}
	})
	t.Run("final reply from before the daemon restarted", func(t *testing.T) {
		a, p, now := setup(t)
		q := a.workers()
		q.mu.Lock()
		q.startedAt = now.Add(-30 * time.Second) // after the final reply
		q.mu.Unlock()
		newDirtyWorktree(t, p.Cwd, "later")
		if archived(a, p, now) {
			t.Fatal("rule (b) dated a worktree by a final reply from before the restart")
		}
	})
	t.Run("exited pane", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		peer(a, "person", other, func(q *Pane) { q.Status = PaneExited })
		if archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("an exited pane answered for a worktree")
		}
	})
	t.Run("another worker's process cwd", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		peer(a, "sibling", t.TempDir(), func(q *Pane) {
			q.Origin = &PaneOrigin{Source: paneOriginAgent, TeamKind: paneTeamWorker}
		})
		a.workerArchives().liveCwd = func(q *Pane) string {
			if q.ID == "sibling" {
				return other
			}
			return ""
		}
		if archived(a, p, replyAfterNow(t, a, p)) {
			t.Fatal("a worker's passing process cwd answered for a worktree")
		}
	})
}

// @secure condition 3: a birth time that is missing, zero or later than now
// plus the slack is undeterminable and holds the worker.
func TestWorkerGitImplausibleBirthTimeHoldsWorker(t *testing.T) {
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	later := newDirtyWorktree(t, cwd, "later")
	ctx := context.Background()
	own := workerGitOwnership{FinalAt: time.Now().Add(-time.Hour)}
	real := workerGitBirth
	t.Cleanup(func() { workerGitBirth = real })
	for name, born := range map[string]func(string) (time.Time, bool){
		"not recorded": func(string) (time.Time, bool) { return time.Time{}, false },
		"zero":         func(string) (time.Time, bool) { return time.Time{}, true },
		"epoch":        func(string) (time.Time, bool) { return time.Unix(0, 0), true },
		"future":       func(string) (time.Time, bool) { return time.Now().Add(time.Minute), true },
		"one future": func(p string) (time.Time, bool) {
			return time.Now().Add(map[bool]time.Duration{true: time.Hour}[p != later]), true
		},
	} {
		t.Run(name, func(t *testing.T) {
			workerGitBirth = born
			if _, ok := workerGitWorktreeCreated(ctx, later, cwd); ok {
				t.Fatal("an implausible birth time was accepted")
			}
			if _, why := workerGitForArchiveOwned(ctx, snap, "", own); why != "uncommitted work (new worktree: "+later+")" {
				t.Fatal("an implausible birth time excused the worktree:", why)
			}
		})
	}
	workerGitBirth = func(string) (time.Time, bool) { return time.Now().Add(-2 * time.Hour), true }
	if _, why := workerGitForArchiveOwned(ctx, snap, "", own); !strings.Contains(why, "new worktree: "+later) {
		t.Fatal("a plausible birth before the final reply excused the worktree:", why)
	}
	workerGitBirth = func(string) (time.Time, bool) { return time.Now().Add(time.Second), true }
	if _, why := workerGitForArchiveOwned(ctx, snap, "", own); why != "" {
		t.Fatal("a plausible birth after the final reply, within the slack of now, held:", why)
	}
}

// @secure condition 4: rule (b) never applies on a Windows drive mounted into
// WSL (or any file system whose clock may not be this machine's); only rule
// (a) may excuse such a worktree.
func TestWorkerGitTimeRuleNeedsLocalClock(t *testing.T) {
	for path, want := range map[string]bool{
		"/mnt/c":                         true,
		"/mnt/c/Users/x/repo":            true,
		"/mnt/D/work":                    true,
		"/mnt/wsl/shared":                false,
		"/mnt/cc/x":                      false,
		"/mnt":                           false,
		"/home/x/mnt/c/repo":             false,
		"/mnt/c/../../home/x":            false,
		"/home/x/repo/.claude/worktrees": false,
	} {
		if got := workerGitWindowsMount(path); got != want {
			t.Errorf("workerGitWindowsMount(%q) = %v", path, got)
		}
	}
	if runtime.GOOS == "linux" {
		if workerGitLocalClock("/proc") {
			t.Error("procfs counted as a local-disk clock")
		}
		if st, err := os.Stat("/mnt/c"); err == nil && st.IsDir() && workerGitLocalClock("/mnt/c") {
			t.Error("/mnt/c counted as a local-disk clock")
		}
	}

	needBirthTimes(t)
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	later := newDirtyWorktree(t, cwd, "later")
	ctx := context.Background()
	real := workerGitClockLocal
	t.Cleanup(func() { workerGitClockLocal = real })
	for _, which := range []string{"worktree", "admin entry"} {
		workerGitClockLocal = func(p string) bool {
			if which == "worktree" {
				return p != later
			}
			return p == later
		}
		own := workerGitOwnership{FinalAt: time.Now().Add(-time.Minute)}
		if _, why := workerGitForArchiveOwned(ctx, snap, "", own); why != "uncommitted work (new worktree: "+later+")" {
			t.Fatalf("%s off the local clock: rule (b) excused it: %q", which, why)
		}
		own.Peers = func() []workerGitPeer { return []workerGitPeer{{Dir: later}} }
		if _, why := workerGitForArchiveOwned(ctx, snap, "", own); why != "" {
			t.Fatalf("%s off the local clock: rule (a) no longer excused it: %q", which, why)
		}
	}
}

// @secure condition 2: archiving past an excused, unsaved worktree leaves the
// lead a worker notice naming it and the rule; it survives the worker's
// cleanup and closes once the worktree is saved, or when dismissed.
func TestWorkerArchiveTellsLeadAboutExcusedWorktrees(t *testing.T) {
	needBirthTimes(t)
	setup := func(t *testing.T) (*apiServer, *Pane, time.Time) {
		a, p, now := archiveFixture(t)
		start := workerGitBaseline(t, p.Cwd).Origin.WorkerGit
		p.mu.Lock()
		o := p.Origin.clone()
		o.WorkerGit = start
		p.Origin = o
		p.mu.Unlock()
		a.workerArchives().liveCwd = func(*Pane) string { return "" }
		return a, p, now
	}
	archive := func(t *testing.T, a *apiServer, p *Pane, now time.Time) {
		a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
		a.tryArchiveWorker(p, now)
		if _, live := a.mgr.Get(p.ID); live {
			t.Fatal("the worker was not archived")
		}
	}
	notice := func(a *apiServer, worker string) (Ask, bool) {
		return a.workerExcusedAsk(workerExcusedAskKey(worker))
	}
	t.Run("rule b, then saved", func(t *testing.T) {
		a, p, now := setup(t)
		later := newDirtyWorktree(t, p.Cwd, "later")
		archive(t, a, p, now)
		x, ok := notice(a, p.ID)
		if !ok || x.SourcePane != "lead" || x.Kind != askKindWorker || x.WorkerPane != p.ID || x.NeedsUser {
			t.Fatalf("no quiet worker notice on the lead: %+v", x)
		}
		if !strings.Contains(x.Text, workerExcusedShown(later)+" (dirty; "+workerGitExcusedLater+")") {
			t.Fatal(x.Text)
		}
		// The worker's own cleanup and the lead's notice sync leave it open.
		a.asksPaneClosed(p.ID)
		a.reconcileWorkerRetentions(true)
		a.asks.closeWorkerNotice(p.ID)
		if _, ok := notice(a, p.ID); !ok {
			t.Fatal("the notice closed with the worker's retention")
		}
		// Still unsaved: it stays. Pushed and clean: it closes.
		a.recheckWorkerExcused(context.Background(), now.Add(time.Minute))
		if _, ok := notice(a, p.ID); !ok {
			t.Fatal("the notice closed while the worktree was unsaved")
		}
		archiveGit(t, later, "commit", "-am", "saved")
		archiveGit(t, later, "push", "origin", "HEAD:refs/heads/later")
		archiveGit(t, later, "branch", "--set-upstream-to=origin/later")
		a.recheckWorkerExcused(context.Background(), now.Add(2*time.Minute))
		if _, ok := notice(a, p.ID); ok {
			t.Fatal("the notice stayed after the worktree was saved")
		}
		s := a.workerArchives()
		s.noticeMu.Lock()
		_, kept := s.excused[p.ID]
		s.noticeMu.Unlock()
		if kept {
			t.Fatal("the record outlived its notice")
		}
	})
	t.Run("rule a, survives restart, dismissed", func(t *testing.T) {
		a, p, _ := setup(t)
		other := newDirtyWorktree(t, p.Cwd, "other")
		gone := newDirtyWorktree(t, p.Cwd, "gone")
		for id, dir := range map[string]string{"person": other, "person2": gone} {
			q := testPaneWithTail(t, id, id, "")
			q.Cwd = dir
			q.Origin = &PaneOrigin{Source: paneOriginUser}
			a.mgr.mu.Lock()
			a.mgr.panes[id] = q
			a.mgr.mu.Unlock()
		}
		now := time.Now().Add(time.Minute)
		raw := fmt.Sprintf("{\"type\":\"user\",\"promptSource\":\"typed\",\"timestamp\":%q,\"message\":{\"content\":\"task\"}}\n{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"Final answer\"}]}}\n", time.Now().Add(-time.Second).Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano))
		putCapabilityFile(t, p.Snapshot().ClaudeTranscriptPath, raw)
		archive(t, a, p, now)
		x, ok := notice(a, p.ID)
		s := a.workerArchives()
		s.noticeMu.Lock()
		paths := s.excused[p.ID].Paths
		s.noticeMu.Unlock()
		want := "dirty; " + workerGitExcusedPeer
		if !ok || paths[other] != want || paths[gone] != want || len(paths) != 2 {
			t.Fatalf("notice %+v, record %v", x, paths)
		}
		if !strings.Contains(x.Text, workerExcusedShown(other)+" ("+want+")") && !strings.Contains(x.Text, workerExcusedShown(gone)+" ("+want+")") {
			t.Fatal(x.Text)
		}
		// A worktree whose folder is gone drops out; the rest keeps the notice.
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		// A restart reloads the record from disk.
		s.noticeMu.Lock()
		s.excused, s.excusedChecked = nil, nil
		s.noticeMu.Unlock()
		a.recheckWorkerExcused(context.Background(), now.Add(time.Minute))
		x, ok = notice(a, p.ID)
		if !ok || strings.Contains(x.Text, workerExcusedShown(gone)) || !strings.Contains(x.Text, workerExcusedShown(other)+" ("+want+")") {
			t.Fatalf("after the folder went: %+v", x)
		}
		// Dismissed: forgotten, not reopened.
		if _, err := a.asks.closeWhere(askDismissed, "person", func(y Ask) bool { return y.ID == x.ID }); err != nil {
			t.Fatal(err)
		}
		a.recheckWorkerExcused(context.Background(), now.Add(3*time.Minute))
		if _, ok := notice(a, p.ID); ok {
			t.Fatal("a dismissed notice came back")
		}
		s.noticeMu.Lock()
		_, kept := s.excused[p.ID]
		s.noticeMu.Unlock()
		if kept {
			t.Fatal("the record of a dismissed notice was kept")
		}
	})
	t.Run("no lead present: stays open", func(t *testing.T) {
		a, p, now := setup(t)
		newDirtyWorktree(t, p.Cwd, "later")
		lead, _ := a.mgr.Get("lead")
		lead.mu.Lock()
		lead.Status = PaneExited
		lead.mu.Unlock()
		a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
		a.tryArchiveWorker(p, now)
		if _, live := a.mgr.Get(p.ID); !live {
			t.Fatal("archived past an excused worktree with no lead to tell")
		}
	})
	t.Run("nothing excused: no notice", func(t *testing.T) {
		a, p, _ := setup(t)
		archive(t, a, p, time.Now().Add(-30*time.Second))
		if _, ok := notice(a, p.ID); ok {
			t.Fatal("a notice without an excused worktree")
		}
	})
}

func TestWorkerExcusedTextFitsTheAskLimit(t *testing.T) {
	note := workerExcusedNote{Label: "#3 @w", Paths: map[string]string{}}
	for i := 0; i < 6; i++ {
		note.Paths[fmt.Sprintf("/srv/repo/.claude/worktrees/some-long-topic-name-%d", i)] = "dirty; " + workerGitExcusedLater
	}
	text := workerExcusedText(note)
	if askText(text) != text || !strings.Contains(text, "-0 (dirty; "+workerGitExcusedLater+")") || !strings.Contains(text, " more") {
		t.Fatal(text)
	}
	one := workerExcusedNote{Label: "#3 @w", Paths: map[string]string{"/srv/r/.claude/worktrees/x": "unpushed; " + workerGitExcusedPeer}}
	if got := workerExcusedText(one); got != "Worker #3 @w archived; unsaved worktrees it was excused from: /srv/r/.claude/worktrees/x (unpushed; "+workerGitExcusedPeer+")" {
		t.Fatal(got)
	}
}

// The notice stays visible: an unsaved one is retried, and one whose pane is
// gone moves to the nearest present lead up the chain.
func TestWorkerExcusedNoticeFindsAPresentLead(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	for _, id := range []string{"lead2", "lead3"} {
		q := testPaneWithTail(t, id, id, "")
		a.mgr.mu.Lock()
		a.mgr.panes[id] = q
		a.mgr.mu.Unlock()
	}
	setStatus := func(id string, st PaneStatus) {
		q, _ := a.mgr.Get(id)
		q.mu.Lock()
		q.Status = st
		q.mu.Unlock()
	}
	dir := t.TempDir() // exists, not a repository: never "saved"
	paths := map[string]string{dir: "dirty; " + workerGitExcusedPeer}
	key := workerExcusedAskKey("pane_w")
	host := func() string {
		x, ok := a.workerExcusedAsk(key)
		if !ok {
			return ""
		}
		return x.SourcePane
	}
	ctx := context.Background()

	// No host present: recorded, not posted, retried later.
	setStatus("lead2", PaneExited)
	setStatus("lead3", PaneExited)
	a.workerRecordExcusedWorktrees([]string{"lead2", "lead3"}, "pane_w", "#9 w", nil, paths)
	a.workerPostExcused("pane_w")
	if host() != "" {
		t.Fatal("posted on an absent pane")
	}
	setStatus("lead3", PaneRunning)
	a.recheckWorkerExcused(ctx, time.Now())
	if host() != "lead3" {
		t.Fatalf("not retried on the present lead: %q", host())
	}
	// A nearer lead coming back does not move it; its pane going does.
	setStatus("lead2", PaneRunning)
	a.recheckWorkerExcused(ctx, time.Now())
	if host() != "lead3" {
		t.Fatalf("moved while its pane was present: %q", host())
	}
	setStatus("lead3", PaneExited)
	a.recheckWorkerExcused(ctx, time.Now())
	if host() != "lead2" {
		t.Fatalf("not moved off a gone pane: %q", host())
	}
	s := a.workerArchives()
	s.noticeMu.Lock()
	n := len(s.excused)
	s.noticeMu.Unlock()
	if n != 1 {
		t.Fatal("record lost")
	}
}

func TestWorkerExcusedSavedNeedsTheFolderAboveIt(t *testing.T) {
	parent := t.TempDir()
	if !workerExcusedSaved(context.Background(), filepath.Join(parent, "removed"), workerExcusedNote{}) {
		t.Fatal("a removed worktree folder under a present folder is not saved")
	}
	if workerExcusedSaved(context.Background(), filepath.Join(parent, "unmounted", "wt"), workerExcusedNote{}) {
		t.Fatal("a worktree whose folder above is missing (unmounted) counted as saved")
	}
	if workerExcusedSaved(context.Background(), parent, workerExcusedNote{}) {
		t.Fatal("an unreadable checkout counted as saved")
	}
}

// The record is saved only when it changes: no file with no records, and no
// rewrite on a pass that changes nothing.
func TestWorkerExcusedSavesOnlyChanges(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	file := filepath.Join(a.dataDir, "worker-excused.json")
	ctx := context.Background()
	a.recheckWorkerExcused(ctx, time.Now())
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a pass with no records wrote the file:", err)
	}
	dir := t.TempDir()
	a.workerRecordExcusedWorktrees([]string{"lead"}, "pane_w", "#9 w", nil, map[string]string{dir: "dirty; " + workerGitExcusedPeer})
	a.workerPostExcused("pane_w")
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a.recheckWorkerExcused(ctx, time.Now()) // not due: nothing changes
	}
	after, err := os.Stat(file)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("an unchanged pass rewrote the file", err)
	}
	if _, ok := a.workerExcusedAsk(workerExcusedAskKey("pane_w")); !ok {
		t.Fatal("notice not posted")
	}
}

// A record made before teardown is dropped when the archive is undone, and a
// recorded but unposted one (a crash before the post) is posted on reload.
func TestWorkerExcusedRecordSurvivesUntilPosted(t *testing.T) {
	needBirthTimes(t)
	a, p, now := archiveFixture(t)
	start := workerGitBaseline(t, p.Cwd).Origin.WorkerGit
	p.mu.Lock()
	o := p.Origin.clone()
	o.WorkerGit = start
	p.Origin = o
	p.mu.Unlock()
	s := a.workerArchives()
	s.liveCwd = func(*Pane) string { return "" }
	newDirtyWorktree(t, p.Cwd, "later")
	s.teardown = func(*Pane) error { return errors.New("teardown fails") }
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	if _, live := a.mgr.Get(p.ID); !live {
		t.Skip("the failed teardown did not restore the pane here")
	}
	s.noticeMu.Lock()
	_, kept := s.excused[p.ID]
	s.noticeMu.Unlock()
	if kept {
		t.Fatal("an undone archive left an excused-worktree record")
	}
	if _, ok := a.workerExcusedAsk(workerExcusedAskKey(p.ID)); ok {
		t.Fatal("an undone archive posted a notice")
	}

	// Recorded, then the daemon stopped before the post.
	dir := t.TempDir()
	a.workerRecordExcusedWorktrees([]string{"lead"}, "pane_crash", "#9 w", start, map[string]string{dir: "dirty; " + workerGitExcusedLater})
	s.noticeMu.Lock()
	s.excused, s.excusedChecked = nil, nil
	s.noticeMu.Unlock()
	a.recheckWorkerExcused(context.Background(), time.Now())
	if _, ok := a.workerExcusedAsk(workerExcusedAskKey("pane_crash")); !ok {
		t.Fatal("a record left unposted by a crash was not posted on reload")
	}
}

// The recheck inspects as the archive check does: files the unchanged
// exclude file ignores do not keep the notice open.
func TestWorkerExcusedRecheckUsesArchiveOptions(t *testing.T) {
	cwd := archiveRepo(t)
	common := workerGitCommonDir(context.Background(), cwd)
	putCapabilityFile(t, filepath.Join(common, "info", "exclude"), "*.scratch\n")
	start := captureWorkerGitStart(context.Background(), cwd)
	if start == nil || start.ExcludeSHA == "" {
		t.Fatal("no exclude baseline")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	archiveGit(t, cwd, "worktree", "add", "--detach", wt, "origin/main")
	putCapabilityFile(t, filepath.Join(wt, "notes.scratch"), "excluded\n")
	note := workerExcusedNote{StartCwd: start.Cwd, ExcludeSHA: start.ExcludeSHA}
	if !workerExcusedSaved(context.Background(), wt, note) {
		t.Fatal("an excluded file kept the worktree unsaved")
	}
	if workerExcusedSaved(context.Background(), wt, workerExcusedNote{}) {
		t.Fatal("without the baseline the excluded file should still count")
	}
	putCapabilityFile(t, filepath.Join(wt, "tracked.txt"), "real change\n")
	if workerExcusedSaved(context.Background(), wt, note) {
		t.Fatal("a real change read as saved")
	}
}

// Round 3: both rules excuse only plain dirt or plain unpushed work; every
// other reason holds the worker, even with evidence that would otherwise
// excuse it under rule (a) and rule (b) at once.
func TestWorkerGitOnlyPlainReasonsAreExcused(t *testing.T) {
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	wt := newDirtyWorktree(t, cwd, "wt")
	paths := []string{cwd, wt}
	realBirth, realClock := workerGitBirth, workerGitClockLocal
	t.Cleanup(func() { workerGitBirth, workerGitClockLocal = realBirth, realClock })
	workerGitBirth = func(string) (time.Time, bool) { return time.Now(), true }
	workerGitClockLocal = func(string) bool { return true }
	ctx := context.Background()
	for _, rule := range []string{"a", "b"} {
		own := func() *workerGitOwnership {
			o := &workerGitOwnership{Excused: map[string]string{}}
			if rule == "a" {
				o.Peers = func() []workerGitPeer { return []workerGitPeer{{Dir: wt}} }
			} else {
				o.FinalAt = time.Now().Add(-time.Hour)
			}
			return o
		}
		for _, reason := range []string{
			"unpushed work (no pushed upstream)",
			"work has no pushed upstream",
			"submodule work requires manual review",
			"worktree paths cannot be verified",
			"worktree index cannot be verified",
			"worktree status cannot be verified",
			"worktree status cannot be verified (over 64 MiB of dirty files)",
			"repository cannot be read: fatal: not a git repository",
			"uncommitted work (tracked.txt)",
			"unpushed work; something else",
			"",
		} {
			o := own()
			if workerGitNotOwn(ctx, snap, snap.Origin.WorkerGit, wt, paths, workerGitProof{Head: "abc"}, reason, o) || len(o.Excused) != 0 {
				t.Errorf("rule %s excused %q", rule, reason)
			}
		}
		for reason, state := range map[string]string{"uncommitted work": "dirty", "unpushed work": "unpushed"} {
			o := own()
			if !workerGitNotOwn(ctx, snap, snap.Origin.WorkerGit, wt, paths, workerGitProof{}, reason, o) || !strings.HasPrefix(o.Excused[wt], state+"; ") {
				t.Errorf("rule %s did not excuse plain %q: %v", rule, reason, o.Excused)
			}
		}
	}
}

// A real worktree whose branch has a commit and no upstream is "unpushed work
// (no pushed upstream)": made after the final reply, or with the person's
// pane in it, it still holds the worker.
func TestWorkerGitNoUpstreamWorktreeHoldsWorker(t *testing.T) {
	needBirthTimes(t)
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	wt := filepath.Join(t.TempDir(), "local-only")
	archiveGit(t, cwd, "worktree", "add", "-b", "local-only", wt)
	putCapabilityFile(t, filepath.Join(wt, "tracked.txt"), "local commit\n")
	archiveGit(t, wt, "commit", "-am", "never pushed")
	own := peersOf(workerGitPeer{Dir: wt})
	own.FinalAt = time.Now().Add(-time.Minute)
	if _, why := workerGitForArchiveOwned(context.Background(), snap, "", own); why != "unpushed work (no pushed upstream) (new worktree: "+wt+")" {
		t.Fatal(why)
	}
}

func TestWorkerExcusedReopenCorruptAndInFlight(t *testing.T) {
	t.Run("reopen closes the notice", func(t *testing.T) {
		needBirthTimes(t)
		a, p, now := archiveFixture(t)
		start := workerGitBaseline(t, p.Cwd).Origin.WorkerGit
		p.mu.Lock()
		o := p.Origin.clone()
		o.WorkerGit = start
		p.Origin = o
		p.mu.Unlock()
		a.workerArchives().liveCwd = func(*Pane) string { return "" }
		newDirtyWorktree(t, p.Cwd, "later")
		a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
		a.tryArchiveWorker(p, now)
		rows := a.archivedWorkerSummaries()
		if len(rows) != 1 {
			t.Fatal("not archived")
		}
		if _, ok := a.workerExcusedAsk(workerExcusedAskKey(p.ID)); !ok {
			t.Fatal("no notice")
		}
		id := rows[0].Worker.ArchiveID
		req := httptest.NewRequest("POST", "/api/worker-archives/"+id+"/reopen", nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.reopenWorkerArchive(w, req)
		if w.Code != 201 {
			t.Fatalf("reopen %d: %s", w.Code, w.Body.String())
		}
		if _, ok := a.workerExcusedAsk(workerExcusedAskKey(p.ID)); ok {
			t.Fatal("a reopened worker's excused-worktree notice stayed open")
		}
		s := a.workerArchives()
		s.noticeMu.Lock()
		_, kept := s.excused[p.ID]
		s.noticeMu.Unlock()
		if kept {
			t.Fatal("a reopened worker's record stayed")
		}
	})
	t.Run("corrupt file is kept aside", func(t *testing.T) {
		a, _ := newQuotaWorkerAPI(t)
		file := filepath.Join(a.dataDir, "worker-excused.json")
		putCapabilityFile(t, file, "{not json")
		a.recheckWorkerExcused(context.Background(), time.Now())
		aside, _ := filepath.Glob(file + ".corrupt-*")
		if len(aside) != 1 {
			t.Fatal("the damaged file was not kept aside")
		}
		if raw, err := os.ReadFile(aside[0]); err != nil || string(raw) != "{not json" {
			t.Fatal("kept-aside file changed", err)
		}
	})
	t.Run("not posted mid-archive; posted after; recheck runs while operation is held", func(t *testing.T) {
		a, _ := newQuotaWorkerAPI(t)
		dir := t.TempDir()
		a.workerRecordExcusedWorktrees([]string{"lead"}, "pane_w", "#9 w", nil, map[string]string{dir: "dirty; " + workerGitExcusedPeer})
		a.recheckWorkerExcused(context.Background(), time.Now())
		if _, ok := a.workerExcusedAsk(workerExcusedAskKey("pane_w")); ok {
			t.Fatal("posted while its archive was under way")
		}
		a.workerExcusedArchiveEnded("pane_w")
		s := a.workerArchives()
		// Let the background recheck finish before the temp dirs go.
		t.Cleanup(func() { s.excusedOp.Lock(); s.excusedOp.Unlock() })
		s.operation.Lock()
		defer s.operation.Unlock()
		a.archiveFinishedWorkers(time.Now())
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, ok := a.workerExcusedAsk(workerExcusedAskKey("pane_w")); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the recheck did not run while the archive operation was held")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
