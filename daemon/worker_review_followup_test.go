package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWorkerGitRecheckDoesNotBlockInputOrManager(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.workerArchives().beforeGitRecheck = func() { close(entered); <-release }
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	go func() { a.tryArchiveWorker(p, now); close(done) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("archive never reached Git recheck")
	case <-time.After(3 * time.Second):
		t.Fatal("Git recheck did not start")
	}
	inputFree := p.agentWriteMu.TryLock()
	if inputFree {
		p.inputRevision.Add(1)
		p.agentWriteMu.Unlock()
	}
	managerFree := a.mgr.mu.TryLock()
	if managerFree {
		a.mgr.mu.Unlock()
	}
	close(release)
	released = true
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("archive did not settle")
	}
	if !inputFree || !managerFree {
		t.Fatalf("slow Git proof blocks input=%v manager=%v", !inputFree, !managerFree)
	}
	if _, live := a.mgr.Get(p.ID); !live {
		t.Fatal("input arriving during Git proof was archived")
	}
}

type cancelWorkerDirtyRead struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancelWorkerDirtyRead) Read(p []byte) (int, error) {
	r.calls++
	p[0] = 'x'
	r.cancel()
	return 1, nil
}
func TestWorkerDirtyHashStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &cancelWorkerDirtyRead{cancel: cancel}
	n, err := io.CopyBuffer(sha256.New(), workerGitContextReader{ctx, source}, make([]byte, 64<<10))
	if n != 1 || !errors.Is(err, context.Canceled) || source.calls != 1 {
		t.Fatalf("read after cancellation: bytes=%d calls=%d err=%v", n, source.calls, err)
	}
}

func TestWorkerGitPrunableWorktrees(t *testing.T) {
	for _, kind := range []string{"published-deleted", "unpublished-deleted", "existing-broken-gitdir"} {
		t.Run(kind, func(t *testing.T) {
			cwd := archiveRepo(t)
			snap := workerGitBaseline(t, cwd)
			other := filepath.Join(t.TempDir(), "preview")
			archiveGit(t, cwd, "worktree", "add", "--detach", other, "origin/main")
			if kind != "published-deleted" {
				putCapabilityFile(t, filepath.Join(other, "tracked.txt"), "worker changes\n")
			}
			if kind == "unpublished-deleted" {
				archiveGit(t, other, "commit", "-am", "unpublished")
			}
			if kind == "existing-broken-gitdir" {
				if err := os.Remove(filepath.Join(other, ".git")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.RemoveAll(other); err != nil {
				t.Fatal(err)
			}
			listed := archiveGit(t, cwd, "worktree", "list", "--porcelain")
			if !strings.Contains(listed, "prunable") {
				t.Fatalf("fixture is not prunable: %s", listed)
			}
			paths, err := workerWorktrees(context.Background(), cwd)
			if err != nil {
				t.Fatal(err)
			}
			_, why := workerGitForArchive(context.Background(), snap, "")
			if kind == "published-deleted" {
				if slices.Contains(paths, other) || why != "" {
					t.Fatalf("stale published checkout retains worker: paths=%v why=%s", paths, why)
				}
			} else if !slices.Contains(paths, other) || why == "" {
				t.Fatalf("prunable entry hid existing/unpublished work: paths=%v why=%s", paths, why)
			}
		})
	}
}

func TestWorkerRestartDoesNotInventUnreadNotices(t *testing.T) {
	for _, kind := range []string{"silent-clean", "read-clean", "dismissed-unread", "retained-dirty", "cleared-git-condition"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			lead, _ := a.mgr.Get("lead")
			if kind == "retained-dirty" {
				putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "worker dirty\n")
				a.tryArchiveWorker(p, now)
			}
			if kind == "dismissed-unread" {
				a.tryArchiveWorker(p, now)
				lead.SetFlag(false, "")
			}
			if kind == "cleared-git-condition" {
				a.flagWorkerLead("lead", p.ID, "uncommitted work")
			}
			memo := lead.Snapshot().FlagMemo
			q := a.workers()
			q.mu.Lock()
			q.startedAt = now
			q.mu.Unlock()
			s := a.workerArchives()
			s.noticeMu.Lock()
			s.noticesLoaded = false
			s.retained = nil
			s.noticeMu.Unlock()
			if kind == "read-clean" {
				a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			}
			a.reconcileWorkerRetentions(true)
			a.tryArchiveWorker(p, now)
			got := lead.Snapshot()
			if kind == "retained-dirty" {
				if !got.Flagged || got.FlagMemo != memo {
					t.Fatalf("existing retention changed: %+v", got)
				}
			} else if got.Flagged {
				t.Fatalf("restart invented a retention: %+v", got)
			}
			if _, live := a.mgr.Get(p.ID); !live {
				t.Fatal("old final archived after restart")
			}
		})
	}
}

type failingWorkerCleanupStore struct {
	paneStore
	fail     bool
	attempts int
}

func (s *failingWorkerCleanupStore) Remove(id string) error {
	s.attempts++
	if s.fail {
		return errors.New("cleanup disk unavailable")
	}
	return s.paneStore.Remove(id)
}
func TestWorkerCleanupFailureNoticeSurvivesUntilRetrySucceeds(t *testing.T) {
	for _, owner := range []string{"worker-manager", "user"} {
		t.Run(owner, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			store := &failingWorkerCleanupStore{paneStore: a.panes, fail: true}
			a.panes = store
			a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			a.tryArchiveWorker(p, now)
			if _, live := a.mgr.Get(p.ID); live {
				t.Fatal("fixture did not detach worker")
			}
			lead, _ := a.mgr.Get("lead")
			if !strings.Contains(lead.Snapshot().FlagMemo, "pane-store cleanup failed") {
				t.Fatal("cleanup failure did not notify lead")
			}
			if owner == "user" {
				lead.SetFlag(true, "my bookmark")
			}
			a.expireWorkerArchivesLocked(now)
			a.reconcileWorkerRetentions(false)
			if !lead.Snapshot().Flagged {
				t.Fatal("failed retry erased the cleanup notice")
			}
			if store.attempts < 2 {
				t.Fatal("cleanup was not retried")
			}
			store.fail = false
			a.expireWorkerArchivesLocked(now)
			a.reconcileWorkerRetentions(false)
			if owner == "user" {
				if lead.Snapshot().FlagOwner != "user" || lead.Snapshot().FlagMemo != "my bookmark" {
					t.Fatal("cleanup changed user bookmark")
				}
			} else if lead.Snapshot().Flagged {
				t.Fatal("successful cleanup left notice")
			}
			s := a.workerArchives()
			s.mu.Lock()
			ready := len(s.entries) == 1 && s.entries[0].Ready
			s.mu.Unlock()
			if !ready {
				t.Fatal("successful cleanup did not finalize archive")
			}
		})
	}
}

func TestWorkerRetentionEmptyDataDirDoesNotReadCwd(t *testing.T) {
	t.Chdir(t.TempDir())
	putCapabilityFile(t, "worker-retentions.json", `{"injected":{"Lead":"lead","Reason":"from cwd"}}`)
	a := &apiServer{mgr: NewManager("")}
	s := a.workerArchives()
	s.noticeMu.Lock()
	a.loadWorkerRetentionsLocked(s)
	count := len(s.retained)
	s.noticeMu.Unlock()
	if count != 0 {
		t.Fatal("loaded retention metadata from process cwd")
	}
}
