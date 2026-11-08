package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubWorkerBox makes the archive check read screen as the worker's pane.
func stubWorkerBox(t *testing.T, screen string, ok bool) {
	t.Helper()
	prev := draftGuardScreen
	draftGuardScreen = func(*Pane) (string, string, bool) { return screen, screen, ok }
	t.Cleanup(func() { draftGuardScreen = prev })
}

// restartWorkerDaemon simulates a daemon restart for the archive state: the
// in-memory receipts are gone and the saved ones reload as Restored, and the
// worker manager starts after the worker's final reply.
func restartWorkerDaemon(t *testing.T, a *apiServer, now time.Time) {
	t.Helper()
	s := a.workerArchives()
	s.mu.Lock()
	s.loaded = false
	s.reads = nil
	s.mu.Unlock()
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now
	q.mu.Unlock()
}

func TestWorkerArchiveFinalFromBeforeRestart(t *testing.T) {
	for _, kind := range []string{"empty-box", "draft-in-box", "unreadable-box", "input-since-restart", "keystroke-since-restart", "never-read"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			if kind != "never-read" {
				a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			}
			restartWorkerDaemon(t, a, now)
			screen, readable := deliveryClaudeScreen(""), true
			switch kind {
			case "draft-in-box":
				screen = deliveryClaudeScreen("half a thought")
			case "unreadable-box":
				readable = false
			case "input-since-restart":
				p.inputRevision.Add(1)
			case "keystroke-since-restart":
				p.noteHumanInput(now)
			}
			stubWorkerBox(t, screen, readable)
			a.tryArchiveWorker(p, now)
			_, open := a.mgr.Get(p.ID)
			archived := len(a.archivedWorkerSummaries()) == 1
			want := kind == "empty-box"
			if archived != want || open == want {
				t.Fatalf("archived=%v open=%v, want archived=%v", archived, open, want)
			}
		})
	}
}

func TestWorkerReadReceiptsSurviveRestart(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	raw, err := os.ReadFile(filepath.Join(a.dataDir, "worker-reads.json"))
	if err != nil || !strings.Contains(string(raw), p.ID) {
		t.Fatalf("receipt not saved: %v %s", err, raw)
	}
	restartWorkerDaemon(t, a, now)
	s := a.workerArchives()
	s.mu.Lock()
	a.loadWorkerArchivesLocked(s)
	got := s.reads[p.ID]
	s.mu.Unlock()
	if !got.Restored || got.Pane != nil || got.Turn != workerReplyKey(a.paneLatestReply(p)) {
		t.Fatalf("restored receipt = %+v", got)
	}
	// A restored receipt never counts once the pane has had input, even if
	// the box reads empty again.
	stubWorkerBox(t, deliveryClaudeScreen(""), true)
	atomic.StoreInt64(&p.LastInputMs, now.UnixMilli())
	if workerFinalAfterRestart(p, false) {
		t.Fatal("input since restart still counted as final")
	}
}

func TestWorkerGitHashFileSamplesLargeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.bin")
	data := make([]byte, workerGitWholeFileBytes+3*workerGitSampleBytes)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := func() (string, int64) {
		t.Helper()
		f, info, err := openUserFile(path, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		h := sha256.New()
		n, err := workerGitHashFile(t.Context(), h, f, info, make([]byte, 64<<10))
		if err != nil {
			t.Fatal(err)
		}
		return string(h.Sum(nil)), n
	}
	base, n := digest()
	if n != 2*workerGitSampleBytes {
		t.Fatalf("read %d bytes of a large file, want two samples", n)
	}
	if again, _ := digest(); again != base {
		t.Fatal("unchanged large file changed its digest")
	}
	stamp := time.Now().Add(-time.Hour)
	for _, edit := range []struct {
		name string
		at   int
	}{{"first MiB", 10}, {"last MiB", len(data) - 10}} {
		data[edit.at]++
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		// Even with its old modification time restored, a sampled edit shows.
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if got, _ := digest(); got == base {
			t.Fatalf("edit in the %s kept the digest", edit.name)
		}
		base, _ = digest()
	}
	// A middle edit is caught by the changed modification time.
	data[len(data)/2]++
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := digest(); got == base {
		t.Fatal("middle edit with a new modification time kept the digest")
	}
}

func TestWorkerGitInspectLargeUntrackedFileIsVerifiable(t *testing.T) {
	cwd := archiveRepo(t)
	big := filepath.Join(cwd, ".claude", "prov", "solud-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(big), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, workerGitWholeFileBytes+workerGitSampleBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	proof, why := workerGitInspect(t.Context(), cwd, true)
	if why != "uncommitted work" || proof.Dirty == "" {
		t.Fatalf("large untracked file: why=%q dirty=%q", why, proof.Dirty)
	}
	again, _ := workerGitInspect(t.Context(), cwd, true)
	if again.Dirty != proof.Dirty {
		t.Fatal("unchanged large untracked file is not a stable baseline")
	}
}

// Saving receipts looks panes up in the manager. Archive commit holds the
// manager lock and then takes s.mu, so the save must never wait for the
// manager while it holds s.mu.
func TestWorkerReadReceiptSaveNeverHoldsArchiveLockForManager(t *testing.T) {
	a, p, _ := archiveFixture(t)
	reply := a.paneLatestReply(p)
	s := a.workerArchives()
	a.mgr.mu.Lock()
	done := make(chan struct{})
	go func() {
		a.noteWorkerReplyRead("lead", p, reply)
		close(done)
	}()
	recorded := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !recorded; time.Sleep(time.Millisecond) {
		if s.mu.TryLock() {
			_, recorded = s.reads[p.ID]
			s.mu.Unlock()
		}
	}
	// The receipt is in place and the save is waiting on the manager, with
	// s.mu free for an archive commit that holds the manager lock.
	if !recorded || !s.mu.TryLock() {
		a.mgr.mu.Unlock()
		t.Fatal("receipt save holds s.mu while it waits for the manager")
	}
	s.mu.Unlock()
	a.mgr.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt save did not finish")
	}
	raw, err := os.ReadFile(filepath.Join(a.dataDir, "worker-reads.json"))
	if err != nil || !strings.Contains(string(raw), p.ID) {
		t.Fatalf("receipt not saved: %v %s", err, raw)
	}
}

// A keystroke noted during the slow Git proof, before its write reaches the
// pane, keeps a worker whose final reply is from before the restart.
func TestWorkerArchiveRestartRechecksKeystrokeAtCommit(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	restartWorkerDaemon(t, a, now)
	stubWorkerBox(t, deliveryClaudeScreen(""), true)
	a.workerArchives().beforeCommit = func() { p.noteHumanInput(time.Now()) }
	a.tryArchiveWorker(p, now)
	if _, open := a.mgr.Get(p.ID); !open || len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("keystroke during the proof did not keep the worker")
	}
}

func TestVisibleWorkerOriginDropsStartBaseline(t *testing.T) {
	o := &PaneOrigin{Worker: &workerInfo{}, WorkerGit: &workerGitStart{Proof: workerGitProof{Dirty: "d", Paths: map[string]string{"a": "b"}}}}
	if v := visibleWorkerOrigin(o); v.WorkerGit != nil {
		t.Fatalf("client view kept the start baseline: %+v", v.WorkerGit)
	}
	if o.WorkerGit == nil {
		t.Fatal("the stored origin lost its start baseline")
	}
}
