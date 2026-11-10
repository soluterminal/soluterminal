package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLocalExclude(t *testing.T, repo, rules string) {
	t.Helper()
	putCapabilityFile(t, filepath.Join(repo, ".git", "info", "exclude"), rules)
}

// A repository's info/exclude counts only while it is byte-identical to the
// copy hashed at spawn (ST-60, @secure's bound 2).
func TestWorkerGitHonoursUnchangedLocalExclude(t *testing.T) {
	cwd := archiveRepo(t)
	writeLocalExclude(t, cwd, "/scratch/\n")
	snap := workerGitBaseline(t, cwd)
	if snap.Origin.WorkerGit.ExcludeSHA == "" {
		t.Fatal("spawn baseline did not record the exclude hash")
	}
	putCapabilityFile(t, filepath.Join(cwd, "scratch", "person.txt"), "hidden by the person's own rule\n")
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatalf("dirt under an unchanged local exclude kept the worker: %q", why)
	}
}

func TestWorkerGitIgnoresLocalExcludeEditedAfterSpawn(t *testing.T) {
	cwd := archiveRepo(t)
	writeLocalExclude(t, cwd, "/scratch/\n")
	snap := workerGitBaseline(t, cwd)
	// The worker hides its own work by editing the rules: strict rule again.
	writeLocalExclude(t, cwd, "/scratch/\n/work/\n")
	putCapabilityFile(t, filepath.Join(cwd, "work", "worker.txt"), "worker's unsaved work\n")
	if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.HasPrefix(why, "uncommitted work") {
		t.Fatalf("an exclude edited after spawn hid work: %q", why)
	}
}

func TestWorkerGitIgnoresSymlinkedLocalExclude(t *testing.T) {
	cwd := archiveRepo(t)
	rules := filepath.Join(t.TempDir(), "rules")
	putCapabilityFile(t, rules, "/work/\n")
	if err := os.MkdirAll(filepath.Join(cwd, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rules, filepath.Join(cwd, ".git", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
	if raw, sha := workerGitReadExclude(context.Background(), cwd); raw != nil || sha != "" {
		t.Fatal("a symlinked info/exclude was read")
	}
	snap := workerGitBaseline(t, cwd)
	putCapabilityFile(t, filepath.Join(cwd, "work", "worker.txt"), "worker's unsaved work\n")
	if _, why := workerGitForArchive(context.Background(), snap, ""); !strings.HasPrefix(why, "uncommitted work") {
		t.Fatalf("a symlinked exclude hid work: %q", why)
	}
}

// Untracked linked worktrees of the same repository do not make the start
// checkout unverifiable (ST-60, @secure's bound 3).
func TestWorkerGitSkipsRegisteredLinkedWorktrees(t *testing.T) {
	cwd := archiveRepo(t)
	archiveGit(t, cwd, "worktree", "add", "-q", "-b", "sibling", filepath.Join(cwd, ".claude", "worktrees", "sibling"))
	snap := workerGitBaseline(t, cwd)
	if _, why := workerGitForArchive(context.Background(), snap, ""); why != "" {
		t.Fatalf("a sibling linked worktree kept the worker: %q", why)
	}
	// Strict inspection (worktree removal) still sees it.
	if _, why := workerGitInspect(context.Background(), cwd, true); why == "" {
		t.Fatal("strict inspection skipped a linked worktree")
	}
}

func TestWorkerGitFakeOrPlainDirectoriesStillBlock(t *testing.T) {
	for _, kind := range []string{"fake-git-file", "plain-directory"} {
		t.Run(kind, func(t *testing.T) {
			cwd := archiveRepo(t)
			snap := workerGitBaseline(t, cwd)
			dir := filepath.Join(cwd, ".claude", "worktrees", "fake")
			switch kind {
			case "fake-git-file":
				// A .git file borrowed from another repository's worktree.
				other := archiveRepo(t)
				real := filepath.Join(other, ".claude", "worktrees", "real")
				archiveGit(t, other, "worktree", "add", "-q", "-b", "real", real)
				link, err := os.ReadFile(filepath.Join(real, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				putCapabilityFile(t, filepath.Join(dir, ".git"), string(link))
				putCapabilityFile(t, filepath.Join(dir, "work.txt"), "work\n")
			case "plain-directory":
				putCapabilityFile(t, filepath.Join(dir, "work.txt"), "work\n")
			}
			if _, why := workerGitForArchive(context.Background(), snap, ""); why == "" {
				t.Fatal("a directory that is not this repository's worktree was skipped")
			}
		})
	}
}

func TestWorkerGitLiveWorktreesSkipMainAndPrunable(t *testing.T) {
	cwd := archiveRepo(t)
	keep := filepath.Join(cwd, ".claude", "worktrees", "keep")
	gone := filepath.Join(cwd, ".claude", "worktrees", "gone")
	archiveGit(t, cwd, "worktree", "add", "-q", "-b", "keep", keep)
	archiveGit(t, cwd, "worktree", "add", "-q", "-b", "gone", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	live := workerGitLiveWorktrees(context.Background(), cwd)
	if !live[keep] || live[gone] || live[cwd] {
		t.Fatalf("live=%v", live)
	}
}

// Without a start baseline new worktrees are not inspected on their own, so
// linked worktrees are not skipped either.
func TestWorkerGitNoBaselineDoesNotSkipLinkedWorktrees(t *testing.T) {
	cwd := archiveRepo(t)
	sibling := filepath.Join(cwd, ".claude", "worktrees", "sibling")
	archiveGit(t, cwd, "worktree", "add", "-q", "-b", "sibling", sibling)
	putCapabilityFile(t, filepath.Join(sibling, "work.txt"), "unsaved\n")
	if opts := workerGitArchiveOptions(context.Background(), cwd, nil); opts.SkipLinkedWorktrees || opts.Exclude != nil {
		t.Fatalf("options without a baseline: %+v", opts)
	}
	if _, why := workerGitForArchive(context.Background(), PaneSnapshot{Cwd: cwd}, ""); why == "" {
		t.Fatal("a worker with no baseline closed over a dirty linked worktree")
	}
}

func TestWorkerGitReadRegularRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	putCapabilityFile(t, target, "rules\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	if _, err := workerGitReadRegular(link, 4096); err == nil {
		t.Fatal("a symlink was read as a regular file")
	}
	if raw, err := workerGitReadRegular(target, 4096); err != nil || string(raw) != "rules\n" {
		t.Fatalf("regular file: %q %v", raw, err)
	}
}

// A worker that moved into another repository gets the strict rule there:
// that repository's worktrees are not enumerated, so none may be skipped.
func TestWorkerGitOtherRepositoryIsStrict(t *testing.T) {
	start := archiveRepo(t)
	writeLocalExclude(t, start, "/scratch/\n")
	snap := workerGitBaseline(t, start)
	other := archiveRepo(t)
	nested := filepath.Join(other, ".claude", "worktrees", "nested")
	archiveGit(t, other, "worktree", "add", "-q", "-b", "nested", nested)
	putCapabilityFile(t, filepath.Join(nested, "work.txt"), "unsaved\n")
	if opts := workerGitArchiveOptions(context.Background(), other, snap.Origin.WorkerGit); opts.SkipLinkedWorktrees || opts.Exclude != nil {
		t.Fatalf("another repository got widened options: %+v", opts)
	}
	if _, why := workerGitForArchive(context.Background(), snap, other); why == "" {
		t.Fatal("a worker closed over another repository's dirty linked worktree")
	}
}
