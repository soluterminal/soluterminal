package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workerGitBaseline(t *testing.T, cwd string) PaneSnapshot {
	t.Helper()
	start := captureWorkerGitStart(context.Background(), cwd)
	if start == nil {
		t.Fatal("baseline unavailable")
	}
	return PaneSnapshot{Cwd: cwd, Origin: &PaneOrigin{WorkerGit: start}}
}
func TestWorkerGitBaselineDistinguishesExistingAndNewDirt(t *testing.T) {
	for _, kind := range []string{"tracked", "untracked", "staged", "deleted", "rename"} {
		t.Run(kind, func(t *testing.T) {
			cwd := archiveRepo(t)
			switch kind {
			case "tracked", "staged":
				putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "person edits\n")
			case "untracked":
				putCapabilityFile(t, filepath.Join(cwd, "new.txt"), "person edits\n")
			case "deleted":
				archiveGit(t, cwd, "rm", "tracked.txt")
			case "rename":
				archiveGit(t, cwd, "mv", "tracked.txt", "renamed.txt")
			}
			if kind == "staged" {
				archiveGit(t, cwd, "add", "tracked.txt")
			}
			snap := workerGitBaseline(t, cwd)
			if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
				t.Fatal("pre-existing dirt attributed to worker:", why)
			}
			path := "tracked.txt"
			if kind == "untracked" {
				path = "new.txt"
			}
			if kind == "rename" {
				path = "renamed.txt"
			}
			putCapabilityFile(t, filepath.Join(cwd, path), "worker edits the same file\n")
			if kind == "staged" {
				archiveGit(t, cwd, "add", path)
			}
			if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.Contains(why, "uncommitted work") {
				t.Fatal("new bytes not retained:", why)
			}
		})
	}
}
func TestWorkerGitBaselineComparesEachPath(t *testing.T) {
	for _, kind := range []string{"unchanged", "one-removed", "one-changed", "one-added"} {
		t.Run(kind, func(t *testing.T) {
			cwd := archiveRepo(t)
			putCapabilityFile(t, filepath.Join(cwd, "a.txt"), "person a\n")
			putCapabilityFile(t, filepath.Join(cwd, "b.txt"), "person b\n")
			snap := workerGitBaseline(t, cwd)
			switch kind {
			case "one-removed":
				if err := os.Remove(filepath.Join(cwd, "b.txt")); err != nil {
					t.Fatal(err)
				}
			case "one-changed":
				putCapabilityFile(t, filepath.Join(cwd, "b.txt"), "worker b\n")
			case "one-added":
				putCapabilityFile(t, filepath.Join(cwd, "c.txt"), "worker c\n")
			}
			_, why := workerGitForArchive(context.Background(), snap, "")
			if want := kind == "unchanged" || kind == "one-removed"; (why == "") != want {
				t.Fatalf("why=%q", why)
			}
		})
	}
}

func TestWorkerGitDirtWithinBaselineNeedsPerPathDigests(t *testing.T) {
	old := workerGitProof{Dirty: "whole"}
	if workerGitDirtWithinBaseline(workerGitProof{Dirty: "other", Paths: map[string]string{"a": "1"}}, old) {
		t.Fatal("a baseline without per-path digests matched different dirt")
	}
	if !workerGitDirtWithinBaseline(workerGitProof{Dirty: "whole"}, old) || !workerGitDirtWithinBaseline(workerGitProof{}, old) {
		t.Fatal("identical or no dirt did not match")
	}
}

