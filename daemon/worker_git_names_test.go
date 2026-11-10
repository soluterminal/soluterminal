package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerGitReasonNamesNewDirtOnly(t *testing.T) {
	cwd := archiveRepo(t)
	putCapabilityFile(t, filepath.Join(cwd, "old.txt"), "person's file\n")
	snap := workerGitBaseline(t, cwd)
	putCapabilityFile(t, filepath.Join(cwd, "notes", "worker.md"), "worker's file\n")
	_, why := workerGitForArchive(context.Background(), snap, "")
	if why != "uncommitted work (notes/worker.md)" {
		t.Fatalf("why=%q", why)
	}
	if !workerUnpublishedReason(why) {
		t.Fatal("a named reason no longer counts as unpublished work")
	}
}

func TestWorkerGitReasonWithoutBaselineNamesAllDirt(t *testing.T) {
	cwd := archiveRepo(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		putCapabilityFile(t, filepath.Join(cwd, name), "x\n")
	}
	_, why := workerGitForArchive(context.Background(), PaneSnapshot{Cwd: cwd}, "")
	if why != "uncommitted work (a.txt, b.txt, c.txt +2 more)" {
		t.Fatalf("why=%q", why)
	}
}

func TestWorkerGitNamePathsCleansUntrustedNames(t *testing.T) {
	got := workerGitNamePaths([]string{"evil\x1b[2J‮name.txt", strings.Repeat("x", 80)})
	if strings.ContainsAny(got, "\x1b‮") {
		t.Fatalf("control characters kept: %q", got)
	}
	if !strings.HasPrefix(got, "evil?[2J?name.txt, ") || !strings.HasSuffix(got, strings.Repeat("x", 59)+"…") {
		t.Fatalf("got %q", got)
	}
}

func TestWorkerGitUnreadableFileIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	cwd := archiveRepo(t)
	locked := filepath.Join(cwd, "locked.txt")
	putCapabilityFile(t, locked, "no read permission\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	_, why := workerGitInspect(context.Background(), cwd, true)
	if why != "worktree status cannot be verified (cannot read locked.txt)" {
		t.Fatalf("why=%q", why)
	}
	if !workerPublicationUncertain(why) {
		t.Fatal("a named reason no longer counts as uncertain")
	}
}