func TestWorkerGitChecksNewWorktrees(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "pre-existing person edit\n")
	snap := workerGitBaseline(t, cwd)
	own := filepath.Join(t.TempDir(), "worker")
	archiveGit(t, cwd, "worktree", "add", "-b", "worker", own)
	putCapabilityFile(t, filepath.Join(own, "tracked.txt"), "worker change\n")
	// Give the branch an upstream so dirt is the first blocking condition.
	archiveGit(t, own, "branch", "--set-upstream-to=origin/main")
	if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.Contains(why, "uncommitted work (new worktree:") {
		t.Fatal(why)
	}
	archiveGit(t, own, "commit", "-am", "worker change")
	if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.Contains(why, "unpushed work") {
		t.Fatal(why)
	}
	archiveGit(t, own, "push", "-u", "origin", "worker")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatal(why)
	}
	// The current process cwd is checked even if its checkout predates spawn.
	putCapabilityFile(t, filepath.Join(own, "tracked.txt"), "more work\n")
	if _, why := workerGitForArchive(context.Background(), snap, own); !strings.Contains(why, "worker process cwd") {
		t.Fatal(why)
	}
}
func TestWorkerGitNoBaselineKeepsConservativeBehavior(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "unknown edits\n")
	if _, why := workerGitForArchive(context.Background(), PaneSnapshot{Cwd: cwd}, ""); why != "uncommitted work" {
		t.Fatal(why)
	}
}
func TestWorkerArchiveUsesBaselineAndRechecksBytes(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed"}[change], func(t *testing.T) {
			a, p, now := archiveFixture(t)
			putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "person edits\n")
			start := workerGitBaseline(t, p.Cwd).Origin.WorkerGit
			p.mu.Lock()
			o := p.Origin.clone()
			o.WorkerGit = start
			p.Origin = o
			p.mu.Unlock()
			a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			if change {
				a.workerArchives().beforeCommit = func() { putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "changed during archive\n") }
			}
			a.tryArchiveWorker(p, now)
			if _, live := a.mgr.Get(p.ID); live != change {
				t.Fatalf("live=%v change=%v", live, change)
			}
		})
	}
}

func TestWorkerGitStartIsDaemonOwned(t *testing.T) {
	a, _ := newQuotaWorkerAPI(t)
	forged := &PaneOrigin{Source: paneOriginAgent, WorkerGit: &workerGitStart{Cwd: "forged"}}
	for _, restore := range []bool{false, true} {
		got := a.validatedWorkerOrigin(forged, createPaneReq{Restore: restore})
		if got.WorkerGit != nil {
			t.Fatal("client supplied baseline survived validation")
		}
	}
	if forged.WorkerGit == nil {
		t.Fatal("validation mutated supplied origin")
	}
}

func TestWorkerGitLiveSubdirectoryUsesBaseline(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "sub", "file.txt"), "pre-existing dirt\n")
	snap := workerGitBaseline(t, cwd)
	for _, live := range []string{filepath.Join(cwd, "sub"), t.TempDir()} {
		if _, why := workerGitForArchive(context.Background(), snap, live); why != "" {
			t.Fatal("unrelated/subdirectory cwd blocked archive:", why)
		}
	}
}

func TestWorkerGitNewDetachedPublishedCheckoutDoesNotRetain(t *testing.T) {
	cwd := archiveRepo(t)
	snap := workerGitBaseline(t, cwd)
	preview := filepath.Join(t.TempDir(), "preview-source")
	archiveGit(t, cwd, "worktree", "add", "--detach", preview, "origin/main")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatal("published detached checkout blocked worker:", why)
	}
	putCapabilityFile(t, filepath.Join(preview, "tracked.txt"), "dirty preview\n")
	if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.Contains(why, "uncommitted work") {
		t.Fatal("detached dirty work ignored:", why)
	}
	archiveGit(t, preview, "commit", "-am", "unpublished detached commit")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why == "" {
		t.Fatal("unpublished detached commit ignored")
	}
}

func TestWorkerGitBaselineSurvivesRemoteRefAdvance(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "person's existing edit\n")
	snap := workerGitBaseline(t, cwd)
	remote := archiveGit(t, cwd, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other-clone")
	archiveGit(t, cwd, "clone", "--branch", "main", remote, other)
	putCapabilityFile(t, filepath.Join(other, "other.txt"), "someone else's commit\n")
	archiveGit(t, other, "add", "other.txt")
	archiveGit(t, other, "commit", "-m", "advance remote")
	archiveGit(t, other, "push", "origin", "main")
	archiveGit(t, cwd, "fetch", "origin")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatal("fetch attributed existing dirt to worker:", why)
	}
}

func TestWorkerGitPushedCommitDoesNotAdoptExistingDirt(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "person's existing edit\n")
	snap := workerGitBaseline(t, cwd)
	putCapabilityFile(t, filepath.Join(cwd, "worker.txt"), "worker's separate change\n")
	archiveGit(t, cwd, "add", "worker.txt")
	archiveGit(t, cwd, "commit", "-m", "worker change only")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why == "" {
		t.Fatal("unpushed worker commit ignored")
	}
	archiveGit(t, cwd, "push", "origin", "main")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatal("pushed work attributed unchanged initial dirt to worker:", why)
	}
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "worker edits existing dirty file\n")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "uncommitted work" {
		t.Fatal("new edits to initial dirt ignored:", why)
	}
}
